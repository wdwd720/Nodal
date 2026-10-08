package sandboxrt

import (
	"context"
	"crypto/sha256"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// --- helpers ------------------------------------------------------------------

// realGo returns the absolute path of the go command, or skips.
func realGo(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("building a strategy needs the go command: %v", err)
	}
	goBin, err = filepath.Abs(goBin)
	require.NoError(t, err)
	return goBin
}

// treeState is every regular file under a directory, by relative path, with
// its size and digest: what "as it was" means for a directory.
func treeState(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." {
				out[rel+"/"] = "dir"
			}
			return nil
		}
		b, err := os.ReadFile(p) // #nosec G304 -- a test's own directory
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[rel] = strconv.Itoa(len(b)) + ":" + string(sum[:])
		return nil
	}))
	return out
}

// entries lists a directory's names.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(es))
	for _, e := range es {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// paddedStrategy is a clean strategy (one allowed import, no directive)
// whose package holds a global of size bytes with its last byte set: the
// compiler writes all of it into the package archive, which the go command
// puts in the build cache (review 3, F4).
func paddedStrategy(size int) Source {
	n := strconv.Itoa(size)
	return Source{"main.go": []byte(`package main

import nodalsdk "nodal.dev/sdk"

var pad = [` + n + `]byte{` + n + ` - 1: 1}

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		_ = pad[len(pad)-1]
		return nil
	})
}
`)}
}

// stubVersion is the part of every stub toolchain that answers the version
// probe.
const stubVersion = `if [ "$1" = "env" ]; then echo go1.27.1; exit 0; fi
`

// --- the work area ------------------------------------------------------------

// TestBuilder_EveryOutcomeLeavesTheWorkAreaAsItFoundIt: a build that succeeds,
// does not compile, outlives its deadline, writes a module past its bound or
// fills its directory past the disk bound leaves nothing behind -- not its
// sources, not its build cache, not the toolchain's temporary files (which it
// was told to write inside the build directory), not a read-only tree.
func TestBuilder_EveryOutcomeLeavesTheWorkAreaAsItFoundIt(t *testing.T) {
	// Every stub writes where the toolchain writes: the build cache, both
	// temporary directories, HOME and a read-only module-cache-like tree
	// under GOPATH.
	const litter = `/bin/mkdir -p "$GOCACHE/ab" "$GOTMPDIR/go-build1/b001" "$TMPDIR/x" "$HOME/.config" "$GOPATH/pkg/mod/m@v1/sub"
/bin/dd if=/dev/zero of="$GOTMPDIR/go-build1/b001/_pkg_.a" bs=1024 count=512 2>/dev/null
/bin/dd if=/dev/zero of="$GOCACHE/ab/abcd-d" bs=1024 count=512 2>/dev/null
echo x > "$TMPDIR/x/f"; echo x > "$HOME/.config/f"; echo x > "$GOPATH/pkg/mod/m@v1/sub/f"
/bin/chmod 0555 "$GOPATH/pkg/mod/m@v1/sub" "$GOPATH/pkg/mod/m@v1"
`
	cases := []struct {
		name    string
		body    string
		cfg     func(*BuilderConfig)
		failure string
	}{
		{"a module", litter + `/bin/dd if=/dev/zero of="$3" bs=1024 count=8 2>/dev/null; exit 0`, nil, ""},
		{"a compile error", litter + `echo "main.go:1: broken"; exit 1`, nil, BuildFailureCompile},
		{"a timeout", litter + `/bin/sleep 30`, func(c *BuilderConfig) { c.Timeout = 2 * time.Second }, BuildFailureTimeout},
		{"a module past its bound", litter + `/bin/dd if=/dev/zero of="$3" bs=1024 count=2048 2>/dev/null; exit 0`,
			func(c *BuilderConfig) { c.MaxArtifactBytes = 1 << 20 }, BuildFailureOutputLimit},
		{"a directory past the disk bound", litter + `/bin/dd if=/dev/zero of="$GOTMPDIR/junk" bs=1048576 count=80 2>/dev/null; /bin/sleep 30`,
			func(c *BuilderConfig) { c.MaxDiskBytes = minBuildDisk }, BuildFailureOutputLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			work := t.TempDir()
			cfg := BuilderConfig{GoBinary: fakeGo(t, stubVersion+c.body), TempDir: work, Timeout: time.Minute, SampleInterval: 20 * time.Millisecond}
			if c.cfg != nil {
				c.cfg(&cfg)
			}
			b, err := NewBuilder(cfg)
			require.NoError(t, err)
			build, err := b.Build(context.Background(), Source{"main.go": []byte(cleanStrategy)})
			if c.failure == "" {
				require.NoError(t, err, "%s", build.BuildLog)
				assert.Len(t, build.Wasm, 8<<10)
			} else {
				require.Error(t, err)
				assert.Equal(t, errs.CodeStrategyRejected, errs.CodeOf(err), "%v", err)
				assert.Equal(t, c.failure, build.Failure)
			}
			assert.Empty(t, entries(t, work), "the work area is as the build found it")
			assert.Equal(t, 0, len(b.slots), "the slot is free again")
		})
	}
}

