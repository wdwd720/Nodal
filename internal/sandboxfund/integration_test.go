//go:build integration

package sandboxfund

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
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
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
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
		fmt.Fprintln(os.Stderr, "sandboxfund integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "sandboxfund-itest", MaxConns: 30, LockTimeout: 60 * time.Second}); err != nil {
		fmt.Fprintln(os.Stderr, "sandboxfund integration: open:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

type fixture struct {
	t       *testing.T
	ctx     context.Context
	clk     *clock.Fake
	ledger  *ledger.Service
	effects *econ.EffectStore
	kills   *switchableKills
	faucet  *Faucet
	dollar  assets.Asset
}

// switchableKills is a kill-switch reader whose global switch a test flips.
type switchableKills struct{ global bool }

func (k *switchableKills) Snapshot(context.Context, db.Querier) (killswitch.Snapshot, error) {
	if !k.global {
		return killswitch.Snapshot{}, nil
	}
	return killswitch.Snapshot{Switches: []killswitch.Switch{{Kind: killswitch.GlobalNewRiskKill, ScopeID: killswitch.GlobalScope, Active: true}}}, nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	f := &fixture{t: t, ctx: context.Background(), clk: clk, ledger: ledger.NewService(clk, "itest"),
		effects: econ.NewEffectStore(clk, event.NewOutbox(clk), audit.NewWriterWithBuildVersion("itest")),
		kills:   &switchableKills{}}
	f.faucet = f.build(config.EnvTest, false, DefaultPolicy())
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		f.dollar, err = EnsureSandboxDollar(ctx, tx)
		return err
	}))
	return f
}

func (f *fixture) build(env config.Environment, enabled bool, p Policy) *Faucet {
	f.t.Helper()
	fc, err := New(Deps{Clock: f.clk, Ledger: f.ledger, Effects: f.effects, Environment: env, Enabled: enabled, Policy: p,
		KillSwitches: f.kills})
	require.NoError(f.t, err)
	return fc
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

type customer struct {
	user    accounts.UserID
	account accounts.AccountID
	p       security.Principal
}

func (f *fixture) customer() customer {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "sandboxfund-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	return customer{user: u.ID, account: a.ID, p: security.Principal{SubjectID: u.ID.String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{a.ID.String()}}}
}

func (f *fixture) fund(fc *Faucet, p security.Principal, acct accounts.AccountID, asset assets.AssetID, qty int64, key string) (Grant, error) {
	var g Grant
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = fc.Fund(ctx, tx, p, acct, asset, money.QuantityFromInt64(qty), key)
		return err
	})
	return g, err
}

func (f *fixture) wallet(acct accounts.AccountID, asset assets.AssetID) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ledger.CustomerAccount(acct, ledger.CodeWallet, asset))
	require.NoError(f.t, err)
	return b
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

func requireCode(t *testing.T, err error, code errs.Code) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, code, errs.CodeOf(err), "%v", err)
}

var system = security.Principal{SubjectID: "demo-seeder", ActorType: security.ActorSystem}

// TestIntegration_AGrantPostsSandboxFundingWithItsEffectAndEvidence: the
// happy path leaves the SANDBOX_FUNDING journal (Dr WALLET / Cr CAPITAL), the
// APPLIED effect naming it, the evidence, and the grant record.
func TestIntegration_AGrantPostsSandboxFundingWithItsEffectAndEvidence(t *testing.T) {
	f := newFixture(t)
	c := f.customer()
	g, err := f.fund(f.faucet, c.p, c.account, f.dollar.ID, 250_000_000, "grant-1")
	require.NoError(t, err)
	assert.False(t, g.Replayed)
	assert.Equal(t, config.EnvTest, g.Environment)
	assert.Equal(t, security.ActorUser, g.ActorType)
	assert.Equal(t, "250000000", f.wallet(c.account, f.dollar.ID).String())
	capital, err := f.ledger.Balance(f.ctx, testDB, ledger.CustomerAccount(c.account, ledger.CodeCapital, f.dollar.ID))
	require.NoError(t, err)
	assert.Equal(t, "250000000", capital.String())

	j, err := f.ledger.Transaction(f.ctx, testDB, g.JournalTransactionID)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindSandboxFunding, j.Kind)
	assert.Len(t, j.Entries, 2)
	eff, err := f.effects.Get(f.ctx, testDB, g.EffectID)
	require.NoError(t, err)
	assert.Equal(t, econ.EffectApplied, eff.Status)
	assert.Equal(t, econ.SubsystemSandboxFunding, eff.Subsystem)
	assert.Equal(t, g.JournalTransactionID, *eff.JournalTransactionID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM provider_evidence WHERE key = $1`, "advanced/sandboxfund/"+g.ID.String()+".json"))
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM outbox_events WHERE topic = 'econ.effect.transitioned' AND aggregate_id = $1`, g.EffectID.String()))
}

