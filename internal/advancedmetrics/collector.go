package advancedmetrics

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/nodal/controlplane/internal/advancedjobs"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/transfers"
)

// Name is the collector's name as a job of the advanced worker.
const Name = "metrics.advanced"

// Collector defaults.
const (
	DefaultInterval = time.Minute
	// DefaultLag is how far behind the database's clock the counted window
	// ends: longer than any transaction is open (the statement timeout is
	// 30s), so a row stamped inside the window has committed before it is
	// counted.
	DefaultLag = time.Minute
	// DefaultHorizon is how long an effect may stay PENDING or UNKNOWN
	// before it counts as unmatched.
	DefaultHorizon = 5 * time.Minute
	// DefaultMarginScan bounds the margin accounts margin.Aggregate values.
	DefaultMarginScan = 1000
	// DefaultMaxMarkAge is the mark age the API's valuation accepts.
	DefaultMaxMarkAge = 5 * time.Minute
)

// Config wires a Collector. Margin and Stable are optional: without them the
// margin gauges and the reserve coverage are not recorded.
type Config struct {
	DB      *db.DB
	Clock   clock.Clock
	Metrics *observability.AdvancedMetrics
	Extra   *Instruments
	// Valuer prices a SIMULATED asset in USD (cmd/api's transfers valuer:
	// par for a registry-declared paper dollar, a fresh mark otherwise).
	Valuer transfers.Valuer
	Assets *assets.Repository
	Margin *margin.Service
	Stable *stableasset.Service
	// Network is the devnet network Stable's host carries.
	Network    string
	Interval   time.Duration
	Lag        time.Duration
	Horizon    time.Duration
	MarginScan int
}

// Collector records the advanced instruments from the database. It is an
// advancedjobs.Job, so the worker's scheduler runs it -- never twice at once
// -- and tracks its health with the other jobs.
type Collector struct {
	cfg Config

	mu sync.Mutex
	// watermark is the end of the last counted window; nil until the first
	// pass. pending is the end of the window a pass has read, applied by
	// record once the counts are recorded.
	watermark *time.Time
	pending   *time.Time
}

var _ advancedjobs.Job = (*Collector)(nil)

