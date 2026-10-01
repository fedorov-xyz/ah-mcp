package tools

import (
	"context"
	"fmt"
	"math"
	"strings"

	appie "github.com/gwillem/appie-go"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/fedorov-xyz/ah-mcp/internal/ah"
	"github.com/fedorov-xyz/ah-mcp/internal/cache"
	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

func (d *Deps) frequentItemsTool() server.ServerTool {
	tool := mcp.NewTool("ah_frequent_items",
		mcp.WithDescription("List the products the member orders most often online, counted over their most recent online orders (upcoming and past): "+
			"order_count, total quantity, last ordered date and last price. Good input for a weekly shopping list or for ah_bonus_for_habits. "+
			"In-store purchases are not included here; use ah_purchase_history for receipts plus orders."),
		mcp.WithInteger("max_orders", mcp.Description("How many recent orders to analyse (default 25, max 100)."), mcp.Min(1), mcp.Max(100)),
		limitParam(),
		readOnly("Albert Heijn: Frequent Items"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			MaxOrders *int `json:"max_orders"`
			Limit     *int `json:"limit"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		limit, r := limitOf(a.Limit)
		if r != nil {
			return r, nil
		}
		maxOrders := 25
		if a.MaxOrders != nil {
			if *a.MaxOrders < 1 || *a.MaxOrders > 100 {
				return invalid("max_orders must be between 1 and 100"), nil
			}
			maxOrders = *a.MaxOrders
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		res, err := d.frequent(c, maxOrders)
		if err != nil {
			return d.upstream(c, "get order history", err), nil
		}
		items, truncated := util.Truncate(res.Items, limit)
		data := map[string]any{"orders_analyzed": res.Orders, "orders_failed": res.Failed, "currency": "EUR",
			"count": len(items), "truncated": truncated, "items": items}
		return ok(fmt.Sprintf("Top %d product(s) over %d order(s).", len(items), res.Orders), data), nil
	}}
}

type historyResult struct {
	Products []ah.HistoryProduct
	Coverage historyCoverage
	Sources  map[string]string
	ByMonth  []ah.MonthStat
}

// historyCoverage says exactly how much of the window was analysed, so a
// capped or partial result is never mistaken for a complete one.
type historyCoverage struct {
	Complete            bool `json:"complete"`
	ReceiptsInWindow    int  `json:"receipts_in_window"`
	ReceiptsAnalyzed    int  `json:"receipts_analyzed"`
	ReceiptsSkipped     int  `json:"receipts_skipped_over_cap"`
	ReceiptListComplete bool `json:"receipt_list_complete"`
	OrdersInWindow      int  `json:"online_orders_in_window"`
	OrdersAnalyzed      int  `json:"online_orders_analyzed"`
	OrdersSkipped       int  `json:"online_orders_skipped_over_cap"`
	DetailsFailed       int  `json:"details_failed"`
}

func (d *Deps) purchaseHistoryTool() server.ServerTool {
	tool := mcp.NewTool("ah_purchase_history",
		mcp.WithDescription("Combine in-store receipts and delivered online orders of the last N days into one per-product history: "+
			"purchases, quantity, last bought date and spend, plus totals. Receipt products are mapped to webshop product ids where AH can (ConvertPOSIDs), "+
			"so the same product bought in store and online is merged. by_month gives receipts, online orders and spend per calendar month from the list totals "+
			"(complete even when detail fetching is capped) — use it for monthly questions instead of calling this tool repeatedly with different windows. "+
			"totals and by_month use what was paid (receipt and order totals); a product's spend is the sum of its line prices, so per-product spend does not add up to totals. "+
			"coverage says how much of the window the per-product stats cover. Use it for spending questions and to plan regular purchases."),
		mcp.WithInteger("days", mcp.Description("Look-back window in days (default 90, max 365)."), mcp.Min(1), mcp.Max(365)),
		limitParam(),
		readOnly("Albert Heijn: Purchase History"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Days  *int `json:"days"`
			Limit *int `json:"limit"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		limit, r := limitOf(a.Limit)
		if r != nil {
			return r, nil
		}
		days := 90
		if a.Days != nil {
			if *a.Days < 1 || *a.Days > 365 {
				return invalid("days must be between 1 and 365"), nil
			}
			days = *a.Days
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		res, fs, ts, err := d.history(c, days)
		if err != nil {
			return d.upstream(c, "get purchase history", err), nil
		}
		products, truncated := util.Truncate(res.Products, limit)
		cov := res.Coverage
		data := map[string]any{
			"from": fs, "to": ts, "currency": "EUR", "sources": res.Sources, "coverage": cov, "by_month": util.NonNil(res.ByMonth),
			"totals":            monthTotals(res.ByMonth),
			"distinct_products": len(res.Products), "count": len(products),
			"truncated": truncated, "products": products,
		}
		summary := fmt.Sprintf("%d product(s) bought in the last %d days.", len(res.Products), days)
		if !cov.Complete {
			summary += fmt.Sprintf(" INCOMPLETE: analysed %d of %d receipts and %d of %d online orders",
				cov.ReceiptsAnalyzed, cov.ReceiptsInWindow, cov.OrdersAnalyzed, cov.OrdersInWindow)
			if !cov.ReceiptListComplete {
				summary += " (the receipt list itself is incomplete)"
			}
			summary += "; counts and spend are lower bounds — use a shorter days window for a complete result."
		}
		return ok(summary, data), nil
	}}
}

func (d *Deps) getOrdersTool() server.ServerTool {
	tool := mcp.NewTool("ah_get_orders",
		mcp.WithDescription("List online orders: scope=upcoming (submitted orders awaiting delivery, with slot and whether they are still modifiable) "+
			"or scope=past (delivered/closed orders). Use order_id with ah_get_order_details. "+
			"This server cannot reopen, edit after placement, or create orders."),
		mcp.WithString("scope", mcp.Description(`"upcoming" (default) or "past".`), mcp.Enum("upcoming", "past")),
		limitParam(),
		readOnly("Albert Heijn: Orders"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Scope string `json:"scope"`
			Limit *int   `json:"limit"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		limit, r := limitOf(a.Limit)
		if r != nil {
			return r, nil
		}
		scope := util.Or(a.Scope, "upcoming")
		if scope != "upcoming" && scope != "past" {
			return invalid(`scope must be "upcoming" or "past"`), nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		type order struct {
			OrderID      int     `json:"order_id"`
			Date         string  `json:"date,omitempty"`
			DateDisplay  string  `json:"date_display,omitempty"`
			TimeWindow   string  `json:"time_window,omitempty"`
			Status       string  `json:"status,omitempty"`
			ShoppingType string  `json:"shopping_type,omitempty"`
			Total        float64 `json:"total"`
			Discount     float64 `json:"discount,omitempty"`
			Modifiable   *bool   `json:"modifiable,omitempty"`
		}
		var out []order
		if scope == "upcoming" {
			fs, err := c.c.GetFulfillments(ctx)
			if err != nil {
				return d.upstream(c, "get upcoming orders", err), nil
			}
			for _, f := range fs {
				m := f.Modifiable
				out = append(out, order{OrderID: f.OrderID, Date: f.Delivery.Slot.Date, DateDisplay: f.Delivery.Slot.DateDisplay,
					TimeWindow: f.Delivery.Slot.TimeDisplay, Status: f.StatusDescription, ShoppingType: f.ShoppingType,
					Total: util.Round2(f.TotalPrice), Modifiable: &m})
			}
		} else {
			closed, err := ah.FetchClosedOrders(ctx, c.c)
			if err != nil {
				return d.upstream(c, "get past orders", err), nil
			}
			for _, o := range closed {
				out = append(out, order{OrderID: o.OrderID, Date: o.Date, DateDisplay: o.DateDisplay, TimeWindow: o.TimeWindow,
					Status: o.Status, ShoppingType: o.ShoppingType, Total: util.Round2(o.TotalPrice), Discount: util.Round2(o.Discount)})
			}
		}
		out, truncated := util.Truncate(out, limit)
		data := map[string]any{"scope": scope, "currency": "EUR", "count": len(out), "truncated": truncated, "orders": out}
		summary := fmt.Sprintf("%d %s order(s).", len(out), scope)
		// A draft order (slot chosen, not placed) is not in the upcoming
		// fulfillments; point to it rather than answer "no orders". Best
		// effort: a failed basket read just leaves the note out.
		if scope == "upcoming" && len(out) == 0 {
			if b, err := ah.FetchBasket(ctx, c.c); err == nil && b.HasActiveOrder() {
				data["note"] = draftOrderNote
				summary += " A draft order exists; see ah_get_cart."
			}
		}
		return ok(summary, data), nil
	}}
}

const draftOrderNote = "No placed orders, but a draft order exists: a slot was chosen in the AH app and the order is not placed yet. " +
	"Use ah_get_cart to see its products, slot and total."

func (d *Deps) getOrderDetailsTool() server.ServerTool {
	tool := mcp.NewTool("ah_get_order_details",
		mcp.WithDescription("Show the products, quantities and prices of one online order (order_id from ah_get_orders). Read-only."),
		mcp.WithInteger("order_id", mcp.Required(), mcp.Description("Numeric order id."), mcp.Min(1)),
		readOnly("Albert Heijn: Order Details"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			OrderID int `json:"order_id"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		if a.OrderID <= 0 {
			return invalid("order_id must be a positive integer"), nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		o, err := c.c.GetOrderDetails(ctx, a.OrderID)
		if err != nil {
			return d.upstream(c, "get order details", err), nil
		}
		items := make([]cartItem, 0, len(o.Items))
		var sum float64
		for _, it := range o.Items {
			ci := cartItem{ProductID: it.ProductID, Quantity: it.Quantity}
			if it.Product != nil {
				ci.Name, ci.Price = it.Product.Title, util.Round2(it.Product.Price.Now)
				ci.LineTotal = util.Round2(ci.Price * float64(it.Quantity))
			}
			sum += ci.LineTotal
			items = append(items, ci)
		}
		total, source := o.TotalPrice, "order"
		if total == 0 {
			// The details endpoint carries no total; fall back to the
			// fulfillment lists (MaxTomahawk's lookupOrderTotal idea).
			total, source = sum, "sum_of_lines"
			if fs, err := c.c.GetFulfillments(ctx); err == nil {
				for _, f := range fs {
					if f.OrderID == a.OrderID && f.TotalPrice > 0 {
						total, source = f.TotalPrice, "fulfillment"
					}
				}
			}
			if source == "sum_of_lines" {
				if closed, err := ah.FetchClosedOrders(ctx, c.c); err == nil {
					for _, co := range closed {
						if co.OrderID == a.OrderID && co.TotalPrice > 0 {
							total, source = co.TotalPrice, "fulfillment"
						}
					}
				}
			}
		}
		data := map[string]any{"order_id": a.OrderID, "state": o.State, "currency": "EUR", "items": items,
			"item_count": len(items), "total": util.Round2(total), "total_source": source, "lines_total": util.Round2(sum)}
		// AH does not itemise the difference between the order total and the
		// product lines (deposits, delivery/service fees, order discounts).
		if diff := util.Round2(total - sum); source != "sum_of_lines" && diff != 0 {
			data["other_charges"] = diff
			data["other_charges_note"] = "Difference between the order total and the product lines, not itemised by Albert Heijn: " +
				"usually bottle/can deposit (statiegeld), delivery or service fees, or order-level discounts (negative)."
		}
		return ok(fmt.Sprintf("Order %d: %d item(s), total €%.2f.", a.OrderID, len(items), util.Round2(total)), data), nil
	}}
}

func (d *Deps) getReceiptsTool() server.ServerTool {
	tool := mcp.NewTool("ah_get_receipts",
		mcp.WithDescription("In-store receipts (kassabonnen). Without receipt_id: list recent receipts (id, date, total). "+
			"With receipt_id: the receipt's items (with webshop product_id where AH can map it), discounts and payments."),
		mcp.WithString("receipt_id", mcp.Description("Receipt id from the list.")),
		limitParam(),
		readOnly("Albert Heijn: Receipts"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			ReceiptID string `json:"receipt_id"`
			Limit     *int   `json:"limit"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		limit, r := limitOf(a.Limit)
		if r != nil {
			return r, nil
		}
		a.ReceiptID = strings.TrimSpace(a.ReceiptID)
		if len(a.ReceiptID) > 200 {
			return invalid("receipt_id is too long"), nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		if a.ReceiptID == "" {
			rs, more, err := ah.RecentReceipts(ctx, c.c, limit)
			if err != nil {
				return d.upstream(c, "get receipts", err), nil
			}
			type receipt struct {
				ReceiptID string  `json:"receipt_id"`
				Date      string  `json:"date"`
				Total     float64 `json:"total"`
			}
			out := make([]receipt, 0, len(rs))
			for _, r := range rs {
				out = append(out, receipt{ReceiptID: r.TransactionID, Date: r.Date, Total: util.Round2(r.TotalAmount)})
			}
			data := map[string]any{"currency": "EUR", "count": len(out), "truncated": more, "receipts": out}
			return ok(fmt.Sprintf("%d receipt(s).", len(out)), data), nil
		}
		rc, err := c.c.GetReceipt(ctx, a.ReceiptID)
		if err != nil {
			// AH answers an unknown receipt id with a GraphQL error rather
			// than a 404 (seen on the live API).
			if ah.StatusOf(err) == 0 && strings.Contains(err.Error(), "graphql error") {
				return toolError(codeNotFound, "receipt "+a.ReceiptID+" not found", nil), nil
			}
			return d.upstream(c, "get receipt", err), nil
		}
		if rc.TransactionID == "" && len(rc.Items) == 0 {
			return toolError(codeNotFound, "receipt "+a.ReceiptID+" not found", nil), nil
		}
		type item struct {
			Description string  `json:"description"`
			Quantity    int     `json:"quantity"`
			Amount      float64 `json:"amount"`
			UnitPrice   float64 `json:"unit_price,omitempty"`
			ProductID   int     `json:"product_id,omitempty"`
		}
		type money struct {
			Name   string  `json:"name"`
			Amount float64 `json:"amount"`
		}
		items := make([]item, 0, len(rc.Items))
		for _, it := range rc.Items {
			items = append(items, item{Description: it.Description, Quantity: it.Quantity, Amount: util.Round2(it.Amount), UnitPrice: util.Round2(it.UnitPrice), ProductID: it.WebshopID})
		}
		discounts := make([]money, 0, len(rc.Discounts))
		var totalDiscount float64
		for _, dl := range rc.Discounts {
			discounts = append(discounts, money{Name: dl.Name, Amount: util.Round2(dl.Amount)})
			totalDiscount += math.Abs(dl.Amount)
		}
		payments := make([]money, 0, len(rc.Payments))
		var paid float64
		for _, p := range rc.Payments {
			payments = append(payments, money{Name: p.Method, Amount: util.Round2(p.Amount)})
			paid += p.Amount
		}
		data := map[string]any{"receipt_id": a.ReceiptID, "currency": "EUR", "items": items, "discounts": discounts,
			"payments": payments, "total_paid": util.Round2(paid), "total_discount": util.Round2(totalDiscount)}
		return ok(fmt.Sprintf("Receipt %s: %d item(s), paid €%.2f.", a.ReceiptID, len(items), util.Round2(paid)), data), nil
	}}
}

// history merges in-store receipts and delivered online orders of the last
// days days (from fs to ts, YYYY-MM-DD) into per-product stats, cached per
// member when complete. ah_purchase_history and ah_bonus_for_habits share
// it.
func (d *Deps) history(c *call, days int) (res historyResult, fs, ts string, err error) {
	to := d.today()
	fs, ts = to.AddDate(0, 0, -days).Format(day), to.Format(day)
	res, err = cache.GetOrLoadIf(d.Cache, c.ns, fmt.Sprintf("history:%s:%d", ts, days), cache.TTLPersonal, func() (historyResult, error) {
		out := historyResult{Sources: map[string]string{}}
		cov := &out.Coverage
		receipts, listComplete, rerr := ah.ReceiptsBetween(c.ctx, c.c, fs, ts)
		allReceipts := receipts // by_month uses every receipt, not just the capped ones
		var rd []ah.ReceiptDetailsResult
		if rerr == nil {
			cov.ReceiptsInWindow, cov.ReceiptListComplete = len(receipts), listComplete
			receipts, _ = util.Truncate(receipts, ah.MaxHistoryReceipts)
			cov.ReceiptsSkipped = cov.ReceiptsInWindow - len(receipts)
			rd = ah.FetchReceiptDetails(c.ctx, c.c, receipts)
		}
		out.Sources["receipts"] = availability(rerr)
		refs, oerr := ah.DeliveredOrders(c.ctx, c.c, fs, ts)
		var od []ah.OrderDetailsResult
		var monthOrders []ah.OrderRef
		if oerr == nil {
			capped, _ := util.Truncate(refs, ah.MaxHistoryOrders)
			for _, r := range ah.FetchOrderDetails(c.ctx, c.c, capped) {
				// Orders whose date only became known from the details
				// may fall outside the window.
				if r.Ref.Date != "" && (r.Ref.Date < fs || r.Ref.Date > ts) {
					continue
				}
				od = append(od, r)
				ref := r.Ref
				if ref.Total == 0 && r.Order != nil {
					ref.Total = orderLineTotal(r.Order)
				}
				monthOrders = append(monthOrders, ref)
			}
			monthOrders = append(monthOrders, refs[len(capped):]...)
			cov.OrdersInWindow = len(od) + len(refs) - len(capped)
			cov.OrdersSkipped = len(refs) - len(capped)
		}
		out.Sources["online_orders"] = availability(oerr)
		if rerr != nil && oerr != nil {
			return out, rerr
		}
		out.Products, cov.DetailsFailed = ah.MergeHistory(rd, od)
		out.ByMonth = ah.ByMonth(allReceipts, monthOrders)
		cov.ReceiptsAnalyzed = len(rd)
		cov.OrdersAnalyzed = len(od)
		for _, r := range rd {
			if r.Err != nil {
				cov.ReceiptsAnalyzed--
			}
		}
		for _, r := range od {
			if r.Err != nil {
				cov.OrdersAnalyzed--
			}
		}
		cov.Complete = rerr == nil && oerr == nil && cov.ReceiptListComplete && cov.ReceiptsSkipped == 0 &&
			cov.OrdersSkipped == 0 && cov.DetailsFailed == 0
		return out, nil
	}, func(r historyResult) bool { return r.Coverage.Complete })
	return res, fs, ts, err
}

// monthTotals sums by_month, so totals always match the monthly breakdown.
func monthTotals(months []ah.MonthStat) map[string]float64 {
	var receipts, online float64
	for _, m := range months {
		receipts += m.ReceiptSpend
		online += m.OnlineSpend
	}
	return map[string]float64{"receipts": util.Round2(receipts), "online_orders": util.Round2(online), "total": util.Round2(receipts + online)}
}

// orderLineTotal is the sum of an order's line prices, used when the
// fulfillment list carries no order total.
func orderLineTotal(o *appie.Order) float64 {
	var sum float64
	for _, it := range o.Items {
		if it.Product != nil {
			sum += it.Product.Price.Now * float64(it.Quantity)
		}
	}
	return util.Round2(sum)
}
