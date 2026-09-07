//go:build integration

package ledger

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The Go checks in internal/valuedomain run first and produce the operator
// facing errors. These tests are about the other half: what PostgreSQL refuses
// when the Go half is bypassed. Every test here therefore posts through the
// migration role's pool with hand-written SQL, exactly as a compromised
// service or a hand-run statement would, and asserts on the SQLSTATE.

// staticCaps is a CapabilityResolver that reports a fixed set as ACTIVE.
type staticCaps map[valuedomain.CapabilityKey]bool

func (s staticCaps) ActiveConversionCapabilities(context.Context) (map[valuedomain.CapabilityKey]bool, error) {
	return s, nil
}

// creditAssetOnce provisions THE Credit asset for the whole suite. Migration
// 00711 permits exactly one, which is the point, so it cannot be per-test --
// and an earlier version of this file created one per call, which passed on a
// fresh database and failed on the second run of the same one.
var (
	creditAssetOnce sync.Once
	creditAssetID   assets.AssetID
)

func creditAsset(t *testing.T) assets.AssetID {
	t.Helper()
	creditAssetOnce.Do(func() {
		ctx := context.Background()
		var existing assets.AssetID
		if err := testDB.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing); err == nil {
			creditAssetID = existing
			return
		}
		creditAssetID = createInternalAsset(t, assets.KindCredit, valuedomain.InternalCredit, "CREDIT")
	})
	require.False(t, creditAssetID.IsZero())
	return creditAssetID
}

func createInternalAsset(t *testing.T, kind assets.Kind, domain valuedomain.Domain, symbol string) assets.AssetID {
	t.Helper()
	created, err := assets.NewRepository().Create(context.Background(), testDB, assets.Asset{
		Chain:       assets.InternalChain,
		Kind:        kind,
		ValueDomain: domain,
		Symbol:      symbol + "-" + uuid.NewString()[:8],
		Name:        symbol,
		Decimals:    6,
		RiskClass:   assets.RiskSpeculative,
		Status:      assets.StatusActive,
	})
	require.NoError(t, err)
	return created.ID
}

// rawEntryRow and rawTx describe a journal transaction written by hand.
type rawEntryRow struct {
	acct  LedgerAccountID
	asset assets.AssetID
	side  string
	qty   int64
}

type rawTx struct {
	kind     string
	convFrom any
	convTo   any
	entries  []rawEntryRow
}

