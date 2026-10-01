// Copyright (C) the bramboe/ah-mcp contributors
// Copyright (C) Igor Fedorov and contributors
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// The receipt-savings part is adapted from github.com/bramboe/ah-mcp/tools/savings.go
// (discountCategory, fetchReceiptStats, ah_get_savings_summary aggregation)
// @ 46b85bc652496590e179a665dda346b206d56291 (AGPL-3.0).
//
// Merging in-store receipts with delivered online orders is an idea from
// wmeints/happie (no code copied).

package ah

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/fedorov-xyz/ah-mcp/internal/util"

	appie "github.com/gwillem/appie-go"
)

// Detail-fetch caps keep history tools responsive (one request per receipt
// or order). Tools must report when a cap was hit.
const (
	MaxHistoryReceipts = 200
	MaxHistoryOrders   = 100
)

// ReceiptDetailsResult pairs a receipt summary with its details.
type ReceiptDetailsResult struct {
	Summary appie.Receipt
	Detail  *appie.Receipt
	Err     error
}

// FetchReceiptDetails fetches details (including POS → webshop id mapping
// done by appie-go's GetReceipt via ConvertPOSIDs) with at most 4
// concurrent requests.
func FetchReceiptDetails(ctx context.Context, c *appie.Client, receipts []appie.Receipt) []ReceiptDetailsResult {
	out := make([]ReceiptDetailsResult, len(receipts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, r := range receipts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, err := c.GetReceipt(ctx, r.TransactionID)
			out[i] = ReceiptDetailsResult{Summary: r, Detail: d, Err: err}
		}()
	}
	wg.Wait()
	return out
}

// HistoryProduct aggregates one product across receipts and orders.
type HistoryProduct struct {
	ProductID  int      `json:"product_id,omitempty"`
	Name       string   `json:"name"`
	Purchases  int      `json:"purchases"`
	Quantity   int      `json:"quantity"`
	Spend      float64  `json:"spend"`
	LastBought string   `json:"last_bought"`
	Sources    []string `json:"sources"`
	nameFromWS bool
}

// MergeHistory merges receipt and order details into per-product stats.
// Products are keyed by webshop id when known: receipt POS ids are mapped by
// ConvertPOSIDs, and a mapping seen on any receipt is applied to all of them.
// Unmapped receipt lines are keyed by their normalised description, because
// the same product can carry different POS ids.
func MergeHistory(receipts []ReceiptDetailsResult, orders []OrderDetailsResult) (products []HistoryProduct, failed int) {
	byKey := map[string]*HistoryProduct{}
	add := func(key string, pid int, name string, wsName bool, qty int, spend float64, day, source string) {
		p := byKey[key]
		if p == nil {
			p = &HistoryProduct{ProductID: pid}
			byKey[key] = p
		}
		p.Purchases++
		p.Quantity += qty
		p.Spend += spend
		if day > p.LastBought {
			p.LastBought = day
		}
		if name != "" && (p.Name == "" || (wsName && !p.nameFromWS)) {
			p.Name, p.nameFromWS = name, wsName
		}
		if !slices.Contains(p.Sources, source) {
			p.Sources = append(p.Sources, source)
		}
	}
	posToWS := map[int]int{}
	for _, r := range receipts {
		if r.Detail == nil {
			continue
		}
		for _, it := range r.Detail.Items {
			if it.ProductID > 0 && it.WebshopID > 0 {
				posToWS[it.ProductID] = it.WebshopID
			}
		}
	}
	for _, r := range receipts {
		if r.Err != nil || r.Detail == nil {
			failed++
			continue
		}
		day := util.DayOf(r.Summary.Date)
		for _, it := range r.Detail.Items {
			if it.Description == "" && it.ProductID == 0 {
				continue
			}
			if it.WebshopID == 0 {
				it.WebshopID = posToWS[it.ProductID]
			}
			add(receiptKey(it), it.WebshopID, it.Description, false, max(it.Quantity, 1), it.Amount, day, "receipt")
		}
	}
	for _, o := range orders {
		if o.Err != nil || o.Order == nil {
			failed++
			continue
		}
		for _, it := range o.Order.Items {
			if it.ProductID == 0 {
				continue
			}
			var name string
			var price float64
			if it.Product != nil {
				name, price = it.Product.Title, it.Product.Price.Now
			}
			spend := util.Round2(price * float64(it.Quantity))
			add(fmt.Sprintf("ws:%d", it.ProductID), it.ProductID, name, true, it.Quantity, spend, o.Ref.Date, "online_order")
		}
	}
	products = make([]HistoryProduct, 0, len(byKey))
	for _, p := range byKey {
		p.Spend = util.Round2(p.Spend)
		products = append(products, *p)
	}
	sort.Slice(products, func(i, j int) bool {
		if products[i].Purchases != products[j].Purchases {
			return products[i].Purchases > products[j].Purchases
		}
		if products[i].Spend != products[j].Spend {
			return products[i].Spend > products[j].Spend
		}
		return products[i].Name < products[j].Name
	})
	return products, failed
}

