package derivatives

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// dbErr maps a database error to a stable *errs.Error, passing retryable
// transaction failures and already-mapped errors through unchanged.
func dbErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if db.IsRetryable(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if ee, ok := errs.As(err); ok {
		return ee
	}
	switch state := db.SQLState(err); state {
	case "DV001", "DV002", "DV003", "DV004", "DV005", "DV007", "DV008", "DV009", "DV011":
		return errs.Wrap(err, errs.CodeConflict, "derivatives: "+op+": the database refused an inconsistent state").
			WithField("sqlstate", state)
	case "DV006", "DV010":
		// A trade the risk kernel did not allow, an override without a second
		// operator: refusals of authority, not of shape.
		return errs.Wrap(err, errs.CodeForbidden, "derivatives: "+op+": the database refused an unauthorized change").
			WithField("sqlstate", state)
	}
	if mapped := ledger.MapError(err); mapped != nil {
		if ee, ok := errs.As(mapped); ok && ee.Code != errs.CodeInternal {
			return ee
		}
	}
	return errs.Wrap(err, errs.CodeInternal, "derivatives: "+op)
}

// lockSpec share-locks an instrument's terms row and then resolves its
// terms into a Spec. Every operation that trades, runs, settles or exercises
// an instrument takes it after the clearing house's lock, so the terms it
// computes with are the terms in force until it commits: an amendment
// (instruments.Repository.AmendTerms takes the row FOR UPDATE) waits for it,
// and finds the positions it opened (the database then refuses the change,
// DV007); an operation that waited for an amendment reads the amended terms.
func lockSpec(ctx context.Context, tx pgx.Tx, repo *instruments.Repository, iid instruments.InstrumentID) (Spec, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM instrument_terms WHERE instrument_id = $1 FOR SHARE`, iid); err != nil {
		return Spec{}, dbErr("lock the instrument's terms", err)
	}
	return loadSpec(ctx, tx, repo, iid)
}

// loadSpec resolves an instrument's current terms into a Spec.
func loadSpec(ctx context.Context, q db.Querier, repo *instruments.Repository, iid instruments.InstrumentID) (Spec, error) {
	ins, err := repo.Get(ctx, q, iid)
	if err != nil {
		return Spec{}, err
	}
	v, err := repo.CurrentTerms(ctx, q, iid)
	if err != nil {
		return Spec{}, err
	}
	ar := assets.NewRepository()
	settle, err := ar.Get(ctx, q, v.Terms.SettlementAssetID)
	if err != nil {
		return Spec{}, err
	}
	var underlyingID *assets.AssetID
	switch {
	case v.Terms.BaseAssetID != nil:
		underlyingID = v.Terms.BaseAssetID
	case v.Terms.UnderlyingInstrumentID != nil:
		u, err := repo.Get(ctx, q, *v.Terms.UnderlyingInstrumentID)
		if err != nil {
			return Spec{}, err
		}
		underlyingID = u.BaseAssetID
	}
	if underlyingID == nil {
		return Spec{}, errs.New(errs.CodeValidationFailed, "derivatives: the instrument names no underlying asset").
			WithField("instrument_id", iid.String())
	}
	under, err := ar.Get(ctx, q, *underlyingID)
	if err != nil {
		return Spec{}, err
	}
	return NewSpec(ins, v.Terms, settle, under)
}

const positionColumns = `id, account_id, instrument_id, settlement_asset_id, status, quantity, cost_value, settled_value,
	realized_pnl, funding_pnl, margin_posted, shortfall, version, opened_at, closed_at`

func scanPosition(row pgx.Row) (Position, error) {
	var p Position
	if err := row.Scan(&p.ID, &p.AccountID, &p.InstrumentID, &p.SettlementAssetID, &p.Status, &p.Quantity, &p.CostValue,
		&p.SettledValue, &p.RealizedPnL, &p.FundingPnL, &p.MarginPosted, &p.Shortfall, &p.Version, &p.OpenedAt, &p.ClosedAt); err != nil {
		return Position{}, err
	}
	p.OpenedAt = p.OpenedAt.UTC()
	if p.ClosedAt != nil {
		t := p.ClosedAt.UTC()
		p.ClosedAt = &t
	}
	return p, nil
}

// lockOpenPosition returns the account's OPEN position on the instrument,
// row-locked, if there is one.
func lockOpenPosition(ctx context.Context, tx pgx.Tx, account accounts.AccountID, iid instruments.InstrumentID) (Position, bool, error) {
	p, err := scanPosition(tx.QueryRow(ctx, `SELECT `+positionColumns+` FROM derivative_positions
		WHERE account_id = $1 AND instrument_id = $2 AND status = 'OPEN' FOR UPDATE`, account, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Position{}, false, nil
	}
	if err != nil {
		return Position{}, false, dbErr("lock position", err)
	}
	return p, true, nil
}

// openPosition returns the account's OPEN position without locking it.
func openPosition(ctx context.Context, q db.Querier, account accounts.AccountID, iid instruments.InstrumentID) (Position, bool, error) {
	p, err := scanPosition(q.QueryRow(ctx, `SELECT `+positionColumns+` FROM derivative_positions
		WHERE account_id = $1 AND instrument_id = $2 AND status = 'OPEN'`, account, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Position{}, false, nil
	}
	if err != nil {
		return Position{}, false, dbErr("read position", err)
	}
	return p, true, nil
}

// GetPosition returns one position.
func GetPosition(ctx context.Context, q db.Querier, pid PositionID) (Position, error) {
	p, err := scanPosition(q.QueryRow(ctx, `SELECT `+positionColumns+` FROM derivative_positions WHERE id = $1`, pid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Position{}, errs.New(errs.CodeNotFound, "derivative position not found").WithField("position_id", pid.String())
	}
	if err != nil {
		return Position{}, dbErr("get position", err)
	}
	return p, nil
}

// insertPosition creates an OPEN position. It reports false, writing nothing,
// when a concurrent transaction committed an OPEN position for the same
// (account, instrument) first.
func insertPosition(ctx context.Context, tx pgx.Tx, p Position) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO derivative_positions
		(id, account_id, instrument_id, settlement_asset_id, status, quantity, cost_value, settled_value, realized_pnl,
		 funding_pnl, margin_posted, shortfall, version, opened_at)
		VALUES ($1,$2,$3,$4,'OPEN',$5,$6,$7,$8,$9,$10,$11,1,$12)
		ON CONFLICT (account_id, instrument_id) WHERE status = 'OPEN' DO NOTHING`,
		p.ID, p.AccountID, p.InstrumentID, p.SettlementAssetID, p.Quantity, p.CostValue, p.SettledValue, p.RealizedPnL,
		p.FundingPnL, p.MarginPosted, p.Shortfall, p.OpenedAt)
	if err != nil {
		return false, dbErr("insert position", err)
	}
	return tag.RowsAffected() == 1, nil
}

