package margin

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// The functions in this file are the hypothetical state changes the engine
// evaluates before the service is allowed to make the real one. They are
// pure; each returns a new state and never changes its input.

// LoanAllocation is the part of a repayment that went to one lot.
type LoanAllocation struct {
	LotID     string         `json:"lot_id"`
	Interest  money.Quantity `json:"interest"`
	Principal money.Quantity `json:"principal"`
}

// Total is interest plus principal.
func (a LoanAllocation) Total() money.Quantity { return a.Interest.Add(a.Principal) }

// allocateRepay repays loans in slice order (the service orders lots oldest
// first), capitalized interest before principal. It returns the new loans,
// the allocation per lot touched and what was left unallocated.
func allocateRepay(loans []Loan, amount money.Quantity) ([]Loan, []LoanAllocation, money.Quantity) {
	out := append([]Loan(nil), loans...)
	var allocs []LoanAllocation
	left := amount
	for i := range out {
		if !left.IsPositive() {
			break
		}
		a := LoanAllocation{LotID: out[i].LotID}
		a.Interest = minQ(left, out[i].Interest)
		left = left.Sub(a.Interest)
		a.Principal = minQ(left, out[i].Principal)
		left = left.Sub(a.Principal)
		if a.Total().IsPositive() {
			out[i].Interest = out[i].Interest.Sub(a.Interest)
			out[i].Principal = out[i].Principal.Sub(a.Principal)
			allocs = append(allocs, a)
		}
	}
	return out, allocs, left
}

// WithBorrow adds a new lot. The borrowed cash goes to the customer's wallet
// (MARGIN_BORROW), outside the margin account, so the account gains the
// liability and none of the cash.
func WithBorrow(s AccountState, lotID string, amount money.Quantity, rate money.BPS) (AccountState, error) {
	if !amount.IsPositive() {
		return AccountState{}, errs.New(errs.CodeValidationFailed, "margin: a borrow amount must be positive")
	}
	out := s.Clone()
	out.Loans = append(out.Loans, Loan{LotID: lotID, Principal: amount, Interest: money.Quantity{}, RateBPS: rate, AccruedThrough: s.Now})
	return out, nil
}

// WithRepay repays from outside the account (the wallet): interest first,
// oldest lot first. Repaying more than is owed is refused.
func WithRepay(s AccountState, amount money.Quantity) (AccountState, []LoanAllocation, error) {
	if !amount.IsPositive() {
		return AccountState{}, nil, errs.New(errs.CodeValidationFailed, "margin: a repayment must be positive")
	}
	if amount.Cmp(s.LoanTotal()) > 0 {
		return AccountState{}, nil, errs.New(errs.CodeValidationFailed, "margin: the repayment exceeds what is owed").
			WithField("owed", s.LoanTotal().String()).WithField("repayment", amount.String())
	}
	out := s.Clone()
	var allocs []LoanAllocation
	out.Loans, allocs, _ = allocateRepay(out.Loans, amount)
	return out, allocs, nil
}

// WithCollateral adds qty of an asset to the account. template describes the
// asset (decimals, profile, mark) when the account does not hold it yet.
func WithCollateral(s AccountState, template Holding, qty money.Quantity) (AccountState, error) {
	if !qty.IsPositive() {
		return AccountState{}, errs.New(errs.CodeValidationFailed, "margin: a collateral quantity must be positive")
	}
	out := s.Clone()
	if i := out.holding(template.AssetID); i >= 0 {
		out.Holdings[i].Quantity = out.Holdings[i].Quantity.Add(qty)
		return out, nil
	}
	h := template
	h.Quantity = qty
	if h.Mark != nil {
		m := *h.Mark
		h.Mark = &m
	}
	out.Holdings = append(out.Holdings, h)
	return out, nil
}

// WithRelease removes qty of an asset from the account.
func WithRelease(s AccountState, assetID string, qty money.Quantity) (AccountState, error) {
	if !qty.IsPositive() {
		return AccountState{}, errs.New(errs.CodeValidationFailed, "margin: a release quantity must be positive")
	}
	out := s.Clone()
	i := out.holding(assetID)
	if i < 0 || out.Holdings[i].Quantity.Cmp(qty) < 0 {
		return AccountState{}, errs.New(errs.CodeInsufficientBuyingPower, "margin: the account does not hold that much collateral").
			WithField("asset_id", assetID).WithField("requested", qty.String())
	}
	out.Holdings[i].Quantity = out.Holdings[i].Quantity.Sub(qty)
	return out, nil
}

// LeveragedBuyCost is what buying qty at an execution price costs, rounded
// up (RoundCeil): the buyer never pays less than the exact product.
func LeveragedBuyCost(s AccountState, template Holding, qty money.Quantity, exec money.Price) (money.Quantity, error) {
	if !qty.IsPositive() {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "margin: a buy quantity must be positive")
	}
	return notional(qty, template.Decimals, exec, s.NumeraireDecimals, money.RoundCeil)
}

// WithLeveragedBuy finances a buy of qty at exec entirely with a new lot and
// adds the asset to the account's collateral: the loan grows by the cost and
// the holding by qty.
func WithLeveragedBuy(s AccountState, template Holding, qty money.Quantity, exec money.Price, lotID string, rate money.BPS) (AccountState, money.Quantity, error) {
	cost, err := LeveragedBuyCost(s, template, qty, exec)
	if err != nil {
		return AccountState{}, money.Quantity{}, err
	}
	if !cost.IsPositive() {
		return AccountState{}, money.Quantity{}, errs.New(errs.CodeValidationFailed, "margin: the buy costs nothing; there is nothing to finance")
	}
	out, err := WithBorrow(s, lotID, cost, rate)
	if err != nil {
		return AccountState{}, money.Quantity{}, err
	}
	out, err = WithCollateral(out, template, qty)
	if err != nil {
		return AccountState{}, money.Quantity{}, err
	}
	return out, cost, nil
}
