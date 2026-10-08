package sandboxrt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
// per-call bounds or the account's remaining window -- counting the runs other
// transactions hold reservations for -- and a charge that waits longer than
// the budget allows is refused too; every refusal is RATE_LIMITED with a
// stable reason (BudgetCallRuns, ...). How a run is reserved and charged with
// no lock held while it runs is the package documentation's "Budget".
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
	// BudgetContended: the account's budget could not be charged now --
	// another charge held it, or no budget connection was free, for longer
	// than the budget waits -- or a run's charge no longer fitted once the
	// run had ended. Retry after the Retry-After.
	BudgetContended = "SANDBOX_BUDGET_CONTENDED"
)

// AllBudgetReasons returns every refusal reason.
func AllBudgetReasons() []string {
	return []string{BudgetCallRuns, BudgetCallCPU, BudgetWindowRuns, BudgetWindowCPU, BudgetWindowWall, BudgetContended}
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
	for name, v := range map[string]int64{
		"max_cpu_ms_per_call": b.MaxCPUMSPerCall, "max_cpu_ms_per_window": b.MaxCPUMSPerWindow,
		"max_wall_ms_per_window": b.MaxWallMSPerWindow,
	} {
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
	// InFlightRuns and InFlightCPUMS are the runs reserved and not yet
	// charged -- by runs in progress, or for a call admitted whole -- and the
	// CPU they were granted. They count against the window like charges.
	InFlightRuns  int
	InFlightCPUMS int64
}

// --- the budget's own connections -------------------------------------------

// BudgetConns is where the sandbox budget's short transactions run: a pool of
// its own (OpenBudgetPool), never the one the callers' transactions come
// from. A run is started inside its caller's transaction, and reaching into
// that pool for a second connection while holding the first is F-27: with
// every connection held by a caller waiting for another, none is ever
// released. *pgxpool.Pool is one.
type BudgetConns interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// The budget pool's size. A connection is held for one short transaction --
// an admission, a reservation, a charge -- and never while code runs, so a few
// serve many concurrent runs.
const (
	DefaultBudgetConns = 4
	MaxBudgetConns     = 4
)

// OpenBudgetPool opens the sandbox budget's own pool on the database d
// connects to, with d's connection settings -- credentials, TLS, the session's
// statement and lock timeouts, tracing -- and connections of its own: at most
// conns (1..MaxBudgetConns), none kept open while idle for more than a minute.
// The caller closes it when the process stops.
func OpenBudgetPool(ctx context.Context, d *db.DB, conns int32) (*pgxpool.Pool, error) {
	if d == nil {
		return nil, errs.New(errs.CodeInternal, "sandboxrt: the budget pool opens on a database")
	}
	if conns < 1 || conns > MaxBudgetConns {
		return nil, errs.Newf(errs.CodeValidationFailed, "sandboxrt: the budget pool holds 1..%d connections", MaxBudgetConns)
	}
	cfg := d.Pool().Config()
	cfg.MaxConns, cfg.MinConns, cfg.MinIdleConns = conns, 0, 0
	cfg.MaxConnIdleTime = time.Minute
	if params := cfg.ConnConfig.RuntimeParams; params != nil {
		name := params["application_name"] + "-sandbox-budget"
		if len(name) > 63 {
			name = name[len(name)-63:]
		}
		params["application_name"] = name
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "sandboxrt: open the budget pool")
	}
	return pool, nil
}

// How long the budget waits. An admission or a reservation waits budgetWait
// for a connection and for the account's budget lock, then refuses
// (BudgetContended). A finished run's charge waits finalizeWait: the code has
// already run, and its record is worth the wait.
const (
	budgetWait   = 5 * time.Second
	finalizeWait = 30 * time.Second
)

// budgetLockKey names the advisory lock that serializes the charges of one
// account; the SX003 trigger takes the same lock.
func budgetLockKey(account accounts.AccountID) string { return "sandbox-budget:" + account.String() }

// lockBudget takes the account's budget lock until the budget transaction btx
// ends. It is only ever taken in a budget transaction, which is short: never
// in a caller's, never while code runs.
func lockBudget(ctx context.Context, btx pgx.Tx, account accounts.AccountID) error {
	if _, err := btx.Exec(ctx, `SELECT pg_advisory_xact_lock((('x' || substr(md5($1), 1, 16))::bit(64))::bigint)`,
		budgetLockKey(account)); err != nil {
		return dbErr("lock sandbox budget", err)
	}
	return nil
}

