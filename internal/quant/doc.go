// Package quant computes deterministic option analytics with exact integer
// arithmetic: a fixed-point decimal (Dec), certified transcendental
// functions, Black–Scholes–Merton prices and Greeks, an implied-volatility
// solver, and exact payoff and scenario helpers for margin.
//
// # Reference values, never truth
//
// Everything this package returns is a deterministic REFERENCE value for
// risk display and for margin scenario inputs. It is never ledger truth and
// never a guaranteed, quoted or executable market price: a model value says
// what the model computes from its inputs, not what anyone will pay.
// Nothing here may post to a ledger, and the package cannot reach one: it
// imports only the standard library, internal/money and internal/errs
// (enforced by TestImportsAreRestricted).
//
// # Numbers
//
// Dec is a big.Int coefficient with exactly Scale = 40 fractional digits.
// Add, Sub, Neg, Abs and comparisons are exact; Mul rounds half-even at the
// 40th digit; Quo, MulRound, Round, StringFixed and every conversion to
// money types take an explicit money.RoundingMode. ParseDec accepts only
// ["-"] digits ["." digits] and String renders a canonical form that parses
// back to the same value. Conversions: DecFromInt64, DecFromBPS,
// DecFromQuantity, DecFromPrice, Dec.ToInt64, Dec.ToQuantity and
// Dec.ToPrice.
//
// # Functions and their error bounds
//
// Exp, Ln, Sqrt, NormPDF and NormCDF return the exact mathematical result
// correctly rounded to 40 decimal places: absolute error at most ½·10^-40.
// Exp, Ln, NormPDF and NormCDF are evaluated in binary fixed point with a
// rigorous error bound per call (Taylor series after argument reduction for
// exp, an atanh series for ln, Marsaglia's positive series for Φ) and the
// decimal result is accepted only when the whole error interval rounds to
// it, at 192, 384 or 768 bits (Ziv's strategy); Sqrt uses integer Newton
// iteration and the exact remainder. Domains:
//
//   - Exp: x <= 100 (OVERFLOW above); exactly 0 for x <= −93.
//   - Ln: x > 0 (VALIDATION_FAILED otherwise).
//   - Sqrt: x >= 0 (VALIDATION_FAILED otherwise).
//   - NormPDF, NormCDF: every x. For |x| >= 14 NormPDF returns exactly 0
//     and NormCDF exactly 0 or 1, which are the correctly rounded values
//     there. NormCDF is monotone, lies in [0, 1] and satisfies
//     Φ(−x) = 1 − Φ(x) exactly.
//
// # Options
//
// BlackScholes prices European calls and puts with a continuous dividend
// yield and returns Delta, Gamma, Vega (per 1.00 of volatility; helper per
// volatility point), Theta (per year; helper per day of a 365-day year) and
// Rho. Inputs are
// validated against a documented domain. When σ√T < 10^-16 the option is
// valued at the σ√T → 0 limit, the discounted forward intrinsic value
// (RegimeIntrinsicLimit). ImpliedVol inverts the price on [0.0001, 5] by
// safeguarded Newton with bisection fallback and reports prices outside the
// model's range with a stable reason code.
//
// IntrinsicCall, IntrinsicPut, ScenarioGrid and ScenarioGridChecked work on
// money.Quantity without any Dec, so a margin engine can compute exact
// worst-case payoffs over a grid of shocked prices.
//
// # What this package must never do
//
//   - Post to a ledger, reserve, hold or move value, or import any package
//     that can. Its outputs feed displays and margin scenarios; the ledger
//     and the capital and settlement services decide what is owed.
//   - Present a model value as a market price, a quote or a guarantee.
//   - Use binary floating point in any form, including arbitrary-precision
//     binary floats and the floating point functions of package math, or
//     write a fractional literal in source. TestNoFloatingPointInSource
//     enforces this mechanically on every non-test file.
//   - Round implicitly. Every operation that can lose precision takes a
//     money.RoundingMode or documents its rounding (half-even at the 40th
//     digit, or correct rounding for the transcendental functions).
//   - Read a clock, use randomness, perform I/O, or keep package-level
//     variables: identical inputs give identical digits on every machine,
//     in every goroutine, every time.
//   - Panic on data. Apart from assertions of internal invariants that no
//     input can reach, the only panics are ScenarioGrid's documented
//     programmer-error panics (invalid or exact rounding mode);
//     ScenarioGridChecked reports those as errors instead.
//
// Errors are *errs.Error values: VALIDATION_FAILED for inputs outside a
// domain (with Fields["field"] naming the input), OVERFLOW and
// PRECISION_LOSS as in package money.
package quant
