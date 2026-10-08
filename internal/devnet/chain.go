package devnet

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"slices"
	"strconv"
	"sync"

	"github.com/nodal/controlplane/internal/errs"
)

// Values DefaultChainConfig uses.
const (
	// DefaultBlockhashWindow is how many recent canonical blocks a
	// transaction's blockhash may come from (Solana's max processing age).
	DefaultBlockhashWindow = 150
	// DefaultFinalityDepth is how many further blocks finalize a block.
	DefaultFinalityDepth = 32
	// DefaultFeePerTx is the flat fee in native units (lamports).
	DefaultFeePerTx = 5000
)

// BridgeConfig enables the bridge programs on a chain. Without one,
// BRIDGE_LOCK and BRIDGE_RELEASE fail with BRIDGE_NOT_CONFIGURED.
type BridgeConfig struct {
	// Authority is the key whose signature authorises BRIDGE_RELEASE on this
	// chain: the attester a relayer must get to sign.
	Authority PublicKey
}

// ChainConfig configures one chain. No field has a zero-value default;
// DefaultChainConfig fills in the usual values.
type ChainConfig struct {
	// ID names the chain; every message must carry it.
	ID ChainID
	// BlockhashWindow is how many of the most recent canonical blocks
	// (the tip included) a transaction's recent blockhash may come from.
	// At least 1.
	BlockhashWindow uint64
	// FinalityDepth is how many further blocks make a block FINALIZED. At
	// least 1; a reorg can never reach below it.
	FinalityDepth uint64
	// FeePerTx is the flat fee, in native units, every included transaction
	// pays when its fee payer can afford it. Zero makes the chain free.
	FeePerTx uint64
	// FeeCollector receives the fees, so native supply is conserved.
	FeeCollector PublicKey
	// Bridge enables BRIDGE_LOCK and BRIDGE_RELEASE; nil disables them.
	Bridge *BridgeConfig
}

// DefaultChainConfig returns a configuration for id with a 150-block
// blockhash window, a 32-block finality depth, a 5000-unit fee collected at
// FeeCollectorAddress(id), and no bridge.
func DefaultChainConfig(id ChainID) ChainConfig {
	return ChainConfig{
		ID:              id,
		BlockhashWindow: DefaultBlockhashWindow,
		FinalityDepth:   DefaultFinalityDepth,
		FeePerTx:        DefaultFeePerTx,
		FeeCollector:    FeeCollectorAddress(id),
	}
}

// Validate checks the configuration.
func (c ChainConfig) Validate() error {
	if err := c.ID.Validate(); err != nil {
		return err
	}
	switch {
	case c.BlockhashWindow == 0:
		return invalid(CodeInvalidConfig, "blockhash window must be at least 1 block")
	case c.FinalityDepth == 0:
		return invalid(CodeInvalidConfig, "finality depth must be at least 1 block")
	case c.FeeCollector.IsZero():
		return invalid(CodeInvalidConfig, "fee collector is the zero key")
	case c.Bridge != nil && c.Bridge.Authority.IsZero():
		return invalid(CodeInvalidConfig, "bridge authority is the zero key")
	}
	return nil
}

// clone returns a copy that shares no memory with c.
func (c ChainConfig) clone() ChainConfig {
	if c.Bridge != nil {
		b := *c.Bridge
		c.Bridge = &b
	}
	return c
}

// TxState is where a transaction is in its lifecycle.
type TxState uint8

// Transaction states.
const (
	// TxNotFound: never accepted, dropped, expired before inclusion, or
	// forgotten by a ReorgDrop.
	TxNotFound TxState = iota
	// TxPending: accepted and waiting in the mempool.
	TxPending
	// TxConfirmed: executed successfully in a canonical block that a reorg
	// can still remove.
	TxConfirmed
	// TxFinalized: executed successfully in a finalized block. Permanent.
	TxFinalized
	// TxFailed: included in a canonical block but failed; only the fee
	// moved. Permanent once TxStatus.Final.
	TxFailed
)

