-- +goose Up
-- Nodal-native assets and their markets (gola.md PARTS XIII, XIV, XV, XVI).
--
-- WHAT THIS IS. A user creates an asset; the platform mints a fixed supply into a deterministic
-- constant-product market with a virtual Credit reserve; other users buy and sell it with Credits.
-- The asset exists only inside Nodal. It is not a token, it has no chain, and migration 00710 already
-- refuses to let it share a transaction with any real-capital domain.
--
-- IDENTITY. A native asset IS an entry in the asset registry: native_assets is keyed by asset_id, so
-- there is one identity from draft to delisting, and every ledger account, balance and journal entry
-- that ever refers to it refers to the same row. The registry row is created at DRAFT with status
-- RESTRICTED, which already means "may not increase exposure", so a draft cannot be traded even if
-- something forgot to check native_assets.status.
--
-- THE INVARIANT LIVES HERE, NOT ONLY IN GO. internal/nativemarket computes fills and proves
-- (V+R)*Y >= V*Y0 in Go, with a fuzz target behind it. That is the half that produces good errors.
-- This migration is the half that holds when a future caller, a migration run by hand, or a
-- compromised service writes a fill directly: market state is a projection that ONLY a trigger
-- writes, the trigger recomputes the reserves from the fill, and it refuses any fill that would put
-- the pool below its constant product or take the real reserve negative. numeric is arbitrary
-- precision in PostgreSQL, so the check is exact rather than approximate.
--
-- Custom SQLSTATEs: NM001 invariant violated, NM002 stale state version, NM003 immutable economics,
-- NM004 market not tradable, NM005 supply conservation broken.

-- ---------------------------------------------------------------------------
-- 0. Prune the chart of accounts to what the flows actually use
-- ---------------------------------------------------------------------------

-- 00710 added seven customer-side and six platform-side codes for the internal economy, sketched
-- before the postings existed. Writing them settled it, and three have no flow:
--
--   NATIVE_TRADING_OUTFLOW / NATIVE_TRADING_INFLOW were modelled on the Solana swap, where the
--   counterparty is an external venue that is not on our books, so the customer chart needs contra
--   accounts to stay balanced on its own. A native-market trade is different in kind: the
--   counterparty IS the platform pool, which is on our books, so the trade is a direct transfer
--   between two parties and balances without a contra account.
--
--   PLATFORM_CREDIT_REVENUE duplicates PLATFORM_FEE_RECEIVABLE, which is already per-asset: fee
--   income on the Credit asset IS Credit-denominated platform revenue, and a second account for the
--   same thing is a second number to reconcile.
--
--   CREDIT_FEES was the customer-side contra for the same reason as the trading pair.
--
-- Removing them now, while nothing has posted to them, is the cheap moment. An account that exists
-- and is always empty is worse than one that does not: every future reader has to work out whether
-- the emptiness is a bug.
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_code_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_code_check CHECK (code IN (
    'WALLET','CAPITAL','TRADING_OUTFLOW','TRADING_INFLOW',
    'FEES_NETWORK','FEES_VENUE','FEES_PLATFORM','DEFICIT','RECONCILIATION_ADJUSTMENT',
    'PLATFORM_FEE_RECEIVABLE','PLATFORM_FEE_REVENUE','PLATFORM_ADJUSTMENT',
    'CREDIT_BALANCE','CREDIT_ISSUANCE','NATIVE_ASSET_BALANCE','PAYOUT_RESERVED',
    'MARKET_RESERVE','MARKET_INVENTORY','PAYOUT_CLEARING','PAYOUT_SETTLED'));

-- ---------------------------------------------------------------------------
-- 1. Native assets
-- ---------------------------------------------------------------------------

