package predmarket

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
)

func seed(b byte) []byte { return bytes.Repeat([]byte{b}, ed25519.SeedSize) }

func sandboxOracle(t testing.TB, b byte) *SandboxOracle {
	o, err := NewSandboxOracle("sandbox-oracle", seed(b))
	require.NoError(t, err)
	return o
}

var observed = time.Date(2026, 11, 3, 23, 59, 0, 0, time.UTC)

func TestSandboxOracle_ReportsAreDeterministicAndVerify(t *testing.T) {
	ctx := context.Background()
	o := sandboxOracle(t, 7)
	mid := NewMarketID()

	_, err := o.Report(ctx, mid)
	require.Error(t, err, "no observation yet")
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))

	require.NoError(t, o.Observe(mid, "YES", observed, []byte(`{"source":"election board","winner":"YES"}`)))
	a, err := o.Report(ctx, mid)
	require.NoError(t, err)
	b, err := o.Report(ctx, mid)
	require.NoError(t, err)
	assert.Equal(t, a, b, "the same observation is the same report, byte for byte")
	assert.Equal(t, a.Hash(), b.Hash())
	require.NoError(t, VerifyReport(o.PublicKey(), a))

	// A second oracle built from the same seed is the same oracle.
	again := sandboxOracle(t, 7)
	require.NoError(t, again.Observe(mid, "YES", observed, []byte(`{"source":"election board","winner":"YES"}`)))
	c, err := again.Report(ctx, mid)
	require.NoError(t, err)
	assert.Equal(t, a.Signature, c.Signature)

	_, err = NewSandboxOracle("x", []byte("short"))
	require.Error(t, err)
	require.Error(t, o.Observe(mid, "yes!", observed, []byte("e")), "an outcome is a symbol")
	require.Error(t, o.Observe(mid, "YES", observed, nil), "a report carries evidence")
}

func TestVerifyReport_RefusesEveryTampering(t *testing.T) {
	o := sandboxOracle(t, 1)
	other := sandboxOracle(t, 2)
	good, err := o.Sign(OracleReport{MarketID: NewMarketID(), Outcome: "YES", ObservedAt: observed, Evidence: []byte("doc")})
	require.NoError(t, err)
	require.NoError(t, VerifyReport(o.PublicKey(), good))

	tamper := map[string]func(r *OracleReport){
		"outcome":     func(r *OracleReport) { r.Outcome = "NO" },
		"market":      func(r *OracleReport) { r.MarketID = NewMarketID() },
		"source":      func(r *OracleReport) { r.Source = "another-source" },
		"time":        func(r *OracleReport) { r.ObservedAt = r.ObservedAt.Add(time.Second) },
		"evidence":    func(r *OracleReport) { r.Evidence = []byte("forged doc") },
		"signature":   func(r *OracleReport) { r.Signature = append([]byte(nil), r.Signature...); r.Signature[0] ^= 1 },
		"truncated":   func(r *OracleReport) { r.Signature = r.Signature[:63] },
		"sub-micro":   func(r *OracleReport) { r.ObservedAt = r.ObservedAt.Add(time.Nanosecond) },
		"no evidence": func(r *OracleReport) { r.Evidence = nil },
	}
	for name, mutate := range tamper {
		t.Run(name, func(t *testing.T) {
			r := good
			r.Evidence = append([]byte(nil), good.Evidence...)
			mutate(&r)
			require.Error(t, VerifyReport(o.PublicKey(), r))
		})
	}
	err = VerifyReport(other.PublicKey(), good)
	require.Error(t, err, "another oracle's key")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

func TestDecodeOracleReport_RoundTripsAndIsStrict(t *testing.T) {
	o := sandboxOracle(t, 3)
	r, err := o.Sign(OracleReport{MarketID: NewMarketID(), Outcome: "B_WINS", ObservedAt: observed, Evidence: []byte{0, 1, 2, 255}})
	require.NoError(t, err)
	body, err := EncodeOracleReport(r)
	require.NoError(t, err)
	back, err := DecodeOracleReport(body)
	require.NoError(t, err)
	assert.Equal(t, r.Hash(), back.Hash())
	require.NoError(t, VerifyReport(o.PublicKey(), back))

	for name, raw := range map[string]string{
		"empty":         ``,
		"array":         `[]`,
		"unknown field": `{"market_id":"x","extra":1}`,
		"trailing":      string(body) + `{}`,
		"missing":       `{"market_id":"` + r.MarketID.String() + `"}`,
		"number":        `{"market_id":1}`,
	} {
		_, err := DecodeOracleReport([]byte(raw))
		require.Error(t, err, name)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
	}
}

// FuzzDecodeOracleReport: the decoder never panics, accepts only well-formed
// reports, and whatever it accepts re-encodes to a report with the same hash.
func FuzzDecodeOracleReport(f *testing.F) {
	o, err := NewSandboxOracle("fuzz-oracle", seed(9))
	if err != nil {
		f.Fatal(err)
	}
	r, err := o.Sign(OracleReport{MarketID: NewMarketID(), Outcome: "YES", ObservedAt: observed, Evidence: []byte("e")})
	if err != nil {
		f.Fatal(err)
	}
	body, err := EncodeOracleReport(r)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(body)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"market_id":"","outcome":"","source":"","observed_at":"","evidence":"","signature":""}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := DecodeOracleReport(b)
		if err != nil {
			if errs.CodeOf(err) != errs.CodeValidationFailed {
				t.Fatalf("decode failure must be VALIDATION_FAILED, got %v", err)
			}
			return
		}
		if err := got.validateShape(); err != nil {
			t.Fatalf("decoded an ill-formed report: %v", err)
		}
		enc, err := EncodeOracleReport(got)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		again, err := DecodeOracleReport(enc)
		if err != nil {
			t.Fatalf("re-decode: %v", err)
		}
		if !bytes.Equal(got.Hash(), again.Hash()) {
			t.Fatalf("round trip changed the report")
		}
	})
}

// TestProp_ReportIdentityIsDeterministic: a report's hash depends only on its
// content, and any change to what was signed changes it.
func TestProp_ReportIdentityIsDeterministic(t *testing.T) {
	o := sandboxOracle(t, 5)
	rapid.Check(t, func(rt *rapid.T) {
		mid := NewMarketID()
		outcome := rapid.SampledFrom([]string{"YES", "NO", "A", "B_2"}).Draw(rt, "outcome")
		ev := rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(rt, "evidence")
		at := observed.Add(time.Duration(rapid.Int64Range(0, 1_000_000).Draw(rt, "us")) * time.Microsecond)
		a, err := o.Sign(OracleReport{MarketID: mid, Outcome: outcome, ObservedAt: at, Evidence: ev})
		if err != nil {
			rt.Fatalf("sign: %v", err)
		}
		b, err := o.Sign(OracleReport{MarketID: mid, Outcome: outcome, ObservedAt: at, Evidence: append([]byte(nil), ev...)})
		if err != nil {
			rt.Fatalf("sign: %v", err)
		}
		if !bytes.Equal(a.Hash(), b.Hash()) {
			rt.Fatalf("same report, different hash")
		}
		c, err := o.Sign(OracleReport{MarketID: mid, Outcome: outcome, ObservedAt: at.Add(time.Microsecond), Evidence: ev})
		if err != nil {
			rt.Fatalf("sign: %v", err)
		}
		if bytes.Equal(a.Hash(), c.Hash()) {
			rt.Fatalf("a different observation time kept the hash")
		}
	})
}
