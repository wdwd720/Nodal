package gates

import (
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/money"
)

// Mode is the execution mode an advanced capability is exercised in.
//
// PAPER, SANDBOX and DEVNET move only value domain SIMULATED: a paper dollar,
// a share in a simulated book, a token on a local simulated chain. LIVE would
// move real value, and in this build no advanced capability implements it.
type Mode string

// Modes.
const (
	// ModePaper is a simulated fill against a reference price or an internal
	// simulated book.
	ModePaper Mode = "PAPER"
	// ModeSandbox is a full simulated workflow against a deterministic
	// provider (a simulated broker, a simulated clearing house).
	ModeSandbox Mode = "SANDBOX"
	// ModeDevnet is a workflow that signs and submits to a simulated public
	// chain (internal/devnet), with real signatures and real finality states
	// over value that does not exist outside it.
	ModeDevnet Mode = "DEVNET"
	// ModeLive would move real value. Every advanced capability refuses it.
	ModeLive Mode = "LIVE"
)

var allModes = []Mode{ModePaper, ModeSandbox, ModeDevnet, ModeLive}

// AllModes returns every mode in declaration order (a copy).
func AllModes() []Mode { return append([]Mode(nil), allModes...) }

// Valid reports whether m is a declared mode.
func (m Mode) Valid() bool {
	for _, x := range allModes {
		if x == m {
			return true
		}
	}
	return false
}

// Simulated reports whether the mode moves only SIMULATED value.
func (m Mode) Simulated() bool { return m == ModePaper || m == ModeSandbox || m == ModeDevnet }

// Evidence kinds a live activation of an advanced capability would require,
// in addition to the four every high-risk gate already demands.
const (
	EvidenceLegalReview      = "LEGAL_REVIEW"
	EvidenceProviderContract = "PROVIDER_CONTRACT"
	EvidenceRiskApproval     = "RISK_APPROVAL"
	EvidenceSecurityApproval = "SECURITY_APPROVAL"
	EvidenceLicense          = "REGULATORY_LICENSE_OR_EXEMPTION"
	EvidenceAttestation      = "RESERVE_ATTESTATION"
	EvidenceCreditPolicy     = "CREDIT_POLICY"
	EvidenceSandboxAudit     = "SANDBOX_ISOLATION_AUDIT"
	EvidenceCustodyAnalysis  = "CUSTODY_ANALYSIS"
)

// AdvancedSpec is the gate specification of one advanced capability: where it
// may run, in which modes, and everything a live activation would require.
//
// The live fields are not decoration. They are what DecideMode would enforce
// if LiveImplemented were true, and the capability matrix renders them so an
// operator can read, per capability, exactly what stands between this build
// and real customer exposure.
type AdvancedSpec struct {
	Capability Capability
	// SimulatedModes are the modes this build implements.
	SimulatedModes []Mode
	// UngatedEnvironments are where the simulated modes run without an ACTIVE
	// gate row. Anywhere else -- STAGING and PROD -- even a simulated mode
	// needs the gate, because exposing a paper product to real users of a
	// deployed system is itself a product decision someone has to own.
	UngatedEnvironments []string
	// LiveImplemented is false for every capability in this build. It is the
	// single fact that makes LIVE unreachable: DecideMode refuses LIVE when it
	// is false whatever the gate, the configuration or the caller says.
	LiveImplemented bool
	// LiveEvidence is the evidence a live activation would require.
	LiveEvidence []string
	// MinApprovers for a live activation (beyond the proposer).
	MinApprovers int
	// MaxLiveValidity bounds a live activation's validity window.
	MaxLiveValidity time.Duration
	// MaxLiveCapital is the most real value a first live activation could
	// put at risk. Zero means none at all.
	MaxLiveCapital money.USD
	// Jurisdictions a live activation would be limited to. Empty means no
	// jurisdiction has been analysed, which permits none.
	Jurisdictions []string
	// ProviderReadiness names what an external provider would have to prove
	// before live use, in words an operator can check.
	ProviderReadiness string
}

var sandboxOnly = []string{"LOCAL", "TEST", "DEV"}

func spec(c Capability, modes []Mode, evidence []string, provider string) AdvancedSpec {
	ev := append([]string{EvidenceLegalReview, EvidenceProviderContract, EvidenceRiskApproval, EvidenceSecurityApproval}, evidence...)
	return AdvancedSpec{
		Capability:          c,
		SimulatedModes:      modes,
		UngatedEnvironments: append([]string(nil), sandboxOnly...),
		LiveImplemented:     false,
		LiveEvidence:        ev,
		MinApprovers:        2,
		MaxLiveValidity:     30 * 24 * time.Hour,
		MaxLiveCapital:      money.USD{},
		Jurisdictions:       nil,
		ProviderReadiness:   provider,
	}
}

var paperSandbox = []Mode{ModePaper, ModeSandbox}

