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
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxchain"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Delegated agent authority end to end: the real delegation service over the
// real database and a simulated chain of the test's own, every human and
// operator step over HTTP, and the agent's step in-process -- the delegation
// service called under the agent's own principal, exactly as the agent
// runtime calls it. No route of the HTTP surface admits an agent.

// The test chain is fast: a 12-block blockhash window, finality after 2.
const (
	dgItWindow = 12
	dgItDepth  = 2
	dgItFee    = 10
)

// dgWorld is one delegation deployment: its own network, venue, tokens and
// house book, so tests never see one another's books.
type dgWorld struct {
	t        *testing.T
	ctx      context.Context
	d        *db.DB
	clk      clock.Clock
	tag      string
	host     *devnethost.Host
	svc      *delegation.Service
	signer   *delegation.DelegatedSigner
	venue    delegation.Venue
	ledger   *ledger.Service
	prices   *valuation.PriceStore
	mintAuth devnet.Keypair
	usd      assets.Asset
	tok      assets.Asset
	port     DelegationPort
}

func newDgWorld(t *testing.T) *dgWorld {
	t.Helper()
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.System()
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]

	// The keys derive from one seed exactly as cmd/api derives them.
	seed := sandboxchain.Seed(sha256.Sum256([]byte("httpapi-delegation-itest/" + tag)))
	cfg := devnethost.DefaultConfig("httpapi-dg-"+tag, config.EnvTest, sandboxchain.DeriveKeys(seed).Attester.PublicKey())
	for i := range cfg.Chains {
		cfg.Chains[i].BlockhashWindow, cfg.Chains[i].FinalityDepth, cfg.Chains[i].FeePerTx = dgItWindow, dgItDepth, dgItFee
	}
	host, err := devnethost.Open(ctx, devnethost.NewPgStore(d), cfg, clk)
	require.NoError(t, err)
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-delegation-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	signer, err := delegation.NewDelegatedSigner(seed.Derive(sandboxchain.PurposeDelegatedSigner), delegation.SignerConfig{
		Environment: config.EnvTest, Clock: clk, Policy: delegation.DefaultSigningPolicy(), Fees: delegation.HostFees(host)})
	require.NoError(t, err)
	venue := DelegationSandboxVenue{Name: "dg-" + tag, Chain: devnethost.ChainSolana, SpreadBPS: 50}
	coSigner := delegation.NewVenueSigner(seed.VenueSeed(venue.Name))

	w := &dgWorld{t: t, ctx: ctx, d: d, clk: clk, tag: tag, host: host, signer: signer,
		ledger: ledger.NewService(clk, "httpapi-delegation-itest"), prices: valuation.NewPriceStore(clk),
		mintAuth: devnet.NewKeypairFromSeed(sha256.Sum256([]byte("httpapi-delegation-itest/mint-authority/" + tag)))}
	w.svc, err = delegation.New(delegation.Deps{
		DB: d, Clock: clk, Environment: config.EnvTest, Host: host,
		Intents: econ.NewIntentStore(clk, out, aw), Effects: econ.NewEffectStore(clk, out, aw),
		Authorizer: econ.NewAuthorizer(checker, kills), Kills: kills, Ledger: w.ledger,
		Capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: out, Clock: clk}), Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Prices: w.prices, Identity: compliance.NewAccountVerification(clk),
		Profiles: compliance.NewRepository(aw), Reconciliation: reconciliation.NewRepository(clk, out, aw), Audit: aw,
		Accounts: accounts.NewRepository(), Assets: assets.NewRepository(), Signer: signer,
		Venues: []*delegation.VenueSigner{coSigner},
	})
	require.NoError(t, err)
	deps := DelegationDeps{Service: w.svc,
		CoSigners: []DelegationCoSigner{{Name: venue.Name, Chain: venue.Chain, Address: coSigner.PublicKey()}}}

	// The startup registration, twice: the second finds the first.
	w.venue, err = EnsureDelegationVenue(ctx, d, deps, venue)
	require.NoError(t, err)
	again, err := EnsureDelegationVenue(ctx, d, deps, venue)
	require.NoError(t, err)
	require.Equal(t, w.venue.ID, again.ID, "the startup registration is idempotent")
	require.Equal(t, coSigner.PublicKey(), w.venue.Address, "the venue settles at its co-signer's address, never another")

	ports, err := Wire(WireDeps{DB: d, Clock: clk, Env: "TEST", Advanced: AdvancedDeps{Delegation: deps}})
	require.NoError(t, err)
	require.NotNil(t, ports.Delegation, "a service wires the delegation port")
	w.port = ports.Delegation

	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	// Two simulated tokens of the test's own on sbx-solana: a USD-pegged unit
	// and a token marked at 100.00 sandbox dollars.
	require.NoError(t, host.Airdrop(ctx, devnethost.ChainSolana, w.mintAuth.PublicKey(), 1_000_000_000))
	usdMint := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("httpapi-delegation-itest/usd/" + tag))).PublicKey()
	tokMint := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("httpapi-delegation-itest/tok/" + tag))).PublicKey()
	require.NoError(t, host.EnsureMint(ctx, devnethost.ChainSolana, devnet.MintConfig{Address: usdMint, Decimals: 6, Authority: w.mintAuth.PublicKey()}))
	require.NoError(t, host.EnsureMint(ctx, devnethost.ChainSolana, devnet.MintConfig{Address: tokMint, Decimals: 9, Authority: w.mintAuth.PublicKey()}))
	w.usd = w.asset("DU"+strings.ToUpper(tag[:6]), usdMint, 6, true, assets.RiskSettlement)
	w.tok = w.asset("DT"+strings.ToUpper(tag[:6]), tokMint, 9, false, assets.RiskMajor)
	w.priceTok()

	// The venue's inventory, in its house book and on chain.
	w.fundHouse(w.tok, 1_000_000_000_000)
	w.fundHouse(w.usd, 1_000_000_000)
	w.mint(dgMint{w.venue.Address, w.tok, 1_000_000_000_000}, dgMint{w.venue.Address, w.usd, 1_000_000_000})
	return w
}

