//go:build integration

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/delegation/delegationtest"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/prediction"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy/ir"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The devnet runs fast here, as in internal/delegation's own suite: a short
// blockhash window and finality after two blocks.
const (
	dgWindow = 12
	dgDepth  = 2
	dgFee    = 10
)

// delegatedEvaluator is a deterministic evaluator that decides from what the
// run was given -- the delegated toolset included -- and can act as the world
// does in the middle of an evaluation (an owner revoking, an operator
// pausing). It performs no I/O of its own.
type delegatedEvaluator struct {
	mu     sync.Mutex
	decide func(in EvalInput) []Decision
	seen   EvalInput
	calls  int
}

func (e *delegatedEvaluator) Evaluate(_ context.Context, in EvalInput) (EvalOutput, error) {
	e.mu.Lock()
	e.calls++
	e.seen = in
	decide := e.decide
	e.mu.Unlock()
	var ds []Decision
	if decide != nil {
		ds = decide(in)
	}
	return EvalOutput{
		EvaluatorVersion: "delegated-itest/v1", InputHash: bytes32("in"), TraceHash: bytes32("trace"),
		Signals: json.RawMessage(`[]`), ConditionResults: json.RawMessage(`[]`),
		Rationale: json.RawMessage(`{"summary":"delegated"}`), Decisions: ds,
	}, nil
}

// clockedAdapter answers the price dependency on the fixture's clock, so a
// test may move time (an expiring capability) and the run's data stays as
// fresh as the run's own instant.
type clockedAdapter struct {
	clk   clock.Clock
	code  string
	dials int
}

func (a *clockedAdapter) Code() string { return a.code }
func (a *clockedAdapter) Version() int { return 1 }
func (a *clockedAdapter) Fetch(context.Context, AdapterRequest) (AdapterResponse, error) {
	a.dials++
	return AdapterResponse{Payload: json.RawMessage(`{"mid":"100.00"}`), Source: "test-oracle",
		SourceEventAt: a.clk.Now().Add(-time.Second)}, nil
}

// delegatedFixture is a delegated-authority world beside a real agent run:
// the agent integration fixture's account, strategy and tools; the agent at
// BACKTEST_ELIGIBLE running PAPER; a devnet history of its own with a
// registered venue, its house book and two simulated assets; the platform's
// side of delegated authority (grants, the owner's decisions, the delegated
// signer, settlement -- delegationtest) and the agent's side, the admission
// facade, which is all the runtime holds.
type delegatedFixture struct {
	*fixture
	t         *testing.T
	agent     Agent
	account   accounts.AccountID
	tag       string
	host      *devnethost.Host
	platform  *delegation.Service
	admission *delegation.AgentAdmission
	ledger    *ledger.Service
	prices    *valuation.PriceStore
	mintAuth  devnet.Keypair
	usd, tok  assets.Asset
	venue     delegation.Venue
	wallet    devnet.PublicKey
	evaluator *delegatedEvaluator
	adapter   *clockedAdapter
	doc       *ir.IR
	runner    *Runner
}

func dgSeed(label string) [32]byte { return sha256.Sum256([]byte("agent-delegated-itest/" + label)) }

