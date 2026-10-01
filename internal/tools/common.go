// Package tools implements the 25 ah-mcp MCP tools. register.go lists them.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	appie "github.com/gwillem/appie-go"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/fedorov-xyz/ah-mcp/internal/ah"
	"github.com/fedorov-xyz/ah-mcp/internal/cache"
	"github.com/fedorov-xyz/ah-mcp/internal/logx"
	"github.com/fedorov-xyz/ah-mcp/internal/session"
)

// Deps are the shared dependencies of every tool.
type Deps struct {
	AH               *ah.Factory
	Cache            *cache.Cache
	Version          string
	Commit           string
	AllowlistEnabled bool
	Started          time.Time
	Logger           *slog.Logger
	// Now is the clock (Europe/Amsterdam dates are derived from it).
	Now func() time.Time
}

func (d *Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

var amsterdam = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		return time.FixedZone("CET", 3600)
	}
	return loc
}()

// today returns the current date in the Netherlands.
func (d *Deps) today() time.Time {
	t := d.now().In(amsterdam)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, amsterdam)
}

const day = "2006-01-02"

// call is the per-request context of a tool handler.
type call struct {
	ctx  context.Context
	sess session.Session
	c    *appie.Client
	// ns is the cache namespace for this member's personal data.
	ns string
}

// begin resolves the authenticated session. Auth problems are handled by
// the HTTP layer, so a missing session is an internal error.
func (d *Deps) begin(ctx context.Context) (*call, *mcp.CallToolResult) {
	sess, ok := session.From(ctx)
	if !ok || sess.AHAccessToken == "" {
		return nil, toolError(codeUpstream, "no authenticated session on this request", nil)
	}
	return &call{ctx: ctx, sess: sess, c: d.AH.Client(sess), ns: cache.Member(sess.MemberID)}, nil
}

// Error codes.
const (
	codeConfirm     = "confirm_required"
	codeNoOrder     = "no_active_order"
	codeInvalid     = "invalid_argument"
	codeNotFound    = "not_found"
	codeRateLimited = "upstream_rate_limited"
	codeUpstream    = "upstream_error"
)

var hints = map[string]string{
	codeConfirm:     `Show the preview to the user and call again with confirm="yes" only after they agree.`,
	codeNoOrder:     "Create an order/slot in the AH app first; until then use the shopping list tools.",
	codeInvalid:     "Fix the arguments as described and call the tool again.",
	codeNotFound:    "Check the id (use a search or list tool to get a valid one) and try again.",
	codeRateLimited: "Albert Heijn is rate limiting requests; wait a minute before retrying.",
	codeUpstream:    "Albert Heijn returned an error; retry later. If it keeps failing, remove and re-add the connector.",
}

// toolError builds an isError result with a code, message, hint and
// optional extra structured fields (e.g. a preview).
func toolError(code, msg string, extra map[string]any) *mcp.CallToolResult {
	data := map[string]any{"error": code, "message": msg, "hint": hints[code]}
	for k, v := range extra {
		data[k] = v
	}
	text := code + ": " + msg + "\nNext: " + hints[code]
	if len(extra) > 0 {
		if b, err := json.Marshal(data); err == nil {
			text += "\n" + string(b)
		}
	}
	res := mcp.NewToolResultStructured(data, text)
	res.IsError = true
	return res
}

func invalid(msg string) *mcp.CallToolResult { return toolError(codeInvalid, msg, nil) }

// upstream maps an AH error to a tool error. The raw error is logged
// (redacted) but never returned, since AH error bodies can contain
// personal data.
func (d *Deps) upstream(c *call, what string, err error) *mcp.CallToolResult {
	if d.Logger != nil {
		// A 404 means the caller asked for something that does not exist
		// (a wrong id): expected, not a server problem.
		level := slog.LevelWarn
		if ah.StatusOf(err) == http.StatusNotFound {
			level = slog.LevelInfo
		}
		d.Logger.Log(c.ctx, level, "AH call failed", "op", what, "status", ah.StatusOf(err), "member", logx.MemberHash(c.sess.MemberID), "err", err)
	}
	switch {
	case errors.Is(err, ah.ErrRateLimited):
		return toolError(codeRateLimited, what+": Albert Heijn rate limit reached", nil)
	case ah.StatusOf(err) == http.StatusNotFound:
		return toolError(codeNotFound, what+": Albert Heijn says it does not exist", nil)
	case ah.StatusOf(err) == http.StatusUnauthorized || ah.StatusOf(err) == http.StatusForbidden:
		return toolError(codeUpstream, what+": Albert Heijn rejected the session (reconnect the connector if this persists)", nil)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return toolError(codeUpstream, what+": request timed out", nil)
	default:
		return toolError(codeUpstream, what+" failed", nil)
	}
}

// ok returns structured content plus a text block holding a one-line
// summary and the compact JSON (for clients that only read text).
func ok(summary string, data any) *mcp.CallToolResult {
	b, err := json.Marshal(data)
	if err != nil {
		return mcp.NewToolResultError("internal error: " + err.Error())
	}
	return mcp.NewToolResultStructured(data, summary+"\n"+string(b))
}

// bind decodes the arguments into dst, rejecting wrong types.
func bind(req mcp.CallToolRequest, dst any) *mcp.CallToolResult {
	raw := req.GetRawArguments()
	if raw == nil {
		return nil
	}
	if m, isMap := raw.(map[string]any); isMap && len(m) == 0 {
		return nil
	}
	if err := req.BindArguments(dst); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return invalid("argument " + te.Field + " must be of type " + te.Type.String())
		}
		return invalid("arguments must be a JSON object matching the tool schema")
	}
	return nil
}

// Limits.
const (
	defaultLimit = 20
	maxLimit     = 100
)

func limitOf(v *int) (int, *mcp.CallToolResult) {
	if v == nil {
		return defaultLimit, nil
	}
	if *v < 1 || *v > maxLimit {
		return 0, invalid("limit must be between 1 and 100")
	}
	return *v, nil
}

// annotations for a tool. openWorldHint is always true: every tool talks to
// Albert Heijn.
func annotations(title string, readOnly, destructive, idempotent bool) mcp.ToolOption {
	return mcp.WithToolAnnotation(mcp.ToolAnnotation{
		Title:           title,
		ReadOnlyHint:    mcp.ToBoolPtr(readOnly),
		DestructiveHint: mcp.ToBoolPtr(destructive),
		IdempotentHint:  mcp.ToBoolPtr(idempotent),
		OpenWorldHint:   mcp.ToBoolPtr(true),
	})
}

func readOnly(title string) mcp.ToolOption { return annotations(title, true, false, true) }

func limitParam() mcp.ToolOption {
	return mcp.WithInteger("limit", mcp.Description("Maximum number of results (default 20, max 100)."), mcp.Min(1), mcp.Max(100))
}

func confirmParam(desc string) mcp.ToolOption {
	return mcp.WithString("confirm", mcp.Description(desc))
}

// afterCartMutation drops member caches derived from the active order:
// frequent items (and the habit matches built on them) count open
// (upcoming) orders, whose contents a cart mutation may have changed.
func (d *Deps) afterCartMutation(c *call) {
	d.Cache.Invalidate(c.ns, "frequent:")
}
