package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/observability"
)

func TestRun_Usage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no arguments", nil, exitUsage},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"run with arguments", []string{"run", "extra"}, exitUsage},
		{"once with a positional argument", []string{"once", "extra"}, exitUsage},
		{"once with a negative pass count", []string{"once", "-passes", "-1"}, exitUsage},
		{"once with a non-numeric pass count", []string{"once", "-passes", "many"}, exitUsage},
		{"status with a positional argument", []string{"status", "extra"}, exitUsage},
		{"status with an unknown flag", []string{"status", "-verbose"}, exitUsage},
		{"status with a negative lag threshold", []string{"status", "-max-lag", "-1s"}, exitUsage},
		{"help", []string{"help"}, exitOK},
		{"long help", []string{"--help"}, exitOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := run(tc.args, func(string) (string, bool) { return "", false }, &out, &errOut)
			assert.Equal(t, tc.want, code)
			if tc.want == exitOK {
				assert.Contains(t, out.String(), "usage: relay-worker")
			}
		})
	}
}

// A relay that publishes into an in-process buffer nobody reads is worse
// than one that does not start: it drains the outbox and the events are
// gone. The refusal must happen before any connection is opened, and it must
// name the variable an operator has to change.
func TestRun_RefusesWithoutARealBus(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{"run", "once"} {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := run([]string{cmd}, func(k string) (string, bool) {
				if k == config.EnvVarEnvironment {
					return "LOCAL", true
				}
				return "", false
			}, &out, &errOut)
			require.Equal(t, exitFailure, code)
			assert.Contains(t, errOut.String(), "no real event bus binding for provider mode \"fake\"")
			assert.Contains(t, errOut.String(), envVarEventBusMode)
			assert.Contains(t, errOut.String(), envAllowLoopback)
		})
	}
}

func TestResolveBusBinding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		env           config.Environment
		mode          config.ProviderMode
		allowLoopback bool
		want          busKind
		wantErr       string
	}{
		{name: "live is the real client", env: config.EnvProd, mode: config.ProviderModeLive, want: busRedpanda},
		{name: "sandbox is the real client", env: config.EnvStaging, mode: config.ProviderModeSandbox, want: busRedpanda},
		{name: "fake is refused by default", env: config.EnvLocal, mode: config.ProviderModeFake, wantErr: "no real event bus binding"},
		{name: "fake is allowed in LOCAL with the explicit opt-in", env: config.EnvLocal, mode: config.ProviderModeFake, allowLoopback: true, want: busLoopback},
		{name: "fake is allowed in TEST with the explicit opt-in", env: config.EnvTest, mode: config.ProviderModeFake, allowLoopback: true, want: busLoopback},
		{name: "fake is allowed in DEV with the explicit opt-in", env: config.EnvDev, mode: config.ProviderModeFake, allowLoopback: true, want: busLoopback},
		{name: "fake is refused in STAGING even with the opt-in", env: config.EnvStaging, mode: config.ProviderModeFake, allowLoopback: true, wantErr: "never permitted in STAGING"},
		{name: "fake is refused in PROD even with the opt-in", env: config.EnvProd, mode: config.ProviderModeFake, allowLoopback: true, wantErr: "never permitted in PROD"},
		{name: "an unknown mode fails closed", env: config.EnvLocal, mode: config.ProviderMode("half"), wantErr: "unknown event bus provider mode"},
		{name: "an empty mode fails closed", env: config.EnvLocal, mode: "", wantErr: "unknown event bus provider mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveBusBinding(tc.env, tc.mode, tc.allowLoopback)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, string(got))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLoadWorkerConfig_Defaults(t *testing.T) {
	t.Parallel()
	wc, err := loadWorkerConfig(func(string) (string, bool) { return "", false }, config.EnvLocal)
	require.NoError(t, err)
	assert.Equal(t, event.DefaultBatchSize, wc.Relay.BatchSize)
	assert.Equal(t, event.DefaultPollInterval, wc.Relay.PollInterval)
	assert.Equal(t, event.DefaultMaxInterval, wc.Relay.MaxInterval)
	assert.Equal(t, event.DefaultRetryBackoffBase, wc.Relay.RetryBackoffBase)
	assert.Equal(t, event.DefaultRetryBackoffMax, wc.Relay.RetryBackoffMax)
	assert.Equal(t, event.DefaultRunTimeout, wc.Relay.RunTimeout)
	assert.Equal(t, DefaultDrainTimeout, wc.Drain)
	assert.Equal(t, DefaultSampleInterval, wc.Sample)
	assert.False(t, wc.AllowLoopback)
	assert.False(t, wc.Exclusive,
		"every instance relays by default: concurrent relaying is safe since D-036, and a singleton costs drain rate and failover latency")
}

func TestLoadWorkerConfig_FromEnvironment(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		envBatchSize:        "250",
		envPollInterval:     "50ms",
		envMaxInterval:      "3s",
		envRetryBackoffBase: "2s",
		envRetryBackoffMax:  "1m",
		envRunTimeout:       "12s",
		envDrainTimeout:     "9s",
		envSampleInterval:   "0s",
		envAllowLoopback:    "true",
		envExclusive:        "true",
	}
	wc, err := loadWorkerConfig(lookupMap(env), config.EnvLocal)
	require.NoError(t, err)
	assert.Equal(t, 250, wc.Relay.BatchSize)
	assert.Equal(t, 50*time.Millisecond, wc.Relay.PollInterval)
	assert.Equal(t, 3*time.Second, wc.Relay.MaxInterval)
	assert.Equal(t, 2*time.Second, wc.Relay.RetryBackoffBase)
	assert.Equal(t, time.Minute, wc.Relay.RetryBackoffMax)
	assert.Equal(t, 12*time.Second, wc.Relay.RunTimeout)
	assert.Equal(t, 9*time.Second, wc.Drain)
	assert.Equal(t, time.Duration(0), wc.Sample, "a zero sample interval disables the sampler")
	assert.True(t, wc.AllowLoopback)
	assert.True(t, wc.Exclusive)
}

