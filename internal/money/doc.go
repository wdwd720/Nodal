// Package money provides the exact numeric types used for every financial
// value in the control plane:
//
//   - USD: United States dollars as int64 minor units (cents). All arithmetic
//     is overflow-checked and returns an error instead of wrapping.
//   - BPS: basis points as int64; 10_000 basis points is 100%.
//   - Quantity: an arbitrary-precision integer number of asset base units
//     (lamports, wei, USDC micro-units, ...). Immutable value semantics.
//   - Price: mantissa, scale, quote asset, source and timestamp. A price is
//     never a bare number; it always says what it is quoted in, where it came
//     from and when it was observed.
//   - RoundingMode: every operation that can lose precision takes an explicit
//     rounding mode. RoundExact turns any would-be rounding into an error.
//
// # Money is exact
//
// Every value is an integer in its smallest unit. Division, scaling and
// basis-point multiplication are computed exactly in big integers and rounded
// exactly once, at the end, in the mode the caller names. Two values that are
// equal render to identical strings, and every rendered string parses back to
// the same value. Parsers accept only the plain decimal grammar
// ["-"] digits ["." digits] and reject exponents, hexadecimal, whitespace,
// separators, NaN, infinities and empty input. USD and Quantity are
// serialized to JSON as strings and never as JSON numbers.
//
// # What this package must never do
//
//   - Represent, compute, parse or format a financial value with binary
//     floating point. No float types appear in this package; a test enforces
//     this by scanning the source.
//   - Round implicitly. There is no rounding API without a RoundingMode, and
//     the zero RoundingMode is invalid so an unset mode is an error rather
//     than a silent truncation.
//   - Wrap silently on overflow. Every int64 operation checks its bounds and
//     returns ErrOverflow.
//   - Hold global mutable state, perform I/O, read clocks, or know about
//     accounts, venues, eligibility, or any other domain concept. It decides
//     nothing about whether a quote asset is USD-pegged; callers do.
//   - Balance different assets against each other. A Quantity carries no
//     asset identity; the ledger, not this package, keeps assets apart.
//
// The errors returned here are local sentinels (ErrOverflow, ErrPrecisionLoss,
// ErrInvalidFormat, ErrDivisionByZero, ErrInvalidRoundingMode,
// ErrInvalidPrice) that wrap with %w so callers can map them to errs codes at
// the API boundary.
package money
