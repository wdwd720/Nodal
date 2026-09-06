package funding_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit/audittest"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/funding/fundingtest"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// nopDB records InTx calls and never opens a transaction; it lets the
// pre-transaction refusals of Start be tested without a database.
type nopDB struct{ calls atomic.Int32 }

func (n *nopDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (n *nopDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (n *nopDB) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }
func (n *nopDB) InTx(context.Context, db.TxOptions, func(context.Context, pgx.Tx) error) error {
	n.calls.Add(1)
	return errs.New(errs.CodeInternal, "nopDB: transaction reached")
}

type nopLedger struct{}

func (nopLedger) Post(context.Context, pgx.Tx, ledger.Posting) (ledger.PostResult, error) {
	return ledger.PostResult{}, nil
}

func (nopLedger) Balance(context.Context, db.Querier, ledger.AccountRef) (money.Quantity, error) {
	return money.Quantity{}, nil
}

type nopHolds struct{}

func (nopHolds) PlaceHold(_ context.Context, _ pgx.Tx, h capital.WithdrawalHold) (capital.WithdrawalHold, error) {
	return h, nil
}

func (nopHolds) ReleaseHold(context.Context, pgx.Tx, capital.WithdrawalHoldID, string) (capital.WithdrawalHold, error) {
	return capital.WithdrawalHold{}, nil
}

type nopAccounts struct{}

func (nopAccounts) Get(context.Context, db.Querier, accounts.AccountID) (accounts.Account, error) {
	return accounts.Account{Status: accounts.StatusActive}, nil
}

func (nopAccounts) Transition(context.Context, pgx.Tx, accounts.AccountID, accounts.StatusChange, time.Time) (accounts.Account, error) {
	return accounts.Account{}, nil
}

type nopAssets struct{}

func (nopAssets) Get(context.Context, db.Querier, assets.AssetID) (assets.Asset, error) {
	return assets.Asset{Symbol: "USDC", Decimals: 6, Status: assets.StatusActive}, nil
}

func unitDeps(t *testing.T, env config.Environment, mode config.ProviderMode) (funding.Config, funding.Deps, *nopDB) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	repo, err := funding.NewRepository(clk, event.NewOutbox(clk), audittest.New())
	require.NoError(t, err)
	d := &nopDB{}
	cfg := funding.Config{
		Env: env, ProviderMode: mode, Reconcile: funding.ReconcilePolicy{ToleranceBPS: 50},
		Availability: funding.AvailabilityPolicy{Version: "v1", ReversibleFor: 72 * time.Hour},
		SessionTTL:   30 * time.Minute, SettlementTimeout: 2 * time.Hour,
	}
	deps := funding.Deps{
		DB: d, Clock: clk, Repo: repo, Provider: fundingtest.NewProvider(clk, "s"), Ledger: nopLedger{}, Holds: nopHolds{},
		Accounts: nopAccounts{}, Assets: nopAssets{}, Gates: &fundingtest.Gates{}, KillSwitches: &fundingtest.KillSwitches{},
	}
	return cfg, deps, d
}

func TestNewService_Validation(t *testing.T) {
	t.Parallel()
	cfg, deps, _ := unitDeps(t, config.EnvTest, config.ProviderModeFake)
	_, err := funding.NewService(cfg, deps)
	require.NoError(t, err)

	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		c := cfg
		c.Env = env
		_, err := funding.NewService(c, deps)
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "fake provider must be refused in %s", env)
	}
	c := cfg
	c.Reconcile.ToleranceBPS = 10_001
	_, err = funding.NewService(c, deps)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	c = cfg
	c.Availability.Version = ""
	_, err = funding.NewService(c, deps)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	c = cfg
	c.SessionTTL = 0
	_, err = funding.NewService(c, deps)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	for name, mutate := range map[string]func(*funding.Deps){
		"gates":    func(d *funding.Deps) { d.Gates = nil },
		"kill":     func(d *funding.Deps) { d.KillSwitches = nil },
		"ledger":   func(d *funding.Deps) { d.Ledger = nil },
		"provider": func(d *funding.Deps) { d.Provider = nil },
		"db":       func(d *funding.Deps) { d.DB = nil },
	} {
		d := deps
		mutate(&d)
		_, err := funding.NewService(cfg, d)
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "missing %s must be refused at wiring", name)
	}
}

