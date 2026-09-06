package helius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
)

// StreamOptions tunes StreamWalletEvents.
type StreamOptions struct {
	// PollInterval is the backfill period (default 60 s). Backfill also runs
	// on every (re)connect and whenever a hint could not be resolved.
	PollInterval time.Duration
	// Lookback is how far back the first backfill looks when
	// StreamWalletEvents (not StreamFrom) starts (default 10 min).
	Lookback time.Duration
	// KeepaliveInterval is the WebSocket ping period (default 30 s; the
	// documentation asks for at least one per minute).
	KeepaliveInterval time.Duration
	// ReadTimeout forces a reconnect when no frame arrives (default 3 min).
	ReadTimeout time.Duration
	// ReconnectMin / ReconnectMax bound the jittered reconnect backoff
	// (defaults 1 s / 30 s per the documentation).
	ReconnectMin, ReconnectMax time.Duration
	// DialTimeout bounds the WebSocket handshake (default 10 s).
	DialTimeout time.Duration
	// Commitment for the subscription (default confirmed).
	Commitment chain.Commitment
	// DisableWebSocket runs the polling backfill only.
	DisableWebSocket bool
	// MaxBackfill bounds transactions per wallet per backfill (default 100).
	MaxBackfill int
	// SeenCapacity bounds the signature dedup window (default 4096).
	SeenCapacity int
}

func (o StreamOptions) withDefaults() StreamOptions {
	if o.PollInterval <= 0 {
		o.PollInterval = 60 * time.Second
	}
	if o.Lookback <= 0 {
		o.Lookback = 10 * time.Minute
	}
	if o.KeepaliveInterval <= 0 {
		o.KeepaliveInterval = 30 * time.Second
	}
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = 3 * time.Minute
	}
	if o.ReconnectMin <= 0 {
		o.ReconnectMin = time.Second
	}
	if o.ReconnectMax <= 0 {
		o.ReconnectMax = 30 * time.Second
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.Commitment == "" {
		o.Commitment = chain.CommitmentConfirmed
	}
	if o.MaxBackfill <= 0 {
		o.MaxBackfill = 100
	}
	if o.SeenCapacity <= 0 {
		o.SeenCapacity = 4096
	}
	return o
}

// Conn is one WebSocket connection as the stream uses it.
type Conn interface {
	// Send writes v as one JSON text frame.
	Send(v any) error
	// Receive returns the next data frame.
	Receive() ([]byte, error)
	// Ping sends a WebSocket ping control frame.
	Ping() error
	SetReadDeadline(t time.Time) error
	Close() error
}

// Dialer opens WebSocket connections. wsURL carries the api key; origin is
// the handshake Origin header.
type Dialer interface {
	Dial(ctx context.Context, wsURL, origin string, timeout time.Duration) (Conn, error)
}

// xnetDialer is the default transport over golang.org/x/net/websocket.
type xnetDialer struct{}

func (xnetDialer) Dial(ctx context.Context, wsURL, origin string, timeout time.Duration) (Conn, error) {
	cfg, err := websocket.NewConfig(wsURL, origin)
	if err != nil {
		return nil, errors.New("invalid websocket configuration")
	}
	cfg.Dialer = &net.Dialer{Timeout: timeout}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ws, err := cfg.DialContext(dctx)
	if err != nil {
		var de *websocket.DialError
		if errors.As(err, &de) && de.Err != nil {
			err = de.Err // DialError renders the full URL; keep only the cause
		}
		return nil, sanitizeDialError(err)
	}
	return &xnetConn{ws: ws}, nil
}

// sanitizeDialError keeps context sentinels and drops anything that could
// carry a URL.
func sanitizeDialError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		return context.Canceled
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return fmt.Errorf("websocket dial failed (timeout=%v)", ne.Timeout())
	}
	return errors.New("websocket dial failed")
}

// xnetConn serializes every writer — Send, Ping and Close (which writes a
// close frame) — behind one mutex, because x/net shares a single buffered
// writer between them without locking. Each write carries a deadline so a
// stalled peer can never hold the mutex against Close.
type xnetConn struct {
	mu     sync.Mutex
	ws     *websocket.Conn
	closed bool
}

const wsWriteTimeout = 10 * time.Second

