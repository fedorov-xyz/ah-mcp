package tools

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/fedorov-xyz/ah-mcp/internal/ah"
	"github.com/fedorov-xyz/ah-mcp/internal/cache"
	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

type periods struct {
	Current, Next *ah.Period
}

// period resolves "current" or "next". It returns (nil, unavailable
// result) when AH has not published the requested week.
func (d *Deps) period(c *call, which string) (p *ah.Period, current bool, unavailable *mcp.CallToolResult, fail *mcp.CallToolResult) {
	if which == "" {
		which = "current"
	}
	if which != "current" && which != "next" {
		return nil, false, nil, invalid(`period must be "current" or "next"`)
	}
	today := d.today().Format(day)
	ps, err := cache.GetOrLoad(d.Cache, cache.Global, "bonus:periods:"+today, cache.TTLBonus, func() (periods, error) {
		cur, next, err := ah.BonusPeriods(c.ctx, c.c, today)
		return periods{Current: cur, Next: next}, err
	})
	if err != nil {
		return nil, false, nil, d.upstream(c, "get bonus periods", err)
	}
	p = ps.Current
	if which == "next" {
		p = ps.Next
	}
	if p == nil {
		data := map[string]any{"available": false, "period": which, "offers": []any{}}
		msg := fmt.Sprintf("No %s bonus period is published yet.", which)
		if which == "next" && ps.Current != nil && ps.Current.NextVisibleFrom != "" {
			data["visible_from"] = ps.Current.NextVisibleFrom
			msg = "Next week's bonus is not published yet; AH publishes it from " + ps.Current.NextVisibleFrom + "."
		}
		data["message"] = msg
		return nil, false, ok(msg, data), nil
	}
	return p, which == "current", nil, nil
}

func (d *Deps) periodOffers(c *call, p *ah.Period) ([]ah.Offer, error) {
	return cache.GetOrLoad(d.Cache, cache.Global, "bonus:offers:"+p.Start, cache.TTLBonus, func() ([]ah.Offer, error) {
		return ah.PeriodOffers(c.ctx, c.c, p)
	})
}

func matchesKeyword(o ah.Offer, kw string) bool {
	if kw == "" {
		return true
	}
	kw = strings.ToLower(kw)
	return strings.Contains(strings.ToLower(o.Title), kw) || strings.Contains(strings.ToLower(o.Category), kw) ||
		strings.Contains(strings.ToLower(o.Mechanism), kw)
}

func periodParam() mcp.ToolOption {
	return mcp.WithString("period", mcp.Description(`Bonus week: "current" (default) or "next" (published a few days ahead, usually from the weekend).`), mcp.Enum("current", "next"))
}

