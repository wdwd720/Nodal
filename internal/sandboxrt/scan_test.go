package sandboxrt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cleanStrategy = `package main

import (
	"errors"
	"strings"

	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		if strings.TrimSpace(s.AsOf()) == "" {
			return errors.New("no as-of")
		}
		return nil
	})
}
`

func withImport(path string) Source {
	return Source{"main.go": []byte("package main\n\nimport _ \"" + path + "\"\n\nfunc main() {}\n")}
}

func TestScan_ACleanStrategyPasses(t *testing.T) {
	rep := Scan(Source{"main.go": []byte(cleanStrategy)}, DefaultScanLimits())
	assert.True(t, rep.OK(), "%v", rep.Violations)
	assert.Equal(t, Source{"main.go": []byte(cleanStrategy)}.Digest(), rep.SourceDigest)
	for _, p := range AllowedImports() {
		rep := Scan(withImport(p), DefaultScanLimits())
		assert.True(t, rep.OK(), "%s should be allowed: %v", p, rep.Violations)
	}
}

func TestScan_ForbiddenImportBlockedByScanner(t *testing.T) {
	forbidden := []string{
		"unsafe", "reflect", "runtime", "runtime/debug", "os", "os/exec", "os/signal", "net", "net/http", "net/netip",
		"syscall", "syscall/js", "plugin", "io/fs", "embed",
	}
	for _, p := range forbidden {
		rep := Scan(withImport(p), DefaultScanLimits())
		require.False(t, rep.OK(), p)
		assert.Equal(t, []string{string(ScanImportForbidden)}, rep.Codes(), p)
		assert.Equal(t, p, rep.Violations[0].Detail)
	}
	notAllowed := []string{"fmt", "time", "math/rand", "math/rand/v2", "crypto/rand", "io", "bufio", "log", "sync", "context",
		"github.com/nodal/controlplane/internal/signing", "nodal.dev/sdk/internal", "math", "bytes"}
	for _, p := range notAllowed {
		rep := Scan(withImport(p), DefaultScanLimits())
		require.False(t, rep.OK(), p)
		assert.Equal(t, []string{string(ScanImportNotAllowed)}, rep.Codes(), p)
	}
	rep := Scan(Source{"main.go": []byte("package main\n\n// #include <stdio.h>\nimport \"C\"\n\nfunc main() {}\n")}, DefaultScanLimits())
	assert.Equal(t, []string{string(ScanCgo)}, rep.Codes())
}

func TestScan_EveryDirectiveIsRefused(t *testing.T) {
	cases := map[string]ScanCode{
		"//go:linkname now time.now":        ScanDirectiveLinkname,
		"//go:wasmimport nodal sign":        ScanDirectiveWasmImport,
		"//go:wasmexport steal":             ScanDirectiveWasmExport,
		"//go:embed secrets.txt":            ScanDirectiveEmbed,
		"//go:noinline":                     ScanDirectiveForbidden,
		"//go:nosplit":                      ScanDirectiveForbidden,
		"//go:cgo_import_dynamic x y \"z\"": ScanDirectiveForbidden,
		"//line other.go:1":                 ScanDirectiveForbidden,
	}
	for directive, code := range cases {
		src := Source{"main.go": []byte("package main\n\n" + directive + "\nfunc f() {}\n\nfunc main() {}\n")}
		rep := Scan(src, DefaultScanLimits())
		require.False(t, rep.OK(), directive)
		assert.Contains(t, rep.Codes(), string(code), directive)
	}
	for _, header := range []string{"//go:build wasip1\n\n", "// +build wasip1\n\n"} {
		rep := Scan(Source{"main.go": []byte(header + "package main\n\nfunc main() {}\n")}, DefaultScanLimits())
		assert.Contains(t, rep.Codes(), string(ScanDirectiveBuild), header)
	}
	// A comment that merely mentions a directive is not one.
	rep := Scan(Source{"main.go": []byte("package main\n\n// see go:linkname docs\n/*go:embed x*/\nfunc main() {}\n")}, DefaultScanLimits())
	assert.True(t, rep.OK(), "%v", rep.Violations)
}

