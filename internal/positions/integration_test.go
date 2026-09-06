//go:build integration

package positions

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

var testAppURL = os.Getenv("CP_TEST_DATABASE_URL")

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	if testAppURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test (provision one with `go run ./scripts/testdb -name positions`)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "positions-itest", MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func seedAccount(t *testing.T, d *db.DB) accounts.AccountID {
	t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(t.Context(), d, "itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	a, err := repo.CreateAccount(t.Context(), d, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return a.ID
}

func seedAsset(t *testing.T, d *db.DB, symbol string, decimals uint8) assets.Asset {
	t.Helper()
	a := assets.Asset{
		Chain: "solana-devnet", MintAddress: "mint-" + id.New[id.Any]().String(), Kind: assets.KindSPLToken, Symbol: symbol, Name: symbol,
		Decimals: decimals, RiskClass: assets.RiskStandard, Status: assets.StatusActive,
	}
	created, err := assets.NewRepository().Create(t.Context(), d, a)
	require.NoError(t, err)
	return created
}

// seedWalletBalance posts a SEED journal transaction Dr WALLET / Cr CAPITAL
// for qty of asset, letting the ledger trigger maintain ledger_balances.
func seedWalletBalance(t *testing.T, d *db.DB, account accounts.AccountID, asset assets.AssetID, qty money.Quantity) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		wallet := ledgerAccount(t, tx, account, asset, "WALLET", "DEBIT")
		capital := ledgerAccount(t, tx, account, asset, "CAPITAL", "CREDIT")
		txID := id.New[id.Any]()
		if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id, effective_at, posted_by_actor_type, posted_by_actor_id, content_hash)
			VALUES ($1,'SEED',$2,'seed',$3,now(),'SYSTEM','positions-itest',$4)`, txID, "seed:"+txID.String(), txID.String(), []byte{0}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1,$2,0,$3,$4,'DEBIT',$5)`,
			id.New[id.Any](), txID, wallet, asset, qty); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES ($1,$2,1,$3,$4,'CREDIT',$5)`,
			id.New[id.Any](), txID, capital, asset, qty)
		return err
	}))
}

func ledgerAccount(t *testing.T, tx pgx.Tx, account accounts.AccountID, asset assets.AssetID, code, side string) id.ID[id.Any] {
	t.Helper()
	ctx := t.Context()
	_, err := tx.Exec(ctx, `INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side)
		VALUES ($1,'CUSTOMER',$2,$3,$4,$5) ON CONFLICT (owner_type, owner_id, code, asset_id) DO NOTHING`, id.New[id.Any](), account, code, asset, side)
	require.NoError(t, err)
	var out id.ID[id.Any]
	require.NoError(t, tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE owner_type='CUSTOMER' AND owner_id=$1 AND code=$2 AND asset_id=$3`, account, code, asset).Scan(&out))
	return out
}

func acquire(t *testing.T, d *db.DB, e *Engine, in AcquireLot) Lot {
	t.Helper()
	var lot Lot
	require.NoError(t, d.InTx(t.Context(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		lot, err = e.Acquire(ctx, tx, in)
		return err
	}))
	return lot
}

func dispose(t *testing.T, d *db.DB, e *Engine, in Disposal) ([]LotDisposition, RealizedPnL, error) {
	t.Helper()
	var out []LotDisposition
	var pnl RealizedPnL
	err := d.InTx(t.Context(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, pnl, err = e.Dispose(ctx, tx, in)
		return err
	})
	return out, pnl, err
}

