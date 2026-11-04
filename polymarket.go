package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

type polymarketPage struct{ ID, Event, Market, Series, Filter string }

var polySlug = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

var errPolyNextEventPending = errors.New("next matching Polymarket event pending")

func parsePolymarketPage(raw string) (polymarketPage, error) {
	p := polymarketPage{}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "series:") {
		parts := strings.Split(raw, ":")
		// These selectors have known resolution rules. In particular, mu-kpis
		// includes NAND and margin markets that must not replace DRAM revenue.
		if raw != "series:best-ai-model" && raw != "series:mu-kpis:dram-revenue" {
			return p, fmt.Errorf("supported Polymarket series are series:best-ai-model and series:mu-kpis:dram-revenue")
		}
		p.Series = parts[1]
		if len(parts) == 3 {
			p.Filter = parts[2]
		}
		sum := sha256.Sum256([]byte(raw))
		p.ID = fmt.Sprintf("poly-%x", sum[:12])
		return p, nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || (u.Host != "polymarket.com" && u.Host != "www.polymarket.com") || u.User != nil {
		return p, fmt.Errorf("polymarket_pages requires a supported series selector or https://polymarket.com/event/... URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || len(parts) > 3 || (parts[0] != "event" && parts[0] != "market") || (parts[0] == "market" && len(parts) != 2) {
		return p, fmt.Errorf("unsupported Polymarket page path")
	}
	for _, s := range parts[1:] {
		if !polySlug.MatchString(s) || len(s) > 250 {
			return p, fmt.Errorf("invalid Polymarket slug")
		}
	}
	if parts[0] == "event" {
		p.Event = parts[1]
		if len(parts) == 3 {
			p.Market = parts[2]
		}
	} else {
		p.Market = parts[1]
	}
	sum := sha256.Sum256([]byte(p.Event + "/" + p.Market))
	p.ID = fmt.Sprintf("poly-%x", sum[:12])
	return p, nil
}

type polymarketService struct {
	configured []polymarketPage
	client     *http.Client
}

func newPolymarket(raw []string) (*polymarketService, error) {
	p := &polymarketService{client: &http.Client{Timeout: 18 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 3 || r.URL.Scheme != "https" || r.URL.Host != "gamma-api.polymarket.com" {
			return fmt.Errorf("unexpected Polymarket redirect")
		}
		return nil
	}}}
	for _, s := range raw {
		if strings.TrimSpace(s) == "" {
			continue
		}
		page, err := parsePolymarketPage(s)
		if err != nil {
			return nil, err
		}
		p.configured = append(p.configured, page)
	}
	return p, nil
}
func (p *polymarketService) pages() []polymarketPage {
	if p == nil {
		return nil
	}
	return p.configured
}

type polyMarket struct {
	Slug, Question, GroupItemTitle  string
	Description, StartDate, EndDate string
	Outcomes, OutcomePrices         json.RawMessage
	Closed, Active, Archived        bool
	AcceptingOrders                 *bool
}
type polyEvent struct {
	ID, Slug, Title, Description, StartDate, EndDate string
	Closed, Active, Archived                         bool
	Markets                                          []polyMarket
	Series                                           []polySeries
}

type polySeries struct{ ID, Slug string }

func (p *polymarketService) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://gamma-api.polymarket.com"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "terminal-server (https://github.com/yencarnacion/terminal-server)")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Polymarket HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return fmt.Errorf("Polymarket response too large")
	}
	return json.Unmarshal(data, out)
}
func (p *polymarketService) fetch(ctx context.Context, page polymarketPage) (polyEvent, error) {
	return p.fetchAt(ctx, page, time.Now())
}

func (p *polymarketService) fetchAt(ctx context.Context, page polymarketPage, now time.Time) (polyEvent, error) {
	// Discovery may make several requests; bound the complete operation as well
	// as each HTTP request, so pagination cannot stall the slideshow.
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	if page.Series != "" {
		return p.fetchSeries(ctx, page, now)
	}
	var event polyEvent
	if page.Event != "" {
		if err := p.get(ctx, "/events/slug/"+page.Event, &event); err != nil {
			return event, err
		}
		if page.Market != "" {
			for _, m := range event.Markets {
				if m.Slug == page.Market {
					return polyEvent{Title: m.Question, Closed: m.Closed, Markets: []polyMarket{m}}, nil
				}
			}
			return event, fmt.Errorf("selected market not found in event")
		}
	} else {
		var m polyMarket
		if err := p.get(ctx, "/markets/slug/"+page.Market, &m); err != nil {
			return event, err
		}
		event = polyEvent{Title: m.Question, Closed: m.Closed, Markets: []polyMarket{m}}
	}
	if event.Title == "" || len(event.Markets) == 0 {
		return event, fmt.Errorf("missing event data")
	}
	return event, nil
}

