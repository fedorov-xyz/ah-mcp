// Package ahlogin talks to Albert Heijn's identity endpoints: it relays the
// AH login page through a reverse proxy (see proxy.go), exchanges the
// resulting authorization code for AH tokens, and refreshes them.
package ahlogin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	appie "github.com/gwillem/appie-go"
)

// Upstream defaults for AH_SITE=nl.
const (
	DefaultAPIBase   = "https://api.ah.nl"
	DefaultLoginBase = "https://login.ah.nl"
	// ClientID is the AH OAuth client id; copied from appie-go
	// clientIDForSite("nl").
	ClientID = "appie-ios"
	// AppRedirect is the only redirect_uri AH accepts for ClientID.
	AppRedirect = "appie://login-exit"
)

// ErrUpstream is returned for any failed AH auth call. The underlying error
// is deliberately dropped: AH error bodies on token endpoints can echo
// credentials.
var ErrUpstream = errors.New("ahlogin: AH auth request failed")

// Tokens is a set of AH tokens.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	MemberID     string
	ExpiresAt    time.Time
}

// tokenResponse mirrors appie-go's unexported token type.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	MemberID     string `json:"member_id,omitempty"`
	ExpiresIn    int    `json:"expires_in"`
}

// DefaultTokenLifetime is assumed when AH omits expires_in. It errs short:
// a too-short guess only causes an extra refresh.
const DefaultTokenLifetime = 15 * time.Minute

// Member is the subset of the AH member profile the auth flow needs.
type Member struct {
	ID    string
	Email string
}

// Client performs AH auth calls.
type Client struct {
	APIBase    string
	LoginBase  string
	HTTPClient *http.Client
	Now        func() time.Time
}

// New returns a Client for the given hosts ("" selects the NL defaults).
func New(apiBase, loginBase string, hc *http.Client) *Client {
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if loginBase == "" {
		loginBase = DefaultLoginBase
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{APIBase: strings.TrimSuffix(apiBase, "/"), LoginBase: strings.TrimSuffix(loginBase, "/"), HTTPClient: hc, Now: time.Now}
}

func (c *Client) appie(opts ...appie.Option) *appie.Client {
	base := []appie.Option{appie.WithSite("nl"), appie.WithBaseURL(c.APIBase), appie.WithHTTPClient(c.HTTPClient)}
	return appie.New(append(base, opts...)...)
}

// LoginPath is the path+query of the AH login page for our client id.
func LoginPath() string {
	return "/login?client_id=" + ClientID + "&response_type=code&redirect_uri=" + AppRedirect
}

// Exchange trades an AH authorization code for tokens. It mirrors
// appie-go's unexported exchangeCode, using the exported DoRequest.
func (c *Client) Exchange(ctx context.Context, code string) (Tokens, error) {
	body := map[string]string{"clientId": ClientID, "code": code}
	return c.tokenCall(ctx, "/mobile-auth/v1/auth/token", body)
}

// Refresh trades an AH refresh token for a new pair. AH rotates refresh
// tokens: the old one is invalid afterwards.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	body := map[string]string{"clientId": ClientID, "refreshToken": refreshToken}
	return c.tokenCall(ctx, "/mobile-auth/v1/auth/token/refresh", body)
}

func (c *Client) tokenCall(ctx context.Context, path string, body any) (Tokens, error) {
	var tok tokenResponse
	if err := c.appie().DoRequest(ctx, http.MethodPost, path, body, &tok); err != nil {
		return Tokens{}, ErrUpstream
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return Tokens{}, ErrUpstream
	}
	lifetime := time.Duration(tok.ExpiresIn) * time.Second
	if tok.ExpiresIn <= 0 {
		lifetime = DefaultTokenLifetime
	}
	return Tokens{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		MemberID:     tok.MemberID,
		ExpiresAt:    c.Now().Add(lifetime),
	}, nil
}

// Member fetches the member profile for an AH access token.
func (c *Client) Member(ctx context.Context, accessToken string) (Member, error) {
	m, err := c.appie(appie.WithTokens(accessToken, "")).GetMember(ctx)
	if err != nil {
		return Member{}, fmt.Errorf("ahlogin: get member: %w", ErrUpstream)
	}
	if m.ID == "" || m.ID == "0" {
		return Member{}, fmt.Errorf("ahlogin: member without id: %w", ErrUpstream)
	}
	return Member{ID: m.ID, Email: m.Email}, nil
}
