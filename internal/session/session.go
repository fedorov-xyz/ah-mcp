// Package session carries the authenticated caller of an MCP request through
// the request context.
package session

import (
	"context"
	"time"
)

// Session is the identity unsealed from a valid access token.
type Session struct {
	MemberID string
	Email    string
	// AHAccessToken is the upstream AH access token. It is never logged and
	// never returned by a tool.
	AHAccessToken string
	AHExpiresAt   time.Time
	ClientHash    string
}

type ctxKey struct{}

// With returns a copy of ctx carrying s.
func With(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// From returns the session stored in ctx.
func From(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(ctxKey{}).(Session)
	return s, ok
}
