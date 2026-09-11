//go:build integration

package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Two admin action kinds with live executors and no test at all (F-80).
//
// F-69's inventory listed four: ENVELOPE_AUTHORITY_CHANGE, WITHDRAWAL_APPROVE,
// NATIVE_ASSET_DELIST and PAYOUT_MANUAL_REVIEW_RESOLVE. The first two execute
// nothing yet -- their subsystems are unreachable, which is F-34's territory
// and recorded there. These two run real code against real state.
//
// Grepping for the constant AND the literal across every *_test.go found
// neither. That is worse than an untested branch: an executor is what turns two
// signatures into an effect, and nothing had ever run these two.

// TestIntegration_DelistingALiveAssetGoesThroughTheStatusTable.
//
// Delist is deliberately single-signature: it takes an asset OUT of the
// tradable set, and a control that only ever stops things is one an operator
// must be able to reach alone.
//
// Running it for the first time is what showed the more interesting half. An
// ACTIVE asset cannot be delisted at all: `statusTransitions` sends a live
// asset through CLOSE_ONLY or HALTED first, so holders are either given the
// chance to exit or the halt is a recorded decision somebody has to make. An
// approval does not create an edge the subsystem does not have.
func TestIntegration_DelistingALiveAssetGoesThroughTheStatusTable(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	compliance := seedOperator(t, d, security.RoleCompliance)

	spec, ok := admin.Spec(admin.KindNativeAssetDelist)
	require.True(t, ok)
	require.False(t, spec.RequiresDual, "this test is written for the single-signature form of the kind")
	require.Equal(t, nativeasset.StatusActive, h.assetStatus(t, h.asset.AssetID))

	// Straight from ACTIVE: refused, with two signatures or none.
	action := proposeWithParams(t, h.as(&compliance), admin.KindNativeAssetDelist,
		"native_asset", h.asset.AssetID.String(), nil)
	res := execute(h.as(&compliance), action.ID)
	require.Equal(t, http.StatusConflict, res.Code, "a live asset was delisted in one step; body=%s", res.Body.String())
	assert.Equal(t, nativeasset.StatusActive, h.assetStatus(t, h.asset.AssetID))

	// Stepped down first, and then it goes.
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, serr := h.nativeAssets.SetStatus(ctx, tx, h.asset.AssetID, nativeasset.StatusHalted, "incident")
			return serr
		}))

	action = proposeWithParams(t, h.as(&compliance), admin.KindNativeAssetDelist,
		"native_asset", h.asset.AssetID.String(), nil)
	res = execute(h.as(&compliance), action.ID)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, nativeasset.StatusDelisted, h.assetStatus(t, h.asset.AssetID))

	// And it is terminal: DELISTED has no outgoing edge, and an approval does
	// not create one either.
	err := h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, serr := h.nativeAssets.SetStatus(ctx, tx, h.asset.AssetID, nativeasset.StatusActive, "put it back")
			return serr
		})
	require.Error(t, err, "a delisted asset was relisted")
}

