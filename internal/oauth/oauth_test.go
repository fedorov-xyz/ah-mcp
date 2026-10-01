package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/ahlogin"
	"github.com/fedorov-xyz/ah-mcp/internal/oauth/seal"
	"github.com/fedorov-xyz/ah-mcp/internal/testutil/fakeah"
)

const testBase = "https://ah-mcp.example.com"

type testEnv struct {
	t       *testing.T
	s       *Server
	fake    *fakeah.Fake
	mux     *http.ServeMux
	mu      sync.Mutex
	now     time.Time
	allowed map[string]bool // nil = everyone
	cookie  string          // the browser's login cookie, if any
}

func newEnv(t *testing.T, opts ...func(*Options)) *testEnv {
	t.Helper()
	e := &testEnv{t: t, fake: fakeah.New(t), now: time.Now()}
	sealer, err := seal.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	o := Options{
		BaseURL:     testBase,
		SessionTTL:  720 * time.Hour,
		Sealer:      sealer,
		AH:          ahlogin.New(e.fake.URL(), e.fake.URL(), nil),
		AHLoginBase: e.fake.URL(),
		Logger:      slog.New(slog.DiscardHandler),
		Now:         e.clock,
		EmailAllowed: func(email string) bool {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.allowed == nil || e.allowed[strings.ToLower(email)]
		},
	}
	for _, f := range opts {
		f(&o)
	}
	if e.s, err = New(o); err != nil {
		t.Fatal(err)
	}
	e.mux = http.NewServeMux()
	e.s.Routes(e.mux)
	// As in internal/server: unknown paths go to the login proxy if the
	// browser carries a live login cookie.
	e.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !e.s.ServeLoginFallback(w, r) {
			http.NotFound(w, r)
		}
	})
	return e
}

func (e *testEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *testEnv) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func (e *testEnv) allow(emails ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.allowed = map[string]bool{}
	for _, m := range emails {
		e.allowed[strings.ToLower(m)] = true
	}
}

func (e *testEnv) do(method, target string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if method == http.MethodPost && strings.HasPrefix(target, "/oauth/token") {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if e.cookie != "" {
		req.AddCookie(&http.Cookie{Name: ahlogin.LoginCookie, Value: e.cookie})
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == ahlogin.LoginCookie {
			if c.MaxAge < 0 {
				e.cookie = ""
			} else {
				e.cookie = c.Value
			}
		}
	}
	return rec
}

func (e *testEnv) register(redirects ...string) string {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"redirect_uris": redirects, "client_name": "Test Client"})
	rec := e.do("POST", "/oauth/register", string(b))
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out["client_id"].(string)
}

const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk-and-some-more"

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizeURL(clientID, redirect string, extra url.Values) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"state":                 {"st-123"},
		"code_challenge":        {challenge(verifier)},
		"code_challenge_method": {"S256"},
		"resource":              {testBase + "/mcp"},
	}
	for k, v := range extra {
		if len(v) == 0 || v[0] == "" {
			q.Del(k)
		} else {
			q[k] = v
		}
	}
	return "/oauth/authorize?" + q.Encode()
}

var sidRe = regexp.MustCompile(`/oauth/ah/([A-Za-z0-9_-]+)/start"`)

// startLogin runs /oauth/authorize and returns the login sid.
func (e *testEnv) startLogin(clientID, redirect string) string {
	e.t.Helper()
	rec := e.do("GET", authorizeURL(clientID, redirect, nil), "")
	if rec.Code != http.StatusOK {
		e.t.Fatalf("authorize: %d %s", rec.Code, rec.Body)
	}
	m := sidRe.FindStringSubmatch(rec.Body.String())
	if m == nil {
		e.t.Fatalf("no login link in consent page: %s", rec.Body)
	}
	return m[1]
}

