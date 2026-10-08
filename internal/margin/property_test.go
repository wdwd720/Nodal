package margin

import (
	"fmt"
	"time"

	"testing"

	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

var riskyIDs = []string{"btc", "eth", "sol", "stb"}

func genProfile(t *rapid.T, label string) AssetProfile {
	return AssetProfile{
		Volatility:    rapid.SampledFrom(AllVolatilityClasses()).Draw(t, label+".vol"),
		Liquidity:     rapid.SampledFrom(AllLiquidityClasses()).Draw(t, label+".liq"),
		Venue:         rapid.SampledFrom(AllVenueTiers()).Draw(t, label+".venue"),
		RiskWeightBPS: rapid.SampledFrom([]money.BPS{5_000, 10_000, 15_000}).Draw(t, label+".weight"),
	}
}

// genMark is a fresh mark, a stale one, one from the future, or none.
func genMark(t *rapid.T, label string, now time.Time, freshOnly bool) *money.Price {
	kind := "fresh"
	if !freshOnly {
		kind = rapid.SampledFrom([]string{"fresh", "fresh", "fresh", "stale", "missing", "future"}).Draw(t, label+".kind")
	}
	if kind == "missing" {
		return nil
	}
	at := now.Add(-time.Duration(rapid.Int64Range(0, 30_000).Draw(t, label+".age")) * time.Millisecond)
	switch kind {
	case "stale":
		at = now.Add(-time.Duration(rapid.Int64Range(30_001, 86_400_000).Draw(t, label+".staleAge")) * time.Millisecond)
	case "future":
		at = now.Add(time.Duration(rapid.Int64Range(1, 60_000).Draw(t, label+".ahead")) * time.Millisecond)
	}
	p, err := money.NewPrice(money.QuantityFromInt64(rapid.Int64Range(0, 5_000_000).Draw(t, label+".mantissa")),
		int32(rapid.IntRange(0, 4).Draw(t, label+".scale")), usdID, "prop", at)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	return &p
}

type genOpts struct {
	modes     []MarginMode
	freshOnly bool
	minRisky  int
}

func genState(t *rapid.T, o genOpts) AccountState {
	now := t0
	mode := rapid.SampledFrom(o.modes).Draw(t, "mode")
	s := AccountState{AccountID: "prop", Mode: mode, NumeraireAssetID: usdID, NumeraireDecimals: 6, Now: now,
		Holdings: []Holding{}, Loans: []Loan{}, Derivatives: []DerivativeExposure{}}
	if rapid.Bool().Draw(t, "hasCash") {
		s.Holdings = append(s.Holdings, Holding{AssetID: usdID, Decimals: 6, Numeraire: true,
			Quantity: money.QuantityFromInt64(rapid.Int64Range(0, 2_000_000_000_000).Draw(t, "cash"))})
	}
	maxRisky := 3
	if mode == ModeIsolated {
		maxRisky = 1
		s.IsolatedAssetID = rapid.SampledFrom(riskyIDs).Draw(t, "isolated")
	}
	n := rapid.IntRange(o.minRisky, maxRisky).Draw(t, "risky")
	ids := rapid.Permutation(riskyIDs).Draw(t, "ids")
	for i := 0; i < n; i++ {
		aid := ids[i]
		if mode == ModeIsolated {
			aid = s.IsolatedAssetID
		}
		label := fmt.Sprintf("h%d", i)
		dec := rapid.SampledFrom([]int{0, 6, 9}).Draw(t, label+".dec")
		// At most a million whole units at a mark of at most five million:
		// every value stays inside the cents range the engine reports in.
		maxQty := int64(1_000_000)
		for k := 0; k < dec; k++ {
			maxQty *= 10
		}
		s.Holdings = append(s.Holdings, Holding{
			AssetID: aid, Decimals: uint8(dec),
			Quantity: money.QuantityFromInt64(rapid.Int64Range(0, maxQty).Draw(t, label+".qty")),
			Mark:     genMark(t, label, now, o.freshOnly), Profile: genProfile(t, label),
			SettlementDelayMS: rapid.SampledFrom([]int64{0, 0, 3_600_000, 90_000_000}).Draw(t, label+".settle"),
			InFlight:          rapid.Bool().Draw(t, label+".inflight"),
			InFlightMS:        rapid.Int64Range(0, 10_000_000).Draw(t, label+".inflightMS"),
		})
	}
	if mode != ModeCash {
		for i := 0; i < rapid.IntRange(0, 2).Draw(t, "loans"); i++ {
			label := fmt.Sprintf("l%d", i)
			s.Loans = append(s.Loans, Loan{LotID: label,
				Principal:      money.QuantityFromInt64(rapid.Int64Range(0, 3_000_000_000_000).Draw(t, label+".p")),
				Interest:       money.QuantityFromInt64(rapid.Int64Range(0, 1_000_000).Draw(t, label+".i")),
				RateBPS:        money.BPS(rapid.Int64Range(0, 3_000).Draw(t, label+".rate")),
				AccruedThrough: now.Add(-time.Duration(rapid.Int64Range(0, 864_000).Draw(t, label+".age")) * time.Second)})
		}
	}
	if mode != ModeIsolated {
		for i := 0; i < rapid.IntRange(0, 2).Draw(t, "derivs"); i++ {
			label := fmt.Sprintf("d%d", i)
			im := rapid.Int64Range(0, 10_000_000).Draw(t, label+".im")
			s.Derivatives = append(s.Derivatives, DerivativeExposure{InstrumentID: label,
				GrossNotional:           money.USDFromMinor(rapid.Int64Range(0, 100_000_000).Draw(t, label+".gross")),
				InitialMargin:           money.USDFromMinor(im),
				MaintenanceMargin:       money.USDFromMinor(rapid.Int64Range(0, im).Draw(t, label+".mm")),
				UnrealizedPnL:           money.USDFromMinor(rapid.Int64Range(-10_000_000, 10_000_000).Draw(t, label+".upnl")),
				RiskIncreasingDirection: rapid.Bool().Draw(t, label+".dir")})
		}
	}
	s.RealizedPnL = money.USDFromMinor(rapid.Int64Range(-1_000_000, 1_000_000).Draw(t, "realized"))
	s.AccruedFees = money.USDFromMinor(rapid.Int64Range(0, 100_000).Draw(t, "fees"))
	return s
}

func evalOrFatal(t *rapid.T, p Policy, s AccountState) Evaluation {
	e, err := Evaluate(p, s)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return e
}

// TestProp_EquityIdentityHolds: for every state, equity is exactly eligible
// collateral + realized + unrealized − liabilities − fees − interest, the
// liquidation buffer is equity − maintenance, and the evaluation is
// reproducible byte for byte.
func TestProp_EquityIdentityHolds(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(t *rapid.T) {
		s := genState(t, genOpts{modes: AllMarginModes()})
		e := evalOrFatal(t, p, s)
		sum := e.EligibleCollateral.Add(e.RealizedPnL).Add(e.UnrealizedPnL).Sub(e.BorrowLiabilities).Sub(e.AccruedFees).Sub(e.AccruedInterest)
		if !sum.Equal(e.Equity) {
			t.Fatalf("identity broken: components %s, equity %s", sum, e.Equity)
		}
		if !e.Equity.Sub(e.MaintenanceMargin).Equal(e.LiquidationBuffer) {
			t.Fatalf("buffer is not equity − maintenance")
		}
		if e.EligibleCollateral.Cmp(e.MarketValue) > 0 {
			t.Fatalf("collateral %s above market value %s", e.EligibleCollateral, e.MarketValue)
		}
		if again := evalOrFatal(t, p, s); again.Hash != e.Hash {
			t.Fatalf("evaluation is not deterministic")
		}
	})
}

