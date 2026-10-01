package util

import (
	"sort"
	"strings"
)

// Truncate returns at most n elements of xs and whether it dropped any. A
// nil input becomes an empty (non-nil) slice, so it encodes as [] in JSON.
func Truncate[T any](xs []T, n int) ([]T, bool) {
	if len(xs) > n {
		return xs[:n], true
	}
	return NonNil(xs), false
}

// NonNil returns xs, or an empty slice when xs is nil.
func NonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

// FirstNonNil returns the first non-nil pointer, or nil.
func FirstNonNil[T any](ps ...*T) *T {
	for _, p := range ps {
		if p != nil {
			return p
		}
	}
	return nil
}

// UniqueNonEmpty returns the trimmed, non-empty strings of xs in order,
// without duplicates (nil if none remain).
func UniqueNonEmpty(xs ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

// SetOf returns the set of xs.
func SetOf[T comparable](xs []T) map[T]bool {
	m := make(map[T]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// SortedIntersection returns the keys present in both sets, sorted.
func SortedIntersection(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
