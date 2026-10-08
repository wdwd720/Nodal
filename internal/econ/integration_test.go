//go:build integration

package econ

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
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
		fmt.Fprintln(os.Stderr, "econ integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "econ-itest", MaxConns: 30}); err != nil {
		fmt.Fprintln(os.Stderr, "econ integration: open:", err)
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

type fixture struct {
	t       *testing.T
	ctx     context.Context
	clk     *clock.Fake
	effects *EffectStore
	intents *IntentStore
	ledger  *ledger.Service
	reg     *Registry
}

func newFixture(t *testing.T) *fixture {
	requireEnv(t)
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("itest")
	return &fixture{
		t: t, ctx: context.Background(), clk: clk,
		effects: NewEffectStore(clk, out, aw), intents: NewIntentStore(clk, out, aw),
		ledger: ledger.NewService(clk, "itest"), reg: NewRegistry(),
	}
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

func (f *fixture) account() accounts.AccountID {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "econ-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	return a.ID
}

func (f *fixture) simAsset(symbol string) assets.AssetID {
	f.t.Helper()
	a, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: symbol, Name: symbol + " (sandbox)", Decimals: 6,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	return a.ID
}

func (f *fixture) realAsset(symbol string) assets.AssetID {
	f.t.Helper()
	a, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: "solana-itest-" + uuid.NewString(), MintAddress: uuid.NewString(), Kind: assets.KindSPLToken,
		ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: symbol, Name: symbol, Decimals: 6,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	return a.ID
}

func (f *fixture) fund(acct accounts.AccountID, asset assets.AssetID, qty int64) ledger.PostResult {
	f.t.Helper()
	q := money.QuantityFromInt64(qty)
	var res ledger.PostResult
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ref := uuid.NewString()
		res, err = f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "sbx-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: q},
			},
		})
		return err
	}))
	return res
}

var systemActor = Actor{Type: security.ActorSystem, ID: "econ-itest"}

// --- Exactly-once -------------------------------------------------------------

func TestIntegration_EffectIsClaimedExactlyOnceUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	key := "itest:" + uuid.NewString()
	req := BeginRequest{Key: key, Subsystem: SubsystemTransfers, Kind: "P2P_TRANSFER",
		Request: map[string]string{"amount": "100"}, PostsLedger: true, Actor: systemActor}

	const n = 24
	var wg sync.WaitGroup
	results := make(chan struct {
		id     EffectID
		replay bool
		err    error
	}, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var e Effect
			var replay bool
			err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				var err error
				e, replay, err = f.effects.Begin(ctx, tx, req)
				return err
			})
			results <- struct {
				id     EffectID
				replay bool
				err    error
			}{e.ID, replay, err}
		}()
	}
	wg.Wait()
	close(results)
	ids := map[EffectID]bool{}
	fresh := 0
	for r := range results {
		require.NoError(t, r.err)
		ids[r.id] = true
		if !r.replay {
			fresh++
		}
	}
	assert.Len(t, ids, 1, "every attempt must resolve to the same effect")
	assert.Equal(t, 1, fresh, "exactly one attempt claims the effect; the rest are replays")

	var rows int
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM economic_effects WHERE effect_key = $1`, key).Scan(&rows))
	assert.Equal(t, 1, rows)
}

func TestIntegration_SameKeyDifferentRequestIsRefused(t *testing.T) {
	f := newFixture(t)
	key := "itest:" + uuid.NewString()
	begin := func(amount string) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := f.effects.Begin(ctx, tx, BeginRequest{Key: key, Subsystem: SubsystemTransfers, Kind: "P2P_TRANSFER",
				Request: map[string]string{"amount": amount}, PostsLedger: true, Actor: systemActor})
			return err
		})
	}
	require.NoError(t, begin("100"))
	require.NoError(t, begin("100"), "a replay is not an error")
	err := begin("999")
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))
}

func TestIntegration_ALedgerEffectIsNotAppliedWithoutItsJournal(t *testing.T) {
	f := newFixture(t)
	var e Effect
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		e, _, err = f.effects.Begin(ctx, tx, BeginRequest{Key: "itest:" + uuid.NewString(), Subsystem: SubsystemMargin,
			Kind: "BORROW", Request: map[string]string{"x": "1"}, PostsLedger: true, Actor: systemActor})
		return err
	}))
	// Through Go: refused before SQL.
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.effects.Apply(ctx, tx, e.ID, ApplyRequest{Actor: systemActor})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// Around Go: a bound transition row and a bare UPDATE, still refused by the CHECK.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO economic_effect_transitions (id, effect_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'PENDING','APPLIED','SYSTEM','x','bypass')`, uuid.New(), e.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE economic_effects SET status = 'APPLIED' WHERE id = $1`, e.ID)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "economic_effects_applied_has_journal")
}

func TestIntegration_TheEffectStatusMachineHoldsInSQL(t *testing.T) {
	f := newFixture(t)
	var e Effect
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		e, _, err = f.effects.Begin(ctx, tx, BeginRequest{Key: "itest:" + uuid.NewString(), Subsystem: SubsystemBridge,
			Kind: "BRIDGE", Request: map[string]string{"x": "1"}, PostsLedger: false, Actor: systemActor})
		if err != nil {
			return err
		}
		e, err = f.effects.Fail(ctx, tx, e.ID, "DESTINATION_REJECTED", systemActor, "")
		return err
	}))
	require.Equal(t, EffectFailed, e.Status)

	// FAILED -> APPLIED is illegal, even with a transition row describing it.
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO economic_effect_transitions (id, effect_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'FAILED','APPLIED','SYSTEM','x','bypass')`, uuid.New(), e.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE economic_effects SET status = 'APPLIED' WHERE id = $1`, e.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "EF001", db.SQLState(err))

	// A status change with no transition row is refused at COMMIT.
	var e2 Effect
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		e2, _, err = f.effects.Begin(ctx, tx, BeginRequest{Key: "itest:" + uuid.NewString(), Subsystem: SubsystemBridge,
			Kind: "BRIDGE", Request: map[string]string{"x": "2"}, PostsLedger: false, Actor: systemActor})
		return err
	}))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE economic_effects SET status = 'UNKNOWN' WHERE id = $1`, e2.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err))

	// Identity is frozen.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE economic_effects SET effect_key = 'rewritten' WHERE id = $1`, e2.ID)
		return err
	})
	require.Error(t, err)
}

