/goal

# NODAL — ULTIMATE MASTER GOAL
# INDEPENDENT ADVERSARIAL AUDIT + FINAL ARCHITECTURE MIGRATION + PRODUCTION PROOF

You are taking ownership of an existing, sophisticated financial software repository.

You are NOT starting a greenfield project.

A previous engineering effort claims that large portions of this system are already implemented, tested, security-hardened, and production-oriented. You must NOT trust those claims merely because documentation, TODO files, tests, dashboards, or previous agents say they are true.

Your job is simultaneously to:

1. independently prove or disprove every meaningful production-readiness claim in the existing repository;
2. preserve every valuable and correct piece of existing engineering;
3. identify architectural assumptions that no longer match the final product;
4. migrate the system into the finalized Nodal architecture defined below;
5. implement all missing code, migrations, tests, security controls, operations, interfaces, and UX required by that architecture;
6. attack the resulting system adversarially;
7. fix everything you can fix in code;
8. produce executable evidence for every readiness claim;
9. clearly isolate the small number of things that genuinely require external legal, provider, commercial, credential, or production-environment evidence;
10. leave the repository in the strongest possible state for controlled real-world activation.

Do not merely write a plan.

Do not merely scaffold interfaces.

Do not merely add TODO comments.

Do not merely create tests that assert mocks behave as expected.

Do not declare success because the repository compiles.

Do not declare success because existing tests pass.

Do not trust MASTER_BUILD_STATE.md or any previous readiness document without verifying the underlying claim yourself.

Actually inspect, run, attack, fix, migrate, test, and prove the system.

========================================================================
PART I — THE FINAL PRODUCT
========================================================================

The internal codename is Nodal.

IMPORTANT:
"Nodal" is an internal codename. A financial company called Nodal Exchange already exists. Do not embed "Nodal" as an irreversible public/legal brand assumption. All public product/company branding must be configurable.

The final company is NOT simply:

- a crypto exchange;
- a wallet;
- a memecoin launcher;
- an AI trading bot;
- a strategy marketplace;
- an RIA;
- a brokerage;
- a blockchain;
- a game economy;
- a payment app.

It is:

# AN INTELLIGENCE AND MARKET OPERATING SYSTEM ABOVE MULTIPLE KINDS OF CAPITAL.

The user should be able to enter the product extremely quickly, build financial agents, research markets, create strategies, participate in a Nodal-native economy, simulate and prove strategies, and optionally connect those same strategies to real financial capital.

The system separates THREE major capital/value domains.

----------------------------------------------------------------------
DOMAIN A — NODAL-NATIVE ECONOMY
----------------------------------------------------------------------

This is a friction-minimized internal economy.

A user can enter Nodal with a lightweight account, subject to whatever basic age/jurisdiction/platform rules are required.

The user can potentially:

- purchase Nodal Credits;
- receive promotional Credits;
- earn Credits through approved activity;
- spend Credits;
- create Nodal-native assets;
- launch Nodal-native markets;
- buy/sell Nodal-native assets against Credits;
- pay for AI compute;
- purchase data;
- purchase agent services;
- purchase research;
- enter competitions;
- run expensive simulations;
- publish agents;
- purchase approved digital/platform services;
- receive creator revenue;
- participate in an internal agent economy.

Nodal-native assets are NOT automatically blockchain tokens.

They live in Nodal's internal economic system.

A Pump.fun-like product experience is a product inspiration:

User:
"Create DOGGU"

Nodal:
- creates a Nodal-native asset;
- creates a deterministic internal market;
- allows other eligible Nodal users to acquire/sell the asset using Nodal Credits;
- records all trades, positions, pricing, liquidity and provenance internally.

But do NOT blindly copy Pump.fun's legal, technical, or economic structure.

Nodal Credits and Nodal-native assets must be architected so their real-world redemption status is an explicit policy capability, not an implicit property.

DEFAULT SAFE CODE STATE:

NODAL CREDITS:
CLOSED_LOOP_NONREDEEMABLE

NODAL NATIVE ASSETS:
INTERNAL_ONLY

REAL-WORLD PAYOUT:
DISABLED

The architecture must ALSO support a future:

PARTNER_ISSUED_OR_PARTNER_REDEEMED_VALUE

mode where an appropriately licensed virtual-economy/payment/payout provider may permit eligible value to be converted to:

- USD;
- another fiat payout;
- and/or crypto,

but ONLY when:

- the provider contract explicitly supports the exact economic model;
- KYC/AML/provider verification requirements are satisfied;
- jurisdiction is approved;
- value provenance is payout-eligible;
- legal/compliance approval exists;
- ProductionCapabilityGate is active.

DO NOT implement a hidden or disguised path:

Credits -> SOL

unless it occurs through an explicitly configured approved provider capability.

Do not invent a three-hop workaround.

Do not disguise redemption as:

- gifts;
- NFTs;
- secondary transfers;
- fake rewards;
- token swaps;
- game prizes;
- rebates;

if the actual economic purpose is cash conversion.

Economic substance matters.

----------------------------------------------------------------------
DOMAIN B — SIMULATED CAPITAL
----------------------------------------------------------------------

A user can build and operate strategies with:

- BACKTEST;
- PAPER;
- SHADOW;
- synthetic/adversarial universes;
- test markets;
- historical replay.

This should require no financial-account activation.

The entire agent platform must be useful before the user connects real capital.

The same Strategy IR and agent architecture must work across simulated and real rails.

----------------------------------------------------------------------
DOMAIN C — REAL CAPITAL
----------------------------------------------------------------------

Users who want actual external financial exposure can connect separate real-capital rails.

Initial real-capital rails include abstractions for:

1. SELF-CUSTODIAL ONCHAIN
   - customer-owned wallet;
   - e.g. Solana-compatible wallet architecture;
   - real blockchain assets;
   - external DEX execution where legally/provider approved.

2. HOSTED / LICENSED FINANCIAL PARTNER
   - customer account carried by licensed financial partner;
   - actual USD and/or supported financial assets;
   - partner custody;
   - partner KYC/AML;
   - partner execution;
   - partner authoritative balance;
   - Nodal remains intelligence/strategy/risk/orchestration layer.

Future rails may include:

- securities broker infrastructure;
- prediction-market DCM/intermediary;
- other regulated asset classes.

Do not implement live unsupported products merely because an interface exists.

========================================================================
PART II — PRODUCT EXPERIENCE
========================================================================

The product should conceptually support this journey:

USER
  |
  v
lightweight Nodal account
  |
  +----------------------------+
  |                            |
  v                            v
Agents / Lab             Nodal Economy
Research                 Credits
Backtests                Native Assets
Paper                    Native Markets
Shadow                   Data / Compute
  |                            |
  +-------------+--------------+
                |
                v
          Financial Intent
                |
                v
         Settlement Compiler
                |
       +--------+--------+
       |        |        |
       v        v        v
    PAPER     NATIVE    REAL
               |         |
               |     +---+---+
               |     |       |
               v     v       v
          Nodal Market Hosted Onchain
                      Rail    Rail

Nodal should make the complexity underneath feel like one coherent operating system.

However:

DO NOT mislead users into believing internal Credits, virtual simulation capital, hosted USD, stablecoins, and real crypto are the same thing.

The UI and domain model must make them CLEARLY distinct.

Example:

NODAL ECONOMY
Credits              18,450

SIMULATION
Shadow Portfolio     $52,381

REAL CAPITAL
Hosted Cash           $1,000
SOL                      $217

Do not combine those into:

"Total Cash $71,048"

unless a legally and economically correct aggregation is explicitly defined.

========================================================================
PART III — THE CENTRAL DESIGN PRINCIPLE
========================================================================

NODAL OWNS:

- intelligence;
- agents;
- strategy compilation;
- agent runtime;
- research;
- Reality Engine;
- Prediction Ledger;
- risk controls;
- eligibility overlays;
- Nodal-native markets;
- platform UX;
- financial intent;
- evidence/proof;
- analytics;
- orchestration;
- provider abstraction;
- simulation;
- capital envelopes;
- performance measurement.

APPROPRIATE EXTERNAL PROVIDERS OWN OR PERFORM, WHEN APPLICABLE:

