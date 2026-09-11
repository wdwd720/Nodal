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

	// MaxAtRiskMinor is the most money that may be undecided at once, in minor
	// units -- see atRiskFundingStates for exactly which funding states that
	// means. This is the cap that matters most, because it bounds what a
	// failure can cost rather than what it can consume.
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

// atRiskFundingStates are the credit_fundings states whose money is not yet
// decided, and so the states the money-at-risk ceiling sums over. It is every
// state in internal/credit's funding machine that is neither terminal nor
// SETTLED:
//
//   - CREATED, AUTHORIZATION_PENDING, AUTHORIZED and CAPTURE_PENDING are money
//     promised. No Credit exists yet, but the provider can complete any of
//     them without asking this system first.
//   - CAPTURED is money already taken from the payer, one transition away from
//     minting Credit against it. It is the single largest exposure in the list.
//   - REVERSIBLE and DISPUTED are money taken with Credit already spendable
//     against it, which the funder can still take back.
//   - MANUAL_REVIEW is money nobody has decided about yet.
//
// SETTLED is deliberately left out even though the machine can still move it
// to DISPUTED or REFUNDED. Settlement is the point at which this system stops
// treating money as reversible; it is what makes value payout-eligible.
// Counting it would turn the ceiling into a lifetime cumulative cap that can
// only ever rise, so the tier would end up refusing every purchase forever --
// an outage, not a ceiling. REVERSED, REFUNDED, FAILED and CANCELED are
// terminal: the money went back, or was never taken.
//
// This list named four states and omitted CAPTURED. The ceiling therefore
// measured less exposure than existed and admitted purchases it should have
// refused, in the one direction that costs money. It was found by running the
// real query against the real schema; the four-state version parses fine and
// simply returns a smaller number, so nothing short of that could catch it.
// internal/credit's TestFundingStates_TheCapacityCeilingClassifiesEveryState
// now fails if a state is added to the machine and not classified here.
var atRiskFundingStates = []string{
	"CREATED",
	"AUTHORIZATION_PENDING",
	"AUTHORIZED",
	"CAPTURE_PENDING",
	"CAPTURED",
	"REVERSIBLE",
	"DISPUTED",
	"MANUAL_REVIEW",
}

// AtRiskFundingStates returns the funding states the money-at-risk ceiling
// counts (a copy). It is exported so the package that owns the funding state
// machine can check this classification still covers it.
func AtRiskFundingStates() []string {
	return append([]string(nil), atRiskFundingStates...)
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
		// The states are atRiskFundingStates, not literals written here: the
		// set they have to agree with lives in internal/credit, and a list
		// spelled out at the point of use is a list that drifts from it.
		if err := q.QueryRow(ctx,
			`SELECT coalesce(sum(paid_amount_minor), 0)::bigint
			   FROM credit_fundings
			  WHERE state = ANY($1)`,
			atRiskFundingStates,
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

// lockKeys serialise the measure-then-act window, one key per action so that
// opening an account does not queue behind buying Credits.
//
// The numbers are arbitrary and permanent: an advisory lock key means nothing
// except "the same number is the same lock", and changing one would silently
// stop coordinating with a deployment still running the old value.
var lockKeys = map[Action]int64{
	ActionOpenAccount:    7_010_001,
	ActionCreditPurchase: 7_010_002,
}

// Admit decides whether one action may proceed.
//
// Every refusal names the number that caused it, because "at capacity" without
// a number is an outage report rather than a decision. Every measurement
// failure is also a refusal: see ErrUnmeasurable.
//
// # Why it takes a lock
//
// A ceiling that is read and then acted on is not a ceiling unless the read and
// the act are one step. internal/credit's comment used to claim they were --
// "The guard reads inside this transaction, so two concurrent purchases cannot
// both be admitted against the same headroom" -- and under READ COMMITTED that
// is false: a concurrent uncommitted INSERT is invisible to sum(), so N
// transactions each measure the same headroom and all N are admitted. With the
// shipped numbers that is eight simultaneous $2,000 purchases against a $2,000
// ceiling (F-96).
//
// pg_advisory_xact_lock is the smallest thing that makes the window atomic
// without raising the isolation level of a transaction that also writes a
// funding row. It is transaction-scoped, so it is released by the commit or
// rollback that ends the caller's transaction and cannot be leaked.
//
// It is only sound while that transaction is short. It became short in the same
// change: the provider call moved out of it, because holding this lock across a
// 20-second network call would queue every other purchase behind one HTTP
// request and hold one of eight pool connections while doing it.
func (g *Guard) Admit(ctx context.Context, q db.Querier, a Action) (Reading, error) {
	if q == nil {
		// Measure says the same thing a line later; the lock has to be told
		// first because it is now the first thing that touches the database.
		return Reading{}, errs.Wrap(fmt.Errorf("%w: nil querier", ErrUnmeasurable), errs.CodeAtCapacity,
			"the launch-tier capacity guard could not measure usage, so the action is refused")
	}
	if key, ok := lockKeys[a]; ok {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
			return Reading{}, errs.Wrap(fmt.Errorf("%w: %v", ErrUnmeasurable, err), errs.CodeAtCapacity,
				"the launch-tier capacity guard could not take its lock, so the action is refused")
		}
	}
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
//
// # Why the comparison is a subtraction
//
// It used to be `after := r.AtRiskMinor + amountMinor` compared against the
// ceiling. That sum is unchecked int64: an amount near math.MaxInt64 wraps
// negative, the comparison passes, and the guard whose whole job is to answer
// "what could this deployment owe after the action" answers with a number
// below zero (F-159).
//
// It was inert only because internal/credit happened to call this BEFORE
// pricing, and PricingPolicy.CreditsFor then refused the amount for exceeding
// MaxAmountMinor -- so the order of two calls was the only thing between that
// arithmetic and a ceiling that could be stepped over. Comparing headroom
// instead cannot overflow for any non-negative amount, because both sides of
// `amountMinor > MaxAtRiskMinor - AtRiskMinor` are already bounded by values
// this guard measured. The API bounds amount_minor before the guard ever sees
// it as well; a guard that depends on its caller having done that is not a
// guard.
func (g *Guard) AdmitAmount(ctx context.Context, q db.Querier, a Action, amountMinor int64) (Reading, error) {
	if amountMinor < 0 {
		return Reading{}, errs.New(errs.CodeValidationFailed, "capacity: a negative amount")
	}
	r, err := g.Admit(ctx, q, a)
	if err != nil {
		return r, err
	}
	if g.budget.MaxAtRiskMinor > 0 && a == ActionCreditPurchase {
		// Headroom, not a sum. A measured AtRiskMinor above the ceiling gives
		// a negative headroom, which refuses every positive amount -- which is
		// the right answer for a tier already past its cap.
		headroom := g.budget.MaxAtRiskMinor - r.AtRiskMinor
		if amountMinor > headroom {
			return r, g.refuse(a, "money at risk",
				fmt.Sprintf("%d minor units at risk now leaves %d of headroom under the ceiling of %d, and this asks for %d",
					r.AtRiskMinor, headroom, g.budget.MaxAtRiskMinor, amountMinor))
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
