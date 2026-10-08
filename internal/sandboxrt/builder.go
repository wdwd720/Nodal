package sandboxrt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

// sdkSources are the SDK files compiled into every strategy. They are listed
// one by one -- not globbed -- so a test file can never be embedded and a new
// SDK file is a deliberate change (TestBuilder_EmbedsEverySDKSourceFile).
//
//go:embed sdk/doc.go sdk/decimal.go sdk/indicators.go sdk/input.go sdk/session.go
var sdkSources embed.FS

// strategyGoVersion is the go directive of every strategy module. The
// toolchain must be at least this version (GOTOOLCHAIN=local never
// downloads one).
const strategyGoVersion = "1.27"

// Build failure reasons.
const (
	BuildFailureScan    = "SCAN_REJECTED"
	BuildFailureCompile = "COMPILE_FAILED"
	BuildFailureTimeout = "BUILD_TIMEOUT"
)

// BuilderConfig configures a Builder.
type BuilderConfig struct {
	// GoBinary is the absolute path of the go command.
	GoBinary string
	// CacheDir, when set, is a dedicated GOCACHE shared by builds (the cache
	// is content-addressed, so sharing it cannot change a build's output).
	// When empty every build gets a fresh cache inside its own directory.
	CacheDir string
	// TempDir is where build directories are created (os.TempDir() when
	// empty).
	TempDir string
	// Timeout bounds one build (default 2 minutes).
	Timeout time.Duration
	// MaxLogBytes bounds the captured build log (default 64 KiB).
	MaxLogBytes int
	// Limits bounds the source tree (DefaultScanLimits when zero).
	Limits ScanLimits
	// Clock stamps builds (clock.System when nil).
	Clock clock.Clock
}

// Builder compiles scanned strategy source into a wasip1 artifact.
type Builder struct {
	cfg        BuilderConfig
	sdk        Source
	sdkVersion string
}

// Build is the outcome of building a source tree. On failure it still
// carries the source digest, the scan report, the failure reason and the
// build log digest, so the refusal can be recorded.
type Build struct {
	SourceDigest     []byte
	ArtifactDigest   []byte
	Wasm             []byte
	ToolchainVersion string
	SDKVersion       string
	BuildLogDigest   []byte
	// BuildLog is the (bounded) compiler output, kept for the author.
	BuildLog []byte
	Scan     ScanReport
	Failure  string
	BuiltAt  time.Time
	Duration time.Duration
}

// NewBuilder validates the configuration and loads the embedded SDK.
func NewBuilder(cfg BuilderConfig) (*Builder, error) {
	if cfg.GoBinary == "" || !filepath.IsAbs(cfg.GoBinary) {
		return nil, errs.New(errs.CodeValidationFailed, "sandboxrt: the go binary must be an absolute path")
	}
	if st, err := os.Stat(cfg.GoBinary); err != nil || st.IsDir() {
		return nil, errs.New(errs.CodeValidationFailed, "sandboxrt: the go binary does not exist").WithField("go", cfg.GoBinary)
	}
	if cfg.CacheDir != "" && !filepath.IsAbs(cfg.CacheDir) {
		return nil, errs.New(errs.CodeValidationFailed, "sandboxrt: the build cache must be an absolute path")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.MaxLogBytes <= 0 {
		cfg.MaxLogBytes = 64 << 10
	}
	if cfg.Limits == (ScanLimits{}) {
		cfg.Limits = DefaultScanLimits()
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System()
	}
	sdk, err := EmbeddedSDK()
	if err != nil {
		return nil, err
	}
	return &Builder{cfg: cfg, sdk: sdk, sdkVersion: SDKVersionOf(sdk)}, nil
}

// EmbeddedSDK returns the SDK sources compiled into every strategy.
func EmbeddedSDK() (Source, error) {
	out := Source{}
	err := fs.WalkDir(sdkSources, "sdk", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := sdkSources.ReadFile(p)
		if err != nil {
			return err
		}
		out[path.Base(p)] = b
		return nil
	})
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "sandboxrt: read embedded SDK")
	}
	return out, nil
}

// SDKVersionOf names an SDK by its content: nodalsdk/v1@sha256:<digest>.
func SDKVersionOf(sdk Source) string {
	return "nodalsdk/v1@sha256:" + hex.EncodeToString(sdk.Digest())
}

// SDKVersion is the version of the embedded SDK.
func (b *Builder) SDKVersion() string { return b.sdkVersion }

// Build scans the tree and, only if the scan is clean, compiles it. This is
// the only exported way to produce an artifact.
func (b *Builder) Build(ctx context.Context, src Source) (Build, error) {
	rep := Scan(src, b.cfg.Limits)
	if !rep.OK() {
		return Build{SourceDigest: rep.SourceDigest, Scan: rep, Failure: BuildFailureScan, SDKVersion: b.sdkVersion, BuiltAt: b.cfg.Clock.Now()},
			errs.New(errs.CodeStrategyRejected, "sandboxrt: the strategy source was refused by the scanner").
				WithField("scan_codes", rep.Codes()).
				WithField("violations", rep.Violations)
	}
	return b.compile(ctx, src, rep)
}

