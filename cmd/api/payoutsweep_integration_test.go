//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/provider/payoutsandbox"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The payout submission and settlement passes (D-085), end to end.
//
// Everything up to the reservation already worked and was tested. What had no
// caller was the step after it: Submit, and then the lookup that turns the
// provider's answer into SETTLED. So a payout stopped at VERIFIED with the
// customer's Credits held in PAYOUT_RESERVED and nothing on its way anywhere.
//
// This drives the whole of it against the real sandbox provider on a fake
// clock: reserve, submit, PROVIDER_PENDING, and SETTLED once SettleAfter has
// passed.

// payoutSweepFixture is the smallest deployment that can pay somebody out.
type payoutSweepFixture struct {
	db       *db.DB
	clk      *clock.Fake
	svc      *payout.Service
	credits  *credit.Service
	provider *payoutsandbox.Provider

	account     accounts.AccountID
	creditAsset assets.AssetID
	destination payout.DestinationID
}

// sweepCaps lets the ledger commit the postings a reservation and a settlement
// need. On a real deployment these come from the gate checker; the gate
// ceremony is not what this test is about.
type sweepCaps map[valuedomain.CapabilityKey]bool

func (c sweepCaps) ActiveConversionCapabilities(context.Context, db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return c, nil
}

// sweepPolicy permits purchased value once verified and nothing else, which is
// the shape of the sandbox tier's own rehearsal policy.
func sweepPolicy() valuedomain.Policy {
	p := valuedomain.DefaultPolicy()
	p.Version = "sweep-itest-v1"
	p.Rules[valuedomain.OriginPurchased] = valuedomain.OriginRule{
		PayoutAllowed:        true,
		RequiredCapability:   valuedomain.CapPayoutReserve,
		RequiredVerification: valuedomain.VerificationPayoutKYC,
	}
	return p
}

func newPayoutSweepFixture(t *testing.T) *payoutSweepFixture {
	t.Helper()
	d := openNotificationsDB(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))

	led := ledger.NewService(clk, "payout-sweep-itest")
	led.SetCapabilityResolver(sweepCaps{
		valuedomain.CapPayoutReserve: true,
		valuedomain.CapPayoutSettle:  true,
	})
	credits := credit.NewService(led, clk)

	provider, err := payoutsandbox.New(config.EnvStaging, clk.Now)
	require.NoError(t, err)
	registry := payout.NewRegistry(true)
	require.NoError(t, registry.Register(provider))
	svc := payout.NewService(led, credits, payout.NewEngine(credits), registry, clk)

	repo := accounts.NewRepository()
	user, err := repo.CreateUser(ctx, d, "payout-sweep-itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	acct, err := repo.CreateAccount(ctx, d, user.ID, accounts.KindCustomer)
	require.NoError(t, err)

	f := &payoutSweepFixture{
		db: d, clk: clk, svc: svc, credits: credits, provider: provider,
		account: acct.ID, creditAsset: sweepCreditAsset(t, d),
	}
	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			dest, derr := svc.CreateDestination(ctx, tx, payout.Destination{
				AccountID: acct.ID, Kind: payout.DestinationBank,
				Provider: payoutsandbox.Name, ProviderReference: "dest-" + id.New[id.Any]().String(),
				DisplayLabel: "Test bank", Currency: "USD",
			})
			if derr != nil {
				return derr
			}
			if _, derr = svc.SetDestinationStatus(ctx, tx, dest.ID, payout.DestinationVerified); derr != nil {
				return derr
			}
			f.destination = dest.ID
			return nil
		}))
	return f
}

func sweepCreditAsset(t *testing.T, d *db.DB) assets.AssetID {
	t.Helper()
	ctx := context.Background()
	var existing assets.AssetID
	if err := d.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing); err == nil {
		return existing
	}
	created, err := assets.NewRepository().Create(ctx, d, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindCredit,
		ValueDomain: valuedomain.InternalCredit,
		Symbol:      "CREDIT", Name: "Nodal Credit", Decimals: 6,
		RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	return created.ID
}

// reserve issues settled purchased Credits and creates a payout against them,
// which is the state the surfaces leave a request in.
func (f *payoutSweepFixture) reserve(t *testing.T, amount int64) payout.Request {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: f.account, Quantity: money.QuantityFromInt64(amount),
				Origin: valuedomain.OriginPurchased, Finality: valuedomain.FinalitySettled,
				Reference:      credit.Reference{Type: "test_issue", ID: id.New[id.Any]().String()},
				IdempotencyKey: "issue-" + id.New[id.Any]().String(),
				Reason:         "sweep fixture", EffectiveAt: f.clk.Now(),
			})
			return err
		}))

	var req payout.Request
	dest := f.destination
	require.NoError(t, f.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			r, _, cerr := f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest,
				Quantity:           money.QuantityFromInt64(amount),
				DisclosureAccepted: true,
				IdempotencyKey:     "payout-" + id.New[id.Any]().String(),
				EffectiveAt:        f.clk.Now(),
			}, payout.EligibilityInput{
				AccountID: f.account, Requested: money.QuantityFromInt64(amount),
				Policy: sweepPolicy(), Verified: valuedomain.VerificationPayoutKYC,
				ActiveCaps:          map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
				Now:                 f.clk.Now(),
				DestinationVerified: true, ProviderSupports: true,
			})
			req = r
			return cerr
		}))
	require.Equal(t, payout.StateVerified, req.State, "the fixture needs a reserved request to submit")
	return req
}

