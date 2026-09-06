package solanarpc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
)

func TestContract_GetTransaction_V0Swap(t *testing.T) {
	t.Parallel()
	r := newReplay(t).on("getTransaction", "getTransaction_v0_swap.json").on("getSignatureStatuses", "getSignatureStatuses_confirmed.json")
	h := newHarness(t, r, nil)
	obs, err := h.client.GetTransaction(context.Background(), sigSwap)
	require.NoError(t, err)
	require.True(t, obs.Found)
	require.Equal(t, uint64(250000123), obs.Slot)
	require.Equal(t, chain.VersionV0, obs.Version)
	require.Equal(t, chain.CommitmentConfirmed, obs.Commitment)
	require.Equal(t, "", obs.Err)
	require.Equal(t, "5000", obs.Fee.String())
	require.Equal(t, time.Unix(1757073600, 0).UTC(), *obs.BlockTime)
	require.Equal(t, []string{wallet, ataUSDC, ataBONK, "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4", poolAcct, "6tY8uJ1oP3aS5dF7gH9jK2mZ4xC6vB8nM1qW3eR5tY7u"}, obs.AccountKeys)

	deltas := obs.DeltasFor(wallet)
	require.Len(t, deltas, 2)
	require.Equal(t, ataUSDC, deltas[0].TokenAccount)
	require.Equal(t, "-1000000", deltas[0].Delta().String())
	require.Equal(t, uint8(6), deltas[0].Decimals)
	require.Equal(t, ataBONK, deltas[1].TokenAccount)
	require.Equal(t, "123456789", deltas[1].Delta().String())
	require.Equal(t, "0", deltas[1].Pre.String(), "token account created by the swap")
	pool := obs.DeltasFor(poolAcct)
	require.Len(t, pool, 1, "lookup-table account resolved through loadedAddresses")
	require.Equal(t, "1000", pool[0].Delta().String())
	require.Equal(t, "-5000", chain.SumLamportDeltas(obs.LamportDeltas).String())

	call := r.recorded("getTransaction")[0]
	var cfg map[string]any
	param(t, call, 1, &cfg)
	require.Equal(t, json.Number("0"), cfg["maxSupportedTransactionVersion"])
	require.Equal(t, "json", cfg["encoding"])
	require.Equal(t, "confirmed", cfg["commitment"])

	// Evidence: the raw body is archived and referenced.
	require.NotEmpty(t, obs.RawRef)
	raw, ok := h.archive.Get(obs.RawRef)
	require.True(t, ok)
	require.Equal(t, "getTransaction", raw.EventType)
	require.Equal(t, sigSwap, raw.SourceEventID)
	require.Contains(t, string(raw.Body), `"amount":"123456789"`)
	require.Equal(t, "rpc-fallback", obs.Source)
	require.Equal(t, h.clk.Now(), obs.ReceivedAt)
}

func TestContract_GetTransaction_NotFoundAndFailed(t *testing.T) {
	t.Parallel()
	r := newReplay(t).on("getTransaction", "getTransaction_null.json")
	h := newHarness(t, r, nil)
	obs, err := h.client.GetTransaction(context.Background(), sigSwap)
	require.NoError(t, err)
	require.False(t, obs.Found)
	require.Empty(t, r.recorded("getSignatureStatuses"))

	r = newReplay(t).on("getTransaction", "getTransaction_failed.json").on("getSignatureStatuses", "getSignatureStatuses_finalized.json")
	h = newHarness(t, r, nil)
	obs, err = h.client.GetTransaction(context.Background(), sigSwap)
	require.NoError(t, err)
	require.True(t, obs.Found, "a failed transaction landed and paid its fee")
	require.False(t, obs.Succeeded())
	require.Equal(t, `{"InstructionError":[0,{"Custom":6001}]}`, obs.Err)
	require.Equal(t, "-5000", chain.SumLamportDeltas(obs.LamportDeltas).String())
	require.Empty(t, obs.DeltasFor(wallet)[0].Delta().Sign(), "no token movement on failure")
}

