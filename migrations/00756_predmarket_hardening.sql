-- +goose Up
-- Prediction market hardening (internal/predmarket; amends 00749 without editing it).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. An outcome share exists only as its market issued it. A journal entry on a registered share asset
--      belongs to a PREDICTION_SET_ISSUANCE, PREDICTION_SET_REDEMPTION, PREDICTION_PAYOUT or PREDICTION_REFUND
--      transaction, booked to a customer's WALLET, TRADING_INFLOW or TRADING_OUTFLOW or to the share's own
--      market facility's PREDICTION_SHARES_ISSUED or PREDICTION_SHARES_OUTSTANDING, or to a SIM_TRADE between
--      customer accounts (the internal order book). No HOUSE book, PLATFORM account or other facility can ever
--      hold a share, and no other journal kind -- a faucet grant, a transfer, a compensation -- can create, move
--      or destroy one. Every such transaction changes the customers' share wallets by exactly what it changes
--      the market's outstanding shares by (a trade: by nothing), so the sum of customer WALLET balances of a
--      share always equals its PREDICTION_SHARES_OUTSTANDING; a share asset is registered only before any
--      ledger account names it, so both start at zero.
--   2. Voiding is dual-controlled. A void is proposed by one OPERATOR with a reason and archived evidence and
--      approved by a different OPERATOR; neither may be the market's creator or the author of its resolution in
--      force, and the approver may not be the operator who disputed that resolution. A proposal names the
--      resolution in force when it was made and cannot be approved once another is in force. A RESOLVED market
--      inside its dispute window cannot be voided (it is disputed first). A market becomes VOIDED only with an
--      approval, its VOIDED transition row is written by the approver, and its REFUND record is settled by them.
--   3. Dispute rulings and resolutions are independent: the creator of a market, an operator who recorded an
--      oracle observation for it, the operator whose resolution is disputed and the operator who raised the
--      dispute can never accept a resolution of it (an oracle resolution or a ruling, upheld or overturned).
--   4. A payout or refund the platform cannot deliver to its holder (a frozen or closed account, an unverified
--      identity) is HELD: the holder's shares are retired like everyone else's and the amount stays in the
--      market's PREDICTION_ESCROW, recorded per holder. A settlement record states total_held, the held records
--      of a settlement sum to exactly that (checked at COMMIT), and a held payout is claimed at most once, for
--      exactly its amount, by its own account, through a journal that pays that account's WALLET out of that
--      market's escrow. The escrow identity is therefore: escrow = outstanding shares + residual + unclaimed held.
--   5. Sandbox oracle observations are durable: every observation the sandbox oracle signs is an append-only row
--      (market, outcome, observed_at, the evidence and its sha256, the signature and report hash, who recorded it
--      and when) naming one of its market's outcomes and the market's own oracle source.
--
-- Custom SQLSTATEs (prefix PM): PM006 a separation-of-duties breach (a creator, an observation recorder, the
-- author of a disputed resolution or its disputer accepting a resolution of the market; a void proposed or
-- approved by an operator the rules exclude, or approved by its proposer), PM007 a void outside its rules (no
-- approved proposal behind a VOIDED market, its transition row or its refund record; a proposal or approval that
-- does not match the market as it stands; a RESOLVED market voided inside its dispute window), PM008 an outcome
-- share moved outside its market (a foreign journal kind or holder, a trade that creates or destroys shares, a
-- share asset registered after a ledger account named it), PM009 a held payout or claim that does not match its
-- settlement, effect, journal or account, PM010 an oracle observation that does not name its market's outcome
-- and oracle source, or names a market already settled or voided.

-- ---------------------------------------------------------------------------
-- 1. Outcome shares move only as their market moves them
-- ---------------------------------------------------------------------------

-- Every journal entry pays one probe of the registry's unique index on share_asset_id; only entries on a share
-- asset go further.
-- +goose StatementBegin
CREATE FUNCTION cp_prediction_share_entry_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    mkt record;
    k   text;
    la  record;