// compile builds a tree in a fresh directory. It does not scan: the only
// production caller is Build, after Scan (TestBuilder_CompileIsReachableOnlyThroughScan).
func (b *Builder) compile(ctx context.Context, src Source, rep ScanReport) (Build, error) {
	out := Build{SourceDigest: src.Digest(), Scan: rep, SDKVersion: b.sdkVersion, BuiltAt: b.cfg.Clock.Now()}
	sw := clock.Start()
	defer func() { out.Duration = sw.Elapsed() }()
	for name := range src {
		if name != filepath.Base(name) || name == "." || name == ".." || name == "go.mod" || name == "go.sum" || strings.ContainsAny(name, `/\`) {
			return out, errs.New(errs.CodeValidationFailed, "sandboxrt: source file names must be plain file names").WithField("file", name)
		}
	}
	root, err := os.MkdirTemp(b.cfg.TempDir, "nodal-strategy-build-")
	if err != nil {
		return out, errs.Wrap(err, errs.CodeInternal, "sandboxrt: create build directory")
	}
	defer func() { _ = os.RemoveAll(root) }()
	srcDir, sdkDir, outDir := filepath.Join(root, "src"), filepath.Join(root, "sdk"), filepath.Join(root, "out")
	for _, d := range []string{srcDir, sdkDir, outDir, filepath.Join(root, "home"), filepath.Join(root, "gopath")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return out, errs.Wrap(err, errs.CodeInternal, "sandboxrt: create build directory")
		}
	}
	if err := writeTree(sdkDir, b.sdk); err != nil {
		return out, err
	}
	if err := os.WriteFile(filepath.Join(sdkDir, "go.mod"), []byte("module "+SDKModulePath+"\n\ngo "+strategyGoVersion+"\n"), 0o600); err != nil {
		return out, errs.Wrap(err, errs.CodeInternal, "sandboxrt: write SDK go.mod")
	}
	if err := writeTree(srcDir, src); err != nil {
		return out, err
	}
	gomod := "module strategy\n\ngo " + strategyGoVersion + "\n\nrequire " + SDKModulePath + " v0.0.0\n\nreplace " + SDKModulePath + " => ../sdk\n"
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte(gomod), 0o600); err != nil {
		return out, errs.Wrap(err, errs.CodeInternal, "sandboxrt: write go.mod")
	}

	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	env := b.environment(root)

	timedOut := func() (Build, error) {
		out.Failure = BuildFailureTimeout
		return out, errs.New(errs.CodeStrategyRejected, "sandboxrt: the build exceeded its time limit").
			WithField("reason", BuildFailureTimeout)
	}
	version, err := b.goCommand(ctx, srcDir, env, b.cfg.MaxLogBytes, "env", "GOVERSION")
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return timedOut()
		}
		return out, errs.Wrap(err, errs.CodeInternal, "sandboxrt: read toolchain version")
	}
	out.ToolchainVersion = strings.TrimSpace(string(version))

	wasmPath := filepath.Join(outDir, "strategy.wasm")
	log, buildErr := b.goCommand(ctx, srcDir, env, b.cfg.MaxLogBytes, "build", "-o", wasmPath, ".")
	sum := sha256.Sum256(log)
	out.BuildLogDigest, out.BuildLog = sum[:], log
	if buildErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return timedOut()
		}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		out.Failure = BuildFailureCompile
		return out, errs.New(errs.CodeStrategyRejected, "sandboxrt: the strategy does not compile").
			WithField("reason", BuildFailureCompile).
			WithField("build_log", truncate(string(log), 4096))
	}
	wasm, err := os.ReadFile(wasmPath) // #nosec G304 -- a path inside the build directory this function created
	if err != nil {
		return out, errs.Wrap(err, errs.CodeInternal, "sandboxrt: read the built module")
	}
	digest := sha256.Sum256(wasm)
	out.Wasm, out.ArtifactDigest = wasm, digest[:]
	return out, nil
}

// environment is the complete environment of every go command the builder
// runs. Nothing is inherited from the parent process: a secret in the
// parent's environment is not in it (TestBuilder_EnvironmentCarriesNothingFromTheParent).
//
// Beyond the toolchain settings, GOWORK=off stops a go.work file in any
// parent directory of the build directory from redirecting module
// resolution; it removes behaviour and adds none.
func (b *Builder) environment(root string) []string {
	cache := filepath.Join(root, "gocache")
	if b.cfg.CacheDir != "" {
		cache = b.cfg.CacheDir
	}
	return []string{
		"PATH=" + filepath.Dir(b.cfg.GoBinary),
		"GOOS=wasip1",
		"GOARCH=wasm",
		"CGO_ENABLED=0",
		"GOFLAGS=-mod=mod -trimpath",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOCACHE=" + cache,
		"GOPATH=" + filepath.Join(root, "gopath"),
		"HOME=" + filepath.Join(root, "home"),
	}
}

// BuildEnvironmentKeys returns the names of every variable a build sees.
func BuildEnvironmentKeys() []string {
	keys := []string{"CGO_ENABLED", "GOARCH", "GOCACHE", "GOFLAGS", "GOOS", "GOPATH", "GOPROXY", "GOSUMDB", "GOTOOLCHAIN", "GOWORK", "HOME", "PATH"}
	sort.Strings(keys)
	return keys
}

func (b *Builder) goCommand(ctx context.Context, dir string, env []string, maxLog int, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, b.cfg.GoBinary, args...) // #nosec G204 -- the configured toolchain with fixed arguments
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = nil
	log := &cappedBuffer{max: maxLog}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.WaitDelay = 5 * time.Second
	configureProcessGroup(cmd)
	err := cmd.Run()
	return log.bytes(), err
}

func writeTree(dir string, src Source) error {
	for _, name := range src.Paths() {
		if err := os.WriteFile(filepath.Join(dir, name), src[name], 0o600); err != nil {
			return errs.Wrap(err, errs.CodeInternal, "sandboxrt: write source file")
		}
	}
	return nil
}

// cappedBuffer keeps the first max bytes written to it.
type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}
