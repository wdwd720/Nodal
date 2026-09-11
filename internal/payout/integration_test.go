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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/killswitch"
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

func (c payoutCaps) ActiveConversionCapabilities(context.Context, db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
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
		svc: payout.NewService(led, credits, payout.NewEngine(credits), registry, clk,
			killswitch.NewChecker(killswitch.Policy{}), accounts.NewRepository()),
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
				// The withdrawal disclosure, accepted. The tests that are about
				// the disclosure itself set it false; every other test in this
				// file is about eligibility and provenance, and an unsigned
				// document would refuse before either was reached.
				DisclosureAccepted: true,
				IdempotencyKey:     "payout-" + uuid.NewString(), EffectiveAt: f.clk.Now(),
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
					DisclosureAccepted: true,
					IdempotencyKey:     key, EffectiveAt: f.clk.Now(),
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

// ---------------------------------------------------------------------------
// Manual review (gola.md Stage 17)
// ---------------------------------------------------------------------------

// toManualReview drives a request into MANUAL_REVIEW through the real state
// machine rather than by writing the row, so the history the resolver reads is
// the history a real incident would leave.
func (f *fixture) toManualReview(req payout.Request, reason string) payout.Request {
	f.t.Helper()
	var out payout.Request
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.svc.FlagForManualReview(ctx, tx, req.ID, reason)
			return err
		}))
	return out
}

func (f *fixture) resolve(id payout.RequestID, r payout.ManualResolution, reason string) (payout.Request, error) {
	var out payout.Request
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var rerr error
			out, rerr = f.svc.ResolveManualReview(ctx, tx, id, r, reason)
			return rerr
		})
	return out, err
}

// TestIntegration_FailingAManualReviewReturnsTheExactUnits: resolving a stuck
// payout as FAILED gives the user back exactly what was reserved, to exactly
// the lots it came from.
func TestIntegration_FailingAManualReviewReturnsTheExactUnits(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(400, f.input())
	require.NoError(t, err)
	require.Equal(t, "400", req.ReservedQuantity.String())

	before := f.balance(ledger.CodeCreditBalance)
	stuck := f.toManualReview(req, "provider settled and the settlement could not be recorded")
	require.Equal(t, payout.StateManualReview, stuck.State)

	resolved, err := f.resolve(req.ID, payout.ResolveFail, "provider confirmed no payment was made")
	require.NoError(t, err)
	require.Equal(t, payout.StateFailed, resolved.State)
	require.Equal(t, "0", resolved.ReservedQuantity.String())

	after := f.balance(ledger.CodeCreditBalance)
	require.Equal(t, "400", after.Sub(before).String(), "the exact reserved units come back")
	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.account))
}

// TestIntegration_AManualReviewCanNeverBeDeclaredSettled. The provider is
// authoritative for settlement. An operator who could assert it by hand could
// close a ticket by claiming money moved.
func TestIntegration_AManualReviewCanNeverBeDeclaredSettled(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(400, f.input())
	require.NoError(t, err)
	f.toManualReview(req, "stuck")

	for _, bad := range []payout.ManualResolution{"SETTLED", "SETTLE", "PAID", "", "settled"} {
		_, err := f.resolve(req.ID, bad, "closing the ticket")
		require.Error(t, err, "resolution %q must be refused", bad)
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	}
	current, err := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.Equal(t, payout.StateManualReview, current.State, "nothing moved")
}

// TestIntegration_AResolutionRequiresAReason: the reason is the only record of
// why a human overrode a machine.
func TestIntegration_AResolutionRequiresAReason(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(400, f.input())
	require.NoError(t, err)
	f.toManualReview(req, "stuck")

	_, err = f.resolve(req.ID, payout.ResolveFail, "   ")
	require.Error(t, err)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// TestIntegration_APayoutThatMayHaveBeenSubmittedCannotBeRetried is the
// property the whole resolution API exists to protect. A payout that reached
// MANUAL_REVIEW from SUBMITTED may already exist at the provider; sending it
// back to VERIFIED would make a second submission possible.
func TestIntegration_APayoutThatMayHaveBeenSubmittedCannotBeRetried(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(400, f.input())
	require.NoError(t, err)

	// Never submitted: a retry is legitimate.
	f.toManualReview(req, "eligibility needed a human")
	retried, err := f.resolve(req.ID, payout.ResolveRetryVerification, "human confirmed eligibility")
	require.NoError(t, err)
	require.Equal(t, payout.StateVerified, retried.State)

	// Now submit it, force it back to MANUAL_REVIEW, and the same retry is
	// refused -- because the provider may already hold it.
	f.provider.TimeoutNext()
	_, _ = f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	f.toManualReview(req, "submission outcome unknown")

	_, err = f.resolve(req.ID, payout.ResolveRetryVerification, "let us just try again")
	require.Error(t, err, "a payout that may already be at the provider must never be retried")
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))

	current, err := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.Equal(t, payout.StateManualReview, current.State)
}

