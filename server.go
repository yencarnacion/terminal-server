package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type app struct {
	news       *newsService
	base       string
	refresh    int
	key        []byte
	screen     Screen
	quotes     map[string]string
	ids        []string
	mu         sync.Mutex
	cache      map[string][]byte
	mode       string
	location   *time.Location
	now        func() time.Time
	playMu     sync.Mutex
	next       map[string]int
	frontpages *frontpages
	weather    *weatherService
	slideOrder []string
	polymarket *polymarketService
}

func loadKey(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "device-key")
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("invalid device-key")
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(key)
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	return key, closeErr
}

func newApp(base string, refresh int, screenName string, quotes []string, key []byte) (*app, error) {
	if screenName != "rss" && screenName != "cowsay" && screenName != "calendar" && screenName != "quarter" && screenName != "slideshow" && screenName != "weather" {
		return nil, fmt.Errorf("unknown screen %q", screenName)
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, err
	}
	a := &app{base: base, refresh: refresh, key: key, screen: cowScreen{}, quotes: map[string]string{}, cache: map[string][]byte{}, mode: screenName, location: location, now: time.Now, next: map[string]int{}}
	for _, q := range quotes {
		sum := sha256.Sum256([]byte("cowsay-v1:" + q))
		id := hex.EncodeToString(sum[:16])
		if _, exists := a.quotes[id]; !exists {
			a.ids = append(a.ids, id)
		}
		a.quotes[id] = q
	}
	if len(a.ids) == 0 {
		return nil, fmt.Errorf("no quotes")
	}
	return a, nil
}

func (a *app) token(id string) string {
	mac, _ := net.ParseMAC(id)
	m := hmac.New(sha256.New, a.key)
	m.Write(mac)
	return base64.RawURLEncoding.EncodeToString(append(mac, m.Sum(nil)[:16]...))
}
func (a *app) authorized(r *http.Request) bool {
	token := r.Header.Get("Access_Token")
	if token == "" {
		token = r.Header.Get("Access-Token")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 22 {
		return false
	}
	return hmac.Equal([]byte(token), []byte(a.token(net.HardwareAddr(decoded[:6]).String())))
}
func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
func (a *app) randomID() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(a.ids))))
	if err != nil {
		return "", err
	}
	return a.ids[n.Int64()], nil
}
func (a *app) image(id string) ([]byte, error) {
	return a.imageContext(context.Background(), id)
}

