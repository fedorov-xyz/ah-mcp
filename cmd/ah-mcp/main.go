// Command ah-mcp is a self-hostable Albert Heijn MCP server (Streamable
// HTTP, OAuth 2.1 sign-in with each user's own AH account).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/util"

	"github.com/fedorov-xyz/ah-mcp/internal/ah"
	"github.com/fedorov-xyz/ah-mcp/internal/ahlogin"
	"github.com/fedorov-xyz/ah-mcp/internal/cache"
	"github.com/fedorov-xyz/ah-mcp/internal/config"
	"github.com/fedorov-xyz/ah-mcp/internal/logx"
	"github.com/fedorov-xyz/ah-mcp/internal/oauth"
	"github.com/fedorov-xyz/ah-mcp/internal/oauth/seal"
	"github.com/fedorov-xyz/ah-mcp/internal/server"
	"github.com/fedorov-xyz/ah-mcp/internal/tools"
)

// Set at build time via -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "healthcheck" {
		return healthcheck(getenv, stderr)
	}

	fs := flag.NewFlagSet("ah-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print version and exit")
	remote := fs.Bool("remote", false, "listen on all interfaces (same as AH_REMOTE=true)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: ah-mcp [--remote] [--version]\n       ah-mcp healthcheck\n\nConfiguration is read from AH_MCP_* environment variables; see README.md.\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unknown command %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "ah-mcp %s (commit %s)\n", version, commit)
		return 0
	}

	cfg, err := config.Load(getenv, *remote)
	if err != nil {
		fmt.Fprintf(stderr, "ah-mcp: invalid configuration:\n%v\n", err)
		return 1
	}
	logger := logx.New(stderr, cfg.LogLevel)
	slog.SetDefault(logger)

	startupWarnings(cfg, logger)

	sealer, err := seal.New(cfg.SecretKey)
	if err != nil {
		fmt.Fprintf(stderr, "ah-mcp: %v\n", err)
		return 1
	}
	ahAuth := ahlogin.New("", "", &http.Client{
		Timeout:   30 * time.Second,
		Transport: &ah.RetryTransport{Base: http.DefaultTransport},
	})
	as, err := oauth.New(oauth.Options{
		BaseURL:      cfg.BaseURL,
		SessionTTL:   cfg.SessionTTL,
		EmailAllowed: cfg.EmailAllowed,
		Sealer:       sealer,
		AH:           ahAuth,
		AHLoginBase:  ahAuth.LoginBase,
		Logger:       logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "ah-mcp: %v\n", err)
		return 1
	}
	deps := &tools.Deps{
		AH:               ah.NewFactory(""),
		Cache:            cache.New(4096),
		Version:          version,
		Commit:           commit,
		AllowlistEnabled: cfg.AllowlistEnabled(),
		Started:          time.Now(),
		Logger:           logger,
	}
	handler := server.New(server.Options{Config: cfg, OAuth: as, Tools: deps, Logger: logger})

	srv := &http.Server{
		Addr:              cfg.ListenAddr(),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		// No Read/WriteTimeout: /mcp responses may stream.
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		fmt.Fprintf(stderr, "ah-mcp: listen %s: %v\n", srv.Addr, err)
		return 1
	}
	logger.Info("ah-mcp started", "version", version, "commit", commit, "addr", ln.Addr().String(), "base_url", cfg.BaseURL,
		"allowlist", cfg.AllowlistEnabled())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "err", err)
			return 1
		}
	case <-ctx.Done():
		logger.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}
	return 0
}

// startupWarnings logs configuration risks once at start.
func startupWarnings(cfg *config.Config, logger *slog.Logger) {
	if !cfg.AllowlistEnabled() {
		logger.Warn("AH_MCP_ALLOWED_EMAILS is not set: ANY Albert Heijn account can sign in to this server. Set it if anyone but you can reach it.")
	}
	if u, err := url.Parse(cfg.BaseURL); err == nil && strings.EqualFold(u.Hostname(), "localhost") {
		logger.Warn("AH_MCP_BASE_URL uses localhost: the Albert Heijn login page is protected by hCaptcha, which rejects the host 'localhost', so sign-in will hang. Use http://127.0.0.1:<port> instead.")
	}
	if !util.IsLoopbackHost(cfg.Bind) {
		logger.Info("listening on a non-loopback address; put a TLS reverse proxy in front", "bind", cfg.Bind)
	}
}

// healthcheck GETs http://127.0.0.1:$AH_MCP_PORT/healthz (distroless has no
// curl). Exit 0 on 200, 1 otherwise.
func healthcheck(getenv config.Getenv, stderr io.Writer) int {
	port := config.DefaultPort
	if v := getenv("AH_MCP_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			fmt.Fprintf(stderr, "healthcheck: invalid AH_MCP_PORT %q\n", v)
			return 1
		}
		port = p
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/healthz")
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
