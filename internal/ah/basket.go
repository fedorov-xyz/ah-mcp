// Copyright (C) the MaxTomahawk/ah-mcp contributors
// Copyright (C) Igor Fedorov and contributors
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Adapted from github.com/MaxTomahawk/ah-mcp/tools/ahgraphql.go and
// tools/basket_current.go @ a770dfb521f7b3616ac3e592e0baa009c39133ef (AGPL-3.0).
//
// Changes: exported adapter API, *appie.Client-independent interface kept,
// list entries with stable refs, notes PATCH extracted, active-order
// detection kept verbatim.

package ah

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// GraphQLDoer is the slice of *appie.Client the basket adapter needs.
type GraphQLDoer interface {
	DoGraphQL(ctx context.Context, query string, variables map[string]any, result any) error
	DoRequest(ctx context.Context, method, path string, body, result any) error
}

// Money is an AH GraphQL amount.
type Money struct {
	Amount float64 `json:"amount"`
}

// BasketProductPrice holds current and pre-discount prices.
type BasketProductPrice struct {
	Now Money `json:"now"`
	Was Money `json:"was"`
}

// Regular is the price before any (bonus) reduction.
func (p BasketProductPrice) Regular() float64 {
	if p.Was.Amount > p.Now.Amount {
		return p.Was.Amount
	}
	return p.Now.Amount
}

// Reduced is the reduced per-piece price, or 0 when there is none. AH lowers
// it for per-piece deals (e.g. an activated personal "15% korting"); deals
// such as "2 voor X" or "1+1 gratis" are applied to the basket total instead.
func (p BasketProductPrice) Reduced() float64 {
	if p.Was.Amount > p.Now.Amount && p.Now.Amount > 0 {
		return p.Now.Amount
	}
	return 0
}

// BasketProduct is the product attached to a basket item.
type BasketProduct struct {
	ID      int                `json:"id"`
	Title   string             `json:"title"`
	PriceV2 BasketProductPrice `json:"priceV2"`
}

// BasketItem is a product line in the list or the order.
type BasketItem struct {
	ID                int           `json:"id"`
	Quantity          int           `json:"quantity"`
	AllocatedQuantity int           `json:"allocatedQuantity,omitempty"`
	OriginCode        string        `json:"originCode,omitempty"`
	Position          int           `json:"position,omitempty"`
	IsStrikethrough   bool          `json:"isStrikethrough,omitempty"`
	IsClosed          bool          `json:"isClosed,omitempty"`
	Product           BasketProduct `json:"product"`
}

// ProductID returns the item's webshop product id.
func (i BasketItem) ProductID() int {
	if i.Product.ID != 0 {
		return i.Product.ID
	}
	return i.ID
}

// BasketNote is a free-text shopping-list entry.
type BasketNote struct {
	Description     string `json:"description"`
	Quantity        int    `json:"quantity"`
	SearchTerm      string `json:"searchTerm,omitempty"`
	OriginCode      string `json:"originCode,omitempty"`
	Position        int    `json:"position,omitempty"`
	IsStrikethrough bool   `json:"isStrikethrough,omitempty"`
}

// BasketPrice is the basket price summary.
type BasketPrice struct {
	PriceBeforeDiscount Money `json:"priceBeforeDiscount"`
	PriceAfterDiscount  Money `json:"priceAfterDiscount"`
	TotalPrice          Money `json:"totalPrice"`
	Discount            Money `json:"discount"`
}

// BasketSummary is the basket summary.
type BasketSummary struct {
	Price         BasketPrice `json:"price"`
	Quantity      int         `json:"quantity"`
	IsCancellable bool        `json:"isCancellable"`
	ShoppingType  string      `json:"shoppingType"`
	DeliveryDate  string      `json:"deliveryDate"`
}

// Basket is the GraphQL basket: the shopping list (ItemsInList + Notes) and,
// when an online order exists, its items (ItemsInOrder).
type Basket struct {
	ID                string        `json:"id"`
	ItemsInOrder      []BasketItem  `json:"itemsInOrder"`
	ItemsInList       []BasketItem  `json:"itemsInList"`
	Notes             []BasketNote  `json:"notes"`
	Summary           BasketSummary `json:"summary"`
	CanChangeDelivery bool          `json:"canChangeDelivery"`
}