// finishLogin follows the proxied AH login to the client redirect.
func (e *testEnv) finishLogin(sid string) *url.URL {
	e.t.Helper()
	cb := e.proxiedLogin(sid)
	if cb.Host != "ah-mcp.example.com" || cb.Path != "/oauth/ah/"+sid+"/callback" {
		e.t.Fatalf("app redirect not rewritten: %s", cb)
	}
	rec := e.do("GET", cb.RequestURI(), "")
	if rec.Code != http.StatusFound {
		e.t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	if e.cookie != "" {
		e.t.Fatal("login cookie not cleared by the callback")
	}
	u, _ := url.Parse(rec.Header().Get("Location"))
	return u
}

// proxiedLogin clicks "Sign in" (start sets the login cookie and redirects
// to the AH login page at our root), and returns the rewritten app redirect.
func (e *testEnv) proxiedLogin(sid string) *url.URL {
	e.t.Helper()
	rec := e.do("GET", "/oauth/ah/"+sid+"/start", "")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != ahlogin.LoginPath() || e.cookie != sid {
		e.t.Fatalf("start: %d %q cookie=%q", rec.Code, rec.Header().Get("Location"), e.cookie)
	}
	ck := rec.Header().Get("Set-Cookie")
	if !strings.Contains(ck, "HttpOnly") || !strings.Contains(ck, "SameSite=Lax") || !strings.Contains(ck, "Secure") || !strings.Contains(ck, "Path=/") {
		e.t.Fatalf("login cookie attributes: %q", ck)
	}
	rec = e.do("GET", ahlogin.LoginPath(), "")
	if rec.Code != http.StatusFound {
		e.t.Fatalf("proxied login: %d %s", rec.Code, rec.Body)
	}
	cb, _ := url.Parse(rec.Header().Get("Location"))
	return cb
}

func (e *testEnv) login(clientID, redirect string) string {
	e.t.Helper()
	u := e.finishLogin(e.startLogin(clientID, redirect))
	if u.Query().Get("state") != "st-123" || u.Query().Get("iss") != testBase || u.Query().Get("code") == "" {
		e.t.Fatalf("bad client redirect: %s", u)
	}
	return u.Query().Get("code")
}

func (e *testEnv) token(form url.Values) (int, map[string]any) {
	rec := e.do("POST", "/oauth/token", form.Encode())
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Header().Get("Cache-Control") != "no-store" {
		e.t.Errorf("token response without Cache-Control: no-store")
	}
	return rec.Code, out
}

func codeForm(code, clientID, redirect string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {testBase + "/mcp"}}
}

const cb = "http://127.0.0.1:33418/callback"

func TestMetadataDocuments(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		rec := e.do("GET", p, "")
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || rec.Code != 200 {
			t.Fatalf("%s: %d %v", p, rec.Code, err)
		}
		if m["resource"] != testBase+"/mcp" || fmt.Sprint(m["authorization_servers"]) != "["+testBase+"]" {
			t.Errorf("%s = %v", p, m)
		}
	}
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"} {
		rec := e.do("GET", p, "")
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		checks := map[string]string{
			"issuer": testBase, "authorization_endpoint": testBase + "/oauth/authorize", "token_endpoint": testBase + "/oauth/token",
			"registration_endpoint": testBase + "/oauth/register", "code_challenge_methods_supported": "[S256]",
			"grant_types_supported": "[authorization_code refresh_token]", "response_types_supported": "[code]",
			"token_endpoint_auth_methods_supported": "[none]", "client_id_metadata_document_supported": "true",
			"authorization_response_iss_parameter_supported": "true",
		}
		for k, want := range checks {
			if got := fmt.Sprint(m[k]); got != want {
				t.Errorf("%s %s = %s, want %s", p, k, got, want)
			}
		}
	}
}

