# DECISION REGISTER

Every meaningful deviation from, or concretisation of, the goal architecture. Never silently change architecture.

Format per entry: decision · original recommendation · chosen implementation · why · evidence · consequences · migration impact.

Last updated: 2026-09-06

---

## D-001 — Repository is greenfield; adopt goal monorepo layout verbatim

- **Original recommendation:** "Preserve good current layout if it exists."
- **Chosen:** No prior layout existed. Adopt the PART 14 layout exactly.
- **Why:** nothing to preserve; the recommended layout is a sound modular monolith.
- **Evidence:** Stage 0 audit 2026-09-05 — only the goal file existed.
- **Consequences:** single Go module at root; `internal/` packages are the domain boundaries.
- **Migration impact:** none.

## D-002 — Go module path `github.com/nodal/controlplane`

- **Original recommendation:** brand-neutral; "Nodal" only as internal codename if already present.
- **Chosen:** module path `github.com/nodal/controlplane`. The codename appears only in the internal module path, never in public schemas, API paths, DB identifiers, or UI copy. User-facing name comes from `PUBLIC_PRODUCT_NAME` config.
- **Why:** an importable path is required; the codename was already present in the workspace name and goal file. Changing later is a mechanical rename.
- **Consequences:** `go.mod` and imports carry the codename.
- **Migration impact:** trivial if brand/org changes.

## D-003 — Modular monolith, multiple binaries, single Go module

- **Chosen:** one `go.mod`; binaries in `cmd/{api,execution-worker,reconciliation-worker,market-ingest-worker,agent-worker,workflow-worker,audit-worker,migrate}` share `internal/` packages. gRPC/protobuf only where a process boundary is a security requirement (execution-worker ↔ signing service); everything else in-process.
- **Why:** avoids distributed-monolith failure modes while preserving IAM blast-radius separation at the binary/task-role level.
- **Consequences:** `proto/` initially holds signing and internal admin contracts only.

## D-004 — Postgres access: pgx v5 + sqlc, hand-written SQL allowed for transactional financial paths

- **Original recommendation:** explicit SQL, pgx, sqlc or equivalent compile-time checked layer.
- **Chosen:** pgx v5 pool; sqlc-generated typed queries for repositories; hand-written explicit SQL constants (covered by integration tests) permitted for multi-statement transactional paths (reservation commit, journal posting) where lock ordering and CTE control matter. No ORM anywhere.
- **Why:** sqlc gives compile-time checking; critical paths need precise control. sqlc needs a prebuilt binary on this cgo-less host (SB-005).
- **Consequences:** `make sqlc` regenerates; CI verifies generated code is current.

## D-005 — Migrations: goose v3 as a library, embedded SQL, forward-only for ledger tables

- **Chosen:** `migrations/` holds numbered `.sql` files with goose annotations; `cmd/migrate` runs them via `embed.FS` under the dedicated migration DB role. Ledger/journal tables have no destructive down migrations.
- **Why:** mature, embeddable, checksum-tracked, no external binary.

## D-006 — Authentication: OIDC-generic IdentityProvider + server-side sessions

- **Original recommendation:** standards-based; provider abstraction if no provider decision exists; no homemade password crypto.
- **Chosen:** `internal/auth` defines `IdentityProvider` (OIDC authorization-code + PKCE), server-side sessions in Postgres with opaque cookie tokens (HttpOnly/Secure/SameSite, rotation, revocation, device listing), MFA/passkeys/step-up delegated to the IdP via `acr`/`amr`/`auth_time` claims and enforced by our step-up policy. A dev-only IdP lives in a dev/test package and is rejected in PROD programmatically.
- **Why:** no provider chosen (EB-014); OIDC is provider-portable.
- **Consequences:** production requires an OIDC issuer; passkey UX depends on the chosen IdP.

## D-007 — Exact numerics

- **Chosen:** USD minor units `int64` with checked arithmetic; basis points `int64`; asset base-unit quantities as a `big.Int`-backed value type serialized as decimal strings; DB `NUMERIC(38,0)` for asset quantities, `BIGINT` for USD minor units; `Price{Mantissa, Scale, QuoteAsset, Source, At}`; every rounding call takes an explicit `RoundingMode`.
- **Why:** PART 17.

## D-008 — Event bus: Redpanda (franz-go) behind `EventBus`; transactional outbox/inbox in Postgres

- **Chosen:** outbox rows written in the same transaction as financial state; relay worker publishes to Redpanda; inbox table keyed by (source, message_id) with a unique constraint; in-memory bus only in a dev/test package and rejected in PROD.

## D-009 — Workflow engine: Temporal Go SDK behind `WorkflowEngine`

- **Chosen:** Temporal for funding lifecycle, reconciliation escalation, strategy promotion, withdrawal (capability disabled), provider remediation. Not per market tick. Workflow state is never balance truth.

## D-010 — Local development infrastructure via docker-compose

- **Chosen:** Postgres 16, Redis 7, Redpanda, ClickHouse, Temporal (auto-setup + UI), MinIO (S3-compatible with Object Lock) for the local archive. Same interfaces as production managed services. Ports chosen to avoid host conflicts (SB-002 note).

## D-011 — Frontend: Next.js App Router, strict TS, TanStack Query, zod, generated OpenAPI client, Playwright

- **Chosen:** `apps/web`; generated client in `packages/generated-client` from `openapi/openapi.yaml`; CI checks drift.

## D-012 — Testing tiers and provider verification labels

- **Chosen:** unit; property (`pgregory.net/rapid`); race (`-race` on capital/ledger/execution/reconciliation/event); fuzz (native Go fuzzing); contract (`test/contract`, recorded provider fixtures); integration (`test/integration`, requires `DATABASE_URL`, skipped with explicit reason when absent); e2e (Go API-level + Playwright); chaos; load (k6); security. Provider integrations carry exactly one label: CODE_COMPLETE / CONTRACT_TESTED / SANDBOX_VERIFIED / CANARY_VERIFIED / LIVE_VERIFIED / BLOCKED_EXTERNAL.

