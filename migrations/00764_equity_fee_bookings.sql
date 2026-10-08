-- +goose Up
-- Every spread and fee an equity fill charges is booked through the fee ledger, in the transaction that applies
-- the fill (internal/equities, internal/feeledger; ADVANCED_FINANCIAL_MODEL.md §5, ADR-0020). 00743 records each
-- equity fill with the spread and fees it charged, the schedule that priced them and the fill's full disclosure;
-- 00761 bound three charge points to the fee ledger -- internal-book trades, paper-venue fills and margin
-- liquidation steps -- and not equities, whose fills disclosed and posted their charges on their own. An equity
-- fill could therefore charge a customer a spread the fee ledger never saw, and the spread audit could not count
-- it. This migration binds the fourth charge point.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. An equity fill (equity_fills) that charges anything -- a spread, platform fees (the platform fee and the
--      strategy fee) or a venue pass-through -- commits only with its payer's fee booking against the fill's own
--      journal that carries exactly the fill's spread, its platform fees as the explicit fee, its venue fees and
--      no network cost, discloses exactly the fill's quantity, was priced under the schedule the fill names
--      (fee_schedule_hash), and whose disclosure is byte for byte the fill's own: the fill's stored disclosure
--      hashes to the booking's disclosure hash, which is the hash the fill records (FW001).
--
-- The check is deferred to COMMIT: the fill and its booking are written in one transaction, in whatever order. A
-- fill that charges nothing needs no booking. Rows written before this migration are not touched. It reuses
-- 00761's cp_fee_booking_of, and its SQLSTATE: FW001 means here exactly what it means there.
--
-- Custom SQLSTATEs: FW001 (00761) a fee charged by a fill or a liquidation step without its fee-ledger booking,
-- here an equity fill's.

-- +goose StatementBegin
CREATE FUNCTION cp_equity_fill_fees_booked() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM cp_fee_booking_of(NEW.journal_transaction_id, NEW.account_id) f
         WHERE f.spread = NEW.spread_amount AND f.explicit = NEW.platform_fees AND f.venue = NEW.venue_fees
           AND f.network = 0 AND f.base_quantity = NEW.quantity AND f.schedule_hash = NEW.fee_schedule_hash
           AND f.disclosure_hash = NEW.fee_disclosure_hash
           AND f.disclosure_hash = encode(sha256(convert_to(NEW.fee_disclosure, 'UTF8')), 'hex')) THEN
        RAISE EXCEPTION 'FEE_NOT_BOOKED: equity fill % of order % charged account % (spread %, fees %, venue %) without its fee-ledger booking',
            NEW.id, NEW.order_id, NEW.account_id, NEW.spread_amount, NEW.platform_fees, NEW.venue_fees
            USING ERRCODE = 'FW001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_equity_fill_fees_booked() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER equity_fills_fees_booked AFTER INSERT ON equity_fills
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (NEW.spread_amount + NEW.platform_fees + NEW.venue_fees > 0)
    EXECUTE FUNCTION cp_equity_fill_fees_booked();

-- +goose Down
SELECT 1; -- protected: the binding guards the fee history of equity fills
