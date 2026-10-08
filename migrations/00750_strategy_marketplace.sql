-- +goose Up
-- The strategy marketplace (advanced goal §14; ADR-0020; ADVANCED_FINANCIAL_MODEL §2: MARKETPLACE,
-- internal/marketplace, capability STRATEGY_MARKETPLACE, modes PAPER and SANDBOX).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A listing version is immutable apart from its lifecycle. Its manifest (the typed permissions a
--      strategy is held to at run time), source, configuration, fee metadata and provenance can never
--      change (MK004); the stored manifest hash is the sha256 of the stored manifest bytes (CHECK), and
--      the stored source digest is recomputed from the stored files at COMMIT (MK005). A strategy
--      therefore can never gain a permission its manifest did not state when it was reviewed.
--   2. The lifecycle is a bound state machine: DRAFT -> SUBMITTED -> VALIDATED -> IN_REVIEW ->
--      APPROVED | REJECTED, SUBMITTED/VALIDATED -> REJECTED, APPROVED -> DEPRECATED. An illegal edge is
--      refused (MK001) and every change needs a transition row naming both endpoints (AU001). The
--      moderation state has its own bound machine (MK007).
--   3. VALIDATED needs a PASSED validation (a clean scan and a built artifact) (MK003).
--   4. Dual control: a review is written by an OPERATOR who is neither the listing's creator nor the
--      version's author, and APPROVED needs such an APPROVE review (MK002). The creator can never
--      approve their own listing, whatever the application does.
--   5. An install is PAPER and nothing else (CHECK marketplace_installs_mode_check): the marketplace
--      cannot allocate real customer money. An install needs an APPROVED, CLEARED version (MK006).
--   6. A follow intent recorded as DISPATCHED carries an ALLOW Risk V2 decision for that very intent
--      and a MARKETPLACE effect for it (MK009): nothing a strategy proposes reaches execution without
--      the risk kernel.
--   7. A fork names an APPROVED or DEPRECATED parent version (MK010).
--   8. Every history table is append-only.
--
-- Migration 00751 adds the foreign keys from this migration's artifact and run references to the
-- sandbox runtime's tables, which are created there.
--
-- Custom SQLSTATEs: MK001 illegal version transition, MK002 review dual control, MK003 validation
-- required, MK004 version identity frozen, MK005 source digest mismatch, MK006 install not allowed or
-- install identity changed, MK007 illegal moderation transition, MK008 illegal install transition,
-- MK009 dispatch without an ALLOW decision, MK010 fork of a non-public version.

-- ---------------------------------------------------------------------------
-- 1. Listings and versions
-- ---------------------------------------------------------------------------