- regulated custody;
- financial KYC/CIP;
- money transmission;
- real-world redemption;
- real-world payout;
- licensed exchange activity;
- hosted crypto execution;
- banking/payments;
- securities brokerage;
- prediction-market execution;
- tax reporting where contractually assigned.

This distinction is a product architecture rule.

Do NOT assume a licensed partner automatically licenses Nodal's independent activity.

Every capability must have its own legal/provider evidence.

========================================================================
PART IV — EXECUTION CONTRACT FOR YOU
========================================================================

You are not an advisory consultant.

You are the senior engineering team.

Operate autonomously.

If code is wrong:
FIX IT.

If a migration is missing:
WRITE IT.

If an invariant isn't tested:
TEST IT.

If a state machine is informal:
MAKE IT EXPLICIT.

If recovery is ambiguous:
IMPLEMENT IT.

If existing architecture conflicts with the final architecture:
MIGRATE IT.

If an existing system is good:
KEEP IT.

If a provider API cannot be verified:
DO NOT INVENT IT.

If an external commercial/legal approval is missing:
IMPLEMENT THE INTERNAL CAPABILITY, CONTRACT, TEST DOUBLE, GATE, ADMIN WORKFLOW AND EVIDENCE REQUIREMENT, THEN MARK ONLY THE EXTERNAL ACTIVATION BLOCKED.

Use:

BLOCKED_EXTERNAL

ONLY for facts that genuinely cannot be completed from the repository.

Examples:

- provider contract signature;
- legal opinion;
- provider production credentials;
- actual SOC report from third party;
- real production account approval;
- unknown non-public provider pricing.

Do NOT use BLOCKED_EXTERNAL for:

- missing code;
- missing tests;
- missing Terraform;
- missing migrations;
- missing runbooks;
- missing API contracts;
- missing state machines;
- missing mocks/test harnesses;
- missing chaos tests;
- missing admin tools.

Those are YOUR JOB.

========================================================================
PART V — DO NOT DESTROY THE EXISTING SYSTEM
========================================================================

Before changing architecture:

1. inspect the entire repository;
2. determine packages/services/apps;
3. determine build system;
4. determine current test suite;
5. determine DB schema/migrations;
6. determine deployed architecture;
7. determine existing financial state machines;
8. determine current provider adapters;
9. determine existing audit/security systems;
10. determine current frontend capabilities;
11. determine current documentation claims.

Create:

docs/build/FINAL_ARCHITECTURE_MIGRATION.md
docs/build/CURRENT_SYSTEM_INVENTORY.md
docs/build/KEEP_MODIFY_REPLACE_MATRIX.md

Classify every major subsystem:

KEEP
KEEP_AND_HARDEN
MODIFY
REPLACE
DEPRECATE
REMOVE
UNKNOWN

Do not perform destructive rewrites before this inventory exists.

Preserve git history.

Do not discard correct systems simply because building something new feels easier.

========================================================================
PART VI — PERSISTENT EXECUTION MEMORY
========================================================================

This project will span contexts.

Maintain these files continuously:

docs/build/MASTER_BUILD_STATE.md
docs/build/REQUIREMENTS_TRACEABILITY.md
docs/build/BLOCKERS.md
docs/build/DECISION_REGISTER.md
docs/build/CURRENT_SYSTEM_INVENTORY.md
docs/build/KEEP_MODIFY_REPLACE_MATRIX.md

Add:

docs/audit/INDEPENDENT_AUDIT.md
docs/audit/AUDIT_FINDINGS.md
docs/audit/PRODUCTION_EVIDENCE_INDEX.md
docs/audit/FAILURE_INJECTION_RESULTS.md
docs/audit/SECURITY_FINDINGS.md
docs/audit/FINANCIAL_INVARIANT_RESULTS.md
docs/audit/LAUNCH_GATE_MATRIX.md

Each requirement gets an ID.

Example:

NODAL-LEDGER-001
NODAL-CREDIT-017
NODAL-MARKET-023
NODAL-AGENT-044
NODAL-PAYOUT-007
NODAL-SEC-031

REQUIREMENTS_TRACEABILITY.md must map:

Requirement
-> implementation files
-> tests
-> evidence
-> status
-> blockers

Never mark COMPLETE merely because code exists.

========================================================================
PART VII — PHASE ZERO: INDEPENDENT ADVERSARIAL AUDIT
========================================================================

Before major migration, audit the system as though another team claimed:

"This system is safe to handle real money."

Your job is to disprove them.

Do not trust existing tests.

Run them.

Inspect what they actually prove.

Find things they do NOT prove.

Audit:

- build reproducibility;
- dependency integrity;
- lint;
- static analysis;
- race detector;
- secret scan;
- vulnerability scan;
- schema compatibility;
- migrations;
- idempotency;
- concurrency;
- ledger conservation;
- provider retry behavior;
- reconciliation;
- signing;
- transaction inspection;
- state transitions;
- rollback;
- failover;
- configuration;
- environment separation;
- auth;
- authorization;
- tenant isolation;
- admin actions;
- audit trail;
- data consistency;
- event ordering;
- webhooks;
- retry storms;
- stale caches;
- clock skew;
- malformed data;
- integer overflow;
- rounding;
- financial precision;
- compromised providers;
- malicious users;
- malicious agents;
- malicious market creators.

Attack the application.

Do not merely read.

========================================================================
PART VIII — EXISTING CORE SYSTEMS THAT SHOULD GENERALLY SURVIVE
========================================================================

Unless repo evidence disproves their value, preserve and integrate:

- PostgreSQL authoritative transactional accounting;
- double-entry ledger design;
- reservations;
- Financial Intent;
- Settlement Compiler;
- deterministic Risk Kernel;
- agent/strategy IR;
- backtest/paper/shadow/canary/live distinction;
- Reality Engine;
- Prediction Ledger;
- audit evidence;
- hash chaining / tamper evidence;
- reconciliation;
- idempotency;
- provider abstraction;
- Temporal workflows if already functioning correctly;
- event transport if correctly implemented;
- ClickHouse analytics if correctly implemented;
- object storage for evidence;
- Redis as NON-AUTHORITATIVE cache only;
- OpenTelemetry;
- AWS infrastructure where appropriate;
- strict production capability gating.

Do not let Redis become financial authority.

Do not use floating point for financial quantities.

========================================================================
PART IX — VALUE DOMAIN MODEL
========================================================================

This is CRITICAL.

The system must never treat all "balances" as equivalent.

Create explicit value-domain types.

At minimum:

ValueDomain =
- INTERNAL_CREDIT
- INTERNAL_NATIVE_ASSET
- SIMULATED
- HOSTED_FIAT
- HOSTED_CRYPTO
- SELF_CUSTODIAL_CRYPTO
- PAYOUT_PENDING
- EXTERNAL_SETTLED

Create explicit ValueClass / provenance classes.

At minimum:

CreditOrigin =
- PURCHASED
- PROMOTIONAL
- REFUND
- CREATOR_EARNING
- DATA_SALE_EARNING
- AGENT_SERVICE_EARNING
- MARKET_CREATOR_EARNING
- MARKET_TRADING_PROCEEDS
- COMPETITION_REWARD
- ADMIN_ADJUSTMENT
- PROVIDER_SETTLEMENT

Do not assume two Credits with different provenance have identical payout eligibility.

Create immutable value-provenance lots or equivalent accounting lineage.

Every unit entering a payout request must be traceable back to its origin.

Payout eligibility must be policy-derived.

Example:

PROMOTIONAL
-> NOT_WITHDRAWABLE

PURCHASED
-> NOT_WITHDRAWABLE by default

CREATOR_EARNING
-> MAY_BE_WITHDRAWABLE if provider/legal policy allows

MARKET_TRADING_PROCEEDS
-> DISABLED unless explicitly approved

Provider/counsel may later approve a different mapping.

Do not hardcode policy as business truth.

========================================================================
PART X — DOUBLE-ENTRY ACCOUNTING
========================================================================

All authoritative economic movement must be double-entry.

Separate ledgers where appropriate:

1. Internal Credit Ledger
2. Native Asset Ledger
3. Provider Mirror Ledger
4. Reservation Ledger / reservation subsystem
5. Payout Settlement Ledger

Use exact integers / decimals with explicit scale.

NO float64 for money.

For each asset define:

asset_id
symbol
scale
minimum_unit
rounding_policy

Core invariant:

SUM(debits) == SUM(credits)

