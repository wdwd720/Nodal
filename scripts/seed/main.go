// Command seed loads clearly labeled LOCAL development data (PART 146): dev
// identities with accounts, a devnet USDC/SOL asset pair with a Jupiter
// listing, conservative asset policies, one price observation, and a SEED
// ledger posting of fake USDC for the first customer.
//
// It refuses to run unless CP_ENV is LOCAL, DEV or TEST (unset means LOCAL)
// and the database host is local. Production refuses seed mode twice: here,
// and in config.Validate (Seed.Enabled must be false outside LOCAL/TEST/DEV).
//
//	go run ./scripts/seed [-force]
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
)

const (
	localAppDSN = "postgres://cp_app:cp_app_local@127.0.0.1:5433/controlplane?sslmode=disable" // #nosec G101 -- LOCAL docker-compose credential; seed is a local-only developer script
	devIssuer   = "devidp"
	chain       = "solana-devnet"
	usdcDevMint = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU" // official devnet USDC mint
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		if e, ok := errs.As(err); ok {
			for k, v := range e.Fields {
				fmt.Fprintf(os.Stderr, "seed:   %s = %v\n", k, v)
			}
		}
		os.Exit(1)
	}
}

func run() error {
	force := flag.Bool("force", false, "seed even if dev data already exists")
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctx = security.WithPrincipal(ctx, security.Principal{SubjectID: "seed", ActorType: security.ActorSystem, AuthTime: time.Now()})
	d, err := db.Open(ctx, db.Config{URL: dsn, MaxConns: 4, MinConns: 1, AppName: "seed"})
	if err != nil {
		return err
	}
	defer d.Close()

	acct := accounts.NewRepository()
	if _, err := acct.GetUserBySubject(ctx, d.Pool(), devIssuer, "dev:customer-a"); err == nil && !*force {
		fmt.Println("seed: dev data already present (use -force to add anyway)")
		return nil
	}

	clk := clock.System()
	now := clk.Now()
	assetRepo := assets.NewRepository()
	instRepo := instruments.NewRepository()
	policies := valuation.NewPolicyStore()
	prices := valuation.NewPriceStore(clk)
	ledgerSvc := ledger.NewService(clk, "seed")
	ledgerSvc.AllowSeedPostings()

	var summary []string
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// Assets (idempotent on chain+mint).
		usdc, err := ensureAsset(ctx, tx, assetRepo, assets.Asset{Chain: chain, MintAddress: usdcDevMint, Kind: assets.KindSPLToken, Symbol: "USDC", Name: "USD Coin (devnet)", Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive})
		if err != nil {
			return err
		}
		sol, err := ensureAsset(ctx, tx, assetRepo, assets.Asset{Chain: chain, MintAddress: assets.NativeMintSentinel, Kind: assets.KindNative, Symbol: "SOL", Name: "Solana (devnet)", Decimals: 9, RiskClass: assets.RiskMajor, Status: assets.StatusActive})
		if err != nil {
			return err
		}
		usd, err := ensureAsset(ctx, tx, assetRepo, assets.Asset{Chain: assets.FiatChain, MintAddress: "USD", Kind: assets.KindFiat, Symbol: "USD", Name: "US Dollar (valuation reference)", Decimals: 2, RiskClass: assets.RiskUnsupported, Status: assets.StatusRestricted})
		if err != nil {
			return err
		}
		summary = append(summary, "assets: USDC(devnet) SOL(devnet) USD(fiat reference)")

		// Venue + instrument + listing.
		venue, err := instRepo.GetVenueByCode(ctx, tx, "JUPITER")
		if errs.CodeOf(err) == errs.CodeNotFound {
			venue, err = instRepo.CreateVenue(ctx, tx, instruments.Venue{Code: "JUPITER", Name: "Jupiter Swap V2 (devnet fake)", Kind: instruments.VenueDEXAggregator, Chain: chain, Status: instruments.VenueActive})
		}
		if err != nil {
			return err
		}
		var ins instruments.Instrument
		pairExists, err := tolerateConflict(ctx, tx, func(sp pgx.Tx) error {
			var cerr error
			ins, cerr = instRepo.CreateSpotPair(ctx, sp, instruments.SpotPairSpec{Base: sol.ID, Quote: usdc.ID, Settlement: usdc.ID, CanonicalName: "SOL/USDC", RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: now})
			return cerr
		})
		if err != nil {
			return err
		}
		if pairExists {
			list, lerr := instRepo.List(ctx, tx, 100)
			if lerr != nil {
				return lerr
			}
			found := false
			for _, i := range list {
				if i.CanonicalName == "SOL/USDC" {
					ins, found = i, true
				}
			}
			if !found {
				return errs.New(errs.CodeInternal, "seed: SOL/USDC conflicted on create but is not in the instrument list")
			}
		}
		minNotional, _ := money.ParseQuantity("1000000") // 1 USDC
		if _, err := tolerateConflict(ctx, tx, func(sp pgx.Tx) error {
			_, cerr := instRepo.CreateListing(ctx, sp, instruments.VenueListing{VenueID: venue.ID, InstrumentID: ins.ID, VenueNativeID: assets.NativeMintSentinel + "/" + usdcDevMint, Network: chain, BaseMint: "So11111111111111111111111111111111111111112", QuoteMint: usdcDevMint, BasePrecision: 9, QuotePrecision: 6, MinNotionalQuote: minNotional, Status: instruments.VenueActive})
			return cerr
		}); err != nil {
			return err
		}
		summary = append(summary, "instrument: SOL/USDC listed on JUPITER")

		// Asset policies (append-only; a new row per run is harmless but labeled).
		for _, np := range []valuation.NewPolicy{
			{AssetID: usdc.ID, Status: assets.StatusActive, CollateralFactor: 10_000, StablecoinStatus: valuation.StablecoinNormal, MaxPriceAge: time.Minute, PolicyVersion: "seed-v1", EffectiveAt: now, ActorType: "SYSTEM", ActorID: "seed", Reason: "LOCAL seed"},
			{AssetID: sol.ID, Status: assets.StatusActive, CollateralFactor: 8_000, MaxPriceAge: time.Minute, PolicyVersion: "seed-v1", EffectiveAt: now, ActorType: "SYSTEM", ActorID: "seed", Reason: "LOCAL seed"},
		} {
			if _, err := policies.RecordPolicy(ctx, tx, np); err != nil {
				return err
			}
		}
		// Prices: 150.00 USD per SOL; 1.00 USD per USDC (mantissa/scale exact).
		for _, po := range []valuation.PriceObservation{
			{AssetID: sol.ID, QuoteAssetID: usd.ID, Mantissa: money.QuantityFromInt64(15000), Scale: 2, Source: "seed", ObservedAt: now},
			{AssetID: usdc.ID, QuoteAssetID: usd.ID, Mantissa: money.QuantityFromInt64(100), Scale: 2, Source: "seed", ObservedAt: now},
		} {
			if _, err := tolerateConflict(ctx, tx, func(sp pgx.Tx) error {
				_, cerr := prices.RecordPrice(ctx, sp, po)
				return cerr
			}); err != nil {
				return err
			}
		}
		summary = append(summary, "policies: USDC NORMAL 100%, SOL 80% collateral; prices: SOL 150.00 USD, USDC 1.00 USD")

		// Dev identities → users + accounts.
		var customerA accounts.Account
		for _, name := range []string{"customer-a", "customer-b", "admin"} {
			subject := "dev:" + name
			u, err := acct.GetUserBySubject(ctx, tx, devIssuer, subject)
			switch {
			case errs.CodeOf(err) == errs.CodeNotFound:
				if u, err = acct.CreateUser(ctx, tx, devIssuer, subject, nil); err != nil {
					return err
				}
				a, err := acct.CreateAccount(ctx, tx, u.ID, accounts.KindCustomer)
				if err != nil {
					return err
				}
				if name == "customer-a" {
					customerA = a
				}
				if name == "admin" {
					if _, err := tx.Exec(ctx, `INSERT INTO operator_roles (user_id, role, reason) VALUES ($1, 'ADMIN', 'LOCAL seed') ON CONFLICT DO NOTHING`, u.ID); err != nil {
						return err
					}
				}
			case err != nil:
				return err
			case name == "customer-a":
				owned, err := acct.ListByOwner(ctx, tx, u.ID)
				if err != nil {
					return err
				}
				customerA = owned[0]
			}
		}
		summary = append(summary, "users: dev:customer-a, dev:customer-b, dev:admin (ADMIN operator role)")

		// SEED posting: 10,000 fake USDC for customer-a.
		//
		// EffectiveAt is a FIXED epoch, not `now`. internal/ledger includes
		// effective_at in the posting content hash at RFC3339Nano precision, so
		// passing the wall clock would produce different content under the same
		// idempotency key on every run — which the ledger correctly rejects as
		// INVALID_IDEMPOTENCY_REUSE. Re-seeding used to fail with exactly that.
		// A fixed effective time is also simply more correct for deterministic
		// dev data: the same seed should describe the same financial fact.
		qty, _ := money.ParseQuantity("10000000000")
		res, err := ledgerSvc.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSeed, IdempotencyKey: "seed:customer-a:usdc:10000", Reference: ledger.FinancialEventReference{Type: "seed", ID: "customer-a"},
			EffectiveAt: seedEpoch, Description: "LOCAL SEED — fake balance, never real value",
			Entries: []ledger.Entry{
				{Account: ledger.AccountRef{OwnerType: ledger.OwnerCustomer, OwnerID: customerA.ID.String(), Code: ledger.CodeWallet, AssetID: usdc.ID}, Side: ledger.Debit, Quantity: qty},
				{Account: ledger.AccountRef{OwnerType: ledger.OwnerCustomer, OwnerID: customerA.ID.String(), Code: ledger.CodeCapital, AssetID: usdc.ID}, Side: ledger.Credit, Quantity: qty},
			},
		})
		if err != nil {
			return err
		}
		if res.Existing {
			summary = append(summary, "ledger: SEED posting already present (idempotent)")
		} else {
			summary = append(summary, "ledger: SEED posting 10,000.000000 USDC → customer-a WALLET")
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Println("seed: LOCAL FAKE DATA loaded (environment " + env + ")")
	for _, s := range summary {
		fmt.Println("  - " + s)
	}
	return nil
}

