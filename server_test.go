package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestQuoteParsing(t *testing.T) {
	got, err := parseQuotes("\r\nFirst\r\nline\r\n%\r\nSecond\r\n%\r\n")
	if err != nil || len(got) != 2 || got[0] != "First line" || got[1] != "Second" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := parseQuotes("%\n \n%"); err == nil {
		t.Fatal("accepted empty fortunes")
	}
	if _, err := parseQuotes(strings.Repeat("x", 2001)); err == nil {
		t.Fatal("accepted oversized fortune")
	}
}

func TestDeviceLifecycle(t *testing.T) {
	key, err := loadKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := newApp("http://10.17.17.90:8177", 600, "cowsay", []string{"Turn your wounds into wisdom."}, key)
	if err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	request := func(method, path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("ACCESS_TOKEN", token)
		r.Header.Set("ID", "AA:BB:CC:DD:EE:FF")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/api/display", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	setup := request("GET", "/api/setup", "", "")
	var provision struct {
		APIKey     string `json:"api_key"`
		Status     int    `json:"status"`
		FriendlyID string `json:"friendly_id"`
	}
	if err := json.Unmarshal(setup.Body.Bytes(), &provision); err != nil {
		t.Fatal(err)
	}
	if len(provision.APIKey) > 32 || provision.APIKey == "" || provision.Status != 200 || provision.FriendlyID != "DDEEFF" {
		t.Fatal(setup.Body.String())
	}
	display := request("GET", "/api/display", provision.APIKey, "")
	var payload struct {
		ImageURL string `json:"image_url"`
		Refresh  int    `json:"refresh_rate"`
		Update   bool   `json:"update_firmware"`
	}
	if err := json.Unmarshal(display.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if display.Code != 200 || payload.Refresh != 600 || payload.Update {
		t.Fatal(display.Body.String())
	}
	img := request("GET", payload.ImageURL, "", "")
	if img.Code != 200 || img.Header().Get("Content-Type") != "image/png" {
		t.Fatal(img.Code)
	}
	config, err := png.DecodeConfig(img.Body)
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != 1872 || config.Height != 1404 {
		t.Fatal(config)
	}
	if request("POST", "/api/log", provision.APIKey, `{"logs":[]}`).Code != 204 {
		t.Fatal("log failed")
	}
	if request("POST", "/api/log", provision.APIKey, "invalid").Code != 400 {
		t.Fatal("bad JSON accepted")
	}
	if request("POST", "/api/log", provision.APIKey, strings.Repeat("x", 65537)).Code != 413 {
		t.Fatal("oversized log accepted")
	}
	if request("GET", "/screens/missing.png", "", "").Code != 404 {
		t.Fatal("missing image")
	}
	if request("GET", "/api/display", provision.APIKey+"x", "").Code != 401 {
		t.Fatal("tampered token accepted")
	}
	if request("GET", "/preview", "", "").Code != http.StatusOK {
		t.Fatal("preview failed")
	}
	r := httptest.NewRequest("GET", "/api/display", nil)
	r.Header.Set("Access-Token", provision.APIKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("hyphenated firmware header: %d", w.Code)
	}
}

func TestPersistentKey(t *testing.T) {
	dir := t.TempDir()
	first, err := loadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("key changed on restart")
	}
	info, err := os.Stat(dir + "/device-key")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}

func TestRenderFortunes(t *testing.T) {
	quotes, err := parseQuotes(bundledQuotes)
	if err != nil {
		t.Fatal(err)
	}
	longest := ""
	for _, q := range quotes {
		if len(q) > len(longest) {
			longest = q
		}
	}
	for _, q := range []string{quotes[0], longest, "Done is better than perfect.", strings.Repeat("Unbroken", 100), "Don’t stop—keep going."} {
		data, err := (cowScreen{}).Render(q)
		if err != nil {
			t.Fatal(err)
		}
		if data[24] != 4 || data[25] != 3 {
			t.Fatalf("want indexed 4-bit PNG, got depth=%d type=%d", data[24], data[25])
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != width || img.Bounds().Dy() != height {
			t.Fatal(img.Bounds())
		}
	}
}