var bestModelDate = regexp.MustCompile(`(?i)^Best AI model on ([a-z]+) ([0-9]{1,2})(?:,? ([0-9]{4}))?\?$`)
var bestModelNoon = regexp.MustCompile(`(?i)12:00\s*PM\s*ET`)
var micronPublicationRule = regexp.MustCompile(`(?i)does not release quarterly earnings materials for the specified quarter by ([a-z]+ [0-9]{1,2}, [0-9]{4}, [0-9]{1,2}:[0-9]{2} [AP]M) ET`)

// Micron's endDate is a scheduled earnings date, not a hard expiration. Its
// resolution rules explicitly permit delayed publication through a later date.
// Use that deadline to bound active markets instead of dropping them early or
// retaining stale quarters indefinitely. Unknown rule formats fail closed.
func polyMicronPublicationDeadline(description string) (time.Time, error) {
	lower := strings.ToLower(description)
	parts := micronPublicationRule.FindAllStringSubmatch(description, -1)
	if len(parts) != 1 || !strings.Contains(lower, "micron") || !strings.Contains(lower, "dram revenue") {
		return time.Time{}, fmt.Errorf("unrecognized Micron DRAM publication deadline")
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.Time{}, err
	}
	deadline, err := time.ParseInLocation("January 2, 2006, 3:04 PM", parts[0][1], loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid Micron DRAM publication deadline: %w", err)
	}
	return deadline, nil
}

// The best-model series observes its leaderboard at noon New York time, even
// though Gamma's endDate is usually that night's end. Never retain a finished
// observation just because the API still marks it active.
func polyObservationTime(page polymarketPage, event polyEvent) (time.Time, error) {
	end, err := time.Parse(time.RFC3339, event.EndDate)
	if err != nil {
		return time.Time{}, fmt.Errorf("missing or invalid event end date")
	}
	if page.Series != "best-ai-model" {
		return end, nil
	}
	parts := bestModelDate.FindStringSubmatch(event.Title)
	if parts == nil || !bestModelNoon.MatchString(event.Description) {
		return time.Time{}, fmt.Errorf("unrecognized best-model observation rule")
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.Time{}, err
	}
	year := parts[3]
	if year == "" {
		year = strconv.Itoa(end.In(loc).Year())
	}
	date, err := time.ParseInLocation("January 2 2006 15:04", parts[1]+" "+parts[2]+" "+year+" 12:00", loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid best-model observation date: %w", err)
	}
	if end.Before(date) {
		return end, nil
	}
	return date, nil
}

func polySeriesMatches(page polymarketPage, event polyEvent, seriesID string) bool {
	member := false
	for _, series := range event.Series {
		if series.ID == seriesID && series.Slug == page.Series {
			member = true
		}
	}
	if !member {
		return false
	}
	if page.Filter == "dram-revenue" {
		return strings.Contains(strings.ToLower(event.Title), "micron") &&
			strings.Contains(strings.ToLower(event.Title), "dram revenue")
	}
	return true
}

