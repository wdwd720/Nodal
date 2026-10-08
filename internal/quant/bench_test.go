package quant

import (
	"testing"
)

func benchInput(b *testing.B, kind OptionKind, s, k string) BSMInput {
	return goldenInput(b, kind, s, k, "0.5", "0.05", "0.01", "0.25")
}

func BenchmarkBlackScholesPrice(b *testing.B) {
	in := benchInput(b, Call, "100", "105")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := BlackScholesPrice(in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBlackScholesGreeks(b *testing.B) {
	in := benchInput(b, Put, "100", "105")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := BlackScholes(in); err != nil {
			b.Fatal(err)
		}
	}
}

func benchImpliedVol(b *testing.B, in BSMInput) {
	price, err := BlackScholesPrice(in)
	if err != nil {
		b.Fatal(err)
	}
	iv := ivInput(in, price)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ImpliedVol(iv); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkImpliedVol is a near-the-money option (5 safeguarded Newton
// iterations); BenchmarkImpliedVolDeepOTM a 25-delta-away wing where the
// flat tail slows Newton and bisection steps in.
func BenchmarkImpliedVol(b *testing.B) { benchImpliedVol(b, benchInput(b, Call, "100", "105")) }

func BenchmarkImpliedVolDeepOTM(b *testing.B) { benchImpliedVol(b, benchInput(b, Call, "100", "160")) }

func BenchmarkExp(b *testing.B) {
	x := d(b, "-1.2345678901234567890123456789")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Exp(x); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLn(b *testing.B) {
	x := d(b, "1.2345678901234567890123456789")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Ln(x); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSqrt(b *testing.B) {
	x := d(b, "1.2345678901234567890123456789")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Sqrt(x); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNormCDF(b *testing.B) {
	x := d(b, "0.4567890123456789012345678901")
	b.ReportAllocs()
	for b.Loop() {
		_ = NormCDF(x)
	}
}

func BenchmarkNormCDFTail(b *testing.B) {
	x := d(b, "-9.876543210987654321")
	b.ReportAllocs()
	for b.Loop() {
		_ = NormCDF(x)
	}
}

func BenchmarkNormPDF(b *testing.B) {
	x := d(b, "0.4567890123456789012345678901")
	b.ReportAllocs()
	for b.Loop() {
		_ = NormPDF(x)
	}
}

func BenchmarkDecMul(b *testing.B) {
	x, y := d(b, "1.2345678901234567890123456789"), d(b, "98765.4321")
	b.ReportAllocs()
	for b.Loop() {
		_ = x.Mul(y)
	}
}
