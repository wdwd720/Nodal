package risk

import (
	"sort"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
)

func genUSD(rt *rapid.T, label string, maxMinor int64) money.USD {
	return money.USDFromMinor(rapid.Int64Range(0, maxMinor).Draw(rt, label))
}

func genOptUSD(rt *rapid.T, label string, maxMinor int64) *money.USD {
	if rapid.Bool().Draw(rt, label+"_nil") {
		return nil
	}
	v := genUSD(rt, label, maxMinor)
	return &v
}

func genOptBPS(rt *rapid.T, label string) *money.BPS {
	if rapid.Bool().Draw(rt, label+"_nil") {
		return nil
	}
	v := money.BPS(rapid.Int64Range(0, 10_000).Draw(rt, label))
	return &v
}

func genOptInt(rt *rapid.T, label string, max int) *int {
	if rapid.Bool().Draw(rt, label+"_nil") {
		return nil
	}
	v := rapid.IntRange(0, max).Draw(rt, label)
	return &v
}

func genOptInt64(rt *rapid.T, label string, max int64) *int64 {
	if rapid.Bool().Draw(rt, label+"_nil") {
		return nil
	}
	v := rapid.Int64Range(0, max).Draw(rt, label)
	return &v
}

func genOptBool(rt *rapid.T, label string) *bool {
	if rapid.Bool().Draw(rt, label+"_nil") {
		return nil
	}
	v := rapid.Bool().Draw(rt, label)
	return &v
}

func genOptList(rt *rapid.T, label string, universe []string) []string {
	if rapid.Bool().Draw(rt, label+"_nil") {
		return nil
	}
	return rapid.SliceOfN(rapid.SampledFrom(universe), 0, len(universe)).Draw(rt, label)
}

