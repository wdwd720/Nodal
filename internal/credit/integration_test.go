//go:build integration

package credit

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
	// testOwnerDB is the migration role, and it exists for exactly one thing:
	// backdating a money timestamp so a settlement window can be observed
	// closing without moving this process's clock.
	//
	// It is separate because 00743 revoked UPDATE on credit_fundings from
	// cp_app and granted back only lot_id and provider_reference. Two tests
	// were backdating reversible_at through testDB -- the APPLICATION pool --
	// which is precisely the write that must be impossible, since reversible_at
	// is what the settlement window is measured from and moving it settles
	// money early. The tests were right about what they needed and wrong about
	// who should do it: rewinding a clock the database owns is an operator
	// action.
	testOwnerDB *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "credit integration: migrate up:", err)
		return 1
	}
	var err error
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "credit-itest", MaxConns: 30})
	if err != nil {
		fmt.Fprintln(os.Stderr, "credit integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	testOwnerDB, err = db.Open(ctx, db.Config{URL: testMigrateURL, AppName: "credit-itest-owner", MaxConns: 2})
	if err != nil {
		fmt.Fprintln(os.Stderr, "credit integration: open owner pool:", err)
		return 1
	}
	defer testOwnerDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set")
	}
}

// creditAssetOnce provisions the single CREDIT asset for the whole suite.
// Migration 00711 permits exactly one, which is the point, so it cannot be
// per-test.
var (
	creditAssetOnce sync.Once
	creditAssetID   assets.AssetID
)

func creditAsset(t *testing.T) assets.AssetID {
	t.Helper()
	creditAssetOnce.Do(func() {
		ctx := context.Background()
		var existing assets.AssetID
		err := testDB.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing)
		if err == nil {
			creditAssetID = existing
			return
		}
		created, cerr := assets.NewRepository().Create(ctx, testDB, assets.Asset{
			Chain:       assets.InternalChain,
			Kind:        assets.KindCredit,
			ValueDomain: valuedomain.InternalCredit,
			Symbol:      "CREDIT",
			Name:        "Nodal Credit",
			// Six decimals: the quote asset of a bonding-curve market has to
			// price units far below one Credit, and a coarser scale would make
			// rounding-to-zero a usable exploit.
			Decimals:  6,
			RiskClass: assets.RiskUnsupported,
			Status:    assets.StatusActive,
		})
		require.NoError(t, cerr)
		creditAssetID = created.ID
	})
	require.False(t, creditAssetID.IsZero())
	return creditAssetID
}

type fixture struct {
	t       *testing.T
	ctx     context.Context
	svc     *Service
	led     *ledger.Service
	clk     *clock.Fake
	account accounts.AccountID
	asset   assets.AssetID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	led := ledger.NewService(clk, "credit-itest")
	led.AllowSeedPostings()
	return &fixture{
		t: t, ctx: ctx, clk: clk, led: led,
		svc:     NewService(led, clk),
		account: createAccount(t),
		asset:   creditAsset(t),
	}
}

func createAccount(t *testing.T) accounts.AccountID {
	t.Helper()
	repo := accounts.NewRepository()
	user, err := repo.CreateUser(context.Background(), testDB, "credit-itest", uuid.NewString(), nil)
	require.NoError(t, err)
	acct, err := repo.CreateAccount(context.Background(), testDB, user.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return acct.ID
}

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

// issue mints a lot in its own transaction.
func (f *fixture) issue(origin valuedomain.CreditOrigin, fin valuedomain.FundingFinality, qty int64) Lot {
	f.t.Helper()
	var lot Lot
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		lot, err = f.svc.Issue(ctx, tx, IssueRequest{
			AccountID:      f.account,
			Quantity:       q(qty),
			Origin:         origin,
			Finality:       fin,
			Reference:      Reference{Type: "test_issue", ID: uuid.NewString()},
			IdempotencyKey: "issue-" + uuid.NewString(),
			Reason:         "test",
			EffectiveAt:    f.clk.Now(),
		})
		return err
	})
	require.NoError(f.t, err)
	return lot
}

