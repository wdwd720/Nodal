# THREAT MODEL

Status: analysis fixed 2026-09-05 against the working tree of the same date (parallel build agents were still landing packages; the inventory is a snapshot). Method: STRIDE per threat actor (goal PART 156) plus explicit financial-abuse cases. Companion: `docs/security/SECURITY.md`.

**Truthfulness rule.** A row is **mitigated** only when the control and its test exist on disk today. Otherwise it is **designed** (contract fixed in `docs/architecture/*`, code absent) or **pending** (code present, test absent). Every reference is a package, test, migration or trigger that was verified on disk. Traceability rows are `R-<PART>-<n>` in `docs/build/REQUIREMENTS_TRACEABILITY.md`.

## 1. What we protect

| Asset | Where it lives | Loss mode |
|---|---|---|
| Customer asset entitlements | `ledger_balances` / journal (Postgres) | value moved, balance edited, double-spent |
| Wallet signing authority | wallet provider under a bounded delegated signer (design) | arbitrary transaction signed |
| Capital and risk authority | `capital_envelopes`, `risk_policies`, `capability_gates`, `kill_switches` | agent or attacker raises its own limits or enables live money |
| Identity and sessions | `users`, `identity_pii`, `sessions` | account takeover, PII exposure |
| Evidence | `audit_events`, `signing_decisions`, archive (design) | tampering hides a loss |
| Provider credentials and secrets | SecretRef targets, task roles (design) | exfiltration |
| Availability of controls | reconciliation, settlement, ledger posting, kill switches | a control is stopped by the thing it should stop |

## 2. What exists today (the boundary every table below is scored against)

| Layer | Implemented and tested | Code present, untested or partially tested | Absent |
|---|---|---|---|
| Identity, sessions, authorization | `internal/security`, `internal/auth` (+ `oidc`, `devidp`, `httpmw`, `pgstore`) | — | `cmd/api`, auth rate limiting, login-anomaly detection |
| Configuration and secrets | `internal/config` (PROD rules, SecretRef), `internal/observability` (redaction) | — | `aws-sm://` resolver, Terraform, IAM |
| Policy and authority | `internal/gates`, `internal/killswitch` (matrix, controller), `internal/valuation` policies | `internal/eligibility` (no tests), `killswitch` Postgres checker/store (no DB test) | `internal/risk` (testdata only), `internal/admin` |
| Money | `internal/money`, `internal/positions`, `internal/fees`, `internal/idempotency`, `internal/db` | `internal/capital` (types/patch/pnl/state machine tested; `Reserve`/`Consume`/envelope service untested), `internal/ledger` (validate/hash/verify; no poster), `internal/accounts`, `internal/assets` | `internal/funding`, withdrawal domain, buying-power engine |
| Events and evidence | `internal/event` (envelope, outbox, inbox, relay unit tests), `internal/audit` canonical JSON | `audit.PGWriter.Append` / `Verifier` (no DB test) | Merkle/KMS/WORM (`cmd/audit-worker`), security-event emitter |
| Execution and agents | `internal/provider` health tracker | — | `internal/{signing, wallet, execution, reconciliation, settlement, quote, instruments, intent, agent, strategy, model, prediction}`, every worker binary |
| Database | migrations 00001–00601: immutability triggers, `AGENT` CHECKs, per-table grants (`TestIntegration_ApplicationRolePrivileges`, `TestIntegration_Migrations`) | trigger behaviour beyond privileges exercised only by hand | production role bootstrap |
| Verification harness | unit, property, fuzz, migration and integration suites | — | `test/security`, `test/contract`, `test/e2e`; CI has never run (no remote, SB-004); `make security`/`make contract` pass vacuously |

### STRIDE legend and scoring

- **S**poofing (identity, provider, webhook), **T**ampering (data, transactions, code), **R**epudiation (evidence), **I**nformation disclosure (PII, secrets, positions), **D**enial of service (availability of controls and workers), **E**levation of privilege (agent → authority, operator → dual control, customer → tenant).
- Status vocabulary: **mitigated** = control + test on disk; **pending** = control on disk, test absent (or test on disk, wiring absent); **designed** = neither on disk, contract fixed.
- Residual risk is judged on the on-disk state, never on the design; a designed control contributes nothing to the score until it lands.

## 3. STRIDE by threat actor

Columns: threat · attack path · asset · existing control (verified reference) · residual · detection signal · owner/stage.

### 3.1 Customer (authenticated, honest-but-curious or malicious)