// genPolicy draws a partial policy; complete=true forces every limit set.
func genPolicy(rt *rapid.T, label string, complete bool) Policy {
	p := Policy{
		Version:                           label,
		MaxSingleTradeUSD:                 genOptUSD(rt, label+"_single", 1_000_000_00),
		MaxPositionUSD:                    genOptUSD(rt, label+"_pos", 1_000_000_00),
		MaxTotalExposureUSD:               genOptUSD(rt, label+"_total", 10_000_000_00),
		MaxConcentrationBPS:               genOptBPS(rt, label+"_conc"),
		MaxAssetClassConcentrationBPS:     genOptBPS(rt, label+"_cconc"),
		MaxNativeMarketConcentrationBPS:   genOptBPS(rt, label+"_nmconc"),
		MaxCreatorConcentrationBPS:        genOptBPS(rt, label+"_crconc"),
		MaxDailyLossUSD:                   genOptUSD(rt, label+"_loss", 100_000_00),
		MaxDrawdownUSD:                    genOptUSD(rt, label+"_dd", 100_000_00),
		MaxOrdersPerHour:                  genOptInt(rt, label+"_orders", 100),
		MaxSlippageBPS:                    genOptBPS(rt, label+"_slip"),
		MaxFeeBPS:                         genOptBPS(rt, label+"_fee"),
		MaxPriceImpactBPS:                 genOptBPS(rt, label+"_impact"),
		MaxQuoteAgeMS:                     genOptInt64(rt, label+"_qage", 60_000),
		MinLiquidityUSD:                   genOptUSD(rt, label+"_liq", 10_000_000_00),
		MaxDataAgeMS:                      map[string]int64{},
		AllowedVenues:                     genOptList(rt, label+"_venues", []string{"JUPITER", "RAYDIUM", "ORCA"}),
		AllowedVenueStatuses:              genOptList(rt, label+"_vstat", venueStatuses),
		AllowedAssetRiskClasses:           genOptList(rt, label+"_classes", riskClasses),
		AllowedProviderHealth:             genOptList(rt, label+"_health", providerHealths),
		AllowRiskReductionDuringKill:      genOptBool(rt, label+"_allowkill"),
		BlockRiskReductionOnAccountFreeze: genOptBool(rt, label+"_blockfreeze"),
	}
	for _, kind := range []string{"price", "wallet_event", "social"} {
		if rapid.Bool().Draw(rt, label+"_dage_"+kind) {
			p.MaxDataAgeMS[kind] = rapid.Int64Range(1, 100_000).Draw(rt, label+"_dagev_"+kind)
		}
	}
	if complete {
		fill := func(u **money.USD, v int64) {
			if *u == nil {
				x := money.USDFromMinor(v)
				*u = &x
			}
		}
		fill(&p.MaxSingleTradeUSD, 100_000_00)
		fill(&p.MaxPositionUSD, 500_000_00)
		fill(&p.MaxTotalExposureUSD, 1_000_000_00)
		fill(&p.MaxDailyLossUSD, 50_000_00)
		fill(&p.MaxDrawdownUSD, 100_000_00)
		fill(&p.MinLiquidityUSD, 0)
		bps := func(b **money.BPS, v money.BPS) {
			if *b == nil {
				x := v
				*b = &x
			}
		}
		bps(&p.MaxConcentrationBPS, 10_000)
		bps(&p.MaxAssetClassConcentrationBPS, 10_000)
		bps(&p.MaxNativeMarketConcentrationBPS, 10_000)
		bps(&p.MaxCreatorConcentrationBPS, 10_000)
		bps(&p.MaxSlippageBPS, 10_000)
		bps(&p.MaxFeeBPS, 10_000)
		bps(&p.MaxPriceImpactBPS, 10_000)
		if p.MaxOrdersPerHour == nil {
			v := 100
			p.MaxOrdersPerHour = &v
		}
		if p.MaxQuoteAgeMS == nil {
			v := int64(60_000)
			p.MaxQuoteAgeMS = &v
		}
		if p.AllowedVenues == nil {
			p.AllowedVenues = []string{"JUPITER", "RAYDIUM", "ORCA"}
		}
		if p.AllowedVenueStatuses == nil {
			p.AllowedVenueStatuses = append([]string(nil), venueStatuses...)
		}
		if p.AllowedAssetRiskClasses == nil {
			p.AllowedAssetRiskClasses = append([]string(nil), riskClasses...)
		}
		if p.AllowedProviderHealth == nil {
			p.AllowedProviderHealth = append([]string(nil), providerHealths...)
		}
		if p.AllowRiskReductionDuringKill == nil {
			v := true
			p.AllowRiskReductionDuringKill = &v
		}
		if p.BlockRiskReductionOnAccountFreeze == nil {
			v := false
			p.BlockRiskReductionOnAccountFreeze = &v
		}
	}
	p.normalize()
	return p
}

