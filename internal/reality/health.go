package reality

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/provider"
)

// HealthEvaluatorVersion names the sampling rules below in every row.
const HealthEvaluatorVersion = "reality.health.v1"

// Reason codes recorded on samples (sorted in the row).
const (
	ReasonErrorRateHigh       = "ERROR_RATE_HIGH"
	ReasonLatencyHigh         = "LATENCY_HIGH"
	ReasonStale               = "STALE"
	ReasonInsufficientSamples = "INSUFFICIENT_SAMPLES"
	ReasonOperatorDisabled    = "OPERATOR_DISABLED"
	ReasonReportedUnhealthy   = "REPORTED_UNHEALTHY"
	ReasonReportedDegraded    = "REPORTED_DEGRADED"
	ReasonNoSamples           = "NO_SAMPLES"
	ReasonNoRecentSample      = "NO_RECENT_SAMPLE"
)

// healthSampleKind is the phantom kind of sample ids.
type healthSampleKind struct{}

type latencySample struct {
	at      time.Time
	ok      bool
	latency time.Duration
}

// HealthSampler turns call observations into provider_health_samples rows
// (PART 79). It keeps its own window of observations so it can report p50
// and p99 exactly, and combines them with the provider's reported effective
// state (which carries operator DISABLE) by taking the worse of the two.
type HealthSampler struct {
	th       provider.Thresholds
	reported func() provider.Health
	version  string
	source   string

	mu          sync.Mutex
	samples     []latencySample
	lastSuccess time.Time
	createdAt   time.Time
}

var _ HealthEvaluator = (*HealthSampler)(nil)

// NewHealthSampler builds a sampler. reported may be nil when the provider
// has no tracker of its own; dataSourceID may be empty.
func NewHealthSampler(th provider.Thresholds, reported func() provider.Health, dataSourceID string, now time.Time) (*HealthSampler, error) {
	if err := th.Validate(); err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, err.Error())
	}
	return &HealthSampler{th: th, reported: reported, version: HealthEvaluatorVersion, source: dataSourceID, createdAt: now.UTC()}, nil
}

// Observe records one call outcome.
func (s *HealthSampler) Observe(now time.Time, ok bool, latency time.Duration) {
	if latency < 0 {
		latency = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, latencySample{at: now.UTC(), ok: ok, latency: latency})
	if ok {
		s.lastSuccess = now.UTC()
	}
	s.prune(now)
}

func (s *HealthSampler) prune(now time.Time) {
	cutoff := now.Add(-s.th.Window)
	i := 0
	for i < len(s.samples) && s.samples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		s.samples = append([]latencySample(nil), s.samples[i:]...)
	}
}

