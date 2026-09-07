//go:build integration

package ledger

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The suite runs against an isolated database provisioned by
// `go run ./scripts/testdb -name ledger`, never the shared controlplane_test:
// posted rows can never be deleted, so every test creates its own users,
// accounts and assets and asserts only on those.
var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB        // application role (cp_app)
	testMigrate    *pgxpool.Pool // migration role (table owner), for tamper and trigger tests
)

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "ledger integration: migrate up:", err)
		return 1
	}
	var err error
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "ledger-itest", MaxConns: 25})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledger integration: open app pool:", err)
		return 1
	}
	testMigrate, err = pgxpool.New(ctx, testMigrateURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledger integration: open migrate pool:", err)
		return 1
	}
	code := m.Run()
	testMigrate.Close()
	testDB.Close()
	return code
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping ledger integration test")
	}
}

const (
	usdcDecimals = 6
	solDecimals  = 9
	oneUSDC      = 1_000_000
)

type fixture struct {
	t       *testing.T
	ctx     context.Context
	svc     *Service
	clk     *clock.Fake
	account accounts.AccountID
	usdc    assets.AssetID
	sol     assets.AssetID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	svc := NewService(clk, "itest")
	svc.AllowSeedPostings()
	return &fixture{
		t: t, ctx: ctx, svc: svc, clk: clk,
		account: createAccount(t),
		usdc:    createAsset(t, "USDC", usdcDecimals, true),
		sol:     createAsset(t, "SOL", solDecimals, false),
	}
}

func createAccount(t *testing.T) accounts.AccountID {
	t.Helper()
	repo := accounts.NewRepository()
	user, err := repo.CreateUser(context.Background(), testDB, "ledger-itest", uuid.NewString(), nil)
	require.NoError(t, err)
	acct, err := repo.CreateAccount(context.Background(), testDB, user.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return acct.ID
}

func createAsset(t *testing.T, symbol string, decimals uint8, stable bool) assets.AssetID {
	t.Helper()
	a := assets.Asset{
		Chain: "solana-itest-" + uuid.NewString(), MintAddress: uuid.NewString(), Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto,
		Symbol: symbol, Name: symbol, Decimals: decimals, IsStablecoin: stable,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	}
	if stable {
		a.PegCurrency = "USD"
	}
	created, err := assets.NewRepository().Create(context.Background(), testDB, a)
	require.NoError(t, err)
	return created.ID
}

func (f *fixture) cust(code Code, asset assets.AssetID) AccountRef {
	return CustomerAccount(f.account, code, asset)
}

func (f *fixture) post(p Posting) PostResult {
	f.t.Helper()
	res, err := f.svc.PostInTx(f.ctx, testDB, p)
	require.NoError(f.t, err)
	return res
}

func (f *fixture) balance(ref AccountRef) string {
	f.t.Helper()
	b, err := f.svc.Balance(f.ctx, testDB, ref)
	require.NoError(f.t, err)
	return b.String()
}

// fund posts a settled deposit. The deposit id is made unique per call:
// idempotency keys derive from it, posted rows are never deleted, and the
// suite must be re-runnable against the same database.
func (f *fixture) fund(label string, asset assets.AssetID, qty int64) PostResult {
	f.t.Helper()
	depositID := label + "-" + uuid.NewString()
	p, err := FundingSettledPosting(FundingInputs{AccountID: f.account, DepositID: depositID, AssetID: asset, Quantity: q(qty), EffectiveAt: f.clk.Now()})
	require.NoError(f.t, err)
	return f.post(p)
}

// seedTransfer is a SEED posting moving qty from one WALLET to another.
func (f *fixture) seedTransfer(key string, from, to AccountRef, qty int64) Posting {
	return Posting{
		Kind: KindSeed, IdempotencyKey: key, Reference: FinancialEventReference{Type: "seed", ID: key}, EffectiveAt: f.clk.Now(),
		Entries: []Entry{
			{Account: from, Side: Credit, Quantity: q(qty)},
			{Account: to, Side: Debit, Quantity: q(qty)},
		},
	}
}

func countByKey(t *testing.T, key string) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT count(*) FROM journal_transactions WHERE idempotency_key = $1`, key).Scan(&n))
	return n
}

func rawHeader(ctx context.Context, tx pgx.Tx, txID TransactionID, key string) error {
	_, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id, effective_at, posted_by_actor_type, posted_by_actor_id, content_hash)
		VALUES ($1, 'SEED', $2, 'test', $2, now(), 'SYSTEM', 'itest', $3)`, txID, key, []byte{0})
	return err
}

