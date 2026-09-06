package provider

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// Health is a provider's operational state (PART 79).
type Health string

// Health states, ordered from best to worst.
const (
	Healthy   Health = "HEALTHY"
	Degraded  Health = "DEGRADED"
	Unhealthy Health = "UNHEALTHY"
	Disabled  Health = "DISABLED"
)

// AllowsNewActions reports whether the provider may be used for a new
// external action (submission, funding session, model call).
func (h Health) AllowsNewActions() bool { return h == Healthy || h == Degraded }

// AllowsObservation reports whether reads/observation may continue. Only an
// operator DISABLE stops reads; degraded and unhealthy providers are still
// consulted for status so the platform never goes blind (PART 107).
func (h Health) AllowsObservation() bool { return h != Disabled }

// Rank orders states for "worst of" composition.
func (h Health) Rank() int {
	switch h {
	case Healthy:
		return 0
	case Degraded:
		return 1
	case Unhealthy:
		return 2
	case Disabled:
		return 3
	}
	return 3
}

// Worst returns the worse of two states; unknown values are treated as DISABLED.
func Worst(a, b Health) Health {
	if a.Rank() >= b.Rank() {
		return a
	}
	return b
}

// RetryClass classifies an external operation (PART 106).
type RetryClass string

// Retry classes.
const (
	// SafeRetry: idempotent reads; retry freely within budget.
	SafeRetry RetryClass = "SAFE_RETRY"
	// IdempotentWrite: writes the provider deduplicates by key; retry with the same key.
	IdempotentWrite RetryClass = "IDEMPOTENT_WRITE"
	// UnknownEffectWrite: the effect may have happened; never retry before a status investigation.
	UnknownEffectWrite RetryClass = "UNKNOWN_EFFECT_WRITE"
)

// MayRetryBlindly reports whether an operation of this class may be retried
// without first investigating the outcome of the previous attempt.
func (c RetryClass) MayRetryBlindly() bool { return c == SafeRetry || c == IdempotentWrite }

// VerificationLabel is the honest integration status of an adapter (PART 208).
type VerificationLabel string

// Verification labels, in increasing order of evidence.
const (
	CodeComplete    VerificationLabel = "CODE_COMPLETE"
	ContractTested  VerificationLabel = "CONTRACT_TESTED"
	SandboxVerified VerificationLabel = "SANDBOX_VERIFIED"
	CanaryVerified  VerificationLabel = "CANARY_VERIFIED"
	LiveVerified    VerificationLabel = "LIVE_VERIFIED"
	BlockedExternal VerificationLabel = "BLOCKED_EXTERNAL"
)

// Thresholds define when observed samples move a provider between states.
// Error rates are expressed in basis points of calls within the window so no
// floating point is involved.
type Thresholds struct {
	Window          time.Duration // sample window considered
	MinSamples      int           // below this the tracker keeps its previous state (no evidence, no change)
	DegradedErrBPS  int64         // error rate ≥ this → DEGRADED
	UnhealthyErrBPS int64         // error rate ≥ this → UNHEALTHY
	DegradedP95     time.Duration // p95 latency ≥ this → DEGRADED (0 disables)
	UnhealthyP95    time.Duration // p95 latency ≥ this → UNHEALTHY (0 disables)
	MaxStaleness    time.Duration // no successful sample for longer than this → UNHEALTHY (0 disables)
	RecoverStreak   int           // consecutive successes required to step one state up (hysteresis)
}

// DefaultThresholds are conservative starting values; production tunes them
// per provider through configuration.
func DefaultThresholds() Thresholds {
	return Thresholds{
		Window:          60 * time.Second,
		MinSamples:      5,
		DegradedErrBPS:  1_000, // 10%
		UnhealthyErrBPS: 5_000, // 50%
		DegradedP95:     2 * time.Second,
		UnhealthyP95:    8 * time.Second,
		MaxStaleness:    30 * time.Second,
		RecoverStreak:   5,
	}
}

