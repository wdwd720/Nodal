package sandboxrt

import (
	"bytes"
	"context"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	insA = "0193b2e0-0000-7000-8000-00000000000a"
	insB = "0193b2e0-0000-7000-8000-00000000000b"
)

func testInput() Input {
	return Input{
		AsOf: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
		Instruments: []InputInstrument{
			{InstrumentID: insA, Kind: "SPOT_PAIR", BaseDecimals: 6, QuoteDecimals: 6, Mark: "150.25", BestBid: "150.20", BestAsk: "150.30",
				Closes: []string{"140.00", "142.50", "145.00", "147.50", "150.00", "152.50"}},
			{InstrumentID: insB, Kind: "EQUITY", BaseDecimals: 0, QuoteDecimals: 2, Mark: "410.10",
				Closes: []string{"420.00", "418.00", "415.00", "412.00", "410.00"}},
		},
		Portfolio: InputPortfolio{Cash: "100000.00", Positions: []InputPosition{{InstrumentID: insA, Quantity: "2000000"}}},
		Config:    map[string]string{"order_units": "1000000"},
	}
}

func testLimits() Limits {
	l := DefaultLimits()
	l.Version = "sandbox-limits/test"
	return l
}

func runFixture(t *testing.T, name string, in Input, limits Limits) Result {
	t.Helper()
	f := requireGoModules(t)
	raw, err := f.runner.Run(context.Background(), RunRequest{Artifact: f.artifact(t, name), Input: in, Limits: limits})
	require.NoError(t, err)
	res, err := Interpret(raw, limits)
	require.NoError(t, err)
	return res
}

// attempts indexes an adversarial fixture's "attempt" log records by name.
func attempts(t *testing.T, res Result) map[string]map[string]string {
	t.Helper()
	require.Equal(t, TerminationOK, res.Termination, "detail=%s stderr=%s", res.Detail, res.Stderr)
	out := map[string]map[string]string{}
	for _, l := range res.Logs {
		if l.Event == "attempt" {
			out[l.Fields["name"]] = l.Fields
		}
	}
	return out
}

func requireAllFailed(t *testing.T, got map[string]map[string]string, names ...string) {
	t.Helper()
	require.Len(t, got, len(names), "every attempt must be reported")
	for _, n := range names {
		a, ok := got[n]
		require.True(t, ok, "attempt %s not reported", n)
		assert.Equal(t, "false", a["ok"], "attempt %s succeeded inside the sandbox", n)
		assert.NotEmpty(t, a["error"], "attempt %s failed without an error", n)
	}
}

func TestSandbox_FilesystemEscapeAttemptFails(t *testing.T) {
	res := runFixture(t, "fs_escape", testInput(), testLimits())
	requireAllFailed(t, attempts(t, res),
		"read_etc_passwd", "list_root", "list_cwd", "relative_traversal", "write_tmp", "list_getwd", "stat_root", "fd3_preopen")
	_, err := os.Stat("/tmp/nodal-sandbox-escape")
	assert.True(t, os.IsNotExist(err), "the strategy wrote to the host filesystem")
}

func TestSandbox_NetworkAttemptFails(t *testing.T) {
	res := runFixture(t, "network", testInput(), testLimits())
	requireAllFailed(t, attempts(t, res),
		"dial_tcp", "dial_tcp_by_name", "udp_exchange", "accept_inbound", "dns_lookup", "inherited_socket")
}

func TestSandbox_ForkBombBlocked(t *testing.T) {
	t.Run("processes cannot be started", func(t *testing.T) {
		res := runFixture(t, "fork_process", testInput(), testLimits())
		requireAllFailed(t, attempts(t, res), "start_process", "exec_command", "look_path")
	})
	t.Run("unbounded goroutines hit the memory limit", func(t *testing.T) {
		limits := testLimits()
		limits.MemoryLimitPages = 256 // 16 MiB
		res := runFixture(t, "goroutine_bomb", testInput(), limits)
		assert.Equal(t, TerminationMemoryLimit, res.Termination, "detail=%s", res.Detail)
		assert.LessOrEqual(t, res.Usage.PeakMemoryBytes, limits.MemoryLimitBytes())
		assert.Less(t, res.Usage.WallTime, limits.WallTime(), "stopped by the memory limit, before the clock")
		assert.Empty(t, res.Proposals)
	})
}

