package intent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// Audit actions and resource type written by this package.
const (
	AuditResourceType      = "trade_intent"
	AuditActionReceived    = "intent.received"
	AuditActionTransitions = "intent.transitioned"
	AuditActionLinked      = "intent.linked"

	// EventSource is the Envelope.Source of every event this package emits.
	EventSource = "intent"
	// SQLStateImmutableIntent is raised by the trade_intents_immutable_identity
	// trigger (migration 00605).
	SQLStateImmutableIntent = "IN001"
)

// Pagination bounds for ListForAccount and ListOpen.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// IsImmutableIntent reports an error raised by the identity guard of
// migration 00605: an UPDATE tried to change the identity, economics or mode
// of a persisted intent.
func IsImmutableIntent(err error) bool { return db.SQLState(err) == SQLStateImmutableIntent }

// TransitionEvidence is who moved the intent, why, and with what proof.
// ActorType is any declared security.ActorType: pipeline workers transition
// as SERVICE or SYSTEM, humans as USER or OPERATOR; AGENT transitions are
// accepted as evidence of what an agent did (an agent can cancel its own
// intent) but never as authority.
type TransitionEvidence struct {
	ActorType     security.ActorType
	ActorID       string
	Reason        string
	EvidenceRef   string
	RejectionCode string
}

