package buyingpower

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuation"
)

// MarginReader reports an owner's margin state. The margin and derivatives
// subsystems implement it; without one an owner is a CASH account whose only
// liabilities are what the ledger says it borrowed.
type MarginReader interface {
	MarginState(ctx context.Context, q db.Querier, ownerType, ownerID string) (V2MarginState, bool, error)
}

// ExtraReservationReader reports reservations the capital tables cannot
// classify (strategy reservations, pending transfers held outside
// asset_reservation_totals). Optional.
type ExtraReservationReader interface {
	Reservations(ctx context.Context, q db.Querier, ownerType, ownerID string) (V2Reservations, error)
}

// V2LoaderConfig wires a V2Loader.
type V2LoaderConfig struct {
	Clock clock.Clock
	// Prices supplies marks; valuation.PriceStore satisfies it.
	Prices valuation.PriceSource
	// QuoteAssetID is the sandbox dollar every mark is quoted in.
	QuoteAssetID assets.AssetID
	// MaxPriceAge bounds how old a mark may be.
	MaxPriceAge time.Duration
	// BridgeInFlightFactorBPS is how much of a bridge leg in flight counts as
	// collateral. Zero by default: in-flight capital is not final.
	BridgeInFlightFactorBPS money.BPS
	Policy                  V2Policy
	Margin                  MarginReader
	Extra                   ExtraReservationReader
}

// V2Loader builds V2Input for one owner from the ledger. It reads only
// SIMULATED-domain accounts: V2 never values real capital.
type V2Loader struct {
	cfg    V2LoaderConfig
	assets *assets.Repository
}

// NewV2Loader validates the wiring.
func NewV2Loader(cfg V2LoaderConfig) (*V2Loader, error) {
	if cfg.Clock == nil || cfg.Prices == nil || cfg.QuoteAssetID.IsZero() || cfg.MaxPriceAge <= 0 {
		return nil, errs.New(errs.CodeInternal, "buyingpower: V2 loader needs a clock, a price source, a quote asset and a positive max price age")
	}
	if cfg.BridgeInFlightFactorBPS < 0 || cfg.BridgeInFlightFactorBPS > money.OneHundredPercent {
		return nil, errs.New(errs.CodeInternal, "buyingpower: bridge in-flight factor out of range")
	}
	if cfg.Policy.Version == "" {
		cfg.Policy = DefaultV2Policy()
	}
	return &V2Loader{cfg: cfg, assets: assets.NewRepository()}, nil
}

// collateralFactor is the default haircut by risk class. A subsystem with a
// finer haircut engine (margin) reports its requirements through
// MarginReader; this is the conservative floor for anything held.
func collateralFactor(a assets.Asset) (money.BPS, string) {
	switch a.RiskClass {
	case assets.RiskSettlement:
		return money.OneHundredPercent, ""
	case assets.RiskMajor:
		return 8_000, "MAJOR risk class haircut 20%"
	case assets.RiskStandard:
		return 6_000, "STANDARD risk class haircut 40%"
	case assets.RiskSpeculative:
		return 3_000, "SPECULATIVE risk class haircut 70%"
	}
	return 0, "UNSUPPORTED risk class: not collateral"
}

var v2Components = map[string]V2Component{
	"WALLET":               ComponentWallet,
	"HOUSE_INVENTORY":      ComponentWallet,
	"MARGIN_COLLATERAL":    ComponentMarginCollateral,
	"DERIVATIVES_MARGIN":   ComponentDerivativesMargin,
	"UNSETTLED_RECEIVABLE": ComponentUnsettledReceivable,
	"BRIDGE_IN_FLIGHT":     ComponentBridgeInFlight,
}

