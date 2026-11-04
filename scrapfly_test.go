package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type scrapflyTestTransport func(*http.Request) (*http.Response, error)

func (f scrapflyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func scrapflyTestResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

type scrapflyTestMCP struct {
	server                                *httptest.Server
	calls, initializations, notifications atomic.Int32
	expire                                atomic.Bool
	sse                                   bool
	textOnly                              bool
	content                               string
	contentType                           string
	toolError                             bool
}

func newScrapflyTestMCP(t *testing.T, sse bool) *scrapflyTestMCP {
	t.Helper()
	f := &scrapflyTestMCP{sse: sse, content: `{"answer":42}`, contentType: "application/json"}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Error("incorrect MCP request")
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Access_Token") != "" {
			t.Error("credentials forwarded to MCP")
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			n := f.initializations.Add(1)
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", n))
			result = map[string]any{"protocolVersion": scrapflyProtocol, "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "test", "version": "1"}}
		case "notifications/initialized":
			f.notifications.Add(1)
			if r.Header.Get("Mcp-Session-Id") == "" {
				t.Error("notification missing session")
			}
			w.WriteHeader(202)
			return
		case "tools/call":
			f.calls.Add(1)
			if f.expire.CompareAndSwap(true, false) {
				w.WriteHeader(404)
				return
			}
			if r.Header.Get("Mcp-Session-Id") == "" {
				t.Error("tool request missing session")
			}
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(msg.Params, &params); err != nil {
				t.Error(err)
			}
			if params.Name != "web_scrape" || params.Arguments["format"] != "raw" || len(params.Arguments) != 6 || params.Arguments["cache"] != false || params.Arguments["render_js"] != false || params.Arguments["retry"] != false {
				t.Error("unexpected fetch arguments", string(msg.Params))
			}
			if strings.Contains(string(msg.Params), "SECRET") {
				t.Error("secret leaked")
			}
			page := map[string]any{"content": f.content, "status_code": 200, "content_type": f.contentType}
			if f.textOnly {
				raw, _ := json.Marshal(page)
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(raw)}}, "isError": f.toolError}
			} else {
				result = map[string]any{"structuredContent": page, "isError": f.toolError}
			}
		default:
			t.Error("unexpected method", msg.Method)
			w.WriteHeader(400)
			return
		}
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		if f.sse {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, ": ping\n\nevent: message\ndata: %s\n\n", data)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}
func scrapflyTestClient(m *scrapflyTestMCP, direct scrapflyTestTransport) *http.Client {
	c := withScrapflyFallback(&http.Client{Transport: direct, Timeout: 2 * time.Second}, m.server.URL)
	c.Transport.(*scrapflyTransport).lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	return c
}
func scrapflyTestRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://api.example.com/data?limit=50", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	return req
}