| Threat | Attack path | Asset | Existing control | Residual | Detection | Owner / stage |
|---|---|---|---|---|---|---|
| Elevation: IDOR on another customer's account, wallet, intent, order, position, strategy, agent, prediction, backtest, audit evidence | guess or enumerate UUIDv7 ids in `/v1` reads and writes | entitlements, PII | `security.RequireAccount` refuses non-owned accounts; break-glass grants no visibility (`TestRequireAccount_TenantIsolation`, `TestRequireAccount_BreakGlassNeverGrantsVisibility`); ids are UUIDv7 not sequential | **pending**: no HTTP handler exists, so nothing calls `RequireAccount` yet; ten resource types untested (R-092-1) | `ErrCrossTenant` must be audited (`cross_tenant_attempt` security event — emitter absent) | API team, Stage 4 |
| Tampering: reuse an `Idempotency-Key` with a different body to slip a second economic effect | replay POST with changed amount | reservations, orders | `idempotency.Store.Begin` returns `ErrKeyReuseConflict` on hash mismatch; PK on `idempotency_keys` (`TestIntegration_DifferentHashIsDeterministicConflict`, `TestIntegration_ConcurrentBegin_ExactlyOneAcquires`) | mitigated at the store; API mapping to `INVALID_IDEMPOTENCY_REUSE` pending | metric `duplicate_command_rejections` | mitigated (store) |
| Repudiation: deny having placed a trade | dispute after loss | evidence | `audit_events` per-stream hash chain, immutable (`migrations/00106`, `audit.PGWriter.Append`) | pending: no DB test of `Append`/`VerifyStream`; no Merkle/KMS/WORM | `make verify-audit` (verifier binary absent) | audit, Stage 13 |
| Spoofing: forge a principal via headers | send `X-User-Id`, `Authorization` | sessions | `httpmw.Session` ignores client-asserted identity (`TestSession_IgnoresClientAssertedIdentity`) | mitigated | none needed | mitigated |
| DoS: burst trade intents to exhaust provider budget | scripted `/v1/intents` | provider quotas | rate limit from persisted intent counts in risk kernel (`RISK_ORDER_RATE`) — designed; Redis limiter — designed | designed | `risk_rejections` | risk, Stage 3 |

### 3.2 Malicious strategy creator

| Threat | Attack path | Asset | Existing control | Residual | Detection | Owner / stage |
|---|---|---|---|---|---|---|
| Elevation: strategy text asks for a transfer, withdrawal, secret export or arbitrary program call | NL compile or SDK document with forbidden effect | wallet authority | DB CHECK `strategy_versions.effect_set <@ allowed[]` (`migrations/00500_strategies.sql`); `strategies.created_by_actor_type <> 'AGENT'` | designed: compiler EFFECT stage, `DeriveEffects`, corpus (STRATEGY_IR §3, §11); no `internal/strategy` on disk | `compile_attempts` rows with `EFFECT_FORBIDDEN` | strategy, Stage 9 |
| Tampering: mutate a compiled version after owner acceptance | UPDATE `strategy_versions` | evidence, effect set | `strategy_versions_guard` raises `ST001` on compiled-field change or delete | pending (trigger untested) | trigger error | strategy, Stage 9 |
| DoS: unbounded loop / self-spawning strategy | `EveryMS < 1000`, self-referencing signal, action enqueuing a trigger | worker capacity, provider spend | `agent_runs UNIQUE (agent_id, trigger_dedup_key)` (`migrations/00501`) | designed: STRUCTURAL checks, 256-node/8-depth limits, per-minute run limits, day budgets (STRATEGY_IR §7) | `strategy_evaluations`, `model_cost`, `tool_cost` | agent, Stage 10 |
| Elevation: sizing above policy ("use all funds") | `NotionalUSD` above envelope | capital | envelope CHECK `available + reserved + deployed <= allocation` (`migrations/00102`) | designed: RISK_COMPAT stage; risk kernel | `risk_rejections` | strategy/risk |
| Information disclosure: strategy reads another customer's wallet intelligence | dependency params naming a foreign wallet set | PII/positions | agent principal bound to one account (`TestProp_AgentNeverCrossesAccount`) | designed: ToolBroker scoping of tool params to the run's account | `tool_invocations` provenance | agent, Stage 10 |

### 3.3 Compromised model (poisoned, backdoored, or hijacked provider)

| Threat | Attack path | Asset | Existing control | Residual | Detection | Owner / stage |
|---|---|---|---|---|---|---|
| Elevation: model output attempts a value transfer | structured response with forbidden action | wallet authority | agent permission set is closed (`TestAgent_ExactPermissionSet`, `TestProp_AgentNeverGainsForbiddenPermissions`); `RAW_SIGN`/`TRANSFER_VALUE` are not permissions | the signing boundary IS unreachable from agent code and it is tested: `TestAgentTreesNeverImportAuthority` and `TestSigningImportedOnlyByExecutionBoundary`. This cell said the packages were absent and the test did not exist; both are present (F-111, corrected 2026-09-10) | `EFFECT_FORBIDDEN` rejections; `policy_violations` | agent/signing, Stages 9–10 |
| Tampering: model emits its own `Hash`, `Version`, `Lineage` | crafted compile response | provenance | designed: compiler overwrites these fields (STRATEGY_IR §4) | designed | `compile_attempts` | strategy |
| Elevation: model raises envelope or risk limits | output referencing authority fields | capital/risk | `capital_envelopes` and `capital_envelope_changes` CHECK `<> 'AGENT'`; `capital.EnvelopeService.requireAdministrator` rejects agents; `risk_policies.created_by_actor_type IN ('OPERATOR','SYSTEM','USER')` | pending: envelope service untested for agent rejection; `internal/risk` absent | `capital.envelope.changed` topic | capital/risk |
| DoS: model loop drives unbounded provider charges | repeated `CALL_MODEL` | spend | designed: persisted day budgets in `model_calls`, auto-pause after N failures (AGENT_RUNTIME §6–7) | designed | `model_cost`, `agent_pauses` | agent |
| Information disclosure: model context contains secrets | prompt assembled with credentials | secrets | ToolBroker resolves credentials server-side (design); `observability.Secret` never renders in logs | designed | prompt archive review | agent |

### 3.4 Compromised employee (operator, developer, or on-call)

