package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const moneySrc = `package money

import (
	"math/big"
	"strconv"
)

type Bad struct {
	Rate float64 // FIELD
}

func Conv(x int64) int64 {
	y := float64(x) // CONV
	z := 1.5 // LITERAL
	p, _ := strconv.ParseFloat("1", 64) // PARSE
	f := big.NewFloat(1) // BIGFLOAT
	v, _ := f.Float64() // METHOD
	w := float32(3) // lintfin:allow legacy bridge, removed in PR-42
	_, _, _, _, _ = y, z, p, v, w
	return x
}
`

const plannerSrc = `package planner

import (
	_ "example.com/cp/internal/signing" // SIGNING
	_ "example.com/cp/internal/event/eventtest" // EVENTTEST
	_ "example.com/cp/internal/testkit/db" // TESTKIT
	_ "example.com/cp/internal/riskpolicy"
	_ "example.com/cp/internal/risk/policy/rules" // POLICY
	_ "example.com/cp/internal/attest"
	_ "example.com/cp/internal/marketdata"
)
`

func write(t *testing.T, root, rel, src string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lineOf(t *testing.T, src, marker string) int {
	t.Helper()
	for i, l := range strings.Split(src, "\n") {
		if strings.Contains(l, marker) {
			return i + 1
		}
	}
	t.Fatalf("marker %q not in source", marker)
	return 0
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	write(t, root, "go.mod", "module example.com/cp\n\ngo 1.27\n")
	write(t, root, "internal/money/usd.go", moneySrc)
	write(t, root, "internal/money/usd_test.go", "package money\n\nvar _ float64 = 1.5\n")
	write(t, root, "internal/other/x.go", "package other\n\nvar X float64 = 1.5\n")
	write(t, root, "internal/agent/planner/p.go", plannerSrc)
	write(t, root, "internal/ledger/testdata/ignored.go", "package ignored\n\nvar X float64\n")
	write(t, root, "cmd/api/main.go", "package main\n\nimport _ \"example.com/cp/internal/testkit\" // CMDTESTKIT\n\nfunc main() {}\n")
	write(t, root, "cmd/api/main_test.go", "package main\n\nimport _ \"example.com/cp/internal/testkit\"\n")

	module, err := ModulePath(root)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := Run(DefaultConfig(root, module))
	if err != nil {
		t.Fatal(err)
	}

	type want struct {
		file string
		line int
		msg  string
	}
	wants := []want{
		{"cmd/api/main.go", 3, "test-only package"},
		{"internal/agent/planner/p.go", lineOf(t, plannerSrc, "// SIGNING"), "internal/signing"},
		{"internal/agent/planner/p.go", lineOf(t, plannerSrc, "// EVENTTEST"), "test-only package"},
		{"internal/agent/planner/p.go", lineOf(t, plannerSrc, "// TESTKIT"), "test-only package"},
		{"internal/agent/planner/p.go", lineOf(t, plannerSrc, "// POLICY"), "internal/risk/policy/rules"},
		{"internal/money/usd.go", lineOf(t, moneySrc, "// FIELD"), "float64 is forbidden"},
		{"internal/money/usd.go", lineOf(t, moneySrc, "// CONV"), "float64 is forbidden"},
		{"internal/money/usd.go", lineOf(t, moneySrc, "// LITERAL"), "literal 1.5"},
		{"internal/money/usd.go", lineOf(t, moneySrc, "// PARSE"), "strconv.ParseFloat"},
		{"internal/money/usd.go", lineOf(t, moneySrc, "// BIGFLOAT"), "big.NewFloat"},
		{"internal/money/usd.go", lineOf(t, moneySrc, "// METHOD"), ".Float64()"},
	}
	if len(findings) != len(wants) {
		for _, f := range findings {
			t.Log(f)
		}
		t.Fatalf("got %d findings, want %d", len(findings), len(wants))
	}
	for i, w := range wants {
		f := findings[i]
		if f.File != w.file || f.Line != w.line || !strings.Contains(f.Msg, w.msg) {
			t.Errorf("finding %d = %s, want %s:%d containing %q", i, f, w.file, w.line, w.msg)
		}
	}
}

func TestIsTestOnlyImport(t *testing.T) {
	const m = "example.com/cp"
	cases := map[string]bool{
		m + "/internal/testkit":              true,
		m + "/internal/testkit/db":           true,
		m + "/internal/event/eventtest":      true,
		m + "/internal/attest":               false,
		m + "/internal/event":                false,
		m + "/internal/latest":               false,
		"github.com/other/internal/testkit":  false,
		"github.com/stretchr/testify/assert": false,
	}
	for ip, want := range cases {
		if got := isTestOnlyImport(m, ip); got != want {
			t.Errorf("isTestOnlyImport(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestAllowedLines(t *testing.T) {
	src := "a\nb // lintfin:allow reason\n// lintfin:allow whole line\nc\n"
	got := allowedLines([]byte(src))
	if !got[2] || !got[3] || got[1] || got[4] {
		t.Fatalf("allowedLines = %v", got)
	}
}
