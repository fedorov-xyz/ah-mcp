package oauth

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/util"

	"github.com/fedorov-xyz/ah-mcp/internal/ahlogin"
	"github.com/fedorov-xyz/ah-mcp/internal/logx"
)

// loginPrefix is where per-login proxy routes live: /oauth/ah/<sid>/...
const loginPrefix = "/oauth/ah/"

// Paths below a login prefix.
const (
	startPath    = "/start"
	callbackPath = "/callback"
	cancelPath   = "/_ahmcp_cancel"
)

// pendingAuth is an in-memory authorization in progress, keyed by its
// unguessable sid. It ends (and its proxy route disappears) on success,
// cancel or expiry.
type pendingAuth struct {
	sid         string
	client      *Client
	redirectURI string
	state       string
	challenge   string
	resource    string
	proxy       http.Handler
}

// authCode is our single-use authorization code.
type authCode struct {
	clientID    string
	redirectURI string
	challenge   string
	resource    string
	member      ahlogin.Member
	ah          ahlogin.Tokens
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")

	// Until client and redirect_uri are verified, errors are shown to the
	// user and never redirected (RFC 6749 §4.1.2.1).
	if clientID == "" || redirectURI == "" {
		s.renderError(w, http.StatusBadRequest, "Invalid request", "The application did not send a client_id and redirect_uri.")
		return
	}
	client, err := s.resolveClient(r.Context(), clientID)
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "Unknown application", "This application is not registered with this server. Remove the connector and add it again.")
		return
	}
	if !MatchRedirectURI(client.RedirectURIs, redirectURI) {
		s.renderError(w, http.StatusBadRequest, "Redirect URI mismatch", "The redirect_uri does not match the one the application registered. Remove the connector and add it again.")
		return
	}

	state := q.Get("state")
	fail := func(code, desc string) {
		s.redirectError(w, r, redirectURI, state, code, desc)
	}
	switch {
	case q.Get("response_type") != "code":
		fail("unsupported_response_type", "response_type must be code")
		return
	case state == "":
		fail("invalid_request", "state is required")
		return
	case q.Get("code_challenge") == "":
		fail("invalid_request", "code_challenge is required (PKCE)")
		return
	case q.Get("code_challenge_method") != "S256":
		fail("invalid_request", "code_challenge_method must be S256")
		return
	case !s.resourceOK(q.Get("resource")):
		fail("invalid_target", "resource must be "+s.Resource())
		return
	}

	sid := randomToken(32)
	prefix := loginPrefix + sid
	p := &pendingAuth{
		sid:         sid,
		client:      client,
		redirectURI: redirectURI,
		state:       state,
		challenge:   q.Get("code_challenge"),
		resource:    util.CanonicalURL(q.Get("resource")),
		proxy:       ahlogin.NewProxy(s.loginBase, s.opts.BaseURL, s.opts.BaseURL+prefix+callbackPath, s.log),
	}
	if !s.pending.put(sid, p, PendingTTL) {
		s.renderError(w, http.StatusServiceUnavailable, "Busy", "Too many sign-ins in progress. Try again in a few minutes.")
		return
	}

	redirectHost := util.URLOrigin(redirectURI)
	name := client.Name
	if name == "" {
		name = "An MCP client"
	}
	setPageHeaders(w)
	w.WriteHeader(http.StatusOK)
	_ = consentTmpl.Execute(w, consentData{
		ClientName:   name,
		ClientKind:   client.Kind,
		RedirectHost: redirectHost,
		LoginURL:     prefix + startPath,
		CancelURL:    prefix + cancelPath,
		Minutes:      int(PendingTTL / time.Minute),
	})
}

