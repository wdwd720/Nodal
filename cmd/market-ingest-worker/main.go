// Command market-ingest-worker is the reality engine's ingest process
// (STAGE 11, PARTS 74-79, 120, 124, 199, 200;
// docs/architecture/POINT_IN_TIME.md): provider stream -> raw archive ->
// normalizer -> Redpanda -> ClickHouse, with ingest checkpoints, gap
// detection, provider health sampling and scheduled archive verification.
//
// Usage:
//
//	market-ingest-worker run       ingest until SIGINT/SIGTERM
//	market-ingest-worker schema    apply the ClickHouse DDL (Appendix A) and exit
//	market-ingest-worker sources   register the configured data source if absent; print the registry
//	market-ingest-worker gaps      print open gaps and continuity over the window; exit 1 when blocked
//	market-ingest-worker verify    re-hash archived raw objects over the window; exit 1 on any violation
//
// It writes no financial truth. Its only database writes are ingest state:
// data_sources, raw_archive_objects, ingest_checkpoints, stream_gaps and
// provider_health_samples (docs/architecture/SYSTEM.md, SECURITY.md). Nothing
// it writes to ClickHouse is ever read as a balance.
//
// Knowledge time is set here and nowhere else. platform_received_at is our
// clock at first byte; the normalizer stamps normalized_at; the availability
// policy adds the feature and pipeline latencies to reach
// decision_available_at. Provider clocks are stored beside ours and never
// drive that arithmetic, so a provider clock running ahead can never make a
// datum look knowable earlier than it was.
//
// Configuration is internal/config (CP_*) plus the worker variables below.
// Sensible defaults exist for every one except the wallets, which `run`
// requires: this process never invents a subscription list.
//
//	CP_INGEST_DATA_SOURCE       data_sources.code to ingest under (default helius.wallet_events)
//	CP_INGEST_WALLETS           comma-separated wallet addresses to stream (required by run)
//	CP_INGEST_CONSUMER          ingest_checkpoints.consumer (default market-ingest-worker)
//	CP_INGEST_TOPIC             bus topic for normalized events (default reality.normalized.wallet_events)
//	CP_INGEST_FEATURE_LATENCY   normalized_at -> feature_available_at (default 0s)
//	CP_INGEST_PIPELINE_LATENCY  tools.pipeline_latency_ms (default 250ms)
//	CP_INGEST_HEARTBEAT         data_sources.heartbeat_timeout_ms for a new registration (default 2m)
//	CP_INGEST_SILENCE_INTERVAL  how often SILENCE is evaluated (default 5s)
//	CP_INGEST_HEALTH_INTERVAL   provider_health_samples period (default 30s)
//	CP_INGEST_VERIFY_INTERVAL   archive verification period during run (default 1h; 0 disables)
//	CP_INGEST_WINDOW            window verify/gaps look back over (default 24h)
//	CP_INGEST_VERIFY_LIMIT      objects per verification sweep (default 1000)
//
// Exit codes: 0 success, 1 failure (verify: an integrity violation; gaps: a
// blocked window), 2 usage error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/helius"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
	"github.com/nodal/controlplane/internal/reality"
	"github.com/nodal/controlplane/internal/reality/redpandabus"
)

