package oauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

// CIMD limits.
const (
	cimdTimeout  = 5 * time.Second
	cimdMaxBytes = 64 << 10
	cimdCacheTTL = 10 * time.Minute
	cimdMaxCache = 1000
)

// ClientMetadata is the validated subset of a Client ID Metadata Document.
type ClientMetadata struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// CIMDFetcher fetches Client ID Metadata Documents with an SSRF guard:
// https only, no private/loopback/link-local destinations (checked on the
// resolved IP at dial time, so DNS rebinding cannot bypass it), a 5s
// timeout, a 64 KB cap and no redirects to other hosts. Results are cached
// for 10 minutes.
type CIMDFetcher struct {
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cimdEntry
}

type cimdEntry struct {
	doc     *ClientMetadata
	expires time.Time
}

// NewCIMDFetcher returns the production fetcher.
func NewCIMDFetcher() *CIMDFetcher {
	return newCIMDFetcher(util.IsPublicAddr, nil)
}

// NewCIMDFetcherForTest allows a custom address policy and TLS roots.
func NewCIMDFetcherForTest(allowed func(netip.Addr) bool, tlsCfg *tls.Config) *CIMDFetcher {
	return newCIMDFetcher(allowed, tlsCfg)
}

func newCIMDFetcher(allowed func(netip.Addr) bool, tlsCfg *tls.Config) *CIMDFetcher {
	dialer := &net.Dialer{
		Timeout: cimdTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("cimd: unparseable address %q", address)
			}
			if !allowed(ap.Addr().Unmap()) {
				return fmt.Errorf("cimd: destination %s is not a public address", ap.Addr())
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   cimdTimeout,
		ResponseHeaderTimeout: cimdTimeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
	f := &CIMDFetcher{now: time.Now, cache: map[string]cimdEntry{}}
	f.client = &http.Client{
		Transport: tr,
		Timeout:   cimdTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("cimd: too many redirects")
			}
			if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				return errors.New("cimd: redirect to another host is not allowed")
			}
			return nil
		},
	}
	return f
}

// Fetch returns the validated metadata document for clientID (an https
// URL).
func (f *CIMDFetcher) Fetch(ctx context.Context, clientID string) (*ClientMetadata, error) {
	u, err := url.Parse(clientID)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" ||
		u.Path == "" || u.Path == "/" {
		return nil, errors.New("cimd: client_id must be an https URL with a path")
	}

	f.mu.Lock()
	if e, ok := f.cache[clientID]; ok && f.now().Before(e.expires) {
		f.mu.Unlock()
		return e.doc, nil
	}
	f.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, cimdTimeout)
	defer cancel()
	// The dialer only connects to public addresses (see newCIMDFetcher).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil) //nolint:gosec // SSRF-guarded client
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req) //nolint:gosec // SSRF-guarded client
	if err != nil {
		return nil, fmt.Errorf("cimd: fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cimd: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cimd: read failed: %w", err)
	}
	if len(body) > cimdMaxBytes {
		return nil, errors.New("cimd: document exceeds 64 KB")
	}
	var doc ClientMetadata
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, errors.New("cimd: document is not valid JSON")
	}
	if doc.ClientID != clientID {
		return nil, errors.New("cimd: client_id in document does not match its URL")
	}
	if strings.TrimSpace(doc.ClientName) == "" {
		return nil, errors.New("cimd: client_name is required")
	}
	if len(doc.RedirectURIs) == 0 {
		return nil, errors.New("cimd: redirect_uris is required")
	}
	for _, r := range doc.RedirectURIs {
		if err := ValidateRedirectURI(r); err != nil {
			return nil, fmt.Errorf("cimd: %w", err)
		}
	}
	if m := doc.TokenEndpointAuthMethod; m != "" && m != "none" {
		return nil, errors.New("cimd: only public clients (token_endpoint_auth_method=none) are supported")
	}
	doc.ClientName = util.CleanText(doc.ClientName, maxClientNameLen)

	f.mu.Lock()
	if len(f.cache) >= cimdMaxCache {
		f.cache = map[string]cimdEntry{}
	}
	f.cache[clientID] = cimdEntry{doc: &doc, expires: f.now().Add(cimdCacheTTL)}
	f.mu.Unlock()
	return &doc, nil
}
