package equities

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// MaxRatioTerm bounds each term of a split ratio.
const MaxRatioTerm = 1_000_000

// Ratio is the number of new shares per old share, Numerator/Denominator: a
// 4-for-1 split is 4/1, a 1-for-10 reverse split is 1/10, a 3-for-2 split is
// 3/2.
type Ratio struct {
	Numerator   int64 `json:"numerator"`
	Denominator int64 `json:"denominator"`
}

// validateFor checks the ratio against the action type: a split increases
// the share count, a reverse split decreases it.
func (r Ratio) validateFor(t CorporateActionType) error {
	if r.Numerator <= 0 || r.Denominator <= 0 || r.Numerator > MaxRatioTerm || r.Denominator > MaxRatioTerm {
		return errs.Newf(errs.CodeValidationFailed, "equities: a split ratio has positive terms of at most %d", MaxRatioTerm)
	}
	switch t {
	case ActionSplit:
		if r.Numerator <= r.Denominator {
			return errs.New(errs.CodeValidationFailed, "equities: a split gives more new shares than old (numerator > denominator)")
		}
	case ActionReverseSplit:
		if r.Numerator >= r.Denominator {
			return errs.New(errs.CodeValidationFailed, "equities: a reverse split gives fewer new shares than old (numerator < denominator)")
		}
	}
	return nil
}

// PricePoint is an exact amount of the settlement asset per whole share:
// Mantissa × 10^-Scale. A dividend is declared as one; a cash-in-lieu price
// is one per whole new share.
type PricePoint struct {
	Mantissa money.Quantity `json:"mantissa"`
	Scale    int32          `json:"scale"`
}

func (p PricePoint) validate(what string) error {
	if !p.Mantissa.IsPositive() || p.Scale < 0 || p.Scale > 18 {
		return errs.Newf(errs.CodeValidationFailed, "equities: %s is a positive amount at a scale within 0..18", what)
	}
	return nil
}

// CorporateRounding is the explicit rounding policy of a corporate action.
// Shares always round down to a whole base unit -- rounding a position up
// would create shares that do not exist -- and the fraction given up is paid
// as cash-in-lieu, whose rounding the policy names; dividends round in the
// same cash mode.
type CorporateRounding string

// Rounding policies.
const (
	// RoundingSharesDownCashDown never pays more than the exact amount: the
	// paying agent's convention, and the default.
	RoundingSharesDownCashDown CorporateRounding = "SHARES_DOWN_CASH_DOWN"
	// RoundingSharesDownCashHalfEven pays the nearest base unit, ties to even.
	RoundingSharesDownCashHalfEven CorporateRounding = "SHARES_DOWN_CASH_HALF_EVEN"
)

// AllCorporateRoundings returns every policy
// (equity_corporate_actions_rounding_policy_check).
func AllCorporateRoundings() []CorporateRounding {
	return []CorporateRounding{RoundingSharesDownCashDown, RoundingSharesDownCashHalfEven}
}

// Modes returns the share and cash rounding modes of the policy.
func (p CorporateRounding) Modes() (shares, cash money.RoundingMode, err error) {
	switch p {
	case RoundingSharesDownCashDown:
		return money.RoundDown, money.RoundDown, nil
	case RoundingSharesDownCashHalfEven:
		return money.RoundDown, money.RoundHalfEven, nil
	}
	return 0, 0, errs.Newf(errs.CodeValidationFailed, "equities: unknown corporate action rounding policy %q", p)
}

// SplitOutcome is one holder's result of a split or reverse split.
//
// With old holding Q base units and ratio N/D, the exact new holding is
// Q×N/D base units. NewQuantity is that rounded down to a whole base unit
// and Remainder/Denominator (0 <= Remainder < Denominator) is the fraction of
// one new base unit given up, so that, exactly,
//
//	Q × N = NewQuantity × D + Remainder.
//
// The fraction is paid in cash at the cash-in-lieu price P (per whole new
// share): CashExactNum/CashExactDen cash base units exactly, and CashInLieu
// once rounded in the policy's cash mode. Economic value is preserved
// exactly: Q × (P × N/D) = NewQuantity × P + the exact cash-in-lieu.
type SplitOutcome struct {
	OldQuantity  money.Quantity `json:"old_quantity"`
	NewQuantity  money.Quantity `json:"new_quantity"`
	Remainder    money.Quantity `json:"remainder"`
	Denominator  money.Quantity `json:"denominator"`
	CashExactNum money.Quantity `json:"cash_exact_numerator"`
	CashExactDen money.Quantity `json:"cash_exact_denominator"`
	CashInLieu   money.Quantity `json:"cash_in_lieu"`
	Policy       CorporateRounding
}

