-- +goose Up
-- Wallet status changes (ACTIVE/SUSPENDED/REVOKED, PART 96) must carry a transition row
-- written in the same transaction, exactly like accounts/assets/orders (migration 00603).
-- The wallets table (00300) shipped without a transition table; this adds one and binds it.

CREATE TABLE wallet_status_transitions (
    id              uuid PRIMARY KEY,
    wallet_id       uuid NOT NULL REFERENCES wallets(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL CHECK (to_status IN ('ACTIVE','SUSPENDED','REVOKED')),
    actor_type      text NOT NULL CHECK (actor_type <> 'AGENT'),   -- agents never change wallet authority (PART 9)
    actor_id        text NOT NULL,
    reason          text NOT NULL CHECK (length(reason) > 0),
    correlation_id  text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX wallet_status_transitions_wallet_idx ON wallet_status_transitions (wallet_id, occurred_at);
CREATE TRIGGER wallet_status_transitions_immutable BEFORE UPDATE OR DELETE ON wallet_status_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Bind wallets.status to the transition table (same mechanism as 00603).
CREATE TRIGGER wallet_status_transitions_flag AFTER INSERT ON wallet_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('wallet_id', 'to_status', 'wallets');
CREATE CONSTRAINT TRIGGER wallets_require_transition AFTER UPDATE OF status ON wallets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status');

GRANT SELECT, INSERT ON wallet_status_transitions TO cp_app;
GRANT SELECT ON wallet_status_transitions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: wallet authority history is financial evidence and is never dropped
