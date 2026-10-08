package derivatives

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
)

// CircuitClearingUnreconciled is the circuit breaker raised while the
// clearing house's books disagree with the cash-flow register: no new
// derivative risk until reconciliation resolves it.
const CircuitClearingUnreconciled = "CLEARING_UNRECONCILED"

// ClearingScopeType is the reconciliation scope type of a clearing record;
// its scope id is "<clearing house id>:<asset id>".
const ClearingScopeType = "clearing_house_asset"

func clearingScopeID(ch econ.FacilityID, asset assets.AssetID) string {
	return ch.String() + ":" + asset.String()
}

// usdValuer values quantities of settlement assets in USD for the duration of
// one operation, reading each asset and each price once. The configured USD
// asset is valued at face; any other needs a price fresh within the risk
// policy's oracle age (STALE_MARKET_DATA otherwise).
type usdValuer struct {
	s      *Service
	q      db.Querier
	now    time.Time
	assets map[assets.AssetID]assets.Asset
	prices map[assets.AssetID]money.Price
}

func (s *Service) newValuer(q db.Querier, now time.Time) *usdValuer {
	return &usdValuer{s: s, q: q, now: now, assets: map[assets.AssetID]assets.Asset{}, prices: map[assets.AssetID]money.Price{}}
}

func (v *usdValuer) asset(ctx context.Context, id assets.AssetID) (assets.Asset, error) {
	if a, ok := v.assets[id]; ok {
		return a, nil
	}
	a, err := assets.NewRepository().Get(ctx, v.q, id)
	if err != nil {
		return assets.Asset{}, err
	}
	v.assets[id] = a
	return a, nil
}

// value values qty of asset in USD, rounding once in mode.
func (v *usdValuer) value(ctx context.Context, asset assets.AssetID, qty money.Quantity, mode money.RoundingMode) (money.USD, error) {
	a, err := v.asset(ctx, asset)
	if err != nil {
		return money.USD{}, err
	}
	if asset == v.s.usdAsset {
		u, err := money.QuoteQuantityToUSD(qty, a.Decimals, mode)
		if err != nil {
			return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "derivatives: value in USD")
		}
		return u, nil
	}
	usd, err := v.asset(ctx, v.s.usdAsset)
	if err != nil {
		return money.USD{}, err
	}
	p, ok := v.prices[asset]
	if !ok {
		if p, err = v.s.prices.Latest(ctx, v.q, asset, v.s.usdAsset, time.Duration(v.s.riskPolicy.MaxOracleAgeMS)*time.Millisecond, v.now); err != nil {
			return money.USD{}, err
		}
		v.prices[asset] = p
	}
	n, err := money.Notional(qty, a.Decimals, p, usd.Decimals, mode)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "derivatives: value in USD")
	}
	u, err := money.QuoteQuantityToUSD(n, usd.Decimals, mode)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "derivatives: value in USD")
	}
	return u, nil
}

// markUsable reports whether a mark may back new risk: fresh within the
// instrument's own oracle max age and carrying the index the divergence
// check (and an option's margin) needs.
func markUsable(s Spec, m Mark, now time.Time) bool {
	if !m.HasIndex {
		return false
	}
	age := now.Sub(m.ObservedAt)
	return age >= 0 && age <= s.OracleMaxAge
}

// divergenceOf is the mark/index divergence of a mark: a linear contract's
// (mark − index) / index, an option's distance outside its model-free bounds.
func divergenceOf(s Spec, m Mark) (money.BPS, error) {
	if !m.HasIndex {
		return 0, errs.New(errs.CodeStaleMarketData, "derivatives: the mark has no index")
	}
	if s.IsOption() {
		return OptionDivergenceBPS(s, m.MarkMantissa, m.IndexMantissa, m.IndexScale)
	}
	return DivergenceBPS(sampleOf(m))
}

// tradeRisk is what the risk evaluation of one intent needs beyond the
// request.
type tradeRisk struct {
	spec           Spec
	mark           Mark
	hasMark        bool
	riskIncreasing bool
	notional       money.Quantity
	margin         *risk.MarginSnapshot
	// unvaluable is set when the book could not be valued in USD (a stale
	// price of a settlement asset): new risk is then refused as an
	// unavailable oracle; reducing risk is not affected.
	unvaluable bool
}

