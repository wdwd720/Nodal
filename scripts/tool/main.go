// Command tool installs and runs the repository's pinned developer tools.
//
//	go run ./scripts/tool install [-only a,b] [-skip a,b] [-force] [-allow-unverified]
//	go run ./scripts/tool list
//	go run ./scripts/tool <name> [args...]
//
// Every version is declared once, in the goTools and releaseTools tables below.
// Go tools are built with `go install pkg@version` into ./bin (GOBIN). Prebuilt
// tools are downloaded from GitHub releases and their SHA-256 is verified against
// the project's checksums file before anything is extracted; projects that publish
// no checksums file are verified against the per-asset digest served by the GitHub
// releases API, and anything else is refused unless -allow-unverified is given.
//
// Running a tool never installs it: a missing tool prints the install command and
// exits 1, so CI and developers always run exactly the pinned version.
package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"
)

// GoTool is installed with `go install Package@Version` into ./bin.
type GoTool struct {
	Name    string // binary name
	Package string // main package import path
	Version string // module version (go install pkg@Version)
}

// ReleaseTool is a prebuilt binary downloaded from a GitHub release.
type ReleaseTool struct {
	Name      string
	Repo      string            // owner/repo
	Tag       string            // release tag exactly as published
	Assets    map[string]string // "GOOS/GOARCH" -> asset name; {tag} and {ver} (tag without "v") are expanded
	Checksums string            // asset holding "<sha256>  <asset>" lines; "" when the project publishes none
	Binary    string            // file name inside the archive, without .exe
	Note      string
}

// goTools are pure-Go tools. Versions checked against proxy.golang.org @latest on 2026-09-05.
var goTools = []GoTool{
	{Name: "staticcheck", Package: "honnef.co/go/tools/cmd/staticcheck", Version: "v0.8.1"}, // staticcheck 2026.2.1
	{Name: "govulncheck", Package: "golang.org/x/vuln/cmd/govulncheck", Version: "v1.7.0"},
	{Name: "gofumpt", Package: "mvdan.cc/gofumpt", Version: "v0.11.0"},
	{Name: "goimports", Package: "golang.org/x/tools/cmd/goimports", Version: "v0.49.0"},
	{Name: "buf", Package: "github.com/bufbuild/buf/cmd/buf", Version: "v1.72.0"},
	{Name: "oapi-codegen", Package: "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen", Version: "v2.8.0"},
	{Name: "protoc-gen-go", Package: "google.golang.org/protobuf/cmd/protoc-gen-go", Version: "v1.36.12"},
	{Name: "protoc-gen-go-grpc", Package: "google.golang.org/grpc/cmd/protoc-gen-go-grpc", Version: "v1.6.2"},
	{Name: "gosec", Package: "github.com/securego/gosec/v2/cmd/gosec", Version: "v2.29.0"},
	{Name: "golangci-lint", Package: "github.com/golangci/golangci-lint/v2/cmd/golangci-lint", Version: "v2.13.2"},
}

