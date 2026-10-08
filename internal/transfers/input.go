package transfers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// MaxIdempotencyKeyLen bounds a client idempotency key. The economic intent
// is stored under "p2p:" + key, which stays inside the intent store's 256.
const MaxIdempotencyKeyLen = 200

// maxAmountLen bounds a decimal amount before any parsing is attempted.
const maxAmountLen = 64

// ParseAmount converts a decimal amount such as "12.5" into exact base units
// of an asset with the given number of decimals. It never rounds: an amount
// with more fractional digits than the asset has is PRECISION_LOSS, so what
// moves is exactly what the sender typed. Signs, exponents, separators and
// whitespace are refused, and the result must be positive.
func ParseAmount(s string, decimals uint8) (money.Quantity, error) {
	if s == "" || len(s) > maxAmountLen {
		return money.Quantity{}, errs.Newf(errs.CodeValidationFailed, "amount must be 1..%d characters", maxAmountLen)
	}
	if s[0] == '-' || s[0] == '+' {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "amount is unsigned and must be positive")
	}
	q, err := money.QuantityFromDecimalString(s, decimals, money.RoundExact)
	if err != nil {
		if errors.Is(err, money.ErrPrecisionLoss) {
			return money.Quantity{}, errs.Wrap(err, errs.CodePrecisionLoss,
				"amount has more decimal places than the asset; it would have to be rounded, and it never is").
				WithField("decimals", int(decimals))
		}
		if errors.Is(err, money.ErrOverflow) {
			return money.Quantity{}, errs.Wrap(err, errs.CodeOverflow, "amount is too large")
		}
		return money.Quantity{}, errs.Wrap(err, errs.CodeValidationFailed, "amount is not a plain decimal")
	}
	if !q.IsPositive() {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "amount must be positive")
	}
	return q, nil
}

// ValidateMemo checks a memo: valid UTF-8, at most maxRunes characters, and
// only printable characters (no control or format characters, so a memo
// cannot reorder or hide the text a recipient sees). The empty memo is valid
// and means "no memo".
func ValidateMemo(memo string, maxRunes int) error {
	if memo == "" {
		return nil
	}
	if maxRunes <= 0 || maxRunes > MaxMemoRunes {
		maxRunes = MaxMemoRunes
	}
	if !utf8.ValidString(memo) {
		return errs.New(errs.CodeValidationFailed, "memo must be valid UTF-8")
	}
	if n := utf8.RuneCountInString(memo); n > maxRunes {
		return errs.Newf(errs.CodeValidationFailed, "memo has %d characters; at most %d are allowed", n, maxRunes)
	}
	for _, r := range memo {
		if !unicode.IsPrint(r) {
			return errs.Newf(errs.CodeValidationFailed, "memo contains the non-printable character %U", r)
		}
	}
	if strings.TrimSpace(memo) != memo {
		return errs.New(errs.CodeValidationFailed, "memo may not start or end with whitespace")
	}
	return nil
}

// MemoDigest is the hex sha256 of a memo, or "" for no memo. The economic
// intent and the evidence carry the digest; the text lives in the transfer
// row only.
func MemoDigest(memo string) string {
	if memo == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(memo))
	return hex.EncodeToString(sum[:])
}

var handleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{2,31}$`)

// NormalizeHandle lower-cases an ASCII handle (an optional leading "@" is
// dropped) and checks it against the handle grammar shared with the
// database: 3..32 characters of [a-z0-9_.-], starting with a letter or digit.
// Non-ASCII handles are refused rather than case-folded, so two handles that
// render alike can never belong to two people.
func NormalizeHandle(s string) (string, error) {
	s = strings.TrimPrefix(s, "@")
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return "", errs.New(errs.CodeValidationFailed, "a handle is ASCII only")
		}
	}
	h := strings.ToLower(s)
	if !handleRE.MatchString(h) {
		return "", errs.New(errs.CodeValidationFailed,
			"a handle is 3..32 characters of a-z, 0-9, '_', '.', '-' and starts with a letter or digit")
	}
	return h, nil
}

var reasonCodeRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

// validReasonCode reports whether s is a stable machine-readable code.
func validReasonCode(s string) bool { return reasonCodeRE.MatchString(s) }

func validIdempotencyKey(k string) bool {
	return k != "" && strings.TrimSpace(k) == k && len(k) <= MaxIdempotencyKeyLen && utf8.ValidString(k)
}