// rawPosting writes a journal transaction through the MIGRATION role, which
// owns every table in the schema. It is deliberately the most privileged path
// available: whatever it cannot do, nothing in the system can do.
func rawPosting(t *testing.T, in rawTx) error {
	t.Helper()
	ctx := context.Background()
	tx, err := testMigrate.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	txID := NewTransactionID()
	key := "raw-" + uuid.NewString()
	if _, err := tx.Exec(ctx,
		`INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id,
		     effective_at, posted_by_actor_type, posted_by_actor_id, content_hash, conversion_from, conversion_to)
		 VALUES ($1, $2, $3, 'test', $3, now(), 'SYSTEM', 'itest', $4, $5, $6)`,
		txID, in.kind, key, []byte{0}, in.convFrom, in.convTo); err != nil {
		return err
	}
	for i, e := range in.entries {
		if _, err := tx.Exec(ctx,
			`INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity)
			 VALUES ($1, $2, $3, $4, $5, $6, $7::numeric)`,
			NewJournalEntryID(), txID, i, e.acct, e.asset, e.side, money.QuantityFromInt64(e.qty).String()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// TestIntegration_AssetsCarryTheirValueDomain proves the registry round-trips
// the classification and refuses ones that contradict the kind.
func TestIntegration_AssetsCarryTheirValueDomain(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	repo := assets.NewRepository()

	creditID := creditAsset(t)
	got, err := repo.Get(ctx, testDB, creditID)
	require.NoError(t, err)
	require.Equal(t, valuedomain.InternalCredit, got.ValueDomain)
	require.Equal(t, assets.InternalChain, got.Chain)
	require.Equal(t, got.ID.String(), got.MintAddress,
		"an internal asset's mint address is its own id; it has no mint")

	// A kind and a domain that disagree must be refused before the database
	// is reached, and by the database if it is.
	_, err = repo.Create(ctx, testDB, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindCredit,
		ValueDomain: valuedomain.HostedFiat,
		Symbol:      "BAD", Name: "Bad", Decimals: 6,
		RiskClass: assets.RiskSpeculative, Status: assets.StatusActive,
	})
	require.Error(t, err, "a CREDIT asset claiming to be HOSTED_FIAT must be refused")

	// A chain asset with no declared domain is refused: custody is a decision.
	_, err = repo.Create(ctx, testDB, assets.Asset{
		Chain: "solana-itest-" + uuid.NewString(), MintAddress: uuid.NewString(),
		Kind: assets.KindSPLToken, Symbol: "NOD", Name: "No Domain", Decimals: 6,
		RiskClass: assets.RiskStandard, Status: assets.StatusActive,
	})
	require.Error(t, err)
	require.Contains(t, assetProblems(t, err), "chain assets must declare a value domain",
		"the refusal must say why, so an operator knows what to supply")
}

// assetProblems renders the problems field assets.Validate attaches.
func assetProblems(t *testing.T, err error) string {
	t.Helper()
	ee, ok := errs.As(err)
	require.True(t, ok, "expected an *errs.Error, got %T", err)
	return fmt.Sprint(ee.Fields["problems"])
}

// seed gives an account a starting balance so that a later test posting fails
// for the reason under test rather than for want of funds: the per-entry
// negative-balance trigger fires before the deferred domain check, so an
// unfunded probe would prove nothing about isolation.
func (f *fixture) seed(code Code, asset assets.AssetID, qty int64) {
	f.t.Helper()
	offset := CodeCreditIssuance
	if code == CodeWallet {
		offset = CodeCapital
	}
	f.post(Posting{
		Kind: KindSeed, IdempotencyKey: "vd-seed-" + uuid.NewString(),
		Reference:   FinancialEventReference{Type: "seed", ID: uuid.NewString()},
		EffectiveAt: f.clk.Now(),
		Entries: []Entry{
			{Account: f.cust(code, asset), Side: Debit, Quantity: money.QuantityFromInt64(qty)},
			{Account: f.cust(offset, asset), Side: Credit, Quantity: money.QuantityFromInt64(qty)},
		},
	})
}

// seedPlatform is seed for a PLATFORM-owned account.
func (f *fixture) seedPlatform(code Code, asset assets.AssetID, qty int64) {
	f.t.Helper()
	f.post(Posting{
		Kind: KindSeed, IdempotencyKey: "vd-seedp-" + uuid.NewString(),
		Reference:   FinancialEventReference{Type: "seed", ID: uuid.NewString()},
		EffectiveAt: f.clk.Now(),
		Entries: []Entry{
			{Account: PlatformAccount(code, asset), Side: Debit, Quantity: money.QuantityFromInt64(qty)},
			{Account: PlatformAccount(CodePlatformAdjustment, asset), Side: Credit, Quantity: money.QuantityFromInt64(qty)},
		},
	})
}

// TestIntegration_LedgerAccountInheritsDomainFromAssetAndCode proves the
// derivation rule, including the payout override that makes reserving a payout
// a cross-domain movement in the first place.
func TestIntegration_LedgerAccountInheritsDomainFromAssetAndCode(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	credit := creditAsset(t)

	balance, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeCreditBalance, credit))
	require.NoError(t, err)
	require.Equal(t, valuedomain.InternalCredit, balance.Domain)

	reserved, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodePayoutReserved, credit))
	require.NoError(t, err)
	require.Equal(t, valuedomain.PayoutPending, reserved.Domain,
		"PAYOUT_RESERVED holds the same asset in a different domain; that is what the payout gate bites on")

	settled, err := f.svc.EnsureAccount(f.ctx, testDB, PlatformAccount(CodePayoutSettled, credit))
	require.NoError(t, err)
	require.Equal(t, valuedomain.ExternalSettled, settled.Domain)

	wallet, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeWallet, f.sol))
	require.NoError(t, err)
	require.Equal(t, valuedomain.SelfCustodialCrypto, wallet.Domain)
}

