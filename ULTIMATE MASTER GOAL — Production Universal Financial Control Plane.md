# ULTIMATE MASTER GOAL
## BUILD THE COMPLETE PRODUCTION-GRADE USD-NATIVE UNIVERSAL FINANCIAL CONTROL PLANE

use subagents while coding and doing stuff

You are operating as the principal engineer and technical owner of this repository.

Your responsibility is not merely to write code.

Your responsibility is to take the current repository from whatever state it is presently in to the strongest genuinely production-grade implementation that can be completed through software, infrastructure-as-code, automated testing, documentation, provider integrations, operational tooling, security controls, and explicit external activation gates.

You are simultaneously acting as:

- principal backend engineer
- financial-systems engineer
- distributed-systems engineer
- platform architect
- frontend lead
- security engineer
- SRE
- infrastructure engineer
- data engineer
- quant-platform engineer
- test architect
- developer-experience engineer
- technical product owner

You must reason across all of these domains together.

This system may eventually control meaningful amounts of real customer capital.

That means correctness outranks convenience.

Financial integrity outranks feature velocity.

Recoverability outranks optimistic assumptions.

Fail-closed behavior outranks availability when the two conflict.

Truthful product semantics outrank impressive-looking demos.

You are not building a hackathon project.

You are not building a mock trading terminal.

You are not building a generic "AI agent" framework.

You are not building a collection of disconnected microservices.

You are not building a UI on top of fake balances.

You are building a financial control plane.

---

# PART 1 — FIRST PRINCIPLE

The platform's core abstraction is:

# HUMANS AND SOFTWARE EXPRESS ECONOMIC INTENT.
# THE PLATFORM DETERMINISTICALLY TRANSLATES THAT INTENT INTO SAFE FINANCIAL OPERATIONS.

The long-term product should allow a customer to think:

> "I have $5,000 of usable capital."

and:

> "Acquire $200 of exposure to this asset."

without requiring that customer—or an autonomous strategy—to understand:

- wallet implementation details
- chain selection
- RPC infrastructure
- token account creation
- gas
- priority fees
- signing mechanisms
- settlement currencies
- exchange-native symbols
- bridge mechanics
- venue APIs
- finality rules
- provider retry semantics

The user-facing abstraction is normalized economic value.

The backend truth is more complicated.

Never lie about that distinction.

The product should eventually support many underlying markets, but V1 is intentionally narrower.

---

# PART 2 — AUTHORITATIVE PRODUCT DEFINITION

The V1 is:

# A USD-NATIVE SOLANA SPOT-TRADING AND FINANCIAL-AGENT PLATFORM

with:

- customer accounts
- compliance/eligibility state
- partner-based fiat-to-USDC funding
- embedded user wallets
- truthful USD-equivalent buying power
- underlying asset disclosure
- manual Solana spot trading
- typed strategy definitions
- natural-language-to-strategy compilation
- deterministic agent execution
- strict capital envelopes
- deterministic Risk Kernel
- universal financial intent
- Settlement Compiler
- Jupiter execution
- Helius market/on-chain data
- independent RPC fallback
- transaction inspection before signing
- financial double-entry accounting
- exact asset quantity accounting
- capital reservations
- continuous reconciliation
- point-in-time event history
- prediction commitments
- backtesting
- paper mode
- shadow mode
- canary capital
- limited live capital
- production audit evidence
- operator controls
- production capability gates
- full observability
- production infrastructure
- disaster recovery
- security testing
- financial property testing
- fuzz testing
- chaos testing
- load testing
- provider contract testing
- CI/CD
- capital CI/CD

Do not redefine V1.

Do not quietly expand scope before the core system is complete.

---

# PART 3 — WHAT THIS PRODUCT IS NOT

Do not reduce the company to:

"AI crypto bot."

Do not reduce it to:

"ChatGPT that trades."

Do not reduce it to:

"Robinhood with agents."

Do not reduce it to:

"Axiom with AI."

Do not reduce it to:

"strategy marketplace."

The core proprietary primitives are:

1. Unified Capital Model
2. Unified Buying-Power Engine
3. Universal Instrument Model
4. Universal Financial Intent IR
5. Settlement Compiler
6. Deterministic Risk Kernel
7. Capital Reservation System
8. Exactly-Once Economic Effect Layer
9. Execution/Reconciliation Fabric
10. Point-in-Time Reality Engine
11. Prediction/Calibration Ledger
12. Proof-Carrying Financial History
13. Typed Agent Runtime
14. Provider/Settlement Adapter Fabric

Protect these boundaries.

---

# PART 4 — V1 HARD EXCLUSIONS

Do NOT implement the following as live customer capabilities in V1:

- proprietary stablecoin
- NodalUSD
- platform-issued redeemable dollar token
- margin
- leverage
- futures
- options
- equities
- CFDs
- internal customer order matching
- customer-to-customer transfers
- principal trading against customers
- hidden internal crossing
- performance fees
- strategy profit sharing
- auto-allocation marketplace
- personalized investment recommendations
- unrestricted copy trading
- arbitrary user Python
- arbitrary user Node.js
- arbitrary user Rust
- arbitrary npm/pip dependency execution
- arbitrary network access by strategies
- unrestricted wallet access
- direct agent signing
- direct agent withdrawals
- instant cross-chain buying power
- company balance-sheet credit
- treasury-fronted user settlement
- prediction markets
- CEX omnibus retail accounts

Design interfaces so later implementations remain possible.

Do NOT create fake implementations.

---

# PART 5 — INTERNAL CODENAME

"Nodal" may be used only as an internal codename if already present.

Do NOT hardcode it deeply into public schemas, domains, user-facing copy, or package naming unnecessarily.

Use:

`PUBLIC_PRODUCT_NAME`

for customer-facing branding.

The system should be technically brand-neutral.

---

# PART 6 — SOURCE OF TRUTH

Before changing architecture:

READ THE REPOSITORY.

If the research diligence file exists, read it completely.

Likely names include:

- `deep-research-report (1).md`
- `deep-research-report.md`
- architecture diligence documents
- existing GOAL.md
- ADRs
- README
- design docs

Do not skim them.

Do not assume the repository is greenfield.

Do not replace correct work merely because you prefer your own implementation.

---

# PART 7 — PERSISTENT EXECUTION MEMORY

This project will likely exceed one context window.

Therefore your progress MUST survive context compaction and future sessions.

Immediately create:

`docs/build/MASTER_BUILD_STATE.md`

and:

`docs/build/REQUIREMENTS_TRACEABILITY.md`

and:

`docs/build/BLOCKERS.md`

and:

`docs/build/DECISION_REGISTER.md`

These are mandatory.

## MASTER_BUILD_STATE.md

Must contain:

- current milestone
- completed milestones
- current work
- next exact work
- failing tests
- architectural changes made
- migrations applied
- external blockers
- unresolved defects
- production-capability state

Update this file continuously.

If you resume in another session:

READ THIS FILE FIRST.

## REQUIREMENTS_TRACEABILITY.md

Every major requirement from this prompt must have:

- ID
- requirement
- implementation state
- implementation path
- test proving it
- operational evidence if applicable
- blocker if blocked

States:

- NOT_STARTED
- IN_PROGRESS
- IMPLEMENTED
- VERIFIED
- BLOCKED_EXTERNAL
- DEFERRED_OUT_OF_SCOPE

Do not mark VERIFIED without evidence.

## BLOCKERS.md

Separate:

SOFTWARE BLOCKER

from:

EXTERNAL BLOCKER

Examples of external blockers:

- provider commercial approval
- production credentials
- legal opinion
- licensing decision
- trademark decision

## DECISION_REGISTER.md

For every meaningful deviation from the research architecture:

- decision
- original recommendation
- chosen implementation
- why
- evidence
- consequences
- migration impact

Do not silently change architecture.

---

# PART 8 — CLAUDE EXECUTION CONTRACT

Do NOT spend the session merely writing a plan.

Planning is Stage 0.

Then implementation begins.

Do not repeatedly ask the user ordinary engineering questions.

Use professional judgment.

Only treat something as externally blocked when it genuinely requires:

- legal approval
- provider contract
- production credentials
- external account access
- external business approval
- information impossible to infer safely

If an integration cannot be activated:

IMPLEMENT:

- interface
- production adapter code where API contracts are known
- configuration
- validation
- webhook handling
- state machines
- tests
- contract fixtures
- sandbox support where officially available
- failure modes
- operational gating

Then mark:

`BLOCKED_EXTERNAL`

Do NOT substitute a fake production integration.

---

# PART 9 — GOLDEN RULE

# AN AGENT IS AN UNTRUSTED PROPOSAL GENERATOR.

It is NEVER a financial authority.

This must be enforced architecturally.

Agent may:

- receive normalized events
- call explicitly permitted read tools
- calculate signals
- request model inference
- commit predictions
- create typed financial intents

Agent may NOT:

- possess wallet private keys
- possess wallet API secrets
- possess signing tokens
- call signing provider directly
- construct unrestricted transfers
- withdraw
- change account ownership
- change risk
- increase capital
- modify eligibility
- disable controls
- enable production capability
- bypass Settlement Compiler
- bypass Risk Kernel
- bypass reservation system
- call arbitrary network destinations
- invoke arbitrary smart contracts
- export credentials

Even total model compromise must not permit arbitrary value transfer.

---

# PART 10 — REQUIRED CONTROL PATH

All autonomous money-affecting actions MUST follow:

EVENT / USER OBJECTIVE
↓
AGENT / STRATEGY EVALUATION
↓
STRUCTURED PREDICTION
↓
TYPED TRADE INTENT
↓
ELIGIBILITY ENGINE
↓
DETERMINISTIC RISK KERNEL
↓
ATOMIC CAPITAL RESERVATION
↓
SETTLEMENT COMPILER
↓
EXECUTABLE QUOTE
↓
FINAL RISK VALIDATION
↓
TRANSACTION CONSTRUCTION
↓
TRANSACTION INSPECTION
↓
BOUNDED SIGNING
↓
EXTERNAL SUBMISSION
↓
FINALITY / FILL OBSERVATION
↓
RECONCILIATION
↓
LEDGER POSTING
↓
POSITION UPDATE
↓
AUDIT EVIDENCE

No alternate money path is permitted.

---

# PART 11 — THREE TRUTHS

Explicitly model three different realities.

## 11.1 EXTERNAL TRUTH

Examples:

- Solana blockchain
- wallet provider
- funding provider
- exchange/venue

## 11.2 INTERNAL ACCOUNTING TRUTH

Examples:

- reservations
- customer entitlements
- expected settlement
- fees
- operational states

## 11.3 ECONOMIC VALUATION

Examples:

- marked positions
- USD portfolio value
- buying power
- risk-adjusted collateral value

Never confuse them.

ClickHouse analytics cannot become accounting truth.

Redis cannot become accounting truth.

Temporal workflow memory cannot become accounting truth.

Frontend state cannot become accounting truth.

---

# PART 12 — TECHNOLOGY DECISIONS

Unless strong existing repository evidence makes a migration counterproductive, use:

Frontend:
- TypeScript
- React
- Next.js
- strict TS

Core:
- Go

Database:
- PostgreSQL

Postgres access:
- explicit SQL
- pgx
- sqlc or an equivalent compile-time checked query layer

Avoid opaque ORM behavior in financial paths.

Workflow:
- Temporal

High-volume event transport:
- managed Redpanda
- Kafka protocol

Analytics/time-series:
- ClickHouse

Raw evidence:
- S3

Immutable evidence:
- S3 Object Lock / appropriate WORM configuration

Cache:
- Redis

Cloud:
- AWS

Trusted compute:
- ECS/Fargate

Infrastructure:
- Terraform

Observability:
- OpenTelemetry

Internal RPC:
- Protobuf/gRPC where process boundaries exist

External command/query API:
- REST

Realtime client updates:
- WebSocket or SSE

Research/offline:
- Python

Python must not control production money.

---

# PART 13 — DO NOT PREMATURELY INTRODUCE

Do not introduce without measured need:

- Kubernetes
- service mesh
- custom consensus
- active-active global writes
- blockchain-based audit merely for marketing
- TigerBeetle
- Rust financial backend
- GraphQL money commands
- bespoke cryptography
- bespoke MPC
- custom KYC
- custom stablecoin
- untrusted code execution
- ZK proofs
- Nitro Enclaves
- Firecracker fleet

Prepare interfaces where appropriate.