func (e TransitionEvidence) validate(to Status) error {
	fields := map[string]any{}
	if !e.ActorType.Valid() {
		fields["actor_type"] = "unknown actor type"
	}
	if strings.TrimSpace(e.ActorID) == "" || len(e.ActorID) > MaxActorIDLength || !isClean(e.ActorID) {
		fields["actor_id"] = "required"
	}
	if strings.TrimSpace(e.Reason) == "" || !isClean(e.Reason) {
		fields["reason"] = "required"
	}
	if e.EvidenceRef != "" && !isClean(e.EvidenceRef) {
		fields["evidence_ref"] = "must be valid utf-8 without control characters"
	}
	if !to.Valid() {
		fields["to"] = "unknown status"
	} else {
		switch {
		case RequiresRejectionCode(to) && strings.TrimSpace(e.RejectionCode) == "":
			fields["rejection_code"] = "required for " + string(to)
		case !to.IsTerminal() && e.RejectionCode != "":
			fields["rejection_code"] = "only terminal transitions carry a rejection code"
		case e.RejectionCode != "" && !isClean(e.RejectionCode):
			fields["rejection_code"] = "must be valid utf-8 without control characters"
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "intent: invalid transition evidence").WithFields(fields)
}

// TransitionEvent is the payload of every intent.transitioned outbox event
// and of the audit events this package appends. A creation is published as
// a transition from "" into RECEIVED so consumers see one stream per intent.
type TransitionEvent struct {
	IntentID      string    `json:"intent_id"`
	TransitionID  string    `json:"transition_id,omitempty"`
	AccountID     string    `json:"account_id"`
	AgentID       string    `json:"agent_id,omitempty"`
	FromStatus    Status    `json:"from_status"`
	ToStatus      Status    `json:"to_status"`
	ActorType     string    `json:"actor_type"`
	ActorID       string    `json:"actor_id"`
	Reason        string    `json:"reason,omitempty"`
	EvidenceRef   string    `json:"evidence_ref,omitempty"`
	RejectionCode string    `json:"rejection_code,omitempty"`
	Action        Action    `json:"action"`
	Mode          Mode      `json:"mode"`
	Links         Links     `json:"links"`
	CorrelationID string    `json:"correlation_id"`
	OccurredAt    time.Time `json:"occurred_at"`
}

// Page is one page of ListForAccount. NextCursor is "" on the last page.
type Page struct {
	Items      []TradeIntent
	NextCursor string
}

// Repository persists trade_intents and intent_transitions. It holds no
// connection: every method runs on the caller's transaction or Querier. The
// outbox and audit writer are mandatory so a transition can never be
// recorded without its event and its audit row.
type Repository struct {
	clk    clock.Clock
	outbox *event.Outbox
	audit  audit.Writer
}

// NewRepository wires a Repository. All three dependencies are required.
func NewRepository(clk clock.Clock, outbox *event.Outbox, aw audit.Writer) *Repository {
	if clk == nil {
		panic("intent: NewRepository: nil clock")
	}
	if outbox == nil {
		panic("intent: NewRepository: nil outbox")
	}
	if aw == nil {
		panic("intent: NewRepository: nil audit writer")
	}
	return &Repository{clk: clk, outbox: outbox, audit: aw}
}

const intentColumns = `id, account_id::text, actor_type, actor_id, agent_id::text, strategy_version_id::text, prediction_id::text,
	action, instrument_id, notional_usd_minor, target_exposure_usd_minor, quantity::text, constraints, deadline, requested_at, received_at,
	idempotency_key, correlation_id, mode, status, coalesce(rejection_code, ''),
	coalesce(eligibility_decision_id::text, ''), coalesce(risk_decision_id::text, ''), coalesce(reservation_id::text, ''),
	coalesce(plan_id::text, ''), coalesce(order_id::text, ''), terminal_at, content_hash, created_at, updated_at`

func scanIntent(row pgx.Row) (TradeIntent, error) {
	var (
		t           TradeIntent
		actorType   string
		notional    *int64
		target      *int64
		quantity    *string
		constraints []byte
		deadline    *time.Time
	)
	err := row.Scan(&t.ID, &t.AccountID, &actorType, &t.ActorID, &t.AgentID, &t.StrategyVersionID, &t.PredictionID,
		&t.Action, &t.InstrumentID, &notional, &target, &quantity, &constraints, &deadline, &t.RequestedAt, &t.ReceivedAt,
		&t.IdempotencyKey, &t.CorrelationID, &t.Mode, &t.Status, &t.RejectionCode,
		&t.Links.EligibilityDecisionID, &t.Links.RiskDecisionID, &t.Links.ReservationID,
		&t.Links.PlanID, &t.Links.OrderID, &t.TerminalAt, &t.ContentHash, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return TradeIntent{}, err
	}
	t.ActorType = security.ActorType(actorType)
	if notional != nil {
		u := money.USDFromMinor(*notional)
		t.NotionalUSD = &u
	}
	if target != nil {
		u := money.USDFromMinor(*target)
		t.TargetExposureUSD = &u
	}
	if quantity != nil {
		q, err := money.ScanQuantity(*quantity)
		if err != nil {
			return TradeIntent{}, fmt.Errorf("intent: scan quantity: %w", err)
		}
		t.Quantity = &q
	}
	c, err := decodeConstraints(constraints)
	if err != nil {
		return TradeIntent{}, err
	}
	t.Constraints = c
	if deadline != nil {
		t.Deadline = deadline.UTC()
	}
	t.RequestedAt = t.RequestedAt.UTC()
	t.ReceivedAt = t.ReceivedAt.UTC()
	t.CreatedAt = t.CreatedAt.UTC()
	t.UpdatedAt = t.UpdatedAt.UTC()
	if t.TerminalAt != nil {
		u := t.TerminalAt.UTC()
		t.TerminalAt = &u
	}
	return t, nil
}

// constraintsJSON is the persisted form of Constraints (trade_intents.constraints).
type constraintsJSON struct {
	MaxSlippageBPS    money.BPS       `json:"max_slippage_bps"`
	MaxFeeBPS         money.BPS       `json:"max_fee_bps"`
	MaxPriceImpactBPS money.BPS       `json:"max_price_impact_bps"`
	MaxPrice          *priceJSON      `json:"max_price,omitempty"`
	MinReceive        *money.Quantity `json:"min_receive,omitempty"`
	AllowedVenues     []string        `json:"allowed_venues,omitempty"`
	QuoteFreshnessMS  int64           `json:"quote_freshness_ms"`
	ExecutionDeadline *time.Time      `json:"execution_deadline,omitempty"`
}

type priceJSON struct {
	Mantissa   money.Quantity `json:"mantissa"`
	Scale      int32          `json:"scale"`
	QuoteAsset string         `json:"quote_asset"`
	Source     string         `json:"source"`
	At         time.Time      `json:"at"`
}

func encodeConstraints(c Constraints) ([]byte, error) {
	j := constraintsJSON{
		MaxSlippageBPS:    c.MaxSlippageBPS,
		MaxFeeBPS:         c.MaxFeeBPS,
		MaxPriceImpactBPS: c.MaxPriceImpactBPS,
		MinReceive:        c.MinReceive,
		AllowedVenues:     c.AllowedVenues,
		QuoteFreshnessMS:  int64(c.QuoteFreshness / time.Millisecond),
	}
	if c.MaxPrice != nil {
		j.MaxPrice = &priceJSON{Mantissa: c.MaxPrice.Mantissa, Scale: c.MaxPrice.Scale, QuoteAsset: c.MaxPrice.QuoteAsset, Source: c.MaxPrice.Source, At: c.MaxPrice.At.UTC()}
	}
	if !c.ExecutionDeadline.IsZero() {
		d := c.ExecutionDeadline.UTC()
		j.ExecutionDeadline = &d
	}
	b, err := json.Marshal(j)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "intent: encode constraints")
	}
	return b, nil
}

