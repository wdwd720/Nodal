package sandboxrt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// Build failure reasons. Each is a refusal of the strategy, recorded as such:
// the source was refused, did not compile, or its build went past a resource
// bound (time, output, memory). None is ever a reason to try again.
const (
	BuildFailureScan    = "SCAN_REJECTED"
	BuildFailureCompile = "COMPILE_FAILED"
	BuildFailureTimeout = "BUILD_TIMEOUT"
	// BuildFailureOutputLimit: the module the build produced (or was
	// producing) is larger than MaxArtifactBytes.
	BuildFailureOutputLimit = "BUILD_OUTPUT_LIMIT"
	// BuildFailureMemoryLimit: the build's processes together held more
	// resident memory than MaxMemoryBytes.
	BuildFailureMemoryLimit = "BUILD_MEMORY_LIMIT"
)

// AllBuildFailures returns every build failure reason
// (marketplace_version_validations_failure_reason_check).
func AllBuildFailures() []string {
	return []string{BuildFailureScan, BuildFailureCompile, BuildFailureTimeout, BuildFailureOutputLimit, BuildFailureMemoryLimit}
}

// Build resource bounds.
const (
	// MaxArtifactBytesCap is the largest artifact strategy_artifacts accepts.
	MaxArtifactBytesCap = 64 << 20
	// MaxBuildLogBytesCap is the largest build log a validation stores.
	MaxBuildLogBytesCap  = 64 << 10
	defaultArtifactBytes = 32 << 20
	defaultBuildMemory   = 1 << 30
	minBuildMemory       = 64 << 20
	defaultSampleEvery   = 100 * time.Millisecond
)

// BuilderConfig configures a Builder. Every bound has a safe default; a bound
// outside its policy range is refused rather than clamped.
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
	// MaxLogBytes bounds the captured build log (default and at most 64 KiB:
	// the log is stored with the validation, for the strategy's author).
	MaxLogBytes int
	// MaxArtifactBytes bounds the built module (default 32 MiB, at most the
	// 64 MiB strategy_artifacts accepts). A build whose output grows past it
	// is stopped and refused (BUILD_OUTPUT_LIMIT); the host never reads more
	// of a module than this.
	MaxArtifactBytes int64
	// MaxMemoryBytes bounds the resident memory of the whole build: the go
	// command and every compiler and linker process it starts, measured
	// together while they run and again from the peak they report when they
	// finish (default 1 GiB, at least 64 MiB; a cold build of a strategy
	// holds about a quarter of the default). A build past it is stopped and
	// refused (BUILD_MEMORY_LIMIT).
	MaxMemoryBytes uint64
	// SampleInterval is how often a running build's memory and output are
	// measured (default 100 ms).
	SampleInterval time.Duration
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
	// BuildLog is the (bounded) compiler output, kept for the author, with
	// the build directory's path replaced by $BUILD.
	BuildLog []byte
	Scan     ScanReport
	Failure  string
	BuiltAt  time.Time
	Duration time.Duration
	// PeakMemoryBytes is the largest resident set the build's processes
	// reported (0 where the platform cannot tell).
	PeakMemoryBytes uint64
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
		cfg.MaxLogBytes = MaxBuildLogBytesCap
	}
	if cfg.MaxArtifactBytes <= 0 {
		cfg.MaxArtifactBytes = defaultArtifactBytes
	}
	if cfg.MaxMemoryBytes == 0 {
		cfg.MaxMemoryBytes = defaultBuildMemory
	}
	if cfg.SampleInterval <= 0 {
		cfg.SampleInterval = defaultSampleEvery
	}
	switch {
	case cfg.MaxLogBytes > MaxBuildLogBytesCap:
		return nil, errs.Newf(errs.CodeValidationFailed, "sandboxrt: the build log bound is at most %d bytes", MaxBuildLogBytesCap)
	case cfg.MaxArtifactBytes > MaxArtifactBytesCap:
		return nil, errs.Newf(errs.CodeValidationFailed, "sandboxrt: the artifact bound is at most %d bytes", MaxArtifactBytesCap)
	case cfg.MaxMemoryBytes < minBuildMemory:
		return nil, errs.Newf(errs.CodeValidationFailed, "sandboxrt: the build memory bound is at least %d bytes", minBuildMemory)
	case cfg.SampleInterval > time.Second:
		return nil, errs.New(errs.CodeValidationFailed, "sandboxrt: a running build is measured at least once a second")
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

	refuse := func(reason, detail string) (Build, error) {
		out.Failure = reason
		return out, errs.New(errs.CodeStrategyRejected, "sandboxrt: "+detail).WithField("reason", reason)
	}
	version, _, err := b.goCommand(ctx, srcDir, env, nil, "env", "GOVERSION")
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return refuse(BuildFailureTimeout, "the build exceeded its time limit")
		}
		return out, errs.Wrap(err, errs.CodeInternal, "sandboxrt: read toolchain version")
	}
	out.ToolchainVersion = strings.TrimSpace(string(version))

	wasmPath := filepath.Join(outDir, "strategy.wasm")
	watch := &buildWatch{output: wasmPath, maxOutput: b.cfg.MaxArtifactBytes, maxMemory: b.cfg.MaxMemoryBytes, every: b.cfg.SampleInterval}
	log, peak, buildErr := b.goCommand(ctx, srcDir, env, watch, "build", "-o", wasmPath, ".")
	log = sanitizeBuildLog(log, root)
	sum := sha256.Sum256(log)
	out.BuildLogDigest, out.BuildLog, out.PeakMemoryBytes = sum[:], log, peak
	// A bound the build went past is the strategy's refusal whatever else
	// happened: the memory bound is judged both while the build runs and on
	// the peak the finished processes report.
	switch tripped := watch.tripped(); {
	case tripped == BuildFailureMemoryLimit || peak > b.cfg.MaxMemoryBytes:
		return refuse(BuildFailureMemoryLimit, fmt.Sprintf("the build held more than its %d bytes of memory", b.cfg.MaxMemoryBytes))
	case tripped == BuildFailureOutputLimit:
		return refuse(BuildFailureOutputLimit, fmt.Sprintf("the build produced a module larger than its %d bytes", b.cfg.MaxArtifactBytes))
	case buildErr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return refuse(BuildFailureTimeout, "the build exceeded its time limit")
	case buildErr != nil && ctx.Err() != nil:
		return out, ctx.Err()
	case buildErr != nil:
		out.Failure = BuildFailureCompile
		return out, errs.New(errs.CodeStrategyRejected, "sandboxrt: the strategy does not compile").
			WithField("reason", BuildFailureCompile).
			WithField("build_log", truncate(string(log), 4096))
	}
	wasm, err := readBounded(wasmPath, b.cfg.MaxArtifactBytes)
	if errors.Is(err, errArtifactTooLarge) {
		return refuse(BuildFailureOutputLimit, fmt.Sprintf("the build produced a module larger than its %d bytes", b.cfg.MaxArtifactBytes))
	}
	if err != nil {
		return out, errs.Wrap(err, errs.CodeInternal, "sandboxrt: read the built module")
	}
	digest := sha256.Sum256(wasm)
	out.Wasm, out.ArtifactDigest = wasm, digest[:]
	return out, nil
}

