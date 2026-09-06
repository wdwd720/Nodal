package settlement_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/settlement/settlementtest"
)

func newLive(t *testing.T, w *settlementtest.World) *settlement.Executor {
	t.Helper()
	ex, err := settlement.NewExecutor(w.Deps(), w.Signer)
	require.NoError(t, err)
	return ex
}

// assertExactlyOnce is the economic-effect invariant every executor test
// ends with: one submission landed, one fill, one ledger posting, one
// position update, reservation fully settled.
func assertExactlyOnce(t *testing.T, w *settlementtest.World, plan settlement.PlanID) {
	t.Helper()
	require.Equal(t, 1, w.Chain.Landed(), "exactly one transaction landed")
	require.Equal(t, 1, w.Adapter.Submissions, "exactly one submission")
	require.Equal(t, 1, w.Orders.FillCount(), "exactly one fill")
	require.Equal(t, 1, w.Ledger.Postings(), "exactly one ledger posting")
	require.Equal(t, 1, w.Positions.FillApplications(), "exactly one position update")
	res := w.Capital.All()
	require.Len(t, res, 1, "exactly one reservation")
	require.NotEqual(t, capital.ReservationActive, res[0].Status, "reservation settled")
	require.True(t, res[0].ConsumedQuantity.IsPositive(), "reservation consumed by the fill")
	o, ok := w.Order(plan)
	require.True(t, ok)
	require.Equal(t, execution.OrderSettled, o.Status)
	p := w.Plan(t, plan)
	require.Equal(t, settlement.PlanCompleted, p.Status)
	for _, s := range p.Steps {
		require.Equal(t, settlement.StepSucceeded, s.State, "step %s", s.Type)
	}
	fills, err := w.Orders.ListFills(context.Background(), nil, o.ID)
	require.NoError(t, err)
	require.Len(t, fills, 1)
	require.True(t, fills[0].Posted())
	require.True(t, fills[0].PositionApplied())
	require.NotEmpty(t, w.Audit.Events("account:"+w.AccountID.String()), "audit events on the account stream")
}

func TestExecutor_BuyHappyPath(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	res, err := ex.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, res.Status)
	assertExactlyOnce(t, w, plan.ID)
	require.Equal(t, 1, w.Signer.CallCount())
	require.Equal(t, 1, w.Inspector.Calls)
	require.Equal(t, 1, w.Risk.Calls, "FINAL risk check ran once")
	// The buy acquired a SOL lot for the fill output (less the network fee
	// paid in SOL, which is disposed from the same lot).
	oid, err := execution.ParseOrderID(res.OrderID)
	require.NoError(t, err)
	fills, _ := w.Orders.ListFills(context.Background(), nil, oid)
	require.Equal(t, 1, w.Positions.Acquires)
	require.True(t, w.Positions.OpenQuantity(w.SOL.ID).Equal(fills[0].OutputQuantity.Sub(fills[0].NetworkFeeQuantity)))
	// The platform fee is an explicit, separate deduction (PART 126).
	require.True(t, fills[0].PlatformFeeQuantity.IsPositive())
	require.Equal(t, w.USDC.ID, fills[0].PlatformFeeAssetID)
}

func TestExecutor_SellHappyPath(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	// Seed the open SOL lot the sell disposes of.
	seedSOLLot(t, w)
	w.Positions.Acquires = 0
	plan := w.PlanAndApprove(t, w.SellInput())
	require.Equal(t, execution.SideSell, plan.HardConstraints.Side)
	ex := newLive(t, w)
	res, err := ex.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, res.Status)
	assertExactlyOnce(t, w, plan.ID)
	require.Equal(t, 0, w.Positions.Acquires, "a sell acquires nothing")
}

func TestExecutor_RunIsIdempotentAfterCompletion(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	_, err := ex.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	_, err = ex.Run(context.Background(), plan.ID)
	require.Error(t, err, "a COMPLETED plan cannot run again")
	require.True(t, errs.HasCode(err, errs.CodeInvalidStateTransition))
	assertExactlyOnce(t, w, plan.ID)
}

func TestExecutor_DraftPlanRefused(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan, err := w.Planner.Plan(w.Input())
	require.NoError(t, err)
	plan.AccountID = w.AccountID.String()
	w.Plans.Put(plan)
	w.Intents.Put(w.IntentRef())
	ex := newLive(t, w)
	_, err = ex.Run(context.Background(), plan.ID)
	require.True(t, errs.HasCode(err, errs.CodeInvalidStateTransition))
	require.Equal(t, 0, w.Adapter.Submissions)
}

