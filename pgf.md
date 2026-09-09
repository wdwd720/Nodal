# NODAL — PRODUCTION PROVIDER ACTIVATION WORKSTREAM
# STRIPE FIAT-IN + STRIPE CRYPTO PAYOUT + STRIPE IDENTITY/KYC + SELF-CUSTODIAL WALLET
# BROWSER SETUP + CODE INTEGRATION + SECURITY + RECONCILIATION + PRODUCTION EVIDENCE

You are taking ownership of a dedicated production-integration workstream inside the existing Nodal repository.

This is NOT a greenfield build.

The main Nodal architecture/audit already exists and must be preserved.

Your mission is to take FOUR currently external/incomplete areas and push each one as far toward production readiness as can legitimately be done:

1. PAYMENT PROVIDER
   Stripe as the sole V1 fiat-in / Credit-purchase provider.

2. PAYOUT PROVIDER
   Stripe as the preferred sole V1 crypto/stablecoin payout provider, if and only if Stripe's current products and our account are eligible for the exact use case.

3. IDENTITY / FINANCIAL KYC
   Prefer Stripe-managed onboarding / verification for the payout boundary so Nodal does not unnecessarily collect sensitive identity documents itself.

4. WALLET / CUSTODY
   Nodal does NOT custody payout crypto.
   The user controls the external wallet.
   Phantom/Solana-compatible self-custodial wallets are the primary V1 destination/user-capital model.

You must use both:

A. the code repository;
B. Claude in Chrome / browser tooling if available

to verify current Stripe capabilities, configure sandbox/test resources, retrieve official configuration values, and complete every safe setup task that can actually be completed.

Do NOT blindly trust previous assumptions about Stripe products.

Verify current official Stripe documentation and the user's actual Stripe Dashboard.

Do NOT invent API names, features, availability, supported assets, chains, Connect modes, or account capabilities.

If Stripe does not support the exact desired feature for this account/business model, record that accurately and keep the production gate disabled.

========================================================================
SECTION 0 — CRITICAL OPERATING RULE
========================================================================

DO NOT TURN A PRODUCT ASSUMPTION INTO A FALSE PROVIDER CLAIM.

The target architecture is:

USER
  |
  | card / supported payment rail
  v
STRIPE
  |
  | successful eligible Credit purchase
  v
NODAL CREDIT LEDGER
  |
  +--------------------------------------------+
  |                                            |
  v                                            v
Nodal-native markets                    Nodal services
Credits <-> native assets               AI / data / agents / compute
  |
  v
eligible payout provenance
  |
  v
USER CLICKS WITHDRAW
  |
  v
STRIPE-MANAGED FINANCIAL VERIFICATION
  |
  v
STRIPE-APPROVED STABLECOIN/CRYPTO PAYOUT
  |
  v
USER-CONTROLLED EXTERNAL WALLET
e.g. Phantom-compatible wallet

The user's desired V1 philosophy is:

- Stripe is the only fiat payment provider;
- no Tilia requirement;
- no bank cash-out requirement;
- payout is crypto/stablecoin only;
- Nodal should not custody the user's external crypto;
- KYC should occur as late as legitimately possible, ideally when the user activates payout;
- production behavior must exactly match what Stripe and applicable policy actually allow.

"Prefer KYC at exit" is a TARGET UX.

It is NOT permission to bypass Stripe requirements.

========================================================================
SECTION 1 — FIRST INSPECT THE EXISTING REPOSITORY
========================================================================

Before changing code:

1. inspect current git state;
2. identify current branch;
3. identify latest clean checkpoint;
4. read:
   docs/build/MASTER_BUILD_STATE.md
   docs/build/REQUIREMENTS_TRACEABILITY.md
   docs/build/BLOCKERS.md
   docs/build/DECISION_REGISTER.md
   docs/audit/AUDIT_FINDINGS.md
   docs/audit/LAUNCH_GATE_MATRIX.md
5. locate:
   CreditPurchaseProvider
   PayoutProvider
   PayoutEligibilityEngine
   ProductionCapabilityGate
   LegalCapabilityRouter
   ledger
   reservations
   webhook infrastructure
   provider abstractions
   identity abstractions
   CapitalRail
   wallet / Solana adapters
   admin controls
   frontend funding/payout flows
6. determine what is already correct;
7. do not duplicate abstractions unnecessarily.

Create:

docs/providers/STRIPE_INTEGRATION_STATE.md
docs/providers/STRIPE_BROWSER_SETUP.md
docs/providers/STRIPE_CAPABILITY_MATRIX.md
docs/providers/STRIPE_PRODUCTION_CHECKLIST.md
docs/providers/WALLET_INTEGRATION_STATE.md

========================================================================
SECTION 2 — BROWSER / CLAUDE IN CHROME RULES
========================================================================

If Claude in Chrome or equivalent browser automation is available, USE IT.

Use it for:

- Stripe official docs;
- Stripe Dashboard;
- test-mode configuration;
- webhook configuration;
- product/account capability discovery;
- sandbox onboarding configuration;
- test connected-account configuration if relevant;
- Stripe Identity configuration if relevant;
- payout capability discovery;
- stablecoin/crypto payout capability discovery;
- official supported asset/network discovery;
- developer settings;
- test API resources;
- account settings;
- restricted-business/application flow discovery.

