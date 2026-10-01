// Copyright (C) the bramboe/ah-mcp contributors
// Copyright (C) Igor Fedorov and contributors
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Adapted from github.com/bramboe/ah-mcp/tools/bonus.go and tools/bonusmatch.go
// @ 46b85bc652496590e179a665dda346b206d56291 (AGPL-3.0): response types,
// computeTiers, bestTierPrice, applyPricing, selectPeriods, sectionToOffers,
// collectTabOffers, fetchGroupProducts.
//
// Changes: koopzegel maths, numbered snapshots and activation dropped (the
// personal bonus is read-only here); offer type renamed; English field names.

package ah

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	appie "github.com/gwillem/appie-go"

	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

// --- bonuspage API response types (subset of the real payloads) ---

type bonusMetadataResp struct {
	Periods []bonusPeriodResp `json:"periods"`
}

type bonusPeriodResp struct {
	BonusStartDate        string `json:"bonusStartDate"`
	BonusEndDate          string `json:"bonusEndDate"`
	NextPeriodVisibleFrom string `json:"nextPeriodVisibleFrom"`
	Tabs                  []struct {
		Description     string `json:"description"`
		URLMetadataList []struct {
			URL         string `json:"url"`
			Count       int    `json:"count"`
			BonusType   string `json:"bonusType"`
			Description string `json:"description"`
		} `json:"urlMetadataList"`
	} `json:"tabs"`
}

type bonusSectionResp struct {
	BonusGroupOrProducts []struct {
		Product    *sectionProductResp `json:"product"`
		BonusGroup *sectionGroupResp   `json:"bonusGroup"`
	} `json:"bonusGroupOrProducts"`
}

type sectionProductResp struct {
	WebshopID            int             `json:"webshopId"`
	Title                string          `json:"title"`
	CurrentPrice         float64         `json:"currentPrice"`
	PriceBeforeBonus     float64         `json:"priceBeforeBonus"`
	BonusMechanism       string          `json:"bonusMechanism"`
	SalesUnitSize        string          `json:"salesUnitSize"`
	UnitPriceDescription string          `json:"unitPriceDescription"`
	DiscountLabels       []DiscountLabel `json:"discountLabels"`
	ActivationStatus     string          `json:"activationStatus"`
	IsStapelBonus        bool            `json:"isStapelBonus"`
	StoreOnlyPromotion   bool            `json:"storeOnlyPromotion"`
}

type sectionGroupResp struct {
	ID                  string               `json:"id"`
	SegmentDescription  string               `json:"segmentDescription"`
	DiscountDescription string               `json:"discountDescription"`
	ExampleFromPrice    float64              `json:"exampleFromPrice"`
	ExampleForPrice     float64              `json:"exampleForPrice"`
	Products            []sectionProductResp `json:"products"`
	DiscountLabels      []DiscountLabel      `json:"discountLabels"`
	SegmentID           json.Number          `json:"segmentId"`
	ActivationStatus    string               `json:"activationStatus"`
	// Conditions and scope (verified against the live bonus page,
	// 2026-10-01): exceptions such as "*M.u.v. Biologisch", instructions
	// for combo deals, store-only promotions and stacked ("stapel") bonuses.
	ExceptionRule      string   `json:"exceptionRule"`
	ExtraDescriptions  []string `json:"extraDescriptions"`
	StoreOnlyPromotion bool     `json:"storeOnlyPromotion"`
	IsStapelBonus      bool     `json:"isStapelBonus"`
}

// DiscountLabel is one promotion tier from the AH bonuspage. AH uses several
// codes; each maps to an effective price differently (see ComputeTiers).
type DiscountLabel struct {
	Code               string  `json:"code"`
	DefaultDescription string  `json:"defaultDescription"`
	Count              int     `json:"count"`
	FreeCount          int     `json:"freeCount"`
	Percentage         float64 `json:"percentage"`
	Price              float64 `json:"price"`
	Unit               string  `json:"unit"`
}

// PriceTier is a computed price step of a (possibly tiered) bonus offer,
// e.g. "1 stuk 30%" and "2 stuks 50%".
type PriceTier struct {
	Description   string  `json:"description"`
	Count         int     `json:"count,omitempty"`
	DiscountPct   float64 `json:"discount_pct,omitempty"`
	PricePerPiece float64 `json:"price_per_piece,omitempty"`
	TotalPrice    float64 `json:"total_price,omitempty"`
	Unit          string  `json:"unit,omitempty"`
}

