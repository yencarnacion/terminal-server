package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const kalshiAPI = "https://api.elections.kalshi.com/trade-api/v2"

func defaultKalshiPages() []string {
	return []string{
		"https://kalshi.com/markets/kxbtcy/btc-price-range-eoy/kxbtcy-27jan0100",
		"https://kalshi.com/markets/kxbalancepowercombo/congress-balance-of-power-combo/kxbalancepowercombo-27feb",
		"series:KXNETFLIXRANKMOVIE",
		"series:KXNETFLIXRANKSHOWRUNNERUP",
	}
}

type kalshiPage struct{ ID, Event, Series, Label string }

func parseKalshiPage(raw string) (kalshiPage, error) {
	p := kalshiPage{}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "series:") {
		p.Series = strings.ToUpper(strings.TrimPrefix(raw, "series:"))
		if !polySlug.MatchString(p.Series) || len(p.Series) > 100 {
			return p, fmt.Errorf("invalid Kalshi series ticker")
		}
		p.Label = "Kalshi " + p.Series
		switch p.Series {
		case "KXNETFLIXRANKMOVIE":
			p.Label = "Kalshi Netflix Movie"
		case "KXNETFLIXRANKSHOWRUNNERUP":
			p.Label = "Kalshi #2 Netflix Show"
		}
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || (u.Host != "kalshi.com" && u.Host != "www.kalshi.com") || u.User != nil {
			return p, fmt.Errorf("kalshi_pages requires https://kalshi.com/markets/... URLs or series:TICKER")
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 4 || parts[0] != "markets" {
			return p, fmt.Errorf("unsupported Kalshi page path")
		}
		for _, part := range parts[1:] {
			if !polySlug.MatchString(part) || len(part) > 250 {
				return p, fmt.Errorf("invalid Kalshi page")
			}
		}
		p.Event = strings.ToUpper(parts[3])
		if !strings.HasPrefix(p.Event, strings.ToUpper(parts[1])+"-") {
			return p, fmt.Errorf("Kalshi event does not match series")
		}
		p.Label = "Kalshi " + strings.ReplaceAll(parts[2], "-", " ")
	}
	sum := sha256.Sum256([]byte(p.Series + "/" + p.Event))
	p.ID = fmt.Sprintf("kalshi-%x", sum[:12])
	return p, nil
}

type kalshiService struct {
	configured []kalshiPage
	client     *http.Client
}

func newKalshi(raw []string) (*kalshiService, error) {
	k := &kalshiService{client: &http.Client{Timeout: 18 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 3 || r.URL.Scheme != "https" || r.URL.Host != "api.elections.kalshi.com" {
			return fmt.Errorf("unexpected Kalshi redirect")
		}
		return nil
	}}}
	if len(raw) > 4 {
		return nil, fmt.Errorf("kalshi_pages supports up to four entries")
	}
	seen := map[string]bool{}
	for _, s := range raw {
		if strings.TrimSpace(s) == "" {
			continue
		}
		p, err := parseKalshiPage(s)
		if err != nil {
			return nil, err
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("duplicate Kalshi page")
		}
		seen[p.ID] = true
		k.configured = append(k.configured, p)
	}
	return k, nil
}

func (k *kalshiService) pages() []kalshiPage {
	if k == nil {
		return nil
	}
	return k.configured
}

type kalshiMarket struct {
	Ticker    string    `json:"ticker"`
	Title     string    `json:"title"`
	YesTitle  string    `json:"yes_sub_title"`
	Status    string    `json:"status"`
	Result    string    `json:"result"`
	LastPrice string    `json:"last_price_dollars"`
	Volume    string    `json:"volume_fp"`
	OpenTime  time.Time `json:"open_time"`
	CloseTime time.Time `json:"close_time"`
}
type kalshiEvent struct {
	Ticker   string         `json:"event_ticker"`
	Series   string         `json:"series_ticker"`
	Title    string         `json:"title"`
	Subtitle string         `json:"sub_title"`
	Markets  []kalshiMarket `json:"markets"`
}

func (k *kalshiService) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, kalshiAPI+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "terminal-server (https://github.com/yencarnacion/terminal-server)")
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Kalshi HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return fmt.Errorf("Kalshi response too large")
	}
	return json.Unmarshal(data, out)
}

