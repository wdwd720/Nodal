package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/metric"

	"github.com/nodal/controlplane/internal/auth/httpmw"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/observability"
)

// BasePath is the version prefix every route is mounted under (PART 108).
const BasePath = "/v1"

// DefaultMaxBodyBytes bounds a request body when configuration does not.
const DefaultMaxBodyBytes int64 = 1 << 20

// DefaultIdempotencyTTL is how long a completed command's response is
// replayable.
const DefaultIdempotencyTTL = 24 * time.Hour

// Ports is everything the handlers depend on. A nil port makes its operations
// answer UNSUPPORTED; nothing is ever invented in its place.
type Ports struct {
	Identity       IdentityPort
	Sessions       SessionsPort
	Accounts       AccountsPort
	BuyingPower    BuyingPowerPort
	Holdings       HoldingsPort
	Ledger         LedgerPort
	Activity       ActivityPort
	Export         ExportPort
	Assets         AssetsPort
	Instruments    InstrumentsPort
	Quotes         QuotePort
	Intents        IntentsPort
	Orders         OrdersPort
	Funding        FundingPort
	Withdrawals    WithdrawalsPort
	Gates          GatesPort
	KillSwitches   KillSwitchesPort
	AdminActions   AdminActionsPort
	Providers      ProvidersPort
	Reconciliation ReconciliationPort
	// The Nodal-native economy. A nil port answers UNSUPPORTED: a deployment
	// that has not provisioned the internal economy says so rather than
	// returning an empty balance.
	Credits       CreditsPort
	NativeAssets  NativeAssetsPort
	NativeMarkets NativeMarketsPort
	Payouts       PayoutsPort
	Commerce      CommercePort
	// The withdrawal journey (goal PARTS 19-25). A nil port answers
	// UNSUPPORTED: a deployment with no identity vendor and no conversion
	// contract says so, and does not report that somebody failed a check.
	Verification VerificationPort
	Eligibility  EligibilityPort
	Conversion   ConversionPort
	Health       HealthPort
	Idempotency  IdempotencyPort
	// Market discovery, charts, the portfolio and the activity timeline
	// (product goal SS12-16, 35, 47). Nil answers UNSUPPORTED like every
	// other port here.
	MarketData   MarketDataPort
	Portfolio    PortfolioPort
	ActivityFeed ActivityFeedPort
	// ---- profile, terms and account lifecycle ----
	// A nil port answers UNSUPPORTED on the /me/profile, /me/terms-acceptances,
	// /me/account and /admin/users routes, and leaves GET /v1/me answering
	// exactly what it answered before the product surfaces existed.
	Profile ProfilePort
	// ---- notifications, realtime and the customer's own audit trail ----
	//
	// Notifications is the customer's notification centre; MeAudit is their own
	// security and account history. Both are scoped to the caller and to nobody
	// else -- neither has an account:read_any mode -- so neither takes an
	// account id from a path.
	Notifications NotificationsPort
	MeAudit       MeAuditPort
	// ---- agents ----
	//
	// The agent product surface. Both are management only: nothing here runs an
	// agent, evaluates a strategy or emits an intent, and the runtime they
	// describe has no production caller in this build (F-65). A nil port
	// answers UNSUPPORTED rather than an empty list.
	Agents     AgentsPort
	Strategies StrategiesPort

	// Webhooks is keyed by the provider name in the path.
	Webhooks map[string]WebhookPort
	// Stream serves GET /v1/events/stream. It is an http.Handler because
	// SSE owns the connection for its lifetime (internal/stream).
	Stream http.Handler

	// SettlementPolicy is the deployment policy every compiled route is judged
	// against. It is a STRUCT, not an interface, and its zero value is the
	// conservative deployment -- no legal policy on record, no capability
	// active, no verification, unknown jurisdiction -- under which the only
	// thing anybody may do is simulate. A caller that forgets to set it
	// therefore refuses real capital rather than permitting it, which is the
	// opposite of what a nil interface would have done.
	//
	// Its fields are not native-economy specific despite the type's name;
	// Domain A simply needed them first.
	SettlementPolicy NativeEconomyDeps
}

