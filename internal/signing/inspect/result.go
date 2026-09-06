package inspect

import (
	"sort"
	"strings"
)

// InspectorVersion is recorded on every signing decision so a decision can be
// re-evaluated against the exact rule set that produced it. Bump on any rule
// change.
const InspectorVersion = "inspect/1.0.0"

// Check names, exactly as listed in EXECUTION.md §2.
const (
	CheckFeePayer                = "FEE_PAYER"
	CheckSigners                 = "SIGNERS"
	CheckProgramAllowlist        = "PROGRAM_ALLOWLIST"
	CheckTokenProgram            = "TOKEN_PROGRAM"
	CheckInputDebitBound         = "INPUT_DEBIT_BOUND"
	CheckOutputToken             = "OUTPUT_TOKEN"
	CheckMinOutput               = "MIN_OUTPUT"
	CheckNoSystemTransfer        = "NO_SYSTEM_TRANSFER"
	CheckNoUnexpectedDestination = "NO_UNEXPECTED_DESTINATION"
	CheckNoAuthorityChange       = "NO_AUTHORITY_CHANGE"
	CheckNoArbitraryCPI          = "NO_ARBITRARY_CPI"
	CheckComputeBudget           = "COMPUTE_BUDGET"
	CheckBlockhash               = "BLOCKHASH"
	CheckPlanIdentity            = "PLAN_IDENTITY"
	CheckSlippage                = "SLIPPAGE"
	CheckSimulation              = "SIMULATION"
)

// CheckNames lists every check in canonical (sorted) order. A Result always
// contains exactly these checks in this order.
var CheckNames = func() []string {
	names := []string{
		CheckFeePayer, CheckSigners, CheckProgramAllowlist, CheckTokenProgram, CheckInputDebitBound,
		CheckOutputToken, CheckMinOutput, CheckNoSystemTransfer, CheckNoUnexpectedDestination,
		CheckNoAuthorityChange, CheckNoArbitraryCPI, CheckComputeBudget, CheckBlockhash,
		CheckPlanIdentity, CheckSlippage, CheckSimulation,
	}
	sort.Strings(names)
	return names
}()

// Reason codes. Stable, machine-readable, sorted in Result.ReasonCodes.
const (
	ReasonDecodeError            = "DECODE_ERROR"
	ReasonTransactionMalformed   = "TRANSACTION_MALFORMED"
	ReasonExpectationsInvalid    = "EXPECTATIONS_INVALID"
	ReasonInspectorPanic         = "INSPECTOR_PANIC"
	ReasonLookupTableUnresolved  = "LOOKUP_TABLE_UNRESOLVED"
	ReasonFeePayerMismatch       = "FEE_PAYER_MISMATCH"
	ReasonExtraSigner            = "EXTRA_SIGNER"
	ReasonSignerMismatch         = "SIGNER_MISMATCH"
	ReasonProgramNotAllowed      = "PROGRAM_NOT_ALLOWED"
	ReasonUnknownInstruction     = "UNKNOWN_INSTRUCTION"
	ReasonRouteCount             = "ROUTE_COUNT"
	ReasonToken2022Disabled      = "TOKEN_2022_DISABLED" // #nosec G101 -- inspection reason code, not a credential
	ReasonUnexpectedMint         = "UNEXPECTED_MINT"
	ReasonTokenAccountUnknown    = "TOKEN_ACCOUNT_UNKNOWN"
	ReasonTokenProgramMismatch   = "TOKEN_PROGRAM_MISMATCH"
	ReasonInputDebitExceeded     = "INPUT_DEBIT_EXCEEDED"
	ReasonInputAmountZero        = "INPUT_AMOUNT_ZERO"
	ReasonOutputAccountMismatch  = "OUTPUT_ACCOUNT_MISMATCH"
	ReasonOutputMintMismatch     = "OUTPUT_MINT_MISMATCH"
	ReasonMinOutputBelowPlan     = "MIN_OUTPUT_BELOW_PLAN"
	ReasonSystemTransfer         = "SYSTEM_TRANSFER_FROM_WALLET"
	ReasonWrapExceeded           = "WRAP_ALLOWANCE_EXCEEDED"
	ReasonUnexpectedDestination  = "UNEXPECTED_DESTINATION"
	ReasonAuthorityChange        = "AUTHORITY_CHANGE"
	ReasonDelegation             = "DELEGATION"
	ReasonUnexpectedClose        = "UNEXPECTED_CLOSE"
	ReasonPriorityFeeExceeded    = "PRIORITY_FEE_EXCEEDED"
	ReasonComputeUnitsExceeded   = "COMPUTE_UNITS_EXCEEDED"
	ReasonComputeBudgetDuplicate = "COMPUTE_BUDGET_DUPLICATE"
	ReasonBlockhashMismatch      = "BLOCKHASH_MISMATCH"
	ReasonBlockhashExpired       = "BLOCKHASH_EXPIRED"
	ReasonPlanHashMismatch       = "PLAN_HASH_MISMATCH"
	ReasonQuoteIDMismatch        = "QUOTE_ID_MISMATCH"
	ReasonSlippageExceeded       = "SLIPPAGE_EXCEEDED"
	ReasonSimulationMissing      = "SIMULATION_MISSING"
	ReasonSimulationFailed       = "SIMULATION_FAILED"
	ReasonSimulationMismatch     = "SIMULATION_MISMATCH"
	ReasonSimulationDelta        = "SIMULATION_DELTA_VIOLATION"
)

