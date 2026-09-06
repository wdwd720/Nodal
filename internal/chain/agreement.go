package chain

import (
	"fmt"
	"sort"
	"strings"
)

// ResolutionState is the outcome of comparing observers.
type ResolutionState string

// Resolution states.
const (
	// Agreed: both observers found the transaction with identical economics.
	Agreed ResolutionState = "AGREED"
	// Disagreed: the observers conflict on found-ness or economics. Dependent
	// activity is blocked until reconciliation resolves it.
	Disagreed ResolutionState = "DISAGREED"
	// PrimaryOnly / SecondaryOnly: a single observer answered (the other is
	// disabled, unhealthy or unavailable); finality is capped at CONFIRMED.
	PrimaryOnly   ResolutionState = "PRIMARY_ONLY"
	SecondaryOnly ResolutionState = "SECONDARY_ONLY"
	// NotFound: no available observer knows the signature.
	NotFound ResolutionState = "NOT_FOUND"
)

// Side names an observer in the pair.
type Side string

// Sides.
const (
	SidePrimary   Side = "PRIMARY"
	SideSecondary Side = "SECONDARY"
)

// ConflictPreference selects which observation is reported when both
// observers found the transaction with identical economics but at different
// commitment levels. It never raises finality: the resolved level is always
// the lower of the two.
type ConflictPreference string

// Conflict preferences.
const (
	// PreferChainRPC reports the secondary (canonical chain RPC) observation
	// on a commitment-level conflict.
	PreferChainRPC ConflictPreference = "CHAIN_RPC"
	// PreferPrimary reports the primary observation.
	PreferPrimary ConflictPreference = "PRIMARY"
)

// DisagreementAction is what the policy asks callers to do on DISAGREED.
type DisagreementAction string

// Disagreement actions.
const (
	// BlockDependentActivity: open a reconciliation mismatch and block every
	// activity that depends on the transaction (the only supported action).
	BlockDependentActivity DisagreementAction = "BLOCK_DEPENDENT_ACTIVITY"
)

// AgreementPolicy decides what two observers may jointly assert
// (EXECUTION.md §6, PART 196). The zero value is invalid; use DefaultPolicy.
type AgreementPolicy struct {
	// RequireBothFor is the finality that can only be granted when both
	// observers agree at that level. Anything at or above it is capped one
	// level lower in single-observer resolutions.
	RequireBothFor Finality
	// PreferChainRPCFor selects the reported observation on a
	// commitment-level conflict.
	PreferChainRPCFor ConflictPreference
	// OnDisagreement is recorded on every DISAGREED resolution.
	OnDisagreement DisagreementAction
}

// DefaultPolicy is the fixed production policy:
// {RequireBothFor: FINALIZED, PreferChainRPCFor: CHAIN_RPC, OnDisagreement:
// BLOCK_DEPENDENT_ACTIVITY}.
func DefaultPolicy() AgreementPolicy {
	return AgreementPolicy{
		RequireBothFor:    FinalityFinalized,
		PreferChainRPCFor: PreferChainRPC,
		OnDisagreement:    BlockDependentActivity,
	}
}

// Validate checks the policy is well-formed. RequireBothFor must be at least
// CONFIRMED: allowing a single observer to assert CONFIRMED is the documented
// degraded mode, allowing more is not supported.
func (p AgreementPolicy) Validate() error {
	if !p.RequireBothFor.Valid() || p.RequireBothFor.Rank() < FinalityConfirmed.Rank() {
		return fmt.Errorf("chain: RequireBothFor must be CONFIRMED or FINALIZED, got %q", p.RequireBothFor)
	}
	switch p.PreferChainRPCFor {
	case PreferChainRPC, PreferPrimary:
	default:
		return fmt.Errorf("chain: unknown conflict preference %q", p.PreferChainRPCFor)
	}
	if p.OnDisagreement != BlockDependentActivity {
		return fmt.Errorf("chain: unsupported disagreement action %q", p.OnDisagreement)
	}
	return nil
}

// singleCap is the strongest finality a single observer may assert.
func (p AgreementPolicy) singleCap() Finality {
	switch p.RequireBothFor {
	case FinalityFinalized:
		return FinalityConfirmed
	default:
		return FinalityObserved
	}
}

// Resolution is the joint verdict.
type Resolution struct {
	State ResolutionState `json:"state"`
	// Finality is the strongest level the evidence supports; SUBMITTED when
	// nothing may be relied on.
	Finality Finality `json:"finality"`
	// Satisfied reports whether Finality meets the finality the caller asked
	// for and the state is AGREED or a single-observer state.
	Satisfied bool `json:"satisfied"`
	// BlockDependent asks the caller to block activity that depends on this
	// transaction (mismatch, or absence that cannot be proven).
	BlockDependent bool `json:"block_dependent"`
	// Degraded is true when only one observer contributed.
	Degraded bool   `json:"degraded"`
	Detail   string `json:"detail"`
	// Observation is the reported observation (nil for NOT_FOUND). Primary
	// and Secondary carry what each side answered, when it answered.
	Observation *TxObservation `json:"observation,omitempty"`
	Primary     *TxObservation `json:"primary,omitempty"`
	Secondary   *TxObservation `json:"secondary,omitempty"`
	// Differences lists the economic fields that differ on DISAGREED,
	// sorted and stable.
	Differences []string `json:"differences,omitempty"`
}

