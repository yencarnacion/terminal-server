package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const scrapflyMaxResponse = 8 << 20
const scrapflyProtocol = "2024-11-05"

// withScrapflyFallback preserves the source client's redirect policy and overall
// timeout. Only public, unauthenticated GET URLs are sent to the configured MCP.
// Successful direct requests do not use Scrapfly.
func withScrapflyFallback(client *http.Client, endpoint string) *http.Client {
	if client == nil || strings.TrimSpace(endpoint) == "" {
		return client
	}
	copy := *client
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copy.Transport = &scrapflyTransport{direct: transport, endpoint: endpoint,
		lookup: net.DefaultResolver.LookupIPAddr,
		mcp: &http.Client{Timeout: 18 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("Scrapfly MCP redirects are not permitted")
		}}}
	return &copy
}

type scrapflyTransport struct {
	direct       http.RoundTripper
	endpoint     string
	mcp          *http.Client
	lookup       func(context.Context, string) ([]net.IPAddr, error)
	mu           sync.Mutex
	session      string
	initialized  bool
	initializing chan struct{}
	counter      atomic.Uint64
}

type scrapflyRPC struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type scrapflyPage struct {
	Content     *string         `json:"content"`
	StatusCode  int             `json:"status_code"`
	ContentType string          `json:"content_type"`
	Errors      json.RawMessage `json:"errors"`
}

type scrapflyMCPStatusError int

func (e scrapflyMCPStatusError) Error() string { return fmt.Sprintf("Scrapfly MCP HTTP %d", int(e)) }

func publicScrapflyURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || !strings.Contains(host, ".") || strings.Contains(host, "%") || host == "localhost" {
		return false
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home", ".test", ".invalid"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	if ip := net.ParseIP(host); ip != nil && !publicScrapflyIP(ip) {
		return false
	}
	for k := range u.Query() {
		key := strings.ToLower(strings.ReplaceAll(k, "-", "_"))
		if strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "signature") || strings.Contains(key, "credential") || key == "key" || key == "api_key" || key == "apikey" || key == "auth" {
			return false
		}
	}
	return true
}

func publicScrapflyIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, raw := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32"} {
		_, network, _ := net.ParseCIDR(raw)
		if network.Contains(ip) {
			return false
		}
	}
	return true
}

func (s *scrapflyTransport) publicDestination(ctx context.Context, u *url.URL) bool {
	if !publicScrapflyURL(u) {
		return false
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		return publicScrapflyIP(ip)
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ips, err := s.lookup(bounded, u.Hostname())
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !publicScrapflyIP(ip.IP) {
			return false
		}
	}
	return true
}

func scrapflyRetryStatus(status int) bool {
	return status == http.StatusForbidden || status == http.StatusRequestTimeout || status == http.StatusTeapot || status == http.StatusTooManyRequests || status >= 500
}

func scrapflyContentProblem(req *http.Request, resp *http.Response, body []byte) bool {
	if resp.StatusCode != http.StatusOK {
		return false
	}
	accept := strings.ToLower(req.Header.Get("Accept"))
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(accept, "json") {
		return !json.Valid(body)
	}
	if strings.Contains(accept, "xml") || strings.Contains(accept, "rss") {
		return strings.Contains(ct, "text/html")
	}
	return false
}

type scrapflyCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *scrapflyCancelBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }
func scrapflyBinaryType(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	return err == nil && !strings.HasPrefix(media, "text/") && !strings.Contains(media, "json") && !strings.Contains(media, "xml")
}

