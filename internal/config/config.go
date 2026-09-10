package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// BuildVersion is the service build identifier. It is set at link time via
// -ldflags "-X github.com/nodal/controlplane/internal/config.BuildVersion=<sha>"
// and copied into Config.BuildVersion by Load. It is included in Hash so that
// audit records can name the exact build.
var BuildVersion = "dev"

// ProviderMode selects the adapter implementation for an external provider.
type ProviderMode string

const (
	// ProviderModeFake is an in-process test double. Rejected in STAGING/PROD.
	ProviderModeFake ProviderMode = "fake"
	// ProviderModeSandbox talks to the provider's sandbox/testnet.
	ProviderModeSandbox ProviderMode = "sandbox"
	// ProviderModeLive talks to the real provider with real money.
	ProviderModeLive ProviderMode = "live"
)

// ParseProviderMode parses a provider mode, failing closed on unknown values.
func ParseProviderMode(s string) (ProviderMode, error) {
	switch ProviderMode(s) {
	case ProviderModeFake, ProviderModeSandbox, ProviderModeLive:
		return ProviderMode(s), nil
	}
	return "", fmt.Errorf("config: unknown provider mode %q (want fake|sandbox|live)", s)
}

// IsValid reports whether m is a declared mode.
func (m ProviderMode) IsValid() bool {
	_, err := ParseProviderMode(string(m))
	return err == nil
}

// Auth modes.
const (
	// AuthModeOIDC authenticates against an external OpenID Connect issuer.
	AuthModeOIDC = "oidc"
	// AuthModeDev uses the in-repo development identity provider
	// (internal/auth/devidp). Rejected in STAGING/PROD.
	AuthModeDev = "dev"
)

// Config is the fully typed, validated service configuration. It is built by
// Load and must be treated as immutable afterwards. There are no
// map[string]string escape hatches: every setting has a typed field.
type Config struct {
	// Service is the binary this configuration was loaded for. It decides
	// which external dependencies are required: see Service.Requires.
	Service           Service
	Env               Environment
	ServiceName       string
	PublicProductName string
	BuildVersion      string
	HTTP              HTTPConfig
	Database          DatabaseConfig
	API               APIConfig
	Capacity          CapacityConfig
	RateLimit         RateLimitConfig
	Redis             RedisConfig
	Redpanda          RedpandaConfig
	ClickHouse        ClickHouseConfig
	Temporal          TemporalConfig
	Archive           ArchiveConfig
	KMS               KMSConfig
	Auth              AuthConfig
	Providers         ProvidersConfig
	Telemetry         TelemetryConfig
	Alert             AlertConfig
	PII               PIIConfig
	Seed              SeedConfig
	Capability        CapabilityConfig
	Credit            CreditConfig
	Retention         RetentionConfig
}

// HTTPConfig configures the public HTTP listener.
type HTTPConfig struct {
	Addr              string
	PublicBaseURL     string
	CORSOrigins       []string
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxBodyBytes      int64
	TrustedProxyCIDRs []string
}

// DatabaseConfig configures Postgres. The URLs embed credentials and are
// therefore SecretRefs (plain only in LOCAL/TEST).
type DatabaseConfig struct {
	AppURL     SecretRef
	MigrateURL SecretRef
	// ConnectTimeout bounds one dial; MaxConnIdleTime discards a pooled
	// connection before the database it points at suspends underneath it. Both
	// exist because Neon's free tier scales to zero (F-93).
	ConnectTimeout  time.Duration
	MaxConnIdleTime time.Duration
	ReadOnlyURL     SecretRef
	// OpsURL is the cp_ops role. It exists for the deletions the application
	// role is deliberately refused: cp_app may write a login attempt and never
	// remove one, so an attacker holding it cannot erase the record of the
	// logins they tried. Optional, and a retention pass that needs it says so
	// rather than running on the app pool.
	OpsURL           SecretRef
	RequireTLS       bool
	MaxConns         int32
	MinConns         int32
	StatementTimeout time.Duration
	LockTimeout      time.Duration
}

// RateLimitBackend selects where transport rate-limit counters live.
type RateLimitBackend string

