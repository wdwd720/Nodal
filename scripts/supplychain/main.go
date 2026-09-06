// Command supplychain wraps the SBOM and container-scan steps so make targets
// behave identically on every platform.
//
//	go run ./scripts/supplychain sbom            # syft SBOM → dist/sbom.spdx.json
//	go run ./scripts/supplychain scan            # trivy image scan of every ref in dist/images.txt
//	go run ./scripts/supplychain scan -severity HIGH,CRITICAL
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var err error
	switch args[0] {
	case "sbom":
		err = sbom(ctx, args[1:])
	case "scan":
		err = scan(ctx, args[1:])
	default:
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "supplychain:", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: supplychain sbom | scan [-severity HIGH,CRITICAL] [-images dist/images.txt]")
}

func toolPath(name string) (string, error) {
	p := filepath.Join("bin", name)
	if runtime.GOOS == "windows" {
		p += ".exe"
	}
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if lp, err := exec.LookPath(name); err == nil {
		return lp, nil
	}
	return "", fmt.Errorf("%s not found in ./bin or PATH; run `make tools`", name)
}

func execTool(ctx context.Context, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- dev/CI tool: bin resolves to ./bin/<pinned tool> or a PATH lookup of a literal name; args are assembled by this program, and exec.Command never invokes a shell
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sbom(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sbom", flag.ContinueOnError)
	out := fs.String("out", filepath.Join("dist", "sbom.spdx.json"), "output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o750); err != nil {
		return err
	}
	syft, err := toolPath("syft")
	if err != nil {
		return err
	}
	if err := execTool(ctx, syft, "dir:.", "-o", "spdx-json="+*out); err != nil {
		return fmt.Errorf("syft: %w", err)
	}
	fmt.Println("sbom written to", *out)
	return nil
}

func scan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	severity := fs.String("severity", "HIGH,CRITICAL", "severities that fail the scan")
	list := fs.String("images", filepath.Join("dist", "images.txt"), "file listing image refs, one per line")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f, err := os.Open(filepath.Clean(*list))
	if err != nil {
		return fmt.Errorf("open %s (run `make images` first): %w", *list, err)
	}
	defer func() { _ = f.Close() }()
	trivy, err := toolPath("trivy")
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		ref := strings.TrimSpace(sc.Text())
		if ref == "" || strings.HasPrefix(ref, "#") {
			continue
		}
		n++
		fmt.Println("== scanning", ref)
		if err := execTool(ctx, trivy, "image", "--exit-code", "1", "--severity", *severity, "--ignore-unfixed", ref); err != nil {
			return fmt.Errorf("trivy %s: %w", ref, err)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no image refs in %s", *list)
	}
	fmt.Printf("scanned %d images: ok\n", n)
	return nil
}
