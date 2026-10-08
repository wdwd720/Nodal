package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/devidp"
	"github.com/nodal/controlplane/internal/auth/httpmw"
	"github.com/nodal/controlplane/internal/auth/oidc"
	"github.com/nodal/controlplane/internal/auth/pgstore"
	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/httpapi"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/notification"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/stripe"
	"github.com/nodal/controlplane/internal/ratelimit"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxchain"
	"github.com/nodal/controlplane/internal/sandboxfund"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/stream"
	"github.com/nodal/controlplane/internal/transfers"
	"github.com/nodal/controlplane/internal/treasury"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/withdrawal"
)

type buildInput struct {
	cfg      *config.Config
	lookup   func(string) (string, bool)
	resolver config.Resolver
	database *db.DB
	clock    clock.Clock
	logger   *slog.Logger

	// rateLimitStore is where the transport limiters keep their counters, and
	// rateLimitFailOpen says whether a store failure admits the request. Both
	// are opened by main, like the database, because they outlive composition:
	// the Redis client behind the distributed store is a connection pool with
	// a process lifetime, and build has no shutdown of its own to close it in.
	rateLimitStore    ratelimit.Store
	rateLimitFailOpen bool
}

// build constructs every dependency explicitly and returns the mounted server.
// It performs no business logic: each block below is a constructor call and the
// arguments that constructor documents.
func build(ctx context.Context, in buildInput) (*httpapi.Server, error) {
	cfg, database, clk, log := in.cfg, in.database, in.clock, in.logger

	// --- foundations ---------------------------------------------------
	meter := otel.GetMeterProvider().Meter("github.com/nodal/controlplane/cmd/api")
	droppedEvents, err := newDroppedEventCounter(meter)
	if err != nil {
		return nil, fmt.Errorf("outbox drop counter: %w", err)
	}
	outbox := event.NewOutbox(clk)
	auditWriter := audit.NewWriterWithBuildVersion(config.BuildVersion)
	auditAdapter := httpapi.NewAuditAppender(auditWriter)
	emitter := outboxEmitter{outbox: outbox, clk: clk, log: log, dropped: droppedEvents}

	accountRepo := accounts.NewRepository()
	assetRepo := assets.NewRepository()
	instrumentRepo := instruments.NewRepository()
	ledgerSvc := ledger.NewService(clk, config.BuildVersion)
	lots := positions.NewEngine()
	capitalSvc := capital.NewService(clk, emitter)
	idemStore := idempotency.NewStore(clk.Now)

	// --- authority plane ------------------------------------------------
	adminSvc := admin.NewService(clk, auditWriter)
	// A resolution-shaped reconciliation engine (F-52). The worker owns
	// detection; this owns the operator's answer to it. No observers, no
	// adapters and no ledger: this plane clears a record, it does not post.
	reconEngine, rerr := reconciliation.NewEngine(reconciliation.Config{
		DB:        database,
		Clock:     clk,
		Records:   reconciliation.NewRepository(clk, outbox, auditWriter),
		Policy:    reconciliation.DefaultPolicy(),
		Approvals: adminSvc,
		Metrics:   reconciliation.NoopMetrics(),
		Logger:    log,
	})
	if rerr != nil {
		return nil, fmt.Errorf("wiring: reconciliation engine: %w", rerr)
	}

	killController, cerr := killswitch.NewController(clk, auditAdapter.KillSwitchAudit(), httpapi.NewApprovalVerifier(adminSvc))
	if cerr != nil {
		return nil, fmt.Errorf("kill switch controller: %w", cerr)
	}
	killChecker := killswitch.NewChecker(killswitch.Policy{})

	// From the configuration, not the environment: this list is condition 1 of
	// the policy authority, and it belongs in the hash that proves which
	// configuration a running binary loaded.
	enabledCaps := parseCapabilities(in.cfg.API.EnabledCapabilities)
	gateChecker, err := gates.NewChecker(string(cfg.Env), func(c gates.Capability) bool {
		_, ok := enabledCaps[c]
		return ok
	}, clk)
	if err != nil {
		return nil, fmt.Errorf("gate checker: %w", err)
	}
	verificationResolver, err := identity.NewVerificationResolver(database)
	if err != nil {
		return nil, fmt.Errorf("verification resolver: %w", err)
	}
	gateAdmin, err := gates.NewAdmin(string(cfg.Env), clk, auditAdapter.GateAudit())
	if err != nil {
		return nil, fmt.Errorf("gate admin: %w", err)
	}

	// --- valuation and buying power -------------------------------------
	policyStore := valuation.NewPolicyStore()
	priceStore := valuation.NewPriceStore(clk)

	var bpEngine *buyingpower.Engine
	settlement, err := resolveSettlementAsset(ctx, in, assetRepo)
	if err != nil {
		return nil, err
	}
	if !settlement.AssetID.IsZero() {
		bpEngine, err = buyingpower.NewEngine(buyingpower.Deps{
			Clock:          clk,
			Policies:       policyStore,
			Prices:         priceStore,
			KillSwitches:   httpapi.NewKillSwitchReader(killController, killswitch.Policy{}),
			Reconciliation: httpapi.NewReconciliationBlockReader(),
			QuoteAssetID:   settlement.AssetID,
			Assets:         assetRepo,
			Valuer:         valuation.NewValuer(),
		})
		if err != nil {
			return nil, fmt.Errorf("buying power engine: %w", err)
		}
	} else {
		log.Warn("no settlement asset configured; buying power, holdings and funding are disabled",
			"chain_var", envSettlementChain, "mint_var", envSettlementMint)
	}

	// --- trading ---------------------------------------------------------
	intentRepo := intent.NewRepository(clk, outbox, auditWriter)
	intentSvc := intent.NewService(intentRepo, idemStore, clk, intent.DefaultIdempotencyTTL)
	orderRepo := execution.NewRepository(clk, outbox, auditWriter)
	attemptRepo := execution.NewAttemptRepository(clk, outbox, auditWriter)

	// --- funding ----------------------------------------------------------
	var fundingSvc *funding.Service
	if !settlement.AssetID.IsZero() {
		fundingProvider, perr := stripe.New(ctx, cfg.Providers.Funding, cfg.Env, in.resolver, clk,
			&http.Client{Timeout: providerTimeout(cfg.Providers.Funding)}, nil)
		if perr != nil {
			log.Warn("funding provider is not configured; the funding endpoints are disabled",
				"error", perr.Error())
		} else {
			repo, rerr := funding.NewRepository(clk, outbox, auditWriter)
			if rerr != nil {
				return nil, fmt.Errorf("funding repository: %w", rerr)
			}
			fundingSvc, rerr = funding.NewService(funding.Config{
				Env:          cfg.Env,
				ProviderMode: cfg.Providers.Funding.Mode,
				Reconcile:    funding.ReconcilePolicy{ToleranceBPS: 0},
				Availability: funding.AvailabilityPolicy{
					Version:       "funding/1",
					HoldDuration:  0,
					ReversibleFor: 72 * time.Hour,
				},
				SessionTTL:        24 * time.Hour,
				SettlementTimeout: 6 * time.Hour,
			}, funding.Deps{
				DB: database, Clock: clk, Repo: repo, Provider: fundingProvider,
				Ledger: ledgerSvc, Holds: capitalSvc, Accounts: accountRepo, Assets: assetRepo,
				Gates: gateChecker, KillSwitches: killChecker,
			})
			if rerr != nil {
				return nil, fmt.Errorf("funding service: %w", rerr)
			}
		}
	}

	// --- withdrawals -------------------------------------------------------
	withdrawalRepo, err := withdrawal.NewRepository(clk, auditWriter)
	if err != nil {
		return nil, fmt.Errorf("withdrawal repository: %w", err)
	}
	withdrawalSvc, err := withdrawal.NewService(withdrawal.Deps{
		DB: database, Clock: clk, Store: withdrawalRepo, Accounts: accountRepo,
		Gates: gateChecker, KillSwitches: killChecker,
		// The WITHDRAWALS capability is DISABLED in every environment, so no
		// request reaches the velocity check at all -- the gate refuses several
		// checks earlier and is the load-bearing control here.
		//
		// This value bounds NOTHING. The comment that used to sit here said the
		// bounds "stay at zero (no rolling allowance)", which reads as a refusal
		// and is the opposite of what a zero policy does: Check returns nil for
		// every request (F-68). The name now says so, so that whoever approves
		// the gate has to replace it rather than inherit it.
		Velocity: withdrawal.UnboundedVelocity(),
	})
	if err != nil {
		return nil, fmt.Errorf("withdrawal service: %w", err)
	}

	// --- identity and sessions ---------------------------------------------
	sessionStore := pgstore.New()
	sessionMgr, err := auth.NewManager(sessionStore, auth.ManagerConfig{
		TTL: cfg.Auth.SessionTTL, Now: clk.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("session manager: %w", err)
	}
	idp, err := identityProvider(ctx, cfg, in.resolver, clk)
	if err != nil {
		return nil, fmt.Errorf("identity provider: %w", err)
	}
	identitySvc, err := identity.New(identity.Deps{
		IdP: idp, DB: database, Accounts: accountRepo, Sessions: sessionMgr,
		Audit: auditWriter, Clock: clk, AttemptTTL: identity.DefaultAttemptTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("identity service: %w", err)
	}

	// --- the Nodal-native economy -------------------------------------------
	//
	// These are constructed unconditionally and gated at runtime. There is no
	// "enable the internal economy" flag here on purpose: whether a Credit may
	// move, an asset may launch or a payout may leave is a capability-gate and
	// legal-router question answered per request, not a boolean read at
	// startup. A deployment with no Credit asset provisioned still gets the
	// routes, and they answer NOT_FOUND with a reason rather than 404-ing as
	// though the feature did not exist.
	creditSvc := credit.NewService(ledgerSvc, clk)

	// The Credit purchase path (pgf.md). Separate from the funding block
	// above, and deliberately so: that is the crypto onramp, where a
	// customer's own money becomes crypto in a wallet Nodal never holds. This
	// one takes a card payment and issues internal Credits, which is the
	// reversible-card / irreversible-value problem the whole funding lifecycle
	// exists to manage. One set of credentials for both would mean one mode
	// for two products with different risk.
	creditPurchases := wireCreditPurchase(ctx, cfg, database, in.resolver, clk, creditSvc, gateChecker, log)
	nativeAssetSvc := nativeasset.NewService(clk, nil)
	nativeMarketSvc := nativemarket.NewService(ledgerSvc, creditSvc, valuation.NewPriceStore(clk), audit.NewWriter(),
		instruments.NewRepository(), nativemarket.NewRiskGate(risk.NewStore(), clk), clk)

	// Payout providers. A registry built with allowSandbox=false refuses any
	// provider with no contract reference, which is the programmatic assertion
	// PART LXV asks for: production cannot load a test double. Nothing is
	// registered, so every payout answers "no provider configured" -- the
	// honest state until a contract exists (BLOCKERS: B-PAYOUT-PROVIDER).
	payoutRegistry := payout.NewRegistry(cfg.Env == config.EnvLocal || cfg.Env == config.EnvTest)
	payoutEngine := payout.NewEngine(creditSvc)
	payoutSvc := payout.NewService(ledgerSvc, creditSvc, payoutEngine, payoutRegistry, clk)
	commerceSvc := commerce.NewService(ledgerSvc, creditSvc, audit.NewWriter(), clk)
	// The marketplace gate is resolved from the database on every purchase, so
	// pulling MARKETPLACE stops sales without a restart. Until it is ACTIVE,
	// internal/commerce refuses every purchase on its own -- the compiler in
	// front of it is an addition, not the only check.
	commerceSvc.SetCapabilityResolver(gateCapabilityResolver{checker: gateChecker, q: database})

	// The ledger's value-domain isolation consults the same gate checker every
	// other capability decision uses, so turning a capability off stops the
	// movement at the journal rather than only in a service.
	ledgerSvc.SetCapabilityResolver(gateCapabilityResolver{checker: gateChecker, q: database})

	// The persistent simulated chain host the sandbox capabilities run on,
	// opened once for all of them; nil when no sandbox chain is configured
	// (see openSandboxChain).
	sandboxChain, sandboxSeed, err := openSandboxChain(ctx, cfg, in.resolver, database, clk)
	if err != nil {
		return nil, err
	}

	// --- advanced financial capabilities (ADR-0020) ----------------------
	//
	// Constructed unconditionally and decided per request, like the native
	// economy above. Every capability here moves only SIMULATED value, LIVE is
	// unrepresentable in the schema, and in STAGING and PROD even a simulated
	// mode needs its gate ACTIVE (gates.DecideMode).
	// >>> advanced:margin services
	//
	// Margin, leverage, borrowing and liquidation (internal/margin), with the
	// production identity reader (D-055: an ACTIVE owner with a verified email
	// address, or a VERIFIED unexpired compliance profile; fail closed), the
	// reference-price paper venue and no derivatives book connected. Its legal router is the determination the rest of the API
	// applies: CONSERVATIVE unless CP_API_LEGAL_POLICY chose otherwise where
	// that is allowed. The numeraire is the sandbox dollar, which the margin
	// routes resolve per request; nothing here creates an asset.
	marginRouter, err := legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy)
	if err != nil {
		return nil, err
	}
	if marginRouter == nil {
		if marginRouter, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
			return nil, fmt.Errorf("margin legal router: %w", err)
		}
	}
	marginSvc, err := margin.NewService(margin.Config{
		Policy:            margin.DefaultPolicy(),
		RiskPolicy:        risk.DefaultAdvancedPolicy(),
		LendingPoolName:   "Margin lending pool (sandbox)",
		InsuranceFundName: "Margin insurance fund (sandbox)",
	}, margin.Dependencies{
		Clock:      clk,
		Ledger:     ledgerSvc,
		Effects:    econ.NewEffectStore(clk, outbox, auditWriter),
		Intents:    econ.NewIntentStore(clk, outbox, auditWriter),
		Authorizer: econ.NewAuthorizer(gateChecker, killChecker),
		Registry:   econ.NewRegistry(),
		Risk:       risk.NewStore(),
		Capital:    capitalSvc,
		Prices:     priceStore,
		Router:     marginRouter,
		Identity:   compliance.NewAccountVerification(clk),
		KillSwitch: killChecker,
		Venue:      margin.PaperVenue{},
		Audit:      auditWriter,
	})
	if err != nil {
		return nil, fmt.Errorf("margin service: %w", err)
	}

	// >>> advanced:derivatives services
	//
	// Futures, perpetuals and options in PAPER and SANDBOX mode, cleared
	// through one CLEARING_HOUSE facility. The service decides every rule
	// itself (route, gate, kill switches, Risk Kernel V2, reservation, fill,
	// runs, settlement); this block hands it the collaborators already built
	// above and the defaults its package documents. Its settlement compiler
	// judges each intent against the deployment's configured legal policy,
	// defaulting -- as the native economy's does -- to the conservative one,
	// which permits simulation and nothing that is not (httpapi's
	// TestDerivatives_TheDeploymentLegalPoliciesRouteEveryDerivativeIntent).
	derivativesRouter, err := legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy)
	if err != nil {
		return nil, err
	}
	if derivativesRouter == nil {
		if derivativesRouter, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
			return nil, fmt.Errorf("derivatives legal router: %w", err)
		}
	}
	derivativesSvc, err := derivatives.NewService(derivatives.Config{
		Clock: clk, Intents: econ.NewIntentStore(clk, outbox, auditWriter), Effects: econ.NewEffectStore(clk, outbox, auditWriter),
		Authorizer: econ.NewAuthorizer(gateChecker, killChecker), Registry: econ.NewRegistry(), Ledger: ledgerSvc,
		Capital: capitalSvc, RiskStore: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		Policy: derivatives.DefaultPolicy(), Prices: priceStore, Instruments: instrumentRepo, Router: derivativesRouter,
		KillSwitches: killChecker, Reconciliation: reconciliation.NewRepository(clk, outbox, auditWriter),
		// Margin snapshots and books are valued in the sandbox dollar.
		USDAssetID: econ.SandboxDollarID(),
	})
	if err != nil {
		return nil, fmt.Errorf("derivatives service: %w", err)
	}

	// >>> advanced:equities services
	//
	// Paper and sandbox equities (internal/equities). The broker boundary is
	// chosen from the environment and the execution provider's mode:
	// equities.NewExecutionAdapter refuses provider mode live unconditionally
	// and fake or sandbox outside LOCAL, TEST and DEV, so in STAGING and PROD
	// there is no adapter, no service, and the equities routes answer
	// UNSUPPORTED. Where it runs it is the deterministic paper venue, never a
	// test double: its calendar is XNYS with the closures recorded when the
	// process started (the service itself reads the recorded closures on
	// every order, so a closure added later still stops orders on its day).
	// Identity is the advanced capabilities' one rule (D-055).
	//
	// Charges: one schedule, in the sandbox dollar, every component of it
	// itemized on each fill's stored disclosure -- a 10 bps spread, a 10 bps
	// platform fee, 5 bps more for a strategy-originated order, and a 50 bps
	// slippage reserve that is held against adverse execution and never
	// charged. No venue fee is passed through: the paper venue charges none.
	// A market order's protection price (and its reservation) is 1% beyond
	// the venue's indicative price; the smallest order is worth one sandbox
	// dollar. Settlement is the sandbox dollar; nothing here creates an asset.
	var equitiesSvc *equities.Service
	equitiesHolidays, err := equities.Holidays(ctx, database, equities.CalendarXNYS)
	if err != nil {
		return nil, fmt.Errorf("equities holidays: %w", err)
	}
	equitiesCalendar, err := equities.NewCalendar(equities.CalendarXNYS, equitiesHolidays)
	if err != nil {
		return nil, fmt.Errorf("equities calendar: %w", err)
	}
	equitiesVenue, venueErr := equities.NewExecutionAdapter(cfg.Env, cfg.Providers.Execution.Mode, equities.PaperConfig{
		Name: "nodal-paper-equities", Calendar: equitiesCalendar, SlippageBPS: 10,
	})
	if venueErr != nil {
		log.Warn("no equities execution adapter in this deployment; the equities endpoints are disabled",
			"environment", string(cfg.Env), "error", venueErr.Error())
	} else {
		equitiesRouter, rerr := legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy)
		if rerr != nil {
			return nil, rerr
		}
		if equitiesRouter == nil {
			if equitiesRouter, rerr = legalrouter.New(legalrouter.ConservativePolicy()); rerr != nil {
				return nil, fmt.Errorf("equities legal router: %w", rerr)
			}
		}
		equitiesSvc, err = equities.NewService(equities.Config{
			Clock: clk, Intents: econ.NewIntentStore(clk, outbox, auditWriter), Effects: econ.NewEffectStore(clk, outbox, auditWriter),
			Authorizer: econ.NewAuthorizer(gateChecker, killChecker), KillSwitches: killChecker, Router: equitiesRouter,
			RiskPolicy: risk.DefaultAdvancedPolicy(), Capital: capitalSvc, Ledger: ledgerSvc, Prices: priceStore,
			Audit: auditWriter, Adapter: equitiesVenue,
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
	}

	// >>> advanced:orders services
	//
	// The internal order book (internal/orders) and the pipeline that runs a
	// customer order one stage per transaction. An operator opens each book;
	// nothing here opens one. Identity is the advanced capabilities' one rule
	// (D-055), and the legal router the determination the rest of the API
	// applies. No exchange calendar is wired to the book, so only "24x7"
	// instruments are in session (the package's fail-closed default).
	ordersRouter, err := legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy)
	if err != nil {
		return nil, err
	}
	if ordersRouter == nil {
		if ordersRouter, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
			return nil, fmt.Errorf("orders legal router: %w", err)
		}
	}
	ordersSvc, err := orders.NewService(orders.Config{
		Clock: clk, Ledger: ledgerSvc, Capital: capitalSvc,
		Intents: econ.NewIntentStore(clk, outbox, auditWriter), Effects: econ.NewEffectStore(clk, outbox, auditWriter),
		Authorizer: econ.NewAuthorizer(gateChecker, killChecker), Registry: econ.NewRegistry(), Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: ordersRouter, Instruments: instrumentRepo, Assets: assetRepo,
		Prices: priceStore, KillSwitches: killChecker,
		Verification: httpapi.AdvancedVerificationLevels{Rule: compliance.NewAccountVerification(clk)},
	})
	if err != nil {
		return nil, fmt.Errorf("orders service: %w", err)
	}
	ordersPipeline, err := orders.NewPipeline(ordersSvc, database)
	if err != nil {
		return nil, fmt.Errorf("orders pipeline: %w", err)
	}

	// >>> advanced:transfers services
	//
	// Customer-to-customer transfers of simulated value, and the sandbox
	// faucet. The transfer service decides every rule itself (eligibility, the
	// P2P_TRANSFERS gate, step-up, policy limits, Risk Kernel V2, the screen,
	// the reservation); this block hands it its collaborators and the defaults
	// its package documents.
	//
	// The settlement compiler judges each transfer against the deployment's
	// configured legal policy, defaulting -- as the native economy's does -- to
	// the conservative one when none is configured. Both permit simulation and
	// nothing that is not.
	transfersRouter, err := legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy)
	if err != nil {
		return nil, err
	}
	if transfersRouter == nil {
		if transfersRouter, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
			return nil, fmt.Errorf("transfers legal router: %w", err)
		}
	}
	// The package's default screen: deterministic, versioned, and named in
	// every decision's evidence. No sanctions list is configured for this
	// deployment, so it blocks no party; a configured list replaces the nil.
	transfersScreen, err := transfers.NewListScreen("transfers-list-screen-v1", nil)
	if err != nil {
		return nil, fmt.Errorf("transfers compliance screen: %w", err)
	}
	transfersSvc, err := transfers.NewService(transfers.Deps{
		Clock: clk, Ledger: ledgerSvc, Capital: capitalSvc,
		Intents:    econ.NewIntentStore(clk, outbox, auditWriter),
		Effects:    econ.NewEffectStore(clk, outbox, auditWriter),
		Authorizer: econ.NewAuthorizer(gateChecker, killChecker), KillSwitches: killChecker,
		Router: transfersRouter, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		Policy: transfers.DefaultPolicy(),
		// Par for the sandbox dollar and any other registry-declared paper
		// dollar; anything else at a mark in the sandbox dollar no older than
		// the one buying power V2 accepts. No mark, no transfer.
		Valuer: transfers.RegistryValuer{
			Prices: priceStore, Assets: assetRepo, QuoteAssetID: econ.SandboxDollarID(), MaxMarkAge: 5 * time.Minute,
		},
		Screen:   transfersScreen,
		Notifier: transfers.NewNotificationNotifier(notification.NewRepository()),
		Audit:    auditWriter,
	})
	if err != nil {
		return nil, fmt.Errorf("transfers service: %w", err)
	}
	// The faucet always runs in LOCAL, TEST and DEV. No configuration setting
	// enables it in STAGING or PROD, so it stays off there: handing play money
	// to the users of a deployed system is a decision someone has to own. The
	// global new-risk kill switch stops it too: minting play money is new value.
	const sandboxFaucetEnabled = false
	sandboxFaucet, err := sandboxfund.New(sandboxfund.Deps{
		Clock: clk, Ledger: ledgerSvc, Effects: econ.NewEffectStore(clk, outbox, auditWriter),
		Environment: cfg.Env, Enabled: sandboxFaucetEnabled, Policy: sandboxfund.DefaultPolicy(),
		KillSwitches: killChecker,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox faucet: %w", err)
	}

	// >>> advanced:treasury services
	//
	// The house book (internal/treasury) and the fee ledger (internal/feeledger).
	// The treasury decides every control itself (its signing policy, the
	// PRINCIPAL_TRADING and INTERNAL_MATCHING gates and kill switches, its
	// versioned limits, Risk Kernel V2, the order book's own house rules); this
	// block hands it its collaborators. It places house orders on the internal
	// book through the one order service above, and since 00759 nothing else
	// can place a house order at all.
	treasurySvc, err := treasury.NewService(treasury.Config{
		Clock: clk, Ledger: ledgerSvc, Effects: econ.NewEffectStore(clk, outbox, auditWriter),
		Authorizer: econ.NewAuthorizer(gateChecker, killChecker), Registry: econ.NewRegistry(), Orders: ordersSvc,
		Prices: priceStore, Assets: assetRepo, Instruments: instrumentRepo, KillSwitches: killChecker, Audit: auditWriter,
		RiskPolicy: risk.DefaultAdvancedPolicy(),
	})
	if err != nil {
		return nil, fmt.Errorf("treasury service: %w", err)
	}
	// The fee ledger books disclosed fees and performance fees (sandbox
	// accounting, never charged live) with its package defaults: a two-minute
	// quote TTL and the SYSTEM fee-ledger service applying its effects.
	feeLedgerSvc, err := feeledger.NewService(feeledger.Config{
		Clock: clk, Ledger: ledgerSvc, Capital: capitalSvc, Effects: econ.NewEffectStore(clk, outbox, auditWriter),
		Authorizer: econ.NewAuthorizer(gateChecker, killChecker), Assets: assetRepo, Audit: auditWriter,
	})
	if err != nil {
		return nil, fmt.Errorf("fee ledger: %w", err)
	}

	// >>> advanced:stable services
	//
	// The sandbox stable asset (internal/stableasset) runs on the sandbox chain
	// opened above, which exists only where one is configured
	// (CP_SANDBOX_DEVNET_NETWORK names its journal, CP_SANDBOX_DEVNET_SEED_REF
	// resolves to the seed every platform sandbox key derives from). Without
	// it every stable asset and bridge route answers UNSUPPORTED. The service
	// gets the collaborators every other advanced service gets; the account
	// facts it routes on come from the deployment's own resolvers (D-055
	// identity, the verification Nodal establishes itself, no jurisdiction
	// determination), never from a request.
	var stableDeps httpapi.StableDeps
	if sandboxChain != nil {
		chain := *sandboxChain
		var stableRouter *legalrouter.Router
		if stableRouter, err = legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy); err != nil {
			return nil, err
		}
		if stableRouter == nil {
			if stableRouter, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
				return nil, fmt.Errorf("stable asset legal router: %w", err)
			}
		}
		var stableSvc *stableasset.Service
		if stableSvc, err = stableasset.New(stableasset.Deps{
			DB: database, Clock: clk, Host: chain.Host,
			Intents: econ.NewIntentStore(clk, outbox, auditWriter), Effects: econ.NewEffectStore(clk, outbox, auditWriter),
			Authorizer: econ.NewAuthorizer(gateChecker, killChecker), Facilities: econ.NewRegistry(), Ledger: ledgerSvc,
			Capital: capitalSvc, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: stableRouter,
			Prices: priceStore, Kills: killChecker, Reconciliation: reconciliation.NewRepository(clk, outbox, auditWriter),
			Audit: auditWriter, Accounts: accountRepo, Assets: assetRepo,
			Issuer: chain.Keys.Issuer, Custody: chain.Keys.Custody, Snapshots: chain.Keys.Snapshots,
		}); err != nil {
			return nil, fmt.Errorf("stable assets: %w", err)
		}
		stableDeps = httpapi.StableDeps{
			Service: stableSvc, Host: chain.Host, HolderAddress: chain.Keys.Custody.HolderAddress,
			SnapshotKey: chain.Keys.Snapshots.PublicKey(), Capital: capitalSvc,
			Identity: compliance.NewAccountVerification(clk), Verification: verificationResolver,
		}
	} else {
		log.Warn("no sandbox chain is configured; the stable asset and bridge routes answer UNSUPPORTED",
			"network_var", "CP_SANDBOX_DEVNET_NETWORK", "seed_var", "CP_SANDBOX_DEVNET_SEED_REF")
	}

	// >>> advanced:bridge services
	//
	// Cross-chain capital (internal/bridge) over the same sandbox chain: the
	// custodial wallet signer is its holder signer (the bridge never holds a
	// customer key) and its attester is the one the host's genesis pins. The
	// holdings route reports the bridge's part of buying power under the
	// default haircut policy, which counts nothing that is not final; no
	// configuration chooses another. Buying power V2 itself takes no bridge
	// reader: its loader has no slot that would not count the bridge's pending
	// locks twice (they are capital reservations it already reads).
	var bridgeDeps httpapi.BridgeDeps
	if sandboxChain != nil {
		var bridgeRouter *legalrouter.Router
		if bridgeRouter, err = legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy); err != nil {
			return nil, err
		}
		if bridgeRouter == nil {
			if bridgeRouter, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
				return nil, fmt.Errorf("bridge legal router: %w", err)
			}
		}
		var bridgeSvc *bridge.Service
		if bridgeSvc, err = bridge.New(bridge.Deps{
			DB: database, Clock: clk, Host: sandboxChain.Host,
			Intents: econ.NewIntentStore(clk, outbox, auditWriter), Effects: econ.NewEffectStore(clk, outbox, auditWriter),
			Authorizer: econ.NewAuthorizer(gateChecker, killChecker), Facilities: econ.NewRegistry(), Ledger: ledgerSvc,
			Capital: capitalSvc, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: bridgeRouter,
			Kills: killChecker, Reconciliation: reconciliation.NewRepository(clk, outbox, auditWriter), Audit: auditWriter,
			Accounts: accountRepo, Assets: assetRepo, Holder: sandboxChain.Keys.Custody, Attester: sandboxChain.Keys.Attester,
		}); err != nil {
			return nil, fmt.Errorf("bridge: %w", err)
		}
		bridgeDeps = httpapi.BridgeDeps{
			Service: bridgeSvc, Host: sandboxChain.Host, Haircut: bridge.DefaultHaircutPolicy(),
			Identity: compliance.NewAccountVerification(clk), Verification: verificationResolver,
		}
	}

	// >>> advanced:predmarket services
	//
	// Prediction markets (internal/predmarket). The service is built from the
	// ledger, capital service, asset and instrument repositories, gate checker,
	// kill switches and audit writer constructed above; the econ stores, risk
	// store and reconciliation repository it takes are stateless wrappers over
	// the same clock, outbox and audit writer as their siblings below.
	//
	// The oracle: the deterministic sandbox oracle, whose Ed25519 key derives
	// from a fixed seed anyone can read, exists only where fake providers are
	// allowed (LOCAL, TEST, DEV). Everywhere else no oracle is configured, so
	// resolution answers PROVIDER_UNAVAILABLE instead of trusting a key whose
	// private half is in the source tree. The sandbox oracle only signs: every
	// observation an operator records is stored in the database (shared by
	// every replica, kept across restarts, audited), and resolution reads it
	// back from there. OutcomeBooks is not wired yet: until the order book's
	// adapter is, a settlement or void that finds outcome shares still
	// reserved by resting orders is refused, naming the reservations.
	var predmarketOracle *predmarket.SandboxOracle
	if cfg.Env.AllowsFakeProviders() {
		predmarketOracle, err = predmarket.NewSandboxOracle("nodal-sandbox-oracle", []byte("nodal.predmarket.sandbox-oracle!"))
		if err != nil {
			return nil, fmt.Errorf("prediction market sandbox oracle: %w", err)
		}
	}
	// The same legal policy the rest of the API routes against; the
	// conservative one (which permits simulation) when none is selected, since
	// the service needs a router rather than a nil meaning "conservative".
	predmarketRouter, err := legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy)
	if err != nil {
		return nil, err
	}
	if predmarketRouter == nil {
		if predmarketRouter, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
			return nil, fmt.Errorf("prediction market legal router: %w", err)
		}
	}
	predmarketConfig := predmarket.Config{
		Clock:          clk,
		Intents:        econ.NewIntentStore(clk, outbox, auditWriter),
		Effects:        econ.NewEffectStore(clk, outbox, auditWriter),
		Authorizer:     econ.NewAuthorizer(gateChecker, killChecker),
		Registry:       econ.NewRegistry(),
		KillSwitches:   killChecker,
		Ledger:         ledgerSvc,
		Capital:        capitalSvc,
		Risk:           risk.NewStore(),
		RiskPolicy:     risk.DefaultAdvancedPolicy(),
		Router:         predmarketRouter,
		Instruments:    instrumentRepo,
		Assets:         assetRepo,
		Reconciliation: reconciliation.NewRepository(clk, outbox, auditWriter),
		Audit:          auditWriter,
	}
	predmarketConfig.SandboxOracle = predmarketOracle
	// The order book cancels the working orders on a market's outcome books
	// at close, void and settlement, so no reservation still holds a share
	// when the escrow pays out (review-2 F5).
	predmarketConfig.Books = ordersSvc
	predmarketSvc, err := predmarket.NewService(predmarketConfig)
	if err != nil {
		return nil, fmt.Errorf("prediction markets: %w", err)
	}

	// >>> advanced:marketplace services
	//
	// The strategy marketplace and the sandbox it runs strategies in. The
	// services decide every rule themselves; this block hands them real
	// collaborators: fresh econ stores, the authorizer over the gate checker
	// and kill switches, the risk store and default Risk V2 policy, the
	// deployment's legal router (conservative by default, as for transfers)
	// and a wazero runner. Three collaborators have no production
	// implementation in this build, so internal/httpapi carries the thinnest
	// honest ones: risk sections read from the database, an executor that
	// refuses every dispatch (no paper venue executes economic intents yet),
	// and -- only where no Go toolchain is found -- a builder that refuses.
	strategyRouter, err := legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy)
	if err != nil {
		return nil, err
	}
	if strategyRouter == nil {
		if strategyRouter, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
			return nil, fmt.Errorf("marketplace legal router: %w", err)
		}
	}
	strategyRunner, err := sandboxrt.NewRunner(ctx, sandboxrt.RunnerConfig{})
	if err != nil {
		return nil, fmt.Errorf("strategy sandbox runner: %w", err)
	}
	strategyAuth := econ.NewAuthorizer(gateChecker, killChecker)
	strategySandbox := sandboxrt.NewService(clk, strategyAuth, strategyRunner, auditWriter)
	// The builder is found, not configured: the go command on PATH, made
	// absolute as sandboxrt.Builder requires. Its builds share one cache in
	// this user's cache directory (content-addressed, so sharing cannot change
	// an artifact) so only the first build on a machine is cold. Without a
	// toolchain, validation answers UNSUPPORTED and records nothing.
	var strategyBuilder marketplace.ArtifactBuilder = httpapi.UnavailableStrategyBuilder{
		Reason: "no go command was found on PATH when this process started",
	}
	if goBin, lookErr := exec.LookPath("go"); lookErr == nil {
		if goBin, lookErr = filepath.Abs(goBin); lookErr == nil {
			strategyCache := ""
			if base, cacheErr := os.UserCacheDir(); cacheErr == nil {
				strategyCache = filepath.Join(base, "nodal", "strategy-gocache")
				if mkErr := os.MkdirAll(strategyCache, 0o700); mkErr != nil {
					strategyCache = ""
				}
			}
			built, buildErr := sandboxrt.NewBuilder(sandboxrt.BuilderConfig{GoBinary: goBin, CacheDir: strategyCache, Clock: clk})
			if buildErr != nil {
				strategyBuilder = httpapi.UnavailableStrategyBuilder{Reason: "the go command found on PATH is unusable: " + buildErr.Error()}
			} else {
				strategyBuilder = built
			}
		}
	}
	if _, refuses := strategyBuilder.(httpapi.UnavailableStrategyBuilder); refuses {
		log.Warn("no usable Go toolchain; strategy validation answers UNSUPPORTED")
	}
	strategySections, err := httpapi.NewMarketplaceRiskSections(httpapi.MarketplaceSectionsConfig{
		Clock: clk, Accounts: accountRepo, Instruments: instrumentRepo, Identity: compliance.NewAccountVerification(clk),
		Prices: priceStore, KillSwitches: killChecker,
		// The same bound buying power V2 accepts a sandbox mark at; Risk V2
		// then judges the reference price's freshness by its own policy.
		MaxPriceAge: 5 * time.Minute,
		// As for the native economy: the level Nodal establishes itself, and
		// no jurisdiction determination (UNKNOWN), which simulation permits.
		Verification: verificationResolver,
	})
	if err != nil {
		return nil, fmt.Errorf("marketplace risk sections: %w", err)
	}
	strategyMarketplace, err := marketplace.NewService(marketplace.Config{
		Clock: clk, Intents: econ.NewIntentStore(clk, outbox, auditWriter), Effects: econ.NewEffectStore(clk, outbox, auditWriter),
		Auth: strategyAuth, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: strategyRouter,
		Sandbox: strategySandbox, Builder: strategyBuilder, Sections: strategySections, Executor: httpapi.UnwiredPaperExecutor{},
		Audit: auditWriter,
	})
	if err != nil {
		return nil, fmt.Errorf("strategy marketplace: %w", err)
	}

	// >>> advanced:delegation services
	//
	// Delegated agent authority (internal/delegation): scoped, expiring
	// capabilities an account's owner grants to the account's own agents,
	// delegated swaps signed only after the transaction inspector proves them
	// the approved plan, and withdrawals to allow-listed destinations under the
	// customer's policy. It runs on the sandbox chain opened above, so without
	// one every delegation route answers UNSUPPORTED. Its keys derive from the
	// same seed (internal/sandboxchain): the delegated wallet signer from
	// PurposeDelegatedSigner and one co-signer per sandbox venue from the
	// venue's name; the seed and the keys never leave the signers. No agent
	// reaches any of it over HTTP: the agent runtime admits an agent's requests
	// in-process. The identity rule is D-055's, never a request's.
	var delegationDeps httpapi.DelegationDeps
	if sandboxChain != nil {
		var delegationRouter *legalrouter.Router
		if delegationRouter, err = legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy); err != nil {
			return nil, err
		}
		if delegationRouter == nil {
			if delegationRouter, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
				return nil, fmt.Errorf("delegation legal router: %w", err)
			}
		}
		var delegationSigner *delegation.DelegatedSigner
		if delegationSigner, err = delegation.NewDelegatedSigner(sandboxSeed.Derive(sandboxchain.PurposeDelegatedSigner),
			delegation.SignerConfig{Environment: cfg.Env, Clock: clk, Policy: delegation.DefaultSigningPolicy(),
				Fees: delegation.HostFees(sandboxChain.Host)}); err != nil {
			return nil, fmt.Errorf("delegated signer: %w", err)
		}
		delegationVenues := httpapi.DelegationSandboxVenues()
		venueSigners := make([]*delegation.VenueSigner, 0, len(delegationVenues))
		for _, v := range delegationVenues {
			venueSigner := delegation.NewVenueSigner(sandboxSeed.VenueSeed(v.Name))
			venueSigners = append(venueSigners, venueSigner)
			delegationDeps.CoSigners = append(delegationDeps.CoSigners,
				httpapi.DelegationCoSigner{Name: v.Name, Chain: v.Chain, Address: venueSigner.PublicKey()})
		}
		if delegationDeps.Service, err = delegation.New(delegation.Deps{
			DB: database, Clock: clk, Environment: cfg.Env, Host: sandboxChain.Host,
			Intents: econ.NewIntentStore(clk, outbox, auditWriter), Effects: econ.NewEffectStore(clk, outbox, auditWriter),
			Authorizer: econ.NewAuthorizer(gateChecker, killChecker), Kills: killChecker, Ledger: ledgerSvc, Capital: capitalSvc,
			Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: delegationRouter, Prices: priceStore,
			Identity: compliance.NewAccountVerification(clk), Profiles: compliance.NewRepository(auditWriter),
			Reconciliation: reconciliation.NewRepository(clk, outbox, auditWriter), Audit: auditWriter,
			Accounts: accountRepo, Assets: assetRepo, Signer: delegationSigner, Venues: venueSigners,
		}); err != nil {
			return nil, fmt.Errorf("delegation service: %w", err)
		}
		// One sandbox venue is registered at startup, idempotently; an operator
		// registers the others this deployment co-signs for. A failure leaves
		// delegated swaps refused (no venue to settle at), never the API down.
		if _, verr := httpapi.EnsureDelegationVenue(ctx, database, delegationDeps, delegationVenues[0]); verr != nil {
			log.Warn("delegation: the sandbox venue is not registered; delegated swaps are refused until an operator registers one",
				"venue", delegationVenues[0].Name, "error", verr.Error())
		}
	}

	// >>> advanced:operator services
	//
	// The operator console reads the database at the instant of each request and
	// records sandbox reference marks through the same price store every
	// advanced reader values a SIMULATED asset at, quoted in the sandbox dollar.
	// A mark is accepted only while it is younger than the bound buying power V2
	// and the transfer valuer accept one, and the console calls older marks
	// stale. The chains it reports are the sandbox chain's host, where one is
	// configured; elsewhere /advanced/ops/chains answers UNSUPPORTED rather than
	// describing chains that do not exist here.
	operatorDeps := httpapi.OperatorDeps{Prices: priceStore, Audit: auditWriter, MaxMarkAge: 5 * time.Minute}
	if sandboxChain != nil {
		operatorDeps.Chains = sandboxChain.Host
	}

	advancedDeps := httpapi.AdvancedDeps{
		Authorizer: econ.NewAuthorizer(gateChecker, killChecker),
		Intents:    econ.NewIntentStore(clk, outbox, auditWriter),
		Effects:    econ.NewEffectStore(clk, outbox, auditWriter),
		Risk:       risk.NewStore(),
		Prices:     valuation.NewPriceStore(clk),
		// A sandbox mark older than this is stale and buying power V2 refuses
		// to value the holding rather than use it.
		MaxPriceAge: 5 * time.Minute,

		// >>> advanced:margin deps
		// Buying power V2 reads each customer's margin state from the engines'
		// own evaluations, so neither can disagree with it. The margin and
		// derivatives books are margined separately (D-054): the combined
		// reader sums their requirements, takes the stricter rate, and fails
		// the read if either part fails.
		Margin:        buyingpower.CombinedMarginReader{marginSvc.BuyingPowerReader(), derivativesSvc.MarginReader()},
		MarginService: marginSvc,

		// >>> advanced:derivatives deps
		// ValuationAssetID is the USDAssetID the service was built with. The
		// route facts come from the deployment's own resolvers: the
		// verification Nodal establishes itself, and no jurisdiction
		// determination (UNKNOWN), exactly as for the native economy below.
		Derivatives: httpapi.DerivativesDeps{
			Service: derivativesSvc, ValuationAssetID: econ.SandboxDollarID(), Verification: verificationResolver,
		},

		// >>> advanced:equities deps
		// Nil where no execution adapter exists (STAGING, PROD): the routes
		// then answer UNSUPPORTED. Listings show the freshest mark from the
		// price store the service prices orders with.
		Equities: httpapi.EquitiesDeps{Service: equitiesSvc, Prices: priceStore},

		// >>> advanced:orders deps
		Orders: httpapi.OrdersDeps{Service: ordersSvc, Pipeline: ordersPipeline},

		// >>> advanced:transfers deps
		Transfers: httpapi.TransfersDeps{
			Service: transfersSvc, Faucet: sandboxFaucet, FaucetEnabled: sandboxFaucetEnabled,
		},

		// >>> advanced:treasury deps
		Treasury: httpapi.TreasuryDeps{Service: treasurySvc, Fees: feeLedgerSvc},

		// >>> advanced:stable deps
		// Empty -- every route UNSUPPORTED -- unless a sandbox chain is
		// configured (see the services block).
		Stable: stableDeps,

		// >>> advanced:bridge deps
		Bridge: bridgeDeps,

		// >>> advanced:predmarket deps
		PredictionMarkets: predmarketSvc,
		PredictionOracle:  predmarketOracle,

		// >>> advanced:marketplace deps
		Marketplace: httpapi.MarketplaceDeps{Service: strategyMarketplace, Sandbox: strategySandbox},

		// >>> advanced:delegation deps
		Delegation: delegationDeps,

		// >>> advanced:operator deps
		Operator: operatorDeps,
	}

	// --- realtime ----------------------------------------------------------
	// The hub is mounted so the endpoint honors Last-Event-ID, heartbeats and
	// per-client cleanup. No producer is attached: the event bus adapter lives
	// in a package this binary does not yet depend on, so the stream carries
	// heartbeats only until it is wired. It is never authoritative either way
	// (PART 109).
	hub := stream.NewHub(1024, log)
	sse := stream.NewHandler(hub, 15*time.Second)

	legalPolicy, err := legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy)
	if err != nil {
		return nil, err
	}

	ports, err := httpapi.Wire(httpapi.WireDeps{
		DB:                database,
		Clock:             clk,
		Env:               cfg.Env,
		Identity:          identitySvc,
		Sessions:          sessionMgr,
		Accounts:          accountRepo,
		Assets:            assetRepo,
		Instruments:       instrumentRepo,
		Ledger:            ledgerSvc,
		Positions:         lots,
		BuyingPower:       bpEngine,
		Intents:           intentSvc,
		IntentRepo:        intentRepo,
		Orders:            orderRepo,
		Attempts:          attemptRepo,
		Funding:           fundingSvc,
		Withdrawals:       withdrawalSvc,
		Gates:             gateAdmin,
		GateChecker:       gateChecker,
		KillSwitches:      killController,
		AdminActions:      adminSvc,
		Idempotency:       idemStore,
		Providers:         provider.NewRegistry(),
		FundingSettlement: settlement,
		ProviderCatalog:   providerCatalog(cfg),
		NativeEconomy: httpapi.NativeEconomyDeps{
			Credits:         creditSvc,
			CreditPurchases: creditPurchases.Service,
			NativeAssets:    nativeAssetSvc,
			NativeMarkets:   nativeMarketSvc,
			Payouts:         payoutSvc,
			PayoutEngine:    payoutEngine,
			Commerce:        commerceSvc,
			// No payout policy is configured, so the fail-closed default
			// applies and no origin is withdrawable. Activating one is a
			// policy version with evidence, not a code change here.
			PayoutPolicy: nil,
			Capabilities: gateCapabilityResolver{checker: gateChecker, q: database},
			// The verification level Nodal can establish BY ITSELF, and no
			// level above it: NODAL_IDENTITY when the identity provider
			// asserted a verified email address, otherwise NONE.
			//
			// This was `nil` -- every account NONE -- on the reasoning that a
			// deployment with no KYC provider has established nothing. The
			// provider is genuinely external (BLOCKERS B-06) and PAYOUT_KYC
			// and ENHANCED remain unreachable. But NODAL_IDENTITY is a fact
			// this system establishes at login, and reporting NONE for it made
			// every Domain A action -- which needs exactly that level --
			// impossible in every deployment whatever its gates said. A
			// control no user can ever satisfy is not a control.
			Verification: verificationResolver,
			// The Settlement Compiler's legal policy. The default is the
			// conservative one: it permits simulation and denies every
			// internal-economy product and every payout. CP_API_LEGAL_POLICY
			// selects a different one, and legalPolicy refuses anything but
			// CONSERVATIVE outside LOCAL, DEV and TEST.
			LegalRouter: legalPolicy,
			// No jurisdiction determination exists, so every account is
			// UNKNOWN and the conservative policy refuses accordingly. It is
			// deliberately NOT inferred from an IP address: that is a legal
			// determination wearing a network header's clothes.
			Jurisdiction: nil,
			Clock:        clk,
		},
		// No execution adapter is wired: quote previews answer
		// PROVIDER_UNAVAILABLE rather than invent a price.
		Quotes: nil,
		// The reconciliation engine raises records in its own binary; this is
		// the plane an operator resolves them from.
		//
		// It used to be nil, and the asymmetry was a trap: the worker raises
		// records, `blocks_new_risk` is read by buying power, and nothing a
		// deployment could run ever cleared one. An account could be frozen by
		// an automated check with no button to unfreeze it (F-52).
		//
		// The engine here is deliberately resolution-shaped. It carries no
		// observers, no adapters and NO LEDGER: this API resolves records and
		// does not post compensating entries, and the adapter refuses a request
		// for one by name. A nil ledger means `applyRepair` refuses too, so the
		// two agree even if somebody later changes only one of them.
		Reconcile: httpapi.NewReconciliationPort(httpapi.ReconciliationDeps{
			Engine:    reconEngine,
			ReadModel: httpapi.NewReadModel(database),
			DB:        database,
		}),
		// Executable kinds are BREAK_GLASS_GRANT (which is what makes any
		// approve-side permission obtainable at all) and KILL_SWITCH_RELEASE.
		// Every other kind answers 422 UNSUPPORTED here: its effect belongs
		// to the domain endpoint that quotes the approval, not to a second
		// path through the action table.
		AdminExecutors: mergeExecutors(
			httpapi.AdminExecutors(adminSvc, sessionMgr, killController),
			httpapi.DomainAExecutors(httpapi.DomainAExecutorDeps{
				NativeAssets:  nativeAssetSvc,
				NativeMarkets: nativeMarketSvc,
				Commerce:      commerceSvc,
				Payouts:       payoutSvc,
				Credits:       creditSvc,
			}),
		),
		Advanced:       advancedDeps,
		IdempotencyTTL: httpapi.DefaultIdempotencyTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("wiring: %w", err)
	}
	ports.Stream = sse

	// Provider webhooks. The map is keyed by the provider name in the path, so
	// POST /v1/webhooks/stripe_credit reaches the Credit purchase pipeline and
	// nothing else does. An unconfigured provider leaves no key, and the
	// handler answers 404 rather than accepting a delivery it cannot process
	// -- which is what makes Stripe retry instead of considering it delivered.
	if creditPurchases.WebhookPort != nil {
		if ports.Webhooks == nil {
			ports.Webhooks = map[string]httpapi.WebhookPort{}
		}
		ports.Webhooks[creditPurchases.ProviderKey] = creditPurchases.WebhookPort
	}

	limits, err := rateLimits(clk, cfg.Env, cfg.RateLimit, in.rateLimitStore, in.rateLimitFailOpen)
	if err != nil {
		return nil, fmt.Errorf("rate limits: %w", err)
	}

	// The development identity provider needs a picker page to come back
	// from; without one, GET /v1/auth/login redirects to a 404 and no
	// browser-style login can complete. It is mounted only when the
	// configuration selected dev auth, which config.Validate already refuses
	// outside LOCAL, TEST and DEV, and httpapi.New refuses the mount itself
	// in any other environment.
	var nonSpecRoutes map[string]http.Handler
	if cfg.Auth.Mode == config.AuthModeDev {
		// Default to the relative callback path so the picker always lands
		// back on the host the browser is already talking to, whatever port
		// the process was told to listen on. An explicit
		// CP_AUTH_REDIRECT_URL still wins.
		callback := cfg.Auth.RedirectURL
		if callback == "" {
			callback = defaultCallbackPath
		}
		devLogin, derr := devLoginHandler(cfg.Env, callback, log)
		if derr != nil {
			return nil, fmt.Errorf("dev login page: %w", derr)
		}
		nonSpecRoutes = map[string]http.Handler{devLoginPath: devLogin}
		log.Warn("development identity provider is enabled; the sign-in picker is mounted",
			"path", devLoginPath, "callback", callback, "env", string(cfg.Env))
	}

	cookieName := httpmw.EffectiveCookieName(cfg.Auth.CookieName, cfg.Auth.CookieDomain, cfg.Auth.CookieSecure)

	return httpapi.New(httpapi.Options{
		Env:               cfg.Env,
		BuildVersion:      config.BuildVersion,
		ConfigHash:        cfg.Hash(),
		PublicBaseURL:     cfg.HTTP.PublicBaseURL,
		CORSOrigins:       cfg.HTTP.CORSOrigins,
		TrustedProxyCIDRs: cfg.HTTP.TrustedProxyCIDRs,
		MaxBodyBytes:      cfg.HTTP.MaxBodyBytes,
		CookieName:        cookieName,
		CookieDomain:      cfg.Auth.CookieDomain,
		CookieSecure:      cfg.Auth.CookieSecure,
		SessionTTL:        cfg.Auth.SessionTTL,
		IdempotencyTTL:    httpapi.DefaultIdempotencyTTL,
		Clock:             clk,
		Logger:            log,
		Meter:             meter,
		Limits:            limits,
		Authenticator:     httpmw.Session(sessionMgr, database, cookieName),
		NonSpecRoutes:     nonSpecRoutes,
		Ports:             ports,
	})
}

