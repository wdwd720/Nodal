//go:build integration

package advancedmetrics

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxchain"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// delegationWorld is delegated agent authority on a devnet network of its
// own, built as the advanced worker builds it, on the system clock: an
// agent intent's transitions are stamped by its service's clock, and the
// counted window is the database's, so the two must agree. It carries a
// venue with inventory in its house book and on chain, a devnet dollar and a
// token priced at 100.
type delegationWorld struct {
	f        *fixture
	clk      clock.Clock
	chain    sandboxchain.Chain
	ledger   *ledger.Service
	prices   *valuation.PriceStore
	svc      *delegation.Service
	signer   *delegation.DelegatedSigner
	venue    delegation.Venue
	mintAuth devnet.Keypair
	usd, tok assets.Asset
	house    econ.HouseAccount
}

func (f *fixture) delegationWorld() *delegationWorld {
	f.t.Helper()
	sys := clock.System()
	out := event.NewOutbox(sys)
	aw := audit.NewWriterWithBuildVersion("advancedmetrics-itest")
	checker, err := gates.NewChecker(string(config.EnvLocal), func(gates.Capability) bool { return false }, sys)
	require.NoError(f.t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	var seed sandboxchain.Seed
	_, err = rand.Read(seed[:])
	require.NoError(f.t, err)
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	chain, err := sandboxchain.Open(f.ctx, testDB, sys, config.EnvLocal, "amdg-"+tag, seed)
	require.NoError(f.t, err)
	signer, err := delegation.NewDelegatedSigner(seed.Derive(sandboxchain.PurposeDelegatedSigner), delegation.SignerConfig{
		Environment: config.EnvLocal, Clock: sys, Policy: delegation.DefaultSigningPolicy(), Fees: delegation.HostFees(chain.Host),
	})
	require.NoError(f.t, err)
	venueName := "amdg-" + tag
	venueKey := delegation.NewVenueSigner(seed.VenueSeed(venueName))
	var authSeed [32]byte
	_, err = rand.Read(authSeed[:])
	require.NoError(f.t, err)
	d := &delegationWorld{f: f, clk: sys, chain: chain, ledger: ledger.NewService(sys, "advancedmetrics-itest"),
		prices: valuation.NewPriceStore(sys), signer: signer, mintAuth: devnet.NewKeypairFromSeed(authSeed)}
	d.svc, err = delegation.New(delegation.Deps{
		DB: testDB, Clock: sys, Environment: config.EnvLocal, Host: chain.Host, Intents: econ.NewIntentStore(sys, out, aw),
		Effects: econ.NewEffectStore(sys, out, aw), Authorizer: econ.NewAuthorizer(checker, kills), Kills: kills, Ledger: d.ledger,
		Capital: capital.NewService(sys, capital.OutboxEmitter{Outbox: out, Clock: sys}), Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: f.router, Prices: d.prices, Identity: compliance.NewAccountVerification(sys),
		Profiles: compliance.NewRepository(aw), Reconciliation: reconciliation.NewRepository(sys, out, aw), Audit: aw,
		Accounts: accounts.NewRepository(), Assets: f.assets, Signer: signer, Venues: []*delegation.VenueSigner{venueKey},
	})
	require.NoError(f.t, err)

	require.NoError(f.t, chain.Host.Airdrop(f.ctx, devnethost.ChainSolana, d.mintAuth.PublicKey(), 1_000_000_000))
	d.usd = d.devAsset("AMDU"+strings.ToUpper(tag[:6]), 6, true)
	d.tok = d.devAsset("AMDT"+strings.ToUpper(tag[:6]), 9, false)
	_, err = d.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{AssetID: d.tok.ID, QuoteAssetID: econ.SandboxDollarID(),
		Mantissa: money.QuantityFromInt64(10_000), Scale: 2, Source: "advancedmetrics-itest", ObservedAt: sys.Now()})
	require.NoError(f.t, err)

	operator := f.operator()
	d.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d.house, err = econ.NewRegistry().CreateHouseAccount(ctx, tx, "amdg-house-"+tag, econ.HouseLiquidityProvision, operator)
		if err != nil {
			return err
		}
		d.venue, err = d.svc.RegisterVenue(ctx, tx, delegation.VenueSpec{Name: venueName, Chain: devnethost.ChainSolana,
			Address: venueKey.PublicKey(), HouseAccountID: d.house.ID, SpreadBPS: 50}, operator)
		return err
	})
	d.fundHouse(d.tok, 1_000_000_000_000)
	d.fundHouse(d.usd, 1_000_000_000)
	d.mintTo(venueKey.PublicKey(), d.tok, 1_000_000_000_000)
	d.mintTo(venueKey.PublicKey(), d.usd, 1_000_000_000)
	return d
}