// NewCollector validates the wiring.
func NewCollector(cfg Config) (*Collector, error) {
	if cfg.DB == nil || cfg.Clock == nil || cfg.Metrics == nil || cfg.Extra == nil || cfg.Valuer == nil || cfg.Assets == nil {
		return nil, errs.New(errs.CodeInternal, "advancedmetrics: the collector needs a database, a clock, its instruments, a valuer and the asset registry")
	}
	if cfg.Stable != nil && cfg.Network == "" {
		return nil, errs.New(errs.CodeInternal, "advancedmetrics: reserve coverage needs the network the stable asset controller's host carries")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Lag < 0 {
		return nil, errs.New(errs.CodeInternal, "advancedmetrics: the counted window's lag cannot be negative")
	}
	if cfg.Horizon <= 0 {
		cfg.Horizon = DefaultHorizon
	}
	if cfg.MarginScan <= 0 {
		cfg.MarginScan = DefaultMarginScan
	}
	return &Collector{cfg: cfg}, nil
}

// Spec implements advancedjobs.Job.
func (c *Collector) Spec() advancedjobs.Spec {
	return advancedjobs.Spec{
		Name: Name, Settings: advancedjobs.Settings{Interval: c.cfg.Interval, Batch: 1},
		Calls:      "reads ledger holdings, margin.Service.Aggregate, stableasset.Service.ReserveLedger, effects, reconciliation records and new risk, margin, sandbox and trade rows",
		KillSwitch: "reads only; consults no kill switch",
	}
}

// Snapshot is what one pass read and recorded. Values are int64: USD minor
// units, basis points, counts.
type Snapshot struct {
	// Capital.
	GrossAssets         int64
	CustomerLiabilities int64
	HouseAssets         int64
	UnvaluedAssets      int64
	// Margin is nil without a margin service.
	Margin *MarginFigures
	// ReserveCoverage is basis points per stable asset name.
	ReserveCoverage map[string]int64
	// Reconciliation.
	UnmatchedEffects  map[string]int64 // per subsystem
	OpenRecords       map[string]int64 // per kind
	ChainMismatches   int64
	ReserveMismatches int64
	// Counters: nil on the pass that only sets the watermark.
	Counted *Counted
}

// MarginFigures are margin.Aggregate's, in USD minor units and bps.
type MarginFigures struct {
	Accounts, Unvalued int64
	GrossExposure      int64
	LiquidationBuffer  int64
	// LeverageBPS is nil while aggregate equity is not positive.
	LeverageBPS *int64
}

// Counted are the rows created inside one window.
type Counted struct {
	From, To            time.Time
	RiskDenies          map[string]int64 // per reason code
	StaleOracleEvents   int64
	MarginCalls         int64
	LiquidationEvents   int64
	SandboxRuns         int64
	SandboxKilled       map[string]int64 // per termination reason
	SandboxCPULimit     int64
	SandboxMemoryLimit  int64
	InternalMatchVolume int64 // USD minor units
	UnvaluedMatchTrades int64
}

// Run implements advancedjobs.Job: one collection. Each section is read in a
// read-only transaction of its own, so a section that cannot be read is a
// failure of that section and the others are still recorded.
func (c *Collector) Run(ctx context.Context) (advancedjobs.Report, error) {
	_, rep, err := c.Collect(ctx)
	return rep, err
}

// Collect reads every section, records what it read and returns it.
func (c *Collector) Collect(ctx context.Context) (Snapshot, advancedjobs.Report, error) {
	var snap Snapshot
	var rep advancedjobs.Report
	section := func(name string, fn func(ctx context.Context, tx pgx.Tx) error) {
		rep.Examined++
		err := c.cfg.DB.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true, MaxRetries: 3}, fn)
		if err != nil {
			rep.Failures = append(rep.Failures, sectionFailure(name, err))
			return
		}
		rep.Changed++
	}
	section("capital", func(ctx context.Context, tx pgx.Tx) error { return c.capital(ctx, tx, &snap) })
	if c.cfg.Margin != nil {
		section("margin", func(ctx context.Context, tx pgx.Tx) error { return c.margin(ctx, tx, &snap) })
	}
	if c.cfg.Stable != nil {
		section("reserve_coverage", func(ctx context.Context, tx pgx.Tx) error { return c.reserves(ctx, tx, &snap) })
	}
	section("reconciliation", func(ctx context.Context, tx pgx.Tx) error { return c.reconciliation(ctx, tx, &snap) })
	section("counters", func(ctx context.Context, tx pgx.Tx) error { return c.counters(ctx, tx, &snap) })
	if err := ctx.Err(); err != nil {
		return snap, rep, err
	}
	c.record(ctx, snap)
	return snap, rep, nil
}

func sectionFailure(name string, err error) advancedjobs.Failure {
	code, reason := advancedjobs.Redact(err)
	return advancedjobs.Failure{Unit: name, Code: code, Reason: reason}
}

// holdingsSQL sums every SIMULATED holding by owner type and asset: the codes
// the ledger classifies as holding value ($1), whoever owns them.
const holdingsSQL = `SELECT la.owner_type, la.asset_id, sum(b.balance)::text
	FROM ledger_accounts la
	JOIN ledger_balances b ON b.ledger_account_id = la.id
	JOIN assets a ON a.id = la.asset_id
	WHERE a.value_domain = 'SIMULATED' AND la.code = ANY($1) AND b.balance <> 0
	GROUP BY la.owner_type, la.asset_id ORDER BY la.asset_id, la.owner_type`

