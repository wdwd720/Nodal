-- +goose Up
-- provider_evidence: the archive itself, in the database, because the launch
-- tier has no object store.
--
-- Every archived object so far has lived in S3 with Object Lock, and the
-- guarantee was the storage engine's: a retained object cannot be rewritten or
-- deleted before its retention expires, by anyone, including the account that
-- owns the bucket. The $0 launch tier has no bucket to put that lock on -- R2
-- asks for a credit card and S3 is not free -- so the choice is between the
-- evidence living in Postgres and there being no evidence at all.
--
-- The point of the archive is that nobody can rewrite it, so it is worth being
-- exact about which half of that survives the move and which half does not.
--
-- What Postgres cannot do: WORM. There is no storage-level lock here and no
-- way to build one. A superuser, the table owner, or anyone who reaches the
-- data directory can rewrite a row, and neither a grant nor a trigger stops
-- them; S3 Object Lock in COMPLIANCE mode does. That difference is real, it is
-- not papered over anywhere in this file, and anyone reasoning about what this
-- archive proves should reason from the paragraph below and not from the S3
-- one it replaces.
--
-- What Postgres can do, and what the write-once property here actually rests
-- on, is two controls neither of which is sufficient alone -- the house pattern
-- 00719 states plainly ("two different mechanisms saying no, so a mistake in
-- either one is not sufficient"):
--
--   1. The privilege model, which is the load-bearing one. cp_app -- the role
--      every process that archives evidence connects as -- holds INSERT and
--      SELECT and nothing else. An UPDATE or a DELETE from the application,
--      whether it is a defect or somebody holding the application's
--      credential, is refused by the privilege system before it reaches a row.
--      This is the same control 00604, 00701, 00713 and 00719 already rely on.
--
--   2. A guard trigger, which covers what the grant does not: the migration
--      role, and any future grant somebody widens without reading this. It
--      refuses every UPDATE, and refuses a DELETE while the object is still
--      inside its retention window -- the one part of Object Lock that can be
--      emulated here, binding everyone who has not first disabled the trigger.
--
-- Retention is a timestamp rather than a mode plus a duration because there is
-- nothing to hand a duration to. retain_until NULL means "no expiry": the
-- guard refuses the DELETE for good, which is the conservative reading and the
-- one 00107 and 00719 already take for provider evidence.
--
-- The sha256 is stored beside the bytes so a reader can tell an intact object
-- from an altered one. internal/proof.PgArchive re-hashes on every Get and
-- refuses to return bytes whose digest no longer matches, which is the only
-- honest thing to do with an archive whose engine cannot promise immutability:
-- an archive that hands back silently-altered evidence is worse than one that
-- fails, because the first is believed.

CREATE TABLE provider_evidence (
    id            uuid PRIMARY KEY,
    key           text NOT NULL UNIQUE,
    body          bytea NOT NULL,
    sha256        bytea NOT NULL CHECK (octet_length(sha256) = 32),
    -- Derivable from body, and stored anyway: the whole archive lives inside a
    -- 0.5 GB database, and `SELECT sum(byte_len)` answers "how much room is
    -- left" without dragging every body back out of TOAST. The CHECK is what
    -- keeps it from ever becoming a second, disagreeing account of the object.
    byte_len      integer NOT NULL CHECK (byte_len = octet_length(body)),
    retain_until  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- A pruning job on this tier asks exactly one question -- "what is out of
-- retention" -- and the rows it must never touch are the NULL ones, so they do
-- not belong in the index.
CREATE INDEX provider_evidence_retain_until_idx ON provider_evidence (retain_until)
    WHERE retain_until IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION provider_evidence_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'PROVIDER_EVIDENCE_IMMUTABLE: archived object % is never rewritten', OLD.key
            USING ERRCODE = 'LG003';
    END IF;
    IF OLD.retain_until IS NULL OR OLD.retain_until > now() THEN
        RAISE EXCEPTION 'PROVIDER_EVIDENCE_RETAINED: archived object % is retained until %',
            OLD.key, COALESCE(OLD.retain_until::text, 'further notice')
            USING ERRCODE = 'LG003';
    END IF;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER provider_evidence_guard BEFORE UPDATE OR DELETE ON provider_evidence
    FOR EACH ROW EXECUTE FUNCTION provider_evidence_guard();

-- Write once, read many, and nothing else. TRUNCATE is revoked alongside
-- DELETE because a TRUNCATE privilege makes a DELETE revocation decorative,
-- and it is one statement rather than one row, so the trigger never sees it.
--
-- cp_app receives no default table privileges (docker/postgres/init/001_roles.sql),
-- so today the REVOKE removes nothing. It is written anyway: this table's
-- guarantee should be legible in the migration that creates it rather than
-- inferred from a bootstrap file, and if the default privileges are ever
-- widened this line is what keeps the archive write-once.
GRANT SELECT, INSERT ON provider_evidence TO cp_app;
REVOKE UPDATE, DELETE, TRUNCATE ON provider_evidence FROM cp_app;
GRANT SELECT ON provider_evidence TO cp_readonly, cp_ops;

COMMENT ON TABLE provider_evidence IS
    'Write-once evidence archive for the object-storage-free launch tier (internal/proof.PgArchive, pg:// URIs). Write-once comes from the privilege model -- cp_app holds INSERT and SELECT only -- and from provider_evidence_guard, NOT from the storage engine: Postgres has no equivalent of S3 Object Lock, and a role with wider privileges can still rewrite a row.';
COMMENT ON COLUMN provider_evidence.sha256 IS
    'sha256 of body as stored. Get re-hashes and refuses to return bytes that no longer match it.';
COMMENT ON COLUMN provider_evidence.retain_until IS
    'The object may not be deleted before this instant. NULL means no expiry, which the guard trigger reads as "never".';

-- +goose Down
SELECT 1; -- protected: dropping this drops the archive the checkpoints and the provider evidence point at, and there is no bucket holding a second copy