BEGIN
    SELECT o.market_id, pm.facility_id INTO mkt
      FROM prediction_outcomes_registry o JOIN prediction_markets pm ON pm.id = o.market_id
     WHERE o.share_asset_id = NEW.asset_id;
    IF NOT FOUND THEN
        RETURN NEW;
    END IF;
    SELECT kind INTO k FROM journal_transactions WHERE id = NEW.transaction_id;
    IF k IS NULL OR k NOT IN ('PREDICTION_SET_ISSUANCE','PREDICTION_SET_REDEMPTION','PREDICTION_PAYOUT',
                              'PREDICTION_REFUND','SIM_TRADE') THEN
        RAISE EXCEPTION 'PREDICTION_SHARE_FOREIGN_KIND: outcome share % of market % cannot move in a % transaction',
            NEW.asset_id, mkt.market_id, coalesce(k, 'unknown') USING ERRCODE = 'PM008';
    END IF;
    SELECT owner_type, owner_id, code INTO la FROM ledger_accounts WHERE id = NEW.ledger_account_id;
    IF la.owner_type = 'CUSTOMER' AND la.code IN ('WALLET','TRADING_INFLOW','TRADING_OUTFLOW') THEN
        RETURN NEW;
    END IF;
    IF k <> 'SIM_TRADE' AND la.owner_type = 'FACILITY' AND la.owner_id = mkt.facility_id
       AND la.code IN ('PREDICTION_SHARES_ISSUED','PREDICTION_SHARES_OUTSTANDING') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'PREDICTION_SHARE_FOREIGN_HOLDER: outcome share % of market % cannot be booked to %/%/% in a % transaction',
        NEW.asset_id, mkt.market_id, la.owner_type, la.owner_id, la.code, k USING ERRCODE = 'PM008';
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_share_entry_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER journal_entries_prediction_share_guard BEFORE INSERT ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION cp_prediction_share_entry_guard();

-- At COMMIT, once per transaction: for every share asset it touches, the customers' WALLET balances move by
-- exactly what the market's PREDICTION_SHARES_OUTSTANDING moves by. A trade moves both by nothing; an
-- issuance, redemption, payout or refund moves both by the same amount.
-- +goose StatementBegin
CREATE FUNCTION cp_check_prediction_share_journal() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    flag text;
    bad  record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM prediction_outcomes_registry WHERE share_asset_id = NEW.asset_id) THEN
        RETURN NULL;
    END IF;
    flag := 'cp.predmarket.shares.x' || translate(NEW.transaction_id::text, '-', '_');
    IF current_setting(flag, true) = '1' THEN
        RETURN NULL;
    END IF;
    PERFORM set_config(flag, '1', true);

    SELECT e.asset_id,
           sum(CASE WHEN la.owner_type = 'CUSTOMER' AND la.code = 'WALLET'
                    THEN CASE WHEN e.side = 'DEBIT' THEN e.quantity ELSE -e.quantity END ELSE 0 END) AS wallets,
           sum(CASE WHEN la.owner_type = 'FACILITY' AND la.code = 'PREDICTION_SHARES_OUTSTANDING'
                    THEN CASE WHEN e.side = 'CREDIT' THEN e.quantity ELSE -e.quantity END ELSE 0 END) AS outstanding
      INTO bad
      FROM journal_entries e
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
      JOIN prediction_outcomes_registry o ON o.share_asset_id = e.asset_id
     WHERE e.transaction_id = NEW.transaction_id
     GROUP BY e.asset_id
    HAVING sum(CASE WHEN la.owner_type = 'CUSTOMER' AND la.code = 'WALLET'
                    THEN CASE WHEN e.side = 'DEBIT' THEN e.quantity ELSE -e.quantity END ELSE 0 END)
        <> sum(CASE WHEN la.owner_type = 'FACILITY' AND la.code = 'PREDICTION_SHARES_OUTSTANDING'
                    THEN CASE WHEN e.side = 'CREDIT' THEN e.quantity ELSE -e.quantity END ELSE 0 END)
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'PREDICTION_SHARES_NOT_CONSERVED: transaction % moves customer wallets of share % by % and its outstanding shares by %',
            NEW.transaction_id, bad.asset_id, bad.wallets, bad.outstanding USING ERRCODE = 'PM008';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_check_prediction_share_journal() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER journal_entries_prediction_shares_conserved AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_check_prediction_share_journal();

-- A share asset is registered before any value moves in it: an asset some ledger account already names (a
-- faucet grant, a stray posting) can never become an outcome share. Otherwise as 00749.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_prediction_outcome_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    m record;
BEGIN
    SELECT status, outcome_count INTO m FROM prediction_markets WHERE id = NEW.market_id;
    IF NOT FOUND OR m.status <> 'DRAFT' OR NEW.outcome_index >= m.outcome_count THEN
        RAISE EXCEPTION 'PREDICTION_OUTCOME_SET_FROZEN: outcome % of market % is out of range or its market is not a DRAFT',
            NEW.outcome_index, NEW.market_id USING ERRCODE = 'PM005';
    END IF;
    PERFORM cp_prediction_asset_is_simulated(NEW.share_asset_id, 'outcome share asset');
    IF EXISTS (SELECT 1 FROM ledger_accounts WHERE asset_id = NEW.share_asset_id) THEN
        RAISE EXCEPTION 'PREDICTION_SHARE_PREEXISTING: asset % already has ledger accounts and cannot become an outcome share of market %',
            NEW.share_asset_id, NEW.market_id USING ERRCODE = 'PM008';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 2. Sandbox oracle observations
-- ---------------------------------------------------------------------------

