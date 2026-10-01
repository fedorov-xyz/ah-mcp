package logx

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	secrets := []string{
		"Authorization: Bearer abc.def-ghi",
		"token ahm1.Zm9vYmFyYmF6",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl",
		"/cb?code=SECRETCODE&state=SECRETSTATE",
		"code_verifier=VERIFIER123",
		`{"refresh_token":"RT-1","accessToken":"AT-2"}`,
		"Set-Cookie: ahsession=COOKIEVAL; Path=/",
		"GET /oauth/ah/SIDSIDSID/login",
		"user alice@example.com signed in",
		"password: hunter2",
	}
	leaks := []string{"abc.def-ghi", "Zm9vYmFyYmF6", "c2lnbmF0dXJl", "SECRETCODE", "SECRETSTATE", "VERIFIER123", "RT-1", "AT-2",
		"COOKIEVAL", "SIDSIDSID", "alice@example.com", "hunter2"}
	out := Redact(strings.Join(secrets, "\n"))
	for _, l := range leaks {
		if strings.Contains(out, l) {
			t.Errorf("Redact leaked %q in:\n%s", l, out)
		}
	}
	if !strings.Contains(out, "/oauth/ah/[REDACTED]/login") || !strings.Contains(out, "code=[REDACTED]") {
		t.Errorf("unexpected redaction shape:\n%s", out)
	}
	if got := Redact("GET /mcp 200 in 12ms"); got != "GET /mcp 200 in 12ms" {
		t.Errorf("benign text changed: %q", got)
	}
}

func TestHandlerRedactsAttrsAndMessage(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelDebug)
	log.With("who", "bob@example.com").Info("got code=ABC123",
		"header", "Bearer TOKENVALUE", "err", errors.New("refresh_token=RTVAL failed"),
		slog.Group("g", "email", "carol@example.org"), "n", 3)
	out := buf.String()
	for _, l := range []string{"bob@example.com", "ABC123", "TOKENVALUE", "RTVAL", "carol@example.org"} {
		if strings.Contains(out, l) {
			t.Errorf("log leaked %q: %s", l, out)
		}
	}
	if !strings.Contains(out, `"n":3`) {
		t.Errorf("non-string attr lost: %s", out)
	}
}

func TestMemberHash(t *testing.T) {
	a, b := MemberHash("4242"), MemberHash("4243")
	if a == b || len(a) != 12 || MemberHash("4242") != a || strings.Contains(a, "4242") {
		t.Fatalf("MemberHash: %q %q", a, b)
	}
}
