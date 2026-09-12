package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMonthLayout(t *testing.T) {
	for _, tc := range []struct {
		date       string
		weekday    time.Weekday
		days, rows int
	}{
		{"2026-09-12", time.Tuesday, 30, 5},
		{"2024-02-29", time.Thursday, 29, 5},
		{"2026-02-01", time.Sunday, 28, 5},
		{"2026-08-31", time.Saturday, 31, 6},
		{"2027-01-01", time.Friday, 31, 6},
	} {
		day, _ := time.Parse("2006-01-02", tc.date)
		first, days, rows := monthLayout(day)
		if first.Weekday() != tc.weekday || days != tc.days || rows != tc.rows {
			t.Fatalf("%s: %s %d %d", tc.date, first.Weekday(), days, rows)
		}
	}
}

func TestCalendarPNGAndTodayHighlight(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []string{"2026-09-12T18:30:00Z", "2024-02-29T17:00:00Z", "2026-08-31T17:00:00Z", "2027-01-01T01:00:00Z", "2026-11-01T06:30:00Z"} {
		data, err := (calendarScreen{zone}).Render(stamp)
		if err != nil {
			t.Fatal(err)
		}
		if data[24] != 4 || data[25] != 3 {
			t.Fatal("calendar must be 4-bit indexed PNG")
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != width || img.Bounds().Dy() != height {
			t.Fatal(img.Bounds())
		}
		utc, _ := time.Parse(time.RFC3339, stamp)
		now := utc.In(zone)
		first, _, rows := monthLayout(now)
		cell := int(first.Weekday()) + now.Day() - 1
		x := 54 + (width-108)*(cell%7)/7 + 10
		y := 360 + (1170-360)*(cell/7)/rows + 10
		r, g, b, _ := img.At(x, y).RGBA()
		if r != 0 || g != 0 || b != 0 {
			t.Fatalf("today not highlighted for local date %s", now)
		}
	}
}

func TestSlideshowLifecycle(t *testing.T) {
	a, err := newApp("http://10.17.17.90:8177", 180, "slideshow", []string{"Hello, calendar."}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 13, 3, 59, 0, 0, time.UTC) // Sept 12, 11:59 PM in New York.
	a.now = func() time.Time { return now }
	h := a.routes()
	display := func(mac string) string {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/display", nil)
		r.Header.Set("Access-Token", a.token(mac))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var p struct {
			Filename string `json:"filename"`
			Refresh  int    `json:"refresh_rate"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || p.Refresh != 180 {
			t.Fatal(w.Code, w.Body.String())
		}
		return p.Filename
	}
	first := display("aa:bb:cc:dd:ee:01")
	if strings.HasPrefix(first, "calendar-") {
		t.Fatal("first slide should be fortune")
	}
	preview := httptest.NewRecorder()
	h.ServeHTTP(preview, httptest.NewRequest("GET", "/preview?screen=calendar", nil))
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), "calendar-") {
		t.Fatal("calendar preview")
	}
	second := display("aa:bb:cc:dd:ee:01")
	if second != "calendar-20260913T0359Z" {
		t.Fatal(second)
	}
	other := display("aa:bb:cc:dd:ee:02")
	if strings.HasPrefix(other, "calendar-") {
		t.Fatal("devices must have separate cursors")
	}
	before, err := a.image(second)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	a.cache = map[string][]byte{} // Simulate an eviction, then a delayed download after midnight.
	after, err := a.image(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("calendar image changed after midnight")
	}
	if third := display("aa:bb:cc:dd:ee:01"); strings.HasPrefix(third, "calendar-") {
		t.Fatal("third slide should be fortune")
	}
	if fourth := display("aa:bb:cc:dd:ee:01"); fourth != "calendar-20260913T0402Z" {
		t.Fatal(fourth)
	}
	if _, err := a.image("calendar-invalid"); err == nil {
		t.Fatal("invalid calendar ID accepted")
	}
	if _, err := a.image("calendar-99990101T0000Z"); err == nil {
		t.Fatal("out of range date accepted")
	}
}