| Threat | Attack path | Asset | Existing control | Residual | Detection | Owner / stage |
|---|---|---|---|---|---|---|
| Elevation: single operator activates live money | propose + approve + activate a gate alone | capital authority | `gates`: dual-control `gate:approve` held by no standing role; approver ≠ proposer; activator ≠ proposer and ≠ first approver for high-risk; 15-min step-up (`TestRules_SamePrincipalCannotApproveAndActivate`, `TestDistinctApprovers_ExcludesProposer`, `TestGoldenMatrix_DualControlHeldByNoStandingRole`) | pending: gate store has no Postgres integration test; break-glass grant workflow (`internal/admin`) absent | `gate.transitioned` topic; `capability_activation` event (emitter absent) | gates/admin, Stage 3 |
| Tampering: edit a balance or delete a journal row | direct SQL as app role | entitlements | `cp_app` has no UPDATE/DELETE on journal tables, no default privileges (D-016), `ledger_balances` only via SECURITY DEFINER trigger (`TestIntegration_ApplicationRolePrivileges`, `TestIntegration_Migrations/role separation`) | pending: the migrate role can still mutate; `ledger.VerifyBalances` exists but is unscheduled and untested; production role bootstrap pending Terraform | `ledger_integrity_violation` (designed), `reconciliation_mismatches` | ledger/infra |
| Repudiation: hide an admin action | act without a reason or outside the audit stream | evidence | `admin_actions.reason` length ≥ 8; transitions immutable (`migrations/00153`); gate and kill transitions append audit events in the same transaction | pending: `internal/admin` absent; audit chain untested against Postgres | audit chain verification | admin/audit |
| Elevation: self-approve a dual-control action | approve own proposal with break-glass | authority | `RequireDualControl` refuses `proposerSubjectID == SubjectID` (`TestRequireDualControl`); DB CHECK `approved_by_user_id <> proposed_by_user_id` | mitigated at both layers for the check itself; end-to-end pending | `admin_privilege_use` event (absent) | admin |
| Elevation: keep break-glass forever | reuse an expired elevation | authority | `BreakGlassActive` compares `now < BreakGlassUntil` (`TestBreakGlass_Expiry`); `sessions.break_glass_until` OPERATOR-only (`00011`) | mitigated | session listing shows elevation | mitigated |
| Information disclosure: developer reads secrets from logs or config dumps | grep logs, print config | secrets | redaction denylist and value patterns (`TestRedaction_DenylistKeys`, `TestRedaction_ValuePatterns`); `Config.Hash` excludes secrets (`TestHash_StableAndExcludesSecrets`) | mitigated for the logger; struct dumps bypass it by design (log through attributes) | none | mitigated (logger) |
| Elevation: kill switch abused to hide fills or stop reconciliation | activate `GLOBAL_NEW_RISK_KILL` during an incident | evidence, entitlements | `OBSERVE/SETTLE/RECONCILE/LEDGER_POST/CANCEL` never blocked (`TestProp_NeverBlockedClasses`, `TestProp_GlobalKillNeverStopsRiskReduction`) | mitigated in the matrix; reconciliation engine absent so "still runs" is unproven end-to-end | `killswitch.changed`, `global_kill` event | killswitch (matrix mitigated) |

### 3.5 Compromised provider

| Provider | Threat | Attack path | Existing control | Residual | Detection | Owner / stage |
|---|---|---|---|---|---|---|
| Route provider (execution router) | Tampering: returns a transaction with extra transfer, delegate approval, authority change, unknown program, or Token-2022 extension | crafted swap response | `assets.token_extensions` + `HasUnsupportedExtensions()`; `signing_decisions` immutable table | **designed**: inspector with 16 checks and `FuzzInspect` (EXECUTION.md §2, R-034-1..3); `internal/signing` absent | `signing_rejection` event; `unknown_submissions` | signing, Stage 6 |
| Wallet / signing provider | Spoofing/Tampering: signs outside policy, or provider policy cannot resolve ALT accounts (documented limitation) | provider-side policy bypass | `wallets.delegation_verified_at`; production start must fail closed if unverified (PART 96) | designed; provider policy is defence in depth only — our inspector is the control | reconciliation of wallet activity vs attempts (`SUBMISSION_UNKNOWN` records) | wallet, Stage 6 (EB-002, EB-005) |
| Funding provider | Spoofing: forged or replayed webhook; Tampering: reversal after trading | POST to webhook endpoint | `provider_events UNIQUE (provider, provider_event_id)` + `signature_verified` column; inbox PK `(source, message_id)` (`event.Inbox.Process`, unit-tested only); `deposits UNIQUE (provider, provider_session_id)` | designed: signature verifier, deposit state machine, deficit posting (`TestProp_DuplicateWebhookOneEffect`, `TestFunding_ReversalCreatesDeficitAndFreezes` absent) | `webhook_signature_failed`, `provider_duplicate_events`, `negative_deficit_accounts` | funding, Stage 5 (EB-003) |
| Data provider (chain observer, market data) | Tampering: false finality or stale prices; Information: feeds prompt injection via token metadata | poisoned observation | `provider.Tracker` health with hysteresis (`TestTracker_DegradesImmediatelyRecoversWithHysteresis`, `TestTracker_DisableOverridesObservation`); `valuation` staleness (`TestPriceStore_Latest_NonPositiveMaxAgeIsStale`) | designed: agreement policy (require both observers for `FINALIZED`, block on disagreement), `RISK_STALE_DATA`, untrusted-content segment | `reconciliation_mismatches`, `data_freshness` | execution/reality, Stages 6–8 |
| Model provider | see 3.3 | | | | | |
| Identity provider | Spoofing: issues tokens for the wrong subject or with `alg=none` | compromised IdP | issuer/audience/signature/nonce/expiry checks; `alg=none` rejected (`TestExchange_AlgNoneRejected`, `TestExchange_IssuerMismatch`) | a fully compromised IdP can still mint valid tokens: step-up and dual control limit blast radius | `login_anomaly` (absent) | auth (EB-017) |

