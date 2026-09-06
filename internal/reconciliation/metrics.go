package reconciliation

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/nodal/controlplane/internal/observability"
)

// Severity labels the alert an event raises. SEV1 is "wake somebody up": an
// internal-consistency violation or an unresolved material mismatch.
type Severity string

// Severities.
const (
	SEV1 Severity = "SEV1"
	SEV2 Severity = "SEV2"
)

// Alert names emitted by this package. They are the strings an alerting rule
// matches on, so they are stable.
const (
	// AlertLedgerIntegrity is raised when Σ journal entries disagrees with
	// ledger_balances, or Σ open lots with the WALLET balance, or Σ active
	// reservations with the totals row (PART 21, RECONCILIATION.md §6).
	AlertLedgerIntegrity = "ledger_integrity_violation"
	// AlertUnknownSubmission is raised when a submission's outcome is
	// unknown (PART 48).
	AlertUnknownSubmission = "unknown_submission"
	// AlertUnauthorizedSigning is raised when an observed fill falls outside
	// the plan's signed expectations: a signing-boundary failure candidate.
	AlertUnauthorizedSigning = "unauthorized_signing_candidate"
	// AlertObserverDisagreement is raised when two chain observers disagree
	// about a transaction or a balance (PART 196).
	AlertObserverDisagreement = "observer_disagreement"
	// AlertUnknownTransaction is raised when wallet activity matches no
	// attempt (RECONCILIATION.md §3).
	AlertUnknownTransaction = "unknown_transaction"
)

// Metrics records the PART 132 reconciliation instruments and counts the SEV1
// alerts raised, so a test can assert that a drift really did page somebody.
// The zero value is not usable; use NewMetrics or NoopMetrics.
type Metrics struct {
	financial *observability.FinancialMetrics

	sev1 atomic.Int64
	sev2 atomic.Int64
	// alerts counts raised alerts by name; it is small, bounded by the
	// constants above, and exists so tests and the worker's health endpoint
	// can read what happened without scraping a collector.
	alerts   atomic.Pointer[map[string]int64]
	observer func(Alert)
}

// Alert is one raised alert.
type Alert struct {
	Name     string
	Severity Severity
	Detail   string
	RecordID string
	Fields   map[string]any
	At       time.Time
}

// NewMetrics wires the OpenTelemetry instruments. A nil FinancialMetrics is
// allowed: the counters are then only kept in process.
func NewMetrics(fm *observability.FinancialMetrics) *Metrics {
	m := &Metrics{financial: fm}
	empty := map[string]int64{}
	m.alerts.Store(&empty)
	return m
}

// NoopMetrics returns metrics with no exporter attached.
func NoopMetrics() *Metrics { return NewMetrics(nil) }

// OnAlert registers a callback invoked for every raised alert. It is how the
// worker forwards alerts to the notification path and how tests observe them.
func (m *Metrics) OnAlert(fn func(Alert)) { m.observer = fn }

// SEV1Count returns how many SEV1 alerts this process has raised.
func (m *Metrics) SEV1Count() int64 { return m.sev1.Load() }

// SEV2Count returns how many SEV2 alerts this process has raised.
func (m *Metrics) SEV2Count() int64 { return m.sev2.Load() }

// AlertCount returns how many times an alert name was raised.
func (m *Metrics) AlertCount(name string) int64 {
	cur := m.alerts.Load()
	if cur == nil {
		return 0
	}
	return (*cur)[name]
}

// Raise records an alert.
func (m *Metrics) Raise(ctx context.Context, a Alert) {
	if m == nil {
		return
	}
	if a.At.IsZero() {
		a.At = time.Now().UTC()
	}
	switch a.Severity {
	case SEV1:
		m.sev1.Add(1)
	default:
		m.sev2.Add(1)
	}
	for {
		cur := m.alerts.Load()
		next := map[string]int64{}
		if cur != nil {
			for k, v := range *cur {
				next[k] = v
			}
		}
		next[a.Name]++
		if m.alerts.CompareAndSwap(cur, &next) {
			break
		}
	}
	if m.financial != nil && m.financial.ReconciliationMismatches != nil {
		m.financial.ReconciliationMismatches.Add(ctx, 1, observability.WithSafeAttrs(
			attribute.String("alert", a.Name), attribute.String("severity", string(a.Severity))))
	}
	if m.observer != nil {
		m.observer(a)
	}
}

// mismatchOpened records a newly opened mismatch and raises the alert its kind
// and materiality deserve.
func (m *Metrics) mismatchOpened(ctx context.Context, rec Record) {
	if m == nil {
		return
	}
	sev := SEV2
	name := "reconciliation_mismatch"
	switch rec.Kind {
	case KindLedgerInternal, KindPositionLedger:
		sev, name = SEV1, AlertLedgerIntegrity
	case KindSubmissionUnknown:
		sev, name = SEV1, AlertUnknownSubmission
	default:
		if rec.Material {
			sev = SEV1
		}
	}
	m.Raise(ctx, Alert{
		Name: name, Severity: sev, RecordID: rec.ID.String(),
		Detail: string(rec.Kind) + " mismatch on " + rec.ScopeType + " " + rec.ScopeID,
		Fields: map[string]any{"material": rec.Material, "blocks_new_risk": rec.BlocksNewRisk},
	})
}

// unknownSubmission increments the PART 132 unknown_submissions counter.
func (m *Metrics) unknownSubmission(ctx context.Context, attemptID string) {
	if m == nil {
		return
	}
	if m.financial != nil && m.financial.UnknownSubmissions != nil {
		m.financial.UnknownSubmissions.Add(ctx, 1)
	}
	m.Raise(ctx, Alert{
		Name: AlertUnknownSubmission, Severity: SEV1,
		Detail: "submission outcome unknown", Fields: map[string]any{"attempt_id": attemptID},
	})
}

// oldestUnresolved reports the age in seconds of the oldest unresolved
// material mismatch (PART 132 oldest_unresolved_mismatch).
func (m *Metrics) oldestUnresolved(ctx context.Context, seconds int64) {
	if m == nil || m.financial == nil || m.financial.OldestUnresolvedMismatch == nil {
		return
	}
	m.financial.OldestUnresolvedMismatch.Record(ctx, seconds)
}
