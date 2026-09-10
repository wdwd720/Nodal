# SECURITY ARCHITECTURE

Status: design fixed 2026-09-05. **The on-disk inventory below is dated 2026-09-05 and much of it is now wrong in the UNDERSTATING direction — read it as a snapshot, not as current state (F-111, corrected 2026-09-10).** It was taken while parallel build agents were still landing packages, and the DESIGNED tags were accurate that afternoon. Since then, at least: `internal/signing` exists and is the most thoroughly built area of the system (sixteen inspector checks, every expectation re-derived from persisted rows); `internal/admin` exists with propose/approve/execute and a params-hash replay guard; `cmd/api` exists and is deployed; `infra/` holds 63 Terraform files across three environments; and every binary named in SYSTEM.md §2 is on disk. A DESIGNED tag in this document means "was not built on 2026-09-05" and nothing more. This matters because this is one of the two documents a reviewer would use to score security posture, and neither of them is machine-checked the way the build documents are; `test/docs/references_test.go` now checks the test names both of them cite. Every claim below is tagged either **IMPLEMENTED** (package and test exist on disk) or **DESIGNED** (contract fixed in `docs/architecture/*`, implementation pending; traceability row in `docs/build/REQUIREMENTS_TRACEABILITY.md`). The traceability document lags the tree: several rows it still marks `NOT_STARTED` (for example R-091-1, R-092-1, R-097-1, R-098-2, R-101-1, R-103-1, R-104-1, R-190-1, R-192-1) have code and tests today; this document reports what is on disk.

Companion: `docs/threat-model/THREAT_MODEL.md` (STRIDE and financial-abuse analysis, residual-risk list).

### How to reproduce the inventory

```
go test -count=1 ./internal/security/... ./internal/auth/... ./internal/config/... ./internal/observability/...
go test -count=1 ./internal/gates/... ./internal/killswitch/... ./internal/idempotency/... ./internal/audit/... ./internal/event/...
go test -count=1 -tags=integration ./test/integration/... ./internal/auth/pgstore/... ./internal/idempotency/...   # needs CP_TEST_*_DATABASE_URL
go run ./scripts/lintfin && go run ./scripts/tool golangci-lint run ./...
ls test/security test/contract infra docs/runbooks   # each is absent or empty today
```

## 1. Principles

1. **Fail closed.** Unknown environment, missing policy, unknown jurisdiction, missing gate row, absent grant, session-store outage, unparsable principal: every one of these is a refusal, never a default. IMPLEMENTED: `config.ParseEnvironment` (`TestProp_ParseEnvironmentFailsClosed`), `security.principalFor` (`TestRequire_InvalidPrincipalFailsClosed`), `httpmw.Session` 503 on store outage (`TestSession_StoreOutageFailsClosed`), `gates.Evaluate` (`TestEvaluate_EachConditionFailsWithDistinctReason`), `eligibility` reason `ELIGIBILITY_POLICY_UNKNOWN` (code on disk, no test yet).
2. **An agent is an untrusted proposal generator** (goal PART 9, ADR-0012). It holds a `security.Principal` with `ActorType = AGENT`, no roles, one bound account, and exactly `agent:run`, `prediction:commit`, `intent:create_agent`, `account:read`, `trade:read`, `strategy:read`. IMPLEMENTED: `internal/security/principal.go`, `roles.go` (`TestAgent_ExactPermissionSet`, `TestAgent_RolesNeverCount`, `TestProp_AgentNeverGainsForbiddenPermissions`, `TestProp_AgentNeverCrossesAccount`).
3. **No blind signing** (PART 34). Every transaction is decoded and inspected against the persisted approved plan before a bounded signing request is made. DESIGNED (R-034-1..3, R-095-1): no `internal/signing` package exists on disk.
4. **No balance editing.** The only financial repair is a compensating journal transaction (`LEDGER_CORRECTION` admin action). IMPLEMENTED at the database: `journal_transactions`/`journal_entries` `BEFORE UPDATE OR DELETE` triggers, `ledger_balances` maintained only by a `SECURITY DEFINER` trigger, `cp_app` has no UPDATE/DELETE on posted rows (`migrations/00101_ledger.sql`; `TestIntegration_ApplicationRolePrivileges`, `TestIntegration_Migrations/role separation`). The Go poster (`ledger.Service.Post`) is not yet on disk.
5. **Exact money.** No binary floating point in financial packages: `money.USD/BPS/Quantity/Price`, `forbidigo` + `scripts/lintfin` (rule 1), `TestNoFloatingPointInSource`, fuzzers `FuzzParseUSD`, `FuzzParseQuantity`, `FuzzScanQuantity`.
6. **One environment variable never enables live money.** Config is condition 1 of 5; a persisted dual-approved `ACTIVE` gate row inside its window with evidence is required. IMPLEMENTED: `internal/gates` (`TestEvaluate_ConfigAloneNeverActivates`, `TestChecker_NilEnabledFailsClosed`, `TestDistinctApprovers_ExcludesProposer`).

## 2. Trust boundaries and binaries

Design per `docs/architecture/SYSTEM.md` §2. Only `cmd/migrate` exists on disk today; every other binary is DESIGNED (D-003).

| Binary | Responsibility | May hold | Must never hold | On disk |
|---|---|---|---|---|
| `cmd/api` | REST `/v1`, SSE, webhook ingestion, session auth | DB app role, Redis, bus producer, provider read creds, funding webhook secret | wallet signing credentials | no |
| `cmd/execution-worker` | build/inspect/sign/submit, finality observation | DB app role, execution provider, chain observers, **bounded signing credential** | admin/gate mutation | no |
| `cmd/reconciliation-worker` | event-driven/periodic/full reconciliation | DB app role, chain observers, provider status endpoints | signing credentials | no |
| `cmd/market-ingest-worker` | provider streams → archive → bus → ClickHouse | data provider creds, archive write, bus producer, ClickHouse write, ingest-state tables | any financial write beyond ingest state | no |
| `cmd/agent-worker` | strategy evaluation, ToolBroker, model calls, predictions, intents | DB app role (agent-scoped), model key via ToolBroker, bus consumer | **signing, wallet provider, admin, capital/risk authority** | no |
| `cmd/workflow-worker` | Temporal workflows (funding, escalation, promotion, withdrawal-disabled) | DB app role, Temporal, activity provider clients | signing credentials | no |
| `cmd/audit-worker` | chain verification, Merkle checkpoints, KMS signing, WORM archive | DB audit read, KMS sign, archive write | financial writes | no |
| `cmd/migrate` | schema migrations | DB migrate role | production application data access | yes (`cmd/migrate/main.go`, `TestRun_UsageAndExitCodes`) |

