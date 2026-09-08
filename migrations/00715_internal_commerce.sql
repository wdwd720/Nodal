-- +goose Up
-- Internal commerce: the creator economy (gola.md PART XVII).
--
-- WHY THIS MATTERS MORE THAN IT LOOKS. PART XVII's requirement is one sentence -- "revenue earned by
-- creator activity MUST have provenance distinct from speculative trading proceeds" -- and it is the
-- sentence the whole payout architecture rests on. Creator earnings are the origin most likely to be
-- the FIRST thing a provider and counsel permit to be withdrawn, precisely because the user supplied
-- something real for them. Speculative proceeds are the last. If both arrived as "Credits", that
-- distinction could never be made afterwards.
--
-- So a product's KIND determines the provenance of the seller's earning, and the mapping is not a
-- runtime choice: DATA sales become DATA_SALE_EARNING, agent services become
-- AGENT_SERVICE_EARNING, everything else becomes CREATOR_EARNING. internal/payout already treats
-- those three separately from MARKET_TRADING_PROCEEDS, so the moment a policy permits one it does
-- not accidentally permit the other.
--
-- WHAT THIS IS NOT. There is no escrow, no delivery tracking and no dispute state machine. A sale is
-- atomic: Credits move and the entitlement exists, in one transaction. Adding a delivery lifecycle
-- would mean holding value in a state nobody has decided the legal character of, and PART XVII does
-- not ask for one.
--
-- Custom SQLSTATEs: IC001 order accounting does not balance, IC002 immutable row.

-- ---------------------------------------------------------------------------
-- 1. Sellers
-- ---------------------------------------------------------------------------