Do not spend V1 complexity budget on them.

---

# PART 14 — MONOREPO TARGET

Preserve good current layout if it exists.

If architecture is immature, converge toward:

```text
/
├── apps/
│   └── web/
│
├── cmd/
│   ├── api/
│   ├── execution-worker/
│   ├── reconciliation-worker/
│   ├── market-ingest-worker/
│   ├── agent-worker/
│   ├── workflow-worker/
│   └── audit-worker/
│
├── internal/
│   ├── auth/
│   ├── identity/
│   ├── accounts/
│   ├── compliance/
│   ├── eligibility/
│   ├── funding/
│   ├── wallet/
│   ├── money/
│   ├── ledger/
│   ├── capital/
│   ├── assets/
│   ├── instruments/
│   ├── valuation/
│   ├── positions/
│   ├── quote/
│   ├── intent/
│   ├── settlement/
│   ├── execution/
│   ├── signing/
│   ├── reconciliation/
│   ├── risk/
│   ├── strategy/
│   ├── agent/
│   ├── model/
│   ├── prediction/
│   ├── reality/
│   ├── backtest/
│   ├── performance/
│   ├── proof/
│   ├── audit/
│   ├── event/
│   ├── provider/
│   ├── admin/
│   ├── config/
│   ├── observability/
│   └── security/
│
├── proto/
├── openapi/
│
├── packages/
│   ├── strategy-sdk/
│   ├── shared-types/
│   └── generated-client/
│
├── migrations/
│
├── research/
│   ├── replay/
│   ├── simulation/
│   └── analysis/
│
├── test/
│   ├── contract/
│   ├── integration/
│   ├── e2e/
│   ├── property/
│   ├── chaos/
│   ├── load/
│   └── security/
│
├── infra/
│   └── terraform/
│       ├── modules/
│       └── environments/
│           ├── dev/
│           ├── staging/
│           └── prod/
│
├── docs/
│   ├── architecture/
│   ├── adr/
│   ├── build/
│   ├── api/
│   ├── security/
│   ├── threat-model/
│   ├── runbooks/
│   ├── operations/
│   └── compliance-gates/
│
├── scripts/
├── Makefile
└── README.md
```

---

# PART 15 — IDENTIFIERS

Use globally unique immutable IDs.

Prefer UUIDv7 if the existing ecosystem supports it cleanly.

Never use:

- ticker
- email
- wallet address
- provider ID

as internal primary identity.

Provider identifiers are external references.

Financial objects should have separate:

- internal ID
- provider ID
- client idempotency ID
- correlation ID

where appropriate.

---

# PART 16 — TIME SEMANTICS

All persisted financial timestamps:

UTC.

Database:

`timestamptz`.

Serialized APIs:

RFC3339 UTC with appropriate precision.

Use monotonic time sources for duration measurement where possible.

Never compare business events using local timezone assumptions.

Distinguish:

- occurred_at
- provider_published_at
- received_at
- processed_at
- available_at
- settled_at
- finalized_at

These are different concepts.

---

# PART 17 — EXACT NUMERIC REPRESENTATION

Absolutely no floating-point financial values.

USD minor units:

integer.

Basis points:

integer.

Probability:

bounded deterministic decimal representation where persistence matters.

Crypto asset quantity:

exact base units.

If generic atomic asset quantity can exceed machine integer range:

use arbitrary precision integer representation.

Database may use appropriately bounded `NUMERIC(...,0)` or another exact representation.

Price must have:

- mantissa
- scale
- quote asset
- source
- timestamp

Rounding must always specify mode.

Never rely on language defaults.

Write tests for:

- minimum unit
- maximum supported amount
- precision boundaries
- overflow
- rounding up
- rounding down
- conversion round trips

---

# PART 18 — CANONICAL FINANCIAL VOCABULARY

Use these concepts consistently:

## Portfolio Value

USD-equivalent current mark of eligible holdings and positions.

## Buying Power

Value currently permitted for deployment under financial, settlement, risk, eligibility, and operational rules.

## Available Now

Buying power that does not require an upstream asset movement.

## Reserved

Capital committed to an in-flight operation.

## Pending

Funding/settlement not yet usable.

## Withdrawable

Value permitted to leave the platform/wallet path after settlement/fraud/security restrictions.

## Underlying Balance

Actual asset balance such as:

- USDC
- SOL
- token units

Do NOT call stablecoins "bank cash."

---

# PART 19 — FINANCIAL SUBLEDGER

Use PostgreSQL for V1.

Implement append-only double-entry accounting.

Required entities:

LedgerAccount
JournalTransaction
JournalEntry
Reservation
CapitalEnvelope
FinancialEventReference
ReconciliationRecord

Posted journal rows become immutable.

Changes are made through:

compensating transaction

not mutation.

Database privileges should make accidental update/delete of posted ledger data difficult.

Application code must never expose generic ledger mutation.

---

# PART 20 — MULTI-ASSET ACCOUNTING

Do not attempt to "balance" different assets against each other.

Maintain:

## ASSET QUANTITY LEDGER

Each asset balances independently.

Example:

USDC accounts balance in USDC base units.

BONK accounts balance in BONK base units.

## FUNCTIONAL-CURRENCY ACCOUNTING

Separately track:

- execution price
- acquisition valuation
- disposition valuation
- fees
- realized P&L
- reporting values

A swap creates linked postings.

Do not hide asset conversion inside one ambiguous balance field.

---

# PART 21 — REQUIRED FINANCIAL INVARIANTS

Implement as many as possible at:

- database constraint level
- application level
- property-test level

All must be tested.

```text
FOR EVERY POSTED JOURNAL TRANSACTION AND ASSET:
SUM(DEBITS) == SUM(CREDITS)
```

```text
POSTED JOURNAL ENTRIES ARE IMMUTABLE
```

```text
CORRECTIONS REQUIRE COMPENSATING TRANSACTIONS
```

```text
NO FINANCIAL QUANTITY USES BINARY FLOATING POINT
```

```text
NO CAPITAL RESERVATION EXCEEDS AVAILABLE CAPITAL AT COMMIT
```

```text
NO AGENT CAN INCREASE ITS OWN CAPITAL
```

```text
NO AGENT CAN CHANGE ITS OWN RISK POLICY
```

```text
NO AGENT CAN WITHDRAW
```

```text
NO AGENT CAN SIGN ARBITRARY VALUE TRANSFER
```

```text
ONE IDEMPOTENCY KEY CAN PRODUCE AT MOST ONE ECONOMIC EFFECT
```

```text
ONE PROVIDER EVENT CAN PRODUCE AT MOST ONE CANONICAL EVENT
```

```text
ONE EXTERNAL FILL CAN PRODUCE AT MOST ONE INTERNAL FILL
```

```text
EVERY EXTERNAL FILL EVENTUALLY MAPS TO:
ORDER
+
POSITION IMPACT
+
LEDGER IMPACT
```

```text
HTTP SUCCESS != FINANCIAL SETTLEMENT
```

```text
UNKNOWN EXTERNAL EXECUTION != FAILED EXECUTION
```

```text
A RELEASED RESERVATION CANNOT STILL SUPPORT NEW EXECUTION
```

```text
DUPLICATE WEBHOOK != DUPLICATE MONEY
```

```text
DUPLICATE COMMAND != DUPLICATE TRADE
```

```text
REPLAYED EVENT != DUPLICATE FINANCIAL STATE
```

---

# PART 22 — CAPITAL RESERVATION

Financial concurrency must be solved transactionally.

Do NOT solve this with:

- Redis lock
- process mutex
- frontend disable button
- optimistic local cache

Authoritative reservation happens in PostgreSQL.

The operation:

1. lock relevant capital envelope state
2. verify available amount
3. create reservation
4. decrement available
5. increment reserved
6. persist intent state
7. persist transactional outbox event
8. commit

must occur atomically.

If anything fails:

nothing is reserved.

Use SERIALIZABLE isolation or rigorously equivalent locking where needed.

Test heavy contention.

---

# PART 23 — CONCURRENCY TORTURE TEST

Write an automated test:

Account:

$10,000 available.

Create:

100 concurrent agents.

Each attempts:

$500 reservation.

Expected:

exactly 20 successful reservations.

Total reserved:

$10,000.

Available:

$0.

No negative balance.

No duplicate reservation.

No oversubscription.

Repeat with randomized timing thousands of times.

Use the Go race detector where applicable.

---

# PART 24 — CAPITAL ENVELOPE

Every autonomous strategy runs inside a CapitalEnvelope.

Fields should include:

- account_id
- agent_id
- strategy_version_id
- allocation
- available
- reserved
- deployed
- realized_pnl
- realized_loss
- current_drawdown
- daily_loss
- max_daily_loss
- max_drawdown
- max_single_trade
- max_position
- allowed_instruments
- allowed_asset_classes
- allowed_venues
- max_model_spend
- max_data_spend
- max_order_rate
- policy_version
- status
- effective_at
- expires_at

Agent can consume envelope.

Agent cannot mutate authority fields.

---

# PART 25 — BUYING-POWER ENGINE

Build a dedicated domain module.

Its job is NOT:

"sum wallet balances."

Its job is:

> Determine what value the account may legally, operationally, financially, and safely deploy for this requested action right now.

Inputs may include:

- wallet balances
- pending funding
- funding reversibility
- withdrawal hold
- active reservations
- asset collateral factor
- asset health
- stablecoin market price
- chain status
- provider status
- venue eligibility
- account restrictions
- position exposure
- risk policy
- settlement feasibility
- quote requirements

Output:

```text
portfolio_value
buying_power
available_now
reserved
pending
withdrawable
underlying_balances
haircuts
restrictions
policy_version
as_of
```

Do not cache this as financial truth in Redis.

Cache derived UI responses if necessary with short TTL and invalidation.

---

# PART 26 — STABLECOIN RISK

Never hardcode:

`1 USDC = $1 forever`

Implement status:

NORMAL
DEGRADED
RESTRICTED
HALTED

and configurable collateral factors.

If peg/liquidity/redeemability data becomes unhealthy:

reduce buying-power contribution.

Possible policy:

NORMAL:
standard collateral

DEGRADED:
mark market price
apply haircut

RESTRICTED:
no new risk

HALTED:
block affected operations

Record the policy version used for decisions.

---

# PART 27 — FUNDING RISK

Funding status and withdrawal status must be distinct.

Represent:

- provider authorization
- provider settlement
- chain settlement
- fraud state
- reversibility
- buying-power eligibility
- withdrawal eligibility

Do not assume:

funds can trade

means:

funds can withdraw.

If provider terms eventually allow trading before complete reversibility expires, the ledger must support a later reversal.

A later funding reversal must:

- post a compensating financial event
- freeze or restrict account if deficit occurs
- prohibit new risky activity
- create operational alert
- retain full audit trail

Never silently make ledger negative without an explicit deficit account classification.

---

# PART 28 — FUNDING STATE MACHINE

Implement explicit transition validation.

Example:

```text
CREATED
↓
SESSION_CREATED
↓
CUSTOMER_ACTION_REQUIRED
↓
PROVIDER_PROCESSING
↓
PROVIDER_CONFIRMED
↓
SETTLEMENT_OBSERVED
↓
RECONCILED
↓
AVAILABLE
```

Possible side states:

FAILED
EXPIRED
CANCELLED
REVERSED
REVIEW_REQUIRED

Every transition:

- validated
- timestamped
- audited

Illegal transitions must fail.

---

# PART 29 — FUNDING PROVIDER

Implement Stripe fiat-to-crypto onramp behind:

`FundingProvider`

Do NOT integrate generic Stripe card payment as a substitute.

The critical reason is the reversible-card / irreversible-crypto risk.

Implement production code only against current official provider contracts.

Verify provider schemas.

Do not invent fields.

If credentials/approval are unavailable:

adapter may be:

CODE_READY
SANDBOX_TESTED

but not:

LIVE_VERIFIED.

---

# PART 30 — FUNDING WEBHOOKS

Webhook ingestion must:

1. preserve raw request
2. verify provider signature
3. validate timestamp/replay rules
4. hash payload
5. identify provider event ID
6. insert canonical inbox record under unique constraint
7. safely acknowledge duplicate event
8. process idempotently
9. emit canonical domain event via outbox
10. retain processing/error state

Do not perform non-idempotent money operations directly from an unpersisted HTTP handler.

---

# PART 31 — TRANSACTIONAL OUTBOX + INBOX

Required.

