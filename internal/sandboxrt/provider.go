package sandboxrt

import (
	"context"
	"strings"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
)

// SandboxRuntimeProvider is the isolated runtime user strategy code runs in
// and the builder that turns a scanned source tree into what it runs: the
// external system the strategy sandbox depends on (advanced goal §27). The
// Service runs code only through its Engine half; the marketplace builds only
// through its Build half.
//
// Its promises, which TestContract_LocalRuntimeKeepsTheRuntimeContract checks
// for the local runtime:
//
//   - Run gives the code WASI preview1 and nothing else: no filesystem, no
//     environment, no network, no real clock or randomness; it is closed at
//     its wall-time limit and bounded in memory and output. The same
//     artifact and input give the same output bytes and run digest. What
//     the code does is a RawRun with a Termination; an error is only a
//     malformed request, a closed runtime or a caller that went away.
//   - Build compiles only source the scanner passed (Builder.Build), and its
//     artifact is the bytes its digest names; a refused, failed or
//     unbuildable source is never an artifact. A provider that cannot build
//     refuses with UNSUPPORTED and builds nothing.
//   - Nothing the code does reaches the database, a key or a signer: a run's
//     only output is a document the Service interprets.
type SandboxRuntimeProvider interface {
	Engine
	// Build scans src and, only if the scan is clean, compiles it into an
	// artifact the runtime can run.
	Build(ctx context.Context, src Source) (Build, error)
	// Mode is the provider mode the runtime was built for.
	Mode() config.ProviderMode
	// Name names the runtime and its version; a validation records it.
	Name() string
	// Close releases the runtime; Run fails afterwards.
	Close(ctx context.Context) error
}

// RuntimeName names the runtime LocalRuntime runs strategies in: the wazero
// version this binary links (TestRuntimeName_IsTheLinkedWazero).
const RuntimeName = "wazero/v1.12.0"

// RuntimeConfig configures the local runtime.
type RuntimeConfig struct {
	// Runner configures the wazero runtime.
	Runner RunnerConfig
	// Builder is the local toolchain builder (NewBuilder). Nil runs without
	// one: Build then refuses with UNSUPPORTED, saying NoBuildReason.
	Builder *Builder
	// NoBuildReason says why there is no builder; required without one.
	NoBuildReason string
}

// LocalRuntime is the strategy sandbox in this process: the wazero Runner
// and, where a Go toolchain is available, the local Builder. Its mode is
// sandbox because it is the sandbox itself -- the isolation boundary ADR-0020
// decision 4 describes, every limit enforced for real -- not a stand-in for a
// runtime somewhere else (doc.go, "Provider").
type LocalRuntime struct {
	runner  *Runner
	builder *Builder
	noBuild string
}

func newLocalRuntime(ctx context.Context, cfg RuntimeConfig) (*LocalRuntime, error) {
	reason := strings.TrimSpace(cfg.NoBuildReason)
	switch {
	case cfg.Builder != nil && reason != "":
		return nil, errs.New(errs.CodeValidationFailed, "sandboxrt: a runtime with a builder has no reason not to build")
	case cfg.Builder == nil && reason == "":
		return nil, errs.New(errs.CodeValidationFailed, "sandboxrt: a runtime without a builder says why it has none")
	}
	runner, err := NewRunner(ctx, cfg.Runner)
	if err != nil {
		return nil, err
	}
	return &LocalRuntime{runner: runner, builder: cfg.Builder, noBuild: reason}, nil
}

// Run implements Engine.
func (r *LocalRuntime) Run(ctx context.Context, req RunRequest) (RawRun, error) {
	return r.runner.Run(ctx, req)
}

// Build implements SandboxRuntimeProvider.
func (r *LocalRuntime) Build(ctx context.Context, src Source) (Build, error) {
	if r.builder == nil {
		return Build{}, errs.New(errs.CodeUnsupported,
			"sandboxrt: strategies cannot be built in this deployment: "+r.noBuild+"; the version is unchanged")
	}
	return r.builder.Build(ctx, src)
}

// Mode implements SandboxRuntimeProvider.
func (r *LocalRuntime) Mode() config.ProviderMode { return config.ProviderModeSandbox }

// Name implements SandboxRuntimeProvider.
func (r *LocalRuntime) Name() string { return RuntimeName }

// Close implements SandboxRuntimeProvider.
func (r *LocalRuntime) Close(ctx context.Context) error { return r.runner.Close(ctx) }

// Stable reasons for refusing a runtime.
const (
	// ReasonLiveRuntimeUnsupported: USER_CODE_SANDBOX has no LIVE mode
	// (gates.AdvancedSpecs). No environment, flag or configuration changes
	// that.
	ReasonLiveRuntimeUnsupported = "SANDBOX_RUNTIME_LIVE_UNSUPPORTED"
	// ReasonProviderModeNotAllowed: a fake provider never runs where the
	// environment refuses fakes (STAGING and PROD).
	ReasonProviderModeNotAllowed = "SANDBOX_RUNTIME_PROVIDER_MODE_NOT_ALLOWED"
	// ReasonProviderModeUnimplemented: this build has no fake runtime; the
	// local runtime is the sandbox.
	ReasonProviderModeUnimplemented = "SANDBOX_RUNTIME_PROVIDER_MODE_UNIMPLEMENTED"
)

// NewRuntimeProvider returns the strategy runtime for a provider mode in an
// environment. Live is refused unconditionally, before the environment is
// even looked at; fake is refused where the environment refuses fakes, and
// everywhere else because no fake runtime exists; sandbox is the local
// runtime, in every environment -- in STAGING and PROD the USER_CODE_SANDBOX
// gate still has to be ACTIVE before code reaches it (gates.DecideMode). A
// refusal builds nothing and leaves nothing to close.
func NewRuntimeProvider(ctx context.Context, env config.Environment, mode config.ProviderMode, cfg RuntimeConfig) (SandboxRuntimeProvider, error) {
	if mode == config.ProviderModeLive {
		return nil, errs.New(errs.CodeUnsupported, "sandboxrt: no live strategy runtime exists in this build").
			WithField("reason", ReasonLiveRuntimeUnsupported)
	}
	if !mode.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "sandboxrt: unknown provider mode %q", mode)
	}
	if !env.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "sandboxrt: unknown environment %q", env)
	}
	if mode == config.ProviderModeFake {
		if !env.AllowsFakeProviders() {
			return nil, errs.Newf(errs.CodeForbidden, "sandboxrt: provider mode fake is not allowed in environment %s", env).
				WithField("reason", ReasonProviderModeNotAllowed)
		}
		return nil, errs.New(errs.CodeUnsupported, "sandboxrt: there is no fake strategy runtime; the local runtime is the sandbox").
			WithField("reason", ReasonProviderModeUnimplemented)
	}
	return newLocalRuntime(ctx, cfg)
}

// RuntimeModeFor is the provider mode a binary builds the strategy runtime in
// when no configuration names one: sandbox, in every environment. The local
// runtime is the only one, and it is the sandbox; there is no fake to choose
// where fakes are allowed, and live is refused everywhere.
func RuntimeModeFor(config.Environment) config.ProviderMode { return config.ProviderModeSandbox }

// moded is an engine that says which provider mode it runs in.
type moded interface{ Mode() config.ProviderMode }
