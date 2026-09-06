package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/security"
)

// DefaultMaxAttempts bounds how many attempts a plan may build after a
// submission was proven absent (PART 48 step 7).
const DefaultMaxAttempts = 2

// ExecutorActorID is the audit actor id of the executor.
const ExecutorActorID = "settlement-executor"

// Deps are the executor's collaborators. Every field except Sleep, Hooks
// and MaxAttempts is required; a nil dependency fails at construction, not
// on the first plan.
type Deps struct {
	DB           Transactor
	Plans        PlanStore
	Orders       OrderStore
	Attempts     AttemptStore
	Quotes       QuoteStore
	Intents      IntentReader
	Adapter      execution.ExecutionAdapter
	Observer     ChainObserver
	Capital      CapitalService
	Ledger       ledger.Poster
	Positions    positions.LotEngine
	KillSwitches KillSwitchChecker
	Risk         FinalRiskChecker
	Inspector    TransactionInspector
	Recoverer    Recoverer
	Audit        audit.Writer
	Archive      execution.ArchiveWriter
	Clock        clock.Clock
	Sleep        Sleeper
	Hooks        Hooks
	MaxAttempts  int
}

func (d Deps) validate() error {
	missing := []string{}
	add := func(name string, nilp bool) {
		if nilp {
			missing = append(missing, name)
		}
	}
	add("DB", d.DB == nil)
	add("Plans", d.Plans == nil)
	add("Orders", d.Orders == nil)
	add("Attempts", d.Attempts == nil)
	add("Quotes", d.Quotes == nil)
	add("Intents", d.Intents == nil)
	add("Adapter", d.Adapter == nil)
	add("Observer", d.Observer == nil)
	add("Capital", d.Capital == nil)
	add("Ledger", d.Ledger == nil)
	add("Positions", d.Positions == nil)
	add("KillSwitches", d.KillSwitches == nil)
	add("Risk", d.Risk == nil)
	add("Inspector", d.Inspector == nil)
	add("Recoverer", d.Recoverer == nil)
	add("Audit", d.Audit == nil)
	add("Archive", d.Archive == nil)
	if len(missing) > 0 {
		return errs.New(errs.CodeInternal, "settlement: executor dependencies missing").WithField("missing", missing)
	}
	return nil
}

// Executor runs approved plans for real: it holds a Signer.
type Executor struct {
	r *runner
}

// NewExecutor wires a live executor. The signer is mandatory here and
// absent from NewDryRunExecutor by construction.
func NewExecutor(d Deps, signer Signer) (*Executor, error) {
	if signer == nil {
		return nil, errs.New(errs.CodeInternal, "settlement: live executor requires a signer")
	}
	r, err := newRunner(d, signer, false)
	if err != nil {
		return nil, err
	}
	return &Executor{r: r}, nil
}

// Run executes (or resumes) a plan. It returns nil when the plan
// COMPLETED; RECONCILIATION_REQUIRED or SUBMISSION_STATE_UNKNOWN when the
// plan is paused on the operator path; the step's error otherwise. A plan
// with DryRun set stops after INSPECT_TRANSACTION even here.
func (e *Executor) Run(ctx context.Context, planID PlanID) (RunResult, error) {
	return e.r.run(ctx, planID)
}

// DryRunExecutor previews plans without money (PART 220). It has no signer
// field and no way to acquire one, so it can never call a signing service;
// it refuses plans whose DryRun flag is not set.
type DryRunExecutor struct {
	r *runner
}

// NewDryRunExecutor wires a dry-run executor. There is no signer parameter.
func NewDryRunExecutor(d Deps) (*DryRunExecutor, error) {
	r, err := newRunner(d, nil, true)
	if err != nil {
		return nil, err
	}
	return &DryRunExecutor{r: r}, nil
}

// Run previews a dry-run plan up to and including INSPECT_TRANSACTION.
func (e *DryRunExecutor) Run(ctx context.Context, planID PlanID) (RunResult, error) {
	return e.r.run(ctx, planID)
}

// RunResult summarizes a Run.
type RunResult struct {
	PlanID    PlanID
	Status    PlanStatus
	OrderID   string
	StoppedAt StepType
	Reason    string
	Steps     []Step
}

type runner struct {
	d          Deps
	signer     Signer
	dryRunOnly bool
}

