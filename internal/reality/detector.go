package reality

import (
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// ObservationKind classifies what the detector is shown.
type ObservationKind string

// Observation kinds.
const (
	// ObservationEvent is a data event with position information.
	ObservationEvent ObservationKind = "EVENT"
	// ObservationReconnect is a stream (re)connection.
	ObservationReconnect ObservationKind = "RECONNECT"
)

// Observation is the position information of one ingested item.
type Observation struct {
	Kind               ObservationKind
	Sequence           *uint64
	SourceEventAt      time.Time // provider clock, untrusted
	PlatformReceivedAt time.Time
	RawObjectID        string
	Detail             string
}

// DetectorOptions tune detection per data source.
type DetectorOptions struct {
	// ContiguousSequence says the source's sequence increases by exactly one
	// per event, so a jump is a GAP. Chain streams keyed by slot are not
	// contiguous: their gaps are found by reconnect-driven backfill.
	ContiguousSequence bool
	// HeartbeatTimeout is data_sources.heartbeat_timeout_ms: silence beyond
	// it while connected is a SILENCE gap.
	HeartbeatTimeout time.Duration
	// OrderingTolerance is how far source_event_at may go backwards before
	// it is an ORDERING_ANOMALY (provider clocks are noisy; 0 means any
	// decrease).
	OrderingTolerance time.Duration
}

// Validate checks the options.
func (o DetectorOptions) Validate() error {
	if o.HeartbeatTimeout <= 0 {
		return errs.New(errs.CodeValidationFailed, "reality: heartbeat timeout must be > 0")
	}
	if o.OrderingTolerance < 0 {
		return errs.New(errs.CodeValidationFailed, "reality: ordering tolerance must not be negative")
	}
	return nil
}

// Finding is one detected discontinuity, ready to become a stream_gaps row.
type Finding struct {
	Kind             string
	ExpectedSequence *uint64
	ObservedSequence *uint64
	GapStartSequence *uint64
	GapEndSequence   *uint64
	GapStartAt       time.Time
	GapEndAt         time.Time // zero = open-ended
	Detail           map[string]any
	// Resolution is the initial resolution of the row: OPEN for GAP and
	// SILENCE, ACKNOWLEDGED (by SYSTEM) for RECONNECT and ORDERING_ANOMALY.
	Resolution string
	// FlagEvent marks the observed event as anomalous (kept, flagged).
	FlagEvent bool
}

// Detector applies the POINT_IN_TIME.md §4 rules. It is pure.
type Detector struct {
	opts DetectorOptions
}

// NewDetector validates the options.
func NewDetector(opts DetectorOptions) (Detector, error) {
	if err := opts.Validate(); err != nil {
		return Detector{}, err
	}
	return Detector{opts: opts}, nil
}

// Options returns the detector's options.
func (d Detector) Options() DetectorOptions { return d.opts }

// Inspect compares an observation with the checkpoint it will advance and
// returns the discontinuities found. A checkpoint in REPLAYING status is a
// deliberate rewind: going backwards is then expected, not an anomaly.
func (d Detector) Inspect(cp Checkpoint, obs Observation) []Finding {
	var out []Finding
	switch obs.Kind {
	case ObservationReconnect:
		f := Finding{Kind: GapKindReconnect, GapStartAt: obs.PlatformReceivedAt, GapEndAt: obs.PlatformReceivedAt, Resolution: ResolutionAcknowledged, Detail: map[string]any{}}
		if !cp.LastPlatformReceivedAt.IsZero() {
			f.GapStartAt = cp.LastPlatformReceivedAt
		}
		if !cp.DisconnectedAt.IsZero() && cp.DisconnectedAt.After(f.GapStartAt) {
			f.GapStartAt = cp.DisconnectedAt
		}
		if obs.Detail != "" {
			f.Detail["detail"] = obs.Detail
		}
		f.Detail["previous_status"] = cp.Status
		f.Detail["first_connection"] = !cp.Found
		return append(out, f)
	case ObservationEvent:
	default:
		return nil
	}
	replaying := cp.Status == CheckpointReplaying
	if obs.Sequence != nil && cp.LastSequence != nil {
		seq, last := *obs.Sequence, *cp.LastSequence
		switch {
		case seq > last+1 && d.opts.ContiguousSequence:
			expected, start, end := last+1, last+1, seq-1
			out = append(out, Finding{
				Kind: GapKindGap, ExpectedSequence: &expected, ObservedSequence: &seq, GapStartSequence: &start, GapEndSequence: &end,
				GapStartAt: firstNonZero(cp.LastPlatformReceivedAt, obs.PlatformReceivedAt), GapEndAt: obs.PlatformReceivedAt,
				Resolution: ResolutionOpen, Detail: map[string]any{"missing": end - start + 1},
			})
		case seq < last && !replaying:
			expected := last
			out = append(out, Finding{
				Kind: GapKindOrderingAnomaly, ExpectedSequence: &expected, ObservedSequence: &seq,
				GapStartAt: obs.PlatformReceivedAt, GapEndAt: obs.PlatformReceivedAt, Resolution: ResolutionAcknowledged, FlagEvent: true,
				Detail: map[string]any{"reason": "sequence_decreased", "raw_object_id": obs.RawObjectID},
			})
		}
	}
	if !replaying && !obs.SourceEventAt.IsZero() && !cp.LastSourceEventAt.IsZero() {
		if back := cp.LastSourceEventAt.Sub(obs.SourceEventAt); back > d.opts.OrderingTolerance {
			f := Finding{
				Kind: GapKindOrderingAnomaly, GapStartAt: obs.PlatformReceivedAt, GapEndAt: obs.PlatformReceivedAt,
				Resolution: ResolutionAcknowledged, FlagEvent: true,
				Detail: map[string]any{"reason": "source_event_at_decreased", "backwards_ms": back.Milliseconds(), "raw_object_id": obs.RawObjectID},
			}
			f.ObservedSequence, f.ExpectedSequence = obs.Sequence, cp.LastSequence
			out = append(out, f)
		}
	}
	return out
}

// Silence reports a SILENCE finding when the checkpoint is connected and no
// event arrived for HeartbeatTimeout as of now. It returns false when the
// stream is not connected, has never delivered, or is within the timeout.
func (d Detector) Silence(cp Checkpoint, now time.Time) (Finding, bool) {
	if !cp.Found || cp.Status != CheckpointActive {
		return Finding{}, false
	}
	last := cp.LastPlatformReceivedAt
	if last.IsZero() {
		last = cp.ConnectedAt
	}
	if last.IsZero() || now.Sub(last) <= d.opts.HeartbeatTimeout {
		return Finding{}, false
	}
	return Finding{
		Kind: GapKindSilence, GapStartAt: last, Resolution: ResolutionOpen,
		Detail: map[string]any{"heartbeat_timeout_ms": d.opts.HeartbeatTimeout.Milliseconds(), "silent_ms": now.Sub(last).Milliseconds()},
	}, true
}

func firstNonZero(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}
