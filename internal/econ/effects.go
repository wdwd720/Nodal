package econ

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/security"
)

// Actor is who caused a state change. Agents never do: an agent proposes an
// intent, and a deterministic service applies the effect.
type Actor struct {
	Type security.ActorType
	ID   string
}

// SystemActor is the actor of background work.
func SystemActor(component string) Actor { return Actor{Type: security.ActorSystem, ID: component} }

func (a Actor) validate() error {
	if !a.Type.Valid() || strings.TrimSpace(a.ID) == "" {
		return errs.New(errs.CodeValidationFailed, "econ: an actor type and id are required")
	}
	if a.Type == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "econ: an agent proposes intents; it never applies an economic effect")
	}
	return nil
}

// Effect is an economic_effects row.
type Effect struct {
	ID                               EffectID
	Key                              string
	Subsystem                        Subsystem
	Kind                             string
	IntentID                         *IntentID
	AccountID                        string
	RequestHash                      []byte
	PostsLedger                      bool
	Status                           EffectStatus
	JournalTransactionID             *ledger.TransactionID
	CompensationJournalTransactionID *ledger.TransactionID
	ReservationID                    string
	ExternalRef                      string
	EvidenceRef                      string
	EvidenceSHA256                   []byte
	FailureCode                      string
	CorrelationID                    string
	CreatedAt                        time.Time
	UpdatedAt                        time.Time
}

// Terminal reports whether the effect can no longer change, except that an
// APPLIED effect can still be COMPENSATED.
func (e Effect) Terminal() bool { return e.Status == EffectFailed || e.Status == EffectCompensated }

// BeginRequest claims an economic effect.
type BeginRequest struct {
	// Key is deterministic per economic effect: the same logical operation
	// always produces the same key (e.g. "p2p:<transfer id>").
	Key       string
	Subsystem Subsystem
	// Kind names the effect inside its subsystem, e.g. "P2P_TRANSFER".
	Kind      string
	IntentID  *IntentID
	AccountID string
	// Request is the canonical description of what was asked for. Its hash is
	// stored, and a later Begin with the same key and a different request is
	// refused with INVALID_IDEMPOTENCY_REUSE.
	Request       any
	PostsLedger   bool
	Actor         Actor
	CorrelationID string
}

func (r BeginRequest) validate() error {
	var problems []string
	if k := strings.TrimSpace(r.Key); k == "" || k != r.Key || len(k) > 300 {
		problems = append(problems, "key must be 1..300 bytes without surrounding whitespace")
	}
	if !r.Subsystem.Valid() {
		problems = append(problems, fmt.Sprintf("unknown subsystem %q", r.Subsystem))
	}
	if !effectKindPattern(r.Kind) {
		problems = append(problems, "kind must match ^[A-Z][A-Z0-9_]{1,63}$")
	}
	if r.Request == nil {
		problems = append(problems, "request is required: it is what makes a replay distinguishable from a reuse")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "econ: invalid effect").WithField("problems", problems)
	}
	return r.Actor.validate()
}

