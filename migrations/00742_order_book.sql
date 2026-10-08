-- +goose Up
-- The internal order book and the paper venue (internal/orders; ADVANCED_FINANCIAL_MODEL.md §2 ORDERS,
-- ADR-0020). The V1 table "orders" (real spot orders) is a different thing and is not touched: every
-- table here is prefixed book_ or paper_.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A book is simulated. book_states carries venue_label SIMULATED_INTERNAL_BOOK and value_domain
--      SIMULATED with CHECKs that admit no other value, and paper_fills carries SIMULATED_PAPER_VENUE.
--   2. The command journal of a book is gap-free and hash-chained: command seq N+1 can only be written
--      when the book's cursor is at N, and it must name the state hash the cursor holds (OB001). The
--      cursor (last command seq, event seq, trade seq, state hash) can only move to a command row that
--      states exactly that cursor (OB002). A snapshot's hash is sha256 of its bytes (CHECK) and equals
--      the hash the journal recorded after the command it was taken at (OB005).
--   3. An order never overfills: quantity = filled + remaining + cancelled for every order that reached
--      the book, fills and cancellations never shrink, a terminal order never changes again, and the
--      status machine is enforced in SQL (OB003, OB004) with every status change bound to a transition
--      row naming both endpoints (AU001, migration 00726).
--   4. A trade is never a self-trade (maker owner <> taker owner) and never crosses a HOUSE book with a
--      CUSTOMER unless it names the version of an explicit crossing rule that allowed it.
--   5. Crossing rules are append-only and consecutively versioned (OB006); house reservations follow a
--      legal status machine with frozen identity (OB007); a book's identity, terms and fee schedules
--      are frozen once opened (OB008) and its status machine is enforced (OB009).
--   6. Every history table (commands, events, trades, snapshots, transitions, crossing rules and
--      decisions, paper fills) is append-only.
--
-- Custom SQLSTATEs: OB001 book command journal is not the next link of its chain, OB002 book cursor
-- does not match its journal, OB003 illegal book order status transition or change to a terminal order,
-- OB004 book order identity changed or its fills/cancellations shrank, OB005 snapshot hash differs from
-- the journal, OB006 crossing rule version not the next one, OB007 illegal house reservation change,
-- OB008 book identity changed, OB009 illegal book status transition.

-- ---------------------------------------------------------------------------
-- 1. Books
-- ---------------------------------------------------------------------------

