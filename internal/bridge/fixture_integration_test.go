//go:build integration

package bridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
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
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
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
		fmt.Fprintln(os.Stderr, "bridge integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "bridge-itest", MaxConns: 40}); err != nil {
		fmt.Fprintln(os.Stderr, "bridge integration: open:", err)
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

// Short chains: a 12-block window and finality two blocks deep.
const (
	itWindow   = 12
	itDepth    = 2
	itAttempts = 3
)

type nopEmitter struct{}

func (nopEmitter) Emit(context.Context, pgx.Tx, string, any) error { return nil }

type fixture struct {
	t        *testing.T
	ctx      context.Context
	clk      *clock.Fake
	host     *devnethost.Host
	svc      *Service
	ledger   *ledger.Service
	capital  *capital.Service
	recon    *reconciliation.Repository
	effects  *econ.EffectStore
	intents  *econ.IntentStore
	custody  *stableasset.CustodialWalletSigner
	attester *Attester
	minter   devnet.Keypair
	network  string
	tag      string
	route    Route
}

var operator = econ.Actor{Type: security.ActorOperator, ID: "operator-1"}

var facts = AccountFacts{Jurisdiction: "US-CA", Verification: valuedomain.VerificationEnhanced, IdentityVerified: true}

func newFixture(t *testing.T) *fixture {
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("bridge-itest")
	tag := uuid.NewString()[:8]
	network := "br-" + tag
	attester := NewAttester(seed("attester-" + tag))
	cfg := devnethost.DefaultConfig(network, config.EnvTest, attester.PublicKey())
	for i := range cfg.Chains {
		cfg.Chains[i].BlockhashWindow, cfg.Chains[i].FinalityDepth, cfg.Chains[i].FeePerTx = itWindow, itDepth, 10
	}
	host, err := devnethost.Open(ctx, devnethost.NewPgStore(testDB), cfg, clk)
	require.NoError(t, err)
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	f := &fixture{
		t: t, ctx: ctx, clk: clk, host: host, network: network, tag: tag, attester: attester,
		ledger: ledger.NewService(clk, "bridge-itest"), capital: capital.NewService(clk, nopEmitter{}),
		recon: reconciliation.NewRepository(clk, out, aw), effects: econ.NewEffectStore(clk, out, aw),
		intents: econ.NewIntentStore(clk, out, aw), custody: stableasset.NewCustodialWalletSigner(seed("custody-" + tag)),
		minter: devnet.NewKeypairFromSeed(seed("minter-" + tag)),
	}
	f.svc, err = New(Deps{
		DB: testDB, Clock: clk, Host: host, Intents: f.intents, Effects: f.effects,
		Authorizer: econ.NewAuthorizer(checker, killswitch.NewChecker(killswitch.Policy{})),
		Facilities: econ.NewRegistry(), Ledger: f.ledger, Capital: f.capital, Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Kills: killswitch.NewChecker(killswitch.Policy{}),
		Reconciliation: f.recon, Audit: aw, Accounts: accounts.NewRepository(), Assets: assets.NewRepository(),
		Holder: f.custody, Attester: attester,
	})
	require.NoError(t, err)
	f.route = f.newRoute(itAttempts)
	return f
}

// newRoute creates a SIMULATED source asset on sbx-solana, minted by a test
// authority, and a route to sbx-ethereum. Symbols are unique to the fixture:
// the platform's one sandbox dollar belongs to econ and is never created here.
func (f *fixture) newRoute(attempts int) Route {
	f.t.Helper()
	suffix := uuid.NewString()[:6]
	mint := pk("src-mint-" + f.tag + suffix)
	require.NoError(f.t, f.host.EnsureMint(f.ctx, devnethost.ChainSolana, devnet.MintConfig{
		Address: mint, Decimals: 6, Authority: f.minter.PublicKey(),
	}))
	src, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: string(devnethost.ChainSolana), MintAddress: mint.String(), Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: "BS" + suffix, Name: "bridge source " + suffix + " (sandbox test asset)",
		Decimals: 6, RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	price, err := money.NewPrice(money.QuantityFromInt64(1), 0, "USD", "ROUTE_UNIT_PRICE", f.clk.Now())
	require.NoError(f.t, err)
	r, err := f.svc.CreateRoute(f.ctx, RouteDefinition{
		Name: "it-" + f.tag + "-" + suffix, SourceAssetID: src.ID, DestinationChain: devnethost.ChainEthereum,
		DestinationSymbol: "BD" + suffix, UnitPrice: price, MaxReleaseAttempts: attempts,
	}, operator)
	require.NoError(f.t, err)
	return r
}

type customer struct {
	id     accounts.AccountID
	ctx    context.Context
	actor  econ.Actor
	holder devnet.PublicKey
}

func (f *fixture) customer() customer {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "bridge-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	acct, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	p := security.Principal{SubjectID: u.ID.String(), ActorType: security.ActorUser, AccountIDs: []string{acct.ID.String()}}
	return customer{id: acct.ID, ctx: security.WithPrincipal(f.ctx, p), actor: econ.Actor{Type: security.ActorUser, ID: u.ID.String()},
		holder: f.custody.HolderAddress(acct.ID)}
}

// fund gives a customer q units of the route's source asset in both places
// that must agree: the ledger WALLET (SANDBOX_FUNDING) and the holder's
// wallet on the source chain (a finalized TOKEN_MINT_TO by the test minter).
func (f *fixture) fund(r Route, c customer, q uint64) {
	f.t.Helper()
	qty := devnet.QuantityOf(q)
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "sbx-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(c.id, ledger.CodeWallet, r.SourceAssetID), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(c.id, ledger.CodeCapital, r.SourceAssetID), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
	f.submitFinal(r.SourceChain, f.minter.PublicKey(), func(tip devnet.BlockInfo) []byte {
		msg := devnet.Message{ChainID: r.SourceChain, RecentBlockhash: tip.Hash, FeePayer: f.minter.PublicKey(),
			Instructions: []devnet.Instruction{devnet.TokenMintTo{Mint: r.SourceMint, ToOwner: c.holder, Amount: q, Authority: f.minter.PublicKey()}}}
		tx, err := devnet.Sign(msg, f.minter)
		require.NoError(f.t, err)
		raw, err := tx.Encode()
		require.NoError(f.t, err)
		return raw
	})
}

// submitFinal pays gas for payer, submits what build returns and finalizes
// it.
func (f *fixture) submitFinal(chain devnet.ChainID, payer devnet.PublicKey, build func(tip devnet.BlockInfo) []byte) devnet.TxID {
	f.t.Helper()
	require.NoError(f.t, f.host.EnsureNativeBalance(f.ctx, chain, payer, 1_000_000, 100_000_000))
	tip, err := f.host.Tip(f.ctx, chain)
	require.NoError(f.t, err)
	txID, err := f.host.Submit(f.ctx, chain, build(tip))
	require.NoError(f.t, err)
	_, err = f.host.ProduceBlocks(f.ctx, chain, 1+itDepth)
	require.NoError(f.t, err)
	st, err := f.host.Status(f.ctx, chain, txID)
	require.NoError(f.t, err)
	require.Equal(f.t, devnet.TxFinalized, st.State, "%s failed with %s", txID, st.Err)
	return txID
}

func (f *fixture) request(r Route, c customer, q uint64, key string, mode gates.Mode) BridgeRequest {
	return BridgeRequest{RouteID: r.ID, AccountID: c.id, Quantity: devnet.QuantityOf(q), IdempotencyKey: key, Mode: mode,
		Actor: c.actor, Facts: facts}
}

func (f *fixture) bridge(r Route, c customer, q uint64, mode gates.Mode) (LegResult, error) {
	return f.svc.Bridge(c.ctx, f.request(r, c, q, "bridge-"+uuid.NewString(), mode))
}

func (f *fixture) mustBridge(r Route, c customer, q uint64) Leg {
	f.t.Helper()
	res, err := f.bridge(r, c, q, gates.ModeSandbox)
	require.NoError(f.t, err)
	require.Equal(f.t, StateReconciled, res.Leg.Status, "failure %q", res.Leg.FailureCode)
	return res.Leg
}

func (f *fixture) leg(lid LegID) Leg {
	f.t.Helper()
	l, err := f.svc.GetLeg(f.ctx, testDB, lid)
	require.NoError(f.t, err)
	return l
}

func (f *fixture) balance(ref ledger.AccountRef) string {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ref)
	require.NoError(f.t, err)
	return b.String()
}

