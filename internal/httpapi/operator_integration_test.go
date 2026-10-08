//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// opsWorld is the operator console over the real adapter and database.
type opsWorld struct {
	t      *testing.T
	d      *db.DB
	clk    *clock.Fake
	h      *harness
	prices *valuation.PriceStore
	ledger *ledger.Service
}

func newOpsWorld(t *testing.T, chains *devnethost.Host) *opsWorld {
	t.Helper()
	d := openTestDB(t)
	w := &opsWorld{t: t, d: d, clk: clock.NewFake(time.Now().UTC().Truncate(time.Millisecond))}
	require.NoError(t, d.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	w.prices = valuation.NewPriceStore(w.clk)
	w.ledger = ledger.NewService(w.clk, "httpapi-ops-itest")
	controller, err := killswitch.NewController(w.clk, noopKillAudit{}, NewApprovalVerifier(admin.NewService(w.clk, audit.NewWriter())))
	require.NoError(t, err)
	ports, err := Wire(WireDeps{DB: d, Clock: w.clk, Env: config.EnvTest, KillSwitches: controller, Advanced: AdvancedDeps{
		Operator: OperatorDeps{Prices: w.prices, Audit: audit.NewWriterWithBuildVersion("httpapi-ops-itest"), MaxMarkAge: 5 * time.Minute,
			Chains: chains},
	}})
	require.NoError(t, err)
	require.NotNil(t, ports.Operator, "the console wires with its price store, audit writer and kill switches")
	userID, acct := seedAccount(t, d)
	w.h = newIntegrationHarness(t, d, acct, userID)
	w.h.server.opts.Ports.Operator = ports.Operator
	return w
}

func (w *opsWorld) operator() security.Principal {
	return security.Principal{
		SubjectID: uuid.NewString(), ActorType: security.ActorOperator, Roles: []security.Role{security.RoleOperations},
		SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
}

func (w *opsWorld) simulated(symbol string) assets.Asset {
	w.t.Helper()
	a, err := assets.NewRepository().Create(w.t.Context(), w.d, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: symbol + "-" + uuid.NewString()[:8], Name: "operator console itest " + symbol, Decimals: 9,
		RiskClass: assets.RiskMajor, Status: assets.StatusActive,
	})
	require.NoError(w.t, err)
	return a
}

// hold gives a fresh customer a wallet balance of asset (SANDBOX_FUNDING).
func (w *opsWorld) hold(asset assets.Asset, units int64) {
	w.t.Helper()
	_, acct := seedAccount(w.t, w.d)
	qty := money.QuantityFromInt64(units)
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := w.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "itest-ops-hold:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset.ID), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset.ID), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

func (w *opsWorld) recordMark(p security.Principal, body map[string]any) *response {
	w.t.Helper()
	w.h.as(&p)
	return w.h.do(http.MethodPost, "/v1/advanced/ops/marks", body, "Idempotency-Key", "itest-ops-mark-"+uuid.NewString())
}

// TestIntegration_ReferenceMarksOverHTTP records sandbox reference marks
// through the real adapter: a mark of a SIMULATED asset is stored with its
// audit event and read back with its age; the same observation again writes
// nothing; and every observation no reader could use is refused with nothing
// stored.
func TestIntegration_ReferenceMarksOverHTTP(t *testing.T) {
	w := newOpsWorld(t, nil)
	op := w.operator()
	sol := w.simulated("XSOL")
	crypto := seedAsset(t, w.d)
	now := w.clk.Now()
	marksOf := func(asset assets.AssetID) int {
		t.Helper()
		var n int
		require.NoError(t, w.d.QueryRow(t.Context(), `SELECT count(*) FROM asset_prices WHERE asset_id = $1`, asset).Scan(&n))
		return n
	}

	res := w.recordMark(op, map[string]any{"asset_id": sol.ID.String(), "price": "2500.125", "source": "itest-desk"})
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var recorded api.AdvancedReferenceMarkRecorded
	res.json(&recorded)
	assert.False(t, recorded.Duplicate)
	assert.Equal(t, api.SIMULATED, recorded.ValueDomain)
	assert.Equal(t, "2500.125", recorded.Mark.Price)
	assert.Equal(t, "2500125", recorded.Mark.Mantissa)
	assert.Equal(t, 3, recorded.Mark.Scale)
	assert.Equal(t, econ.SandboxDollarID().String(), recorded.Mark.QuoteAssetId.String())
	assert.True(t, recorded.Mark.ObservedAt.Equal(now), "no observed_at is the time of the request")
	assert.Equal(t, int64(0), recorded.Mark.AgeMs)
	assert.Equal(t, econ.SandboxDollarSymbol, recorded.QuoteAssetSymbol)
	var audits int
	require.NoError(t, w.d.QueryRow(t.Context(), `SELECT count(*) FROM audit_events
		WHERE action = 'sandbox.reference_mark.recorded' AND resource_id = $1 AND actor_id = $2`,
		recorded.Mark.Id.String(), op.SubjectID).Scan(&audits))
	assert.Equal(t, 1, audits, "every recorded mark writes its audit event")

	// The same observation again: nothing new, no second audit event.
	same := map[string]any{"asset_id": sol.ID.String(), "price": "2500.125", "source": "itest-desk",
		"observed_at": now.Format(time.RFC3339Nano)}
	res = w.recordMark(op, same)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, true, res.raw()["duplicate"])
	require.NoError(t, w.d.QueryRow(t.Context(), `SELECT count(*) FROM audit_events
		WHERE action = 'sandbox.reference_mark.recorded' AND resource_id = $1`, recorded.Mark.Id.String()).Scan(&audits))
	assert.Equal(t, 1, audits)
	// A different price for the same observation is a provider inconsistency.
	same["price"] = "2500.126"
	res = w.recordMark(op, same)
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())

	// The oldest observation a reader still accepts is accepted.
	edge := now.Add(-5 * time.Minute)
	res = w.recordMark(op, map[string]any{"asset_id": sol.ID.String(), "price": "2400", "source": "itest-desk-2",
		"observed_at": edge.Format(time.RFC3339Nano)})
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	stored := marksOf(sol.ID)
	assert.Equal(t, 2, stored)

	for name, c := range map[string]struct {
		body   map[string]any
		status int
		code   errs.Code
	}{
		"not simulated": {map[string]any{"asset_id": crypto.ID.String(), "price": "1", "source": "s"}, http.StatusForbidden, errs.CodeForbidden},
		"unknown asset": {map[string]any{"asset_id": assets.NewAssetID().String(), "price": "1", "source": "s"}, http.StatusNotFound, errs.CodeNotFound},
		"the dollar":    {map[string]any{"asset_id": econ.SandboxDollarID().String(), "price": "1", "source": "s"}, http.StatusBadRequest, errs.CodeValidationFailed},
		"other quote": {map[string]any{"asset_id": sol.ID.String(), "quote_asset_id": crypto.ID.String(), "price": "1", "source": "s"},
			http.StatusBadRequest, errs.CodeValidationFailed},
		"zero":         {map[string]any{"asset_id": sol.ID.String(), "price": "0.000", "source": "s"}, http.StatusBadRequest, errs.CodeValidationFailed},
		"over-precise": {map[string]any{"asset_id": sol.ID.String(), "price": "1.0000000000000000001", "source": "s"}, http.StatusUnprocessableEntity, errs.CodePrecisionLoss},
		"future": {map[string]any{"asset_id": sol.ID.String(), "price": "1", "source": "s",
			"observed_at": now.Add(time.Second).Format(time.RFC3339Nano)}, http.StatusBadRequest, errs.CodeValidationFailed},
		"too old": {map[string]any{"asset_id": sol.ID.String(), "price": "1", "source": "s",
			"observed_at": edge.Add(-time.Millisecond).Format(time.RFC3339Nano)}, http.StatusBadRequest, errs.CodeValidationFailed},
	} {
		res = w.recordMark(op, c.body)
		require.Equal(t, c.status, res.Code, "%s body=%s", name, res.Body.String())
		assert.Equal(t, c.code, res.problem().Code, name)
	}
	assert.Equal(t, stored, marksOf(sol.ID), "a refusal stores nothing")
	assert.Zero(t, marksOf(crypto.ID))

	// Only an operator with a fresh step-up records one.
	_, acct := seedAccount(t, w.d)
	customer := security.Principal{SubjectID: uuid.NewString(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{acct.String()}, SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"}}
	res = w.recordMark(customer, map[string]any{"asset_id": sol.ID.String(), "price": "1", "source": "s"})
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	stale := op
	stale.AuthTime = time.Now().UTC().Add(-time.Hour)
	res = w.recordMark(stale, map[string]any{"asset_id": sol.ID.String(), "price": "1", "source": "s"})
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
	assert.Equal(t, stored, marksOf(sol.ID))

	// The list: newest observation first, with its age; one millisecond past
	// the maximum age a mark is stale, and an observation stamped in the
	// future (recorded around the route) is flagged as one every reader ignores.
	_, err := w.prices.RecordPrice(t.Context(), w.d, valuation.PriceObservation{AssetID: sol.ID, QuoteAssetID: econ.SandboxDollarID(),
		Mantissa: money.QuantityFromInt64(1), Scale: 0, Source: "skewed-clock", ObservedAt: now.Add(time.Hour)})
	require.NoError(t, err)
	w.clk.Advance(time.Millisecond)
	w.h.as(&op)
	res = w.h.do(http.MethodGet, "/v1/advanced/ops/marks?asset_id="+sol.ID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var list api.AdvancedReferenceMarkList
	res.json(&list)
	assert.Equal(t, api.SIMULATED, list.ValueDomain)
	assert.Equal(t, "SIMULATED", list.AssetKind)
	assert.Equal(t, int64(300_000), list.MaxAgeMs)
	require.Len(t, list.Items, 3)
	assert.True(t, list.Items[0].Future)
	assert.Equal(t, "2500.125", list.Items[1].Price)
	assert.Equal(t, int64(1), list.Items[1].AgeMs)
	assert.False(t, list.Items[1].Stale)
	assert.Equal(t, "2400", list.Items[2].Price)
	assert.True(t, list.Items[2].Stale, "five minutes and a millisecond old")

	// The chains: no host in this deployment.
	res = w.h.do(http.MethodGet, "/v1/advanced/ops/chains", nil)
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
}

// TestIntegration_TheOperatorOverviewReadsTheDatabase reads the overview over
// the real database: every held SIMULATED asset with the age of its latest
// mark (none at all is stale), one entry per advanced reconciliation kind,
// and no chain host.
func TestIntegration_TheOperatorOverviewReadsTheDatabase(t *testing.T) {
	w := newOpsWorld(t, nil)
	op := w.operator()
	marked, unmarked, unheld := w.simulated("XMARK"), w.simulated("XNONE"), w.simulated("XIDLE")
	w.hold(marked, 1_000_000_000)
	w.hold(unmarked, 1_000_000_000)
	res := w.recordMark(op, map[string]any{"asset_id": marked.ID.String(), "price": "42.5", "source": "itest-desk",
		"observed_at": w.clk.Now().Add(-time.Minute).Format(time.RFC3339Nano)})
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	res = w.recordMark(op, map[string]any{"asset_id": unheld.ID.String(), "price": "1", "source": "itest-desk"})
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())

	w.h.as(&op)
	res = w.h.do(http.MethodGet, "/v1/advanced/ops/overview", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var o api.AdvancedOpsOverview
	res.json(&o)
	assert.Equal(t, api.SIMULATED, o.ValueDomain)
	assert.Equal(t, "TEST", o.Environment)
	assert.Equal(t, int64(300_000), o.MaxMarkAgeMs)
	assert.False(t, o.ChainsConfigured)
	assert.NotNil(t, o.KillSwitches)
	assert.NotNil(t, o.StableAssets)
	assert.NotNil(t, o.BridgeRoutes)

	kinds := make([]string, 0, len(o.Reconciliation))
	for _, k := range o.Reconciliation {
		kinds = append(kinds, k.Kind)
	}
	assert.Equal(t, []string{"ECONOMIC_EFFECT", "STABLE_RESERVE", "BRIDGE", "PREDICTION_ESCROW", "CLEARING", "DEVNET_CHAIN",
		"BOOK_JOURNAL", "TRANSFERS"}, kinds)

	byAsset := map[string]api.AdvancedOpsOracleAsset{}
	for _, a := range o.Oracle {
		byAsset[a.AssetId.String()] = a
	}
	got, ok := byAsset[marked.ID.String()]
	require.True(t, ok, "a held asset with a mark is listed")
	require.NotNil(t, got.AgeMs)
	assert.Equal(t, int64(60_000), *got.AgeMs)
	assert.False(t, got.Stale)
	require.NotNil(t, got.Source)
	assert.Equal(t, "itest-desk", *got.Source)
	none, ok := byAsset[unmarked.ID.String()]
	require.True(t, ok, "a held asset without a mark is listed too")
	assert.Nil(t, none.ObservedAt)
	assert.True(t, none.Stale, "no mark is stale")
	_, listed := byAsset[unheld.ID.String()]
	assert.False(t, listed, "an asset nobody holds is not the oracle's concern")
	_, dollar := byAsset[econ.SandboxDollarID().String()]
	assert.False(t, dollar, "the quote has no mark of its own")

	// Customers never read it.
	_, acct := seedAccount(t, w.d)
	customer := security.Principal{SubjectID: accounts.NewUserID().String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{acct.String()}, SessionID: testSessionID,
		AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"}}
	w.h.as(&customer)
	res = w.h.do(http.MethodGet, "/v1/advanced/ops/overview", nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
}

// TestIntegration_TheChainsReadReportsTheDevnetHost: with a devnet host
// configured, the chains read reports each chain's tip as the host replays it
// and when blocks were last produced on it.
func TestIntegration_TheChainsReadReportsTheDevnetHost(t *testing.T) {
	d := openTestDB(t)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Millisecond))
	network := "ops-itest-" + uuid.NewString()[:8]
	host, err := devnethost.Open(t.Context(), devnethost.NewPgStore(d),
		devnethost.DefaultConfig(network, config.EnvTest, devnet.PublicKey{}), clk)
	require.NoError(t, err)
	produced, err := host.ProduceBlocks(t.Context(), devnethost.ChainSolana, 3)
	require.NoError(t, err)

	w := newOpsWorld(t, host)
	op := w.operator()
	w.h.as(&op)
	res := w.h.do(http.MethodGet, "/v1/advanced/ops/chains", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.AdvancedOpsChains
	res.json(&out)
	assert.Equal(t, network, out.Network)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	require.Len(t, out.Items, 2)
	tips := map[string]api.AdvancedOpsChainTip{}
	for _, c := range out.Items {
		tips[c.ChainId] = c
	}
	sol := tips[string(devnethost.ChainSolana)]
	assert.Equal(t, int64(produced.Height), sol.Height) // #nosec G115 -- a handful of test blocks
	assert.Equal(t, produced.Hash.String(), sol.Hash)
	require.NotNil(t, sol.LastBlockProducedAt, "the host produced blocks on this chain")
	eth := tips[string(devnethost.ChainEthereum)]
	assert.Nil(t, eth.LastBlockProducedAt, "and none on this one")

	res = w.h.do(http.MethodGet, "/v1/advanced/ops/overview", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, true, res.raw()["chains_configured"])
}

// TestIntegration_BridgeLegsInReviewOverHTTP drives the three ways out of a
// bridge leg's MANUAL_REVIEW over the real bridge service and the sandbox
// chain: a leg stuck behind a frozen destination mint waits in the console's
// pending items; it cannot be resolved as reconciled before it released; once
// the mint is unfrozen an operator resumes its relay and it completes; a leg
// whose destination released is never refunded; and a second stuck leg is
// refunded on its source chain exactly once.
func TestIntegration_BridgeLegsInReviewOverHTTP(t *testing.T) {
	w := newStableChainWorld(t)
	ctx := t.Context()
	user, acct := w.customer(0)
	w.serve(user, acct)
	cust := customerOf(user, acct)
	op := w.operator()
	controller, err := killswitch.NewController(w.clk, noopKillAudit{}, NewApprovalVerifier(admin.NewService(w.clk, audit.NewWriter())))
	require.NoError(t, err)
	ops, err := Wire(WireDeps{DB: w.d, Clock: w.clk, Env: config.EnvTest, KillSwitches: controller, Advanced: AdvancedDeps{
		Operator: OperatorDeps{Prices: w.prices, Audit: audit.NewWriter(), MaxMarkAge: 5 * time.Minute, Chains: w.chain.Host},
	}})
	require.NoError(t, err)
	w.h.server.opts.Ports.Operator = ops.Operator
	host := w.chain.Host

	// A source asset no stable asset controller reconciles (so it may be
	// refunded), minted on sbx-solana by a test minter.
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	minter := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("ops-itest-minter-" + tag)))
	mint := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("ops-itest-mint-" + tag))).PublicKey()
	require.NoError(t, host.EnsureMint(ctx, devnethost.ChainSolana, devnet.MintConfig{Address: mint, Decimals: 6, Authority: minter.PublicKey()}))
	src, err := assets.NewRepository().Create(ctx, w.d, assets.Asset{
		Chain: string(devnethost.ChainSolana), MintAddress: mint.String(), Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: "OPSB" + strings.ToUpper(tag[:4]), Name: "operator console itest bridge source", Decimals: 6,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	var route api.BridgeRoute
	w.post(op, "/v1/advanced/bridge/routes", map[string]any{
		"name": "ops-itest-" + tag, "source_asset_id": src.ID.String(), "destination_chain": "sbx-ethereum",
		"destination_symbol": "OPSE" + strings.ToUpper(tag[:4]), "unit_price": "1", "max_release_attempts": 1,
	}, w.key("route"), http.StatusOK, &route)
	destMint, err := devnet.ParsePublicKey(route.DestinationMint)
	require.NoError(t, err)
	twin := stableItestAssetID(t, route.DestinationAssetId)

	// The customer holds 1,000,000 units on the books and on the source chain.
	holder := w.chain.Keys.Custody.HolderAddress(acct)
	units := money.QuantityFromInt64(1_000_000)
	require.NoError(t, w.d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := w.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "ops-itest-bridge-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, src.ID), Side: ledger.Debit, Quantity: units},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, src.ID), Side: ledger.Credit, Quantity: units},
			},
		})
		return err
	}))
	require.NoError(t, host.EnsureNativeBalance(ctx, devnethost.ChainSolana, minter.PublicKey(), 1_000_000, 100_000_000))
	tip, err := host.Tip(ctx, devnethost.ChainSolana)
	require.NoError(t, err)
	signed, err := devnet.Sign(devnet.Message{ChainID: devnethost.ChainSolana, RecentBlockhash: tip.Hash, FeePayer: minter.PublicKey(),
		Instructions: []devnet.Instruction{devnet.TokenMintTo{Mint: mint, ToOwner: holder, Amount: 1_000_000, Authority: minter.PublicKey()}}}, minter)
	require.NoError(t, err)
	raw, err := signed.Encode()
	require.NoError(t, err)
	minted, err := host.Submit(ctx, devnethost.ChainSolana, raw)
	require.NoError(t, err)
	_, err = host.ProduceBlocks(ctx, devnethost.ChainSolana, devnet.DefaultFinalityDepth+1)
	require.NoError(t, err)
	st, err := host.Status(ctx, devnethost.ChainSolana, minted)
	require.NoError(t, err)
	require.Equal(t, devnet.TxFinalized, st.State, "the funding mint is final")

	// The destination mint is frozen: the leg's one release fails and it waits for an operator.
	require.NoError(t, host.SetMintFrozen(ctx, devnethost.ChainEthereum, destMint, true))
	bridgeLeg := func(quantity string) api.BridgeLegResult {
		t.Helper()
		var out api.BridgeLegResult
		w.post(cust, "/v1/advanced/bridge/legs", map[string]any{
			"account_id": acct.String(), "route_id": route.Id.String(), "quantity": quantity, "mode": "SANDBOX",
		}, w.key("leg"), http.StatusAccepted, &out)
		require.Equal(t, api.BridgeLegStatusManualReview, out.Leg.Status)
		return out
	}
	first := bridgeLeg("400000")
	legPath := "/v1/advanced/bridge/legs/" + first.Leg.Id.String()
	review := func(p security.Principal, verb, reason, key string) *response {
		t.Helper()
		w.h.as(&p)
		return w.h.do(http.MethodPost, legPath+"/"+verb, map[string]any{"reason": reason}, "Idempotency-Key", key)
	}
	inReview := func() map[string]api.AdvancedOpsBridgeLeg {
		t.Helper()
		var o api.AdvancedOpsOverview
		w.get(op, "/v1/advanced/ops/overview", &o)
		out := map[string]api.AdvancedOpsBridgeLeg{}
		for _, l := range o.BridgeLegsInReview {
			out[l.Id.String()] = l
		}
		return out
	}
	pending, ok := inReview()[first.Leg.Id.String()]
	require.True(t, ok, "the console lists the leg among the items waiting for an operator")
	assert.False(t, pending.Released)
	assert.False(t, pending.RefundRequested)
	assert.Equal(t, "400000", pending.Quantity)
	assert.Equal(t, route.Id, pending.RouteId)

	// Not released, so not resolvable as reconciled; a customer decides nothing.
	res := review(op, "resolve-review", "checked the chains", w.key("resolve-early"))
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidStateTransition, res.problem().Code)
	res = review(cust, "resume-relay", "mine", w.key("resume-customer"))
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	// Unfrozen, the operator resumes the relay; reconciling the leg completes it.
	require.NoError(t, host.SetMintFrozen(ctx, devnethost.ChainEthereum, destMint, false))
	resumeKey := w.key("resume")
	res = review(op, "resume-relay", "the destination mint is unfrozen", resumeKey)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var resumed api.BridgeLeg
	res.json(&resumed)
	assert.Equal(t, api.BridgeLegStatusMessageInFlight, resumed.Status)
	again := review(op, "resume-relay", "the destination mint is unfrozen", resumeKey)
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String(), "the same key replays the first answer")
	var done api.BridgeLegResult
	w.post(op, legPath+"/reconcile", nil, w.key("reconcile"), http.StatusOK, &done)
	assert.Equal(t, api.BridgeLegStatusReconciled, done.Leg.Status)
	assert.Equal(t, "400000", w.wallet(acct, twin), "the destination released")
	_, stillPending := inReview()[first.Leg.Id.String()]
	assert.False(t, stillPending)

	// A leg whose destination released is never refunded, and nothing moves.
	res = review(op, "refund", "refund it anyway", w.key("refund-released"))
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidStateTransition, res.problem().Code)
	assert.Zero(t, w.count(`SELECT count(*) FROM bridge_release_attempts WHERE leg_id = $1 AND kind = 'REFUND'`, first.Leg.Id),
		"no refund was recorded or submitted")
	assert.Zero(t, w.count(`SELECT count(*) FROM journal_transactions WHERE kind = 'BRIDGE_REFUND' AND reference_id = $1`, first.Leg.Id.String()))
	assert.Equal(t, "400000", w.wallet(acct, twin))
	assert.Equal(t, "600000", w.wallet(acct, src.ID))

	// A second leg sticks behind the frozen mint and is refunded on its source, once.
	require.NoError(t, host.SetMintFrozen(ctx, devnethost.ChainEthereum, destMint, true))
	second := bridgeLeg("250000")
	assert.Equal(t, "350000", w.wallet(acct, src.ID), "the lock left the wallet")
	legPath = "/v1/advanced/bridge/legs/" + second.Leg.Id.String()
	refundKey := w.key("refund")
	res = review(op, "refund", "the destination mint stays frozen", refundKey)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var refunded api.BridgeLegResult
	res.json(&refunded)
	assert.True(t, refunded.Settled)
	assert.Equal(t, api.BridgeLegStatusFailed, refunded.Leg.Status)
	assert.Equal(t, "600000", w.wallet(acct, src.ID), "the units are back in the source wallet")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM bridge_release_attempts WHERE leg_id = $1 AND kind = 'REFUND'`, second.Leg.Id))
	replayed := review(op, "refund", "the destination mint stays frozen", refundKey)
	require.Equal(t, http.StatusOK, replayed.Code, "body=%s", replayed.Body.String())
	assert.JSONEq(t, res.Body.String(), replayed.Body.String())
	res = review(op, "refund", "the same decision again", w.key("refund-again"))
	require.Equal(t, http.StatusOK, res.Code, "an already refunded leg changes nothing; body=%s", res.Body.String())
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM bridge_release_attempts WHERE leg_id = $1 AND kind = 'REFUND'`, second.Leg.Id),
		"a refund is never sent twice")
	res = review(op, "resume-relay", "relay it after all", w.key("resume-refunded"))
	require.Equal(t, http.StatusConflict, res.Code, "a refunded leg never relays; body=%s", res.Body.String())

	// The chains the console reports are the host's.
	var chains api.AdvancedOpsChains
	w.get(op, "/v1/advanced/ops/chains", &chains)
	assert.Equal(t, w.network, chains.Network)
	require.Len(t, chains.Items, 2)
}