for every journal transaction.

Additional invariants:

- no double-spend;
- no negative available balance unless explicitly permitted;
- reservations cannot exceed spendable balance;
- unsettled/reversible funding cannot magically become settled;
- payout cannot consume ineligible value;
- credits cannot cross into real-capital domains without an explicit approved conversion mechanism;
- external provider balances do not become authoritative because Nodal cached them;
- crash between external success and internal persistence must reconcile;
- same idempotency key + same request returns original result;
- same idempotency key + different semantic request MUST reject.

========================================================================
PART XI — CREDIT FUNDING
========================================================================

Create:

CreditPurchaseProvider

NOT a hard-coded Stripe dependency.

Provider interface should support:

- checkout/payment creation;
- funding intent;
- authorization;
- capture;
- settlement;
- reversal;
- refund;
- dispute/chargeback;
- provider webhook verification;
- reconciliation.

CreditFundingState:

CREATED
AUTHORIZATION_PENDING
AUTHORIZED
CAPTURE_PENDING
CAPTURED
REVERSIBLE
SETTLED
REVERSED
REFUNDED
DISPUTED
FAILED

Do not assume captured card funds are final.

Separate:

credited_amount
spendable_amount
settled_amount
payout_eligible_amount

This matters because:

User buys 10,000 credits.
Trades them.
Then card chargeback occurs.

The system must not create uncollateralized economic holes.

Implement a policy for:

- settlement hold;
- reversible funding;
- dispute reserve;
- negative user economic balance;
- collection/freeze;
- market impact when funding reverses.

Design this robustly.

Provider may own some fraud risk contractually.

Do not assume that unless verified.

========================================================================
PART XII — NODAL CREDITS
========================================================================

Credits are first-class internal assets.

Required properties:

- immutable asset definition;
- explicit issuance reason;
- exact ledger history;
- no arbitrary balance mutation;
- idempotent issuance;
- refund handling;
- expiry only if explicitly product/legal approved;
- promotional-vs-purchased distinction;
- administrative adjustments require privileged workflow;
- every admin adjustment is fully audited;
- no direct database edits.

Default:

credits.transfer_to_external_wallet = FALSE
credits.redeemable = FALSE
credits.cash_value_claim = FALSE

Future capability:

credits.partner_redemption = ENABLED

only if all required provider/legal evidence is active.

Do not expose language like:

"$1 = 100 Credits forever redeemable"

unless the actual approved partner/legal model supports that statement.

========================================================================
PART XIII — NODAL-NATIVE ASSET CREATION
========================================================================

Implement a first-class:

NativeAsset

Fields should include at least:

id
creator_user_id
name
symbol
description
metadata
status
created_at
asset_version
supply_model
market_model
content_moderation_state
jurisdiction_policy
risk_policy
creator_allocation
treasury_allocation if any
maximum_supply if any
mint_rules
transfer_rules
trading_status
evidence references

Do not permit invisible supply changes.

Asset state machine:

DRAFT
PENDING_REVIEW
ACTIVE
CLOSE_ONLY
HALTED
DELISTED
REJECTED

User-generated asset creation must pass:

- name moderation;
- impersonation detection;
- illegal content screening;
- restricted entity screening where applicable;
- duplicate/near-duplicate checks;
- malicious metadata checks;
- URL safety;
- symbol collision handling.

Do not allow a creator to silently change economics after buyers enter.

Economic configuration becomes immutable at activation unless governed by an explicitly versioned transparent mechanism.

========================================================================
PART XIV — NODAL-NATIVE MARKET ENGINE
========================================================================

Build a deterministic, auditable internal market engine.

Do not merely fake prices.

The engine must support a clearly defined market model.

For initial implementation, choose the most appropriate model after inspecting the existing code and documenting the decision.

Potential initial models:

- deterministic bonding curve;
- constant-product AMM;
- order book.

Do not build three incomplete systems.

Choose ONE production-quality initial market model and abstract it cleanly for later expansion.

Decision criteria:

- deterministic pricing;
- simple accounting;
- no hidden counterparties;
- resistant to rounding exploits;
- easy simulation;
- understandable user experience;
- auditable;
- reproducible;
- bounded computation;
- strong concurrency properties.

If choosing a bonding curve:

document exact formula.

If choosing constant-product:

document invariant.

If choosing an order book:

document matching priority.

For every quote include:

market_id
asset_id
side
input_amount
expected_output
price
effective_price
fees
slippage
state_version
quote_expiry
quote_id

Quotes are NOT execution.

Execution revalidates market state.

Do not accept stale quotes.

========================================================================
PART XV — MARKET ACCOUNTING
========================================================================

Every internal market trade must produce atomic accounting.

Example:

BUY DOGGU using Credits

Must atomically:

- debit user's available Credits;
- credit the appropriate market/reserve account;
- debit market/native-asset inventory or mint according to curve;
- credit user's DOGGU balance;
- account for fees;
- account for creator fees if allowed;
- update deterministic market state;
- create fill;
- create immutable audit record.

If any part fails:
NONE of the economic effect occurs.

Do not emulate atomicity with eventual event handlers.

Use a transactional financial core.

Events publish AFTER durable commit.

Outbox pattern where appropriate.

========================================================================
PART XVI — MARKET INTEGRITY
========================================================================

Implement market-integrity controls even though the economy is internal.

Required:

- self-trade prevention;
- same-account wash-trade prevention;
- obvious same-beneficial-control heuristics where available;
- rate limiting;
- quote spam controls;
- front-running protections where applicable;
- deterministic ordering;
- manipulation detection;
- anomalous volume detection;
- pump coordination detection signals;
- creator self-dealing visibility;
- concentration metrics;
- insider/creator allocation visibility;
- trade surveillance events;
- halt mechanism;
- close-only mode;
- market freeze;
- dispute tooling.

Do not claim these make a speculative market legally approved.

They are product/security controls.

========================================================================
PART XVII — CREATOR ECONOMY
========================================================================

Nodal supports more than native assets.

Build an internal commerce abstraction for:

- data;
- agent services;
- compute;
- strategy templates;
- APIs;
- research;
- competitions;
- creator products.

Create:

InternalCommerceOrder
InternalProduct
InternalSeller
InternalPayoutAttribution

Revenue earned by creator activity MUST have provenance distinct from speculative trading proceeds.

This is important for future payout rules.

========================================================================
PART XVIII — PAYOUT ARCHITECTURE
========================================================================

This is one of the most important migrations.

Create:

PayoutProvider
PayoutEligibilityEngine
PayoutRequest
PayoutDestination
PayoutComplianceState

Do not make Nodal itself directly send arbitrary fiat/crypto as the default.

Possible provider classes:

- licensed virtual-economy provider;
- payment/payout provider;
- regulated crypto payout provider;
- hosted financial provider.

Do not hard-code or falsely implement Tilia, Thunes, Zero Hash, Coinbase, BitGo, Stripe or another provider without verified APIs.

Create provider interfaces.

If official current API docs are available:
verify them.

If not:
implement interface + contract tests + sandbox fake + capability gate.

Do not invent endpoint names.

PayoutDestination types may include:

BANK
CARD_PUSH where supported
FIAT_WALLET where supported
CRYPTO_WALLET where supported

But every destination capability is provider-specific.

========================================================================
PART XIX — TARGET "KYC AT EXIT" EXPERIENCE
========================================================================

The target UX is:

User enters Nodal
-> participates in allowed internal activities
-> does not face financial-account onboarding at entry
-> later selects:
   "Cash Out" / "Withdraw Eligible Value"
-> provider identity/compliance flow begins
-> if approved:
   payout executes.

BUT THIS IS A TARGET PRODUCT ARCHITECTURE.

Do NOT assume:

- all users can trade native assets pre-KYC;
- all states permit it;
- all providers allow it;
- purchased Credits can be redeemed;
- trading profits can be redeemed;
- crypto payout is supported;
- only exit KYC is sufficient.

ProductionCapabilityGate controls each of those.

Example:

US_CA
+ INTERNAL_NATIVE_MARKET
+ PRE_KYC
= DISABLED

unless legal/provider evidence says otherwise.

Example:

US_APPROVED_STATE
+ CREATOR_EARNING
+ KYC_COMPLETE
+ PARTNER_PAYOUT
= ENABLED

only if supported.

