-- +goose Up
-- A lost key says so, instead of blaming the caller.
--
-- 00741 made the transition flag a keyed tag computed from the single row in
-- `cp_transition_key`. That closed F-42 and F-128, and it introduced a new
-- single point of failure that the restore drill found within the hour.
--
-- ## What a lost key looked like
--
-- `cp_transition_tag` is `LANGUAGE sql` and reads the secret with a scalar
-- subquery. With no row, that subquery is NULL, `NULL || '|' || ...` is NULL,
-- `sha256(NULL)` is NULL, and the tag is NULL. Every comparison against it is
-- then NULL, which is not true, so **every state change on seventeen tables is
-- refused.**
--
-- Failing closed is the right direction and is not the problem. The problem is
-- what it says. Reproduced by emptying the table on a restored database and
-- driving one legitimate transition:
--
--     ERROR: AUDIT_TRANSITION_REQUIRED: accounts <id> changed status
--            ACTIVE -> FROZEN without a transition row describing that change
--
-- The caller wrote the transition row. The message says it did not. An operator
-- reading that at three in the morning goes looking for a bug in the code that
-- writes transitions, and the fault is a missing row in a table they have never
-- heard of and cannot read.
--
-- ## How it was found, which matters more than the fix
--
-- Not by review. The restore drill compares row counts, ledger balances and
-- journal hashes -- and **a restore that brought back every row but lost this
-- one table would have passed all three.** None of them proves the restored
-- database can still be USED, and until 00741 that was implied. It is not
-- implied any more.
--
-- So the drill now drives one real audited state transition on the restored
-- database as `cp_app`, and `CP_DRILL_BREAK=lose_the_transition_key` empties the
-- table so the probe can be watched firing. The first attempt at that break
-- failed with `permission denied for table cp_transition_key`, which is its own
-- small confirmation: the application role cannot cause this fault, only a
-- restore or an operator can.
--
-- ## What this changes
--
-- 1. `cp_transition_tag` becomes plpgsql and RAISES when the key is absent,
--    naming the fault and the remedy instead of blaming the caller. Custom
--    SQLSTATE AU001 is deliberately NOT used: this is not an audit failure, and
--    a handler catching AU001 must not treat it as one.
--
-- 2. The row cannot be deleted. `forbid_mutation` refuses DELETE for every role
--    including the owner, the same protection fifty-three append-only tables
--    already have. UPDATE is left open so the key can be rotated -- with one
--    caveat worth stating: a rotation committed between a setter and its
--    verifier invalidates tags in transactions already in flight, so a rotation
--    is a brief window in which some transactions fail closed and retry. That
--    is acceptable for a key; losing it is not.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_transition_tag(setting text, value text) RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    key text;
BEGIN
    SELECT secret INTO key FROM cp_transition_key WHERE only_row;
    IF key IS NULL THEN
        RAISE EXCEPTION 'TRANSITION_KEY_MISSING: cp_transition_key holds no row, so no state change on any audited table can be verified. This is a restore or provisioning fault, not a caller error: the transition row you wrote is fine. Restore the row from a backup of that table, or rotate a new key into it; do not disable the binding.'
            USING ERRCODE = 'AD001',
                  HINT = 'INSERT INTO cp_transition_key (only_row, secret) VALUES (true, encode(gen_random_bytes(32), ''hex'')) -- only if the original is genuinely unrecoverable';
    END IF;
    RETURN encode(sha256((key || '|' || pg_current_xact_id()::text || '|' || setting || '|' || value)::bytea), 'hex');
END;
$$;
-- +goose StatementEnd

REVOKE EXECUTE ON FUNCTION cp_transition_tag(text, text) FROM PUBLIC;

CREATE TRIGGER cp_transition_key_undeletable BEFORE DELETE ON cp_transition_key
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

COMMENT ON FUNCTION cp_transition_tag(text, text) IS
    'The keyed tag the transition flag carries. Raises AD001 naming the fault when the key row is absent, rather than letting a NULL tag surface as an AU001 that blames the caller (00742, F-129).';

-- +goose Down
SELECT 1; -- protected: reverting returns a lost key to an error that sends the operator to the wrong place
