package funding

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func predecessors(to Status) []Status {
	var out []Status
	for _, from := range AllStatuses() {
		if CanTransition(from, to) {
			out = append(out, from)
		}
	}
	return out
}

// TestTransitions_Exhaustive checks every (from, to) pair against the
// table, that every status is a key, that final states have no exits, and
// that platform-owned states cannot be skipped into.
func TestTransitions_Exhaustive(t *testing.T) {
	t.Parallel()
	all := AllStatuses()
	require.Len(t, all, 13)
	for _, from := range all {
		_, ok := Transitions[from]
		require.True(t, ok, "status %s missing from Transitions", from)
		for _, to := range all {
			want := from != to && slices.Contains(Transitions[from], to)
			require.Equal(t, want, CanTransition(from, to), "%s -> %s", from, to)
		}
		if from.Final() {
			require.Empty(t, Transitions[from], "final state %s must have no exits", from)
		} else {
			require.NotEmpty(t, Transitions[from], "non-final state %s must have exits", from)
		}
		require.NotEmpty(t, from.TimestampColumn(), "status %s has no timestamp column", from)
		require.True(t, from.Valid())
	}
	require.ElementsMatch(t, []Status{StatusProviderConfirmed, StatusReviewRequired}, predecessors(StatusSettlementObserved))
	require.ElementsMatch(t, []Status{StatusSettlementObserved, StatusReviewRequired}, predecessors(StatusReconciled))
	require.ElementsMatch(t, []Status{StatusReconciled, StatusReviewRequired}, predecessors(StatusAvailable))
	require.ElementsMatch(t, []Status{StatusReconciled, StatusAvailable, StatusReviewRequired}, predecessors(StatusReversed))
	require.ElementsMatch(t, []Status{StatusCreated}, predecessors(StatusSessionCreated))
	// PROVIDER_CONFIRMED never fails: the provider says crypto was delivered.
	require.False(t, CanTransition(StatusProviderConfirmed, StatusFailed))
	require.False(t, CanTransition(StatusProviderConfirmed, StatusReversed))
	// Nothing is reversed before a posting exists.
	for _, s := range []Status{StatusCreated, StatusSessionCreated, StatusCustomerActionRequired, StatusProviderProcessing, StatusProviderConfirmed, StatusSettlementObserved} {
		require.False(t, CanTransition(s, StatusReversed), "%s -> REVERSED must be illegal", s)
	}
	require.False(t, CanTransition(StatusCreated, StatusCreated))
	require.False(t, CanTransition("NOPE", StatusFailed))
	require.False(t, CanTransition(StatusCreated, "NOPE"))
	require.Empty(t, Status("NOPE").TimestampColumn())
	require.False(t, Status("NOPE").Valid())
}

func TestStatus_RankAndFlags(t *testing.T) {
	t.Parallel()
	prev := -1
	for _, s := range []Status{StatusCreated, StatusSessionCreated, StatusCustomerActionRequired, StatusProviderProcessing, StatusProviderConfirmed, StatusSettlementObserved, StatusReconciled, StatusAvailable} {
		r, ok := s.Rank()
		require.True(t, ok, s)
		require.Greater(t, r, prev)
		prev = r
	}
	for _, s := range []Status{StatusFailed, StatusExpired, StatusCancelled, StatusReversed, StatusReviewRequired} {
		_, ok := s.Rank()
		require.False(t, ok, s)
	}
	require.True(t, StatusCreated.Pending())
	require.True(t, StatusReviewRequired.Pending())
	require.False(t, StatusAvailable.Pending())
	require.False(t, StatusFailed.Pending())
	require.True(t, StatusProviderConfirmed.ProviderOwned())
	require.False(t, StatusSettlementObserved.ProviderOwned())
	require.False(t, StatusAvailable.ProviderOwned())
}

// TestProp_LegalWalksNeverReachForbiddenState walks random legal
// transitions and asserts the structural invariants every path must keep.
func TestProp_LegalWalksNeverReachForbiddenState(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		cur := StatusCreated
		path := []Status{cur}
		steps := rapid.IntRange(0, 16).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			next := Transitions[cur]
			if len(next) == 0 {
				break
			}
			cur = next[rapid.IntRange(0, len(next)-1).Draw(rt, "choice")]
			path = append(path, cur)
		}
		for i := 1; i < len(path); i++ {
			from, to := path[i-1], path[i]
			require.True(rt, CanTransition(from, to))
			require.False(rt, from.Final(), "left a final state %s", from)
			switch to {
			case StatusAvailable:
				require.Contains(rt, []Status{StatusReconciled, StatusReviewRequired}, from)
			case StatusReconciled:
				require.Contains(rt, []Status{StatusSettlementObserved, StatusReviewRequired}, from)
			case StatusSettlementObserved:
				require.Contains(rt, []Status{StatusProviderConfirmed, StatusReviewRequired}, from)
			case StatusReversed:
				require.Contains(rt, []Status{StatusReconciled, StatusAvailable, StatusReviewRequired}, from)
			}
		}
		// Once a walk that never touched REVIEW_REQUIRED reaches AVAILABLE, it
		// visited every platform-owned state in order.
		if slices.Contains(path, StatusAvailable) && !slices.Contains(path, StatusReviewRequired) {
			require.Less(rt, slices.Index(path, StatusProviderConfirmed), slices.Index(path, StatusSettlementObserved))
			require.Less(rt, slices.Index(path, StatusSettlementObserved), slices.Index(path, StatusReconciled))
			require.Less(rt, slices.Index(path, StatusReconciled), slices.Index(path, StatusAvailable))
		}
	})
}

func TestMapProviderStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    ProviderStatus
		want  Status
		known bool
	}{
		{ProviderStatusInitialized, StatusCustomerActionRequired, true},
		{ProviderStatusCustomerActionRequired, StatusCustomerActionRequired, true},
		{ProviderStatusProcessing, StatusProviderProcessing, true},
		{ProviderStatusConfirmed, StatusProviderConfirmed, true},
		{ProviderStatusRejected, StatusFailed, true},
		{ProviderStatusUnknown, "", false},
		{ProviderStatus("quote_ready"), "", false},
		{ProviderStatus(""), "", false},
	}
	for _, c := range cases {
		got, ok := MapProviderStatus(c.in)
		require.Equal(t, c.known, ok, c.in)
		require.Equal(t, c.want, got, c.in)
	}
}

func TestParseDecimalAmount(t *testing.T) {
	t.Parallel()
	ok := func(s string, decimals uint8, want string) {
		t.Helper()
		q, err := ParseDecimalAmount(s, decimals)
		require.NoError(t, err, s)
		require.Equal(t, want, q.String(), s)
	}
	bad := func(s string, decimals uint8, code errs.Code) {
		t.Helper()
		_, err := ParseDecimalAmount(s, decimals)
		require.Error(t, err, s)
		require.Equal(t, code, errs.CodeOf(err), "%s: %v", s, err)
	}
	ok("100.00", 6, "100000000")
	ok("0.123456", 6, "123456")
	ok("0.029133919178255537", 18, "29133919178255537")
	ok("7", 9, "7000000000")
	ok("0", 6, "0")
	bad("", 6, errs.CodeValidationFailed)
	bad("-1", 6, errs.CodeValidationFailed)
	bad("+1", 6, errs.CodeValidationFailed)
	bad("1e3", 6, errs.CodeValidationFailed)
	bad("1E3", 6, errs.CodeValidationFailed)
	bad(" 1", 6, errs.CodeValidationFailed)
	bad("1,000", 6, errs.CodeValidationFailed)
	bad("0x10", 6, errs.CodeValidationFailed)
	bad("NaN", 6, errs.CodeValidationFailed)
	bad("1.2345678", 6, errs.CodePrecisionLoss)
}

func FuzzParseDecimalAmount(f *testing.F) {
	for _, s := range []string{"100.00", "0.123456", "1e3", "-1", "", ".", "1.", "9999999999999999999999999999999999999999"} {
		f.Add(s, uint8(6))
	}
	f.Fuzz(func(t *testing.T, s string, decimals uint8) {
		q, err := ParseDecimalAmount(s, decimals)
		if err != nil {
			return
		}
		require.False(t, q.IsNegative())
		for _, c := range q.String() {
			require.True(t, c >= '0' && c <= '9')
		}
		var zero money.Quantity
		require.GreaterOrEqual(t, q.Cmp(zero), 0)
	})
}

func TestTransitionEvidence_Validate(t *testing.T) {
	t.Parallel()
	require.NoError(t, SystemEvidence("r", "", "").Validate())
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(TransitionEvidence{ActorType: "AGENT", ActorID: "a", Reason: "r"}.Validate()))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(TransitionEvidence{ActorType: "USER", ActorID: "", Reason: "r"}.Validate()))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(TransitionEvidence{ActorType: "USER", ActorID: "u", Reason: " "}.Validate()))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(TransitionEvidence{ActorType: "ROBOT", ActorID: "u", Reason: "r"}.Validate()))
}

func TestFormatMinor2(t *testing.T) {
	t.Parallel()
	require.Equal(t, "100.00", formatMinor2(10000))
	require.Equal(t, "0.05", formatMinor2(5))
	require.Equal(t, "12.34", formatMinor2(1234))
}

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()
	depositID := NewDepositID()
	at := depositID.Time()
	c := encodeCursor(at, depositID)
	gotAt, gotID, ok, err := decodeCursor(c)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, depositID, gotID)
	require.Equal(t, at.UTC().UnixNano(), gotAt.UnixNano())
	_, _, ok, err = decodeCursor("")
	require.NoError(t, err)
	require.False(t, ok)
	_, _, _, err = decodeCursor("not-base64!!")
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}
