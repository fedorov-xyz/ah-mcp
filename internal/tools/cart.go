package tools

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	appie "github.com/gwillem/appie-go"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/fedorov-xyz/ah-mcp/internal/ah"
	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

// Cart modes.
const (
	modeList  = "shopping_list"
	modeOrder = "active_order"
)

const cartNote = "The cart is the ACTIVE ONLINE ORDER if one exists (a delivery/pickup slot was chosen in the AH app), otherwise it is the shopping-list basket — " +
	"in that case cart and shopping list are the same data, so do not add items through both. The response field mode says which one was used. " +
	"In active_order mode, order_status=draft means a slot is chosen but the order is not placed yet (edit it freely, like in the app); " +
	"order_status=scheduled means the order was placed — edits change what is delivered and paid. This server cannot check out or place orders."

type cartItem struct {
	ProductID int     `json:"product_id"`
	Name      string  `json:"name"`
	Quantity  int     `json:"quantity"`
	Price     float64 `json:"price,omitempty"`
	LineTotal float64 `json:"line_total,omitempty"`
	// BonusPrice is the reduced per-piece price for per-piece deals; price
	// and line_total stay at the regular price so the lines add up to
	// totals.subtotal, and all reductions are in totals.discount.
	BonusPrice float64 `json:"bonus_price,omitempty"`
}

func basketCartItem(id int, name string, qty int, p ah.BasketProductPrice) cartItem {
	price := util.Round2(p.Regular())
	return cartItem{ProductID: id, Name: name, Quantity: qty, Price: price,
		LineTotal: util.Round2(price * float64(qty)), BonusPrice: util.Round2(p.Reduced())}
}

type deliverySlot struct {
	Date      string `json:"date,omitempty"`
	Window    string `json:"window,omitempty"`
	Display   string `json:"display,omitempty"`
	Type      string `json:"shopping_type,omitempty"`
	Status    string `json:"status,omitempty"`
	Modifable bool   `json:"modifiable"`
}

type cartState struct {
	mode   string
	basket ah.Basket
	order  *ah.ActiveOrder
	slot   *deliverySlot
	items  []cartItem
	// scheduled: the active order has been placed (it is in the upcoming
	// fulfillments, or was reopened for editing), so edits need confirm.
	scheduled bool
}

func (s *cartState) orderStatus() string {
	if s.scheduled {
		return "scheduled"
	}
	return "draft"
}

func (s *cartState) quantity(productID int) int {
	for _, it := range s.items {
		if it.ProductID == productID {
			return it.Quantity
		}
	}
	return 0
}

// loadCart detects the mode and loads the cart contents. In active-order
// mode it also primes the appie client's current-order header via GetOrder,
// which the order write path requires.
func (d *Deps) loadCart(c *call) (*cartState, *mcp.CallToolResult) {
	b, err := ah.FetchBasket(c.ctx, c.c)
	if err != nil {
		return nil, d.upstream(c, "get basket", err)
	}
	s := &cartState{mode: modeList, basket: b}
	if !b.HasActiveOrder() {
		for _, it := range b.ItemsInList {
			s.items = append(s.items, basketCartItem(it.ProductID(), it.Product.Title, it.Quantity, it.Product.PriceV2))
		}
		return s, nil
	}
	s.mode = modeOrder
	o, err := ah.GetActiveOrder(c.ctx, c.c)
	if err != nil {
		return nil, d.upstream(c, "get active order", err)
	}
	s.order = o
	// Per-item prices come from the GraphQL basket (the REST summary has none).
	prices := map[int]ah.BasketProductPrice{}
	for _, it := range b.ItemsInOrder {
		prices[it.ProductID()] = it.Product.PriceV2
	}
	for _, it := range o.Items {
		name := ""
		if it.Product != nil {
			name = it.Product.Title
		}
		s.items = append(s.items, basketCartItem(it.ProductID, name, it.Quantity, prices[it.ProductID]))
	}
	// Delivery slot: the fulfillment list once the order is submitted,
	// otherwise the active order summary.
	if id, err := strconv.Atoi(o.ID); err == nil {
		if fs, err := c.c.GetFulfillments(c.ctx); err == nil {
			for _, f := range fs {
				if f.OrderID == id {
					s.scheduled = true
					s.slot = &deliverySlot{Date: f.Delivery.Slot.Date, Display: f.Delivery.Slot.DateDisplay,
						Window: f.Delivery.Slot.TimeDisplay, Type: f.ShoppingType, Status: f.StatusDescription, Modifable: f.Modifiable}
				}
			}
		}
	}
	if strings.EqualFold(o.State, "REOPENED") {
		s.scheduled = true
	}
	if s.slot == nil && (o.DeliveryDate != "" || b.Summary.DeliveryDate != "") {
		s.slot = &deliverySlot{Date: util.Or(o.DeliveryDate, util.DayOf(b.Summary.DeliveryDate)), Type: util.Or(o.ShoppingType, b.Summary.ShoppingType), Modifable: true}
		if o.DeliveryStart != "" && o.DeliveryEnd != "" {
			s.slot.Window = o.DeliveryStart + " - " + o.DeliveryEnd
		}
	}
	return s, nil
}

