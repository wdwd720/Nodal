package delegation

import (
	"math/big"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

// TestProp_QuoteRoundsAgainstTheAgentAndIsDeterministic: whatever the
// amount, decimals, prices, spread and bound, a quote never pays more than
// the mark, never accepts less than its own bound, is the exact floor of its
// rational, and is the same on every call.
func TestProp_QuoteRoundsAgainstTheAgentAndIsDeterministic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := QuoteInput{
			AmountIn:        money.QuantityFromInt64(rapid.Int64Range(1, 1_000_000_000_000).Draw(t, "amount")),
			PayDecimals:     uint8(rapid.IntRange(0, 12).Draw(t, "pd")),
			ReceiveDecimals: uint8(rapid.IntRange(0, 12).Draw(t, "rd")),
			PayPrice:        price(rapid.Int64Range(1, 1_000_000_000).Draw(t, "pm"), int32(rapid.IntRange(0, 9).Draw(t, "ps"))),
			ReceivePrice:    price(rapid.Int64Range(1, 1_000_000_000).Draw(t, "rm"), int32(rapid.IntRange(0, 9).Draw(t, "rs"))),
			SpreadBPS:       money.BPS(rapid.IntRange(0, 1000).Draw(t, "spread")),
		}
		in.MaxSlippageBPS = money.BPS(rapid.IntRange(0, 10_000).Draw(t, "slip"))
		q, err := ComputeQuote(in)
		again, err2 := ComputeQuote(in)
		if (err == nil) != (err2 == nil) {
			t.Fatalf("non-deterministic refusal")
		}
		if err != nil {
			return
		}
		if !q.AmountOut.Equal(again.AmountOut) || !q.MinAmountOut.Equal(again.MinAmountOut) || q.NotionalUSD != again.NotionalUSD {
			t.Fatalf("non-deterministic quote")
		}
		if q.AmountOut.Cmp(q.MarkOut) > 0 {
			t.Fatalf("pays %s above the mark %s", q.AmountOut, q.MarkOut)
		}
		if q.AmountOut.Cmp(q.MinAmountOut) < 0 {
			t.Fatalf("accepted %s below its bound %s", q.AmountOut, q.MinAmountOut)
		}
		// AmountOut is the floor: out x den x 10000 <= num x (10000-spread) < (out+1) x den x 10000.
		num := new(big.Int).Mul(in.AmountIn.BigInt(), in.PayPrice.Mantissa.BigInt())
		num.Mul(num, pow10(int(in.ReceiveDecimals)+int(in.ReceivePrice.Scale)))
		num.Mul(num, big.NewInt(int64(money.OneHundredPercent-in.SpreadBPS)))
		den := new(big.Int).Mul(in.ReceivePrice.Mantissa.BigInt(), pow10(int(in.PayDecimals)+int(in.PayPrice.Scale)))
		den.Mul(den, big.NewInt(int64(money.OneHundredPercent)))
		lo := new(big.Int).Mul(q.AmountOut.BigInt(), den)
		hi := new(big.Int).Mul(new(big.Int).Add(q.AmountOut.BigInt(), big.NewInt(1)), den)
		if lo.Cmp(num) > 0 || hi.Cmp(num) <= 0 {
			t.Fatalf("amount out %s is not the floor", q.AmountOut)
		}
		if !q.NotionalUSD.IsPositive() {
			t.Fatalf("a quoted swap has a positive notional")
		}
	})
}

