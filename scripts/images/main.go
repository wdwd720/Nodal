// Command images builds one immutable OCI image per cmd/<name> from the shared
// build/Dockerfile, tags it <registry>/<name>:<tag> and optionally pushes it.
// After a push it records the registry digests in dist/images.json and
// dist/images.txt so the release workflow signs, attests and scans exactly the
// bytes that were pushed (PART 143).
//
// Usage:
//
//	go run ./scripts/images -tag <git-sha> [-registry ghcr.io/org/repo] [-push] [-pull]
//	                        [-only api,migrate] [-platform linux/amd64] [-dry-run]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Options controls a build run.
type Options struct {
	Root       string
	Tag        string
	Registry   string
	Dockerfile string
	Context    string
	Platform   string
	Revision   string
	Created    string
	Pull       bool
	Push       bool
}

// Record describes one built (and possibly pushed) image.
type Record struct {
	Cmd    string `json:"cmd"`
	Image  string `json:"image"`
	Tag    string `json:"tag"`
	Ref    string `json:"ref"`              // image:tag
	Digest string `json:"digest,omitempty"` // sha256:... after push
}

// PinnedRef returns image@digest when known, otherwise image:tag.
func (r Record) PinnedRef() string {
	if r.Digest != "" {
		return r.Image + "@" + r.Digest
	}
	return r.Ref
}

