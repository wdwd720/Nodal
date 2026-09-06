-- +goose Up
-- Versioned eligibility and risk policies (PARTS 56-60) and their persisted decisions.
-- intent_id columns are plain uuids here; migration 00201 adds the FK once trade_intents exists.

CREATE TABLE eligibility_policies (
    id                     uuid PRIMARY KEY,
    version                text NOT NULL UNIQUE,
    rules                  jsonb NOT NULL,
    rules_hash             bytea NOT NULL,
    effective_at           timestamptz NOT NULL,
    expires_at             timestamptz,
    created_by_actor_type  text NOT NULL CHECK (created_by_actor_type IN ('OPERATOR','SYSTEM')),
    created_by_actor_id    text NOT NULL,
    reason                 text NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX eligibility_policies_effective_idx ON eligibility_policies (effective_at DESC);
CREATE TRIGGER eligibility_policies_immutable BEFORE UPDATE OR DELETE ON eligibility_policies
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE eligibility_decisions (
    id              uuid PRIMARY KEY,
    account_id      uuid REFERENCES accounts(id),
    user_id         uuid REFERENCES users(id),
    intent_id       uuid,
    context_kind    text NOT NULL CHECK (context_kind IN ('TRADE','FUNDING','WITHDRAWAL','AGENT_RUN','STRATEGY_PROMOTION')),
    instrument_id   uuid,
    asset_class     text,
    venue           text,
    provider        text,
    eligible        boolean NOT NULL,
    policy_version  text NOT NULL,
    reason_codes    text[] NOT NULL DEFAULT '{}',
    context_hash    bytea NOT NULL,
    evaluated_at    timestamptz NOT NULL,
    correlation_id  text
);
CREATE INDEX eligibility_decisions_account_idx ON eligibility_decisions (account_id, evaluated_at);
CREATE INDEX eligibility_decisions_intent_idx ON eligibility_decisions (intent_id) WHERE intent_id IS NOT NULL;
CREATE TRIGGER eligibility_decisions_immutable BEFORE UPDATE OR DELETE ON eligibility_decisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE risk_policies (
    id                     uuid PRIMARY KEY,
    version                text NOT NULL UNIQUE,
    scope                  text NOT NULL CHECK (scope IN ('GLOBAL','ACCOUNT','AGENT')),
    scope_id               text NOT NULL DEFAULT '*',
    rules                  jsonb NOT NULL,
    rules_hash             bytea NOT NULL,
    effective_at           timestamptz NOT NULL,
    expires_at             timestamptz,
    created_by_actor_type  text NOT NULL CHECK (created_by_actor_type IN ('OPERATOR','SYSTEM','USER')),
    created_by_actor_id    text NOT NULL,
    reason                 text NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX risk_policies_scope_idx ON risk_policies (scope, scope_id, effective_at DESC);
CREATE TRIGGER risk_policies_immutable BEFORE UPDATE OR DELETE ON risk_policies
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE risk_decisions (
    id                      uuid PRIMARY KEY,
    intent_id               uuid,
    account_id              uuid REFERENCES accounts(id),
    agent_id                uuid,
    stage                   text NOT NULL CHECK (stage IN ('PRE_TRADE','FINAL','CONTINUOUS')),
    policy_version          text NOT NULL,
    policy_hash             bytea NOT NULL,
    account_snapshot        jsonb NOT NULL,
    market_snapshot         jsonb NOT NULL,
    quote_id                uuid,
    decision                text NOT NULL CHECK (decision IN ('ALLOW','REJECT')),
    reason_codes            text[] NOT NULL DEFAULT '{}',
    resulting_constraints   jsonb NOT NULL DEFAULT '{}'::jsonb,
    evaluator_version       text NOT NULL,
    evaluated_at            timestamptz NOT NULL,
    correlation_id          text
);
CREATE INDEX risk_decisions_intent_idx ON risk_decisions (intent_id, stage) WHERE intent_id IS NOT NULL;
CREATE INDEX risk_decisions_account_idx ON risk_decisions (account_id, evaluated_at);
CREATE TRIGGER risk_decisions_immutable BEFORE UPDATE OR DELETE ON risk_decisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT ON eligibility_policies, eligibility_decisions, risk_policies, risk_decisions TO cp_app;
GRANT SELECT ON eligibility_policies, eligibility_decisions, risk_policies, risk_decisions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: policy and decision history is audit record
