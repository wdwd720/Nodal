-- +goose Up
-- Emergency controls (PARTS 52, 53, 165). Current state per (kind, scope) plus immutable transition history.

CREATE TABLE kill_switches (
    id                     uuid PRIMARY KEY,
    kind                   text NOT NULL CHECK (kind IN (
                               'GLOBAL_NEW_RISK_KILL','ACCOUNT_FREEZE','AGENT_PAUSE','STRATEGY_VERSION_DISABLE','VENUE_DISABLE',
                               'INSTRUMENT_CLOSE_ONLY','INSTRUMENT_HALT','CHAIN_DISABLE_NEW_ACTIONS','PROVIDER_DISABLE_NEW_ACTIONS',
                               'FUNDING_DISABLE','WITHDRAWALS_DISABLE','MODEL_DISABLE')),
    scope_id               text NOT NULL DEFAULT '*',
    active                 boolean NOT NULL,
    severity               text NOT NULL CHECK (severity IN ('STANDARD','SEVERE')),
    reason                 text NOT NULL,
    activated_by_actor_id  text,
    activated_at           timestamptz,
    released_by_actor_id   text,
    released_at            timestamptz,
    release_approval_id    uuid,
    release_reason         text,
    version                bigint NOT NULL DEFAULT 1,
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (kind, scope_id)
);
CREATE INDEX kill_switches_active_idx ON kill_switches (kind, scope_id) WHERE active;
CREATE TRIGGER kill_switches_updated_at BEFORE UPDATE ON kill_switches FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE kill_switch_transitions (
    id            uuid PRIMARY KEY,
    switch_id     uuid NOT NULL REFERENCES kill_switches(id),
    kind          text NOT NULL,
    scope_id      text NOT NULL,
    to_active     boolean NOT NULL,
    actor_type    text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SYSTEM')),
    actor_id      text NOT NULL,
    reason        text NOT NULL,
    approval_id   uuid,
    occurred_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX kill_switch_transitions_switch_idx ON kill_switch_transitions (switch_id, occurred_at);
CREATE TRIGGER kill_switch_transitions_immutable BEFORE UPDATE OR DELETE ON kill_switch_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON kill_switches TO cp_app;
GRANT SELECT, INSERT ON kill_switch_transitions TO cp_app;
GRANT SELECT ON kill_switches, kill_switch_transitions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: emergency-control history is audit record
