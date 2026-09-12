package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

type quarterProgress struct {
	number                      int
	start, end                  time.Time // End is exclusive, at the start of the next quarter.
	total, completed, remaining int
	percent                     float64
}

func quarterAt(now time.Time) quarterProgress {
	q := quarterProgress{number: (int(now.Month())-1)/3 + 1}
	q.start = time.Date(now.Year(), time.Month((q.number-1)*3+1), 1, 0, 0, 0, 0, now.Location())
	q.end = q.start.AddDate(0, 3, 0)
	// Count civil dates, not 24-hour durations: DST days can be 23 or 25 hours.
	civil := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
	q.total = int(civil(q.end).Sub(civil(q.start)) / (24 * time.Hour))
	q.completed = int(civil(now).Sub(civil(q.start)) / (24 * time.Hour))
	q.remaining = q.total - q.completed // Includes today, which is not complete yet.
	q.percent = 100 * float64(q.completed) / float64(q.total)
	return q
}

type quarterScreen struct{ location *time.Location }

func (q quarterScreen) Render(timestamp string) ([]byte, error) {
	now, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return nil, err
	}
	now = now.In(q.location)
	progress := quarterAt(now)
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
		for _, face := range faces {
			face.Close()
		}
	}()
	var fontErr error
	text := func(x, y, size int, label string, strong bool, align string) {
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
			x -= font.MeasureString(face, label).Ceil()
		} else if align == "center" {
			x -= font.MeasureString(face, label).Ceil() / 2
		}
		d := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x, y)}
		d.DrawString(label)
	}
	rect := func(x0, y0, x1, y1 int, v uint8) {
		draw.Draw(img, image.Rect(x0, y0, x1, y1), image.NewUniform(color.Gray{Y: v}), image.Point{}, draw.Src)
	}
	text(84, 100, 30, "QUARTER PROGRESS", true, "left")
	text(80, 210, 92, fmt.Sprintf("Q%d %d", progress.number, now.Year()), true, "left")
	text(width-84, 115, 38, now.Format("Monday, January 2"), true, "right")
	text(width-84, 174, 30, progress.start.Format("Jan 2")+" – "+progress.end.AddDate(0, 0, -1).Format("Jan 2, 2006"), false, "right")
	rect(84, 244, width-84, 247, 0)
	text(84, 396, 126, fmt.Sprint(progress.completed), true, "left")
	text(88, 446, 28, "DAYS COMPLETE", false, "left")
	text(708, 396, 126, fmt.Sprint(progress.remaining), true, "left")
	text(712, 446, 28, "DAYS LEFT · INCLUDING TODAY", false, "left")
	text(width-84, 396, 112, fmt.Sprintf("%.1f%%", progress.percent), true, "right")
	text(width-84, 446, 28, "OF THE QUARTER COMPLETE", false, "right")
	// One square for each actual day, read left-to-right, top-to-bottom.
	const columns, size, gap, top = 16, 84, 24, 518
	for day := 0; day < progress.total; day++ {
		x := 84 + (day%columns)*(size+gap)
		y := top + (day/columns)*(size+gap)
		switch {
		case day < progress.completed:
			rect(x, y, x+size, y+size, 34)
		case day == progress.completed:
			rect(x, y, x+size, y+size, 0)
			rect(x+5, y+5, x+size-5, y+size-5, 255)
			text(x+size/2, y+54, 32, fmt.Sprint(now.Day()), true, "center")
		default:
			rect(x, y, x+size, y+size, 221)
		}
	}
	rect(84, 1202, 108, 1226, 34)
	text(120, 1225, 26, "Complete", false, "left")
	rect(326, 1202, 350, 1226, 0)
	rect(329, 1205, 347, 1223, 255)
	text(362, 1225, 26, "Today", false, "left")
	rect(516, 1202, 540, 1226, 221)
	text(552, 1225, 26, "Ahead", false, "left")
	text(width-84, 1225, 27, fmt.Sprintf("DAY %d OF %d · ONE SQUARE = ONE DAY", progress.completed+1, progress.total), false, "right")
	text(width/2, 1290, 30, "Next quarter begins "+progress.end.Format("January 2, 2006"), false, "center")
	text(width/2, 1330, 24, "Updated "+now.Format("3:04 PM MST"), false, "center")
	if fontErr != nil {
		return nil, fontErr
	}
	return encodeGrayPNG(img)
}
