// Copyright (C) the gwillem/appie-go contributors
// Copyright (C) Igor Fedorov and contributors
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Adapted from github.com/gwillem/appie-go/order.go (orderDetailsResponse,
// toOrder) and receipts.go (fetchPosReceiptsQuery)
// @ 65f41abdeaffb4b990cc35525cc0f20837102e1c (AGPL-3.0).
//
// Changes: the order details and the active order summary keep the delivery
// date and window (appie-go's Order drops them), and receipts are paged past
// appie-go's fixed first page of 100.

package ah

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	appie "github.com/gwillem/appie-go"

	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

type orderDetailsResp struct {
	OrderID         int    `json:"orderId"`
	OrderState      string `json:"orderState"`
	DeliveryDate    string `json:"deliveryDate"`
	GroupedProducts []struct {
		OrderedProducts []struct {
			Quantity int `json:"quantity"`
			Product  struct {
				WebshopID        int      `json:"webshopId"`
				Title            string   `json:"title"`
				Brand            string   `json:"brand"`
				SalesUnitSize    string   `json:"salesUnitSize"`
				PriceBeforeBonus float64  `json:"priceBeforeBonus"`
				CurrentPrice     *float64 `json:"currentPrice"`
				IsBonus          bool     `json:"isBonus"`
				BonusMechanism   string   `json:"bonusMechanism"`
			} `json:"product"`
		} `json:"orderedProducts"`
	} `json:"groupedProductsInTaxonomy"`
}

// GetOrderDetails fetches one order's products (as appie-go's
// GetOrderDetails does) plus its delivery date (YYYY-MM-DD, "" if absent).
func GetOrderDetails(ctx context.Context, c GraphQLDoer, orderID int) (*appie.Order, string, error) {
	var r orderDetailsResp
	path := fmt.Sprintf("/mobile-services/order/v1/%d/details-grouped-by-taxonomy", orderID)
	if err := c.DoRequest(ctx, http.MethodGet, path, nil, &r); err != nil {
		return nil, "", fmt.Errorf("get order details: %w", err)
	}
	var items []appie.OrderItem
	for _, g := range r.GroupedProducts {
		for _, op := range g.OrderedProducts {
			p := op.Product
			price := appie.Price{Now: p.PriceBeforeBonus}
			if p.CurrentPrice != nil {
				price = appie.Price{Now: *p.CurrentPrice, Was: p.PriceBeforeBonus}
			}
			items = append(items, appie.OrderItem{
				ProductID: p.WebshopID,
				Quantity:  op.Quantity,
				Product: &appie.Product{ID: p.WebshopID, Title: p.Title, Brand: p.Brand, UnitSize: p.SalesUnitSize,
					Price: price, IsBonus: p.IsBonus, BonusMechanism: p.BonusMechanism},
			})
		}
	}
	o := &appie.Order{ID: fmt.Sprint(r.OrderID), State: r.OrderState, Items: items, TotalCount: len(items)}
	return o, util.DayOf(r.DeliveryDate), nil
}

const receiptsPageQuery = `query FetchPosReceipts($offset: Int!, $limit: Int!) {
	posReceiptsPage(pagination: {offset: $offset, limit: $limit}) {
		posReceipts {
			id
			dateTime
			totalAmount {
				amount
			}
		}
	}
}`

// Receipt paging limits.
const (
	receiptsPageSize = 100
	maxReceiptPages  = 20
)

// receiptsPage fetches one page of receipts, newest first.
func receiptsPage(ctx context.Context, c GraphQLDoer, page int) ([]appie.Receipt, error) {
	var resp struct {
		PosReceiptsPage struct {
			PosReceipts []struct {
				ID          string `json:"id"`
				DateTime    string `json:"dateTime"`
				TotalAmount Money  `json:"totalAmount"`
			} `json:"posReceipts"`
		} `json:"posReceiptsPage"`
	}
	vars := map[string]any{"offset": page * receiptsPageSize, "limit": receiptsPageSize}
	if err := c.DoGraphQL(ctx, receiptsPageQuery, vars, &resp); err != nil {
		return nil, fmt.Errorf("get receipts: %w", err)
	}
	out := make([]appie.Receipt, 0, len(resp.PosReceiptsPage.PosReceipts))
	for _, r := range resp.PosReceiptsPage.PosReceipts {
		out = append(out, appie.Receipt{TransactionID: r.ID, Date: r.DateTime, TotalAmount: r.TotalAmount.Amount})
	}
	return out, nil
}

// RecentReceipts lists the n newest receipts; more reports that older ones
// exist. Unlike appie-go's GetReceipts it is not capped at the first 100.
func RecentReceipts(ctx context.Context, c GraphQLDoer, n int) (receipts []appie.Receipt, more bool, err error) {
	seen := map[string]bool{}
	for page := 0; page < maxReceiptPages; page++ {
		rs, err := receiptsPage(ctx, c, page)
		if err != nil {
			return nil, false, err
		}
		for _, r := range rs {
			if seen[r.TransactionID] {
				continue
			}
			if len(receipts) == n {
				return receipts, true, nil
			}
			seen[r.TransactionID] = true
			receipts = append(receipts, r)
		}
		if len(rs) < receiptsPageSize {
			break
		}
	}
	return receipts, false, nil
}

// ReceiptsBetween lists the receipts whose day is in [from, to], newest
// first, paging through posReceiptsPage until a page holds nothing on or
// after from. complete is false when the page budget ran out first.
//
// Offsets beyond the first page (appie-go only ever asks for offset 0) were
// verified against the live API on 2026-10-01 (170 receipts in one window).
func ReceiptsBetween(ctx context.Context, c GraphQLDoer, from, to string) (receipts []appie.Receipt, complete bool, err error) {
	seen := map[string]bool{}
	for page := 0; page < maxReceiptPages; page++ {
		rs, err := receiptsPage(ctx, c, page)
		if err != nil {
			return nil, false, err
		}
		reachedWindow := false
		for _, r := range rs {
			day := util.DayOf(r.Date)
			if day >= from {
				reachedWindow = true
			}
			if day >= from && day <= to && !seen[r.TransactionID] {
				seen[r.TransactionID] = true
				receipts = append(receipts, r)
			}
		}
		if len(rs) < receiptsPageSize || !reachedWindow {
			complete = true
			break
		}
	}
	sort.SliceStable(receipts, func(i, j int) bool { return receipts[i].Date > receipts[j].Date })
	return receipts, complete, nil
}

// ActiveOrder is the active (not yet submitted) online order.
type ActiveOrder struct {
	ID             string
	State          string
	ShoppingType   string
	Items          []appie.OrderItem
	Total          float64 // payable
	Discount       float64
	BeforeDiscount float64
	DeliveryDate   string // YYYY-MM-DD
	DeliveryStart  string // HH:MM
	DeliveryEnd    string // HH:MM
}

// GetActiveOrder reads the active order summary, like appie-go's GetOrder,
// keeping the delivery information, and primes c's current-order header for
// the order write path (as GetOrder does).
//
// UNVERIFIED: the deliveryInformation time format (decoded as appie-go does,
// but not yet observed live).
func GetActiveOrder(ctx context.Context, c *appie.Client) (*ActiveOrder, error) {
	var r struct {
		ID           int    `json:"id"`
		State        string `json:"state"`
		ShoppingType string `json:"shoppingType"`
		TotalPrice   struct {
			PriceBeforeDiscount float64 `json:"priceBeforeDiscount"`
			PriceDiscount       float64 `json:"priceDiscount"`
			PriceTotalPayable   float64 `json:"priceTotalPayable"`
		} `json:"totalPrice"`
		DeliveryInformation struct {
			DeliveryDate      string `json:"deliveryDate"`
			DeliveryStartTime string `json:"deliveryStartTime"`
			DeliveryEndTime   string `json:"deliveryEndTime"`
		} `json:"deliveryInformation"`
		OrderedProducts []struct {
			Quantity int `json:"quantity"`
			Product  struct {
				WebshopID int    `json:"webshopId"`
				Title     string `json:"title"`
				Brand     string `json:"brand"`
			} `json:"product"`
		} `json:"orderedProducts"`
	}
	if err := c.DoRequest(ctx, http.MethodGet, "/mobile-services/order/v1/summaries/active?sortBy=DEFAULT", nil, &r); err != nil {
		return nil, fmt.Errorf("get active order: %w", err)
	}
	c.SetOrderID(r.ID)
	o := &ActiveOrder{
		ID: fmt.Sprint(r.ID), State: r.State, ShoppingType: r.ShoppingType,
		Total: r.TotalPrice.PriceTotalPayable, Discount: r.TotalPrice.PriceDiscount, BeforeDiscount: r.TotalPrice.PriceBeforeDiscount,
		DeliveryDate:  util.DayOf(r.DeliveryInformation.DeliveryDate),
		DeliveryStart: util.TimeOfDay(r.DeliveryInformation.DeliveryStartTime),
		DeliveryEnd:   util.TimeOfDay(r.DeliveryInformation.DeliveryEndTime),
	}
	for _, op := range r.OrderedProducts {
		o.Items = append(o.Items, appie.OrderItem{ProductID: op.Product.WebshopID, Quantity: op.Quantity,
			Product: &appie.Product{ID: op.Product.WebshopID, Title: op.Product.Title, Brand: op.Product.Brand}})
	}
	return o, nil
}