### 3.6 Wallet attacker (on-chain adversary)

| Threat | Attack path | Asset | Existing control | Residual | Detection | Owner / stage |
|---|---|---|---|---|---|---|
| Tampering: unsolicited token/airdrop or dust alters balances | send tokens to platform wallets | ledger truth | `RECONCILIATION_ADJUSTMENT` account may go negative; `WALLET` may not (`ledger_apply_entry` LG001) | designed: wallet-balance reconciliation with dust threshold (RECONCILIATION §4); engine absent | `reconciliation_mismatches` | reconciliation, Stage 7 |
| Elevation: transaction with `Approve`/`SetAuthority` slipped into a route | malicious program in route | wallet authority | none on disk | designed: `NO_AUTHORITY_CHANGE`, `NO_ARBITRARY_CPI` checks | `signing_rejection` | signing, Stage 6 |
| Repudiation/DoS: reorg removes a `CONFIRMED` transaction | chain reorganisation | ledger truth | ledger posts at `CONFIRMED` (design); `fills` economic fields immutable (`fills_guard`) | designed: reorg opens `MISMATCH` and compensating postings (EXECUTION §5) | `reconciliation_mismatches` | reconciliation |
| Tampering: unknown transaction from a platform wallet | stolen provider signer | entitlements | `execution_attempts.tx_signature UNIQUE` | designed: periodic wallet-activity scan opens `SUBMISSION_UNKNOWN`, blocks new risk, SEV1 (RECONCILIATION §3) | `unknown_submissions` | reconciliation |

### 3.7 Account-takeover attacker

| Threat | Attack path | Asset | Existing control | Residual | Detection | Owner / stage |
|---|---|---|---|---|---|---|
| Spoofing: stolen session cookie replay | XSS elsewhere, malware, network | sessions | `HttpOnly`, `Secure`, `__Host-`, `SameSite=Lax` (`TestCookieHelpers`); idle timeout 30 min; absolute expiry never extended (`TestManager_Expiry`, `TestManager_IdleTimeoutAndTouch`); revoke one/all (`TestManager_Revoke`, `TestManager_RevokeAllForSubject`) | no device binding or IP-change detection in V1; no auth-endpoint rate limiting on disk | `session_revoke`, `login_anomaly` events (absent) | auth, Stage 4 |
| Tampering: cookie or token tampering | edit token bytes | sessions | tokens are opaque; store keyed by `sha256`; non-canonical tokens never reach the store (`TestProp_ValidateTokenOnlyAcceptsCanonical`, `TestManager_MalformedTokenNeverHitsStore`, `TestSession_BadCookiesAreAnonymousAndCleared`) | mitigated | none needed | mitigated |
| Spoofing: PKCE/nonce/state bypass on login | intercept authorization code | identity | PKCE S256, nonce bound to ID token (`TestExchange_PKCEMismatch`, `TestExchange_BadNonce`) | state/CSRF binding of the callback is `cmd/api` work (absent) | `login_anomaly` | auth |
| Elevation: perform a sensitive action without a second factor | reuse an old password-only session | authority | `RequireStepUp` demands strong AMR within `maxAge` (`TestRequireStepUp`, `TestHasStrongAMR`); gates/kill release use 15 min | mitigated for the primitive; withdrawal and envelope changes must call it (services absent) | `STEP_UP_REQUIRED` responses | auth/admin |
| Tampering: CSRF on state-changing endpoints | cross-site form post | orders, funding | `httpmw.CSRF` (`TestCSRF_Matrix`) | mitigated | 403 rate | mitigated |
| Elevation: attacker with a customer session withdraws | `withdrawal:create` | entitlements | `WITHDRAWALS` gate DISABLED; `withdrawal:approve` is dual-control; step-up designed | designed: withdrawal domain absent (R-094-1, EB-015) | `capability_gate_rejections` | funding, deferred |

### 3.8 Supply-chain attacker

| Threat | Attack path | Asset | Existing control | Residual | Detection | Owner / stage |
|---|---|---|---|---|---|---|
| Tampering: malicious Go module or transitive dependency | typosquat, maintainer compromise | every binary | `go.mod`/`go.sum` pinned; `go mod verify` in `build/Dockerfile`; CI `govulncheck`, `trivy fs`; SBOM via `syft` | CI has never executed (no remote, SB-004); no runtime SBOM diff; "dependency compromise simulation" absent (R-155-3) | trivy/govulncheck findings | tooling, now |
| Tampering: compromised build tool binary | poisoned release asset | CI | `scripts/tool` verifies checksums and fails closed (`TestInstallReleaseTool_ChecksumMismatch`, `TestInstallReleaseTool_APIErrorFailsClosed`, `TestInstallReleaseTool_UnverifiableRequiresFlag`) | mitigated for pinned tools | install failure | mitigated |
| Tampering: unsigned or substituted container image | registry compromise | deployment | `release.yml`: trivy image scan, keyless cosign signature via OIDC, SBOM attestation, SLSA provenance | designed at deploy side: no admission policy verifying signatures (Terraform absent) | cosign verify (not wired) | infra (EB-012) |
| Information disclosure: secret committed to git | developer mistake | secrets | gitleaks over full history in CI; `.gitleaks.toml` allow-lists only `cp_*_local` | CI not running | gitleaks job | tooling |
| Tampering: test fake wired into production | import of `eventtest`, `devidp`, `testkit` | fail-closed guarantees | depguard `no-test-fakes-in-production`, lintfin rule 2 (`TestIsTestOnlyImport`); `NO_FAKE_PROVIDERS` config rule (`TestValidate_NoFakeProvidersCoversEverySlot`) | mitigated at lint and config; runtime wiring (`cmd/*`) absent | lint failure | mitigated (lint/config) |

