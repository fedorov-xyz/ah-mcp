package ahlogin_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fedorov-xyz/ah-mcp/internal/ahlogin"
	"github.com/fedorov-xyz/ah-mcp/internal/testutil/fakeah"
)

func TestExchangeRefreshMember(t *testing.T) {
	fake := fakeah.New(t)
	c := ahlogin.New(fake.URL(), fake.URL(), nil)
	ctx := context.Background()

	if _, err := c.Exchange(ctx, "wrong"); !errors.Is(err, ahlogin.ErrUpstream) {
		t.Fatalf("bad code: %v", err)
	}
	tok, err := c.Exchange(ctx, fakeah.LoginCode)
	if err != nil || tok.AccessToken == "" || tok.RefreshToken == "" || tok.MemberID != fakeah.MemberID || tok.ExpiresAt.IsZero() {
		t.Fatalf("exchange = %+v, %v", tok, err)
	}
	m, err := c.Member(ctx, tok.AccessToken)
	if err != nil || m.ID != "4242" || m.Email != fake.Email {
		t.Fatalf("member = %+v, %v", m, err)
	}
	rotated, err := c.Refresh(ctx, tok.RefreshToken)
	if err != nil || rotated.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh = %+v, %v", rotated, err)
	}
	// AH rotates refresh tokens: the old one is dead now.
	if _, err := c.Refresh(ctx, tok.RefreshToken); !errors.Is(err, ahlogin.ErrUpstream) {
		t.Fatalf("old refresh token reused: %v", err)
	}
	// The error never carries the upstream body.
	if strings.Contains(ahlogin.ErrUpstream.Error(), "refresh token expired") {
		t.Fatal("upstream body leaked into error")
	}
}

func TestLoginPath(t *testing.T) {
	if got := ahlogin.LoginPath(); got != "/login?client_id=appie-ios&response_type=code&redirect_uri=appie://login-exit" {
		t.Fatal(got)
	}
}

const testCallback = "https://ah-mcp.example.com/oauth/ah/SID/callback"

func proxyFor(t *testing.T, target string) http.Handler {
	t.Helper()
	u, _ := url.Parse(target)
	return ahlogin.NewProxy(u, "https://ah-mcp.example.com", testCallback, slog.New(slog.DiscardHandler))
}

func TestProxyRewritesAppRedirectAndCookies(t *testing.T) {
	fake := fakeah.New(t)
	rec := httptest.NewRecorder()
	proxyFor(t, fake.URL()).ServeHTTP(rec, httptest.NewRequest("GET", ahlogin.LoginPath(), nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != testCallback+"?code="+fakeah.LoginCode {
		t.Fatalf("Location = %q", loc)
	}
	ck := rec.Header().Get("Set-Cookie")
	if strings.Contains(strings.ToLower(ck), "domain=") || strings.Contains(strings.ToLower(ck), "samesite") || !strings.Contains(ck, "Secure") {
		t.Fatalf("cookie not sanitised for https origin: %q", ck)
	}
	if rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("AH CSP not stripped")
	}
}

func TestProxyRewritesBody(t *testing.T) {
	fake := fakeah.New(t)
	rec := httptest.NewRecorder()
	proxyFor(t, fake.URL()).ServeHTTP(rec, httptest.NewRequest("GET", "/login/js", nil))
	body, _ := io.ReadAll(rec.Body)
	s := string(body)
	for _, want := range []string{
		`location.href="` + testCallback + `?code=`,
		`action="https://ah-mcp.example.com/login/submit"`,
		// Root-relative paths stay as they are: the proxy is served from the
		// root of our origin, where the AH app expects its assets.
		`href="/assets/x.js"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("body missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "appie://") || strings.Contains(s, fake.URL()) {
		t.Errorf("body still references AH directly:\n%s", s)
	}
}

func TestProxyDropsClientCredentials(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Location", up2(r)+"/next")
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer ahm1.secret")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("Origin", "https://ah-mcp.example.com")
	req.AddCookie(&http.Cookie{Name: ahlogin.LoginCookie, Value: "SID"})
	req.AddCookie(&http.Cookie{Name: "SESSION", Value: "ah-session"})
	rec := httptest.NewRecorder()
	proxyFor(t, up.URL).ServeHTTP(rec, req)
	if got.Get("Authorization") != "" || got.Get("X-Forwarded-For") != "" {
		t.Fatalf("credentials forwarded: %v", got)
	}
	if ck := got.Get("Cookie"); strings.Contains(ck, ahlogin.LoginCookie) || !strings.Contains(ck, "SESSION=ah-session") {
		t.Fatalf("cookies upstream = %q", ck)
	}
	if got.Get("Origin") != up.URL {
		t.Fatalf("Origin not rewritten: %q", got.Get("Origin"))
	}
	if loc := rec.Header().Get("Location"); loc != "https://ah-mcp.example.com/next" {
		t.Fatalf("absolute AH Location = %q", loc)
	}
}

func up2(r *http.Request) string { return "http://" + r.Host }

func TestSanitizeCookie(t *testing.T) {
	in := "s=1; Domain=login.ah.nl; Path=/; Secure; HttpOnly; SameSite=None"
	if got := ahlogin.SanitizeCookie(in, false); got != "s=1; Path=/; Secure; HttpOnly" {
		t.Errorf("https: %q", got)
	}
	if got := ahlogin.SanitizeCookie(in, true); got != "s=1; Path=/; HttpOnly" {
		t.Errorf("http: %q", got)
	}
}
