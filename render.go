package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const width, height = 1872, 1404

// Screen is the extension point for future dashboards and other generators.
type Screen interface {
	Render(quote string) ([]byte, error)
}
type cowScreen struct{}

func parseQuotes(text string) ([]string, error) {
	var quotes []string
	var part []string
	flush := func() {
		q := strings.Join(strings.Fields(strings.Join(part, " ")), " ")
		if q != "" {
			quotes = append(quotes, q)
		}
		part = nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "%" {
			flush()
		} else {
			part = append(part, line)
		}
	}
	flush()
	if len(quotes) == 0 {
		return nil, fmt.Errorf("quote file is empty")
	}
	for _, q := range quotes {
		if !utf8.ValidString(q) || utf8.RuneCountInString(q) > 2000 {
			return nil, fmt.Errorf("quotes must be valid UTF-8 and at most 2000 characters each")
		}
	}
	return quotes, nil
}

func wrap(text string, columns int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		if line != "" && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > columns {
			lines = append(lines, line)
			line = ""
		}
		for utf8.RuneCountInString(word) > columns {
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			r := []rune(word)
			lines = append(lines, string(r[:columns]))
			word = string(r[columns:])
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func (cowScreen) Render(quote string) ([]byte, error) {
	tf, err := opentype.Parse(gomono.TTF)
	if err != nil {
		return nil, err
	}
	var face font.Face
	var lines []string
	var cell, step, size int
	for size = 64; size >= 18; size -= 2 {
		face, err = opentype.NewFace(tf, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
		if err != nil {
			return nil, err
		}
		cell = font.MeasureString(face, "M").Ceil()
		step = size * 5 / 4
		lines = wrap(quote, (width-220)/cell-4)
		if (len(lines)+9)*step < height-300 {
			break
		}
		face.Close()
		face = nil
	}
	if face == nil {
		return nil, fmt.Errorf("quote does not fit display")
	}
	defer face.Close()
	max := 0
	for _, line := range lines {
		if n := utf8.RuneCountInString(line); n > max {
			max = n
		}
	}
	bubble := []string{" " + strings.Repeat("_", max+2)}
	for i, line := range lines {
		left, right := "|", "|"
		if len(lines) == 1 {
			left, right = "<", ">"
		} else if i == 0 {
			left, right = "/", "\\"
		} else if i == len(lines)-1 {
			left, right = "\\", "/"
		}
		bubble = append(bubble, left+" "+line+strings.Repeat(" ", max-utf8.RuneCountInString(line))+" "+right)
	}
	bubble = append(bubble, " "+strings.Repeat("-", max+2),
		"        \\   ^__^",
		"         \\  (oo)\\_______",
		"            (__)\\       )\\/\\",
		"                ||----w |",
		"                ||     ||")
	img := image.NewGray(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	d := font.Drawer{Dst: img, Src: image.Black, Face: face}
	blockWidth := (max + 4) * cell
	if blockWidth < 30*cell {
		blockWidth = 30 * cell
	}
	x := (width - blockWidth) / 2
	y := (height-len(bubble)*step)/2 + size
	for _, line := range bubble {
		d.Dot = fixed.P(x, y)
		d.DrawString(line)
		y += step
	}
	small, err := opentype.NewFace(tf, &opentype.FaceOptions{Size: 24, DPI: 72})
	if err != nil {
		return nil, err
	}
	defer small.Close()
	d.Face = small
	d.Dot = fixed.P(90, 100)
	d.DrawString("FORTUNE / COWSAY")
	draw.Draw(img, image.Rect(90, 126, width-90, 128), image.Black, image.Point{}, draw.Src)
	d.Dot = fixed.P(90, height-72)
	d.DrawString("TERMINAL SERVER")
	label := "TRMNL X  /  1872 x 1404"
	d.Dot = fixed.P(width-90-font.MeasureString(small, label).Ceil(), height-72)
	d.DrawString(label)
	// Indexed PNG with exactly 16 grayscale levels: 4-bit output for TRMNL X.
	return encodeGrayPNG(img)
}

func encodeGrayPNG(img *image.Gray) ([]byte, error) {
	palette := make(color.Palette, 16)
	for i := range palette {
		palette[i] = color.Gray{Y: uint8(i * 17)}
	}
	output := image.NewPaletted(img.Bounds(), palette)
	for i, v := range img.Pix {
		output.Pix[i] = uint8((int(v) + 8) / 17)
	}
	var buf bytes.Buffer
	err := png.Encode(&buf, output)
	return buf.Bytes(), err
}
