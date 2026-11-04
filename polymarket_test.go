package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPolymarketConfig(t *testing.T) {
	for _, s := range []string{"https://polymarket.com/event/example", "https://www.polymarket.com/event/example/market-one?tid=123", "https://polymarket.com/market/market-one", "series:best-ai-model", "series:mu-kpis:dram-revenue"} {
		if _, err := parsePolymarketPage(s); err != nil {
			t.Fatal(s, err)
		}
	}
	for _, s := range []string{"http://polymarket.com/event/example", "https://evil.test/event/example", "https://polymarket.com:443/event/example", "https://polymarket.com/event/../example", "https://polymarket.com/politics", "https://polymarket.com/event/a/b/c", "https://polymarket.com/event/%2Fbad", "series:", "series:unknown", "series:mu-kpis", "series:mu-kpis:nand-revenue", "series:best-ai-model:extra"} {
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

func polyOpenSeriesEvent(slug, title, end, series, seriesID string) polyEvent {
	yes := true
	description := "The leaderboard is checked on the specified date, 12:00 PM ET."
	if series == "mu-kpis" {
		date, _ := time.Parse(time.RFC3339, end)
		description = polyMicronRule(date.AddDate(0, 0, 7).Format("January 2, 2006") + ", 11:59 PM")
	}
	return polyEvent{Slug: slug, Title: title, EndDate: end, Active: true,
		Description: description,
		Series:      []polySeries{{ID: seriesID, Slug: series}},
		Markets: []polyMarket{{Slug: slug + "-one", Question: title, GroupItemTitle: "Option one", Active: true,
			AcceptingOrders: &yes, EndDate: end, Outcomes: json.RawMessage(`["Yes","No"]`), OutcomePrices: json.RawMessage(`["0.7","0.3"]`)}}}
}

func polyMicronRule(deadline string) string {
	return "This market resolves based on Micron's DRAM revenue for the specified fiscal quarter. " +
		"If the specified company does not release quarterly earnings materials for the specified quarter by " +
		deadline + " ET, this market will resolve to No."
}

func polySeriesTestClient(t *testing.T, p *polymarketService, events func(int) []polyEvent) *int {
	t.Helper()
	calls := new(int)
	p.client = &http.Client{Transport: weatherTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.URL.Host != "gamma-api.polymarket.com" {
			t.Fatalf("unexpected host %s", r.URL)
		}
		var data any
		switch r.URL.Path {
		case "/series":
			data = []polySeries{{ID: "123", Slug: r.URL.Query().Get("slug")}}
		case "/events":
			if r.URL.Query().Get("series_id") != "123" || r.URL.Query().Get("closed") != "false" || r.URL.Query().Get("active") != "true" || r.URL.Query().Get("limit") != "20" {
				t.Fatalf("unexpected discovery query %s", r.URL)
			}
			offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
			if err != nil {
				t.Fatal(err)
			}
			data = events(offset)
		default:
			t.Fatalf("unexpected path %s", r.URL)
		}
		body, _ := json.Marshal(data)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}, nil
	})}
	return calls
}

func TestPolymarketSeriesFiltersAndPagination(t *testing.T) {
	p, _ := newPolymarket([]string{"series:mu-kpis:dram-revenue"})
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	dram := polyOpenSeriesEvent("micron-dram-current", "Will Micron Q1 DRAM revenue be above __?", "2026-12-23T23:00:00Z", "mu-kpis", "123")
	wrong := polyOpenSeriesEvent("micron-nand-current", "Will Micron Q1 NAND revenue be above __?", "2026-12-20T23:00:00Z", "mu-kpis", "123")
	old := polyOpenSeriesEvent("micron-dram-old", "Will Micron Q4 DRAM revenue be above __?", "2026-10-01T23:00:00Z", "mu-kpis", "123")
	otherSeries := dram
	otherSeries.Slug, otherSeries.Series = "wrong-series", []polySeries{{ID: "999", Slug: "mu-kpis"}}
	closed := dram
	closed.Slug, closed.Closed = "closed-event", true
	unopened := dram
	unopened.Slug, unopened.StartDate = "unopened-event", "2026-12-01T00:00:00Z"
	later := dram
	later.Slug, later.EndDate = "micron-dram-next", "2027-03-23T23:00:00Z"
	invalid := dram
	invalid.Slug, invalid.EndDate = "invalid-date", ""
	noOrders := dram
	noOrders.Slug = "not-trading"
	noOrders.Markets = []polyMarket{{Active: true}}
	first := []polyEvent{wrong, old, otherSeries, closed, unopened, invalid, noOrders}
	for len(first) < 20 {
		filler := wrong
		filler.Slug = fmt.Sprintf("nand-%d", len(first))
		first = append(first, filler)
	}
	calls := polySeriesTestClient(t, p, func(offset int) []polyEvent {
		if offset == 0 {
			return first
		}
		if offset != 20 {
			t.Fatalf("unexpected offset %d", offset)
		}
		return []polyEvent{later, dram}
	})
	event, err := p.fetchAt(context.Background(), p.pages()[0], now)
	if err != nil || event.Slug != dram.Slug || *calls != 3 {
		t.Fatalf("selected %s, calls %d: %v", event.Slug, *calls, err)
	}
	// A closed contract in an otherwise live event cannot leak old odds.
	dram.Markets = append(dram.Markets, polyMarket{Slug: "closed", Active: true, Closed: true})
	event, err = p.fetchAt(context.Background(), p.pages()[0], now)
	if err != nil || len(event.Markets) != 1 {
		t.Fatalf("closed market retained: %+v, %v", event, err)
	}
}

