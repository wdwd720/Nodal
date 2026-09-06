package chaintest

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/money"
)

// Chain is a deterministic Solana-like ledger for tests: slots advance on
// demand, some slots may be skipped (so block height < slot), landed
// transactions progress processed → confirmed → finalized as slots pass,
// blockhashes expire after MaxBlockhashAge blocks, and balances follow the
// token/lamport deltas of landed transactions. Observers created with
// NewObserver read the chain through an optional lag.
type Chain struct {
	mu sync.Mutex

	clk clock.Clock
	// ConfirmAfter / FinalizeAfter are the slot distances at which a landed
	// transaction becomes confirmed / finalized (defaults 1 and 32).
	ConfirmAfter, FinalizeAfter uint64
	// MaxBlockhashAge is the number of blocks a blockhash stays valid
	// (default 150, matching the 151-entry max processing age).
	MaxBlockhashAge uint64

	slot       uint64
	blockSlots []uint64 // slots that produced a block, ascending
	txs        map[string]*landed
	balances   map[string]map[string]chain.BalanceObservation // owner → mint/tokenAccount → balance
	hashes     map[string]uint64                              // blockhash → lastValidBlockHeight
	hashSeq    uint64
	observers  []*Fake
}

type landed struct {
	obs   chain.TxObservation
	slot  uint64
	index uint32
}

// NewChain returns a chain at slot 0 with one block.
func NewChain(clk clock.Clock) *Chain {
	if clk == nil {
		clk = clock.System()
	}
	return &Chain{
		clk: clk, ConfirmAfter: 1, FinalizeAfter: 32, MaxBlockhashAge: 150,
		blockSlots: []uint64{0},
		txs:        map[string]*landed{},
		balances:   map[string]map[string]chain.BalanceObservation{},
		hashes:     map[string]uint64{},
	}
}

// Slot returns the current slot.
func (c *Chain) Slot() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slot
}

// Height returns the current block height (blocks produced − 1).
func (c *Chain) Height() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.heightAt(c.slot)
}

// heightAt returns the block height as of slot s (locked).
func (c *Chain) heightAt(s uint64) uint64 {
	n := sort.Search(len(c.blockSlots), func(i int) bool { return c.blockSlots[i] > s })
	if n == 0 {
		return 0
	}
	return uint64(n - 1) //nolint:gosec // G115: n >= 1 here (sort.Search over a non-empty block list)
}

// Advance produces n consecutive blocks (one per slot).
func (c *Chain) Advance(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := uint64(0); i < n; i++ {
		c.slot++
		c.blockSlots = append(c.blockSlots, c.slot)
	}
}

// Skip advances n slots without producing blocks (height unchanged).
func (c *Chain) Skip(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slot += n
}

// Land records obs as landed in the current slot: Slot, BlockTime and Found
// are stamped, balances are updated from its deltas, and every observer
// with streaming enabled receives a STREAM hint. The stamped observation is
// returned.
func (c *Chain) Land(obs chain.TxObservation) chain.TxObservation {
	c.mu.Lock()
	if obs.Signature == "" {
		obs.Signature = fmt.Sprintf("sig-%d-%d", c.slot, len(c.txs))
	}
	if _, dup := c.txs[obs.Signature]; dup {
		c.mu.Unlock()
		panic("chaintest: signature already landed: " + obs.Signature)
	}
	now := c.clk.Now()
	obs.Found = true
	obs.Slot = c.slot
	obs.BlockTime = &now
	if obs.Version == "" {
		obs.Version = chain.VersionV0
	}
	if obs.Err == "" {
		for _, d := range obs.TokenBalanceDeltas {
			c.setBalanceLocked(chain.BalanceObservation{
				Owner: d.Owner, Mint: d.Mint, TokenAccount: d.TokenAccount, Program: d.Program,
				Amount: d.Post, Decimals: d.Decimals, DecimalsKnown: true,
			})
		}
	}
	for _, d := range obs.LamportDeltas {
		c.setBalanceLocked(chain.BalanceObservation{
			Owner: d.Account, Mint: chain.MintNativeSOL, TokenAccount: d.Account,
			Amount: d.Post, Decimals: chain.NativeDecimals, DecimalsKnown: true,
		})
	}
	index := uint32(0)
	for _, l := range c.txs {
		if l.slot == c.slot {
			index++
		}
	}
	c.txs[obs.Signature] = &landed{obs: obs, slot: c.slot, index: index}
	observers := append([]*Fake(nil), c.observers...)
	c.mu.Unlock()
	for _, f := range observers {
		f.hint(obs.Signature, obs.Slot, index)
	}
	return obs
}

// Drop removes a landed transaction (a reorg dropped it).
func (c *Chain) Drop(sig string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.txs, sig)
}

// SetBalance sets a balance directly.
func (c *Chain) SetBalance(b chain.BalanceObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setBalanceLocked(b)
}

func (c *Chain) setBalanceLocked(b chain.BalanceObservation) {
	if b.TokenAccount == "" {
		b.TokenAccount = b.Owner
	}
	m := c.balances[b.Owner]
	if m == nil {
		m = map[string]chain.BalanceObservation{}
		c.balances[b.Owner] = m
	}
	m[b.Mint+"/"+b.TokenAccount] = b
}

// IssueBlockhash issues a fresh blockhash valid for MaxBlockhashAge blocks.
func (c *Chain) IssueBlockhash() chain.Blockhash {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hashSeq++
	h := fmt.Sprintf("hash-%d", c.hashSeq)
	lvbh := c.heightAt(c.slot) + c.MaxBlockhashAge
	c.hashes[h] = lvbh
	return chain.Blockhash{Blockhash: h, LastValidBlockHeight: lvbh, Slot: c.slot}
}

