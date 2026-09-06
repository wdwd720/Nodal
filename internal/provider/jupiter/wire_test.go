package jupiter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

const validOrderJSON = `{
  "mode":"manual","router":"metis","requestId":"req-123",
  "inAmount":"1000000","outAmount":"6500000","otherAmountThreshold":"6467500",
  "inUsdValue":1.0,"outUsdValue":0.99,"swapUsdValue":0.99,
  "priceImpact":0.0123,"priceImpactPct":"0.000123",
  "slippageBps":50,"feeBps":10,
  "platformFee":{"amount":"650","feeBps":10,"feeMint":"So11111111111111111111111111111111111111112"},
  "routePlan":[{"swapInfo":{"ammKey":"amm1","label":"Orca","inputMint":"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU","outputMint":"So11111111111111111111111111111111111111112","inAmount":"1000000","outAmount":"6500000"},"percent":100,"bps":10000,"usdValue":1.0}],
  "transaction":null,"lastValidBlockHeight":"250000150",
  "signatureFeeLamports":5000,"prioritizationFeeLamports":12345,"rentFeeLamports":0,"gasless":false,
  "totalTime":42,"errorCode":null,"errorMessage":null,"error":null
}`

func TestDecodeOrderResponse_Valid(t *testing.T) {
	t.Parallel()
	d, err := decodeOrderResponse([]byte(validOrderJSON))
	require.NoError(t, err)
	require.Equal(t, "req-123", d.requestID)
	require.Equal(t, "1000000", d.inAmount.String())
	require.Equal(t, "6500000", d.outAmount.String())
	require.Equal(t, "6467500", d.otherAmountThreshold.String())
	require.Equal(t, money.BPS(50), d.slippageBPS)
	require.Equal(t, money.BPS(10), d.feeBPS)
	require.NotNil(t, d.platformFee)
	require.Equal(t, "650", d.platformFee.Amount.String())
	require.True(t, d.priceImpactOK)
	require.Equal(t, money.BPS(2), d.priceImpactBPS, "0.0123 percentage points = 1.23 bps, RoundCeil -> 2")
	require.Len(t, d.route, 1)
	require.Equal(t, int64(10000), d.route[0].BPS)
	require.False(t, d.hasTransaction)
	require.Equal(t, uint64(250000150), d.lastValidBlockHeight)
	require.Equal(t, "250000150", d.lastValidBlockHeightRaw)
	require.Equal(t, "12345", d.prioritizationFeeLamports.String())
	require.Equal(t, "5000", d.signatureFeeLamports.String())
	require.NotEmpty(t, d.routeHash)
	require.JSONEq(t, `[{"bps":10000,"percent":100,"swapInfo":{"ammKey":"amm1","inAmount":"1000000","inputMint":"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU","label":"Orca","outAmount":"6500000","outputMint":"So11111111111111111111111111111111111111112"},"usdValue":1.0}]`, string(d.routeSummary))
}

func mutate(t *testing.T, base string, edit func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(base))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&m))
	edit(m)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

