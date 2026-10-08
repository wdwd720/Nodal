-- +goose Up
-- Transfer reversals under dual control (internal/transfers; amends 00744 without editing it). Review finding
-- F3: one operator holding ledger:post_correction could reverse a completed transfer alone.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A reversal starts as a proposal: one OPERATOR names a COMPLETED transfer, a stable reason code and a
--      reason. Proposals of one transfer are numbered 1, 2, 3 ... and a new one may be made only when every
--      earlier one has been decided, so at most one proposal per transfer is ever open; two concurrent
--      proposals collide on UNIQUE (transfer_id, seq) whatever the isolation level. Proposals are append-only.
--   2. A proposal is decided exactly once (UNIQUE proposal_id), APPROVED or REJECTED, by an OPERATOR; a
--      rejection states its reason. The operator who proposed a reversal can never approve it. Decisions are
--      append-only.
--   3. An approval IS the reversal: at COMMIT, an APPROVED decision's transfer must be REVERSED by that very
--      proposal, by the approving operator, in the same transaction. An approved proposal is therefore
--      consumed, and a rejected one can never execute.
--   4. A transfer moves COMPLETED -> REVERSED only when it names (reversal_proposal_id) an APPROVED proposal
--      of that transfer with the same reason code, approved by an OPERATOR other than the proposer, and the
--      operator recorded as reversing it is that approver. A proposal licenses at most one reversal (UNIQUE).
--      The new column is frozen like the other reversal columns: written once, by COMPLETED -> REVERSED.
--
-- Reversals made before this migration (by one operator, under 00744) keep reversal_proposal_id NULL: the
-- CHECK below is added NOT VALID, so history stays exactly as it was, and every row written from now on is
-- held to it.
--
-- Custom SQLSTATEs: PT006 an approval decided by the operator who proposed the reversal; PT007 a reversal
-- that is not bound to an APPROVED proposal of that transfer by two distinct operators (another transfer's
-- proposal, a different reason code, an undecided or rejected proposal, an approver who is not the operator
-- recorded as reversing it), or an APPROVED decision whose reversal did not happen in its transaction;
-- PT008 a proposal for a transfer that is not COMPLETED, made while another proposal of the same transfer
-- is undecided, or out of sequence.

-- ---------------------------------------------------------------------------
-- 1. Proposals
-- ---------------------------------------------------------------------------

CREATE TABLE transfer_reversal_proposals (
    id                      uuid PRIMARY KEY,
    transfer_id             uuid NOT NULL REFERENCES internal_transfers(id),
    -- 1 for the first proposal of a transfer, then 2, 3 ...: a new one only once every earlier one is decided.
    seq                     integer NOT NULL CHECK (seq >= 1),
    reason_code             text NOT NULL CHECK (reason_code ~ '^[A-Z][A-Z0-9_]{2,63}$'),
    reason                  text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    proposed_by_actor_type  text NOT NULL CHECK (proposed_by_actor_type = 'OPERATOR'),
    proposed_by_actor_id    text NOT NULL CHECK (length(proposed_by_actor_id) BETWEEN 1 AND 200),
    correlation_id          text,
    proposed_at             timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (transfer_id, seq)
);
CREATE INDEX transfer_reversal_proposals_listing_idx ON transfer_reversal_proposals (proposed_at DESC, id DESC);
CREATE TRIGGER transfer_reversal_proposals_immutable BEFORE UPDATE OR DELETE ON transfer_reversal_proposals
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_transfer_reversal_proposal_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    transfer_status text;
    proposals       integer;
    undecided       integer;
