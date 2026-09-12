package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

type weatherPeriod struct {
	Name                                                      string
	StartTime, EndTime                                        time.Time
	IsDaytime                                                 bool
	Temperature                                               *float64
	TemperatureUnit                                           string
	ShortForecast, DetailedForecast, WindSpeed, WindDirection string
	ProbabilityOfPrecipitation                                struct{ Value *float64 }
}
type weatherForecast struct {
	Properties struct {
		UpdateTime time.Time
		Periods    []weatherPeriod
	}
}
type weatherService struct {
	name, pointURL     string
	client             *http.Client
	location           *time.Location
	mu                 sync.Mutex
	forecast           weatherForecast
	fetched, attempted time.Time
}

func newWeather(c configuration) *weatherService {
	loc, _ := time.LoadLocation("America/Puerto_Rico")
	return &weatherService{name: c.WeatherLocation, pointURL: fmt.Sprintf("https://api.weather.gov/points/%.4f,%.4f", c.WeatherLatitude, c.WeatherLongitude), location: loc,
		client: &http.Client{Timeout: 18 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) > 3 || r.URL.Scheme != "https" || r.URL.Host != "api.weather.gov" {
				return fmt.Errorf("unexpected NWS redirect")
			}
			return nil
		}}}
}

func (w *weatherService) get(ctx context.Context, target string, out any) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host != "api.weather.gov" || u.User != nil {
		return fmt.Errorf("invalid NWS URL")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "terminal-server (https://github.com/yencarnacion/terminal-server)")
	req.Header.Set("Accept", "application/geo+json")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("NWS HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 2<<20 {
		return fmt.Errorf("NWS response too large")
	}
	return json.Unmarshal(data, out)
}

// Small shared cache respects the public API. Never present an old forecast as live.
// A failed request backs off for two minutes; stale forecasts are not displayed.
func (w *weatherService) snapshot(ctx context.Context, now time.Time) (weatherForecast, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.fetched.IsZero() && now.Sub(w.fetched) >= 0 && now.Sub(w.fetched) < 15*time.Minute {
		return w.forecast, nil
	}
	if !w.attempted.IsZero() && now.Sub(w.attempted) >= 0 && now.Sub(w.attempted) < 2*time.Minute {
		return weatherForecast{}, fmt.Errorf("NWS retry pending")
	}
	w.attempted = now
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var point struct{ Properties struct{ Forecast string } }
	if err := w.get(bounded, w.pointURL, &point); err != nil {
		return weatherForecast{}, err
	}
	target, err := url.Parse(point.Properties.Forecast)
	if err != nil {
		return weatherForecast{}, err
	}
	query := target.Query()
	query.Set("units", "us")
	target.RawQuery = query.Encode()
	var forecast weatherForecast
	if err := w.get(bounded, target.String(), &forecast); err != nil {
		return weatherForecast{}, err
	}
	if len(forecast.Properties.Periods) == 0 || forecast.Properties.UpdateTime.IsZero() || now.Sub(forecast.Properties.UpdateTime) > 24*time.Hour {
		return weatherForecast{}, fmt.Errorf("NWS forecast missing or outdated")
	}
	for _, p := range forecast.Properties.Periods {
		if p.StartTime.IsZero() || !p.EndTime.After(p.StartTime) {
			return weatherForecast{}, fmt.Errorf("invalid NWS period")
		}
	}
	w.forecast, w.fetched = forecast, now
	return forecast, nil
}

func weatherTemperature(p weatherPeriod) string {
	if p.Temperature == nil {
		return "—"
	}
	v := *p.Temperature
	switch p.TemperatureUnit {
	case "C":
		v = v*9/5 + 32
	case "F":
	default:
		return "—"
	}
	return fmt.Sprintf("%.0f°", v)
}
func rainChance(v *float64) string {
	if v == nil || *v < 0 || *v > 100 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", *v)
}

type weatherDay struct {
	date      time.Time
	high, low *weatherPeriod
	rain      *float64
}

func weatherDays(periods []weatherPeriod, now time.Time) []weatherDay {
	days := make([]weatherDay, 5)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for i := range days {
		days[i].date = start.AddDate(0, 0, i)
	}
	for _, p := range periods {
		if !p.EndTime.After(now) {
			continue
		}
		date := p.StartTime.In(now.Location()).Format("2006-01-02")
		for i := range days {
			if date != days[i].date.Format("2006-01-02") {
				continue
			}
			if p.IsDaytime {
				copy := p
				days[i].high = &copy
			} else {
				copy := p
				days[i].low = &copy
			}
			rain := p.ProbabilityOfPrecipitation.Value
			if rain != nil && *rain >= 0 && *rain <= 100 && (days[i].rain == nil || *rain > *days[i].rain) {
				v := *rain
				days[i].rain = &v
			}
		}
	}
	return days
}

func (w *weatherService) render(ctx context.Context, now time.Time) ([]byte, error) {
	forecast, fetchErr := w.snapshot(ctx, now)
	if fetchErr != nil {
		log.Printf("weather: %v", fetchErr)
	}
	return renderWeather(w.name, now.In(w.location), forecast, fetchErr)
}