CREATE TABLE book_states (
    id                     uuid PRIMARY KEY,
    instrument_id          uuid NOT NULL UNIQUE REFERENCES instruments(id),
    -- The matching engine's Config.Instrument: the instrument id in canonical text form.
    engine_instrument      text NOT NULL UNIQUE CHECK (length(engine_instrument) BETWEEN 1 AND 128),
    venue_label            text NOT NULL DEFAULT 'SIMULATED_INTERNAL_BOOK' CHECK (venue_label = 'SIMULATED_INTERNAL_BOOK'),
    value_domain           text NOT NULL DEFAULT 'SIMULATED' CHECK (value_domain = 'SIMULATED'),
    mode                   text NOT NULL CHECK (mode IN ('PAPER','SANDBOX')),
    status                 text NOT NULL CHECK (status IN ('OPEN','HALTED','CLOSED')),
    instrument_kind        text NOT NULL CHECK (instrument_kind IN ('SPOT_PAIR','EQUITY','EVENT_OUTCOME')),
    base_asset_id          uuid NOT NULL REFERENCES assets(id),
    quote_asset_id         uuid NOT NULL REFERENCES assets(id),
    base_decimals          smallint NOT NULL CHECK (base_decimals BETWEEN 0 AND 18),
    quote_decimals         smallint NOT NULL CHECK (quote_decimals BETWEEN 0 AND 18),
    price_scale            integer NOT NULL CHECK (price_scale BETWEEN 0 AND 18),
    tick_size              numeric(38,0) NOT NULL CHECK (tick_size > 0),
    lot_size               numeric(38,0) NOT NULL CHECK (lot_size > 0),
    min_quantity           numeric(38,0) NOT NULL CHECK (min_quantity >= 0),
    -- Quote-asset base units of one tick times one lot. A positive integer by construction, so every
    -- trade notional is exact: notional = price_ticks x qty_lots x notional_per_tick_lot.
    notional_per_tick_lot  numeric(38,0) NOT NULL CHECK (notional_per_tick_lot > 0),
    max_price_ticks        bigint NOT NULL CHECK (max_price_ticks BETWEEN 1 AND 1000000000000000),
    terms_source           text NOT NULL CHECK (terms_source IN ('CANONICAL','BOOK_SPOT')),
    terms_version          integer CHECK (terms_version IS NULL OR terms_version >= 1),
    terms                  jsonb NOT NULL,
    terms_hash             bytea NOT NULL CHECK (length(terms_hash) = 32),
    -- The fees.Schedule canonical form of the maker and taker schedules; the hash a quote records is
    -- the hex sha256 of exactly these bytes, and the database checks it.
    maker_schedule         bytea NOT NULL,
    maker_schedule_hash    text NOT NULL,
    taker_schedule         bytea NOT NULL,
    taker_schedule_hash    text NOT NULL,
    snapshot_interval      integer NOT NULL CHECK (snapshot_interval BETWEEN 1 AND 100000),
    last_command_seq       bigint NOT NULL DEFAULT 0 CHECK (last_command_seq >= 0),
    last_event_seq         bigint NOT NULL DEFAULT 0 CHECK (last_event_seq >= last_command_seq),
    last_trade_seq         bigint NOT NULL DEFAULT 0 CHECK (last_trade_seq >= 0 AND last_trade_seq <= last_event_seq),
    genesis_state_hash     bytea NOT NULL CHECK (length(genesis_state_hash) = 32),
    state_hash             bytea NOT NULL CHECK (length(state_hash) = 32),
    created_by_actor_type  text NOT NULL CHECK (created_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    created_by_actor_id    text NOT NULL CHECK (length(created_by_actor_id) > 0),
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CHECK (base_asset_id <> quote_asset_id),
    CHECK ((terms_source = 'CANONICAL') = (terms_version IS NOT NULL)),
    CONSTRAINT book_states_maker_schedule_hash CHECK (encode(sha256(maker_schedule), 'hex') = maker_schedule_hash),
    CONSTRAINT book_states_taker_schedule_hash CHECK (encode(sha256(taker_schedule), 'hex') = taker_schedule_hash),
    CONSTRAINT book_states_genesis CHECK (last_command_seq > 0 OR (state_hash = genesis_state_hash AND last_event_seq = 0 AND last_trade_seq = 0))
);
CREATE TRIGGER book_states_updated_at BEFORE UPDATE ON book_states FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_book_status_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (('OPEN','HALTED'), ('OPEN','CLOSED'), ('HALTED','OPEN'), ('HALTED','CLOSED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_book_state_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cmd record;
BEGIN
    IF (NEW.id, NEW.instrument_id, NEW.engine_instrument, NEW.venue_label, NEW.value_domain, NEW.mode, NEW.instrument_kind,
        NEW.base_asset_id, NEW.quote_asset_id, NEW.base_decimals, NEW.quote_decimals, NEW.price_scale, NEW.tick_size,
        NEW.lot_size, NEW.min_quantity, NEW.notional_per_tick_lot, NEW.max_price_ticks, NEW.terms_source, NEW.terms_version,
        NEW.terms, NEW.terms_hash, NEW.maker_schedule, NEW.maker_schedule_hash, NEW.taker_schedule, NEW.taker_schedule_hash,
        NEW.snapshot_interval, NEW.genesis_state_hash, NEW.created_by_actor_type, NEW.created_by_actor_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.instrument_id, OLD.engine_instrument, OLD.venue_label, OLD.value_domain, OLD.mode, OLD.instrument_kind,
        OLD.base_asset_id, OLD.quote_asset_id, OLD.base_decimals, OLD.quote_decimals, OLD.price_scale, OLD.tick_size,
        OLD.lot_size, OLD.min_quantity, OLD.notional_per_tick_lot, OLD.max_price_ticks, OLD.terms_source, OLD.terms_version,
        OLD.terms, OLD.terms_hash, OLD.maker_schedule, OLD.maker_schedule_hash, OLD.taker_schedule, OLD.taker_schedule_hash,
        OLD.snapshot_interval, OLD.genesis_state_hash, OLD.created_by_actor_type, OLD.created_by_actor_id, OLD.created_at) THEN
        RAISE EXCEPTION 'BOOK_IDENTITY_FROZEN: book % may change only its status and its journal cursor', OLD.id
            USING ERRCODE = 'OB008';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_book_status_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'BOOK_STATUS_ILLEGAL: book % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'OB009';
    END IF;
    IF NEW.last_command_seq = OLD.last_command_seq THEN
        IF (NEW.state_hash, NEW.last_event_seq, NEW.last_trade_seq) IS DISTINCT FROM
           (OLD.state_hash, OLD.last_event_seq, OLD.last_trade_seq) THEN
            RAISE EXCEPTION 'BOOK_CURSOR_UNBOUND: book % changed its state without a journalled command', OLD.id
                USING ERRCODE = 'OB002';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.last_command_seq <> OLD.last_command_seq + 1 THEN
        RAISE EXCEPTION 'BOOK_CURSOR_UNBOUND: book % cursor must advance by exactly one command (% -> %)',
            OLD.id, OLD.last_command_seq, NEW.last_command_seq USING ERRCODE = 'OB002';
    END IF;
    SELECT state_hash_after, last_event_seq, trade_seq_after INTO cmd
      FROM book_commands WHERE book_id = NEW.id AND seq = NEW.last_command_seq;
    IF NOT FOUND OR cmd.state_hash_after <> NEW.state_hash OR cmd.last_event_seq <> NEW.last_event_seq
       OR cmd.trade_seq_after <> NEW.last_trade_seq THEN
        RAISE EXCEPTION 'BOOK_CURSOR_UNBOUND: book % cursor % does not match its journalled command', OLD.id, NEW.last_command_seq
            USING ERRCODE = 'OB002';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_book_state_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER book_states_guard BEFORE UPDATE ON book_states FOR EACH ROW EXECUTE FUNCTION cp_book_state_guard();

CREATE TABLE book_state_transitions (
    id              uuid PRIMARY KEY,
    book_id         uuid NOT NULL REFERENCES book_states(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id        text NOT NULL,
    reason          text NOT NULL CHECK (length(reason) > 0),
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX book_state_transitions_idx ON book_state_transitions (book_id, occurred_at);
CREATE TRIGGER book_state_transitions_immutable BEFORE UPDATE OR DELETE ON book_state_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER book_state_transitions_flag_edge AFTER INSERT ON book_state_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('book_id', 'from_status', 'to_status', 'book_state_status');
CREATE CONSTRAINT TRIGGER book_states_require_transition AFTER UPDATE OF status ON book_states
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'book_state_status');

-- ---------------------------------------------------------------------------
-- 2. Orders
-- ---------------------------------------------------------------------------

CREATE TABLE book_orders (
    id                     uuid PRIMARY KEY,
    book_id                uuid NOT NULL REFERENCES book_states(id),
    instrument_id          uuid NOT NULL REFERENCES instruments(id),
    owner_type             text NOT NULL CHECK (owner_type IN ('CUSTOMER','HOUSE')),
    owner_id               uuid NOT NULL,
    account_id             uuid REFERENCES accounts(id),
    house_account_id       uuid REFERENCES house_accounts(id),
    -- A customer order is the execution of exactly one economic intent; its id is the intent's id.
    intent_id              uuid UNIQUE REFERENCES economic_intents(id),
    idempotency_key        text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    request_hash           bytea NOT NULL CHECK (length(request_hash) = 32),
    client_order_id        text CHECK (client_order_id IS NULL OR length(client_order_id) BETWEEN 1 AND 128),
    side                   text NOT NULL CHECK (side IN ('BUY','SELL')),
    order_type             text NOT NULL CHECK (order_type IN ('LIMIT','MARKET')),
    time_in_force          text NOT NULL CHECK (time_in_force IN ('GTC','IOC','FOK')),
    stp_mode               text NOT NULL CHECK (stp_mode IN ('CANCEL_TAKER','CANCEL_MAKER','CANCEL_BOTH')),
    liquidity_intent       text NOT NULL CHECK (liquidity_intent IN ('TAKE_LIQUIDITY','PROVIDE_LIQUIDITY')),
    post_only              boolean NOT NULL,
    reduce_only            boolean NOT NULL,
    -- A strategy-originated order pays the schedule's strategy fee, disclosed per fill.
    strategy_originated    boolean NOT NULL DEFAULT false,
    mode                   text NOT NULL CHECK (mode IN ('PAPER','SANDBOX')),
    -- The limit price, or a market order's protection price, which this book always requires.
    price_ticks            bigint NOT NULL CHECK (price_ticks BETWEEN 1 AND 1000000000000000),
    qty_lots               bigint NOT NULL CHECK (qty_lots BETWEEN 1 AND 1000000000000000),
    filled_lots            bigint NOT NULL DEFAULT 0 CHECK (filled_lots >= 0),
    remaining_lots         bigint NOT NULL CHECK (remaining_lots >= 0),
    cancelled_lots         bigint NOT NULL DEFAULT 0 CHECK (cancelled_lots >= 0),
    reduce_only_limit_lots bigint NOT NULL DEFAULT 0 CHECK (reduce_only_limit_lots >= 0),
    status                 text NOT NULL CHECK (status IN ('PENDING','OPEN','PARTIALLY_FILLED','FILLED','CANCELLED','REJECTED')),
    reject_reason          text CHECK (reject_reason IS NULL OR length(reject_reason) BETWEEN 1 AND 128),
    cancel_reason          text CHECK (cancel_reason IS NULL OR length(cancel_reason) BETWEEN 1 AND 128),
    reservation_id         uuid,
    accepted_command_seq   bigint CHECK (accepted_command_seq IS NULL OR accepted_command_seq >= 1),
    last_command_seq       bigint CHECK (last_command_seq IS NULL OR last_command_seq >= 1),
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id, idempotency_key),
    CONSTRAINT book_orders_customer_shape CHECK ((owner_type = 'CUSTOMER') =
        (account_id IS NOT NULL AND account_id = owner_id AND intent_id IS NOT NULL AND intent_id = id AND house_account_id IS NULL)),
    CONSTRAINT book_orders_house_shape CHECK ((owner_type = 'HOUSE') =
        (house_account_id IS NOT NULL AND house_account_id = owner_id AND intent_id IS NULL AND account_id IS NULL)),
    -- No overfill: everything that reached the book is filled, working or cancelled, exactly.
    CONSTRAINT book_orders_quantity_conserved CHECK (
        status IN ('PENDING','REJECTED') OR qty_lots = filled_lots + remaining_lots + cancelled_lots),
    CONSTRAINT book_orders_pending_shape CHECK (
        status <> 'PENDING' OR (filled_lots = 0 AND cancelled_lots = 0 AND remaining_lots = qty_lots)),
    CONSTRAINT book_orders_rejected_shape CHECK (
        status <> 'REJECTED' OR (filled_lots = 0 AND remaining_lots = 0 AND cancelled_lots = 0)),
    CONSTRAINT book_orders_open_shape CHECK (status <> 'OPEN' OR (filled_lots = 0 AND remaining_lots > 0)),
    CONSTRAINT book_orders_partial_shape CHECK (status <> 'PARTIALLY_FILLED' OR (filled_lots > 0 AND remaining_lots > 0)),
    CONSTRAINT book_orders_filled_shape CHECK (status <> 'FILLED' OR (filled_lots > 0 AND remaining_lots = 0)),
    CONSTRAINT book_orders_cancelled_shape CHECK (status <> 'CANCELLED' OR (remaining_lots = 0 AND cancel_reason IS NOT NULL)),
    CONSTRAINT book_orders_reject_reason CHECK ((status = 'REJECTED') = (reject_reason IS NOT NULL)),
    -- Spot has no short position: reduce-only is a sell capped at the seller's holdings.
    CONSTRAINT book_orders_reduce_only_sell CHECK (NOT reduce_only OR side = 'SELL'),
    CONSTRAINT book_orders_reduce_only_limit CHECK (reduce_only OR reduce_only_limit_lots = 0),
    CONSTRAINT book_orders_market_tif CHECK (order_type = 'LIMIT' OR time_in_force IN ('IOC','FOK')),
    CONSTRAINT book_orders_post_only CHECK (NOT post_only OR (order_type = 'LIMIT' AND time_in_force = 'GTC'))
);
-- A client order id is the owner's own reference, unique per owner among orders that were not rejected
-- (a rejected order does not consume it, exactly as in the engine).
CREATE UNIQUE INDEX book_orders_owner_client_order_id ON book_orders (owner_id, client_order_id)
    WHERE client_order_id IS NOT NULL AND status <> 'REJECTED';
CREATE INDEX book_orders_resting_idx ON book_orders (book_id, side, price_ticks) WHERE status IN ('OPEN','PARTIALLY_FILLED');
CREATE INDEX book_orders_owner_idx ON book_orders (owner_id, created_at DESC);
CREATE TRIGGER book_orders_updated_at BEFORE UPDATE ON book_orders FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_book_order_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('PENDING','OPEN'), ('PENDING','PARTIALLY_FILLED'), ('PENDING','FILLED'), ('PENDING','CANCELLED'), ('PENDING','REJECTED'),
        ('OPEN','PARTIALLY_FILLED'), ('OPEN','FILLED'), ('OPEN','CANCELLED'),
        ('PARTIALLY_FILLED','FILLED'), ('PARTIALLY_FILLED','CANCELLED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_book_order_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.book_id, NEW.instrument_id, NEW.owner_type, NEW.owner_id, NEW.account_id, NEW.house_account_id,
        NEW.intent_id, NEW.idempotency_key, NEW.request_hash, NEW.client_order_id, NEW.side, NEW.order_type,
        NEW.time_in_force, NEW.stp_mode, NEW.liquidity_intent, NEW.post_only, NEW.reduce_only, NEW.strategy_originated,
        NEW.mode, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.book_id, OLD.instrument_id, OLD.owner_type, OLD.owner_id, OLD.account_id, OLD.house_account_id,
        OLD.intent_id, OLD.idempotency_key, OLD.request_hash, OLD.client_order_id, OLD.side, OLD.order_type,
        OLD.time_in_force, OLD.stp_mode, OLD.liquidity_intent, OLD.post_only, OLD.reduce_only, OLD.strategy_originated,
        OLD.mode, OLD.created_at) THEN
        RAISE EXCEPTION 'BOOK_ORDER_IDENTITY_FROZEN: book order % may change only its state', OLD.id USING ERRCODE = 'OB004';
    END IF;
    IF OLD.status IN ('FILLED','CANCELLED','REJECTED') THEN
        RAISE EXCEPTION 'BOOK_ORDER_TERMINAL: book order % is % and never changes again', OLD.id, OLD.status
            USING ERRCODE = 'OB003';
    END IF;
    IF OLD.status <> 'PENDING' AND (NEW.filled_lots < OLD.filled_lots OR NEW.cancelled_lots < OLD.cancelled_lots) THEN
        RAISE EXCEPTION 'BOOK_ORDER_IDENTITY_FROZEN: book order % fills and cancellations never shrink', OLD.id
            USING ERRCODE = 'OB004';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_book_order_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'BOOK_ORDER_TRANSITION_ILLEGAL: book order % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'OB003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_book_order_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER book_orders_guard BEFORE UPDATE ON book_orders FOR EACH ROW EXECUTE FUNCTION cp_book_order_guard();

CREATE TABLE book_order_transitions (
    id              uuid PRIMARY KEY,
    order_id        uuid NOT NULL REFERENCES book_orders(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    command_seq     bigint CHECK (command_seq IS NULL OR command_seq >= 1),
    reason          text NOT NULL CHECK (length(reason) > 0),
    actor_type      text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id        text NOT NULL,
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX book_order_transitions_idx ON book_order_transitions (order_id, occurred_at);
CREATE TRIGGER book_order_transitions_immutable BEFORE UPDATE OR DELETE ON book_order_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER book_order_transitions_flag_edge AFTER INSERT ON book_order_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('order_id', 'from_status', 'to_status', 'book_order_status');
CREATE CONSTRAINT TRIGGER book_orders_require_transition AFTER UPDATE OF status ON book_orders
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'book_order_status');

-- ---------------------------------------------------------------------------
-- 3. The command journal, its events, trades and snapshots
-- ---------------------------------------------------------------------------

CREATE TABLE book_commands (
    id                 uuid PRIMARY KEY,
    book_id            uuid NOT NULL REFERENCES book_states(id),
    seq                bigint NOT NULL CHECK (seq >= 1),
    kind               text NOT NULL CHECK (kind IN ('NEW_ORDER','CANCEL','REPLACE')),
    order_id           uuid NOT NULL,
    -- The canonical encoding of the matching command, exactly as it was applied. Replaying these bytes
    -- in seq order reproduces the book.
    command            bytea NOT NULL CHECK (length(command) BETWEEN 2 AND 8192),
    command_sha256     bytea NOT NULL,
    timestamp_unix_ns  bigint NOT NULL,
    rejected           boolean NOT NULL,
    reject_reason      text,
    first_event_seq    bigint NOT NULL CHECK (first_event_seq >= 1),
    last_event_seq     bigint NOT NULL,
    trade_seq_after    bigint NOT NULL CHECK (trade_seq_after >= 0),
    prev_state_hash    bytea NOT NULL CHECK (length(prev_state_hash) = 32),
    state_hash_after   bytea NOT NULL CHECK (length(state_hash_after) = 32),
    actor_type         text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id           text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (book_id, seq),
    CONSTRAINT book_commands_hash CHECK (command_sha256 = sha256(command)),
    CONSTRAINT book_commands_events CHECK (last_event_seq >= first_event_seq),
    CONSTRAINT book_commands_rejection CHECK (rejected = (reject_reason IS NOT NULL)),
    -- A rejected command emits exactly one event.
    CONSTRAINT book_commands_rejection_is_one_event CHECK (NOT rejected OR first_event_seq = last_event_seq),
    -- Every command, a rejection included, advances the command sequence, which is part of the state.
    CONSTRAINT book_commands_state_advances CHECK (state_hash_after <> prev_state_hash)
);
CREATE INDEX book_commands_order_idx ON book_commands (order_id);
CREATE TRIGGER book_commands_immutable BEFORE UPDATE OR DELETE ON book_commands
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_book_command_is_next() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cur record;
BEGIN
    SELECT last_command_seq, last_event_seq, last_trade_seq, state_hash INTO cur FROM book_states WHERE id = NEW.book_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'BOOK_JOURNAL_BROKEN: command for unknown book %', NEW.book_id USING ERRCODE = 'OB001';
    END IF;
    IF NEW.seq <> cur.last_command_seq + 1 THEN
        RAISE EXCEPTION 'BOOK_JOURNAL_BROKEN: book % command % is not the next after %', NEW.book_id, NEW.seq, cur.last_command_seq
            USING ERRCODE = 'OB001';
    END IF;
    IF NEW.prev_state_hash <> cur.state_hash THEN
        RAISE EXCEPTION 'BOOK_JOURNAL_BROKEN: book % command % does not continue the recorded state', NEW.book_id, NEW.seq
            USING ERRCODE = 'OB001';
    END IF;
    IF NEW.first_event_seq <> cur.last_event_seq + 1 OR NEW.trade_seq_after < cur.last_trade_seq THEN
        RAISE EXCEPTION 'BOOK_JOURNAL_BROKEN: book % command % does not continue the event and trade sequences', NEW.book_id, NEW.seq
            USING ERRCODE = 'OB001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_book_command_is_next() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER book_commands_is_next BEFORE INSERT ON book_commands FOR EACH ROW EXECUTE FUNCTION cp_book_command_is_next();

CREATE TABLE book_events (
    id            uuid PRIMARY KEY,
    book_id       uuid NOT NULL REFERENCES book_states(id),
    seq           bigint NOT NULL CHECK (seq >= 1),
    command_seq   bigint NOT NULL,
    event_type    text NOT NULL CHECK (event_type IN ('ACCEPTED','REJECTED','RESTING','TRADE','PARTIAL_FILL','FULL_FILL',
                                                      'CANCELLED','REPLACED','BOOK_DELTA')),
    order_id      text,
    owner_id      text,
    trade_seq     bigint CHECK (trade_seq IS NULL OR trade_seq >= 1),
    -- The canonical encoding of the matching event.
    event         bytea NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (book_id, seq),
    FOREIGN KEY (book_id, command_seq) REFERENCES book_commands (book_id, seq)
);
CREATE INDEX book_events_order_idx ON book_events (order_id, seq) WHERE order_id IS NOT NULL;
CREATE TRIGGER book_events_immutable BEFORE UPDATE OR DELETE ON book_events
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE book_trades (
    id                      uuid PRIMARY KEY,
    book_id                 uuid NOT NULL REFERENCES book_states(id),
    instrument_id           uuid NOT NULL REFERENCES instruments(id),
    trade_seq               bigint NOT NULL CHECK (trade_seq >= 1),
    command_seq             bigint NOT NULL,
    event_seq               bigint NOT NULL,
    price_ticks             bigint NOT NULL CHECK (price_ticks >= 1),
    qty_lots                bigint NOT NULL CHECK (qty_lots >= 1),
    maker_order_id          uuid NOT NULL REFERENCES book_orders(id),
    taker_order_id          uuid NOT NULL REFERENCES book_orders(id),
    maker_owner_type        text NOT NULL CHECK (maker_owner_type IN ('CUSTOMER','HOUSE')),
    maker_owner_id          uuid NOT NULL,
    taker_owner_type        text NOT NULL CHECK (taker_owner_type IN ('CUSTOMER','HOUSE')),
    taker_owner_id          uuid NOT NULL,
    aggressor_side          text NOT NULL CHECK (aggressor_side IN ('BUY','SELL')),
    base_asset_id           uuid NOT NULL REFERENCES assets(id),
    quote_asset_id          uuid NOT NULL REFERENCES assets(id),
    base_quantity           numeric(38,0) NOT NULL CHECK (base_quantity > 0),
    quote_notional          numeric(38,0) NOT NULL CHECK (quote_notional > 0),
    maker_fee               numeric(38,0) NOT NULL CHECK (maker_fee >= 0),
    taker_fee               numeric(38,0) NOT NULL CHECK (taker_fee >= 0),
    maker_schedule_hash     text NOT NULL,
    taker_schedule_hash     text NOT NULL,
    -- 0 when maker and taker are the same kind of owner; otherwise the version of the crossing rule
    -- that allowed a HOUSE book to trade with a CUSTOMER.
    crossing_rule_version   integer NOT NULL CHECK (crossing_rule_version >= 0),
    journal_transaction_id  uuid NOT NULL UNIQUE REFERENCES journal_transactions(id),
    effect_id               uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    evidence_ref            text NOT NULL,
    evidence_sha256         bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (book_id, trade_seq),
    FOREIGN KEY (book_id, command_seq) REFERENCES book_commands (book_id, seq),
    CONSTRAINT book_trades_two_orders CHECK (maker_order_id <> taker_order_id),
    -- Self-trade prevention, in SQL as well as in the engine.
    CONSTRAINT book_trades_no_self_trade CHECK (maker_owner_id <> taker_owner_id),
    -- No house book trades with a customer unless an explicit, versioned crossing rule allowed it.
    CONSTRAINT book_trades_house_crossing_needs_rule CHECK (
        maker_owner_type = taker_owner_type OR crossing_rule_version >= 1)
);
CREATE INDEX book_trades_maker_idx ON book_trades (maker_order_id);
CREATE INDEX book_trades_taker_idx ON book_trades (taker_order_id);
CREATE TRIGGER book_trades_immutable BEFORE UPDATE OR DELETE ON book_trades
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE book_snapshots (
    id            uuid PRIMARY KEY,
    book_id       uuid NOT NULL REFERENCES book_states(id),
    command_seq   bigint NOT NULL CHECK (command_seq >= 1),
    snapshot      bytea NOT NULL,
    state_hash    bytea NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (book_id, command_seq),
    FOREIGN KEY (book_id, command_seq) REFERENCES book_commands (book_id, seq),
    CONSTRAINT book_snapshots_hash CHECK (state_hash = sha256(snapshot))
);
CREATE TRIGGER book_snapshots_immutable BEFORE UPDATE OR DELETE ON book_snapshots
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_book_snapshot_matches_journal() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    want bytea;
BEGIN
    SELECT state_hash_after INTO want FROM book_commands WHERE book_id = NEW.book_id AND seq = NEW.command_seq;
    IF NOT FOUND OR want <> NEW.state_hash THEN
        RAISE EXCEPTION 'BOOK_SNAPSHOT_DIVERGES: book % snapshot at % is not the state the journal recorded', NEW.book_id, NEW.command_seq
            USING ERRCODE = 'OB005';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_book_snapshot_matches_journal() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER book_snapshots_match_journal BEFORE INSERT ON book_snapshots
    FOR EACH ROW EXECUTE FUNCTION cp_book_snapshot_matches_journal();

-- ---------------------------------------------------------------------------
-- 4. House-versus-customer crossing
-- ---------------------------------------------------------------------------

CREATE TABLE book_crossing_rules (
    id                             uuid PRIMARY KEY,
    book_id                        uuid NOT NULL REFERENCES book_states(id),
    version                        integer NOT NULL CHECK (version >= 1),
    allow_house_customer_crossing  boolean NOT NULL,
    -- A crossing rule can only ever apply to simulated value.
    value_domain                   text NOT NULL DEFAULT 'SIMULATED' CHECK (value_domain = 'SIMULATED'),
    reason                         text NOT NULL CHECK (length(reason) > 0),
    actor_type                     text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id                       text NOT NULL,
    created_at                     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (book_id, version)
);
CREATE TRIGGER book_crossing_rules_immutable BEFORE UPDATE OR DELETE ON book_crossing_rules
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_book_crossing_rule_is_next() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    latest integer;
BEGIN
    SELECT max(version) INTO latest FROM book_crossing_rules WHERE book_id = NEW.book_id;
    IF NEW.version <> coalesce(latest, 0) + 1 THEN
        RAISE EXCEPTION 'CROSSING_RULE_VERSION_GAP: book % crossing rule % is not the next after %', NEW.book_id, NEW.version, coalesce(latest, 0)
            USING ERRCODE = 'OB006';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_book_crossing_rule_is_next() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER book_crossing_rules_consecutive BEFORE INSERT ON book_crossing_rules
    FOR EACH ROW EXECUTE FUNCTION cp_book_crossing_rule_is_next();

CREATE TABLE book_crossing_decisions (
    id                uuid PRIMARY KEY,
    book_id           uuid NOT NULL REFERENCES book_states(id),
    order_id          uuid NOT NULL,
    command_kind      text NOT NULL CHECK (command_kind IN ('NEW_ORDER','REPLACE')),
    taker_owner_type  text NOT NULL CHECK (taker_owner_type IN ('CUSTOMER','HOUSE')),
    would_cross       boolean NOT NULL,
    allowed           boolean NOT NULL,
    -- 0 is the default policy (no rule on record): house-versus-customer crossing is forbidden.
    rule_version      integer NOT NULL CHECK (rule_version >= 0),
    crossing_trades   integer NOT NULL CHECK (crossing_trades >= 0),
    evidence_ref      text NOT NULL,
    evidence_sha256   bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT book_crossing_decisions_shape CHECK (would_cross = (crossing_trades > 0)),
    CONSTRAINT book_crossing_decisions_needs_rule CHECK (NOT (would_cross AND allowed) OR rule_version >= 1)
);
CREATE INDEX book_crossing_decisions_order_idx ON book_crossing_decisions (order_id);
CREATE TRIGGER book_crossing_decisions_immutable BEFORE UPDATE OR DELETE ON book_crossing_decisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 5. House reservations (capital.Reserve is customer-only; a house book reserves its HOUSE_INVENTORY)
-- ---------------------------------------------------------------------------

CREATE TABLE book_house_reservations (
    id                  uuid PRIMARY KEY,
    house_account_id    uuid NOT NULL REFERENCES house_accounts(id),
    asset_id            uuid NOT NULL REFERENCES assets(id),
    order_id            uuid NOT NULL,
    quantity            numeric(38,0) NOT NULL CHECK (quantity > 0),
    consumed_quantity   numeric(38,0) NOT NULL DEFAULT 0 CHECK (consumed_quantity >= 0 AND consumed_quantity <= quantity),
    status              text NOT NULL CHECK (status IN ('ACTIVE','CONSUMED','RELEASED')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT book_house_reservations_consumed_shape CHECK (status <> 'CONSUMED' OR consumed_quantity = quantity),
    -- The order row is written after the engine decides its state, in the same transaction.
    CONSTRAINT book_house_reservations_order_fk FOREIGN KEY (order_id) REFERENCES book_orders(id) DEFERRABLE INITIALLY DEFERRED
);
CREATE UNIQUE INDEX book_house_reservations_one_active ON book_house_reservations (order_id) WHERE status = 'ACTIVE';
CREATE INDEX book_house_reservations_active_idx ON book_house_reservations (house_account_id, asset_id) WHERE status = 'ACTIVE';
CREATE TRIGGER book_house_reservations_updated_at BEFORE UPDATE ON book_house_reservations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_book_house_reservation_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.house_account_id, NEW.asset_id, NEW.order_id, NEW.quantity, NEW.created_at)
       IS DISTINCT FROM (OLD.id, OLD.house_account_id, OLD.asset_id, OLD.order_id, OLD.quantity, OLD.created_at) THEN
        RAISE EXCEPTION 'HOUSE_RESERVATION_FROZEN: house reservation % may change only its consumption and status', OLD.id
            USING ERRCODE = 'OB007';
    END IF;
    IF OLD.status <> 'ACTIVE' THEN
        RAISE EXCEPTION 'HOUSE_RESERVATION_FROZEN: house reservation % is % and never changes again', OLD.id, OLD.status
            USING ERRCODE = 'OB007';
    END IF;
    IF NEW.consumed_quantity < OLD.consumed_quantity THEN
        RAISE EXCEPTION 'HOUSE_RESERVATION_FROZEN: house reservation % consumption never shrinks', OLD.id USING ERRCODE = 'OB007';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NEW.status NOT IN ('CONSUMED','RELEASED') THEN
        RAISE EXCEPTION 'HOUSE_RESERVATION_FROZEN: house reservation % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'OB007';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_book_house_reservation_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER book_house_reservations_guard BEFORE UPDATE ON book_house_reservations
    FOR EACH ROW EXECUTE FUNCTION cp_book_house_reservation_guard();

CREATE TABLE book_house_reservation_transitions (
    id               uuid PRIMARY KEY,
    reservation_id   uuid NOT NULL REFERENCES book_house_reservations(id),
    from_status      text NOT NULL,
    to_status        text NOT NULL,
    reason           text NOT NULL CHECK (length(reason) > 0),
    actor_type       text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id         text NOT NULL,
    occurred_at      timestamptz NOT NULL DEFAULT now(),
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER book_house_reservation_transitions_immutable BEFORE UPDATE OR DELETE ON book_house_reservation_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER book_house_reservation_transitions_flag_edge AFTER INSERT ON book_house_reservation_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('reservation_id', 'from_status', 'to_status', 'book_house_reservation_status');
CREATE CONSTRAINT TRIGGER book_house_reservations_require_transition AFTER UPDATE OF status ON book_house_reservations
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'book_house_reservation_status');

-- ---------------------------------------------------------------------------
-- 6. The paper venue
-- ---------------------------------------------------------------------------

CREATE TABLE paper_fills (
    id                          uuid PRIMARY KEY,
    intent_id                   uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    account_id                  uuid NOT NULL REFERENCES accounts(id),
    instrument_id               uuid NOT NULL REFERENCES instruments(id),
    venue_label                 text NOT NULL DEFAULT 'SIMULATED_PAPER_VENUE' CHECK (venue_label = 'SIMULATED_PAPER_VENUE'),
    side                        text NOT NULL CHECK (side IN ('BUY','SELL')),
    base_asset_id               uuid NOT NULL REFERENCES assets(id),
    quote_asset_id              uuid NOT NULL REFERENCES assets(id),
    base_quantity               numeric(38,0) NOT NULL CHECK (base_quantity > 0),
    notional_at_reference       numeric(38,0) NOT NULL CHECK (notional_at_reference >= 0),
    spread_amount               numeric(38,0) NOT NULL CHECK (spread_amount >= 0),
    platform_fee_amount         numeric(38,0) NOT NULL CHECK (platform_fee_amount >= 0),
    venue_fee_amount            numeric(38,0) NOT NULL CHECK (venue_fee_amount >= 0),
    network_fee_amount          numeric(38,0) NOT NULL CHECK (network_fee_amount >= 0),
    total_quote                 numeric(38,0) NOT NULL CHECK (total_quote >= 0),
    mark_price_id               uuid,
    schedule_version            text NOT NULL,
    schedule_hash               text NOT NULL,
    -- The fees.Schedule canonical form the fill was priced under; its hex sha256 is schedule_hash.
    schedule                    bytea NOT NULL,
    -- The complete fees.Disclosure, canonical form, and its hash: every unit of spread and fee this
    -- fill charged is recomputable from these two documents alone.
    disclosure                  bytea NOT NULL,
    disclosure_hash             text NOT NULL,
    reservation_id              uuid NOT NULL,
    journal_transaction_id      uuid NOT NULL UNIQUE REFERENCES journal_transactions(id),
    effect_id                   uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    evidence_ref                text NOT NULL,
    evidence_sha256             bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    created_at                  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT paper_fills_schedule_hash CHECK (encode(sha256(schedule), 'hex') = schedule_hash),
    CONSTRAINT paper_fills_disclosure_hash CHECK (encode(sha256(disclosure), 'hex') = disclosure_hash),
    CHECK (base_asset_id <> quote_asset_id)
);
CREATE INDEX paper_fills_account_idx ON paper_fills (account_id, created_at DESC);
CREATE TRIGGER paper_fills_immutable BEFORE UPDATE OR DELETE ON paper_fills
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 7. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON book_states, book_state_transitions, book_orders, book_order_transitions, book_commands,
                        book_events, book_trades, book_snapshots, book_crossing_rules, book_crossing_decisions,
                        book_house_reservations, book_house_reservation_transitions, paper_fills TO cp_app;
GRANT UPDATE (status, last_command_seq, last_event_seq, last_trade_seq, state_hash) ON book_states TO cp_app;
GRANT UPDATE (status, price_ticks, qty_lots, filled_lots, remaining_lots, cancelled_lots, reduce_only_limit_lots,
              reject_reason, cancel_reason, reservation_id, accepted_command_seq, last_command_seq) ON book_orders TO cp_app;
GRANT UPDATE (status, consumed_quantity) ON book_house_reservations TO cp_app;
GRANT SELECT ON book_states, book_state_transitions, book_orders, book_order_transitions, book_commands, book_events,
               book_trades, book_snapshots, book_crossing_rules, book_crossing_decisions, book_house_reservations,
               book_house_reservation_transitions, paper_fills TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: the order journal, its trades and the paper fills are referenced by ledger history
