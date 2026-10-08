-- +goose Up
-- Every fee and spread the internal book, the paper venue and margin liquidation charge is booked through the fee
-- ledger, in the transaction that charges it (internal/orders, internal/margin, internal/feeledger;
-- ADVANCED_FINANCIAL_MODEL.md §5, ADR-0020). 00746 makes a fee booking exactly what its stored disclosure says and
-- what its journal carries; 00742 and 00740 record each fill and each liquidation step with the fees it charged.
-- Nothing bound the two: a fill or a step could charge a customer a fee the fee ledger never saw, so the customer
-- could not find the charge there and the spread audit could not count it. This migration binds them.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. An internal-book trade (book_trades) that charges a party a fee commits only with that party's fee booking
--      against the trade's own journal: the booking's payer is the party, its explicit fee (platform plus strategy)
--      is exactly the fee the trade records, it charges no spread, venue or network cost, and its disclosure was
--      priced under the schedule the trade names (maker_schedule_hash, taker_schedule_hash). A house party, which
--      is no customer account, can therefore never be charged a fee at all (FW001).
--   2. A paper-venue fill (paper_fills) that charges anything commits only with its payer's fee booking against the
--      fill's own journal, carrying exactly the fill's spread, explicit fees, venue and network costs, under the
--      schedule the fill names, and whose disclosure is byte for byte the fill's own (the same hash) (FW001).
--   3. A margin liquidation step (margin_liquidation_steps) that charges a fee commits only with the customer's fee
--      booking against the step's LIQUIDATION journal, carrying exactly that fee as its explicit fee and nothing
--      else, disclosed as a share of exactly the step's proceeds (FW001).
--
-- The checks are deferred to COMMIT: a subsystem writes the fill or the step and the booking in one transaction, in
-- whatever order. A row that charges nothing needs no booking. Rows written before this migration are not touched.
--
-- Custom SQLSTATEs: FW001 a fee charged by a fill or a liquidation step without its fee-ledger booking.

-- One booking of a journal for a payer, with its disclosure's schedule and document hashes.
-- +goose StatementBegin
CREATE FUNCTION cp_fee_booking_of(p_journal uuid, p_account uuid)
RETURNS TABLE (spread numeric, explicit numeric, venue numeric, network numeric, schedule_hash text,
               disclosure_hash text, base_quantity numeric)
LANGUAGE sql STABLE AS $$
    SELECT b.spread_amount, b.platform_fee + b.strategy_fee, b.venue_fee, b.network_fee, d.schedule_hash,
           d.disclosure_hash, d.base_quantity
      FROM fee_bookings b JOIN fee_quote_disclosures d ON d.id = b.disclosure_id
     WHERE b.journal_transaction_id = p_journal AND b.account_id = p_account;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_fee_booking_of(uuid, uuid) SET search_path = pg_catalog, public, pg_temp;

-- +goose StatementBegin
CREATE FUNCTION cp_book_trade_fees_booked() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    party   record;
BEGIN
    FOR party IN
        SELECT 'maker' AS role, NEW.maker_owner_id AS owner_id, NEW.maker_fee AS fee, NEW.maker_schedule_hash AS schedule_hash
        UNION ALL
        SELECT 'taker', NEW.taker_owner_id, NEW.taker_fee, NEW.taker_schedule_hash
    LOOP
        CONTINUE WHEN party.fee = 0;
        IF NOT EXISTS (
            SELECT 1 FROM cp_fee_booking_of(NEW.journal_transaction_id, party.owner_id) f
             WHERE f.explicit = party.fee AND f.spread = 0 AND f.venue = 0 AND f.network = 0
               AND f.schedule_hash = party.schedule_hash) THEN
            RAISE EXCEPTION 'FEE_NOT_BOOKED: trade % of book % charged its % (%) a fee of % that the fee ledger did not book',
                NEW.trade_seq, NEW.book_id, party.role, party.owner_id, party.fee USING ERRCODE = 'FW001';
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_book_trade_fees_booked() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER book_trades_fees_booked AFTER INSERT ON book_trades
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.maker_fee > 0 OR NEW.taker_fee > 0)
    EXECUTE FUNCTION cp_book_trade_fees_booked();

-- +goose StatementBegin
CREATE FUNCTION cp_paper_fill_fees_booked() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM cp_fee_booking_of(NEW.journal_transaction_id, NEW.account_id) f
         WHERE f.spread = NEW.spread_amount AND f.explicit = NEW.platform_fee_amount AND f.venue = NEW.venue_fee_amount
           AND f.network = NEW.network_fee_amount AND f.schedule_hash = NEW.schedule_hash
           AND f.disclosure_hash = NEW.disclosure_hash) THEN
        RAISE EXCEPTION 'FEE_NOT_BOOKED: paper fill % charged account % (spread %, fees %, venue %, network %) without its fee-ledger booking',
            NEW.id, NEW.account_id, NEW.spread_amount, NEW.platform_fee_amount, NEW.venue_fee_amount, NEW.network_fee_amount
            USING ERRCODE = 'FW001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_paper_fill_fees_booked() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER paper_fills_fees_booked AFTER INSERT ON paper_fills
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (NEW.spread_amount + NEW.platform_fee_amount + NEW.venue_fee_amount + NEW.network_fee_amount > 0)
    EXECUTE FUNCTION cp_paper_fill_fees_booked();

-- +goose StatementBegin
CREATE FUNCTION cp_liquidation_fee_booked() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    payer uuid;
BEGIN
    SELECT ma.account_id INTO payer
      FROM margin_liquidation_cases c JOIN margin_accounts ma ON ma.id = c.margin_account_id
     WHERE c.id = NEW.case_id;
    IF NEW.trade_journal_transaction_id IS NULL OR NOT EXISTS (
        SELECT 1 FROM cp_fee_booking_of(NEW.trade_journal_transaction_id, payer) f
         WHERE f.explicit = NEW.fee AND f.spread = 0 AND f.venue = 0 AND f.network = 0 AND f.base_quantity = NEW.proceeds) THEN
        RAISE EXCEPTION 'FEE_NOT_BOOKED: liquidation step % of case % charged account % a fee of % without its fee-ledger booking',
            NEW.step_no, NEW.case_id, payer, NEW.fee USING ERRCODE = 'FW001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_liquidation_fee_booked() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER margin_liquidation_steps_fee_booked AFTER INSERT ON margin_liquidation_steps
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.fee > 0)
    EXECUTE FUNCTION cp_liquidation_fee_booked();

-- +goose Down
SELECT 1; -- protected: the binding guards the fee history of fills and liquidations
