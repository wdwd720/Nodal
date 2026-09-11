# DECISION REGISTER

Every meaningful deviation from, or concretisation of, the goal architecture. Never silently change architecture.

Format per entry: decision · original recommendation · chosen implementation · why · evidence · consequences · migration impact.

Last updated: 2026-09-09

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
- **Test change (PART 235):** `TestIntegration_RelayFailedPublishBacksOffAndRetries` (`internal/event/relay_integration_test.go`) encoded the old derived timing (second retry at `recorded_at + 2s`); it now asserts failure-based timing (`recorded_at + 3s`). The old expectation was the defect being fixed.

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

## D-043 — The five-condition activation rule lives in the database, not only in `internal/gates`

- **Problem (2026-09-06):** `gates.Evaluate` decides whether a live-money capability is active by re-deriving all five conditions of POLICY_AUTHORITY §1 from the single `capability_gates` row it is handed — `state`, `proposed_by_user_id`, `approvers`, the evidence references, `effective_at`, `expires_at`, `revoked_at`. Migration `00150` granted `cp_app` table-wide `UPDATE` on that table **and** `INSERT` on `capability_gate_transitions`. So one transaction — a self-signed transition row (which satisfied `00603`'s deferred `AU001` binding) plus `UPDATE capability_gates SET state='ACTIVE', approvers='[…two names…]', proposed_by_user_id=…, effective_at=now()` — turned on `LIVE_FUNDING` or `WITHDRAWALS`, and every check passed because all of them read what that transaction had just written. On a deployment with no gate rows (`gates.Bootstrap` has no caller) a single `INSERT` did the same, since nothing constrained the state a row was born in. Not remotely reachable — `internal/httpapi` exposes no endpoint that writes those columns freely — but in a system whose premise is that no single actor can enable live money, "holds against an operator, not against the application's credential" is the wrong side of the line. `docs/compliance-gates/PRODUCTION_GATES.md` §1 claimed the stronger property.
- **Chosen:** the same principle as D-016 (no privilege the application does not need), applied at column granularity as in D-016's descendant `00604`. Migration `00701_capability_gate_state_authority.sql`:
  - revokes `UPDATE` on `capability_gates` from `cp_app` and grants back only `UPDATE (version)`, because PostgreSQL requires the `UPDATE` privilege for `SELECT … FOR UPDATE` and `version` is the optimistic-concurrency counter, which decides nothing;
  - revokes `INSERT` on `capability_gate_transitions` from `cp_app`, so gate history has exactly one writer;
  - adds a `BEFORE INSERT` trigger so a gate row can only be born `DISABLED` with an empty approval chain — PART 244 as a stored constraint rather than a startup routine;
  - adds `cp_gate_transition(…)`, `SECURITY DEFINER` and owned by `cp_migrate` (which `cp_app` is not a member of), as the only writer of `state`. It takes an *operation*, not a desired row, so columns the operation does not own keep their stored value; it appends each approval-chain entry to the **stored** chain after checking the entry names the acting principal; and it re-derives the legal-transition table, both dual-control rules and conditions 2–5 before writing the row and its transition record together.
- **Why the grant is a revoke and not a column list:** every remaining mutable column feeds one of the five conditions, and every write to the table in `internal/gates` is part of a state transition. Granting `UPDATE` on the approval chain while withholding it on `state` would have left a one-transaction forgery — rewrite the chain, then ask the function to activate against it.
- **The Go checks are unchanged.** They run first and produce the operator-facing error codes; the database is the line that holds when they are bypassed. No check was deleted because the database now also makes it.
- **What this buys, stated honestly:** reaching `ACTIVE` now requires three calls naming three distinct principals, each leaving an immutable transition row, with evidence and window checked server-side. The database cannot authenticate an operator, so a holder of the application credential can still name principals of its choosing; what it can no longer do is hold an `ACTIVE` gate whose approval history is absent, incomplete, self-approved or inconsistent with the row. `cp_migrate` and any superuser remain unconstrained by design.
- **Evidence:** `internal/gates` `TestIntegration_DatabaseRefusesForgedActivation` issues each forgery as `cp_app`; against the pre-migration schema it fails with `self-signed transition + UPDATE: the forged row evaluates ACTIVE ({Active:true …})` and leaves `CEX_TRADING/LOCAL` in state `ACTIVE` with `proposed_by_user_id = mallory`; after the migration every case is refused (`42501`, `GT005`, `GT002`, `GT004`, `GT001`) and the row never leaves `DISABLED`. `test/integration/migrations` `TestIntegration_CapabilityGateStateAuthority` pins the privileges, the function's owner, its `SECURITY DEFINER` flag and the absence of `PUBLIC` `EXECUTE`, so a later migration cannot quietly restore them.
- **Consequences:** `gates.saveGate` and `gates.insertTransition` are replaced by `gates.applyTransition`, which calls the function and re-scans the row it wrote, so the in-memory `Gate` is always exactly what the database holds. `Admin`'s public API, its error codes and its checks are unchanged.
- **Migration impact:** none to data. Production role bootstrap (Terraform) must not grant `cp_app` table-wide `UPDATE` on `capability_gates` or `INSERT` on `capability_gate_transitions`; the migration revokes both, but a bootstrap that re-grants them after migrating would undo this.
- **Open, deliberately not changed here:** `gates.Bootstrap` still has no caller (a fresh deployment's gates are absent rather than persisted `DISABLED` — safe, because both `Evaluate` and the new trigger fail closed, but not what the design says), and `MARKETPLACE` is documented as high-risk while `gates.IsHighRisk` excludes it. Both are recorded in `PRODUCTION_GATES.md` §9.

## D-044 — A transition row licenses only the change it describes

- **Original recommendation:** PART 89 audit completeness — every state change of an audited entity is bound to a transition row written in the same transaction.
- **Problem (2026-09-08, F-78):** `00603` implements that binding by comparing ONE thing: `cp_flag_transition` records the transition row's destination (`to_state`), and `cp_require_transition` raises `AU001` unless it equals the NEW value of the bound column. Nothing looks at `from_state`. `00690` applied it to `agents.state` and to no other column of that table. Both promotion CHECKs on `agent_lifecycle_transitions` open with `from_stage = to_stage OR ...`, and they must — a pause or a resume keeps the stage and cannot be made to carry promotion evidence. Those two facts compose into a promotion with no approval and no evidence, available to the application role in one transaction: insert a row claiming the stage did not move (`from_stage = to_stage = 'LIVE'`), then `UPDATE agents SET state='LIVE', stage='LIVE', mode='LIVE'`. Observed committing against the pre-migration schema, from the application pool, on an agent walked to CANARY through the real lifecycle. A second route needed no lie at all: `agents_check` permits any stage while the state is a side state, so a PAUSED agent's stage could be moved with no transition row, and `Resume` — which needs only `agent:pause` — then set the state to whatever stage it found.
- **Chosen:** bind the EDGE rather than the destination. `00726` adds `cp_flag_transition_edge`, which records `<from>><to>` under a per-column label, and `cp_require_transition_edge`, which compares that flag against `OLD.col || '>' || NEW.col`. A row licenses a change only if it says where the change started. `agents.stage` gets a binding of its own under a separate label; the destination-only trigger on `agents.state` is REPLACED rather than kept beside the new one, because two triggers raising one SQLSTATE for one write make every failure ambiguous about which rule fired.
- **Why not `mode` as well:** `agent_lifecycle_transitions` has no `to_mode` column to bind it to, and `agents_check3` pins mode to stage for every rung except BACKTEST_ELIGIBLE, whose two modes both move simulated money only.
- **Scope, stated rather than implied:** this covers `agents`. The other ten bindings `00603` established (`capability_gates`, `kill_switches`, `accounts`, `assets`, `instruments`, `deposits`, `withdrawals`, `trade_intents`, `orders`, `reconciliation_records`) still compare the destination alone. Whether the same composition is reachable there depends on each table's own CHECKs, and answering that is a migration per table with its own exploit test. Recorded in `MASTER_BUILD_STATE.md` §4 as the next work rather than done on the strength of the analogy — the analogy is what would make it a claim instead of a proof.
- **Evidence:** `internal/agent` `TestIntegration_APromotionCannotBeLicensedByARowThatDeniesIt` and `TestIntegration_ABareStageUpdateIsRefused`, both observed failing ("An error is expected but got nil — the application role promoted an agent to LIVE with no approval and no evidence") before the migration and refused with `AU001` after. `TestIntegration_AStageChangeWithAnHonestRowIsAccepted` and the suite's existing full ladder walk are the controls: a binding one condition too strict would refuse every real promotion.
- **Consequences:** none to the Go layer. `Lifecycle` already writes `from_state`/`from_stage` from the loaded agent, so every legitimate flow satisfies the stricter rule unchanged.
- **Migration impact:** none to data. A deployment that writes `agents` outside `internal/agent` must now write a transition row naming both endpoints.

## D-045 — Retention passes live in `audit-worker`, under a `cp_ops` DSN, not in a ninth binary

- **Original recommendation:** ADR-0003 lists eight deployables (`api`, `execution-worker`, `reconciliation-worker`, `market-ingest-worker`, `agent-worker`, `workflow-worker`, `audit-worker`, `migrate`) and puts "any additional binaries" out of scope.
- **Problem (2026-09-08, F-79):** `login_attempts` holds the OIDC state, nonce and PKCE `code_verifier` in plaintext plus the caller's IP and user agent. `00641` grants `DELETE` to `cp_ops` and states the table "is transient and purged by the ops role instead". Nothing purged it — no worker, no script, no scheduled task.
- **Chosen:** `audit-worker purge` (one pass) and an hourly pass inside `audit-worker run`. Of the eight deployables this is the one that already owns what the platform keeps and for how long: it reads `CP_RETENTION_SECURITY_AUDIT_DAYS` to set the archive's Object Lock window. A ninth binary would be an ADR-0003 amendment, which is a decision about deployment topology rather than a patch.
- **The credential stays where it belongs.** `cp_app` holds SELECT/INSERT/UPDATE on `login_attempts` and deliberately not DELETE, so an attacker holding the application credential cannot erase the record of the logins they attempted. The purge therefore needs `cp_ops`. `CP_DATABASE_OPS_URL` is a new **optional** variable rather than a production requirement for every binary: requiring it in STAGING/PROD would hand `cmd/api` an operations credential it never uses, against PART 100's rule that a binary loads only the secrets its role permits. `audit-worker` opens that pool before its first tick and refuses to start without it, because a retention pass that quietly does nothing is the defect this closes.
- **Retention:** `CP_RETENTION_LOGIN_ATTEMPT_DAYS`, default 2, with a 24-hour floor enforced in Go — a configuration value is one edit away from meaning "all of them", and an operator investigating a login-flow anomaly is usually doing it the next morning. The durable record of a login is a `security_events` row of kind `login`, which the purge never touches. The interval is fixed rather than configurable: retention is measured in days and the pass is a single DELETE on an indexed column, so there is nothing to tune and one more knob is one more thing that can be set to a value meaning "never".
- **Evidence:** `internal/identity` `TestIntegration_ExpiredLoginAttemptsArePurged` (asserts the application role's own DELETE is refused with `42501` first, then that only rows past retention go) and `TestIntegration_ThePurgeRefusesTooShortARetention`. Observed failing with the DELETE made a no-op.
- **Consequences:** `audit-worker` holds two database roles. A deployment that runs it must provide `CP_DATABASE_OPS_URL`; one that does not will fail at startup rather than silently skip.
- **Open, deliberately:** three of the six declared retention classes (`SOCIAL_DATA`, `MODEL_IO`, `OPERATIONAL_LOG`) still have no enforcement. The tables they would cover carry `forbid_mutation` triggers that refuse DELETE outright, so retention there is partition management or archival-then-drop — a design decision about how an append-only financial record is aged out, and it belongs in an ADR before it belongs in a purge command.

## D-046 — The launch tier's settlement sweep runs in the API process

- **Original recommendation:** ADR-0003's eight binaries; the reconciliation worker owns `SettleDue`.
- **Problem (2026-09-09, F-90):** Render charges for a worker service, so `render.yaml` deploys one web service and records the sweep as "operator-run for now". `internal/capacity` excludes SETTLED from money at risk **because** something settles — its own comment says counting SETTLED "would turn the ceiling into a lifetime cumulative cap that can only ever rise, so the tier would end up refusing every purchase forever — an outage, not a ceiling." With no sweep, that is exactly what it became: the only exits from REVERSIBLE are `SettleDue` and a won dispute, so the sum could only rise and the deployment would have refused every Credit purchase with `AT_CAPACITY`, permanently, at $2,000 of lifetime sales.
- **Chosen:** `cmd/api` runs the sweep on a 15-minute ticker bound to the process lifetime, once at startup and then on the tick.
- **Why this is acceptable in the API process, stated rather than assumed:** the sweep is idempotent and takes its rows `FOR UPDATE SKIP LOCKED`, so a worker tier added later runs alongside it with no coordination and no double effect. It holds no provider call and no lock across a network hop — one bounded UPDATE per pass. A free instance spins down when idle, so it does not run continuously; that is tolerable because it only has to run while purchases are happening, and purchases require the process to be up.
- **What it deliberately does not become:** the only reason a number is correct. The ceiling still refuses when it cannot measure, and the settlement window is a recorded risk decision rather than an artefact of how often the ticker fires.
- **Consequences:** `CP_CREDIT_SETTLEMENT_WINDOW` moves out of the worker's environment reads and into the configuration table, so it is in `scripts/configcheck` and in the configuration hash. The worker reads it from configuration too; two readers of one risk decision is what the move removes.
- **Evidence:** `TestIntegration_SettlementDrainsTheMoneyAtRiskCeiling` measures the ceiling before a purchase, after it, after a pass that finds nothing because the window is open, and after one that settles.

## D-047 — A production-like deployment declares the networks its load balancer speaks from

- **Problem (2026-09-09, F-88):** the rate limiter keyed on `r.RemoteAddr`. Every deployment of this service terminates TLS at a balancer, so that address is the balancer's and identical for every caller: `CP_API_RATE_LIMIT_AUTH: 30/1m` was one bucket for the whole deployment. Any unauthenticated client issuing 31 requests a minute to `/v1/auth/login` returned 429 to every user's login, and an attacker's attempts were counted against the crowd. `clientIP` was already proxy-aware and was used only for audit records — two notions of "who is calling", disagreeing.
- **Chosen:** one notion. The limiter keys on `clientIP`, and `config.Validate` gains `TRUSTED_PROXY_DECLARED`: `CP_HTTP_TRUSTED_PROXY_CIDRS` must be non-empty in STAGING and PROD.
- **Why required rather than defaulted:** the answer depends on the platform, and a default would be a guess that silently trusts the wrong thing. Requiring it makes the operator state a fact they know and the code cannot discover.
- **Why trusting private ranges is safe on Render, since the blueprint now does:** nothing routes to that container except through the platform's router, so an `X-Forwarded-For` arriving from a private peer is the router's record of the caller. A caller that somehow arrived from a public address is not in those ranges, so its header is ignored and it is keyed on where it really came from.
- **Consequences:** the configuration hash changes, so the deployed hash in `PROVIDER_ACTIVATION_CHECKPOINT.md` is superseded. Audit records and `login_attempts` rows now carry the caller's address rather than the balancer's.
- **Evidence:** `TestRateLimitKeyDistinguishesCallersBehindAProxy`; the validation rule was observed failing against the real blueprint before `render.yaml` was changed.

## D-048 — `state` is bound to the browser, not only to a row

- **Problem (2026-09-09, F-87):** `internal/auth/identity.go` said "state binds the callback to the browser session" and nothing implemented it. The state was persisted server-side and consumed once, which stops a REPLAYED callback and does nothing about a PLANTED one. An attacker who begins a flow, authenticates as themselves and induces a victim's browser to load the callback signs the victim in as the attacker — so a Credit purchase on the victim's card, their identity documents and their payout destination all land in the attacker's account, attributed to the attacker.
- **Chosen:** a short-lived `__Host-` cookie carrying `base64url(sha256(state))`, required to match before `Complete` runs.
- **Why the digest and not the value:** a cookie is readable by anything that can read the browser's storage for the host, and the raw state is the key to a pending attempt row. The digest is enough to compare and useless to present.
- **Why `SameSite=Lax`:** the callback is a top-level cross-site GET from the identity provider. Strict would not send the cookie at all, breaking every login rather than only the planted ones — the same reason the session cookie is Lax.
- **Why the check runs before `Complete`:** a planted callback then consumes nothing, so the refusal costs the real browser nothing and the attacker's own flow expires on its own.
- **Consequences:** any client driving the flow must carry cookies between the two requests. `test/security`'s login helper did not, which is to say the security suite had been logging in the way the attack does.

## D-049 — The provider call is outside the transaction, and the ceiling takes a lock

- **Problem (2026-09-09, F-96):** `StartPurchase`'s doc said the funding row was "persisted BEFORE the provider is called". It ran inside one transaction the caller opened, with the Stripe call between the INSERT and the COMMIT, so the row was written before the call and not persisted before it. A rollback erased the row and left the Stripe object; the retry made a second row that the first object's metadata still named, and every webhook delivery then failed with a 500 until Stripe gave up — money captured, Credits never minted. The same transaction held one of eight pool connections across a 20-second network call, which is the starvation `cmd/reconciliation-worker` documents itself avoiding for exactly this reason. And the capacity ceiling read inside that transaction under READ COMMITTED, so N concurrent purchases measured the same headroom and all N were admitted.
- **Chosen:** three transactions. Commit the gate, the ceiling, the price and the funding row; call the provider with nothing held; commit the provider reference and the state it reported. The capacity guard then takes `pg_advisory_xact_lock` before measuring, one key per action.
- **Why the two had to move together:** raising isolation while the provider call is inside the transaction makes a serialization retry re-call the provider; taking a lock instead holds a pool connection across that call and queues every other purchase behind one HTTP request. Neither is safe until the call is outside, which is why F-93 recorded them as one item.
- **Why an advisory lock rather than SERIALIZABLE:** the transaction also writes a funding row, and SSI would abort it on a read-write conflict that is not an error — the ceiling is a queue, not a contention failure. A transaction-scoped advisory lock makes the measure-then-write window atomic without turning a legitimate wait into a retry, and it cannot be leaked because the transaction's end releases it.
- **Consequences:** `StartPurchase` takes a `Transactor`, not a `pgx.Tx`. A caller that wrapped it in a transaction would put the provider call back inside one, so the adapter deliberately does not. A crash between any two phases leaves a funding row with no provider reference, which is the state the replay path already handled.
- **Evidence:** `TestIntegration_ConcurrentPurchasesCannotAllPassOneCeiling` requires exactly three of eight simultaneous purchases to be admitted at a ceiling of three, and the database's own sum to agree. Observed failing with the lock disabled: "the ceiling admitted 8 purchases of 10000 against a ceiling of 30000".

## D-050 — Personal data: encrypted in the application, and unreadable by the roles that have no use for it (2026-09-10, F-47)

- **Problem:** `identity_pii`'s encrypted columns had no encryption and no writer for a year; two deliberate statements about whether `cp_readonly` and `cp_ops` may read the table contradicted each other and the role bootstrap's blanket default won silently. F-47 refused to decide, because whether reading the columns was an exposure depended on whether they held ciphertext.
- **Chosen:** `internal/pii` — AES-256-GCM, a versioned keyring in one SecretRef, additional authenticated data binding each ciphertext to row, column and key version — as the only writer of the table; the login path stores the verified e-mail sealed; migration 00754 revokes both tables from both roles and grants `cp_ops` `SELECT (expires_at)` on `sessions`.
- **Why derived rather than chosen:** once the columns are ciphertext under a key the database never holds, a grant to a role with no use for the table is an exposure deferred until a key leaks, and neither role has a use — `cp_ops`'s one use of `sessions` is a DELETE that filters by one column, which the column grant satisfies without trading the constraint away.
- **Why no KMS:** a fixed monthly cost the launch tier may not carry; the paid tier wraps the same keyring in an envelope and the row format does not change.
- **Consequences:** `NODAL_PII_KEYRING` is a dashboard secret and STAGING/PROD refuse to start without it; rotation is add-raise-deploy-reseal-remove; a ring missing a version a row names errors on read. Checking what `cp_ops` did with `sessions` found that nothing purged them (F-133).
- **Evidence:** `internal/pii` unit and integration tests; `TestIntegration_Login_StoresTheVerifiedEmailEncrypted`; `TestIntegration_ExpiredSessionsArePurgedAsOps`; `TestPII_OnlyTheEncryptingStoreWritesPersonalData`; `TestIntegration_ApplicationRolePrivileges`. ADR-0021.

## D-051 — One identity source of truth for the product surfaces (2026-09-10, product goal §3)

- **Problem:** the productization goal raised Supabase as a possible home for user accounts. A second authentication system would create two subjects per person, an account-linking problem Nodal has deliberately not invented (F-64/F-93), and ambiguous authority over who a session belongs to.
- **Chosen:** ZITADEL remains the only authenticator; Neon owns the Nodal user (`users` keyed by issuer+subject, `identity_pii` sealed, `sessions` as Nodal's own hashed tokens) and gains a separate `user_profiles` table for product-level state. No Supabase.
- **Why derived rather than chosen:** the code's three eliminating requirements — a strong RFC 8176 `amr`, `auth_time`, `email_verified` — are what step-up and therefore capability activation rest on; ZITADEL meets them at $0 and is verified live, and Supabase Auth is not an OIDC provider the relying-party flow can point at without rewriting the control. Supabase offers no non-auth capability Nodal genuinely needs: SSE exists, evidence goes to the archive, and user documents are provider-hosted by design.
- **Consequences:** signup is a ZITADEL identity plus a Nodal profile; the browser never holds a provider token; a future provider change is a superseding ADR and an issuer migration, never a parallel directory.
- **Evidence:** ADR-0022; `docs/operations/IDENTITY_PROVIDER.md`; `docs/operations/LAUNCH_TIER.md` §4; the live login redirect in the final checkpoint §6.

## D-052 — The sandbox tier: how a non-production deployment exercises gated surfaces (2026-09-10, product goal §9, §43)

- **Problem:** STAGING could not reach a single Domain-A write. The conservative legal policy denies the internal economy and every payout; a gate reaches ACTIVE only through three principals, four evidence references and step-ups; the payout policy is fail-closed; the payout registry holds no provider outside PROD; the verification resolver stops at `NODAL_IDENTITY`. The goal requires the full UI/API path to be exercised in STAGING without bypassing the capability gate and without a fabricated approval reference (§9, §59).
- **Chosen:** ADR-0023. One declaration, `CP_API_LEGAL_POLICY=SANDBOX`, refused by `config.Validate` in PROD, is the only condition for every sandbox affordance: a `SANDBOX` gate state that is never on the path to ACTIVE (migration 00755: a CHECK forbids a PROD row, a SECURITY DEFINER function refuses to write one, and `EvaluateWith` reads it as active only for a checker built for a sandbox tier, with `Sandbox: true` on the verdict and in the API); boot-time activation of the blueprint's `CP_API_SANDBOX_GATES` as the SYSTEM actor `config:CP_API_SANDBOX_GATES`, idempotent, erroring rather than moving a gate that is part of a real ceremony; `legalrouter.SandboxPolicy` (payout permitted at `PAYOUT_KYC`+ under `PAYOUT_RESERVE`, `REQUIRES_VERIFICATION` below, every reference reading `NOT-AN-APPROVAL-SANDBOX-TIER-ONLY`); `valuedomain.SandboxPolicy` behind `CP_API_PAYOUT_POLICY=SANDBOX` (granted, refunded, adjusted and provider-settled value never withdrawable); and `internal/provider/payoutsandbox`, which accepts, settles ten seconds later, moves nothing and refuses to be built in PROD.
- **Why derived rather than chosen:** the alternatives were fabricating evidence references (forbidden, and it would teach the register that a gate can be green without the ceremony) or a flag that skips the router or the gates (then STAGING rehearses a code path PROD never runs). The sandbox tier runs the real router, compiler, gates, engine, ledger and value-domain isolation; the only differences are the policy loaded and a state whose verdict says in words that it is not an approval. PROD's path to ACTIVE, the five conditions, `cp_gate_transition`, the conservative policy and the fail-closed payout default are untouched.
- **Consequences:** STAGING enables and sandbox-activates `CREDIT_PURCHASE, NATIVE_ASSET_CREATION, NATIVE_MARKET_TRADING, MARKETPLACE, PAYOUT_RESERVE, PAYOUT_SETTLE` and names `sandbox_payout` in the payout slot; a test-card purchase mints sandbox Credits, native markets trade, and a verified sandbox identity can request a payout a provider settles without moving value. `LAUNCH_GATE_MATRIX` does not move: `LIVE_READY` still needs three principals, four references and a licensed provider, because PROD does. `test/infra` holds the blueprint to this (the sandbox provider may be named only on a sandbox tier).
- **Evidence:** ADR-0023; migration 00755; `internal/gates/sandbox*.go` and their unit and integration tests; `internal/legalrouter/sandbox_test.go`; `internal/valuedomain/sandboxpolicy_test.go`; `internal/config` (`RuleSandboxTierNotInProd`, `TestValidate_*Sandbox*`); `cmd/api/sandboxtier.go`; `test/integration/enums`; `render.yaml`.
## D-057 — `UNVERIFIED` keeps its name, and §20's ten states are the schema's (2026-09-10, product goal §20)

- **Problem:** §20 lists ten canonical verification states and the schema held five. The first of the ten is `NOT_STARTED`; the schema's first was `UNVERIFIED`. Adopting the goal's word would have been the tidier reading of the document.
- **Chosen:** the CHECK grows to ten values — UNVERIFIED, REQUIRED, STARTED, PENDING, NEEDS_INFORMATION, VERIFIED, REJECTED, EXPIRED, RESTRICTED, SUSPENDED — with `UNVERIFIED` playing `NOT_STARTED`'s part under the name the schema already used.
- **Why derived rather than chosen:** rows carry `UNVERIFIED`, `internal/eligibility`'s policy document allowlists identity states BY NAME (`DefaultPolicyJSON` names `VERIFIED`; an operator policy may name any of them), and `eligibility.Evaluate` reads an unrecognised state as `ELIGIBILITY_POLICY_UNKNOWN`. Renaming a value to match a document therefore breaks the documents somebody already wrote, and gains a word. The five states that are genuinely new — REQUIRED, STARTED, NEEDS_INFORMATION, RESTRICTED, SUSPENDED — are added, because each names a moment the journey actually has and none of them existed.
- **Consequences:** the list is now declared in two Go packages — `internal/compliance` owns the column and `internal/verification` owns the edges between its values — so `test/integration/enums` gained `TestVerificationStatesAgreeAcrossPackages` alongside the schema comparison. Comparing each against the schema separately would not catch the two drifting together, which is the failure that leaves a state the database accepts and no transition table licenses. `eligibility`'s recognised identity states gained the five new values so a profile in one of them is judged rather than reported as an unknown policy.
- **Evidence:** migration 00761; `compliance.AllIdentityStates`, `verification.AllStates`; `test/integration/enums`; `internal/verification` `TestStateMachine_EveryEdgeIsTheOneIntended`.

## D-058 — The age thresholds, and the direction a mistake in them falls (2026-09-10, product goal §21)

- **Problem:** §21 requires verification to gate on age and forbids faking it with a checkbox. A threshold has to come from somewhere, and "18" is an assumption until somebody writes down where it came from.
- **Chosen:** `internal/verification/rules` holds a default minimum of 18, per-country overrides (empty today) and per-subdivision overrides for the three United States states whose age of MAJORITY is higher: Alabama 19, Nebraska 19, Mississippi 21. A jurisdiction whose subdivision is unknown is compared against the HIGHEST floor in that country. A provider that attested no age at all yields `AGE_UNKNOWN`, never a pass.
- **Why derived rather than chosen:** the product rests on a contract, and a person below the age of majority in their own state cannot form one. Eighteen is the floor every identity provider in `PROVIDER_BOUNDARY.md` §5 attests against by default, so it is also the highest floor a provider can satisfy without a bespoke configuration — which is why `Capabilities.AttestsAgeAtLeast` exists and why a provider attesting 18 is refused for Mississippi BEFORE a session opens rather than after somebody has handed over identity documents.
- **What this is not:** a legal opinion. It is an engineering floor that fails in the conservative direction, and counsel confirms it. The table is constructed so that every entry can only RAISE a minimum — there is no code path by which a table entry lowers one — so a wrong entry refuses somebody who could have been verified rather than verifying somebody who could not.
- **Consequences:** the rule version (`verification-rules-v1-us-only`) is written onto every `verification_checks` row it judged, so a decision made under one version stays explicable after two more. Changing any threshold is a new version and a new decision record, never an edit.
- **Evidence:** `internal/verification/rules/rules_test.go` (`TestAge_ThresholdsOnlyEverRise`, `TestAge_UnknownIsNotAPass`); `TestCapabilities_SupportsOnlyWhatItCanActuallyDo`; ADR-0025.

## D-059 — The jurisdiction list is US-only, and a rail's exclusions are not a person's (2026-09-10, product goal §21)

- **Problem:** §21 requires gating on country and state. Two different things were being asked for under one heading: whether the PRODUCT is offered where a person is, and whether the RAIL will pay there. `PROVIDER_BOUNDARY.md` §3 records that Stripe's stablecoin payouts exclude New York and Hawaii and that Bridge excludes New York by principal address — facts about a payout rail, not about a person's eligibility to be verified.
- **Chosen:** the jurisdiction check holds a country ALLOWLIST of `US` alone, an explicit sanctions denylist, a per-country "a subdivision is required" rule, and an EMPTY restricted-regions map. The rail's own exclusions stay in `payout.Capabilities.ExcludedRegions` and are applied at the point of payout by `CanPayRecipient`.
- **Why derived rather than chosen:** the allowlist matches the sandbox payout provider's `SupportedCountries`, because offering verification somewhere no provider will pay is an onboarding flow that ends in a refusal after the person has handed over identity documents. The denylist is redundant against the allowlist today and is kept anyway: the allowlist is a business decision that will widen, the denylist is a sanctions fact that must survive that widening, and the failure mode it guards is somebody adding a country without re-deriving the exclusions. The restricted-regions map is empty because which United States states restrict a closed-loop credit that becomes convertible at withdrawal is a counsel determination (BLOCKERS B-02); inventing a list would be the fabricated legal conclusion the goal forbids, and an empty map plus a comment saying why is the honest form of "we do not know".
- **Consequences:** a jurisdiction refusal names WHICH rule refused — `COUNTRY_PROHIBITED` and `COUNTRY_NOT_OFFERED` are different codes because they have different remedies. Keeping the OFAC list current is an operational obligation named in BLOCKERS rather than a property of the file.
- **Evidence:** `internal/verification/rules` and its tests; `TestIntegration_AnUnofferedJurisdictionIsRefusedBeforeAnythingStarts`; ADR-0025.

## D-060 — A payout destination keeps its four status names, and never comes back (2026-09-10, product goal §25)

- **Problem:** §25 sketches VERIFYING / ACTIVE / DISABLED for a payout destination. `payout_destinations.status` already held UNVERIFIED / VERIFIED / REJECTED / DISABLED, with `DestinationStatus.Usable` reading VERIFIED and the eligibility engine keying on it.
- **Chosen:** the existing names stay, with the mapping recorded here — UNVERIFIED is §25's VERIFYING, VERIFIED is its ACTIVE — and the column gains F-42's treatment (migration 00763): a transitions table, an edge binding, a trigger that writes `status` and `verified_at`, no UPDATE for `cp_app` beyond `display_label`, and a birth control so a destination cannot be INSERTed already usable. REJECTED and DISABLED are terminal.
- **Why derived rather than chosen:** renaming four values to match a sketch would rewrite `Usable`, the eligibility engine's reading of it and a registered `test/integration/enums` pairing, to gain nothing a comment cannot say. What the sketch was actually asking for — that whether value may leave to a given account is a recorded decision rather than an UPDATE — is what the F-42 treatment delivers.
- **Why terminal:** a destination that could return from DISABLED would need a "when did it last change" field somebody remembers to reset. Re-adding one as a NEW row makes §25's cooldown on a changed destination a fact about `created_at` instead.
- **Consequences:** a person may disable their own destination (the one USER edge the transition writer permits) and may not decide that it may receive value. `test/reachability`'s exemption for `payout.CreateDestination` is dropped, because the surface that reaches it collects a provider token rather than bank details.
- **Evidence:** migration 00763; `payout.CanTransitionDestination` and `TestDestinationLifecycle_EveryEdgeIsTheOneIntended`; `TestIntegration_ADestinationStatusIsNotTheApplicationsToWrite`; `TestIntegration_DestinationLifecycleAndWhoMayMoveIt`; ADR-0026.

## D-061 — A verification decision lasts a year, and a stale row reports the lower level (2026-09-10, product goal §20)

- **Problem:** `compliance_profiles.expires_at` existed and nothing wrote it or read it. A verification decision with no validity window is a decision about a person as they were, forever — and the sanctions and PEP screens of §21 are exactly the things that change underneath one.
- **Chosen:** `verification.ValidityWindow` is 365 days, written onto the profile by the transition that reaches VERIFIED. The composite resolver reports the BASE level for a profile whose window has elapsed, even while the state still says VERIFIED.
- **Why derived rather than chosen:** no provider in `PROVIDER_BOUNDARY.md` §5 publishes a re-verification interval Nodal could adopt, so the number is ours. A year is the interval at which re-running a sanctions screen is worth the friction, and it is a version-controlled constant rather than configuration because shortening or lengthening it is a compliance decision and not a deployment one.
- **Why the resolver does not wait for a sweep:** a sweep that moves an expired profile to EXPIRED will exist and will sometimes be late. Reporting PAYOUT_KYC on the strength of a row whose window has passed is how an expired verification pays somebody out, so the resolver reads the timestamp itself and the sweep only tidies the state.
- **Consequences:** the level and the state can legitimately disagree for as long as it takes a sweep to run, and the disagreement is always in the conservative direction. The profile view reports `VERIFICATION_EXPIRED` with the action `REVERIFY`, which is a next step rather than a refusal.
- **Evidence:** `internal/verification` `TestLevelFrom_EarnsTheLevelFromEvidence` ("an elapsed window reports the base before any sweep runs"); migration 00761's trigger; ADR-0025.

## D-062 — A quote stands for five minutes, and an expired one is refused rather than re-priced (2026-09-10, product goal §19, §22)

- **Problem:** §19 and §22 want the customer to see the fee and the net before committing. A quote therefore has to expire; what it should do when it has is a product decision with two defensible answers.
- **Chosen:** `payout.QuoteTTL` is five minutes, and `POST /v1/payouts` against an expired quote answers `QUOTE_EXPIRED` rather than silently computing a fresh one.
- **Why five minutes:** a provider's fee schedule does not move minute to minute, so the fee is not what expires. The ELIGIBILITY behind the quote is: a dispute can arrive, a gate can be pulled, a capability can be revoked, a sanctions screen can change. Five minutes is long enough for a person to read the number and press the button, and short enough that the eligibility the quote rests on is still the eligibility that was checked.
- **Why refused rather than re-priced:** a person who saw a number and pressed the button a quarter of an hour later is entitled to be told the number moved. Charging a different one silently is the behaviour a customer discovers afterwards, from a statement.
- **What the quote also refuses:** a provider with no PUBLISHED fee model produces no quote at all rather than a quote of zero — `Capabilities.FeeModelPublished` exists precisely because a zero fee that means "we do not know" is how a customer is promised a net amount nobody agreed to. And `minimum_ok` is judged NET of fees, because sub-minimum dust is destroyed rather than returned (`PROVIDER_BOUNDARY.md` §3).
- **Consequences:** a quote funds exactly one payout — it is consumed inside the transaction that reserves the value — and its uniqueness is `(account_id, idempotency_key)` in the schema, so another caller's identical key cannot collide with it at all. The row is immutable but for `consumed_at`, because a quote that can be edited is not a quote.
- **Evidence:** migration 00764; `internal/payout` `TestQuote_ExpiryAndUsability`, `TestQuoteFee_AnUnpublishedModelIsNotAFreeOne`; `TestIntegration_AQuoteStandsAndIsSpentOnce`, `TestIntegration_AnExpiredQuoteRefusesThePayout`, `TestIntegration_AQuoteMustMatchTheRequestItFunds`; ADR-0026.
## D-063 — A native position is a derived read model only the database writes, with average cost (2026-09-10, product goal §15)

- **Problem:** §15 asks for cost basis, realised P&L, unrealised P&L and fees per holding, and warns "do not infer P&L from superficial balance differences". Nothing recorded what an account had paid for a native asset. `position_lots` (00104) is the hosted rail's FIFO lot ledger and holds its basis in `bigint` USD MINOR UNITS, which a Credit-denominated position cannot be expressed in without inventing an exchange rate for a Credit — the one thing `internal/valuedomain` and §10 forbid.
- **Chosen:** `native_positions` (migration 00772), keyed by (account, native asset), written **only** by triggers on `native_market_fills` and on `native_markets` (the creator's zero-cost allocation). `cp_app` holds SELECT and nothing else. A table CHECK states `quantity = allocation_units + units_bought_total - units_sold_total`; `cp_native_positions_unreconciled()` compares every row to `ledger_balances`, which neither the table nor its triggers write. Cost basis is AVERAGE, not FIFO. `Position.ApplyBuy`/`ApplySell` restate the trigger arithmetic in Go and are called by nothing in production; the integration test drives real fills and compares the two. Market value and unrealised P&L are computed at read time from the market's reserves, and `GET /v1/me/portfolio` states the instant.
- **Why derived rather than chosen:** the two books that already exist are authoritative and disagree about nothing — the ledger says what an account holds, the fills say what it traded — so a position is a restatement, and the only question is what stops a restatement drifting. A CHECK the database refuses to store a row without, a reconciliation query against the source the triggers do not write, and no application write path answer it. Average cost follows from the asset: closed-loop, non-redeemable, no tax event and no external price, so what a holder needs is one number nobody can game by reordering their sells — and it makes the invariant one arithmetic identity instead of a join across lots. Storing market value would mean rewriting every holder's row on every fill and the row would still be stale between fills.
- **Consequences:** a portfolio figure can never be set by an application, and a disagreement with the ledger is visible rather than absorbed. The creator's allocation is part of the position at zero cost, so the portfolio agrees with the ledger for a creator who never traded. Unrealised P&L is marked at the marginal price, which is optimistic for a large holder and is stated as such; the order ticket quotes the real exit. FIFO lots for a native asset, if a tax question ever needs them, are recomputable from `native_market_fills`.
- **Evidence:** migration 00772; `internal/nativemarket/positions.go`; `TestProp_PositionQuantityIsAlwaysTheSumOfItsFills`, `TestIntegration_PositionsAgreeWithTheGoStatementOfTheArithmetic`, `TestIntegration_PositionsReconcileAgainstTheLedger`, `TestIntegration_APositionCannotBeWrittenByTheApplication`, `TestIntegration_TheCreatorAllocationIsPartOfTheirPosition`; ADR-0027.

## D-064 — A chart reads a print table the database validates against the fill, and a gap stays a gap (2026-09-10, product goal §14)

- **Problem:** §14 asks for real charts from internal historical data and forbids fake candles. `asset_prices` carries this market's post-trade spot price and no volume, and links to the trade that set it only through a text `raw_ref`, so "how many Credits traded in this minute" was unanswerable from it. `native_market_fills` carries the amounts and no price.
- **Chosen:** `native_market_prints` (migration 00771): one row per fill with `spot_price_before`, `spot_price_after`, `effective_price`, both volumes and an instant. The application inserts it — so the instant is the same one `publishPrice` stamped on the `asset_prices` row for that fill, because they are the same observation — and a BEFORE INSERT trigger recovers the pre-trade reserves from the fill's own amounts, recomputes all three prices with PostgreSQL's truncating integer division, and raises NM001 on any disagreement. Candles are computed in SQL over a window bounded to 1,500 buckets, aligned by `date_bin` to a fixed epoch; open is the first print's pre-trade spot, close the last print's post-trade spot, and high and low look at BOTH sides of every print. A bucket with no trades is ABSENT.
- **Why derived rather than chosen:** the alternative — deriving candles from `asset_prices` joined on a parsed `raw_ref` — produces no volume and a join on a string. The alternative to the trigger — trusting the insert — is the shape migration 00712 already refused for market state. Looking at both sides of a print rather than only the trade price is not a refinement: between two trades the market sat at a price nobody traded at, and the first print's pre-trade spot IS the price the bucket opened at, so a candle from trade prices alone understates the range. Filling a gap forward invents a trade that did not happen, which §14 names directly.
- **Consequences:** the chart, the tape and the price series cannot disagree about when the market moved. A print is immutable and cannot be updated, so a chart cannot be rewritten. A market with no history returns an empty list and the UI shows the honest empty state. The window is bounded, so a client cannot request a table scan by typing a date.
- **Evidence:** migration 00771; `internal/nativemarket/prints.go`; `TestIntegration_CandlesMatchHandComputedFixtures`, `TestIntegration_APrintThatDisagreesWithItsFillIsRefused`, `TestCandleRequest_TheWindowIsBounded`; ADR-0027.

## D-065 — Market safety is its own versioned policy, and two of its defaults are deliberately permissive (2026-09-10, product goal §47)

- **Problem:** §47 asks to prevent or expose wash trading, self-dealing, spam assets, fake volume, price impact, low liquidity and halted markets. The risk kernel bounded two of them (native-market and creator concentration) and `surveillance.go` alerted on four more without blocking. Nothing bounded price impact, nothing set a floor under the liquidity a market may open with, and nothing stopped a market moving violently with no operator present.
- **Chosen:** a separate versioned document, `native_market_safety_policies` (migration 00773): per-order max price impact and max slippage, a circuit breaker (move threshold + window), a minimum opening virtual reserve, and whether a creator may buy their own asset. Immutable rows, unique versions, hashed canonical rendering, chosen by `effective_at`, recorded with an actor and a reason through `RecordSafetyPolicy` (`scripts/marketsafety`). Not `risk.Policy` columns. Impact and slippage bind a BUY only. A breaker trip pauses to CLOSE_ONLY and the tripping trade stands. The refusal is `VENUE_LIQUIDITY_INSUFFICIENT`. The compiled-in conservative policy permits 9,000 bps of impact and slippage, leaves the **breaker disarmed**, and **permits a creator to buy their own asset**.
- **Why derived rather than chosen:** a risk policy composes GLOBAL ∧ ACCOUNT ∧ AGENT and answers "how much risk may this ACCOUNT take"; three of these four are properties of a MARKET, and composing them per account would let an account-scoped row loosen a venue rule. Impact and slippage bind a buy only for the reason `risk.go` already gives for the concentration limits: refusing an exit because the exit is large traps the holder, and a control that can trap a holder is worse than the manipulation it prevents. CLOSE_ONLY rather than HALTED for the same reason, stated in 00712's own comment. The two permissive defaults are the load-bearing part: a breaker trip is undone only by a person, and a freshly launched constant-product market on a virtual reserve legitimately moves several hundred percent in minutes — the first buyers ARE the price discovery — so any threshold low enough to catch manipulation catches every launch and strands holders on a deployment nobody is watching; and the creator question was already answered in `internal/nativemarket/doc.go` ("an automated market maker has no order book, so classic self-trade prevention has nothing to match against… a detector that halts a market on a heuristic is a denial-of-service vector against creators"), which `TestIntegration_SurveillanceRaisesAlertsWithoutBlocking` holds. §47 says "prevent OR expose"; this build exposes and makes preventing a recorded policy change. `VENUE_LIQUIDITY_INSUFFICIENT` rather than a new code because on a constant-product pool "this order moves the price too far" and "there is not enough liquidity for this size" are one measurement seen from two sides.
- **Consequences:** both documents' limits are reported together on the asset detail response as "limits in force", which is §47's expose limb made literal. A deployment with an operator arms the breaker with `scripts/marketsafety`; until it does, markets never pause automatically, and that is written down here rather than discovered. A deployment that wants to refuse creator buys records the flag false and every creator BUY is then `ASSET_RESTRICTED`. The 9,000 bps ceiling means one order may be at most about 38% of the pool's effective reserve.
- **Evidence:** migration 00773; `internal/nativemarket/safety.go`; `TestCheckSafety_EachLimitAtItsBoundary`, `TestCheckSafety_CreatorSelfBuyIsAPolicyChoice`, `TestCheckOpeningLiquidity_AtTheBoundary`, `TestIntegration_TheSafetyPolicyRefusesWhatItSaysItRefuses`, `TestIntegration_TheCircuitBreakerPausesToCloseOnly`, `TestIntegration_TheBreakerDoesNotTripOnAMoveOutsideItsWindow`; `scripts/marketsafety`; ADR-0027.

## D-066 — `/v1/me/activity` is the product timeline; `/v1/accounts/{id}/activity` stays the hosted rail's operational one (2026-09-10, product goal §16)

- **Problem:** §16 asks for a unified activity timeline of Credit purchases, trades, refunds, disputes, verification and payout events. `GET /v1/accounts/{accountId}/activity` already existed and reads intents, orders, venue fills, deposits, journal transactions and reconciliation records — none of which is a §16 event. The brief asked whether to unify them.
- **Chosen:** both, unchanged in each other's territory. `/v1/accounts/{id}/activity` stays the OPERATIONAL timeline of the hosted real-capital rail, which is what an operator reads when something went wrong with somebody's money. `/v1/me/activity` is the PRODUCT timeline of §16, served by a new package `internal/activity` that owns no table and reads other domains' rows: Credit purchases and reversals, native trades, asset creation, payout requests and state changes, and admin adjustments.
- **Why derived rather than chosen:** merging them means either showing a customer their own journal transactions — the same movement counted twice beside the trade that caused it — or dropping the operational rows an operator needs. Neither answer is better than having two reads that say what they are for. `internal/activity` owns no table because an activity table fed by events is a second copy of the financial record with its own failure mode (a transaction that commits and an event that does not), and §52's audit trail would then have two answers to "what happened to this account".
- **Consequences:** a Kind is added by adding a `Source` (one SQL branch) and a summary template, and the union is ONE compiled-in constant with the kind filter as a bound parameter each branch tests against its own literal kind, so `TestSQLInjection_EveryStatementIsBuiltFromConstants` can prove it and PostgreSQL still skips a filtered-out branch. `TestEveryKindHasASource` and `TestEveryKindHasASummaryTemplate` make a half-added domain a red test rather than an empty feed. Verification, profile, security and agent kinds are left for the domains that will raise them.
- **Evidence:** `internal/activity/{doc,activity,sources,feed,summary}.go`; `internal/httpapi/wiring_markets.go`; `TestIntegration_TheActivityFeedSeesWhatTheSeederDid`; ADR-0027.

## D-067 — A non-PROD deployment records the conservative GLOBAL risk policy at boot; PROD stays manual (2026-09-10, F-26 shape)

- **Problem:** `internal/nativemarket` fails closed when no GLOBAL risk policy is on record — correctly: a deployment that has not decided how much of a market one account may hold has not decided that any amount is fine. Nothing in any deployment wrote one. `scripts/riskpolicy` exists and is a person running a command, so every fresh LOCAL, DEV, TEST and STAGING database refused every internal trade until somebody remembered, which is a control that mostly teaches people to work around it.
- **Chosen:** `cmd/api` records `risk.DefaultGlobalPolicyJSON` at boot when `EffectivePolicy` answers `ErrNoPolicy` and the environment is not PROD, through `risk.Store.RecordPolicy` — the same domain service `scripts/riskpolicy` calls, never SQL — as the SYSTEM actor `config:bootstrap`, with a reason that says in words that these are starter limits nobody signed off. The version is derived from the policy's own content hash, so a second boot is a CONFLICT on the unique version rather than a second row saying the same thing, and two deployments with the same compiled-in limits carry the same version.
- **Why derived rather than chosen:** the alternative for PROD is the one the whole audit is against — seeding a production deployment with limits nobody approved produces a control that LOOKS decided and is not, and a production deployment with no policy refusing every trade is the correct failure. The alternative for non-PROD is the status quo, in which the first thing anybody learns about the risk kernel is how to bypass it.
- **Consequences:** a fresh non-production database can trade the moment it boots, and the row says who put it there and that the numbers are provisional. PROD needs `go run ./scripts/riskpolicy -rules`. A race between two instances resolves as a CONFLICT and is treated as success.
- **Evidence:** `cmd/api/marketsurfaces.go` (`riskPolicyAtBoot`); `scripts/riskpolicy`; `internal/risk/store.go`.

## D-068 — Demo data is created through the domain services, labelled in its own register, and refused in PROD three times (2026-09-10, product goal §51, §12)

- **Problem:** §51 asks for deterministic sandbox data created "through legitimate operator/domain APIs", not raw SQL around invariants, and §12 asks that staging data be clearly distinguished from live real-value data. STAGING's markets page had nothing on it.
- **Chosen:** `internal/demo`, a fixed catalogue of four assets and their markets, created through `nativeasset.CreateDraft` → `SetStatus` → `Activate` → `nativemarket.Create` → `SetStatus`, funded with `credit.Issue` at origin `PROMOTIONAL`, traded with `nativemarket.Execute`. Every object is registered in `demo_seed_rows` (migration 00774) under a deterministic seed key in the same transaction that created it, which is both the idempotence key and the label the API joins to mark an object as demo. It runs at boot on a sandbox tier (`cfg.SandboxTier()`) and from `scripts/demodata`. PROD is refused in `NewSeeder`, in `cmd/api`, and by a CHECK on `demo_seed_rows.environment`.
- **Why derived rather than chosen:** a seeder that wrote rows directly would be the one path in the system that skips content screening, the mint, the ledger and the constant-product trigger, and the data it produced would prove nothing about the product. `PROMOTIONAL` because nobody paid for these Credits and the provenance must say so — and because no payout policy in this build, including the sandbox one, permits a promotional Credit to leave, so demo money can be spent inside the product and can never leave it. The moderation verdict is the screener's own: the demo copy is written to pass, and the seeder stops rather than approving its own content, because approving it would be manufacturing a moderation decision even where the content is harmless. The label is a table rather than a naming convention because a convention can be imitated.
- **Consequences:** a second run creates nothing and a failure part-way through is resumable. The catalogue deliberately includes one market with no trades, so the empty-chart and no-history states have a subject, and one thin market so a low-liquidity warning has something to warn about. It activates no capability gate and records no policy; where a gate is missing its trades fail with the capability's own refusal, which is the correct outcome.
- **Evidence:** migration 00774; `internal/demo`; `scripts/demodata`; `cmd/api/marketsurfaces.go` (`demoDataAtBoot`); `TestIntegration_TheSeederRefusesWhereItMustRefuse`, `TestIntegration_SeedingIsIdempotentAndEverythingItMakesIsLabelled`, `TestIntegration_DemoCreditsCanBeSpentAndCanNeverLeave`; ADR-0027.
## D-053 — Onboarding is timestamps and consent is a hashed record, not flags (2026-09-10, product goal §6, §48, §52)

- **Problem:** two pieces of product state needed a shape. Onboarding progress looked like a state machine, which is the pattern this schema uses everywhere; and "has this user accepted the terms" looked like a boolean, which is what most products store.
- **Chosen:** onboarding is four fill-once timestamps on `user_profiles` (`onboarding_started_at`, `display_name_set_at`, `terms_accepted_at`, `onboarding_completed_at`) with a birth control and a restamp control in 00756, and no state column. Consent is one append-only row per `(user, document, version, sha256 of the exact bytes shown)` in `terms_acceptances` (00759); the API reports a document as unaccepted unless the caller has a row matching the version AND the hash currently served.
- **Why derived rather than chosen:** every state machine in this schema exists because an ORDER is the invariant and an illegal edge is worth refusing at the database. Onboarding has no illegal edge: the steps are independent, can be done in either order, and cannot be undone. A state column would have invented a linearisation the product does not have and then needed a transition table to protect it. What IS worth refusing — a profile born finished, and a completion instant rewritten later — is enforced directly, and is the half a state machine would not have given for free. For consent, the hash is what makes the record mean anything: a version string whose text has since changed records that somebody agreed to something nobody can now identify. Including the hash makes "the copy was edited without the version being bumped" re-ask the user automatically, which is a small cost paid only when the text actually changes.
- **Consequences:** a typo fix in a legal document re-asks every user unless the version moves with it, which is the safe direction and is stated in `internal/terms`. An acceptance row is never updated or deleted — `cp_app` holds SELECT and INSERT only, and `forbid_mutation` is behind that — so withdrawing consent will have to be a new fact rather than an edit. `onboarding_completed_at` is stamped by the action that completes the last step, never by a sweep, so there is no job that can disagree with the columns.
- **Evidence:** migrations 00756, 00759; `internal/terms` and `TestRegistry_*`; `TestBuildTermsView_AcceptanceCountsOnlyAtTheCurrentBytes`; `TestIntegration_Terms_AcceptanceCompletesOnboardingAndIsIdempotent`, `TestIntegration_Terms_AStaleHashIsOutstandingAgain`, `TestIntegration_Profile_AStepCannotBeRestampedOrBornFinished`; `test/integration/enums` (`terms_acceptances_document_id_check` ↔ `terms.AllDocumentIDs()`).

## D-054 — What the product surfaces deliberately did not add: a third status, and a new permission (2026-09-10, product goal §4, §37)

- **Problem:** §4 lists "product status" as a field of the Nodal profile, and the `/me` surface adds six routes of which four are commands. Both invite an addition — a `user_profiles.status` column, and a `profile:write` permission — and both additions would have been defensible on the surface.
- **Chosen:** neither. The profile carries no status: `users.status` (ACTIVE/SUSPENDED/CLOSED) answers "may this person be here" and `accounts.status` (ACTIVE/RESTRICTED/FROZEN/CLOSED) answers "may this account take risk", and the product surfaces read both. Every `/me` route is authorized by `account:read`, the permission every role already holds; `GET /v1/me/security` uses `session:list_own`. The two admin routes reuse `account:read_any` and `account:freeze`.
- **Why derived rather than chosen:** a third status would be a third answer to a question that must have exactly one, and the risk kernel, the legal router and the buying-power engine all read the first two — so a profile status would either be ignored (dead) or consulted (a fourth authority over money). For the permission: a permission exists so a deployment can grant one capability without another, and there is no deployment in which a person may hold an account but must not choose their own display name, accept the terms that let them use the product, or ask to leave. A permission no deployment would ever withhold is not a control; it is a second name for "has an account", which `account:read` already is. The controls that do carry weight on these routes are the step-up on closure, the cooling-off period behind it, and the fact that only an operator can effect one.
- **Consequences:** the `/me` routes are reachable by every role including operators acting on their own records, which is correct: they are self-scoped by construction, with no identifier in the path. If a future deployment ever needs to withhold profile editing specifically, adding `profile:write` is a matrix change and a golden-test update, not a redesign. A profile therefore reports no status of its own and `GET /v1/me/account` reports the user's and the accounts' together, with restrictions written for the person they apply to.
- **Evidence:** `internal/httpapi/authz.go` (the profile block); `TestProfile_NoSelfServiceRouteTakesAnIdentifier`; `TestProfile_ACustomerCannotReachTheSupportView`; `TestIntegration_UserStatus_MovesOnlyThroughATransitionRow`; migration 00756's header.

## D-055 — Closing an account is a request that waits, and cancelling is never harder than requesting (2026-09-10, product goal §4, §37)

- **Problem:** §4 requires account deactivation. It cannot be a deletion — every financial table references `users.id`, the ledger and the audit trail are append-only, and ADR-0020 already settled that retention against an append-only table is partition detachment rather than row removal — and it cannot be an immediate status change either, because the session that asks might not be the account holder's.
- **Chosen:** `account_closure_requests` (00758). A request is born PENDING with a `cooling_off_until` fourteen days out, at most one open per user (a partial unique index, not a read-then-write); the user can cancel it at any time; an operator cancels, refuses with a reason, or effects it, and effecting writes the `users` and `accounts` transitions and revokes every session. `POST /v1/me/account/close` requires a step-up; `POST /v1/me/account/close/cancel` deliberately does not. The cooling-off period is enforced by the trigger that writes the state, not only by the service.
- **Why derived rather than chosen:** the asymmetry is the interesting half. Requesting closure is the direction that ends an account and every session, so it takes the recent strong authentication `PostWithdrawals` takes. Making cancellation take one too would mean a user who cannot step up — no MFA enrolled, a lost device — could not undo a request made from a session that could, which turns the control into the attack. The wait is in the database rather than in Go because a cooling-off period whose only enforcement is a service method is a cooling-off period one refactor away from being zero; the transition trigger refuses an EFFECTED row stamped before the deadline whoever writes it. Fourteen days is a judgement: longer than any plausible gap between a session compromise and its owner noticing a login they did not make, and short enough not to trap somebody who wants to leave.
- **Consequences:** closure is never automatic — the period makes it possible, a person makes it happen — so a deployment needs an operator before anybody can actually leave, which is part of why ADR-0024 exists. Nothing is deleted, and `TestIntegration_Closure_EffectingClosesTheUserTheAccountsAndTheSessions` asserts the user, the account and the transitions all still exist afterwards. A refused or cancelled request stays visible to the person it is about, with the reason, so the surface can say what happened rather than "no request".
- **Evidence:** migration 00758 (`CLOSURE_BORN_DECIDED`, `CLOSURE_STILL_COOLING`); `internal/profile/closure.go` and `TestClosureStateMachine_EveryPair`; `TestIntegration_Closure_CannotBeEffectedBeforeTheCoolingOffPeriod`, `TestIntegration_Closure_RequestWaitsAndCanBeCancelled`, `TestIntegration_Closure_AnOperatorCannotDecideTheirOwnRequest`; `TestProfile_ClosureRequestRequiresStepUp`, `TestProfile_CancellingAClosureDoesNotRequireStepUp`.

## D-056 — The first operator is a declaration in the deployment, not a claim from the identity provider (2026-09-10, product goal §38, ADR-0024)

- **Problem:** `operator_roles` has decided who is an operator since 00010 and nothing writes it — no route, no admin action kind, no CLI, only the LOCAL seed script. A deployment that has never had an operator cannot get one, so the gate ceremony, the kill switches, the admin plane and §38's support surface are all unreachable in it. STAGING is in that state now.
- **Chosen:** ADR-0024. `CP_AUTH_BOOTSTRAP_OPERATORS` declares `issuer|subject=ROLE` entries; at the named identity's login the row is written to the directory inside the login transaction with `granted_by` NULL and a reason naming the variable, audited on the admin stream as the SYSTEM actor `config:CP_AUTH_BOOTSTRAP_OPERATORS`, and the login then reads the directory exactly as it always did. Idempotent, never revives a revoked grant, refuses BREAK_GLASS by name and by CHECK, and refused in PROD unless it is empty or exactly one ADMIN.
- **Why derived rather than chosen:** the alternative that looks modern is a ZITADEL project-role claim, and it fails on three independent grounds. `internal/identity`'s package doc has said since it was written that the package must never take roles from provider claims, and ADR-0022 put Nodal's authority in Neon. A claim carries no `granted_by`, no `reason` and no `expires_at`, so it replaces a reviewable record with a setting in another system. And revocation would then live in two places with no reconciliation, whose failure mode is a live operator role somebody believes they removed. An HTTP route or a dual-control admin action is the right answer for the *second* operator and cannot be the answer for the first, because both need an operator to already exist.
- **Consequences:** naming the first operator is a human action — copying an opaque subject out of the ZITADEL console — and belongs in `HUMAN_ACTIONS_QUEUE.md`. Granting the *second* operator in the product is still not possible and is recorded as a gap rather than hidden; the route that closes it is an admin action of a new kind, which now has an operator to propose it. Dual control is untouched: every approval path compares subjects, not roles, and a principal holding every role the directory may name still cannot be two people. 00760 also gives `operator_roles.role` the CHECK it never had, paired with `operatorroles.Directory()` in the enum suite.
- **Evidence:** ADR-0024; migration 00760; `internal/operatorroles` and its unit tests; `internal/identity/bootstrap.go`; `TestIntegration_Bootstrap_GrantsTheDeclaredRoleAtLogin`, `TestIntegration_Bootstrap_IsIdempotent`, `TestIntegration_Bootstrap_DoesNotRestoreARevokedGrant`, `TestIntegration_Bootstrap_GrantsNothingToAnUndeclaredIdentity`, `TestIntegration_Bootstrap_MatchesOnIssuerAsWellAsSubject`, `TestIntegration_OperatorDirectoryRefusesAnUnknownRoleAndBreakGlass`; `TestBootstrap_APrincipalWithEveryRoleStillCannotApproveItsOwnProposal`, `TestBootstrap_APrincipalWithEveryRoleCannotActivateAGateAlone`, `TestBootstrap_NoDirectoryRoleHoldsADualControlPermission`; `internal/config` `RuleBootstrapOperators`.
## D-069 — The notification producer is a follower of the transition tables, not a call inside each domain service (2026-09-10, product goal §36)

- **Original recommendation:** emit a notification at the domain service's transition point, in the same transaction — the shape `internal/notification`'s own doc comment describes ("written in the same transaction as the domain event that caused it").
- **Problem:** that shape requires an `Emit` call inside `internal/credit`, `internal/payout`, `internal/nativemarket`, `internal/identity` and `internal/accounts`. Four of those were being changed by other agents in the same wave, and the fifth is the login path. A change that edits five packages nobody else may touch is a change that cannot be merged; a change that adds a notification hook to a service is also a change that gives that service a reason to know about notifications at all.
- **Chosen:** `internal/notifications.Follower`, an in-process poller in `cmd/api` that reads the rows those services already write — `credit_funding_transitions`, `payout_request_transitions`, `native_market_fills`, `native_market_transitions`, `account_status_transitions`, and the `login` rows of `security_events` — and emits a notification for each one a person needs to know about, in one transaction per source per pass. Every notification still lands inside a transaction with the row it describes read in it; what moved is which transaction, not whether there is one. `Producer.Emit` keeps the in-transaction signature, so a domain service that later wants to emit directly does so with no change to this package.
- **Why derived rather than chosen:** the in-transaction guarantee is what makes a notification honest, and the follower keeps it: the source row is already committed when the follower reads it, so a notification exists only for a state change that happened. The only thing lost is latency — up to one tick — and the only thing gained is that five packages stay untouched. Against that, an `Emit` in each service would have to be added five times, correctly, with five different reviewers, and every future domain would have to remember. A follower forgets nothing, because a transition row is the thing the state machine already cannot skip (F-42 makes the transition row the ONLY way most of these states move).
- **Why the cursor is not the safety property:** every table it follows orders on a timestamp defaulting to `now()`, which is the transaction's START time, so a transaction that began before the cursor passed and committed after it writes a row the cursor has already gone past. No ordering fixes that. The follower re-reads a two-minute lap behind its own position on every pass and deduplicates on `(user_id, dedup_key)` where the key is derived from the source row's own id. The lap finds late rows; the unique index refuses the ones the lap sees twice. Each pass commits its emits and its cursor advance in one transaction, so a crash leaves the old position and no notifications, and the next pass re-reads the window to no effect.
- **Consequences:** a notification arrives up to fifteen seconds after the fact. Six hand-written queries read six tables owned by other packages, so a renamed column is a follower that stops silently — `TestIntegration_EverySourceQueryRunsAgainstTheRealSchema` runs all six against the migrated schema for exactly that reason. A worker tier added later runs the same follower with no coordination: each source takes a try-advisory-lock and the database refuses duplicates whoever writes them.
- **Evidence:** `internal/notifications` `TestIntegration_TheFollowerNotifiesOnceForACapturedPurchase`, `TestIntegration_ACrashBetweenReadingAndEmittingDuplicatesNothing`, `TestIntegration_TheFollowerFindsARowThatCommittedBehindItsCursor`, `TestIntegration_NobodyIsToldAboutHistoryTheFollowerNeverSaw`, `TestIntegration_ANewSessionTellsThePersonWhoSignedIn`, `TestIntegration_AnAccountLeavingActiveTellsItsOwner`, `TestIntegration_EverySourceQueryRunsAgainstTheRealSchema`. ADR-0028; migration 00783.

## D-070 — One notifications table, two packages, and a CHECK that is deliberately the union (2026-09-10, product goal §36)

- **Original recommendation:** the product surface writes the `notifications` table `internal/notification` (singular) already owns.
- **Problem:** that package has no production caller and never had one; its nine kinds (`FUNDING_AVAILABLE`, `TRADE_FILLED`, …) name events that no code emits, and its `Provider` interface abstracts external delivery over two implementations, a no-op and a fan-out across nothing. The product needs a different vocabulary, a preference model, a sandbox label and a realtime hook. But the package has integration tests that write those nine kinds, and deleting a package deletes its tests.
- **Chosen:** `internal/notifications` (plural) is the product surface and the only writer on any production path. It reuses the same table, altered by 00781 rather than replaced. The kind CHECK is the **union**: thirteen product kinds plus the eight legacy names that are not also product kinds, twenty-one in total. `AllKinds()` names all twenty-one and is registered in `test/integration/enums`, so `notifications.kind` and `notifications.severity` leave the `unpaired` inventory and are compared against Go for the first time. `Producer.Emit` refuses a legacy kind, so the only writer that can put one in the table is the older package's own test.
- **Why derived rather than chosen:** the enums suite's rule is that a value the database accepts and no Go list names is a value no switch handles. Narrowing the CHECK to thirteen would break a test that still inserts the other nine; leaving the CHECK unpaired would keep the divergence the suite exists to detect. The union satisfies both and makes the debt legible: the list shrinks in one change that deletes the old package and narrows the constraint, and until then `AllKinds` says in code exactly which names are inherited.
- **What else 00781 does, and why:** a `sandbox boolean`, because ADR-0023 requires a sandbox outcome to be labelled sandbox everywhere it is stored and shown; there is no environment column here to CHECK against, so the refusal lives in Go and runs in both directions — a producer that is not a sandbox tier refuses the label, and one that is stamps it whether or not the caller asked. The immutability guard grows to cover `severity`, `sandbox` and the resource reference, which were writable after insert because they were added or overlooked when it was written.
- **Authorization: no new permission.** Every route is `account:read`. A notification centre grants no authority a customer did not already have — reading what you were told about your own account is the same permission as reading the account, and marking your own notification read adds nothing to it. The question these routes actually turn on, *is this row addressed to you*, is tenant scoping, which no entry in `operationPolicies` can express and which `internal/notifications` answers against the principal on every call. `account:read_any` is absent from every row deliberately: an operator investigating an account reads the audit trail, and a copy of what the customer was shown is a different document.
- **Consequences:** two packages name the same table, and one of them is dead. `internal/notification`'s dispatcher claims rows `WHERE delivered_at IS NULL`, so the product path stamps `delivered_at` at insert — the in-app record IS the delivery — and a product row is untouchable by it whatever a future deployment runs.
- **Evidence:** `internal/notifications` `TestKinds_TheCheckAdmitsEveryNameEitherPackageDeclares` (imports the older package and asserts containment), `TestEmit_RefusesASandboxLabelOffASandboxTier`, `TestValidate_RefusesWhatCannotBeDeduplicatedOrAddressed`; `test/integration/enums` `TestIntegration_EveryDeclaredEnumMatchesItsCheck`, `TestIntegration_NoEnumCheckAppearsUnnoticed`; `internal/httpapi` `TestEveryGeneratedOperationHasAnExplicitPolicy`, `TestEveryRouteRefusesAPrincipalWithoutPermissions`. Migrations 00781, 00782.

## D-071 — Realtime is one process's memory, and the event id is the durable cursor (2026-09-10, product goal §34)

- **Original recommendation:** PART 109's SSE stream, fed from the event bus by `Hub.Attach`.
- **Problem:** `Hub.Attach` has no caller, because attaching it needs a broker and a relay this tier does not deploy. Worse, the resume path was quietly broken for the tier it runs on: the hub's ids are a counter that restarts with the process, and `Subscribe` only sent `resync` when the client's position was BEHIND the buffer. A free instance restarts on every redeploy and after every sleep, so a browser reconnecting with `Last-Event-ID: 812` against a counter back at zero matched neither branch and silently resumed a stream that had lost everything.
- **Chosen:** the hub is fed in-process, by the notification follower, with two new event types: `notification.created` (identifiers and a title, never a body and never a figure) and `data.changed` (a scope — balance, position, market, payout, account — and a reference, and no values at all). Events gain a per-**user** address that `account:read_any` does not open, and an explicit `Broadcast` flag for the one thing that is genuinely public, a native market's price. Event ids encode the millisecond they were published at, so they are monotonic across a restart; `Subscribe` now sends `resync` unless continuity can be PROVEN; and `Last-Event-ID` names an instant the handler replays from the `notifications` table. The table is the durable resume cursor and the in-memory buffer is a nicety. Streams are capped at four per person, refused with 429 and `Retry-After` rather than a closed socket, and the stream sends `retry: 3000` so a browser reconnecting against a cold start neither hammers it nor looks broken.
- **Why derived rather than chosen:** the alternative to an in-process producer is a broker, which is a fixed monthly cost §42 forbids. The alternative to a durable resume is the buffer alone, which covers a redeploy for nobody. Encoding the instant in the id was not a preference: without it there is no honest answer to "what did I miss", because the only thing that survives the process is the table and nothing in the protocol pointed at it.
- **Postgres LISTEN/NOTIFY, deliberately not built:** it is what a second instance would need, because a notification produced by process A cannot reach a stream held by process B. It was not built now for three reasons that are all about this tier rather than about the technique: `CP_HTTP_REPLICAS=1` and one free web service mean there is no process B; `LISTEN` needs a connection held open outside the pool for the process's lifetime, which on a free instance's connection budget is a real cost for no current benefit; and NOTIFY delivers nothing to a process that was asleep when it fired, so the cursor and the durable replay would still have to exist exactly as they do. **It is the next step, not a missing one:** the seam is `notifications.Publisher`, which `cmd/api` satisfies with a hub adapter today and would satisfy with a LISTEN/NOTIFY fan-out on a multi-instance tier, with no change to the follower or the producer.
- **Consequences:** a client sees a notification within one follower tick plus the stream's own latency. A restart costs a `resync` and a refetch, which is what the client is told to do. The realtime path is not authoritative and nothing here changes that.
- **Evidence:** `internal/stream` `TestVisibility_ANotificationGoesToItsRecipientAndNobodyElse`, `TestEventIDs_SurviveARestart`, `TestSubscribe_ResyncsWhateverTheBufferCannotProve`, `TestSSE_CapsConcurrentStreamsPerPerson`, `TestSSE_ResumesFromTheDurableRecordBeforeTheBuffer`, `TestSSE_DeliversANotificationAndADataChangeAfterAnEmit`, and the pre-existing `TestHub_ResumeAndResync`, `TestSSEHandler_FramingHeartbeatAndAuth`, `TestSSEHandler_ARevokedSessionStopsStreaming`, all still passing. ADR-0028.

## D-072 — The outbox relay is not run in the API process, because nothing user-facing depends on it (2026-09-10, product goal §34, §42)

- **Original recommendation:** the pattern of D-046 and F-118 — work that would be a worker's on a paid tier runs in `cmd/api` on this one — applied to `event.Relay`, whose only production caller is the undeployed `cmd/relay-worker`.
- **Problem to answer first:** does any customer-facing surface depend on the outbox draining? `cmd/relay-worker`'s own doc says the consequence of not running it is that *"the API's SSE stream has no producer, the execution worker's wake-ups never arrive, and every read model built from events is silently frozen."* The first clause is the one that mattered here.
- **Chosen: no relay loop.** Traced each of the three claims at this commit. (1) The SSE stream's would-be producer was `Hub.Attach`, and D-071 replaces it with a follower that reads tables directly and needs no broker — so the stream now has a producer that does not involve the outbox. (2) `cmd/execution-worker` is not deployed either, so its wake-ups have no consumer to arrive at; draining rows to a broker that does not exist would not wake it. (3) The read models a customer actually reads — the activity timeline, the ledger, the export, positions — are SQL over the domain tables, not projections built from events; `internal/httpapi/readmodel.go` joins `intent_transitions`, `order_transitions`, `fills`, `deposit_transitions`, `journal_transactions` and `reconciliation_records` directly. And the whole internal economy — Credits, native markets, payouts, commerce — writes no outbox rows at all, so the product surface this goal is about is not in the outbox to begin with.
- **Why derived rather than chosen:** running the relay here would require a broker (`CP_REDPANDA_BROKERS`), which is a paid dependency §42 forbids, and it would publish to nothing. Adding a loop that cannot succeed is worse than not adding one: it converts an honest "not deployed" into a failing background task and a log line nobody can act on. The rows staying in `outbox_events` is a documented, bounded state — the table is append-only for `cp_app`, `cp_ops` may purge it, and `internal/capacity`'s database ceiling counts it.
- **Consequences:** `outbox_events` continues to accumulate on this tier. That is unchanged by this work and is recorded here rather than fixed, because the fix is a broker or a retention pass, and both are decisions about deployment topology rather than about notifications. A deployment that DOES have a broker runs `cmd/relay-worker` and everything above still holds: the follower and the relay do not interact.
- **Evidence:** `internal/event/topics.go` (the 27 registered topics; none is written by `internal/{credit,nativemarket,payout,commerce}`); `internal/httpapi/readmodel.go` (the activity timeline is a UNION over domain tables); `grep -rn "hub.Attach" cmd/ internal/ --include=*.go` outside tests returns nothing before this change and nothing after it; `render.yaml` deploys `nodal-api` and `nodal-web` and no worker.
## D-073 — The management surface is a package beside the agent runtime, not inside it (2026-09-10, product goal §17)

- **Problem:** §17 and §18 need HTTP routes that create agents, record what a person authorised, move them through a lifecycle and say honestly what is running. There were no routes for strategies or agents at all. The obvious home, `internal/agent`, is an agent TREE: depguard and `test/security` forbid it from importing signing, wallets, admin, gates, withdrawal, the kill switch, capital and risk policy, because code an untrusted proposal generator can reach must not reach authority. An HTTP-facing service placed there would either inherit restrictions it does not need or be the reason somebody loosens them. Worse, the runtime's lifecycle service is the natural thing to call, and calling it from `cmd/api` would give the agent runtime a production caller — which is exactly the fact F-65's deferral rests on and `test/security` watches.
- **Chosen:** a new package `internal/agents` (plural), called by the HTTP surface on behalf of a human. It runs no strategy code and evaluates nothing. It reuses the runtime's TYPES (`agent.State`, `agent.Stage`, `agent.CanTransition`, `agent.PauseReason`) and its TABLES, and constructs none of its services. Every state change is an INSERT into `agent_lifecycle_transitions`; since 00750 the application holds no UPDATE on `agents` at all and a SECURITY DEFINER trigger writes the row from the transition's `to_*` columns, so there is no second path even in principle.
- **Why derived rather than chosen:** the split is the import boundary the architecture already draws, read in the other direction. The restriction on `internal/agent` is about what agent code may REACH; nothing forbids a management package from reading its tables. And the alternative — one package for both — would have made "does the runtime have a caller" a question about which function in a file was called, rather than about which packages exist.
- **Consequences:** `internal/agents` may be imported by `internal/httpapi` and `cmd/api` and imports neither `internal/notifications` nor `internal/activity` (it publishes an `agents.Event` and lets those packages decide what a user is told). The gate check is a one-method `CapabilityChecker` implemented in `cmd/api` over `gates.Checker`, so `internal/gates` is not imported by a package sitting beside an agent tree. No file under `internal/` or `cmd/` names the runtime's lifecycle or emitter constructors, deliberately: the watcher reads text.
- **Evidence:** ADR-0029; `internal/agents/doc.go`; `test/security/deferred_bridge_test.go` green with the surface built; `.golangci.yml` `agent-authority` unchanged.

## D-074 — A deployment with no compiler records the attempt and says so, and fabricates no IR (2026-09-10, product goal §18, §62)

- **Problem:** §18's flow is describe → parse → review → approve, and it forbids turning natural-language text into financial authority without a compiled strategy the user has read. The NL→IR compiler exists and has no callers. Wiring it needs two independent things this build does not have: a model provider credential on the `model` slot, and a validation registry — instruments, venues, tools, composed risk policy — for the compiler's TYPE and RISK_COMPAT stages. Compiling with a key and an EMPTY registry is worse than not compiling: every instrument the user named fails the TYPE stage, so the API would tell them their strategy is invalid when the truth is that the deployment has no registry.
- **Chosen:** two interfaces are the seam — `CompilerBackend` (which `*strategy.Compiler` satisfies exactly) and `RefsLoader` — and a compile is attempted only when both are present. This build supplies neither. Every compile request writes exactly one `compile_attempts` row regardless: outcome `MODEL_UNAVAILABLE`, failure code `COMPILER_UNAVAILABLE`, `parse_result NOT_ATTEMPTED`, `stage_reached PROMPT`, no strategy version. The API returns 200 with that attempt and a sentence naming what did not happen and what would change it, and `compiler_configured` is on the strategy read model so the create flow can say so before a user writes a description rather than after.
- **Why `MODEL_UNAVAILABLE` as the outcome and `COMPILER_UNAVAILABLE` as the code:** `compile_attempts.outcome` is a closed CHECK written in 00500, and no member of it means "not configured". `MODEL_UNAVAILABLE` is true — no model was reachable — and the failure code says WHY it was not reachable, which is the distinction an operator reading the row and a user reading the API both need. Widening the CHECK for a fifth outcome would have added a value to a list every existing reader would have to learn, to express something a code already expresses.
- **Why not an error:** an unconfigured compiler is an OUTCOME, not a failure of the request. The attempt is a fact about the strategy (PART 64 records every attempt, successful or not), and a 5xx would have left no row and no explanation.
- **Consequences:** no strategy version can be produced on this tier, so no agent can be created from one, and the API says why at the point a user would try. The day a model credential and a registry both exist, the same route compiles through the real `strategy.Compiler` with no code change to the handler. Recorded in `BLOCKERS.md` under the agents heading.
- **Evidence:** `TestIntegration_ADeploymentWithNoCompilerRecordsTheAttemptAndSaysSo`; `TestIntegration_ACompilerBackendProducesAVersionAndEveryAttemptIsRecorded` and `...ARejectedCompile...` with a fake backend (no network is ever contacted from a test); `TestStrategies_CompileSaysCompilerUnavailableWithoutInventingAnything`; ADR-0029.

## D-075 — enable, pause, resume, disable, archive: what each one means on a state machine that has no backwards edge (2026-09-10, product goal §17)

- **Problem:** §17 names Pause, Resume and Disable as product actions, and the creation flow needs a separate act that turns an agent on. The agent state machine admits none of that directly: an agent is born DRAFT/DRAFT with no mode and no envelope (00736 refuses anything else), the ladder never walks backwards ("reducing authority is PAUSE or REVOKE"), REVOKED and SUPERSEDED are terminal and 00734 refuses a transition that claims to leave them, and CANARY, LIMITED and LIVE each need a bound capital envelope, hashed evidence and a dual-controlled approval.
- **Chosen:** `enable` walks DRAFT → COMPILED → VALIDATED → BACKTEST_ELIGIBLE in PAPER mode, one rung per transition row, and stops there — the first rung at which an agent may be evaluated at all, and the last one that grants no real capital. `pause` opens an `agent_pauses` row with `open_orders_policy = LEAVE` and moves the agent to PAUSED. `resume` closes the pause and returns it to its recorded stage. `disable` is REVOKED, final. `archive` is not a lifecycle transition at all: it stamps `archived_at` on the grant and removes the agent from the default list, and is refused unless the agent is already stopped.
- **Why `disable` is terminal rather than a toggle:** the state machine has no backwards edge, and inventing one would mean either walking the ladder down (which the architecture calls reducing authority and routes through PAUSE or REVOKE) or reaching a terminal state and coming back, which 00734 exists to refuse. An owner who wants the agent back makes a new grant — and reads it again, which is the point.
- **Why `archive` is not a state:** a terminal state has no outgoing edge, so "archived" could not be one without contradicting that; and hiding a row from a list is a presentation concern, not an authority change. Putting it on the grant keeps the lifecycle exactly as long as it was.
- **Why `enable` demands a step-up and the other four do not:** granting authority is what a step-up is for, and it is the same window `internal/agent` demands for a promotion. Requiring one for `pause` would put a re-authentication in front of an emergency stop, which is the reasoning POLICY_AUTHORITY §2 already applies to kill-switch activation. The five actions share one operation id, so the demand lives in the domain service rather than the boundary policy.
- **Why the operator pause route is floored on `kill:activate`:** the CUSTOMER role holds `agent:pause` — it is how an owner stops their own agent — so an admin route floored on it is reachable by every customer, which `TestCustomerRoleHoldsNoAdminRoutePermission` correctly refuses. `kill:activate` is the authority to stop risk in this deployment and no customer holds it. `internal/agents` then demands `agent:pause` AND an OPERATOR actor, so the route floor says who may reach it and the domain says who may do it.
- **Consequences:** the operator promotion ladder is untouched and is not on this surface; a customer can never reach CANARY or above from the API. An operator pause is recorded under reason code OPERATOR, so the owner's own history shows plainly that somebody else stopped it, and either of them may resume it because PART 71 says a resume is always by a person.
- **Evidence:** `TestIntegration_EnablingWalksTheLadderOneRungAtATime` (1 creation + 3 rungs, states in order); `...PauseResumeDisableArchive`; `...EnablingRequiresARecentStrongAuthentication`; `...AnOperatorPauseIsRecordedAsAnOperatorPause`; `TestAdminCommandsDeclareStepUp` and `TestCustomerRoleHoldsNoAdminRoutePermission` green.

## D-076 — An agent's budget is a ceiling, not a reservation, and "used" is derived (2026-09-10, product goal §17, §46)

- **Problem:** an agent is granted a budget in Credits. The obvious implementation reserves them, and §46 forbids showing "available", "spendable" and "reserved" as synonyms — so if Credits were reserved at creation, a user's spendable balance would fall the moment they drafted an agent that has never run, on a tier where nothing can run it. The question the brief posed was whether the ledger already has an agent reservation concept to use instead.
- **Chosen:** it does not, and none is invented. `capital_envelopes` is the nearest thing and it is a different animal: it is platform-granted, dual-controlled, required only from CANARY upwards, and holds USD minor units rather than Credits. So `budget_credits` on the grant is a LIMIT: no ledger row is written when an agent is created, `internal/credit` remains the only thing that moves Credits, and the read model reports `budget: { granted_credits, used_credits, source }` where `used_credits` is derived from the intents this agent's runs created that reached a state in which value is committed (RESERVED, PLANNED, EXECUTING, COMPLETED).
- **Why `source` is on the wire:** the answer here is always `"0"`, and a zero with no provenance is indistinguishable from a measurement that was never made. `NO_RUNS_RECORDED`, `NO_INTENTS_CREATED` and `COMMITTED_INTENTS` say which of the three it is, so a UI can render "never run" differently from "ran and committed nothing".
- **Why not RECEIVED, ELIGIBILITY_CHECKED or RISK_CHECKED:** nothing is reserved in those states. Counting them would report a budget as spent because an agent thought about spending it.
- **Consequences:** a budget cannot be overdrawn by the API because the API never spends it; enforcement belongs to the risk kernel and the capital envelope on the day an agent executes, and the grant is the user-facing statement of what they agreed to. `agent_grants.budget_credits` is `numeric(38,0)` and every Credit figure crosses the wire as an exact integer string.
- **Evidence:** `TestIntegration_AnAgentIsBornStoppedAndItsGrantIsRecorded` (the account holds no Credit lot after an agent is created); `TestAgents_CreateCarriesTheLimitsThroughAsExactQuantities`; `internal/agents.Limits.Validate`; migration 00786's column comment.
## D-077 — The customer app shows the closed-loop product; the hosted-rail pages leave the customer surface (2026-09-10, product goal §5–§9, §12–§19, §62)

- **Problem:** `apps/web` carried fourteen routes from the previous product, seven of them for the hosted rail (`/trade` against instruments, `/add-funds` in USDC, `/lab`, `/strategy` without persistence, `/nodal-economy`, `/payouts`, `/marketplace`). The hosted rail is gated off and has no venue adapter, so those pages render refusals; the product goal defines a navigation of Home, Markets, Agents, Portfolio, Activity with Buy Credits and Withdraw as primary actions, and forbids placeholder UI (§62).
- **Chosen:** the customer app's routes are the goal's: public `/`, `/product`, `/product/markets`, `/product/agents`, `/how-it-works`, `/security`, `/learn`, `/terms`, `/privacy`, `/risk`, `/get-started`, `/sign-in`; onboarding `/welcome`, `/welcome/terms`, `/welcome/done`; app `/home`, `/markets`, `/markets/products`, `/markets/:marketId`, `/create-asset`, `/agents`, `/agents/new`, `/agents/:agentId`, `/portfolio`, `/activity`, `/buy-credits`, `/withdraw`, `/verify`, `/notifications`, `/settings`, `/settings/security`, `/settings/account`. The hosted-rail pages and their routes are removed from the customer app; the internal product marketplace stays, under Markets; native asset creation stays. The Playwright suite is rebuilt around the goal's scenarios A–J (`docs/product/STAGING_E2E.md`); the honesty, accessibility and no-dead-control patterns of the previous suite are kept as cross-cutting checks.
- **Why derived rather than chosen:** a page whose only content is a refusal from a rail the product does not operate is the placeholder UI the goal forbids, and the previous goal's page list was superseded by this goal's. Nothing in the backend is removed: the hosted-rail API stays, gated, for the day it has a venue.
- **Consequences:** the operator console (`apps/admin`) is unaffected; `test/e2e` (Go, API-level) is unaffected; the frontend agents own pages under `src/pages/<area>/` and the shell agent owns routing; the old pages are deleted in the shell agent's branch once every replacement exists, not before.
- **Evidence:** `docs/product/USER_JOURNEY.md`; `docs/product/STAGING_E2E.md`; the `wt/*` frontend branches.

## D-078 — Card entry is Stripe's Payment Element; the publishable key is build-time configuration of the static site (2026-09-10, product goal §9, §27, §40)

- **Problem:** `POST /v1/payments` returns a PaymentIntent `client_secret` once; completing it needs Stripe.js in the browser, which needs the account's publishable key and a Content-Security-Policy that admits Stripe's script, iframes and API. The API has no publishable key in its configuration and should not: it is not a secret and the API never uses it.
- **Chosen:** the static site receives `VITE_STRIPE_PUBLISHABLE_KEY` at build time from the blueprint (test-mode key of the NODAL Integration account, public by design); the app refuses a value that is not `pk_test_` / `pk_live_` and renders "Payments unavailable" without it; the static site's CSP admits exactly `js.stripe.com` (script, frame), `hooks.stripe.com` and `m.stripe.network` (frame), `api.stripe.com` (connect) and `*.stripe.com` (img), nothing else. Card data never touches Nodal's origin: it is entered inside Stripe's iframe.
- **Why derived rather than chosen:** the alternative, serving the key from an API endpoint, adds a route and a config variable for a value that is meant to be embedded in client code; the other alternative, Stripe Checkout (hosted page), would need a different backend flow than the PaymentIntent the audited path already implements.
- **Consequences:** a live key can never pair with the API's sandbox provider mode, so a mistaken live key fails closed at PaymentIntent confirmation; `test/infra` keeps refusing secrets in the blueprint (a publishable key is not one); the Buy Credits page's E2E uses Stripe's test cards.
- **Evidence:** `render.yaml` (`nodal-web` env and CSP); the Buy Credits page and its scenario B spec.

## D-079 — The operator console's authority is the generated authority document, not the OpenAPI contract (2026-09-10, ADR-0023, product goal §38, §62)

- **Problem:** `apps/admin` reads two generated artefacts and they disagree. `src/generated/authority.json` is exported by `internal/adminplane` from `internal/security`, `internal/admin`, `internal/gates` and `internal/killswitch` — the tables the server enforces — and a Go golden test keeps it equal to them. `packages/generated-client/src/schema.d.ts` is generated from `openapi/openapi.yaml`, whose `Capability` enum is a hand-written restatement of `gates.AllCapabilities()` and has fallen behind it: ten names against twenty. The console resolved the disagreement in the contract's favour, refusing to address any capability the enum did not list, on the reasoning that a request the server would reject with 400 is worse than a visible refusal. ADR-0023 made the cost of that concrete: five of the six capabilities a sandbox tier activates (`CREDIT_PURCHASE`, `NATIVE_ASSET_CREATION`, `NATIVE_MARKET_TRADING`, `PAYOUT_RESERVE`, `PAYOUT_SETTLE`) are missing from the enum, so on the deployment the console exists to operate, an operator could neither sandbox, unsandbox, suspend nor revoke the gates that were live.
- **Chosen:** the authority document decides what exists; the contract is treated as a possibly-stale restatement and the disagreement is reported to the operator rather than acted on. The console addresses every capability the document declares, `inApiContract` becomes a drift detector that renders a named notice in the gates view, and the one cast this requires sits in `api.ts` with the reason attached.
- **Why derived rather than chosen:** the premise was checked and is false. No request validator is wired — the generated server binds `{capability}` as a plain string (`BindStyledParameterWithOptions`, `Type: "string"`) and validates it with `gates.Capability.Valid`, which is the twenty-name list. So the console was refusing requests the server accepts. Between the two artefacts the choice then makes itself: the authority document is generated from the enforcing code and is machine-checked against it, and the contract's enum is neither. Deferring to the narrower one costs an operator the ability to stop a live capability, which is the last thing a console may lose.
- **Consequences and its bound:** this settles which artefact wins about *what exists*, not about what an operator may do. Permissions, step-up windows and surfaces still come only from the authority document, and nothing in the console has become more permissive: `sandbox` and `unsandbox`, which `internal/adminplane` does not export yet, are gated on the `gate.propose` write because `gates.Admin.sandboxOp` requires exactly `security.PermGatePropose` and exactly `gates.StepUpMaxAge` — the same permission and window as the first step of the real ceremony — and the fallback prefers a declared `gate.sandbox` write the moment one appears. Every command is still refused again by the server. The drift itself is not fixed here (`openapi/openapi.yaml` was out of scope for this branch); regenerating the enum from `internal/gates` removes both the notice and the cast.
- **Evidence:** `apps/admin/src/api.ts` (`inApiContract`, `contractCapabilities`), `apps/admin/src/views/gates.ts` (`contractDriftNotice`, `gateWrite`), `apps/admin/src/scan.test.ts` ("the gates surface can address every declared capability"). Observed against a local sandbox tier on 2026-09-10: `GET /v1/admin/gates` returned five `SANDBOX` rows of which four are outside the contract's enum, and `POST /v1/admin/gates/CREDIT_PURCHASE/unsandbox` then `/sandbox` both succeeded through the console's own request path.

**Resolved, 2026-09-10 (later).** `openapi/openapi.yaml`'s `Capability` enum was regenerated and names all twenty, so the premise this decision worked around is gone. The console's side of it is withdrawn: `views/gates.ts` no longer renders the drift notice, `api.ts` takes a typed `Capability` and drops the cast, and a declared name the contract does not list is once again refused with the drift named — which is the behaviour that was correct all along and only became harmful while the two artefacts disagreed. What replaces the notice is `scan.test.ts`, which holds the enum and the authority document's capability list equal, so a future divergence fails CI rather than reaching an operator as something to read and act on. The general rule the decision states — between two generated artefacts, the one generated from the enforcing code wins, and a disagreement is reported rather than acted on — stands; it simply has no instance today. The bound also stands: it settles what exists, never what an operator may do.

## D-080 — Two reads are public: the legal registry and market discovery (2026-09-10, product goal §5, §48)

- **Problem:** the public site must show what Nodal is, including its markets, and a visitor must be able to read the binding legal text before creating an account. Every market read and the legal registry were behind a session, so the site could only render explainers and examples — and an explainer beside a document is a second text that can drift from the one whose hash an acceptance records.
- **Chosen:** `GET /v1/terms` serves the registry (`internal/terms`) with bodies and no acceptance state, publicly; `GET /v1/native-markets` (the discovery list) is public. Nothing else moves: a market's summary, candles, trades, every `/me` read, every account read and every command stay behind a session, and acceptance stays `POST /v1/me/terms-acceptances`.
- **Why derived rather than chosen:** the list carries product data only — no balance, position, holder or identity — and the registry is by definition the text everyone must be able to read; the deny-by-default authorization keeps its invariant test (`TestNoRouteIsUnintentionallyUnauthenticated`) with the two paths named explicitly, so a third public read cannot appear by accident.
- **Correction (2026-09-10, F-196/F-198, D-110):** the premise above was true of what the list was FOR and false of what it returned. `NativeMarketSummary` carried `creator_account_id` as a required field and the route accepted `?creator_account_id=` as a filter, so an unauthenticated caller could both read the identifier and enumerate one account's creations by it; and the statement had no predicate on `content_moderation_state`, so a REJECTED verdict removed nothing and the creator-supplied name, symbol, description and image kept being served to anonymous visitors beside the verdict that rejected them. D-110 removes the field and the filter from the public projection under one flag, and adds two always-present NULL-disabled exclusions (REJECTED content, and DELISTED from the default page). The sentence above now describes the response.
- **Search cost (F-200):** the `?q=` term is matched by a `to_tsvector`/`plainto_tsquery` predicate plus a prefix `LIKE` on `symbol` and an unanchored `ILIKE` on `name`, over `native_assets` only — a table with one row per launched asset, bounded by the creation flow and by moderation, not by traffic. The unanchored `ILIKE` cannot use an index, so its cost is a sequential scan of that table per request; at the general budget of 600/min per IP that is the same order of work as the discovery list itself, which scans the same join. It is stated here rather than fixed because a trigram index would be the fix if the table ever stops being small, and "small" is a property somebody has to re-check rather than assume.
- **Consequences:** the public site's `/terms`, `/privacy`, `/risk` render the served bytes and the explainers retire; `/product/markets` previews the live sandbox list with its labels; rate limits on the general class apply to both reads.
- **Evidence:** `internal/httpapi/handlers_public_terms.go`, `authz.go`, `authz_test.go` (`publicPaths`), `openapi/openapi.yaml`.

## D-115 — The sandbox demo catalogue gets its own variable; CP_SEED_ENABLED keeps the scripts it always named (2026-09-10, goal §54, F-144)

- **Problem:** `CP_SEED_ENABLED` was required of every binary, documented as *"Allow seeding clearly-labeled fake users/assets/balances"*, validated by `RuleNoSeed`, set to `"false"` in `render.yaml` — and read by nothing outside `internal/config`. Meanwhile `demoDataAtBoot` seeded eight SANDBOX-labelled demo markets, a demo Credit balance and an activity feed on every boot of a sandbox tier, keyed on `cfg.SandboxTier()` alone while its own docstring said *"when the deployment is a sandbox tier AND ASKS FOR IT"*. So the deployed STAGING declared that it does not seed and seeded, and the one variable an operator would reach for did nothing. The obvious fix — make `CP_SEED_ENABLED` the control — is not available: `RuleNoSeed` refuses `true` in STAGING and PROD, which is every deployment that could ever run the catalogue.
- **Chosen:** two variables for two things. `CP_API_DEMO_DATA` (bool, default `false`, `only(ServiceAPI)`) is the catalogue's control: `demoDataAtBoot` runs on `cfg.SandboxTier() && cfg.API.DemoData`, `config.Validate` refuses `true` on any deployment that is not a sandbox tier (which excludes every PROD, because `SandboxTier()` is false there whatever the policy says), and `render.yaml` sets it `true` on STAGING with the reason written beside it. `CP_SEED_ENABLED` keeps the developer seed scripts it always claimed: `config.SeedScriptsAllowed` is the reader, and `scripts/seed` and `scripts/seedeconomy` call it.
- **Why derived rather than chosen:** the two kinds of fake data are not the same kind. The demo catalogue is SANDBOX-labelled product data, created through the real domain services, registered in `demo_seed_rows`, forbidden in PROD by a CHECK, and *wanted* on the one tier that may hold it — ADR-0023 exists to let a sandbox tier rehearse the product. The seed scripts write dev identities, a devnet USDC pair and a SEED ledger posting, and are LOCAL/DEV/TEST-only by construction. A single switch would have had to be simultaneously forbidden in STAGING (`RuleNoSeed`) and true in STAGING (the rehearsal), which is not a configuration, it is a contradiction.
- **Why `SeedScriptsAllowed` treats absence as yes:** `make seed` and CI's browser suite have never set the variable and have always worked. Requiring one would break the recipes rather than control anything, and the environment guard each script already applies is what decides who may say yes. The variable's job is to be able to say **no**, which is what an operator reaching for it wants, and it now can. An unparseable value is an error rather than a default: the one thing worse than a switch that does nothing is a switch that reads "no" as "yes".
- **Consequences:** `scripts/demodata` is deliberately **not** gated on `CP_SEED_ENABLED` — it loads the SANDBOX catalogue a sandbox tier is entitled to hold, and gating it there would make the command unrunnable on the one deployment it exists to serve (its own docstring: *"an operator who wants to see the failure rather than a warning in a log stream"*). A STAGING that genuinely does not want demo markets sets `CP_API_DEMO_DATA=false` and gets an empty markets page, honestly. The requirements table grows by one, which `test/infra`'s config-table check and `.env.example` both see.
- **Evidence:** `internal/config/seed.go`, `internal/config/config.go` (`APIConfig.DemoData`), `cmd/api/marketsurfaces.go`; `TestSeedScriptsAllowed`; `TestAuditConfigDeploy_SeedingHonoursTheVariableThatForbidsIt`; migration 00774's CHECK; ADR-0023.

## D-116 — The static site's CSP admits `'unsafe-inline'` on `style-src` and nothing else (2026-09-10, goal §54, F-139)

- **Problem:** CSP Level 3 governs a `style=` ATTRIBUTE with `style-src-attr`, which falls back to `style-src` when it is absent. The deployed policy declared `style-src 'self'` and no `style-src-attr`, so every inline style attribute in the bundle was refused. The app ships four, and one of them is `SegmentedBar`'s `flexGrow` — the width of every segment of the available/reserved/pending balance bar, which its own file header calls the central information-design problem on Home. Blocked, the bar renders empty while its `aria-label` still reads "Available now, about 62%; …": a figure stated to a screen reader and withheld from an eye, which is the honesty rule inverted.
- **Chosen:** `style-src 'self' 'unsafe-inline'`. The components are not rewritten.
- **Why derived rather than chosen:** there are three candidates and two of them fail. `style-src 'self'; style-src-attr 'unsafe-inline'` is the precise instrument and is not supported by every browser the app targets; a browser that does not implement `style-src-attr` falls back to `style-src` and blocks the attribute again, so the precise form is the current bug plus a directive that hides it. Hashes cannot express `SegmentedBar`'s attribute at all: the value is computed per render from an exact base-unit quantity, so there is no finite set to hash. Rewriting the components into CSS custom properties set from JavaScript moves the same value into a `style` attribute on the same element — `el.style.setProperty` is not governed by CSP, but writing it through React's `style` prop is, and a rewrite that changes how the value is applied rather than whether it is inline buys nothing while touching the one component the goal calls central.
- **The bound, which is the reason this is acceptable and is asserted rather than described:** `script-src` stays `'self' https://js.stripe.com` with no `'unsafe-inline'` and no `'unsafe-eval'`; this bundle renders no user-controlled HTML (there is no `dangerouslySetInnerHTML` in the tree); `object-src 'none'`, `base-uri 'self'` and `frame-ancestors 'none'` are unchanged. So what an injected style could reach is the appearance of a page an attacker cannot get script into and cannot reframe. `TestRender_TheStaticSitesSecurityHeadersAreWhatTheAppNeeds` holds every one of those, so the relaxation cannot quietly widen.
- **Consequences:** the balance bar renders. A future component may add an inline style without thinking about the policy, which is the cost; the compensating control is that the policy's other directives are now pinned by value rather than by presence, so the next widening is a test failure.
- **Evidence:** `render.yaml` (`nodal-web` headers); `apps/web/src/components/SegmentedBar.tsx`, `Skeleton.tsx`; `TestAuditConfigDeploy_TheCSPPermitsTheInlineStylesTheAppShips`, which walks the app source and fails if the policy refuses what it finds.

## D-117 — The deployed build stamps `RENDER_GIT_COMMIT`, and no configuration variable carries a version (2026-09-10, goal §54, F-142)

- **Problem:** `build/Dockerfile` stamps `config.BuildVersion` from a `VERSION` build arg that defaulted to `dev`, and `render.yaml` declared no `VERSION`. So every image ever built from this blueprint reported `build_version: dev` from `/v1/version` — the endpoint whose entire purpose is to prove which build is running — and because `BuildVersion` is part of `Config.Hash`, the hash could not distinguish two builds of the same configuration either. `HUMAN_ACTIONS_QUEUE.md` item 3 promised the push would unblock exactly that comparison.
- **Chosen:** `ARG RENDER_GIT_COMMIT=` and `ARG VERSION=${RENDER_GIT_COMMIT:-dev}`, in both stages of the Dockerfile. Render passes a service's environment variables to `docker build` as build args — which is the sole reason `CMD` is declared in `render.yaml` — and sets `RENDER_GIT_COMMIT` itself, at build time and at run time. Nothing is added to `render.yaml` and nothing joins the configuration table.
- **Why derived rather than chosen:** the alternative the finding proposed is `- key: VERSION` in the blueprint, and a blueprint `value:` is a literal — Render does not interpolate environment variables in `render.yaml`, so the entry could only ever carry a fixed string that would be wrong from the first commit after it was written. A prompted (`sync: false`) `VERSION` would be an operator retyping a SHA on every deploy, which is a step that will be skipped and whose being skipped is invisible. `RENDER_GIT_COMMIT` is supplied by the platform, is the exact commit being built, and needs nobody to remember anything.
- **Why both stages:** an `ARG` declared inside a stage is scoped to it. The builder stamps the binary; the runtime stage writes `org.opencontainers.image.version`. A label that says `dev` over a binary that says the commit is the same defect wearing a smaller hat.
- **Consequences:** a local `docker build` with no `--build-arg` is unchanged and still says `dev`, which is correct — it is not a Render build. `scripts/images` passes `--build-arg VERSION=<sha>` explicitly and that still wins, because an explicit `--build-arg` overrides the default. The mechanism depends on Render continuing to pass service environment variables as build args; if that stopped, `CMD` would stop arriving too and the image would fail to build rather than quietly reporting `dev`, which is why the test asserts `CMD` is still declared.
- **Evidence:** `build/Dockerfile`; `TestAuditConfigDeploy_TheBuildStampsAVersion`; `HUMAN_ACTIONS_QUEUE.md` item 3.
## D-081 — The product domains join the activity timeline as Sources here, not as hooks there (2026-09-10, product goal §16)

- **Problem:** D-066 built `/v1/me/activity` over Credit purchases, trades, asset creation, payouts and admin adjustments, and left seven kinds for "the domains that will raise them". Those domains landed. A person could verify their identity, accept a withdrawal disclosure, add and disable a payout destination, ask to close their account, create an agent and have it paused by an operator — and not one line of it appeared anywhere they could read.
- **Chosen:** eleven kinds — `VERIFICATION_UPDATED`, `PAYOUT_DESTINATION_ADDED`, `PAYOUT_DESTINATION_DISABLED`, `TERMS_ACCEPTED`, `ACCOUNT_CLOSURE_REQUESTED`, `ACCOUNT_CLOSURE_DECIDED`, `AGENT_CREATED`, `AGENT_PAUSED`, `AGENT_RESUMED`, `AGENT_DISABLED`, `NATIVE_MARKET_PAUSED` — each a `const src…` branch plus a summary template in `internal/activity`, and nothing at all in the domain that wrote the row.
- **Why derived rather than chosen:** the extension point D-066 designed already said where a kind goes. A hook in each domain would make "is this on somebody's timeline" a property of the lifecycle rather than of what happened, and it would need adding eight times, correctly, by eight people. A branch here is a row that already exists, read once; an item cannot exist for something that did not happen and cannot be missing for something that did, which is the whole premise of a feed that owns no table.
- **The owner pin, which is the one thing that could have leaked:** six branches read tables keyed on a USER — compliance profile transitions, terms acceptances, closure requests and their transitions. Each joins `accounts` on the owner AND pins `acc.id = $1`. Filtering by owner alone would show a person with three accounts one verification decision three times; joining without the pin would put it on an account it is not about. `TestIntegration_TheProductDomainsAppearOnTheTimeline` gives a stranger their own verification decision and asserts the owner's feed does not contain it.
- **Why `ACCOUNT_CLOSURE_DECIDED` exists and the brief did not ask for it:** a closure request is born PENDING and decided by a transition row (00758). One kind sourced from the request would report the request's CURRENT state on an item stamped with the moment it was made — a status that changes under a past item, which an activity feed must not have. Two kinds are two facts that each happened once.
- **Who hears about a market pause:** everyone who has traded it, which is the population `internal/notifications` already tells, PLUS the creator of the asset. A creator may never have traded their own market and is the person a delisting matters most to.
- **Sandbox labelling:** where the domain that wrote the row labelled it sandbox — a verification session, a payout destination — that flag is the `demo` column and the item is `Simulated`. To a reader "a demo seeder made this" and "this was a rehearsal" are the same fact: nothing moved anywhere (ADR-0023). The column's contract comment was widened to say so rather than the meaning being stretched quietly.
- **No agent name reaches a sentence.** It is owner-supplied text bounded only by a length CHECK; `summary.go` builds a string that is displayed, and a symbol has passed `nativeasset.Screen` while a name has not. The sentence says what happened and the reference says which agent.
- **Consequences:** `feedQuery` is eighteen branches, which is past the depth `test/security`'s constant-expression scanner walks, so the union is three shallow named groups joined by a shallow one — the same string, and a provable one. `ActivityFeedKind` in the contract grows to eighteen. Security events stay absent deliberately: `internal/notifications` already tells a person about a new sign-in, and a security page has a different retention policy from a timeline of what happened to an account's value.
- **Evidence:** `internal/activity/{activity,sources,summary,doc}.go`; `TestEveryKindHasASource`, `TestEveryKindHasASummaryTemplate`, `TestEverySourceIsInTheCompiledQuery`, `TestSummary_TheProductKindsSayWhatHappened`, `TestSources_TheProductKindsScopeToTheAccountInTheParameter`; `internal/activity/integration_test.go` (four integration tests); `test/security` `TestSQLInjection_EveryStatementIsBuiltFromConstants` green.

## D-082 — Two writers tell somebody their agent stopped, and they agree by construction (2026-09-10, product goal §17, §36)

- **Problem:** two seams at once. The follower had no source for verification decisions or agent pauses, so §20's whole journey and every operator pause were silent. And `agents.Deps.Events` was wired `nil` in `cmd/api` with a comment saying a publisher would be attached "when those surfaces exist" — they existed.
- **Chosen:** `VERIFICATION_UPDATED` and `AGENT_PAUSED` become follower sources, and `cmd/api` additionally wires an `agents.Publisher` that emits the SAME `AGENT_PAUSED` notification the instant the agent transaction commits. Both derive the dedup key from the `agent_pauses` row through one exported pair, `notifications.AgentPauseRef` and `AgentPauseCopy`, so the unique index on `(user_id, dedup_key)` makes whichever arrives second a no-op. The publisher is the latency; the follower is the guarantee.
- **Why two writers rather than one:** a follower alone is up to fifteen seconds late, and "an operator stopped your agent" is a message a person is waiting for. A publisher alone would lose the telling whenever the process died between the commit and the emit — which is exactly what D-069's follower shape exists to survive. Having both costs one unique-index conflict per pause and buys both properties.
- **Why the publisher opens its own transaction:** `agents.Publisher` is called AFTER the agent transaction committed, deliberately: an agent that could not be paused because a notification failed would be a control defeated by a mailbox. The row it describes is therefore already durable, and the notification is a fact of a transaction of its own — the same guarantee the follower has.
- **`agents.Event` gains `PauseID`.** A consumer has to key on the PAUSE and not on the agent: a second pause of the same agent is a second telling and the same pause seen twice is one. Looking the row up in the adapter instead would be a query that can race the resume that closes it.
- **Who is told, and who is deliberately not:** only a pause somebody OTHER than the owner opened (`paused_by_actor_type <> 'USER'`), applied identically by both writers. An owner who paused their own agent pressed the button. `AGENT_DISABLED` raises nothing: disabling is an owner-only act — `agents.ownerActor` refuses every principal but the account's own USER and the operator surface can only pause — so the only kind that could carry it is `SYSTEM`, which `Kind.Suppressible` reports false for, and an unsuppressible notification for something the reader just did is the worst version of this. The timeline carries it instead (D-081).
- **Verification: seven states of ten, and no account id.** UNVERIFIED, STARTED and PENDING are the machinery of a check the person is standing in front of. The notification names no account, because verification is a property of a person: joining `accounts` would produce one `Emit` per account for the index to collapse, and whichever sorted first would be the one on the row. EXPIRED gets its own words — an expired decision and a rejected one land in the same inbox a year apart and only one of them is a judgement about the person (D-061).
- **The sandbox flag rides in the copy and the data, not on the row.** `Producer.Emit` stamps `notifications.sandbox` from the DEPLOYMENT and refuses a caller that sets it otherwise, so a rehearsal decision on a tier that labels nothing else would be indistinguishable from an approval. The body says SANDBOX and says it is not an approval.
- **Two claims proved rather than assumed:** a circuit breaker pauses a market through the transition row `SetStatus` writes, so the existing `native_market_transitions` source IS the breaker's notification; and effecting a closure writes the accounts transition, so `ACCOUNT_RESTRICTED` already covers account closure. Neither needed a new source and both now have a test that fails if either stops being true.
- **Consequences:** the follower reads eight tables, and `ScopeVerification`, `ScopeEligibility` and `ScopeAgent` join the realtime invalidation scopes.
- **Evidence:** `internal/notifications/sources.go`, `follower.go`; `cmd/api/agentevents.go`; `TestVerificationCopy_AnExpiredDecisionIsNotARejection`, `TestAgentPauseCopy_NamesTheReasonAndNotThePerson`; `TestIntegration_AVerificationDecisionTellsThePersonItIsAbout`, `…AProviderSessionInFlightTellsNobody`, `…ASandboxVerificationSaysItIsARehearsal`, `…OnlySomebodyElsesPauseTellsTheOwner`, `…ABreakerTripTellsTheMarketsTraders`, `…AClosedAccountTellsItsOwner`, `…AnOperatorPauseTellsTheOwnerOnceAndOnlyOnce`, `…TheOtherAgentEventsTellNobody`.

## D-083 — The withdrawal disclosure is refused in the domain service, and reported as a step (2026-09-10, product goal §48, §19)

- **Problem:** `internal/terms` said it plainly — "BeforeWithdrawal must be accepted before a conversion or payout request. Nothing in this package enforces that; the withdrawal surface asks." Nothing asked. `POST /v1/payouts/quote` and `POST /v1/payouts` both succeeded for somebody who had never been shown the document about value leaving the platform.
- **Chosen:** `payout.CreateRequest` and `payout.QuoteRequest` carry `DisclosureAccepted`, and `Service.Create` and `Service.Quote` refuse without it with a new code `TERMS_ACCEPTANCE_REQUIRED` (422) naming the document in `documents`. `GET /v1/me/eligibility` reports `TERMS_NOT_ACCEPTED` and keeps every figure. The fact comes from a new `profile.Service.Outstanding`, read inside the same transaction the payout is decided in.
- **Why the domain service and not the handler:** `internal/payout` has several ways in — a person pressing a button, an operator resolving a stuck payout, a retry, a worker — and a check in the HTTP layer applies to one of them. The field has no "unknown" value on purpose: `false` refuses, so a caller that forgets to supply it stops a payout rather than permitting one, which is the direction a mistake here has to fall.
- **Why an error and not an ineligible Decision:** a `Decision` is about WHICH of an account's units may leave, and an ineligible one still writes a `payout_requests` row. "This person has not agreed to the terms under which value leaves" is not a fact about their units, and a request row standing against it would be a record of an ask that should never have been taken. The integration test asserts no row and no reservation exist after the refusal.
- **Why the quote refuses before it is priced:** quoting first and refusing at the commit would show somebody a number and then tell them they may not have it.
- **Why the eligibility page keeps its numbers:** an unsigned disclosure is the normal state of everybody who has never withdrawn, because §48 deliberately does not ask at signup. Zeroing the buckets would tell all of them their money is stuck when an unread document is the whole of it. So it lowers the verdict and changes no amount — the shape `MINIMUM_NOT_MET` already had — and it is a next step like `REQUIRES_VERIFICATION`.
- **Why a new code:** `VERIFICATION_REQUIRED` would send somebody into an identity flow they may already have completed; `FORBIDDEN` says the account may not do this at all. What is missing is a signature, and a client that cannot tell those apart shows the wrong screen.
- **Consequences:** an acceptance counts only when the version AND the content hash match the bytes served now (D-053), through the one function the terms view also uses, so amending the disclosure without bumping its version asks everybody again. A deployment that has not wired the legal registry answers "not accepted", because a deployment that cannot establish agreement has not obtained it.
- **Evidence:** `internal/payout/service.go` (`disclosureRefusal`), `quote.go`; `internal/profile/service.go` (`Outstanding`, `acceptedNow`); `internal/eligibility/withdrawal.go`; `internal/httpapi/wiring_verification.go` (`disclosureAccepted`); `TestIntegration_APayoutIsRefusedUntilTheDisclosureIsAccepted`, `…AQuoteIsRefusedBeforeItIsPriced`, `…TheDisclosureIsReadForTheAccountsOwner`, `…AnAcceptanceOfOtherBytesDoesNotCount`, `…AnUnwiredRegistryRefusesRatherThanPermits`, `…TheEligibilityExplanationReportsItAsAStep`; `docs/product/VERIFICATION_AND_WITHDRAWAL.md` §3.

## D-084 — What a sandbox tier provisions at boot, and the sweep D-061 said would exist (2026-09-10, product goal §20, §51)

- **Problem:** two boot-time gaps with the same shape — a control or a fixture that a document said would exist and no deployment ran.
- **The Credit asset.** D-068's demo seeder was a permanent no-op on the one tier it was built for. Everything in the internal economy is keyed on a single `assets` row with kind CREDIT; `demo.NewSeeder` refuses without it, `credit.Service.AssetID` looks it up, and the payout quote's scale is its decimals. The only thing that ever wrote one is `scripts/seedeconomy`, which refuses to run anywhere but LOCAL, DEV and TEST — exactly the environments that are not the sandbox tier. STAGING booted, logged "this deployment has no Credit asset", and served an empty markets page forever.
- **Chosen:** `creditAssetAtBoot` registers it through `assets.Repository.Create` — the domain service, the same call the script makes, never SQL — on a sandbox tier that has none. Idempotent because 00711 permits exactly one, so it is a lookup first and a race resolves as a CONFLICT treated as success. The same shape as D-067's risk policy at boot, and the same refusal: PROD is not seeded, because six decimals is a decision and a production deployment's unit of account should be created by somebody who knows they are creating it.
- **The verification expiry sweep.** D-061 recorded that "a sweep that moves an expired profile to EXPIRED will exist and will sometimes be late". It did not exist. The resolver already reports the base level for a profile whose window has elapsed, so nothing an expired verification permits can leave — that half was built and is what makes this safe. What was missing left a profile reading VERIFIED while every surface treated the person as unverified, and made §20's EXPIRED, whose next step the profile view renders as REVERIFY, a state no deployment could reach.
- **Chosen:** `verification.Service.ExpireOverdue`, run by `cmd/api` on the same five-minute ticker as the internal verification sweep. One transaction per profile: batching would hold a row lock on every one of them until the last committed, and a person verifying at that moment would block behind a maintenance pass. A transition that races a re-verification or an operator's suspension is skipped rather than raised — the row is no longer VERIFIED, so there is nothing to expire. The actor is SYSTEM `verification:expiry-sweep` and the reason says in words that this is not a rejection.
- **Why both live in `cmd/api`:** the answer D-046, F-118 and D-069 already gave. This deployment runs one web service and no workers (`render.yaml`), so the alternative to running periodic work here is not running it somewhere better, it is not running it at all.
- **Consequences:** a fresh sandbox database boots into a working product — one Credit asset, one GLOBAL risk policy, four demo markets with six fills, six sandbox gates — and boots into the same one twice. `TestIntegration_BootingTwiceCreatesEverythingOnceOnASandboxTier` counts the rows, so four idempotence claims that were four comments are one assertion; its negative case withdraws the single `CP_API_LEGAL_POLICY=SANDBOX` declaration and finds no Credit asset and no demo data, while the risk policy is still recorded.
- **Evidence:** `cmd/api/sandboxtier.go` (`creditAssetAtBoot`), `cmd/api/verifyexpiry.go`, `internal/verification/expiry.go`; `TestIntegration_BootingTwiceCreatesEverythingOnceOnASandboxTier`, `…ADeploymentThatIsNotASandboxTierSeedsNothing`, `…TheSweepExpiresOnlyWhatIsOverdue`, `…TheSweepIsIdempotentAndFindsNothingTwice`, `…TheResolverDidNotNeedTheSweep`, `…TheSweepLeavesEveryOtherStateAlone`.

## D-085 — The payout submission and settlement passes run in the API process (2026-09-10, product goal §22, §25)

- **Problem:** `payout.Service.Submit` had no caller anywhere in `cmd/`, and neither did `Reconcile`. Everything up to the reservation worked and was tested: a person quoted, asked, and watched their Credits leave their spendable balance into `PAYOUT_RESERVED`. Then nothing. The request sat in VERIFIED forever, the provider was never told, and the sandbox provider's ten-second settlement — built so the journey could be rehearsed end to end — was never observed by anything. Value reserved with no path out is the trap F-52 found on reconciliation records and F-90 found on credit settlement.
- **Chosen:** `runPayoutSweeps` in `cmd/api`, on a fifteen-second ticker: submit every request in VERIFIED to the deployment's single payout provider, then ask the provider about everything in `SUBMITTED`, `PROVIDER_PENDING` or `PAYOUT_STATUS_UNKNOWN` and apply the answer through `Reconcile`. `payout.Service.AwaitingSubmission` is the one new read.
- **Why fifteen seconds rather than five minutes:** a person is standing in front of this one. The other in-process passes reconcile invariants nobody is waiting on; this is the step between "I asked for my money" and "it is on its way", and the sandbox provider settles after ten seconds precisely so the whole journey fits inside a browser session. It is the notification follower's cadence, for the same reason.
- **Neither pass owns any of the correctness, deliberately.** `Submit` writes and COMMITS the provider idempotency key before it calls, its re-entry branch returns without calling again for anything already claimed, and the provider is idempotent on that key. So a pass that runs twice submits once, and this could run every second without changing the outcome. The advisory lock is about load — two instances doing the same provider round trip — and a loser skips rather than queues.
- **Why a deployment with no provider does nothing and says so once:** zero providers is the honest state of a deployment with no conversion contract and every payout answers PROVIDER_UNAVAILABLE long before reaching this; more than one is a deployment that has not decided which provider a payout belongs to, and a sweep that guessed would be choosing for it.
- **Consequences:** `RECONCILIATION_REQUIRED` from the settlement pass is logged at info, not error — a payout the provider has not decided yet is normal and an alarm on it is noise an operator learns to ignore. One request's failure never abandons the rest.
- **Evidence:** `cmd/api/payoutsweep.go`; `internal/payout/service.go` (`AwaitingSubmission`); `TestIntegration_AReservedPayoutReachesTheProviderAndSettles`, `…ASweepThatRunsTwiceSubmitsOnce`, `…ASweepWithNoProviderDoesNothing`.

## D-086 — A sandbox tier's settlement window is two minutes, and is not configuration (2026-09-10, product goal §12, ADR-0023)

- **Problem:** only `SETTLED` Credit value is payout-eligible under any policy in this build, including the sandbox one. `CP_CREDIT_SETTLEMENT_WINDOW` is 720 hours on STAGING and that is right — it is the card chargeback window, and a captured payment really is reversible for that long. So on the tier whose entire purpose is rehearsing the product, nothing anybody bought could ever be withdrawn. The withdrawal journey ADR-0023 exists to let a sandbox tier exercise was unreachable past its first step, and the failure was invisible, because it is a clock rather than a refusal.
- **Chosen:** on a sandbox tier the window is two minutes and the existing `cmd/api` sweep ticks every twenty seconds. Both are compiled in and keyed off `cfg.SandboxTier()`, refused in PROD by construction and by a second environment check.
- **Why two minutes:** long enough to observe `REVERSIBLE` — the state matters and a rehearsal that skipped it would rehearse the wrong thing — and short enough that a person testing the product does not go and do something else. The sweep ticks faster because a two-minute window swept every fifteen minutes is a fifteen-minute window.
- **Why not a second environment variable:** the reason `CreditConfig` already gives for the real one — a risk determination read straight from the environment is outside `scripts/configcheck` and outside the configuration hash, so it could be changed in a dashboard while `/v1/version` reported the hash that exists to detect exactly that. A sandbox window is not a risk determination at all: nothing was charged and nothing can be charged back. So it is a property of the tier, stated once beside the sweep.
- **Consequences:** the boot logs a WARN naming the window, because a two-minute settlement is a startling thing to read in a log and should be. PROD and every non-sandbox deployment keep the window they recorded, and a window nobody configured still falls back to the conservative thirty-day default rather than to zero.
- **Evidence:** `cmd/api/creditsettle.go` (`creditSettlement`, `sandboxSettleWait`); `TestCreditSettlement_ASandboxTierSettlesInMinutesAndProdNeverDoes`; `TestIntegration_ASandboxTierSettlesInMinutesAndAChargebackWindowDoesNot`; `docs/product/CREDIT_ECONOMY.md` §2.

## D-112 — The stream's scope map is derived from the query keys, not copied out of them (2026-09-10, frontend audit F-202)

- **Problem:** `SCOPE_KEYS` in `StreamStatus.tsx` mapped each `data.changed` scope to a list of query prefixes written out as strings. Five of them named reads no page has made since D-077 removed the hosted rail, and the two keys a fill actually changes — `["me","portfolio",…]` and `["me","activity",…]` — were in no scope at all. Neither half is visible when it breaks: TanStack Query matches a prefix, and a prefix that matches nothing invalidates nothing and raises nothing. A fill emitted `position` and `balance`, five invalidations hit nothing, and the position on screen was the one thing that did not refresh. The trade screen looked right because it carried its own copy of the map beside its mutation, which hid the defect precisely where a reviewer would have looked.
- **Chosen:** every entry in the map is a constant taken from a key factory in `api/queries.ts` (`prefixOf(portfolioKeys.portfolio(""), 2)` and so on). The map contains no string literal of its own. A page that must invalidate after its own command calls the exported `invalidateScopes(client, "balance", "position")` rather than writing a second list.
- **Why derived rather than asserted:** both were offered. A test comparing two hand-written lists can only fail after somebody has already written the wrong thing down; deriving makes the wrong thing unwritable — renaming a key moves the map with it, and deleting one stops the build. The assertion is kept as well, in `src/lib/stream-scopes.test.ts`, because the derivation cannot check the SHAPE of the map: that no literal has crept back in, that every declared prefix is used, that the reads a fill changes are reachable from the signals a fill emits, and that the app knows every scope `internal/notifications/follower.go` can send.
- **Why a source-level test:** `StreamStatus.tsx` is a React module and the unit suite is plain `node --test`. Importing it would mean either a bundler in the test path or a fourth copy of the map to import instead. Reading the two files is honest about what it can prove and proves the property that matters.
- **Consequences:** a page may say that a fill happened and may not decide for itself which reads that made stale. `MarketDetail`'s local workaround is deleted, so there is one mechanism. The three scopes the follower emits that the app had never heard of — verification, eligibility, agent — are mapped rather than falling through to invalidating the entire cache.
- **Evidence:** `apps/web/src/components/StreamStatus.tsx`, `apps/web/src/pages/markets/MarketDetail.tsx`, `apps/web/src/lib/stream-scopes.test.ts`; the audit's two reproductions of F-202 pass.

## D-113 — `Money.tsx`, `MintIdentity.tsx` and `Tabs.tsx` are deleted rather than kept (2026-09-10, frontend audit F-214)

- **Problem:** three components under `src/components/` that nothing imports. One of them, `Money.tsx`, is a second money formatter with its own absent and malformed states.
- **Chosen:** all three deleted, with `TabButton` from `Button.tsx`, which exists only to build a tablist.
- **Why `Money.tsx` is not a judgement call:** the rule this interface is built on is that every figure goes through `Figure`, and that rule is only true while there is nothing else to go through. A second formatter that predates the number ladder in `UI_UX_SYSTEM.md` §6 is a correct-looking module a future page can reach for, and the failure would be a rounded figure nobody noticed rather than a crash.
- **Why `Tabs` went too:** the condition was "keep it if a page uses it by the end of the work". None does, and the one surface that looks like tabs — the markets navigation — argues in its own header why it is a `<nav>` of links instead: its two views are two addresses, and a tab that changes the URL tells a screen-reader user to expect a panel and hands them a navigation. Keeping a correct primitive with no caller is keeping a component nobody has ever rendered, which is what this finding is about. The argument survives it, written into §7 for whoever builds the next one.
- **Why the stylesheet keeps `.tab`:** the markets navigation styles its links with it. The rule is about modules nothing imports, not about every declaration that becomes unreachable.
- **Consequences:** `UI_UX_SYSTEM.md` §7 records a primitive that was built and deleted rather than describing one that is not there, and §11's "render every figure through `Figure` rather than through `Money.tsx`" no longer names a file.
- **Evidence:** `docs/product/UI_UX_SYSTEM.md` §7 and §11; `apps/web/src/lib/audit-frontend.test.ts` — "every component in the tree has a caller" — passes.

## D-114 — The audit reproductions join the guard tests in the source scan's exclusion list (2026-09-10, frontend audit F-212)

- **Problem:** `src/lib/scan.ts` walks the whole app tree and both `e2e/` and `src/`, and the two guard suites it feeds ban a vocabulary of constructs — the numeric constructor, the float parsers, and by name the generic apology `UI_UX_SYSTEM.md` §4 forbids. The audit's own reproductions are in that tree, and they cannot do their job without writing down what they forbid: `src/lib/audit-frontend.test.ts` asserts that the forbidden sentence appears nowhere and has to spell it three times to say so, and `e2e/audit-frontend.spec.ts` explains in prose which numeric constructor it is avoiding. Scanned, the first test could never pass — it matched itself — and the second broke the float guard for the whole tree.
- **Chosen:** both files are added to the exclusion set in `scan.ts`, beside `source-scan.test.ts`, `honesty.test.ts` and `e2e/honesty.spec.ts`, which are there for exactly this reason and say so.
- **Why not blank the comments before scanning:** the float rule deliberately scans comments, and it is right to — a commented-out `Number(balance)` is a line somebody uncomments. `codeOnly()` exists in `source-scan.test.ts` and is applied only to the three chart rules, which are rules about what code does. Widening it would weaken the guard for every file to accommodate two.
- **Why not edit the auditor's prose:** it would work, and it would make both files say less about what they are testing than they say now. A reproduction that has to be vague about the construct it reproduces is a worse artefact than an exclusion list with five entries on it.
- **Consequences:** the exclusion list is five files, all of them tests whose subject is the rule they quote. Every page, every component, every library module and every other spec is still scanned, which is the whole surface these rules were ever about. A `Number(` that appears in a product file still fails the build.
- **Evidence:** `apps/web/src/lib/scan.ts`; `node --test src/lib/*.test.ts` — `source-scan.test.ts` and `honesty.test.ts` green, "no error state says 'Something went wrong'" green.
## D-104 — A notification carries the instant it became visible, and the resume cursor reads that one (2026-09-10, product goal §34, §36, ADR-0028)

- **Problem:** three clocks described one notification and `Last-Event-ID` compared two of them. `created_at` is the occurrence, copied from the source row by the follower; the live event id is the publish instant, assigned by the hub; `notifications.Since` filtered `created_at > since`. A client whose last event was published at P was therefore never told about any notification that occurred at or before P and was WRITTEN after it — which is every row the two-minute lap picks up and every row from the tick it was disconnected for. `sse.go` then skipped the buffer's copies of those notifications because the durable hook had "succeeded", so neither replay covered them (F-186).
- **Chosen:** a fourth column that is the missing clock. `notifications.inserted_at` (00801) records when the ROW became a fact; `Since` filters and orders on it, the replayed event ids encode it, and the guard makes it immutable like the rest of the content — a row whose insert instant could be edited is a row that could be moved out of somebody's resume window.
- **Why `clock_timestamp()` and not `now()`:** `now()` is the transaction's start, so a follower pass writing two hundred notifications would stamp all two hundred identically and a cursor could not order them. `clock_timestamp()` rises within the transaction. What it does NOT do is order two concurrent writers by commit time, and nothing here claims it does.
- **Why both replays now overlap instead of meeting:** the live id is the publish instant, which is always at or after the insert, so a client's cursor can stand slightly ahead of a notification it has not been sent — every other notification of the same follower pass is exactly that. The hub's buffer covers that window while the process lives, and an empty buffer answers `resync`, which the client turns into a refetch. Trimming the two to meet would need the two clocks to be provably equal, and they are not. A notification delivered twice costs one query invalidation (`StreamStatus.tsx` invalidates the notifications query and does nothing else with it); one skipped is a person never told.
- **Why the live id was left on the hub's clock:** making it `EventIDAt(inserted_at)` would put a notification's id slightly BEHIND events published before it, and the hub's replay filter (`e.ID > afterID`) and its buffer ordering both assume ids rise with publication. The gap that fix would close is the one the buffer replay already covers; the disorder it would introduce is in the mechanism doing the covering. Recorded because the brief for this fix proposed it and this is the reason it was not done.
- **Consequences:** one column, one index (`user_id, inserted_at, id`), and a resume that returns more rather than less. `created_at` keeps its meaning everywhere it is shown — the notification centre still orders and displays what happened, not when it was recorded.
- **Evidence:** `migrations/00801_a_notification_carries_the_instant_it_became_visible.sql`; `internal/notifications/store.go` (`Since`); `cmd/api/notifications.go` (`notificationResume`, `hubPublisher.Notify`); `internal/stream/sse.go`; `TestAuditAgnot_LastEventIDSkipsNotificationsWrittenBehindThePublishInstant`, `TestIntegration_NotificationResumeReadsTheTableNotTheBuffer`, `TestIntegration_SinceIsOldestFirstAndReportsTruncation`.

## D-105 — An agent is created only from a version of its own account's strategy, accepted by a person (2026-09-10, product goal §18, ADR-0029)

- **Problem:** `agents.Service.Create` checked that two ids were non-empty. It never asked whether the strategy belonged to the account making the grant, whether the version belonged to the strategy it named, or whether anybody had accepted it — so a stranger could bind an agent on their own account to somebody else's private compiled version, `agents.strategy_id` and `agents.strategy_version_id` could describe two different strategies, and an agent could hold authority over IR nobody ever approved (F-187).
- **Chosen:** the check runs inside the transaction that writes the agent and the grant, under `FOR SHARE` on the version and its strategy, and refuses unless all three hold. The schema carries the half that is a schema question: `UNIQUE (strategy_id, id)` on `strategy_versions` and a composite foreign key from `agents (strategy_id, strategy_version_id)` (00802).
- **Why the codes differ:** "not yours" and "does not exist" both answer NOT_FOUND, because a FORBIDDEN tells a stranger that the id they guessed names a real strategy version and a 404 on the next one tells them it does not. "Yours, but nobody accepted it" answers INVALID_STATE_TRANSITION: the caller may see this one, and what is missing is the approval step goal §18 requires, which is a thing they can go and do.
- **Why in the transaction rather than before it:** a check made before the insert is a check about the state at the time of the check. The share lock is what makes the grant and the fact it rests on the same fact.
- **Why ownership is not a constraint:** who owns the strategy and whether the REQUESTING principal is that owner are questions no foreign key can see. The schema says the pair is consistent; the service says the pair is the caller's.
- **Consequences:** `ownerActor` and `requireOwnership` (and the strategy equivalents) move from `security.RequireAccount` to `RequireAccountOwner` — `account:read_any` is a read capability and has no business granting an agent authority — and `Act`, `Get` and reading somebody else's strategy answer NOT_FOUND. A deployment whose `agents` rows already mismatched would fail to migrate, loudly, which is the correct outcome for a row recording authority over the wrong document.
- **Evidence:** `internal/agents/service.go` (`requireGrantableVersion`, `notYours`); `migrations/00802_an_agent_names_a_version_of_the_strategy_it_names.sql`; `TestAuditAgnot_AnAgentCannotBindToAnotherAccountsStrategyVersion`, `TestAuditAgnot_TheStrategyVersionMustBelongToTheStrategy`, `TestAuditAgnot_AnAgentIsRefusedFromANeverAcceptedVersion`, `TestIntegration_OnlyTheOwnerActsOnTheirAgent`.

## D-106 — What a notification may carry about a sign-in, and who may read the table (2026-09-10, ADR-0021 §4, product goal §36, §52)

- **Problem:** the follower copied a login's IP address and User-Agent out of `security_events` — partitioned by month so a month can be DROPped (00740) — into `notifications.data`, which refuses DELETE from every role including the owner and which no retention pass names. Both analytics roles held SELECT on it, and `privileges_test.go` listed the table under `opsHousekeeping`, asserting a retention capability the trigger falsifies (F-188).
- **Chosen:** bound the CONTENT, because the privilege cannot be bounded — the row is undeletable by design and that design is right. The notification carries a /24 (or /48) locality, a two-word device summary, and the route where the exact address is served: `/v1/me/audit`, which reads `security_events`, which has a retention policy. 00803 revokes SELECT on `notifications` from `cp_readonly` and `cp_ops`, and the DELETE with it.
- **Why a /24 and not the address:** the question a person is answering is "was this me", and a /24 answers it — a sign-in from another country reads differently from one on your own network — while not being the identifier a subpoena, a leak or an analytics export would want. The exact address is one documented route away, from the copy that can be forgotten.
- **Why the masking is in SQL:** the exact address then never enters the process at all, so no later change to this code can accidentally store it. A fix that relied on remembering to mask would be a fix that lasts until the next person edits the query.
- **Why `cp_ops` loses SELECT as well as `cp_readonly`:** ADR-0021 §4 says a table holding personal data gets its own REVOKE, and §3's test is "a role with no use for personal data cannot read it". `cp_ops` runs retention and there is no retention here to run. Its DELETE grant was never real: the guard refuses every role, so the grant named a capability the database does not have, and a test asserted it.
- **Consequences:** the new-session notification's copy changes (it names a network and a browser instead of an address); `notifications.data` keys change from `ip`/`user_agent` to `ip_prefix`/`device`/`exact_address_at`; no client reads those keys — `StreamStatus.tsx` invalidates a query and the notification centre renders title and body. Nothing that was recorded is lost: it is read from `security_events`.
- **Evidence:** `internal/notifications/device.go`, `sources.go` (`readNewSessions`); `migrations/00803_two_capabilities_the_grants_claimed_and_the_table_refuses.sql`; `test/integration/migrations/privileges_test.go`; `TestAuditAgnot_ALoginsIPAddressDoesNotLandInATableNothingCanPurge`, `TestIntegration_ANewSessionTellsThePersonWhoSignedIn`, `TestIntegration_ApplicationRolePrivileges`, `TestDeviceSummary_NamesTheBrowserAndThePlatformAndNothingElse`.

## D-107 — One market pause notifies five hundred people per pass, and the rest on the next one (2026-09-10, product goal §34, §42)

- **Problem:** the market-pause fan-out was unbounded and said it was bounded: its comment named `CP_CAPACITY_MAX_ACCOUNTS`, which may be zero (`capacity.Budget` documents zero as "no ceiling of this kind") and which only STAGING and PROD are required to state. One pause on a popular market was one transaction writing a notification per holder while holding a pool connection; past the statement timeout the pass fails, and every retry fails identically, so the source stops (F-192).
- **Chosen:** `notifications.MaxPauseFanOut` = 500 per pass, with the participant query excluding whoever already holds this pause's notification and whoever switched the kind off. Those two exclusions are what make a cap safe to page: consecutive passes drain the remainder while the transition row is still inside the follower's two-minute lap, and the dedup key makes the re-read free.
- **Why not refuse a zero `CP_CAPACITY_MAX_ACCOUNTS` instead:** zero is a documented, correct value — it is how a paid tier turns the infrastructure ceilings off while keeping the financial ones — and a fan-out that is only bounded when an unrelated variable happens to be set is not bounded. A ceiling on a pass belongs beside the pass.
- **Why compiled in:** D-086's reason. This is a property of one process's transaction budget, not a risk determination somebody should be able to change in a dashboard outside the configuration hash.
- **Why the preference exclusion is conditional on the kind being suppressible:** a holder who switched the kind off never gets a row, so the "already told" exclusion never excludes them, and they would occupy a place in every page forever and starve whoever sorts after them. The clause is turned off by a NULL parameter when the kind is not suppressible, so it can never skip somebody `Emit` would have told.
- **Consequences:** a pause on a market with more than five hundred holders is notified over several passes rather than one, up to the lap. A change that produces no notification at all — a pause on a market nobody has traded — now publishes no `data.changed` signal either, because signals travel with created notifications (F-190); that market's viewers learn of the pause on their next REST read rather than immediately, which is the price of not broadcasting the same signal to every client eight times.
- **Evidence:** `internal/notifications/sources.go` (`MaxPauseFanOut`, `marketParticipants`, `suppressibleKind`); `internal/notifications/follower.go` (`runSource`); `TestIntegration_ABreakerTripTellsTheMarketsTraders`, `TestAuditAgnot_TheLapRepublishesTheSameSignalsEveryPass`, `TestIntegration_EverySourceQueryRunsAgainstTheRealSchema`.
## D-108 — A market must open at a price the controls can measure, and the controls saturate rather than pass (2026-09-10, product goal §47, §54, F-193)

- **Problem:** the marginal price is `(V+R)·10^18 / Y`, truncated, and nothing bounded `Y` against `V`. `checkOpeningLiquidity` bounded the virtual reserve from below and said nothing about the supply it is spread over, so ten billion units of an eighteen-decimal asset on the 1,000-Credit floor priced at `10^27/10^28` — zero — and stayed zero, because on this curve the price can only rise from the one a market opened at. The price-impact ceiling, the slippage ceiling and the circuit breaker all divide by that price, and all three answered ZERO, which reads as "within limits" and "the market has not moved". The portfolio marked every holder at nothing.
- **Chosen:** three things. (1) `MinSpotUnits` = 10^6: a market may not open at a marginal price below a million units of price scale, checked once at creation. (2) `PriceImpactBPS` is computed from the reserves, `|Y² − Y'²| / Y'²`, and saturates on a fill that does not carry them; `SlippageBPS` and `moveBPS` saturate on a reference below `MinSpotUnits`; `applyBreaker`'s early return on a zero reference is removed. (3) `ratioScaled` reports one unit rather than zero for a positive ratio, mirrored in SQL by `cp_native_market_price` (migration 00805) so the Go curve, the discovery ordering and the NM001 print check still agree to the unit.
- **Why 10^6 and not something else:** every §47 control is a ratio quoted in basis points, and one basis point of a price `S` is `S/10,000` units. Below `S = 10^4` a basis point is not representable at all, so a limit expressed in basis points is measuring rounding; 10^6 leaves two further digits, so no single unit of movement can flip a limit. What it costs: at the default six decimals nothing anybody meets — a billion tokens on the 1,000-Credit floor open at 10^13 — and at eighteen decimals it binds, which is the point. A thousand eighteen-decimal tokens open on that floor, ten thousand need ten times the reserve, and a creator who wants a billion has to say what pool prices them. That is the trade the price scale forces, stated at creation instead of discovered as a market priced at nothing.
- **Why checked once, at creation:** `x = V+R` never falls below `V` (every payout draws on `R`, which starts at zero) and `y` never rises above `Y0` (holders in aggregate hold `Y0 − y`), so `S = x·10^18/y` is at every instant at least `V·10^18/Y0`. Bounding the opening price bounds the price for the life of the market, and a per-trade check would be the same check run a million more times.
- **Why derived rather than chosen:** it is compiled in rather than a `SafetyPolicy` field, and that follows from what a policy is FOR. `MinOpeningLiquidityCredits` is a deployment deciding how deep a market must be — a product judgement, reasonably different on different deployments. Whether a price of zero can be divided by is not a judgement; a policy value here would be a policy that could turn the three §47 controls off by setting one number, which is the thing D-047 says a limit must never be able to do.
- **Why impact moved to the reserves rather than being guarded like the other two:** a guard would have refused the unmeasurable case, which is right, and left the measurable ones reading two rendered prices whose difference is only as good as the rendering. The reserves are what the curve actually moves and they do not round. The formula is exact for both directions, and it makes the impact independent of `PriceScale` entirely.
- **Consequences:** `PriceImpactBPS` on a hand-built Fill with no reserves now saturates instead of returning a number, which is a refusal; two unit tests that stated the old contract are restated on reserves. An armed circuit breaker on a market whose price cannot be measured trips, pausing to CLOSE_ONLY — holders can still sell. A creator of an eighteen-decimal asset may be refused at launch with a message naming both the price it would open at and the two ways to fix it.
- **Evidence:** `internal/nativemarket/{curve,safety,discovery}.go`; `migrations/00805_a_positive_price_is_never_printed_as_zero.sql`; `TestAudit_AMarketWhoseSpotTruncatesToZeroIsNotOpenable`, `TestCheckOpeningLiquidity_RefusesAPriceTooSmallToMeasure`, `TestTheSafetyReadersFailClosedOnAPriceTheyCannotMeasure`, `TestPriceImpactBPS_IsTheMarketsMoveNotTheCallersCost`; F-193.

## D-109 — pg_temp is pinned by a test over the catalogue, not by remembering (2026-09-10, product goal §54, F-194)

- **Problem:** 00717 fixed F-48 by pinning `pg_catalog, public, pg_temp` on five SECURITY DEFINER functions and explaining the hazard in prose. 00771 and 00772, written after it, wrote four more functions with `SET search_path = public`. One of them lets `cp_app` shadow `native_assets` in `pg_temp` and have a definer trigger write a `native_positions` row with an owner and a quantity of the caller's choosing — past the control ADR-0027 §1 and D-063 both name.
- **Chosen:** migration 00804 pins all four, and `test/integration/migrations` asserts over `pg_proc` that every `prosecdef` function in `public` has `pg_temp` in its `proconfig`, with a negative control naming five functions the rule exists for.
- **Why derived rather than chosen:** the fix for a defect that has occurred twice is not a third correct migration. Both previous fixes were correct and neither was a property anything could check, so the third occurrence was a matter of who wrote the next migration. A catalogue assertion is the same shape as the rules this suite already holds — append-only tables, money columns, transition binding — and it costs one query.
- **Consequences:** a new SECURITY DEFINER function fails the suite until it pins the path, including one that pins a path without naming `pg_temp` (which inherits the caller's, and is the worse version). `cp_native_positions_unreconciled` is pinned too although it is SECURITY INVOKER and not exploitable: a rule with an exception is a rule nobody can check.
- **Evidence:** `migrations/00804_a_definers_search_path_pins_pg_temp.sql`; `TestIntegration_EverySecurityDefinerPinsPgTemp`; `TestAudit_APositionCannotBeForgedThroughAnUnpinnedSearchPath`, `TestAudit_TheNM001PrintCheckReadsOnlyTablesTheCallerCannotControl`; F-194, F-48.

## D-110 — The public markets list carries no identity, and the projection is what says so (2026-09-10, product goal §12, §35, §54, F-196)

- **Problem:** D-080 made `GET /v1/native-markets` unauthenticated on the ground that "the list carries product data only — no balance, position, holder or identity". Every row carried `creator_account_id` as a REQUIRED field and the route accepted it as a filter, so an anonymous caller could read the identifier and enumerate one account's creations by it.
- **Chosen:** the field is off `NativeMarketSummary` and the filter is off the public route. Inside `internal/nativemarket` a `listScope` — not a `ListRequest` field — carries whether the caller may see identity, and it drives BOTH halves of the answer: the projection selects the nil UUID in place of the creator, and the creator predicate is `($3::uuid IS NULL OR ($13::boolean AND a.creator_account_id = $3))`. The creator is a field of `NativeMarketDetail`, on the gated `GET /v1/native-markets/{id}/summary`.
- **Why an empty page rather than an ignored filter:** ignoring a filter means answering a different question and calling it the answer, which is how an enumeration becomes a full listing nobody notices. The predicate that blanks the column is the predicate that refuses the question, so the response and the filter cannot come apart in a later edit.
- **Why the scope is not on ListRequest:** a request is what the CALLER asked for; a scope is what the SURFACE may answer. Making it a request field would let the unauthenticated handler widen its own answer by filling in a bool, which is exactly the mistake being fixed.
- **Why the gated detail keeps it rather than publishing a pseudonym:** a per-asset creator pseudonym would answer "same creator?" without naming anybody, and nothing in §12 or §13 asks that question — the markets page does not group by creator and there is no creator page. An opaque identifier nobody consumes is a second identity space to keep consistent and to explain. The gated read carries the real account id, which is what the profile join will need when profiles land.
- **Consequences:** the web's "Created by" row reads the detail response and renders nothing when it is absent; `MarketQuery.creatorAccountId` is gone from the client. `authz.go`'s comment and D-080's "why derived" sentence now describe the response rather than the intention.
- **Evidence:** `internal/nativemarket/discovery.go`; `internal/httpapi/{handlers_native_markets,authz}.go`; `openapi/openapi.yaml`; `apps/web/src/{api/queries.ts,pages/markets/MarketDetail.tsx}`; `TestAudit_ThePublicMarketListCarriesNoIdentity`, `TestIntegration_MarketDiscoveryFiltersSortsAndSearches`; F-196.

## D-111 — A holder list is a shape, not a list of people (2026-09-10, product goal §16, §54, F-197)

- **Problem:** `top_holders[]` rendered `{account_id, quantity}` to any caller with `native_asset:read`, which every customer role has. Any signed-in stranger could read a market's largest positions by account and watch them move trade by trade, while `prints.go` refuses to put an account id on the tape for exactly this reason.
- **Chosen:** a holder row is `{rank, quantity, share_bps}` plus `is_you` on the caller's own row. No account id is rendered at all — not even the caller's own, which they sent. The caller is resolved from an optional `account_id` query parameter through the same ownership check `/me/portfolio` uses, so naming somebody else's account is a 403 rather than a leak.
- **Why not even the caller's own id:** a field that is sometimes an identity is a field a client will eventually render as one, and the difference between "sometimes present" and "present" is a code path nobody tests. `is_you` carries the whole of what the caller is entitled to learn about a person from this list, and it carries it as a boolean that cannot be anything else.
- **Why the rank and the denominator come from the database:** a rank counted in Go is a rank within the returned page, and a share taken over the page would make the tenth holder of a thousand look like a tenth of the asset. Both are computed with window functions over every holder, before the limit. The denominator is the sum of customer balances rather than the circulating supply: units still in the pool are held by nobody, and a creator's allocation is minted outside the curve, so a share of circulating supply is a figure that can exceed one hundred per cent — which the web page was previously computing and rendering.
- **Why no operator route here:** a named holder list is surveillance. `native_market:surveil` exists as a permission and is on no route in this build; adding one is a route, an authz row, a contract and a page, and it is not what this fix is. When it is built, it takes the same rows with the names attached and it is gated by that permission.
- **Consequences:** `nativemarket.Holders` returns rows naming nobody and `HoldersFor` marks one; `Holding.AccountID` is set only on the caller's own row and is zero everywhere else. `GET /v1/native-markets/{id}` takes no account, so no row there is marked. The web's holder table shows a place, a holding and a share, and marks the reader's own row.
- **Evidence:** `internal/nativemarket/repository.go`; `internal/httpapi/handlers_native_markets.go`; `openapi/openapi.yaml` (`NativeAssetHolder`); `apps/web/src/pages/markets/MarketDetail.tsx`; `TestAudit_NoSignedInAccountLearnsAnotherAccountsHolding`, `TestGetNativeMarketsMarketIdSummary_NamesNoHolder`; F-197.
## D-097 — The notification follower's cursor only moves forward, and never far into the future (2026-09-10, product goal §16, §36, F-167)

- **Problem:** one read was doing two jobs. `Follower.runSource` read from `cursor - lap`, took at most `batch` rows, and made the LAST of them the new cursor — so the new cursor was the 200th row counted from a point two minutes BEHIND where the cursor already stood. It could move backwards, and it did as soon as a source produced 200 rows inside one two-minute lap: every later pass re-read the same window, deduplicated all of it, and wrote the same instant back, forever. The lap and the dedup index, which exist to make a re-read free, were also what made the stall invisible.
- **Chosen:** two reads. The DRAIN reads forward from exactly where the last pass stopped and is the only thing that moves the cursor, so the cursor is monotone by construction. The LAP re-reads the window behind the cursor and moves nothing.
- **Why the lap runs only when the drain left room:** a pass that filled its batch draining a backlog is not near the head, and the head is where a late commit hides — `occurred_at` defaults to the transaction's START time, so the rows that can land behind a cursor are the ones stamped moments ago. Re-reading the window on every pass of a long drain would spend the batch on rows everybody has already been told about, which is the stall again with extra steps. The pass after the drain catches up re-reads it.
- **Why the cursor may not stand more than half a lap past the database's clock:** `occurred_at` is settable by every writer, so one row carrying a wrong clock — a skewed host, a service passing the wrong instant — would drag the cursor to that instant and silently skip everything written between now and then. That is the same permanent invisible loss in the other direction. The bound is not zero because the two clocks are not the same clock and a row stamped milliseconds ahead is ordinary; it is half a lap because anything a cursor that far ahead skipped is still inside the window the next pass re-reads. A row beyond the horizon is still REPORTED — it exists and somebody needs to know — it just does not carry the cursor with it.
- **Consequences:** a signal now rides with the fact it accompanies rather than with every row the lap sees, so a client no longer receives an invalidation for the same resource every fifteen seconds for as long as the two-minute window holds it. A full drain that produced no notifications is logged at WARN: legitimate after a restore winds a cursor back, and the shape the stall had, which nothing could see. And the reproduction is declared last in its package (`stall_audit_test.go`) because its fixture writes 260 transitions stamped up to 52 seconds in the future into a database every test in that package shares; no assertion of it changed.
- **Evidence:** `internal/notifications/follower.go` (`runSource`, `notAfterCursor`, `futureHorizon`); `TestAudit_FollowerStallsForeverAfterABatchSizedBurst`, `TestIntegration_TheFollowerFindsARowThatCommittedBehindItsCursor`, `TestIntegration_TheFollowerNotifiesOnceForACapturedPurchase`.

## D-098 — The sanctions screen rides the verification transition row, and a birth records itself (2026-09-10, product goal §20, §21, F-168)

- **Problem:** migration 00761 took `identity_state` out of the application's reach with F-42's full treatment and, one statement later, granted `UPDATE (… sanctions_state …)` back. The screen is not an attribute: `internal/eligibility` reads it as one of the allowlists that decides whether a payout may proceed. One UPDATE cleared a HIT with no edge, no actor, no evidence and no record that anything had happened.
- **Chosen:** the screen rides on `compliance_profile_transitions`, in two new columns, both NULL when a row says nothing about it; the existing SECURITY DEFINER trigger writes the column; the column grant loses it; a constraint trigger refuses a change no row describes.
- **Why not a second transition table:** a screening decision and a verification decision are made from the same provider answer, recorded by the same writer, in the same transaction, and read by the same regulator. Two tables would be two trails to join and two places to forget, and the trail a regulator reads is the whole point of the binding.
- **Why a birth writes its own row:** `compliance.Upsert` creates a profile from a provider's first answer, so a row can be INSERTed already holding CLEAR or HIT. Every binding above is about CHANGES, and a row born screened never changed — which is exactly the hole F-122 found in the state next to it. The birth of a profile whose screen is not the default writes its own transition row, from UNVERIFIED to UNVERIFIED, recording `UNKNOWN → «what it was born with»`. A profile born UNKNOWN writes nothing: there is no decision to record, and a row per profile created would be noise in the table a person reads to find out what was decided about them.
- **Why the trigger generates a UUIDv7 rather than `gen_random_uuid()`:** `internal/id.Parse` accepts only an RFC 9562 version 7 UUID. A v4 in that column would be a row the database can write and this system cannot read, which is F-131 exactly. PostgreSQL 16 has no `uuidv7()`, so `cp_uuid_v7()` builds the layout by hand.
- **Consequences:** `cp_compliance_apply_state_transition` had to stop a same-state row erasing the standing `verified_at` and blanking `expires_at` — a row shape this migration is the first to create. The notification follower stops reading a same-state row as "your verification was updated", because that is not what happened to the person. A caller of `Upsert` still just sets the screen and the change is recorded for it; what it cannot do any more is change the screen without saying who decided and why, which `Change` already required and nothing enforced.
- **Evidence:** `migrations/00796_a_sanctions_screen_is_a_decision_not_an_attribute.sql`; `internal/compliance/profile.go` (`Upsert`, `screen`); `TestAudit_SanctionsStateIsWritableWithNoTransitionRow`, `TestIntegration_Compliance_ASanctionsScreenIsRecordedAsADecision`, `TestIntegration_Compliance_AProfileBornScreenedRecordsIt`.

## D-099 — What the API process runs on a timer, and what a process-local limiter does when it runs out of counters (2026-09-10, product goal §20, §54, F-169, F-170)

- **Problem:** two more passes that existed and ran nowhere, and one bound that was documented and enforced by nothing. `gates.Admin.ExpireDue`, `admin.Service.ExpireDue` and `capital.Service.ExpireDue` were written, tested and called by no binary; nothing expired a verification SESSION at all; and `ratelimit.MemoryStore` promised "bounded memory: expired windows are dropped lazily on access and by Sweep" while nothing anywhere called `Sweep` and the lazy drop only ever helped a key that came back.
- **Chosen:** one expiry ticker in `cmd/api` at the five-minute cadence the other passes use, each sweep in its own transaction and its own error path, the whole pass under a timeout, a nil service skipped rather than panicked on; and a sweeper goroutine for the memory store on a ticker at the largest configured window, living exactly as long as the store does.
- **Why here:** the answer D-046, F-118, D-069, D-084 and D-085 have already given. This deployment runs one web service and no workers (`render.yaml`), so the alternative to running periodic work in the API process is not running it somewhere better, it is not running it at all.
- **Why the verification session sweep is the one that mattered:** the other three only make a row agree with what every reader of it already believes — the gate checker treats a closed window as inactive, `admin.checkPending` refuses an expired action, a reservation past its window is not buying power. Migration 00762 permits exactly ONE open verification session per person, so a hosted link that ran out and that nothing closes is that person's verification blocked for good. §20 says "your link expired, start again" and the product could not honour it.
- **What that sweep deliberately does not do:** it does not touch the profile. A session that ran out decided nothing, and `applyToProfile` already refuses to let an abandoned attempt undo a standing verification; moving the profile on a timer would be a clock deciding something about a person, which is what D-061 was careful not to do. It also closes a session still in CREATED that the provider was never told about after `UnstartedSessionGrace` (fifteen minutes): that row is written BEFORE the provider call so a crash leaves something to reconcile, `Resume` refuses it, no answer can ever arrive for it, and it holds its owner's only session shut.
- **What a saturated rate-limit window does:** when one window holds more distinct keys than `DefaultMaxKeys`, every counter is dropped and the rest of that window is counted against ONE shared budget, reported as a WARN. It is a real degradation — callers share a budget for up to one window, which is F-88 in miniature — and it is the least bad of three. Evicting the oldest entries keeps the limiter exact for whoever remains and hands an attacker a way to evict the counter watching them; admitting new keys without counting them removes the limit at the moment it is being tested; a shared budget still refuses a flood, recovers by itself, and cannot grow. The backstop is that `config.Validate` refuses this backend outside a single-process deployment, and that since F-166 a caller can no longer choose its own key.
- **Consequences:** four indexed reads every five minutes that find nothing on a launch tier, and one map sweep per window. `gates` and `admin` take no batch bound and are not this branch's packages to change; their row counts are one environment's gates and the actions still pending, and the pass timeout bounds them. An operator reading "the rate limiter ran out of counters and shared one budget for a window" has found a deployment whose caller cardinality has passed what a process-local limiter can count exactly.
- **Evidence:** `cmd/api/expiresweeps.go`, `cmd/api/ratelimitstore.go`, `internal/verification/expiry.go`, `internal/ratelimit/ratelimit.go`; `TestIntegration_TheExpirySweepsRunInThisProcess`, `TestIntegration_TheSessionSweepClosesWhatCannotBeDecided`, `TestIntegration_TheSessionSweepLeavesADecidedSessionAlone`, `TestAudit_TheMemoryRateLimitStoreIsNeverSwept`, `TestMemoryStore_StopsGrowingAtItsCap`, `TestRateLimitSweeper_RemovesExpiredCountersAndStops`.
## D-093 — A pricing policy prices the scale of the asset it sells (2026-09-10, product goal §54, F-151)

- **Problem:** `PricingPolicy.creditsFor` computed `minor × CreditsPerMajorUnit / MinorUnitsPerMajorUnit` and wrote the result into `credit_fundings.credit_quantity`, the OpenAPI `Quantity`, and a ledger entry — all three of which mean asset BASE UNITS — while `CreditsPerMajorUnit` is a count of whole CREDITS. The CREDIT asset has six decimals, so $10.00 at the shipped rate of 100 Credits per dollar bought 1,000 base units: 0.001 Credits. Every row agreed with every other row, `VerifyProvenance` reconciled, and the Buy Credits page rendered "100 Credits per 1 USD" over "0.001000 Credits" without either half being able to tell the other was wrong. The defect was in the unit, not in the arithmetic between the rows, which is why nothing in the system could see it.
- **Chosen:** `PricingPolicy` gains `Decimals`, hashed with every other field so a scale change is a new version and a funding recorded under the old one stays explicable. `creditsFor` multiplies by `10^Decimals` — the same conversion `internal/payout`'s `moneyToCredits` already performed in the other direction. `NewPurchaseService` reads the registered CREDIT asset's decimals and refuses to build a service whose policy prices a different scale, which is why it now takes a querier; `cmd/api` registers the sandbox tier's Credit asset above the purchase path rather than below it, so there is something to compare against.
- **Why the constructor and not `Validate`:** `Validate` asks whether a policy is internally coherent and never sees a deployment. "Is this the scale of the asset this deployment registered" cannot be answered without one, and it is the only question that would have caught this: a policy that prices a six-decimal Credit is perfectly coherent on a deployment whose Credit has eight.
- **Why the whole conversion is published:** `GET /v1/credits/pricing` returned the rate alone, so a page holding it could not compute what a customer was about to be charged for and had to assume the rest. It now returns `decimals`, `minor_units_per_major_unit` and `rounding`, and `GET /v1/credits/balance` carries `credit_decimals` beside its figures, so the browser reads the scale instead of hardcoding six in four files.
- **Consequences:** the shipped policy issues 10^6 times what it did, which is the point; no deployment has ever sold a Credit (`CREDIT_PURCHASE` is ACTIVE nowhere), so there are no fundings recorded under the old arithmetic to reconcile. A deployment that registers a Credit at another scale now has its purchase path disabled with a reason rather than silently mispricing, and the remaining hardcoded scales in the browser are the responses that still state none — a market summary and an agent budget — named in `apps/web/src/lib/credits.ts`.
- **Evidence:** `internal/credit/pricing.go`, `purchase.go` (`NewPurchaseService`), `service.go` (`AssetDecimals`); `TestPricingPolicy_DefaultIsValidAndPricesTheObviousCase`, `TestPricingPolicy_TheScaleIsTheAssetsAndNotAConstant`, `TestAudit_PricingPolicyIssuesBaseUnitsAsIfACreditHadNoDecimals`; `TestIntegration_APurchaseServiceRefusesAPolicyAtTheWrongScale`, `TestIntegration_APurchaseMintsWhatTheFundingPagePromised`; `apps/web/src/lib/credits.ts` (`creditScale`).

## D-094 — A dispute that closes in our favour returns the funding to its window; only the clock settles (2026-09-10, product goal §54, F-155)

- **Problem:** `charge.dispute.closed` with status `warning_closed` — the close of an early-fraud-warning INQUIRY, which Stripe is explicit is not a dispute and which a chargeback may still follow — mapped to `DISPUTE_WON`, which mapped to `SETTLED`. `SettleFunding` then promoted the lot REVERSIBLE → SETTLED, which is what `FundingFinality.PayoutEligible()` reads. A card payment minutes old became withdrawable on a sandbox tier and stopped counting against the money-at-risk ceiling, with its reversibility window untouched.
- **Chosen:** `warning_closed` becomes its own provider status, `PurchaseDisputeLifted`, and BOTH it and `PurchaseDisputeWon` map to `FundingReversible`. `DISPUTED → REVERSIBLE` joins the funding transition table; `apply` gains a case that advances the funding AND unfreezes the lot, the mirror of `DisputeFunding`. `SettleDue` remains the only thing that writes SETTLED.
- **Why DISPUTE_WON moved too, and not only the inquiry:** `SETTLED` does not mean "we are confident the money is ours"; it means the reversibility window has CLOSED, which is a fact about a clock and a policy. A card can be disputed more than once, and the scheme rules that admitted the first dispute still apply the day after the second is won. The package already refuses to let an operator assert SETTLED — "an operator who could assert it by hand could make value payout-eligible by closing a ticket" — and a card network closing a dispute is not more entitled to that than an operator is.
- **Why the window does not restart:** 00743's trigger writes `reversible_at` as `coalesce(reversible_at, occurred_at)`, so re-entering REVERSIBLE keeps the original stamp. A funding that spent three weeks disputed returns to a window with three weeks already elapsed, which is the honest answer: the money was as reversible during the dispute as before it.
- **Why two statuses for one destination:** a dispute won and an inquiry closed are different provider facts. They reach the same funding state today, and a deployment reading its `credit_funding_transitions` rows can still tell which happened, because the raw status is recorded verbatim.
- **Consequences:** `DISPUTED → SETTLED` stays legal in the transition table so an operator resolution, and a provider that reports settlement after a dispute, remain representable; nothing in this binary takes that edge on its own any more. Purchased value now reaches payout eligibility through exactly one door.
- **Evidence:** `internal/credit/purchaseprovider.go` (`PurchaseDisputeLifted`, `FundingStateFor`), `funding.go` (`fundingTransitions`, `UnfreezeFunding`), `purchase.go` (`apply`); `internal/provider/stripecredit/webhook.go` (`disputeStatus`); `TestAudit_AnInquiryThatClosesUnfreezesTheFundingAndDoesNotSettleIt`, `TestAudit_AWonDisputeReturnsTheFundingToItsWindowRatherThanSettlingIt`.

## D-095 — A competition prize is a grant, and a grant never leaves (2026-09-10, product goal §54, F-157)

- **Problem:** `valuedomain.SandboxPolicy` permitted seven origins where `docs/product/CREDIT_ECONOMY.md` §4 lists six — `PURCHASED` and the five earning origins. The seventh was `COMPETITION_REWARD`: "a prize or reward from a platform competition", not `EarnedByUser()`, nobody paid for it and nobody earned it. The section's own conclusion is the rule it broke: "A granted Credit that could leave the system would be the first rule somebody copied."
- **Chosen:** `COMPETITION_REWARD` is closed, beside `PROMOTIONAL`, `REFUND`, `ADMIN_ADJUSTMENT` and `PROVIDER_SETTLEMENT`.
- **Why closed rather than documented as permitted:** a platform that mints its own prizes and lets them out has a payout path whose only gate is a competition it runs itself. The document's six is the defensible list and the policy is the thing that was wrong. `SandboxPolicy` is explicitly not a product or legal decision about real payouts — the real one is a new version through the approval path — so the shape it rehearses should be the conservative one.
- **Consequences:** nothing in this build issues a `COMPETITION_REWARD` lot, so no held value changes. `internal/eligibility`'s per-origin explanation reports `ORIGIN_NOT_WITHDRAWABLE` for it, which is the sentence a prize winner would be shown.
- **Why the test changed shape:** `TestSandboxPolicy` asserted three permitted origins and four closed ones out of eleven, so the other four could be anything. It now asserts over every origin, which is how this would have been caught the day it was written.
- **Evidence:** `internal/valuedomain/sandboxpolicy.go`; `TestSandboxPolicy`, `TestAudit_SandboxPolicyPermitsOnlyTheOriginsTheDocumentLists`, `TestExplainWithdrawal_TheSandboxPolicyPerOrigin`.

## D-096 — A purchase records the provider mode that opened it (2026-09-10, product goal §54, §12, F-158)

- **Problem:** `CreditPurchase.sandbox` was rendered from `httpapi.Options.CreditPurchaseSandbox`, a boolean the composition root computed once at startup from `cfg.Providers.CreditPurchase.Mode`. Every funding the API returned carried it, including ones opened months earlier under a different mode. It answered "what mode is this deployment in now" to a question that asks "was this payment real", and the direction that costs is the cheap one: a deployment promoted from sandbox to live re-labels every sandbox purchase it ever made as real value, in the API and in the browser's temperature badge.
- **Chosen:** migration 00793 adds `credit_fundings.provider_mode`, written once by `CreateFunding` from the mode of the provider that opened the payment. `CreateFundingRequest.Validate` refuses a request without one, so nothing in this binary can write a funding whose mode is unrecorded. The API renders `sandbox` from the row and publishes the recorded mode beside it.
- **Why write-once by privilege rather than by convention:** 00743 already revoked UPDATE on `credit_fundings` from `cp_app` and granted back only `lot_id` and `provider_reference`, so the new column is unreachable by any statement the application can issue. No trigger and no extra rule were needed.
- **Why the column is nullable and is not backfilled:** a funding opened before it existed did not record the fact, and any value written now would be derived from today's configuration — which is exactly the thing that must stop deciding what an old payment was. NULL means "this funding predates the record", and the API renders it as SANDBOX: an unrecorded mode cannot be asserted to be real money, and over-labelling value as simulated is the safe direction of that mistake.
- **What the recorded mode buys beyond the label:** `Dispatch` now compares it against the provider's own `livemode` on every event and parks a disagreement for a person. The adapter already refuses an event whose livemode disagrees with the ADAPTER's mode; this catches the other case — a deployment whose mode changed between opening a payment and hearing about it — which had nothing to compare against while the flag was recomputed on every read.
- **Consequences:** `httpapi.Options.CreditPurchaseSandbox` is gone, so there is no deployment-wide answer left to stamp on a past payment. `test/integration/enums` pairs the CHECK with `credit.AllProviderModes()`, which is built from `internal/config`'s constants so the two lists cannot drift.
- **Evidence:** `migrations/00793_a_funding_records_the_mode_that_opened_its_payment.sql`; `internal/credit/funding.go` (`AllProviderModes`, `SandboxMode`), `purchase.go` (`providerModeMismatch`); `internal/httpapi/handlers_credits.go` (`toAPICreditPurchase`); `TestCreditPurchaseCarriesItsTemperature`, `TestCreateFundingRequest_Validate`.
## D-100 — The operator directory gets a transition table, and a revocation is one-way whoever writes it (2026-09-10, accounts-auth audit F-175, ADR-0024)

- **Problem:** `operator_roles` is the only source of operator authority in this system and it carried migration 00010's blanket `GRANT SELECT, INSERT, UPDATE`. Nothing in Go had ever updated it, so the grant served nothing, and with it a revocation did not stay revoked, a `SUPPORT_READ_ONLY` row could become `ADMIN` in place with its provenance columns unchanged, and nothing recorded that either had happened. It is the one authority-bearing table that never got the treatment 00744 gave `accounts`, 00757 gave `users` and 00758 gave `account_closure_requests`.
- **Chosen:** the F-42 shape, adapted to a table whose "state" is two nullable timestamps rather than one text column. `operator_role_transitions` is append-only, names the grant by the directory's own primary key, and carries the actor, the reason and the correlation id; `cp_operator_role_apply_transition` is SECURITY DEFINER and writes `revoked_at` / `expires_at` from the row; `cp_operator_role_provenance_is_immutable` refuses any change to who was granted what, by whom, when and why, refuses un-revoking and refuses moving a revoked grant's expiry. `cp_app` loses UPDATE and keeps `UPDATE (reason)` only, for the row lock.
- **Why derived rather than chosen:** every other authority-bearing table in this schema already answers "how does this column move" the same way, and ADR-0024 §3 already promised that a revoked grant stays revoked. The promise was made about the bootstrap INSERT while the column stayed writable; this is the promise being true rather than a new position.
- **Why the invariant is a trigger and not only a grant:** a grant answers "which role may write this", and the answer would have to be re-derived every time a role is added. `revoked_at` being one-way is a property of the fact, not of the writer, so it holds against the migration role too. Restoring authority means INSERTing a new grant, which carries its own `granted_by` and its own reason — which is the point.
- **Why `UPDATE (reason)` and not nothing:** PostgreSQL requires UPDATE privilege on at least one column for `SELECT ... FOR UPDATE` (00744 discovered this the hard way), and `reason` is a column the immutability trigger refuses to let anybody change. The grant buys a row lock and provably nothing else, which `TestIntegration_OperatorDirectoryAuthority` asserts in both directions.
- **Consequences:** there is still no HTTP route that writes the directory, and ADR-0024 §7 is still true. This builds the mechanism a route would use, so the route cannot be built as a bare UPDATE when it arrives. Until then a deployment revokes with the migration credential by inserting a transition row, which is one statement and leaves the record behind it. `test/security`'s `grantOperatorRole` helper had to stop using `ON CONFLICT DO UPDATE SET revoked_at = NULL` — which is exactly the thing this refuses — and asserts the grant is live instead of asserting a row count.
- **Evidence:** `migrations/00799_a_revoked_operator_role_stays_revoked.sql`; `internal/operatorroles` (`AllTransitionActions`); `TestAudit_TheOperatorDirectoryIsRewritableByTheApplicationRole`, `TestIntegration_OperatorDirectoryAuthority`, `TestIntegration_Bootstrap_DoesNotRestoreARevokedGrant`; `test/integration/enums` (`operator_role_transitions_action_check`).

## D-101 — A step-up rotates the session it raises; a cold step-up issues (2026-09-10, accounts-auth audit F-177, PART 192)

- **Problem:** PART 192 requires rotation on privilege change and `auth.Manager.Rotate` had no caller. A step-up login is a privilege change — it raises `AuthTime` and `AMR`, and it is what a person is asked for before closing their account or registering a payout destination — and `identity.Complete` always called `Issue`, so the pre-step-up session stayed live with its own full absolute lifetime, carrying the weaker authentication.
- **Chosen:** the callback reads the session the browser already holds (`httpmw.SessionFrom` — the callback is a public route and the session middleware attaches whatever the cookie resolved to) and passes it into `identity.Complete` as `CompleteRequest.Current`. `Complete` rotates when the replacement is the same login moving forward: the same subject, the same actor type, not an agent, and a session the store still accepts inside the transaction.
- **Why a session and not a subject id:** `Rotate` re-reads the session from the store inside the transaction and refuses a stale copy. A caller that could only name a subject could ask for somebody else's sessions to be replaced, and this route is reachable by anybody with a valid login attempt.
- **Why a cold step-up still issues:** there is nothing to rotate from, and refusing would break the step-up a signed-out person performs on their way into the product. The same branch covers a session the store no longer accepts — revoked elsewhere, expired, or idled out between the redirect and the callback — because that browser holds no usable credential either, so treating it as cold leaves nothing live.
- **Why an actor-type change issues and revokes rather than rotating:** `Rotate` keeps the current session's actor type, and a change means the directory decided something different about this person between the two logins. Carrying an OPERATOR actor type onto a principal the directory no longer names would make every audit row about them wrong; issuing a new session and revoking the old one is the same guarantee with the right label.
- **Why break-glass survives:** the elevation is bounded by the clock rather than by the session, and ending an emergency because somebody re-authenticated more strongly would be the wrong way round. Both halves move together, because `security.Principal` refuses the role without the expiry.
- **Consequences:** one browser that steps up reports one session on `GET /v1/me/security` rather than two devices. The replacement keeps the replaced session's absolute `ExpiresAt`, so rotation cannot extend a login. The revocation is recorded as its own `session_revoked` security event with `by: step_up_rotation`, because a revocation the user did not ask for belongs on their security page beside the ones they did. `httpmw.WithSession` is exported so a test harness standing in for the middleware can put a session where the handlers look for one.
- **Evidence:** `internal/identity/login.go` (`issueOrRotate`), `internal/httpapi/handlers_auth.go`; `TestAudit_StepUpLoginLeavesThePreviousSessionLiveAndUsable`, `TestAudit_AColdStepUpIssuesAndAStrangersSessionIsNotRotated`, `TestAudit_TheCallbackHandsTheCurrentSessionToTheLoginService`.

## D-102 — A closure decision reads three financial facts, and refuses naming the one that blocks it (2026-09-10, accounts-auth audit F-179, product goal §38)

- **Problem:** EFFECT is the one irreversible operator action on the support surface, and `profile.Decide` did it after checking a PENDING request, a distinct principal and the cooling-off period. Migration 00758 and `internal/profile/closure.go` both say REFUSED exists for "an unsettled payout, an open dispute, or a balance to deal with first"; none of the three was enforced anywhere or reported anywhere, so the operator could not have consulted them either.
- **Chosen:** three facts, one statement, one read. A non-zero gross Credit balance, a payout request not in a terminal state, and an open native position, summed across every account the person owns. `Decide(EFFECT)` refuses with `INVALID_STATE_TRANSITION` naming the blocker; `AdminUserView.Blockers` and `AdminUserView.closure_blockers` carry the same read; the admin console states all three above the form and stops offering EFFECT while any of them stands.
- **Why derived rather than chosen:** the three are the three the schema and the domain already name as the reason REFUSED exists. Choosing a different set would have been inventing a policy; this is enforcing the one already written down.
- **Why one statement and not three:** the operator is shown one answer and the decision is refused on one answer. Three round trips could report a balance from before a payout reserved against it, which is the shape of a support view that disagrees with the service that reads it.
- **Why gross and not spendable:** a disputed or frozen lot is still value that belongs to the person whose account this is, and closing over it puts it out of their reach exactly as a spendable balance would.
- **Why the refusal names the blocker:** the operator has to be able to tell the person what to do about it, and REFUSE is the decision that carries that reason to them. A blocker means "not like this", never "no": the person clears it and asks again, and a new request restarts the cooling-off period, which is D-055's rule unchanged.
- **Why the terminal payout states are copied into `internal/profile`:** importing `internal/payout` would pull the ledger, the capability gates and the configuration into a package that needs four strings. The copy is held against `payout.State.Terminal()` by a test whose import is in a test file, so it costs the production build nothing and cannot drift.
- **Consequences:** `AdminUserView` gains a required `closure_blockers` object, so every deployment answers it. `GET /v1/admin/users/{userId}` does three more subqueries. The console no longer offers EFFECT it knows will be refused, because offering an option the service refuses teaches an operator that refusals are noise.
- **Evidence:** `internal/profile/closure.go` (`ClosureBlockers`), `repo.go` (`closureBlockersSQL`), `account.go`; `openapi/openapi.yaml`; `apps/admin/src/views/accounts.ts`; `TestAudit_EffectingAClosureChecksNothingFinancial`, `TestAudit_EffectingAClosureIsRefusedWhileCreditsRemain`, `TestAudit_EffectingAClosureIsRefusedWhileAPositionIsOpen`, `TestAudit_TheClosureDecisionSurfaceCarriesNoFinancialFact`, `TestClosureBlockers_TheTerminalPayoutStatesAreThePayoutPackages`.

## D-103 — The login-state cookie stays one slot, and the message stops describing an attack (2026-09-10, accounts-auth audit F-182, F-87)

- **Problem:** `httpmw.SetLoginState` writes one cookie name at `Path=/`, so a second `GET /v1/auth/login` overwrites the first flow's digest. Two sign-in tabs, or a sign-in and then a step-up begun beside it, leave the older tab's callback answering `401 this sign-in did not start in this browser; start again from the beginning` — a sentence that names an attack as the only explanation for something whose common cause is two tabs.
- **Chosen:** keep one slot; change the copy to `this sign-in did not start in this browser, or a newer sign-in replaced it; start again`.
- **Why derived rather than chosen:** the control is F-87's and it is the thing that works. A per-flow cookie keyed on the state would let both tabs finish, and would also mean the browser holds several live digests at once, each of them a value an attacker who started their own flow already knows. Widening the surface of a control to improve an error message is the wrong trade, and the error message was the part that was actually wrong.
- **Why not silently accept the older flow:** the digest is the whole binding. A callback that did not begin in this browser signs the victim in as the attacker, and there is no way to tell "my other tab" from "somebody else's tab" without keeping the thing this refuses.
- **Consequences:** a person with two sign-in tabs is told what happened in plain words and starts again, which costs one redirect. The refusal, its status and its code are unchanged, so nothing that depends on the control moves. The auditor's reproduction keeps its assertion that the callback is refused and moves its expectation to the copy.
- **Evidence:** `internal/httpapi/handlers_auth.go`; `TestAudit_ASecondLoginInvalidatesTheFirstTabsFlow`.
## D-090 — Entering SANDBOX clears the row rather than declining to write it (2026-09-10, product goal §54, ADR-0023, F-160, F-161)

- **Problem:** migration 00755 made "a SANDBOX gate carries no approval" true by having `cp_gate_sandbox` touch none of the approval columns. That works for the only source anybody tested — a freshly bootstrapped DISABLED row, which has nothing on it. EXPIRED and REVOKED are documented sources too, and a gate sandboxed from either kept the whole approval version that reached ACTIVE: three chain entries, the proposer, the approval version, four evidence references, and, through `revoked_at`, a column that made the resulting row permanently inactive while every surface reported it as sandbox-activated. The admin API returned all of it beside `state: SANDBOX, active: true`, under a console banner stating there is none.
- **Chosen:** the function's UPDATE blanks them — `approval_version` 0, `approvers` and `evidence_hashes` `'[]'`, `proposed_by_user_id` and the four `*_ref` columns NULL, `revoked_at` and `revoke_reason` NULL — in the same statement as the state change, and migration 00791 backfills the rows the old function wrote. The sandbox transition attests the digest of a gate with no evidence, so the transition, the audit event and the row agree.
- **Why clearing and not refusing the transition:** refusing EXPIRED → SANDBOX would make the state unreachable for exactly the capability a rehearsal is most likely to need back, and REVOKED → SANDBOX is the tier's only way to recover a capability it pulled during an incident without running the dual-control ceremony it exists to avoid fabricating. The documents were right about what a SANDBOX row should be; the function was the thing that did not match them.
- **Why this does not destroy an audit trail:** the row carries the CURRENT approval version, and a sandbox row's current approval version is none. Every transition of the cleared version, with the evidence digest each principal attested, is in `capability_gate_transitions`, which no application role may write or delete; `propose` has always replaced the chain wholesale for the same reason.
- **Why `evaluateSandbox` stops reading `revoked_at`:** a revoke is a state. Revoking a gate moves it to REVOKED, where the state check refuses it on every deployment; the column on a SANDBOX row is residue of an earlier life, and reading it as a refusal made a documented, reported, logged activation inert with no way back.
- **Consequences:** an operator who sandboxes a capability that was ACTIVE last week sees a clean row, and the real ceremony for it starts from DISABLED with fresh evidence, as it always did. The console's "none, and none is expected" is now true of every SANDBOX row rather than of one of three.
- **Evidence:** `migrations/00791_a_sandbox_row_carries_no_approval_and_no_revoke.sql`; `internal/gates/sandbox.go` (`sandboxEvidenceDigest`, `evaluateSandbox`), `sandbox_bootstrap.go`; `TestIntegration_SandboxGate`, `TestIntegration_SandboxFromAFinishedCeremonyCarriesNoneOfIt`, `TestIntegration_AUDIT_SandboxFromRevokedKeepsTheRevoke`, `TestIntegration_AUDIT_SandboxFromExpiredCarriesTheApprovalItClaimsNotToHave`, `TestEvaluateWith_SandboxRowIsActiveOnlyOnASandboxTier`; ADR-0023 §1.

## D-091 — A gate in a real ceremony is skipped at boot, loudly, and does not stop the deployment (2026-09-10, product goal §54, ADR-0023, F-162)

- **Problem:** `CP_API_SANDBOX_GATES` names the capabilities a sandbox tier activates at boot. A gate in a state that is not a legal source of SANDBOX was an error, `cmd/api` returned it from wire, and the process exited. So an operator who rehearsed the dual-control ceremony — the thing a sandbox tier is for — on any listed capability could not restart the deployment until somebody edited the blueprint, and the only in-machine escape was a revoke, which before D-090 produced a permanently inert gate.
- **Chosen:** the gate is still never moved. `gates.SandboxAtBoot` returns it as skipped, `cmd/api` logs a WARN naming the capability, its state and what to do about it, and the rest of the list is activated.
- **Why not keep the fatal error:** the control is "a configuration line does not move an approval's state", and the skip enforces it exactly as the error did. Refusing to boot was a second control nobody chose, protecting nothing the first did not already protect, and its cost is a deployment that cannot restart during an incident — the state in which an operator most needs it to.
- **Why not sandbox it anyway:** that is the forbidden thing. A blueprint line would be overwriting a real proposal or approval, which is the one outcome this whole path exists to prevent.
- **What still refuses:** `CP_API_SANDBOX_GATES` set on a deployment that is not a sandbox tier is still a startup error, because that is a configuration the binary must not run under at all; the operator-driven `Admin.Sandbox` still refuses PENDING_APPROVAL → SANDBOX as an illegal transition, in the transition table and in the database.
- **Consequences:** a skipped capability is governed by its ceremony on that deployment and not by the blueprint, which is what the operator asked for by proposing it; the WARN says so in those words, so the state is visible rather than silently different from the blueprint.
- **Evidence:** `internal/gates/sandbox_bootstrap.go` (`SandboxAtBoot`, `SkippedGate`); `cmd/api/sandboxtier.go`; `TestIntegration_BootstrapSandboxIsIdempotentAndNeverMovesAnApproval`, `TestIntegration_AUDIT_ARealCeremonyOnASandboxCapabilityBlocksTheNextBoot`; ADR-0023 §2.

## D-092 — The conversion-request path declares WITHDRAW and checks it where it acts (2026-09-10, product goal §54, PARTS 52-53, F-163)

- **Problem:** POLICY_AUTHORITY §2 says the `WITHDRAW` class is blocked by `WITHDRAWALS_DISABLE`, `ACCOUNT_FREEZE` and `GLOBAL_NEW_RISK_KILL`. `internal/payout` imported `internal/killswitch` nowhere, so none of the three stopped a conversion request, a reservation or a submission — on the one withdrawal surface a deployment can actually reach, because only `legalrouter.SandboxPolicy` permits `PAYOUT` at all. The account's own status was not read either.
- **Chosen:** `payout.Service` takes a `KillSwitchChecker` and an account reader, both required at construction, and `Create`, `CompleteVerification` and `Submit` call `Check` and read `accounts.Status` inside the transaction that authorizes them — switches first, then status, the order and the shape `internal/withdrawal` already uses at the same boundary.
- **Why required and not optional:** a guard a caller may leave nil is a guard that is eventually left nil, and the failure is silent in the permissive direction. The constructor panics, at wiring time, in every deployment and every test.
- **Where each call sits, and why:** `Create` checks before the quote is consumed, because burning somebody's quote on the way to refusing them costs them something while stopping nothing. `CompleteVerification` checks because days can pass between the request and the verification and it is the call that reserves the value. `Submit` checks inside the claim transaction, which is the last moment before a provider is called under a committed idempotency key and only reconciliation can answer what happened; the request stays VERIFIED with the value reserved, and the next sweep submits it once the switch is released.
- **Why the boundary pre-check is not the authority:** `compileRoute` pre-checks the compiled action's class against an in-process snapshot of at most one second, which POLICY_AUTHORITY §2 permits for pre-checks only. It buys an early refusal that names the switch instead of a downstream symptom. The authoritative check is the one in the domain transaction, which sees every activation committed before it.
- **Why cancellation consults nothing:** `CANCEL` is never blocked, by the same section. A switch that stopped a cancellation would hold a user's reserved Credits in `PAYOUT_RESERVED` for the length of an incident, which is the trap the cancel endpoint was written to close. It declares its class in words instead.
- **Why RESTRICTED and CLOSED refuse as well as FROZEN:** `withdrawal.Service.Request` requires ACTIVE at the same boundary, and two withdrawal surfaces disagreeing about which account statuses may take value out is how one of them becomes the way around the other. FROZEN answers `ACCOUNT_FROZEN`; the rest answer `FORBIDDEN` naming the status.
- **Consequences:** an operator's `WITHDRAWALS_DISABLE` now stops the product's only working withdrawal path, in flight, without touching a reservation that already exists. `POLICY_AUTHORITY.md` §2 names which code declares which class, so the table describes the system rather than an intention.
- **Evidence:** `internal/payout/service.go` (`guardWithdraw`), `doc.go`; `internal/httpapi/wiring_compiler.go` (`killSwitchClassOf`, `preCheckKillSwitches`), `wiring_native.go`, `handlers_payout_cancel.go`; `cmd/api/wire.go`; `TestIntegration_EveryWithdrawSwitchStopsAConversionRequest`, `TestIntegration_AFrozenAccountCannotConvert`, `TestIntegration_AKillSwitchStopsASubmissionMidFlight`, `TestAUDIT_NoKillSwitchCanReachTheConversionRequestPath`; `docs/architecture/POLICY_AUTHORITY.md` §2.

## D-118 — A signal for a change that names nobody is published once, on the pass that first passes the row (2026-09-10, product goal §36, §54, F-167, F-190)

- **Problem:** two fixes to `Follower.runSource` met at the merge and disagreed about one line. `fix/platform` (F-167) rewrote the pass as a drain (`ahead`, rows past the cursor) plus a lap re-read (`behind`) and published a change's `data.changed` signals when the pass wrote a notification for it OR when the change carried no notifications at all — the second half so that a broadcast signal with no recipients is still announced. `fix/agnot` (F-190) published them only when a notification was written, and in the same branch made `marketParticipants` exclude everyone a pause has already told (D-107). Together, the platform rule re-broadcasts every market pause on every lap pass: once all participants are told, the re-read change names nobody, and "names nobody" was the platform's condition for announcing it.
- **Chosen:** `fresh > 0 || (len(c.Notify) == 0 && newRow)`, where `newRow` means the change came from the drain (it is past the cursor, so this pass is its first). A change that told somebody is signalled with that telling; a change that names nobody is signalled once, when the cursor first passes it; from the lap re-read a change that wrote nothing is silent.
- **Why not the platform rule alone:** it is F-190's shape for exactly the broadcast case D-107 created — a pause on a popular market would invalidate every connected client's market queries eight times per pause at a fifteen-second tick, which is the defect F-190 fixed for the per-user case.
- **Why not the agnot rule alone:** it never signals a change that names nobody, so a paused market with no participants would not invalidate the public list, and the agnot entry (D-107) recorded that as the price. The drain/lap split the platform introduced makes the price unnecessary: whether a row is being seen for the first time is now known.
- **What is still not covered:** a change that names nobody AND committed late, behind the cursor (found only by the lap), is never signalled — the lap cannot distinguish a late commit from a re-read without a notification row to ask. A late-committed pause of a market nobody holds is the only case, and the next fill on that market carries its own signal.
- **Evidence:** `internal/notifications/follower.go` (`runSource`); `TestAuditAgnot_TheLapRepublishesTheSameSignalsEveryPass`, `TestAudit_FollowerStallsForeverAfterABatchSizedBurst`, `TestIntegration_TheFollowerNotifiesOnceForACapturedPurchase`; F-167, F-190, D-107.

## D-128 — Accepting a compiled strategy is a route of its own, owner-only, and it echoes the hash that was read (2026-09-11, product goal §18, ADR-0029, F-255)

- **Problem:** `agents.Service.Create` refuses an agent whose strategy version is not `ACCEPTED` with `accepted_by_user_id` and `accepted_at` set (F-187, D-105), migration 00500 pairs the status and the columns in a CHECK, and `strategies.go`'s own comment forbids a compiler from returning `ACCEPTED` because "a backend that could return it would be approving on the user's behalf, which is exactly what goal §18's review step exists to stop". Every one of those controls was in place and nothing anywhere could write the row: no route, no service method, no SQL. An agent was unreachable on every deployment of this build, with or without a compiler.
- **Chosen:** `POST /v1/strategies/{strategyId}/versions/{version}/accept`, owner-only (`RequireAccountOwner`; a stranger gets `NOT_FOUND`, F-41's rule), step-up at the boundary, `Mutating`, idempotent, `COMPILED`-only, with a body that must echo the version's `ir_hash`. It writes the two columns and the status through `strategy_versions`' existing UPDATE path, records `strategy.version.accepted` on the owner's account stream, and answers the version.
- **Why the version NUMBER in the path and not the version id:** `strategy_versions` is `UNIQUE (strategy_id, version)`, so the pair is a complete key, and the number is the thing the review screen shows a person. A request built from what is on the screen names what is on the screen. The id would have been equally unambiguous and less legible in a URL a human reads in a log.
- **Why the `ir_hash` echo is required rather than optional:** without it, "accept version 2" means "accept whatever version 2 is when this request arrives". Versions are immutable, so the hash cannot drift under a caller who read THIS version; what the echo catches is a caller who read a DIFFERENT one, which is exactly what a second compile landing between the reading and the pressing produces. A mismatch is `CONFLICT`, not `VALIDATION_FAILED`: the request is well formed and the caller did nothing wrong — what they read is not what is here.
- **Why the step-up is at the boundary here and in the domain for `enable`:** the five lifecycle actions share one operation id and pausing must stay fast, so a boundary step-up would argue with an operator during an incident (POLICY_AUTHORITY §2's reasoning for a kill switch). Acceptance has no emergency twin. It is a single-purpose route whose entire content is a person saying "I read this and I approve it", and it is the gate every later grant of authority rests on: a hijacked session that can accept a strategy can create an agent from it.
- **Why a second acceptance is a replay and not a conflict:** the caller asked for a state the row is already in, by the same person; refusing would make a lost response look like an error. The row already carries the person and the instant, so the answer is the same as the first time and no second audit event is written.
- **Why accepting version N does not supersede N−1:** an agent is bound to a specific version, so two accepted versions of one strategy are two documents two agents may legitimately deploy. Superseding would revoke authority nobody asked to revoke.
- **Residual:** `strategy_versions.status` is a plain text column with a CHECK and `cp_app` holds UPDATE on the table, so acceptance is an application-written state with no transition table behind it — F-42's pattern is not applied. Recorded as F-258 with the exact grant rather than fixed here: the column predates the pattern, is referenced by predictions, intents and financial history, and retro-fitting a transition table for it is a migration of its own.
- **Consequences:** goal §18's review step is reachable and is a person's act. `test/security`'s premise is untouched, nothing here constructs a runtime, and an accepted version still grants nothing until an agent is created from it and enabled.
- **Evidence:** `internal/agents/accept.go`, `accept_integration_test.go`; `internal/httpapi/handlers_strategies.go`, `authz.go`; `openapi/openapi.yaml` (`AcceptStrategyVersionRequest`); `cmd/api/compiler_integration_test.go` (`TestIntegration_ScenarioDBackendEndToEnd`); `apps/web/e2e/scenarios/d-agent.spec.ts`.

## D-129 — A sandbox tier compiles a strategy somebody stated field by field, and reads no natural language at all (2026-09-11, product goal §17, §18, §51, ADR-0023, ADR-0029, F-256, F-257)

- **Problem:** ADR-0029 declared the compiler as two seams that must both be present, and the `RefsLoader` half had no implementation anywhere in the tree, so the pair could never be satisfied and "this deployment has no compiler" was true by construction rather than by configuration. Underneath it, `risk.DefaultGlobalPolicyJSON` sets `"allowed_venues": []` and nothing ever listed a venue, so the RISK_COMPAT stage would have refused every strategy naming the only venue a deployment has. Goal §17/§18's whole journey was unreachable everywhere, which meant the review step nobody could reach was also the review step nobody could test.
- **Chosen:** `internal/provider/compilersandbox`, a compiler for a sandbox tier that compiles only a fully DECLARED strategy — `strategies.constraints`, given its own column by 00813 — and never the description. `cmd/api/compiler.go` is the `RefsLoader` the ADR described, reading instruments, venues, tools and the composed GLOBAL policy at the instant of each compile. Both are wired only when `cfg.SandboxTier()`.
- **The grammar, exactly:** `schema_version: 1`; `universe: {instrument, venue}` (a canonical instrument name the registry holds and ACTIVE, a venue code the registry holds, able to take new actions, and listing that instrument); `entry` and `exit`, each `{kind: PRICE_THRESHOLD, comparator: LT|LTE|GT|GTE, price_usd}` or `{kind: EVERY_INTERVAL}`; `risk_limits: {max_single_trade_usd, max_position_usd, max_daily_loss_usd}`; `capital_limit: {min_allocation_usd}`; `frequency: {interval_minutes 1..10080, max_intents_per_hour 1..3600}`; `mode: PAPER`. Every amount is an exact USD MINOR-unit digit string. Unknown fields are refused rather than ignored.
- **Why no default for anything:** a default is an inference about what somebody meant, which is the one thing this compiler exists not to do. An absent or incomplete document compiles to a refusal naming EVERY field it needed (`STRUCTURED_CONSTRAINTS_REQUIRED`, `stage_reached PROMPT`, no version) — the same honest shape `COMPILER_UNAVAILABLE` has, and told once rather than one refusal at a time.
- **Why the constraints moved out of the description (00813):** they were concatenated onto it, which made "what you wrote" untrue on every screen that shows a description back, and left a compiler no way to claim it had not read the prose — it would have had to find the constraints by parsing them out of it.
- **Why the prediction claims nothing:** the IR requires a committed prediction before a trade intent (PART 72) and the caller stated no forecast. The emitted prediction is FLAT with probability zero, expected return zero, a downside probability of one and a maximum downside of the whole position — the values that cannot flatter the strategy. A zero downside would have been the comfortable choice and would have been a claim.
- **Why the execution constraints come from the risk policy:** slippage, fee, price-impact and quote-freshness bounds are not in §18's list and the caller does not state them. They are copied from the GLOBAL policy in force — a declared source, recorded and readable — rather than invented, and the rationale says so in those words beside the fields that did come from the caller.
- **Why PROD is refused three times:** `New` refuses `EnvProd`; the wiring is keyed on `cfg.SandboxTier()`, which is `CP_API_LEGAL_POLICY=SANDBOX`, a value `config.Validate` already refuses in PROD (so no new environment variable was added); and 00812's CHECKs pair `STRUCTURED_SANDBOX` with `sandbox` and refuse `(sandbox, PROD)` in any database. The two new columns are immutable under 00500's guard, so `cp_app` cannot clear the label on a row it wrote.
- **Why the venue allowlist is widened on a sandbox tier and nowhere else (F-257):** an empty allowlist permits no venue, which is the correct fail-closed default and makes the product unrehearsable. `sandboxVenuePolicyAtBoot` records a GLOBAL policy VERSION whose allowlist is the venue codes the registry itself holds, through `risk.Store.RecordPolicy`, append-only, with a SYSTEM actor and a reason that says what it is; every other limit is copied from the policy in force. A production allowlist is a risk-desk decision about where money may go and stays one.
- **Why a second interface rather than widening `CompilerBackend`:** the two read different things. `CompilerBackend` is handed the text a person wrote; the structured one is handed a document they filled in and is never handed the text. Folding them into one would put the description within reach of a compiler whose entire claim is that it cannot see it. `NewStrategyService` refuses a service wired with both.
- **Consequences:** Scenario D is reachable end to end on a sandbox tier, and every version and agent it produces is labelled a rehearsal in the row, in the API and on the page. Off a sandbox tier nothing changes: `Compiler: nil, Refs: nil` stays, and `COMPILER_UNAVAILABLE`'s sentence stays true word for word.
- **Evidence:** migrations 00811, 00812, 00813; `internal/provider/compilersandbox` and its suite; `internal/agents/structured.go`; `cmd/api/compiler.go`, `compiler_integration_test.go`; `internal/strategy/ir.AllLineageSources` paired in `test/integration/enums`; ADR-0029's amendment.

## D-130 — An agent on this tier is a record of granted authority, and "inspect decisions" is an honest empty state everywhere (2026-09-11, product goal §17, §18, ADR-0029, F-65)

- **Problem:** with a compiler and an acceptance route in place, an agent can now exist on a sandbox tier — so the question Scenario D's last step asks ("inspect its decisions") has a subject for the first time, and the answer must not become less honest because there is finally something to answer about.
- **Chosen:** nothing about the runtime changes. `POST /v1/agents` works at levels 0–3 exactly as before; the agent is born DRAFT with no mode and no envelope; `enable` walks DRAFT → COMPILED → VALIDATED → BACKTEST_ELIGIBLE in PAPER mode and stops; `pause`, `resume`, `disable` and `archive` behave as ADR-0029 describes; no Credits move at creation; levels 4–6 stay disabled by policy with the capability each would need. `agentRuntimeDeployment()` still answers `NOT_DEPLOYED` for both components, and the detail page still says the agent is not being evaluated and nothing is scheduled — for an ENABLED agent as much as for a stopped one.
- **So "inspect decisions" shows the empty state on EVERY tier of this build,** sandbox or not, and the reason is a property of the deployment rather than of the account: no evaluator process exists, so there are no runs, so there are no decisions. The API reports zero runs with `BudgetUsedSource = NO_RUNS_RECORDED`, which is a statement that the measurement was made and came out at zero rather than a placeholder.
- **What would change it:** F-65's bridge — a deployed `cmd/agent-worker` with an evaluator wired, and the two kill-switch kinds (`AGENT_PAUSE`, `MODEL_DISABLE`) reaching it. That is deliberately not this wave: `test/security` watches the premise and fails the day the runtime acquires a production caller, and nothing in this wave names those constructors.
- **Why an agent is still worth creating on a tier that evaluates nothing:** the grant is the product. What a person authorised, at which level, with which ceiling, over which universe, on which reviewed document, is a record with legal and financial meaning independent of whether anything acted on it — and it is the record the whole of §17/§18 is about. The interface says which half is real, in words, on the same screen.
- **Consequences:** the agent-detail sweep in `audit-frontend.spec.ts` and `d-agent.spec.ts` has a real subject on a sandbox tier and keeps a precise skip elsewhere (F-251). An agent built on a sandbox-compiled version is labelled a rehearsal at the top of its own page and carries `sandbox: true` in the API, read from its strategy version so the two cannot disagree.
- **Evidence:** `cmd/api/agents.go` (`agentRuntimeDeployment`); `internal/agents/runtime.go`, `store.go` (`View.Sandbox`); `apps/web/src/pages/agents/AgentDetail.tsx`; `cmd/api/compiler_integration_test.go` (`TestIntegration_ScenarioDBackendEndToEnd` walks enable → pause → resume → disable and asserts `NOT_DEPLOYED` after every one); `apps/web/e2e/scenarios/d-agent.spec.ts`; `test/security/deferred_bridge_test.go` still green.

## D-119 — A payout names the quote the customer was shown, and the minimum lives in the domain (2026-09-11, product goal §54, F-224)

**Problem.** `quote_id` was optional on `POST /v1/payouts` and optional in
`payout.CreateRequest`, and the whole minimum-and-fee branch of
`payout.Service.Create` sat inside `if r.QuoteID != nil`. `payout.Engine.Evaluate`
had no minimum and no fee input at all. So 50 Credits against a provider
publishing a $1.00 minimum and a 25c + 25bp fee — which `POST /v1/payouts/quote`
refuses in as many words — reached VERIFIED with the whole gross reserved and
then SETTLED, with the fee never taken (F-224).

**Chosen.** The quote is REQUIRED: `required` in the schema, refused
`VALIDATION_FAILED` naming `quote_id` in the handler, refused by
`CreateRequest.Validate` in the domain. `CreateRequest` carries `ProviderTerms`
— the provider's published fee model and minimum, read through `TermsFrom(caps)`
so no caller assembles them by hand — the way it carries `DisclosureAccepted`,
with no permissive zero value: an unpublished fee model is refused rather than
read as a fee of zero. `Create` refuses `MINIMUM_NOT_MET` (as a `refusal` field
on a VALIDATION_FAILED, alongside the net and the minimum) when the quote's net
is under what the provider publishes TODAY. The request row records the quote's
gross, fee and net and the currency they were in (00808). The reservation is the
GROSS.

**Why derived rather than chosen.** The handler's reason for the option — "an
operator resolving a stuck payout has no quote to name" — was checkable and
false: the route is `accountScopeWrite`, and an operator resolves through
`ResolveManualReview`, which creates nothing. With that gone there is no caller
that legitimately has no quote, and a required field is the only way the domain
can see the fee and the minimum at all. Reserving the gross follows from the fee
coming out of what leaves: reserving the net would leave the fee spendable and
the account short at settlement. Judging the minimum against TODAY'S terms rather
than the number copied onto the quote follows from what a minimum IS — a
statement by the provider about what it will send, now.

**Consequences.** Every payout has a price somebody was shown, and that price is
on the request rather than re-derivable from a schedule that may have moved. A
deployment whose provider publishes no fee model can create no payout, which is
the honest state of a system that cannot say what would reach the customer.
`internal/payout`'s fixtures, `cmd/api`'s sweep fixture and two `internal/httpapi`
fixtures all had to produce a real quote, which is the cost of the API being
honest and is paid once.

**Evidence.** F-224. `internal/payout/payout.go`, `internal/payout/service.go`,
`internal/httpapi/handlers_native.go`, `migrations/00808_*.sql`;
`TestCreateRequest_Validate`,
`TestAuditWV_APayoutBelowTheProviderMinimumIsReservedAndSettledWithoutAQuote`.

## D-120 — The compliance facts the eligibility page refuses on reach the conversion path, from one reader (2026-09-11, product goal §54, F-226)

**Problem.** An open sanctions review, a restriction recorded against the account
and an unsupported jurisdiction stopped `GET /v1/me/eligibility` and stopped
nothing on the conversion path: `payout.EligibilityInput` had a field for none of
them, and `payoutsAdapter.Create` read no compliance column. A person could be
told ACCOUNT_RESTRICTED and 0 withdrawable while `Create` reserved their whole
balance and `Submit` settled it (F-226).

**Chosen.** `payout.EligibilityInput` gains `SanctionsState`,
`AccountRestrictions` and `JurisdictionSupported`; `Engine.Evaluate` adds them to
the absolute blocks beside `ReasonAccountFrozen`, with the reasons
`ACCOUNT_RESTRICTED` and `JURISDICTION_RESTRICTED` — `eligibility.ExplainWithdrawal`'s
own words. One reader answers them, `httpapi.WithdrawalDeps.complianceFacts`, and
the eligibility page, `conversionAdapter.Quote` and `payoutsAdapter.Create` all
call it; the last two inside their own transaction, so the answer belongs to the
snapshot the decision is made in. None of the three has a permissive zero value:
an empty sanctions state is nobody having answered, and nobody having answered is
not a clearance. UNKNOWN is a supplied answer — nobody has screened this person
yet — and is read exactly as the eligibility page reads it.

**Why derived rather than chosen.** The shape is `DisclosureAccepted`'s, already
in this type and already argued for: the caller knows the fact, a second lookup
could disagree with the first, and a decision made in March has to be replayable
in June against the inputs it was made with. The words are the eligibility
engine's because two surfaces answering one question in two vocabularies is how
this defect survived — a person reading ACCOUNT_RESTRICTED on one screen and
nothing on the other cannot tell which is wrong.

**`internal/eligibility`'s own engine has no production caller.** `eligibility.Evaluate`,
`eligibility.Policy` and the whole `Input`/`Decision` machine are unreachable:
`settlement.PlanInput.Eligibility` is filled only by `settlementtest/world.go`.
They are LEFT in place rather than deleted — they are the policy shape §19
describes and what a second consumer would use — and recorded here so the next
person does not assume a caller exists. F-168's entry, which said
`evaluate.go` reads the sanctions screen as one of the allowlists deciding a
payout, is corrected in F-226.

**Consequences.** A deployment with no compliance repository wired establishes
nothing, and nothing established blocks: `JurisdictionSupported` false is
`JURISDICTION_RESTRICTED`. That is the same direction `applyAccountFacts` already
fell in, and it means a person who has never begun verification is refused for
their jurisdiction as well as for their level — which is what the eligibility
page has always said.

**Evidence.** F-226. `internal/payout/eligibility.go`,
`internal/httpapi/wiring_verification.go`, `internal/httpapi/wiring_native.go`;
`TestAuditWV_AnOpenSanctionsReviewStopsAConversionRequest`.

## D-121 — A state machine's edges live in the schema, not only in Go (2026-09-11, product goal §54, F-227)

**Problem.** The edge bindings of 00731 and 00741 make a state change require a
transition row that names the state the entity is really in. Neither asks whether
the edge that row describes is one the state machine HAS. So one INSERT as
`cp_app` moved a compliance profile from UNVERIFIED to VERIFIED — writing a
verified standing, a `verified_at`, an expiry and a CLEAR sanctions screen — and
one more moved a verification session from CREATED to APPROVED for a session the
provider had never been called for (F-227).

**Chosen.** The edge set becomes a table the database reads.
`compliance_profile_state_edges` and `verification_session_status_edges` (00806),
and `payout_request_state_edges` (00807, D-123), are populated by the migration
from the Go transition tables and held identical to them by
`test/integration/enums` — the same pairing `AllStates()` has with the CHECK. Each
apply function refuses an edge that is not in its table with AD001. No role but
`cp_migrate` may write them. Evidence gets the same treatment at a different
level: a `verification_checks` row must name its session's own provider, and the
session must be in a status only a provider ANSWER produces.

**Why derived rather than chosen.** The Go tables stay authoritative because they
are what the code walks, and a second copy that nothing compares is worse than
one copy; the enum-parity suite already exists for exactly this shape of
duplication. Holding the EDGE rather than the destination follows from what the
attack was: the destination was already bound, and the hole was the origin.

**Residuals, recorded rather than hidden.**
1. A session that HAS been answered can still be given further check rows by
   `cp_app`. Closing it is the `capability_gates` treatment — `REVOKE INSERT ON
   verification_checks FROM cp_app` and a SECURITY DEFINER
   `cp_verification_record_check()` behind `Repository.RecordCheck`, one call
   site — and it wants its own migration and its own exploit test, because a
   privilege change to evidence rows is not a line to slip into a wave.
2. A transition row whose endpoints are the same state is left legal, because
   00796's birth screen writes exactly one. Such a row can still carry a
   sanctions screening decision, since the screen has no edge table in either
   language yet.

**Amended 2026-09-11 (second withdrawal-verification audit, F-259, F-265).**
Residual 2 was wider than it said and the edge-table family was one member short.

A same-state row did not only carry "a screening decision": both apply functions
skipped the edge check and then wrote every other column the row held, so a
`VERIFIED -> VERIFIED` compliance row moved `expires_at` -- renewing, for as long
as the writer liked, a validity window a provider decided once and that
`ExpireOverdue` and `Resolver` both read (F-265). 00815 narrows the exemption to
the one row 00796 needs: a same-state row must carry `to_sanctions_state` and
must not carry `verified_at`, `expires_at`, a provider, a provider reference or
a session, and the apply function leaves both timestamps untouched on such a row
rather than trusting the refusal. The residual now reads, exactly: a same-state
row may still change the SANCTIONS SCREEN, which is what 00796's birth trigger
and `compliance.Repository.screen` write and which the screen's own edge flag
(00796) binds; nothing else rides on it. `payout_requests` has no same-state
exemption at all any more, because `transitionWith` returns early and no
legitimate writer produces one.

And the fourth state machine in this area had no edge table: 00763's
`payout_destinations.status` kept the rest of the F-42 treatment and an apply
function that consulted nothing, so one INSERT moved a destination its holder had
disabled back to VERIFIED with a fresh `verified_at` (F-259). 00814 adds
`payout_destination_status_edges` from a new exported
`payout.DestinationStateEdges()`, with no same-state exemption, and
`test/integration/enums` gains the fourth pairing -- so "held identical by the
enum suite" now covers every edge set in this area rather than three of four.

Residual 1 is unchanged: `verification_checks` INSERT is still `cp_app`'s.

**Evidence.** F-227. `migrations/00806_*.sql`, `internal/verification/state.go`,
`internal/verification/session.go`; `TestIntegration_EveryLegalEdgeTableMatchesItsGoTable`,
`TestIntegration_NobodyButTheMigrationRoleWritesAnEdgeTable`.

## D-122 — A destination says where it pays into, and the provider is asked about the whole recipient (2026-09-11, product goal §54, F-228)

**Problem.** `country` was optional on `POST /v1/me/payout-destinations`, and
`CanPayRecipient` was asked only when the client supplied one. The same body with
`"country":"FR"` was refused RECIPIENT_COUNTRY_UNSUPPORTED; without the field it
was accepted and marked VERIFIED. Nothing ever set `RecipientProfile.Region`, so
two of the six refusals could not fire at all (F-228).

**Chosen.** `country` is required — schema, handler and domain — and
`CanPayRecipient` is asked unconditionally, about the whole profile. `region` is
accepted and stored (00810), and required wherever the provider publishes
`ExcludedRegions` for that country, refused `VALIDATION_FAILED` rather than
`PROVIDER_UNAVAILABLE` because it is something the caller can supply.
`providerSupports` at payout re-asks `CanPayRecipient` from the stored country
and region instead of re-deriving a kind-and-currency check.

**Why derived rather than chosen.** `RecipientProfile.Validate` has refused an
empty country since it was written; the adapter's conditional was the only thing
standing between that refusal and the caller. Asking early is the stated reason
`CanPayRecipient` exists — telling somebody "this cannot work for you" before
they hand over a passport — and a question skipped when the answer is missing is
not asking early, it is not asking.

**Consequences.** A provider that publishes no `SupportedCountries` can now
register no destination at all, which is the correct reading of an adapter nobody
has verified and is why the test fixtures had to publish what they pay. Every
destination row carries a country, so `providerSupports` has something to ask
with at the moment value would leave rather than only at registration.

**Evidence.** F-228. `internal/httpapi/wiring_verification.go`,
`internal/httpapi/handlers_payout_destinations.go`, `migrations/00810_*.sql`,
`apps/web/src/pages/withdraw/Destinations.tsx`;
`TestPostMePayoutDestinations_RequiresTheCountryItPaysInto`.

## D-123 — A conversion request's state and the money beside it are written by the transition row (2026-09-11, product goal §54, F-229)

**Problem.** `payout_requests` — the row that says whether somebody's money left
— kept a column grant that included `state`, `reserved_quantity`,
`settled_quantity`, `reserved_at` and `settled_at`, with only the edge binding in
front of it. One transaction as `cp_app` moved a REJECTED request to SETTLED with
the whole amount settled and a forged provider reference, and the holder was
shown it (F-229).

**Chosen.** 00761's treatment, applied here. A legal-edge table populated from
`payout.StateEdges()`; a SECURITY DEFINER `cp_payout_apply_state_transition()`
that writes the state and refuses an edge that is not in the table; the money
moved onto the transition row — the two quantities, their instants, and the
provider's reference and status — and written by the same trigger; `REVOKE UPDATE
ON payout_requests FROM cp_app` with a column grant back for the provider slot it
claims before it calls (`provider`, `provider_idempotency_key`, `submitted_at`),
the decision it records (`verification_level`, `policy_version`, `policy_hash`,
`eligibility_reasons`) and `failure_reason`. Every new definer pins
`pg_catalog, public, pg_temp`.

**Why derived rather than chosen.** The money columns move onto the row rather
than staying granted because every place this package wrote one of them it was
ALSO moving the state — reserving is the VERIFIED step, settling is the SETTLED
step, returning a reservation is the FAILED or REJECTED step — so they were
already one event and writing them as two was what let a quantity be rewritten
beside a lawful move. That is 00733's own header describing a defect it could not
close for the columns it left granted. `provider_reference` and `provider_status`
come with them because they were written in the same breath in both places that
set them, and leaving them granted would have left `provider_reference =
'forged-by-cp_app'` reachable.

**Consequences.** `CompleteVerification` now records the reasons it re-decided,
which is why `eligibility_reasons` is on the grant: it was previously left at
whatever `Create` wrote, so a request that became eligible on verification still
rendered a shortfall it no longer had. A test fixture can no longer force a state
by hand; the one that did now produces the crash it was simulating
(`payouttest.Sandbox.CrashNext`), which is a better test than the forgery was.

**Evidence.** F-229. `migrations/00807_*.sql`, `internal/payout/service.go`;
`TestIntegration_MoneyColumnsAreOutOfTheApplicationsReach`,
`TestAuditWV_TheConversionRequestStateMachineIsEnforcedByTheDatabase`.

**Amended 2026-09-11 (second withdrawal-verification audit, F-264).** 00807
copied 00806's same-state exemption without copying the reason for it, and the
money it had just moved onto the transition row rode in through the copy.

A row whose endpoints are the same state skipped the edge check and the trigger
then wrote everything else it carried. So a `VERIFIED -> REJECTED` row inserted
onto a request ALREADY REJECTED changed no state -- and wrote
`reserved_quantity`, `settled_quantity`, both instants, `provider_status` and
`provider_reference = 'forged-by-cp_app'`, which is the exact column this entry
says was revoked for being the provider's word. 00731's deferred binding cannot
see it: it compares `old_val` to `new_val` and returns NULL when they are not
distinct.

The second half was the reservation invariant. `cp_payout_reservation_balanced`
(PO001, 00713) is a constraint trigger on `payout_allocations`, so a
`reserved_quantity` written with no allocation row behind it touched that table
not at all and was compared to nothing.

00815: the same-state exemption is dropped outright for `payout_requests` -- no
legitimate writer produces such a row, because `transitionWith` returns early --
and `payout_requests_reservation_backed` asks PO001's question where the quantity
is written. Both sides are re-read at COMMIT rather than taken from NEW, because
a deferred constraint trigger replays each row event with the values it had at
the time and an ordinary `Create` passes through `reserved = 0` with the
allocations already written. `settled_quantity` needs no clause of its own:
00713's CHECK already says it cannot exceed the reservation, so a forged
settlement has to forge the reservation first.

Residual: a transaction that writes BOTH a reservation and matching allocation
rows satisfies the invariant, which is correct -- it is what `Create` does -- so
what PO001 proves is that a quantity has provenance behind it, not that the
provenance is the right person's. That is what `payout_allocations`' own foreign
key to `credit_lots` and the consumption path enforce.

## D-124 — Proceeds are as final as what paid for them (2026-09-11, product goal §54, F-230, F-e2e-1)

**Problem.** `internal/nativemarket` and `internal/commerce` minted every earning
at `FinalityReversible` unconditionally. The only writer that promotes a lot out
of REVERSIBLE is `SettleFunding`, which keys on `credit_fundings.lot_id` — a row
an earning never has. So five of the six origins `SandboxPolicy` marks
withdrawable could never be withdrawn on any deployment, and the eligibility page
reported FUNDING_NOT_SETTLED — a reason it documents as one that waiting fixes —
on value whose finality nothing could move (F-230, and F-e2e-1 from the browser
audit).

**Chosen.** A derived lot records the lots consumed to fund it
(`credit_lot_parents`, written at mint in the same transaction) and is minted at
the LEAST final finality among them. `credit.Service.SettleDerived` promotes a
REVERSIBLE derived lot to SETTLED once EVERY parent is payout-eligible, and moves
one whose parent is DISPUTED or REVERSED to DISPUTED; it runs from the existing
settlement ticker in `cmd/api`, immediately after `settleOnce` because it reads
what that pass writes, batch-bounded, each lot taken under the advisory lock
`SetFinality` already uses and tried rather than waited for.

The AMM pool is fungible, so it keeps the same kind of record:
`native_market_credit_sources` holds one row per lot paid into a market's reserve
and the sell side draws them down in arrival order, so a sale's proceeds name
real parents rather than an average. A draw-down the record cannot cover mints
REVERSIBLE.

**Why derived rather than chosen.** A default in either direction is wrong, and
that is what forces the parent record. REVERSIBLE for ever is the defect renamed.
Payout-eligible by default is the laundering route the whole finality model
exists to close: buy with card-funded Credits, sell them into a market and out
again, withdraw, charge back. The only honest answer is the one the lots
themselves give, so the lots have to be named. Drawing the pool down in arrival
order rather than averaging keeps the record bounded by the reserve rather than
by the market's whole history, and makes a promotion possible later when those
contributions settle.

**What a chargeback on a spent purchase does, and the residual.**
`credit.Service.Reverse` unwinds the funding's OWN lot and posts a deficit
against the payer. Taking value back from a third party who earned it is a
posting kind this ledger does not have, and the earner may have spent it or had
it paid out. What is expressible is applied: a derived lot whose parent is
disputed or reversed is moved to DISPUTED — neither spendable nor payout-eligible
— and is never promoted. What is not expressible is recorded rather than
invented: a derived lot promoted on a SETTLED parent that is disputed months
later cannot be clawed back from the earner, and value already spent or already
paid out cannot be recovered at all.

**Consequences.** `TestIntegration_DemoCreditsCanBeSpentAndCanNeverLeave` held
seeded earnings by their FINALITY, and that floor was this defect rather than a
control. Its claim is narrowed to what is now load-bearing and is stronger for
it: the GRANT never leaves by ORIGIN on every policy in this build, and every
other seeded lot is proved to be derived from the grant and from nothing a person
paid. The residual there: a sandbox tier deliberately configured with a LIVE
payout adapter could pay a seeded earning out for real. No such adapter exists
(BLOCKERS B-01, B-06) and `payoutPolicyFor` returns `SandboxPolicy` only on a
sandbox tier, so the combination is unreachable today; what would close it is
`cmd/api` refusing to boot with `CP_API_DEMO_DATA` set and a payout slot that is
not the rehearsal one.

**Evidence.** F-230. `migrations/00809_*.sql`, `internal/credit/derived.go`,
`internal/nativemarket/poolprovenance.go`, `cmd/api/creditsettle.go`;
`TestIntegration_ASandboxTraderEarnsProceedsThatCanReachAPayout`,
`TestIntegration_ProceedsOfAReversiblePurchaseAreReversible`,
`TestAuditWV_AProceedsLotSettlesWhenItsFundingDoesAndNotBefore`.

**Amended 2026-09-11 (second withdrawal-verification audit, F-260, F-261,
F-262, F-266).** Four things this decision got wrong, superseded by D-131 and
D-132 and by 00816.

1. **Only the finality travelled.** "A derived lot inherits its parents'
   finality and nothing else" left the ORIGIN behind and made goal §23's
   forbidden round trip reachable for the first time: a trader holding only
   PROMOTIONAL, UNFUNDED Credits buys and sells, and the proceeds are
   MARKET_TRADING_PROCEEDS at UNFUNDED -- an origin `SandboxPolicy` releases, a
   finality `PayoutEligible()` admits (F-261). Before this decision the pattern
   was unreachable by accident, because proceeds were REVERSIBLE for ever. D-131
   adds the origin floor and supersedes this paragraph.

2. **"Arrival order" was the wrong draw-down.** The claim above that arrival
   order "keeps the record bounded by the reserve" is true and is not the
   question. FIFO hands a seller the BEST provenance the pool happens to hold,
   so a reversible purchase is laundered by an earlier contributor's settled
   Credits -- the exact route this entry says the finality model exists to close
   (F-262). D-132 draws the pool down worst first.

3. **`SettleDerived`'s candidate set never shrank.** `finality IN
   ('REVERSIBLE','SETTLED') AND EXISTS (a parent row)`, `ORDER BY lot_id LIMIT
   100`: promoting a lot left it in the predicate and UUIDv7s sort
   chronologically, so the sweep returned the oldest hundred derived lots for
   ever and both directions starved after the hundredth -- F-230 restored by the
   sweep written to fix it, at a volume any deployment passes in its first week
   (F-260). The predicate now selects only lots a pass would move.

4. **`credit_lot_parents` was INSERTable at any time.** Nothing bound a parent
   row to the mint, so one INSERT invented the provenance of a card payment
   months later and the sweep promoted it (F-266). 00816 binds the row to the
   lot's creating transaction; the residual is stated in that migration's header.

The consequences paragraph above is also superseded: the narrowing of
`TestIntegration_DemoCreditsCanBeSpentAndCanNeverLeave` was a mistake rather than
a simplification, and D-131 restores the strong form.

**Amended 2026-09-11 (third withdrawal audit, F-273).** The freeze direction
could not see the finality this decision's own rule mints into.

A derived lot is minted at the LEAST FINAL finality among its parents, and
valuedomain's ordering puts UNFUNDED between REVERSIBLE and SETTLED. So a
seller's earning funded by a buyer's promotional grant AND a buyer's settled card
purchase is minted UNFUNDED. `settleDerivedCandidates` opened
`st.finality IN ('REVERSIBLE','SETTLED')`, so that lot was outside the candidate
set for ever; and `finalityTransitions` called UNFUNDED terminal, so even a
candidate query that selected it could not have moved it. When the card was
charged back, nothing froze the earning. Had the buyer held only the purchase the
earning would have been SETTLED and the sweep would have frozen it; because a
grant was in the mix, nothing ever would. F-260 named this direction: "Freezing
starving is worse and quieter: an earning whose funding was charged back stays
spendable."

Two changes, both narrow. The candidate predicate's freeze clause is now "a
derived lot with a frozen parent that is not itself frozen", at ANY finality,
derived from `valuedomain.FrozenFinalities()` rather than a SQL literal — the
promotion clause keeps its REVERSIBLE test, because promotion out of UNFUNDED
would be value inventing a backer. And UNFUNDED gains exactly one edge, towards
DISPUTED, in `finalityTransitions` and in `cp_credit_finality_can_transition`
(00821); the finality parity test compares all twenty-five pairs.

The residual this decision already recorded is unchanged and now reachable one
state further: a derived lot frozen this way and later resolved in the platform's
favour stays DISPUTED, because no edge returns it to UNFUNDED and `SettleDerived`
promotes only from REVERSIBLE. Un-freezing automatically would be a sweep
deciding that somebody else's dispute ended well, and this build has no evidence
it could read to decide that.

**Amended 2026-09-11 (fourth withdrawal audit, F-278).** The residual above is
closed: D-140 builds the thaw this decision left out, and the sentence "un-freezing
automatically would be a sweep deciding that somebody else's dispute ended well"
was answering the wrong question.

The sweep does not decide anything about a dispute. It reads what the LOTS say,
which is what every other direction of this sweep does: a derived lot's finality
is the least final among its parents, and when no parent is frozen any more, the
lot's own frozen state is the stale fact. Deciding that a dispute ended well is
`UnfreezeFunding`'s job and it is driven by an external event, exactly as
`DisputeFunding` is; what was missing was that nothing carried its outcome
DOWNWARDS to the lots the freeze had reached. D-094 promised that mirror for
funded value, and D-124 never built it for derived value.

What stays true is the other half of the residual: value that was actually taken
back does not come back. A REVERSED parent is terminal, so the thaw clause —
which requires that NO parent is still frozen — never selects its children, and
they stay frozen for ever by design.

## D-125 — The idempotency record keeps what may be kept, not the whole answer (2026-09-11, product goal §54, F-231)

**Problem.** `runCommand` marshals a command's whole response into
`idempotency_keys.response_body`. `POST /v1/me/verification/sessions` returns the
hosted verification link, which ADR-0025 §3 says is "handed to the browser that
asked for it and written down nowhere" — and it was in a row `cp_readonly` and
`cp_ops` may SELECT and `cp_app` may not DELETE, for the whole 24-hour TTL,
for every session anybody started (F-231).

**Chosen.** `CommandResult` gains a `StoredBody`. The caller gets the whole
answer; the record gets the answer minus the fields the route declares
never-stored, plus a `resume` sentence saying why. A replay therefore answers
with the session, without the link, and with something for the client to do.
`neverStored` is keyed by operation AND field, and `hosted_url` is its only
entry.

**Why derived rather than chosen.** "Do not make that route idempotent" was the
other option and is wrong: idempotency on a command route is what stops a retry
starting a second identity check. A `Redacted()` method on the result was the
shape the brief offered first; the generated API types live in
`internal/gen/api` and cannot carry one, so the declaration lives beside the
route. Per field rather than a global deny-list of names because the same field
name means different things on different routes — the session's `expires_at` is a
link's life and a quote's `expires_at` is a price's, and the second MUST be
recorded. A replay says something rather than omitting the field silently,
because a browser waiting for a field that is never coming is a worse answer than
a refusal.

**Consequences.** A route in this area that starts returning a credential has to
declare it, and `TestNoNeverStoredFieldReachesTheIdempotencyRecord` drives every
command route in the journey and reads what landed, so forgetting fails a test
rather than an audit.

**Evidence.** F-231. `internal/httpapi/neverstored.go`,
`internal/httpapi/command.go`, `internal/httpapi/ports.go`;
`TestNoNeverStoredFieldReachesTheIdempotencyRecord`,
`TestAuditWV_TheHostedVerificationURLIsWrittenDownNowhere`.
## D-126 — A closed residual risk is struck in its slot, and a promotion takes the next number (2026-09-11, goal §54, F-239)

- **Problem:** THREAT_MODEL §8's "top ten residual risks" carried two entries the tree had closed. Correcting them is not a matter of deleting two lines: runbooks cite these by number (`unknown-transaction.md` and `wallet-provider-compromise.md` both say "re-score residual risk #1"), and the list is also the place the *current* top risks are supposed to be visible. Deleting an entry renumbers everything below it and silently redirects those citations; leaving the slot empty makes the list shorter than its own heading; and simply striking two entries without promoting anything leaves the document saying nothing about the risks that replaced them.
- **Chosen:** a closed risk stays in its slot, struck through, with the evidence that closed it and with whatever narrower part genuinely remains; a risk promoted to replace one takes the **next free number**, so the numbering only ever grows. The section states the convention where a reader meets it, and says how many slots are live. F-111 had already struck #8 in place — this makes that the rule rather than one instance of it.
- **Why derived rather than chosen:** the same rule the register itself follows. A finding number is permanent and its status changes; a residual risk is the same kind of object, and the document that says "listing a closed control among the top ten understates the system" cannot also be a document whose numbering moves under a citation.
- **Which two were promoted, and why those:** the un-armed circuit breaker (`nativemarket.ConservativeSafetyPolicy()` sets `circuit_breaker_move_bps: 0`, D-065) and the absence of a per-identity auth rate limit and any `login_anomaly` event. Both were already written down elsewhere in the tree — one in a decision entry and an ADR, the other in this document's own §3.1 and F15 row and in SECURITY.md §12's list of PART 130 kinds with no writer — and neither was on the list a reviewer reads to find the top risks. A risk that is recorded in three places and absent from the list that ranks them is not being managed.
- **Consequences:** §8 now has twelve numbered entries, nine live. A future correction adds 13 rather than reusing 9. The two promotions are both closeable by work that exists: arming the breaker is one `scripts/marketsafety` record on a deployment with an operator; the auth limit needs a per-identity counter and one event kind.
- **Evidence:** `docs/threat-model/THREAT_MODEL.md` §8; `internal/nativemarket/safety.go`, `internal/httpapi/middleware.go` (`RateLimits.Auth`), D-065, ADR-0027; `TestAuditDocs_NoDocumentDeclaresAnAbsenceTheTreeContradicts`.

## D-127 — The dead `internal/risk/policy` deny stays, named in the document that quotes it (2026-09-11, goal §54, F-238)

- **Problem:** `.golangci.yml`'s `agent-authority` depguard rule and `scripts/lintfin` rule 3 both deny `internal/risk/policy` to the agent trees. No such package has ever existed — the risk policy types are in `internal/risk` itself — so that one deny can never fire, and SECURITY.md §5 quoted the list as though all five entries were live controls. A rule that cannot fire, quoted as evidence, is the shape this audit exists to catch.
- **Chosen:** leave both rules exactly as they are, and say in SECURITY.md §5 that the entry names a package that has never existed, that it therefore cannot fire, and what actually keeps agents out of risk policy: `risk_policies.created_by_actor_type IN ('OPERATOR','SYSTEM','USER')` (migration 00152), `risk:policy_write` outside the agent permission set (`TestGoldenMatrix_AgentSet`), and `TestAgentTreesNeverImportAuthority`.
- **Why derived rather than chosen:** the two alternatives are both worse. *Re-pointing it at `internal/risk`* would deny `internal/strategy/validate.go` an import it legitimately makes — reading the kernel's limits to validate a strategy is not mutating a policy, and depguard matches package paths, not intent — so `make lint` would fail on correct code and the rule would be relaxed again by whoever hit it. *Deleting it* would leave `MASTER_BUILD_STATE.md:1216`, `REQUIREMENTS_TRACEABILITY.md` R-009-1 and `PRODUCTION_READINESS_REPORT.md:158–159` citing a rule that no longer exists, which is the same defect in three more documents, and one of those three is a file this work may not edit.
- **Consequences:** the guard keeps a fifth entry that does nothing, and the document a reviewer scores posture from says so. If `internal/risk` is ever split so that policy writing has its own package, the deny starts working and this note is deleted with the same commit.
- **Evidence:** `.golangci.yml:164`, `scripts/lintfin/main.go:70`, `docs/security/SECURITY.md` §5; `internal/risk/policy.go`, `internal/strategy/validate.go`, migration 00152.


## D-131 — A derived lot is as withdrawable as the least withdrawable thing that funded it, in origin as well as in finality (2026-09-11, product goal §23, §54, F-261)

**Problem.** D-124 made a derived lot inherit its parents' FINALITY and nothing
else. A trader holding only PROMOTIONAL, UNFUNDED Credits buys into a native
market and sells back out; the proceeds are MARKET_TRADING_PROCEEDS — an origin
`SandboxPolicy` marks withdrawable — at UNFUNDED, which `PayoutEligible()`
admits; and `payout.Engine.Evaluate` returns `Sufficient()` at PAYOUT_KYC.

Goal §23 forbids that shape in as many words: "nonwithdrawable source → trade →
magically payout-eligible balance unless the eventual external/legal/provider
policy explicitly allows it". `SandboxPolicy` says the opposite of allowing it —
"a promotional grant that could leave the system would be the first rule somebody
copied" — and `CREDIT_ECONOMY.md` §4 says promotional value "can never leave this
system under any policy in this build". Before D-124 the pattern was unreachable
by accident, because every earning was minted REVERSIBLE for ever; D-124 opened
it, and two shipped tests then asserted the opening as correct (F-261).

**Chosen.** Every lot carries an **origin floor** beside its finality, in
`credit_lot_state.origin_floor`, maintained by a trigger and by nothing else
(00816). A lot with no parents has its own origin as its floor; a derived lot's
floor is the most restricted floor among its parents. "Most restricted" is
defined once, in `valuedomain`: closed under `DefaultPolicy` and `SandboxPolicy`
both < closed under one < permitted, with ties broken on the origin's own name.
`cp_credit_origin_floor_rank` is the same ordering in SQL, and
`TestIntegration_GoAndSQLAgreeOnHowRestrictedEveryOriginIs` holds the two
identical the way `cp_credit_finality_can_transition` is held to
`CanTransitionFinality`.

`Policy.Permits` permits a lot only if it permits BOTH the lot's origin and its
floor, and the floor has **no permissive zero value**: an unstated or undeclared
one is `UNKNOWN_ORIGIN` and refuses. `eligibility.ExplainWithdrawal` carries the
floor onto the bucket and `GET /v1/me/eligibility` renders it as `origin_floor`
when it differs from the origin, so a person reads "this came from a promotional
grant" rather than `ORIGIN_NOT_PAYOUT_ELIGIBLE` on a bucket of trading proceeds,
which is an answer nobody can act on. `verification_would_suffice` is suppressed
on a bucket the floor forbids: verifying will never release it, and saying
otherwise is the refusal §19 forbids, dressed as encouragement.

**Why derived rather than chosen.** The finality was never the dimension this
finding lives in. UNFUNDED value IS final — nobody can claw back a gift — and
what makes a grant unwithdrawable is its ORIGIN, which the policy closes.
Answering F-261 by minting a grant's proceeds REVERSIBLE would be F-230 restored:
a grant has no `credit_fundings` row, so `SettleFunding` cannot reach it and
`SettleDerived` would wait for a parent already as final as it will ever be.

The floor is the MOST RESTRICTED parent rather than an apportionment for the same
reason the finality is the least final: a bucket containing one grant-funded unit
is a bucket that is not wholly withdrawable, and splitting a lot into a
withdrawable part and a granted part is a second provenance model on top of the
one the ledger has, whose first question — which part is the profit — nobody can
answer.

Reading the floor's rule for `PayoutAllowed` only, and not for the capability,
the verification level or the hold period, follows from `OriginRule.Validate`: a
rule that forbids payout may not name a capability, so asking a grant's rule for
one would demand a gate that cannot exist.

**Consequences, stated and tested.**
1. A demo trader's proceeds are never withdrawable.
   `TestIntegration_DemoCreditsCanBeSpentAndCanNeverLeave` is restored to the
   strong form D-124 narrowed away: EVERY seeded lot, in every account the seeder
   touched, is refused by `Policy.Permits` under BOTH policies in this build,
   with `ORIGIN_NOT_PAYOUT_ELIGIBLE` among the reasons — asked of the policy
   rather than of a rule repeated in the test.
2. A trader whose parents are all PURCHASED still earns withdrawable proceeds.
   `TestIntegration_ASandboxTraderEarnsProceedsThatCanReachAPayout` and
   `TestAuditWV_AnEarnedCreditCanReachAPayoutEligibleFinality` are funded with a
   settled purchase instead of a grant, each with the reason in its own comment;
   browser scenario F reaches a conversion request unchanged, because the
   sandbox fixture buys Credits.
3. A trader who buys with a grant and sells at a profit cannot withdraw the
   profit either. That is the intended answer and it is not hidden.
4. `credit.Lot`, `eligibility.OriginHolding` and `eligibility.OriginBucket` grow
   a floor; `WithdrawalOriginBucket.origin_floor` is an additive optional field.

**Documents reconciled.** `CREDIT_ECONOMY.md` §4 and §7,
`VERIFICATION_AND_WITHDRAWAL.md` §7 and §9, and D-124 (amended, dated) now say
this rather than the finality-only rule.

**Residuals.**

1. The ordering is computed from the policies this BUILD ships. A policy
   persisted through the approval path that released an origin `SandboxPolicy`
   closes would not change any floor already written, because a floor is fixed
   when the lot is minted — which is the conservative direction, and is the same
   property `credit_lots.origin` already has.

2. **A seeded local or sandbox tier can no longer reach a settled conversion
   request from the browser, and that is not a defect in the journey.**
   `scripts/seedeconomy` issues 25,000 PROMOTIONAL/UNFUNDED Credits to each
   customer and says in its own comment why a seeder must not mint PURCHASED
   ones — it would be inventing a funding event, and PURCHASED is the origin a
   payout policy is most likely to permit. Everything a seeded tier's customers
   hold is therefore a grant, so under this decision everything they earn from
   each other carries a PROMOTIONAL floor and nothing can leave.

   Reaching that leg from a browser needs a customer holding a PURCHASED lot
   behind a RECORDED funding, and nothing can produce one on a local tier: the
   credit-purchase slot has one adapter (`internal/provider/stripecredit`) and it
   refuses a fake mode on the stated ground that Stripe's own test mode is a
   better fake than any we would write. Minting the lot without the funding, or
   relaxing the seeder, would be the fabrication the goal forbids — so neither
   was done here.

   What holds meanwhile: `f-verified-sandbox.spec.ts` asserts the refusal is the
   FLOOR, by name and by `origin_floor`, so the browser suite proves this
   decision rather than tolerating an unexplained no; `audit-journey.spec.ts`
   step 10 skips with the per-origin reasons printed; and the withdrawable case
   — an earning funded by value the policy permits, reserved, submitted and
   settled — is driven end to end by the Go suites
   (`TestIntegration_ASandboxTraderEarnsProceedsThatCanReachAPayout`,
   `TestAuditWV_AnEarnedCreditCanReachAPayoutEligibleFinality`, and
   `internal/payout`'s own reserve-and-settle tests). What would close it is a
   seeding decision, not a test change: a LOCAL seed that records a
   sandbox-MODE credit funding (`credit_fundings.provider_mode`, 00793/D-096
   already model exactly that) and mints the PURCHASED lot from it, labelled a
   rehearsal everywhere. That is a change to a seeder's policy and belongs to
   whoever owns `scripts/seedeconomy`.

**Evidence.** F-261. `migrations/00816_*.sql`,
`internal/valuedomain/originfloor.go`, `internal/valuedomain/policy.go`,
`internal/credit/types.go`, `internal/eligibility/withdrawal.go`,
`internal/httpapi/wiring_verification.go`;
`TestAuditWV2_AGrantThePolicyForbidsCannotBeTradedIntoWithdrawableValue`,
`TestPolicy_PermitsReadsTheFloorAsWellAsTheOrigin`,
`TestIntegration_GoAndSQLAgreeOnHowRestrictedEveryOriginIs`,
`TestIntegration_DemoCreditsCanBeSpentAndCanNeverLeave`,
`TestGetMeEligibility_ExplainsPerOrigin`.

**Amended 2026-09-11 (third withdrawal audit, F-271, F-275).** Two claims in this
entry were wrong, and D-137 and D-138 replace them.

*Residual 1 is corrected.* It says a policy persisted after a lot was minted
"would not change any floor already written … which is the conservative
direction". It is conservative only for a policy that releases a SUPERSET of what
this build's policies release. The ordering is computed from `DefaultPolicy` and
`SandboxPolicy`, and ties break on the origin's own NAME — so a lot funded half
by a purchase and half by trading gains records MARKET_TRADING_PROCEEDS, and a
policy that releases trading gains and closes the purchased float releases it.
That policy is one of the two answers B-02 can come back with. D-138 carries the
whole root SET on `credit_lot_state.root_origins` and requires a policy to
release every root, which IS conservative for every policy; the floor and its SQL
rank remain, for display and for ordering.

*The paragraph beginning "a floor is fixed the moment the lot exists" was true
one level deep and false in general.* The trigger computed a child's floor from
its parents' CURRENT floors, and a parent's floor falls only when ITS parent rows
land — so a transaction that wrote a grandchild's parent row before the child's
gave the grandchild a floor better than its provenance. D-137 computes the floor
from the provenance ROOTS and refuses a parent row for a lot that is already
somebody else's parent, which makes the sentence true at every depth.

*The residual of F-266 recorded in 00816's header is corrected with it.* It
argues that naming an EXTRA parent "can only make a lot LESS withdrawable", and
that the direction that could launder is OMITTING one. Nothing was omitted in the
audit's reproduction; every true parent row was written, and the floor was still
better than the provenance. The argument holds for one level and the fix makes it
hold for all.

## D-132 — The pooled reserve is drawn down worst first (2026-09-11, product goal §54, F-262)

**Problem.** `native_market_credit_sources` was drawn down in arrival order
(`ORDER BY created_at, id`). A pool is fungible, so "whose Credits left" is a
CHOICE rather than a fact, and FIFO makes the choice that hands a seller the best
provenance the pool happens to be holding. A pays in 40,000 settled Credits; B
pays in 40,000 a card issuer can still take back, sells back less than A put in,
and is minted proceeds funded by A's settled contribution — payout-eligible at
birth — while B's own reversible Credits stay in the pool for whoever sells next
(F-262). That is the laundering route D-124 says the finality model exists to
close, reached through somebody else's money instead of the seller's own. The
shipped test missed it because its market had ONE contributor, so FIFO handed the
trader back their own lot.

**Chosen.** Open sources are consumed in order of: origin floor most restricted
first, then least final first, then oldest. A seller can never be handed
provenance better than the pool's worst outstanding contribution.

The rows are still SELECTed and LOCKED in arrival order, which is not the same
ordering and is deliberate: two concurrent sales against one market must take the
same row locks in the same sequence or they deadlock, and arrival order is the
one ordering that cannot change while a transaction runs. The draw-down order is
decided afterwards, in memory, over the rows the transaction already holds.

**Why derived rather than chosen.** Fail closed is the architecture's rule and
this is what it looks like when it is inconvenient. Of the three orderings
available, FIFO is a laundering route; pro-rata (a sale draws a slice of every
outstanding contribution) gives every seller a floor as bad as the pool's worst
ANYWAY, and multiplies the parent rows by the number of contributors, so it is
worse in both directions; worst-first gives the same answer as pro-rata for the
floor and the fewest parent rows.

**What it costs, stated plainly.** An honest seller paying into a pool that still
holds somebody else's REVERSIBLE contribution receives reversible proceeds until
that contribution settles — and `credit.Service.SettleDerived` promotes them on
the next pass when it does, so the wait is bounded by the other person's dispute
window rather than permanent. Somebody else's promotional grant in the pool gives
a seller a PROMOTIONAL origin floor, which no policy in this build releases and
which does NOT move when anything settles (D-131). On a seeded demo tier every
pool contribution is a grant, so that is the state demo markets are in, and it is
correct: demo money does not become withdrawable by passing through a market.

**Evidence.** F-262. `internal/nativemarket/poolprovenance.go`;
`TestAuditWV2_TheFirstContributorsFinalityIsHandedToTheNextSeller`,
`TestIntegration_ProceedsOfAReversiblePurchaseAreReversible` (widened to two
contributors), `TestIntegration_AnHonestSellerWaitsForTheWorstContributionToSettle`.

## D-133 — A poll is a call to a provider, and the session records when the last one happened (2026-09-11, product goal §54, second-round observation (a))

**Problem.** `GET /v1/me/verification/sessions/{id}` calls
`verification.Provider.Get` on every request whose session is not terminal. It is
a GET, so `internal/httpapi`'s rate-limit selection puts it in the General class
— 600 a minute per principal on the deployment, 6000 under the browser suite — so
one signed-in person can make this deployment call an identity provider six
hundred times a minute, against a contract whose pricing and rate limits are the
provider's and not ours.

**Chosen.** A per-session minimum interval, not a rate-limit class.
`verification_sessions.provider_polled_at` (00818) records when the provider was
last asked, and a poll inside `verification.PollMinimumInterval` — ten seconds —
answers from the session's recorded status and calls nobody.

**Why derived rather than chosen.** The other option was moving the route into
the Quote class, and it was not taken for three reasons. The Quote class is
selected by URL PATH before routing, so the rule would be a second path pattern
in a function whose job is to classify by shape, and the next provider-calling
GET would need a third. A budget is per PRINCIPAL: two people polling one
session, or one person with two tabs, get two budgets and the provider sees the
sum — an interval on the SESSION bounds the quantity a provider contract is
actually written in. And a rate limit REFUSES, where a poll inside the interval
does not need to be refused: the answer is already on the row, it is the answer
the last call got, and this is the route a person watching a spinner hits.
Answering from the record is better product behaviour AND fewer provider calls
than a 429.

Ten seconds is chosen rather than inherited: a hosted identity check takes tens
of seconds to minutes, the web client polls while a person waits, and a provider
answering faster than ten seconds is answering faster than the screen can be
read. The write happens BEFORE the call and in its own committed transaction, for
the reason the payout provider's idempotency key is written first: a process that
dies between the two must not leave a provider that was asked and a record that
says it was not. The cost of that ordering is one interval's delay after a crash.

**Consequences.** `IngestWebhook` is untouched, so a deployment with webhooks
wired sees a decision immediately whatever the interval says. A terminal session
is still not polled at all. `cp_app` gets UPDATE on the one new column and
nothing else.

**Residual.** The interval bounds calls per session, not per deployment. A
caller who opens sessions in a loop still makes one provider call per session —
which `verification_sessions_one_open_per_user` bounds to one open session per
PERSON, so the remaining lever is account creation, which the capacity ceiling
(`CP_CAPACITY_MAX_ACCOUNTS`) governs.

**Evidence.** Second-round observation (a). `migrations/00818_*.sql`,
`internal/verification/service.go`, `internal/verification/repository.go`;
`TestIntegration_APollInsideTheMinimumIntervalCallsNobody`.

**Amended 2026-09-11 (third withdrawal audit, F-274).** The interval was three
steps with nothing holding them together: `Poll` READ `provider_polled_at`,
compared it with the clock, and only then committed `MarkProviderPolled` in a
transaction of its own. Every request that read the row before that write landed
saw the old timestamp and proceeded. Twenty concurrent polls of one session made
seventeen provider calls. Fifty polls IN SERIES were handled correctly, which is
why `TestIntegration_APollInsideTheMinimumIntervalCallsNobody` passed and why the
bound this entry claims was never actually enforced against the case a browser
produces: tabs, or one client with a connection pool.

`Repository.ClaimProviderPoll` replaces `MarkProviderPolled` with one statement —
`UPDATE verification_sessions SET provider_polled_at = $2 WHERE id = $1 AND
(provider_polled_at IS NULL OR provider_polled_at < $3) RETURNING …`. PostgreSQL
evaluates the predicate under the row lock the UPDATE itself takes, so exactly one
concurrent statement can satisfy it; the losers match no row and answer from the
record, which is what the interval has always meant them to do. It is still
committed BEFORE the provider is called, for the reason this entry already gives.

No migration: the column, its grant and its comment are 00818's and unchanged.
The residual above is unchanged.

## D-134 — A rehearsal is recorded, not inferred from a NULL two readers read differently (2026-09-11, product goal §54, second-round observation (b))

**Problem.** 00810 added `payout_requests.sandbox` NULLABLE, on 00793's
reasoning: a row written before the migration has no recorded fact, and `NOT NULL
DEFAULT false` would assert about every one of them that it was a real payout,
which is the one direction this label must never be wrong in. The reasoning is
right and the result is a column two readers disagree about.
`httpapi.toAPIPayout` reads NULL as a REHEARSAL; the CHECK reads it as REAL —
`CHECK (NOT coalesce(sandbox, false) OR environment IS DISTINCT FROM 'PROD')` —
so a NULL row is exempt from the rule the constraint exists to state, and the
exemption is invisible because `coalesce(sandbox, false)` looks like a default
rather than like a hole. One of the two is wrong on any given row and nothing can
say which.

**Chosen.** The column becomes NOT NULL (00817), the CHECK drops the `coalesce`
so it says what it means, and the rows with no recorded fact are backfilled
`true`.

**Why `true` is a statement and not a convenience.** Every `payout_requests` row
that exists anywhere was written on a non-PROD tier, because no PROD deployment
of this system has ever existed.
`docs/audit/FINAL_CHECKPOINT_2026-09-10.md` §11 states the deployment as it
stands — "Every provider is `sandbox`. `CP_AUTH_MODE` is `oidc`. `CP_ENV` is
`STAGING`" — and §13 states that every capability gate is inactive by absence,
`capability_gates` holding zero rows, so `PAYOUT_RESERVE` and `PAYOUT_SETTLE`
have never been active anywhere. A payout row written under those conditions is a
rehearsal by every definition the system has. The migration CHECKS the ground
rather than assuming it: it refuses to run if it finds a row with
`environment = 'PROD'` and no flag, so on a database where the claim is false it
fails loudly instead of relabelling somebody's money.

**Why not a DEFAULT.** A default lets a new writer forget, and this is a safety
label. `payout.CreateRequest.Validate` already refuses a request with no
environment and `Create` writes both columns, so the only writers that could
forget are test fixtures — which is exactly where a loud failure belongs, and
where three of them now state the fact.

**Consequences.** `environment` stays nullable: nothing reads it as permission,
`scanRequest` already coalesces it to an empty string, and making it NOT NULL
would be a second change riding on the first.

**Evidence.** Second-round observation (b). `migrations/00817_*.sql`,
`internal/payout/payout.go`, `internal/payout/repository.go`,
`internal/httpapi/handlers_native.go`; `TestIntegration_TheWithdrawnInvariantsAreStillEnforced`.

## D-135 — A refusal to store a body stores an empty document, because nil means "store the whole body" (2026-09-11, product goal §54, F-267)

**Problem.** `redactForStorage` returns `nil` on both branches that exist to
refuse a response nobody could inspect, under a comment calling that "the safe
direction". `CommandResult.stored()` reads nil as "there is nothing special to
store, keep Body". So the two branches that exist to keep a credential out of
`idempotency_keys.response_body` put the WHOLE answer in it, and the comment
beside them said the opposite of what happened (F-267).

**Chosen.** Both branches return `[]byte("{}")`, and so does a body that parses
as a bare `null` — it carries no field to strip and is not a document either.

**Why derived rather than chosen.** The alternative is changing the SENTINEL —
making `StoredBody` an explicit "store nothing" flag rather than overloading nil.
That is a wider change to a type three call sites share, for a distinction one
constant expresses, and the bug is not that nil is a bad sentinel: it is that a
function returned the sentinel for "keep everything" while meaning "keep
nothing".

**Consequences.** Not reachable from today's routes, because every command
response is a struct that marshals to a JSON object. It is one response type away
— a route returning a list, a string or `null` — which is why the fix is the
constant and not a note. The added unit test drives all four uninspectable shapes through `stored()`.

**Evidence.** F-267. `internal/httpapi/neverstored.go`;
`TestAuditWV2_RedactForStorageDoesNotFallBackToTheWholeBody`,
`TestRedactForStorageStoresNothingRecognisableWhenItCannotInspectABody`.

## D-136 — A payout takes exactly the units its decision evaluated (2026-09-11, product goal §23, §54, F-270)

**Problem.** Eligibility is decided per LOT and the reservation consumed by
ORIGIN. `payout.Engine.Evaluate` walks `credit.EligibleLots`, which asks
`valuedomain.Policy.Permits` about each lot's finality, its origin and its
provenance, and records the lots it approved in `Decision.Lots`.
`payout.Service.reserve` then passed `credit.EligibleOrigins(d.Lots)` — the set
of origin STRINGS — as `ConsumeRequest.AllowedOrigins`, and `credit.Consume`
took whichever lot of those origins sorted first in consumption order at any
merely SPENDABLE finality.

`ConsumeRequest.LotIDs` existed for exactly this and was unused. Its own comment
says why the origin filter is not enough: "two purchases produce two lots of the
same origin".

Two independently reachable consequences, both driven by the auditor:

1. **Finality.** `FundingFinality.PayoutEligible()` admits SETTLED and UNFUNDED —
   "paying out value that a card issuer can still reclaim turns a chargeback into
   an uncollateralised loss, which PART XI forbids" — but `Spendable()` also
   admits REVERSIBLE, and `RequireSpendableFinality` is the filter the consume
   applied. A decision approving a settled purchase was filled from a reversible
   one.
2. **Provenance.** A decision approving MARKET_TRADING_PROCEEDS whose floor is a
   settled purchase was filled from proceeds whose floor is a promotional grant.
   That is F-261's laundering route reopened through the filter the reservation
   uses, one layer below where D-131 closed it.

**Chosen.** `reserve` passes `credit.EligibleLotIDs(d.Lots)` as
`ConsumeRequest.LotIDs`, and the consume path refuses to draw from any lot
outside them: the restriction is a parameter of the one constant statement
`openLotsQuery` is, and `Consume` asserts on the way out that every lot it
selected is in the set. The payout path's finality bar is its own parameter,
`RequirePayoutFinality`, which admits UNFUNDED and SETTLED only;
`RequireSpendableFinality` stays exactly as it is, for spending, because
REVERSIBLE value may buy things and that is the deliberate product answer.

`payout_allocations` records `origin_floor` beside `origin` (00820, backfilled
from the lot's state), and `payout.Provenance`, `DecisionProvenance` and
`ProvenanceSlice` fold by (origin, floor) — so `GET /v1/payouts/{id}` and the
quote's preview report a payout drawn on trading proceeds out of a purchase
separately from one drawn on trading proceeds out of a grant.

**Why derived rather than chosen.** The lots are what the decision is ABOUT.
Every alternative is a way of describing them more coarsely: origin plus
finality, origin plus floor, origin plus floor plus finality. Each closes the
case in front of it and none closes the next one, because the coarse key is
always missing whichever field the next audit round looks at. `Decision.Lots`
already holds the answer, and the field that passes it already existed.

The finality filter could have been done by tightening `RequireSpendableFinality`
to the payout-eligible set. It was not: `internal/commerce` and
`internal/nativemarket` set the same flag to spend, and narrowing it would stop a
reversible purchase buying anything — a product change, made silently, to fix a
payout bug.

**Consequences.**

1. A reservation whose lots have moved between the decision and the consume now
   REFUSES with `INSUFFICIENT_BUYING_POWER` rather than silently substituting a
   different lot. Both run in one transaction on the commit path, so the window
   is theoretical; the refusal is the right answer to it either way.
2. `Decision.Origins` stays, reported so a caller can say what kind of value a
   decision draws on. It is no longer a filter, and its comment says so.
3. `credit.Allocation` carries the lot's floor, so a consumer recording what left
   can record what it WAS. `internal/commerce` and `internal/nativemarket` ignore
   the new field.
4. Additive contract fields: `origin_floor` on `PayoutProvenanceSlice`.

**Residuals.** `payout_allocations.origin_floor` is a RECORD, not a control: what
stops the wrong lot being reserved is the lot restriction, and the column is how
a reader can tell afterwards. Its backfill reads the floor off the lot's state
row, which is correct only because a floor never moves once written — 00816 fixes
it at mint and 00819 makes it independent of insertion order.

**Evidence.** F-270. `internal/payout/service.go`, `internal/payout/payout.go`,
`internal/payout/provenance.go`, `internal/payout/repository.go`,
`internal/credit/types.go`, `internal/credit/balances.go`,
`internal/credit/repository.go`, `internal/credit/service.go`,
`migrations/00820_a_payout_records_what_left_as_what_it_was.sql`;
`TestAuditWV3_APayoutApprovedOnASettledLotIsFilledFromAReversibleOne`,
`TestAuditWV3_APayoutApprovedOnAPurchasedFloorIsFilledFromAPromotionalOne`,
`TestIntegration_AMixedBalanceDrawsOnlyTheLotTheDecisionApproved`,
`TestIntegration_ProvenanceReportsTwoFloorsOfOneOriginSeparately`.

**Amended 2026-09-11 (fourth withdrawal audit, F-281).** Two of this decision's
sentences were true of every set except the empty one.

"The restriction is a parameter of the one constant statement `openLotsQuery` is"
and "`Consume` asserts on the way out that every lot it selected is in the set"
both stopped holding when the set was EMPTY. The statement read `cardinality($5)
= 0 OR ...`, so a restriction to no lots selected every lot the account held, and
the assertion was guarded by `len(r.LotIDs) > 0`, so the one input on which the
filter was absent was also the one on which the check was. `reserve` passes
`credit.EligibleLotIDs(d.Lots)`, and a decision that approved nothing produces
exactly that empty set; what an unrestricted consume takes first is a promotional
grant.

So the restriction is DECLARED rather than inferred from a slice's length:
`ConsumeRequest.RestrictToLots` beside `LotIDs`, nil for "unrestricted" and a
non-null array — empty included — for "exactly these". The cardinality escape is
gone, the assertion runs whenever a restriction is declared, `reserve` always
declares one, and naming lots without declaring the restriction is refused by
`Validate` rather than guessed at.

`ConsumeRequest.AllowedOrigins` is deleted. This decision left it in place with a
comment saying it "is no longer what a payout reservation uses"; it was no longer
what anything used, it was the coarse filter F-270 was about, and it read an
empty slice the same permissive way. A filter nothing sets is a filter nobody
notices going wrong. `credit.EligibleOrigins` stays: it reports what KIND of
value a decision draws on and is not a filter.

## D-137 — A floor is computed from the provenance roots, and a lot's provenance is closed before anything derives from it (2026-09-11, product goal §23, §54, F-271)

**Problem.** 00816's trigger computes a child's floor from its parents' CURRENT
`credit_lot_state.origin_floor`, which reads one level deep. A lot's own floor is
opened at its OWN origin by `cp_credit_lot_open` and falls only when ITS parent
rows land, and `cp_credit_lot_parent_is_written_at_mint` permits a parent row for
any lot created in the same transaction. So a transaction that mints a child of a
grant and a child of THAT, writing the GRANDCHILD's parent row first, gives the
grandchild a floor of MARKET_TRADING_PROCEEDS — and `SandboxPolicy` releases it.
Nothing recomputes it afterwards. 00816's own BACKFILL states the correct rule,
with `WITH RECURSIVE`; the trigger implemented a different one.

**Chosen.** Two halves, one computation.

`cp_credit_lot_root_origins(uuid)` walks the ancestry to the lots that nothing
funded — the backfill's query, named — and both `origin_floor` and
`root_origins` are set from its answer on every parent row. And
`cp_credit_lot_parent_has_no_descendant_yet` refuses a `credit_lot_parents` row
whose CHILD is already somebody else's parent, BEFORE INSERT, with CR005 (00819).

**Why derived rather than chosen.** The alternative the finding offers is a
DEFERRED constraint trigger that recomputes, at COMMIT, the floor of every lot
whose ancestry changed in the transaction. Both make the floor independent of
insertion order. They differ in how they fail: under the deferred recomputation
the wrong floor exists for the length of the transaction and is corrected by a
second computation that has to find every affected descendant, and a defect in
that search leaves a floor that is wrong and looks settled. Under the ordering
constraint the sequence that produces a stale read cannot be written at all — the
INSERT raises and the mint fails with it. A refusal is a mint that did not happen;
a wrong floor is money that may leave.

The constraint also makes the incremental rule and the recursive rule provably
the same, which is why the recursion is a statement of intent rather than a
correction. If a row (L,P) may only be written while nothing names L as a parent,
then every row (P,Q) precedes every row (L,P) — because (L,P) names P, and the
constraint would have refused (P,Q) after it. So when L's floor is computed, P's
provenance is complete and P's floor is final; by induction over the chain, every
ancestor's floor is final before it is read.

**Consequences.**

1. A mint site that writes a CHAIN of derived lots in one transaction must write
   each lot's own provenance before using it as a parent. No mint site in this
   build writes such a chain: `internal/commerce` mints one earning,
   `internal/nativemarket` mints proceeds and fees out of one draw-down, and none
   of them is a parent of another. The constraint refuses nothing that happens
   today.
2. 00819 recomputes `origin_floor` for every existing row from the recursive
   rule, which corrects any floor 00816's trigger computed from a parent whose
   own floor had not fallen. A deployment carrying such a floor has no way to
   know which rows they are, so every row is recomputed.
3. `credit_lot_state` gains a CHECK that `origin_floor` is one of
   `root_origins`, so the display answer and the permission answer cannot come
   apart.

**Residuals.** The second residual 00816 records is unchanged: two transactions
could in principle share a `transaction_timestamp()`, which is what binds a
parent row to its mint. The first one — the "extra parent only lowers" argument —
is corrected by this decision and by D-138.

**Evidence.** F-271. `migrations/00819_a_floor_is_computed_from_the_roots_and_provenance_is_a_set.sql`;
`TestAuditWV3_AFloorCannotDependOnTheOrderParentRowsWereInserted`,
`TestAuditWV3_AParentRowStillCannotBeWrittenAfterTheMint`.

**Amended 2026-09-11 (fourth withdrawal audit, F-283).** The recursion's answer
for a cycle is NULL, and this decision's backfill read that as "no parents".

The UNION makes the recursion terminate on a cycle rather than recurse, which is
what 00819's comment promises. What it terminates with is nothing:
`cp_credit_lot_root_origins` aggregates the reachable nodes that are nobody's
child, and in a cycle every node is somebody's child. The backfill then read that
through `coalesce(..., ARRAY[the lot's own origin])` — the mint-time rule for a
lot with no parents, applied to a lot that HAS them — which for trading proceeds
out of a promotional grant writes root_origins = {MARKET_TRADING_PROCEEDS} and
floors it there. That is the permissive direction, in the migration whose whole
subject is that a floor must be conservative. The trigger path fails closed on
the same input with CREDIT_PARENT_ROOTLESS; only the backfill substituted.

Migration 00825 refuses to migrate while any lot has parent rows and no
computable root, and `cp_credit_lots_without_computable_roots()` names them. A
migration that cannot compute a provenance has two honest options — stop, or
write down a guess — and a guess about where value came from is the one thing
this model may never make. There is no automatic repair, because every automatic
one would have to choose which edge of the cycle is the lie.

The case is reachable only in data restored from a deployment that ran on 00816
to 00818, where `cp_credit_lot_parent_has_no_descendant_yet` did not exist: this
decision's own ordering rule is what makes a cycle unwritable now.

## D-138 — Provenance is a set, not a rank (2026-09-11, product goal §23, §54, F-275)

**Problem.** "The most restricted parent" is decided by
`CreditOrigin.Restriction()`: how restricted an origin is across the two policies
THIS BUILD ships, with the origin's own name breaking a tie. PURCHASED and
MARKET_TRADING_PROCEEDS both rank `OriginClosedSomewhere`, so a lot funded by one
of each records MARKET_TRADING_PROCEEDS — the name that sorts first — as its
floor, and `cp_credit_origin_floor_rank` agrees with it about an answer that is
arbitrary with respect to any policy.

B-02, the open blocker the whole payout model waits on, is written as two
questions: "is Nodal's closed-loop Credit float itself stored value requiring a
licence; may trading gains ever be withdrawn". The answer "the float is stored
value, the gains are not" is a policy that releases MARKET_TRADING_PROCEEDS and
closes PURCHASED. Under it that lot is released, and half of what funded it was
refused — the shape §23 forbids, reached without any trade being wrong, because
the floor column holds one origin and the provenance had two.

**Chosen.** `credit_lot_state.root_origins text[]`, trigger-maintained from the
provenance roots by the same recursive computation D-137 uses — one query, two
columns — and `Policy.Permits` releases a lot only when the policy releases the
lot's own origin AND every root. An empty root set is read as UNKNOWN_ORIGIN and
refuses, exactly as an empty floor does.

`origin_floor` stays as the single most restricted origin, for display and for
ordering, and the rank and its SQL mirror stay with it; the parity test is
unchanged. `Policy.RefusedRoot` names the first root a policy refuses, most
restricted first, because ORIGIN_NOT_PAYOUT_ELIGIBLE on a bucket of
MARKET_TRADING_PROCEEDS is an answer nobody can act on — and under a policy this
build does not ship, the origin to name is not always the one this build's rank
calls the most restricted.

**Why derived rather than chosen.** The alternatives were to compute the rank
from the PERSISTED policy, or to widen the rank so no two origins can tie.

Computing from the persisted policy makes a floor a function of the policy in
force when the lot was minted, so the same lot on two deployments carries two
floors, and a policy change silently re-ranks history. Widening the rank only
moves the tie: a rank is a total order over origins and a policy is not, so
whatever the ordering, some policy refuses an origin the ordering calls less
restricted. A set is the only form that can be conservative for a policy nobody
has written yet, because every root is asked rather than one of them being chosen
on the strength of today's two policies.

**Consequences.**

1. `Permits` refuses a lot whose root set the caller did not supply. Every lot the
   database returns carries one; a hand-built `PermitInput` in a test does not,
   and refuses — which is the same direction the floor already failed in.
2. Two reasons can now raise UNKNOWN_ORIGIN, so `Permits` de-duplicates its
   reason list. A decision record that hashed differently depending on how many
   inputs were wrong would be the defect the reason ordering exists to prevent.
3. `credit.Lot`, `eligibility.OriginHolding` and `eligibility.OriginBucket` carry
   the root set; `root_origins` and `refused_root` are additive optional fields
   on `WithdrawalOriginBucket`.
4. The rank is now used for DISPLAY and ORDERING only. Its parity with SQL still
   matters, because the floor is still written by the database and read by the
   API.

**Residuals.** A policy persisted through the approval path can still release an
origin no lot's root set contains, which is not a provenance question. And the
root set is computed at mint, like the floor: it is a property of what funded a
lot, so a later policy changes what the set MEANS and not what it holds. That is
the direction this decision exists to make safe.

**Evidence.** F-275. `internal/valuedomain/policy.go`,
`internal/valuedomain/originfloor.go`, `internal/credit/types.go`,
`internal/credit/repository.go`, `internal/eligibility/withdrawal.go`,
`migrations/00819_a_floor_is_computed_from_the_roots_and_provenance_is_a_set.sql`;
`TestAuditWV3_TheFloorIsTheMostRestrictedParentUnderThePolicyThatJudgesIt`,
`TestPolicy_PermitsReadsTheFloorAsWellAsTheOrigin`,
`TestIntegration_GoAndSQLAgreeOnHowRestrictedEveryOriginIs`.

## D-139 — A reserved payout that cannot be sent says so on the request, and is not failed (2026-09-11, product goal §19, §54, F-277)

**Problem.** F-263 made `Submit` refuse a request whose destination has stopped
being usable, inside the claim transaction and before the transition to
SUBMITTED, so the transaction rolls back and the request stays VERIFIED with its
value reserved. The shape is right and the silence is not. The request reads
VERIFIED, which reads as "on its way"; the refusal is an error the sweep logs for
an operator; and the only customer-facing mention was a field on the DELETE
response that disabled the destination, which is gone as soon as the page is. The
refusal's own words told the holder to "point it at a destination that can", and
no route re-points a payout.

**Chosen.** `payout_requests.blocked_reason` and `blocked_at` (00822), paired by
a CHECK so a reason without an instant cannot exist. `Submit` carries the reason
out of the rolled-back transaction and records it in one of its own, and the read
carries it so `GET /v1/payouts/{id}` and the Withdraw page can put the sentence
beside the Cancel control. The holder is told once, through an optional
`payout.Notifier` the wiring layer implements over the notification producer, as
PAYOUT_NEEDS_REVIEW. The refusal's words now say "cancel the payout to release
the Credits it reserved".

**Why derived rather than chosen.** Three alternatives.

*Reuse `failure_reason`.* It belongs to a request that FAILED, and
`readPayoutRequests` renders it as "your withdrawal failed". This one did not
fail; it is waiting for a decision only its holder can make, and saying otherwise
would be worse than saying nothing.

*Move the request to FAILED, or to a BLOCKED state of its own.* FAILED returns
the reservation on the strength of a fact the person can undo by registering a
destination, which is what F-263 deliberately refused to do. A new state is a new
node in a machine whose edges are a table in the schema, for a fact that is not a
state: the request is VERIFIED and reserved either way.

*Let the notification follower raise it.* The follower turns TRANSITION ROWS into
notifications, and this refusal deliberately writes none — writing one would mean
a same-state row, which 00815 exists to stop carrying facts. So the notice is
emitted by the domain in the transaction that records the reason.

*A route that re-points a payout at a different destination.* Not built. The
destination is what the quote, the fee and the provider idempotency key were all
computed against; changing it after the fact is a new request in everything but
name, and "cancel and ask again" is the honest version of it.

**Consequences.**

1. The write is conditional on the reason CHANGING. The sweep re-reads every
   reserved request on every pass, so an unconditional write would restamp
   `blocked_at` every fifteen seconds and emit a notification each time.
2. A deployment with no notifier still refuses, still records and still renders.
   The notice is how somebody who is not looking at the page finds out; it is not
   what makes the refusal safe.
3. `blocked_reason` survives cancellation. Why a payout could not be sent is part
   of its history.
4. Additive contract fields: `blocked_reason` and `blocked_at` on
   `PayoutRequest`.

**Residuals.** The reason is recorded when a SUBMIT is attempted, so a request
whose destination is removed on a deployment with no submission sweep running
carries no reason until the sweep next runs. The alternative — writing it onto
every affected request at the moment the destination is disabled — is a fan-out
inside a step-up-protected command, and it would still have to be re-checked at
submit, because a destination is not the only thing that can make a request
unsendable.

**Evidence.** F-277. `migrations/00822_a_refused_payout_says_why_where_its_holder_can_read_it.sql`,
`internal/payout/service.go`, `internal/payout/payout.go`,
`internal/httpapi/handlers_native.go`, `cmd/api/payoutsweep.go`,
`cmd/api/wire.go`, `apps/web/src/pages/withdraw/Withdraw.tsx`;
`TestIntegration_APayoutRefusedAtSubmitSaysWhyAndTellsItsHolder`,
`TestIntegration_ABlockedPayoutRecordsItsReasonWithNoNotifierWired`.

**Amended 2026-09-11 (fourth withdrawal audit, F-279).** Consequence 3 above —
"`blocked_reason` survives cancellation. Why a payout could not be sent is part
of its history" — is right about the RECORD and was taken as licence by the
readers.

`Request.Blocked()` asked only whether the string was non-empty, `toAPIPayout`
emitted the field on any state, and the Withdraw page rendered two present-tense
sentences with no state test. A cancelled request therefore said "this withdrawal
cannot be sent … its Credits are still reserved and still yours" beside a
Reserved figure of zero, under a State badge reading REJECTED, above a Cancel
control the page had already removed. So the record keeps the fact and the read
reports it only while it is true: `Blocked()` is the string AND the state,
VERIFIED being the only state in which the sentence is true, and the page renders
the panel under the same condition as the Cancel control rather than relying on
the API's discipline.

Migration 00823 adds the database's half, which 00822 left out: cp_app held
UPDATE on `(blocked_reason, blocked_at)` with no state predicate, and
`recordBlocked`'s own `AND state = 'VERIFIED'` was the only thing holding the
column. A BEFORE UPDATE trigger now refuses a write of a NEW reason onto a
request whose state has no outgoing edge in `payout_request_state_edges` —
terminal read from the state machine rather than listed a fourth time — and
refuses it with AD001 rather than silently affecting no rows, because a write
that quietly does nothing is how a caller comes to believe something was
recorded. The Go writer stays narrower than the database's floor (VERIFIED versus
non-terminal) on purpose: the schema refuses what can never be true, and the
service decides what it is willing to say.

## D-140 — A dispute the platform won thaws what its freeze reached (2026-09-11, product goal §54, F-278)

**Problem.** D-124 freezes a derived lot when a lot it was derived from is
DISPUTED or REVERSED. Nothing ever unfroze one. `SettleDerived`'s promotion
clause opens `st.finality = 'REVERSIBLE'` and a frozen lot is not; its freeze
clause opens "not already frozen" and a frozen lot is; and no other writer can
reach a lot with no `credit_fundings` row — `SettleFunding`, `UnfreezeFunding` and
`DisputeFunding` all key on that column, no operator route moves a lot's
finality, and `credit_lot_state` is a projection `cp_app` may only SELECT. So
`UnfreezeFunding` returned the card to its window and then to SETTLED, and the
earning derived from it stayed DISPUTED for ever: neither spendable nor
payout-eligible, with the eligibility page reporting FUNDING_NOT_SETTLED — the
reason whose own declaration says "waiting fixes it" (F-230, F-278).

**Chosen.** A third clause in `settleDerivedCandidates`: a derived lot at a
frozen finality it can legally leave, none of whose parents is still frozen,
moved to the finality `DerivedFinality(parents)` yields NOW — SETTLED when every
parent is payout-eligible, REVERSIBLE otherwise. Both are legal edges out of
DISPUTED. The frozen finalities the clause opens are computed from
`finalityTransitions` (`thawableFinalities()`), so the clause that selects a lot
and the table that decides whether the move is legal cannot come apart. The pass
counts thaws separately from promotions and freezes, and `cmd/api` logs all
three.

**Why derived rather than chosen.** Four alternatives.

*Leave it, as D-124's residual proposed.* The residual's reasoning was that
"un-freezing automatically would be a sweep deciding that somebody else's dispute
ended well". The sweep decides nothing of the kind: it reads what the LOTS say,
which is what its other two directions do. Whether the dispute ended well is
`UnfreezeFunding`'s answer, driven by an external event exactly as
`DisputeFunding` is. What was missing was that nothing carried that answer
downwards.

*Return the lot to the finality it was minted at.* A lot minted UNFUNDED cannot
go back: `finalityTransitions` has no DISPUTED → UNFUNDED edge, and adding one
would give this sweep a way to un-fund value. SETTLED and UNFUNDED are the same
answer to every reader that matters — `Spendable()` and `PayoutEligible()` admit
both — and SETTLED is the one that is true of a lot whose whole provenance has
finished moving.

*Thaw to REVERSIBLE always, and let the promotion clause finish.* It is one pass
slower and, worse, it is a claim: REVERSIBLE says "something external can still
take this back", and when every parent is payout-eligible that is false. The
finality a derived lot carries is the least final among its parents, and the thaw
answers the same question the mint does.

*An operator route that unfreezes a lot by hand.* A route that moves a lot's
finality with no event behind it is the thing this whole model exists to prevent,
and it would need a reviewer, a reason and an audit trail to do what the parents
already say.

**Consequences.**

1. A lot whose funding was REVERSED stays frozen for ever, and that is the
   design: REVERSED is terminal, so a reversed parent is frozen for ever, and the
   clause requires that no parent is frozen. Thawing its children would be the
   ledger releasing value it has already lost.
2. A lot with two frozen parents thaws only when both are resolved. The clause is
   about ALL of them, like every other direction of this sweep.
3. The thaw reaches one level per pass, as the freeze does: a grandchild thaws the
   pass after its parent.
4. The lot leaves the candidate predicate the moment it moves — to SETTLED, which
   neither the promotion clause nor the thaw clause opens, or to REVERSIBLE with a
   parent that is not payout-eligible, which the promotion clause does not open —
   so F-260's starvation property holds in the new direction. A batch of 100
   thaws, then the hundred-and-first, then nothing.
5. F-230's sentence is true again: for a dispute the platform wins, waiting does
   fix it.

**Residuals.** A lot frozen by a REVERSED parent is inaccessible in both
directions for ever, which is deliberate and is D-124's recorded residual rather
than a new one: taking value back from a third party who earned it and may have
spent it is a posting kind this ledger does not have. Nothing tells that holder
why, beyond FUNDING_NOT_SETTLED, and a reason that named the dispute would be
telling one customer about another's chargeback.

**Evidence.** F-278. `internal/credit/derived.go`, `cmd/api/creditsettle.go`;
`TestAuditWV4_ADerivedLotFrozenByADisputeIsNeverThawedWhenTheDisputeIsWon`,
`TestIntegration_ADisputeWonThawsTheEarningsDerivedFromIt`,
`TestIntegration_ALotFrozenByAReversedParentIsNeverThawed`,
`TestIntegration_AFrozenLotWithOneParentStillFrozenIsNotThawed`,
`TestIntegration_TheThawReachesMoreLotsThanItsBatchAndThenStops`,
`TestIntegration_TheThawClauseOpensOnlyTheEdgesTheTableHas`.

## D-141 — A payout's record carries the whole provenance it drew on (2026-09-11, product goal §23, §54, F-282)

**Problem.** `payout_allocations` records the origin and the floor (00820), and
D-138 moved the permission itself onto the root SET: a lot is released only when
the policy releases its own origin AND every origin its provenance bottoms out
in. The floor is the most restricted root, so two different sets share a floor
whenever they share a minimum — {CREATOR_EARNING} and {CREATOR_EARNING,
PURCHASED} both floor at CREATOR_EARNING. Under the policy B-02 can come back
with, one of those may leave and the other may not, and the record and
`GET /v1/payouts/{id}` reported them as one provenance.

**Chosen.** `root_origins text[]` on `payout_allocations` (00824), NOT NULL,
non-empty, CHECKed against the declared origins and with `origin_floor =
ANY(root_origins)`; backfilled from `credit_lot_state` by the argument 00820
already makes — a lot's provenance is fixed at mint and independent of insertion
order, so the set now is the set then. `credit.Allocation` carries the set the
units were taken from, `reserve` writes it, and `payout.ProvenanceSlice` folds by
(origin, floor, set) with the set on the contract as an additive optional field,
mirroring `WithdrawalOriginBucket`.

**Why derived rather than chosen.** Three alternatives.

*Join to `credit_lot_state` at read time.* The fact is not lost — `lot_id`
references `credit_lots` and a root set never moves — so a join answers it. But
00820's own reason for recording the floor is that this table is the only reader
that can answer "what actually left" after the lot has been consumed, and a
reader who has to know to join is a reader who will one day not. An auditor
reading the table alone would conclude there was one provenance where there are
two.

*Record only the set and drop the floor.* The floor is what a screen shows and
what an ordering sorts by, and 00819's CHECK makes it one of the roots. Dropping
a column that is a projection of another to avoid duplication would cost every
reader the display answer to save a hundred bytes.

*Leave the fold at (origin, floor) and report the set only on the allocation.*
Then the page and the record would disagree, which is the shape F-272 was.

**Consequences.**

1. A payout drawn on two provenances that share a floor is two slices, and the
   preview and the record still agree because both fold on the same key.
2. The withdraw page's provenance table is keyed on the whole identity and names
   what funded each slice, for the reason F-280 gives one surface along.
3. Additive contract field: `root_origins` on `PayoutProvenanceSlice`.
4. The column is a RECORD and not a control. What stops the wrong lot being
   reserved is the lot restriction (D-136, as amended by F-281).

**Residuals.** The set is copied at reservation rather than joined, so a lot whose
root set could change after reservation would leave the copy stale. Nothing can:
a parent row may only be written at mint (00816) and only while the lot is
nobody's parent (00819), so a root set is immutable once written. The copy is
stated to be a historical record for exactly that reason.

**Evidence.** F-282.
`migrations/00824_a_payout_records_the_whole_provenance_it_drew_on.sql`,
`internal/payout/provenance.go`, `internal/payout/repository.go`,
`internal/payout/service.go`, `internal/credit/types.go`,
`internal/httpapi/handlers_payout_destinations.go`, `openapi/openapi.yaml`,
`apps/web/src/pages/withdraw/Withdraw.tsx`;
`TestAuditWV4_TheAllocationRecordFoldsTwoRootSetsIntoOneProvenance`,
`TestAuditWV4_ThePreviewAndTheRecordAgreeAboutWhatLeaves`.
