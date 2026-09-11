//go:build integration

package credit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/capacity"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// ---------------------------------------------------------------------------
// F-credits-payments-2: a chargeback destroys lots that did not fund it, and
// leaves the lot it DID reverse holding units nobody can ever use.
// ---------------------------------------------------------------------------

// Service.reverseTo computed `covered = min(accountBalance, lot.Quantity)` and
// then called Consume with no AllowedOrigins and no lot restriction. Consume
// walks the account's lots in CONSUMPTION order -- promotional first, purchased
// fourth -- so the units it destroyed were whichever lots sorted first, not the
// lot the chargeback was about. The funding's own lot was then stamped REVERSED
// with its remaining quantity untouched.
//
// The account was left holding CREDIT_BALANCE it could never spend, never
// withdraw and never clear, and a promotional grant that had nothing to do with
// the disputed card payment had been seized instead.
//
// Inverted for F-152: the assertions that recorded the grant at zero and the
// reversed lot at 300 are kept, the other way round. A clawback destroys what
// its own lot still holds and books what was already spent out of that lot as
// the recorded DEFICIT.
func TestAudit_ChargebackDestroysTheLotItReverses(t *testing.T) {
	f := newFixture(t)

	// A 300-unit promotional grant the platform gave this user. It is
	// spendable and it is not payout-eligible under any policy in this build.
	grant := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 300)

	// And a 1,000-unit card purchase, minted the ordinary way.
	funding := f.mintedFunding(t, 1000)
	purchased := *f.fundingRow(t, funding).LotID

	before, err := f.svc.creditBalance(f.ctx, testDB, f.account, f.asset)
	require.NoError(t, err)
	require.Equal(t, "1300", before.String())

	// The cardholder charges back the 1,000. Nothing has been spent.
	var res ReverseResult
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var rerr error
			res, rerr = f.svc.Reverse(ctx, tx, funding, f.clk.Now(), "chargeback received")
			return rerr
		}))

	// What Reverse reports, and what it records on the funding: 1,000 units
	// destroyed, none owed, and the purchased lot is the lot it touched.
	assert.Equal(t, "1000", res.Destroyed.String())
	assert.Equal(t, "0", res.Deficit.String())
	assert.Equal(t, []LotID{purchased}, res.LotIDs)

	grantAfter, err := f.svc.Lot(f.ctx, testDB, grant.ID)
	require.NoError(t, err)
	purchasedAfter, err := f.svc.Lot(f.ctx, testDB, purchased)
	require.NoError(t, err)

	// What happens. The grant was untouched -- it did not fund this payment --
	// and the charged-back lot is empty.
	assert.Equal(t, "300", grantAfter.Remaining.String(),
		"the promotional grant did not fund the card payment and is not taken by its chargeback")
	assert.Equal(t, "0", purchasedAfter.Remaining.String(),
		"the CHARGED BACK purchase keeps nothing")
	assert.Equal(t, valuedomain.FinalityReversed, purchasedAfter.Finality)

	// The account's books afterwards.
	after, err := f.svc.creditBalance(f.ctx, testDB, f.account, f.asset)
	require.NoError(t, err)
	require.Equal(t, "300", after.String())
	require.NoError(t, f.svc.VerifyProvenance(f.ctx, testDB, f.account),
		"provenance reconciles, as it did before -- the defect was never a divergence")

	bal := f.balancesFor(t, valuedomain.SandboxPolicy(), valuedomain.VerificationPayoutKYC)

	// The invariant. A chargeback claws back the units the reversed funding
	// minted; it does not take units from an unrelated lot, and it does not
	// leave the reversed lot holding spendable-looking balance.
	assert.Equal(t, "300", bal.Gross.String(),
		"GET /v1/credits/balance reports a gross balance of 300")
	assert.Equal(t, "0", bal.Reversed.String(),
		"nothing is stranded inside the REVERSED lot")
	assert.Equal(t, "300", bal.Spendable.String(),
		"the user keeps the 300 promotional units the chargeback did not fund, and they are spendable")
}

