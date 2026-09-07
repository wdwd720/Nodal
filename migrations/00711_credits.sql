-- +goose Up
-- Nodal Credits and their provenance (gola.md PARTS IX, XI, XII).
--
-- THE PROBLEM THIS SOLVES. "The user has 18,450 Credits" is not enough information to answer "how
-- much may they withdraw?", and treating it as though it were is how a platform ends up paying out
-- promotional grants or value backed by a payment that is about to be charged back. Payout
-- eligibility is a property of the individual UNITS, derived from where they came from and how final
-- the funding behind them is. So Credits are held in immutable provenance lots, spending consumes
-- specific lots in a defined order, and what remains is what determines what can be withdrawn.
--
-- THE SHAPE, which deliberately mirrors migration 00101's ledger:
--
--   credit_lots            append-only. What was issued, to whom, from where. Never updated.
--   credit_lot_events      append-only. Every consumption, restoration and finality change.
--   credit_lot_state       a projection maintained ONLY by a SECURITY DEFINER trigger, exactly as
--                          ledger_balances is. cp_app can read it and cannot write it, so a lot's
--                          remaining quantity cannot be edited into existence.
--
-- The journal remains the authority on balances; these tables are the authority on PROVENANCE. Both
-- are written in the same transaction, and a lot that does not tie back to a journal transaction is
-- refused.
--
-- Custom SQLSTATEs: CR001 lot over-consumed, CR002 immutable row, CR003 illegal finality transition,
-- CR004 lot/journal mismatch, CR005 more than one Credit asset.

-- ---------------------------------------------------------------------------
-- 1. There is exactly one Credit asset
-- ---------------------------------------------------------------------------

-- Credits are the platform's unit of account, not a class of them. A second CREDIT asset would make
-- "Credits" ambiguous in every balance, price and payout in the system, and nothing in the product
-- has a use for one. A partial unique index says so in a way no code path can get around.
CREATE UNIQUE INDEX assets_single_credit_asset ON assets ((kind)) WHERE kind = 'CREDIT';

-- ---------------------------------------------------------------------------
-- 1b. Retire an account code that turned out to have no flow
-- ---------------------------------------------------------------------------

-- CREDIT_LIABILITY was added by 00710 as the platform-side mirror of credits outstanding. Working
-- through the actual postings, it has no use: issuance mirrors the proven WALLET/CAPITAL pattern and
-- balances entirely within the customer chart, market flows use MARKET_RESERVE, and platform service
-- revenue uses the existing PLATFORM_FEE_RECEIVABLE/PLATFORM_CREDIT_REVENUE pair. Credits
-- outstanding is SUM(CREDIT_BALANCE) over customer accounts, one query, no second source of truth.
-- Removing it now is cheaper than explaining a permanently empty account to every future reader, and
-- no row can reference it: it is dropped from the CHECK, and any existing account would fail the
-- new constraint, which is the check that it was never used.
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_code_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_code_check CHECK (code IN (
    'WALLET','CAPITAL','TRADING_OUTFLOW','TRADING_INFLOW',
    'FEES_NETWORK','FEES_VENUE','FEES_PLATFORM','DEFICIT','RECONCILIATION_ADJUSTMENT',
    'PLATFORM_FEE_RECEIVABLE','PLATFORM_FEE_REVENUE','PLATFORM_ADJUSTMENT',
    'CREDIT_BALANCE','CREDIT_ISSUANCE','NATIVE_ASSET_BALANCE',
    'NATIVE_TRADING_OUTFLOW','NATIVE_TRADING_INFLOW','CREDIT_FEES','PAYOUT_RESERVED',
    'MARKET_RESERVE','MARKET_INVENTORY','PLATFORM_CREDIT_REVENUE','PAYOUT_CLEARING','PAYOUT_SETTLED'));

-- ---------------------------------------------------------------------------
-- 2. Lots
-- ---------------------------------------------------------------------------

CREATE TABLE credit_lots (
    id                      uuid PRIMARY KEY,
    account_id              uuid NOT NULL REFERENCES accounts(id),
    asset_id                uuid NOT NULL REFERENCES assets(id),
    origin                  text NOT NULL CHECK (origin IN (
                                'PURCHASED','PROMOTIONAL','REFUND','CREATOR_EARNING','DATA_SALE_EARNING',
                                'AGENT_SERVICE_EARNING','MARKET_CREATOR_EARNING','MARKET_TRADING_PROCEEDS',
                                'COMPETITION_REWARD','ADMIN_ADJUSTMENT','PROVIDER_SETTLEMENT')),
    initial_finality        text NOT NULL CHECK (initial_finality IN ('UNFUNDED','REVERSIBLE','SETTLED','DISPUTED','REVERSED')),
    quantity                numeric(38,0) NOT NULL CHECK (quantity > 0),
    -- What produced this value. funding_reference points at the credit_fundings row for PURCHASED
    -- lots, at an internal commerce order for earnings, and is null for grants.
    funding_reference_type  text,
    funding_reference_id    text,
    -- The journal transaction that actually moved the units. A lot without one would be provenance
    -- for value that was never posted.
    journal_transaction_id  uuid NOT NULL REFERENCES journal_transactions(id),
    issued_by_actor_type    text NOT NULL,
    issued_by_actor_id      text NOT NULL,
    reason                  text NOT NULL DEFAULT '',
    created_at              timestamptz NOT NULL DEFAULT now(),
    CHECK ((funding_reference_type IS NULL) = (funding_reference_id IS NULL))
);
CREATE INDEX credit_lots_account_idx ON credit_lots (account_id, asset_id, created_at, id);
CREATE INDEX credit_lots_funding_idx ON credit_lots (funding_reference_type, funding_reference_id)
    WHERE funding_reference_type IS NOT NULL;