Each binary maps to its own ECS task role (PART 100) and DB role (PART 101): DESIGNED, pending Terraform (`infra/` is empty; EB-012).

## 3. Identity and sessions

IMPLEMENTED in `internal/auth` (D-006, D-014) and `internal/auth/httpmw`; Postgres store in `internal/auth/pgstore`.

- **OIDC authorization code + PKCE (S256) + nonce.** `oidc.Provider.AuthCodeURL` sets `code_challenge_method=S256` and `nonce`; step-up adds `prompt=login`, `max_age=0`, `acr_values`. ID tokens are verified for signature (RS/PS/ES), issuer, audience/azp, exp/nbf/iat, nonce, and `alg=none` is rejected (`TestExchange_PKCEMismatch`, `TestExchange_BadNonce`, `TestExchange_WrongAudience`, `TestExchange_Expiry`, `TestExchange_IssuerMismatch`, `TestExchange_BadSignature`, `TestExchange_AlgNoneRejected`, `TestExchange_UnknownKidAndRotation`, `TestExchange_MalformedIDToken`, `FuzzVerifyNeverPanics`). D-014: verification is standard-library until `go-jose` is pinned; the negative tests are the contract.
- **Server-side opaque sessions.** 32-byte random token (base64url, 43 chars); store keyed by `hex(sha256(token))`; raw token never persisted or logged (`TestNewToken_Shape`, `TestHashToken_Deterministic`, `TestProp_ValidateTokenOnlyAcceptsCanonical`, `TestManager_MalformedTokenNeverHitsStore`). Validity = not revoked, absolute expiry (`Auth.SessionTTL`), idle timeout (default 30 min), structural validity (`TestManager_Expiry`, `TestManager_IdleTimeoutAndTouch`, `TestSession_PrincipalFailsClosedOnCorruptRow`). Rotation on privilege change revokes the old row and issues a new token in the same transaction without extending absolute expiry (`TestManager_Rotate`).
- **Cookie attributes** (`httpmw/cookie.go`): `HttpOnly` always; `SameSite=Lax` (Strict drops the OIDC callback); `Secure` from config, forced true in STAGING/PROD by rule `COOKIE_SECURE`; `Path=/`; `__Host-` prefix when secure and host-only; `Max-Age` = session TTL (`TestCookieHelpers`).
- **CSRF model** (`httpmw/csrf.go`): unsafe methods pass only if `Sec-Fetch-Site ∈ {same-origin, none}`, or `Origin` exactly matches the allow-list (`null` never), or, with neither header, `X-Requested-With: XMLHttpRequest` plus an allow-listed `Referer` origin. The `Host` header is never consulted (`TestCSRF_Matrix`, `TestChiIntegration`).
- **Client-asserted identity is ignored.** `Authorization`, `X-User-Id`, `X-Roles` and similar never produce a principal (`TestSession_IgnoresClientAssertedIdentity`). A bad cookie is anonymous and cleared; a store error is 503 (`TestSession_BadCookiesAreAnonymousAndCleared`, `TestSession_StoreOutageFailsClosed`).
- **Secure headers**: `X-Content-Type-Options`, `Referrer-Policy`, `Permissions-Policy`, `Content-Security-Policy: frame-ancestors 'none'`, `X-Frame-Options: DENY`, `Cross-Origin-Opener-Policy`, `Cache-Control: no-store`, HSTS when TLS (`TestSecureHeaders`).
- **Step-up rules** (`security.RequireStepUp`): `AuthTime` within `maxAge` and `AMR` contains one of `mfa, otp, hwk, swk, pop, webauthn, passkey`; `pwd`, `sms`, `kba` never qualify; non-positive `maxAge` fails closed (`TestRequireStepUp`, `TestHasStrongAMR`, `TestRequirePermissionAndStepUp`). Gate approve/activate and kill-switch release use a 15-minute window (`gates.StepUpMaxAge`, `killswitch.StepUpMaxAge`).
- **Device listing and revocation**: `sessions` rows carry `ip`, `user_agent`, `device_label`, `rotated_from`, `revoked_at`; `Manager.ListForSubject` returns `Summary` (no hash, no roles); `Revoke` and `RevokeAllForSubject` (`TestManager_ListForSubject`, `TestManager_Revoke`, `TestManager_RevokeAllForSubject`, `TestIntegration_PgStore_Lifecycle`, `TestIntegration_PgStore_RevokeAllAndList`). Permissions `session:list_own`, `session:revoke_own` (every human), `session:revoke_any` (SECURITY, ADMIN).
- **Break-glass**: `Principal.BreakGlassUntil` with role `BREAK_GLASS`; only live while `now < BreakGlassUntil` (`TestBreakGlass_Expiry`, `TestBreakGlass_WallClockWrapper`); `sessions.break_glass_until` is constrained to `actor_type = 'OPERATOR'` (`migrations/00011_sessions_break_glass.sql`); a session must be rotated to obtain it. Grant through an `admin_actions` row of kind `BREAK_GLASS_GRANT` with scope, expiry, reason and notification: DESIGNED (R-093-1; `internal/admin` absent).
- **Dev identity provider** refuses any environment outside LOCAL/TEST/DEV (`devidp.New` → `ErrDevIdPNotAllowed`; `TestDevIdPRefusesProd`); depguard forbids importing it outside `cmd/api`.
- Rate limiting of authentication endpoints: DESIGNED (Redis limiter; explicitly out of `httpmw` scope). MFA enrolment and passkeys are delegated to the identity provider (EB-017).

## 4. Authorization

IMPLEMENTED in `internal/security` (`roles.go`, `authz.go`, `principal.go`). The matrix is a package value with a golden copy; any privilege expansion fails CI (`TestGoldenMatrix_Equal`, `TestGoldenMatrix_PermissionListClosed`, `TestRoleMatrix_EveryCell`, `TestGoldenMatrix_EveryPermissionReachable`).