## OUTBOX

Financial state mutation and domain event creation occur in same Postgres transaction.

Worker publishes events to Redpanda.

Published state is persisted.

Retries are idempotent.

## INBOX

Consumers persist:

- message ID
- source
- schema version
- processed state

Duplicate delivery produces no duplicate economic effect.

Assume Redpanda is at-least-once.

Consumers must be idempotent.

---

# PART 32 — UNIVERSAL ASSET / INSTRUMENT MODEL

Do not use string ticker as identity.

Required separation:

```text
Asset
EconomicExposure
Instrument
Venue
VenueListing
SettlementRules
ExternalIdentifier
```

Crypto identity:

chain/network
+
mint/contract address

Instrument stores:

- internal UUID
- type
- canonical name
- base asset
- quote asset
- settlement asset
- risk class
- active interval
- policy reference
- metadata version

VenueListing stores:

- venue
- venue-native ID
- network
- mint/contract
- tick
- precision
- minimum notional
- status
- settlement rules

---

# PART 33 — ASSET SAFETY STATE

Add explicit status:

ACTIVE
CLOSE_ONLY
RESTRICTED
HALTED
DELISTING
DELISTED

Risk Kernel must distinguish:

"prevent increasing exposure"

from:

"prevent all transactions."

In an emergency, `CLOSE_ONLY` may permit reducing risk while prohibiting buys.

This must be policy driven.

---

# PART 34 — SOLANA TOKEN SAFETY

Do not blindly sign any arbitrary Solana transaction returned by a routing provider.

Signing Service must independently inspect transaction instructions.

At minimum validate:

- expected wallet
- expected fee payer
- expected token inputs
- maximum token debit
- expected output token
- minimum output
- allowed programs
- allowed token programs
- prohibited authority changes
- no unknown SystemProgram value transfers
- no unexpected destination
- no arbitrary program invocation outside approved plan
- no unexpected delegate/approval
- no wallet ownership changes
- recent blockhash/expiry
- plan identity/hash
- quote identity
- slippage bounds

If Token-2022 or specialized token extensions are unsupported:

reject by default.

Explicitly add support later only after understanding:

- transfer fees
- transfer hooks
- freeze rules
- confidential transfers
- restrictions
- extensions affecting balance semantics

# NO BLIND SIGNING.

Even compromise of Jupiter or another upstream route provider must not automatically permit arbitrary value exfiltration.

---

# PART 35 — UNIVERSAL FINANCIAL INTENT

Define a typed internal representation.

V1 types:

AcquireNotional
ReduceNotional
ClosePosition
TargetExposure

Future reserved type:

BuyEventOutcome

but disabled.

Intent includes:

- intent_id
- account_id
- actor_type
- actor_id
- strategy_version
- prediction_id if autonomous
- action
- instrument_id
- notional
- constraints
- deadline
- requested_at
- idempotency_key
- correlation_id

Constraints:

- max_slippage_bps
- max_fee_bps
- max_price
- min_receive
- allowed_venues
- quote_freshness_ms
- execution_deadline
- max_price_impact_bps

---

# PART 36 — IDEMPOTENCY CONTRACT

For every money-affecting HTTP command:

Require:

`Idempotency-Key`

Persist:

- key
- actor
- endpoint
- canonical request hash
- resulting resource
- resulting status
- response metadata

Behavior:

Same key + same request:

return original semantic result.

Same key + different request:

reject with deterministic conflict error.

Never silently reuse a key for different financial intent.

---

# PART 37 — API ERROR MODEL

Use structured errors.

Prefer `application/problem+json` style semantics.

Every business rejection has stable machine-readable code.

Examples:

INSUFFICIENT_BUYING_POWER
ACCOUNT_FROZEN
ASSET_RESTRICTED
VENUE_UNAVAILABLE
QUOTE_EXPIRED
RISK_MAX_POSITION
RISK_DAILY_LOSS
ELIGIBILITY_JURISDICTION
CAPABILITY_NOT_APPROVED
SUBMISSION_STATE_UNKNOWN
RECONCILIATION_REQUIRED
PROVIDER_UNAVAILABLE
STALE_MARKET_DATA
INVALID_IDEMPOTENCY_REUSE

Do not expose stack traces.

---

# PART 38 — SETTLEMENT COMPILER

This is central proprietary logic.

It must be deterministic.

It must be typed.

It must be testable without an LLM.

Input:

TradeIntent
+
account state
+
buying-power state
+
instrument metadata
+
eligible venues
+
system health

Output:

ExecutionPlan.

Example:

```text
ValidateEligibility
↓
EvaluateRisk
↓
ReserveCapital
↓
ResolveVenueListing
↓
LocateSettlementAsset
↓
AcquireQuote
↓
ValidateQuote
↓
FinalRiskCheck
↓
BuildTransaction
↓
InspectTransaction
↓
RequestSignature
↓
Submit
↓
ObserveFinality
↓
Reconcile
↓
PostLedger
↓
UpdatePosition
↓
ReleaseReservation
```

---

# PART 39 — EXECUTION PLAN

Every plan is immutable after approval.

Create new plan version if replanning occurs.

Plan fields:

- plan ID
- intent ID
- plan version
- planner version
- created timestamp
- hard constraints
- estimated costs
- selected venue
- selected settlement asset
- steps
- policy versions
- risk decision
- instrument version
- quote reference
- hash

PlanStep:

- id
- type
- dependency IDs
- state
- semantic idempotency key
- retry class
- timeout policy
- finality policy
- compensation policy
- evidence inputs
- evidence output

---

# PART 40 — NO VALID PLAN

Settlement Compiler is allowed to return:

`NO_VALID_PLAN`

This is a legitimate result.

Examples:

- money on wrong settlement rail
- deadline impossible
- quote too expensive
- venue disabled
- liquidity insufficient
- required provider degraded
- eligibility mismatch
- risk violation

Never "force" a route to satisfy product UX.

---

# PART 41 — EXECUTION ADAPTER CONTRACT

Create provider-neutral interface approximately:

```go
type ExecutionAdapter interface {
    Quote(ctx context.Context, req QuoteRequest) (Quote, error)
    ValidateQuote(ctx context.Context, quote Quote) error
    Build(ctx context.Context, req BuildRequest) (UnsignedAction, error)
    Submit(ctx context.Context, req SignedSubmission) (SubmissionResult, error)
    Status(ctx context.Context, ref ExternalReference) (ExecutionStatus, error)
    Cancel(ctx context.Context, ref ExternalReference) error
    Reconcile(ctx context.Context, scope ReconcileScope) ([]ExternalExecutionEvent, error)
}
```

Provider implementation owns:

- venue protocol
- route decoding
- transaction expiry
- partial fill semantics
- fee interpretation
- provider request IDs
- status mapping
- cancellation semantics

Core Settlement Compiler does not contain provider-specific hacks.

---

# PART 42 — QUOTE MODEL

Quote must store:

- quote ID
- provider
- provider request ID
- instrument
- input asset
- input quantity
- output asset
- expected output
- minimum output
- effective price
- price impact
- estimated network cost
- estimated venue fee
- platform fee if any
- received_at
- expires_at
- route metadata hash
- raw response reference

UI shows:

- expected receive
- estimated fees
- slippage
- route where appropriate
- quote expiry

No hidden spread.

---

# PART 43 — JUPITER

Use Jupiter as V1 Solana execution router.

Verify current official API at implementation time.

Do NOT write integration from memory if documentation is available.

Store request/response evidence.

Do not make Jupiter's internal schema leak throughout system.

Jupiter implementation is one adapter.

---

# PART 44 — SOLANA SUBMISSION

Execution flow:

1. obtain quote
2. verify freshness
3. build candidate transaction
4. deserialize transaction
5. inspect instructions
6. simulate transaction where appropriate
7. compare predicted state changes to approved plan
8. enforce fee/slippage/program limits
9. sign through bounded signing service
10. submit
11. persist submission evidence
12. observe status
13. reconcile independently

No blind transaction signing.

---

# PART 45 — FINALITY POLICY

Finality must be policy-driven.

Different action classes can require different confidence.

Examples:

UI "submitted" can appear earlier.

Funding availability may require stronger evidence.

Position finality may differ from immediate provisional display.

Represent explicitly:

SUBMITTED
OBSERVED
CONFIRMED
FINALIZED

Do not collapse all into:

SUCCESS.

---

# PART 46 — EXACTLY-ONCE ECONOMIC EFFECT

Distributed exactly-once execution is not assumed.

Achieve exactly-once economic effect via:

- semantic idempotency
- unique intent
- immutable execution attempts
- provider identifiers
- transaction signature
- capital reservation
- durable state machine
- status recovery
- reconciliation
- compensation

Never blindly retry money movement.

---

# PART 47 — ORDER STATE MACHINE

Implement explicit transition table.

```text
CREATED
↓
VALIDATED
↓
CAPITAL_RESERVED
↓
PLANNED
↓
SUBMITTING
↓
SUBMITTED
↓
ACKNOWLEDGED
↓
PARTIALLY_FILLED
↓
FILLED
↓
SETTLING
↓
SETTLED
```

Side states:

REJECTED
EXPIRED
CANCEL_REQUESTED
CANCELLED
SUBMISSION_UNKNOWN
RECONCILIATION_REQUIRED
FAILED_FINAL

Illegal transitions fail.

Transitions create events.

---

# PART 48 — THE TIMEOUT RULE

Absolute requirement:

# TRANSPORT TIMEOUT IS NOT EXECUTION FAILURE.

When submit times out:

```text
SUBMISSION_UNKNOWN
```

Then:

- keep reservation
- do not submit duplicate
- query provider
- query chain
- search transaction signature if available
- inspect wallet activity
- inspect fills
- reconcile

If execution exists:

adopt it.

If proven absent:

retry under same semantic context.

If uncertainty remains:

operator/reconciliation path.

---

# PART 49 — REQUIRED CRASH TEST

Automate:

1. user has $100
2. reserve $50
3. transaction submits successfully externally
4. crash process before local success persistence
5. restart
6. state is uncertainty
7. no duplicate transaction is sent
8. external truth is discovered
9. exactly one fill is persisted
10. exactly one position change occurs
11. correct ledger entries post
12. unused reservation releases
13. audit chain contains complete recovery evidence

This test must exist before production readiness can be claimed.

---

# PART 50 — RECONCILIATION IS CORE LOGIC

Implement three modes:

## EVENT-DRIVEN RECONCILIATION

After every execution/funding action.

## PERIODIC INCREMENTAL RECONCILIATION

Provider/account windows.

## FULL BALANCE RECONCILIATION

Periodic complete comparison.

Compare:

- expected wallet balance
- actual wallet balance
- expected fills
- actual transaction data
- expected fees
- actual fees
- expected positions
- actual asset units
- ledger state
- reservations

---

# PART 51 — RECONCILIATION STATE

Model:

OPEN
MATCHED
MISMATCH
INVESTIGATING
RESOLVED_AUTOMATIC
RESOLVED_MANUAL
ESCALATED

Every manual resolution requires:

- operator
- reason
- evidence
- approval if material
- compensating posting where needed

Never modify financial state invisibly.

---

# PART 52 — KILL SWITCH SEMANTICS

Kill switches stop NEW RISK.

They must NOT prevent:

- reading external state
- processing fills already received
- settlement
- reconciliation
- ledger posting
- risk-reducing cleanup where policy permits

Example:

GLOBAL_KILL

should stop new submissions.

It should not make the system blind to already-executed transactions.

This separation is critical.

---

# PART 53 — EMERGENCY CONTROLS

Implement:

GLOBAL_NEW_RISK_KILL
ACCOUNT_FREEZE
AGENT_PAUSE
STRATEGY_VERSION_DISABLE
VENUE_DISABLE
INSTRUMENT_CLOSE_ONLY
INSTRUMENT_HALT
CHAIN_DISABLE_NEW_ACTIONS
PROVIDER_DISABLE_NEW_ACTIONS
FUNDING_DISABLE
WITHDRAWALS_DISABLE
MODEL_DISABLE

Activation of safety kill should be fast.

Re-enabling after severe/global kill should require stronger approval.

---

# PART 54 — PRODUCTION CAPABILITY GATES

External legal/commercial questions must become software-enforced gates.

Capability examples:

LIVE_FUNDING
LIVE_MANUAL_TRADING
LIVE_AGENT_TRADING
WITHDRAWALS
SOCIAL_DATA_PERSISTENCE
MARKETPLACE
CROSS_CHAIN
PREDICTION_MARKETS
SECURITIES
CEX_TRADING

