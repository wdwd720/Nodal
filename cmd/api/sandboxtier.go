package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/provider/payoutsandbox"
	"github.com/nodal/controlplane/internal/valuedomain"
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
