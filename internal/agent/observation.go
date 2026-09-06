package agent

import (
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// Observation is what the ToolBroker returns to the evaluator: a typed,
// schema-checked value with its full provenance and the six point-in-time
// timestamps of POINT_IN_TIME.md. The evaluator never sees a raw provider
// response, a credential, a URL or a status code.
//
// Free text that came from outside the platform is not part of Payload: it
// is carried separately in Untrusted, already quarantined, so no evaluator
// path and no prompt-assembly path can mistake it for a typed value or for
// an instruction (PART 67).
type Observation struct {
	// Dependency is the IR dependency name this observation satisfies.
	Dependency  string
	ToolCode    string
	ToolVersion int
	Effect      ir.Effect
	// InvocationID is the tool_invocations row that recorded the call.
	InvocationID InvocationID
	// Source is the data source or provider that answered.
	Source string
	// Mode is copied from the run and never inferred.
	Mode Mode

	// The six timestamps. Provider-supplied times are stored next to ours
	// and never drive knowledge-time logic.
	SourceEventAt       time.Time
	ProviderPublishedAt *time.Time
	PlatformReceivedAt  time.Time
	NormalizedAt        time.Time
	FeatureAvailableAt  time.Time
	DecisionAvailableAt time.Time

	// Payload is the typed value, canonical JSON, validated against the
	// tool's declared output schema.
	Payload json.RawMessage
	// OutputHash is sha256 over Payload; OutputRef is its archive URI.
	OutputHash []byte
	OutputRef  string

	// Untrusted is external free text carried alongside the typed payload.
	Untrusted []Untrusted

	CostUSD money.USD
	Latency time.Duration
}

// UsableAt reports whether a decision taken at t could lawfully have used
// this observation: decision_available_at <= t (POINT_IN_TIME.md). A backtest
// passes simulated time; a live run passes the run's decision time.
func (o Observation) UsableAt(t time.Time) bool {
	return !o.DecisionAvailableAt.IsZero() && !o.DecisionAvailableAt.After(t)
}

// Age is t minus the earlier of the source event time and our receipt time,
// so a provider clock running ahead can never make data look fresher and a
// provider clock running behind only makes it look older.
func (o Observation) Age(t time.Time) time.Duration {
	base := o.PlatformReceivedAt
	if !o.SourceEventAt.IsZero() && o.SourceEventAt.Before(base) {
		base = o.SourceEventAt
	}
	if base.IsZero() {
		return 0
	}
	d := t.Sub(base)
	if d < 0 {
		return 0
	}
	return d
}

// Fresh reports whether the observation is within maxAge at t. A zero or
// negative maxAge means the dependency declared no staleness bound, which
// the IR forbids, so it is treated as stale (fail closed).
func (o Observation) Fresh(t time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return false
	}
	return o.Age(t) <= maxAge
}

// Suspicious reports whether any quarantined item carried an injection
// pattern. It is evidence, never a gate.
func (o Observation) Suspicious() bool {
	for _, u := range o.Untrusted {
		if u.Suspicious() {
			return true
		}
	}
	return false
}

// Signals returns the deduplicated injection signals across every quarantined
// item, for the run's evidence record.
func (o Observation) Signals() []InjectionSignal {
	seen := map[InjectionSignal]struct{}{}
	for _, u := range o.Untrusted {
		for _, s := range u.Signals {
			seen[s] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]InjectionSignal, 0, len(seen))
	for _, known := range allInjectionSignals {
		if _, ok := seen[known]; ok {
			out = append(out, known)
		}
	}
	return out
}

// allInjectionSignals fixes the reporting order of Signals.
var allInjectionSignals = []InjectionSignal{
	SignalApprovalImpersonation,
	SignalCredentialProbe,
	SignalDelimiterForgery,
	SignalEffectEscalation,
	SignalHiddenText,
	SignalInstructionOverride,
	SignalToolRequest,
}

// FreshnessCheck is one dependency's freshness verdict, recorded in
// agent_runs.evidence so a later reader can see exactly what the run knew.
type FreshnessCheck struct {
	Dependency          string        `json:"dependency"`
	ToolCode            string        `json:"tool_code"`
	ToolVersion         int           `json:"tool_version"`
	InvocationID        string        `json:"invocation_id,omitempty"`
	DecisionAvailableAt time.Time     `json:"decision_available_at"`
	AgeMS               int64         `json:"age_ms"`
	MaxAgeMS            int64         `json:"max_age_ms"`
	Fresh               bool          `json:"fresh"`
	PointInTimeValid    bool          `json:"point_in_time_valid"`
	Required            bool          `json:"required"`
	Present             bool          `json:"present"`
	OutputHash          string        `json:"output_hash,omitempty"`
	Refusal             string        `json:"refusal,omitempty"`
	InjectionSignals    []string      `json:"injection_signals,omitempty"`
	Untrusted           int           `json:"untrusted_items,omitempty"`
	latency             time.Duration // not serialized; kept for metrics
}

// Latency is how long the call took.
func (f FreshnessCheck) Latency() time.Duration { return f.latency }
