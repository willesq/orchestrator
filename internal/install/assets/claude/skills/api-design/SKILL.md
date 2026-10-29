---
name: api-design
description: >-
  Best practices for designing, authoring, or reviewing HTTP/JSON REST APIs and
  OpenAPI contracts. Use whenever you are adding/changing an API endpoint, choosing
  a URL/resource shape, picking an HTTP method or status code, designing an error
  format, adding pagination/filtering/sorting, versioning an API, securing an
  endpoint, or reviewing an API for correctness. Triggers: "design an API/endpoint",
  "add a route", "REST API", "what status code", "how should I paginate", "API error
  format", "version this API", "OpenAPI/contract", "review this API", "idempotency",
  "rate limiting", "is this endpoint RESTful". Language-agnostic; contract-first
  (OpenAPI) friendly. Grounded in Google AIP, Microsoft/Zalando guidelines, Stripe,
  the HTTP RFCs, and OWASP API Security Top 10.
---
<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->


# REST/HTTP+JSON API Design

Apply these when authoring or reviewing an HTTP/JSON endpoint or an OpenAPI spec.
Prefer **contract-first**: write/adjust the OpenAPI 3.1 document, then generate
server + client types from it so the two sides can't drift.

> Two things are genuinely project decisions, not universal rules — decide once,
> document it, apply everywhere: **JSON field casing** (`snake_case` per Zalando vs
> `camelCase` per Microsoft) and **versioning transport** (header/media-type vs
> URL path). The failure mode is *mixing* strategies, not picking the "wrong" one.

## 1. Resources & URLs
- **Nouns, plural collections.** `/orders`, `/orders/{id}` — never `/getOrder`, `/createOrder`.
- **No verbs / RPC in URLs.** The one escape hatch (Google AIP-136): a custom action
  as `POST /orders/{id}:cancel` — only when no standard method fits.
- Path segments lowercase (kebab-case is safe); case-sensitive.
- **Shallow:** nesting depth ≤ 3, ~4–8 resource types per API. Flatten deep trees with a filter param.
- **Sub-resources** for real ownership: `/orders/{id}/line-items`. **Singletons** have no id: `/users/{id}/settings`.

## 2. Methods & semantics (RFC 9110)
- **Safe** (no writes): GET, HEAD, OPTIONS. **Idempotent**: GET, HEAD, PUT, DELETE. **POST**: neither.
- **PUT** = full replace. **PATCH** = partial: default to **JSON Merge Patch** (RFC 7396,
  `application/merge-patch+json`; `null` deletes a field). Use **JSON Patch** (RFC 6902,
  ordered add/remove/replace/test ops) only for atomic multi-step or array-element edits.
- **Idempotency-Key header** on POSTs that create/charge/mutate irreversibly (Stripe pattern):
  cache the first result, replay it on retry with the same key.

