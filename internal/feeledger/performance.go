package feeledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// strategyRefPattern is a strategy reference: short enough that
// "<account id>/<strategy>" is a valid statement account reference.
var strategyRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,89}$`)

// AccountRef is the account reference a period's statement names: the
// account and the strategy, so a statement can never be read as another
// pair's.
func AccountRef(account accounts.AccountID, strategy string) string {
	return account.String() + "/" + strategy
}

// Flow is one deposit (positive) or withdrawal (negative) of a period.
type Flow struct {
	ID             FlowID
	PeriodID       PeriodID
	Seq            int
	At             time.Time
	Amount         money.USD
	NAVBefore      money.USD
	Actor          econ.Actor
	IdempotencyKey string
}

// Line is one persisted line of a period: a valuation's change in the
// running accrual, the period-end true-up, or the crystallization.
type Line struct {
	ID                   LineID
	PeriodID             PeriodID
	Seq                  int
	Kind                 fees.PerformanceLineKind
	At                   time.Time
	NAV                  money.USD
	HWMAdjusted          money.USD
	HurdleThreshold      money.USD
	FeeBase              money.USD
	Amount               money.USD
	AccrualBefore        money.USD
	AccrualAfter         money.USD
	JournalTransactionID *ledger.TransactionID
	EffectID             econ.EffectID
}

// Period is one performance fee period of an (account, strategy) pair.
type Period struct {
	ID          PeriodID
	AccountID   accounts.AccountID
	StrategyRef string
	Seq         int
	PreviousID  *PeriodID
	Mode        gates.Mode
	FeeAsset    assets.AssetID
	// Terms are the agreed terms with every recorded flow.
	Terms           fees.PerformanceTerms
	TermsHash       string
	Status          PeriodStatus
	Accrued         money.USD
	LastValuationAt *time.Time
	Flows           []Flow
	Lines           []Line
	// The crystallization outcome, once there is one.
	Statement                *fees.PerformanceStatement
	StatementBytes           []byte
	StatementHash            string
	CrystallizationJournalID *ledger.TransactionID
	CrystallizationEffectID  *econ.EffectID
	CrystallizedAt           *time.Time
	ReversalJournalID        *ledger.TransactionID
	ReversalReasonCode       string
	ReversedAt               *time.Time
	// GrossReturnBPS and NetReturnBPS are the period's Modified Dietz
	// returns (gross and net of the fee) in basis points, rounded toward
	// zero; nil while the period is open or when no capital was at work.
	GrossReturnBPS *money.BPS
	NetReturnBPS   *money.BPS
}

// OpenPeriodRequest opens the next performance period of an (account,
// strategy) pair.
type OpenPeriodRequest struct {
	AccountID   accounts.AccountID
	StrategyRef string
	// Mode is PAPER (default) or SANDBOX.
	Mode gates.Mode
	// FeeAsset is the SIMULATED asset the fee is accounted in (default the
	// sandbox dollar); it needs at least two decimals to carry cents.
	FeeAsset     assets.AssetID
	TermsVersion string
	// PeriodStart and OpeningNAV are stated for a pair's first period only;
	// every later period opens where the previous crystallized statement
	// closed (NextTerms), and may leave them zero.
	PeriodStart     time.Time
	PeriodEnd       time.Time
	OpeningNAV      money.USD
	HurdleAnnualBPS money.BPS
	FeeRateBPS      money.BPS
	IdempotencyKey  string
	Actor           econ.Actor
}

// OpenPeriod opens a period. A pair's first period starts with its
// high-water mark at its opening NAV; every later one chains from the latest
// crystallized period's statement -- its end, its net closing NAV, its new
// high-water mark -- under the terms version and rates the request states.
// One period of a pair is open at a time (and the database refuses a second,
// FE006). It is idempotent on (account, idempotency key).
func (s *Service) OpenPeriod(ctx context.Context, tx pgx.Tx, r OpenPeriodRequest) (Period, error) {
	if err := requireActor(r.Actor); err != nil {
		return Period{}, err
	}
	if err := validKey(r.IdempotencyKey); err != nil {
		return Period{}, err
	}
	if !strategyRefPattern.MatchString(r.StrategyRef) {
		return Period{}, errs.New(errs.CodeValidationFailed, "feeledger: a strategy reference is 1..90 bytes of [A-Za-z0-9_.:/-]")
	}
	mode := r.Mode
	if mode == "" {
		mode = gates.ModePaper
	}
	if err := s.requireMode(ctx, tx, gates.PerformanceFees, mode); err != nil {
		return Period{}, err
	}
	reqHash, err := econ.RequestHash(map[string]any{
		"account_id": r.AccountID.String(), "strategy_ref": r.StrategyRef, "mode": string(mode),
		"fee_asset": r.FeeAsset.String(), "terms_version": r.TermsVersion, "period_start": r.PeriodStart.UTC(),
		"period_end": r.PeriodEnd.UTC(), "opening_nav": r.OpeningNAV, "hurdle_annual_bps": int64(r.HurdleAnnualBPS),
		"fee_rate_bps": int64(r.FeeRateBPS),
	})
	if err != nil {
		return Period{}, err
	}
	var prior PeriodID
	var priorHash []byte
	switch err := tx.QueryRow(ctx, `SELECT id, request_hash FROM performance_fee_periods WHERE account_id = $1 AND idempotency_key = $2`,
		r.AccountID, r.IdempotencyKey).Scan(&prior, &priorHash); {
	case err == nil:
		if !bytes.Equal(priorHash, reqHash) {
			return Period{}, errs.New(errs.CodeInvalidIdempotencyReuse, "feeledger: this idempotency key opened a different period").
				WithField("period_id", prior.String())
		}
		return s.loadPeriod(ctx, tx, prior, false)
	case !errors.Is(err, pgx.ErrNoRows):
		return Period{}, dbErr("look up period", err)
	}
	if err := lockKey(ctx, tx, "perf:"+AccountRef(r.AccountID, r.StrategyRef)); err != nil {
		return Period{}, err
	}
	feeAsset := r.FeeAsset
	if feeAsset.IsZero() {
		feeAsset = econ.SandboxDollarID()
	}
	asset, err := s.assets.Get(ctx, tx, feeAsset)
	if err != nil {
		return Period{}, err
	}
	if asset.Kind != assets.KindSimulated || asset.ValueDomain != valuedomain.Simulated || asset.Decimals < 2 {
		return Period{}, errs.New(errs.CodeValidationFailed,
			"feeledger: a performance fee is accounted in a SIMULATED asset with at least two decimals")
	}
	// A period's accruals and its crystallization post
	// PERFORMANCE_FEE_ACCRUAL and PERFORMANCE_FEE_CRYSTALLIZATION in its fee
	// asset, kinds the database refuses for a platform stable asset or a
	// bridged twin (SU001, migration 00763). Opening the period is the one
	// entry to both: Accrue, Crystallize, CancelPeriod and
	// ReverseCrystallization act on a period that exists, whose fee asset
	// never changes. So the fee asset is refused here, before the period is
	// written.
	if err := econ.RefuseSupplyControlled(ctx, tx, asset.ID, "feeledger: a performance-fee period accounted in it",
		"its accruals and its crystallization would be PERFORMANCE_FEE_ACCRUAL and PERFORMANCE_FEE_CRYSTALLIZATION journals "+
			"in it, which the database refuses for such an asset (SU001, migration 00763): the period could never accrue or "+
			"crystallize its fee"); err != nil {
		return Period{}, err
	}
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM performance_fee_periods WHERE account_id = $1 AND strategy_ref = $2 AND status = 'OPEN'`,
		r.AccountID, r.StrategyRef).Scan(&open); err != nil {
		return Period{}, dbErr("look up open period", err)
	}
	if open > 0 {
		return Period{}, errs.New(errs.CodeConflict, "feeledger: this account and strategy already have an open period")
	}
	terms := fees.PerformanceTerms{
		AccountRef: AccountRef(r.AccountID, r.StrategyRef), TermsVersion: r.TermsVersion, PeriodStart: r.PeriodStart.UTC(),
		PeriodEnd: r.PeriodEnd.UTC(), OpeningNAV: r.OpeningNAV, PriorHWM: r.OpeningNAV, HurdleAnnualBPS: r.HurdleAnnualBPS,
		FeeRateBPS: r.FeeRateBPS, Crystallization: fees.CrystallizeAtPeriodEnd,
	}
	var previous *PeriodID
	var prevID PeriodID
	switch err := tx.QueryRow(ctx, `SELECT id FROM performance_fee_periods WHERE account_id = $1 AND strategy_ref = $2
		AND status = 'CRYSTALLIZED' ORDER BY period_seq DESC LIMIT 1`, r.AccountID, r.StrategyRef).Scan(&prevID); {
	case err == nil:
		prev, err := s.loadPeriod(ctx, tx, prevID, false)
		if err != nil {
			return Period{}, err
		}
		next := prev.Statement.NextTerms(r.PeriodEnd.UTC(), nil)
		if (!r.PeriodStart.IsZero() && !r.PeriodStart.UTC().Equal(next.PeriodStart)) ||
			(!r.OpeningNAV.IsZero() && !r.OpeningNAV.Equal(next.OpeningNAV)) {
			return Period{}, rejection(errs.CodeValidationFailed, ReasonChainBroken,
				"a later period opens where the previous statement closed")
		}
		terms.PeriodStart, terms.OpeningNAV, terms.PriorHWM = next.PeriodStart, next.OpeningNAV, next.PriorHWM
		previous = &prevID
	case !errors.Is(err, pgx.ErrNoRows):
		return Period{}, dbErr("look up previous period", err)
	}
	if err := terms.Validate(); err != nil {
		return Period{}, err
	}
	th, err := termsHash(terms, asset.ID, mode)
	if err != nil {
		return Period{}, err
	}
	var seq int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(period_seq), 0) + 1 FROM performance_fee_periods WHERE account_id = $1 AND strategy_ref = $2`,
		r.AccountID, r.StrategyRef).Scan(&seq); err != nil {
		return Period{}, dbErr("next period", err)
	}
	pid := id.New[periodKind]()
	if _, err := tx.Exec(ctx, `INSERT INTO performance_fee_periods (id, account_id, strategy_ref, period_seq, previous_period_id, mode,
		fee_asset_id, terms_version, period_start, period_end, opening_nav_minor, prior_hwm_minor, hurdle_annual_bps, fee_rate_bps,
		crystallization, prior_accrued_fee_minor, terms_hash, status, accrued_fee_minor, opened_by_actor_type, opened_by_actor_id,
		idempotency_key, request_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,0,$16,'OPEN',0,$17,$18,$19,$20)`,
		pid, r.AccountID, r.StrategyRef, seq, previous, mode, asset.ID, terms.TermsVersion, terms.PeriodStart, terms.PeriodEnd,
		terms.OpeningNAV.Minor(), terms.PriorHWM.Minor(), int(terms.HurdleAnnualBPS), int(terms.FeeRateBPS), terms.Crystallization,
		th, r.Actor.Type, r.Actor.ID, r.IdempotencyKey, reqHash); err != nil {
		return Period{}, dbErr("open performance period", err)
	}
	return s.loadPeriod(ctx, tx, pid, false)
}

// termsHash is the hex sha256 of the canonical agreed terms of a period,
// flows excluded (they are recorded as they happen).
func termsHash(t fees.PerformanceTerms, feeAsset assets.AssetID, mode gates.Mode) (string, error) {
	b, err := audit.CanonicalJSON(map[string]any{
		"kind": "feeledger.performance_terms.v1", "account_ref": t.AccountRef, "terms_version": t.TermsVersion,
		"period_start": t.PeriodStart.UTC(), "period_end": t.PeriodEnd.UTC(), "opening_nav": t.OpeningNAV, "prior_hwm": t.PriorHWM,
		"hurdle_annual_bps": int64(t.HurdleAnnualBPS), "fee_rate_bps": int64(t.FeeRateBPS), "crystallization": string(t.Crystallization),
		"prior_accrued_fee": t.PriorAccruedFee, "fee_asset": feeAsset.String(), "mode": string(mode),
	})
	if err != nil {
		return "", errs.Wrap(err, errs.CodeInternal, "feeledger: encode performance terms")
	}
	return hashHex(b), nil
}

// FlowRequest records a deposit or withdrawal of an open period.
type FlowRequest struct {
	PeriodID PeriodID
	At       time.Time
	// Amount is positive for a deposit, negative for a withdrawal.
	Amount money.USD
	// NAVBefore is the gross NAV immediately before the flow.
	NAVBefore      money.USD
	IdempotencyKey string
	Actor          econ.Actor
}

// RecordFlow records a flow of an open period. Flows are recorded in time
// order, inside the period, and never before a recorded valuation: a flow
// before a valuation would change a fee already accrued. A deposit raises the
// high-water mark by itself and a withdrawal scales it, so no flow creates or
// destroys performance.
func (s *Service) RecordFlow(ctx context.Context, tx pgx.Tx, r FlowRequest) (Flow, error) {
	if err := requireActor(r.Actor); err != nil {
		return Flow{}, err
	}
	if err := validKey(r.IdempotencyKey); err != nil {
		return Flow{}, err
	}
	p, err := s.loadPeriod(ctx, tx, r.PeriodID, true)
	if err != nil {
		return Flow{}, err
	}
	reqHash, err := econ.RequestHash(map[string]any{"at": r.At.UTC(), "amount": r.Amount, "nav_before": r.NAVBefore})
	if err != nil {
		return Flow{}, err
	}
	for _, f := range p.Flows {
		if f.IdempotencyKey == r.IdempotencyKey {
			var stored []byte
			if err := tx.QueryRow(ctx, `SELECT request_hash FROM performance_fee_flows WHERE id = $1`, f.ID).Scan(&stored); err != nil {
				return Flow{}, dbErr("load flow", err)
			}
			if !bytes.Equal(stored, reqHash) {
				return Flow{}, errs.New(errs.CodeInvalidIdempotencyReuse, "feeledger: this idempotency key recorded a different flow")
			}
			return f, nil
		}
	}
	if p.Status != PeriodOpen {
		return Flow{}, errs.Newf(errs.CodeInvalidStateTransition, "feeledger: the period is %s and takes no more flows", p.Status)
	}
	at := r.At.UTC()
	if p.LastValuationAt != nil && !at.After(*p.LastValuationAt) {
		return Flow{}, errs.New(errs.CodeValidationFailed, "feeledger: a flow cannot precede a recorded valuation")
	}
	terms := p.Terms
	terms.Flows = append(append([]fees.CapitalFlow(nil), p.Terms.Flows...), fees.CapitalFlow{At: at, Amount: r.Amount, NAVBefore: r.NAVBefore})
	if err := terms.Validate(); err != nil {
		return Flow{}, err
	}
	f := Flow{ID: id.New[flowKind](), PeriodID: p.ID, Seq: len(p.Flows) + 1, At: at, Amount: r.Amount, NAVBefore: r.NAVBefore,
		Actor: r.Actor, IdempotencyKey: r.IdempotencyKey}
	if _, err := tx.Exec(ctx, `INSERT INTO performance_fee_flows (id, period_id, seq, occurred_at, amount_minor, nav_before_minor,
		recorded_by_actor_type, recorded_by_actor_id, idempotency_key, request_hash) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		f.ID, p.ID, f.Seq, at, r.Amount.Minor(), r.NAVBefore.Minor(), r.Actor.Type, r.Actor.ID, r.IdempotencyKey, reqHash); err != nil {
		return Flow{}, dbErr("record flow", err)
	}
	return f, nil
}

