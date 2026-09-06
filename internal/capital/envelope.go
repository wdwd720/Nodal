package capital

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// EnvelopeAdmin is the only path that creates envelopes or changes their
// authority fields. Every method requires a non-agent security.Principal
// in ctx that is scoped to the envelope's account (security.RequireAccount)
// and writes a capital_envelope_changes audit row for every change.
type EnvelopeAdmin interface {
	Create(ctx context.Context, tx pgx.Tx, e Envelope) (Envelope, error)
	Update(ctx context.Context, tx pgx.Tx, id EnvelopeID, p EnvelopeAuthorityPatch) (Envelope, error)
	SetStatus(ctx context.Context, tx pgx.Tx, id EnvelopeID, to EnvelopeStatus, reason string) (Envelope, error)
	Get(ctx context.Context, q db.Querier, id EnvelopeID) (Envelope, error)
	ListForAccount(ctx context.Context, q db.Querier, accountID string) ([]Envelope, error)
}

// System actor recorded on limit-driven status changes.
const (
	systemActorType = security.ActorSystem
	systemActorID   = "capital.pnl"
)

// EnvelopeService implements EnvelopeAdmin against PostgreSQL.
type EnvelopeService struct {
	clk  clock.Clock
	emit Emitter
}

var _ EnvelopeAdmin = (*EnvelopeService)(nil)

// NewEnvelopeService wires an EnvelopeService; both dependencies are
// mandatory (see NewService).
func NewEnvelopeService(clk clock.Clock, em Emitter) *EnvelopeService {
	if clk == nil {
		panic("capital: NewEnvelopeService: nil clock")
	}
	if em == nil {
		panic("capital: NewEnvelopeService: nil emitter")
	}
	return &EnvelopeService{clk: clk, emit: em}
}

// requireAdministrator fails closed: no principal → UNAUTHENTICATED; an
// invalid principal or an AGENT → FORBIDDEN. It runs before any database
// access so an agent cannot even cause a lock to be taken.
func requireAdministrator(ctx context.Context) (security.Principal, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return security.Principal{}, errs.New(errs.CodeUnauthenticated, "authentication required")
	}
	if err := p.Validate(); err != nil {
		return security.Principal{}, errs.Wrap(err, errs.CodeForbidden, "invalid principal")
	}
	if p.IsAgent() {
		return security.Principal{}, errs.New(errs.CodeForbidden, "agents cannot administer capital envelopes").
			WithField("actor_type", string(p.ActorType))
	}
	return p, nil
}

// requireAccount applies tenant scoping and maps the security sentinels
// onto stable codes. The API layer may render FORBIDDEN as NOT_FOUND.
func requireAccount(ctx context.Context, account accounts.AccountID) error {
	err := security.RequireAccount(ctx, account.String())
	switch {
	case err == nil:
		return nil
	case errors.Is(err, security.ErrUnauthenticated):
		return errs.Wrap(err, errs.CodeUnauthenticated, "authentication required")
	default:
		return errs.Wrap(err, errs.CodeForbidden, "principal is not scoped to this account").
			WithField("account_id", account.String())
	}
}

