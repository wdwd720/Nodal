package devnethost

import (
	"bytes"
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

// Host owns the network of simulated chains and its persistent history.
// Every method is serialised by one mutex, so concurrent callers see one
// order of operations, and that order is the order of the event log.
type Host struct {
	mu    sync.Mutex
	cfg   Config
	store Store
	clk   clock.Clock

	net     *devnet.Network
	applied uint64   // Seq of the last event reflected in net
	head    [32]byte // EventHash of that event
	// broken is set when net may disagree with the committed history and a
	// rebuild failed; every call retries the rebuild before doing anything.
	broken error
}

// draft is a journaled call before it is sealed into an Event.
type draft struct {
	kind    EventKind
	chain   devnet.ChainID
	args    any
	rawTx   []byte
	txID    devnet.TxID
	outcome Outcome
	// deferred is returned to the caller once the event is durable (the
	// SUBMISSION_STATE_UNKNOWN of an injected timeout).
	deferred error
}

// Open loads the network's history from store and replays it, verifying the
// hash chain and every recorded state hash, or writes GENESIS when the
// history is empty. It refuses a history recorded under a different chain
// configuration and a history it cannot reproduce.
func Open(ctx context.Context, store Store, cfg Config, clk clock.Clock) (*Host, error) {
	if store == nil || clk == nil {
		return nil, errs.New(errs.CodeInternal, "devnethost: Open requires a store and a clock")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.Chains = append([]ChainParams(nil), cfg.Chains...)
	h := &Host{cfg: cfg, store: store, clk: clk}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.rebuildLocked(ctx, nil); err != nil {
		return nil, err
	}
	return h, nil
}

// Config returns a copy of the host's configuration.
func (h *Host) Config() Config {
	c := h.cfg
	c.Chains = append([]ChainParams(nil), h.cfg.Chains...)
	return c
}

// Applied returns the sequence number of the last event the host reflects.
func (h *Host) Applied() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.applied
}

// rebuildLocked discards the in-memory network and reconstructs it from the
// committed history, read through q when it is not nil.
func (h *Host) rebuildLocked(ctx context.Context, q db.Querier) error {
	net, err := h.cfg.newNetwork()
	if err != nil {
		return h.fail(err)
	}
	h.net, h.applied, h.head, h.broken = net, 0, [32]byte{}, nil
	events, err := h.store.Since(ctx, q, h.cfg.Network, 0)
	if err != nil {
		return h.fail(err)
	}
	if len(events) > 0 {
		if err := h.replayAllLocked(events); err != nil {
			return h.fail(err)
		}
		return nil
	}
	err = h.store.Exclusive(ctx, h.cfg.Network, 0, func(missed []Event) (*Event, error) {
		if len(missed) > 0 { // another process wrote the genesis first
			return nil, h.replayAllLocked(missed)
		}
		ev, err := h.sealLocked(draft{kind: EventGenesis, args: h.cfg.genesis(), outcome: OutcomeApplied})
		if err != nil {
			return nil, err
		}
		h.applied, h.head = ev.Seq, ev.EventHash
		return &ev, nil
	})
	if err != nil {
		return h.fail(err)
	}
	return nil
}

func (h *Host) fail(err error) error {
	h.broken = err
	return err
}

// usableLocked returns nil when the in-memory network can be trusted,
// rebuilding it first if an earlier failure left it in doubt.
func (h *Host) usableLocked(ctx context.Context, q db.Querier) error {
	if h.broken == nil {
		return nil
	}
	return h.rebuildLocked(ctx, q)
}

func (h *Host) replayAllLocked(events []Event) error {
	for _, ev := range events {
		if err := h.applyLocked(ev); err != nil {
			return err
		}
	}
	return nil
}

// applyLocked verifies one stored event against the hash chain, re-applies
// it and checks that the recorded state hash is reproduced.
func (h *Host) applyLocked(ev Event) error {
	if ev.Network != h.cfg.Network {
		return diverged("event belongs to network " + ev.Network)
	}
	if ev.Seq != h.applied+1 {
		return diverged("event " + strconv.FormatUint(ev.Seq, 10) + " does not follow " + strconv.FormatUint(h.applied, 10))
	}
	if ev.PrevHash != h.head {
		return diverged("event " + strconv.FormatUint(ev.Seq, 10) + " does not link to its predecessor")
	}
	sum, err := ev.computeHash()
	if err != nil {
		return err
	}
	if sum != ev.EventHash {
		return diverged("event " + strconv.FormatUint(ev.Seq, 10) + " does not hash to its recorded event hash")
	}
	if ev.Kind == EventGenesis {
		if ev.Seq != 1 || ev.Chain != "" || ev.Outcome != OutcomeApplied {
			return diverged("GENESIS is malformed")
		}
		want, err := canonicalArgs(h.cfg.genesis())
		if err != nil {
			return err
		}
		if !bytes.Equal(want, ev.Args) {
			return errs.New(errs.CodeInternal, "devnethost: the stored history was produced under a different chain configuration").
				WithField(reasonField, ReasonConfigMismatch).WithField("network", h.cfg.Network)
		}
	} else {
		if ev.Seq == 1 {
			return diverged("the first event is not GENESIS")
		}
		if err := replayEvent(h.net, ev); err != nil {
			return err
		}
	}
	if got := h.net.StateHash(); got != ev.StateHash {
		return diverged("state hash after event " + strconv.FormatUint(ev.Seq, 10) + " is " + got.String() +
			", the history recorded " + ev.StateHash.String())
	}
	h.applied, h.head = ev.Seq, ev.EventHash
	return nil
}

// sealLocked turns a draft into the next event of the history.
func (h *Host) sealLocked(d draft) (Event, error) {
	args, err := canonicalArgs(d.args)
	if err != nil {
		return Event{}, err
	}
	ev := Event{
		Network: h.cfg.Network, Seq: h.applied + 1, Kind: d.kind, Chain: d.chain, Args: args,
		RawTx: d.rawTx, TxID: d.txID, Outcome: d.outcome, StateHash: h.net.StateHash(), PrevHash: h.head,
		OccurredAt: h.clk.Now().UTC().Truncate(time.Microsecond),
	}
	if ev.EventHash, err = ev.computeHash(); err != nil {
		return Event{}, err
	}
	return ev, nil
}

// mutate runs op under the host lock and the store's exclusive lock, after
// catching up with every event other processes committed. op either refuses
// (returning an error, having changed nothing), changes nothing (nil draft),
// or changes the network and describes the change as a draft, which becomes
// the next event. persisted reports that an event was committed; the error is
// then the draft's deferred error. If the store fails after op changed the
// network, the network is rebuilt from what is committed.
func (h *Host) mutate(ctx context.Context, op func(net *devnet.Network) (*draft, error)) (persisted bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.usableLocked(ctx, nil); err != nil {
		return false, err
	}
	var refusal, deferred error
	err = h.store.Exclusive(ctx, h.cfg.Network, h.applied, func(missed []Event) (*Event, error) {
		if err := h.replayAllLocked(missed); err != nil {
			return nil, err
		}
		d, err := op(h.net)
		if err != nil {
			refusal = err
			return nil, nil
		}
		if d == nil {
			return nil, nil
		}
		ev, err := h.sealLocked(*d)
		if err != nil {
			return nil, err
		}
		h.applied, h.head = ev.Seq, ev.EventHash
		deferred = d.deferred
		persisted = true
		return &ev, nil
	})
	if err != nil {
		// Whatever op did is not durable, or the catch-up diverged: the
		// committed history is the only truth, so rebuild from it.
		_ = h.rebuildLocked(ctx, nil)
		return false, err
	}
	if refusal != nil {
		return false, refusal
	}
	return persisted, deferred
}

// read runs fn on the network after catching up with the committed history
// (through q when it is not nil). A read that cannot confirm it is current
// fails closed.
func (h *Host) read(ctx context.Context, q db.Querier, fn func(net *devnet.Network) error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.usableLocked(ctx, q); err != nil {
		return err
	}
	missed, err := h.store.Since(ctx, q, h.cfg.Network, h.applied)
	if err != nil {
		return err
	}
	if err := h.replayAllLocked(missed); err != nil {
		_ = h.rebuildLocked(ctx, q)
		return err
	}
	return fn(h.net)
}

func chainOf(net *devnet.Network, id devnet.ChainID) (*devnet.Chain, error) { return net.Chain(id) }

// --- Mutations --------------------------------------------------------------

// Submit submits a signed transaction to chain and returns its TxID. A
// submission the chain refuses changes nothing and is not journaled. A
// transaction the chain already holds is acknowledged without a new event,
// exactly as the chain does. Every newly accepted submission -- including one
// an armed fault drops or times out -- is journaled with its raw bytes before
// Submit returns, so a restart replays it.
//
// A SUBMISSION_STATE_UNKNOWN error means the outcome is unknown: either the
// chain's own injected timeout, or the chain accepted the transaction but
// the event store failed, so it may or may not be part of the durable
// history. The TxID is returned with it; observe Status before acting again,
// and never resubmit blindly.
func (h *Host) Submit(ctx context.Context, chain devnet.ChainID, raw []byte) (devnet.TxID, error) {
	var id devnet.TxID
	var drafted bool
	persisted, err := h.mutate(ctx, func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil {
			return nil, err
		}
		if known, ok := txIDOf(raw); ok && c.Status(known).State != devnet.TxNotFound {
			id = known
			_, err := c.Submit(raw) // an acknowledgement, or TXID_CONFLICT; never a change
			return nil, err
		}
		got, err := c.Submit(raw)
		d := &draft{kind: EventSubmit, chain: chain, rawTx: bytes.Clone(raw), txID: got, args: submitArgs{TxID: string(got)}}
		switch {
		case err == nil:
			d.outcome = OutcomeAccepted
		case devnet.ReasonOf(err) == devnet.CodeSubmissionTimeout:
			d.outcome, d.deferred = OutcomeTimeout, err
		default:
			return nil, err
		}
		id, drafted = got, true
		return d, nil
	})
	if err != nil && drafted && !persisted {
		return id, errs.Wrap(err, errs.CodeSubmissionStateUnknown,
			"devnethost: the chain accepted the transaction but recording it failed; observe its status before acting again").
			WithField("tx_id", string(id)).WithField(reasonField, ReasonSubmissionPersistenceUnknown)
	}
	return id, err
}