// Worker-specific variables (everything else is internal/config).
const (
	envDataSource       = "CP_INGEST_DATA_SOURCE"
	envWallets          = "CP_INGEST_WALLETS"
	envConsumer         = "CP_INGEST_CONSUMER"
	envTopic            = "CP_INGEST_TOPIC"
	envFeatureLatency   = "CP_INGEST_FEATURE_LATENCY"
	envPipelineLatency  = "CP_INGEST_PIPELINE_LATENCY"
	envHeartbeat        = "CP_INGEST_HEARTBEAT"
	envSilenceInterval  = "CP_INGEST_SILENCE_INTERVAL"
	envHealthInterval   = "CP_INGEST_HEALTH_INTERVAL"
	envVerifyInterval   = "CP_INGEST_VERIFY_INTERVAL"
	envWindow           = "CP_INGEST_WINDOW"
	envVerifyLimit      = "CP_INGEST_VERIFY_LIMIT"
	defaultDataSource   = "helius.wallet_events"
	defaultConsumer     = "market-ingest-worker"
	defaultTopic        = "reality.normalized.wallet_events"
	defaultPipelineLat  = 250 * time.Millisecond
	defaultHeartbeat    = 2 * time.Minute
	defaultSilenceEvery = 5 * time.Second
	defaultHealthEvery  = 30 * time.Second
	defaultVerifyEvery  = time.Hour
	defaultWindow       = 24 * time.Hour

	// availabilityPolicyVersion names the latency policy in evidence. Bump
	// it whenever the meaning of the latencies changes, never when their
	// configured values do.
	availabilityPolicyVersion = "reality.availability.v1"

	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

// deps is everything a subcommand needs. It is built once by wire.
type deps struct {
	cfg      *config.Config
	db       *db.DB
	clk      clock.Clock
	log      *slog.Logger
	lookup   func(string) (string, bool)
	resolver config.Resolver
	raw      *reality.RawArchive
	source   reality.DataSource
}

func run(args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	case "run", "schema", "sources", "gaps", "verify":
	default:
		fmt.Fprintf(stderr, "market-ingest-worker: unknown command %q\n", cmd)
		usage(stderr)
		return exitUsage
	}
	if len(rest) != 0 {
		fmt.Fprintf(stderr, "market-ingest-worker: %s takes no arguments\n", cmd)
		usage(stderr)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// schema touches ClickHouse only: it must work before the database has
	// any ingest state, which is exactly when an operator needs it.
	if cmd == "schema" {
		if err := cmdSchema(ctx, lookup, stdout, stderr); err != nil {
			fmt.Fprintln(stderr, "market-ingest-worker:", err)
			return exitFailure
		}
		return exitOK
	}

	d, err := wire(ctx, lookup, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "market-ingest-worker:", err)
		return exitFailure
	}
	defer d.db.Close()

	var clean bool
	switch cmd {
	case "run":
		err = cmdRun(ctx, d)
	case "sources":
		err = cmdSources(ctx, d, stdout)
	case "gaps":
		clean, err = cmdGaps(ctx, d, stdout)
	case "verify":
		clean, err = cmdVerify(ctx, d, stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, "market-ingest-worker:", err)
		return exitFailure
	}
	if (cmd == "gaps" || cmd == "verify") && !clean {
		return exitFailure
	}
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: market-ingest-worker <command>

  run       provider stream -> raw archive -> normalizer -> bus -> ClickHouse,
            with checkpoints, gap detection, health sampling (%s, default %s)
            and archive verification (%s, default %s), until SIGINT/SIGTERM
  schema    apply the ClickHouse DDL (POINT_IN_TIME.md Appendix A) and exit
  sources   register %s if absent; print the data source registry
  gaps      open gaps and continuity over the last %s (default %s); exit 1 when the window is blocked
  verify    re-hash archived raw objects over the same window; exit 1 on any integrity violation

env: CP_* (see internal/config) plus %s, %s, %s, %s, %s, %s, %s, %s
`, envHealthInterval, defaultHealthEvery, envVerifyInterval, defaultVerifyEvery,
		envDataSource, envWindow, defaultWindow,
		envDataSource, envWallets, envConsumer, envTopic, envFeatureLatency, envPipelineLatency, envHeartbeat, envVerifyLimit)
}

// loadConfig loads CP_* configuration, defaulting the environment to LOCAL
// with a warning so a developer shell is usable without ceremony while
// STAGING and PROD still have to say what they are.
func loadConfig(ctx context.Context, lookup func(string) (string, bool), stderr io.Writer) (*config.Config, func(string) (string, bool), error) {
	if v, ok := lookup(config.EnvVarEnvironment); !ok || strings.TrimSpace(v) == "" {
		fmt.Fprintf(stderr, "market-ingest-worker: WARNING %s is not set; assuming LOCAL\n", config.EnvVarEnvironment)
		inner := lookup
		lookup = func(k string) (string, bool) {
			if k == config.EnvVarEnvironment {
				return string(config.EnvLocal), true
			}
			return inner(k)
		}
	}
	cfg, err := config.Load(ctx, config.ServiceMarketIngestWorker, lookup)
	if err != nil {
		return nil, nil, err
	}
	return cfg, lookup, nil
}

func wire(ctx context.Context, lookup func(string) (string, bool), stderr io.Writer) (*deps, error) {
	cfg, lookup, err := loadConfig(ctx, lookup, stderr)
	if err != nil {
		return nil, err
	}
	log := observability.NewLogger(cfg.Env, stderr).
		With("service", "market-ingest-worker", "build_version", cfg.BuildVersion, "env", cfg.Env)
	resolver := config.NewResolver(cfg.Env, lookup)
	dbURL, err := resolver.Resolve(ctx, cfg.Database.AppURL)
	if err != nil {
		return nil, fmt.Errorf("resolve database url: %w", err)
	}
	pool, err := db.Open(ctx, db.Config{
		URL: dbURL, AppName: "market-ingest-worker", RequireTLS: cfg.Database.RequireTLS,
		MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns,
		StatementTimeout: cfg.Database.StatementTimeout, LockTimeout: cfg.Database.LockTimeout,
	})
	if err != nil {
		return nil, err
	}
	d := &deps{cfg: cfg, db: pool, clk: clock.System(), log: log, lookup: lookup, resolver: resolver}

	store, err := archive.NewS3(ctx, cfg.Archive, resolver, archive.S3Options{Clock: d.clk})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("object archive: %w", err)
	}
	if d.raw, err = reality.NewRawArchive(store, cfg.Archive, pool, d.clk, log); err != nil {
		pool.Close()
		return nil, err
	}
	if d.source, err = d.ensureSource(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return d, nil
}

// ensureSource registers the configured data source when it does not exist
// yet. The default registration is deliberately restrictive: historical use
// UNKNOWN, persistence BLOCKED (PART 120). Widening those is an operator
// decision recorded against a license, never something a worker infers from
// the fact that data is flowing.
func (d *deps) ensureSource(ctx context.Context) (reality.DataSource, error) {
	code := stringVar(d.lookup, envDataSource, defaultDataSource)
	providerName := d.cfg.Providers.ChainObserver.Name
	if providerName == "" {
		providerName = helius.ProviderName
	}
	want := reality.DefaultChainDataSource(code, providerName,
		d.cfg.Retention.RawMarketDataDays, durationVar(d.lookup, envHeartbeat, defaultHeartbeat), "market-ingest-worker")
	ds, err := reality.PgDataSourceStore{}.EnsureRegistered(ctx, d.db, want)
	if err != nil {
		return reality.DataSource{}, fmt.Errorf("data source %s: %w", code, err)
	}
	if ds.Status == reality.SourceDisabled {
		return reality.DataSource{}, fmt.Errorf("data source %s is DISABLED", ds.Code)
	}
	if !ds.AllowedIn(string(d.cfg.Env)) {
		return reality.DataSource{}, fmt.Errorf("data source %s is not allowed in %s", ds.Code, d.cfg.Env)
	}
	return ds, nil
}

// availabilityPolicy is the one place decision_available_at latencies are
// set for this process.
func (d *deps) availabilityPolicy() (reality.AvailabilityPolicy, error) {
	p := reality.AvailabilityPolicy{
		FeatureLatency:  durationVar(d.lookup, envFeatureLatency, 0),
		PipelineLatency: durationVar(d.lookup, envPipelineLatency, defaultPipelineLat),
		Version:         availabilityPolicyVersion,
	}
	return p, p.Validate()
}

// window is the interval the window-scoped commands report over.
func (d *deps) window(now time.Time) reality.Window {
	return reality.Window{Start: now.Add(-durationVar(d.lookup, envWindow, defaultWindow)).UTC(), End: now.UTC()}
}

// openClickHouse opens the analytics store with the RAW_MARKET_DATA
// retention as the normalized_events TTL: the store never keeps history
// longer than the class it holds is configured for.
func openClickHouse(ctx context.Context, cfg *config.Config, resolver config.Resolver) (*reality.ClickHouseStore, error) {
	s, err := reality.NewClickHouseStore(ctx, cfg.ClickHouse, resolver, reality.ClickHouseOptions{TTLDays: cfg.Retention.RawMarketDataDays})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	return s, nil
}

// openBus returns the normalized-event transport. Fake mode gets the
// in-process loopback (refused outside LOCAL/TEST/DEV); every other mode gets
// the franz-go client, which does not return until a broker has answered.
//
// redpandabus.Open never substitutes one for the other, and neither does
// this: if the broker is unreachable the worker fails to start. Downgrading
// to the loopback here would produce a process that logs healthy ingest
// while every normalized event is published into a buffer nothing reads.
func (d *deps) openBus(ctx context.Context) (event.Bus, error) {
	bus, err := redpandabus.Open(ctx, d.cfg.Env, d.cfg.Providers.EventBus.Mode, d.cfg.Redpanda, d.resolver,
		redpandabus.Options{ClientID: "market-ingest-worker", Logger: d.log})
	if err != nil {
		return nil, fmt.Errorf("event bus: %w", err)
	}
	return bus, nil
}

// openProvider builds the Solana data provider that feeds the pipeline.
// Every RPC response it returns is archived through the chain archive first,
// so an observation that cannot be evidenced never becomes an event.
func (d *deps) openProvider(ctx context.Context) (chain.SolanaDataProvider, *provider.Tracker, error) {
	pc := d.cfg.Providers.ChainObserver
	if pc.Mode == config.ProviderModeFake {
		// chaintest is test-only and must never be linked into a binary,
		// so there is no fake path here.
		return nil, nil, errors.New("chain observer is in fake mode: set CP_PROVIDER_CHAIN_OBSERVER_MODE=sandbox|live with an API key to ingest")
	}
	chainArchive, err := reality.NewChainArchive(d.raw, d.db, d.source.Code)
	if err != nil {
		return nil, nil, err
	}
	th := provider.DefaultThresholds()
	tracker, err := provider.NewTracker(d.source.Provider, th, d.clk.Now())
	if err != nil {
		return nil, nil, err
	}
	client, err := helius.New(ctx, pc, helius.Options{}, helius.Deps{
		Deps: solanarpc.Deps{
			Resolver: d.resolver, Archive: chainArchive, Tracker: tracker,
			Thresholds: &th, Clock: d.clk, Logger: d.log,
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("chain observer: %w", err)
	}
	return client, tracker, nil
}

func cmdSchema(ctx context.Context, lookup func(string) (string, bool), stdout, stderr io.Writer) error {
	cfg, lookup, err := loadConfig(ctx, lookup, stderr)
	if err != nil {
		return err
	}
	store, err := openClickHouse(ctx, cfg, config.NewResolver(cfg.Env, lookup))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := store.EnsureSchema(ctx); err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{
		"database": cfg.ClickHouse.Database, "addr": cfg.ClickHouse.Addr,
		"normalized_events_ttl_days": cfg.Retention.RawMarketDataDays, "applied": true,
	})
}

func cmdSources(ctx context.Context, d *deps, stdout io.Writer) error {
	list, err := reality.PgDataSourceStore{}.List(ctx, d.db)
	if err != nil {
		return err
	}
	rows := make([]map[string]any, 0, len(list))
	for _, s := range list {
		rows = append(rows, map[string]any{
			"code": s.Code, "provider": s.Provider, "kind": s.Kind, "status": s.Status,
			"retention_class": s.RetentionClass, "retention_days": s.RetentionDays,
			"historical_use_permitted": s.HistoricalUsePermitted, "persistence_capability": s.PersistenceCapability,
			"persistence_allowed": s.PersistenceAllowed(), "dedup_strategy": s.DedupStrategy,
			"heartbeat_timeout": s.HeartbeatTimeout.String(), "supports_replay": s.SupportsReplay,
			"license_ref": s.LicenseRef, "environments": s.Environments,
		})
	}
	return writeJSON(stdout, map[string]any{"ingesting": d.source.Code, "sources": rows})
}

func cmdGaps(ctx context.Context, d *deps, stdout io.Writer) (bool, error) {
	w := d.window(d.clk.Now())
	cont, err := reality.PgCheckpointStore{}.Continuity(ctx, d.db, d.source.Code, w)
	if err != nil {
		return false, err
	}
	gaps, err := reality.PgCheckpointStore{}.OpenGaps(ctx, d.db, d.source.Code, w)
	if err != nil {
		return false, err
	}
	rows := make([]map[string]any, 0, len(gaps))
	for _, g := range gaps {
		rows = append(rows, map[string]any{
			"id": g.ID.String(), "kind": g.Kind, "stream": g.Stream, "partition": g.Partition,
			"resolution": g.Resolution, "gap_start_at": g.GapStartAt, "gap_end_at": nilIfZero(g.GapEndAt),
			"expected_sequence": g.ExpectedSequence, "observed_sequence": g.ObservedSequence, "blocking": g.Blocking(),
		})
	}
	return cont.OK, writeJSON(stdout, map[string]any{
		"data_source": d.source.Code, "from": w.Start, "to": w.End,
		"continuous": cont.OK, "impurity_reasons": cont.ImpurityReasons(), "open_gaps": rows,
	})
}

func cmdVerify(ctx context.Context, d *deps, stdout io.Writer) (bool, error) {
	w := d.window(d.clk.Now())
	report, err := d.raw.VerifySweep(ctx, d.source.Code, w, intVar(d.lookup, envVerifyLimit, reality.DefaultVerifyLimit))
	if err != nil {
		return false, err
	}
	return report.OK(), writeJSON(stdout, report)
}

func cmdRun(ctx context.Context, d *deps) error {
	wallets := listVar(d.lookup, envWallets)
	if len(wallets) == 0 {
		return fmt.Errorf("%s is required: this worker never invents a subscription list", envWallets)
	}
	policy, err := d.availabilityPolicy()
	if err != nil {
		return err
	}
	normalizer, err := reality.NewChainNormalizer(d.source.Code, d.source.DedupStrategy, policy)
	if err != nil {
		return err
	}
	sink, err := openClickHouse(ctx, d.cfg, d.resolver)
	if err != nil {
		return err
	}
	defer func() { _ = sink.Close() }()
	if err := sink.EnsureSchema(ctx); err != nil {
		return err
	}
	bus, err := d.openBus(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = bus.Close(context.WithoutCancel(ctx)) }()
	dataProvider, tracker, err := d.openProvider(ctx)
	if err != nil {
		return err
	}
	src, err := reality.NewSolanaWalletSource(dataProvider, wallets, d.source.Code, d.clk)
	if err != nil {
		return err
	}
	pipeline, err := reality.NewPipeline(reality.PipelineConfig{
		DataSource: d.source.Code,
		Stream:     reality.StreamWalletEvents,
		Consumer:   stringVar(d.lookup, envConsumer, defaultConsumer),
		Topic:      stringVar(d.lookup, envTopic, defaultTopic),
		// Chain streams are keyed by slot, so their sequence is not
		// contiguous: a jump is normal and gaps are found by the
		// reconnect-driven backfill instead (POINT_IN_TIME.md §4).
		Detector:             reality.DetectorOptions{HeartbeatTimeout: d.source.HeartbeatTimeout},
		SilenceCheckInterval: durationVar(d.lookup, envSilenceInterval, defaultSilenceEvery),
	}, reality.PipelineDeps{
		DB: d.db, Archive: d.raw, Normalizer: normalizer, Bus: bus, Sink: sink,
		Source: src, Clock: d.clk, Logger: d.log,
	})
	if err != nil {
		return err
	}

	d.log.InfoContext(ctx, "market ingest worker started",
		slog.String("data_source", d.source.Code), slog.String("provider", d.source.Provider),
		slog.Int("wallets", len(src.Wallets())), slog.Duration("pipeline_latency", policy.PipelineLatency),
		slog.Duration("feature_latency", policy.FeatureLatency), slog.String("policy_version", policy.Version),
		slog.Bool("persistence_allowed", d.source.PersistenceAllowed()))
	if !d.source.PersistenceAllowed() {
		// Not fatal: a BLOCKED source may be read live where licensed. It is
		// loud because history retained under it is not backtest-eligible.
		d.log.WarnContext(ctx, "data source persistence is BLOCKED: history is not usable for backtests until an operator records the license",
			slog.String("data_source", d.source.Code), slog.String("historical_use_permitted", d.source.HistoricalUsePermitted))
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pipeline.Run(ctx) }()
	d.background(ctx, tracker, pipeline)

	err = <-done
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		err = nil
	}
	d.log.InfoContext(context.WithoutCancel(ctx), "market ingest worker stopped", slog.Any("stats", statsAttrs(pipeline.Stats())))
	return err
}

// background samples provider health and verifies archived objects on their
// own schedules until ctx is done. Neither failure stops ingest: losing the
// ability to record health or to re-hash evidence is an operational alarm,
// not a reason to stop archiving what is arriving now.
func (d *deps) background(ctx context.Context, tracker *provider.Tracker, pipeline *reality.Pipeline) {
	sampler, err := reality.NewHealthSampler(provider.DefaultThresholds(), tracker.Health, d.source.ID, d.clk.Now())
	if err != nil {
		d.log.ErrorContext(ctx, "health sampler unavailable", slog.String("error", err.Error()))
		return
	}
	healthEvery := durationVar(d.lookup, envHealthInterval, defaultHealthEvery)
	verifyEvery := durationVar(d.lookup, envVerifyInterval, defaultVerifyEvery)
	limit := intVar(d.lookup, envVerifyLimit, reality.DefaultVerifyLimit)

	go func() {
		t := time.NewTicker(healthEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.sampleHealth(ctx, sampler, pipeline)
			}
		}
	}()
	if verifyEvery <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(verifyEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.verifyArchive(ctx, verifyEvery, limit)
			}
		}
	}()
}

func (d *deps) sampleHealth(ctx context.Context, sampler *reality.HealthSampler, pipeline *reality.Pipeline) {
	now := d.clk.Now()
	// The pipeline's own error counter is the platform-side evidence of the
	// stream's health; the provider tracker contributes the call-level view.
	stats := pipeline.Stats()
	sampler.Observe(now, stats.LastError == "", 0)
	sample, err := sampler.Sample(ctx, d.source.Provider, reality.RoleData, now)
	if err != nil {
		d.log.ErrorContext(ctx, "health sample failed", slog.String("error", err.Error()))
		return
	}
	sample.DataSourceID = d.source.ID
	if err := (reality.PgHealthStore{}).Insert(ctx, d.db, sample); err != nil && ctx.Err() == nil {
		d.log.ErrorContext(ctx, "health sample not recorded", slog.String("code", string(errs.CodeOf(err))))
	}
}

func (d *deps) verifyArchive(ctx context.Context, lookback time.Duration, limit int) {
	w := reality.Window{Start: d.clk.Now().Add(-lookback).UTC(), End: d.clk.Now().UTC()}
	report, err := d.raw.VerifySweep(ctx, d.source.Code, w, limit)
	if err != nil {
		if ctx.Err() == nil {
			d.log.ErrorContext(ctx, "archive verification failed", slog.String("code", string(errs.CodeOf(err))))
		}
		return
	}
	if !report.OK() {
		d.log.ErrorContext(ctx, "archive integrity violation",
			slog.Int("checked", report.Checked), slog.Int("violations", len(report.Violations)))
		return
	}
	d.log.InfoContext(ctx, "archive verified", slog.Int("checked", report.Checked))
}

func statsAttrs(s reality.Stats) map[string]any {
	return map[string]any{
		"ingested": s.Ingested, "duplicates": s.Duplicates, "published": s.Published,
		"findings": s.Findings, "errors": s.Errors, "last_ingest_at": nilIfZero(s.LastIngestAt),
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func nilIfZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func stringVar(lookup func(string) (string, bool), name, def string) string {
	v, ok := lookup(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}

func listVar(lookup func(string) (string, bool), name string) []string {
	v, ok := lookup(name)
	if !ok {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// durationVar reads a duration, falling back to def for anything unparseable
// or negative. Zero is kept when it is written explicitly, so an operator can
// disable an interval.
func durationVar(lookup func(string) (string, bool), name string, def time.Duration) time.Duration {
	v, ok := lookup(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d < 0 {
		return def
	}
	return d
}

func intVar(lookup func(string) (string, bool), name string, def int) int {
	v, ok := lookup(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}