CREATE TABLE prediction_oracle_observations (
    id                      uuid PRIMARY KEY,
    market_id               uuid NOT NULL REFERENCES prediction_markets(id),
    outcome_index           smallint NOT NULL,
    outcome_symbol          text NOT NULL,
    source                  text NOT NULL CHECK (length(source) BETWEEN 1 AND 200),
    observed_at             timestamptz NOT NULL,
    evidence                bytea NOT NULL CHECK (length(evidence) BETWEEN 1 AND 16384),
    evidence_sha256         bytea NOT NULL,
    signature               bytea NOT NULL CHECK (length(signature) = 64),
    report_hash             bytea NOT NULL CHECK (length(report_hash) = 32),
    recorded_by_actor_type  text NOT NULL CHECK (recorded_by_actor_type = 'OPERATOR'),
    recorded_by_actor_id    text NOT NULL CHECK (length(recorded_by_actor_id) BETWEEN 1 AND 200),
    recorded_at             timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    -- The digest is the evidence's own.
    CONSTRAINT prediction_oracle_observations_evidence_digest CHECK (evidence_sha256 = sha256(evidence)),
    -- The same signed observation is recorded once; recording it again is a replay.
    UNIQUE (market_id, report_hash),
    FOREIGN KEY (market_id, outcome_index) REFERENCES prediction_outcomes_registry (market_id, outcome_index)
);
CREATE INDEX prediction_oracle_observations_latest_idx ON prediction_oracle_observations (market_id, recorded_at DESC, id DESC);
CREATE INDEX prediction_oracle_observations_recorder_idx ON prediction_oracle_observations (market_id, lower(recorded_by_actor_id));
CREATE TRIGGER prediction_oracle_observations_immutable BEFORE UPDATE OR DELETE ON prediction_oracle_observations
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_prediction_observation_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    m   record;
    sym text;
BEGIN
    SELECT status, oracle_source INTO m FROM prediction_markets WHERE id = NEW.market_id;
    SELECT symbol INTO sym FROM prediction_outcomes_registry WHERE market_id = NEW.market_id AND outcome_index = NEW.outcome_index;
    IF NOT FOUND OR m.status IN ('SETTLED','VOIDED') OR sym IS DISTINCT FROM NEW.outcome_symbol
       OR m.oracle_source IS DISTINCT FROM NEW.source THEN
        RAISE EXCEPTION 'PREDICTION_OBSERVATION_FOREIGN: observation of % by % does not name an outcome and the oracle of open market %',
            NEW.outcome_symbol, NEW.source, NEW.market_id USING ERRCODE = 'PM010';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_observation_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER prediction_oracle_observations_guard BEFORE INSERT ON prediction_oracle_observations
    FOR EACH ROW EXECUTE FUNCTION cp_prediction_observation_guard();

-- ---------------------------------------------------------------------------
-- 3. Resolutions and rulings are accepted by someone independent of them
-- ---------------------------------------------------------------------------

-- As 00749 (a resolution only for a market awaiting one), plus separation of duties.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_prediction_resolution_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    m        record;
    author   text;
    disputer text;
BEGIN
    SELECT status, current_resolution_id, created_by_actor_id INTO m FROM prediction_markets WHERE id = NEW.market_id;
    IF NOT FOUND
       OR (NEW.kind = 'ORACLE' AND m.status <> 'RESOLVING')
       OR (NEW.kind <> 'ORACLE' AND (
              m.status <> 'DISPUTED'
              OR NEW.supersedes_id IS DISTINCT FROM m.current_resolution_id
              OR NOT EXISTS (SELECT 1 FROM prediction_disputes d
                              WHERE d.id = NEW.dispute_id AND d.resolution_id = NEW.supersedes_id))) THEN
        RAISE EXCEPTION 'PREDICTION_LIFECYCLE: market % is not awaiting a % resolution', NEW.market_id, NEW.kind
            USING ERRCODE = 'PM001';
    END IF;
    IF lower(NEW.accepted_by_actor_id) = lower(m.created_by_actor_id) THEN
        RAISE EXCEPTION 'PREDICTION_DUAL_CONTROL: the creator of market % cannot resolve it', NEW.market_id
            USING ERRCODE = 'PM006';
    END IF;
    IF EXISTS (SELECT 1 FROM prediction_oracle_observations o
                WHERE o.market_id = NEW.market_id AND lower(o.recorded_by_actor_id) = lower(NEW.accepted_by_actor_id)) THEN
        RAISE EXCEPTION 'PREDICTION_DUAL_CONTROL: % recorded an oracle observation for market % and cannot resolve it',
            NEW.accepted_by_actor_id, NEW.market_id USING ERRCODE = 'PM006';
    END IF;
    IF NEW.kind <> 'ORACLE' THEN
        SELECT accepted_by_actor_id INTO author FROM prediction_resolutions WHERE id = NEW.supersedes_id;
        SELECT raised_by_actor_id INTO disputer FROM prediction_disputes WHERE id = NEW.dispute_id;
        IF lower(NEW.accepted_by_actor_id) = lower(author) THEN
            RAISE EXCEPTION 'PREDICTION_DUAL_CONTROL: % accepted the disputed resolution % and cannot rule on its dispute',
                NEW.accepted_by_actor_id, NEW.supersedes_id USING ERRCODE = 'PM006';
        END IF;
        IF lower(NEW.accepted_by_actor_id) = lower(disputer) THEN
            RAISE EXCEPTION 'PREDICTION_DUAL_CONTROL: % raised dispute % and cannot rule on it',
                NEW.accepted_by_actor_id, NEW.dispute_id USING ERRCODE = 'PM006';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 4. Voids: proposed by one operator, approved by another
