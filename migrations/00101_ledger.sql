-- +goose Up
-- Append-only, per-asset double-entry ledger (PARTS 19-21). See docs/architecture/FINANCIAL_MODEL.md.
-- Custom SQLSTATEs raised by triggers: LG001 negative balance, LG002 unbalanced transaction,
-- LG003 immutable row, LG004 asset mismatch, LG005 closed account.

CREATE TABLE ledger_accounts (
    id              uuid PRIMARY KEY,
    owner_type      text NOT NULL CHECK (owner_type IN ('CUSTOMER','PLATFORM')),
    owner_id        uuid NOT NULL,
    code            text NOT NULL CHECK (code IN (
                        'WALLET','CAPITAL','TRADING_OUTFLOW','TRADING_INFLOW',
                        'FEES_NETWORK','FEES_VENUE','FEES_PLATFORM','DEFICIT','RECONCILIATION_ADJUSTMENT',
                        'PLATFORM_FEE_RECEIVABLE','PLATFORM_FEE_REVENUE','PLATFORM_ADJUSTMENT')),
    asset_id        uuid NOT NULL REFERENCES assets(id),
    normal_side     text NOT NULL CHECK (normal_side IN ('DEBIT','CREDIT')),
    allow_negative  boolean NOT NULL DEFAULT false,
    status          text NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','CLOSED')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_type, owner_id, code, asset_id)
);

CREATE TABLE journal_transactions (
    id                    uuid PRIMARY KEY,
    kind                  text NOT NULL CHECK (kind IN (
                              'FUNDING_SETTLED','FUNDING_REVERSAL','FUNDING_REVERSAL_DEFICIT','TRADE_FILL','FEE',
                              'WITHDRAWAL_SETTLED','COMPENSATION','CORRECTION','RECONCILIATION_ADJUSTMENT','SEED')),
    idempotency_key       text NOT NULL UNIQUE,
    reference_type        text NOT NULL,
    reference_id          text NOT NULL,
    reversal_of           uuid REFERENCES journal_transactions(id),
    effective_at          timestamptz NOT NULL,
    posted_at             timestamptz NOT NULL DEFAULT now(),
    description           text NOT NULL DEFAULT '',
    correlation_id        text,
    posted_by_actor_type  text NOT NULL,
    posted_by_actor_id    text NOT NULL,
    reason_code           text,
    metadata              jsonb NOT NULL DEFAULT '{}'::jsonb,
    content_hash          bytea NOT NULL,
    build_version         text
);
CREATE INDEX journal_transactions_reference_idx ON journal_transactions (reference_type, reference_id);
CREATE INDEX journal_transactions_posted_idx ON journal_transactions (posted_at, id);
CREATE INDEX journal_transactions_reversal_idx ON journal_transactions (reversal_of) WHERE reversal_of IS NOT NULL;

CREATE TABLE journal_entries (
    id                 uuid PRIMARY KEY,
    transaction_id     uuid NOT NULL REFERENCES journal_transactions(id),
    seq                integer NOT NULL CHECK (seq >= 0),
    ledger_account_id  uuid NOT NULL REFERENCES ledger_accounts(id),
    asset_id           uuid NOT NULL REFERENCES assets(id),
    side               text NOT NULL CHECK (side IN ('DEBIT','CREDIT')),
    quantity           numeric(38,0) NOT NULL CHECK (quantity > 0),
    usd_value_minor    bigint,
    price_ref          text,
    UNIQUE (transaction_id, seq)
);
CREATE INDEX journal_entries_account_idx ON journal_entries (ledger_account_id, id);

