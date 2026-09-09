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
	// envSettlementWindow is how long a captured card payment stays
	// reversible before its Credits may be treated as settled.
	//
	// There is no default seven days here, because the goal document is
	// explicit that an arbitrary number is not a policy. The default below is
	// thirty days and it is a CONSERVATIVE placeholder, not a determination:
	// card scheme chargeback windows run to 120 days and beyond, and the real
	// value is a risk decision somebody has to make and record.
	envSettlementWindow = "CP_CREDIT_SETTLEMENT_WINDOW"
	envCreditInterval   = "CP_CREDIT_SWEEP_INTERVAL"

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
	resolver config.Resolver, clk clock.Clock, log *slog.Logger) *creditSweeper {

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
	svc, err := credit.NewPurchaseService(credit.PurchaseServiceConfig{
		Credits: credits, Provider: prov, Pricing: credit.DefaultPricingPolicy(),
		Gates: checker, Clock: clk, Environment: string(cfg.Env),
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
// flight, one transaction each.
//
// One transaction per purchase, not one for the batch: a provider call inside a
// transaction holds a database connection across the network, and a batch of
// them holds several. The pool starvation that caused (F-27) is the reason this
// loop looks inefficient and is not.
func (s *creditSweeper) reconcile(ctx context.Context, batch int) {
	if s == nil {
		return
	}
	ids, err := s.stale(ctx, batch)
	if err != nil {
		s.log.ErrorContext(ctx, "could not list purchases to reconcile", slog.String("error", err.Error()))
		return
	}
	var checked int
	for _, id := range ids {
		err := s.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, rerr := s.svc.Reconcile(ctx, tx, id)
				return rerr
			})
		if err != nil {
			s.log.WarnContext(ctx, "purchase reconciliation failed",
				slog.String("funding_id", id.String()), slog.String("error", err.Error()))
			continue
		}
		checked++
	}
	if checked > 0 {
		s.log.InfoContext(ctx, "Credit purchase reconciliation complete", slog.Int("checked", checked))
	}
}

// stale lists purchases that have been waiting on the provider longer than the
// customer would have.
//
// Fifteen minutes is not a policy: it is longer than any card authorisation
// takes and shorter than a person's patience, so anything past it is either
// abandoned or a lost response, and both want the same question asked.
func (s *creditSweeper) stale(ctx context.Context, batch int) ([]credit.FundingID, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id FROM credit_fundings
		  WHERE state IN ('CREATED','AUTHORIZATION_PENDING','AUTHORIZED','CAPTURE_PENDING')
		    AND provider_reference IS NOT NULL
		    AND updated_at < now() - interval '15 minutes'
		  ORDER BY updated_at
		  LIMIT $1`, batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []credit.FundingID
	for rows.Next() {
		var id credit.FundingID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
