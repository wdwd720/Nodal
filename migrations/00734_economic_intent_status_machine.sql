-- +goose Up
-- The economic intent status machine, enforced in SQL.
--
-- 00731 enforced the economic EFFECT status machine in SQL (EF001) but left the
-- INTENT machine to Go: the database accepted any status change an
-- application-role writer licensed with a transition row. The advanced review
-- showed it: a COMPLETED intent moved back to RECEIVED with one transition row.
-- It also exposed a second consequence. 00733 lets an intent's transition flags
-- accumulate within a transaction and argued that this cannot license a change
-- twice because the machine is acyclic -- an argument that held only for code
-- that went through econ.IntentStore. With this migration the database itself
-- refuses every edge the machine does not have, so the machine is acyclic for
-- every writer and the 00733 argument holds unconditionally.
--
-- cp_intent_transition_legal is exactly econ.CanTransitionIntent;
-- TestIntegration_SQLAndGoAgreeOnIntentTransitions compares every pair.
--
-- Custom SQLSTATEs: EI001 an economic intent status change, or a transition row
-- describing one, that the status machine does not allow.

-- +goose StatementBegin
CREATE FUNCTION cp_intent_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('RECEIVED','ROUTED'), ('RECEIVED','REJECTED'), ('RECEIVED','CANCELLED'),
        ('ROUTED','RISK_APPROVED'), ('ROUTED','REJECTED'), ('ROUTED','CANCELLED'),
        ('RISK_APPROVED','RESERVED'), ('RISK_APPROVED','EXECUTING'), ('RISK_APPROVED','REJECTED'), ('RISK_APPROVED','CANCELLED'),
        ('RESERVED','EXECUTING'), ('RESERVED','REJECTED'), ('RESERVED','CANCELLED'),
        ('EXECUTING','COMPLETED'), ('EXECUTING','FAILED'), ('EXECUTING','UNKNOWN'),
        ('UNKNOWN','RECONCILED'), ('UNKNOWN','FAILED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_intent_status_legal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'economic_intents' THEN
        IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_intent_transition_legal(OLD.status, NEW.status) THEN
            RAISE EXCEPTION 'economic intent % cannot move % -> %', NEW.id, OLD.status, NEW.status
                USING ERRCODE = 'EI001';
        END IF;
    ELSIF NOT cp_intent_transition_legal(NEW.from_status, NEW.to_status) THEN
        RAISE EXCEPTION 'a transition row cannot describe economic intent % moving % -> %', NEW.intent_id, NEW.from_status, NEW.to_status
            USING ERRCODE = 'EI001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER economic_intents_status_legal BEFORE UPDATE OF status ON economic_intents
    FOR EACH ROW EXECUTE FUNCTION cp_intent_status_legal();
CREATE TRIGGER economic_intent_transitions_legal BEFORE INSERT ON economic_intent_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_intent_status_legal();

-- +goose Down
SELECT 1; -- protected: the status machine guards financial history
