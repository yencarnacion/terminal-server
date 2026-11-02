package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func directionEvent(series, day string) kalshiEvent {
	location, _ := time.LoadLocation("America/New_York")
	target, err := time.ParseInLocation("2006-01-02 15:04", day+" 16:00", location)
	if err != nil {
		panic(err)
	}
	strike := 28384.85
	return kalshiEvent{Ticker: series + "-" + day, Series: series, Title: "Daily Up/Down", StrikeDate: target,
		Markets: []kalshiMarket{{Status: "active", MarketType: "binary", StrikeType: "greater", FloorStrike: &strike,
			OpenTime: target.AddDate(0, 0, -7), CloseTime: target, LastPrice: "0.72", Volume: "402.94"}}}
}

func TestKalshiDirectionRollover(t *testing.T) {
	series := "KXNASDAQDUD"
	for _, dates := range [][2]string{
		{"2026-11-02", "2026-11-03"}, // Ordinary trading day, EST.
		{"2026-10-30", "2026-11-02"}, // Friday to Monday, across DST.
		{"2026-11-25", "2026-11-27"}, // Skip the unlisted holiday.
	} {
		t.Run(dates[0], func(t *testing.T) {
			current, next := directionEvent(series, dates[0]), directionEvent(series, dates[1])
			cutoff := current.StrikeDate
			// A delayed API close must not keep today's odds on screen after 4 PM.
			current.Markets[0].CloseTime = cutoff.Add(24 * time.Hour)
			events := []kalshiEvent{next, current, directionEvent("OTHER", dates[0])}
			for _, step := range []struct {
				now  time.Time
				want string
			}{
				{cutoff.Add(-time.Second).UTC(), current.Ticker},
				{cutoff.UTC(), next.Ticker},
				{cutoff.Add(time.Hour).UTC(), next.Ticker},
			} {
				got, err := currentKalshiDirectionEvent(events, series, step.now)
				if err != nil || got.Ticker != step.want {
					t.Fatal(step.now, got.Ticker, step.want, err)
				}
			}
			if _, err := currentKalshiDirectionEvent(events[1:], series, cutoff); err != errKalshiDirectionPending {
				t.Fatal("must wait for next day, not reuse expired odds", err)
			}
			next.Markets[0].OpenTime = cutoff.Add(time.Hour)
			next.Markets[0].Status = "initialized"
			card, err := directionFromEvent(next, cutoff)
			if err != nil || !card.target.Equal(next.StrikeDate) || card.up != nil || card.message != "TRADING NOT OPEN YET" {
				t.Fatal("next day's listing should appear with pending odds before trading opens", card, err)
			}
		})
	}
}

func TestKalshiDirectionRulesAndPrices(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*kalshiEvent)
		bad    bool
		price  bool
	}{
		{"valid", func(e *kalshiEvent) {}, false, true},
		{"zero traded", func(e *kalshiEvent) { e.Markets[0].LastPrice = "0" }, false, true},
		{"one traded", func(e *kalshiEvent) { e.Markets[0].LastPrice = "1" }, false, true},
		{"untraded", func(e *kalshiEvent) { e.Markets[0].Volume = "0" }, false, false},
		{"missing price", func(e *kalshiEvent) { e.Markets[0].LastPrice = "" }, false, false},
		{"nan price", func(e *kalshiEvent) { e.Markets[0].LastPrice = "NaN" }, false, false},
		{"out of range", func(e *kalshiEvent) { e.Markets[0].LastPrice = "1.1" }, false, false},
		{"infinite volume", func(e *kalshiEvent) { e.Markets[0].Volume = "+Inf" }, false, false},
		{"unknown level", func(e *kalshiEvent) { e.Markets[0].FloorStrike = nil }, true, false},
		{"invalid level", func(e *kalshiEvent) { *e.Markets[0].FloorStrike = math.NaN() }, true, false},
		{"wrong rule", func(e *kalshiEvent) { e.Markets[0].StrikeType = "less" }, true, false},
		{"wrong series", func(e *kalshiEvent) { e.Series = "OTHER" }, true, false},
		{"multiple markets", func(e *kalshiEvent) { e.Markets = append(e.Markets, e.Markets[0]) }, true, false},
		{"settled", func(e *kalshiEvent) { e.Markets[0].Result = "yes" }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := directionEvent("KXNASDAQDUD", "2026-11-02")
			tc.mutate(&event)
			card, err := directionFromEvent(event, event.StrikeDate.Add(-time.Hour))
			if (err != nil) != tc.bad || (card.up != nil) != tc.price {
				t.Fatal(card, err)
			}
		})
	}
	for _, tc := range []struct {
		value           float64
		up, down, label string
	}{
		{0.72, "72%", "28%", "UP MORE LIKELY"},
		{0.5, "50%", "50%", "ABOUT EVEN"},
		{0.12, "12%", "88%", "DOWN / FLAT MORE LIKELY"},
		{0.001, "<1%", ">99%", "DOWN / FLAT MORE LIKELY"},
		{0.999, ">99%", "<1%", "UP MORE LIKELY"},
	} {
		up, down, label := directionPercentages(tc.value)
		if up != tc.up || down != tc.down || label != tc.label {
			t.Fatal(tc, up, down, label)
		}
	}
}

