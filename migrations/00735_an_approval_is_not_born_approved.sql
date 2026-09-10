-- +goose Up
-- An approval is not born approved.
--
-- Dual control in this system is `admin_actions`: one principal proposes, a
-- different one approves, and the row is the record that both happened. Every
-- control on it guards the UPDATE path.
--
--   admin_actions_identity_frozen      BEFORE DELETE OR UPDATE
--   admin_actions_require_transition   AFTER UPDATE OF status
--
-- Neither fires on an INSERT. And the only CHECK about approval is
--
--   CHECK (approved_by_user_id IS NULL OR approved_by_user_id <> proposed_by_user_id)
--
-- which is satisfied by naming any second user id. `status` permits APPROVED
-- and EXECUTED among its seven values. So `cp_app`, which holds INSERT, could
-- write a row **born APPROVED** with an approver, an approval timestamp and an
-- approver step-up already filled in -- forging dual control outright, with no
-- proposal, no second person and no transition row, because there was no
-- transition (F-121).
--
-- That is not a theoretical shape. `agent_lifecycle_transitions.approval_id`
-- REFERENCES admin_actions(id), so a forged APPROVED row is the missing half of
-- a fully-evidenced agent promotion to LIVE. `internal/admin`'s own integration
-- test names this forgery and declines to write a test for it, which is F-42.
--
-- capability_gates is the one entity in this schema that is closed at birth:
-- `cp_gate_born_disabled` (00701) refuses a gate born in any state but DISABLED
-- and refuses a pre-filled approval chain, for every role including cp_migrate.
-- This is that treatment applied to the table dual control actually lives in.
--
-- Stated as a trigger rather than a CHECK because a CHECK cannot distinguish an
-- INSERT from an UPDATE, and the UPDATE path is exactly where these columns are
-- legitimately filled in.
--
-- Custom SQLSTATE: AD001, as in 00723.

-- +goose StatementBegin
CREATE FUNCTION cp_admin_action_born_proposed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status <> 'PROPOSED' THEN
        RAISE EXCEPTION 'ADMIN_ACTION_BORN_DECIDED: an admin action is created PROPOSED, not %; a decision is an UPDATE with a transition row behind it',
            NEW.status USING ERRCODE = 'AD001';
    END IF;
    IF NEW.approved_by_user_id IS NOT NULL
       OR NEW.approved_at IS NOT NULL
       OR NEW.approver_step_up_at IS NOT NULL
       OR NEW.approval_note IS NOT NULL
       OR NEW.rejected_by_user_id IS NOT NULL
       OR NEW.rejected_at IS NOT NULL
       OR NEW.rejected_reason IS NOT NULL
       OR NEW.executed_at IS NOT NULL
       OR NEW.execution_result IS NOT NULL
       OR NEW.execution_error IS NOT NULL THEN
        RAISE EXCEPTION 'ADMIN_ACTION_BORN_DECIDED: an admin action is created with no approver, no rejection and no execution recorded'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER admin_actions_born_proposed
    BEFORE INSERT ON admin_actions
    FOR EACH ROW EXECUTE FUNCTION cp_admin_action_born_proposed();

COMMENT ON FUNCTION cp_admin_action_born_proposed() IS
    'An admin action is created PROPOSED with nothing decided. Every other control on this table guards UPDATE, so a row born APPROVED forged dual control with no proposal and no second person (F-121).';

-- +goose Down
SELECT 1; -- protected: reverting lets the application role forge a dual-control approval at INSERT