// The other half of F-152: what a chargeback does when the purchase HAS been
// spent. The funding's own lot is empty, so there is nothing to destroy and the
// whole amount is the recorded DEFICIT -- and the grant that was sitting beside
// it is still not touched.
func TestAudit_ChargebackOfSpentCreditsBooksADeficitAndTakesNoOtherLot(t *testing.T) {
	f := newFixture(t)
	grant := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 300)
	funding := f.mintedFunding(t, 1000)
	purchased := *f.fundingRow(t, funding).LotID

	// The user spends exactly the purchased lot. Consumption order takes the
	// grant first, so the spend has to name the origin to reach the purchase.
	_, err := f.spend(1000, valuedomain.OriginPurchased)
	require.NoError(t, err)

	var res ReverseResult
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var rerr error
			res, rerr = f.svc.Reverse(ctx, tx, funding, f.clk.Now(), "chargeback received")
			return rerr
		}))

	assert.Equal(t, "0", res.Destroyed.String(), "there was nothing left in the lot to destroy")
	assert.Equal(t, "1000", res.Deficit.String(), "all of it was spent, so all of it is owed")
	assert.Equal(t, []LotID{purchased}, res.LotIDs)

	grantAfter, err := f.svc.Lot(f.ctx, testDB, grant.ID)
	require.NoError(t, err)
	assert.Equal(t, "300", grantAfter.Remaining.String(),
		"a deficit is recorded against the account; it is not collected out of an unrelated grant")
	require.NoError(t, f.svc.VerifyProvenance(f.ctx, testDB, f.account))
}

// ---------------------------------------------------------------------------
// F-credits-payments-3: a purchase that never reaches the provider, and a
// checkout the customer abandons, consume the money-at-risk ceiling forever.
// ---------------------------------------------------------------------------

