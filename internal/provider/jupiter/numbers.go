package jupiter

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Exact decoding of wire numbers. Every helper takes the raw JSON token of
// one field and the field's wire name so that a failure is a
// VALIDATION_FAILED naming that field. No float is ever formed.

// fieldError builds the VALIDATION_FAILED error for a wire field.
func fieldError(field, detail string) *errs.Error {
	return errs.New(errs.CodeValidationFailed, "jupiter: "+field+": "+detail).WithField("field", field)
}

// isNull reports whether raw is absent or the JSON null token.
func isNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// tokenKind classifies the first byte of a raw JSON token for messages.
func tokenKind(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return "absent"
	}
	switch t[0] {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// stringField decodes a required JSON string.
func stringField(raw json.RawMessage, field string) (string, error) {
	if isNull(raw) {
		return "", fieldError(field, "missing required field")
	}
	return optionalStringField(raw, field)
}

// optionalStringField decodes a JSON string; absent/null yields "".
func optionalStringField(raw json.RawMessage, field string) (string, error) {
	if isNull(raw) {
		return "", nil
	}
	if tokenKind(raw) != "string" {
		return "", fieldError(field, "must be a JSON string, got "+tokenKind(raw))
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fieldError(field, "invalid JSON string")
	}
	return s, nil
}

// quantityStringField decodes a required amount typed as a string in the
// OpenAPI specification. A JSON number, a fractional or exponent string, or
// anything but ASCII digits is rejected.
func quantityStringField(raw json.RawMessage, field string) (money.Quantity, error) {
	if isNull(raw) {
		return money.Quantity{}, fieldError(field, "missing required field")
	}
	return optionalQuantityStringField(raw, field)
}

// optionalQuantityStringField is quantityStringField for optional fields;
// absent/null yields zero.
func optionalQuantityStringField(raw json.RawMessage, field string) (money.Quantity, error) {
	if isNull(raw) {
		return money.Quantity{}, nil
	}
	if tokenKind(raw) != "string" {
		return money.Quantity{}, fieldError(field, "amount must be a JSON string, got "+tokenKind(raw)+" (float-formatted or numeric amounts are rejected)")
	}
	s, err := optionalStringField(raw, field)
	if err != nil {
		return money.Quantity{}, err
	}
	q, err := money.ParseQuantity(s)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeValidationFailed, "jupiter: "+field+": amount is not a decimal integer string").WithField("field", field)
	}
	return q, nil
}

// optionalQuantityPtrField is optionalQuantityStringField returning nil for
// absent fields so callers can tell "absent" from "zero".
func optionalQuantityPtrField(raw json.RawMessage, field string) (*money.Quantity, error) {
	if isNull(raw) {
		return nil, nil //nolint:nilnil // absent is a legitimate outcome distinct from zero
	}
	q, err := optionalQuantityStringField(raw, field)
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// isIntegerToken reports whether s is ^-?(0|[1-9][0-9]*)$.
func isIntegerToken(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	if s[0] == '0' {
		return len(s) == 1
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isPlainDecimalToken reports whether s is ^-?[0-9]+(\.[0-9]+)?$ (no
// exponent, no sign other than a leading minus).
func isPlainDecimalToken(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
	}
	intPart, frac, hasDot := strings.Cut(s, ".")
	if intPart == "" || (hasDot && frac == "") {
		return false
	}
	for _, part := range []string{intPart, frac} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return false
			}
		}
	}
	return true
}

// numberToken returns the exact text of a JSON number token. A JSON string
// is not accepted where the specification says number, so that a type drift
// on the wire is detected rather than silently tolerated.
func numberToken(raw json.RawMessage, field string) (string, error) {
	if tokenKind(raw) != "number" {
		return "", fieldError(field, "must be a JSON number, got "+tokenKind(raw))
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", fieldError(field, "invalid JSON number")
	}
	return string(n), nil
}

// integerNumberField decodes a required JSON number that must be an
// integer token (no fraction, no exponent) into int64.
func integerNumberField(raw json.RawMessage, field string) (int64, error) {
	if isNull(raw) {
		return 0, fieldError(field, "missing required field")
	}
	return optionalIntegerNumberField(raw, field)
}

// optionalIntegerNumberField is integerNumberField with absent/null = 0.
func optionalIntegerNumberField(raw json.RawMessage, field string) (int64, error) {
	if isNull(raw) {
		return 0, nil
	}
	text, err := numberToken(raw, field)
	if err != nil {
		return 0, err
	}
	if !isIntegerToken(text) {
		return 0, fieldError(field, "must be an integer JSON number, got float-formatted "+text)
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fieldError(field, "integer out of range")
	}
	return v, nil
}

// optionalQuantityNumberField decodes a lamport amount that the OpenAPI
// specification types as a JSON number (fee fields). Only an integer token
// is accepted; a fractional or exponent token is VALIDATION_FAILED.
func optionalQuantityNumberField(raw json.RawMessage, field string) (money.Quantity, error) {
	if isNull(raw) {
		return money.Quantity{}, nil
	}
	text, err := numberToken(raw, field)
	if err != nil {
		return money.Quantity{}, err
	}
	if !isIntegerToken(text) {
		return money.Quantity{}, fieldError(field, "amount must be an integer JSON number, got float-formatted "+text)
	}
	q, err := money.ParseQuantity(text)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeValidationFailed, "jupiter: "+field+": amount is not a decimal integer").WithField("field", field)
	}
	return q, nil
}

