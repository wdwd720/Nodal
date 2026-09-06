package helius_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/chain/chaintest"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider/helius"
)

const streamTimeout = 10 * time.Second

// runStream starts StreamFrom and returns the result channel.
func runStream(ctx context.Context, c *helius.Client, sink func(chain.WalletEvent) error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.StreamFrom(ctx, []string{wallet}, time.Unix(1757000000, 0), sink) }()
	return done
}

func waitFor(t *testing.T, events func() []chain.WalletEvent, n int, why string) []chain.WalletEvent {
	t.Helper()
	deadline := time.Now().Add(streamTimeout)
	for time.Now().Before(deadline) {
		if evs := events(); len(evs) >= n {
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d events (%s); got %d", n, why, len(events()))
	return nil
}

func TestStream_HintsBecomeRPCObservations(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	quietRPC(s)
	c, d := newClient(t, s, nil)
	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	defer cancel()
	sink, events := chaintest.Collect(0)
	done := runStream(ctx, c, sink)

	sub := <-s.subs
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	require.NoError(t, json.Unmarshal(sub, &req))
	require.Equal(t, "transactionSubscribe", req.Method)
	var filter, options map[string]any
	require.NoError(t, json.Unmarshal(req.Params[0], &filter))
	require.NoError(t, json.Unmarshal(req.Params[1], &options))
	require.Equal(t, []any{wallet}, filter["accountInclude"])
	require.Equal(t, "balanceChanged", filter["tokenAccounts"])
	require.Equal(t, false, filter["vote"])
	require.Equal(t, true, filter["failed"], "failed transactions still cost fees")
	require.Equal(t, "confirmed", options["commitment"])
	require.Equal(t, float64(0), options["maxSupportedTransactionVersion"])

	s.notify <- notification(sigA, 250000123)
	evs := waitFor(t, events, 2, "reconnect + stream event")
	require.Equal(t, chain.EventReconnect, evs[0].Kind)
	require.Equal(t, "helius", evs[0].Source)
	require.Equal(t, "connected", evs[0].Detail)
	ev := evs[1]
	require.Equal(t, chain.EventTransaction, ev.Kind)
	require.Equal(t, chain.OriginStream, ev.Origin)
	require.Equal(t, wallet, ev.Wallet)
	require.True(t, ev.Observation.Found, "the event is the RPC observation, not the notification")
	require.Equal(t, sigA, ev.Observation.Signature)
	require.Equal(t, "helius", ev.Observation.Source)
	require.NotEmpty(t, ev.Observation.RawRef, "archived before use")
	require.Equal(t, "-1000000", ev.Observation.DeltasFor(wallet)[0].Delta().String())
	require.NotNil(t, ev.Sequence)
	require.Equal(t, chain.SequenceOf(250000123, 0), *ev.Sequence)
	require.Nil(t, ev.ProviderPublishedAt, "no provider timestamp in the notification")
	require.Equal(t, d.clk.Now(), ev.ReceivedAt)

	// The same signature again is deduplicated; an unparseable frame and
	// a notification for another wallet are ignored (backfill covers gaps).
	s.notify <- notification(sigA, 250000123)
	s.notify <- json.RawMessage(`{"jsonrpc":"2.0","method":"transactionNotification","params":{"result":{"nothing":true}}}`)
	s.notify <- json.RawMessage(`garbage`)
	s.notify <- json.RawMessage(`{"jsonrpc":"2.0","id":9,"error":{"code":-32602,"message":"plan gated"}}`)
	s.notify <- notification(sigB, 250000124)
	evs = waitFor(t, events, 3, "second stream event")
	require.Equal(t, sigB, evs[2].Observation.Signature)
	require.Len(t, events(), 3)
	require.Len(t, s.recorded("getTransaction"), 2, "one RPC read per new signature")

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, 1, s.connections())
}

func TestStream_ReconnectAnnouncedAndBackfilled(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	quietRPC(s)
	s.mu.Lock()
	s.wsCloseIn = 20 * time.Millisecond
	s.mu.Unlock()
	c, _ := newClient(t, s, nil)
	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	defer cancel()
	sink, events := chaintest.Collect(0)
	done := runStream(ctx, c, sink)
	evs := waitFor(t, events, 2, "two connections")
	require.Equal(t, chain.EventReconnect, evs[0].Kind)
	require.Equal(t, chain.EventReconnect, evs[1].Kind)
	require.Contains(t, evs[1].Detail, "reconnected after 1")
	require.Contains(t, evs[1].Detail, "backfill requested")
	require.GreaterOrEqual(t, s.connections(), 2)
	deadline := time.Now().Add(streamTimeout)
	for len(s.recorded("getSignaturesForAddress")) < 2 {
		require.True(t, time.Now().Before(deadline), "every connection triggers a backfill")
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestStream_PollingWhenSocketUnavailable(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"disabled", "refused", "dial-error"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s := newServer(t)
			quietRPC(s)
			c, d := newClient(t, s, func(_ *config.ProviderConfig, opts *helius.Options, hd *helius.Deps) {
				switch mode {
				case "disabled":
					opts.Stream.DisableWebSocket = true
				case "dial-error":
					hd.Dialer = failingDialer{}
				}
			})
			if mode == "refused" {
				s.mu.Lock()
				s.wsAccept = false
				s.mu.Unlock()
			}
			ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
			defer cancel()
			sink, events := chaintest.Collect(0)
			done := runStream(ctx, c, sink)
			// Backfill finds nothing at first; then activity appears.
			time.Sleep(40 * time.Millisecond)
			s.handle("getSignaturesForAddress", func(rpcCall) (any, *rpcErr) {
				return []any{sigItem(sigB, 250000124, 1757073601), sigItem(sigA, 250000123, 1757073600)}, nil
			})
			evs := waitFor(t, events, 2, "backfill events")
			require.Equal(t, chain.EventTransaction, evs[0].Kind)
			require.Equal(t, chain.OriginBackfill, evs[0].Origin)
			require.Equal(t, sigA, evs[0].Observation.Signature, "oldest first")
			require.Equal(t, sigB, evs[1].Observation.Signature)
			require.Equal(t, wallet, evs[0].Wallet)
			time.Sleep(80 * time.Millisecond) // several more polls
			require.Len(t, events(), 2, "polls never re-deliver")
			cancel()
			<-done
			if mode == "dial-error" {
				require.Contains(t, d.logs.String(), "stream dial failed")
				require.NotContains(t, d.logs.String(), apiKey)
			}
			if mode == "disabled" {
				require.Equal(t, 0, s.connections())
			}
		})
	}
}

