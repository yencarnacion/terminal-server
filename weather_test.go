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

type weatherTransport func(*http.Request) (*http.Response, error)

func TestWeatherLive(t *testing.T) {
	path := os.Getenv("WEATHER_LIVE_PREVIEW")
	if path == "" {
		t.Skip("set WEATHER_LIVE_PREVIEW for opt-in NWS integration and visual QA")
	}
	w := newWeather(defaultConfiguration())
	now := time.Now()
	f, err := w.snapshot(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	data, err := renderWeather(w.name, now.In(w.location), f, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err = addBatteryFooter(data, readBattery(http.Header{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f weatherTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func weatherFixture(now time.Time) weatherForecast {
	var f weatherForecast
	f.Properties.UpdateTime = now.Add(-time.Hour)
	for i := 0; i < 12; i++ {
		start := time.Date(now.Year(), now.Month(), now.Day(), 6, 0, 0, 0, now.Location()).Add(time.Duration(i) * 12 * time.Hour)
		temp, rain := 86.0, 30.0
		if i%2 == 1 {
			temp = 78
			rain = 40
		}
		p := weatherPeriod{Name: "Today", StartTime: start, EndTime: start.Add(12 * time.Hour), IsDaytime: i%2 == 0, Temperature: &temp, TemperatureUnit: "F", ShortForecast: "Scattered Rain Showers", DetailedForecast: "Scattered rain showers. Mostly sunny, with a high near 86. East wind 10 to 17 mph, with gusts as high as 24 mph. Chance of precipitation is 30%.", WindSpeed: "10 to 17 mph", WindDirection: "E"}
		p.ProbabilityOfPrecipitation.Value = &rain
		f.Properties.Periods = append(f.Properties.Periods, p)
	}
	return f
}

func TestWeatherData(t *testing.T) {
	w := newWeather(defaultConfiguration())
	now := time.Date(2026, 9, 12, 13, 0, 0, 0, w.location)
	forecast := weatherFixture(now)
	calls := 0
	fail := false
	w.client = &http.Client{Transport: weatherTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("User-Agent") == "" {
			t.Error("missing NWS user agent")
		}
		if fail {
			return nil, fmt.Errorf("offline")
		}
		body := `{"properties":{"forecast":"https://api.weather.gov/gridpoints/SJU/162,132/forecast"}}`
		if strings.Contains(r.URL.Path, "gridpoints") {
			if r.URL.Query().Get("units") != "us" {
				t.Error("not Fahrenheit")
			}
			b, _ := json.Marshal(forecast)
			body = string(b)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	if _, err := w.snapshot(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := w.snapshot(context.Background(), now.Add(time.Minute)); err != nil || calls != 2 {
		t.Fatalf("cache: %d %v", calls, err)
	}
	fail = true
	if _, err := w.snapshot(context.Background(), now.Add(16*time.Minute)); err == nil {
		t.Fatal("served old data on failure")
	}
	before := calls
	if _, err := w.snapshot(context.Background(), now.Add(17*time.Minute)); err == nil || calls != before {
		t.Fatal("missing backoff")
	}
	if err := w.get(context.Background(), "https://example.com/forecast", &forecast); err == nil {
		t.Fatal("accepted off-origin link")
	}
	days := weatherDays(forecast.Properties.Periods, now)
	if len(days) != 5 || days[0].high == nil || days[0].low == nil || rainChance(days[0].rain) != "40%" {
		t.Fatalf("days: %+v", days)
	}
	tonight := weatherDays(forecast.Properties.Periods, now.Add(7*time.Hour))
	if tonight[0].high != nil || tonight[0].low == nil {
		t.Fatal("expired high retained")
	}
	c := 30.0
	if weatherTemperature(weatherPeriod{Temperature: &c, TemperatureUnit: "C"}) != "86°" || weatherTemperature(weatherPeriod{}) != "—" || rainChance(nil) != "—" {
		t.Fatal("unit or missing-value handling")
	}
}

func TestWeatherScreenAndPlaylist(t *testing.T) {
	a, err := newApp("http://trmnl.local", 120, "slideshow", []string{"Hello"}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	a.weather = newWeather(defaultConfiguration())
	now := time.Date(2026, 9, 12, 13, 0, 0, 0, a.weather.location)
	a.now = func() time.Time { return now }
	a.weather.forecast = weatherFixture(now)
	a.weather.fetched = now
	if strings.Join(a.playlist(), ",") != "weather,calendar,quarter,cowsay" {
		t.Fatal(a.playlist())
	}
	id, _ := a.screenID("weather", now)
	r := httptest.NewRequest("GET", "/screens/"+id+"~b78n.png", nil)
	resp := httptest.NewRecorder()
	a.routes().ServeHTTP(resp, r)
	if resp.Code != 200 || !strings.Contains(resp.Header().Get("Cache-Control"), "no-store") {
		t.Fatal(resp.Code, resp.Header())
	}
	data := resp.Body.Bytes()
	if data[24] != 4 || data[25] != 3 {
		t.Fatal("not 4-bit indexed PNG")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != width || img.Bounds().Dy() != height {
		t.Fatal("image", err)
	}
	if len(a.cache) != 0 {
		t.Fatal("weather image cached")
	}
	// Optional local visual QA artifact; normal tests do not write outside TempDir.
	if path := os.Getenv("WEATHER_TEST_PREVIEW"); path != "" {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err = renderWeather("San Juan, PR", now, weatherForecast{}, fmt.Errorf("offline"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal("outage screen", err)
	}
	resp = httptest.NewRecorder()
	a.routes().ServeHTTP(resp, httptest.NewRequest("GET", "/preview?screen=weather", nil))
	if resp.Code != 200 || !strings.Contains(resp.Body.String(), id) {
		t.Fatal("weather preview", resp.Code)
	}
	for _, yaml := range []string{"weather_latitude: 91", "weather_longitude: .nan", "weather_location: ''", "screen: weather\nweather_enabled: false"} {
		if _, err := readConfiguration([]string{"--config", configFile(t, yaml)}, io.Discard); err == nil {
			t.Fatal("accepted", yaml)
		}
	}
}
