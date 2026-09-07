package payout

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// ---------------------------------------------------------------------------
// State machine
// ---------------------------------------------------------------------------

func TestState_AllThirteenDeclared(t *testing.T) {
	require.Len(t, AllStates(), 13, "PART XXI names twelve states plus PAYOUT_STATUS_UNKNOWN")
	for _, s := range AllStates() {
		require.True(t, s.Valid(), "%s", s)
	}
	require.False(t, State("DONE").Valid())
}

// TestState_UnknownCannotBeResubmitted is the property that stops a payout
// being sent twice. Once a submission may have happened, the only ways out are
// finding out that it did, finding out that it did not, or asking a human.
func TestState_UnknownCannotBeResubmitted(t *testing.T) {
	require.False(t, CanTransition(StateStatusUnknown, StateSubmitted),
		"resubmitting an uncertain payout is how a user gets paid twice")
	require.False(t, CanTransition(StateStatusUnknown, StateVerified))
	require.True(t, CanTransition(StateStatusUnknown, StateSettled))
	require.True(t, CanTransition(StateStatusUnknown, StateFailed))
	require.True(t, CanTransition(StateStatusUnknown, StateManualReview))
}

func TestState_TerminalStatesGoNowhereExceptSettledWhichCanBeClawedBack(t *testing.T) {
	for _, s := range []State{StateFailed, StateRejected, StateReversed} {
		require.True(t, s.Terminal(), "%s", s)
		for _, to := range AllStates() {
			require.False(t, CanTransition(s, to), "%s claims it can become %s", s, to)
		}
	}
	require.True(t, StateSettled.Terminal())
	require.True(t, CanTransition(StateSettled, StateReversed),
		"a provider can claw back a settled payout, and the system has to be able to record that")
	require.False(t, CanTransition(StateSettled, StateFailed))
}

// TestState_HoldsValueCoversEveryNonTerminalStateThatReserved is what a
// reconciliation sweep depends on: if a state holds value and is not terminal,
// something has to finish it.
func TestState_HoldsValueCoversEveryNonTerminalStateThatReserved(t *testing.T) {
	require.True(t, StateVerified.HoldsValue())
	require.True(t, StateSubmitted.HoldsValue())
	require.True(t, StateProviderPending.HoldsValue())
	require.True(t, StateStatusUnknown.HoldsValue())
	require.True(t, StateManualReview.HoldsValue())

	require.False(t, StateDraft.HoldsValue())
	require.False(t, StateEligibilityCheck.HoldsValue())
	for _, s := range []State{StateSettled, StateFailed, StateRejected, StateReversed} {
		require.False(t, s.HoldsValue(), "%s is terminal and holds nothing", s)
	}
}

func TestState_EveryTransitionTargetIsDeclared(t *testing.T) {
	for from, tos := range stateTransitions {
		require.True(t, from.Valid(), "unknown source %q", from)
		for _, to := range tos {
			require.True(t, to.Valid(), "%s -> %q names an unknown state", from, to)
			require.NotEqual(t, from, to)
		}
	}
	// Every declared state must be reachable, or it is decoration.
	reachable := map[State]bool{StateDraft: true, StateEligibilityCheck: true}
	for _, tos := range stateTransitions {
		for _, to := range tos {
			reachable[to] = true
		}
	}
	for _, s := range AllStates() {
		require.True(t, reachable[s], "state %s is declared but nothing can reach it", s)
	}
}

func TestDestinationKind_AllFourDeclared(t *testing.T) {
	require.Len(t, AllDestinationKinds(), 4)
	for _, k := range AllDestinationKinds() {
		require.True(t, k.Valid(), "%s", k)
	}
	require.False(t, DestinationKind("CHEQUE").Valid())
}

func TestDestinationStatus_OnlyVerifiedIsUsable(t *testing.T) {
	require.True(t, DestinationVerified.Usable())
	require.False(t, DestinationUnverified.Usable(),
		"owning an account and controlling a bank account are different facts")
	require.False(t, DestinationRejected.Usable())
	require.False(t, DestinationDisabled.Usable())
}

// ---------------------------------------------------------------------------
// Provider registry
// ---------------------------------------------------------------------------

type fakeProvider struct {
	name string
	caps Capabilities
}

func (f fakeProvider) Name() string               { return f.name }
func (f fakeProvider) Capabilities() Capabilities { return f.caps }
func (f fakeProvider) Submit(context.Context, SubmitRequest) (SubmitResult, error) {
	return SubmitResult{}, nil
}
func (f fakeProvider) Lookup(context.Context, string) (SubmitResult, error) {
	return SubmitResult{}, nil
}

func contracted() Capabilities {
	return Capabilities{
		SupportsBankPayout: true, SupportsLookup: true,
		Currencies: []string{"USD"}, ContractReference: "MSA-2026-001",
	}
}