-- ---------------------------------------------------------------------------

CREATE TABLE prediction_void_proposals (
    id                      uuid PRIMARY KEY,
    market_id               uuid NOT NULL REFERENCES prediction_markets(id),
    -- The market as the proposer saw it: its status and the resolution in force (none before resolution).
    market_status           text NOT NULL CHECK (market_status IN ('DRAFT','OPEN','CLOSED','RESOLVING','RESOLVED','DISPUTED')),
    resolution_id           uuid,
    reason                  text NOT NULL CHECK (length(reason) BETWEEN 8 AND 2000),
    evidence_ref            text NOT NULL CHECK (length(evidence_ref) > 0),
    evidence_sha256         bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    proposed_by_actor_type  text NOT NULL CHECK (proposed_by_actor_type = 'OPERATOR'),
    proposed_by_actor_id    text NOT NULL CHECK (length(proposed_by_actor_id) BETWEEN 1 AND 200),
    proposed_at             timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, market_id),
    FOREIGN KEY (resolution_id, market_id) REFERENCES prediction_resolutions (id, market_id)
);
CREATE INDEX prediction_void_proposals_market_idx ON prediction_void_proposals (market_id, proposed_at, id);
CREATE TRIGGER prediction_void_proposals_immutable BEFORE UPDATE OR DELETE ON prediction_void_proposals
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE prediction_void_approvals (
    id                      uuid PRIMARY KEY,
    proposal_id             uuid NOT NULL UNIQUE,
    -- A market is voided once, so it has at most one approved proposal.
    market_id               uuid NOT NULL UNIQUE REFERENCES prediction_markets(id),
    reason                  text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
    approved_by_actor_type  text NOT NULL CHECK (approved_by_actor_type = 'OPERATOR'),
    approved_by_actor_id    text NOT NULL CHECK (length(approved_by_actor_id) BETWEEN 1 AND 200),
    approved_at             timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (proposal_id, market_id) REFERENCES prediction_void_proposals (id, market_id)
);
CREATE TRIGGER prediction_void_approvals_immutable BEFORE UPDATE OR DELETE ON prediction_void_approvals
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_prediction_void_proposal_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    m record;
    r record;
BEGIN
    SELECT status, current_resolution_id, created_by_actor_id, dispute_window_ms INTO m
      FROM prediction_markets WHERE id = NEW.market_id;
    IF NOT FOUND OR m.status IN ('SETTLED','VOIDED') OR NEW.market_status IS DISTINCT FROM m.status
       OR NEW.resolution_id IS DISTINCT FROM m.current_resolution_id THEN
        RAISE EXCEPTION 'PREDICTION_VOID: a void proposal for market % does not describe the market as it stands', NEW.market_id
            USING ERRCODE = 'PM007';
    END IF;
    IF lower(NEW.proposed_by_actor_id) = lower(m.created_by_actor_id) THEN
        RAISE EXCEPTION 'PREDICTION_DUAL_CONTROL: the creator of market % cannot propose to void it', NEW.market_id
            USING ERRCODE = 'PM006';
    END IF;
    IF m.current_resolution_id IS NOT NULL THEN
        SELECT accepted_by_actor_id, accepted_at INTO r FROM prediction_resolutions WHERE id = m.current_resolution_id;
        IF lower(NEW.proposed_by_actor_id) = lower(r.accepted_by_actor_id) THEN
            RAISE EXCEPTION 'PREDICTION_DUAL_CONTROL: % accepted the resolution in force of market % and cannot propose to void it',
                NEW.proposed_by_actor_id, NEW.market_id USING ERRCODE = 'PM006';
        END IF;
        IF m.status = 'RESOLVED' AND NEW.proposed_at < r.accepted_at + (m.dispute_window_ms::text || ' milliseconds')::interval THEN
            RAISE EXCEPTION 'PREDICTION_VOID: market % is inside its dispute window; it is disputed, not voided', NEW.market_id
                USING ERRCODE = 'PM007';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_void_proposal_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER prediction_void_proposals_guard BEFORE INSERT ON prediction_void_proposals
    FOR EACH ROW EXECUTE FUNCTION cp_prediction_void_proposal_guard();

-- +goose StatementBegin
CREATE FUNCTION cp_prediction_void_approval_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    p        prediction_void_proposals%ROWTYPE;
    m        record;
    r        record;
    disputer text;