func TestIntegration_SQLAndGoAgreeOnEffectTransitions(t *testing.T) {
	requireEnv(t)
	for _, from := range AllEffectStatuses() {
		for _, to := range AllEffectStatuses() {
			var legal bool
			require.NoError(t, testDB.QueryRow(context.Background(), `SELECT cp_effect_transition_legal($1,$2)`, from, to).Scan(&legal))
			assert.Equal(t, CanTransitionEffect(from, to), legal, "%s -> %s", from, to)
		}
	}
}

func TestIntegration_UnknownIsResolvedNotRetried(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	usd := f.simAsset("SBXUSD")
	key := "itest:" + uuid.NewString()
	var e Effect
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		e, _, err = f.effects.Begin(ctx, tx, BeginRequest{Key: key, Subsystem: SubsystemSandboxFunding, Kind: "FAUCET",
			AccountID: acct.String(), Request: map[string]string{"x": "1"}, PostsLedger: true, Actor: systemActor})
		if err != nil {
			return err
		}
		e, err = f.effects.MarkUnknown(ctx, tx, e.ID, "devnet:tx-1", systemActor, "submit timed out")
		return err
	}))
	// updated_at is the database's clock, not the fixture's, so the bound is
	// taken from the wall clock.
	unresolved, err := f.effects.ListUnresolved(f.ctx, testDB, SubsystemSandboxFunding, time.Now().Add(time.Hour), 1000)
	require.NoError(t, err)
	found := false
	for _, u := range unresolved {
		if u.ID == e.ID {
			found = true
		}
	}
	assert.True(t, found, "an UNKNOWN effect is on the reconciliation worklist")

	// A replay of the same request returns UNKNOWN; the caller must not retry.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		again, replay, err := f.effects.Begin(ctx, tx, BeginRequest{Key: key, Subsystem: SubsystemSandboxFunding, Kind: "FAUCET",
			AccountID: acct.String(), Request: map[string]string{"x": "1"}, PostsLedger: true, Actor: systemActor})
		require.True(t, replay)
		require.Equal(t, EffectUnknown, again.Status)
		return err
	}))

	// Reconciliation observed the outcome and posts the journal it implies.
	post := f.fund(acct, usd, 5_000_000)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		applied, err := f.effects.Apply(ctx, tx, e.ID, ApplyRequest{JournalTransactionID: &post.TransactionID, Actor: systemActor, Reason: "observed on devnet"})
		require.Equal(t, EffectApplied, applied.Status)
		return err
	}))
	// Applying the same journal again is the same fact; applying another is refused.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.effects.Apply(ctx, tx, e.ID, ApplyRequest{JournalTransactionID: &post.TransactionID, Actor: systemActor})
		return err
	}))
	other := f.fund(acct, usd, 1)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.effects.Apply(ctx, tx, e.ID, ApplyRequest{JournalTransactionID: &other.TransactionID, Actor: systemActor})
		return err
	})
	require.Error(t, err)

	// One journal can back only one effect.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		e2, _, err := f.effects.Begin(ctx, tx, BeginRequest{Key: "itest:" + uuid.NewString(), Subsystem: SubsystemSandboxFunding, Kind: "FAUCET",
			Request: map[string]string{"x": "2"}, PostsLedger: true, Actor: systemActor})
		if err != nil {
			return err
		}
		_, err = f.effects.Apply(ctx, tx, e2.ID, ApplyRequest{JournalTransactionID: &post.TransactionID, Actor: systemActor})
		return err
	})
	require.Error(t, err, "economic_effects_journal_once")
}

