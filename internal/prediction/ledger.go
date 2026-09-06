package prediction

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// Ledger is the prediction ledger. It appends and reads; it never amends.
// There is no Update and no Delete on this interface, and none in the
// implementation, because a prediction that could be edited after the outcome
// is known is worthless as evidence (PART 72).
type Ledger interface {
	Commit(ctx context.Context, tx pgx.Tx, p Prediction) (Prediction, error)
	Get(ctx context.Context, q db.Querier, predictionID PredictionID) (Prediction, error)
	GetByRunAction(ctx context.Context, q db.Querier, runID, action string) (Prediction, error)
	DueForResolution(ctx context.Context, q db.Querier, now time.Time, limit int) ([]Prediction, error)
	RecordOutcome(ctx context.Context, tx pgx.Tx, o Outcome) error
}

const predictionColumns = `id, agent_id::text, agent_version, agent_run_id::text, action_name,
       strategy_version_id::text, account_id::text, mode, instrument_id, horizon_ms,
       coalesce(direction, ''), probability_direction::text, expected_return_bps,
       downside_probability::text, max_downside_bps, confidence::text,
       information_set_hash, decision_available_at, committed_at,
       coalesce(model_call_id::text, ''), coalesce(template_version, ''),
       rationale, evidence_refs, signals, coalesce(correlation_id, '')`

func scanPrediction(row pgx.Row) (Prediction, error) {
	var (
		p                          Prediction
		mode, direction            string
		horizonMS                  int64
		probDir                    *string
		downside, confidence       string
		expectedBPS, maxDownsideBP int32
	)
	err := row.Scan(&p.ID, &p.AgentID, &p.AgentVersion, &p.RunID, &p.ActionName,
		&p.StrategyVersionID, &p.AccountID, &mode, &p.InstrumentID, &horizonMS,
		&direction, &probDir, &expectedBPS,
		&downside, &maxDownsideBP, &confidence,
		&p.InformationSetHash, &p.DecisionAvailableAt, &p.CommittedAt,
		&p.ModelCallID, &p.TemplateVersion,
		&p.Rationale, &p.EvidenceRefs, &p.Signals, &p.CorrelationID)
	if err != nil {
		return Prediction{}, err
	}
	p.Mode = Mode(mode)
	p.Direction = Direction(direction)
	p.Horizon = time.Duration(horizonMS) * time.Millisecond
	p.ExpectedReturnBPS = money.BPS(expectedBPS)
	p.MaxDownsideBPS = money.BPS(maxDownsideBP)
	if probDir != nil {
		d, derr := ir.ParseDecimalString(*probDir)
		if derr != nil {
			return Prediction{}, errs.Wrap(derr, errs.CodeInternal, "prediction: decode probability_direction")
		}
		p.ProbabilityDirection = d
	}
	dp, err := ir.ParseDecimalString(downside)
	if err != nil {
		return Prediction{}, errs.Wrap(err, errs.CodeInternal, "prediction: decode downside_probability")
	}
	p.DownsideProbability = dp
	cf, err := ir.ParseDecimalString(confidence)
	if err != nil {
		return Prediction{}, errs.Wrap(err, errs.CodeInternal, "prediction: decode confidence")
	}
	p.Confidence = cf
	return p, nil
}

const insertPredictionSQL = `
INSERT INTO predictions (
    id, agent_id, agent_version, agent_run_id, action_name, strategy_version_id, account_id, mode,
    instrument_id, horizon_ms, direction, probability_direction, expected_return_bps,
    downside_probability, max_downside_bps, confidence, information_set_hash,
    decision_available_at, committed_at, model_call_id, template_version,
    rationale, evidence_refs, signals, correlation_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9, $10, $11, $12::numeric, $13,
    $14::numeric, $15, $16::numeric, $17,
    $18, $19, $20, $21,
    $22, $23, $24, $25
)
ON CONFLICT (agent_run_id, action_name) DO NOTHING
RETURNING id`

// PGLedger is the PostgreSQL prediction ledger. It holds no connection.
type PGLedger struct {
	clk clock.Clock
}

var _ Ledger = (*PGLedger)(nil)

// NewLedger builds the ledger. The clock stamps committed_at, so a caller
// cannot backdate a prediction by passing a time of its own choosing.
func NewLedger(clk clock.Clock) (*PGLedger, error) {
	if clk == nil {
		return nil, errs.New(errs.CodeValidationFailed, "prediction: ledger requires a clock")
	}
	return &PGLedger{clk: clk}, nil
}