// Validate checks threshold consistency.
func (t Thresholds) Validate() error {
	switch {
	case t.Window <= 0:
		return errors.New("provider: window must be positive")
	case t.MinSamples < 1:
		return errors.New("provider: min samples must be >= 1")
	case t.DegradedErrBPS < 0 || t.UnhealthyErrBPS < 0 || t.DegradedErrBPS > 10_000 || t.UnhealthyErrBPS > 10_000:
		return errors.New("provider: error thresholds must be within 0..10000 bps")
	case t.DegradedErrBPS > t.UnhealthyErrBPS:
		return errors.New("provider: degraded error threshold must not exceed unhealthy threshold")
	case t.DegradedP95 < 0 || t.UnhealthyP95 < 0 || t.MaxStaleness < 0:
		return errors.New("provider: latency/staleness thresholds must not be negative")
	case t.DegradedP95 > 0 && t.UnhealthyP95 > 0 && t.DegradedP95 > t.UnhealthyP95:
		return errors.New("provider: degraded p95 must not exceed unhealthy p95")
	case t.RecoverStreak < 1:
		return errors.New("provider: recover streak must be >= 1")
	}
	return nil
}

type sample struct {
	at      time.Time
	ok      bool
	latency time.Duration
}

// Tracker derives a provider's Health from observed call samples. It is safe
// for concurrent use. Operator disable/enable overrides observation.
type Tracker struct {
	name string
	th   Thresholds

	mu          sync.Mutex
	samples     []sample
	state       Health
	disabled    bool
	disableWhy  string
	successRun  int
	lastSuccess time.Time
	changedAt   time.Time
}

// NewTracker creates a tracker starting in HEALTHY (no evidence of failure).
func NewTracker(name string, th Thresholds, now time.Time) (*Tracker, error) {
	if name == "" {
		return nil, errors.New("provider: tracker name required")
	}
	if err := th.Validate(); err != nil {
		return nil, err
	}
	return &Tracker{name: name, th: th, state: Healthy, lastSuccess: now, changedAt: now}, nil
}

// Name returns the provider name.
func (t *Tracker) Name() string { return t.name }

// Observe records one call outcome and returns the resulting health.
func (t *Tracker) Observe(now time.Time, ok bool, latency time.Duration) Health {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.samples = append(t.samples, sample{at: now, ok: ok, latency: latency})
	t.prune(now)
	if ok {
		t.successRun++
		t.lastSuccess = now
	} else {
		t.successRun = 0
	}
	t.recompute(now)
	return t.effective()
}

// Tick re-evaluates staleness without a new sample (call periodically).
func (t *Tracker) Tick(now time.Time) Health {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune(now)
	t.recompute(now)
	return t.effective()
}

// Disable is the operator/kill-switch override: no new actions until Enable.
func (t *Tracker) Disable(now time.Time, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.disabled {
		t.changedAt = now
	}
	t.disabled = true
	t.disableWhy = reason
}

// Enable lifts the operator override; observed health applies again.
func (t *Tracker) Enable(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.disabled {
		t.changedAt = now
	}
	t.disabled = false
	t.disableWhy = ""
}

// Health returns the effective state.
func (t *Tracker) Health() Health {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.effective()
}

// Snapshot is a point-in-time view for logs, metrics, and risk inputs.
type Snapshot struct {
	Name          string
	Health        Health
	Observed      Health // health from samples alone, ignoring operator disable
	Disabled      bool
	DisableReason string
	Samples       int
	ErrorRateBPS  int64
	P95           time.Duration
	LastSuccessAt time.Time
	ChangedAt     time.Time
}