func TestExecutor_TamperedPlanHashRefused(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan := w.PlanAndApprove(t, w.Input())
	plan.HardConstraints.MaxSlippageBPS = 9_999 // loosen after approval
	w.Plans.Put(plan)
	ex := newLive(t, w)
	_, err := ex.Run(context.Background(), plan.ID)
	require.True(t, errs.HasCode(err, errs.CodeConflict), "hash mismatch: %v", err)
	require.Equal(t, 0, w.Adapter.Submissions)
}

// --- timeout rule and recovery (PART 48) -----------------------------------

func TestExecutor_SubmitTimeoutLanded_AdoptedNoDuplicate(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Adapter.SubmitScript = []settlementtest.SubmitKind{settlementtest.SubmitTimeoutLanded}
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	res, err := ex.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, res.Status)
	assertExactlyOnce(t, w, plan.ID)
	require.Equal(t, 1, w.Adapter.Calls[execution.MethodSubmit], "never re-submitted")
	require.Equal(t, 1, w.Recoverer.Calls)
	attempts := w.Attempts.All()
	require.Len(t, attempts, 1)
	require.Equal(t, execution.AttemptConfirmed, attempts[0].Status, "adopted attempt reaches the observed finality")
	// The order passed through SUBMISSION_UNKNOWN.
	var sawUnknown bool
	for _, tr := range w.Orders.Transitions {
		if tr.To == execution.OrderSubmissionUnknown {
			sawUnknown = true
		}
	}
	require.True(t, sawUnknown)
}

func TestExecutor_SubmitTimeoutLost_ProvenAbsent_NewAttemptOnce(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Adapter.SubmitScript = []settlementtest.SubmitKind{settlementtest.SubmitTimeoutLost, settlementtest.SubmitOK}
	// The investigation proves absence: both observers miss the signature and
	// the chain height passed the last valid block height.
	w.Recoverer.Force = settlement.RecoveryProvenAbsent
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	res, err := ex.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, res.Status)
	assertExactlyOnce(t, w, plan.ID)
	require.Equal(t, 2, w.Adapter.Calls[execution.MethodSubmit], "one submission per attempt")
	attempts := w.Attempts.All()
	require.Len(t, attempts, 2)
	require.Equal(t, execution.AttemptExpired, attempts[0].Status)
	require.Equal(t, int32(1), attempts[0].AttemptNo)
	require.Equal(t, int32(2), attempts[1].AttemptNo)
	require.Equal(t, 2, w.Risk.Calls, "fresh FINAL risk check before the new attempt")
	require.Equal(t, 2, w.Signer.CallCount())
}

func TestExecutor_SubmitTimeoutLost_BudgetExhausted_OrderExpiresReservationReleased(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Adapter.SubmitScript = []settlementtest.SubmitKind{settlementtest.SubmitTimeoutLost, settlementtest.SubmitTimeoutLost, settlementtest.SubmitTimeoutLost}
	w.Recoverer.Force = settlement.RecoveryProvenAbsent
	plan := w.PlanAndApprove(t, w.Input())
	deps := w.Deps()
	deps.MaxAttempts = 2
	ex, err := settlement.NewExecutor(deps, w.Signer)
	require.NoError(t, err)
	_, err = ex.Run(context.Background(), plan.ID)
	require.Error(t, err)
	require.True(t, errs.HasCode(err, errs.CodeSubmissionStateUnknown), "%v", err)
	require.Equal(t, 2, w.Adapter.Calls[execution.MethodSubmit])
	require.Equal(t, 0, w.Chain.Landed())
	o, ok := w.Order(plan.ID)
	require.True(t, ok)
	require.Equal(t, execution.OrderExpired, o.Status)
	res := w.Capital.All()
	require.Len(t, res, 1)
	require.Equal(t, capital.ReservationReleased, res[0].Status)
	require.Equal(t, settlement.PlanFailed, w.Plan(t, plan.ID).Status)
}

