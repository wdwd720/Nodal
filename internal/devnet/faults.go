package devnet

import (
	"strconv"

	"github.com/nodal/controlplane/internal/errs"
)

// Sandbox fault-injection hooks. Every fault is armed by an explicit call on
// the Chain; nothing arms one implicitly, and none of them changes state that
// is already final.

// fault is a one-shot submission fault.
type fault uint8

const (
	faultNone fault = iota
	// faultDrop: Submit succeeds, the transaction is never included.
	faultDrop
	// faultTimeoutLand: Submit reports a timeout, the transaction is pending.
	faultTimeoutLand
	// faultTimeoutDrop: Submit reports a timeout, the transaction is dropped.
	faultTimeoutDrop
)

// faultRun is n consecutive armed faults of one kind.
type faultRun struct {
	kind fault
	n    uint64
}

// arm appends n faults of kind to the queue.
func (c *Chain) arm(kind fault, n uint64) {
	if n == 0 {
		return
	}
	if last := len(c.faults) - 1; last >= 0 && c.faults[last].kind == kind {
		c.faults[last].n = addSat(c.faults[last].n, n)
		return
	}
	c.faults = append(c.faults, faultRun{kind: kind, n: n})
}

// nextFault consumes the fault for the next newly accepted submission.
func (c *Chain) nextFault() fault {
	if len(c.faults) == 0 {
		return faultNone
	}
	head := &c.faults[0]
	kind := head.kind
	head.n--
	if head.n == 0 {
		c.faults = c.faults[1:]
	}
	return kind
}

// DropNext arms n drops: each of the next n newly accepted submissions
// returns its TxID and no error, but the transaction is never placed in the
// mempool, so it stays NOT_FOUND and can be submitted again. Rejected and
// idempotent submissions do not consume a drop. Faults are consumed in the
// order they were armed.
func (c *Chain) DropNext(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.arm(faultDrop, n)
}

// TimeoutButLand arms one "unknown submission" that landed: the next newly
// accepted submission returns its TxID with a SUBMISSION_STATE_UNKNOWN error,
// yet the transaction is pending and will be included like any other.
func (c *Chain) TimeoutButLand() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.arm(faultTimeoutLand, 1)
}

// TimeoutAndDrop arms one "unknown submission" that did not land: the next
// newly accepted submission returns its TxID with a SUBMISSION_STATE_UNKNOWN
// error and the transaction is dropped. Only once its blockhash leaves the
// window (IsBlockhashValid false) is it certain it can never land.
func (c *Chain) TimeoutAndDrop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.arm(faultTimeoutDrop, 1)
}

// DelayInclusion makes every transaction accepted from now on wait the given
// number of blocks before it can be included: accepted at tip height h, it is
// first eligible at height h+1+blocks. Already pending transactions keep
// their eligibility. A delay that outlasts the blockhash window means the
// transaction expires and never lands. DelayInclusion(0) restores immediate
// inclusion.
func (c *Chain) DelayInclusion(blocks uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delay = blocks
}

// ClearFaults disarms every pending submission fault and removes the
// inclusion delay.
func (c *Chain) ClearFaults() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults = nil
	c.delay = 0
}

// ReorgMode says what happens to the transactions of removed blocks.
type ReorgMode uint8

// Reorg modes.
const (
	// ReorgRequeue returns the removed transactions to the mempool (PENDING)
	// in submission order. One whose blockhash belonged to a removed block,
	// or has otherwise left the window, expires at the next block.
	ReorgRequeue ReorgMode = iota + 1
	// ReorgDrop forgets the removed transactions (NOT_FOUND); they may be
	// submitted again while their blockhash is valid.
	ReorgDrop
)

// ReorgResult reports what a Reorg did.
type ReorgResult struct {
	// Removed is the number of blocks removed.
	Removed uint64
	// TxIDs are the removed blocks' transactions in their original order.
	TxIDs []TxID
	// Tip is the canonical tip afterwards.
	Tip BlockInfo
}

// Reorg removes up to depth of the most recent blocks, never reaching a
// finalized block: the number removed is min(depth, tip - finalized height).
// The removed blocks' effects are reverted atomically (all or nothing), their
// hashes leave the blockhash window, and their transactions are requeued or
// forgotten per mode. CONFIRMED and FAILED-but-not-final statuses are thereby
// downgraded; FINALIZED ones are untouched. Slots are not reused: the next
// block takes the next unused slot, as a new fork would.
func (c *Chain) Reorg(depth uint64, mode ReorgMode) (ReorgResult, error) {
	if mode != ReorgRequeue && mode != ReorgDrop {
		return ReorgResult{}, invalid(CodeInvalidArgument, "unknown reorg mode")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	tip := c.tipHeight()
	n := min(depth, tip-c.finalized)
	if n == 0 {
		return ReorgResult{Tip: c.blockInfoLocked(c.tip())}, nil
	}
	keep := tip - n + 1 // heights 0..tip-n stay canonical
	removed := append([]*block(nil), c.blocks[keep:]...)

	// Revert newest first, each block's effects in reverse order. The
	// overlay is committed only if every step succeeds.
	t := c.state.begin()
	for i := len(removed) - 1; i >= 0; i-- {
		effects := removed[i].effects
		for j := len(effects) - 1; j >= 0; j-- {
			if code := t.revert(effects[j]); code != "" {
				return ReorgResult{}, newError(errs.CodeInternal, CodeInvariantViolated,
					"reorg could not revert block "+strconv.FormatUint(removed[i].height, 10)+"; nothing was changed")
			}
		}
	}
	t.commit()

	clear(c.blocks[keep:])
	c.blocks = c.blocks[:keep]
	var ids []TxID
	var requeued []*txRecord
	for _, b := range removed {
		delete(c.hashes, b.hash)
		for _, sig := range b.txs {
			ids = append(ids, TxIDFromSignature(sig))
			rec, ok := c.txs[sig]
			if !ok {
				continue
			}
			if mode == ReorgDrop {
				c.forget(rec)
				continue
			}
			rec.included = false
			rec.height, rec.slot, rec.fee, rec.failure, rec.eligibleAt = 0, 0, 0, "", 0
			requeued = append(requeued, rec)
		}
	}
	c.requeue(requeued)
	return ReorgResult{Removed: n, TxIDs: ids, Tip: c.blockInfoLocked(c.tip())}, nil
}