// Snapshot returns the current view.
func (t *Tracker) Snapshot(now time.Time) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune(now)
	errBPS, p95 := t.stats()
	return Snapshot{
		Name: t.name, Health: t.effective(), Observed: t.state, Disabled: t.disabled, DisableReason: t.disableWhy,
		Samples: len(t.samples), ErrorRateBPS: errBPS, P95: p95, LastSuccessAt: t.lastSuccess, ChangedAt: t.changedAt,
	}
}

func (t *Tracker) effective() Health {
	if t.disabled {
		return Disabled
	}
	return t.state
}

func (t *Tracker) prune(now time.Time) {
	cutoff := now.Add(-t.th.Window)
	i := 0
	for i < len(t.samples) && t.samples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		t.samples = append([]sample(nil), t.samples[i:]...)
	}
}

// stats returns error rate in bps and p95 latency over the window.
func (t *Tracker) stats() (int64, time.Duration) {
	n := len(t.samples)
	if n == 0 {
		return 0, 0
	}
	failures := 0
	lat := make([]time.Duration, 0, n)
	for _, s := range t.samples {
		if !s.ok {
			failures++
		} else {
			lat = append(lat, s.latency)
		}
	}
	errBPS := int64(failures) * 10_000 / int64(n)
	var p95 time.Duration
	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		idx := (len(lat)*95 + 99) / 100 // ceil(0.95 n), 1-based
		if idx < 1 {
			idx = 1
		}
		p95 = lat[idx-1]
	}
	return errBPS, p95
}

// recompute applies thresholds with hysteresis: moving to a worse state is
// immediate; moving to a better state requires RecoverStreak consecutive
// successes and the window statistics to support the better state.
func (t *Tracker) recompute(now time.Time) {
	target := t.targetState(now)
	switch {
	case target.Rank() > t.state.Rank():
		t.transition(target, now)
	case target.Rank() < t.state.Rank():
		if t.successRun >= t.th.RecoverStreak {
			// Step up one level at a time so recovery is visible and gradual.
			next := t.state
			switch t.state {
			case Unhealthy:
				next = Degraded
			case Degraded:
				next = Healthy
			}
			if next.Rank() < target.Rank() {
				next = target
			}
			t.transition(next, now)
			t.successRun = 0
		}
	}
}

func (t *Tracker) transition(to Health, now time.Time) {
	if to != t.state {
		t.state = to
		t.changedAt = now
	}
}

// targetState is the state the window statistics alone justify.
func (t *Tracker) targetState(now time.Time) Health {
	if t.th.MaxStaleness > 0 && now.Sub(t.lastSuccess) > t.th.MaxStaleness {
		return Unhealthy
	}
	if len(t.samples) < t.th.MinSamples {
		return t.state // insufficient evidence: hold
	}
	errBPS, p95 := t.stats()
	switch {
	case errBPS >= t.th.UnhealthyErrBPS, t.th.UnhealthyP95 > 0 && p95 >= t.th.UnhealthyP95:
		return Unhealthy
	case errBPS >= t.th.DegradedErrBPS, t.th.DegradedP95 > 0 && p95 >= t.th.DegradedP95:
		return Degraded
	default:
		return Healthy
	}
}

// Registry holds trackers by provider name.
type Registry struct {
	mu       sync.RWMutex
	trackers map[string]*Tracker
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{trackers: map[string]*Tracker{}} }

// Register adds a tracker; duplicate names are rejected.
func (r *Registry) Register(t *Tracker) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.trackers[t.name]; dup {
		return errors.New("provider: duplicate tracker " + t.name)
	}
	r.trackers[t.name] = t
	return nil
}

// Get returns a tracker by name.
func (r *Registry) Get(name string) (*Tracker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.trackers[name]
	return t, ok
}

// HealthMap returns name → effective health, sorted keys not required (map);
// callers that need determinism should iterate Names().
func (r *Registry) HealthMap() map[string]Health {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Health, len(r.trackers))
	for n, t := range r.trackers {
		out[n] = t.Health()
	}
	return out
}

// Names returns registered names sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.trackers))
	for n := range r.trackers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