func (w *dgWorld) asset(symbol string, mint devnet.PublicKey, decimals uint8, stable bool, class assets.RiskClass) assets.Asset {
	w.t.Helper()
	a := assets.Asset{Chain: string(devnethost.ChainSolana), MintAddress: mint.String(), Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: symbol, Name: symbol + " (simulated devnet token)", Decimals: decimals,
		RiskClass: class, Status: assets.StatusActive}
	if stable {
		a.IsStablecoin, a.PegCurrency = true, valuation.USDPeg
	}
	out, err := assets.NewRepository().Create(w.ctx, w.d, a)
	require.NoError(w.t, err)
	return out
}

// priceTok marks the token at 100.00 sandbox dollars, observed now. The
// service accepts a mark no older than a minute.
func (w *dgWorld) priceTok() {
	w.t.Helper()
	_, err := w.prices.RecordPrice(w.ctx, w.d, valuation.PriceObservation{AssetID: w.tok.ID, QuoteAssetID: econ.SandboxDollarID(),
		Mantissa: money.QuantityFromInt64(10_000), Scale: 2, Source: "httpapi-delegation-itest", ObservedAt: w.clk.Now()})
	require.NoError(w.t, err)
}

func (w *dgWorld) post(p ledger.Posting) {
	w.t.Helper()
	require.NoError(w.t, w.d.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.ledger.Post(ctx, tx, p)
		return err
	}))
}

func (w *dgWorld) fundHouse(a assets.Asset, q int64) {
	w.t.Helper()
	house := econ.HouseAccount{ID: w.venue.HouseAccountID}
	ref := uuid.NewString()
	qty := money.QuantityFromInt64(q)
	w.post(ledger.Posting{Kind: ledger.KindHouseFunding, IdempotencyKey: "dg-house:" + ref,
		Reference: ledger.FinancialEventReference{Type: "house_funding", ID: ref}, EffectiveAt: w.clk.Now(),
		Entries: []ledger.Entry{
			{Account: house.Account(ledger.CodeHouseInventory, a.ID), Side: ledger.Debit, Quantity: qty},
			{Account: house.Account(ledger.CodeHouseCapital, a.ID), Side: ledger.Credit, Quantity: qty},
		}})
}

// dgMint is one issuance of a test token on chain.
type dgMint struct {
	owner  devnet.PublicKey
	asset  assets.Asset
	amount uint64
}

// mint issues each amount on chain and waits for finality.
func (w *dgWorld) mint(mints ...dgMint) {
	w.t.Helper()
	bh, err := w.host.LatestBlockhash(w.ctx, devnethost.ChainSolana)
	require.NoError(w.t, err)
	var ids []devnet.TxID
	for _, m := range mints {
		mint, err := devnet.ParsePublicKey(m.asset.MintAddress)
		require.NoError(w.t, err)
		tx, err := devnet.Sign(devnet.Message{ChainID: devnethost.ChainSolana, RecentBlockhash: bh, FeePayer: w.mintAuth.PublicKey(),
			Instructions: []devnet.Instruction{
				devnet.TokenMintTo{Mint: mint, ToOwner: m.owner, Amount: m.amount, Authority: w.mintAuth.PublicKey()},
				devnet.Memo{Data: []byte(uuid.NewString())},
			}}, w.mintAuth)
		require.NoError(w.t, err)
		raw, err := tx.Encode()
		require.NoError(w.t, err)
		txID, err := w.host.Submit(w.ctx, devnethost.ChainSolana, raw)
		require.NoError(w.t, err)
		ids = append(ids, txID)
	}
	_, err = w.host.ProduceBlocks(w.ctx, devnethost.ChainSolana, 1+dgItDepth)
	require.NoError(w.t, err)
	for _, txID := range ids {
		st, err := w.host.Status(w.ctx, devnethost.ChainSolana, txID)
		require.NoError(w.t, err)
		require.Equal(w.t, devnet.TxFinalized, st.State, "mint %s", st.Err)
	}
}

func (w *dgWorld) onChain(owner devnet.PublicKey, a assets.Asset) uint64 {
	w.t.Helper()
	mint, err := devnet.ParsePublicKey(a.MintAddress)
	require.NoError(w.t, err)
	b, err := w.host.TokenBalance(w.ctx, devnethost.ChainSolana, owner, mint)
	require.NoError(w.t, err)
	return b
}

func (w *dgWorld) booked(acct accounts.AccountID, code ledger.Code, a assets.Asset) money.Quantity {
	w.t.Helper()
	b, err := w.ledger.Balance(w.ctx, w.d, ledger.CustomerAccount(acct, code, a.ID))
	require.NoError(w.t, err)
	return b
}

// dgCustomer is a verified customer with one account, a strategy and an
// agent for it, and the HTTP session of the account's owner.
type dgCustomer struct {
	user    accounts.UserID
	account accounts.AccountID
	agentID string
	wallet  devnet.PublicKey
	h       *harness
	owner   security.Principal
}