// Commit appends one prediction. committed_at is stamped from the injected
// clock, never from the caller, so "the prediction predates the trade" is a
// fact about the platform's clock rather than a claim by the agent.
//
// The caller's principal must be the agent itself (ActorType AGENT holding
// prediction:commit): a human cannot file a prediction through this path and
// then attribute a trade to it.
func (l *PGLedger) Commit(ctx context.Context, tx pgx.Tx, p Prediction) (Prediction, error) {
	if tx == nil {
		return Prediction{}, errs.New(errs.CodeInternal, "prediction: Commit requires a transaction")
	}
	if err := security.RequireAt(ctx, security.PermPredictionCommit, l.clk.Now); err != nil {
		return Prediction{}, errs.Wrap(err, errs.CodeForbidden, "prediction: commit is not permitted")
	}
	if p.ID.IsZero() {
		p.ID = NewPredictionID()
	}
	p.CommittedAt = l.clk.Now()
	if p.DecisionAvailableAt.IsZero() {
		p.DecisionAvailableAt = p.CommittedAt
	}
	if p.DecisionAvailableAt.After(p.CommittedAt) {
		return Prediction{}, errs.New(errs.CodeValidationFailed,
			"prediction: the information set became available after the commit; a decision cannot use data it did not have").
			WithField("decision_available_at", p.DecisionAvailableAt.Format(time.RFC3339Nano))
	}
	normalized, err := normalizeProbabilities(p)
	if err != nil {
		return Prediction{}, err
	}
	p = normalized
	if err := p.Validate(); err != nil {
		return Prediction{}, err
	}

	var probDir *string
	if p.Direction != "" {
		s := p.ProbabilityDirection.String()
		probDir = &s
	}
	expectedBPS, err := bpsColumn("expected_return_bps", p.ExpectedReturnBPS)
	if err != nil {
		return Prediction{}, err
	}
	maxDownsideBPS, err := bpsColumn("max_downside_bps", p.MaxDownsideBPS)
	if err != nil {
		return Prediction{}, err
	}
	// ON CONFLICT DO NOTHING rather than a plain insert: one action of one run
	// commits exactly one prediction, and a retry after a crash must return
	// the original. Letting the unique violation fire would abort the
	// transaction and make the original unreadable from inside it.
	var written PredictionID
	err = tx.QueryRow(ctx, insertPredictionSQL,
		p.ID, p.AgentID, p.AgentVersion, p.RunID, p.ActionName, p.StrategyVersionID, p.AccountID, string(p.Mode),
		p.InstrumentID, p.Horizon.Milliseconds(), nullText(string(p.Direction)), probDir, expectedBPS,
		p.DownsideProbability.String(), maxDownsideBPS, p.Confidence.String(), p.InformationSetHash,
		p.DecisionAvailableAt, p.CommittedAt, nullUUID(p.ModelCallID), nullText(p.TemplateVersion),
		jsonOr(p.Rationale, "{}"), jsonOr(p.EvidenceRefs, "[]"), jsonOr(p.Signals, "[]"),
		nullText(p.CorrelationID)).Scan(&written)
	switch {
	case isNoRows(err):
		// The forecast for this run and action already stands. It is returned
		// unchanged: a retry can never revise what was predicted.
		existing, gerr := l.GetByRunAction(ctx, tx, p.RunID, p.ActionName)
		if gerr != nil {
			return Prediction{}, gerr
		}
		return existing, nil
	case err != nil:
		if db.IsUniqueViolation(err) {
			return Prediction{}, errs.Wrap(err, errs.CodeConflict, "prediction: duplicate prediction id")
		}
		return Prediction{}, errs.Wrap(err, errs.CodeInternal, "prediction: commit")
	}
	return p, nil
}

// normalizeProbabilities widens every probability to the ledger scale.
func normalizeProbabilities(p Prediction) (Prediction, error) {
	if p.Direction != "" {
		v, err := NormalizeProbability(p.ProbabilityDirection)
		if err != nil {
			return Prediction{}, err
		}
		p.ProbabilityDirection = v
	}
	dp, err := NormalizeProbability(p.DownsideProbability)
	if err != nil {
		return Prediction{}, err
	}
	p.DownsideProbability = dp
	cf, err := NormalizeProbability(p.Confidence)
	if err != nil {
		return Prediction{}, err
	}
	p.Confidence = cf
	return p, nil
}

// Get returns one prediction, or NOT_FOUND.
func (l *PGLedger) Get(ctx context.Context, q db.Querier, predictionID PredictionID) (Prediction, error) {
	p, err := scanPrediction(q.QueryRow(ctx, `SELECT `+predictionColumns+` FROM predictions WHERE id = $1`, predictionID))
	if err != nil {
		if isNoRows(err) {
			return Prediction{}, errs.Newf(errs.CodeNotFound, "prediction: %s not found", predictionID).
				WithField("prediction_id", predictionID.String())
		}
		return Prediction{}, errs.Wrap(err, errs.CodeInternal, "prediction: load")
	}
	return p, nil
}

