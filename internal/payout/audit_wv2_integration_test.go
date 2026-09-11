//go:build integration

package payout_test

// Reproductions for the SECOND round of the withdrawal-verification audit
// (goal §54, "repeat until findings flatten"). Every test here is expected to
// FAIL on 92280c6, and each asserts the invariant the migrations and the
// decision register claim, so a fix makes it pass rather than making it moot.
//
// Nothing here changes product code.
//
// One ordering note for whoever runs these: F-wv2-5 below settles a payout,
// and TestAuditWV_TheProviderIsToldTheAmountAndTheDestination asserts that the
// PLATFORM-scoped PAYOUT_SETTLED balance equals exactly what ITS payout
// settled. That assertion is only true when no other test in the package
// settles one, and `scripts/inttest` gives the package a single database, so
// the two are order-coupled. The coupling is in the round-one assertion, not
// in the product; it is recorded here so a red run is not misread.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/provider/verifysandbox"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// ---------------------------------------------------------------------------
// F-wv2-1 — payout_destinations is the one member of this area's F-42 family
// with NO legal-edge table, so cp_app can re-verify a destination that was
// disabled or that the provider rejected.
//
// 00806 says of the two tables it creates: "a transition table that records
// edges constrains none", and 00807 repeats the treatment for
// payout_requests, listing its siblings in its own header -- 00761
// compliance_profiles.identity_state, 00762 verification_sessions.status,
// 00763 payout_destinations.status. Three of those four now have an edge table
// and an AD001 in their apply function. The fourth does not:
// cp_destination_apply_status_transition() (00763) writes `status` and
// `verified_at` from whatever the transition row says, and no
// `payout_destination_status_edges` exists in the schema or in
// test/integration/enums.
//
// internal/payout.destinationTransitions has the Go table to pair with, and it
// is explicit: "A destination never returns from DISABLED or REJECTED."
// ---------------------------------------------------------------------------

