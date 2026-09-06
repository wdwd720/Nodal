package helius_test

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
	"golang.org/x/net/websocket"

	"github.com/nodal/controlplane/internal/chain/chaintest"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/helius"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
)

// Identifiers (shapes only).
const (
	sigA     = "5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW"
	sigB     = "4Wf4kGKv5qLj1Uf7VvNbY5v9F3Zq8bJ5aE4v2n1kY8f3M9p7Qw2tR6sX1cV5bN8mK3jH7gF4dS2aP9oL6iU3yT1e"
	wallet   = "9aE476sH92Vz7DMPyq5WLPkrKWivxnuXaMLh3Gvy8Bev"
	other    = "2nZ8vB6mK4jH2gF9dS7aP5oL3iU1yT9eR7wQ5vX3cV1b"
	ataUSDC  = "4kJ3Uc9BwYpKcgZfN1VzT8n7mQ6xR2aS5dF7gH9jK2mZ"
	ataBONK  = "7pL2mN4qR6sT8vX1cV3bN5mK7jH9gF2dS4aP6oL8iU1y"
	mintUSDC = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	mintBONK = "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263"
	jupiter  = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"
	tokenPID = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	apiKey   = "HELIUS-SECRET-KEY"
)

type rpcErr struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

type rpcCall struct {
	Method string
	Params json.RawMessage
	Query  string
}

// server serves JSON-RPC on POST and transactionSubscribe on WebSocket
// upgrades at the same URL, like the Helius endpoint pair.
type server struct {
	t        *testing.T
	mu       sync.Mutex
	handlers map[string]func(call rpcCall) (any, *rpcErr)
	calls    []rpcCall
	status   int // when non-zero every RPC response uses this status with an empty body
	srv      *httptest.Server

	// WebSocket scripting.
	wsConns   int
	wsAccept  bool                 // when false the upgrade is refused (plan-gated)
	wsCloseIn time.Duration        // close the socket this long after the ack (0 = keep open)
	wsAckErr  *rpcErr              // reply to the subscription with an error instead of an ack
	notify    chan json.RawMessage // frames pushed to every open socket
	subs      chan json.RawMessage // subscription requests seen
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{
		t: t, handlers: map[string]func(rpcCall) (any, *rpcErr){}, wsAccept: true,
		notify: make(chan json.RawMessage, 16), subs: make(chan json.RawMessage, 16),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *server) handle(method string, fn func(call rpcCall) (any, *rpcErr)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = fn
}

func (s *server) recorded(method string) []rpcCall {
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

func (s *server) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wsConns
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") == "websocket" {
		s.mu.Lock()
		accept := s.wsAccept
		s.mu.Unlock()
		if !accept || r.URL.Query().Get("api-key") != apiKey {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		websocket.Handler(s.serveWS).ServeHTTP(w, r)
		return
	}
	if r.URL.Query().Get("api-key") != apiKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	status := s.status
	s.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	call := rpcCall{Method: req.Method, Params: req.Params, Query: r.URL.RawQuery}
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

func (s *server) serveWS(ws *websocket.Conn) {
	defer func() { _ = ws.Close() }()
	s.mu.Lock()
	s.wsConns++
	closeIn, ackErr := s.wsCloseIn, s.wsAckErr
	s.mu.Unlock()
	var raw []byte
	if err := websocket.Message.Receive(ws, &raw); err != nil {
		return
	}
	s.subs <- json.RawMessage(raw)
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(raw, &req)
	ack := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": 4242}
	if ackErr != nil {
		ack = map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": ackErr}
	}
	if err := websocket.JSON.Send(ws, ack); err != nil {
		return
	}
	var closeTimer <-chan time.Time
	if closeIn > 0 {
		closeTimer = time.After(closeIn)
	}
	done := make(chan struct{})
	go func() { // drain client frames (pings are answered inside x/net)
		defer close(done)
		var b []byte
		for {
			if err := websocket.Message.Receive(ws, &b); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-closeTimer:
			return
		case <-done:
			return
		case frame := <-s.notify:
			if err := websocket.Message.Send(ws, string(frame)); err != nil {
				return
			}
		}
	}
}

// param decodes the i-th positional param.
func param(t *testing.T, call rpcCall, i int, v any) {
	t.Helper()
	var arr []json.RawMessage
	require.NoError(t, json.Unmarshal(call.Params, &arr))
	require.Greater(t, len(arr), i)
	dec := json.NewDecoder(bytes.NewReader(arr[i]))
	dec.UseNumber()
	require.NoError(t, dec.Decode(v))
}

// namedParams decodes object params (DAS).
func namedParams(t *testing.T, call rpcCall) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(call.Params))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&m))
	return m
}

// syncBuffer is a concurrency-safe log sink: stream goroutines write while
// tests read.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type deps struct {
	archive *chaintest.MemArchive
	clk     *clock.Fake
	logs    *syncBuffer
}