CREATE TABLE ledger_balances (
    ledger_account_id  uuid PRIMARY KEY REFERENCES ledger_accounts(id),
    balance            numeric(38,0) NOT NULL DEFAULT 0,
    entry_count        bigint NOT NULL DEFAULT 0,
    version            bigint NOT NULL DEFAULT 0,
    last_entry_id      uuid,
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementBegin
CREATE FUNCTION ledger_forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'LEDGER_IMMUTABLE: % rows are append-only; post a compensating transaction', TG_TABLE_NAME
        USING ERRCODE = 'LG003';
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Applies one entry to the balance projection. SECURITY DEFINER so the application role can
-- never update ledger_balances directly but the trigger (owned by the migration role) can.
CREATE FUNCTION ledger_apply_entry() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    acct    ledger_accounts%ROWTYPE;
    delta   numeric(38,0);
    newbal  numeric(38,0);
BEGIN
    SELECT * INTO acct FROM ledger_accounts WHERE id = NEW.ledger_account_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'LEDGER_ACCOUNT_MISSING: %', NEW.ledger_account_id USING ERRCODE = 'LG004';
    END IF;
    IF acct.asset_id <> NEW.asset_id THEN
        RAISE EXCEPTION 'LEDGER_ASSET_MISMATCH: entry asset % does not match account asset %', NEW.asset_id, acct.asset_id
            USING ERRCODE = 'LG004';
    END IF;
    IF acct.status <> 'OPEN' THEN
        RAISE EXCEPTION 'LEDGER_ACCOUNT_CLOSED: %', acct.id USING ERRCODE = 'LG005';
    END IF;
    IF (NEW.side = 'DEBIT') = (acct.normal_side = 'DEBIT') THEN
        delta := NEW.quantity;
    ELSE
        delta := -NEW.quantity;
    END IF;
    INSERT INTO ledger_balances (ledger_account_id, balance, entry_count, version, last_entry_id, updated_at)
    VALUES (NEW.ledger_account_id, delta, 1, 1, NEW.id, now())
    ON CONFLICT (ledger_account_id) DO UPDATE
        SET balance       = ledger_balances.balance + EXCLUDED.balance,
            entry_count   = ledger_balances.entry_count + 1,
            version       = ledger_balances.version + 1,
            last_entry_id = EXCLUDED.last_entry_id,
            updated_at    = now()
    RETURNING balance INTO newbal;
    IF newbal < 0 AND NOT acct.allow_negative THEN
        RAISE EXCEPTION 'LEDGER_NEGATIVE_BALANCE: account % would be %', NEW.ledger_account_id, newbal
            USING ERRCODE = 'LG001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Deferred to commit: every transaction that received entries must have >= 2 entries and balance per asset.
CREATE FUNCTION ledger_check_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cnt  integer;
    bad  record;
BEGIN
    SELECT count(*) INTO cnt FROM journal_entries WHERE transaction_id = NEW.transaction_id;
    IF cnt < 2 THEN
        RAISE EXCEPTION 'LEDGER_UNBALANCED: transaction % has % entries', NEW.transaction_id, cnt USING ERRCODE = 'LG002';
    END IF;
    SELECT asset_id,
           sum(CASE WHEN side = 'DEBIT'  THEN quantity ELSE 0 END) AS debits,
           sum(CASE WHEN side = 'CREDIT' THEN quantity ELSE 0 END) AS credits
      INTO bad
      FROM journal_entries
     WHERE transaction_id = NEW.transaction_id
     GROUP BY asset_id
    HAVING sum(CASE WHEN side = 'DEBIT' THEN quantity ELSE -quantity END) <> 0
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'LEDGER_UNBALANCED: transaction % asset % debits % credits %',
            NEW.transaction_id, bad.asset_id, bad.debits, bad.credits USING ERRCODE = 'LG002';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- A transaction header without entries is also unbalanced (checked at commit).
CREATE FUNCTION ledger_check_transaction_has_entries() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE cnt integer;
BEGIN
    SELECT count(*) INTO cnt FROM journal_entries WHERE transaction_id = NEW.id;
    IF cnt < 2 THEN
        RAISE EXCEPTION 'LEDGER_UNBALANCED: transaction % committed with % entries', NEW.id, cnt USING ERRCODE = 'LG002';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER journal_transactions_immutable BEFORE UPDATE OR DELETE ON journal_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER journal_entries_immutable BEFORE UPDATE OR DELETE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER journal_entries_apply AFTER INSERT ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_apply_entry();
CREATE CONSTRAINT TRIGGER journal_entries_balanced AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_check_balanced();
CREATE CONSTRAINT TRIGGER journal_transactions_have_entries AFTER INSERT ON journal_transactions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_check_transaction_has_entries();

-- Application role: insert-only on journal tables; read-only on balances (maintained by SECURITY DEFINER trigger).
GRANT SELECT, INSERT, UPDATE ON ledger_accounts TO cp_app;
GRANT SELECT, INSERT ON journal_transactions, journal_entries TO cp_app;
GRANT SELECT ON ledger_balances TO cp_app;
GRANT SELECT ON ledger_accounts, journal_transactions, journal_entries, ledger_balances TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: ledger history is never dropped by rollback
