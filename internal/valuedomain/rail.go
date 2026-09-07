package valuedomain

import (
	"strings"

	"github.com/nodal/controlplane/internal/errs"
)

// CapitalRail is where value is held and executed, and who is authoritative
// for it (PART XXII).
//
// A rail is not a provider. HOSTED_PARTNER is a rail; Zero Hash or BitGo would
// be providers on it. Choosing a rail is a Settlement Compiler decision;
// choosing a provider within a rail is a routing decision.
type CapitalRail string

// Capital rails.
const (
	// RailSimulated executes against simulated markets. No provider, no
	// custody, no settlement, no identity requirement.
	RailSimulated CapitalRail = "SIMULATED"

	// RailNativeInternal executes against Nodal's own deterministic market
	// engine and ledger. Nodal is authoritative for balances and execution.
	RailNativeInternal CapitalRail = "NATIVE_INTERNAL"

	// RailHostedPartner executes through a licensed financial partner that
	// holds custody and is authoritative for balances (PART XXIII).
	RailHostedPartner CapitalRail = "HOSTED_PARTNER"

	// RailSelfCustodialOnchain executes on a public chain against a wallet the
	// customer controls. The chain is authoritative (PART XXIV).
	RailSelfCustodialOnchain CapitalRail = "SELF_CUSTODIAL_ONCHAIN"

	// RailSecuritiesBroker is declared and permanently unsupported here. It
	// exists so that code which switches on a rail is exhaustive and so that
	// adding it later is a capability activation rather than a redesign.
	RailSecuritiesBroker CapitalRail = "SECURITIES_BROKER"

	// RailPredictionDCM is declared and permanently unsupported here, for the
	// same reason as RailSecuritiesBroker.
	RailPredictionDCM CapitalRail = "PREDICTION_DCM"
)

type railInfo struct {
	// authoritativeBalanceSource names who owns the truth for balances.
	authoritativeBalanceSource string
	custodyModel               string
	executionModel             string
	settlementModel            string
	reconciliationModel        string
	// implemented reports whether this build can execute on the rail at all.
	// A declared-but-unimplemented rail must never be selected by the
	// Settlement Compiler, no matter what a policy row says.
	implemented bool
	// requiresFinancialIdentity reports whether the rail cannot be used
	// without financial (as opposed to Nodal) identity verification.
	requiresFinancialIdentity bool
}

var railRegistry = map[CapitalRail]railInfo{
	RailSimulated: {
		authoritativeBalanceSource: "NODAL_SIMULATION",
		custodyModel:               "NONE",
		executionModel:             "SIMULATED_FILL",
		settlementModel:            "NONE",
		reconciliationModel:        "NONE",
		implemented:                true,
	},
	RailNativeInternal: {
		authoritativeBalanceSource: "NODAL_LEDGER",
		custodyModel:               "NODAL_INTERNAL_RECORD",
		executionModel:             "NODAL_DETERMINISTIC_MARKET",
		settlementModel:            "ATOMIC_LEDGER_COMMIT",
		reconciliationModel:        "LEDGER_VS_MARKET_STATE",
		implemented:                true,
	},
	RailHostedPartner: {
		authoritativeBalanceSource: "PROVIDER",
		custodyModel:               "PARTNER_CUSTODY",
		executionModel:             "PARTNER_EXECUTION",
		settlementModel:            "PARTNER_SETTLEMENT",
		reconciliationModel:        "PROVIDER_STATEMENT_VS_MIRROR",
		implemented:                true,
		requiresFinancialIdentity:  true,
	},
	RailSelfCustodialOnchain: {
		authoritativeBalanceSource: "CHAIN",
		custodyModel:               "CUSTOMER_CONTROLLED_WALLET",
		executionModel:             "CUSTOMER_AUTHORIZED_TRANSACTION",
		settlementModel:            "CHAIN_FINALITY",
		reconciliationModel:        "CHAIN_VS_MIRROR",
		implemented:                true,
	},
	RailSecuritiesBroker: {
		authoritativeBalanceSource: "BROKER",
		custodyModel:               "BROKER_CUSTODY",
		executionModel:             "BROKER_EXECUTION",
		settlementModel:            "BROKER_SETTLEMENT",
		reconciliationModel:        "BROKER_STATEMENT_VS_MIRROR",
		implemented:                false,
		requiresFinancialIdentity:  true,
	},
	RailPredictionDCM: {
		authoritativeBalanceSource: "DCM_OR_INTERMEDIARY",
		custodyModel:               "INTERMEDIARY_CUSTODY",
		executionModel:             "DCM_EXECUTION",
		settlementModel:            "DCM_SETTLEMENT",
		reconciliationModel:        "DCM_STATEMENT_VS_MIRROR",
		implemented:                false,
		requiresFinancialIdentity:  true,
	},
}

var allRails = []CapitalRail{
	RailSimulated, RailNativeInternal, RailHostedPartner, RailSelfCustodialOnchain,
	RailSecuritiesBroker, RailPredictionDCM,
}

// AllRails returns every declared rail in declaration order (a copy).
func AllRails() []CapitalRail { return append([]CapitalRail(nil), allRails...) }

// Valid reports whether r is a declared rail.
func (r CapitalRail) Valid() bool {
	_, ok := railRegistry[r]
	return ok
}

func (r CapitalRail) String() string { return string(r) }

// Implemented reports whether this build can execute on the rail. Declared but
// unimplemented rails exist to keep switches exhaustive; selecting one is a
// programming error, and the Settlement Compiler refuses it regardless of
// policy (PART XXII: "do not implement live unsupported products merely
// because an interface exists").
func (r CapitalRail) Implemented() bool { return railRegistry[r].implemented }

// RequiresFinancialIdentity reports whether the rail cannot be used without
// financial identity verification, which is distinct from Nodal identity
// (PART XLVII).
func (r CapitalRail) RequiresFinancialIdentity() bool {
	return railRegistry[r].requiresFinancialIdentity
}

// AuthoritativeBalanceSource names who owns the truth for balances on the
// rail. Where it is not NODAL_LEDGER, Nodal holds a mirror and a divergence is
// a reconciliation finding, never a silent correction (PART LXXXIII).
func (r CapitalRail) AuthoritativeBalanceSource() string {
	return railRegistry[r].authoritativeBalanceSource
}

// CustodyModel, ExecutionModel, SettlementModel and ReconciliationModel are
// the remaining rail descriptors required by PART XXII. They are strings
// rather than enums because they are descriptive metadata surfaced to
// operators and documentation, not inputs to any decision.
func (r CapitalRail) CustodyModel() string        { return railRegistry[r].custodyModel }
func (r CapitalRail) ExecutionModel() string      { return railRegistry[r].executionModel }
func (r CapitalRail) SettlementModel() string     { return railRegistry[r].settlementModel }
func (r CapitalRail) ReconciliationModel() string { return railRegistry[r].reconciliationModel }

// Domains returns the value domains that live on the rail, in canonical order.
func (r CapitalRail) Domains() []Domain {
	var out []Domain
	for _, d := range allDomains {
		if domainRegistry[d].rail == r {
			out = append(out, d)
		}
	}
	return out
}

// ParseCapitalRail parses the canonical uppercase string form.
func ParseCapitalRail(s string) (CapitalRail, error) {
	r := CapitalRail(strings.ToUpper(strings.TrimSpace(s)))
	if !r.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown capital rail %q", s)
	}
	return r, nil
}