BEGIN
    SELECT status INTO transfer_status FROM internal_transfers WHERE id = NEW.transfer_id;
    IF transfer_status IS DISTINCT FROM 'COMPLETED' THEN
        RAISE EXCEPTION 'TRANSFER_REVERSAL_PROPOSAL: transfer % is %; only a COMPLETED transfer is proposed for reversal',
            NEW.transfer_id, coalesce(transfer_status, 'missing') USING ERRCODE = 'PT008';
    END IF;
    SELECT count(*), count(*) FILTER (WHERE d.id IS NULL) INTO proposals, undecided
      FROM transfer_reversal_proposals p LEFT JOIN transfer_reversal_decisions d ON d.proposal_id = p.id
     WHERE p.transfer_id = NEW.transfer_id;
    IF undecided > 0 THEN
        RAISE EXCEPTION 'TRANSFER_REVERSAL_PROPOSAL: a reversal of transfer % is already proposed and undecided', NEW.transfer_id
            USING ERRCODE = 'PT008';
    END IF;
    IF NEW.seq <> proposals + 1 THEN
        RAISE EXCEPTION 'TRANSFER_REVERSAL_PROPOSAL: transfer % has % proposals; the next is number %, not %',
            NEW.transfer_id, proposals, proposals + 1, NEW.seq USING ERRCODE = 'PT008';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_transfer_reversal_proposal_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER transfer_reversal_proposals_guard BEFORE INSERT ON transfer_reversal_proposals
    FOR EACH ROW EXECUTE FUNCTION cp_transfer_reversal_proposal_guard();

-- ---------------------------------------------------------------------------
-- 2. Decisions
-- ---------------------------------------------------------------------------

CREATE TABLE transfer_reversal_decisions (
    id                     uuid PRIMARY KEY,
    -- One decision per proposal, for good.
    proposal_id            uuid NOT NULL UNIQUE REFERENCES transfer_reversal_proposals(id),
    decision               text NOT NULL CHECK (decision IN ('APPROVED','REJECTED')),
    reason                 text CHECK (reason IS NULL OR length(reason) BETWEEN 1 AND 500),
    decided_by_actor_type  text NOT NULL CHECK (decided_by_actor_type = 'OPERATOR'),
    decided_by_actor_id    text NOT NULL CHECK (length(decided_by_actor_id) BETWEEN 1 AND 200),
    correlation_id         text,
    decided_at             timestamptz NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT transfer_reversal_decisions_rejection_has_reason CHECK (decision <> 'REJECTED' OR reason IS NOT NULL)
);
CREATE TRIGGER transfer_reversal_decisions_immutable BEFORE UPDATE OR DELETE ON transfer_reversal_decisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- The proposal must exist (the foreign key says so too); approving one's own proposal is refused outright.
-- +goose StatementBegin
CREATE FUNCTION cp_transfer_reversal_decision_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    p transfer_reversal_proposals%ROWTYPE;
BEGIN
    SELECT * INTO p FROM transfer_reversal_proposals WHERE id = NEW.proposal_id;
    IF p.id IS NOT NULL AND NEW.decision = 'APPROVED' AND NEW.decided_by_actor_id = p.proposed_by_actor_id THEN
        RAISE EXCEPTION 'TRANSFER_REVERSAL_SELF_APPROVAL: reversal proposal % was proposed by % and cannot be approved by them',
            p.id, p.proposed_by_actor_id USING ERRCODE = 'PT006';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_transfer_reversal_decision_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER transfer_reversal_decisions_guard BEFORE INSERT ON transfer_reversal_decisions
    FOR EACH ROW EXECUTE FUNCTION cp_transfer_reversal_decision_guard();

-- An approval is the reversal: checked at COMMIT, so the order of the statements inside the transaction does
-- not matter, only that the transaction that approves also reverses.
-- +goose StatementBegin
CREATE FUNCTION cp_transfer_reversal_approval_executed() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    p transfer_reversal_proposals%ROWTYPE;
    t internal_transfers%ROWTYPE;