Fields:

- capability
- environment
- state
- approval version
- legal review reference
- provider contract reference
- risk approval
- security approval
- approvers
- evidence hashes
- effective date
- expiry
- revocation

States:

DISABLED
PENDING_APPROVAL
APPROVED
ACTIVE
SUSPENDED
REVOKED
EXPIRED

---

# PART 55 — CAPABILITY ACTIVATION

Production capability cannot become ACTIVE from:

one environment variable.

Require:

deployment config
+
persistent capability approval
+
valid non-expired evidence

High-risk activation requires dual authorization.

Agent cannot access this subsystem.

---

# PART 56 — ELIGIBILITY ENGINE

Eligibility evaluation:

Identity state
×
age
×
jurisdiction
×
residency
×
sanctions/compliance state
×
account restrictions
×
instrument
×
asset class
×
venue
×
provider restrictions
×
transaction context
×
production capability state

Return:

eligible
policy version
reason codes
timestamp
evaluated context hash

Persist every trade attempt's eligibility result.

---

# PART 57 — AGE/JURISDICTION

Never hardcode assumption:

"all U.S. users can trade."

Eligibility policy must support:

- state-level restrictions
- age restrictions
- provider-specific restrictions
- product-specific restrictions

Unknown policy:

FAIL CLOSED.

---

# PART 58 — DETERMINISTIC RISK KERNEL

Risk Kernel cannot call an LLM to decide permission.

RiskPolicy is versioned.

Every RiskDecision stores:

- policy version
- policy hash
- relevant account values
- relevant market values
- decision
- reason codes
- timestamp
- intent
- quote
- resulting constraints

---

# PART 59 — RISK LAYERS

## COMPILE-TIME

- forbidden capabilities
- unsupported instrument
- unbounded logic
- missing policy
- invalid dependency
- unknown effect

## PRE-TRADE

- available capital
- capital envelope
- max trade
- max position
- concentration
- instrument status
- venue status
- liquidity
- slippage
- fees
- quote age
- trading frequency
- self-trade risk
- eligibility
- provider health

## CONTINUOUS

- drawdown
- daily loss
- total exposure
- data/model spend
- strategy malfunction
- stale data
- provider degradation
- unresolved reconciliation

## EMERGENCY

kill switches.

---

# PART 60 — RISK CANNOT SELF-MODIFY

Agent cannot request:

"increase my max position."

Strategy author cannot use prompt text to bypass policy.

Runtime must always load independently controlled RiskPolicy.

---

# PART 61 — STRATEGY V1

No arbitrary live user code.

Support:

## NATURAL-LANGUAGE AUTHORING

Natural language
→
model compiler
→
typed AST

## TYPESCRIPT SDK

Developer-defined typed structure
→
same AST/IR

Both converge into identical deterministic runtime representation.

---

# PART 62 — STRATEGY IR

Include:

- schema version
- strategy ID
- strategy version
- immutable hash
- owner
- triggers
- signals
- conditions
- actions
- dependencies
- risk policy reference
- model budget
- data budget
- capital envelope requirements
- effect set
- lineage
- build timestamp

No unbounded loops.

No arbitrary eval.

No shell.

No arbitrary file access.

---

# PART 63 — EFFECT SYSTEM

Represent capabilities explicitly.

Allowed:

READ_MARKET_DATA
READ_ONCHAIN_DATA
READ_APPROVED_SOCIAL_DATA
READ_WALLET_INTELLIGENCE
CALL_MODEL
COMMIT_PREDICTION
CREATE_TRADE_INTENT

Forbidden:

RAW_SIGN
TRANSFER_VALUE
WITHDRAW
CHANGE_RISK
CHANGE_CAPITAL
EXPORT_SECRET
ARBITRARY_NETWORK
ARBITRARY_CONTRACT_CALL
MODIFY_CAPABILITY_GATE
ACCESS_ADMIN_API

Compiler rejects forbidden effect.

Runtime rejects again independently.

Defense in depth.

---

# PART 64 — NATURAL-LANGUAGE COMPILER SAFETY

Model output is untrusted.

Pipeline:

input
→
prompt template
→
schema constrained response
→
parse
→
structural validation
→
type validation
→
effect validation
→
risk compatibility validation
→
human-visible compiled form

Invalid model output:

does NOT execute.

May retry compilation a bounded number of times.

All attempts recorded.

---

# PART 65 — MODEL PROVENANCE

Store:

- provider
- model identifier
- prompt template version
- request timestamp
- response timestamp
- input hash
- output hash
- structured output
- parse result
- usage/cost
- strategy version

Do not store or expose hidden chain-of-thought.

Store structured user-relevant explanation and evidence.

---

# PART 66 — TOOL BROKER

Agents access data through:

ToolBroker.

Never direct API secrets.

Tool invocation records:

- tool
- version
- request hash
- output hash
- source
- received time
- cost
- success/failure
- latency

Tool Broker enforces:

- permissions
- budgets
- rate limits
- egress policy
- provenance

---

# PART 67 — PROMPT INJECTION BOUNDARY

External:

- X posts
- news
- Telegram
- blockchain metadata
- token descriptions

are DATA.

They are never instructions.

Model prompts must separate:

SYSTEM POLICY
TOOL RESULTS
UNTRUSTED CONTENT

Do not permit external content to grant tools.

Never expose secrets to model context.

---

# PART 68 — AGENT LIFECYCLE

State machine:

DRAFT
↓
COMPILED
↓
VALIDATED
↓
BACKTEST_ELIGIBLE
↓
SHADOW
↓
CANARY
↓
LIMITED
↓
LIVE

Side states:

PAUSED
FAILED
REVOKED
SUPERSEDED

Promotion cannot skip stages for autonomous real-capital strategies.

---

# PART 69 — CAPITAL CI/CD

Code deployment != capital deployment.

New code may reach production environment while strategy remains SHADOW.

Promotion requires evidence.

At each stage store:

- strategy version
- evaluation dataset
- risk policy
- performance
- error rates
- operational health
- approval

---

# PART 70 — PROMOTION GATES

Example:

SHADOW → CANARY requires:

- compile success
- financial property tests
- historical replay complete
- no forbidden effects
- no critical security issues
- risk policy attached
- no unresolved data dependency failure

CANARY → LIMITED requires:

- successful real execution evidence
- reconciliation healthy
- no policy violation
- no unexpected submission
- no material mismatch

LIMITED → LIVE:

must require stronger operational approval.

Do not hardcode marketing-driven automatic promotion.

---

# PART 71 — AGENT PAUSE SEMANTICS

Pausing an agent:

- prevents NEW intents
- retains historical state
- existing submitted transactions continue reconciliation
- open cancelable orders may be safely cancelled by explicit workflow
- settlement continues
- ledger posting continues

Never abandon an external transaction because the strategy was paused.

---

# PART 72 — PREDICTION LEDGER

Before consequential autonomous trade:

commit structured forecast.

Required:

- prediction ID
- strategy version
- agent version
- instrument
- horizon
- probability direction/upside if applicable
- expected return
- downside probability
- confidence
- information set hash
- decision-available timestamp

Prediction must predate execution.

---

# PART 73 — CALIBRATION

Compute later:

- Brier score where applicable
- log loss where appropriate
- calibration buckets
- expected vs realized
- confidence vs error
- regime splits

Do not equate:

high return

with:

good prediction calibration.

---

# PART 74 — POINT-IN-TIME REALITY ENGINE

This system is strategic.

Every piece of data needs knowledge-time semantics.

Minimum timestamps:

source_event_at
provider_published_at
nodal_received_at
normalized_at
feature_available_at
decision_available_at

A historical replay can only use information where:

decision_available_at <= simulated decision time.

---

# PART 75 — RAW DATA ARCHIVE

Raw external provider payload:

store in S3.

Record:

- object URI
- hash
- provider
- source event ID
- ingest timestamp
- schema version

Do not transform away original evidence.

---

# PART 76 — NORMALIZED DATA

Normalized events:

ClickHouse.

Financial command truth:

Postgres.

Do not use ClickHouse as authoritative live money store.

---

# PART 77 — EVENT STREAM

Redpanda carries normalized high-volume events.

Maintain:

- schema version
- sequence where source supports it
- source offsets
- received time
- dedup ID

Detect:

- stream gap
- reconnect
- replay
- ordering anomaly

---

# PART 78 — HELIUS

Use Helius behind:

`SolanaDataProvider`

Primary role:

- stream on-chain events
- RPC/transaction observation

Maintain independent fallback RPC interface.

Do not assume Helius observation alone overrides blockchain truth in all failure scenarios.

---

# PART 79 — PROVIDER HEALTH

Every provider has health states:

HEALTHY
DEGRADED
UNHEALTHY
DISABLED

Risk/Settlement Compiler consumes health.

If data required by a strategy is stale:

no trade.

If execution provider unhealthy:

no new submission.

If observation provider unhealthy:

do not falsely finalize.

---

# PART 80 — BACKTESTING REPRODUCIBILITY

Every backtest run must store:

- strategy version hash
- strategy IR schema
- simulator version
- dataset manifest hash
- data cutoff
- model mode
- risk policy
- fee model
- slippage model
- latency model
- random seed
- run timestamp

Same deterministic inputs should reproduce same result where simulation is deterministic.

---

# PART 81 — BACKTEST LABELS

Never conflate:

BACKTEST
PAPER
SHADOW
CANARY
LIVE

These must be immutable performance dimensions.

UI cannot aggregate them into one return number.

---

# PART 82 — POINT-IN-TIME VALIDITY

Backtest report states:

POINT-IN-TIME VALID

or:

RESEARCH-ONLY / TEMPORALLY IMPURE

If current LLM analyzes old text using knowledge unavailable historically:

label accordingly.

Never market it as true historical performance.

---

# PART 83 — EXECUTION SIMULATION

Historical execution should model as data permits:

- fees
- quote age
- spread
- liquidity
- order size
- slippage
- price impact
- network latency
- failed submission
- priority fee
- transaction expiry
- partial fill where relevant
- asset lifecycle

Do not invent unavailable precision.

Label assumptions.

---

# PART 84 — COUNTERFACTUALS

Possible shadow branches:

NO_TRADE
ENTRY_PLUS_5S
ENTRY_PLUS_30S
HALF_SIZE
DOUBLE_SIZE
ALTERNATIVE_ROUTE

Store as estimates.

Never include counterfactual gains in realized performance.

---

# PART 85 — POSITION ENGINE

Represent:

- asset units
- acquisition lots where appropriate
- average basis for UI if chosen
- exact fills
- venue/custody location
- realized P&L
- unrealized P&L
- fees

Preserve enough lot-level data for later tax reporting.

---

# PART 86 — PERFORMANCE ENGINE

Metrics:

- realized return
- unrealized return
- drawdown
- Sharpe where statistically meaningful
- Sortino
- turnover
- hit rate
- slippage
- execution quality
- prediction calibration
- strategy age
- live capital history
- model cost
- data cost
- compute cost

Always state mode.

---

# PART 87 — PROOF SYSTEM

Implement tamper-evident financial history without introducing unnecessary blockchain infrastructure.

Canonical evidence record
↓
canonical serialization
↓
hash
↓
previous hash reference
↓
KMS signature
↓
append-only stream
↓
periodic Merkle root
↓
immutable S3 archive

Use deterministic canonical serialization.

Hashing the same logical record must not change because JSON field ordering changed.

---

# PART 88 — PROOF CONTENT

Include hashes/references for:

- strategy version
- Strategy IR
- model identifier
- information set
- Prediction
- EligibilityDecision
- RiskDecision
- TradeIntent
- ExecutionPlan
- quote
- transaction
- external transaction signature
- fill
- JournalTransaction

Provide verification utility.

---

# PART 89 — AUDIT EVENT

AuditEvent:

- actor
- actor type
- action
- resource
- before hash where relevant
- after hash
- request correlation
- policy version
- reason
- timestamp
- source IP/device where lawful/relevant
- evidence reference

Money-affecting path audit completeness target:

100%.

---

# PART 90 — AUTHENTICATION

Use a standards-based production authentication architecture.

If existing secure provider exists:

integrate it.

If no provider decision exists:

implement provider abstraction rather than inventing insecure auth.

Architecture must support:

- secure sessions
- MFA
- passkey/WebAuthn capability
- session revocation
- device/session listing
- short-lived service credentials
- step-up authentication

Do not implement homemade password cryptography if mature identity infrastructure is available.

---

# PART 91 — AUTHORIZATION

RBAC + contextual authorization.

Roles:

CUSTOMER
SUPPORT_READ_ONLY
OPERATIONS
RISK
COMPLIANCE
FINANCE
SECURITY
ADMIN

Break-glass privilege separately controlled.

No shared operator account.

Every admin action audited.

---

# PART 92 — TENANT ISOLATION

Every customer read/write path must be scoped.

Test:

User A cannot access:

- User B account
- wallet
- intent
- order
- position
- strategy
- agent
- prediction
- backtest
- audit evidence

Use explicit authorization tests.

Never trust ID obscurity.

---

# PART 93 — ADMIN BREAK GLASS

Break-glass:

- step-up auth
- narrow scope
- expiry
- reason
- audit
- notification

Global kill activation should be fast and should not require approval delay.

Re-enabling risky capability after emergency should require stronger approval.

---

# PART 94 — WITHDRAWAL BOUNDARY

Even if production fiat off-ramp is not V1-enabled, design separate withdrawal domain.

Withdrawal can NEVER be invoked through Agent capability.

Withdrawal requires:

- human authorization
- step-up authentication
- eligibility/security checks
- withdrawal capability gate
- destination validation
- velocity limits
- audit

If provider/custody/legal path is unresolved:

WITHDRAWALS capability remains disabled.

---

# PART 95 — SIGNING SERVICE

Signing Service is isolated from strategy runtime.

It receives:

bounded request referencing:

- approved intent
- approved plan
- approved risk decision
- approved wallet
- expected transaction

It independently validates all relevant constraints.

Only then can it ask external wallet provider to sign.

Agent cannot call WalletProvider.

---

# PART 96 — WALLET PROVIDER

Use an abstraction capable of Privy-style delegated embedded wallets.

Do not assume production security semantics beyond verified provider functionality.

Define:

WalletProvider
SigningProvider

separately if useful.

Prod startup fails closed if provider capability needed for live signing is unverified.

---

# PART 97 — PROVIDER MOCK SEPARATION

Test fakes belong in:

test/dev-only packages.

Production binary must reject:

provider mode = fake

when:

environment == production.

Do this programmatically.

Not by convention.

---

# PART 98 — CONFIGURATION SYSTEM

Typed configuration.

Fail startup if required production configuration missing.

Never silently use development defaults in prod.

Explicit environment:

LOCAL
TEST
DEV
STAGING
PROD

Examples of production-only validation:

- no fake providers
- no debug auth
- no seeded balances
- no insecure TLS
- no permissive CORS wildcard
- audit archive configured
- KMS configured
- capability database configured
- database TLS configured

---

# PART 99 — SECRETS

Production:

AWS Secrets Manager / appropriate secret infrastructure.

No production secrets in:

- repo
- Docker image
- frontend
- logs
- Terraform state plaintext where avoidable

Use IAM task roles.

Avoid static AWS keys.

Use GitHub OIDC for deployment where possible.

---

# PART 100 — IAM BLAST RADIUS

Separate task roles.

Examples:

API:
no wallet signing privilege unless absolutely necessary.

Execution worker:
bounded signing capability.

Market ingest:
no financial write privilege beyond needed ingest state.

Reconciliation:
financial read + controlled reconciliation workflows.

Agent runtime:
NO signing credentials.

Frontend:
none.

Least privilege.

---

# PART 101 — DATABASE ROLES

Separate:

migration role
application role
read-only analytics role
operations/reporting role where appropriate

Application role should not have arbitrary schema modification.

---

# PART 102 — NETWORK ARCHITECTURE

Production:

- private database
- private Redis
- internal workers private
- public entry only through controlled edge
- WAF
- load balancer/API ingress
- TLS
- restricted security groups
- no public database
- no open management ports

---

# PART 103 — CONTAINER SECURITY

Production containers:

- minimal base images
- non-root
- read-only filesystem where possible
- dropped Linux capabilities
- no shell where unnecessary
- pinned dependency versions
- health checks
- resource limits

Scan with modern container tooling.

---

# PART 104 — SOFTWARE SUPPLY CHAIN

CI must include:

- secret scan
- dependency vulnerability scan
- Go vulnerability scan
- frontend dependency audit
- SAST
- IaC scan
- container scan
- SBOM generation

Prefer signed release containers.

Generate provenance where practical.

---

# PART 105 — PROVIDER ABSTRACTION REQUIREMENT

At minimum define:

FundingProvider
WalletProvider
SigningProvider
ExecutionAdapter
MarketDataProvider
ChainObserver
ModelProvider
EventBus
WorkflowEngine
ObjectArchive
NotificationProvider

Keep provider types from leaking into core domain.

---

# PART 106 — PROVIDER RETRY MATRIX

Not all operations may retry.

Explicitly categorize.

## SAFE RETRY

idempotent reads.

## IDEMPOTENT WRITE

only when provider idempotency supported.

## UNKNOWN-EFFECT WRITE

do not retry until status investigation.

Codify this distinction.

---

# PART 107 — CIRCUIT BREAKERS

Provider breaker should prevent NEW external operations while preserving:

- reconciliation
- status reads
- observation

A provider disable cannot make existing transactions invisible.

---

# PART 108 — API DESIGN

Use versioned REST API.

Prefer `/v1/...`.

Commands:

POST with Idempotency-Key.

Queries:

GET.

Use pagination.

Avoid returning unlimited histories.

Use cursor pagination where large/order-dependent.

---

# PART 109 — REALTIME CLIENT STREAMS

For balances/orders/agent state:

SSE or WebSocket.

Include:

- event ID
- sequence/cursor
- resume semantics
- reconnect handling

Client must recover by refetching canonical REST state after stream gap.

Realtime stream is not authoritative.

---

# PART 110 — FRONTEND FINANCIAL TRUTH

Every displayed number should map to a backend-defined financial concept.

No client-side re-creation of financial math where backend authoritative output exists.

UI may format.

Backend computes.

Show `as of` when relevant.

---

# PART 111 — CUSTOMER UX

Required pages:

## HOME

- portfolio value
- buying power
- available now
- reserved
- pending
- P&L
- underlying assets
- agent status

## ADD FUNDS

- funding method
- provider session
- pending state
- completed state
- funding history
- clear underlying USDC disclosure

## TRADE

- instrument
- exact mint identity/details
- current quote
- expected receive
- fees
- price impact
- slippage
- amount
- risk/error messages
- order state

## PORTFOLIO

- holdings
- units
- USD mark
- basis
- realized/unrealized P&L
- underlying location where relevant

## STRATEGY BUILDER

- natural language
- structured editor
- compile
- validation
- effects
- capital
- risk

## AGENTS

- agent status
- strategy version
- capital envelope
- live positions
- decisions
- predictions
- costs
- risk events
- P&L

## LAB

- historical replay
- shadow
- comparison
- counterfactuals

## ACTIVITY

timeline:

data event
→ prediction
→ intent
→ eligibility
→ risk
→ execution
→ fill
→ reconciliation

## SETTINGS / SECURITY

- sessions
- security
- funding
- wallets
- disclosures
- relevant risk preferences

---

# PART 112 — UX HONESTY

Never show:

"cash"

for stablecoin.

Never show:

"live performance"

for backtest.

Never show:

"guaranteed."

Never imply:

model confidence = probability of profit.

Never hide:

pending settlement.

Never show fake balances.

Never add casino-style confetti for trades.

---

# PART 113 — ACCESSIBILITY

Target strong WCAG AA behavior.

Support:

- keyboard
- visible focus
- semantic structure
- screen reader labels
- contrast
- reduced motion
- responsive layout

---

# PART 114 — FRONTEND STACK QUALITY

Strict TypeScript.

Avoid `any` without justification.

Use schema validation at API boundary.

Prefer generated API client from OpenAPI so contracts cannot drift easily.

Use robust query/cache library.

Server authoritative data.

Test critical flows with Playwright.

---

# PART 115 — TEMPORAL

Use Temporal only where durable workflow semantics matter.

Examples:

- funding lifecycle
- reconciliation escalation
- strategy promotion
- canary promotion
- controlled withdrawal
- provider remediation
- later cross-chain settlement

Do not use a Temporal workflow per market tick.

---

# PART 116 — TEMPORAL FINANCIAL RULE

Workflow state is NOT authoritative balance.

Temporal orchestrates.

Postgres records authoritative financial state.

Workflow retry must be safe against already-completed external activity.

---

# PART 117 — REDPANDA

Use event streaming for:

- market data
- normalized blockchain data
- high-volume internal domain events

Critical financial mutation first commits Postgres + outbox.

Do not make successful event publication prerequisite to Postgres transaction durability.

---

# PART 118 — CLICKHOUSE

Use for:

- market history
- normalized events
- strategy decision analysis
- analytics
- backtests
- telemetry-oriented datasets

Do NOT use ClickHouse to determine authoritative available balance.

---

# PART 119 — REDIS

Allowed:

- cache
- rate limit
- ephemeral session/supporting state
- temporary computation cache

Not allowed:

- authoritative balance
- financial reservation
- ledger
- irreversible state machine authority

---

# PART 120 — DATA LICENSING

Every external dataset should have:

- provider
- retention policy
- redistribution policy
- historical use permission
- environment capability

If legal rights unknown:

do not silently build permanent historical dependence.

Mark persistence capability blocked.

---

# PART 121 — PRIVACY BOUNDARIES

Separate:

identity/PII

from:

financial state

from:

market/strategy telemetry.

Core financial tables should reference identity IDs instead of unnecessary raw PII.

Apply encryption.

Restrict roles.

Do not leak PII into ClickHouse unnecessarily.

---

# PART 122 — RETENTION CLASSES

Create configurable classes:

FINANCIAL_RECORD
SECURITY_AUDIT
RAW_MARKET_DATA
SOCIAL_DATA
MODEL_IO
OPERATIONAL_LOG

Retention differs.

Do not hardcode one forever value.

Production cannot set critical audit retention to zero.

---

# PART 123 — AUDIT WORM

Evidence archive:

S3 with immutable retention configuration appropriate to deployment.

Use KMS.

Record object hash.

Periodic verification job checks evidence integrity.

---

# PART 124 — S3 RAW DATA

Suggested partition:

provider/
event_type/
YYYY/
MM/
DD/
HH/

but use an efficient final layout.

Object metadata should permit provenance reconstruction.

---

# PART 125 — TAX-QUALITY RECORDS

Preserve:

- asset ID
- quantity
- acquisition time
- disposition time
- transaction reference
- venue/wallet
- proceeds
- fees
- cost basis source
- valuation source
- corrections

Do not claim actual tax filing unless partner exists.

---

# PART 126 — FEE ENGINE

Platform fee should be explicit.

Create FeePolicy.

Quote displays:

venue fee
network estimate
platform fee
total estimated cost

Ledger posts platform fees separately.

Do not create hidden spread unless business model explicitly and legally adopts it with appropriate disclosure.

Default may be zero until configured.

---

# PART 127 — PERFORMANCE COST ACCOUNTING

Track:

- model cost
- data cost
- execution fees
- network fees

Per:

strategy
agent
account

This enables later:

Net Economic Alpha.

---

# PART 128 — ADMIN PLANE

Build actual operator interface.

Features:

- search account
- inspect account
- freeze
- inspect funding
- inspect reservation
- inspect intent
- inspect order
- inspect reconciliation
- inspect strategy
- pause agent
- disable asset
- disable venue
- disable provider
- activate kill
- review capability gates
- approve/reject controlled administrative action

---

# PART 129 — NEVER EDIT BALANCE

No admin endpoint:

`PATCH /balance`

No SQL script workflow presented as standard repair.

Financial repair:

reason-coded JournalTransaction.

Material correction:

dual control.

Audit.

---

# PART 130 — SECURITY EVENTING

Important security events:

- login anomaly
- MFA change
- session revoke
- admin privilege use
- failed webhook signature
- provider credential change
- capability activation
- global kill
- signing rejection
- wallet policy violation
- cross-tenant access attempt

Record and alert appropriately.

---

# PART 131 — OBSERVABILITY

OpenTelemetry instrumentation across:

HTTP
gRPC
Temporal
provider calls
database
event processing

Trace identifiers:

request_id
correlation_id
intent_id
order_id
workflow_id