| Role | Beyond the operator base (all `*:read`, `account:read_any`, own-session permissions) |
|---|---|
| CUSTOMER | `account:read`, `trade:create/read`, `funding:create/read`, `withdrawal:create`, `strategy:write/read`, `agent:pause`, own sessions (no `account:read_any`) |
| SUPPORT_READ_ONLY | operator base only |
| OPERATIONS | `agent:pause`, `provider:disable`, `kill:activate`, `instrument:status_write`, `reconciliation:resolve` |
| RISK | `risk:policy_write`, `kill:activate`, `instrument:status_write`, `gate:propose` |
| COMPLIANCE | `account:freeze`, `gate:propose` |
| FINANCE | `ledger:post_correction`, `reconciliation:resolve` |
| SECURITY | `session:revoke_any`, `kill:activate`, `provider:disable` |
| ADMIN | every permission **except** the dual-control set and the agent-only set |
| BREAK_GLASS (time-boxed) | exactly the dual-control set, only while `BreakGlassUntil` is in the future |
| AGENT (actor type, never a role) | `agent:run`, `prediction:commit`, `intent:create_agent`, `account:read`, `trade:read`, `strategy:read` on its single bound account |

- **Dual-control permissions** — `ledger:approve_correction`, `gate:approve`, `kill:release`, `withdrawal:approve`, `reconciliation:approve` — are held by no standing role (`TestGoldenMatrix_DualControlHeldByNoStandingRole`, `TestProp_NoRoleGrantsDualControlOrAgentOnly`). `RequireDualControl` refuses self-approval (`TestRequireDualControl`); `admin_actions` additionally has `CHECK (approved_by_user_id <> proposed_by_user_id)` and `reason` length ≥ 8 (`migrations/00153_admin_actions.sql`). The `internal/admin` Actions service (propose/approve/execute with `params_hash` replay) is DESIGNED (POLICY_AUTHORITY §5).
- **Tenant scoping**: `RequireAccount` passes only when the principal owns the account or is a non-agent holding `account:read_any`; empty ids are refused; break-glass never grants visibility (`TestRequireAccount_TenantIsolation`, `TestRequireAccount_BreakGlassNeverGrantsVisibility`, `TestAgent_RequireAccountExactMatch`). `ErrCrossTenant` wraps `ErrForbidden`; the API may render it as `NOT_FOUND` but must audit it. API-level enforcement across the ten PART 92 resource types is DESIGNED (R-092-1; `cmd/api` absent).
- **Agent principal constraints**: `Principal.Validate` rejects an AGENT with roles, with ≠ 1 account, or with break-glass (`TestAgentPrincipal_Shape`); `sessions.actor_type` allows only `USER`/`OPERATOR`, so an agent never gets a browser session (`TestIntegration_PgStore_RejectsAgentAndBadSubject`, `TestManager_AgentSessions`). Agent-only permissions are never granted to a role, ADMIN included, so no human can act through the agent path.
- Documentation drift to fix: `AGENT_RUNTIME.md` §11 names the agent set as `agent:run, agent:tool_invoke, prediction:commit, trade:create_agent_intent`; the code (authoritative) uses `intent:create_agent` and has no `agent:tool_invoke`.

## 5. Agent authority boundary

| Layer | Control | State |
|---|---|---|
| Effect system | Allowed effects `READ_MARKET_DATA, READ_ONCHAIN_DATA, READ_APPROVED_SOCIAL_DATA, READ_WALLET_INTELLIGENCE, CALL_MODEL, COMMIT_PREDICTION, CREATE_TRADE_INTENT`; forbidden constants `RAW_SIGN, TRANSFER_VALUE, WITHDRAW, CHANGE_RISK, CHANGE_CAPITAL, EXPORT_SECRET, ARBITRARY_NETWORK, ARBITRARY_CONTRACT_CALL, MODIFY_CAPABILITY_GATE, ACCESS_ADMIN_API` (STRATEGY_IR §3) | compiler DESIGNED (`internal/strategy` absent); DB CHECK on `strategy_versions.effect_set` IMPLEMENTED (`migrations/00500_strategies.sql`) |
| Package import rules | depguard `agent-authority` in `.golangci.yml` and `scripts/lintfin` rule 3: `internal/agent/**`, `internal/strategy/**` may not import `internal/signing`, `internal/wallet`, `internal/admin`, `internal/capital`, `internal/risk/policy`; `no-test-fakes-in-production` forbids `testkit`, `eventtest`, `devidp`, `dbtest`, `clocktest`, `providertest` in production wiring | lint rules IMPLEMENTED (`scripts/lintfin/main_test.go: TestRun, TestIsTestOnlyImport`); the guarded packages do not exist yet, so the rules have never fired; the import boundary is tested by `TestAgentTreesNeverImportAuthority` and `TestSigningImportedOnlyByExecutionBoundary` in `test/security/authority_boundary_test.go`. This cell said `TestAgentImportBoundary` was DESIGNED and that "the guarded packages do not exist yet, so the rules have never fired"; both packages and both tests exist (F-111) |
| ToolBroker | permission (effect in run set, tool ACTIVE; **not** `PROVIDER_DISABLE_NEW_ACTIONS`/`MODEL_DISABLE` — the broker cannot read kill switches, see F-65) → budgets → persisted rate limits → egress allow-list (`tools.egress_hosts`, no generic HTTP tool) → provenance row per call; agents never see secrets or raw provider responses (AGENT_RUNTIME §5) | DESIGNED (R-009-3, R-067-1) |
| Prompt-injection boundary | three fixed segments `SYSTEM POLICY / TOOL RESULTS / UNTRUSTED CONTENT`; content can never add a tool, effect, permission or destination; schema-constrained output; prompts archived as evidence (STRATEGY_IR §10, corpus §11) | DESIGNED (`internal/model` absent) |
| Database | `created_by_actor_type <> 'AGENT'` on `capital_envelopes`, `capital_envelope_changes`, `withdrawal_transitions`, `asset_policies`, `instrument` status transitions, `strategies`, `agents`, `tools`, data sources; `reconciliation_records.resolved_by_actor_type <> 'AGENT'`; `capability_gate_transitions`/`kill_switch_transitions` actor CHECK excludes AGENT; `risk_policies.created_by_actor_type IN ('OPERATOR','SYSTEM','USER')` | IMPLEMENTED in migrations 00102, 00103, 00105, 00150, 00151, 00152, 00200, 00301, 00500–00502, 00600 (no trigger-level tests yet) |
| Prediction predates intent | `trade_intents_prediction_guard` raises `AG001/AG002/AG003` | IMPLEMENTED (`migrations/00502_predictions_tools.sql`), untested |
| Services | `capital.EnvelopeService.requireAdministrator` rejects agents; `gates.Admin` and `killswitch.Controller` reject AGENT and SERVICE before any query; `accounts` and `valuation` reject agent status/policy writes | IMPLEMENTED; tested for gates (`TestAdmin_AgentRejectedBeforeAnyQuery`), killswitch (`TestController_AgentAndServiceRejectedBeforeAnyQuery`), valuation (`TestIntegration_PolicyStore_RejectsAgentAndUnknownAsset`); untested for capital and accounts |

