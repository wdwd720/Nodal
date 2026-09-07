//go:build integration

package payout_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/payout/payouttest"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "payout integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "payout-itest", MaxConns: 20}); err != nil {
		fmt.Fprintln(os.Stderr, "payout integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set")
	}
}

// payoutCaps is what the ledger needs active for the reserve and settle
// movements to commit.
type payoutCaps map[valuedomain.CapabilityKey]bool

func (c payoutCaps) ActiveConversionCapabilities(context.Context) (map[valuedomain.CapabilityKey]bool, error) {
	return c, nil
}

var (
	creditAssetOnce sync.Once
	creditAssetID   assets.AssetID
)

func creditAsset(t *testing.T) assets.AssetID {
	t.Helper()
	creditAssetOnce.Do(func() {
		ctx := context.Background()
		var existing assets.AssetID
		if err := testDB.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing); err == nil {
			creditAssetID = existing
			return
		}
		created, err := assets.NewRepository().Create(ctx, testDB, assets.Asset{
			Chain: assets.InternalChain, Kind: assets.KindCredit,
			ValueDomain: valuedomain.InternalCredit,
			Symbol:      "CREDIT", Name: "Nodal Credit", Decimals: 6,
			RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
		})
		require.NoError(t, err)
		creditAssetID = created.ID
	})
	return creditAssetID
}

// creatorPayoutPolicy is what an evidence-backed activation looks like:
// creator earnings become payable, everything else stays shut.
const creatorCap valuedomain.CapabilityKey = "PAYOUT_CREATOR_EARNINGS"

func creatorPayoutPolicy() valuedomain.Policy {
	p := valuedomain.DefaultPolicy()
	p.Version = "itest-creator-earnings-v1"
	p.Rules[valuedomain.OriginCreatorEarning] = valuedomain.OriginRule{
		PayoutAllowed:        true,
		RequiredCapability:   creatorCap,
		RequiredVerification: valuedomain.VerificationPayoutKYC,
	}
	return p
}

type fixture struct {
	t        *testing.T
	ctx      context.Context
	clk      *clock.Fake
	led      *ledger.Service
	credits  *credit.Service
	svc      *payout.Service
	provider *payouttest.Sandbox
	registry *payout.Registry

	account     accounts.AccountID
	creditAsset assets.AssetID
	destination payout.DestinationID
}

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func newAccount(t *testing.T) accounts.AccountID {
	t.Helper()
	repo := accounts.NewRepository()
	user, err := repo.CreateUser(context.Background(), testDB, "payout-itest", uuid.NewString(), nil)
	require.NoError(t, err)
	acct, err := repo.CreateAccount(context.Background(), testDB, user.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return acct.ID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	led := ledger.NewService(clk, "payout-itest")
	led.SetCapabilityResolver(payoutCaps{
		valuedomain.CapPayoutReserve: true,
		valuedomain.CapPayoutSettle:  true,
	})
	credits := credit.NewService(led, clk)

	// Sandboxes allowed: this is a test. A registry built with false refuses
	// the same provider, which is its own test below.
	registry := payout.NewRegistry(true)
	provider := payouttest.NewSandbox("sandbox")
	require.NoError(t, registry.Register(provider))

	f := &fixture{
		t: t, ctx: ctx, clk: clk, led: led, credits: credits,
		provider: provider, registry: registry,
		svc:         payout.NewService(led, credits, payout.NewEngine(credits), registry, clk),
		account:     newAccount(t),
		creditAsset: creditAsset(t),
	}

	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			d, err := f.svc.CreateDestination(ctx, tx, payout.Destination{
				AccountID: f.account, Kind: payout.DestinationBank,
				Provider: "sandbox", ProviderReference: "dest-" + uuid.NewString(),
				DisplayLabel: "Test bank", Currency: "USD",
			})
			if err != nil {
				return err
			}
			if _, err := f.svc.SetDestinationStatus(ctx, tx, d.ID, payout.DestinationVerified); err != nil {
				return err
			}
			f.destination = d.ID
			return nil
		}))
	return f
}

// issue mints a lot of a given provenance.
func (f *fixture) issue(origin valuedomain.CreditOrigin, fin valuedomain.FundingFinality, qty int64) credit.Lot {
	f.t.Helper()
	var lot credit.Lot
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			lot, err = f.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: f.account, Quantity: q(qty), Origin: origin, Finality: fin,
				Reference:      credit.Reference{Type: "test_issue", ID: uuid.NewString()},
				IdempotencyKey: "issue-" + uuid.NewString(),
				Reason:         "test", EffectiveAt: f.clk.Now(),
			})
			return err
		}))
	return lot
}

