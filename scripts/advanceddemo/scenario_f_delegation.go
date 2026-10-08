package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/prediction"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy/ir"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

func init() {
	register("F", "Delegated agent: an owner grants her agent capped authority; the agent's run requests a swap, the owner approves it, the platform's delegated signer signs the inspected plan and it settles on the devnet host; a swap over the cap and a mislabelled one are refused, and after revocation so is the next attempt", scenarioDelegation)
}

// scriptedEvaluator is the deterministic evaluator that stands in for the
// model in the agent runtime (agent.RunnerDeps.Evaluator, the runtime's own
// injection point; cmd/agent-worker wires one that refuses every evaluation
// until internal/strategy exposes its evaluator). It commits the forecast the
// strategy document states -- the constants of its COMMIT_PREDICTION action
// -- and then makes the delegated requests the scenario scripts for the run.
// It performs no I/O, signs nothing and holds nothing but its script.
type scriptedEvaluator struct {
	next []agent.Decision
	seen agent.EvalInput
}

func (e *scriptedEvaluator) Evaluate(_ context.Context, in agent.EvalInput) (agent.EvalOutput, error) {
	e.seen = in
	forecast, err := forecastOf(in.IR)
	if err != nil {
		return agent.EvalOutput{}, err
	}
	decisions := append([]agent.Decision{forecast}, e.next...)
	tools := make([]string, 0, len(in.Delegated.Tools))
	for _, t := range in.Delegated.Tools {
		tools = append(tools, string(t.Kind)+"@"+t.CapabilityID)
	}
	inputHash := sha256.Sum256([]byte(in.RunID.String() + "\x00" + in.Now.UTC().Format(time.RFC3339Nano) + "\x00" +
		strings.Join(tools, ",") + "\x00" + in.Delegated.Reason))
	trace, err := json.Marshal(decisions)
	if err != nil {
		return agent.EvalOutput{}, err
	}
	traceHash := sha256.Sum256(trace)
	return agent.EvalOutput{
		EvaluatorVersion: "advanced-demo-scripted-evaluator/v1", InputHash: inputHash[:], TraceHash: traceHash[:],
		Signals: json.RawMessage(`[]`), ConditionResults: json.RawMessage(`[]`),
		Rationale: json.RawMessage(`{"summary":"a deterministic evaluator stands in for the model: the document's forecast, then the scripted delegated request"}`),
		Decisions: decisions,
	}, nil
}

// forecastOf is the strategy document's COMMIT_PREDICTION action with its
// constant expressions evaluated.
func forecastOf(doc *ir.IR) (agent.Decision, error) {
	if doc == nil {
		return agent.Decision{}, errors.New("no strategy document")
	}
	instrumentsByRef := map[ir.Ref]string{}
	for _, d := range doc.Instruments {
		instrumentsByRef[d.Name] = d.InstrumentID
	}
	constant := func(e ir.Expr) (ir.Decimal, error) {
		if e.Const == nil {
			return ir.Decimal{}, errors.New("the stand-in evaluator reads constant forecasts only")
		}
		return *e.Const, nil
	}
	bps := func(e ir.Expr) (money.BPS, error) {
		d, err := constant(e)
		if err != nil {
			return 0, err
		}
		if d.Scale != 0 {
			return 0, errors.New("a basis-point forecast is a whole number")
		}
		v, err := strconv.ParseInt(d.Mantissa, 10, 64)
		return money.BPS(v), err
	}
	for _, a := range doc.Actions {
		if a.Kind != ir.ActionCommitPrediction || a.Prediction == nil {
			continue
		}
		spec := a.Prediction
		inst, err := instruments.ParseInstrumentID(instrumentsByRef[spec.Instrument])
		if err != nil {
			return agent.Decision{}, err
		}
		p := prediction.Prediction{InstrumentID: inst, Horizon: time.Duration(spec.HorizonMS) * time.Millisecond,
			Direction: prediction.Direction(spec.Direction)}
		if p.ProbabilityDirection, err = constant(spec.Probability); err != nil {
			return agent.Decision{}, err
		}
		if p.DownsideProbability, err = constant(spec.DownsideProbability); err != nil {
			return agent.Decision{}, err
		}
		if p.Confidence, err = constant(spec.Confidence); err != nil {
			return agent.Decision{}, err
		}
		if p.ExpectedReturnBPS, err = bps(spec.ExpectedReturnBPS); err != nil {
			return agent.Decision{}, err
		}
		if p.MaxDownsideBPS, err = bps(spec.MaxDownsideBPS); err != nil {
			return agent.Decision{}, err
		}
		return agent.Decision{ActionName: a.Name.String(), Kind: ir.ActionCommitPrediction, Prediction: &p}, nil
	}
	return agent.Decision{}, errors.New("the strategy document commits no prediction")
}

// runEvidence is what a run recorded on itself (agent_runs.evidence): the
// freshness check of each dependency it gathered, the delegated tools it was
// offered, and every delegated request it made.
type runEvidence struct {
	checks []agent.FreshnessCheck
	tools  agent.DelegatedToolsRecord
	calls  []agent.DelegatedRecord
}

