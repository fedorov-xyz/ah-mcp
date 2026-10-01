// Copyright (C) the MaxTomahawk/ah-mcp and bramboe/ah-mcp contributors
// Copyright (C) Igor Fedorov and contributors
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Adapted from github.com/MaxTomahawk/ah-mcp/tools/orders.go
// (ah_get_past_orders, ah_get_frequent_items) @ a770dfb521f7b3616ac3e592e0baa009c39133ef
// and github.com/bramboe/ah-mcp/tools/savings.go (fetchOnlineOrderSavings)
// @ 46b85bc652496590e179a665dda346b206d56291 (AGPL-3.0).
//
// Changes: one CLOSED-fulfillments query serves past orders, frequent items
// and savings; concurrent bounded order-detail fetching.

package ah

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	appie "github.com/gwillem/appie-go"

	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

// ClosedOrder is a past (closed) online order.
type ClosedOrder struct {
	OrderID      int
	Date         string // YYYY-MM-DD delivery date
	DateDisplay  string
	TimeWindow   string
	Status       string
	ShoppingType string
	TotalPrice   float64
	Discount     float64
}

// closedOrdersQuery merges the past-orders query of MaxTomahawk with the
// discount field bramboe uses for savings.
//
// From MaxTomahawk/ah-mcp@a770dfb (OrderFulfillmentsClosed) and
// bramboe/ah-mcp@46b85bc (totalPrice.discount). Verified against the live API on 2026-10-01.
const closedOrdersQuery = `query OrderFulfillmentsClosed {
  orderFulfillments(status: CLOSED) {
    result {
      orderId
      statusDescription
      shoppingType
      totalPrice {
        totalPrice { amount }
        discount { amount }
      }
      delivery {
        status
        slot {
          date
          dateDisplay
          timeDisplay
        }
      }
    }
  }
}`