// savePosition writes a position's economic columns under optimistic
// concurrency on version (the row is already locked; the version check is a
// second guard).
func savePosition(ctx context.Context, tx pgx.Tx, p Position) (Position, error) {
	out, err := scanPosition(tx.QueryRow(ctx, `UPDATE derivative_positions SET
		quantity = $3, cost_value = $4, settled_value = $5, realized_pnl = $6, funding_pnl = $7, margin_posted = $8,
		shortfall = $9, version = version + 1
		WHERE id = $1 AND version = $2 RETURNING `+positionColumns,
		p.ID, p.Version, p.Quantity, p.CostValue, p.SettledValue, p.RealizedPnL, p.FundingPnL, p.MarginPosted, p.Shortfall))
	if errors.Is(err, pgx.ErrNoRows) {
		return Position{}, errs.New(errs.CodeConflict, "derivatives: position changed concurrently").WithField("position_id", p.ID.String())
	}
	if err != nil {
		return Position{}, dbErr("save position", err)
	}
	return out, nil
}

// saveAndClosePosition writes a flat position's final economics and moves it
// to CLOSED or SETTLED in one statement (an OPEN position may never hold zero
// contracts, not even between two statements), with the transition row the
// database binds the status change to.
func saveAndClosePosition(ctx context.Context, tx pgx.Tx, p Position, to PositionStatus, actor econ.Actor, reason string, at time.Time) (Position, error) {
	if !CanTransitionPosition(p.Status, to) {
		return Position{}, errs.Newf(errs.CodeInvalidStateTransition, "derivative position %s cannot move %s -> %s", p.ID, p.Status, to)
	}
	if !p.Quantity.IsZero() {
		return Position{}, errs.New(errs.CodeInternal, "derivatives: only a flat position closes").WithField("position_id", p.ID.String())
	}
	if _, err := tx.Exec(ctx, `INSERT INTO derivative_position_transitions
		(id, position_id, from_status, to_status, actor_type, actor_id, reason, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id.New[id.Any](), p.ID, p.Status, to, actor.Type, actor.ID, reason, at.UTC()); err != nil {
		return Position{}, dbErr("record position transition", err)
	}
	out, err := scanPosition(tx.QueryRow(ctx, `UPDATE derivative_positions SET
		quantity = $3, cost_value = $4, settled_value = $5, realized_pnl = $6, funding_pnl = $7, margin_posted = $8,
		shortfall = $9, status = $10, closed_at = $11, version = version + 1
		WHERE id = $1 AND version = $2 RETURNING `+positionColumns,
		p.ID, p.Version, p.Quantity, p.CostValue, p.SettledValue, p.RealizedPnL, p.FundingPnL, p.MarginPosted, p.Shortfall,
		to, at.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Position{}, errs.New(errs.CodeConflict, "derivatives: position changed concurrently").WithField("position_id", p.ID.String())
	}
	if err != nil {
		return Position{}, dbErr("close position", err)
	}
	return out, nil
}

// persistPosition writes a position after an operation: an insert for a new
// one, an update for an open one, and an update with the closing transition
// when it went flat.
func persistPosition(ctx context.Context, tx pgx.Tx, p Position, isNew bool, closeTo PositionStatus, actor econ.Actor, reason string, at time.Time) (Position, bool, error) {
	if isNew {
		ok, err := insertPosition(ctx, tx, p)
		return p, ok, err
	}
	if p.Quantity.IsZero() {
		out, err := saveAndClosePosition(ctx, tx, p, closeTo, actor, reason, at)
		return out, true, err
	}
	out, err := savePosition(ctx, tx, p)
	return out, true, err
}

// lockPositionsForInstrument row-locks the instrument's OPEN positions in
// (opened_at, id) order: the FIFO order of assignment and the one global
// lock order of every run.
func lockPositionsForInstrument(ctx context.Context, tx pgx.Tx, iid instruments.InstrumentID) ([]Position, error) {
	return listPositions(ctx, tx, `SELECT `+positionColumns+` FROM derivative_positions
		WHERE instrument_id = $1 AND status = 'OPEN' ORDER BY opened_at, id FOR UPDATE`, iid)
}

// lockPositionsOpenAt row-locks every position of the instrument that was
// open at t: opened before it and not closed before it.
func lockPositionsOpenAt(ctx context.Context, tx pgx.Tx, iid instruments.InstrumentID, t time.Time) ([]Position, error) {
	return listPositions(ctx, tx, `SELECT `+positionColumns+` FROM derivative_positions
		WHERE instrument_id = $1 AND opened_at < $2 AND (closed_at IS NULL OR closed_at >= $2)
		ORDER BY opened_at, id FOR UPDATE`, iid, t.UTC())
}

// openPositionsForAccount returns an account's OPEN positions.
func openPositionsForAccount(ctx context.Context, q db.Querier, account string) ([]Position, error) {
	return listPositions(ctx, q, `SELECT `+positionColumns+` FROM derivative_positions
		WHERE account_id = $1 AND status = 'OPEN' ORDER BY opened_at, id`, account)
}

func listPositions(ctx context.Context, q db.Querier, sql string, args ...any) ([]Position, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, dbErr("list positions", err)
	}
	defer rows.Close()
	var out []Position
	for rows.Next() {
		p, err := scanPosition(rows)
		if err != nil {
			return nil, dbErr("scan position", err)
		}
		out = append(out, p)
	}
	return out, dbErr("list positions", rows.Err())
}

// quantityAt is a position's signed quantity just before t: the sum of its
// fills that occurred before t.
func quantityAt(ctx context.Context, q db.Querier, pid PositionID, t time.Time) (money.Quantity, error) {
	var qty money.Quantity
	if err := q.QueryRow(ctx, `SELECT coalesce(sum(quantity), 0) FROM derivative_fills WHERE position_id = $1 AND occurred_at < $2`,
		pid, t.UTC()).Scan(&qty); err != nil {
		return money.Quantity{}, dbErr("position quantity at time", err)
	}
	return qty, nil
}

const fillColumns = `id, fill_key, position_id, account_id, instrument_id, source, side, quantity, price_mantissa, price_scale,
	value, realized_pnl, cash_settled, mark_id, slippage_bps, coalesce(intent_id::text, ''), coalesce(risk_decision_id::text, ''), occurred_at`

func scanFill(row pgx.Row) (FillRecord, error) {
	var f FillRecord
	var slip int64
	if err := row.Scan(&f.ID, &f.Key, &f.PositionID, &f.AccountID, &f.InstrumentID, &f.Source, &f.Side, &f.Quantity,
		&f.PriceMantissa, &f.PriceScale, &f.Value, &f.RealizedPnL, &f.CashSettled, &f.MarkID, &slip, &f.IntentID,
		&f.RiskDecisionID, &f.OccurredAt); err != nil {
		return FillRecord{}, err
	}
	f.SlippageBPS = money.BPS(slip)
	f.OccurredAt = f.OccurredAt.UTC()
	return f, nil
}

func fillByKey(ctx context.Context, q db.Querier, key string) (FillRecord, bool, error) {
	f, err := scanFill(q.QueryRow(ctx, `SELECT `+fillColumns+` FROM derivative_fills WHERE fill_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return FillRecord{}, false, nil
	}
	if err != nil {
		return FillRecord{}, false, dbErr("fill by key", err)
	}
	return f, true, nil
}

