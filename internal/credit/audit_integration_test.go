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

// Service.reverseTo computes `covered = min(accountBalance, lot.Quantity)` and
// then calls Consume with no AllowedOrigins and no lot restriction
// (funding.go:519-566). Consume walks the account's lots in CONSUMPTION order
// -- promotional first, purchased fourth (repository.go:33) -- so the units it
// destroys are whichever lots sort first, not the lot the chargeback is about.
// The funding's own lot is then stamped REVERSED with its remaining quantity
// untouched.
//
// The account is left holding CREDIT_BALANCE it can never spend, never
// withdraw and never clear, and a promotional grant that had nothing to do with
// the disputed card payment has been seized instead.
func TestAudit_ChargebackDestroysADifferentLotThanTheOneItReverses(t *testing.T) {
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

	// What actually happened.
	assert.Equal(t, "0", grantAfter.Remaining.String(),
		"the promotional grant was consumed by a chargeback of a card payment it did not fund")
	assert.Equal(t, "300", purchasedAfter.Remaining.String(),
		"300 units of the CHARGED BACK purchase survive")
	assert.Equal(t, valuedomain.FinalityReversed, purchasedAfter.Finality)

	// The account's books afterwards.
	after, err := f.svc.creditBalance(f.ctx, testDB, f.account, f.asset)
	require.NoError(t, err)
	require.Equal(t, "300", after.String())
	require.NoError(t, f.svc.VerifyProvenance(f.ctx, testDB, f.account),
		"provenance still reconciles, which is why nothing catches this")

	bal := f.balancesFor(t, valuedomain.SandboxPolicy(), valuedomain.VerificationPayoutKYC)

	// The invariant this breaks. A chargeback must claw back the units the
	// reversed funding minted; it must not take units from an unrelated lot,
	// and it must not leave the reversed lot holding spendable-looking balance.
	assert.Equal(t, "300", bal.Gross.String(),
		"GET /v1/credits/balance reports a gross balance of 300")
	assert.Equal(t, "0", bal.Spendable.String(),
		"none of which can be spent")
	assert.Equal(t, "300", bal.Reversed.String(),
		"because all 300 units are stranded inside the REVERSED lot")

	// The user should have been left with their 300-unit grant, spendable.
	assert.Equal(t, "300", bal.Spendable.String(),
		"the user is entitled to keep the 300 promotional units the chargeback did not fund; "+
			"instead the grant was destroyed and 300 dead units of the reversed purchase remain "+
			"on the account and in SUM(CREDIT_BALANCE) forever")
}

// ---------------------------------------------------------------------------
// F-credits-payments-3: a purchase that never reaches the provider, and a
// checkout the customer abandons, consume the money-at-risk ceiling forever.
// ---------------------------------------------------------------------------

// staleQuery is cmd/reconciliation-worker/creditsweep.go's stale() verbatim.
// The sweep is the only thing that asks a provider what became of an in-flight
// purchase, and this is the filter it uses.
const staleQuery = `SELECT id FROM credit_fundings
	  WHERE state IN ('CREATED','AUTHORIZATION_PENDING','AUTHORIZED','CAPTURE_PENDING')
	    AND provider_reference IS NOT NULL
	    AND updated_at < now() - interval '15 minutes'
	  ORDER BY updated_at
	  LIMIT $1`

