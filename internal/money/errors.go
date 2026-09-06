package money

import "errors"

// Sentinel errors. Every error returned by this package wraps exactly one of
// these so that errors.Is works at any depth; the API layer maps them to
// stable errs codes (OVERFLOW, PRECISION_LOSS, VALIDATION_FAILED, ...).
var (
	// ErrOverflow is returned when a result does not fit the target type
	// (int64 for USD, the parse magnitude bound for Quantity).
	ErrOverflow = errors.New("money: overflow")
	// ErrPrecisionLoss is returned when a value carries more precision than
	// the target can hold and the caller did not ask for rounding
	// (ParseUSD with more than two decimals, or any operation in RoundExact
	// mode whose result is inexact).
	ErrPrecisionLoss = errors.New("money: precision loss")
	// ErrInvalidFormat is returned for input that is not a plain decimal
	// literal, for JSON numbers or null where strings are required, and for
	// unsupported database scan types.
	ErrInvalidFormat = errors.New("money: invalid format")
	// ErrDivisionByZero is returned when a divisor or ratio denominator is zero.
	ErrDivisionByZero = errors.New("money: division by zero")
	// ErrInvalidRoundingMode is returned when a RoundingMode is not one of
	// the declared constants, including the zero value.
	ErrInvalidRoundingMode = errors.New("money: invalid rounding mode")
	// ErrInvalidPrice is returned by Price.Validate and by every function
	// that consumes a Price which fails validation.
	ErrInvalidPrice = errors.New("money: invalid price")
)