// StartPurchase commits the funding row in phase 1 and calls the provider in
// phase 2. A phase-2 failure returns an error to the caller and leaves the
// committed row in CREATED with a NULL provider_reference.
//
// CREATED is in capacity.atRiskFundingStates, so the amount counted against
// CP_CAPACITY_MAX_AT_RISK_MINOR. Nothing ever moved it out:
//
//   - SettleDue only selects state = 'REVERSIBLE';
//   - the reconciliation sweep's stale() required provider_reference IS NOT
//     NULL, and Reconcile returned early for an empty reference anyway;
//   - nothing else in the binary wrote CANCELED or FAILED.
//
// So the ceiling internal/capacity documents as drainable was monotonically
// non-decreasing again, which is F-90's failure ("the deployment would have
// refused every Credit purchase with AT_CAPACITY, permanently") reached through
// a different door.
//
// Inverted for F-153: ExpireInFlight ends it, and the assertions that recorded
// the money as stuck forever now record it draining.
func TestAudit_AFailedProviderCreateIsCancelledAndReleasesTheCeiling(t *testing.T) {
	f := newPurchaseFixtureWithCeiling(t, 200_000) // the blueprint's $2,000

	guard, err := capacity.NewGuard(capacity.Budget{MaxAtRiskMinor: 200_000}, f.clk.Now)
	require.NoError(t, err)
	atRisk := func() int64 {
		t.Helper()
		var r capacity.Reading
		require.NoError(t, f.tx(func(tx pgx.Tx) error {
			var merr error
			r, merr = guard.Measure(f.ctx, tx)
			return merr
		}))
		return r.AtRiskMinor
	}
	before := atRisk()

	// The provider refuses. Any 4xx, rate limit or timeout does this.
	f.prov.failCreate = errs.New(errs.CodeProviderUnavailable, "provider is unavailable")
	_, err = f.svcP.StartPurchase(f.ctx, testDB, StartPurchaseRequest{
		AccountID: f.account, Amount: money.USDFromMinor(100_000), // $1,000
		Currency: "USD", IdempotencyKey: f.keyPrefix + ":failed-create",
	})
	require.Error(t, err, "the caller is told the purchase failed")
	f.prov.failCreate = nil

	// And yet the row is committed, in CREATED, with no provider reference.
	// That part is deliberate: a record to reconcile against is better than a
	// charge nobody knows about (F-96).
	var state string
	var ref *string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT state, provider_reference FROM credit_fundings WHERE idempotency_key = $1`,
		f.keyPrefix+":failed-create").Scan(&state, &ref))
	require.Equal(t, "CREATED", state)
	require.Nil(t, ref)

	require.Equal(t, before+100_000, atRisk(),
		"$1,000 of a payment that was never opened counts against the launch tier's ceiling")

	// SettleDue is not the thing that takes it back out, and never was.
	var n int
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var serr error
		n, serr = f.svcP.SettleDue(f.ctx, tx, 30*24*time.Hour, 100)
		return serr
	}))
	require.Zero(t, n, "SettleDue only looks at REVERSIBLE")

	// A purchase this fresh is not abandoned yet, and the pass leaves it alone.
	expired, err := f.svcP.ExpireInFlight(f.ctx, testDB, DefaultInFlightLifetime, 100)
	require.NoError(t, err)
	require.Zero(t, expired, "a checkout opened a moment ago is not an abandoned one")
	require.Equal(t, before+100_000, atRisk())

	// A day later it is. The listing takes its cutoff from the database's own
	// clock, so the row is backdated rather than the process's clock moved.
	f.backdate(t, f.keyPrefix+":failed-create", 25*time.Hour)
	expired, err = f.svcP.ExpireInFlight(f.ctx, testDB, DefaultInFlightLifetime, 100)
	require.NoError(t, err)
	require.Equal(t, 1, expired)

	got := f.fundingByKey(t, f.keyPrefix+":failed-create")
	assert.Equal(t, FundingCanceled, got.State,
		"a payment that was never opened is CANCELED, not FAILED: nobody's card was declined")
	assert.True(t, got.State.Terminal())
	assert.Zero(t, f.prov.canceled, "there was nothing at the provider to cancel")
	assert.Equal(t, before, atRisk(), "and the ceiling has its headroom back")

	// Which is the whole point: an honest purchase is admitted again.
	honest, err := f.svcP.StartPurchase(f.ctx, testDB, StartPurchaseRequest{
		AccountID: f.account, Amount: money.USDFromMinor(1000), // $10
		Currency: "USD", IdempotencyKey: f.keyPrefix + ":honest",
	})
	require.NoError(t, err, "the ceiling is no longer exhausted by a payment that never existed")
	require.NotEmpty(t, honest.Funding.ProviderReference)
}

// The same hole, reached the way a real user reaches it: open the Buy Credits
// page, get a PaymentIntent, and close the tab.
//
// The funding gets a provider reference, so the sweep did find it -- and asked
// the provider, which reports requires_payment_method forever, which maps to
// AUTHORIZATION_PENDING, which is also an at-risk state. Reconcile then had
// nothing left to do on every subsequent pass, and no code path in this binary
// ever cancelled an abandoned PaymentIntent or expired the funding.
//
// Inverted for F-153. The pass asks the provider first, and only a payment the
// provider still reports as awaiting its customer is cancelled -- at the
// provider before it is cancelled here, because a funding marked CANCELED over
// a live PaymentIntent is a card that can still be charged against a terminal
// funding that will never mint.
func TestAudit_AnAbandonedCheckoutIsCancelledWithTheProviderAndLeavesTheCeiling(t *testing.T) {
	f := newPurchaseFixture(t)

	guard, err := capacity.NewGuard(capacity.Budget{MaxAtRiskMinor: 200_000}, f.clk.Now)
	require.NoError(t, err)
	atRisk := func() int64 {
		t.Helper()
		var r capacity.Reading
		require.NoError(t, f.tx(func(tx pgx.Tx) error {
			var merr error
			r, merr = guard.Measure(f.ctx, tx)
			return merr
		}))
		return r.AtRiskMinor
	}
	before := atRisk()

	p := f.start(t, "abandoned", 100_000) // $1,000, then the customer walks away
	require.Equal(t, before+100_000, atRisk())

	// The reconciliation pass runs, repeatedly, and adopts the provider's view
	// -- which is "still waiting", forever. It is not the thing that ends this.
	for i := 0; i < 3; i++ {
		f.backdate(t, p.Funding.IdempotencyKey, 400*24*time.Hour)
		_, rerr := f.svcP.ReconcileDue(f.ctx, testDB, DefaultReconcileAfter, 100)
		require.NoError(t, rerr)
	}
	still := f.funding(t, p.Funding.ID)
	assert.Contains(t, []FundingState{FundingCreated, FundingAuthorizationPending}, still.State,
		"reconciliation adopts the provider's view and the provider's view is 'still waiting'")
	assert.False(t, still.State.Terminal())
	assert.Equal(t, before+100_000, atRisk())

	// Expiry is. It cancels the PaymentIntent and then the funding.
	expired, err := f.svcP.ExpireInFlight(f.ctx, testDB, DefaultInFlightLifetime, 100)
	require.NoError(t, err)
	require.Equal(t, 1, expired)

	got := f.funding(t, p.Funding.ID)
	assert.Equal(t, FundingCanceled, got.State)
	assert.True(t, got.State.Terminal())
	assert.Equal(t, 1, f.prov.canceled, "the PaymentIntent was cancelled at the provider, not just here")
	snap, err := f.prov.GetPurchase(f.ctx, p.Funding.ProviderReference)
	require.NoError(t, err)
	assert.Equal(t, PurchaseCanceled, snap.Status, "so the customer's card cannot be charged against it later")
	assert.Equal(t, before, atRisk(),
		"an abandoned checkout stops counting against the launch tier's money-at-risk ceiling")
}

// A checkout the customer DID complete, whose success this system has not heard
// about yet, must not be cancelled by the expiry pass. The pass asks the
// provider first, and a provider that says the money arrived is the authority.
func TestAudit_ExpiryNeverCancelsAPaymentTheProviderSaysSucceeded(t *testing.T) {
	f := newPurchaseFixture(t)
	p := f.start(t, "paid-late", 10_000)

	// The customer paid; the webhook was lost.
	f.prov.advance(p.Funding.ProviderReference, PurchaseSucceeded, "succeeded")
	f.backdate(t, p.Funding.IdempotencyKey, 400*24*time.Hour)

	expired, err := f.svcP.ExpireInFlight(f.ctx, testDB, DefaultInFlightLifetime, 100)
	require.NoError(t, err)
	assert.Zero(t, expired, "nothing was abandoned")
	assert.Zero(t, f.prov.canceled, "and nothing was cancelled at the provider")

	got := f.funding(t, p.Funding.ID)
	assert.Equal(t, FundingReversible, got.State, "it was minted instead")
	require.NotNil(t, got.LotID)
}

// ---------------------------------------------------------------------------
// F-credits-payments-4: a card-network INQUIRY that closes promotes the
// funding to SETTLED, which is payout eligibility, without the reversibility
// window having closed.
// ---------------------------------------------------------------------------

// stripecredit.disputeStatus mapped charge.dispute.closed with status
// "warning_closed" to credit.PurchaseDisputeWon, and FundingStateFor mapped
// PurchaseDisputeWon to FundingSettled. SettleFunding then promoted the lot
// REVERSIBLE -> SETTLED, which is what FundingFinality.PayoutEligible() reads.
//
// "warning_closed" is the close of an early-fraud-warning INQUIRY. Stripe's own
// documentation is explicit that an inquiry is not a dispute and that a
// chargeback may still follow it; the adapter's comment agreed -- "an
// early-warning notice that closed without becoming a dispute". The money was
// therefore exactly as reversible as it had been before the inquiry opened, and
// the funding was SETTLED: the state credit.FundingState documents as "the
// money is ours", that internal/capacity stops counting as money at risk, and
// that docs/product/CREDIT_ECONOMY.md says means "the funding is final".
//
// ManualResolution refuses to let an operator assert SETTLED for precisely this
// reason ("an operator who could assert it by hand could make value
// payout-eligible by closing a ticket"). A card network inquiry could.
//
// Inverted for F-155 and D-094: warning_closed is DISPUTE_LIFTED, a won dispute
// is DISPUTE_WON, and both return the funding to REVERSIBLE -- the window it
// was already in, since 00743 stamps reversible_at once. SettleDue is the only
// thing that settles.
func TestAudit_AnInquiryThatClosesUnfreezesTheFundingAndDoesNotSettleIt(t *testing.T) {
	f := newPurchaseFixture(t)

	p := f.start(t, "inquiry", 10_000)
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, "inq-e1", 10_000))

	minted := f.funding(t, p.Funding.ID)
	require.Equal(t, FundingReversible, minted.State)
	require.NotNil(t, minted.ReversibleAt)
	require.NotNil(t, minted.LotID)
	openedAt := *minted.ReversibleAt

	// charge.dispute.created for an early-fraud-warning inquiry.
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseDisputed, "inq-e2", 10_000))
	frozen, err := f.svc.Lot(f.ctx, testDB, *minted.LotID)
	require.NoError(t, err)
	require.Equal(t, valuedomain.FinalityDisputed, frozen.Finality)

	// charge.dispute.closed, status "warning_closed": the inquiry was closed
	// without becoming a dispute. Nothing moved; nothing is final.
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseDisputeLifted, "inq-e3", 10_000))

	lifted := f.funding(t, p.Funding.ID)
	lot, err := f.svc.Lot(f.ctx, testDB, *minted.LotID)
	require.NoError(t, err)

	assert.Equal(t, FundingReversible, lifted.State, "the freeze lifts; nothing settles")
	assert.Equal(t, valuedomain.FinalityReversible, lot.Finality)
	assert.False(t, lot.Finality.PayoutEligible(),
		"an inquiry that closed must not make a card payment inside its dispute window withdrawable")
	assert.True(t, lot.Finality.Spendable(), "and the value stops being frozen")
	require.NotNil(t, lifted.ReversibleAt)
	assert.True(t, lifted.ReversibleAt.Equal(openedAt),
		"the funding returns to the window it was in; re-entering REVERSIBLE does not restart the clock")
	assert.Nil(t, lifted.SettledAt)

	// The window has not closed, and the sweep -- the only thing that settles
	// -- still finds nothing due.
	var n int
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var serr error
		n, serr = f.svcP.SettleDue(f.ctx, tx, 30*24*time.Hour, 10)
		return serr
	}))
	assert.Zero(t, n, "SettleDue finds nothing due; the money is minutes old")

	// The balance API reports it as not withdrawable, even on a sandbox tier
	// whose policy permits PURCHASED.
	bal := f.balancesForAccount(t, valuedomain.SandboxPolicy(), valuedomain.VerificationPayoutKYC)
	assert.Equal(t, "0", bal.PayoutEligible.String())
	assert.Equal(t, creditsForDollars(t, f, 100).String(), bal.Spendable.String())

	// And it is still money at risk, which is what a reversible payment is.
	// SETTLED is the state internal/capacity stops counting, and the funding is
	// not in it.
	assert.Contains(t, capacity.AtRiskFundingStates(), string(lifted.State),
		"an inquiry closing does not take a reversible payment out of the money-at-risk ceiling")
	assert.NotContains(t, capacity.AtRiskFundingStates(), string(FundingSettled),
		"which is the state the funding would have been in")
}

// The same rule for the outcome that IS a dispute won (D-094). A card network
// finding in our favour is strong evidence the money is ours; it is not the
// closing of the reversibility window, and another dispute can follow it.
func TestAudit_AWonDisputeReturnsTheFundingToItsWindowRatherThanSettlingIt(t *testing.T) {
	f := newPurchaseFixture(t)

	p := f.start(t, "won", 10_000)
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, "won-e1", 10_000))
	minted := f.funding(t, p.Funding.ID)
	require.NotNil(t, minted.LotID)

	f.deliver(t, event(p.Funding.ProviderReference, PurchaseDisputed, "won-e2", 10_000))
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseDisputeWon, "won-e3", 10_000))

	got := f.funding(t, p.Funding.ID)
	assert.Equal(t, FundingReversible, got.State)
	assert.Nil(t, got.SettledAt, "only SettleDue writes settled_at")

	// And a second dispute is still representable, which is the reason.
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseDisputed, "won-e4", 10_000))
	again := f.funding(t, p.Funding.ID)
	assert.Equal(t, FundingDisputed, again.State)
	lot, err := f.svc.Lot(f.ctx, testDB, *minted.LotID)
	require.NoError(t, err)
	assert.Equal(t, valuedomain.FinalityDisputed, lot.Finality)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// mintedFunding walks a funding to REVERSIBLE the ordinary way and returns its
// id.
func (f *fixture) mintedFunding(t *testing.T, qty int64) FundingID {
	t.Helper()
	var funding Funding
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			funding, err = f.svc.CreateFunding(ctx, tx, CreateFundingRequest{
				AccountID: f.account, Provider: "audit-provider",
				CreditQuantity: q(qty), PaidAmount: money.USDFromMinor(qty),
				IdempotencyKey: "audit-funding-" + randomKey(),
			})
			return err
		}))
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.AdvanceFunding(ctx, tx, funding.ID, FundingCaptured, "provider webhook", "")
			return err
		}))
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.MintFrom(ctx, tx, funding.ID, f.clk.Now())
			return err
		}))
	return funding.ID
}

// backdate moves a funding's created_at into the past, as the migration role.
//
// It has to be the owner: 00743 revoked UPDATE on credit_fundings from cp_app
// and granted back only lot_id and provider_reference, which is exactly the
// guarantee that stops an application from ageing its own money. And it has to
// be created_at rather than updated_at, because credit_fundings_updated_at is a
// BEFORE UPDATE trigger that writes now() over whatever the statement said --
// so an UPDATE that backdated updated_at would silently do nothing.
func (f *fixture) backdate(t *testing.T, idempotencyKey string, age time.Duration) {
	t.Helper()
	tag, err := testOwnerDB.Exec(f.ctx,
		`UPDATE credit_fundings SET created_at = now() - make_interval(secs => $2)
		  WHERE idempotency_key = $1`, idempotencyKey, age.Seconds())
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
}

func (f *fixture) fundingByKey(t *testing.T, idempotencyKey string) Funding {
	t.Helper()
	var id FundingID
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT id FROM credit_fundings WHERE idempotency_key = $1`, idempotencyKey).Scan(&id))
	return f.fundingRow(t, id)
}

