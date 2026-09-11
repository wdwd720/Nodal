//go:build integration

package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// TestIntegration_AUserCanCancelTheirOwnPayoutAndNobodyElses covers the
// half of the cancel path that `internal/payout` cannot: WHO may cancel.
//
// `payout.Cancel` was well tested and had no caller outside tests, so a user
// could request a payout, watch their Credits leave their spendable balance
// into PAYOUT_RESERVED, and have no way to get them back. Only an operator
// could, and only by running their own tool. Reserving somebody's money with no
// path to release it is not a conservative control; it is a trap.
//
// The endpoint that fixes it introduces a new question the service never had to
// answer, and this is that question: a payout is cancellable by its OWNER.
func TestIntegration_AUserCanCancelTheirOwnPayoutAndNobodyElses(t *testing.T) {
	appURL := testAppDSN(t)
	ctx := context.Background()

	pool, err := db.Open(ctx, db.Config{URL: appURL, AppName: "payout-cancel-itest", MaxConns: 6})
	require.NoError(t, err)
	defer pool.Close()

	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	led := ledger.NewService(clk, "payout-cancel-itest")
	led.SetCapabilityResolver(commerceCaps{
		valuedomain.CapPayoutReserve: true, valuedomain.CapPayoutSettle: true,
	})
	credits := credit.NewService(led, clk)
	svc := payout.NewService(led, credits, payout.NewEngine(credits), payout.NewRegistry(true), clk,
		killswitch.NewChecker(killswitch.Policy{}), accounts.NewRepository())

	owner := newPayoutAccount(t, pool)
	stranger := newPayoutAccount(t, pool)
	creditAsset := commerceCreditAsset(t, pool)

	adapter := payoutsAdapter{
		deps: NativeEconomyDeps{Payouts: svc, Clock: clk},
		db:   pool, clk: clk,
	}

	// Fund the owner and request a payout, so there is a real reservation to
	// return rather than an empty request to transition.
	issueCredits(t, pool, credits, clk, owner, "1000")
	before := creditBalance(t, pool, owner, creditAsset, ledger.CodeCreditBalance)

	var request payout.Request
	require.NoError(t, pool.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			r, _, cerr := svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: owner, Quantity: money.QuantityFromInt64(400),
				DisclosureAccepted: true,
				IdempotencyKey:     "cancel-itest-" + id.New[id.Any]().String(),
				EffectiveAt:        clk.Now(),
			}, payoutInputAllowing(clk))
			request = r
			return cerr
		}))
	require.Equal(t, payout.StateVerified, request.State, "the fixture needs a reserved request to cancel")
	require.Equal(t, "400", request.ReservedQuantity.String())
	require.Equal(t, "600", creditBalance(t, pool, owner, creditAsset, ledger.CodeCreditBalance).String(),
		"the reservation must have left the spendable balance")

	// A stranger cannot cancel it, and cannot learn that it exists.
	_, err = adapter.Cancel(ctx, stranger, request.ID, "not mine")
	require.Error(t, err)
	require.Equal(t, errs.CodeNotFound, errs.CodeOf(err),
		"a distinguishable refusal for somebody else's payout is a membership oracle")

	still, err := svc.Get(ctx, pool, request.ID)
	require.NoError(t, err)
	require.Equal(t, payout.StateVerified, still.State, "a refused cancel must change nothing")
	require.Equal(t, "400", still.ReservedQuantity.String())

	// The owner can, and the Credits come back.
	cancelled, err := adapter.Cancel(ctx, owner, request.ID, "changed my mind")
	require.NoError(t, err)
	require.Equal(t, payout.StateRejected, cancelled.State)
	require.Equal(t, "0", cancelled.ReservedQuantity.String())
	require.Equal(t, before.String(), creditBalance(t, pool, owner, creditAsset, ledger.CodeCreditBalance).String(),
		"cancelling returns exactly what was reserved, and nothing else")
	require.NoError(t, svc.VerifyReservations(ctx, pool, creditAsset))
}

// payoutInputAllowing is an eligibility input under which a creator earning is
// payable, so the fixture can produce a RESERVED request to cancel. It says
// nothing about what a real deployment permits.
func payoutInputAllowing(clk clock.Clock) payout.EligibilityInput {
	const cap valuedomain.CapabilityKey = "PAYOUT_CREATOR_EARNINGS"
	p := valuedomain.DefaultPolicy()
	p.Version = "cancel-itest-v1"
	p.Rules[valuedomain.OriginCreatorEarning] = valuedomain.OriginRule{
		PayoutAllowed: true, RequiredCapability: cap,
		RequiredVerification: valuedomain.VerificationPayoutKYC,
	}
	return payout.EligibilityInput{
		Policy:     p,
		Verified:   valuedomain.VerificationPayoutKYC,
		ActiveCaps: map[valuedomain.CapabilityKey]bool{cap: true},
		Now:        clk.Now().Add(48 * time.Hour),
		// No destination is required to reserve; the destination question is
		// about where value GOES, and this request never gets that far.
		DestinationVerified: true, ProviderSupports: true,
	}
}

func newPayoutAccount(t *testing.T, d *db.DB) accounts.AccountID {
	t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(t.Context(), d, "payout-cancel-itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	a, err := repo.CreateAccount(t.Context(), d, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return a.ID
}

func issueCredits(t *testing.T, d *db.DB, credits *credit.Service, clk clock.Clock, account accounts.AccountID, amount string) {
	t.Helper()
	q, err := money.ParseQuantity(amount)
	require.NoError(t, err)
	require.NoError(t, d.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, ierr := credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: account, Quantity: q,
				Origin: valuedomain.OriginCreatorEarning, Finality: valuedomain.FinalitySettled,
				Reference:      credit.Reference{Type: "test_issue", ID: id.New[id.Any]().String()},
				IdempotencyKey: "cancel-fund-" + id.New[id.Any]().String(),
				Reason:         "payout cancel test", EffectiveAt: clk.Now(),
			})
			return ierr
		}))
}

func creditBalance(t *testing.T, d *db.DB, account accounts.AccountID, asset assets.AssetID, code ledger.Code) money.Quantity {
	t.Helper()
	var raw string
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT coalesce((SELECT b.balance FROM ledger_accounts la
		    JOIN ledger_balances b ON b.ledger_account_id = la.id
		   WHERE la.owner_type='CUSTOMER' AND la.owner_id=$1 AND la.code=$2 AND la.asset_id=$3), 0)::text`,
		account, string(code), asset).Scan(&raw))
	q, err := money.ParseQuantity(raw)
	require.NoError(t, err)
	return q
}
