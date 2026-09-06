// Package jupitertest holds test doubles and fixtures for the Jupiter
// provider client: an in-memory RawArchive, a recording SecurityEventSink,
// a scripted HTTP fixture server and deterministic wallets/signing helpers.
// It is test-only and must never be imported by production code (lintfin
// enforces the <pkg>/<pkg>test rule).
package jupitertest

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"

	"github.com/nodal/controlplane/internal/provider/jupiter"
)

// MemoryArchive is a RawArchive that keeps every Evidence in memory.
type MemoryArchive struct {
	mu       sync.Mutex
	items    []jupiter.Evidence
	FailWith error // when set, Archive returns it
}

// Archive implements jupiter.RawArchive.
func (m *MemoryArchive) Archive(_ context.Context, ev jupiter.Evidence) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWith != nil {
		return "", m.FailWith
	}
	m.items = append(m.items, ev)
	return fmt.Sprintf("mem://jupiter/%s/%d/%s", ev.Operation, len(m.items), hex.EncodeToString(ev.ResponseHash[:8])), nil
}

// Items returns a copy of every archived Evidence.
func (m *MemoryArchive) Items() []jupiter.Evidence {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]jupiter.Evidence(nil), m.items...)
}

// Len returns the number of archived records.
func (m *MemoryArchive) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

// RecordingSecuritySink records security events.
type RecordingSecuritySink struct {
	mu     sync.Mutex
	events []jupiter.SecurityEvent
}

// SecurityEvent implements jupiter.SecurityEventSink.
func (r *RecordingSecuritySink) SecurityEvent(_ context.Context, ev jupiter.SecurityEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

// Events returns a copy of recorded events.
func (r *RecordingSecuritySink) Events() []jupiter.SecurityEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]jupiter.SecurityEvent(nil), r.events...)
}

// Response is one scripted HTTP response.
type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
	// Delay is waited before responding.
	Delay time.Duration
	// Hang blocks until the request context is done (client timeout) and
	// then returns nothing useful; it simulates a transport timeout.
	Hang bool
}

// Call is one recorded request.
type Call struct {
	Method  string
	Path    string
	Query   url.Values
	Headers http.Header
	Body    []byte
}

// Server is an httptest.Server with per-route scripted responses. Queued
// responses are consumed in order; the last one is sticky once the queue
// is exhausted. Unscripted routes answer 404.
type Server struct {
	*httptest.Server
	mu      sync.Mutex
	queues  map[string][]Response
	calls   []Call
	hanging sync.WaitGroup
}

// NewServer starts a Server and registers cleanup on t.
func NewServer(t testing.TB) *Server {
	t.Helper()
	s := &Server{queues: map[string][]Response{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(func() {
		s.Close()
		s.hanging.Wait()
	})
	return s
}

func routeKey(method, path string) string { return method + " " + path }

// Enqueue scripts responses for METHOD path (e.g. "GET", "/order").
func (s *Server) Enqueue(method, path string, resps ...Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queues[routeKey(method, path)] = append(s.queues[routeKey(method, path)], resps...)
}

// Calls returns every recorded request.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// CallCount returns how many requests hit METHOD path.
func (s *Server) CallCount(method, path string) int {
	n := 0
	for _, c := range s.Calls() {
		if c.Method == method && c.Path == path {
			n++
		}
	}
	return n
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Headers: r.Header.Clone(), Body: body})
	key := routeKey(r.Method, r.URL.Path)
	q := s.queues[key]
	var resp Response
	switch {
	case len(q) == 0:
		resp = Response{Status: http.StatusNotFound, Body: []byte("no fixture for " + key)}
	case len(q) == 1:
		resp = q[0]
	default:
		resp = q[0]
		s.queues[key] = q[1:]
	}
	if resp.Hang {
		s.hanging.Add(1)
	}
	s.mu.Unlock()

	if resp.Hang {
		defer s.hanging.Done()
		<-r.Context().Done()
		return
	}
	if resp.Delay > 0 {
		select {
		case <-time.After(resp.Delay):
		case <-r.Context().Done():
			return
		}
	}
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	if _, ok := resp.Headers["Content-Type"]; !ok {
		w.Header().Set("Content-Type", "application/json")
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
}

// JSON is a convenience for a 200 JSON response with a gateway request id.
func JSON(body string) Response {
	return Response{Status: http.StatusOK, Headers: map[string]string{jupiter.HeaderGatewayRequestID: "gw-" + shortHash(body)}, Body: []byte(body)}
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// Wallet is a deterministic ed25519 keypair.
type Wallet struct {
	PrivateKey solana.PrivateKey
	PublicKey  solana.PublicKey
}

// NewWallet derives a wallet from a label (sha256 of the label is the
// seed), so tests and fixtures are reproducible.
func NewWallet(label string) Wallet {
	seed := sha256.Sum256([]byte("jupitertest-wallet|" + label))
	priv := solana.PrivateKey(ed25519.NewKeyFromSeed(seed[:]))
	return Wallet{PrivateKey: priv, PublicKey: priv.PublicKey()}
}

// Sign signs an unsigned serialized transaction with every provided wallet
// whose public key is a required signer and returns the signed bytes.
func Sign(t testing.TB, unsigned []byte, wallets ...Wallet) []byte {
	t.Helper()
	tx, err := solana.TransactionFromBytes(unsigned)
	if err != nil {
		t.Fatalf("jupitertest: decode unsigned transaction: %v", err)
	}
	_, err = tx.PartialSign(func(key solana.PublicKey) *solana.PrivateKey {
		for i := range wallets {
			if wallets[i].PublicKey.Equals(key) {
				return &wallets[i].PrivateKey
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("jupitertest: sign: %v", err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("jupitertest: marshal signed transaction: %v", err)
	}
	return raw
}

// SignatureOf returns the base58 fee-payer signature of signed bytes.
func SignatureOf(t testing.TB, signed []byte) string {
	t.Helper()
	tx, err := solana.TransactionFromBytes(signed)
	if err != nil || len(tx.Signatures) == 0 {
		t.Fatalf("jupitertest: decode signed transaction: %v", err)
	}
	return tx.Signatures[0].String()
}

// Fixture mints. MintSOL is the real wrapped-SOL mint; MintUSDC is a
// deterministic stand-in for the input mint so fixtures never depend on
// recalling a real token address correctly.
var (
	MintUSDC = NewWallet("fixture-mint-usdc").PublicKey.String()
	MintSOL  = "So11111111111111111111111111111111111111112"
)

// HasHeader reports whether the recorded call carried header k with value v.
func (c Call) HasHeader(k, v string) bool {
	return strings.EqualFold(strings.TrimSpace(c.Headers.Get(k)), v)
}
