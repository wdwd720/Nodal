-- +goose Up
-- A payout destination status is not the application's to write, and a
-- destination says which country it pays into.
--
-- `payout_destinations.status` decides whether value may leave to a given bank
-- account or wallet. It was an ordinary column with an ordinary UPDATE grant:
-- `payout.SetDestinationStatus` wrote it directly, so the one fact that stands
-- between a user's Credits and somebody else's account had no transition row,
-- no actor and no reason. F-42's remedy applies here for the same reason it
-- applied to `accounts`.
--
-- ## The lifecycle, and why the names do not change
--
-- Goal §25 sketches VERIFYING / ACTIVE / DISABLED. The table already holds
-- UNVERIFIED / VERIFIED / REJECTED / DISABLED, `payout.DestinationStatus.Usable`
-- reads VERIFIED, and `payout.Capabilities` and the eligibility engine both key
-- on it. Renaming four values to match a sketch would rewrite working code and
-- a registered enum pairing to gain nothing a comment cannot say, so the
-- existing names stay and the mapping is recorded in D-060:
--
--   UNVERIFIED  = §25 VERIFYING: created, not yet usable
--   VERIFIED    = §25 ACTIVE: the provider confirmed it can pay here
--   REJECTED    = the provider refused it; terminal
--   DISABLED    = the user or an operator turned it off; terminal
--
-- Edges: UNVERIFIED -> VERIFIED | REJECTED | DISABLED, VERIFIED -> DISABLED.
-- A destination never returns from DISABLED or REJECTED. Re-adding one is a new
-- row with its own creation time, which is what makes the §25 cooldown on a
-- changed destination a fact the database can state.
--
-- ## Three columns a payout page cannot work without
--
--   country         which jurisdiction this pays into. `Capabilities` already
--                   carries SupportedCountries and ExcludedRegions and nothing
--                   could feed them, because the destination did not know.
--   masked_display  what a person recognises without Nodal holding the number:
--                   "••••4242". `display_label` is the user's own name for it.
--   sandbox         whether this destination belongs to a rehearsal. It is
--                   shown on every response that mentions it, so a sandbox
--                   destination is never displayed as a real one.
--
-- Nodal still stores no account number, card number, IBAN or key: the token is
-- the provider's, and `internal/payout` refuses an input that looks like a raw
-- number rather than a token.
--
-- Custom SQLSTATE: AD001.

ALTER TABLE payout_destinations ADD COLUMN country        text;
ALTER TABLE payout_destinations ADD COLUMN masked_display text NOT NULL DEFAULT '';
ALTER TABLE payout_destinations ADD COLUMN sandbox        boolean NOT NULL DEFAULT false;

CREATE TABLE payout_destination_transitions (
    id             uuid PRIMARY KEY,
    destination_id uuid NOT NULL REFERENCES payout_destinations(id),
    from_status    text NOT NULL,
    to_status      text NOT NULL,
    actor_type     text NOT NULL CHECK (actor_type IN ('SYSTEM','OPERATOR','USER')),
    actor_id       text NOT NULL,
    reason         text NOT NULL,
    provider_event text,
    correlation_id text,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (btrim(actor_id) <> ''),
    CHECK (btrim(reason) <> '')
);
CREATE INDEX payout_destination_transitions_idx ON payout_destination_transitions (destination_id, occurred_at);
CREATE TRIGGER payout_destination_transitions_immutable
    BEFORE UPDATE OR DELETE ON payout_destination_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER payout_destination_transitions_flag_edge
    AFTER INSERT ON payout_destination_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('destination_id', 'from_status', 'to_status', 'payout_destinations');

-- +goose StatementBegin
CREATE FUNCTION cp_destination_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    UPDATE payout_destinations
       SET status = NEW.to_status,
           -- Recorded when it becomes usable and cleared when it stops being,
           -- so a reader cannot mistake a historical confirmation for a current
           -- one.
           verified_at = CASE WHEN NEW.to_status = 'VERIFIED' THEN NEW.occurred_at ELSE NULL END
     WHERE id = NEW.destination_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'DESTINATION_TRANSITION_ORPHANED: payout_destination_transitions names destination % which does not exist',
            NEW.destination_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payout_destination_transitions_writes_the_status
    AFTER INSERT ON payout_destination_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_destination_apply_status_transition();

REVOKE EXECUTE ON FUNCTION cp_destination_apply_status_transition() FROM PUBLIC;

CREATE CONSTRAINT TRIGGER payout_destinations_require_transition
    AFTER UPDATE OF status ON payout_destinations
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'payout_destinations');

-- +goose StatementBegin
CREATE FUNCTION cp_payout_destination_born_unverified() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM 'UNVERIFIED' THEN
        RAISE EXCEPTION 'DESTINATION_BORN_VERIFIED: a payout destination is born UNVERIFIED and reaches % through a transition row; a destination inserted VERIFIED never changed, so no binding would apply to it',
            NEW.status USING ERRCODE = 'AD001';
    END IF;
    IF NEW.verified_at IS NOT NULL THEN
        RAISE EXCEPTION 'DESTINATION_BORN_VERIFIED: a payout destination cannot be born with a verified_at'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payout_destinations_born_unverified
    BEFORE INSERT ON payout_destinations
    FOR EACH ROW EXECUTE FUNCTION cp_payout_destination_born_unverified();

REVOKE UPDATE ON payout_destinations FROM cp_app;
-- The one field a person legitimately renames, and the grant that permits the
-- row lock the status change takes before checking the edge is legal (00744).
GRANT UPDATE (display_label) ON payout_destinations TO cp_app;

GRANT SELECT, INSERT ON payout_destination_transitions TO cp_app;
GRANT SELECT ON payout_destination_transitions TO cp_readonly, cp_ops;

COMMENT ON FUNCTION cp_destination_apply_status_transition() IS
    'Writes payout_destinations.status and verified_at from the transition row. cp_app holds UPDATE on display_label only, so inserting the transition row is the only way a destination becomes usable (00763, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting returns the destination status column to the application's reach
