package config

import (
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/ratelimit"
)

// Rule names a validation rule. Every Violation carries one so that tests and
// operators can identify exactly which rule failed.
type Rule string

// Rules applied in every environment.
const (
	// RuleField is a basic well-formedness violation (empty, unparsable,
	// out of range).
	RuleField Rule = "FIELD"
	// RuleSecretRefScheme rejects SecretRef schemes not permitted in the
	// environment (plain outside LOCAL/TEST, file:// outside LOCAL/TEST/DEV).
	RuleSecretRefScheme Rule = "SECRET_REF_SCHEME" // #nosec G101 -- validation rule name, not a credential
	// RuleOIDCConfigured requires issuer, client id, client secret and
	// redirect URL when auth mode is oidc (https in STAGING/PROD).
	RuleOIDCConfigured Rule = "OIDC_CONFIGURED"
	// RuleNoDevAuth rejects auth mode "dev" outside LOCAL/TEST/DEV.
	RuleNoDevAuth Rule = "NO_DEV_AUTH"
	// RuleNoFakeProviders rejects provider mode "fake" in STAGING/PROD.
	RuleNoFakeProviders Rule = "NO_FAKE_PROVIDERS"
	// RuleProviderModeMatchesEnv requires the provider mode and the
	// environment to make the same claim about whose money is moving: PROD is
	// live, STAGING is the provider's sandbox.
	RuleProviderModeMatchesEnv Rule = "PROVIDER_MODE_MATCHES_ENV"
)

// SecurityEventRetentionFloorDays is the shortest security-event retention
// window this system will accept once pruning is switched on at all.
//
// It is duplicated in the database, in cp_security_events_drop_expired (00740),
// and that is deliberate rather than an oversight of the kind this register
// keeps recording. The two copies do not guard the same thing: this one refuses
// a bad value at boot, where an operator sees it; the SQL one refuses a bad
// argument at call time, where an attacker holding the operations credential
// would be. A floor that only exists in Go is a floor an attacker can step over
// by calling the function directly.
const SecurityEventRetentionFloorDays = 90

// Rules applied only when Environment.IsProductionLike (STAGING, PROD).
const (
	RuleNoDebugAuth       Rule = "NO_DEBUG_AUTH"
	RuleNoSeed            Rule = "NO_SEED"
	RuleDatabaseTLS       Rule = "DATABASE_TLS"
	RuleRedisTLS          Rule = "REDIS_TLS"
	RuleRedpandaTLS       Rule = "REDPANDA_TLS"
	RuleClickHouseTLS     Rule = "CLICKHOUSE_TLS"
	RuleTemporalTLS       Rule = "TEMPORAL_TLS"
	RuleNoCORSWildcard    Rule = "NO_CORS_WILDCARD"
	RuleArchiveConfigured Rule = "ARCHIVE_CONFIGURED"
	RuleArchiveObjectLock Rule = "ARCHIVE_OBJECT_LOCK"
	RuleKMSConfigured     Rule = "KMS_CONFIGURED"
	RuleCookieSecure      Rule = "COOKIE_SECURE"
	// RuleTrustedProxyDeclared: a STAGING or PROD deployment says which
	// networks its load balancer speaks from.
	//
	// Every deployment of this service sits behind something that terminates
	// TLS, so the peer address the process sees is the balancer's and is the
	// same for every caller. With no trusted networks declared, clientIP
	// returns that address -- so every audit record names the balancer, and
	// every unauthenticated rate-limit bucket collapses into one shared
	// counter. The auth budget is then 30 requests a minute for the entire
	// cohort together, which is not a per-client control at all (F-88).
	//
	// The list is what makes X-Forwarded-For readable, and reading it is the
	// only way to tell callers apart. It is required rather than defaulted
	// because the answer depends on the platform, and a default would be a
	// guess that silently trusts the wrong thing.
	RuleTrustedProxyDeclared Rule = "TRUSTED_PROXY_DECLARED"
	RulePublicBaseURLHTTPS   Rule = "PUBLIC_BASE_URL_HTTPS"
	RuleRetentionNonZero     Rule = "RETENTION_NON_ZERO"
	// RuleLegalPolicyNotDevelopment: a STAGING or PROD deployment does not name
	// the permissive jurisdiction policy. cmd/api refuses to start on one, so
	// without this the configuration check passes a deployment that cannot boot.
	RuleLegalPolicyNotDevelopment Rule = "LEGAL_POLICY_NOT_DEVELOPMENT"
	// RuleCookieHostOnly: a STAGING or PROD deployment sets no cookie Domain,
	// because a Domain removes the __Host- prefix and the prefix is the only
	// thing binding the session and login-state cookies to one host.
	RuleCookieHostOnly    Rule = "COOKIE_HOST_ONLY"
	RulePublicProductName Rule = "PUBLIC_PRODUCT_NAME"
	RuleCapabilityStore   Rule = "CAPABILITY_STORE_CONFIGURED"
	// RuleRateLimitStated rejects a transport budget that cannot be parsed,
	// and one switched off where money is at stake.
	RuleRateLimitStated Rule = "RATE_LIMIT_STATED"
	// RuleDistributedRateLimit rejects a per-process rate-limit store on a
	// binary that serves public HTTP in STAGING/PROD, where it runs as more
	// than one replica.
	RuleDistributedRateLimit Rule = "DISTRIBUTED_RATE_LIMIT"
	// RuleCapacityCeiling requires a binary that takes money to state at least
	// one ceiling on how much it may take or store.
	RuleCapacityCeiling Rule = "CAPACITY_CEILING"
	// RuleSettlementAsset requires the API to name the asset funds settle
	// into before it will serve production traffic.
	RuleSettlementAsset Rule = "SETTLEMENT_ASSET"
	// RuleNoInsecureOTLP applies to PROD only: telemetry must be exported
	// over TLS.
	RuleNoInsecureOTLP Rule = "NO_INSECURE_OTLP"
)

