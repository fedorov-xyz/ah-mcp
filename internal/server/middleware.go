// Copyright (C) the Jelle7/ah-mcp contributors
// Copyright (C) Igor Fedorov and contributors
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// The Origin allowlist is adapted from github.com/Jelle7/ah-mcp/main.go
// (allowedOrigins, originMiddleware) @ 9ee133696d52ceeb689c3cde1d07eb8a1e0a7019 (AGPL-3.0).

package server

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/util"

	"github.com/fedorov-xyz/ah-mcp/internal/ahlogin"
	"github.com/fedorov-xyz/ah-mcp/internal/logx"
)

// OriginCheck blocks cross-origin browser requests to /mcp, which is what
// stops a malicious web page from driving the server via DNS rebinding.
// Requests without an Origin header (non-browser MCP clients) pass.
func OriginCheck(allowed []string, allowAll bool, logger *slog.Logger, next http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[strings.ToLower(strings.TrimSuffix(a, "/"))] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || allowAll || set[strings.ToLower(strings.TrimSuffix(origin, "/"))] {
			next.ServeHTTP(w, r)
			return
		}
		logger.Warn("rejected request with disallowed Origin (set AH_MCP_ALLOWED_ORIGINS to permit it)", "origin", origin)
		http.Error(w, "Forbidden origin", http.StatusForbidden)
	})
}

// ClientIP returns the caller's IP. X-Forwarded-For is honoured only when
// the direct peer is a trusted proxy; then the right-most address that is
// not itself a trusted proxy wins.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if !util.InPrefixes(peer, trusted) {
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !util.InPrefixes(a, trusted) {
			return a
		}
	}
	return peer
}

// RateLimiter is a per-IP token bucket.
type RateLimiter struct {
	rate    float64 // tokens per second
	burst   float64
	trusted []netip.Prefix
	now     func() time.Time

	mu      sync.Mutex
	buckets map[netip.Addr]*bucket
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows perMinute requests per IP with an equal burst.
func NewRateLimiter(perMinute int, trusted []netip.Prefix) *RateLimiter {
	return &RateLimiter{
		rate: float64(perMinute) / 60, burst: float64(perMinute), trusted: trusted,
		now: time.Now, buckets: map[netip.Addr]*bucket{},
	}
}

// SetClock overrides the clock (for tests).
func (l *RateLimiter) SetClock(now func() time.Time) { l.now = now }

// Allow reports whether ip may make another request.
func (l *RateLimiter) Allow(ip netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}
	b, ok := l.buckets[ip]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Middleware rejects over-limit requests with 429.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(ClientIP(r, l.trusted)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush on the real writer
// (needed for streaming responses).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Flush implements http.Flusher.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// AccessLog logs method, redacted path (never the query), status and
// duration.
func AccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		level := slog.LevelInfo
		if _, err := r.Cookie(ahlogin.LoginCookie); err == nil || strings.HasPrefix(r.URL.Path, "/oauth/ah/") {
			level = slog.LevelDebug // the relayed AH login page loads many assets
		}
		logger.Log(r.Context(), level, "http", "method", r.Method, "path", logx.Redact(r.URL.Path),
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}