func genInput(rt *rapid.T) Input {
	pick := func(label string, opts ...string) string { return rapid.SampledFrom(opts).Draw(rt, label) }
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	if rapid.Bool().Draw(rt, "zero_now") {
		now = time.Time{}
	}
	inst := pick("instrument", "i1", "i2", "")
	in := Input{
		Stage: Stage(pick("stage", "PRE_TRADE", "FINAL", "CONTINUOUS", "BOGUS")),
		Intent: Intent{
			ID:                pick("intent_id", "019917c0-0000-7000-8000-000000000001", ""),
			AccountID:         pick("account", "acct-1", "acct-2", ""),
			AgentID:           pick("agent", "agent-1", ""),
			StrategyVersionID: pick("sv", "sv-1", ""),
			ModelID:           pick("model", "m-1", ""),
			Action:            Action(pick("action", "ACQUIRE_NOTIONAL", "REDUCE_NOTIONAL", "CLOSE_POSITION", "TARGET_EXPOSURE", "BOGUS")),
			NotionalUSD:       genUSD(rt, "notional", 5_000_00),
			TargetExposureUSD: genUSD(rt, "target", 5_000_00),
			InstrumentID:      inst,
			AssetClass:        pick("asset_class", "CRYPTO_SPOT", "OTHER"),
			AssetRiskClass:    assets.RiskClass(pick("risk_class", "SETTLEMENT", "MAJOR", "STANDARD", "SPECULATIVE", "UNSUPPORTED", "")),
			AssetStatus:       assets.Status(pick("asset_status", "ACTIVE", "CLOSE_ONLY", "RESTRICTED", "HALTED", "DELISTING", "DELISTED", "")),
			Venue:             pick("venue", "JUPITER", "RAYDIUM", "ORCA", ""),
			Chain:             pick("chain", "solana", ""),
			Provider:          pick("provider", "JUPITER_API", "HELIUS", ""),
			Constraints: IntentConstraints{
				MaxSlippageBPS:    money.BPS(rapid.Int64Range(0, 500).Draw(rt, "c_slip")),
				MaxFeeBPS:         money.BPS(rapid.Int64Range(0, 500).Draw(rt, "c_fee")),
				MaxPriceImpactBPS: money.BPS(rapid.Int64Range(0, 500).Draw(rt, "c_impact")),
			},
		},
		Account: AccountSnapshot{
			Status:                       accounts.Status(pick("acct_status", "ACTIVE", "RESTRICTED", "FROZEN", "CLOSED", "")),
			PortfolioValueUSD:            genUSD(rt, "portfolio", 50_000_00),
			AvailableNowUSD:              genUSD(rt, "available", 50_000_00),
			AsOf:                         now,
			PositionsUSD:                 map[string]money.USD{"i1": genUSD(rt, "pos1", 10_000_00), "i2": genUSD(rt, "pos2", 10_000_00)},
			AssetClassExposureUSD:        map[string]money.USD{"CRYPTO_SPOT": genUSD(rt, "cexp", 20_000_00)},
			TotalExposureUSD:             genUSD(rt, "total", 20_000_00),
			DailyRealizedLossUSD:         genUSD(rt, "loss", 1_000_00),
			DrawdownUSD:                  genUSD(rt, "dd", 2_000_00),
			OrdersInWindow:               rapid.IntRange(0, 40).Draw(rt, "orders"),
			UnresolvedMaterialMismatches: rapid.IntRange(0, 2).Draw(rt, "mismatches"),
		},
		Market: MarketSnapshot{
			VenueStatus:    pick("venue_status", "ACTIVE", "DEGRADED", "DISABLED", ""),
			ProviderHealth: map[string]string{},
			DataFreshness:  map[string]DataAge{},
		},
		Now: now,
	}
	if rapid.Bool().Draw(rt, "quote") {
		in.Market.Quote = &Quote{
			ID:             pick("quote_id", "019917c0-0000-7000-8000-000000000201", ""),
			Provider:       pick("quote_provider", "JUPITER_API", "HELIUS", ""),
			PriceImpactBPS: money.BPS(rapid.Int64Range(0, 300).Draw(rt, "q_impact")),
			SlippageBPS:    money.BPS(rapid.Int64Range(0, 300).Draw(rt, "q_slip")),
			FeeBPS:         money.BPS(rapid.Int64Range(0, 300).Draw(rt, "q_fee")),
			ReceivedAt:     now.Add(-time.Duration(rapid.Int64Range(-1000, 10_000).Draw(rt, "q_age")) * time.Millisecond),
			ExpiresAt:      now.Add(time.Duration(rapid.Int64Range(-10, 60).Draw(rt, "q_exp")) * time.Second),
		}
	}
	if rapid.Bool().Draw(rt, "liquidity") {
		l := genUSD(rt, "liq", 1_000_000_00)
		in.Market.LiquidityUSD = &l
	}
	for _, prov := range []string{"JUPITER_API", "HELIUS"} {
		if h := pick("health_"+prov, "HEALTHY", "DEGRADED", "UNHEALTHY", "DISABLED", "BOGUS", ""); h != "" {
			in.Market.ProviderHealth[prov] = h
		}
	}
	for _, kind := range []string{"price", "wallet_event", "social"} {
		if rapid.Bool().Draw(rt, "fresh_"+kind) {
			in.Market.DataFreshness[kind] = DataAge{
				AgeMS:            rapid.Int64Range(-10, 5000).Draw(rt, "age_"+kind),
				DeclaredMaxAgeMS: rapid.Int64Range(0, 3000).Draw(rt, "decl_"+kind),
			}
		}
	}
	if rapid.Bool().Draw(rt, "envelope") {
		in.Envelope = &EnvelopeSnapshot{
			ID:                  "env-1",
			Status:              pick("env_status", "ACTIVE", "PAUSED", "EXHAUSTED", "REVOKED"),
			AvailableUSD:        genUSD(rt, "env_avail", 5_000_00),
			MaxSingleTradeUSD:   genUSD(rt, "env_single", 5_000_00),
			MaxPositionUSD:      genUSD(rt, "env_pos", 20_000_00),
			MaxDailyLossUSD:     genUSD(rt, "env_maxloss", 1_000_00),
			MaxDrawdownUSD:      genUSD(rt, "env_maxdd", 1_000_00),
			DailyLossUSD:        genUSD(rt, "env_loss", 1_000_00),
			DrawdownUSD:         genUSD(rt, "env_dd", 1_000_00),
			AllowedInstruments:  rapid.SliceOfN(rapid.SampledFrom([]string{"i1", "i2"}), 0, 2).Draw(rt, "env_inst"),
			AllowedAssetClasses: rapid.SliceOfN(rapid.SampledFrom([]string{"CRYPTO_SPOT", "OTHER"}), 0, 2).Draw(rt, "env_classes"),
			AllowedVenues:       rapid.SliceOfN(rapid.SampledFrom([]string{"JUPITER", "RAYDIUM"}), 0, 2).Draw(rt, "env_venues"),
			MaxOrderRatePerHour: rapid.IntRange(0, 40).Draw(rt, "env_rate"),
		}
	}
	kinds := []string{
		KillGlobalNewRisk, KillAccountFreeze, KillAgentPause, KillStrategyVersionDisable, KillVenueDisable,
		KillInstrumentCloseOnly, KillInstrumentHalt, KillChainDisable, KillProviderDisable, KillFundingDisable,
		KillWithdrawalsDisable, KillModelDisable, "UNKNOWN",
	}
	n := rapid.IntRange(0, 4).Draw(rt, "n_switches")
	for i := 0; i < n; i++ {
		in.KillSwitches = append(in.KillSwitches, KillSwitch{
			Kind:  rapid.SampledFrom(kinds).Draw(rt, "ks_kind"),
			Scope: pick("ks_scope", "*", "acct-1", "agent-1", "sv-1", "m-1", "JUPITER", "i1", "solana", "JUPITER_API", "other"),
		})
	}
	return in
}

