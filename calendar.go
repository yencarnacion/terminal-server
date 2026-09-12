package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Calendar timestamps are captured when a screen is scheduled, so a download
// across midnight still returns the exact date/time promised by its filename.
type calendarScreen struct{ location *time.Location }

func monthLayout(t time.Time) (first time.Time, days, rows int) {
	first = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	days = first.AddDate(0, 1, -1).Day()
	rows = (int(first.Weekday()) + days + 6) / 7
	if rows < 5 {
		rows = 5
	}
	return
}

func (c calendarScreen) Render(timestamp string) ([]byte, error) {
	now, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return nil, err
	}
	now = now.In(c.location)
	regular, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return nil, err
	}
	bold, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return nil, err
	}
	img := image.NewGray(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	faces := map[string]font.Face{}
	defer func() {
		for _, f := range faces {
			f.Close()
		}
	}()
	var fontErr error
	text := func(x, y, size int, s string, strong, white bool, align string) {
		key := fmt.Sprintf("%d/%t", size, strong)
		face := faces[key]
		if face == nil {
			tf := regular
			if strong {
				tf = bold
			}
			face, err = opentype.NewFace(tf, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
			if err != nil {
				fontErr = err
				return
			}
			faces[key] = face
		}
		if align == "right" {
			x -= font.MeasureString(face, s).Ceil()
		} else if align == "center" {
			x -= font.MeasureString(face, s).Ceil() / 2
		}
		ink := image.Black
		if white {
			ink = image.White
		}
		d := font.Drawer{Dst: img, Src: ink, Face: face, Dot: fixed.P(x, y)}
		d.DrawString(s)
	}
	rect := func(x0, y0, x1, y1 int, gray uint8) {
		draw.Draw(img, image.Rect(x0, y0, x1, y1), image.NewUniform(color.Gray{Y: gray}), image.Point{}, draw.Src)
	}
	// Bold month band and Sunday-first ruled grid echo a paper wall calendar.
	rect(54, 48, width-54, 206, 0)
	text(86, 95, 25, "MONTH AT A GLANCE", true, true, "left")
	text(82, 175, 80, strings.ToUpper(now.Format("January")), true, true, "left")
	text(930, 175, 64, now.Format("2006"), false, true, "left")
	text(width-86, 148, 82, now.Format("3:04 PM"), true, true, "right")
	text(width-86, 183, 25, now.Format("MST")+"  /  TIME AT REFRESH", false, true, "right")
	text(60, 278, 44, "TODAY  "+strings.ToUpper(now.Format("Monday, January 2")), true, false, "left")
	text(width-60, 275, 25, fmt.Sprintf("DAY %d OF %d", now.YearDay(), time.Date(now.Year(), 12, 31, 0, 0, 0, 0, now.Location()).YearDay()), false, false, "right")
	first, days, rows := monthLayout(now)
	const left, right, top, bottom = 54, width - 54, 360, 1170
	for col, name := range []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"} {
		x0 := left + (right-left)*col/7
		x1 := left + (right-left)*(col+1)/7
		rect(x0, 311, x1, 360, 0)
		text((x0+x1)/2, 346, 27, name, true, true, "center")
	}
	for row := 0; row < rows; row++ {
		for col := 0; col < 7; col++ {
			x0 := left + (right-left)*col/7
			x1 := left + (right-left)*(col+1)/7
			y0 := top + (bottom-top)*row/rows
			y1 := top + (bottom-top)*(row+1)/rows
			day := row*7 + col - int(first.Weekday()) + 1
			current := day == now.Day()
			if current {
				rect(x0, y0, x1, y1, 0)
			} else if day < 1 || day > days {
				rect(x0, y0, x1, y1, 238)
			}
			if day >= 1 && day <= days {
				text(x0+20, y0+60, 50, fmt.Sprint(day), true, current, "left")
				if current {
					text(x1-18, y0+55, 22, "TODAY", true, true, "right")
				} else {
					for ly := y0 + 86; ly < y1-14; ly += 30 {
						rect(x0+18, ly, x1-18, ly+1, 204)
					}
				}
			}
		}
	}
	for col := 0; col <= 7; col++ {
		x := left + (right-left)*col/7
		rect(x, top, x+2, bottom+2, 102)
	}
	for row := 0; row <= rows; row++ {
		y := top + (bottom-top)*row/rows
		rect(left, y, right+2, y+2, 102)
	}
	mini := func(x int, t time.Time) {
		f, n, _ := monthLayout(t)
		text(x, 1215, 26, strings.ToUpper(t.Format("January 2006")), true, false, "left")
		for col, d := range []string{"S", "M", "T", "W", "T", "F", "S"} {
			text(x+col*40+12, 1247, 19, d, true, false, "center")
		}
		for day := 1; day <= n; day++ {
			pos := int(f.Weekday()) + day - 1
			text(x+(pos%7)*40+12, 1272+(pos/7)*22, 19, fmt.Sprint(day), false, false, "center")
		}
	}
	mini(64, first.AddDate(0, -1, 0))
	mini(width-340, first.AddDate(0, 1, 0))
	text(width/2, 1250, 30, now.Format("Monday, January 2, 2006"), true, false, "center")
	text(width/2, 1295, 26, "Updated "+now.Format("3:04 PM MST"), false, false, "center")
	text(width/2, 1335, 23, "Calendar & fortune slideshow", false, false, "center")
	if fontErr != nil {
		return nil, fontErr
	}
	return encodeGrayPNG(img)
}