// txIDOf returns the TxID raw would have, when raw decodes and is signed.
func txIDOf(raw []byte) (devnet.TxID, bool) {
	tx, err := devnet.Decode(raw)
	if err != nil {
		return "", false
	}
	id, err := tx.ID()
	if err != nil {
		return "", false
	}
	return id, true
}

// ProduceBlocks produces n blocks on chain (1..MaxBlocksPerCall) and returns
// the new tip. ProduceBlocks(0) returns the tip and journals nothing.
func (h *Host) ProduceBlocks(ctx context.Context, chain devnet.ChainID, n uint64) (devnet.BlockInfo, error) {
	if n > MaxBlocksPerCall {
		return devnet.BlockInfo{}, errs.Newf(errs.CodeValidationFailed, "devnethost: at most %d blocks per call", MaxBlocksPerCall)
	}
	if n == 0 {
		return h.Tip(ctx, chain)
	}
	var tip devnet.BlockInfo
	_, err := h.mutate(ctx, func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil {
			return nil, err
		}
		tip = c.ProduceBlocks(n)
		return &draft{kind: EventProduceBlocks, chain: chain, outcome: OutcomeApplied, args: produceArgs{
			Blocks: strconv.FormatUint(n, 10), TipHeight: strconv.FormatUint(tip.Height, 10), TipHash: tip.Hash.String(),
		}}, nil
	})
	return tip, err
}

