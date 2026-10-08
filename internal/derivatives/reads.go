package derivatives

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
)

// The read model of this package's registers: newest-first, keyset-paged
// listings of marks, positions, the cash-flow register and funding accruals,
// and an instrument's settlement record. Readers (the HTTP adapter) page
// through these rather than through their own SQL, so the tables have one
// owner.

// MaxListLimit bounds every listing.
const MaxListLimit = 1000

// Keyset is where a newest-first listing resumes: the time and id of the last
// row of the previous page. Rows strictly before it in the listing's
// (time DESC, id DESC) order come next.
type Keyset struct {
	At time.Time
	ID string
}

func (k *Keyset) validate() error {
	if k == nil {
		return nil
	}
	if k.At.IsZero() {
		return errs.New(errs.CodeValidationFailed, "derivatives: a keyset names the time of the last row")
	}
	if _, err := id.ParseAny(k.ID); err != nil {
		return errs.New(errs.CodeValidationFailed, "derivatives: a keyset names the id of the last row")
	}
	return nil
}

func listLimit(limit int) (int, error) {
	if limit <= 0 || limit > MaxListLimit {
		return 0, errs.Newf(errs.CodeValidationFailed, "derivatives: a listing returns 1..%d rows", MaxListLimit)
	}
	return limit, nil
}

// sqlParams numbers the bound parameters of a statement assembled clause by
// clause. Every clause is constant text and every value a request supplies
// travels as a parameter, which test/security's constant-derivation scan
// proves of each statement below.
type sqlParams struct{ list []any }

// bind adds a parameter and returns its placeholder.
func (p *sqlParams) bind(v any) string {
	p.list = append(p.list, v)
	return "$" + strconv.Itoa(len(p.list))
}

// ListMarks pages an instrument's marks, newest observation first.
func ListMarks(ctx context.Context, q db.Querier, iid instruments.InstrumentID, after *Keyset, limit int) ([]Mark, error) {
	n, err := listLimit(limit)
	if err != nil {
		return nil, err
	}
	if err := after.validate(); err != nil {
		return nil, err
	}
	var p sqlParams
	sql := `SELECT ` + markColumns + ` FROM derivative_marks WHERE instrument_id = ` + p.bind(iid)
	if after != nil {
		sql += ` AND (observed_at, id) < (` + p.bind(after.At.UTC()) + `, ` + p.bind(after.ID) + `::uuid)`
	}
	sql += ` ORDER BY observed_at DESC, id DESC LIMIT ` + strconv.Itoa(n)
	rows, err := q.Query(ctx, sql, p.list...)
	if err != nil {
		return nil, dbErr("list marks", err)
	}
	defer rows.Close()
	out := []Mark{}
	for rows.Next() {
		m, err := scanMark(rows)
		if err != nil {
			return nil, dbErr("scan mark", err)
		}
		out = append(out, m)
	}
	return out, dbErr("list marks", rows.Err())
}

// PositionQuery selects an account's positions.
type PositionQuery struct {
	AccountID accounts.AccountID
	// Status "" is every status.
	Status PositionStatus
	After  *Keyset
	Limit  int
}

// ListPositions pages an account's positions, newest opened first.
func ListPositions(ctx context.Context, q db.Querier, pq PositionQuery) ([]Position, error) {
	n, err := listLimit(pq.Limit)
	if err != nil {
		return nil, err
	}
	if err := pq.After.validate(); err != nil {
		return nil, err
	}
	if pq.AccountID.IsZero() {
		return nil, errs.New(errs.CodeValidationFailed, "derivatives: positions are listed per account")
	}
	var p sqlParams
	sql := `SELECT ` + positionColumns + ` FROM derivative_positions WHERE account_id = ` + p.bind(pq.AccountID)
	if pq.Status != "" {
		sql += ` AND status = ` + p.bind(string(pq.Status))
	}
	if pq.After != nil {
		sql += ` AND (opened_at, id) < (` + p.bind(pq.After.At.UTC()) + `, ` + p.bind(pq.After.ID) + `::uuid)`
	}
	sql += ` ORDER BY opened_at DESC, id DESC LIMIT ` + strconv.Itoa(n)
	out, err := listPositions(ctx, q, sql, p.list...)
	if out == nil && err == nil {
		out = []Position{}
	}
	return out, err
}