-- Two widenings to the state-change binding of migration 00603, both found by using it on a table
-- whose shape the original callers happened to share.
--
-- 1. cp_require_transition read NEW.id, because every table it guarded had an `id` primary key.
--    native_assets is keyed by asset_id: a native asset IS its registry entry, and giving it a second
--    surrogate key so that one trigger could find it would be the tail wagging the dog. The function
--    now takes an optional second argument naming the id column, defaulting to `id`, so every
--    existing trigger is unaffected.
--
-- 2. The flag was a single value, so an entity could change state only ONCE per transaction: a
--    second cp_flag_transition overwrote the first, and at COMMIT the earlier update found the later
--    state in the setting and was refused. That restriction was never intended and is wrong for any
--    flow that submits and approves in one call -- which is exactly what creating and launching a
--    native asset does. The flag now accumulates the states flagged for an entity and the check is
--    membership.
--
--    Membership is exactly as strong as equality was. Writing a transition row was always the only
--    way to set the flag, and it still is; a caller that wants to reach state X must still insert an
--    immutable row saying it went to X. What changes is only that having gone A -> B -> C in one
--    transaction now satisfies both checks instead of neither.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_flag_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    entity_col text := TG_ARGV[0];
    state_col  text := TG_ARGV[1];
    entity_id  text;
    new_state  text;
    setting    text;
    current    text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($1).%I::text', entity_col, state_col) INTO entity_id, new_state USING NEW;
    setting := 'cp.transition.' || TG_ARGV[2] || '.x' || translate(entity_id, '-', '_');
    current := current_setting(setting, true);
    IF current IS NULL OR current = '' THEN
        PERFORM set_config(setting, new_state, true);
    ELSE
        PERFORM set_config(setting, current || '|' || new_state, true);
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_require_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    state_col text := TG_ARGV[0];
    id_col    text := coalesce(TG_ARGV[1], 'id');
    old_state text;
    new_state text;
    entity_id text;
    flagged   text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($2).%I::text', state_col, state_col) INTO old_state, new_state USING OLD, NEW;
    IF old_state IS NOT DISTINCT FROM new_state THEN
        RETURN NULL;
    END IF;
    EXECUTE format('SELECT ($1).%I::text', id_col) INTO entity_id USING NEW;
    flagged := current_setting('cp.transition.' || TG_TABLE_NAME || '.x' || translate(entity_id, '-', '_'), true);
    IF flagged IS NULL OR NOT (new_state = ANY (string_to_array(flagged, '|'))) THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % changed % -> % without a matching transition row in this transaction',
            TG_TABLE_NAME, entity_id, old_state, new_state USING ERRCODE = 'AU001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TABLE native_assets (
    asset_id                 uuid PRIMARY KEY REFERENCES assets(id),
    creator_account_id       uuid NOT NULL REFERENCES accounts(id),
    name                     text NOT NULL,
    symbol                   text NOT NULL,
    description              text NOT NULL DEFAULT '',
    image_url                text,
    metadata                 jsonb NOT NULL DEFAULT '{}'::jsonb,

    status                   text NOT NULL CHECK (status IN
                                 ('DRAFT','PENDING_REVIEW','ACTIVE','CLOSE_ONLY','HALTED','DELISTED','REJECTED')),

    -- Supply model. Fixed at activation and immutable thereafter: PART XIII forbids a creator
    -- changing economics after buyers enter, and "invisible supply changes" specifically.
    max_supply               numeric(38,0) NOT NULL CHECK (max_supply > 0),
    creator_allocation       numeric(38,0) NOT NULL DEFAULT 0 CHECK (creator_allocation >= 0),
    treasury_allocation      numeric(38,0) NOT NULL DEFAULT 0 CHECK (treasury_allocation >= 0),
    CHECK (creator_allocation + treasury_allocation <= max_supply),

    -- Policy profile (PART XXXIV). Every one of these is a stated position rather than a default,
    -- because "we never decided" and "we decided no" are different facts.
    internal_only                  boolean NOT NULL DEFAULT true,
    transferable                   boolean NOT NULL DEFAULT false,
    cashout_eligible               boolean NOT NULL DEFAULT false,
    creator_earning_eligible       boolean NOT NULL DEFAULT false,
    market_proceeds_eligible       boolean NOT NULL DEFAULT false,
    minimum_age                    integer NOT NULL DEFAULT 18 CHECK (minimum_age >= 0),
    jurisdiction_policy            text NOT NULL DEFAULT 'DEFAULT_CONSERVATIVE',
    marketing_restrictions         text NOT NULL DEFAULT 'NO_RETURN_CLAIMS',

    content_moderation_state text NOT NULL DEFAULT 'PENDING'
                                 CHECK (content_moderation_state IN ('PENDING','APPROVED','REJECTED','FLAGGED')),
    moderation_notes         text NOT NULL DEFAULT '',

    -- economics_locked_at is set when the asset first becomes ACTIVE. Once set, the supply and
    -- policy columns above are frozen by the trigger below.
    economics_locked_at      timestamptz,
    activated_at             timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),

    -- A native asset cannot be its own creator's escape hatch: it may only ever be INTERNAL_ONLY in
    -- this build, and turning that off is a schema change plus a legal decision, not a column write.
    CHECK (internal_only)
);
CREATE INDEX native_assets_creator_idx ON native_assets (creator_account_id, created_at DESC);
CREATE INDEX native_assets_status_idx ON native_assets (status) WHERE status IN ('ACTIVE','CLOSE_ONLY');