// TestIntegration_APayoutIsRefusedWhileTheFundingIsDisputed is PART LXXII item
// 9: a payout request during a pending chargeback.
//
// The user's Credits are still there and still spendable inside the system.
// What has changed is that the money behind them is being clawed back, and
// paying out against a disputed funding is how a platform pays the same money
// twice — once to the user and once back to the card network.
func TestIntegration_APayoutIsRefusedWhileTheFundingIsDisputed(t *testing.T) {
	f := newFixture(t)
	lot := f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)

	// While settled, the policy permits it.
	req, dec, err := f.create(400, f.input())
	require.NoError(t, err)
	require.Equal(t, "400", req.ReservedQuantity.String(), "reasons=%v", dec.ReasonStrings())

	// A chargeback opens on the funding behind those Credits.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.SetFinality(ctx, tx, lot.ID, valuedomain.FinalityDisputed,
				credit.Reference{Type: "test_dispute", ID: uuid.NewString()}, "chargeback opened")
		}))

	// A NEW payout against the remaining balance is refused. A refusal is a
	// DECISION here, not an error -- the request exists, in REJECTED, with the
	// reasons attached -- and the reason names the funding state rather than
	// reporting a bare shortfall.
	rejected, dec2, err := f.create(400, f.input())
	require.NoError(t, err)
	require.False(t, dec2.Sufficient(), "disputed funding must not be payable")
	require.Equal(t, payout.StateRejected, rejected.State)
	require.Contains(t, dec2.Reasons, valuedomain.ReasonFundingNotFinal,
		"the refusal must say the funding is not final, not just 'insufficient': %v", dec2.ReasonStrings())
	require.Equal(t, "0", rejected.ReservedQuantity.String(),
		"a rejected payout reserves nothing")

	// And the first payout, already reserved, is not silently settled by the
	// dispute: it is still holding its reservation for a human or a
	// reconciliation to resolve.
	current, err := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.False(t, current.State.Terminal(), "a dispute must not terminate a payout by itself")
	require.Equal(t, "400", current.ReservedQuantity.String())
}

// TestIntegration_ACompromisedProviderCannotRewriteAFinishedPayout is PART
// LXXII item 23.
//
// The scenario is a provider that changes its story after the fact: it settled
// a payout and now says it failed, or it failed one and now says it settled.
// Either could be a compromise, a bug or a bad migration on their side; from
// here they are indistinguishable, and the only safe reading of "the provider
// contradicts itself" is that the provider is currently not authoritative.
//
// So the recorded outcome does not move. It is not re-posted, not reversed and
// not quietly corrected — every one of those would let whoever controls the
// provider's responses move Nodal's money by lying twice. What DOES happen is
// that the contradiction is recorded and surfaced as an error, because the
// failure this test was written after was the opposite one: Reconcile returned
// early on a finished payout without asking anything, so a provider that
// changed its answer left no trace at all.
func TestIntegration_ACompromisedProviderCannotRewriteAFinishedPayout(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(400, f.input())
	require.NoError(t, err)

	// The platform PAYOUT_SETTLED account is shared by every payout in this
	// database, so what matters is the change this payout makes to it.
	beforeSubmit := f.platformBalance(ledger.CodePayoutSettled)
	settled, err := f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)
	require.Equal(t, payout.StateSettled, settled.State)
	require.NotEmpty(t, settled.ProviderIdempotencyKey)
	afterSubmit := f.platformBalance(ledger.CodePayoutSettled)
	require.Equal(t, "400", afterSubmit.Sub(beforeSubmit).String())

	// The provider now says it failed.
	f.provider.Corrupt(settled.ProviderIdempotencyKey, payout.SubmitResult{
		Status: payout.ProviderFailed, RawStatus: "failed",
		FailureReason: "no such payout",
	})

	after, err := f.svc.Reconcile(f.ctx, testDB, req.ID)
	require.Error(t, err, "a provider contradicting a settled payout must not pass in silence")
	require.Equal(t, errs.CodeReconciliationRequired, errs.CodeOf(err))
	require.Contains(t, err.Error(), "recorded as settled")

	require.Equal(t, payout.StateSettled, after.State,
		"the recorded outcome must not follow the provider's new story")
	require.Equal(t, "400", after.SettledQuantity.String())
	require.Equal(t, afterSubmit.String(), f.platformBalance(ledger.CodePayoutSettled).String(),
		"no compensating posting may be made on the word of a contradictory provider")
	require.Equal(t, "0", f.balance(ledger.CodePayoutReserved).String())
	require.Equal(t, "600", f.balance(ledger.CodeCreditBalance).String(),
		"the customer's Credits must not come back because the provider changed its mind")
	require.NoError(t, f.svc.VerifyReservations(f.ctx, testDB, f.creditAsset))
	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.account))

	// And the contradiction is on file. An operator who has to decide what
	// really happened needs the provider's own words, not our summary of them.
	require.Equal(t, 1, f.providerEvents(req.ID, "failed"),
		"the contradictory answer must be recorded as a provider event")
}

