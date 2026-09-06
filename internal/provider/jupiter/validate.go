package jupiter

import (
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// Validation reason codes, sorted and stable, carried in Fields["reasons"].
const (
	ReasonExpired           = "EXPIRED"
	ReasonTooOld            = "TOO_OLD"
	ReasonBlockHeight       = "BLOCK_HEIGHT_EXCEEDED"
	ReasonBlockHeightZero   = "BLOCK_HEIGHT_MISSING"
	ReasonInputMint         = "INPUT_MINT_MISMATCH"
	ReasonOutputMint        = "OUTPUT_MINT_MISMATCH"
	ReasonTaker             = "TAKER_MISMATCH"
	ReasonFeePayer          = "FEE_PAYER_MISMATCH"
	ReasonInAmount          = "IN_AMOUNT_MISMATCH"
	ReasonMinOut            = "MIN_OUTPUT_BELOW_EXPECTED"
	ReasonMinOutAboveQuote  = "MIN_OUTPUT_ABOVE_QUOTE"
	ReasonSlippage          = "SLIPPAGE_ABOVE_POLICY"
	ReasonPriceImpact       = "PRICE_IMPACT_ABOVE_POLICY"
	ReasonPriceImpactAbsent = "PRICE_IMPACT_UNAVAILABLE"
	ReasonNoTransaction     = "TRANSACTION_MISSING"
	ReasonNoQuoteID         = "QUOTE_ID_MISSING"
	ReasonNonPositive       = "NON_POSITIVE_AMOUNT"
	ReasonRouteMint         = "ROUTE_MINT_MISMATCH"
)

// ValidateQuote is the pure validation of an Order against a policy at
// time now (SETTLEMENT_COMPILER.md section 6, VALIDATE_QUOTE step). It
// returns nil when every check passes; otherwise a *errs.Error whose code
// is QUOTE_EXPIRED when any freshness check failed and VALIDATION_FAILED
// otherwise, with Fields["reasons"] listing every failed check sorted.
//
// Freshness: now must be before ExpiresAt; now-ReceivedAt must not exceed
// MaxAge; and, when CurrentBlockHeight is known, CurrentBlockHeight +
// MinBlockHeightMargin must be below LastValidBlockHeight.
//
// Identity and economics: mints, taker (and the transaction's fee payer),
// the input amount, OtherAmountThreshold >= ExpectedMinOut (and <=
// OutAmount), SlippageBPS and PriceImpactBPS within policy, route endpoints
// consistent with the mints, positive amounts, non-empty quote id and,
// when required, a transaction.
func ValidateQuote(o Order, now time.Time, p ValidationPolicy) error {
	var reasons []string
	expired := false
	add := func(r string) { reasons = append(reasons, r) }

	// Freshness.
	if o.QuoteID == "" {
		add(ReasonNoQuoteID)
	}
	if !o.ExpiresAt.IsZero() && !now.Before(o.ExpiresAt) {
		add(ReasonExpired)
		expired = true
	}
	if p.MaxAge > 0 && (o.ReceivedAt.IsZero() || now.Sub(o.ReceivedAt) > p.MaxAge || now.Before(o.ReceivedAt)) {
		add(ReasonTooOld)
		expired = true
	}
	if o.HasTransaction && o.LastValidBlockHeight == 0 {
		add(ReasonBlockHeightZero)
	}
	if p.CurrentBlockHeight > 0 && o.LastValidBlockHeight > 0 {
		if p.CurrentBlockHeight+p.MinBlockHeightMargin >= o.LastValidBlockHeight ||
			p.CurrentBlockHeight+p.MinBlockHeightMargin < p.CurrentBlockHeight { // overflow guard
			add(ReasonBlockHeight)
			expired = true
		}
	}

	// Identity.
	if p.ExpectedInputMint != "" && o.InputMint != p.ExpectedInputMint {
		add(ReasonInputMint)
	}
	if p.ExpectedOutputMint != "" && o.OutputMint != p.ExpectedOutputMint {
		add(ReasonOutputMint)
	}
	if p.ExpectedTaker != "" {
		if o.Taker != p.ExpectedTaker {
			add(ReasonTaker)
		}
		if o.HasTransaction && o.FeePayer != "" && o.FeePayer != p.ExpectedTaker {
			add(ReasonFeePayer)
		}
	}
	if len(o.Route) > 0 {
		if o.Route[0].InputMint != o.InputMint || o.Route[len(o.Route)-1].OutputMint != o.OutputMint {
			add(ReasonRouteMint)
		}
	}

	// Economics.
	if !o.InAmount.IsPositive() || !o.OutAmount.IsPositive() || !o.OtherAmountThreshold.IsPositive() {
		add(ReasonNonPositive)
	}
	if p.ExpectedInAmount != nil && !o.InAmount.Equal(*p.ExpectedInAmount) {
		add(ReasonInAmount)
	}
	if p.ExpectedMinOut != nil && o.OtherAmountThreshold.Cmp(*p.ExpectedMinOut) < 0 {
		add(ReasonMinOut)
	}
	if o.OtherAmountThreshold.Cmp(o.OutAmount) > 0 {
		add(ReasonMinOutAboveQuote)
	}
	if p.MaxSlippageBPS != nil && o.SlippageBPS > *p.MaxSlippageBPS {
		add(ReasonSlippage)
	}
	if o.PriceImpactUnavailable {
		if p.RequirePriceImpact {
			add(ReasonPriceImpactAbsent)
		}
	} else if p.MaxPriceImpactBPS != nil && o.PriceImpactBPS > *p.MaxPriceImpactBPS {
		add(ReasonPriceImpact)
	}
	if p.RequireTransaction && !o.HasTransaction {
		add(ReasonNoTransaction)
	}

	if len(reasons) == 0 {
		return nil
	}
	sort.Strings(reasons)
	code := errs.CodeValidationFailed
	detail := "jupiter: quote failed validation"
	if expired {
		code = errs.CodeQuoteExpired
		detail = "jupiter: quote is no longer fresh"
	}
	return errs.New(code, detail).WithField("reasons", reasons).WithField("quote_id", o.QuoteID)
}