// TestIntegration_TheDatabaseRefusesCreditsReachingRealCapital is acceptance
// test VAL-001 at the layer that actually holds the money.
//
// It writes the journal by hand through the migration role — the most
// privileged credential in the system, which owns every table and bypasses
// every application check — and the posting is still refused.
func TestIntegration_TheDatabaseRefusesCreditsReachingRealCapital(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	credit := creditAsset(t)

	f.seed(CodeCreditBalance, credit, 10_000)
	f.seed(CodeWallet, f.sol, 10_000)

	creditAcct, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeCreditBalance, credit))
	require.NoError(t, err)
	creditSink, err := f.svc.EnsureAccount(f.ctx, testDB, PlatformAccount(CodeMarketReserve, credit))
	require.NoError(t, err)
	solAcct, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeWallet, f.sol))
	require.NoError(t, err)
	solSource, err := f.svc.EnsureAccount(f.ctx, testDB, PlatformAccount(CodePlatformAdjustment, f.sol))
	require.NoError(t, err)

	// The shape a real "Credits buy you SOL" implementation would have: the
	// user's Credits move to a platform account, and SOL appears in their
	// wallet. It balances per asset, so the ledger's own balance triggers have
	// no objection. The only thing standing between this transaction and a
	// committed row is the value-domain rule.
	swap := []rawEntryRow{
		{acct: creditAcct.ID, asset: credit, side: "CREDIT", qty: 1000},
		{acct: creditSink.ID, asset: credit, side: "DEBIT", qty: 1000},
		{acct: solAcct.ID, asset: f.sol, side: "DEBIT", qty: 1000},
		{acct: solSource.ID, asset: f.sol, side: "CREDIT", qty: 1000},
	}

	for _, tc := range []struct {
		name     string
		convFrom any
		convTo   any
	}{
		{"undeclared", nil, nil},
		{"declared anyway", "INTERNAL_CREDIT", "SELF_CUSTODIAL_CRYPTO"},
		{"declared backwards", "SELF_CUSTODIAL_CRYPTO", "INTERNAL_CREDIT"},
		{"disguised as a native trade", "INTERNAL_CREDIT", "INTERNAL_NATIVE_ASSET"},
		{"disguised as a payout", "INTERNAL_CREDIT", "PAYOUT_PENDING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := rawPosting(t, rawTx{
				kind: "NATIVE_TRADE", convFrom: tc.convFrom, convTo: tc.convTo, entries: swap,
			})
			require.Error(t, err, "the database must refuse Credits and self-custodial crypto in one transaction")
			require.Contains(t, err.Error(), "VALUE_DOMAIN_FORBIDDEN",
				"the structural rule must fire before any declaration is even considered")
			require.Contains(t, err.Error(), "may never move together")
		})
	}

	// The same probe against partner-held crypto, which is not self-custodial
	// and so exercises the closed-loop rule itself rather than the
	// on-chain-authority rule that shadows it above.
	t.Run("credits to hosted crypto", func(t *testing.T) {
		hosted, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
			Chain: "solana-itest-" + uuid.NewString(), MintAddress: uuid.NewString(),
			Kind: assets.KindSPLToken, ValueDomain: valuedomain.HostedCrypto,
			Symbol: "HSOL", Name: "Hosted SOL", Decimals: 9,
			RiskClass: assets.RiskMajor, Status: assets.StatusActive,
		})
		require.NoError(t, err)
		f.seed(CodeWallet, hosted.ID, 10_000)
		hostedAcct, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeWallet, hosted.ID))
		require.NoError(t, err)
		hostedSource, err := f.svc.EnsureAccount(f.ctx, testDB, PlatformAccount(CodePlatformAdjustment, hosted.ID))
		require.NoError(t, err)

		err = rawPosting(t, rawTx{
			kind: "NATIVE_TRADE", convFrom: "INTERNAL_CREDIT", convTo: "HOSTED_CRYPTO",
			entries: []rawEntryRow{
				{acct: creditAcct.ID, asset: credit, side: "CREDIT", qty: 1000},
				{acct: creditSink.ID, asset: credit, side: "DEBIT", qty: 1000},
				{acct: hostedAcct.ID, asset: hosted.ID, side: "DEBIT", qty: 1000},
				{acct: hostedSource.ID, asset: hosted.ID, side: "CREDIT", qty: 1000},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "closed-loop",
			"the only route from Credits to external value is the gated payout path")
	})

	// The negative control: the same four entries, all within one domain,
	// commit. Without this the test could be passing because the posting is
	// malformed rather than because isolation works.
	other := creditAsset(t)
	f.seed(CodeCreditBalance, other, 10_000)
	otherAcct, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeCreditBalance, other))
	require.NoError(t, err)
	otherSink, err := f.svc.EnsureAccount(f.ctx, testDB, PlatformAccount(CodeMarketReserve, other))
	require.NoError(t, err)
	require.NoError(t, rawPosting(t, rawTx{
		kind: "CREDIT_SPENT",
		entries: []rawEntryRow{
			{acct: otherAcct.ID, asset: other, side: "CREDIT", qty: 1000},
			{acct: otherSink.ID, asset: other, side: "DEBIT", qty: 1000},
		},
	}))
}

