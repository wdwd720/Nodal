//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/quote"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/settlement/settlementtest"
)

var testAppURL = os.Getenv("CP_TEST_DATABASE_URL")

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	if testAppURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; provision one with `go run ./scripts/testdb -name workers -export`")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "execution-worker-itest", MaxConns: 16})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

// world is a settlementtest.World whose account, assets, instrument, venue
// listing, wallet and intent all exist in the isolated database, wired to the
// same production repositories cmd/execution-worker wires in main.go. The only
// fakes are the ones no build can supply yet: the venue adapter, the chain,
// the inspector, the signer and the recoverer.
type world struct {
	*settlementtest.World
	d *db.DB

	plans    *settlement.PlanRepository
	orders   *execution.Repository
	attempts *execution.AttemptRepository
	capital  *capital.Service
	lots     *positions.Engine
	intents  *intent.Repository
	audit    audit.Writer
	outbox   *event.Outbox
}

func newWorld(t *testing.T, d *db.DB) *world {
	t.Helper()
	ctx := t.Context()
	w := settlementtest.NewWorld()
	arepo := accounts.NewRepository()
	u, err := arepo.CreateUser(ctx, d, "workers-itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	a, err := arepo.CreateAccount(ctx, d, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	w.AccountID = a.ID

	suffix := id.New[id.Any]().String()
	suffix = suffix[len(suffix)-12:]
	usdc := w.USDC
	usdc.ID, usdc.MintAddress = assets.AssetID{}, "usdc-"+suffix
	usdc, err = assets.NewRepository().Create(ctx, d, usdc)
	require.NoError(t, err)
	sol := w.SOL
	sol.ID, sol.MintAddress, sol.Kind = assets.AssetID{}, "sol-"+suffix, assets.KindSPLToken
	sol, err = assets.NewRepository().Create(ctx, d, sol)
	require.NoError(t, err)
	w.USDC, w.SOL = usdc, sol
	w.Adapter.NetworkFeeAsset = sol.ID

	irepo := instruments.NewRepository()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		inst, err := irepo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: sol.ID, Quote: usdc.ID, Settlement: usdc.ID, CanonicalName: "SOL/USDC-" + suffix, RiskClass: assets.RiskMajor,
			Status: assets.StatusActive, ActiveFrom: w.Clock.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		w.Instrument = inst
		venue, err := irepo.CreateVenue(ctx, tx, instruments.Venue{
			Code: "JUP-" + suffix, Name: "Jupiter", Kind: instruments.VenueDEXAggregator, Chain: "solana-devnet", Status: instruments.VenueActive,
		})
		if err != nil {
			return err
		}
		w.Venue = venue
		listing, err := irepo.CreateListing(ctx, tx, instruments.VenueListing{
			VenueID: venue.ID, InstrumentID: inst.ID, VenueNativeID: "SOL-USDC", Network: "solana-devnet",
			BaseMint: sol.MintAddress, QuoteMint: usdc.MintAddress, BasePrecision: 9, QuotePrecision: 6,
			MinNotionalQuote: money.QuantityFromInt64(1_000_000), Status: instruments.VenueActive,
		})
		if err != nil {
			return err
		}
		w.Listing = listing
		return nil
	}))

	w.WalletAddress = "wallet-" + suffix
	w.Chain.SetBalance(w.WalletAddress, usdc.MintAddress, money.QuantityFromInt64(1_000_000_000))
	w.Chain.SetBalance(w.WalletAddress, sol.MintAddress, money.QuantityFromInt64(5_000_000_000))
	_, err = d.Exec(ctx, `INSERT INTO wallets (id, account_id, provider, provider_wallet_id, chain, address, kind, status)
		VALUES ($1,$2,'privy',$3,'solana-devnet',$4,'EMBEDDED_DELEGATED','ACTIVE')`,
		w.WalletID, w.AccountID, "pw-"+w.WalletID, w.WalletAddress)
	require.NoError(t, err)

	hash := sha256.Sum256([]byte(w.IntentID))
	_, err = d.Exec(ctx, `INSERT INTO trade_intents (id, account_id, actor_type, actor_id, action, instrument_id, notional_usd_minor,
			constraints, requested_at, idempotency_key, correlation_id, mode, status, content_hash)
		VALUES ($1,$2,'USER','user-1','ACQUIRE_NOTIONAL',$3,10000,'{}',now(),$4,$5,'LIVE','PLANNED',$6)`,
		w.IntentID, w.AccountID, w.Instrument.ID, "idem-"+w.IntentID, "corr-"+w.IntentID[:8], hash[:])
	require.NoError(t, err)

	out := &world{
		World: w, d: d,
		audit:  audit.NewWriter(),
		outbox: event.NewOutbox(w.Clock),
	}
	out.plans = settlement.NewPlanRepository(w.Clock, out.audit)
	out.orders = execution.NewRepository(w.Clock, out.outbox, out.audit)
	out.attempts = execution.NewAttemptRepository(w.Clock, out.outbox, out.audit)
	out.capital = capital.NewService(w.Clock, capitalEmitter{outbox: out.outbox, clk: w.Clock})
	out.lots = positions.NewEngine()
	out.intents = intent.NewRepository(w.Clock, out.outbox, out.audit)
	// The fake signer writes no signing_decisions row, so the attempt must not
	// carry a decision id that has no matching row.
	w.Signer.OmitDecisionID = true

	seedWalletBalance(t, d, w.AccountID, usdc.ID, money.QuantityFromInt64(1_000_000_000))
	seedWalletBalance(t, d, w.AccountID, sol.ID, money.QuantityFromInt64(1_000_000_000))
	return out
}

