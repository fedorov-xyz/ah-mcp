// Package server wires the single HTTP port: /mcp (Streamable HTTP, bearer
// protected), the OAuth endpoints and metadata, the AH login proxy and
// /healthz.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/fedorov-xyz/ah-mcp/internal/ahlogin"
	"github.com/fedorov-xyz/ah-mcp/internal/config"
	"github.com/fedorov-xyz/ah-mcp/internal/oauth"
	"github.com/fedorov-xyz/ah-mcp/internal/session"
	"github.com/fedorov-xyz/ah-mcp/internal/tools"
)

// Rate limits per client IP.
const (
	oauthPerMinute = 30
	// The relayed AH login page loads its own scripts, styles and API
	// calls, so the proxy gets a larger budget than the OAuth endpoints.
	loginProxyPerMinute = 600
)

// Options are the server's collaborators.
type Options struct {
	Config *config.Config
	OAuth  *oauth.Server
	Tools  *tools.Deps
	Logger *slog.Logger
	// Instructions are sent to MCP clients on initialize.
	Instructions string
}

const defaultInstructions = "Albert Heijn (Dutch supermarket) tools. Key facts: without an active online order the shopping list and the cart are the same basket; " +
	"an active order exists only after the user picks a delivery/pickup slot in the AH app; this server never checks out, creates orders or books slots; " +
	"destructive tools need confirm=\"yes\" after showing the user a preview; personal Bonus Box offers are read-only. Prices are in EUR."

// New returns the root handler.
func New(o Options) http.Handler {
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	instructions := o.Instructions
	if instructions == "" {
		instructions = defaultInstructions
	}
	mcpSrv := mcpserver.NewMCPServer("ah-mcp", o.Tools.Version,
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithRecovery(),
		mcpserver.WithInstructions(instructions),
		mcpserver.WithTitle("Albert Heijn"),
	)
	tools.Register(mcpSrv, o.Tools)

	streamable := mcpserver.NewStreamableHTTPServer(mcpSrv,
		mcpserver.WithEndpointPath("/mcp"),
		mcpserver.WithHeartbeatInterval(30*time.Second),
		mcpserver.WithStreamableHTTPLogger(logger),
		// Our own Origin allowlist plus mandatory bearer auth replace the
		// SDK's Host-header check, which would reject a same-host reverse
		// proxy (Caddy on 127.0.0.1) that preserves the public Host.
		mcpserver.WithDisableLocalhostProtection(true),
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			if s, ok := session.From(r.Context()); ok {
				return session.With(ctx, s)
			}
			return ctx
		}),
	)

	cfg := o.Config
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	oauthMux := http.NewServeMux()
	o.OAuth.Routes(oauthMux)
	oauthLimiter := NewRateLimiter(oauthPerMinute, cfg.TrustedProxies)
	proxyLimiter := NewRateLimiter(loginProxyPerMinute, cfg.TrustedProxies)
	mux.Handle("/.well-known/", oauthMux)
	mux.Handle("/oauth/", oauthLimiter.Middleware(oauthMux))
	mux.Handle("/oauth/ah/", proxyLimiter.Middleware(oauthMux))

	mcpHandler := OriginCheck(cfg.AllowedOrigins, cfg.AllowAllOrigins, logger,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, aerr := o.OAuth.Authenticate(r)
			if aerr != nil {
				o.OAuth.WriteChallenge(w, aerr)
				return
			}
			streamable.ServeHTTP(w, r.WithContext(session.With(r.Context(), sess)))
		}))
	mux.Handle("/mcp", mcpHandler)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if proxyLimiter.Allow(ClientIP(r, cfg.TrustedProxies)) && o.OAuth.ServeLoginFallback(w, r) {
			return
		}
		http.NotFound(w, r)
	})

	return AccessLog(logger, securityHeaders(mux))
}

// securityHeaders sets defensive headers on our own responses. Requests of
// a browser in the middle of an AH sign-in (login cookie) may be relayed to
// AH, whose responses keep their own headers (minus those the proxy strips).
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie(ahlogin.LoginCookie); err != nil && !strings.HasPrefix(r.URL.Path, "/oauth/ah/") {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			if h.Get("Referrer-Policy") == "" {
				h.Set("Referrer-Policy", "no-referrer")
			}
		}
		next.ServeHTTP(w, r)
	})
}