func TestChallengeHeader(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest("POST", "/mcp", nil)
	_, aerr := e.s.Authenticate(req)
	rec := httptest.NewRecorder()
	e.s.WriteChallenge(rec, aerr)
	want := `Bearer resource_metadata="` + testBase + `/.well-known/oauth-protected-resource/mcp"`
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != want {
		t.Fatalf("no-token challenge: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	req.Header.Set("Authorization", "Bearer ahm1.garbage")
	_, aerr = e.s.Authenticate(req)
	rec = httptest.NewRecorder()
	e.s.WriteChallenge(rec, aerr)
	if h := rec.Header().Get("WWW-Authenticate"); rec.Code != 401 || !strings.Contains(h, `error="invalid_token"`) || !strings.Contains(h, "resource_metadata=") {
		t.Fatalf("invalid-token challenge: %d %q", rec.Code, h)
	}
}

func TestRegisterValidation(t *testing.T) {
	e := newEnv(t)
	ok := []string{"https://claude.ai/api/mcp/auth_callback", "http://localhost:6274/oauth/callback", "http://127.0.0.1/cb",
		"http://[::1]:8080/cb", "cursor://anysphere.cursor-retrieval/oauth/callback", "com.example.app:/oauth"}
	bad := []string{"http://example.com/cb", "https://claude.ai/cb#frag", "https://*.claude.ai/cb", "javascript:alert(1)",
		"data:text/html,x", "https://user:pw@claude.ai/cb", "/relative", "", "file:///etc/passwd", "https:///nohost"}
	for _, u := range ok {
		if err := ValidateRedirectURI(u); err != nil {
			t.Errorf("%q rejected: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := ValidateRedirectURI(u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	for name, body := range map[string]string{
		"bad json":       `{`,
		"no uris":        `{"redirect_uris":[]}`,
		"bad uri":        `{"redirect_uris":["http://evil.example/cb"]}`,
		"secret client":  `{"redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`,
		"too many":       `{"redirect_uris":["https://a.example/1","https://a.example/2","https://a.example/3","https://a.example/4","https://a.example/5","https://a.example/6"]}`,
		"bad grant":      `{"redirect_uris":["https://a.example/cb"],"grant_types":["client_credentials"]}`,
		"bad resp. type": `{"redirect_uris":["https://a.example/cb"],"response_types":["token"]}`,
	} {
		if rec := e.do("POST", "/oauth/register", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	// No client_secret is ever issued.
	rec := e.do("POST", "/oauth/register", `{"redirect_uris":["https://a.example/cb"],"client_name":"<b>x</b>"}`)
	if rec.Code != 201 || strings.Contains(rec.Body.String(), "client_secret\"") {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
}

func TestMatchRedirectURI(t *testing.T) {
	reg := []string{"http://127.0.0.1:1234/callback", "https://claude.ai/api/mcp/auth_callback", "cursor://x/cb"}
	for req, want := range map[string]bool{
		"http://127.0.0.1:1234/callback":              true,
		"http://127.0.0.1:5555/callback":              true, // loopback port may vary
		"http://127.0.0.1/callback":                   true,
		"http://localhost:1234/callback":              false, // host must match
		"http://127.0.0.1:1234/other":                 false,
		"http://127.0.0.1:1234/callback?x=1":          false,
		"https://claude.ai/api/mcp/auth_callback":     true,
		"https://claude.ai:444/api/mcp/auth_callback": false, // exact for non-loopback
		"https://claude.ai/api/mcp/auth_callback/":    false,
		"cursor://x/cb":                               true,
		"cursor://x/cb2":                              false,
	} {
		if got := MatchRedirectURI(reg, req); got != want {
			t.Errorf("MatchRedirectURI(%q) = %v, want %v", req, got, want)
		}
	}
}

func TestAuthorizeValidation(t *testing.T) {
	e := newEnv(t)
	id := e.register(cb)

	for name, target := range map[string]string{
		"unknown client":    authorizeURL("ahm1.forged", cb, nil),
		"https cimd bad":    authorizeURL("https://127.0.0.1/client.json", cb, nil),
		"redirect mismatch": authorizeURL(id, "http://localhost:33418/callback", nil),
		"no client":         "/oauth/authorize?redirect_uri=" + url.QueryEscape(cb),
	} {
		rec := e.do("GET", target, "")
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("%s: want 400 page without redirect, got %d %q", name, rec.Code, rec.Header().Get("Location"))
		}
	}
	for name, c := range map[string]struct {
		extra url.Values
		code  string
	}{
		"plain PKCE":     {url.Values{"code_challenge_method": {"plain"}}, "invalid_request"},
		"no PKCE method": {url.Values{"code_challenge_method": {""}}, "invalid_request"},
		"no challenge":   {url.Values{"code_challenge": {""}}, "invalid_request"},
		"no state":       {url.Values{"state": {""}}, "invalid_request"},
		"token flow":     {url.Values{"response_type": {"token"}}, "unsupported_response_type"},
		"wrong resource": {url.Values{"resource": {"https://other.example/mcp"}}, "invalid_target"},
	} {
		rec := e.do("GET", authorizeURL(id, cb, c.extra), "")
		u, _ := url.Parse(rec.Header().Get("Location"))
		if rec.Code != http.StatusFound || u.Query().Get("error") != c.code || u.Query().Get("iss") != testBase {
			t.Errorf("%s: %d %s", name, rec.Code, u)
		}
	}
	// Case-insensitive scheme/host and trailing slash are accepted.
	if rec := e.do("GET", authorizeURL(id, cb, url.Values{"resource": {"HTTPS://AH-MCP.example.com/mcp/"}}), ""); rec.Code != 200 {
		t.Errorf("canonical resource variant rejected: %d", rec.Code)
	}
	// resource may be omitted.
	if rec := e.do("GET", authorizeURL(id, cb, url.Values{"resource": {""}}), ""); rec.Code != 200 {
		t.Errorf("missing resource rejected: %d", rec.Code)
	}
}

func TestConsentPageEscapingAndHeaders(t *testing.T) {
	e := newEnv(t)
	b, _ := json.Marshal(map[string]any{"redirect_uris": []string{cb}, "client_name": `<script>alert("x")</script>`})
	var reg map[string]any
	_ = json.Unmarshal(e.do("POST", "/oauth/register", string(b)).Body.Bytes(), &reg)
	rec := e.do("GET", authorizeURL(reg["client_id"].(string), cb, nil), "")
	body := rec.Body.String()
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("client name not escaped: %s", body)
	}
	if !strings.Contains(body, "http://127.0.0.1:33418") || !strings.Contains(body, "Sign in with Albert Heijn") {
		t.Fatalf("consent page lacks redirect host or button: %s", body)
	}
	h := rec.Header()
	if h.Get("Content-Security-Policy") == "" || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") ||
		!strings.Contains(h.Get("Content-Security-Policy"), "form-action 'self'") || h.Get("X-Frame-Options") != "DENY" ||
		h.Get("Cache-Control") != "no-store" {
		t.Fatalf("security headers: %v", h)
	}
}

func TestCodeFlowAndCodeRules(t *testing.T) {
	e := newEnv(t)
	id := e.register(cb)
	other := e.register(cb)

	code := e.login(id, cb)
	status, tok := e.token(codeForm(code, id, cb))
	if status != 200 || tok["token_type"] != "Bearer" || tok["access_token"] == nil || tok["refresh_token"] == nil {
		t.Fatalf("token: %d %v", status, tok)
	}
	if exp := tok["expires_in"].(float64); exp <= 0 || exp > 3600 {
		t.Fatalf("expires_in = %v", exp)
	}
	// Single use.
	if status, out := e.token(codeForm(code, id, cb)); status != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("replay: %d %v", status, out)
	}

	bad := map[string]func(url.Values){
		"wrong verifier": func(f url.Values) { f.Set("code_verifier", strings.Repeat("x", 50)) },
		"short verifier": func(f url.Values) { f.Set("code_verifier", "abc") },
		"other client":   func(f url.Values) { f.Set("client_id", other) },
		"other redirect": func(f url.Values) { f.Set("redirect_uri", "http://127.0.0.1:9/callback") },
	}
	for name, mutate := range bad {
		c := e.login(id, cb)
		f := codeForm(c, id, cb)
		mutate(f)
		if status, out := e.token(f); status != 400 || out["error"] != "invalid_grant" {
			t.Errorf("%s: %d %v", name, status, out)
		}
		// The code is burnt even by a failed attempt.
		if status, _ := e.token(codeForm(c, id, cb)); status != 400 {
			t.Errorf("%s: code reusable after failed attempt", name)
		}
	}

	c := e.login(id, cb)
	f := codeForm(c, id, cb)
	f.Set("resource", "https://other.example/mcp")
	if status, out := e.token(f); status != 400 || out["error"] != "invalid_target" {
		t.Errorf("resource mismatch: %d %v", status, out)
	}

	c = e.login(id, cb)
	e.advance(CodeTTL + time.Second)
	if status, out := e.token(codeForm(c, id, cb)); status != 400 || out["error"] != "invalid_grant" {
		t.Errorf("expired code: %d %v", status, out)
	}

	if status, out := e.token(url.Values{"grant_type": {"password"}}); status != 400 || out["error"] != "unsupported_grant_type" {
		t.Errorf("grant type: %d %v", status, out)
	}
	if status, out := e.token(url.Values{"grant_type": {"authorization_code"}, "client_secret": {"x"}}); status != 401 || out["error"] != "invalid_client" {
		t.Errorf("client secret: %d %v", status, out)
	}
}

func TestPendingSessions(t *testing.T) {
	e := newEnv(t)
	id := e.register(cb)

	if rec := e.do("GET", "/oauth/ah/unknownsid/start", ""); rec.Code != 404 || e.cookie != "" {
		t.Fatalf("unknown sid started: %d", rec.Code)
	}
	if rec := e.do("GET", ahlogin.LoginPath(), ""); rec.Code != 404 {
		t.Fatalf("proxied without a login cookie: %d", rec.Code)
	}
	if rec := e.do("GET", "/oauth/ah/unknownsid/callback?code=x", ""); rec.Code != 404 {
		t.Fatalf("unknown sid callback: %d", rec.Code)
	}
	before := len(e.fake.Requests())

	// One-time: after the callback, the sid and its proxy route are gone.
	sid := e.startLogin(id, cb)
	e.finishLogin(sid)
	if rec := e.do("GET", "/oauth/ah/"+sid+"/callback?code="+fakeah.LoginCode, ""); rec.Code != 404 {
		t.Fatalf("sid reusable: %d", rec.Code)
	}
	e.cookie = sid // a browser that kept the old cookie
	if rec := e.do("GET", ahlogin.LoginPath(), ""); rec.Code != 404 {
		t.Fatalf("proxy alive after callback: %d", rec.Code)
	}
	e.cookie = ""

	// TTL.
	sid = e.startLogin(id, cb)
	e.do("GET", "/oauth/ah/"+sid+"/start", "")
	e.advance(PendingTTL + time.Second)
	if rec := e.do("GET", ahlogin.LoginPath(), ""); rec.Code != 404 {
		t.Fatalf("expired sid proxied: %d", rec.Code)
	}
	e.cookie = ""
	if got := len(e.fake.Requests()) - before; got != 3 { // login + token + member of the one successful flow
		t.Fatalf("unexpected upstream requests: %d (%v)", got, e.fake.Requests())
	}

	// Cancel.
	sid = e.startLogin(id, cb)
	rec := e.do("GET", "/oauth/ah/"+sid+"/_ahmcp_cancel", "")
	u, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != 302 || u.Query().Get("error") != "access_denied" || u.Query().Get("state") != "st-123" {
		t.Fatalf("cancel: %d %s", rec.Code, u)
	}
	if e.s.pending.len() != 0 {
		t.Fatalf("pending sessions left: %d", e.s.pending.len())
	}
}

func TestLoginCookieSelectsProxy(t *testing.T) {
	e := newEnv(t)
	id := e.register(cb)
	sid := e.startLogin(id, cb)
	req := httptest.NewRequest("GET", ahlogin.LoginPath(), nil)
	if e.s.ServeLoginFallback(httptest.NewRecorder(), req) {
		t.Fatal("served without cookie")
	}
	req.AddCookie(&http.Cookie{Name: ahlogin.LoginCookie, Value: "forged"})
	if e.s.ServeLoginFallback(httptest.NewRecorder(), req) {
		t.Fatal("served with an unknown sid")
	}
	req = httptest.NewRequest("GET", ahlogin.LoginPath(), nil)
	req.AddCookie(&http.Cookie{Name: ahlogin.LoginCookie, Value: sid})
	rec := httptest.NewRecorder()
	if !e.s.ServeLoginFallback(rec, req) || rec.Code != 302 {
		t.Fatalf("not served with a live sid: %d", rec.Code)
	}
}

func TestAllowlistAtSignIn(t *testing.T) {
	e := newEnv(t)
	e.allow("someone.else@example.com")
	id := e.register(cb)
	sid := e.startLogin(id, cb)
	loc := e.proxiedLogin(sid)
	rec := e.do("GET", loc.RequestURI(), "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("denied sign-in: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "error=access_denied") || !strings.Contains(body, `http-equiv="refresh"`) || strings.Contains(body, "code=") {
		t.Fatalf("denied page: %s", body)
	}
	e.allow("TEST.USER@example.com") // case-insensitive
	if code := e.login(id, cb); code == "" {
		t.Fatal("allowed user could not sign in")
	}
}

func issue(t *testing.T, e *testEnv) (access, refresh, clientID string) {
	t.Helper()
	clientID = e.register(cb)
	_, tok := e.token(codeForm(e.login(clientID, cb), clientID, cb))
	return tok["access_token"].(string), tok["refresh_token"].(string), clientID
}

func bearer(tok string) *http.Request {
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

func TestAuthenticate(t *testing.T) {
	e := newEnv(t)
	access, refresh, _ := issue(t, e)
	sess, aerr := e.s.Authenticate(bearer(access))
	if aerr != nil || sess.MemberID != fakeah.MemberID || sess.Email != e.fake.Email || sess.AHAccessToken == "" {
		t.Fatalf("valid token: %+v %v", sess, aerr)
	}
	if _, aerr := e.s.Authenticate(bearer(refresh)); aerr == nil || aerr.Status != 401 {
		t.Fatal("refresh token accepted as access token")
	}

	// Audience mismatch.
	now := e.clock()
	claims := TokenClaims{V: 1, Kind: kindAccess, Aud: "https://other.example/mcp", MemberID: "1", Email: "a@b.c", AHAccess: "x",
		AHExpiresAt: now.Add(time.Hour).Unix(), Iat: now.Unix(), Exp: now.Add(time.Hour).Unix()}
	tok, _ := e.s.opts.Sealer.Seal(seal.PurposeAccess, claims, now.Add(time.Hour))
	if _, aerr := e.s.Authenticate(bearer(tok)); aerr == nil || aerr.Code != "invalid_token" {
		t.Fatalf("aud mismatch: %v", aerr)
	}

	// AH part expired although our token is not: 401 so the client refreshes.
	claims.Aud, claims.AHExpiresAt = e.s.Resource(), now.Add(-time.Second).Unix()
	tok, _ = e.s.opts.Sealer.Seal(seal.PurposeAccess, claims, now.Add(time.Hour))
	if _, aerr := e.s.Authenticate(bearer(tok)); aerr == nil || aerr.Status != 401 || aerr.Code != "invalid_token" {
		t.Fatalf("expired AH token: %v", aerr)
	}
	if e.fake.RefreshCalls.Load() != 0 {
		t.Fatal("Authenticate refreshed AH")
	}

	// Our own expiry.
	e.advance(2 * time.Hour)
	if _, aerr := e.s.Authenticate(bearer(access)); aerr == nil || aerr.Status != 401 {
		t.Fatal("expired access token accepted")
	}
}

func TestAccessTTLFollowsAHExpiry(t *testing.T) {
	e := newEnv(t)
	e.fake.ExpiresIn = 600
	id := e.register(cb)
	_, tok := e.token(codeForm(e.login(id, cb), id, cb))
	if got := tok["expires_in"].(float64); got != 540 {
		t.Fatalf("expires_in = %v, want AH expiry - 60s = 540", got)
	}
	e.fake.ExpiresIn = 30
	_, out := e.token(codeForm(e.login(id, cb), id, cb))
	if out["error"] != "invalid_grant" {
		t.Fatalf("AH token expiring within the margin must not be wrapped: %v", out)
	}
}

func refreshForm(rt, clientID string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {clientID}}
}

func TestRefresh(t *testing.T) {
	e := newEnv(t)
	access, refresh, id := issue(t, e)
	oldSess, _ := e.s.Authenticate(bearer(access))

	status, out := e.token(refreshForm(refresh, id))
	if status != 200 || out["refresh_token"] == refresh || out["access_token"] == access {
		t.Fatalf("refresh: %d %v", status, out)
	}
	if e.fake.RefreshCalls.Load() != 1 {
		t.Fatalf("AH refresh calls = %d", e.fake.RefreshCalls.Load())
	}
	newSess, aerr := e.s.Authenticate(bearer(out["access_token"].(string)))
	if aerr != nil || newSess.AHAccessToken == oldSess.AHAccessToken || newSess.MemberID != oldSess.MemberID {
		t.Fatalf("new access token: %+v %v", newSess, aerr)
	}
	rotated := out["refresh_token"].(string)
	// Reusing the old refresh token after the replay window fails: AH
	// rotated its refresh token.
	e.advance(refreshReuse + time.Second)
	if status, out := e.token(refreshForm(refresh, id)); status != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("old refresh token: %d %v", status, out)
	}
	// The rotated one still works, and rotates again.
	if status, out := e.token(refreshForm(rotated, id)); status != 200 || out["refresh_token"] == rotated {
		t.Fatalf("rotated refresh token: %d %v", status, out)
	}
	if status, _ := e.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"ahm1.x"}}); status != 400 {
		t.Fatal("garbage refresh token accepted")
	}
}