func receiptKey(it appie.ReceiptItem) string {
	if it.WebshopID > 0 {
		return fmt.Sprintf("ws:%d", it.WebshopID)
	}
	if name := strings.Join(strings.Fields(strings.ToLower(it.Description)), " "); name != "" {
		return "name:" + name
	}
	return fmt.Sprintf("pos:%d", it.ProductID)
}

// DiscountCategory classifies a receipt discount line by its name. AH
// names bonus discounts after the product/segment, premium discounts
// "bio premium", miles redemptions "MIJN AH MILES", and koopzegel
// redemptions contain "KOOPZEGEL".
func DiscountCategory(name string) string {
	upper := strings.ToUpper(name)
	switch {
	case strings.Contains(upper, "KOOPZEGEL"):
		return "koopzegels"
	case strings.Contains(upper, "MILES"):
		return "miles"
	case strings.Contains(upper, "PREMIUM"):
		return "premium"
	default:
		return "bonus"
	}
}

// MonthStat is one calendar month of purchases.
type MonthStat struct {
	Month        string  `json:"month"`
	Receipts     int     `json:"receipts"`
	ReceiptSpend float64 `json:"receipt_spend"`
	OnlineOrders int     `json:"online_orders"`
	OnlineSpend  float64 `json:"online_spend"`
	Purchases    int     `json:"purchases"`
	TotalSpend   float64 `json:"total_spend"`
}

// ByMonth aggregates receipts and online orders per calendar month, newest
// first, from the list totals (no detail requests needed). Orders with an
// unknown date fall into month "unknown".
func ByMonth(receipts []appie.Receipt, orders []OrderRef) []MonthStat {
	stats := map[string]*MonthStat{}
	get := func(m string) *MonthStat {
		if m == "" {
			m = "unknown"
		}
		if stats[m] == nil {
			stats[m] = &MonthStat{Month: m}
		}
		return stats[m]
	}
	for _, r := range receipts {
		s := get(util.MonthOf(r.Date))
		s.Receipts++
		s.ReceiptSpend += r.TotalAmount
	}
	for _, o := range orders {
		s := get(util.MonthOf(o.Date))
		s.OnlineOrders++
		s.OnlineSpend += o.Total
	}
	out := make([]MonthStat, 0, len(stats))
	for _, s := range stats {
		s.ReceiptSpend, s.OnlineSpend = util.Round2(s.ReceiptSpend), util.Round2(s.OnlineSpend)
		s.Purchases = s.Receipts + s.OnlineOrders
		s.TotalSpend = util.Round2(s.ReceiptSpend + s.OnlineSpend)
		out = append(out, *s)
	}
	// Newest first; "unknown" last.
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Month == "unknown") != (out[j].Month == "unknown") {
			return out[j].Month == "unknown"
		}
		return out[i].Month > out[j].Month
	})
	return out
}