// budgetTx runs fn in a short transaction of the budget's own, on a budget
// connection: it waits at most wait for the connection and, inside fn, for any
// lock. A wait that runs out is BudgetContended; a caller that went away gets
// its context's error. A deadlock is retried, twice.
func (s *Service) budgetTx(ctx context.Context, wait time.Duration, fn func(ctx context.Context, btx pgx.Tx) error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = s.budgetTxOnce(ctx, wait, fn); !db.IsDeadlock(err) {
			return err
		}
	}
	return err
}

func (s *Service) budgetTxOnce(ctx context.Context, wait time.Duration, fn func(ctx context.Context, btx pgx.Tx) error) error {
	actx, cancel := context.WithTimeout(ctx, wait)
	conn, err := s.conns.Acquire(actx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return contended(fmt.Sprintf("no budget connection was free within %s", wait))
	}
	defer conn.Release()
	btx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return dbErr("begin a budget transaction", err)
	}
	done := false
	defer func() {
		if !done {
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer rcancel()
			_ = btx.Rollback(rctx) // a rollback that fails discards the connection
		}
	}()
	if _, err := btx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, strconv.FormatInt(wait.Milliseconds(), 10)); err != nil {
		return dbErr("bound the budget's lock wait", err)
	}
	if err := fn(ctx, btx); err != nil {
		return classifyBudget(err, wait)
	}
	if err := btx.Commit(ctx); err != nil {
		return classifyBudget(dbErr("commit the budget", err), wait)
	}
	done = true
	return nil
}

// classifyBudget turns a lock wait that ran out into BudgetContended: a
// contended charge is a refusal to retry, never an internal error.
func classifyBudget(err error, wait time.Duration) error {
	if db.IsLockTimeout(err) {
		return contended(fmt.Sprintf("the account's budget was held by another charge for longer than %s", wait))
	}
	return err
}

func contended(detail string) error {
	retry := time.Second
	return budgetRefusal(BudgetContended, detail, &retry)
}

// --- reservations ---------------------------------------------------------------

// A reservation is a pair of SHARED transaction-level advisory locks held by
// the caller's transaction -- the one the code runs in -- and nothing else:
//
//   - the hold lock, holdKey(account), names the account;
//   - the run lock, (holdClass + granted CPU ms, tag), says how much CPU the
//     run was granted and which run it is: the tag is the low 32 bits of the
//     run's id.
//
// A check of the account's window reads them from pg_locks, under the
// account's budget lock, and counts each run lock whose holder also holds the
// account's hold lock -- unless a charge in the account's window carries its
// tag, for then the run is charged and its charge counts instead. A
// reservation therefore counts from the moment it is taken until its run is
// charged, and no longer; and it lives exactly as long as the caller's
// transaction, which PostgreSQL ends at commit, at rollback and when the
// connection goes away: nothing can outlive the transaction that took it, and
// no migration was needed, because a reservation is never a row.
const (
	holdDomain = "nodal/sandboxrt/budget-hold/v1\x00"
	// holdClass is the first key of every run lock: holdClass plus the CPU
	// the run was granted (at most maxWallTimeMS), recognisable among the
	// two-key advisory locks, which nothing else in the platform takes.
	holdClass int64 = 0x5342_0000
)

// holdKey is the account's hold lock.
func holdKey(account accounts.AccountID) int64 {
	sum := sha256.Sum256([]byte(holdDomain + account.String()))
	return int64(binary.BigEndian.Uint64(sum[:8])) // #nosec G115 -- a lock key: any 64 bits will do
}

// runTag is a run's tag: the low 32 bits of its id, which its charge carries
// as run_id.
func runTag(run RunID) uint32 {
	b := run.Bytes()
	return binary.BigEndian.Uint32(b[12:])
}

// runIDWithTag is a fresh run id whose low 32 bits are tag: a version 7 UUID
// keeps those bits random, so the id is still unique and well-formed.
func runIDWithTag(tag uint32) (RunID, error) {
	b := NewRunID().Bytes()
	binary.BigEndian.PutUint32(b[12:], tag)
	r, err := id.Bytes16[runKind](b)
	if err != nil {
		return RunID{}, errs.Wrap(err, errs.CodeInternal, "sandboxrt: a run id carrying a reserved tag")
	}
	return r, nil
}

// heldReservationsSQL selects an account's reservations not yet charged: $1
// the hold key, $2 holdClass, $3 the largest grant, $4 the account, $5 the
// window's start, $6 the backend whose own to read (0: every backend's).
const heldReservationsSQL = `WITH l AS MATERIALIZED (
	SELECT pid, classid::bigint AS c, objid::bigint AS o, objsubid FROM pg_locks
	 WHERE locktype = 'advisory' AND granted
	   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
	   AND ($6::int = 0 OR pid = $6::int)),
held AS (
	SELECT DISTINCT b.pid, b.c - $2::bigint AS cpu_ms, b.o AS tag
	  FROM l a JOIN l b ON b.pid = a.pid AND b.objsubid = 2 AND b.c BETWEEN $2::bigint AND $2::bigint + $3::bigint
	 WHERE a.objsubid = 1 AND ((a.c << 32) | a.o) = $1::bigint)
SELECT h.cpu_ms, h.tag FROM held h
 WHERE NOT EXISTS (SELECT 1 FROM sandbox_usage u WHERE u.account_id = $4 AND u.charged_at > $5
                    AND ('x' || right(u.run_id::text, 8))::bit(32)::bigint = h.tag)
 ORDER BY h.tag`

