-- +goose Up
-- An admin action's identity is fixed at proposal (F-64).
--
-- Custom SQLSTATEs: AD001 proposal identity changed.
--
-- Dual control turns on three columns that nothing protected:
--
--   requires_dual   whether a second signature is needed at all
--   kind            what the approval authorises
--   target_id       what it authorises it against
--
-- All three were plain updatable columns on a table `cp_app` holds table-wide
-- UPDATE on, with no CHECK, no trigger, and no coverage by `params_hash` --
-- which hashes `params` alone. Two consequences, both reachable by anything
-- holding the application's database credential:
--
--   * `admin.VerifyApproved` reads `requires_dual` from the ROW, while
--     `Approve` and `executable` read it from the code's KindSpec. It is the
--     only consumer that trusts the column, and it is the gate `killswitch`,
--     `agent` and `reconciliation` call before acting. A row set to
--     `requires_dual = false` satisfies it with no approver at all.
--   * An approval's `target_id` can be repointed after approval. Both
--     principals sign for kill switch A; a single UPDATE makes the same
--     approval verify for kill switch B, because `VerifyApproved` compares the
--     caller's target against the stored one and nothing binds the stored one
--     to what was approved.
--
-- The Go half is fixed alongside this: VerifyApproved now derives dual control
-- from the KindSpec like every other caller. This is the database half, and it
-- is the one that holds whatever the Go layer does -- which is the lesson of
-- F-49, F-53 and F-61, all of which were invariants a careful service kept.
--
-- The mutable set is the lifecycle: status and the columns each transition
-- writes. Everything else is what was proposed and signed for.
--
-- `params` is deliberately NOT frozen, though `params_hash` is. The hash check
-- in Execute already refuses a rewritten params, it is driven by a test, and
-- freezing the column would make that check unreachable -- turning a working
-- control into one only a test can walk, which is the class F-26 and F-34 are
-- about. Freezing the HASH is what makes the pair sound: change params and the
-- hash no longer matches; change the hash and this trigger refuses; change both
-- and this trigger refuses. `params` is also outside the grant below, so the
-- application cannot rewrite it at all.

-- +goose StatementBegin
CREATE FUNCTION cp_admin_action_identity_frozen() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ADMIN_ACTION_IMMUTABLE: an administrative action is never deleted'
            USING ERRCODE = 'AD001';
    END IF;
    IF NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.target_type IS DISTINCT FROM OLD.target_type
       OR NEW.target_id IS DISTINCT FROM OLD.target_id
       OR NEW.requires_dual IS DISTINCT FROM OLD.requires_dual
       OR NEW.params_hash IS DISTINCT FROM OLD.params_hash
       OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.proposed_by_user_id IS DISTINCT FROM OLD.proposed_by_user_id
       OR NEW.proposed_at IS DISTINCT FROM OLD.proposed_at
       OR NEW.proposer_step_up_at IS DISTINCT FROM OLD.proposer_step_up_at
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION
            'ADMIN_ACTION_IMMUTABLE: action % was proposed as %/% and cannot be changed into %/%',
            OLD.id, OLD.kind, OLD.target_id, NEW.kind, NEW.target_id
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER admin_actions_identity_frozen BEFORE UPDATE OR DELETE ON admin_actions
    FOR EACH ROW EXECUTE FUNCTION cp_admin_action_identity_frozen();

-- The privilege half, matching 00604, 00701, 00712, 00713 and 00719: the
-- application may write only what a transition writes. A statement touching an
-- identity column is refused by permission before the trigger is reached, so a
-- mistake in either mechanism is not enough on its own.
REVOKE UPDATE ON admin_actions FROM cp_app;
GRANT UPDATE (status, approved_by_user_id, approved_at, approver_step_up_at, approval_note,
              rejected_by_user_id, rejected_at, rejected_reason,
              executed_at, execution_result, execution_error)
    ON admin_actions TO cp_app;
-- That list is exactly what updateStatus writes, read off its five call sites.
-- correlation_id is set at proposal and never updated, so it is not here.
-- updated_at is deliberately absent: set_updated_at assigns it in a BEFORE
-- trigger, and PostgreSQL checks column privileges against the statement's SET
-- list rather than against what a trigger writes. Verified by running the
-- propose/approve/execute path against this migration, not reasoned about.

COMMENT ON COLUMN admin_actions.requires_dual IS
    'Whether this action needs a second signature. Frozen at proposal by 00723. admin.VerifyApproved derives dual control from the KindSpec rather than reading this column (F-64); it is kept because it records what was true when the action was proposed.';

-- +goose Down
SELECT 1; -- protected: reverting would let an approval be repointed at another target
