#!/usr/bin/env bash
# Smoke test for a running ah-mcp instance (no AH account needed).
# Usage: scripts/smoke.sh <base-url>     e.g. scripts/smoke.sh http://localhost:3000
set -euo pipefail

BASE="${1:?usage: $0 <base-url>}"
BASE="${BASE%/}"

command -v curl >/dev/null || { echo "curl is required" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

# --- wait for /healthz (up to ~30s) ---------------------------------------
code=000
for _ in $(seq 1 30); do
  code="$(curl -s -o "$TMP/healthz" -w '%{http_code}' "$BASE/healthz" || true)"
  [ "$code" = "200" ] && break
  sleep 1
done
if [ "$code" = "200" ]; then
  pass "/healthz -> 200"
else
  fail "/healthz -> $code (after 30s)"
  echo "server not healthy; aborting" >&2
  exit 1
fi

# --- unauthenticated /mcp -> 401 + resource_metadata -----------------------
code="$(curl -s -o /dev/null -D "$TMP/mcp.h" -w '%{http_code}' \
  -X POST "$BASE/mcp" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  --data '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' || true)"
www="$(grep -i '^www-authenticate:' "$TMP/mcp.h" | tr -d '\r' || true)"
expected_rm="resource_metadata=\"$BASE/.well-known/oauth-protected-resource/mcp\""
if [ "$code" = "401" ] && grep -qi '^www-authenticate: *bearer' <<<"$www" && grep -qF "$expected_rm" <<<"$www"; then
  pass "/mcp -> 401 with $expected_rm"
else
  fail "/mcp -> $code, WWW-Authenticate: ${www:-<missing>}"
fi

# --- protected resource metadata (RFC 9728) --------------------------------
for path in /.well-known/oauth-protected-resource /.well-known/oauth-protected-resource/mcp; do
  if curl -sf "$BASE$path" -o "$TMP/prm.json" &&
    jq -e --arg base "$BASE" \
      '.resource == ($base + "/mcp") and (.authorization_servers | index($base)) != null' \
      "$TMP/prm.json" >/dev/null; then
    pass "$path"
  else
    fail "$path: $(cat "$TMP/prm.json" 2>/dev/null || echo '<no body>')"
  fi
done

# --- authorization server metadata (RFC 8414) ------------------------------
for path in /.well-known/oauth-authorization-server /.well-known/openid-configuration; do
  if curl -sf "$BASE$path" -o "$TMP/asm.json" &&
    jq -e --arg base "$BASE" '
      .issuer == $base
      and (.authorization_endpoint | type) == "string"
      and (.token_endpoint | type) == "string"
      and (.registration_endpoint | type) == "string"
      and (.code_challenge_methods_supported | index("S256")) != null' \
      "$TMP/asm.json" >/dev/null; then
    pass "$path"
  else
    fail "$path: $(cat "$TMP/asm.json" 2>/dev/null || echo '<no body>')"
  fi
done

# --- dynamic client registration (RFC 7591) --------------------------------
reg_endpoint="$(jq -r '.registration_endpoint // empty' "$TMP/asm.json" 2>/dev/null || true)"
reg_endpoint="${reg_endpoint:-$BASE/oauth/register}"
code="$(curl -s -o "$TMP/reg.json" -w '%{http_code}' -X POST "$reg_endpoint" \
  -H 'Content-Type: application/json' \
  --data '{"redirect_uris":["http://127.0.0.1:33418/callback"],"client_name":"smoke"}' || true)"
if [ "$code" = "201" ] && jq -e '(.client_id | type) == "string" and (.client_id | length) > 0' "$TMP/reg.json" >/dev/null; then
  pass "DCR -> 201 with client_id"
else
  fail "DCR -> $code: $(cat "$TMP/reg.json" 2>/dev/null || echo '<no body>')"
fi

if [ "$failures" -gt 0 ]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