// GetByRunAction returns the prediction one run committed for one action.
func (l *PGLedger) GetByRunAction(ctx context.Context, q db.Querier, runID, action string) (Prediction, error) {
	p, err := scanPrediction(q.QueryRow(ctx,
		`SELECT `+predictionColumns+` FROM predictions WHERE agent_run_id = $1 AND action_name = $2`, runID, action))
	if err != nil {
		if isNoRows(err) {
			return Prediction{}, errs.Newf(errs.CodeNotFound, "prediction: run %s has no prediction for action %s", runID, action)
		}
		return Prediction{}, errs.Wrap(err, errs.CodeInternal, "prediction: load by run and action")
	}
	return p, nil
}

const dueForResolutionSQL = `
SELECT ` + predictionColumns + `
  FROM predictions p
 WHERE p.committed_at + make_interval(secs => p.horizon_ms / 1000.0) <= $1
   AND NOT EXISTS (SELECT 1 FROM prediction_outcomes o WHERE o.prediction_id = p.id)
 ORDER BY p.committed_at
 LIMIT $2`

// DueForResolution returns predictions whose horizon has elapsed and that
// have no outcome yet. It never returns a prediction whose horizon is still
// open: scoring one early would score it against knowledge from inside its
// own window.
func (l *PGLedger) DueForResolution(ctx context.Context, q db.Querier, now time.Time, limit int) ([]Prediction, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := q.Query(ctx, dueForResolutionSQL, now.UTC(), limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "prediction: list due for resolution")
	}
	defer rows.Close()
	var out []Prediction
	for rows.Next() {
		p, err := scanPrediction(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "prediction: scan due prediction")
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "prediction: iterate due predictions")
	}
	return out, nil
}

const insertOutcomeSQL = `
INSERT INTO prediction_outcomes (
    id, prediction_id, strategy_version_id, mode, horizon_end_at, resolved_at,
    realized_direction, realized_return_bps, realized_max_drawdown_bps, direction_hit,
    brier, log_loss, abs_return_error_bps, regime_label, valuation_source,
    price_ref_start, price_ref_end, resolver_version
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10,
    $11::numeric, $12::numeric, $13, $14, $15,
    $16, $17, $18
)`

// RecordOutcome appends the realized outcome. The table has a UNIQUE
// constraint on prediction_id and a forbid_mutation trigger, so an outcome is
// written once and never revised: re-resolving with a different price is
// impossible by construction.
func (l *PGLedger) RecordOutcome(ctx context.Context, tx pgx.Tx, o Outcome) error {
	if tx == nil {
		return errs.New(errs.CodeInternal, "prediction: RecordOutcome requires a transaction")
	}
	if err := o.Validate(); err != nil {
		return err
	}
	var brier, logLoss *string
	if !isUnset(o.Brier) {
		s := o.Brier.String()
		brier = &s
	}
	if !isUnset(o.LogLoss) {
		s := o.LogLoss.String()
		logLoss = &s
	}
	realizedBPS, err := bpsColumn("realized_return_bps", o.RealizedReturnBPS)
	if err != nil {
		return err
	}
	drawdownBPS, err := bpsColumn("realized_max_drawdown_bps", o.RealizedMaxDrawdownBPS)
	if err != nil {
		return err
	}
	absErrorBPS, err := bpsColumn("abs_return_error_bps", o.AbsReturnErrorBPS)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, insertOutcomeSQL,
		o.ID, o.PredictionID, o.StrategyVersionID, string(o.Mode), o.HorizonEndAt, o.ResolvedAt,
		nullText(string(o.RealizedDirection)), realizedBPS, drawdownBPS, o.DirectionHit,
		brier, logLoss, absErrorBPS, o.RegimeLabel, o.ValuationSource,
		o.PriceRefStart, o.PriceRefEnd, o.ResolverVersion)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return errs.Newf(errs.CodeConflict, "prediction: %s already has an outcome and outcomes are never revised", o.PredictionID).
				WithField("prediction_id", o.PredictionID.String())
		}
		if db.IsMutationForbidden(err) {
			return errs.Wrap(err, errs.CodeConflict, "prediction: outcomes are immutable")
		}
		return errs.Wrap(err, errs.CodeInternal, "prediction: record outcome")
	}
	return nil
}

// bpsColumn narrows a money.BPS onto the integer column the schema declares.
// money.BPS is an int64 and the column is a 32-bit integer, so a silent
// truncation here would turn an absurd number into a plausible one. It is
// refused instead.
func bpsColumn(field string, b money.BPS) (int32, error) {
	if b < math.MinInt32 || b > math.MaxInt32 {
		return 0, errs.Newf(errs.CodeOverflow, "prediction: %s of %d does not fit the column", field, int64(b))
	}
	return int32(b), nil
}

// jsonOr returns raw, or the given JSON default when raw is empty.
func jsonOr(raw json.RawMessage, def string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(def)
	}
	return raw
}

func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullUUID(s string) *string {
	if s == "" || s == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	return &s
}

// InstrumentOf is a convenience for callers holding only the id string.
func InstrumentOf(s string) (instruments.InstrumentID, error) {
	return instruments.ParseInstrumentID(s)
}