func (f *fixture) fundingRow(t *testing.T, id FundingID) Funding {
	t.Helper()
	got, err := f.svc.Funding(f.ctx, testDB, id)
	require.NoError(t, err)
	return got
}

func (f *fixture) balancesFor(t *testing.T, p valuedomain.Policy, v valuedomain.VerificationLevel) Balances {
	t.Helper()
	b, err := f.svc.Balances(f.ctx, testDB, BalanceRequest{
		AccountID: f.account, Policy: p, Verified: v, Now: f.clk.Now(),
		ActiveCaps: map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
	})
	require.NoError(t, err)
	return b
}

func (f *purchaseFixture) balancesForAccount(t *testing.T, p valuedomain.Policy, v valuedomain.VerificationLevel) Balances {
	t.Helper()
	return f.fixture.balancesFor(t, p, v)
}

func randomKey() string {
	var s string
	err := testDB.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&s)
	if err != nil {
		panic(errors.New("audit: could not generate a key: " + err.Error()))
	}
	return s
}

// ---------------------------------------------------------------------------
// F-credits-payments-5: the sweep that recovers a swallowed provider event has
// no caller in the deployed topology.
// ---------------------------------------------------------------------------

// A provider event that names a reference no funding holds is Ignored, and the
// webhook pipeline records the event id as processed, so the provider's
// redelivery of the SAME event id is answered Duplicate and never reaches
// Dispatch again (internal/webhook/handler.go, via event.Inbox.ProcessHashed).
//
// That window is real: StartPurchase commits the provider reference in phase 3,
// AFTER the provider call returns, and Stripe can deliver
// payment_intent.succeeded before that commit lands.
//
// PurchaseService.Reconcile is the recovery, and it was the only one. It had
// exactly one caller in the repository -- cmd/reconciliation-worker -- and
// render.yaml declares two services, both `type: web`, neither of them that
// worker. cmd/api deliberately ran SettleDue itself for exactly this reason
// ("the launch tier has no worker tier") and did not run Reconcile. So on the
// deployed tier the card was charged and no Credits were ever minted.
//
// Inverted for F-154: the pass is ReconcileDue, it runs in cmd/api beside the
// settlement sweep, and this asserts the recovery through it rather than
// through a hand-rolled Reconcile call the deployment never makes.
func TestAudit_ASwallowedSucceededEventIsRecoveredByTheInProcessPass(t *testing.T) {
	f := newPurchaseFixture(t)

	// Phase 1 and 2 have happened; phase 3 has not. The provider holds a
	// PaymentIntent this row does not yet name.
	var funding Funding
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			funding, err = f.svc.CreateFunding(ctx, tx, CreateFundingRequest{
				AccountID: f.account, Provider: f.prov.Name(),
				CreditQuantity: q(10_000), PaidAmount: money.USDFromMinor(10_000),
				IdempotencyKey: f.keyPrefix + ":swallowed",
			})
			return err
		}))
	ref := "pi_fake_" + randomKey()

	// payment_intent.succeeded arrives in the window. The pipeline verified it,
	// archived it and will mark it processed.
	d := f.deliver(t, event(ref, PurchaseSucceeded, "swallowed-e1", 10_000))
	require.Equal(t, "IGNORED", string(d))

	// Phase 3 commits a moment later.
	_, err := testDB.Exec(f.ctx,
		`UPDATE credit_fundings SET provider_reference = $2 WHERE id = $1`, funding.ID, ref)
	require.NoError(t, err)

	// Stripe redelivers the same event id; the inbox answers Duplicate and
	// Dispatch is never called again. The funding is still where it was.
	got := f.funding(t, funding.ID)
	require.Equal(t, FundingCreated, got.State)
	require.Nil(t, got.LotID, "the card was charged and no Credits exist")

	bal, err := f.svc.creditBalance(f.ctx, testDB, f.account, f.asset)
	require.NoError(t, err)
	require.Equal(t, "0", bal.String())

	// The provider knows what happened, and is never asked by anything the
	// deployed topology runs -- that was the finding.
	f.prov.mu.Lock()
	f.prov.byRef[ref] = PurchaseSnapshot{
		ProviderReference: ref, Status: PurchaseSucceeded, RawStatus: "succeeded",
		Amount: money.USDFromMinor(10_000), Currency: "USD",
	}
	f.prov.mu.Unlock()

	// A purchase this fresh is not stale, so the pass leaves it alone: a
	// customer mid-checkout must not be reconciled out from under themselves.
	checked, err := f.svcP.ReconcileDue(f.ctx, testDB, DefaultReconcileAfter, 100)
	require.NoError(t, err)
	assert.Zero(t, checked)

	// Fifteen minutes later the pass that cmd/api now runs on its own ticker,
	// beside runCreditSettlement, finds it and mints.
	f.backdate(t, f.keyPrefix+":swallowed", 20*time.Minute)
	checked, err = f.svcP.ReconcileDue(f.ctx, testDB, DefaultReconcileAfter, 100)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, checked, 1)

	recovered := f.funding(t, funding.ID)
	assert.Equal(t, FundingReversible, recovered.State)
	require.NotNil(t, recovered.LotID, "the swallowed event is recovered by a pass the deployment runs")

	bal, err = f.svc.creditBalance(f.ctx, testDB, f.account, f.asset)
	require.NoError(t, err)
	assert.Equal(t, "10000", bal.String())
	require.NoError(t, f.svc.VerifyProvenance(f.ctx, testDB, f.account))

	// And it is idempotent: a second pass over the same funding mints nothing
	// more, which is what lets a worker tier be added later with no
	// coordination.
	_, err = f.svcP.ReconcileDue(f.ctx, testDB, DefaultReconcileAfter, 100)
	require.NoError(t, err)
	bal, err = f.svc.creditBalance(f.ctx, testDB, f.account, f.asset)
	require.NoError(t, err)
	assert.Equal(t, "10000", bal.String())
}

