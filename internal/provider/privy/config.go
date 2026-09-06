package privy

import (
	"net/http"
	"strings"
	"time"

	"github.com/privy-io/go-sdk/authorization"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/signing/inspect"
)

// Name is the provider name recorded on wallets and decisions.
const Name = "privy"

// CAIP-2 identifiers of the Solana networks (privy.md).
const (
	CAIP2Mainnet = "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp"
	CAIP2Devnet  = "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1"
	CAIP2Testnet = "solana:4uhcVJyU9pJkvQyS88uRDiswHXSCkY3z"
)

// DefaultTimeout bounds every provider call when the ProviderConfig has none.
const DefaultTimeout = 15 * time.Second

// Options is the Privy-specific configuration layered on config.ProviderConfig.
type Options struct {
	// Env decides whether an insecure base URL (httptest) is acceptable.
	Env config.Environment
	// AppID is the Privy application id (sent as privy-app-id; not a secret).
	AppID string
	// AppSecretRef resolves the app secret. Defaults to ProviderConfig.APIKeyRef.
	AppSecretRef config.SecretRef
	// AuthorizationKeyRef resolves the base64 PKCS#8 P-256 private key of the
	// platform signer used for privy-authorization-signature. Either this or
	// Signer must be set.
	AuthorizationKeyRef config.SecretRef
	// Signer is an external signer (e.g. KMS) alternative to AuthorizationKeyRef.
	Signer authorization.AuthorizationSigner
	// OwnerQuorumID is the key quorum set as owner_id on created wallets.
	OwnerQuorumID string
	// SignerQuorumID is the platform signer id expected to be the wallet owner
	// or one of its additional signers. Defaults to OwnerQuorumID.
	SignerQuorumID string
	// PolicyID is the provider policy every wallet must carry.
	PolicyID string
	// AllowedProgramIDs is the exact programId allow-list the policy must
	// enforce for signTransaction. Defaults to inspect.DefaultAllowedPrograms.
	AllowedProgramIDs []string
	// CAIP2 selects the Solana network; defaults to mainnet.
	CAIP2 string
	// Chain is the platform chain name wallets are created on
	// (assets.chain convention, e.g. "solana-mainnet").
	Chain string
	// HTTPClient overrides the transport (tests). nil → a client with the
	// configured timeout.
	HTTPClient *http.Client
}

func (o Options) withDefaults(cfg config.ProviderConfig) Options {
	if o.AppSecretRef.IsZero() {
		o.AppSecretRef = cfg.APIKeyRef
	}
	if o.SignerQuorumID == "" {
		o.SignerQuorumID = o.OwnerQuorumID
	}
	if len(o.AllowedProgramIDs) == 0 {
		o.AllowedProgramIDs = inspect.DefaultAllowedPrograms()
	}
	if o.CAIP2 == "" {
		o.CAIP2 = CAIP2Mainnet
	}
	if o.Chain == "" {
		switch o.CAIP2 {
		case CAIP2Devnet:
			o.Chain = "solana-devnet"
		case CAIP2Testnet:
			o.Chain = "solana-testnet"
		default:
			o.Chain = "solana-mainnet"
		}
	}
	return o
}

func (o Options) validate(cfg config.ProviderConfig) error {
	switch {
	case cfg.Mode == config.ProviderModeFake:
		return errs.New(errs.CodeValidationFailed, "privy: provider mode fake is not a Privy adapter; use wallettest.Fake")
	case !cfg.Mode.IsValid():
		return errs.Newf(errs.CodeValidationFailed, "privy: unknown provider mode %q", cfg.Mode)
	case o.AppID == "":
		return errs.New(errs.CodeValidationFailed, "privy: app id required")
	case o.AppSecretRef.IsZero():
		return errs.New(errs.CodeValidationFailed, "privy: app secret reference required")
	case o.AuthorizationKeyRef.IsZero() && o.Signer == nil:
		return errs.New(errs.CodeValidationFailed, "privy: an authorization key reference or an external signer is required")
	case o.OwnerQuorumID == "":
		return errs.New(errs.CodeValidationFailed, "privy: owner key quorum id required")
	case o.PolicyID == "":
		return errs.New(errs.CodeValidationFailed, "privy: policy id required")
	case !strings.HasPrefix(o.CAIP2, "solana:"):
		return errs.New(errs.CodeValidationFailed, "privy: CAIP2 must be a solana network")
	case !strings.HasPrefix(o.Chain, "solana"):
		return errs.New(errs.CodeValidationFailed, "privy: chain must be a solana chain")
	}
	if err := o.AppSecretRef.ValidateFor(o.Env); err != nil {
		return errs.Wrap(err, errs.CodeValidationFailed, "privy: app secret reference not permitted in this environment")
	}
	if !o.AuthorizationKeyRef.IsZero() {
		if err := o.AuthorizationKeyRef.ValidateFor(o.Env); err != nil {
			return errs.Wrap(err, errs.CodeValidationFailed, "privy: authorization key reference not permitted in this environment")
		}
	}
	if o.Env.IsProductionLike() {
		if cfg.BaseURL != "" && !strings.HasPrefix(cfg.BaseURL, "https://") {
			return errs.New(errs.CodeValidationFailed, "privy: base URL must be https in STAGING/PROD")
		}
		if cfg.Mode != config.ProviderModeLive && cfg.Mode != config.ProviderModeSandbox {
			return errs.New(errs.CodeValidationFailed, "privy: STAGING/PROD require sandbox or live mode")
		}
	}
	return nil
}