// resolveSettlementAsset looks the USD-pegged settlement asset up by its
// identity, (chain, mint). A production-like deployment must name it; a
// developer environment may leave it unset, in which case the endpoints that
// need it answer UNSUPPORTED instead of guessing.
func resolveSettlementAsset(ctx context.Context, in buildInput, repo *assets.Repository) (httpapi.FundingSettlement, error) {
	// From the configuration, not from the environment. They were read straight
	// from the environment here, which put them outside the one table that
	// documents and validates everything else -- so a deployment could pass
	// every configuration check and still refuse to start on these two.
	chain := strings.TrimSpace(in.cfg.API.SettlementChain)
	mint := strings.TrimSpace(in.cfg.API.SettlementMint)
	out := httpapi.FundingSettlement{
		Network:  in.cfg.API.FundingNetwork,
		Currency: in.cfg.API.FundingCurrency,
	}
	if chain == "" || mint == "" {
		if in.cfg.Env.IsProductionLike() {
			return out, fmt.Errorf("%s and %s are required in %s", envSettlementChain, envSettlementMint, in.cfg.Env)
		}
		return out, nil
	}
	asset, err := repo.GetByMint(ctx, in.database, chain, mint)
	if err != nil {
		return out, fmt.Errorf("settlement asset %s/%s: %w", chain, mint, err)
	}
	if !asset.IsStablecoin {
		return out, fmt.Errorf("settlement asset %s/%s is not a stablecoin", chain, mint)
	}
	out.AssetID = asset.ID
	return out, nil
}

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

