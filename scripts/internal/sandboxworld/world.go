// Package sandboxworld wires the advanced services over one database for the
// commands that drive them outside the HTTP API: the end-to-end demo
// (scripts/advanceddemo) and the performance report (scripts/advperf).
//
// The construction is the one cmd/api performs -- the same constructors and
// the same real dependencies, nothing doubled -- with two differences that
// make a run reproducible: the clock is a clock.Fake the caller advances, and
// the environment is LOCAL, where the advanced capabilities run in their
// simulated modes without a gate row (gates.DecideMode) and LIVE is refused
// as everywhere.
//
// Every value it creates is value domain SIMULATED. It must never be pointed
// at anything but a local database: the commands that use it provision their
// own through internal/testkit/localdb, which refuses any other host.
package sandboxworld

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/notification"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxfund"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/transfers"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Options configure Open.
type Options struct {
	// AppDSN connects as the application role to a migrated database.
	AppDSN string
	// Start is the instant the world's clock starts at.
	Start time.Time
	// MaxConns bounds the pool; concurrency measurements need headroom.
	MaxConns int32
	// Component names the writer in audit rows and journal metadata.
	Component string
}

// World is the wired set of services.
type World struct {
	DB     *db.DB
	Clock  *clock.Fake
	Env    config.Environment
	Ledger *ledger.Service
	Outbox *event.Outbox
	Audit  audit.Writer

	Intents    *econ.IntentStore
	Effects    *econ.EffectStore
	Registry   *econ.Registry
	Authorizer *econ.Authorizer
	Gates      *gates.Checker
	Kills      *killswitch.Checker
	Router     *legalrouter.Router
	Risk       *risk.Store
	Prices     *valuation.PriceStore
	Capital    *capital.Service

	Transfers *transfers.Service
	Faucet    *sandboxfund.Faucet

	// USD is the sandbox dollar every scenario settles in.
	USD assets.Asset
	// System is the actor of work the world does on its own behalf.
	System econ.Actor

	component string
}

// Open wires the world and provisions the sandbox dollar.
func Open(ctx context.Context, o Options) (*World, error) {
	if o.AppDSN == "" {
		return nil, errors.New("sandboxworld: an application DSN is required")
	}
	if o.Component == "" {
		o.Component = "sandboxworld"
	}
	if o.MaxConns <= 0 {
		o.MaxConns = 16
	}
	d, err := db.Open(ctx, db.Config{URL: o.AppDSN, AppName: o.Component, MaxConns: o.MaxConns})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: open database: %w", err)
	}
	w, err := wire(d, o)
	if err != nil {
		d.Close()
		return nil, err
	}
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		w.USD, err = econ.EnsureSandboxDollar(ctx, tx)
		return err
	}); err != nil {
		d.Close()
		return nil, fmt.Errorf("sandboxworld: provision the sandbox dollar: %w", err)
	}
	return w, nil
}

func wire(d *db.DB, o Options) (*World, error) {
	clk := clock.NewFake(o.Start)
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion(o.Component)
	env := config.EnvLocal
	// No capability is enabled by configuration: in LOCAL the advanced
	// capabilities need no gate row for their simulated modes, and nothing
	// else this world drives is gated by configuration.
	checker, err := gates.NewChecker(string(env), func(gates.Capability) bool { return false }, clk)
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: gate checker: %w", err)
	}
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: legal router: %w", err)
	}
	led := ledger.NewService(clk, o.Component)
	w := &World{
		DB: d, Clock: clk, Env: env, Ledger: led, Outbox: out, Audit: aw,
		Intents: econ.NewIntentStore(clk, out, aw), Effects: econ.NewEffectStore(clk, out, aw),
		Registry: econ.NewRegistry(), Authorizer: econ.NewAuthorizer(checker, kills),
		Gates: checker, Kills: kills, Router: router, Risk: risk.NewStore(),
		Prices:  valuation.NewPriceStore(clk),
		Capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: out, Clock: clk}),
		System:  econ.SystemActor(o.Component), component: o.Component,
	}
	screen, err := transfers.NewListScreen("sandboxworld-screen-v1", nil)
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: compliance screen: %w", err)
	}
	w.Transfers, err = transfers.NewService(transfers.Deps{
		Clock: clk, Ledger: led, Capital: w.Capital, Intents: w.Intents, Effects: w.Effects,
		Authorizer: w.Authorizer, KillSwitches: kills, Router: router, Risk: w.Risk,
		RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: transfers.DefaultPolicy(),
		Valuer: transfers.RegistryValuer{}, Screen: screen,
		Notifier: transfers.NewNotificationNotifier(notification.NewRepository()), Audit: aw,
	})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: transfers: %w", err)
	}
	w.Faucet, err = sandboxfund.New(sandboxfund.Deps{
		Clock: clk, Ledger: led, Effects: w.Effects, Environment: env, Policy: sandboxfund.DefaultPolicy(),
	})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: faucet: %w", err)
	}
	return w, nil
}