type reservation struct {
	cpuMS int64
	tag   uint32
}

// heldReservations reads an account's reservations not yet charged, through
// q: every transaction's (pid 0), or only those of the backend pid.
func heldReservations(ctx context.Context, q db.Querier, account accounts.AccountID, windowStart time.Time, pid int32) ([]reservation, error) {
	rows, err := q.Query(ctx, heldReservationsSQL, holdKey(account), holdClass, int64(maxWallTimeMS), account, windowStart, pid)
	if err != nil {
		return nil, dbErr("read sandbox reservations", err)
	}
	defer rows.Close()
	var out []reservation
	for rows.Next() {
		var r reservation
		var tag int64
		if err := rows.Scan(&r.cpuMS, &tag); err != nil {
			return nil, dbErr("read sandbox reservations", err)
		}
		r.tag = uint32(tag) // #nosec G115 -- an oid: 0..2^32-1
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("read sandbox reservations", err)
	}
	return out, nil
}

// reserveIn takes reservations of runs granted cpu each, one per tag, in the
// caller's transaction tx and inside a savepoint: a failure leaves tx usable
// and holding none of them.
func reserveIn(ctx context.Context, tx pgx.Tx, account accounts.AccountID, cpu int64, tags []uint32) error {
	if cpu < minWallTimeMS || cpu > maxWallTimeMS {
		return errs.New(errs.CodeInternal, "sandboxrt: a reservation is granted a run's wall-time limit")
	}
	keys := make([]int32, len(tags))
	for i, t := range tags {
		keys[i] = int32(t) // #nosec G115 -- the lock's second key is the tag's 32 bits
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return dbErr("open a reservation savepoint", err)
	}
	fail := func(op string, err error) error {
		_ = sp.Rollback(context.WithoutCancel(ctx)) // releases whatever the savepoint took
		return dbErr(op, err)
	}
	if _, err := sp.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1::bigint)`, holdKey(account)); err != nil {
		return fail("hold the account's budget", err)
	}
	if _, err := sp.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1::int4, t) FROM unnest($2::int4[]) AS t`,
		int32(holdClass+cpu), keys); err != nil { // #nosec G115 -- holdClass plus at most maxWallTimeMS fits an int4
		return fail("reserve sandbox runs", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return fail("keep the reservation", err)
	}
	return nil
}

// backendPID is the server process of the caller's transaction tx: the
// holder of every reservation tx takes.
func backendPID(ctx context.Context, tx pgx.Tx) (int32, error) {
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return 0, dbErr("read the caller's backend", err)
	}
	return pid, nil
}

// claim reads, in the budget transaction btx, whether the caller's
// transaction -- the one on backend pid -- holds a reservation of account
// granted cpu that it has not used (one Admit took for the call, or one a run
// of the caller's left uncharged), and returns a run id that takes it up. It
// reads on the budget's connection, so what counts as charged is what is
// committed, whatever the caller's isolation level.
func claim(ctx context.Context, btx pgx.Tx, pid int32, account accounts.AccountID, cpu int64, windowStart time.Time) (RunID, bool, error) {
	held, err := heldReservations(ctx, btx, account, windowStart, pid)
	if err != nil {
		return RunID{}, false, err
	}
	for _, r := range held {
		if r.cpuMS == cpu {
			run, err := runIDWithTag(r.tag)
			return run, err == nil, err
		}
	}
	return RunID{}, false, nil
}

// distinctTags draws n distinct random tags.
func distinctTags(n int) ([]uint32, error) {
	seen := make(map[uint32]bool, n)
	out := make([]uint32, 0, n)
	var b [4]byte
	for len(out) < n {
		if _, err := rand.Read(b[:]); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "sandboxrt: draw reservation tags")
		}
		if t := binary.BigEndian.Uint32(b[:]); !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}

// --- usage and admission -------------------------------------------------------

// UsageOf returns an account's consumption in the window ending at now: its
// charges, and the runs reserved and not yet charged. It takes no lock, so a
// charge committing while it reads may show as neither, or as both, for that
// instant.
func (s *Service) UsageOf(ctx context.Context, q db.Querier, account accounts.AccountID) (BudgetUsage, error) {
	return usageWithReservations(ctx, q, account, s.clk.Now().UTC(), s.budget.Window)
}