func newRunner(d Deps, signer Signer, dryRunOnly bool) (*runner, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	if d.Clock == nil {
		d.Clock = clock.System()
	}
	if d.Sleep == nil {
		d.Sleep = defaultSleep
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = DefaultMaxAttempts
	}
	return &runner{d: d, signer: signer, dryRunOnly: dryRunOnly}, nil
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runCtx is the executor's working memory for one plan. Everything in it
// is restored from persisted step outputs on resume.
type runCtx struct {
	plan      Plan
	intent    IntentRef
	accountID accounts.AccountID
	dryRun    bool

	reservationID  string
	quoteID        string
	quote          execution.QuoteSnapshot
	order          *execution.Order
	riskDecisionID string
	attempt        *execution.Attempt
	action         execution.UnsignedAction
	signed         SignOutput
	externalRef    execution.ExternalReference
	observed       ObserveOutput
	submitAdopted  bool
}

func (rc *runCtx) orderID() string {
	if rc.order == nil {
		return ""
	}
	return rc.order.ID.String()
}

// stepFailure is a business failure of a step: the plan cannot continue and
// the executor compensates as described.
type stepFailure struct {
	code      errs.Code
	reason    string
	orderTo   execution.OrderStatus
	rejection string
	attemptTo execution.AttemptStatus
	release   bool
	planTo    PlanStatus
	fields    map[string]any
	// pause parks the plan instead of failing it: the step becomes UNKNOWN
	// and Run returns the paused code (RECONCILIATION_REQUIRED).
	pause bool
}

func (f *stepFailure) Error() string { return fmt.Sprintf("%s: %s", f.code, f.reason) }

func (f *stepFailure) errsError() *errs.Error {
	e := errs.New(f.code, f.reason)
	if f.fields != nil {
		e = e.WithFields(f.fields)
	}
	return e
}

// errPaused signals that the plan is parked on the operator path.
type errPaused struct {
	code   errs.Code
	reason string
}

func (e *errPaused) Error() string { return fmt.Sprintf("%s: %s", e.code, e.reason) }

func (r *runner) now() time.Time { return r.d.Clock.Now() }

// inTxMaxRetries absorbs deadlocks and serialization failures when several
// workers resume the same plan at once.
//
// Retrying here is safe because of how a step is structured: every external
// effect (Adapter.Quote/ValidateQuote/Build/Submit/Status/Reconcile and
// signer.Sign) runs in a step's `effect` closure, which takes only a context
// and executes OUTSIDE any transaction. Nothing passed to inTx performs an
// external effect, so re-running the closure cannot re-submit, re-sign or
// otherwise duplicate anything outside the database — it only repeats database
// work that the rollback already undid. If a future step ever performs an
// external effect inside inTx, this retry becomes a double-submission bug and
// must be removed along with it.
const inTxMaxRetries = 3

func (r *runner) inTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return r.d.DB.InTx(ctx, db.TxOptions{MaxRetries: inTxMaxRetries}, fn)
}

func (r *runner) read(ctx context.Context, fn func(ctx context.Context, q db.Querier) error) error {
	return r.d.DB.InTx(ctx, db.TxOptions{ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, querierOf(tx))
	})
}

// querierOf turns a (possibly nil, in fakes) transaction into a Querier.
func querierOf(tx pgx.Tx) db.Querier {
	if tx == nil {
		return nil
	}
	return tx
}

func (r *runner) hook(ctx context.Context, step Step, phase Phase) error {
	if r.d.Hooks.OnStep == nil {
		return nil
	}
	return r.d.Hooks.OnStep(ctx, step, phase)
}

// run is the durable loop (SETTLEMENT_COMPILER §5).
func (r *runner) run(ctx context.Context, planID PlanID) (RunResult, error) {
	rc, err := r.load(ctx, planID)
	if err != nil {
		return RunResult{PlanID: planID}, err
	}
	res := RunResult{PlanID: planID, Status: rc.plan.Status, OrderID: rc.orderID()}
	for {
		next, done, err := r.nextStep(rc)
		if err != nil {
			return r.finish(rc, res, err)
		}
		if done {
			return r.complete(ctx, rc, res)
		}
		step := next
		if step.Type == StepSubmit && (step.State == StepRunning || step.State == StepUnknown) {
			// A crash or a timeout left the submission's fate unknown. Never
			// re-submit: investigate (EXECUTION.md §4).
			if err := r.recoverSubmission(ctx, rc, step); err != nil {
				return r.finish(rc, res, err)
			}
			continue
		}
		if err := r.runStep(ctx, rc, step); err != nil {
			return r.finish(rc, res, err)
		}
		if rc.dryRun && step.Type.DryRunStop() {
			return r.stopDryRun(ctx, rc, res)
		}
	}
}

func (r *runner) finish(rc *runCtx, res RunResult, err error) (RunResult, error) {
	res.Status = rc.plan.Status
	res.OrderID = rc.orderID()
	res.Steps = rc.plan.Steps
	var paused *errPaused
	if errors.As(err, &paused) {
		res.Reason = paused.reason
		return res, errs.New(paused.code, paused.reason).WithField("plan_id", rc.plan.ID.String())
	}
	var sf *stepFailure
	if errors.As(err, &sf) {
		res.Reason = sf.reason
		return res, sf.errsError().WithField("plan_id", rc.plan.ID.String())
	}
	res.Reason = err.Error()
	return res, err
}

