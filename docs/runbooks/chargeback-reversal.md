# Runbook: chargeback / funding reversal

Severity: SEV2 (operational alert, PART 27); SEV1 if reversals cluster (fraud ring) or the aggregate deficit exceeds policy · Owner: COMPLIANCE (account), FINANCE (postings) · Related: [funding-provider-compromise.md](./funding-provider-compromise.md), [ledger-mismatch.md](./ledger-mismatch.md), `docs/architecture/FINANCIAL_MODEL.md` §2.2

## Trigger

- Security events written by `funding.Service.Reverse` (`internal/funding/service.go`): `funding_reversed` (HIGH) for every reversal; `negative_deficit_accounts` (CRITICAL) when the customer no longer holds the reversed quantity.
- `negative_deficit_accounts` gauge > 0 (PART 132).
- Provider signal: a Stripe onramp session event indicating dispute/reversal (`crypto.onramp_session.updated`; `docs/api/providers/stripe-crypto-onramp.md`; note Stripe is merchant of record and "assumes full liability for all fraud and disputes"; on-chain delivery is irreversible, so a reversal is a fiat-side event that the platform must mirror in the ledger).
- `deposits.fraud_state` moving to `REVIEW`/`CONFIRMED_FRAUD`; a deposit reversed after `reversible_until`.
- BLOCKED_EXTERNAL: alert routing for these security events and the gauge (`infra/terraform/modules/observability`).

## Blast radius

- The affected account: `Reverse` posts the compensating transactions and, on a shortfall, sets the account `FROZEN` (PART 27: "freeze or restrict account if deficit occurs; prohibit new risky activity"). New risk for that account is refused by the account status; withdrawals were never eligible (`withdrawal_eligible` only after `reversible_until` with `fraud_state = CLEARED|NONE`).
- Platform: the shortfall is an explicit `DEFICIT` credit-normal balance (customer owes the platform); no ledger account goes negative (`LEDGER_NEGATIVE_BALANCE` trigger).
- Keeps running: everything. A reversal is `LEDGER_POST`/`SETTLE` class work and is never blocked by any switch.

## Immediate actions (first 10 minutes)

1. Read what the service already did (read-only, `cp_readonly`):
   ```sql
   SELECT id, account_id, provider, status, fiat_amount_minor, expected_quantity, observed_quantity, fraud_state,
          reversible_until, journal_transaction_id, reversal_journal_transaction_id, reversed_at
     FROM deposits WHERE status = 'REVERSED' AND reversed_at > now - interval '24 hours' ORDER BY reversed_at DESC;
   SELECT id, kind, reference_id, reason_code, posted_at FROM journal_transactions
    WHERE kind IN ('FUNDING_REVERSAL','FUNDING_REVERSAL_DEFICIT') AND posted_at > now - interval '24 hours';
   SELECT kind, severity, account_id, detail, occurred_at FROM security_events
    WHERE kind IN ('funding_reversed','negative_deficit_accounts') AND occurred_at > now - interval '24 hours';
   SELECT id, status, status_reason, frozen_at FROM accounts WHERE id IN (<account ids>);
   ```
2. Verify the invariant from FINANCIAL_MODEL.md §2.2: for a deficit case there are **two** balanced transactions in one database transaction, `T1 FUNDING_REVERSAL (Dr CAPITAL / Cr WALLET for what the customer still held)` and `T2 FUNDING_REVERSAL_DEFICIT (Dr CAPITAL / Cr DEFICIT for the shortfall)`, and `accounts.status = 'FROZEN'`. If the account is not frozen while a `FUNDING_REVERSAL_DEFICIT` exists, freeze it now: `POST /admin/accounts/{accountId}/status {"to":"FROZEN","reason":"<INC-id>: reversal deficit"}` (COMPLIANCE, `account:freeze`) and open a SEV1 defect.
3. Check for open orders and in-flight attempts on the account; they continue to finality and post normally (the freeze stops *new* risk). `SELECT id, status FROM orders WHERE account_id = '<id>' AND terminal_at IS NULL;`.
4. Look for a pattern: more than N reversals in the window, shared funding source, same device/IP in `security_events`/`sessions` ⇒ fraud ring ⇒ SEV1 and `FUNDING_DISABLE(*)` (SEVERE; `kill:activate`) while COMPLIANCE reviews; consider [funding-provider-compromise.md](./funding-provider-compromise.md) if the provider's own events look forged.
5. Announce the accounts, deficit totals and switch state.

## Diagnosis