// TestIntegration_CrossDomainPostingMustDeclareItself proves the declaration
// requirement in SQL, and that a mismatched declaration is caught.
func TestIntegration_CrossDomainPostingMustDeclareItself(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	credit := creditAsset(t)
	doggu := createInternalAsset(t, assets.KindNativeAsset, valuedomain.InternalNativeAsset, "DOGGU")

	f.seed(CodeCreditBalance, credit, 10_000)
	f.seedPlatform(CodeMarketInventory, doggu, 1_000_000)

	userCredit, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeCreditBalance, credit))
	require.NoError(t, err)
	marketReserve, err := f.svc.EnsureAccount(f.ctx, testDB, PlatformAccount(CodeMarketReserve, credit))
	require.NoError(t, err)
	userAsset, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeNativeAssetBalance, doggu))
	require.NoError(t, err)
	marketInventory, err := f.svc.EnsureAccount(f.ctx, testDB, PlatformAccount(CodeMarketInventory, doggu))
	require.NoError(t, err)

	entries := []rawEntryRow{
		{acct: userCredit.ID, asset: credit, side: "CREDIT", qty: 100},
		{acct: marketReserve.ID, asset: credit, side: "DEBIT", qty: 100},
		{acct: marketInventory.ID, asset: doggu, side: "CREDIT", qty: 5000},
		{acct: userAsset.ID, asset: doggu, side: "DEBIT", qty: 5000},
	}

	t.Run("undeclared is refused", func(t *testing.T) {
		err := rawPosting(t, rawTx{kind: "NATIVE_TRADE", entries: entries})
		require.Error(t, err)
		require.Contains(t, err.Error(), "declares no conversion")
	})

	t.Run("a declaration naming other domains is refused", func(t *testing.T) {
		err := rawPosting(t, rawTx{
			kind: "NATIVE_TRADE", convFrom: "INTERNAL_CREDIT", convTo: "PAYOUT_PENDING",
			entries: entries,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "VALUE_DOMAIN_DECLARATION")
	})

	t.Run("the correct declaration commits", func(t *testing.T) {
		require.NoError(t, rawPosting(t, rawTx{
			kind: "NATIVE_TRADE", convFrom: "INTERNAL_CREDIT", convTo: "INTERNAL_NATIVE_ASSET",
			entries: entries,
		}))
	})

	t.Run("a single-domain posting claiming a conversion is refused", func(t *testing.T) {
		issuance, err := f.svc.EnsureAccount(f.ctx, testDB, f.cust(CodeCreditIssuance, credit))
		require.NoError(t, err)
		err = rawPosting(t, rawTx{
			kind: "CREDIT_ISSUED", convFrom: "INTERNAL_CREDIT", convTo: "PAYOUT_PENDING",
			entries: []rawEntryRow{
				{acct: userCredit.ID, asset: credit, side: "DEBIT", qty: 50},
				{acct: issuance.ID, asset: credit, side: "CREDIT", qty: 50},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "touches only")
	})
}

// TestIntegration_ServiceEnforcesCapabilityOnCrossDomainPostings covers the
// half the database deliberately does not: capability activation.
func TestIntegration_ServiceEnforcesCapabilityOnCrossDomainPostings(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	credit := creditAsset(t)
	doggu := createInternalAsset(t, assets.KindNativeAsset, valuedomain.InternalNativeAsset, "DOGGU")

	buy := valuedomain.ConversionKey{From: valuedomain.InternalCredit, To: valuedomain.InternalNativeAsset}
	posting := func(key string) Posting {
		return Posting{
			Kind: KindNativeTrade, IdempotencyKey: key,
			Reference:   FinancialEventReference{Type: "native_fill", ID: uuid.NewString()},
			EffectiveAt: f.clk.Now(), Conversion: &buy,
			Entries: []Entry{
				{Account: f.cust(CodeCreditBalance, credit), Side: Credit, Quantity: q(100)},
				{Account: PlatformAccount(CodeMarketReserve, credit), Side: Debit, Quantity: q(100)},
				{Account: PlatformAccount(CodeMarketInventory, doggu), Side: Credit, Quantity: q(5000)},
				{Account: f.cust(CodeNativeAssetBalance, doggu), Side: Debit, Quantity: q(5000)},
			},
		}
	}

	// Seed the customer with Credits and the market with inventory so the buy
	// does not fail for want of balance rather than for want of a capability.
	f.post(Posting{
		Kind: KindSeed, IdempotencyKey: "vd-seed-credit-" + uuid.NewString(),
		Reference:   FinancialEventReference{Type: "seed", ID: uuid.NewString()},
		EffectiveAt: f.clk.Now(),
		Entries: []Entry{
			{Account: f.cust(CodeCreditBalance, credit), Side: Debit, Quantity: q(1000)},
			{Account: f.cust(CodeCreditIssuance, credit), Side: Credit, Quantity: q(1000)},
		},
	})
	f.post(Posting{
		Kind: KindSeed, IdempotencyKey: "vd-seed-inv-" + uuid.NewString(),
		Reference:   FinancialEventReference{Type: "seed", ID: uuid.NewString()},
		EffectiveAt: f.clk.Now(),
		Entries: []Entry{
			{Account: PlatformAccount(CodeMarketInventory, doggu), Side: Debit, Quantity: q(100000)},
			{Account: PlatformAccount(CodePlatformAdjustment, doggu), Side: Credit, Quantity: q(100000)},
		},
	})

	t.Run("no resolver means no capability", func(t *testing.T) {
		_, err := f.svc.PostInTx(f.ctx, testDB, posting("vd-cap-none-"+uuid.NewString()))
		require.Error(t, err)
		require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))
	})

	t.Run("the wrong capability does not help", func(t *testing.T) {
		f.svc.SetCapabilityResolver(staticCaps{valuedomain.CapPayoutSettle: true})
		defer f.svc.SetCapabilityResolver(nil)
		_, err := f.svc.PostInTx(f.ctx, testDB, posting("vd-cap-wrong-"+uuid.NewString()))
		require.Error(t, err)
		require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))
	})

	t.Run("the right capability commits", func(t *testing.T) {
		f.svc.SetCapabilityResolver(staticCaps{valuedomain.CapNativeMarketTrading: true})
		defer f.svc.SetCapabilityResolver(nil)
		res, err := f.svc.PostInTx(f.ctx, testDB, posting("vd-cap-right-"+uuid.NewString()))
		require.NoError(t, err)
		require.False(t, res.Existing)
	})
}