## 6. Signing boundary and transaction inspection

DESIGNED (EXECUTION.md §2–§4, ADR-0012; R-034-1..3, R-095-1, R-096-1). Nothing under `internal/signing`, `internal/wallet`, `internal/execution` or `proto/` exists on disk; SB-005 (decoder selection) is open. What exists: `wallets.delegation_verified_at` and the immutable `signing_decisions` table (`migrations/00300_wallets_execution.sql`), `assets.token_extensions` with `Asset.HasUnsupportedExtensions()` (`internal/assets/extensions.go`), and `execution_attempts.tx_signature UNIQUE`.

Contract: `signing.Service.Sign` re-loads plan, quote, risk decision and reservation from Postgres, rebuilds `Expectations` from persisted data, runs the pure inspector, writes `signing_decisions` plus an audit event in one transaction, and only then calls `wallet.SigningProvider.SignTransaction`. Rejections emit `signing_rejection` security events. `signing` is importable only by `cmd/execution-worker`. Production startup fails closed unless delegated-signing capability is `VERIFIED` (PART 96).

Inspection checklist (every failed check, unknown instruction or decode error ⇒ `REJECTED`; reason codes sorted and stable; the inspector does no I/O — the executor passes address-lookup-table contents in `Expectations`):

| Check | Rule (PART 34 minimum) | Attack it stops |
|---|---|---|
| `FEE_PAYER` | fee payer == expected wallet | draining another wallet's SOL for fees |
| `SIGNERS` | the only required signer is the expected wallet | extra signer smuggled into a multisig-style transaction |
| `PROGRAM_ALLOWLIST` | every top-level and inner program id ∈ System, ComputeBudget, Token, Associated Token, configured router ids, per-plan additions | arbitrary program invocation |
| `TOKEN_PROGRAM` | token instructions target expected mints; Token-2022 rejected unless `token_extensions` empty and explicitly enabled per asset | transfer hooks, fees, freeze rules altering balance semantics |
| `INPUT_DEBIT_BOUND` | total possible input-token debit ≤ `MaxInputDebit` from the reservation | over-spend beyond the reserved quantity |
| `OUTPUT_TOKEN` | output token account belongs to the wallet and mint == expected output | proceeds routed to a foreign account |
| `MIN_OUTPUT` | encoded minimum out ≥ plan `MinOutputQuantity` | route silently lowering the floor |
| `NO_SYSTEM_TRANSFER` | no `SystemProgram.Transfer` from the wallet except bounded rent for own ATA and plan-allowed wrapped-SOL | hidden SOL transfer |
| `NO_UNEXPECTED_DESTINATION` | every value-receiving account is the wallet, its ATAs, or a venue program-owned account in the route | exfiltration destination |
| `NO_AUTHORITY_CHANGE` | no `SetAuthority`, foreign `CloseAccount`, `Approve`/`ApproveChecked`, `FreezeAccount`, `Assign`, `Allocate`, nonce/ownership change | delegation or ownership theft |
| `NO_ARBITRARY_CPI` | no instruction outside the allowlist, including inner instructions revealed by simulation | CPI into a malicious program |
| `COMPUTE_BUDGET` | priority fee ≤ `MaxPriorityFeeLamports`; CU limit ≤ `MaxComputeUnits` | fee griefing |
| `BLOCKHASH` | matches adapter's blockhash; `LastValidBlockHeight` in the future by ≥ margin | stale or replayable transaction |
| `PLAN_IDENTITY` | `PlanHash` and `QuoteID` match the persisted approved plan and quote | approved request reused for a different transaction |
| `SLIPPAGE` | encoded slippage bps ≤ plan `MaxSlippageBPS` | sandwich exposure beyond policy |
| `SIMULATION` | simulation succeeded and predicted deltas satisfy `INPUT_DEBIT_BOUND` and `MIN_OUTPUT` | state effects not visible from static decoding |

Required tests before Stage 6 exit: table test per check, `FuzzInspect` (random bytes and mutated real transactions must never panic and must reject), golden real-transaction fixtures with mutated variants, crash test (PART 49), submission-unknown recovery (timeout ⇒ `SUBMISSION_UNKNOWN`, reservation locked to the order, never a duplicate submission for the same plan, observers disagreeing ⇒ `RECONCILIATION_REQUIRED`).

## 7. Capability gates and kill switches

**Gates** — IMPLEMENTED in `internal/gates` (`migrations/00150_capability_gates.sql`); operator contract in `docs/compliance-gates/PRODUCTION_GATES.md`. ACTIVE requires all five conditions (config enabled, row `ACTIVE`, inside window and not revoked, required evidence refs, ≥ 2 distinct approvers none of whom proposed). High-risk gates additionally require the activator to differ from proposer and first approver, each with a step-up within 15 minutes. `Bootstrap` persists `DISABLED` for every capability. Agents and services are rejected before any query. Tests: `TestCanTransition_EveryPair`, `TestEvaluate_ValidGateIsActive`, `TestEvaluate_EachConditionFailsWithDistinctReason`, `TestEvaluate_ConfigAloneNeverActivates`, `TestChecker_ConfigDisabledSkipsQuery`, `TestChecker_NilEnabledFailsClosed`, `TestAdmin_AgentRejectedBeforeAnyQuery`, `TestAdmin_ServiceAndAnonymousRejectedBeforeAnyQuery`, `TestAdmin_PermissionAndStepUpBeforeAnyQuery`, `TestRules_SamePrincipalCannotApproveAndActivate`, `TestDistinctApprovers_ExcludesProposer`, `TestEvidenceDigest_DeterministicAndOrderIndependent`. No Postgres integration test of the gate store exists yet. Current state: every capability `DISABLED` in every environment; platform `NOT_READY`.