// Load reads one owner's holdings, liabilities and reservations. ownerType is
// CUSTOMER (owner id = account id) or HOUSE (owner id = house book id).
func (l *V2Loader) Load(ctx context.Context, q db.Querier, ownerType, ownerID string) (V2Input, error) {
	if ownerType != "CUSTOMER" && ownerType != "HOUSE" {
		return V2Input{}, errs.Newf(errs.CodeValidationFailed, "buying power v2: owner type %q", ownerType)
	}
	now := l.cfg.Clock.Now().UTC()
	in := V2Input{OwnerType: ownerType, OwnerID: ownerID, Policy: l.cfg.Policy, AsOf: now}

	if ownerType == "CUSTOMER" {
		var status string
		if err := q.QueryRow(ctx, `SELECT status FROM accounts WHERE id = $1`, ownerID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return V2Input{}, errs.New(errs.CodeNotFound, "account not found")
			}
			return V2Input{}, fmt.Errorf("buyingpower v2: account: %w", err)
		}
		if status != "ACTIVE" {
			in.Blocked = true
			in.BlockReasons = append(in.BlockReasons, "ACCOUNT_"+status)
		}
	} else {
		var status string
		if err := q.QueryRow(ctx, `SELECT status FROM house_accounts WHERE id = $1`, ownerID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return V2Input{}, errs.New(errs.CodeNotFound, "house book not found")
			}
			return V2Input{}, fmt.Errorf("buyingpower v2: house book: %w", err)
		}
		if status != "ACTIVE" {
			in.Blocked = true
			in.BlockReasons = append(in.BlockReasons, "HOUSE_"+status)
		}
	}
	var killed bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kill_switches WHERE active AND
		(kind = 'GLOBAL_NEW_RISK_KILL' OR (kind = 'ACCOUNT_FREEZE' AND scope_id IN ('*', $1))))`, ownerID).Scan(&killed); err != nil {
		return V2Input{}, fmt.Errorf("buyingpower v2: kill switches: %w", err)
	}
	if killed {
		in.Blocked = true
		in.BlockReasons = append(in.BlockReasons, "KILL_SWITCH")
	}

	rows, err := q.Query(ctx, `SELECT la.code, la.asset_id, lb.balance::text
		FROM ledger_accounts la JOIN ledger_balances lb ON lb.ledger_account_id = la.id
		WHERE la.owner_type = $1 AND la.owner_id = $2 AND la.value_domain = 'SIMULATED'
		  AND la.code IN ('WALLET','HOUSE_INVENTORY','MARGIN_COLLATERAL','DERIVATIVES_MARGIN',
		                  'UNSETTLED_RECEIVABLE','BRIDGE_IN_FLIGHT','MARGIN_LOAN')
		  AND lb.balance <> 0
		ORDER BY la.code, la.asset_id`, ownerType, ownerID)
	if err != nil {
		return V2Input{}, fmt.Errorf("buyingpower v2: balances: %w", err)
	}
	type bal struct {
		code  string
		asset assets.AssetID
		qty   money.Quantity
	}
	var bals []bal
	for rows.Next() {
		var b bal
		var s string
		if err := rows.Scan(&b.code, &b.asset, &s); err != nil {
			rows.Close()
			return V2Input{}, fmt.Errorf("buyingpower v2: scan balance: %w", err)
		}
		if b.qty, err = money.ScanQuantity(s); err != nil {
			rows.Close()
			return V2Input{}, err
		}
		bals = append(bals, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return V2Input{}, err
	}

	quote, err := l.assets.Get(ctx, q, l.cfg.QuoteAssetID)
	if err != nil {
		return V2Input{}, errs.Wrap(err, errs.CodeInternal, "buyingpower v2: quote asset not registered")
	}
	var borrow money.USD
	for _, b := range bals {
		a, err := l.assets.Get(ctx, q, b.asset)
		if err != nil {
			return V2Input{}, fmt.Errorf("buyingpower v2: asset %s: %w", b.asset, err)
		}
		marked, priced, err := l.mark(ctx, q, a, quote, b.qty, now)
		if err != nil {
			return V2Input{}, err
		}
		if b.code == "MARGIN_LOAN" {
			if !priced {
				// A liability we cannot value makes equity unknowable: fail
				// closed rather than understate debt.
				in.Blocked = true
				in.BlockReasons = append(in.BlockReasons, "LIABILITY_UNPRICED:"+a.Symbol)
				continue
			}
			if borrow, err = borrow.Add(marked); err != nil {
				return V2Input{}, errs.Wrap(err, errs.CodeOverflow, "buyingpower v2: borrow overflow")
			}
			continue
		}
		comp := v2Components[b.code]
		h := V2Holding{Component: comp, AssetID: a.ID.String(), Symbol: a.Symbol, Quantity: b.qty, MarkedUSD: marked}
		switch {
		case !priced:
			h.FactorBPS, h.Reason = 0, "STALE_PRICE: no mark within the maximum age"
		case comp == ComponentBridgeInFlight:
			h.FactorBPS, h.Reason = l.cfg.BridgeInFlightFactorBPS, "BRIDGE_IN_FLIGHT: not final"
		case comp == ComponentUnsettledReceivable:
			h.FactorBPS, h.Reason = 0, "UNSETTLED: not final"
		default:
			h.FactorBPS, h.Reason = collateralFactor(a)
		}
		h.SpendableCash = comp == ComponentWallet && a.ID == quote.ID && priced
		in.Holdings = append(in.Holdings, h)
	}

	if l.cfg.Margin != nil {
		ms, ok, err := l.cfg.Margin.MarginState(ctx, q, ownerType, ownerID)
		if err != nil {
			return V2Input{}, fmt.Errorf("buyingpower v2: margin state: %w", err)
		}
		if ok {
			in.Margin = ms
		}
	}
	if in.Margin.Mode == "" {
		in.Margin.Mode = "CASH"
	}
	// The ledger is the authority on what was borrowed; a margin reader that
	// reports less than the ledger holds is overridden, never trusted.
	if borrow.Cmp(in.Margin.BorrowLiabilities) > 0 {
		in.Margin.BorrowLiabilities = borrow
	}

	if ownerType == "CUSTOMER" {
		if in.Reservations.OpenOrders, err = l.valued(ctx, q, quote, now,
			`SELECT asset_id, reserved::text FROM asset_reservation_totals WHERE account_id = $1 AND reserved > 0 ORDER BY asset_id`, ownerID); err != nil {
			return V2Input{}, err
		}
		if in.Reservations.PendingWithdrawals, err = l.valued(ctx, q, quote, now,
			`SELECT asset_id, sum(quantity)::text FROM withdrawal_holds WHERE account_id = $1 AND released_at IS NULL
			 AND (expires_at IS NULL OR expires_at > now()) GROUP BY asset_id ORDER BY asset_id`, ownerID); err != nil {
			return V2Input{}, err
		}
	}
	if l.cfg.Extra != nil {
		extra, err := l.cfg.Extra.Reservations(ctx, q, ownerType, ownerID)
		if err != nil {
			return V2Input{}, fmt.Errorf("buyingpower v2: extra reservations: %w", err)
		}
		add := func(a, b money.USD) money.USD {
			s, aerr := a.Add(b)
			if aerr != nil && err == nil {
				err = aerr
			}
			return s
		}
		in.Reservations.OpenOrders = add(in.Reservations.OpenOrders, extra.OpenOrders)
		in.Reservations.PendingTransfers = add(in.Reservations.PendingTransfers, extra.PendingTransfers)
		in.Reservations.PendingWithdrawals = add(in.Reservations.PendingWithdrawals, extra.PendingWithdrawals)
		in.Reservations.Strategies = add(in.Reservations.Strategies, extra.Strategies)
		if err != nil {
			return V2Input{}, errs.Wrap(err, errs.CodeOverflow, "buyingpower v2: reservation overflow")
		}
	}
	sort.Strings(in.BlockReasons)
	return in, nil
}

// Compute is Load followed by EvaluateV2.
func (l *V2Loader) Compute(ctx context.Context, q db.Querier, ownerType, ownerID string) (V2, error) {
	in, err := l.Load(ctx, q, ownerType, ownerID)
	if err != nil {
		return V2{}, err
	}
	return EvaluateV2(in)
}

// mark values qty of a in USD minor units, rounding down. The quote asset is
// valued at face; anything else needs a fresh price. priced is false when no
// fresh mark exists.
func (l *V2Loader) mark(ctx context.Context, q db.Querier, a, quote assets.Asset, qty money.Quantity, now time.Time) (money.USD, bool, error) {
	if a.ID == quote.ID {
		u, err := money.QuoteQuantityToUSD(qty, quote.Decimals, money.RoundDown)
		if err != nil {
			return money.USD{}, false, errs.Wrap(err, errs.CodeOverflow, "buyingpower v2: face value")
		}
		return u, true, nil
	}
	p, err := l.cfg.Prices.Latest(ctx, q, a.ID, quote.ID, l.cfg.MaxPriceAge, now)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeStaleMarketData {
			return money.USD{}, false, nil
		}
		return money.USD{}, false, err
	}
	notional, err := money.Notional(qty, a.Decimals, p, quote.Decimals, money.RoundDown)
	if err != nil {
		return money.USD{}, false, errs.Wrap(err, errs.CodeOverflow, "buyingpower v2: notional")
	}
	u, err := money.QuoteQuantityToUSD(notional, quote.Decimals, money.RoundDown)
	if err != nil {
		return money.USD{}, false, errs.Wrap(err, errs.CodeOverflow, "buyingpower v2: notional to usd")
	}
	return u, true, nil
}

// valued sums (asset, quantity) rows at their marks. An unpriced reserved
// asset is valued at zero for display, which can only make the reservation
// look smaller; reservation itself is enforced in quantities by capital.
func (l *V2Loader) valued(ctx context.Context, q db.Querier, quote assets.Asset, now time.Time, sql string, args ...any) (money.USD, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return money.USD{}, fmt.Errorf("buyingpower v2: reservations: %w", err)
	}
	type rq struct {
		asset assets.AssetID
		qty   money.Quantity
	}
	var items []rq
	for rows.Next() {
		var r rq
		var s string
		if err := rows.Scan(&r.asset, &s); err != nil {
			rows.Close()
			return money.USD{}, err
		}
		if r.qty, err = money.ScanQuantity(s); err != nil {
			rows.Close()
			return money.USD{}, err
		}
		items = append(items, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return money.USD{}, err
	}
	var total money.USD
	for _, r := range items {
		a, err := l.assets.Get(ctx, q, r.asset)
		if err != nil {
			return money.USD{}, err
		}
		if a.ValueDomain != "SIMULATED" {
			continue
		}
		m, _, err := l.mark(ctx, q, a, quote, r.qty, now)
		if err != nil {
			return money.USD{}, err
		}
		if total, err = total.Add(m); err != nil {
			return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "buyingpower v2: reservation overflow")
		}
	}
	return total, nil
}
