package settlement

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// handler is one step's three phases. prepare runs in the RUNNING
// transaction before any side effect; effect runs outside any transaction
// and may call providers; persist runs in the SUCCEEDED transaction; output
// is the evidence recorded with SUCCEEDED.
type handler struct {
	prepare func(ctx context.Context, tx pgx.Tx) error
	effect  func(ctx context.Context) error
	persist func(ctx context.Context, tx pgx.Tx) error
	output  func() any
}

func zeroQty() money.Quantity { return money.Quantity{} }

func (r *runner) handlerFor(rc *runCtx, step Step) (handler, error) {
	switch step.Type {
	case StepValidateEligibility:
		return r.validateEligibility(rc, step)
	case StepEvaluateRisk:
		return r.evaluateRisk(rc, step)
	case StepReserveCapital:
		return r.reserveCapital(rc, step)
	case StepResolveVenueListing:
		return r.resolveListing(rc, step)
	case StepLocateSettlementAsset:
		return r.locateSettlementAsset(rc, step)
	case StepAcquireQuote:
		return r.acquireQuote(rc, step)
	case StepValidateQuote:
		return r.validateQuote(rc, step)
	case StepFinalRiskCheck:
		return r.finalRiskCheck(rc, step)
	case StepBuildTransaction:
		return r.buildTransaction(rc, step)
	case StepInspectTransaction:
		return r.inspectTransaction(rc, step)
	case StepRequestSignature:
		return r.requestSignature(rc, step)
	case StepSubmit:
		return r.submit(rc, step)
	case StepObserveFinality:
		return r.observeFinality(rc, step)
	case StepReconcile:
		return r.reconcile(rc, step)
	case StepPostLedger:
		return r.postLedger(rc, step)
	case StepUpdatePosition:
		return r.updatePosition(rc, step)
	case StepReleaseReservation:
		return r.releaseReservationStep(rc, step)
	}
	if step.Type.Reserved() {
		return handler{}, errs.Newf(errs.CodeUnsupported, "settlement: reserved step %s cannot execute (CROSS_CHAIN is disabled)", step.Type)
	}
	return handler{}, errs.Newf(errs.CodeInternal, "settlement: no handler for step %s", step.Type)
}

func preFailure(code errs.Code, reason string) *stepFailure {
	return &stepFailure{code: code, reason: reason, orderTo: execution.OrderRejected, rejection: string(code), release: true, planTo: PlanFailed}
}

// --- 1. VALIDATE_ELIGIBILITY --------------------------------------------------

func (r *runner) validateEligibility(_ *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[EligibilityStepInput](step)
	if err != nil {
		return handler{}, err
	}
	return handler{
		effect: func(context.Context) error {
			if !in.Eligible {
				return preFailure(errs.CodeEligibilityJurisdiction, "eligibility decision is not eligible")
			}
			if in.PolicyVersion == "" || in.DecisionHash == "" {
				return preFailure(errs.CodeValidationFailed, "eligibility decision is incomplete")
			}
			return nil
		},
		output: func() any {
			return map[string]any{"eligible": true, "policy_version": in.PolicyVersion, "decision_hash": in.DecisionHash}
		},
	}, nil
}

// --- 2. EVALUATE_RISK -----------------------------------------------------------

func (r *runner) evaluateRisk(_ *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[RiskStepInput](step)
	if err != nil {
		return handler{}, err
	}
	return handler{
		effect: func(context.Context) error {
			if in.Verdict != risk.Allow {
				return preFailure(errs.CodeNoValidPlan, "PRE_TRADE risk decision is "+string(in.Verdict))
			}
			if in.Stage != risk.StagePreTrade {
				return preFailure(errs.CodeValidationFailed, "risk decision stage is not PRE_TRADE")
			}
			if in.DecisionHash == "" {
				return preFailure(errs.CodeValidationFailed, "risk decision is incomplete")
			}
			return nil
		},
		output: func() any {
			return map[string]any{"verdict": string(in.Verdict), "policy_version": in.PolicyVersion, "decision_hash": in.DecisionHash}
		},
	}, nil
}

// --- 3. RESERVE_CAPITAL ---------------------------------------------------------