// load reads the plan, claims it (APPROVED → EXECUTING) and restores the
// working memory from persisted step outputs.
func (r *runner) load(ctx context.Context, planID PlanID) (*runCtx, error) {
	var plan Plan
	if err := r.read(ctx, func(ctx context.Context, q db.Querier) error {
		p, err := r.d.Plans.Get(ctx, q, planID)
		if err != nil {
			return err
		}
		plan = p
		return nil
	}); err != nil {
		return nil, err
	}
	if r.dryRunOnly && !plan.DryRun {
		return nil, errs.New(errs.CodeForbidden, "settlement: the dry-run executor only runs dry-run plans").WithField("plan_id", planID.String())
	}
	switch plan.Status {
	case PlanApproved:
		if err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			p, err := r.d.Plans.SetStatus(ctx, tx, planID, PlanApproved, PlanExecuting, "execution started")
			if err != nil {
				return err
			}
			plan = p
			return nil
		}); err != nil {
			return nil, err
		}
	case PlanExecuting:
	case PlanDraft:
		if !plan.DryRun {
			return nil, errs.New(errs.CodeInvalidStateTransition, "settlement: plan is DRAFT; approve it before execution").WithField("plan_id", planID.String())
		}
	default:
		return nil, errs.Newf(errs.CodeInvalidStateTransition, "settlement: plan is %s and cannot run", plan.Status).WithField("plan_id", planID.String())
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if ok, err := VerifyHash(plan); err != nil {
		return nil, err
	} else if !ok {
		return nil, errs.New(errs.CodeConflict, "settlement: persisted plan hash does not match its content").WithField("plan_id", planID.String())
	}
	rc := &runCtx{plan: plan, dryRun: plan.DryRun}
	if err := r.read(ctx, func(ctx context.Context, q db.Querier) error {
		in, err := r.d.Intents.Intent(ctx, q, plan.IntentID)
		if err != nil {
			return err
		}
		rc.intent = in
		acct, err := accounts.ParseAccountID(in.AccountID)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "settlement: intent account id")
		}
		rc.accountID = acct
		rc.reservationID = in.ReservationID
		if o, err := r.d.Orders.GetByPlan(ctx, q, plan.ID.String()); err == nil {
			rc.order = &o
		} else if !errs.HasCode(err, errs.CodeNotFound) {
			return err
		}
		if rc.order != nil {
			attempts, err := r.d.Attempts.ListForOrder(ctx, q, rc.order.ID)
			if err != nil {
				return err
			}
			if n := len(attempts); n > 0 {
				a := attempts[n-1]
				rc.attempt = &a
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if rc.plan.AccountID == "" {
		rc.plan.AccountID = rc.intent.AccountID
	}
	for _, s := range plan.Steps {
		if s.State.Done() {
			if err := r.restore(rc, s); err != nil {
				return nil, err
			}
		}
	}
	return rc, nil
}

// restore rebuilds working memory from one succeeded step's output.
func (r *runner) restore(rc *runCtx, s Step) error {
	if len(s.EvidenceOutput) == 0 {
		return nil
	}
	decode := func(v any) error {
		if err := json.Unmarshal(s.EvidenceOutput, v); err != nil {
			return errs.Wrap(err, errs.CodeInternal, "settlement: decode step output").WithField("step", string(s.Type))
		}
		return nil
	}
	switch s.Type {
	case StepReserveCapital:
		var o ReserveOutput
		if err := decode(&o); err != nil {
			return err
		}
		if o.ReservationID != "" {
			rc.reservationID = o.ReservationID
		}
	case StepAcquireQuote:
		var o QuoteOutput
		if err := decode(&o); err != nil {
			return err
		}
		rc.quote, rc.quoteID = o.Quote, o.QuoteID
	case StepFinalRiskCheck:
		var o FinalRiskOutput
		if err := decode(&o); err != nil {
			return err
		}
		rc.riskDecisionID = o.DecisionID
	case StepBuildTransaction:
		var o BuildOutput
		if err := decode(&o); err != nil {
			return err
		}
		rc.action = o.Action
	case StepRequestSignature:
		var o SignOutput
		if err := decode(&o); err != nil {
			return err
		}
		rc.signed = o
	case StepSubmit:
		var o SubmitOutput
		if err := decode(&o); err != nil {
			return err
		}
		rc.externalRef = o.ExternalRef
		rc.submitAdopted = o.Adopted
	case StepObserveFinality:
		var o ObserveOutput
		if err := decode(&o); err != nil {
			return err
		}
		rc.observed = o
	}
	return nil
}

// nextStep returns the first step that still needs work, checking that its
// dependencies are done. done is true when every step is done.
func (r *runner) nextStep(rc *runCtx) (Step, bool, error) {
	byID := map[StepID]Step{}
	for _, s := range rc.plan.Steps {
		byID[s.ID] = s
	}
	for _, s := range rc.plan.Steps {
		if s.State.Done() {
			continue
		}
		if s.State == StepFailed {
			return Step{}, false, errs.Newf(errs.CodeInvalidStateTransition, "settlement: step %s is FAILED; the plan cannot resume", s.Type).
				WithField("plan_id", rc.plan.ID.String()).WithField("step", string(s.Type))
		}
		if s.State == StepUnknown && s.Type != StepSubmit {
			return Step{}, false, &errPaused{code: errs.CodeReconciliationRequired, reason: "step " + string(s.Type) + " is UNKNOWN; reconciliation must resolve it"}
		}
		for _, dep := range s.DependsOn {
			d, ok := byID[dep]
			if !ok || !d.State.Done() {
				return Step{}, false, errs.Newf(errs.CodeInvalidStateTransition, "settlement: step %s has an unfinished dependency", s.Type).
					WithField("plan_id", rc.plan.ID.String())
			}
		}
		return s, false, nil
	}
	return Step{}, true, nil
}

func (r *runner) setStep(rc *runCtx, s Step) {
	for i := range rc.plan.Steps {
		if rc.plan.Steps[i].ID == s.ID {
			rc.plan.Steps[i] = s
			return
		}
	}
}

// runStep drives one step through prepare (in the RUNNING transaction),
// effect (outside any transaction), and persist (in the terminal
// transaction).
func (r *runner) runStep(ctx context.Context, rc *runCtx, step Step) error {
	h, err := r.handlerFor(rc, step)
	if err != nil {
		return err
	}
	class, guarded := step.Type.KillSwitchClass(rc.plan.HardConstraints.ActionClass)

	// 1. RUNNING is persisted before any side effect, together with the
	//    kill-switch check and the step's pre-effect writes.
	if err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if guarded && !rc.dryRun {
			if err := r.checkKillSwitch(ctx, tx, rc, class); err != nil {
				return err
			}
		}
		if h.prepare != nil {
			if err := h.prepare(ctx, tx); err != nil {
				return err
			}
		}
		s, err := r.d.Plans.MarkStep(ctx, tx, step.ID, step.State, StepRunning, StepOutcome{})
		if err != nil {
			return err
		}
		step = s
		return r.auditStep(ctx, tx, rc, step, StepRunning, "", nil)
	}); err != nil {
		var sf *stepFailure
		if errors.As(err, &sf) {
			return r.failStep(ctx, rc, step, sf)
		}
		return err
	}
	r.setStep(rc, step)
	if err := r.hook(ctx, step, PhaseBeforeEffect); err != nil {
		return err
	}

	// 2. The side effect, bounded by the step timeout.
	var effectErr error
	if h.effect != nil {
		ectx, cancel := context.WithTimeout(ctx, step.Timeout)
		effectErr = h.effect(ectx)
		cancel()
	}
	if err := r.hook(ctx, step, PhaseAfterEffect); err != nil {
		return err
	}
	if effectErr != nil {
		return r.handleEffectError(ctx, rc, step, effectErr)
	}

	// 3. The terminal state, together with the step's writes.
	if err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if h.persist != nil {
			if err := h.persist(ctx, tx); err != nil {
				return err
			}
		}
		var out any
		if h.output != nil {
			out = h.output()
		}
		s, err := r.d.Plans.MarkStep(ctx, tx, step.ID, StepRunning, StepSucceeded, StepOutcome{EvidenceOutput: out})
		if err != nil {
			return err
		}
		step = s
		return r.auditStep(ctx, tx, rc, step, StepSucceeded, "", out)
	}); err != nil {
		var sf *stepFailure
		if errors.As(err, &sf) {
			return r.failStep(ctx, rc, step, sf)
		}
		return err
	}
	r.setStep(rc, step)
	return r.hook(ctx, step, PhaseAfterPersist)
}

