package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/fedorov-xyz/ah-mcp/internal/ah"
	"github.com/fedorov-xyz/ah-mcp/internal/cache"
	"github.com/fedorov-xyz/ah-mcp/internal/session"
	"github.com/fedorov-xyz/ah-mcp/internal/testutil/fakeah"
)

type harness struct {
	t    *testing.T
	fake *fakeah.Fake
	d    *Deps
	ctx  context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := fakeah.New(t)
	access, _ := fake.IssueTokens()
	d := &Deps{
		AH: &ah.Factory{APIBase: fake.URL(), HTTP: &http.Client{Transport: &ah.RetryTransport{
			Sleep: func(context.Context, time.Duration) error { return nil },
		}}},
		Cache:   cache.New(1000),
		Version: "test",
		Started: time.Now().Add(-time.Minute),
		Logger:  slog.New(slog.DiscardHandler),
	}
	return &harness{t: t, fake: fake, d: d, ctx: sessionCtx(fakeah.MemberID, access)}
}

func sessionCtx(memberID, access string) context.Context {
	return session.With(context.Background(), session.Session{
		MemberID: memberID, Email: "test.user@example.com", AHAccessToken: access, AHExpiresAt: time.Now().Add(time.Hour),
	})
}

func (h *harness) tool(name string) mcp.Tool {
	for _, st := range h.d.All() {
		if st.Tool.Name == name {
			return st.Tool
		}
	}
	h.t.Fatalf("no tool %s", name)
	return mcp.Tool{}
}

// call invokes a tool and returns the result and its structured content
// as a generic map.
func (h *harness) callCtx(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	h.t.Helper()
	for _, st := range h.d.All() {
		if st.Tool.Name != name {
			continue
		}
		req := mcp.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = args
		res, err := st.Handler(ctx, req)
		if err != nil {
			h.t.Fatalf("%s returned Go error: %v", name, err)
		}
		b, _ := json.Marshal(res.StructuredContent)
		var data map[string]any
		_ = json.Unmarshal(b, &data)
		if len(res.Content) == 0 {
			h.t.Fatalf("%s: no text content", name)
		}
		return res, data
	}
	h.t.Fatalf("no tool %s", name)
	return nil, nil
}

func (h *harness) call(name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	h.t.Helper()
	return h.callCtx(h.ctx, name, args)
}

func (h *harness) ok(name string, args map[string]any) map[string]any {
	h.t.Helper()
	res, data := h.call(name, args)
	if res.IsError {
		h.t.Fatalf("%s(%v) failed: %v", name, args, data)
	}
	return data
}

func (h *harness) fails(name string, args map[string]any, code string) map[string]any {
	h.t.Helper()
	res, data := h.call(name, args)
	if !res.IsError || data["error"] != code {
		h.t.Fatalf("%s(%v): want error %s, got isError=%v %v", name, args, code, res.IsError, data)
	}
	if data["hint"] == "" || data["hint"] == nil {
		h.t.Fatalf("%s: error without hint", name)
	}
	return data
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

var expectedTools = []string{
	"ah_whoami", "ah_server_info",
	"ah_search_products", "ah_get_product", "ah_search_stores", "ah_last_chance",
	"ah_bonus_offers", "ah_bonus_group", "ah_personal_bonus", "ah_bonus_for_habits", "ah_savings_summary",
	"ah_get_shopping_list", "ah_add_to_shopping_list", "ah_remove_from_shopping_list", "ah_clear_shopping_list",
	"ah_get_cart", "ah_update_cart_item", "ah_clear_cart", "ah_shopping_list_to_cart", "ah_find_duplicates",
	"ah_frequent_items", "ah_purchase_history", "ah_get_orders", "ah_get_order_details", "ah_get_receipts",
}

func TestRegistryIsExactlyThe25Tools(t *testing.T) {
	h := newHarness(t)
	var names []string
	numeric := map[string]bool{"limit": true, "product_id": true, "quantity": true, "max_price": true, "days": true,
		"max_orders": true, "order_id": true, "store_id": true, "min_purchases": true}
	arrays := map[string]bool{"items": true, "notes": true, "refs": true, "product_ids": true}
	for _, st := range h.d.All() {
		tool := st.Tool
		names = append(names, tool.Name)
		a := tool.Annotations
		if a.ReadOnlyHint == nil || a.DestructiveHint == nil || a.IdempotentHint == nil || a.OpenWorldHint == nil || !*a.OpenWorldHint || a.Title == "" {
			t.Errorf("%s: incomplete annotations %+v", tool.Name, a)
		}
		if *a.ReadOnlyHint && *a.DestructiveHint {
			t.Errorf("%s: read-only and destructive", tool.Name)
		}
		if len(tool.Description) < 60 {
			t.Errorf("%s: description too short", tool.Name)
		}
		for pname, raw := range tool.InputSchema.Properties {
			p, _ := raw.(map[string]any)
			switch {
			case numeric[pname]:
				if p["type"] != "integer" && p["type"] != "number" {
					t.Errorf("%s.%s: type %v, want integer/number", tool.Name, pname, p["type"])
				}
			case arrays[pname]:
				if p["type"] != "array" || p["items"] == nil {
					t.Errorf("%s.%s: want a typed array, got %v", tool.Name, pname, p)
				}
			}
		}
	}
	if fmt.Sprint(names) != fmt.Sprint(expectedTools) {
		t.Fatalf("tools =\n%v\nwant\n%v", names, expectedTools)
	}
	for _, name := range []string{"ah_clear_shopping_list", "ah_clear_cart", "ah_update_cart_item", "ah_remove_from_shopping_list"} {
		if !*h.tool(name).Annotations.DestructiveHint {
			t.Errorf("%s must be marked destructive", name)
		}
	}
}

func TestREADMECatalogueMatchesRegistry(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	start := strings.Index(s, "## Tool catalogue")
	if start < 0 {
		t.Fatal("README has no '## Tool catalogue' section")
	}
	section := s[start+3:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	re := regexp.MustCompile("(?m)^\\| *`(ah_[a-z_]+)`")
	var got []string
	for _, m := range re.FindAllStringSubmatch(section, -1) {
		got = append(got, m[1])
	}
	want := append([]string(nil), expectedTools...)
	sort.Strings(got)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("README catalogue =\n%v\nregistry =\n%v", got, want)
	}
}

// Out-of-scope AH operations must stay unreachable from the code base.
func TestOutOfScopeOperationsUnreachable(t *testing.T) {
	forbidden := []string{"ReopenOrder(", "RevertOrder(", "orderReopen", "orderRevert", "CheckShoppingListItem(", "favoriteList",
		"AddToFavoriteList(", "RemoveFromFavoriteList(", "choose-and-activate", "ah_login", "ah_logout", "NewSSEServer", "NewStdioServer"}
	for _, root := range []string{"../../internal", "../../cmd"} {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, _ := os.ReadFile(p)
			for _, f := range forbidden {
				if strings.Contains(string(b), f) {
					t.Errorf("%s references out-of-scope %q", p, f)
				}
			}
			return nil
		})
	}
}

