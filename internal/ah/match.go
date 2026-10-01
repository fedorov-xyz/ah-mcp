// Copyright (C) the bramboe/ah-mcp contributors
// Copyright (C) Igor Fedorov and contributors
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Adapted from github.com/bramboe/ah-mcp/tools/dupes.go (findDuplicatePairs)
// and tools/bonusmatch.go (matchStopwords, titleTokens, sharedTokens)
// @ 46b85bc652496590e179a665dda346b206d56291 (AGPL-3.0).
//
// Changes: exported, accent folding added, structured output instead of a
// Dutch text warning.

package ah

import (
	"slices"
	"strings"

	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

// MinSharedTokens is how many significant tokens two titles must share to
// count as near-duplicates or as a bonus match. Two keeps real pairs
// ("lasagne all'uovo": LASAGNE, UOVO) while rejecting brand-only overlap.
const MinSharedTokens = 2

// matchStopwords are title tokens too generic to indicate a product match.
var matchStopwords = map[string]bool{
	"AH": true, "ALLE": true, "DE": true, "HET": true, "EEN": true,
	"OF": true, "EN": true, "MET": true, "VOOR": true, "PER": true,
	"GRAM": true, "GR": true, "KG": true, "ML": true, "LITER": true,
	"STUK": true, "STUKS": true, "PAK": true, "PAKKEN": true, "ZAK": true,
	"FLES": true, "VERPAKKING": true, "VOORDEELPAKKEN": true,
	// Generic descriptors that create false positives.
	"BIOLOGISCH": true, "BIOLOGISCHE": true, "BIO": true,
	"LEKKER": true, "LEKKERE": true, "LATER": true, "READY": true,
	"VERS": true, "VERSE": true, "EXTRA": true, "MINI": true,
	"KLEINVERPAKKING": true, "VOORDEELPAK": true, "VERSAFDELING": true,
	"ZELFBEDIENINGSAFDELING": true,
	// Pack and diet descriptors shared by unrelated products ("zero sugar"
	// cola vs "zero sugar added" muesli, "4-pack" of different flavours).
	"PACK": true, "MULTIPACK": true, "ZERO": true, "SUGAR": true, "SUIKER": true,
	"LIGHT": true, "ADDED": true, "TOEGEVOEGD": true,
}

// TitleTokens reduces a product title to significant, normalised tokens.
func TitleTokens(s string) map[string]bool {
	return util.SetOf(TitleTokenList(s))
}

// TitleTokenList is TitleTokens in title order, without duplicates.
func TitleTokenList(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToUpper(util.FoldAccents(s)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	var tokens []string
	seen := map[string]bool{}
	for _, t := range strings.Fields(b.String()) {
		if len(t) < 3 || matchStopwords[t] || seen[t] {
			continue
		}
		if strings.IndexFunc(t, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
			continue // pure numbers (weights, counts) are not distinctive
		}
		seen[t] = true
		tokens = append(tokens, t)
	}
	return tokens
}

// OfferTitleMatch reports whether a product title (as tokens) matches a
// bonus offer title, and on which tokens: when they share at least
// MinSharedTokens, or when every significant token of the offer is in the
// product title — group offers are often short ("AH Kipgehakt", "Zanetti")
// and then a single shared token is the whole offer.
func OfferTitleMatch(product, offer map[string]bool) (shared []string, subset, ok bool) {
	shared = util.SortedIntersection(product, offer)
	subset = len(offer) > 0 && len(shared) == len(offer)
	return shared, subset, subset || len(shared) >= MinSharedTokens
}

// NamedItem is any cart/order/list entry that can be compared by title.
type NamedItem struct {
	ProductID int    `json:"product_id,omitempty"`
	Title     string `json:"title"`
	Quantity  int    `json:"quantity"`
	Ref       string `json:"ref,omitempty"`
}

// DuplicatePair is two entries considered near-duplicates.
type DuplicatePair struct {
	A      NamedItem `json:"a"`
	B      NamedItem `json:"b"`
	Shared []string  `json:"shared_tokens"`
}

// FindDuplicatePairs returns every pair of distinct items whose titles share
// at least MinSharedTokens significant tokens (the same product family).
//
// Titles that start with the same tokens belong to one brand or product line
// ("Fish Tales …", "Knorr Good potatoes …"): those are near-duplicates only
// when one title adds nothing to the other (e.g. another size), not when they
// name different flavours or products. Titles from different brands are
// near-duplicates when they share at least MinSharedTokens product tokens
// (two brands of "lasagne all'uovo").
func FindDuplicatePairs(items []NamedItem) []DuplicatePair {
	toks := make([][]string, len(items))
	for i, it := range items {
		toks[i] = TitleTokenList(it.Title)
	}
	var pairs []DuplicatePair
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[i].ProductID != 0 && items[i].ProductID == items[j].ProductID {
				continue // same product: quantity, not a duplicate
			}
			if shared, ok := nearDuplicate(toks[i], toks[j]); ok {
				pairs = append(pairs, DuplicatePair{A: items[i], B: items[j], Shared: shared})
			}
		}
	}
	return pairs
}

func nearDuplicate(a, b []string) ([]string, bool) {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	if p > 0 {
		if p == len(a) || p == len(b) {
			return slices.Clone(a[:p]), true
		}
		return nil, false
	}
	shared := util.SortedIntersection(util.SetOf(a), util.SetOf(b))
	return shared, len(shared) >= MinSharedTokens
}