func decodeConstraints(b []byte) (Constraints, error) {
	var j constraintsJSON
	if len(b) > 0 {
		if err := json.Unmarshal(b, &j); err != nil {
			return Constraints{}, errs.Wrap(err, errs.CodeInternal, "intent: stored constraints do not decode")
		}
	}
	c := Constraints{
		MaxSlippageBPS:    j.MaxSlippageBPS,
		MaxFeeBPS:         j.MaxFeeBPS,
		MaxPriceImpactBPS: j.MaxPriceImpactBPS,
		MinReceive:        j.MinReceive,
		AllowedVenues:     j.AllowedVenues,
		QuoteFreshness:    time.Duration(j.QuoteFreshnessMS) * time.Millisecond,
	}
	if j.MaxPrice != nil {
		c.MaxPrice = &money.Price{Mantissa: j.MaxPrice.Mantissa, Scale: j.MaxPrice.Scale, QuoteAsset: j.MaxPrice.QuoteAsset, Source: j.MaxPrice.Source, At: j.MaxPrice.At.UTC()}
	}
	if j.ExecutionDeadline != nil {
		c.ExecutionDeadline = j.ExecutionDeadline.UTC()
	}
	return c, nil
}

const insertIntentSQL = `
INSERT INTO trade_intents (
    id, account_id, actor_type, actor_id, agent_id, strategy_version_id, prediction_id, action, instrument_id,
    notional_usd_minor, target_exposure_usd_minor, quantity, constraints, deadline, requested_at, received_at,
    idempotency_key, correlation_id, mode, status, content_hash)
VALUES ($1, $2::uuid, $3, $4, $5::uuid, $6::uuid, $7::uuid, $8, $9,
        $10, $11, $12::numeric, $13::jsonb, $14, $15, $16,
        $17, $18, $19, 'RECEIVED', $20)
ON CONFLICT (account_id, idempotency_key) DO NOTHING
RETURNING ` + intentColumns