// TestIntegration_AProviderClaimingItPaidAFailedPayoutIsNotBelieved is the
// other direction, and the more expensive one: the reservation has already
// been returned to the customer, so believing the provider would mean the
// units are spendable AND gone.
func TestIntegration_AProviderClaimingItPaidAFailedPayoutIsNotBelieved(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(400, f.input())
	require.NoError(t, err)

	settledBefore := f.platformBalance(ledger.CodePayoutSettled)
	f.provider.FailNext("account closed at the receiving bank")
	failed, err := f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)
	require.Equal(t, payout.StateFailed, failed.State)
	require.Equal(t, "1000", f.balance(ledger.CodeCreditBalance).String())

	f.provider.Corrupt(failed.ProviderIdempotencyKey, payout.SubmitResult{
		Status: payout.ProviderSettled, ProviderReference: "sbx-invented", RawStatus: "settled",
	})

	after, err := f.svc.Reconcile(f.ctx, testDB, req.ID)
	require.Error(t, err)
	require.Equal(t, errs.CodeReconciliationRequired, errs.CodeOf(err))
	require.Contains(t, err.Error(), "recorded as failed")

	require.Equal(t, payout.StateFailed, after.State)
	require.Equal(t, "0", after.SettledQuantity.String())
	require.Equal(t, "1000", f.balance(ledger.CodeCreditBalance).String(),
		"the returned units stay returned")
	require.Equal(t, settledBefore.String(), f.platformBalance(ledger.CodePayoutSettled).String(),
		"nothing may be posted as settled on a claim that contradicts our record")
	require.NoError(t, f.svc.VerifyReservations(f.ctx, testDB, f.creditAsset))
	require.NoError(t, f.credits.VerifyProvenance(f.ctx, testDB, f.account))
	require.Equal(t, 1, f.providerEvents(req.ID, "settled"))
}

// TestIntegration_AProviderSwappingTheReferenceOfASettledPayoutContradictsItself:
// the status still says SETTLED, so nothing looks wrong at a glance, but the
// payout being described is a different one. This is what a compromised
// provider stitching two payouts together looks like.
func TestIntegration_AProviderSwappingTheReferenceOfASettledPayoutContradictsItself(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(400, f.input())
	require.NoError(t, err)

	settled, err := f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)
	require.NotEmpty(t, settled.ProviderReference)

	f.provider.Corrupt(settled.ProviderIdempotencyKey, payout.SubmitResult{
		Status: payout.ProviderSettled, ProviderReference: "sbx-somebody-elses-payout",
		RawStatus: "settled",
	})

	after, err := f.svc.Reconcile(f.ctx, testDB, req.ID)
	require.Error(t, err)
	require.Equal(t, errs.CodeReconciliationRequired, errs.CodeOf(err))
	require.Contains(t, err.Error(), "different reference")
	require.Equal(t, payout.StateSettled, after.State)
	require.Equal(t, settled.ProviderReference, after.ProviderReference,
		"the reference on file is the one recorded when the payout settled")
}