// Create inserts a new envelope. Available is set to Allocation (an unset
// Available is normalised, any other value is rejected), the status
// defaults to DRAFT, EffectiveAt defaults to now, and the daily-loss reset
// is the next UTC midnight. The creating principal is recorded and every
// authority field is written to the audit row as {null → value}.
func (s *EnvelopeService) Create(ctx context.Context, tx pgx.Tx, e Envelope) (Envelope, error) {
	p, err := requireAdministrator(ctx)
	if err != nil {
		return Envelope{}, err
	}
	if e.AccountID.IsZero() {
		return Envelope{}, errs.New(errs.CodeValidationFailed, "invalid envelope").WithField("problems", []string{"account_id required"})
	}
	if err := requireAccount(ctx, e.AccountID); err != nil {
		return Envelope{}, err
	}
	now := s.clk.Now()
	if e.ID.IsZero() {
		e.ID = NewEnvelopeID()
	}
	if e.Available.IsZero() {
		e.Available = e.Allocation
	}
	if e.Status == "" {
		e.Status = EnvelopeDraft
	}
	if e.EffectiveAt.IsZero() {
		e.EffectiveAt = now
	}
	e.EffectiveAt = e.EffectiveAt.UTC()
	if e.ExpiresAt != nil {
		t := e.ExpiresAt.UTC()
		e.ExpiresAt = &t
	}
	e.AllowedInstruments = normalizeList(e.AllowedInstruments)
	e.AllowedAssetClasses = normalizeList(e.AllowedAssetClasses)
	e.AllowedVenues = normalizeList(e.AllowedVenues)
	e.CreatedByActorType = p.ActorType
	e.CreatedByActorID = p.SubjectID
	resetFrom := now
	if e.EffectiveAt.After(now) {
		resetFrom = e.EffectiveAt
	}
	e.DailyLossResetAt = nextUTCMidnight(resetFrom)
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}

	created, err := scanEnvelope(tx.QueryRow(ctx, `INSERT INTO capital_envelopes
			(id, account_id, agent_id, strategy_version_id, settlement_asset_id,
			 allocation_usd_minor, available_usd_minor, reserved_usd_minor, deployed_usd_minor,
			 realized_pnl_usd_minor, realized_loss_usd_minor, current_drawdown_usd_minor, daily_loss_usd_minor, daily_loss_reset_at,
			 max_daily_loss_usd_minor, max_drawdown_usd_minor, max_single_trade_usd_minor, max_position_usd_minor,
			 allowed_instruments, allowed_asset_classes, allowed_venues,
			 max_model_spend_usd_minor, max_data_spend_usd_minor, max_order_rate_per_hour, policy_version, status,
			 effective_at, expires_at, version, created_by_actor_type, created_by_actor_id, created_at, updated_at)
		VALUES ($1, $2, $3::uuid, $4::uuid, $5,
			$6, $7, 0, 0,
			0, 0, 0, 0, $8,
			$9, $10, $11, $12,
			($13::text[])::uuid[], $14, $15,
			$16, $17, $18, $19, $20,
			$21, $22, 1, $23, $24, $25, $25)
		RETURNING `+envelopeColumns,
		e.ID, e.AccountID, e.AgentID, e.StrategyVersionID, e.SettlementAssetID,
		e.Allocation, e.Available, e.DailyLossResetAt,
		e.MaxDailyLoss, e.MaxDrawdown, e.MaxSingleTrade, e.MaxPosition,
		e.AllowedInstruments, e.AllowedAssetClasses, e.AllowedVenues,
		e.MaxModelSpend, e.MaxDataSpend, e.MaxOrderRatePerHour, e.PolicyVersion, string(e.Status),
		e.EffectiveAt, e.ExpiresAt, string(e.CreatedByActorType), e.CreatedByActorID, now))
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Envelope{}, errs.Wrap(err, errs.CodeConflict, "envelope already exists").WithField("envelope_id", e.ID.String())
		}
		if db.IsForeignKeyViolation(err) {
			return Envelope{}, errs.Wrap(err, errs.CodeValidationFailed, "envelope references an unknown account or asset")
		}
		return Envelope{}, dbErr("insert envelope", err)
	}
	changes := authoritySnapshot(created)
	if err := insertEnvelopeChange(ctx, tx, created.ID, changes, p.ActorType, p.SubjectID, "envelope created", "", now); err != nil {
		return Envelope{}, err
	}
	if err := s.emit.Emit(ctx, tx, TopicEnvelopeCreated, newEnvelopeEvent(created, changes, "envelope created", now)); err != nil {
		return Envelope{}, fmt.Errorf("capital: emit %s: %w", TopicEnvelopeCreated, err)
	}
	return created, nil
}

