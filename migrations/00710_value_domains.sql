-- +goose Up
-- Value domains in the database (gola.md PART IX, PART X).
--
-- WHY THIS EXISTS. internal/valuedomain gives Go a vocabulary in which internal Credits, simulated
-- capital, partner-held fiat and self-custodial crypto are different types. That is worth having,
-- and it is not enough. The claim the product makes -- that Credits are closed-loop and cannot
-- become somebody's SOL -- is a claim about what the ledger contains, and the ledger is a database.
-- A Go check protects against the code we wrote; this protects against the code we will write, a
-- migration run by hand, a compromised service, and every future path that reaches the app role.
--
-- WHAT IS ENFORCED HERE, AND WHAT IS NOT.
--   * Structural isolation is enforced here. No capability, policy version, approval or operator
--     enables it, so it needs no environment context and belongs in the database.
--   * The requirement that a cross-domain transaction NAMES the conversion it performs is enforced
--     here. A movement between domains is always a stated intent, never an emergent property of
--     which accounts happened to be involved.
--   * Capability activation is NOT enforced here, and the honest reason is that a capability gate is
--     keyed by (capability, environment) and a database connection does not carry an environment
--     that the application could not simply assert. Adding a session GUC would look like a control
--     and be none. So capability remains in internal/valuedomain.CheckConversion, backed by the gate
--     state authority migration 00701 already moved into the database.
--
-- The split is deliberate: the database holds the promises no configuration may revoke, and the
-- application holds the ones that are configuration.
--
-- Custom SQLSTATEs raised here: VD001 domain/asset mismatch, VD002 structurally forbidden pair,
-- VD003 undeclared or mis-declared cross-domain movement, VD004 too many domains, VD005 bad domain.

-- ---------------------------------------------------------------------------
-- 1. Assets carry a value domain
-- ---------------------------------------------------------------------------

-- Two new kinds. CREDIT is the internal unit of account; NATIVE_ASSET is a user-created Nodal asset.
-- Neither exists on any chain, so neither has a real mint address (see the identity note below).
ALTER TABLE assets DROP CONSTRAINT assets_kind_check;
ALTER TABLE assets ADD CONSTRAINT assets_kind_check CHECK (
    kind IN ('NATIVE','SPL_TOKEN','SPL_TOKEN_2022','FIAT','CREDIT','NATIVE_ASSET'));

ALTER TABLE assets ADD COLUMN value_domain text;

-- Backfill before the constraint. Every asset that exists at this migration is either a Solana
-- asset held in a customer-controlled delegated wallet, or a fiat quote reference.
UPDATE assets SET value_domain = 'SELF_CUSTODIAL_CRYPTO' WHERE kind IN ('NATIVE','SPL_TOKEN','SPL_TOKEN_2022');

-- FIAT rows are quote references: migration 00602/00108 already forbid them from being held, traded
-- or posted to the ledger. A quote reference is not a balance, so giving it a value domain would be
-- inventing a fact. It stays NULL, and the constraint says exactly that.
ALTER TABLE assets ADD CONSTRAINT assets_value_domain_check CHECK (
    (kind = 'FIAT' AND value_domain IS NULL)
    OR (kind <> 'FIAT' AND value_domain IN (
        'INTERNAL_CREDIT','INTERNAL_NATIVE_ASSET','SIMULATED',
        'HOSTED_FIAT','HOSTED_CRYPTO','SELF_CUSTODIAL_CRYPTO',
        'PAYOUT_PENDING','EXTERNAL_SETTLED')));

-- A kind and a domain that disagree is a data-entry error that would silently reclassify value.
ALTER TABLE assets ADD CONSTRAINT assets_kind_domain_agree CHECK (
    (kind = 'CREDIT'       AND value_domain = 'INTERNAL_CREDIT')
    OR (kind = 'NATIVE_ASSET' AND value_domain = 'INTERNAL_NATIVE_ASSET')
    OR (kind = 'FIAT'         AND value_domain IS NULL)
    OR (kind IN ('NATIVE','SPL_TOKEN','SPL_TOKEN_2022')
        AND value_domain IN ('SELF_CUSTODIAL_CRYPTO','HOSTED_CRYPTO','SIMULATED')));