func (d *Deps) bonusOffersTool() server.ServerTool {
	tool := mcp.NewTool("ah_bonus_offers",
		mcp.WithDescription("List national Albert Heijn bonus offers for the current or next bonus week, optionally filtered by keyword. "+
			"Each offer has base_price, bonus_price, discount_pct, mechanism, computed price tiers (e.g. \"2+3 gratis\": pay 2 of 5) and the period dates. "+
			"conditions lists AH's exceptions and instructions (e.g. \"*M.u.v. Biologisch\" = organic excluded); store_only=true deals do NOT apply to online orders; "+
			"stapel_bonus=true means the discount grows with the quantity. AH does not say whether different promotions can be combined on one product — do not assume they can. "+
			"Group deals have a segment_id: pass it to ah_bonus_group to see the products. Personal (Bonus Box) offers are in ah_personal_bonus."),
		periodParam(),
		mcp.WithString("keyword", mcp.Description("Case-insensitive filter on title, category and mechanism, e.g. \"kaas\".")),
		limitParam(),
		readOnly("Albert Heijn: Bonus Offers"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Period  string `json:"period"`
			Keyword string `json:"keyword"`
			Limit   *int   `json:"limit"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		limit, r := limitOf(a.Limit)
		if r != nil {
			return r, nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		p, _, unavailable, fail := d.period(c, a.Period)
		if fail != nil || unavailable != nil {
			return util.FirstNonNil(fail, unavailable), nil
		}
		offers, err := d.periodOffers(c, p)
		if err != nil {
			return d.upstream(c, "get bonus offers", err), nil
		}
		var filtered []ah.Offer
		for _, o := range offers {
			if matchesKeyword(o, strings.TrimSpace(a.Keyword)) {
				filtered = append(filtered, o)
			}
		}
		total := len(filtered)
		filtered, truncated := util.Truncate(filtered, limit)
		data := map[string]any{
			"available": true, "period": util.Or(a.Period, "current"), "period_start": p.Start, "period_end": p.End,
			"currency": "EUR", "total_matching": total, "count": len(filtered), "truncated": truncated, "offers": filtered,
		}
		return ok(fmt.Sprintf("%d bonus offer(s) for %s–%s.", total, p.Start, p.End), data), nil
	}}
}

func (d *Deps) bonusGroupTool() server.ServerTool {
	tool := mcp.NewTool("ah_bonus_group",
		mcp.WithDescription("List the individual products in a bonus group deal (e.g. \"Alle AH yoghurt 25% korting\"). "+
			"Get segment_id from ah_bonus_offers. Use the same period as the offer. bonus_price is omitted when AH keeps the per-piece price and applies the deal "+
			"(e.g. \"1+1 gratis\") at checkout — the mechanism then says what you get."),
		mcp.WithString("segment_id", mcp.Required(), mcp.Description("Bonus segment id (segment_id from ah_bonus_offers)."), mcp.MinLength(1)),
		periodParam(),
		limitParam(),
		readOnly("Albert Heijn: Bonus Group Products"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			SegmentID string `json:"segment_id"`
			Period    string `json:"period"`
			Limit     *int   `json:"limit"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		a.SegmentID = strings.TrimSpace(a.SegmentID)
		if a.SegmentID == "" || len(a.SegmentID) > 64 {
			return invalid("segment_id is required"), nil
		}
		limit, r := limitOf(a.Limit)
		if r != nil {
			return r, nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		p, current, unavailable, fail := d.period(c, a.Period)
		if fail != nil || unavailable != nil {
			return util.FirstNonNil(fail, unavailable), nil
		}
		products, err := cache.GetOrLoad(d.Cache, cache.Global, "bonus:group:"+a.SegmentID+":"+p.Start, cache.TTLBonus, func() ([]ah.GroupProduct, error) {
			return ah.GroupProducts(ctx, c.c, a.SegmentID, p, current)
		})
		if err != nil {
			return d.upstream(c, "get bonus group", err), nil
		}
		if len(products) == 0 {
			return toolError(codeNotFound, "bonus group "+a.SegmentID+" has no products in this period", nil), nil
		}
		products, truncated := util.Truncate(products, limit)
		data := map[string]any{"segment_id": a.SegmentID, "period_start": p.Start, "period_end": p.End, "currency": "EUR",
			"count": len(products), "truncated": truncated, "products": products}
		return ok(fmt.Sprintf("%d product(s) in bonus group %s.", len(products), a.SegmentID), data), nil
	}}
}

func (d *Deps) personalOffers(c *call, p *ah.Period) ([]ah.Offer, error) {
	return cache.GetOrLoad(d.Cache, c.ns, "personal:"+p.Start, cache.TTLBonus, func() ([]ah.Offer, error) {
		return ah.PersonalOffers(c.ctx, c.c, p.Start)
	})
}

func (d *Deps) personalBonusTool() server.ServerTool {
	tool := mcp.NewTool("ah_personal_bonus",
		mcp.WithDescription("List this member's personal Bonus Box offers for the current or next bonus week, each with activation_status (e.g. ACTIVATABLE, ACTIVATED). "+
			"Read-only: this server cannot activate offers — tell the user to activate them in the AH app."),
		periodParam(),
		limitParam(),
		readOnly("Albert Heijn: Personal Bonus (Bonus Box)"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Period string `json:"period"`
			Limit  *int   `json:"limit"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		limit, r := limitOf(a.Limit)
		if r != nil {
			return r, nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		p, _, unavailable, fail := d.period(c, a.Period)
		if fail != nil || unavailable != nil {
			return util.FirstNonNil(fail, unavailable), nil
		}
		offers, err := d.personalOffers(c, p)
		if err != nil {
			return d.upstream(c, "get personal bonus", err), nil
		}
		counts := map[string]int{}
		for _, o := range offers {
			counts[o.ActivationStatus]++
		}
		offers, truncated := util.Truncate(offers, limit)
		data := map[string]any{"available": true, "period_start": p.Start, "period_end": p.End, "currency": "EUR", "read_only": true,
			"status_counts": counts, "count": len(offers), "truncated": truncated, "offers": offers}
		return ok(fmt.Sprintf("%d personal offer(s); activate them in the AH app.", len(offers)), data), nil
	}}
}

// frequent returns frequent items over the last maxOrders orders (cached
// per member).
func (d *Deps) frequent(c *call, maxOrders int) (frequentResult, error) {
	// Only results without failed order details are cached.
	return cache.GetOrLoadIf(d.Cache, c.ns, fmt.Sprintf("frequent:%d", maxOrders), cache.TTLPersonal, func() (frequentResult, error) {
		refs, err := ah.RecentOrders(c.ctx, c.c, maxOrders)
		if err != nil {
			return frequentResult{}, err
		}
		items, failed := ah.FrequentItems(ah.FetchOrderDetails(c.ctx, c.c, refs))
		return frequentResult{Items: items, Orders: len(refs), Failed: failed}, nil
	}, func(r frequentResult) bool { return r.Failed == 0 })
}

type frequentResult struct {
	Items  []ah.FrequentItem
	Orders int
	Failed int
}

func (d *Deps) bonusForHabitsTool() server.ServerTool {
	tool := mcp.NewTool("ah_bonus_for_habits",
		mcp.WithDescription("Find which of the member's usual products are on bonus in the current or next week, to build a cheaper basket. "+
			"Usual products come from the purchase history of the last `days` days: in-store receipts and delivered online orders (as ah_purchase_history). "+
			"Matches by product id first, then by title keywords (match_type and matched_on show how — title matches are suggestions, verify them). "+
			"Personal Bonus Box offers are included for information only (personal=true); they cannot be activated here."),
		periodParam(),
		mcp.WithInteger("days", mcp.Description("Purchase history window in days (default 90, max 365)."), mcp.Min(1), mcp.Max(365)),
		mcp.WithInteger("min_purchases", mcp.Description("Only consider products bought at least this many times — receipts and orders (default 2)."), mcp.Min(1), mcp.Max(365)),
		limitParam(),
		readOnly("Albert Heijn: Bonus for My Habits"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Period       string `json:"period"`
			Days         *int   `json:"days"`
			MinPurchases *int   `json:"min_purchases"`
			Limit        *int   `json:"limit"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		limit, r := limitOf(a.Limit)
		if r != nil {
			return r, nil
		}
		days, minPurchases := 90, 2
		if a.Days != nil {
			if *a.Days < 1 || *a.Days > 365 {
				return invalid("days must be between 1 and 365"), nil
			}
			days = *a.Days
		}
		if a.MinPurchases != nil {
			if *a.MinPurchases < 1 || *a.MinPurchases > 365 {
				return invalid("min_purchases must be between 1 and 365"), nil
			}
			minPurchases = *a.MinPurchases
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		p, _, unavailable, fail := d.period(c, a.Period)
		if fail != nil || unavailable != nil {
			return util.FirstNonNil(fail, unavailable), nil
		}
		offers, err := d.periodOffers(c, p)
		if err != nil {
			return d.upstream(c, "get bonus offers", err), nil
		}
		personal, perr := d.personalOffers(c, p)
		hist, fs, ts, err := d.history(c, days)
		if err != nil {
			return d.upstream(c, "get purchase history", err), nil
		}
		matches := matchHabits(hist.Products, offers, personal, minPurchases)
		saving, ifActivated := habitSavings(matches)
		total := len(matches)
		matches, truncated := util.Truncate(matches, limit)
		data := map[string]any{
			"period_start": p.Start, "period_end": p.End, "currency": "EUR",
			"history_from": fs, "history_to": ts, "history_sources": hist.Sources, "history_coverage": hist.Coverage,
			"products_considered": countAtLeast(hist.Products, minPurchases),
			"personal_offers":     availability(perr), "total_matches": total, "count": len(matches), "truncated": truncated,
			"estimated_saving_exact_matches": util.Round2(saving), "extra_saving_if_personal_offers_activated": util.Round2(ifActivated), "matches": matches,
		}
		summary := fmt.Sprintf("%d of your usual products match bonus offers in %s–%s.", total, p.Start, p.End)
		if !hist.Coverage.Complete {
			summary += " The purchase history is incomplete (see history_coverage); use a shorter days window for a complete one."
		}
		return ok(summary, data), nil
	}}
}

type habitMatch struct {
	ProductID     int      `json:"product_id,omitempty"`
	Name          string   `json:"name"`
	Purchases     int      `json:"purchases"`
	LastBought    string   `json:"last_bought,omitempty"`
	Sources       []string `json:"sources,omitempty"`
	MatchType     string   `json:"match_type"`
	MatchedOn     []string `json:"matched_on,omitempty"`
	Personal      bool     `json:"personal"`
	SavingPerItem float64  `json:"saving_per_item,omitempty"`
	Offer         ah.Offer `json:"offer"`
}

// matchHabits pairs products bought at least minPurchases times with the
// best offer: an exact product id match wins, otherwise the title match
// sharing most tokens (see ah.OfferTitleMatch; on a tie, an offer whose
// whole title is in the product's wins).
func matchHabits(items []ah.HistoryProduct, offers, personal []ah.Offer, minPurchases int) []habitMatch {
	type sourced struct {
		o        ah.Offer
		personal bool
		tokens   map[string]bool
	}
	all := make([]sourced, 0, len(offers)+len(personal))
	for _, o := range offers {
		all = append(all, sourced{o: o, tokens: ah.TitleTokens(o.Title)})
	}
	for _, o := range personal {
		all = append(all, sourced{o: o, personal: true, tokens: ah.TitleTokens(o.Title)})
	}
	var out []habitMatch
	for _, it := range items {
		if it.Purchases < minPurchases {
			continue
		}
		pt := ah.TitleTokens(it.Name)
		best, bestType := -1, ""
		var bestShared []string
		bestScore := 0
		for i, s := range all {
			if s.o.ProductID != 0 && s.o.ProductID == it.ProductID {
				best, bestType, bestShared = i, "product_id", nil
				break
			}
			shared, subset, ok := ah.OfferTitleMatch(pt, s.tokens)
			if !ok {
				continue
			}
			score := 2 * len(shared)
			if subset {
				score++
			}
			if score > bestScore {
				best, bestScore, bestType, bestShared = i, score, "title", shared
			}
		}
		if best < 0 {
			continue
		}
		s := all[best]
		m := habitMatch{ProductID: it.ProductID, Name: it.Name, Purchases: it.Purchases, LastBought: it.LastBought, Sources: it.Sources,
			MatchType: bestType, MatchedOn: bestShared, Personal: s.personal, Offer: s.o}
		if s.o.BasePrice > 0 && s.o.BonusPrice > 0 && s.o.BonusPrice < s.o.BasePrice {
			m.SavingPerItem = util.Round2(s.o.BasePrice - s.o.BonusPrice)
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].MatchType == "product_id") != (out[j].MatchType == "product_id") {
			return out[i].MatchType == "product_id"
		}
		return out[i].Purchases > out[j].Purchases
	})
	return out
}