func (s *cartState) view() map[string]any {
	v := map[string]any{
		"mode":             s.mode,
		"has_active_order": s.mode == modeOrder,
		"currency":         "EUR",
		"items":            util.NonNil(s.items),
		"item_count":       len(s.items),
	}
	var subtotal, discount, total float64
	if s.mode == modeOrder {
		total, discount = s.order.Total, s.order.Discount
		subtotal = s.order.BeforeDiscount
		if subtotal == 0 {
			subtotal = total + math.Abs(discount)
		}
		v["order_id"] = s.order.ID
		v["order_status"] = s.orderStatus()
		if s.slot != nil {
			v["delivery_slot"] = s.slot
		}
	} else {
		p := s.basket.Summary.Price
		subtotal, discount, total = p.PriceBeforeDiscount.Amount, p.Discount.Amount, p.TotalPrice.Amount
		if total == 0 {
			for _, it := range s.items {
				total += it.LineTotal
			}
			subtotal = total + discount
		}
		v["note"] = "No active order: this is the shopping list basket. Nothing will be delivered until a slot is chosen in the AH app."
	}
	discount = math.Abs(discount)
	v["totals"] = map[string]float64{"subtotal": util.Round2(subtotal), "discount": util.Round2(discount), "total": util.Round2(total)}
	return v
}

func (d *Deps) getCartTool() server.ServerTool {
	tool := mcp.NewTool("ah_get_cart",
		mcp.WithDescription("Show the Albert Heijn cart with items and totals (subtotal, discount, total), plus mode, has_active_order, order_id and delivery_slot when an order exists. "+cartNote),
		readOnly("Albert Heijn: Cart"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		s, fail := d.loadCart(c)
		if fail != nil {
			return fail, nil
		}
		v := s.view()
		return ok(fmt.Sprintf("Cart (%s): %d item(s), total €%.2f.", s.mode, len(s.items), v["totals"].(map[string]float64)["total"]), v), nil
	}}
}

// nearDuplicates returns duplicate pairs involving productID.
func nearDuplicates(items []cartItem, productID int) []ah.DuplicatePair {
	named := make([]ah.NamedItem, 0, len(items))
	for _, it := range items {
		named = append(named, ah.NamedItem{ProductID: it.ProductID, Title: it.Name, Quantity: it.Quantity})
	}
	var out []ah.DuplicatePair
	for _, p := range ah.FindDuplicatePairs(named) {
		if p.A.ProductID == productID || p.B.ProductID == productID {
			out = append(out, p)
		}
	}
	return util.NonNil(out)
}