BEGIN
    SELECT * INTO p FROM transfer_reversal_proposals WHERE id = NEW.proposal_id;
    SELECT * INTO t FROM internal_transfers WHERE id = p.transfer_id;
    IF t.id IS NULL OR t.status IS DISTINCT FROM 'REVERSED' OR t.reversal_proposal_id IS DISTINCT FROM NEW.proposal_id
       OR t.reversed_by_actor_id IS DISTINCT FROM NEW.decided_by_actor_id THEN
        RAISE EXCEPTION 'TRANSFER_REVERSAL_NOT_EXECUTED: approval of reversal proposal % did not reverse transfer % in its transaction',
            NEW.proposal_id, p.transfer_id USING ERRCODE = 'PT007';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_transfer_reversal_approval_executed() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER transfer_reversal_decisions_approval_executed AFTER INSERT ON transfer_reversal_decisions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (NEW.decision = 'APPROVED')
    EXECUTE FUNCTION cp_transfer_reversal_approval_executed();

-- ---------------------------------------------------------------------------
-- 3. The reversal names its approved proposal
-- ---------------------------------------------------------------------------

ALTER TABLE internal_transfers ADD COLUMN reversal_proposal_id uuid UNIQUE REFERENCES transfer_reversal_proposals(id);
-- Every reversal from now on names its proposal. NOT VALID: the reversals 00744 allowed one operator to make
-- keep NULL, and no legal change can ever touch a REVERSED row again.
ALTER TABLE internal_transfers ADD CONSTRAINT internal_transfers_reversal_proposed
    CHECK (status <> 'REVERSED' OR reversal_proposal_id IS NOT NULL) NOT VALID;

-- 00744's birth rule, verbatim, plus the new column: a transfer is born without a reversal proposal.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_internal_transfer_born_pending() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM 'PENDING'
       OR NEW.rejection_code IS NOT NULL OR cardinality(NEW.reason_codes) <> 0 OR NEW.risk_decision_id IS NOT NULL
       OR NEW.effect_id IS NOT NULL OR NEW.journal_transaction_id IS NOT NULL OR NEW.reservation_id IS NOT NULL
       OR NEW.evidence_ref IS NOT NULL OR NEW.evidence_sha256 IS NOT NULL OR NEW.decided_at IS NOT NULL
       OR NEW.reversal_journal_transaction_id IS NOT NULL OR NEW.reversal_reason_code IS NOT NULL
       OR NEW.reversed_by_actor_id IS NOT NULL OR NEW.reversal_evidence_ref IS NOT NULL OR NEW.reversed_at IS NOT NULL
       OR NEW.reversal_proposal_id IS NOT NULL THEN
        RAISE EXCEPTION 'TRANSFER_NOT_BORN_PENDING: transfer % must be created PENDING with no outcome', NEW.id
            USING ERRCODE = 'PT003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- CREATE OR REPLACE resets a function's settings; pin the search path again, as 00744 did.
ALTER FUNCTION cp_internal_transfer_born_pending() SET search_path = pg_catalog, public, pg_temp;

