package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
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
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/stripe"
	"github.com/nodal/controlplane/internal/ratelimit"
	"github.com/nodal/controlplane/internal/stream"
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
	killController, cerr := killswitch.NewController(clk, auditAdapter.KillSwitchAudit(), httpapi.NewApprovalVerifier(adminSvc))
	if cerr != nil {
		return nil, fmt.Errorf("kill switch controller: %w", cerr)
	}
	killChecker := killswitch.NewChecker(killswitch.Policy{})

	enabledCaps := parseCapabilities(stringEnv(in.lookup, envEnabledCapabilities, ""))
	gateChecker, err := gates.NewChecker(string(cfg.Env), func(c gates.Capability) bool {
		_, ok := enabledCaps[c]
		return ok
	}, clk)
	if err != nil {
		return nil, fmt.Errorf("gate checker: %w", err)
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
		// request reaches the velocity check; the bounds stay at zero
		// (no rolling allowance) until the gate is approved and a policy is
		// written down with it.
		Velocity: withdrawal.VelocityPolicy{},
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
	nativeAssetSvc := nativeasset.NewService(clk, nil)
	nativeMarketSvc := nativemarket.NewService(ledgerSvc, creditSvc, clk)

	// Payout providers. A registry built with allowSandbox=false refuses any
	// provider with no contract reference, which is the programmatic assertion
	// PART LXV asks for: production cannot load a test double. Nothing is
	// registered, so every payout answers "no provider configured" -- the
	// honest state until a contract exists (BLOCKERS: B-PAYOUT-PROVIDER).
	payoutRegistry := payout.NewRegistry(cfg.Env == config.EnvLocal || cfg.Env == config.EnvTest)
	payoutEngine := payout.NewEngine(creditSvc)
	payoutSvc := payout.NewService(ledgerSvc, creditSvc, payoutEngine, payoutRegistry, clk)

	// The ledger's value-domain isolation consults the same gate checker every
	// other capability decision uses, so turning a capability off stops the
	// movement at the journal rather than only in a service.
	ledgerSvc.SetCapabilityResolver(gateCapabilityResolver{checker: gateChecker, q: database})

	// --- realtime ----------------------------------------------------------
	// The hub is mounted so the endpoint honors Last-Event-ID, heartbeats and
	// per-client cleanup. No producer is attached: the event bus adapter lives
	// in a package this binary does not yet depend on, so the stream carries
	// heartbeats only until it is wired. It is never authoritative either way
	// (PART 109).
	hub := stream.NewHub(1024, log)
	sse := stream.NewHandler(hub, 15*time.Second)

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
			Credits:       creditSvc,
			NativeAssets:  nativeAssetSvc,
			NativeMarkets: nativeMarketSvc,
			Payouts:       payoutSvc,
			PayoutEngine:  payoutEngine,
			// No payout policy is configured, so the fail-closed default
			// applies and no origin is withdrawable. Activating one is a
			// policy version with evidence, not a code change here.
			PayoutPolicy: nil,
			Capabilities: gateCapabilityResolver{checker: gateChecker, q: database},
			// No financial verification provider is wired, so every account is
			// VerificationNone. That is not a placeholder: a deployment that
			// cannot establish identity has not established it, and the payout
			// engine refuses accordingly.
			Verification: nil,
		},
		// No execution adapter is wired: quote previews answer
		// PROVIDER_UNAVAILABLE rather than invent a price.
		Quotes: nil,
		// The reconciliation engine is owned by its own binary; the API
		// exposes no resolution path until it is wired.
		Reconcile: nil,
		// Executable kinds are BREAK_GLASS_GRANT (which is what makes any
		// approve-side permission obtainable at all) and KILL_SWITCH_RELEASE.
		// Every other kind answers 422 UNSUPPORTED here: its effect belongs
		// to the domain endpoint that quotes the approval, not to a second
		// path through the action table.
		AdminExecutors: httpapi.AdminExecutors(adminSvc, sessionMgr, killController),
		IdempotencyTTL: httpapi.DefaultIdempotencyTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("wiring: %w", err)
	}
	ports.Stream = sse

	limits, err := rateLimits(clk, cfg.Env, in.lookup)
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
	chain := stringEnv(in.lookup, envSettlementChain, "")
	mint := stringEnv(in.lookup, envSettlementMint, "")
	out := httpapi.FundingSettlement{
		Network:  stringEnv(in.lookup, envFundingNetwork, defaultFundingNetwork),
		Currency: stringEnv(in.lookup, envFundingCurrency, defaultFundingCurrency),
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

// parseRateLimit reads "<requests>/<window>", e.g. "600/1m" or "50/10s".
// "off" (or a zero request count) disables the limiter entirely. An empty
// value takes the default.
func parseRateLimit(spec string, def ratelimit.Limit) (ratelimit.Limit, bool, error) {
	spec = strings.TrimSpace(spec)
	switch {
	case spec == "":
		return def, true, nil
	case strings.EqualFold(spec, "off"), spec == "0":
		return ratelimit.Limit{}, false, nil
	}
	count, window, found := strings.Cut(spec, "/")
	if !found {
		return ratelimit.Limit{}, false, fmt.Errorf("rate limit %q must be \"<requests>/<window>\", e.g. 600/1m", spec)
	}
	requests, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil || requests < 0 {
		return ratelimit.Limit{}, false, fmt.Errorf("rate limit %q has a non-numeric request count", spec)
	}
	if requests == 0 {
		return ratelimit.Limit{}, false, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(window))
	if err != nil || d <= 0 {
		return ratelimit.Limit{}, false, fmt.Errorf("rate limit %q has an invalid window", spec)
	}
	limit := ratelimit.Limit{Requests: requests, Window: d}
	if verr := limit.Validate(); verr != nil {
		return ratelimit.Limit{}, false, fmt.Errorf("rate limit %q: %w", spec, verr)
	}
	return limit, true, nil
}

// rateLimits builds the four transport limiters from configuration, defaulting
// to the values above. The limiter behavior itself is internal/ratelimit's:
// 429 with RateLimit-Limit, RateLimit-Remaining, RateLimit-Reset and
// Retry-After, and problem+json.
//
// The counters live in process memory, so the budget is per replica. That is
// deliberate for now: a Redis outage must never take the API down, and these
// counters are not financial authority (PART 181).
func rateLimits(clk clock.Clock, env config.Environment, lookup func(string) (string, bool)) (httpapi.RateLimits, error) {
	store := ratelimit.NewMemoryStore()
	build := func(name string) (*ratelimit.Limiter, error) {
		raw, _ := lookup(name)
		limit, enabled, err := parseRateLimit(raw, defaultRateLimits[name])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if !enabled {
			if env.IsProductionLike() {
				return nil, fmt.Errorf("%s: %w", name, errRateLimitDisabledInProduction)
			}
			return nil, nil
		}
		// failOpen: a limiter store failure must never refuse a request that
		// the financial rules would have allowed.
		return ratelimit.NewLimiter(strings.ToLower(name), store, limit, clk.Now, true)
	}

	var (
		out httpapi.RateLimits
		err error
	)
	if out.General, err = build(envRateLimitGeneral); err != nil {
		return httpapi.RateLimits{}, err
	}
	if out.Auth, err = build(envRateLimitAuth); err != nil {
		return httpapi.RateLimits{}, err
	}
	if out.Quote, err = build(envRateLimitQuote); err != nil {
		return httpapi.RateLimits{}, err
	}
	if out.Command, err = build(envRateLimitCommand); err != nil {
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
