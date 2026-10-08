-- +goose Up
-- Strategy marketplace and sandbox hardening: review 2, findings F10-F14 (internal/marketplace,
-- internal/sandboxrt; amends 00750 and 00751 without editing them).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. (F10) A person acts only for an account they own. A listing's creator, an install's installer, the
--      actor of an EXPLICIT follow and the actor of an evaluation are the owner of the account the row names;
--      a SCHEDULED follow is the SYSTEM scheduler's; a follow run names its install's own account (MK012).
--   2. (F10) Paper performance comes from the books, never from a caller. Every install has a paper capital.
--      A follow run records the reference marks it valued the install's paper book at, each the very price
--      observation asset_prices holds for the instrument's pair, observed within the run's freshness bound
--      before its as-of time. Its paper cash is the install's paper capital moved by the fills of earlier runs,
--      its equity is that cash plus every earlier position at its recorded mark, and its turnover is the
--      notional it dispatched, exactly (checked at COMMIT). A paper fill exists only for an intent the run
--      DISPATCHED with an APPLIED effect, at that intent's account, instrument, side and quantity and at the
--      run's mark. A performance snapshot covers exactly the install's book-valued runs (MK013).
--   3. (F11) A follow intent's notional -- the figure the manifest's cap and Risk V2 judged -- is the intent's
--      quantity at the run's recorded reference mark, and the intent's terms name that mark: never a price
--      the strategy chose (MK014). REFERENCE_PRICE_UNAVAILABLE joins the sandbox violation codes.
--   4. (F12) A validation is recorded under a build claim: at most one live (unexpired, unreleased,
--      unrecorded) claim per version, only while the version is SUBMITTED and unvalidated; the outcome names
--      the version's latest claim, unreleased. The bounded compiler output is stored with it and hashes to
--      its build-log digest, and a build past its output or memory bound is a recorded rejection
--      (BUILD_OUTPUT_LIMIT, BUILD_MEMORY_LIMIT) (MK015).
--   5. (F13) Every sandbox run is charged to an account (SX002); a charge is the run's own wall time and the
--      wall-time limit it was granted (none for a run the kill switch stopped), and no charge takes the
--      account beyond the budget it states for its window (SX003).
--   6. (F14) The creator of a listing or author of a version never takes it into review, approves it, or
--      moderates it -- re-clearing a flagged version included (MK011).
--   7. marketplace_follow_runs gets the (account_id, created_at DESC, id DESC) index its account reads use.
--
-- Rows written before this migration (installs, follow runs, performance snapshots, validations) are kept as
-- they are; a follow run without a paper cash was valued from a caller's snapshot and no performance is ever
-- computed from it again.
--
-- Custom SQLSTATEs: MK011 self-clearance, MK012 actor is not the account's owner (or a follow names another
-- account), MK013 paper book does not follow from its fills and recorded marks, MK014 notional not at the
-- reference mark, MK015 validation outside a valid build claim, SX002 sandbox run not charged, SX003 sandbox
-- charge mismatched or beyond its budget.

-- 10^n, exactly: every comparison below is a multiplication, never a division.
-- +goose StatementBegin
CREATE FUNCTION cp_pow10(n integer) RETURNS numeric
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT ('1' || repeat('0', n))::numeric;
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 1. F14: no self-clearance
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_is_creator(p_version uuid, p_actor text) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM marketplace_listing_versions v JOIN marketplace_listings l ON l.id = v.listing_id
                    WHERE v.id = p_version AND (l.creator_actor_id = p_actor OR v.created_by_actor_id = p_actor));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_no_self_review() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.to_status IN ('IN_REVIEW','APPROVED') AND cp_marketplace_is_creator(NEW.version_id, NEW.actor_id) THEN
        RAISE EXCEPTION 'SELF_CLEARANCE: % may not take version % they created to %', NEW.actor_id, NEW.version_id, NEW.to_status
            USING ERRCODE = 'MK011';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_no_self_review() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_version_transitions_no_self_review BEFORE INSERT ON marketplace_version_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_no_self_review();

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_no_self_moderation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF cp_marketplace_is_creator(NEW.version_id, NEW.actor_id) THEN
        RAISE EXCEPTION 'SELF_CLEARANCE: % may not moderate version % they created (% -> %)', NEW.actor_id, NEW.version_id,
            NEW.from_state, NEW.to_state USING ERRCODE = 'MK011';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_no_self_moderation() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_moderation_transitions_no_self_moderation BEFORE INSERT ON marketplace_moderation_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_no_self_moderation();

