package sandboxrt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
)

// The builder's files on disk.
//
// The WORK AREA (BuilderConfig.TempDir) holds one directory per running
// build, named buildDirPrefix*, and nothing else of the builder's. A build
// directory holds the build's sources, its own build cache, the toolchain's
// temporary files and its module; it is removed when the build ends, and one
// whose process died is removed by the next builder that starts in the work
// area (a directory is in use exactly while its lock file is locked).
//
// The CACHE DIRECTORY (BuilderConfig.CacheDir) holds the builder's SEED, a
// build cache with the standard library and the SDK compiled for the strategy
// target. Only the builder writes it, from its own warm-up program
// (warmupSource), never from a strategy; builds read it by copying it into
// their own directories.
const (
	buildDirPrefix = "nodal-strategy-build-"
	seedDirPrefix  = "seed-"
	seedTempPrefix = ".seed-"
	// maxSeedBytes bounds a seed: a cold build of the warm-up program writes
	// about 32 MiB, so a seed far larger than that is not one this builder
	// made and is not used.
	maxSeedBytes = 256 << 20
)

// --- slots ------------------------------------------------------------------

// buildSlot is one of the builder's MaxConcurrentBuilds slots, held by one
// goroutine from before its build directory exists until after it is gone.
type buildSlot struct {
	slots    chan struct{}
	released bool
	kept     bool
}

// acquire waits for a free slot for as long as ctx allows. A context that is
// already done is its own error; one that ends while every slot is in use is
// AT_CAPACITY: the build did not start, and nothing was created.
func (b *Builder) acquire(ctx context.Context) (*buildSlot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case b.slots <- struct{}{}:
		return &buildSlot{slots: b.slots}, nil
	case <-ctx.Done():
		return nil, errs.Wrap(ctx.Err(), errs.CodeAtCapacity, "sandboxrt: every build slot stayed in use; the build did not start").
			WithField("max_concurrent_builds", cap(b.slots))
	}
}

// keep marks the slot as holding a build directory that could not be
// removed: it is never released, so the work area still never holds more
// directories than there are slots.
func (s *buildSlot) keep() { s.kept = true }

func (s *buildSlot) release() {
	if s == nil || s.released || s.kept {
		return
	}
	s.released = true
	<-s.slots
}

// --- build directories ----------------------------------------------------

// newBuildDir creates a build directory in the work area and locks it for
// this process.
func newBuildDir(workArea string) (string, *os.File, error) {
	root, err := os.MkdirTemp(workArea, buildDirPrefix)
	if err != nil {
		return "", nil, errs.Wrap(err, errs.CodeInternal, "sandboxrt: create build directory")
	}
	lock, err := lockBuildDir(root)
	if err != nil {
		_ = removeTree(root)
		return "", nil, errs.Wrap(err, errs.CodeInternal, "sandboxrt: claim build directory")
	}
	return root, lock, nil
}

// closeBuildDir removes a build directory and everything in it, then gives
// up its lock. A directory that cannot be removed keeps its slot.
func closeBuildDir(root string, lock *os.File, slot *buildSlot) {
	if removeTree(root) != nil {
		slot.keep()
	}
	if lock != nil {
		_ = lock.Close()
	}
}

// prepareBuildDir creates a build directory's standard layout -- its build
// cache, HOME, GOPATH and temporary directory -- and turns the toolchain's
// telemetry off in its HOME.
func prepareBuildDir(root string) error {
	for _, d := range []string{"gocache", "home", "gopath", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			return errs.Wrap(err, errs.CodeInternal, "sandboxrt: create build directory")
		}
	}
	if err := telemetryOff(filepath.Join(root, "home")); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "sandboxrt: turn the toolchain's telemetry off")
	}
	return nil
}

// telemetryOff writes the Go toolchain's telemetry mode file -- what `go
// telemetry off` writes -- under a build's HOME. Left at its default the go
// command collects counters there and, once a day per configuration
// directory (and a build's is always new), starts a detached child process
// in a session of its own: it outlives the build and its process group, and
// writes into the build directory after the directory was removed.
func telemetryOff(home string) error {
	dir := userConfigDir(home)
	if dir == "" {
		return nil
	}
	tel := filepath.Join(dir, "go", "telemetry")
	if err := os.MkdirAll(tel, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(tel, "mode"), []byte("off"), 0o600)
}