### 3.9 API attacker (unauthenticated or low-privilege network adversary)

| Threat | Attack path | Asset | Existing control | Residual | Detection | Owner / stage |
|---|---|---|---|---|---|---|
| Information disclosure: stack traces or internal causes in errors | trigger 500s | internals | `errs.ToProblem` never exposes causes (`TestToProblem_NeverExposesCauseForAnyCode`, `TestToProblem_RedactsInternal`) | mitigated | none | mitigated |
| DoS: oversized bodies, slowloris | large POST | availability | `HTTP.MaxBodyBytes`, read/write/idle timeouts required > 0 (`TestValidate_FieldRulesApplyEverywhere`) | server wiring absent; WAF/ALB pending Terraform | edge metrics | api/infra |
| Spoofing: trusted-proxy header abuse to fake client IP | forged `X-Forwarded-For` | audit accuracy, rate limits | `HTTP.TrustedProxyCIDRs` validated as CIDRs | middleware absent | none | api |
| Tampering: SSRF through a URL-taking endpoint | user-supplied URL | internal network | no URL-taking endpoints designed; ToolBroker egress allow-list, no generic HTTP tool (ADR-0011) | designed; SSRF test absent | egress denials | agent, Stage 10 |
| Tampering: malformed JSON / numbers as floats | crafted payloads | exact money | money decoded from strings only (`TestUSD_JSON_Unmarshal`, `TestQuantity_JSON_Unmarshal`, fuzzers); canonical JSON rejects non-integer numbers (`TestCanonicalJSON_RejectsNonCanonicalNumbers`) | mitigated at the type layer; oapi-codegen strict server absent | 400 rate | mitigated (types) |
| Elevation: CORS wildcard or permissive origin | browser cross-origin call | sessions | `NO_CORS_WILDCARD` in STAGING/PROD; origins must be `scheme://host[:port]` | mitigated (config) | none | mitigated |

## 4. Financial abuse cases

