-- +goose Up
-- Accepting a document is a record of what was shown, not a boolean.
--
-- Goal PART 48 asks for terms, privacy, risk, Credits and withdrawal documents
-- and is explicit that final legal copy must not be fabricated and that the
-- copy must be replaceable without a code rewrite. PART 52 lists signup and
-- profile changes among the consequential actions that must produce evidence.
--
-- A boolean `terms_accepted` answers none of the questions that are ever asked
-- about an acceptance. What was accepted? Which version? Was that the version
-- in force at the time? From which session, at which address? A row per
-- acceptance answers all of them and can never be quietly flipped, because the
-- table is append-only: withdrawing consent is a new fact, not an UPDATE.
--
-- ## What `content_hash` is for
--
-- The document text lives in `internal/terms`, embedded in the binary, with a
-- declared version. `content_hash` is sha256 over the exact bytes shown. That
-- makes one class of quiet failure impossible: copy edited without the version
-- being bumped. The acceptance no longer matches the current document, the API
-- reports the document as unaccepted, and the user is asked again. Re-asking
-- after a typo fix is a small cost; a consent record that names a version whose
-- text has since changed is a record of nothing (D-056).
--
-- The unique key therefore includes the hash: the same person may hold two
-- acceptances of one version when its bytes changed between them, and both rows
-- stay, because both happened.
--
-- ## What is not here
--
-- No document text. Storing a copy of every document beside every acceptance
-- would put the same bytes in the database tens of thousands of times to prove
-- something the hash already proves against the binary that served it.

CREATE TABLE terms_acceptances (
    id            uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users(id),
    document_id   text NOT NULL CHECK (document_id IN
                      ('TERMS_OF_SERVICE','PRIVACY_POLICY','RISK_DISCLOSURE','CREDITS_TERMS','WITHDRAWAL_DISCLOSURE')),
    version       text NOT NULL CHECK (version ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}\.[0-9]+$'),
    content_hash  text NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    -- Who accepted, from where. actor_type is USER for every row this product
    -- writes; the column exists because an acceptance recorded on somebody's
    -- behalf by an operator would be a different fact and must be legible as one.
    actor_type    text NOT NULL CHECK (actor_type IN ('USER','OPERATOR')),
    actor_id      text NOT NULL,
    session_id    uuid,
    source_ip     inet,
    user_agent    text,
    accepted_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, document_id, version, content_hash)
);
CREATE INDEX terms_acceptances_user_idx ON terms_acceptances (user_id, accepted_at DESC);

CREATE TRIGGER terms_acceptances_immutable BEFORE UPDATE OR DELETE ON terms_acceptances
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

COMMENT ON TABLE terms_acceptances IS
    'One row per document version a user accepted, with the sha256 of the exact bytes they were shown. Append-only: consent is a record of something that happened, and a changed document is a new row rather than an edit (PART 48, PART 52, D-056).';

GRANT SELECT, INSERT ON terms_acceptances TO cp_app;
GRANT SELECT ON terms_acceptances TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: an acceptance is evidence that a person agreed to a named version of a document, and evidence is not something a schema change may withdraw