// userConfigDir is os.UserConfigDir for a process whose HOME is home and
// whose environment names no other configuration directory -- a build's
// (buildEnvironment sets no XDG_CONFIG_HOME, and no AppData, so on Windows
// the toolchain finds no configuration directory and telemetry is off).
func userConfigDir(home string) string {
	switch runtime.GOOS {
	case "darwin", "ios":
		return filepath.Join(home, "Library", "Application Support")
	case "plan9":
		return filepath.Join(home, "lib")
	case "windows":
		return ""
	}
	return filepath.Join(home, ".config")
}

// sweepAbandonedBuilds removes the build directories in a work area whose
// lock nobody holds: their process died before it could remove them. A
// directory without a lock file is left alone (it is being created, or it is
// not this builder's).
func sweepAbandonedBuilds(workArea string) {
	entries, err := os.ReadDir(workArea)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), buildDirPrefix) {
			continue
		}
		dir := filepath.Join(workArea, e.Name())
		if buildDirAbandoned(dir) {
			_ = removeTree(dir)
		}
	}
}

// removeTree removes a directory and everything in it. A tree the
// toolchain made read-only (a module cache is written read-only) cannot be
// emptied as it is, so on failure every directory in it is made writable
// and the removal tried again.
func removeTree(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) // #nosec G302 -- a directory of this builder's, made removable
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// dirSize is the total size of the regular files under dir. What vanishes
// or cannot be read while it walks is not counted.
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

var errCopyTooLarge = errors.New("sandboxrt: the tree is larger than its copy may be")

// copyTree copies the regular files and directories under src into dst
// (which may exist), and never more than limit bytes; it returns how many
// bytes it copied. Anything else (a link, a device) is not copied.
func copyTree(dst, src string, limit int64) (int64, error) {
	var total int64
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if total += info.Size(); total > limit {
			return errCopyTooLarge
		}
		return copyFile(target, p, info.Size())
	})
	return total, err
}

func copyFile(dst, src string, size int64) error {
	in, err := os.Open(src) // #nosec G304 -- a file of the builder's own seed
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a path in a directory the builder created
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = fmt.Errorf("sandboxrt: %s changed while it was copied", src)
	}
	return err
}

// --- the seed ---------------------------------------------------------------

// seedCache is a published seed: a complete build cache that never changes.
type seedCache struct {
	dir string
	// version is the toolchain that built it.
	version string
	bytes   int64
}

// seedFor returns the builder's seed, finding or building it on the first
// build that asks; nil when there is none (no CacheDir, or it could not be
// built -- builds then start cold, which is slower and otherwise the same).
// An attempt cut short by the caller's context is tried again by the next
// build; any other failure is not retried for the builder's lifetime.
func (b *Builder) seedFor(ctx context.Context, slot *buildSlot) *seedCache {
	if b.cfg.CacheDir == "" {
		return nil
	}
	b.seedMu.Lock()
	defer b.seedMu.Unlock()
	if b.seedTried {
		return b.seed
	}
	s, err := b.warm(ctx, slot)
	if err != nil && ctx.Err() != nil {
		return nil
	}
	b.seedTried, b.seed = true, s
	return s
}

// warm finds the seed this toolchain and SDK would build, or builds it: it
// compiles the package's own warm-up program -- after scanning it, like any
// source -- in a build directory of the caller's slot, and publishes that
// build's cache.
func (b *Builder) warm(ctx context.Context, slot *buildSlot) (*seedCache, error) {
	version, err := b.probeVersion(ctx, slot)
	if err != nil {
		return nil, err
	}
	name := seedName(version, b.sdkVersion)
	dir := filepath.Join(b.cfg.CacheDir, name)
	if s, err := openSeed(dir, version); err == nil {
		return s, nil
	}
	src := warmupSource()
	if rep := Scan(src, DefaultScanLimits()); !rep.OK() {
		return nil, errs.New(errs.CodeInternal, "sandboxrt: the warm-up program does not pass the scanner").WithField("scan_codes", rep.Codes())
	}
	start := Build{SourceDigest: src.Digest(), SDKVersion: b.sdkVersion, BuiltAt: b.cfg.Clock.Now()}
	if _, err := b.buildIn(ctx, slot, start, src, nil, func(root string, built Build) error {
		if built.ToolchainVersion != version {
			return errs.New(errs.CodeInternal, "sandboxrt: the toolchain changed while the seed was built")
		}
		return publishSeed(filepath.Join(root, "gocache"), b.cfg.CacheDir, name)
	}); err != nil {
		return nil, err
	}
	return openSeed(dir, version)
}