// Resolve compares a primary and a secondary observation of the same
// signature. It never picks the optimistic answer: any economic difference
// is DISAGREED, commitment conflicts resolve to the lower level, and
// FINALIZED needs both at finalized.
func (p AgreementPolicy) Resolve(primary, secondary TxObservation, required Finality) Resolution {
	pc, sc := primary, secondary
	res := Resolution{Primary: &pc, Secondary: &sc}
	if primary.Signature != secondary.Signature {
		res.State = Disagreed
		res.Finality = FinalitySubmitted
		res.BlockDependent = true
		res.Differences = []string{"signature"}
		res.Detail = "observations are of different signatures; " + string(p.OnDisagreement)
		return res
	}
	switch {
	case !primary.Found && !secondary.Found:
		res.State = NotFound
		res.Finality = FinalitySubmitted
		res.Detail = "not found by either observer"
		return res
	case primary.Found != secondary.Found:
		res.State = Disagreed
		res.Finality = FinalitySubmitted
		res.BlockDependent = true
		res.Differences = []string{"found"}
		found, missing := primary.Source, secondary.Source
		if secondary.Found {
			found, missing = secondary.Source, primary.Source
		}
		res.Detail = fmt.Sprintf("found by %s but not by %s; %s", nameOr(found, "primary"), nameOr(missing, "secondary"), p.OnDisagreement)
		return res
	}
	diffs := EconomicDifferences(primary, secondary)
	if len(diffs) > 0 {
		res.State = Disagreed
		res.Finality = FinalitySubmitted
		res.BlockDependent = true
		res.Differences = diffs
		res.Detail = "observers disagree on " + strings.Join(diffs, ", ") + "; " + string(p.OnDisagreement)
		return res
	}
	// Economics agree. Finality is the weaker commitment.
	res.State = Agreed
	res.Finality = MinFinality(primary.Finality(), secondary.Finality())
	reported := &pc
	if primary.Commitment == secondary.Commitment {
		res.Detail = "both observers agree at " + string(primary.Commitment)
	} else {
		if p.PreferChainRPCFor == PreferChainRPC {
			reported = &sc
		}
		res.Detail = fmt.Sprintf("economics agree; commitment conflict %s=%s %s=%s resolved to %s",
			nameOr(primary.Source, "primary"), primary.Commitment,
			nameOr(secondary.Source, "secondary"), secondary.Commitment, res.Finality)
	}
	res.Observation = reported
	if required.AtLeast(p.RequireBothFor) && !res.Finality.AtLeast(p.RequireBothFor) {
		res.Detail += fmt.Sprintf("; %s requires both observers at %s", required, p.RequireBothFor)
	}
	res.Satisfied = res.Finality.AtLeast(required)
	return res
}

// ResolveSingle is the degraded path: only one observer answered. Finality
// is capped below RequireBothFor and absence is never proven.
func (p AgreementPolicy) ResolveSingle(obs TxObservation, side Side, required Finality, reason string) Resolution {
	oc := obs
	res := Resolution{Degraded: true}
	if side == SideSecondary {
		res.Secondary = &oc
	} else {
		res.Primary = &oc
	}
	prefix := "degraded observation: " + reason
	if !obs.Found {
		res.State = NotFound
		res.Finality = FinalitySubmitted
		res.BlockDependent = true
		res.Detail = prefix + "; absence cannot be proven by one observer"
		return res
	}
	if side == SideSecondary {
		res.State = SecondaryOnly
	} else {
		res.State = PrimaryOnly
	}
	res.Observation = &oc
	res.Finality = MinFinality(obs.Finality(), p.singleCap())
	res.Detail = fmt.Sprintf("%s; finality capped at %s", prefix, p.singleCap())
	res.Satisfied = res.Finality.AtLeast(required)
	res.BlockDependent = !res.Satisfied && required.AtLeast(p.RequireBothFor)
	return res
}

