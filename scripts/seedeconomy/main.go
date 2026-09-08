// Command seedeconomy loads clearly-labelled LOCAL development data for the
// Nodal-native economy: the Credit asset, Credits for the dev customers, a
// registered seller and a small catalogue of products.
//
//	go run ./scripts/seedeconomy
//	go run ./scripts/seedeconomy -force
//
// It exists because Domain A is otherwise unreachable in a development
// environment. Without a Credit asset nobody has Credits; without Credits
// nobody can buy anything; and without something to buy, the marketplace, the
// payout page and the load scripts all measure an empty catalogue and report
// success having exercised nothing.
//
// # What it deliberately does NOT do
//
// It does not activate any capability gate. MARKETPLACE and
// NATIVE_MARKET_TRADING are high risk (internal/gates.IsHighRisk), which means
// activating one requires three distinct principals, a step-up, and four
// evidence references. A seeder that wrote an ACTIVE gate row would be
// bypassing the control; one that drove the flow with invented legal, provider,
// risk and security references would be filling a control with fiction, which
// is worse — it produces a gate that LOOKS approved. So it seeds data, prints
// the exact steps, and leaves the decision to a person.
//
// It also does not set a legal policy. That is CP_API_LEGAL_POLICY=DEVELOPMENT
// on the API, which is refused outside LOCAL, DEV and TEST.
//
// The same guards as scripts/seed apply: LOCAL, DEV or TEST only, and a local
// database host only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

const (
	devIssuer = "devidp"
	// localAppDSN is the LOCAL docker-compose credential. seedeconomy is a
	// local-only developer script and refuses a non-local host below.
	localAppDSN = "postgres://cp_app:cp_app_local@127.0.0.1:5433/controlplane?sslmode=disable" // #nosec G101 -- the LOCAL docker-compose credential, not a secret; run() refuses any non-local database host before this is used
	// seedEpoch is fixed, not the wall clock: internal/ledger includes
	// effective_at in the posting content hash, so a moving timestamp under a
	// fixed idempotency key is refused as INVALID_IDEMPOTENCY_REUSE on the
	// second run. The same seed should describe the same financial fact.
	seedEpochRFC = "2026-01-01T00:00:00Z"
	// Credits are held at six decimal places, so this is 25,000 Credits each.
	creditsPerCustomer = "25000000000"
)

// seedProduct is one catalogue entry. The kinds are chosen to span the three
// earning provenances, so a developer can see DATA_SALE_EARNING,
// AGENT_SERVICE_EARNING and CREATOR_EARNING actually arise.
type seedProduct struct {
	kind   commerce.Kind
	title  string
	body   string
	price  string
	feeBPS money.BPS
}

var catalogue = []seedProduct{
	{commerce.KindData, "Order-book snapshots, 2026 (LOCAL FAKE)",
		"A year of level-2 snapshots. Development data; the file does not exist.", "1500000000", 1_000},
	{commerce.KindAgentService, "Agent run: portfolio review (LOCAL FAKE)",
		"An agent reviews a portfolio and writes a note. Development data.", "800000000", 500},
	{commerce.KindResearch, "Weekly research note (LOCAL FAKE)",
		"Written analysis. Development data, and not advice.", "300000000", 0},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seedeconomy:", err)
		os.Exit(1)
	}
}

