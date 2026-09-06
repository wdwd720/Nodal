// Command devrun starts the control plane's long-lived processes together on
// the LOCAL docker-compose stack and streams their logs, prefixed by service,
// until interrupted.
//
//	make dev            # infra-up + migrate, then this
//	go run ./scripts/devrun
//	go run ./scripts/devrun -only api,relay-worker
//	go run ./scripts/devrun -list
//
// It refuses to run outside LOCAL/DEV. Every process is started with the same
// environment this process holds, so the usual CP_* variables apply; the only
// thing devrun supplies is a default for the ones with no sensible zero value.
//
// Workers that have no provider binding refuse to start rather than half-wiring
// themselves — that is deliberate (a worker that appears to run and does
// nothing is worse than one that will not start), so seeing some of them exit
// with a message naming what is missing is the expected LOCAL experience, not a
// failure of this script. Their exit is reported and the rest keep running.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// service is one binary devrun can start. args are the subcommand: the runtime
// image entrypoint is the bare binary, and every worker needs its subcommand —
// only the api takes none.
type service struct {
	name string
	args []string
}

var services = []service{
	{name: "api"},
	{name: "relay-worker", args: []string{"run"}},
	{name: "execution-worker", args: []string{"run"}},
	{name: "reconciliation-worker", args: []string{"run"}},
	{name: "workflow-worker", args: []string{"run"}},
	{name: "agent-worker", args: []string{"dispatch"}},
	{name: "audit-worker", args: []string{"run"}},
	{name: "market-ingest-worker", args: []string{"run"}},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "devrun:", err)
		os.Exit(1)
	}
}

func run() error {
	only := flag.String("only", "", "comma-separated subset of services to run (default: all)")
	list := flag.Bool("list", false, "list the services devrun knows about and exit")
	shutdown := flag.Duration("shutdown", 20*time.Second, "how long to wait for graceful shutdown before killing")
	flag.Parse()

	if *list {
		for _, s := range services {
			fmt.Printf("%-24s %s\n", s.name, strings.Join(s.args, " "))
		}
		return nil
	}

	env := strings.ToUpper(strings.TrimSpace(os.Getenv("CP_ENV")))
	if env == "" {
		env = "LOCAL"
		_ = os.Setenv("CP_ENV", env)
		fmt.Fprintln(os.Stderr, "devrun: CP_ENV is not set; assuming LOCAL")
	}
	if env != "LOCAL" && env != "DEV" {
		return fmt.Errorf("refusing to run in environment %q: devrun starts every process on one host with shared local defaults, which is a development convenience and never a deployment", env)
	}

	selected, err := pick(*only)
	if err != nil {
		return err
	}

	// Build first, so a compile error surfaces once here rather than eight
	// times interleaved across the log stream.
	bin, err := build(context.Background(), selected)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	width := 0
	for _, s := range selected {
		if len(s.name) > width {
			width = len(s.name)
		}
	}

	for _, s := range selected {
		wg.Add(1)
		go func(s service) {
			defer wg.Done()
			supervise(ctx, s, bin[s.name], width)
		}(s)
	}

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "\ndevrun: interrupt received; draining")

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(*shutdown):
		fmt.Fprintf(os.Stderr, "devrun: some processes did not exit within %s\n", *shutdown)
	}
	return nil
}

func pick(only string) ([]service, error) {
	if strings.TrimSpace(only) == "" {
		return services, nil
	}
	known := map[string]service{}
	for _, s := range services {
		known[s.name] = s
	}
	var out []service
	for _, name := range strings.Split(only, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		s, ok := known[name]
		if !ok {
			names := make([]string, 0, len(known))
			for n := range known {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("unknown service %q; known: %s", name, strings.Join(names, ", "))
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("-only selected no services")
	}
	return out, nil
}

// build compiles each selected binary once and returns its path. Building up
// front keeps `go run`'s compile output from interleaving with runtime logs.
func build(ctx context.Context, selected []service) (map[string]string, error) {
	dir, err := os.MkdirTemp("", "devrun-")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(selected))
	for _, s := range selected {
		path := dir + string(os.PathSeparator) + s.name
		if os.PathSeparator == '\\' {
			path += ".exe"
		}
		fmt.Fprintf(os.Stderr, "devrun: building %s\n", s.name)
		cmd := exec.CommandContext(ctx, "go", "build", "-o", path, "./cmd/"+s.name)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("build %s: %w", s.name, err)
		}
		out[s.name] = path
	}
	return out, nil
}

// supervise runs one process to completion, prefixing every line it writes.
// It does not restart: a worker that refuses to start is telling you something,
// and a restart loop would bury the message it is trying to deliver.
func supervise(ctx context.Context, s service, bin string, width int) {
	// Deliberately NOT CommandContext: cancellation here must ask the child to
	// drain (SIGINT below) and wait for it, not have os/exec kill it the moment
	// the context ends. A worker killed mid-transaction is exactly what every
	// graceful-shutdown path in this repo exists to avoid.
	cmd := exec.Command(bin, s.args...) //nolint:noctx // see above: shutdown is signaled, not context-killed
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "devrun: %s: %v\n", s.name, err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "devrun: %s: %v\n", s.name, err)
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "devrun: %s: start: %v\n", s.name, err)
		return
	}

	var streams sync.WaitGroup
	streams.Add(2)
	go func() { defer streams.Done(); prefix(s.name, width, stdout) }()
	go func() { defer streams.Done(); prefix(s.name, width, stderr) }()

	exited := make(chan error, 1)
	go func() { streams.Wait(); exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		report(s.name, err)
	case <-ctx.Done():
		// Ask politely first. On Windows Interrupt is not deliverable to
		// another process, so Kill is the only option there; the graceful
		// paths are covered by each worker's own tests.
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case err := <-exited:
			report(s.name, err)
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			fmt.Fprintf(os.Stderr, "devrun: %s did not drain in time and was killed\n", s.name)
		}
	}
}

func report(name string, err error) {
	if err == nil {
		fmt.Fprintf(os.Stderr, "devrun: %s exited cleanly\n", name)
		return
	}
	// Not necessarily wrong: a worker with no provider binding refuses to start
	// on purpose, and its own message says what is missing.
	fmt.Fprintf(os.Stderr, "devrun: %s exited: %v\n", name, err)
}

func prefix(name string, width int, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fmt.Printf("%-*s | %s\n", width, name, sc.Text())
	}
}