// TestIntegration_DeclaredConversionIsPartOfTheIdempotencyIdentity proves that
// replaying a key while claiming a different conversion is a reuse error, not
// a silent no-op that hands back the original transaction.
func TestIntegration_DeclaredConversionIsPartOfTheIdempotencyIdentity(t *testing.T) {
	requireEnv(t)
	f := newFixture(t)
	credit := creditAsset(t)

	key := "vd-idem-" + uuid.NewString()
	base := Posting{
		Kind: KindCreditIssued, IdempotencyKey: key,
		Reference:   FinancialEventReference{Type: "credit_grant", ID: uuid.NewString()},
		EffectiveAt: f.clk.Now(),
		Entries: []Entry{
			{Account: f.cust(CodeCreditBalance, credit), Side: Debit, Quantity: q(500)},
			{Account: f.cust(CodeCreditIssuance, credit), Side: Credit, Quantity: q(500)},
		},
	}
	first, err := f.svc.PostInTx(f.ctx, testDB, base)
	require.NoError(t, err)

	replay, err := f.svc.PostInTx(f.ctx, testDB, base)
	require.NoError(t, err)
	require.True(t, replay.Existing)
	require.Equal(t, first.TransactionID, replay.TransactionID)

	// Same key, same entries, but now claiming to be a payout reservation.
	lie := base
	lie.Conversion = &valuedomain.ConversionKey{From: valuedomain.InternalCredit, To: valuedomain.PayoutPending}
	_, err = f.svc.PostInTx(f.ctx, testDB, lie)
	require.Error(t, err)
	require.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))
}