// TestProp_MarginAvailableNeverExceedsCapacity: margin available is never
// negative, never above the policy-defined capacity (the equity not needed
// to keep gross exposure within the mode's leverage cap) and never above
// equity.
func TestProp_MarginAvailableNeverExceedsCapacity(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(t *rapid.T) {
		s := genState(t, genOpts{modes: AllMarginModes()})
		e := evalOrFatal(t, p, s)
		capacity := e.Equity
		if e.Levered {
			needed, err := e.GrossExposure.MulDiv(money.QuantityFromInt64(10_000), money.QuantityFromInt64(int64(p.LeverageCap(s.Mode))), money.RoundCeil)
			if err != nil {
				t.Fatal(err)
			}
			capacity = e.Equity.Sub(needed)
		}
		if !capacity.Equal(e.Capacity) {
			t.Fatalf("capacity %s, independently %s", e.Capacity, capacity)
		}
		if e.MarginAvailable.IsNegative() || e.MarginAvailable.Cmp(capacity.Max(money.Quantity{})) > 0 ||
			e.MarginAvailable.Cmp(e.Equity.Max(money.Quantity{})) > 0 {
			t.Fatalf("available %s exceeds capacity %s or equity %s", e.MarginAvailable, capacity, e.Equity)
		}
		if e.Levered && e.MarginAvailable.Cmp(e.Equity.Sub(e.InitialMargin).Max(money.Quantity{})) > 0 {
			t.Fatalf("available %s exceeds equity − initial", e.MarginAvailable)
		}
	})
}

