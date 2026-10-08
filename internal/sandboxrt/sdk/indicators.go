package nodalsdk

import "errors"

// ErrWindow is returned when an indicator's window is not positive or is
// larger than the series.
var ErrWindow = errors.New("nodalsdk: indicator window out of range")

// ErrEmpty is returned for an indicator over an empty series.
var ErrEmpty = errors.New("nodalsdk: empty series")

// Sum returns the exact sum of values.
func Sum(values []Decimal) Decimal {
	total := Decimal{}
	for _, v := range values {
		total = total.Add(v)
	}
	return total
}

// SMA is the simple moving average of the last window values, rounded to
// scale with mode.
func SMA(values []Decimal, window int, scale int32, mode Rounding) (Decimal, error) {
	if window <= 0 || window > len(values) {
		return Decimal{}, ErrWindow
	}
	return Sum(values[len(values)-window:]).QuoInt(int64(window), scale, mode)
}

// EMA is the exponential moving average with smoothing 2/(window+1). It is
// seeded with the SMA of the first window values and then, for every later
// value v, ema = ema + (v - ema) x 2 / (window + 1), each step rounded to
// scale with mode. The result is exact for that stated rounding rule.
func EMA(values []Decimal, window int, scale int32, mode Rounding) (Decimal, error) {
	if window <= 0 || window > len(values) {
		return Decimal{}, ErrWindow
	}
	ema, err := Sum(values[:window]).QuoInt(int64(window), scale, mode)
	if err != nil {
		return Decimal{}, err
	}
	den := int64(window) + 1
	for _, v := range values[window:] {
		step, err := v.Sub(ema).MulInt(2).QuoInt(den, scale, mode)
		if err != nil {
			return Decimal{}, err
		}
		ema = ema.Add(step)
	}
	return ema, nil
}

// Min returns the smallest value.
func Min(values []Decimal) (Decimal, error) {
	if len(values) == 0 {
		return Decimal{}, ErrEmpty
	}
	m := values[0]
	for _, v := range values[1:] {
		if v.Cmp(m) < 0 {
			m = v
		}
	}
	return m, nil
}

// Max returns the largest value.
func Max(values []Decimal) (Decimal, error) {
	if len(values) == 0 {
		return Decimal{}, ErrEmpty
	}
	m := values[0]
	for _, v := range values[1:] {
		if v.Cmp(m) > 0 {
			m = v
		}
	}
	return m, nil
}

// Returns are the simple period returns (v[i] - v[i-1]) / v[i-1], each
// rounded to scale with mode. A zero previous value is an error rather than a
// silent infinity.
func Returns(values []Decimal, scale int32, mode Rounding) ([]Decimal, error) {
	if len(values) < 2 {
		return nil, ErrWindow
	}
	out := make([]Decimal, 0, len(values)-1)
	for i := 1; i < len(values); i++ {
		r, err := values[i].Sub(values[i-1]).Quo(values[i-1], scale, mode)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
