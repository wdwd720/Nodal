package jupiter_test

import (
	"context"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider/jupiter"
	"github.com/nodal/controlplane/internal/provider/jupiter/jupitertest"
)

func newFake(t *testing.T, edit func(*jupiter.FakeConfig)) (*jupiter.Fake, *jupitertest.MemoryArchive, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(testNow)
	arch := &jupitertest.MemoryArchive{}
	cfg := jupiter.FakeConfig{Env: config.EnvTest, Clock: clk, Archive: arch, Rates: map[string]jupiter.FakeRate{jupitertest.MintUSDC + ">" + jupitertest.MintSOL: {Num: 13, Den: 2000}}}
	if edit != nil {
		edit(&cfg)
	}
	f, err := jupiter.NewFake(cfg)
	require.NoError(t, err)
	return f, arch, clk
}

func TestFake_RejectsProductionLike(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd, "BOGUS"} {
		_, err := jupiter.NewFake(jupiter.FakeConfig{Env: env, Clock: clock.NewFake(testNow)})
		require.Error(t, err, string(env))
	}
	cfg := jupiter.DefaultConfig(config.EnvProd)
	cfg.Mode = config.ProviderModeFake
	require.Error(t, cfg.Validate())
}

func TestFake_DeterministicOrderWithRealTransaction(t *testing.T) {
	t.Parallel()
	f1, arch, _ := newFake(t, nil)
	f2, _, _ := newFake(t, nil)
	ctx := context.Background()
	o1, err := f1.Order(ctx, orderReq())
	require.NoError(t, err)
	o2, err := f2.Order(ctx, orderReq())
	require.NoError(t, err)
	require.Equal(t, o1.QuoteID, o2.QuoteID)
	require.Equal(t, o1.UnsignedTransaction, o2.UnsignedTransaction, "byte-identical across instances")
	require.Equal(t, o1.RouteHash, o2.RouteHash)

	require.Equal(t, "1000000", o1.InAmount.String())
	require.Equal(t, "6500", o1.OutAmount.String(), "1_000_000 x 13 / 2000")
	require.Equal(t, "6467", o1.OtherAmountThreshold.String(), "6500 x 9950 / 10000 rounded down")
	require.Equal(t, money.BPS(50), o1.SlippageBPS)
	require.Equal(t, money.BPS(5), o1.PriceImpactBPS)
	require.True(t, o1.HasTransaction)
	require.Equal(t, uint64(250_000_150), o1.LastValidBlockHeight)
	require.Equal(t, uint32(200_000), o1.ComputeUnitLimit)
	require.Equal(t, taker.PublicKey.String(), o1.FeePayer)
	require.Equal(t, 1, arch.Len())

	tx, err := solana.TransactionFromBytes(o1.UnsignedTransaction)
	require.NoError(t, err)
	require.True(t, tx.Message.IsVersioned(), "v0 like the real API")
	require.Len(t, tx.Signatures, 1)
	require.Equal(t, []string{"ComputeBudget111111111111111111111111111111", "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL", jupiter.DefaultProgramID}, o1.ProgramIDs)
	require.NoError(t, jupiter.ValidateQuote(o1, testNow.Add(time.Second), jupiter.ValidationPolicy{
		ExpectedTaker: taker.PublicKey.String(), ExpectedMinOut: qtyPtr(6_400), CurrentBlockHeight: 250_000_000, MinBlockHeightMargin: 50, RequireTransaction: true,
	}))

	// A second order in the same instance gets a new id (sequence), like a new /order call.
	o3, err := f1.Order(ctx, orderReq())
	require.NoError(t, err)
	require.NotEqual(t, o1.QuoteID, o3.QuoteID)

	quoteOnly := orderReq()
	quoteOnly.TakerPubkey = ""
	o4, err := f1.Order(ctx, quoteOnly)
	require.NoError(t, err)
	require.False(t, o4.HasTransaction)
}

func TestFake_ExecuteRoundTrip(t *testing.T) {
	t.Parallel()
	f, _, _ := newFake(t, nil)
	ctx := context.Background()
	o, err := f.Order(ctx, orderReq())
	require.NoError(t, err)
	signed := jupitertest.Sign(t, o.UnsignedTransaction, taker)
	res, err := f.Execute(ctx, jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: o.QuoteID, LastValidBlockHeight: o.LastValidBlockHeightRaw})
	require.NoError(t, err)
	require.Equal(t, jupiter.ExecuteSuccess, res.Status)
	require.Equal(t, jupitertest.SignatureOf(t, signed), res.Signature)
	require.Equal(t, "6500", res.TotalOutputAmount.String())
	subs := f.Submissions()
	require.Len(t, subs, 1)
	require.Equal(t, o.QuoteID, subs[0].RequestID)

	// Unsigned transaction is rejected locally.
	_, err = f.Execute(ctx, jupiter.ExecuteRequest{SignedTransaction: o.UnsignedTransaction, RequestID: o.QuoteID})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	// Unknown request id: missing cached order.
	_, err = f.Execute(ctx, jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: "nope"})
	require.Equal(t, errs.CodeQuoteExpired, errs.CodeOf(err))
	// Past lastValidBlockHeight: Failed, nothing lands.
	o2, err := f.Order(ctx, orderReq())
	require.NoError(t, err)
	f.AdvanceBlockHeight(200)
	res, err = f.Execute(ctx, jupiter.ExecuteRequest{SignedTransaction: jupitertest.Sign(t, o2.UnsignedTransaction, taker), RequestID: o2.QuoteID})
	require.NoError(t, err)
	require.Equal(t, jupiter.ExecuteFailed, res.Status)
	require.Len(t, f.Submissions(), 1)
	_, err = f.Status(ctx, res.Signature)
	require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
}