// authoritySnapshot renders every authority field as a {null → value}
// change, for the creation audit row.
func authoritySnapshot(e Envelope) map[string]FieldChange {
	return map[string]FieldChange{
		"allocation_usd_minor":       {From: nil, To: e.Allocation.Minor()},
		"available_usd_minor":        {From: nil, To: e.Available.Minor()},
		"max_daily_loss_usd_minor":   {From: nil, To: e.MaxDailyLoss.Minor()},
		"max_drawdown_usd_minor":     {From: nil, To: e.MaxDrawdown.Minor()},
		"max_single_trade_usd_minor": {From: nil, To: e.MaxSingleTrade.Minor()},
		"max_position_usd_minor":     {From: nil, To: e.MaxPosition.Minor()},
		"allowed_instruments":        {From: nil, To: e.AllowedInstruments},
		"allowed_asset_classes":      {From: nil, To: e.AllowedAssetClasses},
		"allowed_venues":             {From: nil, To: e.AllowedVenues},
		"max_model_spend_usd_minor":  {From: nil, To: e.MaxModelSpend.Minor()},
		"max_data_spend_usd_minor":   {From: nil, To: e.MaxDataSpend.Minor()},
		"max_order_rate_per_hour":    {From: nil, To: e.MaxOrderRatePerHour},
		"policy_version":             {From: nil, To: e.PolicyVersion},
		"status":                     {From: nil, To: string(e.Status)},
		"effective_at":               {From: nil, To: timeValue(&e.EffectiveAt)},
		"expires_at":                 {From: nil, To: timeValue(e.ExpiresAt)},
	}
}

// authorize checks the principal, then the envelope's account, before any
// lock is taken. It returns the principal for the audit row.
func (s *EnvelopeService) authorize(ctx context.Context, q db.Querier, eid EnvelopeID) (security.Principal, error) {
	p, err := requireAdministrator(ctx)
	if err != nil {
		return security.Principal{}, err
	}
	account, err := envelopeAccount(ctx, q, eid)
	if err != nil {
		return security.Principal{}, err
	}
	if err := requireAccount(ctx, account); err != nil {
		return security.Principal{}, err
	}
	return p, nil
}

// Update applies an authority patch under the envelope's row lock. A patch
// that changes nothing is a no-op (no audit row, no version bump). A
// REVOKED envelope is immutable.
func (s *EnvelopeService) Update(ctx context.Context, tx pgx.Tx, eid EnvelopeID, p EnvelopeAuthorityPatch) (Envelope, error) {
	principal, err := s.authorize(ctx, tx, eid)
	if err != nil {
		return Envelope{}, err
	}
	if err := p.Validate(); err != nil {
		return Envelope{}, err
	}
	now := s.clk.Now()
	cur, err := getEnvelope(ctx, tx, eid, true)
	if err != nil {
		return Envelope{}, err
	}
	if cur.Status == EnvelopeRevoked {
		return Envelope{}, errs.New(errs.CodeInvalidStateTransition, "a revoked envelope cannot be changed").
			WithField("envelope_id", eid.String()).WithField("from", string(cur.Status))
	}
	next, changes, err := applyAuthorityPatch(cur, p)
	if err != nil {
		return Envelope{}, err
	}
	if len(changes) == 0 {
		return cur, nil
	}
	updated, err := scanEnvelope(tx.QueryRow(ctx, `UPDATE capital_envelopes SET
			allocation_usd_minor = $2, available_usd_minor = $3,
			max_daily_loss_usd_minor = $4, max_drawdown_usd_minor = $5, max_single_trade_usd_minor = $6, max_position_usd_minor = $7,
			allowed_instruments = ($8::text[])::uuid[], allowed_asset_classes = $9, allowed_venues = $10,
			max_model_spend_usd_minor = $11, max_data_spend_usd_minor = $12, max_order_rate_per_hour = $13, policy_version = $14,
			effective_at = $15, expires_at = $16, version = version + 1
		WHERE id = $1 RETURNING `+envelopeColumns,
		eid, next.Allocation, next.Available,
		next.MaxDailyLoss, next.MaxDrawdown, next.MaxSingleTrade, next.MaxPosition,
		next.AllowedInstruments, next.AllowedAssetClasses, next.AllowedVenues,
		next.MaxModelSpend, next.MaxDataSpend, next.MaxOrderRatePerHour, next.PolicyVersion,
		next.EffectiveAt, next.ExpiresAt))
	if err != nil {
		return Envelope{}, dbErr("update envelope", err)
	}
	if err := insertEnvelopeChange(ctx, tx, eid, changes, principal.ActorType, principal.SubjectID, p.Reason, p.ApprovalID, now); err != nil {
		return Envelope{}, err
	}
	if err := s.emit.Emit(ctx, tx, TopicEnvelopeUpdated, newEnvelopeEvent(updated, changes, p.Reason, now)); err != nil {
		return Envelope{}, fmt.Errorf("capital: emit %s: %w", TopicEnvelopeUpdated, err)
	}
	return updated, nil
}

