//go:build integration

package advancedjobs

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
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxchain"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// delegationWorld is delegated agent authority on a chain of the test's own,
// built the way the worker builds it: the delegated signer and the venue's
// co-signer derive from the chain's seed, the identity rule is the advanced
// capabilities' one. It carries a venue with inventory on chain and in its
// house book, and two devnet tokens: a dollar and a token priced at 100.
type delegationWorld struct {
	w        *world
	chain    sandboxchain.Chain
	svc      *delegation.Service
	signer   *delegation.DelegatedSigner
	venue    delegation.Venue
	mintAuth devnet.Keypair
	usd, tok assets.Asset
	house    econ.HouseAccount
}

// delegationService builds the service as the worker does.
func (w *world) delegationService(c sandboxchain.Chain, seed sandboxchain.Seed, venues ...string) (*delegation.Service, *delegation.DelegatedSigner) {
	w.t.Helper()
	signer, err := delegation.NewDelegatedSigner(seed.Derive(sandboxchain.PurposeDelegatedSigner), delegation.SignerConfig{
		Environment: config.EnvLocal, Clock: w.clk, Policy: delegation.DefaultSigningPolicy(), Fees: delegation.HostFees(c.Host),
	})
	require.NoError(w.t, err)
	var cosigners []*delegation.VenueSigner
	for _, name := range venues {
		cosigners = append(cosigners, delegation.NewVenueSigner(seed.VenueSeed(name)))
	}
	svc, err := delegation.New(delegation.Deps{
		DB: testDB, Clock: w.clk, Environment: config.EnvLocal, Host: c.Host, Intents: w.intents, Effects: w.effects,
		Authorizer: w.authz, Kills: w.kills, Ledger: w.ledger, Capital: w.capital, Risk: w.risk,
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: w.router, Prices: w.prices,
		Identity: compliance.NewAccountVerification(w.clk), Profiles: compliance.NewRepository(w.audit),
		Reconciliation: w.recon, Audit: w.audit, Accounts: accounts.NewRepository(), Assets: w.assets,
		Signer: signer, Venues: cosigners,
	})
	require.NoError(w.t, err)
	return svc, signer
}

func (w *world) delegationWorld() *delegationWorld {
	w.t.Helper()
	c, seed := w.seededChain()
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	venueName := "ajobs-" + tag
	svc, signer := w.delegationService(c, seed, venueName)
	var authSeed [32]byte
	_, err := rand.Read(authSeed[:])
	require.NoError(w.t, err)
	d := &delegationWorld{w: w, chain: c, svc: svc, signer: signer, mintAuth: devnet.NewKeypairFromSeed(authSeed)}

	require.NoError(w.t, c.Host.Airdrop(w.ctx, devnethost.ChainSolana, d.mintAuth.PublicKey(), 1_000_000_000))
	d.usd = d.devAsset("AJDU"+strings.ToUpper(tag[:6]), 6, true)
	d.tok = d.devAsset("AJDT"+strings.ToUpper(tag[:6]), 9, false)
	d.priceTok()

	operator := w.operator("delegation-" + tag)
	venueKey := delegation.NewVenueSigner(seed.VenueSeed(venueName))
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d.house, err = w.registry.CreateHouseAccount(ctx, tx, "ajobs-dg-house-"+tag, econ.HouseLiquidityProvision, operator)
		if err != nil {
			return err
		}
		d.venue, err = d.svc.RegisterVenue(ctx, tx, delegation.VenueSpec{Name: venueName, Chain: devnethost.ChainSolana,
			Address: venueKey.PublicKey(), HouseAccountID: d.house.ID, SpreadBPS: 50}, operator)
		return err
	}))
	d.fundHouse(d.tok, 1_000_000_000_000)
	d.fundHouse(d.usd, 1_000_000_000)
	d.mintTo(venueKey.PublicKey(), d.tok, 1_000_000_000_000)
	d.mintTo(venueKey.PublicKey(), d.usd, 1_000_000_000)
	return d
}