// evaluate builds the Risk V2 input -- account, identity, instrument, oracle
// and margin sections, kill switches and the clearing circuit breaker --
// evaluates it, and records the decision in tx. The account's status and its
// owner's identity verification are read from the database (accounts,
// compliance_profiles), never taken from the request.
func (s *Service) evaluate(ctx context.Context, tx pgx.Tx, ch econ.Facility, in econ.Intent, r IntentRequest, tr tradeRisk, now time.Time) (risk.AdvancedDecision, risk.DecisionID, error) {
	var status string
	var verified bool
	if err := tx.QueryRow(ctx, `SELECT a.status,
			coalesce(c.identity_state = 'VERIFIED' AND (c.expires_at IS NULL OR c.expires_at > $2), false)
		FROM accounts a LEFT JOIN compliance_profiles c ON c.user_id = a.owner_user_id WHERE a.id = $1`,
		r.AccountID, now).Scan(&status, &verified); err != nil {
		return risk.AdvancedDecision{}, risk.DecisionID{}, dbErr("read account status and identity", err)
	}
	notionalUSD, err := s.newValuer(tx, now).value(ctx, tr.spec.SettlementAssetID, tr.notional, money.RoundCeil)
	switch {
	case err == nil:
	case errs.CodeOf(err) == errs.CodeStaleMarketData:
		// A settlement asset the policy cannot price now: new risk is
		// refused below as an unavailable oracle; reducing risk proceeds.
		notionalUSD, tr.unvaluable = money.USD{}, true
	default:
		return risk.AdvancedDecision{}, risk.DecisionID{}, err
	}
	oracle := &risk.OracleSnapshot{}
	if tr.hasMark {
		oracle.ObservedAt = tr.mark.ObservedAt
		oracle.Available = markUsable(tr.spec, tr.mark, now) && !tr.unvaluable
		if div, err := divergenceOf(tr.spec, tr.mark); err == nil {
			oracle.DivergenceBPS = div
		} else if errs.CodeOf(err) != errs.CodeStaleMarketData {
			return risk.AdvancedDecision{}, risk.DecisionID{}, err
		}
	}
	snap, err := s.kills.Snapshot(ctx, tx)
	if err != nil {
		return risk.AdvancedDecision{}, risk.DecisionID{}, err
	}
	kills := make([]risk.KillSwitch, 0, len(snap.Switches))
	for _, sw := range snap.Switches {
		kills = append(kills, risk.KillSwitch{Kind: string(sw.Kind), Scope: sw.ScopeID})
	}
	var breakers []string
	if _, open, err := s.recon.FindUnresolvedByScope(ctx, tx, reconciliation.KindClearing, ClearingScopeType,
		clearingScopeID(ch.ID, tr.spec.SettlementAssetID)); err != nil {
		return risk.AdvancedDecision{}, risk.DecisionID{}, err
	} else if open {
		breakers = append(breakers, CircuitClearingUnreconciled)
	}
	input := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{
			ID: in.ID.String(), Kind: string(in.Kind), AccountID: in.AccountID.String(), ActorType: string(in.ActorType),
			AgentID: in.AgentID, StrategyRef: in.StrategyRef, Capability: string(gates.Derivatives), Mode: string(in.Mode),
			InstrumentID: tr.spec.InstrumentID.String(), InstrumentKind: string(tr.spec.Kind), NotionalUSD: notionalUSD,
			RiskIncreasing: tr.riskIncreasing,
		},
		Identity: &risk.IdentitySnapshot{Verified: verified},
		Account:  &risk.AdvancedAccountSnapshot{Status: status},
		Instrument: &risk.InstrumentSnapshot{
			Status: string(tr.spec.InstrumentStatus), SessionOpen: tr.spec.TradingCalendar == Calendar24x7,
			Expired: tr.spec.ExpiredAt(now),
		},
		Oracle:          oracle,
		Margin:          tr.margin,
		KillSwitches:    kills,
		CircuitBreakers: breakers,
		Now:             now,
		// Authority is deliberately absent: an agent's delegated authority is
		// evaluated by the subsystem that issued it, and the kernel requires
		// the section for an agent intent, so an agent intent reaching this
		// service unaccompanied is refused (fail closed).
	}
	d := risk.EvaluateAdvanced(s.riskPolicy, input)
	did, err := s.riskStore.RecordAdvancedDecision(ctx, tx, input, d, r.CorrelationID)
	if err != nil {
		return risk.AdvancedDecision{}, risk.DecisionID{}, err
	}
	return d, did, nil
}

// bookLine is one position of an account's book at its latest mark.
type bookLine struct {
	pos  Position
	spec Spec
	mark Mark
}

// accountBook reads an account's open positions with their specs and latest
// marks. A position whose instrument has never been marked makes the book
// unvaluable: that fails closed.
func (s *Service) accountBook(ctx context.Context, q db.Querier, account string) ([]bookLine, error) {
	positions, err := openPositionsForAccount(ctx, q, account)
	if err != nil {
		return nil, err
	}
	out := make([]bookLine, 0, len(positions))
	for _, p := range positions {
		spec, err := loadSpec(ctx, q, s.instruments, p.InstrumentID)
		if err != nil {
			return nil, err
		}
		m, ok, err := LatestMark(ctx, q, p.InstrumentID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errs.New(errs.CodeStaleMarketData, "derivatives: an open position's instrument has no mark").
				WithField("position_id", p.ID.String())
		}
		out = append(out, bookLine{pos: p, spec: spec, mark: m})
	}
	return out, nil
}

// usdSum accumulates USD with overflow detection.
type usdSum struct {
	v   money.USD
	err error
}