func (f *fixture) wallet(c customer, asset assets.AssetID) string {
	return f.balance(ledger.CustomerAccount(c.id, ledger.CodeWallet, asset))
}

func (f *fixture) inFlight(r Route, c customer) string {
	return f.balance(ledger.CustomerAccount(c.id, ledger.CodeBridgeInFlight, r.SourceAssetID))
}

func (f *fixture) facility(r Route, code ledger.Code, asset assets.AssetID) string {
	return f.balance(r.account(code, asset))
}

func (f *fixture) chainBalance(chain devnet.ChainID, owner, mint devnet.PublicKey) uint64 {
	f.t.Helper()
	b, err := f.host.TokenBalance(f.ctx, chain, owner, mint)
	require.NoError(f.t, err)
	return b
}

func (f *fixture) escrow(r Route) uint64 {
	return f.chainBalance(r.SourceChain, devnet.BridgeEscrowAddress(r.SourceChain), r.SourceMint)
}

func (f *fixture) supply(chain devnet.ChainID, mint devnet.PublicKey) uint64 {
	f.t.Helper()
	s, err := f.host.Supply(f.ctx, chain, mint)
	require.NoError(f.t, err)
	return s
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

// submissions counts the SUBMIT events the host journaled for a transaction:
// the proof that something was, or was not, submitted again.
func (f *fixture) submissions(txID devnet.TxID) int {
	return f.count(`SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`, f.network, string(txID))
}

func (f *fixture) journals(l Leg, kind ledger.Kind) int {
	return f.count(`SELECT count(*) FROM journal_transactions WHERE reference_type = 'bridge_leg' AND reference_id = $1 AND kind = $2`,
		l.ID.String(), string(kind))
}

func (f *fixture) reconcile(r Route) RouteReconciliation {
	f.t.Helper()
	var out RouteReconciliation
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.ReconcileRoute(ctx, tx, r.ID, reconciliation.ModePeriodic, operator)
		return err
	}))
	return out
}

