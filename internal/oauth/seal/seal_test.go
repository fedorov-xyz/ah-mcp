package seal

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

type payload struct {
	A string `json:"a"`
	B int    `json:"b"`
}

func newSealer(t *testing.T, key string) *Sealer {
	t.Helper()
	s, err := New([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const key1 = "0123456789abcdef0123456789abcdef"
const key2 = "fedcba9876543210fedcba9876543210"

func TestRoundTrip(t *testing.T) {
	s := newSealer(t, key1)
	tok, err := s.Seal(PurposeAccess, payload{A: "x", B: 7}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, Prefix) {
		t.Fatalf("missing prefix: %s", tok)
	}
	var got payload
	if err := s.Open(PurposeAccess, tok, &got); err != nil || got.A != "x" || got.B != 7 {
		t.Fatalf("open = %+v, %v", got, err)
	}
	// Ciphertexts are randomised.
	tok2, _ := s.Seal(PurposeAccess, payload{A: "x", B: 7}, time.Now().Add(time.Hour))
	if tok == tok2 {
		t.Fatal("identical tokens for identical payloads")
	}
}

func TestTamper(t *testing.T) {
	s := newSealer(t, key1)
	tok, _ := s.Seal(PurposeAccess, payload{A: "x"}, time.Now().Add(time.Hour))
	raw, _ := base64.RawURLEncoding.DecodeString(tok[len(Prefix):])
	for i := range []int{0, len(raw) / 2, len(raw) - 1} {
		pos := []int{0, len(raw) / 2, len(raw) - 1}[i]
		b := append([]byte(nil), raw...)
		b[pos] ^= 0x01
		bad := Prefix + base64.RawURLEncoding.EncodeToString(b)
		if err := s.Open(PurposeAccess, bad, &payload{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("tampered byte %d accepted: %v", pos, err)
		}
	}
	for _, bad := range []string{"", "ahm1.", "ahm2." + tok[len(Prefix):], "ahm1.!!!", tok[:len(tok)-5]} {
		if err := s.Open(PurposeAccess, bad, &payload{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("garbage %q accepted: %v", bad, err)
		}
	}
}

func TestWrongPurpose(t *testing.T) {
	s := newSealer(t, key1)
	tok, _ := s.Seal(PurposeRefresh, payload{A: "x"}, time.Now().Add(time.Hour))
	for _, p := range []Purpose{PurposeAccess, PurposeClient} {
		if err := s.Open(p, tok, &payload{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("refresh token accepted as %s: %v", p, err)
		}
	}
	if _, err := s.Seal("bogus", payload{}, time.Now().Add(time.Hour)); err == nil {
		t.Error("unknown purpose accepted")
	}
}

func TestExpiry(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s := newSealer(t, key1).WithClock(func() time.Time { return now })
	tok, _ := s.Seal(PurposeAccess, payload{A: "x"}, now.Add(time.Minute))
	if err := s.Open(PurposeAccess, tok, &payload{}); err != nil {
		t.Fatalf("fresh token rejected: %v", err)
	}
	later := s.WithClock(func() time.Time { return now.Add(time.Minute) })
	if err := later.Open(PurposeAccess, tok, &payload{}); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired token: %v", err)
	}
}

// Changing AH_MCP_SECRET_KEY invalidates every issued token ("log everyone
// out"); there is no previous-key fallback.
func TestKeyChangeLogsEveryoneOut(t *testing.T) {
	tok, _ := newSealer(t, key1).Seal(PurposeRefresh, payload{A: "x"}, time.Now().Add(time.Hour))
	if err := newSealer(t, key2).Open(PurposeRefresh, tok, &payload{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("token sealed with the old key accepted: %v", err)
	}
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short secret accepted")
	}
}

func TestSizeLimit(t *testing.T) {
	s := newSealer(t, key1)
	// Realistic access-token payload: AH JWTs are ~1 KB.
	big := struct {
		V      int    `json:"v"`
		Kind   string `json:"kind"`
		Aud    string `json:"aud"`
		CID    string `json:"client_id_hash"`
		Member string `json:"member_id"`
		Email  string `json:"email"`
		AHA    string `json:"ah_access"`
		Iat    int64  `json:"iat"`
		Exp    int64  `json:"exp"`
	}{1, "access", "https://ah-mcp.example.com/mcp", strings.Repeat("a", 32), "123456789", "someone.with.a.long.name@example.com", strings.Repeat("x", 1400), 1, 2}
	tok, err := s.Seal(PurposeAccess, big, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) >= 4096 {
		t.Fatalf("token is %d bytes, want < 4096", len(tok))
	}
	if _, err := s.Seal(PurposeAccess, strings.Repeat("y", 4000), time.Now().Add(time.Hour)); err == nil {
		t.Fatal("oversized payload sealed")
	}
	if err := s.Open(PurposeAccess, Prefix+strings.Repeat("A", MaxTokenLen), &payload{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversized token not rejected")
	}
}
