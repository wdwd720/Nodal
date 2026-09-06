# BLOCKERS

Two categories. Never mix them.

- **SOFTWARE BLOCKER** — something engineering can resolve inside this repository.
- **EXTERNAL BLOCKER** — requires legal approval, provider contract, production credentials, external account access, business approval, or information impossible to infer safely. Software for these must still be implemented, tested, and gated. `BLOCKED_EXTERNAL` does not mean "not built".

IDs are referenced from `REQUIREMENTS_TRACEABILITY.md`; do not renumber.

Last updated: 2026-09-06

---

## SOFTWARE BLOCKERS

| ID | Blocker | Impact | Status | Resolution path |
|---|---|---|---|---|
| SB-001 | Go / make / terraform toolchain not installed on build host at session start | cannot build core | RESOLVED (Go) 2026-09-05; make/terraform pending | Go 1.27.0 installed from SHA-256-verified zip at `C:/Dev/tools/go`; winget MSI stuck behind UAC (operator may approve/dismiss); ezwinports.make + Hashicorp.Terraform queued behind it |
| SB-002 | Docker daemon / local compose stack not running at session start | no Postgres/Redis/Redpanda/ClickHouse/Temporal/MinIO locally | RESOLVED 2026-09-05 | Docker Desktop launched; `docker compose up -d --wait` healthy (7 services). Host also runs unrelated containers on 127.0.0.1:8788/8789 — compose uses offset ports |
| SB-003 | sqlc, buf, golangci-lint, staticcheck, govulncheck, gosec, k6, gitleaks, trivy, syft not installed | lint/scan/generate targets | PARTIAL 2026-09-05 | staticcheck, govulncheck, gofumpt, goimports, buf, oapi-codegen, gosec, golangci-lint in `./bin`; sqlc/gitleaks/trivy/syft/k6 via `scripts/tool install` (prebuilt, checksum-verified) |
| SB-004 | No git remote / GitHub repository | CI workflows cannot run; no OIDC deploy identity | **RESOLVED 2026-09-06** | workflows authored under `.github/workflows`; the operator supplied `https://github.com/wdwd720/Nodal.git` and `main` is pushed (673d9bf). **CI is now fully green** (run 34062522222, commit 5143d0e, all 18 jobs). Getting there took six runs and surfaced twelve distinct defects, none of which was reachable by reading the workflow files: targets that had never been invoked once, lint suppressions written in a syntax the failing tool cannot read, a shared database two suites correctly refuse, a process assertion true on Windows and false on Linux, and a test double whose ordering depended on Go's randomized map iteration. `release.yml` remains unverified — it triggers on tags and none has been pushed. OIDC deploy identity (EB-012) is still an operator action |
| SB-005 | Solana transaction decoding + Token-2022 extension inspection library selection | transaction inspector (Stage 6) | OPEN | evaluate `github.com/gagliardetto/solana-go` (versioned tx + ALT support) vs hand-written decoder; decision recorded in DECISION_REGISTER before Stage 6 |
| SB-007 | Jupiter v6 on-chain instruction layout (discriminators, account order, Swap variant sizes) in `internal/signing/inspect/jupiter.go` was reproduced from memory and is marked UNVERIFIED in code | the inspector's `PROGRAM_ALLOWLIST`/`MIN_OUTPUT`/`SLIPPAGE` checks on real Jupiter transactions could reject valid swaps (fail closed) or, worse, misparse a variant | OPEN — **must be resolved before any canary trade** | verify against the published Jupiter v6 IDL / recorded mainnet transactions; add golden fixtures from real transactions; until then live signing stays gated. **2026-09-06 — a false corroboration path was found and closed:** the Jupiter fake's route builder mirrored the same layout the inspector parses, both derived from one unverified source, so the fake and the inspector agreeing looked like confirmation while proving nothing. The fake and its README now carry explicit UNVERIFIED/SB-007 notes saying exactly that. Nothing about the layout itself has been verified; only the illusion of verification was removed |
| SB-008 | Entire build was uncommitted: 1 commit, 1,300+ untracked files, no remote | total loss to a disk failure or an errant delete; no bisect, no blame, no per-stage diff | **RESOLVED 2026-09-06** | operator supplied a remote; 1,340 files committed as `673d9bf` and pushed to `origin/main`. Verified before pushing: `gitleaks detect` reports no leaks, and a negative control confirmed realistic secrets are still caught in production paths, so the new test-fixture allowlist has not blinded the scan. No `node_modules`, `.terraform`, binaries or `.env` are tracked |
| SB-006 | No gcc on host (no cgo; `go test -race` unavailable) | race detector, cgo-dependent tools | RESOLVED 2026-09-05 | WinLibs GCC 16.2 (SHA-256 verified) extracted to `C:/Dev/tools/mingw64`; `-race` verified on money/id/clock/errs. Production builds remain `CGO_ENABLED=0` |