func (f *fixture) input() payout.EligibilityInput {
	return payout.EligibilityInput{
		Policy:              creatorPayoutPolicy(),
		Verified:            valuedomain.VerificationPayoutKYC,
		ActiveCaps:          map[valuedomain.CapabilityKey]bool{creatorCap: true},
		Now:                 f.clk.Now().Add(48 * time.Hour),
		DestinationVerified: true,
		ProviderSupports:    true,
	}
}

func (f *fixture) create(amount int64, in payout.EligibilityInput) (payout.Request, payout.Decision, error) {
	var (
		req payout.Request
		dec payout.Decision
	)
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			dest := f.destination
			req, dec, err = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, Quantity: q(amount),
				IdempotencyKey: "payout-" + uuid.NewString(), EffectiveAt: f.clk.Now(),
			}, in)
			return err
		})
	return req, dec, err
}

func (f *fixture) balance(code ledger.Code) money.Quantity {
	f.t.Helper()
	var raw string
	require.NoError(f.t, testDB.QueryRow(f.ctx,
		`SELECT coalesce((SELECT b.balance FROM ledger_accounts la
		    JOIN ledger_balances b ON b.ledger_account_id = la.id
		   WHERE la.owner_type='CUSTOMER' AND la.owner_id=$1 AND la.code=$2 AND la.asset_id=$3), 0)::text`,
		f.account, string(code), f.creditAsset).Scan(&raw))
	v, err := money.ParseQuantity(raw)
	require.NoError(f.t, err)
	return v
}

// ---------------------------------------------------------------------------

// TestIntegration_PayoutReservesExactlyOnce is acceptance test PAY-001.
func TestIntegration_PayoutReservesExactlyOnce(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)

	req, dec, err := f.create(400, f.input())
	require.NoError(t, err)
	require.True(t, dec.Sufficient())
	require.Equal(t, payout.StateVerified, req.State)
	require.Equal(t, "400", req.ReservedQuantity.String())

	// The Credits left the spendable balance and are held in PAYOUT_RESERVED.
	require.Equal(t, "600", f.balance(ledger.CodeCreditBalance).String())
	require.Equal(t, "400", f.balance(ledger.CodePayoutReserved).String())
	require.NoError(t, f.svc.VerifyReservations(f.ctx, testDB, f.creditAsset))
	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.account))

	// The reserved units are unspendable: a second payout for the whole
	// balance can only reach what is left.
	_, dec2, err := f.create(700, f.input())
	require.NoError(t, err)
	require.False(t, dec2.Sufficient(),
		"reserved Credits must not be available to a second payout")
	require.Contains(t, dec2.Reasons, payout.ReasonInsufficientEligibleValue)
}

// TestIntegration_PromotionalCreditsCannotCashOut is acceptance test VAL-002.
func TestIntegration_PromotionalCreditsCannotCashOut(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 5_000)

	req, dec, err := f.create(100, f.input())
	require.NoError(t, err)
	require.False(t, dec.Sufficient())
	require.Equal(t, payout.StateRejected, req.State)
	require.Contains(t, dec.Reasons, valuedomain.ReasonOriginForbidden)

	require.Equal(t, "5000", f.balance(ledger.CodeCreditBalance).String(),
		"a rejected payout must not have moved anything")
	require.Equal(t, "0", f.balance(ledger.CodePayoutReserved).String())
}

// TestIntegration_PayoutEnforcesProvenance is acceptance test PAY-003 and
// VAL-003: an account with plenty of Credits but the wrong kind cannot pay out.
func TestIntegration_PayoutEnforcesProvenance(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 10_000)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 10_000)
	f.issue(valuedomain.OriginMarketTradingProceeds, valuedomain.FinalitySettled, 10_000)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 250)

	// 30,250 Credits, of which exactly 250 may leave.
	_, dec, err := f.create(1_000, f.input())
	require.NoError(t, err)
	require.False(t, dec.Sufficient())
	require.Equal(t, "250", dec.Eligible.String())
	require.Contains(t, dec.Reasons, valuedomain.ReasonOriginForbidden,
		"the user is told why the rest cannot leave, not only that it cannot")

	req, dec, err := f.create(250, f.input())
	require.NoError(t, err)
	require.True(t, dec.Sufficient())
	require.Equal(t, payout.StateVerified, req.State)

	// And what it reserved is the creator earning, not the promotional grant
	// that sorts first in consumption order.
	allocs, err := f.svc.Allocations(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.Len(t, allocs, 1)
	require.Equal(t, valuedomain.OriginCreatorEarning, allocs[0].Origin,
		"a payout approved for creator earnings must not sweep up a promotional grant")
}

