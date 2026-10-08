package delegation

import (
	"bytes"
	"context"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
)

// ChainReader is what an admission needs of the simulated chains: their
// configuration (the network, each chain's fee) and whether a chain is
// healthy. It submits nothing, produces no block and funds no wallet.
// *devnethost.Host satisfies it, and so does a host whose history refuses
// every append.
type ChainReader interface {
	Config() devnethost.Config
	Health(ctx context.Context, chain devnet.ChainID) error
}

// ChainHost is what this package needs of the simulated chains to carry an
// admitted swap or withdrawal to its end. *devnethost.Host satisfies it; it
// never holds a key or signs.
type ChainHost interface {
	ChainReader
	ViewIn(ctx context.Context, q db.Querier, fn func(v devnethost.View) error) error
	Submit(ctx context.Context, chain devnet.ChainID, raw []byte) (devnet.TxID, error)
	GetTransaction(ctx context.Context, chain devnet.ChainID, txID devnet.TxID) (devnet.TxRecord, error)
	ProduceBlocks(ctx context.Context, chain devnet.ChainID, n uint64) (devnet.BlockInfo, error)
	EnsureNativeBalance(ctx context.Context, chain devnet.ChainID, owner devnet.PublicKey, minimum, topUp uint64) error
}

// HostFees reads the fee per transaction of each chain from the host's
// configuration: the fee the signing policy holds a plan to.
func HostFees(h ChainReader) ChainFees {
	return func(chain devnet.ChainID) (uint64, bool) {
		p, ok := h.Config().Params(chain)
		return p.FeePerTx, ok
	}
}

// Submission outcomes.
const (
	execSubmitted = "SUBMITTED"
	execUnknown   = "UNKNOWN"
	execRefused   = "REFUSED"
)

// execution is how the one submission of a signed transaction ended.
type execution struct {
	outcome string
	err     error
}

// definiteRefusals are rejections that prove the chain did not accept the
// transaction and never will.
var definiteRefusals = map[devnet.ErrorCode]bool{
	devnet.CodeMalformedTransaction: true, devnet.CodeInvalidMessage: true, devnet.CodeInvalidChainID: true,
	devnet.CodeInvalidBase58: true, devnet.CodeWrongChain: true, devnet.CodeBlockhashNotFound: true,
	devnet.CodeSignatureMissing: true, devnet.CodeSignatureExtra: true, devnet.CodeSignatureInvalid: true,
}

// submitOnce submits raw exactly once and classifies the answer. Anything
// that is not a definite acceptance or a definite refusal is UNKNOWN.
func submitOnce(ctx context.Context, h ChainHost, chain devnet.ChainID, raw []byte) execution {
	_, err := h.Submit(ctx, chain, raw)
	switch {
	case err == nil:
		return execution{outcome: execSubmitted}
	case definiteRefusals[devnet.ReasonOf(err)]:
		return execution{outcome: execRefused, err: err}
	default:
		return execution{outcome: execUnknown, err: err}
	}
}

// observation is what a chain says about one transaction at one instant.
type observation struct {
	State           string          `json:"state"`
	Status          devnet.TxStatus `json:"-"`
	Slot            uint64          `json:"-"`
	FinalizedHeight uint64          `json:"-"`
	// Expired: the chain does not hold the transaction and its blockhash is a
	// full window below the finalized height, so it can never land.
	Expired bool `json:"expired"`
}

func (o observation) finalized() bool   { return o.Status.State == devnet.TxFinalized }
func (o observation) failedFinal() bool { return o.Status.State == devnet.TxFailed && o.Status.Final }
func (o observation) landed() bool {
	return o.Status.State == devnet.TxConfirmed || o.Status.State == devnet.TxFinalized
}

// observe reads a transaction's status and the chain's finalized height in
// one consistent view, through q when it is a transaction (F-27).
func observe(ctx context.Context, h ChainHost, q db.Querier, chain devnet.ChainID, txID devnet.TxID, bhHeight uint64) (observation, error) {
	params, ok := h.Config().Params(chain)
	if !ok {
		return observation{}, errs.New(errs.CodeValidationFailed, "delegation: the host does not run chain "+string(chain))
	}
	var obs observation
	err := h.ViewIn(ctx, q, func(v devnethost.View) error {
		st, err := v.Status(chain, txID)
		if err != nil {
			return err
		}
		finalized, err := v.FinalizedHeight(chain)
		if err != nil {
			return err
		}
		obs = observation{State: st.State.String(), Status: st, Slot: st.Slot, FinalizedHeight: finalized,
			Expired: st.State == devnet.TxNotFound && finalized >= bhHeight+params.BlockhashWindow}
		return nil
	})
	return obs, err
}

// tipOf returns a chain's tip through q.
func tipOf(ctx context.Context, h ChainHost, q db.Querier, chain devnet.ChainID) (devnet.BlockInfo, error) {
	var tip devnet.BlockInfo
	err := h.ViewIn(ctx, q, func(v devnethost.View) error {
		var err error
		tip, err = v.Tip(chain)
		return err
	})
	return tip, err
}

// awaitFinal observes a transaction until its outcome is final. When produce
// is set (SANDBOX mode) it produces the blocks that finality needs; otherwise
// (DEVNET) it observes once. It never submits anything.
func awaitFinal(ctx context.Context, h ChainHost, chain devnet.ChainID, txID devnet.TxID, bhHeight uint64, produce bool) (observation, error) {
	params, ok := h.Config().Params(chain)
	if !ok {
		return observation{}, errs.New(errs.CodeValidationFailed, "delegation: the host does not run chain "+string(chain))
	}
	budget := params.BlockhashWindow + params.FinalityDepth + 2
	for round := uint64(0); ; round++ {
		obs, err := observe(ctx, h, nil, chain, txID, bhHeight)
		if err != nil || obs.finalized() || obs.failedFinal() || obs.Expired || !produce || round >= budget {
			return obs, err
		}
		n := uint64(1)
		switch obs.Status.State {
		case devnet.TxConfirmed, devnet.TxFailed:
			var tip devnet.BlockInfo
			if tip, err = tipOf(ctx, h, nil, chain); err != nil {
				return obs, err
			}
			if target := obs.Status.Height + params.FinalityDepth; tip.Height < target {
				n = target - tip.Height
			}
		case devnet.TxNotFound:
			n = max(1, params.BlockhashWindow/4)
		}
		if _, err := h.ProduceBlocks(ctx, chain, min(n, devnethost.MaxBlocksPerCall)); err != nil {
			return obs, err
		}
	}
}

// matchesChain is the reconciliation of one finalized transaction: the
// transaction the chain holds under the tx id is exactly the one that was
// signed, byte for byte, and it is finalized.
func matchesChain(ctx context.Context, h ChainHost, chain devnet.ChainID, txID devnet.TxID, signed []byte) (bool, error) {
	rec, err := h.GetTransaction(ctx, chain, txID)
	if err != nil {
		return false, err
	}
	raw, err := rec.Transaction.Encode()
	if err != nil {
		return false, nil
	}
	return rec.Status.State == devnet.TxFinalized && bytes.Equal(raw, signed), nil
}

func externalRef(chain devnet.ChainID, txID devnet.TxID) string {
	return "devnet:" + string(chain) + ":" + string(txID)
}