// TestProp_ReasonCodesSorted: for any policy and input the reason codes are
// sorted, unique and known; ALLOW is exactly "no reason codes"; the hash is a
// pure function of (policy, input).
func TestProp_ReasonCodesSorted(t *testing.T) {
	known := map[string]bool{}
	for _, c := range ReasonCodes() {
		known[c] = true
	}
	rapid.Check(t, func(rt *rapid.T) {
		p := genPolicy(rt, "g", rapid.Bool().Draw(rt, "complete"))
		if rapid.Bool().Draw(rt, "missing_policy") {
			p = Policy{}
		}
		in := genInput(rt)
		d := Evaluate(p, in)
		if !sort.StringsAreSorted(d.ReasonCodes) {
			rt.Fatalf("not sorted: %v", d.ReasonCodes)
		}
		for i := 1; i < len(d.ReasonCodes); i++ {
			if d.ReasonCodes[i] == d.ReasonCodes[i-1] {
				rt.Fatalf("duplicate %s", d.ReasonCodes[i])
			}
		}
		for _, c := range d.ReasonCodes {
			if !known[c] {
				rt.Fatalf("unknown code %s", c)
			}
		}
		if (d.Verdict == Allow) != (len(d.ReasonCodes) == 0) {
			rt.Fatalf("verdict %s with codes %v", d.Verdict, d.ReasonCodes)
		}
		if (p.Missing() || len(p.MissingLimits()) > 0) && !containsCode(d.ReasonCodes, ReasonPolicyMissing) {
			rt.Fatalf("incomplete policy must yield %s: %v", ReasonPolicyMissing, d.ReasonCodes)
		}
		if d.Hash != d.ComputeHash() || d.Hash != Evaluate(p, in).Hash {
			rt.Fatal("hash is not a pure function of (policy, input)")
		}
		if containsCode(d.ReasonCodes, ReasonKillSwitch) != (len(d.MatchedKillSwitches) > 0) {
			rt.Fatalf("kill switch code and matched list disagree: %v / %v", d.ReasonCodes, d.MatchedKillSwitches)
		}
	})
}

