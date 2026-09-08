package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// EscalationInput starts an escalation for one reconciliation record.
type EscalationInput struct {
	RecordID      string `json:"record_id"`
	CorrelationID string `json:"correlation_id,omitempty"`
	// Tiers overrides the default ladder. It is carried in the input rather
	// than read from configuration so that a running workflow's ladder cannot
	// change under it between replays.
	Tiers []EscalationTier `json:"tiers,omitempty"`
	// PollInterval is how often the record is re-read while waiting inside a
	// tier. A resolution signal wakes the workflow earlier.
	PollInterval time.Duration `json:"poll_interval,omitempty"`
}

// EscalationTier is one rung of the ladder: tell these people at this
// severity, then wait this long for the record to be resolved.
type EscalationTier struct {
	Name     string        `json:"name"`
	Severity string        `json:"severity"`
	Wait     time.Duration `json:"wait"`
}

// Escalation defaults.
const (
	DefaultEscalationPollInterval = time.Minute
)

// DefaultEscalationTiers is the PART 163 ladder: the on-call operator first,
// then the finance and risk owners, then the incident channel. It is a
// function rather than a package variable so no caller can mutate the
// defaults another workflow will replay against.
func DefaultEscalationTiers() []EscalationTier {
	return []EscalationTier{
		{Name: "OPERATIONS", Severity: SeverityWarning, Wait: 15 * time.Minute},
		{Name: "FINANCE_RISK", Severity: SeverityCritical, Wait: 45 * time.Minute},
		{Name: "INCIDENT", Severity: SeverityCritical, Wait: 2 * time.Hour},
	}
}

// EscalationResult is what the workflow returns.
type EscalationResult struct {
	RecordID string `json:"record_id"`
	Status   string `json:"status"`
	// Resolved is true when a human or an automatic rule closed the record
	// before the ladder ran out.
	Resolved bool `json:"resolved"`
	// Escalated is true when the ladder ran out and the record was marked
	// ESCALATED.
	Escalated bool `json:"escalated"`
	// TiersNotified counts the rungs actually used.
	TiersNotified int `json:"tiers_notified"`
	// ContainmentRef names the kill switch raised, if any.
	ContainmentRef string `json:"containment_ref,omitempty"`
}