func (u *usdSum) add(x money.USD) {
	if u.err != nil {
		return
	}
	u.v, u.err = u.v.Add(x)
}

// marginSnapshot is the account's margin position after a trade of d
// contracts at price on spec: equity (cash and derivatives margin in every
// settlement asset of the book plus the positions' equity adjustments) against
// the initial and maintenance requirements of the book with the trade, and the
// leverage that leaves.
func (s *Service) marginSnapshot(ctx context.Context, q db.Querier, account accounts.AccountID, spec Spec, d, price money.Quantity, tradeMark Mark, now time.Time) (*risk.MarginSnapshot, error) {
	book, err := s.accountBook(ctx, q, account.String())
	if err != nil {
		return nil, err
	}
	v := s.newValuer(q, now)
	var equity, initial, maintenance, gross usdSum
	settlementAssets := []assets.AssetID{spec.SettlementAssetID}
	seen := map[assets.AssetID]bool{spec.SettlementAssetID: true}
	traded := false
	for _, l := range book {
		if !seen[l.spec.SettlementAssetID] {
			seen[l.spec.SettlementAssetID] = true
			settlementAssets = append(settlementAssets, l.spec.SettlementAssetID)
		}
		st, mark := l.pos.state(), l.mark
		var cashDelta money.Quantity
		if l.pos.InstrumentID == spec.InstrumentID {
			traded = true
			mark = tradeMark
			eff, err := applyFill(l.spec, st, d, price)
			if err != nil {
				return nil, err
			}
			cashDelta, st = eff.Cash, eff.After
		}
		if err := s.addLine(ctx, v, l.spec, st, markInputOf(mark), cashDelta, &equity, &initial, &maintenance, &gross); err != nil {
			return nil, err
		}
	}
	if !traded {
		eff, err := applyFill(spec, positionState{}, d, price)
		if err != nil {
			return nil, err
		}
		if err := s.addLine(ctx, v, spec, eff.After, markInputOf(tradeMark), eff.Cash, &equity, &initial, &maintenance, &gross); err != nil {
			return nil, err
		}
	}
	for _, a := range settlementAssets {
		for _, code := range []ledger.Code{ledger.CodeWallet, ledger.CodeDerivativesMargin} {
			bal, err := s.ledger.Balance(ctx, q, ledger.CustomerAccount(account, code, a))
			if err != nil {
				return nil, err
			}
			u, err := v.value(ctx, a, bal, money.RoundFloor)
			if err != nil {
				return nil, err
			}
			equity.add(u)
		}
	}
	for _, x := range []*usdSum{&equity, &initial, &maintenance, &gross} {
		if x.err != nil {
			return nil, errs.Wrap(x.err, errs.CodeOverflow, "derivatives: margin snapshot overflow")
		}
	}
	return &risk.MarginSnapshot{
		EquityAfter: equity.v, InitialRequiredAfter: initial.v, MaintenanceRequiredAfter: maintenance.v,
		LeverageAfterBPS: leverageBPS(gross.v, equity.v),
	}, nil
}

// addLine adds one position's equity adjustment (plus the cash a trade moves)
// and requirements to the running sums.
func (s *Service) addLine(ctx context.Context, v *usdValuer, spec Spec, st positionState, m MarkInput, cashDelta money.Quantity,
	equity, initial, maintenance, gross *usdSum) error {
	adj, err := equityAdjustment(spec, st, m.Mark)
	if err != nil {
		return err
	}
	adj = adj.Add(cashDelta)
	req, err := RequirementFor(s.policy, spec, st.Quantity, m, v.now)
	if err != nil {
		return err
	}
	for _, x := range []struct {
		sum  *usdSum
		v    money.Quantity
		mode money.RoundingMode
	}{{equity, adj, money.RoundFloor}, {initial, req.Initial, money.RoundCeil}, {maintenance, req.Maintenance, money.RoundCeil}, {gross, req.Notional, money.RoundCeil}} {
		u, err := v.value(ctx, spec.SettlementAssetID, x.v, x.mode)
		if err != nil {
			return err
		}
		x.sum.add(u)
	}
	return nil
}

// maxLeverageBPS stands for unbounded leverage (equity at or below zero).
const maxLeverageBPS money.BPS = 1_000_000_000

// leverageBPS is gross / equity in basis points, rounded up.
func leverageBPS(gross, equity money.USD) money.BPS {
	if gross.IsZero() {
		return 0
	}
	if !equity.IsPositive() {
		return maxLeverageBPS
	}
	v, err := money.QuantityFromInt64(gross.Minor()).MulDiv(money.QuantityFromInt64(int64(money.OneHundredPercent)),
		money.QuantityFromInt64(equity.Minor()), money.RoundUp)
	if err != nil || v.Cmp(money.QuantityFromInt64(int64(maxLeverageBPS))) > 0 {
		return maxLeverageBPS
	}
	b, _ := v.Int64()
	return money.BPS(b)
}