// operator is an operator: a user of its own, acting as an OPERATOR.
func (f *fixture) operator() econ.Actor {
	f.t.Helper()
	email := sha256.Sum256([]byte("advancedmetrics-operator-" + uuid.NewString() + "@sandbox.invalid"))
	u, err := accounts.NewRepository().CreateUser(f.ctx, testDB, "advancedmetrics-itest", "advancedmetrics-operator-"+uuid.NewString(), email[:])
	require.NoError(f.t, err)
	return econ.Actor{Type: security.ActorOperator, ID: u.ID.String()}
}

func (d *delegationWorld) inTx(fn func(ctx context.Context, tx pgx.Tx) error) {
	d.f.t.Helper()
	require.NoError(d.f.t, testDB.InTx(d.f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn))
}

func (d *delegationWorld) devAsset(symbol string, decimals uint8, stable bool) assets.Asset {
	d.f.t.Helper()
	var s [32]byte
	_, err := rand.Read(s[:])
	require.NoError(d.f.t, err)
	mint := devnet.NewKeypairFromSeed(s).PublicKey()
	require.NoError(d.f.t, d.chain.Host.EnsureMint(d.f.ctx, devnethost.ChainSolana,
		devnet.MintConfig{Address: mint, Decimals: decimals, Authority: d.mintAuth.PublicKey()}))
	a := assets.Asset{Chain: string(devnethost.ChainSolana), MintAddress: mint.String(), Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: symbol, Name: symbol + " (simulated devnet token)", Decimals: decimals,
		RiskClass: assets.RiskMajor, Status: assets.StatusActive}
	if stable {
		a.IsStablecoin, a.PegCurrency, a.RiskClass = true, valuation.USDPeg, assets.RiskSettlement
	}
	out, err := d.f.assets.Create(d.f.ctx, testDB, a)
	require.NoError(d.f.t, err)
	return out
}

func (d *delegationWorld) fundHouse(a assets.Asset, q int64) {
	d.f.t.Helper()
	d.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := d.ledger.Post(ctx, tx, ledger.Posting{Kind: ledger.KindHouseFunding, IdempotencyKey: "amdg-house:" + ref,
			Reference: ledger.FinancialEventReference{Type: "house_funding", ID: ref}, EffectiveAt: d.clk.Now(),
			Entries: []ledger.Entry{
				{Account: d.house.Account(ledger.CodeHouseInventory, a.ID), Side: ledger.Debit, Quantity: money.QuantityFromInt64(q)},
				{Account: d.house.Account(ledger.CodeHouseCapital, a.ID), Side: ledger.Credit, Quantity: money.QuantityFromInt64(q)},
			}})
		return err
	})
}

// mintTo issues q base units of a devnet token to owner and waits for
// finality.
func (d *delegationWorld) mintTo(owner devnet.PublicKey, a assets.Asset, q uint64) {
	d.f.t.Helper()
	ctx, h := d.f.ctx, d.chain.Host
	mint, err := devnet.ParsePublicKey(a.MintAddress)
	require.NoError(d.f.t, err)
	bh, err := h.LatestBlockhash(ctx, devnethost.ChainSolana)
	require.NoError(d.f.t, err)
	tx, err := devnet.Sign(devnet.Message{ChainID: devnethost.ChainSolana, RecentBlockhash: bh, FeePayer: d.mintAuth.PublicKey(),
		Instructions: []devnet.Instruction{
			devnet.TokenMintTo{Mint: mint, ToOwner: owner, Amount: q, Authority: d.mintAuth.PublicKey()},
			devnet.Memo{Data: []byte(uuid.NewString())},
		}}, d.mintAuth)
	require.NoError(d.f.t, err)
	raw, err := tx.Encode()
	require.NoError(d.f.t, err)
	txID, err := h.Submit(ctx, devnethost.ChainSolana, raw)
	require.NoError(d.f.t, err)
	_, err = h.ProduceBlocks(ctx, devnethost.ChainSolana, 1+devnet.DefaultFinalityDepth)
	require.NoError(d.f.t, err)
	st, err := h.Status(ctx, devnethost.ChainSolana, txID)
	require.NoError(d.f.t, err)
	require.Equal(d.f.t, devnet.TxFinalized, st.State, "mint %s", st.Err)
}

// delegator is a verified, sanctions-clear customer with a strategy and an
// agent for it, whose delegated wallet holds usd base units of the dollar on
// the books and on chain.
type delegator struct {
	user    accounts.UserID
	account accounts.AccountID
	agentID string
	wallet  devnet.PublicKey
}