func TestSystemTools(t *testing.T) {
	h := newHarness(t)
	who := h.ok("ah_whoami", nil)
	if who["bonus_card_number"] != "*********0017" || who["email"] != "test.user@example.com" || who["postal_code"] != "1011AA" || who["name"] != "Test Gebruiker" {
		t.Fatalf("whoami = %v", who)
	}
	info := h.ok("ah_server_info", nil)
	if info["auth_mode"] != "oauth" || info["allowlist_enabled"] != false || info["version"] != "test" || info["uptime_seconds"].(float64) < 59 {
		t.Fatalf("server_info = %v", info)
	}
	if _, hasCache := info["cache"]; !hasCache {
		t.Fatal("server_info without cache stats")
	}
}

func TestProductTools(t *testing.T) {
	h := newHarness(t)
	res := h.ok("ah_search_products", map[string]any{"query": "melk", "limit": 2})
	if len(list(res["products"])) != 2 || res["truncated"] != true || res["currency"] != "EUR" {
		t.Fatalf("search = %v", res)
	}
	p0 := list(res["products"])[0].(map[string]any)
	if _, leaked := p0["isPreviouslyBought"]; leaked || p0["price"] != 1.19 {
		t.Fatalf("product view = %v", p0)
	}
	res = h.ok("ah_search_products", map[string]any{"query": "melk", "bonus": true})
	for _, p := range list(res["products"]) {
		if p.(map[string]any)["is_bonus"] != true {
			t.Fatalf("bonus filter: %v", p)
		}
	}
	res = h.ok("ah_search_products", map[string]any{"query": "melk", "max_price": 1.0})
	if n := len(list(res["products"])); n != 1 {
		t.Fatalf("max_price filter returned %d", n)
	}
	// Search results are cached globally.
	before := h.fake.Ops("search")
	h.ok("ah_search_products", map[string]any{"query": "MELK", "limit": 2})
	if h.fake.Ops("search") != before {
		t.Fatal("search not cached")
	}

	prod := h.ok("ah_get_product", map[string]any{"product_id": 100001, "include_nutrition": true})
	if pm := prod["product"].(map[string]any); pm["title"] != "Testmerk Halfvolle melk" || len(list(pm["nutrition"])) != 1 {
		t.Fatalf("get_product = %v", prod)
	}
	h.fails("ah_get_product", map[string]any{"product_id": 999}, codeNotFound)
	h.fails("ah_get_product", map[string]any{"product_id": "abc"}, codeInvalid)
	h.fails("ah_search_products", map[string]any{"query": "x", "limit": 0}, codeInvalid)
	h.fails("ah_search_products", map[string]any{"query": "x", "limit": 101}, codeInvalid)
	h.fails("ah_search_products", map[string]any{"query": "  "}, codeInvalid)

	stores := h.ok("ah_search_stores", nil)
	if stores["postal_code"] != "1011AA" || len(list(stores["stores"])) != 1 {
		t.Fatalf("stores fallback = %v", stores)
	}
	h.fails("ah_search_stores", map[string]any{"postal_code": "nope"}, codeInvalid)
	lc := h.ok("ah_last_chance", map[string]any{"postal_code": "1011 aa"})
	b := list(lc["bargains"])[0].(map[string]any)
	if lc["store_id"].(float64) != 1234 || b["price_now"] != 1.62 || b["price_was"] != 2.49 {
		t.Fatalf("last_chance = %v", lc)
	}
	h.fails("ah_last_chance", nil, codeInvalid)
}

func TestErrorMapping(t *testing.T) {
	h := newHarness(t)
	h.fake.RateLimit.Store(10)
	data := h.fails("ah_get_shopping_list", nil, codeRateLimited)
	if strings.Contains(fmt.Sprint(data), "too many requests") {
		t.Fatal("upstream body leaked into tool error")
	}
	h.fake.RateLimit.Store(0)
	h.fake.ServerErrors.Store(10)
	h.fails("ah_get_shopping_list", nil, codeUpstream)
	h.fake.ServerErrors.Store(0)
	// A transient 503 is retried transparently for reads.
	h.fake.ServerErrors.Store(1)
	h.ok("ah_get_shopping_list", nil)
	// Without a session the tool refuses (never happens behind the auth middleware).
	res, _ := h.callCtx(context.Background(), "ah_whoami", nil)
	if !res.IsError {
		t.Fatal("tool ran without session")
	}
	// Tool output never contains the AH token.
	sess, _ := session.From(h.ctx)
	for _, name := range []string{"ah_whoami", "ah_get_cart", "ah_server_info"} {
		res, _ := h.call(name, nil)
		b, _ := json.Marshal(res)
		if strings.Contains(string(b), sess.AHAccessToken) {
			t.Fatalf("%s leaked the AH token", name)
		}
	}
}

func TestBasketNeverCached(t *testing.T) {
	h := newHarness(t)
	h.ok("ah_get_shopping_list", nil)
	h.ok("ah_get_shopping_list", nil)
	h.ok("ah_get_cart", nil)
	if n := h.fake.Ops("basket"); n != 3 {
		t.Fatalf("basket fetched %d times, want 3 (never cached)", n)
	}
}