func TestDecodeOrderResponse_Rejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		edit  func(m map[string]any)
		field string
	}{
		{"float inAmount number", func(m map[string]any) { m["inAmount"] = json.Number("1000000.0") }, "inAmount"},
		{"numeric outAmount", func(m map[string]any) { m["outAmount"] = json.Number("6500000") }, "outAmount"},
		{"fractional string threshold", func(m map[string]any) { m["otherAmountThreshold"] = "6467500.5" }, "otherAmountThreshold"},
		{"exponent string amount", func(m map[string]any) { m["inAmount"] = "1e6" }, "inAmount"},
		{"missing otherAmountThreshold", func(m map[string]any) { delete(m, "otherAmountThreshold") }, "otherAmountThreshold"},
		{"missing requestId", func(m map[string]any) { delete(m, "requestId") }, "requestId"},
		{"missing slippageBps", func(m map[string]any) { delete(m, "slippageBps") }, "slippageBps"},
		{"float slippageBps", func(m map[string]any) { m["slippageBps"] = json.Number("50.5") }, "slippageBps"},
		{"slippage out of range", func(m map[string]any) { m["slippageBps"] = json.Number("20000") }, "slippageBps"},
		{"float fee lamports", func(m map[string]any) { m["prioritizationFeeLamports"] = json.Number("12345.6") }, "prioritizationFeeLamports"},
		{"string fee lamports", func(m map[string]any) { m["signatureFeeLamports"] = "5000" }, "signatureFeeLamports"},
		{"lastValidBlockHeight number with tx", func(m map[string]any) {
			m["transaction"] = "AQ=="
			m["lastValidBlockHeight"] = json.Number("250000150")
		}, "lastValidBlockHeight"},
		{"missing lastValidBlockHeight with tx", func(m map[string]any) {
			m["transaction"] = "AQ=="
			delete(m, "lastValidBlockHeight")
		}, "lastValidBlockHeight"},
		{"route amount float", func(m map[string]any) {
			obj(m, "routePlan", 0, "swapInfo")["inAmount"] = json.Number("1000000.0")
		}, "routePlan[0].swapInfo.inAmount"},
		{"platform fee amount number", func(m map[string]any) {
			obj(m, "platformFee")["amount"] = json.Number("650")
		}, "platformFee.amount"},
		{"bad base64 transaction", func(m map[string]any) { m["transaction"] = "not*base64" }, "transaction"},
		{"gasless not bool", func(m map[string]any) { m["gasless"] = "yes" }, "gasless"},
		{"expireAt garbage", func(m map[string]any) { m["expireAt"] = "tomorrow" }, "expireAt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeOrderResponse(mutate(t, validOrderJSON, tc.edit))
			require.Error(t, err)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			e, ok := errs.As(err)
			require.True(t, ok)
			require.Equal(t, tc.field, e.Fields["field"], "error must name the field: %v", err)
		})
	}
}

func TestDecodeOrderResponse_ErrorCodeShortCircuits(t *testing.T) {
	t.Parallel()
	body := []byte(`{"requestId":"r1","router":"metis","errorCode":2,"errorMessage":"insufficient SOL","transaction":""}`)
	d, err := decodeOrderResponse(body)
	require.NoError(t, err)
	require.Equal(t, int64(2), d.errorCode)
	require.Equal(t, "insufficient SOL", d.errorMessage)
	require.Equal(t, "r1", d.requestID)
}

func TestDecodeOrderResponse_NotObject(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"", "[]", "null", "42", `"x"`, "{"} {
		_, err := decodeOrderResponse([]byte(body))
		if body == "null" {
			// json.Unmarshal of null into a struct is a no-op; requestId is then missing.
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			continue
		}
		require.Error(t, err, body)
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), body)
	}
}

func TestPriceImpactDerivation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw   string
		bps   money.BPS
		ok    bool
		isErr bool
	}{
		{`0.05`, 5, true, false},
		{`0.0123`, 2, true, false},
		{`1`, 100, true, false},
		{`0`, 0, true, false},
		{`-0.01`, -1, true, false},
		{`-0.011`, -1, true, false}, // ceil toward +inf: -1.1 -> -1
		{`1e-7`, 0, false, false},   // exponent: unavailable, never guessed
		{`"0.05"`, 0, false, true},  // string where number documented
		{`null`, 0, false, false},
	}
	for _, tc := range cases {
		bps, ok, err := priceImpactFromNumber(json.RawMessage(tc.raw), "priceImpact")
		if tc.isErr {
			require.Error(t, err, tc.raw)
			continue
		}
		require.NoError(t, err, tc.raw)
		require.Equal(t, tc.ok, ok, tc.raw)
		require.Equal(t, tc.bps, bps, tc.raw)
	}
	// /build fraction string: "0.001" = 0.1% = 10 bps.
	bps, ok, err := priceImpactFromFractionString(json.RawMessage(`"0.001"`), "priceImpactPct")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, money.BPS(10), bps)
	bps, ok, err = priceImpactFromFractionString(json.RawMessage(`"0.00015"`), "priceImpactPct")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, money.BPS(2), bps, "1.5 bps rounds up")
}

func TestParseExpireAt(t *testing.T) {
	t.Parallel()
	ts, err := parseExpireAt("1760000000")
	require.NoError(t, err)
	require.Equal(t, time.Unix(1760000000, 0).UTC(), ts)
	ts, err = parseExpireAt("1760000000123")
	require.NoError(t, err)
	require.Equal(t, time.UnixMilli(1760000000123).UTC(), ts)
	ts, err = parseExpireAt("2026-09-05T12:00:00Z")
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC), ts)
	ts, err = parseExpireAt("")
	require.NoError(t, err)
	require.True(t, ts.IsZero())
	_, err = parseExpireAt("soon")
	require.Error(t, err)
}

