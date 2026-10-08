package quant

import (
	"math/big"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// checkMode rejects anything that is not a declared money.RoundingMode,
// including the zero value. Every rounding API calls it first, even when the
// particular inputs would not need rounding, so a bad mode is always caught.
func checkMode(mode money.RoundingMode) error {
	if !mode.Valid() {
		return errs.New(errs.CodeValidationFailed, "quant: invalid rounding mode").WithField("mode", mode.String())
	}
	return nil
}

// divRound returns num/den rounded per mode, with exactly the semantics of
// the money package's own rounding primitive (a property test cross-checks
// the two on random operands for every mode). The result is freshly
// allocated; num and den are not modified.
func divRound(num, den *big.Int, mode money.RoundingMode) (*big.Int, error) {
	if err := checkMode(mode); err != nil {
		return nil, err
	}
	if den.Sign() == 0 {
		return nil, errInvalid("division by zero")
	}
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int)) // truncated toward zero
	if rem.Sign() == 0 {
		return quo, nil
	}
	if mode == money.RoundExact {
		return nil, errPrecision("result is not exact at the requested scale")
	}
	negative := (num.Sign() < 0) != (den.Sign() < 0)
	awayFromZero := func() *big.Int {
		if negative {
			return quo.Sub(quo, bigOne())
		}
		return quo.Add(quo, bigOne())
	}
	switch mode {
	case money.RoundDown:
		return quo, nil
	case money.RoundUp:
		return awayFromZero(), nil
	case money.RoundFloor:
		if negative {
			return awayFromZero(), nil
		}
		return quo, nil
	case money.RoundCeil:
		if negative {
			return quo, nil
		}
		return awayFromZero(), nil
	case money.RoundHalfEven, money.RoundHalfUp:
		twiceRem := new(big.Int).Abs(rem)
		twiceRem.Lsh(twiceRem, 1)
		switch twiceRem.Cmp(new(big.Int).Abs(den)) {
		case -1:
			return quo, nil
		case 1:
			return awayFromZero(), nil
		default: // exact tie
			if mode == money.RoundHalfUp || quo.Bit(0) == 1 {
				return awayFromZero(), nil
			}
			return quo, nil
		}
	}
	// Unreachable: checkMode admitted only the declared modes.
	return nil, errInvalid("unsupported rounding mode")
}

// quoHalfEven returns num/den rounded half-even. It is the internal,
// error-free form of divRound for callers that guarantee den != 0.
func quoHalfEven(num, den *big.Int) *big.Int {
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() == 0 {
		return quo
	}
	twiceRem := rem.Abs(rem)
	twiceRem.Lsh(twiceRem, 1)
	c := twiceRem.Cmp(new(big.Int).Abs(den))
	if c > 0 || (c == 0 && quo.Bit(0) == 1) {
		if (num.Sign() < 0) != (den.Sign() < 0) {
			return quo.Sub(quo, bigOne())
		}
		return quo.Add(quo, bigOne())
	}
	return quo
}

// bigOne returns a fresh big.Int holding 1. The package keeps no
// package-level variables, so small constants are materialised on demand.
func bigOne() *big.Int { return big.NewInt(1) }