// The listing the recovery depends on. It used to require
// `provider_reference IS NOT NULL`, which excluded exactly the fundings the
// sweep existed to recover: StartPurchase commits the row in phase 1 and the
// reference in phase 3, so every crash or provider failure between them leaves
// a committed row with a NULL reference that nothing would ever look at again.
func TestAudit_TheInFlightListingDoesNotSkipFundingsWithNoProviderReference(t *testing.T) {
	f := newPurchaseFixture(t)

	f.prov.failCreate = errs.New(errs.CodeProviderUnavailable, "provider is unavailable")
	_, err := f.svcP.StartPurchase(f.ctx, testDB, StartPurchaseRequest{
		AccountID: f.account, Amount: money.USDFromMinor(5_000),
		Currency: "USD", IdempotencyKey: f.keyPrefix + ":no-ref",
	})
	require.Error(t, err)
	f.prov.failCreate = nil

	referenceless := f.fundingByKey(t, f.keyPrefix+":no-ref")
	require.Empty(t, referenceless.ProviderReference)
	f.backdate(t, f.keyPrefix+":no-ref", 20*time.Minute)

	var ids []FundingID
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var lerr error
		ids, lerr = f.svcP.InFlightFundings(f.ctx, tx, inFlightStates, DefaultReconcileAfter, 100)
		return lerr
	}))
	assert.Contains(t, ids, referenceless.ID,
		"a funding whose provider reference was never committed is the one case the sweep is for")
}