func TestScan_RefusesNonGoFilesBadNamesAndOversizedTrees(t *testing.T) {
	rep := Scan(Source{}, DefaultScanLimits())
	assert.Equal(t, []string{string(ScanSourceEmpty)}, rep.Codes())

	rep = Scan(Source{"main.go": []byte(cleanStrategy), "notes.txt": []byte("x"), "go.mod": []byte("module x")}, DefaultScanLimits())
	assert.Equal(t, []string{string(ScanFileNotGo)}, rep.Codes())
	assert.Len(t, rep.Violations, 2)

	for _, name := range []string{"../main.go", "dir/main.go", "Main.go", "main_test.go", "a-b.go", ".go", strings.Repeat("a", 65) + ".go"} {
		rep := Scan(Source{name: []byte("package main\nfunc main(){}\n")}, DefaultScanLimits())
		assert.Contains(t, rep.Codes(), string(ScanFileNameInvalid), name)
	}
	for _, name := range []string{"main_linux.go", "main_windows_amd64.go", "x_js.go"} {
		rep := Scan(Source{name: []byte("package main\nfunc main(){}\n")}, DefaultScanLimits())
		assert.Contains(t, rep.Codes(), string(ScanFileExcludedByBuild), name)
	}
	rep = Scan(Source{"main.go": []byte("package strategy\nfunc main(){}\n")}, DefaultScanLimits())
	assert.Equal(t, []string{string(ScanPackageNotMain)}, rep.Codes())
	rep = Scan(Source{"main.go": []byte("package main\nfunc main( {\n")}, DefaultScanLimits())
	assert.Contains(t, rep.Codes(), string(ScanParseError))
	rep = Scan(Source{"main.go": []byte("package main\n// \xff\xfe\nfunc main(){}\n")}, DefaultScanLimits())
	assert.Equal(t, []string{string(ScanSourceNotUTF8)}, rep.Codes())

	limits := ScanLimits{MaxFiles: 2, MaxFileBytes: 100, MaxTotalBytes: 150}
	big := []byte("package main\n\nfunc main() {}\n" + strings.Repeat("// padding\n", 20))
	rep = Scan(Source{"a.go": big}, limits)
	assert.Contains(t, rep.Codes(), string(ScanFileTooLarge))
	assert.Contains(t, rep.Codes(), string(ScanSourceTooLarge))
	rep = Scan(Source{"a.go": []byte("package main\n"), "b.go": []byte("package main\n"), "c.go": []byte("package main\n")}, limits)
	assert.Contains(t, rep.Codes(), string(ScanSourceTooManyFiles))
}

func TestScan_ReportIsCompleteSortedAndStable(t *testing.T) {
	src := Source{
		"b.go": []byte("package main\n\nimport (\n\t\"os\"\n\t\"fmt\"\n)\n\n//go:linkname x y\nfunc main() { _ = os.Args; fmt.Println() }\n"),
		"a.go": []byte("package main\n\nimport \"unsafe\"\n\nvar _ = unsafe.Sizeof(0)\n"),
	}
	first := Scan(src, DefaultScanLimits())
	for i := 0; i < 20; i++ {
		assert.Equal(t, first, Scan(src, DefaultScanLimits()))
	}
	require.Len(t, first.Violations, 4, "every finding is reported at once: %v", first.Violations)
	assert.Equal(t, "a.go", first.Violations[0].File)
	assert.Equal(t, []string{string(ScanDirectiveLinkname), string(ScanImportForbidden), string(ScanImportNotAllowed)}, first.Codes())
	for _, v := range first.Violations {
		assert.Contains(t, AllScanCodes(), v.Code)
	}
}

func TestSource_DigestIsCanonical(t *testing.T) {
	a := Source{"a.go": []byte("x"), "b.go": []byte("y")}
	b := Source{"b.go": []byte("y"), "a.go": []byte("x")}
	assert.Equal(t, a.Digest(), b.Digest(), "map order is irrelevant")
	assert.NotEqual(t, a.Digest(), Source{"a.go": []byte("xy"), "b.go": []byte("")}.Digest(), "content boundaries matter")
	assert.NotEqual(t, a.Digest(), Source{"a.go": []byte("y"), "b.go": []byte("x")}.Digest())
	assert.Len(t, a.Digest(), 32)
}

// FuzzScan: the scanner never panics, whatever the source and file name, and
// every reported code is a declared one.
func FuzzScan(f *testing.F) {
	f.Add("main.go", cleanStrategy)
	f.Add("main.go", "package main\nimport \"os\"\n//go:linkname a b\nfunc main(){}")
	f.Add("x_linux.go", "//go:build linux\npackage main")
	f.Add("../../etc", "\x00\xff")
	f.Add("main.go", "package main\nimport \"C\"\n")
	f.Add("main.go", "package main\nfunc main() { /* unterminated")
	codes := map[ScanCode]bool{}
	for _, c := range AllScanCodes() {
		codes[c] = true
	}
	f.Fuzz(func(t *testing.T, name, body string) {
		rep := Scan(Source{name: []byte(body)}, DefaultScanLimits())
		for _, v := range rep.Violations {
			if !codes[v.Code] {
				t.Fatalf("undeclared code %q", v.Code)
			}
		}
		if rep.OK() && !strings.HasSuffix(name, ".go") {
			t.Fatalf("a non-.go file passed the scan: %q", name)
		}
	})
}