BEGIN
    SELECT * INTO p FROM prediction_void_proposals WHERE id = NEW.proposal_id;
    SELECT status, current_resolution_id, created_by_actor_id, dispute_window_ms INTO m
      FROM prediction_markets WHERE id = NEW.market_id;
    IF p.id IS NULL OR p.market_id IS DISTINCT FROM NEW.market_id OR m.status IN ('SETTLED','VOIDED')
       OR p.resolution_id IS DISTINCT FROM m.current_resolution_id THEN
        RAISE EXCEPTION 'PREDICTION_VOID: proposal % no longer describes market % as it stands', NEW.proposal_id, NEW.market_id
            USING ERRCODE = 'PM007';
    END IF;
    IF lower(NEW.approved_by_actor_id) = lower(p.proposed_by_actor_id) THEN
        RAISE EXCEPTION 'PREDICTION_DUAL_CONTROL: % proposed void % and cannot approve it', p.proposed_by_actor_id, p.id
            USING ERRCODE = 'PM006';
    END IF;
    IF lower(NEW.approved_by_actor_id) = lower(m.created_by_actor_id) THEN
        RAISE EXCEPTION 'PREDICTION_DUAL_CONTROL: the creator of market % cannot approve its void', NEW.market_id
            USING ERRCODE = 'PM006';
    END IF;
    IF m.current_resolution_id IS NOT NULL THEN
        SELECT accepted_by_actor_id, accepted_at INTO r FROM prediction_resolutions WHERE id = m.current_resolution_id;
        IF lower(NEW.approved_by_actor_id) = lower(r.accepted_by_actor_id)
           OR lower(p.proposed_by_actor_id) = lower(r.accepted_by_actor_id) THEN
            RAISE EXCEPTION 'PREDICTION_DUAL_CONTROL: the author of the resolution in force of market % takes no part in voiding it',
                NEW.market_id USING ERRCODE = 'PM006';
        END IF;
        IF m.status = 'DISPUTED' THEN
            SELECT raised_by_actor_id INTO disputer FROM prediction_disputes WHERE resolution_id = m.current_resolution_id;
            IF lower(NEW.approved_by_actor_id) = lower(disputer) THEN
                RAISE EXCEPTION 'PREDICTION_DUAL_CONTROL: % raised the dispute on market % and cannot approve the void that rules on it',
                    NEW.approved_by_actor_id, NEW.market_id USING ERRCODE = 'PM006';
            END IF;
        END IF;
        IF m.status = 'RESOLVED' AND NEW.approved_at < r.accepted_at + (m.dispute_window_ms::text || ' milliseconds')::interval THEN
            RAISE EXCEPTION 'PREDICTION_VOID: market % is inside its dispute window; it is disputed, not voided', NEW.market_id
                USING ERRCODE = 'PM007';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_void_approval_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER prediction_void_approvals_guard BEFORE INSERT ON prediction_void_approvals
    FOR EACH ROW EXECUTE FUNCTION cp_prediction_void_approval_guard();

-- The VOIDED transition row is written by the operator who approved the void.
-- +goose StatementBegin
CREATE FUNCTION cp_prediction_void_transition_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM prediction_void_approvals a
                    WHERE a.market_id = NEW.market_id AND NEW.actor_type = 'OPERATOR'
                      AND lower(a.approved_by_actor_id) = lower(NEW.actor_id)) THEN
        RAISE EXCEPTION 'PREDICTION_VOID: market % moves to VOIDED only by the operator who approved its void', NEW.market_id
            USING ERRCODE = 'PM007';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_void_transition_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER prediction_market_transitions_void_approved BEFORE INSERT ON prediction_market_transitions
    FOR EACH ROW WHEN (NEW.to_status = 'VOIDED') EXECUTE FUNCTION cp_prediction_void_transition_guard();

-- ---------------------------------------------------------------------------
-- 5. Settlements: held payouts, and refunds only on an approved void
-- ---------------------------------------------------------------------------

ALTER TABLE prediction_settlements ADD COLUMN total_held numeric(38,0) NOT NULL DEFAULT 0;
ALTER TABLE prediction_settlements ADD CONSTRAINT prediction_settlements_held_within_paid
    CHECK (total_held >= 0 AND total_held <= total_paid);
ALTER TABLE prediction_settlements ADD CONSTRAINT prediction_settlements_id_market_key UNIQUE (id, market_id);

