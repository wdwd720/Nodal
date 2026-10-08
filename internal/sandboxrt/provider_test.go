package sandboxrt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
)

func runtimeReason(t *testing.T, err error) any {
	t.Helper()
	e, ok := errs.As(err)
	require.True(t, ok, "%v", err)
	return e.Fields["reason"]
}

// TestNewRuntimeProvider_RefusesLiveEverywhereAndFakeWhereItIsNotAllowed is
// the constructor's whole table, every environment against every mode: live
// is refused in every environment (USER_CODE_SANDBOX has no LIVE), fake is
// refused where the environment refuses fakes and, where it allows them,
// because no fake runtime exists; sandbox is the local runtime everywhere.
func TestNewRuntimeProvider_RefusesLiveEverywhereAndFakeWhereItIsNotAllowed(t *testing.T) {
	ctx := context.Background()
	cfg := RuntimeConfig{NoBuildReason: "no toolchain in this test"}
	for _, env := range append(config.Environments(), config.Environment("NOWHERE")) {
		for _, mode := range []config.ProviderMode{config.ProviderModeFake, config.ProviderModeSandbox, config.ProviderModeLive, "carrier-pigeon"} {
			t.Run(string(env)+"/"+string(mode), func(t *testing.T) {
				p, err := NewRuntimeProvider(ctx, env, mode, cfg)
				switch {
				case mode == config.ProviderModeLive:
					assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err), "live, even where the environment is unknown")
					assert.Equal(t, ReasonLiveRuntimeUnsupported, runtimeReason(t, err))
				case !mode.IsValid(), !env.IsValid():
					assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
				case mode == config.ProviderModeFake && !env.AllowsFakeProviders():
					assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
					assert.Equal(t, ReasonProviderModeNotAllowed, runtimeReason(t, err))
				case mode == config.ProviderModeFake:
					assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
					assert.Equal(t, ReasonProviderModeUnimplemented, runtimeReason(t, err))
				default:
					require.NoError(t, err)
					assert.Equal(t, config.ProviderModeSandbox, p.Mode())
					assert.Equal(t, RuntimeName, p.Name())
					// Without a builder, building is refused and builds nothing.
					b, err := p.Build(ctx, Source{"main.go": []byte(cleanStrategy)})
					assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
					assert.Contains(t, err.Error(), "no toolchain in this test")
					assert.Empty(t, b.Wasm)
					require.NoError(t, p.Close(ctx))
				}
				if err != nil {
					assert.Nil(t, p, "a refusal builds nothing, and leaves nothing to close")
				}
			})
		}
		if env.IsValid() {
			p, err := NewRuntimeProvider(ctx, env, RuntimeModeFor(env), cfg)
			require.NoError(t, err, "%s: the derived mode is one this environment runs", env)
			require.NoError(t, p.Close(ctx))
		}
	}

	// A runtime says why it cannot build, or it builds; never both, never
	// neither.
	_, err := NewRuntimeProvider(ctx, config.EnvLocal, config.ProviderModeSandbox, RuntimeConfig{})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	b, err := NewBuilder(BuilderConfig{GoBinary: fakeGo(t, "exit 1"), TempDir: t.TempDir()})
	require.NoError(t, err)
	_, err = NewRuntimeProvider(ctx, config.EnvLocal, config.ProviderModeSandbox, RuntimeConfig{Builder: b, NoBuildReason: "both"})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// TestContract_LocalRuntimeKeepsTheRuntimeContract runs the local runtime --
