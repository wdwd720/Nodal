-- +goose Up
-- Three amendments to the advanced kernel (00731), found by the first subsystem built on it
-- (internal/transfers, 00744).
--
-- 1. An economic intent or effect may change status more than once in one transaction.
--
--    00731 bound economic_intents.status and economic_effects.status with the edge binding of
--    00726: the transition row's AFTER INSERT trigger records '<from>><to>' in a transaction-local
--    setting keyed by the entity, and a DEFERRABLE INITIALLY DEFERRED constraint trigger compares
--    it at commit with OLD.status || '>' || NEW.status. The setting holds ONE edge. An operation
--    that walks an intent RECEIVED -> ROUTED -> RISK_APPROVED -> RESERVED -> EXECUTING -> COMPLETED
--    in one transaction -- which is exactly the shape the advanced model prescribes (§6: one
--    db.InTx) -- leaves only the last edge in the setting, and at commit every earlier UPDATE's
--    deferred check compares its own edge with the last one and raises AU001. Transfers worked
--    around it with SET CONSTRAINTS ... IMMEDIATE; every other subsystem would have had to.
--
--    The fix keeps the deferred check and makes the flag ACCUMULATE: the setting holds
--    '|e1|e2|...|' and the check asks whether the row's own edge is among them. Each UPDATE event
--    is checked at commit against its own OLD and NEW, so every step still needs a transition row
--    naming both of its endpoints; a row for one edge still licenses nothing else.
--
--    Why accumulation cannot license a change twice: within one transaction an edge can occur at
--    most once per entity, because both status machines are acyclic (intents: RECEIVED -> ROUTED
--    -> RISK_APPROVED -> [RESERVED ->] EXECUTING -> COMPLETED|FAILED|UNKNOWN, UNKNOWN ->
--    RECONCILED|FAILED, plus REJECTED/CANCELLED exits; effects: PENDING -> APPLIED|UNKNOWN|FAILED,
--    UNKNOWN -> APPLIED|FAILED, APPLIED -> COMPENSATED), and each machine's legality is enforced
--    separately (Go and, for effects, EF001). Taking the same edge twice would need a cycle.
--
--    The single-edge functions of 00726 are left as they are for the tables that use them: their
--    status machines are not all acyclic, and changing them is not this migration's business.
--
-- 2. A capital reservation can name the economic intent it serves.
--
--    asset_reservations.intent_id references trade_intents (00201), so a reservation made for an
--    economic intent could not name it and the link lived only in an idempotency key. The new
--    column economic_intent_id references economic_intents; a reservation names at most one kind
--    of intent; and the link, once written, is frozen (RV001), because which intent a hold
--    serves is its identity, not its state.
--
-- 3. Transfer notifications have kinds of their own.
--
--    TRANSFER_RECEIVED and TRANSFER_REVERSED. Until now a transfer notification had to borrow
--    FUNDING_AVAILABLE / FUNDING_REVERSED, which told a customer their deposit had arrived when a
--    person had sent them simulated value.
--
-- Custom SQLSTATEs: RV001 an asset reservation's economic intent changed after it was written.
-- AU001 (00603) is raised by the accumulated edge check, as by every other transition binding.

-- +goose StatementBegin
CREATE FUNCTION cp_flag_transition_edges() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    entity_col text := TG_ARGV[0];
    from_col   text := TG_ARGV[1];
    to_col     text := TG_ARGV[2];
    label      text := TG_ARGV[3];
    entity_id  text;
    edge       text;
    setting    text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($1).%I::text || ''>'' || ($1).%I::text', entity_col, from_col, to_col)
        INTO entity_id, edge USING NEW;
    setting := 'cp.edges.' || label || '.x' || translate(entity_id, '-', '_');
    -- An unset placeholder reads as NULL before first use in a session and as
    -- '' after a transaction that set it ended; both mean "no edges yet".
    PERFORM set_config(setting, coalesce(nullif(current_setting(setting, true), ''), '|') || edge || '|', true);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_require_transition_edges() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    col     text := TG_ARGV[0];
    label   text := TG_ARGV[1];
    old_val text;
    new_val text;
    flagged text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($2).%I::text', col, col) INTO old_val, new_val USING OLD, NEW;
    IF old_val IS NOT DISTINCT FROM new_val THEN
        RETURN NULL;
    END IF;
    flagged := coalesce(current_setting('cp.edges.' || label || '.x' || translate(NEW.id::text, '-', '_'), true), '');
    IF position('|' || old_val || '>' || new_val || '|' IN flagged) = 0 THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % changed % % -> % without a transition row describing that change',
            TG_TABLE_NAME, NEW.id, col, old_val, new_val USING ERRCODE = 'AU001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION cp_require_transition_edges() IS
    'Edge binding for acyclic status machines that may take several steps in one transaction: every UPDATE event must find its own <from>><to> among the edges its transition rows flagged (00733).';

DROP TRIGGER economic_intent_transitions_flag_edge ON economic_intent_transitions;
DROP TRIGGER economic_intents_require_transition ON economic_intents;
CREATE TRIGGER economic_intent_transitions_flag_edge AFTER INSERT ON economic_intent_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edges('intent_id', 'from_status', 'to_status', 'econ_intent_status');
CREATE CONSTRAINT TRIGGER economic_intents_require_transition AFTER UPDATE OF status ON economic_intents
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edges('status', 'econ_intent_status');

DROP TRIGGER economic_effect_transitions_flag_edge ON economic_effect_transitions;
DROP TRIGGER economic_effects_require_transition ON economic_effects;
CREATE TRIGGER economic_effect_transitions_flag_edge AFTER INSERT ON economic_effect_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edges('effect_id', 'from_status', 'to_status', 'econ_effect_status');
CREATE CONSTRAINT TRIGGER economic_effects_require_transition AFTER UPDATE OF status ON economic_effects
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edges('status', 'econ_effect_status');

-- 2. Reservations for economic intents.
ALTER TABLE asset_reservations ADD COLUMN economic_intent_id uuid REFERENCES economic_intents(id);
ALTER TABLE asset_reservations ADD CONSTRAINT asset_reservations_one_intent_kind
    CHECK (intent_id IS NULL OR economic_intent_id IS NULL);
CREATE INDEX asset_reservations_economic_intent_idx ON asset_reservations (economic_intent_id)
    WHERE economic_intent_id IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION cp_reservation_economic_intent_frozen() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.economic_intent_id IS DISTINCT FROM NEW.economic_intent_id THEN
        RAISE EXCEPTION 'asset reservation % serves economic intent % and cannot be moved to %',
            OLD.id, OLD.economic_intent_id, NEW.economic_intent_id USING ERRCODE = 'RV001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER asset_reservations_economic_intent_frozen BEFORE UPDATE OF economic_intent_id ON asset_reservations
    FOR EACH ROW EXECUTE FUNCTION cp_reservation_economic_intent_frozen();

-- 3. Transfer notification kinds.
ALTER TABLE notifications DROP CONSTRAINT notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check CHECK (kind IN (
    'FUNDING_AVAILABLE','FUNDING_FAILED','FUNDING_REVERSED','TRADE_FILLED','TRADE_FAILED',
    'AGENT_PAUSED','RISK_LIMIT_HIT','SECURITY_SESSION_EVENT','RECONCILIATION_HOLD',
    'TRANSFER_RECEIVED','TRANSFER_REVERSED'));

-- +goose Down
SELECT 1; -- protected: the transition bindings and the reservation link guard financial history