func (d *delegationWorld) devAsset(symbol string, decimals uint8, stable bool) assets.Asset {
	d.w.t.Helper()
	var s [32]byte
	_, err := rand.Read(s[:])
	require.NoError(d.w.t, err)
	mint := devnet.NewKeypairFromSeed(s).PublicKey()
	require.NoError(d.w.t, d.chain.Host.EnsureMint(d.w.ctx, devnethost.ChainSolana,
		devnet.MintConfig{Address: mint, Decimals: decimals, Authority: d.mintAuth.PublicKey()}))
	a := assets.Asset{Chain: string(devnethost.ChainSolana), MintAddress: mint.String(), Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: symbol, Name: symbol + " (simulated devnet token)", Decimals: decimals,
		RiskClass: assets.RiskMajor, Status: assets.StatusActive}
	if stable {
		a.IsStablecoin, a.PegCurrency, a.RiskClass = true, valuation.USDPeg, assets.RiskSettlement
	}
	out, err := d.w.assets.Create(d.w.ctx, testDB, a)
	require.NoError(d.w.t, err)
	return out
}

// priceTok records the token at 100.00 sandbox dollars, observed now.
func (d *delegationWorld) priceTok() {
	d.w.t.Helper()
	_, err := d.w.prices.RecordPrice(d.w.ctx, testDB, valuation.PriceObservation{AssetID: d.tok.ID, QuoteAssetID: econ.SandboxDollarID(),
		Mantissa: money.QuantityFromInt64(10_000), Scale: 2, Source: "advancedjobs-itest", ObservedAt: d.w.clk.Now()})
	require.NoError(d.w.t, err)
}

// advance moves the clock and keeps the token's price fresh.
func (d *delegationWorld) advance(by time.Duration) {
	d.w.clk.Advance(by)
	d.priceTok()
}

func (d *delegationWorld) fundHouse(a assets.Asset, q int64) {
	d.w.t.Helper()
	qty := money.QuantityFromInt64(q)
	require.NoError(d.w.t, d.w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := d.w.ledger.Post(ctx, tx, ledger.Posting{Kind: ledger.KindHouseFunding, IdempotencyKey: "ajobs-dg-house:" + ref,
			Reference: ledger.FinancialEventReference{Type: "house_funding", ID: ref}, EffectiveAt: d.w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: d.house.Account(ledger.CodeHouseInventory, a.ID), Side: ledger.Debit, Quantity: qty},
				{Account: d.house.Account(ledger.CodeHouseCapital, a.ID), Side: ledger.Credit, Quantity: qty},
			}})
		return err
	}))
}

// mintTo issues q base units of a devnet token to owner and waits for
// finality.
func (d *delegationWorld) mintTo(owner devnet.PublicKey, a assets.Asset, q uint64) {
	d.w.t.Helper()
	ctx, h := d.w.ctx, d.chain.Host
	mint, err := devnet.ParsePublicKey(a.MintAddress)
	require.NoError(d.w.t, err)
	bh, err := h.LatestBlockhash(ctx, devnethost.ChainSolana)
	require.NoError(d.w.t, err)
	tx, err := devnet.Sign(devnet.Message{ChainID: devnethost.ChainSolana, RecentBlockhash: bh, FeePayer: d.mintAuth.PublicKey(),
		Instructions: []devnet.Instruction{
			devnet.TokenMintTo{Mint: mint, ToOwner: owner, Amount: q, Authority: d.mintAuth.PublicKey()},
			devnet.Memo{Data: []byte(uuid.NewString())},
		}}, d.mintAuth)
	require.NoError(d.w.t, err)
	raw, err := tx.Encode()
	require.NoError(d.w.t, err)
	txID, err := h.Submit(ctx, devnethost.ChainSolana, raw)
	require.NoError(d.w.t, err)
	_, err = h.ProduceBlocks(ctx, devnethost.ChainSolana, 1+devnet.DefaultFinalityDepth)
	require.NoError(d.w.t, err)
	st, err := h.Status(ctx, devnethost.ChainSolana, txID)
	require.NoError(d.w.t, err)
	require.Equal(d.w.t, devnet.TxFinalized, st.State, "mint %s", st.Err)
}

// delegator is a verified, sanctions-clear customer with a strategy and an
// agent for it, whose delegated wallet holds usd dollars on the books and on
// chain.
type delegator struct {
	user    accounts.UserID
	account accounts.AccountID
	agentID string
	wallet  devnet.PublicKey
}

