package money

import (
	"database/sql"
	"database/sql/driver"
	"encoding"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Compile-time interface checks.
var (
	_ json.Marshaler           = USD{}
	_ json.Unmarshaler         = (*USD)(nil)
	_ encoding.TextMarshaler   = USD{}
	_ encoding.TextUnmarshaler = (*USD)(nil)
	_ driver.Valuer            = USD{}
	_ sql.Scanner              = (*USD)(nil)
	_ fmt.Stringer             = USD{}

	_ json.Marshaler           = Quantity{}
	_ json.Unmarshaler         = (*Quantity)(nil)
	_ encoding.TextMarshaler   = Quantity{}
	_ encoding.TextUnmarshaler = (*Quantity)(nil)
	_ driver.Valuer            = Quantity{}
	_ sql.Scanner              = (*Quantity)(nil)
	_ fmt.Stringer             = Quantity{}
)

type usdDoc struct {
	Amount   USD  `json:"amount"`
	Optional *USD `json:"optional,omitempty"`
}

type qtyDoc struct {
	Qty      Quantity  `json:"qty"`
	Optional *Quantity `json:"optional,omitempty"`
}

func TestUSD_JSON_Marshal(t *testing.T) {
	cases := map[int64]string{0: `"0.00"`, 1: `"0.01"`, -1: `"-0.01"`, 123456: `"1234.56"`, math.MaxInt64: `"92233720368547758.07"`, math.MinInt64: `"-92233720368547758.08"`}
	for minor, want := range cases {
		b, err := json.Marshal(USDFromMinor(minor))
		require.NoError(t, err)
		require.Equal(t, want, string(b))
	}
	b, err := json.Marshal(usdDoc{Amount: USDFromMinor(150)})
	require.NoError(t, err)
	require.JSONEq(t, `{"amount":"1.50"}`, string(b))

	// Map keys go through MarshalText.
	b, err = json.Marshal(map[USD]int{USDFromMinor(100): 1})
	require.NoError(t, err)
	require.JSONEq(t, `{"1.00":1}`, string(b))
}

func TestUSD_JSON_Unmarshal(t *testing.T) {
	var d usdDoc
	require.NoError(t, json.Unmarshal([]byte(`{"amount":"1234.56"}`), &d))
	require.Equal(t, int64(123456), d.Amount.Minor())
	require.Nil(t, d.Optional)

	require.NoError(t, json.Unmarshal([]byte(`{"amount":"-0.01","optional":"2.00"}`), &d))
	require.Equal(t, int64(-1), d.Amount.Minor())
	require.NotNil(t, d.Optional)
	require.Equal(t, int64(200), d.Optional.Minor())

	// null into a pointer clears it without calling UnmarshalJSON.
	require.NoError(t, json.Unmarshal([]byte(`{"amount":"0.05","optional":null}`), &d))
	require.Nil(t, d.Optional)

	rejected := []struct {
		body string
		want error
	}{
		{`{"amount":1234.56}`, ErrInvalidFormat},
		{`{"amount":1234}`, ErrInvalidFormat},
		{`{"amount":0}`, ErrInvalidFormat},
		{`{"amount":-1}`, ErrInvalidFormat},
		{`{"amount":1e3}`, ErrInvalidFormat},
		{`{"amount":null}`, ErrInvalidFormat},
		{`{"amount":true}`, ErrInvalidFormat},
		{`{"amount":[]}`, ErrInvalidFormat},
		{`{"amount":{}}`, ErrInvalidFormat},
		{`{"amount":""}`, ErrInvalidFormat},
		{`{"amount":"abc"}`, ErrInvalidFormat},
		{`{"amount":" 1.00"}`, ErrInvalidFormat},
		{`{"amount":"+1.00"}`, ErrInvalidFormat},
		{`{"amount":"1.234"}`, ErrPrecisionLoss},
		{`{"amount":"92233720368547758.08"}`, ErrOverflow},
		{`{"optional":1}`, ErrInvalidFormat},
	}
	for _, tc := range rejected {
		t.Run(tc.body, func(t *testing.T) {
			var d usdDoc
			err := json.Unmarshal([]byte(tc.body), &d)
			require.ErrorIs(t, err, tc.want)
		})
	}

	var u USD
	require.ErrorIs(t, u.UnmarshalJSON([]byte(`null`)), ErrInvalidFormat)
	require.ErrorIs(t, u.UnmarshalJSON([]byte(``)), ErrInvalidFormat)
	require.ErrorIs(t, u.UnmarshalJSON([]byte(`"1.00`)), ErrInvalidFormat)
	require.NoError(t, u.UnmarshalJSON([]byte(` "1.00" `)))
	require.Equal(t, int64(100), u.Minor())
}

func TestUSD_JSON_RoundTrip(t *testing.T) {
	for _, minor := range []int64{0, 1, -1, 123456, math.MaxInt64, math.MinInt64} {
		in := usdDoc{Amount: USDFromMinor(minor)}
		b, err := json.Marshal(in)
		require.NoError(t, err)
		var out usdDoc
		require.NoError(t, json.Unmarshal(b, &out))
		require.Equal(t, in.Amount, out.Amount)
	}
}

func TestUSD_Text(t *testing.T) {
	b, err := USDFromMinor(-1).MarshalText()
	require.NoError(t, err)
	require.Equal(t, "-0.01", string(b))
	var u USD
	require.NoError(t, u.UnmarshalText([]byte("1234.56")))
	require.Equal(t, int64(123456), u.Minor())
	require.ErrorIs(t, u.UnmarshalText([]byte("1.234")), ErrPrecisionLoss)
	require.ErrorIs(t, u.UnmarshalText([]byte("x")), ErrInvalidFormat)
}

func TestUSD_SQL(t *testing.T) {
	v, err := USDFromMinor(123456).Value()
	require.NoError(t, err)
	require.Equal(t, int64(123456), v)
	v, err = MinUSD().Value()
	require.NoError(t, err)
	require.Equal(t, int64(math.MinInt64), v)

	var u USD
	require.NoError(t, u.Scan(int64(-1)))
	require.Equal(t, "-0.01", u.String())
	require.NoError(t, u.Scan(int64(math.MaxInt64)))
	require.Equal(t, MaxUSD(), u)

	for _, src := range []any{nil, "100", []byte("100"), int32(1), uint64(1), int(1), true, 1.5} {
		require.ErrorIs(t, u.Scan(src), ErrInvalidFormat, "%T", src)
	}
	require.Equal(t, MaxUSD(), u, "a failed scan leaves the value untouched")

	var n sql.Null[USD]
	require.NoError(t, n.Scan(nil))
	require.False(t, n.Valid)
	require.NoError(t, n.Scan(int64(5)))
	require.True(t, n.Valid)
	require.Equal(t, "0.05", n.V.String())
	nv, err := n.Value()
	require.NoError(t, err)
	require.Equal(t, int64(5), nv)
}

func TestQuantity_JSON_Marshal(t *testing.T) {
	cases := map[string]string{"0": `"0"`, "-1": `"-1"`, "1500000000": `"1500000000"`, uint256Max: `"` + uint256Max + `"`}
	for in, want := range cases {
		b, err := json.Marshal(mustQ(t, in))
		require.NoError(t, err)
		require.Equal(t, want, string(b))
	}
	var zero Quantity
	b, err := json.Marshal(qtyDoc{Qty: zero})
	require.NoError(t, err)
	require.JSONEq(t, `{"qty":"0"}`, string(b))
	b, err = json.Marshal(map[Quantity]int{q64(7): 1})
	require.NoError(t, err)
	require.JSONEq(t, `{"7":1}`, string(b))
}

func TestQuantity_JSON_Unmarshal(t *testing.T) {
	var d qtyDoc
	require.NoError(t, json.Unmarshal([]byte(`{"qty":"1500000000","optional":"-5"}`), &d))
	require.Equal(t, "1500000000", d.Qty.String())
	require.NotNil(t, d.Optional)
	require.Equal(t, "-5", d.Optional.String())
	require.NoError(t, json.Unmarshal([]byte(`{"qty":"`+uint256Max+`","optional":null}`), &d))
	require.Equal(t, uint256Max, d.Qty.String())
	require.Nil(t, d.Optional)

	rejected := []struct {
		body string
		want error
	}{
		{`{"qty":1500000000}`, ErrInvalidFormat},
		{`{"qty":1.5}`, ErrInvalidFormat},
		{`{"qty":0}`, ErrInvalidFormat},
		{`{"qty":null}`, ErrInvalidFormat},
		{`{"qty":true}`, ErrInvalidFormat},
		{`{"qty":""}`, ErrInvalidFormat},
		{`{"qty":"1.5"}`, ErrInvalidFormat},
		{`{"qty":"1e3"}`, ErrInvalidFormat},
		{`{"qty":"+1"}`, ErrInvalidFormat},
		{`{"qty":" 1"}`, ErrInvalidFormat},
		{`{"qty":"1` + strings.Repeat("0", 128) + `"}`, ErrOverflow},
	}
	for _, tc := range rejected {
		t.Run(tc.body[:min(len(tc.body), 40)], func(t *testing.T) {
			var d qtyDoc
			require.ErrorIs(t, json.Unmarshal([]byte(tc.body), &d), tc.want)
		})
	}
}

func TestQuantity_JSON_RoundTrip(t *testing.T) {
	for _, s := range []string{"0", "1", "-1", "9223372036854775808", uint256Max, "-" + uint256Max} {
		in := qtyDoc{Qty: mustQ(t, s)}
		b, err := json.Marshal(in)
		require.NoError(t, err)
		var out qtyDoc
		require.NoError(t, json.Unmarshal(b, &out))
		require.True(t, out.Qty.Equal(in.Qty))
	}
}

func TestQuantity_Text(t *testing.T) {
	b, err := q64(-42).MarshalText()
	require.NoError(t, err)
	require.Equal(t, "-42", string(b))
	var q Quantity
	require.NoError(t, q.UnmarshalText([]byte(uint256Max)))
	require.Equal(t, uint256Max, q.String())
	require.ErrorIs(t, q.UnmarshalText([]byte("1.0")), ErrInvalidFormat)
}

func TestQuantity_SQL(t *testing.T) {
	v, err := mustQ(t, uint256Max).Value()
	require.NoError(t, err)
	require.Equal(t, uint256Max, v)
	var zero Quantity
	v, err = zero.Value()
	require.NoError(t, err)
	require.Equal(t, "0", v)

	var q Quantity
	require.NoError(t, q.Scan("1500000000"))
	require.Equal(t, "1500000000", q.String())
	require.NoError(t, q.Scan([]byte("-5")))
	require.Equal(t, "-5", q.String())
	require.NoError(t, q.Scan(int64(math.MinInt64)))
	require.Equal(t, "-9223372036854775808", q.String())
	require.NoError(t, q.Scan(uint256Max))
	require.Equal(t, uint256Max, q.String())
	require.NoError(t, q.Scan("123.000"), "NUMERIC text with an all-zero fraction is exact")
	require.Equal(t, "123", q.String())
	require.NoError(t, q.Scan("-0"))
	require.True(t, q.IsZero())

	before := q
	require.ErrorIs(t, q.Scan("123.5"), ErrPrecisionLoss)
	require.ErrorIs(t, q.Scan("NaN"), ErrInvalidFormat)
	require.ErrorIs(t, q.Scan("Infinity"), ErrInvalidFormat)
	require.ErrorIs(t, q.Scan("1e3"), ErrInvalidFormat)
	require.ErrorIs(t, q.Scan(""), ErrInvalidFormat)
	require.ErrorIs(t, q.Scan(nil), ErrInvalidFormat)
	require.ErrorIs(t, q.Scan(int32(1)), ErrInvalidFormat)
	require.ErrorIs(t, q.Scan(1.5), ErrInvalidFormat)
	require.ErrorIs(t, q.Scan(true), ErrInvalidFormat)
	require.True(t, q.Equal(before), "a failed scan leaves the value untouched")

	var n sql.Null[Quantity]
	require.NoError(t, n.Scan(nil))
	require.False(t, n.Valid)
	require.NoError(t, n.Scan("77"))
	require.True(t, n.Valid)
	require.Equal(t, "77", n.V.String())
}

func TestScanQuantity(t *testing.T) {
	for in, want := range map[string]string{"0": "0", "-5": "-5", "123.000": "123", "007": "7", uint256Max: uint256Max} {
		got, err := ScanQuantity(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got.String(), in)
	}
	_, err := ScanQuantity("1.5")
	require.ErrorIs(t, err, ErrPrecisionLoss)
	_, err = ScanQuantity("abc")
	require.ErrorIs(t, err, ErrInvalidFormat)
}
