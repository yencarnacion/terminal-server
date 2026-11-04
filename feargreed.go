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
	"math"
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

const (
	fearGreedURL        = "https://production.dataviz.cnn.io/index/fearandgreed/graphdata"
	fearGreedCacheTTL   = 5 * time.Minute
	fearGreedRetryDelay = 2 * time.Minute
	// A latest published reading can legitimately predate a weekend or holiday.
	// Its source time is always shown; readings older than a week are unavailable.
	fearGreedMaxAge = 7 * 24 * time.Hour
)

type fearGreedSnapshot struct {
	Score         *float64  `json:"score"`
	Rating        string    `json:"rating"`
	Timestamp     time.Time `json:"timestamp"`
	PreviousClose *float64  `json:"previous_close"`
	PreviousWeek  *float64  `json:"previous_1_week"`
	PreviousMonth *float64  `json:"previous_1_month"`
	PreviousYear  *float64  `json:"previous_1_year"`
}

type fearGreedService struct {
	client             *http.Client
	mu                 sync.Mutex
	data               fearGreedSnapshot
	fetched, attempted time.Time
}

func newFearGreed() *fearGreedService {
	return &fearGreedService{client: &http.Client{Timeout: 18 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 3 || !validFearGreedURL(r.URL) {
			return fmt.Errorf("unexpected CNN redirect")
		}
		return nil
	}}}
}

func validFearGreedURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.Host == "production.dataviz.cnn.io" && u.User == nil
}

func validFearGreedScore(score *float64) bool {
	return score != nil && !math.IsNaN(*score) && !math.IsInf(*score, 0) && *score >= 0 && *score <= 100
}

func (s fearGreedSnapshot) validate(now time.Time) error {
	if !validFearGreedScore(s.Score) {
		return fmt.Errorf("CNN index score missing or invalid")
	}
	switch strings.ToLower(strings.TrimSpace(s.Rating)) {
	case "extreme fear", "fear", "neutral", "greed", "extreme greed":
	default:
		return fmt.Errorf("CNN index rating missing or invalid")
	}
	if s.Timestamp.IsZero() || now.Sub(s.Timestamp) > fearGreedMaxAge || s.Timestamp.After(now.Add(15*time.Minute)) {
		return fmt.Errorf("CNN index source time missing or outdated")
	}
	return nil
}

