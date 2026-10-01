package util

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// IsLoopbackHost reports whether host (optionally in [brackets]) is
// "localhost" or a loopback IP literal.
func IsLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// InPrefixes reports whether a is inside any of ps.
func InPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ParsePrefixes parses a comma-separated list of CIDRs or bare IPs (a bare
// IP becomes a single-address prefix).
func ParsePrefixes(v string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range SplitCSV(v) {
		if !strings.Contains(s, "/") {
			addr, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("invalid IP or CIDR %q", s)
			}
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", s)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT
	netip.MustParsePrefix("169.254.0.0/16"), // link-local / cloud metadata
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 can reach private IPv4
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("fc00::/7"),       // unique local
}

// IsPublicAddr reports whether addr is a globally routable unicast
// address: not loopback, private, link-local, multicast, unspecified,
// CGNAT, NAT64 or otherwise reserved. IPv4-mapped IPv6 is checked as IPv4.
func IsPublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	return !InPrefixes(addr, nonPublicPrefixes)
}

// ParseRetryAfter parses a Retry-After header value given in seconds or as
// an HTTP date (relative to now). A date in the past yields 0.
func ParseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// Backoff returns the delay before retry number attempt (1-based): base
// doubled per attempt, capped at maxd, with random jitter in [d/2, d).
func Backoff(base, maxd time.Duration, attempt int) time.Duration {
	if base <= 0 || maxd <= 0 {
		return 0
	}
	d := min(base, maxd)
	for i := 1; i < attempt && d < maxd; i++ {
		d = min(d*2, maxd) // never overflows: d < maxd before doubling
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(d-half))) //nolint:gosec // retry jitter, not a secret
}
