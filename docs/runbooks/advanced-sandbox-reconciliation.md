# Runbook: advanced sandbox books stop reconciling

Written 2026-10-08 against the working tree of the same date. Applies to the advanced capabilities of
ADR-0020 (stable asset, bridge, margin, derivatives, prediction markets, order books, delegated authority).
Every value they move is in value domain `SIMULATED`; the database refuses to mix it with real capital
(VD002). These alarms are paged anyway: a sandbox whose books stop reconciling is a defect in the controls
that would one day guard real value.

## Trigger

- `<prefix>-advanced-reserve-mismatches` (SEV1): `advanced_reserve_mismatches` ≥ 1 — a stable asset's supply
  reconciliation (ledger outstanding vs every holding, `ledger.HoldingCodes`, vs the chain supply, vs the
  eligible reserve) does not match.
- `<prefix>-advanced-unmatched-effects` (SEV2): `advanced_unmatched_economic_effects` ≥ 1 for 15 minutes — an
  economic effect stayed `UNKNOWN` or unmatched (a chain submission whose outcome is not known).
- `<prefix>-advanced-chain-mismatches` (SEV2): `advanced_chain_mismatches` ≥ 1 — a bridge route's or stable
  asset's ledger disagrees with a simulated chain.

The metrics come from `cmd/advanced-worker` (job `metrics.advanced`), which reads them from the database
(`internal/advancedmetrics`). Alarm definitions: `infra/terraform/modules/observability/advanced.tf`.

## Blast radius

Simulated value only. The controls already contain the event before anyone reads this: a stable asset whose
reconciliation mismatches is paused by its circuit breaker (no new issuance); a bridge route whose books
disagree with a chain is paused; an account with an `UNKNOWN` effect takes no new risk
(`RISK_SETTLEMENT_UNKNOWN`) until reconciliation resolves it. Settlement, liquidation, expiry and
reconciliation keep running (kill switches never stop them).

## Immediate actions (first 10 minutes)

1. Open the operator console overview: `GET /v1/advanced/ops/overview` (or the web operator console). It lists
   open reconciliation records by kind, stable assets and bridge routes with their status, and chain tips.
2. Confirm the containment held: the affected stable asset shows `PAUSED`, the affected route is paused, and
   new risk on an account with an unknown effect is refused. If any of these did not happen, treat it as a
   control failure and escalate to SEV1.
3. Do nothing to the chain or the ledger by hand. No manual posting, no resubmission.

## Diagnosis

- **Reserve mismatch:** run a supply reconciliation of the asset now —
  `POST /v1/advanced/stable-assets/{stableAssetId}/reconciliations` — and read its evidence: ledger
  outstanding, customer wallets, held elsewhere, chain supply, landed-but-unposted mints and burns, coverage.
  A difference equal to an in-flight mint or burn that has since landed is resolved by the next pass. A
  difference nothing explains is an out-of-band mint or burn (threat 1) — keep the asset paused.
- **Unmatched effects:** `advanced-worker once stable.unknown_submissions` (stable mints and burns),
  `advanced-worker once bridge.legs` (bridge locks and releases), `advanced-worker once delegation.upkeep`
  (delegated swaps and withdrawals) resolve unknown submissions by observation — never by resubmitting. A
  transaction still in the mempool waits; one whose blockhash expired below finality fails and releases its
  funds; one that landed is posted exactly once.
- **Chain mismatch:** `POST /v1/advanced/bridge/routes/{bridgeRouteId}/reconciliations` compares the route's
  locked and outstanding books with both chains (`GET /v1/advanced/ops/chains` shows the tips).

## Containment and recovery

- A stable asset resumes only by a person's decision, after a reconciliation matches:
  `POST /v1/advanced/stable-assets/{stableAssetId}/resume` (step-up).
- A bridge route resumes the same way: `POST /v1/advanced/bridge/routes/{bridgeRouteId}/resume` (step-up).
  A leg stuck in manual review is resumed, resolved or refunded through the operator routes under
  `/v1/advanced/bridge/legs/{bridgeLegId}/` (a refund is refused if the destination released).

## What NOT to do

- Do not resubmit a transaction whose outcome is unknown. Reconciliation observes; it never retries.
- Do not post a ledger correction to make a reconciliation match. Find the transaction that explains it.
- Do not resume an asset or a route before a reconciliation matches.

## Verification / exit criteria

- The alarm clears, the reconciliation record of the scope is `MATCHED`, and the affected asset or route is
  `ACTIVE` again by an operator's decision.
- `make advanced-demo` Scenarios A, G and I pass against a fresh database (they exercise exactly these paths).

## Post-incident

Record the cause and the transaction that explained the mismatch. If no transaction explained it, the
mismatch is a finding against the subsystem's controls: file it with the reconciliation record's evidence.
