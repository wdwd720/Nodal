-- +goose Up
-- A money column is not the application's to rewrite.
--
-- Every audited entity in this schema binds its STATE change to a transition
-- row (00603, 00726, 00731, 00732). None of that says anything about the other
-- columns, and the binding cannot: a constraint trigger declared
-- `AFTER UPDATE OF status` fires only when `status` appears in the statement's
-- SET list. So
--
--     UPDATE withdrawals SET destination_address = '<attacker>' WHERE id = $1;
--
-- fires nothing at all -- no transition row, no audit event, no refusal -- and
--
--     INSERT INTO withdrawal_transitions (...,'APPROVED','SUBMITTED',...);
--     UPDATE withdrawals SET status = 'SUBMITTED',
--            destination_address = '<attacker>', quantity = quantity * 10
--      WHERE id = $1;
--
-- commits cleanly: AU001 is satisfied, the edge is legal, and the trail records
-- a lawful state move while the payload was rewritten underneath it. Confirmed
-- against the live catalogue: cp_app held table-wide UPDATE on all eighteen
-- columns of `withdrawals`, all twenty-three of `payout_requests`, all
-- seventeen of `assets` and all fifteen of `instruments` (F-109).
--
-- The remedy is privilege rather than detection, which is why it works, and it
-- is the treatment this schema already applies five times: 00604
-- (ledger_accounts), 00701 (capability_gates), 00719 (provider_events), 00720
-- (position_lots), 00723 (admin_actions), 00730 (provider_evidence). F-42 has
-- recorded since the beginning that the remaining tables want the same thing;
-- these are the four where the columns are money, a destination, or the
-- definition of what counts as money.
--
-- `assets` is the sharpest of them. `cp_app` could write:
--
--     UPDATE assets SET risk_class = 'SETTLEMENT', is_stablecoin = true,
--                       peg_currency = 'USD', decimals = 0 WHERE symbol = 'SOL';
--
-- and `internal/reconciliation` marks any `is_stablecoin AND peg_currency='USD'`
-- asset at face value, scaled by that same mutable `decimals`, with no status or
-- risk-class check -- which is the materiality test deciding whether a
-- reconciliation break is worth a human.
--
-- The granted columns are exactly the ones the application writes today, read
-- from its UPDATE statements one at a time:
--
--   withdrawals      status, step_up_verified_at   (repository.go Transition)
--   assets           status                        (asset.go Transition)
--   instruments      status                        (instruments.go Transition)
--   payout_requests  the fourteen listed below     (service.go, nine statements)
--
-- Everything else -- an account id, a requested quantity, a destination, an
-- idempotency key, a mint address, a settlement asset -- is written once at
-- INSERT and is not the application's to change afterwards. If one of them ever
-- legitimately needs to move, that is a migration and a decision, which is the
-- point.
--
-- No trigger or default is affected: PostgreSQL checks column privileges
-- against the columns named in the statement, so `set_updated_at` assigning
-- NEW.updated_at needs no grant.

REVOKE UPDATE ON withdrawals FROM cp_app;
GRANT UPDATE (status, step_up_verified_at) ON withdrawals TO cp_app;

REVOKE UPDATE ON assets FROM cp_app;
GRANT UPDATE (status) ON assets TO cp_app;

REVOKE UPDATE ON instruments FROM cp_app;
GRANT UPDATE (status) ON instruments TO cp_app;

REVOKE UPDATE ON payout_requests FROM cp_app;
GRANT UPDATE (
    state,
    verification_level, policy_version, policy_hash,
    reserved_quantity, reserved_at,
    provider, provider_idempotency_key, submitted_at,
    provider_reference, provider_status,
    settled_quantity, settled_at,
    failure_reason
) ON payout_requests TO cp_app;

COMMENT ON TABLE withdrawals IS
    'Money out on a chain rail. cp_app may move the status and stamp the step-up. The amount, the destination and the approval are written once and are not the application''s to change (F-109).';
COMMENT ON TABLE assets IS
    'The asset registry. cp_app may move the status. What an asset IS, meaning its risk class, its peg, its decimals and its mint, is a migration or an operator decision, because reconciliation values stablecoins at face value from these columns (F-109).';

-- +goose Down
SELECT 1; -- protected: reverting returns the application role table-wide UPDATE on four tables whose columns are money