// Create validates t and inserts it as RECEIVED, appending an intent.received
// audit event on the account stream and an intent.transitioned ("" → RECEIVED)
// outbox event in the same transaction. It is idempotent on
// (account_id, idempotency_key): when a row already exists with the same
// ContentHash the stored intent is returned with Existing = true and nothing
// is written; with a different ContentHash the call fails with
// INVALID_IDEMPOTENCY_REUSE. Server-owned fields on t are ignored.
func (r *Repository) Create(ctx context.Context, tx pgx.Tx, t TradeIntent) (TradeIntent, error) {
	if tx == nil {
		return TradeIntent{}, errs.New(errs.CodeInternal, "intent: Create requires a transaction")
	}
	if err := t.Validate(); err != nil {
		return TradeIntent{}, err
	}
	hash := ContentHash(t)
	constraints, err := encodeConstraints(t.Constraints)
	if err != nil {
		return TradeIntent{}, err
	}
	now := r.clk.Now().UTC()
	var (
		notional, target *int64
		quantity         *string
		deadline         *time.Time
	)
	if t.NotionalUSD != nil {
		m := t.NotionalUSD.Minor()
		notional = &m
	}
	if t.TargetExposureUSD != nil {
		m := t.TargetExposureUSD.Minor()
		target = &m
	}
	if t.Quantity != nil {
		s := t.Quantity.String()
		quantity = &s
	}
	if !t.Deadline.IsZero() {
		d := t.Deadline.UTC()
		deadline = &d
	}
	created, err := scanIntent(tx.QueryRow(ctx, insertIntentSQL,
		t.ID, strings.ToLower(t.AccountID), string(t.ActorType), t.ActorID, t.AgentID, t.StrategyVersionID, t.PredictionID,
		string(t.Action), t.InstrumentID, notional, target, quantity, constraints, deadline, t.RequestedAt.UTC(), now,
		t.IdempotencyKey, t.CorrelationID, string(t.Mode), hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return r.replay(ctx, tx, t, hash)
	}
	if err != nil {
		return TradeIntent{}, classifyWriteError(err, "create")
	}
	ev := TransitionEvent{
		IntentID: created.ID.String(), AccountID: created.AccountID, AgentID: deref(created.AgentID),
		FromStatus: "", ToStatus: StatusReceived, ActorType: string(created.ActorType), ActorID: created.ActorID,
		Reason: "intent received", Action: created.Action, Mode: created.Mode, Links: created.Links,
		CorrelationID: created.CorrelationID, OccurredAt: now,
	}
	if err := r.record(ctx, tx, created, ev, AuditActionReceived, nil, "received"); err != nil {
		return TradeIntent{}, err
	}
	return created, nil
}

// replay resolves an (account_id, idempotency_key) collision.
func (r *Repository) replay(ctx context.Context, tx pgx.Tx, t TradeIntent, hash []byte) (TradeIntent, error) {
	existing, err := scanIntent(tx.QueryRow(ctx,
		`SELECT `+intentColumns+` FROM trade_intents WHERE account_id = $1::uuid AND idempotency_key = $2`,
		strings.ToLower(t.AccountID), t.IdempotencyKey))
	if err != nil {
		return TradeIntent{}, errs.Wrap(err, errs.CodeInternal, "intent: read existing intent for idempotency key")
	}
	if !bytes.Equal(existing.ContentHash, hash) {
		return TradeIntent{}, errs.New(errs.CodeInvalidIdempotencyReuse, "intent: idempotency key already used for a different intent").
			WithField("account_id", existing.AccountID).
			WithField("idempotency_key", t.IdempotencyKey).
			WithField("existing_intent_id", existing.ID.String())
	}
	existing.Existing = true
	return existing, nil
}

// Get returns one intent.
func (r *Repository) Get(ctx context.Context, q db.Querier, intentID IntentID) (TradeIntent, error) {
	if intentID.IsZero() {
		return TradeIntent{}, errs.New(errs.CodeValidationFailed, "intent: id required")
	}
	t, err := scanIntent(q.QueryRow(ctx, `SELECT `+intentColumns+` FROM trade_intents WHERE id = $1`, intentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TradeIntent{}, errs.New(errs.CodeNotFound, "intent not found").WithField("intent_id", intentID.String())
		}
		return TradeIntent{}, errs.Wrap(err, errs.CodeInternal, "intent: get")
	}
	return t, nil
}

// ListForAccount returns intents of an account ordered by (requested_at
// DESC, id DESC) with keyset pagination. cursor is "" for the first page or
// the NextCursor of the previous one; limit is clamped to 1..MaxPageSize and
// defaults to DefaultPageSize when zero.
func (r *Repository) ListForAccount(ctx context.Context, q db.Querier, accountID, cursor string, limit int) (Page, error) {
	if _, err := id.ParseAny(accountID); err != nil {
		return Page{}, errs.New(errs.CodeValidationFailed, "intent: account_id must be a canonical uuid")
	}
	limit = clampLimit(limit)
	var (
		afterAt *time.Time
		afterID *string
	)
	if cursor != "" {
		at, cid, err := decodeCursor(cursor)
		if err != nil {
			return Page{}, err
		}
		afterAt, afterID = &at, &cid
	}
	rows, err := q.Query(ctx, `SELECT `+intentColumns+` FROM trade_intents
		WHERE account_id = $1::uuid AND ($2::timestamptz IS NULL OR (requested_at, id) < ($2::timestamptz, $3::uuid))
		ORDER BY requested_at DESC, id DESC LIMIT $4`,
		strings.ToLower(accountID), afterAt, afterID, limit+1)
	if err != nil {
		return Page{}, errs.Wrap(err, errs.CodeInternal, "intent: list for account")
	}
	items, err := collect(rows)
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(last.RequestedAt, last.ID)
	}
	return page, nil
}

// ListOpen returns up to limit non-terminal intents, oldest first, for
// pipeline workers. limit is clamped like ListForAccount.
func (r *Repository) ListOpen(ctx context.Context, q db.Querier, limit int) ([]TradeIntent, error) {
	limit = clampLimit(limit)
	terminal := make([]string, 0, len(terminalStatuses))
	for _, s := range terminalStatuses {
		terminal = append(terminal, string(s))
	}
	rows, err := q.Query(ctx, `SELECT `+intentColumns+` FROM trade_intents
		WHERE status <> ALL($1::text[]) ORDER BY requested_at, id LIMIT $2`, terminal, limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "intent: list open")
	}
	return collect(rows)
}