// TestRegistry_ProductionRefusesAProviderWithNoContract is acceptance test
// SEC-001 applied where it bites: a payout provider with no commercial
// agreement behind it is a sandbox, whatever it calls itself.
func TestRegistry_ProductionRefusesAProviderWithNoContract(t *testing.T) {
	prod := NewRegistry(false)
	caps := contracted()
	caps.ContractReference = ""
	err := prod.Register(fakeProvider{name: "sandbox", caps: caps})
	require.Error(t, err)
	require.Contains(t, err.Error(), "contract reference")

	require.NoError(t, prod.Register(fakeProvider{name: "real", caps: contracted()}))

	sandbox := NewRegistry(true)
	require.NoError(t, sandbox.Register(fakeProvider{name: "sandbox", caps: caps}),
		"the same provider loads where sandboxes are permitted")
}

// TestRegistry_RefusesAProviderThatCannotAnswerLookups is the requirement that
// makes PAYOUT_STATUS_UNKNOWN resolvable at all.
func TestRegistry_RefusesAProviderThatCannotAnswerLookups(t *testing.T) {
	caps := contracted()
	caps.SupportsLookup = false
	err := NewRegistry(true).Register(fakeProvider{name: "blind", caps: caps})
	require.Error(t, err)
	require.Contains(t, err.Error(), "permanently ambiguous",
		"a provider that cannot say what happened to a key cannot be used for payouts")
}

func TestRegistry_RejectsDuplicatesAndNonsense(t *testing.T) {
	r := NewRegistry(true)
	require.NoError(t, r.Register(fakeProvider{name: "p", caps: contracted()}))
	require.Error(t, r.Register(fakeProvider{name: "p", caps: contracted()}))
	require.Error(t, r.Register(fakeProvider{name: "  ", caps: contracted()}))
	require.Error(t, r.Register(nil))

	_, err := r.Get("missing")
	require.Error(t, err)
	got, err := r.Get("p")
	require.NoError(t, err)
	require.Equal(t, "p", got.Name())
	require.Equal(t, []string{"p"}, r.Names())
}

func TestCapabilities_NothingIsInferred(t *testing.T) {
	// The zero value supports nothing, which is the correct posture for an
	// integration nobody has verified.
	var zero Capabilities
	for _, k := range AllDestinationKinds() {
		require.False(t, zero.Supports(k), "%s", k)
	}
	require.False(t, zero.SupportsCurrency("USD"))
	require.False(t, zero.SupportsCurrency(""))

	c := contracted()
	require.True(t, c.Supports(DestinationBank))
	require.False(t, c.Supports(DestinationCryptoWallet),
		"crypto payout is a separate capability and is never implied by fiat payout")
	require.True(t, c.SupportsCurrency("usd"), "currency matching is case-insensitive")
	require.False(t, c.SupportsCurrency("EUR"))
}

func TestProviderStatus_UnknownIsALegitimateAnswer(t *testing.T) {
	require.True(t, ProviderUnknown.Valid())
	require.True(t, ProviderAccepted.Valid())
	require.True(t, ProviderSettled.Valid())
	require.True(t, ProviderFailed.Valid())
	require.False(t, ProviderStatus("MAYBE").Valid())
}

// ---------------------------------------------------------------------------
// Requests and decisions
// ---------------------------------------------------------------------------

func TestCreateRequest_Validate(t *testing.T) {
	base := CreateRequest{
		AccountID:      accounts.NewAccountID(),
		Quantity:       money.QuantityFromInt64(100),
		IdempotencyKey: "k",
		EffectiveAt:    time.Now(),
	}
	require.NoError(t, base.Validate())

	t.Run("no account", func(t *testing.T) {
		r := base
		r.AccountID = accounts.AccountID{}
		require.Error(t, r.Validate())
	})
	t.Run("zero amount", func(t *testing.T) {
		r := base
		r.Quantity = money.QuantityFromInt64(0)
		require.Error(t, r.Validate())
	})
	t.Run("negative amount", func(t *testing.T) {
		r := base
		r.Quantity = money.QuantityFromInt64(-1)
		require.Error(t, r.Validate(), "a negative payout would be a deposit wearing a withdrawal's clothes")
	})
	t.Run("no idempotency key", func(t *testing.T) {
		r := base
		r.IdempotencyKey = " "
		require.Error(t, r.Validate())
	})
}

func TestDecision_SufficientRequiresFullCoverage(t *testing.T) {
	d := Decision{Requested: money.QuantityFromInt64(100), Eligible: money.QuantityFromInt64(100)}
	require.True(t, d.Sufficient())

	d.Eligible = money.QuantityFromInt64(99)
	require.False(t, d.Sufficient(), "a partial payout is not what was asked for")

	d.Eligible = money.QuantityFromInt64(101)
	require.True(t, d.Sufficient())

	require.False(t, Decision{}.Sufficient(), "a zero request is not sufficient for anything")
}

func TestDecision_ReasonStrings(t *testing.T) {
	d := Decision{Reasons: []valuedomain.PermitReason{
		valuedomain.ReasonOriginForbidden, ReasonAccountFrozen,
	}}
	require.Equal(t, []string{"ORIGIN_NOT_PAYOUT_ELIGIBLE", "ACCOUNT_FROZEN"}, d.ReasonStrings())
	require.Empty(t, Decision{}.ReasonStrings())
}
