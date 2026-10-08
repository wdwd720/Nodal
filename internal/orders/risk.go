package orders

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// riskInput builds the Risk Kernel V2 input of an order. Every section is
// read inside the caller's transaction:
//
//   - Identity and Account: the account's verification level and status;
//   - Instrument: the instrument's status, whether its calendar is open and
//     whether it has expired;
//   - Balance: what the order needs against what the account has available,
//     in USD at the book's USD-pegged quote asset (a buy needs its notional
//     at the limit price plus the most its fees can be; a sell needs its
//     base quantity, valued at the limit price). For a replace, ownReserved
//     is what the order already holds reserved, which the replace frees for
//     itself;
//   - Oracle, for a risk-increasing order: the freshest mark of base in
//     quote, or "unavailable" when there is none; the kernel decides whether
//     it is fresh enough, and a stale or missing mark blocks new risk only;
//   - Authority, Strategy and Sandbox when the caller resolved them;
//   - the active kill switches.
func (s *Service) riskInput(ctx context.Context, q db.Querier, c *bookConfig, in econ.Intent, o orderRow, rc *RiskContext,
	ticks, lots int64, riskIncreasing bool, ownReserved money.Quantity) (risk.AdvancedInput, error) {
	now := s.clk.Now().UTC()
	t := c.Terms
	account := *o.AccountID
	level, err := s.verify.Level(ctx, q, account)
	if err != nil {
		return risk.AdvancedInput{}, err
	}
	acct, err := accounts.NewRepository().Get(ctx, q, account)
	if err != nil {
		return risk.AdvancedInput{}, err
	}
	inst, err := s.instr.Get(ctx, q, c.InstrumentID)
	if err != nil {
		return risk.AdvancedInput{}, err
	}
	status := string(inst.Status)
	if !inst.IsActiveAt(now) {
		status = "OUTSIDE_ACTIVE_WINDOW"
	}
	notional := t.Notional(ticks, lots)
	notionalUSD, err := money.QuoteQuantityToUSD(notional, t.QuoteDecimals, money.RoundUp)
	if err != nil {
		return risk.AdvancedInput{}, errs.Wrap(err, errs.CodeOverflow, "orders: order notional in USD")
	}
	balance, err := s.balanceSection(ctx, q, c, o, ticks, lots, notional, ownReserved, now)
	if err != nil {
		return risk.AdvancedInput{}, err
	}
	input := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{
			ID: in.ID.String(), Kind: string(in.Kind), AccountID: in.AccountID.String(), ActorType: string(in.ActorType),
			AgentID: in.AgentID, StrategyRef: in.StrategyRef, Capability: string(in.Capability), Mode: string(in.Mode),
			InstrumentID: c.InstrumentID.String(), InstrumentKind: string(t.Kind), NotionalUSD: notionalUSD,
			RiskIncreasing: riskIncreasing, SandboxOriginated: rc != nil && rc.Sandbox != nil,
		},
		Identity: &risk.IdentitySnapshot{Verified: level.AtLeast(valuedomain.VerificationNodalIdentity)},
		Account:  &risk.AdvancedAccountSnapshot{Status: string(acct.Status)},
		Instrument: &risk.InstrumentSnapshot{
			Status: status, SessionOpen: s.sessions.IsOpen(t.TradingCalendar, now),
			Expired: t.Expiry != nil && !now.Before(*t.Expiry),
		},
		Balance: balance,
		Now:     now,
	}
	if riskIncreasing {
		if input.Oracle, err = s.markSection(ctx, q, t, now); err != nil {
			return risk.AdvancedInput{}, err
		}
	}
	if rc != nil {
		input.Authority, input.Strategy, input.Sandbox = rc.Authority, rc.Strategy, rc.Sandbox
	}
	if input.KillSwitches, err = s.killSwitches(ctx, q); err != nil {
		return risk.AdvancedInput{}, err
	}
	return input, nil
}

func (s *Service) balanceSection(ctx context.Context, q db.Querier, c *bookConfig, o orderRow, ticks, lots int64, notional,
	ownReserved money.Quantity, now time.Time) (*risk.BalanceSnapshot, error) {
	t := c.Terms
	asset := t.QuoteAsset
	if o.Side == SideSell {
		asset = t.BaseAsset
	}
	avail, err := s.capital.Availability(ctx, q, o.OwnerID, asset, now)
	if err != nil {
		return nil, err
	}
	available := avail.Available.Add(ownReserved)
	var availableQuote, requiredQuote money.Quantity
	switch o.Side {
	case SideBuy:
		bound, err := FeeBound(c.MakerSchedule, c.TakerSchedule, notional, o.StrategyOriginated)
		if err != nil {
			return nil, err
		}
		availableQuote, requiredQuote = available, notional.Add(bound)
	default:
		price := t.TicksToPrice(ticks, "order-limit", now)
		availableQuote, err = money.Notional(available, t.BaseDecimals, price, t.QuoteDecimals, money.RoundDown)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "orders: value holdings")
		}
		requiredQuote = notional
		if o.ReduceOnly {
			// A reduce-only sell is capped at the holdings by construction;
			// the reservation stage refuses one with nothing to reduce.
			requiredQuote = notional.Min(availableQuote)
		}
	}
	availableUSD, err := money.QuoteQuantityToUSD(availableQuote, t.QuoteDecimals, money.RoundDown)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeOverflow, "orders: available balance in USD")
	}
	requiredUSD, err := money.QuoteQuantityToUSD(requiredQuote, t.QuoteDecimals, money.RoundUp)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeOverflow, "orders: required balance in USD")
	}
	return &risk.BalanceSnapshot{Available: availableUSD, Required: requiredUSD}, nil
}

// markSection reports the freshest mark of the book's base asset in its quote
// asset within the lookback window, or an unavailable oracle when none exists.
func (s *Service) markSection(ctx context.Context, q db.Querier, t BookTerms, now time.Time) (*risk.OracleSnapshot, error) {
	return oracleSection(ctx, s.prices, q, t.BaseAsset, t.QuoteAsset, s.lookback, now)
}

func oracleSection(ctx context.Context, prices valuation.PriceSource, q db.Querier, base, quote assets.AssetID, lookback time.Duration, now time.Time) (*risk.OracleSnapshot, error) {
	p, err := prices.Latest(ctx, q, base, quote, lookback, now)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeStaleMarketData {
			return &risk.OracleSnapshot{Available: false}, nil
		}
		return nil, err
	}
	return &risk.OracleSnapshot{Available: true, ObservedAt: p.At.UTC()}, nil
}

// killSwitches returns the active kill switches as the kernel reads them.
func (s *Service) killSwitches(ctx context.Context, q db.Querier) ([]risk.KillSwitch, error) {
	return activeKillSwitches(ctx, s.kills, q)
}

func activeKillSwitches(ctx context.Context, kills econ.KillSwitchReader, q db.Querier) ([]risk.KillSwitch, error) {
	snap, err := kills.Snapshot(ctx, q)
	if err != nil {
		return nil, err
	}
	out := []risk.KillSwitch{}
	for _, sw := range snap.Switches {
		if sw.Active {
			out = append(out, risk.KillSwitch{Kind: string(sw.Kind), Scope: sw.ScopeID})
		}
	}
	return out, nil
}