// Offer is a bonus offer as returned to agents.
type Offer struct {
	ProductID        int         `json:"product_id,omitempty"`
	SegmentID        string      `json:"segment_id,omitempty"`
	Category         string      `json:"category,omitempty"`
	Title            string      `json:"title"`
	UnitSize         string      `json:"unit_size,omitempty"`
	UnitPrice        string      `json:"unit_price_description,omitempty"`
	BasePrice        float64     `json:"base_price,omitempty"`
	BonusPrice       float64     `json:"bonus_price,omitempty"`
	DiscountPct      float64     `json:"discount_pct,omitempty"`
	Mechanism        string      `json:"mechanism,omitempty"`
	Tiers            []PriceTier `json:"tiers,omitempty"`
	ActivationStatus string      `json:"activation_status,omitempty"`
	// Conditions are AH's exceptions and instructions for the deal, e.g.
	// "*M.u.v. Biologisch" (organic excluded).
	Conditions []string `json:"conditions,omitempty"`
	// StoreOnly deals do not apply to online orders.
	StoreOnly bool `json:"store_only,omitempty"`
	// Stapel marks a stacked bonus: the discount grows with the quantity.
	Stapel bool `json:"stapel_bonus,omitempty"`
	// PriceQuantity says how many pieces base_price and bonus_price cover
	// when AH quotes them for the whole deal (e.g. 2 for "2e halve prijs").
	PriceQuantity int `json:"price_quantity,omitempty"`
}

// ComputeTiers turns AH discount labels into effective price tiers. base is
// the regular price; it is required for percentage and free-item deals but
// not for fixed / x-for-y deals.
func ComputeTiers(base float64, labels []DiscountLabel) []PriceTier {
	tiers := make([]PriceTier, 0, len(labels))
	for _, l := range labels {
		t := PriceTier{Description: l.DefaultDescription, Count: l.Count}
		switch l.Code {
		case "DISCOUNT_TIERED_PERCENT", "DISCOUNT_PERCENTAGE":
			if t.Count == 0 {
				t.Count = 1
			}
			t.DiscountPct = l.Percentage
			if base > 0 {
				t.PricePerPiece = util.Round2(base * (1 - l.Percentage/100))
				t.TotalPrice = util.Round2(t.PricePerPiece * float64(t.Count))
			}
		case "DISCOUNT_X_FOR_Y", "DISCOUNT_TIERED_PRICE":
			// price is the total for `count` pieces (e.g. "2 stuks 2.49").
			if l.Count > 0 && l.Price > 0 {
				t.TotalPrice = util.Round2(l.Price)
				t.PricePerPiece = util.Round2(l.Price / float64(l.Count))
				if base > 0 {
					// From the exact totals: the rounded per-piece price
					// would skew the percentage.
					t.DiscountPct = util.PercentOff(base*float64(l.Count), l.Price)
				}
			}
		case "DISCOUNT_FIXED_PRICE":
			if t.Count == 0 {
				t.Count = 1
			}
			t.PricePerPiece = util.Round2(l.Price)
			t.TotalPrice = util.Round2(l.Price * float64(t.Count))
			if base > 0 && l.Price > 0 {
				t.DiscountPct = util.PercentOff(base, l.Price)
			}
		case "DISCOUNT_X_PLUS_Y_FREE":
			pieces := l.Count + l.FreeCount
			t.Count = pieces
			if pieces > 0 {
				// The discount fraction is structural (pay Count of pieces).
				t.DiscountPct = util.PercentOff(float64(pieces), float64(l.Count))
				if base > 0 {
					t.TotalPrice = util.Round2(base * float64(l.Count))
					t.PricePerPiece = util.Round2(t.TotalPrice / float64(pieces))
				}
			}
		case "DISCOUNT_ONE_HALF_PRICE":
			// "2e halve prijs": on Count items, one half is free.
			if t.Count == 0 {
				t.Count = 2
			}
			t.DiscountPct = util.Round1(0.5 / float64(t.Count) * 100)
			if base > 0 {
				t.PricePerPiece = util.Round2(base * (1 - 0.5/float64(t.Count)))
				t.TotalPrice = util.Round2(t.PricePerPiece * float64(t.Count))
			}
		case "DISCOUNT_WEIGHT":
			// Price per `count` units of `unit` (e.g. per 100 GRAM voor €2.69).
			t.Unit = l.Unit
			t.PricePerPiece = util.Round2(l.Price)
		}
		tiers = append(tiers, t)
	}
	return tiers
}

