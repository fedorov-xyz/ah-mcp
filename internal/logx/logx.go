// Package logx provides structured logging with secret redaction.
//
// Nothing that grants access (our tokens, AH tokens, authorization codes,
// PKCE verifiers, cookies, login-session ids) and no email address may reach
// the logs. Callers log a member-id hash instead of identities; the
// redacting handler is a safety net for anything that slips through.
package logx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"regexp"
)

const redacted = "[REDACTED]"

var patterns = []*regexp.Regexp{
	// Authorization headers and bearer values.
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`),
	// Our sealed tokens and client ids.
	regexp.MustCompile(`\bahm1\.[A-Za-z0-9_-]+`),
	// JWT-shaped strings (AH tokens).
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]*)?`),
	// Query/form/JSON parameters carrying secrets.
	regexp.MustCompile(`(?i)\b(code|code_verifier|refresh_token|access_token|token|refreshToken|accessToken|client_secret|password|state)(["']?\s*[=:]\s*["']?)[^&\s"',;}]+`),
	// Cookie headers.
	regexp.MustCompile(`(?i)\b(set-cookie|cookie)(\s*[=:]\s*)[^\n]+`),
	// Login proxy session ids in paths.
	regexp.MustCompile(`/oauth/ah/[A-Za-z0-9_-]+`),
	// Email addresses.
	regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`),
}

// Redact masks secrets and email addresses in s.
func Redact(s string) string {
	for i, re := range patterns {
		switch i {
		case 3, 4:
			s = re.ReplaceAllString(s, "${1}${2}"+redacted)
		case 5:
			s = re.ReplaceAllString(s, "/oauth/ah/"+redacted)
		default:
			s = re.ReplaceAllString(s, redacted)
		}
	}
	return s
}

// MemberHash returns a short stable pseudonym for an AH member id, safe to
// log.
func MemberHash(memberID string) string {
	sum := sha256.Sum256([]byte("ah-mcp/member/" + memberID))
	return hex.EncodeToString(sum[:6])
}

// New returns a JSON slog logger writing to w that redacts every string
// attribute and the message.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(NewRedactingHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})))
}

// RedactingHandler wraps a slog.Handler and redacts message and attributes.
type RedactingHandler struct{ next slog.Handler }

// NewRedactingHandler wraps next.
func NewRedactingHandler(next slog.Handler) *RedactingHandler { return &RedactingHandler{next: next} }

// Enabled implements slog.Handler.
func (h *RedactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

// WithAttrs implements slog.Handler.
func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redactAttr(a)
	}
	return &RedactingHandler{next: h.next.WithAttrs(red)}
}

// WithGroup implements slog.Handler.
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{next: h.next.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindGroup:
		attrs := v.Group()
		out := make([]any, len(attrs))
		for i, g := range attrs {
			out[i] = redactAttr(g)
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, Redact(err.Error()))
		}
		return slog.String(a.Key, Redact(v.String()))
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}