func (d *delegationWorld) delegator(usd int64) delegator {
	d.w.t.Helper()
	w := d.w
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(w.ctx, testDB, "advancedjobs-itest", "advancedjobs-dg-"+uuid.NewString(), nil)
	require.NoError(w.t, err)
	a, err := repo.CreateAccount(w.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(w.t, err)
	verifiedAt := w.clk.Now().Add(-24 * time.Hour)
	strategyID, versionID, agentID := id.New[id.Any](), id.New[id.Any](), id.New[id.Any]()
	hash := sha256.Sum256([]byte(versionID.String()))
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := compliance.NewRepository(w.audit).Upsert(ctx, tx, compliance.Profile{
			UserID: u.ID, IdentityState: compliance.IdentityVerified, SanctionsState: compliance.SanctionsClear, VerifiedAt: &verifiedAt,
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: "advancedjobs-idv", Reason: "fixture"}); err != nil {
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
			versionID, strategyID, hash[:], w.clk.Now()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO agents (id, account_id, strategy_id, strategy_version_id, name, stage, state, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, $5, 'VALIDATED', 'VALIDATED', 'USER', $6)`, agentID, a.ID, strategyID, versionID, "agent-"+agentID.String(), u.ID.String())
		return err
	}))
	c := delegator{user: u.ID, account: a.ID, agentID: agentID.String(), wallet: d.signer.PublicKey(a.ID, devnethost.ChainSolana)}
	if usd > 0 {
		qty := money.QuantityFromInt64(usd)
		require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
			ref := uuid.NewString()
			_, err := w.ledger.Post(ctx, tx, ledger.Posting{Kind: ledger.KindSandboxFunding, IdempotencyKey: "ajobs-dg-fund:" + ref,
				Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: w.clk.Now(),
				Entries: []ledger.Entry{
					{Account: ledger.CustomerAccount(a.ID, ledger.CodeWallet, d.usd.ID), Side: ledger.Debit, Quantity: qty},
					{Account: ledger.CustomerAccount(a.ID, ledger.CodeCapital, d.usd.ID), Side: ledger.Credit, Quantity: qty},
				}})
			return err
		}))
		d.mintTo(c.wallet, d.usd, uint64(usd))
	}
	return c
}

func (d *delegationWorld) owner(c delegator) context.Context {
	return security.WithPrincipal(d.w.ctx, security.Principal{SubjectID: c.user.String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{c.account.String()}, AuthTime: d.w.clk.Now(), AMR: []string{"mfa"}})
}

func (d *delegationWorld) agent(c delegator) context.Context {
	return security.WithPrincipal(d.w.ctx, security.Principal{SubjectID: c.agentID, ActorType: security.ActorAgent,
		AccountIDs: []string{c.account.String()}})
}

// grant gives the customer's agent a generous sandbox capability, valid for
// three days, covering the venue and any extra destinations.
func (d *delegationWorld) grant(c delegator, extra ...devnet.PublicKey) delegation.Capability {
	d.w.t.Helper()
	var out delegation.Capability
	require.NoError(d.w.t, d.w.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		out, _, err = d.svc.GrantCapability(d.owner(c), tx, delegation.GrantRequest{
			AccountID: c.account, AgentID: c.agentID, Mode: gates.ModeSandbox,
			InstrumentClasses: []delegation.InstrumentClass{delegation.ClassStableValue, delegation.ClassSpotToken},
			ActionClasses:     []delegation.ActionClass{delegation.ActionAcquireExposure, delegation.ActionReduceExposure, delegation.ActionProposeWithdrawal},
			MaxNotional:       money.USDFromMinor(1_000_000), MaxDailyNotional: money.USDFromMinor(500_000),
			MaxSingleTrade:       money.USDFromMinor(100_000),
			DestinationAllowlist: append([]devnet.PublicKey{d.venue.Address}, extra...),
			ChainAllowlist:       []devnet.ChainID{devnethost.ChainSolana}, ExpiresAt: d.w.clk.Now().Add(72 * time.Hour),
			IdempotencyKey: "ajobs-grant-" + uuid.NewString(),
		})
		return err
	}))
	return out
}

// swap has the agent ask to swap amountIn base units of the dollar for the
// token at the venue.
func (d *delegationWorld) swap(c delegator, cid delegation.CapabilityID, nonce uint64, amountIn int64) delegation.Swap {
	d.w.t.Helper()
	var ticket delegation.SwapTicket
	require.NoError(d.w.t, d.w.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = d.svc.RequestSwap(d.agent(c), tx, delegation.SwapRequest{CapabilityID: cid, AccountID: c.account, Nonce: nonce,
			Action: delegation.ActionAcquireExposure, VenueID: d.venue.ID, PayAssetID: d.usd.ID, ReceiveAssetID: d.tok.ID,
			AmountIn: money.QuantityFromInt64(amountIn), MaxSlippageBPS: 100, Jurisdiction: "US-CA", CorrelationID: "corr-" + uuid.NewString()})
		return err
	}))
	require.NoError(d.w.t, ticket.Refusal)
	return ticket.Swap
}

func (d *delegationWorld) getSwap(sid delegation.SwapID) delegation.Swap {
	d.w.t.Helper()
	sw, err := d.svc.GetSwap(d.w.ctx, testDB, sid)
	require.NoError(d.w.t, err)
	return sw
}

func (d *delegationWorld) reservation(rid capital.ReservationID) capital.Reservation {
	d.w.t.Helper()
	r, err := d.w.capital.Get(d.w.ctx, testDB, rid)
	require.NoError(d.w.t, err)
	return r
}

// TestIntegration_AStaleDelegatedSwapIsExpiredOnce: a swap admitted but
// never signed is left alone while its venue quote stands, is rejected by
// the job once the quote has expired -- its reservation released and its
// intent REJECTED -- and is rejected once.
func TestIntegration_AStaleDelegatedSwapIsExpiredOnce(t *testing.T) {
	w := newWorld(t)
	d := w.delegationWorld()
	c := d.delegator(10_000_000)
	capability := d.grant(c)
	sw := d.swap(c, capability.ID, 1, 1_000_000)
	require.Equal(t, delegation.SwapAdmitted, sw.Status)
	job, err := NewDelegationUpkeep(testDB, d.svc, w.clk, Settings{})
	require.NoError(t, err)

	rep := run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, "swap "+sw.ID.String()), "%+v", rep.Failures)
	assert.Equal(t, delegation.SwapAdmitted, d.getSwap(sw.ID).Status, "the quote still stands")

	d.advance(3 * time.Minute)
	rep = run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, "swap "+sw.ID.String()), "%+v", rep.Failures)
	got := d.getSwap(sw.ID)
	assert.Equal(t, delegation.SwapRejected, got.Status)
	assert.Equal(t, delegation.ReasonQuoteExpired, got.FailureCode)
	assert.Equal(t, capital.ReservationReleased, d.reservation(got.ReservationID).Status)
	in, err := w.intents.Get(w.ctx, testDB, got.IntentID)
	require.NoError(t, err)
	assert.Equal(t, econ.StatusRejected, in.Status)

	run(t, w.ctx, job)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM delegated_swap_transitions WHERE swap_id = $1 AND to_status = 'REJECTED'`, sw.ID),
		"rejected once")
}