// BestTierPrice returns the lowest positive per-piece price across
// weight-free tiers, or 0.
func BestTierPrice(tiers []PriceTier) float64 {
	if t := bestTier(tiers); t != nil {
		return t.PricePerPiece
	}
	return 0
}

// bestTier returns the weight-free tier with the lowest positive per-piece
// price, or nil.
func bestTier(tiers []PriceTier) *PriceTier {
	var best *PriceTier
	for i, t := range tiers {
		if t.Unit != "" || t.PricePerPiece <= 0 {
			continue
		}
		if best == nil || t.PricePerPiece < best.PricePerPiece {
			best = &tiers[i]
		}
	}
	return best
}

// alignSinglePiece replaces the computed price of single-piece tiers with
// AH's actual reduced price now when they differ only by rounding (AH
// rounds percentage discounts its own way: 1.48 where the maths gives
// 1.49).
func alignSinglePiece(tiers []PriceTier, now float64) []PriceTier {
	if now <= 0 {
		return tiers
	}
	for i, t := range tiers {
		if t.Count == 1 && t.Unit == "" && t.PricePerPiece > 0 && t.PricePerPiece != now && math.Abs(t.PricePerPiece-now) < 0.025 {
			tiers[i].PricePerPiece, tiers[i].TotalPrice = util.Round2(now), util.Round2(now)
		}
	}
	return tiers
}

// applyPricing fills Tiers and guarantees BonusPrice/DiscountPct are set to
// the effective bonus price, even for tiered deals without a single price.
// now is AH's actual reduced per-piece price, or 0 when it has none.
func (o *Offer) applyPricing(base, now float64, labels []DiscountLabel) {
	o.Tiers = alignSinglePiece(pricedTiers(ComputeTiers(base, labels)), now)
	switch {
	case o.BonusPrice == 0:
		// Derived from the tiers: the tier's own percentage is exact,
		// recomputing it from the rounded per-piece price is not.
		if t := bestTier(o.Tiers); t != nil {
			o.BonusPrice, o.DiscountPct = t.PricePerPiece, t.DiscountPct
		}
	case o.BasePrice > 0 && o.BonusPrice < o.BasePrice:
		o.DiscountPct = util.PercentOff(o.BasePrice, o.BonusPrice)
	}
	if o.DiscountPct == 0 {
		for _, tr := range o.Tiers {
			o.DiscountPct = math.Max(o.DiscountPct, tr.DiscountPct)
		}
	}
	o.BasePrice, o.BonusPrice = util.Round2(o.BasePrice), util.Round2(o.BonusPrice)
}

func (p *sectionProductResp) toOffer() Offer {
	o := Offer{
		ProductID:        p.WebshopID,
		Title:            p.Title,
		UnitSize:         p.SalesUnitSize,
		UnitPrice:        p.UnitPriceDescription,
		BasePrice:        p.PriceBeforeBonus,
		BonusPrice:       p.CurrentPrice,
		Mechanism:        p.BonusMechanism,
		ActivationStatus: p.ActivationStatus,
		StoreOnly:        p.StoreOnlyPromotion,
		Stapel:           p.IsStapelBonus,
	}
	now := 0.0
	if p.CurrentPrice > 0 && p.CurrentPrice < p.PriceBeforeBonus {
		now = p.CurrentPrice
	}
	o.applyPricing(p.PriceBeforeBonus, now, p.DiscountLabels)
	return o
}

