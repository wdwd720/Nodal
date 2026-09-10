package reconciliation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// EventEmitter appends envelopes to the transactional outbox inside the
// caller's transaction. *event.Outbox satisfies it.
type EventEmitter interface {
	Enqueue(ctx context.Context, tx pgx.Tx, topic string, events ...event.Envelope) error
}

// Repository persists reconciliation records, their transitions and wallet
// balance observations. It holds no connection: every method runs on the
// caller's Querier or transaction, so a status change, its transition row, its
// outbox event and its audit event commit together or not at all.
//
// Migration 00603 refuses a status change without a matching transition row in
// the same transaction (SQLSTATE AU001), and reconciliation_transitions is
// immutable. Repository is therefore the only supported write path.
type Repository struct {
	clk   clock.Clock
	emit  EventEmitter
	audit audit.Writer
}

// NewRepository wires a Repository. The emitter and the audit writer are
// mandatory: a status change nobody can see downstream, or that leaves no
// audit row, is not an acceptable outcome.
func NewRepository(clk clock.Clock, emit EventEmitter, aud audit.Writer) *Repository {
	if clk == nil {
		clk = clock.System()
	}
	return &Repository{clk: clk, emit: emit, audit: aud}
}

func (r *Repository) check() error {
	if r.emit == nil {
		return errs.New(errs.CodeInternal, "reconciliation: outbox emitter is not configured")
	}
	if r.audit == nil {
		return errs.New(errs.CodeInternal, "reconciliation: audit writer is not configured")
	}
	return nil
}

func dbErr(op string, err error) error {
	if e, ok := errs.As(err); ok {
		return e
	}
	if db.IsRetryable(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if db.SQLState(err) == sqlStateTransitionRequired {
		return errs.Wrap(err, errs.CodeInternal,
			"reconciliation: a status change reached the database without a matching transition row")
	}
	return errs.Wrap(err, errs.CodeInternal, "reconciliation: "+op)
}

// sqlStateTransitionRequired is the SQLSTATE raised by cp_require_transition
// (migration 00603).
const sqlStateTransitionRequired = "AU001"

const recordColumns = `id, kind, mode, scope_type, scope_id,
	coalesce(account_id, '00000000-0000-0000-0000-000000000000'::uuid),
	coalesce(asset_id, '00000000-0000-0000-0000-000000000000'::uuid),
	expected, observed, difference, status, material, blocks_new_risk,
	opened_at, matched_at, resolved_at,
	coalesce(resolved_by_actor_type,''), coalesce(resolved_by_actor_id,''),
	coalesce(resolution_reason,''), coalesce(resolution_evidence_ref,''),
	coalesce(approval_id::text,''), coalesce(compensating_journal_transaction_id::text,''),
	coalesce(correlation_id,''), created_at, updated_at`

func scanRecord(row pgx.Row) (Record, error) {
	var rec Record
	var expected, observed, difference []byte
	if err := row.Scan(&rec.ID, &rec.Kind, &rec.Mode, &rec.ScopeType, &rec.ScopeID, &rec.AccountID, &rec.AssetID,
		&expected, &observed, &difference, &rec.Status, &rec.Material, &rec.BlocksNewRisk,
		&rec.OpenedAt, &rec.MatchedAt, &rec.ResolvedAt,
		&rec.ResolvedByActorType, &rec.ResolvedByActorID, &rec.ResolutionReason, &rec.ResolutionEvidenceRef,
		&rec.ApprovalID, &rec.CompensatingJournalTxID, &rec.CorrelationID, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return Record{}, err
	}
	rec.Expected = json.RawMessage(expected)
	rec.Observed = json.RawMessage(observed)
	rec.Difference = json.RawMessage(difference)
	return rec, nil
}

// OpenRequest describes a comparison whose result is being recorded.
type OpenRequest struct {
	Kind      Kind
	Mode      Mode
	ScopeType string
	ScopeID   string
	AccountID accounts.AccountID
	AssetID   assets.AssetID

	Expected   any
	Observed   any
	Difference any

	// Status is the classification: MATCHED when internal and external agree,
	// MISMATCH when they do not, OPEN when the comparison is inconclusive and
	// a later pass must decide. It is reached through a real OPEN → Status
	// transition, so the transition history is complete.
	Status        Status
	Material      bool
	BlocksNewRisk bool

	CorrelationID string
	Actor         Actor
	Reason        string
	EvidenceRef   string
}

func (o OpenRequest) validate() error {
	problems := map[string]any{}
	if !o.Kind.Valid() {
		problems["kind"] = "unknown kind"
	}
	if !o.Mode.Valid() {
		problems["mode"] = "unknown mode"
	}
	if strings.TrimSpace(o.ScopeType) == "" {
		problems["scope_type"] = "required"
	}
	if strings.TrimSpace(o.ScopeID) == "" {
		problems["scope_id"] = "required"
	}
	switch o.Status {
	case StatusOpen, StatusMatched, StatusMismatch:
	default:
		problems["status"] = "a new record is OPEN, MATCHED or MISMATCH"
	}
	if o.Status == StatusMatched && o.BlocksNewRisk {
		problems["blocks_new_risk"] = "a matched record never blocks new risk"
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "reconciliation: invalid open request").WithFields(problems)
	}
	return o.Actor.normalize().validate()
}

