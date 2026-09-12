package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

type batteryStatus struct {
	percent  int
	known    bool
	charging bool
	plugged  bool
}

func deviceHeader(h http.Header, name string) string {
	if value := h.Get(name); value != "" {
		return value
	}
	return h.Get(strings.ReplaceAll(name, "_", "-"))
}

func readBattery(h http.Header) batteryStatus {
	truth := func(name string) bool {
		v := strings.ToLower(strings.TrimSpace(deviceHeader(h, name)))
		return v == "1" || v == "true"
	}
	b := batteryStatus{charging: truth("BATTERY_CHARGING"), plugged: truth("USB_CONNECTED")}
	valid := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 100 }
	if value, err := strconv.ParseFloat(deviceHeader(h, "PERCENT_CHARGED"), 64); err == nil && valid(value) {
		b.percent = int(math.Round(value))
		b.known = true
		return b
	}
	// Capacity is a measured remaining/full ratio, not a voltage-based guess.
	if current, full, ok := strings.Cut(deviceHeader(h, "BATTERY_CAPACITY"), "/"); ok {
		c, e1 := strconv.ParseFloat(current, 64)
		f, e2 := strconv.ParseFloat(full, 64)
		if e1 == nil && e2 == nil && f > 0 && !math.IsInf(f, 0) && c <= f && valid(c/f*100) {
			b.percent = int(math.Round(c / f * 100))
			b.known = true
		}
	}
	return b
}

func (b batteryStatus) suffix() string {
	if !b.known && !b.charging && !b.plugged {
		return ""
	}
	charge := "u"
	if b.known {
		charge = strconv.Itoa(b.percent)
	}
	state := "n"
	if b.plugged {
		state = "p"
	}
	if b.charging {
		state = "c"
	}
	return "~b" + charge + state
}

func splitBatteryID(id string) (string, batteryStatus, error) {
	base, raw, found := strings.Cut(id, "~b")
	b := batteryStatus{}
	if !found {
		return base, b, nil
	}
	if len(raw) < 2 {
		return "", b, fmt.Errorf("invalid battery image ID")
	}
	level, state := raw[:len(raw)-1], raw[len(raw)-1:]
	if level != "u" {
		p, err := strconv.Atoi(level)
		if err != nil || p < 0 || p > 100 {
			return "", b, fmt.Errorf("invalid battery level")
		}
		b.known = true
		b.percent = p
	}
	switch state {
	case "c":
		b.charging = true
	case "p":
		b.plugged = true
	case "n":
	default:
		return "", b, fmt.Errorf("invalid battery state")
	}
	if b.suffix() != "~b"+raw {
		return "", b, fmt.Errorf("noncanonical battery image ID")
	}
	return base, b, nil
}

func (b batteryStatus) label() string {
	label := "Battery —"
	if b.known {
		label = fmt.Sprintf("%d%%", b.percent)
	}
	if b.charging {
		return label + " · Charging"
	}
	if b.plugged {
		return label + " · Power connected"
	}
	if b.known && b.percent <= 20 {
		return label + " · LOW"
	}
	return label
}

// A shared footer keeps battery status unobtrusive and consistent on all screens.
func addBatteryFooter(data []byte, b batteryStatus) ([]byte, error) {
	source, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	img := image.NewGray(source.Bounds())
	draw.Draw(img, img.Bounds(), source, image.Point{}, draw.Src)
	tf, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return nil, err
	}
	face, err := opentype.NewFace(tf, &opentype.FaceOptions{Size: 24, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	defer face.Close()
	label := b.label()
	total := 56 + font.MeasureString(face, label).Ceil()
	x := (width - total) / 2
	y := height - 48
	ink := uint8(85)
	if b.known && b.percent <= 20 && !b.charging && !b.plugged {
		ink = 0
	}
	rect := func(x0, y0, x1, y1 int, v uint8) {
		draw.Draw(img, image.Rect(x0, y0, x1, y1), image.NewUniform(color.Gray{Y: v}), image.Point{}, draw.Src)
	}
	rect(x, y, x+38, y+21, ink)
	rect(x+2, y+2, x+36, y+19, 255)
	rect(x+38, y+6, x+42, y+15, ink)
	if b.known {
		fill := int(math.Round(30 * float64(b.percent) / 100))
		rect(x+4, y+4, x+4+fill, y+17, ink)
	}
	d := font.Drawer{Dst: img, Src: image.NewUniform(color.Gray{Y: ink}), Face: face, Dot: fixed.P(x+56, y+19)}
	d.DrawString(label)
	return encodeGrayPNG(img)
}