func TestPerMemberCacheIsolation(t *testing.T) {
	h := newHarness(t)
	h.ok("ah_personal_bonus", nil)
	h.ok("ah_personal_bonus", nil)
	if n := h.fake.Ops("bonus_personal"); n != 1 {
		t.Fatalf("personal bonus not cached per member: %d", n)
	}
	access, _ := h.fake.IssueTokens()
	h.callCtx(sessionCtx("9999", access), "ah_personal_bonus", nil)
	if n := h.fake.Ops("bonus_personal"); n != 2 {
		t.Fatalf("member B was served member A's personal bonus (fetches=%d)", n)
	}
	// National offers are shared.
	h.ok("ah_bonus_offers", nil)
	sections := h.fake.Ops("bonus_section")
	h.callCtx(sessionCtx("9999", access), "ah_bonus_offers", nil)
	if h.fake.Ops("bonus_section") != sections {
		t.Fatal("global bonus offers not shared across members")
	}
}

func TestNoAHRefreshDuringToolCalls(t *testing.T) {
	h := newHarness(t)
	for _, name := range expectedTools {
		args := map[string]any{}
		switch name {
		case "ah_search_products":
			args["query"] = "melk"
		case "ah_get_product":
			args["product_id"] = 100001
		case "ah_last_chance":
			args["store_id"] = 1234
		case "ah_bonus_group":
			args["segment_id"] = "SEG-1"
		case "ah_add_to_shopping_list":
			args["notes"] = []any{map[string]any{"text": "kaas"}}
		case "ah_remove_from_shopping_list":
			args["notes"] = []any{"kaas"}
		case "ah_update_cart_item":
			args["product_id"], args["quantity"] = 100001, 3
		case "ah_find_duplicates":
			args["scope"] = "list"
		case "ah_get_order_details":
			args["order_id"] = 9001
		}
		h.call(name, args)
	}
	if n := h.fake.RefreshCalls.Load(); n != 0 {
		t.Fatalf("AH refresh called %d times during tool calls", n)
	}
}

func TestShoppingListTools(t *testing.T) {
	h := newHarness(t)
	l := h.ok("ah_get_shopping_list", nil)
	items := list(l["items"])
	if len(items) != 3 || l["has_active_order"] != false {
		t.Fatalf("list = %v", l)
	}
	refs := []string{}
	for _, it := range items {
		refs = append(refs, it.(map[string]any)["ref"].(string))
	}
	if fmt.Sprint(refs) != "[product:100001 product:200002 note:0]" {
		t.Fatalf("refs = %v", refs)
	}

	add := h.ok("ah_add_to_shopping_list", map[string]any{
		"items": []any{map[string]any{"product_id": 300001, "quantity": 1}, map[string]any{"product_id": 300001}},
		"notes": []any{map[string]any{"text": "eieren", "quantity": 2}},
	})
	if add["added_products"].(float64) != 1 || add["added_notes"].(float64) != 1 {
		t.Fatalf("add = %v", add)
	}
	lst, notes := h.fake.List()
	if lst[len(lst)-1].ID != 300001 || lst[len(lst)-1].Quantity != 2 || notes[len(notes)-1].Text != "eieren" {
		t.Fatalf("fake list after add: %+v %+v", lst, notes)
	}
	h.fails("ah_add_to_shopping_list", nil, codeInvalid)
	h.fails("ah_add_to_shopping_list", map[string]any{"items": "[{\"product_id\":1}]"}, codeInvalid) // no JSON-in-string
	h.fails("ah_add_to_shopping_list", map[string]any{"items": []any{map[string]any{"product_id": 1, "quantity": 500}}}, codeInvalid)

	rm := h.ok("ah_remove_from_shopping_list", map[string]any{"refs": []any{"note:0", "product:200002", "product:42"}})
	if len(list(rm["removed"])) != 2 || fmt.Sprint(rm["not_found"]) != "[product:42]" || len(list(rm["still_present"])) != 0 {
		t.Fatalf("remove = %v", rm)
	}
	lst, notes = h.fake.List()
	for _, it := range lst {
		if it.ID == 200002 {
			t.Fatal("product not removed")
		}
	}
	for _, n := range notes {
		if n.Text == "brood" {
			t.Fatal("note not removed")
		}
	}
	h.ok("ah_remove_from_shopping_list", map[string]any{"product_ids": []any{300001}, "notes": []any{"EIEREN"}})
	h.fails("ah_remove_from_shopping_list", map[string]any{"refs": []any{"product:1"}}, codeNotFound)
	h.fails("ah_remove_from_shopping_list", nil, codeInvalid)
}

