package sandboxrt

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// fakeGo writes an executable script standing in for the go command.
func fakeGo(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake toolchain is a shell script")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "go")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700)) // #nosec G306 -- test toolchain stub
	return p
}

// envDumper compiles, with the real toolchain, a native program named "go"
// that prints exactly the environment it was started with. No shell sits in
// between to add variables of its own.
func envDumper(t *testing.T) string {
	t.Helper()
	f := requireFixtures(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module envdump\n\ngo 1.27\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(
		"package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc main() {\n\tfor _, kv := range os.Environ() {\n\t\tfmt.Println(kv)\n\t}\n}\n"), 0o600))
	out := filepath.Join(t.TempDir(), "go")
	cmd := exec.Command(f.goBinary, "build", "-o", out, ".") // #nosec G204 -- test helper with fixed arguments
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off", "GOOS=", "GOARCH=", "CGO_ENABLED=0")
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", b)
	return out
}

func TestBuilder_EnvironmentCarriesNothingFromTheParent(t *testing.T) {
	// Build the dumper before planting anything in this process's
	// environment.
	goBin := envDumper(t)
	const secret = "nodal-fake-build-secret-91c2"
	t.Setenv("NODAL_FAKE_SECRET", secret)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)
	t.Setenv("GOPROXY", "https://proxy.attacker.example")
	t.Setenv("GOFLAGS", "-toolexec=/tmp/evil")
	t.Setenv("GONOSUMDB", "*")

	// The fake toolchain prints the environment it was given, into the build
	// log, for both the version probe and the build.
	b, err := NewBuilder(BuilderConfig{GoBinary: goBin, TempDir: t.TempDir(), Timeout: 30 * time.Second})
	require.NoError(t, err)
	build, err := b.Build(context.Background(), Source{"main.go": []byte(cleanStrategy)})
	require.Error(t, err, "the fake toolchain produces no module")

	seen := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(build.BuildLog)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		require.True(t, ok, "unexpected log line %q", line)
		seen[k] = v
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, BuildEnvironmentKeys(), keys, "the build sees exactly the documented variables")
	assert.NotContains(t, string(build.BuildLog), secret, "a parent secret must not reach the build")
	assert.Equal(t, "off", seen["GOPROXY"])
	assert.Equal(t, "off", seen["GOSUMDB"])
	assert.Equal(t, "local", seen["GOTOOLCHAIN"])
	assert.Equal(t, "off", seen["GOWORK"])
	assert.Equal(t, "0", seen["CGO_ENABLED"])
	assert.Equal(t, "-mod=mod -trimpath", seen["GOFLAGS"])
	assert.Equal(t, "wasip1", seen["GOOS"])
	assert.Equal(t, "wasm", seen["GOARCH"])
	assert.Equal(t, filepath.Dir(goBin), seen["PATH"])
	for _, k := range []string{"GOCACHE", "GOPATH", "HOME"} {
		assert.True(t, strings.Contains(seen[k], "nodal-strategy-build-"), "%s=%s lives in the build directory", k, seen[k])
	}
	assert.Len(t, b.environment("/x"), len(BuildEnvironmentKeys()))
}

func TestBuilder_RefusesUnscannedSourceWithoutInvokingTheToolchain(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "invoked")
	goBin := fakeGo(t, "/usr/bin/touch "+marker)
	b, err := NewBuilder(BuilderConfig{GoBinary: goBin, TempDir: t.TempDir()})
	require.NoError(t, err)
	build, err := b.Build(context.Background(), withImport("os"))
	require.Error(t, err)
	assert.Equal(t, errs.CodeStrategyRejected, errs.CodeOf(err))
	assert.Equal(t, BuildFailureScan, build.Failure)
	assert.Equal(t, []string{string(ScanImportForbidden)}, build.Scan.Codes())
	assert.Empty(t, build.Wasm)
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "the toolchain was invoked for source the scanner refused")
}