// Violation is one failed validation rule.
type Violation struct {
	Rule   Rule
	Field  string
	Detail string
}

func (v *Violation) Error() string {
	return fmt.Sprintf("config: rule %s: %s: %s", v.Rule, v.Field, v.Detail)
}

// Violations flattens err (typically the joined error from Validate) into
// the individual violations it contains.
func Violations(err error) []*Violation {
	var out []*Violation
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if v, ok := e.(*Violation); ok {
			out = append(out, v)
			return
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return out
}

// HasViolation reports whether err contains a violation of rule.
func HasViolation(err error, rule Rule) bool {
	for _, v := range Violations(err) {
		if v.Rule == rule {
			return true
		}
	}
	return false
}

// Validate checks every rule and returns all violations joined into one
// error (nil when valid). Environment-independent rules run everywhere;
// production rules run when Env.IsProductionLike. Validation never applies a
// default: it only rejects.
func (c *Config) Validate() error {
	var errs []error
	add := func(rule Rule, field, detail string) {
		errs = append(errs, &Violation{Rule: rule, Field: field, Detail: detail})
	}

	if !c.Env.IsValid() {
		add(RuleField, "Env", fmt.Sprintf("unknown environment %q", string(c.Env)))
		return errors.Join(errs...)
	}
	env := c.Env
	prodLike := env.IsProductionLike()

	// Which external systems this binary actually uses. An unknown or unset
	// Service requires nothing, and that is the safe direction here rather than
	// the dangerous one: Load refuses an unknown service before Validate is
	// ever reached, so the only way to arrive here without one is a Config
	// somebody built by hand in a test, which is not a deployment.
	if c.Service != "" && !c.Service.Valid() {
		add(RuleField, "Service", fmt.Sprintf("unknown service %q", string(c.Service)))
		return errors.Join(errs...)
	}
	needsRedis := c.RequiresDependency(DepRedis)
	needsRedpanda := c.Service.Requires(DepRedpanda)
	needsClickHouse := c.Service.Requires(DepClickHouse)
	needsTemporal := c.Service.Requires(DepTemporal)
	// The archive backend decides whether the object-store values are needed
	// at all, so it is validated before anything reads needsArchive.
	if c.Archive.Backend != "" && !c.Archive.Backend.IsValid() {
		add(RuleField, "Archive.Backend", fmt.Sprintf("unknown backend %q", string(c.Archive.Backend)))
	}
	needsArchive := c.RequiresDependency(DepArchive)

	// ---- core --------------------------------------------------------------
	if c.ServiceName == "" {
		add(RuleField, "ServiceName", "must not be empty")
	}
	if prodLike && c.PublicProductName == "" {
		add(RulePublicProductName, "PublicProductName", "must be set in STAGING/PROD")
	}

	// ---- HTTP --------------------------------------------------------------
	if c.HTTP.Addr == "" {
		add(RuleField, "HTTP.Addr", "must not be empty")
	}
	if u, err := parseHTTPURL(c.HTTP.PublicBaseURL); err != nil {
		add(RuleField, "HTTP.PublicBaseURL", err.Error())
	} else if prodLike && u.Scheme != "https" {
		add(RulePublicBaseURLHTTPS, "HTTP.PublicBaseURL", "must use https in STAGING/PROD")
	}
	for name, d := range map[string]int64{
		"HTTP.ReadTimeout":  int64(c.HTTP.ReadTimeout),
		"HTTP.WriteTimeout": int64(c.HTTP.WriteTimeout),
		"HTTP.IdleTimeout":  int64(c.HTTP.IdleTimeout),
		"HTTP.MaxBodyBytes": c.HTTP.MaxBodyBytes,
	} {
		if d <= 0 {
			add(RuleField, name, "must be > 0")
		}
	}
	for _, o := range c.HTTP.CORSOrigins {
		if o == "*" {
			if prodLike {
				add(RuleNoCORSWildcard, "HTTP.CORSOrigins", "wildcard origin is not allowed in STAGING/PROD")
			}
			continue
		}
		if u, err := parseHTTPURL(o); err != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			add(RuleField, "HTTP.CORSOrigins", fmt.Sprintf("%q is not an origin (scheme://host[:port])", o))
		}
	}
	for _, cidr := range c.HTTP.TrustedProxyCIDRs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			add(RuleField, "HTTP.TrustedProxyCIDRs", fmt.Sprintf("%q is not a CIDR", cidr))
			continue
		}
		// A default route trusts every peer, and a trusted peer's
		// X-Forwarded-For is taken at face value. So `0.0.0.0/0` does not widen
		// the control -- it inverts it: every caller chooses the address that
		// lands in the audit record, in login_attempts, and in every
		// unauthenticated rate-limit bucket including the auth budget. F-88
		// made this list mandatory; nothing made it mean anything (F-103).
		if ones, _ := network.Mask.Size(); ones == 0 {
			add(RuleField, "HTTP.TrustedProxyCIDRs", fmt.Sprintf(
				"%q trusts every peer, which lets any caller forge its own address in "+
					"audit records and rate-limit buckets; name the networks the load balancer speaks from", cidr,
			))
		}
	}
	// The legal policy names a router the composition root will build. A name
	// it does not know, or a development policy in a production-like
	// environment, is refused at startup -- so without a rule here the
	// deployment passes every configuration check and then will not boot,
	// which is the failure test/infra records for the settlement asset,
	// reproduced by a variable that is in the table and has no rule (F-103).
	if policy, ok := NormalizeLegalPolicy(c.API.LegalPolicy); !ok {
		add(RuleField, "API.LegalPolicy",
			fmt.Sprintf("%q is not a legal policy; expected %s or %s", c.API.LegalPolicy,
				LegalPolicyConservative, LegalPolicyDevelopment))
	} else if prodLike && policy == LegalPolicyDevelopment {
		add(RuleLegalPolicyNotDevelopment, "API.LegalPolicy",
			"a development legal policy may not be loaded in STAGING/PROD: it permits the internal economy without the jurisdiction questions the conservative policy asks")
	}

	if c.Credit.SettlementWindow <= 0 {
		add(RuleField, "Credit.SettlementWindow", "must be > 0: a zero window settles a payment the instant it is captured")
	}
	if prodLike && len(c.HTTP.TrustedProxyCIDRs) == 0 {
		add(RuleTrustedProxyDeclared, "HTTP.TrustedProxyCIDRs",
			"must name the networks the load balancer speaks from in STAGING/PROD: without them "+
				"every caller looks like the balancer, so audit records its address and every "+
				"unauthenticated rate-limit bucket becomes one shared counter")
	}

	// ---- database ----------------------------------------------------------
	if c.Database.AppURL.IsZero() {
		add(RuleField, "Database.AppURL", "must not be empty")
	}
	// Database.MigrateURL is deliberately NOT required (F-93).
	//
	// It is the schema-owner credential, and the owner can DISABLE TRIGGER --
	// which is what every state machine in this system now rests on. Requiring
	// it here meant the internet-facing binary had to hold it, and nothing that
	// loads configuration reads it: cmd/migrate takes it from the environment
	// itself. A rule demanding a credential nobody uses is a rule that hands out
	// a credential.
	//
	// It is still parsed when supplied, so a malformed value fails closed for
	// whoever supplies it.
	if c.Database.MaxConns < 1 {
		add(RuleField, "Database.MaxConns", "must be >= 1")
	}
	if c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		add(RuleField, "Database.MinConns", "must be between 0 and MaxConns")
	}
	// A dial must be bounded, and bounded below the request it serves.
	//
	// The second half is the one worth stating: an unbounded or over-long
	// connect on a database that suspends means the request that opened it has
	// already been abandoned by the time the connection arrives, and the slot it
	// took belonged to the next request. The API's own request deadline is the
	// natural ceiling, so the rule is derived rather than picked (F-93).
	if c.Database.ConnectTimeout <= 0 {
		add(RuleField, "Database.ConnectTimeout", "must be > 0: an unbounded dial outlives the request that needed it")
	}
	if c.Service.ServesHTTP() && c.API.RequestTimeout > 0 && c.Database.ConnectTimeout >= c.API.RequestTimeout {
		add(RuleField, "Database.ConnectTimeout",
			"must be shorter than CP_API_REQUEST_TIMEOUT: a connect that outlives its request holds a pool slot for nobody")
	}
	if c.Database.MaxConnIdleTime < 0 {
		add(RuleField, "Database.MaxConnIdleTime", "must not be negative")
	}
	if c.Database.StatementTimeout <= 0 {
		add(RuleField, "Database.StatementTimeout", "must be > 0")
	}
	if c.Database.LockTimeout <= 0 {
		add(RuleField, "Database.LockTimeout", "must be > 0")
	}
	if prodLike && !c.Database.RequireTLS {
		add(RuleDatabaseTLS, "Database.RequireTLS", "must be true in STAGING/PROD")
	}
	if prodLike && !c.Capability.StoreConfigured {
		add(RuleCapabilityStore, "Capability.StoreConfigured", "capability store (application database) must be configured in STAGING/PROD")
	}

	// ---- the transport budgets ---------------------------------------------
	//
	// Checked here so that an unparseable spec is a failed configuration check
	// rather than a binary that will not start, and so that "off" is refused
	// before a deployment carrying it goes anywhere near production.
	if c.Service.ServesHTTP() {
		for field, spec := range map[string]string{
			"RateLimit.General": c.RateLimit.General,
			"RateLimit.Auth":    c.RateLimit.Auth,
			"RateLimit.Quote":   c.RateLimit.Quote,
			"RateLimit.Command": c.RateLimit.Command,
		} {
			_, enabled, err := ratelimit.ParseLimit(spec, ratelimit.Limit{Requests: 1, Window: time.Minute})
			switch {
			case err != nil:
				add(RuleRateLimitStated, field, err.Error())
			case !enabled && prodLike:
				add(RuleRateLimitStated, field,
					"a transport rate limit may not be switched off in STAGING/PROD")
			}
		}
	}

	// ---- the settlement asset ----------------------------------------------
	//
	// cmd/api refuses to start in STAGING or PROD without both, and used to
	// discover that after configuration had already passed -- because it read
	// them from the environment directly and they were in no table. Checking
	// here is what lets scripts/configcheck and the deployment tests catch it
	// before anything is deployed.
	if c.Service.ServesHTTP() && prodLike {
		if strings.TrimSpace(c.API.SettlementChain) == "" {
			add(RuleSettlementAsset, "API.SettlementChain", "must name the chain of the settlement asset in STAGING/PROD")
		}
		if strings.TrimSpace(c.API.SettlementMint) == "" {
			add(RuleSettlementAsset, "API.SettlementMint", "must name the mint of the settlement asset in STAGING/PROD")
		}
	}

	// ---- capacity ceilings -------------------------------------------------
	//
	// A deployment that takes money states what it is prepared to owe. An
	// individual ceiling may be zero, because paid infrastructure has no
	// database quota to guard -- but all four being zero means nothing bounds
	// how much this deployment may take or store, and that is a configuration
	// nobody would choose deliberately and anybody could reach by deleting a
	// line.
	if c.Service.ServesHTTP() {
		for name, v := range map[string]int64{
			"Capacity.MaxAccounts":        c.Capacity.MaxAccounts,
			"Capacity.MaxPurchasesPerDay": c.Capacity.MaxPurchasesPerDay,
			"Capacity.MaxAtRiskMinor":     c.Capacity.MaxAtRiskMinor,
			"Capacity.MaxDatabaseBytes":   c.Capacity.MaxDatabaseBytes,
		} {
			if v < 0 {
				add(RuleField, name, "must not be negative")
			}
		}
		if c.Capacity.MaxAccounts == 0 && c.Capacity.MaxPurchasesPerDay == 0 &&
			c.Capacity.MaxAtRiskMinor == 0 && c.Capacity.MaxDatabaseBytes == 0 {
			add(RuleCapacityCeiling, "Capacity",
				"every ceiling is zero, so nothing bounds how much this deployment may take or store; state at least one")
		}
		// In STAGING and PROD the three ceilings about people and money must
		// each be stated, not merely one of them.
		//
		// Zero means "this ceiling does not apply" to the guard, and that is
		// the right library semantics -- MaxDatabaseBytes is genuinely zero on
		// paid infrastructure with no quota. What it must not be is reachable
		// by a one-character edit in a dashboard on a deployment that takes
		// money: setting CP_CAPACITY_MAX_AT_RISK_MINOR to 0 loaded cleanly,
		// passed validation, and logged "launch-tier capacity ceilings in
		// force" with the cap that matters most silently off (F-97).
		//
		// MaxDatabaseBytes is deliberately not in this list: a deployment on
		// managed Postgres with no storage quota has nothing to state.
		if prodLike {
			for name, v := range map[string]int64{
				"Capacity.MaxAccounts":        c.Capacity.MaxAccounts,
				"Capacity.MaxPurchasesPerDay": c.Capacity.MaxPurchasesPerDay,
				"Capacity.MaxAtRiskMinor":     c.Capacity.MaxAtRiskMinor,
			} {
				if v == 0 {
					add(RuleCapacityCeiling, name,
						"must be stated in STAGING/PROD: zero disables this ceiling, and a deployment that "+
							"takes money says what it is prepared to owe")
				}
			}
		}
	}

	// ---- rate limiting -----------------------------------------------------
	//
	// A rate limit is a budget, and an in-memory store gives each replica its
	// own copy of it. The API autoscales from three tasks, so a limit of 100
	// admits 300 requests and twelve times that at maximum capacity -- the
	// configured number is then not the enforced number, which is worse than a
	// wrong limit because it reads as a right one.
	//
	// So a binary that serves HTTP must either share its counters or run as a
	// single process, and it must SAY which -- in every environment, because
	// three DEV replicas get the limit just as wrong as three PROD ones.
	if c.Service.ServesHTTP() {
		if c.RateLimit.Replicas < 1 {
			add(RuleField, "RateLimit.Replicas",
				"must be at least 1; a binary that serves HTTP runs at least one process")
		}
		switch {
		case c.RateLimit.Backend == "":
			// Only reachable outside LOCAL/TEST, where no default is applied.
			// Load already reports the absence; saying it twice is worse than
			// saying it once, so this only names the production consequence.
			if prodLike {
				add(RuleDistributedRateLimit, "RateLimit.Backend",
					"must be set to a distributed backend (redis) in STAGING/PROD")
			}
		case !c.RateLimit.Backend.IsValid():
			add(RuleField, "RateLimit.Backend",
				fmt.Sprintf("unknown backend %q", string(c.RateLimit.Backend)))
		case c.RateLimit.Replicas > 1 && !c.RateLimit.Backend.Distributed():
			// The condition is the replica count, not the environment.
			//
			// A rate limit is a budget and process-local counters give each
			// process its own copy, so the configured limit is the enforced
			// limit exactly when there is one process. The environment was a
			// proxy for that and was wrong both ways: it refused a correct
			// single-process production deployment, and permitted an incorrect
			// three-process DEV one. This refuses the second as well.
			add(RuleDistributedRateLimit, "RateLimit.Backend",
				fmt.Sprintf("%q keeps counters in the process and this deployment declares %d of them, so a limit of N would admit %d*N; use redis or run one",
					string(c.RateLimit.Backend), c.RateLimit.Replicas, c.RateLimit.Replicas))
		}
	}

	// ---- redis -------------------------------------------------------------
	//
	// The presence rules below are asked only of a service that declares the
	// dependency. The FORMAT rules are not conditional: a malformed value is a
	// mistake whoever set it wants to hear about, whether or not this binary
	// would have dialled it. That split is why each block tests presence under
	// `needs` and parses unconditionally.
	if needsRedis {
		if c.Redis.URL.IsZero() {
			add(RuleField, "Redis.URL", "must not be empty")
		}
		if prodLike && !c.Redis.RequireTLS {
			add(RuleRedisTLS, "Redis.RequireTLS", "must be true in STAGING/PROD")
		}
	}

	// ---- redpanda ----------------------------------------------------------
	if needsRedpanda {
		if len(c.Redpanda.Brokers) == 0 {
			add(RuleField, "Redpanda.Brokers", "at least one broker is required")
		}
		if prodLike && !c.Redpanda.RequireTLS {
			add(RuleRedpandaTLS, "Redpanda.RequireTLS", "must be true in STAGING/PROD")
		}
	}
	if c.Redpanda.SASLMechanism != "" {
		if c.Redpanda.SASLUsernameRef.IsZero() {
			add(RuleField, "Redpanda.SASLUsernameRef", "required when SASLMechanism is set")
		}
		if c.Redpanda.SASLPasswordRef.IsZero() {
			add(RuleField, "Redpanda.SASLPasswordRef", "required when SASLMechanism is set")
		}
	}

	// ---- clickhouse --------------------------------------------------------
	if needsClickHouse {
		if c.ClickHouse.Addr == "" {
			add(RuleField, "ClickHouse.Addr", "must not be empty")
		}
		if c.ClickHouse.Database == "" {
			add(RuleField, "ClickHouse.Database", "must not be empty")
		}
		if prodLike && !c.ClickHouse.RequireTLS {
			add(RuleClickHouseTLS, "ClickHouse.RequireTLS", "must be true in STAGING/PROD")
		}
	}

	// ---- temporal ----------------------------------------------------------
	if needsTemporal {
		if c.Temporal.HostPort == "" {
			add(RuleField, "Temporal.HostPort", "must not be empty")
		}
		if c.Temporal.Namespace == "" {
			add(RuleField, "Temporal.Namespace", "must not be empty")
		}
		if c.Temporal.TaskQueuePrefix == "" {
			add(RuleField, "Temporal.TaskQueuePrefix", "must not be empty")
		}
		if prodLike && !c.Temporal.RequireTLS {
			add(RuleTemporalTLS, "Temporal.RequireTLS", "must be true in STAGING/PROD")
		}
	}

	// ---- archive -----------------------------------------------------------
	if c.Archive.Endpoint != "" {
		if _, err := parseHTTPURL(c.Archive.Endpoint); err != nil {
			add(RuleField, "Archive.Endpoint", err.Error())
		}
	}
	if prodLike && needsArchive {
		for name, v := range map[string]string{
			"Archive.Region":         c.Archive.Region,
			"Archive.RawBucket":      c.Archive.RawBucket,
			"Archive.EvidenceBucket": c.Archive.EvidenceBucket,
			"Archive.AuditBucket":    c.Archive.AuditBucket,
		} {
			if v == "" {
				add(RuleArchiveConfigured, name, "must be set in STAGING/PROD")
			}
		}
		// Object Lock is a compliance control on the audit bucket, and it is
		// asked of the services that write to an archive. A worker with no
		// archive being required to declare Object Lock on a bucket it never
		// touches is not a control; it is a value somebody sets to true to make
		// a startup error go away, which is how controls stop meaning anything.
		if !c.Archive.ObjectLockRequired {
			add(RuleArchiveObjectLock, "Archive.ObjectLockRequired", "audit bucket Object Lock must be required in STAGING/PROD")
		}
	}

	// ---- kms ---------------------------------------------------------------
	if c.KMS.AuditSigningKeyID != "" && c.KMS.Region == "" {
		add(RuleField, "KMS.Region", "required when AuditSigningKeyID is set")
	}
	if prodLike && c.RequiresDependency(DepKMS) && c.KMS.AuditSigningKeyID == "" {
		add(RuleKMSConfigured, "KMS.AuditSigningKeyID", "must be set in STAGING/PROD")
	}

	// ---- auth --------------------------------------------------------------
	switch c.Auth.Mode {
	case AuthModeOIDC:
		for name, v := range map[string]string{
			"Auth.Issuer":      c.Auth.Issuer,
			"Auth.ClientID":    c.Auth.ClientID,
			"Auth.RedirectURL": c.Auth.RedirectURL,
		} {
			if v == "" {
				add(RuleOIDCConfigured, name, "required when Auth.Mode is oidc")
			}
		}
		if c.Auth.ClientSecretRef.IsZero() {
			add(RuleOIDCConfigured, "Auth.ClientSecretRef", "required when Auth.Mode is oidc")
		}
		for name, v := range map[string]string{"Auth.Issuer": c.Auth.Issuer, "Auth.RedirectURL": c.Auth.RedirectURL} {
			if v == "" {
				continue
			}
			u, err := parseHTTPURL(v)
			if err != nil {
				add(RuleOIDCConfigured, name, err.Error())
			} else if prodLike && u.Scheme != "https" {
				add(RuleOIDCConfigured, name, "must use https in STAGING/PROD")
			}
		}
	case AuthModeDev:
		if !env.AllowsDevAuth() {
			add(RuleNoDevAuth, "Auth.Mode", "dev auth is only allowed in LOCAL/TEST/DEV")
		}
	default:
		add(RuleField, "Auth.Mode", fmt.Sprintf("unknown auth mode %q (want oidc|dev)", c.Auth.Mode))
	}
	if c.Auth.CookieName == "" {
		add(RuleField, "Auth.CookieName", "must not be empty")
	}
	if c.Auth.SessionTTL <= 0 {
		add(RuleField, "Auth.SessionTTL", "must be > 0")
	}
	if c.Auth.StepUpMaxAge <= 0 {
		add(RuleField, "Auth.StepUpMaxAge", "must be > 0")
	}
	if prodLike && c.Auth.DebugAuthEnabled {
		add(RuleNoDebugAuth, "Auth.DebugAuthEnabled", "must be false in STAGING/PROD")
	}
	// A cookie Domain removes the __Host- prefix, and the prefix is the whole
	// binding.
	//
	// httpmw.EffectiveCookieName adds __Host- only when the cookie is secure
	// AND host-only, and SetSessionCookie sets c.Domain only when the name is
	// unprefixed. So naming a domain silently turns both the session cookie and
	// the login-state cookie into ordinary domain cookies -- writable by any
	// host that can set a cookie for a suffix of that domain.
	//
	// That re-opens F-87. The login-state cookie is a SHA-256 of the state with
	// no server secret, so an attacker who starts their own sign-in knows the
	// digest, and being able to write the cookie lets them plant a callback
	// that signs the victim's browser in as them -- the exact attack
	// SetLoginState was written to stop. The session cookie half is classic
	// fixation.
	//
	// Nothing refused it, and infra/terraform's PROD example sets it (F-112).
	if prodLike && strings.TrimSpace(c.Auth.CookieDomain) != "" {
		add(RuleCookieHostOnly, "Auth.CookieDomain",
			"must be empty in STAGING/PROD: a Domain attribute removes the __Host- prefix, which is what binds the session and login-state cookies to one host")
	}
	if prodLike && !c.Auth.CookieSecure {
		add(RuleCookieSecure, "Auth.CookieSecure", "must be true in STAGING/PROD")
	}

	// ---- providers ---------------------------------------------------------
	for _, slot := range providerSlots() {
		p := slot.Get(&c.Providers)
		field := "Providers." + slot.Name
		if !p.Mode.IsValid() {
			add(RuleField, field+".Mode", fmt.Sprintf("unknown provider mode %q", string(p.Mode)))
			continue
		}
		if p.Mode == ProviderModeFake && !env.AllowsFakeProviders() {
			add(RuleNoFakeProviders, field+".Mode", "fake providers are not allowed in STAGING/PROD")
		}
		// The environment and the mode must make the same claim about whose
		// money is moving. PROD in sandbox mode mints value against test
		// objects nobody paid for; STAGING in live mode charges real cards for
		// a rehearsal. Both adapters that take money refuse to be built on a
		// mismatch, but they refuse at startup, where the symptom is a WARN
		// and a silently disabled capability. Refusing here is what lets
		// scripts/configcheck and the deployment tests see it first.
		//
		// Stated as a rule about the MODE rather than a switch over
		// environments, because the switch listed PROD and STAGING and every
		// other environment fell through it. DEV therefore accepted live mode
		// -- alongside CP_AUTH_MODE=dev, a wildcard CORS origin, a non-secure
		// cookie, no TLS to the database and no rate limit, none of which DEV
		// constrains either. The same configuration file was refused by twenty
		// rules at CP_ENV=PROD and passed clean at CP_ENV=DEV. The two
		// money-taking adapters carry the same two-case switch and the same
		// hole, so the belt-and-braces had it too (F-103).
		//
		// A total rule cannot acquire that hole again when a sixth environment
		// is declared.
		if p.Mode == ProviderModeLive && env != EnvProd {
			add(RuleProviderModeMatchesEnv, field+".Mode",
				fmt.Sprintf("live mode is only permitted in PROD, not %s; a deployment that is not production must not hold credentials that move real money", env))
		}
		if env == EnvProd && p.Mode != ProviderModeFake && p.Mode != ProviderModeLive {
			add(RuleProviderModeMatchesEnv, field+".Mode",
				fmt.Sprintf("PROD requires live mode, not %q; a production deployment must not run against a provider's test environment", string(p.Mode)))
		}
		if p.Mode != ProviderModeFake && p.Name == "" {
			add(RuleField, field+".Name", "required unless mode is fake")
		}
		if p.Timeout <= 0 {
			add(RuleField, field+".Timeout", "must be > 0")
		}
		if p.BaseURL != "" {
			if _, err := parseHTTPURL(p.BaseURL); err != nil {
				add(RuleField, field+".BaseURL", err.Error())
			}
		}
	}

	// ---- telemetry ---------------------------------------------------------
	if err := validateSampleRatio(c.Telemetry.TraceSampleRatio); err != nil {
		add(RuleField, "Telemetry.TraceSampleRatio", err.Error())
	}
	if c.Telemetry.MetricsInterval <= 0 {
		add(RuleField, "Telemetry.MetricsInterval", "must be > 0")
	}
	if ep := c.Telemetry.OTLPEndpoint; ep != "" {
		if strings.Contains(ep, "://") {
			add(RuleField, "Telemetry.OTLPEndpoint", "must be host:port without a scheme")
		} else if _, _, err := net.SplitHostPort(ep); err != nil {
			add(RuleField, "Telemetry.OTLPEndpoint", "must be host:port")
		}
	}
	if env == EnvProd && c.Telemetry.OTLPInsecure {
		add(RuleNoInsecureOTLP, "Telemetry.OTLPInsecure", "must be false in PROD")
	}

	// ---- seed --------------------------------------------------------------
	if prodLike && c.Seed.Enabled {
		add(RuleNoSeed, "Seed.Enabled", "must be false in STAGING/PROD")
	}

	// ---- retention ---------------------------------------------------------
	for name, d := range map[string]int{
		"Retention.FinancialRecordDays": c.Retention.FinancialRecordDays,
		"Retention.SecurityAuditDays":   c.Retention.SecurityAuditDays,
		"Retention.RawMarketDataDays":   c.Retention.RawMarketDataDays,
		"Retention.SocialDataDays":      c.Retention.SocialDataDays,
		"Retention.ModelIODays":         c.Retention.ModelIODays,
		"Retention.OperationalLogDays":  c.Retention.OperationalLogDays,
		// The seventh, and the one that was missing. Its own documentation
		// says "Minimum 1"; nothing enforced it, and cmd/audit-worker hard
		// errors on every purge pass with a negative value -- so the plaintext
		// OIDC nonces and PKCE verifiers it exists to delete are never deleted,
		// and the operator sees a failing worker rather than a failing config
		// check (F-103).
		"Retention.LoginAttemptDays":  c.Retention.LoginAttemptDays,
		"Retention.SecurityEventDays": c.Retention.SecurityEventDays,
	} {
		if d < 0 {
			add(RuleField, name, "must not be negative")
		}
	}
	// The login-attempt window has a documented minimum of 1 day and nothing
	// enforced it (F-93). The row holds a plaintext OIDC nonce and PKCE
	// verifier; the durable record of a login is a security_events row, which is
	// why the documented default is 2 and not 2555.
	//
	// Enforcing the variable's own stated minimum rather than inventing a
	// maximum: how long single-use secrets are kept is a deployment decision,
	// and this rule only refuses the value the documentation already refuses.
	if c.Retention.LoginAttemptDays > 0 && c.Retention.LoginAttemptDays < 1 {
		add(RuleField, "Retention.LoginAttemptDays", "must be at least 1 day when purging is enabled; its own documentation says so")
	}

	// Zero disables security-event pruning. Anything positive is a real
	// retention decision about a security audit trail, and the floor is the
	// same 90 days cp_security_events_drop_expired refuses below -- checked
	// here too so the refusal arrives at boot rather than on the first pass,
	// months later, in a log nobody is reading (F-105).
	if c.Retention.SecurityEventDays > 0 && c.Retention.SecurityEventDays < SecurityEventRetentionFloorDays {
		add(RuleField, "Retention.SecurityEventDays",
			fmt.Sprintf("must be 0 (no pruning) or at least %d days; a shorter window on a security audit trail is an erase control with a retention control's name", SecurityEventRetentionFloorDays))
	}
	if prodLike {
		if c.Retention.FinancialRecordDays <= 0 {
			add(RuleRetentionNonZero, "Retention.FinancialRecordDays", "must be > 0 in STAGING/PROD")
		}
		if c.Retention.SecurityAuditDays <= 0 {
			add(RuleRetentionNonZero, "Retention.SecurityAuditDays", "must be > 0 in STAGING/PROD")
		}
	}

	// ---- secret references (structural: every SecretRef field) -------------
	for path, ref := range c.secretRefs() {
		if err := ref.ValidateFor(env); err != nil {
			add(RuleSecretRefScheme, path, err.Error())
		}
	}

	return errors.Join(errs...)
}

func parseHTTPURL(s string) (*url.URL, error) {
	if s == "" {
		return nil, errors.New("must not be empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q", s)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q must be an absolute http(s) URL", s)
	}
	return u, nil
}

// validateSampleRatio checks that s is a decimal in [0, 1] using exact
// rational arithmetic; the config package never parses floats.
func validateSampleRatio(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("must not be empty")
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return fmt.Errorf("%q must be a plain decimal such as 1 or 0.25", s)
		}
	}
	rat, ok := new(big.Rat).SetString(s)
	if !ok {
		return fmt.Errorf("%q is not a decimal", s)
	}
	if rat.Sign() < 0 || rat.Cmp(big.NewRat(1, 1)) > 0 {
		return fmt.Errorf("%q must be between 0 and 1", s)
	}
	return nil
}