func TestConfirmGuards(t *testing.T) {
	h := newHarness(t)
	// 1. clear_shopping_list
	data := h.fails("ah_clear_shopping_list", nil, codeConfirm)
	if len(list(data["would_remove"])) != 3 {
		t.Fatalf("preview = %v", data)
	}
	if lst, notes := h.fake.List(); len(lst) != 2 || len(notes) != 1 {
		t.Fatal("list changed without confirm")
	}
	h.fails("ah_clear_shopping_list", map[string]any{"confirm": "no"}, codeConfirm)
	cleared := h.ok("ah_clear_shopping_list", map[string]any{"confirm": "yes"})
	if lst, notes := h.fake.List(); len(lst) != 0 || len(notes) != 0 || len(list(cleared["removed"])) != 3 {
		t.Fatalf("not cleared: %v %v %v", lst, notes, cleared)
	}

	// 2. clear_cart in shopping_list mode = clear_shopping_list
	h = newHarness(t)
	data = h.fails("ah_clear_cart", nil, codeConfirm)
	if data["mode"] != modeList || len(list(data["would_remove"])) != 3 {
		t.Fatalf("clear_cart list preview = %v", data)
	}
	h.ok("ah_clear_cart", map[string]any{"confirm": "YES"})
	if lst, notes := h.fake.List(); len(lst) != 0 || len(notes) != 0 {
		t.Fatal("clear_cart did not clear the shopping list")
	}

	// 3. clear_cart in active_order mode
	h = newHarness(t)
	h.fake.SetActiveOrder(true, map[int]int{100001: 2, 200001: 1})
	data = h.fails("ah_clear_cart", nil, codeConfirm)
	if data["mode"] != modeOrder || data["order_id"] != "7777" || len(list(data["would_remove"])) != 2 {
		t.Fatalf("clear_cart order preview = %v", data)
	}
	if len(h.fake.Order()) != 2 {
		t.Fatal("order changed without confirm")
	}
	h.ok("ah_clear_cart", map[string]any{"confirm": "yes"})
	if len(h.fake.Order()) != 0 {
		t.Fatalf("order not cleared: %v", h.fake.Order())
	}
	if lst, _ := h.fake.List(); len(lst) != 2 {
		t.Fatal("clearing the order touched the shopping list")
	}

	// 4. update_cart_item in active_order mode
	h.fake.SetActiveOrder(true, map[int]int{100001: 2})
	data = h.fails("ah_update_cart_item", map[string]any{"product_id": 100001, "quantity": 5}, codeConfirm)
	if p := data["preview"].(map[string]any); p["current_quantity"].(float64) != 2 || p["new_quantity"].(float64) != 5 {
		t.Fatalf("update preview = %v", data)
	}
	if h.fake.Order()[100001] != 2 {
		t.Fatal("order changed without confirm")
	}
	up := h.ok("ah_update_cart_item", map[string]any{"product_id": 100001, "quantity": 5, "confirm": "yes"})
	if h.fake.Order()[100001] != 5 || up["mode"] != modeOrder {
		t.Fatalf("update = %v order=%v", up, h.fake.Order())
	}
}

func TestCartModeDetection(t *testing.T) {
	h := newHarness(t)
	c := h.ok("ah_get_cart", nil)
	if c["mode"] != modeList || c["has_active_order"] != false || len(list(c["items"])) != 2 || c["order_id"] != nil {
		t.Fatalf("list-mode cart = %v", c)
	}
	if tot := c["totals"].(map[string]any); tot["total"].(float64) != 4.67 {
		t.Fatalf("totals = %v", tot)
	}
	h.fake.SetActiveOrder(true, map[int]int{100001: 1})
	c = h.ok("ah_get_cart", nil)
	if c["mode"] != modeOrder || c["has_active_order"] != true || c["order_id"] != "7777" || c["delivery_slot"] == nil {
		t.Fatalf("order-mode cart = %v", c)
	}
	if tot := c["totals"].(map[string]any); tot["total"].(float64) != 1.5 || tot["discount"].(float64) != 0.5 || tot["subtotal"].(float64) != 2 {
		t.Fatalf("order totals = %v", tot)
	}
}

func TestUpdateCartItemShoppingListMode(t *testing.T) {
	h := newHarness(t)
	// No confirm needed without an active order; near-duplicates reported.
	up := h.ok("ah_update_cart_item", map[string]any{"product_id": 200001, "quantity": 1})
	if up["mode"] != modeList {
		t.Fatalf("mode = %v", up["mode"])
	}
	lst, _ := h.fake.List()
	if lst[len(lst)-1].ID != 200001 {
		t.Fatal("product not added to the basket")
	}
	// The fake names unknown products generically, so give it a lasagne
	// title through the order path instead: check via find_duplicates.
	h.ok("ah_update_cart_item", map[string]any{"product_id": 100001, "quantity": 0})
	if lst, _ := h.fake.List(); len(lst) != 2 {
		t.Fatalf("quantity 0 did not remove: %v", lst)
	}
	h.fails("ah_update_cart_item", map[string]any{"product_id": 555, "quantity": 0}, codeNotFound)
	h.fails("ah_update_cart_item", map[string]any{"product_id": 1}, codeInvalid)

	h.fake.SetActiveOrder(true, map[int]int{200002: 1})
	up = h.ok("ah_update_cart_item", map[string]any{"product_id": 200001, "quantity": 1, "confirm": "yes"})
	dups := list(up["near_duplicates"])
	if len(dups) != 1 {
		t.Fatalf("near_duplicates = %v", up["near_duplicates"])
	}
}

func TestShoppingListToCart(t *testing.T) {
	h := newHarness(t)
	h.fails("ah_shopping_list_to_cart", map[string]any{"confirm": "yes"}, codeNoOrder)
	if h.fake.Ops("order_put") != 0 {
		t.Fatal("order written without an active order")
	}
	h.fake.SetActiveOrder(true, map[int]int{100001: 5})
	data := h.fails("ah_shopping_list_to_cart", nil, codeConfirm)
	if fmt.Sprint(data["skipped_notes"]) != "[brood]" || len(list(data["changes"])) != 1 {
		t.Fatalf("preview = %v", data)
	}
	h.ok("ah_shopping_list_to_cart", map[string]any{"confirm": "yes"})
	o := h.fake.Order()
	if o[100001] != 5 || o[200002] != 1 {
		t.Fatalf("order = %v (quantities must never be lowered)", o)
	}
}

func TestFindDuplicates(t *testing.T) {
	h := newHarness(t)
	h.fails("ah_find_duplicates", map[string]any{"scope": "order"}, codeInvalid)
	h.fails("ah_find_duplicates", map[string]any{"scope": "everything"}, codeInvalid)
	d := h.ok("ah_find_duplicates", map[string]any{"scope": "order", "order_id": 9001})
	if len(list(d["duplicates"])) != 0 || d["items_checked"].(float64) != 3 {
		t.Fatalf("order dups = %v", d)
	}
	h.fake.SetActiveOrder(true, map[int]int{200001: 1, 200002: 1, 100001: 1})
	d = h.ok("ah_find_duplicates", map[string]any{"scope": "cart"})
	if len(list(d["duplicates"])) != 1 || d["mode"] != modeOrder {
		t.Fatalf("cart dups = %v", d)
	}
	d = h.ok("ah_find_duplicates", map[string]any{"scope": "list"})
	if d["items_checked"].(float64) != 3 {
		t.Fatalf("list dups = %v", d)
	}
}