func TestStream_SinkErrorStopsEverything(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	quietRPC(s)
	c, _ := newClient(t, s, nil)
	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	defer cancel()
	sink, _ := chaintest.Collect(1)
	err := <-runStream(ctx, c, sink)
	require.ErrorIs(t, err, chaintest.ErrStop)
	require.NoError(t, ctx.Err(), "the stream stopped because of the sink, not the context")
}

func TestStream_Validation(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	c, _ := newClient(t, s, nil)
	ctx := context.Background()
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(c.StreamWalletEvents(ctx, nil, func(chain.WalletEvent) error { return nil })))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(c.StreamWalletEvents(ctx, []string{"bad"}, func(chain.WalletEvent) error { return nil })))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(c.StreamWalletEvents(ctx, []string{wallet}, nil)))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	quietRPC(s)
	require.ErrorIs(t, c.StreamWalletEvents(cctx, []string{wallet, wallet}, func(chain.WalletEvent) error { return nil }), context.Canceled)
}

func TestStream_BackfillProviderFailureIsRetried(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	quietRPC(s)
	s.handle("getSignaturesForAddress", func(rpcCall) (any, *rpcErr) { return nil, &rpcErr{Code: -32005, Message: "unhealthy"} })
	c, d := newClient(t, s, func(_ *config.ProviderConfig, opts *helius.Options, _ *helius.Deps) {
		opts.Stream.DisableWebSocket = true
	})
	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	defer cancel()
	sink, events := chaintest.Collect(0)
	done := runStream(ctx, c, sink)
	deadline := time.Now().Add(streamTimeout)
	for !strings.Contains(d.logs.String(), "stream backfill failed") {
		require.True(t, time.Now().Before(deadline), "backfill failure was never logged")
		time.Sleep(5 * time.Millisecond)
	}
	s.handle("getSignaturesForAddress", func(rpcCall) (any, *rpcErr) { return []any{sigItem(sigA, 250000123, 1757073600)}, nil })
	evs := waitFor(t, events, 1, "backfill after recovery")
	require.Equal(t, sigA, evs[0].Observation.Signature)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

type failingDialer struct{}

func (failingDialer) Dial(context.Context, string, string, time.Duration) (helius.Conn, error) {
	return nil, errors.New("dial refused")
}
