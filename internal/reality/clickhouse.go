package reality

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/big"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

// ClickHouse protocols.
const (
	ClickHouseNative = "native"
	ClickHouseHTTP   = "http"
)

// Query limits.
const (
	DefaultQueryLimit = 1000
	MaxQueryLimit     = 10_000
	// DefaultTTLDays is the Appendix A TTL of normalized_events.
	DefaultTTLDays = 730
)

var tableSuffixRe = regexp.MustCompile(`^[a-z0-9_]{0,32}$`)

// ClickHouseOptions tune the store.
type ClickHouseOptions struct {
	// TableSuffix is appended to every table name (tests use a unique
	// suffix so runs never collide). Must match ^[a-z0-9_]{0,32}$.
	TableSuffix string
	// Protocol is "native" or "http". Empty selects http for the 8123/18123
	// ports and native otherwise.
	Protocol string
	// TTLDays is the normalized_events TTL (DefaultTTLDays when zero); the
	// composition root passes the RAW_MARKET_DATA retention.
	TTLDays int
	// DialTimeout bounds connection establishment (default 10 s).
	DialTimeout time.Duration
	// MaxOpenConns bounds the pool (default 8).
	MaxOpenConns int
}

// ClickHouseStore is the non-authoritative analytics store (Appendix A). It
// is never read by a money decision.
type ClickHouseStore struct {
	conn    driver.Conn
	suffix  string
	ttlDays int
}

// EventSink is what the ingest pipeline writes normalized events to.
type EventSink interface {
	InsertNormalized(ctx context.Context, events []NormalizedEvent) error
}

var _ EventSink = (*ClickHouseStore)(nil)