func newDelegatedFixture(t *testing.T) *delegatedFixture {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	a := f.walkTo(t, StageBacktestEligible)
	require.Equal(t, ModePaper, a.Mode, "BACKTEST_ELIGIBLE runs PAPER by default")
	account, err := accounts.ParseAccountID(f.accountID)
	require.NoError(t, err)
	tagSrc := strings.ReplaceAll(uuid.NewString(), "-", "")
	tag := tagSrc[len(tagSrc)-12:]

	// The account's owner is verified and sanctions-clear (D-055).
	owner, err := accounts.ParseUserID(f.userID)
	require.NoError(t, err)
	verifiedAt := f.clk.Now().Add(-24 * time.Hour)
	require.NoError(t, inTx(ctx, t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := compliance.NewRepository(audit.NewWriterWithBuildVersion("agent-itest")).Upsert(ctx, tx, compliance.Profile{
			UserID: owner, IdentityState: compliance.IdentityVerified, SanctionsState: compliance.SanctionsClear, VerifiedAt: &verifiedAt,
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: "agent-itest-idv", Reason: "fixture"})
		return err
	}))

	hostCfg := devnethost.DefaultConfig("ag-"+tag, config.EnvTest, devnet.NewKeypairFromSeed(dgSeed("attester-"+tag)).PublicKey())
	for i := range hostCfg.Chains {
		hostCfg.Chains[i].BlockhashWindow, hostCfg.Chains[i].FinalityDepth, hostCfg.Chains[i].FeePerTx = dgWindow, dgDepth, dgFee
	}
	host, err := devnethost.Open(ctx, devnethost.NewPgStore(testDB), hostCfg, f.clk)
	require.NoError(t, err)
	_, err = econ.EnsureSandboxDollar(ctx, testDB)
	require.NoError(t, err)

	// One configuration, two sides: what the platform builds and what the
	// agent tree may hold.
	venueName, venueSeed := "ag-"+tag, dgSeed("venue-"+tag)
	cfg := delegation.AdmissionConfig{
		DB: testDB, Clock: f.clk, Environment: config.EnvTest, Host: host,
		EnabledCapabilities: []string{"AGENT_SIGNING", "AGENT_WITHDRAWALS"}, BuildVersion: "agent-itest",
		SignerSeed: dgSeed("master-" + tag), VenueSeeds: map[string][devnet.SeedSize]byte{venueName: venueSeed},
	}
	platform, err := delegationtest.NewService(cfg)
	require.NoError(t, err)
	admission, err := delegation.NewAgentAdmission(cfg)
	require.NoError(t, err)
	signer, err := delegation.NewDelegatedSigner(cfg.SignerSeed, delegation.SignerConfig{Environment: config.EnvTest, Clock: f.clk,
		Policy: delegation.DefaultSigningPolicy(), Fees: delegation.HostFees(host)})
	require.NoError(t, err)

	df := &delegatedFixture{
		fixture: f, t: t, agent: a, account: account, tag: tag, host: host, platform: platform, admission: admission,
		ledger: ledger.NewService(f.clk, "agent-itest"), prices: valuation.NewPriceStore(f.clk),
		mintAuth: devnet.NewKeypairFromSeed(dgSeed("mint-authority-" + tag)),
		wallet:   signer.PublicKey(account, devnethost.ChainSolana),
		adapter:  &clockedAdapter{clk: f.clk, code: f.toolCode}, evaluator: &delegatedEvaluator{},
	}
	require.NoError(t, host.Airdrop(ctx, devnethost.ChainSolana, df.mintAuth.PublicKey(), 1_000_000_000))
	usdMint := devnet.NewKeypairFromSeed(dgSeed("usd-mint-" + tag)).PublicKey()
	tokMint := devnet.NewKeypairFromSeed(dgSeed("tok-mint-" + tag)).PublicKey()
	require.NoError(t, host.EnsureMint(ctx, devnethost.ChainSolana, devnet.MintConfig{Address: usdMint, Decimals: 6, Authority: df.mintAuth.PublicKey()}))
	require.NoError(t, host.EnsureMint(ctx, devnethost.ChainSolana, devnet.MintConfig{Address: tokMint, Decimals: 9, Authority: df.mintAuth.PublicKey()}))
	df.usd = df.asset("AU"+strings.ToUpper(tag[:6]), usdMint, 6, true, assets.RiskSettlement)
	df.tok = df.asset("AT"+strings.ToUpper(tag[:6]), tokMint, 9, false, assets.RiskMajor)
	df.priceTok()

	operator := delegation.Actor{Type: security.ActorOperator, ID: f.operatorID}
	var house econ.HouseAccount
	require.NoError(t, inTx(ctx, t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if house, err = econ.NewRegistry().CreateHouseAccount(ctx, tx, "ag-house-"+tag, econ.HouseLiquidityProvision, operator); err != nil {
			return err
		}
		df.venue, err = platform.RegisterVenue(ctx, tx, delegation.VenueSpec{Name: venueName, Chain: devnethost.ChainSolana,
			Address: delegation.NewVenueSigner(venueSeed).PublicKey(), HouseAccountID: house.ID, SpreadBPS: 50}, operator)
		return err
	}))
	df.fundHouse(house, df.tok, 1_000_000_000_000) // 1,000 tokens
	df.fundHouse(house, df.usd, 1_000_000_000)     // 1,000 dollars
	df.mintTo(df.venue.Address, df.tok, 1_000_000_000_000)
	df.mintTo(df.venue.Address, df.usd, 1_000_000_000)
	df.fundCustomer(100_000_000) // $100 in the delegated wallet, books and chain

	df.doc = f.authorityIR()
	predictions, err := prediction.NewLedger(f.clk)
	require.NoError(t, err)
	intents := intent.NewRepository(f.clk, event.NewOutbox(f.clk), audit.NewWriter())
	df.runner, err = NewRunner(RunnerDeps{
		DB: testDB, Clock: f.clk, Store: f.store, Pauses: NewPauseChecker(), Envelope: NewEnvelopeReader(),
		Evaluator: df.evaluator, Predictions: predictions,
		BrokerFor: func(a Authority, runID RunID) (ToolBroker, error) {
			return NewBroker(BrokerDeps{
				DB: testDB, Clock: f.clk, Registry: NewToolRegistry(), Budgets: NewBudgetReader(),
				Pauses: NewPauseChecker(), Recorder: NewInvocationRecorder(), ModelCalls: NewModelCallRecorder(),
				Adapters: map[string]ToolAdapter{ToolKey(f.toolCode, 1): df.adapter},
			}, a, runID)
		},
		EmitterFor: func(a Authority) (IntentEmitter, error) {
			return NewEmitter(EmitterDeps{Clock: f.clk, Intents: intents, Pauses: NewPauseChecker(),
				Budgets: NewBudgetReader(), Envelope: NewEnvelopeReader()}, a)
		},
		// The runtime holds the admission facade and nothing else of
		// delegated authority.
		DelegatedFor: func(a Authority) (DelegatedProposer, error) {
			return NewDelegatedEmitter(DelegatedEmitterDeps{Clock: f.clk, Authority: admission,
				Pauses: NewPauseChecker(), Budgets: NewBudgetReader()}, a)
		},
		IRFor:        func(context.Context, db.Querier, string) (*ir.IR, error) { return df.doc, nil },
		BuildVersion: "itest",
	})
	require.NoError(t, err)
	return df
}

func (df *delegatedFixture) asset(symbol string, mint devnet.PublicKey, decimals uint8, stable bool, class assets.RiskClass) assets.Asset {
	df.t.Helper()
	a := assets.Asset{Chain: string(devnethost.ChainSolana), MintAddress: mint.String(), Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: symbol, Name: symbol + " (simulated devnet token)", Decimals: decimals,
		RiskClass: class, Status: assets.StatusActive}
	if stable {
		a.IsStablecoin, a.PegCurrency = true, valuation.USDPeg
	}
	out, err := assets.NewRepository().Create(context.Background(), testDB, a)
	require.NoError(df.t, err)
	return out
}

// priceTok marks the token at 100.00 sandbox dollars, observed now.
func (df *delegatedFixture) priceTok() {
	df.t.Helper()
	_, err := df.prices.RecordPrice(context.Background(), testDB, valuation.PriceObservation{AssetID: df.tok.ID,
		QuoteAssetID: econ.SandboxDollarID(), Mantissa: money.QuantityFromInt64(10_000), Scale: 2, Source: "agent-itest",
		ObservedAt: df.clk.Now()})
	require.NoError(df.t, err)
}