func TestLoadWorkerConfig_RejectsBadValues(t *testing.T) {
	t.Parallel()
	for name, bad := range map[string]string{
		envBatchSize:        "0",
		envPollInterval:     "-1s",
		envMaxInterval:      "soon",
		envRetryBackoffBase: "0s",
		envRetryBackoffMax:  "",
		envRunTimeout:       "12",
		envSampleInterval:   "-1s",
		envAllowLoopback:    "yes-please",
		envExclusive:        "maybe",
	} {
		if bad == "" {
			continue // an unset variable is the default, not an error
		}
		_, err := loadWorkerConfig(lookupMap(map[string]string{name: bad}), config.EnvLocal)
		assert.Error(t, err, "%s=%s", name, bad)
	}
}

// The loopback opt-in is a developer convenience and must be impossible to
// carry into a production-like environment, independently of
// config.Validate's own RuleNoFakeProviders.
func TestLoadWorkerConfig_RefusesTheLoopbackOptInInProductionLikeEnvironments(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		_, err := loadWorkerConfig(lookupMap(map[string]string{envAllowLoopback: "true"}), env)
		require.Error(t, err, "%s", env)
		assert.Contains(t, err.Error(), "never permitted in "+string(env))
	}
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		wc, err := loadWorkerConfig(lookupMap(map[string]string{envAllowLoopback: "true"}), env)
		require.NoError(t, err, "%s", env)
		assert.True(t, wc.AllowLoopback)
	}
}

// An unregistered topic is a real defect somewhere: it means a producer
// bypassed the registry or a topic was renamed under it. It must be loud and
// it must not be a reason to drop a committed financial event.
func TestTopicGuardBus_UnregisteredTopicIsLoudAndStillPublished(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	rec := &recordingBus{}
	m, err := newRelayMetrics(nil)
	require.NoError(t, err)
	guard := &topicGuardBus{inner: rec, log: observability.NewLogger(config.EnvTest, &logs), metrics: m}

	headers := map[string]string{event.HeaderEventID: "01900000-0000-7000-8000-000000000001", event.HeaderEventType: "ghost.topic"}
	require.NoError(t, guard.Publish(t.Context(), "ghost.topic", "agg-1", []byte(`{}`), headers))

	require.Len(t, rec.published, 1, "an unregistered topic is never dropped")
	assert.Equal(t, "ghost.topic", rec.published[0].topic)
	assert.Equal(t, int64(1), m.counters().unregistered)
	assert.Contains(t, logs.String(), "not in the event registry")
	assert.Contains(t, logs.String(), "ghost.topic")
	assert.Contains(t, logs.String(), `"level":"WARN"`)
}

