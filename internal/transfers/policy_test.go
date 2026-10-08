package transfers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func TestPolicy_DefaultIsValidAndItsHashIsStable(t *testing.T) {
	p := DefaultPolicy()
	require.NoError(t, p.Validate())
	h1, err := p.Hash()
	require.NoError(t, err)
	h2, err := DefaultPolicy().Hash()
	require.NoError(t, err)
	assert.Equal(t, h1, h2)
	assert.Len(t, h1, 64)
	q := p
	q.MaxDaily = money.USDFromMinor(p.MaxDaily.Minor() + 1)
	h3, err := q.Hash()
	require.NoError(t, err)
	assert.NotEqual(t, h1, h3, "any limit change is a different policy")
}

func TestPolicy_ValidateRefusesAnUnenforceablePolicy(t *testing.T) {
	cases := map[string]func(*Policy){
		"no version":          func(p *Policy) { p.Version = "" },
		"padded version":      func(p *Policy) { p.Version = " v1" },
		"zero per transfer":   func(p *Policy) { p.MaxPerTransfer = money.USD{} },
		"negative daily":      func(p *Policy) { p.MaxDaily = money.USDFromMinor(-1) },
		"zero velocity value": func(p *Policy) { p.VelocityMaxNotional = money.USD{} },
		"zero velocity count": func(p *Policy) { p.VelocityMaxCount = 0 },
		"zero daily window":   func(p *Policy) { p.DailyWindowMS = 0 },
		"zero step-up age":    func(p *Policy) { p.StepUpMaxAgeMS = 0 },
		"negative step-up":    func(p *Policy) { p.StepUpAbove = money.USDFromMinor(-1) },
		"memo too long":       func(p *Policy) { p.MemoMaxRunes = MaxMemoRunes + 1 },
		"memo zero":           func(p *Policy) { p.MemoMaxRunes = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := DefaultPolicy()
			mutate(&p)
			err := p.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

func usd(minor int64) money.USD { return money.USDFromMinor(minor) }

func TestPolicy_EvaluateNamesEveryViolatedRuleInPrecedenceOrder(t *testing.T) {
	p := DefaultPolicy()
	ok := PolicyInput{Notional: usd(100_00)}
	assert.Empty(t, p.Evaluate(ok).Codes)
	assert.True(t, p.Evaluate(ok).Allowlisted)

	for name, c := range map[string]struct {
		in   PolicyInput
		want []string
	}{
		"blocked":           {PolicyInput{Notional: usd(1), Rule: RuleBlock}, []string{RejectRecipientBlocked}},
		"allowlist only":    {PolicyInput{Notional: usd(1), SenderMode: SenderAllowlistOnly}, []string{RejectRecipientNotAllowed}},
		"allowlisted":       {PolicyInput{Notional: usd(1), SenderMode: SenderAllowlistOnly, Rule: RuleAllow}, nil},
		"block beats allow": {PolicyInput{Notional: usd(1), SenderMode: SenderAllowlistOnly, Rule: RuleBlock}, []string{RejectRecipientBlocked}},
		"unknown rule":      {PolicyInput{Notional: usd(1), Rule: "MAYBE"}, []string{RejectRecipientBlocked}},
		"unknown mode":      {PolicyInput{Notional: usd(1), SenderMode: "WHATEVER"}, []string{RejectRecipientNotAllowed}},
		"per transfer":      {PolicyInput{Notional: usd(10_000_01)}, []string{RejectLimitPerTransfer}},
		"zero notional":     {PolicyInput{Notional: usd(0)}, []string{RejectLimitPerTransfer}},
		"daily":             {PolicyInput{Notional: usd(1), Daily: Usage{Notional: usd(25_000_00)}}, []string{RejectLimitDaily}},
		"velocity count":    {PolicyInput{Notional: usd(1), Velocity: Usage{Count: 20}}, []string{RejectVelocityCount}},
		"velocity value":    {PolicyInput{Notional: usd(1), Velocity: Usage{Notional: usd(20_000_00)}}, []string{RejectVelocityNotional}},
		"everything": {PolicyInput{Notional: usd(10_000_01), Rule: RuleBlock, Daily: Usage{Notional: usd(20_000_00)},
			Velocity: Usage{Count: 20, Notional: usd(10_000_00)}},
			[]string{RejectRecipientBlocked, RejectLimitPerTransfer, RejectLimitDaily, RejectVelocityCount, RejectVelocityNotional}},
		"overflow refuses": {PolicyInput{Notional: usd(1), Daily: Usage{Notional: money.MaxUSD()}}, []string{RejectLimitDaily}},
	} {
		t.Run(name, func(t *testing.T) {
			d := p.Evaluate(c.in)
			if c.want == nil {
				c.want = []string{}
			}
			assert.Equal(t, c.want, d.Codes)
			assert.Equal(t, d.Allowlisted, !contains(d.Codes, RejectRecipientBlocked) && !contains(d.Codes, RejectRecipientNotAllowed))
		})
	}
	assert.True(t, p.Evaluate(PolicyInput{Notional: usd(1_000_01)}).StepUpRequired)
	assert.False(t, p.Evaluate(PolicyInput{Notional: usd(1_000_00)}).StepUpRequired, "at the threshold is not above it")
	assert.True(t, (Policy{AllowlistOnly: true}).Evaluate(PolicyInput{SenderMode: SenderOpen}).AllowlistOnly,
		"a policy-wide allowlist wins over an open sender")
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func genInput(t *rapid.T) PolicyInput {
	return PolicyInput{
		Notional:   usd(rapid.Int64Range(-5, 30_000_00).Draw(t, "notional")),
		Daily:      Usage{Count: rapid.IntRange(0, 40).Draw(t, "daily_count"), Notional: usd(rapid.Int64Range(0, 40_000_00).Draw(t, "daily"))},
		Velocity:   Usage{Count: rapid.IntRange(0, 40).Draw(t, "vel_count"), Notional: usd(rapid.Int64Range(0, 40_000_00).Draw(t, "vel"))},
		Rule:       rapid.SampledFrom([]RuleKind{"", RuleAllow, RuleBlock, RuleClear}).Draw(t, "rule"),
		SenderMode: rapid.SampledFrom([]SenderMode{"", SenderOpen, SenderAllowlistOnly}).Draw(t, "mode"),
	}
}

// TestProp_PolicyDecisionsAreDeterministic: the same policy and input give
// the same decision, every time.
func TestProp_PolicyDecisionsAreDeterministic(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(rt *rapid.T) {
		in := genInput(rt)
		a, b := p.Evaluate(in), p.Evaluate(in)
		if !assert.ObjectsAreEqual(a, b) {
			rt.Fatalf("non-deterministic: %+v vs %+v", a, b)
		}
	})
}

// TestProp_MoreUsageNeverTurnsARefusalIntoAnAcceptance: the limits are
// monotone -- having sent more can only refuse more.
func TestProp_MoreUsageNeverTurnsARefusalIntoAnAcceptance(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(rt *rapid.T) {
		in := genInput(rt)
		more := in
		more.Daily.Notional = usd(in.Daily.Notional.Minor() + rapid.Int64Range(0, 10_000_00).Draw(rt, "more_daily"))
		more.Velocity.Count = in.Velocity.Count + rapid.IntRange(0, 10).Draw(rt, "more_count")
		more.Velocity.Notional = usd(in.Velocity.Notional.Minor() + rapid.Int64Range(0, 10_000_00).Draw(rt, "more_vel"))
		if len(p.Evaluate(in).Codes) > 0 && len(p.Evaluate(more).Codes) == 0 {
			rt.Fatalf("more usage accepted what less usage refused: %+v", more)
		}
	})
}

// TestProp_AcceptedTransfersNeverExceedTheLimits: drive a sequence of
// transfers through Evaluate, accumulating usage exactly as the register
// does (refused ones move nothing); whatever is accepted stays within every
// limit.
func TestProp_AcceptedTransfersNeverExceedTheLimits(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(rt *rapid.T) {
		daily, velocity := Usage{}, Usage{}
		for range rapid.IntRange(1, 60).Draw(rt, "transfers") {
			n := usd(rapid.Int64Range(1, 12_000_00).Draw(rt, "notional"))
			d := p.Evaluate(PolicyInput{Notional: n, Daily: daily, Velocity: velocity})
			if len(d.Codes) > 0 {
				continue
			}
			daily = Usage{Count: daily.Count + 1, Notional: saturatingAdd(daily.Notional, n)}
			velocity = Usage{Count: velocity.Count + 1, Notional: saturatingAdd(velocity.Notional, n)}
			if n.Cmp(p.MaxPerTransfer) > 0 || daily.Notional.Cmp(p.MaxDaily) > 0 ||
				velocity.Count > p.VelocityMaxCount || velocity.Notional.Cmp(p.VelocityMaxNotional) > 0 {
				rt.Fatalf("accepted past a limit: transfer %s, daily %s, velocity %d / %s", n, daily.Notional, velocity.Count, velocity.Notional)
			}
		}
	})
}
