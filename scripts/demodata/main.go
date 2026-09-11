// Command demodata loads the sandbox demo catalogue for the internal economy.
//
//	go run ./scripts/demodata
//	go run ./scripts/demodata -print
//
// # Why it exists beside the boot-time seeder
//
// cmd/api seeds the same catalogue at boot on a sandbox tier, which is what a
// STAGING deployment needs: the markets page has markets the first time anybody
// opens it. This is the same seeder as a command, for the two cases the boot
// path cannot serve — a developer who wants the data without restarting the API,
// and an operator who wants to see the failure rather than a warning in a log
// stream.
//
// # What it refuses
//
// PROD, three times over: here, in demo.NewSeeder, and in migration 00774's
// CHECK, which refuses a demo row whose environment is PROD. And any deployment
// that is not a sandbox tier (ADR-0023), because a deployment where value can
// move has no place to put simulated data.
//
// It activates no capability gate, records no risk policy and moves no legal
// policy. Every object it creates goes through the same domain service a
// person's request goes through — content screening, the moderation verdict, the
// single mint, the ledger posting, the risk kernel, the market-safety policy and
// the constant-product trigger. Where a gate is missing its trades fail with the
// capability's own refusal, which is the correct outcome and not something a
// seeder may route around.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capresolver"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/demo"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuation"
)

// localAppDSN is the LOCAL docker-compose credential, used only when
// CP_DATABASE_APP_URL is unset.
const localAppDSN = "postgres://cp_app:cp_app_local@127.0.0.1:5433/controlplane?sslmode=disable" // #nosec G101 -- the LOCAL docker-compose development credential, not a secret

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "demodata:", err)
		os.Exit(1)
	}
}

func run() error {
	showOnly := flag.Bool("print", false, "print what would be seeded and exit")
	flag.Parse()

	env := strings.ToUpper(strings.TrimSpace(os.Getenv("CP_ENV")))
	if env == "" {
		env = "LOCAL"
	}
	// The sandbox tier is one declaration (ADR-0023), and config.Validate
	// refuses it in PROD. This reads the same variable rather than inventing a
	// second way to be a sandbox.
	sandbox := strings.EqualFold(strings.TrimSpace(os.Getenv("CP_API_LEGAL_POLICY")), "SANDBOX")
	if env == "LOCAL" || env == "DEV" || env == "TEST" {
		// A development database is a sandbox by construction: no provider
		// outside PROD may be live, so nothing there can move real value.
		sandbox = true
	}

	if *showOnly {
		fmt.Printf("environment:  %s\n", env)
		fmt.Printf("sandbox tier: %t\n", sandbox)
		fmt.Printf("catalogue:    %s\n", strings.Join(demo.Specs(), ", "))
		fmt.Printf("seed epoch:   %s\n", demo.SeedEpoch.Format(time.RFC3339))
		return nil
	}

	dsn := os.Getenv("CP_DATABASE_APP_URL")
	if dsn == "" {
		dsn = localAppDSN
	}
	if u, err := url.Parse(dsn); err == nil && env == "LOCAL" {
		if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" {
			return fmt.Errorf("CP_ENV is LOCAL but the database host is %q; refusing to seed it", h)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: dsn, MaxConns: 4, MinConns: 1, AppName: "demodata"})
	if err != nil {
		return err
	}
	defer d.Close()

	clk := clock.System()
	led := ledger.NewService(clk, "demodata")
	// The same answer to "which capabilities are ACTIVE" the API gives itself:
	// the gate rows, read as a sandbox tier reads them when this is one, and
	// only for the capabilities the deployment declares enabled. Without it the
	// ledger fails closed on every cross-domain posting, so every demo trade
	// was refused on a database whose gates were sandbox-active (F-223). A gate
	// that really is missing still refuses, with the capability's own words.
	checker, err := gates.NewChecker(env, enabledCapabilities(), clk)
	if err != nil {
		return err
	}
	led.SetCapabilityResolver(capresolver.New(checker.WithSandbox(sandbox), d))
	credits := credit.NewService(led, clk)
	markets := nativemarket.NewService(led, credits, valuation.NewPriceStore(clk), audit.NewWriter(),
		instruments.NewRepository(), nativemarket.NewRiskGate(risk.NewStore(), clk), clk)
	markets.SetSafety(nativemarket.NewSafetyStore())

	creditAsset, err := credits.AssetID(ctx, d)
	if err != nil {
		return fmt.Errorf("%w\nrun `go run ./scripts/seedeconomy` first: a market priced in nothing is not a market", err)
	}

	seeder, err := demo.NewSeeder(demo.Deps{
		DB: d, Assets: nativeasset.NewService(clk, nil), Markets: markets, Credits: credits,
		Clock: clk, CreditAssetID: creditAsset, Environment: env, SandboxTier: sandbox,
	})
	if err != nil {
		return err
	}

	res, err := seeder.Seed(ctx)
	if err != nil {
		return fmt.Errorf("%w\n%d object(s) were created before this failed; run again to continue", err, len(res.Created))
	}

	fmt.Printf("demodata: %d created, %d already present, %d demo markets in %s\n",
		len(res.Created), len(res.Existing), len(res.Markets), env)
	for _, key := range res.Created {
		fmt.Println("  + " + key)
	}
	if len(res.Created) == 0 {
		fmt.Println("  (nothing to do: every object this seeder makes was already registered)")
	}
	fmt.Println()
	fmt.Println("Every one of these is SIMULATED. The assets are named DEMO-, their descriptions")
	fmt.Println("say so, their Credits are PROMOTIONAL and no payout policy in this build lets a")
	fmt.Println("promotional Credit leave. They exist only on a sandbox tier and nowhere else.")
	return nil
}

// enabledCapabilities reads CP_API_ENABLED_CAPABILITIES the way cmd/api does:
// condition 1 of POLICY_AUTHORITY §1, the deployment's own declaration. Unset
// means nothing is enabled, which fails closed exactly as the API would.
func enabledCapabilities() func(gates.Capability) bool {
	enabled := map[gates.Capability]bool{}
	for _, raw := range strings.Split(os.Getenv("CP_API_ENABLED_CAPABILITIES"), ",") {
		name := gates.Capability(strings.ToUpper(strings.TrimSpace(raw)))
		if name != "" && name.Valid() {
			enabled[name] = true
		}
	}
	return func(c gates.Capability) bool { return enabled[c] }
}
