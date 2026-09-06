-- +goose Up
-- Tamper-evident audit proof (PARTS 87, 123, 201): signed Merkle checkpoints over the per-stream
-- hash chain of audit_events (00106), the per-stream coverage index the checkpointer reads its
-- watermarks from, and the record of every full verification run.
--
-- A checkpoint covers, for every stream it names, the contiguous range [from_seq, to_seq] of
-- audit_events (from_seq is always the previous checkpoint's to_seq + 1 for that stream, or 1).
-- Its Merkle leaves are the content_hash values of those events ordered by (stream bytewise,
-- stream_seq); the tree follows RFC 6962 (leaf = sha256(0x00 || h), node = sha256(0x01 || l || r),
-- unbalanced split at the largest power of two below n, no duplicated nodes). The signature is
-- ECDSA P-256 over sha256(canonical JSON of {seq, prev_root, merkle_root, streams_covered,
-- leaf_count, build_version}) and prev_root chains every checkpoint to the previous one. The
-- canonical document plus its signature is archived (WORM bucket) before the row is inserted;
-- archive_sha256 is the hash of that object and is re-checked by every verification run.

CREATE TABLE audit_checkpoints (
    id                   uuid PRIMARY KEY,
    seq                  bigint NOT NULL UNIQUE CHECK (seq >= 1),
    prev_checkpoint_id   uuid REFERENCES audit_checkpoints(id),
    streams_covered      jsonb NOT NULL,                     -- {stream: {from_seq, to_seq, last_content_hash(base64)}}
    leaf_count           integer NOT NULL CHECK (leaf_count >= 1),
    merkle_root          bytea NOT NULL CHECK (length(merkle_root) = 32),
    prev_root            bytea CHECK (prev_root IS NULL OR length(prev_root) = 32),
    signature            bytea NOT NULL CHECK (length(signature) > 0),
    signing_key_id       text NOT NULL CHECK (signing_key_id <> ''),
    signature_algorithm  text NOT NULL CHECK (signature_algorithm IN ('ECDSA_SHA_256')),
    signer               text NOT NULL CHECK (signer IN ('kms', 'local-test')),
    archive_uri          text NOT NULL CHECK (archive_uri <> ''),
    archive_sha256       bytea NOT NULL CHECK (length(archive_sha256) = 32),
    build_version        text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CHECK ((seq = 1) = (prev_checkpoint_id IS NULL)),
    CHECK ((seq = 1) = (prev_root IS NULL))
);

-- The checkpoint chain is a database invariant, not only a worker convention: a new row must be
-- exactly seq = max + 1 and must name the current head as its predecessor with the head's root.
-- SQLSTATE AU002 (AU001 is the state-transition binding of 00603).
-- +goose StatementBegin
CREATE FUNCTION audit_checkpoints_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    head_seq  bigint;
    head_id   uuid;
    head_root bytea;
BEGIN
    SELECT seq, id, merkle_root INTO head_seq, head_id, head_root
    FROM audit_checkpoints ORDER BY seq DESC LIMIT 1;
    IF head_seq IS NULL THEN
        IF NEW.seq <> 1 OR NEW.prev_checkpoint_id IS NOT NULL OR NEW.prev_root IS NOT NULL THEN
            RAISE EXCEPTION 'AUDIT_CHECKPOINT_CHAIN: first checkpoint must be seq 1 without a predecessor (got seq %)', NEW.seq
                USING ERRCODE = 'AU002';
        END IF;
    ELSE
        IF NEW.seq <> head_seq + 1 THEN
            RAISE EXCEPTION 'AUDIT_CHECKPOINT_CHAIN: expected seq %, got %', head_seq + 1, NEW.seq USING ERRCODE = 'AU002';
        END IF;
        IF NEW.prev_checkpoint_id IS DISTINCT FROM head_id THEN
            RAISE EXCEPTION 'AUDIT_CHECKPOINT_CHAIN: prev_checkpoint_id must be % (head seq %)', head_id, head_seq USING ERRCODE = 'AU002';
        END IF;
        IF NEW.prev_root IS DISTINCT FROM head_root THEN
            RAISE EXCEPTION 'AUDIT_CHECKPOINT_CHAIN: prev_root must equal the merkle_root of checkpoint %', head_seq USING ERRCODE = 'AU002';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER audit_checkpoints_chain BEFORE INSERT ON audit_checkpoints
    FOR EACH ROW EXECUTE FUNCTION audit_checkpoints_guard();
CREATE TRIGGER audit_checkpoints_immutable BEFORE UPDATE OR DELETE ON audit_checkpoints
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Per-stream coverage index: one row per (checkpoint, stream), the normalized form of
-- streams_covered. The checkpointer reads each stream's watermark (max to_seq) here instead of
-- scanning every checkpoint's jsonb; the verifier cross-checks these rows against the signed
-- streams_covered, so the index is never the authority.
CREATE TABLE audit_checkpoint_streams (
    id                 uuid PRIMARY KEY,
    checkpoint_id      uuid NOT NULL REFERENCES audit_checkpoints(id),
    stream             text NOT NULL,
    from_seq           bigint NOT NULL CHECK (from_seq >= 1),
    to_seq             bigint NOT NULL CHECK (to_seq >= from_seq),
    last_content_hash  bytea NOT NULL CHECK (length(last_content_hash) = 32),
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (checkpoint_id, stream),
    UNIQUE (stream, to_seq)
);
CREATE INDEX audit_checkpoint_streams_watermark_idx ON audit_checkpoint_streams (stream, to_seq DESC);
CREATE TRIGGER audit_checkpoint_streams_immutable BEFORE UPDATE OR DELETE ON audit_checkpoint_streams
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Every full verification run (make verify-audit, the worker's periodic job) leaves a row, passed
-- or failed. first_failure names the stream/seq, checkpoint or archived object that failed first.
CREATE TABLE audit_verification_runs (
    id                   uuid PRIMARY KEY,
    started_at           timestamptz NOT NULL,
    finished_at          timestamptz NOT NULL CHECK (finished_at >= started_at),
    ok                   boolean NOT NULL,
    checked_events       bigint NOT NULL CHECK (checked_events >= 0),
    checked_checkpoints  integer NOT NULL CHECK (checked_checkpoints >= 0),
    first_failure        jsonb,
    build_version        text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CHECK (ok = (first_failure IS NULL))
);
CREATE INDEX audit_verification_runs_started_idx ON audit_verification_runs (started_at DESC);
CREATE TRIGGER audit_verification_runs_immutable BEFORE UPDATE OR DELETE ON audit_verification_runs
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT ON audit_checkpoints, audit_checkpoint_streams, audit_verification_runs TO cp_app;
GRANT SELECT ON audit_checkpoints, audit_checkpoint_streams, audit_verification_runs TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: audit proof history is never dropped
