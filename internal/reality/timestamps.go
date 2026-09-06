package reality

import (
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// AvailabilityPolicy sets the platform-controlled availability instants
// (POINT_IN_TIME.md §1). It is an explicit, versioned input: nothing in the
// engine infers availability from provider clocks.
type AvailabilityPolicy struct {
	// FeatureLatency is added to normalized_at to obtain
	// feature_available_at when no feature layer has set a later value.
	FeatureLatency time.Duration
	// PipelineLatency is tools.pipeline_latency_ms: the time a decision
	// process needs before a datum can influence it.
	PipelineLatency time.Duration
	// Version names the policy in evidence.
	Version string
}

// Validate rejects negative latencies (they would make data look available
// before it was received).
func (p AvailabilityPolicy) Validate() error {
	if p.FeatureLatency < 0 || p.PipelineLatency < 0 {
		return errs.New(errs.CodeValidationFailed, "reality: availability latencies must not be negative")
	}
	return nil
}

// Apply fills normalized_at, feature_available_at and decision_available_at
// from the policy and returns timestamps that satisfy Validate. It requires
// platform_received_at; normalized_at is clamped so it is never earlier
// than platform_received_at (a clock read from a different core can be
// nanoseconds behind); a caller-provided feature_available_at is kept when
// it is later than the computed one.
func (p AvailabilityPolicy) Apply(ts Timestamps, normalizedAt time.Time) (Timestamps, error) {
	if err := p.Validate(); err != nil {
		return Timestamps{}, err
	}
	if ts.PlatformReceivedAt.IsZero() {
		return Timestamps{}, errs.New(errs.CodeValidationFailed, "reality: platform_received_at is required")
	}
	out := ts.UTC()
	received := out.PlatformReceivedAt
	n := normalizedAt.UTC()
	if n.IsZero() || n.Before(received) {
		n = received
	}
	out.NormalizedAt = n
	feature := n.Add(p.FeatureLatency)
	if !out.FeatureAvailableAt.IsZero() && out.FeatureAvailableAt.After(feature) {
		feature = out.FeatureAvailableAt
	}
	out.FeatureAvailableAt = feature
	decision := maxTime(received, n, feature).Add(p.PipelineLatency)
	out.DecisionAvailableAt = decision
	if out.SourceEventAt.IsZero() {
		// Unknown source time: the conservative choice is our own receipt,
		// which can only make the datum look older, never fresher.
		out.SourceEventAt = received
	}
	return out, nil
}

// Validate enforces the platform-clock ordering invariant and that every
// instant is UTC.
func (ts Timestamps) Validate() error {
	fields := map[string]any{}
	if ts.SourceEventAt.IsZero() {
		fields["source_event_at"] = "required"
	}
	if ts.PlatformReceivedAt.IsZero() {
		fields["platform_received_at"] = "required"
	}
	if ts.NormalizedAt.IsZero() {
		fields["normalized_at"] = "required"
	}
	if ts.FeatureAvailableAt.IsZero() {
		fields["feature_available_at"] = "required"
	}
	if ts.DecisionAvailableAt.IsZero() {
		fields["decision_available_at"] = "required"
	}
	if len(fields) == 0 {
		if ts.NormalizedAt.Before(ts.PlatformReceivedAt) {
			fields["normalized_at"] = "must not precede platform_received_at"
		}
		if ts.FeatureAvailableAt.Before(ts.NormalizedAt) {
			fields["feature_available_at"] = "must not precede normalized_at"
		}
		if ts.DecisionAvailableAt.Before(ts.FeatureAvailableAt) {
			fields["decision_available_at"] = "must not precede feature_available_at"
		}
		if ts.DecisionAvailableAt.Before(ts.PlatformReceivedAt) {
			fields["decision_available_at"] = "must not precede platform_received_at"
		}
	}
	for name, t := range map[string]time.Time{
		"source_event_at": ts.SourceEventAt, "provider_published_at": ts.ProviderPublishedAt,
		"platform_received_at": ts.PlatformReceivedAt, "normalized_at": ts.NormalizedAt,
		"feature_available_at": ts.FeatureAvailableAt, "decision_available_at": ts.DecisionAvailableAt,
	} {
		if !t.IsZero() && t.Location() != time.UTC {
			fields[name] = "must be UTC"
		}
	}
	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "reality: invalid timestamps").WithFields(fields)
	}
	return nil
}

// UTC returns a copy with every instant in UTC (zero values stay zero).
func (ts Timestamps) UTC() Timestamps {
	conv := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}
		return t.UTC()
	}
	return Timestamps{
		SourceEventAt: conv(ts.SourceEventAt), ProviderPublishedAt: conv(ts.ProviderPublishedAt),
		PlatformReceivedAt: conv(ts.PlatformReceivedAt), NormalizedAt: conv(ts.NormalizedAt),
		FeatureAvailableAt: conv(ts.FeatureAvailableAt), DecisionAvailableAt: conv(ts.DecisionAvailableAt),
	}
}

// AvailableAt reports whether a decision taken at t may use the datum:
// decision_available_at <= t (PART 74, 226).
func (ts Timestamps) AvailableAt(t time.Time) bool {
	return !ts.DecisionAvailableAt.IsZero() && !ts.DecisionAvailableAt.After(t)
}

// Age is the freshness age at t: t − min(source_event_at,
// platform_received_at). A provider clock running ahead can never make the
// datum look fresher; one running behind only makes it look older.
func (ts Timestamps) Age(t time.Time) time.Duration {
	anchor := ts.PlatformReceivedAt
	if !ts.SourceEventAt.IsZero() && ts.SourceEventAt.Before(anchor) {
		anchor = ts.SourceEventAt
	}
	if anchor.IsZero() {
		return 0
	}
	return t.Sub(anchor)
}

func maxTime(ts ...time.Time) time.Time {
	var m time.Time
	for _, t := range ts {
		if t.After(m) {
			m = t
		}
	}
	return m
}