**Kill switches** — IMPLEMENTED in `internal/killswitch` (`migrations/00151_kill_switches.sql`). Activation: one operator with `kill:activate` and a reason; no step-up, no approval, effective on the next check (checks read Postgres inside the authorizing transaction; `CachedChecker` ≤ 1 s for pre-checks only). Release: `kill:release` (a live BREAK_GLASS elevation) plus step-up within 15 minutes; `SEVERE` kinds (`GLOBAL_NEW_RISK_KILL`, `CHAIN_DISABLE_NEW_ACTIONS`, `PROVIDER_DISABLE_NEW_ACTIONS`, `FUNDING_DISABLE`, `WITHDRAWALS_DISABLE`) additionally need a verified `KILL_SWITCH_RELEASE` admin action that is dual-controlled and targets the same `(kind, scope)`. **What kills never stop**: `OBSERVE`, `SETTLE`, `RECONCILE`, `LEDGER_POST`, `CANCEL` are never blocked and never touch the database; `REDUCE_RISK` survives a global kill. Tests: `TestMatrix_NewRisk`, `TestMatrix_ReduceRisk`, `TestMatrix_Withdraw`, `TestProp_NeverBlockedClasses`, `TestProp_EveryActiveSwitchCombination`, `TestProp_InactiveNeverBlocks`, `TestProp_GlobalKillNeverStopsRiskReduction`, `TestController_AgentAndServiceRejectedBeforeAnyQuery`, `TestController_PermissionsBeforeAnyQuery`, `TestController_ApprovalVerificationRules`. The `ApprovalVerifier` implementation (reads `admin_actions`) is DESIGNED with `internal/admin`.

## 8. Secrets and configuration

IMPLEMENTED in `internal/config` (`secret.go`, `validate.go`, `environment.go`) and `internal/observability/logger.go`.

**SecretRef schemes**: `env://NAME` (variable name `^[A-Za-z_][A-Za-z0-9_]*$`), `aws-sm://name-or-arn` (resolver not yet on disk), `file://path` (LOCAL/TEST/DEV only), plain literal (LOCAL/TEST only). Any other prefix, including `postgres://…`, is a plain value and is rejected outside LOCAL/TEST, so a mistyped reference can never reach a deployed environment. References are kept in `Config.Hash`, values never; `Redacted()` masks plain values (`TestParseSecretRef`, `TestSecretRef_ValidateFor`, `TestSecretRef_Redacted`, `TestHash_StableAndExcludesSecrets`, `FuzzParseSecretRef`). Load applies defaults only in LOCAL/TEST (`TestLoad_DefaultsNeverApplyOutsideLocalTest`).

**Validation rules** (`config.Validate`; each named by a `Rule` and tested individually in `TestValidate_ProdRulesIndividually`, `TestValidate_NoFakeProvidersCoversEverySlot`, `TestValidate_InsecureOTLPOnlyRejectedInProd`, `TestValidate_JoinsEveryViolation`):

| Rule | Applies in | Field(s) | Rejects |
|---|---|---|---|
| `FIELD` | all | every basic field | empty, unparsable or out-of-range values; CORS entries that are not `scheme://host[:port]`; non-CIDR trusted proxies |
| `SECRET_REF_SCHEME` | all | every `SecretRef` | plain values outside LOCAL/TEST; `file://` outside LOCAL/TEST/DEV |
| `OIDC_CONFIGURED` | all (https in STAGING/PROD) | `Auth.Issuer`, `Auth.ClientID`, `Auth.ClientSecretRef`, `Auth.RedirectURL` | missing values when mode is `oidc`; non-https issuer/redirect in STAGING/PROD |
| `NO_DEV_AUTH` | all | `Auth.Mode` | `dev` outside LOCAL/TEST/DEV |
| `NO_FAKE_PROVIDERS` | STAGING, PROD | `Providers.<slot>.Mode` for funding, wallet, signing, execution, market_data, chain_observer, chain_observer_fallback, model, event_bus, workflow, archive, notification | mode `fake` in any slot |
| `NO_DEBUG_AUTH` | STAGING, PROD | `Auth.DebugAuthEnabled` | true |
| `NO_SEED` | STAGING, PROD | `Seed.Enabled` | true (no seeded balances) |
| `DATABASE_TLS`, `REDIS_TLS`, `REDPANDA_TLS`, `CLICKHOUSE_TLS`, `TEMPORAL_TLS` | STAGING, PROD | `*.RequireTLS` | false |
| `NO_CORS_WILDCARD` | STAGING, PROD | `HTTP.CORSOrigins` | `*` |
| `ARCHIVE_CONFIGURED` | STAGING, PROD | `Archive.Region`, `Archive.RawBucket`, `Archive.EvidenceBucket`, `Archive.AuditBucket` | empty |
| `ARCHIVE_OBJECT_LOCK` | STAGING, PROD | `Archive.ObjectLockRequired` | false |
| `KMS_CONFIGURED` | STAGING, PROD | `KMS.AuditSigningKeyID` (+ `KMS.Region` whenever a key is set) | empty |
| `COOKIE_SECURE` | STAGING, PROD | `Auth.CookieSecure` | false |
| `PUBLIC_BASE_URL_HTTPS` | STAGING, PROD | `HTTP.PublicBaseURL` | non-https |
| `RETENTION_NON_ZERO` | STAGING, PROD | `Retention.FinancialRecordDays`, `Retention.SecurityAuditDays` | ≤ 0 |
| `PUBLIC_PRODUCT_NAME` | STAGING, PROD | `PublicProductName` | empty (the codename never reaches users; EB-009) |
| `CAPABILITY_STORE_CONFIGURED` | STAGING, PROD | `Capability.StoreConfigured` | false (gate rows must have a database) |
| `NO_INSECURE_OTLP` | PROD only | `Telemetry.OTLPInsecure` | true |