// NewClickHouseStore opens the connection and pings it. Credentials are
// resolved from the SecretRefs; TLS is required when the config says so.
func NewClickHouseStore(ctx context.Context, cfg config.ClickHouseConfig, resolver config.Resolver, opts ClickHouseOptions) (*ClickHouseStore, error) {
	if cfg.Addr == "" || cfg.Database == "" {
		return nil, errors.New("reality: clickhouse addr and database are required")
	}
	if !tableSuffixRe.MatchString(opts.TableSuffix) {
		return nil, errors.New("reality: clickhouse table suffix must match ^[a-z0-9_]{0,32}$")
	}
	var user, pass string
	if !cfg.UsernameRef.IsZero() {
		if resolver == nil {
			return nil, errors.New("reality: secret resolver is required for clickhouse credentials")
		}
		var err error
		if user, err = resolver.Resolve(ctx, cfg.UsernameRef); err != nil {
			return nil, fmt.Errorf("reality: resolve clickhouse username: %w", err)
		}
		if !cfg.PasswordRef.IsZero() {
			if pass, err = resolver.Resolve(ctx, cfg.PasswordRef); err != nil {
				return nil, fmt.Errorf("reality: resolve clickhouse password: %w", err)
			}
		}
	}
	proto := clickhouse.Native
	switch strings.ToLower(opts.Protocol) {
	case ClickHouseHTTP:
		proto = clickhouse.HTTP
	case ClickHouseNative:
	case "":
		if _, port, err := net.SplitHostPort(cfg.Addr); err == nil && (port == "8123" || port == "18123" || port == "8443") {
			proto = clickhouse.HTTP
		}
	default:
		return nil, fmt.Errorf("reality: unknown clickhouse protocol %q", opts.Protocol)
	}
	dial := opts.DialTimeout
	if dial <= 0 {
		dial = 10 * time.Second
	}
	maxOpen := opts.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 8
	}
	o := &clickhouse.Options{
		Addr: []string{cfg.Addr}, Auth: clickhouse.Auth{Database: cfg.Database, Username: user, Password: pass},
		Protocol: proto, DialTimeout: dial, MaxOpenConns: maxOpen, MaxIdleConns: maxOpen / 2, ConnMaxLifetime: time.Hour,
		ClientInfo: clickhouse.ClientInfo{Products: []struct{ Name, Version string }{{Name: "controlplane-reality", Version: config.BuildVersion}}},
	}
	if cfg.RequireTLS {
		o.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	conn, err := clickhouse.Open(o)
	if err != nil {
		return nil, fmt.Errorf("reality: open clickhouse: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, dial)
	defer cancel()
	if err := conn.Ping(pctx); err != nil {
		_ = conn.Close()
		return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "reality: clickhouse ping failed")
	}
	ttl := opts.TTLDays
	if ttl <= 0 {
		ttl = DefaultTTLDays
	}
	return &ClickHouseStore{conn: conn, suffix: opts.TableSuffix, ttlDays: ttl}, nil
}

// Table returns the suffixed name of a base table.
func (s *ClickHouseStore) Table(base string) string { return base + s.suffix }

// Ping checks connectivity.
func (s *ClickHouseStore) Ping(ctx context.Context) error { return s.conn.Ping(ctx) }

// Close releases the pool.
func (s *ClickHouseStore) Close() error { return s.conn.Close() }

// ddl returns the Appendix A statements, idempotent and suffixed.
func (s *ClickHouseStore) ddl() []string {
	return []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
    event_id UUID, dedup_id String, schema_version UInt16, source LowCardinality(String), event_type LowCardinality(String),
    sequence Nullable(UInt64), source_partition String, source_offset String,
    source_event_at DateTime64(9, 'UTC'), provider_published_at Nullable(DateTime64(9, 'UTC')),
    platform_received_at DateTime64(9, 'UTC'), normalized_at DateTime64(9, 'UTC'),
    feature_available_at DateTime64(9, 'UTC'), decision_available_at DateTime64(9, 'UTC'),
    instrument_id Nullable(UUID), asset_id Nullable(UUID), wallet String,
    raw_object_id UUID, raw_object_hash FixedString(32), payload String
) ENGINE = ReplacingMergeTree(platform_received_at)
PARTITION BY toYYYYMM(decision_available_at) ORDER BY (source, event_type, dedup_id) TTL toDateTime(platform_received_at) + INTERVAL %d DAY`, s.Table("normalized_events"), s.ttlDays),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
    instrument_id UUID, source LowCardinality(String), price_mantissa Int128, price_scale UInt8,
    quote_asset_id UUID, liquidity_mantissa Nullable(Int128), liquidity_scale UInt8,
    source_event_at DateTime64(9, 'UTC'), platform_received_at DateTime64(9, 'UTC'), decision_available_at DateTime64(9, 'UTC'),
    raw_object_id UUID, dedup_id String
) ENGINE = ReplacingMergeTree(platform_received_at)
PARTITION BY toYYYYMM(decision_available_at) ORDER BY (instrument_id, source, decision_available_at, dedup_id)`, s.Table("market_prices")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
    run_id UUID, backtest_id Nullable(UUID), agent_id Nullable(UUID), strategy_version_id UUID, mode LowCardinality(String),
    decision_at DateTime64(9, 'UTC'), trigger_name String, information_set_hash FixedString(32), trace_hash FixedString(32),
    observations String, signals String, conditions String, actions String, skips String
) ENGINE = MergeTree PARTITION BY toYYYYMM(decision_at) ORDER BY (strategy_version_id, mode, decision_at, run_id)`, s.Table("strategy_decisions")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
    backtest_id UUID, decision_ref String, simulated_at DateTime64(9, 'UTC'), instrument_id UUID, side LowCardinality(String),
    input_quantity Int128, output_quantity Int128, fee_quantity Int128, slippage_bps Int32, price_impact_bps Int32,
    latency_ms UInt32, submission_failed UInt8, assumptions String, counterfactual_branch LowCardinality(String)
) ENGINE = MergeTree PARTITION BY toYYYYMM(simulated_at) ORDER BY (backtest_id, simulated_at, decision_ref)`, s.Table("backtest_trades")),
	}
}

// EnsureSchema applies the DDL (CREATE TABLE IF NOT EXISTS, so it is safe to
// run at every start).
func (s *ClickHouseStore) EnsureSchema(ctx context.Context) error {
	for _, stmt := range s.ddl() {
		if err := s.conn.Exec(ctx, stmt); err != nil {
			return errs.Wrap(err, errs.CodeProviderUnavailable, "reality: apply clickhouse schema")
		}
	}
	return nil
}

// DropSchema drops the suffixed tables. It refuses to run without a suffix
// so production tables can never be dropped through this path.
func (s *ClickHouseStore) DropSchema(ctx context.Context) error {
	if s.suffix == "" {
		return errs.New(errs.CodeForbidden, "reality: refusing to drop unsuffixed clickhouse tables")
	}
	for _, base := range []string{"normalized_events", "market_prices", "strategy_decisions", "backtest_trades"} {
		if err := s.conn.Exec(ctx, "DROP TABLE IF EXISTS "+s.Table(base)); err != nil {
			return errs.Wrap(err, errs.CodeProviderUnavailable, "reality: drop clickhouse table")
		}
	}
	return nil
}

