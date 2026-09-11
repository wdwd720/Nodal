package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/provider/stripecredit"
)

// Environment variables for the Credit funding sweep.
const (
	// The reversibility window is CP_CREDIT_SETTLEMENT_WINDOW, and it is read
	// from the configuration table rather than from the environment: it is a
	// risk determination somebody has to make and record, and a value read
	// straight from the environment is outside the configuration hash. See
	// config.CreditConfig.
	envCreditInterval = "CP_CREDIT_SWEEP_INTERVAL" //nolint:gosec // G101: the name of an interval variable, not a credential

	defaultSettlementWindow = 30 * 24 * time.Hour
	defaultCreditInterval   = 15 * time.Minute
)

// creditSweeper promotes funded Credits out of the reversibility window, and
// asks the provider about purchases that have been in flight too long.
//
// It exists because both of those are things nothing was doing. SettleDue is
// what makes purchased Credits capable of ever being paid out; without a caller
// every purchase stays REVERSIBLE forever and the payout policy correctly
// refuses all of it. Reconcile is what closes the gap a lost create response
// opens, and without a caller a payment taken during a network failure is never
// discovered.
type creditSweeper struct {
	db      *db.DB
	credits *credit.Service
	svc     *credit.PurchaseService
	window  time.Duration
	log     *slog.Logger
}

// newCreditSweeper builds the sweeper, or returns nil when this deployment has
// no Credit purchase provider.
//
// Nil is the ordinary case and not a failure. A deployment that cannot sell
// Credits has no funding to settle, and starting a sweep over an empty table
// every fifteen minutes would only teach operators to ignore its log lines.
func newCreditSweeper(ctx context.Context, cfg *config.Config, database *db.DB,
	resolver config.Resolver, clk clock.Clock, log *slog.Logger,
) *creditSweeper {
	slot := cfg.Providers.CreditPurchase
	if slot.Mode == "" || slot.Name == "" {
		log.InfoContext(ctx, "credit purchase provider is not configured; the Credit settlement sweep is disabled")
		return nil
	}
	prov, err := stripecredit.New(ctx, slot, cfg.Env, resolver, clk,
		&http.Client{Timeout: slot.Timeout}, nil)
	if err != nil {
		log.WarnContext(ctx, "credit purchase provider could not be built; the Credit settlement sweep is disabled",
			slog.String("error", err.Error()))
		return nil
	}
	credits := credit.NewService(ledger.NewService(clk, config.BuildVersion), clk)

	// The gate checker refuses everything, and that is correct here. This
	// worker never STARTS a purchase -- it only advances ones that already
	// exist -- so the capability that gates selling Credits is not consulted on
	// any path it takes. Supplying a checker that permits nothing means a bug
	// that made this worker start a purchase would be refused rather than
	// silently allowed.
	checker, err := gates.NewChecker(string(cfg.Env), func(gates.Capability) bool { return false }, clk)
	if err != nil {
		log.WarnContext(ctx, "credit gate checker could not be built; the Credit settlement sweep is disabled",
			slog.String("error", err.Error()))
		return nil
	}
	svc, err := credit.NewPurchaseService(ctx, database, credit.PurchaseServiceConfig{
		Credits: credits, Provider: prov, Pricing: credit.DefaultPricingPolicy(),
		Gates: checker, Clock: clk, Environment: string(cfg.Env),
		ProviderMode: string(slot.Mode),
	})
	if err != nil {
		log.WarnContext(ctx, "credit purchase service could not be built; the Credit settlement sweep is disabled",
			slog.String("error", err.Error()))
		return nil
	}
	return &creditSweeper{db: database, credits: credits, svc: svc, log: log}
}

// settle promotes every funding whose reversibility window has closed.
func (s *creditSweeper) settle(ctx context.Context, batch int) {
	if s == nil {
		return
	}
	var n int
	err := s.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			n, serr = s.svc.SettleDue(ctx, tx, s.window, batch)
			return serr
		})
	if err != nil {
		s.log.ErrorContext(ctx, "Credit settlement sweep failed", slog.String("error", err.Error()))
		return
	}
	if n > 0 {
		s.log.InfoContext(ctx, "Credit settlement sweep complete",
			slog.Int("settled", n), slog.Duration("window", s.window))
	}
}

// reconcile asks the provider what happened to purchases that are still in
// flight, and expire ends the ones nobody completed.
//
// Both are internal/credit's, not this file's. They used to be here -- and the
// listing query here carried `provider_reference IS NOT NULL`, which excluded
// exactly the fundings the sweep existed to recover (F-154), while nothing
// anywhere ended an abandoned checkout at all (F-153). A pass that has to run
// on every tier belongs in the package that owns the state machine, so the API
// process and this worker cannot drift into running different ones.
func (s *creditSweeper) reconcile(ctx context.Context, batch int) {
	if s == nil {
		return
	}
	checked, err := s.svc.ReconcileDue(ctx, s.db, credit.DefaultReconcileAfter, batch)
	if err != nil {
		s.log.WarnContext(ctx, "purchase reconciliation failed", slog.String("error", err.Error()))
	}
	if checked > 0 {
		s.log.InfoContext(ctx, "Credit purchase reconciliation complete", slog.Int("checked", checked))
	}
}

// expire cancels purchases that have been in flight past the provider's intent
// lifetime, so an abandoned checkout stops holding money-at-risk headroom that
// nothing else would ever release.
func (s *creditSweeper) expire(ctx context.Context, batch int) {
	if s == nil {
		return
	}
	n, err := s.svc.ExpireInFlight(ctx, s.db, credit.DefaultInFlightLifetime, batch)
	if err != nil {
		s.log.WarnContext(ctx, "Credit purchase expiry failed", slog.String("error", err.Error()))
	}
	if n > 0 {
		s.log.InfoContext(ctx, "Credit purchase expiry complete", slog.Int("expired", n))
	}
}
