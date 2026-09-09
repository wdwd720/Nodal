// Package capacity refuses financial actions before the infrastructure they
// depend on runs out.
//
// It exists because the launch tier runs on free infrastructure with hard
// quotas, and a quota is not a soft warning: a free Postgres that reaches its
// storage limit fails writes, and a write that fails halfway through issuing
// Credit against captured money is the one failure this system exists to
// prevent. So the ceilings are checked BEFORE the provider is called, in the
// same transaction as the action, and every uncertainty is a refusal.
//
// It is deliberately separate from internal/gates. A gate answers "is this
// deployment allowed to sell Credits at all", which is a policy decision made
// by people with dual control. This answers "is there room to do it safely
// right now", which is a measurement. Conflating them would let a capacity
// problem look like a revoked approval, and let raising a limit look like
// granting an approval.
//
// Every ceiling here is a launch-tier ceiling. On the paid AWS architecture the
// same guard runs with much larger numbers, or with MaxDatabaseBytes at zero,
// which disables only the infrastructure ceiling and keeps the financial ones.
package capacity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// Action is the kind of thing being admitted. The ceilings that apply differ:
// opening an account consumes cohort space, and buying Credit consumes
// transaction volume and money-at-risk headroom.
type Action string

// Actions.
const (
	// ActionOpenAccount adds a member to the launch cohort.
	ActionOpenAccount Action = "OPEN_ACCOUNT"
	// ActionCreditPurchase takes money and issues Credit against it.
	ActionCreditPurchase Action = "CREDIT_PURCHASE"
)

// ErrAtCapacity is returned when a ceiling has been reached. It is separate
// from a validation error and from a gate refusal, because the operator
// response differs: this one is "the launch tier is full", and the answer is
// either to wait or to migrate.
var ErrAtCapacity = errors.New("capacity: at the launch-tier ceiling")

// ErrUnmeasurable is returned when a ceiling cannot be evaluated. It is a
// refusal, not a pass. A guard that admits an action because it could not
// measure the thing it guards is not a guard.
var ErrUnmeasurable = errors.New("capacity: could not measure current usage")

// Budget is the set of hard ceilings for one deployment tier.
//
// A zero value in any field means "no ceiling of this kind", which is how the
// paid tier turns off the infrastructure ceilings while keeping the financial
// ones. It is not a default: NewGuard refuses a Budget with no ceilings at all,
// because a guard that guards nothing is worse than no guard -- it reads like
// protection.
type Budget struct {
	// MaxAccounts is the launch cohort size. The point is not that the 51st
	// user is unwelcome; it is that free-tier capacity was measured for a known
	// number and an unbounded one is not a plan.
	MaxAccounts int64

	// MaxPurchasesPerDay bounds transaction volume. It bounds provider webhook
	// volume and database growth at the same time, which is why it is a day
	// rather than a month: a month-long ceiling can be exhausted in an hour.
	MaxPurchasesPerDay int64

	// MaxAtRiskMinor is the most money that may be in a non-terminal funding
	// state at once, in minor units. This is the cap that matters most, because
	// it bounds what a failure can cost rather than what it can consume.
	MaxAtRiskMinor int64

	// MaxDatabaseBytes is the free tier's storage quota. Zero disables the
	// check, which is correct on infrastructure that has no such quota.
	MaxDatabaseBytes int64

	// DatabaseHeadroom is the fraction of MaxDatabaseBytes at which new
	// financial actions stop. It is below 1 on purpose: stopping AT the quota
	// means the write that discovers the quota is the one that fails, and the
	// database still needs room to record the refusal, run reconciliation and
	// be exported. 0 means the default.
	DatabaseHeadroom float64
}

// DefaultDatabaseHeadroom leaves a third of the quota free. That is not a
// round-number guess: the export of a full launch tier's ledger, the audit
// chain and the evidence rows has to fit somewhere, and it has to fit at the
// moment things are going badly rather than before.
const DefaultDatabaseHeadroom = 0.67

// LaunchTier is the budget for the free-infrastructure launch tier.
//
// The numbers come from the quotas they protect, not from ambition:
//
//   - 50 accounts, matching the cohort the tier was sized for.
//   - 200 purchases a day. At roughly 4 KB of ledger, funding and evidence
//     rows per purchase that is under a megabyte a day, so the 500 MB Postgres
//     quota survives well past the point where the cohort cap binds first.
//   - $2,000 at risk. A launch tier should not be able to owe more than its
//     operator can cover out of pocket while a dispute is resolved.
//   - 500 MB, which is the free Postgres storage quota.
func LaunchTier() Budget {
	return Budget{
		MaxAccounts:        50,
		MaxPurchasesPerDay: 200,
		MaxAtRiskMinor:     200_000,
		MaxDatabaseBytes:   500 * 1024 * 1024,
		DatabaseHeadroom:   DefaultDatabaseHeadroom,
	}
}