func TestFake_Faults(t *testing.T) {
	t.Parallel()
	f, _, _ := newFake(t, nil)
	ctx := context.Background()

	f.InjectFault(jupiter.OpOrder, jupiter.FaultTimeout)
	_, err := f.Order(ctx, orderReq())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	f.InjectFault(jupiter.OpOrder, jupiter.FaultRateLimited)
	_, err = f.Order(ctx, orderReq())
	require.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.NotNil(t, e.RetryAfter)
	f.InjectFault(jupiter.OpOrder, jupiter.FaultServerError)
	_, err = f.Order(ctx, orderReq())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	f.InjectFault(jupiter.OpOrder, jupiter.FaultInvalidResponse)
	_, err = f.Order(ctx, orderReq())
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	e, _ = errs.As(err)
	require.Equal(t, "inAmount", e.Fields["field"])
	f.InjectFault(jupiter.OpOrder, jupiter.FaultExpired)
	o, err := f.Order(ctx, orderReq())
	require.NoError(t, err)
	err = jupiter.ValidateQuote(o, testNow, jupiter.ValidationPolicy{CurrentBlockHeight: f.BlockHeight()})
	require.Equal(t, errs.CodeQuoteExpired, errs.CodeOf(err))
	f.ClearFaults()

	o, err = f.Order(ctx, orderReq())
	require.NoError(t, err)
	signed := jupitertest.Sign(t, o.UnsignedTransaction, taker)
	req := jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: o.QuoteID}

	f.InjectFault(jupiter.OpExecute, jupiter.FaultRateLimited)
	_, err = f.Execute(ctx, req)
	require.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	f.InjectFault(jupiter.OpExecute, jupiter.FaultExpired)
	_, err = f.Execute(ctx, req)
	require.Equal(t, errs.CodeQuoteExpired, errs.CodeOf(err))
	f.InjectFault(jupiter.OpExecute, jupiter.FaultServerError)
	_, err = f.Execute(ctx, req)
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	f.InjectFault(jupiter.OpExecute, jupiter.FaultInvalidResponse)
	_, err = f.Execute(ctx, req)
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	require.Empty(t, f.Submissions(), "none of those reached the chain")

	f.InjectFault(jupiter.OpExecute, jupiter.FaultSlippageExceeded)
	res, err := f.Execute(ctx, req)
	require.NoError(t, err)
	require.Equal(t, jupiter.ExecuteFailed, res.Status)
	require.Equal(t, int64(-1001), res.ProviderCode)
	require.Empty(t, f.Submissions())

	f.InjectFault(jupiter.OpExecute, jupiter.FaultTimeout)
	res, err = f.Execute(ctx, req)
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	e, _ = errs.As(err)
	require.Equal(t, true, e.Fields["timed_out"])
	require.Equal(t, jupitertest.SignatureOf(t, signed), res.Signature)
	subs := f.Submissions()
	require.Len(t, subs, 1, "the provider landed it but the response was lost")
	require.True(t, subs[0].ResponseLost)
}

func TestFake_Build(t *testing.T) {
	t.Parallel()
	f, _, _ := newFake(t, nil)
	s := money.BPS(50)
	r, err := f.Build(context.Background(), jupiter.BuildRequest{
		InputMint: jupitertest.MintUSDC, OutputMint: jupitertest.MintSOL, Amount: money.QuantityFromInt64(1_000_000), Taker: taker.PublicKey.String(), SlippageBPS: &s,
	})
	require.NoError(t, err)
	require.Equal(t, "6500", r.OutAmount.String())
	require.Len(t, r.ComputeBudgetInstructions, 2)
	require.Len(t, r.SetupInstructions, 1)
	require.Equal(t, jupiter.DefaultProgramID, r.SwapInstruction.ProgramID)
	require.Len(t, r.SwapInstruction.Accounts, 6)
	require.True(t, r.SwapInstruction.Accounts[1].IsSigner)
	require.True(t, r.SwapInstruction.Accounts[2].IsWritable)
	require.True(t, r.ComputeUnitLimitUnavailable)
	require.NotEmpty(t, r.Blockhash)
	f.InjectFault(jupiter.OpBuild, jupiter.FaultInvalidResponse)
	_, err = f.Build(context.Background(), jupiter.BuildRequest{InputMint: jupitertest.MintUSDC, OutputMint: jupitertest.MintSOL, Amount: money.QuantityFromInt64(1_000_000), Taker: taker.PublicKey.String()})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}
