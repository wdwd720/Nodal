// Command api is the HTTP composition root of the control plane: it loads and
// validates configuration, opens the database, constructs every dependency
// explicitly, mounts the generated v1 router from internal/httpapi, serves it
// with bounded timeouts, and shuts down gracefully.
//
// It contains wiring and nothing else. Every financial rule, every state
// machine and every authority check lives in the internal packages this file
// assembles.
//
// Configuration is internal/config (CP_* variables). A handful of variables
// belong to this binary alone and are read here, the same way the workers read
// theirs:
//
//	CP_API_SETTLEMENT_CHAIN     chain of the USD-pegged settlement asset (e.g. "solana")
//	CP_API_SETTLEMENT_MINT      mint address of that asset; (chain, mint) is its identity
//	CP_API_FUNDING_NETWORK      provider network name for deposits (default "solana")
//	CP_API_FUNDING_CURRENCY     provider currency name for deposits (default "usdc")
//	CP_API_ENABLED_CAPABILITIES comma-separated capabilities this deployment's
//	                            configuration permits. This is condition 1 of the
//	                            five-condition gate check and can never activate a
//	                            capability on its own: the persisted, dual-approved
//	                            gate row decides (PART 54).
//	CP_API_SHUTDOWN_TIMEOUT     bound on draining in-flight requests (default 25s)
//	CP_API_REQUEST_TIMEOUT      per-request deadline for non-streaming routes
//	                            (default: the configured HTTP write timeout)
//
// Transport rate limits (PART 180), each "<requests>/<window>" such as
// "600/1m", or "off" to disable — which STAGING and PROD refuse:
//
//	CP_API_RATE_LIMIT_GENERAL   every authenticated request     (default 600/1m)
//	CP_API_RATE_LIMIT_AUTH      the login endpoints             (default 30/1m)
//	CP_API_RATE_LIMIT_QUOTE     quote previews                  (default 120/1m)
//	CP_API_RATE_LIMIT_COMMAND   every mutating endpoint         (default 120/1m)
//
// In LOCAL, TEST and DEV with CP_AUTH_MODE=dev the development identity
// picker is mounted at /auth/dev/login, so a browser (or a load-test script)
// can complete a real session: GET /v1/auth/login, follow the redirect, choose
// an identity, and the callback sets the session cookie. The picker cannot be
// mounted anywhere else — see cmd/api/devlogin.go.
//
// A dev session is always a CUSTOMER unless the user holds a row in
// operator_roles: internal/identity takes roles from the operator directory
// and never from identity-provider claims, dev claims included. `make seed`
// grants ADMIN to the "admin" dev identity, which is what the operator-facing
// load scenarios need.
//
// Exit codes: 0 clean shutdown, 1 startup or runtime failure.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/observability"
)

const (
	exitOK      = 0
	exitFailure = 1

	envSettlementChain     = "CP_API_SETTLEMENT_CHAIN"
	envSettlementMint      = "CP_API_SETTLEMENT_MINT"
	envFundingNetwork      = "CP_API_FUNDING_NETWORK"
	envFundingCurrency     = "CP_API_FUNDING_CURRENCY"
	envEnabledCapabilities = "CP_API_ENABLED_CAPABILITIES"
	envShutdownTimeout     = "CP_API_SHUTDOWN_TIMEOUT"
	envRequestTimeout      = "CP_API_REQUEST_TIMEOUT"

	// envLegalPolicy selects the Settlement Compiler's legal policy.
	// CONSERVATIVE (the default, and the only value a production-like
	// environment accepts) permits simulation and denies every internal
	// economy product. DEVELOPMENT permits the internal economy so it can be
	// exercised locally; it is refused outside LOCAL, DEV and TEST.
	envLegalPolicy      = "CP_API_LEGAL_POLICY"
	envRateLimitGeneral = "CP_API_RATE_LIMIT_GENERAL"
	envRateLimitAuth    = "CP_API_RATE_LIMIT_AUTH"
	envRateLimitQuote   = "CP_API_RATE_LIMIT_QUOTE"
	envRateLimitCommand = "CP_API_RATE_LIMIT_COMMAND"

	defaultShutdownTimeout = 25 * time.Second
	defaultFundingNetwork  = "solana"
	defaultFundingCurrency = "usdc"
)

