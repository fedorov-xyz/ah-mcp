package tools

import (
	"context"
	"fmt"
	"strings"

	appie "github.com/gwillem/appie-go"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/fedorov-xyz/ah-mcp/internal/ah"
	"github.com/fedorov-xyz/ah-mcp/internal/cache"
	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

// productView is the product shape returned by product tools. It omits
// per-user fields (isPreviouslyBought), because search results are cached
// for every user.
type productView struct {
	ProductID   int     `json:"product_id"`
	Title       string  `json:"title"`
	Brand       string  `json:"brand,omitempty"`
	Category    string  `json:"category,omitempty"`
	UnitSize    string  `json:"unit_size,omitempty"`
	UnitPrice   string  `json:"unit_price_description,omitempty"`
	Price       float64 `json:"price"`
	PriceWas    float64 `json:"price_was,omitempty"`
	IsBonus     bool    `json:"is_bonus"`
	DiscountPct float64 `json:"discount_pct,omitempty"`
	// Tiers are the computed price steps of the promotion (e.g. "1+1
	// gratis": 2 pieces at half price each).
	Tiers          []ah.PriceTier `json:"tiers,omitempty"`
	Stapel         bool           `json:"stapel_bonus,omitempty"`
	BonusMechanism string         `json:"bonus_mechanism,omitempty"`
	BonusPeriod    *period        `json:"bonus_period,omitempty"`
	OnlineOnly     bool           `json:"online_only_bonus,omitempty"`
	BonusSegmentID string         `json:"bonus_segment_id,omitempty"`
	Orderable      bool           `json:"orderable"`
	NutriScore     string         `json:"nutri_score,omitempty"`
	Icons          []string       `json:"property_icons,omitempty"`
	ImageURL       string         `json:"image_url,omitempty"`
}

type period struct {
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
	// OpenEnded replaces a placeholder end date (AH uses e.g. 2999-12-31 or
	// 2034-12-31 for promotions without an end).
	OpenEnded bool `json:"open_ended,omitempty"`
}

// viewProduct maps a product for output. Bonus end dates after
// openEndedAfter (YYYY-MM-DD) are treated as "no end date".
func viewProduct(pi ah.ProductInfo, openEndedAfter string) productView {
	p := pi.Product
	v := productView{
		ProductID: p.ID, Title: p.Title, Brand: p.Brand, Category: p.Category, UnitSize: p.UnitSize,
		UnitPrice: p.UnitPriceDescription, Price: util.Round2(p.Price.Now), IsBonus: p.IsBonus,
		BonusMechanism: pi.Mechanism(), OnlineOnly: pi.OnlineOnly(), BonusSegmentID: p.BonusSegmentID,
		Orderable: p.IsOrderable, NutriScore: p.NutriScore, Icons: p.PropertyIcons,
	}
	if pi.BonusStart != "" || pi.BonusEnd != "" {
		v.BonusPeriod = &period{Start: pi.BonusStart, End: pi.BonusEnd}
		if pi.BonusEnd > openEndedAfter {
			v.BonusPeriod.End, v.BonusPeriod.OpenEnded = "", true
		}
	}
	if p.IsBonus {
		v.DiscountPct = util.PercentOff(p.Price.Was, p.Price.Now)
		v.Tiers, v.Stapel = pi.Tiers(), pi.Stapel
	}
	if p.Price.Was > p.Price.Now {
		v.PriceWas = util.Round2(p.Price.Was)
	}
	if len(p.Images) > 0 {
		v.ImageURL = p.Images[len(p.Images)-1].URL
	}
	return v
}

func (d *Deps) searchProductsTool() server.ServerTool {
	tool := mcp.NewTool("ah_search_products",
		mcp.WithDescription("Search the Albert Heijn assortment by keyword (Dutch works best, e.g. \"halfvolle melk\"). "+
			"Returns product_id, title, price in EUR, bonus status and mechanism. Use product_id with ah_get_product, ah_add_to_shopping_list or ah_update_cart_item. "+
			"Bonus products carry bonus_mechanism, discount_pct (computed from price_was), price tiers, bonus_period and online_only_bonus. "+
			"Prices are current webshop prices; compare products and look up reviews yourself — there is no compare tool."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search term."), mcp.MinLength(1), mcp.MaxLength(100)),
		limitParam(),
		mcp.WithBoolean("bonus", mcp.Description("Only return products that are on bonus now.")),
		mcp.WithNumber("max_price", mcp.Description("Only return products costing at most this many euros."), mcp.Min(0)),
		readOnly("Albert Heijn: Search Products"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Query    string   `json:"query"`
			Limit    *int     `json:"limit"`
			Bonus    bool     `json:"bonus"`
			MaxPrice *float64 `json:"max_price"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		a.Query = strings.TrimSpace(a.Query)
		if a.Query == "" || len(a.Query) > 100 {
			return invalid("query is required (1-100 characters)"), nil
		}
		limit, r := limitOf(a.Limit)
		if r != nil {
			return r, nil
		}
		if a.MaxPrice != nil && *a.MaxPrice < 0 {
			return invalid("max_price must be >= 0"), nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		// One extra result tells whether the list was truncated.
		fetch := limit + 1
		if a.MaxPrice != nil {
			fetch = maxLimit + 1 // over-fetch, then filter by price
		}
		key := fmt.Sprintf("search:%s:%d:%t", strings.ToLower(a.Query), fetch, a.Bonus)
		products, err := cache.GetOrLoad(d.Cache, cache.Global, key, cache.TTLSearch, func() ([]productView, error) {
			ps, err := ah.SearchProducts(ctx, c.c, a.Query, fetch, a.Bonus)
			if err != nil {
				return nil, err
			}
			out := make([]productView, 0, len(ps))
			for _, p := range ps {
				out = append(out, viewProduct(p, d.openEndedAfter()))
			}
			return out, nil
		})
		if err != nil {
			return d.upstream(c, "search products", err), nil
		}
		var filtered []productView
		for _, p := range products {
			if a.MaxPrice != nil && p.Price > *a.MaxPrice {
				continue
			}
			filtered = append(filtered, p)
		}
		filtered, truncated := util.Truncate(filtered, limit)
		data := map[string]any{"query": a.Query, "currency": "EUR", "count": len(filtered), "truncated": truncated, "products": filtered}
		return ok(fmt.Sprintf("%d product(s) for %q.", len(filtered), a.Query), data), nil
	}}
}

func (d *Deps) getProductTool() server.ServerTool {
	tool := mcp.NewTool("ah_get_product",
		mcp.WithDescription("Get one Albert Heijn product by product_id (from ah_search_products): price, bonus (mechanism, discount_pct, price tiers, period, online-only), unit size and, with include_nutrition=true, nutritional values."),
		mcp.WithInteger("product_id", mcp.Required(), mcp.Description("Webshop product id."), mcp.Min(1)),
		mcp.WithBoolean("include_nutrition", mcp.Description("Also fetch nutritional values (one extra request).")),
		readOnly("Albert Heijn: Get Product"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			ProductID        int  `json:"product_id"`
			IncludeNutrition bool `json:"include_nutrition"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		if a.ProductID <= 0 {
			return invalid("product_id must be a positive integer"), nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		type full struct {
			productView
			Nutrition []appie.NutritionalInfo `json:"nutrition,omitempty"`
		}
		key := fmt.Sprintf("product:%d:%t", a.ProductID, a.IncludeNutrition)
		p, err := cache.GetOrLoad(d.Cache, cache.Global, key, cache.TTLProduct, func() (full, error) {
			prod, err := ah.GetProduct(ctx, c.c, a.ProductID, a.IncludeNutrition)
			if err != nil {
				return full{}, err
			}
			return full{productView: viewProduct(*prod, d.openEndedAfter()), Nutrition: prod.NutritionalInfo}, nil
		})
		if err != nil {
			return d.upstream(c, "get product", err), nil
		}
		if p.ProductID == 0 {
			return toolError(codeNotFound, fmt.Sprintf("product %d not found", a.ProductID), nil), nil
		}
		if p.NutriScore == "" {
			p.NutriScore = d.nutriScore(c, p.ProductID, p.Title)
		}
		data := map[string]any{"currency": "EUR", "product": p}
		summary := fmt.Sprintf("%s — €%.2f.", p.Title, p.Price)
		if a.IncludeNutrition {
			data["nutrition_available"] = len(p.Nutrition) > 0
			if len(p.Nutrition) == 0 {
				data["note"] = "Albert Heijn has no nutrition table for this product (common for multipacks and non-food)."
				summary += " No nutrition data available."
			}
		}
		return ok(summary, data), nil
	}}
}

// nutriScore finds a product's Nutri-Score via search (the detail endpoint
// lacks it), cached per product for a day, including "no score". Best
// effort: a failed search returns "" and is not cached.
func (d *Deps) nutriScore(c *call, id int, title string) string {
	if title == "" {
		return ""
	}
	key := fmt.Sprintf("nutriscore:%d", id)
	ns, err := cache.GetOrLoad(d.Cache, cache.Global, key, cache.TTLNutriScore, func() (string, error) {
		return ah.SearchNutriScore(c.ctx, c.c, id, title)
	})
	if err != nil {
		d.Logger.Debug("nutri-score lookup failed", "product_id", id, "error", err)
		return ""
	}
	return ns
}

type storeView struct {
	StoreID int    `json:"store_id"`
	Name    string `json:"name"`
	Type    string `json:"type,omitempty"`
	Address string `json:"address"`
}

// resolvePostal returns the given postal code or the member's own.
func (d *Deps) resolvePostal(c *call, given string) (string, *mcp.CallToolResult) {
	if given != "" {
		pc, ok := util.NormalizePostalCode(given)
		if !ok {
			return "", invalid("postal_code must be a Dutch postal code like 1234AB")
		}
		return pc, nil
	}
	m, err := c.c.GetMember(c.ctx)
	if err != nil {
		return "", d.upstream(c, "get member", err)
	}
	pc, ok := util.NormalizePostalCode(m.Address.PostalCode)
	if !ok {
		return "", invalid("no postal_code given and the account has no postal code; pass postal_code")
	}
	return pc, nil
}

func (d *Deps) stores(c *call, postal string) ([]storeView, error) {
	return cache.GetOrLoad(d.Cache, cache.Global, "stores:"+postal, cache.TTLStores, func() ([]storeView, error) {
		ss, err := c.c.SearchStores(c.ctx, postal)
		if err != nil {
			return nil, err
		}
		out := make([]storeView, 0, len(ss))
		for _, s := range ss {
			addr := strings.TrimSpace(fmt.Sprintf("%s %s%s, %s %s", s.Address.Street, s.Address.HouseNumber, s.Address.HouseNumberExtra, s.Address.PostalCode, s.Address.City))
			out = append(out, storeView{StoreID: s.ID, Name: s.Name, Type: s.StoreType, Address: addr})
		}
		return out, nil
	})
}

func (d *Deps) searchStoresTool() server.ServerTool {
	tool := mcp.NewTool("ah_search_stores",
		mcp.WithDescription("Find Albert Heijn stores near a Dutch postal code, in the order Albert Heijn returns them (no distance is given). Without postal_code the account's own postal code is used. "+
			"Use store_id with ah_last_chance."),
		mcp.WithString("postal_code", mcp.Description("Dutch postal code, e.g. 1012AB. Defaults to the account address.")),
		readOnly("Albert Heijn: Search Stores"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			PostalCode string `json:"postal_code"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		pc, r := d.resolvePostal(c, a.PostalCode)
		if r != nil {
			return r, nil
		}
		ss, err := d.stores(c, pc)
		if err != nil {
			return d.upstream(c, "search stores", err), nil
		}
		data := map[string]any{"postal_code": pc, "stores": ss}
		return ok(fmt.Sprintf("%d store(s) near %s.", len(ss), pc), data), nil
	}}
}

func (d *Deps) lastChanceTool() server.ServerTool {
	tool := mcp.NewTool("ah_last_chance",
		mcp.WithDescription("List last-chance markdowns (\"laatste kans\" bargains, usually near expiry) in one store. "+
			"Pass store_id (from ah_search_stores) or postal_code (the first store Albert Heijn returns for it is used). In-store only; these cannot be ordered online."),
		mcp.WithInteger("store_id", mcp.Description("Store id from ah_search_stores."), mcp.Min(1)),
		mcp.WithString("postal_code", mcp.Description("Dutch postal code; the first store Albert Heijn returns for it is used.")),
		limitParam(),
		readOnly("Albert Heijn: Last Chance Bargains"),
	)
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			StoreID    int    `json:"store_id"`
			PostalCode string `json:"postal_code"`
			Limit      *int   `json:"limit"`
		}
		if r := bind(req, &a); r != nil {
			return r, nil
		}
		limit, r := limitOf(a.Limit)
		if r != nil {
			return r, nil
		}
		if a.StoreID == 0 && a.PostalCode == "" {
			return invalid("pass store_id or postal_code"), nil
		}
		c, fail := d.begin(ctx)
		if fail != nil {
			return fail, nil
		}
		storeID, storeName := a.StoreID, ""
		if storeID == 0 {
			pc, ok := util.NormalizePostalCode(a.PostalCode)
			if !ok {
				return invalid("postal_code must be a Dutch postal code like 1234AB"), nil
			}
			ss, err := d.stores(c, pc)
			if err != nil {
				return d.upstream(c, "search stores", err), nil
			}
			if len(ss) == 0 {
				return toolError(codeNotFound, "no store found near "+pc, nil), nil
			}
			storeID, storeName = ss[0].StoreID, ss[0].Name
		} else {
			storeName = d.storeNameNearMember(c, storeID)
		}
		bargains, err := c.c.GetBargains(ctx, storeID)
		if err != nil {
			return d.upstream(c, "get bargains", err), nil
		}
		type bargain struct {
			ProductID   int     `json:"product_id"`
			Title       string  `json:"title"`
			Brand       string  `json:"brand,omitempty"`
			UnitSize    string  `json:"unit_size,omitempty"`
			Category    string  `json:"category,omitempty"`
			PriceWas    float64 `json:"price_was,omitempty"`
			PriceNow    float64 `json:"price_now,omitempty"`
			DiscountPct float64 `json:"discount_pct,omitempty"`
			Expires     string  `json:"expires,omitempty"`
			Stock       int     `json:"stock"`
		}
		out := make([]bargain, 0, len(bargains))
		for _, b := range bargains {
			out = append(out, bargain{
				ProductID: b.Product.ID, Title: b.Product.Title, Brand: b.Product.Brand, UnitSize: b.Product.UnitSize,
				Category: b.Category, PriceWas: util.ParseEuro(b.PriceWas), PriceNow: util.ParseEuro(b.PriceNow),
				DiscountPct: b.MarkdownPercentage, Expires: b.ExpirationDate, Stock: b.Stock,
			})
		}
		out, truncated := util.Truncate(out, limit)
		data := map[string]any{"store_id": storeID, "currency": "EUR", "count": len(out), "truncated": truncated, "bargains": out}
		if storeName != "" {
			data["store_name"] = storeName
		}
		return ok(fmt.Sprintf("%d bargain(s) in store %d.", len(out), storeID), data), nil
	}}
}

// storeNameNearMember finds the name of storeID among the stores near the
// member's postal code (cached for a day). Best effort: "" if unknown.
func (d *Deps) storeNameNearMember(c *call, storeID int) string {
	m, err := c.c.GetMember(c.ctx)
	if err != nil {
		return ""
	}
	pc, ok := util.NormalizePostalCode(m.Address.PostalCode)
	if !ok {
		return ""
	}
	ss, err := d.stores(c, pc)
	if err != nil {
		return ""
	}
	for _, s := range ss {
		if s.StoreID == storeID {
			return s.Name
		}
	}
	return ""
}

// openEndedAfter is the cutoff beyond which a bonus end date is considered a
// placeholder: one year from today.
func (d *Deps) openEndedAfter() string { return d.today().AddDate(1, 0, 0).Format(day) }