// blockhashValid reports validity as of slot s (locked).
func (c *Chain) blockhashValidAt(h string, s uint64) bool {
	lvbh, ok := c.hashes[h]
	return ok && c.heightAt(s) <= lvbh
}

// commitmentAt returns the commitment of a landed tx as seen from slot s
// (locked); ok is false when the tx is not visible yet.
func (c *Chain) commitmentAt(l *landed, s uint64) (chain.Commitment, uint64, bool) {
	if l.slot > s {
		return "", 0, false
	}
	age := s - l.slot
	switch {
	case age >= c.FinalizeAfter:
		return chain.CommitmentFinalized, age, true
	case age >= c.ConfirmAfter:
		return chain.CommitmentConfirmed, age, true
	default:
		return chain.CommitmentProcessed, age, true
	}
}

// NewObserver creates a Fake bound to this chain. Lag makes the observer
// see the chain as it was lag slots ago.
func (c *Chain) NewObserver(name string, lag uint64) *Fake {
	f := NewFake(name, c.clk)
	f.chain = c
	f.lag = lag
	c.mu.Lock()
	c.observers = append(c.observers, f)
	c.mu.Unlock()
	return f
}

// view returns the observation of sig as seen at (slot − lag). Transactions
// below confirmed commitment are invisible to getTransaction semantics.
func (c *Chain) view(sig string, lag uint64, minCommitment chain.Commitment) (chain.TxObservation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.txs[sig]
	if !ok {
		return chain.TxObservation{}, false
	}
	s := c.effectiveSlot(lag)
	cm, _, visible := c.commitmentAt(l, s)
	if !visible || cm.Rank() < minCommitment.Rank() {
		return chain.TxObservation{}, false
	}
	obs := l.obs
	obs.Commitment = cm
	return obs, true
}

func (c *Chain) effectiveSlot(lag uint64) uint64 {
	if lag > c.slot {
		return 0
	}
	return c.slot - lag
}

// status returns the signature status as seen at (slot − lag).
func (c *Chain) status(sig string, lag uint64) (chain.SignatureStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.txs[sig]
	if !ok {
		return chain.SignatureStatus{Signature: sig}, false
	}
	cm, age, visible := c.commitmentAt(l, c.effectiveSlot(lag))
	if !visible {
		return chain.SignatureStatus{Signature: sig}, false
	}
	st := chain.SignatureStatus{Signature: sig, Found: true, Slot: l.slot, Commitment: cm, Err: l.obs.Err}
	if cm != chain.CommitmentFinalized {
		conf := age
		st.Confirmations = &conf
	}
	return st, true
}

// balancesOf returns the owner's balances (filtered by mints) stamped with
// the observer's effective slot.
func (c *Chain) balancesOf(owner string, mints []string, lag uint64) []chain.BalanceObservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	want := map[string]bool{}
	for _, m := range mints {
		want[m] = true
	}
	var out []chain.BalanceObservation
	for _, b := range c.balances[owner] {
		if len(want) > 0 && !want[b.Mint] {
			continue
		}
		b.Slot = c.effectiveSlot(lag)
		out = append(out, b)
	}
	chain.SortBalances(out)
	return out
}

// activity returns confirmed-or-better transactions touching wallet with
// BlockTime ≥ since, newest first.
func (c *Chain) activity(wallet string, since time.Time, limit int, lag uint64) []chain.TxObservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.effectiveSlot(lag)
	var out []chain.TxObservation
	for _, l := range c.txs {
		cm, _, visible := c.commitmentAt(l, s)
		if !visible || cm.Rank() < chain.CommitmentConfirmed.Rank() || !l.obs.Touches(wallet) {
			continue
		}
		if l.obs.BlockTime != nil && l.obs.BlockTime.Before(since) {
			continue
		}
		obs := l.obs
		obs.Commitment = cm
		out = append(out, obs)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Slot != out[j].Slot {
			return out[i].Slot > out[j].Slot
		}
		return out[i].Signature > out[j].Signature
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Swap builds a successful swap observation for wallet: −inAmount of
// inMint, +outAmount of outMint, fee lamports paid by the wallet. Balances
// before the swap are taken from the chain (zero when unknown).
func (c *Chain) Swap(wallet, inMint, outMint string, inAmount, outAmount, fee money.Quantity) chain.TxObservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	pre := func(mint, acct string) money.Quantity {
		if b, ok := c.balances[wallet][mint+"/"+acct]; ok {
			return b.Amount
		}
		return money.QuantityFromInt64(0)
	}
	inAcct, outAcct := wallet+"-ata-"+inMint, wallet+"-ata-"+outMint
	preIn, preOut, preSOL := pre(inMint, inAcct), pre(outMint, outAcct), pre(chain.MintNativeSOL, wallet)
	return chain.TxObservation{
		Version:     chain.VersionV0,
		AccountKeys: []string{wallet, inAcct, outAcct, "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"},
		TokenBalanceDeltas: []chain.TokenDelta{
			{Owner: wallet, Mint: inMint, TokenAccount: inAcct, Pre: preIn, Post: preIn.Sub(inAmount), Decimals: 6},
			{Owner: wallet, Mint: outMint, TokenAccount: outAcct, Pre: preOut, Post: preOut.Add(outAmount), Decimals: 6},
		},
		LamportDeltas: []chain.LamportDelta{{Account: wallet, Pre: preSOL, Post: preSOL.Sub(fee)}},
		Fee:           fee,
	}
}