Avoid high-cardinality dangerous labels in metrics.

---

# PART 132 — FINANCIAL METRICS

Track:

ledger_posting_errors
reservation_conflicts
unknown_submissions
reconciliation_mismatches
oldest_unresolved_mismatch
provider_duplicate_events
duplicate_command_rejections
negative_deficit_accounts
risk_rejections
capability_gate_rejections

---

# PART 133 — EXECUTION METRICS

Track:

quote_latency
quote_age_at_submit
build_latency
simulation_latency
sign_latency
submit_latency
confirmation_latency
finality_latency
slippage
price_impact
execution_failure_rate
unknown_submission_rate

---

# PART 134 — AGENT METRICS

Track:

strategy evaluations
intent proposals
risk rejects
prediction count
model cost
tool cost
data freshness
decision latency
agent pauses
policy violations

---

# PART 135 — ALERT PRIORITIES

SEV1 candidates:

- ledger integrity violation
- unauthorized signing
- duplicate economic execution
- unexplained financial mismatch
- signing credential compromise
- cross-tenant financial access
- global money-impacting reconciliation drift

SEV2:

- provider outage
- chain-data disagreement
- sustained stale data
- elevated unknown submissions

Define runbooks.

---

# PART 136 — SLO OBJECTIVES

Use:

acknowledged ledger loss:
0

unauthorized agent transfer:
0

escaped risk-policy violation:
0

audit completeness for financial paths:
100%

core API:
>= 99.95% design objective

trade command durability after accepted:
99.999% design objective

kill switch:
seconds

Do not claim unmeasured SLO compliance.

---

# PART 137 — INFRASTRUCTURE

AWS V1.

Use Terraform.

Recommended:

CloudFront/WAF
↓
Load Balancer/API entry
↓
ECS/Fargate

plus:

RDS PostgreSQL Multi-AZ
ElastiCache Redis
S3
KMS
Secrets Manager
ECR
CloudWatch/OpenTelemetry

External managed:

Temporal
Redpanda
ClickHouse

where chosen.

---

# PART 138 — NO ACTIVE-ACTIVE MONEY DATABASE V1

Use one primary write region.

Multi-AZ.

Reliable backup/PITR.

Do not create global active-active financial writes before there is a real need and rigorous architecture.

---

# PART 139 — POSTGRES PRODUCTION

Enable:

- Multi-AZ
- encryption
- automated backups
- PITR
- monitoring
- reasonable connection management

Use connection pool.

No unlimited connection spawning.

---

# PART 140 — DATABASE MIGRATIONS

Every migration tested.

For large/busy table change:

consider:

- lock duration
- index build mode
- backfill
- rollout compatibility

Avoid destructive schema changes without staged migration.

Ledger history cannot be destroyed by rollback.

---

# PART 141 — BACKUP RESTORE

A backup that was never restored is not a proven backup.

Create restore drill documentation and automation where practical.

Verify:

- database can restore
- app can point to restored copy
- reconciliation can converge external state

---

# PART 142 — CI

Required checks:

Go format
Go vet
staticcheck
Go unit
Go race on critical packages
Go fuzz targets
frontend lint
frontend type check
frontend tests
financial property tests
migration tests
secret scan
dependency scan
SAST
IaC scan
container scan
SBOM

Do not allow known critical issue to silently pass.

---

# PART 143 — RELEASE SUPPLY CHAIN

Build immutable images.

Tag with:

git SHA.

Prefer signing images.

Generate SBOM.

Use deployment identity via GitHub OIDC where appropriate rather than static cloud credentials.

---

# PART 144 — STAGING

Staging is NOT production database with test users.

Separate:

- AWS resources
- database
- secrets
- provider config
- capability state

Use provider sandbox/test mode only where officially supported.

---

# PART 145 — LOCAL DEV

Provide easy commands.

Suggested:

```text
make dev
make stop
make test
make unit
make property
make integration
make e2e
make fuzz
make chaos
make load
make lint
make migrate
make seed
make build
```

Local infrastructure may include:

Postgres
Redis
Redpanda
ClickHouse
Temporal

via containers.

---

# PART 146 — DEV SEEDING

Safe fake user/assets/orders.

Clearly labeled.

Production refuses seed mode.

Production refuses fake balances.

---

# PART 147 — PROVIDER TESTING REALITY

Some financial providers do not provide exact production-equivalent sandboxes.

Do not invent one.

Use:

1. unit tests
2. contract tests with schema fixtures
3. official sandbox/test where available
4. replay fixtures
5. gated tiny canary in authorized production environment

Label each honestly.

---

# PART 148 — CONTRACT TESTS

For every provider:

- valid response
- invalid response
- timeout
- rate limit
- 5xx
- duplicate webhook
- unknown webhook event
- schema missing required field
- stale quote
- provider unexpected state

Adapters fail safely.

---

# PART 149 — UNIT TESTS

Financial math.

Ledger postings.

State transitions.

Risk.

Eligibility.

Intent compiler.

Planner.

Signing validator.

Prediction scoring.

Cost basis.

Provider normalization.

No critical financial code untested.

---

# PART 150 — PROPERTY TESTS

Use property testing heavily.

Examples:

for any valid journal transaction:
balance equation holds.

for any sequence of reservation/create/release:
capital conserved.

for arbitrary duplicate webhook count:
one economic effect.

for arbitrary concurrent reservations:
allocation never exceeded.

for arbitrary legal state-machine transitions:
forbidden state never reached.

---

# PART 151 — FUZZ TESTS

Fuzz:

- monetary amounts
- base units
- decimal scale
- route responses
- provider payload
- Solana transaction decoding
- idempotency keys
- timestamps
- state transitions
- JSON
- addresses
- mint metadata
- policy structures

Never panic on hostile external input.

---

# PART 152 — SOLANA TRANSACTION FUZZING

Especially fuzz transaction inspector.

Feed:

- unknown programs
- extra transfers
- malicious delegate instruction
- authority update
- unexpected token accounts
- malformed instructions
- unsupported token extensions

Expected:

reject.

---

# PART 153 — CHAOS TESTS

Automate controlled failure:

DB unavailable
DB failover
execution timeout
provider succeeded but response lost
duplicated provider callback
reordered events
event bus unavailable
Temporal retry
market stream disconnect
stale quote
wallet provider outage
RPC disagreement
LLM outage

System must preserve money invariants.

---

# PART 154 — LOAD TESTS

Use k6 or equivalent.

Test:

- portfolio reads
- quote load
- concurrent strategies
- event ingestion
- websocket clients
- reservation contention
- reconciliation throughput

Document measured results.

Do not invent numbers.

---

# PART 155 — SECURITY TESTS

Automate:

IDOR
cross-tenant reads
cross-tenant writes
CSRF if applicable
SSRF
prompt injection
webhook forgery
replay
agent withdrawal attempt
agent raw sign attempt
agent capital escalation
agent risk escalation
admin privilege misuse
production capability bypass
fake provider activation in production
JWT tampering
stolen session scenarios
malformed provider payload
dependency compromise simulations where practical

---

# PART 156 — THREAT MODEL

Maintain:

`docs/threat-model/THREAT_MODEL.md`

Use:

STRIDE-style analysis
+
financial abuse cases.

Threat actors:

customer
malicious strategy creator
compromised model
compromised employee
compromised provider
wallet attacker
account takeover attacker
supply chain attacker
API attacker

---

# PART 157 — INCIDENT RUNBOOKS

Create runbooks for:

wallet provider compromise
funding provider compromise
Helius outage
RPC disagreement
Jupiter outage
duplicated trade suspicion
unknown transaction
ledger mismatch
chargeback/reversal
model malfunction
stale market data
secret exposure
admin compromise
database corruption
Redpanda outage
Temporal outage
ClickHouse outage

---

# PART 158 — IMPORTANT OPERATIONAL DISTINCTION

If ClickHouse fails:

live financial trading may continue if required critical data remains available.

If Postgres financial database fails:

do not accept new financial commands.

If Risk Kernel fails:

do not accept new risk.

If reconciliation lags materially:

may halt new trading according to policy.

If model fails:

deterministic/manual product can remain operational.

Design graceful degradation intentionally.

---

# PART 159 — PERFORMANCE MODE INTEGRITY

Store mode directly with every:

agent run
trade
performance snapshot

Mode is not inferred from UI.

Values:

BACKTEST
PAPER
SHADOW
CANARY
LIMITED
LIVE

Never change historical mode.

---

# PART 160 — MANUAL TRADE E2E

Acceptance flow:

1. valid account
2. eligible
3. USDC exists
4. user chooses supported asset
5. quote retrieved
6. financial disclosure displayed
7. intent submitted with idempotency
8. risk evaluated
9. capital reserved
10. transaction built
11. inspected
12. signed
13. submitted
14. confirmation observed
15. external truth reconciled
16. ledger posted
17. position updated
18. UI updates
19. audit proof generated

All steps observable.

---

# PART 161 — AGENT TRADE E2E

1. event arrives
2. relevant strategy triggered
3. tool inputs gathered
4. structured prediction committed
5. typed intent emitted
6. eligibility
7. risk
8. capital reservation
9. settlement plan
10. quote
11. transaction inspection
12. signing
13. submission
14. reconciliation
15. ledger
16. performance
17. audit

At no point does model receive wallet key.

---

# PART 162 — FUNDING E2E

1. user requests add funds
2. platform creates provider session
3. provider owns payment/KYC step according to integration
4. provider sends status
5. webhook verified
6. chain receipt observed
7. reconciliation
8. ledger funding state
9. buying power becomes available under policy
10. underlying USDC shown

Redirect success alone does not credit money.

---

# PART 163 — RECONCILIATION E2E

Simulate:

internal expected 100 USDC
external observed 99.99 USDC

System:

- creates mismatch
- blocks unsafe new action according to threshold/policy
- operator can inspect evidence
- resolution records reason
- compensating journal if necessary
- mismatch resolved
- audit proof

---

# PART 164 — ADMIN E2E

Operator:

1. signs in
2. step-up if needed
3. initiates high-risk action
4. reason required
5. second approval where policy requires
6. action applied
7. audit created
8. affected service reacts
9. notification emitted

---

# PART 165 — GLOBAL KILL E2E

Trigger global new-risk kill.

Verify:

- new manual trades rejected
- new agent intents rejected
- existing submitted trade still reconciles
- fills still post
- positions still update
- audit still works
- operator sees state

Then verify re-enable requires configured approval.

---

# PART 166 — EXACT DEFINITION OF PRODUCTION-READY CODE

Production-ready code means:

- no fake balance
- no in-memory money store
- no TODO safety path
- exact arithmetic
- migrations
- idempotency
- durable state
- transaction boundaries
- error handling
- retries designed per effect
- reconciliation
- observability
- alerts
- backup
- restore procedure
- auth
- authorization
- audit
- rate limiting
- security tests
- load tests
- runbooks
- deployment code
- configuration validation
- production gates

---

# PART 167 — WHAT BLOCKED_EXTERNAL MEANS

BLOCKED_EXTERNAL does not mean:

"not built."

It means:

software implementation complete
+
tests complete
+
activation gate complete
+
external item genuinely unavailable.

Example:

Stripe commercial approval.

Code should exist.

Live gate remains off.

---

# PART 168 — NO CRITICAL TODOS

No TODO/FIXME in:

ledger
capital
risk
eligibility
execution
signing
reconciliation
auth
authorization
production gates
audit

If external dependency:

explicit typed capability state.

---

# PART 169 — BUILD STAGES

Follow these in sequence.

Do not skip.

---

# STAGE 0 — FORENSIC REPOSITORY AUDIT

Inspect:

all source
git history if useful
configuration
existing tests
migrations
infra
provider integrations
README
architecture docs
security files
CI
secrets exposure

Create requirement traceability.

Run baseline:

build
tests
lint
security scans

Record failures.

Exit criteria:

you understand the repository and have persistent build state.

Then continue.

---

# STAGE 1 — FOUNDATION CONTRACTS

Implement/finalize:

config
IDs
time
money primitives
asset precision
error model
auth abstraction
authorization foundation
event envelope
outbox/inbox
logging/tracing
database migration infrastructure

Exit criteria:

foundation tests green.

---

# STAGE 2 — FINANCIAL CORE

Build:

ledger
journal
multi-asset accounting
capital envelope
reservation
balance projection
buying power
funding state
positions
valuation

Immediately create property tests.

Exit criteria:

financial invariant test suite green.