func TestTopicGuardBus_RegisteredTopicIsSilent(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	rec := &recordingBus{}
	m, err := newRelayMetrics(nil)
	require.NoError(t, err)
	guard := &topicGuardBus{inner: rec, log: observability.NewLogger(config.EnvTest, &logs), metrics: m}

	topic := event.TopicOrderTransitioned.String()
	require.NoError(t, guard.Publish(t.Context(), topic, "order-1", []byte(`{}`), nil))
	assert.Len(t, rec.published, 1)
	assert.Zero(t, m.counters().unregistered)
	assert.Empty(t, logs.String())

	// The wrapper is transparent for the rest of the Bus contract.
	require.NoError(t, guard.Subscribe(t.Context(), topic, "g", func(context.Context, event.Message) error { return nil }))
	assert.Equal(t, []string{topic + "/g"}, rec.subscribed)
	require.NoError(t, guard.Close(t.Context()))
	assert.True(t, rec.closed)
}

// The topic attribute must stay bounded: a registered topic comes from a
// fixed set, and anything else collapses to one label so a stray row can
// never blow up metric cardinality. The exact name stays in the log line.
func TestTopicAttrIsBounded(t *testing.T) {
	t.Parallel()
	assert.Equal(t, event.TopicFillObserved.String(), topicAttr(event.TopicFillObserved.String()).Value.AsString())
	assert.Equal(t, unregisteredTopicLabel, topicAttr("ghost.topic").Value.AsString())
	assert.False(t, observability.IsHighCardinalityKey("topic"))
}

func TestRelayMetrics_ObserverCountsAttemptsAndRetries(t *testing.T) {
	t.Parallel()
	m, err := newRelayMetrics(nil)
	require.NoError(t, err)

	m.OnPublished(event.TopicOrderTransitioned.String(), 1, 5*time.Millisecond)
	m.OnPublished(event.TopicOrderTransitioned.String(), 3, -time.Second) // clock skew must not panic
	m.OnPublishFailed(event.TopicFillObserved.String(), 1, errors.New("bus down"))
	m.OnPublishFailed(event.TopicFillObserved.String(), 2, errors.New("bus down"))
	m.OnDuplicate("provider")

	c := m.counters()
	assert.Equal(t, int64(2), c.published)
	assert.Equal(t, int64(2), c.failed)
	assert.Equal(t, int64(2), c.retried, "one retried success and one retried failure")
	assert.Zero(t, c.unregistered)
}

func TestBackoffDelay(t *testing.T) {
	t.Parallel()
	base, maximum := 100*time.Millisecond, time.Second
	assert.Equal(t, base, backoffDelay(base, maximum, 1))
	assert.Equal(t, 200*time.Millisecond, backoffDelay(base, maximum, 2))
	assert.Equal(t, 400*time.Millisecond, backoffDelay(base, maximum, 3))
	assert.Equal(t, maximum, backoffDelay(base, maximum, 50), "capped, never overflowing")
	assert.Equal(t, maximum, backoffDelay(base, maximum, 1_000_000))
	assert.Equal(t, time.Duration(0), backoffDelay(0, maximum, 3))
}

func TestJitterStaysInsideItsBand(t *testing.T) {
	t.Parallel()
	const poll = time.Second
	for i := 0; i < 500; i++ {
		d := jitter(10*time.Second, poll, 20)
		assert.GreaterOrEqual(t, d, 8*time.Second)
		assert.LessOrEqual(t, d, 12*time.Second)

		idle := jitter(0, poll, 20)
		assert.GreaterOrEqual(t, idle, time.Duration(0))
		assert.LessOrEqual(t, idle, 200*time.Millisecond)
	}
	assert.Equal(t, 5*time.Second, jitter(5*time.Second, poll, 0), "no jitter configured means the delay is exact")
}

