package margin

import (
	"sort"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// PlanInput is what the planner needs for one step of a liquidation case.
type PlanInput struct {
	State AccountState `json:"state"`
	// StepsTaken is how many TRADING steps (sales) the case has executed in
	// its current budget window. Applying collateral and sweeping the wallet
	// are terminal accounting and do not count: the budget bounds trading,
	// never the bookkeeping that ends a case.
	StepsTaken int `json:"steps_taken"`
	// VenueAvailable is false when the paper venue cannot execute.
	VenueAvailable bool `json:"venue_available"`
	// SlippageAtCap prices every sale at the liquidation slippage cap (the
	// extreme-spread condition); otherwise each asset takes its liquidity
	// class's slippage, capped.
	SlippageAtCap bool `json:"slippage_at_cap"`
	// FreeNumeraire is the customer's free numeraire cash outside the margin
	// account (the wallet balance neither reserved nor held), which repays
	// the loans before the account can be declared bankrupt. Zero when the
	// customer account cannot reserve it.
	FreeNumeraire money.Quantity `json:"free_numeraire"`
}

// LiquidationOrder is one reduce-only instruction: sell part or all of a
// holding, or apply numeraire collateral to the loans. Values are numeraire
// base units.
type LiquidationOrder struct {
	Action   StepAction     `json:"action"`
	AssetID  string         `json:"asset_id"`
	Quantity money.Quantity `json:"quantity"`
	// Mark is the price the sale is planned against; ExecutionPrice is
	// mark × (1 − SlippageBPS), with SlippageBPS never above the policy cap.
	Mark           *money.Price `json:"mark,omitempty"`
	ExecutionPrice *money.Price `json:"execution_price,omitempty"`
	SlippageBPS    money.BPS    `json:"slippage_bps"`
	// Proceeds is quantity × execution price, rounded down; Fee is
	// ceil(proceeds × fee BPS) and is platform fee revenue; Repay is what
	// repays the loans; Excess stays in the account as numeraire collateral.
	// Proceeds = Fee + Repay + Excess. A sweep's proceeds are the swept
	// balance, all of it repaying.
	Proceeds    money.Quantity   `json:"proceeds"`
	Fee         money.Quantity   `json:"fee"`
	Repay       money.Quantity   `json:"repay"`
	Excess      money.Quantity   `json:"excess"`
	Allocations []LoanAllocation `json:"allocations"`
}

// Plan is the planner's deterministic answer for one step.
type Plan struct {
	Decision   PlanDecision      `json:"decision"`
	Reason     string            `json:"reason"`
	Target     LiquidationTarget `json:"target"`
	StepsTaken int               `json:"steps_taken"`
	// TargetUnits is the equity the case restores, at the current state.
	TargetUnits money.Quantity `json:"target_units"`
	Before      Evaluation     `json:"before"`
	// Orders are in execution order: a step executes Orders[0] and plans
	// again.
	Orders []LiquidationOrder `json:"orders"`
	// After is the evaluation once every order has executed in full.
	After *Evaluation `json:"after,omitempty"`
	// DeficitUnits is what remains owed once nothing is left (BANKRUPT).
	DeficitUnits money.Quantity `json:"deficit_units"`
	// FreeNumeraire is the free wallet cash the plan could sweep.
	FreeNumeraire money.Quantity `json:"free_numeraire"`
	Hash          string         `json:"hash"`
}

// ComputeHash returns the hash a plan with these fields carries.
func (pl Plan) ComputeHash() string {
	pl.Hash = ""
	return hashOf(pl)
}

// liquidationSlippage is the price concession a forced sale of a holding
// takes: its liquidity class's slippage, never above the cap; at the cap when
// asked (extreme spread).
func liquidationSlippage(p Policy, h Holding, atCap bool) money.BPS {
	if atCap {
		return p.LiquidationSlippageCapBPS
	}
	l, _ := p.liquidity(h.Profile.Liquidity)
	if l.LiquidationSlippageBPS > p.LiquidationSlippageCapBPS {
		return p.LiquidationSlippageCapBPS
	}
	return l.LiquidationSlippageBPS
}

// applySale executes a reduce-only sale of qty of the holding at exec on s:
// the holding shrinks, the proceeds net of the fee repay the loans (interest
// first, oldest lot first) and any excess stays as numeraire collateral.
func applySale(p Policy, s *AccountState, assetID string, qty money.Quantity, exec money.Price, slip money.BPS) (LiquidationOrder, error) {
	i := s.holding(assetID)
	if i < 0 || s.Holdings[i].Numeraire {
		return LiquidationOrder{}, errs.Newf(errs.CodeValidationFailed, "margin: %s is not a risky holding of the account", assetID)
	}
	h := s.Holdings[i]
	if !qty.IsPositive() || qty.Cmp(h.Quantity) > 0 {
		return LiquidationOrder{}, errs.New(errs.CodeValidationFailed, "margin: a liquidation sale is reduce-only and positive").
			WithField("asset_id", assetID).WithField("held", h.Quantity.String()).WithField("quantity", qty.String())
	}
	proceeds, err := notional(qty, h.Decimals, exec, s.NumeraireDecimals, money.RoundFloor)
	if err != nil {
		return LiquidationOrder{}, err
	}
	fee, err := mulBPS(proceeds, p.LiquidationFeeBPS, money.RoundCeil)
	if err != nil {
		return LiquidationOrder{}, err
	}
	net := proceeds.Sub(fee)
	repay := minQ(net, s.LoanTotal())
	loans, allocs, _ := allocateRepay(s.Loans, repay)
	s.Loans = loans
	s.Holdings[i].Quantity = h.Quantity.Sub(qty)
	excess := net.Sub(repay)
	if excess.IsPositive() {
		n := s.numeraireIndex()
		s.Holdings[n].Quantity = s.Holdings[n].Quantity.Add(excess)
	}
	mark := *h.Mark
	ex := exec
	return LiquidationOrder{
		Action: ActionSell, AssetID: assetID, Quantity: qty, Mark: &mark, ExecutionPrice: &ex, SlippageBPS: slip,
		Proceeds: proceeds, Fee: fee, Repay: repay, Excess: excess, Allocations: nonNilAllocs(allocs),
	}, nil
}

// applyCollateral uses numeraire collateral to repay the loans.
func applyCollateral(s *AccountState, amount money.Quantity) (LiquidationOrder, error) {
	n := s.holding(s.NumeraireAssetID)
	if n < 0 || !amount.IsPositive() || s.Holdings[n].Quantity.Cmp(amount) < 0 || amount.Cmp(s.LoanTotal()) > 0 {
		return LiquidationOrder{}, errs.New(errs.CodeValidationFailed, "margin: cannot apply that much numeraire collateral")
	}
	loans, allocs, _ := allocateRepay(s.Loans, amount)
	s.Loans = loans
	s.Holdings[n].Quantity = s.Holdings[n].Quantity.Sub(amount)
	return LiquidationOrder{
		Action: ActionApplyCollateral, AssetID: s.NumeraireAssetID, Quantity: amount,
		Proceeds: amount, Fee: money.Quantity{}, Repay: amount, Excess: money.Quantity{}, Allocations: nonNilAllocs(allocs),
	}, nil
}

// applySweep repays the loans from the customer's free numeraire cash
// outside the account. The account's holdings do not change: that cash was
// never collateral.
func applySweep(s *AccountState, amount money.Quantity) (LiquidationOrder, error) {
	if !amount.IsPositive() || amount.Cmp(s.LoanTotal()) > 0 {
		return LiquidationOrder{}, errs.New(errs.CodeValidationFailed, "margin: cannot sweep that much of the free balance into the loans")
	}
	loans, allocs, _ := allocateRepay(s.Loans, amount)
	s.Loans = loans
	return LiquidationOrder{
		Action: ActionSweepWallet, AssetID: s.NumeraireAssetID, Quantity: amount,
		Proceeds: amount, Fee: money.Quantity{}, Repay: amount, Excess: money.Quantity{}, Allocations: nonNilAllocs(allocs),
	}, nil
}

func nonNilAllocs(a []LoanAllocation) []LoanAllocation {
	if a == nil {
		return []LoanAllocation{}
	}
	return a
}

type candidate struct {
	assetID string
	haircut money.BPS
	rank    int
}

// PlanLiquidation decides one step of a liquidation case. It is pure and
// deterministic. In order:
//
//  1. COMPLETE when the account owes nothing or its equity is at or above
//     the target (initial margin, or maintenance plus the warning buffer).
//  2. With no risky holding left -- terminal accounting, which the trading
//     budget never preempts: APPLY_COLLATERAL while numeraire collateral and
//     loans remain; MANUAL_REVIEW while derivative exposure remains (the
//     derivatives lane closes it); SWEEP_WALLET while loans remain and the
//     customer has free numeraire cash outside the account; BANKRUPT when
//     only liabilities remain.
//  3. MANUAL_REVIEW when the case has used its budget of trading steps: a
//     case never loops.
//  4. AWAIT_PRICE when any risky holding has a stale or missing mark. No
//     liquidation runs on a price nobody can vouch for.
//  5. AWAIT_VENUE when the venue cannot execute.
//  6. SELL: the smallest set of reductions that restores the target. Risky
//     holdings are taken in priority order -- largest haircut first, then
//     riskiest volatility class, ties by asset id -- each sold in full until
//     the next one can restore the target, and that one is sold in the
//     smallest quantity that does (found by bisection over exact
//     re-evaluation, and verified).
//  7. SELL_ALL when even selling everything does not restore the target.
//
// Every sale is reduce-only and executes at mark × (1 − slippage) with the
// slippage capped by the policy; the fee is platform fee revenue and the rest
// repays the loans.
func PlanLiquidation(p Policy, in PlanInput) (Plan, error) {
	if in.StepsTaken < 0 || in.FreeNumeraire.IsNegative() {
		return Plan{}, errs.New(errs.CodeValidationFailed, "margin: steps taken and the free balance cannot be negative")
	}
	policyHash := p.Hash()
	before, err := evaluateCore(p, policyHash, in.State)
	if err != nil {
		return Plan{}, err
	}
	before.Hash = before.ComputeHash()
	pl := Plan{
		Target: p.PartialTarget, StepsTaken: in.StepsTaken, Before: before,
		TargetUnits: before.Target(p), Orders: []LiquidationOrder{}, DeficitUnits: money.Quantity{},
		FreeNumeraire: in.FreeNumeraire,
	}
	finish := func(d PlanDecision, reason string) (Plan, error) {
		pl.Decision, pl.Reason = d, reason
		pl.Hash = pl.ComputeHash()
		return pl, nil
	}
	if before.Restored(p) {
		return finish(DecisionComplete, "equity is at or above the target, or nothing is owed")
	}

	var cands []candidate
	for _, v := range before.Holdings {
		if v.Numeraire || !v.Quantity.IsPositive() {
			continue
		}
		h := in.State.Holdings[in.State.holding(v.AssetID)]
		cands = append(cands, candidate{assetID: v.AssetID, haircut: v.Haircut.TotalBPS, rank: h.Profile.Volatility.Rank()})
	}
	if len(cands) == 0 {
		// Terminal accounting. Nothing is left to trade, so the trading
		// budget does not apply: a case that has sold everything always
		// reaches its applying, sweeping or bankruptcy.
		st := in.State.Clone()
		loans := st.LoanTotal()
		numeraire := money.Quantity{}
		if n := st.holding(st.NumeraireAssetID); n >= 0 {
			numeraire = st.Holdings[n].Quantity
		}
		switch {
		case numeraire.IsPositive() && loans.IsPositive():
			o, err := applyCollateral(&st, minQ(numeraire, loans))
			if err != nil {
				return Plan{}, err
			}
			after, err := Evaluate(p, st)
			if err != nil {
				return Plan{}, err
			}
			pl.Orders, pl.After = []LiquidationOrder{o}, &after
			return finish(DecisionApplyCollateral, "nothing risky is left; numeraire collateral repays the loans")
		case len(in.State.Derivatives) > 0:
			return finish(DecisionManualReview, "the remaining exposure is derivative; the derivatives lane closes it")
		case loans.IsPositive() && in.FreeNumeraire.IsPositive():
			o, err := applySweep(&st, minQ(in.FreeNumeraire, loans))
			if err != nil {
				return Plan{}, err
			}
			after, err := Evaluate(p, st)
			if err != nil {
				return Plan{}, err
			}
			pl.Orders, pl.After = []LiquidationOrder{o}, &after
			return finish(DecisionSweepWallet, "nothing is left in the account; the customer's free balance repays the loans before any bankruptcy")
		case before.Equity.IsNegative():
			pl.DeficitUnits = before.Equity.Neg()
			return finish(DecisionBankrupt, "nothing is left to sell, apply or sweep and liabilities remain")
		default:
			return finish(DecisionManualReview, "the remaining shortfall is not something a liquidation can reduce")
		}
	}
	if in.StepsTaken >= p.MaxLiquidationSteps {
		return finish(DecisionManualReview, "the case has used its budget of trading steps")
	}
	if !before.PricesFresh {
		return finish(DecisionAwaitPrice, "a risky holding has a stale or missing mark")
	}
	if !in.VenueAvailable {
		return finish(DecisionAwaitVenue, "the venue cannot execute a liquidation")
	}

	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.haircut != b.haircut {
			return a.haircut > b.haircut
		}
		if a.rank != b.rank {
			return a.rank > b.rank
		}
		return a.assetID < b.assetID
	})

	restored := func(st AccountState) (bool, Evaluation, error) {
		ev, err := evaluateCore(p, policyHash, st)
		if err != nil {
			return false, Evaluation{}, err
		}
		return ev.Restored(p), ev, nil
	}
	sell := func(st *AccountState, assetID string, qty money.Quantity) (LiquidationOrder, error) {
		h := st.Holdings[st.holding(assetID)]
		slip := liquidationSlippage(p, h, in.SlippageAtCap)
		exec, err := scalePrice(*h.Mark, int64(10_000-slip))
		if err != nil {
			return LiquidationOrder{}, err
		}
		return applySale(p, st, assetID, qty, exec, slip)
	}

	cur := in.State.Clone()
	for _, c := range cands {
		full := cur.Holdings[cur.holding(c.assetID)].Quantity
		trial := cur.Clone()
		if _, err := sell(&trial, c.assetID, full); err != nil {
			return Plan{}, err
		}
		ok, _, err := restored(trial)
		if err != nil {
			return Plan{}, err
		}
		if ok {
			// Smallest quantity of this holding that restores the target:
			// health(lo) fails, health(hi) holds.
			lo, hi := money.Quantity{}, full
			one := money.QuantityFromInt64(1)
			for hi.Sub(lo).Cmp(one) > 0 {
				mid, err := lo.Add(hi).Div(money.QuantityFromInt64(2), money.RoundFloor)
				if err != nil {
					return Plan{}, errs.Wrap(err, errs.CodeInternal, "margin: bisection")
				}
				t := cur.Clone()
				if _, err := sell(&t, c.assetID, mid); err != nil {
					return Plan{}, err
				}
				okMid, _, err := restored(t)
				if err != nil {
					return Plan{}, err
				}
				if okMid {
					hi = mid
				} else {
					lo = mid
				}
			}
			final := cur.Clone()
			o, err := sell(&final, c.assetID, hi)
			if err != nil {
				return Plan{}, err
			}
			okFinal, after, err := restored(final)
			if err != nil {
				return Plan{}, err
			}
			if okFinal {
				after.Hash = after.ComputeHash()
				pl.Orders = append(pl.Orders, o)
				pl.After = &after
				return finish(DecisionSell, "a partial liquidation restores the target")
			}
			// Not monotone here: fall through and sell this holding in full.
		}
		o, err := sell(&cur, c.assetID, full)
		if err != nil {
			return Plan{}, err
		}
		pl.Orders = append(pl.Orders, o)
		if ok {
			after, err := Evaluate(p, cur)
			if err != nil {
				return Plan{}, err
			}
			pl.After = &after
			return finish(DecisionSell, "a partial liquidation restores the target")
		}
	}
	after, err := Evaluate(p, cur)
	if err != nil {
		return Plan{}, err
	}
	pl.After = &after
	return finish(DecisionSellAll, "no partial liquidation restores the target; every risky holding is sold")
}

