# ADR-0020: Advanced financial capabilities, sandbox first

## Status

Accepted — implementation tracked in docs/build/ADVANCED_CAPABILITIES_MATRIX.md and
docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-10-07

## Context

The V1 goal excluded, by name: a proprietary stablecoin, margin, leverage, derivatives, equities, internal
order matching, customer-to-customer transfers, principal trading, hidden spread, performance fees, a
marketplace, arbitrary user code execution, direct agent signing, direct agent withdrawals, cross-chain
buying power and prediction markets. ADR-0011 (no arbitrary live code), ADR-0012 (no AI signing), ADR-0018
(no proprietary stablecoin) and ADR-0019 (no internal crossing) record four of those exclusions as safety
boundaries.

A later goal (`NODAL_MEGA_GOAL_ADVANCED_FINANCIAL_SYSTEMS`) changes the engineering target: implement the
underlying systems for every one of them, as real software with real state transitions and real tests,
while keeping live customer exposure disabled. The reasons are concrete:

- The exclusions were drawn around *legal activities*, not around *engineering problems*. A margin engine,
  a matching engine, a stable-asset reserve ledger, a delegated-authority model and a sandboxed code
  runtime are engineering problems whose correctness can be proven without a single real customer dollar.
- Each of them exercises the platform's existing controls harder than spot trading does: per-asset
  double-entry with segregated books, exactly-once effects with unknown-outcome reconciliation, deterministic
  risk with fail-closed inputs, capability gates, kill switches and evidence. Building them is how those
  controls get tested against the cases they were designed for.
- Every one of them has a legitimate *simulated* product shape (paper trading, sandbox issuance, devnet
  signing) that is useful on its own.

What does not change: none of these activities may be offered with real customer value until a licence or
exemption, a provider contract and the platform's activation ceremony exist for it.

## Decision

1. **Build every advanced capability, in simulated modes only.** The permitted modes are `PAPER`
   (simulated fills against a reference or an internal book), `SANDBOX` (a full workflow against a
   deterministic provider) and `DEVNET` (real Ed25519 signatures and finality states on a deterministic
   local simulated chain, `internal/devnet`). `LIVE` is not a mode any advanced capability implements.

2. **Make live unrepresentable, not merely disabled.** Every advanced capability posts only value in
   domain `SIMULATED`, which migration 00710 already forbids from ever moving to or from any other domain.
   Migration 00731 adds: a `SIMULATED` asset kind; a rule that every advanced journal kind may touch only
   `SIMULATED` accounts (SM001) and must balance per party (SM002); house and facility books that cannot
   hold anything but `SIMULATED` value; and an `economic_intents.mode` CHECK that does not contain `LIVE`.
   `gates.DecideMode` refuses `LIVE` for every advanced capability (`LIVE_NOT_IMPLEMENTED`) because
   `AdvancedSpec.LiveImplemented` is false for all of them. No gate state, configuration value, environment
   variable or operator can change that; only a code change plus a migration can.

3. **Gate even the simulations where they face users.** Each capability has its own gate (15 new ones,
   all high risk, plus the existing `PREDICTION_MARKETS`). In LOCAL, TEST and DEV the simulated modes run
   without a gate so a fresh deployment can demonstrate itself. In STAGING and PROD even a simulated mode
   needs the gate ACTIVE through the existing dual-control, evidence-backed ceremony, because exposing a
   paper product to the users of a deployed system is itself a product decision. A `CAPABILITY_DISABLE`
   kill switch stops any one capability instantly.

4. **Replace the forbidden ideas with their honest engineering equivalents.**
   - "Hidden spread" → a transparent spread and fee engine: every component is disclosed in the quote,
     booked to its own revenue account, and recomputable by an auditor from the stored disclosure.
   - "Direct agent signing" → delegated, policy-scoped, expiring, revocable capability tokens checked by
     the risk kernel, a transaction inspector and the signer again. Agents never read a key.
   - "Direct agent withdrawals" → agent *proposals* that a policy or a person approves; the withdrawal is
     carried out by a service. The settlement compiler still forbids an agent to withdraw or transfer
     value at every authority level.
   - "Arbitrary user code execution" → a WASM sandbox with no filesystem, no network, no environment, CPU,
     memory, wall-time and output limits, a deterministic clock and random source, and a single output
     channel whose only effect is a typed intent proposal.

5. **One architecture, not fifteen.** Every capability reuses the existing ledger (extended chart, new
   owner types), capital reservations, settlement compiler (16 new `SIMULATED_*` actions), risk kernel
   (V2 composable checks beside V1), gates, kill switches, audit, outbox and evidence archive, through the
   shared kernel in `internal/econ`. `docs/architecture/ADVANCED_FINANCIAL_MODEL.md` is the contract.

This ADR amends ADR-0011, ADR-0012, ADR-0018 and ADR-0019 **for simulated modes only**. Their decisions
stand unchanged for real value: no arbitrary code in any live path, no AI signing of real transactions, no
platform-issued real dollar token, no internal crossing of real customer orders and no principal trading
against real customers.

## Consequences

### Positive

- The platform's financial controls are exercised against margin calls, liquidations, unknown bridge
  outcomes, reserve shortfalls, payout conservation and capability escalation, with tests that prove the
  system refuses unsafe states.
- Activation later is a bounded, reviewable change: implement the live adapter, flip `LiveImplemented`,
  lift the SIMULATED-only constraint for that one book in a migration, and run the gate ceremony with the
  evidence the spec lists.

### Negative

- The chart of accounts grows from 20 to 67 codes and the journal kinds from 19 to 50. Each new SQL list is
  compared with its Go twin by a parity test, which is maintenance.
- Simulated products can be mistaken for real ones by users. Every surface must say "simulated" and the
  honesty tests of the web application enforce the vocabulary.

### Operational

- Operators gain a kill switch per capability (`CAPABILITY_DISABLE`) and six new reconciliation kinds.
- The evidence archive receives one document per advanced economic effect.

## Alternatives considered

- **Keep the exclusions.** Rejected: it leaves the controls untested against the cases they exist for,
  and the goal explicitly asks for the systems.
- **A separate sandbox ledger.** Rejected: a second ledger would have to re-implement balance, immutability,
  idempotency and isolation, and would prove nothing about the real one. The `SIMULATED` domain gives the
  same isolation inside the real ledger, enforced by the same triggers.
- **Enable live behind configuration.** Rejected outright: one environment variable must never be able to
  activate live capital, and for these capabilities no combination of configuration can.

## Related

ADR-0001, ADR-0010, ADR-0011, ADR-0012, ADR-0013, ADR-0014, ADR-0015, ADR-0016, ADR-0018, ADR-0019.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- `internal/econ` integration tests: a LIVE intent cannot be written; a simulated-only kind cannot touch
  real capital (SM001); a transfer must balance per party (SM002); house and facility books cannot hold
  real value; effects are exactly once under concurrency.
- `internal/gates`: `TestProp_LiveIsNeverAllowed` (in `internal/econ`), `DecideMode` table tests.
- `internal/settlement`: `TestCompile_AnAgentCanNeverTransferOrWithdrawEvenInSimulation`.
- Each capability's own conservation and refusal tests, listed in docs/build/ADVANCED_TEST_EVIDENCE.md.
