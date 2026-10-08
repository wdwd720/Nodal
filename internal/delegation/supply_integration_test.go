//go:build integration

package delegation

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// supplyOnSolana registers, on the fixture's chain (sbx-solana), a platform
// stable asset reserved in the sandbox dollar and a bridged twin (of a source
// on sbx-ethereum).
func (f *fixture) supplyOnSolana() (econtest.Stable, econtest.Twin) {
	f.t.Helper()
	var stable econtest.Stable
	var twin econtest.Twin
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		dollar, err := econ.EnsureSandboxDollar(ctx, tx)
		if err != nil {
			return err
		}
		if stable, err = econtest.RegisterStable(ctx, tx, dollar.ID); err != nil {
			return err
		}
		source, err := econtest.SourceAssetOn(ctx, tx, string(devnethost.ChainEthereum))
		if err != nil {
			return err
		}
		twin, err = econtest.RegisterTwin(ctx, tx, source)
		return err
	}))
	require.Equal(f.t, string(devnethost.ChainSolana), stable.Asset.Chain)
	require.Equal(f.t, string(devnethost.ChainSolana), twin.Asset.Chain)
	return stable, twin
}

// issueStable gives acct q units of the stable asset in the books, paid for
// in sandbox dollars (no unit moves on the chain).
func (f *fixture) issueStable(s econtest.Stable, acct accounts.AccountID, q money.Quantity) {
	f.t.Helper()
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		if _, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "dg-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, s.Reserve), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, s.Reserve), Side: ledger.Credit, Quantity: q},
			},
		}); err != nil {
			return err
		}
		_, err := f.ledger.Post(ctx, tx, s.Issuance(acct, q, ref, f.clk.Now()))
		return err
	}))
}

// exchange is a trade between a customer and the venue's house book, booked
// on both sides as a settled swap is (SIM_TRADE): the customer gives giveQty
// of give to the house and receives getQty of get from it.
func (f *fixture) exchange(acct accounts.AccountID, give assets.AssetID, giveQty money.Quantity, get assets.AssetID, getQty money.Quantity) {
	f.t.Helper()
	c := func(code ledger.Code, a assets.AssetID) ledger.AccountRef {
		return ledger.CustomerAccount(acct, code, a)
	}
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSimTrade, IdempotencyKey: "dg-exchange:" + ref,
			Reference: ledger.FinancialEventReference{Type: "itest_exchange", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: c(ledger.CodeTradingOutflow, give), Side: ledger.Debit, Quantity: giveQty},
				{Account: c(ledger.CodeWallet, give), Side: ledger.Credit, Quantity: giveQty},
				{Account: c(ledger.CodeWallet, get), Side: ledger.Debit, Quantity: getQty},
				{Account: c(ledger.CodeTradingInflow, get), Side: ledger.Credit, Quantity: getQty},
				{Account: f.house.Account(ledger.CodeHouseInventory, give), Side: ledger.Debit, Quantity: giveQty},
				{Account: f.house.Account(ledger.CodeHouseTradingInflow, give), Side: ledger.Credit, Quantity: giveQty},
				{Account: f.house.Account(ledger.CodeHouseTradingOutflow, get), Side: ledger.Debit, Quantity: getQty},
				{Account: f.house.Account(ledger.CodeHouseInventory, get), Side: ledger.Credit, Quantity: getQty},
			},
		})
		return err
	}))
}