func TestSandbox_CPUInfiniteLoopKilled(t *testing.T) {
	limits := testLimits()
	limits.WallTimeMS = 300
	res := runFixture(t, "cpu_loop", testInput(), limits)
	assert.Equal(t, TerminationTimeout, res.Termination, "detail=%s", res.Detail)
	// Usage.WallTime is the run alone, compilation excluded.
	assert.GreaterOrEqual(t, res.Usage.WallTime, 300*time.Millisecond)
	assert.Less(t, res.Usage.WallTime, 3*time.Second, "an infinite loop must be killed at the deadline, not run on")
}

func TestSandbox_MemoryBombKilled(t *testing.T) {
	limits := testLimits()
	limits.MemoryLimitPages = 256 // 16 MiB
	for _, fixture := range []string{"memory_bomb", "memory_huge"} {
		t.Run(fixture, func(t *testing.T) {
			res := runFixture(t, fixture, testInput(), limits)
			assert.Equal(t, TerminationMemoryLimit, res.Termination, "detail=%s", res.Detail)
			assert.LessOrEqual(t, res.Usage.PeakMemoryBytes, limits.MemoryLimitBytes(), "physical memory never exceeds the limit")
			assert.Contains(t, res.Detail, "refused")
		})
	}
}

func TestSandbox_OversizedOutputKilled(t *testing.T) {
	limits := testLimits()
	limits.MaxOutputBytes = 4096
	limits.MaxStderrBytes = 2048
	t.Run("stdout", func(t *testing.T) {
		res := runFixture(t, "output_flood", testInput(), limits)
		assert.Equal(t, TerminationOutputLimit, res.Termination, "detail=%s", res.Detail)
		assert.Equal(t, int64(4096), res.Usage.OutputBytes, "exactly the limit is kept, nothing more")
		assert.Empty(t, res.Proposals)
	})
	t.Run("stderr", func(t *testing.T) {
		res := runFixture(t, "stderr_flood", testInput(), limits)
		assert.Equal(t, TerminationOutputLimit, res.Termination, "detail=%s", res.Detail)
		assert.Equal(t, int64(2048), res.Usage.StderrBytes)
	})
}

func TestSandbox_EnvironmentSecretsAbsent(t *testing.T) {
	const secret = "nodal-fake-secret-7b9c3e1f0a"
	t.Setenv("NODAL_FAKE_SECRET", secret)
	require.Equal(t, secret, os.Getenv("NODAL_FAKE_SECRET"), "the secret is in the test process environment")

	res := runFixture(t, "env_secret", testInput(), testLimits())
	require.Equal(t, TerminationOK, res.Termination, "detail=%s stderr=%s", res.Detail, res.Stderr)
	require.Len(t, res.Logs, 1)
	f := res.Logs[0].Fields
	assert.Equal(t, "", f["secret"])
	assert.Equal(t, "false", f["secret_found"])
	assert.Equal(t, "0", f["environ_count"], "the module has no environment at all")
	assert.Equal(t, "strategy", f["args"], "the only argument is the program name")
	assert.False(t, bytes.Contains(res.Stdout, []byte(secret)) || bytes.Contains(res.Stderr, []byte(secret)))
}

func TestSandbox_StrategyCannotConstructALiveSigningCall(t *testing.T) {
	// The protocol has two record types and neither can sign, transfer or
	// withdraw.
	assert.Equal(t, []string{RecordIntent, RecordLog}, RecordTypes())

	cases := []struct {
		mode        string
		termination Termination
		violation   ViolationCode
	}{
		{"sign_record", TerminationInvalidOutput, ViolationOutputRecordType},
		{"transfer_record", TerminationInvalidOutput, ViolationOutputRecordType},
		{"withdraw_record", TerminationInvalidOutput, ViolationOutputRecordType},
		{"intent_with_destination", TerminationInvalidOutput, ViolationOutputRecordMalformed},
		{"intent_with_signature", TerminationInvalidOutput, ViolationOutputRecordMalformed},
		{"withdraw_kind", TerminationOK, ViolationKindNotProposable},
		{"transfer_kind", TerminationOK, ViolationKindNotProposable},
		{"bridge_kind", TerminationOK, ViolationKindNotProposable},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			in := testInput()
			in.Config["mode"] = c.mode
			res := runFixture(t, "signing_attempt", in, testLimits())
			assert.Equal(t, c.termination, res.Termination, "detail=%s stderr=%s", res.Detail, res.Stderr)
			require.Len(t, res.Violations, 1)
			assert.Equal(t, c.violation, res.Violations[0].Code)
			assert.Empty(t, res.Proposals, "nothing of the attempt survives as a proposal")
		})
	}

	t.Run("a host import is refused before the module starts", func(t *testing.T) {
		res := runFixture(t, "wasm_import", testInput(), testLimits())
		assert.Equal(t, TerminationInvalidModule, res.Termination)
		assert.Contains(t, res.Detail, "nodal.sign_transaction")
		assert.Empty(t, res.Stdout)
	})
}

