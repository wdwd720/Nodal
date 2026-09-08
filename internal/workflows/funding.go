package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// FundingInput starts a funding workflow. Only the deposit id is authority;
// everything else is scheduling policy, and the zero value takes the defaults
// below.
type FundingInput struct {
	DepositID string `json:"deposit_id"`
	// CorrelationID travels into the activity logs and audit events.
	CorrelationID string `json:"correlation_id,omitempty"`
	// PollInterval is the wait between Advance calls while the deposit is in
	// a provider-owned state. A signal wakes the workflow earlier.
	PollInterval time.Duration `json:"poll_interval,omitempty"`
	// ReviewAfter is how long a deposit may sit in one non-terminal state
	// before an operator is told. Zero takes DefaultFundingReviewAfter.
	ReviewAfter time.Duration `json:"review_after,omitempty"`
	// Deadline is the absolute cutoff after which the workflow stops driving
	// the deposit and hands it to an operator. Zero takes
	// DefaultFundingDeadline measured from the workflow start.
	Deadline time.Duration `json:"deadline,omitempty"`
	// Polls counts the Advance calls made by earlier runs of this workflow
	// chain; ContinueAsNew carries it forward. Callers leave it zero.
	Polls int `json:"polls,omitempty"`
	// EscalatedAt records that an earlier run already told an operator, so a
	// continued run does not tell them again. Callers leave it zero.
	Escalated bool `json:"escalated,omitempty"`
	// StartedAt is the chain's original start, carried across ContinueAsNew
	// so the deadline is measured from the first run and not from the latest.
	// Callers leave it zero and the workflow stamps it from workflow.Now.
	StartedAt time.Time `json:"started_at,omitempty"`
	// LastStatus is the status the previous run ended on, carried across
	// ContinueAsNew so a status change is still detectable. Callers leave it
	// empty.
	LastStatus string `json:"last_status,omitempty"`
	// StatusSince is when LastStatus was first observed. Callers leave it
	// zero.
	StatusSince time.Time `json:"status_since,omitempty"`
}

// Funding workflow defaults.
const (
	DefaultFundingPollInterval = 30 * time.Second
	DefaultFundingReviewAfter  = 2 * time.Hour
	DefaultFundingDeadline     = 72 * time.Hour
	// fundingPollsPerRun bounds one execution's history before it continues
	// as new. Temporal histories are capped, and a deposit that waits days
	// would otherwise grow one without limit.
	fundingPollsPerRun = 200
)

// FundingResult is what the workflow returns.
type FundingResult struct {
	DepositID string `json:"deposit_id"`
	Status    string `json:"status"`
	Terminal  bool   `json:"terminal"`
	Polls     int    `json:"polls"`
	Escalated bool   `json:"escalated"`
	// TimedOut is true when the workflow stopped at its deadline rather than
	// because the deposit reached a terminal status. It is not a verdict
	// about the money: the deposit row still says what is true.
	TimedOut bool `json:"timed_out"`
}

// FundingWorkflow drives one deposit to a terminal status (PART 28, PART 115).
//
// The loop is deliberately dumb: ask internal/funding to advance the deposit
// one step, look at the status it reports, then wait — for the poll interval,
// for a provider signal, or for the deadline, whichever comes first. All the
// state-machine knowledge lives in internal/funding, where it is covered by
// its own tests and, more importantly, by the database constraints. The
// workflow contributes durability and patience, nothing else.
//
// It never decides that a deposit succeeded or failed. Only the deposit row
// says that, and the workflow reports what the row said (PART 116).
func FundingWorkflow(ctx workflow.Context, in FundingInput) (FundingResult, error) {
	wlog := workflow.GetLogger(ctx)
	if in.DepositID == "" {
		return FundingResult{}, temporal.NewNonRetryableApplicationError(
			"funding workflow requires a deposit id", string(codeValidationFailed), nil,
		)
	}
	in = in.withDefaults(workflow.Now(ctx).UTC())

	actx := activityCtx(ctx)
	signals := workflow.GetSignalChannel(ctx, DepositUpdatedSignal)
	deadline := in.StartedAt.Add(in.Deadline)

	for {
		var state DepositState
		if err := workflow.ExecuteActivity(actx, AdvanceDepositName, AdvanceDepositInput{
			DepositID: in.DepositID, CorrelationID: in.CorrelationID,
		}).Get(ctx, &state); err != nil {
			return in.result(state.Status, false), err
		}
		in.Polls++
		now := workflow.Now(ctx).UTC()
		if state.Status != in.LastStatus {
			in.LastStatus, in.StatusSince = state.Status, now
		}
		wlog.Debug("funding workflow advanced the deposit",
			"deposit_id", in.DepositID, "status", state.Status, "terminal", state.Terminal, "polls", in.Polls)

		if state.Terminal {
			return in.result(state.Status, true), nil
		}

		// A deposit stuck in one non-terminal state is an operator's problem
		// long before it is a customer's. Tell them once, and keep driving.
		if !in.Escalated && now.Sub(in.StatusSince) >= in.ReviewAfter {
			if err := workflow.ExecuteActivity(actx, EscalateDepositName, EscalateDepositInput{
				DepositID: in.DepositID, Status: state.Status, StuckFor: now.Sub(in.StatusSince),
				CorrelationID: in.CorrelationID,
			}).Get(ctx, nil); err != nil {
				return in.result(state.Status, false), err
			}
			in.Escalated = true
		}

		if !now.Before(deadline) {
			res := in.result(state.Status, false)
			res.TimedOut = true
			wlog.Warn("funding workflow reached its deadline with the deposit still open",
				"deposit_id", in.DepositID, "status", state.Status)
			return res, nil
		}

		// Bound the history: continue as new with the accumulated counters.
		if in.Polls%fundingPollsPerRun == 0 {
			return FundingResult{}, workflow.NewContinueAsNewError(ctx, FundingWorkflowName, in)
		}

		if err := waitForUpdate(ctx, signals, in.PollInterval); err != nil {
			return in.result(state.Status, false), err
		}
	}
}

// waitForUpdate blocks until the poll interval elapses or a provider signal
// arrives, whichever is first. Draining the channel keeps a burst of signals
// from causing a burst of Advance calls.
func waitForUpdate(ctx workflow.Context, signals workflow.ReceiveChannel, d time.Duration) error {
	timerCtx, cancel := workflow.WithCancel(ctx)
	defer cancel()
	timer := workflow.NewTimer(timerCtx, d)

	var woken bool
	sel := workflow.NewSelector(ctx)
	sel.AddFuture(timer, func(workflow.Future) {})
	sel.AddReceive(signals, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)
		woken = true
	})
	sel.Select(ctx)
	if woken {
		// Collapse any signals already queued behind the first.
		for signals.ReceiveAsync(nil) {
		}
	}
	return ctx.Err()
}

func (in FundingInput) withDefaults(now time.Time) FundingInput {
	if in.PollInterval <= 0 {
		in.PollInterval = DefaultFundingPollInterval
	}
	if in.ReviewAfter <= 0 {
		in.ReviewAfter = DefaultFundingReviewAfter
	}
	if in.Deadline <= 0 {
		in.Deadline = DefaultFundingDeadline
	}
	if in.StartedAt.IsZero() {
		in.StartedAt = now
	}
	if in.StatusSince.IsZero() {
		in.StatusSince = now
	}
	return in
}

func (in FundingInput) result(status string, terminal bool) FundingResult {
	return FundingResult{
		DepositID: in.DepositID, Status: status, Terminal: terminal,
		Polls: in.Polls, Escalated: in.Escalated,
	}
}