// TestBuilder_ABuildPastItsDiskBoundIsStoppedWhileItRuns: a build whose
// directory grows past MaxDiskBytes is stopped at once -- not when its
// deadline passes -- and refused BUILD_OUTPUT_LIMIT with the disk named.
func TestBuilder_ABuildPastItsDiskBoundIsStoppedWhileItRuns(t *testing.T) {
	work := t.TempDir()
	b, err := NewBuilder(BuilderConfig{GoBinary: fakeGo(t, stubVersion+
		`/bin/dd if=/dev/zero of="$GOTMPDIR/junk" bs=1048576 count=80 2>/dev/null; /bin/sleep 60`),
		TempDir: work, Timeout: 2 * time.Minute, MaxDiskBytes: minBuildDisk, SampleInterval: 20 * time.Millisecond})
	require.NoError(t, err)
	start := time.Now()
	build, err := b.Build(context.Background(), Source{"main.go": []byte(cleanStrategy)})
	require.Error(t, err)
	assert.Less(t, time.Since(start), 30*time.Second, "stopped while it ran, long before its deadline")
	assert.Equal(t, errs.CodeStrategyRejected, errs.CodeOf(err))
	assert.Equal(t, BuildFailureOutputLimit, build.Failure)
	assert.Contains(t, err.Error(), "of disk")
	assert.Empty(t, build.Wasm)
	assert.Empty(t, entries(t, work))
}

