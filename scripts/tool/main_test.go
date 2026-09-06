package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExpand(t *testing.T) {
	if got := expand("sqlc_{ver}_windows_amd64.zip", "v1.31.1"); got != "sqlc_1.31.1_windows_amd64.zip" {
		t.Errorf("expand = %q", got)
	}
	if got := expand("k6-{tag}-linux-amd64.tar.gz", "v2.2.0"); got != "k6-v2.2.0-linux-amd64.tar.gz" {
		t.Errorf("expand = %q", got)
	}
}

func TestPinnedTableConsistency(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range goTools {
		if seen[g.Name] {
			t.Errorf("duplicate tool %s", g.Name)
		}
		seen[g.Name] = true
		if !strings.HasPrefix(g.Version, "v") || !strings.Contains(g.Package, "/") {
			t.Errorf("%s: bad pin %+v", g.Name, g)
		}
	}
	for _, r := range releaseTools {
		if seen[r.Name] {
			t.Errorf("duplicate tool %s", r.Name)
		}
		seen[r.Name] = true
		for _, p := range []string{"windows/amd64", "linux/amd64", "darwin/arm64", "darwin/amd64"} {
			a := expand(r.Assets[p], r.Tag)
			if a == "" || strings.ContainsAny(a, "{}") {
				t.Errorf("%s: no usable asset for %s (%q)", r.Name, p, a)
			}
			if !strings.HasSuffix(a, ".zip") && !strings.HasSuffix(a, ".tar.gz") {
				t.Errorf("%s: asset %q is neither .zip nor .tar.gz", r.Name, a)
			}
		}
		if r.Checksums == "" && r.Note == "" {
			t.Errorf("%s: tools without a checksums file must document why in Note", r.Name)
		}
		if r.Binary == "" || !strings.Contains(r.Repo, "/") {
			t.Errorf("%s: bad pin %+v", r.Name, r)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	body := []byte(`# generated
0000000000000000000000000000000000000000000000000000000000000001  a.zip
0000000000000000000000000000000000000000000000000000000000000002 *b.tar.gz
DEADBEEF00000000000000000000000000000000000000000000000000000003  dir/c.zip
notahash  d.zip

`)
	sums, err := parseChecksums(body)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"a.zip":    "0000000000000000000000000000000000000000000000000000000000000001",
		"b.tar.gz": "0000000000000000000000000000000000000000000000000000000000000002",
		"c.zip":    "deadbeef00000000000000000000000000000000000000000000000000000003",
	}
	if len(sums) != len(want) {
		t.Fatalf("sums = %v", sums)
	}
	for k, v := range want {
		if sums[k] != v {
			t.Errorf("%s = %q, want %q", k, sums[k], v)
		}
	}
	if _, err := parseChecksums([]byte("nothing here\n")); err == nil {
		t.Error("expected error for a file without sha256 entries")
	}
}

func TestParseGoVersionM(t *testing.T) {
	out := []byte("bin/gofumpt.exe: go1.27.0\n\tpath\tmvdan.cc/gofumpt\n\tmod\tmvdan.cc/gofumpt\tv0.11.0\th1:abc=\n\tbuild\t-buildmode=exe\n")
	if got := parseGoVersionM(out); got != "v0.11.0" {
		t.Errorf("parseGoVersionM = %q", got)
	}
	if got := parseGoVersionM([]byte("garbage")); got != "" {
		t.Errorf("parseGoVersionM(garbage) = %q", got)
	}
}

func TestSelection(t *testing.T) {
	s := selection("a, b", "b")
	if !s.wants("a") || s.wants("b") || s.wants("c") {
		t.Errorf("selection -only a,b -skip b: a=%v b=%v c=%v", s.wants("a"), s.wants("b"), s.wants("c"))
	}
	all := selection("", "")
	if !all.wants("anything") {
		t.Error("empty selection should want everything")
	}
}

func makeZip(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("nested/dir/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Create("nested/README.md"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct {
		name string
		body []byte
	}{{"top/LICENSE", []byte("mit")}, {"top/" + name, content}} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	dir := t.TempDir()
	content := []byte("#!/bin/sh\necho hi\n")
	zipPath := filepath.Join(dir, "a.zip")
	if err := os.WriteFile(zipPath, makeZip(t, "ztool", content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := extractBinary(zipPath, "a.zip", "ztool")
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("zip: %v %q", err, got)
	}
	if _, missingErr := extractBinary(zipPath, "a.zip", "missing"); missingErr == nil {
		t.Error("zip: expected error for a missing entry")
	}

	tgzPath := filepath.Join(dir, "b.tar.gz")
	if writeErr := os.WriteFile(tgzPath, makeTarGz(t, "ttool", content), 0o644); writeErr != nil {
		t.Fatal(writeErr)
	}
	got, err = extractBinary(tgzPath, "b.tar.gz", "ttool")
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("tar.gz: %v %q", err, got)
	}

	rawPath := filepath.Join(dir, "raw")
	if writeErr := os.WriteFile(rawPath, content, 0o644); writeErr != nil {
		t.Fatal(writeErr)
	}
	got, err = extractBinary(rawPath, "raw", "raw")
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("raw: %v %q", err, got)
	}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeRelease serves a GitHub-like release: download URLs plus the releases API.
type fakeRelease struct {
	assets    map[string][]byte // asset name -> bytes
	checksums string            // body of the checksums asset, "" if none
	digests   map[string]string // asset name -> "sha256:..." reported by the API ("" to omit)
	apiStatus int
}

func (f fakeRelease) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/o/r/releases/download/v1.0.0/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/o/r/releases/download/v1.0.0/")
		if name == "checksums.txt" && f.checksums != "" {
			_, _ = io.WriteString(w, f.checksums)
			return
		}
		if b, ok := f.assets[name]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/o/r/releases/tags/v1.0.0", func(w http.ResponseWriter, r *http.Request) {
		if f.apiStatus != 0 {
			w.WriteHeader(f.apiStatus)
			return
		}
		var parts []string
		for name := range f.assets {
			d := f.digests[name]
			if d == "" {
				parts = append(parts, fmt.Sprintf(`{"name":%q}`, name))
			} else {
				parts = append(parts, fmt.Sprintf(`{"name":%q,"digest":%q}`, name, d))
			}
		}
		fmt.Fprintf(w, `{"tag_name":"v1.0.0","assets":[%s]}`, strings.Join(parts, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	oldRel, oldAPI := releaseBaseURL, apiBaseURL
	releaseBaseURL, apiBaseURL = srv.URL, srv.URL
	t.Cleanup(func() { releaseBaseURL, apiBaseURL = oldRel, oldAPI })
	return srv
}

func platformKey() string { return runtime.GOOS + "/" + runtime.GOARCH }

func TestInstallReleaseTool_ChecksumsFile(t *testing.T) {
	content := []byte("binary-bytes")
	assetName := "ztool_1.0.0.zip"
	archive := makeZip(t, "ztool"+exeSuffix(), content)
	rel := fakeRelease{
		assets:    map[string][]byte{assetName: archive},
		checksums: sha(archive) + "  " + assetName + "\n",
	}
	rel.start(t)
	tool := ReleaseTool{
		Name: "ztool", Repo: "o/r", Tag: "v1.0.0", Binary: "ztool",
		Assets:    map[string]string{platformKey(): "ztool_{ver}.zip"},
		Checksums: "checksums.txt",
	}
	bin := t.TempDir()
	var log bytes.Buffer
	status, err := installReleaseTool(context.Background(), &http.Client{Timeout: 10 * time.Second}, bin, tool, false, false, &log)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, log.String())
	}
	if !strings.HasPrefix(status, "installed") || !strings.Contains(status, "checksums file") {
		t.Errorf("status = %q", status)
	}
	got, err := os.ReadFile(filepath.Join(bin, "ztool"+exeSuffix()))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("installed binary: %v %q", err, got)
	}
	if tag, sum := readPin(bin, "ztool"); tag != "v1.0.0" || sum != sha(archive) {
		t.Errorf("pin = %q %q", tag, sum)
	}
	// Second run is a no-op.
	status, err = installReleaseTool(context.Background(), http.DefaultClient, bin, tool, false, false, &log)
	if err != nil || status != "up-to-date" {
		t.Errorf("second install: %q %v", status, err)
	}
	// Leftover download temp files must not remain.
	entries, _ := os.ReadDir(bin)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".download") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestInstallReleaseTool_ChecksumMismatch(t *testing.T) {
	assetName := "ttool_1.0.0.tar.gz"
	archive := makeTarGz(t, "ttool"+exeSuffix(), []byte("bytes"))
	rel := fakeRelease{
		assets:    map[string][]byte{assetName: archive},
		checksums: strings.Repeat("0", 64) + "  " + assetName + "\n",
	}
	rel.start(t)
	tool := ReleaseTool{
		Name: "ttool", Repo: "o/r", Tag: "v1.0.0", Binary: "ttool",
		Assets:    map[string]string{platformKey(): "ttool_{ver}.tar.gz"},
		Checksums: "checksums.txt",
	}
	bin := t.TempDir()
	_, err := installReleaseTool(context.Background(), http.DefaultClient, bin, tool, false, true, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("expected mismatch error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(bin, "ttool"+exeSuffix())); statErr == nil {
		t.Error("binary was installed despite checksum mismatch")
	}
}

func TestInstallReleaseTool_APIDigest(t *testing.T) {
	assetName := "ztool_1.0.0.zip"
	archive := makeZip(t, "ztool"+exeSuffix(), []byte("x"))
	rel := fakeRelease{
		assets:  map[string][]byte{assetName: archive},
		digests: map[string]string{assetName: "sha256:" + sha(archive)},
	}
	rel.start(t)
	tool := ReleaseTool{
		Name: "ztool", Repo: "o/r", Tag: "v1.0.0", Binary: "ztool", Note: "no checksums file",
		Assets: map[string]string{platformKey(): "ztool_{ver}.zip"},
	}
	bin := t.TempDir()
	status, err := installReleaseTool(context.Background(), http.DefaultClient, bin, tool, false, false, io.Discard)
	if err != nil || !strings.Contains(status, "GitHub API asset digest") {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestInstallReleaseTool_UnverifiableRequiresFlag(t *testing.T) {
	assetName := "ztool_1.0.0.zip"
	archive := makeZip(t, "ztool"+exeSuffix(), []byte("x"))
	rel := fakeRelease{assets: map[string][]byte{assetName: archive}} // API reports no digest
	rel.start(t)
	tool := ReleaseTool{
		Name: "ztool", Repo: "o/r", Tag: "v1.0.0", Binary: "ztool", Note: "no checksums file",
		Assets: map[string]string{platformKey(): "ztool_{ver}.zip"},
	}
	bin := t.TempDir()
	if _, err := installReleaseTool(context.Background(), http.DefaultClient, bin, tool, false, false, io.Discard); err == nil || !strings.Contains(err.Error(), "-allow-unverified") {
		t.Fatalf("expected refusal without -allow-unverified, got %v", err)
	}
	status, err := installReleaseTool(context.Background(), http.DefaultClient, bin, tool, false, true, io.Discard)
	if err != nil || status != "installed (unverified)" {
		t.Fatalf("with -allow-unverified: status=%q err=%v", status, err)
	}
}

func TestInstallReleaseTool_APIErrorFailsClosed(t *testing.T) {
	assetName := "ztool_1.0.0.zip"
	archive := makeZip(t, "ztool"+exeSuffix(), []byte("x"))
	rel := fakeRelease{assets: map[string][]byte{assetName: archive}, apiStatus: http.StatusForbidden}
	rel.start(t)
	tool := ReleaseTool{
		Name: "ztool", Repo: "o/r", Tag: "v1.0.0", Binary: "ztool", Note: "no checksums file",
		Assets: map[string]string{platformKey(): "ztool_{ver}.zip"},
	}
	if _, err := installReleaseTool(context.Background(), http.DefaultClient, t.TempDir(), tool, false, false, io.Discard); err == nil {
		t.Fatal("expected failure when the API digest cannot be fetched")
	}
}

func TestInstallReleaseTool_UnsupportedPlatform(t *testing.T) {
	tool := ReleaseTool{Name: "x", Repo: "o/r", Tag: "v1.0.0", Binary: "x", Assets: map[string]string{"plan9/mips": "x.zip"}}
	status, err := installReleaseTool(context.Background(), http.DefaultClient, t.TempDir(), tool, false, false, io.Discard)
	if err == nil || status != "unsupported" {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestResolve(t *testing.T) {
	bin := t.TempDir()
	if _, _, err := resolve(bin, "definitely-not-a-real-tool-name"); err == nil {
		t.Error("expected error for a missing tool")
	}
	local := filepath.Join(bin, "mytool"+exeSuffix())
	if err := os.WriteFile(local, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe, fromPath, err := resolve(bin, "mytool")
	if err != nil || fromPath || exe != local {
		t.Errorf("resolve = %q %v %v", exe, fromPath, err)
	}
}

func TestListIncludesEveryPin(t *testing.T) {
	var buf bytes.Buffer
	if code := cmdList(&buf, io.Discard); code != 0 {
		t.Fatalf("cmdList exit %d", code)
	}
	for _, n := range allNames() {
		if !strings.Contains(buf.String(), n+"\t") && !strings.Contains(buf.String(), n+" ") {
			t.Errorf("list output missing %s", n)
		}
	}
}
