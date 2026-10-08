package quant

import (
	"errors"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Reason codes carried in Fields["reason"] of the VALIDATION_FAILED errors
// returned by ImpliedVol when the target price has no solution. They are
// stable machine-readable values; clients switch on them, never on Detail.
const (
	// ReasonPriceBelowIntrinsic: the price is below the model-free lower
	// bound, the discounted forward intrinsic value max(S·e^{-qT} − K·e^{-rT}, 0)
	// for a call (mirrored for a put). No volatility can produce it.
	ReasonPriceBelowIntrinsic = "PRICE_BELOW_INTRINSIC"
	// ReasonPriceAboveUpperBound: the price is above the model-free upper
	// bound, S·e^{-qT} for a call or K·e^{-rT} for a put.
	ReasonPriceAboveUpperBound = "PRICE_ABOVE_UPPER_BOUND"
	// ReasonPriceBelowVolMin: the price is attainable in principle but only
	// with a volatility below IVVolMinBPS.
	ReasonPriceBelowVolMin = "PRICE_BELOW_VOL_MIN"
	// ReasonPriceAboveVolMax: the price would need a volatility above
	// IVVolMaxBPS.
	ReasonPriceAboveVolMax = "PRICE_ABOVE_VOL_MAX"
)

// errInvalid is the VALIDATION_FAILED error used for bad arguments.
func errInvalid(detail string) *errs.Error {
	return errs.New(errs.CodeValidationFailed, "quant: "+detail)
}

// errField is errInvalid naming the offending input field.
func errField(field, detail string) *errs.Error {
	return errInvalid(detail).WithField("field", field)
}

// errPrecision is the PRECISION_LOSS error returned when RoundExact was
// requested and the result is not exact.
func errPrecision(detail string) *errs.Error {
	return errs.New(errs.CodePrecisionLoss, "quant: "+detail)
}

// errOverflow is the OVERFLOW error returned when a result would leave the
// documented range of a function.
func errOverflow(detail string) *errs.Error {
	return errs.New(errs.CodeOverflow, "quant: "+detail)
}

// mapMoneyError translates a money sentinel into a stable errs code. Every
// error that leaves this package is an *errs.Error.
func mapMoneyError(err error, op string) error {
	switch {
	case errors.Is(err, money.ErrOverflow):
		return errs.Wrap(err, errs.CodeOverflow, "quant: "+op+": overflow")
	case errors.Is(err, money.ErrPrecisionLoss):
		return errs.Wrap(err, errs.CodePrecisionLoss, "quant: "+op+": precision loss")
	case errors.Is(err, money.ErrInvalidPrice):
		return errs.Wrap(err, errs.CodeValidationFailed, "quant: "+op+": invalid price")
	case errors.Is(err, money.ErrInvalidRoundingMode):
		return errs.Wrap(err, errs.CodeValidationFailed, "quant: "+op+": invalid rounding mode")
	case errors.Is(err, money.ErrDivisionByZero):
		return errs.Wrap(err, errs.CodeValidationFailed, "quant: "+op+": division by zero")
	}
	return errs.Wrap(err, errs.CodeInternal, "quant: "+op)
}