// Close releases the pool.
func (w *World) Close() { w.DB.Close() }

// InTx runs fn in one READ COMMITTED transaction, the isolation every
// advanced service is written for.
func (w *World) InTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return w.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

// Customer is a user, its customer account and the principal it acts as.
type Customer struct {
	Label   string
	User    accounts.UserID
	Account accounts.AccountID
	P       security.Principal
}

// Customer creates a user with one ACTIVE customer account and a VERIFIED,
// sanctions-clear compliance profile, plus a principal that authenticated
// with MFA at the world's current time. The label is recorded in the user's
// subject so demo data says what it is.
func (w *World) Customer(ctx context.Context, label string) (Customer, error) {
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(ctx, w.DB, w.component, "synthetic-"+label+"-"+uuid.NewString(), nil)
	if err != nil {
		return Customer{}, fmt.Errorf("sandboxworld: create user: %w", err)
	}
	a, err := repo.CreateAccount(ctx, w.DB, u.ID, accounts.KindCustomer)
	if err != nil {
		return Customer{}, fmt.Errorf("sandboxworld: create account: %w", err)
	}
	verifiedAt := w.Clock.Now().Add(-24 * time.Hour)
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := compliance.NewRepository(w.Audit).Upsert(ctx, tx, compliance.Profile{
			UserID: u.ID, IdentityState: compliance.IdentityVerified, SanctionsState: compliance.SanctionsClear,
			VerifiedAt: &verifiedAt,
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: w.component, Reason: "synthetic demo customer"})
		return err
	}); err != nil {
		return Customer{}, fmt.Errorf("sandboxworld: compliance profile: %w", err)
	}
	return Customer{Label: label, User: u.ID, Account: a.ID, P: security.Principal{
		SubjectID: u.ID.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{a.ID.String()}, AuthTime: w.Clock.Now(), AMR: []string{"pwd", "mfa"},
	}}, nil
}

// Operator is a human operator principal with the given role, authenticated
// with MFA at the world's current time.
func (w *World) Operator(role security.Role) security.Principal {
	return security.Principal{
		SubjectID: "synthetic-operator-" + string(role), ActorType: security.ActorOperator,
		Roles: []security.Role{role}, AuthTime: w.Clock.Now(), AMR: []string{"pwd", "mfa"},
	}
}

// SimAsset registers a SIMULATED asset on the sandbox chain. A USD-pegged one
// is valued at par; any other needs a recorded mark.
func (w *World) SimAsset(ctx context.Context, symbol, name string, decimals uint8, rc assets.RiskClass, usdPegged bool) (assets.Asset, error) {
	a := assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: symbol, Name: name, Decimals: decimals, RiskClass: rc, Status: assets.StatusActive,
	}
	if usdPegged {
		a.IsStablecoin, a.PegCurrency = true, valuation.USDPeg
	}
	return assets.NewRepository().Create(ctx, w.DB, a)
}

// Fund grants a customer simulated value from the faucet, as the customer.
func (w *World) Fund(ctx context.Context, c Customer, asset assets.AssetID, qty money.Quantity, key string) (sandboxfund.Grant, error) {
	var g sandboxfund.Grant
	err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = w.Faucet.Fund(ctx, tx, c.P, c.Account, asset, qty, key)
		return err
	})
	return g, err
}

// Balance reads one ledger balance.
func (w *World) Balance(ctx context.Context, ref ledger.AccountRef) (money.Quantity, error) {
	return w.Ledger.Balance(ctx, w.DB, ref)
}

// Wallet is a customer's WALLET balance of one asset.
func (w *World) Wallet(ctx context.Context, c Customer, asset assets.AssetID) (money.Quantity, error) {
	return w.Balance(ctx, ledger.CustomerAccount(c.Account, ledger.CodeWallet, asset))
}

// Units converts whole units of an asset with the given decimals to base
// units, exactly.
func Units(whole int64, decimals uint8) money.Quantity {
	q := money.QuantityFromInt64(whole)
	for i := uint8(0); i < decimals; i++ {
		q = q.Mul(money.QuantityFromInt64(10))
	}
	return q
}
