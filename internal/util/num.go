// Package util holds small pure helper functions (numbers, text, slices,
// URLs, network addresses) shared across ah-mcp packages. Every function
// here is deterministic, has no side effects and is covered by tests.
// Domain logic does not belong here.
package util

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Round2 rounds to 2 decimals (cents).
func Round2(v float64) float64 { return math.Round(v*100) / 100 }

// Round1 rounds to 1 decimal.
func Round1(v float64) float64 { return math.Round(v*10) / 10 }

// PercentOff returns the discount from was to now in percent, rounded to
// one decimal (0 when there is no discount or was is not positive).
func PercentOff(was, now float64) float64 {
	if was <= 0 || now < 0 || now >= was {
		return 0
	}
	return Round1((was - now) / was * 100)
}

// ParseEuro parses a price string such as "1.29", "1,29" or "€ 1,29" into
// euros rounded to cents. Unparseable input yields 0.
func ParseEuro(s string) float64 {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "€"))
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return Round2(f)
}

// ToFloat converts a decoded JSON number (float64, json.Number or numeric
// string) to float64; anything else yields 0.
func ToFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}
