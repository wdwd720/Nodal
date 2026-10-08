package delegation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
)

// acyclic reports whether the transition graph has no cycle: the property
// that makes the accumulating transition binding (00733) safe for swaps and
// withdrawals.
func acyclic[S comparable](edges map[S][]S) bool {
	const (
		white = iota
		grey
		black
	)
	color := map[S]int{}
	var visit func(S) bool
	visit = func(n S) bool {
		color[n] = grey
		for _, m := range edges[n] {
			switch color[m] {
			case grey:
				return false
			case white:
				if !visit(m) {
					return false
				}
			}
		}
		color[n] = black
		return true
	}
	for n := range edges {
		if color[n] == white && !visit(n) {
			return false
		}
	}
	return true
}

func TestTheStatusMachinesAreAcyclic(t *testing.T) {
	assert.True(t, acyclic(swapTransitions))
	assert.True(t, acyclic(withdrawalTransitions))
	assert.True(t, acyclic(capabilityTransitions))
}

func TestTerminalStatusesHaveNoExit(t *testing.T) {
	for _, s := range AllSwapStatuses() {
		if s.Terminal() {
			assert.Empty(t, swapTransitions[s], "%s is terminal", s)
		}
	}
	for _, s := range AllWithdrawalStatuses() {
		if s.Terminal() {
			assert.Empty(t, withdrawalTransitions[s], "%s is terminal", s)
		}
	}
	assert.False(t, CanTransitionCapability(CapabilityRevoked, CapabilityActive), "a revocation is final")
	assert.False(t, CanTransitionDestination(DestinationRemoved, DestinationActive))
}

// No path leads from a signed state back to one that could be signed again,
// and nothing leads out of SETTLED: one signature, one settlement.
func TestNothingIsSignedOrSettledTwice(t *testing.T) {
	for _, from := range []SwapStatus{SwapSigned, SwapSubmitted, SwapUnknown, SwapSettled, SwapFailed} {
		for _, to := range []SwapStatus{SwapAdmitted, SwapAwaitingApproval, SwapApproved, SwapSigned} {
			assert.False(t, CanTransitionSwap(from, to), "%s -> %s", from, to)
		}
	}
	for _, from := range []WithdrawalStatus{WithdrawalSigned, WithdrawalSubmitted, WithdrawalUnknown, WithdrawalSettled, WithdrawalFailed} {
		for _, to := range []WithdrawalStatus{WithdrawalProposed, WithdrawalAwaitingApproval, WithdrawalApproved, WithdrawalSigned} {
			assert.False(t, CanTransitionWithdrawal(from, to), "%s -> %s", from, to)
		}
	}
	// An unknown submission is reconciled to an outcome, never retried.
	assert.ElementsMatch(t, []SwapStatus{SwapSettled, SwapFailed}, swapTransitions[SwapUnknown])
	assert.ElementsMatch(t, []WithdrawalStatus{WithdrawalSettled, WithdrawalFailed}, withdrawalTransitions[WithdrawalUnknown])
}

func TestEveryEnumListIsUniqueAndValid(t *testing.T) {
	unique := func(vals []string) {
		seen := map[string]bool{}
		for _, v := range vals {
			require.False(t, seen[v], "duplicate %s", v)
			require.NotEmpty(t, v)
			seen[v] = true
		}
	}
	unique(strs(AllActionClasses()))
	unique(strs(AllInstrumentClasses()))
	unique(strs(AllSwapStatuses()))
	unique(strs(AllWithdrawalStatuses()))
	unique(strs(AllCapabilityStatuses()))
	unique(strs(AllDestinationStatuses()))
	unique(strs(AllDecisions()))
	unique(strs(AllVerdicts()))
	unique(strs(AllSubjectKinds()))
	unique(strs(StoredEvaluationOutcomes()))
	for _, a := range AllActionClasses() {
		assert.True(t, a.Valid())
	}
	for _, c := range AllInstrumentClasses() {
		assert.True(t, c.Valid())
	}
	assert.False(t, ActionClass("WITHDRAW").Valid(), "there is no action class that executes a withdrawal")
}

// The swap action classes are exactly the econ intent kinds of the same name,
// and the capability modes are simulated modes every one of them implements.
func TestActionClassesAreEconIntentKinds(t *testing.T) {
	for _, a := range SwapActionClasses() {
		assert.True(t, econ.IntentKind(a).Valid(), "%s", a)
	}
	for _, m := range SupportedModes() {
		assert.True(t, m.Simulated())
		for _, c := range []gates.Capability{gates.AgentSigning, gates.AgentWithdrawals} {
			spec, ok := gates.LookupAdvanced(c)
			require.True(t, ok)
			assert.Contains(t, spec.SimulatedModes, m)
		}
	}
	assert.NotContains(t, SupportedModes(), gates.ModeLive)
}

// reachable returns from and every status reachable from it.
func reachable[S comparable](edges map[S][]S, from S) map[S]bool {
	seen := map[S]bool{from: true}
	queue := []S{from}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, m := range edges[n] {
			if !seen[m] {
				seen[m] = true
				queue = append(queue, m)
			}
		}
	}
	return seen
}

// The status lists the table CHECKs are built from follow from the machines:
// a subject carries a signed transaction exactly in SIGNED and every status
// reachable from it, and a failure code exactly in the terminal statuses
// other than SETTLED.
func TestTheStatusListsFollowFromTheMachines(t *testing.T) {
	signedSwap := reachable(swapTransitions, SwapSigned)
	var swapBefore, swapFailed []SwapStatus
	for _, s := range AllSwapStatuses() {
		if !signedSwap[s] {
			swapBefore = append(swapBefore, s)
		}
		if s.Terminal() && s != SwapSettled {
			swapFailed = append(swapFailed, s)
		}
	}
	assert.ElementsMatch(t, swapBefore, SwapStatusesBeforeSigning())
	assert.ElementsMatch(t, swapFailed, SwapStatusesWithFailureCode())

	signedWithdrawal := reachable(withdrawalTransitions, WithdrawalSigned)
	var withdrawalBefore, withdrawalFailed []WithdrawalStatus
	for _, s := range AllWithdrawalStatuses() {
		if !signedWithdrawal[s] {
			withdrawalBefore = append(withdrawalBefore, s)
		}
		if s.Terminal() && s != WithdrawalSettled {
			withdrawalFailed = append(withdrawalFailed, s)
		}
	}
	assert.ElementsMatch(t, withdrawalBefore, WithdrawalStatusesBeforeSigning())
	assert.ElementsMatch(t, withdrawalFailed, WithdrawalStatusesWithFailureCode())
}