// TestIntegration_AProviderThatAgreesWithASettledPayoutIsNotAContradiction is
// the control. Without it the three tests above would also pass against a
// Reconcile that called every terminal payout a contradiction, which would be
// an alarm with no information in it.
func TestIntegration_AProviderThatAgreesWithASettledPayoutIsNotAContradiction(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(400, f.input())
	require.NoError(t, err)

	settled, err := f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)
	require.Equal(t, payout.StateSettled, settled.State)
	settledOut := f.platformBalance(ledger.CodePayoutSettled)

	after, err := f.svc.Reconcile(f.ctx, testDB, req.ID)
	require.NoError(t, err, "an agreeing provider is not a contradiction")
	require.Equal(t, payout.StateSettled, after.State)
	require.Equal(t, settledOut.String(), f.platformBalance(ledger.CodePayoutSettled).String(),
		"re-reconciling a settled payout must not post it twice")
	require.Equal(t, 2, f.providerEvents(req.ID, "settled"),
		"the agreeing answer is recorded too — the submission's and the reconcile's; "+
			"only the interpretation of a disagreeing one differs")
}

// platformBalance reads a platform-side ledger balance for the credit asset.
func (f *fixture) platformBalance(code ledger.Code) money.Quantity {
	f.t.Helper()
	var raw string
	require.NoError(f.t, testDB.QueryRow(f.ctx,
		`SELECT coalesce((SELECT b.balance FROM ledger_accounts la
		    JOIN ledger_balances b ON b.ledger_account_id = la.id
		   WHERE la.owner_type='PLATFORM' AND la.code=$1 AND la.asset_id=$2), 0)::text`,
		string(code), f.creditAsset).Scan(&raw))
	v, err := money.ParseQuantity(raw)
	require.NoError(f.t, err)
	return v
}