// TestBuilder_BuildsBeyondTheSlotsWaitAndGiveUpAtCapacity: with one slot, a
// second build waits for the first; one whose context ends first did not
// start -- AT_CAPACITY, nothing created -- so the work area never holds more
// build directories than there are slots. The slot is free again afterwards.
func TestBuilder_BuildsBeyondTheSlotsWaitAndGiveUpAtCapacity(t *testing.T) {
	work := t.TempDir()
	gate := filepath.Join(t.TempDir(), "open")
	b, err := NewBuilder(BuilderConfig{GoBinary: fakeGo(t, stubVersion+
		`i=0; while [ ! -f "`+gate+`" ] && [ $i -lt 600 ]; do /bin/sleep 0.05; i=$((i+1)); done
/bin/dd if=/dev/zero of="$3" bs=1024 count=4 2>/dev/null; exit 0`),
		TempDir: work, Timeout: time.Minute, MaxConcurrentBuilds: 1})
	require.NoError(t, err)

	first := make(chan error, 1)
	go func() {
		_, err := b.Build(context.Background(), Source{"main.go": []byte(cleanStrategy)})
		first <- err
	}()
	require.Eventually(t, func() bool { return len(entries(t, work)) == 1 }, 10*time.Second, 10*time.Millisecond,
		"the first build has its directory")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	build, err := b.Build(ctx, Source{"main.go": []byte(cleanStrategy)})
	require.Error(t, err)
	assert.Equal(t, errs.CodeAtCapacity, errs.CodeOf(err), "%v", err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
	assert.Empty(t, build.Failure, "not a refusal of the strategy: it was never built")
	assert.Len(t, entries(t, work), 1, "the waiting build created nothing")

	require.NoError(t, os.WriteFile(gate, nil, 0o600))
	require.NoError(t, <-first)
	assert.Empty(t, entries(t, work))
	_, err = b.Build(context.Background(), Source{"main.go": []byte(cleanStrategy)})
	require.NoError(t, err, "the slot was given back")
}

// TestBuilder_ADirectoryThatCannotBeRemovedKeepsItsSlot: the work area's bound
// holds even when a build directory survives its build -- here a file the
// system will not let its owner remove (BSD's user-immutable flag) -- because
// that directory keeps its slot: with one slot the next build is
// AT_CAPACITY instead of adding a second directory.
func TestBuilder_ADirectoryThatCannotBeRemovedKeepsItsSlot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("needs chflags(1) and an owner-settable immutable flag")
	}
	work := t.TempDir()
	t.Cleanup(func() {
		_ = exec.Command("/usr/bin/chflags", "-R", "nouchg", work).Run() // #nosec G204 -- a test's own directory
	})
	b, err := NewBuilder(BuilderConfig{GoBinary: fakeGo(t, stubVersion+
		`echo x > "$GOTMPDIR/stuck"; /usr/bin/chflags uchg "$GOTMPDIR/stuck"; echo broken; exit 1`),
		TempDir: work, Timeout: time.Minute, MaxConcurrentBuilds: 1})
	require.NoError(t, err)
	_, err = b.Build(context.Background(), Source{"main.go": []byte(cleanStrategy)})
	require.Error(t, err)
	assert.Len(t, entries(t, work), 1, "the directory could not be removed")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = b.Build(ctx, Source{"main.go": []byte(cleanStrategy)})
	assert.Equal(t, errs.CodeAtCapacity, errs.CodeOf(err), "the stuck directory still holds the only slot: %v", err)
	assert.Len(t, entries(t, work), 1, "no second directory")
}

// TestBuilder_ADirectoryWhoseProcessDiedIsRemovedByTheNextBuilder: a build
// directory whose lock nobody holds was left by a process that died; the next
// builder to start in the work area removes it. A directory whose lock is
// held (a live build) and one with no lock file (not the builder's, or being
// created) are left alone.
func TestBuilder_ADirectoryWhoseProcessDiedIsRemovedByTheNextBuilder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("build directories are locked with flock(2)")
	}
	work := t.TempDir()
	mk := func(name string) string {
		dir := filepath.Join(work, name)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "gocache", "ab"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "gocache", "ab", "x-d"), make([]byte, 4096), 0o600))
		return dir
	}
	dead := mk(buildDirPrefix + "dead")
	lock, err := lockBuildDir(dead)
	require.NoError(t, err)
	require.NoError(t, lock.Close(), "its owner is gone: nobody holds the lock")
	live := mk(buildDirPrefix + "live")
	liveLock, err := lockBuildDir(live)
	require.NoError(t, err)
	unlocked := mk(buildDirPrefix + "nolock")
	other := mk("someone-elses-dir")

	_, err = NewBuilder(BuilderConfig{GoBinary: fakeGo(t, "true"), TempDir: work})
	require.NoError(t, err)
	assert.NoDirExists(t, dead, "abandoned: removed")
	assert.DirExists(t, live, "in use: kept")
	assert.DirExists(t, unlocked, "no lock file: not known to be abandoned")
	assert.DirExists(t, other, "not a build directory")

	require.NoError(t, liveLock.Close())
	_, err = NewBuilder(BuilderConfig{GoBinary: fakeGo(t, "true"), TempDir: work})
	require.NoError(t, err)
	assert.NoDirExists(t, live, "its process let go of it")
}

// --- the seed -----------------------------------------------------------------

// TestBuilder_TheWarmUpProgramImportsEveryAllowedImportAndPassesTheScanner:
// the seed is built from the package's own program, which the scanner
// accepts like any source, and which imports every package a strategy may
// import, so a strategy needs nothing compiled but itself.
func TestBuilder_TheWarmUpProgramImportsEveryAllowedImportAndPassesTheScanner(t *testing.T) {
	src := warmupSource()
	rep := Scan(src, DefaultScanLimits())
	require.True(t, rep.OK(), "%v", rep.Violations)
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", src["main.go"], parser.ImportsOnly)
	require.NoError(t, err)
	var imports []string
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		require.NoError(t, err)
		imports = append(imports, p)
	}
	sort.Strings(imports)
	want := AllowedImports()
	sort.Strings(want)
	assert.Equal(t, want, imports)
}

