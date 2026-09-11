// Command marketsafety records the internal market's safety policy.
//
//	go run ./scripts/marketsafety -print
//	go run ./scripts/marketsafety -operator alice -reason "risk desk RD-2026-21" \
//	  -rules ./deploy/marketsafety/2026-21.json -version market-safety-2026-21
//
// # Why it exists
//
// internal/nativemarket enforces four limits goal §47 names — a per-order price
// impact and slippage ceiling, a floor under the liquidity a market may open
// with, a per-market circuit breaker, and whether a creator may buy their own
// asset — and it reads them from a versioned document (migration 00773). A
// deployment that has recorded none runs the compiled-in conservative policy,
// which is a real policy rather than an absence.
//
// Two of those defaults are deliberately not the strict ones and both need an
// operator to change them, so this is the command that lets one:
//
//   - the CIRCUIT BREAKER is disarmed. A trip moves a market to CLOSE_ONLY and
//     only a person moves it back, and a newly launched constant-product market
//     legitimately moves several hundred percent in minutes, so a threshold low
//     enough to catch manipulation catches every launch. A deployment with
//     somebody watching its markets arms it here.
//   - a CREATOR MAY BUY THEIR OWN ASSET. internal/nativemarket's surveillance
//     reports creator self-dealing and does not block it, because there is no
//     order book and therefore no matched self-trade to prevent. A deployment
//     that would rather prevent than expose records `false` here.
//
// Without `-rules` it prints the compiled-in policy and records nothing: there
// is no "seed the default" path, because the default is already in force.
//
// It never replaces a policy. Versions are unique and the rows are immutable, so
// a second run with the same version is refused by the database and the document
// that was in force when a trade was refused stays readable forever.
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
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/security"
)

// localAppDSN is the LOCAL docker-compose credential, used only when
// CP_DATABASE_APP_URL is unset. Recording a policy an operator wrote is a
// legitimate production act, so a non-local host is permitted.
const localAppDSN = "postgres://cp_app:cp_app_local@127.0.0.1:5433/controlplane?sslmode=disable" // #nosec G101 -- the LOCAL docker-compose development credential, not a secret

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "marketsafety:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		operator = flag.String("operator", "", "identifier of the operator recording this policy (required)")
		reason   = flag.String("reason", "", "why, and against which sign-off (required; recorded forever)")
		rules    = flag.String("rules", "", "path to a policy JSON document")
		version  = flag.String("version", "", "policy version; required with -rules")
		showOnly = flag.Bool("print", false, "print the policy in force and exit")
	)
	flag.Parse()

	if *rules == "" || *showOnly {
		canon, err := nativemarket.ConservativeSafetyPolicy().CanonicalJSON()
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", canon)
		if !*showOnly {
			fmt.Fprintln(os.Stderr,
				"\nthis is the COMPILED-IN policy and it is already in force; pass -rules to record another")
		}
		return nil
	}

	body, err := os.ReadFile(*rules) // #nosec G304 -- an operator names the policy file to record; that is the tool's purpose
	if err != nil {
		return fmt.Errorf("read %s: %w", *rules, err)
	}
	if _, perr := nativemarket.ParseSafetyPolicy(json.RawMessage(body)); perr != nil {
		// Parsed and validated before anything is opened, so a malformed
		// document fails without a database connection and without a partial
		// write.
		return fmt.Errorf("parse %s: %w", *rules, perr)
	}
	switch {
	case strings.TrimSpace(*operator) == "", strings.TrimSpace(*reason) == "":
		return fmt.Errorf("-operator and -reason are required and have no defaults; " +
			"a limit nobody has to justify is a limit nobody will")
	case strings.TrimSpace(*version) == "":
		return fmt.Errorf("-version is required with -rules; a policy with no version cannot be cited " +
			"by a decision that was made under it")
	}

	dsn := os.Getenv("CP_DATABASE_APP_URL")
	if dsn == "" {
		dsn = localAppDSN
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: dsn, MaxConns: 2, MinConns: 1, AppName: "marketsafety"})
	if err != nil {
		return err
	}
	defer d.Close()

	now := clock.System().Now().UTC()
	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: *operator, ActorType: security.ActorOperator, AuthTime: now, AMR: []string{"mfa"},
	})

	if before, berr := nativemarket.NewSafetyStore().SafetyPolicy(ctx, d, now); berr == nil {
		fmt.Printf("current policy: %s\n", before.Version)
	}

	var recorded nativemarket.SafetyPolicy
	if err := d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var rerr error
		recorded, rerr = nativemarket.RecordSafetyPolicy(ctx, tx, nativemarket.SafetyPolicyRecord{
			Version: *version, Rules: json.RawMessage(body), EffectiveAt: now,
			ActorType: security.ActorOperator, ActorID: *operator,
			Reason: *reason + " [recorded by scripts/marketsafety from " + *rules + "]",
		})
		return rerr
	}); err != nil {
		return err
	}

	fmt.Printf("recorded market safety policy %s\n", recorded.Version)
	fmt.Printf("  hash:            %s\n", recorded.Hash())
	fmt.Printf("  price impact:    %s\n", bps(recorded.MaxPriceImpactBPS))
	fmt.Printf("  slippage:        %s\n", bps(recorded.MaxSlippageBPS))
	fmt.Printf("  circuit breaker: %s over %s\n", bps(recorded.CircuitBreakerMoveBPS), seconds(recorded.CircuitBreakerWindowSeconds))
	fmt.Printf("  opening floor:   %s Credit base units\n", quantity(recorded))
	fmt.Printf("  creator may buy: %s\n", boolean(recorded.CreatorMayBuyOwnAsset))
	if !recorded.BreakerEnabled() {
		fmt.Println()
		fmt.Println("The circuit breaker is DISARMED in this policy. Markets will not pause")
		fmt.Println("automatically however far they move.")
	}
	return nil
}

func bps(v *money.BPS) string {
	if v == nil {
		return "unset"
	}
	return fmt.Sprintf("%d bps", int(*v))
}

func seconds(v *int) string {
	if v == nil {
		return "an unset window"
	}
	return fmt.Sprintf("%ds", *v)
}

func boolean(v *bool) string {
	if v == nil {
		return "unset"
	}
	if *v {
		return "yes (surveillance reports it)"
	}
	return "no (refused with ASSET_RESTRICTED)"
}

func quantity(p nativemarket.SafetyPolicy) string {
	if p.MinOpeningLiquidityCredits == nil {
		return "unset"
	}
	return p.MinOpeningLiquidityCredits.String()
}