// BasketMutationItem is a basketItemsAdd/Update input.
type BasketMutationItem struct {
	ID       int `json:"id,omitempty"`
	Quantity int `json:"quantity"`
}

type basketMutationResult struct {
	Status       string `json:"status"`
	ErrorMessage string `json:"errorMessage"`
	Result       Basket `json:"result"`
}

// basketFields intentionally contains only fields present in the
// authenticated api.ah.nl GraphQL schema.
//
// From MaxTomahawk/ah-mcp@a770dfb. Verified against the live API on 2026-10-01.
const basketFields = `
    id
    canChangeDelivery
    itemsInList {
      id
      quantity
      originCode
      position
      isStrikethrough
      product {
        id
        title
        priceV2 {
          now { amount }
          was { amount }
        }
      }
    }
    itemsInOrder {
      id
      quantity
      allocatedQuantity
      originCode
      position
      isClosed
      product {
        id
        title
        priceV2 {
          now { amount }
          was { amount }
        }
      }
    }
    notes {
      description
      quantity
      searchTerm
      originCode
      position
      isStrikethrough
    }
    summary {
      quantity
      isCancellable
      shoppingType
      deliveryDate
      price {
        priceBeforeDiscount { amount }
        priceAfterDiscount { amount }
        totalPrice { amount }
        discount { amount }
      }
    }
`

// FetchBasket reads the basket.
//
// From MaxTomahawk/ah-mcp@a770dfb (query MCPBasket). Verified against the live API on 2026-10-01.
func FetchBasket(ctx context.Context, c GraphQLDoer) (Basket, error) {
	query := `query MCPBasket {
  basket {` + basketFields + `  }
}`
	var resp struct {
		Basket Basket `json:"basket"`
	}
	if err := c.DoGraphQL(ctx, query, nil, &resp); err != nil {
		return Basket{}, fmt.Errorf("get basket: %w", err)
	}
	return resp.Basket, nil
}

// AddBasketProducts adds products to the basket list.
//
// From MaxTomahawk/ah-mcp@a770dfb (mutation basketItemsAdd). Verified against the live API on 2026-10-01.
func AddBasketProducts(ctx context.Context, c GraphQLDoer, items []BasketMutationItem) (Basket, error) {
	mutation := `mutation MCPBasketItemsAdd($items: [BasketMutation!]!) {
  basketItemsAdd(items: $items) {
    status
    errorMessage
    result {` + basketFields + `    }
  }
}`
	var resp struct {
		Mutation basketMutationResult `json:"basketItemsAdd"`
	}
	if err := c.DoGraphQL(ctx, mutation, map[string]any{"items": items}, &resp); err != nil {
		return Basket{}, fmt.Errorf("add basket items: %w", err)
	}
	if err := checkMutationStatus("basketItemsAdd", resp.Mutation.Status, resp.Mutation.ErrorMessage); err != nil {
		return Basket{}, err
	}
	return resp.Mutation.Result, nil
}

// UpdateBasketProducts sets product quantities in the basket list (0
// removes).
//
// From MaxTomahawk/ah-mcp@a770dfb (mutation basketItemsUpdate). Verified against the live API on 2026-10-01.
func UpdateBasketProducts(ctx context.Context, c GraphQLDoer, items []BasketMutationItem) (Basket, error) {
	mutation := `mutation MCPBasketItemsUpdate($items: [BasketMutation!]!) {
  basketItemsUpdate(items: $items) {
    status
    errorMessage
    result {` + basketFields + `    }
  }
}`
	var resp struct {
		Mutation basketMutationResult `json:"basketItemsUpdate"`
	}
	if err := c.DoGraphQL(ctx, mutation, map[string]any{"items": items}, &resp); err != nil {
		return Basket{}, fmt.Errorf("update basket items: %w", err)
	}
	if err := checkMutationStatus("basketItemsUpdate", resp.Mutation.Status, resp.Mutation.ErrorMessage); err != nil {
		return Basket{}, err
	}
	return resp.Mutation.Result, nil
}