// DiscoverCmds returns the names of cmd/<name> directories containing a Go
// main package (at least one non-test .go file), sorted.
func DiscoverCmds(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, "cmd", e.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			n := f.Name()
			if !f.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
				names = append(names, e.Name())
				break
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// BuildArgs returns the docker build argument vector for one command.
func BuildArgs(o Options, cmd string) []string {
	args := []string{
		"build",
		"--file", o.Dockerfile,
		"--build-arg", "CMD=" + cmd,
		"--build-arg", "VERSION=" + o.Tag,
		"--build-arg", "REVISION=" + o.Revision,
		"--build-arg", "CREATED=" + o.Created,
		"--label", "org.opencontainers.image.revision=" + o.Revision,
		"--tag", ImageRef(o, cmd),
	}
	if o.Pull {
		args = append(args, "--pull")
	}
	if o.Platform != "" {
		args = append(args, "--platform", o.Platform)
	}
	return append(args, o.Context)
}

// ImageRef returns <registry>/<cmd>:<tag>.
func ImageRef(o Options, cmd string) string {
	return ImageName(o, cmd) + ":" + o.Tag
}

// ImageName returns <registry>/<cmd>.
func ImageName(o Options, cmd string) string {
	return strings.TrimSuffix(o.Registry, "/") + "/" + cmd
}

// PickDigest selects the sha256 digest for image from docker's RepoDigests list.
func PickDigest(repoDigests []string, image string) (string, bool) {
	for _, rd := range repoDigests {
		if strings.HasPrefix(rd, image+"@") {
			return strings.TrimPrefix(rd, image+"@"), true
		}
	}
	return "", false
}

// runner executes a command; run streams output, capture returns stdout.
type runner func(name string, args ...string) ([]byte, error)

func dockerRunner(ctx context.Context, dir string, out io.Writer) runner {
	return func(name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- docker invocation assembled by BuildArgs
		cmd.Dir = dir
		cmd.Stdin = os.Stdin
		cmd.Stdout = out
		cmd.Stderr = os.Stderr
		return nil, cmd.Run()
	}
}

func dockerCapture(ctx context.Context, dir string) runner {
	return func(name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- docker invocation assembled by this program
		cmd.Dir = dir
		cmd.Stderr = os.Stderr
		return cmd.Output()
	}
}

// Build builds (and pushes when o.Push) one image per cmd and returns the records.
func Build(o Options, cmds []string, run, capture runner, log io.Writer) ([]Record, error) {
	var records []Record
	for _, c := range cmds {
		rec := Record{Cmd: c, Image: ImageName(o, c), Tag: o.Tag, Ref: ImageRef(o, c)}
		fmt.Fprintf(log, "==> building %s\n", rec.Ref)
		if _, err := run("docker", BuildArgs(o, c)...); err != nil {
			return records, fmt.Errorf("docker build %s: %w", rec.Ref, err)
		}
		if o.Push {
			fmt.Fprintf(log, "==> pushing %s\n", rec.Ref)
			if _, err := run("docker", "push", rec.Ref); err != nil {
				return records, fmt.Errorf("docker push %s: %w", rec.Ref, err)
			}
			out, err := capture("docker", "image", "inspect", "--format", "{{json .RepoDigests}}", rec.Ref)
			if err != nil {
				return records, fmt.Errorf("docker image inspect %s: %w", rec.Ref, err)
			}
			var digests []string
			if err := json.Unmarshal(out, &digests); err != nil {
				return records, fmt.Errorf("parse RepoDigests for %s: %w", rec.Ref, err)
			}
			d, ok := PickDigest(digests, rec.Image)
			if !ok {
				return records, fmt.Errorf("no registry digest recorded for %s after push (RepoDigests=%v)", rec.Ref, digests)
			}
			rec.Digest = d
		}
		records = append(records, rec)
	}
	return records, nil
}

// WriteManifest writes records as JSON to jsonPath and pinned refs, one per
// line, to the sibling .txt file.
func WriteManifest(jsonPath string, records []Record) error {
	if err := os.MkdirAll(filepath.Dir(jsonPath), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(jsonPath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	var sb strings.Builder
	for _, r := range records {
		sb.WriteString(r.PinnedRef())
		sb.WriteByte('\n')
	}
	txt := strings.TrimSuffix(jsonPath, filepath.Ext(jsonPath)) + ".txt"
	return os.WriteFile(txt, []byte(sb.String()), 0o600)
}

func gitRevision(ctx context.Context, root string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("images", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tag := fs.String("tag", "", "image tag, normally the git SHA (required)")
	registry := fs.String("registry", "controlplane", "image name prefix, e.g. ghcr.io/org/repo")
	only := fs.String("only", "", "comma-separated subset of cmd/<name> to build")
	push := fs.Bool("push", false, "push images after building and record digests")
	pull := fs.Bool("pull", false, "always pull newer base images (docker build --pull)")
	platform := fs.String("platform", "", "target platform, e.g. linux/amd64 (default: docker's default)")
	revision := fs.String("revision", "", "value for org.opencontainers.image.revision (default: git rev-parse HEAD)")
	dockerfile := fs.String("dockerfile", filepath.Join("build", "Dockerfile"), "shared Dockerfile")
	out := fs.String("out", filepath.Join("dist", "images.json"), "manifest path (a sibling .txt with pinned refs is written too)")
	dryRun := fs.Bool("dry-run", false, "print the docker commands without running them")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tag == "" {
		fmt.Fprintln(stderr, "images: -tag is required (use the git SHA)")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	root := repoRoot()
	if *revision == "" {
		*revision = gitRevision(ctx, root)
	}
	o := Options{
		Root: root, Tag: *tag, Registry: *registry, Dockerfile: *dockerfile, Context: ".",
		Platform: *platform, Revision: *revision, Created: time.Now().UTC().Format(time.RFC3339),
		Pull: *pull, Push: *push,
	}
	cmds, err := DiscoverCmds(root)
	if err != nil {
		fmt.Fprintf(stderr, "images: %v\n", err)
		return 1
	}
	if *only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			want[strings.TrimSpace(n)] = true
		}
		kept := cmds[:0]
		for _, c := range cmds {
			if want[c] {
				kept = append(kept, c)
				delete(want, c)
			}
		}
		cmds = kept
		for missing := range want {
			fmt.Fprintf(stderr, "images: no cmd/%s directory with Go sources\n", missing)
			return 2
		}
	}
	if len(cmds) == 0 {
		fmt.Fprintln(stderr, "images: no commands found under cmd/")
		return 1
	}

	if *dryRun {
		for _, c := range cmds {
			fmt.Fprintln(stdout, "docker "+strings.Join(BuildArgs(o, c), " "))
			if o.Push {
				fmt.Fprintln(stdout, "docker push "+ImageRef(o, c))
			}
		}
		return 0
	}
	if _, lookErr := exec.LookPath("docker"); lookErr != nil {
		fmt.Fprintln(stderr, "images: docker is not on PATH")
		return 1
	}
	records, buildErr := Build(o, cmds, dockerRunner(ctx, root, stderr), dockerCapture(ctx, root), stderr)
	if buildErr != nil {
		fmt.Fprintf(stderr, "images: %v\n", buildErr)
	}
	if len(records) > 0 {
		if werr := WriteManifest(filepath.Join(root, *out), records); werr != nil {
			fmt.Fprintf(stderr, "images: write manifest: %v\n", werr)
			return 1
		}
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CMD\tIMAGE\tDIGEST")
	for _, r := range records {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Cmd, r.Ref, r.Digest)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "images: write output: %v\n", err)
		return 1
	}
	if buildErr != nil {
		return 1
	}
	return 0
}
