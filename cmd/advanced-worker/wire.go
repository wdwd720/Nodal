package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/advancedjobs"
	"github.com/nodal/controlplane/internal/advancedmetrics"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/httpapi"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/notification"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxchain"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/transfers"
	"github.com/nodal/controlplane/internal/valuation"
)

// envLegalPolicy is the API's legal policy variable, which the worker reads
// too: both route against the same determination.
const envLegalPolicy = "CP_API_LEGAL_POLICY"

// maxMarkAge is the mark age buying power V2 and the API's valuer accept.
const maxMarkAge = 5 * time.Minute

type buildInput struct {
	cfg      *config.Config
	plan     plan
	resolver config.Resolver
	database *db.DB
	clock    clock.Clock
	logger   *slog.Logger
	meter    metric.Meter
}

// worker is what build wired: the jobs the plan names, the scheduler's health
// observer, and what must be closed at exit.
type worker struct {
	jobs    []advancedjobs.Job
	health  *advancedmetrics.JobHealth
	closers []func(context.Context)
}

func (w *worker) close(ctx context.Context) {
	for i := len(w.closers) - 1; i >= 0; i-- {
		w.closers[i](ctx)
	}
}

// build constructs every service the way cmd/api/wire.go constructs it --
// the same constructors, collaborators, policies and defaults -- and the jobs
// that drive them. It performs no business logic.
func build(ctx context.Context, in buildInput) (_ *worker, err error) {
	cfg, database, clk, log := in.cfg, in.database, in.clock, in.logger
	w := &worker{}
	defer func() {
		if err != nil {
			w.close(context.WithoutCancel(ctx))
		}
	}()
	settings := func(name string) advancedjobs.Settings {
		return advancedjobs.Settings{Interval: cfg.AdvancedWorker.Intervals[name], Batch: cfg.AdvancedWorker.Batches[name]}
	}

	// --- foundations, as cmd/api ------------------------------------------
	outbox := event.NewOutbox(clk)
	auditWriter := audit.NewWriterWithBuildVersion(config.BuildVersion)
	accountRepo := accounts.NewRepository()
	assetRepo := assets.NewRepository()
	instrumentRepo := instruments.NewRepository()
	// No capability resolver: everything the worker's jobs post is a
	// single-domain SIMULATED posting, and a cross-domain conversion is
	// refused without one (fail closed).
	ledgerSvc := ledger.NewService(clk, config.BuildVersion)
	// The capital service's events go to the outbox through the emitter that
	// fails closed on an unregistered topic or payload, never dropping one.
	capitalSvc := capital.NewService(clk, capital.OutboxEmitter{Outbox: outbox, Clock: clk})
	priceStore := valuation.NewPriceStore(clk)
	reconRepo := reconciliation.NewRepository(clk, outbox, auditWriter)
	killChecker := killswitch.NewChecker(killswitch.Policy{})
	// The same configuration list the API reads (condition 1 of the policy
	// authority), so the worker and the API decide every gate alike.
	enabledCaps := parseCapabilities(cfg.API.EnabledCapabilities)
	gateChecker, err := gates.NewChecker(string(cfg.Env), func(c gates.Capability) bool {
		_, ok := enabledCaps[c]
		return ok
	}, clk)
	if err != nil {
		return nil, fmt.Errorf("gate checker: %w", err)
	}
	authorizer := func() *econ.Authorizer { return econ.NewAuthorizer(gateChecker, killChecker) }
	intents := func() *econ.IntentStore { return econ.NewIntentStore(clk, outbox, auditWriter) }
	effects := func() *econ.EffectStore { return econ.NewEffectStore(clk, outbox, auditWriter) }
	verificationResolver, err := identity.NewVerificationResolver(database)
	if err != nil {
		return nil, fmt.Errorf("verification resolver: %w", err)
	}
	router, err := legalRouterFor(cfg.Env, cfg.API.LegalPolicy)
	if err != nil {
		return nil, err
	}
	if router == nil {
		if router, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
			return nil, fmt.Errorf("legal router: %w", err)
		}
	}

	// As cmd/api: one fee ledger, built before the services that charge fees
	// (margin's liquidation fee, the internal book's fees, the paper venue's
	// strategy fills), each of which refuses to start without it.
	feeLedgerSvc, err := feeledger.NewService(feeledger.Config{
		Clock: clk, Ledger: ledgerSvc, Capital: capitalSvc, Effects: effects(), Authorizer: authorizer(), Assets: assetRepo,
		Audit: auditWriter,
	})
	if err != nil {
		return nil, fmt.Errorf("fee ledger: %w", err)
	}

	add := func(j advancedjobs.Job) { w.jobs = append(w.jobs, j) }
	add(advancedjobs.NewCapitalExpiry(database, capitalSvc, clk, settings(advancedjobs.NameCapitalExpiry)))

	// --- margin ----------------------------------------------------------------
	marginSvc, err := margin.NewService(margin.Config{
		Policy: margin.DefaultPolicy(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		LendingPoolName: "Margin lending pool (sandbox)", InsuranceFundName: "Margin insurance fund (sandbox)",
	}, margin.Dependencies{
		Clock: clk, Ledger: ledgerSvc, Effects: effects(), Intents: intents(), Authorizer: authorizer(),
		Registry: econ.NewRegistry(), Risk: risk.NewStore(), Capital: capitalSvc, Prices: priceStore, Router: router,
		Identity: compliance.NewAccountVerification(clk), KillSwitch: killChecker, Venue: margin.PaperVenue{}, Audit: auditWriter,
		FeeLedger: feeLedgerSvc,
	})
	if err != nil {
		return nil, fmt.Errorf("margin service: %w", err)
	}
	add(advancedjobs.NewMarginInterest(database, marginSvc, settings(advancedjobs.NameMarginInterest)))
	add(advancedjobs.NewMarginMarkToMarket(database, marginSvc, settings(advancedjobs.NameMarginMarkToMarket)))
	add(advancedjobs.NewMarginLiquidation(database, marginSvc, settings(advancedjobs.NameMarginLiquidation)))

	// --- derivatives -------------------------------------------------------------
	derivativesSvc, err := derivatives.NewService(derivatives.Config{
		Clock: clk, Intents: intents(), Effects: effects(), Authorizer: authorizer(), Registry: econ.NewRegistry(),
		Ledger: ledgerSvc, Capital: capitalSvc, RiskStore: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		Policy: derivatives.DefaultPolicy(), Prices: priceStore, Instruments: instrumentRepo, Router: router,
		KillSwitches: killChecker, Reconciliation: reconRepo, USDAssetID: econ.SandboxDollarID(),
	})
	if err != nil {
		return nil, fmt.Errorf("derivatives service: %w", err)
	}
	add(advancedjobs.NewDerivativesVariationMargin(database, derivativesSvc, instrumentRepo, clk, settings(advancedjobs.NameDerivativesVariation)))
	add(advancedjobs.NewDerivativesFunding(database, derivativesSvc, instrumentRepo, clk, settings(advancedjobs.NameDerivativesFunding)))
	add(advancedjobs.NewDerivativesExpiry(database, derivativesSvc, instrumentRepo, clk, settings(advancedjobs.NameDerivativesExpiry)))
	add(advancedjobs.NewDerivativesClearing(database, derivativesSvc, settings(advancedjobs.NameDerivativesClearing)))

	// --- the internal order book (prediction markets cancel through it) -----------
	ordersSvc, err := orders.NewService(orders.Config{
		Clock: clk, Ledger: ledgerSvc, Capital: capitalSvc, Intents: intents(), Effects: effects(), Authorizer: authorizer(),
		Registry: econ.NewRegistry(), Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router,
		Instruments: instrumentRepo, Assets: assetRepo, Prices: priceStore, KillSwitches: killChecker,
		Verification: httpapi.AdvancedVerificationLevels{Rule: compliance.NewAccountVerification(clk)}, FeeLedger: feeLedgerSvc,
	})
	if err != nil {
		return nil, fmt.Errorf("orders service: %w", err)
	}

	// --- prediction markets ---------------------------------------------------------
	// As cmd/api: the order book cancels the working orders on a market's
	// outcome books when it closes (which is what the close job does), and
	// the sandbox oracle -- whose key derives from a seed in the source --
	// exists only where fake providers are allowed. Closing and escrow
	// reconciliation never consult it.
	predmarketConfig := predmarket.Config{
		Clock: clk, Intents: intents(), Effects: effects(), Authorizer: authorizer(), Registry: econ.NewRegistry(),
		KillSwitches: killChecker, Ledger: ledgerSvc, Capital: capitalSvc, Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Instruments: instrumentRepo, Assets: assetRepo,
		Reconciliation: reconRepo, Audit: auditWriter, Books: ordersSvc,
	}
	if cfg.Env.AllowsFakeProviders() {
		if predmarketConfig.SandboxOracle, err = predmarket.NewSandboxOracle("nodal-sandbox-oracle",
			[]byte("nodal.predmarket.sandbox-oracle!")); err != nil {
			return nil, fmt.Errorf("prediction market sandbox oracle: %w", err)
		}
	}
	predmarketSvc, err := predmarket.NewService(predmarketConfig)
	if err != nil {
		return nil, fmt.Errorf("prediction markets: %w", err)
	}
	add(advancedjobs.NewPredictionClose(database, predmarketSvc, settings(advancedjobs.NamePredictionClose)))
	add(advancedjobs.NewPredictionEscrow(database, predmarketSvc, settings(advancedjobs.NamePredictionEscrow)))

	add(advancedjobs.NewOrdersCompleteTerminal(database, ordersSvc, settings(advancedjobs.NameOrdersCompleteTerminal)))
	add(advancedjobs.NewOrdersBookProof(database, ordersSvc, reconRepo, settings(advancedjobs.NameOrdersBookProof)))

	// --- paper equities, where the paper venue may run ------------------------------
	if in.plan.wired(advancedjobs.NameEquitiesSettlement) {
		equitiesSvc, err := newEquities(ctx, cfg, database, clk, intents(), effects(), authorizer(), killChecker, router,
			capitalSvc, ledgerSvc, priceStore, auditWriter)
		if err != nil {
			return nil, err
		}
		add(advancedjobs.NewEquitiesSettlement(database, equitiesSvc, settings(advancedjobs.NameEquitiesSettlement)))
		add(advancedjobs.NewEquitiesCorporateActions(database, equitiesSvc, clk, settings(advancedjobs.NameEquitiesCorporate)))
		add(advancedjobs.NewEquitiesVenue(database, equitiesSvc, clk, settings(advancedjobs.NameEquitiesVenue)))
	}

	// --- the strategy marketplace ----------------------------------------------------
	strategyRunner, err := sandboxrt.NewRunner(ctx, sandboxrt.RunnerConfig{})
	if err != nil {
		return nil, fmt.Errorf("strategy sandbox runner: %w", err)
	}
	w.closers = append(w.closers, func(ctx context.Context) { _ = strategyRunner.Close(ctx) })
	strategyAuth := authorizer()
	strategySandbox := sandboxrt.NewService(clk, strategyAuth, strategyRunner, auditWriter)
	strategySections, err := httpapi.NewMarketplaceRiskSections(httpapi.MarketplaceSectionsConfig{
		Clock: clk, Accounts: accountRepo, Instruments: instrumentRepo, Identity: compliance.NewAccountVerification(clk),
		Prices: priceStore, KillSwitches: killChecker, MaxPriceAge: maxMarkAge, Verification: verificationResolver,
	})
	if err != nil {
		return nil, fmt.Errorf("marketplace risk sections: %w", err)
	}
	// As cmd/api: a scheduled follow's allowed paper intent fills on the order
	// book's paper venue at the reference mark, priced through the one
	// published schedule (httpapi.MarketplacePaperSchedule), its spread and
	// fees itemized on the fill, paid from the account's wallet and booked
	// through the fee ledger.
	strategyIntents := intents()
	strategyExecutor, err := httpapi.NewMarketplacePaperExecutor(httpapi.MarketplacePaperExecutorConfig{
		Venue: orders.PaperConfig{
			Clock: clk, Ledger: ledgerSvc, Capital: capitalSvc, Intents: strategyIntents, Effects: effects(),
			Authorizer: strategyAuth, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router,
			Instruments: instrumentRepo, Assets: assetRepo, Prices: priceStore, KillSwitches: killChecker, MaxMarkAge: maxMarkAge,
			Verification: httpapi.AdvancedVerificationLevels{Rule: compliance.NewAccountVerification(clk)},
			FeeLedger:    feeLedgerSvc,
		},
		Schedule: httpapi.MarketplacePaperSchedule(),
		Intents:  strategyIntents,
	})
	if err != nil {
		return nil, fmt.Errorf("marketplace paper executor: %w", err)
	}
	strategyMarketplace, err := marketplace.NewService(marketplace.Config{
		Clock: clk, Intents: strategyIntents, Effects: effects(), Auth: strategyAuth, Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Sandbox: strategySandbox,
		// A scheduled follow runs an artifact the API built at validation; the
		// worker builds nothing.
		Builder:  httpapi.UnavailableStrategyBuilder{Reason: "the advanced worker builds no strategy; validation is the API's"},
		Sections: strategySections, Executor: strategyExecutor, Audit: auditWriter, MaxPriceAge: maxMarkAge,
	})
	if err != nil {
		return nil, fmt.Errorf("strategy marketplace: %w", err)
	}
	add(advancedjobs.NewMarketplaceFollows(database, strategyMarketplace, clk, settings(advancedjobs.NameMarketplaceFollows)))

	// --- transfers -----------------------------------------------------------------
	valuer := transfers.RegistryValuer{Prices: priceStore, Assets: assetRepo, QuoteAssetID: econ.SandboxDollarID(), MaxMarkAge: maxMarkAge}
	transfersScreen, err := transfers.NewListScreen("transfers-list-screen-v1", nil)
	if err != nil {
		return nil, fmt.Errorf("transfers compliance screen: %w", err)
	}
	transfersSvc, err := transfers.NewService(transfers.Deps{
		Clock: clk, Ledger: ledgerSvc, Capital: capitalSvc, Intents: intents(), Effects: effects(),
		Authorizer: authorizer(), KillSwitches: killChecker, Router: router, Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: transfers.DefaultPolicy(), Valuer: valuer, Screen: transfersScreen,
		Notifier: transfers.NewNotificationNotifier(notification.NewRepository()), Audit: auditWriter,
	})
	if err != nil {
		return nil, fmt.Errorf("transfers service: %w", err)
	}
	add(advancedjobs.NewTransfersReconcile(database, transfersSvc, reconRepo, settings(advancedjobs.NameTransfersReconcile)))

	// --- the sandbox chain: stable assets, bridge, delegated authority -------------
	var stableSvc *stableasset.Service
	network := ""
	chain, seed, err := openSandboxChain(ctx, cfg, in.resolver, database, clk)
	if err != nil {
		return nil, err
	}
	if chain != nil {
		network = chain.Network
		if stableSvc, err = stableasset.New(stableasset.Deps{
			DB: database, Clock: clk, Host: chain.Host, Intents: intents(), Effects: effects(), Authorizer: authorizer(),
			Facilities: econ.NewRegistry(), Ledger: ledgerSvc, Capital: capitalSvc, Risk: risk.NewStore(),
			RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Prices: priceStore, Kills: killChecker,
			Reconciliation: reconRepo, Audit: auditWriter, Accounts: accountRepo, Assets: assetRepo,
			Issuer: chain.Keys.Issuer, Custody: chain.Keys.Custody, Snapshots: chain.Keys.Snapshots,
		}); err != nil {
			return nil, fmt.Errorf("stable assets: %w", err)
		}
		bridgeSvc, err := bridge.New(bridge.Deps{
			DB: database, Clock: clk, Host: chain.Host, Intents: intents(), Effects: effects(), Authorizer: authorizer(),
			Facilities: econ.NewRegistry(), Ledger: ledgerSvc, Capital: capitalSvc, Risk: risk.NewStore(),
			RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Kills: killChecker, Reconciliation: reconRepo,
			Audit: auditWriter, Accounts: accountRepo, Assets: assetRepo, Holder: chain.Keys.Custody, Attester: chain.Keys.Attester,
		})
		if err != nil {
			return nil, fmt.Errorf("bridge: %w", err)
		}
		delegationSvc, err := newDelegation(ctx, cfg, database, clk, *chain, seed, log, delegation.Deps{
			Intents: intents(), Effects: effects(), Authorizer: authorizer(), Kills: killChecker, Ledger: ledgerSvc,
			Capital: capitalSvc, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router,
			Prices: priceStore, Identity: compliance.NewAccountVerification(clk), Profiles: compliance.NewRepository(auditWriter),
			Reconciliation: reconRepo, Audit: auditWriter, Accounts: accountRepo, Assets: assetRepo,
		})
		if err != nil {
			return nil, err
		}
		for _, step := range []struct {
			name string
			make func() (advancedjobs.Job, error)
		}{
			{advancedjobs.NameStableMonitor, func() (advancedjobs.Job, error) {
				return advancedjobs.NewStableMonitor(database, stableSvc, network, settings(advancedjobs.NameStableMonitor))
			}},
			{advancedjobs.NameStableSupply, func() (advancedjobs.Job, error) {
				return advancedjobs.NewStableSupply(database, stableSvc, network, settings(advancedjobs.NameStableSupply))
			}},
			{advancedjobs.NameStableUnknown, func() (advancedjobs.Job, error) {
				return advancedjobs.NewStableUnknown(database, stableSvc, network, settings(advancedjobs.NameStableUnknown))
			}},
			{advancedjobs.NameStableRedemptions, func() (advancedjobs.Job, error) {
				return advancedjobs.NewStableRedemptions(database, stableSvc, network, settings(advancedjobs.NameStableRedemptions))
			}},
			{advancedjobs.NameBridgeLegs, func() (advancedjobs.Job, error) {
				return advancedjobs.NewBridgeLegs(database, bridgeSvc, network, settings(advancedjobs.NameBridgeLegs))
			}},
			{advancedjobs.NameBridgeRoutes, func() (advancedjobs.Job, error) {
				return advancedjobs.NewBridgeRoutes(database, bridgeSvc, network, settings(advancedjobs.NameBridgeRoutes))
			}},
			{advancedjobs.NameDelegationUpkeep, func() (advancedjobs.Job, error) {
				return advancedjobs.NewDelegationUpkeep(database, delegationSvc, clk, settings(advancedjobs.NameDelegationUpkeep))
			}},
		} {
			j, err := step.make()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", step.name, err)
			}
			add(j)
		}
	}

	// --- metrics -------------------------------------------------------------------
	advanced, err := observability.NewAdvancedMetrics(in.meter)
	if err != nil {
		return nil, fmt.Errorf("advanced metrics: %w", err)
	}
	extra, err := advancedmetrics.NewInstruments(in.meter)
	if err != nil {
		return nil, err
	}
	if w.health, err = advancedmetrics.NewJobHealth(in.meter); err != nil {
		return nil, err
	}
	collector, err := advancedmetrics.NewCollector(advancedmetrics.Config{
		DB: database, Clock: clk, Metrics: advanced, Extra: extra, Valuer: valuer, Assets: assetRepo,
		Margin: marginSvc, Stable: stableSvc, Network: network, Interval: cfg.AdvancedWorker.MetricsInterval,
		Lag: advancedmetrics.DefaultLag,
	})
	if err != nil {
		return nil, err
	}
	add(collector)

	// The plan and the wiring must agree: a job the plan calls wired and
	// build did not wire (or the reverse) is a defect, never a silent gap.
	built := map[string]bool{}
	for _, j := range w.jobs {
		built[j.Spec().Name] = true
	}
	for _, pj := range in.plan.jobs {
		if built[pj.Name] != in.plan.wired(pj.Name) {
			return nil, fmt.Errorf("wiring: %s is %s by the plan but not by the build", pj.Name, map[bool]string{true: "wired", false: "unwired"}[in.plan.wired(pj.Name)])
		}
	}
	return w, nil
}

