package positions

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

type lotKind struct{}

type dispositionKind struct{}

// LotID identifies an acquisition lot.
type LotID = id.ID[lotKind]

// DispositionID identifies a lot disposition.
type DispositionID = id.ID[dispositionKind]

// NewLotID returns a fresh lot identifier.
func NewLotID() LotID { return id.New[lotKind]() }

// NewDispositionID returns a fresh disposition identifier.
func NewDispositionID() DispositionID { return id.New[dispositionKind]() }

// LotStatus is OPEN while any quantity remains, CLOSED once fully disposed.
type LotStatus string

// Lot statuses.
const (
	LotOpen   LotStatus = "OPEN"
	LotClosed LotStatus = "CLOSED"
)

// Ref is a financial event reference (mirrors ledger.FinancialEventReference):
// e.g. {"fill", "<uuid>"}, {"deposit", "<uuid>"}, {"reconciliation_record", "<uuid>"}.
type Ref struct {
	Type string
	ID   string
}

func (r Ref) empty() bool { return strings.TrimSpace(r.Type) == "" || strings.TrimSpace(r.ID) == "" }

// Lot is a position_lots row.
type Lot struct {
	ID               LotID
	AccountID        accounts.AccountID
	AssetID          assets.AssetID
	QuantityOriginal money.Quantity
	QuantityOpen     money.Quantity
	AcquiredAt       time.Time
	CostBasis        money.USD // total basis for QuantityOriginal, fees included
	Fees             money.USD // the part of CostBasis that is allocated fees
	BasisSource      string    // 'fill', 'funding', 'reconciliation_adjustment', ...
	ValuationSource  string    // price source used for the basis
	AcquisitionRef   Ref
	Venue            string
	WalletID         id.ID[id.Any] // zero when unknown
	JournalTxID      id.ID[id.Any] // zero when not linked
	Status           LotStatus
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// RemainingBasis is the basis carried by QuantityOpen (see package doc).
func (l Lot) RemainingBasis() (money.USD, error) {
	return remainingBasis(openLot{ID: l.ID, Original: l.QuantityOriginal, Open: l.QuantityOpen, CostBasis: l.CostBasis, AcquiredAt: l.AcquiredAt})
}

// AcquireLot is the input to Acquire. CostBasis of the lot is Cost + Fees.
type AcquireLot struct {
	AccountID       accounts.AccountID
	AssetID         assets.AssetID
	Quantity        money.Quantity
	AcquiredAt      time.Time
	Cost            money.USD // price paid for Quantity, excluding fees
	Fees            money.USD // fees allocated to this acquisition
	BasisSource     string
	ValuationSource string
	AcquisitionRef  Ref
	Venue           string
	WalletID        id.ID[id.Any]
	JournalTxID     id.ID[id.Any]
}

// Validate checks the structural rules of an acquisition.
func (a AcquireLot) Validate() error {
	var problems []string
	if a.AccountID.IsZero() {
		problems = append(problems, "account_id required")
	}
	if a.AssetID.IsZero() {
		problems = append(problems, "asset_id required")
	}
	if !a.Quantity.IsPositive() {
		problems = append(problems, "quantity must be positive")
	}
	if a.AcquiredAt.IsZero() {
		problems = append(problems, "acquired_at required")
	}
	if a.Cost.IsNegative() {
		problems = append(problems, "cost must not be negative")
	}
	if a.Fees.IsNegative() {
		problems = append(problems, "fees must not be negative")
	}
	if strings.TrimSpace(a.BasisSource) == "" {
		problems = append(problems, "basis_source required")
	}
	if strings.TrimSpace(a.ValuationSource) == "" {
		problems = append(problems, "valuation_source required")
	}
	if a.AcquisitionRef.empty() {
		problems = append(problems, "acquisition_ref required")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid lot acquisition").WithField("problems", problems)
	}
	return nil
}

// Disposal is the input to Dispose: a quantity leaving the position with
// its total proceeds and fees in USD.
type Disposal struct {
	AccountID       accounts.AccountID
	AssetID         assets.AssetID
	Quantity        money.Quantity
	DisposedAt      time.Time
	Proceeds        money.USD
	Fees            money.USD
	ValuationSource string
	DispositionRef  Ref
	JournalTxID     id.ID[id.Any]
}

// Validate checks the structural rules of a disposal.
func (d Disposal) Validate() error {
	var problems []string
	if d.AccountID.IsZero() {
		problems = append(problems, "account_id required")
	}
	if d.AssetID.IsZero() {
		problems = append(problems, "asset_id required")
	}
	if !d.Quantity.IsPositive() {
		problems = append(problems, "quantity must be positive")
	}
	if d.DisposedAt.IsZero() {
		problems = append(problems, "disposed_at required")
	}
	if d.Proceeds.IsNegative() {
		problems = append(problems, "proceeds must not be negative")
	}
	if d.Fees.IsNegative() {
		problems = append(problems, "fees must not be negative")
	}
	if strings.TrimSpace(d.ValuationSource) == "" {
		problems = append(problems, "valuation_source required")
	}
	if d.DispositionRef.empty() {
		problems = append(problems, "disposition_ref required")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid disposal").WithField("problems", problems)
	}
	return nil
}

// LotDisposition is a lot_dispositions row: the tax-quality record of one
// lot's share of a disposal (PART 125).
type LotDisposition struct {
	ID              DispositionID
	LotID           LotID
	AccountID       accounts.AccountID
	AssetID         assets.AssetID
	Quantity        money.Quantity
	DisposedAt      time.Time
	Proceeds        money.USD
	Fees            money.USD
	Basis           money.USD
	RealizedPnL     money.USD // Proceeds − Fees − Basis
	ValuationSource string
	DispositionRef  Ref
	JournalTxID     id.ID[id.Any]
	CorrectionOf    DispositionID
	CreatedAt       time.Time
}

// RealizedPnL summarizes dispositions.
type RealizedPnL struct {
	Quantity     money.Quantity
	Proceeds     money.USD
	Fees         money.USD
	Basis        money.USD
	PnL          money.USD // Proceeds − Fees − Basis
	Dispositions int
}

func (r *RealizedPnL) add(d LotDisposition) error {
	var err error
	r.Quantity = r.Quantity.Add(d.Quantity)
	if r.Proceeds, err = r.Proceeds.Add(d.Proceeds); err != nil {
		return mapMoney(err)
	}
	if r.Fees, err = r.Fees.Add(d.Fees); err != nil {
		return mapMoney(err)
	}
	if r.Basis, err = r.Basis.Add(d.Basis); err != nil {
		return mapMoney(err)
	}
	if r.PnL, err = r.PnL.Add(d.RealizedPnL); err != nil {
		return mapMoney(err)
	}
	r.Dispositions++
	return nil
}

// AssetRealizedPnL is the per-asset slice of a report.
type AssetRealizedPnL struct {
	AssetID assets.AssetID
	RealizedPnL
}

// RealizedPnLReport is the result of Engine.RealizedPnL.
type RealizedPnLReport struct {
	Since, Until time.Time // half-open [Since, Until)
	Total        RealizedPnL
	ByAsset      []AssetRealizedPnL // ordered by asset id
}

// AverageBasisDecimals is the number of fractional digits in
// Holding.AverageBasis.
const AverageBasisDecimals uint8 = 8

// Holding is the open position in one asset.
type Holding struct {
	AssetID  assets.AssetID
	Symbol   string
	Decimals uint8
	Quantity money.Quantity // Σ quantity_open
	// CostBasis is Σ remaining basis of the open lots.
	CostBasis money.USD
	// AverageBasis is CostBasis per whole unit rendered with
	// AverageBasisDecimals fractional digits (RoundHalfEven). It is a
	// display value and must never be used for arithmetic.
	AverageBasis     string
	LotCount         int
	OldestAcquiredAt time.Time
}

// Drift is a difference between open lots and the WALLET ledger balance.
type Drift struct {
	AssetID        assets.AssetID
	LotQuantity    money.Quantity
	LedgerQuantity money.Quantity
	Difference     money.Quantity // LotQuantity − LedgerQuantity
}

// LotEngine is the fixed contract (FINANCIAL_MODEL §7).
type LotEngine interface {
	Acquire(ctx context.Context, tx pgx.Tx, in AcquireLot) (Lot, error)
	Dispose(ctx context.Context, tx pgx.Tx, d Disposal) ([]LotDisposition, RealizedPnL, error)
	Holdings(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]Holding, error)
	VerifyAgainstLedger(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]Drift, error)
}

