# ADR-0018: No proprietary stablecoin or platform-issued dollar token

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

Goal PART 4 excludes from V1, verbatim: "proprietary stablecoin", "NodalUSD", and
"platform-issued redeemable dollar token", together with "company balance-sheet
credit", "treasury-fronted user settlement", and "instant cross-chain buying
power". PART 13 lists a custom stablecoin as premature. The failure modes of
issuing one are not hypothetical:

- Regulatory: issuing a redeemable dollar token makes the platform an issuer with
  e-money, money-transmission, and reserve obligations that are unresolved
  (BLOCKERS EB-009) and that dwarf the obligations of a non-custodial trading
  interface.
- Reserve and redemption risk: a run on redemptions, reserve mismanagement, or a
  reserve asset depeg becomes the platform's insolvency.
- Mint authority compromise: a stolen or misused mint key creates unbacked tokens;
  every customer balance is then a claim on nothing.
- Accounting entanglement: customer entitlements and platform liabilities are
  recorded in the same instrument; the "three truths" separation (PART 11)
  collapses because the platform's own token is both external and internal truth.
- Implicit credit: an internal token used to grant "instant" buying power before
  external settlement is credit creation, which PART 4 excludes.

## Decision

- The platform does not issue, mint, or redeem any stablecoin or dollar-denominated
  token, on-chain or off-chain, in V1.
- Customer dollar value is represented as ledger entitlements (ADR-0001) over
  third-party assets held in customer wallets, valued by the buying-power engine
  (ADR-0015) under the stablecoin risk policy (PART 26). An entitlement is never
  greater than the held external asset plus explicitly modelled pending funding.
- No treasury-fronted settlement and no balance-sheet credit: no code path grants
  spendable value that is not backed by a held or reconciled external asset.
- The asset registry (PART 32, 33) treats every stablecoin as an external asset with
  an issuer that is not the platform, a safety state, and a collateral factor. The
  registry has no concept of a platform-issued asset; the interface is generic so a
  regulated tokenized deposit could be listed later as an external asset like any
  other.
- No capability gate named for issuance exists in V1, so the gate system cannot be
  used to activate it without a new ADR and a new capability definition.

### Explicitly not decided / deferred

- Which third-party stablecoins are permitted, and their initial collateral factors
  (BLOCKERS EB-010; risk approval).
- Fiat off-ramp design; withdrawals remain gated `DISABLED` (PART 94).
- Any yield product on held stablecoins.
- Whether a partner-issued tokenized deposit is ever integrated. If it is, it is an
  external asset with its own risk state, not a platform token.

## Consequences

### Positive

- The platform's liabilities are entitlements to identifiable external assets,
  reconcilable on chain.
- The regulatory surface stays that of a trading interface, not an issuer.
- Stablecoin depeg risk is handled by policy (PART 26) rather than by the platform
  defending a peg.

### Negative

- No internally netted dollar rail; every dollar movement is an external asset
  movement with external fees and finality.
- "Instant" experiences are limited to what external finality permits; the UI must
  be honest about `SUBMITTED` versus `FINALIZED` (PART 45, 112).

### Operational

- Stablecoin status monitoring and policy versions are operational responsibilities
  (PART 26, 79).

## Alternatives considered

- Platform-issued token (the excluded item): rejected for the reasons in Context.
- Internal off-chain "USD balance" not backed one-to-one: this is fractional credit;
  rejected. An internal entitlement backed one-to-one by held assets is what the
  ledger already provides.
- Partner-bank tokenized deposits: deferred; would enter as an external asset.

## Related

- ADR-0001, ADR-0015, ADR-0016, ADR-0019.
- Goal PART 4, 11, 13, 20, 26, 27, 29, 32, 33, 45, 94, 112.
- BLOCKERS EB-002, EB-009, EB-010.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Asset registry test: no asset can be registered with the platform as issuer; the
  registry schema has no issuance or mint fields.
- Ledger invariant test (`TestProp_EntitlementBacked`): for every account and
  asset, entitlement is less than or equal to reconciled held quantity plus
  modelled pending funding; the property holds across randomized funding, trade,
  and reversal sequences.
- `test/security` search test: no code path references a mint authority, token
  issuance instruction, or redemption instruction owned by the platform.
- Full-balance reconciliation E2E (PART 163): ledger totals per asset equal
  on-chain totals across customer wallets within the reconciliation tolerance
  policy.
- Compliance review record in `docs/compliance-gates/PRODUCTION_GATES.md`
  confirming no issuance capability exists.
