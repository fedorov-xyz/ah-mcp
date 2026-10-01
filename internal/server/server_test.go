package server_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/ah"
	"github.com/fedorov-xyz/ah-mcp/internal/ahlogin"
	"github.com/fedorov-xyz/ah-mcp/internal/cache"
	"github.com/fedorov-xyz/ah-mcp/internal/config"
	"github.com/fedorov-xyz/ah-mcp/internal/oauth"
	"github.com/fedorov-xyz/ah-mcp/internal/oauth/seal"
	"github.com/fedorov-xyz/ah-mcp/internal/server"
	"github.com/fedorov-xyz/ah-mcp/internal/testutil/fakeah"
	"github.com/fedorov-xyz/ah-mcp/internal/tools"
)

type stack struct {
	t      *testing.T
	fake   *fakeah.Fake
	ts     *httptest.Server
	client *http.Client
	cfg    *config.Config
	logs   *bytes.Buffer

	mu      sync.Mutex
	allowed map[string]bool // nil = everyone
}

func (s *stack) setAllowed(emails ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowed = map[string]bool{}
	for _, e := range emails {
		s.allowed[e] = true
	}
}

func (s *stack) emailAllowed(email string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allowed == nil || s.allowed[strings.ToLower(email)]
}

func newStack(t *testing.T, mutate ...func(*config.Config)) *stack {
	t.Helper()
	s := &stack{t: t, fake: fakeah.New(t), logs: &bytes.Buffer{}}
	var h http.Handler
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(s.ts.Close)
	s.cfg = &config.Config{
		BaseURL:        s.ts.URL,
		SessionTTL:     720 * time.Hour,
		AllowedOrigins: []string{s.ts.URL, "https://claude.ai"},
	}
	for _, m := range mutate {
		m(s.cfg)
	}
	logger := slog.New(slog.NewJSONHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sealer, _ := seal.New([]byte("0123456789abcdef0123456789abcdef"))
	as, err := oauth.New(oauth.Options{
		BaseURL: s.cfg.BaseURL, SessionTTL: s.cfg.SessionTTL, EmailAllowed: s.emailAllowed, Sealer: sealer,
		AH: ahlogin.New(s.fake.URL(), s.fake.URL(), nil), AHLoginBase: s.fake.URL(), Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := &tools.Deps{AH: ah.NewFactory(s.fake.URL()), Cache: cache.New(100), Version: "e2e", Started: time.Now(), Logger: logger}
	h = server.New(server.Options{Config: s.cfg, OAuth: as, Tools: deps, Logger: logger})
	jar, _ := cookiejar.New(nil)
	s.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return s
}

func (s *stack) get(path string) *http.Response {
	s.t.Helper()
	resp, err := s.client.Get(s.ts.URL + path)
	if err != nil {
		s.t.Fatal(err)
	}
	return resp
}

func body(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	return m
}

const redirect = "http://127.0.0.1:33418/callback"
const verifier = "e2e-verifier-0123456789-0123456789-0123456789-abcdef"

// signIn runs DCR → authorize → consent → login proxy → fake AH → callback
// → token, returning the token response.
func (s *stack) signIn() map[string]any {
	t := s.t
	t.Helper()
	resp, err := s.client.Post(s.ts.URL+"/oauth/register", "application/json",
		strings.NewReader(`{"redirect_uris":["`+redirect+`"],"client_name":"E2E","grant_types":["authorization_code","refresh_token"]}`))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %v %v", err, resp.Status)
	}
	clientID := decode(t, resp)["client_id"].(string)

	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"xyz"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "resource": {s.ts.URL + "/mcp"}}
	resp = s.get("/oauth/authorize?" + q.Encode())
	page := body(resp)
	m := regexp.MustCompile(`href="(/oauth/ah/[^"]+/start)"`).FindStringSubmatch(page)
	if resp.StatusCode != 200 || m == nil {
		t.Fatalf("consent page: %d %s", resp.StatusCode, page)
	}

	// "Sign in" sets the login cookie and sends the browser to the AH login
	// page, relayed from the root of our origin.
	resp = s.get(m[1])
	loginPath := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(loginPath, "/login?") {
		t.Fatalf("start: %d %q", resp.StatusCode, loginPath)
	}

	// The proxied AH login redirects to appie://login-exit, rewritten to our callback.
	resp = s.get(loginPath)
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, s.ts.URL+"/oauth/ah/") || !strings.Contains(loc, "/callback?code=") {
		t.Fatalf("login proxy: %d %q", resp.StatusCode, loc)
	}
	resp = s.get(strings.TrimPrefix(loc, s.ts.URL))
	final, _ := url.Parse(resp.Header.Get("Location"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(final.String(), redirect) || final.Query().Get("state") != "xyz" || final.Query().Get("iss") != s.ts.URL {
		t.Fatalf("callback: %d %s", resp.StatusCode, final)
	}

	form := url.Values{"grant_type": {"authorization_code"}, "code": {final.Query().Get("code")}, "client_id": {clientID},
		"redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {s.ts.URL + "/mcp"}}
	resp, err = s.client.PostForm(s.ts.URL+"/oauth/token", form)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("token: %v %s", err, body(resp))
	}
	tok := decode(t, resp)
	tok["client_id"] = clientID
	return tok
}

type rpcClient struct {
	s       *stack
	token   string
	session string
	id      int
}

func (c *rpcClient) send(method string, params any, notify bool) (*http.Response, map[string]any) {
	t := c.s.t
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	if !notify {
		c.id++
		msg["id"] = c.id
	}
	b, _ := json.Marshal(msg)
	req, _ := http.NewRequest("POST", c.s.ts.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	if notify || resp.StatusCode != 200 {
		return resp, nil
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") {
				raw = []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: bad JSON-RPC response %q", method, raw)
	}
	return resp, out
}

func TestEndToEndOAuthAndMCP(t *testing.T) {
	s := newStack(t)

	// Unauthenticated /mcp is challenged.
	c := &rpcClient{s: s}
	resp, _ := c.send("initialize", map[string]any{}, false)
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != `Bearer resource_metadata="`+s.ts.URL+`/.well-known/oauth-protected-resource/mcp"` {
		t.Fatalf("challenge: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	tok := s.signIn()
	c.token = tok["access_token"].(string)
	_, init := c.send("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "1"}}, false)
	result, _ := init["result"].(map[string]any)
	if result == nil || result["serverInfo"].(map[string]any)["name"] != "ah-mcp" || result["instructions"] == nil {
		t.Fatalf("initialize: %v", init)
	}
	if resp, _ := c.send("notifications/initialized", nil, true); resp.StatusCode/100 != 2 {
		t.Fatalf("initialized: %d", resp.StatusCode)
	}
	_, lst := c.send("tools/list", map[string]any{}, false)
	toolsList := lst["result"].(map[string]any)["tools"].([]any)
	if len(toolsList) != 25 {
		t.Fatalf("tools/list returned %d tools, want 25", len(toolsList))
	}

	// Refresh: AH rotates its refresh token; we issue a new pair.
	resp, err := s.client.PostForm(s.ts.URL+"/oauth/token", url.Values{"grant_type": {"refresh_token"},
		"refresh_token": {tok["refresh_token"].(string)}, "client_id": {tok["client_id"].(string)}})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("refresh: %v %s", err, body(resp))
	}
	refreshed := decode(t, resp)
	if refreshed["access_token"] == tok["access_token"] || refreshed["refresh_token"] == tok["refresh_token"] || s.fake.RefreshCalls.Load() != 1 {
		t.Fatalf("refresh did not rotate: %v", refreshed)
	}

	// Tool call with the new token.
	c.token = refreshed["access_token"].(string)
	_, call := c.send("tools/call", map[string]any{"name": "ah_whoami", "arguments": map[string]any{}}, false)
	res := call["result"].(map[string]any)
	if res["isError"] == true || !strings.Contains(fmt.Sprint(res["structuredContent"]), "*********0017") {
		t.Fatalf("tool call: %v", call)
	}
	if s.fake.RefreshCalls.Load() != 1 {
		t.Fatal("AH refreshed during the tool call")
	}

	// Logs never contain tokens, codes, emails or the login sid.
	logs := s.logs.String()
	for _, leak := range []string{tok["access_token"].(string)[:20], tok["refresh_token"].(string)[:20], "test.user@example.com", fakeah.LoginCode, "ah-access-", "ah-refresh-"} {
		if strings.Contains(logs, leak) {
			t.Errorf("logs leak %q", leak)
		}
	}
	if regexp.MustCompile(`/oauth/ah/[A-Za-z0-9_-]{20,}`).MatchString(logs) {
		t.Error("logs leak the login sid")
	}
}

func TestExpiredAHTokenIsChallenged(t *testing.T) {
	s := newStack(t)
	s.fake.ExpiresIn = 62 // our access token lives 2 s
	tok := s.signIn()
	time.Sleep(2100 * time.Millisecond)
	c := &rpcClient{s: s, token: tok["access_token"].(string)}
	resp, _ := c.send("tools/list", map[string]any{}, false)
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("expired token: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if s.fake.RefreshCalls.Load() != 0 {
		t.Fatal("server refreshed AH on its own")
	}
}

func TestAllowlistRemovalRevokesNextRequest(t *testing.T) {
	s := newStack(t)
	s.setAllowed("test.user@example.com")
	tok := s.signIn()
	c := &rpcClient{s: s, token: tok["access_token"].(string)}
	c.send("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "1"}}, false)
	if resp, _ := c.send("tools/list", map[string]any{}, false); resp.StatusCode != 200 {
		t.Fatalf("allowed user: %d", resp.StatusCode)
	}
	s.setAllowed("other@example.com")
	if resp, _ := c.send("tools/list", map[string]any{}, false); resp.StatusCode != 403 {
		t.Fatalf("removed user: %d", resp.StatusCode)
	}
}

func TestPublicEndpoints(t *testing.T) {
	s := newStack(t)
	if resp := s.get("/healthz"); resp.StatusCode != 200 || body(resp) != `{"status":"ok"}` {
		t.Fatal("healthz")
	}
	if resp := s.get("/.well-known/oauth-authorization-server"); resp.StatusCode != 200 || decode(t, resp)["issuer"] != s.ts.URL {
		t.Fatal("AS metadata")
	}
	if resp := s.get("/nope"); resp.StatusCode != 404 {
		t.Fatalf("unknown path: %d", resp.StatusCode)
	}
	if resp := s.get("/oauth/ah/doesnotexist/start"); resp.StatusCode != 404 {
		t.Fatalf("unknown sid: %d", resp.StatusCode)
	}
	// Without a login cookie the AH login page is not relayed.
	if resp := s.get("/login?client_id=appie-ios"); resp.StatusCode != 404 {
		t.Fatalf("login proxied without cookie: %d", resp.StatusCode)
	}
	if resp := s.get("/healthz"); resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
}

func TestOriginCheckOnMCP(t *testing.T) {
	s := newStack(t)
	for origin, want := range map[string]int{"https://evil.example": 403, "https://claude.ai": 401, "": 401, s.ts.URL: 401} {
		req, _ := http.NewRequest("POST", s.ts.URL+"/mcp", strings.NewReader("{}"))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, _ := s.client.Do(req)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("origin %q: %d, want %d", origin, resp.StatusCode, want)
		}
	}
	s2 := newStack(t, func(c *config.Config) { c.AllowAllOrigins = true })
	req, _ := http.NewRequest("POST", s2.ts.URL+"/mcp", nil)
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := s2.client.Do(req); resp.StatusCode != 401 {
		t.Errorf("'*' origins: %d", resp.StatusCode)
	}
}

func TestOAuthRateLimit(t *testing.T) {
	s := newStack(t)
	var last int
	for range 31 {
		resp, _ := s.client.Post(s.ts.URL+"/oauth/register", "application/json", strings.NewReader("{"))
		resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("31st request: %d", last)
	}
	// Metadata and /mcp are not throttled by the OAuth bucket.
	if resp := s.get("/.well-known/oauth-protected-resource"); resp.StatusCode != 200 {
		t.Fatalf("metadata throttled: %d", resp.StatusCode)
	}
}

func TestClientIPAndRateLimiter(t *testing.T) {
	trusted, _ := config.Load(func(k string) string {
		return map[string]string{"AH_MCP_BASE_URL": "https://x.example", "AH_MCP_SECRET_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
			"AH_MCP_TRUSTED_PROXIES": "10.0.0.0/8"}[k]
	}, false)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.1.1.1:5555"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9, 10.2.2.2")
	if got := server.ClientIP(req, trusted.TrustedProxies); got != netip.MustParseAddr("203.0.113.9") {
		t.Fatalf("trusted proxy: %v", got)
	}
	req.RemoteAddr = "198.51.100.7:5555"
	if got := server.ClientIP(req, trusted.TrustedProxies); got != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("untrusted peer must ignore XFF: %v", got)
	}
	if got := server.ClientIP(req, nil); got != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("no trusted proxies: %v", got)
	}

	now := time.Unix(0, 0)
	l := server.NewRateLimiter(30, nil)
	l.SetClock(func() time.Time { return now })
	a, b := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	for i := range 30 {
		if !l.Allow(a) {
			t.Fatalf("request %d denied", i)
		}
	}
	if l.Allow(a) {
		t.Fatal("31st request allowed")
	}
	if !l.Allow(b) {
		t.Fatal("other IP throttled")
	}
	now = now.Add(2 * time.Second) // 30/min = 1 token per 2 s
	if !l.Allow(a) || l.Allow(a) {
		t.Fatal("token bucket refill wrong")
	}
}
