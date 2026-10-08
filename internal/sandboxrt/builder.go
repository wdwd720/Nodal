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
// bound (time, output, disk, memory). None is ever a reason to try again.
const (
	BuildFailureScan    = "SCAN_REJECTED"
	BuildFailureCompile = "COMPILE_FAILED"
	BuildFailureTimeout = "BUILD_TIMEOUT"
	// BuildFailureOutputLimit: the build wrote more than it may -- the module
	// it produced (or was producing) is larger than MaxArtifactBytes, or its
	// directory (its build cache, the toolchain's temporary files and the
	// module) grew past MaxDiskBytes.
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
	MaxBuildLogBytesCap     = 64 << 10
	defaultArtifactBytes    = 32 << 20
	defaultBuildMemory      = 1 << 30
	minBuildMemory          = 64 << 20
	defaultBuildDisk        = 512 << 20
	minBuildDisk            = 64 << 20
	defaultConcurrentBuilds = 2
	defaultSampleEvery      = 100 * time.Millisecond
)

// BuilderConfig configures a Builder. Every bound has a safe default; a bound
// outside its policy range is refused rather than clamped.
type BuilderConfig struct {
	// GoBinary is the absolute path of the go command.
	GoBinary string
	// CacheDir, when set, is where the builder keeps its SEED: the standard
	// library and the SDK compiled for the strategy target once, by the
	// builder itself, from a program of this package (warmupSource) -- never
	// from a strategy. A build never writes to it: every build starts from a
	// private copy of the seed in its own directory, and the copy goes when
	// the build does. A seed is built aside and renamed into place, so it is
	// complete and never changes once it can be read; it is named for the
	// toolchain and the SDK it was built with (seed-<digest>). Anything a
	// previous builder left at the top of CacheDir in the Go cache's own
	// layout -- a cache every build shared and nothing ever cleaned -- is
	// removed when a seed is published. When CacheDir is empty every build
	// starts from an empty cache.
	CacheDir string
	// TempDir is the builder's work area (os.TempDir() when empty). Every
	// build -- its sources, its build cache, the toolchain's temporary files
	// (GOTMPDIR and TMPDIR point into it) and its module -- lives in one
	// directory created there, which is removed when the build ends,
	// whatever the outcome; a directory a process left behind when it died
	// is removed by the next builder that starts there.
	TempDir string
	// MaxDiskBytes bounds one build directory: its sources, its copy of the
	// seed, the cache entries and temporary files the toolchain writes and
	// the module (default 512 MiB, at least 64 MiB). It is measured while the
	// build runs and again when it ends; a build past it is stopped and
	// refused (BUILD_OUTPUT_LIMIT). A seed larger than a quarter of it is not
	// copied: such a build starts cold rather than short of room.
	MaxDiskBytes int64
	// MaxConcurrentBuilds bounds how many builds run at once (default 2, at
	// least 1). A build waits for a slot, for as long as its context allows,
	// before it creates anything, so the work area never holds more than
	// MaxConcurrentBuilds build directories of at most MaxDiskBytes each. A
	// directory that could not be removed keeps its slot.
	MaxConcurrentBuilds int
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
	// slots holds one token per running build (MaxConcurrentBuilds).
	slots chan struct{}

	// seedMu serializes finding or building the seed; seedTried is set once
	// an attempt finished (with or without a seed), so a builder whose seed
	// cannot be built builds cold instead of trying again for every build.
	seedMu    sync.Mutex
	seedTried bool
	seed      *seedCache
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
	if cfg.TempDir == "" {
		cfg.TempDir = os.TempDir()
	}
	if !filepath.IsAbs(cfg.TempDir) {
		return nil, errs.New(errs.CodeValidationFailed, "sandboxrt: the build work area must be an absolute path")
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
	if cfg.MaxDiskBytes == 0 {
		cfg.MaxDiskBytes = defaultBuildDisk
	}
	if cfg.MaxConcurrentBuilds == 0 {
		cfg.MaxConcurrentBuilds = defaultConcurrentBuilds
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
	case cfg.MaxDiskBytes < minBuildDisk:
		return nil, errs.Newf(errs.CodeValidationFailed, "sandboxrt: the build disk bound is at least %d bytes", minBuildDisk)
	case cfg.MaxConcurrentBuilds < 1:
		return nil, errs.New(errs.CodeValidationFailed, "sandboxrt: at least one build must be able to run")
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
	// Build directories a process left when it died are this work area's
	// only leftovers; nobody else would ever remove them.
	sweepAbandonedBuilds(cfg.TempDir)
	return &Builder{cfg: cfg, sdk: sdk, sdkVersion: SDKVersionOf(sdk), slots: make(chan struct{}, cfg.MaxConcurrentBuilds)}, nil
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
	for name := range src {
		if name != filepath.Base(name) || name == "." || name == ".." || name == "go.mod" || name == "go.sum" || strings.ContainsAny(name, `/\`) {
			return out, errs.New(errs.CodeValidationFailed, "sandboxrt: source file names must be plain file names").WithField("file", name)
		}
	}
	slot, err := b.acquire(ctx)
	if err != nil {
		return out, err
	}
	defer slot.release()
	return b.buildIn(ctx, slot, out, src, b.seedFor(ctx, slot), nil)
}

// buildIn builds a tree in a fresh directory of the work area, under the
// caller's slot, starting from a copy of seed (nil: an empty cache). It does
// not scan: its callers are compile, whose only production caller is Build
// after Scan, and warm, which builds the package's own warm-up program after
// scanning it (TestBuilder_CompileIsReachableOnlyThroughScan). after, when
// set, runs once the build succeeded and before the directory goes. The
// directory and everything in it are removed when buildIn returns, whatever
// happened; one that cannot be removed keeps the slot.
func (b *Builder) buildIn(ctx context.Context, slot *buildSlot, out Build, src Source, seed *seedCache,
	after func(root string, built Build) error) (res Build, resErr error) {
	sw := clock.Start()
	defer func() { res.Duration = sw.Elapsed() }()
	root, lock, err := newBuildDir(b.cfg.TempDir)
	if err != nil {
		return out, err
	}
	defer closeBuildDir(root, lock, slot)
	srcDir, sdkDir, outDir, cacheDir := filepath.Join(root, "src"), filepath.Join(root, "sdk"), filepath.Join(root, "out"), filepath.Join(root, "gocache")
	if err := prepareBuildDir(root); err != nil {
		return out, err
	}
	for _, d := range []string{srcDir, sdkDir, outDir} {
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
	// The seed is used only when the toolchain that built it is the one
	// building now and it leaves the build three quarters of its room; a
	// copy that fails part-way is discarded. Either way the build is the
	// same, only slower: the cache is content-addressed.
	if seed != nil && seed.version == out.ToolchainVersion && seed.bytes <= b.cfg.MaxDiskBytes/4 {
		if _, err := copyTree(cacheDir, seed.dir, b.cfg.MaxDiskBytes/4); err != nil {
			if removeTree(cacheDir) != nil || os.MkdirAll(cacheDir, 0o700) != nil {
				return out, errs.Wrap(err, errs.CodeInternal, "sandboxrt: reset the build cache after a failed seed copy")
			}
		}
	}

	wasmPath := filepath.Join(outDir, "strategy.wasm")
	watch := &buildWatch{output: wasmPath, maxOutput: b.cfg.MaxArtifactBytes, dir: root, maxDisk: b.cfg.MaxDiskBytes,
		maxMemory: b.cfg.MaxMemoryBytes, every: b.cfg.SampleInterval}
	log, peak, buildErr := b.goCommand(ctx, srcDir, env, watch, "build", "-o", wasmPath, ".")
	log = sanitizeBuildLog(log, root)
	sum := sha256.Sum256(log)
	out.BuildLogDigest, out.BuildLog, out.PeakMemoryBytes = sum[:], log, peak
	// A bound the build went past is the strategy's refusal whatever else
	// happened: the memory bound is judged both while the build runs and on
	// the peak the finished processes report, the disk bound while it runs
	// and on what it left.
	tripped, trippedOn := watch.tripped()
	switch {
	case tripped == BuildFailureMemoryLimit || peak > b.cfg.MaxMemoryBytes:
		return refuse(BuildFailureMemoryLimit, fmt.Sprintf("the build held more than its %d bytes of memory", b.cfg.MaxMemoryBytes))
	case tripped == BuildFailureOutputLimit && trippedOn == watchDisk, dirSize(root) > b.cfg.MaxDiskBytes:
		return refuse(BuildFailureOutputLimit, fmt.Sprintf("the build wrote more than its %d bytes of disk", b.cfg.MaxDiskBytes))
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
	if after != nil {
		if err := after(root, out); err != nil {
			return out, err
		}
	}
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
//
// Everything the toolchain writes stays in the build directory: the build
// cache (GOCACHE, the build's own, seeded from a copy), the module and
// configuration directories under GOPATH and HOME, and its temporary files
// (GOTMPDIR for the go command's work directory, TMPDIR for anything that
// asks the operating system's default). Nothing is left anywhere else once
// the directory is removed.
func (b *Builder) environment(root string) []string { return buildEnvironment(b.cfg.GoBinary, root) }

func buildEnvironment(goBinary, root string) []string {
	return []string{
		"PATH=" + filepath.Dir(goBinary),
		"GOOS=wasip1",
		"GOARCH=wasm",
		"CGO_ENABLED=0",
		"GOFLAGS=-mod=mod -trimpath",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOCACHE=" + filepath.Join(root, "gocache"),
		"GOPATH=" + filepath.Join(root, "gopath"),
		"HOME=" + filepath.Join(root, "home"),
		"GOTMPDIR=" + filepath.Join(root, "tmp"),
		"TMPDIR=" + filepath.Join(root, "tmp"),
	}
}

// BuildEnvironmentKeys returns the names of every variable a build sees.
func BuildEnvironmentKeys() []string {
	keys := []string{"CGO_ENABLED", "GOARCH", "GOCACHE", "GOFLAGS", "GOOS", "GOPATH", "GOPROXY", "GOSUMDB", "GOTMPDIR", "GOTOOLCHAIN",
		"GOWORK", "HOME", "PATH", "TMPDIR"}
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
// the build is writing, everything in the build's directory and the resident
// memory of the build's whole process group (the go command, its compilers
// and its linker), and the first bound passed stops the group and is
// remembered as the refusal.
type buildWatch struct {
	output    string
	maxOutput int64
	dir       string
	maxDisk   int64
	maxMemory uint64
	every     time.Duration

	mu     sync.Mutex
	reason string
	on     string
}

// What a watch measured when it tripped.
const (
	watchModule = "module"
	watchDisk   = "disk"
	watchMemory = "memory"
)

var (
	errBuildOutputLimit = errors.New("sandboxrt: build output limit reached")
	errBuildDiskLimit   = errors.New("sandboxrt: build disk limit reached")
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
			w.trip(BuildFailureOutputLimit, watchModule)
			stop(errBuildOutputLimit)
			return
		}
		if w.dir != "" && dirSize(w.dir) > w.maxDisk {
			w.trip(BuildFailureOutputLimit, watchDisk)
			stop(errBuildDiskLimit)
			return
		}
		if rss, ok := groupRSS(ctx, pgid); ok && rss > w.maxMemory {
			w.trip(BuildFailureMemoryLimit, watchMemory)
			stop(errBuildMemoryLimit)
			return
		}
	}
}

func (w *buildWatch) trip(reason, on string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reason == "" {
		w.reason, w.on = reason, on
	}
}

// tripped returns the refusal the watch recorded and what it measured.
func (w *buildWatch) tripped() (reason, on string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason, w.on
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