// ReconciliationEscalationWorkflow carries one reconciliation record up the
// notification ladder until somebody resolves it (PARTS 51, 163).
//
// It marks the record INVESTIGATING once, then walks the tiers: notify, wait,
// re-read. A resolution — by an operator, or by an automatic rule that
// matched the record — ends the workflow at any point. If the ladder runs out
// the record is marked ESCALATED, and a record that is both material and
// blocks new risk additionally triggers containment.
//
// Containment stops NEW RISK and nothing else. Reconciliation itself keeps
// running: the workflow goes on polling the record after raising the switch,
// because the one thing worse than an unreconciled position is an
// unreconciled position nobody is looking at any more (PART 52).
func ReconciliationEscalationWorkflow(ctx workflow.Context, in EscalationInput) (EscalationResult, error) {
	wlog := workflow.GetLogger(ctx)
	if in.RecordID == "" {
		return EscalationResult{}, temporal.NewNonRetryableApplicationError(
			"escalation workflow requires a record id", string(codeValidationFailed), nil,
		)
	}
	if len(in.Tiers) == 0 {
		in.Tiers = DefaultEscalationTiers()
	}
	if in.PollInterval <= 0 {
		in.PollInterval = DefaultEscalationPollInterval
	}

	actx := activityCtx(ctx)
	signals := workflow.GetSignalChannel(ctx, RecordResolvedSignal)
	res := EscalationResult{RecordID: in.RecordID}

	state, err := describeRecord(ctx, actx, in)
	if err != nil {
		return res, err
	}
	res.Status = state.Status
	if state.Resolved {
		res.Resolved = true
		return res, nil
	}

	if err := workflow.ExecuteActivity(actx, MarkInvestigatingName, MarkRecordInput{
		RecordID: in.RecordID, Reason: "escalation workflow opened", CorrelationID: in.CorrelationID,
	}).Get(ctx, nil); err != nil {
		return res, err
	}

	for _, tier := range in.Tiers {
		if err := workflow.ExecuteActivity(actx, NotifyEscalationName, EscalationNotice{
			RecordID: in.RecordID, Tier: tier.Name, Severity: tier.Severity, Status: state.Status,
			Kind: state.Kind, Material: state.Material, BlocksNewRisk: state.BlocksNewRisk,
			AccountID: state.AccountID, CorrelationID: in.CorrelationID,
		}).Get(ctx, nil); err != nil {
			return res, err
		}
		res.TiersNotified++

		resolved, st, err := waitForResolution(ctx, actx, in, signals, tier.Wait)
		if err != nil {
			return res, err
		}
		state, res.Status = st, st.Status
		if resolved {
			res.Resolved = true
			wlog.Info("reconciliation record resolved during escalation",
				"record_id", in.RecordID, "tier", tier.Name, "status", st.Status)
			return res, nil
		}
	}

	if err := workflow.ExecuteActivity(actx, MarkEscalatedName, MarkRecordInput{
		RecordID: in.RecordID, Reason: "escalation ladder exhausted without resolution", CorrelationID: in.CorrelationID,
	}).Get(ctx, nil); err != nil {
		return res, err
	}
	res.Escalated = true

	// Containment is the last resort and is deliberately narrow.
	if state.Material && state.BlocksNewRisk {
		var ref string
		if err := workflow.ExecuteActivity(actx, RequestContainmentName, ContainmentRequest{
			RecordID: in.RecordID, AccountID: state.AccountID, Kind: state.Kind,
			Reason: "unresolved material reconciliation mismatch", CorrelationID: in.CorrelationID,
		}).Get(ctx, &ref); err != nil {
			return res, err
		}
		res.ContainmentRef = ref
		wlog.Warn("containment raised for an unresolved material mismatch",
			"record_id", in.RecordID, "containment_ref", ref)
	}
	return res, nil
}

// describeRecord reads the record through an activity.
func describeRecord(ctx, actx workflow.Context, in EscalationInput) (RecordState, error) {
	var state RecordState
	err := workflow.ExecuteActivity(actx, DescribeRecordName, DescribeRecordInput{
		RecordID: in.RecordID, CorrelationID: in.CorrelationID,
	}).Get(ctx, &state)
	return state, err
}

// waitForResolution waits out one tier, re-reading the record every poll
// interval and returning early when it is resolved or a signal says so. The
// remaining wait is tracked in workflow time, so a replay reproduces the same
// number of timers.
func waitForResolution(ctx, actx workflow.Context, in EscalationInput, signals workflow.ReceiveChannel, wait time.Duration) (bool, RecordState, error) {
	deadline := workflow.Now(ctx).UTC().Add(wait)
	var state RecordState
	for {
		remaining := deadline.Sub(workflow.Now(ctx).UTC())
		if remaining <= 0 {
			st, err := describeRecord(ctx, actx, in)
			if err != nil {
				return false, state, err
			}
			return st.Resolved, st, nil
		}
		sleep := in.PollInterval
		if remaining < sleep {
			sleep = remaining
		}

		timerCtx, cancel := workflow.WithCancel(ctx)
		timer := workflow.NewTimer(timerCtx, sleep)
		var signaled bool
		sel := workflow.NewSelector(ctx)
		sel.AddFuture(timer, func(workflow.Future) {})
		sel.AddReceive(signals, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, nil)
			signaled = true
		})
		sel.Select(ctx)
		cancel()
		if signaled {
			for signals.ReceiveAsync(nil) {
			}
		}
		if err := ctx.Err(); err != nil {
			return false, state, err
		}

		st, err := describeRecord(ctx, actx, in)
		if err != nil {
			return false, state, err
		}
		state = st
		if st.Resolved {
			return true, st, nil
		}
	}
}
