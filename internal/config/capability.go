package config

import (
	"sort"
	"strings"
)

// capabilityNames is every capability this system declares, restated here so
// that a configuration check can refuse a name nothing answers to.
//
// # Why it is a restatement and not an import
//
// `internal/gates` is the authority: `gates.AllCapabilities()` is the list, and
// `gates.Capability.Valid()` is the test. This package cannot ask it. The
// import would be config -> gates, and `internal/gates`' own integration test
// imports `internal/agent`, which imports this package -- so the edge compiles
// and then `go vet -tags integration ./...` reports an import cycle in a test
// binary. Moving the type to a leaf package would fix it properly and touches
// every file in the repository that names a capability, which is not this
// change.
//
// A list duplicated in two places diverges, and the copy nobody greps is the
// one that rots -- this register says so in F-130 and again in D-079. So the
// duplication is machine-checked rather than trusted:
// `TestConfigTable_TheCapabilityNamesAreTheOnesGatesDeclares` in `test/infra`
// holds this slice equal to `gates.AllCapabilities()`, in a package that may
// import both. A capability added, renamed or removed in `internal/gates`
// fails that test rather than silently making a deployment's list
// unvalidatable.
//
// The other direction is why it is worth the cost: CP_API_ENABLED_CAPABILITIES
// is condition 1 of the policy authority, so a misspelled name is a capability
// switched off with a healthy service and a 200 on every probe (F-145).
var capabilityNames = []string{
	"LIVE_FUNDING",
	"LIVE_MANUAL_TRADING",
	"LIVE_AGENT_TRADING",
	"WITHDRAWALS",
	"SOCIAL_DATA_PERSISTENCE",
	"MARKETPLACE",
	"CROSS_CHAIN",
	"PREDICTION_MARKETS",
	"SECURITIES",
	"CEX_TRADING",
	"CREDIT_PURCHASE",
	"NATIVE_ASSET_CREATION",
	"NATIVE_MARKET_TRADING",
	"PAYOUT_RESERVE",
	"PAYOUT_SETTLE",
	"HOSTED_TRADING",
	"HOSTED_FUNDING",
	"AGENT_BOUNDED_DISCRETION",
	"AGENT_AUTONOMOUS_SELECTION",
	"AGENT_AUTONOMOUS_PORTFOLIO",
}

// CapabilityNames returns the declared capability names, sorted.
func CapabilityNames() []string {
	out := append([]string(nil), capabilityNames...)
	sort.Strings(out)
	return out
}

// IsDeclaredCapability reports whether name is a capability this system
// declares. Case and surrounding space are forgiven, exactly as the lists in
// CP_API_ENABLED_CAPABILITIES and CP_API_SANDBOX_GATES are read.
func IsDeclaredCapability(name string) bool {
	want := strings.ToUpper(strings.TrimSpace(name))
	for _, c := range capabilityNames {
		if c == want {
			return true
		}
	}
	return false
}