func storedRunEvidence(ctx context.Context, w *sandboxworld.World, rt *sandboxworld.AgentRuntime, runID agent.RunID) (runEvidence, error) {
	run, err := rt.Store.GetRun(ctx, w.DB, runID)
	if err != nil {
		return runEvidence{}, err
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(run.Evidence, &elems); err != nil {
		return runEvidence{}, fmt.Errorf("decode the run's evidence: %w", err)
	}
	var out runEvidence
	for _, el := range elems {
		var k struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(el, &k); err != nil {
			return runEvidence{}, err
		}
		switch k.Kind {
		case agent.EvidenceKindDelegatedTools:
			err = json.Unmarshal(el, &out.tools)
		case agent.EvidenceKindDelegatedCall:
			var c agent.DelegatedRecord
			if err = json.Unmarshal(el, &c); err == nil {
				out.calls = append(out.calls, c)
			}
		default:
			var c agent.FreshnessCheck
			if err = json.Unmarshal(el, &c); err == nil {
				out.checks = append(out.checks, c)
			}
		}
		if err != nil {
			return runEvidence{}, err
		}
	}
	return out, nil
}

// rebalancerDocument is the agent's strategy: once a minute it commits a
// forecast on SBX-DTOK/SBX-USD and may propose a trade of at most 5.00. Its
// optional price dependency names a market-data tool no provider serves in
// this build, as in cmd/agent-worker, which wires no tool adapter.
func rebalancerDocument(inst instruments.InstrumentID, venue string) (*ir.IR, error) {
	policy := risk.DefaultAdvancedPolicy()
	policyHash, err := hex.DecodeString(policy.Hash())
	if err != nil {
		return nil, err
	}
	dec := func(s string) (*ir.Expr, error) {
		d, err := ir.ParseDecimalString(s)
		if err != nil {
			return nil, err
		}
		return &ir.Expr{Const: &d}, nil
	}
	exprs := map[string]*ir.Expr{}
	for k, s := range map[string]string{"up": "0.6000", "down": "0.3000", "confidence": "0.5500", "return": "100", "downside": "200"} {
		if exprs[k], err = dec(s); err != nil {
			return nil, err
		}
	}
	every := int64(60_000)
	two, five, ten := money.USDFromMinor(200), money.USDFromMinor(500), money.USDFromMinor(1_000)
	return &ir.IR{
		Instruments: []ir.InstrumentDecl{{Name: "dtok", InstrumentID: inst.String()}},
		Triggers:    []ir.Trigger{{Name: "tick", Kind: ir.TriggerOnInterval, EveryMS: &every}},
		Dependencies: []ir.Dependency{{Name: "mark", Kind: ir.DepPrice, ToolCode: "sandbox_mark", ToolVersion: 1, DependencyVersion: 1,
			Params: map[string]string{"instrument": "dtok"}, MaxAgeMS: 60_000, Required: false}},
		Actions: []ir.Action{
			{Name: "forecast", Kind: ir.ActionCommitPrediction, Prediction: &ir.PredictionSpec{
				Instrument: "dtok", HorizonMS: 3_600_000, Direction: ir.DirectionUp, Probability: *exprs["up"],
				ExpectedReturnBPS: *exprs["return"], DownsideProbability: *exprs["down"], MaxDownsideBPS: *exprs["downside"],
				Confidence: *exprs["confidence"],
			}},
			{Name: "rebalance", Kind: ir.ActionCreateTradeIntent, Intent: &ir.IntentSpec{
				Action: intent.ActionAcquireNotional, Instrument: "dtok", Sizing: ir.Sizing{Kind: ir.SizingFixedNotional, NotionalUSD: &two},
				Constraints: ir.IntentConstraints{MaxSlippageBPS: 100, MaxFeeBPS: 100, MaxPriceImpactBPS: 100, QuoteFreshnessMS: 60_000,
					AllowedVenues: []string{venue}},
				DeadlineMS: 60_000, Prediction: "forecast",
			}},
		},
		RiskPolicy:  ir.RiskPolicyRef{Version: policy.Version, Hash: policyHash},
		DataBudget:  ir.DataBudget{MaxToolCallsPerRun: 1, MaxToolCallsPerDay: 1_440},
		ModelBudget: ir.ModelBudget{},
		Envelope: ir.EnvelopeRequirements{MaxSingleTrade: five, MaxPosition: ten, MaxDailyLoss: five,
			Instruments: []string{inst.String()}, Venues: []string{venue}, MaxIntentsPerHour: 10, MaxRunsPerMinute: 1},
		Effects: []ir.Effect{ir.EffectReadMarketData, ir.EffectCommitPrediction, ir.EffectCreateTradeIntent},
		Lineage: ir.Lineage{Source: ir.SourceTypeScriptSDK},
	}, nil
}

func scenarioDelegation(ctx context.Context, w *sandboxworld.World, rec *recorder) error {
	dg, err := w.OpenDelegation(ctx)
	if err != nil {
		return err
	}
	venue := dg.Venue
	rec.step("the platform's side of delegated authority is wired as cmd/api wires it: the delegated wallet signer and the venue co-signers derive from the world's sandbox chain seed, and the startup sandbox venue is registered at its co-signer's address on a house book of its own",
		"venue", venue.ID.String(), "venue_name", venue.Name, "venue_address", venue.Address.String(),
		"house_book", venue.HouseAccountID.String(), "spread_bps", fmt.Sprint(int64(venue.SpreadBPS)))
	house, err := w.Registry.HouseAccount(ctx, w.DB, venue.HouseAccountID)
	if err != nil {
		return err
	}
	var treasuryBook bool
	var backedVenues int
	if err := w.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM treasury_books WHERE house_account_id = $1),
		(SELECT count(*) FROM delegation_venues WHERE house_account_id = $1)`, venue.HouseAccountID).Scan(&treasuryBook, &backedVenues); err != nil {
		return err
	}
	var coSigner devnet.PublicKey
	for _, cs := range dg.CoSigners {
		if cs.Name == venue.Name {
			coSigner = cs.Address
		}
	}
	rec.check("the venue settles at its co-signer's address and is backed by a house book of its own: a LIQUIDITY_PROVISION book, no treasury book, backing no other venue",
		!coSigner.IsZero() && venue.Address == coSigner && house.Strategy == econ.HouseLiquidityProvision && house.Status == econ.HouseActive &&
			!treasuryBook && backedVenues == 1,
		"address is the co-signer's %t; house book %s %s, a treasury book %t, backs %d venue", venue.Address == coSigner,
		house.Strategy, house.Status, treasuryBook, backedVenues)

	dusd, err := w.SandboxToken(ctx, "SBX-DUSD", "Sandbox delegated dollar (simulated)", 6, assets.RiskSettlement, true)
	if err != nil {
		return err
	}
	dtok, err := w.SandboxToken(ctx, "SBX-DTOK", "Sandbox delegated token (simulated)", 9, assets.RiskMajor, false)
	if err != nil {
		return err
	}
	showDUSD := func(q money.Quantity) string { return q.ToDecimalString(dusd.Decimals) + " SBX-DUSD" }
	showDTOK := func(q money.Quantity) string { return q.ToDecimalString(dtok.Decimals) + " SBX-DTOK" }
	mark := func() error {
		_, err := w.Mark(ctx, dtok.ID, "100.00", "demo-reference-feed")
		return err
	}
	if err := mark(); err != nil {
		return err
	}
	if err := w.FundVenue(ctx, venue, dtok, 1_000_000_000_000, "demo-f-venue-dtok"); err != nil {
		return err
	}
	if err := w.FundVenue(ctx, venue, dusd, 1_000_000_000, "demo-f-venue-dusd"); err != nil {
		return err
	}
	rec.step("two SIMULATED devnet tokens are listed on sbx-solana -- SBX-DUSD, a USD-pegged unit, and SBX-DTOK, marked at 100.00 SBX-USD -- and the venue holds 1,000 of each, in its house book and at its address on chain",
		"sbx_dusd", dusd.ID.String(), "sbx_dusd_mint", dusd.MintAddress, "sbx_dtok", dtok.ID.String(), "sbx_dtok_mint", dtok.MintAddress)

	cora, err := w.Customer(ctx, "cora-delegator")
	if err != nil {
		return err
	}
	wallet, err := w.FundDelegatedWallet(ctx, dg, cora, dusd, 100_000_000, "demo-f-fund-cora")
	if err != nil {
		return err
	}
	owner := func() context.Context {
		p := cora.P
		p.AuthTime = w.Clock.Now() // she signs in again with MFA: every owner decision needs a fresh strong authentication
		return security.WithPrincipal(ctx, p)
	}
	rec.step("Cora holds 100 SBX-DUSD in her delegated wallet: the faucet grants them in the books and the same units are minted to the wallet's address on chain",
		"account", cora.Account.String(), "delegated_wallet", wallet.String())
	houseDTOK, err := w.Balance(ctx, house.Account(ledger.CodeHouseInventory, dtok.ID))
	if err != nil {
		return err
	}
	houseDUSD, err := w.Balance(ctx, house.Account(ledger.CodeHouseInventory, dusd.ID))
	if err != nil {
		return err
	}
	venueChainDTOK, err := tokenBalance(ctx, w, venue.Address, dtok)
	if err != nil {
		return err
	}
	venueChainDUSD, err := tokenBalance(ctx, w, venue.Address, dusd)
	if err != nil {
		return err
	}
	coraBooks, err := w.Wallet(ctx, cora, dusd.ID)
	if err != nil {
		return err
	}
	coraChain, err := tokenBalance(ctx, w, wallet, dusd)
	if err != nil {
		return err
	}
	rec.check("books and chain agree on where everyone starts: the venue holds 1,000 SBX-DTOK and 1,000 SBX-DUSD, Cora's delegated wallet 100 SBX-DUSD",
		houseDTOK.Equal(money.QuantityFromInt64(1_000_000_000_000)) && venueChainDTOK.Equal(houseDTOK) &&
			houseDUSD.Equal(money.QuantityFromInt64(1_000_000_000)) && venueChainDUSD.Equal(houseDUSD) &&
			coraBooks.Equal(money.QuantityFromInt64(100_000_000)) && coraChain.Equal(coraBooks),
		"venue: house book %s and %s, chain %s and %s; Cora: books %s, chain %s", showDTOK(houseDTOK), showDUSD(houseDUSD),
		showDTOK(venueChainDTOK), showDUSD(venueChainDUSD), showDUSD(coraBooks), showDUSD(coraChain))

	// --- her agent: the strategy it runs, created and promoted to PAPER -------
	var inst instruments.Instrument
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inst, err = w.Instruments.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: dtok.ID, Quote: w.USD.ID, Settlement: w.USD.ID, CanonicalName: "SBX-DTOK/SBX-USD",
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: w.Clock.Now().Add(-time.Hour),
		})
		return err
	}); err != nil {
		return fmt.Errorf("open the SBX-DTOK pair: %w", err)
	}
	doc, err := rebalancerDocument(inst.ID, venue.Name)
	if err != nil {
		return err
	}
	sv, err := w.RecordStrategyVersion(ctx, cora, "Rebalancer", doc)
	if err != nil {
		return err
	}
	evaluator := &scriptedEvaluator{}
	rt, err := w.OpenAgentRuntime(dg.Admission, evaluator)
	if err != nil {
		return err
	}
	operatorActor, err := w.OperatorActor(ctx, "pia-agent-operator")
	if err != nil {
		return err
	}
	operator := security.WithPrincipal(ctx, security.Principal{SubjectID: operatorActor.ID, ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleOperations}, AuthTime: w.Clock.Now(), AMR: []string{"pwd", "mfa"}})
	policy := risk.DefaultAdvancedPolicy()
	policyHash, err := hex.DecodeString(policy.Hash())
	if err != nil {
		return err
	}
	var ag agent.Agent
	if err := w.InTx(ctx, func(_ context.Context, tx pgx.Tx) error {
		var err error
		ag, err = rt.Lifecycle.Create(owner(), tx, agent.CreateAgent{AccountID: cora.Account.String(), StrategyID: sv.StrategyID,
			StrategyVersionID: sv.VersionID, Name: "Cora's rebalancer", RiskPolicyVersion: policy.Version})
		return err
	}); err != nil {
		return fmt.Errorf("create the agent: %w", err)
	}
	for _, stage := range []agent.Stage{agent.StageCompiled, agent.StageValidated, agent.StageBacktestEligible} {
		if err := w.InTx(ctx, func(_ context.Context, tx pgx.Tx) error {
			var err error
			ag, err = rt.Lifecycle.Promote(operator, tx, ag.ID, stage.State(), agent.PromotionEvidence{
				StrategyVersionID: sv.VersionID, IRHash: sv.IRHash, RiskPolicyVersion: policy.Version, RiskPolicyHash: policyHash,
				Reason: "sandbox demonstration: promoted to " + stage.String(),
			})
			return err
		}); err != nil {
			return fmt.Errorf("promote the agent to %s: %w", stage, err)
		}
	}
	rec.step("Cora's agent runs a strategy that commits a forecast on SBX-DTOK/SBX-USD once a minute and may propose trades. No service of this build persists a compiled strategy, so its version is recorded after the document stages the agent worker applies (ir.ParseIR), marked COMPILED by this recorder, never by the compiler; the agent is created by Cora and promoted by an operator to BACKTEST_ELIGIBLE, whose runs are PAPER",
		"agent", ag.ID.String(), "strategy_version", sv.VersionID, "ir_sha256", hex.EncodeToString(sv.IRHash),
		"stage", string(ag.Stage), "mode", string(ag.Mode))
	rec.check("the agent is BACKTEST_ELIGIBLE in PAPER, the only mode delegated authority is offered to",
		ag.Stage == agent.StageBacktestEligible && ag.State == agent.StateBacktestEligible && ag.Mode == agent.ModePaper && ag.Runnable(),
		"stage %s, state %s, mode %s, runnable %t", ag.Stage, ag.State, ag.Mode, ag.Runnable())

	// --- the owner grants a capped capability ----------------------------------
	var capability delegation.Capability
	if err := w.InTx(ctx, func(_ context.Context, tx pgx.Tx) error {
		var err error
		capability, _, err = dg.Service.GrantCapability(owner(), tx, delegation.GrantRequest{
			AccountID: cora.Account, AgentID: ag.ID.String(), Mode: gates.ModeSandbox,
			InstrumentClasses: []delegation.InstrumentClass{delegation.ClassStableValue, delegation.ClassSpotToken},
			ActionClasses:     []delegation.ActionClass{delegation.ActionAcquireExposure},
			MaxNotional:       money.USDFromMinor(1_000), MaxDailyNotional: money.USDFromMinor(800), MaxSingleTrade: money.USDFromMinor(500),
			DestinationAllowlist: []devnet.PublicKey{venue.Address}, ChainAllowlist: []devnet.ChainID{devnethost.ChainSolana},
			ExpiresAt: w.Clock.Now().Add(24 * time.Hour), RequiresHumanApproval: true, IdempotencyKey: "demo-f-grant",
		})
		return err
	}); err != nil {
		return fmt.Errorf("grant: %w", err)
	}
	contentHash, hashErr := capability.ComputeContentHash()
	terms := capability.Terms
	rec.step("Cora grants her agent a capability: buy (ACQUIRE_EXPOSURE) stable value and spot tokens on sbx-solana, only at the sandbox venue, at most 5.00 a trade, 8.00 a day and 10.00 in all, for 24 hours, every swap needing her approval",
		"capability", capability.ID.String(), "content_hash", capability.HashHex(), "expires_at", terms.ExpiresAt.Format(time.RFC3339))
	rec.check("the capability is ACTIVE with exactly those caps, and its content hash verifies",
		capability.Status == delegation.CapabilityActive && hashErr == nil && contentHash == capability.ContentHash &&
			terms.MaxSingleTrade.Minor() == 500 && terms.MaxDailyNotional.Minor() == 800 && terms.MaxNotional.Minor() == 1_000 &&
			terms.ExpiresAt.Equal(capability.GrantedAt.Add(24*time.Hour)) && terms.RequiresHumanApproval &&
			len(terms.DestinationAllowlist) == 1 && terms.DestinationAllowlist[0] == venue.Address &&
			strings.EqualFold(terms.StrategyID, sv.StrategyID),
		"%s; single %s, daily %s, total %s; expires 24h after the grant %t; approval required %t; allowed venue %s only %t",
		capability.Status, terms.MaxSingleTrade, terms.MaxDailyNotional, terms.MaxNotional,
		terms.ExpiresAt.Equal(capability.GrantedAt.Add(24*time.Hour)), terms.RequiresHumanApproval, venue.Name,
		len(terms.DestinationAllowlist) == 1 && terms.DestinationAllowlist[0] == venue.Address)

	swapDecision := func(action delegation.ActionClass, amountIn int64) agent.Decision {
		return agent.Decision{ActionName: "rebalance", Kind: agent.ActionRequestDelegatedSwap, DelegatedSwap: &agent.DelegatedSwapRequest{
			CapabilityID: capability.ID.String(), Action: string(action), VenueID: venue.ID.String(), PayAssetID: dusd.ID.String(),
			ReceiveAssetID: dtok.ID.String(), AmountIn: money.QuantityFromInt64(amountIn), MaxSlippageBPS: 100, Jurisdiction: "US-CA",
		}}
	}
	// runAgent lets a minute pass, has the reference feed mark SBX-DTOK, and
	// has the dispatcher open the agent's run on its interval trigger, as the
	// agent worker's tick does; the runner then executes it.
	runAgent := func(first bool, decisions ...agent.Decision) (agent.Run, agent.RunResult, error) {
		if !first {
			w.Clock.Advance(time.Minute)
			if err := mark(); err != nil {
				return agent.Run{}, agent.RunResult{}, err
			}
		}
		opened, err := rt.Dispatcher.OnTick(ctx, w.Clock.Now())
		if err != nil {
			return agent.Run{}, agent.RunResult{}, err
		}
		if len(opened) != 1 || opened[0].AgentID != ag.ID {
			return agent.Run{}, agent.RunResult{}, fmt.Errorf("the dispatcher opened %d runs, want the agent's one", len(opened))
		}
		evaluator.next = decisions
		res, runErr := rt.Runner.Run(ctx, opened[0].ID)
		return opened[0], res, runErr
	}
	intentsOf := func(status, code string) (int, error) {
		var n int
		err := w.DB.QueryRow(ctx, `SELECT count(*) FROM economic_intents WHERE delegation_id = $1 AND status = $2
			AND coalesce(rejection_code, '') = $3`, capability.ID, status, code).Scan(&n)
		return n, err
	}

	// --- the agent's run requests a swap; nothing is signed ---------------------
	run1, res1, err := runAgent(true, swapDecision(delegation.ActionAcquireExposure, 2_000_000))
	if err != nil {
		return fmt.Errorf("the agent's first run: %w", err)
	}
	offered := evaluator.seen.Delegated
	swapTool, held := offered.Lookup(agent.ActionRequestDelegatedSwap, capability.ID.String())
	rec.step("the dispatcher opens the agent's run on its one-minute trigger and the runner executes it under the agent's own principal; a deterministic evaluator stands in for the model, and it is offered the delegated swap tool under Cora's capability",
		"run", run1.ID.String(), "correlation", run1.CorrelationID, "offered_tools", fmt.Sprint(len(offered.Tools)))
	rec.check("the run holds exactly the swap tool of the capability, sized by its caps",
		len(offered.Tools) == 1 && held && swapTool.MaxSingleTradeUSD.Minor() == 500 && swapTool.RequiresHumanApproval &&
			strings.Join(swapTool.Actions, ",") == string(delegation.ActionAcquireExposure),
		"%d tool(s); REQUEST_DELEGATED_SWAP held %t, single-trade cap %s, approval required %t, actions [%s]", len(offered.Tools),
		held, swapTool.MaxSingleTradeUSD, swapTool.RequiresHumanApproval, strings.Join(swapTool.Actions, ","))
	if len(res1.Delegated) != 1 {
		return fmt.Errorf("the first run made %d delegated requests, want 1", len(res1.Delegated))
	}
	call1 := res1.Delegated[0]
	sid, err := delegation.ParseSwapID(call1.SubjectID)
	if err != nil {
		return fmt.Errorf("the first run's swap: %+v: %w", call1, err)
	}
	sw, err := dg.Service.GetSwap(ctx, w.DB, sid)
	if err != nil {
		return err
	}
	var decisionsBefore int
	if err := w.DB.QueryRow(ctx, `SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1`, sid).Scan(&decisionsBefore); err != nil {
		return err
	}
	rec.step("the agent commits its forecast and asks to pay 2.000000 SBX-DUSD for SBX-DTOK at the venue (ACQUIRE_EXPOSURE); the runtime's delegated emitter hands it to delegation's admission facade, which records the intent, routes it, prices it at the venue, measures the capability, runs Risk V2, reserves the pay asset and records the plan",
		"prediction", res1.PredictionID, "swap", sw.ID.String(), "intent", sw.IntentID.String(), "plan_hash", hex.EncodeToString(sw.PlanHash[:]),
		"status", string(sw.Status), "nonce", strconv.FormatUint(sw.RequestNonce, 10))
	rec.check("the request is admitted under the capability and waits for the owner, at the venue's quote: 2.000000 SBX-DUSD for 0.019900000 SBX-DTOK (at least 0.019800000)",
		res1.Run.Status == agent.RunPredicted && res1.PredictionID != "" && !call1.Refused() && call1.Status == string(delegation.SwapAwaitingApproval) &&
			sw.Status == delegation.SwapAwaitingApproval && sw.Action == delegation.ActionAcquireExposure && sw.Wallet == wallet &&
			sw.AmountIn.Equal(money.QuantityFromInt64(2_000_000)) && sw.AmountOut.Equal(money.QuantityFromInt64(19_900_000)) &&
			sw.MinAmountOut.Equal(money.QuantityFromInt64(19_800_000)) && sw.NotionalUSD.Minor() == 200 &&
			sw.RequestNonce == agent.DelegatedNonce(run1.ID, "rebalance"),
		"run %s; swap %s %s: %s for %s (min %s), notional %s, from Cora's delegated wallet %t, nonce derived from the run %t",
		res1.Run.Status, sw.Status, sw.Action, showDUSD(sw.AmountIn), showDTOK(sw.AmountOut), showDTOK(sw.MinAmountOut), sw.NotionalUSD,
		sw.Wallet == wallet, sw.RequestNonce == agent.DelegatedNonce(run1.ID, "rebalance"))
	ev1, err := storedRunEvidence(ctx, w, rt, run1.ID)
	if err != nil {
		return err
	}
	priceCheck := agent.FreshnessCheck{}
	if len(ev1.checks) == 1 {
		priceCheck = ev1.checks[0]
	}
	rec.check("the run recorded on itself what it was offered and what it asked, and its optional price dependency, which no provider serves in this build, refused by the broker",
		len(ev1.tools.Tools) == 1 && ev1.tools.Tools[0].Tool == string(agent.ActionRequestDelegatedSwap) &&
			ev1.tools.Tools[0].CapabilityID == capability.ID.String() && len(ev1.calls) == 1 && ev1.calls[0].SubjectID == sw.ID.String() &&
			ev1.calls[0].Status == string(delegation.SwapAwaitingApproval) && len(ev1.checks) == 1 && priceCheck.Dependency == "mark" &&
			!priceCheck.Required && !priceCheck.Present && priceCheck.Refusal == string(errs.CodeNotFound),
		"tools [%s]; %d request recorded with the swap's id and status %s; dependency %s@%d required %t, present %t, refused %s",
		strings.Join(toolNames(ev1.tools), ","), len(ev1.calls), firstStatus(ev1.calls), priceCheck.ToolCode, priceCheck.ToolVersion,
		priceCheck.Required, priceCheck.Present, priceCheck.Refusal)
	var actorType, agentID string
	var allowed int
	if err := w.DB.QueryRow(ctx, `SELECT i.actor_type, coalesce(i.agent_id::text, ''),
		(SELECT count(*) FROM advanced_risk_decisions d WHERE d.intent_id = i.id AND d.verdict = 'ALLOW')
		FROM economic_intents i WHERE i.id = $1`, sw.IntentID).Scan(&actorType, &agentID, &allowed); err != nil {
		return err
	}
	reservation, err := w.Capital.Get(ctx, w.DB, sw.ReservationID)
	if err != nil {
		return err
	}
	rec.check("nothing is signed by the agent's request: no transaction, no signing decision; its intent is the agent's, Risk V2 allowed it, and the pay asset is reserved",
		len(sw.SignedTx) == 0 && sw.TxID == "" && decisionsBefore == 0 && actorType == string(security.ActorAgent) && agentID == ag.ID.String() &&
			allowed == 1 && reservation.Status == capital.ReservationActive && reservation.Quantity.Equal(sw.AmountIn),
		"signed transaction %d bytes, %d signing decisions; intent by %s (this agent %t), %d ALLOW decision; reservation %s of %s",
		len(sw.SignedTx), decisionsBefore, actorType, agentID == ag.ID.String(), allowed, reservation.Status, showDUSD(reservation.Quantity))

	// An agent principal can neither decide its own swap nor have it signed.
	asAgent := security.WithPrincipal(ctx, security.AgentPrincipal(ag.ID.String(), cora.Account.String()))
	agentDecideErr := w.InTx(ctx, func(_ context.Context, tx pgx.Tx) error {
		_, err := dg.Service.DecideSwap(asAgent, tx, delegation.SwapDecision{SwapID: sid, PlanHash: sw.PlanHash, Approve: true,
			Reason: "the agent approving its own swap"})
		return err
	})
	_, agentSignErr := dg.Service.ExecuteSwap(asAgent, sid)
	rec.check("under the agent's principal, approving the swap and executing (signing) it are both refused",
		errs.CodeOf(agentDecideErr) == errs.CodeForbidden && errs.CodeOf(agentSignErr) == errs.CodeForbidden,
		"decide: %s, execute: %s", errs.CodeOf(agentDecideErr), errs.CodeOf(agentSignErr))

	// --- the owner approves the exact plan; the platform signs and settles -------
	if err := w.InTx(ctx, func(_ context.Context, tx pgx.Tx) error {
		_, err := dg.Service.DecideSwap(owner(), tx, delegation.SwapDecision{SwapID: sid, PlanHash: sw.PlanHash, Approve: true,
			Reason: "I approve this exact plan"})
		return err
	}); err != nil {
		return fmt.Errorf("the owner's decision: %w", err)
	}
	settled, err := dg.Service.ExecuteSwap(ctx, sid)
	if err != nil {
		return fmt.Errorf("execute the approved swap: %w", err)
	}
	sw = settled.Swap
	rec.step("Cora approves the exact plan (bound to its hash); the platform's executor compiles the transaction from the persisted plan, the delegated signer re-reads the capability, the approval and the message, inspects it field by field, records its decision and signs; the venue co-signs its leg; it is submitted once and settles after finality",
		"tx", string(sw.TxID), "journal", ptrString(sw.JournalTransactionID), "effect", ptrString(sw.EffectID), "status", string(sw.Status))
	var verdict, signerKey, inspector, signingPolicy string
	var reasons int
	var samePlan, sameMessage, decidedBeforeSigning bool
	if err := w.DB.QueryRow(ctx, `SELECT d.verdict, cardinality(d.reason_codes), d.signer_public_key, d.plan_hash = s.plan_hash,
		d.message_sha256 = sha256(s.message), d.inspector_version, d.policy_version, d.decided_at <= s.signed_at
		FROM delegated_signing_decisions d JOIN delegated_swaps s ON s.id = d.swap_id WHERE d.swap_id = $1`, sid).Scan(
		&verdict, &reasons, &signerKey, &samePlan, &sameMessage, &inspector, &signingPolicy, &decidedBeforeSigning); err != nil {
		return fmt.Errorf("read the signing decision: %w", err)
	}
	rec.check("the swap SETTLED, signed once by the platform's delegated signer after its inspection approved the persisted plan and message",
		settled.Settled && sw.Status == delegation.SwapSettled && verdict == string(delegation.VerdictApproved) && reasons == 0 &&
			signerKey == wallet.String() && samePlan && sameMessage && decidedBeforeSigning && inspector == delegation.InspectorVersion &&
			signingPolicy == delegation.DefaultSigningPolicy().Version,
		"%s; decision %s with %d reasons by the wallet's registered key %t, over the plan %t and the exact message %t, before signing %t (%s, %s)",
		sw.Status, verdict, reasons, signerKey == wallet.String(), samePlan, sameMessage, decidedBeforeSigning, inspector, signingPolicy)

	fee, _ := delegation.HostFees(w.Host)(sw.Chain)
	txID, inspectErr := delegation.InspectSwapTransaction(delegation.SwapExpectation{
		Plan: sw.Plan, PlanHash: sw.PlanHash, Blockhash: sw.RecentBlockhash, Wallet: sw.Wallet,
		AllowedChains: terms.ChainAllowlist, AllowedDestinations: terms.DestinationAllowlist, ChainFee: fee,
		MaxFee: delegation.DefaultSigningPolicy().MaxFeePerTx,
	}, sw.SignedTx)
	signed, decodeErr := devnet.Decode(sw.SignedTx)
	signers := []string{}
	if decodeErr == nil {
		for _, k := range signed.Message.RequiredSigners() {
			switch k {
			case wallet:
				signers = append(signers, "delegated wallet")
			case venue.Address:
				signers = append(signers, "venue co-signer")
			default:
				signers = append(signers, "other "+k.String())
			}
		}
	}
	rec.check("inspected again here, the signed transaction is exactly the plan: wallet pays the venue, the venue pays the wallet, the plan hash in the memo; every signature verifies",
		inspectErr == nil && txID == sw.TxID && decodeErr == nil && signed.VerifySignatures() == nil &&
			strings.Join(signers, ",") == "delegated wallet,venue co-signer",
		"inspection %v, transaction id matches %t, signers [%s]", inspectErr, txID == sw.TxID, strings.Join(signers, ","))

	var path []string
	rows, err := w.DB.Query(ctx, `SELECT to_status, actor_type FROM delegated_swap_transitions WHERE swap_id = $1 ORDER BY occurred_at, id`, sid)
	if err != nil {
		return err
	}
	for rows.Next() {
		var to, by string
		if err := rows.Scan(&to, &by); err != nil {
			rows.Close()
			return err
		}
		path = append(path, to+" by "+by)
	}
	rows.Close()
	rec.check("every state change of the swap is bound: the agent only asked; the owner decided; the service signed, submitted and settled",
		strings.Join(path, " > ") == "ADMITTED by AGENT > AWAITING_APPROVAL by SERVICE > APPROVED by USER > SIGNED by SERVICE > SUBMITTED by SERVICE > SETTLED by SERVICE",
		"%s", strings.Join(path, " > "))

	st, err := w.Host.Status(ctx, sw.Chain, sw.TxID)
	if err != nil {
		return err
	}
	var submissions int
	if err := w.DB.QueryRow(ctx, `SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`,
		dg.Service.Network(), string(sw.TxID)).Scan(&submissions); err != nil {
		return err
	}
	chainDUSD, err := tokenBalance(ctx, w, wallet, dusd)
	if err != nil {
		return err
	}
	chainDTOK, err := tokenBalance(ctx, w, wallet, dtok)
	if err != nil {
		return err
	}
	venueDUSD, err := tokenBalance(ctx, w, venue.Address, dusd)
	if err != nil {
		return err
	}
	rec.check("on the devnet host the transaction was submitted once and is final, and the wallet paid exactly 2.000000 SBX-DUSD for exactly the quoted 0.019900000 SBX-DTOK",
		submissions == 1 && st.State == devnet.TxFinalized && chainDUSD.Equal(money.QuantityFromInt64(98_000_000)) &&
			chainDTOK.Equal(sw.AmountOut) && venueDUSD.Equal(money.QuantityFromInt64(1_002_000_000)),
		"%d submission, %s; wallet %s and %s, venue %s", submissions, st.State, showDUSD(chainDUSD), showDTOK(chainDTOK), showDUSD(venueDUSD))

	walletDUSD, err := w.Wallet(ctx, cora, dusd.ID)
	if err != nil {
		return err
	}
	walletDTOK, err := w.Wallet(ctx, cora, dtok.ID)
	if err != nil {
		return err
	}
	var journalKind string
	if sw.JournalTransactionID != nil {
		j, err := w.Ledger.Transaction(ctx, w.DB, *sw.JournalTransactionID)
		if err != nil {
			return err
		}
		journalKind = string(j.Kind)
	}
	var effect econ.Effect
	if sw.EffectID != nil {
		if effect, err = w.Effects.Get(ctx, w.DB, *sw.EffectID); err != nil {
			return err
		}
	}
	consumed, err := w.Capital.Get(ctx, w.DB, sw.ReservationID)
	if err != nil {
		return err
	}
	in, err := w.Intents.Get(ctx, w.DB, sw.IntentID)
	if err != nil {
		return err
	}
	rec.check("the ledger agrees with the chain: one SIM_TRADE posted after finality, the reservation consumed, the effect applied with its evidence, the intent completed",
		journalKind == string(ledger.KindSimTrade) && walletDUSD.Equal(money.QuantityFromInt64(98_000_000)) && walletDTOK.Equal(chainDTOK) &&
			consumed.Status == capital.ReservationConsumed && effect.Status == econ.EffectApplied && effect.EvidenceRef != "" &&
			in.Status == econ.StatusCompleted,
		"%s; wallet %s and %s; reservation %s; effect %s; intent %s", journalKind, showDUSD(walletDUSD), showDTOK(walletDTOK),
		consumed.Status, effect.Status, in.Status)

	// --- a swap above the single-trade limit is refused --------------------------
	_, res2, err2 := runAgent(false, swapDecision(delegation.ActionAcquireExposure, 6_000_000))
	if len(res2.Delegated) != 1 {
		return fmt.Errorf("the second run made %d delegated requests (%v), want 1", len(res2.Delegated), err2)
	}
	call2 := res2.Delegated[0]
	overCap, err := intentsOf(string(econ.StatusRejected), delegation.RefusalRiskRejected)
	if err != nil {
		return err
	}
	rec.step("a minute later the agent's next run asks to pay 6.000000 SBX-DUSD, above the 5.00 single-trade limit",
		"refusal", call2.Refusal, "reason", call2.Reason)
	rec.check("the swap above the single-trade limit is refused by Risk V2's authority section and the refusal recorded on the run and on its intent; the run ends FAILED",
		err2 != nil && res2.Run.Status == agent.RunFailed && call2.Refused() && call2.Refusal == string(errs.CodeRiskRejected) &&
			strings.Contains(call2.Detail, delegation.ReasonSingleLimitExceeded) && overCap == 1,
		"run %s; refusal %s naming %s %t; %d intent REJECTED with %s", res2.Run.Status, call2.Refusal,
		delegation.ReasonSingleLimitExceeded, strings.Contains(call2.Detail, delegation.ReasonSingleLimitExceeded), overCap,
		delegation.RefusalRiskRejected)

	// --- a purchase labelled as a reduction is refused ---------------------------
	_, res3, err3 := runAgent(false, swapDecision(delegation.ActionReduceExposure, 1_000_000))
	if len(res3.Delegated) != 1 {
		return fmt.Errorf("the third run made %d delegated requests (%v), want 1", len(res3.Delegated), err3)
	}
	call3 := res3.Delegated[0]
	mislabelled, err := intentsOf(string(econ.StatusRejected), delegation.ReasonActionMismatch)
	if err != nil {
		return err
	}
	rec.step("the next run asks to pay 1.000000 SBX-DUSD for SBX-DTOK -- a purchase -- labelled REDUCE_EXPOSURE",
		"refusal", call3.Refusal, "reason", call3.Reason)
	rec.check("a purchase labelled as a reduction is refused: what a swap does is decided by its direction of value, never its label",
		err3 != nil && res3.Run.Status == agent.RunFailed && call3.Refusal == string(errs.CodeForbidden) &&
			call3.Reason == delegation.ReasonActionMismatch && mislabelled == 1,
		"run %s; refusal %s, reason %s; %d intent REJECTED with %s", res3.Run.Status, call3.Refusal, call3.Reason, mislabelled,
		delegation.ReasonActionMismatch)

	var swaps, uses int
	if err := w.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM delegated_swaps WHERE capability_id = $1),
		(SELECT count(*) FROM delegated_capability_usage WHERE capability_id = $1)`, capability.ID).Scan(&swaps, &uses); err != nil {
		return err
	}
	rec.check("neither refusal became a swap or a use of the capability", swaps == 1 && uses == 1, "%d swap, %d use", swaps, uses)

	// --- the owner revokes; the agent's next attempt is refused -------------------
	var revoked delegation.Capability
	if err := w.InTx(ctx, func(_ context.Context, tx pgx.Tx) error {
		var err error
		revoked, err = dg.Service.RevokeCapability(owner(), tx, delegation.RevokeRequest{CapabilityID: capability.ID,
			Reason: "Cora withdraws her agent's authority"})
		return err
	}); err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	intentsBefore, err := intentsOf(string(econ.StatusRejected), delegation.RefusalRiskRejected)
	if err != nil {
		return err
	}
	run4, res4, err4 := runAgent(false, swapDecision(delegation.ActionAcquireExposure, 1_000_000))
	if err4 != nil {
		return fmt.Errorf("the run after revocation: %w", err4)
	}
	if len(res4.Delegated) != 1 {
		return fmt.Errorf("the run after revocation made %d delegated requests, want 1", len(res4.Delegated))
	}
	call4 := res4.Delegated[0]
	ev4, err := storedRunEvidence(ctx, w, rt, run4.ID)
	if err != nil {
		return err
	}
	toolsRecord, recordedCalls := ev4.tools, len(ev4.calls)
	intentsAfter, err := intentsOf(string(econ.StatusRejected), delegation.RefusalRiskRejected)
	if err != nil {
		return err
	}
	rec.step("Cora revokes the capability; a minute later the agent's next run is offered no delegated tool, and when its evaluator asks for a swap anyway the runtime refuses it before delegation sees it",
		"run", run4.ID.String(), "toolset_reason", toolsRecord.Reason, "refusal", call4.Refusal, "reason", call4.Reason)
	rec.check("after revocation the next attempt is refused and the reason recorded on the run: no usable capability, no tool, nothing reached delegation",
		revoked.Status == delegation.CapabilityRevoked && res4.Run.Status == agent.RunSkipped && res4.SkipReason == agent.SkipEffectMismatch &&
			len(evaluator.seen.Delegated.Tools) == 0 && evaluator.seen.Delegated.Reason == agent.DelegatedReasonNoCapability &&
			toolsRecord.Reason == agent.DelegatedReasonNoCapability && recordedCalls == 1 &&
			call4.Refusal == string(errs.CodeEffectForbidden) && call4.Reason == agent.DelegatedReasonToolUnavailable &&
			intentsAfter == intentsBefore,
		"capability %s; run %s (%s); offered %d tools (%s), recorded %t; refusal %s, reason %s; delegation intents unchanged %t",
		revoked.Status, res4.Run.Status, res4.SkipReason, len(evaluator.seen.Delegated.Tools), evaluator.seen.Delegated.Reason,
		toolsRecord.Reason == agent.DelegatedReasonNoCapability && recordedCalls == 1, call4.Refusal, call4.Reason,
		intentsAfter == intentsBefore)

	// The same request, straight to the admission facade under the agent's
	// principal -- past the runtime's own check -- is refused by delegation.
	var direct delegation.SwapTicket
	if err := w.InTx(ctx, func(_ context.Context, tx pgx.Tx) error {
		var err error
		direct, err = dg.Admission.RequestSwap(asAgent, tx, delegation.SwapRequest{
			CapabilityID: capability.ID, AccountID: cora.Account, Nonce: agent.DelegatedNonce(run4.ID, "rebalance-direct"),
			Action: delegation.ActionAcquireExposure, VenueID: venue.ID, PayAssetID: dusd.ID, ReceiveAssetID: dtok.ID,
			AmountIn: money.QuantityFromInt64(1_000_000), MaxSlippageBPS: 100, Jurisdiction: "US-CA",
		})
		return err
	}); err != nil {
		return fmt.Errorf("the direct request after revocation: %w", err)
	}
	afterDirect, err := intentsOf(string(econ.StatusRejected), delegation.RefusalRiskRejected)
	if err != nil {
		return err
	}
	directDetail := ""
	if direct.Refusal != nil {
		directDetail = direct.Refusal.Error()
	}
	rec.check("made straight to the admission facade under the agent's principal, the request is refused by delegation itself, its authority section revoked, and recorded on a REJECTED intent",
		errs.CodeOf(direct.Refusal) == errs.CodeRiskRejected && strings.Contains(directDetail, delegation.ReasonCapabilityRevoked) &&
			strings.Contains(directDetail, risk.ReasonAuthorityRevoked) && afterDirect == intentsAfter+1,
		"refusal %s naming %s %t and %s %t; REJECTED intents %d -> %d", errs.CodeOf(direct.Refusal), delegation.ReasonCapabilityRevoked,
		strings.Contains(directDetail, delegation.ReasonCapabilityRevoked), risk.ReasonAuthorityRevoked,
		strings.Contains(directDetail, risk.ReasonAuthorityRevoked), intentsAfter, afterDirect)

	finalDUSD, err := w.Wallet(ctx, cora, dusd.ID)
	if err != nil {
		return err
	}
	finalChain, err := tokenBalance(ctx, w, wallet, dusd)
	if err != nil {
		return err
	}
	rec.check("only the one approved swap ever moved value: Cora holds 98.000000 SBX-DUSD in the books and on chain",
		finalDUSD.Equal(money.QuantityFromInt64(98_000_000)) && finalChain.Equal(finalDUSD), "books %s, chain %s", showDUSD(finalDUSD),
		showDUSD(finalChain))
	return nil
}

func toolNames(r agent.DelegatedToolsRecord) []string {
	out := make([]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		out = append(out, t.Tool)
	}
	return out
}

func firstStatus(calls []agent.DelegatedRecord) string {
	if len(calls) == 0 {
		return ""
	}
	return calls[0].Status
}

// tokenBalance is owner's balance of a sandbox token on sbx-solana, in base
// units, as the devnet host reports it.
func tokenBalance(ctx context.Context, w *sandboxworld.World, owner devnet.PublicKey, token assets.Asset) (money.Quantity, error) {
	mint, err := devnet.ParsePublicKey(token.MintAddress)
	if err != nil {
		return money.Quantity{}, err
	}
	b, err := w.Host.TokenBalance(ctx, devnethost.ChainSolana, owner, mint)
	if err != nil {
		return money.Quantity{}, err
	}
	return money.QuantityFromBigInt(new(big.Int).SetUint64(b)), nil
}