Validation never applies a default; it only rejects, and every violation is reported together (`TestValidate_JoinsEveryViolation`). Defaults exist only in LOCAL/TEST (`Environment.AllowsDefaults`).

`db.Open` refuses to dial when `RequireTLS` is set and `sslmode` is not `verify-ca`/`verify-full` (`TestOpen_RequireTLSFailsClosedBeforeDialling`); telemetry export is TLS unless explicitly insecure (`TestSetup_TLSByDefaultOutsideInsecure`).

**Redaction denylist** (`observability.deniedKeys`, matched on normalised key segments at any group depth): `private_key`, `privatekey`, `seed`, `seed_phrase`, `mnemonic`, `secret`, `token`, `access_token`, `refresh_token`, `id_token`, `authorization`, `cookie`, `set-cookie`, `password`, `passwd`, `api_key`, `apikey`, `card`, `pan`, `cvv`, `ssn`, `signing_token`, `webhook_secret`. Value patterns masked regardless of key: `Bearer <20+ chars>`, base58 strings of 87–88 characters (exported Solana secret keys), PEM blocks. `observability.Secret` never renders. Redaction is not optional in `NewLogger` (`TestRedaction_DenylistKeys`, `TestRedaction_NestedGroups`, `TestRedaction_ValuePatterns`, `TestSecret_NeverRenders`). Problem responses never leak internal causes (`TestToProblem_NeverExposesCauseForAnyCode`).

DESIGNED: AWS Secrets Manager resolver, IAM task roles, GitHub OIDC deployment, no static AWS keys (R-099-1; EB-012).

## 9. Data protection

- **PII separation**: `identity_pii` is a separate table from `users`/`accounts` (`migrations/00010_identity_accounts.sql`); e-mail is informational, never an identity key (`auth.Identity`). No card fields exist in any schema (PART 191); `card`, `pan`, `cvv` are on the log denylist. Encryption of `identity_pii` at the application layer: DESIGNED.
- **Retention classes** (`config.RetentionConfig`): `FinancialRecordDays`, `SecurityAuditDays` (both > 0 in STAGING/PROD), `RawMarketDataDays`, `SocialDataDays`, `ModelIODays`, `OperationalLogDays`. Social persistence is behind the `SOCIAL_DATA_PERSISTENCE` gate (DISABLED). Retention jobs: DESIGNED.
- **Encryption**: TLS to Postgres, Redis, Redpanda, ClickHouse, Temporal and OTLP enforced by config in STAGING/PROD; at-rest encryption (RDS, S3 with KMS) DESIGNED pending Terraform.
- **Evidence integrity**: `audit_events` hash-chained per stream under `pg_advisory_xact_lock(hashtext(stream))` with `content_hash = sha256(CanonicalJSON(record))` and `prev_hash` (`internal/audit/writer.go`, `verify.go`; `TestCanonicalJSON_*`, `FuzzCanonicalJSON`, `TestHashEvent`, `TestHashRecordShape`); immutable rows (`migrations/00106_audit_events.sql`). No Postgres integration test of `Append`/`VerifyStream` exists yet. Merkle checkpoints, KMS signature and S3 Object Lock archive (`cmd/audit-worker`, migrations 00700+): DESIGNED (ADR-0007).
- **ClickHouse boundary**: ClickHouse is never truth for any balance or eligibility (SYSTEM.md §4); it receives normalised market/decision/telemetry data through the ingest worker only; a read-only analytics DB role (`cp_readonly`) is the only path from Postgres to reporting.

## 10. Network, container, IAM and database-role posture

| Area | Designed (AWS, single primary region, ADR-0017) | On disk |
|---|---|---|
| Network (PART 102) | private RDS/Redis/workers; public entry only via WAF + ALB; TLS; restricted security groups; no public DB; no open management ports | `infra/` empty — pending Terraform (EB-012, R-102-1) |
| IAM (PART 100) | one ECS task role per binary; API has no signing privilege; execution worker bounded signing; agent worker none | pending Terraform (R-100-1) |
| Containers (PART 103) | distroless static, non-root, read-only filesystem, no shell, pinned deps, health checks, resource limits | `build/Dockerfile` IMPLEMENTED: `gcr.io/distroless/static-debian12:nonroot`, `USER 65532:65532`, `CGO_ENABLED=0`, `-trimpath`, `go mod verify`; images scanned with trivy in `release.yml`; ECS resource limits/health checks pending Terraform |
| DB roles (PART 101) | migrate, app, read-only analytics, ops | `docker/postgres/init/001_roles.sql` (LOCAL only): `cp_migrate` owns schema; `cp_app` has **no** default table privileges (D-016) and receives per-table grants in each migration, never DELETE, never DDL; `cp_readonly`/`cp_ops` SELECT (ops may DELETE outbox/inbox/idempotency for cleanup). Tests: `TestIntegration_ApplicationRolePrivileges`, `TestIntegration_Migrations` ("role separation" subtest), `TestIntegration_AppRoleCannotDeleteIdempotencyKeys`. Production role bootstrap pending Terraform |
| Migrations | forward-only above `ProtectedVersion` = 100; checksum tamper detection | IMPLEMENTED: `TestGuardDownTo`, `TestProp_GuardDownTo_NeverUndoesProtectedHistory`, `TestIntegration_Migrations` (tampered checksum detected) |

## 11. Supply chain

`.github/workflows/ci.yml` (never executed yet: no git remote, SB-004) runs, per job:

