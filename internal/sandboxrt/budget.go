package sandboxrt

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// Budget is the overall sandbox budget (review-2 F13): what one call and one
// account in one window may consume, whatever the per-run Limits allow. Every
// run is charged to an account (sandbox_usage, SX002) and no charge takes an
// account beyond the budget the charge states (SX003):
//
//   - runs: how many runs started;
//   - CPU (fuel): the wall-time limit each started run was granted, charged
//     before it starts -- a run is single-threaded and closed at its deadline,
//     so this bounds the CPU it can use, deterministically;
//   - wall time: what the runs actually took.
//
// A call is refused before any run starts when it could not finish within the
// per-call bounds or the account's remaining window; the refusal is
// RATE_LIMITED with a stable reason (BudgetCallRuns, ...).
type Budget struct {
	// Version names this policy; every charge records it.
	Version string `json:"version"`
	// Window is the rolling window the per-account bounds apply to.
	Window time.Duration `json:"window"`
	// Per account per window.
	MaxRunsPerWindow   int   `json:"max_runs_per_window"`
	MaxCPUMSPerWindow  int64 `json:"max_cpu_ms_per_window"`
	MaxWallMSPerWindow int64 `json:"max_wall_ms_per_window"`
	// Per call (one simulation, one follow).
	MaxRunsPerCall  int   `json:"max_runs_per_call"`
	MaxCPUMSPerCall int64 `json:"max_cpu_ms_per_call"`
}

// Budget refusal reasons: the "reason" field of the RATE_LIMITED error.
const (
	BudgetCallRuns   = "SANDBOX_BUDGET_CALL_RUNS"
	BudgetCallCPU    = "SANDBOX_BUDGET_CALL_CPU"
	BudgetWindowRuns = "SANDBOX_BUDGET_WINDOW_RUNS"
	BudgetWindowCPU  = "SANDBOX_BUDGET_WINDOW_CPU"
	BudgetWindowWall = "SANDBOX_BUDGET_WINDOW_WALL"
)

// AllBudgetReasons returns every refusal reason.
func AllBudgetReasons() []string {
	return []string{BudgetCallRuns, BudgetCallCPU, BudgetWindowRuns, BudgetWindowCPU, BudgetWindowWall}
}

// DefaultBudget is the budget every Service uses unless it is given another:
// at most 20 runs and 40 s of granted CPU per call, and per account per hour
// 240 runs, 10 minutes of granted CPU and 5 minutes of actual wall time.
func DefaultBudget() Budget {
	return Budget{
		Version:            "sandbox-budget/v1",
		Window:             time.Hour,
		MaxRunsPerWindow:   240,
		MaxCPUMSPerWindow:  600_000,
		MaxWallMSPerWindow: 300_000,
		MaxRunsPerCall:     20,
		MaxCPUMSPerCall:    40_000,
	}
}

// Budget bounds: a Budget outside them is refused rather than clamped.
const (
	minBudgetWindow   = time.Minute
	maxBudgetWindow   = 7 * 24 * time.Hour
	maxRunsPerCall    = 100
	maxRunsPerWindow  = 100_000
	maxBudgetCPUMSCap = 7 * 24 * 3600 * 1000
)

