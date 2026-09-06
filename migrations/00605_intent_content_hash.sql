-- +goose Up
-- Intent content hash and identity immutability (PARTS 35, 36, 159, 173).
--
-- content_hash = sha256(intent.Canonical(t)): a deterministic digest of the
-- semantic content of an intent (account, actor, agent linkage, action,
-- instrument, amounts, constraints, deadline, idempotency key, mode). When an
-- INSERT collides on (account_id, idempotency_key), intent.Repository.Create
-- compares the stored hash with the incoming one: equal means the same command
-- was retried and the original intent is returned; different means the key is
-- being reused for a different financial intent and the command fails with
-- INVALID_IDEMPOTENCY_REUSE. The (account_id, content_hash) index serves
-- support and forensic queries for identical content submitted under different
-- keys (a client that regenerated its key on retry, PART 173); the replay path
-- itself uses the existing (account_id, idempotency_key) unique index.
ALTER TABLE trade_intents ADD COLUMN content_hash bytea;
UPDATE trade_intents SET content_hash = sha256(('legacy:' || id::text)::bytea) WHERE content_hash IS NULL;
ALTER TABLE trade_intents ALTER COLUMN content_hash SET NOT NULL;
ALTER TABLE trade_intents ADD CONSTRAINT trade_intents_content_hash_len CHECK (length(content_hash) = 32);
CREATE INDEX trade_intents_content_hash_idx ON trade_intents (account_id, content_hash);

-- The identity, economics and mode of an intent never change after receipt
-- (PART 159: historical mode is never changed). Only status, rejection_code,
-- terminal_at and the decision/reservation/plan/order links are writable, and
-- a status change is additionally bound to an intent_transitions row by
-- migration 00603. SQLSTATE IN001 is classified by intent.IsImmutableIntent.
-- +goose StatementBegin
CREATE FUNCTION trade_intents_immutable_identity() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.account_id IS DISTINCT FROM OLD.account_id
       OR NEW.actor_type IS DISTINCT FROM OLD.actor_type
       OR NEW.actor_id IS DISTINCT FROM OLD.actor_id
       OR NEW.agent_id IS DISTINCT FROM OLD.agent_id
       OR NEW.strategy_version_id IS DISTINCT FROM OLD.strategy_version_id
       OR NEW.prediction_id IS DISTINCT FROM OLD.prediction_id
       OR NEW.action IS DISTINCT FROM OLD.action
       OR NEW.instrument_id IS DISTINCT FROM OLD.instrument_id
       OR NEW.notional_usd_minor IS DISTINCT FROM OLD.notional_usd_minor
       OR NEW.target_exposure_usd_minor IS DISTINCT FROM OLD.target_exposure_usd_minor
       OR NEW.quantity IS DISTINCT FROM OLD.quantity
       OR NEW.constraints IS DISTINCT FROM OLD.constraints
       OR NEW.deadline IS DISTINCT FROM OLD.deadline
       OR NEW.requested_at IS DISTINCT FROM OLD.requested_at
       OR NEW.received_at IS DISTINCT FROM OLD.received_at
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
       OR NEW.mode IS DISTINCT FROM OLD.mode
       OR NEW.content_hash IS DISTINCT FROM OLD.content_hash
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'INTENT_IMMUTABLE: identity, economics and mode of intent % cannot change', OLD.id
            USING ERRCODE = 'IN001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER trade_intents_immutable_identity BEFORE UPDATE ON trade_intents
    FOR EACH ROW EXECUTE FUNCTION trade_intents_immutable_identity();

-- +goose Down
SELECT 1; -- protected: intents are financial evidence; the content hash and the identity guard are never removed