-- ---------------------------------------------------------------------------
-- 2. F10: a person acts only for an account they own
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_account_owner(p_account uuid) RETURNS text
LANGUAGE sql STABLE AS $$
    SELECT owner_user_id::text FROM accounts WHERE id = p_account;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_listing_creator_owns() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.creator_actor_id IS DISTINCT FROM cp_marketplace_account_owner(NEW.creator_account_id) THEN
        RAISE EXCEPTION 'ACTOR_NOT_OWNER: % does not own account % and cannot list for it', NEW.creator_actor_id,
            NEW.creator_account_id USING ERRCODE = 'MK012';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_listing_creator_owns() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_listings_creator_owns BEFORE INSERT ON marketplace_listings
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_listing_creator_owns();

ALTER TABLE marketplace_installs ADD COLUMN paper_capital numeric NOT NULL DEFAULT 10000.00
    CONSTRAINT marketplace_installs_paper_capital_positive CHECK (paper_capital > 0);

-- The install guard of 00750, now also binding the installer to the account and freezing the paper capital.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_marketplace_install_guard() RETURNS trigger
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
        IF NEW.installed_by_actor_id IS DISTINCT FROM cp_marketplace_account_owner(NEW.account_id) THEN
            RAISE EXCEPTION 'ACTOR_NOT_OWNER: % does not own account % and cannot install for it', NEW.installed_by_actor_id,
                NEW.account_id USING ERRCODE = 'MK012';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.account_id, NEW.version_id, NEW.mode, NEW.installed_by_actor_type, NEW.installed_by_actor_id, NEW.created_at,
        NEW.paper_capital)
       IS DISTINCT FROM
       (OLD.account_id, OLD.version_id, OLD.mode, OLD.installed_by_actor_type, OLD.installed_by_actor_id, OLD.created_at,
        OLD.paper_capital) THEN
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

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_follow_actor_owns() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.account_id IS DISTINCT FROM (SELECT account_id FROM marketplace_installs WHERE id = NEW.install_id) THEN
        RAISE EXCEPTION 'ACTOR_NOT_OWNER: follow run % names account % which is not its install''s', NEW.id, NEW.account_id
            USING ERRCODE = 'MK012';
    END IF;
    IF NEW.trigger = 'SCHEDULED' THEN
        IF NEW.actor_type <> 'SYSTEM' THEN
            RAISE EXCEPTION 'ACTOR_NOT_OWNER: a scheduled follow is the SYSTEM scheduler''s, not %', NEW.actor_type
                USING ERRCODE = 'MK012';
        END IF;
    ELSIF NEW.actor_id IS DISTINCT FROM cp_marketplace_account_owner(NEW.account_id) THEN
        RAISE EXCEPTION 'ACTOR_NOT_OWNER: % does not own account % and cannot follow for it', NEW.actor_id, NEW.account_id
            USING ERRCODE = 'MK012';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_follow_actor_owns() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_follow_runs_actor_owns BEFORE INSERT ON marketplace_follow_runs
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_follow_actor_owns();

ALTER TABLE marketplace_evaluations ADD COLUMN account_id uuid REFERENCES accounts(id);

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_evaluation_actor_owns() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.account_id IS NULL
       OR NEW.account_id IS DISTINCT FROM (SELECT l.creator_account_id FROM marketplace_listing_versions v
                                            JOIN marketplace_listings l ON l.id = v.listing_id WHERE v.id = NEW.version_id)
       OR NEW.actor_id IS DISTINCT FROM cp_marketplace_account_owner(NEW.account_id) THEN
        RAISE EXCEPTION 'ACTOR_NOT_OWNER: evaluation % is charged to the listing creator''s account and run by its owner', NEW.id
            USING ERRCODE = 'MK012';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_evaluation_actor_owns() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_evaluations_actor_owns BEFORE INSERT ON marketplace_evaluations
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_evaluation_actor_owns();