// Rate-limit backends.
const (
	// RateLimitMemory keeps counters in the process. The budget is therefore
	// PER REPLICA: three API tasks with a limit of 100 admit 300. That is
	// acceptable on one machine and is not a rate limit in a deployment that
	// scales horizontally, which is why STAGING and PROD refuse it.
	RateLimitMemory RateLimitBackend = "memory"
	// RateLimitRedis keeps counters in a shared Redis, so every replica spends
	// from one budget.
	RateLimitRedis RateLimitBackend = "redis"
)

// ParseRateLimitBackend parses a backend name, failing closed.
func ParseRateLimitBackend(s string) (RateLimitBackend, error) {
	switch RateLimitBackend(s) {
	case RateLimitMemory, RateLimitRedis:
		return RateLimitBackend(s), nil
	}
	return "", fmt.Errorf("config: unknown rate limit backend %q (want memory|redis)", s)
}

// IsValid reports whether b is a declared backend.
func (b RateLimitBackend) IsValid() bool {
	_, err := ParseRateLimitBackend(string(b))
	return err == nil
}

// Distributed reports whether the backend is shared across replicas.
func (b RateLimitBackend) Distributed() bool { return b == RateLimitRedis }

// EnforcesOneBudget reports whether this configuration enforces the limit it
// states. True for a distributed backend at any replica count, and for a
// process-local backend at exactly one replica -- which is the same thing said
// two ways: one set of counters.
func (c RateLimitConfig) EnforcesOneBudget() bool {
	return c.Backend.Distributed() || c.Replicas == 1
}

// APIConfig is what cmd/api needs and no other binary does.
//
// These two were read straight from the environment in cmd/api/wire.go and
// appeared in no requirements table at all -- so .env.example did not document
// them, scripts/configcheck could not see them, and a deployment could pass
// every configuration check and then refuse to start. That is the exact failure
// internal/config exists to prevent, and it was reached by the one route the
// table does not cover: a variable that never joined it.
type APIConfig struct {
	// SettlementChain and SettlementMint name the USD-pegged asset that funds
	// settle into. Both are required in STAGING and PROD, and cmd/api
	// additionally checks that the pair resolves to a known stablecoin -- which
	// stays in wire.go, because it needs the database and this does not.
	SettlementChain string
	SettlementMint  string

	// EnabledCapabilities is the comma-separated list of capability names this
	// deployment is permitted to run at all.
	//
	// It is condition 1 of the policy authority: a capability absent from it
	// is inactive without any gate row being consulted, so this list is a
	// control in its own right and an empty one disables everything. It was
	// read straight from the environment, which left the single most
	// consequential list in the deployment outside the configuration hash --
	// it could be widened in a platform dashboard and /v1/version would report
	// the same hash as before.
	//
	// Enabling a capability here does not activate it. The gate row still has
	// to be proposed, approved and activated by three distinct principals.
	EnabledCapabilities string

	// FundingNetwork and FundingCurrency describe the onramp the funding
	// endpoints quote in.
	FundingNetwork  string
	FundingCurrency string

	// RequestTimeout is the per-request deadline for non-streaming routes.
	// Zero means HTTP.WriteTimeout, which is what cmd/api defaulted to.
	RequestTimeout time.Duration

	// ShutdownTimeout bounds draining in-flight requests.
	ShutdownTimeout time.Duration

	// LegalPolicy selects the jurisdiction routing policy. A development
	// policy in production is refused by the router itself; naming it here is
	// what lets a configuration check see which one a deployment asked for.
	LegalPolicy string
}

// CapacityConfig is the deployment tier's hard ceilings on financial activity.
//
// They are configuration rather than constants because the same binary runs on
// a free tier sized for fifty people and on infrastructure sized for rather
// more, and the difference between those is a number rather than a code path.
// They are stated explicitly outside LOCAL and TEST for the same reason a
// pricing policy is: a ceiling nobody chose is a ceiling nobody owns.
//
// Zero disables one ceiling. That is meaningful on paid infrastructure, where
// MaxDatabaseBytes would guard a quota that does not exist -- but
// internal/capacity refuses a budget in which EVERY ceiling is zero, because
// such a guard passes every check while reading as protection.
type CapacityConfig struct {
	MaxAccounts        int64
	MaxPurchasesPerDay int64
	MaxAtRiskMinor     int64
	MaxDatabaseBytes   int64
}