func identityProvider(ctx context.Context, cfg *config.Config, resolver config.Resolver, clk clock.Clock) (auth.IdentityProvider, error) {
	switch cfg.Auth.Mode {
	case config.AuthModeDev:
		// devidp's own constructor refuses anything but LOCAL/TEST/DEV, and
		// config.Validate refuses mode "dev" outside them as well.
		return devidp.New(string(cfg.Env), devidp.Config{Now: clk.Now})
	case config.AuthModeOIDC:
		secret := ""
		if !cfg.Auth.ClientSecretRef.IsZero() {
			var err error
			secret, err = resolver.Resolve(ctx, cfg.Auth.ClientSecretRef)
			if err != nil {
				return nil, err
			}
		}
		return oidc.New(ctx, oidc.Config{
			Issuer:       cfg.Auth.Issuer,
			ClientID:     cfg.Auth.ClientID,
			ClientSecret: secret,
			RedirectURL:  cfg.Auth.RedirectURL,
			Now:          clk.Now,
		})
	default:
		return nil, fmt.Errorf("unknown auth mode %q", cfg.Auth.Mode)
	}
}

func providerTimeout(p config.ProviderConfig) time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 15 * time.Second
}

// providerCatalog describes each configured provider slot for the admin status
// view. Verification labels are the ones the adapters themselves claim; slots
// with no adapter in this binary report CODE_COMPLETE, which is the weakest
// label and therefore never overstates readiness.
func providerCatalog(cfg *config.Config) []httpapi.ProviderDescriptor {
	slots := []struct {
		role string
		p    config.ProviderConfig
	}{
		{"FundingProvider", cfg.Providers.Funding},
		{"WalletProvider", cfg.Providers.Wallet},
		{"SigningProvider", cfg.Providers.Signing},
		{"ExecutionAdapter", cfg.Providers.Execution},
		{"MarketDataProvider", cfg.Providers.MarketData},
		{"ChainObserver", cfg.Providers.ChainObserver},
		{"ChainObserverFallback", cfg.Providers.ChainObserverFallback},
		{"ModelProvider", cfg.Providers.Model},
		{"EventBus", cfg.Providers.EventBus},
		{"WorkflowEngine", cfg.Providers.Workflow},
		{"ObjectArchive", cfg.Providers.Archive},
		{"NotificationProvider", cfg.Providers.Notification},
	}
	out := make([]httpapi.ProviderDescriptor, 0, len(slots))
	for _, s := range slots {
		name := s.p.Name
		if name == "" {
			name = strings.ToLower(s.role)
		}
		out = append(out, httpapi.ProviderDescriptor{
			Name:         name,
			Role:         s.role,
			Mode:         s.p.Mode,
			Verification: provider.CodeComplete,
		})
	}
	return out
}

