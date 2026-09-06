# ADR-0012: No AI, agent, or strategy may sign; bounded signing behind an isolated service

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

Goal PART 9 states the rule: an agent is an untrusted proposal generator, never a
financial authority, and "even total model compromise must not permit arbitrary
value transfer". The threats that make this a structural requirement rather than a
policy:

- Model compromise: a poisoned or backdoored model, or a compromised model
  provider, emits instructions designed to move funds. If the model's process can
  reach a key, the funds move.
- Prompt injection through data: token metadata, token descriptions, social posts,
  news, and on-chain memo fields are read by strategies and models. Any of them can
  contain "transfer everything to X". PART 67 requires that such content is data,
  never instruction; but the only reliable enforcement is that the process reading
  it cannot sign.
- Tool-result poisoning: a read tool returning crafted content that steers the
  model into constructing a malicious intent.
- Upstream route provider compromise: the execution router (ADR-0008) returns a
  transaction with extra instructions (delegate approval, authority change, SOL
  transfer, unknown program). Blind signing turns the provider's breach into the
  platform's loss (PART 34).
- Compromised agent-worker host or stolen agent credentials: an attacker with the
  agent's privileges must still be unable to sign, withdraw, change risk, or enable
  capabilities.
- Replay: a legitimately approved signing request reused for a second transaction.
- Token-2022 extensions altering balance semantics of a transaction that otherwise
  matches the plan.

## Decision

- Agents, strategies, models, and the tool broker never hold wallet private keys,
  wallet API secrets, signing tokens, or KMS signing permissions. They cannot call
  `WalletProvider` or `SigningProvider` (PART 95, 96). Agent-facing packages cannot
  import `signing` or `wallet`; a build-time test enforces this (ADR-0003).
- Signing is performed only by an isolated signing service: its own binary, task
  role, secret set, and database role; reached from `execution-worker` over gRPC
  (DECISION_REGISTER D-003). No other binary can reach it.
- A signing request is bounded: it references an approved intent, an approved
  execution plan (by hash), an approved risk decision (by ID and policy hash), an
  approved wallet, and the expected transaction. The signing service independently
  re-validates every PART 34 constraint: expected wallet and fee payer, token inputs
  and maximum debit, expected output token and minimum output, allowed programs and
  token programs, no authority or ownership change, no unknown system transfers, no
  unexpected destination or delegate, blockhash expiry, plan and quote identity, and
  slippage bounds. Any mismatch is a rejection with a typed reason.
- Unsupported token extensions (Token-2022 and specialized extensions) are rejected
  by default; support is added per extension after its semantics are understood.
- Each approval is single-use and bound to one transaction hash; reuse is rejected.
- Withdrawals are never reachable through agent capability (PART 94); the withdrawal
  domain is separate and requires human step-up authorization.
- Even under total compromise of the agent tier, the worst outcome is a stream of
  intents that must still pass eligibility, the risk kernel (ADR-0013), reservation
  within the capital envelope, the settlement compiler (ADR-0014), inspection, and
  bounded signing. The envelope's limits bound the loss.
- Production startup fails closed if the wallet provider's signing semantics are
  unverified (PART 96).

### Explicitly not decided / deferred

- The wallet provider and its delegated-signing model (BLOCKERS EB-003, EB-004).
- Whether the signing service runs in a separate account or VPC.
- HSM, Nitro Enclaves, or MPC custody. PART 13 defers bespoke MPC and enclaves;
  the interface leaves room for either behind `SigningProvider`.

## Consequences

### Positive

- Model quality is a product concern, not a custody concern.
- Provider compromise is bounded to rejected transactions.
- A single, auditable place enforces transaction constraints.

### Negative

- Autonomy is limited to what the intent path allows; there is no "just sign this".
- Inspection rules must be maintained as Solana programs and token standards evolve.

### Operational

- Signing-service rejections are security events (PART 130) with alerting.
- Key rotation and provider credential rotation are runbook items (PART 157).

## Alternatives considered

- Scoped keys for agents with on-key policy: the key remains exfiltratable and the
  policy is enforced by the same compromised process. Rejected.
- Custom MPC or threshold signing built in-house: bespoke cryptography, excluded by
  PART 13.
- Program allowlisting without full instruction inspection: value can still be
  transferred inside allowed programs. Insufficient alone.
- Human approval on every trade: retained as an option via capability gates and
  step-up for high-risk classes; not a substitute for the structural boundary.

## Related

- ADR-0003, ADR-0008, ADR-0009, ADR-0011, ADR-0013, ADR-0014, ADR-0016.
- Goal PART 4, 9, 10, 34, 44, 63, 66, 67, 94, 95, 96, 100, 130, 152, 155, 156,
  221.
- DECISION_REGISTER D-003; BLOCKERS EB-003, EB-004.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Import-boundary test (ADR-0003) covering `signing` and `wallet`.
- Transaction inspector fuzz target (PART 152) with a corpus of malicious delegate,
  authority update, extra transfer, unknown program, unexpected token account,
  malformed instruction, and unsupported extension cases; all rejected.
- Tests that a signing request with a missing or mismatched plan hash, risk
  decision, quote identity, wallet, or transaction bytes is rejected.
- Replay test: the same approval presented twice signs at most once.
- Prompt-injection test corpus (PART 155): injected content in token metadata and
  social data produces no intent outside the effect set and never reaches signing.
- IAM review record: the agent-worker role has no KMS signing permission and no
  access to wallet secrets; the signing service is reachable only from the
  execution worker.
- PROD startup test: unverified wallet provider capability fails closed.
- Evidence-matrix entry for "Signing" per PART 239.
