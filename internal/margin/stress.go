package margin

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// StressResult is one scenario's outcome. Values are numeraire base units,
// with the cents rendering beside them (equity rounded down, requirements
// and deficits rounded up).
type StressResult struct {
	Scenario StressScenario `json:"scenario"`
	// ShockBPS is the price move the scenario applied, when it applied one.
	ShockBPS money.BPS `json:"shock_bps"`

	EquityAfter       money.Quantity `json:"equity_after"`
	MaintenanceAfter  money.Quantity `json:"maintenance_after"`
	InitialAfter      money.Quantity `json:"initial_after"`
	EquityAfterUSD    money.USD      `json:"equity_after_usd"`
	MaintenanceAftUSD money.USD      `json:"maintenance_after_usd"`

	// WouldLiquidate: after the shock the account is below maintenance.
	WouldLiquidate bool `json:"would_liquidate"`
	// Executable: the simulated liquidation could run (prices and venue
	// available). Outcome is how the simulated case ended.
	Executable     bool         `json:"executable"`
	Outcome        PlanDecision `json:"outcome"`
	StepsSimulated int          `json:"steps_simulated"`

	// Deficit is what the lender is left owed: the simulated case's deficit
	// when it ends bankrupt, otherwise the conservative shortfall
	// max(0, −equity) of the state it stopped in. The insurance fund covers
	// a deficit in full, so InsuranceFundDraw equals it.
	Deficit              money.Quantity `json:"deficit"`
	DeficitUSD           money.USD      `json:"deficit_usd"`
	InsuranceFundDraw    money.Quantity `json:"insurance_fund_draw"`
	InsuranceFundDrawUSD money.USD      `json:"insurance_fund_draw_usd"`
}

// StressReport is the deterministic stress run of one account.
type StressReport struct {
	AccountID     string         `json:"account_id"`
	PolicyVersion string         `json:"policy_version"`
	PolicyHash    string         `json:"policy_hash"`
	InputHash     string         `json:"input_hash"`
	Results       []StressResult `json:"results"`
	Hash          string         `json:"hash"`
}

// ComputeHash returns the hash a report with these fields carries.
func (r StressReport) ComputeHash() string {
	r.Hash = ""
	return hashOf(r)
}

// shockState moves the marks of the holdings that match by num / 10_000
// exactly, and charges every derivative exposure an adverse move of bps of
// its gross notional.
func shockState(s AccountState, num int64, match func(Holding) bool, derivBPS money.BPS) (AccountState, error) {
	out := s.Clone()
	for i, h := range out.Holdings {
		if h.Numeraire || h.Mark == nil || !match(h) {
			continue
		}
		m, err := scalePrice(*h.Mark, num)
		if err != nil {
			return AccountState{}, err
		}
		out.Holdings[i].Mark = &m
	}
	if derivBPS > 0 {
		for i, d := range out.Derivatives {
			loss, err := d.GrossNotional.MulBPS(derivBPS, money.RoundCeil)
			if err != nil {
				return AccountState{}, errs.Wrap(err, errs.CodeOverflow, "margin: stress a derivative exposure")
			}
			if out.Derivatives[i].UnrealizedPnL, err = d.UnrealizedPnL.Sub(loss); err != nil {
				return AccountState{}, errs.Wrap(err, errs.CodeOverflow, "margin: stress a derivative exposure")
			}
		}
	}
	return out, nil
}

func risky(h Holding) bool  { return h.Profile.Volatility != VolStable }
func stable(h Holding) bool { return h.Profile.Volatility == VolStable }

type stressCase struct {
	scenario StressScenario
	shock    money.BPS
	prepare  func(AccountState) (AccountState, error)
	venue    bool
	fill     money.BPS
	atCap    bool
}

