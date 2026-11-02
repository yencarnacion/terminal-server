package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const kalshiDirectionID = "kalshi-daily-direction"

var errKalshiDirectionPending = errors.New("next daily Kalshi market not listed yet")

var kalshiIndexes = []struct{ series, name, description string }{
	{"KXNASDAQDUD", "Nasdaq-100", "100 large Nasdaq-listed companies"},
	{"KXINXDUD", "S&P 500", "500 leading U.S. companies"},
}

type kalshiDirection struct {
	name, description string
	target            time.Time
	strike, up        *float64
	message           string
}

func isKalshiDirectionSeries(series string) bool {
	return series == "KXNASDAQDUD" || series == "KXINXDUD"
}

func directionTarget(event kalshiEvent, market kalshiMarket) time.Time {
	if !market.OccurrenceTime.IsZero() {
		return market.OccurrenceTime
	}
	if !event.StrikeDate.IsZero() {
		return event.StrikeDate
	}
	return market.CloseTime
}

// Use the outcome date, not settlement/expiration or the server's timezone.
// Stop showing a trading day's market at 4 PM New York time even if the API
// still reports it active. The next listed event supplies weekends/holidays.
func currentKalshiDirectionEvent(events []kalshiEvent, series string, now time.Time) (kalshiEvent, error) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		return kalshiEvent{}, err
	}
	var selected kalshiEvent
	var earliest time.Time
	for _, event := range events {
		if event.Series != series || len(event.Markets) != 1 {
			continue
		}
		m := event.Markets[0]
		target := directionTarget(event, m).In(location)
		cutoff := time.Date(target.Year(), target.Month(), target.Day(), 16, 0, 0, 0, location)
		if (m.Status != "active" && m.Status != "initialized") || m.Result != "" || !m.CloseTime.After(now) || !target.After(now) || !cutoff.After(now) {
			continue
		}
		if earliest.IsZero() || target.Before(earliest) || (target.Equal(earliest) && event.Ticker < selected.Ticker) {
			selected, earliest = event, target
		}
	}
	if earliest.IsZero() {
		return selected, errKalshiDirectionPending
	}
	return selected, nil
}

func directionFromEvent(event kalshiEvent, now time.Time) (kalshiDirection, error) {
	card := kalshiDirection{}
	if !isKalshiDirectionSeries(event.Series) {
		return card, fmt.Errorf("unexpected daily direction series")
	}
	if _, err := currentKalshiDirectionEvent([]kalshiEvent{event}, event.Series, now); err != nil {
		return card, err
	}
	m := event.Markets[0]
	// YES must mean strictly above the supplied level; NO includes equality.
	// Do not guess from the title or from an unfamiliar market structure.
	if m.MarketType != "binary" || m.StrikeType != "greater" || m.FloorStrike == nil || math.IsNaN(*m.FloorStrike) || math.IsInf(*m.FloorStrike, 0) || *m.FloorStrike <= 0 {
		return card, fmt.Errorf("unrecognized daily direction rule")
	}
	card.target, card.strike = directionTarget(event, m), m.FloorStrike
	if m.Status == "initialized" || m.OpenTime.After(now) {
		card.message = "TRADING NOT OPEN YET"
		return card, nil
	}
	card.up = kalshiLastProbability(m)
	if card.up == nil {
		card.message = "PRICE UNAVAILABLE"
		if volume, err := strconv.ParseFloat(m.Volume, 64); err == nil && volume == 0 {
			card.message = "NO TRADES YET"
		}
	}
	return card, nil
}

func (k *kalshiService) fetchDirections(ctx context.Context, now time.Time) []kalshiDirection {
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	cards := make([]kalshiDirection, len(kalshiIndexes))
	var wg sync.WaitGroup
	for i, index := range kalshiIndexes {
		wg.Add(1)
		go func(i int, series, name, description string) {
			defer wg.Done()
			event, err := k.fetch(ctx, kalshiPage{Series: series}, now)
			if err == nil {
				cards[i], err = directionFromEvent(event, now)
			}
			if err != nil {
				log.Printf("kalshi direction %s: %v", series, err)
				cards[i] = kalshiDirection{message: "MARKET UNAVAILABLE"}
				if errors.Is(err, errKalshiDirectionPending) {
					cards[i].message = "NEXT MARKET PENDING"
				}
			}
			cards[i].name, cards[i].description = name, description
		}(i, index.series, index.name, index.description)
	}
	wg.Wait()
	return cards
}