// advancedSpecs is the table of every advanced capability. A test asserts it
// names each advanced capability exactly once.
var advancedSpecs = []AdvancedSpec{
	spec(StableAssetMint, []Mode{ModeSandbox, ModeDevnet}, []string{EvidenceLicense, EvidenceAttestation},
		"a qualified reserve custodian and an independent attestation provider under contract"),
	spec(Margin, paperSandbox, []string{EvidenceLicense, EvidenceCreditPolicy},
		"a licensed margin lender or broker-dealer carrying the credit"),
	spec(Leverage, paperSandbox, []string{EvidenceLicense, EvidenceCreditPolicy},
		"a licensed margin lender or broker-dealer carrying the credit"),
	spec(Derivatives, paperSandbox, []string{EvidenceLicense},
		"a registered derivatives venue and clearing member"),
	spec(Equities, paperSandbox, []string{EvidenceLicense},
		"a registered broker-dealer with a production execution API"),
	spec(InternalMatching, paperSandbox, []string{EvidenceLicense},
		"a registered ATS or exchange operating the book"),
	spec(P2PTransfers, paperSandbox, []string{EvidenceLicense},
		"a money-transmission licence or a licensed partner holding the balances"),
	spec(PrincipalTrading, paperSandbox, []string{EvidenceLicense},
		"segregated house capital and a registered dealer"),
	spec(SpreadEngine, paperSandbox, nil,
		"a published, disclosed fee schedule approved by compliance"),
	spec(PerformanceFees, paperSandbox, []string{EvidenceLicense},
		"a registered adviser able to charge performance-based compensation"),
	spec(StrategyMarketplace, paperSandbox, []string{EvidenceLicense},
		"a registered adviser or platform exemption for distributing strategies"),
	spec(UserCodeSandbox, []Mode{ModeSandbox}, []string{EvidenceSandboxAudit},
		"an independent sandbox-escape review of the runtime and its host"),
	spec(AgentSigning, []Mode{ModeSandbox, ModeDevnet}, []string{EvidenceCustodyAnalysis},
		"a wallet provider whose delegated-signing policy semantics are verified"),
	spec(AgentWithdrawals, []Mode{ModeSandbox, ModeDevnet}, []string{EvidenceCustodyAnalysis},
		"a withdrawal provider with verified destination controls"),
	spec(CrossChainBuyingPower, []Mode{ModeSandbox, ModeDevnet}, nil,
		"a bridge provider with documented finality and failure semantics"),
	spec(PredictionMarkets, paperSandbox, []string{EvidenceLicense},
		"a designated contract market or an exemption covering event contracts"),
}

// AdvancedSpecs returns every advanced capability's spec in declaration order.
func AdvancedSpecs() []AdvancedSpec {
	out := make([]AdvancedSpec, len(advancedSpecs))
	copy(out, advancedSpecs)
	return out
}

// LookupAdvanced returns the spec for an advanced capability.
func LookupAdvanced(c Capability) (AdvancedSpec, bool) {
	for _, s := range advancedSpecs {
		if s.Capability == c {
			return s, true
		}
	}
	return AdvancedSpec{}, false
}

// Reason codes DecideMode returns. They are stable and machine-readable.
const (
	ReasonModeUnknown            = "MODE_UNKNOWN"
	ReasonNotAdvancedCapability  = "NOT_AN_ADVANCED_CAPABILITY"
	ReasonLiveNotImplemented     = "LIVE_NOT_IMPLEMENTED"
	ReasonModeNotSupported       = "MODE_NOT_SUPPORTED"
	ReasonEnvironmentUnknown     = "ENVIRONMENT_UNKNOWN"
	ReasonGateNotActive          = "CAPABILITY_GATE_NOT_ACTIVE"
	ReasonCapabilityKillSwitched = "CAPABILITY_KILL_SWITCHED"
)

// ModeRequest is everything DecideMode needs. Every field is resolved by the
// caller from deployment state; DecideMode reads nothing else.
type ModeRequest struct {
	Capability  Capability
	Environment string
	Mode        Mode
	// GateActive is the Checker's answer for (Capability, Environment).
	GateActive bool
	// KillSwitched is true when an operator has switched the capability off.
	KillSwitched bool
}

// ModeDecision is DecideMode's answer.
type ModeDecision struct {
	Allowed bool
	// GateRequired reports whether the decision depended on an ACTIVE gate.
	GateRequired bool
	Reasons      []string
}

// DecideMode decides whether an advanced capability may be exercised in a
// mode in an environment. It is pure and fails closed:
//
//   - LIVE is refused for every advanced capability in this build
//     (LIVE_NOT_IMPLEMENTED). No gate state, configuration value or operator
//     can change that; only a code change that implements live and flips
//     LiveImplemented can.
//   - A simulated mode must be one the capability implements.
//   - In LOCAL, TEST and DEV a simulated mode needs no gate.
//   - In STAGING and PROD a simulated mode needs the gate ACTIVE.
//   - A kill switch refuses everything.
//
// Every applicable reason is returned, sorted, so a caller learns everything
// that would have to change at once.
func DecideMode(r ModeRequest) ModeDecision {
	var reasons []string
	s, ok := LookupAdvanced(r.Capability)
	if !ok {
		return ModeDecision{Reasons: []string{ReasonNotAdvancedCapability}}
	}
	if !validEnvironment(r.Environment) {
		reasons = append(reasons, ReasonEnvironmentUnknown)
	}
	gateRequired := false
	switch {
	case !r.Mode.Valid():
		reasons = append(reasons, ReasonModeUnknown)
	case r.Mode == ModeLive:
		if !s.LiveImplemented {
			reasons = append(reasons, ReasonLiveNotImplemented)
		}
		// A live implementation would also need the gate everywhere.
		gateRequired = true
		if !r.GateActive {
			reasons = append(reasons, ReasonGateNotActive)
		}
	default:
		supported := false
		for _, m := range s.SimulatedModes {
			if m == r.Mode {
				supported = true
			}
		}
		if !supported {
			reasons = append(reasons, ReasonModeNotSupported)
		}
		ungated := false
		for _, e := range s.UngatedEnvironments {
			if e == r.Environment {
				ungated = true
			}
		}
		if !ungated {
			gateRequired = true
			if !r.GateActive {
				reasons = append(reasons, ReasonGateNotActive)
			}
		}
	}
	if r.KillSwitched {
		reasons = append(reasons, ReasonCapabilityKillSwitched)
	}
	sort.Strings(reasons)
	return ModeDecision{Allowed: len(reasons) == 0, GateRequired: gateRequired, Reasons: reasons}
}
