-- +goose Up
-- A sandbox row carries no approval and no revoke.
--
-- Migration 00755 gave a gate the SANDBOX state and said three things about it:
-- it can only exist where nothing real can move, it is entered by one operator
-- through a function that "never touches the approval chain, the evidence
-- references or the validity window", and it is active only for a deployment
-- that declared itself a sandbox tier. ADR-0023, the package doc, the admin API
-- and the operator console all repeat the middle one.
--
-- It was true of exactly one of the three documented sources. A gate sandboxed
-- from a freshly bootstrapped DISABLED row has no approval to carry. A gate
-- sandboxed from EXPIRED or REVOKED -- both named by 00755's own header and by
-- cp_gate_can_transition -- kept the entire approval version that reached
-- ACTIVE: the chain of three principals, the proposer, the approval version and
-- all four evidence references. `GET /v1/admin/gates` returned every one of them
-- beside `state: SANDBOX, active: true`, under a console banner that says a
-- sandbox gate records none of it (F-161).
--
-- The same UPDATE left `revoked_at` behind. REVOKED is a documented source of
-- SANDBOX and the transition reported success, but `gates.evaluateSandbox`
-- refused a row with a revoke recorded before it looked at anything else -- so
-- every gate sandbox-activated out of REVOKED was permanently inert, including
-- through the boot-time path that logs "capability sandbox-activated at boot".
-- Nothing said so, and the only operation that clears `revoked_at` is `propose`,
-- which starts the dual-control ceremony the sandbox tier exists so a rehearsal
-- need not fabricate (F-160).
--
-- WHAT CHANGES. cp_gate_sandbox writes a blank row: the approval version goes to
-- zero, the chain to '[]', the evidence hashes to '[]', the proposer and the
-- four evidence references to NULL, and the revoke to NULL -- in the same
-- statement as the state change, so the row a SANDBOX state describes is the row
-- the documents describe. "Never touches the approval chain" becomes "clears the
-- approval chain", which is the property the readers above were promised: a
-- SANDBOX row cannot be mistaken for an approval because there is nothing on it
-- to mistake.
--
-- The approval it clears is not lost. capability_gate_transitions holds every
-- transition of the version, with the evidence digest each principal attested,
-- and the audit stream holds the events; the row carries the CURRENT approval
-- version, and a sandbox row's current approval version is none. `propose` has
-- always replaced the chain wholesale for the same reason.
--
-- WHAT DOES NOT CHANGE. The legal transitions, the PROD refusal, the separation
-- from cp_gate_transition, the privileges, and the fact that SANDBOX is not on
-- the path to ACTIVE. A real ceremony still starts from DISABLED and still needs
-- three principals and four references.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_gate_sandbox(
    p_gate_id          uuid,
    p_expected_version bigint,
    p_op               text,
    p_actor_type       text,
    p_actor_id         text,
    p_reason           text,
    p_transition_id    uuid,
    p_evidence_hash    bytea,
    p_occurred_at      timestamptz
) RETURNS SETOF capability_gates
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    g       capability_gates%ROWTYPE;
    v_to    text;
    out_row capability_gates%ROWTYPE;