-- Identity. assets keeps UNIQUE (chain, mint_address) and internal assets satisfy it honestly:
-- chain is the sentinel 'nodal-internal' and mint_address is the asset's own uuid, which is unique
-- by construction. Nothing pretends an internal asset has a mint.
ALTER TABLE assets ADD CONSTRAINT assets_internal_chain_check CHECK (
    (kind IN ('CREDIT','NATIVE_ASSET') AND chain = 'nodal-internal' AND mint_address = id::text)
    OR (kind NOT IN ('CREDIT','NATIVE_ASSET') AND chain <> 'nodal-internal'));

-- ---------------------------------------------------------------------------
-- 2. New ledger account codes for the internal economy
-- ---------------------------------------------------------------------------

ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_code_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_code_check CHECK (code IN (
    -- existing chart
    'WALLET','CAPITAL','TRADING_OUTFLOW','TRADING_INFLOW',
    'FEES_NETWORK','FEES_VENUE','FEES_PLATFORM','DEFICIT','RECONCILIATION_ADJUSTMENT',
    'PLATFORM_FEE_RECEIVABLE','PLATFORM_FEE_REVENUE','PLATFORM_ADJUSTMENT',
    -- Nodal-native economy, customer side
    'CREDIT_BALANCE','CREDIT_ISSUANCE','NATIVE_ASSET_BALANCE',
    'NATIVE_TRADING_OUTFLOW','NATIVE_TRADING_INFLOW','CREDIT_FEES',
    'PAYOUT_RESERVED',
    -- Nodal-native economy, platform side
    'CREDIT_LIABILITY','MARKET_RESERVE','MARKET_INVENTORY',
    'PLATFORM_CREDIT_REVENUE','PAYOUT_CLEARING','PAYOUT_SETTLED'));

-- ---------------------------------------------------------------------------
-- 3. A ledger account's value domain
-- ---------------------------------------------------------------------------