========================================================================
PART XX — PAYOUT ELIGIBILITY
========================================================================

The system must compute:

gross_internal_balance
eligible_withdrawable_balance
ineligible_balance
pending_balance
frozen_balance

Never simply show:

18,500 Credits
= $185 withdrawable

unless policy says that is true.

PayoutEligibilityEngine must evaluate:

user jurisdiction
user age
verification status
provider capability
value provenance
funding finality
dispute status
account restrictions
asset source
market source
fraud flags
sanctions/compliance status
minimum amount
maximum amount
daily/monthly limits
policy version

Every decision is versioned and auditable.

========================================================================
PART XXI — PAYOUT STATE MACHINE
========================================================================

Implement:

DRAFT
ELIGIBILITY_CHECK
VERIFICATION_REQUIRED
VERIFICATION_PENDING
VERIFIED
SUBMITTED
PROVIDER_PENDING
SETTLED
FAILED
REJECTED
REVERSED
MANUAL_REVIEW

Payout requests must reserve eligible value before external submission.

If provider result is uncertain:
DO NOT release reservation and retry blindly.

Use:

PAYOUT_STATUS_UNKNOWN

or equivalent reconciliation-required state.

Exactly-once economic effect is required.

========================================================================
PART XXII — REAL CAPITAL RAILS
========================================================================

Create a clean abstraction:

CapitalRail

Types:

SIMULATED
NATIVE_INTERNAL
HOSTED_PARTNER
SELF_CUSTODIAL_ONCHAIN

Future:
SECURITIES_BROKER
PREDICTION_DCM

Every rail defines:

authoritative_balance_source
custody_model
execution_model
settlement_model
identity_requirements
jurisdiction_rules
asset_universe
agent_authority_limit
withdrawal_model
reconciliation_model

========================================================================
PART XXIII — HOSTED PARTNER RAIL
========================================================================

Create:

HostedFinancialProvider

Capabilities:

onboard_customer
read_capabilities
read_balances
read_positions
submit_customer_authorized_order
read_order
read_fills
request_withdrawal
read_statements if supported
tax artifacts if supported
webhook verification
provider health
reconciliation

Do NOT assume Zero Hash is chosen.

Support:

ZERO_HASH
BITGO
COINBASE
OTHER

as provider identifiers/configuration only where appropriate.

Provider adapters must be modular.

Nodal's hosted ledger is a MIRROR + RESERVATION SYSTEM.

Provider remains authoritative for actual hosted financial assets.

========================================================================
PART XXIV — SELF-CUSTODIAL ONCHAIN RAIL
========================================================================

Preserve useful existing Solana/Jupiter/wallet architecture.

This is now a separate rail.

Do not make it the only/default customer model.

Self-custodial rail requirements:

- customer-controlled wallet;
- no agent unrestricted private key;
- bounded authority if automation exists;
- transaction inspection;
- approved programs;
- approved tokens;
- max input;
- min output;
- no arbitrary transfers;
- no delegate/authority mutations unless explicitly part of approved workflow;
- fee-payer validation;
- expiry/blockhash checks;
- quote/plan identity;
- simulation where appropriate;
- finality tracking;
- chain reconciliation.

NO BLIND SIGNING.

If an upstream aggregator returns a transaction:

INDEPENDENTLY DECODE IT.

Verify what it actually does.

Malicious transaction fuzzing is required.

========================================================================
PART XXV — FINANCIAL INTENT
========================================================================

Every agent/manual action produces a typed:

FinancialIntent

NOT an arbitrary transaction.

Minimum shape:

intent_id
user_id
agent_id optional
strategy_version optional
capital_domain
action_type
instrument_id
side
quantity / notional
maximum_debit
minimum_receive
time_constraints
risk_context
jurisdiction_context
source_prediction
idempotency_key
created_at

Examples:

BUY_NATIVE_ASSET
SELL_NATIVE_ASSET
BUY_HOSTED_ASSET
SELL_HOSTED_ASSET
BUY_ONCHAIN_ASSET
SELL_ONCHAIN_ASSET
PURCHASE_INTERNAL_SERVICE
REQUEST_PAYOUT

========================================================================
PART XXVI — SETTLEMENT COMPILER
========================================================================

The Settlement Compiler remains a core moat.

Input:

FinancialIntent

Output:

immutable ExecutionPlan

It must determine:

- value domain;
- legal rail;
- provider;
- market;
- eligibility;
- asset status;
- policy;
- reservation requirements;
- quote requirements;
- risk evaluation;
- execution authority;
- required confirmation;
- reconciliation method.

The Settlement Compiler MUST NOT use an LLM for financial authority.

AI can propose.

Deterministic code decides.

========================================================================
PART XXVII — LEGAL ROUTER
========================================================================

Add a deterministic:

LegalCapabilityRouter

This is NOT a substitute for lawyers.

It executes pre-approved machine-readable policy.

Inputs:

jurisdiction
rail
asset
product
agent authority level
value origin
payout mode
provider
verification status
age status
fee model
policy version

Outputs:

ALLOW
DENY
REQUIRES_VERIFICATION
REQUIRES_USER_CONFIRMATION
REQUIRES_PROVIDER
REQUIRES_MANUAL_REVIEW

with reason codes.

========================================================================
PART XXVIII — AGENT AUTHORITY
========================================================================

Explicitly model agent authority.

AgentAuthorityLevel:

0 RESEARCH_ONLY

1 RECOMMENDATION
  Agent proposes.
  User decides.

2 PREPARE_TRANSACTION
  Agent prepares.
  User confirms/signs.

3 USER_APPROVED_RULE
  User reviews and activates exact deterministic rule.
  Subsequent execution may occur within approved bounds.

4 BOUNDED_DISCRETION
  Agent chooses among user-approved options.

5 AUTONOMOUS_SELECTION
  Agent independently chooses investments.

6 AUTONOMOUS_PORTFOLIO
  Agent allocates across strategies/assets.

DEFAULT PRODUCTION:

Levels 0-3 supported as product architecture.

Levels 4-6:
CAPABILITY DISABLED
until appropriate legal/provider approval.

Do not remove ability to implement them later.

========================================================================
PART XXIX — NATURAL LANGUAGE STRATEGY COMPILER
========================================================================

The user may say:

"Buy up to $100 of SOL if conditions X/Y/Z occur.
Never deploy more than $500.
Stop after $40 loss.
Expire after seven days."

AI may compile into typed Strategy IR.

Before activation:

SHOW THE USER THE ACTUAL RULE.

The user approves:

instrument universe
trigger
order amount
capital envelope
loss limit
duration
exit behavior

The runtime executes the IR.

The LLM does NOT silently reinterpret the rule during execution.

========================================================================
PART XXX — STRATEGY IR
========================================================================

Preserve/strengthen typed Strategy IR.

The IR should support:

data predicates
market predicates
wallet/onchain predicates
time conditions
Boolean composition
capital envelopes
max order size
max position
loss limit
cooldown
expiry
take-profit/stop-loss if supported
allowed instruments
forbidden instruments
execution mode
rail restrictions

Reject unsupported expressions.

No arbitrary code execution in live financial strategy execution.

========================================================================
PART XXXI — AGENT SECURITY
========================================================================

Treat every agent as hostile.

Agent may:

- read approved data;
- request model inference;
- generate analysis;
- commit prediction;
- propose typed intent.

Agent may NOT:

- access provider secrets;
- access signing secrets;
- withdraw;
- transfer arbitrary value;
- change its capital limit;
- change its risk constitution;
- edit production policy;
- alter jurisdiction policy;
- call arbitrary network destinations;
- execute arbitrary smart contracts;
- modify ledger;
- bypass Settlement Compiler;
- bypass risk;
- bypass compliance;
- bypass reservations.

========================================================================
PART XXXII — RISK KERNEL
========================================================================

Risk remains deterministic.

At minimum:

capital limits
max position
max order
daily loss
strategy loss
user loss
concentration
correlation where available
liquidity
slippage
stale data
market halt
provider health
asset restriction
native-market concentration
creator concentration
fraud/compliance block
rate limits

Kill switches stop NEW RISK.

They must NOT stop:

- observation;
- reconciliation;
- settlement tracking;
- required unwind workflows;
- evidence capture.

========================================================================
PART XXXIII — ELIGIBILITY / COMPLIANCE / RISK ARE SEPARATE
========================================================================

