package money

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// JSON, text and database/sql encodings.
//
// USD and Quantity are always JSON strings ("1234.56", "1500000000") and
// never JSON numbers, because a JSON number is a binary floating point value
// to most consumers. Decoding rejects numbers and null outright: a null
// amount is not an amount. Optional amounts should be modeled as *USD /
// *Quantity, which encoding/json sets to nil on null without calling
// UnmarshalJSON.
//
// In SQL, USD is a BIGINT of cents and Quantity is a NUMERIC(38,0) whose
// text form is exchanged as a string.

// MarshalJSON renders the amount as a JSON string such as "1234.56".
func (u USD) MarshalJSON() ([]byte, error) { return json.Marshal(u.String()) }

// UnmarshalJSON accepts only a JSON string in ParseUSD format. JSON numbers
// and null fail with ErrInvalidFormat.
func (u *USD) UnmarshalJSON(b []byte) error {
	s, err := jsonString(b, "USD", `"1234.56"`)
	if err != nil {
		return err
	}
	v, err := ParseUSD(s)
	if err != nil {
		return err
	}
	*u = v
	return nil
}

// MarshalText renders the amount exactly as String does.
func (u USD) MarshalText() ([]byte, error) { return []byte(u.String()), nil }

// UnmarshalText parses with ParseUSD.
func (u *USD) UnmarshalText(b []byte) error {
	v, err := ParseUSD(string(b))
	if err != nil {
		return err
	}
	*u = v
	return nil
}

// Value returns the amount in cents as an int64 for a BIGINT column.
func (u USD) Value() (driver.Value, error) { return u.minor, nil }

// Scan accepts an int64 count of cents as produced by a BIGINT column.
// NULL and every other type fail with ErrInvalidFormat; use sql.Null[USD]
// for nullable columns.
func (u *USD) Scan(src any) error {
	switch v := src.(type) {
	case int64:
		*u = USD{minor: v}
		return nil
	case nil:
		return fmt.Errorf("%w: cannot scan NULL into USD (use sql.Null[USD])", ErrInvalidFormat)
	default:
		return fmt.Errorf("%w: cannot scan %T into USD (expected int64 minor units)", ErrInvalidFormat, src)
	}
}

// MarshalJSON renders the quantity as a JSON string of decimal digits.
func (q Quantity) MarshalJSON() ([]byte, error) { return json.Marshal(q.String()) }

// UnmarshalJSON accepts only a JSON string in ParseQuantity format. JSON
// numbers and null fail with ErrInvalidFormat.
func (q *Quantity) UnmarshalJSON(b []byte) error {
	s, err := jsonString(b, "Quantity", `"1500000000"`)
	if err != nil {
		return err
	}
	v, err := ParseQuantity(s)
	if err != nil {
		return err
	}
	*q = v
	return nil
}

// MarshalText renders the quantity exactly as String does.
func (q Quantity) MarshalText() ([]byte, error) { return []byte(q.String()), nil }

// UnmarshalText parses with ParseQuantity.
func (q *Quantity) UnmarshalText(b []byte) error {
	v, err := ParseQuantity(string(b))
	if err != nil {
		return err
	}
	*q = v
	return nil
}

// Value returns the decimal text form for a NUMERIC column.
func (q Quantity) Value() (driver.Value, error) { return q.String(), nil }

// Scan accepts the text form of a NUMERIC column (string or []byte, as pgx
// and database/sql deliver it) or an int64 from an integer column. NULL and
// every other type fail with ErrInvalidFormat; use sql.Null[Quantity] for
// nullable columns.
func (q *Quantity) Scan(src any) error {
	switch v := src.(type) {
	case string:
		r, err := ScanQuantity(v)
		if err != nil {
			return err
		}
		*q = r
		return nil
	case []byte:
		r, err := ScanQuantity(string(v))
		if err != nil {
			return err
		}
		*q = r
		return nil
	case int64:
		*q = QuantityFromInt64(v)
		return nil
	case nil:
		return fmt.Errorf("%w: cannot scan NULL into Quantity (use sql.Null[Quantity])", ErrInvalidFormat)
	default:
		return fmt.Errorf("%w: cannot scan %T into Quantity (expected NUMERIC text, []byte or int64)", ErrInvalidFormat, src)
	}
}

// ScanQuantity parses the text form of a Postgres NUMERIC value into a
// Quantity. Integers ("123", "-5") are accepted; a fractional part is
// accepted only when it is all zeros ("123.000"), because a Quantity is a
// whole number of base units. "123.5" fails with ErrPrecisionLoss; NaN,
// Infinity and anything else non-numeric fail with ErrInvalidFormat.
func ScanQuantity(s string) (Quantity, error) {
	return QuantityFromDecimalString(s, 0, RoundExact)
}

// jsonString extracts the string from a JSON string token, rejecting
// numbers, null and every other token with ErrInvalidFormat.
func jsonString(b []byte, typeName, example string) (string, error) {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || t[0] != '"' {
		if bytes.Equal(t, []byte("null")) {
			return "", fmt.Errorf("%w: %s cannot be JSON null; use a pointer for optional amounts", ErrInvalidFormat, typeName)
		}
		return "", fmt.Errorf("%w: %s must be a JSON string such as %s, never a JSON number", ErrInvalidFormat, typeName, example)
	}
	var s string
	if err := json.Unmarshal(t, &s); err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrInvalidFormat, typeName, err)
	}
	return s, nil
}