func (w *dgWorld) customer(usdUnits int64) dgCustomer {
	w.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(w.ctx, w.d, "httpapi-delegation-itest", uuid.NewString(), nil)
	require.NoError(w.t, err)
	a, err := repo.CreateAccount(w.ctx, w.d, u.ID, accounts.KindCustomer)
	require.NoError(w.t, err)
	verifiedAt := w.clk.Now().Add(-24 * time.Hour)
	strategyID, versionID, agentID := id.New[id.Any](), id.New[id.Any](), id.New[id.Any]()
	hash := sha256.Sum256([]byte(versionID.String()))
	require.NoError(w.t, w.d.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := compliance.NewRepository(audit.NewWriterWithBuildVersion("httpapi-delegation-itest")).Upsert(ctx, tx, compliance.Profile{
			UserID: u.ID, IdentityState: compliance.IdentityVerified, SanctionsState: compliance.SanctionsClear, VerifiedAt: &verifiedAt,
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: "httpapi-delegation-idv", Reason: "fixture"}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO strategies (id, owner_account_id, owner_user_id, name, source_kind, status, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, 'TYPESCRIPT_SDK', 'ACTIVE', 'USER', $5)`, strategyID, a.ID, u.ID, "strategy-"+strategyID.String(), u.ID.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash, effect_set, status, source_kind, source_hash,
				compiler_version, risk_policy_version, risk_policy_hash, model_budget, data_budget, envelope_requirements, human_readable, built_at)
			VALUES ($1, $2, 1, 1, '{}'::jsonb, $3, '{}', 'COMPILED', 'TYPESCRIPT_SDK', $3, 'test', 'risk-v1', $3, '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'test strategy', $4)`,
			versionID, strategyID, hash[:], w.clk.Now()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO agents (id, account_id, strategy_id, strategy_version_id, name, stage, state, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, $5, 'VALIDATED', 'VALIDATED', 'USER', $6)`, agentID, a.ID, strategyID, versionID, "agent-"+agentID.String(), u.ID.String())
		return err
	}))
	c := dgCustomer{user: u.ID, account: a.ID, agentID: agentID.String(), wallet: w.signer.PublicKey(a.ID, devnethost.ChainSolana)}
	c.h = newIntegrationHarness(w.t, w.d, a.ID, u.ID)
	c.h.server.opts.Ports.Delegation = w.port
	c.owner = *c.h.princip
	if usdUnits > 0 {
		ref := uuid.NewString()
		qty := money.QuantityFromInt64(usdUnits)
		w.post(ledger.Posting{Kind: ledger.KindSandboxFunding, IdempotencyKey: "dg-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(a.ID, ledger.CodeWallet, w.usd.ID), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(a.ID, ledger.CodeCapital, w.usd.ID), Side: ledger.Credit, Quantity: qty},
			}})
		w.mint(dgMint{c.wallet, w.usd, uint64(usdUnits)})
	}
	return c
}

// agent is the customer's agent's principal: what the agent runtime acts
// under. It never reaches HTTP.
func (w *dgWorld) agent(c dgCustomer) context.Context {
	return security.WithPrincipal(w.ctx, security.AgentPrincipal(c.agentID, c.account.String()))
}

// dgOperator is an operations principal with a fresh strong authentication.
func dgOperator() security.Principal {
	return security.Principal{SubjectID: uuid.NewString(), ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleOperations}, SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"}}
}

func dgKey(prefix string) string { return prefix + "-" + uuid.NewString() }

// grant grants the customer's agent a capability over HTTP: generous sandbox
// limits, the venue as its only destination, no approval, one day.
func (w *dgWorld) grant(c dgCustomer, mutate func(body map[string]any)) api.DelegationCapability {
	w.t.Helper()
	body := map[string]any{
		"account_id": c.account.String(), "agent_id": c.agentID, "mode": "SANDBOX",
		"instrument_classes": []string{"STABLE_VALUE", "SPOT_TOKEN"},
		"action_classes":     []string{"ACQUIRE_EXPOSURE", "REDUCE_EXPOSURE", "PROPOSE_WITHDRAWAL"},
		"max_notional_usd":   "10000.00", "max_daily_notional_usd": "5000.00", "max_single_trade_usd": "1000.00",
		"destination_allowlist": []string{w.venue.Address.String()}, "chain_allowlist": []string{"sbx-solana"},
		"expires_at":              w.clk.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano),
		"requires_human_approval": false,
	}
	if mutate != nil {
		mutate(body)
	}
	res := c.h.do(http.MethodPost, "/v1/advanced/delegation/capabilities", body, "Idempotency-Key", dgKey("dg-grant"))
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.DelegationCapability
	res.json(&out)
	require.Equal(w.t, api.DelegationCapabilityActive, out.Status)
	require.Equal(w.t, api.DelegationStandingActive, out.Standing)
	return out
}

func (w *dgWorld) setPolicy(c dgCustomer, mutate func(body map[string]any)) api.DelegationWithdrawalPolicy {
	w.t.Helper()
	body := map[string]any{
		"account_id": c.account.String(), "auto_approve_enabled": false, "auto_approve_max_usd": "0.00",
		"auto_approve_max_risk_score": 0, "max_risk_score": 80, "per_withdrawal_limit_usd": "250.00", "daily_limit_usd": "1000.00",
		"velocity_max_count": 10, "min_destination_age_seconds": 0, "min_account_age_seconds": 0, "cooling_period_seconds": 0,
		"strong_auth_required": true, "strong_auth_max_age_seconds": 900,
	}
	if mutate != nil {
		mutate(body)
	}
	res := c.h.do(http.MethodPost, "/v1/advanced/delegation/withdrawal-policy", body, "Idempotency-Key", dgKey("dg-policy"))
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.DelegationWithdrawalPolicy
	res.json(&out)
	return out
}

func (w *dgWorld) addDestination(c dgCustomer, address devnet.PublicKey, label string) api.DelegationDestination {
	w.t.Helper()
	res := c.h.do(http.MethodPost, "/v1/advanced/delegation/destinations", map[string]any{"account_id": c.account.String(),
		"chain": "sbx-solana", "address": address.String(), "label": label}, "Idempotency-Key", dgKey("dg-destination"))
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.DelegationDestination
	res.json(&out)
	require.Equal(w.t, api.DelegationDestinationActive, out.Status)
	return out
}

// requestSwap is the agent asking, in-process, to pay amountIn base units of
// the sandbox-dollar token for the token at the venue. A refusal is recorded
// (committed) and returned as the error.
func (w *dgWorld) requestSwap(c dgCustomer, capability api.UUID, nonce uint64, amountIn int64) (delegation.SwapTicket, error) {
	w.t.Helper()
	w.priceTok()
	cid, err := delegation.ParseCapabilityID(capability.String())
	require.NoError(w.t, err)
	req := delegation.SwapRequest{CapabilityID: cid, AccountID: c.account, Nonce: nonce, Action: delegation.ActionAcquireExposure,
		VenueID: w.venue.ID, PayAssetID: w.usd.ID, ReceiveAssetID: w.tok.ID, AmountIn: money.QuantityFromInt64(amountIn),
		MaxSlippageBPS: 100, Jurisdiction: "US-CA", CorrelationID: "corr-" + uuid.NewString()}
	var ticket delegation.SwapTicket
	if err := w.d.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(_ context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = w.svc.RequestSwap(w.agent(c), tx, req)
		return err
	}); err != nil {
		return delegation.SwapTicket{}, err
	}
	return ticket, ticket.Refusal
}

// proposeWithdrawal is the agent proposing, in-process, a withdrawal of
// amount base units of the sandbox-dollar token to a destination.
func (w *dgWorld) proposeWithdrawal(c dgCustomer, capability, destination api.UUID, nonce uint64, amount int64) (delegation.WithdrawalTicket, error) {
	w.t.Helper()
	cid, err := delegation.ParseCapabilityID(capability.String())
	require.NoError(w.t, err)
	did, err := delegation.ParseDestinationID(destination.String())
	require.NoError(w.t, err)
	req := delegation.ProposalRequest{CapabilityID: cid, AccountID: c.account, Nonce: nonce, DestinationID: did, AssetID: w.usd.ID,
		Amount: money.QuantityFromInt64(amount), Jurisdiction: "US-CA", CorrelationID: "corr-" + uuid.NewString()}
	var ticket delegation.WithdrawalTicket
	if err := w.d.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(_ context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = w.svc.ProposeWithdrawal(w.agent(c), tx, req)
		return err
	}); err != nil {
		return delegation.WithdrawalTicket{}, err
	}
	return ticket, ticket.Refusal
}

func dgRequireRefusal(t *testing.T, err error, code errs.Code, reason string) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, code, errs.CodeOf(err), "%v", err)
	if reason != "" {
		e, ok := errs.As(err)
		require.True(t, ok, "%v", err)
		got, _ := e.Fields["reason"].(string)
		assert.True(t, got == reason || strings.Contains(err.Error(), reason), "want %s in %v", reason, err)
	}
}

func dgStatuses(trail []api.DelegationTransition) []string {
	out := make([]string, 0, len(trail))
	for _, tr := range trail {
		out = append(out, tr.ToStatus)
	}
	return out
}

// An agent asks for a swap within its capability; nothing needs the owner, so
// the operator's execution run takes it: the signing provider inspects the
// compiled transaction against the plan, the wallet and the venue sign, the
// chain finalizes it, and the books and the chain move by exactly the plan.
func TestIntegration_DelegationSwapWithinScopeIsSignedAfterInspectionAndSettles(t *testing.T) {
	w := newDgWorld(t)
	c := w.customer(1_000_000_000) // 1,000 sandbox dollars
	acct := c.account.String()

	res := c.h.do(http.MethodGet, "/v1/advanced/delegation/agents?account_id="+acct, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var agents api.DelegationAgentList
	res.json(&agents)
	require.Len(t, agents.Items, 1)
	assert.Equal(t, c.agentID, agents.Items[0].Id.String())

	capability := w.grant(c, nil)
	assert.Equal(t, "0.00", capability.Usage.UsedTodayUsd)
	assert.Equal(t, "5000.00", capability.Usage.RemainingTodayUsd)
	assert.Equal(t, api.SIMULATED, capability.ValueDomain)

	ticket, err := w.requestSwap(c, capability.Id, 1, 100_000_000) // 100 sandbox dollars
	require.NoError(t, err)
	require.Equal(t, delegation.SwapAdmitted, ticket.Swap.Status)
	sid := ticket.Swap.ID.String()

	// The use is on the capability at once, before anything is signed.
	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/capabilities/"+capability.Id.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var detail api.DelegationCapabilityDetail
	res.json(&detail)
	assert.Equal(t, "100.00", detail.Usage.UsedTodayUsd)
	assert.Equal(t, "4900.00", detail.Usage.RemainingTodayUsd)
	require.Len(t, detail.Uses, 1)
	assert.Equal(t, sid, detail.Uses[0].SubjectId.String())

	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/swaps/"+sid, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var admitted api.DelegationSwap
	res.json(&admitted)
	assert.Equal(t, api.DelegationSwapAdmitted, admitted.Status)
	assert.Nil(t, admitted.Inspection, "nothing has been inspected before it is signed")
	assert.Equal(t, "100000000", admitted.Plan.AmountIn)
	assert.Equal(t, "100.00", admitted.NotionalUsd)

	// The operator's run executes it under the service's own authority.
	op := dgOperator()
	c.h.as(&op)
	runKey := dgKey("dg-run")
	res = c.h.do(http.MethodPost, "/v1/advanced/delegation/ops/execute-pending", map[string]any{"limit": 50}, "Idempotency-Key", runKey)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var run api.DelegationExecutionRun
	res.json(&run)
	var ran *api.DelegationExecutionItem
	for i := range run.Items {
		if run.Items[i].Id.String() == sid {
			ran = &run.Items[i]
		}
	}
	require.NotNil(t, ran, "the run took the admitted swap")
	assert.Equal(t, "ADMITTED", ran.StatusBefore)
	assert.Equal(t, "SETTLED", ran.Status)
	assert.Nil(t, ran.Error)
	replay := c.h.do(http.MethodPost, "/v1/advanced/delegation/ops/execute-pending", map[string]any{"limit": 50}, "Idempotency-Key", runKey)
	require.Equal(t, http.StatusOK, replay.Code)
	assert.JSONEq(t, res.Body.String(), replay.Body.String(), "a replayed run answers the first and executes nothing")

	// The operator sees the capability across accounts, and nothing of this
	// swap is still pending.
	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/ops/capabilities?status=ACTIVE&limit=200", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), capability.Id.String())
	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/ops/pending", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.NotContains(t, res.Body.String(), sid)

	// The owner reads the signing request: inspected, approved by the
	// provider, signed, finalized and posted.
	c.h.as(&c.owner)
	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/swaps/"+sid, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var settled api.DelegationSwap
	res.json(&settled)
	assert.Equal(t, api.DelegationSwapSettled, settled.Status)
	require.NotNil(t, settled.Inspection)
	assert.Equal(t, api.DelegationVerdictApproved, settled.Inspection.Verdict)
	assert.Empty(t, settled.Inspection.ReasonCodes)
	assert.Equal(t, delegation.InspectorVersion, settled.Inspection.InspectorVersion)
	assert.Equal(t, settled.PlanHash, settled.Inspection.PlanHash, "the provider signed exactly the plan the swap carries")
	assert.Equal(t, capability.ContentHash, settled.Inspection.CapabilityHash)
	assert.Equal(t, c.wallet.String(), settled.Inspection.SignerPublicKey)
	require.NotNil(t, settled.TxId)
	require.NotNil(t, settled.JournalTransactionId)
	assert.Subset(t, dgStatuses(settled.Transitions), []string{"ADMITTED", "SIGNED", "SUBMITTED", "SETTLED"})
	assert.Equal(t, ticket.Swap.AmountOut.String(), settled.AmountOut)

	// Exactly the plan moved, once, on chain and in the books.
	out := ticket.Swap.AmountOut
	assert.Equal(t, out.String(), money.QuantityFromInt64(int64(w.onChain(c.wallet, w.tok))).String())
	assert.Equal(t, uint64(900_000_000), w.onChain(c.wallet, w.usd))
	assert.Equal(t, out.String(), w.booked(c.account, ledger.CodeWallet, w.tok).String())
	assert.Equal(t, "900000000", w.booked(c.account, ledger.CodeWallet, w.usd).String())

	// The account's lists show it; another customer reads it as absent.
	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/swaps?account_id="+acct+"&status=SETTLED", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page api.DelegationSwapPage
	res.json(&page)
	require.Len(t, page.Items, 1)
	assert.Equal(t, sid, page.Items[0].Id.String())
	stranger := w.customer(0)
	res = stranger.h.do(http.MethodGet, "/v1/advanced/delegation/swaps/"+sid, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())

	// The agent never reaches HTTP, whatever it asks for.
	agent := security.AgentPrincipal(c.agentID, acct)
	c.h.as(&agent)
	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/swaps/"+sid, nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = c.h.do(http.MethodPost, "/v1/advanced/delegation/ops/execute-pending", map[string]any{}, "Idempotency-Key", dgKey("dg-agent-run"))
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
}

// A request over the capability's single-trade cap is refused before
// anything is reserved or used; a revoked capability admits nothing more,
// and a swap admitted under it before the revocation is never signed, even
// once its owner approves it.
func TestIntegration_DelegationRefusesAnOverCapRequestAndARevokedCapability(t *testing.T) {
	w := newDgWorld(t)
	c := w.customer(1_000_000_000)
	acct := c.account.String()
	capability := w.grant(c, func(b map[string]any) {
		b["max_single_trade_usd"] = "50.00"
		b["requires_human_approval"] = true
	})

	_, err := w.requestSwap(c, capability.Id, 1, 100_000_000) // 100 > 50
	dgRequireRefusal(t, err, errs.CodeRiskRejected, delegation.ReasonSingleLimitExceeded)
	res := c.h.do(http.MethodGet, "/v1/advanced/delegation/swaps?account_id="+acct, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var none api.DelegationSwapPage
	res.json(&none)
	assert.Empty(t, none.Items, "a refused request leaves no signing request")
	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/capabilities/"+capability.Id.String(), nil)
	require.Equal(t, http.StatusOK, res.Code)
	var unused api.DelegationCapabilityDetail
	res.json(&unused)
	assert.Equal(t, 0, unused.Usage.Uses)
	assert.Equal(t, "0.00", unused.Usage.UsedTotalUsd)

	pending, err := w.requestSwap(c, capability.Id, 2, 40_000_000) // within the cap, needs the owner
	require.NoError(t, err)
	require.Equal(t, delegation.SwapAwaitingApproval, pending.Swap.Status)

	res = c.h.do(http.MethodPost, "/v1/advanced/delegation/capabilities/"+capability.Id.String()+"/revoke",
		map[string]any{"reason": "the agent misbehaved"}, "Idempotency-Key", dgKey("dg-revoke"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var revoked api.DelegationCapability
	res.json(&revoked)
	assert.Equal(t, api.DelegationCapabilityRevoked, revoked.Status)
	assert.Equal(t, api.DelegationStandingRevoked, revoked.Standing)
	require.NotNil(t, revoked.RevokedBy)
	assert.Equal(t, c.user.String(), revoked.RevokedBy.ActorId)

	_, err = w.requestSwap(c, capability.Id, 3, 40_000_000)
	dgRequireRefusal(t, err, errs.CodeRiskRejected, delegation.ReasonCapabilityRevoked)

	// The owner approves the swap admitted before the revocation. The
	// decision is recorded; the execution refuses before anything is signed.
	sid := pending.Swap.ID.String()
	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/swaps/"+sid, nil)
	require.Equal(t, http.StatusOK, res.Code)
	var shown api.DelegationSwap
	res.json(&shown)
	res = c.h.do(http.MethodPost, "/v1/advanced/delegation/swaps/"+sid+"/decision",
		map[string]any{"decision": "APPROVE", "plan_hash": shown.PlanHash, "reason": "approved before I saw the revocation"},
		"Idempotency-Key", dgKey("dg-approve"))
	require.Equal(t, http.StatusOK, res.Code, "the decision stands; body=%s", res.Body.String())
	var decided api.DelegationSwapDecisionResult
	res.json(&decided)
	assert.Equal(t, api.DelegationSwapRejected, decided.Swap.Status)
	require.NotNil(t, decided.Swap.FailureCode)
	assert.Equal(t, delegation.ReasonCapabilityRevoked, *decided.Swap.FailureCode)
	assert.True(t, decided.Execution.Attempted)
	require.NotNil(t, decided.Execution.Error)
	assert.Equal(t, string(errs.CodeSigningRejected), decided.Execution.Error.Code)
	require.NotNil(t, decided.Execution.Error.Reason)
	assert.Equal(t, delegation.ReasonCapabilityRevoked, *decided.Execution.Error.Reason)
	require.NotNil(t, decided.Swap.Approval)
	assert.Equal(t, api.DelegationDecisionApprove, decided.Swap.Approval.Decision)
	assert.Nil(t, decided.Swap.Inspection, "the signing provider was never asked")
	assert.Zero(t, w.onChain(c.wallet, w.tok), "nothing reached the chain")

	// An agent cannot grant itself scope: the boundary refuses it, and so does
	// the service, should anything in-process ever ask on its behalf.
	agent := security.AgentPrincipal(c.agentID, acct)
	c.h.as(&agent)
	res = c.h.do(http.MethodPost, "/v1/advanced/delegation/capabilities", map[string]any{"account_id": acct}, "Idempotency-Key", dgKey("dg-agent-grant"))
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	err = w.d.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(_ context.Context, tx pgx.Tx) error {
		_, _, err := w.svc.GrantCapability(w.agent(c), tx, delegation.GrantRequest{AccountID: c.account, AgentID: c.agentID,
			Mode: gates.ModeSandbox, IdempotencyKey: "agent-self-grant"})
		return err
	})
	dgRequireRefusal(t, err, errs.CodeForbidden, "")
}

// A withdrawal goes only to a destination on the account's allow-list and
// the capability's: another account's, one outside the capability, and one
// the owner removed are all refused, and nothing is reserved or moved.
func TestIntegration_DelegationWithdrawalToANonAllowListedDestinationIsRefused(t *testing.T) {
	w := newDgWorld(t)
	c := w.customer(1_000_000_000)
	other := w.customer(0)
	payA := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("dg-pay-a/" + w.tag))).PublicKey()
	payB := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("dg-pay-b/" + w.tag))).PublicKey()
	capability := w.grant(c, func(b map[string]any) {
		b["destination_allowlist"] = []string{w.venue.Address.String(), payA.String()}
	})
	w.setPolicy(c, nil)
	destA := w.addDestination(c, payA, "cold wallet")
	destB := w.addDestination(c, payB, "not in the grant")
	theirs := w.addDestination(other, payA, "theirs")

	_, err := w.proposeWithdrawal(c, capability.Id, theirs.Id, 1, 25_000_000)
	dgRequireRefusal(t, err, errs.CodeNotFound, delegation.ReasonWithdrawalDestinationNotAllowlisted)

	_, err = w.proposeWithdrawal(c, capability.Id, destB.Id, 2, 25_000_000)
	dgRequireRefusal(t, err, errs.CodeRiskRejected, delegation.ReasonDestinationNotGranted)

	res := c.h.do(http.MethodPost, "/v1/advanced/delegation/destinations/"+destA.Id.String()+"/remove",
		map[string]any{"reason": "retired"}, "Idempotency-Key", dgKey("dg-remove"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var removed api.DelegationDestination
	res.json(&removed)
	assert.Equal(t, api.DelegationDestinationRemoved, removed.Status)
	_, err = w.proposeWithdrawal(c, capability.Id, destA.Id, 3, 25_000_000)
	dgRequireRefusal(t, err, errs.CodeRiskRejected, "")

	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/withdrawals?account_id="+c.account.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page api.DelegationWithdrawalPage
	res.json(&page)
	assert.Empty(t, page.Items, "a refused proposal leaves no withdrawal")
	assert.Zero(t, w.onChain(payA, w.usd))
	assert.Zero(t, w.onChain(payB, w.usd))
	assert.Equal(t, uint64(1_000_000_000), w.onChain(c.wallet, w.usd))

	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/destinations?account_id="+c.account.String(), nil)
	require.Equal(t, http.StatusOK, res.Code)
	var dests api.DelegationDestinationList
	res.json(&dests)
	assert.Len(t, dests.Items, 2)
}

// A human approves an allow-listed withdrawal bound to the plan shown; it is
// executed at once, inspected, signed, finalized and posted, and it settles
// exactly once however often the decision is sent again.
func TestIntegration_DelegationHumanApprovedWithdrawalSettlesOnce(t *testing.T) {
	w := newDgWorld(t)
	c := w.customer(1_000_000_000)
	acct := c.account.String()
	payA := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("dg-pay-a/" + w.tag))).PublicKey()
	capability := w.grant(c, func(b map[string]any) {
		b["destination_allowlist"] = []string{w.venue.Address.String(), payA.String()}
	})
	policy := w.setPolicy(c, nil)
	assert.Equal(t, 1, policy.Version)
	res := c.h.do(http.MethodGet, "/v1/advanced/delegation/withdrawal-policy?account_id="+acct, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var state api.DelegationWithdrawalPolicyState
	res.json(&state)
	require.NotNil(t, state.Current)
	assert.Equal(t, policy.Id, state.Current.Id)
	dest := w.addDestination(c, payA, "cold wallet")

	ticket, err := w.proposeWithdrawal(c, capability.Id, dest.Id, 1, 25_000_000) // 25 sandbox dollars
	require.NoError(t, err)
	require.Equal(t, delegation.WithdrawalAwaitingApproval, ticket.Withdrawal.Status, "outside AUTO_APPROVE the owner decides")
	wid := ticket.Withdrawal.ID.String()

	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/withdrawals?account_id="+acct+"&status=AWAITING_APPROVAL", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page api.DelegationWithdrawalPage
	res.json(&page)
	require.Len(t, page.Items, 1)
	shown := page.Items[0]
	assert.Equal(t, wid, shown.Id.String())
	assert.Equal(t, api.DelegationOutcomeHumanRequired, shown.Evaluation.Outcome)
	assert.Contains(t, shown.Evaluation.ReasonCodes, delegation.ReasonHumanAutoApprovalDisabled)
	assert.Equal(t, "cold wallet", shown.Destination.Label)
	assert.Equal(t, payA.String(), shown.DestinationAddress)
	assert.Equal(t, "25000000", shown.Amount)
	assert.Equal(t, "25.00", shown.NotionalUsd)

	op := dgOperator()
	c.h.as(&op)
	res = c.h.do(http.MethodGet, "/v1/advanced/delegation/ops/pending", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), wid, "the operator sees the decision waiting")
	c.h.as(&c.owner)

	// A decision bound to any other plan is refused and changes nothing.
	res = c.h.do(http.MethodPost, "/v1/advanced/delegation/withdrawals/"+wid+"/decision",
		map[string]any{"decision": "APPROVE", "plan_hash": strings.Repeat("0", 64), "reason": "looks right"}, "Idempotency-Key", dgKey("dg-wrong-plan"))
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeConflict, p.Code)
	assert.Equal(t, delegation.ReasonApprovalPlanMismatch, p.Fields["reason"])

	key := dgKey("dg-approve")
	body := map[string]any{"decision": "APPROVE", "plan_hash": shown.PlanHash, "reason": "to my cold wallet"}
	res = c.h.do(http.MethodPost, "/v1/advanced/delegation/withdrawals/"+wid+"/decision", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var decided api.DelegationWithdrawalDecisionResult
	res.json(&decided)
	assert.True(t, decided.Execution.Attempted)
	assert.Nil(t, decided.Execution.Error)
	assert.Equal(t, api.DelegationWithdrawalSettled, decided.Withdrawal.Status)
	require.NotNil(t, decided.Withdrawal.Approval)
	assert.Equal(t, "USER", decided.Withdrawal.Approval.Approver.ActorType)
	assert.Equal(t, c.user.String(), decided.Withdrawal.Approval.Approver.ActorId)
	assert.Equal(t, shown.PlanHash, decided.Withdrawal.Approval.PlanHash, "the approval is bound to the plan shown")
	require.NotNil(t, decided.Withdrawal.Inspection)
	assert.Equal(t, api.DelegationVerdictApproved, decided.Withdrawal.Inspection.Verdict)
	require.NotNil(t, decided.Withdrawal.TxId)
	assert.Subset(t, dgStatuses(decided.Withdrawal.Transitions), []string{"AWAITING_APPROVAL", "APPROVED", "SIGNED", "SETTLED"})

	// Sent again, the decision answers the same and moves nothing again; a
	// new decision finds nothing awaiting one.
	again := c.h.do(http.MethodPost, "/v1/advanced/delegation/withdrawals/"+wid+"/decision", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusOK, again.Code)
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	res = c.h.do(http.MethodPost, "/v1/advanced/delegation/withdrawals/"+wid+"/decision", body, "Idempotency-Key", dgKey("dg-approve-twice"))
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidStateTransition, res.problem().Code)

	// Exactly once, on chain and in the books.
	assert.Equal(t, uint64(25_000_000), w.onChain(payA, w.usd))
	assert.Equal(t, uint64(975_000_000), w.onChain(c.wallet, w.usd))
	assert.Equal(t, "25000000", w.booked(c.account, ledger.CodeWithdrawn, w.usd).String())
	assert.Equal(t, "975000000", w.booked(c.account, ledger.CodeWallet, w.usd).String())

	// The agent proposed; it can never decide, here or in-process.
	err = w.d.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(_ context.Context, tx pgx.Tx) error {
		_, err := w.svc.DecideWithdrawal(w.agent(c), tx, delegation.WithdrawalDecision{WithdrawalID: ticket.Withdrawal.ID,
			PlanHash: ticket.Withdrawal.PlanHash, Approve: true, Reason: "self-approval"})
		return err
	})
	dgRequireRefusal(t, err, errs.CodeForbidden, "")
}

// One account's withdrawals still in their cooling period cannot be signed,
// so they never take a place in the operator's execution run (review 3, F3):
// another account's swap, admitted after fifty of them, is carried out within
// its quote by the first run, and the cooling withdrawals are left exactly as
// they were -- approved, unsigned, nothing decided about them.
func TestIntegration_DelegationCoolingWithdrawalsCannotHoldAnotherAccountsSwapBack(t *testing.T) {
	w := newDgWorld(t)
	busy := w.customer(10_000_000) // $10
	w.setPolicy(busy, func(b map[string]any) {
		b["auto_approve_enabled"], b["auto_approve_max_usd"], b["auto_approve_max_risk_score"] = true, "1.00", 80
		b["per_withdrawal_limit_usd"], b["daily_limit_usd"], b["velocity_max_count"] = "1.00", "100.00", 100
		b["cooling_period_seconds"] = 7 * 24 * 3600 // the most a policy allows
	})
	dest := w.addDestination(busy, devnet.NewKeypairFromSeed(sha256.Sum256([]byte("httpapi-dg-cooling/"+w.tag))).PublicKey(), "cold wallet")
	capBusy := w.grant(busy, func(b map[string]any) {
		b["destination_allowlist"] = []string{w.venue.Address.String(), dest.Address}
	})
	cooling := map[string]bool{}
	for i := 1; i <= 50; i++ {
		ticket, err := w.proposeWithdrawal(busy, capBusy.Id, dest.Id, uint64(i), 10_000) // $0.01 each
		require.NoError(t, err, "proposal %d", i)
		require.Equal(t, delegation.WithdrawalApproved, ticket.Withdrawal.Status, "auto-approved, cooling for seven days")
		cooling[ticket.Withdrawal.ID.String()] = true
	}
	other := w.customer(1_000_000_000)
	ticket, err := w.requestSwap(other, w.grant(other, nil).Id, 1, 100_000_000)
	require.NoError(t, err)
	require.Equal(t, delegation.SwapAdmitted, ticket.Swap.Status)

	op := dgOperator()
	other.h.as(&op)
	res := other.h.do(http.MethodPost, "/v1/advanced/delegation/ops/execute-pending", map[string]any{"limit": 50},
		"Idempotency-Key", dgKey("dg-fair-run"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var run api.DelegationExecutionRun
	res.json(&run)
	var ran *api.DelegationExecutionItem
	for i := range run.Items {
		assert.False(t, cooling[run.Items[i].Id.String()], "a withdrawal still cooling never takes a place in a run")
		if run.Items[i].Id.String() == ticket.Swap.ID.String() {
			ran = &run.Items[i]
		}
	}
	require.NotNil(t, ran, "the other account's swap was carried out by the first run")
	assert.Equal(t, "ADMITTED", ran.StatusBefore)
	assert.Equal(t, "SETTLED", ran.Status)
	assert.Nil(t, ran.Error)

	var approved, decided int
	require.NoError(t, w.d.QueryRow(w.ctx, `SELECT count(*) FILTER (WHERE status = 'APPROVED'),
		(SELECT count(*) FROM delegated_signing_decisions d WHERE d.account_id = $1)
		FROM delegated_withdrawals WHERE account_id = $1`, busy.account).Scan(&approved, &decided))
	assert.Equal(t, 50, approved, "the cooling withdrawals wait, unchanged")
	assert.Zero(t, decided, "nothing about them was decided or signed")
}

// An operator cannot back a delegation venue with a treasury book: the
// venue's swaps would move the treasury's inventory with none of its actions,
// limits or crossing disclosures. The registration is refused and nothing is
// registered.
func TestIntegration_DelegationVenueOnATreasuryBookIsRefused(t *testing.T) {
	w := newDgWorld(t)
	var house econ.HouseAccountID
	require.NoError(t, w.d.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		h, err := econ.NewRegistry().CreateHouseAccount(ctx, tx, "dg-treasury-"+w.tag, econ.HouseTreasuryConversion,
			econ.Actor{Type: security.ActorOperator, ID: "httpapi-delegation-itest"})
		if err != nil {
			return err
		}
		house = h.ID
		_, err = tx.Exec(ctx, `INSERT INTO treasury_books (house_account_id, class, valuation_asset_id, registered_by_actor_type,
			registered_by_actor_id, registered_at) VALUES ($1, 'TREASURY', $2, 'OPERATOR', 'httpapi-delegation-itest', now())`, house, w.usd.ID)
		return err
	}))
	c := w.customer(0)
	op := dgOperator()
	c.h.as(&op)
	res := c.h.do(http.MethodPost, "/v1/advanced/delegation/ops/venues", map[string]any{"name": w.venue.Name, "spread_bps": 50,
		"house_account_id": house.String()}, "Idempotency-Key", dgKey("dg-venue"))
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "treasury book")
	var n int
	require.NoError(t, w.d.QueryRow(w.ctx, `SELECT count(*) FROM delegation_venues WHERE house_account_id = $1`, house).Scan(&n))
	assert.Zero(t, n)
}