func TestExecutor_SubmitTimeout_Unresolved_ReconciliationRequired(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Adapter.SubmitScript = []settlementtest.SubmitKind{settlementtest.SubmitTimeoutLost}
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	_, err := ex.Run(context.Background(), plan.ID)
	require.Error(t, err)
	require.True(t, errs.HasCode(err, errs.CodeReconciliationRequired), "%v", err)
	o, ok := w.Order(plan.ID)
	require.True(t, ok)
	require.Equal(t, execution.OrderReconciliationRequired, o.Status)
	res := w.Capital.All()
	require.Len(t, res, 1)
	require.Equal(t, capital.ReservationActive, res[0].Status, "reservation kept")
	require.Equal(t, o.ID.String(), res[0].LockedByOrderID, "reservation locked to the order")
	require.Equal(t, settlement.PlanExecuting, w.Plan(t, plan.ID).Status, "plan parked, not failed")
	require.Equal(t, 1, w.Adapter.Calls[execution.MethodSubmit], "no duplicate submission")

	// A second Run while still unresolved stays parked and never re-submits.
	_, err = ex.Run(context.Background(), plan.ID)
	require.True(t, errs.HasCode(err, errs.CodeReconciliationRequired), "%v", err)
	require.Equal(t, 1, w.Adapter.Calls[execution.MethodSubmit])

	// External truth is discovered later (the transaction did land): the next
	// Run adopts it and the plan completes with exactly one economic effect.
	att := w.Attempts.All()[0]
	w.Chain.Land(att.TxSignature, []execution.ExternalExecutionEvent{{
		Venue: "jupiter", ExternalFillID: "fill-" + att.TxSignature, TxSignature: att.TxSignature,
		InputAsset: o.InputAssetID, OutputAsset: o.OutputAssetID, InputQty: o.InputQuantity, OutputQty: o.MinOutputQuantity,
		NetworkFee: money.QuantityFromInt64(5000), NetworkFeeAsset: w.SOL.ID, ObservedAt: w.Clock.Now(), Finality: string(execution.FinalityConfirmed),
	}})
	result, err := ex.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, result.Status)
	require.Equal(t, 1, w.Chain.Landed())
	require.Equal(t, 1, w.Orders.FillCount())
	require.Equal(t, 1, w.Ledger.Postings())
	require.Equal(t, 1, w.Positions.FillApplications())
	require.Equal(t, 1, w.Adapter.Calls[execution.MethodSubmit], "still exactly one submission")
	require.NotEqual(t, capital.ReservationActive, w.Capital.All()[0].Status)
}

func TestExecutor_SubmitRejectedButLanded_IsUnknownNotFailure(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Adapter.SubmitScript = []settlementtest.SubmitKind{settlementtest.SubmitRejectLanded}
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	res, err := ex.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, res.Status)
	assertExactlyOnce(t, w, plan.ID)
}

func TestExecutor_SubmitDefinitiveRejection_FailsAndReleases(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Adapter.SubmitScript = []settlementtest.SubmitKind{settlementtest.SubmitReject}
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	_, err := ex.Run(context.Background(), plan.ID)
	require.Error(t, err)
	require.Equal(t, 0, w.Chain.Landed())
	o, _ := w.Order(plan.ID)
	require.Equal(t, execution.OrderRejected, o.Status)
	require.Equal(t, capital.ReservationReleased, w.Capital.All()[0].Status)
	require.Equal(t, execution.AttemptFailed, w.Attempts.All()[0].Status)
}

func TestExecutor_FailedOnChain_OrderFailedFinal(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Adapter.SubmitScript = []settlementtest.SubmitKind{settlementtest.SubmitOKFailedOnChain}
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	_, err := ex.Run(context.Background(), plan.ID)
	require.Error(t, err)
	o, _ := w.Order(plan.ID)
	require.Equal(t, execution.OrderFailedFinal, o.Status)
	require.Equal(t, 0, w.Orders.FillCount())
	require.Equal(t, 0, w.Ledger.Postings())
	require.Equal(t, capital.ReservationReleased, w.Capital.All()[0].Status)
}

func TestExecutor_ObserverDisagreesOnFailure_Paused(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan := w.PlanAndApprove(t, w.Input())
	// Provider will report the landed tx; make the observer see it as failed
	// by failing it on chain after submit... simulate by a blind observer:
	// the provider says CONFIRMED but the observer never finds it, so no
	// finality is ever established and the step times out (transient).
	w.Observer.Blind = true
	deps := w.Deps()
	polls := 0
	deps.Sleep = func(context.Context, time.Duration) error {
		polls++
		if polls > 5 {
			return context.DeadlineExceeded
		}
		return nil
	}
	ex, err := settlement.NewExecutor(deps, w.Signer)
	require.NoError(t, err)
	_, err = ex.Run(context.Background(), plan.ID)
	require.Error(t, err)
	p := w.Plan(t, plan.ID)
	st, _ := p.StepByType(settlement.StepObserveFinality)
	require.Equal(t, settlement.StepRunning, st.State, "observation never adopts the provider's optimistic answer alone; the step stays retryable")
	require.Equal(t, 0, w.Ledger.Postings())
	require.Equal(t, settlement.PlanExecuting, p.Status)
}

