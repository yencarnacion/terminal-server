package main

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
)

func TestNewsAvailabilityAndRecovery(t *testing.T) {
	body := `<rss><channel><item><title>First &amp; foremost</title><link>/story</link></item><item><title>Unsafe</title><link>javascript:alert(1)</link></item></channel></rss>`
	status := 200
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); io.WriteString(w, body) }))
	defer upstream.Close()
	n := newNews(upstream.URL + "/rss.xml")
	a := &app{news: n}
	if err := n.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := n.snapshot(); !reflect.DeepEqual(got, []newsItem{{Title: "First & foremost", Link: upstream.URL + "/story"}}) {
		t.Fatal(got)
	}
	if a.playlist()[0] != "rss" {
		t.Fatal(a.playlist())
	}
	status = 404
	if err := n.refresh(context.Background()); err == nil {
		t.Fatal("accepted missing feed")
	}
	if a.playlist()[0] == "rss" {
		t.Fatal("offline feed in playlist")
	}
	status = 200
	body = `<rss><channel/></rss>`
	if err := n.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.snapshot()) != 0 {
		t.Fatal("empty feed")
	}
	body = `<rss><channel><item><title>Recovered</title><link>/recovered</link></item></channel></rss>`
	if err := n.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.playlist()[0] != "rss" {
		t.Fatal("did not recover")
	}
	body = `<rss>`
	if err := n.refresh(context.Background()); err == nil || len(n.snapshot()) != 0 {
		t.Fatal("malformed feed retained")
	}
}