// built the way the binaries build it -- through every promise of
// SandboxRuntimeProvider: it builds only scanned source, into an artifact that
// is its bytes; it runs that artifact the same way every time; the code it
// runs has no environment, no filesystem and no network, is closed at its
// wall-time limit, and is never run from bytes that are not the digest's; and
// once closed it runs nothing.
func TestContract_LocalRuntimeKeepsTheRuntimeContract(t *testing.T) {
	fx := requireGoModules(t)
	ctx := context.Background()
	p, err := NewRuntimeProvider(ctx, config.EnvTest, RuntimeModeFor(config.EnvTest), RuntimeConfig{Builder: fx.builder})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	run := func(a Artifact, limits Limits) Result {
		t.Helper()
		raw, err := p.Run(ctx, RunRequest{Artifact: a, Input: testInput(), Limits: limits})
		require.NoError(t, err)
		res, err := Interpret(raw, limits)
		require.NoError(t, err)
		return res
	}

	// Build: scanned source only, and the artifact is its bytes.
	built, err := p.Build(ctx, Source{"main.go": []byte(cleanStrategy)})
	require.NoError(t, err, "%s", built.BuildLog)
	sum := sha256.Sum256(built.Wasm)
	assert.Equal(t, sum[:], built.ArtifactDigest)
	refused, err := p.Build(ctx, withImport("os"))
	assert.Equal(t, errs.CodeStrategyRejected, errs.CodeOf(err))
	assert.Equal(t, BuildFailureScan, refused.Failure)
	assert.Empty(t, refused.Wasm, "a refused source is never an artifact")

	// Run: what it built runs, the same way every time.
	artifact := Artifact{Digest: built.ArtifactDigest, Wasm: built.Wasm}
	first, second := run(artifact, testLimits()), run(artifact, testLimits())
	assert.Equal(t, TerminationOK, first.Termination, "detail=%s", first.Detail)
	assert.Equal(t, first.Stdout, second.Stdout)
	assert.Equal(t, first.RunDigest, second.RunDigest)

	// Isolation, through the provider: nothing of the environment, the
	// filesystem or the network.
	const secret = "nodal-provider-contract-secret"
	t.Setenv("NODAL_FAKE_SECRET", secret)
	env := run(fx.artifact(t, "env_secret"), testLimits())
	require.Equal(t, TerminationOK, env.Termination, "detail=%s", env.Detail)
	require.Len(t, env.Logs, 1)
	assert.Equal(t, "0", env.Logs[0].Fields["environ_count"])
	assert.False(t, bytes.Contains(env.Stdout, []byte(secret)) || bytes.Contains(env.Stderr, []byte(secret)))
	requireAllFailed(t, attempts(t, run(fx.artifact(t, "fs_escape"), testLimits())),
		"read_etc_passwd", "list_root", "list_cwd", "relative_traversal", "write_tmp", "list_getwd", "stat_root", "fd3_preopen")
	_, statErr := os.Stat("/tmp/nodal-sandbox-escape")
	assert.True(t, os.IsNotExist(statErr))
	requireAllFailed(t, attempts(t, run(fx.artifact(t, "network"), testLimits())),
		"dial_tcp", "dial_tcp_by_name", "udp_exchange", "accept_inbound", "dns_lookup", "inherited_socket")

	// Bounded: a module that never returns is closed at its wall-time limit.
	short := testLimits()
	short.WallTimeMS = 300
	loop := run(fx.artifact(t, "cpu_loop"), short)
	assert.Equal(t, TerminationTimeout, loop.Termination)
	assert.Less(t, loop.Usage.WallTime, 3*time.Second)

	// Bytes that are not the digest's are never run as the digest's.
	tampered := append([]byte(nil), built.Wasm...)
	tampered[len(tampered)/2] ^= 0xff
	bad, err := p.Run(ctx, RunRequest{Artifact: Artifact{Digest: built.ArtifactDigest, Wasm: tampered}, Input: testInput(), Limits: testLimits()})
	require.NoError(t, err)
	assert.Equal(t, TerminationInvalidModule, bad.Termination)
	assert.False(t, bad.ArtifactVerified)

	// Closed, it runs nothing.
	require.NoError(t, p.Close(ctx))
	_, err = p.Run(ctx, RunRequest{Artifact: artifact, Input: testInput(), Limits: testLimits()})
	assert.Error(t, err)
}

// TestRuntimeName_IsTheLinkedWazero: the runtime a validation records is the
// wazero this binary links.
func TestRuntimeName_IsTheLinkedWazero(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	require.True(t, ok)
	for _, dep := range info.Deps {
		if dep.Path == "github.com/tetratelabs/wazero" {
			assert.Equal(t, "wazero/"+dep.Version, RuntimeName)
			return
		}
	}
	t.Fatal("wazero is not linked into this binary")
}

// liveEngine claims a mode no runtime may have.
type liveEngine struct{ Engine }

func (liveEngine) Mode() config.ProviderMode { return config.ProviderModeLive }

// TestNewService_RequiresTheBudgetsConnectionsAndRefusesALiveRuntime: a
// service without the budget's own connections, or around a runtime that
// says it is live, is never built.
func TestNewService_RequiresTheBudgetsConnectionsAndRefusesALiveRuntime(t *testing.T) {
	clk, auth, aw := unitDeps(t)
	assert.PanicsWithValue(t, "sandboxrt: NewService requires a clock, an authorizer, an engine, an audit writer and the budget's connections",
		func() { NewService(clk, auth, scripted{}, aw, nil) })
	assert.Panics(t, func() { NewService(clk, auth, liveEngine{scripted{}}, aw, refusingConns{}) })
	assert.True(t, strings.Contains(func() (msg string) {
		defer func() { msg, _ = recover().(string) }()
		NewService(clk, auth, liveEngine{scripted{}}, aw, refusingConns{})
		return ""
	}(), ReasonLiveRuntimeUnsupported))
	assert.NotPanics(t, func() { NewService(clk, auth, scripted{}, aw, refusingConns{}) })
}
