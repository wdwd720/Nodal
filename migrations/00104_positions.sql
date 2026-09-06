-- +goose Up
-- Position lots and dispositions (PART 85, PART 125 tax-quality records).

CREATE TABLE position_lots (
    id                     uuid PRIMARY KEY,
    account_id             uuid NOT NULL REFERENCES accounts(id),
    asset_id               uuid NOT NULL REFERENCES assets(id),
    quantity_original      numeric(38,0) NOT NULL CHECK (quantity_original > 0),
    quantity_open          numeric(38,0) NOT NULL CHECK (quantity_open >= 0 AND quantity_open <= quantity_original),
    acquired_at            timestamptz NOT NULL,
    cost_basis_usd_minor   bigint NOT NULL CHECK (cost_basis_usd_minor >= 0),   -- total basis for quantity_original incl. allocated fees
    fees_usd_minor         bigint NOT NULL DEFAULT 0 CHECK (fees_usd_minor >= 0),
    basis_source           text NOT NULL,                                       -- e.g. 'fill', 'funding', 'reconciliation_adjustment'
    valuation_source       text NOT NULL,                                       -- price source used for basis
    acquisition_ref_type   text NOT NULL,
    acquisition_ref_id     text NOT NULL,
    venue                  text,
    wallet_id              uuid,
    journal_transaction_id uuid REFERENCES journal_transactions(id),
    status                 text NOT NULL CHECK (status IN ('OPEN','CLOSED')),
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX position_lots_open_idx ON position_lots (account_id, asset_id, acquired_at, id) WHERE status = 'OPEN';
CREATE TRIGGER position_lots_updated_at BEFORE UPDATE ON position_lots FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE lot_dispositions (
    id                       uuid PRIMARY KEY,
    lot_id                   uuid NOT NULL REFERENCES position_lots(id),
    account_id               uuid NOT NULL REFERENCES accounts(id),
    asset_id                 uuid NOT NULL REFERENCES assets(id),
    quantity                 numeric(38,0) NOT NULL CHECK (quantity > 0),
    disposed_at              timestamptz NOT NULL,
    proceeds_usd_minor       bigint NOT NULL CHECK (proceeds_usd_minor >= 0),
    fees_usd_minor           bigint NOT NULL DEFAULT 0 CHECK (fees_usd_minor >= 0),
    basis_usd_minor          bigint NOT NULL CHECK (basis_usd_minor >= 0),
    realized_pnl_usd_minor   bigint NOT NULL,                                   -- proceeds - fees - basis
    valuation_source         text NOT NULL,
    disposition_ref_type     text NOT NULL,
    disposition_ref_id       text NOT NULL,
    journal_transaction_id   uuid REFERENCES journal_transactions(id),
    correction_of            uuid REFERENCES lot_dispositions(id),
    created_at               timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX lot_dispositions_account_idx ON lot_dispositions (account_id, asset_id, disposed_at);
CREATE INDEX lot_dispositions_lot_idx ON lot_dispositions (lot_id);
CREATE TRIGGER lot_dispositions_immutable BEFORE UPDATE OR DELETE ON lot_dispositions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON position_lots TO cp_app;
GRANT SELECT, INSERT ON lot_dispositions TO cp_app;
GRANT SELECT ON position_lots, lot_dispositions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: lot history is tax-quality financial record