func TestIntegration_EveryEffectTransitionLeavesOutboxAndAudit(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	var e Effect
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		e, _, err = f.effects.Begin(ctx, tx, BeginRequest{Key: "itest:" + uuid.NewString(), Subsystem: SubsystemOrders, Kind: "ORDER_ACCEPTED",
			AccountID: acct.String(), Request: map[string]string{"x": "1"}, PostsLedger: false, Actor: systemActor})
		if err != nil {
			return err
		}
		_, err = f.effects.Apply(ctx, tx, e.ID, ApplyRequest{Actor: systemActor, Reason: "accepted"})
		return err
	}))
	var outbox, auditRows, transitions int
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM outbox_events WHERE topic = 'econ.effect.transitioned' AND aggregate_id = $1`, e.ID.String()).Scan(&outbox))
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM audit_events WHERE resource_type = 'economic_effect' AND resource_id = $1`, e.ID.String()).Scan(&auditRows))
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM economic_effect_transitions WHERE effect_id = $1`, e.ID).Scan(&transitions))
	assert.Equal(t, 2, outbox, "claim and apply each emit one event")
	assert.Equal(t, 2, auditRows)
	assert.Equal(t, 1, transitions, "the claim is the birth; only the change PENDING->APPLIED is a transition")
}

// --- Intents ------------------------------------------------------------------

func TestIntegration_ALiveIntentCannotBeWrittenDown(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	in := validIntent()
	in.AccountID = acct
	in.Mode = gates.ModeLive
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.intents.Submit(ctx, tx, in)
		return err
	})
	require.Error(t, err, "Go refuses LIVE")

	// And the database refuses it when Go is not the caller.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		sum := sha256.Sum256([]byte("x"))
		_, err := tx.Exec(ctx, `INSERT INTO economic_intents (id, account_id, kind, mode, capability, actor_type, actor_id,
			content_hash, idempotency_key, status) VALUES ($1,$2,'TRANSFER_VALUE','LIVE','P2P_TRANSFERS','USER','u',$3,'k','RECEIVED')`,
			uuid.New(), acct, sum[:])
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "economic_intents_mode_check")
}

func TestIntegration_IntentIsIdempotentAndItsLifecycleIsBound(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	in := validIntent()
	in.AccountID = acct
	in.IdempotencyKey = "idem-" + uuid.NewString()

	var first, second Intent
	var existing bool
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, _, err = f.intents.Submit(ctx, tx, in)
		return err
	}))
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, existing, err = f.intents.Submit(ctx, tx, in)
		return err
	}))
	assert.True(t, existing)
	assert.Equal(t, first.ID, second.ID)

	changed := in
	changed.Terms = []byte(`{"recipient":"someone-else"}`)
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.intents.Submit(ctx, tx, changed)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))

	// Legal path.
	for _, to := range []IntentStatus{StatusRouted, StatusRiskApproved, StatusReserved, StatusExecuting, StatusCompleted} {
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.intents.Transition(ctx, tx, first.ID, to, systemActor, "step", "")
			return err
		}), "to %s", to)
	}
	// Terminal.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.intents.Transition(ctx, tx, first.ID, StatusFailed, systemActor, "after completion", "")
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// An agent does not move an intent.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.intents.Transition(ctx, tx, first.ID, StatusCancelled, Actor{Type: security.ActorAgent, ID: "a"}, "x", "")
		return err
	})
	require.Error(t, err)

	// Identity is frozen in SQL, twice over. The application role cannot even
	// name the column (42501: it holds UPDATE on status columns only), and a
	// role that can -- the migration owner -- is stopped by the trigger.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE economic_intents SET quantity = 1 WHERE id = $1`, first.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err))
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `UPDATE economic_intents SET quantity = 1 WHERE id = $1`, first.ID)
	require.Error(t, err)
	assert.Equal(t, "EF002", db.SQLState(err))

	page, next, err := f.intents.ListForAccount(f.ctx, testDB, acct, "", 10)
	require.NoError(t, err)
	assert.Empty(t, next)
	require.Len(t, page, 1)
	assert.Equal(t, StatusCompleted, page[0].Status)
}