func TestInvalidationAfterMutation(t *testing.T) {
	h := newHarness(t)
	h.fake.SetActiveOrder(true, map[int]int{100001: 1})
	h.ok("ah_frequent_items", nil)
	n := h.fake.Ops("closed_orders")
	h.ok("ah_frequent_items", nil)
	if h.fake.Ops("closed_orders") != n {
		t.Fatal("frequent items not cached")
	}
	h.ok("ah_update_cart_item", map[string]any{"product_id": 100001, "quantity": 2, "confirm": "yes"})
	h.ok("ah_frequent_items", nil)
	if h.fake.Ops("closed_orders") != n+1 {
		t.Fatal("frequent items cache not invalidated after an order mutation")
	}
}

func TestBonusTools(t *testing.T) {
	h := newHarness(t)
	cur := h.ok("ah_bonus_offers", nil)
	offers := list(cur["offers"])
	if len(offers) != 3 || cur["period_start"] == nil || cur["available"] != true {
		t.Fatalf("bonus offers = %v", cur)
	}
	milk := offers[0].(map[string]any)
	if milk["bonus_price"] != 1.05 || milk["discount_pct"] != 25.0 || milk["category"] != "Zuivel" || len(list(milk["tiers"])) != 1 {
		t.Fatalf("tiered offer = %v", milk)
	}
	group := offers[2].(map[string]any)
	if group["segment_id"] != "SEG-1" || group["bonus_price"] != 1.0 || group["discount_pct"] != 50.0 {
		t.Fatalf("group offer = %v", group)
	}
	// Conditions (deduplicated exceptionRule + extraDescriptions) and scope.
	if fmt.Sprint(group["conditions"]) != "[*M.u.v. Biologisch Korting wordt verrekend bij afrekenen]" || group["store_only"] != true || group["stapel_bonus"] != nil {
		t.Fatalf("group conditions = %v store_only=%v", group["conditions"], group["store_only"])
	}
	if milk["store_only"] != nil || milk["conditions"] != nil {
		t.Fatalf("plain offer got conditions: %v", milk)
	}
	next := h.ok("ah_bonus_offers", map[string]any{"period": "next", "keyword": "LASAGNE"})
	if len(list(next["offers"])) != 1 || next["period_start"] == cur["period_start"] {
		t.Fatalf("next/keyword = %v", next)
	}
	h.fails("ah_bonus_offers", map[string]any{"period": "later"}, codeInvalid)

	g := h.ok("ah_bonus_group", map[string]any{"segment_id": "SEG-1"})
	if p := list(g["products"])[0].(map[string]any); p["product_id"].(float64) != 300001 || p["bonus_price"] != 1.0 {
		t.Fatalf("bonus group = %v", g)
	}
	h.ok("ah_bonus_group", map[string]any{"segment_id": "SEG-1", "period": "next"})

	pb := h.ok("ah_personal_bonus", nil)
	counts := pb["status_counts"].(map[string]any)
	if counts["ACTIVATABLE"].(float64) != 1 || counts["ACTIVATED"].(float64) != 1 || pb["read_only"] != true {
		t.Fatalf("personal bonus = %v", pb)
	}

	habits := h.ok("ah_bonus_for_habits", nil)
	matches := list(habits["matches"])
	if len(matches) < 2 {
		t.Fatalf("habits = %v", habits)
	}
	first := matches[0].(map[string]any)
	if first["match_type"] != "product_id" || first["product_id"].(float64) != 100002 {
		t.Fatalf("first match = %v", first)
	}
	var titleMatch bool
	for _, m := range matches {
		mm := m.(map[string]any)
		if mm["product_id"].(float64) == 200002 && mm["match_type"] == "title" {
			titleMatch = true
		}
	}
	if !titleMatch {
		t.Fatalf("lasagne title match missing: %v", matches)
	}
}

