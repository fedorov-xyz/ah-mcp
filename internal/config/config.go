// Package config parses and validates the ah-mcp runtime configuration from
// environment variables and command-line flags.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

// MinSecretKeyBytes is the minimum decoded length of AH_MCP_SECRET_KEY.
const MinSecretKeyBytes = 32

// Defaults.
const (
	DefaultPort       = 3000
	DefaultBind       = "127.0.0.1"
	DefaultSessionTTL = 720 * time.Hour
	DefaultSite       = "nl"
)

// Config is the validated runtime configuration.
type Config struct {
	// BaseURL is the public origin without a trailing slash, e.g.
	// "https://ah-mcp.example.com". All advertised URLs are built from it.
	BaseURL string
	// SecretKey seals every token the server issues. Changing it
	// invalidates every issued token ("log everyone out").
	SecretKey []byte
	// AllowedEmails is the lower-cased allowlist. Empty means every AH
	// account may sign in.
	AllowedEmails map[string]struct{}
	SessionTTL    time.Duration
	Port          int
	Bind          string
	// AllowedOrigins is the Origin allowlist for /mcp. AllowAllOrigins
	// disables the check ("*").
	AllowedOrigins  []string
	AllowAllOrigins bool
	TrustedProxies  []netip.Prefix
	LogLevel        slog.Level
	Site            string
}

// Getenv looks up an environment variable; os.Getenv satisfies it.
type Getenv func(string) string

// Load builds a Config from the environment. remote is the value of the
// --remote flag.
func Load(getenv Getenv, remote bool) (*Config, error) {
	var errs []error
	cfg := &Config{
		SessionTTL: DefaultSessionTTL,
		Port:       DefaultPort,
		Site:       DefaultSite,
		LogLevel:   slog.LevelInfo,
	}

	base, err := parseBaseURL(getenv("AH_MCP_BASE_URL"))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.BaseURL = base

	key, err := DecodeKey(getenv("AH_MCP_SECRET_KEY"))
	if err != nil {
		errs = append(errs, fmt.Errorf("AH_MCP_SECRET_KEY: %w", err))
	}
	cfg.SecretKey = key

	cfg.AllowedEmails = util.LowerSet(getenv("AH_MCP_ALLOWED_EMAILS"))

	if v := strings.TrimSpace(getenv("AH_MCP_SESSION_TTL")); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("AH_MCP_SESSION_TTL: %w", err))
		case d < time.Hour:
			errs = append(errs, errors.New("AH_MCP_SESSION_TTL must be at least 1h"))
		default:
			cfg.SessionTTL = d
		}
	}

	if v := strings.TrimSpace(getenv("AH_MCP_PORT")); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("AH_MCP_PORT: invalid port %q", v))
		} else {
			cfg.Port = p
		}
	}

	if util.ParseBool(getenv("AH_REMOTE")) {
		remote = true
	}
	cfg.Bind = DefaultBind
	if remote {
		cfg.Bind = "0.0.0.0"
	}
	if v := strings.TrimSpace(getenv("AH_MCP_BIND")); v != "" {
		host := strings.Trim(v, "[]")
		if _, err := netip.ParseAddr(host); err != nil && host != "localhost" {
			errs = append(errs, fmt.Errorf("AH_MCP_BIND must be an IP address without a port, got %q", v))
		}
		cfg.Bind = host
	}

	origins, all, err := parseOrigins(getenv("AH_MCP_ALLOWED_ORIGINS"), cfg.BaseURL, cfg.Port)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.AllowedOrigins, cfg.AllowAllOrigins = origins, all

	proxies, err := util.ParsePrefixes(getenv("AH_MCP_TRUSTED_PROXIES"))
	if err != nil {
		errs = append(errs, fmt.Errorf("AH_MCP_TRUSTED_PROXIES: %w", err))
	}
	cfg.TrustedProxies = proxies

	if v := strings.TrimSpace(getenv("AH_LOG_LEVEL")); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("AH_LOG_LEVEL: invalid level %q", v))
		}
	}

	if v := strings.ToLower(strings.TrimSpace(getenv("AH_SITE"))); v != "" && v != DefaultSite {
		errs = append(errs, fmt.Errorf("AH_SITE: only %q is supported, got %q", DefaultSite, v))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// ResourceURL is the canonical MCP resource identifier (RFC 8707).
func (c *Config) ResourceURL() string { return c.BaseURL + "/mcp" }

// ListenAddr is the host:port the HTTP server binds.
func (c *Config) ListenAddr() string {
	return net.JoinHostPort(c.Bind, strconv.Itoa(c.Port))
}

// AllowlistEnabled reports whether AH_MCP_ALLOWED_EMAILS is set.
func (c *Config) AllowlistEnabled() bool { return len(c.AllowedEmails) > 0 }

// EmailAllowed reports whether email may use the server. With no
// allowlist every account is allowed.
func (c *Config) EmailAllowed(email string) bool {
	if len(c.AllowedEmails) == 0 {
		return true
	}
	_, ok := c.AllowedEmails[strings.ToLower(strings.TrimSpace(email))]
	return ok
}

// DecodeKey decodes a base64 (standard or URL alphabet, padded or not) key
// and checks its length.
func DecodeKey(v string) ([]byte, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, errors.New("required (generate one with `openssl rand -base64 32`)")
	}
	var key []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err = enc.DecodeString(v); err == nil {
			break
		}
	}
	if err != nil {
		return nil, errors.New("must be base64")
	}
	if len(key) < MinSecretKeyBytes {
		return nil, fmt.Errorf("must decode to at least %d bytes, got %d", MinSecretKeyBytes, len(key))
	}
	return key, nil
}

func parseBaseURL(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", errors.New("AH_MCP_BASE_URL: required, e.g. https://ah-mcp.example.com")
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("AH_MCP_BASE_URL: invalid URL %q", v)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	switch {
	case scheme == "https":
	case scheme == "http" && (host == "localhost" || host == "127.0.0.1"):
	default:
		return "", fmt.Errorf("AH_MCP_BASE_URL: must use https:// (http:// is only allowed for localhost/127.0.0.1), got %q", v)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("AH_MCP_BASE_URL: must be a bare origin without path, query or credentials, got %q", v)
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

func parseOrigins(v, baseURL string, port int) ([]string, bool, error) {
	v = strings.TrimSpace(v)
	if v != "" {
		var out []string
		for _, o := range util.SplitCSV(v) {
			o = strings.ToLower(strings.TrimSuffix(o, "/"))
			if o == "*" {
				return nil, true, nil
			}
			out = append(out, o)
		}
		return out, false, nil
	}
	out := []string{
		fmt.Sprintf("http://localhost:%d", port),
		fmt.Sprintf("http://127.0.0.1:%d", port),
		fmt.Sprintf("http://[::1]:%d", port),
		"https://claude.ai",
	}
	if baseURL != "" {
		out = append(out, baseURL)
	}
	return out, false, nil
}