// Options configures the server. Everything here comes from internal/config in
// the composition root; nothing is read from the environment by this package.
type Options struct {
	Env          config.Environment
	BuildVersion string
	ConfigHash   string
	// SandboxTier says whether this deployment is a sandbox tier (ADR-0023).
	// It is published so the UI labels temperatures from what the API says
	// rather than from where it was loaded.
	//
	// There is deliberately no deployment-wide Credit-purchase sandbox flag
	// beside it any more. One existed, and it was the answer to "what mode is
	// this deployment in now" stamped onto every purchase the API returned,
	// including ones opened months earlier under another mode (F-158). A
	// purchase carries the mode that opened it, on its own row.
	SandboxTier       bool
	PublicBaseURL     string
	CORSOrigins       []string
	TrustedProxyCIDRs []string
	MaxBodyBytes      int64
	CookieName        string
	CookieDomain      string
	CookieSecure      bool
	// PostLoginURL is where the OIDC callback sends the browser once the
	// session cookie is set. Empty means the API's own root. When the web app
	// lives on another origin this is its origin; a local return-to path,
	// when one is ever recorded, is resolved beneath it rather than beneath
	// the API's root.
	PostLoginURL string
	SessionTTL   time.Duration
	// StepUpMaxAge is CP_AUTH_STEP_UP_MAX_AGE. It tightens every step-up
	// window the boundary enforces and can never widen one (F-89). Zero
	// leaves the package constant in force.
	StepUpMaxAge   time.Duration
	IdempotencyTTL time.Duration

	Clock  clock.Clock
	Logger *slog.Logger
	Meter  metric.Meter
	Limits RateLimits

	// Authenticator loads the session cookie and attaches the principal.
	// Nil means every request is anonymous, which — because authorization
	// is deny-by-default — makes every non-public operation fail closed.
	Authenticator func(http.Handler) http.Handler

	// NonSpecRoutes mounts handlers outside the /v1 contract, keyed by path.
	// They bypass the generated server and therefore the per-operation
	// authorization table, so New refuses them unless the environment allows
	// development authentication (LOCAL, TEST, DEV). The only user is the
	// development identity provider's picker page; there is no production
	// path that can reach this field.
	NonSpecRoutes map[string]http.Handler

	Ports Ports
}

// Server implements api.StrictServerInterface.
type Server struct {
	opts    Options
	clk     clock.Clock
	log     *slog.Logger
	metrics *httpMetrics
	trusted []*net.IPNet
	router  http.Handler

	// streamCtx is cancelled by StopStreams so long-lived SSE connections
	// end and a graceful shutdown can drain.
	streamCtx   context.Context
	stopStreams context.CancelFunc
	draining    atomic.Bool
}

// StopStreams ends every open server-sent-events connection. The composition
// root calls it at the start of a graceful shutdown: an SSE connection never
// completes on its own, so draining would otherwise block until the deadline.
// It also flips readiness to "not ready" so a load balancer stops sending new
// work while in-flight requests finish.
func (s *Server) StopStreams() {
	s.draining.Store(true)
	if s.stopStreams != nil {
		s.stopStreams()
	}
}

// compile-time proof that every generated operation is implemented.
var _ api.StrictServerInterface = (*Server)(nil)

// New builds the server and its router.
func New(opts Options) (*Server, error) {
	if opts.Clock == nil {
		opts.Clock = clock.System()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.IdempotencyTTL <= 0 {
		opts.IdempotencyTTL = DefaultIdempotencyTTL
	}
	if opts.CookieName == "" {
		opts.CookieName = httpmw.HostPrefix + "cp_session"
	}
	if !opts.Env.IsValid() {
		return nil, errors.New("httpapi: an explicit config.Environment is required")
	}
	if len(opts.NonSpecRoutes) > 0 && !opts.Env.AllowsDevAuth() {
		return nil, fmt.Errorf(
			"httpapi: routes outside the /v1 contract are refused in %s: %v",
			opts.Env, sortedPaths(opts.NonSpecRoutes),
		)
	}
	trusted, err := parseCIDRs(opts.TrustedProxyCIDRs)
	if err != nil {
		return nil, err
	}
	m, err := newHTTPMetrics(opts.Meter)
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts, clk: opts.Clock, log: opts.Logger, metrics: m, trusted: trusted}
	s.streamCtx, s.stopStreams = context.WithCancel(context.Background())
	s.router = s.buildRouter()
	return s, nil
}

// ServeHTTP makes the server the process's http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.router.ServeHTTP(w, r) }

// Router returns the mounted router. Tests exercise the real chain through it.
func (s *Server) Router() http.Handler { return s.router }

