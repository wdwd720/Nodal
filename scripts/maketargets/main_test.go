package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = `# Task runner
GO ?= go
BIN := $(CURDIR)/bin ## looks like help but is an assignment
.PHONY: help build
help: ## Show targets
	@$(GO) run ./scripts/maketargets Makefile

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
.PHONY: build
build: deps ## Build all Go binaries into ./bin
	$(GO) build ./... ## recipe comments never match
build-web: ## Build the web app
	$(PNPM) --filter web build
a b: ## Two targets on one line
undocumented:
	echo hi
# A stray comment that is not a section
# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------
test: unit property ## Default developer test set
`

func TestParse(t *testing.T) {
	got, err := Parse(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []Target{
		{Name: "help", Help: "Show targets", Section: ""},
		{Name: "build", Help: "Build all Go binaries into ./bin", Section: "Build"},
		{Name: "build-web", Help: "Build the web app", Section: "Build"},
		{Name: "a", Help: "Two targets on one line", Section: "Build"},
		{Name: "b", Help: "Two targets on one line", Section: "Build"},
		{Name: "test", Help: "Default developer test set", Section: "Tests"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d targets %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("target %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseCRLF(t *testing.T) {
	got, err := Parse(strings.NewReader(strings.ReplaceAll(fixture, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 || got[5].Help != "Default developer test set" {
		t.Fatalf("CRLF input parsed as %+v", got)
	}
}

func TestRenderAlignment(t *testing.T) {
	targets, _ := Parse(strings.NewReader(fixture))
	var buf bytes.Buffer
	Render(&buf, targets)
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[0] != "Usage: make <target>" {
		t.Fatalf("first line %q", lines[0])
	}
	// "build-web" is the widest name (9 chars): every target line pads to it plus two spaces.
	for _, want := range []string{
		"  help       Show targets",
		"  build      Build all Go binaries into ./bin",
		"  build-web  Build the web app",
		"  test       Default developer test set",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\nBuild\n  build ") || !strings.Contains(out, "\nTests\n  test ") {
		t.Errorf("sections not rendered as headers:\n%s", out)
	}
}

func TestRepositoryMakefile(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Skip("repository Makefile not found:", err)
	}
	defer func() { _ = f.Close() }()
	targets, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, tg := range targets {
		names[tg.Name] = tg.Section
	}
	for _, want := range []string{"help", "dev", "build", "test", "lint", "fuzz", "tools", "images"} {
		if _, ok := names[want]; !ok {
			t.Errorf("Makefile target %q not discovered", want)
		}
	}
	if names["build"] != "Build" || names["tools"] != "Tooling bootstrap" {
		t.Errorf("sections: build=%q tools=%q", names["build"], names["tools"])
	}
}