func (g *sectionGroupResp) toOffer() Offer {
	o := Offer{
		SegmentID:        g.ID,
		Title:            g.SegmentDescription,
		BasePrice:        g.ExampleFromPrice,
		BonusPrice:       g.ExampleForPrice,
		Mechanism:        g.DiscountDescription,
		ActivationStatus: g.ActivationStatus,
		Conditions:       util.UniqueNonEmpty(slices.Concat(g.ExtraDescriptions, []string{g.ExceptionRule})...),
		StoreOnly:        g.StoreOnlyPromotion,
		Stapel:           g.IsStapelBonus,
	}
	if o.SegmentID == "" {
		if s := g.SegmentID.String(); s != "0" {
			o.SegmentID = s
		}
	}
	base, qty := g.pieceBase()
	if qty > 1 {
		o.PriceQuantity = qty
	}
	o.applyPricing(base, 0, g.DiscountLabels)
	return o
}

// pieceBase returns the regular price of one piece, for tier maths, and how
// many pieces the example prices cover. For multi-piece group deals AH often
// quotes both example prices for the whole deal ("van 4.38 voor 3.00" for
// "2 voor 3.00", "van 8.58 voor 6.44" for "2e halve prijs"): when dividing
// the from-price by the deal size reproduces the for-price as the tier
// total, the prices cover the deal size.
func (g *sectionGroupResp) pieceBase() (float64, int) {
	from, forPrice := g.ExampleFromPrice, g.ExampleForPrice
	if from <= 0 || forPrice <= 0 || forPrice >= from {
		return from, 1
	}
	for _, l := range g.DiscountLabels {
		n := dealSize(l)
		if n <= 1 {
			continue
		}
		base := from / float64(n)
		for _, t := range ComputeTiers(base, []DiscountLabel{l}) {
			if math.Abs(t.TotalPrice-forPrice) < 0.015 {
				return base, n
			}
		}
	}
	return from, 1
}

// dealSize is how many pieces one application of a deal takes.
func dealSize(l DiscountLabel) int {
	switch l.Code {
	case "DISCOUNT_X_PLUS_Y_FREE":
		return l.Count + l.FreeCount
	case "DISCOUNT_ONE_HALF_PRICE":
		return max(l.Count, 2)
	default:
		return l.Count
	}
}

// pricedTiers drops tiers that carry no price information (labels such as
// DISCOUNT_BONUS "BONUS" or unknown codes); their text is already in the
// offer's mechanism.
func pricedTiers(tiers []PriceTier) []PriceTier {
	var out []PriceTier
	for _, t := range tiers {
		if t.PricePerPiece > 0 || t.DiscountPct > 0 {
			out = append(out, t)
		}
	}
	return out
}

func sectionToOffers(section bonusSectionResp) []Offer {
	var offers []Offer
	for _, entry := range section.BonusGroupOrProducts {
		if entry.Product != nil {
			offers = append(offers, entry.Product.toOffer())
		}
		if entry.BonusGroup == nil {
			continue
		}
		group := entry.BonusGroup.toOffer()
		if len(entry.BonusGroup.Products) == 0 {
			offers = append(offers, group)
			continue
		}
		for i := range entry.BonusGroup.Products {
			o := entry.BonusGroup.Products[i].toOffer()
			if o.ActivationStatus == "" {
				o.ActivationStatus = group.ActivationStatus
			}
			if o.SegmentID == "" {
				o.SegmentID = group.SegmentID
			}
			// Products inside a group deal share its conditions and scope.
			o.Conditions = util.UniqueNonEmpty(slices.Concat(group.Conditions, o.Conditions)...)
			o.StoreOnly = o.StoreOnly || group.StoreOnly
			o.Stapel = o.Stapel || group.Stapel
			offers = append(offers, o)
		}
	}
	return offers
}

// Period is a bonus week.
type Period struct {
	Start string `json:"start"`
	End   string `json:"end"`
	// NextVisibleFrom is when AH publishes next week's offers (current
	// period only, if AH reports it).
	NextVisibleFrom string `json:"next_visible_from,omitempty"`
	tabs            *bonusPeriodResp
}

// BonusPeriods returns the current period (covering today) and the
// earliest upcoming one; either may be nil.
//
// The metadata tab structure (from bramboe/ah-mcp@46b85bc) and the section
// discountLabels were verified against the live API on 2026-10-01.
func BonusPeriods(ctx context.Context, c GraphQLDoer, today string) (current, upcoming *Period, err error) {
	var meta bonusMetadataResp
	if err := c.DoRequest(ctx, http.MethodGet, "/mobile-services/bonuspage/v3/metadata", nil, &meta); err != nil {
		return nil, nil, fmt.Errorf("get bonus metadata: %w", err)
	}
	cur, up := selectPeriods(&meta, today)
	return cur, up, nil
}

