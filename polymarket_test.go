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

func TestPolymarketConfig(t *testing.T) {
	for _, s := range []string{"https://polymarket.com/event/example", "https://www.polymarket.com/event/example/market-one?tid=123", "https://polymarket.com/market/market-one"} {
		if _, err := parsePolymarketPage(s); err != nil {
			t.Fatal(s, err)
		}
	}
	for _, s := range []string{"http://polymarket.com/event/example", "https://evil.test/event/example", "https://polymarket.com:443/event/example", "https://polymarket.com/event/../example", "https://polymarket.com/politics", "https://polymarket.com/event/a/b/c", "https://polymarket.com/event/%2Fbad"} {
		if _, err := parsePolymarketPage(s); err == nil {
			t.Fatal("accepted", s)
		}
	}
	c := defaultConfiguration()
	c.PolymarketPages = []string{"https://polymarket.com/event/one", "", "https://polymarket.com/event/two"}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	p, err := newPolymarket(c.PolymarketPages)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{slideOrder: []string{"newspapers", "polymarket"}, polymarket: p, frontpages: &frontpages{slides: []coverSlide{{ID: "paper"}}}}
	got := a.playlist()
	if len(got) != 3 || got[0] != "paper" || got[1] != p.pages()[0].ID || got[2] != p.pages()[1].ID {
		t.Fatal(got)
	}
	c.PolymarketPages = append(c.PolymarketPages, "")
	if c.validate() == nil {
		t.Fatal("accepted 4 slots")
	}
	c.PolymarketPages = []string{"https://polymarket.com/event/one", "https://www.polymarket.com/event/one?tid=2"}
	if c.validate() == nil {
		t.Fatal("accepted duplicates")
	}
}
func polyFixture() polyEvent {
	return polyEvent{Title: "Which outcome will happen by the end of the year?", Markets: []polyMarket{
		{Slug: "one", Question: "Outcome one?", GroupItemTitle: "Outcome one", Active: true, Outcomes: json.RawMessage(`"[\"No\",\"Yes\"]"`), OutcomePrices: json.RawMessage(`"[\"0.25\",\"0.75\"]"`)},
		{Slug: "two", Question: "Outcome two?", Active: true, Outcomes: json.RawMessage(`["Yes","No"]`), OutcomePrices: json.RawMessage(`[0.2,0.8]`)},
		{Slug: "three", Question: "Outcome three?", Closed: true, Outcomes: json.RawMessage(`["Yes","No"]`), OutcomePrices: json.RawMessage(`["1","0"]`)},
	}}
}
func TestPolymarketRows(t *testing.T) {
	f := polyFixture()
	rows := polyRows(f)
	if len(rows) != 3 || rows[0].probability == nil || *rows[0].probability != 0.75 || rows[2].status != "CLOSED" {
		t.Fatalf("%+v", rows)
	}
	f.Markets = f.Markets[:1]
	rows = polyRows(f)
	if len(rows) != 2 || rows[0].label != "Yes" {
		t.Fatal(rows)
	}
	f.Markets[0].OutcomePrices = json.RawMessage(`"[\"NaN\",\"1.2\"]"`)
	for _, r := range polyRows(f) {
		if r.probability != nil {
			t.Fatal("invalid price accepted")
		}
	}
}
func TestPolymarketFetchAndRender(t *testing.T) {
	p, _ := newPolymarket([]string{"https://polymarket.com/event/test-event"})
	calls := 0
	fail := false
	p.client = &http.Client{Transport: weatherTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "gamma-api.polymarket.com" || r.URL.Path != "/events/slug/test-event" {
			t.Fatal(r.URL)
		}
		if fail {
			return nil, fmt.Errorf("offline")
		}
		data, _ := json.Marshal(polyFixture())
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	a, err := newApp("http://trmnl.local", 60, "slideshow", []string{"Hello"}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	a.polymarket = p
	a.slideOrder = []string{"polymarket"}
	now := time.Date(2026, 9, 12, 22, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	id, _ := a.screenID(p.pages()[0].ID, now)
	var data []byte
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		a.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/screens/"+id+"~b85n.png", nil))
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
			t.Fatal(rec.Code)
		}
		data = rec.Body.Bytes()
	}
	if calls != 2 || len(a.cache) != 0 {
		t.Fatal("cached Polymarket", calls)
	}
	selected := p.pages()[0]
	selected.Market = "two"
	event, err := p.fetch(context.Background(), selected)
	if err != nil || len(event.Markets) != 1 || event.Markets[0].Slug != "two" {
		t.Fatal("market selection", event, err)
	}
	selected.Market = "missing"
	if _, err := p.fetch(context.Background(), selected); err == nil {
		t.Fatal("missing selected market accepted")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != width || img.Bounds().Dy() != height || data[24] != 4 || data[25] != 3 {
		t.Fatal("invalid image", err)
	}
	if path := os.Getenv("POLYMARKET_TEST_PREVIEW"); path != "" {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/preview?screen="+p.pages()[0].ID, nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), id) {
		t.Fatal("preview", rec.Code)
	}
	fail = true
	data, err = a.image(id)
	if err != nil {
		t.Fatal("outage must render", err)
	}
	if _, err = png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err = p.render(context.Background(), "poly-unknown-123", now); err != os.ErrNotExist {
		t.Fatal(err)
	}
}
func TestPolymarketLive(t *testing.T) {
	path := os.Getenv("POLYMARKET_LIVE_PREVIEW")
	if path == "" {
		t.Skip("opt-in live API verification")
	}
	p, _ := newPolymarket([]string{"https://polymarket.com/event/kraken-ipo-in-2025"})
	event, err := p.fetch(context.Background(), p.pages()[0])
	if err != nil {
		t.Fatal(err)
	}
	data, err := renderPolymarket(event, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
