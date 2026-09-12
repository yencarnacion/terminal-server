package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	base     string
	refresh  int
	key      []byte
	screen   Screen
	quotes   map[string]string
	ids      []string
	mu       sync.Mutex
	cache    map[string][]byte
	mode     string
	location *time.Location
	now      func() time.Time
	playMu   sync.Mutex
	next     map[string]bool
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
	if screenName != "cowsay" && screenName != "calendar" && screenName != "slideshow" {
		return nil, fmt.Errorf("unknown screen %q", screenName)
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, err
	}
	a := &app{base: base, refresh: refresh, key: key, screen: cowScreen{}, quotes: map[string]string{}, cache: map[string][]byte{}, mode: screenName, location: location, now: time.Now, next: map[string]bool{}}
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if data, ok := a.cache[id]; ok {
		return data, nil
	}
	var data []byte
	var err error
	if strings.HasPrefix(id, "calendar-") {
		stamp, parseErr := time.Parse("20060102T1504Z", strings.TrimPrefix(id, "calendar-"))
		if parseErr != nil || stamp.Year() < 2000 || stamp.Year() > 2100 {
			return nil, os.ErrNotExist
		}
		data, err = (calendarScreen{location: a.location}).Render(stamp.Format(time.RFC3339))
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
	// Bounded cache; evicted URLs can always be rendered again from their quote.
	if len(a.cache) >= 128 {
		a.cache = map[string][]byte{}
	}
	a.cache[id] = data
	return data, nil
}

func (a *app) screenID(mode string, now time.Time) (string, error) {
	if mode == "calendar" {
		return "calendar-" + now.UTC().Format("20060102T1504Z"), nil
	}
	return a.randomID()
}

// Each device advances independently, only after a successfully rendered screen.
// Preview/image downloads never consume a device's next slideshow item.
func (a *app) nextDisplay(r *http.Request) (string, error) {
	a.playMu.Lock()
	defer a.playMu.Unlock()
	token := r.Header.Get("Access_Token")
	if token == "" {
		token = r.Header.Get("Access-Token")
	}
	mode := a.mode
	if mode == "slideshow" {
		mode = "cowsay"
		if a.next[token] {
			mode = "calendar"
		}
	}
	id, err := a.screenID(mode, a.now())
	if err == nil {
		_, err = a.image(id)
	}
	if err == nil && a.mode == "slideshow" {
		if len(a.next) >= 256 {
			if _, ok := a.next[token]; !ok {
				a.next = map[string]bool{}
			}
		}
		a.next[token] = !a.next[token]
	}
	return id, err
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]any{"status": "ok", "screen": a.mode, "timezone": a.location.String(), "refresh_rate": a.refresh, "width": width, "height": height, "quotes": len(a.ids)})
	})
	mux.HandleFunc("GET /api/setup", func(w http.ResponseWriter, r *http.Request) {
		mac, err := net.ParseMAC(r.Header.Get("ID"))
		if err != nil || len(mac) != 6 {
			http.Error(w, "ID header must be a MAC address", 400)
			return
		}
		id := a.ids[0]
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
		data, err := a.image(id)
		if err == os.ErrNotExist {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "render failed", 500)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("GET /preview", func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("screen")
		if mode == "" {
			mode = a.mode
		}
		if mode != "slideshow" && mode != "calendar" && mode != "cowsay" {
			http.Error(w, "unknown screen", 400)
			return
		}
		now := a.now()
		if mode == "slideshow" {
			mode = "cowsay"
			if now.Unix()/int64(a.refresh)%2 == 1 {
				mode = "calendar"
			}
		}
		id, err := a.screenID(mode, now)
		if err != nil {
			http.Error(w, "quote selection failed", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="%d"><title>Calendar & Fortune · Terminal Server</title><style>body{margin:0;padding:24px;background:#e6e5e1;font:16px system-ui;color:#222}header{max-width:1000px;margin:0 auto 20px;display:flex;justify-content:space-between;gap:20px;align-items:center;flex-wrap:wrap}h1{font-size:20px;margin:0 0 6px}p{margin:0;color:#555}a{color:inherit;margin-right:14px}img{display:block;width:100%%;max-width:1000px;height:auto;margin:auto;background:white;box-shadow:0 4px 24px #0002}</style><header><div><h1>Calendar & Fortune</h1><p>TRMNL X · 1872 × 1404 · %d seconds per screen · time shown is time at refresh</p></div><nav><a href="/preview">Slideshow</a><a href="/preview?screen=calendar">Calendar</a><a href="/preview?screen=cowsay">Fortune ↻</a></nav></header><img src="/screens/%s.png" width="1872" height="1404" alt="%s screen"></html>`, a.refresh, a.refresh, id, mode)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/preview", http.StatusSeeOther) })
	return mux
}
