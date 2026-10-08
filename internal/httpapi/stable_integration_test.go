//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxchain"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/valuation"
)

// stableChainWorld is the sandbox stable asset and the bridge over the
// integration database, wired as cmd/api wires them: one sandbox chain opened
// through internal/sandboxchain on its own network, every dependency real, on
// one fake clock, and the ports built by Wire.
type stableChainWorld struct {
	t       *testing.T
	d       *db.DB
	clk     *clock.Fake
	ledger  *ledger.Service
	prices  *valuation.PriceStore
	chain   sandboxchain.Chain
	network string
	ports   Ports
	h       *harness
	keys    int
}

func newStableChainWorld(t *testing.T) *stableChainWorld {
	t.Helper()
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-stable-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	// The deployment's default legal policy, as cmd/api builds it.
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	led := ledger.NewService(clk, "httpapi-stable-itest")
	capSvc := capital.NewService(clk, capital.OutboxEmitter{Outbox: out, Clock: clk})
	prices := valuation.NewPriceStore(clk)
	intents, effects := econ.NewIntentStore(clk, out, aw), econ.NewEffectStore(clk, out, aw)
	authz := econ.NewAuthorizer(checker, kills)
	recon := reconciliation.NewRepository(clk, out, aw)

	// Its own network, so its chains hold this test's transactions only.
	network := "httpapi-sbx-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	chain, err := sandboxchain.Open(ctx, d, clk, config.EnvTest, network, sandboxchain.Seed(sha256.Sum256([]byte(network))))
	require.NoError(t, err)
	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	stableSvc, err := stableasset.New(stableasset.Deps{
		DB: d, Clock: clk, Host: chain.Host, Intents: intents, Effects: effects, Authorizer: authz, Facilities: econ.NewRegistry(),
		Ledger: led, Capital: capSvc, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Prices: prices,
		Kills: kills, Reconciliation: recon, Audit: aw, Accounts: accounts.NewRepository(), Assets: assets.NewRepository(),
		Issuer: chain.Keys.Issuer, Custody: chain.Keys.Custody, Snapshots: chain.Keys.Snapshots,
	})
	require.NoError(t, err)
	bridgeSvc, err := bridge.New(bridge.Deps{
		DB: d, Clock: clk, Host: chain.Host, Intents: intents, Effects: effects, Authorizer: authz, Facilities: econ.NewRegistry(),
		Ledger: led, Capital: capSvc, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Kills: kills,
		Reconciliation: recon, Audit: aw, Accounts: accounts.NewRepository(), Assets: assets.NewRepository(),
		Holder: chain.Keys.Custody, Attester: chain.Keys.Attester,
	})
	require.NoError(t, err)
	verification, err := identity.NewVerificationResolver(d)
	require.NoError(t, err)
	ports, err := Wire(WireDeps{
		DB: d, Clock: clk, Env: "TEST", Assets: assets.NewRepository(), Instruments: instruments.NewRepository(),
		Advanced: AdvancedDeps{
			Authorizer: authz, Intents: intents, Effects: effects, Risk: risk.NewStore(),
			// The facts are the deployment's: the production identity reader
			// (D-055) and verification resolver, no jurisdiction resolver.
			Stable: StableDeps{
				Service: stableSvc, Host: chain.Host, HolderAddress: chain.Keys.Custody.HolderAddress,
				SnapshotKey: chain.Keys.Snapshots.PublicKey(), Capital: capSvc,
				Identity: compliance.NewAccountVerification(clk), Verification: verification,
			},
			Bridge: BridgeDeps{
				Service: bridgeSvc, Host: chain.Host, Identity: compliance.NewAccountVerification(clk), Verification: verification,
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, ports.Stable, "the stable asset dependencies wire the port")
	require.NotNil(t, ports.Bridge, "the bridge dependencies wire the port")
	return &stableChainWorld{t: t, d: d, clk: clk, ledger: led, prices: prices, chain: chain, network: network, ports: ports}
}

// customer creates a user whose identity provider asserted an email address
// (so the account is NODAL_IDENTITY-verified, D-055) with one customer
// account, funded with whole sandbox dollars.
func (w *stableChainWorld) customer(dollars int64) (accounts.UserID, accounts.AccountID) {
	w.t.Helper()
	sum := sha256.Sum256([]byte(uuid.NewString()))
	u, err := accounts.NewRepository().CreateUser(w.t.Context(), w.d, "httpapi-stable-itest", "sub-"+uuid.NewString(), sum[:])
	require.NoError(w.t, err)
	a, err := accounts.NewRepository().CreateAccount(w.t.Context(), w.d, u.ID, accounts.KindCustomer)
	require.NoError(w.t, err)
	if dollars > 0 {
		amount := money.QuantityFromInt64(dollars * 1_000_000)
		ref := uuid.NewString()
		require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := w.ledger.Post(ctx, tx, ledger.Posting{
				Kind: ledger.KindSandboxFunding, IdempotencyKey: "stable-itest:" + ref,
				Reference: ledger.FinancialEventReference{Type: "stable_itest_funding", ID: ref}, EffectiveAt: w.clk.Now(),
				Entries: []ledger.Entry{
					{Account: ledger.CustomerAccount(a.ID, ledger.CodeWallet, econ.SandboxDollarID()), Side: ledger.Debit, Quantity: amount},
					{Account: ledger.CustomerAccount(a.ID, ledger.CodeCapital, econ.SandboxDollarID()), Side: ledger.Credit, Quantity: amount},
				},
			})
			return err
		}))
	}
	return u.ID, a.ID
}

