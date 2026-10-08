-- +goose Up
-- The sandboxed strategy runtime (advanced goal §15; ADR-0020 decision 4; ADVANCED_FINANCIAL_MODEL §2:
-- SANDBOX_RUNTIME, internal/sandboxrt, capability USER_CODE_SANDBOX, mode SANDBOX).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A strategy artifact is immutable and self-verifying: its digest is the sha256 of the very wasm
--      bytes stored with it (CHECK), it exists only for a clean scan (CHECK), and no row is ever
--      updated or deleted. The bytes a run loads are the bytes that were built and reviewed.
--   2. A sandbox run is append-only and self-verifying: its output digest is the sha256 of the stored
--      output, and its run digest is sha256(artifact digest || input digest || output) (CHECKs), so a
--      reproducibility audit can re-run the artifact on the input and compare bytes.
--   3. Only an OK run has proposals; a run refused by the kill switch never started and consumed
--      nothing (CHECKs).
--   4. A run's violation count is exactly the number of its violation rows, checked at COMMIT (SX001):
--      a run can never under-report what its strategy tried.
--   5. The marketplace's references into this store are foreign keys, and a passed validation names an
--      artifact built from exactly the version's source (MK003, the marketplace's code).
--
-- Custom SQLSTATEs: SX001 violation count disagrees with the violation rows. (MK003 is raised here
-- for a validation whose artifact was built from other source; it is the marketplace's code, declared
-- in 00750.)