// TestProp_ComposeStrictest: the composed policy is never looser than any
// layer that sets a limit, allowlists are subsets of every layer that sets
// one, and composition is idempotent.
func TestProp_ComposeStrictest(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		g := genPolicy(rt, "g", true)
		a := genPolicy(rt, "a", false)
		ag := genPolicy(rt, "ag", false)
		var ap, agp *Policy
		if rapid.Bool().Draw(rt, "has_account") {
			ap = &a
		}
		if rapid.Bool().Draw(rt, "has_agent") {
			agp = &ag
		}
		c := Compose(&g, ap, agp)
		if c.Missing() || len(c.MissingLimits()) > 0 {
			rt.Fatalf("complete GLOBAL composed into an incomplete policy: %v", c.MissingLimits())
		}
		layers := []*Policy{&g, ap, agp}
		for _, l := range layers {
			if l == nil {
				continue
			}
			leUSD := func(name string, got, limit *money.USD) {
				if limit != nil && got.Cmp(*limit) > 0 {
					rt.Fatalf("%s: composed %s looser than layer %s", name, got, limit)
				}
			}
			leUSD("single", c.MaxSingleTradeUSD, l.MaxSingleTradeUSD)
			leUSD("position", c.MaxPositionUSD, l.MaxPositionUSD)
			leUSD("total", c.MaxTotalExposureUSD, l.MaxTotalExposureUSD)
			leUSD("loss", c.MaxDailyLossUSD, l.MaxDailyLossUSD)
			leUSD("drawdown", c.MaxDrawdownUSD, l.MaxDrawdownUSD)
			if l.MinLiquidityUSD != nil && c.MinLiquidityUSD.Cmp(*l.MinLiquidityUSD) < 0 {
				rt.Fatalf("liquidity: composed %s below layer %s", c.MinLiquidityUSD, l.MinLiquidityUSD)
			}
			leBPS := func(name string, got, limit *money.BPS) {
				if limit != nil && *got > *limit {
					rt.Fatalf("%s: composed %d looser than layer %d", name, *got, *limit)
				}
			}
			leBPS("conc", c.MaxConcentrationBPS, l.MaxConcentrationBPS)
			leBPS("cconc", c.MaxAssetClassConcentrationBPS, l.MaxAssetClassConcentrationBPS)
			leBPS("nmconc", c.MaxNativeMarketConcentrationBPS, l.MaxNativeMarketConcentrationBPS)
			leBPS("crconc", c.MaxCreatorConcentrationBPS, l.MaxCreatorConcentrationBPS)
			leBPS("slip", c.MaxSlippageBPS, l.MaxSlippageBPS)
			leBPS("fee", c.MaxFeeBPS, l.MaxFeeBPS)
			leBPS("impact", c.MaxPriceImpactBPS, l.MaxPriceImpactBPS)
			if l.MaxOrdersPerHour != nil && *c.MaxOrdersPerHour > *l.MaxOrdersPerHour {
				rt.Fatal("orders looser")
			}
			if l.MaxQuoteAgeMS != nil && *c.MaxQuoteAgeMS > *l.MaxQuoteAgeMS {
				rt.Fatal("quote age looser")
			}
			for k, v := range l.MaxDataAgeMS {
				if got, ok := c.MaxDataAgeMS[k]; !ok || got > v {
					rt.Fatalf("data age %s looser", k)
				}
			}
			subset := func(name string, got, layer []string) {
				if layer == nil {
					return
				}
				for _, s := range got {
					if !contains(layer, s) {
						rt.Fatalf("%s: %q not in layer %v", name, s, layer)
					}
				}
			}
			subset("venues", c.AllowedVenues, l.AllowedVenues)
			subset("vstat", c.AllowedVenueStatuses, l.AllowedVenueStatuses)
			subset("classes", c.AllowedAssetRiskClasses, l.AllowedAssetRiskClasses)
			subset("health", c.AllowedProviderHealth, l.AllowedProviderHealth)
			if l.AllowRiskReductionDuringKill != nil && !*l.AllowRiskReductionDuringKill && *c.AllowRiskReductionDuringKill {
				rt.Fatal("allow_risk_reduction_during_kill loosened")
			}
			if l.BlockRiskReductionOnAccountFreeze != nil && *l.BlockRiskReductionOnAccountFreeze && !*c.BlockRiskReductionOnAccountFreeze {
				rt.Fatal("block_risk_reduction_on_account_freeze loosened")
			}
		}
		again := Compose(&c, nil, nil)
		if again.Hash() != c.Hash() {
			rt.Fatal("compose is not idempotent")
		}
		if Compose(&g, agp, ap).Hash() != c.Hash() {
			rt.Fatal("compose is not symmetric in the refining layers")
		}
	})
}