// FillQuantity is the part of an order a step executes at a fill fraction:
// floor(quantity × fill / 10_000), but at least one base unit so a step
// always makes progress.
func FillQuantity(qty money.Quantity, fillBPS money.BPS) money.Quantity {
	if fillBPS >= 10_000 || !qty.IsPositive() {
		return qty
	}
	f, _ := mulBPS(qty, fillBPS, money.RoundFloor)
	if !f.IsPositive() {
		return money.QuantityFromInt64(1)
	}
	return f
}

// ExecuteFirst executes the first order of a SELL, SELL_ALL,
// APPLY_COLLATERAL or SWEEP_WALLET plan on the state at a fill fraction
// (10_000 = in full) and returns the new state and the order as executed. A
// sale executes at the plan's execution price; a partial fill sells less and
// repays less. Applying collateral and sweeping the wallet are bookkeeping,
// not fills: they always execute in full.
func ExecuteFirst(p Policy, s AccountState, pl Plan, fillBPS money.BPS) (AccountState, LiquidationOrder, error) {
	switch pl.Decision {
	case DecisionSell, DecisionSellAll, DecisionApplyCollateral, DecisionSweepWallet:
	default:
		return AccountState{}, LiquidationOrder{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: a %s plan has nothing to execute", pl.Decision)
	}
	if len(pl.Orders) == 0 {
		return AccountState{}, LiquidationOrder{}, errs.New(errs.CodeInternal, "margin: plan without orders")
	}
	if fillBPS <= 0 || fillBPS > 10_000 {
		return AccountState{}, LiquidationOrder{}, errs.New(errs.CodeValidationFailed, "margin: fill fraction must be within (0, 10000]")
	}
	o := pl.Orders[0]
	out := s.Clone()
	switch o.Action {
	case ActionApplyCollateral:
		done, err := applyCollateral(&out, o.Quantity)
		return out, done, err
	case ActionSweepWallet:
		done, err := applySweep(&out, o.Quantity)
		return out, done, err
	}
	done, err := applySale(p, &out, o.AssetID, FillQuantity(o.Quantity, fillBPS), *o.ExecutionPrice, o.SlippageBPS)
	return out, done, err
}
