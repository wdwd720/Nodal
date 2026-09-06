package stripe

import (
	"context"
	"net/http"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/provider"
)

// New builds the funding provider named by cfg (config.Providers.Funding):
// a Fake for mode "fake" (LOCAL/TEST/DEV only), otherwise a Client whose
// API key and webhook secret are resolved through resolver. Secrets are
// resolved once here and never logged.
func New(ctx context.Context, cfg config.ProviderConfig, env config.Environment, resolver config.Resolver, clk clock.Clock, hc *http.Client, health *provider.Tracker) (funding.FundingProvider, error) {
	if cfg.Name != "" && cfg.Name != ProviderName {
		return nil, errs.Newf(errs.CodeValidationFailed, "stripe: provider config names %q", cfg.Name)
	}
	o := Options{Mode: cfg.Mode, Env: env, BaseURL: cfg.BaseURL, Timeout: cfg.Timeout, HTTPClient: hc, Clock: clk, Health: health}
	if cfg.Mode == config.ProviderModeFake {
		if cfg.WebhookSecretRef != "" && resolver != nil {
			secret, err := resolver.Resolve(ctx, cfg.WebhookSecretRef)
			if err != nil {
				return nil, errs.Wrap(err, errs.CodeInternal, "stripe: resolve webhook secret")
			}
			o.WebhookSecret = secret
		}
		return NewFake(o)
	}
	if resolver == nil {
		return nil, errs.New(errs.CodeValidationFailed, "stripe: a secret resolver is required")
	}
	key, err := resolver.Resolve(ctx, cfg.APIKeyRef)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "stripe: resolve api key")
	}
	secret, err := resolver.Resolve(ctx, cfg.WebhookSecretRef)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "stripe: resolve webhook secret")
	}
	o.APIKey, o.WebhookSecret = key, secret
	return NewClient(o)
}