DO NOT use random blogs where primary Stripe docs exist.

DO NOT expose secrets in chat/transcript.

DO NOT paste secret API keys into source files.

DO NOT commit secrets.

DO NOT store secrets in screenshots or Markdown.

DO NOT accept legal/commercial terms automatically.

STOP AND ASK THE USER before:

- accepting Stripe terms;
- submitting a restricted-business application;
- certifying legal statements;
- submitting a business description with legal implications;
- entering SSN;
- entering tax information;
- entering bank account information;
- approving a financial agreement;
- accepting fees/contracts;
- performing 2FA that requires user interaction;
- completing identity verification;
- enabling a production capability that moves real value.

You may prepare the page/form.

You may fill clearly factual non-sensitive fields when safe.

But before a legally binding or sensitive submission:

ASK.

========================================================================
SECTION 3 — DETERMINE THE ACTUAL STRIPE PRODUCT STACK
========================================================================

Do not assume one Stripe feature handles everything.

Determine, from current official docs and Dashboard, the exact Stripe products needed for:

A. buying Nodal Credits with fiat;
B. verifying payout recipients;
C. paying eligible users in USDC/other supported stablecoin;
D. sending that payout to an external self-custodial wallet;
E. supporting platform / marketplace-style recipient relationships if required.

Investigate the current applicability of:

- Stripe Payments;
- Checkout;
- Payment Element;
- PaymentIntents;
- Stripe Connect;
- Global Payouts;
- stablecoin payouts;
- crypto payouts;
- Stripe Identity;
- Stripe-hosted onboarding;
- connected accounts;
- recipient/customer models;
- Treasury only if actually relevant;
- Crypto Onramp only if actually relevant.

Do not add products merely because they exist.

Prefer the minimum architecture necessary.

For every feature document:

PRODUCT
API / DASHBOARD CAPABILITY
ACCOUNT ELIGIBILITY
REGION
STATUS
PRODUCTION AVAILABILITY
REQUIRES APPLICATION?
REQUIRES CONNECT?
REQUIRES KYC?
SUPPORTED ASSET
SUPPORTED NETWORK
CAN PAY EXTERNAL WALLET?
CAN HANDLE PLATFORM RECIPIENTS?
BLOCKER
SOURCE

Save this in:

docs/providers/STRIPE_CAPABILITY_MATRIX.md

========================================================================
SECTION 4 — STRIPE BUSINESS-MODEL COMPATIBILITY
========================================================================

This is critical.

The Nodal product includes:

- internal Credits;
- user-created Nodal-native assets;
- internal secondary trading;
- agents;
- AI/data/compute purchases;
- creator earnings;
- potentially speculative native-market gains;
- eventual crypto/stablecoin payout.

Check current Stripe restricted-business / financial-services / virtual-credit / stored-value / crypto rules.

Do NOT hide the product structure.

Do NOT recommend misclassification.

Create:

STRIPE_BUSINESS_MODEL_REVIEW.md

Separate these categories:

A. Nodal SaaS/AI/data payments
B. purchase of internal Credits
C. user-created native assets
D. internal secondary trading
E. creator service earnings
F. market-creator fees
G. trading proceeds
H. crypto payout
I. wallet payouts

For each:

LIKELY ORDINARY
REQUIRES STRIPE APPROVAL
UNCLEAR
NOT SUPPORTED
BLOCKED_EXTERNAL

Do not make legal conclusions that require counsel.

========================================================================
SECTION 5 — PAYMENT PROVIDER: STRIPE ONLY
========================================================================

Implement production-grade:

StripeCreditPurchaseProvider

behind the existing:

CreditPurchaseProvider

interface.

Do not destroy abstraction.

Stripe becomes V1 provider configuration, not architecture.

Required payment lifecycle:

CREATED
PAYMENT_METHOD_REQUIRED
AUTHENTICATION_REQUIRED
PROCESSING
AUTHORIZED if relevant
SUCCEEDED
REVERSIBLE
SETTLED
FAILED
CANCELED
REFUNDED
DISPUTED
CHARGEBACK
MANUAL_REVIEW

Map actual Stripe statuses into Nodal statuses.

Do not fabricate Stripe statuses.

Use official Stripe event types.

========================================================================
SECTION 6 — PAYMENT INTENT / CHECKOUT DESIGN
========================================================================

Choose Checkout vs Payment Element based on current existing frontend and security requirements.

Prefer:

- Stripe-hosted collection of card details;
- no raw PAN through Nodal;
- minimal PCI scope;
- strong SCA/3DS support;
- Stripe Radar integration.

Each Credit purchase must contain immutable metadata linking:

nodal_user_id
credit_purchase_id
pricing_version
currency
requested_credit_amount
environment

Never trust browser-provided Credit amount.

Server computes expected Credit issuance from immutable pricing policy.

Example:

client says:
"issue 9,999,999 Credits"

server must ignore that and derive Credits from signed server-side product/pricing policy.

========================================================================
SECTION 7 — CREDIT ISSUANCE
========================================================================

Stripe success is NOT equivalent to:

