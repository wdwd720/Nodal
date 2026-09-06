package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/reality"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestRun_UsageErrors(t *testing.T) {
	cases := [][]string{
		nil,
		{"bogus"},
		{"run", "extra"},
		{"verify", "extra"},
		{"gaps", "extra"},
		{"schema", "extra"},
		{"sources", "extra"},
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		code := run(args, env(nil), &out, &errb)
		assert.Equal(t, exitUsage, code, "%v", args)
		assert.Contains(t, errb.String(), "usage: market-ingest-worker", "%v", args)
		assert.NotContains(t, errb.String(), "postgres://", "connection strings are never printed")
	}
	var out, errb bytes.Buffer
	assert.Equal(t, exitOK, run([]string{"help"}, env(nil), &out, &errb))
	assert.Contains(t, out.String(), "verify")
}

func TestRun_ConfigErrorsAreRuntimeFailures(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"sources"}, env(map[string]string{"CP_ENV": "NOWHERE"}), &out, &errb)
	assert.Equal(t, exitFailure, code)
	assert.Contains(t, errb.String(), "unknown environment")
}

func TestUsage_NamesEveryCommandAndVariable(t *testing.T) {
	var b bytes.Buffer
	usage(&b)
	for _, want := range []string{
		"run", "schema", "sources", "gaps", "verify",
		envDataSource, envWallets, envConsumer, envTopic, envFeatureLatency, envPipelineLatency, envHeartbeat, envVerifyLimit,
	} {
		assert.True(t, strings.Contains(b.String(), want), want)
	}
}

// The availability policy is the only place this process sets
// decision_available_at latencies, so its parsing is worth pinning: a
// negative latency would make a datum look knowable before it was received.
func TestAvailabilityPolicy_DefaultsAndValidation(t *testing.T) {
	d := &deps{lookup: env(nil)}
	p, err := d.availabilityPolicy()
	require.NoError(t, err)
	assert.Equal(t, defaultPipelineLat, p.PipelineLatency)
	assert.Equal(t, time.Duration(0), p.FeatureLatency)
	assert.Equal(t, availabilityPolicyVersion, p.Version)

	d = &deps{lookup: env(map[string]string{envPipelineLatency: " 750ms ", envFeatureLatency: "2s"})}
	p, err = d.availabilityPolicy()
	require.NoError(t, err)
	assert.Equal(t, 750*time.Millisecond, p.PipelineLatency)
	assert.Equal(t, 2*time.Second, p.FeatureLatency)

	// A negative latency is not silently clamped to itself: it falls back to
	// the default, and the policy that reaches the normalizer always validates.
	d = &deps{lookup: env(map[string]string{envPipelineLatency: "-1s"})}
	p, err = d.availabilityPolicy()
	require.NoError(t, err)
	assert.Equal(t, defaultPipelineLat, p.PipelineLatency)

	// An explicit zero is honored: it means "no pipeline latency", not "unset".
	d = &deps{lookup: env(map[string]string{envPipelineLatency: "0s"})}
	p, err = d.availabilityPolicy()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), p.PipelineLatency)
}

func TestWindow_LooksBackFromNow(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	d := &deps{lookup: env(nil)}
	w := d.window(now)
	assert.Equal(t, now, w.End)
	assert.Equal(t, now.Add(-defaultWindow), w.Start)

	d = &deps{lookup: env(map[string]string{envWindow: "15m"})}
	w = d.window(now)
	assert.Equal(t, now.Add(-15*time.Minute), w.Start)
}

func TestVars(t *testing.T) {
	assert.Equal(t, defaultDataSource, stringVar(env(nil), envDataSource, defaultDataSource))
	assert.Equal(t, "x.y", stringVar(env(map[string]string{envDataSource: "  x.y "}), envDataSource, defaultDataSource))
	assert.Equal(t, defaultDataSource, stringVar(env(map[string]string{envDataSource: "   "}), envDataSource, defaultDataSource))

	assert.Nil(t, listVar(env(nil), envWallets))
	assert.Equal(t, []string{"A", "B"}, listVar(env(map[string]string{envWallets: " A , ,B "}), envWallets))

	assert.Equal(t, reality.DefaultVerifyLimit, intVar(env(nil), envVerifyLimit, reality.DefaultVerifyLimit))
	assert.Equal(t, 5, intVar(env(map[string]string{envVerifyLimit: "5"}), envVerifyLimit, reality.DefaultVerifyLimit))
	for _, bad := range []string{"0", "-3", "many"} {
		assert.Equal(t, reality.DefaultVerifyLimit, intVar(env(map[string]string{envVerifyLimit: bad}), envVerifyLimit, reality.DefaultVerifyLimit), bad)
	}
}

func TestStatsAttrs_OmitsZeroInstant(t *testing.T) {
	a := statsAttrs(reality.Stats{Ingested: 3, Duplicates: 1})
	assert.Equal(t, int64(3), a["ingested"])
	assert.Nil(t, a["last_ingest_at"])
	at := time.Date(2026, 9, 6, 1, 2, 3, 0, time.UTC)
	a = statsAttrs(reality.Stats{LastIngestAt: at})
	got, ok := a["last_ingest_at"].(*time.Time)
	require.True(t, ok)
	require.NotNil(t, got)
	assert.Equal(t, at, *got)
}
