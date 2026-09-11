package config

import (
	"fmt"
	"os"
	"strings"
)

// EnvVarSeedEnabled is the variable that decides whether the developer seed
// scripts may write their clearly-labelled fake identities, assets and
// balances.
const EnvVarSeedEnabled = "CP_SEED_ENABLED"

// SeedScriptsAllowed reports whether scripts/seed and scripts/seedeconomy may
// run, given CP_SEED_ENABLED.
//
// scripts/demodata is deliberately NOT one of them. It loads the SANDBOX demo
// catalogue -- the same one cmd/api loads at boot under CP_API_DEMO_DATA -- and
// a sandbox tier is allowed to hold that data by ADR-0023, where RuleNoSeed
// forbids CP_SEED_ENABLED outright. Gating it here would make the command
// unrunnable on the one deployment it exists to serve.
//
// # Why this function exists
//
// Because the variable did not do anything. CP_SEED_ENABLED was required of
// every binary, documented as "Allow seeding clearly-labeled fake
// users/assets/balances", validated by RuleNoSeed, set to "false" in
// render.yaml -- and read by nothing outside this package. The only thing in
// the system that seeded anything on a deployed tier keyed off the sandbox-tier
// declaration instead, so the deployment declared that it does not seed and
// seeded, and an operator who switched this variable off changed nothing at
// all (F-144).
//
// D-086 gave the sandbox demo catalogue its own control (CP_API_DEMO_DATA,
// refused outside a sandbox tier). This gives this variable back the one it
// always claimed: the developer seed scripts.
//
// # What the answers are
//
//   - Outside LOCAL, DEV and TEST: no, whatever the variable says. It is the
//     same answer RuleNoSeed gives, and the same one each script's own
//     environment guard gives; three refusals rather than one is deliberate.
//   - Absent or blank, inside them: yes. `make seed` has never needed a
//     variable set and CI's browser suite sets none, so requiring one here
//     would break the recipes rather than control anything.
//   - Present: exactly what it says, in both directions. CP_SEED_ENABLED=false
//     stops the scripts, which is the whole point of having a variable an
//     operator can reach for.
//
// An unparseable value is an error rather than a default, because the one
// thing worse than a switch that does nothing is a switch that reads "no" as
// "yes".
func SeedScriptsAllowed(env string, lookup func(string) (string, bool)) (bool, error) {
	name, err := ParseEnvironment(env)
	if err != nil {
		return false, err
	}
	switch name {
	case EnvLocal, EnvTest, EnvDev:
	default:
		return false, nil
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	raw, ok := lookup(EnvVarSeedEnabled)
	if !ok || strings.TrimSpace(raw) == "" {
		return true, nil
	}
	// Parsed by the same spec the requirements table carries, so this cannot
	// disagree with what Load would have produced for the same string.
	var c Config
	for _, s := range specs() {
		if s.Name != EnvVarSeedEnabled {
			continue
		}
		if err := s.apply(&c, raw); err != nil {
			return false, &VarError{Name: EnvVarSeedEnabled, Err: err}
		}
		return c.Seed.Enabled, nil
	}
	return false, fmt.Errorf("config: %s is no longer in the requirements table", EnvVarSeedEnabled)
}