// SharesDelta returns NewQuantity − OldQuantity (positive for a split,
// negative or zero for a reverse split).
func (o SplitOutcome) SharesDelta() money.Quantity { return o.NewQuantity.Sub(o.OldQuantity) }

// ComputeSplit computes one holder's split outcome. cashInLieu is required
// whenever the holding leaves a fraction of a base unit.
func ComputeSplit(old money.Quantity, ratio Ratio, t CorporateActionType, shareDecimals, cashDecimals uint8,
	cashInLieu *PricePoint, policy CorporateRounding) (SplitOutcome, error) {
	if t != ActionSplit && t != ActionReverseSplit {
		return SplitOutcome{}, errs.Newf(errs.CodeValidationFailed, "equities: %s is not a split", t)
	}
	if err := ratio.validateFor(t); err != nil {
		return SplitOutcome{}, err
	}
	shareMode, cashMode, err := policy.Modes()
	if err != nil {
		return SplitOutcome{}, err
	}
	if old.IsNegative() {
		return SplitOutcome{}, errs.New(errs.CodeValidationFailed, "equities: a holding cannot be negative")
	}
	if shareDecimals > 18 || cashDecimals > 18 {
		return SplitOutcome{}, errs.New(errs.CodeValidationFailed, "equities: decimals must be within 0..18")
	}
	n := money.QuantityFromInt64(ratio.Numerator)
	d := money.QuantityFromInt64(ratio.Denominator)
	scaled := old.Mul(n)
	newQty, err := scaled.Div(d, shareMode)
	if err != nil {
		return SplitOutcome{}, errs.Wrap(err, errs.CodeInternal, "equities: split shares")
	}
	remainder := scaled.Sub(newQty.Mul(d))
	out := SplitOutcome{
		OldQuantity: old, NewQuantity: newQty, Remainder: remainder, Denominator: d,
		CashExactNum: money.QuantityFromInt64(0), CashExactDen: money.QuantityFromInt64(1),
		CashInLieu: money.QuantityFromInt64(0), Policy: policy,
	}
	if remainder.IsZero() {
		return out, nil
	}
	if cashInLieu == nil {
		return SplitOutcome{}, errs.New(errs.CodeValidationFailed,
			"equities: this holding leaves a fractional share and the action states no cash-in-lieu price")
	}
	if err := cashInLieu.validate("a cash-in-lieu price"); err != nil {
		return SplitOutcome{}, err
	}
	// remainder/D new base units × (m / 10^s per whole share) / 10^sd whole
	// shares per base unit × 10^cd cash base units per whole cash unit.
	out.CashExactNum = remainder.Mul(cashInLieu.Mantissa).Mul(pow10(int(cashDecimals)))
	out.CashExactDen = d.Mul(pow10(int(cashInLieu.Scale) + int(shareDecimals)))
	if out.CashInLieu, err = out.CashExactNum.Div(out.CashExactDen, cashMode); err != nil {
		return SplitOutcome{}, errs.Wrap(err, errs.CodeInternal, "equities: cash-in-lieu")
	}
	return out, nil
}

// DividendOutcome is one holder's cash dividend.
type DividendOutcome struct {
	Quantity  money.Quantity `json:"entitled_quantity"`
	ExactNum  money.Quantity `json:"exact_numerator"`
	ExactDen  money.Quantity `json:"exact_denominator"`
	Amount    money.Quantity `json:"amount"`
	Policy    CorporateRounding
	PerShare  PricePoint `json:"per_share"`
	CashScale uint8      `json:"cash_decimals"`
}

// ComputeDividend computes the dividend on qty share base units at perShare
// (cash per whole share), in cash base units rounded once in the policy's
// cash mode.
func ComputeDividend(qty money.Quantity, shareDecimals, cashDecimals uint8, perShare PricePoint, policy CorporateRounding) (DividendOutcome, error) {
	_, cashMode, err := policy.Modes()
	if err != nil {
		return DividendOutcome{}, err
	}
	if err := perShare.validate("a dividend per share"); err != nil {
		return DividendOutcome{}, err
	}
	if qty.IsNegative() || shareDecimals > 18 || cashDecimals > 18 {
		return DividendOutcome{}, errs.New(errs.CodeValidationFailed, "equities: invalid dividend input")
	}
	out := DividendOutcome{Quantity: qty, Policy: policy, PerShare: perShare, CashScale: cashDecimals}
	out.ExactNum = qty.Mul(perShare.Mantissa).Mul(pow10(int(cashDecimals)))
	out.ExactDen = pow10(int(perShare.Scale) + int(shareDecimals))
	if out.Amount, err = out.ExactNum.Div(out.ExactDen, cashMode); err != nil {
		return DividendOutcome{}, errs.Wrap(err, errs.CodeInternal, "equities: dividend amount")
	}
	return out, nil
}