func rawEntry(ctx context.Context, tx pgx.Tx, txID TransactionID, seq int, acct LedgerAccountID, asset assets.AssetID, side Side, qty int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric)`, NewJournalEntryID(), txID, seq, acct, asset, side, q(qty).String())
	return err
}

// TestIntegration_FundingThenSwap_ExactBalances posts the canonical funding
// and swap of FINANCIAL_MODEL §2.2 and asserts every account balance.
func TestIntegration_FundingThenSwap_ExactBalances(t *testing.T) {
	f := newFixture(t)
	funding := f.fund("dep-1", f.usdc, 1000*oneUSDC)
	assert.False(t, funding.Existing)

	swap, err := SwapPosting(SwapInputs{
		AccountID: f.account, FillID: uuid.NewString(),
		OutAsset: f.usdc, OutQuantity: q(usdc100), InAsset: f.sol, InQuantity: q(sol0_5),
		NetworkFeeAsset: f.sol, NetworkFeeQuantity: q(sol5Micro),
		PlatformFeeAsset: f.usdc, PlatformFeeQuantity: q(usdc0_10),
		OutUSD: usd(10000), InUSD: usd(10000), NetworkFeeUSD: usd(0), PlatformFeeUSD: usd(10),
		EffectiveAt: f.clk.Now(),
	})
	require.NoError(t, err)
	res := f.post(swap)
	require.False(t, res.Existing)
	require.Len(t, res.ContentHash, 32)

	want := map[string]struct {
		ref AccountRef
		bal string
	}{
		"WALLET:USDC":                  {f.cust(CodeWallet, f.usdc), "899900000"},
		"CAPITAL:USDC":                 {f.cust(CodeCapital, f.usdc), "1000000000"},
		"TRADING_OUTFLOW:USDC":         {f.cust(CodeTradingOutflow, f.usdc), "100000000"},
		"FEES_PLATFORM:USDC":           {f.cust(CodeFeesPlatform, f.usdc), "100000"},
		"PLATFORM_FEE_RECEIVABLE:USDC": {PlatformAccount(CodePlatformFeeReceivable, f.usdc), "100000"},
		"PLATFORM_FEE_REVENUE:USDC":    {PlatformAccount(CodePlatformFeeRevenue, f.usdc), "100000"},
		"WALLET:SOL":                   {f.cust(CodeWallet, f.sol), "499995000"},
		"TRADING_INFLOW:SOL":           {f.cust(CodeTradingInflow, f.sol), "500000000"},
		"FEES_NETWORK:SOL":             {f.cust(CodeFeesNetwork, f.sol), "5000"},
	}
	for name, w := range want {
		assert.Equal(t, w.bal, f.balance(w.ref), name)
	}

	tx, err := f.svc.Transaction(f.ctx, testDB, res.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, KindTradeFill, tx.Kind)
	assert.Equal(t, swap.IdempotencyKey, tx.IdempotencyKey)
	assert.Equal(t, res.ContentHash, tx.ContentHash)
	assert.Equal(t, "itest", tx.BuildVersion)
	assert.Equal(t, security.ActorSystem, tx.PostedByActorType)
	require.Len(t, tx.Entries, 9, "the merged Cr WALLET:USDC leg is one entry")
	for i, e := range tx.Entries {
		assert.Equal(t, int32(i), e.Seq)
		assert.Equal(t, res.TransactionID, e.TransactionID)
		if i > 0 {
			prev := tx.Entries[i-1]
			assert.LessOrEqual(t, prev.LedgerAccountID.String(), e.LedgerAccountID.String(), "entries are stored in ledger_account_id order")
		}
		assert.Equal(t, f.usdc == e.Account.AssetID || f.sol == e.Account.AssetID, true)
	}
	var walletUSDC *JournalEntry
	for i := range tx.Entries {
		e := &tx.Entries[i]
		if e.Account.Code == CodeWallet && e.Account.AssetID == f.usdc {
			walletUSDC = e
		}
	}
	require.NotNil(t, walletUSDC)
	assert.Equal(t, Credit, walletUSDC.Side)
	assert.Equal(t, "100100000", walletUSDC.Quantity.String())
	require.NotNil(t, walletUSDC.USDValueMinor)
	assert.Equal(t, int64(10010), *walletUSDC.USDValueMinor)

	balances, err := f.svc.BalancesForOwner(f.ctx, testDB, OwnerCustomer, f.account.String())
	require.NoError(t, err)
	assert.Len(t, balances, 7)
	for _, b := range balances {
		assert.Equal(t, OwnerCustomer, b.Account.OwnerType)
		assert.Equal(t, f.account.String(), b.Account.OwnerID)
		assert.Positive(t, b.EntryCount)
		assert.Equal(t, b.EntryCount, b.Version)
	}
	f.requireNoDrift()
}

// requireNoDrift asserts VerifyBalances reports nothing for the fixture's
// customer and platform accounts.
func (f *fixture) requireNoDrift() {
	f.t.Helper()
	drifts, err := VerifyBalances(f.ctx, testDB)
	require.NoError(f.t, err)
	for _, d := range drifts {
		mine := d.Account.OwnerID == f.account.String() || d.Account.AssetID == f.usdc || d.Account.AssetID == f.sol
		assert.Falsef(f.t, mine, "unexpected drift %+v", d)
	}
}

// TestIntegration_UnbalancedRejectedAtCommit bypasses the application check
// to prove the deferred constraint trigger rejects an unbalanced transaction
// at COMMIT with LEDGER_UNBALANCED and that nothing is persisted.
func TestIntegration_UnbalancedRejectedAtCommit(t *testing.T) {
	f := newFixture(t)
	wallet, capital := f.cust(CodeWallet, f.usdc), f.cust(CodeCapital, f.usdc)

	t.Run("entries do not balance", func(t *testing.T) {
		txID := NewTransactionID()
		key := "raw:" + uuid.NewString()
		err := testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			w, err := f.svc.EnsureAccount(ctx, tx, wallet)
			if err != nil {
				return err
			}
			c, err := f.svc.EnsureAccount(ctx, tx, capital)
			if err != nil {
				return err
			}
			if err := rawHeader(ctx, tx, txID, key); err != nil {
				return err
			}
			if err := rawEntry(ctx, tx, txID, 0, w.ID, f.usdc, Debit, 100); err != nil {
				return err
			}
			return rawEntry(ctx, tx, txID, 1, c.ID, f.usdc, Credit, 99)
		})
		require.Error(t, err, "the deferred trigger must reject the commit")
		requireCode(t, MapError(err), errs.CodeLedgerUnbalanced)
		assert.Equal(t, 0, countByKey(t, key))
		var n int
		require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM journal_entries WHERE transaction_id = $1`, txID).Scan(&n))
		assert.Equal(t, 0, n)
		assert.Equal(t, "0", f.balance(wallet))
		assert.Equal(t, "0", f.balance(capital))
	})

	t.Run("header without entries", func(t *testing.T) {
		key := "raw:" + uuid.NewString()
		err := testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			return rawHeader(ctx, tx, NewTransactionID(), key)
		})
		requireCode(t, MapError(err), errs.CodeLedgerUnbalanced)
		assert.Equal(t, 0, countByKey(t, key))
	})

	t.Run("application rejects before the database", func(t *testing.T) {
		p := fundingPosting(100)
		p.IdempotencyKey = "app:" + uuid.NewString()
		p.Entries[0].Account, p.Entries[1].Account = wallet, capital
		p.Entries[1].Quantity = q(99)
		_, err := f.svc.PostInTx(f.ctx, testDB, p)
		requireCode(t, err, errs.CodeLedgerUnbalanced)
		assert.Equal(t, 0, countByKey(t, p.IdempotencyKey))
	})
}

