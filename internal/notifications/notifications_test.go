package notifications

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/notification"
)

func aUser() accounts.UserID { return accounts.NewUserID() }

// TestKinds_TheCheckAdmitsEveryNameEitherPackageDeclares.
//
// The CHECK on notifications.kind is compared against AllKinds() by
// test/integration/enums. internal/notification declares nine names of its own
// and its integration test writes them, so if AllKinds ever stopped naming one
// of the nine the CHECK would narrow under a test that still inserts it -- and
// the failure would land in a package this one does not import at runtime. This
// asserts the containment directly.
func TestKinds_TheCheckAdmitsEveryNameEitherPackageDeclares(t *testing.T) {
	t.Parallel()
	all := map[Kind]bool{}
	for _, k := range AllKinds() {
		assert.False(t, all[k], "%s appears twice in AllKinds", k)
		all[k] = true
	}
	legacy := []notification.Kind{
		notification.KindFundingAvailable, notification.KindFundingFailed, notification.KindFundingReversed,
		notification.KindTradeFilled, notification.KindTradeFailed, notification.KindAgentPaused,
		notification.KindRiskLimitHit, notification.KindSecuritySessionEvent, notification.KindReconciliationHold,
	}
	for _, k := range legacy {
		assert.Truef(t, all[Kind(k)],
			"internal/notification declares %s and AllKinds does not; the CHECK would narrow under a package that still writes it", k)
	}
	for _, k := range ProductKinds() {
		assert.True(t, all[k], "%s is a product kind and must be in AllKinds", k)
		assert.True(t, k.IsProduct())
		assert.True(t, k.Valid())
	}
	assert.Len(t, ProductKinds(), 13)
	assert.Len(t, AllKinds(), 21, "13 product kinds plus the 8 legacy names that are not also product kinds")
	assert.False(t, Kind("NOPE").Valid())
	assert.False(t, Kind("FUNDING_AVAILABLE").IsProduct())
}

func TestSeverities_MirrorTheColumn(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []Severity{SeverityInfo, SeverityWarn, SeverityCritical}, AllSeverities())
	for _, s := range AllSeverities() {
		assert.True(t, s.Valid())
	}
	assert.False(t, Severity("URGENT").Valid())
}

// TestSuppressible_TheFiveThatCannotBeSwitchedOff pins the product judgement,
// because a change to it is a change to what "I turned that off" can mean.
func TestSuppressible_TheFiveThatCannotBeSwitchedOff(t *testing.T) {
	t.Parallel()
	var fixed []Kind
	for _, k := range ProductKinds() {
		if !k.Suppressible() {
			fixed = append(fixed, k)
		}
	}
	assert.Equal(t, []Kind{
		KindCreditPurchaseReversed, KindPayoutFailed,
		KindAccountRestricted, KindSecurityNewSession, KindSystem,
	}, fixed)
}

func TestDedupKey_IsTheKindRefAndOccurrenceAndNothingElse(t *testing.T) {
	t.Parallel()
	ref := Ref{Type: "credit_funding", ID: "f-1"}
	assert.Equal(t, "CREDIT_PURCHASE_CAPTURED|credit_funding|f-1|t-1",
		DedupKey(KindCreditPurchaseCaptured, ref, "t-1"))
	// Same fact, twice: one key.
	assert.Equal(t, DedupKey(KindCreditPurchaseCaptured, ref, "t-1"),
		DedupKey(KindCreditPurchaseCaptured, ref, "t-1"))
	// A different occurrence of the same ref is a different notification.
	assert.NotEqual(t, DedupKey(KindCreditPurchaseCaptured, ref, "t-1"),
		DedupKey(KindCreditPurchaseCaptured, ref, "t-2"))

	long := DedupKey(KindSystem, Ref{Type: strings.Repeat("x", 300), ID: "y"}, "z")
	assert.LessOrEqual(t, len(long), maxDedupKey)
	assert.True(t, strings.HasPrefix(long, "SYSTEM|sha256:"))
	assert.NotEqual(t, long, DedupKey(KindSystem, Ref{Type: strings.Repeat("x", 300), ID: "y2"}, "z"))
}