// Default transport budgets (PART 180). They protect the process, never the
// money: the risk kernel enforces order frequency from persisted state, so
// losing these counters cannot widen a financial budget (PART 181).
var defaultRateLimits = map[string]ratelimit.Limit{
	envRateLimitGeneral: {Requests: 600, Window: time.Minute},
	envRateLimitAuth:    {Requests: 30, Window: time.Minute},
	envRateLimitQuote:   {Requests: 120, Window: time.Minute},
	envRateLimitCommand: {Requests: 120, Window: time.Minute},
}

// errRateLimitDisabledInProduction refuses a switched-off transport budget
// where real money is at stake. Turning a limiter off is a load-testing and
// development affordance, never a production one.
var errRateLimitDisabledInProduction = errors.New("a transport rate limit may not be disabled in this environment")

// rateLimits builds the four transport limiters from configuration, defaulting
// to the values above. The limiter behavior itself is internal/ratelimit's:
// 429 with RateLimit-Limit, RateLimit-Remaining, RateLimit-Reset and
// Retry-After, and problem+json.
//
// The counters live in process memory, so the budget is per replica. That is
// deliberate for now: a Redis outage must never take the API down, and these
// counters are not financial authority (PART 181).
// rateLimits builds the four transport limiters over a store the caller has
// already chosen and proved usable. It takes the store rather than making one
// because where the counters live is a deployment decision (see
// rateLimitStore), and because a limiter that silently made its own store would
// be a second, invisible answer to that question.
func rateLimits(clk clock.Clock, env config.Environment, cfg config.RateLimitConfig, store ratelimit.Store, failOpen bool) (httpapi.RateLimits, error) {
	if store == nil {
		return httpapi.RateLimits{}, errors.New("rate limits: no store")
	}
	// The specs come from the configuration, not from the environment. They
	// were read here directly, which kept them out of the one table that
	// documents, validates and hashes everything else -- so a typo was a
	// binary that would not start rather than a failed configuration check,
	// and a budget loosened in a platform dashboard left the configuration
	// hash unchanged.
	build := func(name, spec string) (*ratelimit.Limiter, error) {
		limit, enabled, err := ratelimit.ParseLimit(spec, defaultRateLimits[name])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if !enabled {
			if env.IsProductionLike() {
				return nil, fmt.Errorf("%s: %w", name, errRateLimitDisabledInProduction)
			}
			return nil, nil
		}
		// failOpen comes from the store, because it is a property of the
		// store: the in-process one cannot fail, and the shared one failing
		// open would remove the only limit that exists across replicas. It is
		// not a property of the limit, so it is not decided here.
		return ratelimit.NewLimiter(strings.ToLower(name), store, limit, clk.Now, failOpen)
	}

	var (
		out httpapi.RateLimits
		err error
	)
	if out.General, err = build(envRateLimitGeneral, cfg.General); err != nil {
		return httpapi.RateLimits{}, err
	}
	if out.Auth, err = build(envRateLimitAuth, cfg.Auth); err != nil {
		return httpapi.RateLimits{}, err
	}
	if out.Quote, err = build(envRateLimitQuote, cfg.Quote); err != nil {
		return httpapi.RateLimits{}, err
	}
	if out.Command, err = build(envRateLimitCommand, cfg.Command); err != nil {
		return httpapi.RateLimits{}, err
	}
	return out, nil
}