// TestIntegration_GoAndSQLAgreeOnEveryOrderedDomainPair is the test that keeps
// the two halves of the rule from drifting. It asks PostgreSQL the same
// question internal/valuedomain answers, for all 64 ordered pairs.
func TestIntegration_GoAndSQLAgreeOnEveryOrderedDomainPair(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	for _, a := range valuedomain.AllDomains() {
		for _, b := range valuedomain.AllDomains() {
			var sqlWhy *string
			require.NoError(t, testMigrate.QueryRow(ctx,
				`SELECT cp_domains_structurally_forbidden($1, $2)`, string(a), string(b)).Scan(&sqlWhy))
			goForbidden, goWhy := valuedomain.StructurallyForbidden(a, b)

			require.Equal(t, goForbidden, sqlWhy != nil,
				"Go and SQL disagree on whether %s/%s is structurally forbidden", a, b)
			if goForbidden {
				require.Equal(t, goWhy, *sqlWhy,
					"Go and SQL give different reasons for %s/%s", a, b)
			}

			var sqlDeclared bool
			require.NoError(t, testMigrate.QueryRow(ctx,
				`SELECT cp_conversion_is_declared($1, $2)`, string(a), string(b)).Scan(&sqlDeclared))
			_, goDeclared := valuedomain.LookupConversion(a, b)
			require.Equal(t, goDeclared, sqlDeclared,
				"Go and SQL disagree on whether %s->%s is a declared conversion", a, b)
		}
	}
}

// TestIntegration_AccountDomainDerivationAgreesWithGo checks the other shared
// rule: which code overrides its asset's domain.
func TestIntegration_AccountDomainDerivationAgreesWithGo(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	for _, code := range AllCodes() {
		for _, assetDomain := range []valuedomain.Domain{
			valuedomain.InternalCredit, valuedomain.InternalNativeAsset, valuedomain.SelfCustodialCrypto,
		} {
			var got string
			require.NoError(t, testMigrate.QueryRow(ctx,
				`SELECT cp_account_value_domain($1, $2)`, string(assetDomain), string(code)).Scan(&got))

			want := string(assetDomain)
			switch code {
			case CodePayoutReserved, CodePayoutClearing:
				want = string(valuedomain.PayoutPending)
			case CodePayoutSettled:
				want = string(valuedomain.ExternalSettled)
			}
			require.Equal(t, want, got, "code %s on a %s asset", code, assetDomain)
		}
	}
}
