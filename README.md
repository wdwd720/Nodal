# Universal Financial Control Plane (internal codename: Nodal)

**Status: NOT_READY. Capital authority: DISABLED. No live capability is enabled.**

This repository is a USD-native financial control plane whose V1 target is a Solana spot-trading and financial-agent platform. Humans and software express economic intent ("acquire $200 of exposure to this asset"); the platform deterministically translates that intent into safe financial operations through a fixed control path: eligibility → deterministic risk kernel → atomic capital reservation → settlement compiler → quote → transaction inspection → bounded signing → submission → finality observation → reconciliation → ledger posting → audit evidence.

The authoritative specification is `ULTIMATE MASTER GOAL — Production Universal Financial Control Plane.md` at the repository root. Build progress, blockers, and decisions are tracked in `docs/build/`.

## What V1 is

- customer accounts with compliance/eligibility state
- partner-based fiat-to-USDC funding (Stripe onramp adapter; live activation gated)
- embedded, delegated user wallets (Privy-style adapter; live activation gated)
- truthful USD-equivalent buying power with underlying-asset disclosure
- manual Solana spot trading via Jupiter with independent transaction inspection before signing
- typed strategies (natural language → typed IR, or TypeScript SDK → same IR) executed by a deterministic agent runtime under strict capital envelopes
- append-only double-entry, multi-asset ledger; transactional capital reservations; continuous reconciliation
- prediction ledger, backtesting, paper/shadow/canary/limited/live modes kept strictly separate
- tamper-evident audit history, production capability gates, operator controls, observability, infrastructure-as-code, CI/CD

## What V1 is not

No proprietary stablecoin, margin, leverage, derivatives, equities, internal order matching, customer-to-customer transfers, principal trading, hidden spread, performance fees, marketplace, arbitrary user code execution, direct agent signing, direct agent withdrawals, cross-chain buying power, or prediction markets. Interfaces are designed so some of these can be added later; none are implemented as live capabilities.

## Architecture (summary)

Modular monolith in Go with separate binaries per trust boundary (`cmd/api`, `cmd/execution-worker`, `cmd/reconciliation-worker`, `cmd/market-ingest-worker`, `cmd/agent-worker`, `cmd/workflow-worker`, `cmd/audit-worker`). PostgreSQL is the only financial source of truth. Temporal orchestrates long-lived workflows; Redpanda carries high-volume events; ClickHouse holds analytics; S3 with Object Lock holds evidence. Next.js web app in `apps/web`. Terraform for AWS in `infra/terraform`. See `docs/architecture/` and `docs/adr/`.

## Repository layout

See PART 14 of the goal document. Key directories: `cmd/`, `internal/`, `migrations/`, `openapi/`, `proto/`, `packages/`, `apps/web/`, `test/`, `infra/`, `docs/`, `scripts/`.

## Local development

Required tools: Go 1.27, Node 24 + pnpm 10, Docker (Compose v2), GNU make. Optional: Terraform.

```
docker compose up -d --wait      # Postgres 5433, Redis 6380, Redpanda 19092, ClickHouse 18123, Temporal 7233 (UI 8233), MinIO 9100 (console 9101)
make tools                       # pinned developer tools into ./bin (checksum-verified)
make migrate                     # apply migrations to the local dev database
make seed                        # LOCAL-only fake data: dev identities, devnet USDC/SOL, 10,000 fake USDC
make test                        # unit + property + race
eval "$(go run ./scripts/testdb -name mine -export)" && make integration   # isolated, fully migrated test database
make lint                        # gofumpt, vet, staticcheck, golangci-lint, financial lint rules
make restore-drill               # backup → restore → verify → ledger consistency (local)
make proto openapi-server openapi-client   # regenerate gRPC, Go API server, TypeScript client
make help                        # all targets
```

Copy `.env.example` to `.env` for local configuration. Local infrastructure credentials in `docker-compose.yml` are for LOCAL only. The dev identity provider accepts the codes `customer-a`, `customer-b`, `admin` (append `:mfa` for a strong-authentication login); operator roles come from the `operator_roles` table, never from the identity provider.

On Windows, Go 1.27 and a GCC toolchain (for the race detector) are expected under `C:\Dev\tools`; see `docs/architecture/CONVENTIONS.md`.

## Provider configuration

Every external provider sits behind an interface (`FundingProvider`, `WalletProvider`, `SigningProvider`, `ExecutionAdapter`, `MarketDataProvider`, `ChainObserver`, `ModelProvider`, `EventBus`, `WorkflowEngine`, `ObjectArchive`, `NotificationProvider`) with a mode of `fake` (LOCAL/TEST only, rejected programmatically in STAGING/PROD), `sandbox`, or `live`. Credentials are secret references resolved at startup; none live in the repository.

## Test strategy

Unit, property (rapid), race, fuzz, provider contract (recorded fixtures under `test/contract`), integration (each package on an isolated, fully migrated Postgres database), security (`test/security`: authority-boundary import rules, closed agent permissions, production refusal of fake providers), load (`test/load`, k6, unmeasured until an API binary exists), migration, restore drill; API e2e, Playwright UI e2e, and chaos tiers are pending. See `Makefile`, `docs/build/MASTER_BUILD_STATE.md` §12 for the last recorded result of every tier, and `docs/build/REQUIREMENTS_TRACEABILITY.md` for which test proves which requirement. No result is claimed without a recorded run.

## Production safety warnings

- Fresh production deployments start with `LIVE_FUNDING`, `LIVE_MANUAL_TRADING`, `LIVE_AGENT_TRADING`, and `WITHDRAWALS` **DISABLED**. Activation requires deployment configuration **and** persisted, approved, non-expired capability evidence with dual authorization. One environment variable can never enable live money.
- Agents are untrusted proposal generators. They never hold keys, never sign, never withdraw, never change risk or capital.
- No blind signing: every transaction is decoded and inspected against the approved plan before a bounded signing request is made.
- A transport timeout is not an execution failure; unknown submissions are reconciled, never retried blindly.
- Balances are never edited; corrections are reason-coded compensating journal transactions under dual control.