// TestProp_LosingLeveragedPositionNeverIncreasesWithdrawableBuyingPower:
// buying anything on margin at or above its mark -- a position that loses
// from the moment it opens -- never increases margin available, the
// withdrawable buying power. (Concentration is measured against net asset
// value, so a borrowed purchase can never dilute another position's
// concentration haircut.)
func TestProp_LosingLeveragedPositionNeverIncreasesWithdrawableBuyingPower(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(t *rapid.T) {
		s := genState(t, genOpts{modes: []MarginMode{ModeCross, ModeIsolated}})
		before := evalOrFatal(t, p, s)
		aid := rapid.SampledFrom(riskyIDs).Draw(t, "buy")
		if s.Mode == ModeIsolated {
			aid = s.IsolatedAssetID
		}
		var template Holding
		if i := s.holding(aid); i >= 0 {
			template = s.Holdings[i]
		} else {
			template = Holding{AssetID: aid, Decimals: 9, Mark: genMark(t, "new", s.Now, false), Profile: genProfile(t, "new")}
		}
		if template.Mark == nil {
			return // nothing to price a buy at; the service refuses it
		}
		exec, err := scalePrice(*template.Mark, int64(10_000+rapid.Int64Range(0, 2_000).Draw(t, "slippage")))
		if err != nil {
			t.Fatal(err)
		}
		maxQty := int64(1_000_000)
		for k := uint8(0); k < template.Decimals; k++ {
			maxQty *= 10
		}
		qty := money.QuantityFromInt64(rapid.Int64Range(1, maxQty).Draw(t, "qty"))
		after, cost, err := WithLeveragedBuy(s, template, qty, exec, "new-lot", p.BorrowRateBPS)
		if err != nil {
			return // a buy that costs nothing finances nothing
		}
		_ = cost
		ae := evalOrFatal(t, p, after)
		if ae.MarginAvailable.Cmp(before.MarginAvailable) > 0 {
			t.Fatalf("margin available grew from %s to %s by buying %s %s at %s on margin",
				before.MarginAvailable, ae.MarginAvailable, qty, aid, exec.String())
		}
		if ae.Equity.Cmp(before.Equity) > 0 {
			t.Fatalf("equity grew from %s to %s", before.Equity, ae.Equity)
		}
	})
}

