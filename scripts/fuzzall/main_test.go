package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixtureModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module example.com/fx\n\ngo 1.27\n")
	writeFile(t, root, "internal/a/a_test.go", `package a

import "testing"

func FuzzOK(f *testing.F) {}

func Fuzzy(f *testing.F) {} // lowercase after Fuzz: not a target

func FuzzWrongSig(t *testing.T) {}

func FuzzTwo(f *testing.F, n int) {}

func FuzzResult(f *testing.F) error { return nil }

type T struct{}

func (T) FuzzMethod(f *testing.F) {}

func TestX(t *testing.T) {}
`)
	writeFile(t, root, "internal/a/tagged_test.go", `//go:build integration

package a

import "testing"

func FuzzTagged(f *testing.F) {}
`)
	writeFile(t, root, "internal/a/a_windows_test.go", `package a

import "testing"

func FuzzWindowsOnly(f *testing.F) {}
`)
	writeFile(t, root, "internal/a/testdata/x_test.go", `package x

import "testing"

func FuzzIgnored(f *testing.F) {}
`)
	writeFile(t, root, "internal/a/notatest.go", `package a

import "testing"

func FuzzNotInTestFile(f *testing.F) {}
`)
	writeFile(t, root, "cmd/tool/main_test.go", `package main

import "testing"

func FuzzCmd(f *testing.F) {}
`)
	return root
}

func names(ts []Target) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Pkg+"."+t.Name)
	}
	return out
}

func TestDiscover(t *testing.T) {
	root := fixtureModule(t)
	got, err := Discover(root, DefaultDirs, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"./cmd/tool.FuzzCmd", "./internal/a.FuzzOK"}
	if runtime.GOOS == "windows" {
		want = append(want, "./internal/a.FuzzWindowsOnly")
	}
	if g := strings.Join(names(got), " "); g != strings.Join(want, " ") {
		t.Fatalf("got %q, want %q", g, strings.Join(want, " "))
	}
	if got[0].File != "cmd/tool/main_test.go" {
		t.Errorf("file = %q", got[0].File)
	}
}

func TestDiscoverWithTags(t *testing.T) {
	root := fixtureModule(t)
	got, err := Discover(root, []string{"internal"}, []string{"integration"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tg := range got {
		if tg.Name == "FuzzTagged" {
			found = true
		}
	}
	if !found {
		t.Fatalf("FuzzTagged not discovered with -tags integration: %v", names(got))
	}
}

func TestDiscoverMissingDirsIgnored(t *testing.T) {
	root := t.TempDir()
	got, err := Discover(root, DefaultDirs, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestValidFuzztime(t *testing.T) {
	for s, ok := range map[string]bool{"30s": true, "2m": true, "100x": true, "abc": false, "x": false, "": false} {
		if validFuzztime(s) != ok {
			t.Errorf("validFuzztime(%q) = %v, want %v", s, !ok, ok)
		}
	}
}

// TestRunEndToEnd fuzzes a real target for a handful of inputs and checks that a
// crashing target is reported as a failure.
func TestRunEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test -fuzz in -short mode")
	}
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module example.com/e2e\n\ngo 1.27\n")
	writeFile(t, root, "p/p_test.go", `package p

import "testing"

func FuzzAdd(f *testing.F) {
	f.Add(1)
	f.Fuzz(func(t *testing.T, n int) { _ = n + 1 })
}

func FuzzCrash(f *testing.F) {
	f.Add(1)
	f.Fuzz(func(t *testing.T, n int) { panic("boom") })
}
`)
	ctx := context.Background()
	ok := Run(ctx, goExe(), root, Target{Pkg: "./p", Name: "FuzzAdd"}, "3x", nil, nil)
	if !ok.Passed {
		t.Fatalf("FuzzAdd failed: %v\n%s", ok.Err, ok.Output)
	}
	bad := Run(ctx, goExe(), root, Target{Pkg: "./p", Name: "FuzzCrash"}, "3x", nil, nil)
	if bad.Passed {
		t.Fatalf("FuzzCrash unexpectedly passed:\n%s", bad.Output)
	}
	if !strings.Contains(string(bad.Output), "boom") {
		t.Errorf("failure output does not mention the panic:\n%s", bad.Output)
	}
}
