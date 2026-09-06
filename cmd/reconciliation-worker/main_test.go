package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lookupNothing(string) (string, bool) { return "", false }

func TestRun_UsageErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"reconcile-everything"}},
		{"full without an account", []string{"full"}},
		{"full with two accounts", []string{"full", "a", "b"}},
		{"sweep with arguments", []string{"sweep", "extra"}},
		{"verify with arguments", []string{"verify", "extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			assert.Equal(t, exitUsage, run(tc.args, lookupNothing, &out, &errOut))
		})
	}
}

func TestRun_HelpIsSuccessAndDocumentsEveryCommand(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"help", "-h", "--help"} {
		var out, errOut bytes.Buffer
		require.Equal(t, exitOK, run([]string{arg}, lookupNothing, &out, &errOut))
		text := out.String()
		for _, want := range []string{"run", "sweep", "verify", "full", envPeriodicInterval, envBatch} {
			assert.Contains(t, text, want, "help must document %q", want)
		}
	}
}

func TestDurationVar(t *testing.T) {
	t.Parallel()
	lookup := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			v, ok := m[k]
			return v, ok
		}
	}
	assert.Equal(t, time.Minute, durationVar(lookup(nil), "X", time.Minute))
	assert.Equal(t, 30*time.Second, durationVar(lookup(map[string]string{"X": "30s"}), "X", time.Minute))
	assert.Equal(t, time.Minute, durationVar(lookup(map[string]string{"X": "  "}), "X", time.Minute),
		"blank falls back to the default")
	assert.Equal(t, time.Minute, durationVar(lookup(map[string]string{"X": "nonsense"}), "X", time.Minute),
		"an unparseable interval falls back rather than failing the worker")
	assert.Equal(t, time.Minute, durationVar(lookup(map[string]string{"X": "-5s"}), "X", time.Minute),
		"a non-positive interval falls back")
}

func TestIntVar(t *testing.T) {
	t.Parallel()
	lookup := func(v string, ok bool) func(string) (string, bool) {
		return func(string) (string, bool) { return v, ok }
	}
	assert.Equal(t, 100, intVar(lookup("", false), "X", 100))
	assert.Equal(t, 25, intVar(lookup("25", true), "X", 100))
	assert.Equal(t, 100, intVar(lookup("0", true), "X", 100))
	assert.Equal(t, 100, intVar(lookup("-3", true), "X", 100))
	assert.Equal(t, 100, intVar(lookup("many", true), "X", 100))
}

func TestUsage_MentionsThatKillSwitchesDoNotStopReconciliation(t *testing.T) {
	t.Parallel()
	// The package documentation is where an operator learns the PART 52 rule;
	// this keeps it from being deleted silently.
	var out bytes.Buffer
	require.Equal(t, exitOK, run([]string{"help"}, lookupNothing, &out, &bytes.Buffer{}))
	assert.True(t, strings.Contains(out.String(), "reconciliation-worker"))
}