// Validate refuses a budget with a missing version or any bound outside the
// policy bounds, or a per-call bound above the per-window one.
func (b Budget) Validate() error {
	var problems []string
	if v := strings.TrimSpace(b.Version); v == "" || v != b.Version || len(v) > 64 {
		problems = append(problems, "version must be 1..64 characters without surrounding whitespace")
	}
	if b.Window < minBudgetWindow || b.Window > maxBudgetWindow || b.Window%time.Millisecond != 0 {
		problems = append(problems, fmt.Sprintf("window must be a whole number of milliseconds in [%s, %s]", minBudgetWindow, maxBudgetWindow))
	}
	if b.MaxRunsPerCall < 1 || b.MaxRunsPerCall > maxRunsPerCall {
		problems = append(problems, fmt.Sprintf("max_runs_per_call must be in [1, %d]", maxRunsPerCall))
	}
	if b.MaxRunsPerWindow < 1 || b.MaxRunsPerWindow > maxRunsPerWindow {
		problems = append(problems, fmt.Sprintf("max_runs_per_window must be in [1, %d]", maxRunsPerWindow))
	}
	for name, v := range map[string]int64{"max_cpu_ms_per_call": b.MaxCPUMSPerCall, "max_cpu_ms_per_window": b.MaxCPUMSPerWindow,
		"max_wall_ms_per_window": b.MaxWallMSPerWindow} {
		if v < minWallTimeMS || v > maxBudgetCPUMSCap {
			problems = append(problems, fmt.Sprintf("%s must be in [%d, %d]", name, minWallTimeMS, int64(maxBudgetCPUMSCap)))
		}
	}
	if b.MaxRunsPerCall > b.MaxRunsPerWindow || b.MaxCPUMSPerCall > b.MaxCPUMSPerWindow {
		problems = append(problems, "a call may not be granted more than a whole window")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "sandboxrt: invalid sandbox budget").WithField("problems", problems)
	}
	return nil
}

// BudgetUsage is what an account consumed in the window ending at Until.
type BudgetUsage struct {
	Runs   int
	CPUMS  int64
	WallMS int64
	// Oldest is the earliest charge still in the window (zero when none).
	Oldest time.Time
	Until  time.Time
}

// budgetLockKey names the advisory lock that serializes the charges of one
// account; the SX003 trigger takes the same lock.
func budgetLockKey(account accounts.AccountID) string { return "sandbox-budget:" + account.String() }