// AccrueRequest values an open period at an instant.
type AccrueRequest struct {
	PeriodID PeriodID
	At       time.Time
	// NAV is the gross NAV (before any performance fee) at At.
	NAV   money.USD
	Actor econ.Actor
}

// Accrue values an open period at an instant as if it ended then
// (fees.PerformancePeriod.Accrue, after replaying and re-proving every stored
// line) and books the change in the running accrual as an exactly-once FEES
// effect: PERFORMANCE_FEE_ACCRUAL Dr PERFORMANCE_FEE_ACCRUED / Cr
// PERFORMANCE_FEE_DEFERRED for a rise, the opposite for a fall (a REVERSAL
// line), nothing for no change. The same valuation (period, instant, NAV)
// replays its line; another NAV for the same instant is refused.
func (s *Service) Accrue(ctx context.Context, tx pgx.Tx, r AccrueRequest) (Line, error) {
	if err := requireActor(r.Actor); err != nil {
		return Line{}, err
	}
	p, err := s.loadPeriod(ctx, tx, r.PeriodID, true)
	if err != nil {
		return Line{}, err
	}
	at := r.At.UTC()
	key := "fees:perf:" + p.ID.String() + ":valuation:" + strconv.FormatInt(at.UnixNano(), 10)
	request := map[string]any{"period_id": p.ID.String(), "at": at, "nav": r.NAV}
	if line, found, err := s.replayLine(ctx, tx, p, key, request); err != nil || found {
		return line, err
	}
	if p.Status != PeriodOpen {
		return Line{}, errs.Newf(errs.CodeInvalidStateTransition, "feeledger: the period is %s and accrues nothing more", p.Status)
	}
	if err := s.requireMode(ctx, tx, gates.PerformanceFees, p.Mode); err != nil {
		return Line{}, err
	}
	engine, err := replay(p)
	if err != nil {
		return Line{}, err
	}
	pl, err := engine.Accrue(fees.NAVSnapshot{At: at, NAV: r.NAV})
	if err != nil {
		return Line{}, err
	}
	line, err := s.bookLine(ctx, tx, p, pl, key, request, EffectPerfAccrual)
	if err != nil {
		return Line{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE performance_fee_periods SET accrued_fee_minor = $2, last_valuation_at = $3 WHERE id = $1`,
		p.ID, line.AccrualAfter.Minor(), line.At); err != nil {
		return Line{}, dbErr("update accrual", err)
	}
	return line, nil
}

// replayLine returns the line an effect key already booked, refusing a
// different request under the key.
func (s *Service) replayLine(ctx context.Context, tx pgx.Tx, p Period, key string, request any) (Line, bool, error) {
	e, found, err := s.effects.GetByKey(ctx, tx, key)
	if err != nil || !found {
		return Line{}, false, err
	}
	hash, err := econ.RequestHash(request)
	if err != nil {
		return Line{}, false, err
	}
	if !bytes.Equal(hash, e.RequestHash) {
		return Line{}, false, errs.New(errs.CodeInvalidIdempotencyReuse, "feeledger: this valuation was already recorded with another NAV").
			WithField("effect_id", e.ID.String())
	}
	if e.Status != econ.EffectApplied {
		return Line{}, false, errs.Newf(errs.CodeReconciliationRequired, "feeledger: this valuation's effect is %s; it is reconciled, never retried", e.Status)
	}
	for _, l := range p.Lines {
		if l.EffectID == e.ID {
			return l, true, nil
		}
	}
	return Line{}, false, errs.New(errs.CodeInternal, "feeledger: an applied valuation has no line").WithField("effect_id", e.ID.String())
}

// replay rebuilds the period's engine from its terms and proves every stored
// line is the one the engine produces for its recorded valuation.
func replay(p Period) (*fees.PerformancePeriod, error) {
	engine, err := fees.OpenPerformancePeriod(p.Terms)
	if err != nil {
		return nil, err
	}
	for _, l := range p.Lines {
		got, err := engine.Accrue(fees.NAVSnapshot{At: l.At, NAV: l.NAV})
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "feeledger: a stored valuation no longer replays")
		}
		if got.Kind != l.Kind || !got.Amount.Equal(l.Amount) || !got.AccrualBefore.Equal(l.AccrualBefore) ||
			!got.AccrualAfter.Equal(l.AccrualAfter) || !got.FeeBase.Equal(l.FeeBase) || !got.HWMAdjusted.Equal(l.HWMAdjusted) ||
			!got.HurdleThreshold.Equal(l.HurdleThreshold) {
			return nil, rejection(errs.CodeInternal, ReasonRecordDoesNotVerify, "a stored line is not what its valuation produces").
				WithField("line_seq", l.Seq)
		}
	}
	return engine, nil
}

// feeQuantity converts USD cents to base units of the fee asset, exactly.
func feeQuantity(u money.USD, decimals uint8) (money.Quantity, error) {
	abs, err := u.Abs()
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeOverflow, "feeledger: fee amount")
	}
	q, err := money.USDToQuoteQuantity(abs, decimals, money.RoundExact)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodePrecisionLoss, "feeledger: fee amount in the fee asset")
	}
	return q, nil
}

// bookLine claims a line's effect, posts its journal when its amount is not
// zero, applies the effect and stores the line.
func (s *Service) bookLine(ctx context.Context, tx pgx.Tx, p Period, pl fees.PerformanceLine, key string, request any, kind string) (Line, error) {
	posts := !pl.Amount.IsZero()
	eff, existed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: key, Subsystem: econ.SubsystemFees, Kind: kind, AccountID: p.AccountID.String(), Request: request,
		PostsLedger: posts, Actor: s.actor,
	})
	if err != nil {
		return Line{}, err
	}
	if existed {
		return Line{}, errs.New(errs.CodeConflict, "feeledger: this line's effect was already claimed").WithField("effect_id", eff.ID.String())
	}
	line := Line{
		ID: id.New[lineKind](), PeriodID: p.ID, Seq: len(p.Lines) + 1, Kind: pl.Kind, At: pl.At.UTC(), NAV: pl.NAV,
		HWMAdjusted: pl.HWMAdjusted, HurdleThreshold: pl.HurdleThreshold, FeeBase: pl.FeeBase, Amount: pl.Amount,
		AccrualBefore: pl.AccrualBefore, AccrualAfter: pl.AccrualAfter,
	}
	apply := econ.ApplyRequest{Actor: s.actor, Reason: "performance fee " + strings.ToLower(string(pl.Kind))}
	if posts {
		posting, reservation, err := s.linePosting(ctx, tx, p, line, key)
		if err != nil {
			return Line{}, err
		}
		posted, err := s.ledger.Post(s.postingContext(ctx), tx, posting)
		if err != nil {
			return Line{}, err
		}
		if reservation != nil {
			if _, err := s.capital.ConsumeFinal(ctx, tx, reservation.ID, reservation.Quantity, 0, ""); err != nil {
				return Line{}, err
			}
			apply.ReservationID = reservation.ID.String()
		}
		line.JournalTransactionID = &posted.TransactionID
		apply.JournalTransactionID = &posted.TransactionID
	}
	applied, err := s.effects.Apply(ctx, tx, eff.ID, apply)
	if err != nil {
		return Line{}, err
	}
	line.EffectID = applied.ID
	if _, err := tx.Exec(ctx, `INSERT INTO performance_fee_lines (id, period_id, seq, kind, occurred_at, nav_minor, hwm_adjusted_minor,
		hurdle_threshold_minor, fee_base_minor, amount_minor, accrual_before_minor, accrual_after_minor, journal_transaction_id, effect_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		line.ID, p.ID, line.Seq, line.Kind, line.At, line.NAV.Minor(), line.HWMAdjusted.Minor(), line.HurdleThreshold.Minor(),
		line.FeeBase.Minor(), line.Amount.Minor(), line.AccrualBefore.Minor(), line.AccrualAfter.Minor(), line.JournalTransactionID,
		line.EffectID); err != nil {
		return Line{}, dbErr("record performance line", err)
	}
	return line, nil
}

// linePosting is the journal of a non-zero line (ADVANCED_FINANCIAL_MODEL §5):
//
//	ACCRUAL:         P: Dr PERFORMANCE_FEE_ACCRUED / Cr PERFORMANCE_FEE_DEFERRED
//	REVERSAL:        P: Dr PERFORMANCE_FEE_DEFERRED / Cr PERFORMANCE_FEE_ACCRUED
//	CRYSTALLIZATION: C: Dr FEES_PERFORMANCE / Cr WALLET;
//	                 P: Dr PERFORMANCE_FEE_DEFERRED / Cr PERFORMANCE_FEE_ACCRUED,
//	                    Dr PLATFORM_FEE_RECEIVABLE / Cr PERFORMANCE_FEE_REVENUE
//
// A crystallization first reserves the fee on the customer's wallet, so the
// charge never spends what another operation holds.
func (s *Service) linePosting(ctx context.Context, tx pgx.Tx, p Period, l Line, key string) (ledger.Posting, *capital.Reservation, error) {
	asset, err := s.assets.Get(ctx, tx, p.FeeAsset)
	if err != nil {
		return ledger.Posting{}, nil, err
	}
	q, err := feeQuantity(l.Amount, asset.Decimals)
	if err != nil {
		return ledger.Posting{}, nil, err
	}
	platform := func(c ledger.Code) ledger.AccountRef { return ledger.PlatformAccount(c, p.FeeAsset) }
	posting := ledger.Posting{
		IdempotencyKey: key, EffectiveAt: l.At, Reference: ledger.FinancialEventReference{Type: referencePerfAccrual, ID: l.ID.String()},
		Description: "simulated performance fee accounting (sandbox calculation, never charged live)",
		Metadata: map[string]any{"period_id": p.ID.String(), "line_seq": l.Seq, "line_kind": string(l.Kind),
			"account_ref": AccountRef(p.AccountID, p.StrategyRef)},
	}
	var reservation *capital.Reservation
	switch l.Kind {
	case fees.LineAccrual:
		posting.Kind = ledger.KindPerformanceFeeAccrual
		posting.Entries = []ledger.Entry{
			{Account: platform(ledger.CodePerformanceFeeAccrued), Side: ledger.Debit, Quantity: q},
			{Account: platform(ledger.CodePerformanceFeeDeferred), Side: ledger.Credit, Quantity: q},
		}
	case fees.LineReversal:
		posting.Kind = ledger.KindPerformanceFeeAccrual
		posting.Entries = []ledger.Entry{
			{Account: platform(ledger.CodePerformanceFeeDeferred), Side: ledger.Debit, Quantity: q},
			{Account: platform(ledger.CodePerformanceFeeAccrued), Side: ledger.Credit, Quantity: q},
		}
	case fees.LineCrystallization:
		res, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
			AccountID: p.AccountID.String(), AssetID: p.FeeAsset, Quantity: q, ActorType: s.actor.Type, ActorID: s.actor.ID,
			IdempotencyKey: key, TTL: time.Minute, Reason: "crystallized performance fee (period " + p.ID.String() + ")",
		})
		if err != nil {
			return ledger.Posting{}, nil, err
		}
		reservation = &res
		posting.Kind = ledger.KindPerformanceFeeCrystallization
		posting.Reference.Type = referencePerfCharge
		posting.Entries = []ledger.Entry{
			{Account: ledger.CustomerAccount(p.AccountID, ledger.CodeFeesPerformance, p.FeeAsset), Side: ledger.Debit, Quantity: q},
			{Account: ledger.CustomerAccount(p.AccountID, ledger.CodeWallet, p.FeeAsset), Side: ledger.Credit, Quantity: q},
			{Account: platform(ledger.CodePerformanceFeeDeferred), Side: ledger.Debit, Quantity: q},
			{Account: platform(ledger.CodePerformanceFeeAccrued), Side: ledger.Credit, Quantity: q},
			{Account: platform(ledger.CodePlatformFeeReceivable), Side: ledger.Debit, Quantity: q},
			{Account: platform(ledger.CodePerformanceFeeRevenue), Side: ledger.Credit, Quantity: q},
		}
	default:
		return ledger.Posting{}, nil, errs.New(errs.CodeInternal, "feeledger: a line of this kind posts nothing")
	}
	return posting, reservation, nil
}