const normalizedColumns = `event_id, dedup_id, schema_version, source, event_type, sequence, source_partition, source_offset,
	source_event_at, provider_published_at, platform_received_at, normalized_at, feature_available_at, decision_available_at,
	instrument_id, asset_id, wallet, raw_object_id, raw_object_hash, payload`

// InsertNormalized implements EventSink: one batch, validated before any
// byte is sent.
func (s *ClickHouseStore) InsertNormalized(ctx context.Context, events []NormalizedEvent) error {
	if len(events) == 0 {
		return nil
	}
	for i := range events {
		if err := events[i].Validate(); err != nil {
			return err
		}
		if events[i].SchemaVersion > 0xFFFF {
			return errs.New(errs.CodeOverflow, "reality: schema version exceeds UInt16")
		}
	}
	batch, err := s.conn.PrepareBatch(ctx, "INSERT INTO "+s.Table("normalized_events")+" ("+normalizedColumns+")")
	if err != nil {
		return errs.Wrap(err, errs.CodeProviderUnavailable, "reality: prepare clickhouse batch")
	}
	for _, e := range events {
		ts := e.Timestamps.UTC()
		if err := batch.Append(
			e.EventID, e.DedupID, uint16(e.SchemaVersion), e.Source, e.EventType, e.Sequence, e.SourcePartition, e.SourceOffset, //nolint:gosec // G115: bounded above
			ts.SourceEventAt, nilIfZero(ts.ProviderPublishedAt), ts.PlatformReceivedAt, ts.NormalizedAt, ts.FeatureAvailableAt, ts.DecisionAvailableAt,
			nilIfEmpty(e.InstrumentID), nilIfEmpty(e.AssetID), e.Wallet, e.RawObjectID, e.RawObjectHash, string(e.Payload),
		); err != nil {
			_ = batch.Abort()
			return errs.Wrap(err, errs.CodeValidationFailed, "reality: append normalized event")
		}
	}
	if err := batch.Send(); err != nil {
		return errs.Wrap(err, errs.CodeProviderUnavailable, "reality: send clickhouse batch")
	}
	return nil
}

// HistoricalQuery selects normalized events knowable at AsOf.
type HistoricalQuery struct {
	Source    string
	EventType string
	Wallet    string
	// From bounds decision_available_at from below (optional).
	From time.Time
	// AsOf is the decision instant: only rows with decision_available_at <=
	// AsOf are returned. Required.
	AsOf  time.Time
	Limit int
}

func queryLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultQueryLimit
	case n > MaxQueryLimit:
		return MaxQueryLimit
	}
	return n
}

// QueryNormalized returns events ordered by decision_available_at, dedup_id
// with duplicates collapsed (FINAL). The as-of predicate is never optional:
// a historical read cannot see what was not yet decision-available (PART
// 226).
func (s *ClickHouseStore) QueryNormalized(ctx context.Context, q HistoricalQuery) ([]NormalizedEvent, error) {
	if q.AsOf.IsZero() {
		return nil, errs.New(errs.CodeValidationFailed, "reality: as_of is required for a historical query")
	}
	where := []string{"decision_available_at <= @as_of"}
	args := []any{clickhouse.DateNamed("as_of", q.AsOf.UTC(), clickhouse.NanoSeconds)}
	if q.Source != "" {
		where = append(where, "source = @source")
		args = append(args, clickhouse.Named("source", q.Source))
	}
	if q.EventType != "" {
		where = append(where, "event_type = @event_type")
		args = append(args, clickhouse.Named("event_type", q.EventType))
	}
	if q.Wallet != "" {
		where = append(where, "wallet = @wallet")
		args = append(args, clickhouse.Named("wallet", q.Wallet))
	}
	if !q.From.IsZero() {
		where = append(where, "decision_available_at >= @from")
		args = append(args, clickhouse.DateNamed("from", q.From.UTC(), clickhouse.NanoSeconds))
	}
	args = append(args, clickhouse.Named("limit", queryLimit(q.Limit)))
	sql := "SELECT " + normalizedColumns + " FROM " + s.Table("normalized_events") + " FINAL WHERE " + strings.Join(where, " AND ") +
		" ORDER BY decision_available_at, dedup_id LIMIT @limit"
	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "reality: query normalized events")
	}
	defer func() { _ = rows.Close() }()
	var out []NormalizedEvent
	for rows.Next() {
		var e NormalizedEvent
		var schema uint16
		var seq *uint64
		var published *time.Time
		var instrument, asset *string
		var hash []byte
		var payload string
		if err := rows.Scan(&e.EventID, &e.DedupID, &schema, &e.Source, &e.EventType, &seq, &e.SourcePartition, &e.SourceOffset,
			&e.Timestamps.SourceEventAt, &published, &e.Timestamps.PlatformReceivedAt, &e.Timestamps.NormalizedAt, &e.Timestamps.FeatureAvailableAt, &e.Timestamps.DecisionAvailableAt,
			&instrument, &asset, &e.Wallet, &e.RawObjectID, &hash, &payload); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "reality: scan normalized event")
		}
		e.SchemaVersion, e.Sequence = int(schema), seq
		if published != nil {
			e.Timestamps.ProviderPublishedAt = published.UTC()
		}
		e.Timestamps = e.Timestamps.UTC()
		e.InstrumentID, e.AssetID = deref(instrument), deref(asset)
		e.RawObjectHash = hash
		e.Payload = []byte(payload)
		if e.Timestamps.DecisionAvailableAt.After(q.AsOf) {
			return nil, errs.New(errs.CodeInternal, "reality: store returned a row after as_of; refusing to leak").WithField("event_id", e.EventID)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "reality: query normalized events")
	}
	return out, nil
}