-- ---------------------------------------------------------------------------
-- 3. F10/F11: the paper book, its recorded marks and its fills
-- ---------------------------------------------------------------------------

ALTER TABLE marketplace_follow_runs
    ADD COLUMN paper_cash numeric CONSTRAINT marketplace_follow_runs_paper_cash_non_negative CHECK (paper_cash IS NULL OR paper_cash >= 0),
    ADD COLUMN mark_max_age_ms bigint CONSTRAINT marketplace_follow_runs_mark_max_age_positive
        CHECK (mark_max_age_ms IS NULL OR mark_max_age_ms BETWEEN 1 AND 86400000);

CREATE TABLE marketplace_follow_marks (
    id              uuid PRIMARY KEY,
    follow_run_id   uuid NOT NULL REFERENCES marketplace_follow_runs(id),
    instrument_id   uuid NOT NULL REFERENCES instruments(id),
    price_id        uuid NOT NULL REFERENCES asset_prices(id),
    -- Simulated dollars per whole base unit: the observation's mantissa x 10^-scale, exactly.
    mark            numeric NOT NULL CHECK (mark > 0),
    base_decimals   smallint NOT NULL CHECK (base_decimals BETWEEN 0 AND 18),
    observed_at     timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (follow_run_id, instrument_id)
);
CREATE TRIGGER marketplace_follow_marks_immutable BEFORE UPDATE OR DELETE ON marketplace_follow_marks
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_follow_mark_is_observed() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    run_as_of   timestamptz;
    run_max_age bigint;
    p_asset     uuid;
    p_quote     uuid;
    p_mantissa  numeric;
    p_scale     integer;
    p_observed  timestamptz;
    i_base      uuid;
    i_quote     uuid;
    i_decimals  smallint;
BEGIN
    SELECT as_of, mark_max_age_ms INTO run_as_of, run_max_age FROM marketplace_follow_runs WHERE id = NEW.follow_run_id;
    SELECT asset_id, quote_asset_id, mantissa, scale, observed_at INTO p_asset, p_quote, p_mantissa, p_scale, p_observed
      FROM asset_prices WHERE id = NEW.price_id;
    SELECT i.base_asset_id, i.quote_asset_id, a.decimals INTO i_base, i_quote, i_decimals
      FROM instruments i JOIN assets a ON a.id = i.base_asset_id WHERE i.id = NEW.instrument_id;
    IF run_max_age IS NULL OR i_base IS NULL OR i_quote IS NULL
       OR p_asset IS DISTINCT FROM i_base OR p_quote IS DISTINCT FROM i_quote
       OR NEW.mark * cp_pow10(p_scale) IS DISTINCT FROM p_mantissa
       OR NEW.observed_at IS DISTINCT FROM p_observed
       OR NEW.base_decimals IS DISTINCT FROM i_decimals
       OR p_observed > run_as_of
       OR p_observed < run_as_of - (run_max_age::text || ' milliseconds')::interval THEN
        RAISE EXCEPTION 'PAPER_BOOK_MISMATCH: mark of % for follow run % is not a fresh recorded observation of its pair',
            NEW.instrument_id, NEW.follow_run_id USING ERRCODE = 'MK013';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_follow_mark_is_observed() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_follow_marks_observed BEFORE INSERT ON marketplace_follow_marks
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_follow_mark_is_observed();

-- What a follow intent's notional was computed at: the run's recorded mark for the intent's instrument.
-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_follow_intent_at_reference() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    i_qty      numeric;
    i_ins      uuid;
    i_ref      text;
    m_mark     numeric;
    m_decimals smallint;
