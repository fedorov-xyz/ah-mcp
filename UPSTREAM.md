# Upstream sources

ah-mcp is not a git fork, but a derivative work of the AGPL-3.0 projects
below. Every adapted file keeps the upstream copyright line and an
`// Adapted from <repo>/<path> @ <commit> (AGPL-3.0)` header. See README →
"Upstream sources & how to sync" for the sync checklist.

## Reviewed commits

| Source | License | Commit reviewed | Date | Use |
|---|---|---|---|---|
| [gwillem/appie-go](https://github.com/gwillem/appie-go) | AGPL-3.0 | `65f41abdeaffb4b990cc35525cc0f20837102e1c` | 2026-09-23 | Go dependency, pinned to `v0.0.13-0.20260923073138-65f41abdeaff` (`main`, not tag `v0.0.12`). Login proxy and `exchangeCode` shapes, client id constant. |
| [mrserzhan/ah-mcp](https://github.com/mrserzhan/ah-mcp) | AGPL-3.0 | `d10ddf8e554f9c52a796cd1bd7bd295cddb7e70e` | 2026-03-21 | Base layout, AH login reverse proxy (via Jelle7), env names. |
| [Jelle7/ah-mcp](https://github.com/Jelle7/ah-mcp) | AGPL-3.0 | `9ee133696d52ceeb689c3cde1d07eb8a1e0a7019` | 2026-09-17 | Login proxy with secret scoping, cookie sanitising, Origin allowlist, `refreshMu` idea. |
| [MaxTomahawk/ah-mcp](https://github.com/MaxTomahawk/ah-mcp) | AGPL-3.0 | `a770dfb521f7b3616ac3e592e0baa009c39133ef` | 2026-09-05 | GraphQL basket, shopping-list vs active-order mode, list removal, stable refs, past orders, frequent items. |
| [bramboe/ah-mcp](https://github.com/bramboe/ah-mcp) | AGPL-3.0 | `46b85bc652496590e179a665dda346b206d56291` | 2026-07-22 | Bonus tiers (`computeTiers`, `bestTierPrice`), periods, sections, savings, duplicate detection. |
| [wmeints/happie](https://github.com/wmeints/happie) | none | `83e33bd74984680f8e788f28e4df4607b60f8044` | 2026-08-29 | Idea only (merge receipts with delivered orders). No code copied. |
| [ivo-toby/appie-mcp](https://github.com/ivo-toby/appie-mcp) | none | `a16810a1f8786b302355086efed4ab4aa11b4807` | 2026-07-30 | Idea only (remote-callback UX notes). No code copied. |

All Go forks use `mark3labs/mcp-go` v0.45; ah-mcp uses v1.1.1, so tool and
transport code was rewritten rather than copied.

## Adapted files

| File | Adapted from | What |
|---|---|---|
| `internal/ahlogin/proxy.go` | Jelle7/ah-mcp `auth.go` @ `9ee1336`, appie-go `login.go` @ `65f41ab` | Reverse proxy, `appie://login-exit` rewrite, cookie sanitising, body rewriting. Served from the root of the main port, selected per sign-in by a one-time cookie (a path prefix breaks AH's Next.js login app). |
| `internal/ah/products.go` | appie-go `products.go` @ `65f41ab` | Search and product detail decoded with the bonus fields appie-go drops (`discountLabels`, bonus period, promotion type). |
| `internal/ah/details.go` | appie-go `order.go`, `receipts.go` @ `65f41ab` | Order details decoded with the delivery date kept; receipt list paged past the first 100. |
| `internal/ahlogin/ahlogin.go` | appie-go `auth.go` / `client.go` @ `65f41ab` | `exchangeCode` / refresh request shapes (via exported `DoRequest`), `clientIDForSite("nl")` constant. Short, not marked as adapted code. |
| `internal/server/middleware.go` | Jelle7/ah-mcp `main.go` @ `9ee1336` | `allowedOrigins` / `originMiddleware`. |
| `internal/ah/basket.go` | MaxTomahawk/ah-mcp `tools/ahgraphql.go`, `tools/basket_current.go` @ `a770dfb` | Basket types, fields, add/update mutations, notes `PATCH`, active-order heuristic, `product:`/`note:` refs. |
| `internal/ah/orders.go` | MaxTomahawk/ah-mcp `tools/orders.go` @ `a770dfb`; bramboe/ah-mcp `tools/savings.go` @ `46b85bc` | Closed fulfillments query, frequent-items counting, online-order discounts. |
| `internal/ah/bonus.go` | bramboe/ah-mcp `tools/bonus.go`, `tools/bonusmatch.go` @ `46b85bc` | Bonus-page types, `computeTiers`, `bestTierPrice`, `applyPricing`, `selectPeriods`, section collection, period-aware group query. |
| `internal/ah/match.go` | bramboe/ah-mcp `tools/dupes.go`, `tools/bonusmatch.go` @ `46b85bc` | `findDuplicatePairs`, stopwords, title tokens. |
| `internal/ah/history.go` | bramboe/ah-mcp `tools/savings.go` @ `46b85bc` | Discount categories, receipt savings aggregation. |
| `internal/tools/cart.go`, `internal/tools/list.go` | MaxTomahawk/ah-mcp `tools/basket_current.go`, `tools/orders_current.go` @ `a770dfb` | Behaviour of the cart/list tools (mode reporting, verified removal) — reimplemented for mcp-go v1, no code copied verbatim. |

## Changelog

One entry per sync with an upstream. Format:
`YYYY-MM-DD — <area>: <repo> <old sha> → <new sha>; what was ported or skipped`.

- 2026-10-01 — initial import at the commits listed above. Deliberate
  divergences from the upstream code:
  - Login proxy: served from the root of our origin and selected per sign-in
    by a one-time cookie (upstream used a dedicated listener; a path prefix
    breaks AH's Next.js login app); cookies are also sanitised on the final
    `appie://` redirect.
  - Own adapters where appie-go drops fields: products (`discountLabels`,
    bonus period, promotion type; the Nutri-Score is taken from search, as
    the detail endpoint lacks it), order details and the active order
    (delivery date and window), receipts (paged past the first 100).
  - Bonus: tier maths per piece when AH quotes a group deal for the whole set
    (`price_quantity`); AH's conditions, store-only and stapel flags are
    passed through; personal bonus is read-only (no activation).
  - History: open and closed orders, receipt lines merged by webshop id or
    name, `by_month` and `coverage`; partial results are not cached.
  - Near-duplicates: overlap in brand or product line alone is not a
    duplicate.
  - Cart: confirmation follows the order state (draft vs placed); order
    reopen/revert is not reachable.