// --- pre-submission rejections --------------------------------------------------

func TestExecutor_InspectionRejected(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Inspector.Reject = true
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	_, err := ex.Run(context.Background(), plan.ID)
	require.True(t, errs.HasCode(err, errs.CodeSigningRejected), "%v", err)
	require.Equal(t, 0, w.Signer.CallCount(), "rejected transactions never reach the signer")
	require.Equal(t, 0, w.Adapter.Submissions)
	o, _ := w.Order(plan.ID)
	require.Equal(t, execution.OrderRejected, o.Status)
	require.Equal(t, execution.AttemptInspectionRejected, w.Attempts.All()[0].Status)
	require.Equal(t, capital.ReservationReleased, w.Capital.All()[0].Status)
}

func TestExecutor_SigningRejected(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Signer.Reject = true
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	_, err := ex.Run(context.Background(), plan.ID)
	require.True(t, errs.HasCode(err, errs.CodeSigningRejected), "%v", err)
	require.Equal(t, 0, w.Adapter.Submissions)
	require.Equal(t, execution.AttemptSigningRejected, w.Attempts.All()[0].Status)
	require.Equal(t, capital.ReservationReleased, w.Capital.All()[0].Status)
}

func TestExecutor_FinalRiskRejected(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Risk.Verdict = "REJECT"
	w.Risk.ReasonCodes = []string{"RISK_QUOTE_AGE"}
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	_, err := ex.Run(context.Background(), plan.ID)
	require.True(t, errs.HasCode(err, errs.CodeNoValidPlan), "%v", err)
	require.Equal(t, 0, w.Adapter.Calls[execution.MethodBuild])
	o, _ := w.Order(plan.ID)
	require.Equal(t, execution.OrderRejected, o.Status)
	require.Equal(t, capital.ReservationReleased, w.Capital.All()[0].Status)
}

func TestExecutor_QuoteExceedsPriceImpact(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Adapter.QuotePriceImpactBPS = 500 // plan allows 100
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	_, err := ex.Run(context.Background(), plan.ID)
	require.Error(t, err)
	require.Equal(t, 0, w.Adapter.Submissions)
	_, hasOrder := w.Order(plan.ID)
	require.False(t, hasOrder, "no order before the quote is validated")
	require.Equal(t, capital.ReservationReleased, w.Capital.All()[0].Status)
	require.Equal(t, settlement.PlanFailed, w.Plan(t, plan.ID).Status)
}

func TestExecutor_WalletLacksSettlementAsset(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	w.Chain.SetBalance(w.WalletAddress, w.USDC.MintAddress, money.QuantityFromInt64(1))
	plan := w.PlanAndApprove(t, w.Input())
	ex := newLive(t, w)
	_, err := ex.Run(context.Background(), plan.ID)
	require.True(t, errs.HasCode(err, errs.CodeInsufficientBuyingPower), "%v", err)
	require.Equal(t, 0, w.Adapter.Calls[execution.MethodQuote], "no quote for money on the wrong rail")
}

// --- kill switches ----------------------------------------------------------------