// TestIntegration_UnverifiedUserCannotReachSubmission is acceptance test
// PAY-004, and the "KYC at exit" journey of PART XIX.
//
// The distinction under test is between "you cannot" and "you have not
// verified yet". Collapsing the two into a rejection would be a dead end where
// the product has a next step to offer.
func TestIntegration_UnverifiedUserCannotReachSubmission(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)

	in := f.input()
	in.Verified = valuedomain.VerificationNodalIdentity // logged in, not identified
	req, dec, err := f.create(500, in)
	require.NoError(t, err)
	require.False(t, dec.Sufficient(),
		"a Nodal login must never be sufficient to receive money")
	require.Equal(t, payout.StateVerificationRequired, req.State)
	require.True(t, dec.VerificationWouldSuffice,
		"the value is there and its provenance is approved; only identity is missing")
	require.Contains(t, dec.Reasons, valuedomain.ReasonVerificationTooLow)
	require.Equal(t, valuedomain.VerificationPayoutKYC, dec.RequiredVerification,
		"the decision must say what would be needed")

	// Nothing is reserved on a maybe.
	require.Equal(t, "0", f.balance(ledger.CodePayoutReserved).String())
	require.Equal(t, "1000", f.balance(ledger.CodeCreditBalance).String())

	// And it cannot be submitted from here.
	_, err = f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.Error(t, err)
	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// Completing verification re-evaluates and then reserves.
	verified := f.input()
	var after payout.Request
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			r, _, e := f.svc.CompleteVerification(ctx, tx, req.ID, verified, f.clk.Now())
			after = r
			return e
		}))
	require.Equal(t, payout.StateVerified, after.State)
	require.Equal(t, "500", f.balance(ledger.CodePayoutReserved).String())
	require.NoError(t, f.svc.VerifyReservations(f.ctx, testDB, f.creditAsset))
}

// TestIntegration_VerificationDoesNotExcuseIneligibleProvenance: verifying
// answers one question and does not settle the others.
func TestIntegration_VerificationDoesNotExcuseIneligibleProvenance(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 5_000)

	in := f.input()
	in.Verified = valuedomain.VerificationNodalIdentity
	req, dec, err := f.create(500, in)
	require.NoError(t, err)
	require.False(t, dec.VerificationWouldSuffice,
		"no amount of identity verification makes a promotional grant withdrawable")
	require.Equal(t, payout.StateRejected, req.State)
}

func TestIntegration_ReversibleFundingIsNeverPayable(t *testing.T) {
	f := newFixture(t)
	// Creator earnings, but funded by a card payment still inside its dispute
	// window. Origin is approved; finality is not.
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalityReversible, 1_000)

	_, dec, err := f.create(500, f.input())
	require.NoError(t, err)
	require.False(t, dec.Sufficient())
	require.Contains(t, dec.Reasons, valuedomain.ReasonFundingNotFinal)
}

func TestIntegration_AccountLevelBlocksOverrideEverything(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)

	for name, mutate := range map[string]func(*payout.EligibilityInput){
		"frozen":                 func(in *payout.EligibilityInput) { in.AccountFrozen = true },
		"fraud flagged":          func(in *payout.EligibilityInput) { in.FraudFlagged = true },
		"unverified destination": func(in *payout.EligibilityInput) { in.DestinationVerified = false },
		"provider cannot pay":    func(in *payout.EligibilityInput) { in.ProviderSupports = false },
	} {
		t.Run(name, func(t *testing.T) {
			in := f.input()
			mutate(&in)
			req, dec, err := f.create(100, in)
			require.NoError(t, err)
			require.False(t, dec.Sufficient())
			require.Equal(t, payout.StateRejected, req.State)
			require.Empty(t, dec.Lots, "a blocked account must not have lots selected for it")
		})
	}
}