func TestPolymarketBestModelObservationAndRollover(t *testing.T) {
	p, _ := newPolymarket([]string{"series:best-ai-model"})
	first := polyOpenSeriesEvent("best-ai-model-on-november-9", "Best AI model on November 9?", "2026-11-10T04:59:00Z", "best-ai-model", "123")
	second := polyOpenSeriesEvent("best-ai-model-on-november-16", "Best AI model on November 16?", "2026-11-17T04:59:00Z", "best-ai-model", "123")
	calls := polySeriesTestClient(t, p, func(int) []polyEvent { return []polyEvent{second, first} })
	before := time.Date(2026, 11, 9, 16, 59, 0, 0, time.UTC)
	event, err := p.fetchAt(context.Background(), p.pages()[0], before)
	if err != nil || event.Slug != first.Slug {
		t.Fatal("before noon", event.Slug, err)
	}
	after := before.Add(time.Minute)
	event, err = p.fetchAt(context.Background(), p.pages()[0], after)
	if err != nil || event.Slug != second.Slug {
		t.Fatal("did not roll at noon ET", event.Slug, err)
	}
	// Even the same old image URL must rediscover the current observation.
	id := p.pages()[0].ID + "-" + strconv.FormatInt(before.UnixNano(), 10)
	oldImage, err := p.render(context.Background(), id, before)
	if err != nil {
		t.Fatal(err)
	}
	newImage, err := p.render(context.Background(), id, after)
	if err != nil || bytes.Equal(oldImage, newImage) || *calls != 8 {
		t.Fatal("old image URL was reused", err, *calls)
	}
	for _, data := range [][]byte{oldImage, newImage} {
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	// Before the US fall DST transition, noon ET is 16:00 UTC.
	october := polyOpenSeriesEvent("best-ai-model-on-october-26", "Best AI model on October 26?", "2026-10-27T03:59:00Z", "best-ai-model", "123")
	cutoff, err := polyObservationTime(p.pages()[0], october)
	if err != nil || cutoff.UTC().Hour() != 16 || cutoff.Day() != 26 {
		t.Fatal("DST cutoff", cutoff, err)
	}
	october.Description = "The leaderboard is checked at 4:00 PM ET."
	if _, err := polyObservationTime(p.pages()[0], october); err == nil {
		t.Fatal("accepted a changed resolution rule")
	}
}

func TestPolymarketMicronDelayedEarningsAndDeadline(t *testing.T) {
	p, _ := newPolymarket([]string{"series:mu-kpis:dram-revenue"})
	current := polyOpenSeriesEvent("micron-q1-dram", "Will Micron Q1 DRAM revenue be above __?", "2026-12-23T23:00:00Z", "mu-kpis", "123")
	current.Description = polyMicronRule("January 31, 2027, 11:59 PM")
	current.Markets[0].Description = current.Description
	future := polyOpenSeriesEvent("micron-q2-dram", "Will Micron Q2 DRAM revenue be above __?", "2027-03-23T23:00:00Z", "mu-kpis", "123")
	future.Description = polyMicronRule("April 30, 2027, 11:59 PM")
	future.Markets[0].Description = future.Description
	polySeriesTestClient(t, p, func(int) []polyEvent { return []polyEvent{future, current} })
	// Both event and contract endDate are past, but the publication deadline is
	// still ahead and trading remains open. Keep the current quarter first.
	delayed := time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC)
	event, err := p.fetchAt(context.Background(), p.pages()[0], delayed)
	if err != nil || event.Slug != current.Slug {
		t.Fatal("dropped delayed earnings before rule deadline", event.Slug, err)
	}
	deadline, err := polyMicronPublicationDeadline(current.Description)
	if err != nil || !deadline.Equal(time.Date(2027, 2, 1, 4, 59, 0, 0, time.UTC)) {
		t.Fatal("incorrect ET deadline", deadline, err)
	}
	for _, test := range []struct {
		name string
		now  time.Time
		want string
	}{{"just before", deadline.Add(-time.Second), current.Slug}, {"at deadline", deadline, future.Slug}} {
		t.Run(test.name, func(t *testing.T) {
			event, err := p.fetchAt(context.Background(), p.pages()[0], test.now)
			if err != nil || event.Slug != test.want {
				t.Fatal(event.Slug, err)
			}
		})
	}
	current.Markets[0].Closed = true
	event, err = p.fetchAt(context.Background(), p.pages()[0], delayed)
	if err != nil || event.Slug != future.Slug {
		t.Fatal("retained closed current-quarter contract", event.Slug, err)
	}
	current.Markets[0].Closed = false
	current.Closed = true
	event, err = p.fetchAt(context.Background(), p.pages()[0], delayed)
	if err != nil || event.Slug != future.Slug {
		t.Fatal("retained closed current-quarter event", event.Slug, err)
	}
	current.Closed = false
	current.Markets[0].Description = polyMicronRule("January 10, 2027, 11:59 PM")
	event, err = p.fetchAt(context.Background(), p.pages()[0], delayed)
	if err != nil || event.Slug != future.Slug {
		t.Fatal("ignored an earlier market-specific deadline", event.Slug, err)
	}
	// No next quarter should mean pending after the stated deadline, even if
	// the old API records continue to claim active/open/accepting orders.
	polySeriesTestClient(t, p, func(int) []polyEvent { return []polyEvent{current} })
	if _, err := p.fetchAt(context.Background(), p.pages()[0], deadline); !errors.Is(err, errPolyNextEventPending) {
		t.Fatal("expected pending after publication deadline", err)
	}
	for _, rule := range []string{"", "Micron's DRAM revenue will be reported later.", polyMicronRule("January 32, 2027, 11:59 PM"), current.Description + current.Description} {
		if _, err := polyMicronPublicationDeadline(rule); err == nil {
			t.Fatal("accepted invalid or ambiguous rule", rule)
		}
	}
	// A summer publication deadline follows EDT rather than fixed EST.
	summer, err := polyMicronPublicationDeadline(polyMicronRule("July 31, 2027, 11:59 PM"))
	if err != nil || summer.UTC().Hour() != 3 {
		t.Fatal("summer deadline offset", summer, err)
	}
}