// CashFlowQuery selects an account's cash flows.
type CashFlowQuery struct {
	AccountID  accounts.AccountID
	PositionID *PositionID
	// Kind "" is every kind.
	Kind  CashFlowKind
	After *Keyset
	Limit int
}

const cashFlowColumns = `id, flow_key, position_id, account_id, instrument_id, settlement_asset_id, kind, amount, shortfall,
	journal_transaction_id, reservation_id, reference_type, reference_id, occurred_at`

func scanCashFlow(row pgx.Row) (CashFlow, error) {
	var f CashFlow
	if err := row.Scan(&f.ID, &f.Key, &f.PositionID, &f.AccountID, &f.InstrumentID, &f.SettlementAssetID, &f.Kind, &f.Amount,
		&f.Shortfall, &f.JournalTransactionID, &f.ReservationID, &f.ReferenceType, &f.ReferenceID, &f.OccurredAt); err != nil {
		return CashFlow{}, err
	}
	f.OccurredAt = f.OccurredAt.UTC()
	return f, nil
}

// ListCashFlows pages an account's register of clearing-house cash flows,
// newest first (derivative_cash_flows_account_idx).
func ListCashFlows(ctx context.Context, q db.Querier, cq CashFlowQuery) ([]CashFlow, error) {
	n, err := listLimit(cq.Limit)
	if err != nil {
		return nil, err
	}
	if err := cq.After.validate(); err != nil {
		return nil, err
	}
	if cq.AccountID.IsZero() {
		return nil, errs.New(errs.CodeValidationFailed, "derivatives: cash flows are listed per account")
	}
	var p sqlParams
	sql := `SELECT ` + cashFlowColumns + ` FROM derivative_cash_flows WHERE account_id = ` + p.bind(cq.AccountID)
	if cq.PositionID != nil {
		sql += ` AND position_id = ` + p.bind(*cq.PositionID)
	}
	if cq.Kind != "" {
		sql += ` AND kind = ` + p.bind(string(cq.Kind))
	}
	if cq.After != nil {
		sql += ` AND (occurred_at, id) < (` + p.bind(cq.After.At.UTC()) + `, ` + p.bind(cq.After.ID) + `::uuid)`
	}
	sql += ` ORDER BY occurred_at DESC, id DESC LIMIT ` + strconv.Itoa(n)
	rows, err := q.Query(ctx, sql, p.list...)
	if err != nil {
		return nil, dbErr("list cash flows", err)
	}
	defer rows.Close()
	out := []CashFlow{}
	for rows.Next() {
		f, err := scanCashFlow(rows)
		if err != nil {
			return nil, dbErr("scan cash flow", err)
		}
		out = append(out, f)
	}
	return out, dbErr("list cash flows", rows.Err())
}

// FundingAccrual is one position's funding for one interval, with the rate
// the interval was charged at.
type FundingAccrual struct {
	ID                AccrualID
	FundingRateID     FundingRateID
	PositionID        PositionID
	InstrumentID      instruments.InstrumentID
	SettlementAssetID assets.AssetID
	IntervalStart     time.Time
	IntervalEnd       time.Time
	// Quantity is the position's signed quantity at the funding time.
	Quantity       money.Quantity
	Direction      FundingDirection
	Amount         money.Quantity
	RateE8         int64
	PremiumIndexE8 int64
	Clamped        bool
	PerContract    money.Quantity
	MarkMantissa   money.Quantity
	MarkScale      int32
	CashFlowID     *CashFlowID
}

// AccrualQuery selects the funding accrued to an account's positions.
type AccrualQuery struct {
	AccountID  accounts.AccountID
	PositionID *PositionID
	After      *Keyset
	Limit      int
}