// TestBuilder_ASeedIsNamedForWhatBuiltIt: a seed is reused only by builds it
// can serve -- its name changes with the toolchain and with the SDK, and is
// the same for the same ones.
func TestBuilder_ASeedIsNamedForWhatBuiltIt(t *testing.T) {
	a := seedName("go1.27.1", "nodalsdk/v1@sha256:aa")
	assert.True(t, strings.HasPrefix(a, seedDirPrefix))
	assert.Equal(t, a, seedName("go1.27.1", "nodalsdk/v1@sha256:aa"))
	assert.NotEqual(t, a, seedName("go1.27.2", "nodalsdk/v1@sha256:aa"))
	assert.NotEqual(t, a, seedName("go1.27.1", "nodalsdk/v1@sha256:bb"))
}

// TestBuilder_PublishingASeedLeavesOnlyThatSeed: publishing copies a finished
// cache into place under its name and removes every other seed and the Go
// cache layout an earlier builder shared between all its builds (with the
// bulk those builds left there); anything else in the directory stays.
func TestBuilder_PublishingASeedLeavesOnlyThatSeed(t *testing.T) {
	from, parent := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(from, "0a"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(from, "0a", "0a11-d"), []byte("archive"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(from, "README"), []byte("cache"), 0o600))
	for _, d := range []string{"00", "ff", seedDirPrefix + "old"} {
		require.NoError(t, os.MkdirAll(filepath.Join(parent, d), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(parent, d, "bulk"), make([]byte, 1024), 0o600))
	}
	for _, f := range []string{"README", "trim.txt", "testexpire.txt", "operator-notes.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(parent, f), []byte("x"), 0o600))
	}
	require.NoError(t, publishSeed(from, parent, seedDirPrefix+"new"))
	assert.Equal(t, []string{"operator-notes.txt", seedDirPrefix + "new"}, entries(t, parent))
	assert.Equal(t, treeState(t, from), treeState(t, filepath.Join(parent, seedDirPrefix+"new")), "the seed is the cache, whole")

	// Publishing the same seed again (another builder got there first) keeps
	// the one in place.
	require.NoError(t, publishSeed(from, parent, seedDirPrefix+"new"))
	assert.Equal(t, []string{"operator-notes.txt", seedDirPrefix + "new"}, entries(t, parent))
}

// TestBuilder_CopyTreeNeverCopiesMoreThanItsLimit: a copy stops at its limit,
// and dirSize counts what is there.
func TestBuilder_CopyTreeNeverCopiesMoreThanItsLimit(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "a", "b"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a", "one"), make([]byte, 1000), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a", "b", "two"), make([]byte, 2000), 0o600))
	assert.Equal(t, int64(3000), dirSize(src))
	dst := t.TempDir()
	n, err := copyTree(dst, src, 3000)
	require.NoError(t, err)
	assert.Equal(t, int64(3000), n)
	assert.Equal(t, treeState(t, src), treeState(t, dst))
	_, err = copyTree(t.TempDir(), src, 2999)
	assert.ErrorIs(t, err, errCopyTooLarge)
}

// --- review 3, F4, with the real toolchain --------------------------------------

