// Copyright (C) the gwillem/appie-go contributors
// Copyright (C) Igor Fedorov and contributors
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Adapted from github.com/gwillem/appie-go/products.go (searchResponse,
// productResponse, toProduct, SearchProductsFiltered, GetProduct,
// fetchNutritionalInfo) @ 65f41abdeaffb4b990cc35525cc0f20837102e1c (AGPL-3.0).
//
// Changes: the bonus fields appie-go's Product drops are kept —
// discountLabels (the promotion text when bonusMechanism is empty, e.g.
// "pakketkorting" for online bundle discounts), the bonus period and the
// promotion type.

package ah

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	appie "github.com/gwillem/appie-go"
)

// ProductInfo is an appie-go Product plus the bonus details it omits.
type ProductInfo struct {
	appie.Product
	DiscountLabels []DiscountLabel
	BonusStart     string // YYYY-MM-DD, "" if not on bonus
	BonusEnd       string
	PromotionType  string // e.g. NATIONAL, AHONLINE
	Stapel         bool   // stacked bonus: the discount grows with quantity
}

// Tiers computes the price tiers of the product's promotion from its
// discount labels, leaving out labels that carry no price information
// (e.g. DISCOUNT_BONUS "BONUS", DISCOUNT_BUNDLE "pakketkorting").
func (p ProductInfo) Tiers() []PriceTier {
	if !p.IsBonus || len(p.DiscountLabels) == 0 {
		return nil
	}
	base := p.Price.Was
	if base <= 0 {
		base = p.Price.Now
	}
	now := 0.0
	if p.Price.Was > p.Price.Now {
		now = p.Price.Now
	}
	return alignSinglePiece(pricedTiers(ComputeTiers(base, p.DiscountLabels)), now)
}

