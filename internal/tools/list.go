package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/fedorov-xyz/ah-mcp/internal/ah"
	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

const listNote = "Without an active online order the shopping list and the cart are the SAME basket — do not add items through both. "

func listView(b ah.Basket) map[string]any {
	entries := b.ListEntries()
	products, notes := 0, 0
	for i := range entries {
		entries[i].Price = util.Round2(entries[i].Price)
		if entries[i].Kind == "note" {
			notes++
		} else {
			products++
		}
	}
	v := map[string]any{
		"currency":         "EUR",
		"has_active_order": b.HasActiveOrder(),
		"product_count":    products,
		"note_count":       notes,
		"items":            entries,
	}
	if b.HasActiveOrder() {
		v["note"] = activeOrderListNote
	}
	return v
}

const activeOrderListNote = "An online order is active (a slot was chosen in the AH app): its products are in the order, not in this list, " +
	"and products added now go into the order. Use ah_get_cart to see the order."

func (d *Deps) getShoppingListTool() server.ServerTool {
	tool := mcp.NewTool("ah_get_shopping_list",
		mcp.WithDescription("Show the Albert Heijn shopping list (Boodschappenlijst): product items and free-text notes with a stable ref "+
			"(\"product:<id>\" or \"note:<index>\"), quantity and checked state. Use refs with ah_remove_from_shopping_list. "+
			listNote+"The shopping list is not an order: nothing is delivered until the user picks a slot in the AH app."),
		readOnly("Albert Heijn: Shopping List"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		b, err := ah.FetchBasket(ctx, c.c)
		if err != nil {
			return d.upstream(c, "get shopping list", err), nil
		}
		v := listView(b)
		summary := fmt.Sprintf("Shopping list: %d product(s), %d note(s).", v["product_count"], v["note_count"])
		if b.HasActiveOrder() {
			summary += " An order is active; see ah_get_cart for its products."
		}
		return ok(summary, v), nil
	}}
}

type productQty struct {
	ProductID int `json:"product_id"`
	Quantity  int `json:"quantity"`
}

type noteQty struct {
	Text     string `json:"text"`
	Quantity int    `json:"quantity"`
}