func TestKalshiDirectionDisplay(t *testing.T) {
	k, _ := newKalshi([]string{"daily-direction", "series:TEST"})
	event := directionEvent("KXNASDAQDUD", "2026-11-02")
	now := event.StrikeDate.Add(-time.Hour)
	var calls atomic.Int32
	var fail atomic.Bool
	var pending atomic.Bool
	k.client = &http.Client{Transport: weatherTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		series := r.URL.Query().Get("series_ticker")
		if !isKalshiDirectionSeries(series) || r.URL.Query().Get("status") != "" || r.URL.Query().Get("min_close_ts") == "" {
			t.Errorf("unexpected market request: %s", r.URL)
		}
		if fail.Load() && series == "KXINXDUD" {
			return nil, fmt.Errorf("offline")
		}
		events := []kalshiEvent{directionEvent(series, "2026-11-02"), directionEvent(series, "2026-11-03")}
		if pending.Load() {
			events = events[:1]
		}
		data, _ := json.Marshal(map[string]any{"events": events})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	a, _ := newApp("http://trmnl.local", 300, "slideshow", []string{"Hello"}, bytes.Repeat([]byte{1}, 32))
	a.kalshi, a.slideOrder = k, []string{"kalshi", "quote"}
	a.now = func() time.Time { return now }
	routes := a.routes()
	req := httptest.NewRequest("GET", "/api/display", nil)
	req.Header.Set("Access_Token", a.token("aa:bb:cc:dd:ee:ff"))
	req.Header.Set("Percent_Charged", "85")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	var display struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &display); err != nil || rec.Code != 200 || !strings.HasPrefix(display.Filename, kalshiDirectionID+"-") || !strings.HasSuffix(display.Filename, "~b85n") {
		t.Fatal(rec.Code, rec.Body.String(), err)
	}
	if calls.Load() != 0 {
		t.Fatal("display metadata must not fetch or render live markets")
	}
	rec = httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest("GET", "/preview?screen="+kalshiDirectionID, nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Kalshi Stock Direction") {
		t.Fatal("missing preview", rec.Code)
	}
	var before []byte
	for i := 0; i < 2; i++ {
		rec = httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest("GET", "/screens/"+display.Filename+".png", nil))
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
			t.Fatal(rec.Code, rec.Body.String())
		}
		img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
		if err != nil || img.Bounds().Dx() != width || img.Bounds().Dy() != height || rec.Body.Bytes()[24] != 4 || rec.Body.Bytes()[25] != 3 {
			t.Fatal("invalid device PNG", err)
		}
		before = append([]byte(nil), rec.Body.Bytes()...)
	}
	if calls.Load() != 4 || len(a.cache) != 0 {
		t.Fatal("live images cached", calls.Load(), len(a.cache))
	}
	now = event.StrikeDate // Exact 4 PM; even an old URL must fetch the next day.
	after, err := a.image(display.Filename)
	if err != nil || bytes.Equal(before, after) {
		t.Fatal("old URL did not roll forward", err)
	}
	cards := k.fetchDirections(context.Background(), now)
	if cards[0].target.Day() != 3 || cards[1].target.Day() != 3 {
		t.Fatal("next trading day not selected", cards)
	}
	fail.Store(true)
	cards = k.fetchDirections(context.Background(), now)
	if cards[0].up == nil || cards[1].up != nil || cards[1].message != "MARKET UNAVAILABLE" {
		t.Fatal("one outage hid both markets or displayed stale prices", cards)
	}
	if _, err := renderKalshiDirections(cards, now); err != nil {
		t.Fatal("partial outage render", err)
	}
	fail.Store(false)
	pending.Store(true)
	cards = k.fetchDirections(context.Background(), now)
	if cards[0].up != nil || cards[1].up != nil || cards[0].message != "NEXT MARKET PENDING" {
		t.Fatal("finished day reused", cards)
	}
	if _, err := renderKalshiDirections(cards, now); err != nil {
		t.Fatal("pending render", err)
	}
}

func TestKalshiDirectionLayoutStates(t *testing.T) {
	now := directionEvent("KXNASDAQDUD", "2026-11-02").StrikeDate.Add(-time.Hour)
	for _, price := range []string{"0", "0.001", "0.5", "0.999", "1", ""} {
		cards := make([]kalshiDirection, len(kalshiIndexes))
		for i, index := range kalshiIndexes {
			event := directionEvent(index.series, "2026-11-02")
			event.Markets[0].LastPrice = price
			cards[i], _ = directionFromEvent(event, now)
			cards[i].name, cards[i].description = index.name, index.description
		}
		data, err := renderKalshiDirections(cards, now)
		if err != nil {
			t.Fatal(price, err)
		}
		if dir := os.Getenv("KALSHI_LAYOUT_PREVIEW"); dir != "" {
			if err := os.WriteFile(dir+"/direction-"+price+".png", data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}
