-- +goose Up
-- Production capability gates (PARTS 54, 55, 244). Agents can never write here (application-level rejection
-- plus actor_type CHECK on transitions).

CREATE TABLE capability_gates (
    id                     uuid PRIMARY KEY,
    capability             text NOT NULL CHECK (capability IN (
                               'LIVE_FUNDING','LIVE_MANUAL_TRADING','LIVE_AGENT_TRADING','WITHDRAWALS',
                               'SOCIAL_DATA_PERSISTENCE','MARKETPLACE','CROSS_CHAIN','PREDICTION_MARKETS','SECURITIES','CEX_TRADING')),
    environment            text NOT NULL CHECK (environment IN ('LOCAL','TEST','DEV','STAGING','PROD')),
    state                  text NOT NULL CHECK (state IN ('DISABLED','PENDING_APPROVAL','APPROVED','ACTIVE','SUSPENDED','REVOKED','EXPIRED')),
    approval_version       integer NOT NULL DEFAULT 0 CHECK (approval_version >= 0),
    legal_review_ref       text,
    provider_contract_ref  text,
    risk_approval_ref      text,
    security_approval_ref  text,
    proposed_by_user_id    uuid,
    approvers              jsonb NOT NULL DEFAULT '[]'::jsonb,   -- [{user_id, role, at, step, evidence_hash}]
    evidence_hashes        jsonb NOT NULL DEFAULT '[]'::jsonb,
    effective_at           timestamptz,
    expires_at             timestamptz,
    revoked_at             timestamptz,
    revoke_reason          text,
    version                bigint NOT NULL DEFAULT 1,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (capability, environment)
);
CREATE TRIGGER capability_gates_updated_at BEFORE UPDATE ON capability_gates FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE capability_gate_transitions (
    id             uuid PRIMARY KEY,
    gate_id        uuid NOT NULL REFERENCES capability_gates(id),
    from_state     text NOT NULL,
    to_state       text NOT NULL,
    actor_type     text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SYSTEM')),
    actor_id       text NOT NULL,
    reason         text NOT NULL,
    approval_id    uuid,
    evidence_hash  bytea,
    occurred_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX capability_gate_transitions_gate_idx ON capability_gate_transitions (gate_id, occurred_at);
CREATE TRIGGER capability_gate_transitions_immutable BEFORE UPDATE OR DELETE ON capability_gate_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON capability_gates TO cp_app;
GRANT SELECT, INSERT ON capability_gate_transitions TO cp_app;
GRANT SELECT ON capability_gates, capability_gate_transitions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: gate approval history is audit record