func checkMutationStatus(operation, status, message string) error {
	// Some AH GraphQL mutations omit status while still returning a valid result.
	if status == "" || strings.EqualFold(status, "SUCCESS") {
		return nil
	}
	if message == "" {
		message = status
	}
	return fmt.Errorf("%s failed: %s", operation, message)
}

// NotePatch is a free-text list change (Quantity 0 removes the note).
type NotePatch struct {
	Description   string `json:"description,omitempty"`
	Quantity      int    `json:"quantity"`
	Type          string `json:"type"`
	OriginCode    string `json:"originCode"`
	SearchTerm    string `json:"searchTerm,omitempty"`
	StrikeThrough bool   `json:"strikeThrough"`
}

const shoppingListPatchPath = "/mobile-services/shoppinglist/v2/items?orderBy=userInput&orderByParam=0"

// PatchNotes adds or removes free-text notes via the v2 shopping-list REST
// endpoint.
//
// From MaxTomahawk/ah-mcp@a770dfb (patchV2ShoppingList). Verified against the live API on 2026-10-01.
func PatchNotes(ctx context.Context, c GraphQLDoer, notes []NotePatch) error {
	if len(notes) == 0 {
		return nil
	}
	for i := range notes {
		if notes[i].Type == "" {
			notes[i].Type = "SHOPPABLE"
		}
		if notes[i].OriginCode == "" {
			notes[i].OriginCode = "PRD"
		}
	}
	var discard any
	if err := c.DoRequest(ctx, http.MethodPatch, shoppingListPatchPath, map[string]any{"items": notes}, &discard); err != nil {
		return fmt.Errorf("patch shopping list notes: %w", err)
	}
	return nil
}

// HasActiveOrder reports whether the basket belongs to a real online order
// (a delivery/pickup slot was chosen in the AH app).
//
// UNVERIFIED: heuristic from MaxTomahawk/ah-mcp@a770dfb (basketHasActiveOrder).
// Only the "no active order" case was verified (2026-10-01); detection of a
// real active order has not been observed yet.
func (b Basket) HasActiveOrder() bool {
	return b.ItemsInOrder != nil || b.Summary.ShoppingType != "" || b.Summary.DeliveryDate != ""
}

// ListEntry is one shopping-list line with a stable ref.
type ListEntry struct {
	Ref       string  `json:"ref"`
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	ProductID int     `json:"product_id,omitempty"`
	Quantity  int     `json:"quantity"`
	Checked   bool    `json:"checked"`
	Price     float64 `json:"price,omitempty"`
	// BonusPrice is the reduced per-piece price, when AH reports one.
	BonusPrice float64 `json:"bonus_price,omitempty"`
	position   int
}

// ListEntries flattens the basket list into entries with stable refs:
// "product:<id>" and "note:<index in Notes>".
func (b Basket) ListEntries() []ListEntry {
	entries := make([]ListEntry, 0, len(b.ItemsInList)+len(b.Notes))
	for _, item := range b.ItemsInList {
		pid := item.ProductID()
		entries = append(entries, ListEntry{
			Ref:        fmt.Sprintf("product:%d", pid),
			Kind:       "product",
			Name:       item.Product.Title,
			ProductID:  pid,
			Quantity:   item.Quantity,
			Checked:    item.IsStrikethrough,
			Price:      item.Product.PriceV2.Regular(),
			BonusPrice: item.Product.PriceV2.Reduced(),
			position:   item.Position,
		})
	}
	for i, note := range b.Notes {
		entries = append(entries, ListEntry{
			Ref:      fmt.Sprintf("note:%d", i),
			Kind:     "note",
			Name:     note.Description,
			Quantity: max(note.Quantity, 1),
			Checked:  note.IsStrikethrough,
			position: note.Position,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		// AH occasionally omits position. Keep response order in that case.
		if entries[i].position == 0 || entries[j].position == 0 {
			return false
		}
		return entries[i].position < entries[j].position
	})
	return entries
}
