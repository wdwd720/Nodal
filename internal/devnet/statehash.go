package devnet

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"slices"
)

// StateHash returns a canonical SHA-256 commitment to the chain's state:
// native supply and every non-zero native balance; every mint's decimals,
// authority, freeze flag and supply; every non-zero token balance; the
// consumed bridge lock nonces and releases; and the finalized transaction
// set with each transaction's height and outcome.
// Entries are sorted, so the hash depends only on the state and never on
// map iteration order: identical operation sequences give identical hashes.
func (c *Chain) StateHash() Hash {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateHashLocked()
}

func (c *Chain) stateHashLocked() Hash {
	s := &c.state
	w := &writer{buf: make([]byte, 0, 1024)}
	w.str(stateDomain)
	w.label(string(c.cfg.ID))
	w.u64(s.nativeSupply)

	accounts := sortedKeys(s.native, comparePublicKey)
	w.length(len(accounts))
	for _, k := range accounts {
		w.key(k)
		w.u64(s.native[k])
	}

	mints := sortedKeys(s.mints, comparePublicKey)
	w.length(len(mints))
	for _, k := range mints {
		m := s.mints[k]
		w.key(k)
		w.u8(m.decimals)
		w.key(m.authority)
		w.u8(boolByte(m.frozen))
		w.u64(s.supply[k])
	}

	holdings := sortedKeys(s.tokens, func(a, b tokenKey) int {
		return cmp.Or(comparePublicKey(a.mint, b.mint), comparePublicKey(a.owner, b.owner))
	})
	w.length(len(holdings))
	for _, k := range holdings {
		w.key(k.mint)
		w.key(k.owner)
		w.u64(s.tokens[k])
	}

	nonces := sortedKeys(s.lockNonces, cmp.Compare[uint64])
	w.length(len(nonces))
	for _, n := range nonces {
		w.u64(n)
		w.signature(s.lockNonces[n])
	}

	releases := sortedKeys(s.releases, func(a, b releaseKey) int {
		return cmp.Or(cmp.Compare(a.source, b.source), bytes.Compare(a.sourceTx[:], b.sourceTx[:]))
	})
	w.length(len(releases))
	for _, k := range releases {
		r := s.releases[k]
		w.label(string(k.source))
		w.signature(k.sourceTx)
		w.u64(r.nonce)
		w.signature(r.tx)
	}

	var final []*txRecord
	for _, b := range c.blocks[1 : c.finalized+1] {
		for _, sig := range b.txs {
			if rec, ok := c.txs[sig]; ok {
				final = append(final, rec)
			}
		}
	}
	slices.SortFunc(final, func(a, b *txRecord) int { return bytes.Compare(a.id[:], b.id[:]) })
	w.length(len(final))
	for _, rec := range final {
		w.signature(rec.id)
		w.u64(rec.height)
		w.label(string(rec.failure))
	}
	return Hash(sha256.Sum256(w.buf))
}

// sortedKeys returns m's keys in the order cmpFn defines.
func sortedKeys[K comparable, V any](m map[K]V, cmpFn func(a, b K) int) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, cmpFn)
	return keys
}

func comparePublicKey(a, b PublicKey) int { return bytes.Compare(a[:], b[:]) }

func boolByte(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