// RateLimitConfig configures the transport rate limiter.
type RateLimitConfig struct {
	Backend RateLimitBackend

	// General, Auth, Quote and Command are the four transport budgets, each
	// written as "<requests>/<window>" or "off".
	//
	// They were read straight from the environment in cmd/api and appeared in
	// no requirements table -- the same gap the settlement asset was in, with
	// the same three consequences. .env.example did not document them,
	// scripts/configcheck could not see a typo that would stop the binary
	// starting, and, worst of the three, they were absent from the
	// configuration hash: a deployment could have its transport budget
	// loosened in a platform dashboard and still report the same hash that is
	// supposed to prove which configuration is running.
	General string
	Auth    string
	Quote   string
	Command string

	// Replicas is how many processes of this binary serve HTTP.
	//
	// It is here because it is the fact the rate-limit invariant actually
	// depends on. A rate limit is a budget, and process-local counters give
	// each process its own copy of it -- so "the configured limit is the
	// enforced limit" holds if and only if there is exactly one process. The
	// environment was only ever a proxy for that, and a proxy is wrong in both
	// directions: it refused a correct single-process production deployment and
	// permitted an incorrect three-process DEV one.
	//
	// A deployment that lies here gets a limit looser than it configured, which
	// is why it is stated rather than guessed, logged at startup, and pinned by
	// the platform configuration that decides the real count.
	Replicas int
}

// RedisConfig configures Redis (never financial truth).
type RedisConfig struct {
	URL        SecretRef
	RequireTLS bool
}

// RedpandaConfig configures the Kafka-compatible event bus.
type RedpandaConfig struct {
	Brokers         []string
	RequireTLS      bool
	SASLMechanism   string
	SASLUsernameRef SecretRef
	SASLPasswordRef SecretRef
}

// ClickHouseConfig configures the analytics store.
type ClickHouseConfig struct {
	Addr        string
	Database    string
	UsernameRef SecretRef
	PasswordRef SecretRef
	RequireTLS  bool
}

// TemporalConfig configures the workflow engine.
type TemporalConfig struct {
	HostPort        string
	Namespace       string
	TaskQueuePrefix string
	RequireTLS      bool
}

// ArchiveBackend is where evidence objects are stored.
//
// It exists because "an S3-compatible object store" is an infrastructure
// assumption rather than a requirement of the code. What the code needs is a
// write-once keyed store, and on a tier with no object storage -- the free
// tiers all want a card for it -- an append-only Postgres table denied UPDATE,
// DELETE and TRUNCATE at the privilege level provides the same guarantee by a
// different mechanism.
//
// The one thing it does NOT provide is S3 Object Lock, which is a compliance
// control rather than a convenience and is the reason the audit archive wants
// s3 specifically. That distinction is enforced in Validate rather than left to
// whoever reads this.
type ArchiveBackend string

// Archive backends.
const (
	// ArchiveS3 is an S3-compatible object store: AWS S3 in a deployment,
	// MinIO locally.
	ArchiveS3 ArchiveBackend = "s3"
	// ArchivePostgres keeps objects in the application database, write-once by
	// privilege and by trigger. It needs no second provider and no card.
	ArchivePostgres ArchiveBackend = "postgres"
)

// ParseArchiveBackend parses the canonical name.
func ParseArchiveBackend(v string) (ArchiveBackend, error) {
	b := ArchiveBackend(strings.ToLower(strings.TrimSpace(v)))
	if !b.IsValid() {
		return "", fmt.Errorf("config: unknown archive backend %q (want s3 or postgres)", v)
	}
	return b, nil
}

// IsValid reports whether b is a declared backend.
func (b ArchiveBackend) IsValid() bool { return b == ArchiveS3 || b == ArchivePostgres }

// NeedsObjectStore reports whether this backend requires the S3 endpoint,
// region, buckets and credentials to be configured at all.
//
// Only an explicit "postgres" relaxes the requirement. An unset or unrecognised
// backend needs the object store, which is the fail-closed direction: a
// deployment that forgot to say where its evidence goes is asked for everything
// rather than excused from all of it, and the missing variable is reported on
// its own account.
func (b ArchiveBackend) NeedsObjectStore() bool { return b != ArchivePostgres }