func TestContract_ConfirmedVsFinalized(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]chain.Commitment{
		"getSignatureStatuses_confirmed.json": chain.CommitmentConfirmed,
		"getSignatureStatuses_finalized.json": chain.CommitmentFinalized,
		"getSignatureStatuses_unknown.json":   chain.CommitmentConfirmed, // read commitment is the floor
	} {
		r := newReplay(t).on("getTransaction", "getTransaction_v0_swap.json").on("getSignatureStatuses", name)
		h := newHarness(t, r, nil)
		obs, err := h.client.GetTransaction(context.Background(), sigSwap)
		require.NoError(t, err, name)
		require.Equal(t, want, obs.Commitment, name)
		sts, err := h.client.GetSignatureStatuses(context.Background(), []string{sigSwap})
		require.NoError(t, err)
		if name == "getSignatureStatuses_unknown.json" {
			require.False(t, sts[0].Found)
		} else {
			require.True(t, sts[0].Found)
			require.Equal(t, want, sts[0].Commitment)
		}
	}
}

func TestContract_MalformedFloatAmountRejected(t *testing.T) {
	t.Parallel()
	r := newReplay(t).on("getTransaction", "getTransaction_float_amount.json")
	h := newHarness(t, r, nil)
	_, err := h.client.GetTransaction(context.Background(), sigSwap)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	require.Len(t, r.recorded("getTransaction"), 1, "malformed responses are not retried")
	e, _ := errs.As(err)
	require.NotEmpty(t, e.Fields["raw_ref"], "the offending body is archived")
	require.Equal(t, 1, h.archive.Len())
}

// hangDelay is how long the fixture stalls the first response, and hangTimeout
// is the client deadline set against it. The gap between them is deliberately
// enormous: the assertion is that the client gives up on a server that is not
// answering, never that it gives up within some particular number of
// milliseconds. An earlier version used a 40ms client deadline against a 500ms
// stall, which also applied to the *second*, immediate call in this test — under
// a contended `-race` run that call needed more than 40ms just to cross the
// loopback to the httptest server, so it timed out too and the test failed on
// the require.NoError below. The replay handler selects on the request context,
// so a long stall costs nothing at teardown: it returns the moment the client
// cancels.
const (
	hangDelay   = 30 * time.Second
	hangTimeout = 2 * time.Second
)

func TestContract_Timeout(t *testing.T) {
	t.Parallel()
	r := newReplay(t).onceDelay("getBlockHeight", hangDelay, "getBlockHeight.json").on("getBlockHeight", "getBlockHeight.json")
	h := newHarness(t, r, func(cfg *config.ProviderConfig, opts *solanarpc.Options) {
		cfg.Timeout = hangTimeout
		opts.Retry.MaxAttempts = 1
	})
	_, err := h.client.GetBlockHeight(context.Background())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	require.Contains(t, err.Error(), "timed out")
	require.NotContains(t, err.Error(), r.srv.URL)
	snap := h.client.Tracker().Snapshot(h.clk.Now())
	require.Equal(t, 1, snap.Samples)
	require.Equal(t, int64(10_000), snap.ErrorRateBPS)

	height, err := h.client.GetBlockHeight(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(231000000), height)
}

func TestContract_RateLimit429(t *testing.T) {
	t.Parallel()
	r := newReplay(t).
		onceStatus("getBlockHeight", http.StatusTooManyRequests, fixture(t, "error_429.json"), map[string]string{"Retry-After": "0"}).
		on("getBlockHeight", "getBlockHeight.json")
	h := newHarness(t, r, nil)
	height, err := h.client.GetBlockHeight(context.Background())
	require.NoError(t, err, "retried after the rate limit")
	require.Equal(t, uint64(231000000), height)
	require.Len(t, r.recorded("getBlockHeight"), 2)
	require.Equal(t, 2, h.archive.Len(), "the rate-limit body is archived too")

	r = newReplay(t).onceStatus("getBlockHeight", http.StatusTooManyRequests, fixture(t, "error_429.json"), map[string]string{"Retry-After": "3600"})
	h = newHarness(t, r, nil)
	_, err = h.client.GetBlockHeight(context.Background())
	require.Equal(t, errs.CodeRateLimited, errs.CodeOf(err), "a Retry-After beyond the cap is surfaced, not slept")
	require.Len(t, r.recorded("getBlockHeight"), 1)
}

