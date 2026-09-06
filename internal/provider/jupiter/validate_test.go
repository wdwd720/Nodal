package jupiter_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider/jupiter"
	"github.com/nodal/controlplane/internal/provider/jupiter/jupitertest"
)

func goodOrder() jupiter.Order {
	return jupiter.Order{
		QuoteID: "q", InputMint: jupitertest.MintUSDC, OutputMint: jupitertest.MintSOL, Taker: taker.PublicKey.String(),
		InAmount: money.QuantityFromInt64(1_000_000), OutAmount: money.QuantityFromInt64(6_500_000), OtherAmountThreshold: money.QuantityFromInt64(6_467_500),
		SlippageBPS: 50, PriceImpactBPS: 5,
		Route:          []jupiter.RouteStep{{InputMint: jupitertest.MintUSDC, OutputMint: jupitertest.MintSOL}},
		HasTransaction: true, FeePayer: taker.PublicKey.String(), LastValidBlockHeight: 250_000_150,
		ReceivedAt: testNow, ExpiresAt: testNow.Add(30 * time.Second),
	}
}

func bpsPtr(v money.BPS) *money.BPS { return &v }

func qtyPtr(v int64) *money.Quantity {
	q := money.QuantityFromInt64(v)
	return &q
}

func TestValidateQuote(t *testing.T) {
	t.Parallel()
	strict := jupiter.ValidationPolicy{
		MaxAge: 10 * time.Second, ExpectedInputMint: jupitertest.MintUSDC, ExpectedOutputMint: jupitertest.MintSOL,
		ExpectedTaker: taker.PublicKey.String(), ExpectedInAmount: qtyPtr(1_000_000), ExpectedMinOut: qtyPtr(6_400_000),
		MaxSlippageBPS: bpsPtr(100), MaxPriceImpactBPS: bpsPtr(50), RequirePriceImpact: true,
		CurrentBlockHeight: 250_000_000, MinBlockHeightMargin: 20, RequireTransaction: true,
	}
	cases := []struct {
		name    string
		edit    func(o *jupiter.Order)
		now     time.Time
		policy  jupiter.ValidationPolicy
		code    errs.Code
		reasons []string
	}{
		{"passes", nil, testNow.Add(time.Second), strict, "", nil},
		{"expired wall clock", nil, testNow.Add(31 * time.Second), strict, errs.CodeQuoteExpired, []string{jupiter.ReasonExpired, jupiter.ReasonTooOld}},
		{"too old only", nil, testNow.Add(11 * time.Second), strict, errs.CodeQuoteExpired, []string{jupiter.ReasonTooOld}},
		{"block height exceeded", nil, testNow.Add(time.Second), func() jupiter.ValidationPolicy {
			p := strict
			p.CurrentBlockHeight = 250_000_140
			return p
		}(), errs.CodeQuoteExpired, []string{jupiter.ReasonBlockHeight}},
		{"block height missing", func(o *jupiter.Order) { o.LastValidBlockHeight = 0 }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonBlockHeightZero}},
		{"mint mismatch", func(o *jupiter.Order) { o.OutputMint = jupitertest.MintUSDC }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonOutputMint, jupiter.ReasonRouteMint}},
		{"taker and fee payer mismatch", func(o *jupiter.Order) { o.Taker, o.FeePayer = "x", "y" }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonFeePayer, jupiter.ReasonTaker}},
		{"min out below expected", func(o *jupiter.Order) { o.OtherAmountThreshold = money.QuantityFromInt64(6_000_000) }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonMinOut}},
		{"threshold above quote", func(o *jupiter.Order) { o.OtherAmountThreshold = money.QuantityFromInt64(7_000_000) }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonMinOutAboveQuote}},
		{"slippage above policy", func(o *jupiter.Order) { o.SlippageBPS = 101 }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonSlippage}},
		{"price impact above policy", func(o *jupiter.Order) { o.PriceImpactBPS = 51 }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonPriceImpact}},
		{"price impact unavailable", func(o *jupiter.Order) { o.PriceImpactUnavailable = true }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonPriceImpactAbsent}},
		{"missing transaction", func(o *jupiter.Order) { o.HasTransaction = false }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonNoTransaction}},
		{"in amount mismatch", func(o *jupiter.Order) { o.InAmount = money.QuantityFromInt64(2) }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonInAmount}},
		{"non positive", func(o *jupiter.Order) { o.OutAmount = money.QuantityFromInt64(0) }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonMinOutAboveQuote, jupiter.ReasonNonPositive}},
		{"no quote id", func(o *jupiter.Order) { o.QuoteID = "" }, testNow.Add(time.Second), strict, errs.CodeValidationFailed, []string{jupiter.ReasonNoQuoteID}},
		{"expired and mismatched is QUOTE_EXPIRED", func(o *jupiter.Order) { o.SlippageBPS = 101 }, testNow.Add(time.Minute), strict, errs.CodeQuoteExpired, []string{jupiter.ReasonExpired, jupiter.ReasonSlippage, jupiter.ReasonTooOld}},
		{"empty policy only checks intrinsic", func(o *jupiter.Order) { o.PriceImpactUnavailable = true }, testNow.Add(time.Second), jupiter.ValidationPolicy{}, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := goodOrder()
			if tc.edit != nil {
				tc.edit(&o)
			}
			err := jupiter.ValidateQuote(o, tc.now, tc.policy)
			if tc.code == "" {
				require.NoError(t, err)
				return
			}
			require.Equal(t, tc.code, errs.CodeOf(err), "%v", err)
			e, _ := errs.As(err)
			require.Equal(t, tc.reasons, e.Fields["reasons"])
		})
	}
}
