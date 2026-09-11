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
