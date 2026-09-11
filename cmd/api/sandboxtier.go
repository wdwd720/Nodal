package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/provider/payoutsandbox"
	"github.com/nodal/controlplane/internal/provider/verifysandbox"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/verification"
)

// The sandbox tier (ADR-0023).
//
// One declaration -- CP_API_LEGAL_POLICY=SANDBOX, which config.Validate refuses
// in PROD -- is the condition for every affordance here: SANDBOX gate rows are
// read as active, the sandbox payout policy may be named, the sandbox payout
// provider may be registered, and the gates the blueprint lists are
// sandbox-activated at boot. None of it exists on any other deployment, and a
// production deployment reaches ACTIVE the way it always has.

// payoutPolicyFor returns the deployment's payout policy: nil (the fail-closed
// default) unless the sandbox tier's rehearsal policy was named.
func payoutPolicyFor(cfg *config.Config) *valuedomain.Policy {
	name, ok := config.NormalizePayoutPolicy(cfg.API.PayoutPolicy)
	if !ok || name != config.PayoutPolicySandbox || !cfg.SandboxTier() {
		return nil
	}
	p := valuedomain.SandboxPolicy()
	return &p
}

// registerSandboxPayoutProvider registers the provider that moves nothing,
// when the deployment is a sandbox tier and its payout slot names it. Any
// other name in the slot is a real adapter's and is wired elsewhere or not
// at all.
func registerSandboxPayoutProvider(cfg *config.Config, reg *payout.Registry, clk clock.Clock, log *slog.Logger) error {
	slot := cfg.Providers.Payout
	if !cfg.SandboxTier() || slot.Mode != config.ProviderModeSandbox || strings.TrimSpace(slot.Name) != payoutsandbox.Name {
		return nil
	}
	p, err := payoutsandbox.New(cfg.Env, clk.Now)
	if err != nil {
		return err
	}
	if err := reg.Register(p); err != nil {
		return err
	}
	log.Warn("payouts go to a sandbox provider that moves nothing",
		"provider", p.Name(), "settles_after", payoutsandbox.SettleAfter,
		"consequence", "a payout on this deployment is a rehearsal; no value leaves the system")
	return nil
}

// registerSandboxVerificationProvider registers the identity provider that
// decides nothing on its own, when the deployment is a sandbox tier.
//
// It is not keyed on a provider slot, unlike the payout one, because there is
// no identity slot in configuration: a deployment either has a contracted
// identity vendor -- which nobody does yet (BLOCKERS B-06) -- or it has the
// rehearsal one, and a sandbox tier is the only place the rehearsal one may
// exist. The provider refuses PROD on its own account as well, so a production
// binary that somehow reached this line would get an error rather than a
// provider that pretends to verify people.
func registerSandboxVerificationProvider(cfg *config.Config, reg *verification.Registry, clk clock.Clock, log *slog.Logger) error {
	if !cfg.SandboxTier() {
		return nil
	}
	p, err := verifysandbox.New(cfg.Env, clk.Now)
	if err != nil {
		return err
	}
	if err := reg.Register(p); err != nil {
		return err
	}
	log.Warn("identity verification goes to a sandbox provider that decides nothing on its own",
		"provider", p.Name(), "control", verification.SandboxControlPath,
		"consequence", "a verification on this deployment is a rehearsal chosen explicitly; it is not an approval and no provider has assessed anybody")
	return nil
}

// creditAssetDecimals is the scale of the Credit asset, read from the registry
// rather than assumed to be six.
//
// A native asset may be created with up to eighteen decimals and the Credit
// asset is a registry row like any other; F-44 was exactly this figure being
// assumed, and a quote computed at the wrong scale is wrong by a factor of a
// million. A deployment with no Credit asset registered has no internal economy
// and reports zero, which every caller treats as "no pricing".
func creditAssetDecimals(ctx context.Context, database *db.DB, repo *assets.Repository) (uint8, error) {
	var assetID assets.AssetID
	err := database.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&assetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("credit asset: %w", err)
	}
	asset, err := repo.Get(ctx, database, assetID)
	if err != nil {
		return 0, fmt.Errorf("credit asset: %w", err)
	}
	return asset.Decimals, nil
}