func (f *payoutSweepFixture) stateOf(t *testing.T, id payout.RequestID) payout.State {
	t.Helper()
	var s string
	require.NoError(t, f.db.QueryRow(context.Background(),
		`SELECT state FROM payout_requests WHERE id = $1`, id).Scan(&s))
	return payout.State(s)
}

func (f *payoutSweepFixture) sweep(t *testing.T) {
	t.Helper()
	payoutSweepOnce(context.Background(), f.db, f.svc, payoutsandbox.Name, f.clk,
		quietLogger())
}

// TestIntegration_AReservedPayoutReachesTheProviderAndSettles.
func TestIntegration_AReservedPayoutReachesTheProviderAndSettles(t *testing.T) {
	f := newPayoutSweepFixture(t)
	req := f.reserve(t, 400)

	// The submission pass reads requests created BEFORE now, so a fixture that
	// created one in this same instant would be skipped -- which is the point
	// of that bound: a sweep must not race the transaction still creating one.
	f.clk.Advance(time.Second)

	f.sweep(t)
	state := f.stateOf(t, req.ID)
	assert.Equal(t, payout.StateProviderPending, state,
		"the sandbox provider accepts first and settles later, so the request passes through PROVIDER_PENDING")

	// Inside the grace period, and before the provider would settle: asking
	// again changes nothing.
	f.clk.Advance(payoutSettleGrace + time.Second)
	f.sweep(t)
	assert.Equal(t, payout.StateProviderPending, f.stateOf(t, req.ID),
		"a provider that has not settled yet is in flight, never failed")

	// Past the provider's own settlement delay.
	f.clk.Advance(payoutsandbox.SettleAfter)
	f.sweep(t)
	assert.Equal(t, payout.StateSettled, f.stateOf(t, req.ID))

	// The reservation is gone and the value left.
	var reserved, balance string
	require.NoError(t, f.db.QueryRow(context.Background(),
		`SELECT coalesce((SELECT b.balance FROM ledger_accounts la
		    JOIN ledger_balances b ON b.ledger_account_id = la.id
		   WHERE la.owner_type='CUSTOMER' AND la.owner_id=$1 AND la.code='PAYOUT_RESERVED' AND la.asset_id=$2), 0)::text,
		        coalesce((SELECT b.balance FROM ledger_accounts la
		    JOIN ledger_balances b ON b.ledger_account_id = la.id
		   WHERE la.owner_type='CUSTOMER' AND la.owner_id=$1 AND la.code='CREDIT_BALANCE' AND la.asset_id=$2), 0)::text`,
		f.account, f.creditAsset).Scan(&reserved, &balance))
	assert.Equal(t, "0", reserved, "a settled payout holds nothing in reserve")
	assert.Equal(t, "0", balance, "and the value it reserved has left the spendable balance")
}

// TestIntegration_ASweepThatRunsTwiceSubmitsOnce.
//
// The crash this is about: the key is written and committed, the provider is
// called, and the process dies before the answer is recorded. Submit's re-entry
// branch and the provider's own idempotency both have to hold, and a pass that
// runs every fifteen seconds exercises that on every tick.
func TestIntegration_ASweepThatRunsTwiceSubmitsOnce(t *testing.T) {
	f := newPayoutSweepFixture(t)
	req := f.reserve(t, 400)
	f.clk.Advance(time.Second)

	f.sweep(t)
	f.sweep(t)
	f.sweep(t)

	// One submission event, whatever the sweep did. The provider records one
	// acceptance per key and payout_provider_events records every call, so a
	// second SUBMIT here would be a second disbursement at a real provider.
	var submits int
	require.NoError(t, f.db.QueryRow(context.Background(),
		`SELECT count(*) FROM payout_provider_events
		  WHERE request_id = $1 AND direction = 'REQUEST'`, req.ID).Scan(&submits))
	assert.LessOrEqual(t, submits, 1, "the provider is called once per key, however often the pass runs")

	var key string
	require.NoError(t, f.db.QueryRow(context.Background(),
		`SELECT provider_idempotency_key FROM payout_requests WHERE id = $1`, req.ID).Scan(&key))
	assert.Equal(t, "nodal-payout-"+req.ID.String(), key,
		"the key is derived from the request, so a retry cannot invent a second one")

	var transitions int
	require.NoError(t, f.db.QueryRow(context.Background(),
		`SELECT count(*) FROM payout_request_transitions WHERE request_id = $1 AND to_state = 'SUBMITTED'`,
		req.ID).Scan(&transitions))
	assert.Equal(t, 1, transitions, "one submission, one transition row")
}

// TestIntegration_ASweepWithNoProviderDoesNothing.
//
// The honest state of a deployment with no conversion contract. The pass says
// so once, at startup, and never touches a request -- because a sweep that
// guessed which provider a payout belonged to would be choosing for the
// deployment.
func TestIntegration_ASweepWithNoProviderDoesNothing(t *testing.T) {
	f := newPayoutSweepFixture(t)
	req := f.reserve(t, 400)
	f.clk.Advance(time.Second)

	empty := payout.NewService(
		ledger.NewService(f.clk, "empty"), f.credits,
		payout.NewEngine(f.credits), payout.NewRegistry(false), f.clk,
	)
	assert.Empty(t, empty.ProviderNames())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runPayoutSweeps(ctx, f.db, empty, f.clk,
		quietLogger())
	assert.Equal(t, payout.StateVerified, f.stateOf(t, req.ID))
}