// SetStatus applies a reasoned status transition. Activation additionally
// requires the validity window to be open and no loss limit to be breached
// (after applying a due daily reset): an EXHAUSTED envelope only comes back
// once its limits were raised or its daily counter rolled over.
func (s *EnvelopeService) SetStatus(ctx context.Context, tx pgx.Tx, eid EnvelopeID, to EnvelopeStatus, reason string) (Envelope, error) {
	principal, err := s.authorize(ctx, tx, eid)
	if err != nil {
		return Envelope{}, err
	}
	if !to.Valid() {
		return Envelope{}, errs.Newf(errs.CodeValidationFailed, "unknown envelope status %q", to)
	}
	if strings.TrimSpace(reason) == "" {
		return Envelope{}, errs.New(errs.CodeValidationFailed, "reason required")
	}
	now := s.clk.Now()
	cur, err := getEnvelope(ctx, tx, eid, true)
	if err != nil {
		return Envelope{}, err
	}
	if !CanTransitionEnvelope(cur.Status, to) {
		return Envelope{}, errs.Newf(errs.CodeInvalidStateTransition, "envelope status %s -> %s is not allowed", cur.Status, to).
			WithField("envelope_id", eid.String()).WithField("from", string(cur.Status)).WithField("to", string(to))
	}
	next := cur
	if to == EnvelopeActive {
		next, _ = withDailyReset(next, now)
		if next.ExpiredAt(now) {
			return Envelope{}, errs.New(errs.CodeInvalidStateTransition, "envelope validity window has closed; extend expires_at first").
				WithField("envelope_id", eid.String()).WithField("expires_at", timeValue(next.ExpiresAt))
		}
		if limit := limitBreached(next); limit != "" {
			return Envelope{}, errs.Newf(errs.CodeInvalidStateTransition, "envelope %s limit is still breached", limit).
				WithField("envelope_id", eid.String()).WithField("limit", limit).
				WithField("daily_loss_usd_minor", next.DailyLoss.Minor()).
				WithField("current_drawdown_usd_minor", next.CurrentDrawdown.Minor())
		}
	}
	next.Status = to
	updated, err := scanEnvelope(tx.QueryRow(ctx, `UPDATE capital_envelopes SET status = $2, daily_loss_usd_minor = $3, daily_loss_reset_at = $4, version = version + 1
		WHERE id = $1 RETURNING `+envelopeColumns, eid, string(to), next.DailyLoss, next.DailyLossResetAt))
	if err != nil {
		return Envelope{}, dbErr("update envelope status", err)
	}
	changes := map[string]FieldChange{"status": {From: string(cur.Status), To: string(to)}}
	if err := insertEnvelopeChange(ctx, tx, eid, changes, principal.ActorType, principal.SubjectID, reason, "", now); err != nil {
		return Envelope{}, err
	}
	if err := s.emit.Emit(ctx, tx, TopicEnvelopeStatusChanged, newEnvelopeEvent(updated, changes, reason, now)); err != nil {
		return Envelope{}, fmt.Errorf("capital: emit %s: %w", TopicEnvelopeStatusChanged, err)
	}
	return updated, nil
}