// spend posts a single-domain movement of Credits out of the account and
// consumes the matching lots, exactly as a real spend does.
func (f *fixture) spend(qty int64, allowed ...valuedomain.CreditOrigin) ([]Allocation, error) {
	var allocs []Allocation
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		res, err := f.led.Post(ctx, tx, ledger.Posting{
			Kind:           ledger.KindCreditSpent,
			IdempotencyKey: "spend-" + uuid.NewString(),
			Reference:      ledger.FinancialEventReference{Type: "test_spend", ID: uuid.NewString()},
			EffectiveAt:    f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(f.account, ledger.CodeCreditBalance, f.asset), Side: ledger.Credit, Quantity: q(qty)},
				{Account: ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, f.asset), Side: ledger.Debit, Quantity: q(qty)},
			},
		})
		if err != nil {
			return err
		}
		allocs, err = f.svc.Consume(ctx, tx, ConsumeRequest{
			AccountID:                f.account,
			Quantity:                 q(qty),
			JournalTxID:              res.TransactionID,
			Reference:                Reference{Type: "test_spend", ID: uuid.NewString()},
			Reason:                   "test spend",
			RequireSpendableFinality: true,
			AllowedOrigins:           allowed,
		})
		return err
	})
	return allocs, err
}

// ---------------------------------------------------------------------------

func TestIntegration_IssueRecordsProvenanceAndBalance(t *testing.T) {
	f := newFixture(t)
	lot := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 1000)

	require.Equal(t, "1000", lot.Quantity.String())
	require.Equal(t, "1000", lot.Remaining.String())
	require.Equal(t, valuedomain.OriginPromotional, lot.Origin)
	require.False(t, lot.JournalTxID.IsZero(), "a lot must name the posting that moved the units")

	require.NoError(t, f.svc.VerifyProvenance(f.ctx, testDB, f.account),
		"lot remaining must equal CREDIT_BALANCE immediately after issuance")
}