-- A seller is an account that has agreed to sell. It is a separate row rather than a flag on
-- accounts because selling carries obligations an ordinary account does not have, and because the
-- moment a payout provider exists this row is where its onboarding reference will live.
CREATE TABLE internal_sellers (
    account_id           uuid PRIMARY KEY REFERENCES accounts(id),
    display_name         text NOT NULL,
    status               text NOT NULL CHECK (status IN ('ACTIVE','SUSPENDED','CLOSED')),
    -- Where earnings are attributed. NULL means the selling account itself, which is the ordinary
    -- case; a different account is how an organisation collects on behalf of its members, and it is
    -- recorded rather than assumed so that "who earned this" is never inferred.
    payout_account_id    uuid REFERENCES accounts(id),
    suspended_reason     text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER internal_sellers_updated_at BEFORE UPDATE ON internal_sellers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 2. Products
-- ---------------------------------------------------------------------------

CREATE TABLE internal_products (
    id                  uuid PRIMARY KEY,
    seller_account_id   uuid NOT NULL REFERENCES internal_sellers(account_id),
    kind                text NOT NULL CHECK (kind IN
                            ('DATA','AGENT_SERVICE','COMPUTE','STRATEGY_TEMPLATE','RESEARCH',
                             'API_ACCESS','COMPETITION_ENTRY','CREATOR_PRODUCT')),
    title               text NOT NULL,
    description         text NOT NULL DEFAULT '',
    -- Price in Credit base units. Fixed per version: changing a price creates a new version rather
    -- than editing the row, so an order can always name the exact terms it was sold under.
    price               numeric(38,0) NOT NULL CHECK (price > 0),
    version             integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    -- The platform's share, in basis points, fixed at publication.
    platform_fee_bps    integer NOT NULL DEFAULT 0 CHECK (platform_fee_bps >= 0 AND platform_fee_bps <= 3000),
    status              text NOT NULL CHECK (status IN ('DRAFT','ACTIVE','PAUSED','WITHDRAWN')),
    metadata            jsonb NOT NULL DEFAULT '{}'::jsonb,
    published_at        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX internal_products_seller_idx ON internal_products (seller_account_id, created_at DESC);
CREATE INDEX internal_products_listed_idx ON internal_products (kind, created_at DESC) WHERE status = 'ACTIVE';
CREATE TRIGGER internal_products_updated_at BEFORE UPDATE ON internal_products
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Price and fee freeze once published. A buyer agreed to terms; the seller cannot restate them
-- afterwards, and the same reasoning that freezes a native asset's economics applies here.
-- +goose StatementBegin
CREATE FUNCTION cp_internal_product_terms_frozen() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.published_at IS NULL THEN
        RETURN NEW;
    END IF;
    IF NEW.price            IS DISTINCT FROM OLD.price
    OR NEW.platform_fee_bps IS DISTINCT FROM OLD.platform_fee_bps
    OR NEW.kind             IS DISTINCT FROM OLD.kind
    OR NEW.version          IS DISTINCT FROM OLD.version
    OR NEW.published_at     IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'INTERNAL_PRODUCT_TERMS_FROZEN: product % was published at %; price, fee, kind and version are fixed for this version',
            OLD.id, OLD.published_at USING ERRCODE = 'IC002';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER internal_products_terms_frozen BEFORE UPDATE ON internal_products
    FOR EACH ROW EXECUTE FUNCTION cp_internal_product_terms_frozen();

-- ---------------------------------------------------------------------------
-- 3. Orders
-- ---------------------------------------------------------------------------

CREATE TABLE internal_commerce_orders (
    id                     uuid PRIMARY KEY,
    product_id             uuid NOT NULL REFERENCES internal_products(id),
    product_version        integer NOT NULL,
    buyer_account_id       uuid NOT NULL REFERENCES accounts(id),
    seller_account_id      uuid NOT NULL REFERENCES accounts(id),
    -- Where the earning was attributed, resolved at order time from the seller row. Storing it makes
    -- "who was paid" a recorded fact rather than something re-derived from a row that may since have
    -- changed.
    earning_account_id     uuid NOT NULL REFERENCES accounts(id),

    price                  numeric(38,0) NOT NULL CHECK (price > 0),
    platform_fee           numeric(38,0) NOT NULL DEFAULT 0 CHECK (platform_fee >= 0),
    seller_proceeds        numeric(38,0) NOT NULL CHECK (seller_proceeds >= 0),
    -- The provenance the seller's earning carries. Derived from the product kind and stored, because
    -- the whole point is that this is decided once, at the sale, and never re-inferred.
    earning_origin         text NOT NULL CHECK (earning_origin IN
                               ('CREATOR_EARNING','DATA_SALE_EARNING','AGENT_SERVICE_EARNING')),

    journal_transaction_id uuid NOT NULL REFERENCES journal_transactions(id),
    idempotency_key        text NOT NULL UNIQUE,
    created_at             timestamptz NOT NULL DEFAULT now(),

    -- A buyer cannot be their own seller. Self-dealing here would let an account manufacture
    -- withdrawable provenance out of non-withdrawable Credits, which is the single most valuable
    -- thing an attacker could do to this system.
    --
    -- These three are NAMED. An unnamed CHECK reports as "internal_commerce_orders_check", which
    -- tells an operator reading an incident nothing about which rule was broken -- and the whole
    -- point of the first two is that somebody finding them in a log should immediately understand
    -- what was attempted.
    CONSTRAINT internal_commerce_orders_no_self_dealing
        CHECK (buyer_account_id <> seller_account_id),
    CONSTRAINT internal_commerce_orders_no_self_earning
        CHECK (buyer_account_id <> earning_account_id),
    CONSTRAINT internal_commerce_orders_price_is_split
        CHECK (price = platform_fee + seller_proceeds)
);
CREATE INDEX internal_commerce_orders_buyer_idx ON internal_commerce_orders (buyer_account_id, created_at DESC);
CREATE INDEX internal_commerce_orders_seller_idx ON internal_commerce_orders (seller_account_id, created_at DESC);
CREATE INDEX internal_commerce_orders_product_idx ON internal_commerce_orders (product_id, created_at DESC);
CREATE TRIGGER internal_commerce_orders_immutable BEFORE UPDATE OR DELETE ON internal_commerce_orders
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- The order's accounting must match the journal transaction it names: the buyer paid exactly the
-- price and the seller received exactly the proceeds. Checked at commit, against the entries rather
-- than against what the order row claims.
-- +goose StatementBegin
CREATE FUNCTION cp_internal_order_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    paid     numeric;
    received numeric;
BEGIN
    SELECT coalesce(sum(CASE WHEN e.side = 'CREDIT' THEN e.quantity ELSE 0 END), 0)
      INTO paid
      FROM journal_entries e
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id
       AND la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.buyer_account_id
       AND la.code = 'CREDIT_BALANCE';

    SELECT coalesce(sum(CASE WHEN e.side = 'DEBIT' THEN e.quantity ELSE 0 END), 0)
      INTO received
      FROM journal_entries e
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id
       AND la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.earning_account_id
       AND la.code = 'CREDIT_BALANCE';

    IF paid <> NEW.price THEN
        RAISE EXCEPTION 'INTERNAL_ORDER_UNBALANCED: order % claims a price of % but the buyer paid %',
            NEW.id, NEW.price, paid USING ERRCODE = 'IC001';
    END IF;
    IF received <> NEW.seller_proceeds THEN
        RAISE EXCEPTION 'INTERNAL_ORDER_UNBALANCED: order % claims seller proceeds of % but the earner received %',
            NEW.id, NEW.seller_proceeds, received USING ERRCODE = 'IC001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER internal_commerce_orders_balanced AFTER INSERT ON internal_commerce_orders
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_internal_order_balanced();

-- ---------------------------------------------------------------------------
-- 4. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE ON internal_sellers, internal_products TO cp_app;
GRANT SELECT, INSERT ON internal_commerce_orders TO cp_app;
GRANT SELECT ON internal_sellers, internal_products, internal_commerce_orders TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: commerce orders are financial history and are never dropped by rollback
