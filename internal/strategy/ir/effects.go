package ir

import "sort"

// Effect is a capability the runtime grants to a strategy version (goal
// PART 63). Only the allowed table can ever be persisted
// (strategy_versions.effect_set CHECK); the forbidden names exist as
// constants so the compiler, the runtime and the golden corpus can name
// each rejection precisely.
type Effect string

// Allowed effects.
const (
	EffectReadMarketData         Effect = "READ_MARKET_DATA"
	EffectReadOnchainData        Effect = "READ_ONCHAIN_DATA"
	EffectReadApprovedSocialData Effect = "READ_APPROVED_SOCIAL_DATA"
	EffectReadWalletIntelligence Effect = "READ_WALLET_INTELLIGENCE"
	EffectCallModel              Effect = "CALL_MODEL"
	EffectCommitPrediction       Effect = "COMMIT_PREDICTION"
	EffectCreateTradeIntent      Effect = "CREATE_TRADE_INTENT"
)

// Forbidden effects: reserved names that are always rejected, at compile
// time and again at runtime.
const (
	EffectRawSign               Effect = "RAW_SIGN"
	EffectTransferValue         Effect = "TRANSFER_VALUE"
	EffectWithdraw              Effect = "WITHDRAW"
	EffectChangeRisk            Effect = "CHANGE_RISK"
	EffectChangeCapital         Effect = "CHANGE_CAPITAL"
	EffectExportSecret          Effect = "EXPORT_SECRET"
	EffectArbitraryNetwork      Effect = "ARBITRARY_NETWORK"
	EffectArbitraryContractCall Effect = "ARBITRARY_CONTRACT_CALL"
	EffectModifyCapabilityGate  Effect = "MODIFY_CAPABILITY_GATE"
	EffectAccessAdminAPI        Effect = "ACCESS_ADMIN_API"
)

var allowedEffects = []Effect{
	EffectReadMarketData, EffectReadOnchainData, EffectReadApprovedSocialData, EffectReadWalletIntelligence,
	EffectCallModel, EffectCommitPrediction, EffectCreateTradeIntent,
}

var forbiddenEffects = []Effect{
	EffectRawSign, EffectTransferValue, EffectWithdraw, EffectChangeRisk, EffectChangeCapital,
	EffectExportSecret, EffectArbitraryNetwork, EffectArbitraryContractCall, EffectModifyCapabilityGate, EffectAccessAdminAPI,
}

// AllowedEffects returns the allowed table (sorted). The slice is a copy.
func AllowedEffects() []Effect { return SortEffects(allowedEffects) }

// ForbiddenEffects returns the reserved, always-rejected names (sorted).
func ForbiddenEffects() []Effect { return SortEffects(forbiddenEffects) }

// Allowed reports whether e is in the allowed table.
func (e Effect) Allowed() bool {
	for _, a := range allowedEffects {
		if a == e {
			return true
		}
	}
	return false
}

// Forbidden reports whether e is a reserved forbidden name. An unknown name
// is neither allowed nor forbidden and is rejected as forbidden by the
// validator (fail closed).
func (e Effect) Forbidden() bool {
	for _, f := range forbiddenEffects {
		if f == e {
			return true
		}
	}
	return false
}

// String returns the wire form.
func (e Effect) String() string { return string(e) }

// SortEffects returns a sorted, deduplicated copy.
func SortEffects(in []Effect) []Effect {
	out := make([]Effect, 0, len(in))
	seen := map[Effect]struct{}{}
	for _, e := range in {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// EffectOfDependency maps a dependency kind to the effect it requires.
// Unknown kinds map to "" (no effect; the STRUCTURAL stage rejects them).
func EffectOfDependency(kind DependencyKind) Effect {
	switch kind {
	case DepPrice, DepFeature:
		return EffectReadMarketData
	case DepOnchain, DepWalletEvent:
		return EffectReadOnchainData
	case DepSocial:
		return EffectReadApprovedSocialData
	case DepWalletIntelligence:
		return EffectReadWalletIntelligence
	case DepModel:
		return EffectCallModel
	}
	return ""
}

// EffectOfAction maps an action kind to the effect it requires.
func EffectOfAction(kind ActionKind) Effect {
	switch kind {
	case ActionCallModel:
		return EffectCallModel
	case ActionCommitPrediction:
		return EffectCommitPrediction
	case ActionCreateTradeIntent:
		return EffectCreateTradeIntent
	}
	return ""
}

// DeriveEffects computes the effect set a document actually needs from its
// dependencies and actions (STRATEGY_IR.md §3). It is pure and total: an
// unknown kind contributes nothing, and the result is sorted and unique so
// it can be compared with the declared set byte for byte.
func DeriveEffects(ir *IR) []Effect {
	if ir == nil {
		return []Effect{}
	}
	var out []Effect
	for _, d := range ir.Dependencies {
		if e := EffectOfDependency(d.Kind); e != "" {
			out = append(out, e)
		}
	}
	for _, a := range ir.Actions {
		if e := EffectOfAction(a.Kind); e != "" {
			out = append(out, e)
		}
	}
	return SortEffects(out)
}

// EffectsEqual reports whether two effect lists denote the same set.
func EffectsEqual(a, b []Effect) bool {
	x, y := SortEffects(a), SortEffects(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// ContainsEffect reports whether set contains e.
func ContainsEffect(set []Effect, e Effect) bool {
	for _, x := range set {
		if x == e {
			return true
		}
	}
	return false
}

// EffectStrings renders effects as strings for persistence.
func EffectStrings(in []Effect) []string {
	out := make([]string, 0, len(in))
	for _, e := range SortEffects(in) {
		out = append(out, string(e))
	}
	return out
}

// EffectsFromStrings parses persisted effect names without validating them;
// the caller decides what an unknown name means.
func EffectsFromStrings(in []string) []Effect {
	out := make([]Effect, 0, len(in))
	for _, s := range in {
		out = append(out, Effect(s))
	}
	return SortEffects(out)
}