// TestIntegration_NegativeBalanceRejected: crediting a WALLET that holds
// nothing trips the trigger with LEDGER_NEGATIVE_BALANCE.
func TestIntegration_NegativeBalanceRejected(t *testing.T) {
	f := newFixture(t)
	p := Posting{
		Kind: KindTradeFill, IdempotencyKey: "fill:" + uuid.NewString(), Reference: FinancialEventReference{Type: "fill", ID: "x"}, EffectiveAt: f.clk.Now(),
		Entries: []Entry{
			{Account: f.cust(CodeWallet, f.usdc), Side: Credit, Quantity: q(usdc100)},
			{Account: f.cust(CodeTradingOutflow, f.usdc), Side: Debit, Quantity: q(usdc100)},
		},
	}
	_, err := f.svc.PostInTx(f.ctx, testDB, p)
	requireCode(t, err, errs.CodeLedgerNegativeBalance)
	assert.Equal(t, 0, countByKey(t, p.IdempotencyKey))
	assert.Equal(t, "0", f.balance(f.cust(CodeWallet, f.usdc)))
	assert.Equal(t, "0", f.balance(f.cust(CodeTradingOutflow, f.usdc)))

	// A bidirectional account may go negative.
	adj := Posting{
		Kind: KindReconciliationAdjustment, IdempotencyKey: "recon:" + uuid.NewString(), Reference: FinancialEventReference{Type: "reconciliation_record", ID: "r"},
		EffectiveAt: f.clk.Now(), Metadata: map[string]any{MetadataReasonCode: "DUST"},
		Entries: []Entry{
			{Account: f.cust(CodeReconciliationAdjustment, f.usdc), Side: Debit, Quantity: q(5)},
			{Account: f.cust(CodeWallet, f.usdc), Side: Credit, Quantity: q(5)},
		},
	}
	_, err = f.svc.PostInTx(f.ctx, testDB, adj)
	requireCode(t, err, errs.CodeLedgerNegativeBalance, "WALLET still cannot go negative")
	f.fund("dep-neg", f.usdc, 10)
	f.post(adj)
	assert.Equal(t, "-5", f.balance(f.cust(CodeReconciliationAdjustment, f.usdc)))
	assert.Equal(t, "5", f.balance(f.cust(CodeWallet, f.usdc)))
}

