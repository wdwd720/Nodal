package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mk(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverCmds(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "cmd/api/main.go", "package main\n")
	mk(t, root, "cmd/migrate/main.go", "package main\n")
	mk(t, root, "cmd/_scratch/main.go", "package main\n")
	mk(t, root, "cmd/testonly/x_test.go", "package main\n")
	mk(t, root, "cmd/notes.txt", "")
	if err := os.MkdirAll(filepath.Join(root, "cmd", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverCmds(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "api,migrate" {
		t.Errorf("DiscoverCmds = %v", got)
	}
}

func TestBuildArgs(t *testing.T) {
	o := Options{Tag: "abc123", Registry: "ghcr.io/org/repo/", Dockerfile: "build/Dockerfile", Context: ".", Revision: "deadbeef", Created: "2026-09-05T00:00:00Z", Pull: true, Platform: "linux/amd64"}
	args := strings.Join(BuildArgs(o, "api"), " ")
	for _, want := range []string{
		"build --file build/Dockerfile",
		"--build-arg CMD=api",
		"--build-arg VERSION=abc123",
		"--build-arg REVISION=deadbeef",
		"--label org.opencontainers.image.revision=deadbeef",
		"--tag ghcr.io/org/repo/api:abc123",
		"--pull",
		"--platform linux/amd64",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if !strings.HasSuffix(args, " .") {
		t.Errorf("context must be the last argument: %s", args)
	}
}

func TestPickDigest(t *testing.T) {
	digests := []string{"other/api@sha256:aaa", "ghcr.io/org/repo/api@sha256:bbb"}
	d, ok := PickDigest(digests, "ghcr.io/org/repo/api")
	if !ok || d != "sha256:bbb" {
		t.Errorf("PickDigest = %q %v", d, ok)
	}
	if _, ok := PickDigest(digests, "ghcr.io/org/repo/migrate"); ok {
		t.Error("unexpected digest for an unpushed image")
	}
}

func TestBuildAndManifest(t *testing.T) {
	var calls []string
	run := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	capture := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte(`["controlplane/api@sha256:0123"]`), nil
	}
	o := Options{Tag: "t1", Registry: "controlplane", Dockerfile: "build/Dockerfile", Context: ".", Revision: "r", Created: "c", Push: true}
	records, err := Build(o, []string{"api"}, run, capture, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Digest != "sha256:0123" || records[0].PinnedRef() != "controlplane/api@sha256:0123" {
		t.Fatalf("records = %+v", records)
	}
	if len(calls) != 3 || !strings.HasPrefix(calls[0], "docker build") || calls[1] != "docker push controlplane/api:t1" || !strings.HasPrefix(calls[2], "docker image inspect") {
		t.Errorf("calls = %q", calls)
	}

	out := filepath.Join(t.TempDir(), "dist", "images.json")
	if err := WriteManifest(out, records); err != nil {
		t.Fatal(err)
	}
	js, _ := os.ReadFile(out)
	if !strings.Contains(string(js), `"digest": "sha256:0123"`) {
		t.Errorf("manifest json: %s", js)
	}
	txt, _ := os.ReadFile(strings.TrimSuffix(out, ".json") + ".txt")
	if string(txt) != "controlplane/api@sha256:0123\n" {
		t.Errorf("manifest txt: %q", txt)
	}
}

func TestBuildStopsOnFailure(t *testing.T) {
	fail := func(name string, args ...string) ([]byte, error) { return nil, os.ErrPermission }
	_, err := Build(Options{Tag: "t", Registry: "r", Context: "."}, []string{"api"}, fail, fail, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "docker build r/api:t") {
		t.Fatalf("err = %v", err)
	}
}