func TestContract_5xxRetriedThenUnavailable(t *testing.T) {
	t.Parallel()
	r := newReplay(t).
		onceStatus("getBlockHeight", http.StatusServiceUnavailable, fixture(t, "error_503.json"), nil).
		onceStatus("getBlockHeight", http.StatusBadGateway, nil, nil).
		on("getBlockHeight", "getBlockHeight.json")
	h := newHarness(t, r, nil)
	height, err := h.client.GetBlockHeight(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(231000000), height)
	require.Len(t, r.recorded("getBlockHeight"), 3)
	require.Equal(t, provider.Unhealthy, h.client.Health(), "two consecutive failures cross the threshold at MinSamples=2; one success is not a recovery streak")
	require.True(t, h.client.Health().AllowsObservation())

	r = newReplay(t).
		onceStatus("getBlockHeight", http.StatusServiceUnavailable, nil, nil).
		onceStatus("getBlockHeight", http.StatusServiceUnavailable, nil, nil).
		onceStatus("getBlockHeight", http.StatusServiceUnavailable, nil, nil)
	h = newHarness(t, r, nil)
	_, err = h.client.GetBlockHeight(context.Background())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, 3, e.Fields["attempts"])
	require.Equal(t, provider.Unhealthy, h.client.Health())
	require.True(t, h.client.Health().AllowsObservation(), "observation continues while UNHEALTHY (PART 107)")
}

func TestContract_RPCErrorBodies(t *testing.T) {
	t.Parallel()
	r := newReplay(t).on("getTransaction", "rpc_error_invalid_params.json")
	h := newHarness(t, r, nil)
	_, err := h.client.GetTransaction(context.Background(), sigSwap)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	require.Len(t, r.recorded("getTransaction"), 1)

	r = newReplay(t).onceStatus("getBlockHeight", http.StatusOK, fixture(t, "rpc_error_node_unhealthy.json"), nil).on("getBlockHeight", "getBlockHeight.json")
	h = newHarness(t, r, nil)
	height, err := h.client.GetBlockHeight(context.Background())
	require.NoError(t, err, "node unhealthy is retried")
	require.Equal(t, uint64(231000000), height)
	require.Len(t, r.recorded("getBlockHeight"), 2)
}

func TestContract_SearchWalletActivity_Paging(t *testing.T) {
	t.Parallel()
	r := newReplay(t).
		on("getTokenAccountsByOwner", "getTokenAccountsByOwner_empty.json").
		onMatch("getSignaturesForAddress", paramHas(1, "before", sigOlder), "getSignaturesForAddress_page2.json").
		on("getSignaturesForAddress", "getSignaturesForAddress_page1.json").
		on("getTransaction", "getTransaction_v0_swap.json").
		on("getSignatureStatuses", "getSignatureStatuses_finalized.json")
	h := newHarness(t, r, func(_ *config.ProviderConfig, opts *solanarpc.Options) { opts.SignaturesPageSize = 2 })
	// getTransaction replays the swap for every signature; the second
	// signature of page 1 therefore comes back with a mismatching first
	// signature and is rejected — the fetch phase fails closed, but the
	// paging phase has already run and is what this test inspects.
	txs, err := h.client.SearchWalletActivity(context.Background(), wallet, time.Unix(1757000000, 0), 5)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "fixture replay cannot serve a second signature; the mismatch is caught")
	_ = txs
	pages := r.recorded("getSignaturesForAddress")
	require.Len(t, pages, 2, "page 1, then page 2 with before=last signature (page 2 is older than since)")
	var cfg map[string]any
	param(t, pages[1], 1, &cfg)
	require.Equal(t, sigOlder, cfg["before"])
	require.Equal(t, json.Number("2"), cfg["limit"])

	// With since after the older signature, only the swap is fetched.
	r = newReplay(t).
		on("getTokenAccountsByOwner", "getTokenAccountsByOwner_empty.json").
		on("getSignaturesForAddress", "getSignaturesForAddress_page1.json").
		on("getTransaction", "getTransaction_v0_swap.json").
		on("getSignatureStatuses", "getSignatureStatuses_finalized.json")
	h = newHarness(t, r, nil)
	txs, err = h.client.SearchWalletActivity(context.Background(), wallet, time.Unix(1757073595, 0), 1)
	require.NoError(t, err)
	require.Len(t, txs, 1)
	require.Equal(t, sigSwap, txs[0].Signature)
	require.Equal(t, chain.CommitmentFinalized, txs[0].Commitment)
	require.Len(t, r.recorded("getTokenAccountsByOwner"), 2, "Token and Token-2022 programs are both enumerated")
}

