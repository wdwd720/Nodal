-- +goose Up
-- Move the five-condition activation invariant of PARTS 54/55/244 out of application code and
-- into the database, so that it holds against anything holding the application's credential and
-- not only against an operator using the console.
--
-- WHAT WAS WRONG. internal/gates.Evaluate re-derives every condition (state, proposed_by_user_id,
-- approvers, evidence refs, effective_at, expires_at, revoked_at) from the one capability_gates row
-- it is handed, and 00150 granted cp_app table-wide UPDATE plus INSERT on that table. Two
-- single-statement paths therefore enabled a live-money capability with one actor and no approval:
--
--   A. `INSERT INTO capability_gates (..., state, approvers, effective_at, <evidence refs>)
--       VALUES (..., 'ACTIVE', '[<a proposer and two approvers>]', now(), ...)` — nothing constrained
--      the state a row was born in, and a deployment that has never run gates.Bootstrap (it has no
--      callers) has no rows at all, so the UNIQUE (capability, environment) did not stand in the way.
--   B. `INSERT INTO capability_gate_transitions (...,'ACTIVE',...); UPDATE capability_gates
--       SET state='ACTIVE', approvers='[…]', proposed_by_user_id=…, effective_at=now() …` in one
--      transaction — 00603's AU001 binding already refused the bare UPDATE on its own, but cp_app
--      held INSERT on the transitions table, so it could write its own permission slip.
--
-- Both passed every check in Evaluate, because every check read what those statements had just
-- written. This is defence in depth, not a remotely reachable bypass: internal/httpapi exposes no
-- endpoint that writes these columns freely. It matters because the premise of the subsystem is
-- that no single actor can enable live money.
--
-- WHAT THIS MIGRATION DOES.
--   1. cp_app loses UPDATE on capability_gates entirely. Column-level grants (as in 00604 for
--      ledger_accounts) were the intent, but the legitimately-editable set is empty: every write to
--      this table in internal/gates goes through Admin.commit and is part of a state transition, and
--      every remaining column (the approval chain, the evidence references, the validity window)
--      is an input to one of the five conditions. Granting UPDATE on the approval chain while
--      withholding it on `state` would leave a one-transaction forgery: rewrite the chain, then ask
--      the function to activate against it. So the grant is REVOKE, not a column list.
--   2. cp_app loses INSERT on capability_gate_transitions. The gate's history is now written only by
--      the function below, in the same statement as the state change, so an activation without its
--      transition row is impossible rather than merely refused after the fact.
--   3. A gate row can only be born DISABLED, with an empty approval chain and no evidence — PART 244
--      as a database fact rather than a startup routine nobody calls.
--   4. cp_gate_transition() is the only path that moves capability_gates.state. It is SECURITY
--      DEFINER, owned by cp_migrate (which cp_app is not a member of and cannot impersonate), it
--      re-derives the legal-transition table, the dual-control rules and all five activation
--      conditions from the STORED row rather than from what the caller passed, and it writes the row
--      and its capability_gate_transitions record together.
--
-- The Go checks in internal/gates are unchanged and still run first: they give operators precise
-- errors. This is the line that holds when that one is bypassed. Reaching ACTIVE now requires three
-- separate calls by three distinct actor ids, each leaving an immutable transition row — the
-- database cannot authenticate an operator (that is the session layer's job), but it can refuse to
-- hold an ACTIVE row whose approval history is not complete, distinct and self-consistent.
--
-- Custom SQLSTATEs: GT001 malformed request, GT002 illegal transition, GT003 evidence/window,
-- GT004 dual control, GT005 gate not born DISABLED.

-- Mirrors gates.IsHighRisk. High-risk capabilities require the full evidence set (condition 4).
-- +goose StatementBegin
CREATE FUNCTION cp_gate_is_high_risk(p_capability text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT p_capability IN ('LIVE_FUNDING','LIVE_MANUAL_TRADING','LIVE_AGENT_TRADING','WITHDRAWALS',
                            'SECURITIES','CEX_TRADING','CROSS_CHAIN','PREDICTION_MARKETS');
$$;
-- +goose StatementEnd

-- Mirrors gates.CanTransition: the explicit legal-transition table of POLICY_AUTHORITY §1.
-- +goose StatementBegin
CREATE FUNCTION cp_gate_can_transition(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('DISABLED','PENDING_APPROVAL'), ('DISABLED','REVOKED'),
        ('PENDING_APPROVAL','APPROVED'), ('PENDING_APPROVAL','REVOKED'),
        ('APPROVED','ACTIVE'), ('APPROVED','EXPIRED'), ('APPROVED','REVOKED'),
        ('ACTIVE','SUSPENDED'), ('ACTIVE','EXPIRED'), ('ACTIVE','REVOKED'),
        ('SUSPENDED','APPROVED'), ('SUSPENDED','REVOKED'),
        ('REVOKED','PENDING_APPROVAL'),
        ('EXPIRED','PENDING_APPROVAL'), ('EXPIRED','REVOKED'));
$$;
-- +goose StatementEnd

-- PART 244 in the database: a capability gate is created DISABLED, with no approval chain, no
-- evidence and no validity window. Everything after that is a transition (see below). This applies
-- to every role, including cp_migrate: there is no environment in which a gate starts ACTIVE.
-- +goose StatementBegin
CREATE FUNCTION cp_gate_born_disabled() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state <> 'DISABLED'
       OR coalesce(NEW.approval_version, 0) <> 0
       OR coalesce(NEW.approvers, '[]'::jsonb) <> '[]'::jsonb
       OR coalesce(NEW.evidence_hashes, '[]'::jsonb) <> '[]'::jsonb
       OR NEW.proposed_by_user_id IS NOT NULL
       OR NEW.legal_review_ref IS NOT NULL OR NEW.provider_contract_ref IS NOT NULL
       OR NEW.risk_approval_ref IS NOT NULL OR NEW.security_approval_ref IS NOT NULL
       OR NEW.effective_at IS NOT NULL OR NEW.expires_at IS NOT NULL OR NEW.revoked_at IS NOT NULL
    THEN
        RAISE EXCEPTION 'GATE_MUST_BE_BORN_DISABLED: %/% may only be created DISABLED with an empty approval chain',
            NEW.capability, NEW.environment USING ERRCODE = 'GT005';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER capability_gates_born_disabled BEFORE INSERT ON capability_gates
    FOR EACH ROW EXECUTE FUNCTION cp_gate_born_disabled();

-- cp_gate_transition is the only way capability_gates.state moves.
--
-- It takes the operation, not the desired row: the caller says "activate, as this actor, appending
-- this chain entry", and the function decides what the row becomes. Columns an operation does not
-- own keep their stored value, so a caller cannot smuggle a rewritten approval chain, a cleared
-- revoked_at or a widened validity window past the checks by sending them alongside a legal
-- transition. The approval chain is appended to the STORED chain, never replaced (except by
-- 'propose', which starts a new approval version with exactly one PROPOSE entry).
--
-- Returns the resulting row, or no rows when the gate is missing or p_expected_version does not
-- match (the caller maps that to CONFLICT, as the previous optimistic UPDATE did).
--
-- search_path is pinned with pg_temp last so a caller cannot shadow public tables with temporary
-- ones inside a SECURITY DEFINER body.
-- +goose StatementBegin
CREATE FUNCTION cp_gate_transition(
    p_gate_id               uuid,
    p_expected_version      bigint,
    p_op                    text,         -- propose|approve|resume|activate|suspend|revoke|expire
    p_actor_type            text,
    p_actor_id              text,
    p_reason                text,
    p_transition_id         uuid,
    p_evidence_hash         bytea,
    p_occurred_at           timestamptz,
    p_approver              jsonb,        -- chain entry to append; required for propose/approve/resume/activate
    p_legal_review_ref      text,         -- the remaining parameters are read by 'propose' only
    p_provider_contract_ref text,
    p_risk_approval_ref     text,
    p_security_approval_ref text,
    p_evidence_hashes       jsonb,
    p_effective_at          timestamptz,
    p_expires_at            timestamptz
) RETURNS SETOF capability_gates
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    g            capability_gates%ROWTYPE;
    out_row      capability_gates%ROWTYPE;
    v_to         text;
    v_step       text;
    v_want_step  text;
    v_approvers  jsonb;
    v_version    integer;
    v_legal      text;
    v_provider   text;
    v_risk       text;
    v_security   text;
    v_hashes     jsonb;
    v_effective  timestamptz;
    v_expires    timestamptz;
    v_revoked    timestamptz;
    v_revoke_rsn text;
    v_proposer   text;
    v_last       text;
    v_approvals  integer;
BEGIN
    -- 0. Shape of the request.
    IF p_op IS NULL OR p_op NOT IN ('propose','approve','resume','activate','suspend','revoke','expire') THEN
        RAISE EXCEPTION 'GATE_UNKNOWN_OPERATION: %', coalesce(p_op, '<null>') USING ERRCODE = 'GT001';
    END IF;
    IF coalesce(btrim(p_actor_id), '') = '' THEN
        RAISE EXCEPTION 'GATE_ACTOR_REQUIRED: every gate transition names the principal that made it' USING ERRCODE = 'GT001';
    END IF;
    -- Mirrors the actor_type CHECK on capability_gate_transitions and the AGENT ban in gates.actor.
    IF p_actor_type IS NULL OR p_actor_type NOT IN ('USER','OPERATOR','SYSTEM') THEN
        RAISE EXCEPTION 'GATE_ACTOR_TYPE_FORBIDDEN: % may not change a capability gate', coalesce(p_actor_type, '<null>') USING ERRCODE = 'GT001';
    END IF;
    IF coalesce(btrim(p_reason), '') = '' THEN
        RAISE EXCEPTION 'GATE_REASON_REQUIRED: every gate transition carries a reason' USING ERRCODE = 'GT001';
    END IF;
    IF p_transition_id IS NULL OR p_occurred_at IS NULL THEN
        RAISE EXCEPTION 'GATE_TRANSITION_INCOMPLETE: transition id and time are required' USING ERRCODE = 'GT001';
    END IF;

    -- 1. The stored row is the only truth about where the gate is now.
    SELECT * INTO g FROM capability_gates WHERE id = p_gate_id FOR UPDATE;
    IF NOT FOUND OR g.version IS DISTINCT FROM p_expected_version THEN
        RETURN;  -- no rows: optimistic-concurrency conflict, exactly as the previous UPDATE reported
    END IF;
    IF jsonb_typeof(g.approvers) <> 'array' THEN
        RAISE EXCEPTION 'GATE_CHAIN_MALFORMED: stored approval chain of % is not an array', g.id USING ERRCODE = 'GT001';
    END IF;

    v_to := CASE p_op
        WHEN 'propose'  THEN 'PENDING_APPROVAL'
        WHEN 'approve'  THEN 'APPROVED'
        WHEN 'resume'   THEN 'APPROVED'
        WHEN 'activate' THEN 'ACTIVE'
        WHEN 'suspend'  THEN 'SUSPENDED'
        WHEN 'revoke'   THEN 'REVOKED'
        WHEN 'expire'   THEN 'EXPIRED'
    END;
    IF NOT cp_gate_can_transition(g.state, v_to) THEN
        RAISE EXCEPTION 'GATE_ILLEGAL_TRANSITION: % cannot move % -> % (%)', g.capability, g.state, v_to, p_op
            USING ERRCODE = 'GT002';
    END IF;
    -- approve and resume share a target state but not a source state.
    IF p_op = 'approve' AND g.state <> 'PENDING_APPROVAL' THEN
        RAISE EXCEPTION 'GATE_ILLEGAL_TRANSITION: approve expects PENDING_APPROVAL, found %', g.state USING ERRCODE = 'GT002';
    END IF;
    IF p_op = 'resume' AND g.state <> 'SUSPENDED' THEN
        RAISE EXCEPTION 'GATE_ILLEGAL_TRANSITION: resume expects SUSPENDED, found %', g.state USING ERRCODE = 'GT002';
    END IF;

    -- 2. Start from the stored row; only the columns this operation owns may move.
    v_approvers  := g.approvers;
    v_version    := g.approval_version;
    v_legal      := g.legal_review_ref;
    v_provider   := g.provider_contract_ref;
    v_risk       := g.risk_approval_ref;
    v_security   := g.security_approval_ref;
    v_hashes     := g.evidence_hashes;
    v_effective  := g.effective_at;
    v_expires    := g.expires_at;
    v_revoked    := g.revoked_at;
    v_revoke_rsn := g.revoke_reason;

    -- 3. The chain entry, when the operation appends one, is the acting principal's.
    IF p_op IN ('propose','approve','resume','activate') THEN
        IF p_approver IS NULL OR jsonb_typeof(p_approver) <> 'object' THEN
            RAISE EXCEPTION 'GATE_APPROVER_REQUIRED: % records a chain entry', p_op USING ERRCODE = 'GT001';
        END IF;
        IF p_approver->>'user_id' IS DISTINCT FROM p_actor_id THEN
            RAISE EXCEPTION 'GATE_APPROVER_MISMATCH: the chain entry must name the acting principal' USING ERRCODE = 'GT001';
        END IF;
        v_want_step := CASE p_op WHEN 'propose' THEN 'PROPOSE' WHEN 'approve' THEN 'APPROVE'
                                 WHEN 'resume'  THEN 'RESUME'  ELSE 'ACTIVATE' END;
        v_step := p_approver->>'step';
        IF v_step IS DISTINCT FROM v_want_step THEN
            RAISE EXCEPTION 'GATE_APPROVER_STEP_MISMATCH: % records step %, got %', p_op, v_want_step, coalesce(v_step, '<null>')
                USING ERRCODE = 'GT001';
        END IF;
    END IF;

    IF p_op = 'propose' THEN
        -- A new approval version: the chain restarts and the evidence is replaced wholesale.
        v_version    := g.approval_version + 1;
        v_approvers  := jsonb_build_array(p_approver);
        v_legal      := nullif(btrim(coalesce(p_legal_review_ref, '')), '');
        v_provider   := nullif(btrim(coalesce(p_provider_contract_ref, '')), '');
        v_risk       := nullif(btrim(coalesce(p_risk_approval_ref, '')), '');
        v_security   := nullif(btrim(coalesce(p_security_approval_ref, '')), '');
        v_hashes     := coalesce(p_evidence_hashes, '[]'::jsonb);
        v_effective  := p_effective_at;
        v_expires    := p_expires_at;
        v_revoked    := NULL;
        v_revoke_rsn := NULL;
        IF jsonb_typeof(v_hashes) <> 'array' THEN
            RAISE EXCEPTION 'GATE_EVIDENCE_HASHES_MALFORMED: evidence_hashes must be a JSON array' USING ERRCODE = 'GT001';
        END IF;
        IF cp_gate_is_high_risk(g.capability)
           AND (v_legal IS NULL OR v_provider IS NULL OR v_risk IS NULL OR v_security IS NULL) THEN
            RAISE EXCEPTION 'GATE_EVIDENCE_REQUIRED: high-risk capability % needs every evidence reference', g.capability
                USING ERRCODE = 'GT003';
        END IF;
        IF v_expires IS NOT NULL AND v_effective IS NOT NULL AND v_expires <= v_effective THEN
            RAISE EXCEPTION 'GATE_WINDOW_INVALID: expires_at must be after effective_at' USING ERRCODE = 'GT003';
        END IF;
    ELSIF p_op IN ('approve','resume','activate') THEN
        v_approvers := g.approvers || p_approver;   -- append to the stored chain, never to a supplied one
    END IF;

    -- The proposer of the current approval version, derived the way gates.scanGate derives it.
    v_proposer := coalesce(
        (SELECT e->>'user_id' FROM jsonb_array_elements(v_approvers) e
          WHERE e->>'step' = 'PROPOSE' AND coalesce(e->>'user_id', '') <> '' LIMIT 1),
        g.proposed_by_user_id, '');

    -- 4. Dual control (gates.approveRule / gates.activateRule).
    IF p_op IN ('approve','resume','activate') THEN
        IF v_proposer = '' THEN
            RAISE EXCEPTION 'GATE_NO_PROPOSER: approval version of % has no proposer; dual control impossible', g.capability
                USING ERRCODE = 'GT004';
        END IF;
        IF p_actor_id = v_proposer THEN
            RAISE EXCEPTION 'GATE_PROPOSER_CANNOT_APPROVE: the proposer of % may not approve or activate it', g.capability
                USING ERRCODE = 'GT004';
        END IF;
    END IF;
    IF p_op = 'activate' THEN
        SELECT e->>'user_id' INTO v_last
          FROM jsonb_array_elements(g.approvers) WITH ORDINALITY AS t(e, ord)
         WHERE e->>'step' <> 'PROPOSE' AND coalesce(e->>'user_id', '') <> ''
         ORDER BY ord DESC LIMIT 1;
        IF v_last IS NULL THEN
            RAISE EXCEPTION 'GATE_NO_APPROVER: approval version of % has no approver', g.capability USING ERRCODE = 'GT004';
        END IF;
        IF v_last = p_actor_id THEN
            RAISE EXCEPTION 'GATE_APPROVER_CANNOT_ACTIVATE: a principal distinct from the approver must activate %', g.capability
                USING ERRCODE = 'GT004';
        END IF;
    END IF;

    -- 5. The five conditions of POLICY_AUTHORITY §1, re-derived here for the row this call would
    --    write. Condition 1 (deployment configuration) is not knowable in the database and stays
    --    where it is, in Checker; conditions 2-5 are enforced below and re-checked by Evaluate on
    --    every read, so a row that reaches ACTIVE always satisfies them.
    IF p_op = 'activate' THEN
        v_effective := coalesce(g.effective_at, p_occurred_at);
        IF v_revoked IS NOT NULL THEN
            RAISE EXCEPTION 'GATE_REVOKED: % is revoked and cannot be activated', g.capability USING ERRCODE = 'GT003';
        END IF;
        IF v_expires IS NOT NULL AND v_expires <= p_occurred_at THEN
            RAISE EXCEPTION 'GATE_WINDOW_EXPIRED: approval version of % has expired; re-propose', g.capability
                USING ERRCODE = 'GT003';
        END IF;
        IF cp_gate_is_high_risk(g.capability)
           AND (coalesce(btrim(v_legal), '') = '' OR coalesce(btrim(v_provider), '') = ''
                OR coalesce(btrim(v_risk), '') = '' OR coalesce(btrim(v_security), '') = '') THEN
            RAISE EXCEPTION 'GATE_EVIDENCE_REQUIRED: high-risk capability % needs every evidence reference', g.capability
                USING ERRCODE = 'GT003';
        END IF;
        SELECT count(DISTINCT e->>'user_id') INTO v_approvals
          FROM jsonb_array_elements(v_approvers) e
         WHERE e->>'step' <> 'PROPOSE' AND coalesce(e->>'user_id', '') <> '';
        IF v_approvals < 2 THEN
            RAISE EXCEPTION 'GATE_INSUFFICIENT_APPROVERS: % has % distinct approvers, two are required', g.capability, v_approvals
                USING ERRCODE = 'GT004';
        END IF;
        IF EXISTS (SELECT 1 FROM jsonb_array_elements(v_approvers) e
                    WHERE e->>'step' <> 'PROPOSE' AND e->>'user_id' = v_proposer) THEN
            RAISE EXCEPTION 'GATE_PROPOSER_APPROVED: an approver of % is its proposer', g.capability USING ERRCODE = 'GT004';
        END IF;
    ELSIF p_op = 'revoke' THEN
        v_revoked    := p_occurred_at;
        v_revoke_rsn := p_reason;
    END IF;

    -- 6. History and state in one statement. The transition row also satisfies 00603's deferred
    --    AU001 binding, so the two can never come apart.
    INSERT INTO capability_gate_transitions
        (id, gate_id, from_state, to_state, actor_type, actor_id, reason, approval_id, evidence_hash, occurred_at)
    VALUES (p_transition_id, p_gate_id, g.state, v_to, p_actor_type, p_actor_id, p_reason, NULL, p_evidence_hash, p_occurred_at);

    UPDATE capability_gates SET
        state                 = v_to,
        approval_version      = v_version,
        legal_review_ref      = v_legal,
        provider_contract_ref = v_provider,
        risk_approval_ref     = v_risk,
        security_approval_ref = v_security,
        proposed_by_user_id   = nullif(v_proposer, ''),
        approvers             = v_approvers,
        evidence_hashes       = v_hashes,
        effective_at          = v_effective,
        expires_at            = v_expires,
        revoked_at            = v_revoked,
        revoke_reason         = v_revoke_rsn,
        version               = version + 1
    WHERE id = p_gate_id AND version = p_expected_version
    RETURNING * INTO out_row;

    RETURN NEXT out_row;
END;
$$;
-- +goose StatementEnd

-- Least privilege. cp_app keeps SELECT (Checker reads on every live-money call) and INSERT (a gate
-- row is born DISABLED, see the trigger above); it loses every other way to touch these two tables.
--
-- The one column-level UPDATE grant is `version`, and it exists only so the application can still
-- take a row lock: PostgreSQL requires the UPDATE privilege for SELECT ... FOR UPDATE (and for FOR
-- SHARE / FOR KEY SHARE), which internal/gates uses to serialise a gate's read-modify-write.
-- `version` is the optimistic-concurrency counter; it appears in none of the five conditions, so
-- writing it can do nothing but make the writer's own next save conflict. `state`, the approval
-- chain, the evidence references and the validity window are all out of reach.
REVOKE UPDATE ON capability_gates FROM cp_app;
GRANT UPDATE (version) ON capability_gates TO cp_app;
REVOKE INSERT ON capability_gate_transitions FROM cp_app;

REVOKE ALL ON FUNCTION cp_gate_transition(uuid, bigint, text, text, text, text, uuid, bytea, timestamptz,
    jsonb, text, text, text, text, jsonb, timestamptz, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION cp_gate_transition(uuid, bigint, text, text, text, text, uuid, bytea, timestamptz,
    jsonb, text, text, text, text, jsonb, timestamptz, timestamptz) TO cp_app;

-- +goose Down
SELECT 1; -- protected: the activation authority of a live-money capability is never rolled back
