package advancedjobs

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// Settings are the two numbers an operator may tune per job.
type Settings struct {
	// Interval is how often the scheduler starts the job.
	Interval time.Duration
	// Batch is the most units one run takes on.
	Batch int
}

// Spec describes a job to the scheduler, to `advanced-worker list` and to an
// operator reading either.
type Spec struct {
	// Name is the job's stable identity: in metrics, logs, configuration
	// overrides and `advanced-worker once <job>`.
	Name string
	Settings
	// Calls names the subsystem operations the job drives.
	Calls string
	// KillSwitch says how the job behaves while kill switches are active.
	// The subsystem decides; this states what it decides.
	KillSwitch string
}

// Job is one kind of time-driven work.
type Job interface {
	Spec() Spec
	// Run does one bounded pass. It returns an error only when the pass as a
	// whole could not proceed (the due units could not be read, the deadline
	// passed); a unit that could not be processed is a Failure in the Report
	// and the pass goes on.
	Run(ctx context.Context) (Report, error)
}

// Report is what one run did.
type Report struct {
	// Examined is how many units the run considered.
	Examined int `json:"examined"`
	// Changed is how many of them the run changed.
	Changed int `json:"changed"`
	// Deferred is how many were due but whose subsystem said to wait for a
	// condition that clears itself (a corporate action with orders still
	// live, for instance).
	Deferred int `json:"deferred,omitempty"`
	// Failures are the units the run could not process.
	Failures []Failure `json:"failures,omitempty"`
}

// OK reports whether every unit the run examined was processed.
func (r Report) OK() bool { return len(r.Failures) == 0 }

// Failure is one unit a run could not process. Code and Reason are redacted
// (Redact); the full error is kept only for the worker's log.
type Failure struct {
	Unit   string `json:"unit"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
	err    error
}

// Err returns the unredacted error, for the worker's log.
func (f Failure) Err() error { return f.err }

// fail records that unit could not be processed.
func (r *Report) fail(unit string, err error) {
	code, reason := Redact(err)
	r.Failures = append(r.Failures, Failure{Unit: unit, Code: code, Reason: reason, err: err})
}

// Codes a Failure carries besides the errs codes of a subsystem's refusal.
const (
	// CodeTimeout: the run's deadline passed.
	CodeTimeout = "TIMEOUT"
	// CodeCancelled: the run was cancelled (shutdown).
	CodeCancelled = "CANCELLED"
	// CodePanic: the job panicked.
	CodePanic = "PANIC"
	// CodeRetriesExhausted: a serialization failure or deadlock outlasted
	// the transaction's retry budget.
	CodeRetriesExhausted = "RETRIES_EXHAUSTED"
)

// maxReasonRunes bounds a redacted reason.
const maxReasonRunes = 240

// Redact turns an error into a stable code and a reason that is safe to
// print, export and keep: a subsystem refusal (an *errs.Error whose code is
// not INTERNAL) keeps its code and its detail, which errs writes for API
// boundaries and which therefore carries no internal state; everything else
// -- an INTERNAL error, a database or driver error, anything unrecognised --
// is reduced to its class, and its text goes only to the worker's log.
func Redact(err error) (code, reason string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, context.DeadlineExceeded):
		return CodeTimeout, "the run's deadline passed"
	case errors.Is(err, context.Canceled):
		return CodeCancelled, "the run was cancelled"
	case errors.Is(err, db.ErrRetriesExhausted) || db.IsRetryable(err):
		return CodeRetriesExhausted, "a serialization failure or deadlock outlasted the retry budget"
	}
	if e, ok := errs.As(err); ok && e.Code != errs.CodeInternal && e.Code != "" {
		return string(e.Code), sanitize(e.Detail)
	}
	return string(errs.CodeInternal), "internal error; the worker log has the detail"
}

// sanitize bounds a reason and keeps it on one printable line.
func sanitize(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxReasonRunes {
			b.WriteString("...")
			break
		}
		if !unicode.IsPrint(r) {
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// unitRetries is the retry budget of a unit's transaction. Every unit's
// transaction function is wholly inside the database, so re-running it after
// a serialization failure or deadlock is safe.
const unitRetries = 3

// inUnit runs fn as one unit of work: its own READ COMMITTED transaction --
// the isolation every advanced service is written for -- retried on a
// serialization failure or deadlock, committed when fn returns nil and rolled
// back otherwise.
func inUnit(ctx context.Context, d *db.DB, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: unitRetries}, fn)
}

// inSnapshot runs fn in a read-only REPEATABLE READ transaction: one
// consistent view, for proofs and reconciliations that only read.
func inSnapshot(ctx context.Context, d *db.DB, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true, MaxRetries: unitRetries}, fn)
}

// readOnly runs fn in a read-only READ COMMITTED transaction: the reads that
// decide which units are due.
func readOnly(ctx context.Context, d *db.DB, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, ReadOnly: true, MaxRetries: unitRetries}, fn)
}

// clampBatch keeps a batch within 1..max, max being the bound the
// subsystem's own operation enforces.
func clampBatch(n, max int) int {
	if n < 1 {
		return 1
	}
	if max > 0 && n > max {
		return max
	}
	return n
}
