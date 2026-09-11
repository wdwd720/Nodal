-- +goose Up
-- A sandbox tier can exercise a gate without approving it -- and PROD cannot.
--
-- Every capability gate reaches ACTIVE by one path: a proposal, an approval by a
-- distinct principal, an activation by a third, four evidence references for a
-- high-risk capability, and a step-up inside fifteen minutes for each. That is
-- what stands between this system and real money, and nothing here changes it.
--
-- What it cannot do is let a STAGING deployment -- sandbox Stripe, devnet mint,
-- no live provider anywhere, refused at startup if one appears -- exercise the
-- product it exists to rehearse. Buying Credits, trading a native asset and
-- requesting a payout were each refused by the gate before the domain code ran,
-- and the only way past was the ceremony above with fabricated references,
-- which is forbidden and would defeat the control it imitates.
--
-- So a gate gains one more state, SANDBOX, with three properties:
--
--   1. It can only exist where nothing real can move. A table CHECK refuses a
--      SANDBOX row whose environment is PROD, and the function below refuses
--      to write one there, so no combination of credentials produces it.
--   2. It is entered by a single operator, with a reason, and is written by a
--      SECURITY DEFINER function with its own history row -- the same shape as
--      every other transition, minus the approval chain it deliberately does
--      not have. The approval chain, the evidence references and the validity
--      window are untouched: a sandbox gate carries no approval, and cannot
--      be mistaken for one.
--   3. The application reads it as active ONLY when the deployment declared
--      itself a sandbox tier (CP_API_LEGAL_POLICY=SANDBOX, refused in PROD by
--      config.Validate). In every other deployment a SANDBOX row is inactive
--      with a reason that says so.
--
-- The legal transitions: DISABLED, REVOKED or EXPIRED may enter SANDBOX; a
-- SANDBOX gate may be disabled or revoked. It may NOT be approved or activated
-- from SANDBOX: the real ceremony starts from DISABLED, as it always has.

ALTER TABLE capability_gates DROP CONSTRAINT capability_gates_state_check;
ALTER TABLE capability_gates ADD CONSTRAINT capability_gates_state_check CHECK (state IN (
    'DISABLED','PENDING_APPROVAL','APPROVED','ACTIVE','SUSPENDED','REVOKED','EXPIRED','SANDBOX'));

-- Property 1, as a fact about the table rather than about a function.
ALTER TABLE capability_gates ADD CONSTRAINT capability_gates_sandbox_never_in_prod
    CHECK (state <> 'SANDBOX' OR environment <> 'PROD');

-- Mirrors gates.CanTransition, with the five sandbox edges added.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_gate_can_transition(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('DISABLED','PENDING_APPROVAL'), ('DISABLED','REVOKED'), ('DISABLED','SANDBOX'),
        ('PENDING_APPROVAL','APPROVED'), ('PENDING_APPROVAL','REVOKED'),
        ('APPROVED','ACTIVE'), ('APPROVED','EXPIRED'), ('APPROVED','REVOKED'),
        ('ACTIVE','SUSPENDED'), ('ACTIVE','EXPIRED'), ('ACTIVE','REVOKED'),
        ('SUSPENDED','APPROVED'), ('SUSPENDED','REVOKED'),
        ('REVOKED','PENDING_APPROVAL'), ('REVOKED','SANDBOX'),
        ('EXPIRED','PENDING_APPROVAL'), ('EXPIRED','REVOKED'), ('EXPIRED','SANDBOX'),
        ('SANDBOX','DISABLED'), ('SANDBOX','REVOKED'));
$$;
-- +goose StatementEnd

-- cp_gate_sandbox is the only way a gate enters or leaves SANDBOX. It is a
-- sibling of cp_gate_transition rather than an operation inside it: the two
-- ceremonies share nothing but the history table, and keeping them apart is
-- what makes "a SANDBOX row carries no approval chain" true by construction --
-- this function never touches the chain, the evidence references or the
-- validity window.
--
-- Returns the resulting row, or no rows when the gate is missing or
-- p_expected_version does not match (the caller maps that to CONFLICT).
-- +goose StatementBegin
CREATE FUNCTION cp_gate_sandbox(
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

    UPDATE capability_gates SET
        state        = v_to,
        effective_at = CASE WHEN v_to = 'SANDBOX' THEN p_occurred_at ELSE NULL END,
        expires_at   = NULL,
        version      = version + 1
    WHERE id = p_gate_id AND version = p_expected_version
    RETURNING * INTO out_row;

    RETURN NEXT out_row;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION cp_gate_sandbox(uuid, bigint, text, text, text, text, uuid, bytea, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION cp_gate_sandbox(uuid, bigint, text, text, text, text, uuid, bytea, timestamptz) TO cp_app;

-- +goose Down
SELECT 1; -- protected: reverting would strand every SANDBOX row outside its CHECK; a sandbox gate is disabled through cp_gate_sandbox, not by a Down
