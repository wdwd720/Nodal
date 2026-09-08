// Command riskpolicy records the GLOBAL risk policy a deployment evaluates
// against.
//
//	go run ./scripts/riskpolicy -operator alice -reason "seeding development limits"
//	go run ./scripts/riskpolicy -operator alice -reason "risk desk RD-2026-14" \
//	  -rules ./deploy/risk/global-2026-14.json -version global-2026-14
//
// # Why this exists
//
// internal/risk has been complete and unreachable since it was written. Every
// limit, every kill switch, every hash check — and nothing in any deployment
// ever wrote a policy row, so risk_policies was empty, EffectivePolicy answered
// ErrNoPolicy, and the kernel's own doc comment described
// DefaultGlobalPolicyJSON as "the compiled-in GLOBAL policy used only to seed a
// fresh deployment" for a seeding step that did not exist.
//
// That is the F-26 shape a third time: a control that is correct, tested, and
// impossible to reach. Wiring the risk kernel into Domain A trades (see
// internal/nativemarket/risk.go) makes the absence load-bearing — with no
// policy row every native trade now fails closed — so the seeding step has to
// be real.
//
// # What it will and will not do
//
// With -rules it records the policy in that file, in any environment. That is
// an operator writing down limits somebody decided, which is what PART 58 asks
// for, and the -reason is where the sign-off reference goes.
//
// With no -rules it records risk.DefaultGlobalPolicyJSON, and REFUSES to do so
// outside LOCAL, DEV or TEST. Those numbers are starter values chosen to be
// small; seeding a production deployment with limits nobody signed off would
// produce exactly the thing this whole audit is against — a control that looks
// decided and is not. A production deployment with no policy refuses every
// trade until somebody records one, and that is the correct failure.
//
// It never replaces a policy: risk_policies is append-only and versions are
// unique, so a second run with the same version is refused by the database. A
// new version supersedes the old one by effective_at, and the old row stays
// readable forever, which is what makes a decision recomputable.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// localAppDSN is the LOCAL docker-compose credential. A non-local host is
// permitted here, unlike scripts/gateceremony: recording a policy an operator
// wrote is a legitimate production act. What is refused in production is the
// COMPILED-IN default, which is a different thing.
const localAppDSN = "postgres://cp_app:cp_app_local@127.0.0.1:5433/controlplane?sslmode=disable" // #nosec G101 -- the LOCAL docker-compose fallback DSN, used only when CP_DATABASE_APP_URL is unset; it is a development credential, not a secret

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "riskpolicy:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		operator = flag.String("operator", "", "identifier of the operator recording this policy (required)")
		reason   = flag.String("reason", "", "why, and against which sign-off (required; recorded forever)")
		rules    = flag.String("rules", "", "path to a policy JSON document; omitted means the compiled-in development default")
		version  = flag.String("version", "", "policy version; defaults to a timestamped development version")
		showOnly = flag.Bool("print", false, "print the policy that would be recorded and exit")
	)
	flag.Parse()

	env := strings.ToUpper(strings.TrimSpace(os.Getenv("CP_ENV")))
	if env == "" {
		env = "LOCAL"
	}

	body := []byte(risk.DefaultGlobalPolicyJSON)
	source := "the compiled-in development default"
	if *rules != "" {
		read, err := os.ReadFile(*rules) // #nosec G304 -- an operator names the policy file to record; that is the tool's whole purpose
		if err != nil {
			return fmt.Errorf("read %s: %w", *rules, err)
		}
		body, source = read, *rules
	} else if env != "LOCAL" && env != "DEV" && env != "TEST" {
		return fmt.Errorf("refusing to seed %s with the compiled-in default limits: "+
			"they are starter values nobody signed off, and a policy that LOOKS decided is worse "+
			"than none. Pass -rules with the limits your risk desk approved.\n"+
			"Until then this deployment refuses every trade the kernel gates, which is the correct failure", env)
	}

	// Parsed and validated before anything is opened, so a malformed document
	// fails without a database connection and without a partial write.
	parsed, err := risk.ParsePolicy(json.RawMessage(body))
	if err != nil {
		return fmt.Errorf("parse %s: %w", source, err)
	}
	if err := parsed.Validate(risk.ScopeGlobal); err != nil {
		return fmt.Errorf("validate %s: %w", source, err)
	}
	if missing := parsed.MissingLimits(); len(missing) > 0 {
		return fmt.Errorf("%s leaves %d limit(s) unset: %s\n"+
			"a GLOBAL policy must be complete; an absent limit is not a permissive one, it fails closed "+
			"and refuses every evaluation that reaches it", source, len(missing), strings.Join(missing, ", "))
	}

	clk := clock.System()
	now := clk.Now().UTC()
	if *version == "" {
		*version = "global-" + now.Format("20060102T150405Z")
	}
	if *showOnly {
		canon, cerr := parsed.CanonicalJSON()
		if cerr != nil {
			return cerr
		}
		fmt.Printf("%s\n", canon)
		return nil
	}
	if strings.TrimSpace(*operator) == "" || strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("-operator and -reason are required and have no defaults; " +
			"a policy row with a default reason is a limit nobody has to justify")
	}

	dsn := os.Getenv("CP_DATABASE_APP_URL")
	if dsn == "" {
		dsn = localAppDSN
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: dsn, MaxConns: 2, MinConns: 1, AppName: "riskpolicy"})
	if err != nil {
		return err
	}
	defer d.Close()

	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: *operator, ActorType: security.ActorOperator, AuthTime: now, AMR: []string{"mfa"},
	})
	store := risk.NewStore()

	// What is effective right now, before the write. A deployment that already
	// has a policy is told which one it is replacing rather than quietly
	// gaining a second one.
	if before, _, perr := store.EffectivePolicy(ctx, d, "", "", now); perr == nil {
		fmt.Printf("current GLOBAL policy: %s\n", before.Version)
	}

	var recorded risk.Policy
	if err := d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var rerr error
		recorded, rerr = store.RecordPolicy(ctx, tx, risk.PolicyRecord{
			Scope:       risk.ScopeGlobal,
			Version:     *version,
			Rules:       json.RawMessage(body),
			EffectiveAt: now,
			ActorType:   security.ActorOperator,
			ActorID:     *operator,
			Reason:      *reason + " [recorded by scripts/riskpolicy from " + source + " in " + env + "]",
		})
		return rerr
	}); err != nil {
		return err
	}

	fmt.Printf("recorded GLOBAL risk policy %s from %s\n", recorded.Version, source)
	fmt.Printf("  hash:                 %s\n", recorded.Hash())
	fmt.Printf("  native market concn:  %s\n", bps(recorded.MaxNativeMarketConcentrationBPS))
	fmt.Printf("  creator concn:        %s\n", bps(recorded.MaxCreatorConcentrationBPS))
	if *rules == "" {
		fmt.Println()
		fmt.Println("These are STARTER limits from the compiled-in default. They were chosen to")
		fmt.Println("be small, not to be right, and no risk desk has seen them. Replace them with")
		fmt.Println("-rules before this deployment carries anything real.")
	}
	return nil
}

func bps(v *money.BPS) string {
	if v == nil {
		return "unset"
	}
	return fmt.Sprintf("%d bps", *v)
}
