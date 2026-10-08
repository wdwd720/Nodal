package derivatives

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// PositionRisk is one position's contribution, in base units of its
// settlement asset.
type PositionRisk struct {
	PositionID        string         `json:"position_id"`
	InstrumentID      string         `json:"instrument_id"`
	Kind              string         `json:"kind"`
	SettlementAssetID string         `json:"settlement_asset_id"`
	Quantity          money.Quantity `json:"quantity"`
	MarkID            string         `json:"mark_id"`
	Mark              money.Quantity `json:"mark_mantissa"`
	MarkObservedAt    time.Time      `json:"mark_observed_at"`
	Requirement       Requirement    `json:"requirement"`
	// AverageEntryPrice is the exact average entry price, rounded half-even
	// to the price scale (display only).
	AverageEntryPrice money.Quantity `json:"average_entry_price_mantissa"`
	// UnrealizedPnL is Q × V(mark) − cost_value.
	UnrealizedPnL money.Quantity `json:"unrealized_pnl"`
	// EquityAdjustment is what the position adds to equity beyond the
	// ledger balances buying power already counts (see equityAdjustment).
	EquityAdjustment money.Quantity `json:"equity_adjustment"`
}

// RiskContribution is an account's derivative book as risk sees it: gross
// notional, initial and maintenance margin, and unrealized PnL, in USD with
// conservative rounding (requirements and notional up, PnL and equity down).
// The margin subsystem maps it onto its own exposure type.
type RiskContribution struct {
	AccountID         string         `json:"account_id"`
	AsOf              time.Time      `json:"as_of"`
	PolicyVersion     string         `json:"policy_version"`
	PolicyHash        string         `json:"policy_hash"`
	GrossNotional     money.USD      `json:"gross_notional"`
	InitialMargin     money.USD      `json:"initial_margin"`
	MaintenanceMargin money.USD      `json:"maintenance_margin"`
	UnrealizedPnL     money.USD      `json:"unrealized_pnl"`
	EquityAdjustment  money.USD      `json:"equity_adjustment"`
	MaxInitialBPS     money.BPS      `json:"max_initial_bps"`
	Positions         []PositionRisk `json:"positions"`
}

// AccountRisk values an account's open positions at their latest marks. It
// fails closed: a position whose mark is older than its instrument's oracle
// max age (or which an option cannot be margined for, lacking an index) makes
// the account's risk unknowable and returns STALE_MARKET_DATA rather than a
// number that would understate it.
func (s *Service) AccountRisk(ctx context.Context, q db.Querier, account accounts.AccountID) (RiskContribution, error) {
	now := s.clk.Now().UTC()
	book, err := s.accountBook(ctx, q, account.String())
	if err != nil {
		return RiskContribution{}, err
	}
	out := RiskContribution{AccountID: account.String(), AsOf: now, PolicyVersion: s.policy.Version, PolicyHash: s.policy.Hash(),
		Positions: []PositionRisk{}}
	v := s.newValuer(q, now)
	var gross, initial, maintenance, upnl, adj usdSum
	for _, l := range book {
		age := now.Sub(l.mark.ObservedAt)
		if age < 0 || age > l.spec.OracleMaxAge {
			return RiskContribution{}, errs.New(errs.CodeStaleMarketData, "derivatives: a position's mark is stale; its risk is unknown").
				WithField("position_id", l.pos.ID.String()).WithField("mark_observed_at", l.mark.ObservedAt)
		}
		st := l.pos.state()
		req, err := RequirementFor(s.policy, l.spec, st.Quantity, markInputOf(l.mark), now)
		if err != nil {
			return RiskContribution{}, err
		}
		u, err := unrealizedPnL(l.spec, st, l.mark.MarkMantissa)
		if err != nil {
			return RiskContribution{}, err
		}
		a, err := equityAdjustment(l.spec, st, l.mark.MarkMantissa)
		if err != nil {
			return RiskContribution{}, err
		}
		entry, err := AverageEntryPrice(l.spec, l.pos, money.RoundHalfEven)
		if err != nil {
			return RiskContribution{}, err
		}
		for _, x := range []struct {
			sum  *usdSum
			v    money.Quantity
			mode money.RoundingMode
		}{{&gross, req.Notional, money.RoundCeil}, {&initial, req.Initial, money.RoundCeil}, {&maintenance, req.Maintenance, money.RoundCeil},
			{&upnl, u, money.RoundFloor}, {&adj, a, money.RoundFloor}} {
			usd, err := v.value(ctx, l.spec.SettlementAssetID, x.v, x.mode)
			if err != nil {
				return RiskContribution{}, err
			}
			x.sum.add(usd)
		}
		if l.spec.IsLinear() {
			rates, err := s.policy.linearRates(l.spec.UnderlyingRiskClass)
			if err != nil {
				return RiskContribution{}, err
			}
			if rates.InitialBPS > out.MaxInitialBPS {
				out.MaxInitialBPS = rates.InitialBPS
			}
		}
		out.Positions = append(out.Positions, PositionRisk{
			PositionID: l.pos.ID.String(), InstrumentID: l.pos.InstrumentID.String(), Kind: string(l.spec.Kind),
			SettlementAssetID: l.spec.SettlementAssetID.String(), Quantity: l.pos.Quantity, MarkID: l.mark.ID.String(),
			Mark: l.mark.MarkMantissa, MarkObservedAt: l.mark.ObservedAt, Requirement: req, AverageEntryPrice: entry,
			UnrealizedPnL: u, EquityAdjustment: a,
		})
	}
	for _, x := range []*usdSum{&gross, &initial, &maintenance, &upnl, &adj} {
		if x.err != nil {
			return RiskContribution{}, errs.Wrap(x.err, errs.CodeOverflow, "derivatives: account risk overflow")
		}
	}
	out.GrossNotional, out.InitialMargin, out.MaintenanceMargin = gross.v, initial.v, maintenance.v
	out.UnrealizedPnL, out.EquityAdjustment = upnl.v, adj.v
	return out, nil
}