// String returns NOT_FOUND, PENDING, CONFIRMED, FINALIZED or FAILED.
func (s TxState) String() string {
	switch s {
	case TxNotFound:
		return "NOT_FOUND"
	case TxPending:
		return "PENDING"
	case TxConfirmed:
		return "CONFIRMED"
	case TxFinalized:
		return "FINALIZED"
	case TxFailed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

// TxStatus is a transaction's status. Slot and Height locate the including
// block for CONFIRMED, FINALIZED and FAILED; Err is the failure for FAILED;
// Final reports that the including block is finalized, so the status can
// never change again.
type TxStatus struct {
	State  TxState
	Slot   uint64
	Height uint64
	Err    ErrorCode
	Final  bool
}

// BlockInfo describes one canonical block.
type BlockInfo struct {
	Slot   uint64
	Height uint64
	Hash   Hash
	Parent Hash
	// TxIDs are the included transactions in execution order.
	TxIDs []TxID
	// Final reports that the block is finalized.
	Final bool
}

// TxRecord is a transaction as the chain holds it.
type TxRecord struct {
	ID          TxID
	Transaction Transaction
	Status      TxStatus
	// Fee is the fee charged: zero while pending and for
	// INSUFFICIENT_FUNDS_FOR_FEE.
	Fee uint64
}

// Chain is one simulated chain. It is safe for concurrent use; every method
// is serialised by one mutex, and the configuration is immutable.
type Chain struct {
	cfg ChainConfig // immutable after NewChain

	mu    sync.Mutex
	state ledger

	blocks    []*block        // canonical chain; blocks[h].height == h; blocks[0] is genesis
	hashes    map[Hash]uint64 // canonical block hash -> height
	finalized uint64          // highest finalized height; never decreases
	slot      uint64          // last slot used; never decreases, even across reorgs

	txs      map[Signature]*txRecord // every pending or canonically included transaction
	messages map[Hash]Signature      // message hash -> tx, over the same population as txs
	mempool  []*txRecord             // pending, ascending seq
	seq      uint64                  // last submission sequence number

	faults []faultRun // armed one-shot submission faults, consumed in order
	delay  uint64     // blocks newly accepted transactions wait before inclusion
}

// block is one canonical block.
type block struct {
	slot    uint64
	height  uint64
	hash    Hash
	parent  Hash
	txs     []Signature
	effects []effect // forward effects in execution order; nil once finalized
}

// txRecord is one pending or included transaction.
type txRecord struct {
	id         Signature
	msgHash    Hash
	raw        []byte // the submitted bytes, owned by the chain
	tx         Transaction
	seq        uint64
	eligibleAt uint64 // pending: the first block height that may include it

	included bool
	height   uint64
	slot     uint64
	failure  ErrorCode
	fee      uint64
}

// NewChain returns a chain at its genesis block (slot 0, height 0, final).
func NewChain(cfg ChainConfig) (*Chain, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.clone()
	genesis := &block{hash: genesisHash(cfg.ID)}
	return &Chain{
		cfg:      cfg,
		state:    newLedger(),
		blocks:   []*block{genesis},
		hashes:   map[Hash]uint64{genesis.hash: 0},
		txs:      map[Signature]*txRecord{},
		messages: map[Hash]Signature{},
	}, nil
}

// ID returns the chain id.
func (c *Chain) ID() ChainID { return c.cfg.ID }

// Config returns a copy of the configuration.
func (c *Chain) Config() ChainConfig { return c.cfg.clone() }

// Submit accepts a signed transaction into the mempool and returns its TxID.
//
// It decodes raw, checks the chain id, the message rules and every
// signature, then refuses a message it already holds under another TxID
// (DUPLICATE_MESSAGE) or a known TxID with a different message
// (TXID_CONFLICT). Resubmitting a known transaction is idempotent: it returns
// the TxID and no error and changes nothing, whatever the transaction's
// status. A new transaction must reference a blockhash within the window
// (BLOCKHASH_NOT_FOUND otherwise) and is then subject to the armed faults:
// it may be dropped, or Submit may report SUBMISSION_STATE_UNKNOWN. On that
// timeout the TxID is returned with the error, as a real client knows its
// signature before it sends.
func (c *Chain) Submit(raw []byte) (TxID, error) {
	cm, err := check(c.cfg.ID, raw)
	if err != nil {
		return "", err
	}
	id := TxIDFromSignature(cm.id)

	c.mu.Lock()
	defer c.mu.Unlock()
	if rec, known := c.txs[cm.id]; known {
		if rec.msgHash != cm.msgHash {
			return "", newError(errs.CodeConflict, CodeTxIDConflict,
				"transaction id "+string(id)+" is already used by a different message")
		}
		return id, nil
	}
	if other, held := c.messages[cm.msgHash]; held {
		existing := string(TxIDFromSignature(other))
		return "", newError(errs.CodeConflict, CodeDuplicateMessage,
			"message is already held as transaction "+existing).WithField("existing_tx_id", existing)
	}
	if !c.blockhashValidLocked(cm.tx.Message.RecentBlockhash) {
		return "", invalid(CodeBlockhashNotFound, "recent blockhash "+cm.tx.Message.RecentBlockhash.String()+
			" is not among the last "+strconv.FormatUint(c.cfg.BlockhashWindow, 10)+" canonical blocks")
	}

	switch f := c.nextFault(); f {
	case faultDrop:
		return id, nil
	case faultTimeoutDrop:
		return id, timeoutError(id)
	default:
		c.seq++
		rec := &txRecord{
			id:         cm.id,
			msgHash:    cm.msgHash,
			raw:        bytes.Clone(raw),
			tx:         cm.tx,
			seq:        c.seq,
			eligibleAt: addSat(addSat(c.tipHeight(), 1), c.delay),
		}
		c.txs[cm.id] = rec
		c.messages[cm.msgHash] = cm.id
		c.mempool = append(c.mempool, rec) // the newest seq, so the mempool stays ordered
		if f == faultTimeoutLand {
			return id, timeoutError(id)
		}
		return id, nil
	}
}

// timeoutError is the injected transport timeout.
func timeoutError(id TxID) error {
	return newError(errs.CodeSubmissionStateUnknown, CodeSubmissionTimeout,
		"transport timed out after sending; the transaction may or may not land, so observe Status before acting again").
		WithField("tx_id", string(id))
}

// ProduceBlock advances the chain by one slot and one block. Pending
// transactions are considered in submission order: one whose blockhash has
// left the window is forgotten, one still delayed stays pending, and every
// other one is executed atomically and included. Blocks that reach the
// finality depth become final. It returns the new block.
func (c *Chain) ProduceBlock() BlockInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.produceLocked()
}

// ProduceBlocks produces n blocks and returns the last, or the current tip
// when n is zero.
func (c *Chain) ProduceBlocks(n uint64) BlockInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n == 0 {
		return c.blockInfoLocked(c.tip())
	}
	var info BlockInfo
	for i := uint64(0); i < n; i++ {
		info = c.produceLocked()
	}
	return info
}

