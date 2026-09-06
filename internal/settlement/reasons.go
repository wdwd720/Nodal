package settlement

import (
	"sort"

	"github.com/nodal/controlplane/internal/errs"
)

// NO_VALID_PLAN reason codes (PART 40, SETTLEMENT_COMPILER §3, §4). A plan
// rejection carries every applicable reason, sorted.
const (
	ReasonSettlementAssetUnavailable = "SETTLEMENT_ASSET_UNAVAILABLE"
	ReasonDeadlineImpossible         = "DEADLINE_IMPOSSIBLE"
	ReasonQuoteTooExpensive          = "QUOTE_TOO_EXPENSIVE"
	ReasonVenueDisabled              = "VENUE_DISABLED"
	ReasonLiquidityInsufficient      = "LIQUIDITY_INSUFFICIENT"
	ReasonProviderDegraded           = "PROVIDER_DEGRADED"
	ReasonEligibilityFailed          = "ELIGIBILITY_FAILED"
	ReasonRiskRejected               = "RISK_REJECTED"
	ReasonInstrumentStatus           = "INSTRUMENT_STATUS"
	ReasonKillSwitch                 = "KILL_SWITCH"
	ReasonReconciliationBlocked      = "RECONCILIATION_BLOCKED"
	ReasonNotionalBelowMinimum       = "NOTIONAL_BELOW_MINIMUM"
	ReasonNotionalAboveMaximum       = "NOTIONAL_ABOVE_MAXIMUM"
	ReasonNoEligibleListing          = "NO_ELIGIBLE_LISTING"
	ReasonDeltaBelowMinimum          = "DELTA_BELOW_MINIMUM"
)

// ReasonCodes returns every NO_VALID_PLAN reason, sorted.
func ReasonCodes() []string {
	out := []string{
		ReasonSettlementAssetUnavailable, ReasonDeadlineImpossible, ReasonQuoteTooExpensive, ReasonVenueDisabled,
		ReasonLiquidityInsufficient, ReasonProviderDegraded, ReasonEligibilityFailed, ReasonRiskRejected,
		ReasonInstrumentStatus, ReasonKillSwitch, ReasonReconciliationBlocked, ReasonNotionalBelowMinimum,
		ReasonNotionalAboveMaximum, ReasonNoEligibleListing, ReasonDeltaBelowMinimum,
	}
	sort.Strings(out)
	return out
}

// reasonSet collects reasons with details, de-duplicated and sorted.
type reasonSet struct {
	codes   map[string]bool
	details map[string][]string
}

func newReasonSet() *reasonSet {
	return &reasonSet{codes: map[string]bool{}, details: map[string][]string{}}
}

func (r *reasonSet) add(code, detail string) {
	r.codes[code] = true
	if detail != "" {
		r.details[code] = append(r.details[code], detail)
	}
}

func (r *reasonSet) empty() bool { return len(r.codes) == 0 }

func (r *reasonSet) sorted() []string {
	out := make([]string, 0, len(r.codes))
	for c := range r.codes {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (r *reasonSet) sortedDetails() map[string][]string {
	out := make(map[string][]string, len(r.details))
	for k, v := range r.details {
		d := append([]string(nil), v...)
		sort.Strings(d)
		out[k] = d
	}
	return out
}

// NoValidPlanError builds the NO_VALID_PLAN *errs.Error with the sorted
// reason codes in field "reasons" and per-reason details in "details".
func NoValidPlanError(reasons []string, details map[string][]string) *errs.Error {
	sorted := append([]string(nil), reasons...)
	sort.Strings(sorted)
	e := errs.New(errs.CodeNoValidPlan, "settlement: no valid execution plan").WithField("reasons", sorted)
	if len(details) > 0 {
		e = e.WithField("details", details)
	}
	return e
}

// NoValidPlanReasons extracts the reason codes from a NO_VALID_PLAN error,
// or nil when err is not one.
func NoValidPlanReasons(err error) []string {
	e, ok := errs.As(err)
	if !ok || e.Code != errs.CodeNoValidPlan {
		return nil
	}
	switch v := e.Fields["reasons"].(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
