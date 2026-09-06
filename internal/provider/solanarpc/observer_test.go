package solanarpc_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
)

func statusResult(items ...any) any { return ctxSlot("300", items) }

func TestGetTransaction_UpgradesCommitmentThroughStatus(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	s.handle("getTransaction", func(rpcCall) (any, *rpcErr) { return validTx(), nil })
	s.handle("getSignatureStatuses", func(rpcCall) (any, *rpcErr) {
		return statusResult(map[string]any{"slot": json.Number("250000123"), "confirmations": nil, "err": nil, "confirmationStatus": "finalized"}), nil
	})
	c, td := newClient(t, s, nil)
	obs, err := c.GetTransaction(context.Background(), sigA)
	require.NoError(t, err)
	require.True(t, obs.Found)
	require.Equal(t, chain.CommitmentFinalized, obs.Commitment)
	require.Equal(t, "rpc-fallback", obs.Source)
	require.NotEmpty(t, obs.RawRef)
	require.Equal(t, td.clk.Now(), obs.ObservedAt)

	calls := s.recorded("getTransaction")
	require.Len(t, calls, 1)
	var sig string
	param(t, calls[0], 0, &sig)
	require.Equal(t, sigA, sig)
	var cfg map[string]any
	param(t, calls[0], 1, &cfg)
	require.Equal(t, "confirmed", cfg["commitment"])
	require.Equal(t, json.Number("0"), cfg["maxSupportedTransactionVersion"], "always 0: v0 transactions fail otherwise")
	require.Equal(t, "json", cfg["encoding"])
	st := s.recorded("getSignatureStatuses")
	require.Len(t, st, 1)
	var sigs []string
	param(t, st[0], 0, &sigs)
	require.Equal(t, []string{sigA}, sigs)
	var scfg map[string]any
	param(t, st[0], 1, &scfg)
	require.Equal(t, true, scfg["searchTransactionHistory"])

	// A status at a different slot never upgrades (fork between the reads).
	s.handle("getSignatureStatuses", func(rpcCall) (any, *rpcErr) {
		return statusResult(map[string]any{"slot": json.Number("1"), "confirmations": nil, "err": nil, "confirmationStatus": "finalized"}), nil
	})
	obs, err = c.GetTransaction(context.Background(), sigA)
	require.NoError(t, err)
	require.Equal(t, chain.CommitmentConfirmed, obs.Commitment)

	// Status failure keeps the read commitment and does not fail the read.
	s.handle("getSignatureStatuses", func(rpcCall) (any, *rpcErr) { return nil, &rpcErr{Code: -32000, Message: "x"} })
	obs, err = c.GetTransaction(context.Background(), sigA)
	require.NoError(t, err)
	require.Equal(t, chain.CommitmentConfirmed, obs.Commitment)
	require.Contains(t, td.logs.String(), "keeping read commitment")

	// Explicit finalized read.
	s.handle("getSignatureStatuses", func(rpcCall) (any, *rpcErr) { return statusResult(nil), nil })
	obs, err = c.GetTransactionAt(context.Background(), sigA, chain.CommitmentFinalized)
	require.NoError(t, err)
	require.Equal(t, chain.CommitmentFinalized, obs.Commitment)
	_, err = c.GetTransactionAt(context.Background(), sigA, chain.CommitmentProcessed)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestGetTransaction_NotFoundAndValidation(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	s.handle("getTransaction", func(rpcCall) (any, *rpcErr) { return nil, nil })
	c, _ := newClient(t, s, nil)
	obs, err := c.GetTransaction(context.Background(), sigA)
	require.NoError(t, err)
	require.False(t, obs.Found)
	require.Equal(t, sigA, obs.Signature)
	require.Empty(t, s.recorded("getSignatureStatuses"), "no status lookup for an unknown signature")

	_, err = c.GetTransaction(context.Background(), "nope")
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	require.Len(t, s.recorded(""), 1, "invalid input never reaches the network")

	d := validTx()
	d.meta()["fee"] = json.Number("1.5")
	s.handle("getTransaction", func(rpcCall) (any, *rpcErr) { return d, nil })
	_, err = c.GetTransaction(context.Background(), sigA)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.NotEmpty(t, e.Fields["raw_ref"], "the malformed body is archived for investigation")
}

func TestGetSignatureStatuses_Validation(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	c, _ := newClient(t, s, nil)
	_, err := c.GetSignatureStatuses(context.Background(), nil)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	many := make([]string, 257)
	for i := range many {
		many[i] = sigA
	}
	_, err = c.GetSignatureStatuses(context.Background(), many)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = c.GetSignatureStatuses(context.Background(), []string{"bad"})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestGetBalances(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	s.handle("getTokenAccountsByOwner", func(call rpcCall) (any, *rpcErr) {
		var filter map[string]string
		param(t, call, 1, &filter)
		switch {
		case filter["mint"] == mintUSDC:
			return ctxSlot("400", []any{parsedTokenAccount(ataUSDC, mintUSDC, wallet, "1000000", 6)}), nil
		case filter["programId"] == solanarpc.TokenProgramID:
			return ctxSlot("400", []any{parsedTokenAccount(ataUSDC, mintUSDC, wallet, "1000000", 6), parsedTokenAccount(ataBONK, mintBONK, wallet, "5", 5)}), nil
		case filter["programId"] == solanarpc.Token2022ProgramID:
			return ctxSlot("400", []any{}), nil
		}
		return ctxSlot("400", []any{}), nil
	})
	s.handle("getBalance", func(rpcCall) (any, *rpcErr) { return ctxSlot("401", json.Number("7955720")), nil })
	c, _ := newClient(t, s, nil)

	bs, err := c.GetBalances(context.Background(), wallet, []string{mintUSDC, chain.MintNativeSOL, mintUSDC})
	require.NoError(t, err)
	require.Len(t, bs, 2)
	require.Equal(t, mintUSDC, bs[0].Mint)
	require.Equal(t, "1000000", bs[0].Amount.String())
	require.Equal(t, uint64(400), bs[0].Slot)
	require.Equal(t, chain.MintNativeSOL, bs[1].Mint)
	require.Equal(t, wallet, bs[1].TokenAccount)
	require.Equal(t, "7955720", bs[1].Amount.String())
	require.Equal(t, chain.NativeDecimals, bs[1].Decimals)
	require.Equal(t, uint64(401), bs[1].Slot)
	require.Len(t, s.recorded("getTokenAccountsByOwner"), 1, "duplicate mints are queried once")
	var enc map[string]any
	param(t, s.recorded("getTokenAccountsByOwner")[0], 2, &enc)
	require.Equal(t, "jsonParsed", enc["encoding"])
	require.Equal(t, "confirmed", enc["commitment"])

	bs, err = c.GetBalances(context.Background(), wallet, nil)
	require.NoError(t, err)
	require.Len(t, bs, 3, "both token programs plus native")
	require.Len(t, s.recorded("getTokenAccountsByOwner"), 3)

	_, err = c.GetBalances(context.Background(), "x", nil)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = c.GetBalances(context.Background(), wallet, []string{"x"})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestBlockhashHeightAndValidity(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	s.handle("getBlockHeight", func(rpcCall) (any, *rpcErr) { return json.Number("288000000"), nil })
	s.handle("getLatestBlockhash", func(rpcCall) (any, *rpcErr) {
		return ctxSlot("300000000", map[string]any{"blockhash": hashA, "lastValidBlockHeight": json.Number("288000150")}), nil
	})
	s.handle("isBlockhashValid", func(call rpcCall) (any, *rpcErr) {
		var h string
		param(t, call, 0, &h)
		return ctxSlot("300000001", h == hashA), nil
	})
	c, _ := newClient(t, s, nil)
	h, err := c.GetBlockHeight(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(288000000), h)
	bh, err := c.GetLatestBlockhash(context.Background())
	require.NoError(t, err)
	require.Equal(t, hashA, bh.Blockhash)
	require.Equal(t, uint64(288000150), bh.LastValidBlockHeight)
	require.Equal(t, uint64(300000000), bh.Slot)
	require.Equal(t, "rpc-fallback", bh.Source)
	require.Greater(t, bh.LastValidBlockHeight, h, "blockhash is a height, compare with getBlockHeight not slots")
	ok, err := c.IsBlockhashValid(context.Background(), hashA)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = c.IsBlockhashValid(context.Background(), "!")
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	var cfg map[string]any
	param(t, s.recorded("getBlockHeight")[0], 0, &cfg)
	require.Equal(t, "confirmed", cfg["commitment"])
}

func TestSimulate(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	s.handle("simulateTransaction", func(rpcCall) (any, *rpcErr) {
		return ctxSlot("5", map[string]any{"err": nil, "logs": []string{"ok"}, "unitsConsumed": json.Number("1000"), "accounts": nil, "innerInstructions": nil}), nil
	})
	c, _ := newClient(t, s, nil)
	raw := []byte{1, 2, 3, 4}
	sim, err := c.Simulate(context.Background(), raw, chain.SimulateOptions{InnerInstructions: true, ReturnAccounts: []string{ataUSDC}, Commitment: chain.CommitmentProcessed})
	require.NoError(t, err)
	require.True(t, sim.OK)
	require.Equal(t, uint64(1000), sim.UnitsConsumed)
	require.Equal(t, "rpc-fallback", sim.Source)
	call := s.recorded("simulateTransaction")[0]
	var tx string
	param(t, call, 0, &tx)
	require.Equal(t, base64.StdEncoding.EncodeToString(raw), tx)
	var cfg map[string]any
	param(t, call, 1, &cfg)
	require.Equal(t, "base64", cfg["encoding"])
	require.Equal(t, "processed", cfg["commitment"])
	require.Equal(t, true, cfg["innerInstructions"])
	require.Equal(t, false, cfg["sigVerify"])
	require.Equal(t, map[string]any{"addresses": []any{ataUSDC}, "encoding": "jsonParsed"}, cfg["accounts"])

	_, err = c.Simulate(context.Background(), nil, chain.SimulateOptions{})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = c.Simulate(context.Background(), raw, chain.SimulateOptions{SigVerify: true, ReplaceRecentBlockhash: true})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = c.Simulate(context.Background(), raw, chain.SimulateOptions{ReturnAccounts: []string{"bad"}})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = c.Simulate(context.Background(), raw, chain.SimulateOptions{Commitment: "x"})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestSearchWalletActivity_PagingAndTokenAccounts(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	s.handle("getTokenAccountsByOwner", func(call rpcCall) (any, *rpcErr) {
		var filter map[string]string
		param(t, call, 1, &filter)
		if filter["programId"] == solanarpc.TokenProgramID {
			return ctxSlot("400", []any{parsedTokenAccount(ataUSDC, mintUSDC, wallet, "1", 6)}), nil
		}
		return ctxSlot("400", []any{}), nil
	})
	sigItem := func(sig string, slot, bt int64) map[string]any {
		return map[string]any{"signature": sig, "slot": json.Number(jsonInt(int(slot))), "err": nil, "memo": nil, "blockTime": json.Number(jsonInt(int(bt))), "confirmationStatus": "finalized"}
	}
	since := time.Unix(1_000_000, 0).UTC()
	s.handle("getSignaturesForAddress", func(call rpcCall) (any, *rpcErr) {
		var addr string
		param(t, call, 0, &addr)
		var cfg map[string]any
		param(t, call, 1, &cfg)
		require.Equal(t, json.Number("2"), cfg["limit"], "page size option bounds the RPC limit")
		require.Equal(t, "confirmed", cfg["commitment"])
		if addr == ataUSDC {
			return []any{sigItem(sigB, 200, 1_000_500)}, nil // overlaps with the wallet page
		}
		switch cfg["before"] {
		case nil:
			return []any{sigItem(sigA, 210, 1_000_600), sigItem(sigB, 200, 1_000_500)}, nil
		case sigB:
			return []any{sigItem(sigC, 190, 999_000)}, nil // older than since: stops
		}
		t.Fatalf("unexpected before %v", cfg["before"])
		return nil, nil
	})
	s.handle("getTransaction", func(call rpcCall) (any, *rpcErr) {
		var sig string
		param(t, call, 0, &sig)
		d := validTx()
		d.tx()["signatures"] = []string{sig}
		switch sig {
		case sigA:
			d["slot"] = json.Number("210")
		case sigB:
			d["slot"] = json.Number("200")
		default:
			return nil, nil
		}
		return d, nil
	})
	s.handle("getSignatureStatuses", func(rpcCall) (any, *rpcErr) { return statusResult(nil), nil })
	c, _ := newClient(t, s, func(_ *config.ProviderConfig, opts *solanarpc.Options, _ *solanarpc.Deps) {
		opts.SignaturesPageSize = 2
	})
	txs, err := c.SearchWalletActivity(context.Background(), wallet, since, 3)
	require.NoError(t, err)
	require.Len(t, txs, 2, "sigC is older than since and never fetched")
	require.Equal(t, sigA, txs[0].Signature, "newest first")
	require.Equal(t, sigB, txs[1].Signature)
	require.Len(t, s.recorded("getTransaction"), 2, "deduplicated across the wallet and its token accounts")
	require.Len(t, s.recorded("getSignaturesForAddress"), 3, "wallet page 1, page 2 (before=last), token account")
	var cfg2 map[string]any
	param(t, s.recorded("getSignaturesForAddress")[1], 1, &cfg2)
	require.Equal(t, sigB, cfg2["before"], "page 2 continues from the last signature of page 1")

	_, err = c.SearchWalletActivity(context.Background(), "bad", since, 1)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// A failing fetch aborts the search.
	var fails atomic.Int32
	s.handle("getTransaction", func(rpcCall) (any, *rpcErr) {
		fails.Add(1)
		return nil, &rpcErr{Code: -32602, Message: "bad"}
	})
	_, err = c.SearchWalletActivity(context.Background(), wallet, since, 0)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_ = money.QuantityFromInt64
}