// TestIntegration_TheFaucetRefusesNonSimulatedAssets: refused in Go before
// anything is written, and refused by the database if Go is bypassed (SF001).
func TestIntegration_TheFaucetRefusesNonSimulatedAssets(t *testing.T) {
	f := newFixture(t)
	c := f.customer()
	real, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: "solana-itest-" + uuid.NewString(), MintAddress: uuid.NewString(), Kind: assets.KindSPLToken,
		ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "USDC", Name: "USD Coin", Decimals: 6, IsStablecoin: true,
		PegCurrency: valuation.USDPeg, RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	_, err = f.fund(f.faucet, c.p, c.account, real.ID, 1_000_000, "real-1")
	requireCode(t, err, errs.CodeForbidden)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM sandbox_fundings WHERE account_id = $1`, c.account))
	assert.True(t, f.wallet(c.account, real.ID).IsZero())

	// Around Go: a grant row naming a real asset is refused by its trigger,
	// whatever journal and effect it borrows.
	legit, err := f.fund(f.faucet, c.p, c.account, f.dollar.ID, 1_000_000, "legit-1")
	require.NoError(t, err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawGrant(ctx, tx, c.account, real.ID, 1_000_000, legit)
	})
	require.Error(t, err)
	assert.Equal(t, "SF001", db.SQLState(err), "%v", err)
}

func rawGrant(ctx context.Context, tx pgx.Tx, account accounts.AccountID, asset assets.AssetID, qty int64, borrow Grant) error {
	_, err := tx.Exec(ctx, `INSERT INTO sandbox_fundings (id, account_id, asset_id, quantity, idempotency_key, request_hash,
		policy_version, policy_hash, environment, actor_type, actor_id, effect_id, journal_transaction_id, evidence_ref,
		evidence_sha256, granted_at) VALUES ($1,$2,$3,$4,$5,$6,'raw',$7,'TEST','SYSTEM','raw',$8,$9,'pg://raw',$10,now())`,
		NewGrantID(), account, asset, qty, "raw-"+uuid.NewString(), borrow.RequestHash, borrow.PolicyHash,
		borrow.EffectID, borrow.JournalTransactionID, borrow.EvidenceSHA256)
	return err
}

// TestIntegration_AGrantMustMatchItsJournal: a grant row whose quantity is
// not what its journal credited is refused (SF002), and grants are never
// edited.
func TestIntegration_AGrantMustMatchItsJournal(t *testing.T) {
	f := newFixture(t)
	c := f.customer()
	legit, err := f.fund(f.faucet, c.p, c.account, f.dollar.ID, 1_000_000, "legit-1")
	require.NoError(t, err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawGrant(ctx, tx, c.account, f.dollar.ID, 9_000_000, legit)
	})
	require.Error(t, err)
	assert.Equal(t, "SF002", db.SQLState(err), "%v", err)

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sandbox_fundings SET quantity = 9000000 WHERE id = $1`, legit.ID)
		return err
	})
	require.Error(t, err, "a grant is append-only")
}

// TestIntegration_TheFaucetEnforcesItsRollingCap: up to the cap and not one
// base unit more inside the window, per account and per asset; the window
// rolls.
func TestIntegration_TheFaucetEnforcesItsRollingCap(t *testing.T) {
	f := newFixture(t)
	fc := f.build(config.EnvTest, false, Policy{Version: "itest-cap", WindowMS: (24 * time.Hour).Milliseconds(), CapWholeUnits: 1_000})
	c := f.customer()
	_, err := f.fund(fc, c.p, c.account, f.dollar.ID, 600_000_000, "a")
	require.NoError(t, err)
	_, err = f.fund(fc, c.p, c.account, f.dollar.ID, 400_000_000, "b")
	require.NoError(t, err, "exactly the cap")
	_, err = f.fund(fc, c.p, c.account, f.dollar.ID, 1, "c")
	requireCode(t, err, errs.CodeRateLimited)
	assert.Equal(t, "1000000000", f.wallet(c.account, f.dollar.ID).String())

	// Another account and another asset have their own caps.
	other := f.customer()
	_, err = f.fund(fc, other.p, other.account, f.dollar.ID, 1_000_000_000, "a")
	require.NoError(t, err)
	var gold assets.Asset
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		gold, err = EnsureSandboxAsset(ctx, tx, AssetSpec{Symbol: "SBX-GOLD-" + strings.ToUpper(uuid.NewString()[:4]), Name: "Sandbox gold", Decimals: 9})
		return err
	}))
	_, err = f.fund(fc, c.p, c.account, gold.ID, 1_000_000_000_000, "gold")
	require.NoError(t, err, "1,000 whole units of a 9-decimal asset")

	f.clk.Advance(24*time.Hour + time.Second)
	_, err = f.fund(fc, c.p, c.account, f.dollar.ID, 1_000_000_000, "d")
	require.NoError(t, err, "the window rolled")
}

