package fees

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
)

var usdc = assets.NewAssetID()

func q(s string) money.Quantity {
	v, err := money.ParseQuantity(s)
	if err != nil {
		panic(err)
	}
	return v
}

func TestZeroPolicy(t *testing.T) {
	p := Zero(usdc)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := p.Compute(q("100000000"))
	if err != nil {
		t.Fatal(err)
	}
	if !b.PlatformFee.IsZero() || b.PolicyVersion != "fee-zero-v1" || b.PolicyHash == "" {
		t.Fatalf("zero policy breakdown %+v", b)
	}
}

func TestCompute_BPSAndBounds(t *testing.T) {
	p := Policy{Version: "v1", PlatformFeeBPS: 10, FeeAsset: usdc, Rounding: money.RoundHalfEven} // 0.10%
	cases := []struct {
		in, want string
		bounded  string
		min, max string
	}{
		{"100000000", "100000", "", "", ""},          // 100 USDC → 0.10 USDC
		{"1", "0", "", "", ""},                       // rounds to zero
		{"5", "0", "", "", ""},                       // 0.005 → half-even → 0
		{"15", "0", "", "", ""},                      // 0.015 → 0 (half-even on .5? 15*10/10000 = 0.015 → 0)
		{"100000000", "500000", "min", "500000", ""}, // min 0.50 USDC applies
		{"100000000", "50000", "max", "", "50000"},   // max 0.05 USDC applies
	}
	for _, c := range cases {
		pp := p
		if c.min != "" {
			pp.MinFee = q(c.min)
		}
		if c.max != "" {
			pp.MaxFee = q(c.max)
		}
		b, err := pp.Compute(q(c.in))
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if b.PlatformFee.String() != c.want || b.Bounded != c.bounded {
			t.Fatalf("in=%s want=%s got=%s bounded=%q (want %q)", c.in, c.want, b.PlatformFee.String(), b.Bounded, c.bounded)
		}
	}
}

func TestCompute_Rejections(t *testing.T) {
	p := Policy{Version: "v1", PlatformFeeBPS: 10, FeeAsset: usdc, Rounding: money.RoundHalfEven}
	if _, err := p.Compute(q("-1")); err == nil {
		t.Fatal("negative input")
	}
	bad := p
	bad.MinFee = q("10")
	if _, err := bad.Compute(q("5")); err == nil {
		t.Fatal("min fee above input must fail")
	}
	for _, mut := range []func(*Policy){
		func(x *Policy) { x.Version = "" },
		func(x *Policy) { x.PlatformFeeBPS = 10001 },
		func(x *Policy) { x.PlatformFeeBPS = -1 },
		func(x *Policy) { x.FeeAsset = assets.AssetID{} },
		func(x *Policy) { x.Rounding = money.RoundExact },
		func(x *Policy) { x.Rounding = 0 },
		func(x *Policy) { x.MinFee = q("10"); x.MaxFee = q("5") },
	} {
		x := p
		mut(&x)
		if err := x.Validate(); err == nil {
			t.Fatalf("expected validation failure for %+v", x)
		}
	}
}

func TestHash_Stable(t *testing.T) {
	a := Policy{Version: "v1", PlatformFeeBPS: 10, FeeAsset: usdc, Rounding: money.RoundHalfEven}
	b := a
	if a.Hash() != b.Hash() {
		t.Fatal("same policy, different hash")
	}
	b.PlatformFeeBPS = 11
	if a.Hash() == b.Hash() {
		t.Fatal("different policy, same hash")
	}
}

// TestProp_FeeNeverExceedsInputAndIsMonotone: for any policy and input, the
// fee is within [0, input], and a larger input never yields a smaller fee
// (before bounds).
func TestProp_FeeNeverExceedsInputAndIsMonotone(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		p := Policy{
			Version:        "prop",
			PlatformFeeBPS: money.BPS(rapid.Int64Range(0, 10_000).Draw(rt, "bps")),
			FeeAsset:       usdc,
			Rounding:       money.RoundHalfEven,
		}
		x := money.QuantityFromInt64(rapid.Int64Range(0, 1_000_000_000_000).Draw(rt, "x"))
		y := money.QuantityFromInt64(rapid.Int64Range(0, 1_000_000_000_000).Draw(rt, "y"))
		bx, err := p.Compute(x)
		if err != nil {
			rt.Fatal(err)
		}
		by, err := p.Compute(y)
		if err != nil {
			rt.Fatal(err)
		}
		if bx.PlatformFee.Sign() < 0 || bx.PlatformFee.Cmp(x) > 0 {
			rt.Fatalf("fee out of range: %s for input %s", bx.PlatformFee, x)
		}
		if x.Cmp(y) <= 0 && bx.PlatformFee.Cmp(by.PlatformFee) > 0 {
			rt.Fatalf("not monotone: fee(%s)=%s > fee(%s)=%s", x, bx.PlatformFee, y, by.PlatformFee)
		}
	})
}