// providerEvents counts recorded provider events for a request with a raw
// status. It reads the audit trail rather than the service's return value,
// because "the contradiction was recorded" is a claim about the database.
func (f *fixture) providerEvents(id payout.RequestID, rawStatus string) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM payout_provider_events
		  WHERE request_id = $1 AND provider_status = $2 AND direction = 'RESPONSE'`,
		id, rawStatus).Scan(&n))
	return n
}

// An idempotency key belongs to one account (F-106).
//
// payout_requests.idempotency_key is globally UNIQUE, and the HTTP boundary's
// own idempotency record is keyed by (actor, endpoint, key) -- so a DIFFERENT
// caller reusing a key passes the boundary and arrives in the domain. Create
// returned the row it found without asking whose it was, which rendered another
// account's payout to the caller: its account id, its requested, reserved and
// settled quantities, its destination and its failure reason. It also silently
// discarded the caller's own request, and told them a payout existed that they
// had never made.
//
// internal/credit, internal/funding, internal/withdrawal and internal/capital
// all make this comparison. Three tables did not.
func TestIntegration_AnIdempotencyKeyBelongsToOneAccount(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 50_000)

	const key = "shared-key-0001"
	first, _, err := f.createWithKey(f.account, key, 10_000)
	require.NoError(t, err)
	require.False(t, first.ID.IsZero())

	// A second account, same key.
	other := newAccount(t)
	_, _, err = f.createWithKey(other, key, 10_000)
	require.Error(t, err, "another account's payout was returned as this caller's replay")
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))

	// The control: the owner's own retry is still idempotent, which is the
	// whole point of the key and must not have been broken by scoping it.
	again, _, err := f.createWithKey(f.account, key, 10_000)
	require.NoError(t, err)
	assert.Equal(t, first.ID, again.ID, "the owner's retry stopped being a replay")
}

func (f *fixture) createWithKey(account accounts.AccountID, key string, amount int64) (payout.Request, payout.Decision, error) {
	var (
		req payout.Request
		dec payout.Decision
	)
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			dest := f.destination
			in := f.input()
			in.AccountID = account
			req, dec, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: account, DestinationID: &dest, Quantity: q(amount),
				DisclosureAccepted: true,
				IdempotencyKey:     key, EffectiveAt: f.clk.Now(),
			}, in)
			return cerr
		})
	return req, dec, err
}

// A payout that has ever been submitted cannot be cancelled, whatever state it
// is in now (F-107).
//
// Cancel guarded on the CURRENT state and MANUAL_REVIEW is not in that list --
// but applyProviderResult parks a payout there precisely when the provider WAS
// called and the settlement could not be recorded: a lost response, or a ledger
// posting that refused. So a payout the provider has paid could sit in
// MANUAL_REVIEW, and the account owner could release the reservation and get
// their Credits back while the money was already gone.
//
// The asymmetry is what makes it a defect rather than a gap: the dual-controlled
// ResolveManualReview already consulted everSubmitted and refused to retry an
// ever-submitted payout. The single-user endpoint would unwind one.
func TestIntegration_APaidPayoutParkedForReviewCannotBeCancelled(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(600, f.input())
	require.NoError(t, err)
	require.Equal(t, "600", f.balance(ledger.CodePayoutReserved).String())

	// Submitted, and the response never came back.
	f.provider.TimeoutNext()
	_, err = f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)

	// A person parks it, which is the ordinary thing to do with a payout whose
	// outcome nobody knows.
	parked := f.toManualReview(req, "provider did not answer")
	require.Equal(t, payout.StateManualReview, parked.State)

	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := f.svc.Cancel(ctx, tx, req.ID, "user changed their mind")
			return e
		})
	require.Error(t, err, "the reservation was released on a payout the provider may have paid")
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	assert.Equal(t, "600", f.balance(ledger.CodePayoutReserved).String(),
		"the value came back to the user while the provider may already have sent it")
	assert.Equal(t, "400", f.balance(ledger.CodeCreditBalance).String())
}

// "Do not resubmit" does not resubmit (F-115).
//
// Phase one's early-return branch says "Already claimed by an earlier attempt.
// Reconcile, do not resubmit" for SUBMITTED, PROVIDER_PENDING and
// PAYOUT_STATUS_UNKNOWN. Phase two then guarded on `req.State != StateSubmitted`
// -- which sent the last two home and let SUBMITTED fall straight through to the
// provider call.
//
// SUBMITTED is precisely what a crash between the phase-one commit and
// applyProviderResult leaves behind, so the branch whose comment says do not
// resubmit was the one that did. The covering test never reached it: after a
// timeout the state is PAYOUT_STATUS_UNKNOWN, which returns.
//
// The only thing that stood between this and paying twice was the provider
// honouring the idempotency key -- which the sandbox does by construction, so
// the test double hid it.
func TestIntegration_ASecondSubmitDoesNotCallTheProviderAgain(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(600, f.input())
	require.NoError(t, err)

	// A submission that lands the request in SUBMITTED and stays there: the
	// provider answered, but the process died before the answer was recorded.
	//
	// The crash is produced rather than forged. An earlier version wrote the
	// state by hand, which meant the fixture was asserting against a row no
	// crash could actually leave -- and migration 00807 now refuses it, because
	// PAYOUT_STATUS_UNKNOWN -> SUBMITTED is not an edge (F-226).
	f.provider.CrashNext()
	require.Panics(t, func() { _, _ = f.svc.Submit(f.ctx, testDB, req.ID, "sandbox") },
		"fixture check: the provider takes the payout and the process then dies")
	before := f.provider.Submits()
	require.Positive(t, before)
	crashed, err := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.Equal(t, payout.StateSubmitted, crashed.State,
		"fixture check: phase one committed SUBMITTED before the provider was called")

	after, err := f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)
	assert.Equal(t, before, f.provider.Submits(),
		"a second Submit called the provider again on a payout it had already claimed")
	assert.Equal(t, payout.StateSubmitted, after.State)
}

// A payout is submitted to the provider it was claimed for (F-115).
//
// providerName is a caller argument and was compared to nothing. On the
// re-entry branch it was not even written back, so a second call naming another
// provider would have handed that provider the FIRST one's idempotency key:
// two providers, one key, two disbursements, neither able to dedupe the other.
func TestIntegration_APayoutGoesToTheProviderItWasClaimedFor(t *testing.T) {
	f := newFixture(t)
	other := payouttest.NewSandbox("sandbox-two")
	require.NoError(t, f.registry.Register(other))

	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)
	req, _, err := f.create(600, f.input())
	require.NoError(t, err)

	f.provider.TimeoutNext()
	_, err = f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err)

	_, err = f.svc.Submit(f.ctx, testDB, req.ID, "sandbox-two")
	require.Error(t, err, "a payout claimed for one provider was submitted to another")
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	assert.Zero(t, other.Submits(), "the second provider was called with the first's idempotency key")
}