func TestSandbox_SameArtifactSameInputIsByteIdentical(t *testing.T) {
	f := requireGoModules(t)
	for _, fixture := range []string{"momentum", "nondeterminism"} {
		t.Run(fixture, func(t *testing.T) {
			first := runFixture(t, fixture, testInput(), testLimits())
			require.Equal(t, TerminationOK, first.Termination, "detail=%s stderr=%s", first.Detail, first.Stderr)
			require.NotEmpty(t, first.Stdout)
			for i := 0; i < 3; i++ {
				again := runFixture(t, fixture, testInput(), testLimits())
				assert.Equal(t, first.Stdout, again.Stdout, "run %d", i)
				assert.Equal(t, first.RunDigest, again.RunDigest)
			}
			// A brand-new runtime gives the same bytes: nothing carries over
			// between instances.
			fresh, err := NewRunner(context.Background(), RunnerConfig{})
			require.NoError(t, err)
			defer func() { _ = fresh.Close(context.Background()) }()
			raw, err := fresh.Run(context.Background(), RunRequest{Artifact: f.artifact(t, fixture), Input: testInput(), Limits: testLimits()})
			require.NoError(t, err)
			res, err := Interpret(raw, testLimits())
			require.NoError(t, err)
			assert.Equal(t, first.RunDigest, res.RunDigest)
			assert.Equal(t, RunDigest(first.ArtifactDigest, first.InputDigest, first.Stdout), first.RunDigest)

			// A different input is a different run.
			other := testInput()
			other.AsOf = other.AsOf.Add(time.Minute)
			changed := runFixture(t, fixture, other, testLimits())
			assert.NotEqual(t, first.RunDigest, changed.RunDigest)
		})
	}
}

func TestSandbox_ClockAndRandomnessAreDeterministic(t *testing.T) {
	in := testInput()
	res := runFixture(t, "nondeterminism", in, testLimits())
	require.Equal(t, TerminationOK, res.Termination, "detail=%s stderr=%s", res.Detail, res.Stderr)
	require.Len(t, res.Logs, 1)
	seen := res.Logs[0].Fields
	assert.True(t, strings.HasPrefix(seen["now"], "2026-10-07T12:00:00"), "the wall clock is the input's as-of time, got %s", seen["now"])
	elapsed, err := time.ParseDuration(seen["elapsed"])
	require.NoError(t, err)
	assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond, "sleep advances virtual time")

	other := testInput()
	other.AsOf = other.AsOf.Add(time.Hour)
	res2 := runFixture(t, "nondeterminism", other, testLimits())
	require.Len(t, res2.Logs, 1)
	assert.NotEqual(t, seen["crypto_rand"], res2.Logs[0].Fields["crypto_rand"], "the random source is seeded by the input")
	assert.True(t, strings.HasPrefix(res2.Logs[0].Fields["now"], "2026-10-07T13:00:00"))
}

func TestSandbox_SleepIsVirtual(t *testing.T) {
	res := runFixture(t, "sleeper", testInput(), testLimits())
	require.Equal(t, TerminationOK, res.Termination, "detail=%s stderr=%s", res.Detail, res.Stderr)
	assert.Less(t, res.Usage.WallTime, 2*time.Second, "an hour of virtual sleep costs no real time")
	require.Len(t, res.Logs, 1)
	assert.Equal(t, "1h0m0s", res.Logs[0].Fields["slept"])
}

func TestSandbox_APanicYieldsNothing(t *testing.T) {
	res := runFixture(t, "panicker", testInput(), testLimits())
	assert.Equal(t, TerminationTrap, res.Termination)
	assert.Equal(t, uint32(2), res.ExitCode)
	assert.Empty(t, res.Proposals, "what a failed run wrote before failing is not used")
}