// TestProp_CapabilityLimitsNeverOverspend: admitting uses only when the
// capability covers them, in any order and on any days, never lets the sum
// exceed a single, daily or lifetime limit.
func TestProp_CapabilityLimitsNeverOverspend(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := unitCapability(t)
		single := rapid.Int64Range(1, 1_000).Draw(t, "single")
		daily := single + rapid.Int64Range(0, 3_000).Draw(t, "daily")
		total := daily + rapid.Int64Range(0, 6_000).Draw(t, "total")
		c.Terms.MaxSingleTrade, c.Terms.MaxDailyNotional, c.Terms.MaxNotional =
			money.USDFromMinor(single), money.USDFromMinor(daily), money.USDFromMinor(total)
		h, err := c.ComputeContentHash()
		if err != nil {
			t.Fatal(err)
		}
		c.ContentHash = h
		dayUsed := map[int]int64{}
		var all int64
		n := rapid.IntRange(1, 60).Draw(t, "uses")
		for i := 0; i < n; i++ {
			day := rapid.IntRange(0, 3).Draw(t, "day")
			amount := rapid.Int64Range(1, 2*single).Draw(t, "amount")
			u := unitUse(c)
			u.Now = c.GrantedAt.Add(time.Duration(day)*time.Hour + time.Minute)
			u.Notional, u.DayUsed, u.TotalUsed = money.USDFromMinor(amount), money.USDFromMinor(dayUsed[day]), money.USDFromMinor(all)
			if c.Check(u).OK() {
				dayUsed[day] += amount
				all += amount
				if amount > single || dayUsed[day] > daily || all > total {
					t.Fatalf("admitted beyond a limit: amount %d day %d total %d", amount, dayUsed[day], all)
				}
			}
		}
	})
}

// TestProp_CapabilityHashIsAFunctionOfWhatWasGranted: shuffling and
// duplicating the lists of a grant never changes its hash; changing a limit
// always does.
func TestProp_CapabilityHashIsAFunctionOfWhatWasGranted(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := unitCapability(t)
		k := c
		k.Terms.ActionClasses = rapid.Permutation(append(append([]ActionClass(nil), c.Terms.ActionClasses...), c.Terms.ActionClasses[0])).Draw(t, "actions")
		k.Terms.DestinationAllowlist = rapid.Permutation(append([]devnet.PublicKey(nil), c.Terms.DestinationAllowlist...)).Draw(t, "dests")
		h, err := k.ComputeContentHash()
		if err != nil || h != c.ContentHash {
			t.Fatalf("an equivalent grant hashes differently")
		}
		delta := rapid.Int64Range(1, 1_000).Draw(t, "delta")
		k.Terms.MaxNotional = money.USDFromMinor(c.Terms.MaxNotional.Minor() + delta)
		if h2, _ := k.ComputeContentHash(); h2 == c.ContentHash {
			t.Fatalf("a wider grant hashes the same")
		}
	})
}

// TestProp_TheInspectorRefusesEveryMutation: changing any amount, owner or
// mint of either leg of a planned swap is refused.
func TestProp_TheInspectorRefusesEveryMutation(t *testing.T) {
	e := unitSwapExpectation(t)
	base, err := e.Plan.Message(e.Blockhash)
	if err != nil {
		t.Fatal(err)
	}
	rapid.Check(t, func(t *rapid.T) {
		leg := rapid.IntRange(0, 1).Draw(t, "leg")
		field := rapid.IntRange(0, 3).Draw(t, "field")
		m := base
		m.Instructions = append([]devnet.Instruction(nil), base.Instructions...)
		tr := m.Instructions[leg].(devnet.TokenTransfer)
		before := tr
		switch field {
		case 0:
			tr.Amount = rapid.Uint64().Draw(t, "amount")
		case 1:
			tr.Mint = ukey(rapid.StringMatching(`[a-z]{1,8}`).Draw(t, "mint"))
		case 2:
			tr.FromOwner = ukey(rapid.StringMatching(`[a-z]{1,8}`).Draw(t, "from"))
		case 3:
			tr.ToOwner = ukey(rapid.StringMatching(`[a-z]{1,8}`).Draw(t, "to"))
		}
		m.Instructions[leg] = tr
		codes := InspectSwapMessage(e, m)
		if tr == before {
			if len(codes) != 0 {
				t.Fatalf("the unchanged plan is refused: %v", codes)
			}
			return
		}
		if len(codes) == 0 {
			t.Fatalf("a modified transaction passed inspection: leg %d field %d", leg, field)
		}
	})
}

