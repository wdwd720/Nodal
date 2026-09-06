package killswitch

import (
	"context"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// Checker is the authoritative kill-switch check. It holds no state beyond
// the Policy and is safe for concurrent use.
type Checker struct {
	policy Policy
}

// NewChecker builds a Checker with the given policy.
func NewChecker(p Policy) *Checker { return &Checker{policy: p} }

// Policy returns the checker's policy.
func (c *Checker) Policy() Policy { return c.policy }

// Check returns nil when no active switch blocks the action, otherwise
// KILL_SWITCH_ACTIVE with fields "switch" and "scope". Pass the Querier of
// the transaction that authorizes the action (PART 52: checks read Postgres
// inside the authorizing transaction). Never-blocked classes return nil
// without a query, so reconciliation, settlement, ledger posting,
// cancellation and observation are not even slowed down by this package.
func (c *Checker) Check(ctx context.Context, q db.Querier, a Action) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.Class.NeverBlocked() {
		return nil
	}
	active, err := listActive(ctx, q)
	if err != nil {
		return err
	}
	if sw, blocked := Blocking(active, a, c.policy); blocked {
		return BlockedError(sw, a)
	}
	return nil
}

// Snapshot returns every active switch, sorted, for the risk kernel.
func (c *Checker) Snapshot(ctx context.Context, q db.Querier) (Snapshot, error) {
	active, err := listActive(ctx, q)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Switches: active}, nil
}

// MaxCacheTTL bounds CachedChecker: an in-process cache may serve pre-checks
// for at most one second (POLICY_AUTHORITY §2).
const MaxCacheTTL = time.Second

// CachedChecker serves PRE-CHECKS only, from an in-process copy of the active
// switches refreshed at most every ttl (≤ MaxCacheTTL). It exists so hot
// paths can reject early without a round-trip. It is never authoritative:
// the operation's authorizing transaction must still call Checker.Check
// with its own tx Querier, which sees every activation committed before it.
type CachedChecker struct {
	inner *Checker
	q     db.Querier
	clk   clock.Clock
	ttl   time.Duration

	mu        sync.Mutex
	active    []Switch
	fetchedAt time.Time
	primed    bool
}

// NewCachedChecker builds a CachedChecker over q (typically the pool). ttl
// must be in (0, MaxCacheTTL].
func NewCachedChecker(inner *Checker, q db.Querier, clk clock.Clock, ttl time.Duration) (*CachedChecker, error) {
	if inner == nil || q == nil || clk == nil {
		return nil, errs.New(errs.CodeValidationFailed, "checker, querier and clock are required")
	}
	if ttl <= 0 || ttl > MaxCacheTTL {
		return nil, errs.Newf(errs.CodeValidationFailed, "cache ttl must be in (0, %s]", MaxCacheTTL).WithField("ttl", ttl.String())
	}
	return &CachedChecker{inner: inner, q: q, clk: clk, ttl: ttl}, nil
}

// PreCheck is Check against the cached snapshot (refreshed when older than
// ttl). A refresh failure is returned as an error: a pre-check must not
// pass on stale data it could not refresh.
func (c *CachedChecker) PreCheck(ctx context.Context, a Action) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.Class.NeverBlocked() {
		return nil
	}
	active, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	if sw, blocked := Blocking(active, a, c.inner.policy); blocked {
		return BlockedError(sw, a)
	}
	return nil
}

// Snapshot returns the cached active switches (refreshed if stale).
func (c *CachedChecker) Snapshot(ctx context.Context) (Snapshot, error) {
	active, err := c.snapshot(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Switches: active}, nil
}

// Invalidate drops the cached copy so the next call refreshes. Call it after
// Controller.Activate in the same process to shorten the window further.
func (c *CachedChecker) Invalidate() {
	c.mu.Lock()
	c.primed = false
	c.active = nil
	c.mu.Unlock()
}

func (c *CachedChecker) snapshot(ctx context.Context) ([]Switch, error) {
	now := c.clk.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.primed && now.Sub(c.fetchedAt) < c.ttl && !now.Before(c.fetchedAt) {
		return append([]Switch(nil), c.active...), nil
	}
	active, err := listActive(ctx, c.q)
	if err != nil {
		return nil, err
	}
	c.active = active
	c.fetchedAt = now
	c.primed = true
	return append([]Switch(nil), active...), nil
}
