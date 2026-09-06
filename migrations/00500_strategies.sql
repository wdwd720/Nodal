-- +goose Up
-- Strategy registry, immutable compiled versions (typed IR), compile attempts with model provenance and declared
-- data dependencies (PARTS 61-65, 170, 174, 176). See docs/architecture/STRATEGY_IR.md.
-- Custom SQLSTATEs: ST001 immutable strategy version field.

CREATE TABLE strategies (
    id                     uuid PRIMARY KEY,
    owner_account_id       uuid NOT NULL REFERENCES accounts(id),
    owner_user_id          uuid NOT NULL REFERENCES users(id),
    name                   text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    description            text NOT NULL DEFAULT '',
    source_kind            text NOT NULL CHECK (source_kind IN ('NATURAL_LANGUAGE','TYPESCRIPT_SDK','CLONE')),
    status                 text NOT NULL CHECK (status IN ('ACTIVE','ARCHIVED')),
    current_version_id     uuid,                                 -- FK added below once strategy_versions exists
    created_by_actor_type  text NOT NULL CHECK (created_by_actor_type <> 'AGENT'),
    created_by_actor_id    text NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_account_id, name)
);
CREATE INDEX strategies_owner_idx ON strategies (owner_account_id);
CREATE TRIGGER strategies_updated_at BEFORE UPDATE ON strategies FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- One row per successfully compiled IR. Compiled fields are immutable; only lifecycle columns change.
CREATE TABLE strategy_versions (
    id                     uuid PRIMARY KEY,
    strategy_id            uuid NOT NULL REFERENCES strategies(id),
    version                integer NOT NULL CHECK (version >= 1),
    schema_version         integer NOT NULL CHECK (schema_version >= 1),
    ir                     jsonb NOT NULL,
    ir_hash                bytea NOT NULL CHECK (length(ir_hash) = 32),        -- semantic sha256 (STRATEGY_IR.md §2)
    effect_set             text[] NOT NULL CHECK (
                               effect_set <@ ARRAY['READ_MARKET_DATA','READ_ONCHAIN_DATA','READ_APPROVED_SOCIAL_DATA',
                                                   'READ_WALLET_INTELLIGENCE','CALL_MODEL','COMMIT_PREDICTION','CREATE_TRADE_INTENT']::text[]),
    status                 text NOT NULL CHECK (status IN ('COMPILED','ACCEPTED','REJECTED','SUPERSEDED','REVOKED')),
    source_kind            text NOT NULL CHECK (source_kind IN ('NATURAL_LANGUAGE','TYPESCRIPT_SDK','CLONE')),
    source_hash            bytea NOT NULL,
    compiler_version       text NOT NULL,
    sdk_version            text,
    compile_attempt_id     uuid,                                 -- FK (deferred) added after compile_attempts
    parent_version_id      uuid REFERENCES strategy_versions(id),
    risk_policy_version    text NOT NULL,
    risk_policy_hash       bytea NOT NULL,
    model_budget           jsonb NOT NULL,
    data_budget            jsonb NOT NULL,
    envelope_requirements  jsonb NOT NULL,
    human_readable         text NOT NULL,                        -- strategy.Render output shown to the owner
    built_at               timestamptz NOT NULL,
    accepted_by_user_id    uuid REFERENCES users(id),
    accepted_at            timestamptz,
    status_reason          text,
    superseded_at          timestamptz,
    revoked_at             timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (strategy_id, version),
    CHECK (status <> 'ACCEPTED' OR (accepted_by_user_id IS NOT NULL AND accepted_at IS NOT NULL))
);
CREATE INDEX strategy_versions_hash_idx ON strategy_versions (ir_hash);
CREATE INDEX strategy_versions_status_idx ON strategy_versions (strategy_id, status);
ALTER TABLE strategies ADD CONSTRAINT strategies_current_version_fk FOREIGN KEY (current_version_id) REFERENCES strategy_versions(id);

-- +goose StatementBegin
CREATE FUNCTION strategy_versions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'STRATEGY_VERSION_IMMUTABLE: versions are never deleted' USING ERRCODE = 'ST001';
    END IF;
    IF NEW.ir IS DISTINCT FROM OLD.ir OR NEW.ir_hash IS DISTINCT FROM OLD.ir_hash
       OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.effect_set IS DISTINCT FROM OLD.effect_set
       OR NEW.strategy_id IS DISTINCT FROM OLD.strategy_id OR NEW.version IS DISTINCT FROM OLD.version
       OR NEW.source_kind IS DISTINCT FROM OLD.source_kind OR NEW.source_hash IS DISTINCT FROM OLD.source_hash
       OR NEW.compiler_version IS DISTINCT FROM OLD.compiler_version OR NEW.sdk_version IS DISTINCT FROM OLD.sdk_version
       OR NEW.compile_attempt_id IS DISTINCT FROM OLD.compile_attempt_id OR NEW.parent_version_id IS DISTINCT FROM OLD.parent_version_id
       OR NEW.risk_policy_version IS DISTINCT FROM OLD.risk_policy_version OR NEW.risk_policy_hash IS DISTINCT FROM OLD.risk_policy_hash
       OR NEW.model_budget IS DISTINCT FROM OLD.model_budget OR NEW.data_budget IS DISTINCT FROM OLD.data_budget
       OR NEW.envelope_requirements IS DISTINCT FROM OLD.envelope_requirements
       OR NEW.human_readable IS DISTINCT FROM OLD.human_readable OR NEW.built_at IS DISTINCT FROM OLD.built_at THEN
        RAISE EXCEPTION 'STRATEGY_VERSION_IMMUTABLE: compiled fields of % cannot change; compile a new version', OLD.id
            USING ERRCODE = 'ST001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER strategy_versions_guard BEFORE UPDATE OR DELETE ON strategy_versions FOR EACH ROW EXECUTE FUNCTION strategy_versions_guard();
