package chain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
)

// MultiObserverOptions configures MultiObserver.
type MultiObserverOptions struct {
	// PerCallTimeout bounds each observer query (default 5 s).
	PerCallTimeout time.Duration
	// Policy defaults to DefaultPolicy.
	Policy AgreementPolicy
	// PrimaryTracker / SecondaryTracker receive one sample per query when
	// set. Give a tracker to either the adapter or the MultiObserver, not
	// both, or every call is sampled twice.
	PrimaryTracker, SecondaryTracker *provider.Tracker
	Clock                            clock.Clock
	Logger                           *slog.Logger
}

// MultiObserver queries a primary SolanaDataProvider-grade observer and an
// independent secondary observer, samples their health, and applies the
// AgreementPolicy. It is the only component allowed to say FINALIZED.
type MultiObserver struct {
	primary, secondary ChainObserver
	opts               MultiObserverOptions
}

// NewMultiObserver wires two observers. Both are required: a single-observer
// deployment must use the degraded path explicitly through ResolveSingle.
func NewMultiObserver(primary, secondary ChainObserver, opts MultiObserverOptions) (*MultiObserver, error) {
	if primary == nil || secondary == nil {
		return nil, errors.New("chain: both observers are required")
	}
	if primary.Name() == secondary.Name() {
		return nil, fmt.Errorf("chain: observers must be independent vendors, both are %q", primary.Name())
	}
	if opts.PerCallTimeout <= 0 {
		opts.PerCallTimeout = 5 * time.Second
	}
	if opts.Policy == (AgreementPolicy{}) {
		opts.Policy = DefaultPolicy()
	}
	if err := opts.Policy.Validate(); err != nil {
		return nil, err
	}
	if opts.Clock == nil {
		opts.Clock = clock.System()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &MultiObserver{primary: primary, secondary: secondary, opts: opts}, nil
}

// Policy returns the policy in force.
func (m *MultiObserver) Policy() AgreementPolicy { return m.opts.Policy }

// Primary returns the primary observer.
func (m *MultiObserver) Primary() ChainObserver { return m.primary }

// Secondary returns the secondary observer.
func (m *MultiObserver) Secondary() ChainObserver { return m.secondary }

// answer is one side's outcome.
type answer[T any] struct {
	value   T
	err     error
	skipped bool // observer DISABLED: not queried
	health  provider.Health
	latency time.Duration
}

func (a answer[T]) available() bool { return !a.skipped && a.err == nil }

// query runs fn against both observers concurrently with per-call timeouts,
// recording tracker samples. A DISABLED observer is not queried (PART 107:
// only an operator disable removes an observer from reads).
func query[T any](ctx context.Context, m *MultiObserver, fn func(context.Context, ChainObserver) (T, error)) (answer[T], answer[T]) {
	run := func(o ChainObserver, tr *provider.Tracker) answer[T] {
		h := o.Health()
		if !h.AllowsObservation() {
			return answer[T]{skipped: true, health: h}
		}
		cctx, cancel := context.WithTimeout(ctx, m.opts.PerCallTimeout)
		defer cancel()
		start := m.opts.Clock.Now()
		sw := clock.Start()
		v, err := fn(cctx, o)
		lat := sw.Elapsed()
		if tr != nil {
			tr.Observe(start.Add(lat), err == nil, lat)
		}
		if err != nil {
			if cctx.Err() != nil && !errs.HasCode(err, errs.CodeProviderUnavailable) {
				err = errs.Wrap(err, errs.CodeProviderUnavailable, "observer timed out")
			}
			m.opts.Logger.WarnContext(ctx, "chain observer query failed",
				slog.String("observer", o.Name()), slog.String("code", string(errs.CodeOf(err))), slog.Duration("latency", lat))
		}
		return answer[T]{value: v, err: err, health: h, latency: lat}
	}
	var pa, sa answer[T]
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pa = run(m.primary, m.opts.PrimaryTracker) }()
	go func() { defer wg.Done(); sa = run(m.secondary, m.opts.SecondaryTracker) }()
	wg.Wait()
	return pa, sa
}