Create separate objects:

EligibilityDecision
ComplianceDecision
RiskDecision

Do not merge them.

Example:

financially safe trade
+
jurisdiction disallowed
=
DENY

Example:

legally allowed
+
daily loss exceeded
=
DENY

All decisions have:

policy_version
reason_codes
inputs_hash
created_at
evidence reference

========================================================================
PART XXXIV — NATIVE ASSET LEGAL/PRODUCT CLASSIFICATION
========================================================================

Even internal assets need classification.

Add:

NativeAssetPolicyProfile

At minimum:

internal_only
transferability
cashout_eligible
creator_earning_eligible
market_trading_proceeds_eligible
age_policy
jurisdiction_policy
marketing_restrictions
content restrictions
review status

Do NOT label internal speculative assets "investments" or promise returns by default.

========================================================================
PART XXXV — EXTERNAL INSTRUMENT REGISTRY
========================================================================

Preserve/extend external Instrument Registry.

Fields include:

instrument
provider
asset_class
jurisdiction
legal_status
technical_status
risk_status
provider_status
last_review
evidence
allowed_rails

States:

ACTIVE
CLOSE_ONLY
BLOCKED
LEGAL_REVIEW
PROVIDER_UNAVAILABLE
HALTED
DELISTED

"Jupiter can quote this" does NOT imply:
"Nodal may expose it."

========================================================================
PART XXXVI — RESERVATIONS
========================================================================

Reservations are authoritative and transactional.

Use for:

- Credit spend;
- native asset purchase;
- real-capital order;
- payout;
- marketplace purchase;
- creator purchase.

Concurrency invariant:

If balance = 10,000 Credits
and 100 concurrent requests each try to reserve 1,000:

EXACTLY 10 may succeed.

Not 9.
Not 11.

Build the torture test.

========================================================================
PART XXXVII — EXACTLY-ONCE ECONOMIC EFFECT
========================================================================

Do not claim distributed "exactly once."

Implement:

idempotency
durable local state
transactional reservation
outbox
provider identifiers
reconciliation
state machines

The guarantee is:

ONE ECONOMIC EFFECT

even when messages/processes execute multiple times.

========================================================================
PART XXXVIII — CRASH TESTS
========================================================================

Required scenario:

1. Nodal submits external transaction/order/payout.
2. External system succeeds.
3. Process crashes BEFORE local success persistence.
4. Process restarts.
5. System must discover external success.
6. System must NOT duplicate operation.
7. Ledger/mirror must reconcile.

Implement this for every external economic provider category.

========================================================================
PART XXXIX — INTERNAL EVENTING
========================================================================

Financial authority belongs in transactional core.

Events are evidence/integration signals.

Events do not become financial truth.

Use durable outbox.

Consumers must be idempotent.

Do not let Kafka/Redpanda consumer duplication move money twice.

========================================================================
PART XL — PROVIDER WEBHOOKS
========================================================================

For all provider webhooks:

verify signature
verify timestamp
prevent replay
persist raw evidence
assign idempotency key
parse safely
validate schema
use inbox/dedupe
process transactionally
reconcile contradictions

Never trust webhook order.

Never assume webhook delivery.

========================================================================
PART XLI — PROVIDER HEALTH
========================================================================

Every provider has:

AVAILABLE
DEGRADED
READ_ONLY
UNAVAILABLE
UNKNOWN

Provider health can restrict new activity.

Existing operations still reconcile.

========================================================================
PART XLII — REALITY ENGINE
========================================================================

Keep and harden.

Every relevant event should preserve temporal provenance such as:

source_event_at
provider_published_at
nodal_received_at
normalized_at
feature_available_at
decision_available_at

No lookahead.

No historical backtest should use data not available at decision time.

========================================================================
PART XLIII — PREDICTION LEDGER
========================================================================

Every live/shadow/canary strategy decision should be able to commit:

prediction
confidence
time horizon
expected outcome
supporting features/data references
strategy version
agent version
market state

before outcome is known.

Calibration later compares prediction to outcome.

========================================================================
PART XLIV — PERFORMANCE MODES
========================================================================

Modes are immutable concepts:

BACKTEST
PAPER
SHADOW
CANARY
LIMITED
LIVE

Do not mix them.

Performance UI must always identify mode.

Never allow backtest returns to display as live returns.

========================================================================
PART XLV — PROOF / AUDIT
========================================================================

Build canonical evidence.

For economically important actions capture:

who
what
when
why
authority
strategy
prediction
eligibility
compliance
risk
reservation
execution plan
provider
quote
transaction/order
fill
ledger effect
reconciliation
outcome

Use canonical serialization.

Hash important evidence.

Preserve tamper-evident chaining where existing architecture supports it.

========================================================================
PART XLVI — DATA LICENSE POLICY
========================================================================

Preserve a per-source:

DataLicensePolicy

Fields:

can_store_raw
retention_duration
can_backtest
can_train
can_redistribute
can_display
can_create_derived_data
deletion_requirements
contract evidence

Do not treat API access as unlimited archival permission.

========================================================================
PART XLVII — AUTHENTICATION
========================================================================

Support secure lightweight product signup.

Preferred:

passkey
email verification
secure session management

Financial verification is separate.

Do not conflate:

NodalIdentity

with:

FinancialIdentityVerification

A user may have:

NodalIdentity = VERIFIED

FinancialIdentity = NOT_ACTIVATED

PayoutVerification = NOT_STARTED

HostedFinancialVerification = NOT_STARTED

========================================================================
PART XLVIII — AGE / JURISDICTION
========================================================================

Age rules are policy-driven.

Do not invent.

Native speculative markets should default to a conservative policy until approved.

Real financial providers have independent age requirements.

EligibilityEngine determines.

========================================================================
PART XLIX — ADMIN SECURITY
========================================================================

No direct DB edits for money.

Build audited admin workflows for:

freeze user
freeze market
close-only market
disable asset
disable provider
disable jurisdiction
disable agent
refund
reverse allowed internal transaction
manual payout review
reconciliation resolution
economic adjustment

High-risk actions require:

reason
actor
ticket/reference
before state
after state
audit evidence

Consider dual approval for production-critical financial changes.

========================================================================
PART L — FRAUD / ABUSE
========================================================================

Model:

payment fraud
chargebacks
multi-account abuse
wash trading
creator scams
impersonation
phishing metadata
malicious links
market manipulation
bot spam
bonus farming
promotional credit abuse
payout fraud
account takeover
credential stuffing
device farms

Build controls and observability.

========================================================================
PART LI — USER-GENERATED CONTENT
========================================================================

Nodal-native asset creation produces UGC.

Need:

name moderation
symbol moderation
description moderation
image/URL moderation hooks
report flow
takedown
creator identity records
impersonation complaints
copyright/trademark workflow placeholder
illegal-content handling
audit trail

========================================================================
PART LII — FRONTEND
========================================================================

The UI should make the architecture understandable without overwhelming users.

Core product areas:

Home
Markets
Native Markets
Create Asset
Agents
Agent Builder
Lab
Backtests
Shadow
Data
Portfolio
Real Capital
Payouts
Settings

Home should distinguish:

Nodal Economy
Simulated Capital
Real Capital

Never misrepresent balances.

========================================================================
PART LIII — CREATE ASSET UX
========================================================================

Flow:

Create Nodal Asset
-> name
-> symbol
-> description
-> optional image
-> economics preview
-> creator allocation
-> market mechanics explanation
-> fees
-> risk warning
-> moderation
-> activate

Show exact immutable economics before launch.

No deceptive "guaranteed profit" language.

========================================================================
PART LIV — NATIVE MARKET UX
========================================================================

Display:

price
market capitalization equivalent if meaningful
supply
liquidity/reserve
creator holdings
holder concentration
volume
fees
market mechanics
risk warning
asset status
trade history
user position

Do not fake dollar valuations if there is no approved cash equivalence.

Prefer:

Price: 2.4 Credits

not:

Price: $0.024

unless Credits have an approved externally redeemable value and disclosure.

========================================================================
PART LV — CASHOUT UX
========================================================================

Do not show a guaranteed cash value before legally/provider supported.

Possible states:

Cash Out unavailable
Cash Out eligibility pending
Verification required
Eligible amount
Provider processing
Paid
Rejected
Manual review