CREATE TRIGGER credit_lots_immutable BEFORE UPDATE OR DELETE ON credit_lots
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE credit_lot_events (
    id                      uuid PRIMARY KEY,
    lot_id                  uuid NOT NULL REFERENCES credit_lots(id),
    seq                     bigint NOT NULL,
    kind                    text NOT NULL CHECK (kind IN ('CONSUME','RESTORE','FINALITY')),
    -- Positive for both CONSUME and RESTORE; the kind carries the direction, so a sign error cannot
    -- silently invert a movement.
    delta_quantity          numeric(38,0) CHECK (delta_quantity IS NULL OR delta_quantity > 0),
    to_finality             text CHECK (to_finality IS NULL OR to_finality IN ('UNFUNDED','REVERSIBLE','SETTLED','DISPUTED','REVERSED')),
    reference_type          text NOT NULL,
    reference_id            text NOT NULL,
    journal_transaction_id  uuid REFERENCES journal_transactions(id),
    reason                  text NOT NULL DEFAULT '',
    actor_type              text NOT NULL,
    actor_id                text NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (lot_id, seq),
    CHECK ((kind = 'FINALITY') = (to_finality IS NOT NULL)),
    CHECK ((kind IN ('CONSUME','RESTORE')) = (delta_quantity IS NOT NULL))
);
CREATE INDEX credit_lot_events_ref_idx ON credit_lot_events (reference_type, reference_id);
CREATE TRIGGER credit_lot_events_immutable BEFORE UPDATE OR DELETE ON credit_lot_events
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE credit_lot_state (
    lot_id             uuid PRIMARY KEY REFERENCES credit_lots(id),
    remaining_quantity numeric(38,0) NOT NULL CHECK (remaining_quantity >= 0),
    finality           text NOT NULL,
    event_count        bigint NOT NULL DEFAULT 0,
    version            bigint NOT NULL DEFAULT 0,
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX credit_lot_state_open_idx ON credit_lot_state (lot_id) WHERE remaining_quantity > 0;

-- ---------------------------------------------------------------------------
-- 3. The projection is maintained only by the database
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_credit_lot_open() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    txdomain text;
BEGIN
    -- A lot must describe units that a journal transaction actually moved, in the Credit domain.
    SELECT DISTINCT la.value_domain INTO txdomain
      FROM journal_entries e
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id
       AND la.owner_type = 'CUSTOMER'
       AND la.owner_id = NEW.account_id
       AND e.asset_id = NEW.asset_id
     LIMIT 1;
    IF txdomain IS NULL THEN
        RAISE EXCEPTION 'CREDIT_LOT_UNBACKED: journal transaction % has no entry for account % on asset %',
            NEW.journal_transaction_id, NEW.account_id, NEW.asset_id USING ERRCODE = 'CR004';
    END IF;

    INSERT INTO credit_lot_state (lot_id, remaining_quantity, finality, event_count, version, updated_at)
    VALUES (NEW.id, NEW.quantity, NEW.initial_finality, 0, 1, now());
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Mirrors internal/valuedomain.CanTransitionFinality. A test asserts the two agree on every pair.
-- +goose StatementBegin
CREATE FUNCTION cp_credit_finality_can_transition(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('REVERSIBLE','SETTLED'),
        ('REVERSIBLE','DISPUTED'),
        ('REVERSIBLE','REVERSED'),
        ('SETTLED','DISPUTED'),
        ('DISPUTED','SETTLED'),
        ('DISPUTED','REVERSED'),
        ('DISPUTED','REVERSIBLE'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_credit_lot_apply_event() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    st       credit_lot_state%ROWTYPE;
    newqty   numeric(38,0);
    newfin   text;
    lotqty   numeric(38,0);
BEGIN
    SELECT * INTO st FROM credit_lot_state WHERE lot_id = NEW.lot_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'CREDIT_LOT_MISSING: %', NEW.lot_id USING ERRCODE = 'CR001';
    END IF;
    newqty := st.remaining_quantity;
    newfin := st.finality;

    IF NEW.kind = 'CONSUME' THEN
        newqty := st.remaining_quantity - NEW.delta_quantity;
        IF newqty < 0 THEN
            RAISE EXCEPTION 'CREDIT_LOT_OVERCONSUMED: lot % has % remaining, consume % requested',
                NEW.lot_id, st.remaining_quantity, NEW.delta_quantity USING ERRCODE = 'CR001';
        END IF;
    ELSIF NEW.kind = 'RESTORE' THEN
        newqty := st.remaining_quantity + NEW.delta_quantity;
        SELECT quantity INTO lotqty FROM credit_lots WHERE id = NEW.lot_id;
        IF newqty > lotqty THEN
            RAISE EXCEPTION 'CREDIT_LOT_OVERRESTORED: lot % was issued % and would hold % after restoring %',
                NEW.lot_id, lotqty, newqty, NEW.delta_quantity USING ERRCODE = 'CR001';
        END IF;
    ELSE
        IF NOT cp_credit_finality_can_transition(st.finality, NEW.to_finality) THEN
            RAISE EXCEPTION 'CREDIT_FINALITY_ILLEGAL: lot % cannot go % -> %',
                NEW.lot_id, st.finality, NEW.to_finality USING ERRCODE = 'CR003';
        END IF;
        newfin := NEW.to_finality;
    END IF;

    UPDATE credit_lot_state
       SET remaining_quantity = newqty,
           finality           = newfin,
           event_count        = st.event_count + 1,
           version            = st.version + 1,
           updated_at         = now()
     WHERE lot_id = NEW.lot_id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER credit_lots_open AFTER INSERT ON credit_lots
    FOR EACH ROW EXECUTE FUNCTION cp_credit_lot_open();
CREATE TRIGGER credit_lot_events_apply AFTER INSERT ON credit_lot_events
    FOR EACH ROW EXECUTE FUNCTION cp_credit_lot_apply_event();

-- ---------------------------------------------------------------------------
-- 4. Credit funding lifecycle (PART XI)
-- ---------------------------------------------------------------------------

-- The eleven states of PART XI. A captured card payment is not a settled one, and this table exists
-- so the difference is recorded rather than assumed.
CREATE TABLE credit_fundings (
    id                    uuid PRIMARY KEY,
    account_id            uuid NOT NULL REFERENCES accounts(id),
    provider              text NOT NULL,
    provider_reference    text,
    state                 text NOT NULL CHECK (state IN (
                              'CREATED','AUTHORIZATION_PENDING','AUTHORIZED','CAPTURE_PENDING','CAPTURED',
                              'REVERSIBLE','SETTLED','REVERSED','REFUNDED','DISPUTED','FAILED')),
    credit_quantity       numeric(38,0) NOT NULL CHECK (credit_quantity > 0),
    -- What the user actually paid, in USD minor units, kept separate from the Credits issued so the
    -- exchange rate of the day is a recorded fact rather than something re-derived later.
    paid_amount_minor     bigint NOT NULL CHECK (paid_amount_minor >= 0),
    paid_currency         text NOT NULL DEFAULT 'USD',
    fee_amount_minor      bigint NOT NULL DEFAULT 0 CHECK (fee_amount_minor >= 0),
    idempotency_key       text NOT NULL UNIQUE,
    lot_id                uuid REFERENCES credit_lots(id),
    settled_at            timestamptz,
    reversed_at           timestamptz,
    failure_reason        text,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_reference)
);
CREATE INDEX credit_fundings_account_idx ON credit_fundings (account_id, created_at DESC);
CREATE INDEX credit_fundings_state_idx ON credit_fundings (state) WHERE state NOT IN ('SETTLED','REVERSED','REFUNDED','FAILED');
CREATE TRIGGER credit_fundings_updated_at BEFORE UPDATE ON credit_fundings FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE credit_funding_transitions (
    id             uuid PRIMARY KEY,
    funding_id     uuid NOT NULL REFERENCES credit_fundings(id),
    from_state     text NOT NULL,
    to_state       text NOT NULL,
    actor_type     text NOT NULL,
    actor_id       text NOT NULL,
    reason         text NOT NULL,
    provider_event text,
    correlation_id text,
    occurred_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX credit_funding_transitions_idx ON credit_funding_transitions (funding_id, occurred_at);
CREATE TRIGGER credit_funding_transitions_immutable BEFORE UPDATE OR DELETE ON credit_funding_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- A state change must carry its transition row, using the same binding mechanism migration 00603
-- installed for gates, accounts, assets, intents and orders: a bare UPDATE is refused at COMMIT.
CREATE TRIGGER credit_funding_transitions_flag AFTER INSERT ON credit_funding_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('funding_id', 'to_state', 'credit_fundings');
CREATE CONSTRAINT TRIGGER credit_fundings_require_transition AFTER UPDATE OF state ON credit_fundings
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('state');

-- ---------------------------------------------------------------------------
-- 5. Privileges
-- ---------------------------------------------------------------------------

-- Append-only for the application on every provenance table. The projection is readable and not
-- writable: remaining_quantity is a consequence of recorded events, never an assertion.
GRANT SELECT, INSERT ON credit_lots, credit_lot_events, credit_funding_transitions TO cp_app;
GRANT SELECT ON credit_lot_state TO cp_app;
GRANT SELECT, INSERT, UPDATE ON credit_fundings TO cp_app;
GRANT SELECT ON credit_lots, credit_lot_events, credit_lot_state, credit_fundings, credit_funding_transitions
    TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: credit provenance is financial history and is never dropped by rollback
