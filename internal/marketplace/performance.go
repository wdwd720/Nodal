package marketplace

import (
	"context"
	"crypto/sha256"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
)

// FollowPoint is one point of an install's paper-follow history.
type FollowPoint struct {
	AsOf time.Time `json:"as_of"`
	// Equity is the snapshot portfolio's exact value; Turnover the exact
	// notional dispatched by that run. Decimal strings.
	Equity   string `json:"equity"`
	Turnover string `json:"turnover"`
}

// Performance is paper performance computed exactly from a follow history.
//
// Every figure is in basis points and every rounding is named:
//
//   - ReturnBPS = (last - first) / first x 10^4, rounded half-even.
//   - MaxDrawdownBPS = max over the history of (running peak - equity) /
//     running peak x 10^4, rounded half-even.
//   - VolatilityBPS is the population standard deviation of the per-run
//     returns: each per-run return is rounded half-even to 10^-6 BPS, the
//     variance of those integers is exact, and the square root is the
//     integer square root -- rounded down.
//   - TurnoverBPS = total dispatched notional / mean equity x 10^4, rounded
//     half-even.
//
// There is no floating point anywhere: equities are aligned to one scale and
// every step is big.Int arithmetic.
type Performance struct {
	Points         int       `json:"points"`
	FirstAsOf      time.Time `json:"first_as_of"`
	LastAsOf       time.Time `json:"last_as_of"`
	ReturnBPS      int64     `json:"return_bps"`
	MaxDrawdownBPS int64     `json:"max_drawdown_bps"`
	VolatilityBPS  int64     `json:"volatility_bps"`
	TurnoverBPS    int64     `json:"turnover_bps"`
	// HistoryDigest is the sha256 of the canonical history the figures were
	// computed from.
	HistoryDigest []byte `json:"-"`
}

var (
	bpsScale   = big.NewInt(10_000)
	microScale = big.NewInt(1_000_000)
)

// ComputePerformance computes the figures from a history ordered by as-of
// time. It refuses an empty history, an out-of-order one, a negative value
// and a zero equity it would have to divide by: a figure that cannot be
// computed exactly is not computed.
func ComputePerformance(points []FollowPoint) (Performance, error) {
	if len(points) == 0 {
		return Performance{}, errs.New(errs.CodeValidationFailed, "marketplace: performance needs at least one follow run")
	}
	equities := make([]nodalsdk.Decimal, len(points))
	turnovers := make([]nodalsdk.Decimal, len(points))
	for i, p := range points {
		if i > 0 && !p.AsOf.After(points[i-1].AsOf) {
			return Performance{}, errs.New(errs.CodeValidationFailed, "marketplace: the history must be strictly ordered by as-of time")
		}
		e, err := nodalsdk.ParseDecimal(p.Equity)
		if err != nil || e.Sign() < 0 {
			return Performance{}, errs.New(errs.CodeValidationFailed, "marketplace: equity must be a non-negative decimal")
		}
		t, err := nodalsdk.ParseDecimal(p.Turnover)
		if err != nil || t.Sign() < 0 {
			return Performance{}, errs.New(errs.CodeValidationFailed, "marketplace: turnover must be a non-negative decimal")
		}
		equities[i], turnovers[i] = e, t
	}
	// One common scale for every value: each ratio below is then a ratio of
	// integers.
	all := alignedUnscaled(append(append([]nodalsdk.Decimal{}, equities...), turnovers...))
	eq, tv := all[:len(points)], all[len(points):]

	perf := Performance{Points: len(points), FirstAsOf: points[0].AsOf.UTC(), LastAsOf: points[len(points)-1].AsOf.UTC()}
	digest, err := historyDigest(points)
	if err != nil {
		return Performance{}, err
	}
	perf.HistoryDigest = digest

	first, last := eq[0], eq[len(eq)-1]
	if first.Sign() == 0 {
		return Performance{}, errs.New(errs.CodeValidationFailed, "marketplace: a return from zero equity is undefined")
	}
	ret, err := ratioBPS(new(big.Int).Sub(last, first), first)
	if err != nil {
		return Performance{}, err
	}
	perf.ReturnBPS = ret

	peak := new(big.Int).Set(eq[0])
	worst := new(big.Int)
	worstPeak := big.NewInt(1)
	for _, e := range eq {
		if e.Cmp(peak) > 0 {
			peak.Set(e)
		}
		if peak.Sign() == 0 {
			continue
		}
		dd := new(big.Int).Sub(peak, e)
		// dd/peak > worst/worstPeak  <=>  dd x worstPeak > worst x peak
		if new(big.Int).Mul(dd, worstPeak).Cmp(new(big.Int).Mul(worst, peak)) > 0 {
			worst.Set(dd)
			worstPeak.Set(peak)
		}
	}
	if perf.MaxDrawdownBPS, err = ratioBPS(worst, worstPeak); err != nil {
		return Performance{}, err
	}

	if len(eq) > 1 {
		returns := make([]*big.Int, 0, len(eq)-1)
		for i := 1; i < len(eq); i++ {
			if eq[i-1].Sign() == 0 {
				return Performance{}, errs.New(errs.CodeValidationFailed, "marketplace: a period return from zero equity is undefined")
			}
			num := new(big.Int).Mul(new(big.Int).Sub(eq[i], eq[i-1]), new(big.Int).Mul(bpsScale, microScale))
			returns = append(returns, divHalfEven(num, eq[i-1]))
		}
		if perf.VolatilityBPS, err = volatilityBPS(returns); err != nil {
			return Performance{}, err
		}
	}

	totalTurnover, totalEquity := new(big.Int), new(big.Int)
	for i := range tv {
		totalTurnover.Add(totalTurnover, tv[i])
		totalEquity.Add(totalEquity, eq[i])
	}
	if totalEquity.Sign() == 0 {
		if totalTurnover.Sign() != 0 {
			return Performance{}, errs.New(errs.CodeValidationFailed, "marketplace: turnover against zero equity is undefined")
		}
	} else {
		// total / (sum / n) = total x n / sum
		if perf.TurnoverBPS, err = ratioBPS(new(big.Int).Mul(totalTurnover, big.NewInt(int64(len(points)))), totalEquity); err != nil {
			return Performance{}, err
		}
	}
	return perf, nil
}