func (f *fearGreedService) fetch(ctx context.Context, target string, now time.Time) (fearGreedSnapshot, error) {
	u, err := url.Parse(target)
	if err != nil || !validFearGreedURL(u) {
		return fearGreedSnapshot{}, fmt.Errorf("invalid CNN index URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fearGreedSnapshot{}, err
	}
	req.Header.Set("User-Agent", "terminal-server (https://github.com/yencarnacion/terminal-server)")
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return fearGreedSnapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fearGreedSnapshot{}, fmt.Errorf("CNN index HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return fearGreedSnapshot{}, err
	}
	if len(body) > 4<<20 {
		return fearGreedSnapshot{}, fmt.Errorf("CNN index response too large")
	}
	var response struct {
		FearAndGreed fearGreedSnapshot `json:"fear_and_greed"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return fearGreedSnapshot{}, fmt.Errorf("decode CNN index: %w", err)
	}
	if err := response.FearAndGreed.validate(now); err != nil {
		return fearGreedSnapshot{}, err
	}
	return response.FearAndGreed, nil
}

// Cache successful data briefly to avoid requesting CNN for every device/preview.
// An expired cache is never used as a fallback after a failed refresh.
func (f *fearGreedService) snapshot(ctx context.Context, now time.Time) (fearGreedSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if age := now.Sub(f.fetched); !f.fetched.IsZero() && age >= 0 && age < fearGreedCacheTTL && f.data.validate(now) == nil {
		return f.data, nil
	}
	if age := now.Sub(f.attempted); !f.attempted.IsZero() && age >= 0 && age < fearGreedRetryDelay {
		return fearGreedSnapshot{}, fmt.Errorf("CNN index retry pending")
	}
	f.attempted = now
	data, err := f.fetch(ctx, fearGreedURL, now)
	if err != nil {
		return fearGreedSnapshot{}, err
	}
	f.data, f.fetched = data, now
	return data, nil
}

func (f *fearGreedService) render(ctx context.Context, now time.Time) ([]byte, error) {
	data, err := f.snapshot(ctx, now)
	if err != nil {
		log.Printf("CNN fear and greed: %v", err)
	}
	return renderFearGreed(now, data, err)
}

func fearGreedScoreLabel(v *float64) string {
	if !validFearGreedScore(v) {
		return "—"
	}
	return fmt.Sprintf("%.0f", *v)
}

func renderFearGreed(now time.Time, data fearGreedSnapshot, fetchErr error) ([]byte, error) {
	if fetchErr == nil {
		fetchErr = data.validate(now)
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
		xOffset := font.MeasureString(face, label).Ceil()
		if align == "right" {
			x -= xOffset
		} else if align == "center" {
			x -= xOffset / 2
		}
		d := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x, y)}
		d.DrawString(label)
	}
	rect := func(x, y, x1, y1 int, gray uint8) {
		draw.Draw(img, image.Rect(x, y, x1, y1), image.NewUniform(color.Gray{Y: gray}), image.Point{}, draw.Src)
	}
	text(84, 100, 30, "CNN BUSINESS · US STOCK MARKET SENTIMENT", true, "left")
	text(80, 200, 78, "Fear & Greed Index", true, "left")
	text(width-84, 100, 34, now.Format("Monday, January 2"), true, "right")
	text(width-84, 164, 29, "Refreshed "+now.Format("3:04 PM MST"), false, "right")
	rect(84, 240, width-84, 243, 0)
	if fetchErr != nil {
		text(width/2, 510, 72, "CNN index temporarily unavailable", true, "center")
		text(width/2, 605, 38, "The source could not provide a current reading.", false, "center")
		text(width/2, 678, 34, "The slideshow continues and will retry automatically.", false, "center")
	} else {
		text(width/2, 337, 29, "LATEST PUBLISHED READING", true, "center")
		text(width/2, 585, 242, fearGreedScoreLabel(data.Score), true, "center")
		text(width/2, 638, 30, "OUT OF 100", false, "center")
		text(width/2, 730, 66, strings.ToUpper(strings.TrimSpace(data.Rating)), true, "center")
		// The score position is continuous. CNN's rating is displayed as supplied,
		// rather than calculating sentiment bands with potentially different cutoffs.
		const left, right, top, bottom = 155, width - 155, 815, 855
		rect(left, top, right, bottom, 221)
		x := left + int(math.Round(*data.Score/100*float64(right-left)))
		rect(left, top, x, bottom, 85)
		rect(x-4, top-18, x+4, bottom+18, 0)
		for i := 0; i <= 4; i++ {
			tick := left + i*(right-left)/4
			rect(tick-1, bottom, tick+2, bottom+12, 0)
			text(tick, 912, 28, fmt.Sprint(i*25), false, "center")
		}
		text(left, 963, 28, "EXTREME FEAR", true, "left")
		text(right, 963, 28, "EXTREME GREED", true, "right")
		comparisons := []struct {
			label string
			value *float64
		}{
			{"PREVIOUS CLOSE", data.PreviousClose}, {"ONE WEEK AGO", data.PreviousWeek},
			{"ONE MONTH AGO", data.PreviousMonth}, {"ONE YEAR AGO", data.PreviousYear},
		}
		rect(84, 1008, width-84, 1010, 170)
		for i, c := range comparisons {
			center := 84 + (width-168)*(2*i+1)/8
			text(center, 1070, 26, c.label, true, "center")
			text(center, 1160, 76, fearGreedScoreLabel(c.value), true, "center")
			if i > 0 {
				boundary := 84 + i*(width-168)/4
				rect(boundary, 1038, boundary+2, 1185, 204)
			}
		}
		sourceTime := data.Timestamp.In(now.Location()).Format("Jan 2, 2006 at 3:04 PM MST")
		text(width/2, 1242, 28, "CNN source reading: "+sourceTime, false, "center")
		if now.Sub(data.Timestamp) > 36*time.Hour {
			text(width/2, 1284, 25, "Latest source reading predates today; markets may be closed.", false, "center")
		}
	}
	text(width/2, 1325, 25, "Source: CNN Business · cnn.com/markets/fear-and-greed", false, "center")
	if fontErr != nil {
		return nil, fontErr
	}
	return encodeGrayPNG(img)
}
