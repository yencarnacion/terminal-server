package main

import (
	"bytes"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFearGreedDeviceAndPreview(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	a, err := newApp("http://trmnl.local", 300, "slideshow", []string{"Hello"}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return now }
	a.fearGreed = newFearGreed()
	a.slideOrder = []string{"fear-greed", "quote"}
	calls := 0
	fail := false
	a.fearGreed.client.Transport = fearGreedTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if fail {
			return nil, fmt.Errorf("upstream unavailable")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fearGreedBody(fearGreedFixture(now))))}, nil
	})
	h := a.routes()
	preview := httptest.NewRecorder()
	h.ServeHTTP(preview, httptest.NewRequest("GET", "/preview?screen=fear-greed", nil))
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), "CNN Fear &amp; Greed") || !strings.Contains(preview.Body.String(), "fear-greed-20261104T1500Z.png") {
		t.Fatal("missing preview", preview.Code, preview.Body.String())
	}
	r := httptest.NewRequest("GET", "/api/display", nil)
	r.Header.Set("Access-Token", a.token("aa:bb:cc:dd:ee:ff"))
	r.Header.Set("Percent-Charged", "85")
	id, err := a.nextDisplay(r)
	if err != nil || !strings.HasPrefix(id, "fear-greed-") || calls != 0 {
		t.Fatal("display metadata should schedule CNN without fetching it", id, err, calls)
	}
	imageURL := "/screens/" + id + ".png"
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", imageURL, nil))
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("live image route", rec.Code, rec.Header())
		}
		img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
		if err != nil || img.Bounds().Dx() != width || img.Bounds().Dy() != height {
			t.Fatal("invalid device image", err)
		}
	}
	if calls != 1 || len(a.cache) != 0 {
		t.Fatal("CNN data should be cached briefly, not rendered images", calls, len(a.cache))
	}
	// An old image URL must use current data and continue the slideshow on failure.
	now = now.Add(6 * time.Minute)
	fail = true
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", imageURL, nil))
	if rec.Code != 200 || calls != 2 {
		t.Fatal("old URL did not attempt a fresh CNN reading", rec.Code, calls)
	}
	if _, err := png.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil {
		t.Fatal("outage must still produce a displayable PNG", err)
	}
	next, err := a.nextDisplay(r)
	if err != nil || strings.HasPrefix(next, "fear-greed-") {
		t.Fatal("outage stopped slideshow progression", next, err)
	}
	for _, path := range []string{"/screens/fear-greed-invalid.png", "/screens/fear-greed-20261104T1500Z~bad.png"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 404 {
			t.Fatal("accepted invalid image ID", path, rec.Code)
		}
	}
	a.fearGreed = nil
	if got := a.playlist(); !reflect.DeepEqual(got, []string{"cowsay"}) {
		t.Fatal("disabled CNN page remains in playlist", got)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/preview?screen=fear-greed", nil))
	if rec.Code != 400 {
		t.Fatal("disabled CNN preview accepted", rec.Code)
	}
}

func TestNewDashboardConfiguration(t *testing.T) {
	for _, setting := range []string{"screen: fear-greed\nfear_greed_enabled: true", "slide_order: [fear-greed, polymarket, kalshi]"} {
		cfg, err := readConfiguration([]string{"--config", configFile(t, setting)}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := newApp(cfg.BaseURL, cfg.SlideSeconds, cfg.Screen, []string{"hello"}, bytes.Repeat([]byte{1}, 32)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := readConfiguration([]string{"--config", configFile(t, "screen: fear-greed\nfear_greed_enabled: false")}, io.Discard); err == nil {
		t.Fatal("standalone CNN screen accepted while disabled")
	}
	pages := append(defaultKalshiPages(), "series:UNEXPECTED_EIGHTH_PAGE")
	if _, err := newKalshi(pages); err == nil {
		t.Fatal("Kalshi page limit not enforced")
	}
}

func TestScrapflyConfiguration(t *testing.T) {
	for _, endpoint := range []string{"ftp://example.com/mcp", "/mcp", "http://user:password@example.com/mcp", "https://example.com/mcp?token=secret", "https://example.com/mcp#fragment"} {
		if _, err := readConfiguration([]string{"--config", configFile(t, "scrapfly_mcp_url: "+endpoint)}, io.Discard); err == nil {
			t.Fatal("accepted invalid MCP endpoint", endpoint)
		}
	}
	cfg, err := readConfiguration([]string{"--config", configFile(t, "scrapfly_mcp_url: ''")}, io.Discard)
	if err != nil || cfg.ScrapflyMCPURL != "" {
		t.Fatal("cannot disable MCP fallback", cfg.ScrapflyMCPURL, err)
	}
}
