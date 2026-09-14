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
// edges crisp on the e-ink panel in the large bottom QR area.
func newsQR(link string) (image.Image, error) {
	code, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	modules := len(code.Bitmap())
	scale := newsQRSize / modules
	if scale < 4 {
		return nil, fmt.Errorf("article URL too dense for display QR")
	}
	return code.Image(modules * scale), nil
}

const (
	newsQRSize     = 560
	newsQRTop      = 780
	newsTextBottom = 740
	newsTextWidth  = width - 180
)

type newsRow struct {
	lines []string
	y, h  int
}

func layoutNews(items []newsItem, face font.Face) []newsRow {
	const top, rowHeight, gap = 160, 180, 20
	columns := newsTextWidth / font.MeasureString(face, "M").Ceil()
	var rows []newsRow
	for i, item := range items {
		if i == 3 {
			break
		}
		lines := wrap(item.Title, columns)
		if len(lines) > 2 {
			lines = lines[:2]
			last := []rune(lines[1])
			if len(last) >= columns {
				last = last[:columns-1]
			}
			lines[1] = strings.TrimSpace(string(last)) + "…"
		}
		rows = append(rows, newsRow{lines: lines, y: top + i*(rowHeight+gap), h: rowHeight})
	}

	return rows
}

func (n *newsService) render() ([]byte, error) {
	tf, err := opentype.Parse(gomono.TTF)
	if err != nil {
		return nil, err
	}
	face, err := opentype.NewFace(tf, &opentype.FaceOptions{Size: 56, DPI: 72, Hinting: font.HintingFull})
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
	d.DrawString("TOP NEWS")
	draw.Draw(img, image.Rect(90, 126, width-90, 128), image.Black, image.Point{}, draw.Src)
	items := n.snapshot()
	rows := layoutNews(items, face)
	d.Face = face
	if len(rows) == 0 {
		d.Dot = fixed.P(90, 260)
		d.DrawString("No news available yet.")
	}
	for i, row := range rows {
		if i > 0 {
			draw.Draw(img, image.Rect(90, row.y-10, width-90, row.y-8), image.Black, image.Point{}, draw.Src)
		}
		y := row.y + (row.h-len(row.lines)*70)/2 + 56
		for _, line := range row.lines {
			d.Dot = fixed.P(90, y)
			d.DrawString(line)
			y += 70
		}
	}
	if len(rows) > 0 {
		qr, err := newsQR(items[0].Link)
		if err == nil {
			x := (width - qr.Bounds().Dx()) / 2
			y := newsQRTop + (newsQRSize-qr.Bounds().Dy())/2
			draw.Draw(img, image.Rect(x, y, x+qr.Bounds().Dx(), y+qr.Bounds().Dy()), qr, qr.Bounds().Min, draw.Src)
		}

	}

	return encodeGrayPNG(img)
}