// handleLoginProxy serves /oauth/ah/<sid>/{start,callback,_ahmcp_cancel}.
// Unknown or expired sids get 404.
//
// start ties the browser to the pending sign-in with a one-time cookie and
// sends it to the AH login page, which is then relayed from the root of our
// origin by ServeLoginFallback.
func (s *Server) handleLoginProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, loginPrefix)
	sid, sub, _ := strings.Cut(rest, "/")
	sub = "/" + sub
	if _, ok := s.pending.get(sid); !ok || sid == "" {
		http.NotFound(w, r)
		return
	}
	switch sub {
	case startPath:
		s.setLoginCookie(w, sid, int(PendingTTL/time.Second))
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, ahlogin.LoginPath(), http.StatusFound)
	case callbackPath:
		s.setLoginCookie(w, "", -1)
		s.handleCallback(w, r, sid)
	case cancelPath:
		s.setLoginCookie(w, "", -1)
		if p, ok := s.pending.take(sid); ok {
			s.redirectError(w, r, p.redirectURI, p.state, "access_denied", "the user cancelled the sign-in")
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) setLoginCookie(w http.ResponseWriter, sid string, maxAge int) {
	// Secure is off only for the http://127.0.0.1 / localhost development
	// base URL, where browsers would otherwise drop the cookie.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // conditional Secure, see above
		Name:     ahlogin.LoginCookie,
		Value:    sid,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.opts.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

// ServeLoginFallback relays a request that matches none of our own routes to
// the AH login host when it carries the login cookie of a live sign-in. It
// reports whether it handled the request; without a valid cookie nothing is
// proxied.
func (s *Server) ServeLoginFallback(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(ahlogin.LoginCookie)
	if err != nil || c.Value == "" {
		return false
	}
	p, ok := s.pending.get(c.Value)
	if !ok {
		return false
	}
	p.proxy.ServeHTTP(w, r)
	return true
}

// handleCallback receives the rewritten appie://login-exit?code=... redirect.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request, sid string) {
	// One-time: the pending authorization is consumed whatever happens.
	p, ok := s.pending.take(sid)
	if !ok {
		http.NotFound(w, r)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.redirectError(w, r, p.redirectURI, p.state, "access_denied", "Albert Heijn sign-in was not completed")
		return
	}
	ctx := r.Context()
	tokens, err := s.opts.AH.Exchange(ctx, code)
	if err != nil {
		s.log.Warn("AH code exchange failed")
		s.redirectError(w, r, p.redirectURI, p.state, "server_error", "Albert Heijn sign-in failed")
		return
	}
	member, err := s.opts.AH.Member(ctx, tokens.AccessToken)
	if err != nil {
		s.log.Warn("AH member lookup failed")
		s.redirectError(w, r, p.redirectURI, p.state, "server_error", "could not read the Albert Heijn profile")
		return
	}
	if !s.opts.EmailAllowed(member.Email) {
		s.log.Warn("sign-in denied by allowlist", "member", logx.MemberHash(member.ID))
		s.renderDenied(w, s.errorRedirectURL(p.redirectURI, p.state, "access_denied", "this Albert Heijn account is not allowed on this server"))
		return
	}

	ourCode := randomToken(32)
	if !s.codes.put(ourCode, &authCode{
		clientID:    p.client.ID,
		redirectURI: p.redirectURI,
		challenge:   p.challenge,
		resource:    p.resource,
		member:      member,
		ah:          tokens,
	}, CodeTTL) {
		s.redirectError(w, r, p.redirectURI, p.state, "temporarily_unavailable", "server busy")
		return
	}
	s.log.Info("sign-in completed", "member", logx.MemberHash(member.ID), "client", clientHash(p.client.ID)[:12])
	http.Redirect(w, r, util.AppendQuery(p.redirectURI, url.Values{"code": {ourCode}, "state": {p.state}, "iss": {s.Issuer()}}), http.StatusFound)
}

func (s *Server) errorRedirectURL(redirectURI, state, code, desc string) string {
	v := url.Values{"error": {code}, "error_description": {desc}, "iss": {s.Issuer()}}
	if state != "" {
		v.Set("state", state)
	}
	return util.AppendQuery(redirectURI, v)
}

func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	// redirectURI was matched against the client's registered redirect URIs.
	http.Redirect(w, r, s.errorRedirectURL(redirectURI, state, code, desc), http.StatusFound) //nolint:gosec // validated redirect_uri
}

// --- pages ---

const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func setPageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

type consentData struct {
	ClientName   string
	ClientKind   string
	RedirectHost string
	LoginURL     string
	CancelURL    string
	Minutes      int
}

const pageStyle = `<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#1a1a1a;line-height:1.5}
.card{border:1px solid #ddd;border-radius:12px;padding:1.5rem}
h1{font-size:1.3rem;margin-top:0}
.btn{display:inline-block;background:#00a0e2;color:#fff;text-decoration:none;padding:.7rem 1.2rem;border-radius:8px;font-weight:600}
.muted{color:#666;font-size:.9rem}
code{background:#f3f3f3;padding:.1rem .3rem;border-radius:4px;word-break:break-all}
@media (prefers-color-scheme:dark){body{background:#111;color:#eee}.card{border-color:#333}code{background:#222}.muted{color:#aaa}}
</style>`

var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Connect to Albert Heijn</title>` + pageStyle + `</head><body><div class="card">
<h1>Connect {{.ClientName}} to your Albert Heijn account</h1>
<p><strong>{{.ClientName}}</strong> wants to search products, manage your shopping list and active order, and read your orders, receipts and bonus offers.</p>
<p class="muted">After sign-in you will be sent back to <code>{{.RedirectHost}}</code>{{if eq .ClientKind "dcr"}} (the application chose its own name when it registered){{end}}.</p>
<p><a class="btn" href="{{.LoginURL}}">Sign in with Albert Heijn</a></p>
<p class="muted">Your Albert Heijn login page is relayed through this server; only continue if you trust its operator. This link expires in {{.Minutes}} minutes. <a href="{{.CancelURL}}">Cancel</a></p>
</div></body></html>`))

var errorTmpl = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
{{if .Redirect}}<meta http-equiv="refresh" content="5;url={{.Redirect}}">{{end}}
<title>{{.Title}}</title>` + pageStyle + `</head><body><div class="card">
<h1>{{.Title}}</h1><p>{{.Message}}</p>
{{if .Redirect}}<p><a href="{{.Redirect}}">Return to the application</a></p>{{end}}
</div></body></html>`))

type errorData struct {
	Title    string
	Message  string
	Redirect template.URL
}

func (s *Server) renderError(w http.ResponseWriter, status int, title, msg string) {
	setPageHeaders(w)
	w.WriteHeader(status)
	_ = errorTmpl.Execute(w, errorData{Title: title, Message: msg})
}

func (s *Server) renderDenied(w http.ResponseWriter, redirect string) {
	setPageHeaders(w)
	w.WriteHeader(http.StatusForbidden)
	// The redirect target is a registered redirect_uri (possibly a
	// private-use scheme), so it is marked safe explicitly; it was
	// validated at registration and matched above.
	_ = errorTmpl.Execute(w, errorData{
		Title:    "Account not allowed",
		Message:  "This Albert Heijn account is not on this server's allowlist. Ask the operator to add your email address.",
		Redirect: template.URL(redirect), //nolint:gosec // validated redirect_uri
	})
}
