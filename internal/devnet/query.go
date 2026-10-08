package devnet

import (
	"strconv"

	"github.com/nodal/controlplane/internal/errs"
)

// Tip returns the canonical tip block.
func (c *Chain) Tip() BlockInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blockInfoLocked(c.tip())
}

// FinalizedHeight returns the highest finalized block height. It never
// decreases.
func (c *Chain) FinalizedHeight() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finalized
}

// Block returns the canonical block at height.
func (c *Chain) Block(height uint64) (BlockInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if height > c.tipHeight() {
		return BlockInfo{}, newError(errs.CodeNotFound, CodeBlockNotFound,
			"no canonical block at height "+strconv.FormatUint(height, 10))
	}
	return c.blockInfoLocked(c.blocks[height]), nil
}

// LatestBlockhash returns the tip's hash: the blockhash a new transaction
// should reference.
func (c *Chain) LatestBlockhash() Hash {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tip().hash
}

// IsBlockhashValid reports whether a transaction referencing h could be
// accepted now. Once it is false for a NOT_FOUND transaction's blockhash,
// that transaction can never land.
func (c *Chain) IsBlockhashValid(h Hash) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blockhashValidLocked(h)
}

// PendingCount returns the number of transactions in the mempool.
func (c *Chain) PendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.mempool)
}

// Status returns the transaction's status; an unknown or malformed TxID is
// NOT_FOUND.
func (c *Chain) Status(id TxID) TxStatus {
	sig, err := id.Signature()
	if err != nil {
		return TxStatus{State: TxNotFound}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.txs[sig]
	if !ok {
		return TxStatus{State: TxNotFound}
	}
	return c.statusLocked(rec)
}

// GetTransaction returns the decoded transaction with its status and fee.
// The result shares no memory with the chain.
func (c *Chain) GetTransaction(id TxID) (TxRecord, error) {
	sig, err := id.Signature()
	if err != nil {
		return TxRecord{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.txs[sig]
	if !ok {
		return TxRecord{}, newError(errs.CodeNotFound, CodeTransactionNotFound, "transaction "+string(id)+" not found")
	}
	tx, err := Decode(rec.raw)
	if err != nil {
		return TxRecord{}, errs.Wrap(err, errs.CodeInternal, "devnet: a stored transaction no longer decodes")
	}
	return TxRecord{ID: id, Transaction: tx, Status: c.statusLocked(rec), Fee: rec.fee}, nil
}

// Balance returns owner's native balance.
func (c *Chain) Balance(owner PublicKey) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.native[owner]
}

// NativeSupply returns the total native units in existence: everything ever
// airdropped. Transfers and fees conserve it.
func (c *Chain) NativeSupply() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.nativeSupply
}

// TokenBalance returns owner's balance of mint (zero for an unknown mint).
func (c *Chain) TokenBalance(owner, mint PublicKey) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.tokens[tokenKey{mint: mint, owner: owner}]
}

// Supply returns mint's supply.
func (c *Chain) Supply(mint PublicKey) (uint64, error) {
	info, err := c.Mint(mint)
	if err != nil {
		return 0, err
	}
	return info.Supply, nil
}

// Mint returns mint's configuration and supply.
func (c *Chain) Mint(mint PublicKey) (MintInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.state.mints[mint]
	if !ok {
		return MintInfo{}, newError(errs.CodeNotFound, CodeMintNotFound, "mint "+mint.String()+" not found")
	}
	return MintInfo{
		MintConfig: MintConfig{Address: mint, Decimals: m.decimals, Authority: m.authority, Frozen: m.frozen},
		Supply:     c.state.supply[mint],
	}, nil
}