-- As 00749, plus: a REFUND record exists only for an approved void and is settled by its approver.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_prediction_settlement_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    m record;
BEGIN
    SELECT status, current_resolution_id, outcome_count INTO m FROM prediction_markets WHERE id = NEW.market_id;
    IF NOT FOUND OR NEW.outcome_count IS DISTINCT FROM m.outcome_count
       OR (NEW.kind = 'PAYOUT' AND (m.status <> 'RESOLVED' OR NEW.resolution_id IS DISTINCT FROM m.current_resolution_id))
       OR (NEW.kind = 'REFUND' AND (m.status = 'SETTLED' OR m.status = 'VOIDED')) THEN
        RAISE EXCEPTION 'PREDICTION_UNSETTLED: a % record does not pay market % as it stands', NEW.kind, NEW.market_id
            USING ERRCODE = 'PM003';
    END IF;
    IF NEW.kind = 'REFUND' AND NOT EXISTS (
           SELECT 1 FROM prediction_void_approvals a
            WHERE a.market_id = NEW.market_id AND NEW.settled_by_actor_type = 'OPERATOR'
              AND lower(a.approved_by_actor_id) = lower(NEW.settled_by_actor_id)) THEN
        RAISE EXCEPTION 'PREDICTION_VOID: a REFUND of market % is settled only by the operator who approved its void', NEW.market_id
            USING ERRCODE = 'PM007';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- As 00749, plus: VOIDED needs an approved void proposal by two distinct operators behind it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_prediction_market_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'DRAFT' OR NEW.current_resolution_id IS NOT NULL THEN
            RAISE EXCEPTION 'PREDICTION_LIFECYCLE: market % must be born DRAFT with no resolution, not %', NEW.id, NEW.status
                USING ERRCODE = 'PM001';
        END IF;
        PERFORM cp_prediction_asset_is_simulated(NEW.collateral_asset_id, 'collateral asset');
        RETURN NEW;
    END IF;

    IF (NEW.id, NEW.symbol, NEW.question, NEW.mode, NEW.facility_id, NEW.instrument_id, NEW.collateral_asset_id,
        NEW.outcome_count, NEW.unit_quantity, NEW.open_at, NEW.close_at, NEW.dispute_window_ms, NEW.oracle_source,
        NEW.oracle_public_key, NEW.definition_hash, NEW.created_by_actor_type, NEW.created_by_actor_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.symbol, OLD.question, OLD.mode, OLD.facility_id, OLD.instrument_id, OLD.collateral_asset_id,
        OLD.outcome_count, OLD.unit_quantity, OLD.open_at, OLD.close_at, OLD.dispute_window_ms, OLD.oracle_source,
        OLD.oracle_public_key, OLD.definition_hash, OLD.created_by_actor_type, OLD.created_by_actor_id, OLD.created_at) THEN
        RAISE EXCEPTION 'PREDICTION_IDENTITY_FROZEN: market % may change only its status and its resolution', OLD.id
            USING ERRCODE = 'PM002';
    END IF;

    IF NEW.current_resolution_id IS DISTINCT FROM OLD.current_resolution_id
       AND NOT (NEW.status = 'RESOLVED' AND OLD.status <> 'RESOLVED' AND NEW.current_resolution_id IS NOT NULL) THEN
        RAISE EXCEPTION 'PREDICTION_IDENTITY_FROZEN: market % moved its resolution outside a transition into RESOLVED', OLD.id
            USING ERRCODE = 'PM002';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT cp_prediction_market_transition_legal(OLD.status, NEW.status) THEN
            RAISE EXCEPTION 'PREDICTION_LIFECYCLE: market % cannot move % -> %', OLD.id, OLD.status, NEW.status
                USING ERRCODE = 'PM001';
        END IF;
        IF NEW.status = 'SETTLED' AND NOT EXISTS (
               SELECT 1 FROM prediction_settlements s
                WHERE s.market_id = NEW.id AND s.kind = 'PAYOUT' AND s.resolution_id = NEW.current_resolution_id) THEN
            RAISE EXCEPTION 'PREDICTION_UNSETTLED: market % cannot be SETTLED without the payout of its current resolution', NEW.id
                USING ERRCODE = 'PM003';
        END IF;
        IF NEW.status = 'VOIDED' AND NOT EXISTS (
               SELECT 1 FROM prediction_settlements s WHERE s.market_id = NEW.id AND s.kind = 'REFUND') THEN
            RAISE EXCEPTION 'PREDICTION_UNSETTLED: market % cannot be VOIDED without its refund record', NEW.id
                USING ERRCODE = 'PM003';
        END IF;
        IF NEW.status = 'VOIDED' AND NOT EXISTS (
               SELECT 1 FROM prediction_void_approvals a JOIN prediction_void_proposals p ON p.id = a.proposal_id
                WHERE a.market_id = NEW.id AND p.market_id = NEW.id
                  AND lower(a.approved_by_actor_id) <> lower(p.proposed_by_actor_id)) THEN
            RAISE EXCEPTION 'PREDICTION_VOID: market % cannot be VOIDED without a void proposed and approved by two operators', NEW.id
                USING ERRCODE = 'PM007';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- A held payout: the holder's amount, kept in the market's escrow, from a settlement that disclosed it.