func (c *xnetConn) Send(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("websocket closed")
	}
	_ = c.ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return websocket.JSON.Send(c.ws, v)
}

func (c *xnetConn) Receive() ([]byte, error) {
	var b []byte
	if err := websocket.Message.Receive(c.ws, &b); err != nil {
		return nil, err
	}
	return b, nil
}

func (c *xnetConn) Ping() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("websocket closed")
	}
	_ = c.ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	w, err := c.ws.NewFrameWriter(websocket.PingFrame)
	if err != nil {
		return err
	}
	if _, err := w.Write(nil); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func (c *xnetConn) SetReadDeadline(t time.Time) error { return c.ws.SetReadDeadline(t) }

func (c *xnetConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	_ = c.ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return c.ws.Close()
}

// sinkError wraps an error returned by the consumer's sink: it ends the
// stream and is returned verbatim.
type sinkError struct{ err error }

func (e *sinkError) Error() string { return "sink: " + e.err.Error() }
func (e *sinkError) Unwrap() error { return e.err }

// seenSet is a bounded signature window for deduplication.
type seenSet struct {
	mu   sync.Mutex
	set  map[string]struct{}
	ring []string
	next int
}

func newSeenSet(capacity int) *seenSet {
	return &seenSet{set: make(map[string]struct{}, capacity), ring: make([]string, capacity)}
}

// add records sig and reports whether it was new.
func (s *seenSet) add(sig string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.set[sig]; dup {
		return false
	}
	if old := s.ring[s.next]; old != "" {
		delete(s.set, old)
	}
	s.ring[s.next] = sig
	s.next = (s.next + 1) % len(s.ring)
	s.set[sig] = struct{}{}
	return true
}

func (s *seenSet) has(sig string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.set[sig]
	return ok
}

// streamSession is one StreamFrom run.
type streamSession struct {
	c       *Client
	wallets []string
	sink    func(chain.WalletEvent) error
	seen    *seenSet
	kick    chan struct{}

	mu          sync.Mutex
	checkpoints map[string]time.Time
	sinkErr     error
}

// StreamWalletEvents implements chain.SolanaDataProvider, backfilling from
// now − Lookback.
func (c *Client) StreamWalletEvents(ctx context.Context, wallets []string, sink func(chain.WalletEvent) error) error {
	return c.StreamFrom(ctx, wallets, c.clk.Now().Add(-c.stream.Lookback), sink)
}

// StreamFrom streams wallet events with the backfill checkpoint starting at
// since (the consumer's persisted checkpoint). It returns when ctx is done
// (ctx.Err()) or when sink returns an error (that error).
func (c *Client) StreamFrom(parent context.Context, wallets []string, since time.Time, sink func(chain.WalletEvent) error) error {
	if len(wallets) == 0 {
		return errs.New(errs.CodeValidationFailed, "no wallets to stream")
	}
	if sink == nil {
		return errs.New(errs.CodeValidationFailed, "nil sink")
	}
	uniq := make([]string, 0, len(wallets))
	seenW := map[string]bool{}
	for _, w := range wallets {
		if err := solanarpc.ValidatePubkey(w); err != nil {
			return err
		}
		if !seenW[w] {
			seenW[w] = true
			uniq = append(uniq, w)
		}
	}
	s := &streamSession{
		c: c, wallets: uniq, sink: sink, seen: newSeenSet(c.stream.SeenCapacity),
		kick: make(chan struct{}, 1), checkpoints: make(map[string]time.Time, len(uniq)),
	}
	for _, w := range uniq {
		s.checkpoints[w] = since.UTC()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); results <- s.pollLoop(ctx) }()
	if !c.stream.DisableWebSocket {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.socketLoop(ctx) }()
	}
	first := <-results
	cancel()
	wg.Wait()
	if se := s.sinkError(); se != nil {
		return se
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	return first
}

func (s *streamSession) sinkError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sinkErr
}

// emit serializes sink calls; the first sink error stops everything.
func (s *streamSession) emit(ev chain.WalletEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sinkErr != nil {
		return &sinkError{s.sinkErr}
	}
	ev.Source = s.c.name
	if err := s.sink(ev); err != nil {
		s.sinkErr = err
		return &sinkError{err}
	}
	return nil
}

func (s *streamSession) checkpoint(w string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoints[w]
}

