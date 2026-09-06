package solanarpc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/chain/chaintest"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
)

// Identifiers used by the fixtures (see README.md).
const (
	sigSwap    = "5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW"
	sigOlder   = "4Wf4kGKv5qLj1Uf7VvNbY5v9F3Zq8bJ5aE4v2n1kY8f3M9p7Qw2tR6sX1cV5bN8mK3jH7gF4dS2aP9oL6iU3yT1e"
	sigOldest  = "3Kx9vLm2pQ7nR4tY6uJ8oP1aS5dF7gH9jK2mZ4xC6vB8nM1qW3eR5tY7uJ9oP2aS4dF6gH8jK1mZ3xC5vB7nM9qW"
	wallet     = "9aE476sH92Vz7DMPyq5WLPkrKWivxnuXaMLh3Gvy8Bev"
	ataUSDC    = "4kJ3Uc9BwYpKcgZfN1VzT8n7mQ6xR2aS5dF7gH9jK2mZ"
	ataBONK    = "7pL2mN4qR6sT8vX1cV3bN5mK7jH9gF2dS4aP6oL8iU1y"
	poolAcct   = "2nZ8vB6mK4jH2gF9dS7aP5oL3iU1yT9eR7wQ5vX3cV1b"
	mintUSDC   = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	mintBONK   = "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263"
	blockhashA = "EkSnNWid2cvwEVnVx9aBqawnmiCNiDgp3gUdkDPTKN1N"
)

// fixture loads a recorded response body.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("fixtures", name))
	require.NoError(t, err, name)
	return b
}

// response is one scripted HTTP answer.
type response struct {
	status  int
	body    []byte
	headers map[string]string
	delay   time.Duration
}

// route matches a JSON-RPC request and returns the response to replay.
type route struct {
	method string
	match  func(params []json.RawMessage) bool
	resp   response
	once   bool
}

// replay serves recorded fixtures. Bodies are replayed verbatim except that
// the JSON-RPC id is rewritten to echo the request id, exactly as a real
// node does.
type replay struct {
	t      *testing.T
	mu     sync.Mutex
	routes []route
	calls  []call
	srv    *httptest.Server
}

type call struct {
	Method string
	Params []json.RawMessage
	Query  string
}

func newReplay(t *testing.T) *replay {
	t.Helper()
	r := &replay{t: t}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *replay) on(method, fixtureName string) *replay {
	return r.add(route{method: method, resp: response{status: 200, body: fixture(r.t, fixtureName)}})
}

func (r *replay) onMatch(method string, match func(params []json.RawMessage) bool, fixtureName string) *replay {
	return r.add(route{method: method, match: match, resp: response{status: 200, body: fixture(r.t, fixtureName)}})
}

// onceStatus answers the next matching call with an HTTP status and body.
func (r *replay) onceStatus(method string, status int, body []byte, headers map[string]string) *replay {
	return r.add(route{method: method, once: true, resp: response{status: status, body: body, headers: headers}})
}

func (r *replay) onceDelay(method string, d time.Duration, fixtureName string) *replay {
	return r.add(route{method: method, once: true, resp: response{status: 200, body: fixture(r.t, fixtureName), delay: d}})
}

// add registers a route; routes are tried in registration order so
// once-only answers registered first are consumed before the fallbacks.
func (r *replay) add(rt route) *replay {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = append(r.routes, rt)
	return r
}

func (r *replay) recorded(method string) []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []call
	for _, c := range r.calls {
		if method == "" || c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (r *replay) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	var rq struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &rq); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.calls = append(r.calls, call{Method: rq.Method, Params: rq.Params, Query: req.URL.RawQuery})
	var chosen *response
	for i := range r.routes {
		rt := &r.routes[i]
		if rt.method != rq.Method || (rt.match != nil && !rt.match(rq.Params)) {
			continue
		}
		resp := rt.resp
		chosen = &resp
		if rt.once {
			r.routes = append(r.routes[:i], r.routes[i+1:]...)
		}
		break
	}
	r.mu.Unlock()
	if chosen == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(rewriteID([]byte(`{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found"},"id":1}`), rq.ID))
		return
	}
	if chosen.delay > 0 {
		select {
		case <-time.After(chosen.delay):
		case <-req.Context().Done():
			return
		}
	}
	for k, v := range chosen.headers {
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(chosen.status)
	_, _ = w.Write(rewriteID(chosen.body, rq.ID))
}

// rewriteID echoes the request id in a JSON-RPC body; non-JSON bodies are
// returned untouched.
func rewriteID(body []byte, id json.RawMessage) []byte {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil || m == nil {
		return body
	}
	if _, ok := m["jsonrpc"]; !ok {
		return body
	}
	m["id"] = id
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func param(t *testing.T, c call, i int, v any) {
	t.Helper()
	require.Greater(t, len(c.Params), i)
	dec := json.NewDecoder(bytes.NewReader(c.Params[i]))
	dec.UseNumber()
	require.NoError(t, dec.Decode(v))
}

// paramHas reports whether the i-th param object has key == value.
func paramHas(i int, key string, value any) func(params []json.RawMessage) bool {
	return func(params []json.RawMessage) bool {
		if len(params) <= i {
			return false
		}
		var m map[string]any
		if err := json.Unmarshal(params[i], &m); err != nil {
			return false
		}
		return m[key] == value
	}
}

type harness struct {
	client  *solanarpc.Client
	archive *chaintest.MemArchive
	clk     *clock.Fake
}

func newHarness(t *testing.T, r *replay, mutate func(cfg *config.ProviderConfig, opts *solanarpc.Options)) *harness {
	t.Helper()
	h := &harness{archive: chaintest.NewMemArchive(), clk: clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))}
	cfg := config.ProviderConfig{Mode: config.ProviderModeSandbox, Name: "rpc-fallback", BaseURL: r.srv.URL, Timeout: 2 * time.Second}
	opts := solanarpc.Options{Retry: solanarpc.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxRetryAfterWait: 50 * time.Millisecond}}
	if mutate != nil {
		mutate(&cfg, &opts)
	}
	th := provider.DefaultThresholds()
	th.MinSamples = 2
	th.MaxStaleness = 0
	c, err := solanarpc.New(context.Background(), cfg, opts, solanarpc.Deps{
		Archive: h.archive, Clock: h.clk, Thresholds: &th, Logger: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	h.client = c
	return h
}
