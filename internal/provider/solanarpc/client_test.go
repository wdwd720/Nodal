package solanarpc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
)

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	ctx := context.Background()
	base := func() (config.ProviderConfig, solanarpc.Options, solanarpc.Deps) {
		_, td := newClient(t, s, nil)
		return config.ProviderConfig{Mode: config.ProviderModeSandbox, BaseURL: s.URL()}, solanarpc.Options{}, solanarpc.Deps{Archive: td.archive}
	}
	cfg, opts, deps := base()
	cfg.Mode = config.ProviderModeFake
	_, err := solanarpc.New(ctx, cfg, opts, deps)
	require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err), "fake mode is never constructed here")

	cfg, opts, deps = base()
	cfg.Mode = config.ProviderModeLive
	cfg.BaseURL = ""
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "explicit BaseURL")
	cfg.BaseURL = "http://rpc.example.com"
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "https")
	cfg.BaseURL = "https://rpc.example.com"
	c, err := solanarpc.New(ctx, cfg, opts, deps)
	require.NoError(t, err)
	require.Equal(t, solanarpc.DefaultName, c.Name())
	require.Equal(t, provider.CodeComplete, c.VerificationLabel())
	require.Equal(t, provider.Healthy, c.Health())
	require.Equal(t, chain.CommitmentConfirmed, c.Commitment())
	require.NotNil(t, c.Tracker())

	cfg, opts, deps = base()
	cfg.Mode = "weird"
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.Error(t, err)

	cfg, opts, deps = base()
	cfg.BaseURL = "not a url"
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.Error(t, err)

	cfg, opts, deps = base()
	cfg.BaseURL = ""
	c, err = solanarpc.New(ctx, cfg, opts, deps)
	require.NoError(t, err)
	require.Equal(t, "https://api.devnet.solana.com", c.RedactedEndpoint(), "sandbox defaults to devnet")

	cfg, opts, deps = base()
	deps.Archive = nil
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "archive")

	cfg, opts, deps = base()
	opts.Commitment = chain.CommitmentProcessed
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "commitment")

	cfg, opts, deps = base()
	cfg.APIKeyRef = "env://KEY"
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "resolver")
	deps.Resolver = config.EnvResolver{Lookup: func(string) (string, bool) { return "k", true }}
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "exactly one of AuthHeader or AuthQueryParam")
	opts.AuthHeader, opts.AuthQueryParam = "X-Key", "key"
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "exactly one of AuthHeader or AuthQueryParam")
	opts.AuthQueryParam = ""
	deps.Resolver = config.EnvResolver{Lookup: func(string) (string, bool) { return "", false }}
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "resolve api key")
	deps.Resolver = config.EnvResolver{Lookup: func(string) (string, bool) { return "  ", true }}
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "empty")

	cfg, opts, deps = base()
	bad := provider.Thresholds{}
	deps.Thresholds = &bad
	_, err = solanarpc.New(ctx, cfg, opts, deps)
	require.Error(t, err, "invalid thresholds")
}

func TestCall_RetriesOn5xxThenSucceeds(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	var n atomic.Int32
	s.intercept(func(w http.ResponseWriter, _ *http.Request) bool {
		if n.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"overloaded"}`))
			return true
		}
		return false
	})
	s.handle("getBlockHeight", func(rpcCall) (any, *rpcErr) { return json.Number("123"), nil })
	c, td := newClient(t, s, nil)
	h, err := c.GetBlockHeight(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(123), h)
	require.Equal(t, int32(3), n.Load())
	snap := c.Tracker().Snapshot(td.clk.Now())
	require.Equal(t, 3, snap.Samples, "every attempt is a health sample")
	require.Equal(t, int64(6666), snap.ErrorRateBPS)
	require.Equal(t, 3, td.archive.Len(), "every response body is archived, errors included")
	require.Contains(t, td.logs.String(), "rpc retry")
}

func TestCall_ExhaustedRetriesIsProviderUnavailable(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	s.intercept(func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusBadGateway)
		return true
	})
	c, _ := newClient(t, s, nil)
	_, err := c.GetBlockHeight(context.Background())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, 3, e.Fields["attempts"])
	require.Equal(t, 502, e.Fields["http_status"])
	require.Equal(t, "getBlockHeight", e.Fields["method"])
}

func TestCall_RateLimited(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	var n atomic.Int32
	s.intercept(func(w http.ResponseWriter, _ *http.Request) bool {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":429,"message":"Too many requests"},"id":1}`))
			return true
		}
		return false
	})
	s.handle("getBlockHeight", func(rpcCall) (any, *rpcErr) { return json.Number("5"), nil })
	c, _ := newClient(t, s, nil)
	h, err := c.GetBlockHeight(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(5), h)
	require.Equal(t, int32(2), n.Load())

	// A Retry-After beyond the cap is not waited for.
	n.Store(0)
	s.intercept(func(w http.ResponseWriter, _ *http.Request) bool {
		n.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})
	_, err = c.GetBlockHeight(context.Background())
	require.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	require.Equal(t, int32(1), n.Load())
}