// releaseTools need cgo or ship as prebuilt binaries only (SB-005). Tags and asset
// names were taken from each project's latest GitHub release on 2026-09-05.
var releaseTools = []ReleaseTool{
	{
		Name: "sqlc", Repo: "sqlc-dev/sqlc", Tag: "v1.31.1", Binary: "sqlc",
		Assets: map[string]string{
			"windows/amd64": "sqlc_{ver}_windows_amd64.zip",
			"linux/amd64":   "sqlc_{ver}_linux_amd64.tar.gz",
			"darwin/arm64":  "sqlc_{ver}_darwin_arm64.tar.gz",
			"darwin/amd64":  "sqlc_{ver}_darwin_amd64.tar.gz",
		},
		// sqlc publishes no checksums file (verified 2026-09-05: the release lists only
		// archives), so the download is verified against the sha256 digest the GitHub
		// releases API reports for the asset.
		Checksums: "",
		Note:      "no upstream checksums file; verified via GitHub API asset digest",
	},
	{
		Name: "gitleaks", Repo: "gitleaks/gitleaks", Tag: "v8.30.1", Binary: "gitleaks",
		Assets: map[string]string{
			"windows/amd64": "gitleaks_{ver}_windows_x64.zip",
			"linux/amd64":   "gitleaks_{ver}_linux_x64.tar.gz",
			"darwin/arm64":  "gitleaks_{ver}_darwin_arm64.tar.gz",
			"darwin/amd64":  "gitleaks_{ver}_darwin_x64.tar.gz",
		},
		Checksums: "gitleaks_{ver}_checksums.txt",
	},
	{
		Name: "trivy", Repo: "aquasecurity/trivy", Tag: "v0.74.0", Binary: "trivy",
		Assets: map[string]string{
			"windows/amd64": "trivy_{ver}_windows-64bit.zip",
			"linux/amd64":   "trivy_{ver}_Linux-64bit.tar.gz",
			"darwin/arm64":  "trivy_{ver}_macOS-ARM64.tar.gz",
			"darwin/amd64":  "trivy_{ver}_macOS-64bit.tar.gz",
		},
		Checksums: "trivy_{ver}_checksums.txt",
	},
	{
		Name: "syft", Repo: "anchore/syft", Tag: "v1.51.1", Binary: "syft",
		Assets: map[string]string{
			"windows/amd64": "syft_{ver}_windows_amd64.zip",
			"linux/amd64":   "syft_{ver}_linux_amd64.tar.gz",
			"darwin/arm64":  "syft_{ver}_darwin_arm64.tar.gz",
			"darwin/amd64":  "syft_{ver}_darwin_amd64.tar.gz",
		},
		Checksums: "syft_{ver}_checksums.txt",
	},
	{
		Name: "k6", Repo: "grafana/k6", Tag: "v2.2.0", Binary: "k6",
		Assets: map[string]string{
			"windows/amd64": "k6-{tag}-windows-amd64.zip",
			"linux/amd64":   "k6-{tag}-linux-amd64.tar.gz",
			"darwin/arm64":  "k6-{tag}-macos-arm64.zip",
			"darwin/amd64":  "k6-{tag}-macos-amd64.zip",
		},
		Checksums: "k6-{tag}-checksums.txt", // archives contain a k6-<tag>-<os>-<arch>/ directory
	},
}

// URL bases are variables so tests can point them at an httptest server.
var (
	releaseBaseURL = "https://github.com"
	apiBaseURL     = "https://api.github.com"
)

const pinDir = ".pins" // bin/.pins/<name> records "<tag> <sha256>" of the installed release binary

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "install":
		os.Exit(cmdInstall(os.Args[2:]))
	case "list":
		os.Exit(cmdList(os.Stdout, os.Stderr))
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		os.Exit(cmdRun(os.Args[1], os.Args[2:]))
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage:
  go run ./scripts/tool install [-only a,b] [-skip a,b] [-force] [-allow-unverified]
  go run ./scripts/tool list
  go run ./scripts/tool <name> [args...]