// FetchClosedOrders returns past online orders, newest first.
func FetchClosedOrders(ctx context.Context, c GraphQLDoer) ([]ClosedOrder, error) {
	var resp struct {
		OrderFulfillments struct {
			Result []struct {
				OrderID           int    `json:"orderId"`
				StatusDescription string `json:"statusDescription"`
				ShoppingType      string `json:"shoppingType"`
				TotalPrice        struct {
					TotalPrice Money `json:"totalPrice"`
					Discount   Money `json:"discount"`
				} `json:"totalPrice"`
				Delivery struct {
					Status string `json:"status"`
					Slot   struct {
						Date        string `json:"date"`
						DateDisplay string `json:"dateDisplay"`
						TimeDisplay string `json:"timeDisplay"`
					} `json:"slot"`
				} `json:"delivery"`
			} `json:"result"`
		} `json:"orderFulfillments"`
	}
	if err := c.DoGraphQL(ctx, closedOrdersQuery, nil, &resp); err != nil {
		return nil, fmt.Errorf("get past orders: %w", err)
	}
	out := make([]ClosedOrder, 0, len(resp.OrderFulfillments.Result))
	for _, r := range resp.OrderFulfillments.Result {
		discount := math.Abs(r.TotalPrice.Discount.Amount)
		out = append(out, ClosedOrder{
			OrderID:      r.OrderID,
			Date:         util.DayOf(r.Delivery.Slot.Date),
			DateDisplay:  r.Delivery.Slot.DateDisplay,
			TimeWindow:   r.Delivery.Slot.TimeDisplay,
			Status:       r.StatusDescription,
			ShoppingType: r.ShoppingType,
			TotalPrice:   r.TotalPrice.TotalPrice.Amount,
			Discount:     discount,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

// OrderRef identifies an order and its date for history aggregation.
type OrderRef struct {
	OrderID int
	Date    string
	// Total is the order total from the fulfillment list (0 if unknown).
	Total float64
}

// RecentOrders returns open and closed orders, newest first, capped at max.
// Open (upcoming) orders count too, as in MaxTomahawk's frequent items.
func RecentOrders(ctx context.Context, c *appie.Client, maxOrders int) ([]OrderRef, error) {
	open, err := c.GetFulfillments(ctx)
	if err != nil {
		return nil, err
	}
	closed, err := FetchClosedOrders(ctx, c)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var refs []OrderRef
	for _, f := range open {
		if !seen[f.OrderID] {
			seen[f.OrderID] = true
			refs = append(refs, OrderRef{OrderID: f.OrderID, Date: util.DayOf(f.Delivery.Slot.Date)})
		}
	}
	for _, o := range closed {
		if !seen[o.OrderID] {
			seen[o.OrderID] = true
			refs = append(refs, OrderRef{OrderID: o.OrderID, Date: o.Date})
		}
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Date > refs[j].Date })
	if maxOrders > 0 && len(refs) > maxOrders {
		refs = refs[:maxOrders]
	}
	return refs, nil
}

// DeliveredOrders returns the online orders that may fall in [from, to]:
// open fulfillments (which include delivered orders AH has not closed yet)
// and closed ones. Orders without a slot date are included so their date can
// be resolved from the order details; callers drop those outside the window.
func DeliveredOrders(ctx context.Context, c *appie.Client, from, to string) ([]OrderRef, error) {
	open, err := c.GetFulfillments(ctx)
	if err != nil {
		return nil, err
	}
	closed, err := FetchClosedOrders(ctx, c)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var refs []OrderRef
	add := func(id int, date string, total float64) {
		if seen[id] || (date != "" && (date < from || date > to)) {
			return
		}
		seen[id] = true
		refs = append(refs, OrderRef{OrderID: id, Date: date, Total: total})
	}
	for _, f := range open {
		add(f.OrderID, util.DayOf(f.Delivery.Slot.Date), f.TotalPrice)
	}
	for _, o := range closed {
		add(o.OrderID, o.Date, o.TotalPrice)
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Date > refs[j].Date })
	return refs, nil
}

// OrderDetailsResult pairs an order ref with its fetched details.
type OrderDetailsResult struct {
	Ref   OrderRef
	Order *appie.Order
	Err   error
}

// FetchOrderDetails fetches details for refs with at most 4 concurrent
// requests, preserving order. A ref without a date gets the delivery date
// from the details.
func FetchOrderDetails(ctx context.Context, c *appie.Client, refs []OrderRef) []OrderDetailsResult {
	out := make([]OrderDetailsResult, len(refs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, ref := range refs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			o, date, err := GetOrderDetails(ctx, c, ref.OrderID)
			if ref.Date == "" {
				ref.Date = date
			}
			out[i] = OrderDetailsResult{Ref: ref, Order: o, Err: err}
		}()
	}
	wg.Wait()
	return out
}

// FrequentItem is a product bought across several orders.
type FrequentItem struct {
	ProductID   int     `json:"product_id"`
	Name        string  `json:"name"`
	OrderCount  int     `json:"order_count"`
	TotalQty    int     `json:"total_quantity"`
	LastOrdered string  `json:"last_ordered,omitempty"`
	LastPrice   float64 `json:"last_price,omitempty"`
}

// FrequentItems counts, per product, how many of the given orders contain
// it. Orders whose details failed are reported in failed.
func FrequentItems(results []OrderDetailsResult) (items []FrequentItem, failed int) {
	stats := map[int]*FrequentItem{}
	for _, r := range results {
		if r.Err != nil || r.Order == nil {
			failed++
			continue
		}
		seen := map[int]bool{}
		for _, it := range r.Order.Items {
			pid := it.ProductID
			if pid == 0 {
				continue
			}
			st := stats[pid]
			if st == nil {
				st = &FrequentItem{ProductID: pid}
				stats[pid] = st
			}
			st.TotalQty += it.Quantity
			if !seen[pid] {
				seen[pid] = true
				st.OrderCount++
			}
			if r.Ref.Date >= st.LastOrdered {
				st.LastOrdered = r.Ref.Date
				if it.Product != nil {
					if it.Product.Title != "" {
						st.Name = it.Product.Title
					}
					st.LastPrice = it.Product.Price.Now
				}
			}
		}
	}
	items = make([]FrequentItem, 0, len(stats))
	for _, st := range stats {
		if st.Name == "" {
			st.Name = fmt.Sprintf("product %d", st.ProductID)
		}
		items = append(items, *st)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].OrderCount != items[j].OrderCount {
			return items[i].OrderCount > items[j].OrderCount
		}
		if items[i].LastOrdered != items[j].LastOrdered {
			return items[i].LastOrdered > items[j].LastOrdered
		}
		return items[i].ProductID < items[j].ProductID
	})
	return items, failed
}
