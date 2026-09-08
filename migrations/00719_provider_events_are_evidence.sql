-- +goose Up
-- Provider webhook evidence is immutable in the database, not only by
-- convention (F-56).
--
-- 00107 calls this table "the evidence record" and its Down section says
-- "provider evidence is never dropped". Neither statement was enforced. The
-- table had no trigger of any kind, and cp_app held table-wide UPDATE -- so
-- every column was writable by anything holding the application role,
-- including the ones the evidence consists of:
--
--   payload_hash        what the provider actually sent
--   signature_verified  that we checked it was them
--   raw_ref             where the untouched body is archived
--   provider_event_id   which of their events this is
--   received_at         when it arrived
--
-- This is F-49's shape again. `internal/webhook` only ever writes the four
-- lifecycle columns, and does so in two places that are careful about it. The
-- invariant held because the one caller was well behaved, in a table whose
-- entire purpose is to be arguable against a provider who disputes what they
-- sent. Evidence that the disputing party's own software could rewrite is not
-- evidence.
--
-- Two independent controls, which is the house pattern for a table that has
-- lifecycle columns and cannot simply be forbid_mutation()'d:
--
--   1. the grant is narrowed to the columns that legitimately move, so the
--      privilege system refuses the rest (as 00604, 00701, 00712 and 00713 do);
--   2. a guard trigger refuses them again, which also covers the migration
--      role and any future grant somebody widens without reading this.
--
-- canonical_event_id is in the mutable set deliberately. Nothing writes it
-- today, but it exists to be filled in when an event is turned into a
-- canonical one -- a post-insert lifecycle assignment, which is exactly what
-- this guard permits. verification_error is NOT: see below.

-- +goose StatementBegin
CREATE FUNCTION provider_events_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'PROVIDER_EVENT_IMMUTABLE: provider evidence is never deleted'
            USING ERRCODE = 'LG003';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.provider IS DISTINCT FROM OLD.provider
       OR NEW.event_type IS DISTINCT FROM OLD.event_type
       OR NEW.provider_event_id IS DISTINCT FROM OLD.provider_event_id
       OR NEW.received_at IS DISTINCT FROM OLD.received_at
       OR NEW.provider_published_at IS DISTINCT FROM OLD.provider_published_at
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
       OR NEW.raw_ref IS DISTINCT FROM OLD.raw_ref
       OR NEW.signature_verified IS DISTINCT FROM OLD.signature_verified
       OR NEW.verification_error IS DISTINCT FROM OLD.verification_error THEN
        RAISE EXCEPTION 'PROVIDER_EVENT_IMMUTABLE: provider event % evidence fields cannot change', OLD.id
            USING ERRCODE = 'LG003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER provider_events_guard BEFORE UPDATE OR DELETE ON provider_events
    FOR EACH ROW EXECUTE FUNCTION provider_events_guard();

-- The privilege half. Revoking the table-wide UPDATE and re-granting by column
-- means a statement touching an evidence column is refused by permission
-- before the trigger is reached -- two different mechanisms saying no, so a
-- mistake in either one is not sufficient.
REVOKE UPDATE ON provider_events FROM cp_app;
GRANT UPDATE (processing_status, processed_at, error, request_id, canonical_event_id)
    ON provider_events TO cp_app;

-- signature_verified and verification_error are frozen at their inserted
-- values on purpose, and today that value is always `true` and NULL.
--
-- That is not an oversight in the handler: `internal/webhook` rejects an event
-- whose signature does not verify BEFORE it writes any row here, recording a
-- `webhook_signature_failed` row in security_events instead. So a
-- provider_events row exists only for an event that verified, and the column
-- is a record of that fact rather than a field to be revised later.
--
-- `docs/runbooks/funding-provider-compromise.md` told an incident responder
-- the opposite -- to look for `signature_verified = false` rows -- which is a
-- query that cannot return a row however bad the incident is. That is fixed in
-- the same change as F-55.

-- +goose Down
SELECT 1; -- protected: reverting would make provider evidence editable again
