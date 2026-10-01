package util

import (
	"net/url"
	"strings"
)

// CanonicalURL lower-cases scheme and host and drops a trailing slash from
// the path, so "HTTPS://Example.com/mcp/" equals "https://example.com/mcp".
// Query and fragment are discarded. Unparseable or host-less input is
// returned unchanged.
func CanonicalURL(v string) string {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return v
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimSuffix(u.EscapedPath(), "/")
}

// AppendQuery appends encoded query parameters to base, which may already
// carry a query.
func AppendQuery(base string, v url.Values) string {
	if len(v) == 0 {
		return base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + v.Encode()
}

// URLOrigin returns "scheme://host" for URLs with a host and "scheme:" for
// opaque or host-less URIs (e.g. a native app's private-use scheme).
// Unparseable input is returned unchanged.
func URLOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	if u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return u.Scheme + ":"
}

// IsURLUnreserved reports whether c is an RFC 3986 "unreserved" character:
// ALPHA / DIGIT / "-" / "." / "_" / "~".
func IsURLUnreserved(c rune) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}