func collect(rows pgx.Rows) ([]TradeIntent, error) {
	defer rows.Close()
	var out []TradeIntent
	for rows.Next() {
		t, err := scanIntent(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "intent: scan")
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "intent: rows")
	}
	return out, nil
}

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultPageSize
	case limit > MaxPageSize:
		return MaxPageSize
	}
	return limit
}

func encodeCursor(at time.Time, intentID IntentID) string {
	raw := strconv.FormatInt(at.UTC().UnixNano(), 10) + ":" + intentID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (time.Time, string, error) {
	invalid := errs.New(errs.CodeValidationFailed, "intent: invalid cursor")
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", invalid
	}
	nanos, cid, ok := strings.Cut(string(raw), ":")
	if !ok {
		return time.Time{}, "", invalid
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return time.Time{}, "", invalid
	}
	if _, err := ParseIntentID(cid); err != nil {
		return time.Time{}, "", invalid
	}
	return time.Unix(0, n).UTC(), cid, nil
}

// Transition moves an intent from its current status to `to`. Inside the
// caller's transaction it locks the row, checks Transitions, inserts the
// intent_transitions row (which is what lets the status UPDATE commit under
// migration 00603), updates status, rejection_code and terminal_at, enqueues
// an intent.transitioned outbox event and appends an intent.transitioned
// audit event on the account stream. Illegal transitions fail with
// INVALID_STATE_TRANSITION and write nothing.
func (r *Repository) Transition(ctx context.Context, tx pgx.Tx, intentID IntentID, to Status, ev TransitionEvidence) (TradeIntent, error) {
	if tx == nil {
		return TradeIntent{}, errs.New(errs.CodeInternal, "intent: Transition requires a transaction")
	}
	if err := ev.validate(to); err != nil {
		return TradeIntent{}, err
	}
	cur, err := r.lock(ctx, tx, intentID)
	if err != nil {
		return TradeIntent{}, err
	}
	if !CanTransition(cur.Status, to) {
		return TradeIntent{}, errs.Newf(errs.CodeInvalidStateTransition, "intent status %s -> %s is not allowed", cur.Status, to).
			WithField("intent_id", cur.ID.String()).WithField("from", string(cur.Status)).WithField("to", string(to))
	}
	now := r.clk.Now().UTC()
	transitionID := id.New[id.Any]()
	if _, err := tx.Exec(ctx, `INSERT INTO intent_transitions (id, intent_id, from_status, to_status, actor_type, actor_id, reason, evidence_ref, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9)`,
		transitionID, cur.ID, string(cur.Status), string(to), string(ev.ActorType), ev.ActorID, ev.Reason, ev.EvidenceRef, now); err != nil {
		return TradeIntent{}, classifyWriteError(err, "record transition")
	}
	var terminalAt *time.Time
	if to.IsTerminal() {
		terminalAt = &now
	}
	updated, err := scanIntent(tx.QueryRow(ctx, `UPDATE trade_intents
		SET status = $2, rejection_code = COALESCE(NULLIF($3, ''), rejection_code), terminal_at = COALESCE($4, terminal_at)
		WHERE id = $1 RETURNING `+intentColumns, cur.ID, string(to), ev.RejectionCode, terminalAt))
	if err != nil {
		return TradeIntent{}, classifyWriteError(err, "update status")
	}
	payload := TransitionEvent{
		IntentID: updated.ID.String(), TransitionID: transitionID.String(), AccountID: updated.AccountID, AgentID: deref(updated.AgentID),
		FromStatus: cur.Status, ToStatus: to, ActorType: string(ev.ActorType), ActorID: ev.ActorID,
		Reason: ev.Reason, EvidenceRef: ev.EvidenceRef, RejectionCode: ev.RejectionCode,
		Action: updated.Action, Mode: updated.Mode, Links: updated.Links, CorrelationID: updated.CorrelationID, OccurredAt: now,
	}
	if err := r.record(ctx, tx, updated, payload, AuditActionTransitions, &cur, transitionID.String()); err != nil {
		return TradeIntent{}, err
	}
	return updated, nil
}