func TestCall_TimeoutIsProviderUnavailableAndRedacted(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	release := make(chan struct{})
	defer close(release)
	s.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return true
	})
	c, td := newClient(t, s, func(cfg *config.ProviderConfig, opts *solanarpc.Options, deps *solanarpc.Deps) {
		cfg.Timeout = 30 * time.Millisecond
		opts.Retry.MaxAttempts = 2
		cfg.APIKeyRef = "env://K"
		opts.AuthQueryParam = "api-key"
		deps.Resolver = config.EnvResolver{Lookup: func(string) (string, bool) { return "SUPERSECRET", true }}
	})
	_, err := c.GetBlockHeight(context.Background())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	require.Contains(t, err.Error(), "timed out")
	require.NotContains(t, err.Error(), "SUPERSECRET")
	require.NotContains(t, err.Error(), s.URL())
	require.NotContains(t, td.logs.String(), "SUPERSECRET")
	require.NotContains(t, c.RedactedEndpoint(), "SUPERSECRET")
	e, _ := errs.As(err)
	require.Equal(t, 2, e.Fields["attempts"], "timeouts are retried while the caller's context lives")
	require.Equal(t, provider.Healthy, c.Health(), "below MinSamples the state holds")

	// Caller cancellation is not retried.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.GetBlockHeight(ctx)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	e, _ = errs.As(err)
	require.Equal(t, 1, e.Fields["attempts"])
}

func TestCall_AuthPlacement(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	s.handle("getBlockHeight", func(rpcCall) (any, *rpcErr) { return json.Number("1"), nil })
	resolver := config.EnvResolver{Lookup: func(string) (string, bool) { return "KEY123", true }}
	q, _ := newClient(t, s, func(cfg *config.ProviderConfig, opts *solanarpc.Options, deps *solanarpc.Deps) {
		cfg.APIKeyRef = "env://K"
		opts.AuthQueryParam = "api-key"
		deps.Resolver = resolver
	})
	_, err := q.GetBlockHeight(context.Background())
	require.NoError(t, err)
	calls := s.recorded("getBlockHeight")
	require.Equal(t, "api-key=KEY123", calls[0].Query)
	require.Equal(t, s.URL(), q.RedactedEndpoint(), "query stripped from the redacted endpoint")

	h, _ := newClient(t, s, func(cfg *config.ProviderConfig, opts *solanarpc.Options, deps *solanarpc.Deps) {
		cfg.APIKeyRef = "env://K"
		opts.AuthHeader = "X-Api-Key"
		deps.Resolver = resolver
	})
	_, err = h.GetBlockHeight(context.Background())
	require.NoError(t, err)
	calls = s.recorded("getBlockHeight")
	require.Equal(t, "KEY123", calls[1].Header.Get("X-Api-Key"))
	require.Equal(t, "", calls[1].Query)
	require.Equal(t, "application/json", calls[1].Header.Get("Content-Type"))
}