// Engine implements LotEngine over position_lots and lot_dispositions.
type Engine struct{}

// NewEngine returns an Engine.
func NewEngine() *Engine { return &Engine{} }

var _ LotEngine = (*Engine)(nil)

const lotColumns = `id, account_id, asset_id, quantity_original, quantity_open, acquired_at, cost_basis_usd_minor, fees_usd_minor,
	basis_source, valuation_source, acquisition_ref_type, acquisition_ref_id, coalesce(venue,''), wallet_id, journal_transaction_id,
	status, created_at, updated_at`

func scanLot(row pgx.Row) (Lot, error) {
	var l Lot
	if err := row.Scan(&l.ID, &l.AccountID, &l.AssetID, &l.QuantityOriginal, &l.QuantityOpen, &l.AcquiredAt, &l.CostBasis, &l.Fees,
		&l.BasisSource, &l.ValuationSource, &l.AcquisitionRef.Type, &l.AcquisitionRef.ID, &l.Venue, &l.WalletID, &l.JournalTxID,
		&l.Status, &l.CreatedAt, &l.UpdatedAt); err != nil {
		return Lot{}, err
	}
	l.AcquiredAt, l.CreatedAt, l.UpdatedAt = l.AcquiredAt.UTC(), l.CreatedAt.UTC(), l.UpdatedAt.UTC()
	return l, nil
}