func TestSleepWithContext(t *testing.T) {
	t.Parallel()
	assert.True(t, sleepWithContext(t.Context(), 0))
	assert.True(t, sleepWithContext(t.Context(), time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.False(t, sleepWithContext(ctx, 0))
	assert.False(t, sleepWithContext(ctx, time.Hour))
}

func TestSnapshot_OldestAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, time.Duration(0), Snapshot{At: now}.OldestAge(), "a drained outbox has no lag")

	old := now.Add(-90 * time.Second)
	assert.Equal(t, 90*time.Second, Snapshot{At: now, OldestRecordedAt: &old}.OldestAge())

	ahead := now.Add(time.Second)
	assert.Equal(t, time.Duration(0), Snapshot{At: now, OldestRecordedAt: &ahead}.OldestAge(),
		"a row recorded by a clock slightly ahead reads as fresh, never as negative lag")
}

func TestStatus_WriteTextAnswersTheOperatorQuestion(t *testing.T) {
	t.Parallel()
	st := sampleStatus()
	var buf bytes.Buffer
	require.NoError(t, st.WriteText(&buf))
	out := buf.String()

	assert.Contains(t, out, "outbox depth          12 unpublished (7 eligible now, 5 waiting on a retry deadline)")
	assert.Contains(t, out, "relay lag             5m0s")
	assert.Contains(t, out, "partitions            4 with unpublished rows, 2 blocked")
	assert.Contains(t, out, "failing rows          5 (highest publish_attempts 3)")
	assert.Contains(t, out, "order.transitioned")
	assert.Contains(t, out, "ghost.topic (UNREGISTERED)")
	assert.Contains(t, out, "blocked partitions")
	assert.Contains(t, out, "order-77")
	assert.Contains(t, out, "bus unreachable")
	assert.Contains(t, out, "WARNING unpublished rows name 1 topic(s)")
	assert.Contains(t, out, "... 1 more blocked partitions not listed")
}

func TestStatus_WriteJSONIsMachineReadable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	require.NoError(t, sampleStatus().WriteJSON(&buf))

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	snap, ok := got["snapshot"].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, 12, snap["unpublished"], 0)
	assert.InDelta(t, 2, snap["blocked_partitions"], 0)
	assert.InDelta(t, 300, got["oldest_age_seconds"], 0)
	assert.Equal(t, []any{"ghost.topic"}, got["unregistered_topics"])
}

func TestOneLineFlattensAndBounds(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "a b c", oneLine("a\n  b\tc", 80))
	assert.Len(t, oneLine(strings.Repeat("x", 200), 20), 20)
}

// ---- helpers ---------------------------------------------------------------

func lookupMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

type publishedMessage struct {
	topic, key string
	headers    map[string]string
}

// recordingBus is a minimal event.Bus for the wrapper tests.
type recordingBus struct {
	published  []publishedMessage
	subscribed []string
	closed     bool
	err        error
}

func (b *recordingBus) Publish(_ context.Context, topic, key string, _ []byte, headers map[string]string) error {
	if b.err != nil {
		return b.err
	}
	b.published = append(b.published, publishedMessage{topic: topic, key: key, headers: headers})
	return nil
}

func (b *recordingBus) Subscribe(_ context.Context, topic, group string, _ event.Handler) error {
	b.subscribed = append(b.subscribed, topic+"/"+group)
	return nil
}

func (b *recordingBus) Close(context.Context) error {
	b.closed = true
	return nil
}

func sampleStatus() Status {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-5 * time.Minute)
	return Status{
		Snapshot: Snapshot{
			At: now, Unpublished: 12, Eligible: 7, Failing: 5, MaxAttempts: 3,
			OldestRecordedAt: &oldest, NextAttemptAt: &now, Partitions: 4, BlockedPartitions: 2,
		},
		OldestAgeSeconds: 300,
		Topics: []TopicStatus{
			{Topic: event.TopicOrderTransitioned.String(), Registered: true, Unpublished: 9, Eligible: 7, OldestAgeSeconds: 300},
			{Topic: "ghost.topic", Registered: false, Unpublished: 3, MaxAttempts: 3, OldestAgeSeconds: 120, LastError: "unknown topic"},
		},
		Blocked: []BlockedPartition{{
			Topic: event.TopicOrderTransitioned.String(), PartitionKey: "order-77", Depth: 5,
			HeadRecordedAt: oldest, HeadAgeSeconds: 300, Attempts: 3, NextAttemptAt: now.Add(time.Minute),
			RetryInSeconds: 60, LastError: "redpandabus: publish: bus unreachable",
		}},
		UnregisteredTopic: []string{"ghost.topic"},
	}
}
