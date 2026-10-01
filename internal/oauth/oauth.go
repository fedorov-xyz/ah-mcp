// Package oauth implements the in-house OAuth 2.1 authorization server that
// fronts ah-mcp, following the MCP Authorization specification: protected
// resource metadata (RFC 9728), authorization server metadata (RFC 8414),
// public-client registration via DCR (RFC 7591) and Client ID Metadata
// Documents, PKCE S256 (RFC 7636), resource indicators (RFC 8707) and
// issuer identification (RFC 9207).
//
// Users authenticate with Albert Heijn through a login reverse proxy. The
// AH tokens obtained there are sealed inside the tokens issued to the MCP
// client, so the server keeps no persistent state: only pending
// authorizations, authorization codes and the CIMD cache live in memory.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/util"

	"github.com/fedorov-xyz/ah-mcp/internal/ahlogin"
	"github.com/fedorov-xyz/ah-mcp/internal/oauth/seal"
)

// Lifetimes.
const (
	PendingTTL     = 10 * time.Minute
	CodeTTL        = 60 * time.Second
	MaxAccessTTL   = time.Hour
	AHExpiryMargin = 60 * time.Second
	clientTTL      = 10 * 365 * 24 * time.Hour
	refreshReuse   = 30 * time.Second
)

// AHAuth is the AH identity provider (implemented by *ahlogin.Client).
type AHAuth interface {
	Exchange(ctx context.Context, code string) (ahlogin.Tokens, error)
	Refresh(ctx context.Context, refreshToken string) (ahlogin.Tokens, error)
	Member(ctx context.Context, accessToken string) (ahlogin.Member, error)
}

// Options configures a Server.
type Options struct {
	// BaseURL is the public origin, without trailing slash.
	BaseURL string
	// SessionTTL is the refresh-token lifetime.
	SessionTTL time.Duration
	// EmailAllowed implements AH_MCP_ALLOWED_EMAILS.
	EmailAllowed func(email string) bool
	Sealer       *seal.Sealer
	AH           AHAuth
	// AHLoginBase is the AH login host the proxy relays to.
	AHLoginBase string
	Logger      *slog.Logger
	// CIMD fetches Client ID Metadata Documents; nil uses the default
	// SSRF-guarded fetcher.
	CIMD *CIMDFetcher
	Now  func() time.Time
}

// Server is the authorization server. Mount its handlers with Routes.
type Server struct {
	opts      Options
	loginBase *url.URL
	log       *slog.Logger
	now       func() time.Time
	pending   *store[*pendingAuth]
	codes     *store[*authCode]
	cimd      *CIMDFetcher
	refreshes *refreshGroup
}

// New validates opts and returns a Server.
func New(opts Options) (*Server, error) {
	if opts.Sealer == nil || opts.AH == nil || opts.BaseURL == "" {
		return nil, errInvalidOptions
	}
	if opts.AHLoginBase == "" {
		opts.AHLoginBase = ahlogin.DefaultLoginBase
	}
	lb, err := url.Parse(opts.AHLoginBase)
	if err != nil || lb.Host == "" {
		return nil, errInvalidOptions
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	} else {
		// Token expiry and session bookkeeping share one clock.
		opts.Sealer = opts.Sealer.WithClock(opts.Now)
	}
	if opts.EmailAllowed == nil {
		opts.EmailAllowed = func(string) bool { return true }
	}
	if opts.CIMD == nil {
		opts.CIMD = NewCIMDFetcher()
	}
	opts.BaseURL = strings.TrimSuffix(opts.BaseURL, "/")
	s := &Server{
		opts:      opts,
		loginBase: lb,
		log:       opts.Logger,
		now:       opts.Now,
		pending:   newStore[*pendingAuth](opts.Now),
		codes:     newStore[*authCode](opts.Now),
		cimd:      opts.CIMD,
	}
	s.refreshes = newRefreshGroup(opts.Now)
	return s, nil
}

type optionsError string

func (e optionsError) Error() string { return string(e) }

const errInvalidOptions = optionsError("oauth: invalid options")

// Issuer is the authorization server identifier.
func (s *Server) Issuer() string { return s.opts.BaseURL }

// Resource is the canonical MCP resource URI.
func (s *Server) Resource() string { return s.opts.BaseURL + "/mcp" }

// ResourceMetadataURL is advertised in WWW-Authenticate challenges.
func (s *Server) ResourceMetadataURL() string {
	return s.opts.BaseURL + "/.well-known/oauth-protected-resource/mcp"
}

// Routes registers every unauthenticated OAuth endpoint on mux. The caller
// wraps /oauth/* with rate limiting.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.handleResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleASMetadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleASMetadata)
	mux.HandleFunc("POST /oauth/register", s.handleRegister)
	mux.HandleFunc("GET /oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /oauth/token", s.handleToken)
	mux.HandleFunc("/oauth/ah/", s.handleLoginProxy)
}

func (s *Server) handleResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.Resource(),
		"authorization_servers":    []string{s.Issuer()},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Albert Heijn MCP (ah-mcp)",
	})
}

// ASMetadata is the RFC 8414 document.
func (s *Server) ASMetadata() map[string]any {
	b := s.opts.BaseURL
	return map[string]any{
		"issuer":                                         b,
		"authorization_endpoint":                         b + "/oauth/authorize",
		"token_endpoint":                                 b + "/oauth/token",
		"registration_endpoint":                          b + "/oauth/register",
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	}
}

func (s *Server) handleASMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.ASMetadata())
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("oauth: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func clientHash(clientID string) string {
	sum := sha256.Sum256([]byte("ah-mcp/client/" + clientID))
	return hex.EncodeToString(sum[:16])
}

func (s *Server) resourceOK(v string) bool {
	return v == "" || util.CanonicalURL(v) == s.Resource()
}