func (d *Deps) addToShoppingListTool() server.ServerTool {
	tool := mcp.NewTool("ah_add_to_shopping_list",
		mcp.WithDescription("Add products (by product_id from ah_search_products) and/or free-text notes to the Albert Heijn shopping list. "+
			listNote+"While an order is ACTIVE (a slot was chosen), AH adds products STRAIGHT INTO THE ORDER (quantities are added, not set) and rejects notes: "+
			"for a draft order (slot chosen, not placed) this happens directly; for a placed order (order_status=scheduled) it requires confirm=\"yes\". "+
			"The response field mode says where the products went."),
		mcp.WithArray("items", mcp.Description("Products to add."), mcp.MaxItems(50), mcp.Items(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"product_id": map[string]any{"type": "integer", "minimum": 1},
				"quantity":   map[string]any{"type": "integer", "minimum": 1, "maximum": 99, "default": 1},
			},
			"required": []string{"product_id"},
		})),
		mcp.WithArray("notes", mcp.Description("Free-text notes to add, e.g. {\"text\":\"brood\"}."), mcp.MaxItems(50), mcp.Items(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text":     map[string]any{"type": "string", "minLength": 1, "maxLength": 100},
				"quantity": map[string]any{"type": "integer", "minimum": 1, "maximum": 99, "default": 1},
			},
			"required": []string{"text"},
		})),
		confirmParam(`Must be "yes" to add to an already placed (scheduled) order.`),
		annotations("Albert Heijn: Add to Shopping List", false, false, false),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Items   []productQty `json:"items"`
			Notes   []noteQty    `json:"notes"`
			Confirm string       `json:"confirm"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		if len(a.Items) == 0 && len(a.Notes) == 0 {
			return invalid("pass items and/or notes"), nil
		}
		if len(a.Items) > 50 || len(a.Notes) > 50 {
			return invalid("at most 50 items and 50 notes per call"), nil
		}
		merged := map[int]int{}
		var order []int
		for _, it := range a.Items {
			if it.ProductID <= 0 {
				return invalid("every item needs a positive product_id"), nil
			}
			q := it.Quantity
			if q == 0 {
				q = 1
			}
			if q < 1 || q > 99 {
				return invalid("quantity must be between 1 and 99"), nil
			}
			if _, seen := merged[it.ProductID]; !seen {
				order = append(order, it.ProductID)
			}
			merged[it.ProductID] += q
		}
		notes := make([]ah.NotePatch, 0, len(a.Notes))
		for _, n := range a.Notes {
			text := strings.TrimSpace(n.Text)
			q := n.Quantity
			if q == 0 {
				q = 1
			}
			if text == "" || len(text) > 100 || q < 1 || q > 99 {
				return invalid("every note needs text (1-100 chars) and quantity 1-99"), nil
			}
			notes = append(notes, ah.NotePatch{Description: text, SearchTerm: text, Quantity: q})
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		items := make([]ah.BasketMutationItem, 0, len(order))
		for _, id := range order {
			items = append(items, ah.BasketMutationItem{ID: id, Quantity: merged[id]})
		}
		s, fail := d.loadCart(c)
		if fail != nil {
			return fail, nil
		}
		if s.mode == modeOrder {
			return d.addToActiveOrder(c, s, items, a.Notes, a.Confirm), nil
		}
		if len(items) > 0 {
			if _, err := ah.AddBasketProducts(ctx, c.c, items); err != nil {
				return d.upstream(c, "add to shopping list", err), nil
			}
		}
		if err := ah.PatchNotes(ctx, c.c, notes); err != nil {
			return d.upstream(c, "add notes to shopping list", err), nil
		}
		b, err := ah.FetchBasket(ctx, c.c)
		if err != nil {
			return d.upstream(c, "re-read shopping list", err), nil
		}
		v := listView(b)
		v["added_products"] = len(order)
		v["added_notes"] = len(notes)
		return ok(fmt.Sprintf("Added %d product(s) and %d note(s) to the shopping list.", len(order), len(notes)), v), nil
	}}
}

// removalPlan resolves what to remove from the basket list.
type removalPlan struct {
	products []ah.BasketMutationItem
	notes    []ah.NotePatch
	removed  []ah.ListEntry
	notFound []string
}

func planRemoval(b ah.Basket, refs []string, productIDs []int, noteTexts []string) removalPlan {
	var p removalPlan
	entries := b.ListEntries()
	byRef := map[string]ah.ListEntry{}
	for _, e := range entries {
		byRef[e.Ref] = e
	}
	seen := map[string]bool{}
	take := func(e ah.ListEntry) {
		if seen[e.Ref] {
			return
		}
		seen[e.Ref] = true
		p.removed = append(p.removed, e)
		if e.Kind == "note" {
			p.notes = append(p.notes, ah.NotePatch{Description: e.Name, Quantity: 0})
		} else {
			p.products = append(p.products, ah.BasketMutationItem{ID: e.ProductID, Quantity: 0})
		}
	}
	for _, r := range refs {
		if e, ok := byRef[strings.TrimSpace(r)]; ok {
			take(e)
		} else {
			p.notFound = append(p.notFound, r)
		}
	}
	for _, id := range productIDs {
		if e, ok := byRef["product:"+strconv.Itoa(id)]; ok {
			take(e)
		} else {
			p.notFound = append(p.notFound, "product:"+strconv.Itoa(id))
		}
	}
	for _, t := range noteTexts {
		found := false
		for _, e := range entries {
			if e.Kind == "note" && strings.EqualFold(strings.TrimSpace(e.Name), strings.TrimSpace(t)) {
				take(e)
				found = true
			}
		}
		if !found {
			p.notFound = append(p.notFound, "note:"+t)
		}
	}
	return p
}

// applyRemoval executes a plan and verifies the result by re-reading the
// basket (MaxTomahawk's verified removal: product items by id via
// basketItemsUpdate quantity 0, notes by description via the v2 PATCH).
func (d *Deps) applyRemoval(c *call, p removalPlan) (ah.Basket, []string, *mcp.CallToolResult) {
	if len(p.products) > 0 {
		if _, err := ah.UpdateBasketProducts(c.ctx, c.c, p.products); err != nil {
			return ah.Basket{}, nil, d.upstream(c, "remove products from shopping list", err)
		}
	}
	if err := ah.PatchNotes(c.ctx, c.c, p.notes); err != nil {
		return ah.Basket{}, nil, d.upstream(c, "remove notes from shopping list", err)
	}
	b, err := ah.FetchBasket(c.ctx, c.c)
	if err != nil {
		return ah.Basket{}, nil, d.upstream(c, "re-read shopping list", err)
	}
	var still []string
	after := b.ListEntries()
	for _, r := range p.removed {
		for _, e := range after {
			if (r.Kind == "product" && e.Kind == "product" && e.ProductID == r.ProductID) ||
				(r.Kind == "note" && e.Kind == "note" && strings.EqualFold(e.Name, r.Name)) {
				still = append(still, r.Ref)
				break
			}
		}
	}
	return b, still, nil
}

func (d *Deps) removeFromShoppingListTool() server.ServerTool {
	tool := mcp.NewTool("ah_remove_from_shopping_list",
		mcp.WithDescription("Remove specific entries from the Albert Heijn shopping list: by refs from ah_get_shopping_list (preferred), by product_ids, or by note texts. "+
			"The result is verified by re-reading the list (still_present lists anything AH did not remove). "+listNote+
			"Removes only what you name; to empty the list use ah_clear_shopping_list."),
		mcp.WithArray("refs", mcp.Description("Refs like \"product:123456\" or \"note:0\" from ah_get_shopping_list."), mcp.WithStringItems(), mcp.MaxItems(100)),
		mcp.WithArray("product_ids", mcp.Description("Product ids to remove."), mcp.WithIntegerItems(mcp.Min(1)), mcp.MaxItems(100)),
		mcp.WithArray("notes", mcp.Description("Free-text note texts to remove (case-insensitive exact match)."), mcp.WithStringItems(), mcp.MaxItems(100)),
		annotations("Albert Heijn: Remove from Shopping List", false, true, true),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Refs       []string `json:"refs"`
			ProductIDs []int    `json:"product_ids"`
			Notes      []string `json:"notes"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		if len(a.Refs)+len(a.ProductIDs)+len(a.Notes) == 0 {
			return invalid("pass refs, product_ids and/or notes"), nil
		}
		if len(a.Refs) > 100 || len(a.ProductIDs) > 100 || len(a.Notes) > 100 {
			return invalid("at most 100 entries per argument"), nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		b, err := ah.FetchBasket(ctx, c.c)
		if err != nil {
			return d.upstream(c, "get shopping list", err), nil
		}
		plan := planRemoval(b, a.Refs, a.ProductIDs, a.Notes)
		if len(plan.removed) == 0 {
			return toolError(codeNotFound, "none of the given entries are on the shopping list", map[string]any{"not_found": plan.notFound}), nil
		}
		after, still, fail := d.applyRemoval(c, plan)
		if fail != nil {
			return fail, nil
		}
		v := listView(after)
		v["removed"] = plan.removed
		v["not_found"] = util.NonNil(plan.notFound)
		v["still_present"] = util.NonNil(still)
		return ok(fmt.Sprintf("Removed %d entr(y/ies) from the shopping list.", len(plan.removed)-len(still)), v), nil
	}}
}