func TestDecodeExecuteResponse(t *testing.T) {
	t.Parallel()
	d, err := decodeExecuteResponse([]byte(`{"status":"Success","signature":"5x","slot":"123","code":0,"error":null,"totalInputAmount":"1000000","totalOutputAmount":"6500000","inputAmountResult":"1000000","outputAmountResult":"6510000","swapEvents":[{"inputMint":"a","inputAmount":"1000000","outputMint":"b","outputAmount":"6510000"}]}`))
	require.NoError(t, err)
	require.Equal(t, "Success", d.status)
	require.Equal(t, uint64(123), d.slot)
	require.Equal(t, "6510000", d.outputAmountResult.String())
	require.Len(t, d.swapEvents, 1)

	_, err = decodeExecuteResponse([]byte(`{"status":"Success","signature":"5x","slot":123}`))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "slot must be a string")
	_, err = decodeExecuteResponse([]byte(`{"signature":"5x"}`))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "status required")
	_, err = decodeExecuteResponse([]byte(`{"status":"Success","totalInputAmount":1000000.0}`))
	e, ok := errs.As(err)
	require.True(t, ok)
	require.Equal(t, "totalInputAmount", e.Fields["field"])
}

const validBuildJSON = `{
  "inAmount":"1000000","outAmount":"6500000","otherAmountThreshold":"6467500","slippageBps":50,"priceImpactPct":"0.0001",
  "routePlan":[{"swapInfo":{"ammKey":"amm1","label":"Orca","inputMint":"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU","outputMint":"So11111111111111111111111111111111111111112","inAmount":"1000000","outAmount":"6500000"},"percent":100,"bps":10000}],
  "computeBudgetInstructions":[{"programId":"ComputeBudget111111111111111111111111111111","accounts":[],"data":"AsBcFQA="}],
  "setupInstructions":[],
  "swapInstruction":{"programId":"JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4","accounts":[{"pubkey":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","isSigner":false,"isWritable":false}],"data":"5RfLl3rjrSo="},
  "cleanupInstruction":null,"otherInstructions":[],"tipInstruction":null,
  "addressesByLookupTableAddress":{"AddressLookupTab1e1111111111111111111111111":["11111111111111111111111111111111"]},
  "blockhashWithMetadata":{"blockhash":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32],"lastValidBlockHeight":250000150,"fetchedAt":{"secs_since_epoch":1760000000,"nanos_since_epoch":5}}
}`

func TestDecodeBuildResponse(t *testing.T) {
	t.Parallel()
	d, err := decodeBuildResponse([]byte(validBuildJSON))
	require.NoError(t, err)
	require.Equal(t, money.BPS(1), d.priceImpactBPS)
	require.True(t, d.priceImpactOK)
	require.Len(t, d.computeBudget, 1)
	require.Equal(t, []byte{2, 0xc0, 0x5c, 0x15, 0}, d.computeBudget[0].Data)
	require.Equal(t, "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4", d.swap.ProgramID)
	require.Nil(t, d.cleanup)
	require.Equal(t, uint64(250000150), d.lastValidBlockHeight)
	require.Equal(t, time.Unix(1760000000, 5).UTC(), d.fetchedAt)
	require.NotEmpty(t, d.blockhash)
	require.Len(t, d.lookupTables, 1)

	_, err = decodeBuildResponse(mutate(t, validBuildJSON, func(m map[string]any) { delete(m, "swapInstruction") }))
	e, ok := errs.As(err)
	require.True(t, ok)
	require.Equal(t, "swapInstruction", e.Fields["field"])
	_, err = decodeBuildResponse(mutate(t, validBuildJSON, func(m map[string]any) { m["inAmount"] = json.Number("1000000.5") }))
	e, ok = errs.As(err)
	require.True(t, ok)
	require.Equal(t, "inAmount", e.Fields["field"])
}

func TestDecodeErrorBody(t *testing.T) {
	t.Parallel()
	eb := decodeErrorBody([]byte(`{"requestId":"r","error":"No route found"}`))
	require.Equal(t, "r", eb.requestID)
	require.Equal(t, "No route found", eb.message)
	eb = decodeErrorBody([]byte(`{"error":"missing cached order","code":-1}`))
	require.NotNil(t, eb.code)
	require.Equal(t, int64(-1), *eb.code)
	eb = decodeErrorBody([]byte(`[API Gateway] Too many requests`))
	require.Equal(t, "[API Gateway] Too many requests", eb.raw)
	require.Empty(t, eb.message)
}