func (f *fixture) countRows(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

// TestIntegration_ASupplyControlledAssetIsNeverWithdrawn (migration 00763):
// SANDBOX_WITHDRAWAL is refused for a platform stable asset or a bridged twin
// -- a unit leaving the books would leave the asset's holdings below what its
// controller issued. ProposeWithdrawal refuses it by name before anything is
// written, so the chain never moves units the books could not record leaving.
func TestIntegration_ASupplyControlledAssetIsNeverWithdrawn(t *testing.T) {
	f := newFixture(t)
	s := f.withdrawalSetup(nil, nil)
	stable, twin := f.supplyOnSolana()
	f.issueStable(stable, s.c.account, money.QuantityFromInt64(50_000_000))
	for i, c := range []econtest.Controlled{
		{Asset: stable.Asset.ID, Controller: econ.ControllerStableIssuer}, {Asset: twin.Asset.ID, Controller: econ.ControllerBridge},
	} {
		req := f.proposal(s, uint64(100+i), 10_000_000)
		req.AssetID = c.Asset
		_, err := f.propose(f.agent(s.c), req)
		econtest.RequireRefusal(t, err, c.Asset, c.Controller)
	}
	assert.Zero(t, f.countRows(`SELECT count(*) FROM economic_intents WHERE account_id = $1`, s.c.account), "no intent was written")
	assert.Zero(t, f.countRows(`SELECT count(*) FROM delegated_withdrawals WHERE account_id = $1`, s.c.account))
	assert.Equal(t, "50000000", f.balance(s.c.account, ledger.CodeWallet, stable.Asset).String(), "nothing left the wallet")
	f.mustPropose(s, 200, 10_000_000) // the fixture's dollar is no controller's
}

// TestIntegration_AVenueHoldingNoneOfASupplyControlledAssetNeverDeliversIt: a
// venue's house book gets a stable asset only by being paid it in a trade --
// HOUSE_FUNDING of one is refused (SU001). A swap that would receive one from
// a venue holding none is refused by name before anything is written; once
// the venue has been paid some, a swap delivering it is admitted (a trade
// between two booked parties, which the database allows), a replay of that
// request is answered as before whatever the venue holds now, and the
// asset's supply still reconciles.
func TestIntegration_AVenueHoldingNoneOfASupplyControlledAssetNeverDeliversIt(t *testing.T) {
	f := newFixture(t)
	c := f.customer(100_000_000)
	f.fundCustomer(c, f.tok, 5_000_000_000) // 5 tokens
	capability := f.grant(c, nil)
	stable, _ := f.supplyOnSolana()
	request := func(nonce uint64) SwapRequest {
		return SwapRequest{CapabilityID: capability.ID, AccountID: c.account, Nonce: nonce, Action: ActionReduceExposure,
			VenueID: f.venue.ID, PayAssetID: f.tok.ID, ReceiveAssetID: stable.Asset.ID, AmountIn: money.QuantityFromInt64(1_000_000_000),
			MaxSlippageBPS: 100, Jurisdiction: "US-CA", CorrelationID: "corr-" + uuid.NewString()}
	}
	intentKey := func(nonce uint64) string {
		return "delegation:swap:" + capability.ID.String() + ":" + strconv.FormatUint(nonce, 10)
	}

	_, err := f.requestSwap(f.agent(c), request(1))
	econtest.RequireRefusal(t, err, stable.Asset.ID, econ.ControllerStableIssuer)
	assert.Zero(t, f.countRows(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND idempotency_key = $2`, c.account, intentKey(1)),
		"no intent was written")
	assert.Zero(t, f.countRows(`SELECT count(*) FROM delegated_swaps WHERE account_id = $1`, c.account))

	// Another customer pays the venue 200 units of the stable asset.
	holder := f.customer(0)
	f.issueStable(stable, holder.account, money.QuantityFromInt64(1_000_000_000))
	f.exchange(holder.account, stable.Asset.ID, money.QuantityFromInt64(200_000_000), f.tok.ID, money.QuantityFromInt64(2_000_000_000))

	ticket, err := f.requestSwap(f.agent(c), request(2))
	require.NoError(t, err, "a venue that holds the asset may deliver it")
	admitted := ticket.Swap
	require.False(t, admitted.ID.IsZero())
	assert.Equal(t, stable.Asset.ID, admitted.ReceiveAssetID)
	require.True(t, admitted.AmountOut.IsPositive())

	// What the venue holds beyond its promise goes back to the holder.
	spare := money.QuantityFromInt64(200_000_000).Sub(admitted.AmountOut)
	require.True(t, spare.IsPositive())
	f.exchange(holder.account, f.tok.ID, money.QuantityFromInt64(1_000_000_000), stable.Asset.ID, spare)

	replay, err := f.requestSwap(f.agent(c), request(2))
	require.NoError(t, err, "a replay is answered as the first request was")
	assert.True(t, replay.Replay)
	assert.Equal(t, admitted.ID, replay.Swap.ID)
	_, err = f.requestSwap(f.agent(c), request(3))
	econtest.RequireRefusal(t, err, stable.Asset.ID, econ.ControllerStableIssuer)

	held, err := econtest.Holdings(f.ctx, testDB, stable.Asset.ID)
	require.NoError(t, err)
	outstanding, err := stable.Outstanding(f.ctx, testDB, f.ledger)
	require.NoError(t, err)
	assert.True(t, held.Equal(outstanding), "holdings %s, outstanding %s", held, outstanding)
}