// TestProp_NoWithdrawalBreachesMaintenance: whenever Risk Kernel V2 allows a
// release with the sections the engine produces, equity after the release
// is at or above maintenance; and a release that would breach maintenance is
// refused with RISK_WITHDRAW_BREACHES_MARGIN.
func TestProp_NoWithdrawalBreachesMaintenance(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(t *rapid.T) {
		s := genState(t, genOpts{modes: AllMarginModes(), minRisky: 1})
		var candidates []Holding
		for _, h := range s.Holdings {
			if h.Quantity.IsPositive() {
				candidates = append(candidates, h)
			}
		}
		if len(candidates) == 0 {
			return
		}
		h := rapid.SampledFrom(candidates).Draw(t, "release")
		q, err := h.Quantity.MulDiv(money.QuantityFromInt64(rapid.Int64Range(1, 10_000).Draw(t, "share")), money.QuantityFromInt64(10_000), money.RoundCeil)
		if err != nil {
			t.Fatal(err)
		}
		after, err := WithRelease(s, h.AssetID, q)
		if err != nil {
			t.Fatal(err)
		}
		ae := evalOrFatal(t, p, after)
		sections, err := RiskSectionsFor(ae)
		if err != nil {
			t.Fatal(err)
		}
		in := risk.AdvancedInput{
			Stage:    risk.StagePreTrade,
			Intent:   risk.AdvancedIntent{ID: "i", Kind: "TRANSFER_VALUE", AccountID: "a", Capability: "MARGIN", Mode: "PAPER", RiskIncreasing: ae.Levered},
			Identity: &risk.IdentitySnapshot{Verified: true}, Account: &risk.AdvancedAccountSnapshot{Status: "ACTIVE"},
			Balance:     &risk.BalanceSnapshot{},
			Velocity:    &risk.VelocitySnapshot{},
			Destination: &risk.DestinationSnapshot{Allowlisted: true, AddedAt: t0.Add(-time.Hour)},
			Margin:      sections.Margin, Oracle: sections.Oracle, Concentration: sections.Concentration,
			Now: s.Now,
		}
		d := risk.EvaluateAdvanced(EffectiveRiskPolicy(risk.DefaultAdvancedPolicy(), p, s.Mode), in)
		breach := ae.Equity.Cmp(ae.MaintenanceMargin) < 0
		if d.Verdict == risk.Allow && breach {
			t.Fatalf("release allowed leaving equity %s below maintenance %s", ae.Equity, ae.MaintenanceMargin)
		}
		if breach {
			found := false
			for _, c := range d.ReasonCodes {
				if c == risk.ReasonWithdrawBreachesMargin {
					found = true
				}
			}
			if !found {
				t.Fatalf("a maintenance breach was refused without RISK_WITHDRAW_BREACHES_MARGIN: %v", d.ReasonCodes)
			}
		}
	})
}

// TestProp_StaleMarksBlockNewLeverage: a borrow (or a leveraged buy) on an
// account holding any risky asset whose mark is stale, missing or from the
// future is refused by Risk Kernel V2 with an oracle reason, whatever else
// is true of the account.
func TestProp_StaleMarksBlockNewLeverage(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(t *rapid.T) {
		s := genState(t, genOpts{modes: []MarginMode{ModeCross, ModeIsolated}, minRisky: 1})
		i := -1
		for j, h := range s.Holdings {
			if !h.Numeraire {
				i = j
			}
		}
		if i < 0 {
			t.Fatal("generator produced no risky holding")
		}
		s.Holdings[i].Quantity = s.Holdings[i].Quantity.Add(money.QuantityFromInt64(1))
		switch rapid.IntRange(0, 2).Draw(t, "how") {
		case 0:
			m := *px(t, "10", s.Now.Add(-31*time.Second))
			s.Holdings[i].Mark = &m
		case 1:
			s.Holdings[i].Mark = nil
		case 2:
			m := *px(t, "10", s.Now.Add(time.Second))
			s.Holdings[i].Mark = &m
		}
		after, err := WithBorrow(s, "new-lot", money.QuantityFromInt64(rapid.Int64Range(1, 1_000_000_000).Draw(t, "amount")), 800)
		if err != nil {
			t.Fatal(err)
		}
		ae := evalOrFatal(t, p, after)
		if ae.PricesFresh {
			t.Fatal("a stale holding left the evaluation fresh")
		}
		sections, err := RiskSectionsFor(ae)
		if err != nil {
			t.Fatal(err)
		}
		in := risk.AdvancedInput{
			Stage:    risk.StagePreTrade,
			Intent:   risk.AdvancedIntent{ID: "i", Kind: "BORROW", AccountID: "a", Capability: "LEVERAGE", Mode: "PAPER", RiskIncreasing: true},
			Identity: &risk.IdentitySnapshot{Verified: true}, Account: &risk.AdvancedAccountSnapshot{Status: "ACTIVE"},
			Margin: sections.Margin, Oracle: sections.Oracle, Concentration: sections.Concentration, Now: s.Now,
		}
		d := risk.EvaluateAdvanced(EffectiveRiskPolicy(risk.DefaultAdvancedPolicy(), p, s.Mode), in)
		if d.Verdict == risk.Allow {
			t.Fatalf("new leverage allowed on a stale mark")
		}
		oracle := false
		for _, c := range d.ReasonCodes {
			if c == risk.ReasonOracleStale || c == risk.ReasonOracleUnavailable {
				oracle = true
			}
		}
		if !oracle {
			t.Fatalf("refused without an oracle reason: %v", d.ReasonCodes)
		}
	})
}