// alignedUnscaled returns every decimal's unscaled integer at the largest
// scale among them, so ratios between them are exact.
func alignedUnscaled(ds []nodalsdk.Decimal) []*big.Int {
	var scale int32
	for _, d := range ds {
		if d.Scale() > scale {
			scale = d.Scale()
		}
	}
	out := make([]*big.Int, len(ds))
	for i, d := range ds {
		u := d.Unscaled()
		out[i] = u.Mul(u, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale-d.Scale())), nil))
	}
	return out
}

// ratioBPS is num/den x 10^4 rounded half-even, as an int64.
func ratioBPS(num, den *big.Int) (int64, error) {
	q := divHalfEven(new(big.Int).Mul(num, bpsScale), den)
	if !q.IsInt64() {
		return 0, errs.New(errs.CodeOverflow, "marketplace: a performance figure does not fit in int64 basis points")
	}
	return q.Int64(), nil
}

// divHalfEven is num/den rounded half to even; den is never zero here.
func divHalfEven(num, den *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Sign() == 0 {
		return q
	}
	twice := new(big.Int).Abs(r)
	twice.Lsh(twice, 1)
	c := twice.Cmp(new(big.Int).Abs(den))
	if c > 0 || (c == 0 && q.Bit(0) == 1) {
		if num.Sign()*den.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

// volatilityBPS is the population standard deviation of returns given in
// 10^-6 BPS, in whole BPS rounded down:
//
//	floor( sqrt( (n x sum(x^2) - (sum x)^2) / n^2 ) / 10^6 )
//	= isqrt( floor( (n x sum(x^2) - (sum x)^2) / (n^2 x 10^12) ) )
//
// The equality holds because floor(sqrt(y)) = isqrt(floor(y)) for y >= 0.
func volatilityBPS(returns []*big.Int) (int64, error) {
	n := big.NewInt(int64(len(returns)))
	sum, sumSq := new(big.Int), new(big.Int)
	for _, x := range returns {
		sum.Add(sum, x)
		sumSq.Add(sumSq, new(big.Int).Mul(x, x))
	}
	num := new(big.Int).Sub(new(big.Int).Mul(n, sumSq), new(big.Int).Mul(sum, sum))
	den := new(big.Int).Mul(new(big.Int).Mul(n, n), new(big.Int).Mul(microScale, microScale))
	v := new(big.Int).Sqrt(new(big.Int).Quo(num, den))
	if !v.IsInt64() {
		return 0, errs.New(errs.CodeOverflow, "marketplace: volatility does not fit in int64 basis points")
	}
	return v.Int64(), nil
}

func historyDigest(points []FollowPoint) ([]byte, error) {
	normalized := make([]FollowPoint, len(points))
	for i, p := range points {
		normalized[i] = FollowPoint{AsOf: p.AsOf.UTC(), Equity: p.Equity, Turnover: p.Turnover}
	}
	b, err := audit.CanonicalJSON(normalized)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "marketplace: encode follow history")
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// PerformanceSnapshot is a recorded Performance.
type PerformanceSnapshot struct {
	Performance
	InstallID  InstallID
	VersionID  VersionID
	ComputedAt time.Time
}

// RecordPerformance computes an install's paper performance from its follow
// history and records it, with the digest of the exact history it used.
func (s *Service) RecordPerformance(ctx context.Context, tx pgx.Tx, iid InstallID, by econ.Actor) (PerformanceSnapshot, error) {
	if err := requireActor(by, TransitionActorTypes(), "record performance"); err != nil {
		return PerformanceSnapshot{}, err
	}
	install, err := s.Install(ctx, tx, iid)
	if err != nil {
		return PerformanceSnapshot{}, err
	}
	runs, err := s.FollowRuns(ctx, tx, iid)
	if err != nil {
		return PerformanceSnapshot{}, err
	}
	points := make([]FollowPoint, len(runs))
	for i, r := range runs {
		points[i] = FollowPoint{AsOf: r.AsOf, Equity: r.Equity, Turnover: r.Turnover}
	}
	perf, err := ComputePerformance(points)
	if err != nil {
		return PerformanceSnapshot{}, err
	}
	snap := PerformanceSnapshot{Performance: perf, InstallID: iid, VersionID: install.VersionID, ComputedAt: s.now()}
	if _, err := tx.Exec(ctx, `INSERT INTO marketplace_performance_snapshots (id, install_id, version_id, follow_runs_count, first_as_of, last_as_of,
		history_digest, return_bps, max_drawdown_bps, volatility_bps, turnover_bps, computed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		id.New[id.Any](), iid, install.VersionID, perf.Points, perf.FirstAsOf, perf.LastAsOf, perf.HistoryDigest, perf.ReturnBPS,
		perf.MaxDrawdownBPS, perf.VolatilityBPS, perf.TurnoverBPS, snap.ComputedAt); err != nil {
		return PerformanceSnapshot{}, dbErr("record performance", err)
	}
	return snap, nil
}
