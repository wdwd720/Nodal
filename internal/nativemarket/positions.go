package nativemarket

import (
	"context"
	"math/big"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Native positions and P&L (product goal §15 PORTFOLIO).
//
// # Where the numbers come from
//
// Nowhere new. Quantity, cost basis, realised P&L and fees paid are maintained
// by migration 00772's triggers from the fills and the creator allocation, and
// a table CHECK states the identity that makes the quantity checkable:
//
//	quantity = allocation_units + units_bought_total - units_sold_total
//
// cp_app cannot write a row here at all, so there is no application path by
// which a cost basis can be set to something the trades do not support, and
// cp_native_positions_unreconciled() compares every row against the ledger's
// own balance projection -- the source these triggers do not write.
//
// # What is derived here and why it is derived rather than stored
//
// Market value and unrealised P&L. Both depend on the market's CURRENT price,
// which changes on every trade by anyone; storing them would mean rewriting
// every holder's row on every fill, and the row would still be stale between
// fills. They are computed at read time from the market's own reserves, and the
// response says which instant it was computed at (§46: a number on a screen
// that does not say when it was true is a number nobody can act on).
//
// Unrealised P&L is marked at the MARGINAL price, not at what the position
// would actually realise if it were sold. Selling a large position moves the
// price down, so a mark-to-marginal figure is optimistic for exactly the
// holders it matters most for. It is the honest choice anyway: the alternative
// -- marking each holder at their own exit price -- means every holder sees a
// different price for the same asset, and no two portfolio pages agree. The
// order ticket is where the real exit price is quoted, against current state,
// which is where a user finds out what their exit is actually worth.

// Position is one account's holding of one native asset, with the P&L that
// follows from its trades.
type Position struct {
	AccountID accounts.AccountID
	AssetID   assets.AssetID

	Quantity           money.Quantity
	CostBasisCredits   money.Quantity
	RealizedPnLCredits money.Quantity
	FeesPaidCredits    money.Quantity

	AllocationUnits  money.Quantity
	UnitsBoughtTotal money.Quantity
	UnitsSoldTotal   money.Quantity
	CreditsInTotal   money.Quantity
	CreditsOutTotal  money.Quantity
	FillCount        int64

	FirstAcquiredAt *time.Time
	LastTradeAt     *time.Time
	UpdatedAt       time.Time
}

// AverageCostCredits is the cost of one base unit held, scaled by PriceScale so
// it is directly comparable with a market price.
//
// A closed position has no average cost, and reporting zero would read as "it
// cost nothing" rather than "there is nothing".
func (p Position) AverageCostCredits() (money.Quantity, bool) {
	if p.Quantity.Sign() <= 0 {
		return money.Quantity{}, false
	}
	scaled := new(big.Int).Mul(p.CostBasisCredits.BigInt(), priceScaleFactor)
	return money.QuantityFromBigInt(scaled.Quo(scaled, p.Quantity.BigInt())), true
}

// MarketValueCredits is what the holding is worth at a marginal price, in
// Credit base units, rounded DOWN.
//
// Down, always: a rounding that flattered a portfolio by a base unit would be a
// rounding in the platform's favour on the one number a user reads as theirs.
func MarketValueCredits(quantity, priceScaled money.Quantity) money.Quantity {
	if quantity.Sign() <= 0 || priceScaled.Sign() <= 0 {
		return money.Quantity{}
	}
	v := new(big.Int).Mul(quantity.BigInt(), priceScaled.BigInt())
	return money.QuantityFromBigInt(v.Quo(v, priceScaleFactor))
}

// PositionValuation is a position marked against a market at an instant.
type PositionValuation struct {
	Position Position

	// MarketID and MarketStatus are the venue this was marked against. A
	// position in an asset whose market has been delisted still exists and
	// still says so.
	MarketID     MarketID
	MarketStatus Status

	// SpotPrice is the marginal price at AsOf, scaled by PriceScale.
	SpotPrice   money.Quantity
	PriceScale  int
	MarketValue money.Quantity
	// UnrealizedPnL is MarketValue - CostBasisCredits. Signed.
	UnrealizedPnL money.Quantity
	// TotalPnL is realised plus unrealised.
	TotalPnL money.Quantity
	AsOf     time.Time
}

const positionColumns = `p.account_id, p.asset_id, p.quantity::text, p.cost_basis_credits::text,
	p.realized_pnl_credits::text, p.fees_paid_credits::text, p.allocation_units::text,
	p.units_bought_total::text, p.units_sold_total::text, p.credits_in_total::text,
	p.credits_out_total::text, p.fill_count, p.first_acquired_at, p.last_trade_at, p.updated_at`

// Positions returns every native position an account holds or has ever held.
//
// Closed positions are included. A portfolio that hid them would lose the
// realised P&L attached to them, and "what did I make on the thing I already
// sold" is the question §15 exists to answer.
func (s *Service) Positions(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]Position, error) {
	rows, err := q.Query(ctx,
		`SELECT `+positionColumns+`
		   FROM native_positions p
		  WHERE p.account_id = $1
		  ORDER BY p.quantity DESC, p.asset_id`, accountID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Position
	for rows.Next() {
		p, serr := scanPosition(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, p)
	}
	return out, mapError(rows.Err())
}

// Position returns one account's holding of one asset, or a zero position.
func (s *Service) Position(ctx context.Context, q db.Querier, accountID accounts.AccountID, assetID assets.AssetID) (Position, error) {
	rows, err := q.Query(ctx,
		`SELECT `+positionColumns+`
		   FROM native_positions p
		  WHERE p.account_id = $1 AND p.asset_id = $2`, accountID, assetID)
	if err != nil {
		return Position{}, mapError(err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Position{}, mapError(err)
		}
		// Not an error: "you hold none of this" is an answer, and a 404 here
		// would make an empty position indistinguishable from a missing asset.
		return Position{AccountID: accountID, AssetID: assetID}, nil
	}
	p, err := scanPosition(rows)
	if err != nil {
		return Position{}, err
	}
	return p, mapError(rows.Err())
}

type rowScanner interface{ Scan(dest ...any) error }

func scanPosition(row rowScanner) (Position, error) {
	var (
		p                              Position
		qty, basis, realized, fees     string
		alloc, bought, sold, cin, cout string
	)
	if err := row.Scan(&p.AccountID, &p.AssetID, &qty, &basis, &realized, &fees,
		&alloc, &bought, &sold, &cin, &cout, &p.FillCount,
		&p.FirstAcquiredAt, &p.LastTradeAt, &p.UpdatedAt); err != nil {
		return Position{}, mapError(err)
	}
	for dst, src := range map[*money.Quantity]string{
		&p.Quantity: qty, &p.CostBasisCredits: basis, &p.RealizedPnLCredits: realized,
		&p.FeesPaidCredits: fees, &p.AllocationUnits: alloc, &p.UnitsBoughtTotal: bought,
		&p.UnitsSoldTotal: sold, &p.CreditsInTotal: cin, &p.CreditsOutTotal: cout,
	} {
		v, err := money.ParseQuantity(src)
		if err != nil {
			return Position{}, errs.Wrap(err, errs.CodeInternal, "nativemarket: a position field is not an integer")
		}
		*dst = v
	}
	p.UpdatedAt = p.UpdatedAt.UTC()
	return p, nil
}

// ApplyBuy and ApplySell state, in Go, exactly what migration 00772's triggers
// do in SQL.
//
// They are not the production path -- a position is written only by the
// database, and nothing in this package calls these -- and that is what makes
// them useful: TestIntegration_PositionsAgreeWithTheGoStatementOfTheArithmetic
// drives real fills through the engine and compares the rows the triggers wrote
// against the rows these functions produce. Two independent statements of the
// same arithmetic, written in different languages, that must agree.
//
// The rounding is the part worth stating twice. A partial exit removes the
// TRUNCATED share of the basis, so the basis left behind is never short of the
// units left behind; a full exit removes the whole basis exactly, so a closed
// position can never keep a few base units of cost attached to nothing. Getting
// that backwards in either implementation is a slow leak in somebody's reported
// P&L, and the CHECK in 00772 would not catch it.

// ApplyBuy returns the position after a buy of assetsOut units for creditsIn
// Credits, of which fees were fees.
func (p Position) ApplyBuy(assetsOut, creditsIn, fees money.Quantity, at time.Time) Position {
	out := p
	out.Quantity = p.Quantity.Add(assetsOut)
	out.CostBasisCredits = p.CostBasisCredits.Add(creditsIn)
	out.FeesPaidCredits = p.FeesPaidCredits.Add(fees)
	out.UnitsBoughtTotal = p.UnitsBoughtTotal.Add(assetsOut)
	out.CreditsInTotal = p.CreditsInTotal.Add(creditsIn)
	out.FillCount = p.FillCount + 1
	if out.FirstAcquiredAt == nil {
		t := at.UTC()
		out.FirstAcquiredAt = &t
	}
	t := at.UTC()
	out.LastTradeAt = &t
	return out
}

// ApplySell returns the position after a sell of assetsIn units for creditsOut
// Credits net of fees.
//
// It refuses a sell the position cannot support rather than producing a
// negative quantity, for the reason the trigger does: a negative quantity would
// put a disagreement with the ledger INSIDE the read model, where the
// reconciliation query could no longer see it.
func (p Position) ApplySell(assetsIn, creditsOut, fees money.Quantity, at time.Time) (Position, error) {
	if p.Quantity.Cmp(assetsIn) < 0 {
		return Position{}, errs.Newf(errs.CodeValidationFailed,
			"this position holds %s and cannot deliver %s", p.Quantity, assetsIn)
	}
	costRemoved := p.CostBasisCredits
	if assetsIn.Cmp(p.Quantity) != 0 {
		v := new(big.Int).Mul(p.CostBasisCredits.BigInt(), assetsIn.BigInt())
		costRemoved = money.QuantityFromBigInt(v.Quo(v, p.Quantity.BigInt()))
	}
	out := p
	out.Quantity = p.Quantity.Sub(assetsIn)
	out.CostBasisCredits = p.CostBasisCredits.Sub(costRemoved)
	out.RealizedPnLCredits = p.RealizedPnLCredits.Add(creditsOut.Sub(costRemoved))
	out.FeesPaidCredits = p.FeesPaidCredits.Add(fees)
	out.UnitsSoldTotal = p.UnitsSoldTotal.Add(assetsIn)
	out.CreditsOutTotal = p.CreditsOutTotal.Add(creditsOut)
	out.FillCount = p.FillCount + 1
	t := at.UTC()
	out.LastTradeAt = &t
	return out, nil
}

// HoldsInvariant is the CHECK migration 00772 enforces, stated in Go:
// the quantity is the allocation plus everything bought less everything sold,
// and a closed position carries no basis.
func (p Position) HoldsInvariant() bool {
	if p.Quantity.Cmp(p.AllocationUnits.Add(p.UnitsBoughtTotal).Sub(p.UnitsSoldTotal)) != 0 {
		return false
	}
	if p.Quantity.Sign() <= 0 && p.CostBasisCredits.Sign() != 0 {
		return false
	}
	return !p.Quantity.IsNegative() && !p.CostBasisCredits.IsNegative()
}

// Value marks a position against a market's current state.
func Value(p Position, m Market, st State, asOf time.Time) PositionValuation {
	spot := SpotPrice(m.Curve, st)
	value := MarketValueCredits(p.Quantity, spot)
	unrealized := value.Sub(p.CostBasisCredits)
	return PositionValuation{
		Position:      p,
		MarketID:      m.ID,
		MarketStatus:  m.Status,
		SpotPrice:     spot,
		PriceScale:    PriceScale,
		MarketValue:   value,
		UnrealizedPnL: unrealized,
		TotalPnL:      unrealized.Add(p.RealizedPnLCredits),
		AsOf:          asOf.UTC(),
	}
}

// UnreconciledPosition is one position that disagrees with the ledger.
type UnreconciledPosition struct {
	AccountID        accounts.AccountID
	AssetID          assets.AssetID
	PositionQuantity money.Quantity
	LedgerBalance    money.Quantity
}

// VerifyPositions reports every native position that does not equal the
// ledger's balance for the same account and asset.
//
// It is the proof the read model is a read model. The comparison is made by the
// database function migration 00772 installs, against ledger_balances -- which
// neither this table nor its triggers write -- so a disagreement means a fill
// moved units without moving a position, or the reverse. Like
// VerifyReserves, it reports; it never repairs one side to match the other.
func (s *Service) VerifyPositions(ctx context.Context, q db.Querier) ([]UnreconciledPosition, error) {
	rows, err := q.Query(ctx,
		`SELECT account_id, asset_id, position_quantity::text, ledger_balance::text
		   FROM cp_native_positions_unreconciled()`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []UnreconciledPosition
	for rows.Next() {
		var (
			u        UnreconciledPosition
			qty, bal string
		)
		if err := rows.Scan(&u.AccountID, &u.AssetID, &qty, &bal); err != nil {
			return nil, mapError(err)
		}
		var perr error
		if u.PositionQuantity, perr = money.ParseQuantity(qty); perr != nil {
			return nil, errs.Wrap(perr, errs.CodeInternal, "nativemarket: a position quantity is not an integer")
		}
		if u.LedgerBalance, perr = money.ParseQuantity(bal); perr != nil {
			return nil, errs.Wrap(perr, errs.CodeInternal, "nativemarket: a ledger balance is not an integer")
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

// PositionWithMarket is a position together with the market it is marked
// against and the asset facts a portfolio row needs to render.
type PositionWithMarket struct {
	Position Position
	Market   Market
	State    State

	Symbol        string
	Name          string
	AssetDecimals uint8
	// Demo marks a position in an asset a sandbox seeder created.
	Demo bool
}

// PortfolioPositions returns every position an account holds or has held,
// joined to its market in ONE query.
//
// One query rather than a list plus a read per position: a portfolio of twenty
// assets otherwise costs forty round trips, and each figure would then describe
// a different instant. The caller marks them all against one `as of`.
func (s *Service) PortfolioPositions(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]PositionWithMarket, error) {
	rows, err := q.Query(ctx,
		`SELECT `+positionColumns+`,
		        m.id, m.credit_asset_id, m.status, m.activated_at, m.created_at, m.updated_at,
		        m.virtual_credit_reserve::text, m.initial_asset_reserve::text,
		        m.platform_fee_bps, m.creator_fee_bps,
		        st.real_credit_reserve::text, st.asset_reserve::text, st.version,
		        na.symbol, na.name, reg.decimals,
		        (ds.seed_key IS NOT NULL) AS is_demo
		   FROM native_positions p
		   JOIN native_assets na ON na.asset_id = p.asset_id
		   JOIN assets reg ON reg.id = p.asset_id
		   JOIN native_markets m ON m.asset_id = p.asset_id
		   JOIN native_market_state st ON st.market_id = m.id
		   LEFT JOIN demo_seed_rows ds ON ds.kind = 'NATIVE_MARKET' AND ds.ref_id = m.id
		  WHERE p.account_id = $1
		  ORDER BY p.quantity DESC, p.asset_id`, accountID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []PositionWithMarket
	for rows.Next() {
		var (
			row                            PositionWithMarket
			qty, basis, realized, fees     string
			alloc, bought, sold, cin, cout string
			status                         string
			virt, initial, real, assetRes  string
			platBPS, creBPS                int
			decimals                       int16
		)
		if err := rows.Scan(
			&row.Position.AccountID, &row.Position.AssetID, &qty, &basis, &realized, &fees,
			&alloc, &bought, &sold, &cin, &cout, &row.Position.FillCount,
			&row.Position.FirstAcquiredAt, &row.Position.LastTradeAt, &row.Position.UpdatedAt,
			&row.Market.ID, &row.Market.CreditAssetID, &status, &row.Market.ActivatedAt,
			&row.Market.CreatedAt, &row.Market.UpdatedAt,
			&virt, &initial, &platBPS, &creBPS,
			&real, &assetRes, &row.State.Version,
			&row.Symbol, &row.Name, &decimals, &row.Demo,
		); err != nil {
			return nil, mapError(err)
		}
		row.Market.AssetID = row.Position.AssetID
		row.Market.Status = Status(status)
		row.Market.Fees = Fees{PlatformBPS: money.BPS(platBPS), CreatorBPS: money.BPS(creBPS)}
		if decimals < 0 || decimals > 18 {
			return nil, errs.Newf(errs.CodeInternal, "nativemarket: asset decimals %d out of range", decimals)
		}
		row.AssetDecimals = uint8(decimals)
		for dst, src := range map[*money.Quantity]string{
			&row.Position.Quantity: qty, &row.Position.CostBasisCredits: basis,
			&row.Position.RealizedPnLCredits: realized, &row.Position.FeesPaidCredits: fees,
			&row.Position.AllocationUnits: alloc, &row.Position.UnitsBoughtTotal: bought,
			&row.Position.UnitsSoldTotal: sold, &row.Position.CreditsInTotal: cin,
			&row.Position.CreditsOutTotal:          cout,
			&row.Market.Curve.VirtualCreditReserve: virt, &row.Market.Curve.InitialAssetReserve: initial,
			&row.State.RealCreditReserve: real, &row.State.AssetReserve: assetRes,
		} {
			v, perr := money.ParseQuantity(src)
			if perr != nil {
				return nil, errs.Wrap(perr, errs.CodeInternal, "nativemarket: a portfolio field is not an integer")
			}
			*dst = v
		}
		row.Position.UpdatedAt = row.Position.UpdatedAt.UTC()
		row.Market.CreatedAt, row.Market.UpdatedAt = row.Market.CreatedAt.UTC(), row.Market.UpdatedAt.UTC()
		out = append(out, row)
	}
	return out, mapError(rows.Err())
}