| # | Case | Path | Existing control | Status | Required before "mitigated" |
|---|---|---|---|---|---|
| F1 | Duplicate execution via retry after timeout | submit times out; caller retries; two transactions land | `execution_attempts.tx_signature UNIQUE`; `UNIQUE (order_id, attempt_no)`; reservation `LockForOrder` keeps the reservation (`capital.Service.LockForOrder`, untested) | designed (EXECUTION §4 steps 1–8; no executor) | crash test PART 49; submission-unknown recovery tests (found/adopted, proven absent, disagreement) |
| F2 | Reservation oversubscription under contention | N concurrent reserves against one balance | `asset_reservation_totals` `SELECT … FOR UPDATE` serialises per (account, asset); envelope row lock; `db.Serializable` retry (`TestIntegration_Serializable_ConcurrentWritersRetryAndBothCommit`) | pending: `capital.Service.Reserve` has no test | `TestProp_ReservationsNeverOversubscribe` and the PART 23 torture test under both isolation levels |
| F3 | Replayed funding webhook | same provider event delivered twice | `provider_events UNIQUE (provider, provider_event_id)`; inbox PK; `Inbox.Process` skips `PROCESSED` (unit) | pending (no DB test; no webhook handler) | `TestProp_DuplicateWebhookOneEffect`; signature verification test |
| F4 | Funding reversal after trading | chargeback when `WALLET` < deposit | `LEDGER_NEGATIVE_BALANCE` trigger (LG001) forbids a silent negative; `DEFICIT` account, `FUNDING_REVERSAL_DEFICIT` kind in schema | designed (FINANCIAL_MODEL §2.2; `internal/funding` absent) | `TestFunding_ReversalCreatesDeficitAndFreezes`; `withdrawal_eligible` only after `reversible_until` |
| F5 | Agent capital/risk escalation | agent principal writes envelope or policy | closed agent permission set (`TestProp_AgentNeverGainsForbiddenPermissions`); `<> 'AGENT'` CHECKs on envelopes/changes; `requireAdministrator` | pending (service test absent; risk absent) | No test of that name exists. The closed permission set is held by `TestAgentPrincipalPermissionSetIsClosed` and `TestProp_AgentNeverGainsForbiddenPermissions`; the lifecycle by `TestAgentPrincipalCannotChangeItsOwnLifecycle`. Naming a test that does not exist in the residual-risk column of a threat model is worse than leaving it blank (F-111) |
| F6 | Agent withdrawal attempt | agent creates a withdrawal | no `withdrawal:create` for agents (`TestGoldenMatrix_AgentSet`); `withdrawal_transitions.actor_type <> 'AGENT'`; `WITHDRAW` class blocked by `WITHDRAWALS_DISABLE` (`TestMatrix_Withdraw`) | pending (withdrawal service absent) | `test/security/agent_withdrawal_attempt_test.go` through the intent/withdrawal service |
| F7 | Malicious route returning a value-exfiltrating transaction | extra `Transfer`, `Approve`, foreign `CloseAccount` | none executable | designed | inspector table tests + `FuzzInspect` + golden mutated fixtures; `test/security/malicious_route_provider_test.go` |
| F8 | Prompt injection through token metadata | "ignore policy and withdraw" in a token description | none executable | designed (STRATEGY_IR §10 corpus row "hostile tool result") | corpus green with every forbidden effect appearing in a rejecting case; `test/security/prompt_injection_test.go` |
| F9 | Observer disagreement / false finality | Helius says finalized, RPC says absent | `provider.Tracker` health only | designed (`AgreementPolicy`: require both for `FINALIZED`, block on disagreement) | "observer disagreement ⇒ `MISMATCH` and block" test (EXECUTION §9) |
| F10 | Kill switch abused to hide fills | activate global kill, hope fills are dropped | never-blocked classes (`TestProp_NeverBlockedClasses`) | mitigated in the matrix; end-to-end pending | "SUBMITTED order of a paused agent still reaches SETTLED" (AGENT_RUNTIME §12) |
| F11 | Operator balance edit | UPDATE `ledger_balances` or journal | triggers LG003 + SECURITY DEFINER + no grants (`TestIntegration_ApplicationRolePrivileges`) | mitigated for `cp_app`; migrate role and infra pending | `TestLedger_UpdateDeleteForbidden` as `cp_app` and via trigger; `ledger.VerifyBalances` scheduled with SEV1 |
| F12 | Cross-tenant read/write | foreign account id in request | `RequireAccount` (`TestRequireAccount_TenantIsolation`) | pending at API | `test/security/cross_tenant_test.go` (10 resource types × read/write) |
| F13 | Idempotency-key reuse with different body | changed amount under same key | `ErrKeyReuseConflict` (`TestIntegration_DifferentHashIsDeterministicConflict`) | mitigated (store) | API mapping test to `INVALID_IDEMPOTENCY_REUSE` |
| F14 | JWT / cookie tampering | altered ID token or cookie | OIDC negative tests; opaque hashed session tokens | mitigated | none beyond D-014 follow-up (pin `go-jose`) |
| F15 | Stolen session replay | exfiltrated cookie | cookie flags, idle/absolute expiry, revocation | partially mitigated; no anomaly detection or auth rate limit | `test/security/stolen_session_test.go`; `login_anomaly` events; auth rate limiter |
| F16 | Dependency compromise | poisoned module | pinned sums, `go mod verify`, scanners in CI | pending (CI never run) | connect remote; green `security-scans` job; SBOM diff on release |

## 5. The two highest-value attack paths, step by step

Each step names the control that must stop it and its state today. An attacker needs every step to succeed; the platform needs any one control to hold.

### 5.1 Objective: move customer value off the platform path

| Step | Attacker needs | Blocking control | State |
|---|---|---|---|
| 1 | a principal that can create an intent | agent principal or customer session; agents hold `intent:create_agent` only on one account (`TestProp_AgentNeverCrossesAccount`) | mitigated |
| 2 | an intent of a forbidden kind (transfer, withdrawal) | `intent.Action` is a closed enum; `WITHDRAW` is a separate domain requiring `withdrawal:create` + step-up + `WITHDRAWALS` gate | designed (`internal/intent` absent; gate DISABLED) |
| 3 | eligibility and risk to pass | `eligibility.Evaluate` fails closed on unknown state; risk kernel `RISK_*` reasons | pending (eligibility untested) / designed (risk absent) |
| 4 | capital beyond the reservation | `asset_reservation_totals` row lock; envelope allocation CHECK | pending (`Reserve` untested) |
| 5 | a plan step that is not a swap on an allow-listed venue | settlement compiler emits `NO_VALID_PLAN` otherwise; `execution_plans_guard` freezes approved plans | designed |
| 6 | a transaction with a foreign destination, delegate or authority change | inspector checks `NO_UNEXPECTED_DESTINATION`, `NO_AUTHORITY_CHANGE`, `NO_SYSTEM_TRANSFER`, `NO_ARBITRARY_CPI` | designed (`internal/signing` absent) |
| 7 | the signing service to trust the caller's expectations | `Service.Sign` rebuilds `Expectations` from persisted plan/quote/risk/reservation | designed |
| 8 | the wallet provider to sign anyway | provider policy allow-list (defence in depth; cannot resolve ALT accounts) | designed (EB-005) |
| 9 | the loss to go unnoticed | wallet-activity reconciliation opens `SUBMISSION_UNKNOWN`, blocks new risk, SEV1 | designed |
| 10 | the evidence to disappear | immutable `signing_decisions`, hash-chained `audit_events`, WORM archive | pending (chain untested) / designed (WORM) |

Today steps 1 and 10 (partially) hold; steps 2–9 rest on code that is not on disk. This is residual risk #1.

### 5.2 Objective: turn live money on without authority

