package sandboxrt

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SDKModulePath is the import path a strategy uses for the SDK. The Builder
// resolves it to the embedded SDK sources and to nothing else.
const SDKModulePath = "nodal.dev/sdk"

// ScanCode is a stable reason the scanner refused a source tree.
type ScanCode string

// Scan codes.
const (
	ScanSourceEmpty           ScanCode = "SOURCE_EMPTY"
	ScanSourceTooManyFiles    ScanCode = "SOURCE_TOO_MANY_FILES"
	ScanSourceTooLarge        ScanCode = "SOURCE_TOO_LARGE"
	ScanFileTooLarge          ScanCode = "FILE_TOO_LARGE"
	ScanFileNotGo             ScanCode = "FILE_NOT_GO"
	ScanFileNameInvalid       ScanCode = "FILE_NAME_INVALID"
	ScanFileExcludedByBuild   ScanCode = "FILE_EXCLUDED_BY_BUILD"
	ScanParseError            ScanCode = "PARSE_ERROR"
	ScanPackageNotMain        ScanCode = "PACKAGE_NOT_MAIN"
	ScanImportForbidden       ScanCode = "IMPORT_FORBIDDEN"
	ScanImportNotAllowed      ScanCode = "IMPORT_NOT_ALLOWED"
	ScanCgo                   ScanCode = "CGO"
	ScanDirectiveLinkname     ScanCode = "DIRECTIVE_LINKNAME"
	ScanDirectiveWasmImport   ScanCode = "DIRECTIVE_WASMIMPORT"
	ScanDirectiveWasmExport   ScanCode = "DIRECTIVE_WASMEXPORT"
	ScanDirectiveEmbed        ScanCode = "DIRECTIVE_EMBED"
	ScanDirectiveBuild        ScanCode = "DIRECTIVE_BUILD_CONSTRAINT"
	ScanDirectiveForbidden    ScanCode = "DIRECTIVE_FORBIDDEN"
	ScanSourceNotUTF8         ScanCode = "SOURCE_NOT_UTF8"
	ScanFileNameDuplicateCase ScanCode = "FILE_NAME_DUPLICATE_CASE"
)

var allScanCodes = []ScanCode{
	ScanSourceEmpty, ScanSourceTooManyFiles, ScanSourceTooLarge, ScanFileTooLarge, ScanFileNotGo,
	ScanFileNameInvalid, ScanFileExcludedByBuild, ScanParseError, ScanPackageNotMain, ScanImportForbidden,
	ScanImportNotAllowed, ScanCgo, ScanDirectiveLinkname, ScanDirectiveWasmImport, ScanDirectiveWasmExport,
	ScanDirectiveEmbed, ScanDirectiveBuild, ScanDirectiveForbidden, ScanSourceNotUTF8, ScanFileNameDuplicateCase,
}

// AllScanCodes returns every scan code.
func AllScanCodes() []ScanCode { return append([]ScanCode(nil), allScanCodes...) }

// AllowedImports is the complete set of imports a strategy may use: the SDK
// and a handful of pure standard-library packages. Nothing here can reach a
// file, a socket, a clock, the environment or another process.
func AllowedImports() []string {
	return []string{"encoding/json", "errors", "math/big", SDKModulePath, "sort", "strconv", "strings", "unicode/utf8"}
}

// forbiddenImportRoots are refused with IMPORT_FORBIDDEN (rather than the
// generic IMPORT_NOT_ALLOWED) because each is a known escape hatch: memory
// unsafety, reflection, the runtime, the operating system, processes, the
// network, raw system calls, plugins, filesystems and embedding.
var forbiddenImportRoots = []string{"unsafe", "reflect", "runtime", "os", "net", "syscall", "plugin", "io/fs", "embed"}

// ForbiddenImportRoots returns the escape-hatch import roots.
func ForbiddenImportRoots() []string { return append([]string(nil), forbiddenImportRoots...) }

// ScanLimits bounds a source tree.
type ScanLimits struct {
	MaxFiles      int
	MaxFileBytes  int
	MaxTotalBytes int64
}

// DefaultScanLimits are the limits every published strategy is held to.
func DefaultScanLimits() ScanLimits {
	return ScanLimits{MaxFiles: 32, MaxFileBytes: 64 << 10, MaxTotalBytes: 256 << 10}
}

// ScanViolation is one finding.
type ScanViolation struct {
	Code   ScanCode `json:"code"`
	File   string   `json:"file"`
	Line   int      `json:"line"`
	Detail string   `json:"detail"`
}

// ScanReport is the scanner's verdict on a tree.
type ScanReport struct {
	SourceDigest []byte          `json:"-"`
	Files        int             `json:"files"`
	TotalBytes   int64           `json:"total_bytes"`
	Violations   []ScanViolation `json:"violations"`
}

// OK reports whether the tree may be built.
func (r ScanReport) OK() bool { return len(r.Violations) == 0 }

// Codes returns the distinct violation codes, sorted.
func (r ScanReport) Codes() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range r.Violations {
		if !seen[string(v.Code)] {
			seen[string(v.Code)] = true
			out = append(out, string(v.Code))
		}
	}
	sort.Strings(out)
	return out
}