func TestIntegration_SettlementMovesValueOutOfTheSystem(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(400, f.input())
	require.NoError(t, err)

	settled, err := f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)
	require.Equal(t, payout.StateSettled, settled.State)
	require.Equal(t, "400", settled.SettledQuantity.String())

	require.Equal(t, "0", f.balance(ledger.CodePayoutReserved).String(),
		"settled value has left the pending domain")
	require.Equal(t, "600", f.balance(ledger.CodeCreditBalance).String())
	require.NoError(t, f.svc.VerifyReservations(f.ctx, testDB, f.creditAsset))

	var settledOut string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT coalesce((SELECT b.balance FROM ledger_accounts la
		    JOIN ledger_balances b ON b.ledger_account_id = la.id
		   WHERE la.owner_type='PLATFORM' AND la.code='PAYOUT_SETTLED' AND la.asset_id=$1), 0)::text`,
		f.creditAsset).Scan(&settledOut))
	require.NotEqual(t, "0", settledOut)
}

// TestIntegration_AProviderTimeoutDoesNotDuplicateThePayout is acceptance test
// PAY-002 and the crash scenario of PART XXXVIII.
//
// The provider records the payout and then loses the response. Nodal must not
// release the reservation, must not resubmit, and must discover the truth by
// asking about the key it chose before it called.
func TestIntegration_AProviderTimeoutDoesNotDuplicateThePayout(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(400, f.input())
	require.NoError(t, err)

	f.provider.TimeoutNext()
	unknown, err := f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)
	require.Equal(t, payout.StateStatusUnknown, unknown.State,
		"an ambiguous provider answer must not be read as either success or failure")
	require.Equal(t, "400", f.balance(ledger.CodePayoutReserved).String(),
		"the reservation must stay while the outcome is unknown")
	require.Equal(t, 1, f.provider.Submits(), "the provider has one payout on file")

	// Submitting again must not send a second payout.
	again, err := f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)
	require.Equal(t, payout.StateStatusUnknown, again.State)
	require.Equal(t, 1, f.provider.Submits(), "resubmission must not create a second payout")

	// Reconciliation finds out what actually happened.
	resolved, err := f.svc.Reconcile(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.Equal(t, payout.StateSettled, resolved.State)
	require.Equal(t, "400", resolved.SettledQuantity.String())
	require.Equal(t, 1, f.provider.Submits(),
		"the user was paid exactly once despite the lost response and the retry")
	require.Equal(t, "0", f.balance(ledger.CodePayoutReserved).String())
	require.NoError(t, f.svc.VerifyReservations(f.ctx, testDB, f.creditAsset))
}

// TestIntegration_AnUnreachableProviderLeavesTheReservationAlone: if we cannot
// find out, we do not guess.
func TestIntegration_AnUnreachableProviderLeavesTheReservationAlone(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(300, f.input())
	require.NoError(t, err)

	f.provider.TimeoutNext()
	_, err = f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)

	f.provider.SetLookupDown(true)
	still, err := f.svc.Reconcile(f.ctx, testDB, req.ID)
	require.Error(t, err)
	require.Equal(t, errs.CodeReconciliationRequired, errs.CodeOf(err))
	require.Equal(t, payout.StateStatusUnknown, still.State)
	require.Equal(t, "300", f.balance(ledger.CodePayoutReserved).String(),
		"an unreachable provider must not release the reservation")

	f.provider.SetLookupDown(false)
	resolved, err := f.svc.Reconcile(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.Equal(t, payout.StateSettled, resolved.State)
}

func TestIntegration_AFailedPayoutReturnsTheExactUnits(t *testing.T) {
	f := newFixture(t)
	earning := f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(400, f.input())
	require.NoError(t, err)

	f.provider.FailNext("account closed at the receiving bank")
	failed, err := f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)
	require.Equal(t, payout.StateFailed, failed.State)

	require.Equal(t, "1000", f.balance(ledger.CodeCreditBalance).String(),
		"a failed payout returns the value")
	require.Equal(t, "0", f.balance(ledger.CodePayoutReserved).String())

	// And it returned to the SAME lot, so the provenance survives the round
	// trip. Returning a bare quantity would let a promotional grant be
	// laundered into an earning by requesting a payout and having it fail.
	lot, err := f.credits.Lot(f.ctx, testDB, earning.ID)
	require.NoError(t, err)
	require.Equal(t, "1000", lot.Remaining.String())
	require.Equal(t, valuedomain.OriginCreatorEarning, lot.Origin)
	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.account))
}

func TestIntegration_ASubmittedPayoutCannotBeCancelled(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(200, f.input())
	require.NoError(t, err)

	f.provider.TimeoutNext()
	_, err = f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)

	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := f.svc.Cancel(ctx, tx, req.ID, "user changed their mind")
			return e
		})
	require.Error(t, err, "a payout that may already have been paid cannot simply be cancelled")
	require.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	require.Equal(t, "200", f.balance(ledger.CodePayoutReserved).String())
}

func TestIntegration_CancellingBeforeSubmissionReturnsTheUnits(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(600, f.input())
	require.NoError(t, err)
	require.Equal(t, "400", f.balance(ledger.CodeCreditBalance).String())

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := f.svc.Cancel(ctx, tx, req.ID, "user changed their mind")
			return e
		}))
	require.Equal(t, "1000", f.balance(ledger.CodeCreditBalance).String())
	require.Equal(t, "0", f.balance(ledger.CodePayoutReserved).String())
	require.NoError(t, f.svc.VerifyReservations(f.ctx, testDB, f.creditAsset))
}

// TestIntegration_RevokingSettleMidFlightParksThePayoutForAHuman proves the
// second gate has teeth independently of the first, and that a settlement
// which cannot be recorded produces a stated state rather than a stuck one.
func TestIntegration_RevokingSettleMidFlightParksThePayoutForAHuman(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(300, f.input())
	require.NoError(t, err, "reserving works: PAYOUT_RESERVE is active")

	// Revoke only the settle capability, then submit.
	f.led.SetCapabilityResolver(payoutCaps{valuedomain.CapPayoutReserve: true})
	_, err = f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.Error(t, err, "the settle posting must be refused")
	require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))

	// The provider paid and Nodal could not record it. That is exactly what
	// MANUAL_REVIEW is for: the reservation stays, nothing is guessed, and the
	// reason is on the record.
	parked, err := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.Equal(t, payout.StateManualReview, parked.State)
	require.Equal(t, "300", f.balance(ledger.CodePayoutReserved).String(),
		"the value stays reserved; it neither leaves nor silently returns")

	// Restoring the capability lets reconciliation finish the job, exactly
	// once.
	f.led.SetCapabilityResolver(payoutCaps{
		valuedomain.CapPayoutReserve: true, valuedomain.CapPayoutSettle: true,
	})
	resolved, err := f.svc.Reconcile(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.Equal(t, payout.StateSettled, resolved.State)
	require.Equal(t, 1, f.provider.Submits(), "the user is paid once, not twice")
	require.NoError(t, f.svc.VerifyReservations(f.ctx, testDB, f.creditAsset))
}

// TestIntegration_TheUnwindPathIsNeverGated is PART XXXII: stopping new risk
// must not strand value already committed.
func TestIntegration_TheUnwindPathIsNeverGated(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(300, f.input())
	require.NoError(t, err)
	require.Equal(t, "300", f.balance(ledger.CodePayoutReserved).String())

	// Every payout capability revoked, including the one that reserved.
	f.led.SetCapabilityResolver(payoutCaps{})
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := f.svc.Cancel(ctx, tx, req.ID, "capability revoked; returning value")
			return e
		}), "with every payout capability off, reserved value must still be returnable")
	require.Equal(t, "1000", f.balance(ledger.CodeCreditBalance).String())
	require.Equal(t, "0", f.balance(ledger.CodePayoutReserved).String())
}

func TestIntegration_CreateIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)

	key := "payout-" + uuid.NewString()
	run := func() payout.Request {
		var req payout.Request
		require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				dest := f.destination
				r, _, err := f.svc.Create(ctx, tx, payout.CreateRequest{
					AccountID: f.account, DestinationID: &dest, Quantity: q(250),
					IdempotencyKey: key, EffectiveAt: f.clk.Now(),
				}, f.input())
				req = r
				return err
			}))
		return req
	}
	first := run()
	second := run()
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, "250", f.balance(ledger.CodePayoutReserved).String(),
		"a repeated request must not reserve twice")
}

func TestIntegration_OpenRequestsAreWhatAReconcilerSweeps(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 2_000)
	stuck, _, err := f.create(300, f.input())
	require.NoError(t, err)
	f.provider.TimeoutNext()
	_, err = f.svc.Submit(f.ctx, testDB, stuck.ID, "sandbox")
	require.NoError(t, err)

	done, _, err := f.create(300, f.input())
	require.NoError(t, err)
	_, err = f.svc.Submit(f.ctx, testDB, done.ID, "sandbox")
	require.NoError(t, err)

	open, err := f.svc.OpenRequests(f.ctx, testDB, time.Now().Add(time.Hour), 100)
	require.NoError(t, err)
	var found bool
	for _, r := range open {
		require.NotEqual(t, done.ID, r.ID, "a settled payout is not open")
		if r.ID == stuck.ID {
			found = true
			require.True(t, r.State.HoldsValue())
		}
	}
	require.True(t, found, "an unresolved payout must appear in the sweep")
}
