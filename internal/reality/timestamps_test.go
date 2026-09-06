package reality_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/reality"
)

var fixedNow = time.Date(2026, 9, 5, 12, 0, 0, 500_000_000, time.UTC)

func genTime(rt *rapid.T, label string) time.Time {
	return time.Unix(rapid.Int64Range(1_600_000_000, 1_900_000_000).Draw(rt, label+"_sec"), rapid.Int64Range(0, 999_999_999).Draw(rt, label+"_nsec")).UTC()
}

// The invariant of PART 74: platform_received_at <= normalized_at <=
// feature_available_at <= decision_available_at, for every input including
// clocks that ran backwards and provider timestamps anywhere in time.
func TestProp_AvailabilityPolicyOrdersPlatformClocks(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		policy := reality.AvailabilityPolicy{
			FeatureLatency:  time.Duration(rapid.Int64Range(0, int64(10*time.Second)).Draw(rt, "feature")),
			PipelineLatency: time.Duration(rapid.Int64Range(0, int64(10*time.Second)).Draw(rt, "pipeline")),
		}
		received := genTime(rt, "received")
		normalized := received.Add(time.Duration(rapid.Int64Range(-int64(time.Second), int64(time.Hour)).Draw(rt, "skew")))
		in := reality.Timestamps{PlatformReceivedAt: received}
		if rapid.Bool().Draw(rt, "has_source") {
			in.SourceEventAt = genTime(rt, "source")
		}
		if rapid.Bool().Draw(rt, "has_published") {
			in.ProviderPublishedAt = genTime(rt, "published")
		}
		if rapid.Bool().Draw(rt, "has_feature") {
			in.FeatureAvailableAt = genTime(rt, "feature_at")
		}
		out, err := policy.Apply(in, normalized)
		require.NoError(rt, err)
		require.NoError(rt, out.Validate())
		require.False(rt, out.NormalizedAt.Before(out.PlatformReceivedAt))
		require.False(rt, out.FeatureAvailableAt.Before(out.NormalizedAt))
		require.False(rt, out.DecisionAvailableAt.Before(out.FeatureAvailableAt))
		require.False(rt, out.DecisionAvailableAt.Before(out.PlatformReceivedAt), "decision availability never precedes receipt")
		require.False(rt, out.DecisionAvailableAt.Before(out.PlatformReceivedAt.Add(policy.PipelineLatency)))
		// Provider clocks are stored, never used to compute availability.
		require.Equal(rt, in.ProviderPublishedAt, out.ProviderPublishedAt)
		if !in.SourceEventAt.IsZero() {
			require.Equal(rt, in.SourceEventAt, out.SourceEventAt)
		} else {
			require.Equal(rt, received, out.SourceEventAt, "unknown source time falls back to receipt (conservative)")
		}
		if !in.FeatureAvailableAt.IsZero() && in.FeatureAvailableAt.After(out.NormalizedAt.Add(policy.FeatureLatency)) {
			require.Equal(rt, in.FeatureAvailableAt, out.FeatureAvailableAt, "a later feature time set by the feature layer is kept")
		}
	})
}

func TestProp_AgeNeverShrinksWithProviderClockAhead(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		received := genTime(rt, "received")
		ahead := received.Add(time.Duration(rapid.Int64Range(0, int64(time.Hour)).Draw(rt, "ahead")))
		behind := received.Add(-time.Duration(rapid.Int64Range(0, int64(time.Hour)).Draw(rt, "behind")))
		at := received.Add(time.Duration(rapid.Int64Range(0, int64(time.Hour)).Draw(rt, "later")))
		base := reality.Timestamps{PlatformReceivedAt: received}.Age(at)
		require.Equal(rt, base, reality.Timestamps{PlatformReceivedAt: received, SourceEventAt: ahead}.Age(at), "a provider clock ahead cannot make data fresher")
		require.GreaterOrEqual(rt, reality.Timestamps{PlatformReceivedAt: received, SourceEventAt: behind}.Age(at), base, "a provider clock behind only makes data older")
	})
}

func TestAvailabilityPolicy_Rules(t *testing.T) {
	t.Parallel()
	_, err := reality.AvailabilityPolicy{PipelineLatency: -1}.Apply(reality.Timestamps{PlatformReceivedAt: fixedNow}, fixedNow)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = reality.AvailabilityPolicy{}.Apply(reality.Timestamps{}, fixedNow)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "platform_received_at is required")

	p := reality.AvailabilityPolicy{FeatureLatency: 10 * time.Millisecond, PipelineLatency: 250 * time.Millisecond}
	out, err := p.Apply(reality.Timestamps{PlatformReceivedAt: fixedNow, SourceEventAt: fixedNow.Add(-time.Minute)}, fixedNow.Add(2*time.Millisecond))
	require.NoError(t, err)
	require.Equal(t, fixedNow.Add(2*time.Millisecond), out.NormalizedAt)
	require.Equal(t, fixedNow.Add(12*time.Millisecond), out.FeatureAvailableAt)
	require.Equal(t, fixedNow.Add(262*time.Millisecond), out.DecisionAvailableAt)
	require.True(t, out.AvailableAt(out.DecisionAvailableAt))
	require.False(t, out.AvailableAt(out.DecisionAvailableAt.Add(-time.Nanosecond)), "one nanosecond early is not knowable")

	// Normalized before received (clock skew) is clamped, never earlier.
	out, err = p.Apply(reality.Timestamps{PlatformReceivedAt: fixedNow}, fixedNow.Add(-time.Second))
	require.NoError(t, err)
	require.Equal(t, fixedNow, out.NormalizedAt)
}

func TestTimestamps_ValidateRejectsDisorder(t *testing.T) {
	t.Parallel()
	good := reality.Timestamps{SourceEventAt: fixedNow, PlatformReceivedAt: fixedNow, NormalizedAt: fixedNow, FeatureAvailableAt: fixedNow, DecisionAvailableAt: fixedNow}
	require.NoError(t, good.Validate())
	cases := map[string]func(*reality.Timestamps){
		"decision before feature":  func(ts *reality.Timestamps) { ts.DecisionAvailableAt = fixedNow.Add(-1) },
		"feature before normalize": func(ts *reality.Timestamps) { ts.FeatureAvailableAt = fixedNow.Add(-1); ts.NormalizedAt = fixedNow },
		"normalize before receive": func(ts *reality.Timestamps) { ts.NormalizedAt = fixedNow.Add(-1) },
		"missing source":           func(ts *reality.Timestamps) { ts.SourceEventAt = time.Time{} },
		"non utc":                  func(ts *reality.Timestamps) { ts.DecisionAvailableAt = fixedNow.In(time.FixedZone("x", 3600)) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ts := good
			mutate(&ts)
			err := ts.Validate()
			require.Error(t, err)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}
