// Command settlementasset registers the USD-pegged asset that funds settle
// into, in any environment.
//
//	go run ./scripts/settlementasset -chain solana-devnet \
//	  -mint 4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU \
//	  -symbol USDC -name "USD Coin (devnet)" -decimals 6
//
// It exists because cmd/api refuses to start until CP_API_SETTLEMENT_CHAIN and
// CP_API_SETTLEMENT_MINT resolve to a registered stablecoin, and there was no
// way to register one outside LOCAL. scripts/seed does it, and scripts/seed
// refuses to run anywhere but LOCAL, DEV and TEST -- correctly, because seeding
// invents identities, accounts and ledger postings. Registering the one asset a
// deployment settles into is not seeding. It is a deliberate operator act with
// exactly one row in it.
//
// It writes through assets.Repository, not through raw SQL, so every invariant
// that repository enforces applies here too: the value domain, the risk class,
// the peg, and the status transition rules. A tool that INSERTed directly would
// be a second way to create an asset, and the second way is always the one that
// skips a check.
//
// Idempotent: an asset already registered for the chain and mint is reported
// and left exactly as it is. It is never updated, because changing the
// decimals or the peg of an asset that balances already reference is not a
// correction, it is a restatement of every balance denominated in it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/valuedomain"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "settlementasset:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		chain    = flag.String("chain", "", "chain identifier, e.g. solana or solana-devnet")
		mint     = flag.String("mint", "", "mint address of the asset")
		symbol   = flag.String("symbol", "USDC", "ticker symbol")
		name     = flag.String("name", "", "human-readable name")
		decimals = flag.Uint("decimals", 6, "on-chain decimals")
		peg      = flag.String("peg", "USD", "currency this asset is pegged to")
		urlEnv   = flag.String("url-env", "CP_DATABASE_APP_URL_PLAIN", "environment variable holding the connection string")
		timeout  = flag.Duration("timeout", 60*time.Second, "overall timeout")
	)
	flag.Parse()

	for name, v := range map[string]string{"-chain": *chain, "-mint": *mint, "-symbol": *symbol} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if *name == "" {
		*name = *symbol
	}
	if *decimals > 18 {
		return errors.New("-decimals looks wrong; no SPL token has more than 18")
	}

	dsn := strings.TrimSpace(os.Getenv(*urlEnv))
	if dsn == "" {
		return fmt.Errorf("%s is not set", *urlEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	database, err := db.Open(ctx, db.Config{URL: dsn, AppName: "settlementasset", MaxConns: 2, RequireTLS: strings.Contains(dsn, "verify-")})
	if err != nil {
		// Never quote the DSN: it carries a password.
		return fmt.Errorf("opening the database: %w", err)
	}
	defer database.Close()

	repo := assets.NewRepository()

	if existing, err := repo.GetByMint(ctx, database, *chain, *mint); err == nil {
		fmt.Printf("already registered: %s %s/%s id=%s stablecoin=%v status=%s\n",
			existing.Symbol, existing.Chain, existing.MintAddress, existing.ID, existing.IsStablecoin, existing.Status)
		if !existing.IsStablecoin {
			return errors.New("that asset exists and is NOT a stablecoin; cmd/api will refuse it as a settlement asset")
		}
		return nil
	}

	created, err := repo.Create(ctx, database, assets.Asset{
		Chain:       *chain,
		MintAddress: *mint,
		Kind:        assets.KindSPLToken,
		// Self-custodial crypto: the holder's own wallet holds it, which is
		// what makes it a settlement asset rather than an internal balance.
		ValueDomain:  valuedomain.SelfCustodialCrypto,
		Symbol:       *symbol,
		Name:         *name,
		Decimals:     uint8(*decimals),
		IsStablecoin: true,
		PegCurrency:  *peg,
		// RiskSettlement, not RiskMajor: this asset is what other things settle
		// INTO, so its risk class is the one the settlement path assumes.
		RiskClass: assets.RiskSettlement,
		Status:    assets.StatusActive,
	})
	if err != nil {
		return fmt.Errorf("registering the asset: %w", err)
	}

	fmt.Printf("registered: %s %s/%s id=%s decimals=%d peg=%s\n",
		created.Symbol, created.Chain, created.MintAddress, created.ID, created.Decimals, created.PegCurrency)
	return nil
}
