package helius_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/chain/chaintest"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider/helius"
)

func TestContract_AuthQueryParamAndRedaction(t *testing.T) {
	t.Parallel()
	r := newReplay(t).on("getTransaction", "getTransaction_v0_swap.json").on("getSignatureStatuses", "getSignatureStatuses_finalized.json")
	h := newHarness(t, r, apiKey, nil)
	obs, err := h.client.GetTransaction(context.Background(), sigSwap)
	require.NoError(t, err)
	require.True(t, obs.Found)
	require.Equal(t, chain.CommitmentFinalized, obs.Commitment)
	require.Equal(t, "helius", obs.Source)
	require.Equal(t, "api-key="+apiKey, r.recorded("getTransaction")[0].Query)
	require.NotContains(t, h.client.RPC().RedactedEndpoint(), apiKey)
	require.NotContains(t, h.client.RedactedWSURL(), apiKey)
	require.Equal(t, "-1000000", obs.DeltasFor(wallet)[0].Delta().String())

	bad := newHarness(t, r, "WRONG-KEY", nil)
	_, err = bad.client.GetTransaction(context.Background(), sigSwap)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, http.StatusUnauthorized, e.Fields["http_status"])
	require.Equal(t, 1, e.Fields["attempts"], "credential rejections are not retried")
	require.NotContains(t, err.Error(), "WRONG-KEY")
	require.Equal(t, 1, bad.archive.Len(), "the 401 body is archived as evidence")
}

func TestContract_DASBalancesExact(t *testing.T) {
	t.Parallel()
	r := newReplay(t).on("getTokenAccounts", "getTokenAccounts_page1.json").on("getBalance", "getBalance.json")
	h := newHarness(t, r, apiKey, nil)
	bs, err := h.client.GetBalances(context.Background(), wallet, nil)
	require.NoError(t, err)
	require.Len(t, bs, 4, "three DAS accounts plus native")
	byMint := map[string]chain.BalanceObservation{}
	for _, b := range bs {
		byMint[b.Mint] = b
	}
	require.Equal(t, "1000000", byMint[mintUSDC].Amount.String())
	require.Equal(t, ataUSDC, byMint[mintUSDC].TokenAccount)
	require.Equal(t, "123456789", byMint[mintBONK].Amount.String())
	require.Equal(t, "18446744073709551617", byMint[mintWSOL].Amount.String(), "JSON number above 2^64 parsed exactly")
	require.False(t, byMint[mintUSDC].DecimalsKnown, "DAS reports no decimals")
	require.Equal(t, uint64(0), byMint[mintUSDC].Slot, "DAS reports no slot")
	require.Equal(t, "helius", byMint[mintUSDC].Source)
	require.NotEmpty(t, byMint[mintUSDC].RawRef)
	require.Equal(t, "7955720", byMint[chain.MintNativeSOL].Amount.String())
	require.True(t, byMint[chain.MintNativeSOL].DecimalsKnown)
	require.Equal(t, uint64(250000151), byMint[chain.MintNativeSOL].Slot)

	p := namedParams(t, r.recorded("getTokenAccounts")[0])
	require.Equal(t, wallet, p["owner"])
	require.Equal(t, json.Number("1"), p["page"])
	require.Equal(t, json.Number("1000"), p["limit"])
	require.Equal(t, map[string]any{"showZeroBalance": true}, p["options"])

	bs, err = h.client.GetBalances(context.Background(), wallet, []string{mintBONK})
	require.NoError(t, err)
	require.Len(t, bs, 1)
	require.Len(t, r.recorded("getBalance"), 1, "native only when requested or unfiltered")

	r = newReplay(t).on("getTokenAccounts", "getTokenAccounts_float_amount.json")
	h = newHarness(t, r, apiKey, nil)
	_, err = h.client.GetBalances(context.Background(), wallet, []string{mintUSDC})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a float amount is rejected, never rounded")
	require.Len(t, r.recorded("getTokenAccounts"), 1)
}

func TestContract_RateLimitBody(t *testing.T) {
	t.Parallel()
	r := newReplay(t).
		onceStatus("getBlockHeight", http.StatusTooManyRequests, "error_429.json", map[string]string{"Retry-After": "0"})
	// No success fixture on purpose: the retry hits the replay's default
	// "method not found" reply, which proves the 429 was retried once.
	h := newHarness(t, r, apiKey, nil)
	_, err := h.client.GetBlockHeight(context.Background())
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	require.Len(t, r.recorded("getBlockHeight"), 2, "429 retried once within the Retry-After cap")
}

func TestContract_StreamNotificationIsOnlyAHint(t *testing.T) {
	t.Parallel()
	r := newReplay(t).
		on("getTokenAccountsByOwner", "getTokenAccountsByOwner_empty.json").
		on("getSignaturesForAddress", "getSignaturesForAddress_empty.json").
		on("getTransaction", "getTransaction_v0_swap.json").
		on("getSignatureStatuses", "getSignatureStatuses_finalized.json").
		ws("transactionSubscribe_ack.json", "transactionNotification.json")
	h := newHarness(t, r, apiKey, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sink, events := chaintest.Collect(2)
	err := h.client.StreamWalletEvents(ctx, []string{wallet}, sink)
	require.ErrorIs(t, err, chaintest.ErrStop)
	evs := events()
	require.Len(t, evs, 2)
	require.Equal(t, chain.EventReconnect, evs[0].Kind)
	ev := evs[1]
	require.Equal(t, chain.EventTransaction, ev.Kind)
	require.Equal(t, chain.OriginStream, ev.Origin)
	require.Equal(t, wallet, ev.Wallet)
	require.Equal(t, sigSwap, ev.Observation.Signature)
	require.True(t, ev.Observation.Found)
	require.Equal(t, chain.CommitmentFinalized, ev.Observation.Commitment, "commitment comes from the RPC status read, not the socket")
	require.Equal(t, "123456789", ev.Observation.DeltasFor(wallet)[1].Delta().String(), "deltas come from RPC, not the notification's meta")
	require.NotEmpty(t, ev.Observation.RawRef)
	require.Len(t, r.recorded("getTransaction"), 1, "the notification triggered exactly one RPC read")
	require.Equal(t, 1, r.connections())
	require.Nil(t, ev.ProviderPublishedAt)
	require.Equal(t, h.clk.Now(), ev.ReceivedAt)
}

func TestContract_StreamPlanGatedFallsBackToPolling(t *testing.T) {
	t.Parallel()
	r := newReplay(t).
		on("getTokenAccountsByOwner", "getTokenAccountsByOwner_empty.json").
		on("getSignaturesForAddress", "getSignaturesForAddress_one.json").
		on("getTransaction", "getTransaction_v0_swap.json").
		on("getSignatureStatuses", "getSignatureStatuses_finalized.json").
		ws("transactionSubscribe_error.json")
	h := newHarness(t, r, apiKey, func(opts *helius.Options) { opts.Stream.Lookback = 365 * 24 * time.Hour })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sink, events := chaintest.Collect(2)
	err := h.client.StreamWalletEvents(ctx, []string{wallet}, sink)
	require.ErrorIs(t, err, chaintest.ErrStop)
	evs := events()
	var tx *chain.WalletEvent
	for i := range evs {
		if evs[i].Kind == chain.EventTransaction {
			tx = &evs[i]
		}
	}
	require.NotNil(t, tx, "polling delivered the transaction although the subscription was rejected")
	require.Equal(t, chain.OriginBackfill, tx.Origin)
	require.Equal(t, sigSwap, tx.Observation.Signature)
	require.GreaterOrEqual(t, r.connections(), 1)
}
