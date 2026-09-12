package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBatteryTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    batteryStatus
	}{
		{"percent preferred", map[string]string{"PERCENT_CHARGED": "84.6", "BATTERY_CAPACITY": "1/2"}, batteryStatus{percent: 85, known: true}},
		{"empty", nil, batteryStatus{}},
		{"zero", map[string]string{"PERCENT_CHARGED": "0"}, batteryStatus{known: true}},
		{"hyphen and charging", map[string]string{"Percent-Charged": "99", "Battery-Charging": "1"}, batteryStatus{percent: 99, known: true, charging: true}},
		{"capacity fallback", map[string]string{"PERCENT_CHARGED": "-1", "BATTERY_CAPACITY": "11300/11420"}, batteryStatus{percent: 99, known: true}},
		{"power is not charging", map[string]string{"USB_CONNECTED": "true", "PERCENT_CHARGED": "100"}, batteryStatus{percent: 100, known: true, plugged: true}},
		{"invalid", map[string]string{"PERCENT_CHARGED": "NaN", "BATTERY_CAPACITY": "NaN/100"}, batteryStatus{}},
		{"out of range", map[string]string{"PERCENT_CHARGED": "101", "BATTERY_CAPACITY": "120/100"}, batteryStatus{}},
		{"zero capacity", map[string]string{"BATTERY_CAPACITY": "0/0"}, batteryStatus{}},
		{"infinite capacity", map[string]string{"BATTERY_CAPACITY": "10/Inf"}, batteryStatus{}},
		{"voltage alone", map[string]string{"BATTERY_VOLTAGE": "4.1"}, batteryStatus{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			got := readBattery(h)
			if got != tc.want {
				t.Fatalf("%+v != %+v", got, tc.want)
			}
		})
	}
}

func TestBatteryLabelsAndIDs(t *testing.T) {
	for _, tc := range []struct {
		b     batteryStatus
		label string
	}{
		{batteryStatus{}, "Battery —"},
		{batteryStatus{percent: 76, known: true}, "76%"},
		{batteryStatus{percent: 20, known: true}, "20% · LOW"},
		{batteryStatus{percent: 0, known: true}, "0% · LOW"},
		{batteryStatus{percent: 12, known: true, charging: true}, "12% · Charging"},
		{batteryStatus{plugged: true}, "Battery — · Power connected"},
	} {
		if got := tc.b.label(); got != tc.label {
			t.Fatal(got)
		}
		base, b, err := splitBatteryID("calendar-20260912T1914Z" + tc.b.suffix())
		if err != nil || base != "calendar-20260912T1914Z" || b != tc.b {
			t.Fatal(base, b, err)
		}
	}
	for _, id := range []string{"quote~b101n", "quote~b-1n", "quote~b85x", "quote~b", "quote~b01n", "quote~bun"} {
		if _, _, err := splitBatteryID(id); err == nil {
			t.Fatalf("accepted %s", id)
		}
	}
}

func TestBatteryOnEveryScreen(t *testing.T) {
	a, err := newApp("http://10.17.17.90:8177", 180, "slideshow", []string{"A little progress every day."}, bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return time.Date(2026, 9, 12, 19, 14, 0, 0, time.UTC) }
	h := a.routes()
	display := func(mac, charge string) string {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/display", nil)
		r.Header.Set("Access-Token", a.token(mac))
		r.Header.Set("Percent-Charged", charge)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var result struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		return result.Filename
	}
	quoteID := display("aa:bb:cc:dd:ee:01", "85")
	calendarID := display("aa:bb:cc:dd:ee:01", "12")
	if !strings.HasSuffix(quoteID, "~b85n") || calendarID != "calendar-20260912T1914Z~b12n" {
		t.Fatal(quoteID, calendarID)
	}
	old, err := a.image(quoteID)
	if err != nil {
		t.Fatal(err)
	}
	otherID := display("aa:bb:cc:dd:ee:02", "40")
	other, err := a.image(otherID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(old, other) {
		t.Fatal("different device batteries shared an image")
	}
	a.cache = map[string][]byte{}
	after, err := a.image(quoteID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old, after) {
		t.Fatal("battery reading changed after eviction")
	}
	for _, id := range []string{quoteID, calendarID} {
		data, err := a.image(id)
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
		ink := false
		for y := height - 48; y < height-24; y++ {
			for x := width/2 - 220; x < width/2+220; x++ {
				r, _, _, _ := img.At(x, y).RGBA()
				if r < 65535 {
					ink = true
				}
			}
		}
		if !ink {
			t.Fatal("missing battery footer", id)
		}
	}
}