- `go`: `go build`, `make vet`, `gofumpt -l` format check, `staticcheck`, `golangci-lint` (gosec, depguard, forbidigo, errcheck, revive, …), `scripts/lintfin`, `make unit`, `make property`, `make race` (capital, ledger, event and the not-yet-existing execution/reconciliation/settlement/signing trees), `govulncheck`, `gosec`.
- `fuzz`: every fuzz target for 10 s (`scripts/fuzzall`).
- `contract`: `make contract` → `./test/contract/...` — the directory does not exist, so the step is vacuous today.
- `scripts-crossplatform`: Windows and macOS vet/test of `scripts/`.
- `integration`: docker-compose Postgres; `make migrate-test`, `make integration`, `make security` → `./test/security/...` — **the directory does not exist, so the security step passes vacuously**.
- `security-scans`: gitleaks over full history (`.gitleaks.toml` allow-lists only local `cp_*_local` credentials), `trivy fs` (vuln + misconfig, fail on HIGH/CRITICAL, unfixed ignored), `trivy config infra/` (directory empty), `syft` SBOM uploaded as an artifact.
- `web`, `openapi-drift`, `generated-code-drift`: skipped until `apps/web`, `sqlc`, `buf` exist.

`.github/workflows/release.yml`: immutable images tagged by git SHA, `trivy image` fail on HIGH/CRITICAL before push, push to GHCR, keyless `cosign sign` via GitHub OIDC, per-image SBOM with `cosign attest`, SLSA provenance via `actions/attest-build-provenance`; ECR path via OIDC role is commented out pending EB-012. Pinned tool downloads are checksum-verified and fail closed (`scripts/tool`: `TestInstallReleaseTool_ChecksumMismatch`, `TestInstallReleaseTool_APIErrorFailsClosed`, `TestInstallReleaseTool_UnverifiableRequiresFlag`). Missing versus PART 104: frontend dependency audit (no frontend yet), IaC scan with content, a `make security` that fails when the tree is empty.

## 12. Security eventing

On disk: table `security_events(id, kind, severity ∈ {INFO,WARN,HIGH,CRITICAL}, user_id, session_id, account_id, detail jsonb, ip, user_agent, request_id, occurred_at)` with an immutability trigger and INSERT-only grant for `cp_app` (`migrations/00010_identity_accounts.sql`); event topic `security.event` in `internal/event/topics.go`. `kind` is free text: no CHECK, no Go enumeration, and **no writer exists** — nothing on disk records a security event yet. Gate and kill-switch transitions do append to the `admin` audit stream through `AuditAppender`.

Intended kinds (PART 130) and their alerting intent: `login_anomaly` (WARN), `mfa_change` (INFO), `session_revoke` (INFO), `admin_privilege_use` / break-glass use (HIGH, page), `webhook_signature_failed` (HIGH; provider_events has `signature_verified` but no verifier code), `provider_credential_change` (HIGH), `capability_activation` (CRITICAL, page), `global_kill` (CRITICAL, page), `signing_rejection` (CRITICAL, page: a rejected signing request means a route or plan produced a forbidden transaction), `wallet_policy_violation` (CRITICAL), `cross_tenant_attempt` (HIGH, rate-alert). Financial metrics that exist today in `internal/observability/metrics.go` and double as security signals: `capability_gate_rejections`, `risk_rejections`, `duplicate_command_rejections`, `provider_duplicate_events`, `unknown_submissions`, `reconciliation_mismatches`, `negative_deficit_accounts`, `policy_violations`, `agent_pauses`. Emitter, alert routing and the 11-event integration test are DESIGNED (R-130-1).

## 13. Security testing matrix (PART 155)

| PART 155 item | Exists today (file → test) | Planned (traceability) |
|---|---|---|
| IDOR / cross-tenant reads / cross-tenant writes | `internal/security/authz_test.go: TestRequireAccount_TenantIsolation, TestRequireAccount_BreakGlassNeverGrantsVisibility`; `agent_test.go: TestAgent_RequireAccountExactMatch, TestProp_AgentNeverCrossesAccount` (primitive only; no HTTP handlers exist) | `test/security/{idor,cross_tenant}_test.go`, 10 resource types × read/write (R-092-1, R-155-1) |
| CSRF | `internal/auth/httpmw/httpmw_test.go: TestCSRF_Matrix, TestChiIntegration, TestSecureHeaders, TestCookieHelpers` | `test/security/csrf_test.go` end-to-end (R-192-1) |
| SSRF | none | ToolBroker egress allow-list test; `test/security/ssrf_test.go` (R-009-3) |
| Prompt injection | none (`internal/model`, `internal/strategy` absent) | STRATEGY_IR §11 corpus; `test/security/prompt_injection_test.go` (R-067-1) |
| Webhook forgery | none (`provider_events.signature_verified` column only) | `test/security/webhook_forgery_test.go` (R-155-3) |
| Replay | `internal/idempotency/integration_test.go: TestIntegration_AcquireCompleteReplay, TestIntegration_DifferentHashIsDeterministicConflict, TestIntegration_ConcurrentBegin_ExactlyOneAcquires`; `internal/event/inbox_test.go: TestInbox_Process_ValidatesArguments, TestCheckHash` (unit) | `TestProp_DuplicateWebhookOneEffect` (webhooks) and `TestIntegration_ConcurrentBegin_ExactlyOneAcquires` (commands). This cell named a duplicate-commands property test that has never existed; the command half is covered by the idempotency integration tests in the left column rather than by a property test (F-111) |
| Agent withdrawal attempt | `internal/security/agent_test.go: TestAgent_ExactPermissionSet, TestProp_AgentNeverGainsForbiddenPermissions`; DB CHECK `withdrawal_transitions.actor_type <> 'AGENT'` (untested) | `test/security/agent_withdrawal_attempt_test.go` (R-094-1) |
| Agent raw sign attempt | permission-level only (`RAW_SIGN` is not a permission; no signing package) | `test/security/agent_raw_sign_attempt_test.go` (R-095-1) |
| Agent capital escalation | `agent_test.go` as above; `capital.requireAdministrator` (untested); DB CHECKs in 00102 | This cell named a wildcard of agent-escalation tests in `internal/capital` as though they existed; none of them do. What DOES hold the line, and is tested: `TestAgentPrincipalPermissionSetIsClosed` and `TestAgentTreesNeverImportAuthority` in `test/security`, `TestAgentPrincipalCannotChangeItsOwnLifecycle` in `internal/agent`, and the `<> 'AGENT'` CHECKs in migration 00102 (F-111) |
| Agent risk escalation | `risk:policy_write` outside the agent set (`TestGoldenMatrix_AgentSet`); `internal/risk` absent | `test/security/agent_risk_escalation_test.go` |
| Admin privilege misuse | `authz_test.go: TestRequireDualControl, TestBreakGlass_Expiry`; `matrix_golden_test.go: TestGoldenMatrix_DualControlHeldByNoStandingRole`; `gates_test.go: TestRules_SamePrincipalCannotApproveAndActivate, TestDistinctApprovers_ExcludesProposer, TestAdmin_PermissionAndStepUpBeforeAnyQuery`; `killswitch_test.go: TestController_ApprovalVerificationRules` | `internal/admin` tests; `test/security/admin_privilege_misuse_test.go` (R-091-1, R-093-1) |
| Production capability bypass | `gates_test.go: TestEvaluate_ConfigAloneNeverActivates, TestChecker_NilEnabledFailsClosed, TestEvaluate_EachConditionFailsWithDistinctReason, TestAdmin_AgentRejectedBeforeAnyQuery` | Postgres integration of gate store; `test/security/production_capability_bypass_test.go` |
| Fake provider activation in production | `internal/config/validate_test.go: TestValidate_NoFakeProvidersCoversEverySlot, TestValidate_ProdRulesIndividually`; `devidp_test.go: TestDevIdPRefusesProd`; `load_test.go: TestLoad_DefaultsNeverApplyOutsideLocalTest` | `test/security/fake_provider_in_prod_test.go` (boot-level) |
| JWT tampering | `internal/auth/oidc/provider_test.go: TestExchange_BadSignature, TestExchange_AlgNoneRejected, TestExchange_WrongAudience, TestExchange_IssuerMismatch, TestExchange_Expiry, TestExchange_BadNonce, TestExchange_PKCEMismatch`; `verify_test.go: TestExchange_MalformedIDToken, FuzzVerifyNeverPanics` (sessions are opaque, not JWTs) | `test/security/jwt_tampering_test.go` (R-090-1) |
| Stolen session scenarios | `manager_test.go: TestManager_Revoke, TestManager_RevokeAllForSubject, TestManager_Rotate, TestManager_Expiry, TestManager_IdleTimeoutAndTouch, TestManager_MalformedTokenNeverHitsStore`; `httpmw_test.go: TestSession_IgnoresClientAssertedIdentity, TestSession_BadCookiesAreAnonymousAndCleared`; `pgstore: TestIntegration_PgStore_Lifecycle` | `test/security/stolen_session_test.go`; auth rate limiting; login-anomaly events |
| Malformed provider payload | fuzzers: `FuzzParseUSD`, `FuzzParseQuantity`, `FuzzScanQuantity`, `FuzzParse` (id), `FuzzCanonicalJSON` (audit, idempotency), `FuzzEnvelopeJSON`, `FuzzParseSecretRef`, `FuzzVerifyNeverPanics` | provider contract tests under `test/contract` (D-012) |
| Dependency compromise | CI `govulncheck`, `trivy fs`, gitleaks, SBOM; `go mod verify` in Dockerfile; checksum-verified tool pins (`scripts/tool/main_test.go`) | `test/security/dependency_compromise_test.go` simulation (R-155-3) |