// serve puts the world's ports behind the integration router (the real
// idempotency store), acting first as the given customer.
func (w *stableChainWorld) serve(user accounts.UserID, acct accounts.AccountID) *harness {
	w.t.Helper()
	w.h = newIntegrationHarness(w.t, w.d, acct, user)
	w.h.server.opts.Ports.Stable = w.ports.Stable
	w.h.server.opts.Ports.Bridge = w.ports.Bridge
	return w.h
}

// operator is a human operator holding sandbox:operate who authenticated
// strongly just now.
func (w *stableChainWorld) operator() security.Principal {
	w.t.Helper()
	u, _ := w.customer(0)
	return security.Principal{
		SubjectID: u.String(), ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin},
		SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
}

func (w *stableChainWorld) key(label string) string {
	w.keys++
	return "stable-itest-" + label + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10] + "-" + itoa(w.keys)
}

// post sends a command as p and decodes a 2xx answer into out.
func (w *stableChainWorld) post(p security.Principal, path string, body any, key string, wantStatus int, out any) *response {
	w.t.Helper()
	w.h.as(&p)
	res := w.h.do(http.MethodPost, path, body, "Idempotency-Key", key)
	require.Equal(w.t, wantStatus, res.Code, "POST %s body=%s", path, res.Body.String())
	if out != nil {
		res.json(out)
	}
	return res
}

// get reads as p and decodes a 200 answer into out.
func (w *stableChainWorld) get(p security.Principal, path string, out any) {
	w.t.Helper()
	w.h.as(&p)
	res := w.h.do(http.MethodGet, path, nil)
	require.Equal(w.t, http.StatusOK, res.Code, "GET %s body=%s", path, res.Body.String())
	res.json(out)
}

// register has op register a fully reserved 6-decimal stable asset pegged at
// one sandbox dollar and reserved in the sandbox dollar itself.
func (w *stableChainWorld) register(op security.Principal, oracle string) api.StableAsset {
	w.t.Helper()
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	var detail api.StableAssetDetail
	w.post(op, "/v1/advanced/stable-assets", map[string]any{
		"name": "itest-dollar-" + tag, "symbol": "SBXT" + strings.ToUpper(tag[:5]), "peg_currency": "USD", "decimals": 6,
		"issuance_cap": "1000000000000", "redemption_mode": "INSTANT", "peg": "1.000000", "oracle_source": oracle,
		"max_price_age_ms": 3_600_000, "peg_deviation_threshold_bps": 50, "attestable": true,
		"reserves": []map[string]any{
			{"asset_id": econ.SandboxDollarID().String(), "kind": "CASH_EQUIVALENT", "factor_bps": 10000, "accepted_for_mint": true},
		},
	}, w.key("register"), http.StatusOK, &detail)
	require.Equal(w.t, api.StableAssetStatusActive, detail.Asset.Status)
	require.Equal(w.t, string(stableasset.MintAuthorityPlatformSigner), detail.Asset.MintAuthorityMode)
	require.Equal(w.t, w.chain.Keys.Issuer.PublicKey().String(), detail.Asset.MintAuthority, "the platform issuer alone can mint")
	return detail.Asset
}

// wallet is an account's WALLET balance of an asset, on the books.
func (w *stableChainWorld) wallet(acct accounts.AccountID, asset assets.AssetID) string {
	w.t.Helper()
	b, err := w.ledger.Balance(w.t.Context(), w.d, ledger.CustomerAccount(acct, ledger.CodeWallet, asset))
	require.NoError(w.t, err)
	return b.String()
}

