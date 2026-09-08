package risk

import (
	"strings"
	"time"
)

// NativeTradeInput builds the Input a Nodal-native trade is evaluated as.
//
// It is a constructor rather than a struct literal at each call site so that
// every native decision carries the same Stage and the same shape, and so the
// value passed to Store.RecordDecision is provably the one that was evaluated.
func NativeTradeInput(accountID string, n NativeMarketSnapshot, now time.Time) Input {
	return Input{
		Stage:        StagePreTrade,
		Intent:       Intent{AccountID: accountID},
		NativeMarket: &n,
		Now:          now,
	}
}

// EvaluateNativeTrade runs the limits that apply to a Nodal-native trade, and
// only those.
//
// # Why this is a separate entry point
//
// Evaluate takes a full Input: a USD notional, a position, an envelope, a
// quote from a venue. A native trade has none of those. Its size is in Credits,
// which have no approved external value, and its venue is a formula rather than
// a provider. Building an Input for one would mean inventing a USD figure for
// a Credit — the single thing PART LIV forbids — so instead the kernel exposes
// the subset of itself that a native trade can be expressed in.
//
// What that subset is: the two limits PART XXXII names for this economy,
// native-market concentration and creator concentration. Both are ratios of
// counts. Nothing else in the policy can be evaluated here, and pretending
// otherwise by defaulting the USD limits to zero or to infinity would be worse
// than saying so.
//
// # Fail closed
//
// A policy missing either limit yields RISK_POLICY_MISSING naming the missing
// ones. A deployment that has not decided how much of a market one account may
// hold has not decided that any amount is fine. An Input with no native
// snapshot is RISK_INPUT_INVALID for the same reason: this entry point cannot
// evaluate anything else, so it must not answer ALLOW to a question it was not
// asked.
func EvaluateNativeTrade(p Policy, in Input) Decision {
	in = in.normalized()
	d := Decision{
		Stage:               StagePreTrade,
		PolicyHash:          p.Hash(),
		EvaluatorVersion:    EvaluatorVersion + " native-trade/1",
		EvaluatedAt:         in.Now,
		PolicyVersion:       p.Version,
		MatchedKillSwitches: []KillSwitch{},
	}
	rs := reasonSet{}
	if in.Now.IsZero() || in.NativeMarket == nil {
		rs.add(ReasonInputInvalid)
	}
	var missing []string
	if p.MaxNativeMarketConcentrationBPS == nil {
		missing = append(missing, "max_native_market_concentration_bps")
	}
	if p.MaxCreatorConcentrationBPS == nil {
		missing = append(missing, "max_creator_concentration_bps")
	}
	if p.Missing() {
		// A policy with no version at all is not a policy. Hash() on it would
		// stand in for a document nobody wrote, so the decision records no
		// policy hash rather than a hash of nothing.
		d.PolicyHash = ""
	}
	if len(missing) > 0 {
		// The reason code carries the fact; the missing names are what an
		// operator needs, and Decision has nowhere to put them, so they go in
		// the evaluator version string where every persisted decision shows
		// them. A refusal that does not say WHICH limit is absent sends
		// somebody to read the whole policy.
		rs.add(ReasonPolicyMissing)
		d.EvaluatorVersion += " missing:" + strings.Join(missing, ",")
	} else if len(rs) == 0 {
		e := evaluator{p: p, in: in, rs: rs}
		e.nativeMarketChecks()
	}
	d.ReasonCodes = rs.sorted()
	d.Verdict = Reject
	if len(d.ReasonCodes) == 0 {
		d.Verdict = Allow
	}
	d.InputHash = hashOf(in)
	d.Hash = d.ComputeHash()
	return d
}