BEGIN
    IF p_op IS NULL OR p_op NOT IN ('sandbox', 'unsandbox') THEN
        RAISE EXCEPTION 'GATE_UNKNOWN_OPERATION: %', coalesce(p_op, '<null>') USING ERRCODE = 'GT001';
    END IF;
    IF p_actor_id IS NULL OR btrim(p_actor_id) = '' THEN
        RAISE EXCEPTION 'GATE_ACTOR_REQUIRED: every gate transition names the principal that made it' USING ERRCODE = 'GT001';
    END IF;
    IF p_actor_type IS NULL OR p_actor_type NOT IN ('USER', 'OPERATOR', 'SYSTEM') THEN
        RAISE EXCEPTION 'GATE_ACTOR_TYPE_FORBIDDEN: % may not change a capability gate', coalesce(p_actor_type, '<null>') USING ERRCODE = 'GT001';
    END IF;
    IF p_reason IS NULL OR btrim(p_reason) = '' THEN
        RAISE EXCEPTION 'GATE_REASON_REQUIRED: every gate transition carries a reason' USING ERRCODE = 'GT001';
    END IF;
    IF p_transition_id IS NULL OR p_occurred_at IS NULL THEN
        RAISE EXCEPTION 'GATE_TRANSITION_INCOMPLETE: transition id and time are required' USING ERRCODE = 'GT001';
    END IF;

    SELECT * INTO g FROM capability_gates WHERE id = p_gate_id FOR UPDATE;
    IF NOT FOUND OR g.version <> p_expected_version THEN
        RETURN;
    END IF;

    -- Property 1, again, before the CHECK gets a chance: the refusal names
    -- the reason rather than surfacing as a constraint violation.
    IF g.environment = 'PROD' THEN
        RAISE EXCEPTION 'GATE_SANDBOX_NEVER_IN_PROD: %/% cannot be a sandbox gate; production capabilities are activated by dual control or not at all',
            g.capability, g.environment USING ERRCODE = 'GT003';
    END IF;

    v_to := CASE WHEN p_op = 'sandbox' THEN 'SANDBOX' ELSE 'DISABLED' END;
    IF NOT cp_gate_can_transition(g.state, v_to) THEN
        RAISE EXCEPTION 'GATE_ILLEGAL_TRANSITION: % cannot move % -> % (%)', g.capability, g.state, v_to, p_op
            USING ERRCODE = 'GT002';
    END IF;

    -- History and state in one statement, so the edge flag and the deferred
    -- binding (00731) see exactly the change that happened.
    INSERT INTO capability_gate_transitions
        (id, gate_id, from_state, to_state, actor_type, actor_id, reason, approval_id, evidence_hash, occurred_at)
    VALUES (p_transition_id, p_gate_id, g.state, v_to, p_actor_type, p_actor_id, p_reason, NULL, p_evidence_hash, p_occurred_at);

    -- The blank row. Both operations write it: SANDBOX carries no approval, and
    -- the DISABLED row an unsandbox leaves behind is the one a real ceremony
    -- starts from, which proposes its own evidence from scratch.
    UPDATE capability_gates SET
        state                 = v_to,
        approval_version      = 0,
        legal_review_ref      = NULL,
        provider_contract_ref = NULL,
        risk_approval_ref     = NULL,
        security_approval_ref = NULL,
        proposed_by_user_id   = NULL,
        approvers             = '[]'::jsonb,
        evidence_hashes       = '[]'::jsonb,
        effective_at          = CASE WHEN v_to = 'SANDBOX' THEN p_occurred_at ELSE NULL END,
        expires_at            = NULL,
        revoked_at            = NULL,
        revoke_reason         = NULL,
        version               = version + 1
    WHERE id = p_gate_id AND version = p_expected_version
    RETURNING * INTO out_row;

    RETURN NEXT out_row;
END;
$$;
-- +goose StatementEnd

-- The rows the old function already wrote. A deployment that sandbox-activated
-- a capability out of EXPIRED or REVOKED is holding exactly the row this
-- migration exists to stop producing: an approval version under a SANDBOX state,
-- or a revoke that makes the state inert. Leaving them would mean the property
-- holds for rows written after today and not for the STAGING tier that found it.
--
-- No state moves, so no transition row is required and none is written: the
-- transitions that produced these rows are already recorded, and this changes
-- only the columns that should never have survived them.
UPDATE capability_gates SET
    approval_version      = 0,
    legal_review_ref      = NULL,
    provider_contract_ref = NULL,
    risk_approval_ref     = NULL,
    security_approval_ref = NULL,
    proposed_by_user_id   = NULL,
    approvers             = '[]'::jsonb,
    evidence_hashes       = '[]'::jsonb,
    revoked_at            = NULL,
    revoke_reason         = NULL
WHERE state = 'SANDBOX'
  AND (approval_version <> 0
       OR approvers <> '[]'::jsonb
       OR evidence_hashes <> '[]'::jsonb
       OR proposed_by_user_id IS NOT NULL
       OR legal_review_ref IS NOT NULL
       OR provider_contract_ref IS NOT NULL
       OR risk_approval_ref IS NOT NULL
       OR security_approval_ref IS NOT NULL
       OR revoked_at IS NOT NULL
       OR revoke_reason IS NOT NULL);

-- +goose Down
SELECT 1; -- protected: reverting restores a cp_gate_sandbox that leaves a real approval version under a SANDBOX state, and the approval chains this migration cleared are not recoverable from a Down