CREATE TABLE marketplace_listings (
    id                      uuid PRIMARY KEY,
    creator_account_id      uuid NOT NULL REFERENCES accounts(id),
    creator_actor_type      text NOT NULL CHECK (creator_actor_type IN ('USER','OPERATOR','SERVICE')),
    creator_actor_id        text NOT NULL CHECK (length(creator_actor_id) BETWEEN 1 AND 200),
    title                   text NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    forked_from_version_id  uuid,
    created_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX marketplace_listings_creator_idx ON marketplace_listings (creator_account_id, created_at DESC);
CREATE TRIGGER marketplace_listings_immutable BEFORE UPDATE OR DELETE ON marketplace_listings
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE marketplace_listing_versions (
    id                       uuid PRIMARY KEY,
    listing_id               uuid NOT NULL REFERENCES marketplace_listings(id),
    version                  integer NOT NULL CHECK (version BETWEEN 1 AND 100000),
    status                   text NOT NULL CHECK (status IN ('DRAFT','SUBMITTED','VALIDATED','IN_REVIEW','APPROVED',
                                                             'REJECTED','DEPRECATED')),
    moderation_state         text NOT NULL CHECK (moderation_state IN ('UNREVIEWED','CLEARED','FLAGGED','TAKEN_DOWN')),
    description              text NOT NULL CHECK (length(description) BETWEEN 1 AND 4000),
    risk_class               text NOT NULL CHECK (risk_class IN ('CONSERVATIVE','MODERATE','AGGRESSIVE','SPECULATIVE')),
    supported_instrument_ids text[] NOT NULL CHECK (cardinality(supported_instrument_ids) <= 256),
    -- The manifest exactly as Go canonicalized and hashed it.
    manifest_canonical       bytea NOT NULL CHECK (length(manifest_canonical) BETWEEN 2 AND 65536),
    manifest_hash            bytea NOT NULL CHECK (length(manifest_hash) = 32),
    source_digest            bytea NOT NULL CHECK (length(source_digest) = 32),
    source_commit            text NOT NULL CHECK (length(source_commit) BETWEEN 1 AND 100),
    backtest_evidence_ref    text NOT NULL DEFAULT '' CHECK (length(backtest_evidence_ref) <= 500),
    data_dependencies        text[] NOT NULL CHECK (cardinality(data_dependencies) <= 64),
    -- Fee model: metadata only. Nothing in this migration or in internal/marketplace charges a fee.
    performance_fee_bps      integer NOT NULL CHECK (performance_fee_bps BETWEEN 0 AND 5000),
    management_fee_bps       integer NOT NULL CHECK (management_fee_bps BETWEEN 0 AND 500),
    fee_high_water_mark      boolean NOT NULL,
    -- The approved configuration a run sees, string to string.
    config                   jsonb NOT NULL CHECK (jsonb_typeof(config) = 'object'),
    created_by_actor_type    text NOT NULL CHECK (created_by_actor_type IN ('USER','OPERATOR','SERVICE')),
    created_by_actor_id      text NOT NULL CHECK (length(created_by_actor_id) BETWEEN 1 AND 200),
    rejection_code           text CHECK (rejection_code IS NULL OR rejection_code ~ '^[A-Z][A-Z0-9_]{1,63}$'),
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (listing_id, version),
    CONSTRAINT marketplace_listing_versions_manifest_hash_matches CHECK (manifest_hash = sha256(manifest_canonical)),
    CONSTRAINT marketplace_listing_versions_rejected_has_code CHECK (status <> 'REJECTED' OR rejection_code IS NOT NULL)
);
CREATE INDEX marketplace_listing_versions_status_idx ON marketplace_listing_versions (status, moderation_state);
CREATE TRIGGER marketplace_listing_versions_updated_at BEFORE UPDATE ON marketplace_listing_versions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE marketplace_listings ADD CONSTRAINT marketplace_listings_forked_from_fk
    FOREIGN KEY (forked_from_version_id) REFERENCES marketplace_listing_versions(id);

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_fork_parent_is_public() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    parent_status text;
BEGIN
    IF NEW.forked_from_version_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT status INTO parent_status FROM marketplace_listing_versions WHERE id = NEW.forked_from_version_id;
    IF parent_status IS NULL OR parent_status NOT IN ('APPROVED','DEPRECATED') THEN
        RAISE EXCEPTION 'FORK_PARENT_NOT_PUBLIC: listing % forks version % which is %', NEW.id, NEW.forked_from_version_id,
            coalesce(parent_status, 'missing') USING ERRCODE = 'MK010';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_fork_parent_is_public() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_listings_fork_parent BEFORE INSERT ON marketplace_listings
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_fork_parent_is_public();

-- ---------------------------------------------------------------------------
-- 2. Source files and the recomputed source digest
-- ---------------------------------------------------------------------------

CREATE TABLE marketplace_version_sources (
    id          uuid PRIMARY KEY,
    version_id  uuid NOT NULL REFERENCES marketplace_listing_versions(id),
    path        text NOT NULL CHECK (path ~ '^[a-z0-9_]{1,64}\.go$'),
    content     bytea NOT NULL CHECK (length(content) <= 65536),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (version_id, path)
);
CREATE TRIGGER marketplace_version_sources_immutable BEFORE UPDATE OR DELETE ON marketplace_version_sources
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- The digest internal/sandboxrt.Source.Digest computes:
--   sha256('nodal-strategy-source/v1' || LF || for each path in byte order: sha256(path) || sha256(content))
-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_source_digest(p_version uuid) RETURNS bytea
LANGUAGE sql STABLE AS $$
    SELECT sha256(convert_to('nodal-strategy-source/v1' || E'\n', 'UTF8') ||
                  coalesce(string_agg(sha256(convert_to(path, 'UTF8')) || sha256(content), ''::bytea
                                      ORDER BY path COLLATE "C"), ''::bytea))
      FROM marketplace_version_sources
     WHERE version_id = p_version;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_check_source_digest() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_id     uuid;
    declared bytea;
BEGIN
    IF TG_TABLE_NAME = 'marketplace_listing_versions' THEN
        v_id := NEW.id;
    ELSE
        v_id := NEW.version_id;
    END IF;
    SELECT source_digest INTO declared FROM marketplace_listing_versions WHERE id = v_id;
    IF declared IS DISTINCT FROM cp_marketplace_source_digest(v_id) THEN
        RAISE EXCEPTION 'SOURCE_DIGEST_MISMATCH: version % stores files that do not hash to its source digest', v_id
            USING ERRCODE = 'MK005';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_check_source_digest() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER marketplace_listing_versions_source_digest AFTER INSERT ON marketplace_listing_versions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_marketplace_check_source_digest();
CREATE CONSTRAINT TRIGGER marketplace_version_sources_digest AFTER INSERT ON marketplace_version_sources
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_marketplace_check_source_digest();

-- ---------------------------------------------------------------------------
-- 3. Validations and reviews
-- ---------------------------------------------------------------------------

CREATE TABLE marketplace_version_validations (
    id                      uuid PRIMARY KEY,
    version_id              uuid NOT NULL UNIQUE REFERENCES marketplace_listing_versions(id),
    outcome                 text NOT NULL CHECK (outcome IN ('PASSED','FAILED')),
    artifact_digest         bytea CHECK (artifact_digest IS NULL OR length(artifact_digest) = 32),
    failure_reason          text CHECK (failure_reason IS NULL OR failure_reason IN ('SCAN_REJECTED','COMPILE_FAILED','BUILD_TIMEOUT')),
    scan_codes              text[] NOT NULL,
    build_log_digest        bytea CHECK (build_log_digest IS NULL OR length(build_log_digest) = 32),
    toolchain_version       text NOT NULL,
    sdk_version             text NOT NULL,
    runtime_version         text NOT NULL,
    validated_by_actor_type text NOT NULL CHECK (validated_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    validated_by_actor_id   text NOT NULL,
    validated_at            timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT marketplace_version_validations_passed_has_artifact CHECK ((outcome = 'PASSED') = (artifact_digest IS NOT NULL)),
    CONSTRAINT marketplace_version_validations_failed_has_reason CHECK ((outcome = 'FAILED') = (failure_reason IS NOT NULL)),
    CONSTRAINT marketplace_version_validations_passed_scan_clean CHECK (outcome <> 'PASSED' OR cardinality(scan_codes) = 0)
);
CREATE TRIGGER marketplace_version_validations_immutable BEFORE UPDATE OR DELETE ON marketplace_version_validations
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE marketplace_reviews (
    id                  uuid PRIMARY KEY,
    version_id          uuid NOT NULL REFERENCES marketplace_listing_versions(id),
    reviewer_actor_type text NOT NULL CHECK (reviewer_actor_type IN ('OPERATOR')),
    reviewer_actor_id   text NOT NULL CHECK (length(reviewer_actor_id) BETWEEN 1 AND 200),
    decision            text NOT NULL CHECK (decision IN ('APPROVE','REJECT')),
    reason_code         text NOT NULL CHECK (reason_code ~ '^[A-Z][A-Z0-9_]{1,63}$'),
    note                text NOT NULL DEFAULT '' CHECK (length(note) <= 2000),
    reviewed_at         timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX marketplace_reviews_version_idx ON marketplace_reviews (version_id, reviewed_at);
CREATE TRIGGER marketplace_reviews_immutable BEFORE UPDATE OR DELETE ON marketplace_reviews
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_review_dual_control() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    creator text;
    author  text;
BEGIN
    SELECT l.creator_actor_id, v.created_by_actor_id INTO creator, author
      FROM marketplace_listing_versions v JOIN marketplace_listings l ON l.id = v.listing_id
     WHERE v.id = NEW.version_id;
    IF NEW.reviewer_actor_id = creator OR NEW.reviewer_actor_id = author THEN
        RAISE EXCEPTION 'REVIEW_DUAL_CONTROL: % may not review version % they created', NEW.reviewer_actor_id, NEW.version_id
            USING ERRCODE = 'MK002';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_review_dual_control() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_reviews_dual_control BEFORE INSERT ON marketplace_reviews
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_review_dual_control();

-- ---------------------------------------------------------------------------
-- 4. The version lifecycle and moderation, bound in SQL
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('DRAFT','SUBMITTED'),
        ('SUBMITTED','VALIDATED'), ('SUBMITTED','REJECTED'),
        ('VALIDATED','IN_REVIEW'), ('VALIDATED','REJECTED'),
        ('IN_REVIEW','APPROVED'), ('IN_REVIEW','REJECTED'),
        ('APPROVED','DEPRECATED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_moderation_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('UNREVIEWED','CLEARED'), ('UNREVIEWED','FLAGGED'), ('UNREVIEWED','TAKEN_DOWN'),
        ('CLEARED','FLAGGED'), ('CLEARED','TAKEN_DOWN'),
        ('FLAGGED','CLEARED'), ('FLAGGED','TAKEN_DOWN'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_version_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- Every version starts at the beginning: no row is ever born SUBMITTED, VALIDATED, IN_REVIEW,
        -- APPROVED or CLEARED, so no step of the lifecycle can be skipped by writing the row directly.
        IF NEW.status <> 'DRAFT' OR NEW.moderation_state <> 'UNREVIEWED' OR NEW.rejection_code IS NOT NULL THEN
            RAISE EXCEPTION 'VERSION_TRANSITION_ILLEGAL: listing version % must be created DRAFT and UNREVIEWED', NEW.id
                USING ERRCODE = 'MK001';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.listing_id, NEW.version, NEW.description, NEW.risk_class, NEW.supported_instrument_ids, NEW.manifest_canonical,
        NEW.manifest_hash, NEW.source_digest, NEW.source_commit, NEW.backtest_evidence_ref, NEW.data_dependencies,
        NEW.performance_fee_bps, NEW.management_fee_bps, NEW.fee_high_water_mark, NEW.config, NEW.created_by_actor_type,
        NEW.created_by_actor_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.listing_id, OLD.version, OLD.description, OLD.risk_class, OLD.supported_instrument_ids, OLD.manifest_canonical,
        OLD.manifest_hash, OLD.source_digest, OLD.source_commit, OLD.backtest_evidence_ref, OLD.data_dependencies,
        OLD.performance_fee_bps, OLD.management_fee_bps, OLD.fee_high_water_mark, OLD.config, OLD.created_by_actor_type,
        OLD.created_by_actor_id, OLD.created_at) THEN
        RAISE EXCEPTION 'VERSION_IDENTITY_FROZEN: listing version % may change only its lifecycle; publish a new version', OLD.id
            USING ERRCODE = 'MK004';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status THEN
        IF NOT cp_marketplace_transition_legal(OLD.status, NEW.status) THEN
            RAISE EXCEPTION 'VERSION_TRANSITION_ILLEGAL: listing version % cannot move % -> %', OLD.id, OLD.status, NEW.status
                USING ERRCODE = 'MK001';
        END IF;
        IF NEW.status = 'VALIDATED' AND NOT EXISTS (
            SELECT 1 FROM marketplace_version_validations WHERE version_id = NEW.id AND outcome = 'PASSED') THEN
            RAISE EXCEPTION 'VALIDATION_REQUIRED: listing version % has no passed validation', NEW.id USING ERRCODE = 'MK003';
        END IF;
        IF NEW.status = 'APPROVED' AND NOT EXISTS (
            SELECT 1 FROM marketplace_reviews r
             WHERE r.version_id = NEW.id AND r.decision = 'APPROVE' AND r.reviewer_actor_type = 'OPERATOR') THEN
            RAISE EXCEPTION 'REVIEW_DUAL_CONTROL: listing version % has no operator approval by someone other than its creator',
                NEW.id USING ERRCODE = 'MK002';
        END IF;
    END IF;
    IF OLD.moderation_state IS DISTINCT FROM NEW.moderation_state THEN
        IF NOT cp_marketplace_moderation_legal(OLD.moderation_state, NEW.moderation_state) THEN
            RAISE EXCEPTION 'MODERATION_TRANSITION_ILLEGAL: listing version % cannot move % -> %', OLD.id,
                OLD.moderation_state, NEW.moderation_state USING ERRCODE = 'MK007';
        END IF;
        IF NEW.moderation_state = 'CLEARED' AND NEW.status NOT IN ('APPROVED','DEPRECATED') THEN
            RAISE EXCEPTION 'MODERATION_TRANSITION_ILLEGAL: listing version % is % and cannot be cleared', OLD.id, NEW.status
                USING ERRCODE = 'MK007';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_version_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_listing_versions_guard BEFORE INSERT OR UPDATE ON marketplace_listing_versions
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_version_guard();

CREATE TABLE marketplace_version_transitions (
    id           uuid PRIMARY KEY,
    version_id   uuid NOT NULL REFERENCES marketplace_listing_versions(id),
    from_status  text NOT NULL,
    to_status    text NOT NULL,
    actor_type   text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id     text NOT NULL,
    reason       text NOT NULL,
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX marketplace_version_transitions_idx ON marketplace_version_transitions (version_id, occurred_at);
CREATE TRIGGER marketplace_version_transitions_immutable BEFORE UPDATE OR DELETE ON marketplace_version_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER marketplace_version_transitions_flag_edge AFTER INSERT ON marketplace_version_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('version_id', 'from_status', 'to_status', 'mkt_version_status');
CREATE CONSTRAINT TRIGGER marketplace_listing_versions_require_transition AFTER UPDATE OF status ON marketplace_listing_versions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'mkt_version_status');

CREATE TABLE marketplace_moderation_transitions (
    id           uuid PRIMARY KEY,
    version_id   uuid NOT NULL REFERENCES marketplace_listing_versions(id),
    from_state   text NOT NULL,
    to_state     text NOT NULL,
    actor_type   text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id     text NOT NULL,
    reason       text NOT NULL,
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX marketplace_moderation_transitions_idx ON marketplace_moderation_transitions (version_id, occurred_at);
CREATE TRIGGER marketplace_moderation_transitions_immutable BEFORE UPDATE OR DELETE ON marketplace_moderation_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER marketplace_moderation_transitions_flag_edge AFTER INSERT ON marketplace_moderation_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('version_id', 'from_state', 'to_state', 'mkt_version_moderation');
CREATE CONSTRAINT TRIGGER marketplace_listing_versions_moderation_requires_transition
    AFTER UPDATE OF moderation_state ON marketplace_listing_versions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('moderation_state', 'mkt_version_moderation');

-- ---------------------------------------------------------------------------
-- 5. Installs: PAPER only
-- ---------------------------------------------------------------------------

CREATE TABLE marketplace_installs (
    id                      uuid PRIMARY KEY,
    account_id              uuid NOT NULL REFERENCES accounts(id),
    version_id              uuid NOT NULL REFERENCES marketplace_listing_versions(id),
    -- PAPER and nothing else: an install can never allocate real customer money.
    mode                    text NOT NULL CONSTRAINT marketplace_installs_mode_check CHECK (mode IN ('PAPER')),
    status                  text NOT NULL CHECK (status IN ('ACTIVE','REMOVED')),
    installed_by_actor_type text NOT NULL CHECK (installed_by_actor_type IN ('USER','OPERATOR','SERVICE')),
    installed_by_actor_id   text NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX marketplace_installs_one_active ON marketplace_installs (account_id, version_id) WHERE status = 'ACTIVE';
CREATE TRIGGER marketplace_installs_updated_at BEFORE UPDATE ON marketplace_installs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_install_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_status     text;
    v_moderation text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT status, moderation_state INTO v_status, v_moderation FROM marketplace_listing_versions WHERE id = NEW.version_id;
        IF v_status IS DISTINCT FROM 'APPROVED' OR v_moderation IS DISTINCT FROM 'CLEARED' OR NEW.status <> 'ACTIVE' THEN
            RAISE EXCEPTION 'INSTALL_NOT_ALLOWED: version % is %/% and cannot be installed', NEW.version_id, v_status, v_moderation
                USING ERRCODE = 'MK006';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.account_id, NEW.version_id, NEW.mode, NEW.installed_by_actor_type, NEW.installed_by_actor_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.account_id, OLD.version_id, OLD.mode, OLD.installed_by_actor_type, OLD.installed_by_actor_id, OLD.created_at) THEN
        RAISE EXCEPTION 'INSTALL_IDENTITY_FROZEN: install % may change status only', OLD.id USING ERRCODE = 'MK006';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND (OLD.status, NEW.status) NOT IN (('ACTIVE','REMOVED')) THEN
        RAISE EXCEPTION 'INSTALL_TRANSITION_ILLEGAL: install % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'MK008';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_install_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_installs_guard BEFORE INSERT OR UPDATE ON marketplace_installs
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_install_guard();

CREATE TABLE marketplace_install_transitions (
    id           uuid PRIMARY KEY,
    install_id   uuid NOT NULL REFERENCES marketplace_installs(id),
    from_status  text NOT NULL,
    to_status    text NOT NULL,
    actor_type   text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id     text NOT NULL,
    reason       text NOT NULL,
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX marketplace_install_transitions_idx ON marketplace_install_transitions (install_id, occurred_at);
CREATE TRIGGER marketplace_install_transitions_immutable BEFORE UPDATE OR DELETE ON marketplace_install_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER marketplace_install_transitions_flag_edge AFTER INSERT ON marketplace_install_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('install_id', 'from_status', 'to_status', 'mkt_install_status');
CREATE CONSTRAINT TRIGGER marketplace_installs_require_transition AFTER UPDATE OF status ON marketplace_installs
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'mkt_install_status');

-- ---------------------------------------------------------------------------
-- 6. Paper follow: runs, the intents they proposed, and what became of each
-- ---------------------------------------------------------------------------

CREATE TABLE marketplace_follow_runs (
    id                      uuid PRIMARY KEY,
    install_id              uuid NOT NULL REFERENCES marketplace_installs(id),
    version_id              uuid NOT NULL REFERENCES marketplace_listing_versions(id),
    account_id              uuid NOT NULL REFERENCES accounts(id),
    sandbox_run_id          uuid NOT NULL,
    trigger                 text NOT NULL CHECK (trigger IN ('EXPLICIT','SCHEDULED')),
    as_of                   timestamptz NOT NULL,
    input_digest            bytea NOT NULL CHECK (length(input_digest) = 32),
    run_digest              bytea NOT NULL CHECK (length(run_digest) = 32),
    proposals_count         integer NOT NULL CHECK (proposals_count >= 0),
    accepted_count          integer NOT NULL CHECK (accepted_count >= 0),
    violations_count        integer NOT NULL CHECK (violations_count >= 0),
    intents_submitted       integer NOT NULL CHECK (intents_submitted >= 0),
    intents_dispatched      integer NOT NULL CHECK (intents_dispatched >= 0),
    intents_risk_rejected   integer NOT NULL CHECK (intents_risk_rejected >= 0),
    intents_failed          integer NOT NULL CHECK (intents_failed >= 0),
    -- Exact decimals in simulated dollars: the portfolio value of the snapshot the run saw, and the
    -- notional of what was dispatched. Paper performance is computed from these.
    equity                  numeric NOT NULL CHECK (equity >= 0),
    turnover                numeric NOT NULL CHECK (turnover >= 0),
    evidence_ref            text NOT NULL,
    evidence_sha256         bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    actor_type              text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id                text NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (install_id, as_of),
    CONSTRAINT marketplace_follow_runs_every_intent_has_one_outcome
        CHECK (intents_dispatched + intents_risk_rejected + intents_failed = intents_submitted),
    CONSTRAINT marketplace_follow_runs_one_intent_per_accepted_proposal CHECK (intents_submitted = accepted_count),
    CONSTRAINT marketplace_follow_runs_accepted_within_proposals CHECK (accepted_count <= proposals_count)
);
CREATE INDEX marketplace_follow_runs_install_idx ON marketplace_follow_runs (install_id, as_of);
CREATE TRIGGER marketplace_follow_runs_immutable BEFORE UPDATE OR DELETE ON marketplace_follow_runs
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE marketplace_follow_intents (
    id                  uuid PRIMARY KEY,
    follow_run_id       uuid NOT NULL REFERENCES marketplace_follow_runs(id),
    proposal_seq        integer NOT NULL CHECK (proposal_seq >= 1),
    intent_id           uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    outcome             text NOT NULL CHECK (outcome IN ('DISPATCHED','RISK_REJECTED','ROUTE_REFUSED','DISPATCH_FAILED',
                                                         'DISPATCH_UNKNOWN')),
    risk_decision_hash  text,
    effect_id           uuid REFERENCES economic_effects(id),
    notional            numeric NOT NULL CHECK (notional > 0),
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (follow_run_id, proposal_seq),
    CONSTRAINT marketplace_follow_intents_risk_decided
        CHECK (outcome = 'ROUTE_REFUSED' OR risk_decision_hash IS NOT NULL),
    CONSTRAINT marketplace_follow_intents_dispatch_has_effect
        CHECK (outcome NOT IN ('DISPATCHED','DISPATCH_FAILED','DISPATCH_UNKNOWN') OR effect_id IS NOT NULL)
);
CREATE TRIGGER marketplace_follow_intents_immutable BEFORE UPDATE OR DELETE ON marketplace_follow_intents
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Nothing a strategy proposed is recorded as dispatched unless the risk kernel allowed that very
-- intent and a MARKETPLACE effect for it exists.
-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_dispatch_was_allowed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.outcome IN ('DISPATCHED','DISPATCH_FAILED','DISPATCH_UNKNOWN') THEN
        IF NOT EXISTS (SELECT 1 FROM advanced_risk_decisions d
                        WHERE d.intent_id = NEW.intent_id AND d.verdict = 'ALLOW' AND d.decision_hash = NEW.risk_decision_hash) THEN
            RAISE EXCEPTION 'DISPATCH_WITHOUT_ALLOW: intent % was dispatched without an ALLOW risk decision', NEW.intent_id
                USING ERRCODE = 'MK009';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM economic_effects e
                        WHERE e.id = NEW.effect_id AND e.intent_id = NEW.intent_id AND e.subsystem = 'MARKETPLACE') THEN
            RAISE EXCEPTION 'DISPATCH_WITHOUT_ALLOW: intent % names effect % which is not its MARKETPLACE dispatch',
                NEW.intent_id, NEW.effect_id USING ERRCODE = 'MK009';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_dispatch_was_allowed() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_follow_intents_dispatch_allowed BEFORE INSERT ON marketplace_follow_intents
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_dispatch_was_allowed();

-- ---------------------------------------------------------------------------
-- 7. Evaluations (SIMULATE) and paper performance
-- ---------------------------------------------------------------------------

CREATE TABLE marketplace_evaluations (
    id                  uuid PRIMARY KEY,
    version_id          uuid NOT NULL REFERENCES marketplace_listing_versions(id),
    kind                text NOT NULL CHECK (kind IN ('SIMULATION')),
    dataset_version     text NOT NULL CHECK (length(dataset_version) BETWEEN 1 AND 100),
    snapshots_count     integer NOT NULL CHECK (snapshots_count BETWEEN 1 AND 1000),
    snapshots_digest    bytea NOT NULL CHECK (length(snapshots_digest) = 32),
    results_digest      bytea NOT NULL CHECK (length(results_digest) = 32),
    runs_ok             integer NOT NULL CHECK (runs_ok >= 0 AND runs_ok <= snapshots_count),
    proposals_count     integer NOT NULL CHECK (proposals_count >= 0),
    accepted_count      integer NOT NULL CHECK (accepted_count >= 0 AND accepted_count <= proposals_count),
    violations_count    integer NOT NULL CHECK (violations_count >= 0),
    evidence_ref        text NOT NULL,
    evidence_sha256     bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    actor_type          text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id            text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX marketplace_evaluations_version_idx ON marketplace_evaluations (version_id, created_at);
CREATE TRIGGER marketplace_evaluations_immutable BEFORE UPDATE OR DELETE ON marketplace_evaluations
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE marketplace_evaluation_runs (
    id              uuid PRIMARY KEY,
    evaluation_id   uuid NOT NULL REFERENCES marketplace_evaluations(id),
    seq             integer NOT NULL CHECK (seq >= 1),
    sandbox_run_id  uuid NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (evaluation_id, seq)
);
CREATE TRIGGER marketplace_evaluation_runs_immutable BEFORE UPDATE OR DELETE ON marketplace_evaluation_runs
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE marketplace_performance_snapshots (
    id                  uuid PRIMARY KEY,
    install_id          uuid NOT NULL REFERENCES marketplace_installs(id),
    version_id          uuid NOT NULL REFERENCES marketplace_listing_versions(id),
    follow_runs_count   integer NOT NULL CHECK (follow_runs_count >= 1),
    first_as_of         timestamptz NOT NULL,
    last_as_of          timestamptz NOT NULL,
    history_digest      bytea NOT NULL CHECK (length(history_digest) = 32),
    return_bps          bigint NOT NULL,
    max_drawdown_bps    bigint NOT NULL CHECK (max_drawdown_bps >= 0),
    volatility_bps      bigint NOT NULL CHECK (volatility_bps >= 0),
    turnover_bps        bigint NOT NULL CHECK (turnover_bps >= 0),
    computed_at         timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (first_as_of <= last_as_of)
);
CREATE INDEX marketplace_performance_snapshots_install_idx ON marketplace_performance_snapshots (install_id, computed_at DESC);
CREATE TRIGGER marketplace_performance_snapshots_immutable BEFORE UPDATE OR DELETE ON marketplace_performance_snapshots
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 8. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON marketplace_listings, marketplace_listing_versions, marketplace_version_sources,
    marketplace_version_validations, marketplace_reviews, marketplace_version_transitions,
    marketplace_moderation_transitions, marketplace_installs, marketplace_install_transitions, marketplace_follow_runs,
    marketplace_follow_intents, marketplace_evaluations, marketplace_evaluation_runs, marketplace_performance_snapshots
    TO cp_app;
GRANT UPDATE (status, moderation_state, rejection_code) ON marketplace_listing_versions TO cp_app;
GRANT UPDATE (status) ON marketplace_installs TO cp_app;
GRANT SELECT ON marketplace_listings, marketplace_listing_versions, marketplace_version_sources,
    marketplace_version_validations, marketplace_reviews, marketplace_version_transitions,
    marketplace_moderation_transitions, marketplace_installs, marketplace_install_transitions, marketplace_follow_runs,
    marketplace_follow_intents, marketplace_evaluations, marketplace_evaluation_runs, marketplace_performance_snapshots
    TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: listings, reviews, installs and follow history are evidence and are never dropped
