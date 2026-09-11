-- +goose Up
-- A sandbox-compiled strategy version says so, and cannot exist in PROD (D-129).
--
-- The structured compiler migration 00811 named exists for one purpose: to let
-- a sandbox tier rehearse goal SS18 end to end -- describe, compile, review,
-- accept, create an agent -- without a model provider and without inferring
-- anything. Everything it produces is a rehearsal, and ADR-0023's rule for a
-- rehearsal is that it is labelled everywhere it is stored and shown and that
-- it exists only where nothing real can move.
--
-- `strategy_versions` had nowhere to put either fact. It carries no
-- environment, unlike `capability_gates`, `payout_quotes`,
-- `verification_sessions` and `demo_seed_rows`, all of which refuse a PROD row
-- with a CHECK on a column they already had. So this migration gives the table
-- the two columns that make the label a property of the row:
--
--   * `sandbox` -- this version was produced by a compiler that exists only on
--     a sandbox tier. The API renders it, the review screen shows it, and an
--     agent created from it is a rehearsal.
--   * `environment` -- the deployment that compiled it, so the PROD refusal is
--     a constraint rather than a convention. Rows written before today have no
--     recorded environment and carry '' , which is why the CHECKs below are
--     written as implications rather than as a NOT NULL on the value.
--
-- THREE PROPERTIES, AND WHY EACH IS A CHECK RATHER THAN A COMMENT.
--
--   1. STRUCTURED_SANDBOX implies sandbox. A version from the structured
--      compiler cannot be written without the label, so the label cannot be
--      forgotten by a caller that constructs the row itself.
--   2. sandbox implies a named, non-PROD environment. This is the property
--      that matters: a sandbox-compiled version can never exist in a
--      production database, whatever a binary believes about itself.
--   3. Both columns are immutable, added to the guard 00500 installed. Without
--      this, `cp_app` -- which holds UPDATE on this table because acceptance
--      writes status -- could clear `sandbox` on a row it had already written
--      and turn a rehearsal into something that reads as real.
--
-- WHAT DOES NOT CHANGE. Every existing row: `sandbox` defaults to false and
-- `environment` to '', which is the truth about a version compiled before this
-- column existed, and no existing row is STRUCTURED_SANDBOX because migration
-- 00811 introduced the name.

ALTER TABLE strategy_versions ADD COLUMN sandbox boolean NOT NULL DEFAULT false;
ALTER TABLE strategy_versions ADD COLUMN environment text NOT NULL DEFAULT '';

ALTER TABLE strategy_versions ADD CONSTRAINT strategy_versions_structured_sandbox_check
    CHECK (source_kind <> 'STRUCTURED_SANDBOX' OR sandbox);
ALTER TABLE strategy_versions ADD CONSTRAINT strategy_versions_sandbox_not_in_prod_check
    CHECK (NOT sandbox OR (environment <> 'PROD' AND environment <> ''));

-- The guard 00500 installed, restated with the two new columns among the
-- immutable ones. PostgreSQL has no way to replace part of a function body, so
-- the rest is byte-identical to 00500's: same columns, same message, same
-- ERRCODE ST001, same trigger.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION strategy_versions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'STRATEGY_VERSION_IMMUTABLE: versions are never deleted' USING ERRCODE = 'ST001';
    END IF;
    IF NEW.ir IS DISTINCT FROM OLD.ir OR NEW.ir_hash IS DISTINCT FROM OLD.ir_hash
       OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.effect_set IS DISTINCT FROM OLD.effect_set
       OR NEW.strategy_id IS DISTINCT FROM OLD.strategy_id OR NEW.version IS DISTINCT FROM OLD.version
       OR NEW.source_kind IS DISTINCT FROM OLD.source_kind OR NEW.source_hash IS DISTINCT FROM OLD.source_hash
       OR NEW.compiler_version IS DISTINCT FROM OLD.compiler_version OR NEW.sdk_version IS DISTINCT FROM OLD.sdk_version
       OR NEW.compile_attempt_id IS DISTINCT FROM OLD.compile_attempt_id OR NEW.parent_version_id IS DISTINCT FROM OLD.parent_version_id
       OR NEW.risk_policy_version IS DISTINCT FROM OLD.risk_policy_version OR NEW.risk_policy_hash IS DISTINCT FROM OLD.risk_policy_hash
       OR NEW.model_budget IS DISTINCT FROM OLD.model_budget OR NEW.data_budget IS DISTINCT FROM OLD.data_budget
       OR NEW.envelope_requirements IS DISTINCT FROM OLD.envelope_requirements
       OR NEW.human_readable IS DISTINCT FROM OLD.human_readable OR NEW.built_at IS DISTINCT FROM OLD.built_at
       OR NEW.sandbox IS DISTINCT FROM OLD.sandbox OR NEW.environment IS DISTINCT FROM OLD.environment THEN
        RAISE EXCEPTION 'STRATEGY_VERSION_IMMUTABLE: compiled fields of % cannot change; compile a new version', OLD.id
            USING ERRCODE = 'ST001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
SELECT 1; -- protected: dropping the label would leave rehearsal versions indistinguishable from real ones, which is the property this migration exists to create