func (c *Chain) produceLocked() BlockInfo {
	parent := c.tip()
	c.slot++
	b := &block{slot: c.slot, height: parent.height + 1, parent: parent.hash}
	kept := make([]*txRecord, 0, len(c.mempool))
	for _, rec := range c.mempool {
		switch {
		case !c.blockhashValidLocked(rec.tx.Message.RecentBlockhash):
			c.forget(rec) // expired: it can never land
		case rec.eligibleAt > b.height:
			kept = append(kept, rec)
		default:
			c.include(b, rec)
		}
	}
	c.mempool = kept
	b.hash = blockHash(c.cfg.ID, b.slot, b.height, b.parent, b.txs)
	c.blocks = append(c.blocks, b)
	c.hashes[b.hash] = b.height
	c.advanceFinality()
	return c.blockInfoLocked(b)
}

// include executes rec and appends it to b.
func (c *Chain) include(b *block, rec *txRecord) {
	fee, failure, effects := c.execute(rec)
	rec.included = true
	rec.height, rec.slot = b.height, b.slot
	rec.fee, rec.failure = fee, failure
	rec.eligibleAt = 0
	b.txs = append(b.txs, rec.id)
	b.effects = append(b.effects, effects...)
}

// advanceFinality finalizes every block at least FinalityDepth below the tip.
// A finalized block can never be reverted, so its journal is released.
func (c *Chain) advanceFinality() {
	tip := c.tipHeight()
	if tip < c.cfg.FinalityDepth {
		return
	}
	target := tip - c.cfg.FinalityDepth
	for h := c.finalized + 1; h <= target; h++ {
		c.blocks[h].effects = nil
	}
	if target > c.finalized {
		c.finalized = target
	}
}