func countAtLeast(items []ah.HistoryProduct, n int) int {
	c := 0
	for _, it := range items {
		if it.Purchases >= n {
			c++
		}
	}
	return c
}

func availability(err error) string {
	if err != nil {
		return "unavailable"
	}
	return "available"
}

func (d *Deps) savingsSummaryTool() server.ServerTool {
	tool := mcp.NewTool("ah_savings_summary",
		mcp.WithDescription("Summarise how much the member saved with bonus and other discounts in a date range (default: current calendar month): "+
			"discounts on in-store receipts (by category: bonus, premium, miles, koopzegels) plus order-level discounts of delivered online orders. "+
			"Each source is reported as available or unavailable; totals only cover available sources."),
		mcp.WithString("from", mcp.Description("Start date YYYY-MM-DD (inclusive). Default: first day of this month.")),
		mcp.WithString("to", mcp.Description("End date YYYY-MM-DD (inclusive). Default: today.")),
		readOnly("Albert Heijn: Savings Summary"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		today := d.today()
		from := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, amsterdam)
		to := today
		var err error
		if a.From != "" {
			if from, err = time.ParseInLocation(day, a.From, amsterdam); err != nil {
				return invalid("from must be YYYY-MM-DD"), nil
			}
		}
		if a.To != "" {
			if to, err = time.ParseInLocation(day, a.To, amsterdam); err != nil {
				return invalid("to must be YYYY-MM-DD"), nil
			}
		}
		if from.After(to) {
			return invalid("from must not be after to"), nil
		}
		if to.Sub(from) > 366*24*time.Hour {
			return invalid("the range may span at most one year"), nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		fs, ts := from.Format(day), to.Format(day)

		receiptsSrc := map[string]any{"status": "available"}
		byCategory := map[string]float64{}
		var receiptDiscount, receiptPaid float64
		receipts, listComplete, rerr := ah.ReceiptsBetween(ctx, c.c, fs, ts)
		if rerr == nil {
			capped, truncated := util.Truncate(receipts, ah.MaxHistoryReceipts)
			receiptsSrc["receipts_in_window"] = len(receipts)
			receiptsSrc["receipt_list_complete"] = listComplete
			failed := 0
			for _, rd := range ah.FetchReceiptDetails(ctx, c.c, capped) {
				if rd.Err != nil || rd.Detail == nil {
					failed++
					continue
				}
				receiptPaid += rd.Summary.TotalAmount
				for _, dl := range rd.Detail.Discounts {
					amt := math.Abs(dl.Amount)
					receiptDiscount += amt
					byCategory[ah.DiscountCategory(dl.Name)] += amt
				}
			}
			receiptsSrc["receipts"] = len(capped) - failed
			receiptsSrc["failed"] = failed
			receiptsSrc["truncated"] = truncated
			receiptsSrc["total_paid"] = util.Round2(receiptPaid)
			receiptsSrc["discount"] = util.Round2(receiptDiscount)
		} else {
			receiptsSrc = map[string]any{"status": "unavailable", "error": d.upstream(c, "get receipts", rerr).StructuredContent}
		}

		onlineSrc := map[string]any{"status": "available"}
		var onlineDiscount, onlinePaid float64
		closed, oerr := ah.FetchClosedOrders(ctx, c.c)
		if oerr == nil {
			n := 0
			for _, o := range closed {
				if o.Date >= fs && o.Date <= ts {
					n++
					onlineDiscount += o.Discount
					onlinePaid += o.TotalPrice
				}
			}
			onlineSrc["orders"] = n
			onlineSrc["total_paid"] = util.Round2(onlinePaid)
			onlineSrc["discount"] = util.Round2(onlineDiscount)
		} else {
			onlineSrc = map[string]any{"status": "unavailable", "error": d.upstream(c, "get past orders", oerr).StructuredContent}
		}
		if rerr != nil && oerr != nil {
			return d.upstream(c, "savings summary", rerr), nil
		}
		for k, v := range byCategory {
			byCategory[k] = util.Round2(v)
		}
		if onlineDiscount > 0 {
			byCategory["online_orders"] = util.Round2(onlineDiscount)
		}
		totalDiscount := receiptDiscount + onlineDiscount
		totalPaid := receiptPaid + onlinePaid
		pct := 0.0
		if gross := totalPaid + totalDiscount; gross > 0 {
			pct = util.Round2(totalDiscount / gross * 100)
		}
		data := map[string]any{
			"from": fs, "to": ts, "currency": "EUR",
			"total_paid": util.Round2(totalPaid), "total_discount": util.Round2(totalDiscount), "discount_pct": pct,
			"discount_by_category": byCategory,
			"sources":              map[string]any{"receipts": receiptsSrc, "online_orders": onlineSrc},
		}
		return ok(fmt.Sprintf("Saved €%.2f between %s and %s.", util.Round2(totalDiscount), fs, ts), data), nil
	}}
}

// habitSavings sums the per-item saving of exact (product id) matches.
// Personal offers count once activated; those still to be activated are
// returned separately.
func habitSavings(matches []habitMatch) (saving, ifActivated float64) {
	for _, m := range matches {
		if m.MatchType != "product_id" {
			continue
		}
		switch {
		case !m.Personal || strings.EqualFold(m.Offer.ActivationStatus, "ACTIVATED"):
			saving += m.SavingPerItem
		case strings.EqualFold(m.Offer.ActivationStatus, "ACTIVATABLE"):
			ifActivated += m.SavingPerItem
		}
	}
	return util.Round2(saving), util.Round2(ifActivated)
}