"mutate user.balance += credits"

Use existing double-entry system.

Required flow:

Payment succeeds
 ->
Stripe webhook persisted
 ->
event authenticated
 ->
CreditFunding entity state transition
 ->
financial/fraud policy
 ->
idempotent journal transaction
 ->
Credit ledger issuance
 ->
outbox
 ->
user balance reflected.

Every issuance has:

funding_id
stripe_payment_intent
amount_fiat
currency
credit_amount
credit_price_version
reversibility state
provenance
created_at
settlement status

========================================================================
SECTION 8 — CARD REVERSIBILITY / CHARGEBACK RISK
========================================================================

This matters enormously.

A user might:

1. buy $1,000 of Credits;
2. trade Credits;
3. transfer economic value through native markets;
4. request crypto payout;
5. dispute the original charge.

Build robust safeguards.

At minimum distinguish:

PURCHASED_PENDING
PURCHASED_REVERSIBLE
PURCHASED_SETTLED
PURCHASED_DISPUTED
PURCHASED_REVERSED

Do not assume card settlement means irreversible.

PayoutEligibilityEngine must consider funding provenance and reversibility.

Add:

payout_hold_until
funding_risk_state
dispute_exposure
negative_economic_balance
account_frozen

Do not implement arbitrary "7 days" unless policy says so.

Make hold duration policy configurable.

========================================================================
SECTION 9 — STRIPE WEBHOOK SECURITY
========================================================================

Production-grade webhook ingestion:

- verify Stripe signature;
- use official library;
- validate tolerance;
- persist event ID;
- reject duplicate event;
- retain raw canonical evidence safely;
- never trust event ordering;
- never assume delivery;
- support replay reconciliation;
- use transactional inbox;
- idempotent processing.

Handle at minimum relevant events for:

payment success
payment failure
refund
dispute
chargeback
identity/account capability
payout success/failure

ONLY if those event types actually apply to selected products.

========================================================================
SECTION 10 — STRIPE RECONCILIATION
========================================================================

Implement scheduled reconciliation.

For each Stripe-connected economic operation compare:

Nodal state
vs
Stripe authoritative state

States:

MATCHED
PENDING
MISMATCH
INVESTIGATING
RESOLVED

Do not repair by manual DB edits.

Provide audited admin resolution workflow.

========================================================================
SECTION 11 — PAYOUT ARCHITECTURE
========================================================================

Primary V1 target:

eligible Nodal economic value
 ->
payout request
 ->
financial verification
 ->
Stripe approved payout product
 ->
USDC or approved stablecoin
 ->
user-controlled external wallet.

Do NOT implement:

Credits -> SOL directly inside Nodal

as default.

Do NOT implement:

Credits -> BTC directly

unless Stripe actually supports it and capability is approved.

Prefer ONE payout asset initially.

Target:

USDC

ONLY if Stripe currently supports USDC for the selected payout product/account.

If official Stripe support differs:

use actual supported asset
or leave capability disabled.

========================================================================
SECTION 12 — CRYPTO PAYOUT PROVIDER
========================================================================

Implement:

StripeCryptoPayoutProvider

behind:

PayoutProvider.

Never bypass PayoutProvider abstraction.

Provider capability discovery must expose:

supports_payout
supports_stablecoin_payout
supported_assets[]
supported_networks[]
requires_connect
requires_recipient_account
requires_kyc
requires_tax_info
supports_external_wallet
minimum_amount
maximum_amount if known
availability

Do not hardcode unsupported claims.

========================================================================
SECTION 13 — CRYPTO PAYOUT STATES
========================================================================

Use explicit state machine:

DRAFT
ELIGIBILITY_CHECK
VERIFICATION_REQUIRED
VERIFICATION_PENDING
VERIFIED
RESERVING_VALUE
READY_FOR_SUBMISSION
SUBMITTED
PROVIDER_PENDING
SETTLED
FAILED
REJECTED
PAYOUT_STATUS_UNKNOWN
REVERSED
MANUAL_REVIEW

External payout submission must have stable idempotency.

If Stripe times out after receiving request:

DO NOT send again blindly.

Reconcile using Stripe object/reference.

========================================================================
SECTION 14 — EXACTLY-ONCE PAYOUT EFFECT
========================================================================

Required failure test:

1. payout eligible;
2. reserve Nodal value;
3. submit Stripe payout;
4. Stripe accepts;
5. Nodal crashes before local success commit;
6. Nodal restarts;
7. reconciliation discovers provider payout;
8. no second payout is created;
9. ledger reservation finalizes exactly once.

Implement test.

Do not merely document.

========================================================================
SECTION 15 — PAYOUT VALUE PROVENANCE
========================================================================

Never permit:

current_balance > 0
therefore withdrawable.

Use existing provenance.

At minimum:

PURCHASED
PROMOTIONAL
REFUND
CREATOR_EARNING
DATA_SALE_EARNING
AGENT_SERVICE_EARNING
MARKET_CREATOR_EARNING
MARKET_TRADING_PROCEEDS
COMPETITION_REWARD
ADMIN_ADJUSTMENT

Each has payout policy.

DEFAULT conservative:

PROMOTIONAL -> NO
ADMIN_ADJUSTMENT -> NO
PURCHASED -> NO unless explicitly approved
MARKET_TRADING_PROCEEDS -> NO unless explicitly approved
CREATOR_EARNING -> POLICY
DATA_SALE_EARNING -> POLICY
AGENT_SERVICE_EARNING -> POLICY
MARKET_CREATOR_EARNING -> POLICY

Do not change to YES because product wants it.

Require ProductionCapabilityGate evidence.

========================================================================
SECTION 16 — FINANCIAL KYC
========================================================================

Goal:

Nodal itself does not unnecessarily store SSNs, government IDs, or selfies.

Determine if selected Stripe payout architecture includes:

- Stripe-hosted onboarding;
- Stripe Connect verification;
- Stripe Identity;
- another Stripe-managed verification path.

Prefer provider-hosted verification.

Store only:

stripe_entity_id
verification_state
capability_state
requirements_due
requirements_pending
verified_at
provider_evidence_reference

Do not store raw identity documents unless unavoidable.

========================================================================
SECTION 17 — IDENTITY MODEL
========================================================================

Maintain separate concepts:

NodalIdentity
PayoutIdentity
HostedFinancialIdentity

Example:

NodalIdentity:
ACTIVE

PayoutIdentity:
NOT_STARTED

RealCapitalIdentity:
NOT_STARTED

This means user may use non-financial Nodal features without having payout activated.

Do not implement one field:

verified = true

for everything.

========================================================================
SECTION 18 — KYC AT EXIT TARGET
========================================================================

Desired UX:

User signs up
 ->
uses allowed Nodal features
 ->
later clicks Withdraw
 ->
Stripe verification starts
 ->
user completes required verification
 ->
Stripe capability becomes active
 ->
payout eligibility reevaluates
 ->
payout proceeds.

BUT:

If Stripe requires earlier verification for the exact product:

honor it.

Do not bypass.

Implement capability policy:

PRE_KYC_ACTIVITY_ALLOWED
KYC_BEFORE_NATIVE_MARKET
KYC_BEFORE_CONVERTIBLE_BALANCE
KYC_AT_PAYOUT

Only one/multiple enabled according to evidence.

========================================================================
SECTION 19 — STRIPE IDENTITY / CONNECT ONBOARDING UX
========================================================================

If Stripe provides hosted onboarding:

prefer redirect/embedded official Stripe UI.

If Stripe Identity is needed:

use official verification session architecture.

Never expose secret client keys.

Handle:

VERIFICATION_CREATED
USER_ACTION_REQUIRED
PROCESSING
VERIFIED
FAILED
MANUAL_REVIEW

Map only actual provider statuses.

========================================================================
SECTION 20 — WALLET/CUSTODY DESIGN
========================================================================

For V1 crypto payout:

Nodal is NOT the payout wallet custodian.

User supplies/connects a self-custodial wallet.

Primary UX:

Connect Phantom
or
Enter supported wallet address

Prefer actual wallet connection where possible.

A bare pasted address is higher risk.

Support:

wallet_id
user_id
chain
address
ownership_verified
verification_method
verified_at
created_at
last_changed_at
risk_state
status

========================================================================
SECTION 21 — PHANTOM / SOLANA WALLET CONNECTION
========================================================================

If existing frontend already supports Solana wallet adapters:

inspect and preserve correct code.

Otherwise implement standard self-custodial wallet connection.

User signs a non-transaction authentication message to prove address ownership.

Message must include:

Nodal domain
user/account identifier
nonce
wallet address
timestamp
expiration
environment
purpose:
"verify payout wallet ownership"

Server verifies signature.

Nonce:
single use.

Prevent replay.

Do not ask user to transfer crypto merely to verify ownership.

========================================================================
SECTION 22 — WALLET ADDRESS SECURITY
========================================================================

Wallet-address replacement is a major account-takeover risk.

Implement:

- recent-authentication requirement;
- passkey/2FA challenge if available;
- wallet signature proof;
- new-address notification;
- cooling period policy;
- payout hold after high-risk address change;
- IP/device risk event;
- immutable audit log;
- admin visibility.

Do NOT silently allow:

attacker logs in
changes wallet
withdraws immediately.

========================================================================
SECTION 23 — SUPPORTED PAYOUT NETWORKS
========================================================================

Do not assume Solana.

Use whatever Stripe's actual payout product supports.

If Stripe supports:

USDC on Base
but not Solana

then the system must accurately reflect that.

Do not make Phantom/Solana requirement override provider reality.

Phantom supports multiple chains, but do not assume every Stripe payout network is supported by Phantom without verifying.

Capability matrix:

PAYOUT_ASSET
PAYOUT_NETWORK
STRIPE_SUPPORTED
WALLET_SUPPORTED
NODAL_APPROVED

All four must pass.

========================================================================
SECTION 24 — NO CUSTODY
========================================================================

Nodal must never:

- request seed phrase;
- request private key;
- store private key;
- store wallet recovery phrase;
- claim to recover Phantom wallet;
- sign payout-wallet transactions;
- move funds after payout.

Once provider payout reaches user wallet:

user controls asset.