// Check is the outcome of one named rule.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// Result is the deterministic outcome of Inspect. Approved is true only when
// every check passed. Checks are in CheckNames order; ReasonCodes are sorted
// and de-duplicated and empty when approved.
type Result struct {
	Approved    bool     `json:"approved"`
	Checks      []Check  `json:"checks"`
	ReasonCodes []string `json:"reason_codes"`
}

// failure is one recorded rule violation.
type failure struct {
	code   string
	detail string
}

// collector accumulates failures per check and renders a Result.
type collector struct {
	fails map[string][]failure
	notes map[string][]string // informational details for passed checks
}

func newCollector() *collector {
	return &collector{fails: map[string][]failure{}, notes: map[string][]string{}}
}

func (c *collector) fail(check, code, detail string) {
	c.fails[check] = append(c.fails[check], failure{code: code, detail: detail})
}

func (c *collector) note(check, detail string) {
	c.notes[check] = append(c.notes[check], detail)
}

func (c *collector) result() Result {
	res := Result{Approved: true, Checks: make([]Check, 0, len(CheckNames))}
	codes := map[string]struct{}{}
	for _, name := range CheckNames {
		fs := c.fails[name]
		if len(fs) == 0 {
			detail := "ok"
			if n := c.notes[name]; len(n) > 0 {
				detail = strings.Join(n, "; ")
			}
			res.Checks = append(res.Checks, Check{Name: name, Passed: true, Detail: detail})
			continue
		}
		res.Approved = false
		details := make([]string, 0, len(fs))
		for _, f := range fs {
			codes[f.code] = struct{}{}
			details = append(details, f.code+": "+f.detail)
		}
		res.Checks = append(res.Checks, Check{Name: name, Passed: false, Detail: strings.Join(details, "; ")})
	}
	res.ReasonCodes = make([]string, 0, len(codes))
	for code := range codes {
		res.ReasonCodes = append(res.ReasonCodes, code)
	}
	sort.Strings(res.ReasonCodes)
	return res
}

// RejectAll returns a Result in which every check is failed with the same
// reason code and detail. It is used when the transaction could not be
// evaluated at all (decode error, unresolvable lookup table, malformed
// expectations, inspector panic): a transaction that cannot be inspected is
// never approved.
func RejectAll(code, detail string) Result {
	c := newCollector()
	for _, name := range CheckNames {
		c.fail(name, code, "not evaluated: "+detail)
	}
	return c.result()
}