// StartPurchase commits the funding row in phase 1 and calls the provider in
// phase 2 (purchase.go:176-230). A phase-2 failure returns an error to the
// caller and leaves the committed row in CREATED with a NULL
// provider_reference.
//
// CREATED is in capacity.atRiskFundingStates, so the amount counts against
// CP_CAPACITY_MAX_AT_RISK_MINOR. Nothing ever moves it out:
//
//   - SettleDue only selects state = 'REVERSIBLE';
//   - the reconciliation sweep's stale() requires provider_reference IS NOT
//     NULL, and Reconcile returns early for an empty reference anyway;
//   - nothing else in the binary writes CANCELED or FAILED.
//
// So the ceiling internal/capacity documents as drainable is monotonically
// non-decreasing again, which is F-90's failure ("the deployment would have
// refused every Credit purchase with AT_CAPACITY, permanently") reached through
// a different door.
func TestAudit_AFailedProviderCreateHoldsTheAtRiskCeilingForever(t *testing.T) {
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

	// And yet the row is committed, in CREATED, with no provider reference.
	var state string
	var ref *string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT state, provider_reference FROM credit_fundings WHERE idempotency_key = $1`,
		f.keyPrefix+":failed-create").Scan(&state, &ref))
	require.Equal(t, "CREATED", state)
	require.Nil(t, ref)

	require.Equal(t, before+100_000, atRisk(),
		"$1,000 of a payment that was never opened counts against the launch tier's ceiling")

	// Nothing will ever take it back out.
	var staleIDs []FundingID
	rows, err := testDB.Query(f.ctx, staleQuery, 100)
	require.NoError(t, err)
	for rows.Next() {
		var id FundingID
		require.NoError(t, rows.Scan(&id))
		staleIDs = append(staleIDs, id)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	for _, id := range staleIDs {
		got := f.funding(t, id)
		require.NotEqual(t, "", got.ProviderReference)
	}

	var n int
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var serr error
		n, serr = f.svcP.SettleDue(f.ctx, tx, 30*24*time.Hour, 100)
		return serr
	}))
	require.Zero(t, n, "SettleDue only looks at REVERSIBLE")

	// Even backdating it past every window changes nothing.
	_, err = testOwnerDB.Exec(f.ctx,
		`UPDATE credit_fundings SET updated_at = now() - interval '400 days', created_at = now() - interval '400 days'
		   WHERE idempotency_key = $1`, f.keyPrefix+":failed-create")
	require.NoError(t, err)
	require.Equal(t, before+100_000, atRisk(),
		"a year later it is still counted; there is no expiry and no cancellation path")

	// Which is what exhausts the ceiling. One more failure and a deployment
	// whose ceiling is the blueprint's $2,000 refuses every honest purchase.
	f.prov.failCreate = errs.New(errs.CodeProviderUnavailable, "provider is unavailable")
	_, err = f.svcP.StartPurchase(f.ctx, testDB, StartPurchaseRequest{
		AccountID: f.account, Amount: money.USDFromMinor(100_000),
		Currency: "USD", IdempotencyKey: f.keyPrefix + ":failed-create-2",
	})
	require.Error(t, err)
	f.prov.failCreate = nil

	_, err = f.svcP.StartPurchase(f.ctx, testDB, StartPurchaseRequest{
		AccountID: f.account, Amount: money.USDFromMinor(1000), // $10, a working provider
		Currency: "USD", IdempotencyKey: f.keyPrefix + ":honest",
	})
	require.Error(t, err, "the ceiling is exhausted by two payments that never existed")
	assert.Equal(t, errs.CodeAtCapacity, errs.CodeOf(err))
}

// The same hole, reached the way a real user reaches it: open the Buy Credits
// page, get a PaymentIntent, and close the tab.
//
// The funding gets a provider reference, so the sweep does find it -- and asks
// the provider, which reports requires_payment_method forever, which maps to
// AUTHORIZATION_PENDING, which is also an at-risk state. Reconcile then has
// nothing left to do on every subsequent pass, and no code path in this binary
// ever cancels an abandoned PaymentIntent or expires the funding.
func TestAudit_AnAbandonedCheckoutNeverLeavesTheAtRiskCeiling(t *testing.T) {
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

	// The sweep runs, repeatedly, for a year.
	for i := 0; i < 3; i++ {
		_, err = testOwnerDB.Exec(f.ctx,
			`UPDATE credit_fundings SET updated_at = now() - interval '400 days' WHERE id = $1`, p.Funding.ID)
		require.NoError(t, err)
		require.NoError(t, f.tx(func(tx pgx.Tx) error {
			_, rerr := f.svcP.Reconcile(f.ctx, tx, p.Funding.ID)
			return rerr
		}))
	}

	got := f.funding(t, p.Funding.ID)
	assert.Contains(t, []FundingState{FundingCreated, FundingAuthorizationPending}, got.State,
		"reconciliation adopts the provider's view and the provider's view is 'still waiting'")
	assert.False(t, got.State.Terminal())
	assert.Equal(t, before+100_000, atRisk(),
		"an abandoned checkout counts against the launch tier's money-at-risk ceiling forever; "+
			"nothing in this binary writes CANCELED except an operator resolving a MANUAL_REVIEW")
}

// ---------------------------------------------------------------------------
// F-credits-payments-4: a card-network INQUIRY that closes promotes the
// funding to SETTLED, which is payout eligibility, without the reversibility
// window having closed.
// ---------------------------------------------------------------------------

// stripecredit.disputeStatus maps charge.dispute.closed with status
// "warning_closed" to credit.PurchaseDisputeWon (webhook.go:230-236), and
// FundingStateFor maps PurchaseDisputeWon to FundingSettled
// (purchaseprovider.go:237). SettleFunding then promotes the lot
// REVERSIBLE -> SETTLED, which is what FundingFinality.PayoutEligible() reads.
//
// "warning_closed" is the close of an early-fraud-warning INQUIRY. Stripe's own
// documentation is explicit that an inquiry is not a dispute and that a
// chargeback may still follow it; the adapter's comment agrees -- "an
// early-warning notice that closed without becoming a dispute". The money is
// therefore exactly as reversible as it was before the inquiry opened, and the
// funding is now SETTLED: the state credit.FundingState documents as "the money
// is ours", that internal/capacity stops counting as money at risk, and that
// docs/product/CREDIT_ECONOMY.md says means "the funding is final".
//
// ManualResolution refuses to let an operator assert SETTLED for precisely this
// reason ("an operator who could assert it by hand could make value
// payout-eligible by closing a ticket"). A card network inquiry can.
func TestAudit_AnInquiryThatClosesSettlesTheFundingBeforeItsWindow(t *testing.T) {
	f := newPurchaseFixture(t)

	p := f.start(t, "inquiry", 10_000)
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseSucceeded, "inq-e1", 10_000))

	minted := f.funding(t, p.Funding.ID)
	require.Equal(t, FundingReversible, minted.State)
	require.NotNil(t, minted.ReversibleAt)
	require.NotNil(t, minted.LotID)

	// charge.dispute.created for an early-fraud-warning inquiry.
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseDisputed, "inq-e2", 10_000))
	frozen, err := f.svc.Lot(f.ctx, testDB, *minted.LotID)
	require.NoError(t, err)
	require.Equal(t, valuedomain.FinalityDisputed, frozen.Finality)

	// charge.dispute.closed, status "warning_closed": the inquiry was closed
	// without becoming a dispute. Nothing moved; nothing is final.
	f.deliver(t, event(p.Funding.ProviderReference, PurchaseDisputeWon, "inq-e3", 10_000))

	settled := f.funding(t, p.Funding.ID)
	lot, err := f.svc.Lot(f.ctx, testDB, *minted.LotID)
	require.NoError(t, err)

	assert.Equal(t, FundingSettled, settled.State)
	assert.Equal(t, valuedomain.FinalitySettled, lot.Finality)
	assert.True(t, lot.Finality.PayoutEligible())

	// The window has not closed: the sweep would not have settled this.
	var n int
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var serr error
		n, serr = f.svcP.SettleDue(f.ctx, tx, 30*24*time.Hour, 10)
		return serr
	}))
	assert.Zero(t, n, "SettleDue finds nothing due; the money is minutes old")

	// And the balance API now reports it as withdrawable on a sandbox tier.
	bal := f.balancesForAccount(t, valuedomain.SandboxPolicy(), valuedomain.VerificationPayoutKYC)
	assert.Equal(t, "10000", bal.PayoutEligible.String(),
		"an inquiry that closed made a card payment inside its dispute window payout-eligible")

	// It is also no longer counted as money at risk.
	guard, err := capacity.NewGuard(capacity.Budget{MaxAtRiskMinor: 200_000}, f.clk.Now)
	require.NoError(t, err)
	var r capacity.Reading
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		var merr error
		r, merr = guard.Measure(f.ctx, tx)
		return merr
	}))
	assert.NotContains(t, []int64{10_000}, r.AtRiskMinor%100_000,
		"SETTLED is excluded from the money-at-risk ceiling by design")
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

// A provider event that names a reference no funding holds is Ignored
// (purchase.go:365-374) and the webhook pipeline records the event id as
// processed, so Stripe's redelivery of the SAME event id is answered Duplicate
// and never reaches Dispatch again (internal/webhook/handler.go, via
// event.Inbox.ProcessHashed).
//
// That window is real: StartPurchase commits the provider reference in phase 3,
// AFTER the provider call returns (purchase.go:232-256), and Stripe can deliver
// payment_intent.succeeded before that commit lands.
//
// PurchaseService.Reconcile is the recovery, and it is the only one. It has
// exactly one caller in the repository -- cmd/reconciliation-worker -- and
// render.yaml declares two services, both `type: web`, neither of them that
// worker. cmd/api deliberately runs SettleDue itself for exactly this reason
// (cmd/api/creditsettle.go: "the launch tier has no worker tier") and does not
// run Reconcile.
//
// So on the deployed tier the card is charged and no Credits are ever minted.
func TestAudit_ASwallowedSucceededEventIsOnlyRecoveredByASweepNobodyRuns(t *testing.T) {
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
	assert.Equal(t, FundingCreated, got.State)
	assert.Nil(t, got.LotID, "the card was charged and no Credits exist")

	bal, err := f.svc.creditBalance(f.ctx, testDB, f.account, f.asset)
	require.NoError(t, err)
	assert.Equal(t, "0", bal.String())

	// Reconcile is the recovery, and it works. Nothing in the deployed
	// topology calls it: `grep -rn "\.Reconcile(" cmd/ internal/` names only
	// cmd/reconciliation-worker, and render.yaml declares no worker service.
	f.prov.mu.Lock()
	f.prov.byRef[ref] = PurchaseSnapshot{
		ProviderReference: ref, Status: PurchaseSucceeded, RawStatus: "succeeded",
		Amount: money.USDFromMinor(10_000), Currency: "USD",
	}
	f.prov.mu.Unlock()
	require.NoError(t, f.tx(func(tx pgx.Tx) error {
		_, rerr := f.svcP.Reconcile(f.ctx, tx, funding.ID)
		return rerr
	}))
	recovered := f.funding(t, funding.ID)
	assert.Equal(t, FundingReversible, recovered.State)
	require.NotNil(t, recovered.LotID, "Reconcile is what mints; it is not deployed")
}