========================================================================
SECTION 25 — OPTIONAL SELF-CUSTODIAL REAL-CAPITAL RAIL
========================================================================

This workstream may preserve or finish:

Connect Phantom
 ->
read approved assets
 ->
agent produces FinancialIntent
 ->
user-approved Strategy IR
 ->
risk
 ->
transaction construction
 ->
independent transaction inspection
 ->
user signing / approved bounded authority
 ->
Solana.

This is separate from internal-Credit payout.

Do not merge the ledgers.

========================================================================
SECTION 26 — BROWSER SETUP: STRIPE TEST MODE
========================================================================

Using Claude in Chrome:

1. open official Stripe Dashboard;
2. confirm correct business/account;
3. switch to test/sandbox mode;
4. inspect enabled products;
5. inspect developer/API settings;
6. inspect Connect settings;
7. inspect stablecoin/crypto payout availability;
8. inspect Identity/onboarding availability;
9. inspect required applications;
10. document what is actually available.

Do not turn on production real-value capability yet.

========================================================================
SECTION 27 — STRIPE ACCOUNT SEPARATION
========================================================================

If the user currently uses Stripe for unrelated projects:

DO NOT silently reuse configuration.

Determine whether Stripe recommends:

- same legal business with separate account;
- separate Stripe account;
- separate business entity/account.

Do not create a new legal entity.

If separate Stripe account is appropriate and dashboard supports it:

prepare.

Ask user before any legally consequential creation/submission.

========================================================================
SECTION 28 — STRIPE BUSINESS DESCRIPTION
========================================================================

Never describe Nodal misleadingly as:

"AI SaaS"

if payment processing also funds internal Credits and user-created markets.

Draft a transparent business description for Stripe.

Before submission:

show it to user.

It should clearly describe:

- internal Credits;
- user-created digital/native assets;
- internal markets;
- no fiat cash-out;
- planned crypto/stablecoin payout;
- KYC at payout target;
- no Nodal custody of payout wallets;
- no leverage;
- no derivatives;
- no gambling claims;
- no hidden conversion scheme.

========================================================================
SECTION 29 — RESTRICTED BUSINESS REVIEW
========================================================================

If Stripe Dashboard requires:

financial-services approval
crypto approval
stored-value approval
marketplace approval
other review

do not bypass.

Record:

BLOCKED_EXTERNAL_STRIPE_APPROVAL

with:

exact form/page
required evidence
current status
impact
capabilities disabled

========================================================================
SECTION 30 — TEST DATA / TEST CARDS
========================================================================

Use official Stripe test data only.

Test:

successful card
declined card
3DS/authentication
insufficient funds scenario if available
refund
dispute simulation
webhook retry

Do not use real card in test environment.

========================================================================
SECTION 31 — SECRET MANAGEMENT
========================================================================

Secrets:

STRIPE_SECRET_KEY
STRIPE_WEBHOOK_SECRET
CONNECT credentials if any
IDENTITY configuration
other secret IDs

Production:

AWS Secrets Manager / existing secret store.

Local:

.env.local or existing approved local secret store
excluded from git.

Never log secrets.

========================================================================
SECTION 32 — ENVIRONMENT ISOLATION
========================================================================

Separate:

LOCAL
TEST
STRIPE_TEST
STAGING
PRODUCTION

Do not permit test Stripe object IDs in production.

Add startup assertion.

Do not permit production secret in tests.

========================================================================
SECTION 33 — DATABASE
========================================================================

Add only required provider fields.

Do not turn Stripe object into primary business identity.

Maintain internal IDs.

Example:

credit_purchase.id
stripe_payment_intent_id

payout.id
stripe_payout_reference

user.id
stripe_recipient/account_id

wallet.id
external_address

External IDs are unique and indexed.

========================================================================
SECTION 34 — MONEY PRECISION
========================================================================

Fiat:

integer minor units.

Credits:

integer minimum units.

USDC:

explicit 6-decimal base units if that network uses 6 decimals.

Do not use float.

Conversion quotes must be explicit.

========================================================================
SECTION 35 — PAYOUT QUOTE
========================================================================

Before crypto payout show:

eligible internal amount
conversion amount
provider fee
network fee if relevant
stablecoin amount
network
destination
quote expiration

Do not promise output without current provider quote where applicable.

========================================================================
SECTION 36 — USER CONFIRMATION
========================================================================

Before payout:

show exact destination.

Example:

Withdraw
1,000 payout-eligible units

Receive:
10.00 USDC

Network:
[actual supported network]

Wallet:
0x...1234 / Solana address

Fees:
...

Require explicit confirmation.

========================================================================
SECTION 37 — SANCTIONS / COMPLIANCE
========================================================================

Stripe/provider performs its required compliance.

Nodal still enforces provider capability state.

Do not send payout if:

verification missing
capability inactive
account restricted
provider requires information
wallet unsupported
jurisdiction blocked
fraud review active

========================================================================
SECTION 38 — LEGAL CAPABILITY GATES
========================================================================

Do not activate production just because Stripe integration works.

Required gates may include:

STRIPE_CREDIT_PURCHASE_APPROVED
STRIPE_CRYPTO_PAYOUT_APPROVED
STRIPE_INTERNAL_MARKET_MODEL_DISCLOSED
STRIPE_TRADING_PROCEEDS_PAYOUT_APPROVED
STRIPE_CREATOR_EARNINGS_PAYOUT_APPROVED
LEGAL_NATIVE_MARKET_PRE_KYC_APPROVED
LEGAL_CRYPTO_PAYOUT_MODEL_APPROVED
JURISDICTION_APPROVED

Missing gate:
FAIL CLOSED.

========================================================================
SECTION 39 — STRIPE CANNOT SOLVE NODAL'S LEGAL STATUS
========================================================================

Important:

Stripe approval does NOT automatically mean:

FinCEN exemption
California DFAL exemption
state MTL exemption
securities-law approval
adviser-law approval.

Keep separate:

ProviderApproval
LegalApproval

ProductionCapabilityGate requires both where necessary.

========================================================================
SECTION 40 — USER CREATED NATIVE MARKETS
========================================================================

Do not accidentally change native-market economics in this workstream.

But payment/payout policy must account for:

market-trading proceeds.

Every sale/trade flow retains:

Credit provenance
asset provenance
trade IDs
creator
counterparty
fees
funding ancestry

Enough to determine payout eligibility later.

========================================================================
SECTION 41 — FRAUD MODEL
========================================================================

Threats:

stolen card
chargeback after market trade
account farming
self-trading to transform purchased Credits into "earnings"
creator collusion
wash-trading for withdrawable provenance
fake marketplace service sales
wallet replacement
account takeover
payout laundering
multiple accounts
bonus abuse

Add detection signals.

Most important:

Provenance laundering must not transform:

PURCHASED_NONWITHDRAWABLE

into:

CREATOR_EARNING_WITHDRAWABLE

merely because two controlled accounts transact.

========================================================================
SECTION 42 — PROVENANCE TAINT / LINEAGE
========================================================================

Implement or strengthen economic lineage.

Example:

User A purchases 10,000 Credits.

A "buys" fake data from User B.

B now appears to have creator earnings.

This cannot automatically create clean withdrawable funds.

PayoutEligibility must support provenance lineage / risk review.

Do not create an easy laundering primitive.

========================================================================
SECTION 43 — ADMIN
========================================================================

Admin dashboard needs:

Stripe customer/entity
payment state
funding state
dispute state
payout state
verification requirements
wallet destination
wallet age
risk flags
provider status
reconciliation status

No raw sensitive identity documents.

========================================================================
SECTION 44 — SUPPORT TOOLS
========================================================================

Support-safe workflows:

payment missing
Credits missing
duplicate charge concern
refund request
dispute
payout pending
verification required
wallet incorrect
provider outage

No direct DB modifications.

========================================================================
SECTION 45 — OBSERVABILITY
========================================================================

Metrics:

stripe_payment_success
stripe_payment_failure
stripe_webhook_lag
duplicate_webhooks
credit_issuance_latency
payment_disputes
chargeback_exposure
payout_requested
payout_verified
payout_submitted
payout_settled
payout_failed
payout_unknown
wallet_change
wallet_verification_failure
provider_capability_disabled

========================================================================
SECTION 46 — SECURITY TESTS
========================================================================

Add tests for:

forged Stripe webhook
replayed Stripe webhook
modified amount
modified Credit quantity
duplicate PaymentIntent webhook
refund after issuance
chargeback after internal trade
payout of reversible Credits
payout of promotional Credits
wallet address swap
replayed wallet-signature nonce
wrong-wallet signature
unsupported chain
unsupported token
duplicate payout request
provider timeout after payout accepted
cross-user Stripe object access
admin bypass
capability gate bypass

========================================================================
SECTION 47 — INTEGRATION TESTS
========================================================================

Use Stripe test mode.

Test real provider flows where possible:

payment created
payment completed
webhook received
Credits issued
refund
dispute test if supported
identity/connected account sandbox
payout test object if Stripe permits
reconciliation

If a Stripe product has no usable test capability:

document external blocker.

========================================================================
SECTION 48 — END-TO-END TEST A
========================================================================

NEW USER:

sign up
 ->
buy sandbox Credits with Stripe test card
 ->
payment succeeds
 ->
Credit ledger receives exact amount
 ->
Credits display.

No duplicate issuance.

========================================================================
SECTION 49 — END-TO-END TEST B
========================================================================

REFUND:

buy Credits
 ->
refund payment
 ->
provider webhook
 ->
funding state changes
 ->
economic policy applies
 ->
ledger remains conserved.

========================================================================
SECTION 50 — END-TO-END TEST C
========================================================================

DISPUTE:

buy Credits
 ->
spend part
 ->
dispute occurs
 ->
account enters appropriate risk state
 ->
withdrawal blocked
 ->
no accounting corruption.

========================================================================
SECTION 51 — END-TO-END TEST D
========================================================================

PAYOUT:

user has payout-eligible sandbox value
 ->
click Withdraw
 ->
verification required
 ->
verification completed in test/sandbox if possible
 ->
wallet ownership verified
 ->
payout submitted
 ->
provider settles
 ->
internal reservation finalizes.

========================================================================
SECTION 52 — END-TO-END TEST E
========================================================================