// EnsureMint creates the mint unless it exists. An existing mint with the
// same decimals and authority is success; one with a different configuration
// is CONFLICT. Issuance itself is always a signed TOKEN_MINT_TO transaction.
func (h *Host) EnsureMint(ctx context.Context, chain devnet.ChainID, cfg devnet.MintConfig) error {
	_, err := h.mutate(ctx, func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil {
			return nil, err
		}
		if info, err := c.Mint(cfg.Address); err == nil {
			if info.Decimals != cfg.Decimals || info.Authority != cfg.Authority {
				return nil, errs.New(errs.CodeConflict, "devnethost: the mint exists with a different configuration").
					WithField("mint", cfg.Address.String())
			}
			return nil, nil
		}
		if err := c.CreateMint(cfg); err != nil {
			return nil, err
		}
		return &draft{kind: EventCreateMint, chain: chain, outcome: OutcomeApplied, args: createMintArgs{
			Address: cfg.Address.String(), Decimals: int(cfg.Decimals), Authority: cfg.Authority.String(), Frozen: cfg.Frozen,
		}}, nil
	})
	return err
}

// Airdrop credits new native units (the sandbox faucet) to an account.
func (h *Host) Airdrop(ctx context.Context, chain devnet.ChainID, to devnet.PublicKey, lamports uint64) error {
	_, err := h.mutate(ctx, func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil {
			return nil, err
		}
		if err := c.Airdrop(to, lamports); err != nil {
			return nil, err
		}
		return airdropDraft(chain, to, lamports), nil
	})
	return err
}