// Sample implements HealthEvaluator. It is pure over the recorded
// observations and never blocks on I/O.
func (s *HealthSampler) Sample(_ context.Context, providerName, role string, now time.Time) (HealthSample, error) {
	if providerName == "" {
		return HealthSample{}, errs.New(errs.CodeValidationFailed, "reality: provider name is required")
	}
	if !oneOf(role, RoleData, RoleObservation, RoleExecution, RoleFunding, RoleWallet, RoleModel) {
		return HealthSample{}, errs.New(errs.CodeValidationFailed, "reality: unknown provider role").WithField("role", role)
	}
	now = now.UTC()
	s.mu.Lock()
	s.prune(now)
	samples := append([]latencySample(nil), s.samples...)
	lastSuccess := s.lastSuccess
	created := s.createdAt
	s.mu.Unlock()

	n := len(samples)
	failures := 0
	lat := make([]time.Duration, 0, n)
	for _, x := range samples {
		if x.ok {
			lat = append(lat, x.latency)
		} else {
			failures++
		}
	}
	var errBPS int64
	if n > 0 {
		errBPS = int64(failures) * 10_000 / int64(n)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p50, p99 := percentile(lat, 50), percentile(lat, 99)
	anchor := lastSuccess
	if anchor.IsZero() {
		anchor = created
	}
	staleness := now.Sub(anchor)
	if staleness < 0 {
		staleness = 0
	}

	reasons := map[string]struct{}{}
	derived := provider.Healthy
	worsen := func(h provider.Health, reason string) {
		derived = provider.Worst(derived, h)
		reasons[reason] = struct{}{}
	}
	if s.th.MaxStaleness > 0 && staleness > s.th.MaxStaleness {
		worsen(provider.Unhealthy, ReasonStale)
	}
	if n < s.th.MinSamples {
		reasons[ReasonInsufficientSamples] = struct{}{}
	} else {
		switch {
		case errBPS >= s.th.UnhealthyErrBPS:
			worsen(provider.Unhealthy, ReasonErrorRateHigh)
		case errBPS >= s.th.DegradedErrBPS:
			worsen(provider.Degraded, ReasonErrorRateHigh)
		}
		switch {
		case s.th.UnhealthyP95 > 0 && p99 >= s.th.UnhealthyP95:
			worsen(provider.Unhealthy, ReasonLatencyHigh)
		case s.th.DegradedP95 > 0 && p99 >= s.th.DegradedP95:
			worsen(provider.Degraded, ReasonLatencyHigh)
		}
	}
	state := derived
	if s.reported != nil {
		switch r := s.reported(); r {
		case provider.Disabled:
			reasons[ReasonOperatorDisabled] = struct{}{}
			state = provider.Disabled
		case provider.Unhealthy:
			reasons[ReasonReportedUnhealthy] = struct{}{}
			state = provider.Worst(state, r)
		case provider.Degraded:
			reasons[ReasonReportedDegraded] = struct{}{}
			state = provider.Worst(state, r)
		}
	}
	codes := make([]string, 0, len(reasons))
	for r := range reasons {
		codes = append(codes, r)
	}
	sort.Strings(codes)
	return HealthSample{
		ID: id.New[healthSampleKind]().String(), Provider: providerName, Role: role, DataSourceID: s.source, State: state,
		ErrorRateBPS: errBPS, P50LatencyMS: p50.Milliseconds(), P99LatencyMS: p99.Milliseconds(), StalenessMS: staleness.Milliseconds(),
		WindowMS: s.th.Window.Milliseconds(), SampleCount: n, ReasonCodes: codes, EvaluatorVersion: s.version, EvaluatedAt: now,
	}, nil
}

// percentile is the nearest-rank percentile of sorted durations (0 when
// empty).
func percentile(sorted []time.Duration, pct int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted)*pct + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

// PgHealthStore persists and reads provider_health_samples.
type PgHealthStore struct {
	// MaxSampleAge is how old the latest sample may be before the state is
	// reported UNHEALTHY with NO_RECENT_SAMPLE (default 5 minutes).
	MaxSampleAge time.Duration
}

var _ HealthReader = PgHealthStore{}

func (s PgHealthStore) maxAge() time.Duration {
	if s.MaxSampleAge <= 0 {
		return 5 * time.Minute
	}
	return s.MaxSampleAge
}

// Validate checks a sample against the table constraints.
func (h HealthSample) Validate() error {
	fields := map[string]any{}
	if h.Provider == "" {
		fields["provider"] = "required"
	}
	if !oneOf(h.Role, RoleData, RoleObservation, RoleExecution, RoleFunding, RoleWallet, RoleModel) {
		fields["role"] = "unknown"
	}
	if h.State.Rank() < 0 || !oneOf(string(h.State), string(provider.Healthy), string(provider.Degraded), string(provider.Unhealthy), string(provider.Disabled)) {
		fields["state"] = "unknown"
	}
	if h.ErrorRateBPS < 0 || h.ErrorRateBPS > 10_000 {
		fields["error_rate_bps"] = "out of range"
	}
	if h.P50LatencyMS < 0 || h.P99LatencyMS < 0 || h.StalenessMS < 0 || h.SampleCount < 0 {
		fields["latency"] = "must not be negative"
	}
	if h.WindowMS <= 0 {
		fields["window_ms"] = "must be > 0"
	}
	if h.EvaluatorVersion == "" {
		fields["evaluator_version"] = "required"
	}
	if h.EvaluatedAt.IsZero() {
		fields["evaluated_at"] = "required"
	}
	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "reality: invalid health sample").WithFields(fields)
	}
	return nil
}

