package devnet

import (
	"fmt"
	"math/big"
)

// audit checks a chain's internal books against each other. It is the
// oracle the property tests run after every step, so it reads the private
// state directly rather than trusting the query methods it would be checking.
//
// It verifies conservation (native balances sum to native supply; each
// mint's balances sum to its supply), sparsity (no zero balances stored),
// chain linkage (heights, parents, the hash index), that every canonical
// TxID appears in exactly one block position and is recorded as included
// there, that every other known transaction is pending exactly once in a
// mempool ordered by submission, that the message index mirrors the
// transaction index, that bridge bookkeeping points at successful canonical
// transactions, and that finalized blocks have released their journals.
func (c *Chain) audit() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.state

	sum := new(big.Int)
	for k, v := range s.native {
		if v == 0 {
			return fmt.Errorf("native balance of %s stored as zero", k)
		}
		sum.Add(sum, new(big.Int).SetUint64(v))
	}
	if sum.Cmp(new(big.Int).SetUint64(s.nativeSupply)) != 0 {
		return fmt.Errorf("native balances sum to %s, supply is %d", sum, s.nativeSupply)
	}

	perMint := map[PublicKey]*big.Int{}
	for k, v := range s.tokens {
		if v == 0 {
			return fmt.Errorf("token balance %s/%s stored as zero", k.mint, k.owner)
		}
		if _, ok := s.mints[k.mint]; !ok {
			return fmt.Errorf("balance of unknown mint %s", k.mint)
		}
		if perMint[k.mint] == nil {
			perMint[k.mint] = new(big.Int)
		}
		perMint[k.mint].Add(perMint[k.mint], new(big.Int).SetUint64(v))
	}
	for mint, v := range s.supply {
		if _, ok := s.mints[mint]; !ok || v == 0 {
			return fmt.Errorf("supply entry for %s is unknown or zero", mint)
		}
	}
	for mint := range s.mints {
		got := perMint[mint]
		if got == nil {
			got = new(big.Int)
		}
		if got.Cmp(new(big.Int).SetUint64(s.supply[mint])) != 0 {
			return fmt.Errorf("mint %s: balances sum to %s, supply is %d", mint, got, s.supply[mint])
		}
	}

	if len(c.hashes) != len(c.blocks) {
		return fmt.Errorf("hash index has %d entries for %d blocks", len(c.hashes), len(c.blocks))
	}
	if c.finalized > c.tipHeight() {
		return fmt.Errorf("finalized height %d above tip %d", c.finalized, c.tipHeight())
	}
	placed := map[Signature]uint64{}
	for h, b := range c.blocks {
		height := uint64(h) // #nosec G115 -- slice index
		if b.height != height {
			return fmt.Errorf("block at index %d has height %d", h, b.height)
		}
		if got, ok := c.hashes[b.hash]; !ok || got != height {
			return fmt.Errorf("hash index disagrees at height %d", h)
		}
		if h > 0 {
			if b.parent != c.blocks[h-1].hash {
				return fmt.Errorf("block %d does not link to its parent", h)
			}
			if b.slot <= c.blocks[h-1].slot {
				return fmt.Errorf("block %d slot %d not above parent slot %d", h, b.slot, c.blocks[h-1].slot)
			}
		}
		if height <= c.finalized && height > 0 && b.effects != nil {
			return fmt.Errorf("finalized block %d still holds a journal", h)
		}
		for _, sig := range b.txs {
			if prev, dup := placed[sig]; dup {
				return fmt.Errorf("tx %s applied twice: heights %d and %d", TxIDFromSignature(sig), prev, h)
			}
			placed[sig] = height
			rec, ok := c.txs[sig]
			if !ok || !rec.included || rec.height != height || rec.slot != b.slot {
				return fmt.Errorf("tx %s in block %d is not recorded as included there", TxIDFromSignature(sig), h)
			}
		}
	}

	pending := map[Signature]bool{}
	for i, rec := range c.mempool {
		if rec.included {
			return fmt.Errorf("mempool holds included tx %s", TxIDFromSignature(rec.id))
		}
		if i > 0 && c.mempool[i-1].seq >= rec.seq {
			return fmt.Errorf("mempool out of submission order at %d", i)
		}
		if c.txs[rec.id] != rec {
			return fmt.Errorf("mempool tx %s is not indexed", TxIDFromSignature(rec.id))
		}
		pending[rec.id] = true
	}
	for sig, rec := range c.txs {
		_, inBlock := placed[sig]
		if inBlock == pending[sig] {
			return fmt.Errorf("tx %s is in a block %v and pending %v", TxIDFromSignature(sig), inBlock, pending[sig])
		}
		if got, ok := c.messages[rec.msgHash]; !ok || got != sig {
			return fmt.Errorf("message index is missing tx %s", TxIDFromSignature(sig))
		}
	}
	if len(c.messages) != len(c.txs) {
		return fmt.Errorf("message index has %d entries for %d txs", len(c.messages), len(c.txs))
	}

	succeeded := func(sig Signature) bool {
		rec, ok := c.txs[sig]
		return ok && rec.included && rec.failure == ""
	}
	for nonce, sig := range s.lockNonces {
		if !succeeded(sig) {
			return fmt.Errorf("lock nonce %d points at a tx that did not succeed canonically", nonce)
		}
	}
	if len(s.releases) != len(s.releaseNonces) {
		return fmt.Errorf("%d releases but %d release nonces", len(s.releases), len(s.releaseNonces))
	}
	for k, r := range s.releases {
		if !succeeded(r.tx) {
			return fmt.Errorf("release of %s points at a tx that did not succeed canonically", k.source)
		}
		if got, ok := s.releaseNonces[releaseNonceKey{source: k.source, nonce: r.nonce}]; !ok || got != r.tx {
			return fmt.Errorf("release nonce index disagrees for %s/%d", k.source, r.nonce)
		}
	}
	return nil
}