// deps assembles exactly what main.go's settlementDeps assembles, with the
// production repositories and the world's fakes for the unwired providers.
func (w *world) deps() settlement.Deps {
	d := w.Deps()
	d.DB = w.d
	d.Plans = w.plans
	d.Orders = w.orders
	d.Attempts = w.attempts
	d.Quotes = quoteStore{repo: quote.NewRepository()}
	d.Intents = intentReader{repo: w.intents}
	d.Capital = w.capital
	d.Ledger = ledger.NewService(w.Clock, "execution-worker-itest")
	d.Positions = w.lots
	d.Audit = w.audit
	return d
}

func (w *world) executor(t *testing.T) *settlement.Executor {
	t.Helper()
	ex, err := settlement.NewExecutor(w.deps(), w.Signer)
	require.NoError(t, err)
	return ex
}

// approvedPlan plans the world's buy intent and stores it APPROVED.
func (w *world) approvedPlan(t *testing.T) settlement.Plan {
	t.Helper()
	plan, err := w.Planner.Plan(w.Input())
	require.NoError(t, err)
	plan.AccountID = w.AccountID.String()
	require.NoError(t, w.d.InTx(t.Context(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := w.plans.Create(ctx, tx, plan); err != nil {
			return err
		}
		_, err := w.plans.Approve(ctx, tx, plan.ID, "OPERATOR", "op-1", "approved for execution")
		return err
	}))
	return plan
}

func seedWalletBalance(t *testing.T, d *db.DB, account accounts.AccountID, asset assets.AssetID, qty money.Quantity) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		wallet := ledgerAccountID(t, tx, account, asset, "WALLET", "DEBIT")
		capitalAcct := ledgerAccountID(t, tx, account, asset, "CAPITAL", "CREDIT")
		txID := id.New[id.Any]()
		if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id, effective_at, posted_by_actor_type, posted_by_actor_id, content_hash)
			VALUES ($1,'SEED',$2,'seed',$3,now(),'SYSTEM','execution-worker-itest',$4)`, txID, "seed:"+txID.String(), txID.String(), []byte{0}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1,$2,0,$3,$4,'DEBIT',$5)`,
			id.New[id.Any](), txID, wallet, asset, qty); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1,$2,1,$3,$4,'CREDIT',$5)`,
			id.New[id.Any](), txID, capitalAcct, asset, qty)
		return err
	}))
}

func ledgerAccountID(t *testing.T, tx pgx.Tx, account accounts.AccountID, asset assets.AssetID, code, side string) id.ID[id.Any] {
	t.Helper()
	ctx := t.Context()
	_, err := tx.Exec(ctx, `INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side)
		VALUES ($1,'CUSTOMER',$2,$3,$4,$5) ON CONFLICT (owner_type, owner_id, code, asset_id) DO NOTHING`,
		id.New[id.Any](), account, code, asset, side)
	require.NoError(t, err)
	var out id.ID[id.Any]
	require.NoError(t, tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE owner_type='CUSTOMER' AND owner_id=$1 AND code=$2 AND asset_id=$3`,
		account, code, asset).Scan(&out))
	return out
}