func encodeJSON(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage(`{}`), nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "reconciliation: encode record document")
	}
	return b, nil
}

// Open inserts a record and drives it to its classification.
//
// The row is inserted in OPEN with a NONE → OPEN transition row, then, when
// req.Status is MATCHED or MISMATCH, a single status UPDATE with its own
// transition row moves it there. That is exactly one status UPDATE for the
// row in this transaction, which is what the deferred constraint trigger of
// migration 00603 can verify (it compares the flag left by the last
// transition insert with the new status of every queued update event).
func (r *Repository) Open(ctx context.Context, tx pgx.Tx, req OpenRequest) (Record, error) {
	if err := r.check(); err != nil {
		return Record{}, err
	}
	if err := req.validate(); err != nil {
		return Record{}, err
	}
	expected, err := encodeJSON(req.Expected)
	if err != nil {
		return Record{}, err
	}
	observed, err := encodeJSON(req.Observed)
	if err != nil {
		return Record{}, err
	}
	difference, err := encodeJSON(req.Difference)
	if err != nil {
		return Record{}, err
	}
	now := r.clk.Now().UTC()
	actor := req.Actor.normalize()
	rec, err := scanRecord(tx.QueryRow(ctx, `INSERT INTO reconciliation_records
			(id, kind, mode, scope_type, scope_id, account_id, asset_id, expected, observed, difference,
			 status, material, blocks_new_risk, opened_at, correlation_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'OPEN',$11,$12,$13,$14,$13,$13)
		RETURNING `+recordColumns,
		NewRecordID(), string(req.Kind), string(req.Mode), req.ScopeType, req.ScopeID,
		nullableID(req.AccountID.IsZero(), req.AccountID), nullableID(req.AssetID.IsZero(), req.AssetID),
		[]byte(expected), []byte(observed), []byte(difference),
		req.Material, req.BlocksNewRisk && req.Status != StatusMatched, now, nullable(req.CorrelationID)))
	if err != nil {
		return Record{}, dbErr("insert reconciliation record", err)
	}
	if err := r.writeTransition(ctx, tx, rec, StatusNone, StatusOpen, actor, req.Reason, req.EvidenceRef, now, nil); err != nil {
		return Record{}, err
	}
	if err := r.announce(ctx, tx, rec, StatusNone, StatusOpen, actor, req.Reason, req.EvidenceRef, now); err != nil {
		return Record{}, err
	}
	if req.Status == StatusOpen {
		return rec, nil
	}
	return r.Transition(ctx, tx, rec.ID, req.Status, TransitionEvidence{
		Actor: actor, Reason: req.Reason, EvidenceRef: req.EvidenceRef,
	})
}