func (s *scrapflyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Do not send credentials, POST bodies or LAN URLs to a cloud fetcher.
	if req.Method != http.MethodGet || !publicScrapflyURL(req.URL) || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
		return s.direct.RoundTrip(req)
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	budget := 6 * time.Second
	if end, ok := req.Context().Deadline(); ok {
		if remaining := time.Until(end) / 3; remaining < budget {
			budget = remaining
		}
	}
	if budget <= 0 {
		return nil, context.DeadlineExceeded
	}
	directCtx, cancel := context.WithCancel(req.Context())
	timer := time.AfterFunc(budget, cancel)
	directReq := req.Clone(directCtx)
	resp, directErr := s.direct.RoundTrip(directReq)
	if directErr == nil && resp.StatusCode == http.StatusOK && scrapflyBinaryType(resp.Header.Get("Content-Type")) {
		// Large images keep the source service's own byte limit and the caller's
		// original timeout once headers arrive. The MCP text envelope cannot carry
		// arbitrary binary assets losslessly.
		timer.Stop()
		if err := directCtx.Err(); err != nil {
			resp.Body.Close()
			cancel()
			return nil, err
		}
		resp.Request = req
		resp.Body = &scrapflyCancelBody{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
	var body []byte
	if directErr == nil {
		body, directErr = io.ReadAll(io.LimitReader(resp.Body, scrapflyMaxResponse+1))
		resp.Body.Close()
		if len(body) > scrapflyMaxResponse {
			directErr = fmt.Errorf("source response exceeds Scrapfly adapter limit")
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.Request = req
	}
	timer.Stop()
	cancel()
	if directErr == nil && !scrapflyRetryStatus(resp.StatusCode) && !scrapflyContentProblem(req, resp, body) {
		return resp, nil
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if !s.publicDestination(req.Context(), req.URL) {
		if directErr != nil {
			return nil, directErr
		}
		return resp, nil
	}
	page, err := s.fetch(req.Context(), req.URL.String())
	if err != nil {
		if directErr != nil {
			return nil, fmt.Errorf("direct fetch failed (%v); Scrapfly fallback failed: %w", directErr, err)
		}
		return nil, fmt.Errorf("source HTTP %d; Scrapfly fallback failed: %w", resp.StatusCode, err)
	}
	content := []byte(*page.Content)
	fallback := &http.Response{StatusCode: page.StatusCode, Status: fmt.Sprintf("%d %s", page.StatusCode, http.StatusText(page.StatusCode)),
		Header: http.Header{"Content-Type": []string{page.ContentType}, "X-Terminal-Source": []string{"scrapfly"}},
		Body:   io.NopCloser(bytes.NewReader(content)), ContentLength: int64(len(content)), Request: req}
	if scrapflyContentProblem(req, fallback, content) {
		return nil, fmt.Errorf("Scrapfly returned invalid source content")
	}
	return fallback, nil
}

// Each initialized MCP session can serve concurrent calls. A server-expired
// session is replaced once; unrelated RPC/tool failures are not retried.
func (s *scrapflyTransport) getSession(ctx context.Context) (string, error) {
	for {
		s.mu.Lock()
		if s.initialized {
			session := s.session
			s.mu.Unlock()
			return session, nil
		}
		if waiting := s.initializing; waiting != nil {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-waiting:
				continue
			}
		}
		waiting := make(chan struct{})
		s.initializing = waiting
		s.mu.Unlock()
		id := s.counter.Add(1)
		msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": "initialize", "params": map[string]any{
			"protocolVersion": scrapflyProtocol, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "terminal-server", "version": "1"}}}
		_, session, err := s.rpc(ctx, "", msg, id)
		if err == nil {
			_, _, err = s.rpc(ctx, session, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, 0)
		}
		s.mu.Lock()
		if err == nil {
			s.session, s.initialized = session, true
		}
		s.initializing = nil
		close(waiting)
		s.mu.Unlock()
		return session, err
	}
}

func scrapflyRemainingTimeout(ctx context.Context) int {
	timeout := 12 * time.Second
	if end, ok := ctx.Deadline(); ok && time.Until(end) < timeout {
		timeout = time.Until(end)
	}
	if timeout < time.Millisecond {
		return 1
	}
	return int(timeout / time.Millisecond)
}

func (s *scrapflyTransport) fetch(ctx context.Context, target string) (scrapflyPage, error) {
	for attempt := 0; attempt < 2; attempt++ {
		session, err := s.getSession(ctx)
		if err != nil {
			return scrapflyPage{}, err
		}
		id := s.counter.Add(1)
		msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{
			"name": "web_scrape", "arguments": map[string]any{"url": target, "format": "raw", "render_js": false, "cache": false, "retry": false, "timeout": scrapflyRemainingTimeout(ctx)}}}
		result, _, err := s.rpc(ctx, session, msg, id)
		if status, ok := err.(scrapflyMCPStatusError); ok && int(status) == 404 && attempt == 0 && session != "" {
			s.mu.Lock()
			if s.session == session {
				s.initialized = false
				s.session = ""
			}
			s.mu.Unlock()
			continue
		}
		if err != nil {
			return scrapflyPage{}, err
		}
		return decodeScrapflyPage(result)
	}
	return scrapflyPage{}, fmt.Errorf("Scrapfly session expired repeatedly")
}