func unavailableReason[T any](name string, a answer[T]) string {
	switch {
	case a.skipped:
		return name + " is DISABLED"
	case a.err != nil:
		return fmt.Sprintf("%s unavailable (%s)", name, errs.CodeOf(a.err))
	case a.health == provider.Unhealthy:
		return name + " is UNHEALTHY"
	}
	return name + " excluded"
}

// Observe queries both observers for sig and resolves them under the policy.
// It returns an error only when no observer could answer.
func (m *MultiObserver) Observe(ctx context.Context, sig string, required Finality) (Resolution, error) {
	if !required.Valid() {
		return Resolution{}, errs.Newf(errs.CodeValidationFailed, "unknown finality %q", required)
	}
	pa, sa := query(ctx, m, func(c context.Context, o ChainObserver) (TxObservation, error) {
		return o.GetTransaction(c, sig)
	})
	pol := m.opts.Policy
	pOK, sOK := pa.available(), sa.available()
	switch {
	case pOK && sOK && pa.health != provider.Unhealthy && sa.health != provider.Unhealthy:
		return pol.Resolve(pa.value, sa.value, required), nil
	case pOK && sOK:
		// One side is UNHEALTHY: its answer may lower confidence, never raise it.
		return m.resolveWithUnhealthy(pa, sa, required), nil
	case pOK:
		return pol.ResolveSingle(pa.value, SidePrimary, required, unavailableReason(m.secondary.Name(), sa)), nil
	case sOK:
		return pol.ResolveSingle(sa.value, SideSecondary, required, unavailableReason(m.primary.Name(), pa)), nil
	}
	return Resolution{}, errs.Wrap(errors.Join(pa.err, sa.err), errs.CodeProviderUnavailable,
		"no chain observer available: "+unavailableReason(m.primary.Name(), pa)+"; "+unavailableReason(m.secondary.Name(), sa))
}

// resolveWithUnhealthy handles the case where both answered but one is
// UNHEALTHY. Staleness explains an unhealthy observer not finding a
// transaction, so its NOT_FOUND is ignored; a found transaction with
// different economics is a hard conflict regardless of health.
func (m *MultiObserver) resolveWithUnhealthy(pa, sa answer[TxObservation], required Finality) Resolution {
	pol := m.opts.Policy
	healthy, sick := pa, sa
	healthySide, sickName := SidePrimary, m.secondary.Name()
	if pa.health == provider.Unhealthy && sa.health != provider.Unhealthy {
		healthy, sick = sa, pa
		healthySide, sickName = SideSecondary, m.primary.Name()
	}
	if pa.health == provider.Unhealthy && sa.health == provider.Unhealthy {
		// Both unhealthy: full comparison, then cap.
		res := pol.Resolve(pa.value, sa.value, required)
		res.Degraded = true
		if res.State == Agreed {
			res.Finality = MinFinality(res.Finality, pol.singleCap())
			res.Satisfied = res.Finality.AtLeast(required)
			res.BlockDependent = !res.Satisfied && required.AtLeast(pol.RequireBothFor)
			res.Detail += "; both observers UNHEALTHY, finality capped at " + string(pol.singleCap())
		}
		return res
	}
	if sick.value.Found && healthy.value.Found {
		if diffs := EconomicDifferences(healthy.value, sick.value); len(diffs) > 0 {
			res := pol.Resolve(pa.value, sa.value, required)
			res.Degraded = true
			res.Detail += "; " + sickName + " is UNHEALTHY but its answer conflicts"
			return res
		}
	}
	if sick.value.Found && !healthy.value.Found {
		res := pol.Resolve(pa.value, sa.value, required)
		res.Degraded = true
		res.Detail += "; " + sickName + " is UNHEALTHY"
		return res
	}
	res := pol.ResolveSingle(healthy.value, healthySide, required, sickName+" is UNHEALTHY")
	if healthySide == SidePrimary {
		v := sa.value
		res.Secondary = &v
	} else {
		v := pa.value
		res.Primary = &v
	}
	return res
}