// TestProp_HaircutIsMonotoneInEachFactor: raising any single factor -- a
// riskier volatility or liquidity class, a riskier venue, an older mark, a
// larger concentration, a longer settlement or bridge delay, being in
// flight, a stuck bridge -- never lowers the haircut, and the haircut never
// leaves [0, 10000].
func TestProp_HaircutIsMonotoneInEachFactor(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(t *rapid.T) {
		f := HaircutFactors{
			Volatility: rapid.SampledFrom(AllVolatilityClasses()).Draw(t, "vol"), Liquidity: rapid.SampledFrom(AllLiquidityClasses()).Draw(t, "liq"),
			Venue: rapid.SampledFrom(AllVenueTiers()).Draw(t, "venue"), OracleAgeMS: rapid.Int64Range(0, 100_000).Draw(t, "age"),
			MarkMissing: rapid.Bool().Draw(t, "missing"), ConcentrationShareBPS: money.BPS(rapid.Int64Range(0, 12_000).Draw(t, "share")),
			SettlementDelayMS: rapid.Int64Range(0, 400_000_000).Draw(t, "settle"), InFlight: rapid.Bool().Draw(t, "inflight"),
			InFlightMS: rapid.Int64Range(0, 400_000_000).Draw(t, "inflightMS"), BridgeDelayed: rapid.Bool().Draw(t, "stuck"),
		}
		base, err := ComputeHaircut(p, f)
		if err != nil {
			t.Fatal(err)
		}
		if base.TotalBPS < 0 || base.TotalBPS > 10_000 {
			t.Fatalf("haircut %d out of range", base.TotalBPS)
		}
		bumps := map[string]func(HaircutFactors) HaircutFactors{
			"volatility": func(g HaircutFactors) HaircutFactors {
				if r := g.Volatility.Rank(); r+1 < len(AllVolatilityClasses()) {
					g.Volatility = AllVolatilityClasses()[r+1]
				}
				return g
			},
			"liquidity": func(g HaircutFactors) HaircutFactors {
				if r := g.Liquidity.Rank(); r+1 < len(AllLiquidityClasses()) {
					g.Liquidity = AllLiquidityClasses()[r+1]
				}
				return g
			},
			"venue": func(g HaircutFactors) HaircutFactors {
				if r := g.Venue.Rank(); r+1 < len(AllVenueTiers()) {
					g.Venue = AllVenueTiers()[r+1]
				}
				return g
			},
			"oracle age": func(g HaircutFactors) HaircutFactors {
				g.OracleAgeMS += rapid.Int64Range(1, 50_000).Draw(t, "dAge")
				return g
			},
			"mark missing": func(g HaircutFactors) HaircutFactors { g.MarkMissing = true; return g },
			"concentration": func(g HaircutFactors) HaircutFactors {
				g.ConcentrationShareBPS += money.BPS(rapid.Int64Range(1, 5_000).Draw(t, "dShare"))
				return g
			},
			"settlement": func(g HaircutFactors) HaircutFactors {
				g.SettlementDelayMS += rapid.Int64Range(1, 1e8).Draw(t, "dSettle")
				return g
			},
			"in flight": func(g HaircutFactors) HaircutFactors { g.InFlight = true; return g },
			"bridge delay": func(g HaircutFactors) HaircutFactors {
				g.InFlightMS += rapid.Int64Range(1, 1e8).Draw(t, "dFlight")
				return g
			},
			"bridge stuck": func(g HaircutFactors) HaircutFactors { g.BridgeDelayed = true; return g },
		}
		for name, bump := range bumps {
			got, err := ComputeHaircut(p, bump(f))
			if err != nil {
				t.Fatal(err)
			}
			if got.TotalBPS < base.TotalBPS {
				t.Fatalf("raising %s lowered the haircut from %d to %d", name, base.TotalBPS, got.TotalBPS)
			}
		}
	})
}