func TestHistoryTools(t *testing.T) {
	h := newHarness(t)
	f := h.ok("ah_frequent_items", map[string]any{"max_orders": 10})
	items := list(f["items"])
	if f["orders_analyzed"].(float64) != 2 || len(items) != 3 || items[0].(map[string]any)["order_count"].(float64) != 2 {
		t.Fatalf("frequent = %v", f)
	}
	h.fails("ah_frequent_items", map[string]any{"max_orders": 101}, codeInvalid)

	ph := h.ok("ah_purchase_history", map[string]any{"days": 30})
	if cov := ph["coverage"].(map[string]any); cov["receipts_analyzed"].(float64) != 1 || cov["online_orders_analyzed"].(float64) != 2 || cov["complete"] != true {
		t.Fatalf("history = %v", ph)
	}
	months := list(ph["by_month"])
	var receipts, orders float64
	for _, m := range months {
		receipts += m.(map[string]any)["receipts"].(float64)
		orders += m.(map[string]any)["online_orders"].(float64)
	}
	if receipts != 1 || orders != 2 {
		t.Fatalf("by_month = %v", months)
	}
	// totals = sum of by_month = what was paid (receipt and order totals).
	var spend float64
	for _, m := range months {
		spend += m.(map[string]any)["total_spend"].(float64)
	}
	tot := ph["totals"].(map[string]any)
	if math.Abs(tot["total"].(float64)-spend) > 0.001 || tot["online_orders"] != 80.1 || tot["receipts"] != 12.34 {
		t.Fatalf("totals = %v, by_month spend = %v", tot, spend)
	}
	// A complete result is cached.
	pages := h.fake.Ops("receipts")
	h.ok("ah_purchase_history", map[string]any{"days": 30})
	if h.fake.Ops("receipts") != pages {
		t.Fatal("complete purchase history not cached")
	}
	top := list(ph["products"])[0].(map[string]any)
	if top["product_id"].(float64) != 100001 || top["purchases"].(float64) != 3 || len(list(top["sources"])) != 2 {
		t.Fatalf("merged top product = %v", top)
	}

	up := h.ok("ah_get_orders", nil)
	if len(list(up["orders"])) != 0 || up["scope"] != "upcoming" {
		t.Fatalf("upcoming = %v", up)
	}
	past := h.ok("ah_get_orders", map[string]any{"scope": "past", "limit": 1})
	if len(list(past["orders"])) != 1 || past["truncated"] != true {
		t.Fatalf("past = %v", past)
	}
	od := h.ok("ah_get_order_details", map[string]any{"order_id": 9001})
	if od["total"] != 42.1 || od["total_source"] != "fulfillment" || len(list(od["items"])) != 3 {
		t.Fatalf("order details = %v", od)
	}
	// Lines: 2×1.19 + 0.99 + 1×2.29 = 5.66; the rest is reported, not hidden.
	if od["lines_total"] != 5.66 || od["other_charges"] != 36.44 || od["other_charges_note"] == nil {
		t.Fatalf("order charges = %v / %v", od["lines_total"], od["other_charges"])
	}

	rl := h.ok("ah_get_receipts", nil)
	if len(list(rl["receipts"])) != 2 {
		t.Fatalf("receipts = %v", rl)
	}
	rd := h.ok("ah_get_receipts", map[string]any{"receipt_id": "RCPT-1"})
	if rd["total_paid"] != 12.34 || rd["total_discount"] != 1.5 || list(rd["items"])[0].(map[string]any)["product_id"].(float64) != 100001 {
		t.Fatalf("receipt = %v", rd)
	}

	today := time.Now()
	sv := h.ok("ah_savings_summary", map[string]any{"from": today.AddDate(0, 0, -10).Format(day), "to": today.Format(day)})
	src := sv["sources"].(map[string]any)
	if sv["total_discount"] != 4.7 || src["receipts"].(map[string]any)["status"] != "available" || src["online_orders"].(map[string]any)["status"] != "available" {
		t.Fatalf("savings = %v", sv)
	}
	if cat := sv["discount_by_category"].(map[string]any); cat["bonus"] != 0.5 || cat["miles"] != 1.0 || cat["online_orders"] != 3.2 {
		t.Fatalf("savings categories = %v", cat)
	}
	h.fails("ah_savings_summary", map[string]any{"from": "2026-13-01"}, codeInvalid)
	h.fails("ah_savings_summary", map[string]any{"from": "2026-10-02", "to": "2026-10-01"}, codeInvalid)
	h.ok("ah_savings_summary", nil)
}

// Regression: capped receipt detail fetching must be reported, not silent.
func TestPurchaseHistoryReportsCaps(t *testing.T) {
	h := newHarness(t)
	h.fake.ExtraReceipts = ah.MaxHistoryReceipts + 50
	res, data := h.call("ah_purchase_history", map[string]any{"days": 365})
	cov := data["coverage"].(map[string]any)
	if cov["complete"] != false || cov["receipt_list_complete"] != true ||
		cov["receipts_in_window"].(float64) != float64(ah.MaxHistoryReceipts+51) ||
		cov["receipts_analyzed"].(float64) != float64(ah.MaxHistoryReceipts) ||
		cov["receipts_skipped_over_cap"].(float64) != 51 {
		t.Fatalf("coverage = %v", cov)
	}
	if text := res.Content[0].(mcp.TextContent).Text; !strings.Contains(text, "INCOMPLETE") {
		t.Fatalf("summary does not flag the cap: %s", text)
	}
	// Paging went past appie-go's single page of 100.
	pages := h.fake.Ops("receipts")
	if pages < 3 {
		t.Fatalf("receipt pages fetched = %d", pages)
	}
	// by_month counts every receipt in the window, not only the capped ones.
	sum := 0.0
	for _, m := range list(data["by_month"]) {
		sum += m.(map[string]any)["receipts"].(float64)
	}
	if sum != cov["receipts_in_window"].(float64) {
		t.Fatalf("by_month receipts = %v, want %v", sum, cov["receipts_in_window"])
	}
	// An incomplete result is not cached: the next call fetches again.
	h.call("ah_purchase_history", map[string]any{"days": 365})
	if h.fake.Ops("receipts") != 2*pages {
		t.Fatal("incomplete purchase history was served from cache")
	}
}

// Regression: delivered orders that AH still lists as open, and orders
// without a slot date, must count as purchases (ah_frequent_items sees them).
func TestPurchaseHistoryIncludesOpenAndUndatedOrders(t *testing.T) {
	h := newHarness(t)
	h.fake.OpenDelivered, h.fake.OpenUndated = true, true
	ph := h.ok("ah_purchase_history", map[string]any{"days": 30})
	cov := ph["coverage"].(map[string]any)
	if cov["online_orders_analyzed"].(float64) != 4 || cov["complete"] != true {
		t.Fatalf("coverage = %v", cov)
	}
	top := list(ph["products"])[0].(map[string]any)
	if top["product_id"].(float64) != 100001 || top["purchases"].(float64) != 5 {
		t.Fatalf("top = %v", top)
	}
	freq := h.ok("ah_frequent_items", nil)
	if freq["orders_analyzed"].(float64) != 4 {
		t.Fatalf("frequent items saw %v orders", freq["orders_analyzed"])
	}
}

func TestNotFoundIsLoggedAtInfo(t *testing.T) {
	h := newHarness(t)
	var buf strings.Builder
	h.d.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.fails("ah_get_product", map[string]any{"product_id": 999}, codeNotFound)
	if !strings.Contains(buf.String(), "level=INFO") || strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("404 log level: %s", buf.String())
	}
	buf.Reset()
	h.fake.ServerErrors.Store(10)
	h.fails("ah_get_shopping_list", nil, codeUpstream)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("5xx not logged as warning: %s", buf.String())
	}
}