// FillsForIntent returns the fills an intent produced, oldest first.
func FillsForIntent(ctx context.Context, q db.Querier, intent econ.IntentID) ([]FillRecord, error) {
	rows, err := q.Query(ctx, `SELECT `+fillColumns+` FROM derivative_fills WHERE intent_id = $1 ORDER BY occurred_at, id`, intent)
	if err != nil {
		return nil, dbErr("fills for intent", err)
	}
	defer rows.Close()
	var out []FillRecord
	for rows.Next() {
		f, err := scanFill(rows)
		if err != nil {
			return nil, dbErr("scan fill", err)
		}
		out = append(out, f)
	}
	return out, dbErr("fills for intent", rows.Err())
}

func insertFill(ctx context.Context, tx pgx.Tx, f FillRecord) error {
	var intentID, decisionID any
	if f.IntentID != "" {
		intentID = f.IntentID
	}
	if f.RiskDecisionID != "" {
		decisionID = f.RiskDecisionID
	}
	_, err := tx.Exec(ctx, `INSERT INTO derivative_fills
		(id, fill_key, position_id, account_id, instrument_id, source, side, quantity, price_mantissa, price_scale, value,
		 realized_pnl, cash_settled, mark_id, slippage_bps, intent_id, risk_decision_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16::uuid,$17::uuid,$18)`,
		f.ID, f.Key, f.PositionID, f.AccountID, f.InstrumentID, f.Source, f.Side, f.Quantity, f.PriceMantissa, f.PriceScale,
		f.Value, f.RealizedPnL, f.CashSettled, f.MarkID, int64(f.SlippageBPS), intentID, decisionID, f.OccurredAt.UTC())
	if err != nil {
		if db.IsUniqueViolation(err) {
			return errs.Wrap(err, errs.CodeConflict, "derivatives: a fill with this key already exists").WithField("fill_key", f.Key)
		}
		return dbErr("insert fill", err)
	}
	return nil
}