func TestIntegration_LotLifecycle_FIFO_HoldingsAndPnL(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	e := NewEngine()
	acct := seedAccount(t, d)
	sol := seedAsset(t, d, "SOL", 9)
	fill := func(n string) Ref { return Ref{Type: "fill", ID: n} }

	// Two acquisitions: 1 SOL @ 100 (+0.10 fee) and 1 SOL @ 200 (+0.20 fee).
	l1 := acquire(t, d, e, AcquireLot{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "1000000000"), AcquiredAt: t0,
		Cost: usd(t, "100.00"), Fees: usd(t, "0.10"), BasisSource: "fill", ValuationSource: "jupiter-quote", AcquisitionRef: fill("f1"), Venue: "jupiter",
	})
	assert.Equal(t, "100.10", l1.CostBasis.String())
	assert.Equal(t, "0.10", l1.Fees.String())
	assert.Equal(t, LotOpen, l1.Status)
	assert.Equal(t, "jupiter", l1.Venue)
	l2 := acquire(t, d, e, AcquireLot{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "1000000000"), AcquiredAt: t0.Add(time.Hour),
		Cost: usd(t, "200.00"), Fees: usd(t, "0.20"), BasisSource: "fill", ValuationSource: "jupiter-quote", AcquisitionRef: fill("f2"),
	})

	h, err := e.Holdings(ctx, d, acct)
	require.NoError(t, err)
	require.Len(t, h, 1)
	assert.Equal(t, "2000000000", h[0].Quantity.String())
	assert.Equal(t, "300.30", h[0].CostBasis.String())
	assert.Equal(t, "150.15000000", h[0].AverageBasis)
	assert.Equal(t, 2, h[0].LotCount)
	assert.Equal(t, "SOL", h[0].Symbol)
	assert.Equal(t, uint8(9), h[0].Decimals)
	assert.Equal(t, t0, h[0].OldestAcquiredAt)

	// Dispose 1.5 SOL for 270 with 0.30 fees: lot 1 fully (basis 100.10), lot 2 half (basis 100.10).
	disps, pnl, err := dispose(t, d, e, Disposal{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "1500000000"), DisposedAt: t0.Add(2 * time.Hour),
		Proceeds: usd(t, "270.00"), Fees: usd(t, "0.30"), ValuationSource: "fill-price", DispositionRef: fill("f3"),
	})
	require.NoError(t, err)
	require.Len(t, disps, 2)
	assert.Equal(t, l1.ID, disps[0].LotID, "FIFO: oldest lot first")
	assert.Equal(t, "1000000000", disps[0].Quantity.String())
	assert.Equal(t, "100.10", disps[0].Basis.String())
	assert.Equal(t, "180.00", disps[0].Proceeds.String())
	assert.Equal(t, "0.20", disps[0].Fees.String())
	assert.Equal(t, "79.70", disps[0].RealizedPnL.String())
	assert.Equal(t, l2.ID, disps[1].LotID)
	assert.Equal(t, "500000000", disps[1].Quantity.String())
	assert.Equal(t, "100.10", disps[1].Basis.String())
	assert.Equal(t, "90.00", disps[1].Proceeds.String())
	assert.Equal(t, "0.10", disps[1].Fees.String())
	assert.Equal(t, "-10.20", disps[1].RealizedPnL.String())
	assert.Equal(t, "1500000000", pnl.Quantity.String())
	assert.Equal(t, "270.00", pnl.Proceeds.String())
	assert.Equal(t, "0.30", pnl.Fees.String())
	assert.Equal(t, "200.20", pnl.Basis.String())
	assert.Equal(t, "69.50", pnl.PnL.String())
	assert.Equal(t, 2, pnl.Dispositions)

	// Lot states after the split.
	var status string
	var open money.Quantity
	require.NoError(t, d.QueryRow(ctx, `SELECT status, quantity_open FROM position_lots WHERE id = $1`, l1.ID).Scan(&status, &open))
	assert.Equal(t, "CLOSED", status)
	assert.True(t, open.IsZero())
	require.NoError(t, d.QueryRow(ctx, `SELECT status, quantity_open FROM position_lots WHERE id = $1`, l2.ID).Scan(&status, &open))
	assert.Equal(t, "OPEN", status)
	assert.Equal(t, "500000000", open.String())

	// Holdings reflect the remaining half lot with its remaining basis.
	h, err = e.Holdings(ctx, d, acct)
	require.NoError(t, err)
	require.Len(t, h, 1)
	assert.Equal(t, "500000000", h[0].Quantity.String())
	assert.Equal(t, "100.10", h[0].CostBasis.String())
	assert.Equal(t, "200.20000000", h[0].AverageBasis)
	assert.Equal(t, 1, h[0].LotCount)

	// Basis conservation across the whole history: dispositions + remaining == acquisitions.
	var dispBasis money.Quantity
	require.NoError(t, d.QueryRow(ctx, `SELECT coalesce(sum(basis_usd_minor),0) FROM lot_dispositions WHERE account_id = $1`, acct).Scan(&dispBasis))
	assert.Equal(t, "20020", dispBasis.String())
	assert.Equal(t, int64(30030), 20020+h[0].CostBasis.Minor())

	// Over-disposal is rejected and writes nothing.
	_, _, err = dispose(t, d, e, Disposal{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "600000000"), DisposedAt: t0.Add(3 * time.Hour),
		Proceeds: usd(t, "1.00"), ValuationSource: "x", DispositionRef: fill("f4"),
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	var count int
	require.NoError(t, d.QueryRow(ctx, `SELECT count(*) FROM lot_dispositions WHERE account_id = $1`, acct).Scan(&count))
	assert.Equal(t, 2, count)

	// Realized P&L windows are half-open on disposed_at.
	rep, err := e.RealizedPnL(ctx, d, acct, t0, t0.Add(2*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, rep.Total.Dispositions, "window ends before the disposal")
	rep, err = e.RealizedPnL(ctx, d, acct, t0, t0.Add(2*time.Hour+time.Second))
	require.NoError(t, err)
	assert.Equal(t, 2, rep.Total.Dispositions)
	assert.Equal(t, "69.50", rep.Total.PnL.String())
	assert.Equal(t, "200.20", rep.Total.Basis.String())
	require.Len(t, rep.ByAsset, 1)
	assert.Equal(t, sol.ID, rep.ByAsset[0].AssetID)
	assert.Equal(t, "69.50", rep.ByAsset[0].PnL.String())

	// Dispositions are immutable.
	_, err = d.Exec(ctx, `UPDATE lot_dispositions SET realized_pnl_usd_minor = 0 WHERE id = $1`, disps[0].ID)
	require.Error(t, err)
	assert.True(t, db.IsMutationForbidden(err), "got %v", err)
	_, err = d.Exec(ctx, `DELETE FROM lot_dispositions WHERE id = $1`, disps[0].ID)
	require.Error(t, err)
	assert.True(t, db.IsMutationForbidden(err), "got %v", err)
}

func TestIntegration_VerifyAgainstLedger_DetectsDrift(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	e := NewEngine()
	acct := seedAccount(t, d)
	sol := seedAsset(t, d, "SOL", 9)
	usdc := seedAsset(t, d, "USDC", 6)
	bonk := seedAsset(t, d, "BONK", 5)

	// Ledger: 2 SOL, 500 USDC. Lots: 2 SOL, 400 USDC, and 1 BONK with no ledger account at all.
	seedWalletBalance(t, d, acct, sol.ID, q(t, "2000000000"))
	seedWalletBalance(t, d, acct, usdc.ID, q(t, "500000000"))
	acquire(t, d, e, AcquireLot{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "1500000000"), AcquiredAt: t0, Cost: usd(t, "225.00"),
		BasisSource: "fill", ValuationSource: "x", AcquisitionRef: Ref{"fill", "a"},
	})
	acquire(t, d, e, AcquireLot{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "500000000"), AcquiredAt: t0.Add(time.Minute), Cost: usd(t, "75.00"),
		BasisSource: "fill", ValuationSource: "x", AcquisitionRef: Ref{"fill", "b"},
	})
	acquire(t, d, e, AcquireLot{
		AccountID: acct, AssetID: usdc.ID, Quantity: q(t, "400000000"), AcquiredAt: t0, Cost: usd(t, "400.00"),
		BasisSource: "funding", ValuationSource: "face", AcquisitionRef: Ref{"deposit", "d1"},
	})
	acquire(t, d, e, AcquireLot{
		AccountID: acct, AssetID: bonk.ID, Quantity: q(t, "100000"), AcquiredAt: t0, Cost: usd(t, "0.02"),
		BasisSource: "reconciliation_adjustment", ValuationSource: "x", AcquisitionRef: Ref{"reconciliation_record", "r1"},
	})

	drifts, err := e.VerifyAgainstLedger(ctx, d, acct)
	require.NoError(t, err)
	require.Len(t, drifts, 2, "SOL agrees; USDC and BONK drift: %+v", drifts)
	byAsset := map[assets.AssetID]Drift{}
	for _, dr := range drifts {
		byAsset[dr.AssetID] = dr
	}
	assert.Equal(t, "400000000", byAsset[usdc.ID].LotQuantity.String())
	assert.Equal(t, "500000000", byAsset[usdc.ID].LedgerQuantity.String())
	assert.Equal(t, "-100000000", byAsset[usdc.ID].Difference.String())
	assert.Equal(t, "100000", byAsset[bonk.ID].LotQuantity.String())
	assert.True(t, byAsset[bonk.ID].LedgerQuantity.IsZero())
	assert.Equal(t, "100000", byAsset[bonk.ID].Difference.String())

	// Fix the USDC gap with another lot and dispose the BONK: no drift remains.
	acquire(t, d, e, AcquireLot{
		AccountID: acct, AssetID: usdc.ID, Quantity: q(t, "100000000"), AcquiredAt: t0.Add(time.Minute), Cost: usd(t, "100.00"),
		BasisSource: "funding", ValuationSource: "face", AcquisitionRef: Ref{"deposit", "d2"},
	})
	_, _, err = dispose(t, d, e, Disposal{
		AccountID: acct, AssetID: bonk.ID, Quantity: q(t, "100000"), DisposedAt: t0.Add(time.Hour),
		ValuationSource: "x", DispositionRef: Ref{"reconciliation_record", "r2"},
	})
	require.NoError(t, err)
	drifts, err = e.VerifyAgainstLedger(ctx, d, acct)
	require.NoError(t, err)
	assert.Empty(t, drifts)

	// Disposing 0.5 SOL without a matching ledger movement is detected too.
	_, _, err = dispose(t, d, e, Disposal{
		AccountID: acct, AssetID: sol.ID, Quantity: q(t, "500000000"), DisposedAt: t0.Add(time.Hour),
		Proceeds: usd(t, "70.00"), ValuationSource: "x", DispositionRef: Ref{"fill", "c"},
	})
	require.NoError(t, err)
	drifts, err = e.VerifyAgainstLedger(ctx, d, acct)
	require.NoError(t, err)
	require.Len(t, drifts, 1)
	assert.Equal(t, sol.ID, drifts[0].AssetID)
	assert.Equal(t, "-500000000", drifts[0].Difference.String())

	// Holdings match the open lots across assets, ordered by asset id.
	h, err := e.Holdings(ctx, d, acct)
	require.NoError(t, err)
	require.Len(t, h, 2)
	got := map[assets.AssetID]Holding{}
	for _, x := range h {
		got[x.AssetID] = x
	}
	assert.Equal(t, "1500000000", got[sol.ID].Quantity.String())
	assert.Equal(t, "225.00", got[sol.ID].CostBasis.String())
	assert.Equal(t, "500000000", got[usdc.ID].Quantity.String())
	assert.Equal(t, "500.00", got[usdc.ID].CostBasis.String())
	assert.Equal(t, "1.00000000", got[usdc.ID].AverageBasis)
	assert.True(t, id.Compare(h[0].AssetID, h[1].AssetID) < 0)
}