func TestContract_BalancesBlockhashAndSimulation(t *testing.T) {
	t.Parallel()
	r := newReplay(t).
		onMatch("getTokenAccountsByOwner", paramHas(1, "mint", mintUSDC), "getTokenAccountsByOwner_usdc.json").
		on("getTokenAccountsByOwner", "getTokenAccountsByOwner_empty.json").
		on("getBalance", "getBalance.json").
		on("getBlockHeight", "getBlockHeight.json").
		on("getLatestBlockhash", "getLatestBlockhash.json").
		on("isBlockhashValid", "isBlockhashValid.json").
		onMatch("simulateTransaction", func(p []json.RawMessage) bool { return len(p) > 0 && string(p[0]) == `"AQ=="` }, "simulateTransaction_failed.json").
		on("simulateTransaction", "simulateTransaction_ok.json")
	h := newHarness(t, r, nil)
	ctx := context.Background()

	bs, err := h.client.GetBalances(ctx, wallet, []string{mintUSDC, mintBONK, chain.MintNativeSOL})
	require.NoError(t, err)
	require.Len(t, bs, 2, "USDC account plus native; no BONK account")
	require.Equal(t, mintUSDC, bs[0].Mint)
	require.Equal(t, "1000000", bs[0].Amount.String())
	require.Equal(t, uint64(250000150), bs[0].Slot)
	require.True(t, bs[0].DecimalsKnown)
	require.Equal(t, chain.MintNativeSOL, bs[1].Mint)
	require.Equal(t, "7955720", bs[1].Amount.String())
	require.Equal(t, uint64(250000151), bs[1].Slot)

	height, err := h.client.GetBlockHeight(ctx)
	require.NoError(t, err)
	bh, err := h.client.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	require.Equal(t, blockhashA, bh.Blockhash)
	require.Equal(t, uint64(231000150), bh.LastValidBlockHeight)
	require.Greater(t, bh.LastValidBlockHeight, height)
	require.Less(t, bh.LastValidBlockHeight, bh.Slot, "block height < slot because skipped slots produce no block")
	ok, err := h.client.IsBlockhashValid(ctx, blockhashA)
	require.NoError(t, err)
	require.True(t, ok)

	keys := []string{wallet, ataUSDC, ataBONK, "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4", poolAcct}
	sim, err := h.client.Simulate(ctx, []byte{2}, chain.SimulateOptions{InnerInstructions: true, AccountKeys: keys})
	require.NoError(t, err)
	require.True(t, sim.OK)
	require.Equal(t, uint64(145000), sim.UnitsConsumed)
	require.Equal(t, []string{"JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"}, sim.InnerProgramIDs)
	sim, err = h.client.Simulate(ctx, []byte{1}, chain.SimulateOptions{})
	require.NoError(t, err)
	require.False(t, sim.OK)
	require.Equal(t, `{"InstructionError":[0,{"Custom":6001}]}`, sim.Err)
	require.Contains(t, sim.Logs[1], "SlippageToleranceExceeded")
}
