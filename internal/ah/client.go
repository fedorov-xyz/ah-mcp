// Package ah holds thin adapters over github.com/gwillem/appie-go: the
// per-request client factory, an upstream retry policy, and the AH calls
// appie-go does not (yet) expose — the GraphQL basket, past orders, bonus
// tiers and personal bonus status.
package ah

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	appie "github.com/gwillem/appie-go"

	"github.com/fedorov-xyz/ah-mcp/internal/session"
	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

// Factory builds one appie-go client per request. There is no shared
// mutable client: each call gets its own, seeded with the caller's AH
// access token only.
type Factory struct {
	// APIBase overrides https://api.ah.nl (tests point it at a fake).
	APIBase string
	HTTP    *http.Client
}

// NewFactory returns a Factory whose HTTP client limits concurrency per
// caller (see LimitTransport) and retries per the upstream policy (see
// RetryTransport).
func NewFactory(apiBase string) *Factory {
	return &Factory{
		APIBase: apiBase,
		HTTP: &http.Client{
			Timeout:   90 * time.Second,
			Transport: &LimitTransport{Base: &RetryTransport{Base: http.DefaultTransport}},
		},
	}
}

// DefaultMaxPerCaller is how many AH requests one caller may have in flight.
const DefaultMaxPerCaller = 4

// LimitTransport caps the number of concurrent AH requests per caller (keyed
// by a hash of the Authorization header), across all tool calls. Without it,
// an agent running several history tools in parallel multiplies the
// per-tool fan-out and AH answers with 429s and errors.
type LimitTransport struct {
	Base http.RoundTripper
	Max  int // default DefaultMaxPerCaller

	mu    sync.Mutex
	slots map[[32]byte]*callerSlot
}

type callerSlot struct {
	sem   chan struct{}
	users int
}

func (t *LimitTransport) acquire(key [32]byte) *callerSlot {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.slots == nil {
		t.slots = map[[32]byte]*callerSlot{}
	}
	s := t.slots[key]
	if s == nil {
		n := t.Max
		if n <= 0 {
			n = DefaultMaxPerCaller
		}
		s = &callerSlot{sem: make(chan struct{}, n)}
		t.slots[key] = s
	}
	s.users++
	return s
}

func (t *LimitTransport) release(key [32]byte, s *callerSlot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.users--; s.users == 0 {
		delete(t.slots, key)
	}
}

// RoundTrip implements http.RoundTripper.
func (t *LimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	s := t.acquire(key)
	defer t.release(key, s)
	select {
	case s.sem <- struct{}{}:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	defer func() { <-s.sem }()
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// Client returns an appie-go client for s.
//
// It is given the AH access token without a refresh token and without an
// expiry, so appie-go's ensureFreshToken can never refresh inside a tool
// call. The bearer middleware guarantees the token is valid for at least
// another minute when the request starts; refreshing happens only at our
// /oauth/token endpoint, where the rotated AH refresh token can be returned
// to the client.
func (f *Factory) Client(s session.Session) *appie.Client {
	opts := []appie.Option{appie.WithSite("nl")}
	if f.APIBase != "" {
		opts = append(opts, appie.WithBaseURL(f.APIBase))
	}
	opts = append(opts, appie.WithHTTPClient(f.HTTP), appie.WithTokens(s.AHAccessToken, ""))
	return appie.New(opts...)
}

// ErrRateLimited matches a StatusError with status 429.
var ErrRateLimited = errors.New("AH rate limit")

// StatusError is returned (wrapped by appie-go) when AH answers with a
// status the tools need to distinguish: 401, 403, 404, 429 or a 5xx that
// survived retries. It never carries the response body.
type StatusError struct{ Status int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("Albert Heijn API returned HTTP %d", e.Status)
}

// Is lets errors.Is(err, ErrRateLimited) match a 429.
func (e *StatusError) Is(target error) bool {
	return target == ErrRateLimited && e.Status == http.StatusTooManyRequests
}

// StatusOf returns the AH HTTP status carried by err, or 0.
func StatusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// RetryTransport retries AH requests on 429 and 5xx with exponential
// backoff and jitter, honouring Retry-After, for at most MaxAttempts
// attempts. Mutations (non-GET requests, and GraphQL "mutation"
// operations) are retried only on 429, which guarantees the write was not
// applied.
type RetryTransport struct {
	Base        http.RoundTripper
	MaxAttempts int           // default 3
	BaseDelay   time.Duration // default 500ms
	MaxDelay    time.Duration // default 10s
	// Sleep is overridable for tests.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (t *RetryTransport) params() (int, time.Duration, time.Duration) {
	attempts, base, maxd := t.MaxAttempts, t.BaseDelay, t.MaxDelay
	if attempts <= 0 {
		attempts = 3
	}
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	if maxd <= 0 {
		maxd = 10 * time.Second
	}
	return attempts, base, maxd
}

// RoundTrip implements http.RoundTripper.
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	mutation := IsMutation(req.Method, req.URL.Path, body)
	attempts, base, maxd := t.params()
	sleep := t.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	rt := t.Base
	if rt == nil {
		rt = http.DefaultTransport
	}

	for attempt := 1; ; attempt++ {
		r := req.Clone(req.Context())
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		resp, err := rt.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		status := resp.StatusCode
		retryable := status == http.StatusTooManyRequests || (!mutation && status >= 500)
		if !retryable {
			if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound {
				drain(resp)
				return nil, &StatusError{Status: status}
			}
			return resp, nil
		}
		if attempt >= attempts {
			drain(resp)
			return nil, &StatusError{Status: status}
		}
		wait := util.Backoff(base, maxd, attempt)
		if ra, ok := util.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			wait = min(ra, maxd)
		}
		drain(resp)
		if err := sleep(req.Context(), wait); err != nil {
			return nil, err
		}
	}
}

// IsMutation classifies a request for the retry policy.
func IsMutation(method, path string, body []byte) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if strings.HasSuffix(path, "/graphql") {
		var q struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(body, &q) == nil {
			return strings.HasPrefix(strings.TrimSpace(q.Query), "mutation")
		}
	}
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