// probeVersion asks the toolchain its version, in a build directory of its
// own under the caller's slot.
func (b *Builder) probeVersion(ctx context.Context, slot *buildSlot) (string, error) {
	root, lock, err := newBuildDir(b.cfg.TempDir)
	if err != nil {
		return "", err
	}
	defer closeBuildDir(root, lock, slot)
	if err := prepareBuildDir(root); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	out, _, err := b.goCommand(ctx, root, buildEnvironment(b.cfg.GoBinary, root), nil, "env", "GOVERSION")
	if err != nil {
		return "", errs.Wrap(err, errs.CodeInternal, "sandboxrt: read toolchain version")
	}
	v := strings.TrimSpace(string(out))
	if v == "" || strings.ContainsAny(v, "\n\r") {
		return "", errs.New(errs.CodeInternal, "sandboxrt: the toolchain did not state its version")
	}
	return v, nil
}

// warmupSource is the program the seed is built from: every import a
// strategy may use, so the seed holds every package a strategy can need
// besides its own.
func warmupSource() Source {
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n")
	for _, p := range AllowedImports() {
		if p != SDKModulePath {
			b.WriteString("\t_ \"" + p + "\"\n")
		}
	}
	b.WriteString("\n\tnodalsdk \"" + SDKModulePath + "\"\n)\n\nfunc main() {\n\tnodalsdk.Run(func(*nodalsdk.Session) error { return nil })\n}\n")
	return Source{"main.go": []byte(b.String())}
}

// seedName names the seed a toolchain, an SDK, the warm-up program and the
// build environment's fixed settings produce: a seed is only ever reused by
// builds it can serve.
func seedName(version, sdkVersion string) string {
	h := sha256.New()
	fmt.Fprintf(h, "nodal-strategy-seed/v1\n%s\n%s\n%x\n", version, sdkVersion, warmupSource().Digest())
	for _, kv := range buildEnvironment("/", "") {
		if k, _, _ := strings.Cut(kv, "="); !isBuildPathKey(k) {
			fmt.Fprintln(h, kv)
		}
	}
	return seedDirPrefix + hex.EncodeToString(h.Sum(nil))[:32]
}

// isBuildPathKey reports whether an environment variable names a build's own
// directories (or the toolchain's): not a setting a seed depends on.
func isBuildPathKey(k string) bool {
	switch k {
	case "PATH", "GOCACHE", "GOPATH", "HOME", "GOTMPDIR", "TMPDIR":
		return true
	}
	return false
}

// openSeed returns the published seed in dir.
func openSeed(dir, version string) (*seedCache, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, errs.New(errs.CodeInternal, "sandboxrt: the seed is not a directory")
	}
	n := dirSize(dir)
	if n > maxSeedBytes {
		return nil, errs.New(errs.CodeInternal, "sandboxrt: the seed is larger than any seed this builder makes")
	}
	return &seedCache{dir: dir, version: version, bytes: n}, nil
}

// publishSeed copies a finished build cache aside in parent and renames the
// copy into place as name, so a seed is complete whenever it can be read;
// when another builder published the same seed first, its copy stands. It
// then removes every other seed and whatever a previous builder left in the
// Go cache's own layout at the top of parent: CacheDir holds one seed.
func publishSeed(from, parent, name string) error {
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "sandboxrt: create the seed directory")
	}
	tmp, err := os.MkdirTemp(parent, seedTempPrefix)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "sandboxrt: create the seed directory")
	}
	if _, err := copyTree(tmp, from, maxSeedBytes); err != nil {
		_ = removeTree(tmp)
		return errs.Wrap(err, errs.CodeInternal, "sandboxrt: copy the seed")
	}
	target := filepath.Join(parent, name)
	if err := os.Rename(tmp, target); err != nil {
		_ = removeTree(tmp)
		if st, statErr := os.Stat(target); statErr != nil || !st.IsDir() {
			return errs.Wrap(err, errs.CodeInternal, "sandboxrt: publish the seed")
		}
	}
	removeStaleCacheEntries(parent, name)
	return nil
}

// isGoCacheLayoutEntry reports whether a name is one the Go command writes at
// the top of a cache directory: 256 two-hex-digit directories and three
// files.
func isGoCacheLayoutEntry(name string) bool {
	switch name {
	case "README", "trim.txt", "testexpire.txt":
		return true
	}
	return len(name) == 2 && isLowerHex(name[0]) && isLowerHex(name[1])
}

func isLowerHex(c byte) bool { return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') }

// removeStaleCacheEntries removes from a cache directory every seed but keep
// and the Go cache layout a previous builder shared between all its builds.
func removeStaleCacheEntries(parent, keep string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name == keep {
			continue
		}
		if (e.IsDir() && strings.HasPrefix(name, seedDirPrefix)) || isGoCacheLayoutEntry(name) {
			_ = removeTree(filepath.Join(parent, name))
		}
	}
}
