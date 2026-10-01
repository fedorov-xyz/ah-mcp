package tools

import (
	"context"
	"runtime/debug"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

func (d *Deps) whoamiTool() server.ServerTool {
	tool := mcp.NewTool("ah_whoami",
		mcp.WithDescription("Show which Albert Heijn account this connection is signed in with: name, email, masked bonus card number and postal code. "+
			"Use it to confirm the account before changing the shopping list or order. The connection signs in via OAuth; there is no login/logout tool."),
		readOnly("Albert Heijn: Who Am I"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		m, err := c.c.GetMember(ctx)
		if err != nil {
			return d.upstream(c, "get member", err), nil
		}
		name := strings.TrimSpace(m.FirstName + " " + m.LastName)
		postal, valid := util.NormalizePostalCode(m.Address.PostalCode)
		if !valid {
			postal = strings.TrimSpace(m.Address.PostalCode)
		}
		data := map[string]any{
			"name":              name,
			"email":             m.Email,
			"bonus_card_number": util.MaskTail(m.BonusCardNumber, 4),
			"postal_code":       postal,
		}
		return ok("Signed in as "+name+".", data), nil
	}}
}

func (d *Deps) serverInfoTool() server.ServerTool {
	tool := mcp.NewTool("ah_server_info",
		mcp.WithDescription("Report the ah-mcp server version, the appie-go library version, the auth mode (always oauth), whether an email allowlist is enabled, cache statistics and uptime. "+
			"Use it when debugging; it does not call Albert Heijn."),
		readOnly("Albert Heijn: Server Info"),
	)
	return server.ServerTool{Tool: tool, Handler: func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		data := map[string]any{
			"version":           d.Version,
			"commit":            d.Commit,
			"appie_go_version":  depVersion("github.com/gwillem/appie-go"),
			"mcp_go_version":    depVersion("github.com/mark3labs/mcp-go"),
			"auth_mode":         "oauth",
			"allowlist_enabled": d.AllowlistEnabled,
			"cache":             d.Cache.Stats(),
			"uptime_seconds":    int64(d.now().Sub(d.Started) / time.Second),
			"site":              "nl",
		}
		return ok("ah-mcp "+d.Version+".", data), nil
	}}
}

// depVersion returns the version of a module dependency from build info.
func depVersion(path string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == path {
			if dep.Replace != nil {
				return dep.Replace.Version
			}
			return dep.Version
		}
	}
	return "unknown"
}