// MarketPrice is one market_prices row: price = mantissa × 10^-scale quote
// base units per one base unit, exact.
type MarketPrice struct {
	InstrumentID        string
	Source              string
	PriceMantissa       money.Quantity
	PriceScale          uint8
	QuoteAssetID        string
	LiquidityMantissa   *money.Quantity
	LiquidityScale      uint8
	SourceEventAt       time.Time
	PlatformReceivedAt  time.Time
	DecisionAvailableAt time.Time
	RawObjectID         string
	DedupID             string
}

// maxInt128 bounds Int128 columns.
var (
	maxInt128 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 127), big.NewInt(1))
	minInt128 = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 127))
)

func fitsInt128(q money.Quantity) bool {
	b := q.BigInt()
	return b.Cmp(maxInt128) <= 0 && b.Cmp(minInt128) >= 0
}

// Validate checks the row.
func (p MarketPrice) Validate() error {
	fields := map[string]any{}
	for name, v := range map[string]string{"instrument_id": p.InstrumentID, "quote_asset_id": p.QuoteAssetID, "raw_object_id": p.RawObjectID} {
		if _, err := id.ParseAny(v); err != nil {
			fields[name] = "must be a canonical uuid"
		}
	}
	if p.Source == "" {
		fields["source"] = "required"
	}
	if err := validateDedupID(p.DedupID); err != nil {
		fields["dedup_id"] = err.Error()
	}
	if !fitsInt128(p.PriceMantissa) {
		fields["price_mantissa"] = "does not fit Int128"
	}
	if p.LiquidityMantissa != nil && !fitsInt128(*p.LiquidityMantissa) {
		fields["liquidity_mantissa"] = "does not fit Int128"
	}
	if p.SourceEventAt.IsZero() || p.PlatformReceivedAt.IsZero() || p.DecisionAvailableAt.IsZero() {
		fields["timestamps"] = "source_event_at, platform_received_at and decision_available_at are required"
	} else if p.DecisionAvailableAt.Before(p.PlatformReceivedAt) {
		fields["decision_available_at"] = "must not precede platform_received_at"
	}
	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "reality: invalid market price").WithFields(fields)
	}
	return nil
}

const priceColumns = `instrument_id, source, price_mantissa, price_scale, quote_asset_id, liquidity_mantissa, liquidity_scale,
	source_event_at, platform_received_at, decision_available_at, raw_object_id, dedup_id`