func main() {
	// SIGTERM is how a container orchestrator asks for a graceful stop; the
	// context it cancels is the only shutdown trigger, so run stays testable.
	// stop is released before os.Exit, which no deferred call would survive.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.LookupEnv, os.Stderr)
	stop()
	os.Exit(code)
}

// run serves until ctx is cancelled, then drains. It returns the process exit
// code and never calls os.Exit itself.
func run(ctx context.Context, lookup func(string) (string, bool), stderr *os.File) int {
	cfg, err := config.Load(ctx, config.ServiceAPI, lookup)
	if err != nil {
		fmt.Fprintln(stderr, "api: configuration:", err)
		return exitFailure
	}
	// Belt and braces: config.Validate already refuses provider mode "fake"
	// in STAGING and PROD (RuleNoFakeProviders). Re-check here so the binary
	// itself, and not only the loader, refuses to run with fakes where real
	// money is at stake.
	if err := refuseFakeProviders(cfg); err != nil {
		fmt.Fprintln(stderr, "api:", err)
		return exitFailure
	}

	log := observability.NewLogger(cfg.Env, stderr)
	// The root context carries the logger, so a background pass that logs
	// through LoggerFrom -- the reconciliation engine raising an alert, the
	// settlement sweep parking a funding -- writes through the process logger
	// rather than LoggerFrom's slog.Default fallback. Requests already get
	// this from the middleware; the tickers had nothing giving it to them.
	ctx = observability.WithLogger(ctx, log)

	shutdownTelemetry, err := observability.Setup(ctx, cfg.Telemetry, cfg.ServiceName, config.BuildVersion, cfg.Env)
	if err != nil {
		log.Error("telemetry setup failed", "error", err.Error())
		return exitFailure
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(sctx); err != nil {
			log.Warn("telemetry shutdown failed", "error", err.Error())
		}
	}()

	resolver := config.NewResolver(cfg.Env, lookup)
	dsn, err := resolver.Resolve(ctx, cfg.Database.AppURL)
	if err != nil {
		log.Error("database url could not be resolved", "error", err.Error())
		return exitFailure
	}
	database, err := db.Open(ctx, db.Config{
		URL:              dsn,
		MaxConns:         cfg.Database.MaxConns,
		MinConns:         cfg.Database.MinConns,
		AppName:          cfg.ServiceName + "-api",
		RequireTLS:       cfg.Database.RequireTLS,
		StatementTimeout: cfg.Database.StatementTimeout,
		LockTimeout:      cfg.Database.LockTimeout,
	})
	if err != nil {
		log.Error("database could not be opened", "error", err.Error())
		return exitFailure
	}
	defer database.Close()

	// Where the transport rate limiter keeps its counters. Opened here rather
	// than inside build, for the same reason the database is: it is a
	// connection pool with a process lifetime and it has to be closed when the
	// process stops. A distributed backend that does not answer stops startup
	// -- in production the limit is a control, and a control that is not there
	// must not be reported as one.
	rlStore, rlFailOpen, closeRateLimitStore, err := newRateLimitStore(ctx, cfg, resolver, log)
	if err != nil {
		log.Error("rate limit store could not be opened", "error", err.Error())
		return exitFailure
	}
	defer closeRateLimitStore()

	// Process-local counters are correct for exactly one process, and until now
	// that was a number an operator typed rather than a fact anything checked --
	// `render.yaml` sets no numInstances, so the dashboard is authoritative
	// (F-93). An advisory lock makes it something this process verified.
	//
	// Only for the memory backend: a shared Redis store is what makes several
	// instances correct, so locking there would refuse a topology that works.
	if cfg.RateLimit.Backend == config.RateLimitMemory {
		release, lerr := holdSingleInstanceLock(ctx, database, log)
		if lerr != nil {
			log.Error("refusing to start", "error", lerr.Error())
			return exitFailure
		}
		defer release()
	}

	// Where a raised alert goes when it leaves the process (F-118).
	//
	// Opened here rather than in build because it owns a goroutine and a queue
	// that must be drained on shutdown -- the same reason the database and the
	// rate-limit store are opened here. Closed AFTER the server stops, below,
	// so an alert raised by the last request in flight still gets delivered.
	alerts := newAlertDispatcher(ctx, cfg, resolver, log)

	clk := clock.System()
	server, err := build(ctx, buildInput{
		cfg:               cfg,
		lookup:            lookup,
		resolver:          resolver,
		database:          database,
		clock:             clk,
		logger:            log,
		rateLimitStore:    rlStore,
		rateLimitFailOpen: rlFailOpen,
		alerts:            alerts,
	})
	if err != nil {
		log.Error("composition failed", "error", err.Error())
		return exitFailure
	}

	requestTimeout := cfg.API.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = cfg.HTTP.WriteTimeout
	}
	handler := withRequestTimeout(server, requestTimeout)

	httpServer := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		// WriteTimeout stays in force for ordinary requests; the SSE handler
		// clears its own write deadline so a stream is not cut off.
		WriteTimeout:   cfg.HTTP.WriteTimeout,
		IdleTimeout:    cfg.HTTP.IdleTimeout,
		MaxHeaderBytes: 1 << 20,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("api listening",
			"addr", cfg.HTTP.Addr,
			"env", string(cfg.Env),
			"build_version", config.BuildVersion,
			"config_hash", cfg.Hash(),
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			log.Error("http server failed", "error", err.Error())
			return exitFailure
		}
		return exitOK
	case <-ctx.Done():
	}

	// Graceful shutdown. Readiness flips to "not ready" and every open
	// server-sent-events connection is ended first, because a stream never
	// completes on its own and would otherwise consume the whole drain
	// window. Then in-flight requests are drained.
	//
	// Nothing here interrupts a database transaction. Shutdown waits for
	// handlers to return, so a transaction either commits or its own
	// context ends and PostgreSQL rolls it back; there is no path that
	// leaves a half-applied financial change.
	shutdownTimeout := cfg.API.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	log.Info("api draining", "timeout", shutdownTimeout.String())
	server.StopStreams()

	drainCtx, cancel := context.WithTimeout(context.Background(),
		shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(drainCtx); err != nil {
		// The drain window expired with requests still running. Closing the
		// listener and its connections aborts them at the transport; any
		// open transaction is rolled back by PostgreSQL when its connection
		// goes away, never partially committed.
		log.Warn("drain window expired; closing remaining connections", "error", err.Error())
		if cerr := httpServer.Close(); cerr != nil {
			log.Warn("close failed", "error", cerr.Error())
		}
		<-serveErr
		// Drained even on the unclean path: an alert raised by a request that
		// was aborted is exactly the one worth delivering.
		alerts.Close()
		return exitFailure
	}
	<-serveErr
	// After the server has stopped, so an alert raised by the last request in
	// flight is delivered rather than discarded.
	alerts.Close()
	log.Info("api stopped")
	return exitOK
}