func renderWeather(name string, now time.Time, forecast weatherForecast, fetchErr error) ([]byte, error) {
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
	text := func(x, y, size, maxWidth int, label string, strong bool) {
		tf := regular
		if strong {
			tf = bold
		}
		key := fmt.Sprintf("%d/%t", size, strong)
		face := faces[key]
		if face == nil {
			face, err = opentype.NewFace(tf, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
			if err != nil {
				fontErr = err
				return
			}
			faces[key] = face
		}
		if maxWidth > 0 && font.MeasureString(face, label).Ceil() > maxWidth {
			runes := []rune(label)
			for len(runes) > 0 && font.MeasureString(face, string(runes)+"…").Ceil() > maxWidth {
				runes = runes[:len(runes)-1]
			}
			label = string(runes) + "…"
		}
		d := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x, y)}
		d.DrawString(label)
	}
	rect := func(x, y, x1, y1 int, v uint8) {
		draw.Draw(img, image.Rect(x, y, x1, y1), image.NewUniform(color.Gray{Y: v}), image.Point{}, draw.Src)
	}
	text(80, 90, 30, 900, "DAILY WEATHER · FAHRENHEIT", true)
	text(80, 170, 64, 980, name, true)
	text(1120, 95, 36, 680, now.Format("Monday, January 2, 2006"), true)
	text(1120, 155, 32, 680, "As of "+now.Format("3:04 PM MST"), false)
	rect(80, 210, 1792, 213, 0)
	var current *weatherPeriod
	for _, p := range forecast.Properties.Periods {
		if !p.EndTime.After(now) {
			continue
		}
		copy := p
		current = &copy
		break
	}
	if fetchErr != nil || current == nil {
		text(80, 440, 80, 1700, "Weather temporarily unavailable", true)
		text(80, 525, 38, 1700, "The slideshow continues. Forecast will retry automatically.", false)
		text(80, 610, 32, 1700, "Check weather.gov/sju for the latest official weather information.", false)
	} else {
		text(80, 300, 36, 690, strings.ToUpper(current.Name)+" · FORECAST", true)
		text(75, 510, 184, 710, weatherTemperature(*current)+"F", true)
		text(790, 320, 48, 990, current.ShortForecast, true)
		text(790, 410, 38, 980, "Rain chance  "+rainChance(current.ProbabilityOfPrecipitation.Value), false)
		text(790, 480, 38, 980, "Wind  "+current.WindDirection+" "+current.WindSpeed, false)
		// Word-wrap the detailed outlook at a conservative line width.
		words := strings.Fields(current.DetailedForecast)
		line := ""
		y := 570
		for _, word := range words {
			if len(line)+len(word) > 88 {
				text(80, y, 31, 1700, line, false)
				y += 42
				line = ""
				if y > 654 {
					break
				}
			}
			if line != "" {
				line += " "
			}
			line += word
		}
		if line != "" && y <= 654 {
			text(80, y, 31, 1700, line, false)
		}
		rect(80, 710, 1792, 713, 0)
		text(80, 765, 28, 1700, "FIVE-DAY OUTLOOK · HIGHS / NIGHT LOWS · MAXIMUM PERIOD RAIN CHANCE", true)
		for i, day := range weatherDays(forecast.Properties.Periods, now) {
			x := 80 + i*348
			if i > 0 {
				rect(x-18, 800, x-16, 1200, 187)
			}
			title := day.date.Format("Mon")
			if i == 0 {
				title = "Today"
			}
			text(x, 840, 39, 318, title, true)
			text(x, 888, 28, 318, day.date.Format("Jan 2"), false)
			high, low := "—", "—"
			if day.high != nil {
				high = weatherTemperature(*day.high)
			}
			if day.low != nil {
				low = weatherTemperature(*day.low)
			}
			text(x, 976, 52, 320, high+" / "+low, true)
			text(x, 1030, 26, 318, "HIGH / NIGHT LOW · °F", false)
			text(x, 1095, 32, 318, "Rain  "+rainChance(day.rain), true)
			p := day.high
			if p == nil {
				p = day.low
			}
			if p != nil {
				words := strings.Fields(p.ShortForecast)
				line := ""
				y := 1150
				for _, word := range words {
					if len(line)+len(word) > 21 {
						text(x, y, 26, 318, line, false)
						line = ""
						y += 34
					}
					if y > 1218 {
						break
					}
					if line != "" {
						line += " "
					}
					line += word
				}
				if y <= 1218 {
					text(x, y, 26, 318, line, false)
				}
			}
		}
		text(80, 1290, 25, 1700, "NWS forecast issued "+forecast.Properties.UpdateTime.In(now.Location()).Format("Jan 2, 3:04 PM MST")+" · Missing values shown as —", false)
	}
	text(80, 1330, 24, 1700, "Source: NOAA / National Weather Service · weather.gov/sju · Not an emergency alert display", false)
	if fontErr != nil {
		return nil, fontErr
	}
	return encodeGrayPNG(img)
}
