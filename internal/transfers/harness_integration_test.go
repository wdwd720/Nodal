//go:build integration

package transfers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/notification"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "transfers integration: migrate up:", err)
		return 1
	}
	var err error
	// A generous lock timeout: the concurrency tests deliberately queue many
	// transfers behind one sender's lock.
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "transfers-itest", MaxConns: 40,
		LockTimeout: 60 * time.Second, StatementTimeout: 120 * time.Second}); err != nil {
		fmt.Fprintln(os.Stderr, "transfers integration: open:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
}

// outboxEmitter is the capital emitter production wiring uses (see
// cmd/execution-worker/adapters.go), so reservation events land in the
// outbox exactly as they would in a deployment.
type outboxEmitter struct {
	outbox *event.Outbox
	clk    clock.Clock
}

func (e outboxEmitter) Emit(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	spec, ok := event.Lookup(event.Topic(topic))
	if !ok {
		return nil
	}
	ev, ok := payload.(capital.ReservationEvent)
	if !ok || ev.ReservationID == "" {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return e.outbox.Enqueue(ctx, tx, topic, event.Envelope{
		ID: event.NewEventID().String(), Type: topic, SchemaVersion: spec.SchemaVersion, Source: "capital",
		AggregateType: event.AggregateReservation, AggregateID: ev.ReservationID, OccurredAt: e.clk.Now().UTC(), Payload: body,
	})
}

type fixture struct {
	t      *testing.T
	ctx    context.Context
	clk    *clock.Fake
	ledger *ledger.Service
	cap    *capital.Service
	deps   Deps
	svc    *Service
	asset  assets.Asset
	prices *valuation.PriceStore
}

// newFixture wires the real service against the real database. Every test
// gets its own par-valued simulated asset, so Reconcile scoped to it sees only
// that test's transfers.
func newFixture(t *testing.T) *fixture {
	requireEnv(t)
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("itest")
	led := ledger.NewService(clk, "itest")
	capSvc := capital.NewService(clk, outboxEmitter{outbox: out, clk: clk})
	checker, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	screen, err := NewListScreen("itest-list-v1", nil)
	require.NoError(t, err)
	f := &fixture{t: t, ctx: context.Background(), clk: clk, ledger: led, cap: capSvc, prices: valuation.NewPriceStore(clk)}
	f.deps = Deps{
		Clock: clk, Ledger: led, Capital: capSvc,
		Intents: econ.NewIntentStore(clk, out, aw), Effects: econ.NewEffectStore(clk, out, aw),
		Authorizer: econ.NewAuthorizer(checker, kills), KillSwitches: kills, Router: router,
		Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: DefaultPolicy(),
		Valuer: RegistryValuer{}, Screen: screen, Notifier: NewNotificationNotifier(notification.NewRepository()), Audit: aw,
	}
	f.rebuild(nil)
	f.asset = f.parAsset("T" + strings.ToUpper(uuid.NewString()[:8]))
	return f
}

// rebuild re-wires the service after mutating its dependencies.
func (f *fixture) rebuild(mutate func(*Deps)) {
	f.t.Helper()
	if mutate != nil {
		mutate(&f.deps)
	}
	svc, err := NewService(f.deps)
	require.NoError(f.t, err)
	f.svc = svc
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

func (f *fixture) parAsset(symbol string) assets.Asset {
	f.t.Helper()
	a, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: symbol, Name: symbol + " (simulated dollar)", Decimals: 6, IsStablecoin: true, PegCurrency: valuation.USDPeg,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	return a
}

func (f *fixture) simAsset(symbol string, decimals uint8) assets.Asset {
	f.t.Helper()
	a, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: symbol, Name: symbol + " (simulated)", Decimals: decimals,
		RiskClass: assets.RiskStandard, Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	return a
}

type customer struct {
	user    accounts.UserID
	account accounts.AccountID
	p       security.Principal
}

// customer creates a user with one ACTIVE customer account, a VERIFIED
// compliance profile, and a principal that authenticated with MFA just now.
func (f *fixture) customer() customer {
	f.t.Helper()
	return f.customerWith(true)
}

func (f *fixture) customerWith(verified bool) customer {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "transfers-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	if verified {
		at := f.clk.Now().Add(-24 * time.Hour)
		require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := compliance.NewRepository(audit.NewWriterWithBuildVersion("itest")).Upsert(ctx, tx, compliance.Profile{
				UserID: u.ID, IdentityState: compliance.IdentityVerified, SanctionsState: compliance.SanctionsClear,
				VerifiedAt: &at,
			}, compliance.Change{ActorType: security.ActorSystem, ActorID: "itest-idv", Reason: "fixture"})
			return err
		}))
	}
	return customer{user: u.ID, account: a.ID, p: security.Principal{
		SubjectID: u.ID.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{a.ID.String()}, AuthTime: f.clk.Now(), AMR: []string{"mfa"},
	}}
}

func operator() security.Principal {
	return security.Principal{SubjectID: uuid.NewString(), ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleFinance}, AuthTime: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), AMR: []string{"mfa"}}
}

// fund credits qty base units of the fixture asset (SANDBOX_FUNDING).
func (f *fixture) fund(acct accounts.AccountID, qty int64) {
	f.fundAsset(acct, f.asset.ID, qty)
}

