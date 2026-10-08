package margin

import (
	"context"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// BuyingPowerReader reports a customer's margin state to buying power V2: it
// implements buyingpower.MarginReader over this package's own evaluation, so
// the universal buying power and the margin engine can never disagree about
// what an account owes or needs.
//
// The engine works in base units of the account's numeraire, a USD-pegged
// simulated asset; buying power works in USD cents. The conversion is at par
// and conservative in every field: what the account owes or must hold rounds
// UP, what it has gained rounds DOWN, so a rounding never shows more buying
// power than the engine would allow.
type BuyingPowerReader struct{ svc *Service }

// BuyingPowerReader returns the reader for this service.
func (s *Service) BuyingPowerReader() BuyingPowerReader { return BuyingPowerReader{svc: s} }

var _ buyingpower.MarginReader = BuyingPowerReader{}

// MarginState implements buyingpower.MarginReader. A customer without a
// margin account, or with a closed one, is a cash account: (zero, false).
func (r BuyingPowerReader) MarginState(ctx context.Context, q db.Querier, ownerType, ownerID string) (buyingpower.V2MarginState, bool, error) {
	if ownerType != "CUSTOMER" {
		return buyingpower.V2MarginState{}, false, nil
	}
	acct, err := accounts.ParseAccountID(ownerID)
	if err != nil {
		return buyingpower.V2MarginState{}, false, errs.New(errs.CodeValidationFailed, "margin: owner id must be a canonical uuid")
	}
	a, err := r.svc.st.accountByCustomer(ctx, q, acct, false)
	if errs.HasCode(err, errs.CodeNotFound) {
		return buyingpower.V2MarginState{}, false, nil
	}
	if err != nil {
		return buyingpower.V2MarginState{}, false, err
	}
	if a.Status == StatusClosed {
		return buyingpower.V2MarginState{}, false, nil
	}
	st, _, err := r.svc.loadState(ctx, q, a, r.svc.now())
	if err != nil {
		return buyingpower.V2MarginState{}, false, err
	}
	e, err := Evaluate(r.svc.policy, st)
	if err != nil {
		return buyingpower.V2MarginState{}, false, err
	}
	return v2State(e)
}

// v2State converts an evaluation to buying power's terms.
func v2State(e Evaluation) (buyingpower.V2MarginState, bool, error) {
	conv := func(q money.Quantity, mode money.RoundingMode) (money.USD, error) {
		return money.QuoteQuantityToUSD(q, e.NumeraireDecimals, mode)
	}
	var out buyingpower.V2MarginState
	var err error
	out.Mode = string(e.Mode)
	if out.BorrowLiabilities, err = conv(e.BorrowLiabilities, money.RoundCeil); err != nil {
		return out, false, err
	}
	if out.AccruedInterest, err = conv(e.AccruedInterest, money.RoundCeil); err != nil {
		return out, false, err
	}
	if out.AccruedFees, err = conv(e.AccruedFees, money.RoundCeil); err != nil {
		return out, false, err
	}
	if out.UnrealizedPnL, err = conv(e.UnrealizedPnL, money.RoundFloor); err != nil {
		return out, false, err
	}
	if out.InitialMarginUsed, err = conv(e.InitialMargin, money.RoundCeil); err != nil {
		return out, false, err
	}
	if out.MaintenanceMargin, err = conv(e.MaintenanceMargin, money.RoundCeil); err != nil {
		return out, false, err
	}
	out.InitialMarginRateBPS = initialRateFromLeverageCap(e.LeverageCapBPS)
	return out, true, nil
}

// initialRateFromLeverageCap is the initial margin rate a leverage cap
// implies: a 3x cap (30,000 bps) needs a third of the exposure as margin. It
// rounds up, so buying power derived from it never exceeds the cap; a cap of
// zero (no leverage) is a 100% rate.
func initialRateFromLeverageCap(capBPS money.BPS) money.BPS {
	if capBPS <= money.OneHundredPercent {
		return money.OneHundredPercent
	}
	const scale = int64(money.OneHundredPercent) * int64(money.OneHundredPercent)
	c := int64(capBPS)
	return money.BPS((scale + c - 1) / c)
}