func (r *runner) reserveCapital(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[ReserveStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := ReserveOutput{Quantity: in.Quantity}
	if rc.dryRun {
		out.DryRun = true
		return handler{output: func() any { return out }}, nil
	}
	return handler{
		persist: func(ctx context.Context, tx pgx.Tx) error {
			if rc.reservationID != "" {
				rid, err := capital.ParseReservationID(rc.reservationID)
				if err != nil {
					return errs.Wrap(err, errs.CodeInternal, "settlement: reservation id")
				}
				res, err := r.d.Capital.Get(ctx, querierOf(tx), rid)
				if err != nil {
					return err
				}
				if res.Status != capital.ReservationActive {
					return preFailure(errs.CodeInvalidStateTransition, "reservation "+rc.reservationID+" is "+string(res.Status))
				}
				if res.Remaining().Cmp(in.Quantity) < 0 {
					return preFailure(errs.CodeInsufficientBuyingPower, "existing reservation is smaller than the plan's input quantity")
				}
				out.ReservationID, out.Existing = rc.reservationID, true
				return nil
			}
			var env *capital.EnvelopeID
			if in.EnvelopeID != "" {
				e, err := capital.ParseEnvelopeID(in.EnvelopeID)
				if err != nil {
					return errs.Wrap(err, errs.CodeValidationFailed, "settlement: envelope id")
				}
				env = &e
			}
			res, err := r.d.Capital.Reserve(ctx, tx, capital.ReserveRequest{
				AccountID: in.AccountID, AssetID: in.AssetID, Quantity: in.Quantity, USDMinor: in.USDMinor, EnvelopeID: env,
				IntentID: in.IntentID, ActorType: security.ActorType(in.ActorType), ActorID: in.ActorID,
				IdempotencyKey: step.SemanticIdempotencyKey, TTL: time.Duration(in.TTLMS) * time.Millisecond, Reason: in.Reason,
			})
			if err != nil {
				if errs.HasCode(err, errs.CodeInsufficientBuyingPower) {
					return preFailure(errs.CodeInsufficientBuyingPower, err.Error())
				}
				return err
			}
			rc.reservationID = res.ID.String()
			out.ReservationID = rc.reservationID
			return nil
		},
		output: func() any { return out },
	}, nil
}

// --- 4. RESOLVE_VENUE_LISTING --------------------------------------------------

func (r *runner) resolveListing(_ *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[ListingStepInput](step)
	if err != nil {
		return handler{}, err
	}
	return handler{
		effect: func(context.Context) error {
			if name := r.d.Adapter.Name(); name != in.Provider {
				return preFailure(errs.CodeProviderUnavailable, fmt.Sprintf("plan selected provider %s but the executor is wired to %s", in.Provider, name))
			}
			return nil
		},
		output: func() any { return in },
	}, nil
}

// --- 5. LOCATE_SETTLEMENT_ASSET -----------------------------------------------

func (r *runner) locateSettlementAsset(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[SettlementAssetStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := LocateOutput{WalletAddress: in.WalletAddress, Mint: in.Mint, Required: in.RequiredQuantity, Observer: r.d.Observer.Name()}
	return handler{
		effect: func(ctx context.Context) error {
			if rc.intent.WalletChain != "" && in.Chain != rc.intent.WalletChain {
				return preFailure(errs.CodeUnsupported, "wallet chain "+rc.intent.WalletChain+" does not match the listing network "+in.Chain)
			}
			balances, err := r.d.Observer.GetBalances(ctx, in.WalletAddress, []string{in.Mint})
			if err != nil {
				return err
			}
			found := false
			for _, b := range balances {
				if b.Mint == in.Mint {
					out.Balance, out.ObservedAt, found = b.Quantity, b.ObservedAt, true
				}
			}
			if !found || out.Balance.Cmp(in.RequiredQuantity) < 0 {
				return preFailure(errs.CodeInsufficientBuyingPower, "wallet holds "+out.Balance.String()+" of the settlement asset; the plan needs "+in.RequiredQuantity.String())
			}
			return nil
		},
		output: func() any { return out },
	}, nil
}

// --- 6. ACQUIRE_QUOTE -----------------------------------------------------------

func (r *runner) acquireQuote(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[QuoteStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := QuoteOutput{}
	return handler{
		effect: func(ctx context.Context) error {
			req := in.Request
			req.PlanID = rc.plan.ID.String()
			q, err := r.d.Adapter.Quote(ctx, req)
			if err != nil {
				return err
			}
			hc := rc.plan.HardConstraints
			if q.InputAsset != hc.InputAsset.ID || q.OutputAsset != hc.OutputAsset.ID {
				return preFailure(errs.CodeValidationFailed, "quote assets do not match the plan")
			}
			if !q.InputQuantity.IsPositive() || q.InputQuantity.Cmp(hc.MaxInputQuantity) > 0 {
				return preFailure(errs.CodeValidationFailed, "quote input quantity "+q.InputQuantity.String()+" exceeds the reserved "+hc.MaxInputQuantity.String())
			}
			if q.Provider == "" {
				q.Provider = r.d.Adapter.Name()
			}
			if q.IntentID == "" {
				q.IntentID = rc.plan.IntentID
			}
			if q.RawResponseRef == "" || len(q.RawResponseHash) == 0 {
				ev, err := execution.StoreEvidence(ctx, r.d.Archive, r.evidenceKey(rc, step, "quote"), q)
				if err != nil {
					return err
				}
				if q.RawResponseRef == "" {
					q.RawResponseRef = ev.RawRef
				}
				if len(q.RawResponseHash) == 0 {
					q.RawResponseHash = ev.Hash
				}
			}
			out.Quote, out.RawRef = q, q.RawResponseRef
			return nil
		},
		persist: func(ctx context.Context, tx pgx.Tx) error {
			qid, err := r.d.Quotes.Save(ctx, tx, out.Quote)
			if err != nil {
				return err
			}
			out.Quote.ID, out.QuoteID = qid, qid
			rc.quote, rc.quoteID = out.Quote, qid
			return nil
		},
		output: func() any { return out },
	}, nil
}

// --- 7. VALIDATE_QUOTE ----------------------------------------------------------

func (r *runner) validateQuote(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[ValidateQuoteStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := OrderOutput{DryRun: rc.dryRun}
	return handler{
		effect: func(ctx context.Context) error {
			if rc.quoteID == "" {
				return errs.New(errs.CodeInternal, "settlement: VALIDATE_QUOTE without a quote")
			}
			q := rc.quote
			if err := r.d.Adapter.ValidateQuote(ctx, q); err != nil {
				if errs.HasCode(err, errs.CodeQuoteExpired) || errs.HasCode(err, errs.CodeValidationFailed) {
					return preFailure(errs.CodeOf(err), "venue rejected the quote: "+err.Error())
				}
				return err
			}
			now := r.now()
			switch {
			case !q.ExpiresAt.After(now):
				return preFailure(errs.CodeQuoteExpired, "quote expired at "+q.ExpiresAt.Format(time.RFC3339Nano))
			case now.Sub(q.ReceivedAt) > time.Duration(in.QuoteMaxAgeMS)*time.Millisecond:
				return preFailure(errs.CodeQuoteExpired, "quote is older than the plan's maximum age")
			case !in.Deadline.After(now):
				return preFailure(errs.CodeQuoteExpired, "plan deadline passed")
			case in.MaxSlippageBPS > 0 && q.SlippageBPS > in.MaxSlippageBPS:
				return preFailure(errs.CodeValidationFailed, fmt.Sprintf("quote slippage %d bps exceeds %d", q.SlippageBPS, in.MaxSlippageBPS))
			case in.MaxPriceImpactBPS > 0 && q.PriceImpactBPS > in.MaxPriceImpactBPS:
				return preFailure(errs.CodeValidationFailed, fmt.Sprintf("quote price impact %d bps exceeds %d", q.PriceImpactBPS, in.MaxPriceImpactBPS))
			case q.InputQuantity.Cmp(in.MaxInputQuantity) > 0:
				return preFailure(errs.CodeValidationFailed, "quote input exceeds the reserved quantity")
			case q.MinimumOutput.Cmp(in.MinOutputQuantity) < 0:
				return preFailure(errs.CodeValidationFailed, "quote minimum output "+q.MinimumOutput.String()+" is below the plan floor "+in.MinOutputQuantity.String())
			case q.MinimumOutput.Cmp(q.ExpectedOutput) > 0:
				return preFailure(errs.CodeValidationFailed, "quote minimum output exceeds its expected output")
			}
			if in.MaxFeeBPS > 0 {
				feeBPS, err := quoteFeeBPS(rc, q)
				if err != nil {
					return err
				}
				if feeBPS > in.MaxFeeBPS {
					return preFailure(errs.CodeValidationFailed, fmt.Sprintf("quote fees %d bps exceed %d", feeBPS, in.MaxFeeBPS))
				}
			}
			return nil
		},
		persist: func(ctx context.Context, tx pgx.Tx) error {
			out.QuoteID = rc.quoteID
			if rc.dryRun {
				return nil
			}
			if rc.order != nil {
				out.OrderID = rc.order.ID.String()
				return nil
			}
			o, err := r.createOrder(ctx, tx, rc)
			if err != nil {
				return err
			}
			rc.order = &o
			out.OrderID = o.ID.String()
			return nil
		},
		output: func() any { return out },
	}, nil
}

// quoteFeeBPS returns venue + platform fee of a quote as basis points of the
// quote-asset leg.
func quoteFeeBPS(rc *runCtx, q execution.QuoteSnapshot) (money.BPS, error) {
	hc := rc.plan.HardConstraints
	quoteLeg := q.InputQuantity
	if hc.Side == execution.SideSell {
		quoteLeg = q.ExpectedOutput
	}
	if !quoteLeg.IsPositive() {
		return 0, nil
	}
	total := q.EstVenueFee.Add(q.PlatformFee)
	bps, err := total.Mul(money.QuantityFromInt64(int64(money.OneHundredPercent))).Div(quoteLeg, money.RoundUp)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "settlement: quote fee bps")
	}
	n, err := bps.Int64()
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeOverflow, "settlement: quote fee bps")
	}
	return money.BPS(n), nil
}

// createOrder inserts the order in CREATED. The schema requires the quote
// id at insert, which is why the order appears here rather than before
// RESERVE_CAPITAL; the states VALIDATED, CAPITAL_RESERVED and PLANNED are
// then recorded by the steps that justify them (FINAL_RISK_CHECK,
// BUILD_TRANSACTION), one status change per transaction as migration 00603
// requires.
func (r *runner) createOrder(ctx context.Context, tx pgx.Tx, rc *runCtx) (execution.Order, error) {
	hc := rc.plan.HardConstraints
	o := execution.Order{
		ID: execution.NewOrderID(), IntentID: rc.plan.IntentID, PlanID: rc.plan.ID.String(), AccountID: rc.accountID,
		InstrumentID: rc.intent.InstrumentID, VenueListingID: rc.plan.SelectedVenueListingID, Side: hc.Side, Mode: rc.intent.Mode,
		InputAssetID: hc.InputAsset.ID, InputQuantity: rc.quote.InputQuantity, OutputAssetID: hc.OutputAsset.ID, MinOutputQuantity: rc.quote.MinimumOutput,
		ReservationID: rc.reservationID, QuoteID: rc.quoteID, CorrelationID: rc.intent.CorrelationID,
	}
	if o.MinOutputQuantity.Cmp(hc.MinOutputQuantity) < 0 {
		o.MinOutputQuantity = hc.MinOutputQuantity
	}
	return r.d.Orders.Create(ctx, tx, o)
}