// --- The ledger's simulated-only rules, from the database side ---------------

func TestIntegration_ASimulatedOnlyKindCannotTouchRealCapital(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	real := f.realAsset("USDC")
	q := money.QuantityFromInt64(10)
	posting := ledger.Posting{
		Kind: ledger.KindSandboxFunding, IdempotencyKey: "sbx:" + uuid.NewString(),
		Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: uuid.NewString()}, EffectiveAt: f.clk.Now(),
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, real), Side: ledger.Debit, Quantity: q},
			{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, real), Side: ledger.Credit, Quantity: q},
		},
	}
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.ledger.Post(ctx, tx, posting)
		return err
	})
	require.Error(t, err, "the application refuses it")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// The database refuses it too, when the posting bypasses the service.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawJournal(ctx, f, tx, ledger.KindSandboxFunding, []rawEntry{
			{ledger.CustomerAccount(acct, ledger.CodeWallet, real), ledger.Debit, 10},
			{ledger.CustomerAccount(acct, ledger.CodeCapital, real), ledger.Credit, 10},
		})
	})
	require.Error(t, err)
	assert.Equal(t, "SM001", db.SQLState(err), "%v", err)
}

func TestIntegration_ATransferMustBalancePerParty(t *testing.T) {
	f := newFixture(t)
	a, b := f.account(), f.account()
	usd := f.simAsset("SBXUSD")
	f.fund(a, usd, 1_000)

	// Balanced per asset, not per party: B's wallet grows with nothing in B's
	// books to explain it. The database refuses it at COMMIT.
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawJournal(ctx, f, tx, ledger.KindP2PTransfer, []rawEntry{
			{ledger.CustomerAccount(a, ledger.CodeWallet, usd), ledger.Credit, 100},
			{ledger.CustomerAccount(b, ledger.CodeWallet, usd), ledger.Debit, 100},
		})
	})
	require.Error(t, err)
	assert.Equal(t, "SM002", db.SQLState(err))

	// The four-entry transfer balances per party and posts.
	q := money.QuantityFromInt64(100)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindP2PTransfer, IdempotencyKey: "p2p:" + ref,
			Reference: ledger.FinancialEventReference{Type: "p2p_transfer", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(a, ledger.CodeTransferOut, usd), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(a, ledger.CodeWallet, usd), Side: ledger.Credit, Quantity: q},
				{Account: ledger.CustomerAccount(b, ledger.CodeWallet, usd), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(b, ledger.CodeTransferIn, usd), Side: ledger.Credit, Quantity: q},
			},
		})
		return err
	}))
	ba, err := f.ledger.Balance(f.ctx, testDB, ledger.CustomerAccount(a, ledger.CodeWallet, usd))
	require.NoError(t, err)
	bb, err := f.ledger.Balance(f.ctx, testDB, ledger.CustomerAccount(b, ledger.CodeWallet, usd))
	require.NoError(t, err)
	assert.Equal(t, "900", ba.String())
	assert.Equal(t, "100", bb.String())
}