// InsertMarketPrices writes prices in one batch with exact Int128 mantissas.
func (s *ClickHouseStore) InsertMarketPrices(ctx context.Context, prices []MarketPrice) error {
	if len(prices) == 0 {
		return nil
	}
	for i := range prices {
		if err := prices[i].Validate(); err != nil {
			return err
		}
	}
	batch, err := s.conn.PrepareBatch(ctx, "INSERT INTO "+s.Table("market_prices")+" ("+priceColumns+")")
	if err != nil {
		return errs.Wrap(err, errs.CodeProviderUnavailable, "reality: prepare clickhouse batch")
	}
	for _, p := range prices {
		var liq *big.Int
		if p.LiquidityMantissa != nil {
			liq = p.LiquidityMantissa.BigInt()
		}
		if err := batch.Append(p.InstrumentID, p.Source, p.PriceMantissa.BigInt(), p.PriceScale, p.QuoteAssetID, liq, p.LiquidityScale,
			p.SourceEventAt.UTC(), p.PlatformReceivedAt.UTC(), p.DecisionAvailableAt.UTC(), p.RawObjectID, p.DedupID); err != nil {
			_ = batch.Abort()
			return errs.Wrap(err, errs.CodeValidationFailed, "reality: append market price")
		}
	}
	if err := batch.Send(); err != nil {
		return errs.Wrap(err, errs.CodeProviderUnavailable, "reality: send clickhouse batch")
	}
	return nil
}

// PriceQuery selects prices knowable at AsOf.
type PriceQuery struct {
	InstrumentID string
	Source       string
	From         time.Time
	AsOf         time.Time
	Limit        int
}

// QueryMarketPrices returns prices with decision_available_at <= AsOf,
// newest first.
func (s *ClickHouseStore) QueryMarketPrices(ctx context.Context, q PriceQuery) ([]MarketPrice, error) {
	if q.AsOf.IsZero() {
		return nil, errs.New(errs.CodeValidationFailed, "reality: as_of is required for a historical query")
	}
	if _, err := id.ParseAny(q.InstrumentID); err != nil {
		return nil, errs.New(errs.CodeValidationFailed, "reality: instrument id must be a canonical uuid")
	}
	where := []string{"instrument_id = @instrument", "decision_available_at <= @as_of"}
	args := []any{clickhouse.Named("instrument", q.InstrumentID), clickhouse.DateNamed("as_of", q.AsOf.UTC(), clickhouse.NanoSeconds)}
	if q.Source != "" {
		where = append(where, "source = @source")
		args = append(args, clickhouse.Named("source", q.Source))
	}
	if !q.From.IsZero() {
		where = append(where, "decision_available_at >= @from")
		args = append(args, clickhouse.DateNamed("from", q.From.UTC(), clickhouse.NanoSeconds))
	}
	args = append(args, clickhouse.Named("limit", queryLimit(q.Limit)))
	sql := "SELECT " + priceColumns + " FROM " + s.Table("market_prices") + " FINAL WHERE " + strings.Join(where, " AND ") +
		" ORDER BY decision_available_at DESC, dedup_id LIMIT @limit"
	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "reality: query market prices")
	}
	defer func() { _ = rows.Close() }()
	var out []MarketPrice
	for rows.Next() {
		var p MarketPrice
		var mantissa big.Int
		var liq *big.Int
		if err := rows.Scan(&p.InstrumentID, &p.Source, &mantissa, &p.PriceScale, &p.QuoteAssetID, &liq, &p.LiquidityScale,
			&p.SourceEventAt, &p.PlatformReceivedAt, &p.DecisionAvailableAt, &p.RawObjectID, &p.DedupID); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "reality: scan market price")
		}
		p.PriceMantissa = quantityFromBig(&mantissa)
		if liq != nil {
			l := quantityFromBig(liq)
			p.LiquidityMantissa = &l
		}
		p.SourceEventAt, p.PlatformReceivedAt, p.DecisionAvailableAt = p.SourceEventAt.UTC(), p.PlatformReceivedAt.UTC(), p.DecisionAvailableAt.UTC()
		if p.DecisionAvailableAt.After(q.AsOf) {
			return nil, errs.New(errs.CodeInternal, "reality: store returned a price after as_of; refusing to leak")
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "reality: query market prices")
	}
	return out, nil
}

// LatestPrice is the newest price knowable at asOf (found == false when
// none).
func (s *ClickHouseStore) LatestPrice(ctx context.Context, instrumentID, source string, asOf time.Time) (MarketPrice, bool, error) {
	ps, err := s.QueryMarketPrices(ctx, PriceQuery{InstrumentID: instrumentID, Source: source, AsOf: asOf, Limit: 1})
	if err != nil || len(ps) == 0 {
		return MarketPrice{}, false, err
	}
	return ps[0], true, nil
}

func quantityFromBig(b *big.Int) money.Quantity {
	q, err := money.ParseQuantity(b.String())
	if err != nil {
		// A big.Int always renders as an optionally signed digit string.
		return money.QuantityFromInt64(0)
	}
	return q
}
