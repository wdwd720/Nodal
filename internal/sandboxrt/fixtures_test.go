package sandboxrt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// buildTrustedForTest compiles source WITHOUT scanning it.
//
// It exists only in this _test.go file, so no production code can reach it:
// the only exported way to build is Builder.Build, which scans first
// (TestBuilder_CompileIsReachableOnlyThroughScan). It is here so that each
// escape test below runs code the scanner would have refused -- os, net,
// os/exec, //go:wasmimport -- and proves that the RUNTIME refuses it too,
// independently of the scanner.
func (b *Builder) buildTrustedForTest(ctx context.Context, src Source) (Build, error) {
	return b.compile(ctx, src, ScanReport{SourceDigest: src.Digest(), Files: len(src), TotalBytes: src.TotalBytes()})
}

// fixtureSet is every adversarial and well-behaved fixture, compiled once.
type fixtureSet struct {
	builder  *Builder
	builds   map[string]Build
	err      error
	goBinary string
	tempDir  string
	// runner is shared by the tests so each fixture compiles once.
	runner *Runner
}

// The fixtures are compiled once, on first use, so tests that need no module
// (and fuzz workers) never pay for a compile; tests read them, never write
// them.
var (
	fixturesOnce sync.Once
	fixtures     fixtureSet
	fixturesDir  string
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "sandboxrt-fixtures-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandboxrt: temp dir:", err)
		return 1
	}
	fixturesDir = dir
	closeDB, err := setupDatabase()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandboxrt: database:", err)
		_ = os.RemoveAll(dir)
		return 1
	}
	code := m.Run()
	closeDB()
	if fixtures.runner != nil {
		_ = fixtures.runner.Close(context.Background())
	}
	_ = os.RemoveAll(dir)
	return code
}

func loadFixtures() fixtureSet {
	fixturesOnce.Do(func() {
		fixtures = compileFixtures(fixturesDir)
		if fixtures.err != nil {
			return
		}
		runner, err := NewRunner(context.Background(), RunnerConfig{ModuleCacheSize: 32})
		if err != nil {
			fixtures.err = err
			return
		}
		fixtures.runner = runner
	})
	return fixtures
}

func compileFixtures(dir string) fixtureSet {
	set := fixtureSet{builds: map[string]Build{}, tempDir: dir}
	goBin, err := exec.LookPath("go")
	if err != nil {
		set.err = fmt.Errorf("the go toolchain is not on PATH (%w): runtime fixtures cannot be compiled", err)
		return set
	}
	if goBin, err = filepath.Abs(goBin); err != nil {
		set.err = err
		return set
	}
	set.goBinary = goBin
	b, err := NewBuilder(BuilderConfig{GoBinary: goBin, CacheDir: filepath.Join(dir, "gocache"), TempDir: dir, Timeout: 5 * time.Minute})
	if err != nil {
		set.err = err
		return set
	}
	set.builder = b
	entries, err := os.ReadDir(filepath.Join("testdata", "fixtures"))
	if err != nil {
		set.err = err
		return set
	}
	ctx := context.Background()
	type outcome struct {
		name  string
		build Build
		err   error
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	// Four builds at a time: the cache is shared and content-addressed, so
	// concurrent builds only save time.
	results := make(chan outcome, len(names))
	slots := make(chan struct{}, 4)
	for _, name := range names {
		go func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			src, err := loadSource(filepath.Join("testdata", "fixtures", name))
			if err != nil {
				results <- outcome{name: name, err: err}
				return
			}
			build, err := b.buildTrustedForTest(ctx, src)
			results <- outcome{name: name, build: build, err: err}
		}()
	}
	for range names {
		o := <-results
		if o.err != nil && set.err == nil {
			set.err = fmt.Errorf("fixture %s: %w\n%s", o.name, o.err, o.build.BuildLog)
		}
		set.builds[o.name] = o.build
	}
	return set
}

func loadSource(dir string) (Source, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	src := Source{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- test fixtures
		if err != nil {
			return nil, err
		}
		src[e.Name()] = b
	}
	return src, nil
}

// requireFixtures skips with an explicit message when the toolchain is
// missing and fails loudly when a fixture did not compile.
// requireGoModules is requireFixtures for tests that EXECUTE a Go-built
// module. Under -race they are skipped: wazero's compiler is plain Go, and
// the race detector makes compiling one Go-built module (2-5 MB of wasm) take
// about two minutes, so the suite cannot finish. They run in every other
// mode, including the integration run. What the race detector is for -- the
// host side's concurrency: the module cache and its reference-counted
// eviction, the tripwire, the bounded writers, the metered memory -- is raced
// with hand-assembled modules that compile in milliseconds
// (TestRunner_ConcurrentHandmadeRunsShareNothing).
func requireGoModules(t *testing.T) fixtureSet {
	t.Helper()
	if raceDetector {
		t.Skip("skipped under -race: compiling a Go-built module with wazero takes minutes there; " +
			"the host-side concurrency is raced by TestRunner_ConcurrentHandmadeRunsShareNothing")
	}
	return requireFixtures(t)
}

func requireFixtures(t *testing.T) fixtureSet {
	t.Helper()
	f := loadFixtures()
	if f.goBinary == "" {
		t.Skipf("sandbox runtime tests need the go toolchain: %v", f.err)
	}
	require.NoError(t, f.err)
	return f
}

func (f fixtureSet) artifact(t *testing.T, name string) Artifact {
	t.Helper()
	b, ok := f.builds[name]
	if !ok {
		names := make([]string, 0, len(f.builds))
		for n := range f.builds {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("no fixture %q (have %v)", name, names)
	}
	return Artifact{Digest: b.ArtifactDigest, Wasm: b.Wasm}
}
