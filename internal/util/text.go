package util

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaskTail replaces all but the last keep characters of s with '*'.
// Strings no longer than keep are masked entirely.
func MaskTail(s string, keep int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= keep {
		return strings.Repeat("*", len(r))
	}
	return strings.Repeat("*", len(r)-keep) + string(r[len(r)-keep:])
}

// IsYes reports whether v is "yes" (case-insensitive, surrounding spaces
// ignored).
func IsYes(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "yes") }

// Or returns v unless it is the zero value, in which case def.
func Or[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// CleanText strips control characters, trims spaces and truncates to at
// most maxRunes runes.
func CleanText(s string, maxRunes int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
	if utf8.RuneCountInString(s) > maxRunes {
		s = strings.TrimSpace(string([]rune(s)[:maxRunes]))
	}
	return s
}

var accentReplacer = strings.NewReplacer(
	"é", "e", "è", "e", "ë", "e", "ê", "e", "á", "a", "à", "a", "ä", "a", "â", "a",
	"í", "i", "ì", "i", "ï", "i", "î", "i", "ó", "o", "ò", "o", "ö", "o", "ô", "o",
	"ú", "u", "ù", "u", "ü", "u", "û", "u", "ç", "c", "ñ", "n",
	"É", "E", "È", "E", "Ë", "E", "Á", "A", "Ä", "A", "Í", "I", "Ï", "I",
	"Ó", "O", "Ö", "O", "Ú", "U", "Ü", "U", "Ç", "C", "Ñ", "N",
)

// FoldAccents replaces common Latin accented letters with their ASCII base
// letter ("crème brûlée" → "creme brulee").
func FoldAccents(s string) string { return accentReplacer.Replace(s) }

// SplitCSV splits a comma-separated list, trimming spaces and dropping
// empty entries.
func SplitCSV(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// LowerSet turns a comma-separated list into a set of lower-cased entries.
func LowerSet(v string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, s := range SplitCSV(v) {
		out[strings.ToLower(s)] = struct{}{}
	}
	return out
}

// ParseBool reports whether v is a true boolean literal ("1", "t", "true",
// ...). Anything else, including invalid input, is false.
func ParseBool(v string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	return err == nil && b
}

// DayOf reduces a date or date-time string to its YYYY-MM-DD prefix.
func DayOf(date string) string {
	if len(date) >= 10 {
		return date[:10]
	}
	return date
}

// MonthOf reduces a date or date-time string to its YYYY-MM prefix ("" if
// it is shorter than that).
func MonthOf(date string) string {
	if len(date) >= 7 {
		return date[:7]
	}
	return ""
}

// TimeOfDay returns the HH:MM part of a time ("18:00", "18:00:00" or an ISO
// date-time such as "2026-10-03T18:00:00"); "" if there is none.
func TimeOfDay(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, 'T'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) >= 5 && s[2] == ':' {
		return s[:5]
	}
	return ""
}

var postalRe = regexp.MustCompile(`^[1-9][0-9]{3}\s?[A-Za-z]{2}$`)

// NormalizePostalCode validates a Dutch postal code ("1234 ab") and returns
// it in canonical form ("1234AB").
func NormalizePostalCode(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if !postalRe.MatchString(p) {
		return "", false
	}
	return strings.ToUpper(strings.ReplaceAll(p, " ", "")), true
}