CREATE TRIGGER strategy_versions_updated_at BEFORE UPDATE ON strategy_versions FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Every compilation attempt is recorded, successful or not (PART 64), with model provenance (PART 65).
CREATE TABLE compile_attempts (
    id                       uuid PRIMARY KEY,
    strategy_id              uuid NOT NULL REFERENCES strategies(id),
    request_id               text NOT NULL,                      -- one compile request => bounded attempts
    attempt_no               integer NOT NULL CHECK (attempt_no BETWEEN 1 AND 8),
    source_kind              text NOT NULL CHECK (source_kind IN ('NATURAL_LANGUAGE','TYPESCRIPT_SDK','CLONE')),
    input_hash               bytea NOT NULL,
    input_ref                text,                               -- archive URI, retention class MODEL_IO
    prompt_template_version  text,
    model_provider           text,
    model_id                 text,
    request_at               timestamptz,
    response_at              timestamptz,
    input_tokens             bigint CHECK (input_tokens IS NULL OR input_tokens >= 0),
    output_tokens            bigint CHECK (output_tokens IS NULL OR output_tokens >= 0),
    cost_usd_minor           bigint NOT NULL DEFAULT 0 CHECK (cost_usd_minor >= 0),
    output_hash              bytea,
    output_ref               text,
    structured_output        jsonb,
    parse_result             text NOT NULL CHECK (parse_result IN ('OK','INVALID_JSON','SCHEMA_VIOLATION','TOO_LARGE','NOT_ATTEMPTED')),
    stage_reached            text NOT NULL CHECK (stage_reached IN ('PROMPT','RESPONSE','PARSE','STRUCTURAL','TYPE','EFFECT','RISK_COMPAT','RENDER','ACCEPTED')),
    outcome                  text NOT NULL CHECK (outcome IN ('SUCCESS','REJECTED','NEEDS_CLARIFICATION','MODEL_UNAVAILABLE','TIMEOUT')),
    failure_codes            text[] NOT NULL DEFAULT '{}',
    clarifications           jsonb NOT NULL DEFAULT '[]'::jsonb,
    explanation              jsonb NOT NULL DEFAULT '{}'::jsonb,  -- structured, user-visible; never chain-of-thought
    strategy_version_id      uuid REFERENCES strategy_versions(id),
    requested_by_user_id     uuid REFERENCES users(id),
    correlation_id           text,
    created_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (request_id, attempt_no),
    CHECK (outcome <> 'SUCCESS' OR strategy_version_id IS NOT NULL)
);
CREATE INDEX compile_attempts_strategy_idx ON compile_attempts (strategy_id, created_at DESC);
CREATE TRIGGER compile_attempts_immutable BEFORE UPDATE OR DELETE ON compile_attempts
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
-- Version and its successful attempt are written in one transaction; the attempt row lands last.
ALTER TABLE strategy_versions ADD CONSTRAINT strategy_versions_compile_attempt_fk
    FOREIGN KEY (compile_attempt_id) REFERENCES compile_attempts(id) DEFERRABLE INITIALLY DEFERRED;

-- Declared data dependencies with staleness bound and dependency versioning (PARTS 174, 176).
CREATE TABLE strategy_dependencies (
    id                    uuid PRIMARY KEY,
    strategy_version_id   uuid NOT NULL REFERENCES strategy_versions(id),
    name                  text NOT NULL CHECK (name ~ '^[a-z][a-z0-9_]{0,63}$'),
    kind                  text NOT NULL CHECK (kind IN ('PRICE','ONCHAIN','WALLET_EVENT','SOCIAL','WALLET_INTELLIGENCE','MODEL','FEATURE')),
    effect                text NOT NULL CHECK (effect IN ('READ_MARKET_DATA','READ_ONCHAIN_DATA','READ_APPROVED_SOCIAL_DATA','READ_WALLET_INTELLIGENCE','CALL_MODEL')),
    tool_code             text NOT NULL,
    tool_version          integer NOT NULL CHECK (tool_version >= 1),          -- FK to tools(code, version) added in 00502
    dependency_version    integer NOT NULL CHECK (dependency_version >= 1),
    params                jsonb NOT NULL DEFAULT '{}'::jsonb,
    params_hash           bytea NOT NULL,
    max_age_ms            bigint NOT NULL CHECK (max_age_ms > 0),
    required              boolean NOT NULL DEFAULT true,
    instrument_id         uuid REFERENCES instruments(id),
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (strategy_version_id, name)
);
CREATE INDEX strategy_dependencies_tool_idx ON strategy_dependencies (tool_code, tool_version);
CREATE TRIGGER strategy_dependencies_immutable BEFORE UPDATE OR DELETE ON strategy_dependencies
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON strategies, strategy_versions TO cp_app;
GRANT SELECT, INSERT ON compile_attempts, strategy_dependencies TO cp_app;
GRANT SELECT ON strategies, strategy_versions, compile_attempts, strategy_dependencies TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: compiled strategy versions are referenced by predictions, intents and financial history