// Reading is what the guard measured, for logging and for the operator
// endpoint. It is returned even when the decision is a refusal, because the
// number that caused the refusal is the useful part.
type Reading struct {
	Accounts       int64
	PurchasesToday int64
	AtRiskMinor    int64
	DatabaseBytes  int64
	MeasuredAt     time.Time
}

// Guard measures usage and admits or refuses.
type Guard struct {
	budget Budget
	now    func() time.Time
}

// NewGuard returns a Guard. It refuses a budget with no ceilings at all,
// because such a guard would pass every check while looking like protection.
func NewGuard(b Budget, now func() time.Time) (*Guard, error) {
	if now == nil {
		return nil, errors.New("capacity: a clock is required")
	}
	if b.MaxAccounts <= 0 && b.MaxPurchasesPerDay <= 0 && b.MaxAtRiskMinor <= 0 && b.MaxDatabaseBytes <= 0 {
		return nil, errors.New("capacity: a budget with no ceiling guards nothing; state at least one")
	}
	for name, v := range map[string]int64{
		"MaxAccounts":        b.MaxAccounts,
		"MaxPurchasesPerDay": b.MaxPurchasesPerDay,
		"MaxAtRiskMinor":     b.MaxAtRiskMinor,
		"MaxDatabaseBytes":   b.MaxDatabaseBytes,
	} {
		if v < 0 {
			return nil, fmt.Errorf("capacity: %s is negative", name)
		}
	}
	if b.DatabaseHeadroom == 0 {
		b.DatabaseHeadroom = DefaultDatabaseHeadroom
	}
	if b.DatabaseHeadroom <= 0 || b.DatabaseHeadroom > 1 {
		return nil, fmt.Errorf("capacity: DatabaseHeadroom must be in (0,1], got %v", b.DatabaseHeadroom)
	}
	return &Guard{budget: b, now: now}, nil
}

// Budget returns the ceilings in force (a copy).
func (g *Guard) Budget() Budget { return g.budget }

// Measure reads current usage. It is separate from Admit so that an operator
// endpoint can report headroom without pretending to perform an action.
//
// The queries run on the caller's querier, which in Admit's case is the
// transaction the financial action is already in. That matters: a purchase
// admitted against a count read outside its own transaction is admitted
// against a number that was true a moment ago, and "a moment ago" is where
// concurrent overshoot lives.
func (g *Guard) Measure(ctx context.Context, q db.Querier) (Reading, error) {
	if q == nil {
		return Reading{}, fmt.Errorf("%w: no querier", ErrUnmeasurable)
	}
	r := Reading{MeasuredAt: g.now().UTC()}

	if g.budget.MaxAccounts > 0 {
		if err := q.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&r.Accounts); err != nil {
			return r, fmt.Errorf("%w: accounts: %v", ErrUnmeasurable, err)
		}
	}

	if g.budget.MaxPurchasesPerDay > 0 {
		// A rolling 24 hours rather than a calendar day. A calendar day resets
		// at an hour nobody chose and lets twice the ceiling through across a
		// midnight boundary.
		if err := q.QueryRow(ctx,
			`SELECT count(*) FROM credit_fundings WHERE created_at > now() - interval '24 hours'`,
		).Scan(&r.PurchasesToday); err != nil {
			return r, fmt.Errorf("%w: purchases today: %v", ErrUnmeasurable, err)
		}
	}

	if g.budget.MaxAtRiskMinor > 0 {
		// Money at risk is every funding that has not reached a terminal
		// state. AUTHORIZATION_PENDING is money promised and not yet captured;
		// REVERSIBLE and DISPUTED are money captured with Credit already
		// issued against it that could still be taken back; MANUAL_REVIEW is
		// money nobody has decided about. SETTLED, CANCELED, REVERSED and
		// REFUNDED are all decided, and carry no further exposure.
		if err := q.QueryRow(ctx,
			`SELECT coalesce(sum(paid_amount_minor), 0)::bigint
			   FROM credit_fundings
			  WHERE state IN ('AUTHORIZATION_PENDING','REVERSIBLE','DISPUTED','MANUAL_REVIEW')`,
		).Scan(&r.AtRiskMinor); err != nil {
			return r, fmt.Errorf("%w: money at risk: %v", ErrUnmeasurable, err)
		}
	}

	if g.budget.MaxDatabaseBytes > 0 {
		if err := q.QueryRow(ctx,
			`SELECT pg_database_size(current_database())::bigint`,
		).Scan(&r.DatabaseBytes); err != nil {
			return r, fmt.Errorf("%w: database size: %v", ErrUnmeasurable, err)
		}
	}

	return r, nil
}