// facility is a facility's balance under one code and asset, on the books.
func (w *stableChainWorld) facility(facilityID string, code ledger.Code, asset assets.AssetID) string {
	w.t.Helper()
	b, err := w.ledger.Balance(w.t.Context(), w.d, ledger.AccountRef{OwnerType: ledger.OwnerFacility, OwnerID: facilityID, Code: code, AssetID: asset})
	require.NoError(w.t, err)
	return b.String()
}

func (w *stableChainWorld) count(sql string, args ...any) int {
	w.t.Helper()
	var n int
	require.NoError(w.t, w.d.QueryRow(w.t.Context(), sql, args...).Scan(&n))
	return n
}

// submissions counts the SUBMIT events the host journaled for a transaction:
// the proof that it was, or was not, submitted again.
func (w *stableChainWorld) submissions(txID string) int {
	return w.count(`SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`, w.network, txID)
}

func stableItestAssetID(t *testing.T, u api.UUID) assets.AssetID {
	t.Helper()
	aid, err := assets.ParseAssetID(u.String())
	require.NoError(t, err)
	return aid
}

func stableItestMint(t *testing.T, a api.StableAsset) devnet.PublicKey {
	t.Helper()
	m, err := devnet.ParsePublicKey(a.Mint)
	require.NoError(t, err)
	return m
}