// TestBuilder_TwoBuildsLeaveTheSharedCacheAndTheWorkAreaAsTheyFoundThem is
// review 3's F4 turned around. A strategy whose package holds a 150 MiB
// global builds within every bound; before, it left the global in the build
// cache every build shared, and two such builds grew it by ~300 MiB. Now the
// shared cache is the builder's seed -- the standard library and the SDK,
// from its own warm-up program -- and every build works in a copy of it in
// its own directory: after a 150 MiB build and a build that does not compile,
// the shared cache is byte for byte what it was, and the work area is empty.
func TestBuilder_TwoBuildsLeaveTheSharedCacheAndTheWorkAreaAsTheyFoundThem(t *testing.T) {
	goBin := realGo(t)
	cache, work := t.TempDir(), t.TempDir()
	b, err := NewBuilder(BuilderConfig{GoBinary: goBin, CacheDir: cache, TempDir: work, Timeout: 5 * time.Minute})
	require.NoError(t, err)

	// The first build finds no seed and makes one.
	first, err := b.Build(context.Background(), Source{"main.go": []byte(cleanStrategy)})
	require.NoError(t, err, "%s", first.BuildLog)
	names := entries(t, cache)
	require.Len(t, names, 1)
	require.True(t, strings.HasPrefix(names[0], seedDirPrefix), "%v", names)
	before := treeState(t, cache)
	seedBytes := dirSize(cache)
	t.Logf("seed: %d MiB", seedBytes>>20)
	assert.Empty(t, entries(t, work))

	const global = 150 << 20
	padded, err := b.Build(context.Background(), paddedStrategy(global))
	require.NoError(t, err, "a strategy within every bound builds: %s", padded.BuildLog)
	assert.NotEmpty(t, padded.Wasm)
	assert.Positive(t, padded.Duration, "a build reports how long it took")
	broken, err := b.Build(context.Background(), Source{"main.go": []byte("package main\n\nfunc main() { undefinedFunction() }\n")})
	require.Error(t, err)
	assert.Equal(t, BuildFailureCompile, broken.Failure)

	assert.Equal(t, before, treeState(t, cache), "the shared cache is byte for byte as the builds found it")
	assert.Equal(t, seedBytes, dirSize(cache), "the 150 MiB global did not accumulate")
	assert.Empty(t, entries(t, work), "the work area is as the builds found it")

	// Another builder sharing the cache uses the seed as it is.
	again, err := NewBuilder(BuilderConfig{GoBinary: goBin, CacheDir: cache, TempDir: work, Timeout: 5 * time.Minute})
	require.NoError(t, err)
	_, err = again.Build(context.Background(), Source{"main.go": []byte(cleanStrategy)})
	require.NoError(t, err)
	assert.Equal(t, before, treeState(t, cache))
	// Nothing the toolchain started outlives a build to write there later
	// (its telemetry child did, before telemetry was turned off in a build's
	// HOME).
	assert.Never(t, func() bool { return len(entries(t, work)) > 0 }, time.Second, 50*time.Millisecond,
		"the work area stays as the builds found it")
}

// TestBuilder_ABuildsToolchainTelemetryIsOff: every build directory carries
// the toolchain's telemetry mode file, set to off, where the toolchain reads
// it for the build's HOME.
func TestBuilder_ABuildsToolchainTelemetryIsOff(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, prepareBuildDir(root))
	home := filepath.Join(root, "home")
	cfg := userConfigDir(home)
	if cfg == "" {
		t.Skip("no configuration directory for a build on this platform: the toolchain's telemetry is off without one")
	}
	b, err := os.ReadFile(filepath.Join(cfg, "go", "telemetry", "mode")) // #nosec G304 -- a test's own directory
	require.NoError(t, err)
	assert.Equal(t, "off", string(b))
	for _, d := range []string{"gocache", "home", "gopath", "tmp"} {
		assert.DirExists(t, filepath.Join(root, d))
	}
}

// TestBuilder_ThePaddedStrategyPastTheDiskBoundIsRefused: the same 150 MiB
// global, with a disk bound below what its build writes, is stopped and
// refused, and leaves nothing.
func TestBuilder_ThePaddedStrategyPastTheDiskBoundIsRefused(t *testing.T) {
	goBin := realGo(t)
	work := t.TempDir()
	b, err := NewBuilder(BuilderConfig{GoBinary: goBin, TempDir: work, Timeout: 5 * time.Minute, MaxDiskBytes: 128 << 20,
		SampleInterval: 20 * time.Millisecond})
	require.NoError(t, err)
	build, err := b.Build(context.Background(), paddedStrategy(150<<20))
	require.Error(t, err)
	assert.Equal(t, errs.CodeStrategyRejected, errs.CodeOf(err))
	assert.Equal(t, BuildFailureOutputLimit, build.Failure, "%v", err)
	assert.Empty(t, build.Wasm)
	assert.Empty(t, entries(t, work))
}
