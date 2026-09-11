-- +goose Up
-- A rehearsal is recorded, not inferred from a NULL that two readers read
-- differently.
--
-- 00810 added `payout_requests.sandbox` NULLABLE, on 00793's reasoning: a row
-- written before the migration has no recorded fact, and `NOT NULL DEFAULT
-- false` would assert about every one of them that it was a real payout, which
-- is the one direction this label must never be wrong in.
--
-- The reasoning is right and the result is a column two readers disagree about.
--
--   * `httpapi.toAPIPayout` reads NULL as a REHEARSAL, which is the safe
--     direction for a label shown to a person;
--   * the CHECK reads it as REAL:
--
--         CHECK (NOT coalesce(sandbox, false) OR environment IS DISTINCT FROM 'PROD')
--
--     so a NULL row is exempt from the rule the constraint exists to state --
--     "a rehearsal cannot exist where real value moves" -- and the exemption is
--     invisible, because `coalesce(sandbox, false)` looks like a default rather
--     than like a hole.
--
-- One of the two is wrong on any given row and nothing can say which. So the
-- column becomes NOT NULL and the disagreement has nowhere left to live
-- (F-wv2-12's observation, D-134).
--
-- ## The backfill is `true`, and the ground for it is recorded
--
-- Every `payout_requests` row that exists anywhere was written on a non-PROD
-- tier, because no PROD deployment of this system has ever existed.
-- `docs/audit/FINAL_CHECKPOINT_2026-09-10.md` section 11 states the deployment
-- as it stands -- "Every provider is `sandbox`. `CP_AUTH_MODE` is `oidc`.
-- `CP_ENV` is `STAGING`" -- and section 13 states that every capability gate is
-- inactive by absence, `capability_gates` holding zero rows, so `PAYOUT_RESERVE`
-- and `PAYOUT_SETTLE` have never been active anywhere either. A payout row
-- written under those conditions is a rehearsal by every definition the system
-- has.
--
-- That makes `true` a statement about the rows rather than a convenient
-- default, which is the difference between a backfill and an assertion. If this
-- migration ever runs on a database that DOES hold a real payout, the CHECK
-- below refuses it: a row with `environment = 'PROD'` cannot be marked a
-- rehearsal, so the migration fails loudly instead of relabelling somebody's
-- money.
--
-- The CHECK is rewritten without the `coalesce`, so it says what it means.
-- `payout.CreateRequest.Validate` already refuses a request with no
-- environment, and `payout.Service.Create` writes both columns, so nothing in
-- this binary can write another NULL.
--
-- `environment` stays nullable. It is not a safety label -- nothing reads it as
-- permission -- and an empty string is what `scanRequest` already coalesces it
-- to; making it NOT NULL would be a second change riding along on the first.

-- The refusal, before the backfill: a PROD row cannot become a rehearsal.
-- +goose StatementBegin
DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM payout_requests
     WHERE sandbox IS NULL AND environment = 'PROD';
    IF n > 0 THEN
        RAISE EXCEPTION 'PAYOUT_SANDBOX_BACKFILL_UNSAFE: % conversion request(s) were written on a PROD tier with no recorded sandbox flag; this migration backfills true on the stated ground that no PROD deployment has ever existed, and that ground is false here',
            n USING ERRCODE = 'AD001';
    END IF;
END;
$$;
-- +goose StatementEnd

UPDATE payout_requests SET sandbox = true WHERE sandbox IS NULL;

ALTER TABLE payout_requests ALTER COLUMN sandbox SET NOT NULL;

ALTER TABLE payout_requests DROP CONSTRAINT payout_requests_sandbox_never_in_prod;
ALTER TABLE payout_requests
    ADD CONSTRAINT payout_requests_sandbox_never_in_prod
        CHECK (NOT sandbox OR environment IS DISTINCT FROM 'PROD');

COMMENT ON COLUMN payout_requests.sandbox IS
    'Whether this conversion request was a rehearsal, recorded at creation from the provider availability and the deployment tier. NOT NULL since 00817: the nullable form was read as a rehearsal by the API and as a real payout by the CHECK, so one of the two was wrong on every pre-00810 row and nothing could say which. Rows written before 00810 were backfilled true on the ground that no PROD deployment has ever existed (00817, F-232, D-096, D-134).';

-- +goose Down
SELECT 1; -- protected: the tier a payout was made on is financial history and is never dropped by rollback