func (df *delegatedFixture) fundHouse(house econ.HouseAccount, a assets.Asset, q int64) {
	df.t.Helper()
	qty := money.QuantityFromInt64(q)
	require.NoError(df.t, inTx(context.Background(), df.t, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := df.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindHouseFunding, IdempotencyKey: "ag-house:" + ref,
			Reference: ledger.FinancialEventReference{Type: "house_funding", ID: ref}, EffectiveAt: df.clk.Now(),
			Entries: []ledger.Entry{
				{Account: house.Account(ledger.CodeHouseInventory, a.ID), Side: ledger.Debit, Quantity: qty},
				{Account: house.Account(ledger.CodeHouseCapital, a.ID), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

// fundCustomer credits q base units of the dollar to the account's books
// (SANDBOX_FUNDING) and the same units to its delegated wallet on chain.
func (df *delegatedFixture) fundCustomer(q int64) {
	df.t.Helper()
	qty := money.QuantityFromInt64(q)
	require.NoError(df.t, inTx(context.Background(), df.t, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := df.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "ag-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: df.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(df.account, ledger.CodeWallet, df.usd.ID), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(df.account, ledger.CodeCapital, df.usd.ID), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
	df.mintTo(df.wallet, df.usd, uint64(q))
}

// mintTo issues q base units of a fixture asset to owner on chain and waits
// for finality.
func (df *delegatedFixture) mintTo(owner devnet.PublicKey, a assets.Asset, q uint64) {
	df.t.Helper()
	ctx := context.Background()
	mint, err := devnet.ParsePublicKey(a.MintAddress)
	require.NoError(df.t, err)
	bh, err := df.host.LatestBlockhash(ctx, devnethost.ChainSolana)
	require.NoError(df.t, err)
	tx, err := devnet.Sign(devnet.Message{ChainID: devnethost.ChainSolana, RecentBlockhash: bh, FeePayer: df.mintAuth.PublicKey(),
		Instructions: []devnet.Instruction{
			devnet.TokenMintTo{Mint: mint, ToOwner: owner, Amount: q, Authority: df.mintAuth.PublicKey()},
			devnet.Memo{Data: []byte(uuid.NewString())},
		}}, df.mintAuth)
	require.NoError(df.t, err)
	raw, err := tx.Encode()
	require.NoError(df.t, err)
	txID, err := df.host.Submit(ctx, devnethost.ChainSolana, raw)
	require.NoError(df.t, err)
	_, err = df.host.ProduceBlocks(ctx, devnethost.ChainSolana, 1+dgDepth)
	require.NoError(df.t, err)
	st, err := df.host.Status(ctx, devnethost.ChainSolana, txID)
	require.NoError(df.t, err)
	require.Equal(df.t, devnet.TxFinalized, st.State, "mint %s", st.Err)
}

func (df *delegatedFixture) chainBalance(owner devnet.PublicKey, a assets.Asset) uint64 {
	df.t.Helper()
	mint, err := devnet.ParsePublicKey(a.MintAddress)
	require.NoError(df.t, err)
	b, err := df.host.TokenBalance(context.Background(), devnethost.ChainSolana, owner, mint)
	require.NoError(df.t, err)
	return b
}

func (df *delegatedFixture) ownerCtx() context.Context { return ctxAs(df.owner()) }

// grant has the account's owner grant the agent a capability: by default
// every action class, $1,000 in all, $5,000 a day... bounded below by the
// single-trade cap, the venue and any extra destination allow-listed, a day
// long, and the owner's approval required for every swap.
func (df *delegatedFixture) grant(mutate func(*delegation.GrantRequest), extra ...devnet.PublicKey) delegation.Capability {
	df.t.Helper()
	req := delegation.GrantRequest{
		AccountID: df.account, AgentID: df.agent.ID.String(), Mode: "SANDBOX",
		InstrumentClasses: []delegation.InstrumentClass{delegation.ClassStableValue, delegation.ClassSpotToken},
		ActionClasses:     []delegation.ActionClass{delegation.ActionAcquireExposure, delegation.ActionReduceExposure, delegation.ActionProposeWithdrawal},
		MaxNotional:       money.USDFromMinor(1_000_000), MaxDailyNotional: money.USDFromMinor(500_000),
		MaxSingleTrade:        money.USDFromMinor(100_000),
		DestinationAllowlist:  append([]devnet.PublicKey{df.venue.Address}, extra...),
		ChainAllowlist:        []devnet.ChainID{devnethost.ChainSolana},
		ExpiresAt:             df.clk.Now().Add(24 * time.Hour),
		RequiresHumanApproval: true,
		IdempotencyKey:        "grant-" + uuid.NewString(),
	}
	if mutate != nil {
		mutate(&req)
	}
	var out delegation.Capability
	require.NoError(df.t, inTx(context.Background(), df.t, func(_ context.Context, tx pgx.Tx) error {
		var err error
		out, _, err = df.platform.GrantCapability(df.ownerCtx(), tx, req)
		return err
	}))
	return out
}

func (df *delegatedFixture) revoke(cid delegation.CapabilityID) {
	df.t.Helper()
	require.NoError(df.t, inTx(context.Background(), df.t, func(_ context.Context, tx pgx.Tx) error {
		_, err := df.platform.RevokeCapability(df.ownerCtx(), tx, delegation.RevokeRequest{CapabilityID: cid, Reason: "owner revoked it"})
		return err
	}))
}

// withdrawalPolicy auto-approves up to $50 in a test environment, caps a
// withdrawal at $100 and a day at $200.
func (df *delegatedFixture) withdrawalPolicy() {
	df.t.Helper()
	require.NoError(df.t, inTx(context.Background(), df.t, func(_ context.Context, tx pgx.Tx) error {
		_, err := df.platform.SetWithdrawalPolicy(df.ownerCtx(), tx, delegation.WithdrawalPolicy{
			AccountID: df.account, AutoApproveEnabled: true, AutoApproveMax: money.USDFromMinor(5_000), AutoApproveMaxRiskScore: 30,
			MaxRiskScore: 80, PerWithdrawalLimit: money.USDFromMinor(10_000), DailyLimit: money.USDFromMinor(20_000),
			VelocityMaxCount: 10, StrongAuthRequired: true, StrongAuthMaxAge: 5 * time.Minute,
		})
		return err
	}))
}

// destination allow-lists a fresh address on the account.
func (df *delegatedFixture) destination(label string) delegation.Destination {
	df.t.Helper()
	var d delegation.Destination
	require.NoError(df.t, inTx(context.Background(), df.t, func(_ context.Context, tx pgx.Tx) error {
		var err error
		d, err = df.platform.AddDestination(df.ownerCtx(), tx, delegation.DestinationRequest{AccountID: df.account,
			Chain: devnethost.ChainSolana, Address: devnet.NewKeypairFromSeed(dgSeed("dest-" + label + "-" + df.tag)).PublicKey(),
			Label: label})
		return err
	}))
	return d
}

// openRun opens a PAPER run of the agent at the clock's instant.
func (df *delegatedFixture) openRun() Run {
	df.t.Helper()
	run := Run{
		ID: NewRunID(), AgentID: df.agent.ID, AgentVersion: df.agent.Version, StrategyVersionID: df.agent.StrategyVersionID,
		AccountID: df.agent.AccountID, Mode: df.agent.Mode, TriggerName: "tick", TriggerKind: TriggerOnInterval,
		TriggerDedupKey: bytes32(newUUID()), DecisionTime: df.clk.Now(), Status: RunStarted, StartedAt: df.clk.Now(),
		CorrelationID: "corr-" + newUUID(),
	}
	require.NoError(df.t, inTx(context.Background(), df.t, func(ctx context.Context, tx pgx.Tx) error {
		created, _, err := df.store.CreateRun(ctx, tx, run)
		run = created
		return err
	}))
	return run
}

// runWith opens a run and executes it with the given decisions after the
// run's prediction.
func (df *delegatedFixture) runWith(decide func(in EvalInput) []Decision) (Run, RunResult, error) {
	df.t.Helper()
	run := df.openRun()
	df.evaluator.decide = func(in EvalInput) []Decision {
		return append([]Decision{predictionDecision(df.t, mustInstrument(df.t, df.instrumentID))}, decide(in)...)
	}
	res, err := df.runner.Run(context.Background(), run.ID)
	return run, res, err
}

func (df *delegatedFixture) swapDecision(cid string, amountIn int64) Decision {
	return Decision{ActionName: "rebalance", Kind: ActionRequestDelegatedSwap, DelegatedSwap: &DelegatedSwapRequest{
		CapabilityID: cid, Action: "ACQUIRE_EXPOSURE", VenueID: df.venue.ID.String(), PayAssetID: df.usd.ID.String(),
		ReceiveAssetID: df.tok.ID.String(), AmountIn: money.QuantityFromInt64(amountIn), MaxSlippageBPS: 100, Jurisdiction: "US-CA",
	}}
}

func (df *delegatedFixture) withdrawalDecision(cid, destination string, amount int64) Decision {
	return Decision{ActionName: "cash-out", Kind: ActionProposeDelegatedWithdrawal, DelegatedWithdrawal: &DelegatedWithdrawalRequest{
		CapabilityID: cid, DestinationID: destination, AssetID: df.usd.ID.String(), Amount: money.QuantityFromInt64(amount),
		Jurisdiction: "US-CA",
	}}
}

func (df *delegatedFixture) count(sql string, args ...any) int {
	df.t.Helper()
	var n int
	require.NoError(df.t, testDB.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

// storedEvidence is the run's evidence as the database holds it.
func (df *delegatedFixture) storedEvidence(runID RunID) (DelegatedToolsRecord, []DelegatedRecord) {
	df.t.Helper()
	run, err := df.store.GetRun(context.Background(), testDB, runID)
	require.NoError(df.t, err)
	var elems []json.RawMessage
	require.NoError(df.t, json.Unmarshal(run.Evidence, &elems))
	var tools DelegatedToolsRecord
	for _, el := range elems {
		var k struct {
			Kind string `json:"kind"`
		}
		require.NoError(df.t, json.Unmarshal(el, &k))
		if k.Kind == EvidenceKindDelegatedTools {
			require.NoError(df.t, json.Unmarshal(el, &tools))
		}
	}
	return tools, delegatedRecordsOf(run.Evidence)
}

// TestIntegration_AnAgentRunRequestsADelegatedSwapThatSettlesAfterTheOwnerApproves
// is the whole path through the runtime: a PAPER run of an agent holding a
// capability is offered the delegated swap tool, requests a swap that
// delegation admits under the capability and holds for the owner; the run
// records the swap's id and status and never signs; the owner approves the
// exact plan, and the platform's executor -- not the agent -- has it signed
// after inspection and settled on the devnet host. Re-running the run asks
// for nothing again.
func TestIntegration_AnAgentRunRequestsADelegatedSwapThatSettlesAfterTheOwnerApproves(t *testing.T) {
	df := newDelegatedFixture(t)
	capability := df.grant(nil)
	cid := capability.ID.String()

	run, res, err := df.runWith(func(in EvalInput) []Decision {
		return []Decision{df.swapDecision(cid, 2_000_000)} // pay $2 for 0.02 token less the venue's spread
	})
	require.NoError(t, err)
	require.Equal(t, RunPredicted, res.Run.Status, "a run that proposed ends PREDICTED with its proposal recorded")
	require.NotEmpty(t, res.PredictionID)
	assert.Empty(t, res.IntentID, "no trade intent: the request went to delegation")

	// The evaluator was offered exactly this capability's tools.
	offered := df.evaluator.seen.Delegated
	require.Len(t, offered.Tools, 2, "%+v", offered)
	swapTool, ok := offered.Lookup(ActionRequestDelegatedSwap, cid)
	require.True(t, ok)
	assert.Equal(t, capability.Terms.MaxSingleTrade, swapTool.MaxSingleTradeUSD)
	assert.True(t, swapTool.RequiresHumanApproval)

	// The run got the subject's id and status back, and recorded them.
	require.Len(t, res.Delegated, 1)
	rec := res.Delegated[0]
	assert.False(t, rec.Refused(), "%+v", rec)
	assert.Equal(t, "SWAP", rec.SubjectKind)
	assert.Equal(t, string(delegation.SwapAwaitingApproval), rec.Status)
	assert.Equal(t, cid, rec.CapabilityID)
	assert.Equal(t, "rebalance", rec.Action)
	tools, stored := df.storedEvidence(run.ID)
	assert.Equal(t, []DelegatedToolRef{{Tool: string(ActionProposeDelegatedWithdrawal), CapabilityID: cid},
		{Tool: string(ActionRequestDelegatedSwap), CapabilityID: cid}}, sortedRefs(tools.Tools))
	require.Len(t, stored, 1)
	assert.Equal(t, rec, stored[0])

	sid, err := delegation.ParseSwapID(rec.SubjectID)
	require.NoError(t, err)
	sw, err := df.platform.GetSwap(context.Background(), testDB, sid)
	require.NoError(t, err)
	assert.Equal(t, delegation.SwapAwaitingApproval, sw.Status)
	assert.Equal(t, df.wallet, sw.Wallet)
	assert.Equal(t, int64(200), sw.NotionalUSD.Minor())
	assert.Equal(t, DelegatedNonce(run.ID, "rebalance"), sw.RequestNonce, "the runtime chose the nonce, from the run and the action")
	assert.Empty(t, sw.TxID, "nothing is signed by the agent's request")
	assert.Zero(t, df.count(`SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1`, sid))
	var actorType, agentID, delegationID, correlation string
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT actor_type, coalesce(agent_id::text, ''),
		coalesce(delegation_id::text, ''), coalesce(correlation_id, '') FROM economic_intents WHERE id = $1`, sw.IntentID).
		Scan(&actorType, &agentID, &delegationID, &correlation))
	assert.Equal(t, "AGENT", actorType)
	assert.Equal(t, df.agent.ID.String(), agentID)
	assert.Equal(t, cid, delegationID)
	assert.Equal(t, run.CorrelationID, correlation)

	// The run resumes: the recorded request is answered from its record and
	// never made again.
	again, err := df.runner.Run(context.Background(), run.ID)
	require.NoError(t, err)
	require.Len(t, again.Delegated, 1)
	assert.Equal(t, rec, again.Delegated[0])
	assert.Equal(t, 1, df.count(`SELECT count(*) FROM delegated_swaps WHERE capability_id = $1`, capability.ID))
	assert.Equal(t, 1, df.count(`SELECT count(*) FROM delegated_capability_usage WHERE capability_id = $1`, capability.ID))
	assert.Equal(t, 1, df.count(`SELECT count(*) FROM economic_intents WHERE delegation_id = $1`, capability.ID))

	// The owner approves the exact plan; the platform's executor signs after
	// inspection, submits once and settles on the devnet host.
	require.NoError(t, inTx(context.Background(), t, func(_ context.Context, tx pgx.Tx) error {
		out, err := df.platform.DecideSwap(df.ownerCtx(), tx, delegation.SwapDecision{SwapID: sid, PlanHash: sw.PlanHash,
			Approve: true, Reason: "the owner approves the plan"})
		if err == nil {
			assert.Equal(t, delegation.SwapApproved, out.Status)
		}
		return err
	}))
	settled, err := df.platform.ExecuteSwap(context.Background(), sid)
	require.NoError(t, err)
	require.True(t, settled.Settled)
	require.Equal(t, delegation.SwapSettled, settled.Swap.Status, "failure %q", settled.Swap.FailureCode)
	assert.Equal(t, 1, df.count(`SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1 AND verdict = 'APPROVED'`, sid),
		"signed once, by the platform's delegated signer, after inspection")
	st, err := df.host.Status(context.Background(), devnethost.ChainSolana, settled.Swap.TxID)
	require.NoError(t, err)
	assert.Equal(t, devnet.TxFinalized, st.State)
	assert.Equal(t, uint64(98_000_000), df.chainBalance(df.wallet, df.usd), "the wallet paid exactly $2")
	assert.Equal(t, settled.Swap.AmountOut.String(), money.QuantityFromInt64(int64(df.chainBalance(df.wallet, df.tok))).String())
	j, err := df.ledger.Transaction(context.Background(), testDB, *settled.Swap.JournalTransactionID)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindSimTrade, j.Kind)
}

func sortedRefs(in []DelegatedToolRef) []DelegatedToolRef {
	out := append([]DelegatedToolRef(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Tool < out[j-1].Tool; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestIntegration_WithoutAUsableCapabilityTheRunHoldsNoDelegatedTool: no
// capability, a revoked one or an expired one offers no tool, and a decision
// that asks for one anyway is refused by the runtime and recorded -- nothing
// reaches delegation. A capability revoked after the tools were offered is
// refused by delegation itself, and that refusal is recorded too.
func TestIntegration_WithoutAUsableCapabilityTheRunHoldsNoDelegatedTool(t *testing.T) {
	noTool := func(t *testing.T, df *delegatedFixture, cid string) {
		t.Helper()
		run, res, err := df.runWith(func(in EvalInput) []Decision {
			assert.Empty(t, in.Delegated.Tools)
			assert.Equal(t, DelegatedReasonNoCapability, in.Delegated.Reason)
			return []Decision{df.swapDecision(cid, 1_000_000)}
		})
		require.NoError(t, err, "a refusal is a recorded outcome, not an error")
		assert.Equal(t, RunSkipped, res.Run.Status)
		assert.Equal(t, SkipEffectMismatch, res.SkipReason)
		require.Len(t, res.Delegated, 1)
		assert.True(t, res.Delegated[0].Refused())
		assert.Equal(t, string(errs.CodeEffectForbidden), res.Delegated[0].Refusal)
		assert.Equal(t, DelegatedReasonToolUnavailable, res.Delegated[0].Reason)
		tools, stored := df.storedEvidence(run.ID)
		assert.Empty(t, tools.Tools)
		assert.Equal(t, DelegatedReasonNoCapability, tools.Reason)
		require.Len(t, stored, 1)
		assert.Equal(t, res.Delegated[0], stored[0])
		assert.Zero(t, df.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1`, df.account),
			"nothing reached delegation")
	}

	t.Run("no capability", func(t *testing.T) {
		df := newDelegatedFixture(t)
		noTool(t, df, newUUID())
	})
	t.Run("a revoked capability", func(t *testing.T) {
		df := newDelegatedFixture(t)
		c := df.grant(nil)
		df.revoke(c.ID)
		noTool(t, df, c.ID.String())
	})
	t.Run("an expired capability", func(t *testing.T) {
		df := newDelegatedFixture(t)
		c := df.grant(func(r *delegation.GrantRequest) { r.ExpiresAt = df.clk.Now().Add(time.Hour) })
		df.clk.Advance(2 * time.Hour)
		df.priceTok()
		noTool(t, df, c.ID.String())
	})
	t.Run("a capability revoked after the tools were offered", func(t *testing.T) {
		df := newDelegatedFixture(t)
		c := df.grant(nil)
		run, res, err := df.runWith(func(in EvalInput) []Decision {
			require.Len(t, in.Delegated.Tools, 2, "the capability was usable when the tools were read")
			df.revoke(c.ID) // the owner, in the middle of the run
			return []Decision{df.swapDecision(c.ID.String(), 1_000_000)}
		})
		require.Error(t, err)
		assert.Equal(t, RunFailed, res.Run.Status)
		require.Len(t, res.Delegated, 1)
		rec := res.Delegated[0]
		assert.Equal(t, string(errs.CodeRiskRejected), rec.Refusal, "%+v", rec)
		assert.Contains(t, rec.Detail, delegation.ReasonCapabilityRevoked)
		_, stored := df.storedEvidence(run.ID)
		require.Len(t, stored, 1)
		assert.Equal(t, rec, stored[0])
		// Delegation recorded the same refusal: its intent is REJECTED, no
		// swap, no usage, nothing reserved.
		assert.Equal(t, 1, df.count(`SELECT count(*) FROM economic_intents WHERE delegation_id = $1 AND status = 'REJECTED'`, c.ID))
		assert.Zero(t, df.count(`SELECT count(*) FROM delegated_swaps WHERE capability_id = $1`, c.ID))
		assert.Zero(t, df.count(`SELECT count(*) FROM delegated_capability_usage WHERE capability_id = $1`, c.ID))
		// The refusal is final: the run is terminal and running it again asks
		// for nothing.
		again, err := df.runner.Run(context.Background(), run.ID)
		require.NoError(t, err)
		assert.Equal(t, RunFailed, again.Run.Status)
		assert.Equal(t, 1, df.count(`SELECT count(*) FROM economic_intents WHERE delegation_id = $1`, c.ID))
	})
}

// TestIntegration_TheCapabilityBoundsTheAgentsRequests: the single-trade
// limit refuses a swap above it, and a withdrawal to a destination outside
// the capability's allow-list -- or not on the account's at all -- is
// refused. Every refusal is recorded on the run and ends it; none is retried.
func TestIntegration_TheCapabilityBoundsTheAgentsRequests(t *testing.T) {
	t.Run("above the single-trade limit", func(t *testing.T) {
		df := newDelegatedFixture(t)
		c := df.grant(func(r *delegation.GrantRequest) {
			r.MaxSingleTrade, r.MaxDailyNotional, r.MaxNotional = money.USDFromMinor(500), money.USDFromMinor(800), money.USDFromMinor(1_000)
		})
		_, res, err := df.runWith(func(EvalInput) []Decision {
			return []Decision{df.swapDecision(c.ID.String(), 5_000_001)} // $5.000001, one cent over the cap once valued
		})
		require.Error(t, err)
		assert.Equal(t, RunFailed, res.Run.Status)
		require.Len(t, res.Delegated, 1)
		assert.Equal(t, string(errs.CodeRiskRejected), res.Delegated[0].Refusal)
		assert.Contains(t, res.Delegated[0].Detail, delegation.ReasonSingleLimitExceeded)
		assert.Zero(t, df.count(`SELECT count(*) FROM delegated_swaps WHERE capability_id = $1`, c.ID))
		assert.Zero(t, df.count(`SELECT count(*) FROM delegated_capability_usage WHERE capability_id = $1`, c.ID))
		assert.Zero(t, df.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1`, df.account),
			"nothing is reserved for a refused request")
	})
	t.Run("a purchase labelled as a reduction", func(t *testing.T) {
		df := newDelegatedFixture(t)
		c := df.grant(func(r *delegation.GrantRequest) {
			r.ActionClasses = []delegation.ActionClass{delegation.ActionReduceExposure}
		})
		_, res, err := df.runWith(func(EvalInput) []Decision {
			d := df.swapDecision(c.ID.String(), 1_000_000) // pays the stable unit for the token: a purchase
			d.DelegatedSwap.Action = string(delegation.ActionReduceExposure)
			return []Decision{d}
		})
		require.Error(t, err)
		assert.Equal(t, RunFailed, res.Run.Status)
		require.Len(t, res.Delegated, 1)
		assert.Equal(t, string(errs.CodeForbidden), res.Delegated[0].Refusal, "%+v", res.Delegated[0])
		assert.Equal(t, delegation.ReasonActionMismatch, res.Delegated[0].Reason, "the label is not what the swap does")
		assert.Zero(t, df.count(`SELECT count(*) FROM delegated_swaps WHERE capability_id = $1`, c.ID))
		assert.Zero(t, df.count(`SELECT count(*) FROM delegated_capability_usage WHERE capability_id = $1`, c.ID))
		assert.Equal(t, 1, df.count(`SELECT count(*) FROM economic_intents WHERE delegation_id = $1 AND status = 'REJECTED'
			AND rejection_code = $2`, c.ID, delegation.ReasonActionMismatch), "delegation recorded the refusal")
	})
	t.Run("a withdrawal outside the capability's allow-list", func(t *testing.T) {
		df := newDelegatedFixture(t)
		df.withdrawalPolicy()
		allowed := df.destination("allowed")
		elsewhere := df.destination("elsewhere") // on the account's allow-list, not the capability's
		c := df.grant(nil, allowed.Address)
		_, res, err := df.runWith(func(EvalInput) []Decision {
			return []Decision{df.withdrawalDecision(c.ID.String(), elsewhere.ID.String(), 1_000_000)}
		})
		require.Error(t, err)
		assert.Equal(t, RunFailed, res.Run.Status)
		require.Len(t, res.Delegated, 1)
		assert.Equal(t, string(errs.CodeRiskRejected), res.Delegated[0].Refusal, "%+v", res.Delegated[0])
		assert.Contains(t, res.Delegated[0].Detail, delegation.ReasonDestinationNotGranted)
		assert.Zero(t, df.count(`SELECT count(*) FROM delegated_withdrawals WHERE capability_id = $1`, c.ID))
		assert.Equal(t, 1, df.count(`SELECT count(*) FROM economic_intents WHERE delegation_id = $1 AND status = 'REJECTED'`, c.ID))
	})
	t.Run("a withdrawal to a destination the account never allow-listed", func(t *testing.T) {
		df := newDelegatedFixture(t)
		df.withdrawalPolicy()
		c := df.grant(nil)
		_, res, err := df.runWith(func(EvalInput) []Decision {
			return []Decision{df.withdrawalDecision(c.ID.String(), newUUID(), 1_000_000)}
		})
		require.Error(t, err)
		assert.Equal(t, RunFailed, res.Run.Status)
		require.Len(t, res.Delegated, 1)
		assert.Equal(t, string(errs.CodeNotFound), res.Delegated[0].Refusal)
		assert.Equal(t, delegation.ReasonWithdrawalDestinationNotAllowlisted, res.Delegated[0].Reason)
		assert.Zero(t, df.count(`SELECT count(*) FROM economic_intents WHERE delegation_id = $1`, c.ID),
			"refused before anything was recorded by delegation; the run's evidence is the record")
	})
}

// TestIntegration_TheAgentsBoundsApplyToItsDelegatedRequests: a pause raised
// mid-run stops the request inside its transaction, and delegated requests
// spend the same hourly order-rate allowance as trade intents.
func TestIntegration_TheAgentsBoundsApplyToItsDelegatedRequests(t *testing.T) {
	t.Run("a pause raised during the run", func(t *testing.T) {
		df := newDelegatedFixture(t)
		c := df.grant(nil)
		// The first attempt commits the prediction, so the second reaches its
		// delegated request with nothing left to check the pause but the
		// request's own transaction.
		run := df.openRun()
		predict := predictionDecision(t, mustInstrument(t, df.instrumentID))
		df.evaluator.decide = func(EvalInput) []Decision { return []Decision{predict} }
		first, err := df.runner.Run(context.Background(), run.ID)
		require.NoError(t, err)
		require.Equal(t, RunPredicted, first.Run.Status)
		df.evaluator.decide = func(EvalInput) []Decision {
			require.NoError(t, inTx(ctxAs(df.operator()), t, func(ctx context.Context, tx pgx.Tx) error {
				_, err := df.lifecycle.Pause(ctx, tx, df.agent.ID, PauseRequest{ReasonCode: PauseOperator,
					Reason: "paused while the run evaluated", OpenOrdersPolicy: LeaveOpenOrders})
				return err
			}))
			return []Decision{predict, df.swapDecision(c.ID.String(), 1_000_000)}
		}
		res, err := df.runner.Run(context.Background(), run.ID)
		require.NoError(t, err)
		assert.Equal(t, RunSkipped, res.Run.Status)
		assert.Equal(t, SkipKillSwitch, res.SkipReason)
		require.Len(t, res.Delegated, 1)
		assert.Equal(t, DelegatedReasonAgentPaused, res.Delegated[0].Reason)
		assert.Zero(t, df.count(`SELECT count(*) FROM economic_intents WHERE delegation_id = $1`, c.ID), "nothing reached delegation")
	})
	t.Run("the order-rate budget", func(t *testing.T) {
		df := newDelegatedFixture(t)
		df.doc.Envelope.MaxIntentsPerHour = 1
		c := df.grant(func(r *delegation.GrantRequest) { r.RequiresHumanApproval = false })
		_, first, err := df.runWith(func(EvalInput) []Decision { return []Decision{df.swapDecision(c.ID.String(), 1_000_000)} })
		require.NoError(t, err)
		require.Len(t, first.Delegated, 1)
		require.False(t, first.Delegated[0].Refused(), "%+v", first.Delegated[0])
		_, second, err := df.runWith(func(EvalInput) []Decision { return []Decision{df.swapDecision(c.ID.String(), 1_000_000)} })
		require.NoError(t, err)
		assert.Equal(t, RunSkipped, second.Run.Status)
		assert.Equal(t, SkipRateLimited, second.SkipReason)
		require.Len(t, second.Delegated, 1)
		assert.Equal(t, DelegatedReasonOrderRate, second.Delegated[0].Reason)
		assert.Equal(t, 1, df.count(`SELECT count(*) FROM delegated_swaps WHERE capability_id = $1`, c.ID))
	})
}

// TestIntegration_ErasingTheRunsRecordCannotMakeASecondRequest bypasses the
// runtime: the run's own record of its delegated request is erased with raw
// SQL as the migration owner, and the run resumes. The request it makes again
// carries the same nonce, so delegation answers it as the replay it is -- the
// original swap, no second intent, usage or reservation -- and the database
// refuses to rewrite a swap's nonce to make room for another (DG002).
func TestIntegration_ErasingTheRunsRecordCannotMakeASecondRequest(t *testing.T) {
	df := newDelegatedFixture(t)
	c := df.grant(func(r *delegation.GrantRequest) { r.RequiresHumanApproval = false })
	run, first, err := df.runWith(func(EvalInput) []Decision { return []Decision{df.swapDecision(c.ID.String(), 1_000_000)} })
	require.NoError(t, err)
	require.Len(t, first.Delegated, 1)
	require.False(t, first.Delegated[0].Refused(), "%+v", first.Delegated[0])

	ctx := context.Background()
	owner, err := pgx.Connect(ctx, testMigrateURL)
	require.NoError(t, err)
	defer func() { _ = owner.Close(ctx) }()
	tag, err := owner.Exec(ctx, `UPDATE agent_runs SET evidence = '[]'::jsonb WHERE id = $1`, run.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())

	again, err := df.runner.Run(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, again.Delegated, 1)
	rec := again.Delegated[0]
	assert.True(t, rec.Replay, "delegation answered the same request, not a new one")
	assert.Equal(t, first.Delegated[0].SubjectID, rec.SubjectID)
	assert.Equal(t, first.Delegated[0].Nonce, rec.Nonce)
	assert.Equal(t, 1, df.count(`SELECT count(*) FROM delegated_swaps WHERE capability_id = $1`, c.ID))
	assert.Equal(t, 1, df.count(`SELECT count(*) FROM delegated_capability_usage WHERE capability_id = $1`, c.ID))
	assert.Equal(t, 1, df.count(`SELECT count(*) FROM economic_intents WHERE delegation_id = $1`, c.ID))
	assert.Equal(t, 1, df.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1`, df.account))

	_, err = owner.Exec(ctx, `UPDATE delegated_swaps SET request_nonce = request_nonce + 1 WHERE id = $1`, rec.SubjectID)
	require.Error(t, err)
	assert.Equal(t, "DG002", db.SQLState(err), "%v", err)
}

// TestIntegration_ConcurrentRunsCannotSpendOneOrderRateAllowanceTwice: four
// runs of one agent request a delegated swap at the same instant against an
// allowance of one proposal an hour, each in its own transaction. Exactly one
// is admitted; the others are refused RATE_LIMITED and recorded.
func TestIntegration_ConcurrentRunsCannotSpendOneOrderRateAllowanceTwice(t *testing.T) {
	df := newDelegatedFixture(t)
	df.doc.Envelope.MaxIntentsPerHour = 1
	c := df.grant(func(r *delegation.GrantRequest) { r.RequiresHumanApproval = false })
	predict := predictionDecision(t, mustInstrument(t, df.instrumentID))
	df.evaluator.decide = func(EvalInput) []Decision {
		return []Decision{predict, df.swapDecision(c.ID.String(), 1_000_000)}
	}
	const n = 4
	runs := make([]Run, n)
	for i := range runs {
		runs[i] = df.openRun()
	}
	results := make([]RunResult, n)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = df.runner.Run(context.Background(), runs[i].ID)
		}(i)
	}
	wg.Wait()

	admitted, limited := 0, 0
	for _, res := range results {
		require.Len(t, res.Delegated, 1, "%+v", res)
		switch rec := res.Delegated[0]; {
		case !rec.Refused():
			admitted++
		case rec.Reason == DelegatedReasonOrderRate:
			assert.Equal(t, SkipRateLimited, res.SkipReason)
			limited++
		default:
			t.Errorf("unexpected refusal %+v", rec)
		}
	}
	assert.Equal(t, 1, admitted)
	assert.Equal(t, n-1, limited)
	assert.Equal(t, 1, df.count(`SELECT count(*) FROM delegated_swaps WHERE capability_id = $1`, c.ID))
}

// TestIntegration_AnAgentNeverProducesATransferAWithdrawalOrASignature: what
// the agent's run produces through the delegated tools is a swap admitted
// for signing and a withdrawal the account's policy approved -- and nothing
// else. No signature, no submission, no effect, no posting, no internal
// transfer and no withdrawal of the funding path; the WITHDRAW intent is the
// withdrawal service's, never the agent's; and the platform refuses the
// agent's principal at its signing and execution steps. Only the platform's
// executor carries either further.
func TestIntegration_AnAgentNeverProducesATransferAWithdrawalOrASignature(t *testing.T) {
	df := newDelegatedFixture(t)
	df.withdrawalPolicy()
	dest := df.destination("cold")
	c := df.grant(func(r *delegation.GrantRequest) { r.RequiresHumanApproval = false }, dest.Address)
	walletUSD := df.chainBalance(df.wallet, df.usd)

	_, res, err := df.runWith(func(EvalInput) []Decision {
		return []Decision{df.swapDecision(c.ID.String(), 1_000_000), df.withdrawalDecision(c.ID.String(), dest.ID.String(), 2_000_000)}
	})
	require.NoError(t, err)
	require.Equal(t, RunPredicted, res.Run.Status)
	require.Len(t, res.Delegated, 2)
	swapRec, withdrawalRec := res.Delegated[0], res.Delegated[1]
	require.False(t, swapRec.Refused(), "%+v", swapRec)
	require.False(t, withdrawalRec.Refused(), "%+v", withdrawalRec)
	assert.Equal(t, string(delegation.SwapAdmitted), swapRec.Status, "admitted, not signed")
	assert.Equal(t, string(delegation.WithdrawalApproved), withdrawalRec.Status, "approved by the account's own policy, not signed")
	sid, err := delegation.ParseSwapID(swapRec.SubjectID)
	require.NoError(t, err)
	wid, err := delegation.ParseWithdrawalID(withdrawalRec.SubjectID)
	require.NoError(t, err)

	assert.Zero(t, df.count(`SELECT count(*) FROM delegated_signing_decisions WHERE account_id = $1`, df.account), "no signature")
	assert.Zero(t, df.count(`SELECT count(*) FROM delegated_swaps WHERE account_id = $1 AND (tx_id IS NOT NULL OR signed_tx IS NOT NULL)`, df.account))
	assert.Zero(t, df.count(`SELECT count(*) FROM delegated_withdrawals WHERE account_id = $1 AND (tx_id IS NOT NULL OR signed_tx IS NOT NULL)`, df.account))
	assert.Zero(t, df.count(`SELECT count(*) FROM economic_effects WHERE account_id = $1`, df.account), "no effect claimed")
	assert.Zero(t, df.count(`SELECT count(*) FROM journal_transactions WHERE reference_id IN ($1, $2)`, sid.String(), wid.String()), "nothing posted")
	assert.Zero(t, df.count(`SELECT count(*) FROM internal_transfers WHERE sender_account_id = $1`, df.account), "no internal transfer")
	assert.Zero(t, df.count(`SELECT count(*) FROM withdrawals WHERE account_id = $1`, df.account), "no withdrawal on the funding path")
	assert.Zero(t, df.count(`SELECT count(*) FROM signing_decisions sd JOIN trade_intents ti ON ti.id = sd.intent_id WHERE ti.account_id = $1`, df.account))
	assert.Equal(t, walletUSD, df.chainBalance(df.wallet, df.usd), "nothing left the delegated wallet on chain")
	assert.Zero(t, df.chainBalance(dest.Address, df.usd))
	w, err := df.platform.GetWithdrawal(context.Background(), testDB, wid)
	require.NoError(t, err)
	var withdrawActorType, withdrawActorID string
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT actor_type, actor_id FROM economic_intents WHERE id = $1`, w.IntentID).
		Scan(&withdrawActorType, &withdrawActorID))
	assert.Equal(t, "SERVICE", withdrawActorType, "the WITHDRAW intent is the withdrawal service's, never the agent's")
	assert.NotEqual(t, df.agent.ID.String(), withdrawActorID)

	// The platform refuses the agent's principal at every step past
	// admission; the runtime has no way to call them in the first place.
	agentCtx := security.WithPrincipal(context.Background(), security.AgentPrincipal(df.agent.ID.String(), df.agent.AccountID))
	_, err = df.platform.ExecuteSwap(agentCtx, sid)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	_, err = df.platform.ExecuteWithdrawal(agentCtx, wid)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	assert.Zero(t, df.count(`SELECT count(*) FROM delegated_signing_decisions WHERE account_id = $1`, df.account))

	// The platform's executor carries the admitted swap to settlement.
	out, err := df.platform.ExecuteSwap(context.Background(), sid)
	require.NoError(t, err)
	assert.Equal(t, delegation.SwapSettled, out.Swap.Status, "failure %q", out.Swap.FailureCode)
	assert.Equal(t, 1, df.count(`SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1`, sid))
}