func (f *fixture) fundAsset(acct accounts.AccountID, asset assets.AssetID, qty int64) {
	f.t.Helper()
	q := money.QuantityFromInt64(qty)
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "itest-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: q},
			},
		})
		return err
	}))
}

func (f *fixture) req(from, to customer, qty int64) SendRequest {
	return SendRequest{
		SenderAccountID: from.account, RecipientAccountID: to.account, AssetID: f.asset.ID,
		Quantity: money.QuantityFromInt64(qty), Mode: gates.ModeSandbox,
		IdempotencyKey: "itest-" + uuid.NewString(), CorrelationID: "corr-" + uuid.NewString(),
	}
}

// send runs Send in its own transaction. A recorded refusal is committed
// (that is what makes it a record); any other error rolls back.
func (f *fixture) send(p security.Principal, req SendRequest) (Result, error) {
	var res Result
	var sendErr error
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		res, sendErr = f.svc.Send(ctx, tx, p, req)
		if sendErr != nil && res.Transfer.Status != StatusRejected {
			return sendErr
		}
		return nil
	})
	if err != nil {
		if sendErr != nil && err == sendErr {
			return Result{}, sendErr
		}
		return Result{}, err
	}
	return res, sendErr
}

func (f *fixture) reverse(p security.Principal, req ReverseRequest) (Transfer, error) {
	var out Transfer
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.Reverse(ctx, tx, p, req)
		return err
	})
	return out, err
}

func (f *fixture) balance(acct accounts.AccountID, code ledger.Code) money.Quantity {
	f.t.Helper()
	return f.balanceOf(acct, code, f.asset.ID)
}

func (f *fixture) balanceOf(acct accounts.AccountID, code ledger.Code, asset assets.AssetID) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ledger.CustomerAccount(acct, code, asset))
	require.NoError(f.t, err)
	return b
}

// walletSum is Σ WALLET over the given accounts.
func (f *fixture) walletSum(accts ...accounts.AccountID) money.Quantity {
	f.t.Helper()
	sum := money.Quantity{}
	for _, a := range accts {
		sum = sum.Add(f.balance(a, ledger.CodeWallet))
	}
	return sum
}

// requirePartyIdentity proves WALLET + TRANSFER_OUT − TRANSFER_IN − CAPITAL = 0
// for a customer whose only activity in the asset is funding, transfers and
// reversals.
func (f *fixture) requirePartyIdentity(acct accounts.AccountID) {
	f.t.Helper()
	w := f.balance(acct, ledger.CodeWallet)
	out := f.balance(acct, ledger.CodeTransferOut)
	in := f.balance(acct, ledger.CodeTransferIn)
	capital := f.balance(acct, ledger.CodeCapital)
	lhs := w.Add(out).Sub(in).Sub(capital)
	require.True(f.t, lhs.IsZero(), "account %s: WALLET %s + TRANSFER_OUT %s - TRANSFER_IN %s - CAPITAL %s = %s, not 0",
		acct, w, out, in, capital, lhs)
	require.False(f.t, w.IsNegative(), "WALLET of %s is negative", acct)
}

func (f *fixture) requireReconciled() ReconcileReport {
	f.t.Helper()
	rep, err := f.svc.Reconcile(f.ctx, testDB, f.asset.ID)
	require.NoError(f.t, err)
	require.True(f.t, rep.OK(), "reconcile: %+v", rep.Discrepancies)
	return rep
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

// journalsOfTransfer counts the P2P_TRANSFER journals referencing a transfer.
func (f *fixture) journalsOfTransfer(tid TransferID) int {
	return f.count(`SELECT count(*) FROM journal_transactions WHERE kind = 'P2P_TRANSFER' AND reference_id = $1`, tid.String())
}

// journalFingerprint hashes a journal's header and entries as stored, so a
// test can prove they never change.
func (f *fixture) journalFingerprint(jid ledger.TransactionID) [32]byte {
	f.t.Helper()
	var header string
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT row_to_json(j)::text FROM journal_transactions j WHERE id = $1`, jid).Scan(&header))
	var entries string
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT coalesce(json_agg(e ORDER BY e.seq)::text, '') FROM journal_entries e WHERE transaction_id = $1`, jid).Scan(&entries))
	return sha256.Sum256([]byte(header + "|" + entries))
}

// reserveFor is another subsystem spending the same wallet: a capital
// reservation that takes no transfer lock at all.
func reserveFor(c customer, asset assets.AssetID, qty int64) capital.ReserveRequest {
	return capital.ReserveRequest{
		AccountID: c.account.String(), AssetID: asset, Quantity: money.QuantityFromInt64(qty),
		ActorType: security.ActorSystem, ActorID: "itest-other-spender", IdempotencyKey: "itest-other-" + uuid.NewString(),
		TTL: time.Hour, Reason: "another subsystem's reservation",
	}
}

func requireCode(t *testing.T, err error, code errs.Code) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, code, errs.CodeOf(err), "%v", err)
}

func fieldOf(t *testing.T, err error, key string) any {
	t.Helper()
	e, ok := errs.As(err)
	require.True(t, ok, "%v is not an *errs.Error", err)
	return e.Fields[key]
}