// TestIntegration_ALotMustBeBackedByItsJournalTransaction proves SQLSTATE
// CR004: provenance for units the journal never moved is refused by the
// database, not merely avoided by the service.
func TestIntegration_ALotMustBeBackedByItsJournalTransaction(t *testing.T) {
	f := newFixture(t)
	other := createAccount(t)
	lot := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 500)

	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO credit_lots (id, account_id, asset_id, origin, initial_finality, quantity,
			     journal_transaction_id, issued_by_actor_type, issued_by_actor_id)
			 VALUES ($1,$2,$3,'CREATOR_EARNING','SETTLED',999999,$4,'SYSTEM','forged')`,
			NewLotID(), other, f.asset, lot.JournalTxID)
		return err
	})
	require.Error(t, err, "a lot for an account the journal transaction never touched must be refused")
	require.Contains(t, err.Error(), "CREDIT_LOT_UNBACKED")
}

func TestIntegration_ConsumptionTakesTheMostRestrictedValueFirst(t *testing.T) {
	f := newFixture(t)
	// Issued deliberately out of consumption order so the ordering under test
	// cannot be satisfied by insertion order.
	earning := f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 100)
	promo := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 100)
	purchased := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 100)

	allocs, err := f.spend(250)
	require.NoError(t, err)
	require.Len(t, allocs, 3)

	require.Equal(t, promo.ID, allocs[0].LotID, "promotional value is consumed first")
	require.Equal(t, "100", allocs[0].Quantity.String())
	require.Equal(t, purchased.ID, allocs[1].LotID, "purchased value is consumed before earnings")
	require.Equal(t, "100", allocs[1].Quantity.String())
	require.Equal(t, earning.ID, allocs[2].LotID, "earned value is consumed last")
	require.Equal(t, "50", allocs[2].Quantity.String())

	after, err := f.svc.Lot(f.ctx, testDB, earning.ID)
	require.NoError(t, err)
	require.Equal(t, "50", after.Remaining.String(),
		"ordinary spending must leave a creator's earnings largely intact")
	require.NoError(t, f.svc.VerifyProvenance(f.ctx, testDB, f.account))
}

func TestIntegration_ConsumptionCanBeRestrictedToApprovedOrigins(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 1000)
	earning := f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 100)

	allocs, err := f.spend(100, valuedomain.OriginCreatorEarning)
	require.NoError(t, err)
	require.Len(t, allocs, 1)
	require.Equal(t, earning.ID, allocs[0].LotID,
		"a payout restricted to creator earnings must not quietly take the promotional grant")

	// And it cannot exceed what those origins hold, even though the account
	// has plenty of other Credits.
	_, err = f.spend(50, valuedomain.OriginCreatorEarning)
	require.Error(t, err)
	require.Equal(t, errs.CodeInsufficientBuyingPower, errs.CodeOf(err))
}

func TestIntegration_DisputedValueIsNotSpendable(t *testing.T) {
	f := newFixture(t)
	lot := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 500)

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.svc.SetFinality(ctx, tx, lot.ID, valuedomain.FinalityDisputed,
				Reference{Type: "test", ID: uuid.NewString()}, "chargeback opened")
		}))

	_, err := f.spend(100)
	require.Error(t, err, "Credits whose funding is under dispute must not fund new activity")
	require.Equal(t, errs.CodeInsufficientBuyingPower, errs.CodeOf(err))
}

func TestIntegration_FinalityTransitionsAreEnforcedInGoAndInSQL(t *testing.T) {
	f := newFixture(t)
	lot := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 100)

	// Go refuses the illegal transition with a typed error.
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.svc.SetFinality(ctx, tx, lot.ID, valuedomain.FinalityUnfunded,
				Reference{Type: "test", ID: uuid.NewString()}, "should not be possible")
		})
	require.Error(t, err)
	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// The database refuses it too, for anything that does not come through Go.
	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx,
				`INSERT INTO credit_lot_events (id, lot_id, seq, kind, to_finality, reference_type, reference_id, actor_type, actor_id)
				 SELECT $1, $2, event_count + 1, 'FINALITY', 'UNFUNDED', 'forged', 'forged', 'SYSTEM', 'forged'
				   FROM credit_lot_state WHERE lot_id = $2`, NewLotEventID(), lot.ID)
			return e
		})
	require.Error(t, err)
	require.Contains(t, err.Error(), "CREDIT_FINALITY_ILLEGAL")

	// REVERSED is terminal on both sides.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.svc.SetFinality(ctx, tx, lot.ID, valuedomain.FinalityReversed,
				Reference{Type: "test", ID: uuid.NewString()}, "reversed")
		}))
	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.svc.SetFinality(ctx, tx, lot.ID, valuedomain.FinalitySettled,
				Reference{Type: "test", ID: uuid.NewString()}, "resurrect")
		})
	require.Error(t, err, "reversed value must never come back to life")
}

func TestIntegration_ALotCannotBeOverconsumed(t *testing.T) {
	f := newFixture(t)
	lot := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 100)

	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx,
				`INSERT INTO credit_lot_events (id, lot_id, seq, kind, delta_quantity, reference_type, reference_id, actor_type, actor_id)
				 SELECT $1, $2, event_count + 1, 'CONSUME', 101, 'forged', 'forged', 'SYSTEM', 'forged'
				   FROM credit_lot_state WHERE lot_id = $2`, NewLotEventID(), lot.ID)
			return e
		})
	require.Error(t, err)
	require.Contains(t, err.Error(), "CREDIT_LOT_OVERCONSUMED")

	// And restoring more than was issued is refused too, which is what stops a
	// cancelled payout from inflating a lot.
	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx,
				`INSERT INTO credit_lot_events (id, lot_id, seq, kind, delta_quantity, reference_type, reference_id, actor_type, actor_id)
				 SELECT $1, $2, event_count + 1, 'RESTORE', 1, 'forged', 'forged', 'SYSTEM', 'forged'
				   FROM credit_lot_state WHERE lot_id = $2`, NewLotEventID(), lot.ID)
			return e
		})
	require.Error(t, err)
	require.Contains(t, err.Error(), "CREDIT_LOT_OVERRESTORED")
}

// TestIntegration_HundredConcurrentSpendsAgainstInsufficientBalance is
// acceptance test FIN-002 and PART LXXII item 1.
//
// The account holds 10,000 Credits. One hundred goroutines each try to spend
// 1,000. Exactly ten must succeed. Not nine — that would mean the system loses
// value to contention. Not eleven — that would mean it creates value.
func TestIntegration_HundredConcurrentSpendsAgainstInsufficientBalance(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 10_000)

	const workers = 100
	const each = 1_000
	var succeeded, failed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := f.spend(each); err != nil {
				failed.Add(1)
				return
			}
			succeeded.Add(1)
		}()
	}
	close(start)
	wg.Wait()

	require.EqualValues(t, 10, succeeded.Load(),
		"exactly ten spends of 1,000 fit in 10,000 Credits; got %d succeeded, %d failed",
		succeeded.Load(), failed.Load())
	require.EqualValues(t, workers-10, failed.Load())

	bal, err := f.svc.creditBalance(f.ctx, testDB, f.account, f.asset)
	require.NoError(t, err)
	require.Equal(t, "0", bal.String())
	require.NoError(t, f.svc.VerifyProvenance(f.ctx, testDB, f.account),
		"provenance and balance must still agree after 100 concurrent spenders")
}

// TestIntegration_ChargebackAfterTheCreditsAreSpent is PART LXXII item 8 and
// the economics of PART XI.
func TestIntegration_ChargebackAfterTheCreditsAreSpent(t *testing.T) {
	f := newFixture(t)

	var funding Funding
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			funding, err = f.svc.CreateFunding(ctx, tx, CreateFundingRequest{
				AccountID:      f.account,
				Provider:       "test-provider",
				ProviderMode:   "fake",
				CreditQuantity: q(10_000),
				PaidAmount:     money.USDFromMinor(10_000),
				IdempotencyKey: "funding-" + uuid.NewString(),
			})
			return err
		}))

	// Walk the funding to CAPTURED, then mint.
	for _, to := range []FundingState{FundingAuthorizationPending, FundingAuthorized, FundingCapturePending, FundingCaptured} {
		require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := f.svc.AdvanceFunding(ctx, tx, funding.ID, to, "provider webhook", "")
				return err
			}))
	}
	var lot Lot
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			lot, err = f.svc.MintFrom(ctx, tx, funding.ID, f.clk.Now())
			return err
		}))
	require.Equal(t, valuedomain.FinalityReversible, lot.Finality,
		"a captured card payment is not a settled one")

	// The user spends 7,000 of the 10,000.
	_, err := f.spend(7_000)
	require.NoError(t, err)

	// Then charges back.
	var res ReverseResult
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = f.svc.Reverse(ctx, tx, funding.ID, f.clk.Now(), "chargeback received")
			return err
		}))

	require.Equal(t, "3000", res.Destroyed.String(), "what remained is clawed back")
	require.Equal(t, "7000", res.Deficit.String(),
		"what was already spent becomes a recorded debt, not an invisible hole")

	bal, err := f.svc.creditBalance(f.ctx, testDB, f.account, f.asset)
	require.NoError(t, err)
	require.Equal(t, "0", bal.String(), "the balance must not go negative")

	var deficit string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT coalesce(b.balance, 0)::text FROM ledger_accounts la
		   JOIN ledger_balances b ON b.ledger_account_id = la.id
		  WHERE la.owner_type='CUSTOMER' AND la.owner_id=$1 AND la.code='DEFICIT' AND la.asset_id=$2`,
		f.account, f.asset).Scan(&deficit))
	require.Equal(t, "7000", deficit)

	after, err := f.svc.Lot(f.ctx, testDB, lot.ID)
	require.NoError(t, err)
	require.Equal(t, valuedomain.FinalityReversed, after.Finality)

	final, err := f.svc.Funding(f.ctx, testDB, funding.ID)
	require.NoError(t, err)
	require.Equal(t, FundingReversed, final.State)
}