func nullableID[T interface{ String() string }](zero bool, v T) any {
	if zero {
		return nil
	}
	return v
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// TransitionEvidence is what a status change records.
type TransitionEvidence struct {
	Actor       Actor
	Reason      string
	EvidenceRef string
	// Patch carries the fields a resolution must set atomically with the
	// status change. It is nil for plain transitions.
	Patch *ResolutionPatch
}

// ResolutionPatch holds the resolution columns written with a terminal
// transition. It is assembled by the Resolver, never by callers.
type ResolutionPatch struct {
	ResolvedByActorType     security.ActorType
	ResolvedByActorID       string
	Reason                  string
	EvidenceRef             string
	ApprovalID              string
	CompensatingJournalTxID string
}

// Transition moves a record to a new status. It locks the row, checks the
// PART 51 table, writes the immutable transition row that migration 00603
// requires, updates the record, and enqueues the outbox and audit events —
// all inside tx.
//
// A record may change status at most once per transaction: the deferred
// constraint trigger queues one event per UPDATE and compares each against the
// single transaction-local flag, so a second change would fail at COMMIT with
// SQLSTATE AU001. Transition detects that here and returns CONFLICT instead.
func (r *Repository) Transition(ctx context.Context, tx pgx.Tx, recordID RecordID, to Status, ev TransitionEvidence) (Record, error) {
	if err := r.check(); err != nil {
		return Record{}, err
	}
	if !to.Valid() {
		return Record{}, errs.New(errs.CodeValidationFailed, "reconciliation: unknown status").WithField("to", string(to))
	}
	actor := ev.Actor.normalize()
	if err := actor.validate(); err != nil {
		return Record{}, err
	}
	rec, err := r.GetForUpdate(ctx, tx, recordID)
	if err != nil {
		return Record{}, err
	}
	if err := checkTransition(rec.ID, rec.Status, to); err != nil {
		return Record{}, err
	}
	if err := claimStatusUpdate(ctx, tx, rec.ID); err != nil {
		return Record{}, err
	}
	now := r.clk.Now().UTC()
	from := rec.Status

	p := ev.Patch
	if p == nil {
		p = &ResolutionPatch{}
	}

	// The transition row IS the status change since 00751, and it carries the
	// whole resolution with it: who resolved the discrepancy, why, on what
	// evidence, under whose approval, and with which compensating journal
	// transaction. Those are facts about THIS transition, not properties the
	// record acquires beside it.
	//
	// It has to travel in the INSERT rather than in a later UPDATE, because the
	// trigger that applies the row fires on INSERT -- a resolution written
	// afterwards would arrive after the record had already moved without it.
	//
	// `resolved_by_actor_type` is deliberately not this row's own `actor_type`.
	// Only the first is refused to an AGENT, and mapping one onto the other
	// would let an agent be recorded as the resolver of a financial discrepancy.
	if err := r.writeTransition(ctx, tx, rec, from, to, actor, ev.Reason, ev.EvidenceRef, now, p); err != nil {
		return Record{}, err
	}
	updated, err := scanRecord(tx.QueryRow(ctx,
		`SELECT `+recordColumns+` FROM reconciliation_records WHERE id = $1`, rec.ID))
	if err != nil {
		return Record{}, dbErr("read reconciliation record back", err)
	}
	if err := r.announce(ctx, tx, updated, from, to, actor, ev.Reason, ev.EvidenceRef, now); err != nil {
		return Record{}, err
	}
	return updated, nil
}

// claimStatusUpdate records, in a transaction-local setting, that this record
// has already changed status in this transaction, and refuses a second change.
func claimStatusUpdate(ctx context.Context, tx pgx.Tx, recordID RecordID) error {
	key := "cp.recon.updated.x" + strings.ReplaceAll(recordID.String(), "-", "_")
	var claimed string
	if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting($1, true), '')`, key).Scan(&claimed); err != nil {
		return dbErr("read transition claim", err)
	}
	if claimed != "" {
		return errs.New(errs.CodeConflict,
			"reconciliation: a record may change status at most once per transaction").
			WithField("record_id", recordID.String())
	}
	if _, err := tx.Exec(ctx, `SELECT set_config($1, '1', true)`, key); err != nil {
		return dbErr("set transition claim", err)
	}
	return nil
}

func (r *Repository) writeTransition(ctx context.Context, tx pgx.Tx, rec Record, from, to Status, actor Actor, reason, evidenceRef string, now time.Time, p *ResolutionPatch) error {
	if p == nil {
		p = &ResolutionPatch{}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_transitions
			(id, record_id, from_status, to_status, actor_type, actor_id, reason, evidence_ref, occurred_at,
			 to_resolved_by_actor_type, to_resolved_by_actor_id, to_resolution_reason,
			 to_resolution_evidence_ref, to_approval_id, to_compensating_journal_transaction_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::uuid,$15::uuid)`,
		NewTransitionID(), rec.ID, string(from), string(to), string(actor.Type), actor.ID,
		nullable(reason), nullable(evidenceRef), now,
		nullable(string(p.ResolvedByActorType)), nullable(p.ResolvedByActorID),
		nullable(p.Reason), nullable(p.EvidenceRef), nullable(p.ApprovalID),
		nullable(p.CompensatingJournalTxID)); err != nil {
		return dbErr("insert reconciliation transition", err)
	}
	return nil
}