- Timeline: `deposit_transitions` for the deposit (session → provider confirmed → settlement observed → reconciled → available → reversed) with `provider_events` (`provider_event_id`, `signature_verified = true`, `raw_ref`) for each provider status; the reversal event must be a verified webhook or a provider dashboard-confirmed dispute, never an unsigned notice.
- Chain side: the USDC delivered on chain does not come back; the ledger reversal is the platform absorbing or recovering the fiat-side loss per the provider terms (EB-004 records the allocation of fraud/dispute responsibility; do not assume).
- Was trading allowed before `reversible_until`? Policy decision (`buying_power_eligible` under `availability_policy_version`); if yes, the deficit is expected policy exposure, not a defect.
- Deficit recovery options are business decisions: customer repayment, provider dispute outcome, platform write-off. Each is a separate compensating posting under `LEDGER_CORRECTION`.

## Containment and recovery

1. COMPLIANCE keeps the account `FROZEN` (and `fraud_state` per review) until the deficit is settled or written off. Fraud confirmation: `CONFIRMED_FRAUD` and account `CLOSED` follow the compliance procedure; `REVIEW_REQUIRED` deposits are held.
2. Deficit settlement is a compensating journal transaction, dual-controlled, never a balance edit:
   - FINANCE proposes `POST /admin/actions {"kind":"LEDGER_CORRECTION","target_type":"account","target_id":"<account_id>","params":{"reason_code":"DEFICIT_RECOVERY|DEFICIT_WRITE_OFF","entries":[...]},"reason":"<INC-id>:..."}` (`ledger:post_correction`, step-up ≤ 5 min; expires 4 h);
   - a break-glass `ledger:approve_correction` holder approves `POST /admin/actions/{id}/approve`;
   - execution posts `kind = COMPENSATION` with `reason_code`, clearing `DEFICIT` against `PLATFORM_ADJUSTMENT` (write-off) or the new funding (recovery). `admin.Actions` and the ledger poster are wired behind it.
3. Unfreeze only after `DEFICIT` for the account is zero: `ACCOUNT_UNFREEZE` admin action (single COMPLIANCE operator with `account:freeze`, step-up, expires 1 h), then `POST /admin/accounts/{accountId}/status {"to":"ACTIVE",...}`. If an `ACCOUNT_FREEZE` kill switch was also activated, release it (`kill:release` + step-up).
4. If `FUNDING_DISABLE` was activated: release requires the dual-controlled `KILL_SWITCH_RELEASE` path ([global-kill-and-reenable.md](./global-kill-and-reenable.md), same steps with `kind = FUNDING_DISABLE`).
5. If the reversal was received while the deposit was still below `AVAILABLE`, no ledger posting existed; the deposit simply moves to `REVERSED`/`FAILED` and nothing is owed — verify no `FUNDING_SETTLED` posting exists for it.

## What NOT to do

- Never reverse by editing `ledger_balances` or deleting the `FUNDING_SETTLED` transaction (immutable; `LG003`).
- Never post a single `Dr CAPITAL / Cr WALLET` for the full amount when the customer holds less; the trigger rejects it and the deficit must be explicit (PART 27: "never silently make ledger negative without an explicit deficit account classification").
- Never unfreeze to let the customer "trade their way out" of a deficit.
- Never act on an unverified reversal notice; `provider_events.signature_verified` must be true or the dispute confirmed in the provider dashboard.
- Never make withdrawals eligible before `reversible_until` regardless of the trading policy (the WITHDRAWALS gate is DISABLED in V1 anyway).

## Verification / exit criteria

- For every reversed deposit: `status = REVERSED`, `reversal_journal_transaction_id` set, transactions balanced per asset, account frozen iff a `FUNDING_REVERSAL_DEFICIT` exists and the deficit is unsettled.
- `negative_deficit_accounts` gauge reflects the true count; `ledger.VerifyBalances` zero drift.
- Security events `funding_reversed` / `negative_deficit_accounts` present with `account_id`, `request_id` (correlation) and detail hashes.
- Compliance disposition recorded for each account; deficit settled or written off via an executed `LEDGER_CORRECTION` (or explicitly carried with a review date).

## Post-incident

- Archive the deposit transitions, provider events (`raw_ref`), both journal transactions, security events and the compliance disposition.
- Review whether `reversible_until` and the buying-power availability policy should tighten (RISK/COMPLIANCE decision, versioned in policy tables; never an env var).
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-027-*, R-162-1; if `TestFunding_ReversalCreatesDeficitAndFreezes` is still absent, add a BLOCKERS entry (THREAT_MODEL.md F4).
