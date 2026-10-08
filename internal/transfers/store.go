package transfers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

const transferColumns = `id, intent_id, sender_account_id, sender_user_id, recipient_account_id, coalesce(recipient_handle,''),
	asset_id, quantity::text, notional_usd_minor, mode, coalesce(memo,''), idempotency_key, request_hash, policy_version,
	policy_hash, status, coalesce(rejection_code,''), reason_codes, risk_decision_id, effect_id, journal_transaction_id,
	coalesce(reservation_id::text,''), coalesce(evidence_ref,''), evidence_sha256, decided_at,
	reversal_journal_transaction_id, coalesce(reversal_reason_code,''), coalesce(reversed_by_actor_id,''),
	coalesce(reversal_evidence_ref,''), reversed_at, reversal_proposal_id, coalesce(correlation_id,''), created_at, updated_at`

func scanTransfer(row pgx.Row) (Transfer, error) {
	var t Transfer
	var qty string
	var notional int64
	if err := row.Scan(&t.ID, &t.IntentID, &t.SenderAccountID, &t.SenderUserID, &t.RecipientAccountID, &t.RecipientHandle,
		&t.AssetID, &qty, &notional, &t.Mode, &t.Memo, &t.IdempotencyKey, &t.RequestHash, &t.PolicyVersion,
		&t.PolicyHash, &t.Status, &t.RejectionCode, &t.ReasonCodes, &t.RiskDecisionID, &t.EffectID, &t.JournalTransactionID,
		&t.ReservationID, &t.EvidenceRef, &t.EvidenceSHA256, &t.DecidedAt,
		&t.ReversalJournalTransactionID, &t.ReversalReasonCode, &t.ReversedByActorID,
		&t.ReversalEvidenceRef, &t.ReversedAt, &t.ReversalProposalID, &t.CorrelationID, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return Transfer{}, err
	}
	q, err := money.ScanQuantity(qty)
	if err != nil {
		return Transfer{}, err
	}
	t.Quantity = q
	t.NotionalUSD = money.USDFromMinor(notional)
	if t.ReasonCodes == nil {
		t.ReasonCodes = []string{}
	}
	t.CreatedAt = t.CreatedAt.UTC()
	t.UpdatedAt = t.UpdatedAt.UTC()
	if t.DecidedAt != nil {
		u := t.DecidedAt.UTC()
		t.DecidedAt = &u
	}
	if t.ReversedAt != nil {
		u := t.ReversedAt.UTC()
		t.ReversedAt = &u
	}
	return t, nil
}

func insertTransfer(ctx context.Context, tx pgx.Tx, t Transfer) (Transfer, error) {
	out, err := scanTransfer(tx.QueryRow(ctx, `INSERT INTO internal_transfers
		(id, intent_id, sender_account_id, sender_user_id, recipient_account_id, recipient_handle, asset_id, quantity,
		 notional_usd_minor, mode, memo, idempotency_key, request_hash, policy_version, policy_hash, status,
		 correlation_id, created_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8::numeric,$9,$10,NULLIF($11,''),$12,$13,$14,$15,'PENDING',NULLIF($16,''),$17)
		RETURNING `+transferColumns,
		t.ID, t.IntentID, t.SenderAccountID, t.SenderUserID, t.RecipientAccountID, t.RecipientHandle, t.AssetID,
		t.Quantity.String(), t.NotionalUSD.Minor(), t.Mode, t.Memo, t.IdempotencyKey, t.RequestHash, t.PolicyVersion,
		t.PolicyHash, t.CorrelationID, t.CreatedAt))
	if err != nil {
		return Transfer{}, dbErr("insert transfer", err)
	}
	return out, nil
}

