//go:build integration

package httpapi

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/valuation"
)

// TestIntegration_StableAssetAndBridgeLifecycleOverHTTP walks a sandbox
// stable asset through its life over HTTP, against the real adapters, services
// and database: an operator registers it, a customer issues 600 units against
// 600 sandbox dollars, a signed reserve snapshot is published and verified
// from the API's answer alone, the customer redeems 200, a second snapshot
// extends the chain, the supply reconciles; then an operator opens a route to
// sbx-ethereum, the customer bridges 100 units, the leg runs to RECONCILED,
// the route and the stable asset's supply both reconcile, and every unit is
// accounted for to the base unit -- on the books, in the wallets and on both
// chains. A stranger can read none of the customer's requests or legs.
func TestIntegration_StableAssetAndBridgeLifecycleOverHTTP(t *testing.T) {
	w := newStableChainWorld(t)
	user, acct := w.customer(1_000)
	strangerUser, strangerAcct := w.customer(0)
	w.serve(user, acct)
	cust := customerOf(user, acct)
	stranger := customerOf(strangerUser, strangerAcct)
	op := w.operator()
	oracle := "itest-oracle-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
	asset := w.register(op, oracle)
	assetPath := "/v1/advanced/stable-assets/" + asset.Id.String()
	units := stableItestAssetID(t, asset.AssetId)
	usd := econ.SandboxDollarID()
	mint := stableItestMint(t, asset)
	holder := w.chain.Keys.Custody.HolderAddress(acct)
	facility := asset.FacilityId.String()

	// --- issue ---------------------------------------------------------------
	issueKey := w.key("issue")
	issueBody := map[string]any{"account_id": acct.String(), "quantity": "600000000", "reserve_asset_id": usd.String(), "mode": "SANDBOX"}
	var issued api.StableIssuanceResult
	issuedRes := w.post(cust, assetPath+"/issuances", issueBody, issueKey, http.StatusOK, &issued)
	assert.Equal(t, api.SIMULATED, issued.ValueDomain)
	assert.True(t, issued.Settled)
	assert.Equal(t, api.StableIssuanceStatusIssued, issued.Issuance.Status)
	assert.Equal(t, "600000000", issued.Issuance.ReserveQuantity, "one sandbox dollar per unit at the peg")
	assert.Equal(t, holder.String(), issued.Issuance.Recipient)
	assert.Equal(t, api.StableReconciliationNotRequired, issued.Issuance.Reconciliation.State)
	require.NotNil(t, issued.Issuance.JournalTransactionId)
	again := w.post(cust, assetPath+"/issuances", issueBody, issueKey, http.StatusOK, nil)
	assert.JSONEq(t, issuedRes.Body.String(), again.Body.String(), "the same key replays the first answer")
	assert.Equal(t, 1, w.submissions(issued.Issuance.TxId), "and submits nothing again")
	conflicting := map[string]any{"account_id": acct.String(), "quantity": "1", "reserve_asset_id": usd.String(), "mode": "SANDBOX"}
	w.h.as(&cust)
	res := w.h.do(http.MethodPost, assetPath+"/issuances", conflicting, "Idempotency-Key", issueKey)
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())

	var reserve api.StableReserve
	w.get(cust, assetPath+"/reserve", &reserve)
	assert.Equal(t, "600000000", reserve.IssuedSupply)
	require.Len(t, reserve.CashEquivalentReserve, 1)
	assert.Equal(t, "600000000", reserve.CashEquivalentReserve[0].Quantity)
	assert.Equal(t, int64(10_000), reserve.CoverageBps)
	assert.True(t, reserve.Covered)

	// --- the first snapshot, verified from the API's answer alone ------------
	var taken api.StableSnapshotResult
	w.post(op, assetPath+"/snapshots", nil, w.key("snapshot"), http.StatusOK, &taken)
	assert.Equal(t, int64(1), taken.Snapshot.Seq)
	assert.True(t, taken.Snapshot.Verified)
	assert.Equal(t, "600000000", taken.Snapshot.Body.LedgerSupply)
	assert.Equal(t, "600000000", taken.Snapshot.Body.ChainSupply)
	assert.Equal(t, "10000", taken.Snapshot.Body.CoverageBps)
	assert.Contains(t, taken.Snapshot.Body.Disclaimer, "not an attestation")

	// --- redeem ----------------------------------------------------------------
	var redeemed api.StableRedemptionResult
	w.post(cust, assetPath+"/redemptions", map[string]any{"account_id": acct.String(), "quantity": "200000000", "mode": "SANDBOX"},
		w.key("redeem"), http.StatusOK, &redeemed)
	assert.True(t, redeemed.Settled)
	assert.Equal(t, api.StableRedemptionStatusRedeemed, redeemed.Redemption.Status)
	assert.Equal(t, "200000000", redeemed.Redemption.PayoutQuantity, "the exact inverse at par")
	assert.Equal(t, "400000000", w.wallet(acct, units))
	assert.Equal(t, "600000000", w.wallet(acct, usd))

	// --- the second snapshot and the chain's verification ---------------------
	w.post(op, assetPath+"/snapshots", nil, w.key("snapshot"), http.StatusOK, &taken)
	assert.Equal(t, int64(2), taken.Snapshot.Seq)
	assert.Equal(t, "400000000", taken.Snapshot.Body.LedgerSupply)
	var chain api.StableSnapshotChain
	w.get(cust, assetPath+"/snapshots", &chain)
	assert.True(t, chain.Verification.Verified)
	assert.Equal(t, 2, chain.Verification.Checked)
	assert.Equal(t, hex.EncodeToString(w.chain.Keys.Snapshots.PublicKey()), chain.Verification.PublicKey)
	require.Len(t, chain.Items, 2)
	var parsed []stableasset.SignedSnapshot
	for i, sn := range chain.Items {
		assert.True(t, sn.Verified)
		body := []byte(sn.CanonicalBody)
		sum := sha256.Sum256(body)
		assert.Equal(t, hex.EncodeToString(sum[:]), sn.Hash, "the hash covers exactly the published bytes")
		if i > 0 {
			assert.Equal(t, chain.Items[i-1].Hash, sn.PrevHash, "each snapshot names the one before it")
		}
		hash, err := hex.DecodeString(sn.Hash)
		require.NoError(t, err)
		sig, err := hex.DecodeString(sn.Signature)
		require.NoError(t, err)
		key, err := hex.DecodeString(sn.SignerPublicKey)
		require.NoError(t, err)
		p, err := stableasset.ParseSignedSnapshot(body, hash, sig, key)
		require.NoError(t, err)
		parsed = append(parsed, p)
	}
	// Anyone holding the published key verifies the published chain.
	published, err := hex.DecodeString(chain.Verification.PublicKey)
	require.NoError(t, err)
	require.NoError(t, stableasset.VerifySnapshotChain(parsed, ed25519.PublicKey(published)))

	// --- the monitor and the supply reconciliation ----------------------------
	_, err = w.prices.RecordPrice(t.Context(), w.d, valuation.PriceObservation{
		AssetID: units, QuoteAssetID: usd, Mantissa: money.QuantityFromInt64(1_000_000), Scale: 6, Source: oracle, ObservedAt: w.clk.Now(),
	})
	require.NoError(t, err)
	var monitor api.StableMonitorReport
	w.post(op, assetPath+"/monitor-runs", nil, w.key("monitor"), http.StatusOK, &monitor)
	assert.False(t, monitor.PriceStale)
	assert.Equal(t, int64(0), monitor.DeviationBps)
	assert.Empty(t, monitor.Alerts)
	assert.False(t, monitor.BreakerTripped)
	assert.Equal(t, api.StableAssetStatusActive, monitor.Status)
	var recon api.StableSupplyReconciliation
	w.post(op, assetPath+"/reconciliations", nil, w.key("reconcile"), http.StatusOK, &recon)
	assert.True(t, recon.Matched, "%+v", recon)
	assert.Equal(t, "400000000", recon.LedgerOutstanding)
	assert.Equal(t, "400000000", recon.ChainSupply)

	// --- bridge to sbx-ethereum --------------------------------------------------
	twinSymbol := "SBXE" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:6])
	var route api.BridgeRoute
	w.post(op, "/v1/advanced/bridge/routes", map[string]any{
		"name": "itest-route-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10], "source_asset_id": units.String(),
		"destination_chain": "sbx-ethereum", "destination_symbol": twinSymbol, "unit_price": "1", "max_release_attempts": 3,
	}, w.key("route"), http.StatusOK, &route)
	assert.Equal(t, api.BridgeRouteStatusActive, route.Status)
	assert.Equal(t, string(devnethost.ChainSolana), route.SourceChain)
	assert.Equal(t, string(devnethost.ChainEthereum), route.DestinationChain)
	assert.Equal(t, w.chain.Keys.Attester.PublicKey().String(), route.Attester)
	twin := stableItestAssetID(t, route.DestinationAssetId)

	var routes api.BridgeRouteList
	w.get(cust, "/v1/advanced/bridge/routes", &routes)
	found := false
	for _, r := range routes.Items {
		found = found || r.Id == route.Id
	}
	assert.True(t, found, "the customer sees the route")

	var leg api.BridgeLegResult
	w.post(cust, "/v1/advanced/bridge/legs", map[string]any{
		"account_id": acct.String(), "route_id": route.Id.String(), "quantity": "100000000", "mode": "SANDBOX",
	}, w.key("leg"), http.StatusOK, &leg)
	assert.True(t, leg.Settled)
	assert.Equal(t, api.BridgeLegStatusReconciled, leg.Leg.Status)
	assert.Equal(t, api.BridgeLegHoldingWalletDestination, leg.Leg.Holding)
	assert.Equal(t, api.BridgeReconciliationNotRequired, leg.Leg.Reconciliation.State)

	var detail api.BridgeLegDetail
	w.get(cust, "/v1/advanced/bridge/legs/"+leg.Leg.Id.String(), &detail)
	var path []string
	for _, tr := range detail.Transitions {
		path = append(path, string(tr.ToStatus))
	}
	assert.Equal(t, []string{"AVAILABLE", "RESERVED", "BRIDGE_REQUESTED", "SOURCE_FINALIZED", "MESSAGE_IN_FLIGHT",
		"DESTINATION_FINALIZED", "RECONCILED"}, path)
	require.NotNil(t, detail.Lock)
	assert.Equal(t, api.BridgeTxStateFinalized, detail.Lock.State)
	require.Len(t, detail.Attempts, 1)
	assert.Equal(t, api.BridgeAttemptKindRelease, detail.Attempts[0].Kind)
	assert.Equal(t, api.BridgeTxStateFinalized, detail.Attempts[0].OnChain.State)
	assert.Equal(t, 1, w.submissions(detail.Lock.TxId), "the lock was submitted once")
	assert.Equal(t, 1, w.submissions(detail.Attempts[0].TxId), "the release was submitted once")

	var legs api.BridgeLegPage
	w.get(cust, "/v1/advanced/bridge/legs?account_id="+acct.String(), &legs)
	require.Len(t, legs.Items, 1)
	assert.Equal(t, leg.Leg.Id, legs.Items[0].Id)

	var routeRecon api.BridgeRouteReconciliation
	w.post(op, "/v1/advanced/bridge/routes/"+route.Id.String()+"/reconciliations", nil, w.key("route-recon"), http.StatusOK, &routeRecon)
	assert.True(t, routeRecon.Matched, "%+v", routeRecon)
	assert.Equal(t, "100000000", routeRecon.Locked)
	assert.Equal(t, "100000000", routeRecon.Outstanding)
	assert.Equal(t, "0", routeRecon.InFlight)
	assert.Equal(t, "100000000", routeRecon.Escrow)
	assert.Equal(t, "100000000", routeRecon.DestinationSupply)
	w.post(op, assetPath+"/reconciliations", nil, w.key("reconcile"), http.StatusOK, &recon)
	assert.True(t, recon.Matched, "%+v", recon)
	assert.Equal(t, "300000000", recon.CustomerWalletSum)
	assert.Equal(t, "100000000", recon.HeldElsewhere, "the bridged units are held by the bridge, and still outstanding")

	// --- holdings by chain, and what is in flight ------------------------------
	var byChain api.BridgeHoldings
	w.get(cust, "/v1/advanced/bridge/holdings?account_id="+acct.String(), &byChain)
	assert.Equal(t, api.SIMULATED, byChain.ValueDomain)
	got := map[string]string{}
	for _, c := range byChain.Chains {
		for _, h := range c.Holdings {
			got[c.Chain+"/"+h.Symbol] = h.Quantity
		}
	}
	assert.Equal(t, map[string]string{
		"nodal-sandbox/" + econ.SandboxDollarSymbol:         "600000000",
		string(devnethost.ChainSolana) + "/" + asset.Symbol: "300000000",
		string(devnethost.ChainEthereum) + "/" + twinSymbol: "100000000",
	}, got)
	assert.Empty(t, byChain.InFlight, "nothing is in flight once the leg is final")
	assert.Empty(t, byChain.Legs)
	assert.Equal(t, "0.00", byChain.PendingLocksUsd)
	assert.Equal(t, "bridge-haircut-v1", byChain.Policy.Version)

	var holdings api.StableHoldings
	w.get(cust, "/v1/advanced/stable-assets/holdings?account_id="+acct.String(), &holdings)
	assert.Equal(t, holder.String(), holdings.HolderAddress)
	var line *api.StableHolding
	for i := range holdings.Items {
		if holdings.Items[i].StableAssetId == asset.Id {
			line = &holdings.Items[i]
		}
	}
	require.NotNil(t, line)
	assert.Equal(t, "300000000", line.Wallet)
	assert.Equal(t, "300000000", line.Available)
	assert.Equal(t, "300000000", line.ChainBalance)

	// --- conservation, to the base unit ------------------------------------------
	// The sandbox dollar: the customer's wallet and the issuer's reserve.
	assert.Equal(t, "600000000", w.wallet(acct, usd))
	assert.Equal(t, "400000000", w.facility(facility, ledger.CodeStableReserve, usd))
	// The stable asset: outstanding = the customer's wallet + the bridge escrow,
	// and the chain holds exactly that, split the same way.
	assert.Equal(t, "400000000", w.facility(facility, ledger.CodeStableOutstanding, units))
	assert.Equal(t, "300000000", w.wallet(acct, units))
	routeFacility := route.FacilityId.String()
	assert.Equal(t, "100000000", w.facility(routeFacility, ledger.CodeBridgeLocked, units))
	ctx := t.Context()
	srcSupply, err := w.chain.Host.Supply(ctx, devnethost.ChainSolana, mint)
	require.NoError(t, err)
	srcHolder, err := w.chain.Host.TokenBalance(ctx, devnethost.ChainSolana, holder, mint)
	require.NoError(t, err)
	escrow, err := w.chain.Host.TokenBalance(ctx, devnethost.ChainSolana, devnet.BridgeEscrowAddress(devnethost.ChainSolana), mint)
	require.NoError(t, err)
	assert.Equal(t, uint64(400_000_000), srcSupply)
	assert.Equal(t, uint64(300_000_000), srcHolder)
	assert.Equal(t, uint64(100_000_000), escrow)
	// The twin: outstanding on the books = the customer's wallet = the
	// destination supply = the holder's balance on sbx-ethereum.
	twinMint, err := devnet.ParsePublicKey(route.DestinationMint)
	require.NoError(t, err)
	assert.Equal(t, "100000000", w.facility(routeFacility, ledger.CodeBridgeOutstanding, twin))
	assert.Equal(t, "100000000", w.wallet(acct, twin))
	dstSupply, err := w.chain.Host.Supply(ctx, devnethost.ChainEthereum, twinMint)
	require.NoError(t, err)
	dstHolder, err := w.chain.Host.TokenBalance(ctx, devnethost.ChainEthereum, holder, twinMint)
	require.NoError(t, err)
	assert.Equal(t, uint64(100_000_000), dstSupply)
	assert.Equal(t, uint64(100_000_000), dstHolder)

	// --- a stranger reads none of it ----------------------------------------------
	w.h.as(&stranger)
	for _, path := range []string{
		"/v1/advanced/stable-assets/issuances?account_id=" + acct.String(),
		"/v1/advanced/stable-assets/redemptions?account_id=" + acct.String(),
		"/v1/advanced/stable-assets/holdings?account_id=" + acct.String(),
		"/v1/advanced/bridge/legs?account_id=" + acct.String(),
		"/v1/advanced/bridge/holdings?account_id=" + acct.String(),
	} {
		res := w.h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", path, res.Body.String())
	}
	for _, path := range []string{
		"/v1/advanced/stable-assets/issuances/" + issued.Issuance.Id.String(),
		"/v1/advanced/stable-assets/redemptions/" + redeemed.Redemption.Id.String(),
		"/v1/advanced/bridge/legs/" + leg.Leg.Id.String(),
	} {
		res := w.h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusNotFound, res.Code, "another account's record is NOT_FOUND: %s body=%s", path, res.Body.String())
	}
	res = w.h.do(http.MethodPost, assetPath+"/issuances", issueBody, "Idempotency-Key", w.key("foreign"))
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
}