// handleEffectError classifies an effect error: unknown submission,
// business failure, or transient (the step stays RUNNING for a later Run).
func (r *runner) handleEffectError(ctx context.Context, rc *runCtx, step Step, err error) error {
	var sf *stepFailure
	if errors.As(err, &sf) {
		return r.failStep(ctx, rc, step, sf)
	}
	if step.Type == StepSubmit {
		return r.markSubmissionUnknown(ctx, rc, step, err)
	}
	// Transient: keep RUNNING so a later Run re-runs the step (SAFE_RETRY /
	// IDEMPOTENT_WRITE), but record the error.
	_ = r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return r.auditStep(ctx, tx, rc, step, StepRunning, "transient: "+err.Error(), nil)
	})
	return err
}

// checkKillSwitch consults the authoritative checker for the plan's
// dimensions. Never-blocked classes return without a query.
func (r *runner) checkKillSwitch(ctx context.Context, tx pgx.Tx, rc *runCtx, class killswitch.ActionClass) error {
	hc := rc.plan.HardConstraints
	a := killswitch.Action{
		Class: class, AccountID: rc.intent.AccountID, AgentID: rc.intent.AgentID, StrategyVersionID: rc.intent.StrategyVersionID,
		Venue: hc.Venue, InstrumentID: rc.intent.InstrumentID, Chain: hc.Chain, Provider: hc.Provider,
	}
	if err := r.d.KillSwitches.Check(ctx, querierOf(tx), a); err != nil {
		if errs.HasCode(err, errs.CodeKillSwitchActive) {
			return &stepFailure{
				code: errs.CodeKillSwitchActive, reason: err.Error(), orderTo: execution.OrderRejected, rejection: string(errs.CodeKillSwitchActive),
				release: true, planTo: PlanFailed,
			}
		}
		return err
	}
	return nil
}

