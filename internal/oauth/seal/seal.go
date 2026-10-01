// Package seal encrypts and authenticates the stateless tokens ah-mcp issues
// (client ids, access tokens, refresh tokens).
//
// Each purpose gets its own XChaCha20-Poly1305 key derived with HKDF-SHA256
// from AH_MCP_SECRET_KEY, and the purpose is also bound as associated data,
// so a token of one kind can never be accepted as another. Changing the
// secret invalidates every token ever issued.
package seal

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// Prefix is the version prefix of every sealed value.
const Prefix = "ahm1."

// MaxTokenLen bounds a sealed value. Larger payloads are refused when
// sealing and rejected without decrypting when unsealing.
const MaxTokenLen = 4096

// Purpose separates the key spaces of different token kinds.
type Purpose string

// Token purposes.
const (
	PurposeClient  Purpose = "client"
	PurposeAccess  Purpose = "access"
	PurposeRefresh Purpose = "refresh"
)

var purposes = []Purpose{PurposeClient, PurposeAccess, PurposeRefresh}

// Errors returned by Open.
var (
	ErrInvalid = errors.New("seal: invalid or tampered token")
	ErrExpired = errors.New("seal: token expired")
)

type keySet map[Purpose]cipher.AEAD

// Sealer seals and opens values. It is safe for concurrent use.
type Sealer struct {
	keys keySet
	now  func() time.Time
}

// New derives per-purpose keys from secret.
func New(secret []byte) (*Sealer, error) {
	keys, err := deriveKeys(secret)
	if err != nil {
		return nil, err
	}
	return &Sealer{keys: keys, now: time.Now}, nil
}

// WithClock returns a copy of s using now as its clock (for tests).
func (s *Sealer) WithClock(now func() time.Time) *Sealer {
	c := *s
	c.now = now
	return &c
}

func deriveKeys(secret []byte) (keySet, error) {
	if len(secret) < 32 {
		return nil, errors.New("seal: secret must be at least 32 bytes")
	}
	ks := keySet{}
	for _, p := range purposes {
		k, err := hkdf.Key(sha256.New, secret, nil, "ah-mcp/seal/v1/"+string(p), chacha20poly1305.KeySize)
		if err != nil {
			return nil, err
		}
		aead, err := chacha20poly1305.NewX(k)
		if err != nil {
			return nil, err
		}
		ks[p] = aead
	}
	return ks, nil
}

type envelope struct {
	Exp  int64           `json:"exp"`
	Data json.RawMessage `json:"d"`
}

func aad(p Purpose) []byte { return []byte("ah-mcp/v1/" + string(p)) }

// Seal encrypts v for purpose p, valid until exp.
func (s *Sealer) Seal(p Purpose, v any, exp time.Time) (string, error) {
	aead, ok := s.keys[p]
	if !ok {
		return "", fmt.Errorf("seal: unknown purpose %q", p)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(envelope{Exp: exp.Unix(), Data: data})
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := Prefix + base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, aad(p)))
	if len(out) > MaxTokenLen {
		return "", fmt.Errorf("seal: token too large (%d bytes)", len(out))
	}
	return out, nil
}

// Open verifies and decrypts tok for purpose p into v.
func (s *Sealer) Open(p Purpose, tok string, v any) error {
	if len(tok) > MaxTokenLen || !strings.HasPrefix(tok, Prefix) {
		return ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok[len(Prefix):])
	if err != nil {
		return ErrInvalid
	}
	aead := s.keys[p]
	if aead == nil || len(raw) < aead.NonceSize()+aead.Overhead() {
		return ErrInvalid
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], aad(p))
	if err != nil {
		return ErrInvalid
	}
	var env envelope
	if err := json.Unmarshal(plain, &env); err != nil {
		return ErrInvalid
	}
	if !s.now().Before(time.Unix(env.Exp, 0)) {
		return ErrExpired
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		return ErrInvalid
	}
	return nil
}
