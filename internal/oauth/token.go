package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/util"

	"github.com/fedorov-xyz/ah-mcp/internal/ahlogin"
	"github.com/fedorov-xyz/ah-mcp/internal/logx"
	"github.com/fedorov-xyz/ah-mcp/internal/oauth/seal"
)

// Token kinds.
const (
	kindAccess  = "access"
	kindRefresh = "refresh"
)

// TokenClaims is the payload sealed into access and refresh tokens. Access
// tokens carry only the AH access token and refresh tokens only the AH
// refresh token, so an access token can never be used to refresh AH.
type TokenClaims struct {
	V            int    `json:"v"`
	Kind         string `json:"kind"`
	Aud          string `json:"aud"`
	ClientIDHash string `json:"client_id_hash"`
	MemberID     string `json:"member_id"`
	Email        string `json:"email"`
	AHAccess     string `json:"ah_access,omitempty"`
	AHRefresh    string `json:"ah_refresh,omitempty"`
	AHExpiresAt  int64  `json:"ah_expires_at,omitempty"`
	Iat          int64  `json:"iat"`
	Exp          int64  `json:"exp"`
}

// TokenResponse is the RFC 6749 §5.1 success body.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

type tokenError struct {
	status int
	code   string
	desc   string
}

func (e *tokenError) Error() string { return e.code + ": " + e.desc }

func errGrant(desc string) *tokenError {
	return &tokenError{http.StatusBadRequest, "invalid_grant", desc}
}
func errRequest(desc string) *tokenError {
	return &tokenError{http.StatusBadRequest, "invalid_request", desc}
}

func writeTokenError(w http.ResponseWriter, e *tokenError) {
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, e.status, map[string]string{"error": e.code, "error_description": e.desc})
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, errRequest("body must be application/x-www-form-urlencoded"))
		return
	}
	if r.Form.Get("client_secret") != "" {
		writeTokenError(w, &tokenError{http.StatusUnauthorized, "invalid_client", "only public clients are supported"})
		return
	}
	var (
		resp *TokenResponse
		terr *tokenError
	)
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		resp, terr = s.exchangeCode(r)
	case "refresh_token":
		resp, terr = s.refresh(r.Context(), r)
	default:
		terr = &tokenError{http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token"}
	}
	if terr != nil {
		writeTokenError(w, terr)
		return
	}
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) exchangeCode(r *http.Request) (*TokenResponse, *tokenError) {
	f := r.PostForm
	code, verifier, clientID := f.Get("code"), f.Get("code_verifier"), f.Get("client_id")
	if code == "" || verifier == "" || clientID == "" {
		return nil, errRequest("code, code_verifier and client_id are required")
	}
	// Single use: the code is removed before any check, so a replay always
	// fails even if this attempt does.
	ac, ok := s.codes.take(code)
	if !ok {
		return nil, errGrant("authorization code is invalid, expired or already used")
	}
	if subtle.ConstantTimeCompare([]byte(ac.clientID), []byte(clientID)) != 1 {
		return nil, errGrant("code was issued to another client")
	}
	if f.Get("redirect_uri") != ac.redirectURI {
		return nil, errGrant("redirect_uri does not match the authorization request")
	}
	if !VerifyPKCE(verifier, ac.challenge) {
		return nil, errGrant("PKCE verification failed")
	}
	if res := f.Get("resource"); res != "" && util.CanonicalURL(res) != s.Resource() {
		return nil, &tokenError{http.StatusBadRequest, "invalid_target", "resource must be " + s.Resource()}
	}
	if !s.opts.EmailAllowed(ac.member.Email) {
		return nil, errGrant("account is not allowed on this server")
	}
	resp, err := s.issue(clientHash(ac.clientID), ac.member.ID, ac.member.Email, ac.ah)
	if err != nil {
		return nil, errGrant(err.Error())
	}
	return resp, nil
}