-- Most accounts inherit the domain of their asset. The payout staging codes are the exception, and
-- they have to be: reserving Credits against a payout moves the same asset (a Credit) out of the
-- spendable domain and into a domain where it is committed and unspendable. Without the override
-- the reservation would be a single-domain movement and the PAYOUT_RESERVE gate would have nothing
-- to bite on.
-- +goose StatementBegin
CREATE FUNCTION cp_account_value_domain(p_asset_domain text, p_code text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE
        WHEN p_code IN ('PAYOUT_RESERVED','PAYOUT_CLEARING') THEN 'PAYOUT_PENDING'
        WHEN p_code = 'PAYOUT_SETTLED'                       THEN 'EXTERNAL_SETTLED'
        ELSE p_asset_domain
    END;
$$;
-- +goose StatementEnd

ALTER TABLE ledger_accounts ADD COLUMN value_domain text;

UPDATE ledger_accounts la
   SET value_domain = cp_account_value_domain(a.value_domain, la.code)
  FROM assets a
 WHERE a.id = la.asset_id;

ALTER TABLE ledger_accounts ALTER COLUMN value_domain SET NOT NULL;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_value_domain_check CHECK (
    value_domain IN ('INTERNAL_CREDIT','INTERNAL_NATIVE_ASSET','SIMULATED',
                     'HOSTED_FIAT','HOSTED_CRYPTO','SELF_CUSTODIAL_CRYPTO',
                     'PAYOUT_PENDING','EXTERNAL_SETTLED'));

-- The column is denormalised so the isolation trigger is a single join rather than two, and it is
-- kept honest by a trigger rather than by hoping every writer computes it correctly. cp_app has no
-- UPDATE privilege on it (00604 restricted UPDATE to `status`), so the only way it can be wrong is
-- at INSERT, which is where this fires.
-- +goose StatementBegin
CREATE FUNCTION cp_ledger_account_domain() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    asset_domain text;
    expected     text;
BEGIN
    SELECT a.value_domain INTO asset_domain FROM assets a WHERE a.id = NEW.asset_id;
    IF asset_domain IS NULL THEN
        RAISE EXCEPTION 'VALUE_DOMAIN_UNKNOWN: asset % has no value domain and cannot hold a ledger account', NEW.asset_id
            USING ERRCODE = 'VD001';
    END IF;
    expected := cp_account_value_domain(asset_domain, NEW.code);
    IF NEW.value_domain IS NULL THEN
        NEW.value_domain := expected;
    ELSIF NEW.value_domain <> expected THEN
        RAISE EXCEPTION 'VALUE_DOMAIN_MISMATCH: account code % on asset % is domain %, not %',
            NEW.code, NEW.asset_id, expected, NEW.value_domain USING ERRCODE = 'VD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_accounts_value_domain BEFORE INSERT ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION cp_ledger_account_domain();

-- ---------------------------------------------------------------------------
-- 4. Transactions declare their conversion
-- ---------------------------------------------------------------------------

ALTER TABLE journal_transactions ADD COLUMN conversion_from text;
ALTER TABLE journal_transactions ADD COLUMN conversion_to   text;
ALTER TABLE journal_transactions ADD CONSTRAINT journal_transactions_conversion_check CHECK (
    (conversion_from IS NULL AND conversion_to IS NULL)
    OR (conversion_from IS NOT NULL AND conversion_to IS NOT NULL AND conversion_from <> conversion_to));

-- New transaction kinds for the internal economy.
ALTER TABLE journal_transactions DROP CONSTRAINT journal_transactions_kind_check;
ALTER TABLE journal_transactions ADD CONSTRAINT journal_transactions_kind_check CHECK (kind IN (
    'FUNDING_SETTLED','FUNDING_REVERSAL','FUNDING_REVERSAL_DEFICIT','TRADE_FILL','FEE',
    'WITHDRAWAL_SETTLED','COMPENSATION','CORRECTION','RECONCILIATION_ADJUSTMENT','SEED',
    'CREDIT_ISSUED','CREDIT_REVERSED','CREDIT_SPENT','NATIVE_TRADE','INTERNAL_PURCHASE',
    'CREATOR_EARNING','PAYOUT_RESERVED','PAYOUT_SETTLED','PAYOUT_RETURNED'));

-- ---------------------------------------------------------------------------
-- 5. Structural isolation, checked at commit
-- ---------------------------------------------------------------------------

-- Mirrors internal/valuedomain.StructurallyForbidden. Kept as one function so the rule is stated
-- once on this side of the boundary; a test asserts the two sides agree on every ordered pair.
-- +goose StatementBegin
CREATE FUNCTION cp_domains_structurally_forbidden(a text, b text) RETURNS text
LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    IF a = b THEN
        RETURN NULL;
    END IF;
    IF a = 'SIMULATED' OR b = 'SIMULATED' THEN
        RETURN 'simulated capital has no economic substance and never moves to or from any other domain';
    END IF;
    IF a = 'SELF_CUSTODIAL_CRYPTO' OR b = 'SELF_CUSTODIAL_CRYPTO' THEN
        RETURN 'self-custodial value is authoritative on-chain; Nodal mirrors it and never moves it atomically with another domain';
    END IF;
    IF (a IN ('INTERNAL_CREDIT','INTERNAL_NATIVE_ASSET') AND b IN ('HOSTED_FIAT','HOSTED_CRYPTO','SELF_CUSTODIAL_CRYPTO'))
       OR (b IN ('INTERNAL_CREDIT','INTERNAL_NATIVE_ASSET') AND a IN ('HOSTED_FIAT','HOSTED_CRYPTO','SELF_CUSTODIAL_CRYPTO')) THEN
        RETURN 'internal Credits and native assets are closed-loop; the only route to external value is the gated payout path INTERNAL_CREDIT -> PAYOUT_PENDING -> EXTERNAL_SETTLED';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- Mirrors internal/valuedomain gatedConversions: which ordered pairs are declarable at all. The
-- capability each one requires is deliberately absent -- see the header note.
-- +goose StatementBegin
CREATE FUNCTION cp_conversion_is_declared(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('INTERNAL_CREDIT','INTERNAL_NATIVE_ASSET'),
        ('INTERNAL_NATIVE_ASSET','INTERNAL_CREDIT'),
        ('INTERNAL_CREDIT','PAYOUT_PENDING'),
        ('PAYOUT_PENDING','EXTERNAL_SETTLED'),
        ('PAYOUT_PENDING','INTERNAL_CREDIT'),
        ('HOSTED_FIAT','HOSTED_CRYPTO'),
        ('HOSTED_CRYPTO','HOSTED_FIAT'),
        ('EXTERNAL_SETTLED','HOSTED_FIAT'),
        ('HOSTED_FIAT','EXTERNAL_SETTLED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_check_domain_isolation() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    tx        journal_transactions%ROWTYPE;
    domains   text[];
    n         integer;
    a         text;
    b         text;
    why       text;
BEGIN
    SELECT * INTO tx FROM journal_transactions WHERE id = NEW.transaction_id;
    IF NOT FOUND THEN
        -- The header is written before its entries; nothing to check yet.
        RETURN NULL;
    END IF;

    SELECT array_agg(DISTINCT la.value_domain ORDER BY la.value_domain) INTO domains
      FROM journal_entries e
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.transaction_id;

    n := coalesce(array_length(domains, 1), 0);
    IF n = 0 THEN
        RAISE EXCEPTION 'VALUE_DOMAIN_EMPTY: transaction % touches no value domain', NEW.transaction_id
            USING ERRCODE = 'VD005';
    END IF;

    IF n = 1 THEN
        IF tx.conversion_from IS NOT NULL THEN
            RAISE EXCEPTION 'VALUE_DOMAIN_DECLARATION: transaction % declares conversion %->% but touches only %',
                NEW.transaction_id, tx.conversion_from, tx.conversion_to, domains[1] USING ERRCODE = 'VD003';
        END IF;
        RETURN NULL;
    END IF;

    IF n > 2 THEN
        RAISE EXCEPTION 'VALUE_DOMAIN_TOO_MANY: transaction % touches % value domains (%); every declared conversion is a pair',
            NEW.transaction_id, n, array_to_string(domains, ', ') USING ERRCODE = 'VD004';
    END IF;

    a := domains[1];
    b := domains[2];

    why := cp_domains_structurally_forbidden(a, b);
    IF why IS NOT NULL THEN
        RAISE EXCEPTION 'VALUE_DOMAIN_FORBIDDEN: % and % may never move together: %', a, b, why
            USING ERRCODE = 'VD002';
    END IF;

    IF tx.conversion_from IS NULL THEN
        RAISE EXCEPTION 'VALUE_DOMAIN_DECLARATION: transaction % touches % and % but declares no conversion',
            NEW.transaction_id, a, b USING ERRCODE = 'VD003';
    END IF;

    IF NOT ((tx.conversion_from = a AND tx.conversion_to = b) OR (tx.conversion_from = b AND tx.conversion_to = a)) THEN
        RAISE EXCEPTION 'VALUE_DOMAIN_DECLARATION: transaction % declares %->% but touches % and %',
            NEW.transaction_id, tx.conversion_from, tx.conversion_to, a, b USING ERRCODE = 'VD003';
    END IF;

    IF NOT cp_conversion_is_declared(tx.conversion_from, tx.conversion_to) THEN
        RAISE EXCEPTION 'VALUE_DOMAIN_UNDECLARED: no declared conversion moves value from % to %',
            tx.conversion_from, tx.conversion_to USING ERRCODE = 'VD003';
    END IF;

    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER journal_entries_domain_isolation AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_check_domain_isolation();

CREATE INDEX ledger_accounts_value_domain_idx ON ledger_accounts (value_domain, asset_id);
CREATE INDEX assets_value_domain_idx ON assets (value_domain) WHERE value_domain IS NOT NULL;

-- Privileges. cp_app may insert accounts (the trigger fills the domain) but must never change an
-- existing account's domain: 00604 already restricted UPDATE to `status`, and adding a column does
-- not widen a column-level grant, so nothing further is needed here. Stated explicitly because the
-- absence of a GRANT is the control.
GRANT SELECT ON assets, ledger_accounts TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: value-domain classification of existing ledger history is never dropped