Additional invariant tests on disk that carry security weight: `TestProp_BasisConserved`, `TestIntegration_ConcurrentDisposalsNeverOverConsume`, `TestIntegration_VerifyAgainstLedger_DetectsDrift` (positions); `TestReservationTransitions_OnlyActiveHasSuccessors`, `TestCheckReservationTransition_NonActiveRejected` (capital); `TestProp_FeeNeverExceedsInputAndIsMonotone` (fees); `TestTracker_DisableOverridesObservation` (provider health).

## 14. Vulnerability disclosure and incident response

- **Runbooks**: `docs/runbooks/` does not exist. PART 157 requires runbooks for wallet provider compromise, funding provider compromise, Helius outage, RPC disagreement, Jupiter outage, duplicated trade suspicion, unknown transaction, ledger mismatch, chargeback/reversal, model malfunction, stale market data, secret exposure, admin compromise, database corruption, Redpanda outage, Temporal outage, ClickHouse outage (R-157-1, R-157-2, both `NOT_STARTED`). Until they exist, the operational fallbacks are: activate `GLOBAL_NEW_RISK_KILL` (single operator, seconds), `Manager.RevokeAllForSubject` for a compromised principal, and `make verify-audit` (target exists; the verifier binary does not).
- **Disclosure policy**: no `SECURITY.md` contact, PGP key or bounty scope is published; it is deferred until the public product name is cleared (EB-009) and a production tenant exists (EB-012).
- **Secret exposure**: rotate through the SecretRef indirection (`env://`, `aws-sm://`), never by editing images; gitleaks scans history in CI.

## 15. Design-to-code drift register (found during this review)

| Drift | Where | Action |
|---|---|---|
| Agent permission names differ: docs say `agent:tool_invoke`, `trade:create_agent_intent`; code has `intent:create_agent` and no tool-invoke permission | `AGENT_RUNTIME.md` §11 vs `internal/security/roles.go` | align the doc to the code (code is the golden matrix) or add the permission with a golden-test update |
| `make security` and `make contract` pass with no packages | `Makefile`, `.github/workflows/ci.yml` | fail the target when `test/security` / `test/contract` is empty |
| `make iac-scan` scans an empty `infra/` | `Makefile` | fail when no `.tf` files exist |
| `make race` lists packages that do not exist (`execution`, `reconciliation`, `settlement`, `signing`) | `Makefile` `RACE_PKGS` | harmless today; keep, but do not read a green race job as coverage |
| Traceability marks implemented rows `NOT_STARTED`; `MASTER_BUILD_STATE.md` §5 still says "no tests exist yet" | `docs/build/*` | update rows R-090/091/092/097/098/101/103/104/130/190/192 with the tests named in §13 |
| `security_events.kind` is free text with no Go enumeration and no writer | `migrations/00010`, `internal/` | add `security.EventKind` constants, a CHECK or golden test, and an emitter (R-130-1) |
| `audit.PGWriter` and `Verifier` have no database test | `internal/audit` | add a concurrent-append integration test proving no fork or gap |
| `proto/signing/v1` referenced by EXECUTION.md does not exist; `proto/`, `openapi/`, `apps/`, `packages/` are empty | repo root | expected at this stage; do not cite them as controls |