func getTransfer(ctx context.Context, q db.Querier, tid TransferID, forUpdate bool) (Transfer, error) {
	sql := `SELECT ` + transferColumns + ` FROM internal_transfers WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	t, err := scanTransfer(q.QueryRow(ctx, sql, tid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, errs.New(errs.CodeNotFound, "transfer not found").WithField("transfer_id", tid.String())
	}
	if err != nil {
		return Transfer{}, dbErr("load transfer", err)
	}
	return t, nil
}

func transferByKey(ctx context.Context, q db.Querier, sender accounts.AccountID, key string) (Transfer, bool, error) {
	t, err := scanTransfer(q.QueryRow(ctx, `SELECT `+transferColumns+` FROM internal_transfers
		WHERE sender_account_id = $1 AND idempotency_key = $2`, sender, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, false, nil
	}
	if err != nil {
		return Transfer{}, false, dbErr("load transfer by key", err)
	}
	return t, true, nil
}

// outcome is what the one update that leaves PENDING writes.
type outcome struct {
	RejectionCode        string
	ReasonCodes          []string
	RiskDecisionID       *risk.DecisionID
	EffectID             *econ.EffectID
	JournalTransactionID *ledger.TransactionID
	ReservationID        string
	Evidence             econ.EvidenceRef
}

// transferEvent is the payload of every transfer audit record. Ids and
// states only: amounts live in the journal and the memo in the row.
type transferEvent struct {
	TransferID           string `json:"transfer_id"`
	IntentID             string `json:"intent_id"`
	From                 string `json:"from_status,omitempty"`
	To                   string `json:"to_status"`
	RejectionCode        string `json:"rejection_code,omitempty"`
	JournalTransactionID string `json:"journal_transaction_id,omitempty"`
	ReversalJournalID    string `json:"reversal_journal_transaction_id,omitempty"`
}

// decide moves a PENDING transfer to COMPLETED or REJECTED: transition row,
// status and decision columns, and an audit record, in tx.
func (s *Service) decide(ctx context.Context, tx pgx.Tx, t Transfer, to Status, o outcome, actor econ.Actor, reason string, now time.Time) (Transfer, error) {
	if t.Status != StatusPending || !CanTransition(t.Status, to) {
		return Transfer{}, errs.Newf(errs.CodeInvalidStateTransition, "transfer %s cannot move %s -> %s", t.ID, t.Status, to)
	}
	if err := s.writeTransition(ctx, tx, t, to, actor, reason, now); err != nil {
		return Transfer{}, err
	}
	codes := o.ReasonCodes
	if codes == nil {
		codes = []string{}
	}
	var evHash []byte
	if len(o.Evidence.SHA256) > 0 {
		evHash = o.Evidence.SHA256
	}
	out, err := scanTransfer(tx.QueryRow(ctx, `UPDATE internal_transfers
		SET status = $2, rejection_code = NULLIF($3,''), reason_codes = $4, risk_decision_id = $5, effect_id = $6,
		    journal_transaction_id = $7, reservation_id = NULLIF($8,'')::uuid, evidence_ref = NULLIF($9,''),
		    evidence_sha256 = $10, decided_at = $11
		WHERE id = $1 AND status = 'PENDING' RETURNING `+transferColumns,
		t.ID, to, o.RejectionCode, codes, o.RiskDecisionID, o.EffectID, o.JournalTransactionID, o.ReservationID,
		o.Evidence.URI, evHash, now))
	if err != nil {
		return Transfer{}, dbErr("decide transfer", err)
	}
	if err := s.announce(ctx, tx, out, t.Status, to, actor, reason, o.Evidence.URI, now); err != nil {
		return Transfer{}, err
	}
	return out, nil
}

// markReversed moves a COMPLETED transfer to REVERSED on the approved
// proposal it executes. actor is the approving operator.
func (s *Service) markReversed(ctx context.Context, tx pgx.Tx, t Transfer, journal ledger.TransactionID, proposal ReversalProposal, actor econ.Actor, reason string, evidence econ.EvidenceRef, now time.Time) (Transfer, error) {
	if t.Status != StatusCompleted {
		return Transfer{}, errs.Newf(errs.CodeInvalidStateTransition, "transfer %s cannot move %s -> %s", t.ID, t.Status, StatusReversed)
	}
	if err := s.writeTransition(ctx, tx, t, StatusReversed, actor, reason, now); err != nil {
		return Transfer{}, err
	}
	out, err := scanTransfer(tx.QueryRow(ctx, `UPDATE internal_transfers
		SET status = 'REVERSED', reversal_journal_transaction_id = $2, reversal_reason_code = $3,
		    reversed_by_actor_id = $4, reversal_evidence_ref = $5, reversed_at = $6, reversal_proposal_id = $7
		WHERE id = $1 AND status = 'COMPLETED' RETURNING `+transferColumns,
		t.ID, journal, proposal.ReasonCode, actor.ID, evidence.URI, now, proposal.ID))
	if err != nil {
		return Transfer{}, dbErr("reverse transfer", err)
	}
	if err := s.announce(ctx, tx, out, t.Status, StatusReversed, actor, reason, evidence.URI, now); err != nil {
		return Transfer{}, err
	}
	return out, nil
}

func (s *Service) writeTransition(ctx context.Context, tx pgx.Tx, t Transfer, to Status, actor econ.Actor, reason string, now time.Time) error {
	if strings.TrimSpace(reason) == "" {
		reason = string(to)
	}
	_, err := tx.Exec(ctx, `INSERT INTO internal_transfer_transitions
		(id, transfer_id, from_status, to_status, actor_type, actor_id, reason, correlation_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`,
		id.New[id.Any](), t.ID, t.Status, to, actor.Type, actor.ID, reason, t.CorrelationID, now)
	if err != nil {
		return dbErr("record transfer transition", err)
	}
	return nil
}

func (s *Service) announce(ctx context.Context, tx pgx.Tx, t Transfer, from, to Status, actor econ.Actor, reason, evidenceRef string, now time.Time) error {
	ev := transferEvent{
		TransferID: t.ID.String(), IntentID: t.IntentID.String(), From: string(from), To: string(to),
		RejectionCode: t.RejectionCode,
	}
	if t.JournalTransactionID != nil {
		ev.JournalTransactionID = t.JournalTransactionID.String()
	}
	if t.ReversalJournalTransactionID != nil {
		ev.ReversalJournalID = t.ReversalJournalTransactionID.String()
	}
	return s.auditTransfer(ctx, tx, t, "transfers.transfer."+strings.ToLower(string(to)), actor, reason, evidenceRef, ev, now)
}

// auditTransfer appends one audit record about a transfer to the sender
// account's stream. Every transfer record goes to the SENDER's stream, so a
// transfer never takes the audit lock of a second account.
func (s *Service) auditTransfer(ctx context.Context, tx pgx.Tx, t Transfer, action string, actor econ.Actor, reason, evidenceRef string, payload any, now time.Time) error {
	return s.appendAudit(ctx, tx, t, auditSubject{resourceType: "internal_transfer", resourceID: t.ID.String(), correlationID: t.CorrelationID},
		action, actor, reason, evidenceRef, payload, now)
}

// auditSubject is the resource an audit record is about, and the correlation
// id it carries.
type auditSubject struct {
	resourceType  string
	resourceID    string
	correlationID string
}

// appendAudit appends one audit record to the stream of t's sender account,
// whatever resource of the transfer it is about (the transfer itself, or a
// proposal to reverse it).
func (s *Service) appendAudit(ctx context.Context, tx pgx.Tx, t Transfer, subject auditSubject, action string, actor econ.Actor, reason, evidenceRef string, payload any, now time.Time) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "transfers: encode audit payload")
	}
	if strings.TrimSpace(reason) == "" {
		reason = action
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream: audit.AccountStream(t.SenderAccountID.String()), ActorType: string(actor.Type), ActorID: actor.ID,
		Action: action, ResourceType: subject.resourceType, ResourceID: subject.resourceID,
		CorrelationID: subject.correlationID, PolicyVersion: t.PolicyVersion, Reason: reason, EvidenceRef: evidenceRef,
		Payload: body, OccurredAt: now,
	})
	if err != nil {
		return dbErr("audit transfer", err)
	}
	return nil
}

// usageSince is what a sender sent since the start of a rolling window.
// The sum saturates at the int64 bound, which exceeds every limit.
func usageSince(ctx context.Context, q db.Querier, sender accounts.AccountID, since time.Time) (Usage, error) {
	var count, notional int64
	err := q.QueryRow(ctx, `SELECT count(*), LEAST(coalesce(sum(notional_usd_minor), 0), 9223372036854775807)::bigint
		FROM internal_transfers
		WHERE sender_account_id = $1 AND status IN ('COMPLETED','REVERSED') AND created_at > $2`,
		sender, since).Scan(&count, &notional)
	if err != nil {
		return Usage{}, dbErr("read transfer usage", err)
	}
	return Usage{Count: int(count), Notional: money.USDFromMinor(notional)}, nil
}

// ruleInForce returns the latest rule for (sender, recipient) and when it
// was set. A CLEAR, or no rule at all, is the empty rule.
func ruleInForce(ctx context.Context, q db.Querier, sender, recipient accounts.AccountID) (RuleKind, *time.Time, error) {
	var rule RuleKind
	var at time.Time
	err := q.QueryRow(ctx, `SELECT rule, effective_at FROM transfer_recipient_rules
		WHERE sender_account_id = $1 AND recipient_account_id = $2
		ORDER BY effective_at DESC, id DESC LIMIT 1`, sender, recipient).Scan(&rule, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, dbErr("read recipient rule", err)
	}
	if rule == RuleClear {
		return "", nil, nil
	}
	at = at.UTC()
	return rule, &at, nil
}

// modeInForce returns the sender's latest mode; no row means OPEN.
func modeInForce(ctx context.Context, q db.Querier, account accounts.AccountID) (SenderMode, error) {
	var mode SenderMode
	err := q.QueryRow(ctx, `SELECT mode FROM transfer_sender_modes WHERE account_id = $1
		ORDER BY effective_at DESC, id DESC LIMIT 1`, account).Scan(&mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return SenderOpen, nil
	}
	if err != nil {
		return "", dbErr("read sender mode", err)
	}
	return mode, nil
}

// lockAccount serializes, for the rest of tx, every transfer decision that
// debits this account: the sender's own transfers and the reversal of a
// transfer it received. Limits and velocity are read under it, so two
// concurrent transfers can never both fit under a limit only one of them
// fits under.
func lockAccount(ctx context.Context, tx pgx.Tx, account accounts.AccountID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "transfers.account:"+account.String()); err != nil {
		return dbErr("lock account for transfer", err)
	}
	return nil
}

func dbErr(op string, err error) error {
	if db.IsRetryable(err) {
		return err
	}
	if ee, ok := errs.As(err); ok {
		return ee
	}
	if mapped := ledger.MapError(err); mapped != nil {
		if ee, ok := errs.As(mapped); ok && ee.Code != errs.CodeInternal {
			return ee
		}
	}
	return errs.Wrap(err, errs.CodeInternal, "transfers: "+op)
}