// sandboxGatesAtBoot sandbox-activates the capabilities the deployment lists,
// once, idempotently. The deployment configuration is the authority: it is in
// the blueprint, reviewed like every other line there, and config.Validate
// refuses it anywhere but a sandbox tier. A gate already in SANDBOX is left
// alone; a gate in any state that is not a legal source of SANDBOX (an ACTIVE
// one, say) is refused loudly rather than moved, because moving it would be
// changing a real approval's state from a config file.
func sandboxGatesAtBoot(ctx context.Context, database *db.DB, cfg *config.Config, clk clock.Clock, audit gates.AuditAppender, log *slog.Logger) error {
	raw := strings.TrimSpace(cfg.API.SandboxGates)
	if raw == "" {
		return nil
	}
	if !cfg.SandboxTier() {
		return fmt.Errorf("CP_API_SANDBOX_GATES is set but this deployment is not a sandbox tier")
	}
	var caps []gates.Capability
	for _, name := range strings.Split(raw, ",") {
		name = strings.ToUpper(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		c := gates.Capability(name)
		if !c.Valid() {
			return fmt.Errorf("CP_API_SANDBOX_GATES names an unknown capability %q", name)
		}
		caps = append(caps, c)
	}
	var moved []gates.Gate
	err := database.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		moved, err = gates.BootstrapSandbox(ctx, tx, string(cfg.Env), caps, clk, audit)
		return err
	})
	if err != nil {
		return err
	}
	for _, g := range moved {
		log.Warn("capability sandbox-activated at boot: this gate carries no approval and is active on a sandbox tier only",
			"capability", string(g.Capability), "environment", g.Environment, "state", string(g.State))
	}
	if len(moved) == 0 {
		log.Info("sandbox gates already in place", "capabilities", raw)
	}
	return nil
}

// creditAssetAtBoot registers THE Credit asset on a sandbox tier that has none.
//
// # Why this exists
//
// Every part of the internal economy is keyed on one `assets` row with kind
// CREDIT: `credit.Service.AssetID` looks it up, the quote's scale comes from its
// decimals, and `demo.NewSeeder` refuses to run without it. Nothing in any
// deployment wrote one. `scripts/seedeconomy` does, and it refuses to run
// anywhere but LOCAL, DEV and TEST -- which are exactly the environments that
// are NOT the sandbox tier.
//
// So STAGING, the deployment whose whole purpose is to rehearse the product,
// booted with no Credit asset: `demoDataAtBoot` logged "this deployment has no
// Credit asset" and returned, the markets page stayed empty, and nobody could
// buy a Credit. The seeder D-068 built was a permanent no-op on the one tier it
// was built for.
//
// # The same shape as the risk policy at boot (D-067)
//
// Through the domain service -- `assets.Repository.Create`, which validates the
// definition and is the same call `scripts/seedeconomy` makes -- never SQL.
// Idempotent, because migration 00711 permits exactly one Credit asset per
// deployment, so this is a lookup first and a race resolves as a CONFLICT that
// is treated as success.
//
// # Why PROD is refused
//
// The scale is a decision. Six decimals is chosen here because a bonding-curve
// market has to price units far below one Credit and a coarser scale makes
// rounding to zero a usable exploit -- but it is still a decision, and a
// production deployment's unit of account should be created by a person who
// knows they are creating it. A PROD deployment with no Credit asset refuses
// every purchase, which is the correct failure, and `scripts/seedeconomy` is
// not the tool for it.
func creditAssetAtBoot(ctx context.Context, database *db.DB, cfg *config.Config, repo *assets.Repository, log *slog.Logger) error {
	if !cfg.SandboxTier() || cfg.Env == config.EnvProd {
		return nil
	}
	var existing assets.AssetID
	err := database.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("credit asset at boot: %w", err)
	}
	created, err := repo.Create(ctx, database, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindCredit,
		ValueDomain: valuedomain.InternalCredit,
		Symbol:      "CREDIT", Name: "Nodal Credit",
		Decimals:  creditAssetDecimalsAtBoot,
		RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
	})
	if err != nil {
		if errs.CodeOf(err) == errs.CodeConflict {
			// Another instance registered it first. That is the intended
			// outcome of a race, not a failure.
			return nil
		}
		return fmt.Errorf("credit asset at boot: %w", err)
	}
	log.Warn("registered the Credit asset at boot: this is a sandbox tier and its Credits are a rehearsal",
		"asset_id", created.ID.String(), "decimals", created.Decimals, "environment", string(cfg.Env),
		"consequence", "the internal economy can be exercised here; PROD registers its own unit of account by hand")
	return nil
}

// creditAssetDecimalsAtBoot is the scale the sandbox tier's Credit carries.
//
// Six, matching scripts/seedeconomy, for the reason recorded there: a
// bonding-curve market prices units far below one Credit, and a coarser scale
// would make rounding to zero a usable exploit.
const creditAssetDecimalsAtBoot = 6