-- 00744's guard, verbatim, plus the new column in the reversal tuple: written once, by COMPLETED -> REVERSED.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_internal_transfer_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.intent_id, NEW.sender_account_id, NEW.sender_user_id, NEW.recipient_account_id, NEW.recipient_handle,
        NEW.asset_id, NEW.quantity, NEW.notional_usd_minor, NEW.mode, NEW.memo, NEW.idempotency_key, NEW.request_hash,
        NEW.policy_version, NEW.policy_hash, NEW.correlation_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.intent_id, OLD.sender_account_id, OLD.sender_user_id, OLD.recipient_account_id, OLD.recipient_handle,
        OLD.asset_id, OLD.quantity, OLD.notional_usd_minor, OLD.mode, OLD.memo, OLD.idempotency_key, OLD.request_hash,
        OLD.policy_version, OLD.policy_hash, OLD.correlation_id, OLD.created_at) THEN
        RAISE EXCEPTION 'TRANSFER_IDENTITY_FROZEN: transfer % may change status only', OLD.id USING ERRCODE = 'PT002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_internal_transfer_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'TRANSFER_TRANSITION_ILLEGAL: transfer % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'PT004';
    END IF;
    -- The decision is written by the one update that leaves PENDING, and never again.
    IF NOT (OLD.status = 'PENDING' AND NEW.status IS DISTINCT FROM 'PENDING')
       AND (NEW.rejection_code, NEW.reason_codes, NEW.risk_decision_id, NEW.effect_id, NEW.journal_transaction_id,
            NEW.reservation_id, NEW.evidence_ref, NEW.evidence_sha256, NEW.decided_at)
           IS DISTINCT FROM
           (OLD.rejection_code, OLD.reason_codes, OLD.risk_decision_id, OLD.effect_id, OLD.journal_transaction_id,
            OLD.reservation_id, OLD.evidence_ref, OLD.evidence_sha256, OLD.decided_at) THEN
        RAISE EXCEPTION 'TRANSFER_DECISION_FROZEN: the decision of transfer % is written once, when it leaves PENDING', OLD.id
            USING ERRCODE = 'PT002';
    END IF;
    -- The reversal is written by COMPLETED -> REVERSED, and never again.
    IF NOT (OLD.status = 'COMPLETED' AND NEW.status = 'REVERSED')
       AND (NEW.reversal_journal_transaction_id, NEW.reversal_reason_code, NEW.reversed_by_actor_id,
            NEW.reversal_evidence_ref, NEW.reversed_at, NEW.reversal_proposal_id)
           IS DISTINCT FROM
           (OLD.reversal_journal_transaction_id, OLD.reversal_reason_code, OLD.reversed_by_actor_id,
            OLD.reversal_evidence_ref, OLD.reversed_at, OLD.reversal_proposal_id) THEN
        RAISE EXCEPTION 'TRANSFER_REVERSAL_FROZEN: the reversal of transfer % is written once, by COMPLETED -> REVERSED', OLD.id
            USING ERRCODE = 'PT002';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_internal_transfer_guard() SET search_path = pg_catalog, public, pg_temp;

-- The reversal is bound to an approval by a second operator. Deferred to COMMIT like 00744's outcome check:
-- the proposal, the decision and the transfer are written by different statements of one transaction.
-- Fires only on the change INTO REVERSED, so a reversal made before this migration is never re-judged.
-- +goose StatementBegin
CREATE FUNCTION cp_internal_transfer_reversal_approved() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    p transfer_reversal_proposals%ROWTYPE;
    d transfer_reversal_decisions%ROWTYPE;
BEGIN
    SELECT * INTO p FROM transfer_reversal_proposals WHERE id = NEW.reversal_proposal_id;
    SELECT * INTO d FROM transfer_reversal_decisions WHERE proposal_id = NEW.reversal_proposal_id;
    IF NEW.reversal_proposal_id IS NULL OR p.id IS NULL OR p.transfer_id IS DISTINCT FROM NEW.id
       OR p.reason_code IS DISTINCT FROM NEW.reversal_reason_code
       OR p.proposed_by_actor_type IS DISTINCT FROM 'OPERATOR'
       OR d.id IS NULL OR d.decision IS DISTINCT FROM 'APPROVED'
       OR d.decided_by_actor_type IS DISTINCT FROM 'OPERATOR'
       OR d.decided_by_actor_id IS NOT DISTINCT FROM p.proposed_by_actor_id
       OR NEW.reversed_by_actor_id IS DISTINCT FROM d.decided_by_actor_id THEN
        RAISE EXCEPTION 'TRANSFER_REVERSAL_NOT_DUAL_CONTROLLED: transfer % was reversed without an approval of proposal % by a second operator',
            NEW.id, NEW.reversal_proposal_id USING ERRCODE = 'PT007';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_internal_transfer_reversal_approved() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER internal_transfers_reversal_dual_control AFTER UPDATE OF status ON internal_transfers
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (NEW.status = 'REVERSED' AND OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION cp_internal_transfer_reversal_approved();

-- ---------------------------------------------------------------------------
-- 4. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON transfer_reversal_proposals, transfer_reversal_decisions TO cp_app;
GRANT UPDATE (reversal_proposal_id) ON internal_transfers TO cp_app;
GRANT SELECT ON transfer_reversal_proposals, transfer_reversal_decisions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: reversal proposals and their decisions are financial history and are never dropped
