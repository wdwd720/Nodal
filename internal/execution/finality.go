package execution

import (
	"github.com/nodal/controlplane/internal/errs"
)

// FinalityLevel is how much evidence exists that a submission is settled
// (PART 45). The four levels are always represented explicitly; nothing in
// the platform collapses them into "success".
type FinalityLevel string

// Finality levels, weakest first.
const (
	FinalitySubmitted FinalityLevel = "SUBMITTED"
	FinalityObserved  FinalityLevel = "OBSERVED"
	FinalityConfirmed FinalityLevel = "CONFIRMED"
	FinalityFinalized FinalityLevel = "FINALIZED"
)

// AllFinalityLevels returns the levels weakest first.
func AllFinalityLevels() []FinalityLevel {
	return []FinalityLevel{FinalitySubmitted, FinalityObserved, FinalityConfirmed, FinalityFinalized}
}

// Rank orders the levels; an unknown level ranks below SUBMITTED so that a
// typo can never satisfy a requirement.
func (l FinalityLevel) Rank() int {
	switch l {
	case FinalitySubmitted:
		return 1
	case FinalityObserved:
		return 2
	case FinalityConfirmed:
		return 3
	case FinalityFinalized:
		return 4
	}
	return 0
}

// Valid reports whether l is a declared level.
func (l FinalityLevel) Valid() bool { return l.Rank() > 0 }

// FinalityActionClass names what a finality requirement is for.
type FinalityActionClass string

// Action classes of EXECUTION.md §5.
const (
	FinalityForUIProvisional       FinalityActionClass = "UI_PROVISIONAL"
	FinalityForPositionProvisional FinalityActionClass = "POSITION_PROVISIONAL"
	FinalityForLedgerPosting       FinalityActionClass = "LEDGER_POSTING"
	FinalityForFundingAvailability FinalityActionClass = "FUNDING_AVAILABILITY"
	FinalityForWithdrawal          FinalityActionClass = "WITHDRAWAL"
)

// AllFinalityActionClasses returns every class in declaration order.
func AllFinalityActionClasses() []FinalityActionClass {
	return []FinalityActionClass{
		FinalityForUIProvisional, FinalityForPositionProvisional, FinalityForLedgerPosting,
		FinalityForFundingAvailability, FinalityForWithdrawal,
	}
}

// FinalityPolicy is the configurable requirement per action class
// (EXECUTION.md §5). Version names the configuration so plans and fills can
// record which policy they were evaluated under.
type FinalityPolicy struct {
	Version             string
	UIProvisional       FinalityLevel
	PositionProvisional FinalityLevel
	LedgerPosting       FinalityLevel
	FundingAvailability FinalityLevel
	Withdrawal          FinalityLevel
}

// DefaultFinalityPolicy is the documented default: UI at OBSERVED,
// provisional positions and ledger posting at CONFIRMED, funding
// availability and withdrawals at FINALIZED.
func DefaultFinalityPolicy() FinalityPolicy {
	return FinalityPolicy{
		Version:             "finality/1",
		UIProvisional:       FinalityObserved,
		PositionProvisional: FinalityConfirmed,
		LedgerPosting:       FinalityConfirmed,
		FundingAvailability: FinalityFinalized,
		Withdrawal:          FinalityFinalized,
	}
}

// Validate checks that every class names a declared level and that money
// movement never requires less evidence than the provisional display: ledger
// posting >= position provisional >= UI provisional, and funding availability
// and withdrawal >= ledger posting.
func (p FinalityPolicy) Validate() error {
	levels := map[string]FinalityLevel{
		"ui_provisional": p.UIProvisional, "position_provisional": p.PositionProvisional, "ledger_posting": p.LedgerPosting,
		"funding_availability": p.FundingAvailability, "withdrawal": p.Withdrawal,
	}
	for _, name := range []string{"ui_provisional", "position_provisional", "ledger_posting", "funding_availability", "withdrawal"} {
		if !levels[name].Valid() {
			return errs.Newf(errs.CodeValidationFailed, "finality policy: %s is not a declared level", name).WithField("field", name)
		}
	}
	if p.Version == "" {
		return errs.New(errs.CodeValidationFailed, "finality policy: version is required").WithField("field", "version")
	}
	if p.PositionProvisional.Rank() < p.UIProvisional.Rank() {
		return errs.New(errs.CodeValidationFailed, "finality policy: position_provisional must not be weaker than ui_provisional")
	}
	if p.LedgerPosting.Rank() < p.PositionProvisional.Rank() {
		return errs.New(errs.CodeValidationFailed, "finality policy: ledger_posting must not be weaker than position_provisional")
	}
	if p.FundingAvailability.Rank() < p.LedgerPosting.Rank() || p.Withdrawal.Rank() < p.LedgerPosting.Rank() {
		return errs.New(errs.CodeValidationFailed, "finality policy: funding_availability and withdrawal must not be weaker than ledger_posting")
	}
	return nil
}

// Required returns the level the class needs. An unknown class requires
// FINALIZED: the strongest evidence, so a new action class cannot accidentally
// act on weak evidence.
func (p FinalityPolicy) Required(class FinalityActionClass) FinalityLevel {
	switch class {
	case FinalityForUIProvisional:
		return p.UIProvisional
	case FinalityForPositionProvisional:
		return p.PositionProvisional
	case FinalityForLedgerPosting:
		return p.LedgerPosting
	case FinalityForFundingAvailability:
		return p.FundingAvailability
	case FinalityForWithdrawal:
		return p.Withdrawal
	}
	return FinalityFinalized
}

// Satisfies reports whether an observed level meets a required one. An
// unknown observed level never satisfies anything; an unknown required level
// can only be satisfied by FINALIZED (fail closed on both sides).
func (p FinalityPolicy) Satisfies(observed, required FinalityLevel) bool {
	if !observed.Valid() {
		return false
	}
	if !required.Valid() {
		return observed == FinalityFinalized
	}
	return observed.Rank() >= required.Rank()
}

// Stronger returns the stronger of two levels.
func Stronger(a, b FinalityLevel) FinalityLevel {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}