// TestLedger_UpdateDeleteForbidden (invariant matrix): posted rows are
// immutable via the trigger (LG003) for the table owner, and the application
// role cannot mutate them either.
func TestLedger_UpdateDeleteForbidden(t *testing.T) {
	f := newFixture(t)
	res := f.fund("dep-imm", f.usdc, usdc100)
	stmts := []struct{ name, sql string }{
		{"update entry", `UPDATE journal_entries SET quantity = quantity + 1 WHERE transaction_id = $1`},
		{"delete entry", `DELETE FROM journal_entries WHERE transaction_id = $1`},
		{"update transaction", `UPDATE journal_transactions SET description = 'tampered' WHERE id = $1`},
		{"delete transaction", `DELETE FROM journal_transactions WHERE id = $1`},
	}
	t.Run("trigger raises LG003 for the table owner", func(t *testing.T) {
		for _, s := range stmts {
			_, err := testMigrate.Exec(f.ctx, s.sql, res.TransactionID)
			require.Error(t, err, s.name)
			assert.Equal(t, SQLStateImmutable, db.SQLState(err), s.name)
			requireCode(t, MapError(err), errs.CodeLedgerImmutable)
		}
	})
	t.Run("application role cannot mutate posted rows", func(t *testing.T) {
		for _, s := range stmts {
			_, err := testDB.Exec(f.ctx, s.sql, res.TransactionID)
			require.Error(t, err, s.name)
			code := errs.CodeOf(MapError(err))
			assert.Containsf(t, []errs.Code{errs.CodeLedgerImmutable, errs.CodeForbidden}, code, "%s: got %v", s.name, err)
		}
	})
	tx, err := f.svc.Transaction(f.ctx, testDB, res.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "funding settled", tx.Description)
	require.Len(t, tx.Entries, 2)
	for _, e := range tx.Entries {
		assert.Equal(t, "100000000", e.Quantity.String())
	}
	assert.Equal(t, "100000000", f.balance(f.cust(CodeWallet, f.usdc)))
}

// TestIntegration_AppRoleGrantsMatchFinancialModel pins FINANCIAL_MODEL §2.3
// and migration 00101's own comment: cp_app is insert-only on the journal
// tables and read-only on ledger_balances. It fails while the migration
// relies on default privileges instead of revoking them.
func TestIntegration_AppRoleGrantsMatchFinancialModel(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	const fix = `fix in migrations/00101_ledger.sql:
REVOKE UPDATE, DELETE, TRUNCATE ON journal_transactions, journal_entries FROM cp_app;
REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON ledger_balances FROM cp_app;`
	checks := []struct{ table, priv string }{
		{"journal_transactions", "UPDATE"},
		{"journal_transactions", "DELETE"},
		{"journal_entries", "UPDATE"},
		{"journal_entries", "DELETE"},
		{"ledger_balances", "INSERT"},
		{"ledger_balances", "UPDATE"},
		{"ledger_balances", "DELETE"},
	}
	for _, c := range checks {
		var has bool
		require.NoError(t, testDB.QueryRow(ctx, `SELECT has_table_privilege('cp_app', $1, $2)`, c.table, c.priv).Scan(&has))
		assert.Falsef(t, has, "cp_app holds %s on %s (default privileges granted at CREATE TABLE were never revoked); %s", c.priv, c.table, fix)
	}
	// Privilege checks fire at plan time, so WHERE false probes without mutating.
	_, err := testDB.Exec(ctx, `UPDATE ledger_balances SET balance = balance WHERE false`)
	require.Errorf(t, err, "cp_app must not be able to UPDATE ledger_balances; %s", fix)
	requireCode(t, MapError(err), errs.CodeForbidden)
}