func (s *streamSession) advance(w string, t *time.Time) {
	if t == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.After(s.checkpoints[w]) {
		s.checkpoints[w] = t.UTC()
	}
}

// Checkpoints returns the current per-wallet backfill checkpoints.
func (s *streamSession) requestBackfill() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// pollLoop is the polling fallback and gap closer: it runs immediately, on
// every PollInterval tick, and whenever the socket loop asks for it.
func (s *streamSession) pollLoop(ctx context.Context) error {
	if err := s.backfill(ctx); err != nil {
		return err
	}
	t := time.NewTicker(s.c.stream.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-s.kick:
		}
		if err := s.backfill(ctx); err != nil {
			return err
		}
	}
}

// backfill polls SearchWalletActivity per wallet from its checkpoint minus
// one poll interval of overlap (dedup absorbs the overlap) and emits unseen
// transactions oldest first. Provider failures are logged and retried on
// the next round; only sink errors end the stream.
func (s *streamSession) backfill(ctx context.Context) error {
	for _, w := range s.wallets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		since := s.checkpoint(w).Add(-s.c.stream.PollInterval)
		txs, err := s.c.rpc.SearchWalletActivity(ctx, w, since, s.c.stream.MaxBackfill)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.c.log.WarnContext(ctx, "stream backfill failed", slog.String("code", string(errs.CodeOf(err))))
			continue
		}
		for i := len(txs) - 1; i >= 0; i-- {
			tx := txs[i]
			s.advance(w, tx.BlockTime)
			if !s.seen.add(tx.Signature) {
				continue
			}
			if err := s.emit(chain.WalletEvent{
				Kind: chain.EventTransaction, Origin: chain.OriginBackfill, Wallet: w,
				Observation: tx, ReceivedAt: tx.ReceivedAt,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// socketLoop keeps a transactionSubscribe connection alive, reconnecting
// with jittered backoff; every connection is announced and backfilled.
func (s *streamSession) socketLoop(ctx context.Context) error {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := s.c.dialer.Dial(ctx, s.c.wsURL.Reveal(), s.c.wsOrigin, s.c.stream.DialTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			attempt++
			s.c.log.WarnContext(ctx, "stream dial failed", slog.Int("attempt", attempt), slog.String("ws", s.c.wsRedact))
			if err := s.wait(ctx, attempt); err != nil {
				return err
			}
			continue
		}
		err = s.runConn(ctx, conn, attempt)
		var se *sinkError
		if errors.As(err, &se) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attempt++
		s.c.log.WarnContext(ctx, "stream disconnected", slog.Int("attempt", attempt))
		if err := s.wait(ctx, attempt); err != nil {
			return err
		}
	}
}

func (s *streamSession) wait(ctx context.Context, attempt int) error {
	d := s.c.stream.ReconnectMin + solanarpc.JitteredBackoff(attempt, s.c.stream.ReconnectMin, s.c.stream.ReconnectMax-s.c.stream.ReconnectMin)
	if d > s.c.stream.ReconnectMax {
		d = s.c.stream.ReconnectMax
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// subscribeRequest is the documented transactionSubscribe shape
// (helius.md, "Address activity stream").
func (s *streamSession) subscribeRequest() map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "transactionSubscribe",
		"params": []any{
			map[string]any{
				"accountInclude": s.wallets,
				"failed":         true,
				"vote":           false,
				"tokenAccounts":  "balanceChanged",
			},
			map[string]any{
				"commitment":                     string(s.c.stream.Commitment),
				"encoding":                       "jsonParsed",
				"transactionDetails":             "signatures",
				"showRewards":                    false,
				"maxSupportedTransactionVersion": 0,
			},
		},
	}
}

// runConn drives one connection until it fails or ctx ends. It returns a
// *sinkError when the consumer stopped the stream, nil otherwise.
func (s *streamSession) runConn(ctx context.Context, conn Conn, attempt int) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-cctx.Done()
		_ = conn.Close() // unblocks Receive
	}()
	detail := "connected"
	if attempt > 0 {
		detail = fmt.Sprintf("reconnected after %d failed attempt(s); backfill requested", attempt)
	}
	if err := s.emit(chain.WalletEvent{Kind: chain.EventReconnect, ReceivedAt: s.c.clk.Now(), Detail: detail}); err != nil {
		return err
	}
	if err := conn.Send(s.subscribeRequest()); err != nil {
		return nil
	}
	s.requestBackfill()
	go func() {
		t := time.NewTicker(s.c.stream.KeepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				if err := conn.Ping(); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	for {
		if err := conn.SetReadDeadline(time.Now().Add(s.c.stream.ReadTimeout)); err != nil {
			return nil
		}
		msg, err := conn.Receive()
		if err != nil {
			return nil
		}
		if err := s.handleMessage(cctx, msg); err != nil {
			return err
		}
	}
}

// wsEnvelope is the union of subscription acks, errors and notifications.
type wsEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Error   *struct {
		Code    int64  `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// notificationSignature extracts the signature and slot from a
// transactionNotification. The exact payload shape is not on the fetched
// pages (helius.md), so every documented placement is tried; anything else
// is an unparseable hint that triggers a backfill instead.
func notificationSignature(params []byte) (sig string, slot uint64, ok bool) {
	var p struct {
		Result struct {
			Signature   string          `json:"signature"`
			Slot        json.Number     `json:"slot"`
			Transaction json.RawMessage `json:"transaction"`
		} `json:"result"`
	}
	if err := solanarpc.DecodeStrict(params, &p); err != nil {
		return "", 0, false
	}
	sig = p.Result.Signature
	if sig == "" && len(p.Result.Transaction) > 0 {
		var tx struct {
			Signatures  []string `json:"signatures"`
			Transaction struct {
				Signatures []string `json:"signatures"`
			} `json:"transaction"`
		}
		if err := solanarpc.DecodeStrict(p.Result.Transaction, &tx); err == nil {
			switch {
			case len(tx.Signatures) > 0:
				sig = tx.Signatures[0]
			case len(tx.Transaction.Signatures) > 0:
				sig = tx.Transaction.Signatures[0]
			}
		}
	}
	if solanarpc.ValidateSignature(sig) != nil {
		return "", 0, false
	}
	if p.Result.Slot != "" {
		if v, err := p.Result.Slot.Int64(); err == nil && v >= 0 {
			slot = uint64(v)
		}
	}
	return sig, slot, true
}

// handleMessage turns a frame into at most one RPC-backed event.
func (s *streamSession) handleMessage(ctx context.Context, msg []byte) error {
	received := s.c.clk.Now()
	var env wsEnvelope
	if err := solanarpc.DecodeStrict(msg, &env); err != nil {
		s.c.log.DebugContext(ctx, "stream frame ignored", slog.String("reason", "malformed"))
		s.requestBackfill()
		return nil
	}
	if env.Error != nil {
		msg := env.Error.Message
		if len(msg) > 120 {
			msg = msg[:120]
		}
		s.c.log.WarnContext(ctx, "stream subscription error", slog.Int64("code", env.Error.Code), slog.String("message", msg))
		return nil
	}
	if env.Method != "transactionNotification" {
		return nil // subscription ack or unrelated method
	}
	sig, slot, ok := notificationSignature(env.Params)
	if !ok {
		s.requestBackfill()
		return nil
	}
	if s.seen.has(sig) {
		return nil
	}
	obs, err := s.c.rpc.GetTransaction(ctx, sig)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		s.c.log.WarnContext(ctx, "stream hint could not be observed", slog.String("code", string(errs.CodeOf(err))))
		s.requestBackfill()
		return nil
	}
	if !obs.Found {
		// Not yet visible at the read commitment: the backfill will find it.
		s.requestBackfill()
		return nil
	}
	if !s.seen.add(sig) {
		return nil
	}
	wallet := ""
	for _, w := range s.wallets {
		if obs.Touches(w) {
			wallet = w
			break
		}
	}
	if wallet == "" {
		return nil // token-account activity for an ATA we cannot attribute: backfill covers it
	}
	s.advance(wallet, obs.BlockTime)
	ev := chain.WalletEvent{Kind: chain.EventTransaction, Origin: chain.OriginStream, Wallet: wallet, Observation: obs, ReceivedAt: received}
	if slot > 0 {
		seq := chain.SequenceOf(slot, 0)
		ev.Sequence = &seq
		ev.Detail = "sequence index within slot unknown; derived from slot only"
	}
	return s.emit(ev)
}
