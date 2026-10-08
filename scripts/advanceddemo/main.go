// Command advanceddemo runs the end-to-end demonstrations of the advanced
// financial capabilities (advanced goal §34; ADR-0020) against a freshly
// provisioned, fully migrated LOCAL database, through the real services, and
// prints where the evidence of every step is.
//
//	go run ./scripts/advanceddemo [-keep] [-out dist/advanced-demo.json] [-only A,B]
//	make advanced-demo
//
// Every scenario is deterministic: a fake clock starts at a fixed instant,
// every identifier the scenario chooses is derived from its name, and the
// checks compare exact integers. A scenario that cannot prove what it claims
// fails the run (exit status 1); nothing is printed as a success that a check
// did not establish.
//
// It refuses to run against anything but a local database (the admin DSN
// must point at 127.0.0.1/localhost; see internal/testkit/localdb), and every
// value it moves is SIMULATED.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/testkit/localdb"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

// demoStart is the instant every run's clock starts at: a Wednesday, so an
// equities session is open, and fixed, so every run is identical.
var demoStart = time.Date(2026, 10, 7, 14, 30, 0, 0, time.UTC)

// Step is one observable step of a scenario and where its evidence lives.
type Step struct {
	Description string            `json:"description"`
	Evidence    map[string]string `json:"evidence,omitempty"`
}

// Check is one assertion a scenario makes about the books.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Result is a scenario's outcome.
type Result struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Steps    []Step        `json:"steps"`
	Checks   []Check       `json:"checks"`
	Duration time.Duration `json:"duration_ns"`
	Err      string        `json:"error,omitempty"`
}

// OK reports whether the scenario ran and every check held.
func (r Result) OK() bool {
	if r.Err != "" || len(r.Checks) == 0 {
		return false
	}
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// recorder collects a scenario's steps and checks.
type recorder struct {
	res *Result
}

func (r *recorder) step(desc string, kv ...string) {
	s := Step{Description: desc}
	if len(kv) > 0 {
		s.Evidence = map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			s.Evidence[kv[i]] = kv[i+1]
		}
	}
	r.res.Steps = append(r.res.Steps, s)
}

func (r *recorder) check(name string, ok bool, format string, a ...any) {
	r.res.Checks = append(r.res.Checks, Check{Name: name, OK: ok, Detail: fmt.Sprintf(format, a...)})
}

// scenario is one runnable demonstration.
type scenario struct {
	id    string
	title string
	run   func(ctx context.Context, w *sandboxworld.World, rec *recorder) error
}

// scenarios is the registry, in the order of advanced goal §34.
var scenarios []scenario

func register(id, title string, run func(ctx context.Context, w *sandboxworld.World, rec *recorder) error) {
	scenarios = append(scenarios, scenario{id: id, title: title, run: run})
}

// Report is what the run writes.
type Report struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Database   string    `json:"database"`
	Results    []Result  `json:"results"`
	OK         bool      `json:"ok"`
}

func main() { os.Exit(run()) }

func run() int {
	var (
		admin = flag.String("admin", envOr("CP_TEST_ADMIN_DATABASE_URL", localdb.DefaultAdminDSN), "admin DSN (LOCAL only)")
		keep  = flag.Bool("keep", false, "keep the demo database after the run")
		out   = flag.String("out", filepath.Join("dist", "advanced-demo.json"), "report path")
		only  = flag.String("only", "", "comma-separated scenario ids to run (default: all)")
	)
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	rep := Report{StartedAt: time.Now().UTC()}
	dbase, err := localdb.Provision(ctx, "advanced_demo", localdb.Options{AdminDSN: *admin, Migrate: true})
	if err != nil {
		fmt.Fprintln(os.Stderr, "advanceddemo: provision:", err)
		return 1
	}
	rep.Database = dbase.Name
	if !*keep {
		defer func() { _ = localdb.Drop(context.Background(), "advanced_demo", *admin) }()
	}
	w, err := sandboxworld.Open(ctx, sandboxworld.Options{AppDSN: dbase.AppDSN, Start: demoStart, MaxConns: 16, Component: "advanced-demo"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "advanceddemo: environment:", err)
		return 1
	}
	defer w.Close()

	wanted := map[string]bool{}
	for _, id := range strings.Split(*only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			wanted[strings.ToUpper(id)] = true
		}
	}
	sort.SliceStable(scenarios, func(i, j int) bool { return scenarios[i].id < scenarios[j].id })
	allOK := len(scenarios) > 0
	for _, s := range scenarios {
		if len(wanted) > 0 && !wanted[s.id] {
			continue
		}
		res := Result{ID: s.id, Title: s.title}
		start := time.Now()
		if err := s.run(ctx, w, &recorder{res: &res}); err != nil {
			res.Err = err.Error()
		}
		res.Duration = time.Since(start)
		rep.Results = append(rep.Results, res)
		printResult(res)
		if !res.OK() {
			allOK = false
		}
	}
	rep.OK = allOK
	rep.FinishedAt = time.Now().UTC()
	if err := writeReport(*out, rep); err != nil {
		fmt.Fprintln(os.Stderr, "advanceddemo: report:", err)
		return 1
	}
	fmt.Printf("\nadvanceddemo: database %s, report %s\n", rep.Database, *out)
	if !allOK {
		fmt.Println("advanceddemo: FAILED")
		return 1
	}
	fmt.Println("advanceddemo: every scenario ran and every check held")
	return 0
}

func printResult(r Result) {
	status := "OK"
	if !r.OK() {
		status = "FAILED"
	}
	fmt.Printf("\n=== Scenario %s — %s [%s, %s]\n", r.ID, r.Title, status, r.Duration.Round(time.Millisecond))
	for _, s := range r.Steps {
		fmt.Printf("  · %s\n", s.Description)
		keys := make([]string, 0, len(s.Evidence))
		for k := range s.Evidence {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("      %s: %s\n", k, s.Evidence[k])
		}
	}
	for _, c := range r.Checks {
		mark := "✓"
		if !c.OK {
			mark = "✗"
		}
		fmt.Printf("  %s %s — %s\n", mark, c.Name, c.Detail)
	}
	if r.Err != "" {
		fmt.Printf("  error: %s\n", r.Err)
	}
}

func writeReport(path string, rep Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}