var errArtifactTooLarge = errors.New("sandboxrt: artifact larger than its bound")

// readBounded reads a file that must be at most max bytes, and never reads
// more than max+1 of it.
func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- a path inside the build directory this package created
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err == nil && st.Size() > max {
		return nil, errArtifactTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errArtifactTooLarge
	}
	return b, nil
}

// sanitizeBuildLog replaces the build directory's path -- as created and as
// the toolchain may resolve it through symbolic links -- in the compiler's
// output, so the log kept for the author names files, never the host's
// layout.
func sanitizeBuildLog(log []byte, root string) []byte {
	if root == "" {
		return log
	}
	if real, err := filepath.EvalSymlinks(root); err == nil && real != root {
		log = bytes.ReplaceAll(log, []byte(real), []byte("$BUILD"))
	}
	return bytes.ReplaceAll(log, []byte(root), []byte("$BUILD"))
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

// goCommand runs the toolchain in its own process group. With a watch it
// measures the group while it runs and stops it at the watch's bounds; it
// returns the (bounded) log and the peak resident memory the finished
// processes report.
func (b *Builder) goCommand(ctx context.Context, dir string, env []string, watch *buildWatch, args ...string) ([]byte, uint64, error) {
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	cmd := exec.CommandContext(runCtx, b.cfg.GoBinary, args...) // #nosec G204 -- the configured toolchain with fixed arguments
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = nil
	log := &cappedBuffer{max: b.cfg.MaxLogBytes}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.WaitDelay = 5 * time.Second
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	if watch != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			watch.run(runCtx, cmd.Process.Pid, stop, done)
		}()
	}
	err := cmd.Wait()
	close(done)
	wg.Wait()
	return log.bytes(), peakRSS(cmd.ProcessState), err
}

// buildWatch bounds a running build: every interval it measures the module
// the build is writing and the resident memory of the build's whole process
// group (the go command, its compilers and its linker), and the first bound
// passed stops the group and is remembered as the refusal.
type buildWatch struct {
	output    string
	maxOutput int64
	maxMemory uint64
	every     time.Duration

	mu     sync.Mutex
	reason string
}

var (
	errBuildOutputLimit = errors.New("sandboxrt: build output limit reached")
	errBuildMemoryLimit = errors.New("sandboxrt: build memory limit reached")
)

func (w *buildWatch) run(ctx context.Context, pgid int, stop context.CancelCauseFunc, done <-chan struct{}) {
	ticker := time.NewTicker(w.every)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if st, err := os.Stat(w.output); err == nil && st.Size() > w.maxOutput {
			w.trip(BuildFailureOutputLimit)
			stop(errBuildOutputLimit)
			return
		}
		if rss, ok := groupRSS(ctx, pgid); ok && rss > w.maxMemory {
			w.trip(BuildFailureMemoryLimit)
			stop(errBuildMemoryLimit)
			return
		}
	}
}

func (w *buildWatch) trip(reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reason == "" {
		w.reason = reason
	}
}

func (w *buildWatch) tripped() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason
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