// EconomicDifferences lists the economic fields on which two found
// observations differ (sorted). Empty means they describe the same
// transaction outcome. Timestamps, source, raw references and commitment are
// not economic and are ignored.
func EconomicDifferences(a, b TxObservation) []string {
	var diffs []string
	if a.Slot != b.Slot {
		diffs = append(diffs, "slot")
	}
	if a.Err != b.Err {
		diffs = append(diffs, "err")
	}
	if a.Version != b.Version && a.Version != "" && b.Version != "" {
		diffs = append(diffs, "version")
	}
	if !a.Fee.Equal(b.Fee) {
		diffs = append(diffs, "fee")
	}
	if a.BlockTime != nil && b.BlockTime != nil && !a.BlockTime.Equal(*b.BlockTime) {
		diffs = append(diffs, "block_time")
	}
	if !sameStringSet(a.AccountKeys, b.AccountKeys) {
		diffs = append(diffs, "account_keys")
	}
	if !sameTokenDeltas(a.TokenBalanceDeltas, b.TokenBalanceDeltas) {
		diffs = append(diffs, "token_balance_deltas")
	}
	if !sameLamportDeltas(a.LamportDeltas, b.LamportDeltas) {
		diffs = append(diffs, "lamport_deltas")
	}
	sort.Strings(diffs)
	return diffs
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func sameTokenDeltas(a, b []TokenDelta) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]TokenDelta(nil), a...)
	bs := append([]TokenDelta(nil), b...)
	SortTokenDeltas(as)
	SortTokenDeltas(bs)
	for i := range as {
		x, y := as[i], bs[i]
		if x.Owner != y.Owner || x.Mint != y.Mint || x.TokenAccount != y.TokenAccount ||
			x.Decimals != y.Decimals || !x.Pre.Equal(y.Pre) || !x.Post.Equal(y.Post) {
			return false
		}
	}
	return true
}

func sameLamportDeltas(a, b []LamportDelta) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]LamportDelta(nil), a...)
	bs := append([]LamportDelta(nil), b...)
	SortLamportDeltas(as)
	SortLamportDeltas(bs)
	for i := range as {
		x, y := as[i], bs[i]
		if x.Account != y.Account || !x.Pre.Equal(y.Pre) || !x.Post.Equal(y.Post) {
			return false
		}
	}
	return true
}

func nameOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// BalanceResolution is the joint verdict on a wallet's balances.
type BalanceResolution struct {
	State          ResolutionState `json:"state"`
	BlockDependent bool            `json:"block_dependent"`
	Degraded       bool            `json:"degraded"`
	Detail         string          `json:"detail"`
	// Balances are the reported balances (both sides agree, or the single
	// available side), canonically ordered.
	Balances []BalanceObservation `json:"balances,omitempty"`
	// Differences lists "<mint>/<token account>" keys whose amounts differ.
	Differences []string `json:"differences,omitempty"`
	// SlotSkew is |primary slot − secondary slot| over the compared reads
	// (0 when a side reports no slot); a large skew explains a transient
	// difference and callers should re-query before opening a mismatch.
	SlotSkew uint64 `json:"slot_skew"`
}

// ResolveBalances compares two observers' balance sets for one wallet. Every
// (mint, token account) must be present on both sides with equal amounts.
func (p AgreementPolicy) ResolveBalances(primary, secondary []BalanceObservation) BalanceResolution {
	key := func(b BalanceObservation) string { return b.Mint + "/" + b.TokenAccount }
	pm := map[string]BalanceObservation{}
	for _, b := range primary {
		pm[key(b)] = b
	}
	sm := map[string]BalanceObservation{}
	for _, b := range secondary {
		sm[key(b)] = b
	}
	var diffs []string
	var maxP, maxS uint64
	for k, pb := range pm {
		if pb.Slot > maxP {
			maxP = pb.Slot
		}
		sb, ok := sm[k]
		if !ok || !pb.Amount.Equal(sb.Amount) {
			diffs = append(diffs, k)
		}
	}
	for k, sb := range sm {
		if sb.Slot > maxS {
			maxS = sb.Slot
		}
		if _, ok := pm[k]; !ok {
			diffs = append(diffs, k)
		}
	}
	sort.Strings(diffs)
	res := BalanceResolution{}
	if maxP > 0 && maxS > 0 {
		if maxP > maxS {
			res.SlotSkew = maxP - maxS
		} else {
			res.SlotSkew = maxS - maxP
		}
	}
	if len(diffs) > 0 {
		res.State = Disagreed
		res.BlockDependent = true
		res.Differences = diffs
		res.Detail = "balances differ on " + strings.Join(diffs, ", ") + "; " + string(p.OnDisagreement)
		return res
	}
	out := append([]BalanceObservation(nil), primary...)
	SortBalances(out)
	res.State = Agreed
	res.Balances = out
	res.Detail = fmt.Sprintf("both observers agree on %d balances", len(out))
	return res
}

// ResolveBalancesSingle is the degraded balance path.
func (p AgreementPolicy) ResolveBalancesSingle(bs []BalanceObservation, side Side, reason string) BalanceResolution {
	out := append([]BalanceObservation(nil), bs...)
	SortBalances(out)
	state := PrimaryOnly
	if side == SideSecondary {
		state = SecondaryOnly
	}
	return BalanceResolution{
		State:    state,
		Degraded: true,
		Balances: out,
		Detail:   "degraded observation: " + reason,
	}
}