// TestIntegration_ADelistTargetingSomethingElseIsRefused: the executor reads
// the TARGET of the action, not its params, so an action whose target is not an
// asset id must be refused rather than parsed into whatever it happens to be.
func TestIntegration_ADelistTargetingSomethingElseIsRefused(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	compliance := seedOperator(t, d, security.RoleCompliance)

	action := proposeWithParams(t, h.as(&compliance), admin.KindNativeAssetDelist,
		"native_asset", "not-an-asset-id", nil)
	res := execute(h.as(&compliance), action.ID)
	require.NotEqual(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	assert.Equal(t, nativeasset.StatusActive, h.assetStatus(t, h.asset.AssetID), "the real asset was touched")
}

// TestIntegration_ResolvingAStuckPayoutTakesTwoPeople.
//
// PAYOUT_MANUAL_REVIEW_RESOLVE is the one Domain A kind that is dual-controlled
// in both directions, because resolving a payout by hand decides what happens
// to money somebody is waiting for.
func TestIntegration_ResolvingAStuckPayoutTakesTwoPeople(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	reviewer := seedOperator(t, d, security.RoleCompliance)

	spec, ok := admin.Spec(admin.KindPayoutManualReviewResolve)
	require.True(t, ok)
	require.True(t, spec.RequiresDual)

	req := h.stuckPayout(t)
	require.Equal(t, payout.StateManualReview, req.State)

	action := proposeWithParams(t, h.as(&reviewer), admin.KindPayoutManualReviewResolve,
		"payout_request", req.ID.String(), map[string]any{"resolution": "FAIL"})

	// One signature is not enough, and the payout stays where it is.
	res := execute(h.as(&reviewer), action.ID)
	require.NotEqual(t, http.StatusOK, res.Code, "an unapproved resolution executed; body=%s", res.Body.String())
	assert.Equal(t, payout.StateManualReview, h.payoutState(t, req.ID))

	// The proposer cannot approve their own, however elevated.
	self := elevate(reviewer, testNow.Add(time.Hour))
	res = decide(h.as(&self), action.ID, "approve", "approving my own")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, payout.StateManualReview, h.payoutState(t, req.ID))

	// A second person can, and then it executes.
	approver := elevate(seedOperator(t, d, security.RoleCompliance), testNow.Add(time.Hour))
	res = decide(h.as(&approver), action.ID, "approve", "provider confirmed no payment was made")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	res = execute(h.as(&reviewer), action.ID)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, payout.StateFailed, h.payoutState(t, req.ID))
}

// TestIntegration_NoResolutionDeclaresAPayoutSettled is the property the
// executor's own comment names: the provider is authoritative for settlement,
// and an operator who could assert it by hand could close a ticket by claiming
// money moved.
func TestIntegration_NoResolutionDeclaresAPayoutSettled(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	reviewer := seedOperator(t, d, security.RoleCompliance)
	req := h.stuckPayout(t)

	for _, resolution := range []string{"SETTLED", "PAID", "COMPLETE", ""} {
		t.Run("refuses/"+resolution, func(t *testing.T) {
			action := proposeWithParams(t, h.as(&reviewer), admin.KindPayoutManualReviewResolve,
				"payout_request", req.ID.String(), map[string]any{"resolution": resolution})
			approver := elevate(seedOperator(t, d, security.RoleCompliance), testNow.Add(time.Hour))
			require.Equal(t, http.StatusOK, decide(h.as(&approver), action.ID, "approve", "second pair of eyes").Code)

			res := execute(h.as(&reviewer), action.ID)
			require.NotEqual(t, http.StatusOK, res.Code, "resolution %q executed; body=%s", resolution, res.Body.String())
			assert.Equal(t, payout.StateManualReview, h.payoutState(t, req.ID), "the payout moved")
		})
	}

	// The control: a declared resolution, fully approved, does execute. Without
	// it every case above could be failing on approval or on the target rather
	// than on the resolution.
	action := proposeWithParams(t, h.as(&reviewer), admin.KindPayoutManualReviewResolve,
		"payout_request", req.ID.String(), map[string]any{"resolution": "REJECT"})
	approver := elevate(seedOperator(t, d, security.RoleCompliance), testNow.Add(time.Hour))
	require.Equal(t, http.StatusOK, decide(h.as(&approver), action.ID, "approve", "declined on policy").Code)
	require.Equal(t, http.StatusOK, execute(h.as(&reviewer), action.ID).Code)
	assert.Equal(t, payout.StateRejected, h.payoutState(t, req.ID))
}

// payoutCreatorEarnings is the capability a creator-earnings payout needs.
const payoutCreatorEarnings valuedomain.CapabilityKey = "PAYOUT_CREATOR_EARNINGS"