// refuseFakeProviders is the production guard of PART 97: a binary that could
// run with in-process test doubles must never start where real money is at
// stake, whatever the loader was told.
func refuseFakeProviders(cfg *config.Config) error {
	if !cfg.Env.IsProductionLike() {
		return nil
	}
	var offenders []string
	for name, p := range providerSlots(cfg) {
		if p.Mode == config.ProviderModeFake {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) == 0 {
		return nil
	}
	return fmt.Errorf("%s: provider mode %q is refused in %s (slots: %s)",
		config.RuleNoFakeProviders, config.ProviderModeFake, cfg.Env, strings.Join(sorted(offenders), ", "))
}

func providerSlots(cfg *config.Config) map[string]config.ProviderConfig {
	return map[string]config.ProviderConfig{
		"funding":                 cfg.Providers.Funding,
		"wallet":                  cfg.Providers.Wallet,
		"signing":                 cfg.Providers.Signing,
		"execution":               cfg.Providers.Execution,
		"market_data":             cfg.Providers.MarketData,
		"chain_observer":          cfg.Providers.ChainObserver,
		"chain_observer_fallback": cfg.Providers.ChainObserverFallback,
		"model":                   cfg.Providers.Model,
		"event_bus":               cfg.Providers.EventBus,
		"workflow":                cfg.Providers.Workflow,
		"archive":                 cfg.Providers.Archive,
		"notification":            cfg.Providers.Notification,
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// withRequestTimeout bounds every non-streaming request. A timeout here is a
// transport fact: the handler sees a cancelled context and answers
// PROVIDER_UNAVAILABLE, never "rejected".
func withRequestTimeout(next http.Handler, d time.Duration) http.Handler {
	if d <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events/stream") {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
