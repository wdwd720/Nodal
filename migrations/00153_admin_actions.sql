-- +goose Up
-- Controlled administrative actions with dual control (PARTS 91, 93, 128, 129, 164).

CREATE TABLE admin_actions (
    id                    uuid PRIMARY KEY,
    kind                  text NOT NULL,
    target_type           text NOT NULL,
    target_id             text NOT NULL,
    params                jsonb NOT NULL DEFAULT '{}'::jsonb,
    params_hash           bytea NOT NULL,
    reason                text NOT NULL CHECK (length(reason) >= 8),
    requires_dual         boolean NOT NULL,
    status                text NOT NULL CHECK (status IN ('PROPOSED','APPROVED','REJECTED','EXECUTED','FAILED','EXPIRED','CANCELLED')),
    proposed_by_user_id   uuid NOT NULL REFERENCES users(id),
    proposed_at           timestamptz NOT NULL DEFAULT now(),
    proposer_step_up_at   timestamptz NOT NULL,
    approved_by_user_id   uuid REFERENCES users(id),
    approved_at           timestamptz,
    approver_step_up_at   timestamptz,
    approval_note         text,
    rejected_by_user_id   uuid REFERENCES users(id),
    rejected_at           timestamptz,
    rejected_reason       text,
    executed_at           timestamptz,
    execution_result      jsonb,
    execution_error       text,
    expires_at            timestamptz NOT NULL,
    correlation_id        text,
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (approved_by_user_id IS NULL OR approved_by_user_id <> proposed_by_user_id)
);
CREATE INDEX admin_actions_status_idx ON admin_actions (status, expires_at);
CREATE INDEX admin_actions_target_idx ON admin_actions (target_type, target_id, proposed_at);
CREATE TRIGGER admin_actions_updated_at BEFORE UPDATE ON admin_actions FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE admin_action_transitions (
    id           uuid PRIMARY KEY,
    action_id    uuid NOT NULL REFERENCES admin_actions(id),
    from_status  text NOT NULL,
    to_status    text NOT NULL,
    actor_id     text NOT NULL,
    note         text,
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX admin_action_transitions_idx ON admin_action_transitions (action_id, occurred_at);
CREATE TRIGGER admin_action_transitions_immutable BEFORE UPDATE OR DELETE ON admin_action_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON admin_actions TO cp_app;
GRANT SELECT, INSERT ON admin_action_transitions TO cp_app;
GRANT SELECT ON admin_actions, admin_action_transitions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: administrative action history is audit record
