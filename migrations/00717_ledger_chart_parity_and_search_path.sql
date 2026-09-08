-- +goose Up
-- Three things the database said it did and did not (F-48, F-49, F-50).

-- ---------------------------------------------------------------------------
-- F-47 was HERE and has been taken out again.
--
-- It revoked SELECT on identity_pii and sessions from cp_readonly and cp_ops,
-- on the reasoning that migration 00010's grant list deliberately withholds
-- both and a blanket ALTER DEFAULT PRIVILEGES was overriding it.
--
-- test/integration/migrations/privileges_test.go states the opposite contract,
-- in a doc comment and in assertions: "cp_readonly and cp_ops can SELECT
-- everything and write nothing". It failed on this change, which is the suite
-- working. And cp_ops is listed as performing retention cleanup on `sessions`,
-- which a DELETE ... WHERE cannot do without SELECT on the columns it filters
-- on -- so the revoke would have broken a documented operational job.
--
-- Two deliberate statements in this repository contradict each other and one of
-- them is wrong. Which one is a policy decision about who may read encrypted
-- PII, not a decision to take unilaterally inside a migration. Recorded as
-- F-47, OPEN, with both statements and the operational constraint.

-- ---------------------------------------------------------------------------
-- F-48. Five SECURITY DEFINER functions did not pin pg_temp.
--
-- Migration 00701 names this hazard exactly and pins `pg_catalog, public,
-- pg_temp` for cp_gate_transition. The five below were written with
-- `SET search_path = public` and were never updated. TEMP on a database is
-- granted to PUBLIC by default and is revoked nowhere in this tree, so any
-- caller may create a temp table; with pg_temp unpinned it is searched first,
-- and a temp relation shadowing ledger_accounts would be read by
-- ledger_apply_entry as SECURITY DEFINER.
--
-- ALTER FUNCTION ... SET search_path replaces the setting without touching the
-- body, so this is the whole fix and none of the logic moves.
ALTER FUNCTION ledger_apply_entry()        SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION cp_credit_lot_open()        SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION cp_credit_lot_apply_event() SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION cp_native_market_open()     SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION cp_native_market_apply_fill() SET search_path = pg_catalog, public, pg_temp;

-- ---------------------------------------------------------------------------
-- F-49. ledger_accounts.allow_negative was chosen by whoever inserted the row.
--
-- LG001 is the negative-balance guard, and it asks the ACCOUNT:
--
--     IF newbal < 0 AND NOT acct.allow_negative THEN RAISE ... LG001
--
-- The column is `NOT NULL DEFAULT false` and cp_app holds INSERT on the table,
-- so a caller could create an account with allow_negative = true and post it
-- below zero for ever after. 00604 restricts UPDATE to (status) and says the
-- app "must never change an account's negative-balance policy" -- true, and it
-- was setting it at creation instead.
--
-- Go already knows the answer: internal/ledger's chart of accounts owns
-- normal_side and allow_negative per code, and checkDefinition refuses an
-- account whose stored values disagree with its code. That check is in the
-- service; this is the same rule where the guard actually reads it.
--
-- Two codes allow a negative balance, both of them adjustment accounts that
-- exist to absorb a correction in either direction:
-- RECONCILIATION_ADJUSTMENT and PLATFORM_ADJUSTMENT.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_ledger_code_allows_negative(p_code text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT p_code IN ('RECONCILIATION_ADJUSTMENT','PLATFORM_ADJUSTMENT');
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_ledger_code_normal_side(p_code text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE WHEN p_code IN ('WALLET','TRADING_OUTFLOW','FEES_NETWORK','FEES_VENUE','FEES_PLATFORM',
                                'PLATFORM_FEE_RECEIVABLE','CREDIT_BALANCE','NATIVE_ASSET_BALANCE',
                                'PAYOUT_RESERVED','MARKET_RESERVE','MARKET_INVENTORY','PAYOUT_SETTLED')
                THEN 'DEBIT' ELSE 'CREDIT' END;
$$;
-- +goose StatementEnd

-- Existing rows are checked too: NOT VALID would let a forged account already
-- in the table keep working, and there is no reason to expect one -- the
-- application has only ever written what the chart says. If this fails on an
-- existing database, that is a finding rather than an inconvenience.
ALTER TABLE ledger_accounts
    ADD CONSTRAINT ledger_accounts_match_chart
    CHECK (allow_negative = cp_ledger_code_allows_negative(code)
       AND normal_side = cp_ledger_code_normal_side(code));

-- ---------------------------------------------------------------------------
-- F-50. assets_kind_domain_agree accepted an asset with NO value domain.
--
-- 00710 introduces that constraint with "a kind and a domain that disagree is a
-- data-entry error that would silently reclassify value". It does not catch the
-- commonest form of that error, because of SQL's three-valued logic: for a
-- CREDIT row with value_domain NULL the first disjunct is `true AND NULL` = NULL
-- and the rest are false, so the whole expression is NULL -- and a CHECK
-- constraint fails only on FALSE. NULL passes.
--
-- Demonstrated as cp_app against a migrated database: a CREDIT asset with no
-- value domain inserts cleanly. internal/assets refuses the same shape
-- ("CREDIT assets are always domain INTERNAL_CREDIT"), so Go and SQL disagreed
-- on every kind except FIAT -- which is what the parity test beside them was
-- written to find, and did, on its first run.
--
-- The blast radius is contained rather than nil: cp_ledger_account_domain
-- raises VD001 for an asset with no domain, so a malformed asset cannot hold a
-- ledger account. What it can do is exist, be listed, and be referenced by
-- anything that does not open an account.
--
-- Stated as an equivalence so the expression is never NULL: FIAT carries no
-- domain, everything else carries one.
ALTER TABLE assets
    ADD CONSTRAINT assets_value_domain_presence
    CHECK ((kind = 'FIAT') = (value_domain IS NULL));

-- The two functions are a second copy of a Go table, which is the thing this
-- project's own register lists as a maintainability risk -- and F-43 was that
-- risk arriving in a security control. So the copies are compared by a test:
-- TestIntegration_GoAndSQLAgreeOnTheChartOfAccounts drives both functions for
-- every code in the registry.

-- +goose Down
SELECT 1; -- protected: reverting restores a privilege leak and an unenforced ledger invariant
