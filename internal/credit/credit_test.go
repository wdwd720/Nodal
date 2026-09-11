package credit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

func anAccount() accounts.AccountID { return accounts.NewAccountID() }

func newTestTxID() ledger.TransactionID { return ledger.NewTransactionID() }
func zeroTxID() ledger.TransactionID    { return ledger.TransactionID{} }

// ---------------------------------------------------------------------------
// Consumption ordering
// ---------------------------------------------------------------------------

func TestConsumptionRank_CoversEveryOriginAndIsATotalOrder(t *testing.T) {
	seen := map[int]valuedomain.CreditOrigin{}
	for _, o := range valuedomain.AllOrigins() {
		r := ConsumptionRank(o)
		require.Less(t, r, len(valuedomain.AllOrigins()),
			"origin %s has no declared rank and would sort with unknown origins", o)
		if prev, dup := seen[r]; dup {
			t.Fatalf("origins %s and %s share rank %d; the order must be total", prev, o, r)
		}
		seen[r] = o
	}
}

func TestConsumptionRank_RestrictedValueIsSpentBeforeEarnedValue(t *testing.T) {
	require.Less(t, ConsumptionRank(valuedomain.OriginPromotional), ConsumptionRank(valuedomain.OriginPurchased),
		"a promotional grant is spent before money the user paid")
	require.Less(t, ConsumptionRank(valuedomain.OriginPurchased), ConsumptionRank(valuedomain.OriginCreatorEarning),
		"purchased Credits are spent before a creator's earnings")
	require.Less(t, ConsumptionRank(valuedomain.OriginMarketTradingProceeds), ConsumptionRank(valuedomain.OriginCreatorEarning),
		"speculative proceeds are spent before earnings")
}

func TestConsumptionRank_AnUnknownOriginSortsLast(t *testing.T) {
	require.Equal(t, len(valuedomain.AllOrigins()), ConsumptionRank("SOME_FUTURE_ORIGIN"),
		"a newly added origin must be preserved rather than spent first")
}

// TestConsumptionOrderSQL_MatchesTheMap is what lets the SQL be a constant.
//
// The constant is what runs, because test/security proves every statement in
// the repository is built from constants and a value assembled at init is not
// provably one. The Go map stays the authority on the ordering, and this
// asserts the constant implements it exactly -- so the constant cannot drift
// from the rank it is supposed to encode.
func TestConsumptionOrderSQL_MatchesTheMap(t *testing.T) {
	require.Equal(t, buildConsumptionOrderSQL(), ConsumptionOrderSQL(),
		"the ORDER BY constant no longer matches consumptionRank; regenerate it")
}

func TestConsumptionOrderSQL_MentionsEveryOriginExactlyOnce(t *testing.T) {
	sql := ConsumptionOrderSQL()
	for _, o := range valuedomain.AllOrigins() {
		require.Contains(t, sql, "'"+string(o)+"'")
	}
	require.Contains(t, sql, "ELSE")
	require.True(t, len(sql) > 0)
}

// ---------------------------------------------------------------------------
// Funding state machine
// ---------------------------------------------------------------------------

func TestFundingState_AllDeclaredStates(t *testing.T) {
	// PART XI named eleven. Two more were added by the Stripe provider
	// workstream and each one is here because folding it into an existing
	// state loses something real:
	//
	//   CANCELED      -- an abandoned checkout is not a declined card, and a
	//                    support queue that cannot tell them apart chases
	//                    customers who did nothing wrong.
	//   MANUAL_REVIEW -- an unmapped provider status must be able to stop,
	//                    because the alternative is guessing which state it
	//                    meant and acting on the guess.
	require.Len(t, AllFundingStates(), 13)
	for _, s := range AllFundingStates() {
		require.True(t, s.Valid(), "%s", s)
	}
	require.False(t, FundingState("PAID").Valid())
}

func TestFundingState_ReviewCannotBeResolvedToSettled(t *testing.T) {
	// The one resolution an operator must not have. SETTLED means the dispute
	// window closed, which is a fact about a clock and a policy -- not
	// something a person establishes by closing a ticket. An operator who
	// could assert it by hand could make value payout-eligible at will.
	require.False(t, CanTransitionFunding(FundingManualReview, FundingSettled),
		"resolving a review to SETTLED would let an operator make value payout-eligible by hand")
	require.True(t, CanTransitionFunding(FundingManualReview, FundingReversible),
		"the honest resolution is back onto the path that reaches SETTLED through the hold policy")
}

