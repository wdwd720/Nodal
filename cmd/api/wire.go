package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/activity"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/agents"
	"github.com/nodal/controlplane/internal/alert"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/devidp"
	"github.com/nodal/controlplane/internal/auth/httpmw"
	"github.com/nodal/controlplane/internal/auth/oidc"
	"github.com/nodal/controlplane/internal/auth/pgstore"
	"github.com/nodal/controlplane/internal/capacity"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/execution"
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
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/notifications"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/operatorroles"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/profile"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/stripe"
	"github.com/nodal/controlplane/internal/ratelimit"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stream"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/verification"
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

	// alerts is where a raised alert goes when it leaves the process. Opened by
	// main for the same reason as the two above: it owns a goroutine and a queue
	// that have to be drained on shutdown, and build has no shutdown of its own.
	// Nil is a working configuration -- LOCAL and TEST have no destination --
	// and config.Validate refuses that in STAGING and PROD.
	alerts *alert.Dispatcher
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
	// Built here rather than inline so the alert dispatcher can be joined to
	// it: OnAlert is the seam F-118 recorded as having no production caller,
	// and this is the caller.
	reconMetrics := financialMetrics(log)
	attachAlertDispatcher(reconMetrics, in.alerts, cfg)
	// A resolution-shaped reconciliation engine (F-52). The worker owns
	// detection; this owns the operator's answer to it. No observers, no
	// adapters and no ledger: this plane clears a record, it does not post.
	reconEngine, rerr := reconciliation.NewEngine(reconciliation.Config{
		DB:        database,
		Clock:     clk,
		Records:   reconciliation.NewRepository(clk, outbox, auditWriter),
		Policy:    reconciliation.DefaultPolicy(),
		Approvals: adminSvc,
		// Real instruments rather than NoopMetrics().
		//
		// Both composition roots passed the no-op, so the FinancialMetrics
		// instruments the 29 designed CloudWatch alarms are bound to were never
		// constructed at all -- three independent reasons the alarms would have
		// read green over a system emitting nothing (F-118).
		//
		// otel.Meter returns the global provider's meter, which is a no-op
		// while CP_TELEMETRY_OTLP_ENDPOINT is unset. That is the point: this
		// removes the reason that is SOFTWARE and leaves the one that is a
		// deployment decision, instead of leaving both.
		//
		// Safe to arm only because F-117 bounded the caller-chosen labels
		// first; the order mattered.
		Metrics: reconMetrics,
		Logger:  log,
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
	// A sandbox tier reads SANDBOX rows as active; every other deployment
	// reads them as inactive with a reason. The condition is the one config
	// validated: the SANDBOX legal policy, never in PROD (ADR-0023).
	gateChecker = gateChecker.WithSandbox(cfg.SandboxTier())
	verificationResolver, err := identity.NewVerificationResolver(database)
	if err != nil {
		return nil, fmt.Errorf("verification resolver: %w", err)
	}
	gateAdmin, err := gates.NewAdmin(string(cfg.Env), clk, auditAdapter.GateAudit())
	if err != nil {
		return nil, fmt.Errorf("gate admin: %w", err)
	}
	gateAdmin = gateAdmin.WithSandbox(cfg.SandboxTier())
	if err := sandboxGatesAtBoot(ctx, database, cfg, clk, auditAdapter.GateAudit(), log); err != nil {
		return nil, fmt.Errorf("sandbox gates: %w", err)
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
	// The ceilings are built here rather than inside the Credit purchase
	// wiring: the launch cohort is a ceiling on how many accounts exist, and a
	// deployment with no payment provider still has accounts (F-91).
	capGuard, err := capacity.NewGuard(capacity.Budget{
		MaxAccounts:        cfg.Capacity.MaxAccounts,
		MaxPurchasesPerDay: cfg.Capacity.MaxPurchasesPerDay,
		MaxAtRiskMinor:     cfg.Capacity.MaxAtRiskMinor,
		MaxDatabaseBytes:   cfg.Capacity.MaxDatabaseBytes,
	}, clk.Now)
	if err != nil {
		return nil, fmt.Errorf("capacity ceilings: %w", err)
	}
	log.Info("launch-tier capacity ceilings in force",
		"max_accounts", cfg.Capacity.MaxAccounts,
		"max_purchases_per_day", cfg.Capacity.MaxPurchasesPerDay,
		"max_at_risk_minor", cfg.Capacity.MaxAtRiskMinor,
		"max_database_bytes", cfg.Capacity.MaxDatabaseBytes)

	// Personal data is encrypted before it reaches identity_pii, and the
	// login path is what writes it (F-47). Nil in LOCAL/TEST with no keyring;
	// config.Validate requires one in STAGING and PROD.
	piiStore := newPIIStore(ctx, cfg, in.resolver, log)

	// ---- operator bootstrap (ADR-0024, D-056) ------------------------------
	//
	// Nothing else in this system writes operator_roles, so a deployment that
	// has never had an operator cannot get one and every admin route is
	// unreachable in it. The declaration is configuration, parsed and validated
	// by internal/config (which refuses anything in PROD but empty or exactly
	// one ADMIN); the row is written at the declared identity's next login, and
	// the login then reads the directory exactly as it always did.
	//
	// The line below is the only visible effect at boot, and it is deliberate:
	// a control whose effect is invisible in the logs is a control nobody can
	// check happened.
	bootstrapDecls, err := operatorroles.ParseDeclarations(cfg.Auth.BootstrapOperators)
	if err != nil {
		return nil, fmt.Errorf("operator bootstrap: %w", err)
	}
	operatorBootstrap, err := identity.NewOperatorBootstrap(bootstrapDecls, auditWriter)
	if err != nil {
		return nil, fmt.Errorf("operator bootstrap: %w", err)
	}
	if len(bootstrapDecls) > 0 {
		log.Warn("operator roles declared by configuration; they are granted at the named identity's next login and are recorded in the audit trail",
			"declarations", operatorBootstrap.LogFields(), "actor", operatorroles.ActorID)
	}

	identitySvc, err := identity.New(identity.Deps{
		IdP: idp, DB: database, Accounts: accountRepo, Sessions: sessionMgr,
		Audit: auditWriter, Clock: clk, AttemptTTL: identity.DefaultAttemptTTL,
		PII: piiStore, Operators: operatorBootstrap,
		AdmitAccount: func(ctx context.Context, tx pgx.Tx) error {
			_, aerr := capGuard.Admit(ctx, tx, capacity.ActionOpenAccount)
			return aerr
		},
	})
	if err != nil {
		return nil, fmt.Errorf("identity service: %w", err)
	}

	// ---- profile, terms and account lifecycle ------------------------------
	//
	// The product-level user record (PART 4), terms acceptance (PART 48) and
	// the self-service account lifecycle (PART 4, PART 37). It holds no
	// personal data: identity_pii keeps that, sealed, and ADR-0021 decides who
	// may read it.
	profileSvc, err := profile.New(profile.Deps{
		DB: database, Repo: profile.NewRepository(), Accounts: accountRepo,
		Audit: auditWriter, Clock: clk, Sessions: sessionMgr,
		// The level Nodal establishes by itself, for the operator support view.
		// A resolver that reported NONE where it does not know would be an
		// under-report presented as a fact; a nil one is reported as "not known
		// in this deployment".
		Verification: func(ctx context.Context, accountID accounts.AccountID) (string, error) {
			level, lerr := verificationResolver.Level(ctx, accountID)
			return string(level), lerr
		},
	})
	if err != nil {
		return nil, fmt.Errorf("profile service: %w", err)
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
	creditPurchases := wireCreditPurchase(ctx, cfg, database, in.resolver, clk, creditSvc, gateChecker, capGuard, log)
	// A configured provider that ended up disabled is a page, not a line in
	// a log stream nobody watches (F-93). The path stays disabled and the
	// service keeps serving; what must not happen is that nobody is told.
	raiseIfCreditPathDisabled(in.alerts, cfg, creditPurchases, clk.Now())
	// The launch tier deploys no worker, so the settlement sweep runs here or
	// nowhere -- and nowhere turns the money-at-risk ceiling into a lifetime
	// cumulative cap that refuses every purchase forever (F-90). See
	// runCreditSettlement for why this is acceptable in the API process and
	// what it deliberately does not become responsible for.
	if creditPurchases.Service != nil {
		go runCreditSettlement(ctx, database, creditPurchases.Service, cfg, log)
	}
	// The same answer for the same reason: this deployment has one process, so
	// the periodic work belongs in it. Two passes, both needing cp_ops:
	// login_attempts holds a plaintext OIDC nonce and PKCE verifier per attempt
	// and its purge lives in cmd/audit-worker, which this deployment does not
	// run; and security_events partitions are created and pruned there too,
	// because a table nothing can prune is what fills the ceiling (F-105).
	go runOpsRetention(ctx, cfg, in.lookup, log)
	// And the pass that produces the alert F-118 is about. A destination
	// (internal/alert) with nothing on this deployment raising into it would
	// have been the same silence with a URL attached; see runInternalVerification
	// for what runs here and what deliberately does not.
	go runInternalVerification(ctx, reconEngine, log)
	nativeAssetSvc := nativeasset.NewService(clk, nil)
	nativeMarketSvc := nativemarket.NewService(ledgerSvc, creditSvc, valuation.NewPriceStore(clk), audit.NewWriter(),
		instruments.NewRepository(), nativemarket.NewRiskGate(risk.NewStore(), clk), clk)
	// ---- market safety and the risk policy at boot (product goal SS47) ----
	// The market-safety policy in force: the newest recorded version, or the
	// compiled-in conservative one where none has been recorded. It is a real
	// policy either way, so a deployment that has decided nothing still refuses
	// an order that would move a market by a quarter.
	nativeMarketSvc.SetSafety(nativemarket.NewSafetyStore())
	// And the GLOBAL risk policy, which internal/nativemarket requires before
	// it will evaluate any internal trade at all. Non-PROD only; see
	// riskPolicyAtBoot for why PROD stays manual.
	if err := riskPolicyAtBoot(ctx, database, cfg, clk, log); err != nil {
		return nil, fmt.Errorf("risk policy at boot: %w", err)
	}

	// Payout providers. A registry built with allowSandbox=false refuses any
	// provider with no contract reference, which is the programmatic assertion
	// PART LXV asks for: production cannot load a test double. Nothing is
	// registered, so every payout answers "no provider configured" -- the
	// honest state until a contract exists (BLOCKERS: B-PAYOUT-PROVIDER).
	payoutRegistry := payout.NewRegistry(cfg.Env == config.EnvLocal || cfg.Env == config.EnvTest || cfg.SandboxTier())
	if err := registerSandboxPayoutProvider(cfg, payoutRegistry, clk, log); err != nil {
		return nil, fmt.Errorf("sandbox payout provider: %w", err)
	}
	payoutEngine := payout.NewEngine(creditSvc)
	payoutSvc := payout.NewService(ledgerSvc, creditSvc, payoutEngine, payoutRegistry, clk)
	// Submit and Reconcile had no caller anywhere in cmd/, so a reserved payout
	// sat in VERIFIED forever with the customer's Credits held in
	// PAYOUT_RESERVED and the provider never told. See runPayoutSweeps for why
	// these run here, why fifteen seconds, and why neither pass owns any of the
	// crash-safety (D-085).
	go runPayoutSweeps(ctx, database, payoutSvc, clk, log)

	// ---- verification (goal PARTS 19-25) ---------------------------------
	//
	// The identity boundary. A deployment with no contracted identity vendor
	// registers nothing here, and every verification route answers UNSUPPORTED
	// -- which is the honest state of a system that cannot verify anybody, and
	// is different from reporting that somebody failed a check.
	//
	// The sandbox verification provider is registered on the same one
	// condition every other sandbox affordance keys off (ADR-0023), and it
	// refuses PROD on its own account as well.
	verificationRegistry := verification.NewRegistry(cfg.Env == config.EnvLocal || cfg.Env == config.EnvTest || cfg.SandboxTier())
	if err := registerSandboxVerificationProvider(cfg, verificationRegistry, clk, log); err != nil {
		return nil, fmt.Errorf("sandbox verification provider: %w", err)
	}
	verificationRepo := verification.NewRepository()
	complianceRepo := compliance.NewRepository(audit.NewWriter())
	verificationSvc, err := verification.NewService(verification.Deps{
		Repo:        verificationRepo,
		Compliance:  complianceRepo,
		Providers:   verificationRegistry,
		Clock:       clk,
		Environment: string(cfg.Env),
		SandboxTier: cfg.SandboxTier(),
		ReturnURL:   cfg.Auth.PostLoginURL,
		RefreshURL:  cfg.Auth.PostLoginURL,
	})
	if err != nil {
		return nil, fmt.Errorf("verification service: %w", err)
	}
	// The other half of D-061. The resolver below already reports the base
	// level for a profile whose validity window has elapsed, so nothing an
	// expired verification permits can leave; what was missing was anything
	// that moved the STATE, which left a profile reading VERIFIED while every
	// surface treated the person as unverified, and made §20's EXPIRED -- whose
	// next step is REVERIFY -- a state no deployment could ever reach.
	go runVerificationExpiry(ctx, database, verificationSvc, clk, log)
	// The composite resolver replaces the cap that internal/identity documents:
	// NODAL_IDENTITY is what Nodal establishes by itself, and PAYOUT_KYC and
	// ENHANCED come from a provider decision PLUS the sub-checks that justify
	// it. It composes with the base rather than replacing it, so an account
	// whose owner is not ACTIVE still resolves to NONE however good the KYC
	// evidence is (BLOCKERS B-06 narrows to "no contracted vendor", not "no
	// code").
	compositeVerification, err := verification.NewResolver(verificationResolver, verificationRepo, database, clk)
	if err != nil {
		return nil, fmt.Errorf("verification resolver: %w", err)
	}
	// THE Credit asset, on a sandbox tier that has none. Everything below reads
	// it -- the quote's scale, credit.Service.AssetID, the demo seeder -- and
	// `scripts/seedeconomy`, the only thing that ever wrote one, refuses to run
	// anywhere but LOCAL, DEV and TEST, which are exactly the environments that
	// are not the sandbox tier. See creditAssetAtBoot for why PROD is refused.
	if err := creditAssetAtBoot(ctx, database, cfg, assetRepo, log); err != nil {
		return nil, err
	}
	creditDecimals, err := creditAssetDecimals(ctx, database, assetRepo)
	if err != nil {
		return nil, err
	}
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

	// --- realtime ----------------------------------------------------------
	// The hub is mounted so the endpoint honors Last-Event-ID, heartbeats and
	// per-client cleanup. The producer is the notification follower, wired
	// forty lines below; this comment used to say "no producer is attached ...
	// the stream carries heartbeats only until it is wired", and it went on
	// saying it after the wiring landed, in the file that does the wiring
	// (F-191). The stream is never authoritative either way (PART 109).
	hub := stream.NewHub(1024, log)
	// The stream re-checks its session on every heartbeat. Without it a stolen
	// cookie's stream kept delivering after the victim logged out, after an
	// operator revoked the session, and past the session's own absolute expiry
	// -- because revocation in this system is per request and a stream is one
	// request that never ends (F-116).
	sse := stream.NewHandler(hub, 15*time.Second, func(ctx context.Context, sessionID string) error {
		p, ok := security.PrincipalFrom(ctx)
		if !ok {
			return errs.New(errs.CodeUnauthenticated, "stream: no principal to re-check")
		}
		return sessionMgr.StillLive(ctx, database, p.SubjectID, sessionID)
	})

	// ---- notifications ----------------------------------------------------
	//
	// The producer no longer missing (F-69). Two halves:
	//
	//   1. a follower that reads the transition tables domain services already
	//      write -- credit fundings, payout requests, native fills and market
	//      pauses, account status, login security events -- and turns the rows a
	//      person needs to know about into notifications, idempotently, in one
	//      transaction per source per pass (D-069);
	//   2. the hub, which now carries those notifications and the "this is
	//      stale" signals derived from the same rows, per user, so the frontend
	//      invalidates a query instead of polling (D-071).
	//
	// The hub is given the clock so its event ids encode the instant they were
	// published at: the counter used to restart with the process, and a client
	// reconnecting after a redeploy silently resumed a stream that had lost
	// everything. With a time-encoded id, Last-Event-ID names an instant and
	// notificationResume replays what the table holds since it -- which is the
	// only part of a realtime stream that survives a free instance sleeping.
	hub.UseClock(clk.Now)
	sse.SetResume(notificationResume(database))
	notificationProducer := notifications.NewProducer(clk.Now, cfg.SandboxTier())
	go runNotificationFollower(ctx, database, notifications.NewFollower(notificationProducer),
		hubPublisher{hub: hub}, log)

	legalPolicy, err := legalRouterFor(cfg.Env, in.cfg.API.LegalPolicy)
	if err != nil {
		return nil, err
	}

	ports, err := httpapi.Wire(httpapi.WireDeps{
		DB:                database,
		Clock:             clk,
		Env:               cfg.Env,
		Identity:          identitySvc,
		Profile:           profileSvc,
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
			// The fail-closed default unless the deployment is a sandbox tier,
			// whose rehearsal policy is a named, versioned policy too. A real
			// policy is a persisted version with evidence, never a name here.
			PayoutPolicy: payoutPolicyFor(cfg),
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
			Verification: compositeVerification,
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
		// ---- the withdrawal journey (goal PARTS 19-25) ------------------
		Withdrawal: httpapi.WithdrawalDeps{
			Verification: verificationSvc,
			// The base level, deliberately the NODAL_IDENTITY resolver rather
			// than the composite one: the profile view applies the evidence
			// rule on top, and composing the composite with itself would be
			// circular.
			BaseVerification: verificationResolver,
			Compliance:       complianceRepo,
			Accounts:         accountRepo,
			Pricing:          creditPurchases.Service,
			CreditDecimals:   creditDecimals,
			// The legal registry, read at the moment somebody asks to take
			// value out. §48 puts the withdrawal disclosure there and
			// deliberately not at signup, so the quote and the payout ask for
			// it and onboarding does not (D-083).
			Terms:       profileSvc,
			Environment: string(cfg.Env),
			SandboxTier: cfg.SandboxTier(),
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
				// The only exit from a funding parked in MANUAL_REVIEW. Nil
				// when the Credit purchase provider is not configured, which
				// leaves the kind unregistered rather than half-wired.
				CreditPurchases: creditPurchases.Service,
			}),
		),
		// ---- markets, charts, portfolio and activity (M) ----
		MarketSurfaces: httpapi.MarketSurfacesDeps{
			RiskPolicies: risk.NewStore(),
			// On a sandbox tier no value is real by construction (ADR-0023),
			// so every amount these surfaces return is SIMULATED and says so.
			ActivityFeed: activity.NewFeed(cfg.SandboxTier()),
			Simulated:    cfg.SandboxTier(),
		},
		IdempotencyTTL: httpapi.DefaultIdempotencyTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("wiring: %w", err)
	}
	ports.Stream = sse
	// Scoped to the caller and to nobody else: neither port takes an account id
	// and neither has an account:read_any mode.
	ports.Notifications = httpapi.NewNotificationsPort(database, clk)
	ports.MeAudit = httpapi.NewMeAuditPort(database)

	// ---- agents ----
	//
	// The agent product surface: strategies, agents, the authority and limits
	// their owners grant them, and the lifecycle a person can reach. Nothing
	// here runs an agent. `agentRuntimeDeployment()` states that this
	// deployment runs neither worker, and the API reports NOT_DEPLOYED from it
	// rather than inferring "running" from an agent being enabled.
	//
	// The strategy compiler is deliberately left unconfigured (D-074,
	// ADR-0029). Compiling needs two things this deployment does not have: a
	// model provider credential on the `model` slot, and a validation registry
	// (instruments, venues, tools, composed risk policy) for the compiler's
	// TYPE and RISK_COMPAT stages. With a model and no registry the compiler
	// would reject every instrument a user named and blame the user, which is
	// worse than an honest refusal, so both must arrive together. Until they
	// do, every compile attempt is recorded with outcome MODEL_UNAVAILABLE and
	// the failure code COMPILER_UNAVAILABLE, and the API says exactly that.
	agentSvc, err := agents.NewService(agents.Deps{
		DB:           database,
		Clock:        clk,
		Capabilities: agentCapabilityChecker{checker: gateChecker, q: database},
		Runtime:      agentRuntimeDeployment(),
		Logger:       log,
		BuildVersion: config.BuildVersion,
		StepUpMaxAge: cfg.Auth.StepUpMaxAge,
		// The publisher, which is an adapter in THIS package and not an import
		// in internal/agents (D-073, D-082). It emits one notification, for one
		// case: an agent somebody other than its owner stopped. It writes in a
		// transaction of its own after the agent transaction committed, and
		// keys on the agent_pauses row through the same exported helpers the
		// notification follower uses, so the follower's next pass finds the row
		// already there and tells nobody twice.
		//
		// The timeline needs no publisher at all: internal/activity owns no
		// table and reads agents, agent_pauses and agent_lifecycle_transitions
		// directly, so an agent action is on somebody's timeline because it
		// happened rather than because a hook fired.
		Events: agentNotifier{
			db: database, producer: notificationProducer, hub: hubPublisher{hub: hub}, log: log,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("agents: %w", err)
	}
	strategySvc, err := agents.NewStrategyService(agents.StrategyDeps{
		DB:              database,
		Clock:           clk,
		Compiler:        nil,
		Refs:            nil,
		CompilerVersion: config.BuildVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("strategies: %w", err)
	}
	ports.Agents = agentSvc
	ports.Strategies = strategySvc

	// ---- sandbox demo data (product goal SS51) ----
	// Refused outside a sandbox tier, and refused in PROD three times over.
	// A failure is logged, never fatal: an empty markets page is a nuisance,
	// a deployment that will not start is an outage.
	demoDataAtBoot(ctx, cfg, demoSeedDeps{
		DB: database, Assets: nativeAssetSvc, Markets: nativeMarketSvc, Credits: creditSvc, Clock: clk,
	}, log)

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
		Env:          cfg.Env,
		BuildVersion: config.BuildVersion,
		ConfigHash:   cfg.Hash(),
		SandboxTier:  cfg.SandboxTier(),
		// Anything but live is a rehearsal: test cards, sandbox Credits.
		CreditPurchaseSandbox: cfg.Providers.CreditPurchase.Mode != config.ProviderModeLive,
		PublicBaseURL:         cfg.HTTP.PublicBaseURL,
		CORSOrigins:           cfg.HTTP.CORSOrigins,
		TrustedProxyCIDRs:     cfg.HTTP.TrustedProxyCIDRs,
		MaxBodyBytes:          cfg.HTTP.MaxBodyBytes,
		CookieName:            cookieName,
		CookieDomain:          cfg.Auth.CookieDomain,
		CookieSecure:          cfg.Auth.CookieSecure,
		PostLoginURL:          cfg.Auth.PostLoginURL,
		SessionTTL:            cfg.Auth.SessionTTL,
		StepUpMaxAge:          cfg.Auth.StepUpMaxAge,
		IdempotencyTTL:        httpapi.DefaultIdempotencyTTL,
		Clock:                 clk,
		Logger:                log,
		Meter:                 meter,
		Limits:                limits,
		Authenticator:         httpmw.Session(sessionMgr, database, cookieName),
		NonSpecRoutes:         nonSpecRoutes,
		Ports:                 ports,
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
		// The two slots the product economy runs on. They were missing from
		// this list, so the operator console could not see the Credit
		// purchase adapter or the payout provider at all -- including the
		// sandbox tier's sandbox_payout, which the console must render as
		// sandbox and never as a live rail.
		{"CreditPurchaseProvider", cfg.Providers.CreditPurchase},
		{"PayoutProvider", cfg.Providers.Payout},
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
//
// The vocabulary is config.NormalizeLegalPolicy rather than a switch of its
// own. Both halves have to agree -- config decides whether the deployment is
// valid, this decides whether a router can be built from the same string --
// and when they were separate, config had no list at all (F-103).
func legalRouterFor(env config.Environment, policy string) (*legalrouter.Router, error) {
	name, ok := config.NormalizeLegalPolicy(policy)
	if !ok {
		return nil, fmt.Errorf("%s=%q: expected %s, %s or %s", envLegalPolicy, policy,
			config.LegalPolicyConservative, config.LegalPolicyDevelopment, config.LegalPolicySandbox)
	}
	switch name {
	case config.LegalPolicyConservative:
		return nil, nil
	case config.LegalPolicySandbox:
		// A sandbox tier: not PROD, and config.Validate already refused it
		// there. Refused here again because this is the place the router is
		// built, and a router built for the wrong deployment is the failure
		// that matters.
		if env == config.EnvProd {
			return nil, fmt.Errorf("%s=%s: a sandbox tier cannot be PROD", envLegalPolicy, config.LegalPolicySandbox)
		}
		r, err := legalrouter.New(legalrouter.SandboxPolicy())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envLegalPolicy, err)
		}
		return r, nil
	default:
		if env.IsProductionLike() {
			return nil, fmt.Errorf("%s=%s: a development legal policy may not be loaded in %s",
				envLegalPolicy, config.LegalPolicyDevelopment, env)
		}
		r, err := legalrouter.New(legalrouter.DevelopmentPolicy())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envLegalPolicy, err)
		}
		return r, nil
	}
}
