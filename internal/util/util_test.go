package util

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestRound(t *testing.T) {
	for _, c := range []struct{ in, r2, r1 float64 }{
		{1.005, 1.0, 1.0}, {1.239, 1.24, 1.2}, {-0.555, -0.56, -0.6}, {0, 0, 0}, {2.25, 2.25, 2.3},
	} {
		if got := Round2(c.in); got != c.r2 {
			t.Errorf("Round2(%v) = %v, want %v", c.in, got, c.r2)
		}
		if got := Round1(c.in); got != c.r1 {
			t.Errorf("Round1(%v) = %v, want %v", c.in, got, c.r1)
		}
	}
}

func TestParseEuro(t *testing.T) {
	for in, want := range map[string]float64{
		"1.29": 1.29, "1,29": 1.29, " € 2,5 ": 2.5, "€3": 3, "": 0, "abc": 0, "NaN": 0, "1.234": 1.23,
	} {
		if got := ParseEuro(in); got != want {
			t.Errorf("ParseEuro(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestToFloat(t *testing.T) {
	for _, c := range []struct {
		in   any
		want float64
	}{
		{1.5, 1.5}, {3, 3}, {json.Number("2.25"), 2.25}, {"4", 4}, {"x", 0}, {nil, 0}, {true, 0},
	} {
		if got := ToFloat(c.in); got != c.want {
			t.Errorf("ToFloat(%#v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestMaskTail(t *testing.T) {
	for _, c := range []struct {
		in   string
		keep int
		want string
	}{
		{"2620000000017", 4, "*********0017"}, {"123", 4, "***"}, {"", 4, ""}, {" 12345 ", 4, "*2345"}, {"ïëü12", 2, "***12"},
	} {
		if got := MaskTail(c.in, c.keep); got != c.want {
			t.Errorf("MaskTail(%q,%d) = %q, want %q", c.in, c.keep, got, c.want)
		}
	}
}

func TestIsYes(t *testing.T) {
	for in, want := range map[string]bool{"yes": true, " YES ": true, "Yes": true, "y": false, "true": false, "": false, "no": false} {
		if got := IsYes(in); got != want {
			t.Errorf("IsYes(%q) = %v", in, got)
		}
	}
}

func TestOr(t *testing.T) {
	if Or("", "d") != "d" || Or("v", "d") != "v" || Or(0, 5) != 5 || Or(3, 5) != 3 {
		t.Fatal("Or misbehaves")
	}
}

func TestCleanText(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want string
	}{
		{"  Claude\n Code\t ", 50, "Claude Code"}, {"abcdef", 3, "abc"}, {"héllo wörld", 5, "héllo"}, {"\x00\x7f", 5, ""},
	} {
		if got := CleanText(c.in, c.max); got != c.want {
			t.Errorf("CleanText(%q,%d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

func TestFoldAccents(t *testing.T) {
	if got := FoldAccents("Crème brûlée Ñandú ÉÖ"); got != "Creme brulee Nandu EO" {
		t.Fatalf("FoldAccents = %q", got)
	}
}

func TestSplitCSVAndLowerSet(t *testing.T) {
	if got := SplitCSV(" a, ,b ,, c"); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("SplitCSV = %#v", got)
	}
	if got := SplitCSV(""); got != nil {
		t.Fatalf("SplitCSV(empty) = %#v", got)
	}
	set := LowerSet("A@Example.com, b@x.nl ,")
	if _, ok := set["a@example.com"]; !ok || len(set) != 2 {
		t.Fatalf("LowerSet = %#v", set)
	}
}

func TestParseBool(t *testing.T) {
	for in, want := range map[string]bool{"true": true, "1": true, " TRUE ": true, "false": false, "": false, "yes": false} {
		if got := ParseBool(in); got != want {
			t.Errorf("ParseBool(%q) = %v", in, got)
		}
	}
}

func TestDayOf(t *testing.T) {
	for in, want := range map[string]string{"2026-06-27T09:00:00Z": "2026-06-27", "2026-06-27 09:00": "2026-06-27", "2026-06": "2026-06", "": ""} {
		if got := DayOf(in); got != want {
			t.Errorf("DayOf(%q) = %q", in, got)
		}
	}
}

func TestNormalizePostalCode(t *testing.T) {
	for in, want := range map[string]string{"1234AB": "1234AB", "1234 ab": "1234AB", " 9999zz ": "9999ZZ"} {
		if got, ok := NormalizePostalCode(in); !ok || got != want {
			t.Errorf("NormalizePostalCode(%q) = %q,%v", in, got, ok)
		}
	}
	for _, bad := range []string{"0123AB", "123AB", "1234A", "1234ABC", "", "ABCD12"} {
		if _, ok := NormalizePostalCode(bad); ok {
			t.Errorf("NormalizePostalCode(%q) accepted", bad)
		}
	}
}

func TestTruncateNonNil(t *testing.T) {
	got, cut := Truncate([]int{1, 2, 3}, 2)
	if !reflect.DeepEqual(got, []int{1, 2}) || !cut {
		t.Fatalf("Truncate = %v,%v", got, cut)
	}
	got, cut = Truncate([]int{1}, 2)
	if len(got) != 1 || cut {
		t.Fatalf("Truncate short = %v,%v", got, cut)
	}
	empty, _ := Truncate[int](nil, 5)
	if empty == nil {
		t.Fatal("Truncate(nil) must be non-nil")
	}
	if b, _ := json.Marshal(NonNil[string](nil)); string(b) != "[]" {
		t.Fatalf("NonNil(nil) encodes as %s", b)
	}
}

func TestFirstNonNil(t *testing.T) {
	a, b := 1, 2
	if FirstNonNil[int](nil, &a, &b) != &a || FirstNonNil[int](nil, nil) != nil || FirstNonNil[int]() != nil {
		t.Fatal("FirstNonNil misbehaves")
	}
}

func TestSortedIntersection(t *testing.T) {
	got := SortedIntersection(map[string]bool{"UOVO": true, "LASAGNE": true, "X": true}, map[string]bool{"LASAGNE": true, "UOVO": true, "Y": true})
	if !reflect.DeepEqual(got, []string{"LASAGNE", "UOVO"}) {
		t.Fatalf("SortedIntersection = %v", got)
	}
	if SortedIntersection(nil, map[string]bool{"a": true}) != nil {
		t.Fatal("expected nil")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for in, want := range map[string]bool{"localhost": true, "LOCALHOST": true, "127.0.0.1": true, "127.8.9.1": true, "::1": true, "[::1]": true,
		"0.0.0.0": false, "example.com": false, "10.0.0.1": false, "": false} {
		if got := IsLoopbackHost(in); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v", in, got)
		}
	}
}

func TestParsePrefixesAndInPrefixes(t *testing.T) {
	ps, err := ParsePrefixes("10.0.0.0/8, 192.168.1.7 ,fd00::/8")
	if err != nil || len(ps) != 3 {
		t.Fatalf("ParsePrefixes = %v, %v", ps, err)
	}
	for in, want := range map[string]bool{"10.1.2.3": true, "192.168.1.7": true, "192.168.1.8": false, "fd12::1": true, "8.8.8.8": false} {
		if got := InPrefixes(netip.MustParseAddr(in), ps); got != want {
			t.Errorf("InPrefixes(%s) = %v", in, got)
		}
	}
	for _, bad := range []string{"10.0.0.0/33", "nope", "1.2.3.4/x"} {
		if _, err := ParsePrefixes(bad); err == nil {
			t.Errorf("ParsePrefixes(%q) accepted", bad)
		}
	}
	if ps, err := ParsePrefixes(""); err != nil || ps != nil {
		t.Fatalf("ParsePrefixes(empty) = %v, %v", ps, err)
	}
}

func TestIsPublicAddr(t *testing.T) {
	for in, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.0.0.1": false, "172.16.5.4": false, "192.168.0.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fe80::1": false, "fd00::1": false, "224.0.0.1": false,
		"::ffff:10.0.0.1": false, "::ffff:8.8.8.8": true, "64:ff9b::a00:1": false, "198.18.0.1": false, "255.255.255.255": false,
	} {
		if got := IsPublicAddr(netip.MustParseAddr(in)); got != want {
			t.Errorf("IsPublicAddr(%s) = %v, want %v", in, got, want)
		}
	}
	if IsPublicAddr(netip.Addr{}) {
		t.Fatal("zero addr must not be public")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"3", 3 * time.Second, true}, {" 0 ", 0, true}, {"-1", 0, false}, {"", 0, false}, {"soon", 0, false},
		{now.Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
	} {
		got, ok := ParseRetryAfter(c.in, now)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseRetryAfter(%q) = %v,%v want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCanonicalURL(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://Example.COM/mcp/":      "https://example.com/mcp",
		"https://example.com":           "https://example.com",
		"https://example.com/mcp?x=1#f": "https://example.com/mcp",
		"not a url":                     "not a url",
		"/relative":                     "/relative",
	} {
		if got := CanonicalURL(in); got != want {
			t.Errorf("CanonicalURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAppendQuery(t *testing.T) {
	v := url.Values{"code": {"a b"}, "state": {"s"}}
	if got := AppendQuery("https://x.test/cb", v); got != "https://x.test/cb?code=a+b&state=s" {
		t.Errorf("AppendQuery = %q", got)
	}
	if got := AppendQuery("https://x.test/cb?k=1", v); got != "https://x.test/cb?k=1&code=a+b&state=s" {
		t.Errorf("AppendQuery existing = %q", got)
	}
	if got := AppendQuery("https://x.test/cb", nil); got != "https://x.test/cb" {
		t.Errorf("AppendQuery empty = %q", got)
	}
}

func TestURLOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://claude.ai/api/mcp/auth_callback": "https://claude.ai",
		"http://127.0.0.1:33418/cb":               "http://127.0.0.1:33418",
		"cursor://anysphere.cursor/oauth":         "cursor://anysphere.cursor",
		"com.example.app:/callback":               "com.example.app:",
		"%zz":                                     "%zz",
	} {
		if got := URLOrigin(in); got != want {
			t.Errorf("URLOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsURLUnreserved(t *testing.T) {
	for _, c := range "AZaz09-._~" {
		if !IsURLUnreserved(c) {
			t.Errorf("%q should be unreserved", c)
		}
	}
	for _, c := range "+/=% ?#&é" {
		if IsURLUnreserved(c) {
			t.Errorf("%q should be reserved", c)
		}
	}
}

func TestBackoff(t *testing.T) {
	ms := time.Millisecond
	for _, c := range []struct {
		base, maxd time.Duration
		attempt    int
		lo, hi     time.Duration // jitter window [lo, hi)
	}{
		{500 * ms, 10 * time.Second, 1, 250 * ms, 500 * ms},
		{500 * ms, 10 * time.Second, 2, 500 * ms, time.Second},
		{500 * ms, 10 * time.Second, 3, time.Second, 2 * time.Second},
		{500 * ms, 10 * time.Second, 100, 5 * time.Second, 10 * time.Second}, // capped, no overflow
		{500 * ms, 10 * time.Second, 0, 250 * ms, 500 * ms},                  // attempt < 1 acts as 1
		{20 * time.Second, 10 * time.Second, 1, 5 * time.Second, 10 * time.Second},
	} {
		for range 50 {
			if got := Backoff(c.base, c.maxd, c.attempt); got < c.lo || got >= c.hi {
				t.Fatalf("Backoff(%v,%v,%d) = %v, want in [%v,%v)", c.base, c.maxd, c.attempt, got, c.lo, c.hi)
			}
		}
	}
	if Backoff(0, time.Second, 1) != 0 || Backoff(time.Second, 0, 1) != 0 {
		t.Fatal("non-positive durations must yield 0")
	}
}

func TestMonthOf(t *testing.T) {
	for in, want := range map[string]string{"2026-09-27T10:00:00Z": "2026-09", "2026-09-27": "2026-09", "2026-09": "2026-09", "2026": "", "": ""} {
		if got := MonthOf(in); got != want {
			t.Errorf("MonthOf(%q) = %q", in, got)
		}
	}
}

func TestPercentOff(t *testing.T) {
	for _, c := range []struct{ was, now, want float64 }{
		{5.37, 4.60, 14.3}, {7.16, 5.98, 16.5}, {2, 1, 50}, {2, 2, 0}, {2, 3, 0}, {0, 1, 0}, {2, -1, 0},
	} {
		if got := PercentOff(c.was, c.now); got != c.want {
			t.Errorf("PercentOff(%v, %v) = %v, want %v", c.was, c.now, got, c.want)
		}
	}
}

func TestUniqueNonEmpty(t *testing.T) {
	if got := UniqueNonEmpty(" *M.u.v. bio ", "", "*M.u.v. bio", "Korting bij afrekenen", "  "); !reflect.DeepEqual(got, []string{"*M.u.v. bio", "Korting bij afrekenen"}) {
		t.Fatalf("UniqueNonEmpty = %#v", got)
	}
	if UniqueNonEmpty("", " ") != nil {
		t.Fatal("want nil")
	}
}

func TestSetOf(t *testing.T) {
	if got := SetOf([]string{"a", "b", "a"}); !reflect.DeepEqual(got, map[string]bool{"a": true, "b": true}) {
		t.Fatalf("SetOf = %v", got)
	}
	if len(SetOf[int](nil)) != 0 {
		t.Fatal("SetOf(nil) not empty")
	}
}

func TestTimeOfDay(t *testing.T) {
	for in, want := range map[string]string{"18:00": "18:00", "08:30:00": "08:30", "2026-10-03T18:00:00Z": "18:00", "": "", "noon": "", "2026-10-03": ""} {
		if got := TimeOfDay(in); got != want {
			t.Errorf("TimeOfDay(%q) = %q, want %q", in, got, want)
		}
	}
}