Explain WHY.

========================================================================
PART LVI — REAL CAPITAL UX
========================================================================

Real Capital is separate.

Possible actions:

Connect wallet
Activate hosted account
Deposit
Withdraw
Enable strategy
View positions

Do not force real-capital activation to use the rest of the product.

========================================================================
PART LVII — API DESIGN
========================================================================

Public commands must be idempotent where economic.

Examples:

POST /credit-purchases
POST /native-assets
POST /native-markets/{id}/quotes
POST /native-markets/{id}/orders
POST /agents
POST /strategies
POST /strategies/{id}/activate
POST /financial-intents
POST /payouts

Use REST command/query semantics or existing coherent architecture.

Do NOT use GraphQL mutations as primary money-moving commands.

========================================================================
PART LVIII — DATABASE CONSTRAINTS
========================================================================

Use database constraints, not only application checks.

Examples:

unique idempotency keys
nonnegative quantity where appropriate
immutable journal rows
foreign keys
state transition constraints where feasible
unique external provider IDs
unique webhook IDs
version checks
optimistic concurrency

Critical correctness should survive buggy application code.

========================================================================
PART LIX — MIGRATIONS
========================================================================

Every schema change:

migration
rollback strategy
compatibility analysis
data backfill
verification query
production runbook

No destructive migration without staged deployment.

========================================================================
PART LX — SECURITY
========================================================================

Threat-model:

financial theft
credit creation
double spend
payout theft
provider credential compromise
signing compromise
agent escape
prompt injection
admin takeover
SQL injection
SSRF
XSS
CSRF
session theft
webhook spoofing
supply-chain compromise
container escape
cross-tenant access
race conditions
business logic abuse

Create:

docs/security/THREAT_MODEL.md

For each threat:

asset
attacker
attack
control
test
residual risk

========================================================================
PART LXI — SECRET MANAGEMENT
========================================================================

No production secret in:

repo
frontend
logs
analytics
crash dumps
Docker layer

Use secrets manager / KMS.

Provider credentials scoped per capability.

Signing infrastructure isolated.

========================================================================
PART LXII — NETWORK / IAM
========================================================================

Least privilege.

Separate roles for:

API
workflow worker
ledger
analytics
admin
signer
provider integration

Database roles should reflect authority.

Do not let analytics service mutate ledger.

Do not let frontend call financial DB.

========================================================================
PART LXIII — CONFIGURATION
========================================================================

Production must fail closed.

If required legal/provider configuration is missing:

capability remains disabled.

Never:

if configMissing:
    allowEverything()

Fresh production deployment:

ALL REAL-MONEY / REAL-PAYOUT CAPABILITIES DISABLED.

Activation requires explicit evidence-backed enablement.

========================================================================
PART LXIV — PRODUCTION CAPABILITY GATES
========================================================================

Capability key should include:

Jurisdiction
Provider
Rail
Product
Asset
AgentAuthorityLevel
ValueOrigin
PayoutMode
CompensationModel
VerificationLevel

Every active capability has:

status
policy_version
approved_by
evidence
effective_at
expiration/review date
notes

High-risk activation should support dual approval.

========================================================================
PART LXV — NO MOCKS IN PRODUCTION
========================================================================

Production binary/configuration must make mock provider use impossible.

Test doubles only in:

test
local dev
explicit sandbox

Add programmatic assertion.

========================================================================
PART LXVI — INFRASTRUCTURE
========================================================================

Preserve proven architecture if already correct.

Preferred existing architecture may include:

Go financial core
TypeScript/Next.js frontend
PostgreSQL
Temporal
Redpanda/Kafka
ClickHouse
S3
Redis cache
OpenTelemetry
AWS ECS/Fargate
Terraform

Do not migrate stacks for aesthetics.

If current repo differs:
use evidence and document decision.

========================================================================
PART LXVII — OBSERVABILITY
========================================================================

Metrics:

ledger commit latency
reservation contention
market execution latency
quote rejection
state machine age
provider latency
provider error rate
webhook lag
reconciliation mismatch
payout failure
chargeback rate
credit issuance
credit destruction
native market volume
wash-trade alerts
agent intent rejection
risk rejection
compliance rejection
event backlog

Logs must have:

request ID
intent ID
user ID internal
strategy version
provider request ID

Do not log secrets or excessive PII.

========================================================================
PART LXVIII — SLOS
========================================================================

Define measurable SLOs for:

API availability
ledger writes
native market execution
payout workflow
hosted provider adapter
reconciliation
agent decisions
market data freshness

Do not invent impossible SLOs.

Load test and measure.

========================================================================
PART LXIX — DISASTER RECOVERY
========================================================================

Build and document:

backup
restore
RPO
RTO
DB restore
object evidence restore
provider reconciliation after outage
replay procedure
region failure procedure
credential rotation
incident response

Actually test restore where environment allows.

========================================================================
PART LXX — CHAOS TESTS
========================================================================

Kill:

API process
workflow worker
DB connection
event broker
provider response
webhook delivery
Redis
analytics
network

during economic workflows.

Verify:

no money creation
no double spend
no duplicate payout
no duplicate external order
eventual reconciliation
clear unknown states

========================================================================
PART LXXI — FINANCIAL PROPERTY TESTS
========================================================================

Required property families:

LEDGER CONSERVATION
DEBIT == CREDIT

NO DOUBLE SPEND

RESERVATION SAFETY

IDEMPOTENCY

STATE MACHINE VALIDITY

ROUNDING

OVERFLOW

REPLAY SAFETY

PROVIDER DUPLICATE SAFETY

PAYOUT ELIGIBILITY

VALUE DOMAIN ISOLATION

MARKET INVARIANT

CREATOR SUPPLY INVARIANT

========================================================================
PART LXXII — REQUIRED ADVERSARIAL TESTS
========================================================================

At minimum implement tests for:

1. 100 concurrent Credit spends against insufficient balance.
2. 100 concurrent native-asset buys.
3. duplicate order request.
4. duplicate webhook.
5. out-of-order webhook.
6. provider timeout after success.
7. process crash after provider success.
8. chargeback after Credits spent.
9. payout request during pending chargeback.
10. payout using promotional Credits.
11. payout using ineligible trading proceeds.
12. market creator attempting hidden supply increase.
13. self-trade.
14. two accounts under same controlled actor wash-trading where detectable.
15. stale quote.
16. market state changes between quote and execution.
17. integer overflow.
18. rounding edge at minimum unit.
19. malicious negative quantity.
20. malicious huge quantity.
21. replay old request.
22. modify idempotency payload.
23. compromised provider sends contradictory status.
24. agent tries to withdraw.
25. agent attempts prohibited asset.
26. agent attempts limit escalation.
27. admin attempts unaudited balance mutation.
28. mock provider used in production config.
29. legal capability missing.
30. jurisdiction turns blocked mid-session.

========================================================================
PART LXXIII — SOLANA SECURITY TESTS
========================================================================

If current onchain execution remains:

fuzz malicious transaction payloads.

Attempt:

unexpected SystemProgram transfer
extra recipient
wrong mint
wrong token program
delegate change
authority change
unknown program
hidden account writable
fee theft
incorrect min output
incorrect max debit
wrong wallet
wrong fee payer
expired blockhash
quote mismatch
plan mismatch
token-2022 edge cases
malformed address tables
multiple instructions

No blind signing.

========================================================================
PART LXXIV — LOAD TESTING
========================================================================

Measure realistic workloads.

Internal markets:

concurrent quotes
concurrent trades
market hot spots

Agents:

decision fanout
market event spikes

Provider:

rate limit
latency
backpressure

Ledger:

reservation contention
journal throughput

Do not fake performance evidence.

========================================================================
PART LXXV — FRONTEND SECURITY / HONESTY
========================================================================

The UI must never hide:

simulated vs real
internal vs external
cashable vs noncashable
pending vs settled
estimate vs guaranteed
backtest vs live

Avoid deceptive financial dark patterns.

========================================================================
PART LXXVI — PROVIDER INTEGRATION POLICY
========================================================================

Potential providers may include:

Tilia / Thunes
Zero Hash
BitGo
Coinbase
Stripe
other approved providers

Do not assume any specific one is commercially available.

Build adapters behind interfaces.

If official API docs are accessible:
verify current version and implement.