// selectPeriods splits the metadata periods into the one covering today and
// the earliest upcoming one.
func selectPeriods(meta *bonusMetadataResp, today string) (current, upcoming *Period) {
	for i := range meta.Periods {
		p := &meta.Periods[i]
		per := &Period{Start: p.BonusStartDate, End: p.BonusEndDate, NextVisibleFrom: p.NextPeriodVisibleFrom, tabs: p}
		switch {
		case p.BonusStartDate > today:
			if upcoming == nil || p.BonusStartDate < upcoming.Start {
				upcoming = per
			}
		case p.BonusEndDate >= today:
			current = per
		}
	}
	return current, upcoming
}

// PeriodOffers returns the NATIONAL and SPOTLIGHT offers of a period,
// tagged with their category and de-duplicated. Sections are fetched
// concurrently in tab order.
func PeriodOffers(ctx context.Context, c GraphQLDoer, p *Period) ([]Offer, error) {
	if p == nil || p.tabs == nil {
		return nil, nil
	}
	wanted := map[string]bool{"NATIONAL": true, "SPOTLIGHT": true}
	seenURL := map[string]bool{}
	var urls, descs []string
	for _, tab := range p.tabs.Tabs {
		for _, meta := range tab.URLMetadataList {
			if !wanted[meta.BonusType] || seenURL[meta.URL] {
				continue
			}
			seenURL[meta.URL] = true
			urls = append(urls, meta.URL)
			descs = append(descs, meta.Description)
		}
	}

	sections := make([][]Offer, len(urls))
	errs := make([]error, len(urls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for i, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var section bonusSectionResp
			path := "/mobile-services/" + strings.TrimPrefix(u, "/")
			if err := c.DoRequest(ctx, http.MethodGet, path, nil, &section); err != nil {
				errs[i] = err
				return
			}
			sections[i] = sectionToOffers(section)
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	var offers []Offer
	for i := range urls {
		if errs[i] != nil {
			return nil, fmt.Errorf("bonus section %q: %w", descs[i], errs[i])
		}
		for _, o := range sections[i] {
			key := fmt.Sprintf("%d:%s:%s", o.ProductID, o.SegmentID, o.Title)
			if seen[key] {
				continue
			}
			seen[key] = true
			o.Category = descs[i]
			offers = append(offers, o)
		}
	}
	return offers, nil
}

// PersonalOffers returns the member's Bonus Box offers for the bonus week
// starting at startDate, including each offer's activation status. It
// calls the same endpoint as appie-go's GetPersonalBonus, but decodes the
// activationStatus and discountLabels fields that appie-go's Product type
// drops. Read-only: nothing is activated.
//
// The activationStatus field (from bramboe/ah-mcp@46b85bc) was verified
// against the live API on 2026-10-01.
func PersonalOffers(ctx context.Context, c GraphQLDoer, startDate string) ([]Offer, error) {
	path := "/mobile-services/bonuspage/v1/personal?" + url.Values{"bonusStartDate": {startDate}}.Encode()
	var section bonusSectionResp
	if err := c.DoRequest(ctx, http.MethodGet, path, nil, &section); err != nil {
		return nil, fmt.Errorf("get personal bonus: %w", err)
	}
	offers := sectionToOffers(section)
	for i := range offers {
		if offers[i].ActivationStatus == "" {
			offers[i].ActivationStatus = "UNKNOWN"
		}
	}
	return offers, nil
}

// GroupProduct is a product inside a bonus group.
type GroupProduct struct {
	ProductID  int     `json:"product_id"`
	Title      string  `json:"title"`
	Brand      string  `json:"brand,omitempty"`
	UnitSize   string  `json:"unit_size,omitempty"`
	BasePrice  float64 `json:"base_price,omitempty"`
	BonusPrice float64 `json:"bonus_price,omitempty"`
	Mechanism  string  `json:"mechanism,omitempty"`
	Orderable  bool    `json:"orderable"`
}

// groupProductsQuery resolves a bonus group for an explicit period. Adapted
// from appie-go's fetchBonusGroupProductsQuery, which hardcodes the
// current period.
//
// UNVERIFIED: from bramboe/ah-mcp@46b85bc (groupProductsQuery).
const groupProductsQuery = `query FetchBonusPromotionWithProducts(
  $id: String, $periodStart: String, $periodEnd: String
) {
  bonusPromotions(input: {
    id: $id, periodStart: $periodStart, periodEnd: $periodEnd,
    filterUnavailableProducts: true, forcePromotionVisibility: true,
    showAllPromotionSegments: true
  }) {
    id
    products {
      id
      title
      brand
      salesUnitSize
      availability { isOrderable }
      priceV2(
        periodStart: $periodStart, periodEnd: $periodEnd,
        filterUnavailableProducts: true, forcePromotionVisibility: true
      ) {
        now { amount }
        was { amount }
        promotionLabel { tiers { description } }
      }
    }
  }
}`

// withoutFlatBonusPrice drops bonus_price where it is not below the base
// price: for deals such as "1+1 gratis" AH keeps the per-piece price and
// applies the deal at checkout, so an equal "bonus price" would suggest there
// is no discount.
func withoutFlatBonusPrice(ps []GroupProduct) []GroupProduct {
	for i := range ps {
		if ps[i].BonusPrice >= ps[i].BasePrice {
			ps[i].BonusPrice = 0
		}
	}
	return ps
}

// GroupProducts returns the products of a bonus group. For the current
// period it uses appie-go's GetBonusGroupProducts; for another period the
// period-aware query above.
func GroupProducts(ctx context.Context, c *appie.Client, segmentID string, period *Period, current bool) ([]GroupProduct, error) {
	if current {
		products, err := c.GetBonusGroupProducts(ctx, segmentID)
		if err != nil {
			return nil, err
		}
		out := make([]GroupProduct, 0, len(products))
		for _, p := range products {
			base, bonus := p.Price.Was, p.Price.Now
			if base == 0 {
				base = bonus
			}
			out = append(out, GroupProduct{ProductID: p.ID, Title: p.Title, Brand: p.Brand, UnitSize: p.UnitSize,
				BasePrice: util.Round2(base), BonusPrice: util.Round2(bonus), Mechanism: p.BonusMechanism, Orderable: p.IsOrderable})
		}
		return withoutFlatBonusPrice(out), nil
	}
	var resp struct {
		BonusPromotions []struct {
			Products []struct {
				ID            int    `json:"id"`
				Title         string `json:"title"`
				Brand         string `json:"brand"`
				SalesUnitSize string `json:"salesUnitSize"`
				Availability  struct {
					IsOrderable bool `json:"isOrderable"`
				} `json:"availability"`
				PriceV2 struct {
					Now            Money `json:"now"`
					Was            Money `json:"was"`
					PromotionLabel *struct {
						Tiers []struct {
							Description string `json:"description"`
						} `json:"tiers"`
					} `json:"promotionLabel"`
				} `json:"priceV2"`
			} `json:"products"`
		} `json:"bonusPromotions"`
	}
	vars := map[string]any{"id": segmentID, "periodStart": period.Start, "periodEnd": period.End}
	if err := c.DoGraphQL(ctx, groupProductsQuery, vars, &resp); err != nil {
		return nil, fmt.Errorf("get bonus group products: %w", err)
	}
	var out []GroupProduct
	if len(resp.BonusPromotions) > 0 {
		for _, p := range resp.BonusPromotions[0].Products {
			gp := GroupProduct{ProductID: p.ID, Title: p.Title, Brand: p.Brand, UnitSize: p.SalesUnitSize,
				BasePrice: util.Round2(p.PriceV2.Was.Amount), BonusPrice: util.Round2(p.PriceV2.Now.Amount), Orderable: p.Availability.IsOrderable}
			if gp.BasePrice == 0 {
				gp.BasePrice = gp.BonusPrice
			}
			if p.PriceV2.PromotionLabel != nil && len(p.PriceV2.PromotionLabel.Tiers) > 0 {
				gp.Mechanism = p.PriceV2.PromotionLabel.Tiers[0].Description
			}
			out = append(out, gp)
		}
	}
	return withoutFlatBonusPrice(out), nil
}