CRASH:

provider accepts payout
 ->
Nodal process killed
 ->
restart
 ->
reconciliation discovers payout
 ->
no duplicate.

========================================================================
SECTION 53 — WALLET TEST
========================================================================

Connect Phantom/test wallet
 ->
sign ownership nonce
 ->
server verifies
 ->
wallet saved
 ->
change wallet
 ->
cooldown/risk policy activated.

========================================================================
SECTION 54 — USER EXPERIENCE
========================================================================

Desired entry:

Create Nodal account

No financial verification wall unless policy requires it.

User can see:

Agents
Research
Lab
Native Economy

When user clicks:

Withdraw

show:

"To withdraw crypto, verify your payout identity."

Then provider-managed verification.

========================================================================
SECTION 55 — FRONTEND FUNDING
========================================================================

Funding page:

Buy Credits

Amount selector

Payment through Stripe-hosted or secure Stripe UI.

Show:

Credits received
fees
refund/settlement explanation if applicable

Do not present Credits as bank cash.

========================================================================
SECTION 56 — FRONTEND PAYOUT
========================================================================

Payout page:

Eligible to withdraw
Not eligible
Pending
Verification required

Explain separately:

Total Credits
Payout-eligible value

Do not imply all Credits are redeemable.

========================================================================
SECTION 57 — STATUS UX
========================================================================

Display:

Payment processing
Credits pending
Credits available
Payout verification required
Payout under review
Payout processing
Payout completed
Payout failed

Do not leave user staring at spinner forever.

========================================================================
SECTION 58 — PROVIDER FAILURE
========================================================================

If Stripe unavailable:

Credit purchases:
temporarily unavailable.

Payouts:
temporarily unavailable / reconciliation pending.

Existing internal balances:
remain readable.

No destructive state changes.

========================================================================
SECTION 59 — STRIPE RATE LIMITING
========================================================================

Respect provider rate limits.

Use backoff.

Do not retry unsafe POST blindly.

Use idempotency.

========================================================================
SECTION 60 — TAX DATA
========================================================================

Do not invent tax treatment.

Preserve data required for future reporting.

If Stripe requests tax information for recipients:

allow provider to collect it.

Store status, not unnecessary sensitive data.

========================================================================
SECTION 61 — PRIVACY
========================================================================

Update data flow docs:

Nodal
Stripe
wallet provider/browser wallet

Document which entity receives:

name
email
payment data
identity data
wallet address
transaction data

Minimize duplication.

========================================================================
SECTION 62 — CSP / FRONTEND SECURITY
========================================================================

If Stripe JS / Identity iframe / Connect component requires CSP changes:

configure narrowly.

Do not use:

script-src *

Validate origin restrictions.

========================================================================
SECTION 63 — PRODUCTION READINESS
========================================================================

At end produce:

docs/providers/STRIPE_PRODUCTION_READINESS.md

with:

Payment integration
Payout integration
Identity integration
Wallet integration
Security
Reconciliation
Tests
Browser setup
Dashboard state
Provider approvals
Legal approvals
Remaining blockers

========================================================================
SECTION 64 — REQUIRED EXTERNAL BLOCKERS
========================================================================

If applicable, record exact blockers:

STRIPE_RESTRICTED_BUSINESS_APPROVAL
STRIPE_STABLECOIN_PAYOUT_ACCESS
STRIPE_CONNECT_APPROVAL
STRIPE_CRYPTO_PAYOUT_JURISDICTION
STRIPE_NATIVE_MARKET_DISCLOSURE
STRIPE_TRADING_PROCEEDS_PAYOUT
LEGAL_FINANCEN
LEGAL_CA_DFAL
LEGAL_STATE_MATRIX
EXTERNAL_PENTEST

Do not write:

"need Stripe approval"

Write:

what approval
where requested
current status
which feature it blocks
what evidence closes it

========================================================================
SECTION 65 — INDEPENDENT SECURITY AUDIT
========================================================================

Do NOT stop this integration work because external pentest is not done.

The external pentest is:

REQUIRED_BEFORE_PUBLIC_REAL_VALUE_LAUNCH

not:

REQUIRED_BEFORE_SOFTWARE_COMPLETION.

Prepare staging so an auditor can test:

auth
Credit ledger
payments
native markets
wallet ownership
payout eligibility
payout submission
admin
API
cloud/IAM

========================================================================
SECTION 66 — BROWSER DELIVERABLE
========================================================================

At the end report every browser action:

DONE
NEEDS USER
BLOCKED
NOT AVAILABLE

For example:

Stripe test account inspected — DONE
Webhook configured — DONE
Payment product configured — DONE
Connect enabled — NEEDS USER
Stablecoin payouts application — NEEDS USER
Restricted-business attestation — NEEDS USER
Production API keys — NOT YET
Identity test flow — DONE
Production bank data — NEEDS USER

========================================================================
SECTION 67 — DO NOT AUTO-SUBMIT
========================================================================

If you reach a screen containing:

"I certify"
"I agree"
"legal representative"
"beneficial owner"
"regulated activity"
"financial services"
"business description"
"SSN"
"tax ID"
"bank account"
"production activation"

STOP.

