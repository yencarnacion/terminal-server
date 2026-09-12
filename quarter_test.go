package main

import (
	"bytes"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQuarterDates(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		date                           string
		quarter, total, complete, left int
	}{
		{"2026-01-01", 1, 90, 0, 90},
		{"2024-02-29", 1, 91, 59, 32},
		{"2024-03-31", 1, 91, 90, 1},
		{"2026-04-01", 2, 91, 0, 91},
		{"2026-06-30", 2, 91, 90, 1},
		{"2026-07-01", 3, 92, 0, 92},
		{"2026-09-12", 3, 92, 73, 19},
		{"2026-09-30", 3, 92, 91, 1},
		{"2026-10-01", 4, 92, 0, 92},
		{"2026-12-31", 4, 92, 91, 1},
		{"2027-01-01", 1, 90, 0, 90},
		{"2026-03-08", 1, 90, 66, 24},
		{"2026-03-09", 1, 90, 67, 23},
		{"2026-11-01", 4, 92, 31, 61},
		{"2026-11-02", 4, 92, 32, 60},
	} {
		t.Run(tc.date, func(t *testing.T) {
			now, err := time.ParseInLocation("2006-01-02", tc.date, zone)
			if err != nil {
				t.Fatal(err)
			}
			got := quarterAt(now)
			if got.number != tc.quarter || got.total != tc.total || got.completed != tc.complete || got.remaining != tc.left {
				t.Fatalf("%+v", got)
			}
			if got.percent < 0 || got.percent >= 100 {
				t.Fatal("percentage counts unfinished today as complete", got.percent)
			}
		})
	}
}

func TestQuarterScreen(t *testing.T) {
	a, err := newApp("http://trmnl.local", 180, "quarter", []string{"Keep going."}, bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	id, err := a.screenID("quarter", now)
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.image(id + "~b85n")
	if err != nil {
		t.Fatal(err)
	}
	if data[24] != 4 || data[25] != 3 {
		t.Fatal("not a 4-bit indexed PNG")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 1872 || img.Bounds().Dy() != 1404 {
		t.Fatal(img.Bounds())
	}
	for _, pixel := range []struct {
		x, y  int
		value uint32
	}{
		{90, 524, 34 * 257},    // Completed first day.
		{1057, 951, 0},         // Today: day 74, row 5, column 10; outlined black.
		{1170, 956, 221 * 257}, // The next day's square is light.
	} {
		r, _, _, _ := img.At(pixel.x, pixel.y).RGBA()
		if r != pixel.value {
			t.Fatalf("pixel %d,%d = %d", pixel.x, pixel.y, r)
		}
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, httptest.NewRequest("GET", "/preview?screen=quarter", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "quarter-20260912T2000Z") {
		t.Fatal(w.Code, w.Body.String())
	}
	// Late downloads remain associated with the originally scheduled local quarter.
	boundaryID := "quarter-20261001T0359Z"
	before, err := a.image(boundaryID)
	if err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 1, 4, 1, 0, 0, time.UTC)
	a.cache = map[string][]byte{}
	after, err := a.image(boundaryID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("quarter changed on delayed download")
	}
	local := time.Date(2026, 10, 1, 3, 59, 0, 0, time.UTC).In(a.location)
	if got := quarterAt(local); got.number != 3 || got.remaining != 1 {
		t.Fatal("UTC date used instead of local date")
	}
	for _, bad := range []string{"quarter-invalid", "quarter-99990101T0000Z"} {
		if _, err := a.image(bad); err == nil {
			t.Fatal("invalid timestamp accepted")
		}
	}
}
