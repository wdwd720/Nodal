//go:build integration

package credit

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/webhook"
)

// A funding parked for a human is not un-parked by the next webhook (F-100).
//
// fundingTransitions[MANUAL_REVIEW] lists ten destinations, and the comment
// above it says why: "an operator resolving a review may send the funding
// anywhere a provider event could legitimately have sent it". The reasoning is
// careful -- it even excludes SETTLED, because settlement is a fact about a
// clock and an operator who could assert it by hand could make value
// payout-eligible by closing a ticket.
//
// The premise underneath it was not true. Dispatch consults the SAME table, so
// every destination written for a human was also a destination the next
// provider event could take. MANUAL_REVIEW -> CAPTURED is one of them, and
// CAPTURED is the edge that mints.

func TestIntegration_AParkedFundingIsNotUnparkedByTheNextWebhook(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "f100-mint", 10000)
	ref := p.Funding.ProviderReference

	// The payment is in flight.
	require.Equal(t, webhook.Applied, f.deliver(t, event(ref, PurchaseProcessing, "f100-a", 10000)))
	require.Equal(t, FundingCapturePending, f.funding(t, p.Funding.ID).State)

	// The refund lands FIRST. Providers do not order their deliveries, and a
	// fraud auto-refund can fire while the success delivery is being retried.
	// CAPTURE_PENDING -> REFUNDED is not a legal transition and REFUNDED is
	// absent from the isBackwards rank map, so this parks for a person.
	require.Equal(t, webhook.Applied, f.deliver(t, event(ref, PurchaseRefunded, "f100-b", 10000)))
	require.Equal(t, FundingManualReview, f.funding(t, p.Funding.ID).State,
		"a refund that cannot be applied parks the funding")

	// Applied, not an error -- so the inbox marks it PROCESSED and the refund
	// will never be redelivered. That is deliberate and it is why the next
	// event must not be allowed to overwrite what this one recorded.

	// Now the success arrives.
	d := f.deliver(t, event(ref, PurchaseSucceeded, "f100-c", 10000))

	got := f.funding(t, p.Funding.ID)
	assert.Equal(t, FundingManualReview, got.State,
		"a provider event moved a funding out of the review it was parked in; "+
			"the only thing that may resolve MANUAL_REVIEW is a person")
	assert.Nil(t, got.LotID, "Credits were minted for a payment that was refunded")
	assert.Equal(t, "0", f.balances(t).Gross.String(),
		"the refund was consumed and the balance says the money is ours")
	assert.Equal(t, webhook.Applied, d,
		"the event is still recorded rather than retried forever")
}

func TestIntegration_AParkedFundingIsNotFrozenOrSettledByTheNextWebhook(t *testing.T) {
	f := newPurchaseFixture(t)

	// The same shape for the two other economic effects reachable out of
	// MANUAL_REVIEW. Neither destroys value here, but both let a provider event
	// decide something a person was asked to decide.
	for _, tc := range []struct {
		name   string
		status PurchaseStatus
	}{
		{"a dispute does not resolve the review", PurchaseDisputed},
		{"a chargeback does not resolve the review", PurchaseChargeback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := f.start(t, "f100-"+string(tc.status), 10000)
			ref := p.Funding.ProviderReference

			require.Equal(t, webhook.Applied, f.deliver(t, event(ref, PurchaseProcessing, "f100-p-"+string(tc.status), 10000)))
			require.Equal(t, webhook.Applied, f.deliver(t, event(ref, PurchaseRefunded, "f100-r-"+string(tc.status), 10000)))
			require.Equal(t, FundingManualReview, f.funding(t, p.Funding.ID).State)

			require.Equal(t, webhook.Applied, f.deliver(t, event(ref, tc.status, "f100-x-"+string(tc.status), 10000)))
			assert.Equal(t, FundingManualReview, f.funding(t, p.Funding.ID).State,
				"a %s event resolved a review a person had not seen", tc.status)
		})
	}
}

