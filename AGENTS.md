# AGENTS.md

Guidance for AI agents and humans changing this repository. See README.md for
what ah-mcp is and how it works.

## Commands

```sh
go test -race ./...
go vet ./...
gofmt -l .            # must print nothing
golangci-lint run     # v2 config in .golangci.yml
scripts/smoke.sh http://localhost:3000   # against a running instance
```

## Layout

- `cmd/ah-mcp` — entry point (flags, `healthcheck`).
- `internal/config` — env/flag parsing and validation.
- `internal/oauth`, `internal/oauth/seal` — in-house OAuth 2.1 AS, sealed tokens.
- `internal/ahlogin` — AH login reverse proxy, code exchange, refresh.
- `internal/server` — HTTP mux and middleware.
- `internal/ah` — adapters over appie-go (basket, orders, bonus, history).
- `internal/tools` — the MCP tools; `register.go` lists all of them.
- `internal/cache`, `internal/session`, `internal/logx` — infrastructure.
- `internal/util` — pure helper functions (see rule below).
- `internal/testutil/fakeah` — fake AH server with synthetic fixtures.

## Rules

1. **Pure helpers live in `internal/util`.** A small function that is pure (no
   I/O, no globals it mutates; standard-library clock or `math/rand` calls are
   fine) and generic (numbers, text, slices, URLs, network addresses, parsing,
   formatting — not tied to AH or OAuth domain types or policy) goes into
   `internal/util`, gets table tests in `internal/util/*_test.go`, and is
   imported from there. Do not write local copies of such helpers in other
   packages. Domain logic (bonus tier maths, title matching, redirect-URI
   rules, PKCE, token sealing) stays in its domain package with its own tests.
   Code with real side effects (HTTP writers, I/O, crypto-random secrets such
   as tokens and nonces) stays where it is used. Before adding a helper, check
   `internal/util` for an existing one.
2. **Exactly the tools in `register.go`.** No stubs, no hidden tools. The
   README tool catalogue is checked against the registry by a test.
3. **Out of scope stays out:** checkout, delivery slots, order creation,
   order reopen/revert (`ReopenOrder`/`RevertOrder` must stay unreachable —
   a test scans the source), Bonus Box activation, favourite lists, SSE/stdio,
   static bearer tokens, Belgium.
4. **Destructive tools** require `confirm="yes"` and return
   `confirm_required` with a preview otherwise. In `active_order` mode, edits
   of a placed order (`order_status: scheduled` — in the upcoming
   fulfillments or reopened) require confirmation; a draft order (slot chosen
   in the app, not placed) is edited directly, like in the app. Clearing the
   cart always requires confirmation.
5. **Never refresh AH inside a tool call.** AH tokens are refreshed only at
   `/oauth/token`, where the rotated refresh token can be returned.
6. **Secrets never leak:** no tokens, codes, cookies, sids, verifiers or
   emails in logs (use `logx.MemberHash`), tool output, fixtures or commits.
   Test fixtures are synthetic.
7. **Personal data is cached only in `cache.Member(...)` namespaces;**
   baskets, carts, orders and lists are never cached.
8. **Upstream code:** when copying a function or more from an AGPL upstream,
   keep its copyright line, add `// Adapted from <repo>/<path> @ <commit>
   (AGPL-3.0)`, and record the file in `UPSTREAM.md`. Mark AH API calls not
   verified against the real API with `// UNVERIFIED: from <repo>@<sha>` and
   list them in README → Known gaps.
9. **Configuration** comes only from the variables documented in the README
   env table; all public URLs derive from `AH_MCP_BASE_URL`, never from
   `Host`/`X-Forwarded-*`.
10. **Commits follow [Conventional Commits](https://www.conventionalcommits.org/):**
    `<type>(<scope>)?: <summary>` with types such as `feat`, `fix`,
    `refactor`, `docs`, `test`, `ci`, `build`, `chore`; the scope is the
    area (e.g. `tools`, `oauth`, `ahlogin`, `bonus`). Breaking changes use
    `!` after the type/scope and a `BREAKING CHANGE:` footer.
