package execution

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/provider"
)

func TestAttemptTransitions(t *testing.T) {
	t.Parallel()
	all := AllAttemptStatuses()
	require.Len(t, all, 15)
	require.Len(t, AttemptTransitions, 15)
	for _, s := range all {
		require.True(t, s.Valid())
		for _, to := range AttemptTransitions[s] {
			require.True(t, to.Valid())
			require.NotEqual(t, s, to)
		}
	}
	terminal := map[AttemptStatus]bool{
		AttemptInspectionRejected: true, AttemptSigningRejected: true, AttemptFinalized: true, AttemptFailed: true, AttemptExpired: true,
	}
	for _, s := range all {
		require.Equal(t, terminal[s], s.Terminal(), "%s", s)
	}
	recoverable := map[AttemptStatus]bool{AttemptSubmitted: true, AttemptSubmissionUnknown: true, AttemptObserved: true, AttemptConfirmed: true}
	for _, s := range all {
		require.Equal(t, recoverable[s], s.Recoverable(), "%s recoverable", s)
	}
	// The happy path and the recovery path.
	happy := []AttemptStatus{AttemptBuilt, AttemptInspected, AttemptSigningRequested, AttemptSigned, AttemptSubmitting, AttemptSubmitted, AttemptObserved, AttemptConfirmed, AttemptFinalized}
	for i := 1; i < len(happy); i++ {
		require.True(t, CanTransitionAttempt(happy[i-1], happy[i]), "%s -> %s", happy[i-1], happy[i])
	}
	require.True(t, CanTransitionAttempt(AttemptSubmitting, AttemptSubmissionUnknown))
	require.True(t, CanTransitionAttempt(AttemptSubmissionUnknown, AttemptAdopted))
	require.True(t, CanTransitionAttempt(AttemptAdopted, AttemptConfirmed))
	require.True(t, CanTransitionAttempt(AttemptSubmissionUnknown, AttemptExpired))
	require.False(t, CanTransitionAttempt(AttemptSubmissionUnknown, AttemptSubmitted), "unknown submissions are adopted, never re-marked submitted")
	require.False(t, CanTransitionAttempt(AttemptFinalized, AttemptFailed))
	require.False(t, CanTransitionAttempt(AttemptBuilt, AttemptSigned), "signing needs inspection first")
}

func TestAttemptValidate(t *testing.T) {
	t.Parallel()
	a := Attempt{ID: NewAttemptID(), OrderID: NewOrderID(), PlanID: "p", WalletID: "w", Provider: "jupiter", QuoteID: "q", Status: AttemptBuilt, CorrelationID: "c"}
	require.NoError(t, a.Validate())
	a.Status = "NOPE"
	require.Error(t, a.Validate())
	a.Status = AttemptBuilt
	a.Finality = "SOON"
	require.Error(t, a.Validate())
}

func TestMethodRetryClass(t *testing.T) {
	t.Parallel()
	want := map[AdapterMethod]provider.RetryClass{
		MethodQuote: provider.SafeRetry, MethodValidateQuote: provider.SafeRetry, MethodBuild: provider.SafeRetry,
		MethodStatus: provider.SafeRetry, MethodReconcile: provider.SafeRetry,
		MethodSubmit: provider.UnknownEffectWrite, MethodCancel: provider.IdempotentWrite,
	}
	require.Len(t, AllAdapterMethods(), len(want))
	for m, c := range want {
		require.Equal(t, c, MethodRetryClass(m), "%s", m)
	}
	require.False(t, MethodRetryClass(MethodSubmit).MayRetryBlindly())
	require.Equal(t, provider.UnknownEffectWrite, MethodRetryClass("Unknown"), "unclassified operations are never retried blindly")
}

func TestExternalState(t *testing.T) {
	t.Parallel()
	for _, s := range []ExternalState{ExternalPending, ExternalObserved, ExternalConfirmed, ExternalFinalized, ExternalFailed, ExternalExpired, ExternalNotFound} {
		require.True(t, s.Valid())
	}
	require.False(t, ExternalState("DONE").Valid())
	require.Equal(t, FinalityConfirmed, ExternalConfirmed.Finality())
	require.Equal(t, FinalityLevel(""), ExternalPending.Finality())
	require.True(t, ExternalFinalized.Terminal())
	require.False(t, ExternalConfirmed.Terminal(), "CONFIRMED can still be reorged")
}