func TestSandbox_DigestMismatchIsRefused(t *testing.T) {
	f := requireFixtures(t) // refused before compilation: runs under -race too
	a := f.artifact(t, "momentum")
	tampered := append([]byte(nil), a.Wasm...)
	tampered[len(tampered)/2] ^= 0xff
	raw, err := f.runner.Run(context.Background(), RunRequest{Artifact: Artifact{Digest: a.Digest, Wasm: tampered}, Input: testInput(), Limits: testLimits()})
	require.NoError(t, err)
	assert.Equal(t, TerminationInvalidModule, raw.Termination)
	assert.False(t, raw.ArtifactVerified)
}

func TestSandbox_AWellBehavedStrategyProposes(t *testing.T) {
	res := runFixture(t, "momentum", testInput(), testLimits())
	require.Equal(t, TerminationOK, res.Termination, "detail=%s stderr=%s", res.Detail, res.Stderr)
	assert.Empty(t, res.Violations)
	require.Len(t, res.Proposals, 1, "only instrument A trends up")
	p := res.Proposals[0]
	assert.Equal(t, insA, p.InstrumentID)
	assert.Equal(t, SideBuy, p.Side)
	assert.Equal(t, "1000000", p.Quantity.String())
	assert.Len(t, res.Logs, 2)
}

func TestRunner_ConcurrentRunsAreIsolated(t *testing.T) {
	f := requireGoModules(t)
	bomb := testLimits()
	bomb.MemoryLimitPages = 256
	var wg sync.WaitGroup
	type outcome struct {
		fixture string
		res     Result
		err     error
	}
	results := make(chan outcome, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name, limits := "momentum", testLimits()
			if i%3 == 0 {
				name, limits = "memory_bomb", bomb
			}
			raw, err := f.runner.Run(context.Background(), RunRequest{Artifact: f.artifact(t, name), Input: testInput(), Limits: limits})
			if err != nil {
				results <- outcome{fixture: name, err: err}
				return
			}
			res, err := Interpret(raw, limits)
			results <- outcome{fixture: name, res: res, err: err}
		}()
	}
	wg.Wait()
	close(results)
	var digests []string
	for o := range results {
		require.NoError(t, o.err)
		switch o.fixture {
		case "momentum":
			assert.Equal(t, TerminationOK, o.res.Termination)
			digests = append(digests, string(o.res.RunDigest))
		case "memory_bomb":
			assert.Equal(t, TerminationMemoryLimit, o.res.Termination, "one run's memory bomb is its own")
		}
	}
	sort.Strings(digests)
	require.NotEmpty(t, digests)
	assert.Equal(t, digests[0], digests[len(digests)-1], "concurrent honest runs are identical")
}

// TestRunner_ColdStartAndInvocationLatency measures what a run costs: the
// cold start (compiling the module) and the invocation latency over 60 runs
// of a compiled module. The numbers are logged; the assertions are loose
// bounds that only catch a pathological regression.
func TestRunner_ColdStartAndInvocationLatency(t *testing.T) {
	f := requireGoModules(t)
	if testing.Short() {
		t.Skip("latency measurement skipped in -short mode")
	}
	r, err := NewRunner(context.Background(), RunnerConfig{})
	require.NoError(t, err)
	defer func() { _ = r.Close(context.Background()) }()
	art := f.artifact(t, "momentum")
	limits := testLimits()

	first, err := r.Run(context.Background(), RunRequest{Artifact: art, Input: testInput(), Limits: limits})
	require.NoError(t, err)
	require.Equal(t, TerminationOK, first.Termination)
	require.False(t, first.Usage.CacheHit)
	cold := first.Usage.CompileTime

	const runs = 60
	samples := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		start := time.Now()
		raw, err := r.Run(context.Background(), RunRequest{Artifact: art, Input: testInput(), Limits: limits})
		elapsed := time.Since(start)
		require.NoError(t, err)
		require.True(t, raw.Usage.CacheHit)
		require.Equal(t, TerminationOK, raw.Termination)
		samples = append(samples, elapsed)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 := samples[len(samples)/2]
	p95 := samples[(len(samples)*95)/100]
	t.Logf("sandbox runtime: wasm %d bytes; cold start (compile) %s; invocation over %d runs: p50 %s, p95 %s, max %s",
		len(art.Wasm), cold, runs, p50, p95, samples[len(samples)-1])
	assert.Less(t, cold, 60*time.Second)
	assert.Less(t, p95, 2*time.Second)
}