// failStep persists FAILED and applies the compensation the failure names:
// order transition, attempt transition, reservation release, plan status.
func (r *runner) failStep(ctx context.Context, rc *runCtx, step Step, sf *stepFailure) error {
	to := StepFailed
	if sf.pause {
		to = StepUnknown
	}
	err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		s, err := r.d.Plans.MarkStep(ctx, tx, step.ID, step.State, to, StepOutcome{LastError: sf.Error()})
		if err != nil && !errs.HasCode(err, errs.CodeConflict) {
			return err
		}
		if err == nil {
			step = s
		}
		if err := r.compensate(ctx, tx, rc, sf); err != nil {
			return err
		}
		return r.auditStep(ctx, tx, rc, step, to, sf.Error(), map[string]any{"code": string(sf.code), "fields": sf.fields})
	})
	if err != nil {
		return err
	}
	r.setStep(rc, step)
	if sf.pause {
		return &errPaused{code: sf.code, reason: sf.reason}
	}
	return sf
}

func (r *runner) compensate(ctx context.Context, tx pgx.Tx, rc *runCtx, sf *stepFailure) error {
	if rc.attempt != nil && sf.attemptTo != "" && rc.attempt.Status != sf.attemptTo {
		to := sf.attemptTo
		a, err := r.d.Attempts.Update(ctx, tx, rc.attempt.ID, execution.AttemptPatch{Status: &to, Error: strPtr(sf.reason), Reason: sf.reason})
		if err != nil {
			return err
		}
		rc.attempt = &a
	}
	if rc.order != nil && sf.orderTo != "" && rc.order.Status != sf.orderTo {
		ev := execution.TransitionEvidence{Reason: sf.reason, RejectionCode: sf.rejection, ActorType: string(security.ActorSystem), ActorID: ExecutorActorID}
		o, err := r.d.Orders.Transition(ctx, tx, rc.order.ID, sf.orderTo, ev)
		if err != nil {
			return err
		}
		rc.order = &o
	}
	if sf.release && rc.reservationID != "" && !rc.dryRun {
		if err := r.releaseReservation(ctx, tx, rc, sf.reason); err != nil {
			return err
		}
	}
	if sf.planTo != "" && rc.plan.Status != sf.planTo && CanTransitionPlan(rc.plan.Status, sf.planTo) {
		p, err := r.d.Plans.SetStatus(ctx, tx, rc.plan.ID, rc.plan.Status, sf.planTo, sf.reason)
		if err != nil {
			return err
		}
		p.Steps = rc.plan.Steps
		p.AccountID = rc.plan.AccountID
		rc.plan = p
	}
	return nil
}

// releaseReservation returns an ACTIVE reservation with nothing consumed to
// the account; a partially consumed one is finalized so the consumed part
// stays deployed.
func (r *runner) releaseReservation(ctx context.Context, tx pgx.Tx, rc *runCtx, reason string) error {
	rid, err := capital.ParseReservationID(rc.reservationID)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "settlement: reservation id")
	}
	res, err := r.d.Capital.Get(ctx, querierOf(tx), rid)
	if err != nil {
		return err
	}
	if res.Status != capital.ReservationActive {
		return nil
	}
	if res.ConsumedQuantity.IsPositive() {
		_, err = r.d.Capital.ConsumeFinal(ctx, tx, rid, zeroQty(), 0, rc.orderID())
		return err
	}
	_, err = r.d.Capital.Release(ctx, tx, rid, reason)
	return err
}

// markSubmissionUnknown persists the timeout rule (PART 48): step UNKNOWN,
// attempt SUBMISSION_UNKNOWN, order SUBMISSION_UNKNOWN, reservation kept
// and locked; then hands off to the recovery path.
func (r *runner) markSubmissionUnknown(ctx context.Context, rc *runCtx, step Step, cause error) error {
	reason := "submission outcome unknown: " + cause.Error()
	if err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		s, err := r.d.Plans.MarkStep(ctx, tx, step.ID, step.State, StepUnknown, StepOutcome{LastError: reason})
		if err != nil {
			return err
		}
		step = s
		if err := r.transitionUnknown(ctx, tx, rc, reason); err != nil {
			return err
		}
		return r.auditStep(ctx, tx, rc, step, StepUnknown, reason, nil)
	}); err != nil {
		return err
	}
	r.setStep(rc, step)
	return r.recoverSubmission(ctx, rc, step)
}