func TestRefreshClientBindingAndAHFailure(t *testing.T) {
	e := newEnv(t)
	_, refresh, _ := issue(t, e)
	other := e.register(cb)
	if status, out := e.token(refreshForm(refresh, other)); status != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("refresh with other client: %d %v", status, out)
	}
	e.fake.RejectRefreshes()
	if status, out := e.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}); status != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("AH refresh failure: %d %v", status, out)
	}
}

func TestConcurrentRefreshCollapses(t *testing.T) {
	e := newEnv(t)
	_, refresh, id := issue(t, e)
	var wg sync.WaitGroup
	results := make([]string, 5)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, out := e.token(refreshForm(refresh, id))
			if status == 200 {
				results[i] = out["refresh_token"].(string)
			}
		}()
	}
	wg.Wait()
	for _, r := range results {
		if r == "" || r != results[0] {
			t.Fatalf("concurrent refreshes diverged: %v", results)
		}
	}
	if n := e.fake.RefreshCalls.Load(); n != 1 {
		t.Fatalf("AH refreshed %d times", n)
	}
}

func TestAllowlistRemovalRevokes(t *testing.T) {
	e := newEnv(t)
	e.allow("test.user@example.com")
	access, refresh, id := issue(t, e)
	if _, aerr := e.s.Authenticate(bearer(access)); aerr != nil {
		t.Fatal(aerr)
	}
	e.allow("someone.else@example.com")
	if _, aerr := e.s.Authenticate(bearer(access)); aerr == nil || aerr.Status != http.StatusForbidden {
		t.Fatalf("removed email still accepted: %v", aerr)
	}
	calls := e.fake.RefreshCalls.Load()
	if status, out := e.token(refreshForm(refresh, id)); status != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("removed email refreshed: %d %v", status, out)
	}
	if e.fake.RefreshCalls.Load() != calls {
		t.Fatal("AH refresh called for a revoked account")
	}
}