// Mechanism is the promotion text: bonusMechanism, or else the discount
// label descriptions AH sent instead.
func (p ProductInfo) Mechanism() string {
	if p.BonusMechanism != "" || !p.IsBonus {
		return p.BonusMechanism
	}
	var parts []string
	for _, l := range p.DiscountLabels {
		if d := strings.TrimSpace(l.DefaultDescription); d != "" {
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, "; ")
}

// OnlineOnly reports a promotion that only applies to online orders.
func (p ProductInfo) OnlineOnly() bool {
	return p.IsBonus && strings.EqualFold(p.PromotionType, "AHONLINE")
}

type productResp struct {
	WebshopID            int             `json:"webshopId"`
	Title                string          `json:"title"`
	Brand                string          `json:"brand"`
	SalesUnitSize        string          `json:"salesUnitSize"`
	UnitPriceDescription string          `json:"unitPriceDescription"`
	Images               []appie.Image   `json:"images"`
	CurrentPrice         float64         `json:"currentPrice"`
	PriceBeforeBonus     float64         `json:"priceBeforeBonus"`
	IsBonus              bool            `json:"isBonus"`
	BonusMechanism       string          `json:"bonusMechanism"`
	MainCategory         string          `json:"mainCategory"`
	SubCategory          string          `json:"subCategory"`
	NutriScore           string          `json:"nutriscore"`
	AvailableOnline      bool            `json:"availableOnline"`
	IsOrderable          bool            `json:"isOrderable"`
	PropertyIcons        []string        `json:"propertyIcons"`
	DiscountLabels       []DiscountLabel `json:"discountLabels"`
	BonusStartDate       string          `json:"bonusStartDate"`
	BonusEndDate         string          `json:"bonusEndDate"`
	PromotionType        string          `json:"promotionType"`
	IsStapelBonus        bool            `json:"isStapelBonus"`
}

func (p *productResp) toInfo() ProductInfo {
	price := p.CurrentPrice
	if price == 0 {
		price = p.PriceBeforeBonus
	}
	info := ProductInfo{
		Product: appie.Product{
			ID: p.WebshopID, WebshopID: strconv.Itoa(p.WebshopID), Title: p.Title, Brand: p.Brand,
			Category: p.MainCategory, SubCategory: p.SubCategory, Price: appie.Price{Now: price, Was: p.PriceBeforeBonus},
			Images: p.Images, NutriScore: p.NutriScore, IsBonus: p.IsBonus, BonusMechanism: p.BonusMechanism,
			IsAvailable: p.AvailableOnline, IsOrderable: p.IsOrderable, UnitSize: p.SalesUnitSize,
			UnitPriceDescription: p.UnitPriceDescription, PropertyIcons: p.PropertyIcons,
		},
		DiscountLabels: p.DiscountLabels,
		PromotionType:  p.PromotionType,
		Stapel:         p.IsBonus && p.IsStapelBonus,
	}
	if p.IsBonus {
		info.BonusStart, info.BonusEnd = p.BonusStartDate, p.BonusEndDate
	}
	return info
}

// SearchProducts searches like appie-go's SearchProductsFiltered (bonus-only
// over-fetches and filters client-side), keeping the bonus details.
func SearchProducts(ctx context.Context, c GraphQLDoer, query string, limit int, bonusOnly bool) ([]ProductInfo, error) {
	if limit <= 0 {
		limit = 30
	}
	pageSize := limit
	if bonusOnly {
		pageSize = limit * 5
	}
	var out []ProductInfo
	for page := 0; len(out) < limit; page++ {
		params := url.Values{"query": {query}, "page": {strconv.Itoa(page)}, "size": {strconv.Itoa(pageSize)}, "sortOn": {"RELEVANCE"}}
		var resp struct {
			Products []productResp `json:"products"`
			Page     struct {
				TotalElements int `json:"totalElements"`
			} `json:"page"`
		}
		if err := c.DoRequest(ctx, http.MethodGet, "/mobile-services/product/search/v2?"+params.Encode(), nil, &resp); err != nil {
			return nil, fmt.Errorf("search products: %w", err)
		}
		for i := range resp.Products {
			p := resp.Products[i].toInfo()
			if bonusOnly && !p.IsBonus {
				continue
			}
			out = append(out, p)
			if len(out) >= limit {
				break
			}
		}
		if (page+1)*pageSize >= resp.Page.TotalElements || len(resp.Products) == 0 {
			break
		}
	}
	return out, nil
}

const productNutritionQuery = `query FetchProduct($productId: Int!) {
  product(id: $productId) {
    id
    tradeItem {
      nutritions {
        nutrients {
          type
          name
          value
        }
      }
    }
  }
}`

// GetProduct fetches one product (and its nutrition when requested).
func GetProduct(ctx context.Context, c GraphQLDoer, productID int, withNutrition bool) (*ProductInfo, error) {
	var resp struct {
		ProductCard productResp `json:"productCard"`
	}
	if err := c.DoRequest(ctx, http.MethodGet, fmt.Sprintf("/mobile-services/product/detail/v4/fir/%d", productID), nil, &resp); err != nil {
		return nil, fmt.Errorf("get product: %w", err)
	}
	p := resp.ProductCard.toInfo()
	if !withNutrition {
		return &p, nil
	}
	var nr struct {
		Product struct {
			TradeItem *struct {
				Nutritions []struct {
					Nutrients []appie.NutritionalInfo `json:"nutrients"`
				} `json:"nutritions"`
			} `json:"tradeItem"`
		} `json:"product"`
	}
	if err := c.DoGraphQL(ctx, productNutritionQuery, map[string]any{"productId": productID}, &nr); err != nil {
		return nil, fmt.Errorf("get product nutrition: %w", err)
	}
	if ti := nr.Product.TradeItem; ti != nil {
		for _, n := range ti.Nutritions {
			p.NutritionalInfo = append(p.NutritionalInfo, n.Nutrients...)
		}
	}
	return &p, nil
}

// SearchNutriScore looks the product up by its title and returns the
// Nutri-Score from the search result: the detail endpoint does not carry it
// (observed live on 2026-10-01), the search endpoint does. "" when the
// search does not list the product or it has no score (e.g. non-food).
func SearchNutriScore(ctx context.Context, c GraphQLDoer, id int, title string) (string, error) {
	ps, err := SearchProducts(ctx, c, title, 10, false)
	if err != nil {
		return "", err
	}
	for _, p := range ps {
		if p.ID == id {
			return p.NutriScore, nil
		}
	}
	return "", nil
}
