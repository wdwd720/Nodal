package devnet

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// testSeed derives a deterministic seed from a label.
func testSeed(label string) [SeedSize]byte {
	return sha256.Sum256([]byte("nodal-devnet-test/seed/" + label))
}

// testKey derives a deterministic keypair from a label.
func testKey(label string) Keypair { return NewKeypairFromSeed(testSeed(label)) }

// testAddr derives an address that no test keypair controls.
func testAddr(label string) PublicKey {
	return PublicKey(sha256.Sum256([]byte("nodal-devnet-test/addr/" + label)))
}

const testChainID ChainID = "sbx-solana"

// Test chains use a short window and depth so expiry and finality are a few
// blocks away.
const (
	testWindow = 10
	testDepth  = 4
	testFee    = 10
	testFunds  = 1_000_000
)

func testConfig(id ChainID) ChainConfig {
	cfg := DefaultChainConfig(id)
	cfg.BlockhashWindow = testWindow
	cfg.FinalityDepth = testDepth
	cfg.FeePerTx = testFee
	return cfg
}

// world is a chain with a funded fee payer, three funded users and one mint.
type world struct {
	t                 testing.TB
	c                 *Chain
	payer             Keypair
	issuer            Keypair   // the authority of usd
	usd               PublicKey // a 6-decimal mint
	alice, bob, carol Keypair
}

func newWorld(t testing.TB, mutate ...func(*ChainConfig)) *world {
	t.Helper()
	cfg := testConfig(testChainID)
	for _, m := range mutate {
		m(&cfg)
	}
	c, err := NewChain(cfg)
	require.NoError(t, err)
	w := &world{
		t: t, c: c,
		payer: testKey("payer"), issuer: testKey("issuer"), usd: testAddr("usd"),
		alice: testKey("alice"), bob: testKey("bob"), carol: testKey("carol"),
	}
	for _, k := range []Keypair{w.payer, w.alice, w.bob, w.carol} {
		require.NoError(t, c.Airdrop(k.PublicKey(), testFunds))
	}
	require.NoError(t, c.CreateMint(MintConfig{Address: w.usd, Decimals: 6, Authority: w.issuer.PublicKey()}))
	return w
}

// msg builds a message for w's chain, paid by w.payer, with the latest
// blockhash.
func (w *world) msg(ixs ...Instruction) Message {
	return Message{
		ChainID:         w.c.ID(),
		RecentBlockhash: w.c.LatestBlockhash(),
		FeePayer:        w.payer.PublicKey(),
		Instructions:    ixs,
	}
}

// signed signs m with signers and returns the wire encoding.
func signed(t require.TestingT, m Message, signers ...Keypair) []byte {
	if h, ok := t.(interface{ Helper() }); ok {
		h.Helper() // *testing.T has Helper; *rapid.T does not
	}
	tx, err := Sign(m, signers...)
	require.NoError(t, err)
	raw, err := tx.Encode()
	require.NoError(t, err)
	return raw
}

// submit submits raw and requires acceptance.
func (w *world) submit(raw []byte) TxID {
	w.t.Helper()
	id, err := w.c.Submit(raw)
	require.NoError(w.t, err)
	return id
}

// send signs ixs (payer plus signers), submits, and returns the TxID.
func (w *world) send(signers []Keypair, ixs ...Instruction) TxID {
	w.t.Helper()
	return w.submit(signed(w.t, w.msg(ixs...), append([]Keypair{w.payer}, signers...)...))
}

// land is send followed by ProduceBlock.
func (w *world) land(signers []Keypair, ixs ...Instruction) TxID {
	w.t.Helper()
	id := w.send(signers, ixs...)
	w.c.ProduceBlock()
	return id
}

// mintUSD issues amount of usd to owner in its own block and requires
// success.
func (w *world) mintUSD(owner PublicKey, amount uint64) TxID {
	w.t.Helper()
	id := w.land([]Keypair{w.issuer},
		TokenMintTo{Mint: w.usd, ToOwner: owner, Amount: amount, Authority: w.issuer.PublicKey()})
	require.Equal(w.t, TxConfirmed, w.c.Status(id).State)
	return id
}

func (w *world) usdOf(k Keypair) uint64 { return w.c.TokenBalance(k.PublicKey(), w.usd) }

func (w *world) nativeOf(k Keypair) uint64 { return w.c.Balance(k.PublicKey()) }

func (w *world) usdSupply() uint64 {
	w.t.Helper()
	s, err := w.c.Supply(w.usd)
	require.NoError(w.t, err)
	return s
}

// requireFailed requires id to be FAILED with code.
func (w *world) requireFailed(id TxID, code ErrorCode) {
	w.t.Helper()
	st := w.c.Status(id)
	require.Equal(w.t, TxFailed, st.State, "status %+v", st)
	require.Equal(w.t, code, st.Err)
}

// requireReason requires err to carry the boundary code and devnet reason.
func requireReason(t require.TestingT, err error, code errs.Code, reason ErrorCode) {
	if h, ok := t.(interface{ Helper() }); ok {
		h.Helper() // *testing.T has Helper; *rapid.T does not
	}
	require.Error(t, err)
	require.Equal(t, code, errs.CodeOf(err), "error: %v", err)
	require.Equal(t, reason, ReasonOf(err), "error: %v", err)
}

// memo returns a distinct MEMO so otherwise identical messages differ.
func memo(s string) Memo { return Memo{Data: []byte(s)} }
