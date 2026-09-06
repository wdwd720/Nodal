-- +goose Up
-- A FIAT asset kind lets valuation express "USD" as a quote asset (e.g. to mark a
-- USD-pegged stablecoin against its market price under DEGRADED status, PART 26).
-- Fiat assets are never held in wallets, never traded, and never posted to the
-- asset-quantity ledger; they exist only as valuation quote references.
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_kind_check;
ALTER TABLE assets ADD CONSTRAINT assets_kind_check
    CHECK (kind IN ('NATIVE','SPL_TOKEN','SPL_TOKEN_2022','FIAT'));
ALTER TABLE assets ADD CONSTRAINT assets_fiat_shape
    CHECK (kind <> 'FIAT' OR (chain = 'fiat' AND mint_address = symbol AND risk_class = 'UNSUPPORTED' AND is_stablecoin = false));

-- Fiat quote references must never appear as ledger accounts.
-- +goose StatementBegin
CREATE FUNCTION ledger_accounts_reject_fiat() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM assets a WHERE a.id = NEW.asset_id AND a.kind = 'FIAT') THEN
        RAISE EXCEPTION 'LEDGER_ASSET_MISMATCH: fiat assets are valuation references, not ledger assets' USING ERRCODE = 'LG004';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER ledger_accounts_reject_fiat BEFORE INSERT ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION ledger_accounts_reject_fiat();

-- +goose Down
SELECT 1; -- protected: asset registry constraints are part of financial history semantics