If not:
BLOCKED_EXTERNAL only for live connectivity.

Provider capability configuration:

supports_credit_purchase
supports_internal_wallet
supports_virtual_currency
supports_payout
supports_crypto_payout
supports_fiat_payout
supports_kyc_at_exit
supports_hosted_crypto
supports_usd_balance
supports_webhooks
supports_tax_reporting

Never infer capability from marketing copy.

========================================================================
PART LXXVII — LEGAL/COMMERCIAL EVIDENCE
========================================================================

The repository must define external evidence requirements.

Examples:

LEGAL_CA_NATIVE_MARKET_PRE_KYC
PROVIDER_NATIVE_MARKET_APPROVAL
PROVIDER_KYC_AT_EXIT_APPROVAL
PROVIDER_TRADING_PROCEEDS_PAYOUT
PROVIDER_CREATOR_EARNINGS_PAYOUT
PROVIDER_CRYPTO_PAYOUT
ZERO_HASH_HOSTED_ACCOUNT_APPROVAL
ONCHAIN_DELEGATED_SIGNER_OPINION
ASSET_CLASSIFICATION_POLICY

Capability activation requires evidence records.

========================================================================
PART LXXVIII — ECONOMIC MODEL VALIDATION
========================================================================

Before finalizing native market economics:

simulate:

normal users
creator buys
whale buy
whale sell
extreme concentration
zero liquidity
rapid churn
fee accumulation
rounding boundaries
spam asset creation
mass delisting
funding reversals

Ensure system cannot:

create value from rounding
lose accounting conservation
trap impossible states
allow negative reserve
overflow
break during full liquidation

========================================================================
PART LXXIX — PRODUCT FEES
========================================================================

Fee types must be explicit:

Credit purchase fee
Native market fee
Creator fee
Platform service fee
Agent compute fee
Data fee
Payout provider fee
Real-capital provider fee

Do not hide spread.

Do not implement performance/AUM fees unless gated.

========================================================================
PART LXXX — MARKET CREATOR FEES
========================================================================

If creator receives market fees:

record as:

MARKET_CREATOR_EARNING

not ordinary Credit purchase.

Separate payout eligibility.

Creator cannot set arbitrary hidden fee after launch.

========================================================================
PART LXXXI — REFUNDS
========================================================================

Define refund behavior.

If unused Credits can be refunded:
explicit policy.

If Credits already used in native markets:
refund semantics become complex.

Do not silently reverse trades.

Use explicit dispute/chargeback economic handling.

========================================================================
PART LXXXII — WITHDRAWAL / PAYOUT FREEZE
========================================================================

Payout can be frozen for:

chargeback risk
fraud
account takeover
sanctions
provider review
market investigation
funding finality
manual review

Reason must be auditable.

========================================================================
PART LXXXIII — RECONCILIATION
========================================================================

Reconciliation sources:

Credit payment provider
Payout provider
Hosted financial provider
Blockchain
Internal ledger
Native market engine

Reconciliation state:

MATCHED
PENDING
MISMATCH
INVESTIGATING
RESOLVED

Do not "fix" mismatches by silently editing balances.

========================================================================
PART LXXXIV — DATA RETENTION / PRIVACY
========================================================================

Classify data:

PII
financial identity
financial state
agent data
market activity
telemetry
raw provider evidence

Define retention.

Minimize sensitive data.

Do not duplicate SSN/identity docs from financial provider unless necessary.

========================================================================
PART LXXXV — BRAND CONFIGURATION
========================================================================

No hard-coded public "Nodal" brand.

Use configuration for:

company legal name
product name
support email
disclosures
terms URLs
privacy URLs
provider disclosures

========================================================================
PART LXXXVI — LOCAL DEVELOPMENT
========================================================================

One command or very small set of commands should launch:

DB
event infra
analytics if needed
object storage emulator if needed
backend
frontend
provider sandboxes
test payment/payout providers
fake market data

Document setup.

========================================================================
PART LXXXVII — CI
========================================================================

CI must include:

format
lint
typecheck
unit
integration
migration validation
race detector
static analysis
secret scan
dependency vulnerability scan
financial property tests
security tests
contract tests
frontend build
Terraform validation

Long chaos/load suites may run separately but must exist.

========================================================================
PART LXXXVIII — RELEASE PROCESS
========================================================================

Stages:

LOCAL
CI
SANDBOX
STAGING
CANARY
PRODUCTION

No direct developer deploy to live financial capabilities.

Release artifact immutable.

Infrastructure changes reviewed.

========================================================================
PART LXXXIX — FEATURE FLAGS VS FINANCIAL GATES
========================================================================

Do not use ordinary feature flag as sole protection for legal financial capability.

Feature flag:
UX rollout.

ProductionCapabilityGate:
authority.

Both may exist.

========================================================================
PART XC — OPERATIONAL RUNBOOKS
========================================================================

Write runbooks for:

ledger mismatch
credit funding dispute
payout stuck
provider outage
market halt
market manipulation investigation
compromised account
compromised credential
blockchain reorg/finality anomaly
native market corruption
bad deployment
rollback
database restore
legal capability emergency shutdown

========================================================================
PART XCI — AUDIT REPORTING
========================================================================

At the end of each major phase update:

INDEPENDENT_AUDIT.md

with:

claim
evidence
test performed
result
defect
fix
remaining risk

Do not self-score vaguely.

========================================================================
PART XCII — REQUIREMENT ACCEPTANCE TESTS
========================================================================

Create explicit acceptance IDs.

At minimum:

FIN-001
Journal balances exactly.

FIN-002
No double spend under concurrency.

FIN-003
Reservation cannot exceed spendable balance.

FIN-004
External success + local crash reconciles without duplication.

FIN-005
Provider duplicate webhook cannot duplicate economic effect.

FIN-006
Reversible funding cannot become payout-eligible.

VAL-001
Internal Credit cannot enter real-capital ledger without approved conversion.

VAL-002
Promotional Credits cannot cash out.

VAL-003
Payout engine enforces provenance.

MKT-001
Market invariant holds after every trade.

MKT-002
Market cannot execute stale quote.

MKT-003
Creator cannot modify activated supply model.

MKT-004
Self-trade rejected.

MKT-005
Extreme trade cannot underflow/overflow reserve.

AGT-001
Agent cannot withdraw.

AGT-002
Agent cannot exceed capital envelope.

AGT-003
Agent cannot modify own policy.

AGT-004
User-approved Strategy IR executes deterministically.

PAY-001
Payout reserves value exactly once.

PAY-002
Payout provider timeout does not duplicate payout.

PAY-003
Ineligible provenance rejects.

PAY-004
Unverified user cannot use payout requiring verification.

LEG-001
Disabled jurisdiction cannot activate capability.

LEG-002
Missing evidence cannot activate production gate.

SEC-001
Production cannot load mock provider.

SEC-002
Provider webhook replay rejected.

SEC-003
Cross-tenant access rejected.

SEC-004
Admin balance adjustment is audited.

ONC-001
Malicious Solana transaction with arbitrary transfer rejects.

OPS-001
Restore from backup succeeds in test environment.

OPS-002
Provider outage leaves system recoverable.

========================================================================
PART XCIII — IMPLEMENTATION STAGES
========================================================================

Execute in this order unless repo evidence demands a documented alternative.

STAGE 0
Freeze baseline.
Run current tests.
Record exact git SHA.
Inventory architecture.
Record existing claims.

STAGE 1
Independent adversarial audit.
Find defects before migration.
Fix critical existing correctness/security bugs immediately.

STAGE 2
Introduce ValueDomain / CapitalRail / provenance abstractions.
Migrate existing architecture without breaking behavior.

STAGE 3
Harden authoritative accounting.
Ledger, reservations, idempotency, outbox, reconciliation.

STAGE 4
Implement Credit ledger and Credit funding lifecycle.

STAGE 5
Implement Nodal-native asset registry and creation.

STAGE 6
Implement one production-quality native market engine.

STAGE 7
Implement native-market surveillance/integrity.

STAGE 8
Implement internal commerce / creator economy.

STAGE 9
Implement PayoutEligibility and PayoutProvider architecture.
Live payout remains gated until external approval.

STAGE 10
Refactor hosted real-capital rail.

STAGE 11
Refactor/preserve self-custodial onchain rail.