func newClient(t *testing.T, s *server, mutate func(cfg *config.ProviderConfig, opts *helius.Options, d *helius.Deps)) (*helius.Client, *deps) {
	t.Helper()
	d := &deps{archive: chaintest.NewMemArchive(), clk: clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)), logs: &syncBuffer{}}
	cfg := config.ProviderConfig{Mode: config.ProviderModeSandbox, BaseURL: s.srv.URL, APIKeyRef: "env://HELIUS_KEY", Timeout: 2 * time.Second}
	th := provider.DefaultThresholds()
	th.MinSamples = 3
	th.MaxStaleness = 0
	opts := helius.Options{
		RPC: solanarpc.Options{Retry: solanarpc.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxRetryAfterWait: 20 * time.Millisecond}},
		Stream: helius.StreamOptions{
			PollInterval: 30 * time.Millisecond, KeepaliveInterval: 15 * time.Millisecond, ReadTimeout: 2 * time.Second,
			ReconnectMin: 5 * time.Millisecond, ReconnectMax: 20 * time.Millisecond, DialTimeout: time.Second, Lookback: time.Hour,
		},
	}
	hd := helius.Deps{Deps: solanarpc.Deps{
		Archive: d.archive, Clock: d.clk, Thresholds: &th,
		Resolver: config.EnvResolver{Lookup: func(string) (string, bool) { return apiKey, true }},
		Logger:   slog.New(slog.NewTextHandler(d.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}}
	if mutate != nil {
		mutate(&cfg, &opts, &hd)
	}
	c, err := helius.New(context.Background(), cfg, opts, hd)
	require.NoError(t, err)
	return c, d
}

// Fixture builders.
func validTx(sig string) map[string]any {
	return map[string]any{
		"slot":      json.Number("250000123"),
		"blockTime": json.Number("1757073600"),
		"version":   json.Number("0"),
		"transaction": map[string]any{
			"signatures": []string{sig},
			"message":    map[string]any{"accountKeys": []string{wallet, ataUSDC, ataBONK, jupiter}},
		},
		"meta": map[string]any{
			"err": nil, "fee": json.Number("5000"),
			"preBalances":  []json.Number{"10000000", "2039280", "2039280", "1"},
			"postBalances": []json.Number{"9995000", "2039280", "2039280", "1"},
			"preTokenBalances": []any{
				tokenBal(1, mintUSDC, wallet, "1000000", 6),
			},
			"postTokenBalances": []any{
				tokenBal(1, mintUSDC, wallet, "0", 6),
				tokenBal(2, mintBONK, wallet, "123456789", 5),
			},
		},
	}
}

func tokenBal(index int, mint, owner, amount string, decimals int) map[string]any {
	return map[string]any{
		"accountIndex": index, "mint": mint, "owner": owner, "programId": tokenPID,
		"uiTokenAmount": map[string]any{"amount": amount, "decimals": decimals, "uiAmount": nil, "uiAmountString": amount},
	}
}

func ctxSlot(slot string, value any) map[string]any {
	return map[string]any{"context": map[string]any{"slot": json.Number(slot), "apiVersion": "2.1.0"}, "value": value}
}

func dasAccount(addr, mint, owner string, amount any) map[string]any {
	return map[string]any{"address": addr, "mint": mint, "owner": owner, "amount": amount, "delegated_amount": 0, "frozen": false}
}

func dasPage(total, limit, page int, accounts ...any) map[string]any {
	return map[string]any{"total": total, "limit": limit, "page": page, "token_accounts": accounts}
}

func sigItem(sig string, slot, bt int64) map[string]any {
	return map[string]any{"signature": sig, "slot": slot, "err": nil, "memo": nil, "blockTime": bt, "confirmationStatus": "finalized"}
}

// quietRPC scripts the reads a stream backfill needs so that it finds
// nothing unless a test adds signatures.
func quietRPC(s *server) {
	s.handle("getTokenAccountsByOwner", func(rpcCall) (any, *rpcErr) { return ctxSlot("1", []any{}), nil })
	s.handle("getSignaturesForAddress", func(rpcCall) (any, *rpcErr) { return []any{}, nil })
	s.handle("getSignatureStatuses", func(call rpcCall) (any, *rpcErr) {
		var sigs []string
		param(s.t, call, 0, &sigs)
		items := make([]any, len(sigs))
		return ctxSlot("1", items), nil
	})
	s.handle("getTransaction", func(call rpcCall) (any, *rpcErr) {
		var sig string
		param(s.t, call, 0, &sig)
		return validTx(sig), nil
	})
}

func notification(sig string, slot int64) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "transactionNotification",
		"params": map[string]any{"subscription": 4242, "result": map[string]any{
			"signature": sig, "slot": slot,
			"transaction": map[string]any{"transaction": map[string]any{"signatures": []string{sig}}, "meta": map[string]any{}},
		}},
	})
	return b
}
