// Command fmtcheck fails when any Go file under the given directories is not
// gofumpt-formatted. gofumpt -l exits 0 even when it lists files, so make
// targets and CI use this wrapper to get a real failure.
//
// Arguments are directory paths supplied by the developer or CI on the command
// line; they are validated to be existing directories before use.
package main

import (
	"bytes"
	"context"
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
		fmt.Fprintln(os.Stderr, "usage: fmtcheck <dir>...")
		return 2
	}
	var dirs []string
	for _, d := range args {
		clean := filepath.Clean(d)
		if st, err := os.Stat(clean); err == nil && st.IsDir() { // #nosec G703 -- dev tool: the directory list is this program's own command-line arguments
			dirs = append(dirs, clean)
		}
	}
	if len(dirs) == 0 {
		fmt.Println("fmtcheck: no directories to check")
		return 0
	}
	bin := filepath.Join("bin", "gofumpt")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if _, err := os.Stat(bin); err != nil {
		p, lerr := exec.LookPath("gofumpt")
		if lerr != nil {
			fmt.Fprintln(os.Stderr, "fmtcheck: gofumpt not found in ./bin or PATH; run `make tools`")
			return 1
		}
		bin = p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// -extra, to match .golangci.yml's `gofumpt.extra.group-params: true`.
	// Without it the two disagree: this target passed while `make lint`
	// reported the same two files as unformatted, so `make fmt` could produce
	// a file `make lint` rejects. Two formatters with different settings is
	// worse than one.
	cmd := exec.CommandContext(ctx, bin, append([]string{"-l", "-extra"}, dirs...)...) // #nosec G204 G702 -- dev tool: bin is ./bin/gofumpt or a PATH lookup of the literal name; dirs are CLI arguments filtered to existing directories, and exec.Command never invokes a shell
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "fmtcheck: gofumpt failed: %v\n%s", err, errb.String())
		return 1
	}
	files := strings.TrimSpace(out.String())
	if files != "" {
		fmt.Fprintln(os.Stderr, "fmtcheck: files are not gofumpt-formatted:")
		fmt.Fprintln(os.Stderr, files)
		return 1
	}
	fmt.Println("fmtcheck: ok")
	return 0
}