// Insert appends a sample (rows are immutable).
func (PgHealthStore) Insert(ctx context.Context, q db.Querier, h HealthSample) error {
	if err := h.Validate(); err != nil {
		return err
	}
	if h.ID == "" {
		h.ID = id.New[healthSampleKind]().String()
	}
	codes := h.ReasonCodes
	if codes == nil {
		codes = []string{}
	}
	_, err := q.Exec(ctx, `INSERT INTO provider_health_samples (id, provider, role, data_source_id, state, error_rate_bps, p50_latency_ms, p99_latency_ms,
		staleness_ms, window_ms, sample_count, reason_codes, evaluator_version, evaluated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		h.ID, h.Provider, h.Role, nilIfEmpty(h.DataSourceID), string(h.State), h.ErrorRateBPS, h.P50LatencyMS, h.P99LatencyMS,
		h.StalenessMS, h.WindowMS, h.SampleCount, codes, h.EvaluatorVersion, h.EvaluatedAt.UTC())
	if err != nil {
		if db.IsCheckViolation(err) {
			return errs.Wrap(err, errs.CodeValidationFailed, "reality: health sample violates a constraint").WithField("constraint", db.ConstraintName(err))
		}
		return errs.Wrap(err, errs.CodeInternal, "reality: insert health sample")
	}
	return nil
}

// Latest returns the newest sample of (provider, role).
func (PgHealthStore) Latest(ctx context.Context, q db.Querier, providerName, role string) (HealthSample, error) {
	var h HealthSample
	var sid id.ID[healthSampleKind]
	var dsid *id.ID[dataSourceKind]
	var state string
	err := q.QueryRow(ctx, `SELECT id, provider, role, data_source_id, state, error_rate_bps, p50_latency_ms, p99_latency_ms, staleness_ms, window_ms,
		sample_count, reason_codes, evaluator_version, evaluated_at FROM provider_health_samples WHERE provider = $1 AND role = $2
		ORDER BY evaluated_at DESC, id DESC LIMIT 1`, providerName, role).Scan(
		&sid, &h.Provider, &h.Role, &dsid, &state, &h.ErrorRateBPS, &h.P50LatencyMS, &h.P99LatencyMS, &h.StalenessMS, &h.WindowMS,
		&h.SampleCount, &h.ReasonCodes, &h.EvaluatorVersion, &h.EvaluatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HealthSample{}, errs.New(errs.CodeNotFound, "reality: no health sample").WithField("provider", providerName).WithField("role", role)
		}
		return HealthSample{}, errs.Wrap(err, errs.CodeInternal, "reality: load health sample")
	}
	h.ID, h.State, h.EvaluatedAt = sid.String(), provider.Health(state), h.EvaluatedAt.UTC()
	if dsid != nil {
		h.DataSourceID = dsid.String()
	}
	return h, nil
}

// Current implements HealthReader: the latest sample decides, and a sample
// older than MaxSampleAge (or none at all) is UNHEALTHY because silence is
// not evidence of health.
func (s PgHealthStore) Current(ctx context.Context, q db.Querier, providerName, role string, now time.Time) (HealthState, error) {
	h, err := s.Latest(ctx, q, providerName, role)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return HealthState{Provider: providerName, Role: role, State: provider.Unhealthy, ReasonCodes: []string{ReasonNoSamples}, Usable: false}, nil
		}
		return HealthState{}, err
	}
	age := now.Sub(h.EvaluatedAt)
	st := HealthState{Provider: providerName, Role: role, State: h.State, EvaluatedAt: h.EvaluatedAt, SampleAge: age, ReasonCodes: append([]string(nil), h.ReasonCodes...)}
	if age > s.maxAge() {
		st.State = provider.Worst(st.State, provider.Unhealthy)
		st.ReasonCodes = append(st.ReasonCodes, ReasonNoRecentSample)
		sort.Strings(st.ReasonCodes)
	}
	st.Usable = st.State.AllowsNewActions()
	return st, nil
}
