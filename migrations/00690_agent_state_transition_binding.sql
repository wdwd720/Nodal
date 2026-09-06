-- +goose Up
-- Bind every change of agents.state to an agent_lifecycle_transitions row written in the SAME
-- transaction, using the mechanism migration 00603 established for gates, kill switches, accounts,
-- assets, instruments, deposits, withdrawals, intents, orders, reconciliation records and admin
-- actions. Without this, a bare `UPDATE agents SET state = 'LIVE'` by the application role would
-- move an agent onto real customer capital leaving no evidence of the promotion, its approval or
-- its gate evidence — exactly the "bare gate-state update" gap 00603 closed everywhere else.
--
-- agent_lifecycle_transitions is already immutable (forbid_mutation) and its CHECKs already require
-- hashed evidence for a promotion into SHADOW and beyond, and an approval_id for CANARY, LIMITED and
-- LIVE. Binding the state column to it makes those CHECKs unavoidable rather than merely available:
-- the only way to reach LIVE is to insert a transition row that satisfies them.
--
-- The flag is a transaction-local setting only an INSERT into the transitions table can set, and the
-- application role has no privilege to drop a trigger, so cp_app cannot bypass it.
-- Custom SQLSTATE: AU001 (audit transition required), as in 00603.

CREATE TRIGGER agent_lifecycle_transitions_flag AFTER INSERT ON agent_lifecycle_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('agent_id', 'to_state', 'agents');

CREATE CONSTRAINT TRIGGER agents_require_transition AFTER UPDATE OF state ON agents
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('state');

-- +goose Down
SELECT 1; -- protected: the audit binding of agent lifecycle state is part of financial history semantics
