package orders

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/clock"
)

// Observing the request path.
//
// A latency belongs to a request, not to a row, so the database cannot say
// how long a submit or a cancel took: only the code that ran it can. The
// Pipeline therefore times each customer Submit and Cancel it runs, on the
// monotonic clock (clock.Start), and hands the elapsed time and the outcome
// to an Observer injected at construction (WithObserver). The Observer is
// the metrics side's (internal/advancedmetrics.OrderLatency); this package
// imports no metrics SDK and keeps no state of its own about it.
//
// What is timed is the whole command as its caller waits for it: every stage
// transaction of a submission (Accept through Complete, retries included),
// or the cancel's one transaction. A command that panics is not observed.

// CommandTiming is one customer command the Pipeline ran.
type CommandTiming struct {
	// Kind is CommandNewOrder for Pipeline.Submit and CommandCancel for
	// Pipeline.Cancel.
	Kind CommandKind
	// Elapsed is the command's monotonic duration.
	Elapsed time.Duration
	// Err is what the command returned: nil when it was carried out, the
	// refusal it recorded, a refusal of the request before anything was
	// recorded, or a failure.
	Err error
}

// Observer is told about each customer command the Pipeline runs, once,
// after it has returned. It must not block: it runs on the request's path.
type Observer interface {
	ObserveCommand(ctx context.Context, t CommandTiming)
}

// PipelineOption configures a Pipeline at construction.
type PipelineOption func(*Pipeline)

// WithObserver has the Pipeline time every Submit and Cancel and tell o.
// A nil o observes nothing.
func WithObserver(o Observer) PipelineOption {
	return func(p *Pipeline) { p.observer = o }
}

// startTiming starts timing a command; nil when nothing observes.
func (p *Pipeline) startTiming() clock.Stopwatch {
	if p.observer == nil {
		return nil
	}
	return clock.Start()
}

// observe tells the observer a command returned err after the time stop
// measures.
func (p *Pipeline) observe(ctx context.Context, kind CommandKind, stop clock.Stopwatch, err error) {
	if p.observer == nil || stop == nil {
		return
	}
	p.observer.ObserveCommand(ctx, CommandTiming{Kind: kind, Elapsed: stop(), Err: err})
}
