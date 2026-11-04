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
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fearGreedTransport func(*http.Request) (*http.Response, error)

func (f fearGreedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fearGreedFixture(now time.Time) fearGreedSnapshot {
	current, close, week, month, year := 19.4, 20.1, 34.2, 38.4, 21.6
	return fearGreedSnapshot{Score: &current, Rating: "extreme fear", Timestamp: now.Add(-time.Hour),
		PreviousClose: &close, PreviousWeek: &week, PreviousMonth: &month, PreviousYear: &year}
}
func fearGreedBody(s fearGreedSnapshot) string {
	b, _ := json.Marshal(struct {
		FearAndGreed fearGreedSnapshot `json:"fear_and_greed"`
	}{s})
	return string(b)
}

func TestFearGreedSourceValidation(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	f := newFearGreed()
	body := fearGreedBody(fearGreedFixture(now))
	status := 200
	calls := 0
	f.client.Transport = fearGreedTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != fearGreedURL || r.Header.Get("User-Agent") == "" || r.Header.Get("Accept") != "application/json" {
			t.Error("unexpected CNN request", r.URL, r.Header)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	s, err := f.fetch(context.Background(), fearGreedURL, now)
	if err != nil || s.Score == nil || *s.Score != 19.4 || s.Rating != "extreme fear" {
		t.Fatalf("snapshot: %+v %v", s, err)
	}
	for _, v := range []float64{0, 100} {
		s := fearGreedFixture(now)
		s.Score = &v
		body = fearGreedBody(s)
		got, err := f.fetch(context.Background(), fearGreedURL, now)
		if err != nil || *got.Score != v {
			t.Fatalf("valid endpoint score %v: %v", v, err)
		}
	}
	for _, bad := range []string{
		`{}`, `{"fear_and_greed":null}`, `{"fear_and_greed":{"score":null}}`,
		`{"fear_and_greed":{"score":"19"}}`, `not json`, body + `{}`,
	} {
		body = bad
		if _, err := f.fetch(context.Background(), fearGreedURL, now); err == nil {
			t.Fatalf("accepted malformed payload: %.100s", bad)
		}
	}
	body = strings.Repeat(" ", (4<<20)+1)
	if _, err := f.fetch(context.Background(), fearGreedURL, now); err == nil {
		t.Fatal("accepted oversized response")
	}
	status = 418
	body = "unavailable"
	if _, err := f.fetch(context.Background(), fearGreedURL, now); err == nil || !strings.Contains(err.Error(), "418") {
		t.Fatal("missing upstream status", err)
	}
	before := calls
	for _, raw := range []string{"http://production.dataviz.cnn.io/index", "https://example.com/index", "https://production.dataviz.cnn.io.evil.test/index", "https://production.dataviz.cnn.io:444/index", "https://user@production.dataviz.cnn.io/index"} {
		if _, err := f.fetch(context.Background(), raw, now); err == nil {
			t.Fatal("accepted off-origin URL", raw)
		}
		u, _ := url.Parse(raw)
		if err := f.client.CheckRedirect(&http.Request{URL: u}, nil); err == nil {
			t.Fatal("accepted off-origin redirect", raw)
		}
	}
	if calls != before {
		t.Fatal("unapproved URL reached HTTP transport")
	}
	u, _ := url.Parse(fearGreedURL)
	if err := f.client.CheckRedirect(&http.Request{URL: u}, make([]*http.Request, 4)); err == nil {
		t.Fatal("unbounded redirects")
	}
	if err := f.client.CheckRedirect(&http.Request{URL: u}, make([]*http.Request, 1)); err != nil {
		t.Fatal("valid CNN redirect", err)
	}
}

func TestFearGreedMissingAndStaleValues(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		change func(*fearGreedSnapshot)
	}{
		{"missing score", func(s *fearGreedSnapshot) { s.Score = nil }},
		{"score below zero", func(s *fearGreedSnapshot) { v := -1.0; s.Score = &v }},
		{"score above 100", func(s *fearGreedSnapshot) { v := 100.1; s.Score = &v }},
		{"score NaN", func(s *fearGreedSnapshot) { v := math.NaN(); s.Score = &v }},
		{"score infinity", func(s *fearGreedSnapshot) { v := math.Inf(1); s.Score = &v }},
		{"missing rating", func(s *fearGreedSnapshot) { s.Rating = "" }},
		{"unknown rating", func(s *fearGreedSnapshot) { s.Rating = "very bullish" }},
		{"missing time", func(s *fearGreedSnapshot) { s.Timestamp = time.Time{} }},
		{"outdated time", func(s *fearGreedSnapshot) { s.Timestamp = now.Add(-fearGreedMaxAge - time.Second) }},
		{"future time", func(s *fearGreedSnapshot) { s.Timestamp = now.Add(16 * time.Minute) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fearGreedFixture(now)
			tc.change(&s)
			if err := s.validate(now); err == nil {
				t.Fatal("accepted invalid index")
			}
		})
	}
	s := fearGreedFixture(now)
	s.Timestamp = now.Add(-4 * 24 * time.Hour)
	if err := s.validate(now); err != nil {
		t.Fatal("rejected plausible long-weekend reading", err)
	}
	zero, invalid := 0.0, 101.0
	if fearGreedScoreLabel(nil) != "—" || fearGreedScoreLabel(&invalid) != "—" || fearGreedScoreLabel(&zero) != "0" {
		t.Fatal("missing comparisons became zero or invalid values displayed")
	}
}

func TestFearGreedCacheRetryAndConcurrentRequests(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	f := newFearGreed()
	var calls atomic.Int32
	var fail atomic.Bool
	f.client.Transport = fearGreedTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, fmt.Errorf("CNN unavailable")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fearGreedBody(fearGreedFixture(now)))), Header: http.Header{}}, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.snapshot(context.Background(), now); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("concurrent previews fetched CNN repeatedly", calls.Load())
	}
	if _, err := f.snapshot(context.Background(), now.Add(4*time.Minute)); err != nil || calls.Load() != 1 {
		t.Fatal("cache miss", err, calls.Load())
	}
	fail.Store(true)
	if s, err := f.snapshot(context.Background(), now.Add(5*time.Minute)); err == nil || s.Score != nil {
		t.Fatal("served cached score on failed refresh")
	}
	before := calls.Load()
	if _, err := f.snapshot(context.Background(), now.Add(6*time.Minute)); err == nil || calls.Load() != before {
		t.Fatal("retry backoff missing")
	}
	fail.Store(false)
	if _, err := f.snapshot(context.Background(), now.Add(7*time.Minute)); err != nil || calls.Load() != before+1 {
		t.Fatal("did not recover", err)
	}
	// Freshly fetched cached bytes must still pass source-age validation.
	f.data.Timestamp = now.Add(-fearGreedMaxAge + time.Minute)
	f.fetched = now
	f.attempted = time.Time{}
	if _, err := f.snapshot(context.Background(), now.Add(2*time.Minute)); err != nil || calls.Load() != before+2 {
		t.Fatal("stale cached source did not trigger refresh", err)
	}
}

func TestFearGreedRender(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	for _, unavailable := range []bool{false, true} {
		s := fearGreedFixture(now)
		var fetchErr error
		if unavailable {
			s = fearGreedSnapshot{}
			fetchErr = fmt.Errorf("offline")
		}
		data, err := renderFearGreed(now, s, fetchErr)
		if err != nil {
			t.Fatal(err)
		}
		if data[24] != 4 || data[25] != 3 {
			t.Fatal("not 4-bit indexed PNG")
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil || img.Bounds().Dx() != width || img.Bounds().Dy() != height {
			t.Fatal("invalid display image", err)
		}
		if path := os.Getenv("FEAR_GREED_TEST_PREVIEW"); path != "" && !unavailable {
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestFearGreedLive(t *testing.T) {
	path := os.Getenv("FEAR_GREED_LIVE_PREVIEW")
	if path == "" {
		t.Skip("set FEAR_GREED_LIVE_PREVIEW for an opt-in CNN network check and rendered preview")
	}
	f := newFearGreed()
	now := time.Now()
	s, err := f.snapshot(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	data, err := renderFearGreed(now, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