---

# STAGE 3 — POLICY / AUTHORITY

Build:

ProductionCapabilityGate
EligibilityEngine
RiskKernel
KillSwitch
AdminApproval

Exit criteria:

tests prove unauthorized paths impossible.

---

# STAGE 4 — INSTRUMENT / INTENT

Build:

asset registry
instrument registry
venue listing
TradeIntent
idempotency
state machines

Exit criteria:

typed API contracts stable.

---

# STAGE 5 — SETTLEMENT COMPILER

Build:

planner
plan DAG
constraints
provider-neutral adapter interfaces
NO_VALID_PLAN behavior

Exit criteria:

comprehensive planner tests.

---

# STAGE 6 — SOLANA EXECUTION

Build:

Jupiter
transaction decode
transaction inspection
simulation
signing boundary
submission state machine
finality observation
Helius
secondary RPC

Exit criteria:

provider contract tests +
malicious transaction rejection tests.

---

# STAGE 7 — RECONCILIATION

Build:

execution reconciliation
wallet balance reconciliation
funding reconciliation
mismatch system
operator resolution

Exit criteria:

crash/unknown-state E2E passes.

---

# STAGE 8 — FUNDING / WALLET

Build:

FundingProvider
WalletProvider
Stripe adapter
embedded wallet adapter
webhooks
settlement evidence
withdrawal boundary
capability gate

Exit criteria:

sandbox/test/fixture E2Es green.

---

# STAGE 9 — STRATEGY COMPILER

Build:

Strategy AST
IR
effect system
natural-language frontend
TS SDK
compiler validation

Exit criteria:

golden compiler corpus green.

---

# STAGE 10 — AGENT RUNTIME

Build:

event subscriptions
ToolBroker
budgets
Prediction Ledger
capital binding
agent lifecycle
pause
promotion

Exit criteria:

shadow agent E2E green.

---

# STAGE 11 — REALITY ENGINE

Build:

raw archive
normalizer
ClickHouse
timestamp semantics
provenance
historical query

Exit criteria:

look-ahead leakage tests pass.

---

# STAGE 12 — BACKTESTING / PERFORMANCE

Build:

historical replay
paper
shadow
counterfactual
P&L
calibration
performance segregation

Exit criteria:

mode integrity tests.

---

# STAGE 13 — AUDIT / PROOF

Build:

canonical serialization
hash chain
KMS signing
Merkle batch
WORM archive
verification command

Exit criteria:

tampering detection test.

---

# STAGE 14 — CUSTOMER WEB APP

Complete all user workflows.

No dead buttons.

No fake state.

Responsive.

Accessible.

Exit criteria:

Playwright critical-path E2Es.

---

# STAGE 15 — ADMIN

Build full operational control plane.

Exit criteria:

admin RBAC + dual-control tests.

---

# STAGE 16 — INFRASTRUCTURE

Terraform:

AWS
networking
RDS
Redis
ECS
S3
KMS
Secrets
WAF
observability
IAM

External managed service configuration docs.

Exit criteria:

Terraform validate/plan where credentials permit.

---

# STAGE 17 — CI/CD

Implement full pipeline.

Release artifact signing.

Security scans.

Deployment controls.

Exit criteria:

pipeline green.

---

# STAGE 18 — ADVERSARIAL VALIDATION

Run:

property
fuzz
race
chaos
security
load
migration
restore
unknown submission

Fix failures.

Repeat until green.

---

# STAGE 19 — PRODUCTION READINESS

Create:

`PRODUCTION_READINESS_REPORT.md`

Do not write it prematurely.

Only after implementation.

---

# PART 170 — GOLDEN COMPILER TEST CORPUS

Create test examples including:

valid simple momentum
valid wallet trigger
valid liquidity filter
valid max loss
invalid transfer request
invalid unlimited capital
invalid arbitrary program call
invalid secret export
invalid unbounded loop
ambiguous natural language
unsupported asset
unsupported venue

Expected:

correct AST or safe rejection.

---

# PART 171 — REGRESSION DISCIPLINE

Every bug fixed should produce:

test reproducing bug
+
fix
+
green regression.

Especially:

financial
security
concurrency
provider state

bugs.

---

# PART 172 — DATABASE FAILURE SEMANTICS

If Postgres commit outcome is uncertain:

do not issue duplicate external effect based merely on client disconnect.

Follow operation-specific recovery.

Use unique IDs and state inspection.

---

# PART 173 — FRONTEND RETRY SAFETY

User double-clicking BUY cannot duplicate trade.

Browser retry cannot duplicate.

Mobile reconnect cannot duplicate.

Idempotency key generated for user command remains stable until semantic command concludes.

---

# PART 174 — MARKET DATA STALENESS

Each strategy data dependency declares maximum age.

Example:

price:
500ms

wallet event:
2s

social event:
policy-specific

If freshness requirement violated:

strategy cannot trade from stale data.

Record reason.

---

# PART 175 — PROVIDER CLOCK / EVENT CLOCK

Do not assume provider timestamp is trusted truth.

Store:

provider timestamp
+
our receipt timestamp.

Use our availability timestamp for knowledge-time logic.

---

# PART 176 — STRATEGY DATA DEPENDENCY VERSIONING

StrategyVersion references:

source definitions
model requirements
feature definitions

If tool behavior changes materially:

new dependency version.

This matters for performance reproducibility.

---

# PART 177 — MODEL FAILURE

If model unavailable:

do not invent output.

If model is required to make decision:

agent skips decision or enters safe non-trading state.

No unconstrained fallback.

---

# PART 178 — REASONING STORAGE

Do not attempt to store hidden model chain-of-thought.

Store:

structured rationale
evidence references
signal values
decision output
model metadata

This is enough for audit/product explanation without relying on hidden reasoning traces.

---

# PART 179 — API CLIENT SAFETY

Generated client should understand:

idempotency
correlation IDs
structured errors

Frontend must not manually duplicate financial retry logic.

---

# PART 180 — RATE LIMITS

Implement:

user API limits
auth limits
quote limits
strategy event limits
model budget limits
provider protection

Rate limiting is not financial authority.

Redis acceptable for rate-limit counters.

---

# PART 181 — ORDER RATE LIMIT

Risk Kernel enforces financial order frequency independently of infrastructure rate limit.

Even if Redis resets:

agent cannot exceed deterministic risk budget stored authoritatively.

---

# PART 182 — MARKET INTEGRITY

Prohibit:

wash trading
self-trading where avoidable
artificial volume
spoofing
manipulative behavior
fake liquidity interactions designed for manipulation

Build beneficial-owner-aware self-trade controls.

---

# PART 183 — PROVIDER RAW RESPONSE STORAGE

Financially consequential provider response:

retain raw payload/evidence according to policy.

Never depend exclusively on parsed mutable fields.

---

# PART 184 — SCHEMA VERSIONING

Version:

events
Strategy IR
RiskPolicy
EligibilityPolicy
ExecutionPlan
Audit evidence

Never silently reinterpret old historical record using today's schema.

---

# PART 185 — COMPATIBILITY

Rolling deployment should tolerate current/previous compatible event versions.

Plan schema migration deliberately.

---

# PART 186 — OPENAPI

Maintain OpenAPI specification.

Generate frontend client.

CI checks spec/client consistency.

---

# PART 187 — PROTOBUF

Internal gRPC contracts:

version carefully.

Use Buf or equivalent tooling if appropriate.

Breaking changes caught in CI.

---

# PART 188 — DATA MODEL MINIMUM

Implement at least:

User
Account
ComplianceProfile
Wallet
FundingSource
Deposit
Withdrawal
LedgerAccount
JournalTransaction
JournalEntry
CapitalEnvelope
Reservation
Asset
EconomicExposure
Instrument
Venue
VenueListing
Quote
TradeIntent
ExecutionPlan
ExecutionPlanStep
ExecutionAttempt
Order
Fill
Position
Strategy
StrategyVersion
Agent
AgentRun
RiskPolicy
RiskDecision
EligibilityDecision
Prediction
MarketEvent
Tool
ToolInvocation
Backtest
PerformanceSnapshot
AuditEvent
ProviderEvent
ReconciliationRecord
ProductionCapabilityGate
AdminApproval

---

# PART 189 — ADMIN DB ACCESS

Operators should not need direct production SQL for normal incident resolution.

Operational control plane should support safe workflows.

Emergency database access documented and audited separately.

---

# PART 190 — LOG REDACTION

Never log:

private key
seed phrase
wallet signing token
full auth token
provider secret
full card data
sensitive identity documents

Implement structured redaction.

---

# PART 191 — CARD DATA

Nodal backend should not directly ingest raw card details when provider-hosted/embedded secure collection is available.

Keep PCI scope minimal.

---

# PART 192 — SESSION SECURITY

Secure cookies if cookie auth used.

SameSite.

HttpOnly.

Secure.

CSRF protection.

Rotation.

Logout invalidation.

Session listing/revocation.

---

# PART 193 — USER NOTIFICATIONS

At minimum support in-app events for:

funding available
funding failed/reversed
trade fill
trade failed
agent paused
risk limit hit
security session event

Notification delivery provider may be abstracted.

---

# PART 194 — ACCOUNT FREEZE SEMANTICS

Frozen account:

no new risk
no withdrawal unless compliance/operator permits
existing settlement/reconciliation continues

Make state machine explicit.

---

# PART 195 — FINANCIAL REPAIR

If external truth shows missing internal transaction:

create reconciliation recovery workflow.

Do NOT manually overwrite position.

Generate canonical financial event and audit.

---

# PART 196 — PROVIDER DISAGREEMENT

If Helius says transaction confirmed but fallback RPC does not:

apply configured confidence policy.

Do not arbitrarily choose optimistic state.

If uncertainty material:

block dependent activity.

---

# PART 197 — BUDGETS

Agent budgets:

capital
data
model
order count

Enforce independently.

Model provider cannot charge unlimited because agent loops.

---

# PART 198 — LOOP SAFETY

Strategy runtime:

bounded execution.

No recursively self-spawning action.

No infinite event loop.

Dedup event triggers.

Rate limit by strategy.

---

# PART 199 — EVENT DEDUP

Every source event should have canonical dedup strategy.

Use provider event ID where reliable.

Otherwise source-specific composite hash.

Raw event remains archived.

---

# PART 200 — DATA GAP

Market ingestion tracks checkpoints.

On disconnect:

resume/replay where provider supports.

Mark gap.

Strategies requiring missing period cannot pretend continuity.

---

# PART 201 — AUDIT HASH VERIFICATION

Create command:

`make verify-audit`

or equivalent.

It should verify:

hash chain
signatures
Merkle membership where applicable
object hashes

Tampered record test must fail.

---

# PART 202 — CUSTOMER DATA EXPORT

Architecture should permit customer transaction/history export.

Do not implement regulatory filing claims unless appropriate.

Provide CSV/JSON history export if feasible.

---

# PART 203 — PRODUCT ANALYTICS

Product analytics must not become financial source of truth.

Do not send sensitive financial data to generic analytics vendors without explicit approval.

---

# PART 204 — INFRASTRUCTURE COST

Avoid absurdly expensive architecture in dev.

Allow lighter local environment while maintaining same interfaces.

Production managed services configurable.

---

# PART 205 — RTO/RPO

Document measured/tested objectives.

Do not invent claim without restore drill.

Financial committed transaction RPO design objective:

0 in primary database failure model supported by architecture.

Backups/PITR cover broader disaster recovery.

---

# PART 206 — CANARY PRODUCTION

If live approval eventually exists:

canary trading account should have:

strict maximum exposure
strict asset allowlist
strict wallet
strict environment gate
manual visibility
rapid kill

Never use ordinary customer capital as first integration test.

---

# PART 207 — PROVIDER CANARY

Each external provider integration should have health-check/canary procedures that do not accidentally move meaningful capital.

---

# PART 208 — LIVE CLAIMS

Never mark:

LIVE VERIFIED

unless real live provider evidence exists.

Allowed states:

CODE_COMPLETE
CONTRACT_TESTED
SANDBOX_VERIFIED
CANARY_VERIFIED
LIVE_VERIFIED
BLOCKED_EXTERNAL

Use these in readiness report.

---

# PART 209 — DOCUMENTATION

Required:

README.md

docs/architecture/SYSTEM.md
docs/architecture/FINANCIAL_MODEL.md
docs/architecture/SETTLEMENT_COMPILER.md
docs/architecture/EXECUTION.md
docs/architecture/RECONCILIATION.md
docs/architecture/AGENT_RUNTIME.md
docs/architecture/POINT_IN_TIME.md

