//go:build integration

// Read-only checks against the real Albert Heijn API. Not run in CI.
//
//	AH_INTEGRATION_ACCESS_TOKEN=<AH access token> go test -tags integration ./internal/tools/ -run Integration -v
//
// Obtain an access token by signing in to a local ah-mcp with debug tooling of
// your own; never commit it.
package tools

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/ah"
	"github.com/fedorov-xyz/ah-mcp/internal/cache"
)

func TestIntegrationReadOnlyTools(t *testing.T) {
	token := os.Getenv("AH_INTEGRATION_ACCESS_TOKEN")
	if token == "" {
		t.Skip("AH_INTEGRATION_ACCESS_TOKEN not set")
	}
	h := &harness{t: t, d: &Deps{
		AH: ah.NewFactory(""), Cache: cache.New(100), Version: "integration", Started: time.Now(),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}, ctx: sessionCtx("integration", token)}

	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"ah_whoami", nil},
		{"ah_search_products", map[string]any{"query": "melk", "limit": 3}},
		{"ah_get_product", map[string]any{"product_id": 1525}},
		{"ah_search_stores", nil},
		{"ah_bonus_offers", map[string]any{"limit": 5}},
		{"ah_personal_bonus", nil},
		{"ah_get_shopping_list", nil},
		{"ah_get_cart", nil},
		{"ah_get_orders", map[string]any{"scope": "past", "limit": 3}},
		{"ah_get_receipts", map[string]any{"limit": 3}},
		{"ah_frequent_items", map[string]any{"max_orders": 5}},
	} {
		res, data := h.call(c.name, c.args)
		if res.IsError {
			t.Errorf("%s: %v", c.name, data)
		}
	}
}
