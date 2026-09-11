-- +goose Up
-- Every audited entity's transition row says where the change started, not only
-- where it ended (F-94).
--
-- 00726 did this for `agents`, because F-78 was reachable there: both promotion
-- CHECKs on agent_lifecycle_transitions open with `from_stage = to_stage OR ...`,
-- so a row claiming the stage had not moved satisfied them and licensed moving
-- it. The remaining sixteen bindings were left alone deliberately, on the
-- grounds that the analogy was not a proof.
--
-- The recount is the proof, and it says two things. The F-78 exploit is NOT
-- reachable on the other tables: agent_lifecycle_transitions is the only
-- transitions table in the schema with a `from_X = to_X OR ...` CHECK, and
-- eleven of the sixteen have no CHECK constraints at all. But the weaker
-- property is present on all of them, and it is the one this migration is
-- about: NOTHING ANYWHERE READS from_status OR from_state. The application role
-- holds INSERT on every transitions table and UPDATE on every entity, so a row
-- recording an origin that never existed commits cleanly, and the trail this
-- system would argue from is whatever the writer said it was.
--
-- A second widening, found in the same recount. 00712 replaced
-- cp_require_transition so the flag ACCUMULATES and the check is membership:
--
--     flagged := 'B' || '|' || 'C'
--     IF NOT (new_state = ANY (string_to_array(flagged, '|'))) THEN ...
--
-- Its header says "Membership is exactly as strong as equality was." It is not.
-- Under equality only the LAST flagged state satisfied the check; under
-- membership ANY of them does. So two transition rows written in one
-- transaction -- A->B and B->C, which is what submitting and launching a native
-- asset in one call writes -- license a single UPDATE that takes the entity
-- straight from A to C, or lands it on B while the trail says C. That is not a
-- privilege escalation, because a transaction that can insert two rows can
-- insert one; it is a defect the binding used to catch and stopped catching.
--
-- Binding the EDGE closes both. The flag records '<from>><to>' and the check
-- asks whether OLD||'>'||NEW is among the edges flagged, so:
--
--   * a row claiming nothing moved cannot license a move (F-78's shape);
--   * a row claiming an origin the entity was never in licenses nothing;
--   * A->B and B->C license exactly A->B and B->C, and not A->C.
--
-- The edge flag accumulates for the same reason 00712 made the destination flag
-- accumulate: a flow that submits and approves in one transaction writes two
-- rows and performs two updates, and both must be licensed. Accumulating EDGES
-- keeps that working without giving back what the accumulation of destinations
-- gave away, because an edge names both ends.
--
-- kill_switches is deliberately not converted. kill_switch_transitions has no
-- from_active column, and needs none: for a two-valued column the destination
-- determines the origin, so the destination form is already edge-complete
-- there. Adding a column to express something already implied would be
-- ceremony.
--
-- capability_gates is converted like the rest even though it is already the
-- best-protected table in the schema -- 00701 leaves cp_app no INSERT on its
-- transitions and no UPDATE on its state, and cp_gate_transition writes
-- from_state from the STORED row. The binding is belt and braces there, and
-- belt and braces that reads only half the row is worth finishing.
--
-- Custom SQLSTATE: AU001, as in 00603, 00690 and 00726.

-- +goose StatementBegin
-- Accumulating edge flag. Replaces 00726's single-value version so a
-- multi-step transaction can license each of its steps.
CREATE OR REPLACE FUNCTION cp_flag_transition_edge() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    entity_col text := TG_ARGV[0];
    from_col   text := TG_ARGV[1];
    to_col     text := TG_ARGV[2];
    label      text := TG_ARGV[3];
    entity_id  text;
    edge       text;
    setting    text;
    current    text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($1).%I::text || ''>'' || ($1).%I::text', entity_col, from_col, to_col)
        INTO entity_id, edge USING NEW;
    -- Setting-name components must be identifiers, so UUIDs are keyed as
    -- x<uuid with underscores>, exactly as in 00603.
    setting := 'cp.edge.' || label || '.x' || translate(entity_id, '-', '_');
    current := current_setting(setting, true);
    IF current IS NULL OR current = '' THEN
        PERFORM set_config(setting, edge, true);
    ELSE
        PERFORM set_config(setting, current || '|' || edge, true);
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Membership over edges. The third argument names the id column, defaulting to
-- `id`, because native_assets is keyed by asset_id (00712's first widening).
CREATE OR REPLACE FUNCTION cp_require_transition_edge() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    col       text := TG_ARGV[0];
    label     text := TG_ARGV[1];
    id_col    text := coalesce(TG_ARGV[2], 'id');
    old_val   text;
    new_val   text;
    entity_id text;
    flagged   text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($2).%I::text', col, col) INTO old_val, new_val USING OLD, NEW;
    IF old_val IS NOT DISTINCT FROM new_val THEN
        RETURN NULL;
    END IF;
    EXECUTE format('SELECT ($1).%I::text', id_col) INTO entity_id USING NEW;
    flagged := current_setting('cp.edge.' || label || '.x' || translate(entity_id, '-', '_'), true);
    IF flagged IS NULL OR NOT (old_val || '>' || new_val = ANY (string_to_array(flagged, '|'))) THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % changed % % -> % without a transition row describing that change',
            TG_TABLE_NAME, entity_id, col, old_val, new_val USING ERRCODE = 'AU001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- The fifteen conversions. Each drops the destination-only pair and creates the
-- edge pair under a label of its own, so two entities' flags cannot collide.

CREATE TRIGGER account_status_transitions_flag_edge AFTER INSERT ON account_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('account_id', 'from_status', 'to_status', 'accounts');
DROP TRIGGER accounts_require_transition ON accounts;
CREATE CONSTRAINT TRIGGER accounts_require_transition AFTER UPDATE OF status ON accounts
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('status', 'accounts');

CREATE TRIGGER admin_action_transitions_flag_edge AFTER INSERT ON admin_action_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('action_id', 'from_status', 'to_status', 'admin_actions');
DROP TRIGGER admin_actions_require_transition ON admin_actions;
CREATE CONSTRAINT TRIGGER admin_actions_require_transition AFTER UPDATE OF status ON admin_actions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('status', 'admin_actions');

CREATE TRIGGER asset_status_transitions_flag_edge AFTER INSERT ON asset_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('asset_id', 'from_status', 'to_status', 'assets');
DROP TRIGGER assets_require_transition ON assets;
CREATE CONSTRAINT TRIGGER assets_require_transition AFTER UPDATE OF status ON assets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('status', 'assets');

CREATE TRIGGER capability_gate_transitions_flag_edge AFTER INSERT ON capability_gate_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('gate_id', 'from_state', 'to_state', 'capability_gates');
DROP TRIGGER capability_gates_require_transition ON capability_gates;
CREATE CONSTRAINT TRIGGER capability_gates_require_transition AFTER UPDATE OF state ON capability_gates
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('state', 'capability_gates');

CREATE TRIGGER credit_funding_transitions_flag_edge AFTER INSERT ON credit_funding_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('funding_id', 'from_state', 'to_state', 'credit_fundings');
DROP TRIGGER credit_fundings_require_transition ON credit_fundings;
CREATE CONSTRAINT TRIGGER credit_fundings_require_transition AFTER UPDATE OF state ON credit_fundings
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('state', 'credit_fundings');

CREATE TRIGGER deposit_transitions_flag_edge AFTER INSERT ON deposit_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('deposit_id', 'from_status', 'to_status', 'deposits');
DROP TRIGGER deposits_require_transition ON deposits;
CREATE CONSTRAINT TRIGGER deposits_require_transition AFTER UPDATE OF status ON deposits
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('status', 'deposits');

CREATE TRIGGER instrument_status_transitions_flag_edge AFTER INSERT ON instrument_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('instrument_id', 'from_status', 'to_status', 'instruments');
DROP TRIGGER instruments_require_transition ON instruments;
CREATE CONSTRAINT TRIGGER instruments_require_transition AFTER UPDATE OF status ON instruments
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('status', 'instruments');

CREATE TRIGGER intent_transitions_flag_edge AFTER INSERT ON intent_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('intent_id', 'from_status', 'to_status', 'trade_intents');
DROP TRIGGER trade_intents_require_transition ON trade_intents;
CREATE CONSTRAINT TRIGGER trade_intents_require_transition AFTER UPDATE OF status ON trade_intents
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('status', 'trade_intents');

CREATE TRIGGER native_asset_transitions_flag_edge AFTER INSERT ON native_asset_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('asset_id', 'from_status', 'to_status', 'native_assets');
DROP TRIGGER native_assets_require_transition ON native_assets;
CREATE CONSTRAINT TRIGGER native_assets_require_transition AFTER UPDATE OF status ON native_assets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'native_assets', 'asset_id');

CREATE TRIGGER native_market_transitions_flag_edge AFTER INSERT ON native_market_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('market_id', 'from_status', 'to_status', 'native_markets');
DROP TRIGGER native_markets_require_transition ON native_markets;
CREATE CONSTRAINT TRIGGER native_markets_require_transition AFTER UPDATE OF status ON native_markets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('status', 'native_markets');

CREATE TRIGGER order_transitions_flag_edge AFTER INSERT ON order_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('order_id', 'from_status', 'to_status', 'orders');
DROP TRIGGER orders_require_transition ON orders;
CREATE CONSTRAINT TRIGGER orders_require_transition AFTER UPDATE OF status ON orders
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('status', 'orders');

CREATE TRIGGER payout_request_transitions_flag_edge AFTER INSERT ON payout_request_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('request_id', 'from_state', 'to_state', 'payout_requests');
DROP TRIGGER payout_requests_require_transition ON payout_requests;
CREATE CONSTRAINT TRIGGER payout_requests_require_transition AFTER UPDATE OF state ON payout_requests
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('state', 'payout_requests');

CREATE TRIGGER reconciliation_transitions_flag_edge AFTER INSERT ON reconciliation_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('record_id', 'from_status', 'to_status', 'reconciliation_records');
DROP TRIGGER reconciliation_records_require_transition ON reconciliation_records;
CREATE CONSTRAINT TRIGGER reconciliation_records_require_transition AFTER UPDATE OF status ON reconciliation_records
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('status', 'reconciliation_records');

CREATE TRIGGER wallet_status_transitions_flag_edge AFTER INSERT ON wallet_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('wallet_id', 'from_status', 'to_status', 'wallets');
DROP TRIGGER wallets_require_transition ON wallets;
CREATE CONSTRAINT TRIGGER wallets_require_transition AFTER UPDATE OF status ON wallets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('status', 'wallets');

CREATE TRIGGER withdrawal_transitions_flag_edge AFTER INSERT ON withdrawal_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('withdrawal_id', 'from_status', 'to_status', 'withdrawals');
DROP TRIGGER withdrawals_require_transition ON withdrawals;
CREATE CONSTRAINT TRIGGER withdrawals_require_transition AFTER UPDATE OF status ON withdrawals
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition_edge('status', 'withdrawals');

COMMENT ON FUNCTION cp_require_transition_edge() IS
    'Binds a column change to a transition row naming BOTH endpoints. cp_require_transition compares the destination alone, so a row claiming an origin the entity was never in licenses a change it does not describe (F-94).';

-- +goose Down
SELECT 1; -- protected: reverting returns every audited entity to a trail whose recorded origin nothing checks
