package advancedjobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/marketplace"
)

// MarketplaceFollows runs the scheduled paper follow of every due install
// (marketplace.Service.FollowScheduled), as the SYSTEM scheduler: the
// installed strategy version runs in the sandbox on the input the marketplace
// builds itself for the run's instant -- the install's paper book and the
// price store's fresh reference prices -- and the intents it proposes are
// submitted through the marketplace's own pipeline. Which installs are due -- ACTIVE, of an
// approved and cleared version, at least the manifest's interval since their
// last run -- is the marketplace's exported reader's answer
// (marketplace.Service.DueInstalls).
//
// It is the one job that adds risk (paper intents), so the marketplace
// authorizes every run: while the MARKETPLACE gate is inactive in STAGING or
// PROD, or a kill switch stops it, the follow is refused and the install is
// reported Deferred -- the job neither waits for nor works around a switch.
// A run is keyed by (install, instant): a repeat of the same instant replays
// the recorded run.
//
// A paper book that holds an instrument with no fresh reference price cannot
// be valued, so FollowScheduled refuses it STALE_MARKET_DATA until a mark
// arrives; that, a run beyond the account's sandbox budget (RATE_LIMITED) and
// UNSUPPORTED are reported Deferred, not as failures: the marketplace said not
// now, and a job that is red until the next mark teaches people to ignore
// red.
//
// DueInstalls does not filter by mode, and a scheduled follow is a paper
// follow, so a non-PAPER install it returns is not this job's and is skipped.
type MarketplaceFollows struct {
	db   *db.DB
	svc  *marketplace.Service
	clk  clock.Clock
	spec Spec
}

// NewMarketplaceFollows wires the job.
func NewMarketplaceFollows(d *db.DB, svc *marketplace.Service, clk clock.Clock, s Settings) *MarketplaceFollows {
	return &MarketplaceFollows{db: d, svc: svc, clk: clk, spec: spec(NameMarketplaceFollows, s)}
}

// Spec implements Job.
func (j *MarketplaceFollows) Spec() Spec { return j.spec }

// Run follows each due PAPER install in its own transaction at one instant,
// the run's start truncated to the second.
func (j *MarketplaceFollows) Run(ctx context.Context) (Report, error) {
	asOf := j.clk.Now().UTC().Truncate(time.Second)
	var due []marketplace.Install
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		due, err = j.svc.DueInstalls(ctx, tx, asOf, j.spec.Batch)
		return err
	}); err != nil {
		return Report{}, err
	}
	var rep Report
	for _, in := range due {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if in.Mode != gates.ModePaper {
			// A scheduled follow is a paper follow; the marketplace refuses
			// any other mode, so such an install is not this job's.
			continue
		}
		rep.Examined++
		var run marketplace.FollowRun
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			run, err = j.svc.FollowScheduled(ctx, tx, in.ID, asOf)
			return err
		})
		switch errs.CodeOf(err) {
		case "":
			if !run.Replayed {
				rep.Changed++
			}
		case errs.CodeCapabilityNotApproved, errs.CodeKillSwitchActive, errs.CodeRateLimited, errs.CodeUnsupported,
			errs.CodeStaleMarketData:
			// The marketplace said not now: its gate, a kill switch, the
			// manifest's frequency or the account's sandbox budget, or no
			// fresh reference price for what the paper book holds. Nothing
			// to fix here.
			rep.Deferred++
		default:
			rep.fail(in.ID.String(), err)
		}
	}
	return rep, nil
}