// TestIntegration_MintingIsExactlyOnceUnderWebhookReplay covers PART LXXII
// item 4: a duplicated provider webhook must not mint twice.
func TestIntegration_MintingIsExactlyOnceUnderWebhookReplay(t *testing.T) {
	f := newFixture(t)
	var funding Funding
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			funding, err = f.svc.CreateFunding(ctx, tx, CreateFundingRequest{
				AccountID: f.account, Provider: "test-provider", ProviderMode: "fake",
				CreditQuantity: q(2_500), PaidAmount: money.USDFromMinor(2_500),
				IdempotencyKey: "funding-" + uuid.NewString(),
			})
			return err
		}))
	for _, to := range []FundingState{FundingAuthorizationPending, FundingAuthorized, FundingCapturePending, FundingCaptured} {
		require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := f.svc.AdvanceFunding(ctx, tx, funding.ID, to, "webhook", "")
				return err
			}))
	}

	mint := func() (Lot, error) {
		var lot Lot
		err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				var err error
				lot, err = f.svc.MintFrom(ctx, tx, funding.ID, f.clk.Now())
				return err
			})
		return lot, err
	}
	first, err := mint()
	require.NoError(t, err)
	second, err := mint()
	require.NoError(t, err, "a replayed webhook must be a no-op, not an error the provider retries forever")
	require.Equal(t, first.ID, second.ID)

	bal, err := f.svc.creditBalance(f.ctx, testDB, f.account, f.asset)
	require.NoError(t, err)
	require.Equal(t, "2500", bal.String(), "the replay must not have minted a second time")

	// A repeated terminal-state webhook is also a no-op.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.AdvanceFunding(ctx, tx, funding.ID, FundingReversible, "duplicate webhook", "")
			return err
		}))
}

