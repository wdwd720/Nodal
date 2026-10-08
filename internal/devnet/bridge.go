package devnet

import (
	"github.com/nodal/controlplane/internal/errs"
)

// LockedTransfer is a BRIDGE_LOCK as observed on its source chain: what a
// relayer reads before building the release.
type LockedTransfer struct {
	SourceChain ChainID
	SourceTxID  TxID
	Lock        BridgeLock
	Status      TxStatus
}

// LockedTransfer returns the BRIDGE_LOCK carried by transaction id with the
// transaction's current status. A transaction carries at most one lock.
func (c *Chain) LockedTransfer(id TxID) (LockedTransfer, error) {
	sig, err := id.Signature()
	if err != nil {
		return LockedTransfer{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.txs[sig]
	if !ok {
		return LockedTransfer{}, newError(errs.CodeNotFound, CodeTransactionNotFound, "transaction "+string(id)+" not found")
	}
	for _, raw := range rec.tx.Message.Instructions {
		if lock, isLock := raw.(BridgeLock); isLock {
			return LockedTransfer{
				SourceChain: c.cfg.ID,
				SourceTxID:  id,
				Lock:        lock,
				Status:      c.statusLocked(rec),
			}, nil
		}
	}
	return LockedTransfer{}, invalid(CodeNotABridgeLock, "transaction "+string(id)+" carries no BRIDGE_LOCK")
}

// ReleaseParams are the destination-chain values a relayer supplies.
type ReleaseParams struct {
	// DestMint is the destination chain's mint for the bridged asset.
	DestMint PublicKey
	// Authority is the destination chain's bridge authority, who must sign.
	Authority PublicKey
	// FeePayer pays the destination chain's fee and must sign.
	FeePayer PublicKey
	// RecentBlockhash is a recent blockhash of the destination chain.
	RecentBlockhash Hash
}

// BuildRelease is the pure relay step: given a FINALIZED lock it returns the
// BRIDGE_RELEASE message for the lock's destination chain, minting the
// locked amount to the lock's destination owner. It reads nothing and
// changes nothing; signing (fee payer and bridge authority) and submission
// are the caller's.
//
// A lock that is not FINALIZED is refused with LOCK_NOT_FINALIZED: a
// confirmed lock can still be reorged away, and minting against it could
// create value with nothing locked behind it. The destination chain consumes
// (source chain, source tx id) and (source chain, nonce) when the release
// applies, so relaying the same lock twice, under any blockhash or fee payer,
// fails with BRIDGE_ALREADY_RELEASED.
func BuildRelease(lock LockedTransfer, p ReleaseParams) (Message, error) {
	if lock.Status.State != TxFinalized {
		return Message{}, newError(errs.CodeInvalidStateTransition, CodeLockNotFinalized,
			"lock "+string(lock.SourceTxID)+" is "+lock.Status.State.String()+", not FINALIZED")
	}
	msg := Message{
		ChainID:         lock.Lock.DestChain,
		RecentBlockhash: p.RecentBlockhash,
		FeePayer:        p.FeePayer,
		Instructions: []Instruction{BridgeRelease{
			Mint:        p.DestMint,
			ToOwner:     lock.Lock.DestOwner,
			Amount:      lock.Lock.Amount,
			SourceChain: lock.SourceChain,
			SourceTxID:  lock.SourceTxID,
			Nonce:       lock.Lock.Nonce,
			Authority:   p.Authority,
		}},
	}
	if err := msg.Validate(); err != nil {
		return Message{}, err
	}
	return msg, nil
}