BEGIN
    SELECT quantity, instrument_id, terms->>'reference_price' INTO i_qty, i_ins, i_ref FROM economic_intents WHERE id = NEW.intent_id;
    SELECT mark, base_decimals INTO m_mark, m_decimals FROM marketplace_follow_marks
     WHERE follow_run_id = NEW.follow_run_id AND instrument_id = i_ins;
    IF i_qty IS NULL OR m_mark IS NULL OR i_ref IS NULL OR i_ref !~ '^[0-9]+(\.[0-9]+)?$'
       OR i_ref::numeric IS DISTINCT FROM m_mark
       OR NEW.notional * cp_pow10(m_decimals) IS DISTINCT FROM i_qty * m_mark THEN
        RAISE EXCEPTION 'NOTIONAL_NOT_AT_REFERENCE: intent % records notional % which is not its quantity at the run''s reference mark',
            NEW.intent_id, NEW.notional USING ERRCODE = 'MK014';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_follow_intent_at_reference() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_follow_intents_notional_at_reference BEFORE INSERT ON marketplace_follow_intents
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_follow_intent_at_reference();

CREATE TABLE marketplace_paper_fills (
    id              uuid PRIMARY KEY,
    install_id      uuid NOT NULL REFERENCES marketplace_installs(id),
    follow_run_id   uuid NOT NULL REFERENCES marketplace_follow_runs(id),
    intent_id       uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    instrument_id   uuid NOT NULL REFERENCES instruments(id),
    side            text NOT NULL CHECK (side IN ('BUY','SELL')),
    quantity        numeric(38,0) NOT NULL CHECK (quantity > 0),
    mark            numeric NOT NULL CHECK (mark > 0),
    notional        numeric NOT NULL CHECK (notional > 0),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX marketplace_paper_fills_install_idx ON marketplace_paper_fills (install_id, follow_run_id);
CREATE TRIGGER marketplace_paper_fills_immutable BEFORE UPDATE OR DELETE ON marketplace_paper_fills
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_paper_fill_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    r_install   uuid;
    i_account   uuid;
    fi_outcome  text;
    fi_effect   uuid;
    fi_notional numeric;
    e_status    text;
    ei_account  uuid;
    ei_ins      uuid;
    ei_qty      numeric;
    ei_side     text;
    m_mark      numeric;
    m_decimals  smallint;
BEGIN
    SELECT install_id INTO r_install FROM marketplace_follow_runs WHERE id = NEW.follow_run_id;
    SELECT account_id INTO i_account FROM marketplace_installs WHERE id = NEW.install_id;
    SELECT outcome, effect_id, notional INTO fi_outcome, fi_effect, fi_notional FROM marketplace_follow_intents
     WHERE follow_run_id = NEW.follow_run_id AND intent_id = NEW.intent_id;
    SELECT status INTO e_status FROM economic_effects WHERE id = fi_effect AND intent_id = NEW.intent_id AND subsystem = 'MARKETPLACE';
    SELECT account_id, instrument_id, quantity, terms->>'side' INTO ei_account, ei_ins, ei_qty, ei_side
      FROM economic_intents WHERE id = NEW.intent_id;
    SELECT mark, base_decimals INTO m_mark, m_decimals FROM marketplace_follow_marks
     WHERE follow_run_id = NEW.follow_run_id AND instrument_id = NEW.instrument_id;
    IF r_install IS DISTINCT FROM NEW.install_id
       OR fi_outcome IS DISTINCT FROM 'DISPATCHED' OR e_status IS DISTINCT FROM 'APPLIED'
       OR ei_account IS DISTINCT FROM i_account OR ei_ins IS DISTINCT FROM NEW.instrument_id
       OR ei_qty IS DISTINCT FROM NEW.quantity OR ei_side IS DISTINCT FROM NEW.side
       OR m_mark IS DISTINCT FROM NEW.mark OR fi_notional IS DISTINCT FROM NEW.notional
       OR NEW.notional * cp_pow10(m_decimals) IS DISTINCT FROM NEW.quantity * NEW.mark THEN
        RAISE EXCEPTION 'PAPER_BOOK_MISMATCH: fill of intent % is not the dispatched, applied intent at its run''s mark', NEW.intent_id
            USING ERRCODE = 'MK013';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_paper_fill_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_paper_fills_match BEFORE INSERT ON marketplace_paper_fills
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_paper_fill_matches();

-- A run's paper cash, equity and turnover follow from the fills of earlier runs and its own recorded marks.
-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_follow_book_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    capital    numeric;
    cash       numeric;
    held       record;
    m_mark     numeric;
    m_decimals smallint;
    valued     numeric := 0;
    dispatched numeric;
BEGIN
    IF NEW.paper_cash IS NULL OR NEW.mark_max_age_ms IS NULL THEN
        RAISE EXCEPTION 'PAPER_BOOK_MISMATCH: follow run % is not valued from its install''s paper book', NEW.id
            USING ERRCODE = 'MK013';
    END IF;
    SELECT paper_capital INTO capital FROM marketplace_installs WHERE id = NEW.install_id;
    SELECT capital - coalesce(sum(CASE f.side WHEN 'BUY' THEN f.notional ELSE -f.notional END), 0) INTO cash
      FROM marketplace_paper_fills f JOIN marketplace_follow_runs r ON r.id = f.follow_run_id
     WHERE f.install_id = NEW.install_id AND r.as_of < NEW.as_of;
    IF cash IS DISTINCT FROM NEW.paper_cash THEN
        RAISE EXCEPTION 'PAPER_BOOK_MISMATCH: follow run % records paper cash % where its fills leave %', NEW.id, NEW.paper_cash, cash
            USING ERRCODE = 'MK013';
    END IF;
    -- (equity - cash) x 10^18 = sum of position x mark x 10^(18 - base decimals): exact, no division.
    FOR held IN
        SELECT f.instrument_id, sum(CASE f.side WHEN 'BUY' THEN f.quantity ELSE -f.quantity END) AS quantity
          FROM marketplace_paper_fills f JOIN marketplace_follow_runs r ON r.id = f.follow_run_id
         WHERE f.install_id = NEW.install_id AND r.as_of < NEW.as_of
         GROUP BY f.instrument_id
    LOOP
        CONTINUE WHEN held.quantity = 0;
        m_mark := NULL;
        SELECT mark, base_decimals INTO m_mark, m_decimals FROM marketplace_follow_marks
         WHERE follow_run_id = NEW.id AND instrument_id = held.instrument_id;
        IF m_mark IS NULL THEN
            RAISE EXCEPTION 'PAPER_BOOK_MISMATCH: follow run % holds % with no recorded mark', NEW.id, held.instrument_id
                USING ERRCODE = 'MK013';
        END IF;
        valued := valued + held.quantity * m_mark * cp_pow10(18 - m_decimals);
    END LOOP;
    IF (NEW.equity - NEW.paper_cash) * cp_pow10(18) IS DISTINCT FROM valued THEN
        RAISE EXCEPTION 'PAPER_BOOK_MISMATCH: follow run % records equity % which is not its paper book at its marks', NEW.id, NEW.equity
            USING ERRCODE = 'MK013';
    END IF;
    SELECT coalesce(sum(notional), 0) INTO dispatched FROM marketplace_follow_intents
     WHERE follow_run_id = NEW.id AND outcome = 'DISPATCHED';
    IF dispatched IS DISTINCT FROM NEW.turnover THEN
        RAISE EXCEPTION 'PAPER_BOOK_MISMATCH: follow run % records turnover % but dispatched %', NEW.id, NEW.turnover, dispatched
            USING ERRCODE = 'MK013';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_follow_book_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER marketplace_follow_runs_book_matches AFTER INSERT ON marketplace_follow_runs
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_marketplace_follow_book_matches();

ALTER TABLE marketplace_performance_snapshots ADD COLUMN from_paper_book boolean NOT NULL DEFAULT false;

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_performance_from_book() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    runs     integer;
    first_at timestamptz;
    last_at  timestamptz;
BEGIN
    SELECT count(*), min(as_of), max(as_of) INTO runs, first_at, last_at FROM marketplace_follow_runs
     WHERE install_id = NEW.install_id AND paper_cash IS NOT NULL;
    IF NOT NEW.from_paper_book OR runs IS DISTINCT FROM NEW.follow_runs_count
       OR first_at IS DISTINCT FROM NEW.first_as_of OR last_at IS DISTINCT FROM NEW.last_as_of
       OR NEW.version_id IS DISTINCT FROM (SELECT version_id FROM marketplace_installs WHERE id = NEW.install_id) THEN
        RAISE EXCEPTION 'PAPER_BOOK_MISMATCH: performance of install % does not cover exactly its book-valued follow runs',
            NEW.install_id USING ERRCODE = 'MK013';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_performance_from_book() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_performance_snapshots_from_book BEFORE INSERT ON marketplace_performance_snapshots
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_performance_from_book();

-- REFERENCE_PRICE_UNAVAILABLE: a proposal the host could not value at a price it read itself.
ALTER TABLE sandbox_run_violations DROP CONSTRAINT sandbox_run_violations_code_check;
ALTER TABLE sandbox_run_violations ADD CONSTRAINT sandbox_run_violations_code_check CHECK (code IN (
    'OUTPUT_NOT_JSON','OUTPUT_RECORD_TYPE_UNKNOWN','OUTPUT_RECORD_MALFORMED','OUTPUT_TRUNCATED','OUTPUT_TOO_MANY_RECORDS',
    'KIND_NOT_PROPOSABLE','INSTRUMENT_INVALID','INSTRUMENT_NOT_IN_SNAPSHOT','SIDE_INVALID','QUANTITY_INVALID',
    'LIMIT_PRICE_INVALID','LOG_INVALID','MANIFEST_KIND_NOT_ALLOWED','MANIFEST_INSTRUMENT_NOT_ALLOWED',
    'MANIFEST_NOTIONAL_EXCEEDED','MANIFEST_MAX_INTENTS_EXCEEDED','REFERENCE_PRICE_UNAVAILABLE'));

-- ---------------------------------------------------------------------------
-- 4. F12: validations under a build claim, with their bounded compiler output
-- ---------------------------------------------------------------------------

CREATE TABLE marketplace_validation_claims (
    id                     uuid PRIMARY KEY,
    version_id             uuid NOT NULL REFERENCES marketplace_listing_versions(id),
    claimed_by_actor_type  text NOT NULL CHECK (claimed_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    claimed_by_actor_id    text NOT NULL CHECK (length(claimed_by_actor_id) BETWEEN 1 AND 200),
    claimed_at             timestamptz NOT NULL,
    lease_until            timestamptz NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT marketplace_validation_claims_lease_after_claim CHECK (lease_until > claimed_at)
);
CREATE INDEX marketplace_validation_claims_version_idx ON marketplace_validation_claims (version_id, claimed_at DESC, id DESC);
CREATE TRIGGER marketplace_validation_claims_immutable BEFORE UPDATE OR DELETE ON marketplace_validation_claims
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- A claim that ended without an outcome (the build could not run): the version may be claimed again.
CREATE TABLE marketplace_validation_claim_releases (
    claim_id     uuid PRIMARY KEY REFERENCES marketplace_validation_claims(id),
    reason       text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    released_at  timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER marketplace_validation_claim_releases_immutable BEFORE UPDATE OR DELETE ON marketplace_validation_claim_releases
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

ALTER TABLE marketplace_version_validations
    ADD COLUMN claim_id uuid UNIQUE REFERENCES marketplace_validation_claims(id),
    ADD COLUMN build_log bytea NOT NULL DEFAULT ''::bytea
        CONSTRAINT marketplace_version_validations_build_log_bounded CHECK (length(build_log) <= 65536);
ALTER TABLE marketplace_version_validations ADD CONSTRAINT marketplace_version_validations_build_log_is_digested CHECK (
    claim_id IS NULL OR (build_log_digest IS NULL AND length(build_log) = 0) OR build_log_digest = sha256(build_log));
ALTER TABLE marketplace_version_validations DROP CONSTRAINT marketplace_version_validations_failure_reason_check;
ALTER TABLE marketplace_version_validations ADD CONSTRAINT marketplace_version_validations_failure_reason_check CHECK (
    failure_reason IS NULL OR failure_reason IN ('SCAN_REJECTED','COMPILE_FAILED','BUILD_TIMEOUT','BUILD_OUTPUT_LIMIT','BUILD_MEMORY_LIMIT'));

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_claim_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT status FROM marketplace_listing_versions WHERE id = NEW.version_id) IS DISTINCT FROM 'SUBMITTED'
       OR EXISTS (SELECT 1 FROM marketplace_version_validations WHERE version_id = NEW.version_id) THEN
        RAISE EXCEPTION 'BUILD_CLAIM_INVALID: version % is not awaiting validation', NEW.version_id USING ERRCODE = 'MK015';
    END IF;
    IF EXISTS (SELECT 1 FROM marketplace_validation_claims c
                WHERE c.version_id = NEW.version_id AND c.lease_until > NEW.claimed_at
                  AND NOT EXISTS (SELECT 1 FROM marketplace_validation_claim_releases r WHERE r.claim_id = c.id)) THEN
        RAISE EXCEPTION 'BUILD_CLAIM_INVALID: version % is being built under another claim', NEW.version_id USING ERRCODE = 'MK015';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_claim_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_validation_claims_guard BEFORE INSERT ON marketplace_validation_claims
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_claim_guard();

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_claim_release_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM marketplace_version_validations WHERE claim_id = NEW.claim_id) THEN
        RAISE EXCEPTION 'BUILD_CLAIM_INVALID: claim % already recorded its outcome', NEW.claim_id USING ERRCODE = 'MK015';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_claim_release_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_validation_claim_releases_guard BEFORE INSERT ON marketplace_validation_claim_releases
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_claim_release_guard();

-- +goose StatementBegin
CREATE FUNCTION cp_marketplace_validation_under_claim() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    c_version uuid;
    c_at      timestamptz;
BEGIN
    SELECT version_id, claimed_at INTO c_version, c_at FROM marketplace_validation_claims WHERE id = NEW.claim_id;
    IF NEW.claim_id IS NULL OR c_version IS DISTINCT FROM NEW.version_id
       OR EXISTS (SELECT 1 FROM marketplace_validation_claim_releases WHERE claim_id = NEW.claim_id)
       OR EXISTS (SELECT 1 FROM marketplace_validation_claims o
                   WHERE o.version_id = NEW.version_id AND (o.claimed_at, o.id) > (c_at, NEW.claim_id)) THEN
        RAISE EXCEPTION 'BUILD_CLAIM_INVALID: validation of version % is not recorded under its latest live build claim',
            NEW.version_id USING ERRCODE = 'MK015';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_marketplace_validation_under_claim() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER marketplace_version_validations_under_claim BEFORE INSERT ON marketplace_version_validations
    FOR EACH ROW EXECUTE FUNCTION cp_marketplace_validation_under_claim();

-- ---------------------------------------------------------------------------
-- 5. F13: every sandbox run is charged to an account, within its budget
-- ---------------------------------------------------------------------------

CREATE TABLE sandbox_usage (
    id              uuid PRIMARY KEY,
    run_id          uuid NOT NULL UNIQUE REFERENCES sandbox_runs(id),
    account_id      uuid NOT NULL REFERENCES accounts(id),
    purpose         text NOT NULL CHECK (purpose IN ('FOLLOW','SIMULATION')),
    -- The wall-time limit the run was granted (its CPU, a run being single-threaded and closed at its
    -- deadline), and the wall time it took.
    cpu_ms          bigint NOT NULL CHECK (cpu_ms >= 0),
    wall_ms         bigint NOT NULL CHECK (wall_ms >= 0),
    charged_at      timestamptz NOT NULL,
    -- The budget the charge was admitted under.
    budget_version  text NOT NULL CHECK (length(budget_version) BETWEEN 1 AND 64),
    window_ms       bigint NOT NULL CHECK (window_ms > 0),
    max_runs        integer NOT NULL CHECK (max_runs > 0),
    max_cpu_ms      bigint NOT NULL CHECK (max_cpu_ms > 0),
    max_wall_ms     bigint NOT NULL CHECK (max_wall_ms > 0),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sandbox_usage_account_idx ON sandbox_usage (account_id, charged_at DESC);
CREATE TRIGGER sandbox_usage_immutable BEFORE UPDATE OR DELETE ON sandbox_usage
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_sandbox_usage_within_budget() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    r_wall   bigint;
    granted  bigint;
    runs     integer;
    cpu      bigint;
    wall     bigint;
BEGIN
    -- The same lock internal/sandboxrt takes: the charges of one account are serialized.
    PERFORM pg_advisory_xact_lock((('x' || substr(md5('sandbox-budget:' || NEW.account_id::text), 1, 16))::bit(64))::bigint);
    SELECT wall_ms, (CASE WHEN termination_reason = 'KILLED_BY_SWITCH' THEN 0 ELSE (limits->>'wall_time_ms')::bigint END)
      INTO r_wall, granted FROM sandbox_runs WHERE id = NEW.run_id;
    IF r_wall IS DISTINCT FROM NEW.wall_ms OR granted IS DISTINCT FROM NEW.cpu_ms THEN
        RAISE EXCEPTION 'SANDBOX_BUDGET: the charge of run % is not its wall time and granted limit', NEW.run_id
            USING ERRCODE = 'SX003';
    END IF;
    SELECT count(*), coalesce(sum(cpu_ms), 0), coalesce(sum(wall_ms), 0) INTO runs, cpu, wall FROM sandbox_usage
     WHERE account_id = NEW.account_id AND charged_at <= NEW.charged_at
       AND charged_at > NEW.charged_at - (NEW.window_ms::text || ' milliseconds')::interval;
    IF runs + 1 > NEW.max_runs OR cpu + NEW.cpu_ms > NEW.max_cpu_ms OR wall >= NEW.max_wall_ms THEN
        RAISE EXCEPTION 'SANDBOX_BUDGET: account % has used % runs, % ms granted and % ms of wall time of its window',
            NEW.account_id, runs, cpu, wall USING ERRCODE = 'SX003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_sandbox_usage_within_budget() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER sandbox_usage_within_budget BEFORE INSERT ON sandbox_usage
    FOR EACH ROW EXECUTE FUNCTION cp_sandbox_usage_within_budget();

-- +goose StatementBegin
CREATE FUNCTION cp_sandbox_run_is_charged() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM sandbox_usage WHERE run_id = NEW.id) THEN
        RAISE EXCEPTION 'SANDBOX_UNCHARGED: sandbox run % is charged to no account', NEW.id USING ERRCODE = 'SX002';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_sandbox_run_is_charged() SET search_path = pg_catalog, public, pg_temp;
-- Named to fire after sandbox_runs_violations_complete (SX001): triggers of one event fire in name order.
CREATE CONSTRAINT TRIGGER sandbox_runs_were_charged AFTER INSERT ON sandbox_runs
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_sandbox_run_is_charged();

-- ---------------------------------------------------------------------------
-- 6. Reads and privileges
-- ---------------------------------------------------------------------------

CREATE INDEX marketplace_follow_runs_account_idx ON marketplace_follow_runs (account_id, created_at DESC, id DESC);

GRANT SELECT, INSERT ON marketplace_follow_marks, marketplace_paper_fills, marketplace_validation_claims,
    marketplace_validation_claim_releases, sandbox_usage TO cp_app;
GRANT SELECT ON marketplace_follow_marks, marketplace_paper_fills, marketplace_validation_claims,
    marketplace_validation_claim_releases, sandbox_usage TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: fills, marks, claims and charges are evidence of what ran and what it was worth