func (d *delegationWorld) delegator(usd int64) delegator {
	d.f.t.Helper()
	f := d.f
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "advancedmetrics-itest", "advancedmetrics-dg-"+uuid.NewString(), nil)
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	verifiedAt := d.clk.Now().Add(-24 * time.Hour)
	strategyID, versionID, agentID := id.New[id.Any](), id.New[id.Any](), id.New[id.Any]()
	hash := sha256.Sum256([]byte(versionID.String()))
	d.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := compliance.NewRepository(f.audit).Upsert(ctx, tx, compliance.Profile{
			UserID: u.ID, IdentityState: compliance.IdentityVerified, SanctionsState: compliance.SanctionsClear, VerifiedAt: &verifiedAt,
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: "advancedmetrics-idv", Reason: "fixture"}); err != nil {
			return err
		}
		// The strategy and agent rows are fixtures: the agent lane owns their
		// lifecycle, and delegation only reads them.
		if _, err := tx.Exec(ctx, `INSERT INTO strategies (id, owner_account_id, owner_user_id, name, source_kind, status, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, 'TYPESCRIPT_SDK', 'ACTIVE', 'USER', $5)`, strategyID, a.ID, u.ID, "strategy-"+strategyID.String(), u.ID.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash, effect_set, status, source_kind, source_hash,
				compiler_version, risk_policy_version, risk_policy_hash, model_budget, data_budget, envelope_requirements, human_readable, built_at)
			VALUES ($1, $2, 1, 1, '{}'::jsonb, $3, '{}', 'COMPILED', 'TYPESCRIPT_SDK', $3, 'test', 'risk-v1', $3, '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'test strategy', $4)`,
			versionID, strategyID, hash[:], d.clk.Now()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO agents (id, account_id, strategy_id, strategy_version_id, name, stage, state, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, $5, 'VALIDATED', 'VALIDATED', 'USER', $6)`, agentID, a.ID, strategyID, versionID, "agent-"+agentID.String(), u.ID.String())
		return err
	})
	c := delegator{user: u.ID, account: a.ID, agentID: agentID.String(), wallet: d.signer.PublicKey(a.ID, devnethost.ChainSolana)}
	d.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := d.ledger.Post(ctx, tx, ledger.Posting{Kind: ledger.KindSandboxFunding, IdempotencyKey: "amdg-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: d.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(a.ID, ledger.CodeWallet, d.usd.ID), Side: ledger.Debit, Quantity: money.QuantityFromInt64(usd)},
				{Account: ledger.CustomerAccount(a.ID, ledger.CodeCapital, d.usd.ID), Side: ledger.Credit, Quantity: money.QuantityFromInt64(usd)},
			}})
		return err
	})
	d.mintTo(c.wallet, d.usd, uint64(usd))
	return c
}

func (d *delegationWorld) owner(c delegator) context.Context {
	return security.WithPrincipal(d.f.ctx, security.Principal{SubjectID: c.user.String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{c.account.String()}, AuthTime: d.clk.Now(), AMR: []string{"mfa"}})
}

func (d *delegationWorld) agent(c delegator) context.Context {
	return security.WithPrincipal(d.f.ctx, security.Principal{SubjectID: c.agentID, ActorType: security.ActorAgent,
		AccountIDs: []string{c.account.String()}})
}

// grant gives the customer's agent a sandbox capability over the venue,
// valid for three days, with no approval required.
func (d *delegationWorld) grant(c delegator) delegation.Capability {
	d.f.t.Helper()
	var out delegation.Capability
	d.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		out, _, err = d.svc.GrantCapability(d.owner(c), tx, delegation.GrantRequest{
			AccountID: c.account, AgentID: c.agentID, Mode: gates.ModeSandbox,
			InstrumentClasses: []delegation.InstrumentClass{delegation.ClassStableValue, delegation.ClassSpotToken},
			ActionClasses:     []delegation.ActionClass{delegation.ActionAcquireExposure, delegation.ActionReduceExposure},
			MaxNotional:       money.USDFromMinor(1_000_000), MaxDailyNotional: money.USDFromMinor(500_000),
			MaxSingleTrade:       money.USDFromMinor(100_000),
			DestinationAllowlist: []devnet.PublicKey{d.venue.Address},
			ChainAllowlist:       []devnet.ChainID{devnethost.ChainSolana}, ExpiresAt: d.clk.Now().Add(72 * time.Hour),
			IdempotencyKey: "amdg-grant-" + uuid.NewString(),
		})
		return err
	})
	return out
}

// refusalReason is a recorded refusal's stable reason field.
func refusalReason(err error) string {
	e, ok := errs.As(err)
	if !ok {
		return ""
	}
	r, _ := e.Fields["reason"].(string)
	return r
}

// request has the agent ask to swap amountIn base units of the dollar for
// the token, labelled action, with a slippage bound: the ticket, refused or
// not, is committed.
func (d *delegationWorld) request(c delegator, cid delegation.CapabilityID, nonce uint64, amountIn int64, action delegation.ActionClass,
	slippage money.BPS) delegation.SwapTicket {
	d.f.t.Helper()
	var ticket delegation.SwapTicket
	d.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = d.svc.RequestSwap(d.agent(c), tx, delegation.SwapRequest{CapabilityID: cid, AccountID: c.account, Nonce: nonce,
			Action: action, VenueID: d.venue.ID, PayAssetID: d.usd.ID, ReceiveAssetID: d.tok.ID,
			AmountIn: money.QuantityFromInt64(amountIn), MaxSlippageBPS: slippage, Jurisdiction: "US-CA",
			CorrelationID: "corr-" + uuid.NewString()})
		return err
	})
	return ticket
}

// TestIntegration_AuthorityViolationsCountRefusalsBeyondTheGrant: through
// the real delegation service, every refusal of a request beyond the owner's
// grant is counted once, by stage -- at admission the swap whose label is
// not what it does and the swap Risk V2 refused because the capability was
// revoked; at execution the admitted swap the revocation stopped before
// signing and the signed swap it stopped before submission -- and a refusal
// for the platform's own policy (a slippage bound above the signing
// policy's) is not counted at all.
func TestIntegration_AuthorityViolationsCountRefusalsBeyondTheGrant(t *testing.T) {
	f := newFixture(t)
	d := f.delegationWorld()
	c := d.delegator(10_000_000)
	capability := d.grant(c)
	admitted := d.request(c, capability.ID, 1, 1_000_000, delegation.ActionAcquireExposure, 100)
	require.NoError(t, admitted.Refusal)
	signed := d.request(c, capability.ID, 2, 1_000_000, delegation.ActionAcquireExposure, 100)
	require.NoError(t, signed.Refusal)
	require.NoError(t, d.chain.Host.EnsureNativeBalance(f.ctx, devnethost.ChainSolana, c.wallet, 1_000_000, 100_000_000))
	d.inTx(func(ctx context.Context, tx pgx.Tx) error {
		out, err := d.svc.SignSwap(ctx, tx, signed.Swap.ID)
		require.NoError(t, out.Refusal)
		return err
	})
	col := f.collector()
	require.Nil(t, f.collect(col).Counted)

	// Admission: paying the dollar for the token acquires exposure, whatever
	// the agent labels it.
	mislabelled := d.request(c, capability.ID, 3, 1_000_000, delegation.ActionReduceExposure, 100)
	require.Error(t, mislabelled.Refusal)
	assert.Equal(t, delegation.ReasonActionMismatch, refusalReason(mislabelled.Refusal))
	// Not a violation: the slippage bound is the signing policy's to set.
	loose := d.request(c, capability.ID, 4, 1_000_000, delegation.ActionAcquireExposure, money.OneHundredPercent)
	require.Error(t, loose.Refusal)
	assert.Equal(t, delegation.ReasonSlippageAbovePolicy, refusalReason(loose.Refusal))

	d.inTx(func(_ context.Context, tx pgx.Tx) error {
		_, err := d.svc.RevokeCapability(d.owner(c), tx, delegation.RevokeRequest{CapabilityID: capability.ID, Reason: "advancedmetrics itest"})
		return err
	})
	// Admission after the revocation: Risk V2's authority section refuses.
	late := d.request(c, capability.ID, 5, 1_000_000, delegation.ActionAcquireExposure, 100)
	require.Error(t, late.Refusal)
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(late.Refusal))
	// Execution: the admitted swap is not signed, and the signed one is not
	// submitted.
	_, err := d.svc.ExecuteSwap(f.ctx, admitted.Swap.ID)
	require.Error(t, err)
	assert.Equal(t, errs.CodeSigningRejected, errs.CodeOf(err))
	d.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := d.svc.GateSwapSubmission(ctx, tx, signed.Swap.ID)
		if errs.CodeOf(err) == errs.CodeKillSwitchActive {
			return nil // the recorded refusal: the swap is FAILED, never submitted
		}
		return err
	})
	got, err := d.svc.GetSwap(f.ctx, testDB, signed.Swap.ID)
	require.NoError(t, err)
	require.Equal(t, delegation.BlockedBeforeSubmission(delegation.ReasonCapabilityRevoked), got.FailureCode)

	s1 := f.collect(col)
	require.NotNil(t, s1.Counted)
	swap := string(delegation.SubjectSwap)
	assert.Equal(t, map[Pair]int64{{swap, StageAdmission}: 2, {swap, StageExecution}: 2}, s1.Counted.AuthorityViolations)
	s2 := f.collect(col)
	require.NotNil(t, s2.Counted)
	assert.Empty(t, s2.Counted.AuthorityViolations, "counted once")
	assert.Equal(t, int64(2), f.counter("advanced_authority_violations", "stage", StageAdmission))
	assert.Equal(t, int64(2), f.counter("advanced_authority_violations", "stage", StageExecution))
	assert.Equal(t, int64(4), f.counter("advanced_authority_violations", "kind", swap))
}
