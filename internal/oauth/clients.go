package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/fedorov-xyz/ah-mcp/internal/util"

	"github.com/fedorov-xyz/ah-mcp/internal/oauth/seal"
)

// Registration limits keep the sealed client id well under seal.MaxTokenLen.
const (
	maxRedirectURIs   = 5
	maxRedirectURILen = 512
	maxClientNameLen  = 100
)

// Client is a resolved public client.
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	// Kind is "dcr" (stateless sealed id) or "cimd" (metadata document URL).
	Kind string
}

// clientBlob is sealed into a DCR client_id.
type clientBlob struct {
	RedirectURIs []string `json:"redirect_uris"`
	ClientName   string   `json:"client_name,omitempty"`
	Iat          int64    `json:"iat"`
}

var errUnknownClient = errors.New("unknown client")

// ValidateRedirectURI enforces the registration rules: https URLs, http only
// on loopback hosts (RFC 8252), or private-use schemes for native apps; never
// fragments, wildcards, userinfo or script-capable schemes.
func ValidateRedirectURI(raw string) error {
	if raw == "" || len(raw) > maxRedirectURILen {
		return errors.New("redirect_uri is empty or too long")
	}
	if strings.ContainsAny(raw, "*# \t\r\n") {
		return errors.New("redirect_uri must not contain wildcards, fragments or whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return errors.New("redirect_uri must be an absolute URI")
	}
	if u.User != nil {
		return errors.New("redirect_uri must not contain credentials")
	}
	switch scheme := strings.ToLower(u.Scheme); scheme {
	case "https":
		if u.Host == "" {
			return errors.New("https redirect_uri needs a host")
		}
	case "http":
		host := u.Hostname()
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			return errors.New("http redirect_uri is only allowed for localhost, 127.0.0.1 or [::1]")
		}
	case "javascript", "data", "file", "vbscript", "blob", "about", "ftp", "ws", "wss":
		return fmt.Errorf("redirect_uri scheme %q is not allowed", scheme)
	default:
		// Private-use URI scheme for native apps (RFC 8252 §7.1).
		if u.Host == "" && u.Opaque == "" && u.Path == "" {
			return errors.New("redirect_uri with a custom scheme needs a path")
		}
	}
	return nil
}

// isLoopbackRedirect reports whether u is an http loopback redirect, whose
// port may vary (RFC 8252 §7.3).
func isLoopbackRedirect(u *url.URL) bool {
	return strings.EqualFold(u.Scheme, "http") && util.IsLoopbackHost(u.Hostname())
}

// MatchRedirectURI reports whether requested matches one of registered:
// exactly, or for loopback http URIs, with any port.
func MatchRedirectURI(registered []string, requested string) bool {
	ru, err := url.Parse(requested)
	if err != nil {
		return false
	}
	for _, reg := range registered {
		if reg == requested {
			return true
		}
		gu, err := url.Parse(reg)
		if err != nil || !isLoopbackRedirect(gu) || !isLoopbackRedirect(ru) {
			continue
		}
		if strings.EqualFold(gu.Scheme, ru.Scheme) && gu.Hostname() == ru.Hostname() &&
			gu.EscapedPath() == ru.EscapedPath() && gu.RawQuery == ru.RawQuery && ru.Fragment == "" {
			return true
		}
	}
	return false
}

type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registrationRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		registrationError(w, "invalid_client_metadata", "request body must be a JSON object")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > maxRedirectURIs {
		registrationError(w, "invalid_redirect_uri", fmt.Sprintf("between 1 and %d redirect_uris are required", maxRedirectURIs))
		return
	}
	for _, u := range req.RedirectURIs {
		if err := ValidateRedirectURI(u); err != nil {
			registrationError(w, "invalid_redirect_uri", err.Error())
			return
		}
	}
	if m := req.TokenEndpointAuthMethod; m != "" && m != "none" {
		registrationError(w, "invalid_client_metadata", "only public clients (token_endpoint_auth_method=none) are supported")
		return
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			registrationError(w, "invalid_client_metadata", "unsupported grant type "+g)
			return
		}
	}
	for _, rt := range req.ResponseTypes {
		if rt != "code" {
			registrationError(w, "invalid_client_metadata", "unsupported response type "+rt)
			return
		}
	}
	name := util.CleanText(req.ClientName, maxClientNameLen)
	now := s.now()
	id, err := s.opts.Sealer.Seal(seal.PurposeClient, clientBlob{RedirectURIs: req.RedirectURIs, ClientName: name, Iat: now.Unix()}, now.Add(clientTTL))
	if err != nil {
		registrationError(w, "invalid_client_metadata", "client metadata too large")
		return
	}
	s.log.Info("client registered", "client", clientHash(id)[:12])
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        now.Unix(),
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

func registrationError(w http.ResponseWriter, code, desc string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
}

// resolveClient resolves a DCR (sealed) or CIMD (https URL) client_id.
func (s *Server) resolveClient(ctx context.Context, clientID string) (*Client, error) {
	switch {
	case strings.HasPrefix(clientID, seal.Prefix):
		var b clientBlob
		if err := s.opts.Sealer.Open(seal.PurposeClient, clientID, &b); err != nil {
			return nil, errUnknownClient
		}
		return &Client{ID: clientID, Name: b.ClientName, RedirectURIs: b.RedirectURIs, Kind: "dcr"}, nil
	case strings.HasPrefix(clientID, "https://"):
		doc, err := s.cimd.Fetch(ctx, clientID)
		if err != nil {
			s.log.Warn("client metadata document rejected", "err", err)
			return nil, errUnknownClient
		}
		return &Client{ID: clientID, Name: doc.ClientName, RedirectURIs: doc.RedirectURIs, Kind: "cimd"}, nil
	default:
		return nil, errUnknownClient
	}
}