## EXTERNAL BLOCKERS

| ID | Category | Blocker | Blocks | Software state required before "BLOCKED_EXTERNAL" may be claimed |
|---|---|---|---|---|
| EB-001 | LEGAL | U.S. and California licensing or exemption determination | LIVE_* capabilities | capability gate with `legal_review_ref` field; no live gate ACTIVE |
| EB-002 | LEGAL | Delegated-signing custody analysis (embedded wallet control semantics) | all live trading | signing boundary implemented and tested; prod startup fails closed if signing semantics unverified |
| EB-003 | PROVIDER | Stripe fiat-to-crypto onramp commercial approval and production credentials (sandbox is application-gated too) | LIVE_FUNDING | **software state met 2026-09-06**: adapter CODE_COMPLETE + CONTRACT_TESTED (`internal/provider/stripe`, `test/contract/stripe`), webhook pipeline (`internal/webhook`), gate (`internal/gates`) — genuinely BLOCKED_EXTERNAL |
| EB-004 | BUSINESS | Stripe fraud/dispute responsibility allocation | funding reversibility policy values | reversal handling implemented with configurable hold policy |
| EB-005 | PROVIDER | Wallet provider (Privy) production credentials and verified signing semantics (idempotent replay of `signTransaction` is documented but unverified; policy engine cannot resolve lookup-table accounts) | LIVE_MANUAL_TRADING, LIVE_AGENT_TRADING | **software state met 2026-09-06** (`internal/wallet`, `internal/provider/privy`, `internal/signing`, `test/contract/privy`); prod startup fails closed without `delegation_verified_at` |
| EB-006 | LEGAL | Permitted asset universe decision | instrument activation | asset registry with status + policy reference; unlisted assets fail closed |
| EB-007 | LEGAL | Strategy / adviser / CTA regulatory implications | LIVE_AGENT_TRADING, any marketplace | gate; no personalized recommendations implemented |
| EB-008 | LEGAL | Provider data-retention and redistribution rights (social + market data licensing) | SOCIAL_DATA_PERSISTENCE, long-term raw archive of licensed feeds | retention classes + capability gate |
| EB-009 | BUSINESS | Final brand clearance (PUBLIC_PRODUCT_NAME / trademark) | user-facing product name | `PUBLIC_PRODUCT_NAME` config; codename only internally |
| EB-010 | PROVIDER | Helius production credentials and plan | live observation | **software state met 2026-09-06** (`internal/provider/helius`, `internal/provider/solanarpc`, `internal/chain` agreement policy, `test/contract/{helius,solanarpc}`) — genuinely BLOCKED_EXTERNAL |
| EB-011 | PROVIDER | Jupiter API key and commercial terms | live execution | **client software state met 2026-09-06** (`internal/provider/jupiter`, `test/contract/jupiter`); wrapping into `execution.ExecutionAdapter` + executor wiring still pending (Stage 6 integration), so not yet BLOCKED_EXTERNAL end to end |
| EB-012 | PROVIDER / INFRA | AWS production account, IAM bootstrap, GitHub OIDC trust | staging + prod deployment, WORM audit archive, CI/CD to AWS | **software state met 2026-09-06**: `infra/terraform` validates in dev/staging/prod, trivy clean, DEPLOYMENT.md written; local MinIO archive; OIDC deploy role module — `plan`/`apply` impossible without the account |
| EB-013 | PROVIDER | Model provider (Anthropic) production API key and usage terms | NL strategy compilation in prod | ModelProvider adapter; model budgets |
| EB-014 | PROVIDER / INFRA | Managed Temporal / Redpanda / ClickHouse accounts | staging + prod | local containers; config-driven endpoints |
| EB-015 | PROVIDER / LEGAL | Fiat off-ramp / withdrawal partner and custody path | WITHDRAWALS | withdrawal domain + state machine + gate implemented; capability DISABLED |
| EB-016 | PROVIDER | Tax-reporting partner | tax filing claims | lot-level records preserved; no filing claims made |
| EB-017 | BUSINESS / PROVIDER | Identity provider selection (OIDC issuer) + production tenant | production auth, passkey UX | OIDC-generic IdentityProvider adapter; dev IdP rejected in STAGING/PROD |