func TestIntegration_ConcurrentDisposalsNeverOverConsume(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	e := NewEngine()
	acct := seedAccount(t, d)
	sol := seedAsset(t, d, "SOL", 9)
	for i := 0; i < 5; i++ {
		acquire(t, d, e, AcquireLot{
			AccountID: acct, AssetID: sol.ID, Quantity: q(t, "1000000000"), AcquiredAt: t0.Add(time.Duration(i) * time.Minute),
			Cost: usd(t, "100.00"), BasisSource: "fill", ValuationSource: "x", AcquisitionRef: Ref{"fill", string(rune('a' + i))},
		})
	}
	// 8 workers each try to dispose 1 SOL; only 5 can succeed.
	const workers = 8
	errsCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			_, _, err := dispose(t, d, e, Disposal{
				AccountID: acct, AssetID: sol.ID, Quantity: q(t, "1000000000"), DisposedAt: t0.Add(time.Hour),
				Proceeds: usd(t, "120.00"), ValuationSource: "x", DispositionRef: Ref{"fill", "w" + string(rune('0'+i))},
			})
			errsCh <- err
		}(i)
	}
	ok, rejected := 0, 0
	for i := 0; i < workers; i++ {
		if err := <-errsCh; err == nil {
			ok++
		} else {
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "unexpected error %v", err)
			rejected++
		}
	}
	assert.Equal(t, 5, ok)
	assert.Equal(t, 3, rejected)
	var open money.Quantity
	require.NoError(t, d.QueryRow(ctx, `SELECT coalesce(sum(quantity_open),0) FROM position_lots WHERE account_id = $1`, acct).Scan(&open))
	assert.True(t, open.IsZero())
	var disposed money.Quantity
	require.NoError(t, d.QueryRow(ctx, `SELECT coalesce(sum(quantity),0) FROM lot_dispositions WHERE account_id = $1`, acct).Scan(&disposed))
	assert.Equal(t, "5000000000", disposed.String())
}

