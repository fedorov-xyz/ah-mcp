package oauth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/logx"
	"github.com/fedorov-xyz/ah-mcp/internal/oauth/seal"
	"github.com/fedorov-xyz/ah-mcp/internal/session"
)

// AuthError describes why a resource request was rejected.
type AuthError struct {
	Status int
	// Code is the RFC 6750 error code ("" when no credentials were sent).
	Code string
	Desc string
}

func (e *AuthError) Error() string { return e.Code + ": " + e.Desc }

// Authenticate validates the bearer access token on r: it must unseal with
// the access purpose, carry the right audience, be unexpired, wrap an AH
// token that is still valid, and belong to an allowed email.
func (s *Server) Authenticate(r *http.Request) (session.Session, *AuthError) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if h == "" || !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
		return session.Session{}, &AuthError{Status: http.StatusUnauthorized}
	}
	var c TokenClaims
	if err := s.opts.Sealer.Open(seal.PurposeAccess, strings.TrimSpace(tok), &c); err != nil {
		desc := "the access token is invalid"
		if errors.Is(err, seal.ErrExpired) {
			desc = "the access token expired"
		}
		return session.Session{}, &AuthError{Status: http.StatusUnauthorized, Code: "invalid_token", Desc: desc}
	}
	if c.Kind != kindAccess || c.Aud != s.Resource() || c.AHAccess == "" || c.MemberID == "" {
		return session.Session{}, &AuthError{Status: http.StatusUnauthorized, Code: "invalid_token", Desc: "the access token is not valid for this resource"}
	}
	ahExp := time.Unix(c.AHExpiresAt, 0)
	// Never refresh AH here: the rotated AH refresh token could not be
	// handed back to the client. Make the client refresh at /oauth/token.
	if !s.now().Before(ahExp) {
		return session.Session{}, &AuthError{Status: http.StatusUnauthorized, Code: "invalid_token", Desc: "the Albert Heijn session expired; refresh the token"}
	}
	if !s.opts.EmailAllowed(c.Email) {
		s.log.Warn("request denied by allowlist", "member", logx.MemberHash(c.MemberID))
		return session.Session{}, &AuthError{Status: http.StatusForbidden, Code: "insufficient_scope", Desc: "this Albert Heijn account is not allowed on this server"}
	}
	return session.Session{
		MemberID:      c.MemberID,
		Email:         c.Email,
		AHAccessToken: c.AHAccess,
		AHExpiresAt:   ahExp,
		ClientHash:    c.ClientIDHash,
	}, nil
}

// WriteChallenge writes the 401/403 response for e, including the RFC 9728
// resource_metadata pointer.
func (s *Server) WriteChallenge(w http.ResponseWriter, e *AuthError) {
	v := fmt.Sprintf(`Bearer resource_metadata="%s"`, s.ResourceMetadataURL())
	if e.Code != "" {
		v += fmt.Sprintf(`, error="%s", error_description="%s"`, e.Code, strings.ReplaceAll(e.Desc, `"`, "'"))
	}
	w.Header().Set("WWW-Authenticate", v)
	w.Header().Set("Cache-Control", "no-store")
	status := e.Status
	if status == 0 {
		status = http.StatusUnauthorized
	}
	body := map[string]string{"error": e.Code, "error_description": e.Desc}
	if e.Code == "" {
		body = map[string]string{"error": "unauthorized", "error_description": "sign in with Albert Heijn via OAuth"}
	}
	writeJSON(w, status, body)
}
