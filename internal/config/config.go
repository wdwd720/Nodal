package config

import (
	"fmt"
	"reflect"
	"slices"
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
	Env               Environment
	ServiceName       string
	PublicProductName string
	BuildVersion      string
	HTTP              HTTPConfig
	Database          DatabaseConfig
	Redis             RedisConfig
	Redpanda          RedpandaConfig
	ClickHouse        ClickHouseConfig
	Temporal          TemporalConfig
	Archive           ArchiveConfig
	KMS               KMSConfig
	Auth              AuthConfig
	Providers         ProvidersConfig
	Telemetry         TelemetryConfig
	Seed              SeedConfig
	Capability        CapabilityConfig
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
	AppURL           SecretRef
	MigrateURL       SecretRef
	ReadOnlyURL      SecretRef
	RequireTLS       bool
	MaxConns         int32
	MinConns         int32
	StatementTimeout time.Duration
	LockTimeout      time.Duration
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

// ArchiveConfig configures the S3-compatible evidence/audit archive.
type ArchiveConfig struct {
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
}

// ProvidersConfig holds one ProviderConfig per provider slot.
type ProvidersConfig struct {
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

// SeedConfig controls seeding of clearly-labelled fake data.
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

// RetentionConfig holds retention in days for each retention class (goal
// PART 122). Production cannot set the financial or security-audit classes to
// zero.
type RetentionConfig struct {
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
	case reflect.Ptr:
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
				fn(p, fv.Addr().Interface().(*SecretRef))
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
