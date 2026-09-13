package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"image"
	"image/draw"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const defaultRSSURL = "http://10.17.17.98:8090/up2date/rss.xml"

type newsItem struct {
	Title string `xml:"title"`
	Link  string `xml:"link"`
}
type newsService struct {
	url    string
	client *http.Client
	mu     sync.RWMutex
	items  []newsItem
}

func validNewsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil
}

func newNews(raw string) *newsService {
	if raw == "" {
		return nil
	}
	return &newsService{url: raw, client: &http.Client{Timeout: 8 * time.Second}}
}

func (n *newsService) snapshot() []newsItem {
	if n == nil {
		return nil
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return append([]newsItem(nil), n.items...)
}

func (n *newsService) refresh(ctx context.Context) (err error) {
	// Failed refreshes remove the slide rather than showing stale news.
	var items []newsItem
	defer func() { n.mu.Lock(); n.items = items; n.mu.Unlock() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("RSS HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 2<<20 {
		return fmt.Errorf("RSS exceeds 2 MiB")
	}
	var feed struct {
		XMLName xml.Name `xml:"rss"`
		Channel struct {
			Items []newsItem `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(raw, &feed); err != nil {
		return err
	}
	base := resp.Request.URL
	for _, item := range feed.Channel.Items {
		item.Title = strings.Join(strings.Fields(item.Title), " ")
		link, err := url.Parse(strings.TrimSpace(item.Link))
		if err != nil || item.Title == "" || strings.TrimSpace(item.Link) == "" {
			continue
		}
		item.Link = base.ResolveReference(link).String()
		if !validNewsURL(item.Link) {
			continue
		}
		if _, err := newsQR(item.Link); err != nil {
			continue
		}
		items = append(items, item)
		if len(items) == 100 {
			break
		}
	}
	return nil
}

func (n *newsService) run(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		if err := n.refresh(ctx); err != nil && ctx.Err() == nil {
			log.Printf("RSS news: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Integer module scaling and the encoder's four-module quiet zone keep QR
// edges crisp on the e-ink panel. Never squeeze a dense code to fit a row.
func newsQR(link string) (image.Image, error) {
	code, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	modules := len(code.Bitmap())
	scale := 360 / modules
	if scale < 4 {
		return nil, fmt.Errorf("article URL too dense for display QR")
	}
	return code.Image(modules * scale), nil
}

type newsRow struct {
	lines []string
	qr    image.Image
	y, h  int
}

func layoutNews(items []newsItem, face font.Face) []newsRow {
	const top, bottom, textWidth = 160, height - 140, width - 180 - 400
	cell := font.MeasureString(face, "M").Ceil()
	y := top
	var rows []newsRow
	for _, item := range items {
		qr, err := newsQR(item.Link)
		if err != nil {
			continue
		}
		lines := wrap(item.Title, textWidth/cell)
		h := max(len(lines)*80, 360)
		if y+h > bottom {
			break
		}
		rows = append(rows, newsRow{lines: lines, qr: qr, y: y, h: h})
		y += h + 32
	}
	return rows
}

func (n *newsService) render() ([]byte, error) {
	tf, err := opentype.Parse(gomono.TTF)
	if err != nil {
		return nil, err
	}
	face, err := opentype.NewFace(tf, &opentype.FaceOptions{Size: 64, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	defer face.Close()
	small, err := opentype.NewFace(tf, &opentype.FaceOptions{Size: 24, DPI: 72})
	if err != nil {
		return nil, err
	}
	defer small.Close()
	img := image.NewGray(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	d := font.Drawer{Dst: img, Src: image.Black, Face: small, Dot: fixed.P(90, 100)}
	d.DrawString("TOP NEWS / SCAN TO READ")
	draw.Draw(img, image.Rect(90, 126, width-90, 128), image.Black, image.Point{}, draw.Src)
	rows := layoutNews(n.snapshot(), face)
	d.Face = face
	if len(rows) == 0 {
		d.Dot = fixed.P(90, 260)
		d.DrawString("No news available yet.")
	}
	for _, row := range rows {
		y := row.y + (row.h-len(row.lines)*80)/2 + 64
		for _, line := range row.lines {
			d.Dot = fixed.P(90, y)
			d.DrawString(line)
			y += 80
		}
		x := width - 90 - 360 + (360-row.qr.Bounds().Dx())/2
		y = row.y + (row.h-row.qr.Bounds().Dy())/2
		draw.Draw(img, image.Rect(x, y, x+row.qr.Bounds().Dx(), y+row.qr.Bounds().Dy()), row.qr, image.Point{}, draw.Src)
	}
	return encodeGrayPNG(img)
}