// Link attaches decision, reservation, plan and order identifiers. Empty
// fields are left untouched. EligibilityDecisionID, RiskDecisionID and PlanID
// may be superseded by a later value (a FINAL risk decision follows the
// PRE_TRADE one; a re-plan supersedes the previous plan); ReservationID and
// OrderID are set once and a different value fails with CONFLICT. An
// intent.linked audit event records every change; the next transition event
// carries the links to outbox consumers.
func (r *Repository) Link(ctx context.Context, tx pgx.Tx, intentID IntentID, links Links) (TradeIntent, error) {
	if tx == nil {
		return TradeIntent{}, errs.New(errs.CodeInternal, "intent: Link requires a transaction")
	}
	if err := links.validate(); err != nil {
		return TradeIntent{}, err
	}
	cur, err := r.lock(ctx, tx, intentID)
	if err != nil {
		return TradeIntent{}, err
	}
	merged := cur.Links
	for _, f := range []struct {
		name     string
		dst      *string
		src      string
		setOnce  bool
		nonEmpty bool
	}{
		{"eligibility_decision_id", &merged.EligibilityDecisionID, links.EligibilityDecisionID, false, links.EligibilityDecisionID != ""},
		{"risk_decision_id", &merged.RiskDecisionID, links.RiskDecisionID, false, links.RiskDecisionID != ""},
		{"reservation_id", &merged.ReservationID, links.ReservationID, true, links.ReservationID != ""},
		{"plan_id", &merged.PlanID, links.PlanID, false, links.PlanID != ""},
		{"order_id", &merged.OrderID, links.OrderID, true, links.OrderID != ""},
	} {
		if !f.nonEmpty {
			continue
		}
		v := strings.ToLower(f.src)
		if f.setOnce && *f.dst != "" && *f.dst != v {
			return TradeIntent{}, errs.Newf(errs.CodeConflict, "intent: %s is already set", f.name).
				WithField("intent_id", cur.ID.String()).WithField("field", f.name).WithField("existing", *f.dst)
		}
		*f.dst = v
	}
	if merged == cur.Links {
		return cur, nil
	}
	updated, err := scanIntent(tx.QueryRow(ctx, `UPDATE trade_intents
		SET eligibility_decision_id = NULLIF($2, '')::uuid, risk_decision_id = NULLIF($3, '')::uuid, reservation_id = NULLIF($4, '')::uuid,
		    plan_id = NULLIF($5, '')::uuid, order_id = NULLIF($6, '')::uuid
		WHERE id = $1 RETURNING `+intentColumns,
		cur.ID, merged.EligibilityDecisionID, merged.RiskDecisionID, merged.ReservationID, merged.PlanID, merged.OrderID))
	if err != nil {
		return TradeIntent{}, classifyWriteError(err, "link")
	}
	now := r.clk.Now().UTC()
	payload := TransitionEvent{
		IntentID: updated.ID.String(), AccountID: updated.AccountID, AgentID: deref(updated.AgentID),
		FromStatus: updated.Status, ToStatus: updated.Status, ActorType: string(security.ActorSystem), ActorID: EventSource,
		Reason: "links attached", Action: updated.Action, Mode: updated.Mode, Links: updated.Links,
		CorrelationID: updated.CorrelationID, OccurredAt: now,
	}
	if err := r.appendAudit(ctx, tx, updated, payload, AuditActionLinked, &cur, now); err != nil {
		return TradeIntent{}, err
	}
	return updated, nil
}