// EnsureNativeBalance tops an account up with topUp native units when its
// balance is below minimum, atomically with the check: concurrent callers
// cannot both top the same account up for the same shortfall.
func (h *Host) EnsureNativeBalance(ctx context.Context, chain devnet.ChainID, owner devnet.PublicKey, minimum, topUp uint64) error {
	if topUp < minimum {
		return errs.New(errs.CodeValidationFailed, "devnethost: a top-up smaller than the minimum cannot restore it")
	}
	_, err := h.mutate(ctx, func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil {
			return nil, err
		}
		if c.Balance(owner) >= minimum {
			return nil, nil
		}
		if err := c.Airdrop(owner, topUp); err != nil {
			return nil, err
		}
		return airdropDraft(chain, owner, topUp), nil
	})
	return err
}

func airdropDraft(chain devnet.ChainID, to devnet.PublicKey, lamports uint64) *draft {
	return &draft{kind: EventAirdrop, chain: chain, outcome: OutcomeApplied,
		args: airdropArgs{To: to.String(), Lamports: strconv.FormatUint(lamports, 10)}}
}

// --- Fault injection (LOCAL, TEST and DEV only) -------------------------------

func (h *Host) fault(ctx context.Context, name string, op func(net *devnet.Network) (*draft, error)) error {
	if !h.cfg.FaultInjectionAllowed() {
		return faultForbidden(name)
	}
	_, err := h.mutate(ctx, op)
	return err
}

// DropNext arms n drops on chain (see devnet.Chain.DropNext).
func (h *Host) DropNext(ctx context.Context, chain devnet.ChainID, n uint64) error {
	if n > MaxFaultCount {
		return errs.Newf(errs.CodeValidationFailed, "devnethost: at most %d faults per call", MaxFaultCount)
	}
	return h.fault(ctx, "DropNext", func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil || n == 0 {
			return nil, err
		}
		c.DropNext(n)
		return &draft{kind: EventDropNext, chain: chain, outcome: OutcomeApplied, args: countArgs{N: strconv.FormatUint(n, 10)}}, nil
	})
}

// TimeoutButLand arms one unknown submission that lands.
func (h *Host) TimeoutButLand(ctx context.Context, chain devnet.ChainID) error {
	return h.fault(ctx, "TimeoutButLand", func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil {
			return nil, err
		}
		c.TimeoutButLand()
		return &draft{kind: EventTimeoutButLand, chain: chain, outcome: OutcomeApplied, args: noArgs{}}, nil
	})
}

// TimeoutAndDrop arms one unknown submission that does not land.
func (h *Host) TimeoutAndDrop(ctx context.Context, chain devnet.ChainID) error {
	return h.fault(ctx, "TimeoutAndDrop", func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil {
			return nil, err
		}
		c.TimeoutAndDrop()
		return &draft{kind: EventTimeoutAndDrop, chain: chain, outcome: OutcomeApplied, args: noArgs{}}, nil
	})
}