func effectKindPattern(s string) bool {
	if len(s) < 2 || len(s) > 64 || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// RequestHash is the sha256 of the canonical JSON of a request. Money and ids
// render as strings, so the hash never depends on float formatting.
func RequestHash(request any) ([]byte, error) {
	b, err := audit.CanonicalJSON(request)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "econ: request is not canonically encodable")
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// EffectStore persists economic effects. It holds no state beyond its
// dependencies; every method runs in the caller's transaction.
type EffectStore struct {
	clk    clock.Clock
	outbox *event.Outbox
	audit  audit.Writer
}

// NewEffectStore wires an EffectStore. All three dependencies are required:
// an effect whose transition left no outbox event or audit record would be an
// economic fact nobody downstream can see.
func NewEffectStore(clk clock.Clock, outbox *event.Outbox, aud audit.Writer) *EffectStore {
	if clk == nil || outbox == nil || aud == nil {
		panic("econ: NewEffectStore requires a clock, an outbox and an audit writer")
	}
	return &EffectStore{clk: clk, outbox: outbox, audit: aud}
}

const effectColumns = `id, effect_key, subsystem, effect_kind, intent_id, coalesce(account_id::text, ''), request_hash,
	posts_ledger, status, journal_transaction_id, compensation_journal_transaction_id, coalesce(reservation_id::text, ''),
	coalesce(external_ref, ''), coalesce(evidence_ref, ''), evidence_sha256, coalesce(failure_code, ''),
	coalesce(correlation_id, ''), created_at, updated_at`

func scanEffect(row pgx.Row) (Effect, error) {
	var e Effect
	var intentID *IntentID
	var jtx, ctx2 *ledger.TransactionID
	if err := row.Scan(&e.ID, &e.Key, &e.Subsystem, &e.Kind, &intentID, &e.AccountID, &e.RequestHash,
		&e.PostsLedger, &e.Status, &jtx, &ctx2, &e.ReservationID, &e.ExternalRef, &e.EvidenceRef,
		&e.EvidenceSHA256, &e.FailureCode, &e.CorrelationID, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return Effect{}, err
	}
	e.IntentID = intentID
	e.JournalTransactionID = jtx
	e.CompensationJournalTransactionID = ctx2
	e.CreatedAt = e.CreatedAt.UTC()
	e.UpdatedAt = e.UpdatedAt.UTC()
	return e, nil
}

// Begin claims an economic effect. It returns the effect and whether it
// already existed (a replay). A replay with the same request hash returns the
// existing row unchanged, whatever its status: the caller must act on that
// status (an APPLIED effect means "already done", an UNKNOWN one means "do not
// retry, reconcile"). A replay with a different request is refused.
func (s *EffectStore) Begin(ctx context.Context, tx pgx.Tx, r BeginRequest) (Effect, bool, error) {
	if tx == nil {
		return Effect{}, false, errs.New(errs.CodeInternal, "econ: Begin requires a transaction")
	}
	if err := r.validate(); err != nil {
		return Effect{}, false, err
	}
	hash, err := RequestHash(r.Request)
	if err != nil {
		return Effect{}, false, err
	}
	var account any
	if r.AccountID != "" {
		if _, err := id.ParseAny(r.AccountID); err != nil {
			return Effect{}, false, errs.New(errs.CodeValidationFailed, "econ: account_id must be a canonical uuid")
		}
		account = r.AccountID
	}
	now := s.clk.Now().UTC()
	eid := NewEffectID()
	row := tx.QueryRow(ctx, `INSERT INTO economic_effects
		(id, effect_key, subsystem, effect_kind, intent_id, account_id, request_hash, posts_ledger, status, correlation_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'PENDING',NULLIF($9,''),$10,$10)
		ON CONFLICT (effect_key) DO NOTHING
		RETURNING `+effectColumns,
		eid, r.Key, r.Subsystem, r.Kind, r.IntentID, account, hash, r.PostsLedger, r.CorrelationID, now)
	e, err := scanEffect(row)
	if err == nil {
		if err := s.announce(ctx, tx, e, "", EffectPending, r.Actor, "effect claimed", ""); err != nil {
			return Effect{}, false, err
		}
		return e, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Effect{}, false, dbErr("claim effect", err)
	}
	existing, err := s.lockByKey(ctx, tx, r.Key)
	if err != nil {
		return Effect{}, false, err
	}
	if !bytes.Equal(existing.RequestHash, hash) || existing.Subsystem != r.Subsystem || existing.Kind != r.Kind {
		return Effect{}, false, errs.New(errs.CodeInvalidIdempotencyReuse,
			"econ: this effect key was already used for a different request").
			WithField("effect_key", r.Key).
			WithField("effect_id", existing.ID.String())
	}
	return existing, true, nil
}

// ApplyRequest completes a PENDING or UNKNOWN effect.
type ApplyRequest struct {
	JournalTransactionID *ledger.TransactionID
	ReservationID        string
	ExternalRef          string
	Evidence             *EvidenceRef
	Actor                Actor
	Reason               string
}

// Apply moves an effect to APPLIED. A ledger-posting effect must name the
// journal transaction it applied; the database refuses the row otherwise.
func (s *EffectStore) Apply(ctx context.Context, tx pgx.Tx, eid EffectID, r ApplyRequest) (Effect, error) {
	if err := r.Actor.validate(); err != nil {
		return Effect{}, err
	}
	e, err := s.lock(ctx, tx, eid)
	if err != nil {
		return Effect{}, err
	}
	if e.Status == EffectApplied {
		// Idempotent: applying the same journal twice is the same fact.
		if e.PostsLedger && r.JournalTransactionID != nil && e.JournalTransactionID != nil && *e.JournalTransactionID == *r.JournalTransactionID {
			return e, nil
		}
		if !e.PostsLedger && r.JournalTransactionID == nil {
			return e, nil
		}
	}
	if !CanTransitionEffect(e.Status, EffectApplied) {
		return Effect{}, illegal(e, EffectApplied)
	}
	if e.PostsLedger && r.JournalTransactionID == nil {
		return Effect{}, errs.New(errs.CodeValidationFailed,
			"econ: a ledger-posting effect is not applied until it names its journal transaction").
			WithField("effect_id", e.ID.String())
	}
	if !e.PostsLedger && r.JournalTransactionID != nil {
		return Effect{}, errs.New(errs.CodeValidationFailed,
			"econ: this effect was declared not to post to the ledger and cannot name a journal transaction").
			WithField("effect_id", e.ID.String())
	}
	var evRef string
	var evHash []byte
	if r.Evidence != nil {
		evRef, evHash = r.Evidence.URI, r.Evidence.SHA256
	}
	if err := s.transition(ctx, tx, e, EffectApplied, r.Actor, reasonOr(r.Reason, "effect applied"), evRef); err != nil {
		return Effect{}, err
	}
	row := tx.QueryRow(ctx, `UPDATE economic_effects
		SET status = 'APPLIED', journal_transaction_id = coalesce($2, journal_transaction_id),
		    reservation_id = coalesce(NULLIF($3,'')::uuid, reservation_id),
		    external_ref = coalesce(NULLIF($4,''), external_ref),
		    evidence_ref = coalesce(NULLIF($5,''), evidence_ref),
		    evidence_sha256 = coalesce($6, evidence_sha256)
		WHERE id = $1 RETURNING `+effectColumns,
		e.ID, r.JournalTransactionID, r.ReservationID, r.ExternalRef, evRef, evHash)
	out, err := scanEffect(row)
	if err != nil {
		return Effect{}, dbErr("apply effect", err)
	}
	if err := s.announce(ctx, tx, out, e.Status, EffectApplied, r.Actor, reasonOr(r.Reason, "effect applied"), evRef); err != nil {
		return Effect{}, err
	}
	return out, nil
}

// MarkUnknown records that the outcome of an external step is not known. The
// effect may only leave UNKNOWN through Resolve after the outcome has been
// observed; nothing retries it.
func (s *EffectStore) MarkUnknown(ctx context.Context, tx pgx.Tx, eid EffectID, externalRef string, actor Actor, reason string) (Effect, error) {
	if err := actor.validate(); err != nil {
		return Effect{}, err
	}
	e, err := s.lock(ctx, tx, eid)
	if err != nil {
		return Effect{}, err
	}
	if e.Status == EffectUnknown {
		return e, nil
	}
	if !CanTransitionEffect(e.Status, EffectUnknown) {
		return Effect{}, illegal(e, EffectUnknown)
	}
	if err := s.transition(ctx, tx, e, EffectUnknown, actor, reasonOr(reason, "outcome unknown"), ""); err != nil {
		return Effect{}, err
	}
	row := tx.QueryRow(ctx, `UPDATE economic_effects SET status = 'UNKNOWN', external_ref = coalesce(NULLIF($2,''), external_ref)
		WHERE id = $1 RETURNING `+effectColumns, e.ID, externalRef)
	out, err := scanEffect(row)
	if err != nil {
		return Effect{}, dbErr("mark effect unknown", err)
	}
	if err := s.announce(ctx, tx, out, e.Status, EffectUnknown, actor, reasonOr(reason, "outcome unknown"), ""); err != nil {
		return Effect{}, err
	}
	return out, nil
}

// Fail records that the effect did not and will not happen.
func (s *EffectStore) Fail(ctx context.Context, tx pgx.Tx, eid EffectID, failureCode string, actor Actor, reason string) (Effect, error) {
	if err := actor.validate(); err != nil {
		return Effect{}, err
	}
	if strings.TrimSpace(failureCode) == "" {
		return Effect{}, errs.New(errs.CodeValidationFailed, "econ: a failed effect needs a failure code")
	}
	e, err := s.lock(ctx, tx, eid)
	if err != nil {
		return Effect{}, err
	}
	if e.Status == EffectFailed {
		return e, nil
	}
	if !CanTransitionEffect(e.Status, EffectFailed) {
		return Effect{}, illegal(e, EffectFailed)
	}
	if err := s.transition(ctx, tx, e, EffectFailed, actor, reasonOr(reason, failureCode), ""); err != nil {
		return Effect{}, err
	}
	row := tx.QueryRow(ctx, `UPDATE economic_effects SET status = 'FAILED', failure_code = $2 WHERE id = $1 RETURNING `+effectColumns,
		e.ID, failureCode)
	out, err := scanEffect(row)
	if err != nil {
		return Effect{}, dbErr("fail effect", err)
	}
	if err := s.announce(ctx, tx, out, e.Status, EffectFailed, actor, reasonOr(reason, failureCode), ""); err != nil {
		return Effect{}, err
	}
	return out, nil
}

// Compensate records that an APPLIED effect was reversed by a compensating
// journal transaction. History is never edited: the original journal stays,
// and the compensation is a second, linked fact.
func (s *EffectStore) Compensate(ctx context.Context, tx pgx.Tx, eid EffectID, compensation *ledger.TransactionID, actor Actor, reason string) (Effect, error) {
	if err := actor.validate(); err != nil {
		return Effect{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return Effect{}, errs.New(errs.CodeValidationFailed, "econ: a compensation needs a reason")
	}
	e, err := s.lock(ctx, tx, eid)
	if err != nil {
		return Effect{}, err
	}
	if e.Status == EffectCompensated {
		return e, nil
	}
	if !CanTransitionEffect(e.Status, EffectCompensated) {
		return Effect{}, illegal(e, EffectCompensated)
	}
	if e.PostsLedger && compensation == nil {
		return Effect{}, errs.New(errs.CodeValidationFailed, "econ: a ledger-posting effect is compensated only by a journal transaction")
	}
	if err := s.transition(ctx, tx, e, EffectCompensated, actor, reason, ""); err != nil {
		return Effect{}, err
	}
	row := tx.QueryRow(ctx, `UPDATE economic_effects SET status = 'COMPENSATED', compensation_journal_transaction_id = $2
		WHERE id = $1 RETURNING `+effectColumns, e.ID, compensation)
	out, err := scanEffect(row)
	if err != nil {
		return Effect{}, dbErr("compensate effect", err)
	}
	if err := s.announce(ctx, tx, out, e.Status, EffectCompensated, actor, reason, ""); err != nil {
		return Effect{}, err
	}
	return out, nil
}

// Get returns one effect.
func (s *EffectStore) Get(ctx context.Context, q db.Querier, eid EffectID) (Effect, error) {
	e, err := scanEffect(q.QueryRow(ctx, `SELECT `+effectColumns+` FROM economic_effects WHERE id = $1`, eid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Effect{}, errs.New(errs.CodeNotFound, "economic effect not found").WithField("effect_id", eid.String())
	}
	if err != nil {
		return Effect{}, dbErr("get effect", err)
	}
	return e, nil
}

// GetByKey returns the effect claimed under key, if any.
func (s *EffectStore) GetByKey(ctx context.Context, q db.Querier, key string) (Effect, bool, error) {
	e, err := scanEffect(q.QueryRow(ctx, `SELECT `+effectColumns+` FROM economic_effects WHERE effect_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Effect{}, false, nil
	}
	if err != nil {
		return Effect{}, false, dbErr("get effect by key", err)
	}
	return e, true, nil
}

// ListUnresolved returns effects of a subsystem still PENDING or UNKNOWN and
// last touched before olderThan, oldest first: the reconciliation worklist.
func (s *EffectStore) ListUnresolved(ctx context.Context, q db.Querier, subsystem Subsystem, olderThan time.Time, limit int) ([]Effect, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT `+effectColumns+` FROM economic_effects
		WHERE subsystem = $1 AND status IN ('PENDING','UNKNOWN') AND updated_at < $2
		ORDER BY updated_at, id LIMIT $3`, subsystem, olderThan, limit)
	if err != nil {
		return nil, dbErr("list unresolved effects", err)
	}
	defer rows.Close()
	var out []Effect
	for rows.Next() {
		e, err := scanEffect(rows)
		if err != nil {
			return nil, dbErr("scan effect", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *EffectStore) lock(ctx context.Context, tx pgx.Tx, eid EffectID) (Effect, error) {
	e, err := scanEffect(tx.QueryRow(ctx, `SELECT `+effectColumns+` FROM economic_effects WHERE id = $1 FOR UPDATE`, eid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Effect{}, errs.New(errs.CodeNotFound, "economic effect not found").WithField("effect_id", eid.String())
	}
	if err != nil {
		return Effect{}, dbErr("lock effect", err)
	}
	return e, nil
}

func (s *EffectStore) lockByKey(ctx context.Context, tx pgx.Tx, key string) (Effect, error) {
	e, err := scanEffect(tx.QueryRow(ctx, `SELECT `+effectColumns+` FROM economic_effects WHERE effect_key = $1 FOR UPDATE`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Effect{}, errs.New(errs.CodeInternal, "econ: conflict on effect key but no row is visible").WithField("effect_key", key)
	}
	if err != nil {
		return Effect{}, dbErr("lock effect by key", err)
	}
	return e, nil
}

func (s *EffectStore) transition(ctx context.Context, tx pgx.Tx, e Effect, to EffectStatus, actor Actor, reason, evidenceRef string) error {
	_, err := tx.Exec(ctx, `INSERT INTO economic_effect_transitions
		(id, effect_id, from_status, to_status, actor_type, actor_id, reason, evidence_ref, correlation_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10)`,
		id.New[id.Any](), e.ID, e.Status, to, actor.Type, actor.ID, reason, evidenceRef, e.CorrelationID, s.clk.Now().UTC())
	if err != nil {
		return dbErr("record effect transition", err)
	}
	return nil
}

// effectEvent is the payload of econ.effect.transitioned and of the audit
// record. It carries ids and states only; amounts live in the journal.
type effectEvent struct {
	EffectID             string `json:"effect_id"`
	EffectKey            string `json:"effect_key"`
	Subsystem            string `json:"subsystem"`
	Kind                 string `json:"effect_kind"`
	From                 string `json:"from_status,omitempty"`
	To                   string `json:"to_status"`
	IntentID             string `json:"intent_id,omitempty"`
	AccountID            string `json:"account_id,omitempty"`
	JournalTransactionID string `json:"journal_transaction_id,omitempty"`
	ExternalRef          string `json:"external_ref,omitempty"`
	FailureCode          string `json:"failure_code,omitempty"`
	Reason               string `json:"reason"`
}

func (s *EffectStore) announce(ctx context.Context, tx pgx.Tx, e Effect, from, to EffectStatus, actor Actor, reason, evidenceRef string) error {
	p := effectEvent{
		EffectID: e.ID.String(), EffectKey: e.Key, Subsystem: string(e.Subsystem), Kind: e.Kind,
		From: string(from), To: string(to), AccountID: e.AccountID, ExternalRef: e.ExternalRef,
		FailureCode: e.FailureCode, Reason: reason,
	}
	if e.IntentID != nil {
		p.IntentID = e.IntentID.String()
	}
	if e.JournalTransactionID != nil {
		p.JournalTransactionID = e.JournalTransactionID.String()
	}
	body, err := json.Marshal(p)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "econ: encode effect event")
	}
	now := s.clk.Now().UTC()
	env := event.Envelope{
		ID: event.NewEventID().String(), Type: string(event.TopicEconEffectTransitioned),
		SchemaVersion: event.TopicEconEffectTransitioned.Version(), Source: "econ",
		AggregateType: event.AggregateEconomicEffect, AggregateID: e.ID.String(),
		CorrelationID: e.CorrelationID, OccurredAt: now,
		DedupKey: e.ID.String() + ":" + string(to), Payload: body,
	}
	if err := s.outbox.Enqueue(ctx, tx, string(event.TopicEconEffectTransitioned), env); err != nil {
		return err
	}
	stream := audit.SystemStream
	if e.AccountID != "" {
		stream = audit.AccountStream(e.AccountID)
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream: stream, ActorType: string(actor.Type), ActorID: actor.ID,
		Action: "econ.effect." + strings.ToLower(string(to)), ResourceType: "economic_effect", ResourceID: e.ID.String(),
		CorrelationID: e.CorrelationID, Reason: reason, EvidenceRef: evidenceRef, Payload: body, OccurredAt: now,
	})
	return err
}

func illegal(e Effect, to EffectStatus) error {
	return errs.Newf(errs.CodeInvalidStateTransition, "economic effect %s cannot move %s -> %s", e.ID, e.Status, to).
		WithField("effect_id", e.ID.String()).
		WithField("from_status", string(e.Status)).
		WithField("to_status", string(to))
}

func reasonOr(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return reason
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
	return errs.Wrap(err, errs.CodeInternal, "econ: "+op)
}