// Balances queries both observers for the wallet's balances and resolves.
func (m *MultiObserver) Balances(ctx context.Context, wallet string, mints []string) (BalanceResolution, error) {
	pa, sa := query(ctx, m, func(c context.Context, o ChainObserver) ([]BalanceObservation, error) {
		return o.GetBalances(c, wallet, mints)
	})
	pol := m.opts.Policy
	pOK := pa.available() && pa.health != provider.Unhealthy
	sOK := sa.available() && sa.health != provider.Unhealthy
	switch {
	case pOK && sOK:
		return pol.ResolveBalances(pa.value, sa.value), nil
	case pOK:
		return pol.ResolveBalancesSingle(pa.value, SidePrimary, unavailableReason(m.secondary.Name(), sa)), nil
	case sOK:
		return pol.ResolveBalancesSingle(sa.value, SideSecondary, unavailableReason(m.primary.Name(), pa)), nil
	case pa.available() && sa.available():
		res := pol.ResolveBalances(pa.value, sa.value)
		res.Degraded = true
		res.Detail += "; both observers UNHEALTHY"
		return res, nil
	case pa.available():
		return pol.ResolveBalancesSingle(pa.value, SidePrimary, unavailableReason(m.secondary.Name(), sa)+"; "+m.primary.Name()+" is UNHEALTHY"), nil
	case sa.available():
		return pol.ResolveBalancesSingle(sa.value, SideSecondary, unavailableReason(m.primary.Name(), pa)+"; "+m.secondary.Name()+" is UNHEALTHY"), nil
	}
	return BalanceResolution{}, errs.Wrap(errors.Join(pa.err, sa.err), errs.CodeProviderUnavailable, "no chain observer available for balances")
}

// HeightResolution is the joint block height.
type HeightResolution struct {
	// Height is the lower of the available heights: the conservative value
	// for expiry decisions (a transaction is only "provably expired" when
	// every observer agrees the chain has moved past lastValidBlockHeight).
	Height uint64 `json:"height"`
	// Skew is the difference between the two heights when both answered.
	Skew     uint64 `json:"skew"`
	Degraded bool   `json:"degraded"`
	Detail   string `json:"detail"`
}

// BlockHeight returns the conservative (minimum) block height across the
// available observers.
func (m *MultiObserver) BlockHeight(ctx context.Context) (HeightResolution, error) {
	pa, sa := query(ctx, m, func(c context.Context, o ChainObserver) (uint64, error) {
		return o.GetBlockHeight(c)
	})
	switch {
	case pa.available() && sa.available():
		h, skew := pa.value, uint64(0)
		if sa.value < h {
			h = sa.value
		}
		if pa.value > sa.value {
			skew = pa.value - sa.value
		} else {
			skew = sa.value - pa.value
		}
		return HeightResolution{Height: h, Skew: skew, Detail: fmt.Sprintf("min of %d and %d", pa.value, sa.value)}, nil
	case pa.available():
		return HeightResolution{Height: pa.value, Degraded: true, Detail: "degraded: " + unavailableReason(m.secondary.Name(), sa)}, nil
	case sa.available():
		return HeightResolution{Height: sa.value, Degraded: true, Detail: "degraded: " + unavailableReason(m.primary.Name(), pa)}, nil
	}
	return HeightResolution{}, errs.Wrap(errors.Join(pa.err, sa.err), errs.CodeProviderUnavailable, "no chain observer available for block height")
}

// ProvenAbsent applies EXECUTION.md §4 step 7: a signature is provably
// absent only when both observers answered NOT_FOUND (no degradation) and
// the conservative block height exceeds lastValidBlockHeight + margin.
func ProvenAbsent(res Resolution, height HeightResolution, lastValidBlockHeight, margin uint64) bool {
	if res.State != NotFound || res.Degraded || res.BlockDependent || height.Degraded {
		return false
	}
	return height.Height > lastValidBlockHeight+margin
}