// dropReason labels a dropped outbox event. The set is closed and small so it
// is safe as a metric attribute.
const (
	dropUnregisteredTopic = "unregistered_topic"
	dropMissingAggregate  = "missing_aggregate_id"
)

// errUnknownEventPayload is returned when a payload type this binary cannot key
// reaches the emitter. It fails the caller's transaction on purpose: an
// unrecognized payload is a code gap, not a deployment gap, so it must surface
// in a test or a canary rather than silently cost an event in production.
var errUnknownEventPayload = errors.New("outbox: unrecognized event payload type")

// outboxEmitter adapts internal/event's transactional outbox to the Emitter
// internal/capital declares.
//
// Two conditions make an event unpublishable without the domain having done
// anything wrong: a topic this deployment's registry does not know, and a
// payload whose aggregate id is empty. Neither fails the transaction — a
// transport gap must never block money movement — but both are recorded at WARN
// and counted on outbox_events_dropped{topic,reason}, because a dropped event
// means the ledger and its consumers have diverged and nobody may learn of it
// until reconciliation notices, or never.
//
// An unrecognized payload type is different in kind and fails closed.
type outboxEmitter struct {
	outbox  *event.Outbox
	clk     clock.Clock
	log     *slog.Logger
	dropped metric.Int64Counter // nil when no meter is configured
}