// TestProp_ConstraintsNeverLooserThanPolicy: whatever the verdict, the
// resulting constraints never exceed the policy bounds or the intent's own.
func TestProp_ConstraintsNeverLooserThanPolicy(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		p := genPolicy(rt, "g", true)
		in := genInput(rt)
		d := Evaluate(p, in)
		c := d.Constraints
		if c.MaxSlippageBPS > *p.MaxSlippageBPS || c.MaxFeeBPS > *p.MaxFeeBPS || c.MaxPriceImpactBPS > *p.MaxPriceImpactBPS {
			rt.Fatalf("constraints looser than policy: %+v", c)
		}
		if ic := in.Intent.Constraints; (ic.MaxSlippageBPS > 0 && c.MaxSlippageBPS > ic.MaxSlippageBPS) ||
			(ic.MaxFeeBPS > 0 && c.MaxFeeBPS > ic.MaxFeeBPS) || (ic.MaxPriceImpactBPS > 0 && c.MaxPriceImpactBPS > ic.MaxPriceImpactBPS) {
			rt.Fatalf("constraints looser than the intent: %+v vs %+v", c, ic)
		}
		if c.MaxNotionalUSD.IsNegative() {
			rt.Fatal("negative max notional")
		}
		if d.ActionClass == ClassNewRisk && c.MaxNotionalUSD.Cmp(*p.MaxSingleTradeUSD) > 0 {
			rt.Fatal("max notional above max single trade")
		}
		if d.ActionClass == ClassNewRisk && c.MaxNotionalUSD.Cmp(in.Account.AvailableNowUSD) > 0 {
			rt.Fatal("max notional above available buying power")
		}
	})
}

// TestProp_NewRiskNeverAllowedUnderAnyMatchingKill: with a GLOBAL kill active
// no NEW_RISK intent is ever allowed, whatever the policy flags.
func TestProp_NewRiskNeverAllowedUnderGlobalKill(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		p := genPolicy(rt, "g", true)
		in := genInput(rt)
		in.Stage = Stage(rapid.SampledFrom([]string{"PRE_TRADE", "FINAL"}).Draw(rt, "stage"))
		in.KillSwitches = append(in.KillSwitches, KillSwitch{Kind: KillGlobalNewRisk, Scope: "*"})
		d := Evaluate(p, in)
		if d.ActionClass == ClassNewRisk && d.Verdict == Allow {
			rt.Fatal("NEW_RISK allowed under GLOBAL_NEW_RISK_KILL")
		}
		if d.ActionClass == ClassNewRisk && !containsCode(d.ReasonCodes, ReasonKillSwitch) {
			rt.Fatalf("missing %s: %v", ReasonKillSwitch, d.ReasonCodes)
		}
	})
}