| Step | Attacker needs | Blocking control | State |
|---|---|---|---|
| 1 | config that enables the capability | `config.Capabilities.Enabled` is condition 1 of 5 (`TestEvaluate_ConfigAloneNeverActivates`) | mitigated |
| 2 | a gate row in `ACTIVE` | only `gates.Admin.Activate` writes it; agents/services rejected before any query (`TestAdmin_AgentRejectedBeforeAnyQuery`) | mitigated (unit) / pending (no DB test) |
| 3 | an approval chain | two distinct approvers, none the proposer; high-risk: activator ≠ first approver (`TestDistinctApprovers_ExcludesProposer`, `TestRules_SamePrincipalCannotApproveAndActivate`) | mitigated |
| 4 | `gate:approve` | held by no standing role; only a live `BREAK_GLASS` elevation (`TestGoldenMatrix_DualControlHeldByNoStandingRole`, `TestBreakGlass_Expiry`) | mitigated |
| 5 | a break-glass grant | `admin_actions` kind `BREAK_GLASS_GRANT`, dual-controlled, step-up, expiry, notification | designed (`internal/admin` absent) |
| 6 | step-up within 15 minutes for each approver | `RequireStepUp` with strong AMR (`TestRequireStepUp`) | mitigated |
| 7 | evidence references | `MissingEvidence()` refuses activation for LIVE_* and WITHDRAWALS without legal, contract, risk and security refs | mitigated |
| 8 | a direct database write to `capability_gates` | ~~`cp_app` may UPDATE `capability_gates`~~ | **CLOSED by migration 00701** (corrected 2026-09-10, F-111). `REVOKE UPDATE ON capability_gates FROM cp_app; GRANT UPDATE (version)` — confirmed against the live catalogue, where cp_app's only updatable column is `version`. `cp_app` also holds no INSERT on `capability_gate_transitions`; the sole writer is `cp_gate_transition`, a SECURITY DEFINER function with a pinned `search_path` that re-derives the rules from the stored row and requires three distinct actors. `cp_gate_born_disabled` refuses a gate born in any other state. This is now the best-protected table in the schema, and the fix that F-109 applied to four money tables |
| 9 | the change to go unnoticed | `gate.transitioned` topic; `capability_activation` CRITICAL event | designed (emitter absent) |

Step 8 was a design gap surfaced by this review, and migration 00701 took the second of the two options it proposed: state changes moved behind a `SECURITY DEFINER` function, and `cp_app` lost UPDATE on the table. `test/integration/migrations` covers it. Corrected 2026-09-10 (F-111) — this paragraph asked for work that had been done.

## 6. Detection and response mapping

| Security event kind (PART 130) | Threat rows | Severity / alert | Runbook (PART 157) | State |
|---|---|---|---|---|
| `login_anomaly` | 3.7 stolen session, IdP compromise | WARN, rate alert | admin compromise | designed |
| `mfa_change` | 3.7 | INFO | admin compromise | designed (delegated to IdP claims) |
| `session_revoke` | 3.7 | INFO | — | code path exists (`Manager.Revoke`), event absent |
| `admin_privilege_use` / break-glass use | 3.4 | HIGH, page | admin compromise | designed |
| `webhook_signature_failed` | 3.5 funding provider, F3 | HIGH, page on burst | funding provider compromise | designed |
| `provider_credential_change` | 3.5 | HIGH | wallet/funding provider compromise, secret exposure | designed |
| `capability_activation` | 3.4, 5.2 | CRITICAL, page | — (should never be routine) | audit stream only |
| `global_kill` | 3.4, F10 | CRITICAL, page | every outage runbook | audit stream only |
| `signing_rejection` | 3.5 route provider, 3.6, F7 | CRITICAL, page: a rejected signing request means a route or plan produced a forbidden transaction | duplicated trade suspicion, unknown transaction | designed |
| `wallet_policy_violation` | 3.5 wallet provider | CRITICAL | wallet provider compromise | designed |
| `cross_tenant_attempt` | 3.1, F12 | HIGH, rate alert | admin compromise | `ErrCrossTenant` exists; event absent |

None of the runbooks exist (`docs/runbooks/` absent; R-157-1, R-157-2). The metrics that exist today and can be alerted on immediately once a collector is wired: `capability_gate_rejections`, `risk_rejections`, `duplicate_command_rejections`, `provider_duplicate_events`, `unknown_submissions`, `reconciliation_mismatches`, `oldest_unresolved_mismatch`, `negative_deficit_accounts`, `policy_violations`, `agent_pauses`.

## 7. Assumptions and out of scope

- **Trust assumptions**: the identity provider verifies factors honestly (compromise is bounded by step-up and dual control, not eliminated); AWS KMS, RDS and S3 Object Lock behave as documented; the Go toolchain and pinned module checksums are trustworthy; the wallet provider's TEE keeps keys but its policy engine is not relied upon.
- **Out of scope for V1** (goal PARTS 2–4, ADR-0017, ADR-0018, ADR-0019): withdrawals and off-ramp (gate DISABLED), cross-chain, prediction markets, securities, centralised exchanges, marketplace, arbitrary user code (ADR-0011), multi-region failover, a proprietary stablecoin, internal order crossing.
- **Not modelled here**: physical security of developer machines, social engineering of the identity provider's support desk, and legal exposure (tracked as EB-001…EB-017 in `docs/build/BLOCKERS.md`).
- **Review cadence**: this document is re-scored at every stage exit and whenever a row of §3 or §4 changes state; a change from mitigated back to pending (a deleted test) is a release blocker.

## 8. Top ten residual risks (prioritised)

Each becomes "mitigated" only when the named control **and** its test exist on disk and pass in CI on a fresh database.