func TestIntegration_Idempotent(t *testing.T) {
	f := newFixture(t)
	p, err := FundingSettledPosting(FundingInputs{AccountID: f.account, DepositID: uuid.NewString(), AssetID: f.usdc, Quantity: q(usdc100), EffectiveAt: f.clk.Now()})
	require.NoError(t, err)

	first := f.post(p)
	second := f.post(p)
	assert.False(t, first.Existing)
	assert.True(t, second.Existing)
	assert.Equal(t, first.TransactionID, second.TransactionID)
	assert.Equal(t, first.ContentHash, second.ContentHash)
	assert.Equal(t, 1, countByKey(t, p.IdempotencyKey))
	assert.Equal(t, "100000000", f.balance(f.cust(CodeWallet, f.usdc)), "one economic effect")

	// Same key, different content.
	changed := p
	changed.Entries = []Entry{
		{Account: p.Entries[0].Account, Side: Debit, Quantity: q(usdc100 + 1)},
		{Account: p.Entries[1].Account, Side: Credit, Quantity: q(usdc100 + 1)},
	}
	_, err = f.svc.PostInTx(f.ctx, testDB, changed)
	requireCode(t, err, errs.CodeInvalidIdempotencyReuse)
	e, _ := errs.As(err)
	assert.Equal(t, first.TransactionID.String(), e.Fields["existing_transaction_id"])
	assert.Equal(t, 1, countByKey(t, p.IdempotencyKey))
	assert.Equal(t, "100000000", f.balance(f.cust(CodeWallet, f.usdc)))

	// Reordered entries and different valuation are the same content.
	reordered := p
	usdv := int64(1)
	reordered.Entries = []Entry{p.Entries[1], p.Entries[0]}
	reordered.Entries[0].USDValueMinor = &usdv
	reordered.Description = "retry"
	again := f.post(reordered)
	assert.True(t, again.Existing)
	assert.Equal(t, first.TransactionID, again.TransactionID)

	// Concurrent identical postings: exactly one writes, the rest observe it.
	race, err := FundingSettledPosting(FundingInputs{AccountID: f.account, DepositID: uuid.NewString(), AssetID: f.usdc, Quantity: q(7), EffectiveAt: f.clk.Now()})
	require.NoError(t, err)
	const n = 12
	results := make(chan PostResult, n)
	errCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.svc.PostInTx(f.ctx, testDB, race)
			if err != nil {
				errCh <- err
				return
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	var fresh int
	var ids []TransactionID
	for r := range results {
		if !r.Existing {
			fresh++
		}
		ids = append(ids, r.TransactionID)
	}
	require.Len(t, ids, n)
	assert.Equal(t, 1, fresh, "exactly one goroutine posts")
	for _, id := range ids {
		assert.Equal(t, ids[0], id)
	}
	assert.Equal(t, 1, countByKey(t, race.IdempotencyKey))
	assert.Equal(t, "100000007", f.balance(f.cust(CodeWallet, f.usdc)))
}

// TestIntegration_VerifyBalances_DetectsTamper corrupts ledger_balances as
// the table owner and expects exactly that account to be reported.
func TestIntegration_VerifyBalances_DetectsTamper(t *testing.T) {
	f := newFixture(t)
	f.fund("dep-v", f.usdc, usdc100)
	wallet := f.cust(CodeWallet, f.usdc)
	acct, err := f.svc.Account(f.ctx, testDB, wallet)
	require.NoError(t, err)
	f.requireNoDrift()

	_, err = testMigrate.Exec(f.ctx, `UPDATE ledger_balances SET balance = balance + 1, entry_count = entry_count + 1 WHERE ledger_account_id = $1`, acct.ID)
	require.NoError(t, err, "the table owner can tamper; the app role must not (see TestIntegration_AppRoleGrantsMatchFinancialModel)")
	restore := func() {
		_, err := testMigrate.Exec(context.Background(), `UPDATE ledger_balances SET balance = balance - 1, entry_count = entry_count - 1 WHERE ledger_account_id = $1`, acct.ID)
		require.NoError(t, err)
	}
	t.Cleanup(restore)

	drifts, err := VerifyBalances(f.ctx, testDB)
	require.NoError(t, err)
	var found *Drift
	for i := range drifts {
		if drifts[i].LedgerAccountID == acct.ID {
			found = &drifts[i]
		}
	}
	require.NotNil(t, found, "tampered account must be reported")
	assert.Equal(t, wallet.normalized(), found.Account)
	assert.Equal(t, "100000001", found.StoredBalance.String())
	assert.Equal(t, "100000000", found.ComputedBalance.String())
	assert.Equal(t, int64(2), found.StoredEntryCount)
	assert.Equal(t, int64(1), found.ComputedEntryCount)

	restore()
	f.requireNoDrift()
}

// TestIntegration_ConcurrentPostingsOverlappingAccounts: 50 goroutines post
// SEED transfers between four wallets with entries in random order. Entries
// are locked in ledger_account_id order so no deadlock is expected; the
// retry path of db.InTx is nonetheless in place and attempts are counted.
func TestIntegration_ConcurrentPostingsOverlappingAccounts(t *testing.T) {
	f := newFixture(t)
	const customers, workers, seedBalance = 4, 50, int64(1_000_000_000)
	wallets := make([]AccountRef, customers)
	for i := range wallets {
		acct := createAccount(t)
		wallets[i] = CustomerAccount(acct, CodeWallet, f.usdc)
		p, err := FundingSettledPosting(FundingInputs{AccountID: acct, DepositID: uuid.NewString(), AssetID: f.usdc, Quantity: q(seedBalance), EffectiveAt: f.clk.Now()})
		require.NoError(t, err)
		f.post(p)
	}

	var mu sync.Mutex
	expected := make([]money.Quantity, customers)
	var attempts atomic.Int64
	runID := uuid.NewString()
	postings := make([]Posting, workers)
	for w := range postings {
		var entries []Entry
		for pair := 0; pair < 2; pair++ {
			a := rand.IntN(customers)
			b := (a + 1 + rand.IntN(customers-1)) % customers
			amt := int64(1 + rand.IntN(1000))
			entries = append(entries,
				Entry{Account: wallets[a], Side: Credit, Quantity: q(amt)},
				Entry{Account: wallets[b], Side: Debit, Quantity: q(amt)})
			expected[a] = expected[a].Sub(q(amt))
			expected[b] = expected[b].Add(q(amt))
		}
		rand.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
		key := fmt.Sprintf("conc:%s:%d", runID, w)
		postings[w] = Posting{Kind: KindSeed, IdempotencyKey: key, Reference: FinancialEventReference{Type: "seed", ID: key}, EffectiveAt: f.clk.Now(), Entries: entries}
	}

	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for _, p := range postings {
		wg.Add(1)
		go func(p Posting) {
			defer wg.Done()
			err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 10}, func(ctx context.Context, tx pgx.Tx) error {
				attempts.Add(1)
				_, err := f.svc.Post(ctx, tx, p)
				return err
			})
			errCh <- MapError(err)
		}(p)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	t.Logf("attempts=%d retries=%d", attempts.Load(), attempts.Load()-workers)
	require.GreaterOrEqual(t, attempts.Load(), int64(workers))

	mu.Lock()
	defer mu.Unlock()
	for i, w := range wallets {
		want := q(seedBalance).Add(expected[i])
		assert.Equalf(t, want.String(), f.balance(w), "wallet %d", i)
	}
	var n int
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM journal_transactions WHERE idempotency_key LIKE $1`, "conc:"+runID+":%").Scan(&n))
	assert.Equal(t, workers, n)
	drifts, err := VerifyBalances(f.ctx, testDB)
	require.NoError(t, err)
	for _, d := range drifts {
		for _, w := range wallets {
			assert.NotEqual(t, w.normalized(), d.Account, "drift on %+v", d)
		}
	}
}

// TestLedger_CorrectionIsNewTransaction (invariant matrix): a correction is
// a new COMPENSATION transaction referencing the original, which stays
// byte-for-byte intact.
func TestLedger_CorrectionIsNewTransaction(t *testing.T) {
	f := newFixture(t)
	orig := f.fund("dep-c", f.usdc, usdc100)
	before, err := f.svc.Transaction(f.ctx, testDB, orig.TransactionID)
	require.NoError(t, err)

	comp := Posting{
		Kind: KindCompensation, IdempotencyKey: "comp:" + uuid.NewString(),
		Reference:   FinancialEventReference{Type: "reconciliation_record", ID: uuid.NewString()},
		EffectiveAt: f.clk.Now(), ReversalOf: &orig.TransactionID,
		Metadata: map[string]any{MetadataReasonCode: "DUPLICATE_DEPOSIT", "note": "provider sent the settlement twice"},
		Entries: []Entry{
			{Account: f.cust(CodeCapital, f.usdc), Side: Debit, Quantity: q(usdc100)},
			{Account: f.cust(CodeWallet, f.usdc), Side: Credit, Quantity: q(usdc100)},
		},
	}
	res := f.post(comp)
	assert.NotEqual(t, orig.TransactionID, res.TransactionID)

	after, err := f.svc.Transaction(f.ctx, testDB, orig.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the original transaction is untouched")

	c, err := f.svc.Transaction(f.ctx, testDB, res.TransactionID)
	require.NoError(t, err)
	require.NotNil(t, c.ReversalOf)
	assert.Equal(t, orig.TransactionID, *c.ReversalOf)
	assert.Equal(t, KindCompensation, c.Kind)
	assert.Equal(t, "DUPLICATE_DEPOSIT", c.ReasonCode)
	assert.Equal(t, "provider sent the settlement twice", c.Metadata["note"])
	assert.Equal(t, "0", f.balance(f.cust(CodeWallet, f.usdc)))
	assert.Equal(t, "0", f.balance(f.cust(CodeCapital, f.usdc)))

	bogus := NewTransactionID()
	comp.IdempotencyKey = "comp:" + uuid.NewString()
	comp.ReversalOf = &bogus
	_, err = f.svc.PostInTx(f.ctx, testDB, comp)
	requireCode(t, err, errs.CodeValidationFailed)
	assert.Equal(t, 0, countByKey(t, comp.IdempotencyKey))
}

func TestIntegration_ListTransactions(t *testing.T) {
	f := newFixture(t)
	start := f.clk.Now()
	var ids []TransactionID
	for i := 0; i < 5; i++ {
		f.clk.Set(start.Add(time.Duration(i) * time.Second))
		ids = append(ids, f.fund(fmt.Sprintf("dep-list-%d", i), f.usdc, oneUSDC).TransactionID)
	}
	owner := TransactionFilter{OwnerType: OwnerCustomer, OwnerID: f.account.String()}

	var got []TransactionID
	cursor := ""
	pages := 0
	for {
		page, next, err := f.svc.ListTransactions(f.ctx, testDB, owner, cursor, 2)
		require.NoError(t, err)
		pages++
		for _, tx := range page {
			got = append(got, tx.ID)
			assert.Len(t, tx.Entries, 2)
			assert.Equal(t, KindFundingSettled, tx.Kind)
		}
		if next == "" {
			break
		}
		cursor = next
		require.Less(t, pages, 10, "cursor never terminates")
	}
	assert.Equal(t, ids, got, "posted_at, id order across pages")
	assert.Equal(t, 3, pages)

	page, next, err := f.svc.ListTransactions(f.ctx, testDB, TransactionFilter{OwnerType: OwnerCustomer, OwnerID: f.account.String(), Kind: KindSeed}, "", 0)
	require.NoError(t, err)
	assert.Empty(t, page)
	assert.Empty(t, next)

	page, _, err = f.svc.ListTransactions(f.ctx, testDB, TransactionFilter{AssetID: f.sol}, "", 0)
	require.NoError(t, err)
	assert.Empty(t, page)
	page, _, err = f.svc.ListTransactions(f.ctx, testDB, TransactionFilter{AssetID: f.usdc}, "", 0)
	require.NoError(t, err)
	assert.Len(t, page, 5)

	page, _, err = f.svc.ListTransactions(f.ctx, testDB, TransactionFilter{OwnerID: f.account.String(), Since: start.Add(2 * time.Second)}, "", 0)
	require.NoError(t, err)
	assert.Equal(t, ids[2:], txIDs(page))
	page, _, err = f.svc.ListTransactions(f.ctx, testDB, TransactionFilter{OwnerID: f.account.String(), Until: start.Add(2 * time.Second)}, "", 0)
	require.NoError(t, err)
	assert.Equal(t, ids[:2], txIDs(page))

	_, _, err = f.svc.ListTransactions(f.ctx, testDB, owner, "garbage", 0)
	requireCode(t, err, errs.CodeValidationFailed)
	_, _, err = f.svc.ListTransactions(f.ctx, testDB, TransactionFilter{Kind: "NOPE"}, "", 0)
	requireCode(t, err, errs.CodeValidationFailed)
	_, _, err = f.svc.ListTransactions(f.ctx, testDB, TransactionFilter{OwnerID: "nope"}, "", 0)
	requireCode(t, err, errs.CodeValidationFailed)
	_, _, err = f.svc.ListTransactions(f.ctx, testDB, TransactionFilter{Since: start, Until: start}, "", 0)
	requireCode(t, err, errs.CodeValidationFailed)

	balances, err := f.svc.BalancesForOwner(f.ctx, testDB, OwnerCustomer, f.account.String())
	require.NoError(t, err)
	require.Len(t, balances, 2)
	assert.Equal(t, CodeCapital, balances[0].Account.Code)
	assert.Equal(t, "5000000", balances[0].Balance.String())
	assert.Equal(t, CodeWallet, balances[1].Account.Code)
	assert.Equal(t, "5000000", balances[1].Balance.String())
	assert.Equal(t, int64(5), balances[1].EntryCount)

	_, err = f.svc.Transaction(f.ctx, testDB, NewTransactionID())
	requireCode(t, err, errs.CodeNotFound)
}

func txIDs(txs []Transaction) []TransactionID {
	out := make([]TransactionID, len(txs))
	for i, tx := range txs {
		out[i] = tx.ID
	}
	return out
}

// TestIntegration_AssetMismatchAndClosedAccount covers LG004 and LG005.
func TestIntegration_AssetMismatchAndClosedAccount(t *testing.T) {
	f := newFixture(t)
	wallet, capital := f.cust(CodeWallet, f.usdc), f.cust(CodeCapital, f.usdc)

	err := testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		w, err := f.svc.EnsureAccount(ctx, tx, wallet)
		if err != nil {
			return err
		}
		txID := NewTransactionID()
		if err := rawHeader(ctx, tx, txID, "raw:"+uuid.NewString()); err != nil {
			return err
		}
		return rawEntry(ctx, tx, txID, 0, w.ID, f.sol, Debit, 1) // SOL entry on a USDC account
	})
	requireCode(t, MapError(err), errs.CodeLedgerAssetMismatch)

	f.fund("dep-close", f.usdc, usdc100)
	acct, err := f.svc.Account(f.ctx, testDB, wallet)
	require.NoError(t, err)
	_, err = testDB.Exec(f.ctx, `UPDATE ledger_accounts SET status = 'CLOSED' WHERE id = $1`, acct.ID)
	require.NoError(t, err)

	p := f.seedTransfer("closed:"+uuid.NewString(), wallet, f.cust(CodeTradingOutflow, f.usdc), 1)
	_, err = f.svc.PostInTx(f.ctx, testDB, p)
	requireCode(t, err, errs.CodeLedgerAccountClosed)
	assert.Equal(t, 0, countByKey(t, p.IdempotencyKey))

	err = testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		c, err := f.svc.EnsureAccount(ctx, tx, capital)
		if err != nil {
			return err
		}
		txID := NewTransactionID()
		if err := rawHeader(ctx, tx, txID, "raw:"+uuid.NewString()); err != nil {
			return err
		}
		if err := rawEntry(ctx, tx, txID, 0, c.ID, f.usdc, Debit, 1); err != nil {
			return err
		}
		return rawEntry(ctx, tx, txID, 1, acct.ID, f.usdc, Credit, 1)
	})
	requireCode(t, MapError(err), errs.CodeLedgerAccountClosed)
	assert.Equal(t, "100000000", f.balance(wallet))
}

func TestIntegration_PostedByActorAndAccounts(t *testing.T) {
	f := newFixture(t)
	op := security.Principal{SubjectID: "op-1", ActorType: security.ActorOperator, Roles: []security.Role{security.RoleFinance}}
	ctx := security.WithPrincipal(f.ctx, op)
	p, err := FundingSettledPosting(FundingInputs{AccountID: f.account, DepositID: uuid.NewString(), AssetID: f.usdc, Quantity: q(1), EffectiveAt: f.clk.Now()})
	require.NoError(t, err)
	res, err := f.svc.PostInTx(ctx, testDB, p)
	require.NoError(t, err)
	tx, err := f.svc.Transaction(f.ctx, testDB, res.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, security.ActorOperator, tx.PostedByActorType)
	assert.Equal(t, "op-1", tx.PostedByActorID)
	assert.Equal(t, f.clk.Now(), tx.PostedAt)
	assert.Equal(t, p.EffectiveAt, tx.EffectiveAt)
	assert.Equal(t, "itest", tx.BuildVersion)

	agentCtx := security.WithPrincipal(f.ctx, security.AgentPrincipal("agent-1", f.account.String()))
	p2, err := FundingSettledPosting(FundingInputs{AccountID: f.account, DepositID: uuid.NewString(), AssetID: f.usdc, Quantity: q(1), EffectiveAt: f.clk.Now()})
	require.NoError(t, err)
	_, err = f.svc.PostInTx(agentCtx, testDB, p2)
	requireCode(t, err, errs.CodeForbidden)
	assert.Equal(t, 0, countByKey(t, p2.IdempotencyKey))

	// Accounts: zero balance before any posting, NOT_FOUND until ensured,
	// idempotent creation, and drift detection against the chart.
	untouched := f.cust(CodeFeesVenue, f.sol)
	assert.Equal(t, "0", f.balance(untouched))
	_, err = f.svc.Account(f.ctx, testDB, untouched)
	requireCode(t, err, errs.CodeNotFound)
	a1, err := f.svc.EnsureAccount(f.ctx, testDB, untouched)
	require.NoError(t, err)
	a2, err := f.svc.EnsureAccount(f.ctx, testDB, untouched)
	require.NoError(t, err)
	assert.Equal(t, a1.ID, a2.ID)
	assert.Equal(t, Debit, a1.NormalSide)
	assert.False(t, a1.AllowNegative)
	assert.Equal(t, AccountOpen, a1.Status)

	_, err = testMigrate.Exec(f.ctx, `UPDATE ledger_accounts SET allow_negative = true WHERE id = $1`, a1.ID)
	require.NoError(t, err)
	_, err = f.svc.EnsureAccount(f.ctx, testDB, untouched)
	requireCode(t, err, errs.CodeInternal, "a stored definition that disagrees with the chart is refused")
	_, err = testMigrate.Exec(f.ctx, `UPDATE ledger_accounts SET allow_negative = false WHERE id = $1`, a1.ID)
	require.NoError(t, err)
}