// Scan inspects a source tree without building it. It parses every file and
// reports every violation it finds, sorted, so an author learns everything
// that must change at once. Scan never panics, whatever the input.
func Scan(src Source, limits ScanLimits) ScanReport {
	rep := ScanReport{SourceDigest: src.Digest(), Files: len(src), TotalBytes: src.TotalBytes()}
	add := func(code ScanCode, file string, line int, detail string) {
		rep.Violations = append(rep.Violations, ScanViolation{Code: code, File: file, Line: line, Detail: detail})
	}
	if len(src) == 0 {
		add(ScanSourceEmpty, "", 0, "a strategy needs at least one .go file")
	}
	if limits.MaxFiles > 0 && len(src) > limits.MaxFiles {
		add(ScanSourceTooManyFiles, "", 0, fmt.Sprintf("%d files, at most %d", len(src), limits.MaxFiles))
	}
	if limits.MaxTotalBytes > 0 && rep.TotalBytes > limits.MaxTotalBytes {
		add(ScanSourceTooLarge, "", 0, fmt.Sprintf("%d bytes, at most %d", rep.TotalBytes, limits.MaxTotalBytes))
	}
	lowered := map[string]string{}
	for _, name := range src.Paths() {
		body := src[name]
		if prev, dup := lowered[strings.ToLower(name)]; dup {
			add(ScanFileNameDuplicateCase, name, 0, "differs from "+prev+" only by case")
		}
		lowered[strings.ToLower(name)] = name
		if !strings.HasSuffix(name, ".go") {
			add(ScanFileNotGo, name, 0, "only .go files are accepted")
			continue
		}
		if !validFileName(name) {
			add(ScanFileNameInvalid, name, 0, "names are [a-z0-9_]{1,64}.go, no directories, no _test.go")
			continue
		}
		if limits.MaxFileBytes > 0 && len(body) > limits.MaxFileBytes {
			add(ScanFileTooLarge, name, 0, fmt.Sprintf("%d bytes, at most %d", len(body), limits.MaxFileBytes))
			continue
		}
		if !utf8.Valid(body) {
			add(ScanSourceNotUTF8, name, 0, "source must be valid UTF-8")
			continue
		}
		if !matchesWasip1(name, body) {
			add(ScanFileExcludedByBuild, name, 0, "this file would not be compiled for wasip1/wasm; what is scanned must be what is built")
		}
		scanFile(name, body, add)
	}
	sort.SliceStable(rep.Violations, func(i, j int) bool {
		a, b := rep.Violations[i], rep.Violations[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Detail < b.Detail
	})
	return rep
}

func validFileName(name string) bool {
	if name != path.Base(name) || strings.HasSuffix(name, "_test.go") {
		return false
	}
	stem := strings.TrimSuffix(name, ".go")
	if len(stem) == 0 || len(stem) > 64 {
		return false
	}
	for i := 0; i < len(stem); i++ {
		c := stem[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' {
			return false
		}
	}
	return true
}

// matchesWasip1 asks go/build -- the authoritative implementation -- whether
// the file would be compiled for GOOS=wasip1 GOARCH=wasm. A name such as
// x_linux.go is silently excluded by the go tool; it is refused here so the
// scanned tree and the built tree are the same tree.
func matchesWasip1(name string, body []byte) bool {
	ctxt := build.Context{
		GOOS: "wasip1", GOARCH: "wasm", Compiler: "gc", CgoEnabled: false,
		OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil },
	}
	ok, err := ctxt.MatchFile("/strategy", name)
	return err == nil && ok
}

func scanFile(name string, body []byte, add func(ScanCode, string, int, string)) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, body, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		add(ScanParseError, name, firstErrorLine(err), truncate(err.Error(), 200))
		if f == nil {
			return
		}
	}
	if f.Name == nil || f.Name.Name != "main" {
		pkg := ""
		if f.Name != nil {
			pkg = f.Name.Name
		}
		add(ScanPackageNotMain, name, 1, "package "+pkg+"; a strategy is package main")
	}
	allowed := AllowedImports()
	for _, imp := range f.Imports {
		line := fset.Position(imp.Pos()).Line
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			add(ScanParseError, name, line, "unreadable import path")
			continue
		}
		switch {
		case p == "C":
			add(ScanCgo, name, line, `import "C" (cgo) is not available to strategies`)
		case forbiddenImport(p):
			add(ScanImportForbidden, name, line, p)
		case !containsString(allowed, p):
			add(ScanImportNotAllowed, name, line, p)
		}
	}
	for _, group := range f.Comments {
		for _, c := range group.List {
			scanComment(name, fset.Position(c.Pos()).Line, c, add)
		}
	}
}

func scanComment(file string, line int, c *ast.Comment, add func(ScanCode, string, int, string)) {
	text := c.Text
	if strings.HasPrefix(text, "/*") {
		return
	}
	body := strings.TrimPrefix(text, "//")
	switch {
	case strings.HasPrefix(body, "go:linkname"):
		add(ScanDirectiveLinkname, file, line, "//go:linkname reaches unexported runtime symbols")
	case strings.HasPrefix(body, "go:wasmimport"):
		add(ScanDirectiveWasmImport, file, line, "//go:wasmimport would import a host function")
	case strings.HasPrefix(body, "go:wasmexport"):
		add(ScanDirectiveWasmExport, file, line, "//go:wasmexport would export a guest function")
	case strings.HasPrefix(body, "go:embed"):
		add(ScanDirectiveEmbed, file, line, "//go:embed would read files at build time")
	case strings.HasPrefix(body, "go:build"), strings.HasPrefix(strings.TrimSpace(body), "+build"):
		add(ScanDirectiveBuild, file, line, "build constraints are not allowed; every file is built")
	case strings.HasPrefix(body, "go:"), strings.HasPrefix(body, "line "):
		add(ScanDirectiveForbidden, file, line, truncate(strings.Fields(body + " ")[0], 64))
	}
}

func forbiddenImport(p string) bool {
	for _, root := range forbiddenImportRoots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func firstErrorLine(err error) int {
	var list scanner.ErrorList
	if errors.As(err, &list) && len(list) > 0 {
		return list[0].Pos.Line
	}
	return 0
}

// truncate shortens s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