const dispositionColumns = `id, lot_id, account_id, asset_id, quantity, disposed_at, proceeds_usd_minor, fees_usd_minor, basis_usd_minor,
	realized_pnl_usd_minor, valuation_source, disposition_ref_type, disposition_ref_id, journal_transaction_id, correction_of, created_at`

func scanDisposition(row pgx.Row) (LotDisposition, error) {
	var d LotDisposition
	if err := row.Scan(&d.ID, &d.LotID, &d.AccountID, &d.AssetID, &d.Quantity, &d.DisposedAt, &d.Proceeds, &d.Fees, &d.Basis,
		&d.RealizedPnL, &d.ValuationSource, &d.DispositionRef.Type, &d.DispositionRef.ID, &d.JournalTxID, &d.CorrectionOf, &d.CreatedAt); err != nil {
		return LotDisposition{}, err
	}
	d.DisposedAt, d.CreatedAt = d.DisposedAt.UTC(), d.CreatedAt.UTC()
	return d, nil
}

// Acquire inserts an OPEN lot whose cost basis is Cost + Fees.
func (e *Engine) Acquire(ctx context.Context, tx pgx.Tx, in AcquireLot) (Lot, error) {
	if err := in.Validate(); err != nil {
		return Lot{}, err
	}
	basis, err := in.Cost.Add(in.Fees)
	if err != nil {
		return Lot{}, mapMoney(err)
	}
	row := tx.QueryRow(ctx, `INSERT INTO position_lots
		(id, account_id, asset_id, quantity_original, quantity_open, acquired_at, cost_basis_usd_minor, fees_usd_minor,
		 basis_source, valuation_source, acquisition_ref_type, acquisition_ref_id, venue, wallet_id, journal_transaction_id, status)
		VALUES ($1,$2,$3,$4,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13,$14,'OPEN')
		RETURNING `+lotColumns,
		NewLotID(), in.AccountID, in.AssetID, in.Quantity, in.AcquiredAt.UTC(), basis, in.Fees,
		in.BasisSource, in.ValuationSource, in.AcquisitionRef.Type, in.AcquisitionRef.ID, in.Venue, in.WalletID, in.JournalTxID)
	l, err := scanLot(row)
	if err != nil {
		if db.IsForeignKeyViolation(err) {
			return Lot{}, errs.Wrap(err, errs.CodeNotFound, "account, asset or journal transaction not found")
		}
		if db.IsCheckViolation(err) {
			return Lot{}, errs.Wrap(err, errs.CodeValidationFailed, "lot rejected by schema constraint")
		}
		return Lot{}, fmt.Errorf("positions: acquire: %w", err)
	}
	return l, nil
}

