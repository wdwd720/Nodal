package money

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// maxUSDInputLen bounds the strings accepted by ParseUSD and
	// ParseUSDRound. The widest representable USD is 21 bytes; the headroom
	// allows leading zeros and extra fractional digits for ParseUSDRound.
	maxUSDInputLen = 64
	// maxDecimalInputLen bounds the strings accepted by the Quantity and
	// Price parsers so hostile input cannot force unbounded big-integer work.
	maxDecimalInputLen = 1024
)

// decimalParts is the lexical decomposition of a plain decimal literal.
type decimalParts struct {
	negative   bool
	intDigits  string // one or more ASCII digits
	fracDigits string // zero or more ASCII digits; empty when no '.' present
}

// parseDecimalLiteral accepts exactly the grammar
//
//	literal := ["-"] digit+ ["." digit+]
//	digit   := "0" ... "9"
//
// and nothing else: no leading "+", no whitespace, no exponent, no hex, no
// underscores or group separators, no non-ASCII digits, no "NaN"/"Inf",
// no bare "1." or ".5".
func parseDecimalLiteral(s string, maxLen int) (decimalParts, error) {
	var p decimalParts
	if s == "" {
		return p, fmt.Errorf("%w: empty string", ErrInvalidFormat)
	}
	if len(s) > maxLen {
		return p, fmt.Errorf("%w: input longer than %d bytes", ErrInvalidFormat, maxLen)
	}
	i := 0
	if s[0] == '-' {
		p.negative = true
		i = 1
	}
	start := i
	for i < len(s) && isASCIIDigit(s[i]) {
		i++
	}
	if i == start {
		return p, fmt.Errorf("%w: expected a digit at byte %d", ErrInvalidFormat, i)
	}
	p.intDigits = s[start:i]
	if i == len(s) {
		return p, nil
	}
	if s[i] != '.' {
		return p, fmt.Errorf("%w: unexpected character at byte %d", ErrInvalidFormat, i)
	}
	i++
	start = i
	for i < len(s) && isASCIIDigit(s[i]) {
		i++
	}
	if i == start {
		return p, fmt.Errorf("%w: expected a digit after the decimal point", ErrInvalidFormat)
	}
	if i != len(s) {
		return p, fmt.Errorf("%w: unexpected character at byte %d", ErrInvalidFormat, i)
	}
	p.fracDigits = s[start:i]
	return p, nil
}

func isASCIIDigit(c byte) bool { return '0' <= c && c <= '9' }

// formatCents renders an int64 count of hundredths as "[-]units.hh".
// It is shared by USD (cents) and BPS (hundredths of a percent) and is
// correct for the full int64 range including the minimum value.
func formatCents(n int64) string {
	var b strings.Builder
	mag := uint64(n) //nolint:gosec // G115: replaced below when n is negative
	if n < 0 {
		b.WriteByte('-')
		mag = uint64(-(n + 1)) + 1 //nolint:gosec // G115: -(n+1) is non-negative for every negative n, so this is exact even for math.MinInt64
	}
	b.WriteString(strconv.FormatUint(mag/100, 10))
	b.WriteByte('.')
	cents := mag % 100
	b.WriteByte(byte('0' + cents/10))
	b.WriteByte(byte('0' + cents%10))
	return b.String()
}