pinned tools: %s
`, strings.Join(allNames(), ", "))
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

func cmdRun(name string, args []string) int {
	if !isKnown(name) {
		fmt.Fprintf(os.Stderr, "tool: %q is not a pinned tool (known: %s)\n", name, strings.Join(allNames(), ", "))
		return 2
	}
	bin := binDir()
	exe, fromPath, err := resolve(bin, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tool: %v\n  install it with: go run ./scripts/tool install -only %s   (or: make tools)\n", err, name)
		return 1
	}
	if fromPath {
		fmt.Fprintf(os.Stderr, "tool: note: using %s from PATH; ./bin has no pinned copy (run: go run ./scripts/tool install -only %s)\n", exe, name)
	}
	// Running the requested pinned tool with the caller's arguments is the purpose of this command.
	cmd := exec.CommandContext(context.Background(), exe, args...) // #nosec G702 G204
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	signal.Ignore(os.Interrupt) // Ctrl-C goes to the child, which owns the terminal
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "tool: %s: %v\n", name, err)
		return 1
	}
	return 0
}

// resolve finds bin/<name>[.exe] or falls back to PATH.
func resolve(bin, name string) (exe string, fromPath bool, err error) {
	local := filepath.Join(bin, name+exeSuffix())
	if st, statErr := os.Stat(local); statErr == nil && !st.IsDir() { // #nosec G703 -- name is validated against the pinned table
		return local, false, nil
	}
	if p, lookErr := exec.LookPath(name); lookErr == nil {
		return p, true, nil
	}
	return "", false, fmt.Errorf("%s is not installed in %s and not on PATH", name, bin)
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdList(w, stderr io.Writer) int {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	fmt.Fprintf(w, "platform: %s   install dir: %s\n\n", platform, binDir())
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tVERSION\tSOURCE\tVERIFICATION")
	for _, t := range goTools {
		fmt.Fprintf(tw, "%s\tgo-install\t%s\t%s\tgo module checksum database\n", t.Name, t.Version, t.Package)
	}
	for _, t := range releaseTools {
		src := t.Repo + " " + t.assetFor(platform)
		if t.Assets[platform] == "" {
			src = t.Repo + " (no asset for " + platform + ")"
		}
		fmt.Fprintf(tw, "%s\tgithub-release\t%s\t%s\t%s\n", t.Name, t.Tag, src, t.verificationLabel())
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "tool: write output: %v\n", err)
		return 1
	}
	return 0
}

func (t ReleaseTool) assetFor(platform string) string {
	return expand(t.Assets[platform], t.Tag)
}

func (t ReleaseTool) verificationLabel() string {
	if t.Checksums != "" {
		return "sha256 from " + expand(t.Checksums, t.Tag)
	}
	return "sha256 from GitHub API asset digest (" + t.Note + ")"
}

// expand substitutes {tag} and {ver} placeholders in an asset name template.
func expand(tmpl, tag string) string {
	r := strings.NewReplacer("{tag}", tag, "{ver}", strings.TrimPrefix(tag, "v"))
	return r.Replace(tmpl)
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

type outcome struct {
	Name, Version, Status string
	Err                   error
}

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	only := fs.String("only", "", "comma-separated tool names to install (default: all)")
	skip := fs.String("skip", "", "comma-separated tool names to skip")
	force := fs.Bool("force", false, "reinstall even when the pinned version is already present")
	allowUnverified := fs.Bool("allow-unverified", false, "accept release assets whose SHA-256 cannot be verified")
	binFlag := fs.String("bin", "", "install directory (default: ./bin at the repository root, or $CP_TOOLS_BIN)")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-download timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bin := binDir()
	if *binFlag != "" {
		abs, err := filepath.Abs(*binFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tool: %v\n", err)
			return 2
		}
		bin = abs
	}
	if err := os.MkdirAll(bin, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "tool: %v\n", err)
		return 1
	}
	selected := selection(*only, *skip)
	for name := range selected.only {
		if !isKnown(name) {
			fmt.Fprintf(os.Stderr, "tool: unknown tool %q in -only (known: %s)\n", name, strings.Join(allNames(), ", "))
			return 2
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := &http.Client{Timeout: *timeout}
	goExe := goExecutable()
	fmt.Fprintf(os.Stderr, "installing pinned tools into %s (%s/%s)\n", bin, runtime.GOOS, runtime.GOARCH)

	var results []outcome
	for _, t := range goTools {
		if !selected.wants(t.Name) {
			continue
		}
		fmt.Fprintf(os.Stderr, "\n==> %s %s (%s)\n", t.Name, t.Version, t.Package)
		status, err := installGoTool(ctx, goExe, bin, t, *force, os.Stderr)
		results = append(results, outcome{t.Name, t.Version, status, err})
		if ctx.Err() != nil {
			break
		}
	}
	for _, t := range releaseTools {
		if !selected.wants(t.Name) || ctx.Err() != nil {
			continue
		}
		fmt.Fprintf(os.Stderr, "\n==> %s %s (github.com/%s)\n", t.Name, t.Tag, t.Repo)
		status, err := installReleaseTool(ctx, client, bin, t, *force, *allowUnverified, os.Stderr)
		results = append(results, outcome{t.Name, t.Tag, status, err})
	}

	fmt.Fprintln(os.Stderr)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tVERSION\tSTATUS")
	failed := 0
	for _, r := range results {
		status := r.Status
		if r.Err != nil {
			failed++
			status = "FAILED: " + r.Err.Error()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, r.Version, status)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "tool: write output: %v\n", err)
		return 1
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "tool: interrupted")
		return 130
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "tool: %d tool(s) failed to install\n", failed)
		return 1
	}
	return 0
}

type selected struct {
	only, skip map[string]bool
}

func selection(only, skip string) selected {
	s := selected{only: map[string]bool{}, skip: map[string]bool{}}
	for _, n := range strings.Split(only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			s.only[n] = true
		}
	}
	for _, n := range strings.Split(skip, ",") {
		if n = strings.TrimSpace(n); n != "" {
			s.skip[n] = true
		}
	}
	return s
}

func (s selected) wants(name string) bool {
	if s.skip[name] {
		return false
	}
	return len(s.only) == 0 || s.only[name]
}

// installGoTool runs `go install pkg@version` with GOBIN=bin unless the pinned
// version is already present.
func installGoTool(ctx context.Context, goExe, bin string, t GoTool, force bool, log io.Writer) (string, error) {
	exe := filepath.Join(bin, t.Name+exeSuffix())
	if !force {
		if v := installedModuleVersion(ctx, goExe, exe); v == t.Version {
			fmt.Fprintf(log, "already at %s\n", v)
			return "up-to-date", nil
		}
	}
	cmd := exec.CommandContext(ctx, goExe, "install", t.Package+"@"+t.Version) // #nosec G204 -- package and version come from the pinned table
	// GOFLAGS such as -mod=vendor break module-mode installs of a specific version.
	cmd.Env = append(envWithout("GOBIN", "GOFLAGS"), "GOBIN="+bin)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return "failed", fmt.Errorf("go install %s@%s: %w", t.Package, t.Version, err)
	}
	if v := installedModuleVersion(ctx, goExe, exe); v != t.Version {
		return "failed", fmt.Errorf("%s reports module version %q after install, want %s", exe, v, t.Version)
	}
	return "installed", nil
}

// installedModuleVersion returns the main module version embedded in exe via
// `go version -m`, or "" when exe is missing or unreadable.
func installedModuleVersion(ctx context.Context, goExe, exe string) string {
	if _, err := os.Stat(exe); err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, goExe, "version", "-m", exe).Output() // #nosec G204 -- inspects a binary under bin/
	if err != nil {
		return ""
	}
	return parseGoVersionM(out)
}

// parseGoVersionM extracts the version from the "mod" line of `go version -m` output.
func parseGoVersionM(out []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 3 && fields[0] == "mod" {
			return fields[2]
		}
	}
	return ""
}

// installReleaseTool downloads, verifies and extracts a prebuilt release binary.
func installReleaseTool(ctx context.Context, client *http.Client, bin string, t ReleaseTool, force, allowUnverified bool, log io.Writer) (string, error) {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	tmpl, ok := t.Assets[platform]
	if !ok {
		return "unsupported", fmt.Errorf("no pinned %s asset for %s", t.Name, platform)
	}
	asset := expand(tmpl, t.Tag)
	exe := filepath.Join(bin, t.Name+exeSuffix())
	if !force {
		if tag, _ := readPin(bin, t.Name); tag == t.Tag {
			if _, err := os.Stat(exe); err == nil {
				fmt.Fprintf(log, "already at %s\n", t.Tag)
				return "up-to-date", nil
			}
		}
	}

	expected, method, err := expectedDigest(ctx, client, t, asset)
	if err != nil {
		if !allowUnverified {
			return "failed", err
		}
		fmt.Fprintf(log, "warning: %v; continuing unverified because -allow-unverified was given\n", err)
		expected, method = "", "unverified"
	}
	if expected == "" && !allowUnverified {
		return "failed", fmt.Errorf("no SHA-256 available for %s (%s); rerun with -allow-unverified to accept an unverified binary", asset, t.Note)
	}
	if expected == "" {
		method = "unverified"
		fmt.Fprintf(log, "warning: installing %s without checksum verification\n", asset)
	}

	url := downloadURL(t.Repo, t.Tag, asset)
	fmt.Fprintf(log, "downloading %s\n", url)
	archive, actual, err := downloadToTemp(ctx, client, url, bin, t.Name)
	if err != nil {
		return "failed", err
	}
	defer func() { _ = os.Remove(archive) }()
	if expected != "" {
		if actual != expected {
			return "failed", fmt.Errorf("SHA-256 mismatch for %s: got %s, want %s (%s)", asset, actual, expected, method)
		}
		fmt.Fprintf(log, "sha256 %s verified (%s)\n", actual, method)
	}
	data, err := extractBinary(archive, asset, t.Binary+exeSuffix())
	if err != nil {
		return "failed", fmt.Errorf("%s: %w", asset, err)
	}
	if err := writeExecutable(exe, data); err != nil {
		return "failed", err
	}
	if err := writePin(bin, t.Name, t.Tag, actual); err != nil {
		return "failed", err
	}
	return "installed (" + method + ")", nil
}

// expectedDigest returns the hex SHA-256 the asset must match and how it was
// obtained. An empty digest with a nil error means the project offers none.
func expectedDigest(ctx context.Context, client *http.Client, t ReleaseTool, asset string) (string, string, error) {
	if t.Checksums != "" {
		name := expand(t.Checksums, t.Tag)
		body, err := fetchSmall(ctx, client, downloadURL(t.Repo, t.Tag, name), 4<<20)
		if err != nil {
			return "", "", fmt.Errorf("fetch %s: %w", name, err)
		}
		sums, err := parseChecksums(body)
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", name, err)
		}
		h, ok := sums[asset]
		if !ok {
			return "", "", fmt.Errorf("%s has no entry for %s", name, asset)
		}
		return h, "checksums file " + name, nil
	}
	h, err := apiAssetDigest(ctx, client, t.Repo, t.Tag, asset)
	if err != nil {
		return "", "", fmt.Errorf("github api digest for %s: %w", asset, err)
	}
	if h == "" {
		return "", "none", nil
	}
	return h, "GitHub API asset digest", nil
}

// parseChecksums parses "<hex>  <name>" / "<hex> *<name>" lines into name -> hex.
func parseChecksums(body []byte) (map[string]string, error) {
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != 64 {
			continue
		}
		if _, err := hex.DecodeString(sum); err != nil {
			continue
		}
		name := strings.TrimPrefix(strings.Join(fields[1:], " "), "*")
		sums[path.Base(name)] = sum
	}
	if len(sums) == 0 {
		return nil, errors.New("no sha256 entries found")
	}
	return sums, nil
}

// apiAssetDigest returns the hex sha256 the GitHub releases API reports for an
// asset ("" when the API has no digest for it).
func apiAssetDigest(ctx context.Context, client *http.Client, repo, tag, asset string) (string, error) {
	url := apiBaseURL + "/repos/" + repo + "/releases/tags/" + tag
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "controlplane-tool-installer")
	if tok := firstEnv("GITHUB_TOKEN", "GH_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %s (set GITHUB_TOKEN to avoid API rate limits)", url, resp.Status)
	}
	var rel struct {
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rel); err != nil {
		return "", fmt.Errorf("decode %s: %w", url, err)
	}
	for _, a := range rel.Assets {
		if a.Name == asset {
			return strings.ToLower(strings.TrimPrefix(a.Digest, "sha256:")), nil
		}
	}
	return "", fmt.Errorf("release %s has no asset named %s", tag, asset)
}

func downloadURL(repo, tag, asset string) string {
	return releaseBaseURL + "/" + repo + "/releases/download/" + tag + "/" + asset
}

// fetchSmall GETs url into memory, capped at limit bytes.
func fetchSmall(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	resp, err := httpGet(ctx, client, url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// downloadToTemp streams url into a temporary file inside dir and returns its
// path and hex SHA-256.
func downloadToTemp(ctx context.Context, client *http.Client, url, dir, name string) (string, string, error) {
	resp, err := httpGet(ctx, client, url)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	f, err := os.CreateTemp(dir, "."+name+"-*.download")
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", "", fmt.Errorf("download %s: %w", url, err)
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// httpGet performs a GET with up to three attempts on transport errors / 5xx.
func httpGet(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "controlplane-tool-installer")
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil, fmt.Errorf("%s: HTTP %s", url, resp.Status)
			}
			last = fmt.Errorf("%s: HTTP %s", url, resp.Status)
		} else {
			last = err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return nil, last
}

// extractBinary returns the bytes of the file named want inside the archive at
// archivePath (zip, tar.gz, or a raw binary when the asset has neither suffix).
// Entries are matched by base name so archives with a top-level directory work.
// archivePath is always a temp file this program just created under bin/.
func extractBinary(archivePath, asset, want string) ([]byte, error) {
	switch {
	case strings.HasSuffix(asset, ".zip"):
		zr, err := zip.OpenReader(archivePath)
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		for _, f := range zr.File {
			if f.FileInfo().IsDir() || path.Base(f.Name) != want {
				continue
			}
			return readZipEntry(f)
		}
	case strings.HasSuffix(asset, ".tar.gz"), strings.HasSuffix(asset, ".tgz"):
		f, err := os.Open(archivePath) // #nosec G304 -- temp file created by downloadToTemp
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer func() { _ = gz.Close() }()
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if hdr.Typeflag == tar.TypeReg && path.Base(hdr.Name) == want {
				return io.ReadAll(tr)
			}
		}
	default:
		return os.ReadFile(archivePath) // #nosec G304 -- temp file created by downloadToTemp
	}
	return nil, fmt.Errorf("archive contains no file named %s", want)
}

func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// writeExecutable atomically writes data to dest with the execute bit set.
func writeExecutable(dest string, data []byte) error {
	tmp := dest + ".tmp"
	// Installed tools must be executable; bin/ is developer-local and git-ignored.
	if err := os.WriteFile(tmp, data, 0o755); err != nil { // #nosec G306
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil { // #nosec G302
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install %s: %w (is it running?)", dest, err)
	}
	return nil
}

func readPin(bin, name string) (tag, sum string) {
	data, err := os.ReadFile(filepath.Join(bin, pinDir, name)) // #nosec G304 -- name comes from the pinned table
	if err != nil {
		return "", ""
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "", ""
	}
	if len(fields) > 1 {
		sum = fields[1]
	}
	return fields[0], sum
}

func writePin(bin, name, tag, sum string) error {
	if err := os.MkdirAll(filepath.Join(bin, pinDir), 0o750); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bin, pinDir, name), []byte(tag+" "+sum+"\n"), 0o600)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func allNames() []string {
	names := make([]string, 0, len(goTools)+len(releaseTools))
	for _, t := range goTools {
		names = append(names, t.Name)
	}
	for _, t := range releaseTools {
		names = append(names, t.Name)
	}
	return names
}

func isKnown(name string) bool {
	for _, n := range allNames() {
		if n == name {
			return true
		}
	}
	return false
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// binDir is $CP_TOOLS_BIN or <repo root>/bin, where the root is the nearest
// directory above the working directory containing go.mod.
func binDir() string {
	if v := os.Getenv("CP_TOOLS_BIN"); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}
	return filepath.Join(repoRoot(), "bin")
}

func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

// goExecutable is the `go` on PATH, which is the toolchain that ran `go run`.
func goExecutable() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return "go"
}

func envWithout(keys ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") || (runtime.GOOS == "windows" && strings.HasPrefix(strings.ToUpper(kv), strings.ToUpper(k)+"=")) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