// ArchiveConfig configures the evidence and audit archive. Which of these
// values are required depends on Backend.
type ArchiveConfig struct {
	Backend            ArchiveBackend
	Endpoint           string
	Region             string
	RawBucket          string
	EvidenceBucket     string
	AuditBucket        string
	ObjectLockRequired bool
	ForcePathStyle     bool
	AccessKeyRef       SecretRef
	SecretKeyRef       SecretRef
}

// KMSConfig configures the key used to sign audit records.
type KMSConfig struct {
	AuditSigningKeyID string
	Region            string
}

// AuthConfig configures authentication and sessions.
type AuthConfig struct {
	Mode             string
	Issuer           string
	ClientID         string
	ClientSecretRef  SecretRef
	RedirectURL      string
	CookieName       string
	CookieDomain     string
	CookieSecure     bool
	SessionTTL       time.Duration
	StepUpMaxAge     time.Duration
	DebugAuthEnabled bool
}

// ProviderConfig configures one external provider adapter.
type ProviderConfig struct {
	Mode             ProviderMode
	Name             string
	BaseURL          string
	APIKeyRef        SecretRef
	WebhookSecretRef SecretRef
	Timeout          time.Duration

	// AccountRef identifies the provider-side account or tenant this adapter
	// is configured for. An adapter that knows it can assert, at startup, that
	// the credentials it was given belong to the account it expected -- which
	// is how a key rotated to the wrong account is caught before it takes a
	// payment rather than after.
	AccountRef string

	// Shared declares that the provider-side account also serves systems
	// outside this deployment.
	//
	// It is configuration rather than a comment because it changes what the
	// code must do. On a shared account the webhook endpoint receives events
	// belonging to somebody else, and an adapter that assumes every delivery
	// is its own will act on another product's payment.
	Shared bool

	// Availability is how far this integration is actually approved for use
	// on this account, as opposed to how finished the code is.
	//
	// It is carried here as an opaque string that this package does not
	// interpret: the vocabulary belongs to the adapter that reads it, and
	// duplicating an enum across this leaf boundary would create two lists
	// that can disagree. An empty value means nothing has been granted, which
	// every adapter must treat as a refusal.
	Availability string

	// DescriptorPrefix and DescriptorSuffix are what a customer sees on their
	// statement for this product.
	//
	// They are here rather than left to each adapter because the failure they
	// prevent is the same everywhere and stays invisible until it is
	// expensive: a customer who does not recognise a charge disputes it, and
	// on a shared provider account the default descriptor is the OTHER
	// product's name.
	DescriptorPrefix string
	DescriptorSuffix string
}

// ProvidersConfig holds one ProviderConfig per provider slot.
type ProvidersConfig struct {
	// CreditPurchase sells Nodal Credits for fiat. It is a separate slot from
	// Funding, which is the crypto onramp: one takes a card payment and issues
	// internal Credits, the other converts a customer's own money into crypto
	// in a wallet Nodal never holds. Sharing a slot would mean one set of
	// credentials and one mode for two products with different risk.
	CreditPurchase ProviderConfig
	// Payout sends eligible value out of the system.
	Payout                ProviderConfig
	Funding               ProviderConfig
	Wallet                ProviderConfig
	Signing               ProviderConfig
	Execution             ProviderConfig
	MarketData            ProviderConfig
	ChainObserver         ProviderConfig
	ChainObserverFallback ProviderConfig
	Model                 ProviderConfig
	EventBus              ProviderConfig
	Workflow              ProviderConfig
	Archive               ProviderConfig
	Notification          ProviderConfig
}

// providerSlot names a provider field for validation and env-var mapping.
type providerSlot struct {
	Name string // human/rule name, e.g. "funding"
	Env  string // env var infix, e.g. "FUNDING"
	Get  func(*ProvidersConfig) *ProviderConfig
}

