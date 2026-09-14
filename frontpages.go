package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/webp"
)

type newspaper struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type coverMetadata struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Date     string `json:"date"`
	ImageURL string `json:"image_url"`
}
type coverSlide struct {
	ID      string
	PaperID string
	Name    string
}

// Only the list of configured newspapers is retained. Cover metadata, source
// image bytes and rendered cover PNGs are fetched/generated anew on every GET.
type frontpages struct {
	base   *url.URL
	client *http.Client
	mu     sync.RWMutex
	slides []coverSlide
}

func newFrontpages(origin string) (*frontpages, error) {
	base, err := url.Parse(strings.TrimRight(origin, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Path != "" {
		return nil, fmt.Errorf("frontpages-url must be an HTTP(S) origin")
	}
	f := &frontpages{base: base}
	f.client = &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != base.Scheme || req.URL.Host != base.Host {
			return fmt.Errorf("frontpages redirect must stay on configured server")
		}
		return nil
	}}
	return f, nil
}

func (f *frontpages) get(ctx context.Context, path string, limit int64) ([]byte, error) {
	relative, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	target := f.base.ResolveReference(relative)
	if target.Scheme != f.base.Scheme || target.Host != f.base.Host || target.User != nil {
		return nil, fmt.Errorf("cover URL must stay on configured frontpages server")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cache-Control", "no-cache, no-store")
	req.Header.Set("Pragma", "no-cache")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("frontpages %s: HTTP %d", target.Path, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("frontpages response too large")
	}
	return data, nil
}

func (f *frontpages) snapshot() []coverSlide {
	if f == nil {
		return nil
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([]coverSlide(nil), f.slides...)
}

func (f *frontpages) render(ctx context.Context, id string) ([]byte, error) {
	// IDs have a stable paper hash and a per-request suffix to defeat device caches.
	var paper coverSlide
	for _, candidate := range f.snapshot() {
		if strings.HasPrefix(id, candidate.ID+"-") {
			paper = candidate
			break
		}
	}
	if paper.ID == "" {
		return nil, fmt.Errorf("unknown newspaper")
	}
	data, err := f.get(ctx, "/api/newspapers/"+url.PathEscape(paper.PaperID)+"/today", 1<<20)
	if err != nil {
		return nil, err
	}
	var meta coverMetadata
	if err = json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	if meta.ImageURL == "" || meta.ID != paper.PaperID {
		return nil, fmt.Errorf("invalid cover metadata for %s", paper.PaperID)
	}
	if _, err = time.Parse("2006-01-02", meta.Date); err != nil {
		return nil, fmt.Errorf("invalid edition date for %s", paper.PaperID)
	}
	raw, err := f.get(ctx, meta.ImageURL, 20<<20)
	if err != nil {
		return nil, err
	}
	return renderCover(raw, paper.PaperID, paper.Name, meta.Date)
}

func (f *frontpages) refresh(ctx context.Context) error {
	data, err := f.get(ctx, "/api/newspapers", 1<<20)
	if err != nil {
		return err
	}
	var papers []newspaper
	if err = json.Unmarshal(data, &papers); err != nil {
		return err
	}
	if len(papers) > 64 {
		return fmt.Errorf("frontpages catalog exceeds 64 papers")
	}
	next := []coverSlide{}
	seen := map[string]bool{}
	for _, p := range papers {
		if p.ID == "" || p.Name == "" || len(p.ID) > 256 || len(p.Name) > 256 || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		sum := sha256.Sum256([]byte(p.ID))
		next = append(next, coverSlide{ID: "cover-" + hex.EncodeToString(sum[:16]), PaperID: p.ID, Name: p.Name})
	}
	f.mu.Lock()
	f.slides = next
	f.mu.Unlock()
	log.Printf("frontpages: %d newspapers discovered; covers fetched live", len(next))
	return nil
}

func (f *frontpages) run(ctx context.Context) {
	refresh := func() {
		if err := f.refresh(ctx); err != nil && ctx.Err() == nil {
			log.Printf("frontpages catalog: %v", err)
		}
	}
	refresh()
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// Newspaper sources can switch between portrait pages and landscape spreads
// between editions. Choose the crop from this image, never the newspaper name.
func coverSourceRect(bounds image.Rectangle) image.Rectangle {
	if bounds.Empty() {
		return bounds
	}
	crop := bounds
	crop.Max.Y = bounds.Min.Y + (bounds.Dy()+1)/2
	if bounds.Dx() >= bounds.Dy() {
		// A spread has two pages side by side; keep the existing newsstand zoom.
		crop.Min.X = bounds.Min.X + bounds.Dx()/2
	}
	return crop
}

func renderCover(raw []byte, paperID, name, date string) ([]byte, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 40_000_000 {
		return nil, fmt.Errorf("cover dimensions exceed limit")
	}
	source, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	sourceRect := coverSourceRect(source.Bounds())
	if sourceRect.Empty() {
		return nil, fmt.Errorf("cover crop is empty")
	}
	img := image.NewGray(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	const top, bottom = 108, height - 88
	scale := math.Min(float64(width-108)/float64(sourceRect.Dx()), float64(bottom-top)/float64(sourceRect.Dy()))
	w, h := int(float64(sourceRect.Dx())*scale), int(float64(sourceRect.Dy())*scale)
	x, y := (width-w)/2, top+(bottom-top-h)/2
	xdraw.CatmullRom.Scale(img, image.Rect(x, y, x+w, y+h), source, sourceRect, draw.Over, nil)
	tf, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return nil, err
	}
	face, err := opentype.NewFace(tf, &opentype.FaceOptions{Size: 30, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	defer face.Close()
	for font.MeasureString(face, name).Ceil() > width-550 {
		r := []rune(name)
		name = string(r[:len(r)-2]) + "…"
	}
	d := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(54, 72)}
	d.DrawString(name)
	label := "EDITION  " + date
	d.Dot = fixed.P(width-54-font.MeasureString(face, label).Ceil(), 72)
	d.DrawString(label)
	draw.Draw(img, image.Rect(54, 90, width-54, 92), image.Black, image.Point{}, draw.Src)
	return encodeGrayPNG(img)
}
