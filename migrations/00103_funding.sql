-- +goose Up
-- Funding (deposits) state machine (PART 28), funding/withdrawal risk flags (PART 27), withdrawal boundary (PART 94).

CREATE TABLE funding_sources (
    id            uuid PRIMARY KEY,
    account_id    uuid NOT NULL REFERENCES accounts(id),
    provider      text NOT NULL,
    provider_ref  text,
    kind          text NOT NULL CHECK (kind IN ('CARD','BANK','OTHER')),
    label         text,
    status        text NOT NULL CHECK (status IN ('ACTIVE','DISABLED')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER funding_sources_updated_at BEFORE UPDATE ON funding_sources FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE deposits (
    id                               uuid PRIMARY KEY,
    account_id                       uuid NOT NULL REFERENCES accounts(id),
    funding_source_id                uuid REFERENCES funding_sources(id),
    provider                         text NOT NULL,
    provider_session_id              text,
    provider_ref                     text,
    status                           text NOT NULL CHECK (status IN (
                                         'CREATED','SESSION_CREATED','CUSTOMER_ACTION_REQUIRED','PROVIDER_PROCESSING',
                                         'PROVIDER_CONFIRMED','SETTLEMENT_OBSERVED','RECONCILED','AVAILABLE',
                                         'FAILED','EXPIRED','CANCELLED','REVERSED','REVIEW_REQUIRED')),
    fiat_amount_minor                bigint CHECK (fiat_amount_minor IS NULL OR fiat_amount_minor > 0),
    fiat_currency                    text,
    expected_asset_id                uuid NOT NULL REFERENCES assets(id),
    expected_quantity                numeric(38,0) CHECK (expected_quantity IS NULL OR expected_quantity > 0),
    observed_quantity                numeric(38,0) CHECK (observed_quantity IS NULL OR observed_quantity >= 0),
    destination_wallet_id            uuid,
    destination_address              text,
    tx_signature                     text,
    chain_slot                       bigint,
    fraud_state                      text NOT NULL DEFAULT 'NONE' CHECK (fraud_state IN ('NONE','REVIEW','CONFIRMED_FRAUD','CLEARED')),
    reversible_until                 timestamptz,
    buying_power_eligible            boolean NOT NULL DEFAULT false,
    withdrawal_eligible              boolean NOT NULL DEFAULT false,
    availability_policy_version      text,
    journal_transaction_id           uuid REFERENCES journal_transactions(id),
    reversal_journal_transaction_id  uuid REFERENCES journal_transactions(id),
    idempotency_key                  text NOT NULL UNIQUE,
    correlation_id                   text,
    created_at                       timestamptz NOT NULL DEFAULT now(),
    session_created_at               timestamptz,
    customer_action_at               timestamptz,
    provider_processing_at           timestamptz,
    provider_confirmed_at            timestamptz,
    settlement_observed_at           timestamptz,
    reconciled_at                    timestamptz,
    available_at                     timestamptz,
    failed_at                        timestamptz,
    expired_at                       timestamptz,
    cancelled_at                     timestamptz,
    reversed_at                      timestamptz,
    review_required_at               timestamptz,
    updated_at                       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_session_id)
);
CREATE INDEX deposits_account_idx ON deposits (account_id, created_at);
CREATE INDEX deposits_status_idx ON deposits (status) WHERE status NOT IN ('AVAILABLE','FAILED','EXPIRED','CANCELLED','REVERSED');
CREATE INDEX deposits_tx_sig_idx ON deposits (tx_signature) WHERE tx_signature IS NOT NULL;
CREATE TRIGGER deposits_updated_at BEFORE UPDATE ON deposits FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE deposit_transitions (
    id              uuid PRIMARY KEY,
    deposit_id      uuid NOT NULL REFERENCES deposits(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    reason          text NOT NULL,
    evidence_ref    text,
    correlation_id  text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX deposit_transitions_deposit_idx ON deposit_transitions (deposit_id, occurred_at);
CREATE TRIGGER deposit_transitions_immutable BEFORE UPDATE OR DELETE ON deposit_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Withdrawal domain exists; the WITHDRAWALS capability is DISABLED in V1. Agents can never reach this table.
CREATE TABLE withdrawals (
    id                       uuid PRIMARY KEY,
    account_id               uuid NOT NULL REFERENCES accounts(id),
    asset_id                 uuid NOT NULL REFERENCES assets(id),
    quantity                 numeric(38,0) NOT NULL CHECK (quantity > 0),
    destination_address      text NOT NULL,
    destination_validated    boolean NOT NULL DEFAULT false,
    status                   text NOT NULL CHECK (status IN (
                                 'REQUESTED','STEP_UP_VERIFIED','APPROVAL_PENDING','APPROVED','REJECTED',
                                 'SUBMITTED','SUBMISSION_UNKNOWN','SETTLED','FAILED','CANCELLED')),
    requested_by_user_id     uuid NOT NULL REFERENCES users(id),
    step_up_verified_at      timestamptz,
    approval_id              uuid,
    capability_check_ref     text,
    reservation_id           uuid REFERENCES asset_reservations(id),
    tx_signature             text,
    journal_transaction_id   uuid REFERENCES journal_transactions(id),
    idempotency_key          text NOT NULL UNIQUE,
    correlation_id           text,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX withdrawals_account_idx ON withdrawals (account_id, created_at);
CREATE TRIGGER withdrawals_updated_at BEFORE UPDATE ON withdrawals FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE withdrawal_transitions (
    id              uuid PRIMARY KEY,
    withdrawal_id   uuid NOT NULL REFERENCES withdrawals(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL CHECK (actor_type <> 'AGENT'),
    actor_id        text NOT NULL,
    reason          text NOT NULL,
    evidence_ref    text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX withdrawal_transitions_idx ON withdrawal_transitions (withdrawal_id, occurred_at);
CREATE TRIGGER withdrawal_transitions_immutable BEFORE UPDATE OR DELETE ON withdrawal_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON funding_sources, deposits, withdrawals TO cp_app;
GRANT SELECT, INSERT ON deposit_transitions, withdrawal_transitions TO cp_app;
GRANT SELECT ON funding_sources, deposits, deposit_transitions, withdrawals, withdrawal_transitions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: funding history is financial record