func providerSlots() []providerSlot {
	return []providerSlot{
		{"credit_purchase", "CREDIT_PURCHASE", func(p *ProvidersConfig) *ProviderConfig { return &p.CreditPurchase }},
		{"payout", "PAYOUT", func(p *ProvidersConfig) *ProviderConfig { return &p.Payout }},
		{"funding", "FUNDING", func(p *ProvidersConfig) *ProviderConfig { return &p.Funding }},
		{"wallet", "WALLET", func(p *ProvidersConfig) *ProviderConfig { return &p.Wallet }},
		{"signing", "SIGNING", func(p *ProvidersConfig) *ProviderConfig { return &p.Signing }},
		{"execution", "EXECUTION", func(p *ProvidersConfig) *ProviderConfig { return &p.Execution }},
		{"market_data", "MARKET_DATA", func(p *ProvidersConfig) *ProviderConfig { return &p.MarketData }},
		{"chain_observer", "CHAIN_OBSERVER", func(p *ProvidersConfig) *ProviderConfig { return &p.ChainObserver }},
		{"chain_observer_fallback", "CHAIN_OBSERVER_FALLBACK", func(p *ProvidersConfig) *ProviderConfig { return &p.ChainObserverFallback }},
		{"model", "MODEL", func(p *ProvidersConfig) *ProviderConfig { return &p.Model }},
		{"event_bus", "EVENT_BUS", func(p *ProvidersConfig) *ProviderConfig { return &p.EventBus }},
		{"workflow", "WORKFLOW", func(p *ProvidersConfig) *ProviderConfig { return &p.Workflow }},
		{"archive", "ARCHIVE", func(p *ProvidersConfig) *ProviderConfig { return &p.Archive }},
		{"notification", "NOTIFICATION", func(p *ProvidersConfig) *ProviderConfig { return &p.Notification }},
	}
}

// TelemetryConfig configures OpenTelemetry export.
//
// TraceSampleRatio is kept as a decimal string ("1", "0.25") and is converted
// to a float only inside the trace sampler. This is telemetry, not finance:
// the "no floating point" rule protects money, and a sampling probability is
// not money. Keeping the string here means the config package itself never
// touches float64.
type TelemetryConfig struct {
	OTLPEndpoint     string
	OTLPInsecure     bool
	TraceSampleRatio string
	MetricsInterval  time.Duration
}

// PIIConfig is the key material for personal data at rest. internal/pii
// seals identity_pii's columns under it; the database never sees the key.
type PIIConfig struct {
	// Keyring is a SecretRef to the JSON keyring document internal/pii
	// parses: {"active": N, "keys": {"N": "<base64 32 bytes>", ...}}.
	Keyring SecretRef
}

// AlertConfig is where operational alerts go when they leave the process.
//
// F-118: nothing in this deployment pages, and the reason that is SOFTWARE was
// that the raise path's callback had no production caller. The reason that is a
// DEPLOYMENT DECISION is this: somebody has to say where an alert should be
// sent.
//
// A webhook URL is the shape that satisfies "$0 fixed cost" and "commits to no
// vendor" at once -- Slack, Discord, ntfy, healthchecks.io and a three-line
// Worker all accept the same POST. The constraint chose the design.
type AlertConfig struct {
	// WebhookFormat is the payload shape: auto, generic, slack, discord or
	// ntfy. Auto derives it from the URL's host.
	WebhookFormat string

	// WebhookURL is the destination. Empty means alerts are logged and go
	// nowhere, which is said out loud at startup rather than assumed.
	//
	// A SecretRef because a Slack or Discord webhook URL IS its credential:
	// anyone holding it can post to the channel.
	WebhookURL SecretRef
	// MinSeverity is the lowest severity worth sending. SEV2 sends everything.
	MinSeverity string
	// Timeout bounds one delivery attempt.
	Timeout time.Duration
}

// SeedConfig controls seeding of clearly-labeled fake data.
type SeedConfig struct {
	Enabled bool
}

// CapabilityConfig describes the capability store. StoreConfigured is derived
// by Load (true when the application database is configured) and is checked
// by the production rules: capabilities are gated in Postgres, never in
// memory.
type CapabilityConfig struct {
	StoreConfigured bool
}

// CreditConfig holds the Credit funding lifecycle's one risk decision.
type CreditConfig struct {
	// SettlementWindow is how long a captured card payment stays reversible
	// before its Credits may be treated as settled.
	//
	// It is in the table rather than read from the environment because it is a
	// risk determination somebody has to make and record, and a value read
	// straight from the environment is outside scripts/configcheck and outside
	// the configuration hash -- so it could be changed in a dashboard while
	// /v1/version reported the hash that exists to detect exactly that.
	//
	// The default is a CONSERVATIVE placeholder, not a determination: card
	// scheme chargeback windows run to 120 days and beyond.
	SettlementWindow time.Duration
}