// uint64StringField decodes a required unsigned integer typed as a string
// (lastValidBlockHeight, slot).
func uint64StringField(raw json.RawMessage, field string) (uint64, error) {
	if isNull(raw) {
		return 0, fieldError(field, "missing required field")
	}
	return optionalUint64StringField(raw, field)
}

// optionalUint64StringField is uint64StringField with absent/null = 0.
func optionalUint64StringField(raw json.RawMessage, field string) (uint64, error) {
	if isNull(raw) {
		return 0, nil
	}
	if tokenKind(raw) != "string" {
		return 0, fieldError(field, "must be a JSON string, got "+tokenKind(raw))
	}
	s, err := optionalStringField(raw, field)
	if err != nil {
		return 0, err
	}
	if !isIntegerToken(s) || strings.HasPrefix(s, "-") {
		return 0, fieldError(field, "must be an unsigned decimal integer string")
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fieldError(field, "unsigned integer out of range")
	}
	return v, nil
}

// bpsField decodes a required basis-point value typed as a JSON number and
// checks it is within [-10000, 10000].
func bpsField(raw json.RawMessage, field string) (money.BPS, error) {
	v, err := integerNumberField(raw, field)
	if err != nil {
		return 0, err
	}
	if v < -10_000 || v > 10_000 {
		return 0, fieldError(field, "basis points outside [-10000, 10000]")
	}
	return money.BPS(v), nil
}

// optionalBPSField is bpsField with absent/null = 0.
func optionalBPSField(raw json.RawMessage, field string) (money.BPS, error) {
	if isNull(raw) {
		return 0, nil
	}
	return bpsField(raw, field)
}

// optionalBoolField decodes an optional JSON boolean.
func optionalBoolField(raw json.RawMessage, field string) (bool, error) {
	if isNull(raw) {
		return false, nil
	}
	if tokenKind(raw) != "boolean" {
		return false, fieldError(field, "must be a JSON boolean, got "+tokenKind(raw))
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, fieldError(field, "invalid JSON boolean")
	}
	return b, nil
}

// decimalToBPS converts the exact decimal text of a value expressed in
// units of 10^-unitDecimals basis points into whole basis points, rounding
// with RoundCeil so an impact is never understated. unitDecimals is 2 for
// percentage points (1% = 100 bps) and 4 for a fraction (1.0 = 10000 bps).
func decimalToBPS(text string, unitDecimals uint8) (money.BPS, error) {
	if !isPlainDecimalToken(text) {
		return 0, errs.New(errs.CodeValidationFailed, "jupiter: not a plain decimal: "+text)
	}
	q, err := money.QuantityFromDecimalString(text, unitDecimals, money.RoundCeil)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeValidationFailed, "jupiter: decimal conversion failed")
	}
	v, err := q.Int64()
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeOverflow, "jupiter: basis points overflow")
	}
	return money.BPS(v), nil
}

// priceImpactFromNumber derives whole basis points from the documented
// priceImpact JSON number (percentage points; "divide by 100 for decimal").
// The token text is used verbatim, so no binary float is ever formed. It
// returns ok=false (unavailable) for absent fields and for tokens that are
// not plain decimals (exponent forms).
func priceImpactFromNumber(raw json.RawMessage, field string) (money.BPS, bool, error) {
	if isNull(raw) {
		return 0, false, nil
	}
	text, err := numberToken(raw, field)
	if err != nil {
		return 0, false, err
	}
	if !isPlainDecimalToken(text) {
		return 0, false, nil
	}
	bps, err := decimalToBPS(text, 2)
	if err != nil {
		return 0, false, err
	}
	return bps, true, nil
}

// priceImpactFromFractionString derives whole basis points from a
// priceImpactPct string documented as a decimal fraction ("0.001" = 0.1%).
func priceImpactFromFractionString(raw json.RawMessage, field string) (money.BPS, bool, error) {
	if isNull(raw) {
		return 0, false, nil
	}
	s, err := optionalStringField(raw, field)
	if err != nil {
		return 0, false, err
	}
	if !isPlainDecimalToken(s) {
		return 0, false, nil
	}
	bps, err := decimalToBPS(s, 4)
	if err != nil {
		return 0, false, err
	}
	return bps, true, nil
}

// parseExpireAt interprets the documented but format-less expireAt string.
// ASSUMED: all-digit values are unix seconds (milliseconds when the value
// exceeds 10^12); anything else must be RFC 3339. Unparseable values are an
// error rather than a silent "no expiry".
func parseExpireAt(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if isIntegerToken(s) && !strings.HasPrefix(s, "-") {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return time.Time{}, fieldError("expireAt", "timestamp out of range")
		}
		if v > 1_000_000_000_000 {
			return time.UnixMilli(v).UTC(), nil
		}
		return time.Unix(v, 0).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fieldError("expireAt", "not a unix timestamp or RFC 3339 time")
	}
	return t.UTC(), nil
}