func (r *runner) transitionUnknown(ctx context.Context, tx pgx.Tx, rc *runCtx, reason string) error {
	if rc.attempt != nil && rc.attempt.Status != execution.AttemptSubmissionUnknown && execution.CanTransitionAttempt(rc.attempt.Status, execution.AttemptSubmissionUnknown) {
		to := execution.AttemptSubmissionUnknown
		a, err := r.d.Attempts.Update(ctx, tx, rc.attempt.ID, execution.AttemptPatch{Status: &to, Error: strPtr(reason), Reason: reason})
		if err != nil {
			return err
		}
		rc.attempt = &a
	}
	if rc.order != nil && rc.order.Status != execution.OrderSubmissionUnknown && execution.CanTransition(rc.order.Status, execution.OrderSubmissionUnknown) {
		o, err := r.d.Orders.Transition(ctx, tx, rc.order.ID, execution.OrderSubmissionUnknown, execution.TransitionEvidence{
			Reason: reason, ActorType: string(security.ActorSystem), ActorID: ExecutorActorID,
		})
		if err != nil {
			return err
		}
		rc.order = &o
	}
	return nil
}

// recoverSubmission is the unknown-submission path for a SUBMIT step found
// RUNNING (crash) or UNKNOWN (timeout). It never re-submits.
func (r *runner) recoverSubmission(ctx context.Context, rc *runCtx, step Step) error {
	if rc.attempt == nil || rc.order == nil {
		return errs.New(errs.CodeInternal, "settlement: SUBMIT step without an attempt or order").WithField("plan_id", rc.plan.ID.String())
	}
	if step.State == StepRunning {
		reason := "process restarted while SUBMIT was running"
		if err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			s, err := r.d.Plans.MarkStep(ctx, tx, step.ID, StepRunning, StepUnknown, StepOutcome{LastError: reason})
			if err != nil {
				return err
			}
			step = s
			if err := r.transitionUnknown(ctx, tx, rc, reason); err != nil {
				return err
			}
			return r.auditStep(ctx, tx, rc, step, StepUnknown, reason, nil)
		}); err != nil {
			return err
		}
		r.setStep(rc, step)
	}
	if rc.signed.TxSignature == "" && rc.attempt.TxSignature != "" {
		rc.signed.TxSignature = rc.attempt.TxSignature
	}
	req := RecoveryRequest{
		PlanID: rc.plan.ID, PlanHash: rc.plan.Hash, OrderID: rc.order.ID, AttemptID: rc.attempt.ID, TxSignature: rc.signed.TxSignature,
		WalletAddress: rc.intent.WalletAddress, Provider: rc.plan.HardConstraints.Provider, Chain: rc.plan.HardConstraints.Chain,
		Since: rc.attempt.CreatedAt,
	}
	if rc.attempt.LastValidBlockHeight != nil && *rc.attempt.LastValidBlockHeight > 0 {
		req.LastValidBlockHeight = uint64(*rc.attempt.LastValidBlockHeight)
	}
	rctx, cancel := context.WithTimeout(ctx, step.Timeout)
	result, err := r.d.Recoverer.Recover(rctx, req)
	cancel()
	if err != nil {
		return err
	}
	switch result.Outcome {
	case RecoveryAdopted:
		return r.adoptSubmission(ctx, rc, step, result)
	case RecoveryProvenAbsent:
		return r.provenAbsent(ctx, rc, step, result)
	default:
		reason := "submission unresolved: " + result.Reason
		if err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if rc.order.Status != execution.OrderReconciliationRequired && execution.CanTransition(rc.order.Status, execution.OrderReconciliationRequired) {
				o, err := r.d.Orders.Transition(ctx, tx, rc.order.ID, execution.OrderReconciliationRequired, execution.TransitionEvidence{
					Reason: reason, EvidenceRef: result.EvidenceRef, ActorType: string(security.ActorSystem), ActorID: ExecutorActorID,
				})
				if err != nil {
					return err
				}
				rc.order = &o
			}
			return r.auditStep(ctx, tx, rc, step, StepUnknown, reason, map[string]any{"recovery": string(result.Outcome), "evidence_ref": result.EvidenceRef})
		}); err != nil {
			return err
		}
		return &errPaused{code: errs.CodeReconciliationRequired, reason: reason}
	}
}