CREATE TABLE strategy_artifacts (
    id                     uuid PRIMARY KEY,
    artifact_digest        bytea NOT NULL UNIQUE CHECK (length(artifact_digest) = 32),
    source_digest          bytea NOT NULL CHECK (length(source_digest) = 32),
    wasm                   bytea NOT NULL CHECK (length(wasm) BETWEEN 8 AND 67108864),
    wasm_size              integer NOT NULL,
    toolchain_version      text NOT NULL CHECK (length(toolchain_version) BETWEEN 1 AND 64),
    sdk_version            text NOT NULL CHECK (sdk_version ~ '^nodalsdk/v[0-9]+@sha256:[0-9a-f]{64}$'),
    build_log_digest       bytea NOT NULL CHECK (length(build_log_digest) = 32),
    scan_report            jsonb NOT NULL CHECK (jsonb_typeof(scan_report) = 'object'),
    -- An artifact exists only for source the scanner accepted.
    scan_codes             text[] NOT NULL CHECK (cardinality(scan_codes) = 0),
    created_by_actor_type  text NOT NULL CHECK (created_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    created_by_actor_id    text NOT NULL CHECK (length(created_by_actor_id) BETWEEN 1 AND 200),
    created_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT strategy_artifacts_digest_is_the_wasm CHECK (artifact_digest = sha256(wasm)),
    CONSTRAINT strategy_artifacts_size_is_the_wasm CHECK (wasm_size = length(wasm))
);
CREATE INDEX strategy_artifacts_source_idx ON strategy_artifacts (source_digest);
CREATE TRIGGER strategy_artifacts_immutable BEFORE UPDATE OR DELETE ON strategy_artifacts
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Every source tree observed to build to an artifact. Builds are reproducible, and two trees that
-- differ only where the compiler does not look (an inline comment) build to the same bytes: the
-- artifact is the bytes, and this table records each source that produced them.
CREATE TABLE strategy_artifact_sources (
    id                     uuid PRIMARY KEY,
    artifact_digest        bytea NOT NULL REFERENCES strategy_artifacts(artifact_digest),
    source_digest          bytea NOT NULL CHECK (length(source_digest) = 32),
    toolchain_version      text NOT NULL CHECK (length(toolchain_version) BETWEEN 1 AND 64),
    sdk_version            text NOT NULL CHECK (sdk_version ~ '^nodalsdk/v[0-9]+@sha256:[0-9a-f]{64}$'),
    build_log_digest       bytea NOT NULL CHECK (length(build_log_digest) = 32),
    created_by_actor_type  text NOT NULL CHECK (created_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    created_by_actor_id    text NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (artifact_digest, source_digest)
);
CREATE TRIGGER strategy_artifact_sources_immutable BEFORE UPDATE OR DELETE ON strategy_artifact_sources
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE sandbox_runs (
    id                   uuid PRIMARY KEY,
    artifact_digest      bytea NOT NULL REFERENCES strategy_artifacts(artifact_digest),
    input_digest         bytea NOT NULL CHECK (length(input_digest) = 32),
    limits_version       text NOT NULL CHECK (length(limits_version) BETWEEN 1 AND 64),
    limits_hash          text NOT NULL CHECK (limits_hash ~ '^[0-9a-f]{64}$'),
    limits               jsonb NOT NULL CHECK (jsonb_typeof(limits) = 'object'),
    termination_reason   text NOT NULL CHECK (termination_reason IN ('OK','TIMEOUT','MEMORY_LIMIT','OUTPUT_LIMIT','TRAP',
                                                                     'INVALID_OUTPUT','KILLED_BY_SWITCH','INVALID_MODULE')),
    termination_detail   text NOT NULL CHECK (length(termination_detail) <= 1000),
    exit_code            bigint NOT NULL CHECK (exit_code BETWEEN 0 AND 4294967295),
    artifact_verified    boolean NOT NULL,
    wall_ms              bigint NOT NULL CHECK (wall_ms >= 0),
    output               bytea NOT NULL,
    output_bytes         bigint NOT NULL CHECK (output_bytes >= 0),
    stderr_bytes         bigint NOT NULL CHECK (stderr_bytes >= 0),
    stderr_digest        bytea NOT NULL CHECK (length(stderr_digest) = 32),
    peak_memory_bytes    bigint NOT NULL CHECK (peak_memory_bytes >= 0),
    output_digest        bytea NOT NULL CHECK (length(output_digest) = 32),
    run_digest           bytea NOT NULL CHECK (length(run_digest) = 32),
    proposals_count      integer NOT NULL CHECK (proposals_count >= 0),
    accepted_count       integer NOT NULL CHECK (accepted_count >= 0),
    violations_count     integer NOT NULL CHECK (violations_count >= 0),
    manifest_hash        bytea NOT NULL CHECK (length(manifest_hash) = 32),
    strategy_ref         text NOT NULL CHECK (length(strategy_ref) BETWEEN 1 AND 200),
    actor_type           text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id             text NOT NULL,
    correlation_id       text,
    started_at           timestamptz NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT sandbox_runs_output_size CHECK (output_bytes = length(output)),
    CONSTRAINT sandbox_runs_output_digest_is_the_output CHECK (output_digest = sha256(output)),
    CONSTRAINT sandbox_runs_run_digest_is_reproducible CHECK (run_digest = sha256(artifact_digest || input_digest || output)),
    CONSTRAINT sandbox_runs_accepted_within_proposals CHECK (accepted_count <= proposals_count),
    CONSTRAINT sandbox_runs_only_ok_runs_propose CHECK (termination_reason = 'OK' OR (proposals_count = 0 AND accepted_count = 0)),
    CONSTRAINT sandbox_runs_killed_never_started CHECK (termination_reason <> 'KILLED_BY_SWITCH'
        OR (wall_ms = 0 AND output_bytes = 0 AND stderr_bytes = 0 AND peak_memory_bytes = 0))
);
CREATE INDEX sandbox_runs_artifact_idx ON sandbox_runs (artifact_digest, created_at DESC);
CREATE TRIGGER sandbox_runs_immutable BEFORE UPDATE OR DELETE ON sandbox_runs
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE sandbox_run_violations (
    id           uuid PRIMARY KEY,
    run_id       uuid NOT NULL REFERENCES sandbox_runs(id),
    seq          integer NOT NULL CHECK (seq >= 1),
    record_seq   integer NOT NULL CHECK (record_seq >= 0),
    code         text NOT NULL CHECK (code IN ('OUTPUT_NOT_JSON','OUTPUT_RECORD_TYPE_UNKNOWN','OUTPUT_RECORD_MALFORMED',
                                               'OUTPUT_TRUNCATED','OUTPUT_TOO_MANY_RECORDS','KIND_NOT_PROPOSABLE',
                                               'INSTRUMENT_INVALID','INSTRUMENT_NOT_IN_SNAPSHOT','SIDE_INVALID',
                                               'QUANTITY_INVALID','LIMIT_PRICE_INVALID','LOG_INVALID',
                                               'MANIFEST_KIND_NOT_ALLOWED','MANIFEST_INSTRUMENT_NOT_ALLOWED',
                                               'MANIFEST_NOTIONAL_EXCEEDED','MANIFEST_MAX_INTENTS_EXCEEDED')),
    detail       text NOT NULL CHECK (length(detail) <= 1000),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq)
);
CREATE TRIGGER sandbox_run_violations_immutable BEFORE UPDATE OR DELETE ON sandbox_run_violations
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_sandbox_violations_complete() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    r_id     uuid;
    declared integer;
    actual   integer;
BEGIN
    IF TG_TABLE_NAME = 'sandbox_runs' THEN
        r_id := NEW.id;
    ELSE
        r_id := NEW.run_id;
    END IF;
    SELECT violations_count INTO declared FROM sandbox_runs WHERE id = r_id;
    SELECT count(*) INTO actual FROM sandbox_run_violations WHERE run_id = r_id;
    IF declared IS DISTINCT FROM actual THEN
        RAISE EXCEPTION 'VIOLATIONS_INCOMPLETE: sandbox run % declares % violations and records %', r_id, declared, actual
            USING ERRCODE = 'SX001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_sandbox_violations_complete() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER sandbox_runs_violations_complete AFTER INSERT ON sandbox_runs
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_sandbox_violations_complete();
CREATE CONSTRAINT TRIGGER sandbox_run_violations_complete AFTER INSERT ON sandbox_run_violations
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_sandbox_violations_complete();

-- ---------------------------------------------------------------------------
-- The marketplace's references into the artifact store and the run log (00750 runs first).
-- ---------------------------------------------------------------------------

ALTER TABLE marketplace_version_validations ADD CONSTRAINT marketplace_version_validations_artifact_fk
    FOREIGN KEY (artifact_digest) REFERENCES strategy_artifacts(artifact_digest);
ALTER TABLE marketplace_follow_runs ADD CONSTRAINT marketplace_follow_runs_sandbox_run_fk
    FOREIGN KEY (sandbox_run_id) REFERENCES sandbox_runs(id);
ALTER TABLE marketplace_evaluation_runs ADD CONSTRAINT marketplace_evaluation_runs_sandbox_run_fk
    FOREIGN KEY (sandbox_run_id) REFERENCES sandbox_runs(id);

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_validation_artifact_matches_source() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.artifact_digest IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM strategy_artifact_sources s JOIN marketplace_listing_versions v ON v.source_digest = s.source_digest
         WHERE s.artifact_digest = NEW.artifact_digest AND v.id = NEW.version_id) THEN
        RAISE EXCEPTION 'VALIDATION_REQUIRED: artifact for version % was not built from that version''s source', NEW.version_id
            USING ERRCODE = 'MK003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_validation_artifact_matches_source() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_version_validations_artifact_source BEFORE INSERT ON marketplace_version_validations
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_validation_artifact_matches_source();

GRANT SELECT, INSERT ON strategy_artifacts, strategy_artifact_sources, sandbox_runs, sandbox_run_violations TO cp_app;
GRANT SELECT ON strategy_artifacts, strategy_artifact_sources, sandbox_runs, sandbox_run_violations TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: artifacts and runs are the evidence of what code ran and what it proposed