CREATE TABLE prediction_held_payouts (
    id                      uuid PRIMARY KEY,
    market_id               uuid NOT NULL REFERENCES prediction_markets(id),
    settlement_id           uuid NOT NULL,
    account_id              uuid NOT NULL REFERENCES accounts(id),
    kind                    text NOT NULL CHECK (kind IN ('PAYOUT','REFUND')),
    collateral_asset_id     uuid NOT NULL REFERENCES assets(id),
    amount                  numeric(38,0) NOT NULL CHECK (amount > 0),
    -- Why the payout could not be delivered: the refusal's stable reason codes.
    reason_codes            text[] NOT NULL CHECK (cardinality(reason_codes) BETWEEN 1 AND 32),
    -- The holder's payout intent, refused by the route or Risk V2 for the holder's own reasons.
    refused_intent_id       uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    -- The economic effect and journal that retired the holder's shares and kept the amount in escrow.
    effect_id               uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    journal_transaction_id  uuid NOT NULL UNIQUE REFERENCES journal_transactions(id),
    evidence_ref            text NOT NULL CHECK (length(evidence_ref) > 0),
    evidence_sha256         bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    held_at                 timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (market_id, account_id),
    UNIQUE (id, market_id),
    FOREIGN KEY (settlement_id, market_id) REFERENCES prediction_settlements (id, market_id)
);
CREATE INDEX prediction_held_payouts_account_idx ON prediction_held_payouts (account_id, held_at);
CREATE TRIGGER prediction_held_payouts_immutable BEFORE UPDATE OR DELETE ON prediction_held_payouts
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_prediction_held_payout_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    st  record;
    m   record;
    eff record;
    jk  text;
    it  record;
    ak  text;
BEGIN
    SELECT kind INTO st FROM prediction_settlements WHERE id = NEW.settlement_id AND market_id = NEW.market_id;
    SELECT collateral_asset_id INTO m FROM prediction_markets WHERE id = NEW.market_id;
    SELECT status, journal_transaction_id, account_id, subsystem INTO eff FROM economic_effects WHERE id = NEW.effect_id;
    SELECT kind INTO jk FROM journal_transactions WHERE id = NEW.journal_transaction_id;
    SELECT account_id, status, kind INTO it FROM economic_intents WHERE id = NEW.refused_intent_id;
    SELECT kind INTO ak FROM accounts WHERE id = NEW.account_id;
    IF st.kind IS DISTINCT FROM NEW.kind OR m.collateral_asset_id IS DISTINCT FROM NEW.collateral_asset_id
       OR ak IS DISTINCT FROM 'CUSTOMER'
       OR eff.status IS DISTINCT FROM 'APPLIED' OR eff.journal_transaction_id IS DISTINCT FROM NEW.journal_transaction_id
       OR eff.account_id IS DISTINCT FROM NEW.account_id OR eff.subsystem IS DISTINCT FROM 'PREDICTION_MARKETS'
       OR jk IS DISTINCT FROM (CASE NEW.kind WHEN 'PAYOUT' THEN 'PREDICTION_PAYOUT' ELSE 'PREDICTION_REFUND' END)
       OR it.account_id IS DISTINCT FROM NEW.account_id OR it.status IS DISTINCT FROM 'REJECTED'
       OR it.kind IS DISTINCT FROM 'RESOLVE_PREDICTION_POSITION' THEN
        RAISE EXCEPTION 'PREDICTION_HELD_PAYOUT: held % of market % for account % does not match its settlement, effect, journal and refused intent',
            NEW.kind, NEW.market_id, NEW.account_id USING ERRCODE = 'PM009';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_held_payout_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER prediction_held_payouts_guard BEFORE INSERT ON prediction_held_payouts
    FOR EACH ROW EXECUTE FUNCTION cp_prediction_held_payout_guard();

-- At COMMIT: the held records of a settlement sum to exactly the total_held it states.
-- +goose StatementBegin
CREATE FUNCTION cp_check_prediction_held_total() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    sid    uuid;
    stated numeric(38,0);
    held   numeric(38,0);
BEGIN
    IF TG_TABLE_NAME = 'prediction_settlements' THEN
        sid := NEW.id;
    ELSE
        sid := NEW.settlement_id;
    END IF;
    SELECT total_held INTO stated FROM prediction_settlements WHERE id = sid;
    SELECT coalesce(sum(amount), 0) INTO held FROM prediction_held_payouts WHERE settlement_id = sid;
    IF stated IS DISTINCT FROM held THEN
        RAISE EXCEPTION 'PREDICTION_HELD_PAYOUT: settlement % states % held and its held payouts sum to %', sid, stated, held
            USING ERRCODE = 'PM009';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_check_prediction_held_total() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER prediction_settlements_held_total AFTER INSERT ON prediction_settlements
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_check_prediction_held_total();
CREATE CONSTRAINT TRIGGER prediction_held_payouts_total AFTER INSERT ON prediction_held_payouts
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_check_prediction_held_total();