func TestScrapflyDirectSuccessAndFallback(t *testing.T) {
	m := newScrapflyTestMCP(t, true)
	status := 200
	fail := false
	badJSON := false
	c := scrapflyTestClient(m, func(r *http.Request) (*http.Response, error) {
		if fail {
			return nil, fmt.Errorf("offline")
		}
		body := `{"direct":true}`
		if badJSON {
			body = `<html>challenge</html>`
		}
		return scrapflyTestResponse(r, status, body), nil
	})
	req := scrapflyTestRequest(t)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if m.calls.Load() != 0 {
		t.Fatal("successful direct request used MCP")
	}
	for _, code := range []int{403, 408, 418, 429, 500, 503} {
		status = code
		resp, err = c.Do(req)
		if err != nil {
			t.Fatalf("HTTP%d fallback: %v", code, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != m.content || resp.Request == nil || resp.Request.URL.String() != req.URL.String() || resp.Header.Get("X-Terminal-Source") != "scrapfly" {
			t.Fatal("fallback response lost source body/request", resp)
		}
	}
	fail = true
	if resp, err = c.Do(req); err != nil {
		t.Fatal("network-error fallback", err)
	} else {
		resp.Body.Close()
	}
	fail = false
	status = 200
	badJSON = true
	if resp, err = c.Do(req); err != nil {
		t.Fatal("HTML challenge fallback", err)
	} else {
		resp.Body.Close()
	}
	if m.initializations.Load() != 1 || m.notifications.Load() != 1 || m.calls.Load() != 8 {
		t.Fatalf("session was not reused: init%d notify%d calls%d", m.initializations.Load(), m.notifications.Load(), m.calls.Load())
	}
	for _, code := range []int{400, 401, 404} {
		status = code
		badJSON = false
		resp, err = c.Do(req)
		if err != nil || resp.StatusCode != code {
			t.Fatal("unexpected retry", code, err)
		}
		resp.Body.Close()
	}
	if m.calls.Load() != 8 {
		t.Fatal("non-retryable source errors used MCP")
	}
}

func TestScrapflySecretsAndPrivateTargetsStayDirect(t *testing.T) {
	m := newScrapflyTestMCP(t, false)
	c := scrapflyTestClient(m, func(r *http.Request) (*http.Response, error) { return scrapflyTestResponse(r, 418, "blocked"), nil })
	tests := []struct{ url, header string }{
		{"http://10.17.17.90:8100/page", ""}, {"https://127.0.0.1/page", ""}, {"https://10.0.0.1/page", ""},
		{"https://100.64.0.1/page", ""}, {"https://169.254.169.254/page", ""}, {"https://host.local/page", ""},
		{"https://intranet/page", ""}, {"https://localhost/page", ""}, {"https://api.example.com:8443/page", ""},
		{"https://user:SECRET@api.example.com/page", ""}, {"https://api.example.com/page?api_key=SECRET", ""},
		{"https://api.example.com/page", "Authorization"}, {"https://api.example.com/page", "Cookie"},
	}
	for _, tc := range tests {
		req, _ := http.NewRequest(http.MethodGet, tc.url, nil)
		if tc.header != "" {
			req.Header.Set(tc.header, "SECRET")
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	// A public-looking DNS name that resolves onto the LAN is also excluded.
	c.Transport.(*scrapflyTransport).lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("192.168.1.1")}}, nil
	}
	req := scrapflyTestRequest(t)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if m.calls.Load() != 0 || m.initializations.Load() != 0 {
		t.Fatal("private or credential-bearing request reached cloud MCP")
	}
	// Unrelated private headers are not serialized to MCP at all.
	c.Transport.(*scrapflyTransport).lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	req.Header.Set("Access_Token", "SECRET")
	req.Header.Set("X-API-Key", "SECRET")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if m.calls.Load() != 1 {
		t.Fatal("public fallback missing")
	}
}

func TestScrapflyJSONTextResultAndExpiredSession(t *testing.T) {
	m := newScrapflyTestMCP(t, false)
	m.textOnly = true
	m.expire.Store(true)
	c := scrapflyTestClient(m, func(r *http.Request) (*http.Response, error) { return scrapflyTestResponse(r, 418, "blocked"), nil })
	resp, err := c.Do(scrapflyTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if m.initializations.Load() != 2 || m.calls.Load() != 2 {
		t.Fatal("expired session not replaced once", m.initializations.Load(), m.calls.Load())
	}
	m.toolError = true
	if _, err = c.Do(scrapflyTestRequest(t)); err == nil {
		t.Fatal("ignored MCP tool error")
	}
}

func TestScrapflyRPCBoundsAndShape(t *testing.T) {
	for _, tc := range []struct{ contentType, body string }{
		{"application/json", `{"jsonrpc":"2.0","id":8,"result":{}}`},
		{"application/json", `{`}, {"application/json", strings.Repeat(" ", scrapflyMaxResponse+1)},
		{"text/event-stream", "event: message\ndata: invalid\n\n"},
		{"text/event-stream", "data: {\"jsonrpc\":\"2.0\",\"id\":8,\"result\":{}}\n\n"},
		{"text/event-stream", "data: " + strings.Repeat(" ", scrapflyMaxResponse+1)},
	} {
		if _, err := decodeScrapflyRPC(strings.NewReader(tc.body), tc.contentType, 1); err == nil {
			t.Fatal("accepted invalid RPC response")
		}
	}
	good := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	for _, ct := range []string{"application/json", "text/event-stream"} {
		body := good
		if ct == "text/event-stream" {
			body = "event: message\ndata: " + good + "\n\n"
		}
		if _, err := decodeScrapflyRPC(strings.NewReader(body), ct, 1); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{
		`{"isError":true}`, `{"structuredContent":{"content":"x","status_code":403,"content_type":"text/plain"}}`,
		`{"structuredContent":{"status_code":200,"content_type":"application/json"}}`,
		`{"structuredContent":{"content":"x","status_code":200,"content_type":"image/png"}}`,
		`{"structuredContent":{"content":"x","status_code":200,"content_type":"text/plain","errors":{"code":"failed"}}}`,
	} {
		if _, err := decodeScrapflyPage(json.RawMessage(bad)); err == nil {
			t.Fatal("accepted invalid tool result", bad)
		}
	}
}

func TestScrapflyCancellationAndTimeBudget(t *testing.T) {
	m := newScrapflyTestMCP(t, false)
	c := scrapflyTestClient(m, func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	c.Timeout = 450 * time.Millisecond
	start := time.Now()
	resp, err := c.Do(scrapflyTestRequest(t))
	if err != nil {
		t.Fatal("direct timeout consumed fallback budget", err)
	}
	resp.Body.Close()
	if time.Since(start) > c.Timeout {
		t.Fatal("exceeded original timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := scrapflyTestRequest(t).WithContext(ctx)
	before := m.calls.Load()
	if _, err := c.Do(req); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled request not honored", err)
	}
	if m.calls.Load() != before {
		t.Fatal("canceled request used fallback")
	}
	// An initialization already in progress must not block a canceled waiter.
	tr := c.Transport.(*scrapflyTransport)
	tr.initialized = false
	tr.initializing = make(chan struct{})
	if _, err := tr.getSession(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("session waiter ignored cancellation", err)
	}
}

func TestScrapflyConcurrentSessionAndClientPolicy(t *testing.T) {
	m := newScrapflyTestMCP(t, false)
	base := &http.Client{Timeout: 18 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("original policy") }, Transport: scrapflyTestTransport(func(r *http.Request) (*http.Response, error) { return scrapflyTestResponse(r, 418, "blocked"), nil })}
	c := withScrapflyFallback(base, m.server.URL)
	c.Transport.(*scrapflyTransport).lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	if c == base || c.Timeout != base.Timeout || c.CheckRedirect(nil, nil).Error() != "original policy" {
		t.Fatal("source client policy changed")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/data", nil)
			resp, err := c.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	if m.initializations.Load() != 1 || m.calls.Load() != 8 {
		t.Fatal("concurrent session initialization", m.initializations.Load(), m.calls.Load())
	}
	if withScrapflyFallback(base, "") != base {
		t.Fatal("empty endpoint should disable fallback")
	}
}

func TestScrapflyRejectedMCPRedirect(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("MCP followed redirect off endpoint")
		w.WriteHeader(200)
	}))
	defer sink.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c := withScrapflyFallback(&http.Client{Transport: scrapflyTestTransport(func(r *http.Request) (*http.Response, error) { return scrapflyTestResponse(r, 418, "blocked"), nil })}, redirect.URL)
	tr := c.Transport.(*scrapflyTransport)
	tr.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	if _, err := c.Do(scrapflyTestRequest(t)); err == nil {
		t.Fatal("accepted MCP redirect")
	}
}

func TestScrapflyOriginalRequestBodyNotSent(t *testing.T) {
	m := newScrapflyTestMCP(t, false)
	c := scrapflyTestClient(m, func(r *http.Request) (*http.Response, error) { return scrapflyTestResponse(r, 418, "blocked"), nil })
	req, _ := http.NewRequest(http.MethodPost, "https://api.example.com/data", bytes.NewBufferString("SECRET"))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if m.calls.Load() != 0 {
		t.Fatal("POST body sent to MCP")
	}
	u, _ := url.Parse("https://api.example.com/data?token=SECRET")
	if publicScrapflyURL(u) {
		t.Fatal("secret query eligible for MCP")
	}
}

func TestScrapflyLargeDirectImagePreservesSourceLimits(t *testing.T) {
	m := newScrapflyTestMCP(t, false)
	var sourceContext context.Context
	c := scrapflyTestClient(m, func(r *http.Request) (*http.Response, error) {
		sourceContext = r.Context()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 10<<20)))}, nil
	})
	req, _ := http.NewRequest(http.MethodGet, "https://images.example.com/frontpage.png", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if sourceContext.Err() != nil {
		t.Fatal("image context canceled before body consumption")
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || len(b) != 10<<20 {
		t.Fatal("direct image incorrectly truncated", len(b), err)
	}
	if sourceContext.Err() == nil {
		t.Fatal("image body close did not release timer context")
	}
	if m.calls.Load() != 0 {
		t.Fatal("direct image used MCP")
	}
}

func TestScrapflyLiveCNN(t *testing.T) {
	endpoint := os.Getenv("SCRAPFLY_LIVE_MCP_URL")
	if endpoint == "" {
		t.Skip("set SCRAPFLY_LIVE_MCP_URL to verify the configured MCP with actual CNN data")
	}
	service := newFearGreed()
	service.client.Transport = scrapflyTestTransport(func(r *http.Request) (*http.Response, error) {
		return scrapflyTestResponse(r, http.StatusTeapot, "upstream rejected direct request"), nil
	})
	service.client = withScrapflyFallback(service.client, endpoint)
	now := time.Now()
	snapshot, err := service.snapshot(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	data, err := renderFearGreed(now, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SCRAPFLY_LIVE_PREVIEW"); path != "" {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("CNN via Scrapfly: score %.1f, rating %s, source %s", *snapshot.Score, snapshot.Rating, snapshot.Timestamp.Format(time.RFC3339))
}