func TestExecutor_KillSwitchMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		kind    killswitch.Kind
		scope   func(w *settlementtest.World) string
		blocked bool
	}{
		{"global", killswitch.GlobalNewRiskKill, func(*settlementtest.World) string { return "*" }, true},
		{"account", killswitch.AccountFreeze, func(w *settlementtest.World) string { return w.AccountID.String() }, true},
		{"venue", killswitch.VenueDisable, func(*settlementtest.World) string { return "JUPITER" }, true},
		{"provider", killswitch.ProviderDisableNewActions, func(*settlementtest.World) string { return "jupiter" }, true},
		{"chain", killswitch.ChainDisableNewActions, func(*settlementtest.World) string { return "solana-devnet" }, true},
		{"instrument_halt", killswitch.InstrumentHalt, func(w *settlementtest.World) string { return w.Instrument.ID.String() }, true},
		{"instrument_close_only", killswitch.InstrumentCloseOnly, func(w *settlementtest.World) string { return w.Instrument.ID.String() }, true},
		{"other_account", killswitch.AccountFreeze, func(*settlementtest.World) string { return "00000000-0000-7000-8000-000000000009" }, false},
		{"other_venue", killswitch.VenueDisable, func(*settlementtest.World) string { return "RAYDIUM" }, false},
		{"withdrawals", killswitch.WithdrawalsDisable, func(*settlementtest.World) string { return "*" }, false},
		{"funding", killswitch.FundingDisable, func(*settlementtest.World) string { return "*" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := settlementtest.NewWorld()
			plan := w.PlanAndApprove(t, w.Input())
			w.KillSwitches.Activate(tc.kind, tc.scope(w))
			ex := newLive(t, w)
			_, err := ex.Run(context.Background(), plan.ID)
			if !tc.blocked {
				require.NoError(t, err)
				assertExactlyOnce(t, w, plan.ID)
				return
			}
			require.True(t, errs.HasCode(err, errs.CodeKillSwitchActive), "%v", err)
			require.Equal(t, 0, w.Adapter.Submissions)
			require.Equal(t, 0, w.Signer.CallCount())
			require.Equal(t, settlement.PlanFailed, w.Plan(t, plan.ID).Status)
			for _, r := range w.Capital.All() {
				require.NotEqual(t, capital.ReservationActive, r.Status, "reservation released on kill")
			}
		})
	}
}

func TestExecutor_KillSwitchNeverBlocksPostSubmission(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan := w.PlanAndApprove(t, w.Input())
	// Crash right after the submission landed, then activate a GLOBAL kill
	// and resume: OBSERVE_FINALITY, RECONCILE, POST_LEDGER, UPDATE_POSITION
	// and RELEASE_RESERVATION must all run.
	deps := w.Deps()
	deps.Hooks = crashAt(settlement.StepSubmit, settlement.PhaseAfterPersist)
	ex, err := settlement.NewExecutor(deps, w.Signer)
	require.NoError(t, err)
	_, err = ex.Run(context.Background(), plan.ID)
	require.ErrorIs(t, err, errCrash)
	require.Equal(t, 1, w.Chain.Landed())

	w.KillSwitches.Activate(killswitch.GlobalNewRiskKill, "*")
	w.KillSwitches.Activate(killswitch.AccountFreeze, w.AccountID.String())
	checksBefore := len(w.KillSwitches.Checks)
	ex2 := newLive(t, w)
	res, err := ex2.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, res.Status)
	assertExactlyOnce(t, w, plan.ID)
	// Every post-submission check carried a never-blocked class.
	require.Greater(t, len(w.KillSwitches.Checks), checksBefore, "post-submission steps declare their class")
	for _, c := range w.KillSwitches.Checks[checksBefore:] {
		if c.Class == killswitch.NewRisk || c.Class == killswitch.ReduceRisk {
			t.Fatalf("new-risk class checked after submission: %+v", c)
		}
	}
}

func TestExecutor_KillSwitchActivatedBetweenReserveAndSubmit(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan := w.PlanAndApprove(t, w.Input())
	deps := w.Deps()
	deps.Hooks = settlement.Hooks{OnStep: func(_ context.Context, s settlement.Step, ph settlement.Phase) error {
		if s.Type == settlement.StepRequestSignature && ph == settlement.PhaseAfterPersist {
			w.KillSwitches.Activate(killswitch.GlobalNewRiskKill, "*")
		}
		return nil
	}}
	ex, err := settlement.NewExecutor(deps, w.Signer)
	require.NoError(t, err)
	_, err = ex.Run(context.Background(), plan.ID)
	require.True(t, errs.HasCode(err, errs.CodeKillSwitchActive), "%v", err)
	require.Equal(t, 0, w.Adapter.Submissions, "signed but never submitted")
	require.Equal(t, capital.ReservationReleased, w.Capital.All()[0].Status)
	o, _ := w.Order(plan.ID)
	require.Equal(t, execution.OrderRejected, o.Status)
	require.Equal(t, string(errs.CodeKillSwitchActive), o.RejectionCode)
}

// --- dry run (PART 220) -------------------------------------------------------------

