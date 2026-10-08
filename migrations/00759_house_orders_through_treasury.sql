-- +goose Up
-- A house order exists only as the result of a treasury action (internal/treasury; ADVANCED_FINANCIAL_MODEL.md
-- §2 TREASURY and §11 principal trading, ADR-0020). 00742 lets the internal book hold orders of a HOUSE book
-- (book_orders.owner_type = 'HOUSE', house_account_id set); 00745 records every house action the treasury takes in
-- treasury_actions, and a PLACE_ORDER action names the order it placed (treasury_actions.order_id). Nothing bound
-- the two: a house order written through the order book alone -- orders.Service.SubmitHouseOrder called by anything
-- but the treasury, or a raw INSERT -- would rest on the book with none of the treasury's controls having run (the
-- book's signing policy, the PRINCIPAL_TRADING capability, its versioned limits, Risk Kernel V2) and no record of
-- who placed it or why. This migration binds them.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. Every book_orders row whose owner_type is HOUSE is named, at COMMIT, by a PLACE_ORDER treasury action of
--      its own house book (TY007). The check is deferred to COMMIT because the treasury writes its action after
--      the order book wrote the order, in the same transaction (treasury.Service.PlaceOrder); an order inserted
--      in a transaction that commits no such action is refused, and the whole transaction with it.
--   2. An order that reached the book -- any status but REJECTED at COMMIT -- needs that action to be APPLIED. A
--      REJECTED order (the order book refused it before it touched the book) may be named by the REJECTED action
--      that recorded the refusal (TY007).
--
-- Customer orders (owner_type CUSTOMER) are bound to their economic intent by 00742 and are not affected.
--
-- Custom SQLSTATEs, continuing 00745's TY001..TY006: TY007 house order without the treasury action that placed it.

-- +goose StatementBegin
CREATE FUNCTION cp_house_order_placed_by_treasury() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    status_now text;
BEGIN
    -- The row as it stands at COMMIT: a placed order may have filled, rested or been cancelled since its insert.
    SELECT status INTO status_now FROM book_orders WHERE id = NEW.id;
    IF NOT EXISTS (
        SELECT 1 FROM treasury_actions a
         WHERE a.order_id = NEW.id AND a.kind = 'PLACE_ORDER' AND a.house_account_id = NEW.house_account_id
           AND (a.outcome = 'APPLIED' OR status_now = 'REJECTED')) THEN
        RAISE EXCEPTION 'TREASURY_HOUSE_ORDER_UNAUTHORIZED: house order % of house book % (status %) was not placed by a treasury action',
            NEW.id, NEW.house_account_id, coalesce(status_now, 'unknown') USING ERRCODE = 'TY007';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_house_order_placed_by_treasury() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER book_orders_house_placed_by_treasury AFTER INSERT ON book_orders
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.owner_type = 'HOUSE')
    EXECUTE FUNCTION cp_house_order_placed_by_treasury();

-- +goose Down
SELECT 1; -- protected: the binding guards the house book's order history