// DelayInclusion holds every transaction accepted afterwards for blocks
// blocks (0 restores immediate inclusion).
func (h *Host) DelayInclusion(ctx context.Context, chain devnet.ChainID, blocks uint64) error {
	if blocks > MaxFaultCount {
		return errs.Newf(errs.CodeValidationFailed, "devnethost: a delay of at most %d blocks", MaxFaultCount)
	}
	return h.fault(ctx, "DelayInclusion", func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil {
			return nil, err
		}
		c.DelayInclusion(blocks)
		return &draft{kind: EventDelayInclusion, chain: chain, outcome: OutcomeApplied, args: countArgs{N: strconv.FormatUint(blocks, 10)}}, nil
	})
}

// ClearFaults disarms every submission fault and removes the delay.
func (h *Host) ClearFaults(ctx context.Context, chain devnet.ChainID) error {
	return h.fault(ctx, "ClearFaults", func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil {
			return nil, err
		}
		c.ClearFaults()
		return &draft{kind: EventClearFaults, chain: chain, outcome: OutcomeApplied, args: noArgs{}}, nil
	})
}

// Reorg removes up to depth non-finalized blocks (see devnet.Chain.Reorg).
// A finalized block is never removed.
func (h *Host) Reorg(ctx context.Context, chain devnet.ChainID, depth uint64, mode devnet.ReorgMode) (devnet.ReorgResult, error) {
	var res devnet.ReorgResult
	name, ok := reorgModeName(mode)
	if !ok {
		return res, errs.New(errs.CodeValidationFailed, "devnethost: unknown reorg mode")
	}
	err := h.fault(ctx, "Reorg", func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil {
			return nil, err
		}
		if res, err = c.Reorg(depth, mode); err != nil || res.Removed == 0 {
			return nil, err
		}
		return &draft{kind: EventReorg, chain: chain, outcome: OutcomeApplied, args: reorgArgs{
			Depth: strconv.FormatUint(depth, 10), Mode: name, Removed: strconv.FormatUint(res.Removed, 10),
		}}, nil
	})
	return res, err
}

// SetMintFrozen freezes or unfreezes a mint. In this host it is a fault: the
// platform's own pause is the asset's status, and the chain freeze exists to
// make a submitted transaction fail on chain in a test.
func (h *Host) SetMintFrozen(ctx context.Context, chain devnet.ChainID, mint devnet.PublicKey, frozen bool) error {
	return h.fault(ctx, "SetMintFrozen", func(net *devnet.Network) (*draft, error) {
		c, err := chainOf(net, chain)
		if err != nil {
			return nil, err
		}
		info, err := c.Mint(mint)
		if err != nil || info.Frozen == frozen {
			return nil, err
		}
		if err := c.SetMintFrozen(mint, frozen); err != nil {
			return nil, err
		}
		return &draft{kind: EventSetMintFrozen, chain: chain, outcome: OutcomeApplied, args: setMintFrozenArgs{Mint: mint.String(), Frozen: frozen}}, nil
	})
}

// --- Reads --------------------------------------------------------------------

// Status returns a transaction's status on chain.
func (h *Host) Status(ctx context.Context, chain devnet.ChainID, id devnet.TxID) (devnet.TxStatus, error) {
	var st devnet.TxStatus
	err := h.read(ctx, nil, func(net *devnet.Network) error {
		c, err := chainOf(net, chain)
		if err != nil {
			return err
		}
		st = c.Status(id)
		return nil
	})
	return st, err
}

// TokenBalance returns owner's balance of mint on chain.
func (h *Host) TokenBalance(ctx context.Context, chain devnet.ChainID, owner, mint devnet.PublicKey) (uint64, error) {
	var bal uint64
	err := h.read(ctx, nil, func(net *devnet.Network) error {
		c, err := chainOf(net, chain)
		if err != nil {
			return err
		}
		bal = c.TokenBalance(owner, mint)
		return nil
	})
	return bal, err
}