## D-014 — ID-token verification is stdlib-based until go-jose is pinned (temporary deviation from D-006)

- **Original recommendation:** D-006 chose `coreos/go-oidc` for OIDC verification.
- **Chosen (2026-09-05):** `internal/auth/oidc` implements discovery, JWKS caching, and ID-token verification (RS/PS/ES families; iss, aud, azp, exp, nbf, iat, nonce; fuzzed) using only the Go standard library, because `go-oidc` transitively requires `github.com/go-jose/go-jose/v4`, which was not in the pinned module set while six agents built in parallel and `go.mod` was frozen.
- **Why:** unblock Stage 1 without concurrent `go.mod` edits. Standard-library RSA/ECDSA signature verification is not bespoke cryptography; the claim checks are exhaustive and tested.
- **Consequences:** two verification code paths would exist if both were kept. **Follow-up (tracked in MASTER_BUILD_STATE next work):** pin `go-jose/v4`, switch `Exchange` to `oidc.IDTokenVerifier`, keep the existing negative tests as the contract.
- **Migration impact:** none (interface unchanged).
- **Completed (2026-09-06):** `internal/auth/oidc` now runs discovery through `coreos/go-oidc` (`oidc.NewProvider`; `InsecureIssuerURLContext` and the `Skip*`/`Insecure*` verifier options are never used) and verifies ID tokens with `oidc.IDTokenVerifier` (JWS parse against the algorithm allow-list, signature, `iss`, `aud`, `exp`, `nbf`). The verifier is built with `oidc.NewVerifier` over a package-local `oidc.KeySet` (go-jose) instead of `RemoteKeySet`, because `RemoteKeySet` refetches the JWKS on every unknown kid with no rate limit, returns one untyped error for unknown-kid and bad-signature, and cannot drop `use=enc`/foreign-alg/<2048-bit keys; the key set keeps the `JWKSMinRefresh` bound and the typed `ErrUnknownKey`/`ErrBadSignature`. Checks that stay explicit: `azp` (multi-audience), `nbf`/`iat` against the configured `ClockSkew` (go-oidc has no `exp` tolerance, so it is given a clock running `ClockSkew` behind; its `nbf` leeway is fixed at 5 min), constant-time nonce, non-empty `sub`, `amr`/`acr`/`auth_time`, step-up. Code exchange with PKCE stays on `golang.org/x/oauth2`. Every issuer response body is capped at 1 MiB by a transport wrapper and the client carries a timeout. Stdlib JWKS/JWS code (`jwks.go`, the old `verify.go`) deleted; the negative tests (bad nonce, wrong audience, expired/skew, issuer mismatch, alg none, bad signature, unknown kid + rotation, missing amr on step-up) pass unchanged; `FuzzVerifyNeverPanics` now drives raw tokens through `Exchange` via the `authtest` server. Accepted difference: a token whose `exp` equals exactly now+skew is now accepted (go-oidc's comparison is inclusive at the boundary), and `EdDSA` is configurable (not default).

## D-015 — Migration numbering ranges refined

- **Original:** CONVENTIONS first draft put gates/admin at 00700–00799.
- **Chosen:** policy/authority tables (gates, kill switches, eligibility/risk policies and decisions, admin actions) live at 00150–00199 because instruments/intents (00200+) reference `eligibility_decisions`, `risk_decisions`, and `admin_actions` by foreign key. 00700+ is reserved for audit proof (Merkle checkpoints, signatures, archive manifests).
- **Evidence:** ordered apply of the drafts failed on 00201 until renumbered; the full chain 00001–00601 now applies cleanly (`cmd/migrate up` + `verify: ok`).
- **Addendum 2 (same day) — ranges retired for new files:** goose applies migrations in version order and refuses an out-of-order version below the database's current version (correct for production safety: a migration can never be applied "late" into a live schema). Domain-numbered ranges therefore only worked while the schema was drafted in one pass. From now on every new migration takes the next number after the highest existing one (`00602_assets_fiat_kind.sql` was the first). Existing files keep their numbers. `cmd/migrate create` must allocate max+1 (follow-up).
- **Addendum (same day):** `audit_events` and `provider_events` moved from 00020/00030 to 00106/00107. Below `ProtectedVersion` every Down must be a real, reversible rollback (the foundation rollback test depends on it); history tables therefore must live at ≥ 00100 where `DownTo` is refused. No environment other than local test databases had applied the old numbers.

## D-016 — Application DB role has no default table privileges

- **Original:** local role bootstrap granted `SELECT, INSERT, UPDATE, DELETE ON TABLES` to `cp_app` by default for everything the migration role creates.
- **Problem found (2026-09-05):** the session-store integration test showed `cp_app` could DELETE sessions; by the same mechanism it would have had UPDATE on `ledger_balances`, defeating the SECURITY DEFINER trigger design, and DELETE on every history table.
- **Chosen:** `cp_app` receives **no** default table privileges (only sequence usage). Every migration grants exactly what the application needs per table; a table without an explicit grant is unreadable by the app, which fails closed. `cp_readonly`/`cp_ops` keep default SELECT. Enforced by `test/integration/migrations/privileges_test.go` (no DELETE anywhere, no UPDATE on append-only tables, runner bookkeeping invisible, read roles cannot write).
- **Consequences:** local databases created before this change carried the old defaults; `docker/postgres/init/001_roles.sql`, `scripts/testdb`, and the migration suite's `resetSchema` were updated and the live local databases were altered in place.
- **Migration impact:** none to schema; production role bootstrap (Terraform) must apply the same default-privilege policy.

## D-017 — DEFICIT ledger account is credit-normal

- **Original:** FINANCIAL_MODEL §2.1 first draft listed `DEFICIT` as debit-normal ("receivable") and §2.2's example posting `Dr DEFICIT / Cr CAPITAL` did not produce the stated net effect.
- **Found by:** the ledger implementation's property test for the deficit pattern (2026-09-06): with a debit-normal DEFICIT the required net effect (WALLET −covered, CAPITAL −amount, DEFICIT +shortfall) is unreachable, because balanced postings force `Σ debit-normal == Σ credit-normal`.
- **Chosen:** `DEFICIT` is credit-normal (what the customer owes the platform); the deficit transaction is `Dr CAPITAL shortfall / Cr DEFICIT shortfall`. `ledger.EnsureAccount` refuses stored rows whose normal side disagrees with the chart. FINANCIAL_MODEL §2.1/§2.2 corrected.
- **Consequences:** buying power treats a positive DEFICIT as a restriction (account is FROZEN anyway); reports render it as "amount owed".

## D-018 — Ledger validation tightened

- COMPENSATION/CORRECTION postings require `reversal_of` and `metadata.reason_code`; RECONCILIATION_ADJUSTMENT requires a reason code; SEED postings are refused unless the composition root enables them (LOCAL/TEST only); AGENT principals are refused (FORBIDDEN). Same-account entries apply the normal side first so valid postings never trip the negative-balance trigger transiently.

## D-019 — Third-party modules pinned for Stages 4–10

- `github.com/gagliardetto/solana-go v1.23.0` (+ `gagliardetto/binary`) for Solana transaction (de)serialisation and RPC JSON types — chosen over a hand-written decoder because versioned (v0) transactions and address-lookup-table resolution are error-prone; the inspector still parses SPL/System/ComputeBudget instruction data itself and rejects anything it cannot decode (SB-005 resolved).
- `github.com/mr-tron/base58` for addresses.
- `github.com/privy-io/go-sdk v0.15.0` for the wallet/signing provider (computes the undocumented authorization-signature canonicalisation).
- `github.com/go-jose/go-jose/v4 v4.1.5` so `coreos/go-oidc` can replace the stdlib ID-token verifier (D-014 follow-up).
- `github.com/twmb/franz-go v1.21.6` (Redpanda/Kafka), `go.temporal.io/sdk v1.48.0`, `github.com/anthropics/anthropic-sdk-go v1.71.0` (NL strategy compiler with schema-constrained output).
- Added 2026-09-06 for Stages 11 and 13: `github.com/aws/aws-sdk-go-v2 v1.46.0` with `config v1.33.3`, `service/s3 v1.111.0`, `service/kms v1.59.0`, `service/secretsmanager v1.48.0`; `github.com/ClickHouse/clickhouse-go/v2 v2.48.0`. Also pinned earlier for oapi-codegen output: `github.com/getkin/kin-openapi v0.149.0`, `github.com/oapi-codegen/runtime v1.7.0`, `github.com/oapi-codegen/nullable v1.2.0`.
- 2026-09-06: pinning the AWS/ClickHouse modules rewrote `go.sum` without the transitive `go.uber.org/zap` entries that `solana-go` needs (they had been added by an agent's `-modfile` workaround, not by `go get`); repaired with `go get go.uber.org/zap` (records the already-selected version, no upgrade). Lesson: after any `go get`, run `go build ./...` before launching agents.
- Rule: `go.mod` changes are made only by the integrator between agent waves; agents never run `go get`/`go mod tidy`.

## D-020 — Migration numbers are reserved per agent within a wave

- To let parallel agents add schema safely under the linear-numbering rule (D-015), each wave assigns disjoint number blocks (e.g. 00605–00609 intent/quote, 00610–00614 execution/settlement, …). Gaps are harmless; a fresh database applies whatever exists in order. Agents provision fresh isolated databases, so an agent's database may lack a sibling's later-added lower number — acceptable for isolated test databases only, never for shared environments.

## D-021 — Permission matrix extended for envelope, promotion, break-glass and withdrawal review

- **Why:** the admin package's dual-control kinds (`ENVELOPE_AUTHORITY_CHANGE`, `AGENT_PROMOTE`, `BREAK_GLASS_GRANT`, `WITHDRAWAL_APPROVE`) had no dedicated approve-side permission and were falling back to two holders of the propose permission.
- **Chosen (2026-09-06):** added `envelope:authority_write` (RISK, ADMIN), `envelope:approve` (dual), `agent:promote` (OPERATIONS, RISK, ADMIN), `agent:promote_approve` (dual), `break_glass:approve` (dual), `withdrawal:review` (COMPLIANCE, FINANCE, ADMIN). Dual-control permissions remain held by no standing role (BREAK_GLASS only, time-boxed). Golden matrix updated to 42 permissions.
- `break_glass:approve` is deliberately a standing SECURITY/ADMIN permission (not dual-control): the first elevation must be approvable by a second human who does not yet hold an elevation; approver ≠ proposer is still enforced.
- `internal/admin` kind specs now use these permissions (ENVELOPE_AUTHORITY_CHANGE: authority_write → envelope:approve; WITHDRAWAL_APPROVE: withdrawal:review → withdrawal:approve; BREAK_GLASS_GRANT: request → break_glass:approve; AGENT_PROMOTE: agent:promote → agent:promote_approve).
- `internal/capital.EnvelopeAdmin` intentionally stays account-scoped for any non-agent principal: a customer allocates their own capital to their own agents; operator-initiated changes go through the admin action kind.

## D-022 — Funding provider states may skip forward; platform states never do

- **Context:** Stripe onramp webhooks are delivered unordered and retried; a `fulfillment_complete` event can arrive before `fulfillment_processing`.
- **Chosen:** provider-owned deposit states (SESSION_CREATED → CUSTOMER_ACTION_REQUIRED → PROVIDER_PROCESSING → PROVIDER_CONFIRMED) may be skipped forward by a later provider status; platform-owned states (SETTLEMENT_OBSERVED, RECONCILED, AVAILABLE, REVERSED) never skip and each requires its evidence (chain receipt, agreed quantity, journal transaction). Every skip still writes a transition row per intermediate state.
- **Consequence:** `funding.Transitions` distinguishes owner per state; `TestProp_LegalWalksNeverReachForbiddenState` covers it. `funding.reversed` is published on the registered `funding.deposit.transitioned` topic with `event=funding.reversed` (no separate topic in the registry; adding one is a follow-up in `internal/event/topics.go`).

## D-023 — Jupiter Swap V2 has no status endpoint

- Execution status and finality come only from chain observers (`internal/chain`); the Jupiter client's `Status` returns `UNSUPPORTED` by design, and `Execute` is `UNKNOWN_EFFECT_WRITE` with exactly one HTTP attempt ever (contract test asserts call count 1 on timeout).

## D-024 — Outbox retry deadline is persisted, not derived

- **Problem:** the relay computed a row's retry eligibility as `recorded_at + min(base·2^(attempts−1), max)`; once `max` had elapsed since `recorded_at`, a permanently failing row was eligible on every poll (hot loop bounded only by the run-level backoff).
- **Chosen (2026-09-06):** migration 00642 adds `outbox_events.next_attempt_at` (default `now()`, set to `recorded_at` on enqueue); the relay claims `WHERE published_at IS NULL AND next_attempt_at <= now` and on failure sets `next_attempt_at = failure_time + backoff(attempts)` (doubling from `RetryBackoffBase`, capped at `RetryBackoffMax`, overflow-free). Ordering stays `(recorded_at, id)` so the per-partition guard is unchanged.
- **Test change (PART 235):** `TestIntegration_RelayRetriesWithBackoff` encoded the old derived timing (second retry at `recorded_at + 2s`); it now asserts failure-based timing (`recorded_at + 3s`). The old expectation was the defect being fixed.

## D-025 — Observer agreement never picks the optimistic answer

- `internal/chain.AgreementPolicy`: FINALIZED requires both observers; any disagreement on found/not-found or token deltas is DISAGREED and blocks dependent activity; a single healthy observer caps finality at CONFIRMED with a degraded note. `ProvenAbsent` (both NOT_FOUND and height beyond `lastValidBlockHeight` + margin) is the only condition under which a new attempt may be built (PART 48, 196). Helius streams and webhooks are hints only; every event is re-read over RPC.

## D-026 — Signing replay returns stored signed bytes; the provider is called once per attempt

- `signing_decisions` is immutable, so a replayed `Sign` for the same attempt cannot re-sign; migration 00616 adds `signing_results` holding the signed transaction bytes per decision. Concurrent replays serialize on the attempt row lock; exactly one provider call is proven by the integration test. `SignTransaction` is treated as UNKNOWN_EFFECT_WRITE regardless of the provider's idempotency-key claim.
- Token program instruction layouts are hand-written and checked against SPL specification vectors because `solana-go/programs/token` does not build with the pinned module set; the Jupiter v6 layout is UNVERIFIED (SB-007).

## D-032 — Every producer's outbox topic must be in the registry, proven by parsing the source

- **The defect:** `internal/capital` deliberately does not import `internal/event`. It declares a narrow `Emitter` interface taking a topic `string`, so the financial core does not depend on the transport. That decoupling is right, but nothing forced capital's topic constants and the registry to agree, and they drifted badly: capital emits 13 topics and the registry knew 4 of them. **Nine were unregistered**, so the first `capital.reservation.locked` emitted through a real outbox fails with `event: unknown topic`. Every affected package's own tests passed the whole time, because each was internally consistent. It surfaced only when the reconciliation agent wired a real `*event.Outbox` as `capital.Emitter` and hit it at the first `LockForOrder`.
- **Why the existing test could not catch it:** `internal/event/topics_test.go` compares the registry against a hand-maintained list of constants in the same file. That proves the registry agrees with itself, not that it agrees with its producers. A hand-maintained list is the drift, not the guard against it.
- **Chosen:** register the nine missing topics (`capital.reservation.locked`, `capital.hold.{placed,released}` under a new `withdrawal_hold` aggregate since a hold is keyed by hold id, and `capital.envelope.{created,updated,status_changed,pnl_applied,exhausted,undeployed}`), and add `test/contract/eventtopics`, which walks every non-generated package under `internal/` with `go/parser`, finds every `Topic*` constant whose value matches the documented topic grammar, and asserts each is registered. A new producer topic that nobody registers now fails at the moment it is written rather than at the first live emission.
- **The negative control matters.** A passing structural test is worthless unless it can fail, so the guard was verified by unregistering `capital.reservation.locked` and confirming the test failed naming the exact constant and its source line (`TopicReservationLocked at internal/capital/emitter.go:26`), then restoring it.
- `capital.envelope.changed` is kept though capital never emits it, because `internal/stream/hub.go` maps it; it is a consumer-side placeholder, and the test only requires producers ⊆ registry.

## D-041 — Connection strings are redacted from logs, by key and by value

- **Found by** the adversarial security suite while writing the log-redaction control. `observability.deniedKeys` implements PART 190's list and `MaskString` covers bearer tokens, base58 Solana keys and PEM blocks. Nothing covered `scheme://user:password@host`, and no `dsn`/`database_url` key was denied, so `log.Info("connecting", "dsn", url)` would have printed a live database credential.
- **Not exploitable today**, and the reporting agent said so plainly rather than inflating it: `internal/config` holds every DSN as a `SecretRef` that renders `[REDACTED]`, `internal/db` never echoes the URL into an error, and pgconn's own parse error redacts the password. It was fixed anyway, because that is three separate behaviors all having to keep holding forever, and one stray log line defeats all three.
- **Chosen:** both halves, because either alone is insufficient. `dsn`, `database_url`, `connection_string`, `conn_string` and `conninfo` join the key denylist; and `MaskString` gained a userinfo rule, because the credential can travel under any key at all or embedded in a longer message.
- **The mask keeps what an operator needs.** `postgres://cp_app:s3cr3t@db.internal:5432/controlplane?sslmode=require` becomes `postgres://cp_app:[REDACTED]@db.internal:5432/controlplane?sslmode=require` — host, port, database and username survive, which is the whole reason anyone reads a connection log line. A URL with no credential is left completely untouched, so logged endpoints stay readable.
- **The agent did NOT assert this in its test**, on the correct grounds that PART 190 does not list connection strings, and documented the boundary instead. Deciding to widen the rule beyond the specification is an integrator call, and it is recorded here rather than buried in a test.

## D-042 — Request binding runs before authorization; accepted, with the reasoning

- **Observed:** `authorizeMiddleware` is an oapi-codegen *strict-handler* middleware, so the generated parameter and body decoder runs first. An unauthenticated caller therefore receives `400 VALIDATION_FAILED`, not `401`, for a malformed UUID or integer in a path, query or body position. Measured: of 299 injection probes, 161 are answered 401 and the other 138 — all in UUID- or int-typed positions — are answered 400 regardless of session.
- **Accepted rather than restructured.** The binder touches nothing stateful, so there is no data leak, and the only thing the differing status reveals is that a route exists and what shape its parameters are — both already published in `openapi/openapi.yaml`. The information gained by an attacker is nil.
- **What would be required to change it** is a pre-routing authorization decision, which means duplicating route knowledge outside the generated server. That is a second source of truth about which routes are public, and a second source of truth about authorization is a far worse failure mode than a status code that reveals nothing.
- The security suite encodes this honestly in `injectionProbe.reachesHandler` rather than asserting a 401 that does not happen.

## D-038 — The Idempotency-Key charset is narrow, not merely printable

- **Found by** the adversarial security suite, which asserted that a key of `{"a":1,"b":[2,3]}` or `' OR 1=1--abcdefgh` must be refused. The server accepted both, because it enforced only "8–128 printable ASCII" — and the spec declared even less, just `minLength`/`maxLength`. So the test and the contract disagreed, and the test was internally inconsistent too: it exempted an all-spaces key on the grounds that printable ASCII *was* the contract while demanding rejection of two other printable-ASCII keys.
- **Chosen:** tighten the contract rather than relax the test. `openapi.yaml` now declares `pattern: '^[A-Za-z0-9._:-]+$'`, and `internal/httpapi.validateIdempotencyKey` enforces the same charset.
- **Why, given it was not exploitable:** the key becomes part of `idempotency_keys`' primary key, is echoed in responses, and is written to structured logs and audit records. Restricting it at the edge means none of those sinks has to be the place that gets quoting right, today and forever, for a value an untrusted client chose. It costs legitimate callers nothing — `newIdempotencyKey()` returns a UUID, and every key this repo generates is already a UUID or a dash-joined token.
- **What was NOT changed, deliberately:** the raw-CRLF case. `abcdefgh

X-Injected: yes` down a socket is header smuggling, and Go's parser resolves it correctly by reading two headers, leaving an ordinary request with a legal 8-character key. Demanding a 4xx there would demand that the server reject a well-formed request. The test now asserts what actually matters: the smuggled header changed nothing, was never reflected, and never became part of a stored key.

## D-039 — Path identifiers must be canonical UUIDs, and the check reads the same path the router does

- **Found by** the IDOR suite: `/v1/accounts/{01a0…}`, `/v1/accounts/urn:uuid:01a0…` and the undashed 32-character form were all accepted as aliases for one account. `google/uuid`'s parser is generous and the generated server normalises before a handler sees the value, so one resource was addressable by four strings.
- **This was not an authorization bypass** — the tenant check still ran and returned only the caller's own account. It is refused anyway, because every control that keys off the raw path rather than the parsed value silently splits across those spellings: per-resource rate-limit buckets, cache keys, a WAF or proxy rule naming a resource, and the audit trail an operator greps to reconstruct who touched what.
- **Chosen:** `internal/httpapi/canonicalid.go`, a middleware that refuses any path segment which parses as a UUID but is not written canonically. Segments that are not UUID-shaped are left alone, so opaque path values are unaffected. Case is not the test — RFC 4122 makes hex input case-insensitive — shape is.
- **A detail that mattered:** the first version read `r.URL.EscapedPath()`, reasoning that a percent-encoded delimiter must not be decoded before the check. That let `{uuid}` through, because it arrives as `%7B01a0…%7D`, which does not parse as a UUID, so the check waved it past and the router then decoded and bound it anyway. Reading `r.URL.Path` — the same string chi routes on — is the only way the check and the router cannot disagree.

## D-040 — Two test defects that looked like findings

Recorded because each cost real diagnosis time and each is a pattern that will recur.

- **A count assertion that encoded a wrong assumption.** `TestIdempotency_KeysAreScopedPerPrincipal` asserted exactly 2 rows on a shared key and found 4, while per-principal scoping was in fact correct. Two layers record under one key and spell the actor differently: the HTTP command layer writes `actor_id = "<uuid>"` with the operation id as endpoint, and `internal/intent` writes `actor_id = "USER:<uuid>"` with endpoint `intent.submit`. The primary key is `(actor_id, endpoint, key)`, so one POST legitimately produces two rows. It now asserts the property — no row on a shared key belongs to an actor who did not submit it — which also catches a third layer appearing later, something the count never would have.
- **A forgery case that failed inside the HTTP client.** The `stored_hash` case presented `sessions.token_hash` as a cookie to prove that a stolen database dump does not authenticate. It passed the raw 32 bytes, which is not a legal cookie value, so Go's client refused to transmit and the subtest failed before reaching the server. Base64url-encoding it makes the case testable, and it passes: the stored hash does not authenticate.

## D-037 — The relay runs on every replica; single-publisher is an operator switch, not a safety net

- **Chosen:** `CP_RELAY_WORKER_EXCLUSIVE` defaults to **false**. Every replica relays. The Postgres advisory-lock leader election is kept, off by default, as a deliberate operational posture — draining by hand into a fragile downstream, or bisecting a consumer-side ordering complaint without a fleet writing underneath you — and its doc comment now describes it as an operator switch rather than a mitigation, stating the failover cost so nobody enables it casually.
- **Why it was default-true and why that ended:** it was a deliberate response to D-036, shipped rather than pretending the ordering break did not exist. Two independent reasons retire it now that the break is fixed. It could never be airtight — the lock is not taken inside the relay's claim transaction, so a holder whose lease connection dies mid-pass still overlaps a standby for that pass, and a safety property that holds "almost always" is not one to build on. And it trades failover latency for nothing: one active relay is a hard ceiling on drain rate, and an abruptly-dead holder releases the lock only when Postgres reaps the connection, minutes later, during which a live standby relays nothing while the backlog grows. A singleton with slow failover is the wrong shape for the one worker whose entire purpose is keeping lag low.
- **The ordering property is now defended by tests that can fail, which is the part that matters.** `TestIntegration_ConcurrentRelaysPublishExactlyOnceAndInPartitionOrder` (4 instances, 150 events, 15 partitions, batch 5) and `TestIntegration_OrderingSurvivesConcurrentRelaysAndAMidPartitionFailure` (3 instances, restored from the 1 it had been reduced to while ordering was known-broken — byte-for-byte the configuration that produced the original failure). Verified by the integrator: **stubbing the D-036 guard makes both fail**, naming the out-of-order partitions; restoring it makes both pass.
- **A concurrency test that never actually shares a partition proves nothing**, so both assert `splitPartitions() > 0` — the count of partitions two instances demonstrably both published into — with a failure message telling the next author how to fix the precondition. Held across 16 executions.

## D-036 — Per-partition ordering is checked per ROW, because a claimed batch is not contiguous

- **A real ordering defect that shipped.** `Relay.claim` uses `FOR UPDATE SKIP LOCKED`, so a batch is "the oldest rows nobody else holds", **not** a contiguous run of the outbox. The order check consulted only the OLDEST row of each partition in the batch and then cleared every other row of that partition on the strength of that one answer. When a second relay instance held a row in the MIDDLE of a partition, the batch head was genuinely unblocked and the rows after the held one published straight past it. **A consumer would see an aggregate's 4th event before its 2nd.**
- **How it was found, and why nothing caught it earlier.** `internal/event` had `TestIntegration_TwoRelaysNeverDoublePublish`, which covers exactly-once across two relays but says nothing about order. The defect surfaced one level up, when the relay worker's own multi-instance ordering test failed with `order.transitioned/order-3` publishing `…0352` before `…034a`. The package's own comments also contradicted each other: `doc.go` documented the limitation while `relay.go` claimed the opposite.
- **Chosen:** `blockedSQL` now runs against **every** claimed row rather than one per partition, and excludes older rows that belong to the same batch. That exclusion is not optional — claimed rows are still `published_at IS NULL` inside the transaction, so without it every row would be blocked by its own predecessors and no partition could advance more than one event per pass. `blockedPartitions` became `blockedRows`, and the publish loop marks the partition blocked the moment it defers a row, so everything behind it waits too. Rows are sorted, so one carried flag is sufficient.
- **Regression test placed where the defect lived**, not only where it was observed: `TestIntegration_RelayNeverPublishesPastARowHeldByAnotherInstance` in `internal/event`, which also asserts the partition drains in full order once the holder releases. Verified non-vacuous by stubbing the blocked set to empty and watching it fail.
- **The reporting agent's conduct is the pattern to copy:** it was told not to modify `internal/event`, so it reported the defect with a deterministic repro, pinned current behaviour in a test marked for inversion, and shipped a leader-election mitigation while stating plainly that it was a mitigation and not a proof. Shipping a relay that reorders financial events the moment it scales to two replicas would have been far worse than shipping one that elects a leader.

## D-034 — An unknown publish outcome is reported as FAILURE, never success

- **The defect, found by a test that was written to find it.** The Redpanda producer used `ProduceSync`. With the broker container paused, `ProduceSync` **blocked for 634 seconds and then returned `nil`** — a completely stalled broker looked like a successful publish. None of the obvious knobs bound that case: `RecordDeliveryTimeout` is only evaluated for a batch that is not already in flight, `ProduceRequestTimeout` is a value the *broker* honours and a paused broker honours nothing, and `ProduceSync`'s context aborts only buffering, never an in-flight batch. A stalled-but-open connection leaves the batch in flight indefinitely.
- **Chosen:** bound `Publish` itself with `Produce` plus a promise and a `select` on a deadline, and report a timeout as a **failure** even though the record may still land afterwards.
- **Why the asymmetry is deliberate:** an unknown outcome reported as failure costs a duplicate, which at-least-once delivery and `event_id` deduplication already absorb. An unknown outcome reported as success costs the **event**, because the relay marks the outbox row published and nothing ever retries it. Losing a financial event silently is unrecoverable; a duplicate is routine. This is the same principle as the timeout rule for submissions: never convert "I do not know" into "it succeeded".
- **The test double was tightened to match.** `Loopback` now enforces the identical partition-key rule as the real client, because a double that accepts what the real bus rejects blesses exactly the producer bug it exists to catch.
- **Two tests were found to be passing vacuously and were fixed:** the ordering test ran on a single partition, where every ordering holds trivially, and now uses eight keys and asserts they demonstrably spread across more than one partition; the rebalance test's fixed batch was drained by the incumbent consumer before the joining member was ever assigned anything, and now feeds the topic continuously and proves the joining member was assigned work.

## D-035 — The settlement executor retries deadlocks, and why that is safe

- `runner.inTx` used `db.TxOptions{}`, so `MaxRetries` was 0 and concurrent resumption of one plan surfaced a raw SQLSTATE 40P01 instead of retrying. Set to 3.
- **The reason this needed proving rather than assuming:** `InTx` re-runs the whole closure, so retrying a transaction that performs an external effect would be a double-submission bug — the worst failure this system can have. Before changing it, every `Adapter.Quote/ValidateQuote/Build/Submit/Status/Reconcile` and `signer.Sign` call site in `executor_steps.go` was located by parsing the file and walking back to its enclosing closure: **all seven run inside a step's `effect` closure, which takes only a context and executes outside any transaction.** Nothing passed to `inTx` performs an external effect, so a retry repeats only database work that the rollback already undid.
- A comment at the retry constant states that if a future step ever performs an external effect inside `inTx`, this retry becomes a double-submission bug and must be removed with it. Re-verified afterwards: `ok internal/settlement 84.548s`, `ok internal/execution 56.440s` on the integration tier.

## D-033 — An expected conflict is scoped by a SAVEPOINT, and idempotent content never contains the wall clock

Two defects in `scripts/seed`, both of which hid behind a misleading error.

- **A tolerated conflict poisoned the whole transaction.** Three sites created a row, treated `CONFLICT` as acceptable, and carried on in the same transaction. PostgreSQL aborts the *entire* transaction on any failed statement, so every later statement failed with SQLSTATE 25P02, `current transaction is aborted`, and the reported error named an innocent later query while the real conflict was invisible. Re-seeding a database that already held data failed with exactly that. Fixed with a `tolerateConflict` helper that runs the statement inside a SAVEPOINT (`tx.Begin` on a pgx transaction), so an expected conflict is scoped and the enclosing transaction survives. **The rule: only wrap statements whose conflict is genuinely expected, and never swallow a conflict the caller does not then handle** — a swallowed conflict that nobody handles is a silent no-op.
- **An idempotent posting contained the wall clock.** The SEED ledger posting used a fixed idempotency key but passed `now` as `EffectiveAt`. `internal/ledger` includes `effective_at` in the posting content hash at RFC3339Nano precision, so every run produced different content under the same key, which the ledger correctly rejected as `INVALID_IDEMPOTENCY_REUSE`. **The ledger was right and the caller was wrong.** Fixed by pinning a `seedEpoch` constant, which is also simply more correct: the same seed should describe the same financial fact. Verified by running the seed three times in a row, the second and third reporting `SEED posting already present (idempotent)`.
- Also added: the seed now prints an error's structured fields on failure. Diagnosing this took far longer than it should have because the code and message were shown while the fields naming the conflicting key and existing transaction id were not.
- **A sharp edge worth knowing:** `scripts/seed` reads `CP_DATABASE_APP_URL`, not `CP_DATABASE_URL`, and falls back to the default local DSN when it is unset. Setting the wrong variable seeds a different database silently. The blast radius is bounded — it refuses any non-local host and any environment other than LOCAL/DEV/TEST — but the silent fallback cost real time during this investigation.

## D-031 — franz-go's kmsg module pinned so the Redpanda bus can be built

- `github.com/twmb/franz-go v1.21.6` was pinned but `github.com/twmb/franz-go/pkg/kmsg` is a **separate module** and was absent from `go.mod` and `go.sum`, so `internal/reality/redpandabus` could only ship a `Loopback` while its own doc comment described a franz-go producer/consumer. Pinned `kmsg v1.13.1`; franz-go itself stays at v1.21.6. Module list unchanged at 376 with nothing added or removed; a throwaway probe importing `kgo` and `kmsg` compiled and was deleted; `go build ./...` clean and `go mod verify` all verified.
- **The agent behaved correctly and disclosed a side effect:** a diagnostic `go list` run with `GOFLAGS=-mod=mod` silently added two `go.sum` lines; it removed them and reported that it had. That disclosure is the standard — a dependency graph mutated as a side effect of a read-only-looking command is exactly the kind of change that is invisible later.
- Until the real client exists, the market-ingest worker **refuses to start** outside fake mode with an error naming the missing implementation, rather than silently downgrading to loopback. Failing loudly beats a bus that appears to work and delivers nothing.

## D-030 — Temporal SDK transitive requirements pinned by the integrator, never by an agent

- **Chosen:** `go.temporal.io/sdk v1.48.0` was pinned without any of its transitive requirements, so every import of it failed with `missing go.sum entry`. The integrator added the ten missing modules by running `go get` for the six SDK packages actually used (workflow, client, worker, activity, temporal, testsuite): `go.temporal.io/api v1.63.4`, `github.com/gogo/protobuf v1.3.2`, `github.com/facebookgo/clock`, `github.com/golang/mock v1.6.0`, `github.com/grpc-ecosystem/go-grpc-middleware/v2 v2.3.2`, `github.com/nexus-rpc/{sdk-go v0.7.0,nexus-proto-annotations v0.1.0}`, `github.com/robfig/cron v1.2.0`, `github.com/stretchr/objx v0.5.3`, `gopkg.in/yaml.v3`.
- **Why the rule holds:** agents are forbidden from running `go get` or `go mod tidy` because a resolution run in one agent's context can silently drop a `go.sum` entry another package needs — it has happened once in this repo already, with zap via solana-go. The workers agent hit this, resolved the exact minimal set in a throwaway module in its scratchpad, and reported rather than editing `go.mod`. That is the intended behaviour and it worked.
- **Verification, which is the part that makes this safe:** the full module list was captured before and after — 370 to 376, six added, **nothing removed**. A throwaway probe importing all six SDK packages compiled and was deleted, then `go build ./...` was confirmed clean repo-wide and `go mod verify` reported all modules verified. Always diff the module list across a `go get`; a bare success message does not tell you what was dropped.

## D-028 — A test deadline is generous unless the test is about the deadline

- **Chosen:** in tests, a client/call timeout is either (a) large enough that no successful call can plausibly exceed it under a contended `-race` run — 2–10s — or (b) short *and* pointed at a fixture that never answers, in which case the fixture stalls for far longer than the deadline (or blocks on the request context) so the deadline is guaranteed to be the thing that fires. Never a short deadline shared with a call that is expected to succeed. Assertions are on classification and call count, never on elapsed wall-clock time.
- **Why:** four packages independently grew the same defect — a deadline chosen for the timeout case leaking onto the success case in the same test or harness. `test/contract/solanarpc` failed for real in the 91-package race run: a 40ms client deadline set for a delayed first call also governed the immediate second call, which needed more than 40ms just to cross loopback under contention, so `require.NoError` failed with `request timed out after 40ms`. The others were latent: `test/contract/stripe` gave every test a 150ms deadline over real loopback HTTP, and `internal/chain` gave every success path a 50ms budget so that `FaultTimeout` cases would resolve quickly.
- **Applied:** `test/contract/solanarpc` (30s stall vs 2s deadline), `test/contract/stripe` (10s default, `hangTimeout` override only on the hanging test), `internal/chain` (50ms → 500ms), matching the pattern `internal/provider/jupiter` and `test/contract/jupiter` already use.
- **Not changed, verified safe:** `internal/provider/solanarpc` (30ms, but the server hangs on every call, so no success path depends on it) and `internal/db` (300ms *server-side* statement timeout against `pg_sleep(2)`, a 6.7x gap).
- **Why it matters beyond tidiness:** a suite that fails under load and passes when idle cannot support a production-readiness claim, and it trains everyone to re-run red builds instead of reading them.

## D-027 — Container healthchecks address 127.0.0.1, never `localhost`

- **Chosen:** every compose healthcheck targets `127.0.0.1` explicitly. The ClickHouse check used `http://localhost:8123/ping`; inside that image `localhost` resolves to `[::1]` first, ClickHouse binds `0.0.0.0` (IPv4) only, so the check reported `Connection refused` and the container sat `unhealthy` for 3,471 consecutive checks while the server was serving normally from both the host and inside the container.
- **Why:** a healthcheck that is wrong in the pessimistic direction is not harmless. `depends_on: condition: service_healthy` would have deadlocked startup on it, and a permanently-red service trains operators to ignore red — the exact failure mode a control plane cannot afford. It also would have masked a real ClickHouse outage completely.
- **Applies to:** `docker-compose.yml`, and any healthcheck added to Terraform task definitions or Kubernetes probes later. Verified: after the change the container reports `healthy`.

## D-013 — Scripts are Go programs, not shell

- **Chosen:** operational and dev scripts live under `scripts/` as Go `main` packages invoked via `go run`, so `make` targets behave identically on Windows, macOS, Linux, and CI.
- **Why:** the primary dev host is Windows; shell scripts fork behaviour across platforms.

## D-029 — Audit checkpoint signatures are verified with the key the row names, out of a trusted set

- **Problem:** `proof.Verifier` held one signer and verified every checkpoint with it. `audit_checkpoints` rows are append-only and outlive any key, so as soon as the verifying key differed from the signing key, verification reported `checkpoint_signature` — indistinguishable from a forged signature. Found by the integrator: the Stage 13 integration suite passed once per database and failed on every later run against the same database, because each test process minted a fresh ephemeral key while the checkpoints it had written stayed. The same defect in production is the key-rotation failure mode: the first scheduled KMS rotation would make `make verify-audit` claim tampering across all pre-rotation history. An audit system that cries tamper after routine maintenance trains its operators to ignore it, which costs exactly the one alert that matters.
- **Chosen (2026-09-06):** trust is a set of keys addressed by id, not a single key. `proof.KeySet` maps a key id to a status and a verifier; `Verifier` resolves the verifying key by `audit_checkpoints.signing_key_id` and fails closed when the id is not in the set. Statuses: `active` (signs new checkpoints and verifies old ones), `retired` (rotated out; signs nothing further and still verifies everything it signed — retiring says nothing about signatures already made), `revoked` (withdrawn; refused before any cryptography, so no key material is needed to revoke and revocation overrides active/retired).
- **Three outcomes, three reports, never merged:** `checkpoint_key_unknown` (id not trusted — the signature was never judged; this is a trust-configuration gap, typically a rotation whose key id was never added), `checkpoint_key_revoked` (deliberate withdrawal), `checkpoint_signature` (trusted key, signature does not verify — the only one that is evidence of tampering). Reports and `audit_verification_runs.first_failure` carry `signing_key_id` so the operator is told which key to add or investigate.
- **Rotation procedure:** add the incoming key, start signing with it, keep the outgoing key id trusted as `retired` forever (or as long as its checkpoints are retained — a checkpoint whose key is dropped becomes unverifiable, which the verifier reports as unknown rather than as tampering). Never widen the signature check to make history verify again; add the key id. Worker variables: `CP_AUDIT_RETIRED_SIGNING_KEY_IDS` (KMS ARNs), `CP_AUDIT_LOCAL_RETIRED_KEY_REFS` (SecretRefs to retired PEM keys, LOCAL/TEST/DEV), `CP_AUDIT_REVOKED_SIGNING_KEY_IDS`.
- **Trust the ARN, not the alias:** KMS `Sign` reports the key ARN and that is what the row records; an alias can be re-pointed at another key, so trusting an alias is not a decision about a key. `audit-worker` warns when the configured signing key id is an alias, and logs an error naming the untrusted id as soon as it writes a checkpoint whose key is not in the trusted set — so the misconfiguration surfaces at signing time instead of looking like tampering at the next verification.
- **Why a set is also the safer check:** resolving by id means a forged row cannot nominate its own verifying key. The previous single-key design happened to have this property by accident; the set makes it explicit, and `KMSSigner.Verify` (which passes the row's id straight to KMS) is only ever reached for ids an operator listed.
- **Test consequence:** signing keys and archived objects now persist per test database under the OS temp directory (`cp-proof-keys/<db>`, `cp-proof-archive/<db>`), because the evidence they belong to persists. The suite trusts every key in that directory, `TestIntegration_KeyRotation` rotates and asserts that both keys verify, that dropping the retired key reports `checkpoint_key_unknown` (not a signature failure), that revoking reports `checkpoint_key_revoked`, and that a forged signature under a trusted key is still `checkpoint_signature`. Repeatability was NOT bought by weakening the signature check or by deleting audit rows: running the suite twice against one database passes both times.