func usageWithReservations(ctx context.Context, q db.Querier, account accounts.AccountID, now time.Time, window time.Duration) (BudgetUsage, error) {
	u, err := usageAt(ctx, q, account, now, window)
	if err != nil {
		return BudgetUsage{}, err
	}
	held, err := heldReservations(ctx, q, account, now.Add(-window), 0)
	if err != nil {
		return BudgetUsage{}, err
	}
	for _, r := range held {
		u.InFlightRuns++
		u.InFlightCPUMS += r.cpuMS
	}
	return u, nil
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

// Admit admits a call of runs runs under limits whole, or refuses it before
// anything runs: a call with more runs or more granted CPU than a call may
// have, or than the account's window has left -- its charges and every
// reservation not yet charged counted. An admitted call is reserved in the
// caller's transaction tx for as long as tx lasts: the call's runs (Execute,
// in tx) take up those reservations, so no other caller can take the room the
// call was admitted with, and whatever the call does not use is released with
// tx. The check and the reservation are one short budget transaction; tx
// holds nothing but the reservation, and never the budget's lock.
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
	tags, err := distinctTags(runs)
	if err != nil {
		return err
	}
	return s.budgetTx(ctx, s.wait, func(ctx context.Context, btx pgx.Tx) error {
		if err := lockBudget(ctx, btx, account); err != nil {
			return err
		}
		if err := s.fitsWindow(ctx, btx, account, runs, cpu); err != nil {
			return err
		}
		return reserveIn(ctx, tx, account, limits.WallTimeMS, tags)
	})
}

// fitsWindow refuses runs more runs and cpu more granted CPU than the
// account's window has left: its charges, and every reservation not yet
// charged at the CPU it was granted -- which also bounds the wall time it will
// take. The caller holds the account's budget lock in btx.
func (s *Service) fitsWindow(ctx context.Context, btx pgx.Tx, account accounts.AccountID, runs int, cpu int64) error {
	b := s.budget
	u, err := usageWithReservations(ctx, btx, account, s.clk.Now().UTC(), b.Window)
	if err != nil {
		return err
	}
	var retry *time.Duration
	switch {
	case !u.Oldest.IsZero():
		d := u.Oldest.Add(b.Window).Sub(u.Until)
		if d < time.Second {
			d = time.Second
		}
		retry = &d
	case u.InFlightRuns > 0:
		d := time.Second
		retry = &d
	}
	switch {
	case u.Runs+u.InFlightRuns+runs > b.MaxRunsPerWindow:
		return budgetRefusal(BudgetWindowRuns, fmt.Sprintf("the account has started %d and reserved %d of its %d sandbox runs this window",
			u.Runs, u.InFlightRuns, b.MaxRunsPerWindow), retry)
	case u.CPUMS+u.InFlightCPUMS+cpu > b.MaxCPUMSPerWindow:
		return budgetRefusal(BudgetWindowCPU, fmt.Sprintf("the account has been granted %d and reserved %d of its %d ms of sandbox CPU this window",
			u.CPUMS, u.InFlightCPUMS, b.MaxCPUMSPerWindow), retry)
	case u.WallMS+u.InFlightCPUMS >= b.MaxWallMSPerWindow:
		return budgetRefusal(BudgetWindowWall, fmt.Sprintf("the account's sandbox runs took %d, and its reserved runs may take %d, of its %d ms of wall time this window",
			u.WallMS, u.InFlightCPUMS, b.MaxWallMSPerWindow), retry)
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
// that has none (SX002). Its run_id carries the run's tag, so the commit that
// makes the charge count is the one that retires the run's reservation.
func (s *Service) charge(ctx context.Context, btx pgx.Tx, req ExecuteRequest, e Execution) error {
	cpu := req.Limits.WallTimeMS
	if e.Result.Termination == TerminationKilledBySwitch {
		cpu = 0
	}
	b := s.budget
	if _, err := btx.Exec(ctx, `INSERT INTO sandbox_usage (id, run_id, account_id, purpose, cpu_ms, wall_ms, charged_at, budget_version,
		window_ms, max_runs, max_cpu_ms, max_wall_ms) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		id.New[id.Any](), e.RunID, req.AccountID, req.Purpose, cpu, e.Result.Usage.WallTime.Milliseconds(), e.StartedAt, b.Version,
		b.Window.Milliseconds(), b.MaxRunsPerWindow, b.MaxCPUMSPerWindow, b.MaxWallMSPerWindow); err != nil {
		return dbErr("charge sandbox run", err)
	}
	return nil
}