// NativeBalance returns owner's native balance on chain.
func (h *Host) NativeBalance(ctx context.Context, chain devnet.ChainID, owner devnet.PublicKey) (uint64, error) {
	var bal uint64
	err := h.read(ctx, nil, func(net *devnet.Network) error {
		c, err := chainOf(net, chain)
		if err != nil {
			return err
		}
		bal = c.Balance(owner)
		return nil
	})
	return bal, err
}

// Supply returns mint's supply on chain.
func (h *Host) Supply(ctx context.Context, chain devnet.ChainID, mint devnet.PublicKey) (uint64, error) {
	var s uint64
	err := h.read(ctx, nil, func(net *devnet.Network) error {
		c, err := chainOf(net, chain)
		if err != nil {
			return err
		}
		s, err = c.Supply(mint)
		return err
	})
	return s, err
}

// Mint returns a mint's configuration and supply.
func (h *Host) Mint(ctx context.Context, chain devnet.ChainID, mint devnet.PublicKey) (devnet.MintInfo, error) {
	var info devnet.MintInfo
	err := h.read(ctx, nil, func(net *devnet.Network) error {
		c, err := chainOf(net, chain)
		if err != nil {
			return err
		}
		info, err = c.Mint(mint)
		return err
	})
	return info, err
}

// StateHash returns the network's StateHash.
func (h *Host) StateHash(ctx context.Context) (devnet.Hash, error) {
	var sh devnet.Hash
	err := h.read(ctx, nil, func(net *devnet.Network) error {
		sh = net.StateHash()
		return nil
	})
	return sh, err
}

// LatestBlockhash returns chain's tip hash, the blockhash a new transaction
// references.
func (h *Host) LatestBlockhash(ctx context.Context, chain devnet.ChainID) (devnet.Hash, error) {
	var bh devnet.Hash
	err := h.read(ctx, nil, func(net *devnet.Network) error {
		c, err := chainOf(net, chain)
		if err != nil {
			return err
		}
		bh = c.LatestBlockhash()
		return nil
	})
	return bh, err
}

// IsBlockhashValid reports whether a transaction referencing bh could still
// be accepted on chain. Once it is false for a NOT_FOUND transaction, that
// transaction can never land.
func (h *Host) IsBlockhashValid(ctx context.Context, chain devnet.ChainID, bh devnet.Hash) (bool, error) {
	var ok bool
	err := h.read(ctx, nil, func(net *devnet.Network) error {
		c, err := chainOf(net, chain)
		if err != nil {
			return err
		}
		ok = c.IsBlockhashValid(bh)
		return nil
	})
	return ok, err
}

// Tip returns chain's canonical tip.
func (h *Host) Tip(ctx context.Context, chain devnet.ChainID) (devnet.BlockInfo, error) {
	var tip devnet.BlockInfo
	err := h.read(ctx, nil, func(net *devnet.Network) error {
		c, err := chainOf(net, chain)
		if err != nil {
			return err
		}
		tip = c.Tip()
		return nil
	})
	return tip, err
}

// GetTransaction returns a transaction held by chain.
func (h *Host) GetTransaction(ctx context.Context, chain devnet.ChainID, id devnet.TxID) (devnet.TxRecord, error) {
	var rec devnet.TxRecord
	err := h.read(ctx, nil, func(net *devnet.Network) error {
		c, err := chainOf(net, chain)
		if err != nil {
			return err
		}
		rec, err = c.GetTransaction(id)
		return err
	})
	return rec, err
}

// LockedTransfer returns the BRIDGE_LOCK carried by transaction id on chain,
// with its current status.
func (h *Host) LockedTransfer(ctx context.Context, chain devnet.ChainID, id devnet.TxID) (devnet.LockedTransfer, error) {
	var lt devnet.LockedTransfer
	err := h.read(ctx, nil, func(net *devnet.Network) error {
		c, err := chainOf(net, chain)
		if err != nil {
			return err
		}
		lt, err = c.LockedTransfer(id)
		return err
	})
	return lt, err
}

// Health returns nil when the host is consistent with its history and runs
// chain.
func (h *Host) Health(ctx context.Context, chain devnet.ChainID) error {
	return h.read(ctx, nil, func(net *devnet.Network) error {
		_, err := chainOf(net, chain)
		return err
	})
}