// advanceOrder moves the order to `to` when it currently sits in `from`;
// any other status (a resumed or replanned run) leaves it alone.
func (r *runner) advanceOrder(ctx context.Context, tx pgx.Tx, rc *runCtx, from, to execution.OrderStatus, reason string) error {
	if rc.order == nil || rc.order.Status != from {
		return nil
	}
	o, err := r.d.Orders.Transition(ctx, tx, rc.order.ID, to, execution.TransitionEvidence{
		Reason: reason, ActorType: string(security.ActorSystem), ActorID: ExecutorActorID,
	})
	if err != nil {
		return err
	}
	rc.order = &o
	return nil
}

// --- 8. FINAL_RISK_CHECK --------------------------------------------------------

func (r *runner) finalRiskCheck(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[FinalRiskStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := FinalRiskOutput{}
	return handler{
		// The decision is persisted in its own transaction so a rejection
		// is on record even though the step fails.
		effect: func(ctx context.Context) error {
			var snap risk.Decision
			var decisionID string
			if err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				ks, err := r.d.KillSwitches.Snapshot(ctx, querierOf(tx))
				if err != nil {
					return err
				}
				d, idText, err := r.d.Risk.CheckFinal(ctx, tx, FinalRiskRequest{
					PlanID: rc.plan.ID, PlanHash: rc.plan.Hash, IntentID: rc.plan.IntentID, AccountID: rc.intent.AccountID, AgentID: in.AgentID,
					Quote: rc.quote, Constraints: rc.plan.HardConstraints, KillSwitches: ks, CorrelationID: rc.intent.CorrelationID, Now: r.now(),
				})
				if err != nil {
					return err
				}
				snap, decisionID = d, idText
				return nil
			}); err != nil {
				return err
			}
			out = FinalRiskOutput{DecisionID: decisionID, DecisionHash: snap.Hash, Verdict: snap.Verdict, ReasonCodes: snap.ReasonCodes}
			if snap.Verdict != risk.Allow {
				f := preFailure(errs.CodeNoValidPlan, "FINAL risk decision rejected: "+strings.Join(snap.ReasonCodes, ","))
				f.fields = map[string]any{"reasons": snap.ReasonCodes, "decision_id": decisionID}
				return f
			}
			if snap.Stage != risk.StageFinal {
				return preFailure(errs.CodeValidationFailed, "risk decision stage is not FINAL")
			}
			q := rc.quote
			c := snap.Constraints
			switch {
			case c.MaxSlippageBPS > 0 && q.SlippageBPS > c.MaxSlippageBPS:
				return preFailure(errs.CodeNoValidPlan, "quote slippage exceeds the FINAL risk bound")
			case c.MaxPriceImpactBPS > 0 && q.PriceImpactBPS > c.MaxPriceImpactBPS:
				return preFailure(errs.CodeNoValidPlan, "quote price impact exceeds the FINAL risk bound")
			case c.MaxNotionalUSD.IsPositive() && rc.plan.HardConstraints.NotionalUSD.Cmp(c.MaxNotionalUSD) > 0:
				return preFailure(errs.CodeNoValidPlan, "plan notional exceeds the FINAL risk maximum")
			case in.MaxNotionalUSD.IsPositive() && rc.plan.HardConstraints.NotionalUSD.Cmp(in.MaxNotionalUSD) > 0:
				return preFailure(errs.CodeNoValidPlan, "plan notional exceeds the approved maximum")
			}
			rc.riskDecisionID = decisionID
			return nil
		},
		persist: func(ctx context.Context, tx pgx.Tx) error {
			if rc.dryRun {
				return nil
			}
			return r.advanceOrder(ctx, tx, rc, execution.OrderCreated, execution.OrderValidated, "eligibility, PRE_TRADE and FINAL risk decisions verified ("+out.DecisionID+")")
		},
		output: func() any { return out },
	}, nil
}

// --- 9. BUILD_TRANSACTION -------------------------------------------------------