func TestFundingState_ProviderMayReportOutOfOrder(t *testing.T) {
	// A Stripe PaymentIntent with automatic capture never reports
	// requires_capture: it goes requires_payment_method -> succeeded. And any
	// delivery may be lost or re-ordered. A table that only allows one step at
	// a time would jam a funding on either of those ordinary events.
	require.True(t, CanTransitionFunding(FundingAuthorizationPending, FundingCaptured),
		"automatic capture skips the authorized states entirely")
	require.True(t, CanTransitionFunding(FundingCreated, FundingCaptured),
		"the first delivery we see may be the last one that happened")

	// What the skip must never do is run backwards, or skip the mint edge.
	require.False(t, CanTransitionFunding(FundingCaptured, FundingAuthorized))
	require.False(t, CanTransitionFunding(FundingCaptured, FundingCreated))
	require.False(t, CanTransitionFunding(FundingReversible, FundingCaptured))
	require.False(t, CanTransitionFunding(FundingCreated, FundingReversible),
		"Credits are minted on the CAPTURED -> REVERSIBLE edge and nothing may skip it")
}

func TestFundingState_MintedSaysWhetherCreditsExist(t *testing.T) {
	for _, s := range []FundingState{
		FundingCreated, FundingAuthorizationPending, FundingAuthorized,
		FundingCapturePending, FundingCaptured, FundingFailed, FundingCanceled,
		FundingManualReview,
	} {
		require.False(t, s.Minted(), "no Credits exist in %s", s)
	}
	for _, s := range []FundingState{
		FundingReversible, FundingSettled, FundingDisputed, FundingReversed, FundingRefunded,
	} {
		require.True(t, s.Minted(), "Credits exist in %s", s)
	}
	// CAPTURED is the interesting one: the money arrived and the Credits have
	// not been issued yet. That gap is the whole point of having a separate
	// mint edge, and it is why Minted() is not "the payment succeeded".
	require.False(t, FundingCaptured.Minted())
}

func TestFundingState_CapturedIsNotSettled(t *testing.T) {
	// The whole reason the state machine has eleven states rather than three.
	require.True(t, CanTransitionFunding(FundingCaptured, FundingReversible))
	require.False(t, CanTransitionFunding(FundingCaptured, FundingSettled),
		"a captured payment reaches SETTLED only by passing through REVERSIBLE")

	fin, ok := LotFinalityFor(FundingReversible)
	require.True(t, ok)
	require.Equal(t, valuedomain.FinalityReversible, fin)
	require.False(t, fin.PayoutEligible(), "captured value must never be payout-eligible")
	require.True(t, fin.Spendable(), "captured value must be spendable; that is the product")
}

func TestFundingState_TerminalStatesGoNowhere(t *testing.T) {
	for _, s := range []FundingState{FundingReversed, FundingRefunded, FundingFailed, FundingCanceled} {
		require.True(t, s.Terminal(), "%s", s)
		for _, to := range AllFundingStates() {
			require.False(t, CanTransitionFunding(s, to),
				"%s is terminal but claims it can become %s", s, to)
		}
	}
	require.False(t, FundingReversible.Terminal())
	require.False(t, FundingSettled.Terminal())
}

func TestFundingState_SettledCanStillBeDisputed(t *testing.T) {
	require.True(t, CanTransitionFunding(FundingSettled, FundingDisputed),
		"a card network can dispute a payment after a processor calls it settled; refusing the transition would leave the system unable to record something that already happened")
	require.True(t, CanTransitionFunding(FundingDisputed, FundingSettled),
		"a dispute the platform wins must resolve back")
	require.True(t, CanTransitionFunding(FundingDisputed, FundingReversed))
	require.False(t, CanTransitionFunding(FundingSettled, FundingReversed),
		"settled funding is reversed only by first being disputed, so the reversal always has a dispute record")
}

func TestFundingState_LotFinalityIsOnlyDefinedOnceCreditsExist(t *testing.T) {
	for _, s := range []FundingState{
		FundingCreated, FundingAuthorizationPending, FundingAuthorized,
		FundingCapturePending, FundingCaptured, FundingFailed,
	} {
		_, ok := LotFinalityFor(s)
		require.False(t, ok, "%s has minted nothing, so it implies no lot finality", s)
	}
	for _, s := range []FundingState{FundingReversible, FundingSettled, FundingDisputed, FundingReversed, FundingRefunded} {
		fin, ok := LotFinalityFor(s)
		require.True(t, ok, "%s", s)
		require.True(t, fin.Valid())
	}
}

