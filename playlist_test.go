package main

import (
	"io"
	"reflect"
	"testing"
)

func TestConfiguredSlideOrder(t *testing.T) {
	cfg, err := readConfiguration([]string{"--config", configFile(t, "slide_order: [weather, calendar, quarter, quote, newspapers]\n")}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{slideOrder: cfg.SlideOrder, weather: newWeather(cfg), frontpages: &frontpages{slides: []coverSlide{{ID: "nyt"}, {ID: "end"}}}}
	if got := a.playlist(); !reflect.DeepEqual(got, []string{"weather", "calendar", "quarter", "cowsay", "nyt", "end"}) {
		t.Fatal(got)
	}
	a.slideOrder = []string{"newspapers", "quote", "weather"}
	if got := a.playlist(); !reflect.DeepEqual(got, []string{"nyt", "end", "cowsay", "weather"}) {
		t.Fatal(got)
	}
	a.weather = nil
	a.frontpages = nil
	if got := a.playlist(); !reflect.DeepEqual(got, []string{"cowsay"}) {
		t.Fatal(got)
	}
	a.slideOrder = []string{"newspapers"}
	if got := a.playlist(); !reflect.DeepEqual(got, []string{"cowsay"}) {
		t.Fatal("empty playlist fallback", got)
	}
	for _, value := range []string{"[]", "null", "[quote, quote]", "[unknown]"} {
		if _, err := readConfiguration([]string{"--config", configFile(t, "slide_order: "+value)}, io.Discard); err == nil {
			t.Fatal("accepted", value)
		}
	}
}