// UpdateObservation refreshes the evidence documents and the materiality of an
// unresolved record without changing its status. Re-observing the same
// difference is therefore idempotent: the record is updated in place instead
// of a second record being opened.
func (r *Repository) UpdateObservation(ctx context.Context, tx pgx.Tx, recordID RecordID, expected, observed, difference any, material, blocksNewRisk bool) (Record, error) {
	exp, err := encodeJSON(expected)
	if err != nil {
		return Record{}, err
	}
	obs, err := encodeJSON(observed)
	if err != nil {
		return Record{}, err
	}
	diff, err := encodeJSON(difference)
	if err != nil {
		return Record{}, err
	}
	rec, err := scanRecord(tx.QueryRow(ctx, `UPDATE reconciliation_records
		SET expected = $2, observed = $3, difference = $4,
		    material = material OR $5,
		    blocks_new_risk = CASE WHEN status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED')
		                           THEN blocks_new_risk OR $6 ELSE false END
		WHERE id = $1 RETURNING `+recordColumns,
		recordID, []byte(exp), []byte(obs), []byte(diff), material, blocksNewRisk))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, notFound(recordID)
		}
		return Record{}, dbErr("update reconciliation observation", err)
	}
	return rec, nil
}

func notFound(recordID RecordID) error {
	return errs.New(errs.CodeNotFound, "reconciliation record not found").WithField("record_id", recordID.String())
}

// Get returns a record.
func (r *Repository) Get(ctx context.Context, q db.Querier, recordID RecordID) (Record, error) {
	rec, err := scanRecord(q.QueryRow(ctx, `SELECT `+recordColumns+` FROM reconciliation_records WHERE id = $1`, recordID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, notFound(recordID)
		}
		return Record{}, dbErr("get reconciliation record", err)
	}
	return rec, nil
}

// GetForUpdate returns a record with its row locked.
func (r *Repository) GetForUpdate(ctx context.Context, tx pgx.Tx, recordID RecordID) (Record, error) {
	rec, err := scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM reconciliation_records WHERE id = $1 FOR UPDATE`, recordID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, notFound(recordID)
		}
		return Record{}, dbErr("lock reconciliation record", err)
	}
	return rec, nil
}

// FindUnresolvedByScope returns the newest unresolved record for a scope, if
// one exists. It is how the engine avoids opening a second record for a
// difference it has already reported.
func (r *Repository) FindUnresolvedByScope(ctx context.Context, q db.Querier, kind Kind, scopeType, scopeID string) (Record, bool, error) {
	rec, err := scanRecord(q.QueryRow(ctx, `SELECT `+recordColumns+` FROM reconciliation_records
		WHERE kind = $1 AND scope_type = $2 AND scope_id = $3
		  AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED')
		ORDER BY opened_at DESC, id DESC LIMIT 1`, string(kind), scopeType, scopeID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, false, nil
		}
		return Record{}, false, dbErr("find reconciliation record by scope", err)
	}
	return rec, true, nil
}

// FindUnresolvedByScopeForUpdate is FindUnresolvedByScope with the row locked,
// so a concurrent sweep cannot open a duplicate record for the same scope.
func (r *Repository) FindUnresolvedByScopeForUpdate(ctx context.Context, tx pgx.Tx, kind Kind, scopeType, scopeID string) (Record, bool, error) {
	rec, err := scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM reconciliation_records
		WHERE kind = $1 AND scope_type = $2 AND scope_id = $3
		  AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED')
		ORDER BY opened_at DESC, id DESC LIMIT 1 FOR UPDATE`, string(kind), scopeType, scopeID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, false, nil
		}
		return Record{}, false, dbErr("lock reconciliation record by scope", err)
	}
	return rec, true, nil
}

// FindLatestByScopeForUpdate returns the newest record for a scope whatever
// its status, with the row locked. It is what makes a repeated comparison
// converge instead of accumulating records: the caller decides whether the
// existing record still describes what it just observed.
func (r *Repository) FindLatestByScopeForUpdate(ctx context.Context, tx pgx.Tx, kind Kind, scopeType, scopeID string) (Record, bool, error) {
	rec, err := scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM reconciliation_records
		WHERE kind = $1 AND scope_type = $2 AND scope_id = $3
		ORDER BY opened_at DESC, id DESC LIMIT 1 FOR UPDATE`, string(kind), scopeType, scopeID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, false, nil
		}
		return Record{}, false, dbErr("lock latest reconciliation record by scope", err)
	}
	return rec, true, nil
}

// ListUnresolved returns unresolved records oldest first, for the operator
// queue and for the periodic escalation sweep.
func (r *Repository) ListUnresolved(ctx context.Context, q db.Querier, limit int) ([]Record, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return r.list(ctx, q, `SELECT `+recordColumns+` FROM reconciliation_records
		WHERE status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED') ORDER BY opened_at, id LIMIT $1`, limit)
}