// The nearest closing event with an active market is the current tradable week.
// Never fall back to a settled week when the next weekly event is not available.
func currentKalshiEvent(events []kalshiEvent, series string, now time.Time) (kalshiEvent, error) {
	var selected kalshiEvent
	var earliest time.Time
	for _, event := range events {
		if event.Series != series {
			continue
		}
		for _, m := range event.Markets {
			if m.Status != "active" || m.OpenTime.After(now) || !m.CloseTime.After(now) {
				continue
			}
			if earliest.IsZero() || m.CloseTime.Before(earliest) || (m.CloseTime.Equal(earliest) && event.Ticker < selected.Ticker) {
				selected, earliest = event, m.CloseTime
			}
		}
	}
	if earliest.IsZero() {
		return selected, fmt.Errorf("no current open weekly Kalshi event")
	}
	return selected, nil
}

func (k *kalshiService) fetch(ctx context.Context, p kalshiPage, now time.Time) (kalshiEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	var event kalshiEvent
	if p.Event != "" {
		var response struct {
			Event kalshiEvent `json:"event"`
		}
		if err := k.get(ctx, "/events/"+p.Event+"?with_nested_markets=true", &response); err != nil {
			return event, err
		}
		event = response.Event
		if event.Ticker != p.Event {
			return event, fmt.Errorf("unexpected Kalshi event")
		}
	} else {
		var events []kalshiEvent
		cursor := ""
		for page := 0; ; page++ {
			if page >= 20 {
				return event, fmt.Errorf("too many Kalshi event pages")
			}
			query := url.Values{"series_ticker": {p.Series}, "status": {"open"}, "with_nested_markets": {"true"}, "limit": {"200"}}
			if cursor != "" {
				query.Set("cursor", cursor)
			}
			var response struct {
				Events []kalshiEvent `json:"events"`
				Cursor string        `json:"cursor"`
			}
			if err := k.get(ctx, "/events?"+query.Encode(), &response); err != nil {
				return event, err
			}
			events = append(events, response.Events...)
			if response.Cursor == "" {
				break
			}
			if response.Cursor == cursor {
				return event, fmt.Errorf("repeated Kalshi cursor")
			}
			cursor = response.Cursor
		}
		var err error
		event, err = currentKalshiEvent(events, p.Series, now)
		if err != nil {
			return event, err
		}
	}
	if event.Title == "" || len(event.Markets) == 0 {
		return event, fmt.Errorf("missing Kalshi event data")
	}
	return event, nil
}

func kalshiRows(event kalshiEvent) []polyRow {
	rows := make([]polyRow, 0, len(event.Markets))
	for _, m := range event.Markets {
		label := m.YesTitle
		if label == "" {
			label = m.Title
		}
		if label == "" {
			label = m.Ticker
		}
		row := polyRow{label: label, status: strings.ToUpper(m.Status)}
		if m.Status == "active" {
			row.status = "OPEN"
		}
		value, err := strconv.ParseFloat(m.LastPrice, 64)
		volume, volErr := strconv.ParseFloat(m.Volume, 64)
		if err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1 && volErr == nil && volume > 0 {
			row.probability = &value
		}
		if m.Result == "yes" || m.Result == "no" {
			result := 0.0
			if m.Result == "yes" {
				result = 1
			}
			row.probability = &result
			row.status = "SETTLED"
		}
		rows = append(rows, row)
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

func (k *kalshiService) render(ctx context.Context, id string, now time.Time) ([]byte, error) {
	for _, p := range k.pages() {
		if !strings.HasPrefix(id, p.ID+"-") {
			continue
		}
		if _, err := strconv.ParseInt(strings.TrimPrefix(id, p.ID+"-"), 10, 64); err != nil {
			return nil, os.ErrNotExist
		}
		event, err := k.fetch(ctx, p, now)
		if err != nil {
			log.Printf("kalshi %s: %v", p.ID, err)
		}
		title := event.Title
		if event.Subtitle != "" {
			title += " · " + event.Subtitle
		}
		return renderMarketPage("KALSHI", "Kalshi API", title, kalshiRows(event), "LAST TRADE / SETTLEMENT", now, err)
	}
	return nil, os.ErrNotExist
}