// stuckPayout mints Credits to the creator, adds a verified destination,
// requests a payout and flags it for manual review -- the state an operator
// finds when the provider's answer never arrived.
func (h *domainAHarness) stuckPayout(t *testing.T) payout.Request {
	t.Helper()
	ctx := context.Background()
	var (
		dest payout.Destination
		req  payout.Request
	)
	require.NoError(t, h.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := h.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: h.creator, Quantity: qq("1000"),
				Origin: valuedomain.OriginCreatorEarning, Finality: valuedomain.FinalitySettled,
				Reference:      credit.Reference{Type: "test_issue", ID: uuid.NewString()},
				IdempotencyKey: "issue-" + uuid.NewString(),
				Reason:         "creator earnings for the payout test", EffectiveAt: h.clk.Now(),
			}); err != nil {
				return err
			}
			var err error
			dest, err = h.payouts.CreateDestination(ctx, tx, payout.Destination{
				ID: payout.NewDestinationID(), AccountID: h.creator, Kind: payout.DestinationBank,
				Provider: "sandbox", ProviderReference: "dest-" + uuid.NewString(),
				DisplayLabel: "Test bank", Currency: "USD",
			})
			if err != nil {
				return err
			}
			_, err = h.payouts.SetDestinationStatus(ctx, tx, dest.ID, payout.DestinationVerified)
			return err
		}))

	policy := valuedomain.DefaultPolicy()
	policy.Version = "httpapi-itest-creator-earnings-v1"
	// A rule with no RequiredCapability is POLICY_INVALID, and the capability
	// has to be active as well: the precondition assertion below reported both
	// (INSUFFICIENT_ELIGIBLE_VALUE, POLICY_INVALID,
	// REQUIRED_CAPABILITY_NOT_ACTIVE) rather than leaving the failure to land
	// somewhere unrelated.
	policy.Rules[valuedomain.OriginCreatorEarning] = valuedomain.OriginRule{
		PayoutAllowed:        true,
		RequiredCapability:   payoutCreatorEarnings,
		RequiredVerification: valuedomain.VerificationPayoutKYC,
	}
	var decision payout.Decision
	require.NoError(t, h.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			// A payout names the quote the customer was shown (D-119).
			quote, qerr := h.payouts.Quote(ctx, tx, payout.QuoteRequest{
				AccountID: h.creator, DestinationID: dest.ID, Quantity: qq("400"),
				CreditsPerMajorUnit:    1,
				MinorUnitsPerMajorUnit: 100,
				CreditDecimals:         0,
				PricingVersion:         "httpapi-itest-pricing-v1",
				PolicyVersion:          policy.Version,
				Currency:               "USD",
				Environment:            "TEST",
				DisclosureAccepted:     true,
				IdempotencyKey:         "quote-" + uuid.NewString(),
				Now:                    h.clk.Now(),
			}, dest)
			if qerr != nil {
				return qerr
			}
			req, decision, err = h.payouts.Create(ctx, tx, payout.CreateRequest{
				AccountID: h.creator, DestinationID: &dest.ID, QuoteID: &quote.ID, Quantity: qq("400"),
				ProviderTerms:      payout.TermsFrom(h.payoutProv.Capabilities()),
				DisclosureAccepted: true,
				IdempotencyKey:     "payout-" + uuid.NewString(), EffectiveAt: h.clk.Now(),
			}, payout.EligibilityInput{
				Policy: policy, Verified: valuedomain.VerificationPayoutKYC,
				ActiveCaps:          map[valuedomain.CapabilityKey]bool{payoutCreatorEarnings: true},
				Now:                 h.clk.Now().Add(48 * time.Hour),
				DestinationVerified: true, ProviderSupports: true,
				SanctionsState:        compliance.SanctionsClear,
				JurisdictionSupported: true,
			})
			return err
		}))

	// Create records a REJECTED request when the decision denies, rather than
	// erroring, so a helper that ignored the decision would hand every test
	// below it a payout in the wrong state and fail somewhere unrelated. It did:
	// "a payout cannot go REJECTED -> MANUAL_REVIEW".
	require.True(t, decision.Sufficient(),
		"the payout was not eligible, so there is nothing to get stuck: eligible=%s requested=%s reasons=%v",
		decision.Eligible, decision.Requested, decision.ReasonStrings())

	require.NoError(t, h.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			req, err = h.payouts.FlagForManualReview(ctx, tx, req.ID,
				"the provider's answer never arrived")
			return err
		}))
	return req
}

func (h *domainAHarness) payoutState(t *testing.T, id payout.RequestID) payout.State {
	t.Helper()
	got, err := h.payouts.Get(context.Background(), h.db, id)
	require.NoError(t, err)
	return got.State
}