func TestDryRunExecutor_NeverSignsOrSubmits(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan := w.PlanDryRun(t, w.Input())
	ex, err := settlement.NewDryRunExecutor(w.Deps())
	require.NoError(t, err)
	res, err := ex.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, res.Status)
	require.Equal(t, settlement.StepInspectTransaction, res.StoppedAt)
	require.Equal(t, 0, w.Signer.CallCount(), "the signer fake is never called")
	require.Equal(t, 0, w.Adapter.Calls[execution.MethodSubmit])
	require.Equal(t, 0, w.Chain.Landed())
	require.Equal(t, 0, w.Capital.Reserves, "dry run reserves nothing")
	require.Empty(t, w.Orders.Orders(), "dry run creates no order")
	require.Empty(t, w.Attempts.All(), "dry run creates no attempt")
	require.Equal(t, 1, w.Inspector.Calls, "dry run inspects the built transaction")
	require.Equal(t, 1, w.Adapter.Calls[execution.MethodQuote])
	p := w.Plan(t, plan.ID)
	for _, s := range p.Steps {
		switch {
		case s.Seq <= 9:
			require.Equal(t, settlement.StepSucceeded, s.State, "step %s", s.Type)
		default:
			require.Equal(t, settlement.StepSkipped, s.State, "step %s", s.Type)
		}
	}
}

func TestDryRunExecutor_RefusesLivePlan(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan := w.PlanAndApprove(t, w.Input())
	ex, err := settlement.NewDryRunExecutor(w.Deps())
	require.NoError(t, err)
	_, err = ex.Run(context.Background(), plan.ID)
	require.True(t, errs.HasCode(err, errs.CodeForbidden))
	require.Equal(t, 0, w.Adapter.Calls[execution.MethodQuote])
}

func TestLiveExecutor_HonoursDryRunFlag(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan := w.PlanDryRun(t, w.Input())
	ex := newLive(t, w)
	res, err := ex.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.StepInspectTransaction, res.StoppedAt)
	require.Equal(t, 0, w.Signer.CallCount())
	require.Equal(t, 0, w.Adapter.Submissions)
}

func TestNewExecutor_RequiresSigner(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	_, err := settlement.NewExecutor(w.Deps(), nil)
	require.Error(t, err)
	d := w.Deps()
	d.Ledger = nil
	_, err = settlement.NewExecutor(d, w.Signer)
	require.Error(t, err, "missing dependencies fail at construction")
}

// --- resume after crash at every step boundary ------------------------------------

var errCrash = errors.New("simulated crash")

func crashAt(step settlement.StepType, phase settlement.Phase) settlement.Hooks {
	fired := false
	return settlement.Hooks{OnStep: func(_ context.Context, s settlement.Step, ph settlement.Phase) error {
		if !fired && s.Type == step && ph == phase {
			fired = true
			return errCrash
		}
		return nil
	}}
}

func TestExecutor_ResumeAfterCrash_EveryStepBoundary(t *testing.T) {
	t.Parallel()
	phases := []settlement.Phase{settlement.PhaseBeforeEffect, settlement.PhaseAfterEffect, settlement.PhaseAfterPersist}
	for _, step := range settlement.V1StepSequence() {
		for _, phase := range phases {
			name := fmt.Sprintf("%s/%s", step, phase)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				w := settlementtest.NewWorld()
				plan := w.PlanAndApprove(t, w.Input())
				deps := w.Deps()
				deps.Hooks = crashAt(step, phase)
				ex, err := settlement.NewExecutor(deps, w.Signer)
				require.NoError(t, err)
				_, err = ex.Run(context.Background(), plan.ID)
				require.ErrorIs(t, err, errCrash, "the crash must fire")
				before := w.Plan(t, plan.ID)
				require.Equal(t, settlement.PlanExecuting, before.Status)
				if step == settlement.StepSubmit && w.Chain.Landed() == 0 {
					// The signed transaction never left the process; by the
					// restart the blockhash window has passed, so recovery
					// proves absence and a fresh attempt is built.
					w.Chain.SetHeight(w.Chain.Height() + 1000)
				}

				// Restart: a fresh executor over the same durable state.
				ex2 := newLive(t, w)
				res, err := ex2.Run(context.Background(), plan.ID)
				require.NoError(t, err, "resume must complete")
				require.Equal(t, settlement.PlanCompleted, res.Status)
				assertExactlyOnce(t, w, plan.ID)
				require.LessOrEqual(t, w.Adapter.Calls[execution.MethodSubmit], 1, "never more than one Submit call")
			})
		}
	}
}