// TestIntegration_StableUnknownMintIsReconciledExactlyOnceOverHTTP: an
// operator arms TimeoutButLand on sbx-solana, so the next submission times out
// although the chain keeps it. The customer's issuance comes back 202 in
// SUBMISSION_UNKNOWN with its effect UNKNOWN -- neither issued nor failed --
// and nothing posted. Settling unknowns before the transaction is final
// changes nothing; once the operator has produced the blocks, it posts the
// issuance exactly once: one submission, one STABLE_ISSUANCE journal, the
// effect APPLIED, the intent RECONCILED, the request ISSUED with its
// reconciliation RESOLVED. Replaying the original command answers what it
// answered first and submits nothing.
func TestIntegration_StableUnknownMintIsReconciledExactlyOnceOverHTTP(t *testing.T) {
	w := newStableChainWorld(t)
	user, acct := w.customer(1_000)
	w.serve(user, acct)
	cust := customerOf(user, acct)
	op := w.operator()
	asset := w.register(op, "itest-oracle")
	assetPath := "/v1/advanced/stable-assets/" + asset.Id.String()
	units := stableItestAssetID(t, asset.AssetId)

	var fault api.BridgeFaultResult
	w.post(op, "/v1/advanced/bridge/chains/sbx-solana/faults", map[string]any{"kind": "TIMEOUT_BUT_LAND"}, w.key("fault"), http.StatusOK, &fault)
	assert.Equal(t, api.BridgeFaultKindTimeoutButLand, fault.Kind)

	issueKey := w.key("issue")
	issueBody := map[string]any{"account_id": acct.String(), "quantity": "250000000", "reserve_asset_id": econ.SandboxDollarID().String(), "mode": "SANDBOX"}
	var first api.StableIssuanceResult
	firstRes := w.post(cust, assetPath+"/issuances", issueBody, issueKey, http.StatusAccepted, &first)
	is := first.Issuance
	assert.False(t, first.Settled)
	assert.Equal(t, api.StableIssuanceStatusSubmissionUnknown, is.Status, "an unknown outcome is shown as unknown")
	assert.Equal(t, api.StableReconciliationAwaiting, is.Reconciliation.State)
	assert.Equal(t, api.StableEffectStatusUnknown, is.Reconciliation.EffectStatus)
	assert.NotNil(t, is.Reconciliation.UnknownSince)
	assert.Nil(t, is.JournalTransactionId, "nothing is posted for an unknown outcome")
	assert.Equal(t, "250000000", is.ReserveQuantity)
	assert.Equal(t, 1, w.submissions(is.TxId))
	assert.Equal(t, "1000000000", w.wallet(acct, econ.SandboxDollarID()), "the payment is only reserved")
	assert.Equal(t, "0", w.wallet(acct, units))

	var reserve api.StableReserve
	w.get(cust, assetPath+"/reserve", &reserve)
	assert.Equal(t, "0", reserve.IssuedSupply)
	assert.Equal(t, "250000000", reserve.PendingMint, "the unknown issuance is reserved, not issued")

	// Not final yet: settling unknowns looks, waits and resubmits nothing.
	var pass api.StableSettleReport
	w.post(op, "/v1/advanced/stable-assets/ops/resolve-unknown", nil, w.key("resolve"), http.StatusOK, &pass)
	assert.Equal(t, 1, pass.Examined)
	assert.Equal(t, 1, pass.Waiting)
	assert.Zero(t, pass.Issued)
	var still api.StableIssuance
	w.get(cust, "/v1/advanced/stable-assets/issuances/"+is.Id.String(), &still)
	assert.Equal(t, api.StableIssuanceStatusSubmissionUnknown, still.Status)

	var blocks api.BridgeBlocksResult
	w.post(op, "/v1/advanced/bridge/chains/sbx-solana/blocks", map[string]any{"count": 40}, w.key("blocks"), http.StatusOK, &blocks)
	assert.Equal(t, 40, blocks.Produced)
	assert.GreaterOrEqual(t, blocks.Chain.FinalizedHeight, int64(1))

	w.post(op, "/v1/advanced/stable-assets/ops/resolve-unknown", nil, w.key("resolve"), http.StatusOK, &pass)
	assert.Equal(t, 1, pass.Examined)
	assert.Equal(t, 1, pass.Issued, "the landed issuance is settled from the chain")

	var settled api.StableIssuance
	w.get(cust, "/v1/advanced/stable-assets/issuances/"+is.Id.String(), &settled)
	assert.Equal(t, api.StableIssuanceStatusIssued, settled.Status)
	assert.Equal(t, api.StableReconciliationResolved, settled.Reconciliation.State)
	assert.Equal(t, api.StableEffectStatusApplied, settled.Reconciliation.EffectStatus)
	require.NotNil(t, settled.Reconciliation.UnknownSince)
	require.NotNil(t, settled.Reconciliation.ResolvedAt)
	require.NotNil(t, settled.JournalTransactionId)
	require.NotNil(t, settled.FinalSlot)

	// Exactly once, everywhere it could have happened twice.
	w.post(op, "/v1/advanced/stable-assets/ops/resolve-unknown", nil, w.key("resolve"), http.StatusOK, &pass)
	assert.Zero(t, pass.Examined, "nothing is left to settle")
	again := w.post(cust, assetPath+"/issuances", issueBody, issueKey, http.StatusAccepted, nil)
	assert.JSONEq(t, firstRes.Body.String(), again.Body.String(), "a replay answers exactly what the first call answered")
	assert.Equal(t, 1, w.submissions(is.TxId), "the transaction was submitted once and never again")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM journal_transactions WHERE reference_type = 'stable_mint_request' AND reference_id = $1`,
		is.Id.String()), "one issuance journal")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM stable_mint_requests WHERE account_id = $1`, acct))
	var intentStatus string
	require.NoError(t, w.d.QueryRow(t.Context(), `SELECT status FROM economic_intents WHERE id = $1`, is.IntentId).Scan(&intentStatus))
	assert.Equal(t, string(econ.StatusReconciled), intentStatus)

	// The books, the wallets and the chain agree to the base unit.
	assert.Equal(t, "250000000", w.wallet(acct, units))
	assert.Equal(t, "750000000", w.wallet(acct, econ.SandboxDollarID()))
	facility := asset.FacilityId.String()
	assert.Equal(t, "250000000", w.facility(facility, ledger.CodeStableReserve, econ.SandboxDollarID()))
	assert.Equal(t, "250000000", w.facility(facility, ledger.CodeStableOutstanding, units))
	supply, err := w.chain.Host.Supply(t.Context(), devnet.ChainID(asset.Chain), stableItestMint(t, asset))
	require.NoError(t, err)
	assert.Equal(t, uint64(250_000_000), supply)
	var recon api.StableSupplyReconciliation
	w.post(op, assetPath+"/reconciliations", nil, w.key("reconcile"), http.StatusOK, &recon)
	assert.True(t, recon.Matched, "%+v", recon)
	assert.Equal(t, "250000000", recon.ChainSupply)
	assert.Equal(t, "250000000", recon.CustomerWalletSum)
	assert.Equal(t, "0", recon.HeldElsewhere)

	// The fault was one-shot: the next issuance is ordinary and final.
	var next api.StableIssuanceResult
	issueBody["quantity"] = "1000000"
	w.post(cust, assetPath+"/issuances", issueBody, w.key("issue"), http.StatusOK, &next)
	assert.True(t, next.Settled)
	assert.Equal(t, api.StableReconciliationNotRequired, next.Issuance.Reconciliation.State)
	raw, err := json.Marshal(next)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"value_domain":"SIMULATED"`)
}
