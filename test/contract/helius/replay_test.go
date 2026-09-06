package helius_test

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
	"golang.org/x/net/websocket"

	"github.com/nodal/controlplane/internal/chain/chaintest"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/helius"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
)

const (
	apiKey   = "HELIUS-CONTRACT-KEY"
	sigSwap  = "5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW"
	wallet   = "9aE476sH92Vz7DMPyq5WLPkrKWivxnuXaMLh3Gvy8Bev"
	ataUSDC  = "4kJ3Uc9BwYpKcgZfN1VzT8n7mQ6xR2aS5dF7gH9jK2mZ"
	ataBONK  = "7pL2mN4qR6sT8vX1cV3bN5mK7jH9gF2dS4aP6oL8iU1y"
	mintUSDC = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	mintBONK = "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263"
	mintWSOL = "So11111111111111111111111111111111111111112"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("fixtures", name))
	require.NoError(t, err, name)
	return b
}

type response struct {
	status  int
	body    []byte
	headers map[string]string
}

type route struct {
	method string
	resp   response
	once   bool
}

type call struct {
	Method string
	Params json.RawMessage
	Query  string
}

// replay serves recorded JSON-RPC fixtures and a scripted WebSocket:
// after the subscription request it replays wsAck and then every frame in
// wsFrames, then keeps the socket open until the client closes it.
type replay struct {
	t       *testing.T
	mu      sync.Mutex
	routes  []route
	calls   []call
	wsAck   []byte
	wsFrame [][]byte
	wsConns int
	srv     *httptest.Server
}

func newReplay(t *testing.T) *replay {
	t.Helper()
	r := &replay{t: t, wsAck: fixture(t, "transactionSubscribe_ack.json")}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *replay) on(method, fixtureName string) *replay {
	return r.add(route{method: method, resp: response{status: 200, body: fixture(r.t, fixtureName)}})
}

func (r *replay) onceStatus(method string, status int, fixtureName string, headers map[string]string) *replay {
	return r.add(route{method: method, once: true, resp: response{status: status, body: fixture(r.t, fixtureName), headers: headers}})
}

// add registers a route; routes are tried in registration order so
// once-only answers registered first are consumed before the fallbacks.
func (r *replay) add(rt route) *replay {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = append(r.routes, rt)
	return r
}

func (r *replay) ws(ack string, frames ...string) *replay {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wsAck = fixture(r.t, ack)
	r.wsFrame = nil
	for _, f := range frames {
		r.wsFrame = append(r.wsFrame, fixture(r.t, f))
	}
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

func (r *replay) connections() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.wsConns
}

func (r *replay) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Query().Get("api-key") != apiKey {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(fixture(r.t, "error_401.json"))
		return
	}
	if req.Header.Get("Upgrade") == "websocket" {
		websocket.Handler(r.serveWS).ServeHTTP(w, req)
		return
	}
	body, _ := io.ReadAll(req.Body)
	var rq struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
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
		if rt.method != rq.Method {
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
	w.Header().Set("Content-Type", "application/json")
	if chosen == nil {
		_, _ = w.Write(rewriteID([]byte(`{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found"},"id":1}`), rq.ID))
		return
	}
	for k, v := range chosen.headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(chosen.status)
	_, _ = w.Write(rewriteID(chosen.body, rq.ID))
}

func (r *replay) serveWS(ws *websocket.Conn) {
	defer func() { _ = ws.Close() }()
	r.mu.Lock()
	r.wsConns++
	ack, frames := r.wsAck, r.wsFrame
	r.mu.Unlock()
	var raw []byte
	if err := websocket.Message.Receive(ws, &raw); err != nil {
		return
	}
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(raw, &req)
	if err := websocket.Message.Send(ws, string(rewriteID(ack, req.ID))); err != nil {
		return
	}
	for _, f := range frames {
		if err := websocket.Message.Send(ws, string(f)); err != nil {
			return
		}
	}
	var b []byte
	for { // until the client closes
		if err := websocket.Message.Receive(ws, &b); err != nil {
			return
		}
	}
}

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
	if _, ok := m["method"]; ok {
		return body // notifications carry no id
	}
	m["id"] = id
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func namedParams(t *testing.T, c call) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(c.Params))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&m))
	return m
}

type harness struct {
	client  *helius.Client
	archive *chaintest.MemArchive
	clk     *clock.Fake
}

func newHarness(t *testing.T, r *replay, key string, mutate func(opts *helius.Options)) *harness {
	t.Helper()
	h := &harness{archive: chaintest.NewMemArchive(), clk: clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))}
	cfg := config.ProviderConfig{Mode: config.ProviderModeSandbox, BaseURL: r.srv.URL, APIKeyRef: "env://HELIUS_KEY", Timeout: 2 * time.Second}
	opts := helius.Options{
		RPC: solanarpc.Options{Retry: solanarpc.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxRetryAfterWait: 20 * time.Millisecond}},
		Stream: helius.StreamOptions{
			PollInterval: 30 * time.Millisecond, KeepaliveInterval: 20 * time.Millisecond, ReadTimeout: 2 * time.Second,
			ReconnectMin: 5 * time.Millisecond, ReconnectMax: 20 * time.Millisecond, DialTimeout: time.Second, Lookback: time.Hour,
		},
	}
	if mutate != nil {
		mutate(&opts)
	}
	th := provider.DefaultThresholds()
	th.MinSamples = 2
	th.MaxStaleness = 0
	c, err := helius.New(context.Background(), cfg, opts, helius.Deps{Deps: solanarpc.Deps{
		Archive: h.archive, Clock: h.clk, Thresholds: &th, Logger: slog.New(slog.DiscardHandler),
		Resolver: config.EnvResolver{Lookup: func(string) (string, bool) { return key, true }},
	}})
	require.NoError(t, err)
	h.client = c
	return h
}