// Admit decides whether one action may proceed.
//
// Every refusal names the number that caused it, because "at capacity" without
// a number is an outage report rather than a decision. Every measurement
// failure is also a refusal: see ErrUnmeasurable.
func (g *Guard) Admit(ctx context.Context, q db.Querier, a Action) (Reading, error) {
	r, err := g.Measure(ctx, q)
	if err != nil {
		return r, errs.Wrap(err, errs.CodeAtCapacity,
			"the launch-tier capacity guard could not measure usage, so the action is refused")
	}

	// The infrastructure ceiling applies to every action, because running out
	// of database is not specific to what was being written when it happened.
	if g.budget.MaxDatabaseBytes > 0 {
		limit := int64(float64(g.budget.MaxDatabaseBytes) * g.budget.DatabaseHeadroom)
		if r.DatabaseBytes >= limit {
			return r, g.refuse(a, "database storage",
				fmt.Sprintf("%d of %d bytes used, and new financial actions stop at %d to leave room for reconciliation and export",
					r.DatabaseBytes, g.budget.MaxDatabaseBytes, limit))
		}
	}

	switch a {
	case ActionOpenAccount:
		if g.budget.MaxAccounts > 0 && r.Accounts >= g.budget.MaxAccounts {
			return r, g.refuse(a, "launch cohort",
				fmt.Sprintf("%d of %d accounts", r.Accounts, g.budget.MaxAccounts))
		}
	case ActionCreditPurchase:
		if g.budget.MaxPurchasesPerDay > 0 && r.PurchasesToday >= g.budget.MaxPurchasesPerDay {
			return r, g.refuse(a, "daily purchase volume",
				fmt.Sprintf("%d of %d in the last 24 hours", r.PurchasesToday, g.budget.MaxPurchasesPerDay))
		}
		if g.budget.MaxAtRiskMinor > 0 && r.AtRiskMinor >= g.budget.MaxAtRiskMinor {
			return r, g.refuse(a, "money at risk",
				fmt.Sprintf("%d of %d minor units in a non-terminal funding state", r.AtRiskMinor, g.budget.MaxAtRiskMinor))
		}
	default:
		// An unknown action is refused rather than admitted. A new financial
		// action that forgot to declare its ceilings must not inherit "no
		// ceilings apply".
		return r, errs.Newf(errs.CodeUnsupported,
			"capacity: %q declares no ceilings; add it to Admit before using it", string(a))
	}

	return r, nil
}

// AdmitAmount is Admit plus the effect of the amount about to be taken.
//
// Checking the ceiling against current exposure alone lets a single purchase
// vault straight over it: at 199,000 of 200,000 minor units, a further 50,000
// is admitted and the tier ends up 49,000 beyond the cap it was given. The
// ceiling is about what the deployment could owe after the action, not before.
func (g *Guard) AdmitAmount(ctx context.Context, q db.Querier, a Action, amountMinor int64) (Reading, error) {
	if amountMinor < 0 {
		return Reading{}, errs.New(errs.CodeValidationFailed, "capacity: a negative amount")
	}
	r, err := g.Admit(ctx, q, a)
	if err != nil {
		return r, err
	}
	if g.budget.MaxAtRiskMinor > 0 && a == ActionCreditPurchase {
		after := r.AtRiskMinor + amountMinor
		if after > g.budget.MaxAtRiskMinor {
			return r, g.refuse(a, "money at risk",
				fmt.Sprintf("%d minor units at risk now and this would make it %d, past the ceiling of %d",
					r.AtRiskMinor, after, g.budget.MaxAtRiskMinor))
		}
	}
	return r, nil
}

func (g *Guard) refuse(a Action, ceiling, detail string) error {
	return errs.Wrap(
		fmt.Errorf("%w: %s", ErrAtCapacity, ceiling),
		errs.CodeAtCapacity,
		fmt.Sprintf("%s is refused: the launch tier's %s ceiling is reached (%s)", string(a), ceiling, detail),
	).WithField("ceiling", ceiling).WithField("action", string(a))
}