// TestProp_AmountsDecodeExactly: any integer amount rendered as a JSON
// string decodes to the identical Quantity; rendered as a JSON number it is
// rejected naming the field.
func TestProp_AmountsDecodeExactly(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		digits := rapid.StringMatching(`[1-9][0-9]{0,60}`).Draw(rt, "inAmount")
		outDigits := rapid.StringMatching(`[1-9][0-9]{0,60}`).Draw(rt, "outAmount")
		body := mutate(t, validOrderJSON, func(m map[string]any) {
			m["inAmount"] = digits
			m["outAmount"] = outDigits
			m["otherAmountThreshold"] = outDigits
		})
		d, err := decodeOrderResponse(body)
		if err != nil {
			rt.Fatalf("decode: %v", err)
		}
		if d.inAmount.String() != digits || d.outAmount.String() != outDigits {
			rt.Fatalf("round trip mismatch: %s / %s", d.inAmount, d.outAmount)
		}
		numeric := mutate(t, validOrderJSON, func(m map[string]any) { m["inAmount"] = json.Number(digits) })
		_, err = decodeOrderResponse(numeric)
		e, ok := errs.As(err)
		if !ok || e.Code != errs.CodeValidationFailed || e.Fields["field"] != "inAmount" {
			rt.Fatalf("numeric amount must be rejected naming inAmount, got %v", err)
		}
	})
}

func FuzzDecodeOrderResponse(f *testing.F) {
	f.Add([]byte(validOrderJSON))
	f.Add([]byte(`{"requestId":"r","inAmount":1.5}`))
	f.Add([]byte(`{"requestId":"r","inAmount":"1","outAmount":"1","otherAmountThreshold":"2","slippageBps":50,"transaction":"AQID","lastValidBlockHeight":"1"}`))
	f.Add([]byte(`{"errorCode":3,"error":"x"}`))
	f.Add([]byte(`{"routePlan":[{"swapInfo":null}]}`))
	f.Add([]byte(`{"priceImpact":1e400}`))
	f.Add([]byte(`{"expireAt":"99999999999999999999"}`))
	f.Add([]byte("{"))
	f.Add([]byte(strings.Repeat("[", 5000)))
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := decodeOrderResponse(data)
		if err != nil {
			if errs.CodeOf(err) != errs.CodeValidationFailed {
				t.Fatalf("decode error must be VALIDATION_FAILED, got %v", err)
			}
			return
		}
		if d.errorCode == 0 && d.requestID == "" {
			t.Fatal("successful decode without requestId")
		}
		if d.hasTransaction && (len(d.transaction) == 0 || d.lastValidBlockHeight == 0) {
			t.Fatal("transaction without bytes or expiry")
		}
	})
}

func FuzzDecodeExecuteResponse(f *testing.F) {
	f.Add([]byte(`{"status":"Success","signature":"5x","slot":"1"}`))
	f.Add([]byte(`{"status":"Failed","code":-1001,"error":"slippage"}`))
	f.Add([]byte(`{"status":1}`))
	f.Add([]byte(`{"swapEvents":[{}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := decodeExecuteResponse(data)
		if err != nil {
			if errs.CodeOf(err) != errs.CodeValidationFailed {
				t.Fatalf("decode error must be VALIDATION_FAILED, got %v", err)
			}
			return
		}
		if d.status == "" {
			t.Fatal("successful decode without status")
		}
	})
}

func FuzzDecodeBuildResponse(f *testing.F) {
	f.Add([]byte(validBuildJSON))
	f.Add([]byte(`{"blockhashWithMetadata":{"blockhash":[300]}}`))
	f.Add([]byte(`{"swapInstruction":{"programId":1}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, err := decodeBuildResponse(data)
		if err != nil && errs.CodeOf(err) != errs.CodeValidationFailed {
			t.Fatalf("decode error must be VALIDATION_FAILED, got %v", err)
		}
	})
}

// obj walks a decoded JSON value by string keys and integer indices and
// returns the object at the end, failing loudly on a shape mismatch.
func obj(v any, path ...any) map[string]any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			mm, ok := v.(map[string]any)
			if !ok {
				panic("obj: expected object at " + k)
			}
			v = mm[k]
		case int:
			arr, ok := v.([]any)
			if !ok {
				panic("obj: expected array")
			}
			v = arr[k]
		}
	}
	out, ok := v.(map[string]any)
	if !ok {
		panic("obj: expected object at end of path")
	}
	return out
}