// lockBudget serializes every charge of one account until the transaction
// ends, so two concurrent calls cannot both fit in the same remainder.
func lockBudget(ctx context.Context, tx pgx.Tx, account accounts.AccountID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock((('x' || substr(md5($1), 1, 16))::bit(64))::bigint)`,
		budgetLockKey(account)); err != nil {
		return dbErr("lock sandbox budget", err)
	}
	return nil
}

// UsageOf returns an account's consumption in the window ending at now.
func (s *Service) UsageOf(ctx context.Context, q db.Querier, account accounts.AccountID) (BudgetUsage, error) {
	now := s.clk.Now().UTC()
	return usageAt(ctx, q, account, now, s.budget.Window)
}

func usageAt(ctx context.Context, q db.Querier, account accounts.AccountID, now time.Time, window time.Duration) (BudgetUsage, error) {
	u := BudgetUsage{Until: now}
	var oldest *time.Time
	if err := q.QueryRow(ctx, `SELECT count(*), coalesce(sum(cpu_ms), 0), coalesce(sum(wall_ms), 0), min(charged_at)
		FROM sandbox_usage WHERE account_id = $1 AND charged_at > $2 AND charged_at <= $3`,
		account, now.Add(-window), now).Scan(&u.Runs, &u.CPUMS, &u.WallMS, &oldest); err != nil {
		return BudgetUsage{}, dbErr("read sandbox usage", err)
	}
	if oldest != nil {
		u.Oldest = oldest.UTC()
	}
	return u, nil
}

// Budget returns the service's budget.
func (s *Service) Budget() Budget { return s.budget }

// Admit refuses, before anything runs, a call of runs runs under limits that
// could not complete within the per-call bounds or within what the account has
// left of its window. It locks the account's budget for the rest of the
// transaction, so the runs that follow are charged against exactly what it
// saw. Execute charges each run; Admit lets a multi-run call fail whole
// instead of half way.
func (s *Service) Admit(ctx context.Context, tx pgx.Tx, account accounts.AccountID, runs int, limits Limits) error {
	if account.IsZero() {
		return errs.New(errs.CodeValidationFailed, "sandboxrt: a sandbox run is charged to an account")
	}
	if err := limits.Validate(); err != nil {
		return err
	}
	if runs < 1 {
		return errs.New(errs.CodeValidationFailed, "sandboxrt: a call runs at least once")
	}
	b := s.budget
	if runs > b.MaxRunsPerCall {
		return budgetRefusal(BudgetCallRuns, fmt.Sprintf("a call may start at most %d sandbox runs; this one asks for %d", b.MaxRunsPerCall, runs), nil)
	}
	cpu := int64(runs) * limits.WallTimeMS
	if cpu > b.MaxCPUMSPerCall {
		return budgetRefusal(BudgetCallCPU, fmt.Sprintf("a call may be granted at most %d ms of sandbox CPU; %d runs of %d ms ask for %d",
			b.MaxCPUMSPerCall, runs, limits.WallTimeMS, cpu), nil)
	}
	if err := lockBudget(ctx, tx, account); err != nil {
		return err
	}
	return s.fitsWindow(ctx, tx, account, runs, cpu)
}

// fitsWindow refuses runs more runs and cpu more granted CPU than the
// account's window has left. The caller holds the account's budget lock.
func (s *Service) fitsWindow(ctx context.Context, tx pgx.Tx, account accounts.AccountID, runs int, cpu int64) error {
	b := s.budget
	u, err := usageAt(ctx, tx, account, s.clk.Now().UTC(), b.Window)
	if err != nil {
		return err
	}
	var retry *time.Duration
	if !u.Oldest.IsZero() {
		d := u.Oldest.Add(b.Window).Sub(u.Until)
		if d < time.Second {
			d = time.Second
		}
		retry = &d
	}
	switch {
	case u.Runs+runs > b.MaxRunsPerWindow:
		return budgetRefusal(BudgetWindowRuns, fmt.Sprintf("the account has started %d of its %d sandbox runs this window", u.Runs, b.MaxRunsPerWindow), retry)
	case u.CPUMS+cpu > b.MaxCPUMSPerWindow:
		return budgetRefusal(BudgetWindowCPU, fmt.Sprintf("the account has been granted %d of its %d ms of sandbox CPU this window",
			u.CPUMS, b.MaxCPUMSPerWindow), retry)
	case u.WallMS >= b.MaxWallMSPerWindow:
		return budgetRefusal(BudgetWindowWall, fmt.Sprintf("the account's sandbox runs took %d of its %d ms of wall time this window",
			u.WallMS, b.MaxWallMSPerWindow), retry)
	}
	return nil
}

func budgetRefusal(reason, detail string, retry *time.Duration) error {
	e := errs.New(errs.CodeRateLimited, "sandboxrt: the sandbox budget refuses this: "+detail).WithField("reason", reason)
	if retry != nil {
		e.RetryAfter = retry
	}
	return e
}

// charge records one run against the account's budget, with the bounds it was
// admitted under; the database refuses a charge beyond them (SX003) and a run
// that has none (SX002).
func (s *Service) charge(ctx context.Context, tx pgx.Tx, req ExecuteRequest, e Execution) error {
	cpu := req.Limits.WallTimeMS
	if e.Result.Termination == TerminationKilledBySwitch {
		cpu = 0
	}
	b := s.budget
	if _, err := tx.Exec(ctx, `INSERT INTO sandbox_usage (id, run_id, account_id, purpose, cpu_ms, wall_ms, charged_at, budget_version,
		window_ms, max_runs, max_cpu_ms, max_wall_ms) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		id.New[id.Any](), e.RunID, req.AccountID, req.Purpose, cpu, e.Result.Usage.WallTime.Milliseconds(), e.StartedAt, b.Version,
		b.Window.Milliseconds(), b.MaxRunsPerWindow, b.MaxCPUMSPerWindow, b.MaxWallMSPerWindow); err != nil {
		return dbErr("charge sandbox run", err)
	}
	return nil
}