// TestFundingState_EveryTransitionTargetIsDeclared catches a typo in the
// transition table that would otherwise produce a state nothing can leave.
func TestFundingState_EveryTransitionTargetIsDeclared(t *testing.T) {
	for from, tos := range fundingTransitions {
		require.True(t, from.Valid(), "transition table has unknown source %q", from)
		for _, to := range tos {
			require.True(t, to.Valid(), "transition %s -> %q names an unknown state", from, to)
			require.NotEqual(t, from, to, "%s lists itself as a transition", from)
		}
	}
}

// ---------------------------------------------------------------------------
// Request validation
// ---------------------------------------------------------------------------

func validIssue() IssueRequest {
	return IssueRequest{
		AccountID:      anAccount(),
		Quantity:       money.QuantityFromInt64(100),
		Origin:         valuedomain.OriginCreatorEarning,
		Finality:       valuedomain.FinalitySettled,
		Reference:      Reference{Type: "commerce_order", ID: "abc"},
		IdempotencyKey: "k1",
		EffectiveAt:    time.Now(),
	}
}

func TestIssueRequest_Validate(t *testing.T) {
	require.NoError(t, validIssue().Validate())

	t.Run("zero quantity", func(t *testing.T) {
		r := validIssue()
		r.Quantity = money.QuantityFromInt64(0)
		require.Error(t, r.Validate())
	})
	t.Run("negative quantity", func(t *testing.T) {
		r := validIssue()
		r.Quantity = money.QuantityFromInt64(-1)
		require.Error(t, r.Validate())
	})
	t.Run("unknown origin", func(t *testing.T) {
		r := validIssue()
		r.Origin = "FREE_MONEY"
		require.Error(t, r.Validate())
	})
	t.Run("issuing already-reversed value", func(t *testing.T) {
		r := validIssue()
		r.Finality = valuedomain.FinalityReversed
		require.Error(t, r.Validate())
	})
	t.Run("no reference", func(t *testing.T) {
		r := validIssue()
		r.Reference = Reference{}
		require.Error(t, r.Validate())
	})
	t.Run("half a funding reference", func(t *testing.T) {
		r := validIssue()
		r.FundingReference = &Reference{Type: "credit_funding"}
		require.Error(t, r.Validate())
	})
}

// TestIssueRequest_OriginAndFinalityMustAgree is the check that stops a
// promotional grant from being minted as though external money backed it,
// which would make it payout-eligible the moment any policy allowed its origin.
func TestIssueRequest_OriginAndFinalityMustAgree(t *testing.T) {
	r := validIssue()
	r.Origin = valuedomain.OriginPromotional
	r.Finality = valuedomain.FinalitySettled
	err := r.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "UNFUNDED")

	r.Finality = valuedomain.FinalityUnfunded
	require.NoError(t, r.Validate())

	p := validIssue()
	p.Origin = valuedomain.OriginPurchased
	p.Finality = valuedomain.FinalityUnfunded
	require.Error(t, p.Validate(), "purchased Credits are backed by a payment and cannot be UNFUNDED")
}

func TestConsumeRequest_Validate(t *testing.T) {
	base := ConsumeRequest{
		AccountID:   anAccount(),
		Quantity:    money.QuantityFromInt64(10),
		JournalTxID: newTestTxID(),
		Reference:   Reference{Type: "spend", ID: "x"},
	}
	require.NoError(t, base.Validate())

	t.Run("no journal transaction", func(t *testing.T) {
		r := base
		r.JournalTxID = zeroTxID()
		err := r.Validate()
		require.Error(t, err, "consumption must name the posting that moved the units")
		require.Contains(t, err.Error(), "journal transaction")
	})
	t.Run("lots named without a declared restriction", func(t *testing.T) {
		// The origin filter this subtest used to check is gone: nothing set it
		// and it read an empty set as "no restriction" (F-281). What replaces
		// it is the ambiguity that mattered -- a caller that names lots and does
		// not say they are the whole set is refused rather than guessed at.
		r := base
		r.LotIDs = []LotID{NewLotID()}
		require.Error(t, r.Validate())
		r.RestrictToLots = true
		require.NoError(t, r.Validate())
	})
	t.Run("a declared restriction to nothing is a request, not a mistake", func(t *testing.T) {
		r := base
		r.RestrictToLots = true
		require.NoError(t, r.Validate(),
			"restricted to no lots is well formed; it takes nothing and fails for want of Credits")
	})
	t.Run("zero quantity", func(t *testing.T) {
		r := base
		r.Quantity = money.QuantityFromInt64(0)
		require.Error(t, r.Validate())
	})
}