Explain exactly what is being asked.

Let the user approve/complete it.

========================================================================
SECTION 68 — DO NOT CIRCUMVENT A DENIAL
========================================================================

If Stripe says the product is unsupported:

DO NOT:

rename Credits
hide native markets
remove words only from application
use another unrelated Stripe account
misclassify transactions
route around review.

Record the blocker.

The architecture may later need another provider.

========================================================================
SECTION 69 — ACCEPTANCE CRITERIA
========================================================================

PAY-001
Stripe test payment creates exactly one funding record.

PAY-002
Exact Credit amount derives server-side.

PAY-003
Duplicate webhook creates no duplicate Credits.

PAY-004
Refund handled safely.

PAY-005
Dispute affects payout eligibility.

PAY-006
Chargeback cannot create free withdrawable value.

ID-001
Nodal account does not imply payout verification.

ID-002
Stripe verification state is provider authoritative.

ID-003
Raw government ID not stored by Nodal unnecessarily.

WAL-001
User proves wallet ownership cryptographically.

WAL-002
Signature replay rejected.

WAL-003
High-risk wallet replacement cannot immediately drain payout.

OUT-001
Only eligible provenance may payout.

OUT-002
Unverified payout rejected.

OUT-003
Unsupported network rejected.

OUT-004
Duplicate payout request cannot double pay.

OUT-005
Provider success + local crash reconciles.

OUT-006
Provider timeout enters unknown/reconciliation state.

LEG-001
Missing Stripe approval keeps capability disabled.

LEG-002
Missing legal approval keeps capability disabled.

SEC-001
Stripe webhook forgery rejected.

SEC-002
Secrets absent from repository.

SEC-003
Cross-user provider object access rejected.

========================================================================
SECTION 70 — CODE COMPLETION VS PROVIDER COMPLETION
========================================================================

At finish classify each:

SOFTWARE_COMPLETE
STRIPE_SANDBOX_COMPLETE
STRIPE_ACCOUNT_CONFIG_COMPLETE
STRIPE_PRODUCTION_APPROVED
LEGAL_APPROVED
PENTEST_COMPLETE
LIVE_READY

These must be separate.

It is acceptable to reach:

SOFTWARE_COMPLETE = TRUE
STRIPE_SANDBOX_COMPLETE = TRUE
STRIPE_PRODUCTION_APPROVED = FALSE

Do not lie.

========================================================================
SECTION 71 — COMMIT DISCIPLINE
========================================================================

Use coherent commits.

Suggested:

provider: add Stripe credit purchase adapter

payments: add Stripe webhook inbox and reconciliation

risk: enforce funding reversibility for payouts

payouts: add Stripe crypto payout adapter

identity: integrate Stripe payout verification

wallet: add self-custodial ownership verification

ui: implement funding and withdrawal flows

docs: record Stripe capability evidence

Do not make one giant unreviewable commit.

========================================================================
SECTION 72 — CONTEXT MANAGEMENT
========================================================================

Before context exhaustion:

update:

MASTER_BUILD_STATE.md
REQUIREMENTS_TRACEABILITY.md
BLOCKERS.md
DECISION_REGISTER.md
STRIPE_INTEGRATION_STATE.md
STRIPE_CAPABILITY_MATRIX.md
STRIPE_PRODUCTION_CHECKLIST.md

Record:

exact next action
exact tests
exact browser page/status
exact unresolved blocker.

========================================================================
FINAL MISSION
========================================================================

Begin now.

FIRST:

1. inspect the existing provider abstractions;
2. inspect the ledger / payout / wallet architecture;
3. inspect current Stripe code if any;
4. inspect current git state;
5. read build/audit state;
6. use Claude in Chrome to inspect the user's Stripe TEST/SANDBOX environment and official documentation;
7. determine the actual Stripe product stack currently available;
8. document capability evidence;
9. implement test-mode Stripe fiat-in;
10. implement hardened Credit issuance;
11. implement refund/dispute handling;
12. implement payout identity architecture;
13. implement Stripe crypto/stablecoin payout if current test APIs/account allow it;
14. implement external-wallet ownership verification;
15. integrate payout eligibility/provenance;
16. build end-to-end tests;
17. attack it;
18. fix defects;
19. configure every nonbinding browser setting you safely can;
20. stop at legally binding/sensitive submissions and ask the user.

DO NOT let browser setup distract from correctness.

DO NOT let code correctness substitute for provider approval.

DO NOT let provider approval substitute for legal approval.

DO NOT let legal uncertainty block writing safe, gated, testable software.

When finished, tell me exactly:

1. what Stripe products are actually required;
2. what the browser showed is available on my account;
3. what you configured;
4. what code you added/changed;
5. what tests passed;
6. what payout asset/network is actually supported;
7. whether Stripe can own the payout KYC;
8. whether Phantom/user-controlled wallet works as destination;
9. what still requires my action;
10. what still requires Stripe approval;
11. what still requires legal counsel;
12. what blocks live real-value launch.

Until then:

KEEP IMPLEMENTING.
KEEP VERIFYING.
KEEP ATTACKING.
KEEP RECONCILING.
DO NOT FAKE PRODUCTION READINESS.