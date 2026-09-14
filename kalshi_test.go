package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestKalshiConfiguration(t *testing.T) {
	c := defaultConfiguration()
	k, err := newKalshi(c.KalshiPages)
	if err != nil || len(k.pages()) != 4 {
		t.Fatal(k, err)
	}
	p, _ := newPolymarket(c.PolymarketPages)
	a := &app{slideOrder: []string{"polymarket", "kalshi"}, polymarket: p, kalshi: k}
	slides := a.playlist()
	if len(slides) != 7 || slides[3] != k.pages()[0].ID || slides[6] != k.pages()[3].ID {
		t.Fatal(slides)
	}
	for _, raw := range []string{"http://kalshi.com/markets/a/b/a-c", "https://evil.test/markets/a/b/a-c", "https://kalshi.com/markets/a/b/c", "https://kalshi.com/markets/a/../a-c", "series:", "series:A/B"} {
		if _, err := parseKalshiPage(raw); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	c.KalshiPages = []string{defaultKalshiPages()[0], strings.ToUpper("series:kxnetflixrankmovie")}
	if c.validate() == nil {
		t.Fatal("accepted invalid URL")
	}
	if _, err := newKalshi([]string{"series:foo", "series:FOO"}); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestKalshiWeeklyRollover(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	event := func(ticker string, close time.Time, status string) kalshiEvent {
		return kalshiEvent{Ticker: ticker, Series: "TEST", Title: "Weekly", Markets: []kalshiMarket{{Status: status, CloseTime: close}}}
	}
	events := []kalshiEvent{
		event("future", now.Add(8*24*time.Hour), "active"),
		event("old", now.Add(-24*time.Hour), "active"),
		event("current", now.Add(24*time.Hour), "active"),
		event("settled", now.Add(time.Hour), "settled"),
	}
	got, err := currentKalshiEvent(events, "TEST", now)
	if err != nil || got.Ticker != "current" {
		t.Fatal(got, err)
	}
	got, err = currentKalshiEvent(events, "TEST", now.Add(2*24*time.Hour))
	if err != nil || got.Ticker != "future" {
		t.Fatal(got, err)
	}
	if _, err = currentKalshiEvent(events, "TEST", now.Add(9*24*time.Hour)); err == nil {
		t.Fatal("stale week selected")
	}
	if _, err = currentKalshiEvent(events, "OTHER", now); err == nil {
		t.Fatal("wrong series selected")
	}
}

func TestKalshiRows(t *testing.T) {
	event := kalshiEvent{Markets: []kalshiMarket{
		{YesTitle: "missing", Status: "active", LastPrice: "0.0", Volume: "0.00"},
		{YesTitle: "winner", Status: "settled", Result: "yes"},
		{YesTitle: "low", Status: "active", LastPrice: "0.1234", Volume: "3.00"},
		{YesTitle: "high", Status: "active", LastPrice: "0.8", Volume: "1.00"},
		{YesTitle: "invalid", Status: "active", LastPrice: "NaN", Volume: "1.00"},
	}}
	rows := kalshiRows(event)
	if rows[0].label != "high" || *rows[1].probability != 0.1234 || rows[2].probability != nil || rows[3].probability != nil || rows[4].status != "SETTLED" || *rows[4].probability != 1 {
		t.Fatalf("%+v", rows)
	}
}

func TestKalshiFetchAndRender(t *testing.T) {
	k, _ := newKalshi([]string{"series:TEST"})
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	event := kalshiEvent{Ticker: "TEST-WEEK", Series: "TEST", Title: "Top US Netflix Movie this week?", Subtitle: "The chart published on Sep 15, 2026", Markets: []kalshiMarket{{YesTitle: "A movie", Status: "active", LastPrice: "0.65", Volume: "20", CloseTime: now.Add(24 * time.Hour)}}}
	calls := 0
	fail := false
	k.client = &http.Client{Transport: weatherTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.elections.kalshi.com" || r.URL.Query().Get("series_ticker") != "TEST" || r.URL.Query().Get("status") != "open" {
			t.Fatal(r.URL)
		}
		if fail {
			return nil, fmt.Errorf("offline")
		}
		var data []byte
		if r.URL.Query().Get("cursor") == "" {
			data = []byte(`{"events":[],"cursor":"next"}`)
		} else {
			data, _ = json.Marshal(map[string]any{"events": []kalshiEvent{event}})
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	a, _ := newApp("http://trmnl.local", 60, "slideshow", []string{"Hello"}, bytes.Repeat([]byte{1}, 32))
	a.kalshi = k
	a.now = func() time.Time { return now }
	id, _ := a.screenID(k.pages()[0].ID, now)
	var data []byte
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		a.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/screens/"+id+"~b85n.png", nil))
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
			t.Fatal(rec.Code)
		}
		data = rec.Body.Bytes()
	}
	if calls != 4 || len(a.cache) != 0 {
		t.Fatal("cached", calls)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != width || img.Bounds().Dy() != height || data[24] != 4 || data[25] != 3 {
		t.Fatal("invalid PNG", err)
	}
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/preview?screen="+k.pages()[0].ID, nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), id) {
		t.Fatal("preview", rec.Code)
	}
	fail = true
	if _, err := a.image(id); err != nil {
		t.Fatal("outage", err)
	}
	if _, err := k.render(context.Background(), "kalshi-unknown-123", now); err != os.ErrNotExist {
		t.Fatal(err)
	}
}

func TestKalshiLive(t *testing.T) {
	dir := os.Getenv("KALSHI_LIVE_PREVIEW")
	if dir == "" {
		t.Skip("opt-in live API verification")
	}
	k, _ := newKalshi(defaultKalshiPages())
	for i, p := range k.pages() {
		event, err := k.fetch(context.Background(), p, time.Now())
		if err != nil {
			t.Fatal(p, err)
		}
		t.Log(p.Label, event.Ticker, event.Title, event.Subtitle, len(event.Markets))
		data, err := renderMarketPage("KALSHI", "Kalshi API", event.Title+" · "+event.Subtitle, kalshiRows(event), "LAST TRADE / SETTLEMENT", time.Now(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(fmt.Sprintf("%s/kalshi-%d.png", dir, i+1), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