// newEquities builds paper equities exactly as cmd/api does: the paper venue
// NewExecutionAdapter gives this environment and provider mode, on the XNYS
// calendar with the closures recorded at start, and the sandbox schedule.
func newEquities(ctx context.Context, cfg *config.Config, database *db.DB, clk clock.Clock, intents *econ.IntentStore,
	effects *econ.EffectStore, authorizer *econ.Authorizer, kills *killswitch.Checker, router *legalrouter.Router,
	capitalSvc *capital.Service, ledgerSvc *ledger.Service, prices *valuation.PriceStore, auditWriter audit.Writer) (*equities.Service, error) {
	holidays, err := equities.Holidays(ctx, database, equities.CalendarXNYS)
	if err != nil {
		return nil, fmt.Errorf("equities holidays: %w", err)
	}
	cal, err := equities.NewCalendar(equities.CalendarXNYS, holidays)
	if err != nil {
		return nil, fmt.Errorf("equities calendar: %w", err)
	}
	venue, err := equities.NewExecutionAdapter(cfg.Env, cfg.Providers.Execution.Mode, paperConfig(cal))
	if err != nil {
		return nil, fmt.Errorf("equities execution adapter: %w", err)
	}
	svc, err := equities.NewService(equities.Config{
		Clock: clk, Intents: intents, Effects: effects, Authorizer: authorizer, KillSwitches: kills, Router: router,
		RiskPolicy: risk.DefaultAdvancedPolicy(), Capital: capitalSvc, Ledger: ledgerSvc, Prices: prices,
		Audit: auditWriter, Adapter: venue,
		Schedules: []fees.Schedule{{
			Version: "equities-sandbox-v1", QuoteAsset: econ.SandboxDollarID(), QuoteDecimals: econ.SandboxDollarDecimals,
			SpreadBPS: 10, PercentageFeeBPS: 10, StrategyFeeBPS: 5, SlippageReserveBPS: 50, Rounding: fees.DefaultRounding(),
		}},
		MarketCollarBPS: 100, MinOrderNotional: money.USDFromMinor(1_00),
		Verification: httpapi.AdvancedVerificationLevels{Rule: compliance.NewAccountVerification(clk)},
	})
	if err != nil {
		return nil, fmt.Errorf("equities service: %w", err)
	}
	return svc, nil
}

