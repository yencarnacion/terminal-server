package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFrontpagesSlideshowLiveFetch(t *testing.T) {
	fixture := image.NewGray(image.Rect(0, 0, 80, 120))
	for i := range fixture.Pix {
		fixture.Pix[i] = 220
	}
	fixture.SetGray(40, 60, color.Gray{Y: 0})
	var raw bytes.Buffer
	if err := png.Encode(&raw, fixture); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	date := "2026-09-12"
	broken := false
	catalogDown := false
	metadataReads, imageReads := 0, 0
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(r.Header.Get("Cache-Control"), "no-cache") {
			t.Error("upstream request must bypass caches")
		}
		switch r.URL.Path {
		case "/api/newspapers":
			if catalogDown {
				http.Error(w, "offline", 503)
				return
			}
			json.NewEncoder(w).Encode([]newspaper{{ID: "nyt", Name: "The New York Times"}, {ID: "end", Name: "El Nuevo Día"}})
		case "/api/newspapers/nyt/today", "/api/newspapers/end/today":
			metadataReads++
			id := strings.Split(r.URL.Path, "/")[3]
			if broken {
				http.Error(w, "unavailable", 503)
				return
			}
			json.NewEncoder(w).Encode(coverMetadata{ID: id, Date: date, ImageURL: "/images/" + date + "/" + id + ".png"})
		default:
			if strings.HasPrefix(r.URL.Path, "/images/") {
				imageReads++
				w.Write(raw.Bytes())
			} else {
				http.NotFound(w, r)
			}
		}
	}))
	defer source.Close()
	f, err := newFrontpages(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	slides := f.snapshot()
	if len(slides) != 2 || slides[0].PaperID != "nyt" || slides[1].PaperID != "end" {
		t.Fatal(slides)
	}
	mu.Lock()
	if metadataReads != 0 || imageReads != 0 {
		t.Error("catalog refresh must not prefetch/cache covers")
	}
	mu.Unlock()
	a, err := newApp("http://trmnl.local", 180, "slideshow", []string{"Keep going."}, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	a.frontpages = f
	now := time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	h := a.routes()
	expected := []string{a.ids[0], "calendar-20260912T2000Z", "quarter-20260912T2000Z", slides[0].ID, slides[1].ID, a.ids[0]}
	var firstCoverID string
	for _, want := range expected {
		r := httptest.NewRequest("GET", "/api/display", nil)
		r.Header.Set("ACCESS_TOKEN", a.token("aa:bb:cc:dd:ee:01"))
		r.Header.Set("PERCENT_CHARGED", "78")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var response struct {
			Filename string `json:"filename"`
			Refresh  int    `json:"refresh_rate"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || !strings.HasPrefix(response.Filename, want) || !strings.HasSuffix(response.Filename, "~b78n") || response.Refresh != 180 {
			t.Fatal(w.Code, w.Body.String())
		}
		imageReply := httptest.NewRecorder()
		h.ServeHTTP(imageReply, httptest.NewRequest("GET", "/screens/"+response.Filename+".png", nil))
		data := imageReply.Body.Bytes()
		if imageReply.Code != 200 || len(data) < 26 || data[24] != 4 || data[25] != 3 {
			t.Fatal("invalid device PNG")
		}
		if strings.HasPrefix(want, "cover-") {
			if !strings.Contains(imageReply.Header().Get("Cache-Control"), "no-store") {
				t.Fatal("cover response can be cached")
			}
			if firstCoverID == "" {
				firstCoverID = response.Filename
			}
			if _, ok := a.cache[response.Filename]; ok {
				t.Fatal("cover retained in rendered cache")
			}
		}
	}
	mu.Lock()
	if metadataReads != 2 || imageReads != 2 {
		t.Errorf("want exactly one live fetch per cover GET; got %d/%d", metadataReads, imageReads)
	}
	mu.Unlock()
	preview := httptest.NewRecorder()
	h.ServeHTTP(preview, httptest.NewRequest("GET", "/preview?screen="+slides[1].ID, nil))
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), "El Nuevo Día") {
		t.Fatal("cover preview missing")
	}
	before, err := a.image(firstCoverID)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	date = "2026-09-13"
	mu.Unlock()
	// Even reusing yesterday's URL must ask /today again and get the new edition.
	after, err := a.image(firstCoverID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("yesterday's cover was reused")
	}
	mu.Lock()
	if metadataReads != 4 || imageReads != 4 {
		t.Error("same cover URL was cached")
	}
	broken = true
	mu.Unlock()
	if _, err = a.image(firstCoverID); err == nil {
		t.Fatal("must not fall back to a stale cover during outage")
	}
	mu.Lock()
	catalogDown = true
	mu.Unlock()
	if err = f.refresh(context.Background()); err == nil {
		t.Fatal("expected catalog error")
	}
	if len(f.snapshot()) != 2 {
		t.Fatal("catalog outage should not remove newspaper configuration")
	}
}

func TestFrontpagesBoundaries(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("12345")) }))
	defer source.Close()
	f, err := newFrontpages(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.get(context.Background(), "/", 4); err == nil {
		t.Fatal("response limit not enforced")
	}
	if _, err = f.get(context.Background(), "http://elsewhere.invalid/cover.png", 100); err == nil {
		t.Fatal("cross-origin URL accepted")
	}
	if _, err = renderCover([]byte("not an image"), "Paper", "2026-09-12"); err == nil {
		t.Fatal("invalid image accepted")
	}
	var absent *frontpages
	if len(absent.snapshot()) != 0 {
		t.Fatal("disabled source")
	}
	if _, err = f.render(context.Background(), "cover-unknown"); err == nil {
		t.Fatal("unknown paper accepted")
	}
}