docs/security/SECURITY.md
docs/threat-model/THREAT_MODEL.md

docs/operations/DEPLOYMENT.md
docs/operations/BACKUP_RESTORE.md
docs/operations/DISASTER_RECOVERY.md
docs/operations/RECONCILIATION.md

docs/compliance-gates/PRODUCTION_GATES.md

docs/runbooks/

docs/adr/

docs/build/

Do not write aspirational documentation presented as implemented fact.

---

# PART 210 — ADR REQUIREMENTS

Write ADRs for:

Postgres ledger
Go core
modular monolith
Temporal
Redpanda
ClickHouse
S3 WORM
Jupiter
Helius
typed Strategy IR
no arbitrary live code
no AI signing
Risk Kernel
Settlement Compiler
universal buying power
capability gating
single-primary-region V1
no proprietary stablecoin
no internal crossing

---

# PART 211 — README

README should let engineer understand:

what product is
what V1 is
what V1 is not
architecture
local development
required tools
common commands
provider config
test strategy
production safety warnings

---

# PART 212 — MAKEFILE / TASKS

No 40-step manual setup.

Provide convenient commands.

CI should call same commands developers use where practical.

---

# PART 213 — SECURITY SCANS

Use appropriate tools, for example:

govulncheck
gosec
Trivy
Gitleaks

and frontend equivalents.

Do not fail to build simply because one suggested tool unavailable; use current maintained alternatives.

---

# PART 214 — NO STALE DEPENDENCIES BY ACCIDENT

Pin versions.

Use lockfiles.

Do not blindly update every dependency without tests.

Run compatibility tests.

---

# PART 215 — GO QUALITY

Use:

context propagation
timeouts
structured errors
explicit transaction boundaries
dependency interfaces

Avoid:

global mutable state
panic on external input
unbounded goroutines
fire-and-forget financial jobs

---

# PART 216 — GO RACE

Run `go test -race` against concurrency-sensitive packages.

Especially:

capital
ledger
execution state
reconciliation
event consumers

---

# PART 217 — FRONTEND TESTS

Use:

component tests where valuable
Playwright for critical workflows

Do not create brittle pixel tests as primary coverage.

---

# PART 218 — MIGRATION TESTS

From clean DB:

all migrations apply.

From representative previous schema:

upgrade works.

Critical down migration:

must not delete accounting history.

---

# PART 219 — RESTORE TEST

Automate locally/staging simulation:

backup
restore
boot
reconciliation dry-run

Document production procedure.

---

# PART 220 — DRY RUN MODE

Provide non-money:

execution plan preview.

Useful for:

debugging
risk analysis
admin
simulation

Dry run cannot accidentally call signing provider.

---

# PART 221 — OBSERVATION VS ACTION CREDENTIALS

Use separate provider credentials if provider supports it.

Read-only observation path should remain available if action credential disabled.

---

# PART 222 — GLOBAL CONFIG HASH

Production instance should expose non-secret configuration/version hash.

Audit financial decisions with relevant policy/config versions.

---

# PART 223 — DEPLOYMENT VERSION

Every financial AuditEvent should identify:

service/build version

when practical.

Makes incident reconstruction possible.

---

# PART 224 — NO HIDDEN NONDETERMINISM

Risk and financial policy cannot depend on:

current map iteration ordering
floating-point behavior
randomness

If randomness used in simulation:

seed stored.

---

# PART 225 — SIMULATION VS PRODUCTION CODE REUSE

Backtester should share:

Strategy IR evaluator
Risk policy semantics
instrument definitions

with live system.

Avoid two separate strategy implementations drifting apart.

Execution simulation adapter differs.

---

# PART 226 — NO IMPOSSIBLE BACKTEST

Historical strategy cannot consume data whose `decision_available_at` is after simulated time.

Write an explicit automated leakage test.

---

# PART 227 — USER CANCEL

Cancel request does not mean cancelled.

State:

CANCEL_REQUESTED

Only external confirmation moves:

CANCELLED.

Possible race:

fill occurs while cancel requested.

Handle correctly.

---

# PART 228 — PARTIAL EXECUTION

Even if Jupiter V1 mostly produces atomic swaps, generic model should correctly represent partial fills for later venues.

Do not incorrectly assume every future execution is atomic.

---

# PART 229 — PLATFORM EXTENSIBILITY

Future adapters should be addable without rewriting:

ledger
risk
intent
Settlement Compiler

That is one of the main tests of architecture quality.

---

# PART 230 — FUTURE CROSS-CHAIN

Prepare typed plan steps such as:

CONVERT
TRANSFER
WAIT_FINALITY
TRADE

But cross-chain capability remains disabled.

Future first candidate:

CCTP.

Do not implement fake instant bridging.

---

# PART 231 — FUTURE PREDICTIONS MARKET

Universal Instrument Model must be able to represent:

event terms

without pretending event contracts are tokens.

Do not enable trading.

Future adapter requires regulated integration.

---

# PART 232 — FUTURE ARBITRARY CODE

Do not build now.

When eventually implemented:

Firecracker/microVM
no secrets
no wallet access
no arbitrary inbound
restricted egress
resource quota
capability proxy

Keep current interfaces compatible with future sandbox.

---

# PART 233 — FUTURE CONFIDENTIAL AGENTS

Do not implement Nitro Enclaves in V1.

Proof architecture should not prevent them later.

---

# PART 234 — IMPLEMENTATION COMPLETION LOOP

After each subsystem:

1. implement
2. format
3. compile
4. lint
5. run focused tests
6. run integration tests relevant to change
7. update REQUIREMENTS_TRACEABILITY
8. update MASTER_BUILD_STATE
9. fix failures
10. proceed

Never leave major subsystem unverified while moving on without documenting why.

---

# PART 235 — WHEN TESTS FAIL

Do not disable test.

Do not weaken assertion just to get green.

Understand root cause.

If test itself wrong:

document why
+
fix test.

---

# PART 236 — WHEN ARCHITECTURE CONFLICTS WITH EXISTING REPO

Do not immediately rewrite.

Evaluate:

correctness
security
migration cost
test coverage

If existing solution satisfies requirement:

keep it.

Record decision.

---

# PART 237 — DEFINITION OF DONE PER REQUIREMENT

A requirement is VERIFIED only when:

code exists
+
tests exist
+
tests passed
+
operational path exists where relevant
+
documentation matches

No "implemented in theory."

---

# PART 238 — REQUIRED FINAL TEST MATRIX

Before final readiness report, provide actual results for:

build
unit
property
race
fuzz
contract
integration
E2E
chaos
load
security
migration
audit verification
restore drill

If one was not executable:

say exactly why.

Do not fabricate.

---

# PART 239 — REQUIRED EVIDENCE MATRIX

For each critical subsystem record:

Implementation
Test
Runtime evidence
Known limitation

Examples:

Ledger
Reservation
Risk
Signing
Jupiter
Reconciliation
Funding
Agent
Reality Engine
Audit
Capability gates

---

# PART 240 — LAUNCH BLOCKER MATRIX

Separate:

CODE BLOCKER

from:

PROVIDER BLOCKER

from:

LEGAL BLOCKER

from:

BUSINESS BLOCKER

Do not confuse them.

---

# PART 241 — EXTERNAL P0 GATES

Must remain explicit:

- U.S./California licensing/exemption
- delegated signing custody analysis
- Stripe commercial approval
- Stripe fraud/dispute responsibility
- wallet provider signing semantics
- permitted asset universe
- strategy/adviser/CTA implications
- provider data-retention rights
- final brand clearance

Do not invent answers.

---

# PART 242 — FINAL READINESS REPORT

Create:

`PRODUCTION_READINESS_REPORT.md`

Only after implementation.

Sections:

1. Executive conclusion
2. Architecture implemented
3. Repository structure
4. Financial model
5. Security model
6. Agent authority boundary
7. Financial invariants
8. Provider integration status
9. Production capability status
10. Test evidence
11. Chaos evidence
12. Load evidence
13. Backup/restore evidence
14. Observability
15. Operational readiness
16. External blockers
17. Remaining risks
18. Exact launch checklist

---

# PART 243 — FINAL STATUS VALUES

For platform:

NOT_READY
CODE_READY
STAGING_READY
CANARY_READY
LIMITED_READY
LIVE_READY

Do not choose higher state without evidence.

For capital authority:

DISABLED
CANARY_AUTHORIZED
LIMITED_AUTHORIZED
LIVE_AUTHORIZED

Default:

DISABLED.

---

# PART 244 — PRODUCTION GATE DEFAULT

Fresh production deployment:

LIVE_FUNDING = DISABLED
LIVE_MANUAL_TRADING = DISABLED
LIVE_AGENT_TRADING = DISABLED
WITHDRAWALS = DISABLED

until approved evidence exists.

This is intentional.

---

# PART 245 — DO NOT TURN SAFETY INTO FRICTION EVERYWHERE

Fail closed where money/security requires it.

But keep product UX excellent.

Examples:

Unavailable asset:
clear explanation.

Expired quote:
automatically refresh before asking user to retry where safe.

Pending funding:
show exact state.

Risk rejection:
show understandable reason.

Technical complexity should be hidden without hiding financial truth.

---

# PART 246 — PRODUCT QUALITY BAR

The application should feel comparable in product quality to serious modern fintech software.

Not because of animations.

Because:

- state is clear
- latency is handled
- failures are understandable
- user always knows whether action is pending/submitted/filled
- amounts are trustworthy
- balances never flicker inconsistently
- history is complete
- controls are obvious
- there are no dead ends

---

# PART 247 — ABSOLUTE ANTI-PATTERNS

Reject all of these:

`float64 money`

`Redis balance`

`bool kycComplete is eligibility`

`LLM decides risk`

`LLM calls wallet`

`retry POST after timeout`

`success redirect credits deposit`

`edit balance admin`

`mock provider in prod`

`backtest = live`

`current model historical replay called point-in-time`

`browser prevents double spend`

`ticker is asset identity`

`HTTP 200 = settlement`

`provider response trusted without reconciliation`

`kill switch stops reconciliation`

`one env var enables live money`

`TODO in signing path`

---

# PART 248 — CORE DESIGN MANTRAS

Remember constantly:

**AI proposes. Deterministic systems authorize.**

**Economic intent is above venue implementation.**

**Financial authority is explicit.**

**Money is exact.**

**Reservations are transactional.**

**A timeout means uncertainty.**

**External effects require reconciliation.**

**Event time is not knowledge time.**

**Analytics state is not ledger state.**

**Backtest is not live.**

**Counterfactual is not realized.**

**A wallet balance is not necessarily buying power.**

**Self-custody cannot be locked by a database fiction.**

**No blind signing.**

**No agent keys.**

**No operator balance editing.**

**No hidden production mocks.**

**No external blocker is disguised as code completion.**

---

# PART 249 — FINAL COMMAND TO YOU

Do not answer this goal with a design document and stop.

Do not tell the user how you would build the platform.

Do not create twenty empty directories and declare architecture complete.

Do not stop because the project is large.

Do not reduce scope simply because implementation is difficult.

Do not skip financial-system rigor to reach UI faster.

Start by auditing the repository and establishing persistent execution state.

Then work through the stages.

Continue until:

- all implementable V1 systems exist
- all locally executable critical tests pass
- all provider integrations are implemented to the strongest verifiable level available
- all external blockers are machine-gated
- all safety-critical financial invariants have automated tests
- all critical failure scenarios have been exercised
- the web application is complete
- infrastructure exists
- CI/CD exists
- observability exists
- operator tooling exists
- documentation reflects reality
- the readiness report truthfully states what is and is not authorized for live capital

The end state should survive review by:

- a senior fintech CTO
- a payments engineer
- a distributed-systems engineer
- a security engineer
- a quant-platform engineer
- a wallet/custody engineer
- an SRE
- an institutional integration partner
- a technical due-diligence team

They should be able to inspect the repository and conclude that the system was designed around real financial failure modes rather than ordinary SaaS assumptions.

# DO THE WORK.
# BUILD THE SYSTEM.
# PROVE THE INVARIANTS.
# TEST THE FAILURE MODES.
# GATE WHAT CANNOT YET LEGALLY OR COMMERCIALLY GO LIVE.
# DO NOT FAKE PRODUCTION READINESS.