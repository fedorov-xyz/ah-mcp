package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

var testKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func base() map[string]string {
	return map[string]string{"AH_MCP_BASE_URL": "https://ah-mcp.example.com", "AH_MCP_SECRET_KEY": testKey}
}

func TestRequiredVars(t *testing.T) {
	_, err := Load(env(map[string]string{}), false)
	if err == nil {
		t.Fatal("expected error without required vars")
	}
	for _, want := range []string{"AH_MCP_BASE_URL", "AH_MCP_SECRET_KEY", "openssl rand -base64 32"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(env(base()), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://ah-mcp.example.com" || cfg.ResourceURL() != "https://ah-mcp.example.com/mcp" {
		t.Errorf("base = %q", cfg.BaseURL)
	}
	if cfg.Port != 3000 || cfg.Bind != "127.0.0.1" || cfg.ListenAddr() != "127.0.0.1:3000" {
		t.Errorf("listen = %s", cfg.ListenAddr())
	}
	if cfg.SessionTTL != 720*time.Hour || cfg.Site != "nl" || cfg.AllowlistEnabled() {
		t.Errorf("defaults wrong: %+v", cfg)
	}
	if !cfg.EmailAllowed("anyone@example.com") {
		t.Error("no allowlist must allow everyone")
	}
	want := map[string]bool{"https://ah-mcp.example.com": true, "https://claude.ai": true, "http://localhost:3000": true, "http://127.0.0.1:3000": true}
	for _, o := range cfg.AllowedOrigins {
		delete(want, o)
	}
	if len(want) != 0 {
		t.Errorf("missing default origins %v", want)
	}
}

func TestBaseURLRules(t *testing.T) {
	for in, ok := range map[string]bool{
		"https://ah.example.com":     true,
		"https://AH.example.com/":    true,
		"http://localhost:3000":      true,
		"http://127.0.0.1:3000":      true,
		"http://ah.example.com":      false,
		"http://192.168.1.2":         false,
		"https://ah.example.com/x":   false,
		"https://ah.example.com?a":   false,
		"https://u:p@ah.example.com": false,
		"ftp://ah.example.com":       false,
		"ah.example.com":             false,
	} {
		m := base()
		m["AH_MCP_BASE_URL"] = in
		cfg, err := Load(env(m), false)
		if (err == nil) != ok {
			t.Errorf("base %q: err=%v, want ok=%v", in, err, ok)
		}
		if ok && strings.HasSuffix(cfg.BaseURL, "/") {
			t.Errorf("base %q kept trailing slash", in)
		}
	}
}

func TestSecretKey(t *testing.T) {
	for in, ok := range map[string]bool{
		testKey: true,
		base64.RawURLEncoding.EncodeToString(make([]byte, 48)): true,
		base64.StdEncoding.EncodeToString(make([]byte, 31)):    false,
		"not base64 !!": false,
	} {
		m := base()
		m["AH_MCP_SECRET_KEY"] = in
		if _, err := Load(env(m), false); (err == nil) != ok {
			t.Errorf("key %q: err=%v want ok=%v", in, err, ok)
		}
	}
}

func TestSite(t *testing.T) {
	for in, ok := range map[string]bool{"": true, "nl": true, "NL": true, "be": false, "de": false} {
		m := base()
		m["AH_SITE"] = in
		if _, err := Load(env(m), false); (err == nil) != ok {
			t.Errorf("AH_SITE=%q: err=%v want ok=%v", in, err, ok)
		}
	}
}

func TestBindAndRemote(t *testing.T) {
	cfg, _ := Load(env(base()), true)
	if cfg.Bind != "0.0.0.0" {
		t.Errorf("--remote bind = %s", cfg.Bind)
	}
	m := base()
	m["AH_REMOTE"] = "true"
	if cfg, _ := Load(env(m), false); cfg.Bind != "0.0.0.0" {
		t.Errorf("AH_REMOTE bind = %s", cfg.Bind)
	}
	m = base()
	m["AH_MCP_BIND"] = "::"
	m["AH_MCP_PORT"] = "8080"
	if cfg, _ := Load(env(m), false); cfg.ListenAddr() != "[::]:8080" {
		t.Errorf("bind = %s", cfg.ListenAddr())
	}
	m["AH_MCP_BIND"] = "0.0.0.0:3000"
	if _, err := Load(env(m), false); err == nil {
		t.Error("bind with port accepted")
	}
	m = base()
	m["AH_MCP_PORT"] = "70000"
	if _, err := Load(env(m), false); err == nil {
		t.Error("bad port accepted")
	}
}

func TestAllowlistCaseInsensitive(t *testing.T) {
	m := base()
	m["AH_MCP_ALLOWED_EMAILS"] = " Alice@Example.com, bob@example.nl ,"
	cfg, err := Load(env(m), false)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowlistEnabled() || !cfg.EmailAllowed("alice@example.COM") || !cfg.EmailAllowed(" BOB@example.nl") || cfg.EmailAllowed("eve@example.com") {
		t.Error("allowlist matching wrong")
	}
}

func TestOriginsAndProxies(t *testing.T) {
	m := base()
	m["AH_MCP_ALLOWED_ORIGINS"] = "https://a.example/, https://B.example"
	m["AH_MCP_TRUSTED_PROXIES"] = "10.0.0.0/8, 192.168.1.1"
	cfg, err := Load(env(m), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedOrigins) != 2 || cfg.AllowedOrigins[0] != "https://a.example" || cfg.AllowedOrigins[1] != "https://b.example" || cfg.AllowAllOrigins {
		t.Errorf("origins = %v", cfg.AllowedOrigins)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Errorf("proxies = %v", cfg.TrustedProxies)
	}
	m["AH_MCP_ALLOWED_ORIGINS"] = "*"
	if cfg, _ := Load(env(m), false); !cfg.AllowAllOrigins {
		t.Error("* must disable origin check")
	}
	m["AH_MCP_TRUSTED_PROXIES"] = "bogus"
	if _, err := Load(env(m), false); err == nil {
		t.Error("bad proxies accepted")
	}
}

func TestSessionTTLAndLogLevel(t *testing.T) {
	m := base()
	m["AH_MCP_SESSION_TTL"] = "48h"
	m["AH_LOG_LEVEL"] = "debug"
	cfg, err := Load(env(m), false)
	if err != nil || cfg.SessionTTL != 48*time.Hour || cfg.LogLevel.String() != "DEBUG" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	m["AH_MCP_SESSION_TTL"] = "5m"
	if _, err := Load(env(m), false); err == nil {
		t.Error("too short TTL accepted")
	}
	m["AH_MCP_SESSION_TTL"] = "48h"
	m["AH_LOG_LEVEL"] = "loud"
	if _, err := Load(env(m), false); err == nil {
		t.Error("bad log level accepted")
	}
}