-- Symbol uniqueness among native assets. The registry treats symbol as display-only because two
-- chains legitimately host tokens with the same ticker; inside a single internal economy there is no
-- such excuse, and a duplicate symbol is an impersonation vector (PART LI). Case-insensitive.
CREATE UNIQUE INDEX native_assets_symbol_unique ON native_assets (upper(symbol))
    WHERE status <> 'REJECTED';

CREATE TRIGGER native_assets_updated_at BEFORE UPDATE ON native_assets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE native_asset_transitions (
    id             uuid PRIMARY KEY,
    asset_id       uuid NOT NULL REFERENCES native_assets(asset_id),
    from_status    text NOT NULL,
    to_status      text NOT NULL,
    actor_type     text NOT NULL,
    actor_id       text NOT NULL,
    reason         text NOT NULL,
    correlation_id text,
    occurred_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX native_asset_transitions_idx ON native_asset_transitions (asset_id, occurred_at);
CREATE TRIGGER native_asset_transitions_immutable BEFORE UPDATE OR DELETE ON native_asset_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER native_asset_transitions_flag AFTER INSERT ON native_asset_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('asset_id', 'to_status', 'native_assets');
CREATE CONSTRAINT TRIGGER native_assets_require_transition AFTER UPDATE OF status ON native_assets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status', 'asset_id');

-- Economics freeze. PART XIII: "Do not allow a creator to silently change economics after buyers
-- enter. Economic configuration becomes immutable at activation."
-- +goose StatementBegin
CREATE FUNCTION cp_native_asset_economics_frozen() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.economics_locked_at IS NULL THEN
        RETURN NEW;
    END IF;
    IF NEW.max_supply            IS DISTINCT FROM OLD.max_supply
    OR NEW.creator_allocation    IS DISTINCT FROM OLD.creator_allocation
    OR NEW.treasury_allocation   IS DISTINCT FROM OLD.treasury_allocation
    OR NEW.internal_only         IS DISTINCT FROM OLD.internal_only
    OR NEW.transferable          IS DISTINCT FROM OLD.transferable
    OR NEW.cashout_eligible      IS DISTINCT FROM OLD.cashout_eligible
    OR NEW.creator_earning_eligible IS DISTINCT FROM OLD.creator_earning_eligible
    OR NEW.market_proceeds_eligible IS DISTINCT FROM OLD.market_proceeds_eligible
    OR NEW.minimum_age           IS DISTINCT FROM OLD.minimum_age
    OR NEW.symbol                IS DISTINCT FROM OLD.symbol
    OR NEW.economics_locked_at   IS DISTINCT FROM OLD.economics_locked_at THEN
        RAISE EXCEPTION 'NATIVE_ASSET_ECONOMICS_FROZEN: asset % was activated at %; supply, allocation, policy and symbol are immutable',
            OLD.asset_id, OLD.economics_locked_at USING ERRCODE = 'NM003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER native_assets_economics_frozen BEFORE UPDATE ON native_assets
    FOR EACH ROW EXECUTE FUNCTION cp_native_asset_economics_frozen();

-- ---------------------------------------------------------------------------
-- 2. Markets
-- ---------------------------------------------------------------------------

CREATE TABLE native_markets (
    id                       uuid PRIMARY KEY,
    asset_id                 uuid NOT NULL UNIQUE REFERENCES native_assets(asset_id),
    credit_asset_id          uuid NOT NULL REFERENCES assets(id),

    -- The curve, fixed at activation.
    virtual_credit_reserve   numeric(38,0) NOT NULL CHECK (virtual_credit_reserve > 0),
    initial_asset_reserve    numeric(38,0) NOT NULL CHECK (initial_asset_reserve > 0),
    platform_fee_bps         integer NOT NULL DEFAULT 0 CHECK (platform_fee_bps >= 0),
    creator_fee_bps          integer NOT NULL DEFAULT 0 CHECK (creator_fee_bps >= 0),
    CHECK (platform_fee_bps + creator_fee_bps <= 1000),

    status                   text NOT NULL CHECK (status IN ('PENDING','ACTIVE','CLOSE_ONLY','HALTED','FROZEN','DELISTED')),
    activated_at             timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX native_markets_status_idx ON native_markets (status);
CREATE TRIGGER native_markets_updated_at BEFORE UPDATE ON native_markets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE native_market_transitions (
    id             uuid PRIMARY KEY,
    market_id      uuid NOT NULL REFERENCES native_markets(id),
    from_status    text NOT NULL,
    to_status      text NOT NULL,
    actor_type     text NOT NULL,
    actor_id       text NOT NULL,
    reason         text NOT NULL,
    correlation_id text,
    occurred_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX native_market_transitions_idx ON native_market_transitions (market_id, occurred_at);
CREATE TRIGGER native_market_transitions_immutable BEFORE UPDATE OR DELETE ON native_market_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER native_market_transitions_flag AFTER INSERT ON native_market_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('market_id', 'to_status', 'native_markets');
CREATE CONSTRAINT TRIGGER native_markets_require_transition AFTER UPDATE OF status ON native_markets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('status');

-- The curve is immutable once the market is live. A creator who could raise the fee after launch
-- could take a holder's position a basis point at a time (PART LXXX).
-- +goose StatementBegin
CREATE FUNCTION cp_native_market_curve_frozen() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.activated_at IS NULL THEN
        RETURN NEW;
    END IF;
    IF NEW.virtual_credit_reserve IS DISTINCT FROM OLD.virtual_credit_reserve
    OR NEW.initial_asset_reserve  IS DISTINCT FROM OLD.initial_asset_reserve
    OR NEW.platform_fee_bps       IS DISTINCT FROM OLD.platform_fee_bps
    OR NEW.creator_fee_bps        IS DISTINCT FROM OLD.creator_fee_bps
    OR NEW.asset_id               IS DISTINCT FROM OLD.asset_id
    OR NEW.credit_asset_id        IS DISTINCT FROM OLD.credit_asset_id
    OR NEW.activated_at           IS DISTINCT FROM OLD.activated_at THEN
        RAISE EXCEPTION 'NATIVE_MARKET_CURVE_FROZEN: market % went live at %; the curve and fees are immutable',
            OLD.id, OLD.activated_at USING ERRCODE = 'NM003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER native_markets_curve_frozen BEFORE UPDATE ON native_markets
    FOR EACH ROW EXECUTE FUNCTION cp_native_market_curve_frozen();

-- ---------------------------------------------------------------------------
-- 3. Fills are the only way market state moves
-- ---------------------------------------------------------------------------

CREATE TABLE native_market_fills (
    id                       uuid PRIMARY KEY,
    market_id                uuid NOT NULL REFERENCES native_markets(id),
    seq                      bigint NOT NULL,
    account_id               uuid NOT NULL REFERENCES accounts(id),
    side                     text NOT NULL CHECK (side IN ('BUY','SELL')),

    credits_in               numeric(38,0) NOT NULL DEFAULT 0 CHECK (credits_in >= 0),
    credits_out              numeric(38,0) NOT NULL DEFAULT 0 CHECK (credits_out >= 0),
    assets_in                numeric(38,0) NOT NULL DEFAULT 0 CHECK (assets_in >= 0),
    assets_out               numeric(38,0) NOT NULL DEFAULT 0 CHECK (assets_out >= 0),
    credits_to_pool          numeric(38,0) NOT NULL CHECK (credits_to_pool >= 0),
    platform_fee             numeric(38,0) NOT NULL DEFAULT 0 CHECK (platform_fee >= 0),
    creator_fee              numeric(38,0) NOT NULL DEFAULT 0 CHECK (creator_fee >= 0),

    -- The version the quote was priced against. The trigger refuses the fill if the market has moved
    -- since, which is how PART XIV's "execution revalidates market state" is enforced rather than
    -- merely intended.
    state_version_before     bigint NOT NULL,
    real_credit_reserve_after numeric(38,0) NOT NULL CHECK (real_credit_reserve_after >= 0),
    asset_reserve_after       numeric(38,0) NOT NULL CHECK (asset_reserve_after >= 0),

    quote_id                 uuid,
    journal_transaction_id   uuid NOT NULL REFERENCES journal_transactions(id),
    idempotency_key          text NOT NULL UNIQUE,
    created_at               timestamptz NOT NULL DEFAULT now(),

    UNIQUE (market_id, seq),
    -- A fill is one direction or the other, never both and never neither.
    CHECK ((side = 'BUY'  AND credits_in > 0 AND assets_out > 0 AND credits_out = 0 AND assets_in = 0)
        OR (side = 'SELL' AND assets_in  > 0 AND credits_out >= 0 AND credits_in = 0 AND assets_out = 0))
);
CREATE INDEX native_market_fills_market_idx ON native_market_fills (market_id, created_at DESC);
CREATE INDEX native_market_fills_account_idx ON native_market_fills (account_id, created_at DESC);
CREATE TRIGGER native_market_fills_immutable BEFORE UPDATE OR DELETE ON native_market_fills
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE native_market_state (
    market_id            uuid PRIMARY KEY REFERENCES native_markets(id),
    real_credit_reserve  numeric(38,0) NOT NULL DEFAULT 0 CHECK (real_credit_reserve >= 0),
    asset_reserve        numeric(38,0) NOT NULL CHECK (asset_reserve >= 0),
    version              bigint NOT NULL DEFAULT 0,
    fill_count           bigint NOT NULL DEFAULT 0,
    total_credit_volume  numeric(38,0) NOT NULL DEFAULT 0,
    updated_at           timestamptz NOT NULL DEFAULT now()
);

-- The market state is opened when the market row is created, so state and market cannot come apart.
-- +goose StatementBegin
CREATE FUNCTION cp_native_market_open() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    INSERT INTO native_market_state (market_id, real_credit_reserve, asset_reserve, version, fill_count)
    VALUES (NEW.id, 0, NEW.initial_asset_reserve, 0, 0);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER native_markets_open AFTER INSERT ON native_markets
    FOR EACH ROW EXECUTE FUNCTION cp_native_market_open();

-- The heart of it. Every accepted fill must:
--   1. be priced against the current version (no stale quotes),
--   2. leave reserves that match the fill's own arithmetic (no fill that claims one thing and does
--      another),
--   3. leave the pool at or above its constant product (no value created),
--   4. conserve Credits between the user, the pool and the fees,
--   5. leave the market tradable in the direction traded.
-- +goose StatementBegin
CREATE FUNCTION cp_native_market_apply_fill() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    m        native_markets%ROWTYPE;
    st       native_market_state%ROWTYPE;
    k        numeric;
    product  numeric;
    expect_r numeric;
    expect_y numeric;
BEGIN
    SELECT * INTO m FROM native_markets WHERE id = NEW.market_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'NATIVE_MARKET_MISSING: %', NEW.market_id USING ERRCODE = 'NM004';
    END IF;
    SELECT * INTO st FROM native_market_state WHERE market_id = NEW.market_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'NATIVE_MARKET_STATE_MISSING: %', NEW.market_id USING ERRCODE = 'NM004';
    END IF;

    IF NEW.state_version_before <> st.version THEN
        RAISE EXCEPTION 'NATIVE_MARKET_STALE: fill was priced against version % but the market is at version %',
            NEW.state_version_before, st.version USING ERRCODE = 'NM002';
    END IF;
    IF NEW.seq <> st.fill_count + 1 THEN
        RAISE EXCEPTION 'NATIVE_MARKET_STALE: fill sequence % does not follow % fills',
            NEW.seq, st.fill_count USING ERRCODE = 'NM002';
    END IF;

    -- Tradability. ACTIVE trades both ways; CLOSE_ONLY lets holders out and nobody in; anything else
    -- trades not at all. Letting holders exit a CLOSE_ONLY market is deliberate: freezing people
    -- into a position is worse than stopping new ones.
    IF m.status = 'CLOSE_ONLY' AND NEW.side = 'BUY' THEN
        RAISE EXCEPTION 'NATIVE_MARKET_CLOSE_ONLY: market % accepts sells only', m.id USING ERRCODE = 'NM004';
    END IF;
    IF m.status NOT IN ('ACTIVE','CLOSE_ONLY') THEN
        RAISE EXCEPTION 'NATIVE_MARKET_NOT_TRADABLE: market % is %', m.id, m.status USING ERRCODE = 'NM004';
    END IF;

    -- Re-derive the reserves from the fill rather than trusting the numbers it carries.
    IF NEW.side = 'BUY' THEN
        IF NEW.credits_in <> NEW.credits_to_pool + NEW.platform_fee + NEW.creator_fee THEN
            RAISE EXCEPTION 'NATIVE_MARKET_UNCONSERVED: buy paid % but accounts for %',
                NEW.credits_in, NEW.credits_to_pool + NEW.platform_fee + NEW.creator_fee USING ERRCODE = 'NM001';
        END IF;
        expect_r := st.real_credit_reserve + NEW.credits_to_pool;
        expect_y := st.asset_reserve - NEW.assets_out;
    ELSE
        IF NEW.credits_to_pool <> NEW.credits_out + NEW.platform_fee + NEW.creator_fee THEN
            RAISE EXCEPTION 'NATIVE_MARKET_UNCONSERVED: sell released % but accounts for %',
                NEW.credits_to_pool, NEW.credits_out + NEW.platform_fee + NEW.creator_fee USING ERRCODE = 'NM001';
        END IF;
        expect_r := st.real_credit_reserve - NEW.credits_to_pool;
        expect_y := st.asset_reserve + NEW.assets_in;
    END IF;

    IF expect_r <> NEW.real_credit_reserve_after OR expect_y <> NEW.asset_reserve_after THEN
        RAISE EXCEPTION 'NATIVE_MARKET_UNCONSERVED: fill claims reserves (%, %) but its own amounts give (%, %)',
            NEW.real_credit_reserve_after, NEW.asset_reserve_after, expect_r, expect_y USING ERRCODE = 'NM001';
    END IF;
    IF expect_r < 0 THEN
        RAISE EXCEPTION 'NATIVE_MARKET_INSOLVENT: the real Credit reserve would go to %', expect_r
            USING ERRCODE = 'NM001';
    END IF;
    IF expect_y > m.initial_asset_reserve THEN
        RAISE EXCEPTION 'NATIVE_MARKET_SUPPLY: the pool would hold % units, more than the % ever minted into it',
            expect_y, m.initial_asset_reserve USING ERRCODE = 'NM005';
    END IF;

    k := m.virtual_credit_reserve * m.initial_asset_reserve;
    product := (m.virtual_credit_reserve + expect_r) * expect_y;
    IF product < k THEN
        RAISE EXCEPTION 'NATIVE_MARKET_INVARIANT: the constant product would fall from % to %', k, product
            USING ERRCODE = 'NM001';
    END IF;

    UPDATE native_market_state
       SET real_credit_reserve = expect_r,
           asset_reserve       = expect_y,
           version             = st.version + 1,
           fill_count          = st.fill_count + 1,
           total_credit_volume = st.total_credit_volume + GREATEST(NEW.credits_in, NEW.credits_to_pool),
           updated_at          = now()
     WHERE market_id = NEW.market_id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER native_market_fills_apply AFTER INSERT ON native_market_fills
    FOR EACH ROW EXECUTE FUNCTION cp_native_market_apply_fill();

-- ---------------------------------------------------------------------------
-- 4. Quotes
-- ---------------------------------------------------------------------------

-- A quote is a record of what the market said at a version, not a promise. Execution re-prices and
-- re-checks the version; the quote exists so that what the user was shown is auditable and so that a
-- min-output can be bound to it.
CREATE TABLE native_market_quotes (
    id                   uuid PRIMARY KEY,
    market_id            uuid NOT NULL REFERENCES native_markets(id),
    account_id           uuid NOT NULL REFERENCES accounts(id),
    side                 text NOT NULL CHECK (side IN ('BUY','SELL')),
    input_amount         numeric(38,0) NOT NULL CHECK (input_amount > 0),
    expected_output      numeric(38,0) NOT NULL CHECK (expected_output >= 0),
    platform_fee         numeric(38,0) NOT NULL DEFAULT 0,
    creator_fee          numeric(38,0) NOT NULL DEFAULT 0,
    spot_price_before    numeric(78,0) NOT NULL,
    effective_price      numeric(78,0) NOT NULL,
    slippage_bps         integer NOT NULL,
    state_version        bigint NOT NULL,
    expires_at           timestamptz NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX native_market_quotes_account_idx ON native_market_quotes (account_id, created_at DESC);
CREATE INDEX native_market_quotes_expiry_idx ON native_market_quotes (expires_at);
CREATE TRIGGER native_market_quotes_immutable BEFORE UPDATE OR DELETE ON native_market_quotes
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

ALTER TABLE native_market_fills
    ADD CONSTRAINT native_market_fills_quote_fk FOREIGN KEY (quote_id) REFERENCES native_market_quotes(id);

-- ---------------------------------------------------------------------------
-- 5. Surveillance (PART XVI)
-- ---------------------------------------------------------------------------

CREATE TABLE native_market_alerts (
    id             uuid PRIMARY KEY,
    market_id      uuid NOT NULL REFERENCES native_markets(id),
    account_id     uuid REFERENCES accounts(id),
    kind           text NOT NULL CHECK (kind IN
                       ('SELF_TRADE','WASH_TRADE','RAPID_ROUND_TRIP','CONCENTRATION','CREATOR_SELF_DEALING',
                        'ANOMALOUS_VOLUME','QUOTE_SPAM','COORDINATED_ACCUMULATION')),
    severity       text NOT NULL CHECK (severity IN ('INFO','WARN','CRITICAL')),
    detail         jsonb NOT NULL DEFAULT '{}'::jsonb,
    fill_id        uuid REFERENCES native_market_fills(id),
    resolved_at    timestamptz,
    resolution     text,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX native_market_alerts_market_idx ON native_market_alerts (market_id, created_at DESC);
CREATE INDEX native_market_alerts_open_idx ON native_market_alerts (kind, created_at DESC) WHERE resolved_at IS NULL;

-- ---------------------------------------------------------------------------
-- 6. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE ON native_assets, native_markets TO cp_app;
GRANT SELECT, INSERT ON native_asset_transitions, native_market_transitions,
                        native_market_fills, native_market_quotes, native_market_alerts TO cp_app;
GRANT UPDATE (resolved_at, resolution) ON native_market_alerts TO cp_app;
-- The state projection is derived from fills and is never written directly.
GRANT SELECT ON native_market_state TO cp_app;
GRANT SELECT ON native_assets, native_markets, native_market_state, native_market_fills,
                native_market_quotes, native_market_alerts, native_asset_transitions,
                native_market_transitions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: native market history is financial history and is never dropped by rollback