// CrystallizeRequest closes a period at its end with the gross closing NAV.
type CrystallizeRequest struct {
	PeriodID   PeriodID
	ClosingNAV money.USD
	// Contributions attribute the period's gross PnL to strategies; when
	// given they must sum to it exactly.
	Contributions []fees.StrategyContribution
	Actor         econ.Actor
}

// Crystallize closes an open period once its end has passed: the engine
// values it at the end, the true-up line moves the accrual to the final fee,
// the CRYSTALLIZATION line charges it (PERFORMANCE_FEE_CRYSTALLIZATION, the
// customer's wallet reserved first), the fee is attributed to strategies, and
// the deterministic statement is stored with its hash -- all as exactly-once
// FEES effects. A replay returns the crystallized period.
func (s *Service) Crystallize(ctx context.Context, tx pgx.Tx, r CrystallizeRequest) (Period, error) {
	if err := requireActor(r.Actor); err != nil {
		return Period{}, err
	}
	p, err := s.loadPeriod(ctx, tx, r.PeriodID, true)
	if err != nil {
		return Period{}, err
	}
	key := "fees:perf:" + p.ID.String() + ":crystallize"
	request := map[string]any{"period_id": p.ID.String(), "closing_nav": r.ClosingNAV, "contributions": r.Contributions}
	if e, found, err := s.effects.GetByKey(ctx, tx, key); err != nil {
		return Period{}, err
	} else if found {
		hash, err := econ.RequestHash(request)
		if err != nil {
			return Period{}, err
		}
		if !bytes.Equal(hash, e.RequestHash) {
			return Period{}, errs.New(errs.CodeInvalidIdempotencyReuse, "feeledger: this period was crystallized with another closing NAV")
		}
		return p, nil
	}
	if p.Status != PeriodOpen {
		return Period{}, errs.Newf(errs.CodeInvalidStateTransition, "feeledger: the period is %s", p.Status)
	}
	if err := s.requireMode(ctx, tx, gates.PerformanceFees, p.Mode); err != nil {
		return Period{}, err
	}
	now := s.clk.Now().UTC()
	if now.Before(p.Terms.PeriodEnd) {
		return Period{}, rejection(errs.CodeInvalidStateTransition, ReasonPeriodNotEnded, "a period crystallizes at its end, not before")
	}
	engine, err := replay(p)
	if err != nil {
		return Period{}, err
	}
	st, err := engine.Crystallize(r.ClosingNAV, r.Contributions)
	if err != nil {
		return Period{}, err
	}
	n := len(st.Lines)
	trueUp, err := s.bookLine(ctx, tx, p, st.Lines[n-2], "fees:perf:"+p.ID.String()+":trueup",
		map[string]any{"period_id": p.ID.String(), "true_up_at": st.PeriodEnd}, EffectPerfAccrual)
	if err != nil {
		return Period{}, err
	}
	p.Lines = append(p.Lines, trueUp)
	crystallized, err := s.bookLine(ctx, tx, p, st.Lines[n-1], key, request, EffectPerfCrystallize)
	if err != nil {
		return Period{}, err
	}
	for _, a := range st.Attribution {
		if _, err := tx.Exec(ctx, `INSERT INTO performance_fee_attributions (id, period_id, strategy, gross_pnl_minor, eligible,
			fee_share_minor, remainder_minor) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			id.New[id.Any](), p.ID, a.Strategy, a.GrossPnL.Minor(), a.Eligible, a.FeeShare.Minor(), a.RemainderAllocated.Minor()); err != nil {
			return Period{}, dbErr("record attribution", err)
		}
	}
	body, err := st.Canonical()
	if err != nil {
		return Period{}, err
	}
	hash, err := st.Hash()
	if err != nil {
		return Period{}, err
	}
	var stDoc any
	if err := json.Unmarshal(body, &stDoc); err != nil {
		return Period{}, errs.Wrap(err, errs.CodeInternal, "feeledger: re-read statement")
	}
	if _, err := econ.RecordEvidence(ctx, tx, econ.Evidence{
		Key: "fees/performance/" + p.ID.String() + "/statement",
		Document: map[string]any{"kind": "feeledger.performance_statement.v1", "period_id": p.ID.String(), "statement_hash": hash,
			"statement": stDoc, "fee_asset": p.FeeAsset.String(), "mode": string(p.Mode), "value_domain": "SIMULATED"},
	}); err != nil {
		return Period{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO performance_fee_period_transitions (id, period_id, from_status, to_status, actor_type,
		actor_id, reason, occurred_at) VALUES ($1,$2,'OPEN','CRYSTALLIZED',$3,$4,'crystallized at the period end',$5)`,
		id.New[id.Any](), p.ID, r.Actor.Type, r.Actor.ID, now); err != nil {
		return Period{}, dbErr("record period transition", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE performance_fee_periods SET status = 'CRYSTALLIZED', accrued_fee_minor = 0,
		last_valuation_at = $2, statement = $3, statement_hash = $4, closing_nav_gross_minor = $5, net_flows_minor = $6,
		gross_pnl_minor = $7, hwm_adjusted_minor = $8, fee_base_minor = $9, fee_amount_minor = $10, closing_nav_net_minor = $11,
		hwm_after_minor = $12, crystallization_journal_transaction_id = $13, crystallization_effect_id = $14, crystallized_at = $15
		WHERE id = $1`,
		p.ID, crystallized.At, body, hash, st.ClosingNAVGross.Minor(), st.NetFlows.Minor(), st.GrossPnL.Minor(), st.HWMAdjusted.Minor(),
		st.FeeBase.Minor(), st.FeeAmount.Minor(), st.ClosingNAVNet.Minor(), st.HWMAfter.Minor(), crystallized.JournalTransactionID,
		crystallized.EffectID, now); err != nil {
		return Period{}, dbErr("crystallize period", err)
	}
	return s.loadPeriod(ctx, tx, p.ID, false)
}

// CancelPeriod cancels an open period: any running accrual is reversed by a
// REVERSAL line (its own exactly-once effect), and the period is CANCELLED.
func (s *Service) CancelPeriod(ctx context.Context, tx pgx.Tx, pid PeriodID, reason string, actor econ.Actor) (Period, error) {
	if err := requireActor(actor); err != nil {
		return Period{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return Period{}, errs.New(errs.CodeValidationFailed, "feeledger: a cancellation needs a reason")
	}
	p, err := s.loadPeriod(ctx, tx, pid, true)
	if err != nil {
		return Period{}, err
	}
	if p.Status == PeriodCancelled {
		return p, nil
	}
	if p.Status != PeriodOpen {
		return Period{}, errs.Newf(errs.CodeInvalidStateTransition, "feeledger: a %s period cannot be cancelled", p.Status)
	}
	now := s.clk.Now().UTC()
	lastAt := p.LastValuationAt
	if p.Accrued.IsPositive() {
		at := now
		if lastAt != nil && at.Before(*lastAt) {
			at = *lastAt
		}
		last := p.Lines[len(p.Lines)-1]
		pl := fees.PerformanceLine{Kind: fees.LineReversal, At: at, NAV: last.NAV, HWMAdjusted: last.HWMAdjusted,
			HurdleThreshold: last.HurdleThreshold, Amount: p.Accrued.Neg(), AccrualBefore: p.Accrued}
		line, err := s.bookLine(ctx, tx, p, pl, "fees:perf:"+p.ID.String()+":cancel",
			map[string]any{"period_id": p.ID.String(), "cancel": reason}, EffectPerfAccrual)
		if err != nil {
			return Period{}, err
		}
		lastAt = &line.At
	}
	if _, err := tx.Exec(ctx, `INSERT INTO performance_fee_period_transitions (id, period_id, from_status, to_status, actor_type,
		actor_id, reason, occurred_at) VALUES ($1,$2,'OPEN','CANCELLED',$3,$4,$5,$6)`,
		id.New[id.Any](), p.ID, actor.Type, actor.ID, reason, now); err != nil {
		return Period{}, dbErr("record period transition", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE performance_fee_periods SET status = 'CANCELLED', accrued_fee_minor = 0, last_valuation_at = $2
		WHERE id = $1`, p.ID, lastAt); err != nil {
		return Period{}, dbErr("cancel period", err)
	}
	return s.loadPeriod(ctx, tx, p.ID, false)
}

// ReversePeriodRequest reverses a crystallized period.
type ReversePeriodRequest struct {
	PeriodID   PeriodID
	ReasonCode string
	Reason     string
	Actor      econ.Actor
}

// ReverseCrystallization corrects a crystallized period by compensation: a
// COMPENSATION journal (reversal_of the crystallization, with a reason code)
// returns exactly the fee to the customer and takes it out of the platform's
// revenue, the crystallization effect is COMPENSATED, and the period becomes
// REVERSED. Its statement and lines stay as they were. A period another one
// already continues from cannot be reversed; after a reversal a corrected
// period can be opened in its place.
func (s *Service) ReverseCrystallization(ctx context.Context, tx pgx.Tx, r ReversePeriodRequest) (Period, error) {
	if err := requireActor(r.Actor); err != nil {
		return Period{}, err
	}
	if !reasonCodePattern.MatchString(r.ReasonCode) || strings.TrimSpace(r.Reason) == "" {
		return Period{}, errs.New(errs.CodeValidationFailed, "feeledger: a reversal needs a reason code and a reason")
	}
	p, err := s.loadPeriod(ctx, tx, r.PeriodID, true)
	if err != nil {
		return Period{}, err
	}
	if p.Status == PeriodReversed {
		return p, nil
	}
	if p.Status != PeriodCrystallized {
		return Period{}, errs.Newf(errs.CodeInvalidStateTransition, "feeledger: a %s period cannot be reversed", p.Status)
	}
	var continued bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM performance_fee_periods WHERE previous_period_id = $1
		AND status <> 'CANCELLED' AND status <> 'REVERSED')`, p.ID).Scan(&continued); err != nil {
		return Period{}, dbErr("look up successor", err)
	}
	if continued {
		return Period{}, rejection(errs.CodeConflict, ReasonPeriodContinued, "a later period already continues from this one")
	}
	now := s.clk.Now().UTC()
	var journal *ledger.TransactionID
	if p.Statement.FeeAmount.IsPositive() {
		asset, err := s.assets.Get(ctx, tx, p.FeeAsset)
		if err != nil {
			return Period{}, err
		}
		q, err := feeQuantity(p.Statement.FeeAmount, asset.Decimals)
		if err != nil {
			return Period{}, err
		}
		posted, err := s.ledger.Post(s.postingContext(ctx), tx, ledger.Posting{
			Kind: ledger.KindCompensation, IdempotencyKey: "fees:perf:" + p.ID.String() + ":reversal",
			Reference:   ledger.FinancialEventReference{Type: referencePerfReversal, ID: p.ID.String()},
			EffectiveAt: now, Description: "reversal of a simulated performance fee", ReversalOf: p.CrystallizationJournalID,
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(p.AccountID, ledger.CodeWallet, p.FeeAsset), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(p.AccountID, ledger.CodeFeesPerformance, p.FeeAsset), Side: ledger.Credit, Quantity: q},
				{Account: ledger.PlatformAccount(ledger.CodePerformanceFeeRevenue, p.FeeAsset), Side: ledger.Debit, Quantity: q},
				{Account: ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, p.FeeAsset), Side: ledger.Credit, Quantity: q},
			},
			Metadata: map[string]any{ledger.MetadataReasonCode: r.ReasonCode, "period_id": p.ID.String()},
		})
		if err != nil {
			return Period{}, err
		}
		journal = &posted.TransactionID
	}
	if _, err := s.effects.Compensate(ctx, tx, *p.CrystallizationEffectID, journal, s.actor, r.ReasonCode+": "+r.Reason); err != nil {
		return Period{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO performance_fee_period_transitions (id, period_id, from_status, to_status, actor_type,
		actor_id, reason, occurred_at) VALUES ($1,$2,'CRYSTALLIZED','REVERSED',$3,$4,$5,$6)`,
		id.New[id.Any](), p.ID, r.Actor.Type, r.Actor.ID, r.ReasonCode+": "+r.Reason, now); err != nil {
		return Period{}, dbErr("record period transition", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE performance_fee_periods SET status = 'REVERSED', reversal_journal_transaction_id = $2,
		reversal_reason_code = $3, reversed_at = $4 WHERE id = $1`, p.ID, journal, r.ReasonCode, now); err != nil {
		return Period{}, dbErr("reverse period", err)
	}
	return s.loadPeriod(ctx, tx, p.ID, false)
}

// Period returns one period with its flows and lines.
func (s *Service) Period(ctx context.Context, q db.Querier, pid PeriodID) (Period, error) {
	return s.loadPeriod(ctx, q, pid, false)
}

// VerifyPeriod replays a crystallized period from its agreed terms, its
// recorded flows and the valuations its lines record, and requires the stored
// statement byte for byte (fees.VerifyPerformanceStatement) and the stored
// lines to be the statement's.
func (s *Service) VerifyPeriod(ctx context.Context, q db.Querier, pid PeriodID) error {
	p, err := s.loadPeriod(ctx, q, pid, false)
	if err != nil {
		return err
	}
	if p.Statement == nil {
		return errs.New(errs.CodeInvalidStateTransition, "feeledger: the period has no statement yet")
	}
	if err := fees.VerifyPerformanceStatement(p.Terms, *p.Statement); err != nil {
		return err
	}
	if len(p.Lines) != len(p.Statement.Lines) {
		return rejection(errs.CodeValidationFailed, ReasonRecordDoesNotVerify, "the stored lines are not the statement's")
	}
	for i, l := range p.Statement.Lines {
		got := p.Lines[i]
		if got.Kind != l.Kind || !got.Amount.Equal(l.Amount) || !got.NAV.Equal(l.NAV) || !got.At.Equal(l.At.UTC()) ||
			!got.AccrualAfter.Equal(l.AccrualAfter) {
			return rejection(errs.CodeValidationFailed, ReasonRecordDoesNotVerify, "a stored line is not the statement's").
				WithField("line_seq", got.Seq)
		}
	}
	return nil
}

func (s *Service) loadPeriod(ctx context.Context, q db.Querier, pid PeriodID, lock bool) (Period, error) {
	sql := `SELECT id, account_id, strategy_ref, period_seq, previous_period_id, mode, fee_asset_id, terms_version, period_start,
		period_end, opening_nav_minor, prior_hwm_minor, hurdle_annual_bps, fee_rate_bps, crystallization, prior_accrued_fee_minor,
		terms_hash, status, accrued_fee_minor, last_valuation_at, statement, coalesce(statement_hash, ''),
		crystallization_journal_transaction_id, crystallization_effect_id, crystallized_at, reversal_journal_transaction_id,
		coalesce(reversal_reason_code, ''), reversed_at
		FROM performance_fee_periods WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var p Period
	var opening, hwm, prior, accrued int64
	var hurdle, rate int
	err := q.QueryRow(ctx, sql, pid).Scan(&p.ID, &p.AccountID, &p.StrategyRef, &p.Seq, &p.PreviousID, &p.Mode, &p.FeeAsset,
		&p.Terms.TermsVersion, &p.Terms.PeriodStart, &p.Terms.PeriodEnd, &opening, &hwm, &hurdle, &rate, &p.Terms.Crystallization,
		&prior, &p.TermsHash, &p.Status, &accrued, &p.LastValuationAt, &p.StatementBytes, &p.StatementHash,
		&p.CrystallizationJournalID, &p.CrystallizationEffectID, &p.CrystallizedAt, &p.ReversalJournalID, &p.ReversalReasonCode,
		&p.ReversedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Period{}, errs.New(errs.CodeNotFound, "feeledger: performance period not found").WithField("period_id", pid.String())
	}
	if err != nil {
		return Period{}, dbErr("load performance period", err)
	}
	p.Terms.AccountRef = AccountRef(p.AccountID, p.StrategyRef)
	p.Terms.PeriodStart, p.Terms.PeriodEnd = p.Terms.PeriodStart.UTC(), p.Terms.PeriodEnd.UTC()
	p.Terms.OpeningNAV, p.Terms.PriorHWM, p.Terms.PriorAccruedFee = money.USDFromMinor(opening), money.USDFromMinor(hwm), money.USDFromMinor(prior)
	p.Terms.HurdleAnnualBPS, p.Terms.FeeRateBPS = money.BPS(hurdle), money.BPS(rate)
	p.Accrued = money.USDFromMinor(accrued)
	if p.LastValuationAt != nil {
		t := p.LastValuationAt.UTC()
		p.LastValuationAt = &t
	}
	if p.Flows, err = loadFlows(ctx, q, pid); err != nil {
		return Period{}, err
	}
	for _, f := range p.Flows {
		p.Terms.Flows = append(p.Terms.Flows, fees.CapitalFlow{At: f.At, Amount: f.Amount, NAVBefore: f.NAVBefore})
	}
	if p.Lines, err = loadLines(ctx, q, pid); err != nil {
		return Period{}, err
	}
	if len(p.StatementBytes) > 0 {
		st, err := decodeStatement(p.StatementBytes, p.StatementHash)
		if err != nil {
			return Period{}, err
		}
		p.Statement = &st
		p.GrossReturnBPS, p.NetReturnBPS = returns(p.Terms, st)
	}
	return p, nil
}

// decodeStatement decodes a stored performance statement and proves it is
// the canonical form its recorded hash names.
func decodeStatement(body []byte, hash string) (fees.PerformanceStatement, error) {
	var st fees.PerformanceStatement
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return fees.PerformanceStatement{}, rejection(errs.CodeInternal, ReasonRecordDoesNotVerify, "a stored statement is not a statement")
	}
	again, err := st.Canonical()
	if err != nil {
		return fees.PerformanceStatement{}, err
	}
	if !bytes.Equal(again, body) || hashHex(again) != hash {
		return fees.PerformanceStatement{}, rejection(errs.CodeInternal, ReasonRecordDoesNotVerify,
			"a stored statement is not the canonical form its hash names")
	}
	return st, nil
}

func loadFlows(ctx context.Context, q db.Querier, pid PeriodID) ([]Flow, error) {
	rows, err := q.Query(ctx, `SELECT id, seq, occurred_at, amount_minor, nav_before_minor, recorded_by_actor_type,
		recorded_by_actor_id, idempotency_key FROM performance_fee_flows WHERE period_id = $1 ORDER BY seq`, pid)
	if err != nil {
		return nil, dbErr("load flows", err)
	}
	defer rows.Close()
	var out []Flow
	for rows.Next() {
		f := Flow{PeriodID: pid}
		var amount, nav int64
		if err := rows.Scan(&f.ID, &f.Seq, &f.At, &amount, &nav, &f.Actor.Type, &f.Actor.ID, &f.IdempotencyKey); err != nil {
			return nil, dbErr("scan flow", err)
		}
		f.At, f.Amount, f.NAVBefore = f.At.UTC(), money.USDFromMinor(amount), money.USDFromMinor(nav)
		out = append(out, f)
	}
	return out, rows.Err()
}

func loadLines(ctx context.Context, q db.Querier, pid PeriodID) ([]Line, error) {
	rows, err := q.Query(ctx, `SELECT id, seq, kind, occurred_at, nav_minor, hwm_adjusted_minor, hurdle_threshold_minor, fee_base_minor,
		amount_minor, accrual_before_minor, accrual_after_minor, journal_transaction_id, effect_id
		FROM performance_fee_lines WHERE period_id = $1 ORDER BY seq`, pid)
	if err != nil {
		return nil, dbErr("load lines", err)
	}
	defer rows.Close()
	var out []Line
	for rows.Next() {
		l := Line{PeriodID: pid}
		var v [7]int64
		if err := rows.Scan(&l.ID, &l.Seq, &l.Kind, &l.At, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &l.JournalTransactionID,
			&l.EffectID); err != nil {
			return nil, dbErr("scan line", err)
		}
		l.At = l.At.UTC()
		l.NAV, l.HWMAdjusted, l.HurdleThreshold, l.FeeBase = money.USDFromMinor(v[0]), money.USDFromMinor(v[1]), money.USDFromMinor(v[2]), money.USDFromMinor(v[3])
		l.Amount, l.AccrualBefore, l.AccrualAfter = money.USDFromMinor(v[4]), money.USDFromMinor(v[5]), money.USDFromMinor(v[6])
		out = append(out, l)
	}
	return out, rows.Err()
}

// returns computes the Modified Dietz gross and net returns of a crystallized
// period in basis points, exactly, rounded toward zero:
//
//	capital at work = opening NAV + Σ flow × (end − flow time) / (end − start)
//	gross return    = gross PnL / capital at work
//	net return      = (gross PnL − fee) / capital at work
//
// Both are nil when no capital was at work.
func returns(t fees.PerformanceTerms, st fees.PerformanceStatement) (*money.BPS, *money.BPS) {
	span := money.QuantityFromInt64(int64(t.PeriodEnd.Sub(t.PeriodStart)))
	if !span.IsPositive() {
		return nil, nil
	}
	work := money.QuantityFromInt64(t.OpeningNAV.Minor()).Mul(span)
	for _, f := range t.Flows {
		weight := money.QuantityFromInt64(int64(t.PeriodEnd.Sub(f.At)))
		work = work.Add(money.QuantityFromInt64(f.Amount.Minor()).Mul(weight))
	}
	if !work.IsPositive() {
		return nil, nil
	}
	ratio := func(pnl money.USD) *money.BPS {
		num := money.QuantityFromInt64(pnl.Minor()).Mul(span).Mul(money.QuantityFromInt64(int64(money.OneHundredPercent)))
		bps, err := num.Div(work, money.RoundDown)
		if err != nil {
			return nil
		}
		v, err := bps.Int64()
		if err != nil {
			return nil
		}
		b := money.BPS(v)
		return &b
	}
	net, err := st.GrossPnL.Sub(st.FeeAmount)
	if err != nil {
		return ratio(st.GrossPnL), nil
	}
	return ratio(st.GrossPnL), ratio(net)
}

// Periods lists an (account, strategy) pair's periods in sequence order.
func (s *Service) Periods(ctx context.Context, q db.Querier, account accounts.AccountID, strategy string) ([]Period, error) {
	rows, err := q.Query(ctx, `SELECT id FROM performance_fee_periods WHERE account_id = $1 AND strategy_ref = $2 ORDER BY period_seq`,
		account, strategy)
	if err != nil {
		return nil, dbErr("list periods", err)
	}
	var ids []PeriodID
	for rows.Next() {
		var pid PeriodID
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return nil, dbErr("list periods", err)
		}
		ids = append(ids, pid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, dbErr("list periods", err)
	}
	out := make([]Period, 0, len(ids))
	for _, pid := range ids {
		p, err := s.loadPeriod(ctx, q, pid, false)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
