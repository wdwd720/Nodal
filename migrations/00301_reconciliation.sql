-- +goose Up
-- Reconciliation records (PARTS 50, 51, 163, 195) and wallet balance observations.

CREATE TABLE wallet_balance_observations (
    id            uuid PRIMARY KEY,
    wallet_id     uuid NOT NULL REFERENCES wallets(id),
    asset_id      uuid NOT NULL REFERENCES assets(id),
    quantity      numeric(38,0) NOT NULL CHECK (quantity >= 0),
    source        text NOT NULL,                       -- 'helius', 'rpc-fallback'
    slot          bigint,
    observed_at   timestamptz NOT NULL,
    received_at   timestamptz NOT NULL DEFAULT now(),
    raw_ref       text
);
CREATE INDEX wallet_balance_observations_idx ON wallet_balance_observations (wallet_id, asset_id, received_at DESC);

CREATE TABLE reconciliation_records (
    id                                   uuid PRIMARY KEY,
    kind                                 text NOT NULL CHECK (kind IN ('EXECUTION','WALLET_BALANCE','FUNDING','LEDGER_INTERNAL','POSITION_LEDGER','SUBMISSION_UNKNOWN')),
    mode                                 text NOT NULL CHECK (mode IN ('EVENT_DRIVEN','PERIODIC','FULL')),
    scope_type                           text NOT NULL,
    scope_id                             text NOT NULL,
    account_id                           uuid REFERENCES accounts(id),
    asset_id                             uuid REFERENCES assets(id),
    expected                             jsonb NOT NULL,
    observed                             jsonb NOT NULL,
    difference                           jsonb NOT NULL DEFAULT '{}'::jsonb,
    status                               text NOT NULL CHECK (status IN ('OPEN','MATCHED','MISMATCH','INVESTIGATING','RESOLVED_AUTOMATIC','RESOLVED_MANUAL','ESCALATED')),
    material                             boolean NOT NULL DEFAULT false,
    blocks_new_risk                      boolean NOT NULL DEFAULT false,
    opened_at                            timestamptz NOT NULL DEFAULT now(),
    matched_at                           timestamptz,
    resolved_at                          timestamptz,
    resolved_by_actor_type               text CHECK (resolved_by_actor_type IS NULL OR resolved_by_actor_type <> 'AGENT'),
    resolved_by_actor_id                 text,
    resolution_reason                    text,
    resolution_evidence_ref              text,
    approval_id                          uuid REFERENCES admin_actions(id),
    compensating_journal_transaction_id  uuid REFERENCES journal_transactions(id),
    correlation_id                       text,
    created_at                           timestamptz NOT NULL DEFAULT now(),
    updated_at                           timestamptz NOT NULL DEFAULT now(),
    CHECK (status <> 'RESOLVED_MANUAL' OR (resolved_by_actor_id IS NOT NULL AND resolution_reason IS NOT NULL AND resolution_evidence_ref IS NOT NULL))
);
CREATE INDEX reconciliation_records_open_idx ON reconciliation_records (status, opened_at) WHERE status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED');
CREATE INDEX reconciliation_records_account_idx ON reconciliation_records (account_id, opened_at DESC) WHERE account_id IS NOT NULL;
CREATE INDEX reconciliation_records_scope_idx ON reconciliation_records (scope_type, scope_id);
CREATE TRIGGER reconciliation_records_updated_at BEFORE UPDATE ON reconciliation_records FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE reconciliation_transitions (
    id             uuid PRIMARY KEY,
    record_id      uuid NOT NULL REFERENCES reconciliation_records(id),
    from_status    text NOT NULL,
    to_status      text NOT NULL,
    actor_type     text NOT NULL,
    actor_id       text NOT NULL,
    reason         text,
    evidence_ref   text,
    occurred_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX reconciliation_transitions_idx ON reconciliation_transitions (record_id, occurred_at);
CREATE TRIGGER reconciliation_transitions_immutable BEFORE UPDATE OR DELETE ON reconciliation_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT ON wallet_balance_observations, reconciliation_transitions TO cp_app;
GRANT SELECT, INSERT, UPDATE ON reconciliation_records TO cp_app;
GRANT SELECT ON wallet_balance_observations, reconciliation_records, reconciliation_transitions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: reconciliation history is financial record