func (c *Collector) capital(ctx context.Context, tx pgx.Tx, snap *Snapshot) error {
	codes := make([]string, 0, len(ledger.HoldingCodes()))
	for _, code := range ledger.HoldingCodes() {
		codes = append(codes, string(code))
	}
	rows, err := tx.Query(ctx, holdingsSQL, codes)
	if err != nil {
		return err
	}
	type holding struct {
		owner ledger.OwnerType
		asset assets.AssetID
		qty   money.Quantity
	}
	var hs []holding
	for rows.Next() {
		var h holding
		var qty string
		if err := rows.Scan(&h.owner, &h.asset, &qty); err != nil {
			rows.Close()
			return err
		}
		if h.qty, err = money.ScanQuantity(qty); err != nil {
			rows.Close()
			return err
		}
		hs = append(hs, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := c.cfg.Clock.Now()
	quotes := map[assets.AssetID]*transfers.Quote{}
	unvalued := map[assets.AssetID]bool{}
	var gross, customers, house money.USD
	add := func(acc *money.USD, v money.USD) error {
		n, err := acc.Add(v)
		if err != nil {
			return err
		}
		*acc = n
		return nil
	}
	for _, h := range hs {
		q, ok := quotes[h.asset]
		if !ok {
			q, err = c.quote(ctx, tx, h.asset, now)
			if err != nil {
				return err
			}
			quotes[h.asset] = q
		}
		if q == nil || h.qty.IsNegative() {
			unvalued[h.asset] = true
			continue
		}
		v, err := q.USD(h.qty, money.RoundDown)
		if err != nil {
			unvalued[h.asset] = true
			continue
		}
		if err := add(&gross, v); err != nil {
			return err
		}
		switch h.owner {
		case ledger.OwnerCustomer:
			if err := add(&customers, v); err != nil {
				return err
			}
		case ledger.OwnerHouse:
			if err := add(&house, v); err != nil {
				return err
			}
		}
	}
	snap.GrossAssets, snap.CustomerLiabilities, snap.HouseAssets = gross.Minor(), customers.Minor(), house.Minor()
	snap.UnvaluedAssets = int64(len(unvalued))
	return nil
}

// quote prices an asset, or returns nil when it cannot be valued now: a
// stale or missing mark is an unvalued holding, never a zero.
func (c *Collector) quote(ctx context.Context, q db.Querier, id assets.AssetID, now time.Time) (*transfers.Quote, error) {
	a, err := c.cfg.Assets.Get(ctx, q, id)
	if err != nil {
		return nil, err
	}
	qt, err := c.cfg.Valuer.Quote(ctx, q, a, now)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeInternal {
			return nil, err
		}
		return nil, nil
	}
	return &qt, nil
}

func (c *Collector) margin(ctx context.Context, tx pgx.Tx, snap *Snapshot) error {
	agg, err := c.cfg.Margin.Aggregate(ctx, tx, c.cfg.MarginScan)
	if err != nil {
		return err
	}
	f := &MarginFigures{Accounts: int64(agg.Accounts), Unvalued: int64(agg.Unvalued),
		GrossExposure: agg.GrossExposure.Minor(), LiquidationBuffer: agg.LiquidationBuffer.Minor()}
	if bps, ok := agg.LeverageBPS(); ok {
		v := int64(bps)
		f.LeverageBPS = &v
	}
	snap.Margin = f
	return nil
}

// stableAssetsSQL lists the stable assets of the network the controller's
// host carries, by name.
const stableAssetsSQL = `SELECT id, name FROM stable_assets WHERE network = $1 AND status IN ('ACTIVE','PAUSED') ORDER BY name`

func (c *Collector) reserves(ctx context.Context, tx pgx.Tx, snap *Snapshot) error {
	rows, err := tx.Query(ctx, stableAssetsSQL, c.cfg.Network)
	if err != nil {
		return err
	}
	type sa struct {
		id   stableasset.StableAssetID
		name string
	}
	var list []sa
	for rows.Next() {
		var s sa
		if err := rows.Scan(&s.id, &s.name); err != nil {
			rows.Close()
			return err
		}
		list = append(list, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	snap.ReserveCoverage = map[string]int64{}
	for _, s := range list {
		rl, err := c.cfg.Stable.ReserveLedger(ctx, tx, s.id)
		if err != nil {
			return err
		}
		snap.ReserveCoverage[s.name] = int64(rl.CoverageBPS)
	}
	return nil
}

// unmatchedEffectsSQL is econ.EffectStore.ListUnresolved's selection, counted
// per subsystem: effects PENDING or UNKNOWN and unchanged for longer than the
// horizon (seconds, $1) by the database's clock.
const unmatchedEffectsSQL = `SELECT subsystem, count(*) FROM economic_effects
	WHERE status IN ('PENDING','UNKNOWN') AND updated_at < now() - make_interval(secs => $1)
	GROUP BY subsystem`

// openRecordsSQL counts the unresolved reconciliation records per kind.
const openRecordsSQL = `SELECT kind, count(*) FROM reconciliation_records
	WHERE status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED') GROUP BY kind`

func (c *Collector) reconciliation(ctx context.Context, tx pgx.Tx, snap *Snapshot) error {
	snap.UnmatchedEffects = map[string]int64{}
	for _, s := range econ.AllSubsystems() {
		snap.UnmatchedEffects[string(s)] = 0
	}
	if err := countBy(ctx, tx, snap.UnmatchedEffects, unmatchedEffectsSQL, int64(c.cfg.Horizon/time.Second)); err != nil {
		return err
	}
	snap.OpenRecords = map[string]int64{}
	for _, k := range reconciliation.AllKinds() {
		snap.OpenRecords[string(k)] = 0
	}
	if err := countBy(ctx, tx, snap.OpenRecords, openRecordsSQL); err != nil {
		return err
	}
	snap.ChainMismatches = snap.OpenRecords[string(reconciliation.KindDevnetChain)] + snap.OpenRecords[string(reconciliation.KindBridge)]
	snap.ReserveMismatches = snap.OpenRecords[string(reconciliation.KindStableReserve)]
	return nil
}

// Counted rows in the window ($1, $2].
const (
	riskDeniesSQL = `SELECT r.code, count(*) FROM advanced_risk_decisions d, unnest(d.reason_codes) AS r(code)
		WHERE d.verdict = 'REJECT' AND d.created_at > $1 AND d.created_at <= $2 GROUP BY r.code`
	staleOracleSQL = `SELECT count(*) FROM advanced_risk_decisions
		WHERE verdict = 'REJECT' AND created_at > $1 AND created_at <= $2 AND reason_codes && $3::text[]`
	marginCallsSQL       = `SELECT count(*) FROM margin_calls WHERE created_at > $1 AND created_at <= $2`
	liquidationStepsSQL  = `SELECT count(*) FROM margin_liquidation_steps WHERE created_at > $1 AND created_at <= $2`
	sandboxRunsSQL       = `SELECT termination_reason, count(*) FROM sandbox_runs WHERE created_at > $1 AND created_at <= $2 GROUP BY termination_reason`
	internalMatchSQL     = `SELECT quote_asset_id, sum(quote_notional)::text, count(*) FROM book_trades WHERE created_at > $1 AND created_at <= $2 GROUP BY quote_asset_id ORDER BY quote_asset_id`
	databaseClockLagSQL  = `SELECT now() - make_interval(secs => $1)`
	terminationOK        = "OK"
	terminationTimeout   = "TIMEOUT"
	terminationMemoryCap = "MEMORY_LIMIT"
)

func (c *Collector) counters(ctx context.Context, tx pgx.Tx, snap *Snapshot) error {
	var upper time.Time
	if err := tx.QueryRow(ctx, databaseClockLagSQL, int64(c.cfg.Lag/time.Second)).Scan(&upper); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	from := c.watermark
	snap.Counted = nil
	switch {
	case from == nil:
		// The first pass only sets the watermark: history is not counted.
		c.pending = &upper
		return nil
	case !upper.After(*from):
		// A window that has not moved counts nothing.
		return nil
	}
	w := &Counted{From: *from, To: upper, RiskDenies: map[string]int64{}, SandboxKilled: map[string]int64{}}
	if err := countBy(ctx, tx, w.RiskDenies, riskDeniesSQL, w.From, w.To); err != nil {
		return err
	}
	stale := []string{risk.ReasonOracleStale, risk.ReasonOracleUnavailable}
	for _, x := range []struct {
		sql  string
		dst  *int64
		args []any
	}{
		{staleOracleSQL, &w.StaleOracleEvents, []any{w.From, w.To, stale}},
		{marginCallsSQL, &w.MarginCalls, []any{w.From, w.To}},
		{liquidationStepsSQL, &w.LiquidationEvents, []any{w.From, w.To}},
	} {
		if err := tx.QueryRow(ctx, x.sql, x.args...).Scan(x.dst); err != nil {
			return err
		}
	}
	runs := map[string]int64{}
	if err := countBy(ctx, tx, runs, sandboxRunsSQL, w.From, w.To); err != nil {
		return err
	}
	for reason, n := range runs {
		w.SandboxRuns += n
		if reason != terminationOK {
			w.SandboxKilled[reason] = n
		}
		switch reason {
		case terminationTimeout:
			w.SandboxCPULimit += n
		case terminationMemoryCap:
			w.SandboxMemoryLimit += n
		}
	}
	if err := c.matchVolume(ctx, tx, w); err != nil {
		return err
	}
	snap.Counted = w
	c.pending = &upper
	return nil
}

func (c *Collector) matchVolume(ctx context.Context, tx pgx.Tx, w *Counted) error {
	rows, err := tx.Query(ctx, internalMatchSQL, w.From, w.To)
	if err != nil {
		return err
	}
	type line struct {
		asset  assets.AssetID
		sum    money.Quantity
		trades int64
	}
	var lines []line
	for rows.Next() {
		var l line
		var sum string
		if err := rows.Scan(&l.asset, &sum, &l.trades); err != nil {
			rows.Close()
			return err
		}
		if l.sum, err = money.ScanQuantity(sum); err != nil {
			rows.Close()
			return err
		}
		lines = append(lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := c.cfg.Clock.Now()
	total := money.USD{}
	for _, l := range lines {
		q, err := c.quote(ctx, tx, l.asset, now)
		if err != nil {
			return err
		}
		if q == nil {
			w.UnvaluedMatchTrades += l.trades
			continue
		}
		v, err := q.USD(l.sum, money.RoundDown)
		if err != nil {
			w.UnvaluedMatchTrades += l.trades
			continue
		}
		if total, err = total.Add(v); err != nil {
			return err
		}
	}
	w.InternalMatchVolume = total.Minor()
	return nil
}

// countBy adds a (key, count) query's rows into dst.
func countBy(ctx context.Context, q pgx.Tx, dst map[string]int64, sql string, args ...any) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		dst[k] += n
	}
	return rows.Err()
}

// record writes a pass's figures to the instruments. A section that failed
// left its figures zero and records nothing; the counters are recorded only
// when their whole window was read, and only then does the watermark move.
func (c *Collector) record(ctx context.Context, s Snapshot) {
	m, x := c.cfg.Metrics, c.cfg.Extra
	m.GrossAssets.Record(ctx, s.GrossAssets)
	m.CustomerLiabilities.Record(ctx, s.CustomerLiabilities)
	m.HouseAssets.Record(ctx, s.HouseAssets)
	x.UnvaluedHoldings.Record(ctx, s.UnvaluedAssets)
	if s.Margin != nil {
		m.MarginExposure.Record(ctx, s.Margin.GrossExposure)
		m.LiquidationBuffer.Record(ctx, s.Margin.LiquidationBuffer)
		if s.Margin.LeverageBPS != nil {
			m.Leverage.Record(ctx, *s.Margin.LeverageBPS)
		}
	}
	for _, name := range sortedKeys(s.ReserveCoverage) {
		m.ReserveCoverage.Record(ctx, s.ReserveCoverage[name], observability.WithSafeAttrs(attribute.String("stable_asset", name)))
	}
	for _, sub := range sortedKeys(s.UnmatchedEffects) {
		m.UnmatchedEconomicEffects.Record(ctx, s.UnmatchedEffects[sub], observability.WithSafeAttrs(attribute.String("subsystem", sub)))
	}
	for _, kind := range sortedKeys(s.OpenRecords) {
		x.OpenReconciliationRecords.Record(ctx, s.OpenRecords[kind], observability.WithSafeAttrs(attribute.String("kind", kind)))
	}
	if s.OpenRecords != nil {
		m.ChainMismatches.Record(ctx, s.ChainMismatches)
		m.ReserveMismatches.Record(ctx, s.ReserveMismatches)
	}
	if w := s.Counted; w != nil {
		for _, reason := range sortedKeys(w.RiskDenies) {
			m.RiskDenies.Add(ctx, w.RiskDenies[reason], observability.WithSafeAttrs(attribute.String("reason", reason)))
		}
		m.StaleOracleEvents.Add(ctx, w.StaleOracleEvents)
		m.MarginCalls.Add(ctx, w.MarginCalls)
		m.LiquidationEvents.Add(ctx, w.LiquidationEvents)
		m.SandboxRuns.Add(ctx, w.SandboxRuns)
		for _, reason := range sortedKeys(w.SandboxKilled) {
			m.SandboxKilledRuns.Add(ctx, w.SandboxKilled[reason], observability.WithSafeAttrs(attribute.String("reason", reason)))
		}
		m.SandboxCPULimitHits.Add(ctx, w.SandboxCPULimit)
		m.SandboxMemoryLimitHits.Add(ctx, w.SandboxMemoryLimit)
		m.InternalMatchVolume.Add(ctx, w.InternalMatchVolume)
		x.UnvaluedMatchTrades.Add(ctx, w.UnvaluedMatchTrades)
	}
	c.mu.Lock()
	if c.pending != nil {
		c.watermark = c.pending
		c.pending = nil
	}
	c.mu.Unlock()
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