CREATE TABLE prediction_held_payout_claims (
    id                      uuid PRIMARY KEY,
    -- A held payout is claimed once.
    held_payout_id          uuid NOT NULL UNIQUE,
    market_id               uuid NOT NULL REFERENCES prediction_markets(id),
    account_id              uuid NOT NULL REFERENCES accounts(id),
    amount                  numeric(38,0) NOT NULL CHECK (amount > 0),
    intent_id               uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    effect_id               uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    journal_transaction_id  uuid NOT NULL UNIQUE REFERENCES journal_transactions(id),
    claimed_by_actor_type   text NOT NULL CHECK (claimed_by_actor_type IN ('USER','SERVICE')),
    claimed_by_actor_id     text NOT NULL CHECK (length(claimed_by_actor_id) BETWEEN 1 AND 200),
    evidence_ref            text NOT NULL CHECK (length(evidence_ref) > 0),
    evidence_sha256         bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    claimed_at              timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (held_payout_id, market_id) REFERENCES prediction_held_payouts (id, market_id)
);
CREATE TRIGGER prediction_held_payout_claims_immutable BEFORE UPDATE OR DELETE ON prediction_held_payout_claims
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- A claim pays exactly the held amount to the held account's WALLET, out of that market's escrow, through
-- the journal of its own APPLIED effect and COMPLETED intent, once the market has settled or been voided.
-- +goose StatementBegin
CREATE FUNCTION cp_prediction_held_claim_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    h   prediction_held_payouts%ROWTYPE;
    m   record;
    eff record;
    it  record;
    jk  text;
BEGIN
    SELECT * INTO h FROM prediction_held_payouts WHERE id = NEW.held_payout_id;
    SELECT status, facility_id INTO m FROM prediction_markets WHERE id = NEW.market_id;
    SELECT status, journal_transaction_id, account_id, subsystem, intent_id INTO eff FROM economic_effects WHERE id = NEW.effect_id;
    SELECT account_id, status, kind INTO it FROM economic_intents WHERE id = NEW.intent_id;
    SELECT kind INTO jk FROM journal_transactions WHERE id = NEW.journal_transaction_id;
    IF h.id IS NULL OR h.market_id IS DISTINCT FROM NEW.market_id OR h.account_id IS DISTINCT FROM NEW.account_id
       OR h.amount IS DISTINCT FROM NEW.amount OR m.status NOT IN ('SETTLED','VOIDED')
       OR eff.status IS DISTINCT FROM 'APPLIED' OR eff.journal_transaction_id IS DISTINCT FROM NEW.journal_transaction_id
       OR eff.account_id IS DISTINCT FROM NEW.account_id OR eff.subsystem IS DISTINCT FROM 'PREDICTION_MARKETS'
       OR eff.intent_id IS DISTINCT FROM NEW.intent_id
       OR it.account_id IS DISTINCT FROM NEW.account_id OR it.status IS DISTINCT FROM 'COMPLETED'
       OR it.kind IS DISTINCT FROM 'RESOLVE_PREDICTION_POSITION'
       OR jk IS DISTINCT FROM (CASE h.kind WHEN 'PAYOUT' THEN 'PREDICTION_PAYOUT' ELSE 'PREDICTION_REFUND' END)
       OR NOT EXISTS (SELECT 1 FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
                       WHERE e.transaction_id = NEW.journal_transaction_id AND la.owner_type = 'CUSTOMER'
                         AND la.owner_id = NEW.account_id AND la.code = 'WALLET' AND e.asset_id = h.collateral_asset_id
                         AND e.side = 'DEBIT' AND e.quantity = NEW.amount)
       OR NOT EXISTS (SELECT 1 FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
                       WHERE e.transaction_id = NEW.journal_transaction_id AND la.owner_type = 'FACILITY'
                         AND la.owner_id = m.facility_id AND la.code = 'PREDICTION_ESCROW' AND e.asset_id = h.collateral_asset_id
                         AND e.side = 'CREDIT' AND e.quantity = NEW.amount) THEN
        RAISE EXCEPTION 'PREDICTION_HELD_PAYOUT: claim of held payout % does not pay exactly its amount to its account out of market % escrow',
            NEW.held_payout_id, NEW.market_id USING ERRCODE = 'PM009';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_held_claim_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER prediction_held_payout_claims_guard BEFORE INSERT ON prediction_held_payout_claims
    FOR EACH ROW EXECUTE FUNCTION cp_prediction_held_claim_guard();

-- ---------------------------------------------------------------------------
-- 6. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON prediction_oracle_observations, prediction_void_proposals, prediction_void_approvals,
                        prediction_held_payouts, prediction_held_payout_claims TO cp_app;
GRANT SELECT ON prediction_oracle_observations, prediction_void_proposals, prediction_void_approvals,
                prediction_held_payouts, prediction_held_payout_claims TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: prediction market history, its observations, voids and held payouts are never dropped