STAGE 12
Unify all rails behind FinancialIntent + Settlement Compiler.

STAGE 13
Implement LegalCapabilityRouter / expanded ProductionCapabilityGate.

STAGE 14
Harden agents, Strategy IR, authority levels.

STAGE 15
Reality / Prediction / Proof integration.

STAGE 16
Frontend migration to final product model.

STAGE 17
Admin/operations tooling.

STAGE 18
Infrastructure/security/IAM hardening.

STAGE 19
Property testing / fuzzing / adversarial testing.

STAGE 20
Chaos, recovery, backup/restore.

STAGE 21
Load/performance testing.

STAGE 22
Provider sandbox integration where externally possible.

STAGE 23
Final independent re-audit.

STAGE 24
Produce launch evidence package.

Do not jump to Stage 24 because tests appear green.

========================================================================
PART XCIV — COMMIT DISCIPLINE
========================================================================

After each coherent stage:

run verification
update build state
update traceability
commit

Use meaningful commits.

Do not leave enormous uncommitted changes.

========================================================================
PART XCV — WHEN CONTEXT IS RUNNING OUT
========================================================================

Before context exhaustion:

1. update MASTER_BUILD_STATE.md;
2. update REQUIREMENTS_TRACEABILITY.md;
3. update BLOCKERS.md;
4. update DECISION_REGISTER.md;
5. update AUDIT_FINDINGS.md;
6. record exact next command/task;
7. commit stable work.

A future context must be able to continue without rediscovering the entire repository.

========================================================================
PART XCVI — THINGS YOU MUST NOT DO
========================================================================

DO NOT:

- rewrite repository blindly;
- silently weaken tests;
- delete failing tests to get green;
- hard-code provider approvals;
- invent legal conclusions;
- invent provider APIs;
- make Credits secretly convertible;
- implement fake KYC bypass;
- mix internal Credits with real customer cash;
- represent virtual balance as FDIC cash;
- create NodalUSD;
- create synthetic crypto CFDs;
- add leverage;
- add perpetuals;
- add futures;
- add internal customer crossing for external assets;
- give agents withdrawal authority;
- blindly sign blockchain transactions;
- store money in float types;
- use Redis as financial truth;
- allow arbitrary production DB edits;
- enable mocks in production;
- treat event delivery as exactly-once money movement;
- pretend an external blocker is solved.

========================================================================
PART XCVII — EXTERNAL ITEMS THAT MAY REMAIN BLOCKED
========================================================================

Likely legitimate blockers include:

- exact virtual-economy provider approval;
- whether pre-KYC native market trading is contractually/legal allowed;
- whether payout can truly be KYC-at-exit;
- whether trading proceeds can cash out;
- whether creator earnings can cash out;
- whether crypto payout is supported;
- provider production credentials;
- state-by-state legal activation matrix;
- California-specific opinion;
- FinCEN treatment of exact structure;
- self-custodial delegated automation opinion;
- hosted-provider commercial approval.

For every blocker, write:

BLOCKER ID
decision needed
why external
what code already supports
exact evidence required
who must provide it
capabilities currently disabled

========================================================================
PART XCVIII — FINAL LAUNCH EVIDENCE PACKAGE
========================================================================

Produce:

docs/release/PRODUCTION_READINESS_REPORT.md
docs/release/LAUNCH_GATE.md
docs/release/EXTERNAL_APPROVALS_REQUIRED.md
docs/release/TEST_EVIDENCE.md
docs/release/SECURITY_EVIDENCE.md
docs/release/FINANCIAL_INVARIANTS.md
docs/release/RECOVERY_EVIDENCE.md
docs/release/PROVIDER_READINESS.md
docs/release/KNOWN_LIMITATIONS.md

The report must distinguish:

CODE READY
SANDBOX READY
STAGING READY
PROVIDER READY
LEGAL READY
LIVE CAPITAL READY

These are NOT synonyms.

========================================================================
PART XCIX — DEFINITION OF SOFTWARE COMPLETE
========================================================================

SoftwareComplete = TRUE only when:

- architecture migrated;
- no known P0/P1 code defect;
- ledger invariants proven;
- native economy implemented;
- market engine implemented;
- payout architecture implemented;
- real-capital abstractions implemented;
- agent system integrated;
- reconciliation works;
- security boundaries tested;
- chaos/recovery tested;
- admin workflows exist;
- frontend accurately reflects value domains;
- CI green;
- migrations valid;
- infrastructure reproducible;
- production gates fail closed;
- test evidence exists;
- external-only blockers are isolated.

========================================================================
PART C — DEFINITION OF LIVE READY
========================================================================

LiveReady = TRUE only when:

SoftwareComplete
AND provider production contract
AND provider production credentials
AND required legal approvals
AND jurisdiction matrix
AND required security review
AND staging evidence
AND controlled canary
AND no unresolved P0 financial defect.

If external approval is absent:

SoftwareComplete may be TRUE.

LiveReady MUST remain FALSE.

========================================================================
PART CI — FINAL PRODUCT TEST
========================================================================

When finished, the following journeys should work in appropriate test/sandbox environments:

JOURNEY A — FRICTIONLESS ENTRY

User signs up
-> receives lightweight Nodal identity
-> creates an agent
-> runs backtest
-> runs shadow strategy
-> no financial account required.

JOURNEY B — NODAL ECONOMY

User obtains test/sandbox Credits
-> creates Nodal-native asset
-> launches market
-> another user buys
-> market price changes deterministically
-> users sell
-> ledger remains balanced
-> positions correct.

JOURNEY C — CREATOR ECONOMY

User sells an internal data/agent service
-> buyer pays Credits
-> creator receives provenance-tagged earning
-> payout eligibility determined by policy.

JOURNEY D — PAYOUT

Eligible user requests payout
-> eligibility decision
-> verification requirement
-> provider submission
-> settlement
-> no duplicate payout under crash/retry.

JOURNEY E — REAL HOSTED CAPITAL

User activates sandbox hosted account
-> provider balance visible
-> agent Level 3 strategy activated
-> FinancialIntent
-> risk
-> hosted execution
-> fill
-> reconciliation.

JOURNEY F — SELF-CUSTODIAL

User connects sandbox/test wallet
-> agent prepares approved intent
-> transaction constructed
-> independently inspected
-> authorized
-> submitted
-> finalized
-> reconciled.

JOURNEY G — FAILURE

External execution succeeds
-> Nodal crashes
-> restart
-> discovers success
-> no duplicate
-> correct final state.

========================================================================
PART CII — THE ENDGAME
========================================================================

Do not lose sight of what this company becomes.

More agents
-> more decisions
-> more point-in-time evidence
-> more predictions
-> more outcomes
-> better calibration
-> better causal attribution
-> better strategy comparison
-> better agent reputation
-> better markets
-> better capital allocation.

The long-term moat is NOT custody.

It is:

- decision provenance;
- Reality Engine;
- Prediction Ledger;
- strategy lineage;
- causal alpha;
- capacity;
- agent reputation;
- risk intelligence;
- execution intelligence;
- market interaction data;
- failure memory;
- allocation intelligence.

Financial infrastructure underneath should remain modular.

========================================================================
FINAL COMMAND
========================================================================

Start now.

First:

1. determine exact current git state;
2. run the existing verification suite;
3. inspect repository architecture;
4. inspect all build/readiness documentation;
5. create CURRENT_SYSTEM_INVENTORY.md;
6. create KEEP_MODIFY_REPLACE_MATRIX.md;
7. begin an independent adversarial audit;
8. DO NOT start rewriting until you understand what is already correct.

Then execute the stages continuously.

Do not ask me broad architectural questions that can be answered by inspecting the repository.

Make reasonable reversible engineering decisions and record them.

Ask only when a genuinely non-recoverable product decision cannot be inferred.

Do not stop because a previous document says "complete."

Stop only when:

A. you have exhausted what can be improved from code and test evidence,
AND
B. remaining blockers genuinely require external human/provider/legal evidence.

At that point give me:

1. what you found wrong;
2. what you fixed;
3. what architecture changed;
4. what is now proven;
5. exact test/evidence results;
6. what still blocks real activation;
7. the exact next human/provider/legal actions required.

Until then:

KEEP BUILDING.
KEEP ATTACKING.
KEEP FIXING.
KEEP VERIFYING.