func (d *Deps) updateCartItemTool() server.ServerTool {
	tool := mcp.NewTool("ah_update_cart_item",
		mcp.WithDescription("Set the quantity of one product in the cart (0 removes it). "+cartNote+
			" For a placed order (order_status=scheduled) it requires confirm=\"yes\" (without it you get confirm_required and a preview); a draft order is edited directly. "+
			"The response lists near_duplicates: similar products already in the cart (e.g. two brands of lasagne)."),
		mcp.WithInteger("product_id", mcp.Required(), mcp.Description("Webshop product id."), mcp.Min(1)),
		mcp.WithInteger("quantity", mcp.Required(), mcp.Description("New quantity, 0-99; 0 removes the product."), mcp.Min(0), mcp.Max(99)),
		confirmParam(`Must be "yes" when the active order is already placed (order_status=scheduled).`),
		annotations("Albert Heijn: Update Cart Item", false, true, true),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			ProductID int    `json:"product_id"`
			Quantity  *int   `json:"quantity"`
			Confirm   string `json:"confirm"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		if a.ProductID <= 0 {
			return invalid("product_id must be a positive integer"), nil
		}
		if a.Quantity == nil || *a.Quantity < 0 || *a.Quantity > 99 {
			return invalid("quantity is required and must be between 0 and 99"), nil
		}
		qty := *a.Quantity
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		s, fail := d.loadCart(c)
		if fail != nil {
			return fail, nil
		}
		current := s.quantity(a.ProductID)
		if qty == 0 && current == 0 {
			return toolError(codeNotFound, fmt.Sprintf("product %d is not in the cart (mode %s)", a.ProductID, s.mode), map[string]any{"mode": s.mode}), nil
		}
		if s.mode == modeOrder {
			if s.scheduled && !util.IsYes(a.Confirm) {
				return toolError(codeConfirm, fmt.Sprintf("changing product %d from %d to %d changes the placed order that will be delivered", a.ProductID, current, qty),
					map[string]any{"mode": s.mode, "order_status": s.orderStatus(), "order_id": s.order.ID, "preview": map[string]int{"product_id": a.ProductID, "current_quantity": current, "new_quantity": qty}}), nil
			}
			if err := c.c.UpdateOrderItem(ctx, a.ProductID, qty); err != nil {
				return d.upstream(c, "update active order", err), nil
			}
			d.afterCartMutation(c)
		} else {
			item := []ah.BasketMutationItem{{ID: a.ProductID, Quantity: qty}}
			var err error
			if current > 0 || qty == 0 {
				_, err = ah.UpdateBasketProducts(ctx, c.c, item)
			} else {
				_, err = ah.AddBasketProducts(ctx, c.c, item)
			}
			if err != nil {
				return d.upstream(c, "update shopping list basket", err), nil
			}
		}
		after, fail := d.loadCart(c)
		if fail != nil {
			return fail, nil
		}
		v := after.view()
		v["updated"] = map[string]int{"product_id": a.ProductID, "previous_quantity": current, "quantity": qty}
		v["near_duplicates"] = nearDuplicates(after.items, a.ProductID)
		return ok(fmt.Sprintf("Product %d set to %d in the cart (%s).", a.ProductID, qty, after.mode), v), nil
	}}
}

func (d *Deps) clearCartTool() server.ServerTool {
	tool := mcp.NewTool("ah_clear_cart",
		mcp.WithDescription("Remove everything from the cart. Requires confirm=\"yes\"; without it returns confirm_required with a preview. "+cartNote+
			" In shopping_list mode this is exactly ah_clear_shopping_list (products and notes)."),
		confirmParam(`Must be "yes" to actually clear; omit it first to get a preview.`),
		annotations("Albert Heijn: Clear Cart", false, true, true),
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
		s, fail := d.loadCart(c)
		if fail != nil {
			return fail, nil
		}
		if s.mode == modeList {
			return d.clearShoppingList(c, a.Confirm, "ah_clear_cart"), nil
		}
		if len(s.items) == 0 {
			v := s.view()
			v["removed"] = []cartItem{}
			return ok("The active order is already empty.", v), nil
		}
		if !util.IsYes(a.Confirm) {
			return toolError(codeConfirm, fmt.Sprintf("this would remove all %d products from active order %s, which changes what is delivered", len(s.items), s.order.ID),
				map[string]any{"mode": s.mode, "order_id": s.order.ID, "would_remove": s.items}), nil
		}
		if err := c.c.ClearOrder(ctx); err != nil {
			return d.upstream(c, "clear active order", err), nil
		}
		d.afterCartMutation(c)
		after, fail := d.loadCart(c)
		if fail != nil {
			return fail, nil
		}
		v := after.view()
		v["removed"] = s.items
		return ok(fmt.Sprintf("Removed %d product(s) from active order %s.", len(s.items), s.order.ID), v), nil
	}}
}

func (d *Deps) shoppingListToCartTool() server.ServerTool {
	tool := mcp.NewTool("ah_shopping_list_to_cart",
		mcp.WithDescription("Copy the unchecked product items of the shopping list into the ACTIVE ONLINE ORDER. Only works when an order exists; "+
			"otherwise returns no_active_order (create an order/slot in the AH app first — this server never creates orders or checks out). "+
			"Quantities are raised to the list quantity, never lowered. Free-text notes are skipped and listed. "+
			"While an order is active AH keeps no separate list (added products go straight into the order), so usually there is nothing to copy. "+
			"For a placed order (order_status=scheduled) it requires confirm=\"yes\" (without it you get a preview)."),
		confirmParam(`Must be "yes" to change a placed (scheduled) order; omit it first to get a preview.`),
		annotations("Albert Heijn: Shopping List to Active Order", false, false, true),
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
		s, fail := d.loadCart(c)
		if fail != nil {
			return fail, nil
		}
		if s.mode != modeOrder {
			return toolError(codeNoOrder, "there is no active online order; the shopping list already is your basket", map[string]any{"mode": s.mode}), nil
		}
		type change struct {
			ProductID int    `json:"product_id"`
			Name      string `json:"name"`
			From      int    `json:"from_quantity"`
			To        int    `json:"to_quantity"`
		}
		var changes []change
		var skippedNotes, skippedChecked []string
		var items []appie.OrderItem
		for _, e := range s.basket.ListEntries() {
			switch {
			case e.Kind == "note":
				skippedNotes = append(skippedNotes, e.Name)
			case e.Checked:
				skippedChecked = append(skippedChecked, e.Ref)
			case e.ProductID > 0 && e.Quantity > 0:
				cur := s.quantity(e.ProductID)
				if e.Quantity > cur {
					changes = append(changes, change{ProductID: e.ProductID, Name: e.Name, From: cur, To: e.Quantity})
					items = append(items, appie.OrderItem{ProductID: e.ProductID, Quantity: e.Quantity})
				}
			}
		}
		extra := map[string]any{"mode": s.mode, "order_id": s.order.ID, "changes": util.NonNil(changes),
			"skipped_notes": util.NonNil(skippedNotes), "skipped_checked": util.NonNil(skippedChecked)}
		if len(items) == 0 {
			v := s.view()
			for k, val := range extra {
				v[k] = val
			}
			return ok("Nothing to add: the active order already contains every unchecked list product.", v), nil
		}
		if s.scheduled && !util.IsYes(a.Confirm) {
			return toolError(codeConfirm, fmt.Sprintf("this would change %d product(s) in placed order %s", len(items), s.order.ID), extra), nil
		}
		if err := c.c.AddToOrder(ctx, items); err != nil {
			return d.upstream(c, "add to active order", err), nil
		}
		d.afterCartMutation(c)
		after, fail := d.loadCart(c)
		if fail != nil {
			return fail, nil
		}
		v := after.view()
		for k, val := range extra {
			v[k] = val
		}
		return ok(fmt.Sprintf("Added %d product(s) to active order %s.", len(items), s.order.ID), v), nil
	}}
}

func (d *Deps) findDuplicatesTool() server.ServerTool {
	tool := mcp.NewTool("ah_find_duplicates",
		mcp.WithDescription("Find near-duplicate products — different product ids that are functionally the same (e.g. two brands of \"lasagne all'uovo\") — "+
			"in the shopping list (scope=list), the cart (scope=cart) or a specific order (scope=order with order_id). "+
			"Uses normalised title tokens; same product with a higher quantity is not a duplicate. Run it before an order closes."),
		mcp.WithString("scope", mcp.Required(), mcp.Description("Where to look."), mcp.Enum("list", "cart", "order")),
		mcp.WithInteger("order_id", mcp.Description("Required for scope=order (from ah_get_orders)."), mcp.Min(1)),
		readOnly("Albert Heijn: Find Duplicates"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Scope   string `json:"scope"`
			OrderID int    `json:"order_id"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		switch a.Scope {
		case "list", "cart":
		case "order":
			if a.OrderID <= 0 {
				return invalid("order_id is required for scope=order"), nil
			}
		default:
			return invalid(`scope must be "list", "cart" or "order"`), nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		var items []ah.NamedItem
		data := map[string]any{"scope": a.Scope}
		switch a.Scope {
		case "list":
			b, err := ah.FetchBasket(ctx, c.c)
			if err != nil {
				return d.upstream(c, "get shopping list", err), nil
			}
			for _, e := range b.ListEntries() {
				items = append(items, ah.NamedItem{ProductID: e.ProductID, Title: e.Name, Quantity: e.Quantity, Ref: e.Ref})
			}
		case "cart":
			s, fail := d.loadCart(c)
			if fail != nil {
				return fail, nil
			}
			data["mode"] = s.mode
			for _, it := range s.items {
				items = append(items, ah.NamedItem{ProductID: it.ProductID, Title: it.Name, Quantity: it.Quantity})
			}
		case "order":
			o, err := c.c.GetOrderDetails(ctx, a.OrderID)
			if err != nil {
				return d.upstream(c, "get order details", err), nil
			}
			data["order_id"] = a.OrderID
			for _, it := range o.Items {
				n := ah.NamedItem{ProductID: it.ProductID, Quantity: it.Quantity}
				if it.Product != nil {
					n.Title = it.Product.Title
				}
				items = append(items, n)
			}
		}
		pairs := util.NonNil(ah.FindDuplicatePairs(items))
		data["items_checked"] = len(items)
		data["duplicates"] = pairs
		return ok(fmt.Sprintf("%d possible duplicate pair(s) among %d item(s).", len(pairs), len(items)), data), nil
	}}
}
