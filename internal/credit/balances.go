package credit

import (
	"context"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Balances computes the breakdown of an account's Credits under a specific
// policy version (gola.md PART XX).
//
// It answers, per lot, "may this be withdrawn right now, and if not why not",
// then sums. Working lot by lot rather than in aggregate is the point: the
// answer for a 500-Credit balance made of a 400 promotional grant and a 100
// creator earning is not a property of the number 500.
//
// The result carries the policy version and hash that produced it, so a
// decision shown to a user in March can still be explained in June after the
// policy has changed twice.
func (s *Service) Balances(ctx context.Context, q db.Querier, r BalanceRequest) (Balances, error) {
	if r.AccountID.IsZero() {
		return Balances{}, errs.New(errs.CodeValidationFailed, "credit: balances require an account id")
	}
	if r.Now.IsZero() {
		return Balances{}, errs.New(errs.CodeValidationFailed, "credit: balances require the current time")
	}
	policyValid := r.Policy.Validate() == nil
	hash, err := r.Policy.Hash()
	if err != nil {
		return Balances{}, err
	}
	lots, err := s.Lots(ctx, q, r.AccountID)
	if err != nil {
		return Balances{}, err
	}
	decimals, err := s.AssetDecimals(ctx, q)
	if err != nil {
		return Balances{}, err
	}

	b := Balances{
		CreditDecimals:    decimals,
		Gross:             money.Quantity{},
		Spendable:         money.Quantity{},
		Frozen:            money.Quantity{},
		Reversed:          money.Quantity{},
		PayoutEligible:    money.Quantity{},
		Ineligible:        money.Quantity{},
		ByOrigin:          map[valuedomain.CreditOrigin]money.Quantity{},
		ByFinality:        map[valuedomain.FundingFinality]money.Quantity{},
		IneligibleReasons: map[valuedomain.PermitReason]int{},
		PolicyVersion:     r.Policy.Version,
		PolicyHash:        hash,
	}

	for _, lot := range lots {
		if !lot.Remaining.IsPositive() {
			continue
		}
		b.Gross = b.Gross.Add(lot.Remaining)
		// A missing key yields the zero Quantity, which adds correctly.
		b.ByOrigin[lot.Origin] = b.ByOrigin[lot.Origin].Add(lot.Remaining)
		b.ByFinality[lot.Finality] = b.ByFinality[lot.Finality].Add(lot.Remaining)

		switch lot.Finality {
		case valuedomain.FinalityDisputed:
			b.Frozen = b.Frozen.Add(lot.Remaining)
		case valuedomain.FinalityReversed:
			b.Reversed = b.Reversed.Add(lot.Remaining)
		}
		if lot.Finality.Spendable() {
			b.Spendable = b.Spendable.Add(lot.Remaining)
		}

		ok, reasons := r.Policy.Permits(valuedomain.PermitInput{
			Origin:      lot.Origin,
			OriginFloor: lot.OriginFloor,
			RootOrigins: lot.RootOrigins,
			Finality:    lot.Finality,
			Domain:      valuedomain.InternalCredit,
			Verified:    r.Verified,
			HeldDays:    lot.AgeDays(r.Now),
			ActiveCaps:  r.ActiveCaps,
			PolicyValid: policyValid,
		})
		if ok {
			b.PayoutEligible = b.PayoutEligible.Add(lot.Remaining)
			continue
		}
		b.Ineligible = b.Ineligible.Add(lot.Remaining)
		for _, reason := range reasons {
			b.IneligibleReasons[reason]++
		}
	}
	return b, nil
}

// EligibleLots returns the lots a payout of the given policy could draw on, in
// consumption order, together with how much of each is eligible.
//
// internal/payout uses it to decide what a payout may reserve, and to restrict
// consumption to exactly those origins. Returning the lots rather than a total
// is what stops a payout from taking an ineligible lot that happened to sort
// first.
func (s *Service) EligibleLots(ctx context.Context, q db.Querier, r BalanceRequest) ([]Lot, money.Quantity, error) {
	if r.Now.IsZero() {
		return nil, money.Quantity{}, errs.New(errs.CodeValidationFailed, "credit: eligible lots require the current time")
	}
	policyValid := r.Policy.Validate() == nil
	lots, err := s.Lots(ctx, q, r.AccountID)
	if err != nil {
		return nil, money.Quantity{}, err
	}
	total := money.Quantity{}
	var eligible []Lot
	for _, lot := range lots {
		if !lot.Remaining.IsPositive() {
			continue
		}
		ok, _ := r.Policy.Permits(valuedomain.PermitInput{
			Origin:      lot.Origin,
			OriginFloor: lot.OriginFloor,
			RootOrigins: lot.RootOrigins,
			Finality:    lot.Finality,
			Domain:      valuedomain.InternalCredit,
			Verified:    r.Verified,
			HeldDays:    lot.AgeDays(r.Now),
			ActiveCaps:  r.ActiveCaps,
			PolicyValid: policyValid,
		})
		if !ok {
			continue
		}
		eligible = append(eligible, lot)
		total = total.Add(lot.Remaining)
	}
	return eligible, total, nil
}

// EligibleLotIDs are the exact lots a decision approved, in the order it
// approved them.
//
// It is what a payout reservation passes to `ConsumeRequest.LotIDs`. The set of
// ORIGINS is not enough and never was: eligibility is decided per lot -- on the
// lot's finality, its origin and its provenance roots -- and two lots of one
// origin can differ in all three. Passing the origins let a decision approving a
// settled purchase be filled from a reversible one, and a decision approving
// proceeds out of a purchase be filled from proceeds out of a promotional grant
// (D-136, F-270).
func EligibleLotIDs(lots []Lot) []LotID {
	out := make([]LotID, 0, len(lots))
	for _, l := range lots {
		out = append(out, l.ID)
	}
	return out
}

// EligibleOrigins is the set of origins a payout under this policy may consume.
//
// It is reported on a Decision so a caller can say what KIND of value a payout
// draws on without re-deriving it. It is no longer the reservation's filter:
// see EligibleLotIDs.
func EligibleOrigins(lots []Lot) []valuedomain.CreditOrigin {
	seen := map[valuedomain.CreditOrigin]bool{}
	var out []valuedomain.CreditOrigin
	for _, l := range lots {
		if seen[l.Origin] {
			continue
		}
		seen[l.Origin] = true
		out = append(out, l.Origin)
	}
	return out
}