func TestValidate_RefusesWhatCannotBeDeduplicatedOrAddressed(t *testing.T) {
	t.Parallel()
	base := Notification{
		UserID: aUser(), Kind: KindSystem, Severity: SeverityInfo,
		Title: "t", Body: "b", Occurrence: "o",
	}
	require.NoError(t, base.Validate())

	cases := map[string]func(n Notification) Notification{
		"no recipient":      func(n Notification) Notification { n.UserID = accounts.UserID{}; return n },
		"unknown kind":      func(n Notification) Notification { n.Kind = "NOPE"; return n },
		"a legacy kind":     func(n Notification) Notification { n.Kind = "FUNDING_AVAILABLE"; return n },
		"unknown severity":  func(n Notification) Notification { n.Severity = "URGENT"; return n },
		"no title":          func(n Notification) Notification { n.Title = "  "; return n },
		"no body":           func(n Notification) Notification { n.Body = ""; return n },
		"nothing to dedupe": func(n Notification) Notification { n.Occurrence = ""; return n },
		"half a ref":        func(n Notification) Notification { n.Ref = Ref{Type: "x"}; return n },
		"data that is not JSON": func(n Notification) Notification {
			n.Data = []byte("{not json")
			return n
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			err := mutate(base).Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// TestEmit_RefusesASandboxLabelOffASandboxTier: ADR-0023 requires a sandbox
// outcome to exist only on a sandbox tier and to be labelled everywhere it is
// stored. Both directions are enforced here rather than by a caller
// remembering: a producer that is not a sandbox tier refuses the label, and one
// that is stamps it whether or not the caller asked.
func TestEmit_RefusesASandboxLabelOffASandboxTier(t *testing.T) {
	t.Parallel()
	p := NewProducer(func() time.Time { return time.Unix(0, 0).UTC() }, false)
	_, err := p.Emit(context.Background(), nil, Notification{
		UserID: aUser(), Kind: KindSystem, Title: "t", Body: "b", Occurrence: "o", Sandbox: true,
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "sandbox tier")
}

// TestEmit_ValidatesBeforeItTouchesTheTransaction: a malformed notification is
// refused without a statement, so a caller cannot poison their own transaction
// by asking for one.
func TestEmit_ValidatesBeforeItTouchesTheTransaction(t *testing.T) {
	t.Parallel()
	p := NewProducer(nil, false)
	_, err := p.Emit(context.Background(), nil, Notification{UserID: aUser(), Kind: "NOPE", Title: "t", Body: "b", Occurrence: "o"})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// A well-formed one reaches the point where the transaction is required,
	// and says so rather than panicking.
	_, err = p.Emit(context.Background(), nil, Notification{UserID: aUser(), Kind: KindSystem, Title: "t", Body: "b", Occurrence: "o"})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
}

func TestCursor_RoundTripsAndRefusesAForgedOne(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 11, 12, 13, 456000000, time.UTC)
	rowID := aUser().String() // any UUIDv7 in canonical form
	c := encodeCursor(at, rowID)
	assert.NotContains(t, c, rowID, "the cursor is opaque; a client must not be able to read an id out of it")

	gotAt, gotID, err := decodeCursor(c)
	require.NoError(t, err)
	assert.Equal(t, rowID, gotID)
	assert.True(t, at.Equal(gotAt), "want %s got %s", at, gotAt)

	emptyAt, emptyID, err := decodeCursor("")
	require.NoError(t, err)
	assert.True(t, emptyAt.IsZero())
	assert.Empty(t, emptyID)

	for _, bad := range []string{"not-base64!!", "e30", "!!!!", encodeCursor(at, "not-a-uuid")} {
		_, _, err := decodeCursor(bad)
		require.Errorf(t, err, "cursor %q must be refused", bad)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	}
}

func TestSourceNames_AreTheTablesTheFollowerReads(t *testing.T) {
	t.Parallel()
	f := NewFollower(NewProducer(nil, false))
	assert.Equal(t, []string{
		"credit_funding_transitions",
		"payout_request_transitions",
		"native_market_fills",
		"native_market_transitions",
		"account_status_transitions",
		"security_events_login",
	}, f.SourceNames())
}

// TestKeysetOn_TakesTheWholeInstantWhenThereIsNoTieBreaker: the lap back lands
// on an instant no row id was recorded for, and the predicate has to admit
// every row at that instant rather than none.
func TestKeysetOn_TakesTheWholeInstantWhenThereIsNoTieBreaker(t *testing.T) {
	t.Parallel()
	pred := keysetOn("t.occurred_at", "t.id")
	assert.Contains(t, pred, "t.occurred_at > $1::timestamptz")
	assert.Contains(t, pred, "$2::text IS NOT NULL")
	assert.Nil(t, nullable(""))
	assert.Equal(t, any("abc"), nullable("abc"))
}

// TestStateMappings_CoverEveryStateTheColumnAdmitsOrDeliberatelyDoNot keeps the
// follower's opinion about which states are news explicit. A state that is
// neither mapped nor listed here is one nobody decided about.
func TestStateMappings_CoverEveryStateTheColumnAdmitsOrDeliberatelyDoNot(t *testing.T) {
	t.Parallel()
	creditSilent := []string{"CREATED", "AUTHORIZATION_PENDING", "AUTHORIZED", "CAPTURE_PENDING", "REVERSIBLE", "SETTLED"}
	for _, s := range creditSilent {
		_, mapped := creditFundingKinds[s]
		assert.Falsef(t, mapped, "%s is listed as deliberately silent and is also mapped", s)
	}
	assert.Len(t, creditFundingKinds, 5)
	assert.Len(t, creditFundingKinds, 11-len(creditSilent), "every credit funding state is either mapped or deliberately silent")

	payoutSilent := []string{"DRAFT", "ELIGIBILITY_CHECK", "VERIFICATION_PENDING", "VERIFIED"}
	for _, s := range payoutSilent {
		_, mapped := payoutKinds[s]
		assert.Falsef(t, mapped, "%s is listed as deliberately silent and is also mapped", s)
	}
	assert.Len(t, payoutKinds, 13-len(payoutSilent), "every payout state is either mapped or deliberately silent")

	for _, k := range creditFundingKinds {
		assert.True(t, k.IsProduct())
	}
	for _, k := range payoutKinds {
		assert.True(t, k.IsProduct())
	}
	assert.Equal(t, []string{"CLOSE_ONLY", "DELISTED", "FROZEN", "HALTED"}, sortedCopy(pausedStatuses))
	assert.Equal(t, []string{"CLOSED", "FROZEN", "RESTRICTED"}, sortedCopy(restrictedStatuses))
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sortStrings(out)
	return out
}

func TestKeysOf_IsDeterministic(t *testing.T) {
	t.Parallel()
	assert.Equal(t, keysOf(creditFundingKinds), keysOf(creditFundingKinds))
	assert.Equal(t, []string{"CAPTURED", "DISPUTED", "FAILED", "REFUNDED", "REVERSED"}, keysOf(creditFundingKinds))
}
