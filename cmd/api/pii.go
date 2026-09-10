package main

import (
	"context"
	"log/slog"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/pii"
)

// newPIIStore builds the store that encrypts personal data before it reaches
// identity_pii, or returns nil when no keyring is configured.
//
// A nil store is a working configuration in LOCAL and TEST: nothing personal
// is stored, and the login path says so once at WARN rather than assuming.
// `config.Validate` requires the keyring in STAGING and PROD, so this branch
// is a developer's laptop rather than a deployment. A keyring that is
// configured but cannot be resolved or parsed is logged at ERROR and the
// service serves without one: the alternative to encrypted personal data is
// no personal data, never plaintext, and never a stopped service.
func newPIIStore(ctx context.Context, cfg *config.Config, resolver config.Resolver, log *slog.Logger) *pii.Store {
	if cfg.PII.Keyring.IsZero() {
		log.Warn("personal data is not stored: CP_PII_KEYRING_REF is not set",
			"consequence", "a customer's verified e-mail address is hashed for lookup and otherwise discarded at login",
			"note", "config.Validate requires the keyring in STAGING and PROD")
		return nil
	}
	raw, err := resolver.Resolve(ctx, cfg.PII.Keyring)
	if err != nil {
		log.Error("personal data is not stored: CP_PII_KEYRING_REF could not be resolved",
			"error", err.Error(),
			"consequence", "a customer's verified e-mail address is hashed for lookup and otherwise discarded at login")
		return nil
	}
	kr, err := pii.ParseKeyring(raw)
	if err != nil {
		// The parse error names a version at most, never key material.
		log.Error("personal data is not stored: CP_PII_KEYRING_REF is not usable",
			"error", err.Error(),
			"consequence", "a customer's verified e-mail address is hashed for lookup and otherwise discarded at login")
		return nil
	}
	log.Info("personal data is encrypted at the application layer",
		"active_key_version", kr.Active(), "key_versions", kr.Versions())
	return pii.NewStore(kr)
}