// TestIntegration_ConcurrentGrantsCannotExceedTheCap: 30 concurrent grants
// of one whole unit against a cap of 10: exactly 10 succeed.
func TestIntegration_ConcurrentGrantsCannotExceedTheCap(t *testing.T) {
	f := newFixture(t)
	fc := f.build(config.EnvTest, false, Policy{Version: "itest-cap10", WindowMS: (24 * time.Hour).Milliseconds(), CapWholeUnits: 10})
	c := f.customer()
	var wg sync.WaitGroup
	results := make(chan error, 30)
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.fund(fc, c.p, c.account, f.dollar.ID, 1_000_000, fmt.Sprintf("k-%d", i))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
			continue
		}
		require.Equal(t, errs.CodeRateLimited, errs.CodeOf(err), "%v", err)
	}
	assert.Equal(t, 10, ok)
	assert.Equal(t, "10000000", f.wallet(c.account, f.dollar.ID).String())
}

// TestIntegration_TheFaucetIsIdempotent: the same key and request is the same
// grant -- one journal, one effect, one credit -- and the same key with a
// different request is refused.
func TestIntegration_TheFaucetIsIdempotent(t *testing.T) {
	f := newFixture(t)
	c := f.customer()
	first, err := f.fund(f.faucet, c.p, c.account, f.dollar.ID, 5_000_000, "same")
	require.NoError(t, err)
	again, err := f.fund(f.faucet, c.p, c.account, f.dollar.ID, 5_000_000, "same")
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, first.JournalTransactionID, again.JournalTransactionID)
	assert.Equal(t, "5000000", f.wallet(c.account, f.dollar.ID).String())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM sandbox_fundings WHERE account_id = $1`, c.account))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_effects WHERE effect_key = $1`, "sandbox-funding:"+c.account.String()+":same"))
	_, err = f.fund(f.faucet, c.p, c.account, f.dollar.ID, 6_000_000, "same")
	requireCode(t, err, errs.CodeInvalidIdempotencyReuse)
}

// TestIntegration_TheFaucetIsGatedOnTheEnvironment: on in TEST, off in
// STAGING and PROD unless deployment configuration enables it.
func TestIntegration_TheFaucetIsGatedOnTheEnvironment(t *testing.T) {
	f := newFixture(t)
	c := f.customer()
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		_, err := f.fund(f.build(env, false, DefaultPolicy()), system, c.account, f.dollar.ID, 1, "off-"+string(env))
		requireCode(t, err, errs.CodeCapabilityNotApproved)
	}
	assert.True(t, f.wallet(c.account, f.dollar.ID).IsZero())
	g, err := f.fund(f.build(config.EnvProd, true, DefaultPolicy()), system, c.account, f.dollar.ID, 1, "on-prod")
	require.NoError(t, err)
	assert.Equal(t, config.EnvProd, g.Environment)
	assert.Equal(t, security.ActorSystem, g.ActorType)
}

// TestIntegration_OnlyTheOwnerAServiceOrTheSystemMayFund: an agent, an
// operator and another customer are refused; so is a frozen account.
func TestIntegration_OnlyTheOwnerAPermittedServiceOrTheSystemMayFund(t *testing.T) {
	f := newFixture(t)
	c, stranger := f.customer(), f.customer()
	for name, p := range map[string]security.Principal{
		"agent":    security.AgentPrincipal(uuid.NewString(), c.account.String()),
		"operator": {SubjectID: uuid.NewString(), ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin}},
		"stranger": stranger.p,
	} {
		_, err := f.fund(f.faucet, p, c.account, f.dollar.ID, 1, "who-"+name)
		requireCode(t, err, errs.CodeForbidden)
	}
	_, err := f.fund(f.faucet, security.Principal{SubjectID: "svc", ActorType: security.ActorService}, c.account, f.dollar.ID, 1, "svc")
	requireCode(t, err, errs.CodeForbidden) // a service needs sandbox:fund
	_, err = f.fund(f.faucet, security.Principal{SubjectID: "svc", ActorType: security.ActorService,
		Roles: []security.Role{security.RoleCustomer}}, c.account, f.dollar.ID, 1, "svc-permitted")
	require.NoError(t, err)

	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := accounts.NewRepository().Transition(ctx, tx, c.account, accounts.StatusChange{
			To: accounts.StatusFrozen, ActorType: "OPERATOR", ActorID: "itest", Reason: "itest"}, f.clk.Now())
		return err
	}))
	_, err = f.fund(f.faucet, system, c.account, f.dollar.ID, 1, "frozen")
	requireCode(t, err, errs.CodeAccountFrozen)
}