// RetentionConfig holds retention in days for each retention class (goal
// PART 122). Production cannot set the financial or security-audit classes to
// zero.
type RetentionConfig struct {
	// LoginAttemptDays bounds how long login_attempts keeps a plaintext OIDC
	// nonce and PKCE verifier after the attempt expired. Short by design: the
	// secrets are single-use and the durable record of a login is a
	// security_events row.
	LoginAttemptDays int
	// SecurityEventDays is how long a monthly partition of security_events is
	// kept before it is detached and dropped (00740, ADR-0020 decision 2).
	//
	// Zero means the trail is never pruned, and that is the default on purpose:
	// the period is the question ADR-0020 leaves open, and dropping a security
	// audit trail because nobody chose a number is worse than a table that
	// grows. What a growing table costs is a capacity refusal, which is
	// fail-closed and visible; what a wrong number costs is evidence.
	//
	// When it is set it must be at least 90 days, the same floor
	// cp_security_events_drop_expired enforces in the database. The floor is in
	// both places because the SQL function is the one an attacker would call.
	SecurityEventDays   int
	FinancialRecordDays int
	SecurityAuditDays   int
	RawMarketDataDays   int
	SocialDataDays      int
	ModelIODays         int
	OperationalLogDays  int
}

// Clone returns a deep copy (slices are copied).
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	cp := *c
	cp.HTTP.CORSOrigins = slices.Clone(c.HTTP.CORSOrigins)
	cp.HTTP.TrustedProxyCIDRs = slices.Clone(c.HTTP.TrustedProxyCIDRs)
	cp.Redpanda.Brokers = slices.Clone(c.Redpanda.Brokers)
	return &cp
}

var secretRefType = reflect.TypeOf(SecretRef(""))

// forEachSecretRef visits every SecretRef field reachable from v (which must
// be addressable), reporting its dotted path. This is what makes "secrets
// never reach the hash" a structural guarantee rather than a per-field
// convention: adding a SecretRef field anywhere is enough.
func forEachSecretRef(v reflect.Value, path string, fn func(path string, ref *SecretRef)) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			forEachSecretRef(v.Elem(), path, fn)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			p := f.Name
			if path != "" {
				p = path + "." + f.Name
			}
			fv := v.Field(i)
			if fv.Type() == secretRefType {
				ref, _ := fv.Addr().Interface().(*SecretRef) // exact: the type was compared above
				fn(p, ref)
				continue
			}
			forEachSecretRef(fv, p, fn)
		}
	}
}

// secretRefs returns every SecretRef in c keyed by dotted field path.
func (c *Config) secretRefs() map[string]*SecretRef {
	out := map[string]*SecretRef{}
	forEachSecretRef(reflect.ValueOf(c), "", func(path string, ref *SecretRef) { out[path] = ref })
	return out
}

// Legal policy names.
//
// The vocabulary lives here rather than in the composition root because both
// have to agree: internal/config decides whether a deployment is valid, and
// cmd/api decides whether it can build a router from the same string. When the
// two were separate lists, config had no list at all and a name it did not
// recognise -- or a development policy in STAGING -- passed every check and
// then refused to boot (F-103).
const (
	// LegalPolicyConservative asks the jurisdiction questions. It is the
	// default, and the empty string means it.
	LegalPolicyConservative = "CONSERVATIVE"
	// LegalPolicyDevelopment permits the internal economy without them, and is
	// refused outside LOCAL/TEST/DEV.
	LegalPolicyDevelopment = "DEVELOPMENT"
)

// NormalizeLegalPolicy upper-cases and trims a configured policy name and
// reports whether it is one this binary knows. The empty string is
// CONSERVATIVE: naming nothing asks for the careful one.
func NormalizeLegalPolicy(s string) (string, bool) {
	switch name := strings.ToUpper(strings.TrimSpace(s)); name {
	case "":
		return LegalPolicyConservative, true
	case LegalPolicyConservative, LegalPolicyDevelopment:
		return name, true
	default:
		return name, false
	}
}