func TestNewsLayoutAndImage(t *testing.T) {
	tf, _ := opentype.Parse(gomono.TTF)
	face, err := opentype.NewFace(tf, &opentype.FaceOptions{Size: 56, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		t.Fatal(err)
	}
	defer face.Close()
	items := make([]newsItem, 10)
	for i := range items {
		items[i] = newsItem{Title: "A large headline with enough words to wrap across lines", Link: "https://example.com/article"}
	}
	rows := layoutNews(items, face)
	if len(rows) != 3 {
		t.Fatal("incorrect item limit", len(rows))
	}
	for _, row := range rows {
		if row.y+row.h > newsTextBottom {
			t.Fatal("headline enters bottom QR area", row)
		}
		for _, line := range row.lines {
			if font.MeasureString(face, line).Ceil() > newsTextWidth {
				t.Fatal("text exceeds headline width")
			}
		}
	}
	if len(rows[0].lines[0]) <= (width-580)/font.MeasureString(face, "M").Ceil() {
		t.Fatal("headlines still reserve a side QR column")
	}
	longRows := layoutNews([]newsItem{{Title: strings.Repeat("Long headline ", 40)}, {Title: "Second"}, {Title: "Third"}}, face)
	if len(longRows) != 3 || len(longRows[0].lines) != 2 || !strings.HasSuffix(longRows[0].lines[1], "…") {
		t.Fatal("long first headline displaced later headlines", longRows)
	}
	qr, err := newsQR(items[0].Link)
	if err != nil || qr.Bounds().Dx() < 500 || qr.Bounds().Dx() > newsQRSize {
		t.Fatal("bottom QR is not enlarged", err)
	}
	n := &newsService{items: items}
	raw, err := n.render()
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != width || img.Bounds().Dy() != height {
		t.Fatal(img.Bounds())
	}
	// The bottom contains exactly the first article's enlarged QR.
	x := (width - qr.Bounds().Dx()) / 2
	y := newsQRTop + (newsQRSize-qr.Bounds().Dy())/2
	for py := 0; py < qr.Bounds().Dy(); py++ {
		for px := 0; px < qr.Bounds().Dx(); px++ {
			got, _, _, _ := img.At(x+px, y+py).RGBA()
			want, _, _, _ := qr.At(px, py).RGBA()
			if got != want {
				t.Fatalf("bottom QR mismatch at %d,%d", px, py)
			}
		}
	}
	if _, err = newsQR("https://example.com/" + strings.Repeat("x", 3000)); err == nil {
		t.Fatal("accepted overly dense QR")
	}
}

func TestNewsConfiguration(t *testing.T) {
	cfg := defaultConfiguration()
	if cfg.RSSURL != defaultRSSURL || cfg.SlideOrder[0] != "rss" {
		t.Fatal(cfg)
	}
	for _, raw := range []string{"", "https://example.com/feed.xml?format=rss"} {
		cfg.RSSURL = raw
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"/rss.xml", "ftp://example.com/rss", "http://user:secret@example.com/rss"} {
		cfg.RSSURL = raw
		if cfg.validate() == nil {
			t.Fatal("accepted", raw)
		}
	}
}

func TestNewsRoutes(t *testing.T) {
	a, err := newApp("http://example.com", 60, "slideshow", []string{"A quote"}, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	a.news = &newsService{items: []newsItem{{Title: "Top story", Link: "https://example.com/story"}}}
	req := httptest.NewRequest("GET", "/api/display", nil)
	id, err := a.nextDisplay(req)
	if err != nil || !strings.HasPrefix(id, "rss-") {
		t.Fatal(id, err)
	}
	rr := httptest.NewRecorder()
	a.routes().ServeHTTP(rr, httptest.NewRequest("GET", "/screens/"+id+".png", nil))
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("Cache-Control"), "no-store") {
		t.Fatal(rr.Code, rr.Header())
	}
	rr = httptest.NewRecorder()
	a.routes().ServeHTTP(rr, httptest.NewRequest("GET", "/preview?screen=rss", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "rss-") {
		t.Fatal(rr.Code, rr.Body.String())
	}
	a.news.items = nil
	if _, err := a.image(id); err != nil {
		t.Fatal("feed disappearance breaks issued image", err)
	}
}

func TestNewsPagination(t *testing.T) {
	items := make([]newsItem, 12)
	for i := range items {
		items[i] = newsItem{Title: fmt.Sprintf("Headline %d", i+1), Link: fmt.Sprintf("https://example.com/story/%d", i+1)}
	}
	n := &newsService{items: items}
	if got := n.pages(); !reflect.DeepEqual(got, []string{"rss", "rss-2", "rss-3", "rss-4"}) {
		t.Fatal(got)
	}
	for page, want := range []int{3, 3, 3, 1} {
		selected := newsPageItems(items, page)
		if len(selected) != want || selected[0] != items[page*3] {
			t.Fatal(page, selected)
		}
		raw, err := n.renderPage(page)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		qr, err := newsQR(items[page*3].Link)
		if err != nil {
			t.Fatal(err)
		}
		x := (width - qr.Bounds().Dx()) / 2
		y := newsQRTop + (newsQRSize-qr.Bounds().Dy())/2
		for py := 0; py < qr.Bounds().Dy(); py++ {
			for px := 0; px < qr.Bounds().Dx(); px++ {
				got, _, _, _ := img.At(x+px, y+py).RGBA()
				want, _, _, _ := qr.At(px, py).RGBA()
				if got != want {
					t.Fatalf("page %d QR does not match its first item", page+1)
				}
			}
		}
	}
	a, _ := newApp("http://example.com", 60, "slideshow", []string{"quote"}, make([]byte, 32))
	a.news = n
	a.slideOrder = []string{"rss", "quote"}
	for _, mode := range []string{"slideshow", "rss"} {
		a.mode = mode
		a.next = map[string]int{}
		for _, want := range []string{"rss", "rss-2", "rss-3", "rss-4"} {
			id, err := a.nextDisplay(httptest.NewRequest("GET", "/api/display", nil))
			expected, _ := a.screenID(want, a.now())
			if err != nil || id != expected {
				t.Fatal(mode, id, expected, err)
			}
			rec := httptest.NewRecorder()
			a.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/screens/"+id+".png", nil))
			if rec.Code != 200 {
				t.Fatal(rec.Code)
			}
		}
	}
	for _, mode := range n.pages() {
		rec := httptest.NewRecorder()
		a.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/preview?screen="+mode+"&page=1", nil))
		if rec.Code != 200 {
			t.Fatal(mode, rec.Code)
		}
	}
	if got := newsPageItems(items, 4); len(got) != 0 {
		t.Fatal("more than ten items", got)
	}
	n.items = items[:4]
	if len(n.pages()) != 2 || len(newsPageItems(n.snapshot(), 1)) != 1 {
		t.Fatal("partial feed")
	}
	n.items = nil
	if len(n.pages()) != 0 {
		t.Fatal("empty feed")
	}
	for _, id := range []string{"rss-0-20260913T1200Z", "rss-5-20260913T1200Z", "rss-2-bad"} {
		if _, err := a.image(id); err == nil {
			t.Fatal("invalid ID", id)
		}
	}
}

func TestNewsLatestTen(t *testing.T) {
	body := "<rss><channel>"
	for i := 1; i <= 12; i++ {
		body += fmt.Sprintf("<item><title>Story %d</title><link>https://example.com/%d</link><pubDate>2026-09-%02dT12:00:00Z</pubDate></item>", i, i, i)
	}
	body += "</channel></rss>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
	defer upstream.Close()
	n := newNews(upstream.URL)
	if err := n.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	items := n.snapshot()
	if len(items) != 10 || items[0].Title != "Story 12" || items[9].Title != "Story 3" {
		t.Fatal(items)
	}
}