1. **No signing boundary or inspector exists** — every route-provider and wallet-attacker row is open (§5.1).
   - Control: `internal/signing/inspect` (16 checks) and `signing.Service.Sign` re-loading plan/quote/risk/reservation from Postgres, one `signing_decisions` row per call; depguard proving `signing` is importable only by `cmd/execution-worker`.
   - Test: one rejecting table test per check, `FuzzInspect`, golden mutated fixtures, `test/security/malicious_route_provider_test.go`, `test/security/agent_raw_sign_attempt_test.go`.
2. **Reservation and ledger correctness are unproven** — `capital.Service.Reserve` and the ledger poster have no tests; triggers were exercised only by hand.
   - Control: `ledger.Service.Post` (absent), `capital.Service.Reserve` (untested).
   - Test: `TestProp_ReservationsNeverOversubscribe`, the PART 23 torture test under read-committed and serializable, `TestProp_PostingBalanced`, `TestLedger_UpdateDeleteForbidden` as `cp_app`, `TestLedger_CorrectionIsNewTransaction`, `TestReservation_ConsumeAfterReleaseRejected`.
3. **Tenant isolation is a primitive nobody calls** — no `cmd/api`, no handlers.
   - Control: every handler calls `security.RequireAccount` before touching state; cross-tenant failures emit `cross_tenant_attempt`.
   - Test: `test/security/cross_tenant_test.go` covering account, wallet, intent, order, position, strategy, agent, prediction, backtest and audit evidence, read and write; `test/security/idor_test.go`.
4. **Funding replay and reversal paths are unimplemented** — inbox has unit tests only; no signature verifier, no deposit state machine, no deficit posting.
   - Control: webhook signature verification writing `provider_events.signature_verified`; `funding.Transitions`; deficit posting per FINANCIAL_MODEL §2.2.
   - Test: `TestProp_DuplicateWebhookOneEffect` against Postgres, `test/security/webhook_forgery_test.go`, `TestFunding_ReversalCreatesDeficitAndFreezes`.
5. **Agent authority boundary has never fired** — depguard and lintfin rules exist but no agent, strategy or model package exists to violate them.
   - Control: effect system, ToolBroker, prompt segments, intent service accepting AGENT intents only with `CREATE_TRADE_INTENT` in the version's effect set.
   - Test: `TestAgentTreesNeverImportAuthority` and `TestSigningImportedOnlyByExecutionBoundary` (both exist), the STRATEGY_IR §11 corpus with every forbidden effect in a rejecting case, an egress allow-list test that fails closed, `test/security/prompt_injection_test.go`. The two names this line used to carry — `TestAgentImportBoundary` and `TestAgentPrincipalCannot*` — have never existed (F-111).
6. **Security eventing is blind** — `security_events` and the `security.event` topic exist, no code writes them, `kind` is unconstrained.
   - Control: `security.EventKind` enumeration of the eleven PART 130 kinds; emitter wired into `RequireAccount` failures, signing rejections, gate activation, global kill, webhook failures, session revocation; alert routing for CRITICAL.
   - Test: `test/integration/security_events_emitted_test.go` (eleven kinds), golden test binding kinds to severities.
7. **Evidence chain is untested against the database and has no WORM tail** — `audit.PGWriter.Append` and `Verifier.VerifyStream` have no integration test; Merkle, KMS and Object Lock are absent.
   - Control: `cmd/audit-worker` (Merkle root, KMS signature, archive), `make verify-audit` producing a real report.
   - Test: concurrent appends to one stream verify with no fork or gap; restore-and-verify drill from the archive.
8. ~~**Gate state can be flipped by a direct table write**~~ — **CLOSED by migration 00701**, corrected 2026-09-10 (F-111). Listing a closed control among the top ten residual risks understates the system in a document a reviewer uses to score its posture, which is a truthfulness defect in the same way an overstatement is.
   - Control: trigger or `SECURITY DEFINER` function refusing a `state` change without a same-transaction `capability_gate_transitions` row; `Checker` cross-checking the latest transition.
   - Test: gate integration test attempting a bare `UPDATE … SET state = 'ACTIVE'` as `cp_app` and expecting failure; `TestIntegration_ApplicationRolePrivileges` extended.
9. **CI is vacuous where it matters and has never run** — `make security`, `make contract` and `make iac-scan` pass on empty directories; no git remote.
   - Control: targets fail on empty directories; remote connected; green `security-scans` and `integration` jobs required before any gate leaves `DISABLED`.
   - Test: the CI run itself, plus `scripts/maketargets` asserting each target names an existing directory.
10. **Kill-switch guarantees and infrastructure are design-only past the matrix** — the reconciliation, settlement and execution code that must keep running does not exist; no Terraform, IAM task roles, private network, WAF, `aws-sm://` resolver or production DB-role bootstrap.
    - Control: `cmd/reconciliation-worker` and executor honouring never-blocked classes; `infra/terraform` modules; per-binary task roles; `aws-sm` resolver; D-016 default-privilege policy applied at bootstrap.
    - Test: fault-injected test where a `SUBMITTED` order under `GLOBAL_NEW_RISK_KILL` still reaches `SETTLED` and posts to the ledger; Postgres-backed `Checker.Check` seeing a switch activated before the authorizing transaction; `trivy config` producing and passing findings; a config test refusing PROD boot without a secret resolver.

Everything above is reversible in one direction only: a row moves from designed to pending when code lands, and from pending to mitigated when its named test passes in CI on a fresh database.