func decodeScrapflyPage(result json.RawMessage) (scrapflyPage, error) {
	var tool struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &tool); err != nil {
		return scrapflyPage{}, fmt.Errorf("decode Scrapfly tool result: %w", err)
	}
	if tool.IsError {
		return scrapflyPage{}, fmt.Errorf("Scrapfly tool reported an error")
	}
	var page scrapflyPage
	raw := tool.StructuredContent
	if len(raw) == 0 || string(raw) == "null" {
		for _, c := range tool.Content {
			if c.Type == "text" {
				raw = json.RawMessage(c.Text)
				break
			}
		}
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return page, fmt.Errorf("decode Scrapfly page: %w", err)
	}
	if page.Content == nil || page.StatusCode < 200 || page.StatusCode >= 300 {
		return page, fmt.Errorf("Scrapfly source HTTP %d or missing content", page.StatusCode)
	}
	if len(page.Errors) > 0 && string(page.Errors) != "null" && string(page.Errors) != "{}" {
		return page, fmt.Errorf("Scrapfly returned a source error")
	}
	if len(*page.Content) > scrapflyMaxResponse {
		return page, fmt.Errorf("Scrapfly content exceeds limit")
	}
	media, _, err := mime.ParseMediaType(page.ContentType)
	if err != nil {
		return page, fmt.Errorf("invalid Scrapfly content type")
	}
	if !strings.HasPrefix(media, "text/") && !strings.Contains(media, "json") && !strings.Contains(media, "xml") {
		return page, fmt.Errorf("Scrapfly raw binary content is not supported (%s)", media)
	}
	return page, nil
}

func (s *scrapflyTransport) rpc(ctx context.Context, session string, message any, id uint64) (json.RawMessage, string, error) {
	body, err := json.Marshal(message)
	if err != nil {
		return nil, "", err
	}
	u, err := url.Parse(s.endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, "", fmt.Errorf("invalid Scrapfly MCP endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", scrapflyProtocol)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := s.mcp.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", scrapflyMCPStatusError(resp.StatusCode)
	}
	newSession := resp.Header.Get("Mcp-Session-Id")
	if id == 0 {
		return nil, newSession, nil
	}
	envelope, err := decodeScrapflyRPC(resp.Body, resp.Header.Get("Content-Type"), id)
	if err != nil {
		return nil, "", err
	}
	if envelope.Error != nil {
		return nil, "", fmt.Errorf("Scrapfly RPC error %d", envelope.Error.Code)
	}
	return envelope.Result, newSession, nil
}

func decodeScrapflyRPC(r io.Reader, contentType string, id uint64) (scrapflyRPC, error) {
	wanted := strconv.FormatUint(id, 10)
	decode := func(data []byte) (scrapflyRPC, bool, error) {
		var msg scrapflyRPC
		if err := json.Unmarshal(data, &msg); err != nil {
			return msg, false, err
		}
		return msg, string(msg.ID) == wanted && msg.JSONRPC == "2.0", nil
	}
	limited := &io.LimitedReader{R: r, N: scrapflyMaxResponse + 1}
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		scanner := bufio.NewScanner(limited)
		scanner.Buffer(make([]byte, 64<<10), scrapflyMaxResponse)
		var data []string
		for scanner.Scan() {
			line := strings.TrimSuffix(scanner.Text(), "\r")
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
			if line == "" && len(data) > 0 {
				msg, match, err := decode([]byte(strings.Join(data, "\n")))
				data = nil
				if err != nil {
					return msg, fmt.Errorf("decode Scrapfly SSE: %w", err)
				}
				if match {
					return msg, nil
				}
			}
			if limited.N <= 0 {
				return scrapflyRPC{}, fmt.Errorf("Scrapfly MCP response exceeds limit")
			}
		}
		if scanner.Err() != nil {
			return scrapflyRPC{}, scanner.Err()
		}
		if len(data) > 0 {
			msg, match, err := decode([]byte(strings.Join(data, "\n")))
			if err == nil && match {
				return msg, nil
			}
		}
		return scrapflyRPC{}, fmt.Errorf("Scrapfly SSE response did not contain matching RPC id")
	}
	data, err := io.ReadAll(limited)
	if err != nil {
		return scrapflyRPC{}, err
	}
	if len(data) > scrapflyMaxResponse {
		return scrapflyRPC{}, fmt.Errorf("Scrapfly MCP response exceeds limit")
	}
	msg, match, err := decode(data)
	if err != nil {
		return msg, fmt.Errorf("decode Scrapfly RPC: %w", err)
	}
	if !match {
		return msg, fmt.Errorf("Scrapfly RPC response id mismatch")
	}
	return msg, nil
}
