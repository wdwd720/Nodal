-- +goose Up
-- Wallets (PART 96), execution attempts (PARTS 44-48), signing decisions (PARTS 34, 95), fills (PART 21/47).

CREATE TABLE wallets (
    id                       uuid PRIMARY KEY,
    account_id               uuid NOT NULL REFERENCES accounts(id),
    provider                 text NOT NULL,
    provider_wallet_id       text NOT NULL,
    chain                    text NOT NULL,
    address                  text NOT NULL,
    kind                     text NOT NULL CHECK (kind IN ('EMBEDDED_DELEGATED')),
    status                   text NOT NULL CHECK (status IN ('ACTIVE','SUSPENDED','REVOKED')),
    delegation_ref           text,
    delegation_verified_at   timestamptz,
    signing_policy_version   text,
    capabilities             jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_wallet_id),
    UNIQUE (chain, address)
);
CREATE INDEX wallets_account_idx ON wallets (account_id);
CREATE TRIGGER wallets_updated_at BEFORE UPDATE ON wallets FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE deposits ADD CONSTRAINT deposits_wallet_fk FOREIGN KEY (destination_wallet_id) REFERENCES wallets(id);

CREATE TABLE execution_attempts (
    id                        uuid PRIMARY KEY,
    order_id                  uuid NOT NULL REFERENCES orders(id),
    plan_id                   uuid NOT NULL REFERENCES execution_plans(id),
    attempt_no                integer NOT NULL CHECK (attempt_no >= 1),
    wallet_id                 uuid NOT NULL REFERENCES wallets(id),
    provider                  text NOT NULL,
    provider_request_id       text,
    quote_id                  uuid NOT NULL REFERENCES quotes(id),
    unsigned_tx_hash          bytea,
    unsigned_tx_ref           text,
    signed_tx_hash            bytea,
    tx_signature              text UNIQUE,
    recent_blockhash          text,
    last_valid_block_height   bigint,
    simulation_ref            text,
    simulation_ok             boolean,
    inspection_result         jsonb,
    signing_decision_id       uuid,
    status                    text NOT NULL CHECK (status IN (
                                  'BUILT','INSPECTED','INSPECTION_REJECTED','SIGNING_REQUESTED','SIGNED','SIGNING_REJECTED',
                                  'SUBMITTING','SUBMITTED','SUBMISSION_UNKNOWN','OBSERVED','CONFIRMED','FINALIZED',
                                  'FAILED','EXPIRED','ADOPTED')),
    finality                  text CHECK (finality IS NULL OR finality IN ('SUBMITTED','OBSERVED','CONFIRMED','FINALIZED')),
    submitted_at              timestamptz,
    submit_response_ref       text,
    observed_at               timestamptz,
    confirmed_at              timestamptz,
    finalized_at              timestamptz,
    error                     text,
    correlation_id            text NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    UNIQUE (order_id, attempt_no)
);
CREATE INDEX execution_attempts_unknown_idx ON execution_attempts (status) WHERE status IN ('SUBMITTED','SUBMISSION_UNKNOWN','OBSERVED','CONFIRMED');
CREATE TRIGGER execution_attempts_updated_at BEFORE UPDATE ON execution_attempts FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE signing_decisions (
    id                    uuid PRIMARY KEY,
    attempt_id            uuid NOT NULL REFERENCES execution_attempts(id),
    plan_id               uuid NOT NULL REFERENCES execution_plans(id),
    intent_id             uuid NOT NULL REFERENCES trade_intents(id),
    risk_decision_id      uuid NOT NULL REFERENCES risk_decisions(id),
    wallet_id             uuid NOT NULL REFERENCES wallets(id),
    expected_tx_hash      bytea NOT NULL,
    inspected_tx_hash     bytea NOT NULL,
    decision              text NOT NULL CHECK (decision IN ('APPROVED','REJECTED')),
    reason_codes          text[] NOT NULL DEFAULT '{}',
    checks                jsonb NOT NULL,
    inspector_version     text NOT NULL,
    requested_by_service  text NOT NULL,
    provider_sign_ref     text,
    decided_at            timestamptz NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX signing_decisions_attempt_idx ON signing_decisions (attempt_id);
CREATE TRIGGER signing_decisions_immutable BEFORE UPDATE OR DELETE ON signing_decisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
ALTER TABLE execution_attempts ADD CONSTRAINT execution_attempts_signing_fk FOREIGN KEY (signing_decision_id) REFERENCES signing_decisions(id);

CREATE TABLE fills (
    id                          uuid PRIMARY KEY,
    order_id                    uuid NOT NULL REFERENCES orders(id),
    attempt_id                  uuid REFERENCES execution_attempts(id),
    account_id                  uuid NOT NULL REFERENCES accounts(id),
    venue                       text NOT NULL,
    external_fill_id            text NOT NULL,
    tx_signature                text,
    slot                        bigint,
    input_asset_id              uuid NOT NULL REFERENCES assets(id),
    input_quantity              numeric(38,0) NOT NULL CHECK (input_quantity > 0),
    output_asset_id             uuid NOT NULL REFERENCES assets(id),
    output_quantity             numeric(38,0) NOT NULL CHECK (output_quantity >= 0),
    network_fee_quantity        numeric(38,0) NOT NULL DEFAULT 0 CHECK (network_fee_quantity >= 0),
    network_fee_asset_id        uuid REFERENCES assets(id),
    venue_fee_quantity          numeric(38,0) NOT NULL DEFAULT 0 CHECK (venue_fee_quantity >= 0),
    venue_fee_asset_id          uuid REFERENCES assets(id),
    platform_fee_quantity       numeric(38,0) NOT NULL DEFAULT 0 CHECK (platform_fee_quantity >= 0),
    platform_fee_asset_id       uuid REFERENCES assets(id),
    effective_price_mantissa    numeric(38,0) NOT NULL,
    effective_price_scale       integer NOT NULL CHECK (effective_price_scale BETWEEN 0 AND 38),
    source                      text NOT NULL CHECK (source IN ('PROVIDER','CHAIN_OBSERVER','RECONCILIATION')),
    finality                    text NOT NULL CHECK (finality IN ('OBSERVED','CONFIRMED','FINALIZED')),
    observed_at                 timestamptz NOT NULL,
    received_at                 timestamptz NOT NULL DEFAULT now(),
    raw_ref                     text,
    journal_transaction_id      uuid REFERENCES journal_transactions(id),
    position_applied_at         timestamptz,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (venue, external_fill_id)
);
CREATE INDEX fills_order_idx ON fills (order_id);
CREATE INDEX fills_tx_idx ON fills (tx_signature) WHERE tx_signature IS NOT NULL;
CREATE INDEX fills_unposted_idx ON fills (created_at) WHERE journal_transaction_id IS NULL;

-- +goose StatementBegin
-- Fills are immutable except for the two "applied" columns, which may be set exactly once.
CREATE FUNCTION fills_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'FILL_IMMUTABLE: fills are never deleted' USING ERRCODE = 'LG003';
    END IF;
    IF (OLD.journal_transaction_id IS NOT NULL AND NEW.journal_transaction_id IS DISTINCT FROM OLD.journal_transaction_id)
       OR (OLD.position_applied_at IS NOT NULL AND NEW.position_applied_at IS DISTINCT FROM OLD.position_applied_at)
       OR NEW.order_id IS DISTINCT FROM OLD.order_id OR NEW.external_fill_id IS DISTINCT FROM OLD.external_fill_id
       OR NEW.input_quantity IS DISTINCT FROM OLD.input_quantity OR NEW.output_quantity IS DISTINCT FROM OLD.output_quantity
       OR NEW.input_asset_id IS DISTINCT FROM OLD.input_asset_id OR NEW.output_asset_id IS DISTINCT FROM OLD.output_asset_id
       OR NEW.network_fee_quantity IS DISTINCT FROM OLD.network_fee_quantity OR NEW.venue_fee_quantity IS DISTINCT FROM OLD.venue_fee_quantity
       OR NEW.platform_fee_quantity IS DISTINCT FROM OLD.platform_fee_quantity OR NEW.tx_signature IS DISTINCT FROM OLD.tx_signature THEN
        RAISE EXCEPTION 'FILL_IMMUTABLE: fill % economic fields cannot change', OLD.id USING ERRCODE = 'LG003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER fills_guard BEFORE UPDATE OR DELETE ON fills FOR EACH ROW EXECUTE FUNCTION fills_guard();

GRANT SELECT, INSERT, UPDATE ON wallets, execution_attempts, fills TO cp_app;
GRANT SELECT, INSERT ON signing_decisions TO cp_app;
GRANT SELECT ON wallets, execution_attempts, signing_decisions, fills TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: execution evidence is never dropped