// TestIntegration_EnsureSandboxDollarIsOneAssetUnderConcurrency: concurrent
// callers all get the same asset, there is exactly one, and a conflicting
// definition is refused rather than reused.
func TestIntegration_EnsureSandboxDollarIsOneAssetUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	spec := AssetSpec{Symbol: "SBX-T" + strings.ToUpper(uuid.NewString()[:6]), Name: "Sandbox test dollar", Decimals: 6, USDPegged: true}
	type result struct {
		id  assets.AssetID
		err error
	}
	var wg sync.WaitGroup
	results := make(chan result, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var a assets.Asset
			err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				var err error
				a, err = EnsureSandboxAsset(ctx, tx, spec)
				return err
			})
			results <- result{a.ID, err}
		}()
	}
	wg.Wait()
	close(results)
	seen := map[assets.AssetID]bool{}
	for r := range results {
		require.NoError(t, r.err)
		seen[r.id] = true
	}
	assert.Len(t, seen, 1)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM assets WHERE chain = 'nodal-sandbox' AND symbol = $1`, spec.Symbol))

	var dollar assets.Asset
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		dollar, err = EnsureSandboxDollar(ctx, tx)
		return err
	}))
	assert.Equal(t, f.dollar.ID, dollar.ID)
	assert.Equal(t, assets.KindSimulated, dollar.Kind)
	assert.Equal(t, assets.SandboxChain, dollar.Chain)
	assert.Equal(t, uint8(6), dollar.Decimals)
	assert.True(t, dollar.IsStablecoin)
	assert.Equal(t, valuation.USDPeg, dollar.PegCurrency)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM assets WHERE chain = 'nodal-sandbox' AND symbol = 'SBX-USD'`))

	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := EnsureSandboxAsset(ctx, tx, AssetSpec{Symbol: SandboxDollarSymbol, Name: "imposter", Decimals: 2, USDPegged: true})
		return err
	})
	requireCode(t, err, errs.CodeConflict)
}

// TestIntegration_EveryFaucetEnumMatchesItsCheck: the Go lists and the CHECK
// constraints holding the same lists agree exactly.
func TestIntegration_EveryFaucetEnumMatchesItsCheck(t *testing.T) {
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
	literal := regexp.MustCompile(`'([^']*)'::text`)
	values := func(constraint string) []string {
		var def string
		require.NoError(t, testDB.QueryRow(context.Background(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid = 'sandbox_fundings'::regclass AND conname = $1`, constraint).Scan(&def), constraint)
		var out []string
		for _, m := range literal.FindAllStringSubmatch(def, -1) {
			out = append(out, m[1])
		}
		sort.Strings(out)
		return out
	}
	envs := make([]string, 0)
	for _, e := range AllEnvironments() {
		envs = append(envs, string(e))
	}
	sort.Strings(envs)
	actors := make([]string, 0)
	for _, a := range AllowedActorTypes() {
		actors = append(actors, string(a))
	}
	sort.Strings(actors)
	assert.Equal(t, envs, values("sandbox_fundings_environment_check"))
	assert.Equal(t, actors, values("sandbox_fundings_actor_type_check"))
}

// TestIntegration_TheGlobalKillSwitchStopsTheFaucet: minting play money is new
// value, and the global new-risk kill switch stops it.
func TestIntegration_TheGlobalKillSwitchStopsTheFaucet(t *testing.T) {
	f := newFixture(t)
	c := f.customer()
	f.kills.global = true
	_, err := f.fund(f.faucet, c.p, c.account, f.dollar.ID, 1, "killed")
	requireCode(t, err, errs.CodeKillSwitchActive)
	f.kills.global = false
	_, err = f.fund(f.faucet, c.p, c.account, f.dollar.ID, 1, "released")
	require.NoError(t, err)
}

// TestIntegration_AServiceFundsOnlyWithSandboxFund: a service principal no
// longer funds any account it names; it needs the sandbox:fund permission.
func TestIntegration_AServiceFundsOnlyWithSandboxFund(t *testing.T) {
	f := newFixture(t)
	c := f.customer()
	bare := security.Principal{SubjectID: "svc-" + uuid.NewString(), ActorType: security.ActorService, AuthTime: f.clk.Now()}
	_, err := f.fund(f.faucet, bare, c.account, f.dollar.ID, 1, "svc-bare")
	requireCode(t, err, errs.CodeForbidden)
	granted := bare
	granted.Roles = []security.Role{security.RoleCustomer}
	_, err = f.fund(f.faucet, granted, c.account, f.dollar.ID, 1, "svc-granted")
	require.NoError(t, err)
}
