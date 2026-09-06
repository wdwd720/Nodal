package jupiter

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"

	"github.com/nodal/controlplane/internal/config"
)

// Provider identity and documented constants.
const (
	// ProviderName is the provider slot name used for health tracking,
	// evidence and security events.
	ProviderName = "jupiter"

	// DefaultBaseURL is the documented Swap API V2 base URL
	// (docs/api/providers/jupiter.md, "Versioning and status").
	DefaultBaseURL = "https://api.jup.ag/swap/v2"

	// DefaultProgramID is the Jupiter aggregator v6 program id
	// (JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4, from the official
	// jup-ag/instruction-parser repository). Configurable so the inspector's
	// allowlist and this package always agree.
	DefaultProgramID = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"

	// LiveVerificationBlocker is the external blocker id recorded for the
	// missing live API key (goal PART 208 honesty label).
	LiveVerificationBlocker = "EB-011"

	// MaxTransactionBytes is the Solana transaction size limit (1232 bytes,
	// documented for tx.jup.ag and inherent to the network).
	MaxTransactionBytes = 1232
)

// Wire headers documented for the Developer Platform gateway.
const (
	HeaderAPIKey             = "x-api-key"
	HeaderGatewayRequestID   = "x-api-gateway-request-id"
	HeaderRateLimitRemaining = "x-ratelimit-remaining"
	HeaderRateLimitCurrent   = "x-ratelimit-current"
	HeaderRateLimitReset     = "x-ratelimit-reset"
	HeaderRetryAfter         = "Retry-After"
)

// Default tunables. Every value is overridable through Config.
const (
	DefaultOrderTimeout      = 10 * time.Second
	DefaultBuildTimeout      = 10 * time.Second
	DefaultExecuteTimeout    = 45 * time.Second
	DefaultMaxRetries        = 3
	DefaultRetryBaseDelay    = 200 * time.Millisecond
	DefaultRetryMaxDelay     = 2 * time.Second
	DefaultMaxRetryAfterWait = 5 * time.Second
	// DefaultAssumedOrderTTL is used for ExpiresAt when the response carries
	// no expireAt (aggregator routes). ASSUMED: V2 documents no wall-clock
	// validity for aggregator orders; the hard expiry is lastValidBlockHeight
	// (roughly 60-90 seconds, see docs/api/providers/solana-rpc.md). 30s is
	// deliberately shorter than that window.
	DefaultAssumedOrderTTL  = 30 * time.Second
	DefaultMaxResponseBytes = 4 << 20
)

// Config configures a Client or a Fake.
type Config struct {
	// Env is the deployment environment. Fake mode is rejected outside
	// LOCAL/TEST/DEV (config.Environment.AllowsFakeProviders).
	Env config.Environment
	// Mode is fake or live. Jupiter has no sandbox ("mainnet only"), so
	// sandbox is rejected.
	Mode config.ProviderMode
	// BaseURL is the API base (DefaultBaseURL). Must be https in
	// STAGING/PROD. No path beyond the version prefix.
	BaseURL string
	// APIKeyRef resolves to the x-api-key value. Empty means keyless access,
	// which the documentation restricts to testing; it is rejected in
	// STAGING/PROD.
	APIKeyRef config.SecretRef

	// Per-call timeouts (each HTTP attempt).
	OrderTimeout   time.Duration
	BuildTimeout   time.Duration
	ExecuteTimeout time.Duration

	// Retry policy for SAFE_RETRY operations (Order, Build). Execute never
	// retries regardless of these values.
	MaxRetries        int
	RetryBaseDelay    time.Duration
	RetryMaxDelay     time.Duration
	MaxRetryAfterWait time.Duration

	// AssumedOrderTTL bounds ExpiresAt when the response has no expireAt.
	AssumedOrderTTL time.Duration
	// ProgramID is the Jupiter aggregator program id (base58).
	ProgramID string
	// MaxResponseBytes caps response bodies read into memory.
	MaxResponseBytes int64
}

// DefaultConfig returns a Config with documented defaults for env in live
// mode with no API key. Callers set Mode, APIKeyRef and overrides.
func DefaultConfig(env config.Environment) Config {
	return Config{
		Env:               env,
		Mode:              config.ProviderModeLive,
		BaseURL:           DefaultBaseURL,
		OrderTimeout:      DefaultOrderTimeout,
		BuildTimeout:      DefaultBuildTimeout,
		ExecuteTimeout:    DefaultExecuteTimeout,
		MaxRetries:        DefaultMaxRetries,
		RetryBaseDelay:    DefaultRetryBaseDelay,
		RetryMaxDelay:     DefaultRetryMaxDelay,
		MaxRetryAfterWait: DefaultMaxRetryAfterWait,
		AssumedOrderTTL:   DefaultAssumedOrderTTL,
		ProgramID:         DefaultProgramID,
		MaxResponseBytes:  DefaultMaxResponseBytes,
	}
}

