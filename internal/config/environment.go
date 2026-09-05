package config

import (
	"errors"
	"fmt"
	"strings"
)

// Environment is the explicit deployment environment. It is the single switch
// that decides whether development conveniences (defaults, fake providers,
// plain-text secrets, dev auth) are permitted. Unknown values are rejected.
type Environment string

const (
	// EnvLocal is a developer workstation. Defaults and fakes are allowed.
	EnvLocal Environment = "LOCAL"
	// EnvTest is an automated test run. Defaults and fakes are allowed.
	EnvTest Environment = "TEST"
	// EnvDev is a shared, deployed development environment. No defaults are
	// applied; fakes are allowed; file:// secrets are allowed.
	EnvDev Environment = "DEV"
	// EnvStaging is production-like: every production rule applies.
	EnvStaging Environment = "STAGING"
	// EnvProd is production.
	EnvProd Environment = "PROD"
)

// ErrUnknownEnvironment is returned by ParseEnvironment for any value that is
// not one of the five declared environments.
var ErrUnknownEnvironment = errors.New("config: unknown environment")

// Environments lists every valid environment in escalation order.
func Environments() []Environment {
	return []Environment{EnvLocal, EnvTest, EnvDev, EnvStaging, EnvProd}
}

// ParseEnvironment parses an environment name. Matching is case-insensitive
// after trimming whitespace; anything else fails closed with
// ErrUnknownEnvironment (there is no "default" environment).
func ParseEnvironment(s string) (Environment, error) {
	norm := strings.ToUpper(strings.TrimSpace(s))
	for _, e := range Environments() {
		if string(e) == norm {
			return e, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownEnvironment, s)
}

// String implements fmt.Stringer.
func (e Environment) String() string { return string(e) }

// IsValid reports whether e is one of the declared environments.
func (e Environment) IsValid() bool {
	for _, v := range Environments() {
		if v == e {
			return true
		}
	}
	return false
}

// IsProductionLike reports whether every production rule applies (STAGING and
// PROD).
func (e Environment) IsProductionLike() bool {
	return e == EnvStaging || e == EnvProd
}

// AllowsDefaults reports whether Load may fill absent variables with the
// documented development defaults. Only LOCAL and TEST qualify: a missing
// value in DEV, STAGING or PROD is always an error.
func (e Environment) AllowsDefaults() bool {
	return e == EnvLocal || e == EnvTest
}

// AllowsFakeProviders reports whether ProviderModeFake is permitted. Fakes are
// permitted in LOCAL, TEST and DEV; never in STAGING or PROD.
func (e Environment) AllowsFakeProviders() bool {
	return e == EnvLocal || e == EnvTest || e == EnvDev
}

// AllowsPlainSecrets reports whether a SecretRef may be a plain value rather
// than a reference. Only LOCAL and TEST qualify.
func (e Environment) AllowsPlainSecrets() bool {
	return e == EnvLocal || e == EnvTest
}

// AllowsFileSecrets reports whether file:// SecretRefs are permitted (LOCAL,
// TEST and DEV).
func (e Environment) AllowsFileSecrets() bool {
	return e == EnvLocal || e == EnvTest || e == EnvDev
}

// AllowsDevAuth reports whether AuthModeDev is permitted (LOCAL, TEST, DEV).
func (e Environment) AllowsDevAuth() bool {
	return e == EnvLocal || e == EnvTest || e == EnvDev
}