func (p *polymarketService) fetchSeries(ctx context.Context, page polymarketPage, now time.Time) (polyEvent, error) {
	var series []polySeries
	q := url.Values{"slug": {page.Series}, "limit": {"100"}}
	if err := p.get(ctx, "/series?"+q.Encode(), &series); err != nil {
		return polyEvent{}, err
	}
	seriesID := ""
	for _, s := range series {
		if s.Slug == page.Series {
			if _, err := strconv.ParseUint(s.ID, 10, 64); err != nil || seriesID != "" {
				return polyEvent{}, fmt.Errorf("invalid or ambiguous Polymarket series")
			}
			seriesID = s.ID
		}
	}
	if seriesID == "" {
		return polyEvent{}, fmt.Errorf("Polymarket series not found")
	}
	const pageSize, maxPages = 20, 10
	var selected polyEvent
	var next time.Time
	seen := make(map[string]bool)
	for batch := 0; batch < maxPages; batch++ {
		q = url.Values{"series_id": {seriesID}, "active": {"true"}, "closed": {"false"}, "archived": {"false"},
			"limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(batch * pageSize)}, "order": {"endDate"}, "ascending": {"true"}}
		var events []polyEvent
		if err := p.get(ctx, "/events?"+q.Encode(), &events); err != nil {
			return polyEvent{}, err
		}
		for _, event := range events {
			if event.Slug == "" || seen[event.Slug] {
				continue
			}
			seen[event.Slug] = true
			if !polySeriesMatches(page, event, seriesID) || !event.Active || event.Closed || event.Archived {
				continue
			}
			if event.StartDate != "" {
				start, err := time.Parse(time.RFC3339, event.StartDate)
				if err != nil || start.After(now) {
					continue
				}
			}
			observation, err := polyObservationTime(page, event)
			if err != nil {
				continue
			}
			deadline := observation
			if page.Filter == "dram-revenue" {
				deadline, err = polyMicronPublicationDeadline(event.Description)
			}
			if err != nil || !deadline.After(now) {
				continue
			}
			var markets []polyMarket
			for _, m := range event.Markets {
				if !m.Active || m.Closed || m.Archived || m.AcceptingOrders == nil || !*m.AcceptingOrders {
					continue
				}
				if m.StartDate != "" {
					start, err := time.Parse(time.RFC3339, m.StartDate)
					if err != nil || start.After(now) {
						continue
					}
				}
				if page.Filter == "dram-revenue" {
					// Markets normally repeat the event rules. Honor a market's
					// own deadline when supplied; otherwise use the event rule.
					marketDeadline := deadline
					var deadlineErr error
					if m.Description != "" {
						marketDeadline, deadlineErr = polyMicronPublicationDeadline(m.Description)
					}
					if deadlineErr != nil || !marketDeadline.After(now) {
						continue
					}
				} else if m.EndDate != "" {
					end, err := time.Parse(time.RFC3339, m.EndDate)
					if err != nil || !end.After(now) {
						continue
					}
				}
				markets = append(markets, m)
			}
			if len(markets) == 0 {
				continue
			}
			if next.IsZero() || observation.Before(next) || (observation.Equal(next) && event.Slug < selected.Slug) {
				event.Markets = markets
				selected, next = event, observation
			}
		}
		if len(events) < pageSize {
			if next.IsZero() {
				return polyEvent{}, errPolyNextEventPending
			}
			return selected, nil
		}
	}
	// Do not choose from an incomplete result set or reuse previous odds.
	return polyEvent{}, fmt.Errorf("Polymarket series exceeded discovery page limit")
}

// Gamma supplies arrays serialized as JSON strings. Also accept native arrays.
func polyArray(raw json.RawMessage) []string {
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		raw = []byte(encoded)
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	var out []string
	for _, v := range values {
		var s string
		if json.Unmarshal(v, &s) != nil {
			s = string(v)
		}
		out = append(out, s)
	}
	return out
}

type polyRow struct {
	label, status string
	probability   *float64
}

func polyRows(event polyEvent) []polyRow {
	var rows []polyRow
	for _, m := range event.Markets {
		if m.Archived {
			continue
		}
		outcomes, prices := polyArray(m.Outcomes), polyArray(m.OutcomePrices)
		status := "OPEN"
		if m.Closed {
			status = "CLOSED"
		} else if !m.Active {
			status = "INACTIVE"
		}
		if event.Closed {
			status = "CLOSED"
		}
		label := m.GroupItemTitle
		if label == "" {
			label = m.Question
		}
		if len(outcomes) == 0 {
			rows = append(rows, polyRow{label: label, status: status})
			continue
		}
		for i, outcome := range outcomes {
			if len(event.Markets) > 1 && len(outcomes) == 2 && strings.EqualFold(outcome, "No") && strings.EqualFold(outcomes[1-i], "Yes") {
				continue
			}
			row := polyRow{label: outcome, status: status}
			if len(event.Markets) > 1 {
				row.label = label + " · " + outcome
			}
			if i < len(prices) {
				v, err := strconv.ParseFloat(prices[i], 64)
				if err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 {
					row.probability = &v
				}
			}
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if (rows[i].status == "OPEN") != (rows[j].status == "OPEN") {
			return rows[i].status == "OPEN"
		}
		if rows[i].probability == nil {
			return false
		}
		if rows[j].probability == nil {
			return true
		}
		return *rows[i].probability > *rows[j].probability
	})
	return rows
}
func (p *polymarketService) render(ctx context.Context, id string, now time.Time) ([]byte, error) {
	for _, page := range p.pages() {
		if !strings.HasPrefix(id, page.ID+"-") {
			continue
		}
		if _, err := strconv.ParseInt(strings.TrimPrefix(id, page.ID+"-"), 10, 64); err != nil {
			return nil, os.ErrNotExist
		}
		event, err := p.fetchAt(ctx, page, now)
		if err != nil {
			log.Printf("polymarket %s: %v", page.ID, err)
		}
		return renderPolymarket(event, now, err)
	}
	return nil, os.ErrNotExist
}

func renderPolymarket(event polyEvent, now time.Time, fetchErr error) ([]byte, error) {
	return renderMarketPage("POLYMARKET", "Polymarket Gamma API", event.Title, polyRows(event), "OUTCOME PRICES", now, fetchErr)
}

func renderMarketPage(brand, source, title string, rows []polyRow, priceLabel string, now time.Time, fetchErr error) ([]byte, error) {
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
		if font.MeasureString(face, label).Ceil() > maxWidth {
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
	text(80, 95, 48, 750, brand, true)
	text(1080, 90, 32, 710, now.Format("Mon, Jan 2, 2006 · 3:04 PM MST"), false)
	rect(80, 128, 1792, 131, 0)
	if errors.Is(fetchErr, errPolyNextEventPending) {
		text(80, 360, 66, 1700, "Next market pending", true)
		text(80, 460, 34, 1700, "The next matching event is not open yet.", false)
		text(80, 525, 34, 1700, "This slide will check again on its next display. No old prices are shown.", false)
	} else if fetchErr != nil {
		text(80, 360, 66, 1700, "Market data temporarily unavailable", true)
		text(80, 460, 34, 1700, "Check the configured page URL and network connection.", false)
		text(80, 525, 34, 1700, "This slide will retry on its next display. No old prices are shown.", false)
	} else {
		// Two bounded title lines, keeping the large odds area readable on e-ink.
		words := strings.Fields(title)
		line := ""
		y := 225
		for _, word := range words {
			if len(line)+len(word) > 56 && line != "" {
				text(80, y, 50, 1700, line, true)
				y += 64
				line = ""
				if y > 289 {
					break
				}
			}
			if line != "" {
				line += " "
			}
			line += word
		}
		if line != "" && y <= 289 {
			text(80, y, 50, 1700, line, true)
		}
		count := len(rows)
		if count > 6 {
			rows = rows[:6]
		}
		text(80, 350, 27, 1700, fmt.Sprintf("%s · %d OF %d SHOWN · OPEN MARKETS FIRST, THEN HIGHEST PRICE", priceLabel, len(rows), count), true)
		if count == 0 {
			text(80, 540, 50, 1700, "No outcome data available", true)
		}
		for i, row := range rows {
			top := 390 + i*140
			text(80, top+45, 36, 1310, row.label, true)
			value := "—"
			if row.probability != nil {
				value = fmt.Sprintf("%.1f%%", *row.probability*100)
			}
			text(1510, top+53, 55, 280, value, true)
			rect(80, top+76, 1360, top+97, 221)
			if row.probability != nil {
				rect(80, top+76, 80+int(1280*(*row.probability)), top+97, 34)
			}
			text(1400, top+95, 23, 380, row.status, false)
		}
	}
	text(80, 1280, 25, 1700, "Source: "+source+" · Fetched "+now.Format("Jan 2, 3:04 PM MST"), false)
	if fetchErr != nil {
		rect(80, 1245, 1792, 1295, 255)
	}
	text(80, 1325, 25, 1700, "Market prices expressed as percentages—not guarantees. Read-only display; no trading.", false)
	if fontErr != nil {
		return nil, fontErr
	}
	return encodeGrayPNG(img)
}
