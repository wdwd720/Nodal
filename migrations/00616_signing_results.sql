-- +goose Up
-- Signing results (PART 95): the signed transaction produced for an APPROVED signing decision.
--
-- signing_decisions (00300) is immutable, so the outcome of the provider call cannot be
-- written onto the decision row after the fact. A separate append-only row records the
-- signed bytes and the provider reference once, keyed by the decision. A repeated Sign for
-- the same attempt returns this row instead of asking the provider again: the signing
-- service never signs twice for one attempt.

CREATE TABLE signing_results (
    id                  uuid PRIMARY KEY,
    decision_id         uuid NOT NULL UNIQUE REFERENCES signing_decisions(id),
    attempt_id          uuid NOT NULL REFERENCES execution_attempts(id),
    wallet_id           uuid NOT NULL REFERENCES wallets(id),
    provider            text NOT NULL,
    provider_sign_ref   text,
    idempotency_key     text NOT NULL,
    signed_tx           bytea NOT NULL CHECK (length(signed_tx) > 0),
    signed_tx_hash      bytea NOT NULL CHECK (length(signed_tx_hash) = 32),
    signature           bytea NOT NULL CHECK (length(signature) = 64),
    retry_class         text NOT NULL CHECK (retry_class IN ('SAFE_RETRY','IDEMPOTENT_WRITE','UNKNOWN_EFFECT_WRITE')),
    signed_at           timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (attempt_id)
);
CREATE TRIGGER signing_results_immutable BEFORE UPDATE OR DELETE ON signing_results
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT ON signing_results TO cp_app;
GRANT SELECT ON signing_results TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: signing evidence is never dropped
