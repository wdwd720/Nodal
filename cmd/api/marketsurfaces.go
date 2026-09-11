package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/demo"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// Boot-time setup for the internal market surfaces (product goal §47, §51).

// riskPolicyAtBoot records the conservative GLOBAL risk policy on a
// non-production deployment that has none.
//
// # Why this exists
//
// internal/nativemarket refuses every internal trade when no GLOBAL risk policy
// is on record, and it is right to: a deployment that has not decided how much
// of a market one account may hold has not decided that any amount is fine. But
// nothing in any deployment wrote one. `scripts/riskpolicy` exists and is a
// person running a command, so every fresh LOCAL, DEV, TEST or STAGING database
// refused every native trade until somebody remembered — which is a control
// that mostly teaches people to work around it.
//
// # Why PROD is not included, and never will be
//
// The compiled-in limits are starter values chosen to be small. Nobody signed
// them off. Seeding PROD with them would produce exactly the thing the audit is
// against: a control that LOOKS decided and is not. A production deployment
// with no policy refuses every trade until an operator records one with
// `scripts/riskpolicy -rules`, and that is the correct failure.
//
// # How
//
// Through risk.Store.RecordPolicy — the same domain service scripts/riskpolicy
// calls — never SQL. The actor is SYSTEM `config:bootstrap`, so the row says
// what put it there, and the version is derived from the policy's own content
// hash, so a second boot is a CONFLICT on the unique version rather than a
// second row saying the same thing.
func riskPolicyAtBoot(ctx context.Context, database *db.DB, cfg *config.Config, clk clock.Clock, log *slog.Logger) error {
	if cfg.Env == config.EnvProd {
		return nil
	}
	now := clk.Now().UTC()
	store := risk.NewStore()
	if _, _, err := store.EffectivePolicy(ctx, database, "", "", now); err == nil {
		return nil
	} else if !errors.Is(err, risk.ErrNoPolicy) {
		return err
	}

	parsed, err := risk.ParsePolicy(json.RawMessage(risk.DefaultGlobalPolicyJSON))
	if err != nil {
		return err
	}
	if verr := parsed.Validate(risk.ScopeGlobal); verr != nil {
		return verr
	}
	// Derived from the rules, so the version names WHAT was recorded rather
	// than WHEN. Two deployments that boot with the same compiled-in limits
	// carry the same version, and a change to those limits is a new version by
	// construction.
	version := "bootstrap-conservative-" + parsed.Hash()[:12]

	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: "config:bootstrap", ActorType: security.ActorSystem, AuthTime: now,
	})
	err = database.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, rerr := store.RecordPolicy(ctx, tx, risk.PolicyRecord{
			Scope:       risk.ScopeGlobal,
			Version:     version,
			Rules:       json.RawMessage(risk.DefaultGlobalPolicyJSON),
			EffectiveAt: now,
			ActorType:   security.ActorSystem,
			ActorID:     "config:bootstrap",
			Reason: "no GLOBAL risk policy was on record and this is not PROD, so the internal " +
				"economy could not be evaluated at all. These are the COMPILED-IN STARTER limits " +
				"nobody has signed off; replace them with `go run ./scripts/riskpolicy -rules`.",
		})
		return rerr
	})
	switch {
	case err == nil:
		log.Warn("recorded the compiled-in GLOBAL risk policy at boot",
			"version", version, "environment", string(cfg.Env),
			"consequence", "these limits are starter values nobody signed off; PROD is never seeded this way")
		return nil
	case errs.CodeOf(err) == errs.CodeConflict:
		// Another instance of this process recorded it first. That is the
		// intended outcome of a race, not a failure.
		return nil
	default:
		return err
	}
}

// demoSeedDeps are what demoDataAtBoot needs beyond the configuration.
type demoSeedDeps struct {
	DB      *db.DB
	Assets  *nativeasset.Service
	Markets *nativemarket.Service
	Credits *credit.Service
	Clock   clock.Clock
}

// demoDataAtBoot loads the sandbox demo catalogue when the deployment is a
// sandbox tier and asks for it.
//
// It refuses PROD twice — here and in demo.NewSeeder — and a third time in the
// database, where migration 00774's CHECK forbids a demo row whose environment
// is PROD. A failure is logged and does not stop the process: a STAGING
// deployment that came up without its demo markets is a deployment with an
// empty markets page, which is a nuisance; one that refuses to start is an
// outage.
func demoDataAtBoot(ctx context.Context, cfg *config.Config, d demoSeedDeps, log *slog.Logger) {
	if !cfg.SandboxTier() || cfg.Env == config.EnvProd {
		return
	}
	creditAsset, err := d.Credits.AssetID(ctx, d.DB)
	if err != nil {
		log.Warn("demo data not seeded: this deployment has no Credit asset",
			"error", err.Error(),
			"consequence", "the markets page will be empty until the internal economy is provisioned")
		return
	}
	seeder, err := demo.NewSeeder(demo.Deps{
		DB: d.DB, Assets: d.Assets, Markets: d.Markets, Credits: d.Credits, Clock: d.Clock,
		CreditAssetID: creditAsset,
		Environment:   string(cfg.Env),
		SandboxTier:   cfg.SandboxTier(),
	})
	if err != nil {
		log.Warn("demo data not seeded", "error", err.Error())
		return
	}
	res, err := seeder.Seed(ctx)
	if err != nil {
		log.Warn("demo data could not be seeded in full",
			"error", err.Error(),
			"created", len(res.Created),
			"consequence", "some demo markets are missing; nothing real is affected")
		return
	}
	if len(res.Created) == 0 {
		log.Info("demo data already present", "markets", len(res.Markets))
		return
	}
	log.Warn("SANDBOX DEMO DATA seeded: these markets are simulated and represent nothing",
		"created", len(res.Created), "markets", len(res.Markets), "symbols", demo.Specs())
}