func insertCashFlow(ctx context.Context, tx pgx.Tx, cf CashFlow, clearing econ.FacilityID, effect *econ.EffectID) error {
	_, err := tx.Exec(ctx, `INSERT INTO derivative_cash_flows
		(id, flow_key, position_id, account_id, instrument_id, settlement_asset_id, clearing_house_id, kind, amount, shortfall,
		 journal_transaction_id, effect_id, reservation_id, reference_type, reference_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		cf.ID, cf.Key, cf.PositionID, cf.AccountID, cf.InstrumentID, cf.SettlementAssetID, clearing, cf.Kind, cf.Amount,
		cf.Shortfall, cf.JournalTransactionID, effect, cf.ReservationID, cf.ReferenceType, cf.ReferenceID, cf.OccurredAt.UTC())
	if err != nil {
		return dbErr("insert cash flow", err)
	}
	return nil
}

// registerTotals sums the cash-flow register of one clearing house and asset:
// net is Σ amount (customer side), shortfall Σ shortfall.
func registerTotals(ctx context.Context, q db.Querier, clearing econ.FacilityID, asset assets.AssetID) (net, shortfall money.Quantity, flows int64, err error) {
	err = q.QueryRow(ctx, `SELECT coalesce(sum(amount), 0), coalesce(sum(shortfall), 0), count(*) FROM derivative_cash_flows
		WHERE clearing_house_id = $1 AND settlement_asset_id = $2`, clearing, asset).Scan(&net, &shortfall, &flows)
	if err != nil {
		return money.Quantity{}, money.Quantity{}, 0, dbErr("sum the cash-flow register", err)
	}
	return net, shortfall, flows, nil
}

// netOpenInterest is Σ quantity of the OPEN positions of every instrument
// settling in asset: zero for each instrument when the book is internally
// matched.
func netOpenInterest(ctx context.Context, q db.Querier, asset assets.AssetID) (map[string]money.Quantity, error) {
	rows, err := q.Query(ctx, `SELECT instrument_id::text, sum(quantity) FROM derivative_positions
		WHERE settlement_asset_id = $1 AND status = 'OPEN' GROUP BY instrument_id ORDER BY instrument_id`, asset)
	if err != nil {
		return nil, dbErr("net open interest", err)
	}
	defer rows.Close()
	out := map[string]money.Quantity{}
	for rows.Next() {
		var iid string
		var qty money.Quantity
		if err := rows.Scan(&iid, &qty); err != nil {
			return nil, dbErr("scan open interest", err)
		}
		out[iid] = qty
	}
	return out, dbErr("net open interest", rows.Err())
}