func TestProductNutritionAndStoreNameDetails(t *testing.T) {
	h := newHarness(t)
	// The fake product has nutrition; a missing table must be stated, not
	// silently dropped.
	p := h.ok("ah_get_product", map[string]any{"product_id": 100001, "include_nutrition": true})
	if p["nutrition_available"] != true {
		t.Fatalf("nutrition_available = %v", p["nutrition_available"])
	}
	// Regression (live): the detail endpoint has no Nutri-Score; it comes
	// from the search result for the product's title.
	if ns := p["product"].(map[string]any)["nutri_score"]; ns != "B" {
		t.Fatalf("nutri_score = %v", ns)
	}
	// The lookup is cached per product, across include_nutrition variants.
	searches := h.fake.Ops("search")
	h.ok("ah_get_product", map[string]any{"product_id": 100001})
	h.ok("ah_get_product", map[string]any{"product_id": 100001, "include_nutrition": true})
	if got := h.fake.Ops("search"); got != searches || searches != 1 {
		t.Fatalf("nutri-score searches = %d then %d, want 1", searches, got)
	}
	if p := h.ok("ah_get_product", map[string]any{"product_id": 100001}); p["nutrition_available"] != nil {
		t.Fatal("nutrition_available reported without include_nutrition")
	}
	// store_id near the member's address resolves to the store name.
	lc := h.ok("ah_last_chance", map[string]any{"store_id": 1234})
	if lc["store_name"] != "AH Voorbeeldstraat" {
		t.Fatalf("store_name = %v", lc["store_name"])
	}
	// Unknown store: the field is omitted, not an empty string.
	lc = h.ok("ah_last_chance", map[string]any{"store_id": 4321})
	if _, ok := lc["store_name"]; ok {
		t.Fatalf("store_name present for an unknown store: %v", lc["store_name"])
	}
}

// Regression (real AH data): online bundle discounts come with an empty
// bonusMechanism; the promotion text is in discountLabels.
func TestSearchUsesDiscountLabelsWhenMechanismIsEmpty(t *testing.T) {
	h := newHarness(t)
	res := h.ok("ah_search_products", map[string]any{"query": "melk", "bonus": true})
	var bundle map[string]any
	for _, p := range list(res["products"]) {
		if p.(map[string]any)["product_id"].(float64) == 609662 {
			bundle = p.(map[string]any)
		}
	}
	if bundle == nil {
		t.Fatalf("bundle product missing: %v", res["products"])
	}
	period, _ := bundle["bonus_period"].(map[string]any)
	if bundle["bonus_mechanism"] != "pakketkorting" || bundle["online_only_bonus"] != true || period["end"] != "2026-10-04" {
		t.Fatalf("bundle = %v", bundle)
	}
	if bundle["discount_pct"] != 16.5 {
		t.Fatalf("discount_pct = %v", bundle["discount_pct"])
	}
	// "pakketkorting" carries no price data, so no tiers are invented.
	if bundle["tiers"] != nil {
		t.Fatalf("bundle tiers = %v", bundle["tiers"])
	}
	// A regular bonus keeps its own mechanism and is not marked online-only;
	// its placeholder end date (2999-12-31) becomes open_ended.
	for _, p := range list(res["products"]) {
		m := p.(map[string]any)
		if m["product_id"].(float64) != 100002 {
			continue
		}
		// "2e halve prijs" on €1.39: 2 pieces at €1.04, 25% off.
		tiers := list(m["tiers"])
		if len(tiers) != 1 || tiers[0].(map[string]any)["discount_pct"] != 25.0 || tiers[0].(map[string]any)["count"] != 2.0 {
			t.Fatalf("tiers = %v", m["tiers"])
		}
		per, _ := m["bonus_period"].(map[string]any)
		if m["bonus_mechanism"] != "2e halve prijs" || m["online_only_bonus"] != nil || per["open_ended"] != true || per["end"] != nil || per["start"] != "2026-09-28" {
			t.Fatalf("regular bonus = %v", m)
		}
	}
}

// Regression: an activated personal offer is real saving; an activatable
// one is reported separately.
func TestHabitsSavingCountsActivatedPersonalOffers(t *testing.T) {
	items := []ah.HistoryProduct{{ProductID: 1, Name: "Tasty Tom", Purchases: 3}, {ProductID: 2, Name: "Karnemelk", Purchases: 3}}
	personal := []ah.Offer{
		{ProductID: 1, Title: "Tasty Tom", BasePrice: 2.45, BonusPrice: 2.00, ActivationStatus: "ACTIVATED"},
		{ProductID: 2, Title: "Karnemelk", BasePrice: 2.49, BonusPrice: 1.99, ActivationStatus: "ACTIVATABLE"},
	}
	matches := matchHabits(items, []ah.Offer{{ProductID: 9, Title: "Brood"}}, personal, 2)
	if len(matches) != 2 {
		t.Fatalf("matches = %+v", matches)
	}
	saving, ifActivated := habitSavings(append(matches,
		habitMatch{MatchType: "product_id", SavingPerItem: 1.00}, // national offer
		habitMatch{MatchType: "title", SavingPerItem: 5.00}))     // title match: never counted
	if saving != 1.45 || ifActivated != 0.5 {
		t.Fatalf("saving=%v ifActivated=%v", saving, ifActivated)
	}
}

func TestReceiptNotFoundAndFlatGroupPrices(t *testing.T) {
	h := newHarness(t)
	h.fails("ah_get_receipts", map[string]any{"receipt_id": "nope"}, codeNotFound)
	// "1+1 gratis" keeps the per-piece price: no misleading equal bonus price.
	g := h.ok("ah_bonus_group", map[string]any{"segment_id": "SEG-1", "period": "next"})
	for _, p := range list(g["products"]) {
		m := p.(map[string]any)
		if m["product_id"].(float64) == 300002 && m["bonus_price"] != nil {
			t.Fatalf("flat bonus price shown: %v", m)
		}
		if m["product_id"].(float64) == 300001 && m["bonus_price"] != 1.0 {
			t.Fatalf("real bonus price lost: %v", m)
		}
	}
}