func directionPercentages(up float64) (string, string, string) {
	percent := int(math.Round(up * 100))
	label := "ABOUT EVEN"
	if percent > 50 {
		label = "UP MORE LIKELY"
	} else if percent < 50 {
		label = "DOWN / FLAT MORE LIKELY"
	}
	if up > 0 && percent == 0 {
		return "<1%", ">99%", label
	}
	if up < 1 && percent == 100 {
		return ">99%", "<1%", label
	}
	return fmt.Sprintf("%d%%", percent), fmt.Sprintf("%d%%", 100-percent), label
}

func directionLevel(value float64) string {
	parts := strings.Split(fmt.Sprintf("%.2f", value), ".")
	whole := parts[0]
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	return whole + "." + parts[1]
}

func renderKalshiDirections(cards []kalshiDirection, now time.Time) ([]byte, error) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, err
	}
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
	text := func(x, y, size, maxWidth int, label string, strong, white, centered bool) {
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
		w := font.MeasureString(face, label).Ceil()
		if w > maxWidth {
			fontErr = fmt.Errorf("daily direction text does not fit: %q", label)
			return
		}
		if centered {
			x -= w / 2
		}
		ink := image.Black
		if white {
			ink = image.White
		}
		d := font.Drawer{Dst: img, Src: ink, Face: face, Dot: fixed.P(x, y)}
		d.DrawString(label)
	}
	rect := func(x0, y0, x1, y1 int, gray uint8) {
		draw.Draw(img, image.Rect(x0, y0, x1, y1), image.NewUniform(color.Gray{Y: gray}), image.Point{}, draw.Src)
	}
	arrow := func(cx, top int, up bool) {
		for y := 0; y < 90; y++ {
			half := 14
			if y < 48 {
				half = y
			}
			line := top + y
			if !up {
				line = top + 89 - y
			}
			rect(cx-half, line, cx+half+1, line+1, 0)
		}
	}
	text(64, 92, 34, 850, "KALSHI  /  DAILY DIRECTION", true, false, false)
	text(1110, 92, 29, 700, "Refreshed "+now.In(location).Format("Jan 2, 3:04 PM MST"), false, false, false)
	rect(64, 125, width-64, 128, 0)
	text(width/2, 225, 78, width-128, "Will stocks finish higher?", true, false, true)
	text(width/2, 281, 36, width-128, "Traders' estimated chances for the market close", false, false, true)
	for i, card := range cards {
		left := 64 + i*892
		center := left + 426
		rect(left, 330, left+852, 1170, 0)
		rect(left+3, 333, left+849, 1167, 255)
		text(center, 425, 74, 790, card.name, true, false, true)
		text(center, 478, 32, 790, card.description, false, false, true)
		date := "Date unavailable"
		if card.message == "NEXT MARKET PENDING" {
			date = "Next trading day pending"
		}
		if !card.target.IsZero() {
			date = card.target.In(location).Format("Mon, Jan 2, 2006 · 3:04 PM MST")
		}
		text(center, 542, 34, 810, date, true, false, true)
		up, down, label := "—", "—", card.message
		if card.up != nil {
			up, down, label = directionPercentages(*card.up)
		}
		rect(left+3, 579, left+849, 674, 0)
		text(center, 641, 44, 804, label, true, true, true)
		upX, downX := left+223, left+629
		arrow(upX, 714, true)
		arrow(downX, 714, false)
		text(upX, 865, 54, 380, "UP", true, false, true)
		text(downX, 865, 46, 390, "DOWN / FLAT", true, false, true)
		text(upX, 1011, 146, 390, up, true, false, true)
		text(downX, 1011, 146, 390, down, true, false, true)
		if card.strike != nil {
			level := directionLevel(*card.strike)
			text(upX, 1075, 31, 390, "Above "+level, false, false, true)
			text(downX, 1075, 31, 390, "At or below "+level, false, false, true)
			text(center, 1133, 29, 790, "Index points at the close", false, false, true)
		} else {
			message := "Market data temporarily unavailable."
			if card.message == "NEXT MARKET PENDING" {
				message = "Waiting for the next daily market listing."
			}
			text(center, 1082, 32, 790, message, false, false, true)
			text(center, 1133, 29, 790, "Checks again on the next display.", false, false, true)
		}
	}
	text(width/2, 1239, 36, width-128, "Percentages describe the chance of each outcome.", true, false, true)
	text(width/2, 1290, 29, width-128, "Source: Kalshi last trades · Estimates, not guarantees · Down / flat includes an unchanged close", false, false, true)
	if fontErr != nil {
		return nil, fontErr
	}
	return encodeGrayPNG(img)
}
