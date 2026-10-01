package main

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/fedorov-xyz/ah-mcp/internal/config"
)

func envOf(m map[string]string) config.Getenv { return func(k string) string { return m[k] } }

func TestVersionAndUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--version"}, envOf(nil), &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "ah-mcp ") {
		t.Fatalf("--version: %d %q", code, out.String())
	}
	if code := run([]string{"keygen"}, envOf(nil), &out, &errb); code != 2 {
		t.Fatalf("unknown subcommand: %d", code)
	}
	if code := run([]string{"--bogus"}, envOf(nil), &out, &errb); code != 2 {
		t.Fatalf("unknown flag: %d", code)
	}
}

func TestInvalidConfigExitsNonZero(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(nil, envOf(map[string]string{"AH_MCP_BASE_URL": "http://example.com"}), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "AH_MCP_SECRET_KEY") || !strings.Contains(errb.String(), "https://") {
		t.Fatalf("code=%d stderr=%s", code, errb.String())
	}
}

func TestAllowlistWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	startupWarnings(&config.Config{Bind: "127.0.0.1"}, logger)
	if !strings.Contains(buf.String(), "ANY Albert Heijn account can sign in") {
		t.Fatalf("no warning without allowlist: %s", buf.String())
	}
	buf.Reset()
	startupWarnings(&config.Config{Bind: "127.0.0.1", BaseURL: "http://127.0.0.1:3000", AllowedEmails: map[string]struct{}{"a@b.c": {}}}, logger)
	if buf.Len() != 0 {
		t.Fatalf("unexpected warnings: %s", buf.String())
	}
	startupWarnings(&config.Config{Bind: "127.0.0.1", BaseURL: "http://localhost:3000", AllowedEmails: map[string]struct{}{"a@b.c": {}}}, logger)
	if !strings.Contains(buf.String(), "hCaptcha") {
		t.Fatalf("no localhost warning: %s", buf.String())
	}
}

func TestHealthcheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(500)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	var errb bytes.Buffer
	if code := run([]string{"healthcheck"}, envOf(map[string]string{"AH_MCP_PORT": port}), &errb, &errb); code != 0 {
		t.Fatalf("healthy: %d %s", code, errb.String())
	}
	_ = srv.Close()
	if code := run([]string{"healthcheck"}, envOf(map[string]string{"AH_MCP_PORT": port}), &errb, &errb); code != 1 {
		t.Fatalf("down server: %d", code)
	}
	if code := run([]string{"healthcheck"}, envOf(map[string]string{"AH_MCP_PORT": "x"}), &errb, &errb); code != 1 {
		t.Fatalf("bad port: %d", code)
	}
}