// A draft order (slot chosen in the app, not placed) is edited directly,
// like in the app; a placed (scheduled) order needs confirm. Clearing always
// needs confirm.
func TestDraftOrderEditsWithoutConfirm(t *testing.T) {
	h := newHarness(t)
	if l := h.ok("ah_get_shopping_list", nil); l["note"] != nil {
		t.Fatalf("note without an active order: %v", l["note"])
	}
	if o := h.ok("ah_get_orders", nil); o["note"] != nil {
		t.Fatalf("draft note without an order: %v", o["note"])
	}
	h.fake.SetDraftOrder(map[int]int{100001: 1})
	// A draft order is not a placed (upcoming) order, but is pointed to.
	if o := h.ok("ah_get_orders", nil); o["count"].(float64) != 0 || o["note"] != draftOrderNote {
		t.Fatalf("upcoming with a draft order = %v", o)
	}
	c := h.ok("ah_get_cart", nil)
	slot, _ := c["delivery_slot"].(map[string]any)
	if c["order_status"] != "draft" || slot == nil || slot["window"] != "18:00 - 20:00" {
		t.Fatalf("draft cart = %v", c)
	}
	if it := list(c["items"])[0].(map[string]any); it["price"] != 1.5 || it["line_total"] != 1.5 {
		t.Fatalf("active-order item prices = %v", it)
	}
	// The shopping list points to the order instead of looking empty.
	if l := h.ok("ah_get_shopping_list", nil); l["has_active_order"] != true || l["note"] != activeOrderListNote {
		t.Fatalf("list with active order = %v", l)
	}
	h.ok("ah_update_cart_item", map[string]any{"product_id": 100001, "quantity": 3})
	if h.fake.Order()[100001] != 3 {
		t.Fatalf("draft update not applied: %v", h.fake.Order())
	}
	// Adding through the shopping list goes into the order; notes are skipped.
	add := h.ok("ah_add_to_shopping_list", map[string]any{
		"items": []any{map[string]any{"product_id": 200001, "quantity": 2}},
		"notes": []any{map[string]any{"text": "brood"}},
	})
	if add["mode"] != modeOrder || fmt.Sprint(add["skipped_notes"]) != "[brood]" || h.fake.Order()[200001] != 2 {
		t.Fatalf("add to draft order = %v, order = %v", add, h.fake.Order())
	}
	h.fails("ah_add_to_shopping_list", map[string]any{"notes": []any{map[string]any{"text": "kaas"}}}, codeInvalid)
	h.fails("ah_clear_cart", nil, codeConfirm)

	// Placed order: the same edits need confirm.
	h.fake.SetActiveOrder(true, map[int]int{100001: 1})
	if c := h.ok("ah_get_cart", nil); c["order_status"] != "scheduled" {
		t.Fatalf("scheduled cart = %v", c)
	}
	h.fails("ah_update_cart_item", map[string]any{"product_id": 100001, "quantity": 3}, codeConfirm)
	h.fails("ah_add_to_shopping_list", map[string]any{"items": []any{map[string]any{"product_id": 200001}}}, codeConfirm)
	if h.fake.Order()[200001] != 0 {
		t.Fatal("placed order changed without confirm")
	}
	h.ok("ah_add_to_shopping_list", map[string]any{"items": []any{map[string]any{"product_id": 200001}}, "confirm": "yes"})
	if h.fake.Order()[200001] != 1 {
		t.Fatalf("confirmed add not applied: %v", h.fake.Order())
	}
}

// Regression (live): the receipt list was cut at appie-go's first page of
// 100 and reported truncated:false with 170 receipts on the account.
func TestReceiptListPagesPastFirst100(t *testing.T) {
	h := newHarness(t)
	h.fake.ExtraReceipts = 150
	rl := h.ok("ah_get_receipts", map[string]any{"limit": 100})
	if rl["count"].(float64) != 100 || rl["truncated"] != true {
		t.Fatalf("limit 100: count=%v truncated=%v", rl["count"], rl["truncated"])
	}
	if h.fake.Ops("receipts") < 2 {
		t.Fatal("second page not fetched")
	}
	// All receipts fit: not truncated.
	h2 := newHarness(t)
	rl = h2.ok("ah_get_receipts", map[string]any{"limit": 100})
	if rl["truncated"] != false || rl["count"].(float64) == 0 {
		t.Fatalf("small account: %v", rl)
	}
	rl = h2.ok("ah_get_receipts", map[string]any{"limit": 1})
	if rl["count"].(float64) != 1 || rl["truncated"] != true {
		t.Fatalf("limit 1: %v", rl)
	}
}

// Regression (live): habits came from online orders only and missed short
// group titles; in-store products (even without a webshop id) count, and an
// offer whose whole title is in the product's matches.
func TestHabitsUseReceiptsAndShortOfferTitles(t *testing.T) {
	items := []ah.HistoryProduct{
		{Name: "AH Kipgehakt", Purchases: 4, Sources: []string{"receipt"}},
		{ProductID: 7, Name: "Zanetti Grana Padano geraspt", Purchases: 2, Sources: []string{"receipt", "online_order"}},
		{ProductID: 8, Name: "Zanetti Parmigiano", Purchases: 1}, // below min_purchases
	}
	offers := []ah.Offer{
		{SegmentID: "G1", Title: "AH Kipgehakt", BasePrice: 4.99, BonusPrice: 3.99},
		{SegmentID: "G2", Title: "Zanetti", BasePrice: 4.29, BonusPrice: 3.22},
		{SegmentID: "G3", Title: "Zanetti Mozzarella"}, // brand overlap only
	}
	m := matchHabits(items, offers, nil, 2)
	if len(m) != 2 {
		t.Fatalf("matches = %+v", m)
	}
	if m[0].Name != "AH Kipgehakt" || m[0].Offer.SegmentID != "G1" || m[0].MatchType != "title" || m[0].Purchases != 4 || m[0].Sources[0] != "receipt" {
		t.Fatalf("kipgehakt = %+v", m[0])
	}
	if m[1].Offer.SegmentID != "G2" || fmt.Sprint(m[1].MatchedOn) != "[ZANETTI]" {
		t.Fatalf("zanetti = %+v", m[1])
	}
}