func (r *runner) buildTransaction(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[BuildStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := BuildOutput{DryRun: rc.dryRun}
	attemptNo := int32(1)
	if rc.attempt != nil {
		attemptNo = rc.attempt.AttemptNo + 1
	}
	return handler{
		prepare: func(ctx context.Context, tx pgx.Tx) error {
			if rc.dryRun {
				return nil
			}
			return r.advanceOrder(ctx, tx, rc, execution.OrderValidated, execution.OrderCapitalReserved, "reservation "+rc.reservationID+" active")
		},
		effect: func(ctx context.Context) error {
			if rc.quoteID == "" {
				return errs.New(errs.CodeInternal, "settlement: BUILD_TRANSACTION without a quote")
			}
			action, err := r.d.Adapter.Build(ctx, execution.BuildRequest{
				PlanID: rc.plan.ID.String(), PlanHash: rc.plan.Hash, AttemptNo: attemptNo, Quote: rc.quote, WalletAddress: in.WalletAddress,
				MinOutput: in.MinOutputQuantity, MaxSlippageBPS: in.MaxSlippageBPS, MaxPriorityFeeLamports: in.MaxPriorityFeeLamports,
				MaxComputeUnits: in.MaxComputeUnits, Deadline: rc.plan.HardConstraints.Deadline,
			})
			if err != nil {
				return err
			}
			if len(action.Bytes) == 0 {
				return preFailure(errs.CodeProviderUnavailable, "adapter returned an empty transaction")
			}
			if len(action.Hash) == 0 {
				action.Hash = execution.HashBytes(action.Bytes)
			}
			ev, err := execution.StoreRaw(ctx, r.d.Archive, r.evidenceKey(rc, step, "unsigned-tx"), execution.ContentTypeBinary, action.Bytes)
			if err != nil {
				return err
			}
			if action.RawRef == "" {
				action.RawRef = ev.RawRef
			}
			out.Action, out.EvidenceRef, out.AttemptNo = action, ev.RawRef, attemptNo
			rc.action = action
			return nil
		},
		persist: func(ctx context.Context, tx pgx.Tx) error {
			if rc.dryRun {
				return nil
			}
			if rc.order == nil {
				return errs.New(errs.CodeInternal, "settlement: BUILD_TRANSACTION without an order")
			}
			lvbh := int64(out.Action.LastValidBlockHeight) // #nosec G115 -- block heights fit int64
			a, err := r.d.Attempts.Create(ctx, tx, execution.Attempt{
				ID: execution.NewAttemptID(), OrderID: rc.order.ID, PlanID: rc.plan.ID.String(), WalletID: rc.intent.WalletID,
				Provider: r.d.Adapter.Name(), ProviderRequestID: out.Action.ProviderRequestID, QuoteID: rc.quoteID,
				UnsignedTxHash: out.Action.Hash, UnsignedTxRef: out.Action.RawRef, RecentBlockhash: out.Action.RecentBlockhash,
				LastValidBlockHeight: &lvbh, Status: execution.AttemptBuilt, CorrelationID: rc.intent.CorrelationID,
			})
			if err != nil {
				return err
			}
			rc.attempt = &a
			out.AttemptID, out.AttemptNo = a.ID.String(), a.AttemptNo
			return r.advanceOrder(ctx, tx, rc, execution.OrderCapitalReserved, execution.OrderPlanned, "attempt "+a.ID.String()+" built under plan "+rc.plan.ID.String())
		},
		output: func() any { return out },
	}, nil
}

// --- 10. INSPECT_TRANSACTION ----------------------------------------------------

func (r *runner) inspectTransaction(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[InspectStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := InspectOutput{}
	var result InspectResult
	return handler{
		effect: func(ctx context.Context) error {
			if len(rc.action.Bytes) == 0 {
				return errs.New(errs.CodeInternal, "settlement: INSPECT_TRANSACTION without a built transaction")
			}
			req := InspectRequest{PlanID: rc.plan.ID, Action: rc.action, Expectations: r.expectations(rc, in)}
			if rc.attempt != nil {
				req.AttemptID = rc.attempt.ID
			}
			res, err := r.d.Inspector.Inspect(ctx, req)
			if err != nil {
				return err
			}
			result = res
			sort.Strings(result.ReasonCodes)
			ev, err := execution.StoreEvidence(ctx, r.d.Archive, r.evidenceKey(rc, step, "inspection"), map[string]any{
				"approved": res.Approved, "reason_codes": result.ReasonCodes, "checks": res.Checks, "inspector_version": res.InspectorVersion,
				"simulation_ok": res.SimulationOK, "simulation_ref": res.SimulationRef,
			})
			if err != nil {
				return err
			}
			out = InspectOutput{Approved: res.Approved, ReasonCodes: result.ReasonCodes, InspectorVersion: res.InspectorVersion, EvidenceRef: ev.RawRef}
			if !res.Approved {
				f := preFailure(errs.CodeSigningRejected, "transaction inspection rejected: "+strings.Join(result.ReasonCodes, ","))
				f.attemptTo = execution.AttemptInspectionRejected
				f.rejection = "INSPECTION_REJECTED"
				f.fields = map[string]any{"reasons": result.ReasonCodes, "evidence_ref": ev.RawRef}
				return f
			}
			return nil
		},
		persist: func(ctx context.Context, tx pgx.Tx) error {
			if rc.dryRun || rc.attempt == nil {
				return nil
			}
			if rc.attempt.Status == execution.AttemptInspected {
				return nil
			}
			to := execution.AttemptInspected
			a, err := r.d.Attempts.Update(ctx, tx, rc.attempt.ID, execution.AttemptPatch{
				Status: &to, InspectionResult: result.Checks, SimulationOK: result.SimulationOK, SimulationRef: nonEmpty(result.SimulationRef), Reason: "inspection approved",
			})
			if err != nil {
				return err
			}
			rc.attempt = &a
			return nil
		},
		output: func() any { return out },
	}, nil
}

func (r *runner) expectations(rc *runCtx, in InspectStepInput) InspectExpectations {
	return InspectExpectations{
		InspectStepInput: in, PlanHash: rc.plan.Hash, QuoteID: rc.quoteID, RecentBlockhash: rc.action.RecentBlockhash,
		LastValidBlockHeight: rc.action.LastValidBlockHeight, InputAsset: rc.plan.HardConstraints.InputAsset.ID, OutputAsset: rc.plan.HardConstraints.OutputAsset.ID,
	}
}

// --- 11. REQUEST_SIGNATURE ------------------------------------------------------

func (r *runner) requestSignature(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[SignStepInput](step)
	if err != nil {
		return handler{}, err
	}
	inspect, err := r.inspectInputOf(rc)
	if err != nil {
		return handler{}, err
	}
	out := SignOutput{}
	return handler{
		prepare: func(ctx context.Context, tx pgx.Tx) error {
			if r.signer == nil || rc.dryRun {
				return errs.New(errs.CodeForbidden, "settlement: signing is not available to this executor")
			}
			if rc.attempt == nil {
				return errs.New(errs.CodeInternal, "settlement: REQUEST_SIGNATURE without an attempt")
			}
			if rc.attempt.Status != execution.AttemptInspected {
				return nil
			}
			to := execution.AttemptSigningRequested
			a, err := r.d.Attempts.Update(ctx, tx, rc.attempt.ID, execution.AttemptPatch{Status: &to, Reason: "signature requested"})
			if err != nil {
				return err
			}
			rc.attempt = &a
			return nil
		},
		effect: func(ctx context.Context) error {
			res, err := r.signer.Sign(ctx, SignRequest{
				AttemptID: rc.attempt.ID, PlanID: rc.plan.ID, IntentID: rc.plan.IntentID, RiskDecisionID: rc.riskDecisionID, WalletID: in.WalletID,
				UnsignedTx: rc.action.Bytes, ExpectedTxHash: rc.action.Hash, PlanHash: rc.plan.Hash, QuoteID: rc.quoteID,
				Expectations: r.expectations(rc, inspect), IdempotencyKey: step.SemanticIdempotencyKey + ":" + rc.attempt.ID.String(),
			})
			if err != nil {
				return err
			}
			if !res.Approved {
				f := preFailure(errs.CodeSigningRejected, "signing rejected: "+strings.Join(res.ReasonCodes, ","))
				f.attemptTo = execution.AttemptSigningRejected
				f.rejection = "SIGNING_REJECTED"
				f.fields = map[string]any{"reasons": res.ReasonCodes, "decision_id": res.DecisionID}
				return f
			}
			if len(res.SignedTx) == 0 || res.TxSignature == "" {
				return errs.New(errs.CodeInternal, "settlement: signer approved without a signed transaction")
			}
			ev, err := execution.StoreRaw(ctx, r.d.Archive, r.evidenceKey(rc, step, "signed-tx"), execution.ContentTypeBinary, res.SignedTx)
			if err != nil {
				return err
			}
			out = SignOutput{DecisionID: res.DecisionID, TxSignature: res.TxSignature, SignedTx: res.SignedTx, SignedTxHash: ev.Hash, EvidenceRef: ev.RawRef}
			return nil
		},
		persist: func(ctx context.Context, tx pgx.Tx) error {
			a, err := r.d.Attempts.SetSignature(ctx, tx, rc.attempt.ID, out.TxSignature, out.SignedTxHash)
			if err != nil {
				return err
			}
			rc.attempt = &a
			if rc.attempt.Status != execution.AttemptSigned {
				to := execution.AttemptSigned
				a, err := r.d.Attempts.Update(ctx, tx, rc.attempt.ID, execution.AttemptPatch{Status: &to, SigningDecisionID: nonEmpty(out.DecisionID), Reason: "signed"})
				if err != nil {
					return err
				}
				rc.attempt = &a
			}
			rc.signed = out
			return nil
		},
		output: func() any { return out },
	}, nil
}

func (r *runner) inspectInputOf(rc *runCtx) (InspectStepInput, error) {
	s, ok := rc.plan.StepByType(StepInspectTransaction)
	if !ok {
		return InspectStepInput{}, errs.New(errs.CodeInternal, "settlement: plan has no INSPECT_TRANSACTION step")
	}
	return DecodeStepInput[InspectStepInput](s)
}

// --- 12. SUBMIT ------------------------------------------------------------------

func (r *runner) submit(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[SubmitStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := SubmitOutput{}
	return handler{
		prepare: func(ctx context.Context, tx pgx.Tx) error {
			if rc.dryRun {
				return errs.New(errs.CodeForbidden, "settlement: dry-run plans never submit")
			}
			if rc.attempt == nil || rc.order == nil || rc.signed.TxSignature == "" {
				return errs.New(errs.CodeInternal, "settlement: SUBMIT without a signed attempt")
			}
			// The reservation is locked to the order before the transaction
			// leaves the process (PART 48 step 1).
			if rc.reservationID != "" {
				rid, err := capital.ParseReservationID(rc.reservationID)
				if err != nil {
					return errs.Wrap(err, errs.CodeInternal, "settlement: reservation id")
				}
				if err := r.d.Capital.LockForOrder(ctx, tx, rid, rc.order.ID.String()); err != nil {
					return err
				}
			}
			if rc.attempt.Status == execution.AttemptSigned {
				to := execution.AttemptSubmitting
				a, err := r.d.Attempts.Update(ctx, tx, rc.attempt.ID, execution.AttemptPatch{Status: &to, Reason: "submitting"})
				if err != nil {
					return err
				}
				rc.attempt = &a
			}
			return r.advanceOrder(ctx, tx, rc, execution.OrderPlanned, execution.OrderSubmitting, "submitting attempt "+rc.attempt.ID.String())
		},
		effect: func(ctx context.Context) error {
			res, err := r.d.Adapter.Submit(ctx, execution.SignedSubmission{
				AttemptID: rc.attempt.ID.String(), OrderID: rc.order.ID.String(), PlanID: rc.plan.ID.String(), Chain: in.Chain,
				SignedTx: rc.signed.SignedTx, TxSignature: rc.signed.TxSignature, LastValidBlockHeight: rc.action.LastValidBlockHeight,
				IdempotencyKey: step.SemanticIdempotencyKey,
			})
			if err != nil {
				return r.classifySubmitError(ctx, rc, err)
			}
			if res.ExternalRef.TxSignature == "" {
				res.ExternalRef.TxSignature = rc.signed.TxSignature
			}
			if res.ExternalRef.Venue == "" {
				res.ExternalRef.Venue = rc.plan.HardConstraints.Venue
			}
			if res.ExternalRef.WalletAddress == "" {
				res.ExternalRef.WalletAddress = rc.intent.WalletAddress
			}
			if res.AcceptedAt.IsZero() {
				res.AcceptedAt = r.now()
			}
			ev, err := execution.StoreEvidence(ctx, r.d.Archive, r.evidenceKey(rc, step, "submit-response"), res)
			if err != nil {
				return err
			}
			out = SubmitOutput{ExternalRef: res.ExternalRef, AcceptedAt: res.AcceptedAt, EvidenceRef: ev.RawRef}
			return nil
		},
		persist: func(ctx context.Context, tx pgx.Tx) error {
			to := execution.AttemptSubmitted
			at := out.AcceptedAt
			a, err := r.d.Attempts.Update(ctx, tx, rc.attempt.ID, execution.AttemptPatch{
				Status: &to, SubmittedAt: &at, SubmitResponseRef: nonEmpty(out.EvidenceRef), Finality: finalityPtr(execution.FinalitySubmitted), Reason: "submitted",
			})
			if err != nil {
				return err
			}
			rc.attempt = &a
			o, err := r.d.Orders.Transition(ctx, tx, rc.order.ID, execution.OrderSubmitted, execution.TransitionEvidence{
				Reason: "submitted " + out.ExternalRef.TxSignature, EvidenceRef: out.EvidenceRef, ActorType: string(security.ActorSystem), ActorID: ExecutorActorID,
				CausationID: rc.attempt.ID.String(),
			})
			if err != nil {
				return err
			}
			rc.order = &o
			rc.externalRef = out.ExternalRef
			return nil
		},
		output: func() any { return out },
	}, nil
}

// classifySubmitError applies the PART 48 rule. SUBMISSION_STATE_UNKNOWN,
// context timeouts and unclassified errors are unknown outcomes (returned
// as-is so the loop enters recovery). A definitive provider rejection is a
// failure only when the signature is absent from the chain; otherwise the
// transaction exists and the outcome is unknown.
func (r *runner) classifySubmitError(ctx context.Context, rc *runCtx, err error) error {
	code := errs.CodeOf(err)
	definitive := false
	if e, ok := errs.As(err); ok {
		switch e.Code {
		case errs.CodeValidationFailed, errs.CodeQuoteExpired, errs.CodeUnsupported, errs.CodeForbidden, errs.CodeInvalidStateTransition:
			definitive = true
		}
	}
	if !definitive {
		return err
	}
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	obs, oerr := r.d.Observer.GetTransaction(octx, rc.signed.TxSignature)
	if oerr != nil || obs.Found {
		// Cannot prove absence: unknown, never a failure.
		return errs.Wrap(err, errs.CodeSubmissionStateUnknown, "settlement: provider rejected the submission but absence is not proven")
	}
	f := preFailure(code, "submission rejected: "+err.Error())
	f.attemptTo = execution.AttemptFailed
	f.rejection = string(code)
	return f
}

// --- 13. OBSERVE_FINALITY -------------------------------------------------------

func (r *runner) observeFinality(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[ObserveStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := ObserveOutput{Observer: r.d.Observer.Name(), FillIDs: []string{}}
	var status execution.ExecutionStatus
	return handler{
		effect: func(ctx context.Context) error {
			if rc.attempt == nil || rc.order == nil {
				return errs.New(errs.CodeInternal, "settlement: OBSERVE_FINALITY without an attempt")
			}
			ref := rc.externalRef
			if ref.TxSignature == "" {
				ref.TxSignature = rc.attempt.TxSignature
			}
			if ref.Venue == "" {
				ref.Venue = rc.plan.HardConstraints.Venue
			}
			required := in.RequiredFinality
			if !required.Valid() {
				required = execution.FinalityConfirmed
			}
			poll := time.Duration(in.PollIntervalMS) * time.Millisecond
			if poll <= 0 {
				poll = time.Second
			}
			for {
				st, err := r.d.Adapter.Status(ctx, ref)
				if err != nil {
					return err
				}
				obs, err := r.d.Observer.GetTransaction(ctx, ref.TxSignature)
				if err != nil {
					return err
				}
				level, decided, failure := r.combineObservation(ctx, rc, st, obs)
				if failure != nil {
					return failure
				}
				if decided && r.finalityPolicySatisfies(level, required) {
					status = st
					out.State, out.Finality, out.Slot = st.State, level, st.Slot
					if obs.Slot > 0 {
						out.Slot = obs.Slot
					}
					ev, err := execution.StoreEvidence(ctx, r.d.Archive, r.evidenceKey(rc, step, "status"), map[string]any{"provider": st, "observer": obs})
					if err != nil {
						return err
					}
					out.EvidenceRef = ev.RawRef
					return nil
				}
				if err := r.d.Sleep(ctx, poll); err != nil {
					return err
				}
			}
		},
		persist: func(ctx context.Context, tx pgx.Tx) error {
			if err := r.advanceAttempt(ctx, tx, rc, out.Finality, out.EvidenceRef); err != nil {
				return err
			}
			for _, evt := range status.Fills {
				f, err := r.recordFill(ctx, tx, rc, evt, execution.FillFromProvider, out.Finality, out.EvidenceRef)
				if err != nil {
					return err
				}
				out.FillIDs = append(out.FillIDs, f.ID.String())
			}
			if len(status.Fills) == 0 && rc.order.Status == execution.OrderSubmitted {
				o, err := r.d.Orders.Transition(ctx, tx, rc.order.ID, execution.OrderAcknowledged, execution.TransitionEvidence{
					Reason: "observed at " + string(out.Finality), EvidenceRef: out.EvidenceRef, ActorType: string(security.ActorSystem), ActorID: ExecutorActorID,
				})
				if err != nil {
					return err
				}
				rc.order = &o
			}
			rc.observed = out
			return nil
		},
		output: func() any { return out },
	}, nil
}

func (r *runner) finalityPolicySatisfies(observed, required execution.FinalityLevel) bool {
	return execution.DefaultFinalityPolicy().Satisfies(observed, required)
}

// combineObservation merges the provider's and the observer's views without
// ever taking the optimistic one. It returns the agreed finality level and
// whether a level is established; a stepFailure when the transaction is
// proven failed or expired; and a paused failure when the two disagree on a
// terminal outcome.
func (r *runner) combineObservation(ctx context.Context, rc *runCtx, st execution.ExecutionStatus, obs TxObservation) (execution.FinalityLevel, bool, error) {
	switch st.State {
	case execution.ExternalFailed:
		if obs.Found && !obs.Failed {
			return "", false, &stepFailure{
				code: errs.CodeReconciliationRequired, reason: "provider reports FAILED but the chain observer sees a successful transaction",
				orderTo: execution.OrderReconciliationRequired, pause: true,
			}
		}
		f := &stepFailure{
			code: errs.CodeVenueUnavailable, reason: "transaction failed on chain: " + st.Error,
			orderTo: execution.OrderFailedFinal, attemptTo: execution.AttemptFailed, release: true, planTo: PlanFailed,
		}
		return "", false, f
	case execution.ExternalExpired, execution.ExternalNotFound:
		if obs.Found {
			if obs.Failed {
				return "", false, &stepFailure{
					code: errs.CodeVenueUnavailable, reason: "transaction failed on chain: " + obs.Error,
					orderTo: execution.OrderFailedFinal, attemptTo: execution.AttemptFailed, release: true, planTo: PlanFailed,
				}
			}
			return obs.Finality, obs.Finality.Valid(), nil
		}
		if rc.action.LastValidBlockHeight > 0 {
			height, err := r.d.Observer.GetBlockHeight(ctx)
			if err != nil {
				return "", false, err
			}
			if height > rc.action.LastValidBlockHeight+blockHeightMargin {
				return "", false, &stepFailure{
					code: errs.CodeVenueUnavailable, reason: "transaction expired: block height passed last valid block height",
					orderTo: execution.OrderExpired, attemptTo: execution.AttemptExpired, release: true, planTo: PlanFailed,
				}
			}
		}
		return "", false, nil
	}
	providerLevel := st.State.Finality()
	if !obs.Found {
		// The provider's word alone is never enough.
		return "", false, nil
	}
	if obs.Failed {
		return "", false, &stepFailure{
			code: errs.CodeReconciliationRequired, reason: "chain observer reports a failed transaction while the provider reports " + string(st.State),
			orderTo: execution.OrderReconciliationRequired, pause: true,
		}
	}
	if !providerLevel.Valid() || !obs.Finality.Valid() {
		return "", false, nil
	}
	level := providerLevel
	if obs.Finality.Rank() < level.Rank() {
		level = obs.Finality
	}
	return level, true, nil
}

// blockHeightMargin is the number of blocks past LastValidBlockHeight before
// absence is considered proven by observation (EXECUTION.md §4 step 7).
const blockHeightMargin = 32

// advanceAttempt moves the attempt's finality forward, never backward.
func (r *runner) advanceAttempt(ctx context.Context, tx pgx.Tx, rc *runCtx, level execution.FinalityLevel, evidenceRef string) error {
	if !level.Valid() {
		return nil
	}
	var to execution.AttemptStatus
	patch := execution.AttemptPatch{Finality: finalityPtr(level), Reason: "observed " + string(level)}
	now := r.now()
	switch level {
	case execution.FinalityObserved:
		to = execution.AttemptObserved
		patch.ObservedAt = &now
	case execution.FinalityConfirmed:
		to = execution.AttemptConfirmed
		patch.ObservedAt, patch.ConfirmedAt = &now, &now
	case execution.FinalityFinalized:
		to = execution.AttemptFinalized
		patch.ObservedAt, patch.ConfirmedAt, patch.FinalizedAt = &now, &now, &now
	default:
		return nil
	}
	if rc.attempt.Status == to || !execution.CanTransitionAttempt(rc.attempt.Status, to) {
		return nil
	}
	patch.Status = &to
	patch.SubmitResponseRef = nonEmpty(evidenceRef)
	a, err := r.d.Attempts.Update(ctx, tx, rc.attempt.ID, patch)
	if err != nil {
		return err
	}
	rc.attempt = &a
	return nil
}

// recordFill converts an external event into a fill row (platform fee and
// effective price computed from the plan) and records it idempotently.
func (r *runner) recordFill(ctx context.Context, tx pgx.Tx, rc *runCtx, evt execution.ExternalExecutionEvent, source execution.FillSource, level execution.FinalityLevel, rawRef string) (execution.Fill, error) {
	f, err := r.fillFromEvent(rc, evt, source, level, rawRef)
	if err != nil {
		return execution.Fill{}, err
	}
	recorded, err := r.d.Orders.RecordFill(ctx, tx, f)
	if err != nil {
		if errs.HasCode(err, errs.CodeReconciliationRequired) {
			return execution.Fill{}, &stepFailure{code: errs.CodeReconciliationRequired, reason: err.Error(), orderTo: execution.OrderReconciliationRequired, pause: true}
		}
		return execution.Fill{}, err
	}
	if !recorded.Existing {
		o, err := r.d.Orders.Get(ctx, querierOf(tx), rc.order.ID)
		if err != nil {
			return execution.Fill{}, err
		}
		rc.order = &o
	}
	return recorded, nil
}

func (r *runner) fillFromEvent(rc *runCtx, evt execution.ExternalExecutionEvent, source execution.FillSource, level execution.FinalityLevel, rawRef string) (execution.Fill, error) {
	hc := rc.plan.HardConstraints
	if evt.InputAsset.IsZero() {
		evt.InputAsset = hc.InputAsset.ID
	}
	if evt.OutputAsset.IsZero() {
		evt.OutputAsset = hc.OutputAsset.ID
	}
	if evt.Venue == "" {
		evt.Venue = hc.Venue
	}
	if evt.TxSignature == "" {
		evt.TxSignature = rc.signed.TxSignature
	}
	if evt.ExternalFillID == "" {
		evt.ExternalFillID = evt.TxSignature
	}
	if evt.Finality != "" && execution.FinalityLevel(evt.Finality).Valid() && execution.FinalityLevel(evt.Finality).Rank() < level.Rank() {
		level = execution.FinalityLevel(evt.Finality)
	}
	fp, err := FeePolicyFromRef(rc.plan.EstimatedCosts.FeePolicy)
	if err != nil {
		return execution.Fill{}, err
	}
	quoteLeg := evt.InputQty
	if hc.Side == execution.SideSell {
		quoteLeg = evt.OutputQty
	}
	bd, err := fp.Compute(quoteLeg)
	if err != nil {
		return execution.Fill{}, errs.Wrap(err, errs.CodeValidationFailed, "settlement: platform fee on fill")
	}
	var mantissa money.Quantity
	var scale int32
	if hc.Side == execution.SideBuy {
		mantissa, scale, err = execution.EffectivePrice(evt.OutputQty, hc.OutputAsset.Decimals, evt.InputQty, hc.InputAsset.Decimals)
	} else {
		mantissa, scale, err = execution.EffectivePrice(evt.InputQty, hc.InputAsset.Decimals, evt.OutputQty, hc.OutputAsset.Decimals)
	}
	if err != nil {
		return execution.Fill{}, err
	}
	var slot *int64
	if evt.Slot > 0 {
		s := int64(evt.Slot) // #nosec G115 -- slots fit int64
		slot = &s
	}
	observedAt := evt.ObservedAt
	if observedAt.IsZero() {
		observedAt = r.now()
	}
	if rawRef == "" {
		rawRef = evt.RawRef
	}
	f := execution.Fill{
		ID: execution.NewFillID(), OrderID: rc.order.ID, AccountID: rc.accountID, Venue: evt.Venue, ExternalFillID: evt.ExternalFillID,
		TxSignature: evt.TxSignature, Slot: slot, InputAssetID: evt.InputAsset, InputQuantity: evt.InputQty, OutputAssetID: evt.OutputAsset, OutputQuantity: evt.OutputQty,
		NetworkFeeQuantity: evt.NetworkFee, NetworkFeeAssetID: evt.NetworkFeeAsset, VenueFeeQuantity: evt.VenueFee,
		PlatformFeeQuantity: bd.PlatformFee, EffectivePriceMantissa: mantissa, EffectivePriceScale: scale,
		Source: source, Finality: level, ObservedAt: observedAt, RawRef: rawRef,
	}
	if rc.attempt != nil {
		f.AttemptID = rc.attempt.ID
	}
	if f.VenueFeeQuantity.IsPositive() {
		f.VenueFeeAssetID = quoteAssetOf(hc)
	}
	if f.PlatformFeeQuantity.IsPositive() {
		f.PlatformFeeAssetID = fp.FeeAsset
	}
	if f.NetworkFeeQuantity.IsPositive() && f.NetworkFeeAssetID.IsZero() {
		f.NetworkFeeAssetID = hc.NetworkFeeAsset.ID
	}
	return f, nil
}

func quoteAssetOf(hc HardConstraints) assets.AssetID {
	if hc.Side == execution.SideBuy {
		return hc.InputAsset.ID
	}
	return hc.OutputAsset.ID
}

// --- 14. RECONCILE ---------------------------------------------------------------

func (r *runner) reconcile(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[ReconcileStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := ReconcileOutput{FillIDs: []string{}}
	var events []execution.ExternalExecutionEvent
	return handler{
		effect: func(ctx context.Context) error {
			if rc.attempt == nil || rc.order == nil {
				return errs.New(errs.CodeInternal, "settlement: RECONCILE without an attempt")
			}
			sig := rc.externalRef.TxSignature
			if sig == "" {
				sig = rc.attempt.TxSignature
			}
			evts, err := r.d.Adapter.Reconcile(ctx, execution.ReconcileScope{
				WalletAddress: in.WalletAddress, Since: rc.attempt.CreatedAt, Until: r.now(), TxSignature: sig, Limit: 100,
			})
			if err != nil {
				return err
			}
			for _, e := range evts {
				if e.TxSignature == "" || e.TxSignature == sig {
					events = append(events, e)
				}
			}
			ev, err := execution.StoreEvidence(ctx, r.d.Archive, r.evidenceKey(rc, step, "reconcile"), events)
			if err != nil {
				return err
			}
			out.Events, out.EvidenceRef = len(events), ev.RawRef
			return nil
		},
		persist: func(ctx context.Context, tx pgx.Tx) error {
			level := rc.observed.Finality
			if !level.Valid() {
				level = execution.FinalityObserved
			}
			for _, e := range events {
				f, err := r.recordFill(ctx, tx, rc, e, execution.FillFromReconciliation, level, out.EvidenceRef)
				if err != nil {
					return err
				}
				out.FillIDs = append(out.FillIDs, f.ID.String())
			}
			fills, err := r.d.Orders.ListFills(ctx, querierOf(tx), rc.order.ID)
			if err != nil {
				return err
			}
			hc := rc.plan.HardConstraints
			totalIn, totalOut := zeroQty(), zeroQty()
			for _, f := range fills {
				totalIn, totalOut = totalIn.Add(f.InputQuantity), totalOut.Add(f.OutputQuantity)
			}
			out.TotalInput, out.TotalOutput = totalIn, totalOut
			switch {
			case len(fills) == 0:
				return &stepFailure{
					code: errs.CodeReconciliationRequired, reason: "transaction reached " + string(level) + " but no fill is known",
					orderTo: execution.OrderReconciliationRequired, pause: true,
				}
			case totalIn.Cmp(hc.MaxInputQuantity) > 0:
				return &stepFailure{
					code: errs.CodeReconciliationRequired, reason: "fills debited " + totalIn.String() + ", more than the reserved " + hc.MaxInputQuantity.String(),
					orderTo: execution.OrderReconciliationRequired, pause: true,
				}
			case hc.MinOutputQuantity.IsPositive() && rc.order.Remaining().IsZero() && totalOut.Cmp(hc.MinOutputQuantity) < 0:
				return &stepFailure{
					code: errs.CodeReconciliationRequired, reason: "fills delivered " + totalOut.String() + ", less than the minimum " + hc.MinOutputQuantity.String(),
					orderTo: execution.OrderReconciliationRequired, pause: true,
				}
			}
			out.Matched = true
			return nil
		},
		output: func() any { return out },
	}, nil
}

// --- 15. POST_LEDGER -------------------------------------------------------------

func (r *runner) postLedger(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[PostLedgerStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := PostLedgerOutput{Postings: []FillPosting{}}
	return handler{
		persist: func(ctx context.Context, tx pgx.Tx) error {
			if rc.order == nil {
				return errs.New(errs.CodeInternal, "settlement: POST_LEDGER without an order")
			}
			fills, err := r.d.Orders.ListFills(ctx, querierOf(tx), rc.order.ID)
			if err != nil {
				return err
			}
			required := in.RequiredFinality
			if !required.Valid() {
				required = execution.FinalityConfirmed
			}
			posted := 0
			for _, f := range fills {
				if !r.finalityPolicySatisfies(f.Finality, required) {
					return fmt.Errorf("settlement: fill %s is %s; ledger posting needs %s", f.ID, f.Finality, required)
				}
				if f.Posted() {
					out.Postings = append(out.Postings, FillPosting{FillID: f.ID.String(), TransactionID: f.JournalTransactionID, Existing: true})
					posted++
					continue
				}
				posting, err := r.swapPosting(rc, in, f)
				if err != nil {
					return err
				}
				res, err := r.d.Ledger.Post(ctx, tx, posting)
				if err != nil {
					return err
				}
				if err := r.d.Orders.MarkFillPosted(ctx, tx, f.ID, res.TransactionID.String()); err != nil {
					return err
				}
				if err := r.consumeForFill(ctx, tx, rc, in, f); err != nil {
					return err
				}
				out.Postings = append(out.Postings, FillPosting{FillID: f.ID.String(), TransactionID: res.TransactionID.String(), Existing: res.Existing})
				posted++
			}
			if posted == 0 {
				return &stepFailure{code: errs.CodeReconciliationRequired, reason: "no fill to post", orderTo: execution.OrderReconciliationRequired, pause: true}
			}
			if rc.order.Status == execution.OrderFilled || rc.order.Status == execution.OrderPartiallyFilled {
				o, err := r.d.Orders.Transition(ctx, tx, rc.order.ID, execution.OrderSettling, execution.TransitionEvidence{
					Reason: "fills posted to the ledger", ActorType: string(security.ActorSystem), ActorID: ExecutorActorID,
				})
				if err != nil {
					return err
				}
				rc.order = &o
			}
			return nil
		},
		output: func() any { return out },
	}, nil
}

// swapPosting builds the TRADE_FILL posting of a fill (FINANCIAL_MODEL §2.2).
func (r *runner) swapPosting(rc *runCtx, in PostLedgerStepInput, f execution.Fill) (ledger.Posting, error) {
	quoteDecimals := in.InputAsset.Decimals
	quoteQty := f.InputQuantity
	if in.Side == execution.SideSell {
		quoteDecimals, quoteQty = in.OutputAsset.Decimals, f.OutputQuantity
	}
	usd, err := money.QuoteQuantityToUSD(quoteQty, quoteDecimals, money.RoundHalfEven)
	if err != nil {
		return ledger.Posting{}, moneyErr(err)
	}
	feeUSD, err := money.QuoteQuantityToUSD(f.PlatformFeeQuantity, quoteDecimals, money.RoundHalfEven)
	if err != nil {
		return ledger.Posting{}, moneyErr(err)
	}
	priceRef := fmt.Sprintf("fill:%s@%s", f.ID, f.ObservedAt.UTC().Format(time.RFC3339Nano))
	return ledger.SwapPosting(ledger.SwapInputs{
		AccountID: rc.accountID, FillID: f.ID.String(),
		OutAsset: f.InputAssetID, OutQuantity: f.InputQuantity, InAsset: f.OutputAssetID, InQuantity: f.OutputQuantity,
		NetworkFeeAsset: f.NetworkFeeAssetID, NetworkFeeQuantity: f.NetworkFeeQuantity,
		PlatformFeeAsset: f.PlatformFeeAssetID, PlatformFeeQuantity: f.PlatformFeeQuantity,
		OutUSD: &usd, InUSD: &usd, PlatformFeeUSD: &feeUSD, OutPriceRef: priceRef, InPriceRef: priceRef,
		EffectiveAt: f.ObservedAt, CorrelationID: rc.intent.CorrelationID,
		Description: fmt.Sprintf("swap fill %s on %s (order %s)", f.ExternalFillID, f.Venue, rc.order.ID),
	})
}

// consumeForFill consumes the fill's input (and the platform fee when it is
// paid in the input asset) from the reservation, keyed by the fill through
// the posting's idempotency: a re-run finds the fill already posted and
// never reaches here.
func (r *runner) consumeForFill(ctx context.Context, tx pgx.Tx, rc *runCtx, in PostLedgerStepInput, f execution.Fill) error {
	if rc.reservationID == "" {
		return nil
	}
	rid, err := capital.ParseReservationID(rc.reservationID)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "settlement: reservation id")
	}
	qty := f.InputQuantity
	if f.PlatformFeeQuantity.IsPositive() && f.PlatformFeeAssetID == f.InputAssetID {
		qty = qty.Add(f.PlatformFeeQuantity)
	}
	res, err := r.d.Capital.Get(ctx, querierOf(tx), rid)
	if err != nil {
		return err
	}
	if qty.Cmp(res.Remaining()) > 0 {
		qty = res.Remaining()
	}
	quoteDecimals := in.InputAsset.Decimals
	quoteQty := f.InputQuantity
	if in.Side == execution.SideSell {
		quoteDecimals, quoteQty = in.OutputAsset.Decimals, f.OutputQuantity
	}
	usd, err := money.QuoteQuantityToUSD(quoteQty, quoteDecimals, money.RoundHalfEven)
	if err != nil {
		return moneyErr(err)
	}
	remainingUSD, err := res.RemainingUSD()
	if err != nil {
		return moneyErr(err)
	}
	if usd.Cmp(remainingUSD) > 0 {
		usd = remainingUSD
	}
	if !qty.IsPositive() && !usd.IsPositive() {
		return nil
	}
	_, err = r.d.Capital.Consume(ctx, tx, rid, qty, usd.Minor(), rc.order.ID.String())
	return err
}

// --- 16. UPDATE_POSITION --------------------------------------------------------

func (r *runner) updatePosition(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[UpdatePositionStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := UpdatePositionOutput{Applied: []string{}}
	return handler{
		persist: func(ctx context.Context, tx pgx.Tx) error {
			if rc.order == nil {
				return errs.New(errs.CodeInternal, "settlement: UPDATE_POSITION without an order")
			}
			fills, err := r.d.Orders.ListFills(ctx, querierOf(tx), rc.order.ID)
			if err != nil {
				return err
			}
			required := in.RequiredFinality
			if !required.Valid() {
				required = execution.FinalityConfirmed
			}
			var walletID id.ID[id.Any]
			if rc.intent.WalletID != "" {
				if w, err := id.ParseAny(rc.intent.WalletID); err == nil {
					walletID = w
				}
			}
			for _, f := range fills {
				if !f.Posted() {
					return fmt.Errorf("settlement: fill %s is not posted; positions follow the ledger", f.ID)
				}
				if !r.finalityPolicySatisfies(f.Finality, required) {
					return fmt.Errorf("settlement: fill %s is %s; position update needs %s", f.ID, f.Finality, required)
				}
				if f.PositionApplied() {
					out.Applied = append(out.Applied, f.ID.String())
					continue
				}
				journalID, err := id.ParseAny(f.JournalTransactionID)
				if err != nil {
					return errs.Wrap(err, errs.CodeInternal, "settlement: journal transaction id")
				}
				feesUSD, err := money.QuoteQuantityToUSD(f.PlatformFeeQuantity.Add(f.VenueFeeQuantity), in.QuoteAsset.Decimals, money.RoundHalfEven)
				if err != nil {
					return moneyErr(err)
				}
				ref := positions.Ref{Type: "fill", ID: f.ID.String()}
				if in.Side == execution.SideBuy {
					cost, err := money.QuoteQuantityToUSD(f.InputQuantity, in.QuoteAsset.Decimals, money.RoundHalfEven)
					if err != nil {
						return moneyErr(err)
					}
					if _, err := r.d.Positions.Acquire(ctx, tx, positions.AcquireLot{
						AccountID: rc.accountID, AssetID: in.BaseAsset.ID, Quantity: f.OutputQuantity, AcquiredAt: f.ObservedAt,
						Cost: cost, Fees: feesUSD, BasisSource: "fill", ValuationSource: in.ValuationSource, AcquisitionRef: ref,
						Venue: in.Venue, WalletID: walletID, JournalTxID: journalID,
					}); err != nil {
						return err
					}
				} else {
					proceeds, err := money.QuoteQuantityToUSD(f.OutputQuantity, in.QuoteAsset.Decimals, money.RoundHalfEven)
					if err != nil {
						return moneyErr(err)
					}
					if _, _, err := r.d.Positions.Dispose(ctx, tx, positions.Disposal{
						AccountID: rc.accountID, AssetID: in.BaseAsset.ID, Quantity: f.InputQuantity, DisposedAt: f.ObservedAt,
						Proceeds: proceeds, Fees: feesUSD, ValuationSource: in.ValuationSource, DispositionRef: ref, JournalTxID: journalID,
					}); err != nil {
						return err
					}
				}
				if f.NetworkFeeQuantity.IsPositive() && !f.NetworkFeeAssetID.IsZero() {
					// Network fee units left the wallet: dispose them from the
					// fee asset's lots when the account holds any, so lots and
					// the ledger agree. An account without lots in the fee
					// asset is recorded, never silently skipped.
					_, _, err := r.d.Positions.Dispose(ctx, tx, positions.Disposal{
						AccountID: rc.accountID, AssetID: f.NetworkFeeAssetID, Quantity: f.NetworkFeeQuantity, DisposedAt: f.ObservedAt,
						Proceeds: money.USD{}, Fees: money.USD{}, ValuationSource: in.ValuationSource,
						DispositionRef: positions.Ref{Type: "fill_network_fee", ID: f.ID.String()}, JournalTxID: journalID,
					})
					if err != nil {
						if !errs.HasCode(err, errs.CodeValidationFailed) {
							return err
						}
						out.Notes = append(out.Notes, "network fee of fill "+f.ID.String()+" not disposed: "+err.Error())
					}
				}
				if err := r.d.Orders.MarkPositionApplied(ctx, tx, f.ID, r.now()); err != nil {
					return err
				}
				out.Applied = append(out.Applied, f.ID.String())
			}
			return nil
		},
		output: func() any { return out },
	}, nil
}

// --- 17. RELEASE_RESERVATION ----------------------------------------------------

func (r *runner) releaseReservationStep(rc *runCtx, step Step) (handler, error) {
	in, err := DecodeStepInput[ReleaseStepInput](step)
	if err != nil {
		return handler{}, err
	}
	out := ReleaseOutput{ReservationID: rc.reservationID}
	return handler{
		persist: func(ctx context.Context, tx pgx.Tx) error {
			if rc.reservationID != "" {
				rid, err := capital.ParseReservationID(rc.reservationID)
				if err != nil {
					return errs.Wrap(err, errs.CodeInternal, "settlement: reservation id")
				}
				res, err := r.d.Capital.Get(ctx, querierOf(tx), rid)
				if err != nil {
					return err
				}
				if res.Status == capital.ReservationActive {
					if res.ConsumedQuantity.IsPositive() {
						res, err = r.d.Capital.ConsumeFinal(ctx, tx, rid, zeroQty(), 0, rc.orderID())
					} else {
						res, err = r.d.Capital.Release(ctx, tx, rid, in.Reason)
					}
					if err != nil {
						return err
					}
				}
				out.Status, out.Consumed, out.Released = string(res.Status), res.ConsumedQuantity, res.Quantity.Sub(res.ConsumedQuantity)
			}
			if rc.order != nil && rc.order.Status == execution.OrderSettling {
				o, err := r.d.Orders.Transition(ctx, tx, rc.order.ID, execution.OrderSettled, execution.TransitionEvidence{
					Reason: "positions applied and reservation settled", ActorType: string(security.ActorSystem), ActorID: ExecutorActorID,
				})
				if err != nil {
					return err
				}
				rc.order = &o
			}
			return nil
		},
		output: func() any { return out },
	}, nil
}

// --- helpers ------------------------------------------------------------------------

func (r *runner) evidenceKey(rc *runCtx, step Step, kind string) string {
	part := fmt.Sprintf("%02d-%s", step.Seq, strings.ToLower(string(step.Type)))
	if rc.attempt != nil {
		part += fmt.Sprintf("-a%d", rc.attempt.AttemptNo)
	}
	return execution.EvidenceKey("plan", rc.plan.ID.String(), part, kind)
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