func paperConfig(cal *equities.Calendar) equities.PaperConfig {
	return equities.PaperConfig{Name: "nodal-paper-equities", Calendar: cal, SlippageBPS: 10}
}

// equitiesUnavailable is why this configuration has no equities venue ("" if
// it has one): equities.NewExecutionAdapter decides, from the environment and
// the execution provider's mode alone, as it does for cmd/api.
func equitiesUnavailable(cfg *config.Config) string {
	cal, err := equities.NewCalendar(equities.CalendarXNYS, nil)
	if err != nil {
		return "no equities calendar: " + err.Error()
	}
	if _, err := equities.NewExecutionAdapter(cfg.Env, cfg.Providers.Execution.Mode, paperConfig(cal)); err != nil {
		return "no equities execution adapter in " + string(cfg.Env) + ": " + err.Error()
	}
	return ""
}

// openSandboxChain is cmd/api's: the persistent simulated chain host with
// every platform key derived from the configured seed, or nil when no
// sandbox chain is configured. The seed is resolved through the binary's own
// secret resolver and never logged.
func openSandboxChain(ctx context.Context, cfg *config.Config, resolver config.Resolver, database *db.DB, clk clock.Clock) (*sandboxchain.Chain, sandboxchain.Seed, error) {
	network := cfg.Sandbox.DevnetNetwork
	if network == "" {
		return nil, sandboxchain.Seed{}, nil
	}
	text, err := resolver.Resolve(ctx, cfg.Sandbox.DevnetSeedRef)
	if err != nil {
		return nil, sandboxchain.Seed{}, fmt.Errorf("sandbox chain seed: %w", err)
	}
	seed, err := sandboxchain.ParseSeed(text)
	if err != nil {
		return nil, sandboxchain.Seed{}, fmt.Errorf("sandbox chain seed: %w", err)
	}
	chain, err := sandboxchain.Open(ctx, database, clk, cfg.Env, network, seed)
	if err != nil {
		return nil, sandboxchain.Seed{}, fmt.Errorf("sandbox chain: %w", err)
	}
	return &chain, seed, nil
}