func (d *Deps) clearShoppingList(c *call, confirm string, tool string) *mcp.CallToolResult {
	b, err := ah.FetchBasket(c.ctx, c.c)
	if err != nil {
		return d.upstream(c, "get shopping list", err)
	}
	entries := b.ListEntries()
	if len(entries) == 0 {
		v := listView(b)
		v["mode"] = "shopping_list"
		v["removed"] = []ah.ListEntry{}
		return ok("The shopping list is already empty.", v)
	}
	if !util.IsYes(confirm) {
		return toolError(codeConfirm, fmt.Sprintf("%s would remove all %d shopping list entries", tool, len(entries)),
			map[string]any{"mode": "shopping_list", "would_remove": entries})
	}
	refs := make([]string, 0, len(entries))
	for _, e := range entries {
		refs = append(refs, e.Ref)
	}
	plan := planRemoval(b, refs, nil, nil)
	after, still, fail := d.applyRemoval(c, plan)
	if fail != nil {
		return fail
	}
	d.afterCartMutation(c)
	v := listView(after)
	v["mode"] = "shopping_list"
	v["removed"] = plan.removed
	v["still_present"] = util.NonNil(still)
	return ok(fmt.Sprintf("Cleared %d shopping list entr(y/ies).", len(plan.removed)-len(still)), v)
}

func (d *Deps) clearShoppingListTool() server.ServerTool {
	tool := mcp.NewTool("ah_clear_shopping_list",
		mcp.WithDescription("Remove ALL products and notes from the Albert Heijn shopping list. Requires confirm=\"yes\"; without it returns confirm_required with a preview of what would be removed. "+
			listNote+"It does not touch an active online order (use ah_clear_cart for that)."),
		confirmParam(`Must be "yes" to actually clear; omit it first to get a preview.`),
		annotations("Albert Heijn: Clear Shopping List", false, true, true),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Confirm string `json:"confirm"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		return d.clearShoppingList(c, a.Confirm, "ah_clear_shopping_list"), nil
	}}
}

// addToActiveOrder handles ah_add_to_shopping_list while an order is active:
// AH then adds products straight into the order and rejects notes. A placed
// (scheduled) order needs confirm; a draft order is edited directly.
func (d *Deps) addToActiveOrder(c *call, s *cartState, items []ah.BasketMutationItem, notes []noteQty, confirm string) *mcp.CallToolResult {
	skipped := make([]string, 0, len(notes))
	for _, n := range notes {
		skipped = append(skipped, strings.TrimSpace(n.Text))
	}
	extra := map[string]any{"mode": s.mode, "order_status": s.orderStatus(), "order_id": s.order.ID, "would_add": items, "skipped_notes": skipped}
	if len(items) == 0 {
		return toolError(codeInvalid, "Albert Heijn does not accept free-text notes while an order is active; add products instead", extra)
	}
	if s.scheduled && !util.IsYes(confirm) {
		return toolError(codeConfirm, fmt.Sprintf("adding %d product(s) changes placed order %s, which will be delivered", len(items), s.order.ID), extra)
	}
	if _, err := ah.AddBasketProducts(c.ctx, c.c, items); err != nil {
		return d.upstream(c, "add to active order", err)
	}
	d.afterCartMutation(c)
	after, fail := d.loadCart(c)
	if fail != nil {
		return fail
	}
	v := after.view()
	v["added_products"] = len(items)
	v["skipped_notes"] = skipped
	summary := fmt.Sprintf("Added %d product(s) to active order %s (%s).", len(items), s.order.ID, s.orderStatus())
	if len(skipped) > 0 {
		v["note"] = "Notes were skipped: Albert Heijn does not accept free-text notes while an order is active."
		summary += " Notes skipped."
	}
	return ok(summary, v)
}