// Get returns an envelope visible to the principal.
func (s *EnvelopeService) Get(ctx context.Context, q db.Querier, eid EnvelopeID) (Envelope, error) {
	if _, err := requireAdministrator(ctx); err != nil {
		return Envelope{}, err
	}
	e, err := getEnvelope(ctx, q, eid, false)
	if err != nil {
		return Envelope{}, err
	}
	if err := requireAccount(ctx, e.AccountID); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// ListForAccount returns the account's envelopes, oldest first.
func (s *EnvelopeService) ListForAccount(ctx context.Context, q db.Querier, accountID string) ([]Envelope, error) {
	if _, err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	account, err := accounts.ParseAccountID(accountID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "account_id must be a canonical uuid")
	}
	if err := requireAccount(ctx, account); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+envelopeColumns+` FROM capital_envelopes WHERE account_id = $1 ORDER BY created_at, id`, account)
	if err != nil {
		return nil, dbErr("list envelopes", err)
	}
	defer rows.Close()
	out := []Envelope{}
	for rows.Next() {
		e, err := scanEnvelope(rows)
		if err != nil {
			return nil, dbErr("scan envelope", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("iterate envelopes", err)
	}
	return out, nil
}

// ApplyRealizedPnL applies realized P&L (pnlUSD in minor units, negative
// for a loss) to the envelope's counters under its row lock and enforces
// the loss limits: when daily_loss ≥ max_daily_loss or current_drawdown ≥
// max_drawdown an ACTIVE envelope becomes EXHAUSTED. This is limit
// enforcement driven by ledger facts (fills), not an authority change: no
// principal is required, the flip is recorded in capital_envelope_changes
// with the SYSTEM actor, and capital.envelope.exhausted is emitted so the
// agent runtime stops proposing. The daily counter resets at the first
// UTC midnight after daily_loss_reset_at has passed.
func (s *EnvelopeService) ApplyRealizedPnL(ctx context.Context, tx pgx.Tx, eid EnvelopeID, pnlUSD int64, now time.Time) (Envelope, error) {
	if now.IsZero() {
		return Envelope{}, errs.New(errs.CodeValidationFailed, "now required")
	}
	now = now.UTC()
	cur, err := getEnvelope(ctx, tx, eid, true)
	if err != nil {
		return Envelope{}, err
	}
	next, outcome, err := applyRealizedPnL(cur, money.USDFromMinor(pnlUSD), now)
	if err != nil {
		return Envelope{}, err
	}
	updated, err := scanEnvelope(tx.QueryRow(ctx, `UPDATE capital_envelopes SET
			realized_pnl_usd_minor = $2, realized_loss_usd_minor = $3, current_drawdown_usd_minor = $4,
			daily_loss_usd_minor = $5, daily_loss_reset_at = $6, status = $7, version = version + 1
		WHERE id = $1 RETURNING `+envelopeColumns,
		eid, next.RealizedPnL, next.RealizedLoss, next.CurrentDrawdown, next.DailyLoss, next.DailyLossResetAt, string(next.Status)))
	if err != nil {
		return Envelope{}, dbErr("apply realized pnl", err)
	}
	if err := s.emit.Emit(ctx, tx, TopicEnvelopePnLApplied, newEnvelopeEvent(updated, nil, "", now)); err != nil {
		return Envelope{}, fmt.Errorf("capital: emit %s: %w", TopicEnvelopePnLApplied, err)
	}
	if outcome.Exhausted {
		reason := "limit reached: " + outcome.LimitHit
		changes := map[string]FieldChange{"status": {From: string(cur.Status), To: string(updated.Status)}}
		if err := insertEnvelopeChange(ctx, tx, eid, changes, systemActorType, systemActorID, reason, "", now); err != nil {
			return Envelope{}, err
		}
		if err := s.emit.Emit(ctx, tx, TopicEnvelopeExhausted, newEnvelopeEvent(updated, changes, reason, now)); err != nil {
			return Envelope{}, fmt.Errorf("capital: emit %s: %w", TopicEnvelopeExhausted, err)
		}
	}
	return updated, nil
}