func TestIntegration_FundingStateChangeRequiresItsTransitionRow(t *testing.T) {
	f := newFixture(t)
	var funding Funding
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			funding, err = f.svc.CreateFunding(ctx, tx, CreateFundingRequest{
				AccountID: f.account, Provider: "test-provider", ProviderMode: "fake",
				CreditQuantity: q(100), PaidAmount: money.USDFromMinor(100),
				IdempotencyKey: "funding-" + uuid.NewString(),
			})
			return err
		}))

	// The refusal got stronger in 00743 and this assertion moved with it.
	//
	// It used to be AUDIT_TRANSITION_REQUIRED, raised by the deferred audit
	// binding at COMMIT: the application COULD write the column and was caught
	// afterwards. It is now `permission denied`, raised by PostgreSQL at the
	// statement: cp_app holds UPDATE on lot_id and provider_reference and on
	// nothing else, so there is no statement it can issue that writes `state`.
	//
	// Detection became privilege, which is the whole point of F-42's remaining
	// remedy, and a test still expecting the weaker error would have been the
	// thing telling us the stronger one had not landed.
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `UPDATE credit_fundings SET state = 'SETTLED' WHERE id = $1`, funding.ID)
			return e
		})
	require.Error(t, err, "a bare state update must be refused")
	require.Contains(t, err.Error(), "permission denied",
		"the application can still write credit_fundings.state; 00743 did not take")

	// And the stamps went with it, for the same reason: reversible_at is what
	// the settlement window is measured from.
	for _, col := range []string{"reversible_at = now()", "settled_at = now()", "reversed_at = now()", "failure_reason = 'x'"} {
		err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(ctx, `UPDATE credit_fundings SET `+col+` WHERE id = $1`, funding.ID)
				return e
			})
		require.Error(t, err, "cp_app can write %s", col)
		require.Contains(t, err.Error(), "permission denied", col)
	}

	// The two it may still write, so the revoke is a boundary and not a wall.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `UPDATE credit_fundings SET provider_reference = 'pr-1' WHERE id = $1`, funding.ID)
			return e
		}), "cp_app must still record the provider reference")
}

func TestIntegration_BalancesExplainWhatIsAndIsNotWithdrawable(t *testing.T) {
	f := newFixture(t)
	f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 400)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 100)
	reversible := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 250)

	policy := valuedomain.DefaultPolicy()
	policy.Version = "test-creator-payout"
	policy.Rules[valuedomain.OriginCreatorEarning] = valuedomain.OriginRule{
		PayoutAllowed:        true,
		RequiredCapability:   "PAYOUT_CREATOR_EARNINGS",
		RequiredVerification: valuedomain.VerificationPayoutKYC,
	}
	req := BalanceRequest{
		AccountID:  f.account,
		Policy:     policy,
		Verified:   valuedomain.VerificationPayoutKYC,
		ActiveCaps: map[valuedomain.CapabilityKey]bool{"PAYOUT_CREATOR_EARNINGS": true},
		Now:        f.clk.Now().Add(48 * time.Hour),
	}
	b, err := f.svc.Balances(f.ctx, testDB, req)
	require.NoError(t, err)

	require.Equal(t, "750", b.Gross.String())
	require.Equal(t, "750", b.Spendable.String())
	require.Equal(t, "100", b.PayoutEligible.String(),
		"only the creator earning is withdrawable under this policy")
	require.Equal(t, "650", b.Ineligible.String())
	require.Equal(t, "400", b.ByOrigin[valuedomain.OriginPromotional].String())
	require.Equal(t, "250", b.ByFinality[valuedomain.FinalityReversible].String())
	require.Equal(t, policy.Version, b.PolicyVersion)
	require.NotEmpty(t, b.PolicyHash)

	// The reasons must name the promotional grant's origin and the purchased
	// lot's reversible funding, which are different problems.
	require.Contains(t, b.IneligibleReasons, valuedomain.ReasonOriginForbidden)
	require.Contains(t, b.IneligibleReasons, valuedomain.ReasonFundingNotFinal)

	// Under the default fail-closed policy nothing is withdrawable at all.
	req.Policy = valuedomain.DefaultPolicy()
	closed, err := f.svc.Balances(f.ctx, testDB, req)
	require.NoError(t, err)
	require.Equal(t, "0", closed.PayoutEligible.String())

	// Disputing the purchased lot moves it out of spendable and into frozen.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.svc.SetFinality(ctx, tx, reversible.ID, valuedomain.FinalityDisputed,
				Reference{Type: "test", ID: uuid.NewString()}, "dispute")
		}))
	b2, err := f.svc.Balances(f.ctx, testDB, req)
	require.NoError(t, err)
	require.Equal(t, "250", b2.Frozen.String())
	require.Equal(t, "500", b2.Spendable.String())
	require.Equal(t, "750", b2.Gross.String(), "frozen value is still held, just not usable")
}