// tolerateConflict runs fn inside a SAVEPOINT and reports whether it failed
// with CONFLICT.
//
// This is not defensive politeness, it is required. PostgreSQL aborts the
// ENTIRE transaction on any failed statement, so after a unique violation every
// later statement fails with SQLSTATE 25P02, "current transaction is aborted",
// and the original error is gone. Seeding a database that already held data
// used to fail with exactly that: the reported error named an innocent later
// query while the real conflict was invisible. A savepoint scopes the damage so
// the enclosing transaction survives an expected conflict.
//
// Only wrap statements whose conflict is genuinely expected. A conflict that is
// swallowed here and not handled by the caller is a silent no-op.
func tolerateConflict(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) (conflicted bool, err error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	if ferr := fn(sp); ferr != nil {
		_ = sp.Rollback(ctx)
		if errs.CodeOf(ferr) == errs.CodeConflict {
			return true, nil
		}
		return false, ferr
	}
	return false, sp.Commit(ctx)
}

// seedEpoch is the effective time of every idempotent seed posting. It must
// never be derived from the clock: see the SEED posting below.
var seedEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func ensureAsset(ctx context.Context, tx pgx.Tx, repo *assets.Repository, a assets.Asset) (assets.Asset, error) {
	existing, err := repo.GetByMint(ctx, tx, a.Chain, a.MintAddress)
	if err == nil {
		return existing, nil
	}
	if errs.CodeOf(err) != errs.CodeNotFound {
		return assets.Asset{}, err
	}
	return repo.Create(ctx, tx, a)
}