func TestIntegration_AcquireValidationAndForeignKeys(t *testing.T) {
	d := openTestDB(t)
	e := NewEngine()
	acct := seedAccount(t, d)
	sol := seedAsset(t, d, "SOL", 9)

	err := d.InTx(t.Context(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.Acquire(ctx, tx, AcquireLot{AccountID: acct, AssetID: sol.ID, Quantity: q(t, "0"), AcquiredAt: t0, BasisSource: "fill", ValuationSource: "x", AcquisitionRef: Ref{"fill", "z"}})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	err = d.InTx(t.Context(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.Acquire(ctx, tx, AcquireLot{AccountID: accounts.NewAccountID(), AssetID: sol.ID, Quantity: q(t, "1"), AcquiredAt: t0, BasisSource: "fill", ValuationSource: "x", AcquisitionRef: Ref{"fill", "z"}})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))

	// A lot linked to a journal transaction that does not exist is refused.
	err = d.InTx(t.Context(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.Acquire(ctx, tx, AcquireLot{
			AccountID: acct, AssetID: sol.ID, Quantity: q(t, "1"), AcquiredAt: t0, BasisSource: "fill", ValuationSource: "x",
			AcquisitionRef: Ref{"fill", "z"}, JournalTxID: id.New[id.Any](),
		})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}
