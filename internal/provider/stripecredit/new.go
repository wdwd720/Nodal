package stripecredit

import (
	"context"
	"net/http"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
)

// New builds the credit purchase provider named by cfg
// (config.Providers.CreditPurchase).
//
// Secrets are resolved once, here, and never logged. There is deliberately no
// fake mode: the funding onramp has one because its flow is long and involves
// a chain, and a developer needs to walk it without a provider. A card payment
// is one API call and one webhook, and Stripe's own test mode is a better fake
// than any we would write -- one that stays correct when Stripe changes.
func New(ctx context.Context, cfg config.ProviderConfig, env config.Environment, resolver config.Resolver,
	clk clock.Clock, hc *http.Client, health *provider.Tracker) (credit.PurchaseProvider, error) {

	if cfg.Name != "" && cfg.Name != ProviderName {
		return nil, errs.Newf(errs.CodeValidationFailed, "stripecredit: provider config names %q", cfg.Name)
	}
	if cfg.Mode == config.ProviderModeFake {
		return nil, errs.New(errs.CodeUnsupported,
			"stripecredit: there is no fake mode; use Stripe test mode, which stays correct when Stripe changes")
	}
	if resolver == nil {
		return nil, errs.New(errs.CodeValidationFailed, "stripecredit: a secret resolver is required")
	}
	key, err := resolver.Resolve(ctx, cfg.APIKeyRef)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "stripecredit: resolve api key")
	}
	secret, err := resolver.Resolve(ctx, cfg.WebhookSecretRef)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "stripecredit: resolve webhook secret")
	}
	return NewClient(Options{
		Mode:              cfg.Mode,
		Env:               env,
		BaseURL:           cfg.BaseURL,
		APIKey:            key,
		WebhookSecret:     secret,
		Timeout:           cfg.Timeout,
		HTTPClient:        hc,
		Clock:             clk,
		Health:            health,
		AccountID:         cfg.AccountRef,
		SharedAccount:     cfg.Shared,
		ContractReference: cfg.AccountRef,
	})
}