func TestRestoreRequest_Validate(t *testing.T) {
	base := RestoreRequest{
		Allocations: []Allocation{{LotID: NewLotID(), Quantity: money.QuantityFromInt64(5)}},
		JournalTxID: newTestTxID(),
		Reference:   Reference{Type: "payout_cancel", ID: "x"},
	}
	require.NoError(t, base.Validate())

	t.Run("no allocations", func(t *testing.T) {
		r := base
		r.Allocations = nil
		require.Error(t, r.Validate(), "restoring a bare quantity would lose provenance")
	})
	t.Run("allocation without a lot", func(t *testing.T) {
		r := base
		r.Allocations = []Allocation{{Quantity: money.QuantityFromInt64(5)}}
		require.Error(t, r.Validate())
	})
}

func TestCreateFundingRequest_Validate(t *testing.T) {
	base := CreateFundingRequest{
		AccountID:      anAccount(),
		Provider:       "stripe",
		ProviderMode:   "sandbox",
		CreditQuantity: money.QuantityFromInt64(1000),
		PaidAmount:     money.USDFromMinor(1000),
		IdempotencyKey: "k",
	}
	require.NoError(t, base.Validate())

	t.Run("no provider", func(t *testing.T) {
		r := base
		r.Provider = "  "
		require.Error(t, r.Validate())
	})
	t.Run("no provider mode", func(t *testing.T) {
		// The sandbox label on a purchase is a fact about the payment, so it
		// has to be recorded when the payment is opened and cannot be left for
		// a later read to recompute from the configuration (D-096).
		r := base
		r.ProviderMode = ""
		require.Error(t, r.Validate())
	})
	t.Run("an invented provider mode", func(t *testing.T) {
		r := base
		r.ProviderMode = "production"
		require.Error(t, r.Validate())
	})
	t.Run("no idempotency key", func(t *testing.T) {
		r := base
		r.IdempotencyKey = ""
		require.Error(t, r.Validate(), "a retried checkout must not become a second charge")
	})
	t.Run("zero credits", func(t *testing.T) {
		r := base
		r.CreditQuantity = money.QuantityFromInt64(0)
		require.Error(t, r.Validate())
	})
	t.Run("zero paid amount is allowed", func(t *testing.T) {
		// A fully discounted purchase is legitimate; the Credits it produces
		// are still PURCHASED and still reversible if the discount was applied
		// against a payment that reverses.
		r := base
		r.PaidAmount = money.USDFromMinor(0)
		require.NoError(t, r.Validate())
	})
}

// ---------------------------------------------------------------------------
// Lot helpers
// ---------------------------------------------------------------------------

func TestLot_AgeDaysFloorsAndNeverGoesNegative(t *testing.T) {
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	l := Lot{CreatedAt: created}
	require.Equal(t, 0, l.AgeDays(created))
	require.Equal(t, 0, l.AgeDays(created.Add(23*time.Hour)))
	require.Equal(t, 1, l.AgeDays(created.Add(24*time.Hour)))
	require.Equal(t, 30, l.AgeDays(created.Add(30*24*time.Hour+time.Hour)))
	require.Equal(t, 0, l.AgeDays(created.Add(-time.Hour)),
		"a clock that went backwards must not make a lot look older than it is")
}

func TestLot_SpendableRequiresBothUnitsAndUsableFinality(t *testing.T) {
	full := Lot{Remaining: money.QuantityFromInt64(10), Finality: valuedomain.FinalitySettled}
	require.True(t, full.Spendable())

	empty := full
	empty.Remaining = money.QuantityFromInt64(0)
	require.False(t, empty.Spendable())

	disputed := full
	disputed.Finality = valuedomain.FinalityDisputed
	require.False(t, disputed.Spendable())

	reversed := full
	reversed.Finality = valuedomain.FinalityReversed
	require.False(t, reversed.Spendable())
}

func TestEligibleOrigins_DeduplicatesAndPreservesOrder(t *testing.T) {
	lots := []Lot{
		{Origin: valuedomain.OriginCreatorEarning},
		{Origin: valuedomain.OriginDataSaleEarning},
		{Origin: valuedomain.OriginCreatorEarning},
	}
	got := EligibleOrigins(lots)
	require.Equal(t, []valuedomain.CreditOrigin{
		valuedomain.OriginCreatorEarning, valuedomain.OriginDataSaleEarning,
	}, got)
	require.Empty(t, EligibleOrigins(nil))
}

func TestReference_Validity(t *testing.T) {
	require.True(t, Reference{Type: "a", ID: "b"}.Valid())
	require.False(t, Reference{Type: "a"}.Valid())
	require.False(t, Reference{ID: "b"}.Valid())
	require.False(t, Reference{}.Valid())
}