// registeredVenuesSQL lists the delegation venues by name and settlement
// address. Read-only; delegation exports no venue lister.
const registeredVenuesSQL = `SELECT name, address FROM delegation_venues ORDER BY name`

// newDelegation builds delegated agent authority as a deployment does: the
// delegated signer from the sandbox seed's delegated-signer purpose with the
// default signing policy and the host's fees, and a venue co-signer for every
// registered venue whose address is the one the same seed derives for its
// name (sandboxchain.Seed.VenueSeed). The upkeep job never signs or
// co-signs; the service is whole so that every check it makes is the one a
// deployment makes.
func newDelegation(ctx context.Context, cfg *config.Config, database *db.DB, clk clock.Clock, chain sandboxchain.Chain,
	seed sandboxchain.Seed, log *slog.Logger, d delegation.Deps) (*delegation.Service, error) {
	signer, err := delegation.NewDelegatedSigner(seed.Derive(sandboxchain.PurposeDelegatedSigner), delegation.SignerConfig{
		Environment: cfg.Env, Clock: clk, Policy: delegation.DefaultSigningPolicy(), Fees: delegation.HostFees(chain.Host),
	})
	if err != nil {
		return nil, fmt.Errorf("delegated signer: %w", err)
	}
	rows, err := database.Pool().Query(ctx, registeredVenuesSQL)
	if err != nil {
		return nil, fmt.Errorf("delegation venues: %w", err)
	}
	defer rows.Close()
	registered := 0
	for rows.Next() {
		var name, address string
		if err := rows.Scan(&name, &address); err != nil {
			return nil, fmt.Errorf("delegation venues: %w", err)
		}
		registered++
		v := delegation.NewVenueSigner(seed.VenueSeed(name))
		if addr, perr := devnet.ParsePublicKey(address); perr == nil && addr == v.PublicKey() {
			d.Venues = append(d.Venues, v)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("delegation venues: %w", err)
	}
	log.InfoContext(ctx, "delegation venues", slog.Int("registered", registered), slog.Int("co_signers", len(d.Venues)))
	d.DB, d.Clock, d.Environment, d.Host, d.Signer = database, clk, cfg.Env, chain.Host, signer
	svc, err := delegation.New(d)
	if err != nil {
		return nil, fmt.Errorf("delegation: %w", err)
	}
	return svc, nil
}

// parseCapabilities is cmd/api's reading of CP_API_ENABLED_CAPABILITIES.
func parseCapabilities(list string) map[gates.Capability]struct{} {
	out := map[gates.Capability]struct{}{}
	for _, raw := range strings.Split(list, ",") {
		name := gates.Capability(strings.ToUpper(strings.TrimSpace(raw)))
		if name == "" || !name.Valid() {
			continue
		}
		out[name] = struct{}{}
	}
	return out
}

// legalRouterFor is cmd/api's: nil for the conservative policy, the
// development policy only outside production-like environments.
func legalRouterFor(env config.Environment, policy string) (*legalrouter.Router, error) {
	name := strings.ToUpper(strings.TrimSpace(policy))
	switch name {
	case "", "CONSERVATIVE":
		return nil, nil
	case "DEVELOPMENT":
		if env.IsProductionLike() {
			return nil, fmt.Errorf("%s=DEVELOPMENT: a development legal policy may not be loaded in %s", envLegalPolicy, env)
		}
		r, err := legalrouter.New(legalrouter.DevelopmentPolicy())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envLegalPolicy, err)
		}
		return r, nil
	default:
		return nil, fmt.Errorf("%s=%q: expected CONSERVATIVE or DEVELOPMENT", envLegalPolicy, name)
	}
}