func (l Links) validate() error {
	fields := map[string]any{}
	for _, f := range []struct{ name, value string }{
		{"eligibility_decision_id", l.EligibilityDecisionID},
		{"risk_decision_id", l.RiskDecisionID},
		{"reservation_id", l.ReservationID},
		{"plan_id", l.PlanID},
		{"order_id", l.OrderID},
	} {
		if f.value == "" {
			continue
		}
		if _, err := id.ParseAny(f.value); err != nil {
			fields[f.name] = "must be a canonical uuid"
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "intent: invalid links").WithFields(fields)
}

func (r *Repository) lock(ctx context.Context, tx pgx.Tx, intentID IntentID) (TradeIntent, error) {
	if intentID.IsZero() {
		return TradeIntent{}, errs.New(errs.CodeValidationFailed, "intent: id required")
	}
	cur, err := scanIntent(tx.QueryRow(ctx, `SELECT `+intentColumns+` FROM trade_intents WHERE id = $1 FOR UPDATE`, intentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TradeIntent{}, errs.New(errs.CodeNotFound, "intent not found").WithField("intent_id", intentID.String())
		}
		return TradeIntent{}, errs.Wrap(err, errs.CodeInternal, "intent: lock")
	}
	return cur, nil
}

// record enqueues the outbox event and appends the audit event for a state
// change. before is nil for a creation.
func (r *Repository) record(ctx context.Context, tx pgx.Tx, after TradeIntent, payload TransitionEvent, action string, before *TradeIntent, causation string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "intent: encode event")
	}
	env := event.Envelope{
		ID:            event.NewEventID().String(),
		Type:          string(event.TopicIntentTransitioned),
		SchemaVersion: event.TopicIntentTransitioned.Version(),
		Source:        EventSource,
		AggregateType: event.AggregateIntent,
		AggregateID:   after.ID.String(),
		CorrelationID: after.CorrelationID,
		CausationID:   causation,
		OccurredAt:    payload.OccurredAt,
		DedupKey:      string(event.TopicIntentTransitioned) + ":" + after.ID.String() + ":" + causation,
		Payload:       body,
	}
	if err := r.outbox.Enqueue(ctx, tx, string(event.TopicIntentTransitioned), env); err != nil {
		return err
	}
	return r.appendAudit(ctx, tx, after, payload, action, before, payload.OccurredAt)
}

func (r *Repository) appendAudit(ctx context.Context, tx pgx.Tx, after TradeIntent, payload TransitionEvent, action string, before *TradeIntent, now time.Time) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "intent: encode audit payload")
	}
	var beforeHash []byte
	if before != nil {
		beforeHash, err = stateHash(*before)
		if err != nil {
			return err
		}
	}
	afterHash, err := stateHash(after)
	if err != nil {
		return err
	}
	_, err = r.audit.Append(ctx, tx, audit.Event{
		Stream:        audit.AccountStream(after.AccountID),
		ActorType:     payload.ActorType,
		ActorID:       payload.ActorID,
		Action:        action,
		ResourceType:  AuditResourceType,
		ResourceID:    after.ID.String(),
		BeforeHash:    beforeHash,
		AfterHash:     afterHash,
		RequestID:     observability.RequestID(ctx),
		CorrelationID: after.CorrelationID,
		Reason:        payload.Reason,
		EvidenceRef:   payload.EvidenceRef,
		Payload:       body,
		OccurredAt:    now,
	})
	return err
}

// stateHash digests the lifecycle state of an intent for audit before/after
// hashes; the content hash covers the immutable part.
func stateHash(t TradeIntent) ([]byte, error) {
	b, err := audit.CanonicalJSON(struct {
		ContentHash   []byte     `json:"content_hash"`
		Status        Status     `json:"status"`
		RejectionCode string     `json:"rejection_code"`
		Links         Links      `json:"links"`
		TerminalAt    *time.Time `json:"terminal_at"`
	}{t.ContentHash, t.Status, t.RejectionCode, t.Links, t.TerminalAt})
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "intent: state hash")
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// classifyWriteError maps database errors to stable codes while keeping the
// SQLSTATE reachable for retry classification.
func classifyWriteError(err error, op string) error {
	switch {
	case db.IsForeignKeyViolation(err):
		return errs.Wrap(err, errs.CodeNotFound, "intent: referenced entity does not exist").
			WithField("constraint", db.ConstraintName(err))
	case db.IsCheckViolation(err):
		return errs.Wrap(err, errs.CodeValidationFailed, "intent: rejected by a table constraint").
			WithField("constraint", db.ConstraintName(err))
	case db.IsUniqueViolation(err):
		return errs.Wrap(err, errs.CodeConflict, "intent: duplicate identifier").
			WithField("constraint", db.ConstraintName(err))
	case IsImmutableIntent(err):
		return errs.Wrap(err, errs.CodeConflict, "intent: identity, economics and mode are immutable")
	}
	switch db.SQLState(err) {
	case "AG001", "AG002", "AG003":
		return errs.Wrap(err, errs.CodeValidationFailed, "intent: agent linkage rejected by the prediction guard").
			WithField("sqlstate", db.SQLState(err))
	case "AU001":
		return errs.Wrap(err, errs.CodeInternal, "intent: status change without a transition row")
	}
	return errs.Wrap(err, errs.CodeInternal, "intent: "+op)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