func TestIntegration_RestoreReturnsUnitsToTheirOwnLots(t *testing.T) {
	f := newFixture(t)
	promo := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 100)
	earning := f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 100)

	allocs, err := f.spend(150)
	require.NoError(t, err)
	require.Len(t, allocs, 2)

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			res, err := f.led.Post(ctx, tx, ledger.Posting{
				Kind:           ledger.KindPayoutReturned,
				IdempotencyKey: "restore-" + uuid.NewString(),
				Reference:      ledger.FinancialEventReference{Type: "test_restore", ID: uuid.NewString()},
				EffectiveAt:    f.clk.Now(),
				Entries: []ledger.Entry{
					{Account: ledger.CustomerAccount(f.account, ledger.CodeCreditBalance, f.asset), Side: ledger.Debit, Quantity: q(150)},
					{Account: ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, f.asset), Side: ledger.Credit, Quantity: q(150)},
				},
			})
			if err != nil {
				return err
			}
			return f.svc.Restore(ctx, tx, RestoreRequest{
				Allocations: allocs,
				JournalTxID: res.TransactionID,
				Reference:   Reference{Type: "test_restore", ID: uuid.NewString()},
				Reason:      "payout cancelled",
			})
		}))

	gotPromo, err := f.svc.Lot(f.ctx, testDB, promo.ID)
	require.NoError(t, err)
	gotEarning, err := f.svc.Lot(f.ctx, testDB, earning.ID)
	require.NoError(t, err)
	require.Equal(t, "100", gotPromo.Remaining.String())
	require.Equal(t, "100", gotEarning.Remaining.String(),
		"a cancelled payout must return the exact units it took, so a promotional grant cannot be laundered into an earning")
	require.NoError(t, f.svc.VerifyProvenance(f.ctx, testDB, f.account))
}

func TestIntegration_ThereIsOnlyOneCreditAsset(t *testing.T) {
	requireEnv(t)
	creditAsset(t)
	_, err := assets.NewRepository().Create(context.Background(), testDB, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindCredit,
		ValueDomain: valuedomain.InternalCredit,
		Symbol:      "CREDIT2", Name: "Second Credit", Decimals: 6,
		RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
	})
	require.Error(t, err, "a second Credit asset would make every Credit balance ambiguous")
}

// TestIntegration_ConsumptionOrderSQLMatchesGo keeps the ORDER BY and the Go
// ranking from drifting.
func TestIntegration_ConsumptionOrderSQLMatchesGo(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	for _, o := range valuedomain.AllOrigins() {
		var rank int
		require.NoError(t, testDB.QueryRow(ctx,
			`SELECT `+ConsumptionOrderSQL()+` FROM (SELECT $1::text AS origin) l`, string(o)).Scan(&rank))
		require.Equal(t, ConsumptionRank(o), rank, "origin %s ranks differently in SQL and Go", o)
	}
	var unknown int
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT `+ConsumptionOrderSQL()+` FROM (SELECT 'NOT_AN_ORIGIN'::text AS origin) l`).Scan(&unknown))
	require.Equal(t, ConsumptionRank("NOT_AN_ORIGIN"), unknown,
		"an unknown origin must sort last on both sides, so a newly added one is preserved rather than spent first")
}