func (f *fixture) routeNow(r Route) Route {
	f.t.Helper()
	got, err := f.svc.GetRoute(f.ctx, testDB, r.ID)
	require.NoError(f.t, err)
	return got
}

func (f *fixture) effect(eid *econ.EffectID) econ.Effect {
	f.t.Helper()
	require.NotNil(f.t, eid)
	e, err := f.effects.Get(f.ctx, testDB, *eid)
	require.NoError(f.t, err)
	return e
}

func (f *fixture) intent(l Leg) econ.Intent {
	f.t.Helper()
	in, err := f.intents.Get(f.ctx, testDB, l.IntentID)
	require.NoError(f.t, err)
	return in
}

func (f *fixture) reservation(l Leg) capital.Reservation {
	f.t.Helper()
	require.NotNil(f.t, l.ReservationID)
	out, err := f.capital.Get(f.ctx, testDB, *l.ReservationID)
	require.NoError(f.t, err)
	return out
}

func (f *fixture) available(c customer, asset assets.AssetID) string {
	f.t.Helper()
	a, err := f.capital.Availability(f.ctx, testDB, c.id.String(), asset, f.clk.Now())
	require.NoError(f.t, err)
	return a.Available.String()
}

func (f *fixture) produce(chain devnet.ChainID, n uint64) {
	f.t.Helper()
	_, err := f.host.ProduceBlocks(f.ctx, chain, n)
	require.NoError(f.t, err)
}

func (f *fixture) observe(lid LegID) Leg {
	f.t.Helper()
	res, err := f.svc.Observe(f.ctx, lid)
	require.NoError(f.t, err)
	return res.Leg
}

func (f *fixture) attempts(lid LegID, kind AttemptKind) []Attempt {
	f.t.Helper()
	atts, err := attemptsOf(f.ctx, testDB, lid, kind)
	require.NoError(f.t, err)
	return atts
}

func (f *fixture) status(chain devnet.ChainID, txID devnet.TxID) devnet.TxStatus {
	f.t.Helper()
	st, err := f.host.Status(f.ctx, chain, txID)
	require.NoError(f.t, err)
	return st
}

// outOfBandLock has a holder's custodied key lock units the platform never
// booked a leg for: a rogue path the custodian cannot tell from a real one,
// which reconciliation must catch.
func (f *fixture) outOfBandLock(r Route, c customer, amount uint64) devnet.TxID {
	f.t.Helper()
	return f.submitFinal(r.SourceChain, c.holder, func(tip devnet.BlockInfo) []byte {
		u := uuid.New()
		plan := BridgePlan{Version: planVersion, LegID: uuid.NewString(), RouteID: r.ID.String(), AccountID: c.id.String(),
			SourceChain: string(r.SourceChain), DestinationChain: string(r.DestinationChain), SourceMint: r.SourceMint.String(),
			DestinationMint: r.DestinationMint.String(), SourceOwner: c.holder.String(), DestinationOwner: c.holder.String(),
			Amount: strconv.FormatUint(amount, 10), Nonce: strconv.FormatUint(binary.BigEndian.Uint64(u[:8]), 10),
			RecentBlockhash: tip.Hash.String(), BlockhashHeight: strconv.FormatUint(tip.Height, 10), QuoteExpiresAt: f.clk.Now()}
		msg, lock, h, err := plan.LockMessage()
		require.NoError(f.t, err)
		_, raw, err := f.custody.SignHolderLock(c.id, h, lock, msg)
		require.NoError(f.t, err)
		return raw
	})
}

// outOfBandRelease has the attester relay a finalized lock the platform never
// booked: a mint on the destination that reconciliation must catch.
func (f *fixture) outOfBandRelease(r Route, lockTx devnet.TxID) devnet.TxID {
	f.t.Helper()
	lt, err := f.host.LockedTransfer(f.ctx, r.SourceChain, lockTx)
	require.NoError(f.t, err)
	return f.submitFinal(r.DestinationChain, f.attester.PublicKey(), func(tip devnet.BlockInfo) []byte {
		msg, err := devnet.BuildRelease(lt, devnet.ReleaseParams{DestMint: r.DestinationMint, Authority: f.attester.PublicKey(),
			FeePayer: f.attester.PublicKey(), RecentBlockhash: tip.Hash})
		require.NoError(f.t, err)
		exp := ReleaseExpectation{Chain: r.DestinationChain, Mint: r.DestinationMint, ToOwner: lt.Lock.DestOwner, Amount: lt.Lock.Amount,
			SourceChain: r.SourceChain, SourceTxID: lockTx, Nonce: lt.Lock.Nonce}
		_, raw, err := f.attester.SignRelease(exp, msg)
		require.NoError(f.t, err)
		return raw
	})
}