// adoptSubmission records that the transaction exists: attempt ADOPTED,
// order SUBMITTED, SUBMIT SUCCEEDED with the recovery evidence.
func (r *runner) adoptSubmission(ctx context.Context, rc *runCtx, step Step, result RecoveryResult) error {
	ref := result.ExternalRef
	if ref.TxSignature == "" {
		ref.TxSignature = rc.signed.TxSignature
	}
	if ref.Venue == "" {
		ref.Venue = rc.plan.HardConstraints.Venue
	}
	out := SubmitOutput{ExternalRef: ref, AcceptedAt: r.now(), EvidenceRef: result.EvidenceRef, Adopted: true, Recovery: string(result.Outcome)}
	if err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if rc.attempt.Status != execution.AttemptAdopted {
			to := execution.AttemptAdopted
			now := r.now()
			a, err := r.d.Attempts.Update(ctx, tx, rc.attempt.ID, execution.AttemptPatch{
				Status: &to, SubmittedAt: &now, SubmitResponseRef: strPtr(result.EvidenceRef), Reason: "adopted: " + result.Reason,
				Finality: finalityPtr(execution.FinalitySubmitted),
			})
			if err != nil {
				return err
			}
			rc.attempt = &a
		}
		if rc.order.Status != execution.OrderSubmitted && execution.CanTransition(rc.order.Status, execution.OrderSubmitted) {
			o, err := r.d.Orders.Transition(ctx, tx, rc.order.ID, execution.OrderSubmitted, execution.TransitionEvidence{
				Reason: "submission adopted after recovery", EvidenceRef: result.EvidenceRef, ActorType: string(security.ActorSystem), ActorID: ExecutorActorID,
			})
			if err != nil {
				return err
			}
			rc.order = &o
		}
		s, err := r.d.Plans.MarkStep(ctx, tx, step.ID, step.State, StepSucceeded, StepOutcome{EvidenceOutput: out})
		if err != nil {
			return err
		}
		step = s
		return r.auditStep(ctx, tx, rc, step, StepSucceeded, "adopted", out)
	}); err != nil {
		return err
	}
	rc.externalRef = ref
	rc.submitAdopted = true
	r.setStep(rc, step)
	return nil
}

// provenAbsent expires the attempt and, within the attempt budget, resets
// the plan from ACQUIRE_QUOTE for a fresh attempt after a fresh FINAL risk
// check; beyond the budget the order expires and the reservation is
// released.
func (r *runner) provenAbsent(ctx context.Context, rc *runCtx, step Step, result RecoveryResult) error {
	reason := "submission proven absent: " + result.Reason
	retry := int(step.Attempts) < r.d.MaxAttempts
	err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if execution.CanTransitionAttempt(rc.attempt.Status, execution.AttemptExpired) {
			to := execution.AttemptExpired
			a, err := r.d.Attempts.Update(ctx, tx, rc.attempt.ID, execution.AttemptPatch{Status: &to, Error: strPtr(reason), Reason: reason})
			if err != nil {
				return err
			}
			rc.attempt = &a
		}
		if retry {
			o, err := r.d.Orders.Transition(ctx, tx, rc.order.ID, execution.OrderSubmitting, execution.TransitionEvidence{
				Reason: reason + "; building a new attempt", EvidenceRef: result.EvidenceRef, ActorType: string(security.ActorSystem), ActorID: ExecutorActorID,
			})
			if err != nil {
				return err
			}
			rc.order = &o
			from, _ := rc.plan.StepByType(StepAcquireQuote)
			if err := r.d.Plans.ResetSteps(ctx, tx, rc.plan.ID, from.Seq); err != nil {
				return err
			}
			return r.auditStep(ctx, tx, rc, step, StepPending, reason, map[string]any{"recovery": string(result.Outcome), "evidence_ref": result.EvidenceRef})
		}
		s, err := r.d.Plans.MarkStep(ctx, tx, step.ID, step.State, StepFailed, StepOutcome{LastError: reason})
		if err != nil {
			return err
		}
		step = s
		sf := &stepFailure{code: errs.CodeSubmissionStateUnknown, reason: reason, orderTo: execution.OrderExpired, release: true, planTo: PlanFailed}
		if err := r.compensate(ctx, tx, rc, sf); err != nil {
			return err
		}
		return r.auditStep(ctx, tx, rc, step, StepFailed, reason, map[string]any{"recovery": string(result.Outcome)})
	})
	if err != nil {
		return err
	}
	if !retry {
		r.setStep(rc, step)
		return &stepFailure{code: errs.CodeSubmissionStateUnknown, reason: reason + "; attempt budget exhausted"}
	}
	// Reload the plan so the reset steps are seen; the working memory of
	// the reset steps is cleared.
	var plan Plan
	if err := r.read(ctx, func(ctx context.Context, q db.Querier) error {
		p, err := r.d.Plans.Get(ctx, q, rc.plan.ID)
		if err != nil {
			return err
		}
		plan = p
		return nil
	}); err != nil {
		return err
	}
	plan.AccountID = rc.plan.AccountID
	rc.plan = plan
	rc.quote, rc.quoteID = execution.QuoteSnapshot{}, ""
	rc.riskDecisionID = ""
	rc.action = execution.UnsignedAction{}
	rc.signed = SignOutput{}
	rc.externalRef = execution.ExternalReference{}
	rc.submitAdopted = false
	return nil
}