// RunStress applies every scenario of AllStressScenarios to the account and
// simulates the liquidation each one would trigger, deterministically:
//
//   - GAP_DOWN_g: every risky (non-stable) holding's mark falls by g; every
//     derivative exposure loses g of its gross notional.
//   - GAP_UP_g: risky marks rise by g (a long gains) and every derivative
//     exposure still loses g: the contract carries no direction, so a gap in
//     either direction is taken as adverse to a short.
//   - ORACLE_UNAVAILABLE: every non-numeraire mark disappears (100% haircut;
//     the case would wait for a price).
//   - VENUE_UNAVAILABLE, PARTIAL_FILL, EXTREME_SPREAD: the policy's execution
//     gap down is applied, then the liquidation runs with no venue, with
//     each step filling only StressPartialFillBPS, or with every sale at the
//     slippage cap.
//   - BRIDGE_DELAYED: collateral in flight is worth nothing.
//   - COLLATERAL_DEPEG: stable (non-numeraire) holdings fall by
//     StressDepegBPS. The numeraire is the unit of account and cannot move
//     against itself.
//
// The simulated case uses the same planner as a real one and stops at the
// step budget, so a run always terminates.
func RunStress(p Policy, s AccountState) (StressReport, error) {
	if err := s.Validate(); err != nil {
		return StressReport{}, err
	}
	rep := StressReport{AccountID: s.AccountID, PolicyVersion: p.Version, PolicyHash: p.Hash(), InputHash: hashOf(s)}
	gap := func(sc StressScenario, g money.BPS, up bool) stressCase {
		num := int64(10_000 - g)
		if up {
			num = int64(10_000 + g)
		}
		return stressCase{scenario: sc, shock: g, venue: true, fill: 10_000, prepare: func(st AccountState) (AccountState, error) {
			return shockState(st, num, risky, g)
		}}
	}
	execGap := func(st AccountState) (AccountState, error) {
		return shockState(st, int64(10_000-p.StressExecutionGapBPS), risky, p.StressExecutionGapBPS)
	}
	cases := []stressCase{
		gap(StressGapDown5, 500, false), gap(StressGapDown10, 1_000, false),
		gap(StressGapDown25, 2_500, false), gap(StressGapDown50, 5_000, false),
		gap(StressGapUp5, 500, true), gap(StressGapUp10, 1_000, true),
		gap(StressGapUp25, 2_500, true), gap(StressGapUp50, 5_000, true),
		{scenario: StressOracleUnavailable, venue: true, fill: 10_000, prepare: func(st AccountState) (AccountState, error) {
			out := st.Clone()
			for i := range out.Holdings {
				if !out.Holdings[i].Numeraire {
					out.Holdings[i].Mark = nil
				}
			}
			return out, nil
		}},
		{scenario: StressVenueUnavailable, shock: p.StressExecutionGapBPS, venue: false, fill: 10_000, prepare: execGap},
		{scenario: StressPartialFill, shock: p.StressExecutionGapBPS, venue: true, fill: p.StressPartialFillBPS, prepare: execGap},
		{scenario: StressExtremeSpread, shock: p.StressExecutionGapBPS, venue: true, fill: 10_000, atCap: true, prepare: execGap},
		{scenario: StressBridgeDelayed, venue: true, fill: 10_000, prepare: func(st AccountState) (AccountState, error) {
			out := st.Clone()
			for i := range out.Holdings {
				if out.Holdings[i].InFlight {
					out.Holdings[i].BridgeDelayed = true
				}
			}
			return out, nil
		}},
		{scenario: StressCollateralDepeg, shock: p.StressDepegBPS, venue: true, fill: 10_000, prepare: func(st AccountState) (AccountState, error) {
			return shockState(st, int64(10_000-p.StressDepegBPS), stable, 0)
		}},
	}
	for _, c := range cases {
		r, err := runStressCase(p, s, c)
		if err != nil {
			return StressReport{}, err
		}
		rep.Results = append(rep.Results, r)
	}
	rep.Hash = rep.ComputeHash()
	return rep, nil
}

func runStressCase(p Policy, s AccountState, c stressCase) (StressResult, error) {
	shocked, err := c.prepare(s)
	if err != nil {
		return StressResult{}, err
	}
	ev, err := Evaluate(p, shocked)
	if err != nil {
		return StressResult{}, err
	}
	r := StressResult{
		Scenario: c.scenario, ShockBPS: c.shock,
		EquityAfter: ev.Equity, MaintenanceAfter: ev.MaintenanceMargin, InitialAfter: ev.InitialMargin,
		WouldLiquidate: ev.LiquidationEligible, Executable: true, Outcome: DecisionComplete,
	}
	if r.EquityAfterUSD, err = unitsToUSD(ev.Equity, s.NumeraireDecimals, money.RoundFloor); err != nil {
		return StressResult{}, err
	}
	if r.MaintenanceAftUSD, err = unitsToUSD(ev.MaintenanceMargin, s.NumeraireDecimals, money.RoundCeil); err != nil {
		return StressResult{}, err
	}
	deficit := money.Quantity{}
	if ev.LiquidationEligible {
		cur := shocked
		steps := 0
		for {
			pl, err := PlanLiquidation(p, PlanInput{State: cur, StepsTaken: steps, VenueAvailable: c.venue, SlippageAtCap: c.atCap})
			if err != nil {
				return StressResult{}, err
			}
			r.Outcome = pl.Decision
			if pl.Decision == DecisionBankrupt {
				deficit = pl.DeficitUnits
				break
			}
			if pl.Decision == DecisionAwaitPrice || pl.Decision == DecisionAwaitVenue {
				r.Executable = false
			}
			if pl.Decision != DecisionSell && pl.Decision != DecisionSellAll && pl.Decision != DecisionApplyCollateral {
				end, err := Evaluate(p, cur)
				if err != nil {
					return StressResult{}, err
				}
				if end.Equity.IsNegative() {
					deficit = end.Equity.Neg()
				}
				break
			}
			if steps > p.MaxLiquidationSteps {
				return StressResult{}, errs.New(errs.CodeInternal, "margin: the planner did not stop at its step budget")
			}
			if cur, _, err = ExecuteFirst(p, cur, pl, c.fill); err != nil {
				return StressResult{}, err
			}
			steps++
		}
		r.StepsSimulated = steps
	}
	r.Deficit, r.InsuranceFundDraw = deficit, deficit
	if r.DeficitUSD, err = unitsToUSD(deficit, s.NumeraireDecimals, money.RoundCeil); err != nil {
		return StressResult{}, err
	}
	r.InsuranceFundDrawUSD = r.DeficitUSD
	return r, nil
}