func TestExecutor_ResumeAfterCrash_SubmitRunningNeverResubmits(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan := w.PlanAndApprove(t, w.Input())
	deps := w.Deps()
	deps.Hooks = crashAt(settlement.StepSubmit, settlement.PhaseAfterEffect)
	ex, err := settlement.NewExecutor(deps, w.Signer)
	require.NoError(t, err)
	_, err = ex.Run(context.Background(), plan.ID)
	require.ErrorIs(t, err, errCrash)
	p := w.Plan(t, plan.ID)
	st, _ := p.StepByType(settlement.StepSubmit)
	require.Equal(t, settlement.StepRunning, st.State, "crash left SUBMIT RUNNING with the transaction landed")
	require.Equal(t, 1, w.Chain.Landed())

	ex2 := newLive(t, w)
	res, err := ex2.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, res.Status)
	require.Equal(t, 1, w.Adapter.Calls[execution.MethodSubmit], "resume never re-submits")
	require.Equal(t, 1, w.Recoverer.Calls, "resume investigates")
	require.Equal(t, execution.AttemptConfirmed, w.Attempts.All()[0].Status)
	assertExactlyOnce(t, w, plan.ID)
}

func TestExecutor_ResumeAfterCrash_SubmitRunningTxLost(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	// The crash happens before the effect: nothing landed, and the chain
	// then moves past the last valid block height.
	plan := w.PlanAndApprove(t, w.Input())
	deps := w.Deps()
	deps.Hooks = crashAt(settlement.StepSubmit, settlement.PhaseBeforeEffect)
	ex, err := settlement.NewExecutor(deps, w.Signer)
	require.NoError(t, err)
	_, err = ex.Run(context.Background(), plan.ID)
	require.ErrorIs(t, err, errCrash)
	require.Equal(t, 0, w.Chain.Landed())
	w.Chain.SetHeight(w.Chain.Height() + 1000)

	ex2 := newLive(t, w)
	res, err := ex2.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, res.Status)
	assertExactlyOnce(t, w, plan.ID)
	require.Len(t, w.Attempts.All(), 2, "proven absent → one fresh attempt")
	require.Equal(t, 1, w.Adapter.Calls[execution.MethodSubmit], "the lost attempt was never submitted; the new one once")
}

// --- fill during cancel (PART 227) -------------------------------------------------

func TestExecutor_FillWinsOverCancelRequest(t *testing.T) {
	t.Parallel()
	w := settlementtest.NewWorld()
	plan := w.PlanAndApprove(t, w.Input())
	deps := w.Deps()
	deps.Hooks = settlement.Hooks{OnStep: func(_ context.Context, s settlement.Step, ph settlement.Phase) error {
		if s.Type == settlement.StepObserveFinality && ph == settlement.PhaseBeforeEffect {
			o, _ := w.Order(plan.ID)
			_, err := w.Orders.Transition(context.Background(), nil, o.ID, execution.OrderCancelRequested, execution.TransitionEvidence{Reason: "user cancel"})
			return err
		}
		return nil
	}}
	ex, err := settlement.NewExecutor(deps, w.Signer)
	require.NoError(t, err)
	res, err := ex.Run(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, res.Status)
	assertExactlyOnce(t, w, plan.ID)
	var sawCancelRequested, fillAfterCancel bool
	for _, tr := range w.Orders.Transitions {
		if tr.To == execution.OrderCancelRequested {
			sawCancelRequested = true
		}
		if sawCancelRequested && tr.From == execution.OrderCancelRequested && tr.To == execution.OrderFilled {
			fillAfterCancel = true
		}
	}
	require.True(t, sawCancelRequested)
	require.True(t, fillAfterCancel, "the fill won: CANCEL_REQUESTED -> FILLED")
}

// --- helpers --------------------------------------------------------------------------

func seedSOLLot(t *testing.T, w *settlementtest.World) {
	t.Helper()
	_, err := w.Positions.Acquire(context.Background(), nil, positions.AcquireLot{
		AccountID: w.AccountID, AssetID: w.SOL.ID, Quantity: money.QuantityFromInt64(2_000_000_000), AcquiredAt: w.Clock.Now().Add(-time.Hour),
		Cost: money.USDFromMinor(28_000), BasisSource: "funding", ValuationSource: "seed", AcquisitionRef: positions.Ref{Type: "deposit", ID: "seed"},
	})
	require.NoError(t, err)
}