// ListFundingAccruals pages the funding accrued to an account's positions,
// newest interval first.
func ListFundingAccruals(ctx context.Context, q db.Querier, aq AccrualQuery) ([]FundingAccrual, error) {
	n, err := listLimit(aq.Limit)
	if err != nil {
		return nil, err
	}
	if err := aq.After.validate(); err != nil {
		return nil, err
	}
	if aq.AccountID.IsZero() {
		return nil, errs.New(errs.CodeValidationFailed, "derivatives: funding accruals are listed per account")
	}
	var p sqlParams
	sql := `SELECT fa.id, fa.funding_rate_id, fa.position_id, p.instrument_id, p.settlement_asset_id,
		fa.interval_start, fr.interval_end, fa.quantity, fa.direction, fa.amount, fr.rate_e8, fr.premium_index_e8, fr.clamped,
		fr.per_contract_amount, fr.mark_mantissa, dm.mark_scale, fa.cash_flow_id
		FROM funding_accruals fa
		JOIN funding_rates fr ON fr.id = fa.funding_rate_id
		JOIN derivative_marks dm ON dm.id = fr.mark_id
		JOIN derivative_positions p ON p.id = fa.position_id
		WHERE p.account_id = ` + p.bind(aq.AccountID)
	if aq.PositionID != nil {
		sql += ` AND fa.position_id = ` + p.bind(*aq.PositionID)
	}
	if aq.After != nil {
		sql += ` AND (fa.interval_start, fa.id) < (` + p.bind(aq.After.At.UTC()) + `, ` + p.bind(aq.After.ID) + `::uuid)`
	}
	sql += ` ORDER BY fa.interval_start DESC, fa.id DESC LIMIT ` + strconv.Itoa(n)
	rows, err := q.Query(ctx, sql, p.list...)
	if err != nil {
		return nil, dbErr("list funding accruals", err)
	}
	defer rows.Close()
	out := []FundingAccrual{}
	for rows.Next() {
		var a FundingAccrual
		if err := rows.Scan(&a.ID, &a.FundingRateID, &a.PositionID, &a.InstrumentID, &a.SettlementAssetID, &a.IntervalStart,
			&a.IntervalEnd, &a.Quantity, &a.Direction, &a.Amount, &a.RateE8, &a.PremiumIndexE8, &a.Clamped, &a.PerContract,
			&a.MarkMantissa, &a.MarkScale, &a.CashFlowID); err != nil {
			return nil, dbErr("scan funding accrual", err)
		}
		a.IntervalStart, a.IntervalEnd = a.IntervalStart.UTC(), a.IntervalEnd.UTC()
		out = append(out, a)
	}
	return out, dbErr("list funding accruals", rows.Err())
}

// SettlementRecord is an instrument's final settlement as recorded.
type SettlementRecord struct {
	ID            SettlementID
	InstrumentID  instruments.InstrumentID
	Kind          SettlementKind
	PriceMantissa money.Quantity
	PriceScale    int32
	PriceSource   string
	// Rule is what decided the price; "" only for a settlement recorded
	// before migration 00755 bound settlements to a rule.
	Rule       SettlementPriceRule
	MarkID     *MarkID
	OverrideID *SettlementOverrideID
	Positions  int
	SettledAt  time.Time
}

// GetSettlement returns an instrument's final settlement, if it has one.
func GetSettlement(ctx context.Context, q db.Querier, iid instruments.InstrumentID) (SettlementRecord, bool, error) {
	var r SettlementRecord
	var rule *string
	err := q.QueryRow(ctx, `SELECT id, instrument_id, kind, settlement_mantissa, price_scale, price_source, price_rule, mark_id,
		override_id, positions, settled_at FROM derivative_settlements WHERE instrument_id = $1`, iid).
		Scan(&r.ID, &r.InstrumentID, &r.Kind, &r.PriceMantissa, &r.PriceScale, &r.PriceSource, &rule, &r.MarkID, &r.OverrideID,
			&r.Positions, &r.SettledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SettlementRecord{}, false, nil
	}
	if err != nil {
		return SettlementRecord{}, false, dbErr("read settlement", err)
	}
	if rule != nil {
		r.Rule = SettlementPriceRule(*rule)
	}
	r.SettledAt = r.SettledAt.UTC()
	return r, true, nil
}
