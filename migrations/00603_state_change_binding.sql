-- +goose Up
-- Bind every state change of an audited entity to a transition row written in the
-- SAME transaction (PART 89 audit completeness, threat model gap "bare gate-state update").
--
-- Mechanism (order-independent within a transaction):
--   * an AFTER INSERT trigger on each *_transitions table records a transaction-local
--     flag  cp.transition.<table>.<entity id> = <to_state>;
--   * a DEFERRED constraint trigger on the entity table fires at COMMIT for every row whose
--     state column changed and raises SQLSTATE AU001 unless the flag equals the new state.
-- The application role cannot bypass this: it has no privilege to drop triggers, and the
-- flag can only be set by inserting an immutable transition row.

-- +goose StatementBegin
CREATE FUNCTION cp_flag_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    entity_col text := TG_ARGV[0];   -- column on the transitions table holding the entity id
    state_col  text := TG_ARGV[1];   -- column holding the new state
    entity_id  text;
    new_state  text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($1).%I::text', entity_col, state_col) INTO entity_id, new_state USING NEW;
    -- Setting-name components must be identifiers: no '-', and not starting with a digit,
    -- so UUIDs are keyed as x<uuid with underscores>.
    PERFORM set_config('cp.transition.' || TG_ARGV[2] || '.x' || translate(entity_id, '-', '_'), new_state, true);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_require_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    state_col text := TG_ARGV[0];
    old_state text;
    new_state text;
    flagged   text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($2).%I::text', state_col, state_col) INTO old_state, new_state USING OLD, NEW;
    IF old_state IS NOT DISTINCT FROM new_state THEN
        RETURN NULL;
    END IF;
    flagged := current_setting('cp.transition.' || TG_TABLE_NAME || '.x' || translate(NEW.id::text, '-', '_'), true);
    IF flagged IS NULL OR flagged <> new_state THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % changed % -> % without a matching transition row in this transaction',
            TG_TABLE_NAME, NEW.id, old_state, new_state USING ERRCODE = 'AU001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- Gates: state
CREATE TRIGGER capability_gate_transitions_flag AFTER INSERT ON capability_gate_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('gate_id', 'to_state', 'capability_gates');
CREATE CONSTRAINT TRIGGER capability_gates_require_transition AFTER UPDATE OF state ON capability_gates
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('state');

-- Kill switches: active (boolean rendered as text 'true'/'false' on both sides)
CREATE TRIGGER kill_switch_transitions_flag AFTER INSERT ON kill_switch_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('switch_id', 'to_active', 'kill_switches');
CREATE CONSTRAINT TRIGGER kill_switches_require_transition AFTER UPDATE OF active ON kill_switches
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('active');

-- Accounts, assets, instruments: status
CREATE TRIGGER account_status_transitions_flag AFTER INSERT ON account_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('account_id', 'to_status', 'accounts');
CREATE CONSTRAINT TRIGGER accounts_require_transition AFTER UPDATE OF status ON accounts
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status');

CREATE TRIGGER asset_status_transitions_flag AFTER INSERT ON asset_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('asset_id', 'to_status', 'assets');
CREATE CONSTRAINT TRIGGER assets_require_transition AFTER UPDATE OF status ON assets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status');

CREATE TRIGGER instrument_status_transitions_flag AFTER INSERT ON instrument_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('instrument_id', 'to_status', 'instruments');
CREATE CONSTRAINT TRIGGER instruments_require_transition AFTER UPDATE OF status ON instruments
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status');

-- Funding, withdrawals, intents, orders, reconciliation, admin actions: status
CREATE TRIGGER deposit_transitions_flag AFTER INSERT ON deposit_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('deposit_id', 'to_status', 'deposits');
CREATE CONSTRAINT TRIGGER deposits_require_transition AFTER UPDATE OF status ON deposits
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status');

CREATE TRIGGER withdrawal_transitions_flag AFTER INSERT ON withdrawal_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('withdrawal_id', 'to_status', 'withdrawals');
CREATE CONSTRAINT TRIGGER withdrawals_require_transition AFTER UPDATE OF status ON withdrawals
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status');

CREATE TRIGGER intent_transitions_flag AFTER INSERT ON intent_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('intent_id', 'to_status', 'trade_intents');
CREATE CONSTRAINT TRIGGER trade_intents_require_transition AFTER UPDATE OF status ON trade_intents
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status');

CREATE TRIGGER order_transitions_flag AFTER INSERT ON order_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('order_id', 'to_status', 'orders');
CREATE CONSTRAINT TRIGGER orders_require_transition AFTER UPDATE OF status ON orders
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status');

CREATE TRIGGER reconciliation_transitions_flag AFTER INSERT ON reconciliation_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('record_id', 'to_status', 'reconciliation_records');
CREATE CONSTRAINT TRIGGER reconciliation_records_require_transition AFTER UPDATE OF status ON reconciliation_records
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status');

CREATE TRIGGER admin_action_transitions_flag AFTER INSERT ON admin_action_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('action_id', 'to_status', 'admin_actions');
CREATE CONSTRAINT TRIGGER admin_actions_require_transition AFTER UPDATE OF status ON admin_actions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status');

-- Schema corrections reported during implementation.
ALTER TABLE capability_gates ALTER COLUMN proposed_by_user_id TYPE text USING proposed_by_user_id::text;
ALTER TABLE kill_switches ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();

-- +goose Down
SELECT 1; -- protected: audit-binding triggers are part of financial history semantics