// TestIntegration_AnUnknownDelegatedSubmissionResolvesThroughTheDevnetHost: a
// swap whose submission timed out although it landed is UNKNOWN; the job
// observes it by transaction id, produces the blocks its finality needs and
// settles it, once, and the transaction was submitted exactly once.
func TestIntegration_AnUnknownDelegatedSubmissionResolvesThroughTheDevnetHost(t *testing.T) {
	w := newWorld(t)
	d := w.delegationWorld()
	c := d.delegator(10_000_000)
	capability := d.grant(c)
	sw := d.swap(c, capability.ID, 1, 1_000_000)
	require.NoError(t, d.chain.Host.TimeoutButLand(w.ctx, devnethost.ChainSolana))
	_, err := d.svc.ExecuteSwap(w.ctx, sw.ID)
	require.Error(t, err)
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err), "%v", err)
	require.Equal(t, delegation.SwapUnknown, d.getSwap(sw.ID).Status)
	job, err := NewDelegationUpkeep(testDB, d.svc, w.clk, Settings{})
	require.NoError(t, err)

	rep := run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, "reconcile_pending"), "%+v", rep.Failures)
	got := d.getSwap(sw.ID)
	assert.Equal(t, delegation.SwapSettled, got.Status)
	in, err := w.intents.Get(w.ctx, testDB, got.IntentID)
	require.NoError(t, err)
	assert.Equal(t, econ.StatusReconciled, in.Status)

	run(t, w.ctx, job)
	assert.Equal(t, delegation.SwapSettled, d.getSwap(sw.ID).Status)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM journal_transactions WHERE reference_type = 'delegated_swap' AND reference_id = $1`,
		sw.ID.String()), "settled once")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`,
		d.chain.Network, string(got.TxID)), "submitted exactly once")
}