func TestCall_RPCErrorMapping(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	var n atomic.Int32
	s.handle("getBlockHeight", func(rpcCall) (any, *rpcErr) {
		switch n.Add(1) {
		case 1:
			return nil, &rpcErr{Code: -32602, Message: "Invalid params"}
		case 2:
			return nil, &rpcErr{Code: -32005, Message: "Node is unhealthy"}
		case 3:
			return json.Number("9"), nil
		default:
			return nil, &rpcErr{Code: -32000, Message: "Something else"}
		}
	})
	c, _ := newClient(t, s, nil)
	_, err := c.GetBlockHeight(context.Background())
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "request-side rpc errors are not retried")
	e, _ := errs.As(err)
	require.Equal(t, 1, e.Fields["attempts"])
	require.Equal(t, int64(-32602), e.Fields["rpc_code"])

	h, err := c.GetBlockHeight(context.Background())
	require.NoError(t, err, "node unhealthy is retried")
	require.Equal(t, uint64(9), h)

	_, err = c.GetBlockHeight(context.Background())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	e, _ = errs.As(err)
	require.Equal(t, 1, e.Fields["attempts"], "unknown rpc errors are not retried")

	// Method not found (scripted server default).
	_, err = c.GetLatestBlockhash(context.Background())
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestCall_MalformedEnvelopes(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	bodies := []string{
		`{"jsonrpc":"2.0","id":999,"result":1}`,
		`{"jsonrpc":"1.0","id":1,"result":1}`,
		`not json`,
		`{"jsonrpc":"2.0","id":1,"result":1} trailing`,
	}
	var n atomic.Int32
	s.intercept(func(w http.ResponseWriter, _ *http.Request) bool {
		i := int(n.Add(1)) - 1
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bodies[i%len(bodies)]))
		return true
	})
	c, td := newClient(t, s, nil)
	for range bodies {
		_, err := c.GetBlockHeight(context.Background())
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		e, _ := errs.As(err)
		require.Equal(t, 1, e.Fields["attempts"], "malformed responses are never retried")
	}
	require.Equal(t, provider.Unhealthy, c.Health(), "malformed answers count against health")
	require.Equal(t, len(bodies), td.archive.Len())
}

func TestCall_ArchiveFailureRefusesObservation(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	s.handle("getBlockHeight", func(rpcCall) (any, *rpcErr) { return json.Number("1"), nil })
	c, td := newClient(t, s, nil)
	td.archive.Err = errs.New(errs.CodeInternal, "bucket down")
	_, err := c.GetBlockHeight(context.Background())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	require.Contains(t, err.Error(), "raw archive unavailable")
	td.archive.Err = nil
	h, err := c.GetBlockHeight(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(1), h)
	obj, ok := td.archive.Get(td.archive.Refs()[0])
	require.True(t, ok)
	require.Equal(t, "rpc-fallback", obj.Provider)
	require.Equal(t, "getBlockHeight", obj.EventType)
	require.Contains(t, string(obj.Body), `"result":1`)
	require.Len(t, obj.DedupKey, 64)
	require.Equal(t, td.clk.Now(), obj.PlatformReceivedAt)
	require.Nil(t, obj.ProviderPublishedAt)
}

func TestCall_OversizedBody(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	s.intercept(func(w http.ResponseWriter, _ *http.Request) bool {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + strings.Repeat("x", 17<<20) + `"}`))
		return true
	})
	c, _ := newClient(t, s, nil)
	_, err := c.GetBlockHeight(context.Background())
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestHealth_TransitionsFromSamples(t *testing.T) {
	t.Parallel()
	s := newRPCServer(t)
	var fail atomic.Bool
	s.intercept(func(w http.ResponseWriter, _ *http.Request) bool {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})
	s.handle("getBlockHeight", func(rpcCall) (any, *rpcErr) { return json.Number("1"), nil })
	c, td := newClient(t, s, func(_ *config.ProviderConfig, opts *solanarpc.Options, _ *solanarpc.Deps) {
		opts.Retry.MaxAttempts = 1
	})
	require.Equal(t, provider.Healthy, c.Health())
	fail.Store(true)
	for i := 0; i < 3; i++ {
		_, err := c.GetBlockHeight(context.Background())
		require.Error(t, err)
		td.clk.Advance(time.Second)
	}
	require.Equal(t, provider.Unhealthy, c.Health())
	require.True(t, c.Health().AllowsObservation(), "unhealthy still observes (PART 107)")
	require.False(t, c.Health().AllowsNewActions())
	fail.Store(false)
	td.clk.Advance(61 * time.Second) // failures leave the sampling window
	for i := 0; i < 3; i++ {         // MinSamples successes before the state may improve
		_, err := c.GetBlockHeight(context.Background())
		require.NoError(t, err)
		td.clk.Advance(time.Second)
	}
	require.Equal(t, provider.Degraded, c.Health(), "recovery steps one level per RecoverStreak")
	for i := 0; i < 2; i++ {
		_, err := c.GetBlockHeight(context.Background())
		require.NoError(t, err)
		td.clk.Advance(time.Second)
	}
	require.Equal(t, provider.Healthy, c.Health(), "recovers through DEGRADED with hysteresis")
	c.Tracker().Disable(td.clk.Now(), "operator")
	require.Equal(t, provider.Disabled, c.Health())
	require.False(t, c.Health().AllowsObservation())
}
