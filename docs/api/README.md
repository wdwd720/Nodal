# API CONVENTIONS

The public REST contract lives in `openapi/openapi.yaml` (OpenAPI 3.1). The Go server interfaces and the TypeScript client are generated from it (`make openapi-client`, oapi-codegen strict server in `cmd/api`), and CI fails on drift.

## Money and time

- USD values are decimal strings with two fraction digits (`"1234.56"`). Asset quantities are exact base-unit integer strings. Basis points are integers. **No financial value is ever a JSON number.**
- Timestamps are RFC 3339 UTC. Distinct timestamps are exposed where they mean different things (`requested_at`, `received_at`, `observed_at`, `available_at`, `terminal_at`).

## Commands and idempotency (PART 36, 173)

- Every money-affecting command is `POST` with a required `Idempotency-Key` header (8–128 chars). The key is scoped to the authenticated actor and endpoint.
- Same key + same canonical body: the original semantic result is replayed with `200` and the same resource.
- Same key + different body: `409` with code `INVALID_IDEMPOTENCY_REUSE`.
- A command still in flight for the same key: `409` with `IDEMPOTENCY_IN_PROGRESS` and `Retry-After`.
- The web client generates the key when the user first confirms an action and keeps it stable across retries, reconnects, and double clicks.

## Errors (PART 37)

`application/problem+json` per RFC 9457 with an added stable `code` (see `internal/errs/codes.go`), optional `fields` for structured detail, and `request_id`. Internal errors never carry stack traces or upstream messages.

## Queries

`GET` with cursor pagination: `cursor` (opaque) and `limit` (≤ 200). Responses carry `next_cursor` (`null` when exhausted). Histories are never returned unbounded.

## Realtime (PART 109)

`GET /v1/events/stream` is Server-Sent Events. Each event has an `id` cursor; clients send `Last-Event-ID` to resume. On a gap the server emits `resync` and the client must refetch canonical REST state. The stream is a hint, never truth.

## Authentication and authorization

Server-side sessions in an `HttpOnly; Secure; SameSite=Lax` cookie (`__Host-` prefixed when host-only). Unsafe methods require same-origin (`Sec-Fetch-Site`) or an allow-listed `Origin`. Every customer route is tenant-scoped by account ownership; operator routes require operator roles; sensitive routes require step-up (recent MFA/passkey). Agents never hold sessions.

## Admin plane

There is no balance-editing endpoint. Financial repair is a reason-coded compensating journal transaction through the reconciliation resolution route, under dual control when material.