func TestBuilder_TimeoutKillsTheBuild(t *testing.T) {
	goBin := fakeGo(t, "/bin/sleep 30")
	b, err := NewBuilder(BuilderConfig{GoBinary: goBin, TempDir: t.TempDir(), Timeout: 300 * time.Millisecond})
	require.NoError(t, err)
	start := time.Now()
	build, err := b.Build(context.Background(), Source{"main.go": []byte(cleanStrategy)})
	require.Error(t, err)
	assert.Equal(t, errs.CodeStrategyRejected, errs.CodeOf(err))
	assert.Equal(t, BuildFailureTimeout, build.Failure)
	assert.Less(t, time.Since(start), 10*time.Second, "the build is killed at its deadline")
}

func TestBuilder_EmbedsEverySDKSourceFile(t *testing.T) {
	sdk, err := EmbeddedSDK()
	require.NoError(t, err)
	entries, err := os.ReadDir("sdk")
	require.NoError(t, err)
	var onDisk []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			onDisk = append(onDisk, e.Name())
			body, err := os.ReadFile(filepath.Join("sdk", e.Name())) // #nosec G304 -- the SDK sources
			require.NoError(t, err)
			assert.Equal(t, body, sdk[e.Name()], "%s is embedded as it is on disk", e.Name())
		}
	}
	assert.Equal(t, onDisk, sdk.Paths(), "every SDK source file and no test file is embedded")
	assert.True(t, strings.HasPrefix(SDKVersionOf(sdk), "nodalsdk/v1@sha256:"))
}

// TestBuilder_CompileIsReachableOnlyThroughScan proves structurally that the
// unscanned compile step has exactly one production caller, Build, and that
// Build scans first. The test-only buildTrustedForTest is the only other
// caller, and it lives in a _test.go file.
func TestBuilder_CompileIsReachableOnlyThroughScan(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	require.NoError(t, err)
	callers := map[string]bool{}
	buildCallsScan := false
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fun := call.Fun.(type) {
					case *ast.SelectorExpr:
						if fun.Sel.Name == "compile" {
							callers[fn.Name.Name] = true
						}
					case *ast.Ident:
						if fun.Name == "Scan" && fn.Name.Name == "Build" {
							buildCallsScan = true
						}
					}
					return true
				})
			}
		}
	}
	assert.Equal(t, map[string]bool{"Build": true}, callers, "only Build may call compile")
	assert.True(t, buildCallsScan, "Build scans before it compiles")
}

func TestBuilder_RealBuildsAreReproducibleAndRefuseBrokenCode(t *testing.T) {
	f := requireFixtures(t)
	ctx := context.Background()
	src := Source{"main.go": []byte(cleanStrategy)}
	// Two builders with separate, empty caches: nothing is shared but the
	// toolchain and the embedded SDK.
	var digests [][]byte
	for i := 0; i < 2; i++ {
		b, err := NewBuilder(BuilderConfig{GoBinary: f.goBinary, TempDir: t.TempDir(), Timeout: 5 * time.Minute})
		require.NoError(t, err)
		build, err := b.Build(ctx, src)
		require.NoError(t, err, "%s", build.BuildLog)
		assert.Equal(t, src.Digest(), build.SourceDigest)
		assert.True(t, strings.HasPrefix(build.ToolchainVersion, "go1."), build.ToolchainVersion)
		assert.Equal(t, b.SDKVersion(), build.SDKVersion)
		assert.Len(t, build.BuildLogDigest, 32)
		digests = append(digests, build.ArtifactDigest)
	}
	assert.Equal(t, digests[0], digests[1], "the same source builds to the same artifact")

	broken := Source{"main.go": []byte("package main\n\nfunc main() { undefinedFunction() }\n")}
	build, err := f.builder.Build(ctx, broken)
	require.Error(t, err)
	assert.Equal(t, errs.CodeStrategyRejected, errs.CodeOf(err))
	assert.Equal(t, BuildFailureCompile, build.Failure)
	assert.Contains(t, string(build.BuildLog), "undefinedFunction")
	assert.Empty(t, build.Wasm)
}

func TestBuilder_RejectsAnUnusableConfiguration(t *testing.T) {
	_, err := NewBuilder(BuilderConfig{GoBinary: "go"})
	assert.Error(t, err, "a relative toolchain path would be resolved through someone's PATH")
	_, err = NewBuilder(BuilderConfig{GoBinary: "/nonexistent/go"})
	assert.Error(t, err)
	_, err = NewBuilder(BuilderConfig{GoBinary: fakeGo(t, "true"), CacheDir: "relative/cache"})
	assert.Error(t, err)
}
