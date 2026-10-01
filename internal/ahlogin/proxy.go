// Copyright (C) the gwillem/appie-go contributors
// Copyright (C) the mrserzhan/ah-mcp and Jelle7/ah-mcp contributors
// Copyright (C) Igor Fedorov and contributors
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Adapted from github.com/Jelle7/ah-mcp/auth.go @ 9ee133696d52ceeb689c3cde1d07eb8a1e0a7019 (AGPL-3.0)
// (itself derived from github.com/mrserzhan/ah-mcp/auth.go @ d10ddf8e554f9c52a796cd1bd7bd295cddb7e70e)
// and github.com/gwillem/appie-go/login.go @ 65f41abdeaffb4b990cc35525cc0f20837102e1c (AGPL-3.0).
//
// Changes: served from the main port (selected per sign-in by a one-time
// cookie) instead of a dedicated listener; works for any target scheme;
// cookies are sanitised on the final app redirect too; the selection cookie
// is never forwarded upstream.

package ahlogin

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// maxRewriteBody bounds the size of a response body the proxy rewrites.
const maxRewriteBody = 8 << 20

// NewProxy returns a reverse proxy to the AH login host target, mounted at
// the root of publicOrigin (our AH_MCP_BASE_URL). The AH login page is a
// Next.js app whose runtime only recognises its own root-relative asset
// paths (/login/_next/...), so it cannot live under a path prefix; the
// caller decides which requests belong to a sign-in (see oauth's login
// cookie). The AH app redirect appie://login-exit is rewritten to
// callbackURL.
//
// Verified against the real login.ah.nl on 2026-10-01 (password, hCaptcha
// and 2FA (MFA) through to the callback).
func NewProxy(target *url.URL, publicOrigin, callbackURL string, logger *slog.Logger) http.Handler {
	publicOrigin = strings.TrimSuffix(publicOrigin, "/")
	insecure := strings.HasPrefix(strings.ToLower(publicOrigin), "http://")
	targetOrigin := target.Scheme + "://" + target.Host
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Accept-Encoding")
			// Never forward client identity or our own credentials upstream.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("Forwarded")
			pr.Out.Header.Del("Cookie")
			for _, c := range pr.In.Cookies() {
				if c.Name != LoginCookie {
					pr.Out.AddCookie(c)
				}
			}
			// AH may reject requests whose Origin/Referer name our host.
			if pr.In.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", targetOrigin)
			}
			if ref := pr.In.Header.Get("Referer"); ref != "" {
				pr.Out.Header.Set("Referer", strings.Replace(ref, publicOrigin, targetOrigin, 1))
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			return rewriteResponse(resp, publicOrigin, callbackURL, targetOrigin, insecure)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Warn("login proxy upstream error", "method", r.Method, "err", err)
			http.Error(w, "Albert Heijn login is unreachable, try again later", http.StatusBadGateway)
		},
	}
}

// LoginCookie names the cookie that ties a browser to a pending sign-in.
// It is never forwarded to AH.
const LoginCookie = "ahmcp_login"

// rewriteResponse intercepts AH login responses:
//   - rewrites appie:// Location redirects to callbackURL
//   - rewrites Location headers pointing at the login host to our origin
//   - strips security headers that block the proxy
//   - sanitises cookies
//   - replaces appie:// and login-host URLs in HTML/JS/JSON bodies
func rewriteResponse(resp *http.Response, publicOrigin, callbackURL, targetOrigin string, insecure bool) error {
	appRedirect := false
	if loc := resp.Header.Get("Location"); loc != "" {
		switch {
		case strings.HasPrefix(loc, "appie://"):
			u, err := url.Parse(loc)
			if err != nil {
				return fmt.Errorf("parse app redirect: %w", err)
			}
			resp.Header.Set("Location", callbackURL+"?"+u.Query().Encode())
			appRedirect = true
		case strings.HasPrefix(loc, targetOrigin):
			resp.Header.Set("Location", publicOrigin+strings.TrimPrefix(loc, targetOrigin))
		}
	}

	// Headers and cookies are rewritten on every response, including the
	// final app redirect (the upstream code returned early there and let
	// AH's Domain/SameSite attributes through).
	resp.Header.Del("Content-Security-Policy")
	resp.Header.Del("Content-Security-Policy-Report-Only")
	resp.Header.Del("Strict-Transport-Security")
	resp.Header.Del("X-Frame-Options")

	if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
		resp.Header.Del("Set-Cookie")
		for _, c := range cookies {
			resp.Header.Add("Set-Cookie", SanitizeCookie(c, insecure))
		}
	}

	if appRedirect {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") && !strings.Contains(ct, "javascript") && !strings.Contains(ct, "json") {
		return nil
	}
	body, err := readBody(resp)
	if err != nil {
		return err
	}
	body = bytes.ReplaceAll(body, []byte(AppRedirect), []byte(callbackURL))
	body = bytes.ReplaceAll(body, []byte(targetOrigin), []byte(publicOrigin))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("Content-Encoding")
	return nil
}

// SanitizeCookie strips Domain (it names the AH host, not ours) and
// SameSite from a Set-Cookie value, and Secure too when the browser reaches
// us over plain HTTP (local development only).
func SanitizeCookie(cookie string, insecure bool) string {
	parts := strings.Split(cookie, ";")
	out := parts[:1]
	for _, p := range parts[1:] {
		attr := strings.ToLower(strings.TrimSpace(p))
		switch {
		case attr == "secure":
			if !insecure {
				out = append(out, p)
			}
		case strings.HasPrefix(attr, "samesite"), strings.HasPrefix(attr, "domain"):
		default:
			out = append(out, p)
		}
	}
	return strings.Join(out, ";")
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	var r io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	data, err := io.ReadAll(io.LimitReader(r, maxRewriteBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRewriteBody {
		return nil, fmt.Errorf("login page response too large")
	}
	return data, nil
}