func TestAuditWV2_ADestinationCannotReturnFromDisabledOrRejected(t *testing.T) {
	f := newAuditFixture(t)

	require.False(t, payout.CanTransitionDestination(payout.DestinationDisabled, payout.DestinationVerified),
		"fixture check: internal/payout has no DISABLED -> VERIFIED edge")
	require.False(t, payout.CanTransitionDestination(payout.DestinationRejected, payout.DestinationVerified),
		"fixture check: internal/payout has no REJECTED -> VERIFIED edge")

	// The account holder stops using the destination -- or an operator disables
	// it after a fraud report. This is the service's own legal move.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.TransitionDestination(ctx, tx, f.destination, payout.DestinationDisabled,
				payout.DestinationChange{
					ActorType:  security.ActorUser,
					ActorID:    f.account.String(),
					Reason:     "the account holder stopped using this destination",
					OccurredAt: f.clk.Now().UTC(),
				})
			return err
		}))
	disabled, err := f.svc.Destination(f.ctx, testDB, f.destination)
	require.NoError(t, err)
	require.Equal(t, payout.DestinationDisabled, disabled.Status)
	require.False(t, disabled.Status.Usable())

	// The service refuses to bring it back, which is the Go half of the rule.
	require.Error(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, terr := f.svc.TransitionDestination(ctx, tx, f.destination, payout.DestinationVerified,
				payout.DestinationChange{
					ActorType: security.ActorSystem, ActorID: "audit-probe",
					Reason: "an edge the state machine does not have", OccurredAt: f.clk.Now().UTC(),
				})
			return terr
		}), "fixture check: the domain service refuses the edge")

	// One INSERT, as cp_app, naming an edge the state machine does not have.
	// There is no edge table for the trigger to consult, so the trigger writes
	// the status and the verified_at from it, and 00731's deferred binding is
	// satisfied because the row describes exactly the change that happened.
	_, err = testDB.Exec(f.ctx, `INSERT INTO payout_destination_transitions
		  (id, destination_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,'DISABLED','VERIFIED','SYSTEM','audit-probe','an edge CanTransitionDestination forbids')`,
		uuid.New(), f.destination)
	assert.Error(t, err,
		"F-wv2-1: cp_app re-verified a DISABLED payout destination with one INSERT; "+
			"payout_destinations is the only state column in this area whose apply function "+
			"consults no legal-edge table")
	if err == nil {
		assert.Equal(t, "AD001", db.SQLState(err))
	}

	after, gerr := f.svc.Destination(f.ctx, testDB, f.destination)
	require.NoError(t, gerr)
	assert.Equal(t, payout.DestinationDisabled, after.Status,
		"F-wv2-1: the destination a person disabled is usable again, and the row that says "+
			"where their money goes has been brought back from a terminal state")
	assert.False(t, after.Status.Usable())

	// The schema's own account of itself: three edge tables, and the fourth
	// state machine in this area has none.
	var n int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema='public' AND table_name='payout_destination_status_edges'`).Scan(&n))
	assert.Equal(t, 1, n,
		"F-wv2-1: no payout_destination_status_edges table exists, so nothing pairs "+
			"payout.destinationTransitions with the database the way 00806 and 00807 pair theirs")
}

// ---------------------------------------------------------------------------
// F-wv2-6 — 00807's same-state exemption lets cp_app write the money columns
// and the provider's reference onto a conversion request with NO state change
// at all, which is the one case neither control can see.
//
// cp_payout_apply_state_transition() skips the edge check when
// `NEW.from_state IS NOT DISTINCT FROM NEW.to_state` -- copied from 00806,
// where a same-state row is 00796's birth screen and is legitimate. There is
// no such row for a payout: transitionWith returns early when
// `req.State == to`, so internal/payout never writes one. What the exemption
// buys is a row that writes reserved_quantity, settled_quantity, settled_at,
// provider_reference and provider_status while the state stays exactly where
// it was -- and 00731's deferred binding returns NULL without checking
// anything, because `old_val IS NOT DISTINCT FROM new_val`.
//
// D-121 records this residual for the SANCTIONS SCREEN ("a same-state row can
// still carry a screening decision"). For payout_requests it carries money.
//
// The second half is the reservation invariant: cp_payout_reservation_balanced
// (PO001) is a constraint trigger on payout_allocations, so a reserved_quantity
// written from a transition row with no allocation rows behind it is never
// compared to anything.
// ---------------------------------------------------------------------------

func TestAuditWV2_ASameStateTransitionRowCannotWriteTheMoney(t *testing.T) {
	f := newAuditFixture(t)
	// A promotional grant: an origin SandboxPolicy forbids, so the decision is
	// REJECTED and nothing is reserved, consumed or allocated.
	f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 1_000_000_000)

	quote, qerr := f.quote(500_000_000)
	require.NoError(t, qerr)
	dest := f.destination
	var req payout.Request
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			req, _, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
				Quantity:           money.QuantityFromInt64(500_000_000),
				ProviderTerms:      f.terms(),
				Environment:        "TEST",
				DisclosureAccepted: true,
				IdempotencyKey:     "audit2-samestate-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, f.sandboxInput())
			return cerr
		}))
	require.Equal(t, payout.StateRejected, req.State)
	require.Equal(t, "0", req.ReservedQuantity.String())
	require.Zero(t, countAllocations(t, req.ID))

	// One INSERT, as cp_app. The edge VERIFIED -> REJECTED is in the table, and
	// the request is ALREADY REJECTED, so no state change happens and the money
	// rides in on a row that licenses nothing.
	_, err := testDB.Exec(f.ctx, `INSERT INTO payout_request_transitions
		  (id, request_id, from_state, to_state, actor_type, actor_id, reason,
		   reserved_quantity, settled_quantity, reserved_at, settled_at,
		   provider_reference, provider_status)
		SELECT $1, id, 'VERIFIED', state, 'SYSTEM', 'audit-probe',
		       'a row that moves nothing and carries everything',
		       requested_quantity, requested_quantity, now(), now(),
		       'forged-by-cp_app', 'settled'
		  FROM payout_requests WHERE id = $2`, uuid.New(), req.ID)
	assert.Error(t, err,
		"F-wv2-6: a transition row that changes no state wrote the reservation, the "+
			"settlement, their instants and a forged provider reference onto a REJECTED payout")

	after, gerr := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, gerr)
	assert.Equal(t, "0", after.ReservedQuantity.String(),
		"F-wv2-6: reserved_quantity was written with no allocations behind it; "+
			"cp_payout_reservation_balanced only fires on payout_allocations, so PO001 never ran")
	assert.Equal(t, "0", after.SettledQuantity.String(),
		"F-wv2-6: the request says the whole amount settled and no ledger posting exists")
	assert.Empty(t, after.ProviderReference,
		"F-wv2-6: provider_reference was taken out of cp_app's UPDATE grant by 00807 and "+
			"is reachable again through a row that moves nothing")
	assert.Nil(t, after.SettledAt)
}

// ---------------------------------------------------------------------------
// F-wv2-7 — the same exemption on 00806's compliance trigger extends an
// identity verification's validity window with no state change.
//
// D-121's stated residual is that a same-state row "can still carry a sanctions
// screening decision". It also carries `expires_at`:
//
//	expires_at = CASE WHEN NEW.to_state = 'VERIFIED'
//	                  THEN coalesce(NEW.expires_at, expires_at) ELSE expires_at END
//
// so a VERIFIED -> VERIFIED row renews a decision the provider made once, for
// as long as the writer likes, without a session, a provider or a state change.
// verification.Service.ExpireOverdue and verification.Resolver both read that
// column, so the renewal is what decides whether somebody is still PAYOUT_KYC.
// ---------------------------------------------------------------------------

func TestAuditWV2_ASameStateRowCannotRenewAVerification(t *testing.T) {
	f := newAuditFixture(t)
	svc, repo := newAuditVerificationService(t, f.clk)
	session := newAuditSession(t, repo, f.user)
	ingested, err := svc.Ingest(f.ctx, testDB, session, verification.Result{
		ProviderRef: session.ProviderRef,
		Status:      verification.SessionApproved,
		RawStatus:   "audit_approved",
		Sandbox:     true,
		AgeAtLeast:  verifysandbox.AttestsAgeAtLeast,
		Jurisdiction: rules.Jurisdiction{
			Country: session.JurisdictionCountry, Region: session.JurisdictionRegion,
		},
		Checks: []verification.CheckResult{
			{Kind: verification.CheckIdentityDocument, Outcome: verification.OutcomePass, Detail: "AUDIT"},
			{Kind: verification.CheckAge, Outcome: verification.OutcomePass, Detail: "AUDIT"},
			{Kind: verification.CheckJurisdiction, Outcome: verification.OutcomePass, Detail: "AUDIT"},
			{Kind: verification.CheckSanctions, Outcome: verification.OutcomePass, Detail: "AUDIT"},
		},
	}, "audit fixture: a provider decision", "audit2")
	require.NoError(t, err)
	require.Equal(t, verification.SessionApproved, ingested.Status)
	state, _, _, err := repo.ProfileState(f.ctx, testDB, f.user)
	require.NoError(t, err)
	require.Equal(t, verification.StateVerified, state, "fixture check: the profile is VERIFIED")

	var before *time.Time
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT expires_at FROM compliance_profiles WHERE user_id = $1`, f.user).Scan(&before))
	require.NotNil(t, before, "fixture check: a verified profile has a validity window")

	renewed := before.Add(100 * 365 * 24 * time.Hour)
	_, err = testDB.Exec(f.ctx, `INSERT INTO compliance_profile_transitions
		  (id, user_id, from_state, to_state, actor_type, actor_id, reason, expires_at)
		VALUES ($1,$2,'VERIFIED','VERIFIED','SYSTEM','audit-probe','a row that moves nothing',$3)`,
		uuid.New(), f.user, renewed)
	assert.Error(t, err,
		"F-wv2-7: a same-state compliance transition row extended a verification's validity window")

	var after *time.Time
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT expires_at FROM compliance_profiles WHERE user_id = $1`, f.user).Scan(&after))
	require.NotNil(t, after)
	assert.WithinDuration(t, *before, *after, time.Second,
		"F-wv2-7: the window a provider decided was rewritten by a row that licensed no change")
}

// ---------------------------------------------------------------------------
// F-wv2-5 — disabling a payout destination does not stop a payout already
// reserved for it. The sweep submits it to the destination the person just
// removed.
//
// §25 calls a destination change a high-risk operation and authz.go marks both
// POST and DELETE StepUp, with the stated reason that "an attacker who can
// only remove a destination can still deny a person their money". The
// converse is the one that costs money: a person who removes a destination
// because it was compromised has not stopped the value that is already
// reserved for it.
//
// payout.Service.Submit reads the destination in submitRequestFor and checks
// exactly one thing about it -- that it belongs to the same account. It never
// asks `dest.Status.Usable()`. D-122 says providerSupports "re-asks
// CanPayRecipient ... at the moment value would leave", but that is Create;
// value leaves at Submit, and Submit asks nothing.
//
// The window is not a few seconds. A reserved request waits in VERIFIED
// whenever a kill switch is active ("the request stays in VERIFIED with the
// value reserved, and the next sweep submits it once the switch is released"),
// whenever the provider is unavailable, and for as long as the single web
// service is spun down -- which cmd/api's own comments say happens on the
// launch tier.
// ---------------------------------------------------------------------------

func TestAuditWV2_DisablingADestinationStopsAPayoutReservedForIt(t *testing.T) {
	f := newAuditFixture(t)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 1_000_000_000)

	quote, err := f.quote(500_000_000)
	require.NoError(t, err)
	dest := f.destination
	var req payout.Request
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			req, _, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
				Quantity:           money.QuantityFromInt64(500_000_000),
				ProviderTerms:      f.terms(),
				Environment:        "TEST",
				DisclosureAccepted: true,
				IdempotencyKey:     "audit2-disabled-dest-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, f.sandboxInput())
			return cerr
		}))
	require.Equal(t, payout.StateVerified, req.State)
	require.Equal(t, "500000000", req.ReservedQuantity.String())

	// The person removes the destination -- the step-up-protected act §25 calls
	// high-risk -- before the sweep has run. This is exactly what
	// conversionAdapter.DisableDestination does.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, terr := f.svc.TransitionDestination(ctx, tx, dest, payout.DestinationDisabled,
				payout.DestinationChange{
					ActorType:  security.ActorUser,
					ActorID:    f.account.String(),
					Reason:     "this destination was compromised",
					OccurredAt: f.clk.Now().UTC(),
				})
			return terr
		}))
	current, derr := f.svc.Destination(f.ctx, testDB, dest)
	require.NoError(t, derr)
	require.False(t, current.Status.Usable(), "fixture check: the destination is no longer usable")

	// The sweep's call, unchanged.
	after, serr := f.svc.Submit(f.ctx, testDB, req.ID, f.provider.Name())
	assert.Error(t, serr,
		"F-wv2-5: a payout was submitted to a destination the account holder had disabled")
	assert.NotEqual(t, payout.StateSettled, after.State,
		"F-wv2-5: the money went to the removed destination and the request reached SETTLED")
	assert.Empty(t, f.provider.last.DestinationReference,
		"F-wv2-5: the provider was handed the disabled destination's token")
}

// ---------------------------------------------------------------------------
// F-wv2-11 — POST /v1/payouts/quote prices a payout to a destination the
// provider has since said it cannot pay, and the provenance it returns is
// computed with `ProviderSupports: true` hardcoded.
//
// payout.Service.Quote asks the provider three questions -- availability, the
// destination KIND and the CURRENCY -- and never asks CanPayRecipient, which
// is the question D-122 added because "a question skipped when the answer is
// missing is not asking early, it is not asking". httpapi.conversionAdapter.Quote
// then evaluates the provenance beside the price with
//
//	ProviderSupports: true,
//
// under a comment saying the facts are read in the transaction "so the
// provenance shown beside a quote is the provenance the commit would actually
// consume rather than an optimistic one".
//
// The consequence is a person shown a gross, a fee, a net and a list of the
// lots that would leave, for a payout POST /v1/payouts refuses -- and the
// refusal consumes the quote, so the request lands terminal in REJECTED and
// the price they were shown is spent.
//
// This is what happens when a provider narrows ExcludedRegions after a
// destination was registered, which is the only way the two can disagree now
// that AddDestination asks the question.
// ---------------------------------------------------------------------------

func TestAuditWV2_AQuoteIsNotGivenForARecipientTheProviderCannotPay(t *testing.T) {
	f := newAuditFixture(t)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 1_000_000_000)

	// The destination was registered when the provider published no exclusions.
	// The provider now excludes the whole country's subdivisions, so the stored
	// recipient -- which carries no region, because none was required then --
	// is one it will not pay.
	f.provider.caps.ExcludedRegions = map[string][]string{"US": {"NY", "HI"}}
	ok, refusals := f.provider.Capabilities().CanPayRecipient(payout.RecipientProfile{
		Kind: "individual", Country: "US",
	})
	require.False(t, ok, "fixture check: the provider now refuses this recipient")
	require.NotEmpty(t, refusals)

	quote, qerr := f.quote(500_000_000)
	assert.Error(t, qerr,
		"F-wv2-11: a quote was priced for a destination the provider had said it cannot pay; "+
			"payout.Service.Quote asks about the kind and the currency and never about the recipient")
	if qerr != nil {
		return
	}

	// And the commit refuses it, consuming the quote on the way.
	dest := f.destination
	in := f.sandboxInput()
	in.ProviderSupports = false // what payoutsAdapter.providerSupports now answers
	var req payout.Request
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			req, _, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
				Quantity:           money.QuantityFromInt64(500_000_000),
				ProviderTerms:      f.terms(),
				Environment:        "TEST",
				DisclosureAccepted: true,
				IdempotencyKey:     "audit2-quote-region-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, in)
			return cerr
		}))
	assert.NotEqual(t, payout.StateRejected, req.State,
		"F-wv2-11: the person was shown a price and a provenance, and the commit rejected it "+
			"and burned the quote")
	spent, gerr := f.svc.QuoteByID(f.ctx, testDB, quote.ID)
	require.NoError(t, gerr)
	assert.Nil(t, spent.ConsumedAt,
		"F-wv2-11: the quote the person was shown is consumed by the refusal, so asking again "+
			"costs another quote and is refused again")
}