// The control. A funding that was never parked still moves normally, so the
// refusal above is about MANUAL_REVIEW and not about Dispatch in general.
func TestIntegration_AnUnparkedFundingStillMintsOnCapture(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "f100-control", 10000)
	ref := p.Funding.ProviderReference

	require.Equal(t, webhook.Applied, f.deliver(t, event(ref, PurchaseProcessing, "f100-ca", 10000)))
	require.Equal(t, webhook.Applied, f.deliver(t, event(ref, PurchaseSucceeded, "f100-cb", 10000)))

	got := f.funding(t, p.Funding.ID)
	require.Equal(t, FundingReversible, got.State, "capture mints and moves to REVERSIBLE")
	require.NotNil(t, got.LotID)
	require.Equal(t, "10000", f.balances(t).Gross.String())
}

// The other half of F-100. Refusing to let a webhook resolve a review is only
// safe if something else can: without an operator path a parked funding stays
// parked forever, with its money counted against the at-risk ceiling for the
// life of the deployment -- which is F-90's failure returning through a
// different door.

func TestIntegration_AnOperatorResolutionIsTheWayOutOfReview(t *testing.T) {
	f := newPurchaseFixture(t)

	park := func(t *testing.T, key string) FundingID {
		t.Helper()
		p := f.start(t, key, 10000)
		ref := p.Funding.ProviderReference
		require.Equal(t, webhook.Applied, f.deliver(t, event(ref, PurchaseProcessing, key+"-a", 10000)))
		require.Equal(t, webhook.Applied, f.deliver(t, event(ref, PurchaseRefunded, key+"-b", 10000)))
		require.Equal(t, FundingManualReview, f.funding(t, p.Funding.ID).State)
		return p.Funding.ID
	}
	resolve := func(t *testing.T, id FundingID, r ManualResolution) (Funding, error) {
		t.Helper()
		var out Funding
		err := f.tx(func(tx pgx.Tx) error {
			var rerr error
			out, rerr = f.svcP.ResolveManualReview(f.ctx, tx, id, r, "operator looked at the provider", "approval-1")
			return rerr
		})
		return out, err
	}

	t.Run("resolving to FAILED closes the review and mints nothing", func(t *testing.T) {
		id := park(t, "f100-res-failed")
		got, err := resolve(t, id, ResolutionFailed)
		require.NoError(t, err)
		assert.Equal(t, FundingFailed, got.State)
		assert.Nil(t, got.LotID, "a payment that never completed mints nothing")
	})

	t.Run("resolving to CAPTURED mints, because a person said the money arrived", func(t *testing.T) {
		id := park(t, "f100-res-captured")
		got, err := resolve(t, id, ResolutionCaptured)
		require.NoError(t, err)
		// CAPTURED mints and the mint moves it on to REVERSIBLE, exactly as a
		// provider capture would -- the effect runs through the same apply.
		assert.Equal(t, FundingReversible, got.State)
		assert.NotNil(t, got.LotID)
	})

	t.Run("a funding that is not parked has no review to resolve", func(t *testing.T) {
		p := f.start(t, "f100-res-unparked", 10000)
		_, err := resolve(t, p.Funding.ID, ResolutionFailed)
		require.Error(t, err)
		assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	})

	t.Run("there is no resolution that declares a funding settled", func(t *testing.T) {
		id := park(t, "f100-res-settled")
		_, err := resolve(t, id, ManualResolution("SETTLED"))
		require.Error(t, err, "an operator could make value payout-eligible by closing a ticket")
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		assert.Equal(t, FundingManualReview, f.funding(t, id).State, "and the funding stays parked")
	})

	t.Run("every resolution names a state the review may legally reach", func(t *testing.T) {
		// The pairing between the resolution vocabulary and the transition
		// table, checked rather than assumed: a resolution naming a state
		// MANUAL_REVIEW cannot reach would be a control that always refuses.
		for _, r := range AllManualResolutions() {
			assert.True(t, CanTransitionFunding(FundingManualReview, r.fundingState()),
				"resolution %s names %s, which MANUAL_REVIEW cannot reach", r, r.fundingState())
		}
		assert.NotContains(t, fundingTransitions[FundingManualReview], FundingSettled,
			"and SETTLED is not among them, so no resolution could be added for it by accident")
	})
}