// FromProviderConfig derives a Config from the execution provider slot of
// the service configuration: mode, base URL (default when empty), API key
// reference and, when set, a single timeout applied to every call.
func FromProviderConfig(env config.Environment, pc config.ProviderConfig) Config {
	c := DefaultConfig(env)
	c.Mode = pc.Mode
	if strings.TrimSpace(pc.BaseURL) != "" {
		c.BaseURL = strings.TrimSpace(pc.BaseURL)
	}
	c.APIKeyRef = pc.APIKeyRef
	if pc.Timeout > 0 {
		c.OrderTimeout = pc.Timeout
		c.BuildTimeout = pc.Timeout
		c.ExecuteTimeout = pc.Timeout
	}
	return c
}

// Validate checks the configuration. It fails closed: an unknown mode, a
// fake outside LOCAL/TEST/DEV, a sandbox mode (Jupiter has none), a
// non-https base URL in STAGING/PROD, a missing key in STAGING/PROD, an
// invalid program id or a non-positive timeout are all errors.
func (c Config) Validate() error {
	var problems []error
	add := func(format string, a ...any) { problems = append(problems, fmt.Errorf(format, a...)) }

	if !c.Env.IsValid() {
		add("jupiter: unknown environment %q", string(c.Env))
	}
	switch c.Mode {
	case config.ProviderModeFake:
		if !c.Env.AllowsFakeProviders() {
			add("jupiter: fake mode is not allowed in %s", c.Env)
		}
	case config.ProviderModeLive:
		problems = append(problems, c.validateLive()...)
	case config.ProviderModeSandbox:
		add("jupiter: sandbox mode is not available (Jupiter is mainnet only)")
	default:
		add("jupiter: unknown provider mode %q", string(c.Mode))
	}
	if c.AssumedOrderTTL <= 0 {
		add("jupiter: AssumedOrderTTL must be positive")
	}
	if _, err := solana.PublicKeyFromBase58(strings.TrimSpace(c.ProgramID)); err != nil {
		add("jupiter: ProgramID is not a valid base58 public key")
	}
	return errors.Join(problems...)
}

func (c Config) validateLive() []error {
	var problems []error
	add := func(format string, a ...any) { problems = append(problems, fmt.Errorf(format, a...)) }

	u, err := url.Parse(strings.TrimSpace(c.BaseURL))
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		add("jupiter: base URL must be an absolute http(s) URL")
	case u.Scheme != "https" && c.Env.IsProductionLike():
		add("jupiter: base URL must be https in %s", c.Env)
	case u.RawQuery != "" || u.Fragment != "":
		add("jupiter: base URL must not carry a query or fragment")
	}
	if err := c.APIKeyRef.ValidateFor(c.Env); err != nil {
		add("jupiter: api key ref: %w", err)
	}
	if c.APIKeyRef.IsZero() && c.Env.IsProductionLike() {
		add("jupiter: api key is required in %s (keyless access is for testing only)", c.Env)
	}
	if c.OrderTimeout <= 0 {
		add("jupiter: OrderTimeout must be positive")
	}
	if c.BuildTimeout <= 0 {
		add("jupiter: BuildTimeout must be positive")
	}
	if c.ExecuteTimeout <= 0 {
		add("jupiter: ExecuteTimeout must be positive")
	}
	if c.MaxRetries < 0 || c.MaxRetries > 10 {
		add("jupiter: MaxRetries must be within 0..10")
	}
	if c.RetryBaseDelay < 0 || c.RetryMaxDelay < 0 || c.MaxRetryAfterWait < 0 {
		add("jupiter: retry delays must not be negative")
	}
	if c.RetryMaxDelay > 0 && c.RetryBaseDelay > c.RetryMaxDelay {
		add("jupiter: RetryBaseDelay must not exceed RetryMaxDelay")
	}
	if c.MaxResponseBytes <= 0 {
		add("jupiter: MaxResponseBytes must be positive")
	}
	return problems
}

// programID returns the parsed program id; Validate guarantees it parses.
func (c Config) programID() solana.PublicKey {
	pk, err := solana.PublicKeyFromBase58(strings.TrimSpace(c.ProgramID))
	if err != nil {
		return solana.PublicKey{}
	}
	return pk
}