// ListForAccount returns an account's records newest first.
func (r *Repository) ListForAccount(ctx context.Context, q db.Querier, accountID accounts.AccountID, limit int) ([]Record, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return r.list(ctx, q, `SELECT `+recordColumns+` FROM reconciliation_records
		WHERE account_id = $1 ORDER BY opened_at DESC, id DESC LIMIT $2`, accountID, limit)
}

func (r *Repository) list(ctx context.Context, q db.Querier, sql string, args ...any) ([]Record, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, dbErr("list reconciliation records", err)
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, dbErr("scan reconciliation record", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list reconciliation records", err)
	}
	return out, nil
}

// ListTransitions returns a record's transitions oldest first: the complete,
// immutable history an operator or an auditor reads.
func (r *Repository) ListTransitions(ctx context.Context, q db.Querier, recordID RecordID) ([]Transition, error) {
	rows, err := q.Query(ctx, `SELECT id, record_id, from_status, to_status, actor_type, actor_id,
		coalesce(reason,''), coalesce(evidence_ref,''), occurred_at
		FROM reconciliation_transitions WHERE record_id = $1 ORDER BY occurred_at, id`, recordID)
	if err != nil {
		return nil, dbErr("list reconciliation transitions", err)
	}
	defer rows.Close()
	out := []Transition{}
	for rows.Next() {
		var t Transition
		if err := rows.Scan(&t.ID, &t.RecordID, &t.From, &t.To, &t.ActorType, &t.ActorID, &t.Reason, &t.EvidenceRef, &t.OccurredAt); err != nil {
			return nil, dbErr("scan reconciliation transition", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list reconciliation transitions", err)
	}
	return out, nil
}

// RecordBalanceObservation stores one observer's view of a (wallet, asset)
// balance. Observations are append-only evidence: cp_app holds SELECT and
// INSERT on the table and nothing else.
func (r *Repository) RecordBalanceObservation(ctx context.Context, q db.Querier, o BalanceObservation) (BalanceObservation, error) {
	if o.WalletID == "" || o.AssetID.IsZero() {
		return BalanceObservation{}, errs.New(errs.CodeValidationFailed, "reconciliation: wallet id and asset id are required")
	}
	if o.Quantity.IsNegative() {
		return BalanceObservation{}, errs.New(errs.CodeValidationFailed, "reconciliation: an observed balance is never negative").
			WithField("quantity", o.Quantity.String())
	}
	if strings.TrimSpace(o.Source) == "" {
		return BalanceObservation{}, errs.New(errs.CodeValidationFailed, "reconciliation: observation source is required")
	}
	if o.ID.IsZero() {
		o.ID = NewObservationID()
	}
	if o.ObservedAt.IsZero() {
		o.ObservedAt = r.clk.Now()
	}
	received := r.clk.Now().UTC()
	err := q.QueryRow(ctx, `INSERT INTO wallet_balance_observations
			(id, wallet_id, asset_id, quantity, source, slot, observed_at, received_at, raw_ref)
		VALUES ($1,$2::uuid,$3,$4::numeric,$5,$6,$7,$8,$9)
		RETURNING received_at`,
		o.ID, o.WalletID, o.AssetID, o.Quantity.String(), o.Source, o.Slot, o.ObservedAt.UTC(), received, nullable(o.RawRef)).
		Scan(&o.ReceivedAt)
	if err != nil {
		return BalanceObservation{}, dbErr("insert wallet balance observation", err)
	}
	return o, nil
}

// LatestBalanceObservations returns the most recent observation per source for
// a (wallet, asset).
func (r *Repository) LatestBalanceObservations(ctx context.Context, q db.Querier, walletID string, asset assets.AssetID) ([]BalanceObservation, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (source) id, wallet_id::text, asset_id, quantity::text, source, slot, observed_at, received_at, coalesce(raw_ref,'')
		FROM wallet_balance_observations WHERE wallet_id = $1::uuid AND asset_id = $2
		ORDER BY source, received_at DESC`, walletID, asset)
	if err != nil {
		return nil, dbErr("list wallet balance observations", err)
	}
	defer rows.Close()
	out := []BalanceObservation{}
	for rows.Next() {
		var o BalanceObservation
		var qty string
		if err := rows.Scan(&o.ID, &o.WalletID, &o.AssetID, &qty, &o.Source, &o.Slot, &o.ObservedAt, &o.ReceivedAt, &o.RawRef); err != nil {
			return nil, dbErr("scan wallet balance observation", err)
		}
		parsed, err := money.ParseQuantity(qty)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "reconciliation: decode observed balance")
		}
		o.Quantity = parsed
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list wallet balance observations", err)
	}
	return out, nil
}