// stopDryRun ends a dry-run after INSPECT_TRANSACTION: remaining steps are
// SKIPPED and the plan is COMPLETED. Nothing was reserved, ordered, signed
// or submitted.
func (r *runner) stopDryRun(ctx context.Context, rc *runCtx, res RunResult) (RunResult, error) {
	err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for i, s := range rc.plan.Steps {
			if s.State == StepPending {
				marked, err := r.d.Plans.MarkStep(ctx, tx, s.ID, StepPending, StepSkipped, StepOutcome{EvidenceOutput: map[string]any{"dry_run": true}})
				if err != nil {
					return err
				}
				rc.plan.Steps[i] = marked
			}
		}
		if rc.plan.Status == PlanDraft {
			// A DRAFT dry-run preview leaves the plan DRAFT: it is not
			// finished; it was previewed.
			return r.auditPlan(ctx, tx, rc, AuditPlanStatus, "dry-run preview completed", map[string]any{"dry_run": true})
		}
		p, err := r.d.Plans.SetStatus(ctx, tx, rc.plan.ID, rc.plan.Status, PlanCompleted, "dry-run completed after INSPECT_TRANSACTION")
		if err != nil {
			return err
		}
		p.Steps = rc.plan.Steps
		p.AccountID = rc.plan.AccountID
		rc.plan = p
		return nil
	})
	res.Status = rc.plan.Status
	res.StoppedAt = StepInspectTransaction
	res.Steps = rc.plan.Steps
	res.Reason = "dry-run stopped after INSPECT_TRANSACTION"
	return res, err
}

// complete marks the plan COMPLETED once every step is done.
func (r *runner) complete(ctx context.Context, rc *runCtx, res RunResult) (RunResult, error) {
	if rc.plan.Status == PlanExecuting {
		if err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			p, err := r.d.Plans.SetStatus(ctx, tx, rc.plan.ID, PlanExecuting, PlanCompleted, "all steps succeeded")
			if err != nil {
				return err
			}
			p.Steps = rc.plan.Steps
			p.AccountID = rc.plan.AccountID
			rc.plan = p
			return nil
		}); err != nil {
			return r.finish(rc, res, err)
		}
	}
	res.Status = rc.plan.Status
	res.OrderID = rc.orderID()
	res.Steps = rc.plan.Steps
	return res, nil
}

// auditStep appends the per-step audit event on the account stream.
func (r *runner) auditStep(ctx context.Context, tx pgx.Tx, rc *runCtx, step Step, state StepState, reason string, payload any) error {
	body := map[string]any{
		"plan_id": rc.plan.ID.String(), "plan_hash": rc.plan.Hash, "intent_id": rc.plan.IntentID, "step": string(step.Type), "seq": step.Seq,
		"state": string(state), "attempts": step.Attempts, "semantic_key": step.SemanticIdempotencyKey, "dry_run": rc.dryRun,
		"order_id": rc.orderID(), "reservation_id": rc.reservationID, "output": payload,
	}
	if rc.attempt != nil {
		body["attempt_id"] = rc.attempt.ID.String()
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "settlement: encode step audit payload")
	}
	_, err = r.d.Audit.Append(ctx, tx, audit.Event{
		Stream: audit.AccountStream(rc.intent.AccountID), ActorType: string(security.ActorSystem), ActorID: ExecutorActorID,
		Action: AuditStepPrefix + string(step.Type), ResourceType: "execution_plan_step", ResourceID: step.ID.String(),
		Reason: reason, CorrelationID: rc.intent.CorrelationID, PolicyVersion: rc.plan.PlannerVersion, Payload: raw, OccurredAt: r.now(),
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "settlement: append step audit event")
	}
	return nil
}

func (r *runner) auditPlan(ctx context.Context, tx pgx.Tx, rc *runCtx, action, reason string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "settlement: encode plan audit payload")
	}
	_, err = r.d.Audit.Append(ctx, tx, audit.Event{
		Stream: audit.AccountStream(rc.intent.AccountID), ActorType: string(security.ActorSystem), ActorID: ExecutorActorID,
		Action: action, ResourceType: "execution_plan", ResourceID: rc.plan.ID.String(), Reason: reason,
		CorrelationID: rc.intent.CorrelationID, Payload: raw, OccurredAt: r.now(),
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "settlement: append plan audit event")
	}
	return nil
}

func strPtr(s string) *string { return &s }

func finalityPtr(l execution.FinalityLevel) *execution.FinalityLevel { return &l }