// Dispose consumes d.Quantity from the account's open lots in the asset,
// FIFO by (acquired_at, id), under SELECT ... FOR UPDATE. Lots are split as
// needed and one LotDisposition is written per lot touched. Insufficient
// open quantity → VALIDATION_FAILED and nothing is written.
func (e *Engine) Dispose(ctx context.Context, tx pgx.Tx, d Disposal) ([]LotDisposition, RealizedPnL, error) {
	if err := d.Validate(); err != nil {
		return nil, RealizedPnL{}, err
	}
	rows, err := tx.Query(ctx, `SELECT id, quantity_original, quantity_open, cost_basis_usd_minor, acquired_at FROM position_lots
		WHERE account_id = $1 AND asset_id = $2 AND status = 'OPEN' AND quantity_open > 0
		ORDER BY acquired_at, id FOR UPDATE`, d.AccountID, d.AssetID)
	if err != nil {
		return nil, RealizedPnL{}, fmt.Errorf("positions: lock lots: %w", err)
	}
	var lots []openLot
	for rows.Next() {
		var l openLot
		if err := rows.Scan(&l.ID, &l.Original, &l.Open, &l.CostBasis, &l.AcquiredAt); err != nil {
			rows.Close()
			return nil, RealizedPnL{}, fmt.Errorf("positions: scan lot: %w", err)
		}
		lots = append(lots, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, RealizedPnL{}, fmt.Errorf("positions: read lots: %w", err)
	}

	allocs, err := allocateFIFO(lots, d.Quantity, d.Proceeds, d.Fees)
	if err != nil {
		return nil, RealizedPnL{}, err
	}

	var out []LotDisposition
	var total RealizedPnL
	for _, a := range allocs {
		pnl, err := a.pnl()
		if err != nil {
			return nil, RealizedPnL{}, err
		}
		tag, err := tx.Exec(ctx, `UPDATE position_lots
			SET quantity_open = $2, status = CASE WHEN $2::numeric = 0 THEN 'CLOSED' ELSE 'OPEN' END
			WHERE id = $1 AND quantity_open = $3`, a.Lot.ID, a.OpenAfter, a.Lot.Open)
		if err != nil {
			return nil, RealizedPnL{}, fmt.Errorf("positions: update lot: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return nil, RealizedPnL{}, errs.New(errs.CodeConflict, "lot changed while being disposed").WithField("lot_id", a.Lot.ID.String())
		}
		row := tx.QueryRow(ctx, `INSERT INTO lot_dispositions
			(id, lot_id, account_id, asset_id, quantity, disposed_at, proceeds_usd_minor, fees_usd_minor, basis_usd_minor, realized_pnl_usd_minor,
			 valuation_source, disposition_ref_type, disposition_ref_id, journal_transaction_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			RETURNING `+dispositionColumns,
			NewDispositionID(), a.Lot.ID, d.AccountID, d.AssetID, a.Take, d.DisposedAt.UTC(), a.Proceeds, a.Fees, a.Basis, pnl,
			d.ValuationSource, d.DispositionRef.Type, d.DispositionRef.ID, d.JournalTxID)
		disp, err := scanDisposition(row)
		if err != nil {
			if db.IsForeignKeyViolation(err) {
				return nil, RealizedPnL{}, errs.Wrap(err, errs.CodeNotFound, "journal transaction not found")
			}
			return nil, RealizedPnL{}, fmt.Errorf("positions: insert disposition: %w", err)
		}
		if err := total.add(disp); err != nil {
			return nil, RealizedPnL{}, err
		}
		out = append(out, disp)
	}
	return out, total, nil
}

// Holdings returns the open position per asset, ordered by asset id.
func (e *Engine) Holdings(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]Holding, error) {
	if accountID.IsZero() {
		return nil, errs.New(errs.CodeValidationFailed, "account_id required")
	}
	rows, err := q.Query(ctx, `SELECT l.id, l.asset_id, a.symbol, a.decimals, l.quantity_original, l.quantity_open, l.cost_basis_usd_minor, l.acquired_at
		FROM position_lots l JOIN assets a ON a.id = l.asset_id
		WHERE l.account_id = $1 AND l.status = 'OPEN' AND l.quantity_open > 0
		ORDER BY l.asset_id, l.acquired_at, l.id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("positions: holdings: %w", err)
	}
	defer rows.Close()
	var out []Holding
	for rows.Next() {
		var l openLot
		var assetID assets.AssetID
		var symbol string
		var decimals int16
		if err := rows.Scan(&l.ID, &assetID, &symbol, &decimals, &l.Original, &l.Open, &l.CostBasis, &l.AcquiredAt); err != nil {
			return nil, fmt.Errorf("positions: holdings scan: %w", err)
		}
		rem, err := remainingBasis(l)
		if err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].AssetID != assetID {
			if decimals < 0 || decimals > 18 {
				return nil, fmt.Errorf("positions: asset %s decimals %d out of range", assetID, decimals)
			}
			out = append(out, Holding{AssetID: assetID, Symbol: symbol, Decimals: uint8(decimals), OldestAcquiredAt: l.AcquiredAt.UTC()})
		}
		h := &out[len(out)-1]
		h.Quantity = h.Quantity.Add(l.Open)
		if h.CostBasis, err = h.CostBasis.Add(rem); err != nil {
			return nil, mapMoney(err)
		}
		h.LotCount++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("positions: holdings rows: %w", err)
	}
	for i := range out {
		out[i].AverageBasis = averageBasis(out[i].CostBasis, out[i].Quantity, out[i].Decimals)
	}
	return out, nil
}

// averageBasis renders basis / (qty / 10^decimals) USD per whole unit with
// AverageBasisDecimals fractional digits. qty must be positive.
func averageBasis(basis money.USD, qty money.Quantity, decimals uint8) string {
	if !qty.IsPositive() {
		return money.Quantity{}.ToDecimalString(AverageBasisDecimals)
	}
	// basis_minor × 10^decimals × 10^AverageBasisDecimals / (qty × 100)
	num := money.QuantityFromInt64(basis.Minor()).ScaleUp(decimals + AverageBasisDecimals)
	den := qty.Mul(money.QuantityFromInt64(100))
	r, err := num.Div(den, money.RoundHalfEven)
	if err != nil {
		// Unreachable: den is positive and the mode is valid.
		return money.Quantity{}.ToDecimalString(AverageBasisDecimals)
	}
	return r.ToDecimalString(AverageBasisDecimals)
}

// VerifyAgainstLedger compares Σ open lot quantity per asset with the
// account's WALLET ledger balance per asset and returns every difference,
// ordered by asset id. An empty result means the two agree.
func (e *Engine) VerifyAgainstLedger(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]Drift, error) {
	if accountID.IsZero() {
		return nil, errs.New(errs.CodeValidationFailed, "account_id required")
	}
	lots := map[assets.AssetID]money.Quantity{}
	rows, err := q.Query(ctx, `SELECT asset_id, sum(quantity_open) FROM position_lots
		WHERE account_id = $1 AND status = 'OPEN' GROUP BY asset_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("positions: verify: lots: %w", err)
	}
	for rows.Next() {
		var a assets.AssetID
		var qty money.Quantity
		if err := rows.Scan(&a, &qty); err != nil {
			rows.Close()
			return nil, fmt.Errorf("positions: verify: lots scan: %w", err)
		}
		lots[a] = qty
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("positions: verify: lots rows: %w", err)
	}

	ledger := map[assets.AssetID]money.Quantity{}
	rows, err = q.Query(ctx, `SELECT la.asset_id, coalesce(lb.balance, 0)
		FROM ledger_accounts la LEFT JOIN ledger_balances lb ON lb.ledger_account_id = la.id
		WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1 AND la.code = 'WALLET'`, accountID)
	if err != nil {
		return nil, fmt.Errorf("positions: verify: ledger: %w", err)
	}
	for rows.Next() {
		var a assets.AssetID
		var qty money.Quantity
		if err := rows.Scan(&a, &qty); err != nil {
			rows.Close()
			return nil, fmt.Errorf("positions: verify: ledger scan: %w", err)
		}
		ledger[a] = qty
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("positions: verify: ledger rows: %w", err)
	}

	seen := map[assets.AssetID]struct{}{}
	var ids []assets.AssetID
	for a := range lots {
		seen[a] = struct{}{}
		ids = append(ids, a)
	}
	for a := range ledger {
		if _, ok := seen[a]; !ok {
			ids = append(ids, a)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return id.Compare(ids[i], ids[j]) < 0 })
	var out []Drift
	for _, a := range ids {
		lq, wq := lots[a], ledger[a]
		if lq.Equal(wq) {
			continue
		}
		out = append(out, Drift{AssetID: a, LotQuantity: lq, LedgerQuantity: wq, Difference: lq.Sub(wq)})
	}
	return out, nil
}

// RealizedPnL aggregates dispositions with disposed_at in [since, until).
func (e *Engine) RealizedPnL(ctx context.Context, q db.Querier, accountID accounts.AccountID, since, until time.Time) (RealizedPnLReport, error) {
	if accountID.IsZero() {
		return RealizedPnLReport{}, errs.New(errs.CodeValidationFailed, "account_id required")
	}
	if !until.After(since) {
		return RealizedPnLReport{}, errs.New(errs.CodeValidationFailed, "until must be after since")
	}
	report := RealizedPnLReport{Since: since.UTC(), Until: until.UTC()}
	rows, err := q.Query(ctx, `SELECT asset_id, count(*), sum(quantity), sum(proceeds_usd_minor), sum(fees_usd_minor), sum(basis_usd_minor), sum(realized_pnl_usd_minor)
		FROM lot_dispositions WHERE account_id = $1 AND disposed_at >= $2 AND disposed_at < $3
		GROUP BY asset_id ORDER BY asset_id`, accountID, since.UTC(), until.UTC())
	if err != nil {
		return RealizedPnLReport{}, fmt.Errorf("positions: realized pnl: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a AssetRealizedPnL
		var count int64
		var proceeds, fees, basis, pnl money.Quantity
		if err := rows.Scan(&a.AssetID, &count, &a.Quantity, &proceeds, &fees, &basis, &pnl); err != nil {
			return RealizedPnLReport{}, fmt.Errorf("positions: realized pnl scan: %w", err)
		}
		a.Dispositions = int(count)
		if a.Proceeds, err = usdFromQuantity(proceeds); err != nil {
			return RealizedPnLReport{}, err
		}
		if a.Fees, err = usdFromQuantity(fees); err != nil {
			return RealizedPnLReport{}, err
		}
		if a.Basis, err = usdFromQuantity(basis); err != nil {
			return RealizedPnLReport{}, err
		}
		if a.PnL, err = usdFromQuantity(pnl); err != nil {
			return RealizedPnLReport{}, err
		}
		report.ByAsset = append(report.ByAsset, a)
		t := &report.Total
		t.Quantity = t.Quantity.Add(a.Quantity)
		t.Dispositions += a.Dispositions
		if t.Proceeds, err = t.Proceeds.Add(a.Proceeds); err != nil {
			return RealizedPnLReport{}, mapMoney(err)
		}
		if t.Fees, err = t.Fees.Add(a.Fees); err != nil {
			return RealizedPnLReport{}, mapMoney(err)
		}
		if t.Basis, err = t.Basis.Add(a.Basis); err != nil {
			return RealizedPnLReport{}, mapMoney(err)
		}
		if t.PnL, err = t.PnL.Add(a.PnL); err != nil {
			return RealizedPnLReport{}, mapMoney(err)
		}
	}
	if err := rows.Err(); err != nil {
		return RealizedPnLReport{}, fmt.Errorf("positions: realized pnl rows: %w", err)
	}
	return report, nil
}

// usdFromQuantity converts an exact SQL sum of cents (numeric) to USD.
func usdFromQuantity(q money.Quantity) (money.USD, error) {
	n, err := q.Int64()
	if err != nil {
		return money.USD{}, mapMoney(err)
	}
	return money.USDFromMinor(n), nil
}