// TestProp_LiquidationIsDeterministicReduceOnlyAndTerminates: for any state,
// the plan is the same every time; every order is a reduce-only sale of a
// risky holding (or numeraire applied to loans), never larger than the
// holding and never above the slippage cap; executing plans step after step
// never increases any risky holding or the gross exposure, and the case
// reaches a stopping decision within its step budget.
func TestProp_LiquidationIsDeterministicReduceOnlyAndTerminates(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(t *rapid.T) {
		s := genState(t, genOpts{modes: AllMarginModes(), freshOnly: rapid.Bool().Draw(t, "freshOnly")})
		fill := money.BPS(rapid.SampledFrom([]int64{10_000, 10_000, 5_000, 1}).Draw(t, "fill"))
		venue := rapid.IntRange(0, 9).Draw(t, "venueDraw") > 0
		atCap := rapid.Bool().Draw(t, "atCap")
		cur := s
		for steps := 0; ; steps++ {
			if steps > p.MaxLiquidationSteps+1 {
				t.Fatalf("the case did not stop within its budget")
			}
			in := PlanInput{State: cur, StepsTaken: steps, VenueAvailable: venue, SlippageAtCap: atCap}
			pl, err := PlanLiquidation(p, in)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			again, err := PlanLiquidation(p, in)
			if err != nil || again.Hash != pl.Hash {
				t.Fatalf("plan is not deterministic")
			}
			if pl.Decision != DecisionSell && pl.Decision != DecisionSellAll && pl.Decision != DecisionApplyCollateral {
				return
			}
			for _, o := range pl.Orders {
				if o.Action == ActionSell {
					i := cur.holding(o.AssetID)
					if i < 0 || cur.Holdings[i].Numeraire || o.Quantity.Cmp(cur.Holdings[i].Quantity) > 0 || !o.Quantity.IsPositive() {
						t.Fatalf("order is not reduce-only: %+v", o)
					}
					if o.SlippageBPS > p.LiquidationSlippageCapBPS {
						t.Fatalf("slippage %d above the cap", o.SlippageBPS)
					}
				}
				if !o.Proceeds.Equal(o.Fee.Add(o.Repay).Add(o.Excess)) {
					t.Fatalf("proceeds do not split exactly")
				}
			}
			before := pl.Before
			next, _, err := ExecuteFirst(p, cur, pl, fill)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			for _, h := range cur.Holdings {
				if h.Numeraire {
					continue
				}
				if j := next.holding(h.AssetID); j >= 0 && next.Holdings[j].Quantity.Cmp(h.Quantity) > 0 {
					t.Fatalf("liquidation increased the %s holding", h.AssetID)
				}
			}
			if after := evalOrFatal(t, p, next); after.GrossExposure.Cmp(before.GrossExposure) > 0 {
				t.Fatalf("liquidation increased gross exposure from %s to %s", before.GrossExposure, after.GrossExposure)
			}
			if next.LoanTotal().Cmp(cur.LoanTotal()) > 0 {
				t.Fatalf("liquidation increased the loans")
			}
			cur = next
		}
	})
}
