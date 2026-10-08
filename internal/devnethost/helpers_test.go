package devnethost

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
)

// memStore is an in-memory Store for unit tests. It enforces the same
// position rules as devnet_events and can be told to fail appends, which is
// how a lost commit is simulated.
type memStore struct {
	mu         sync.Mutex
	events     map[string][]Event
	failAppend int // the next failAppend appends fail after fn ran
	failReads  bool
}

func newMemStore() *memStore { return &memStore{events: map[string][]Event{}} }

var errInjected = errors.New("injected store failure")

func (m *memStore) Since(_ context.Context, _ db.Querier, network string, after uint64) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failReads {
		return nil, storeUnavailable(errInjected, "read events")
	}
	return m.sinceLocked(network, after), nil
}

func (m *memStore) sinceLocked(network string, after uint64) []Event {
	var out []Event
	for _, e := range m.events[network] {
		if e.Seq > after {
			out = append(out, cloneEvent(e))
		}
	}
	return out
}

func (m *memStore) Exclusive(_ context.Context, network string, after uint64, fn func([]Event) (*Event, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failReads {
		return storeUnavailable(errInjected, "append event")
	}
	ev, err := fn(m.sinceLocked(network, after))
	if err != nil || ev == nil {
		return err
	}
	if m.failAppend > 0 {
		m.failAppend--
		return storeUnavailable(errInjected, "append event")
	}
	if ev.Seq != uint64(len(m.events[network]))+1 {
		return errors.New("memStore: event is not the next one")
	}
	m.events[network] = append(m.events[network], cloneEvent(*ev))
	return nil
}

func (m *memStore) all(network string) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sinceLocked(network, 0)
}

func (m *memStore) count(network string, kind EventKind) int {
	n := 0
	for _, e := range m.all(network) {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// replace swaps the stored event at seq (tests that tamper with history).
func (m *memStore) replace(network string, ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events[network][ev.Seq-1] = cloneEvent(ev)
}

func cloneEvent(e Event) Event {
	e.Args = append([]byte(nil), e.Args...)
	e.RawTx = append([]byte(nil), e.RawTx...)
	return e
}

const testNetwork = "unit"

// Short window and depth so expiry and finality are a few blocks away.
const (
	testWindow = 8
	testDepth  = 3
	testFee    = 10
)

func testConfig(env config.Environment) Config {
	cfg := DefaultConfig(testNetwork, env, testKey("attester").PublicKey())
	for i := range cfg.Chains {
		cfg.Chains[i].BlockhashWindow = testWindow
		cfg.Chains[i].FinalityDepth = testDepth
		cfg.Chains[i].FeePerTx = testFee
	}
	return cfg
}

func testKey(label string) devnet.Keypair {
	return devnet.NewKeypairFromSeed(sha256.Sum256([]byte("devnethost-test/seed/" + label)))
}

func testAddr(label string) devnet.PublicKey {
	return devnet.PublicKey(sha256.Sum256([]byte("devnethost-test/addr/" + label)))
}

func testClock() *clock.Fake { return clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)) }

func openHost(t testing.TB, store Store, cfg Config) *Host {
	t.Helper()
	h, err := Open(context.Background(), store, cfg, testClock())
	require.NoError(t, err)
	return h
}

// world is a host with a funded payer and one mint on sbx-solana.
type world struct {
	t      testing.TB
	ctx    context.Context
	h      *Host
	store  *memStore
	payer  devnet.Keypair
	issuer devnet.Keypair
	usd    devnet.PublicKey
}

func newWorld(t testing.TB, env config.Environment, mutate ...func(*Config)) *world {
	t.Helper()
	store := newMemStore()
	cfg := testConfig(env)
	for _, m := range mutate {
		m(&cfg)
	}
	w := &world{t: t, ctx: context.Background(), store: store, h: openHost(t, store, cfg),
		payer: testKey("payer"), issuer: testKey("issuer"), usd: testAddr("usd")}
	require.NoError(t, w.h.Airdrop(w.ctx, ChainSolana, w.payer.PublicKey(), 1_000_000))
	require.NoError(t, w.h.EnsureMint(w.ctx, ChainSolana, devnet.MintConfig{Address: w.usd, Decimals: 6, Authority: w.issuer.PublicKey()}))
	return w
}

// signedMemo returns a signed MEMO transaction on chain paid by w.payer.
func (w *world) signedMemo(chain devnet.ChainID, note string) []byte {
	w.t.Helper()
	bh, err := w.h.LatestBlockhash(w.ctx, chain)
	require.NoError(w.t, err)
	return w.sign(devnet.Message{ChainID: chain, RecentBlockhash: bh, FeePayer: w.payer.PublicKey(),
		Instructions: []devnet.Instruction{devnet.Memo{Data: []byte(note)}}}, w.payer)
}

// signedMint returns a signed TOKEN_MINT_TO of amount usd to owner.
func (w *world) signedMint(owner devnet.PublicKey, amount uint64) []byte {
	w.t.Helper()
	bh, err := w.h.LatestBlockhash(w.ctx, ChainSolana)
	require.NoError(w.t, err)
	return w.sign(devnet.Message{ChainID: ChainSolana, RecentBlockhash: bh, FeePayer: w.payer.PublicKey(),
		Instructions: []devnet.Instruction{devnet.TokenMintTo{Mint: w.usd, ToOwner: owner, Amount: amount, Authority: w.issuer.PublicKey()}}},
		w.payer, w.issuer)
}

func (w *world) sign(m devnet.Message, signers ...devnet.Keypair) []byte {
	w.t.Helper()
	tx, err := devnet.Sign(m, signers...)
	require.NoError(w.t, err)
	raw, err := tx.Encode()
	require.NoError(w.t, err)
	return raw
}

func (w *world) submit(chain devnet.ChainID, raw []byte) devnet.TxID {
	w.t.Helper()
	id, err := w.h.Submit(w.ctx, chain, raw)
	require.NoError(w.t, err)
	return id
}

func (w *world) produce(chain devnet.ChainID, n uint64) {
	w.t.Helper()
	_, err := w.h.ProduceBlocks(w.ctx, chain, n)
	require.NoError(w.t, err)
}

func (w *world) status(chain devnet.ChainID, id devnet.TxID) devnet.TxStatus {
	w.t.Helper()
	st, err := w.h.Status(w.ctx, chain, id)
	require.NoError(w.t, err)
	return st
}

func (w *world) stateHash() devnet.Hash {
	w.t.Helper()
	sh, err := w.h.StateHash(w.ctx)
	require.NoError(w.t, err)
	return sh
}