func (e outboxEmitter) Emit(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	aggregate, err := capitalAggregateID(payload)
	if err != nil {
		e.log.LogAttrs(ctx, slog.LevelError, "refusing to publish an unrecognized event payload",
			slog.String("topic", topic), slog.String("payload_type", fmt.Sprintf("%T", payload)))
		return fmt.Errorf("%w: topic %s, payload %T", err, topic, payload)
	}
	spec, ok := event.Lookup(event.Topic(topic))
	if !ok {
		e.drop(ctx, topic, dropUnregisteredTopic, aggregate)
		return nil
	}
	if aggregate == "" {
		e.drop(ctx, topic, dropMissingAggregate, aggregate)
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return e.outbox.Enqueue(ctx, tx, topic, event.Envelope{
		ID:            event.NewEventID().String(),
		Type:          topic,
		SchemaVersion: spec.SchemaVersion,
		Source:        "capital",
		AggregateType: spec.AggregateType,
		AggregateID:   aggregate,
		CorrelationID: observability.CorrelationID(ctx),
		OccurredAt:    e.clk.Now(),
		Payload:       body,
	})
}

// drop records an event that could not be published. It is deliberately loud:
// WARN in the log and a counter a dashboard can alert on.
func (e outboxEmitter) drop(ctx context.Context, topic, reason, aggregate string) {
	e.log.LogAttrs(ctx, slog.LevelWarn, "outbox event dropped; downstream consumers will not see it",
		slog.String("topic", topic),
		slog.String("reason", reason),
		slog.String("aggregate_id", aggregate),
		slog.String("correlation_id", observability.CorrelationID(ctx)),
	)
	if e.dropped != nil {
		e.dropped.Add(ctx, 1, observability.WithSafeAttrs(
			attribute.String("topic", topic),
			attribute.String("reason", reason),
		))
	}
}

// capitalAggregateID returns the aggregate id of a capital event payload. An
// unrecognized type is an error, not an empty string, so a payload added later
// cannot inherit the drop path by default.
func capitalAggregateID(payload any) (string, error) {
	switch p := payload.(type) {
	case capital.ReservationEvent:
		return p.ReservationID, nil
	case capital.EnvelopeEvent:
		return p.EnvelopeID, nil
	case capital.HoldEvent:
		return p.HoldID, nil
	default:
		return "", errUnknownEventPayload
	}
}

// newDroppedEventCounter builds the drop counter. A missing meter is not an
// error: the log line still carries the signal.
func newDroppedEventCounter(meter metric.Meter) (metric.Int64Counter, error) {
	if meter == nil {
		return nil, nil
	}
	return meter.Int64Counter("outbox_events_dropped",
		metric.WithDescription("Domain events that could not be published to the transactional outbox"),
		metric.WithUnit(observability.UnitCount))
}

// mergeExecutors joins executor tables, refusing a duplicate kind rather than
// letting one table silently win. Two executors for one administrative action
// would mean the effect depended on map iteration order.
func mergeExecutors(tables ...map[admin.Kind]admin.ExecFunc) map[admin.Kind]admin.ExecFunc {
	out := make(map[admin.Kind]admin.ExecFunc)
	for _, t := range tables {
		for k, fn := range t {
			if _, dup := out[k]; dup {
				panic("cmd/api: two executors registered for administrative action " + string(k))
			}
			out[k] = fn
		}
	}
	return out
}

// legalRouterFor builds the Settlement Compiler's policy from
// CP_API_LEGAL_POLICY.
//
// Unset or CONSERVATIVE returns nil, which internal/httpapi reads as the
// fail-closed default — the absence of a determination, which is what PART
// LXIII requires a fresh production deployment to start from.
//
// DEVELOPMENT returns a policy that permits the internal economy, and is
// REFUSED in STAGING and PROD. The refusal is an error rather than a silent
// downgrade to the conservative policy: an operator who asked for a
// development policy in production has misconfigured something, and starting
// anyway with different behaviour than they asked for is how that goes
// unnoticed.
func legalRouterFor(env config.Environment, policy string) (*legalrouter.Router, error) {
	name := strings.ToUpper(strings.TrimSpace(policy))
	switch name {
	case "", "CONSERVATIVE":
		return nil, nil
	case "DEVELOPMENT":
		if env.IsProductionLike() {
			return nil, fmt.Errorf("%s=DEVELOPMENT: a development legal policy may not be loaded in %s",
				envLegalPolicy, env)
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
