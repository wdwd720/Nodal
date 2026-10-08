package derivatives

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

// listing builds a newest-first keyset query clause by clause.
type listing struct {
	where []string
	args  []any
}

func (l *listing) arg(v any) string {
	l.args = append(l.args, v)
	return "$" + strconv.Itoa(len(l.args))
}

func (l *listing) cond(format string, v any) {
	l.where = append(l.where, fmt.Sprintf(format, l.arg(v)))
}

func (l *listing) after(timeCol, idCol string, k *Keyset) {
	if k == nil {
		return
	}
	at := l.arg(k.At.UTC())
	ref := l.arg(k.ID)
	l.where = append(l.where, fmt.Sprintf("(%s, %s) < (%s, %s::uuid)", timeCol, idCol, at, ref))
}

func (l *listing) sql(selectFrom, timeCol, idCol string, limit int) string {
	return selectFrom + " WHERE " + strings.Join(l.where, " AND ") +
		fmt.Sprintf(" ORDER BY %s DESC, %s DESC LIMIT %d", timeCol, idCol, limit)
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
	var l listing
	l.cond("instrument_id = %s", iid)
	l.after("observed_at", "id", after)
	rows, err := q.Query(ctx, l.sql(`SELECT `+markColumns+` FROM derivative_marks`, "observed_at", "id", n), l.args...)
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
	var l listing
	l.cond("account_id = %s", pq.AccountID)
	if pq.Status != "" {
		l.cond("status = %s", string(pq.Status))
	}
	l.after("opened_at", "id", pq.After)
	out, err := listPositions(ctx, q, l.sql(`SELECT `+positionColumns+` FROM derivative_positions`, "opened_at", "id", n), l.args...)
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
	var l listing
	l.cond("account_id = %s", cq.AccountID)
	if cq.PositionID != nil {
		l.cond("position_id = %s", *cq.PositionID)
	}
	if cq.Kind != "" {
		l.cond("kind = %s", string(cq.Kind))
	}
	l.after("occurred_at", "id", cq.After)
	rows, err := q.Query(ctx, l.sql(`SELECT `+cashFlowColumns+` FROM derivative_cash_flows`, "occurred_at", "id", n), l.args...)
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
	var l listing
	l.cond("p.account_id = %s", aq.AccountID)
	if aq.PositionID != nil {
		l.cond("fa.position_id = %s", *aq.PositionID)
	}
	l.after("fa.interval_start", "fa.id", aq.After)
	rows, err := q.Query(ctx, l.sql(`SELECT fa.id, fa.funding_rate_id, fa.position_id, p.instrument_id, p.settlement_asset_id,
		fa.interval_start, fr.interval_end, fa.quantity, fa.direction, fa.amount, fr.rate_e8, fr.premium_index_e8, fr.clamped,
		fr.per_contract_amount, fr.mark_mantissa, dm.mark_scale, fa.cash_flow_id
		FROM funding_accruals fa
		JOIN funding_rates fr ON fr.id = fa.funding_rate_id
		JOIN derivative_marks dm ON dm.id = fr.mark_id
		JOIN derivative_positions p ON p.id = fa.position_id`, "fa.interval_start", "fa.id", n), l.args...)
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
