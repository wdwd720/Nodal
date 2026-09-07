//go:build integration

package withdrawal_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/withdrawal"
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
		fmt.Fprintln(os.Stderr, "withdrawal integration: migrate up:", err)
		return 1
	}
	var err error
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "withdrawal-itest", MaxConns: 8})
	if err != nil {
		fmt.Fprintln(os.Stderr, "withdrawal integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test (go run ./scripts/testdb -name funding)")
	}
}

type dbFixture struct {
	ctx     context.Context
	clk     *clock.Fake
	repo    *withdrawal.Repository
	user    accounts.UserID
	account accounts.AccountID
	asset   assets.Asset
}

func newDBFixture(t *testing.T) *dbFixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	repo, err := withdrawal.NewRepository(clk, audit.NewWriterWithBuildVersion("withdrawal-itest"))
	require.NoError(t, err)
	ar := accounts.NewRepository()
	u, err := ar.CreateUser(ctx, testDB, "https://idp.test", "sub-"+uuid.NewString(), nil)
	require.NoError(t, err)
	acc, err := ar.CreateAccount(ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	a, err := assets.NewRepository().Create(ctx, testDB, assets.Asset{
		Chain: "solana-devnet", MintAddress: "mint-" + uuid.NewString(), Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "USDC", Name: "USD Coin",
		Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	return &dbFixture{ctx: ctx, clk: clk, repo: repo, user: u.ID, account: acc.ID, asset: a}
}

func (f *dbFixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{}, fn)
}

func TestIntegration_Repository(t *testing.T) {
	f := newDBFixture(t)
	ev := withdrawal.TransitionEvidence{ActorType: security.ActorUser, ActorID: f.user.String(), Reason: "test"}
	var w withdrawal.Withdrawal
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		created, isNew, err := f.repo.Create(ctx, tx, withdrawal.Withdrawal{
			AccountID: f.account, AssetID: f.asset.ID, Quantity: money.QuantityFromInt64(5_000_000), DestinationAddress: wrappedSOL,
			DestinationValidated: true, RequestedByUserID: f.user, CapabilityCheckRef: "gate:WITHDRAWALS", IdempotencyKey: "w-" + uuid.NewString(),
		}, ev)
		require.True(t, isNew)
		w = created
		return err
	}))
	require.Equal(t, withdrawal.StatusRequested, w.Status)
	got, err := f.repo.Get(f.ctx, testDB, w.ID)
	require.NoError(t, err)
	require.Equal(t, w.ID, got.ID)
	require.Equal(t, "5000000", got.Quantity.String())
	recent, err := f.repo.ListRecent(f.ctx, testDB, f.account, f.asset.ID, f.clk.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, recent, 1)

	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.Transition(ctx, tx, w.ID, withdrawal.StatusStepUpVerified, ev)
		return err
	}))
	got, err = f.repo.Get(f.ctx, testDB, w.ID)
	require.NoError(t, err)
	require.Equal(t, withdrawal.StatusStepUpVerified, got.Status)
	require.NotNil(t, got.StepUpVerifiedAt)

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.Transition(ctx, tx, w.ID, withdrawal.StatusSettled, ev)
		return err
	})
	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// AGENT actors are refused by the evidence validator and by the table.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.Transition(ctx, tx, w.ID, withdrawal.StatusCancelled, withdrawal.TransitionEvidence{ActorType: security.ActorAgent, ActorID: "agent", Reason: "x"})
		return err
	})
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	_, err = testDB.Exec(f.ctx, `INSERT INTO withdrawal_transitions (id, withdrawal_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1, $2, 'STEP_UP_VERIFIED', 'CANCELLED', 'AGENT', 'agent', 'x')`, id.New[id.Any](), w.ID)
	require.True(t, db.IsCheckViolation(err), "database refuses AGENT transitions: %v", err)
	_, err = testDB.Exec(f.ctx, `UPDATE withdrawals SET status = 'CANCELLED' WHERE id = $1`, w.ID)
	require.Equal(t, "AU001", db.SQLState(err), "status change without a transition row is refused")

	var n int
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM audit_events WHERE resource_type = 'withdrawal' AND resource_id = $1`, w.ID.String()).Scan(&n))
	require.Equal(t, 2, n)
}

// TestIntegration_GateRefusedWithRealChecker: with the real gate checker
// and no gate row (the fresh-deployment default), a stepped-up customer's
// request is CAPABILITY_NOT_APPROVED and nothing is written.
func TestIntegration_GateRefusedWithRealChecker(t *testing.T) {
	f := newDBFixture(t)
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, f.clk)
	require.NoError(t, err)
	svc, err := withdrawal.NewService(withdrawal.Deps{
		DB: testDB, Clock: f.clk, Store: f.repo, Accounts: accounts.NewRepository(), Gates: checker, KillSwitches: killswitch.NewChecker(killswitch.Policy{}),
	})
	require.NoError(t, err)
	actor, err := withdrawal.HumanFrom(security.Principal{
		SubjectID: f.user.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{f.account.String()}, AuthTime: f.clk.Now(), AMR: []string{"mfa"},
	})
	require.NoError(t, err)
	_, err = svc.Request(f.ctx, actor, withdrawal.Request{
		AccountID: f.account, AssetID: f.asset.ID, Quantity: money.QuantityFromInt64(1), DestinationType: withdrawal.DestinationExternalAddress,
		DestinationAddress: wrappedSOL, IdempotencyKey: "w-" + uuid.NewString(),
	})
	require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))
	recent, err := f.repo.ListRecent(f.ctx, testDB, f.account, f.asset.ID, f.clk.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Empty(t, recent)
}