## 3. Status codes (use the precise one)
- `200` sync success · `201` created (+ `Location`) · `202` async accepted · `204` success, no body.
- `400` unparseable vs **`422` well-formed but validation/semantically invalid**.
- `401` unauthenticated vs `403` authenticated-but-forbidden.
- `404` not found vs `410` gone (you know it existed, won't return).
- `409` state conflict/duplicate · `412` `If-Match` precondition failed · `415` bad media type.
- `429` rate limited (+ `Retry-After`; don't cache) · `500/503` server/unavailable.

## 4. Errors — RFC 9457 `application/problem+json`
Fields: `type` (stable URI, machine-readable), `title`, `status` (== HTTP status),
`detail` (instance-specific), `instance`. Add extension members freely (clients ignore unknowns).
```json
{ "type": "https://api.example.com/errors/out-of-credit",
  "title": "Not enough credit", "status": 403,
  "detail": "Balance is 30, cost is 50.",
  "instance": "/accounts/123/charges/abc",
  "errors": [{ "field": "amount", "reason": "exceeds_balance" }] }
```
Give every error a **stable machine-readable code** (`type` or a `code` field). **Never** leak
stack traces or internal/library exception text. Log the `instance` against the request ID.

## 5. Lists: pagination, filtering, sorting, fields
- **Cursor pagination** for anything large or concurrently mutated (offset skips/dupes rows).
  Request `page_size` (server clamps to a max) + opaque `page_token`; response returns the array
  + `next_page_token` (empty == end). **Tokens must be opaque/unparseable.** (Google AIP-158 / Stripe.)
- Optionally emit RFC 8288 `Link: <…>; rel="next"`.
- **Filter/sort** via query params (`filter[x]=`, `sort=-created_at,name`). **Sparse fieldsets**
  via `?fields=`/`select=` — don't return unrequested fields once the client restricts.
- **No unbounded arrays** in a response — anything growable is paginated, never embedded wholesale.

## 6. Data conventions
- **IDs:** opaque strings (UUID/ULID); never assume internal structure; compare case-sensitively.
- **Timestamps:** RFC 3339, **UTC**, `_at` suffix (`created_at`).
- **Enums:** strings, extensible (document as open so new values don't break clients).
- **Money:** `{ "amount": "12.50", "currency": "USD" }` — never a float.
- Omit fields rather than sending `null` in normal responses (`null` is a Merge-Patch delete signal).

## 7. Versioning & compatibility
- **Additive = no version bump:** new optional fields, new endpoints, new (documented-extensible) enum values.
- **Breaking = new version + deprecation window:** removing/renaming/retyping a field, tightening validation.
- Signal retirement with `Deprecation` / `Sunset` (RFC 8594) response headers + docs.
- A shipped field name/type/semantic is a contract — add-and-deprecate, don't repurpose.

## 8. Concurrency & caching
- **ETag + `If-Match`** on mutating endpoints → `412` on mismatch (optimistic concurrency). Honor preconditions.
- **`If-None-Match` → `304`** for conditional GET; prefer content-hash ETags. `Cache-Control` on cacheable GETs.

## 9. Auth & security — OWASP API Top 10 (2023)
- **BOLA (API1):** on every handler touching an id, verify the caller may access *that specific object* — not just that they're authenticated. (The #1 real-world API breach vector.)
- **Mass assignment / BOPLA (API3):** never bind a raw request body onto an internal/DB model —
  explicit **allow-list** of writable fields per endpoint.
- **Resource exhaustion (API4):** enforce page-size caps, body-size limits, timeouts, rate limits.
- **Function-level auth (API5):** separate admin vs user endpoints; don't rely on hidden routes.
- Bearer tokens / scoped API keys / OAuth2-OIDC; least privilege; TLS only; **no secrets in URLs**
  (query strings get logged); validate untrusted upstream responses too (API10); guard SSRF (API7).

## 10. Async, bulk, observability
- **Long-running** (>~10s): `202` + a pollable operation resource (`{name, done, error, response}`)
  or `operation-location` header; retain status ≥ 24h; support an idempotent operation id.
- **Bulk** with heterogeneous per-item outcomes: `207 Multi-Status`.
- **Observability:** correlation id (`traceparent` / `x-request-id`); tie error `instance` to it.

---

## Checklist — apply to every endpoint

**Always**
- [ ] Noun + plural collection; standard method before a custom `:action`.
- [ ] Precise status code (201+`Location` on create, 204 on empty success, 422 vs 400, 409/412/429).
- [ ] `application/problem+json` errors with a stable machine-readable code; no internal leakage.
- [ ] Cursor pagination + opaque tokens for growable/mutating lists; page-size cap.
- [ ] RFC 3339 UTC timestamps (`_at`); extensible string enums; money as object.
- [ ] OpenAPI 3.1 authored/updated first; types generated for all clients from it.
- [ ] Per-object authorization check (BOLA); allow-list of writable fields (mass-assignment).
- [ ] ETag/`If-Match` where concurrent edits happen; Idempotency-Key on irreversible POSTs.
- [ ] Rate limit + `429` + `Retry-After`; one JSON casing convention project-wide.

**Never**
- [ ] Verbs/RPC in URLs (except the documented `:action` escape hatch).
- [ ] `200` for a create, or `200`+empty body where `204` fits.
- [ ] Leak stack traces / internal exceptions to clients.
- [ ] Offset pagination for large/mutating collections; unbounded arrays in a response.
- [ ] Bind raw request bodies onto internal/DB models; secrets in query strings.
- [ ] Break a shipped field's name/type/semantics without a new version + deprecation.
- [ ] Mix versioning strategies (header vs URL) within one API.

**When REST isn't the right tool:** internal, low-latency, strongly-typed service-to-service →
gRPC/Connect. Client-driven queries over a graph-shaped model → GraphQL. REST+JSON+OpenAPI is the
default for public/partner HTTP APIs needing cacheability, broad tooling, and debuggable payloads.

---

_Sources: Google AIP (aip.dev 136/151/158/193), Microsoft & Zalando REST guidelines, Stripe API,
JSON:API, OpenAPI 3.1; RFC 9110/9457/7396/6902/8288/6585/8594; OWASP API Security Top 10 (2023).
Fuller digest in RAG: `reference://api-design-best-practices`._
