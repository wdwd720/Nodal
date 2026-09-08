-- +goose Up
-- A position lot's status agrees with its quantity, and a consumed lot is not
-- refilled (F-61).
--
-- Custom SQLSTATEs: PL001 lot refilled.
--
-- The status/quantity rule below is a CHECK constraint and so raises 23514, not
-- a custom code. Saying otherwise in this header is exactly F-58: four codes in
-- this tree are documented in a header and raised by nothing, each labelling an
-- invariant a different mechanism enforces under a different SQLSTATE. A header
-- that names a code has to be a header whose migration raises it.
--
-- 00104 guards position_lots well on per-row arithmetic -- quantity_original
-- positive, quantity_open between zero and it, basis non-negative -- and not at
-- all on anything relational:
--
--   * status = 'CLOSED' iff quantity_open = 0 was enforced only by a CASE
--     expression in one Go UPDATE;
--   * quantity_open only ever decreasing was enforced only by the arithmetic
--     that computes the new value;
--   * cp_app holds table-wide UPDATE, so `UPDATE position_lots SET
--     quantity_open = quantity_original` satisfied every constraint and every
--     trigger and refilled a fully consumed lot.
--
-- The asymmetry is what makes this a finding rather than a preference:
-- credit_lots, doing the same job for Credits, gets a SECURITY DEFINER trigger
-- raising CR001 CREDIT_LOT_OVERCONSUMED (00711). The two lot tables were
-- written to different standards, and this is the weaker one -- holding cost
-- basis, which is what a tax lot and a realized-P&L figure are computed from.
--
-- Both statements are written so their expressions can never be NULL, which is
-- F-50's lesson: status and quantity_open are NOT NULL, so each comparison is
-- always true or false. A CHECK that can be NULL is a CHECK that accepts.

ALTER TABLE position_lots
    ADD CONSTRAINT position_lots_status_matches_quantity
    CHECK ((status = 'CLOSED') = (quantity_open = 0));

-- +goose StatementBegin
CREATE FUNCTION cp_position_lot_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.quantity_open > OLD.quantity_open THEN
        RAISE EXCEPTION 'POSITION_LOT_REFILLED: lot % open quantity may not rise from % to %',
            OLD.id, OLD.quantity_open, NEW.quantity_open USING ERRCODE = 'PL001';
    END IF;
    IF NEW.quantity_original <> OLD.quantity_original THEN
        RAISE EXCEPTION 'POSITION_LOT_REFILLED: lot % original quantity is fixed at acquisition', OLD.id
            USING ERRCODE = 'PL001';
    END IF;
    IF NEW.acquired_at <> OLD.acquired_at THEN
        RAISE EXCEPTION 'POSITION_LOT_REFILLED: lot % acquisition time is fixed; FIFO order depends on it', OLD.id
            USING ERRCODE = 'PL001';
    END IF;
    IF NEW.cost_basis_usd_minor <> OLD.cost_basis_usd_minor THEN
        RAISE EXCEPTION 'POSITION_LOT_REFILLED: lot % cost basis is fixed at acquisition', OLD.id
            USING ERRCODE = 'PL001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- BEFORE UPDATE, and deliberately not BEFORE DELETE: cp_app holds DELETE on no
-- table (00104 grants SELECT, INSERT, UPDATE), and the ops role's housekeeping
-- list does not include this one, so deletion is already refused by privilege
-- for every role except the owner. A trigger here would be the third statement
-- of a rule two mechanisms already make.
CREATE TRIGGER position_lots_guard BEFORE UPDATE ON position_lots
    FOR EACH ROW EXECUTE FUNCTION cp_position_lot_guard();

-- The privilege half, matching the house pattern for a table with lifecycle
-- columns (00604, 00701, 00712, 00713): the application may move only what
-- disposal moves.
REVOKE UPDATE ON position_lots FROM cp_app;
GRANT UPDATE (quantity_open, status) ON position_lots TO cp_app;
-- updated_at is deliberately not granted. The set_updated_at BEFORE trigger
-- assigns it, and PostgreSQL checks column privileges against the statement's
-- SET list rather than what a trigger writes -- so the application never needs
-- the grant. That was verified by running the disposal path against this
-- migration rather than reasoned about.

-- Existing rows are validated rather than grandfathered. Every lot written by
-- internal/positions satisfies both statements -- Acquire inserts
-- quantity_open = quantity_original with status 'OPEN', and Dispose sets the
-- status from the quantity in the same UPDATE -- so a row that fails is a lot
-- whose status and quantity already disagree, which is a finding rather than an
-- inconvenience.

-- +goose Down
SELECT 1; -- protected: reverting would let a consumed cost-basis lot be refilled
