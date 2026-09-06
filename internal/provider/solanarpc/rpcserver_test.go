package solanarpc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// rpcErr is a JSON-RPC error a scripted handler can return.
type rpcErr struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

// rpcCall is one recorded request.
type rpcCall struct {
	Method string
	Params []json.RawMessage
	Header http.Header
	Query  string
}

// rpcServer is a scripted JSON-RPC server. Handlers receive raw params and
// return a result or an RPC error; Before lets a test override the whole
// HTTP response (status/body) for the next calls.
type rpcServer struct {
	t        *testing.T
	mu       sync.Mutex
	handlers map[string]func(call rpcCall) (any, *rpcErr)
	calls    []rpcCall
	before   func(w http.ResponseWriter, r *http.Request) bool // return true when handled
	srv      *httptest.Server
}

func newRPCServer(t *testing.T) *rpcServer {
	t.Helper()
	s := &rpcServer{t: t, handlers: map[string]func(rpcCall) (any, *rpcErr){}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *rpcServer) URL() string { return s.srv.URL }

func (s *rpcServer) handle(method string, fn func(call rpcCall) (any, *rpcErr)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = fn
}

func (s *rpcServer) intercept(fn func(w http.ResponseWriter, r *http.Request) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.before = fn
}

func (s *rpcServer) recorded(method string) []rpcCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []rpcCall
	for _, c := range s.calls {
		if method == "" || c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (s *rpcServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	before := s.before
	s.mu.Unlock()
	if before != nil && before(w, r) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		JSONRPC string            `json:"jsonrpc"`
		ID      json.RawMessage   `json:"id"`
		Method  string            `json:"method"`
		Params  []json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	call := rpcCall{Method: req.Method, Params: req.Params, Header: r.Header.Clone(), Query: r.URL.RawQuery}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	h := s.handlers[req.Method]
	s.mu.Unlock()
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if h == nil {
		resp["error"] = rpcErr{Code: -32601, Message: "Method not found"}
	} else if result, rerr := h(call); rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// param decodes the i-th param of a call into v.
func param(t *testing.T, call rpcCall, i int, v any) {
	t.Helper()
	require.Greater(t, len(call.Params), i)
	dec := json.NewDecoder(bytes.NewReader(call.Params[i]))
	dec.UseNumber()
	require.NoError(t, dec.Decode(v))
}

type testDeps struct {
	archive *chaintest.MemArchive
	clk     *clock.Fake
	logs    *bytes.Buffer
}

// newClient builds a sandbox client against the scripted server.
func newClient(t *testing.T, s *rpcServer, mutate func(cfg *config.ProviderConfig, opts *solanarpc.Options, deps *solanarpc.Deps)) (*solanarpc.Client, *testDeps) {
	t.Helper()
	td := &testDeps{archive: chaintest.NewMemArchive(), clk: clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)), logs: &bytes.Buffer{}}
	cfg := config.ProviderConfig{Mode: config.ProviderModeSandbox, Name: "rpc-fallback", BaseURL: s.URL(), Timeout: 2 * time.Second}
	opts := solanarpc.Options{Retry: solanarpc.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxRetryAfterWait: 50 * time.Millisecond}}
	th := provider.DefaultThresholds()
	th.MinSamples = 3
	th.RecoverStreak = 2
	th.MaxStaleness = 0
	deps := solanarpc.Deps{Archive: td.archive, Clock: td.clk, Thresholds: &th, Logger: slog.New(slog.NewTextHandler(td.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	if mutate != nil {
		mutate(&cfg, &opts, &deps)
	}
	c, err := solanarpc.New(context.Background(), cfg, opts, deps)
	require.NoError(t, err)
	return c, td
}