// MarginReader reports an account's derivative margin state to Buying Power
// V2. It implements buyingpower.MarginReader.
type MarginReader struct{ svc *Service }

var _ buyingpower.MarginReader = (*MarginReader)(nil)

// MarginReader returns the service's buying-power margin reader.
func (s *Service) MarginReader() *MarginReader { return &MarginReader{svc: s} }

// MarginState implements buyingpower.MarginReader. An owner without open
// derivative positions (and every HOUSE owner: derivative positions are
// customer accounts') reports nothing. Otherwise the state is ISOLATED margin
// (every position carries its own initial margin in DERIVATIVES_MARGIN, which
// buying power already counts as a holding):
//
//   - UnrealizedPnL is the book's equity adjustment -- what equity holds
//     beyond the ledger balances buying power values: a future's PnL not yet
//     settled by variation margin, and an option's whole mark value (the
//     premium a holder paid sits in OPTION_PREMIUM_PAID, which buying power
//     does not count; a writer's received premium is in its wallet and the
//     option's value is its liability).
//   - InitialMarginUsed and MaintenanceMargin are the book's requirements at
//     current marks.
//   - InitialMarginRateBPS is the highest linear initial rate of the book
//     (the policy's highest when the book holds only options).
//
// A stale mark fails the read (STALE_MARKET_DATA): buying power is not
// computed on a book it cannot value.
func (r *MarginReader) MarginState(ctx context.Context, q db.Querier, ownerType, ownerID string) (buyingpower.V2MarginState, bool, error) {
	if ownerType != "CUSTOMER" {
		return buyingpower.V2MarginState{}, false, nil
	}
	acct, err := accounts.ParseAccountID(ownerID)
	if err != nil {
		return buyingpower.V2MarginState{}, false, errs.New(errs.CodeValidationFailed, "derivatives: owner id must be an account id")
	}
	rc, err := r.svc.AccountRisk(ctx, q, acct)
	if err != nil {
		return buyingpower.V2MarginState{}, false, err
	}
	if len(rc.Positions) == 0 {
		return buyingpower.V2MarginState{}, false, nil
	}
	rate := rc.MaxInitialBPS
	if rate <= 0 {
		rate = r.svc.policy.maxInitialBPS()
	}
	return buyingpower.V2MarginState{
		Mode: "ISOLATED", UnrealizedPnL: rc.EquityAdjustment, InitialMarginUsed: rc.InitialMargin,
		MaintenanceMargin: rc.MaintenanceMargin, InitialMarginRateBPS: rate,
	}, true, nil
}