func TestIntegration_FacilityBooksAreSegregatedByKind(t *testing.T) {
	f := newFixture(t)
	usd := f.simAsset("SBXUSD")
	var issuer Facility
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issuer, err = f.reg.EnsureFacility(ctx, tx, ledger.FacilityStableIssuer, "itest issuer "+uuid.NewString(), systemActor)
		return err
	}))
	q := money.QuantityFromInt64(50)
	fund := func(debit, credit ledger.Code) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			ref := uuid.NewString()
			_, err := f.ledger.Post(ctx, tx, ledger.Posting{
				Kind: ledger.KindFacilityFunding, IdempotencyKey: "fac:" + ref,
				Reference: ledger.FinancialEventReference{Type: "facility_funding", ID: ref}, EffectiveAt: f.clk.Now(),
				Entries: []ledger.Entry{
					{Account: issuer.Account(debit, usd), Side: ledger.Debit, Quantity: q},
					{Account: issuer.Account(credit, usd), Side: ledger.Credit, Quantity: q},
				},
			})
			return err
		})
	}
	require.NoError(t, fund(ledger.CodeStableReserve, ledger.CodeStableReserveFunding))
	err := fund(ledger.CodePoolCash, ledger.CodePoolCapital)
	require.Error(t, err, "a stable issuer cannot hold a lending pool's accounts")
	assert.Equal(t, "FA001", db.SQLState(err))

	// A facility that does not exist cannot hold an account at all.
	ghost := Facility{ID: NewFacilityID(), Kind: ledger.FacilityStableIssuer}
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.ledger.EnsureAccount(ctx, tx, ghost.Account(ledger.CodeStableReserve, usd))
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "FA001", db.SQLState(err))
}

func TestIntegration_HouseAndFacilityBooksCannotHoldRealValue(t *testing.T) {
	f := newFixture(t)
	real := f.realAsset("USDC")
	var house HouseAccount
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		house, err = f.reg.CreateHouseAccount(ctx, tx, "itest house "+uuid.NewString(), HouseMarketMaking, systemActor)
		return err
	}))
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.ledger.EnsureAccount(ctx, tx, house.Account(ledger.CodeHouseInventory, real))
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ledger_accounts_segregated_books_are_simulated")

	// An agent never creates a facility or a house book.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.reg.CreateHouseAccount(ctx, tx, "agent house", HouseHedging, Actor{Type: security.ActorAgent, ID: "agent"})
		return err
	})
	require.Error(t, err)
}

// --- Authorizer against the real gate and kill-switch readers ---------------

func TestIntegration_AuthorizerReadsRealGatesAndKillSwitches(t *testing.T) {
	f := newFixture(t)
	local, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, f.clk)
	require.NoError(t, err)
	staging, err := gates.NewChecker("STAGING", func(gates.Capability) bool { return true }, f.clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})

	require.NoError(t, NewAuthorizer(local, kills).Require(f.ctx, testDB, gates.Derivatives, gates.ModePaper))
	err = NewAuthorizer(staging, kills).Require(f.ctx, testDB, gates.Derivatives, gates.ModePaper)
	require.Error(t, err, "STAGING has no ACTIVE derivatives gate")
	assert.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))
	err = NewAuthorizer(local, kills).Require(f.ctx, testDB, gates.Derivatives, gates.ModeLive)
	require.Error(t, err)
	assert.Contains(t, err.Error(), gates.ReasonLiveNotImplemented)
}

func TestIntegration_EvidenceIsWriteOnceAndReplayable(t *testing.T) {
	f := newFixture(t)
	key := "itest/" + uuid.NewString()
	var a, b EvidenceRef
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = RecordEvidence(ctx, tx, Evidence{Key: key, Document: map[string]string{"decision": "ALLOW"}})
		return err
	}))
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = RecordEvidence(ctx, tx, Evidence{Key: key, Document: map[string]string{"decision": "ALLOW"}})
		return err
	}))
	assert.Equal(t, a.SHA256, b.SHA256, "a replay of the same document is the same evidence")
	assert.True(t, strings.HasPrefix(a.URI, "pg://"))
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordEvidence(ctx, tx, Evidence{Key: key, Document: map[string]string{"decision": "REJECT"}})
		return err
	})
	require.Error(t, err, "evidence under an existing key cannot be rewritten")
}

// --- raw journal helper (bypasses internal/ledger on purpose) -----------------

type rawEntry struct {
	ref  ledger.AccountRef
	side ledger.Side
	qty  int64
}

func rawJournal(ctx context.Context, f *fixture, tx pgx.Tx, kind ledger.Kind, entries []rawEntry) error {
	txID := uuid.New()
	sum := sha256.Sum256([]byte(txID.String()))
	if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id,
		effective_at, posted_by_actor_type, posted_by_actor_id, content_hash) VALUES ($1,$2,$3,'raw',$4,now(),'SYSTEM','raw',$5)`,
		txID, kind, "raw:"+txID.String(), txID.String(), sum[:]); err != nil {
		return err
	}
	for i, e := range entries {
		acct, err := f.ledger.EnsureAccount(ctx, tx, e.ref)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, uuid.New(), txID, i, acct.ID, e.ref.AssetID, e.side, e.qty); err != nil {
			return err
		}
	}
	return nil
}