func (a *app) imageContext(ctx context.Context, id string) ([]byte, error) {
	if strings.HasPrefix(id, "rss-") {
		base, battery, err := splitBatteryID(id)
		if err != nil || a.news == nil {
			return nil, os.ErrNotExist
		}
		if _, err := time.Parse("20060102T1504Z", strings.TrimPrefix(base, "rss-")); err != nil {
			return nil, os.ErrNotExist
		}
		data, err := a.news.render()
		if err != nil {
			return nil, err
		}
		return addBatteryFooter(data, battery)
	}
	if strings.HasPrefix(id, "poly-") {
		base, battery, err := splitBatteryID(id)
		if err != nil || a.polymarket == nil {
			return nil, os.ErrNotExist
		}
		data, err := a.polymarket.render(ctx, base, a.now().In(a.location))
		if err != nil {
			return nil, err
		}
		return addBatteryFooter(data, battery)
	}
	if strings.HasPrefix(id, "weather-") {
		base, battery, err := splitBatteryID(id)
		if err != nil || a.weather == nil {
			return nil, os.ErrNotExist
		}
		if _, err := time.Parse("20060102T1504Z", strings.TrimPrefix(base, "weather-")); err != nil {
			return nil, os.ErrNotExist
		}
		data, err := a.weather.render(ctx, a.now())
		if err != nil {
			return nil, err
		}
		return addBatteryFooter(data, battery)
	}
	// Newspaper images bypass the rendered-image cache entirely.
	if strings.HasPrefix(id, "cover-") {
		base, battery, err := splitBatteryID(id)
		if err != nil {
			return nil, os.ErrNotExist
		}
		if a.frontpages == nil {
			return nil, os.ErrNotExist
		}
		bounded, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		data, err := a.frontpages.render(bounded, base)
		if err != nil {
			return nil, err
		}
		return addBatteryFooter(data, battery)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if data, ok := a.cache[id]; ok {
		return data, nil
	}
	cacheID := id
	id, battery, parseErr := splitBatteryID(id)
	if parseErr != nil {
		return nil, os.ErrNotExist
	}
	var data []byte
	var err error
	if strings.HasPrefix(id, "calendar-") || strings.HasPrefix(id, "quarter-") {
		kind, timestamp, _ := strings.Cut(id, "-")
		stamp, parseErr := time.Parse("20060102T1504Z", timestamp)
		if parseErr != nil || stamp.Year() < 2000 || stamp.Year() > 2100 {
			return nil, os.ErrNotExist
		}
		var renderer Screen = calendarScreen{location: a.location}
		if kind == "quarter" {
			renderer = quarterScreen{location: a.location}
		}
		data, err = renderer.Render(stamp.Format(time.RFC3339))
	} else {
		quote, ok := a.quotes[id]
		if !ok {
			return nil, os.ErrNotExist
		}
		data, err = a.screen.Render(quote)
	}
	if err != nil {
		return nil, err
	}
	data, err = addBatteryFooter(data, battery)
	if err != nil {
		return nil, err
	}
	// Bounded cache; evicted URLs can always be rendered again from their quote.
	if len(a.cache) >= 128 {
		a.cache = map[string][]byte{}
	}
	a.cache[cacheID] = data
	return data, nil
}

func (a *app) screenID(mode string, now time.Time) (string, error) {
	if strings.HasPrefix(mode, "cover-") || strings.HasPrefix(mode, "poly-") {
		return fmt.Sprintf("%s-%d", mode, now.UnixNano()), nil
	}
	if mode == "rss" || mode == "calendar" || mode == "quarter" || mode == "weather" {
		return mode + "-" + now.UTC().Format("20060102T1504Z"), nil
	}
	return a.randomID()
}

func (a *app) playlist() []string {
	order := a.slideOrder
	if order == nil {
		order = defaultConfiguration().SlideOrder
	}
	var slides []string
	for _, name := range order {
		switch name {
		case "rss":
			if len(a.news.snapshot()) > 0 {
				slides = append(slides, "rss")
			}
		case "weather":
			if a.weather != nil {
				slides = append(slides, "weather")
			}
		case "quote":
			slides = append(slides, "cowsay")
		case "calendar", "quarter":
			slides = append(slides, name)
		case "newspapers":
			for _, cover := range a.frontpages.snapshot() {
				slides = append(slides, cover.ID)
			}
		case "polymarket":
			for _, page := range a.polymarket.pages() {
				slides = append(slides, page.ID)
			}
		}
	}
	// Keep the device usable while an exclusively external playlist is unavailable.
	if len(slides) == 0 {
		slides = []string{"cowsay"}
	}
	return slides
}

// Each device advances independently after successful display metadata delivery.
// Cover bytes are fetched live on the subsequent image GET, never prefetched.
// Preview/image downloads never consume a device's next slideshow item.
func (a *app) nextDisplay(r *http.Request) (string, error) {
	a.playMu.Lock()
	defer a.playMu.Unlock()
	token := r.Header.Get("Access_Token")
	if token == "" {
		token = r.Header.Get("Access-Token")
	}
	mode := a.mode
	playlist := a.playlist()
	index := a.next[token] % len(playlist)
	if mode == "slideshow" {
		mode = playlist[index]
	}
	id, err := a.screenID(mode, a.now())
	if err == nil {
		id += readBattery(r.Header).suffix()
		if !strings.HasPrefix(id, "rss-") && !strings.HasPrefix(id, "cover-") && !strings.HasPrefix(id, "weather-") && !strings.HasPrefix(id, "poly-") {
			_, err = a.image(id)
		}
	}
	if err == nil && a.mode == "slideshow" {
		if len(a.next) >= 256 {
			if _, ok := a.next[token]; !ok {
				a.next = map[string]int{}
			}
		}
		a.next[token] = (index + 1) % len(playlist)
	}
	return id, err
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]any{"status": "ok", "screen": a.mode, "timezone": a.location.String(), "refresh_rate": a.refresh, "width": width, "height": height, "quotes": len(a.ids), "covers": len(a.frontpages.snapshot())})
	})
	mux.HandleFunc("GET /api/setup", func(w http.ResponseWriter, r *http.Request) {
		mac, err := net.ParseMAC(r.Header.Get("ID"))
		if err != nil || len(mac) != 6 {
			http.Error(w, "ID header must be a MAC address", 400)
			return
		}
		id := a.ids[0] + readBattery(r.Header).suffix()
		if _, err = a.image(id); err != nil {
			http.Error(w, "render failed", 500)
			return
		}
		jsonResponse(w, map[string]any{"status": 200, "api_key": a.token(mac.String()), "friendly_id": strings.ToUpper(hex.EncodeToString(mac[3:])), "filename": id, "image_url": a.base + "/screens/" + id + ".png", "message": "Welcome to Terminal Server"})
	})
	mux.HandleFunc("GET /api/display", func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			http.Error(w, "invalid ACCESS_TOKEN; provision via /api/setup", 401)
			return
		}
		id, err := a.nextDisplay(r)
		if err != nil {
			http.Error(w, "render failed", 500)
			return
		}
		log.Printf("display served: model=%q firmware=%q image=%s", r.Header.Get("Model"), r.Header.Get("Fw_Version"), id)
		jsonResponse(w, map[string]any{"status": 0, "image_url": a.base + "/screens/" + id + ".png", "filename": id, "refresh_rate": a.refresh, "update_firmware": false, "firmware_url": nil, "reset_firmware": false})
	})
	mux.HandleFunc("POST /api/log", func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			http.Error(w, "invalid ACCESS_TOKEN", 401)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, "log payload too large", 413)
			return
		}
		if !json.Valid(body) {
			http.Error(w, "invalid JSON", 400)
			return
		}
		log.Printf("device log: %s", body)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /screens/{filename}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("filename")
		if !strings.HasSuffix(name, ".png") {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimSuffix(name, ".png")
		isLive := strings.HasPrefix(id, "rss-") || strings.HasPrefix(id, "cover-") || strings.HasPrefix(id, "weather-") || strings.HasPrefix(id, "poly-")
		if isLive {
			w.Header().Set("Cache-Control", "no-store, no-cache, max-age=0")
			w.Header().Set("Pragma", "no-cache")
		}
		data, err := a.imageContext(r.Context(), id)
		if err == os.ErrNotExist {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			log.Printf("screen render: %v", err)
			http.Error(w, "render failed", 500)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if !isLive {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		_, _ = w.Write(data)
	})
	mux.HandleFunc("GET /preview", func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("screen")
		if mode == "" {
			mode = a.mode
		}
		covers := a.frontpages.snapshot()
		links := ""
		if a.news != nil {
			links += `<a href="/preview?screen=rss">Top News</a>`
		}
		if a.weather != nil {
			links += `<a href="/preview?screen=weather">Weather</a>`
		}
		for _, cover := range covers {
			links += fmt.Sprintf(`<a href="/preview?screen=%s">%s</a>`, cover.ID, html.EscapeString(cover.Name))
		}
		isCover := false
		for i, page := range a.polymarket.pages() {
			links += fmt.Sprintf(`<a href="/preview?screen=%s">Polymarket %d</a>`, page.ID, i+1)
			if mode == page.ID {
				isCover = true
			}
		}
		for _, cover := range covers {
			if mode == cover.ID {
				isCover = true
			}
		}
		if !(mode == "rss" && a.news != nil) && mode != "slideshow" && mode != "calendar" && mode != "quarter" && mode != "cowsay" && !(mode == "weather" && a.weather != nil) && !isCover {
			http.Error(w, "unknown screen", 400)
			return
		}
		now := a.now()
		if mode == "slideshow" {
			playlist := a.playlist()
			mode = playlist[now.Unix()/int64(a.refresh)%int64(len(playlist))]
		}
		id, err := a.screenID(mode, now)
		if err != nil {
			http.Error(w, "quote selection failed", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="%d"><title>Terminal Server · Terminal Server</title><style>body{margin:0;padding:24px;background:#e6e5e1;font:16px system-ui;color:#222}header{max-width:1000px;margin:0 auto 20px;display:flex;justify-content:space-between;gap:20px;align-items:center;flex-wrap:wrap}h1{font-size:20px;margin:0 0 6px}p{margin:0;color:#555}a{color:inherit;margin-right:14px}img{display:block;width:100%%;max-width:1000px;height:auto;margin:auto;background:white;box-shadow:0 4px 24px #0002}</style><header><div><h1>Terminal Server</h1><p>TRMNL X · 1872 × 1404 · %d seconds per screen · time shown is time at refresh</p></div><nav><a href="/preview">Slideshow</a><a href="/preview?screen=calendar">Calendar</a><a href="/preview?screen=quarter">Quarter Progress</a><a href="/preview?screen=cowsay">Fortune ↻</a>%s</nav></header><img src="/screens/%s.png" width="1872" height="1404" alt="%s screen"></html>`, a.refresh, a.refresh, links, id, mode)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/preview", http.StatusSeeOther) })
	return mux
}