// TestIntegration_AWithdrawalPastItsApprovalDeadlineIsExpiredOnce: a
// delegated withdrawal waiting for its owner past its deadline is rejected by
// the job -- its reservation released -- once.
func TestIntegration_AWithdrawalPastItsApprovalDeadlineIsExpiredOnce(t *testing.T) {
	w := newWorld(t)
	d := w.delegationWorld()
	c := d.delegator(100_000_000)
	var dest delegation.Destination
	require.NoError(t, w.inTx(func(_ context.Context, tx pgx.Tx) error {
		if _, err := d.svc.SetWithdrawalPolicy(d.owner(c), tx, delegation.WithdrawalPolicy{
			AccountID: c.account, AutoApproveEnabled: true, AutoApproveMax: money.USDFromMinor(5_000), AutoApproveMaxRiskScore: 30,
			MaxRiskScore: 80, PerWithdrawalLimit: money.USDFromMinor(10_000), DailyLimit: money.USDFromMinor(20_000),
			VelocityMaxCount: 10, MinDestinationAge: time.Hour, StrongAuthRequired: true, StrongAuthMaxAge: 5 * time.Minute,
		}); err != nil {
			return err
		}
		var s [32]byte
		if _, err := rand.Read(s[:]); err != nil {
			return err
		}
		var err error
		dest, err = d.svc.AddDestination(d.owner(c), tx, delegation.DestinationRequest{AccountID: c.account, Chain: devnethost.ChainSolana,
			Address: devnet.NewKeypairFromSeed(s).PublicKey(), Label: "cold wallet"})
		return err
	}))
	capability := d.grant(c, dest.Address)
	d.advance(2 * time.Hour)
	var ticket delegation.WithdrawalTicket
	require.NoError(t, w.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = d.svc.ProposeWithdrawal(d.agent(c), tx, delegation.ProposalRequest{CapabilityID: capability.ID, AccountID: c.account,
			Nonce: 1, DestinationID: dest.ID, AssetID: d.usd.ID, Amount: money.QuantityFromInt64(60_000_000), Jurisdiction: "US-CA",
			CorrelationID: "corr-" + uuid.NewString()})
		return err
	}))
	require.NoError(t, ticket.Refusal)
	wd := ticket.Withdrawal
	require.Equal(t, delegation.WithdrawalAwaitingApproval, wd.Status, "$60 is above the $50 the policy approves on its own")
	job, err := NewDelegationUpkeep(testDB, d.svc, w.clk, Settings{})
	require.NoError(t, err)

	rep := run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, "withdrawal "+wd.ID.String()), "%+v", rep.Failures)
	still, err := d.svc.GetWithdrawal(w.ctx, testDB, wd.ID)
	require.NoError(t, err)
	assert.Equal(t, delegation.WithdrawalAwaitingApproval, still.Status, "its owner may still decide")

	d.advance(25 * time.Hour)
	rep = run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, "withdrawal "+wd.ID.String()), "%+v", rep.Failures)
	got, err := d.svc.GetWithdrawal(w.ctx, testDB, wd.ID)
	require.NoError(t, err)
	assert.Equal(t, delegation.WithdrawalRejected, got.Status)
	assert.Equal(t, delegation.ReasonApprovalExpired, got.FailureCode)
	assert.Equal(t, capital.ReservationReleased, d.reservation(got.ReservationID).Status)

	run(t, w.ctx, job)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM delegated_withdrawal_transitions WHERE withdrawal_id = $1 AND to_status = 'REJECTED'`, wd.ID),
		"rejected once")
}