func TestService_RequiresLiveFundingGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		env  config.Environment
		mode config.ProviderMode
		want bool
	}{
		{config.EnvLocal, config.ProviderModeFake, false},
		{config.EnvTest, config.ProviderModeSandbox, false},
		{config.EnvDev, config.ProviderModeSandbox, false},
		{config.EnvTest, config.ProviderModeLive, true},
		{config.EnvStaging, config.ProviderModeSandbox, true},
		{config.EnvProd, config.ProviderModeLive, true},
	}
	for _, c := range cases {
		cfg, deps, _ := unitDeps(t, c.env, c.mode)
		svc, err := funding.NewService(cfg, deps)
		require.NoError(t, err)
		require.Equal(t, c.want, svc.RequiresLiveFundingGate(), "%s/%s", c.env, c.mode)
	}
}

func principal(actor security.ActorType, roles []security.Role, account string) security.Principal {
	return security.Principal{SubjectID: accounts.NewUserID().String(), ActorType: actor, Roles: roles, AccountIDs: []string{account}, AuthTime: time.Now().UTC(), AMR: []string{"mfa"}}
}

func validRequest(account accounts.AccountID) funding.StartRequest {
	return funding.StartRequest{
		AccountID: account, AssetID: assets.NewAssetID(), DestinationNetwork: "solana", DestinationCurrency: "usdc",
		DestinationAddress: "So11111111111111111111111111111111111111112", FiatCurrency: "usd", IdempotencyKey: "k-1",
	}
}

// TestService_Start_PreTransactionRefusals: authority failures are decided
// before any transaction is opened.
func TestService_Start_PreTransactionRefusals(t *testing.T) {
	t.Parallel()
	cfg, deps, d := unitDeps(t, config.EnvTest, config.ProviderModeFake)
	svc, err := funding.NewService(cfg, deps)
	require.NoError(t, err)
	account := accounts.NewAccountID()
	ctx := context.Background()

	_, err = svc.Start(ctx, security.AgentPrincipal("agent-1", account.String()), validRequest(account))
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "agents never hold funding:create")

	_, err = svc.Start(ctx, principal(security.ActorOperator, []security.Role{security.RoleSupportReadOnly}, account.String()), validRequest(account))
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "support has no funding:create")

	_, err = svc.Start(ctx, security.Principal{}, validRequest(account))
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "invalid principal fails closed")

	other := accounts.NewAccountID()
	_, err = svc.Start(ctx, principal(security.ActorUser, []security.Role{security.RoleCustomer}, other.String()), validRequest(account))
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "cross-tenant")

	bad := validRequest(account)
	bad.FiatCurrency = "gbp"
	_, err = svc.Start(ctx, principal(security.ActorUser, []security.Role{security.RoleCustomer}, account.String()), bad)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	require.Equal(t, int32(0), d.calls.Load(), "no transaction may be opened before authority is established")

	_, err = svc.Start(ctx, principal(security.ActorUser, []security.Role{security.RoleCustomer}, account.String()), validRequest(account))
	require.Error(t, err)
	require.Equal(t, int32(1), d.calls.Load(), "an authorized request reaches the transaction")
}

func TestStartRequest_Validate(t *testing.T) {
	t.Parallel()
	account := accounts.NewAccountID()
	require.NoError(t, validRequest(account).Validate())
	r := validRequest(account)
	r.IdempotencyKey = ""
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(r.Validate()))
	r = validRequest(account)
	neg := int64(-5)
	r.FiatAmountMinor = &neg
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(r.Validate()))
	r = validRequest(account)
	r.DestinationAddress = ""
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(r.Validate()))
}

func TestAvailabilityPolicy_Validate(t *testing.T) {
	t.Parallel()
	require.NoError(t, funding.AvailabilityPolicy{Version: "v1"}.Validate())
	require.Error(t, funding.AvailabilityPolicy{}.Validate())
	require.Error(t, funding.AvailabilityPolicy{Version: "v1", ReversibleFor: -time.Second}.Validate())
}