func run() error {
	force := flag.Bool("force", false, "seed even if internal-economy data already exists")
	flag.Parse()

	env := strings.ToUpper(strings.TrimSpace(os.Getenv("CP_ENV")))
	if env == "" {
		env = "LOCAL"
	}
	switch env {
	case "LOCAL", "DEV", "TEST":
	default:
		return fmt.Errorf("refusing to seed in environment %q: seed data is LOCAL/DEV/TEST only (PART 146)", env)
	}

	dsn := os.Getenv("CP_DATABASE_APP_URL")
	if dsn == "" {
		dsn = localAppDSN
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return err
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" {
		return fmt.Errorf("refusing to seed a non-local database host %q", h)
	}

	epoch, err := time.Parse(time.RFC3339, seedEpochRFC)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: "seedeconomy", ActorType: security.ActorSystem, AuthTime: time.Now(),
	})

	d, err := db.Open(ctx, db.Config{URL: dsn, MaxConns: 4, MinConns: 1, AppName: "seedeconomy"})
	if err != nil {
		return err
	}
	defer d.Close()

	acct := accounts.NewRepository()
	buyer, err := devAccount(ctx, d, acct, "dev:customer-a")
	if err != nil {
		return fmt.Errorf("customer-a: %w (run `go run ./scripts/seed` first)", err)
	}
	seller, err := devAccount(ctx, d, acct, "dev:customer-b")
	if err != nil {
		return fmt.Errorf("customer-b: %w (run `go run ./scripts/seed` first)", err)
	}

	var already int
	if err := d.QueryRow(ctx, `SELECT count(*) FROM internal_products`).Scan(&already); err != nil {
		return err
	}
	if already > 0 && !*force {
		fmt.Println("seedeconomy: internal-economy data already present (use -force to add anyway)")
		return nil
	}

	clk := clock.System()
	led := ledger.NewService(clk, "seedeconomy")
	credits := credit.NewService(led, clk)
	com := commerce.NewService(led, credits, audit.NewWriter(), clk)
	// The seeder writes the CATALOGUE, never a sale, so it needs no capability
	// resolver: publishing a product moves nothing. A purchase would, and the
	// marketplace gate would refuse it — correctly, and the printed steps below
	// say how to turn it on.
	var summary []string

	creditAsset, err := ensureCreditAsset(ctx, d)
	if err != nil {
		return err
	}
	summary = append(summary, "asset: CREDIT (internal, 6 decimals)")

	amount, err := money.ParseQuantity(creditsPerCustomer)
	if err != nil {
		return err
	}

	err = d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		for name, account := range map[string]accounts.AccountID{"customer-a": buyer, "customer-b": seller} {
			if _, err := credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: account, Quantity: amount,
				// PROMOTIONAL and UNFUNDED, deliberately: nobody paid for these
				// and the provenance says so. A seeder that minted PURCHASED
				// Credits would be inventing a funding event, and PURCHASED is
				// the origin a payout policy is most likely to permit.
				Origin: valuedomain.OriginPromotional, Finality: valuedomain.FinalityUnfunded,
				Reference:      credit.Reference{Type: "seed", ID: name},
				IdempotencyKey: "seedeconomy:credits:" + name,
				Reason:         "LOCAL SEED — fake Credits, never real value",
				EffectiveAt:    epoch,
			}); err != nil && errs.CodeOf(err) != errs.CodeConflict {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	summary = append(summary, "credits: 25,000 PROMOTIONAL/UNFUNDED to customer-a and customer-b")

	err = d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := com.RegisterSeller(ctx, tx, commerce.Seller{
			AccountID: seller, DisplayName: "dev:customer-b (LOCAL FAKE seller)",
		}); err != nil {
			return err
		}
		for _, sp := range catalogue {
			price, perr := money.ParseQuantity(sp.price)
			if perr != nil {
				return perr
			}
			created, cerr := com.CreateProduct(ctx, tx, commerce.Product{
				SellerAccountID: seller, Kind: sp.kind,
				Title: sp.title, Description: sp.body,
				Price: price, PlatformFeeBPS: sp.feeBPS,
			})
			if cerr != nil {
				return cerr
			}
			if _, perr := com.Publish(ctx, tx, created.ID); perr != nil {
				return perr
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	summary = append(summary, fmt.Sprintf("seller: dev:customer-b with %d published products", len(catalogue)))
	summary = append(summary, "credit asset id: "+creditAsset.String())

	fmt.Println("seedeconomy: LOCAL FAKE DATA loaded (environment " + env + ")")
	for _, s := range summary {
		fmt.Println("  - " + s)
	}
	fmt.Println()
	fmt.Println("Nothing can be BOUGHT yet, and that is correct. THREE things stand between this")
	fmt.Println("data and a working marketplace, and none of them is a seeder's to decide.")
	fmt.Println("They were two until somebody actually tried it and found the third.")
	fmt.Println()
	fmt.Println("  1. The legal policy. Start the API with CP_API_LEGAL_POLICY=DEVELOPMENT, which")
	fmt.Println("     permits the internal economy and is refused in STAGING and PROD.")
	fmt.Println()
	fmt.Println("  2. The MARKETPLACE capability gate. It is HIGH RISK — exercising it mints the")
	fmt.Println("     earning provenance a payout policy may one day release — so activating it")
	fmt.Println("     needs three distinct principals (propose, approve, activate), a recent")
	fmt.Println("     step-up, and all four evidence references. Drive it through the admin")
	fmt.Println("     console, or through scripts/gateceremony, which performs the SAME ceremony")
	fmt.Println("     against gates.Admin and refuses anything but LOCAL/DEV/TEST. Do not write")
	fmt.Println("     the row: a gate that was not approved is not a gate.")
	fmt.Println()
	fmt.Println("  3. CP_API_ENABLED_CAPABILITIES must name MARKETPLACE. Configuration is checked")
	fmt.Println("     BEFORE the gate row, so that no database read can ever be what enables a")
	fmt.Println("     capability. An operator who does step 2 and not this one gets a gate that")
	fmt.Println("     is ACTIVE and a marketplace that still refuses everything.")
	return nil
}

// devAccount resolves one of the dev identities scripts/seed creates.
func devAccount(ctx context.Context, d *db.DB, repo *accounts.Repository, subject string) (accounts.AccountID, error) {
	u, err := repo.GetUserBySubject(ctx, d.Pool(), devIssuer, subject)
	if err != nil {
		return accounts.AccountID{}, err
	}
	owned, err := repo.ListByOwner(ctx, d.Pool(), u.ID)
	if err != nil {
		return accounts.AccountID{}, err
	}
	if len(owned) == 0 {
		return accounts.AccountID{}, errors.New("has no account")
	}
	return owned[0].ID, nil
}

// ensureCreditAsset returns THE Credit asset, creating it if absent. Migration
// 00711 permits exactly one per deployment, so this is a lookup first.
func ensureCreditAsset(ctx context.Context, d *db.DB) (assets.AssetID, error) {
	var existing assets.AssetID
	if err := d.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing); err == nil {
		return existing, nil
	}
	created, err := assets.NewRepository().Create(ctx, d, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindCredit,
		ValueDomain: valuedomain.InternalCredit,
		Symbol:      "CREDIT", Name: "Nodal Credit",
		// Six decimals: a bonding-curve market has to price units far below
		// one Credit, and a coarser scale would make rounding to zero a usable
		// exploit.
		Decimals:  6,
		RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
	})
	if err != nil {
		return assets.AssetID{}, err
	}
	return created.ID, nil
}