func (c *Chain) tip() *block       { return c.blocks[len(c.blocks)-1] }
func (c *Chain) tipHeight() uint64 { return c.tip().height }

// blockhashValidLocked reports whether h is the hash of one of the
// BlockhashWindow most recent canonical blocks.
func (c *Chain) blockhashValidLocked(h Hash) bool {
	height, ok := c.hashes[h]
	return ok && c.tipHeight()-height < c.cfg.BlockhashWindow
}

// forget removes a transaction the chain will never include: it becomes
// NOT_FOUND and its TxID and message may be submitted again.
func (c *Chain) forget(rec *txRecord) {
	delete(c.txs, rec.id)
	delete(c.messages, rec.msgHash)
}

func (c *Chain) statusLocked(rec *txRecord) TxStatus {
	if !rec.included {
		return TxStatus{State: TxPending}
	}
	st := TxStatus{Slot: rec.slot, Height: rec.height, Final: rec.height <= c.finalized}
	switch {
	case rec.failure != "":
		st.State, st.Err = TxFailed, rec.failure
	case st.Final:
		st.State = TxFinalized
	default:
		st.State = TxConfirmed
	}
	return st
}

func (c *Chain) blockInfoLocked(b *block) BlockInfo {
	ids := make([]TxID, len(b.txs))
	for i, s := range b.txs {
		ids[i] = TxIDFromSignature(s)
	}
	return BlockInfo{
		Slot:   b.slot,
		Height: b.height,
		Hash:   b.hash,
		Parent: b.parent,
		TxIDs:  ids,
		Final:  b.height <= c.finalized,
	}
}

// requeue returns transactions to the mempool, which stays in submission
// order. Sequence numbers are unique, so the order is total.
func (c *Chain) requeue(recs []*txRecord) {
	c.mempool = append(c.mempool, recs...)
	slices.SortFunc(c.mempool, func(a, b *txRecord) int { return cmp.Compare(a.seq, b.seq) })
}

// genesisHash is the hash of a chain's genesis block.
func genesisHash(chain ChainID) Hash {
	w := &writer{}
	w.str(genesisDomain)
	w.label(string(chain))
	return Hash(sha256.Sum256(w.buf))
}

// blockHash commits to a block's chain, slot, height, parent and
// transactions. Slots never repeat, so a block produced after a reorg never
// reuses an orphaned block's hash.
func blockHash(chain ChainID, slot, height uint64, parent Hash, txs []Signature) Hash {
	w := &writer{buf: make([]byte, 0, 128+len(txs)*SignatureSize)}
	w.str(blockDomain)
	w.label(string(chain))
	w.u64(slot)
	w.u64(height)
	w.hash(parent)
	w.length(len(txs))
	for _, s := range txs {
		w.signature(s)
	}
	return Hash(sha256.Sum256(w.buf))
}