func (s *Server) buildRouter() http.Handler {
	r := chi.NewRouter()

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		writeProblem(w, req, errs.New(errs.CodeNotFound, "no such resource"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		p := errs.ToProblem(errs.New(errs.CodeValidationFailed, "method not allowed for this resource"),
			req.URL.Path, observability.RequestID(req.Context()))
		p.Status = http.StatusMethodNotAllowed
		errs.WriteProblem(w, p)
	})

	// The order below is deliberate.
	//
	//   - identifiers first, so everything under them logs the request and
	//     correlation id;
	//   - secure headers and CORS before anything can write a body;
	//   - recovery outside the session load, so a panic anywhere below it
	//     still leaves as problem+json;
	//   - the session load before access logging and before the body is
	//     captured, so the log line, the rate-limit key and the request the
	//     SSE handler receives all carry the principal;
	//   - CSRF and rate limiting after the session load, where the principal is
	//     known -- and BEFORE the body is captured, because capturing it means
	//     reading it into memory, and a request the limiter is going to refuse
	//     should not cost an allocation the caller chose the size of (F-85).
	//     Neither CSRF nor the limiter reads the body: CSRF decides on
	//     Sec-Fetch-Site, Origin and Referer, and the limiter on the principal
	//     or the caller's address.
	//   - the body last, bounded per route by bodyLimitFor.
	r.Use(observability.HTTPMiddleware("controlplane-api"))
	r.Use(httpmw.SecureHeaders(httpmw.SecureHeadersOptions{Secure: s.opts.CookieSecure}))
	r.Use(corsPolicy(s.opts.CORSOrigins))
	r.Use(recoverer(s.log, s.metrics))
	if s.opts.Authenticator != nil {
		r.Use(s.opts.Authenticator)
	}
	r.Use(observe(s.log, s.clk, s.metrics))
	r.Use(s.csrf())
	r.Use(rateLimit(s.opts.Limits, s.trusted))
	r.Use(captureBody(s.opts.MaxBodyBytes))
	r.Use(canonicalPathIdentifiers())

	// Routes outside the /v1 contract are mounted before the generated ones
	// so they cannot shadow a spec route: chi refuses a duplicate pattern,
	// and every /v1 path is claimed below.
	for path, handler := range s.opts.NonSpecRoutes {
		r.Handle(path, handler)
	}

	strict := api.NewStrictHandlerWithOptions(s,
		[]api.StrictMiddlewareFunc{s.authorizeMiddleware()},
		api.StrictHTTPServerOptions{
			RequestErrorHandlerFunc: func(w http.ResponseWriter, req *http.Request, err error) {
				writeProblem(w, req, requestBindingError(err))
			},
			ResponseErrorHandlerFunc: func(w http.ResponseWriter, req *http.Request, err error) {
				writeProblem(w, req, err)
			},
		})

	return api.HandlerWithOptions(strict, api.ChiServerOptions{
		BaseURL:    BasePath,
		BaseRouter: r,
		ErrorHandlerFunc: func(w http.ResponseWriter, req *http.Request, err error) {
			writeProblem(w, req, requestBindingError(err))
		},
	})
}

// csrf applies the Origin/Sec-Fetch-Site check to unsafe methods that carry
// the session cookie. A request with no session cookie carries no ambient
// authority, so it cannot be a cross-site forgery; that is what lets the
// signature-verified webhook endpoint through, and it never relaxes the check
// for a browser request.
func (s *Server) csrf() func(http.Handler) http.Handler {
	origins := append([]string{}, s.opts.CORSOrigins...)
	if s.opts.PublicBaseURL != "" {
		origins = append(origins, s.opts.PublicBaseURL)
	}
	inner := httpmw.CSRF(httpmw.CSRFOptions{AllowedOrigins: origins})
	name := s.opts.CookieName
	return func(next http.Handler) http.Handler {
		guarded := inner(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie(name); err == nil && c.Value != "" {
				guarded.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authorizeMiddleware is the deny-by-default authorization gate. It runs
// inside the strict handler, where the generated operation id is available,
// and before any handler body.
func (s *Server) authorizeMiddleware() api.StrictMiddlewareFunc {
	return func(f api.StrictHandlerFunc, operationID string) api.StrictHandlerFunc {
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			if err := authorize(ctx, operationID, s.clk.Now, effectiveStepUpMaxAge(s.opts.StepUpMaxAge)); err != nil {
				return nil, err
			}
			return f(withOperation(ctx, operationID), w, r, request)
		}
	}
}

// sortedPaths renders a route map deterministically for error messages.
func sortedPaths(m map[string]http.Handler) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type operationKey struct{}

func withOperation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, operationKey{}, id)
}

func operationFrom(ctx context.Context) string {
	id, _ := ctx.Value(operationKey{}).(string)
	return id
}
