package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/pii"
)

// newPIIStore builds the store that encrypts personal data before it reaches
// identity_pii, or returns nil when no keyring is configured -- and an error
// when this deployment is one that may not run without one.
//
// A nil store is a working configuration in LOCAL, TEST and DEV: nothing
// personal is stored, and the login path says so once at WARN rather than
// assuming. Outside them a keyring that is missing, unresolvable or unusable
// is fatal, and that changed here (F-137): `RulePIIKeyring` could only see the
// env:// REFERENCE, which render.yaml always writes, so a STAGING deployment
// whose NODAL_PII_KEYRING had never been set reached the ERROR-and-continue
// branch below and served -- storing no personal data at all, which is the
// state F-47 sat in, under a document that said it could not boot.
//
// The degradation itself is still right where it applies: the alternative to
// encrypted personal data is no personal data, never plaintext. What was wrong
// was applying it to a deployment.
func newPIIStore(ctx context.Context, cfg *config.Config, resolver config.Resolver, log *slog.Logger) (*pii.Store, error) {
	fatal := cfg.Env.IsProductionLike()
	if cfg.PII.Keyring.IsZero() {
		if fatal {
			return nil, fmt.Errorf("CP_PII_KEYRING_REF is not set: %s may not serve with personal data "+
				"unencrypted, which here means not stored at all", cfg.Env)
		}
		log.Warn("personal data is not stored: CP_PII_KEYRING_REF is not set",
			"consequence", "a customer's verified e-mail address is hashed for lookup and otherwise discarded at login",
			"note", "STAGING and PROD refuse to start in this state")
		return nil, nil
	}
	raw, err := resolver.Resolve(ctx, cfg.PII.Keyring)
	if err != nil {
		if fatal {
			return nil, fmt.Errorf("CP_PII_KEYRING_REF (%s) could not be resolved in %s: %w",
				cfg.PII.Keyring.Redacted(), cfg.Env, err)
		}
		log.Error("personal data is not stored: CP_PII_KEYRING_REF could not be resolved",
			"error", err.Error(),
			"consequence", "a customer's verified e-mail address is hashed for lookup and otherwise discarded at login")
		return nil, nil
	}
	kr, err := pii.ParseKeyring(raw)
	if err != nil {
		// The parse error names a version at most, never key material.
		if fatal {
			return nil, fmt.Errorf("CP_PII_KEYRING_REF (%s) is not usable in %s: %w",
				cfg.PII.Keyring.Redacted(), cfg.Env, err)
		}
		log.Error("personal data is not stored: CP_PII_KEYRING_REF is not usable",
			"error", err.Error(),
			"consequence", "a customer's verified e-mail address is hashed for lookup and otherwise discarded at login")
		return nil, nil
	}
	log.Info("personal data is encrypted at the application layer",
		"active_key_version", kr.Active(), "key_versions", kr.Versions())
	return pii.NewStore(kr), nil
}