// VerifyPKCE checks an S256 code_verifier against its challenge (RFC 7636).
func VerifyPKCE(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, c := range verifier {
		if !util.IsURLUnreserved(c) {
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(challenge)) == 1
}

func (s *Server) refresh(ctx context.Context, r *http.Request) (*TokenResponse, *tokenError) {
	f := r.PostForm
	rt := f.Get("refresh_token")
	if rt == "" {
		return nil, errRequest("refresh_token is required")
	}
	var c TokenClaims
	if err := s.opts.Sealer.Open(seal.PurposeRefresh, rt, &c); err != nil {
		return nil, errGrant("refresh token is invalid or expired; reconnect")
	}
	if c.Kind != kindRefresh || c.Aud != s.Resource() || c.AHRefresh == "" {
		return nil, errGrant("refresh token is not valid for this server")
	}
	if id := f.Get("client_id"); id != "" && clientHash(id) != c.ClientIDHash {
		return nil, errGrant("refresh token was issued to another client")
	}
	if res := f.Get("resource"); res != "" && util.CanonicalURL(res) != s.Resource() {
		return nil, &tokenError{http.StatusBadRequest, "invalid_target", "resource must be " + s.Resource()}
	}
	if !s.opts.EmailAllowed(c.Email) {
		s.log.Warn("refresh denied by allowlist", "member", logx.MemberHash(c.MemberID))
		return nil, errGrant("account is no longer allowed on this server")
	}
	resp, err := s.refreshes.do(rt, func() (*TokenResponse, error) {
		tokens, err := s.opts.AH.Refresh(ctx, c.AHRefresh)
		if err != nil {
			return nil, err
		}
		return s.issue(c.ClientIDHash, c.MemberID, c.Email, tokens)
	})
	if err != nil {
		s.log.Info("AH refresh failed; client must reconnect", "member", logx.MemberHash(c.MemberID))
		return nil, errGrant("Albert Heijn session expired; reconnect")
	}
	return resp, nil
}

// issue seals a new access/refresh pair around AH tokens.
func (s *Server) issue(cidHash, memberID, email string, ah ahlogin.Tokens) (*TokenResponse, error) {
	now := s.now()
	accessExp := ah.ExpiresAt.Add(-AHExpiryMargin)
	if max := now.Add(MaxAccessTTL); accessExp.After(max) {
		accessExp = max
	}
	if !accessExp.After(now) {
		return nil, errors.New("the Albert Heijn token expires too soon to be issued")
	}
	base := TokenClaims{V: 1, Aud: s.Resource(), ClientIDHash: cidHash, MemberID: memberID, Email: email, Iat: now.Unix()}

	access := base
	access.Kind, access.AHAccess, access.AHExpiresAt, access.Exp = kindAccess, ah.AccessToken, ah.ExpiresAt.Unix(), accessExp.Unix()
	at, err := s.opts.Sealer.Seal(seal.PurposeAccess, access, accessExp)
	if err != nil {
		return nil, err
	}
	refreshExp := now.Add(s.opts.SessionTTL)
	refresh := base
	refresh.Kind, refresh.AHRefresh, refresh.Exp = kindRefresh, ah.RefreshToken, refreshExp.Unix()
	rt, err := s.opts.Sealer.Seal(seal.PurposeRefresh, refresh, refreshExp)
	if err != nil {
		return nil, err
	}
	return &TokenResponse{AccessToken: at, TokenType: "Bearer", ExpiresIn: int64(accessExp.Sub(now).Seconds()), RefreshToken: rt}, nil
}

// refreshGroup collapses concurrent refreshes of the same refresh token and
// replays the result for a short window. AH rotates refresh tokens, so
// without this two parallel refreshes would race and the loser would be
// logged out (same idea as Jelle7/ah-mcp's refreshMu).
type refreshGroup struct {
	mu    sync.Mutex
	calls map[string]*refreshCall
	now   func() time.Time
}

type refreshCall struct {
	done chan struct{}
	resp *TokenResponse
	err  error
	at   time.Time
}

func newRefreshGroup(now func() time.Time) *refreshGroup {
	return &refreshGroup{calls: map[string]*refreshCall{}, now: now}
}

func (g *refreshGroup) do(token string, fn func() (*TokenResponse, error)) (*TokenResponse, error) {
	sum := sha256.Sum256([]byte(token))
	key := string(sum[:])
	g.mu.Lock()
	for k, c := range g.calls {
		select {
		case <-c.done:
			if g.now().Sub(c.at) > refreshReuse {
				delete(g.calls, k)
			}
		default:
		}
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.resp, c.err
	}
	c := &refreshCall{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	c.resp, c.err = fn()
	c.at = g.now()
	close(c.done)
	if c.err != nil {
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
	}
	return c.resp, c.err
}