// TestProp_EvaluationIsDeterministicAndAutoApprovesOnlyLowRisk: whatever the
// facts, the evaluation is the same on every call, its score is 0..100, and
// an auto-approval only ever happens in a test environment, at or below the
// auto limit, with no anomaly and every auto bound met.
func TestProp_EvaluationIsDeterministicAndAutoApprovesOnlyLowRisk(t *testing.T) {
	envs := Environments()
	anomalies := []Anomaly{AnomalyUnresolvedEffects, AnomalyRecentDecline, AnomalyAmountSpike}
	rapid.Check(t, func(t *rapid.T) {
		p := unitPolicy(t)
		p.AutoApproveEnabled = rapid.Bool().Draw(t, "auto")
		in := EvaluationInput{
			Environment: rapid.SampledFrom(envs).Draw(t, "env"), Now: unitNow,
			Amount:             money.USDFromMinor(rapid.Int64Range(1, 120_000).Draw(t, "amount")),
			DestinationActive:  rapid.Bool().Draw(t, "dest"),
			DestinationAddedAt: unitNow.Add(-time.Duration(rapid.Int64Range(0, 7200).Draw(t, "dage")) * time.Second),
			AccountCreatedAt:   unitNow.Add(-time.Duration(rapid.Int64Range(-3600, 3600).Draw(t, "aage")) * time.Second),
			IdentityVerified:   rapid.Bool().Draw(t, "id"), SanctionsClear: rapid.Bool().Draw(t, "sanctions"),
			DayTotal:     money.USDFromMinor(rapid.Int64Range(0, 120_000).Draw(t, "day")),
			CountLast24h: rapid.IntRange(0, 12).Draw(t, "count"),
			AgentInScope: rapid.Bool().Draw(t, "scope"), AgentWithinLimits: rapid.Bool().Draw(t, "limits"),
		}
		for _, a := range anomalies {
			if rapid.Bool().Draw(t, "anomaly-"+string(a)) {
				in.Anomalies = append(in.Anomalies, a)
			}
		}
		ev, again := Evaluate(p, in), Evaluate(p, in)
		if ev.Outcome != again.Outcome || ev.RiskScore != again.RiskScore || len(ev.ReasonCodes) != len(again.ReasonCodes) {
			t.Fatalf("non-deterministic evaluation")
		}
		if ev.RiskScore < 0 || ev.RiskScore > 100 {
			t.Fatalf("risk score %d out of range", ev.RiskScore)
		}
		switch ev.Outcome {
		case OutcomeAutoApprove:
			if !contains(TestEnvironments(), in.Environment) || !p.AutoApproveEnabled || in.Amount.Cmp(p.AutoApproveMax) > 0 ||
				len(in.Anomalies) > 0 || ev.RiskScore > p.AutoApproveMaxRiskScore || !in.DestinationActive ||
				!in.IdentityVerified || !in.SanctionsClear || !in.AgentInScope || !in.AgentWithinLimits ||
				in.Now.Sub(in.DestinationAddedAt) < p.MinDestinationAge {
				t.Fatalf("auto-approved outside low-risk test mode: %+v", ev)
			}
		case OutcomeReject, OutcomeHumanRequired:
			if len(ev.ReasonCodes) == 0 {
				t.Fatalf("%s without a reason", ev.Outcome)
			}
		default:
			t.Fatalf("unknown outcome %q", ev.Outcome)
		}
	})
}

// TestProp_DelegatedKeysAreAFunctionOfSeedAccountAndChain: a second signer
// built from the same seed derives the same key for the same account and
// chain (the key is a function of its inputs, not of a signer's state), and
// another account or another seed never derives that key.
func TestProp_DelegatedKeysAreAFunctionOfSeedAccountAndChain(t *testing.T) {
	s := unitSigner(t, useed("prop master"))
	again := unitSigner(t, useed("prop master"))
	other := unitSigner(t, useed("another master"))
	rapid.Check(t, func(t *rapid.T) {
		a := accounts.AccountID(id.New[id.Any]())
		b := accounts.AccountID(id.New[id.Any]())
		chain := rapid.SampledFrom(Chains()).Draw(t, "chain")
		key := s.PublicKey(a, chain)
		if again.PublicKey(a, chain) != key {
			t.Fatalf("two signers from one seed derive different keys for one account and chain")
		}
		if s.PublicKey(b, chain) == key {
			t.Fatalf("two accounts share a key")
		}
		if other.PublicKey(a, chain) == key {
			t.Fatalf("two seeds derive the same key for one account and chain")
		}
	})
	_ = config.EnvTest
}