func TestPolymarketSeriesPendingAndFailures(t *testing.T) {
	p, _ := newPolymarket([]string{"series:best-ai-model"})
	now := time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC)
	old := polyOpenSeriesEvent("best-ai-model-on-november-9", "Best AI model on November 9?", "2026-11-10T04:59:00Z", "best-ai-model", "123")
	polySeriesTestClient(t, p, func(int) []polyEvent { return []polyEvent{old} })
	if _, err := p.fetchAt(context.Background(), p.pages()[0], now); !errors.Is(err, errPolyNextEventPending) {
		t.Fatal("expected pending after observation", err)
	}
	data, err := p.render(context.Background(), p.pages()[0].ID+"-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	// A provider ignoring pagination cannot cause an unbounded scan.
	full := make([]polyEvent, 20)
	for i := range full {
		full[i] = old
	}
	calls := polySeriesTestClient(t, p, func(int) []polyEvent { return full })
	if _, err := p.fetchAt(context.Background(), p.pages()[0], now); err == nil || !strings.Contains(err.Error(), "page limit") || *calls != 11 {
		t.Fatal("pagination was not bounded", err, *calls)
	}
	p.client = &http.Client{Transport: weatherTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})}
	if _, err := p.fetchAt(context.Background(), p.pages()[0], now); err == nil {
		t.Fatal("API failure accepted")
	}
}

func TestPolymarketSeriesLive(t *testing.T) {
	dir := os.Getenv("POLYMARKET_SERIES_LIVE_PREVIEW_DIR")
	if dir == "" {
		t.Skip("opt-in live recurring-page verification")
	}
	for i, selector := range []string{"series:best-ai-model", "series:mu-kpis:dram-revenue"} {
		p, err := newPolymarket([]string{selector})
		if err != nil {
			t.Fatal(err)
		}
		event, err := p.fetch(context.Background(), p.pages()[0])
		if err != nil {
			t.Fatal(selector, err)
		}
		t.Log(selector, "selected", event.Slug, "contracts", len(event.Markets))
		data, err := renderPolymarket(event, time.Now(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("polymarket-series-%d.png", i+1)), data, 0600); err != nil {
			t.Fatal(err)
		}
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
