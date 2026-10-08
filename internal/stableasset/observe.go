package stableasset

import (
	"context"
	"strconv"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
)

// ChainObservation is what the simulated chain says about one submitted
// transaction, read in one consistent View.
type ChainObservation struct {
	Chain           devnet.ChainID  `json:"chain"`
	TxID            devnet.TxID     `json:"tx_id"`
	State           string          `json:"state"`
	Status          devnet.TxStatus `json:"-"`
	BlockhashValid  bool            `json:"blockhash_valid"`
	BlockhashHeight uint64          `json:"blockhash_height"`
	FinalizedHeight uint64          `json:"finalized_height"`
	// Expired: the chain does not hold the transaction and its blockhash lies
	// a full window below the finalized height, so it can never land again --
	// not even after a reorg, which never reaches below finality. (A blockhash
	// that is merely outside the window could re-enter it if a reorg lowered
	// the tip.)
	Expired bool `json:"expired"`
}

// Finalized: the transaction succeeded in a finalized block. Permanent.
func (o ChainObservation) Finalized() bool { return o.Status.State == devnet.TxFinalized }

// FailedFinal: the transaction failed in a finalized block. Permanent.
func (o ChainObservation) FailedFinal() bool {
	return o.Status.State == devnet.TxFailed && o.Status.Final
}

// Final reports that the outcome can no longer change.
func (o ChainObservation) Final() bool { return o.Finalized() || o.FailedFinal() || o.Expired }

// Landed: the transaction's effects are in the chain state now (CONFIRMED or
// FINALIZED), even if a reorg could still remove them.
func (o ChainObservation) Landed() bool {
	return o.Status.State == devnet.TxConfirmed || o.Status.State == devnet.TxFinalized
}

// observeIn reads a transaction's status, its blockhash's validity and the
// finalized height in one View.
func observeIn(v ChainView, chain devnet.ChainID, txID devnet.TxID, blockhash devnet.Hash, bhHeight, window uint64) (ChainObservation, error) {
	st, err := v.Status(chain, txID)
	if err != nil {
		return ChainObservation{}, err
	}
	valid, err := v.IsBlockhashValid(chain, blockhash)
	if err != nil {
		return ChainObservation{}, err
	}
	finalized, err := v.FinalizedHeight(chain)
	if err != nil {
		return ChainObservation{}, err
	}
	return ChainObservation{
		Chain: chain, TxID: txID, State: st.State.String(), Status: st, BlockhashValid: valid,
		BlockhashHeight: bhHeight, FinalizedHeight: finalized,
		Expired: st.State == devnet.TxNotFound && finalized >= bhHeight+window,
	}, nil
}

func (s *Service) window(chain devnet.ChainID) (devnethost.ChainParams, error) {
	p, ok := s.provider.ChainParams(chain)
	if !ok {
		return devnethost.ChainParams{}, errs.New(errs.CodeValidationFailed, "stableasset: the provider does not run chain "+string(chain))
	}
	return p, nil
}

// observe reads a transaction's outcome at one instant; inside a transaction
// pass it as q so the read needs no second connection.
func (s *Service) observe(ctx context.Context, q db.Querier, chain devnet.ChainID, txID devnet.TxID, blockhash devnet.Hash, bhHeight uint64) (ChainObservation, error) {
	params, err := s.window(chain)
	if err != nil {
		return ChainObservation{}, err
	}
	var obs ChainObservation
	err = s.provider.ViewIn(ctx, q, func(v ChainView) error {
		var err error
		obs, err = observeIn(v, chain, txID, blockhash, bhHeight, params.BlockhashWindow)
		return err
	})
	return obs, err
}

// awaitFinal observes a transaction and, when produce is set (SANDBOX mode,
// where the platform is the block producer), produces blocks until the
// outcome is final: enough blocks to finalize a landed transaction, one
// block for a pending one, and larger steps while the chain does not hold it
// at all, until it has expired beyond any reorg. It never resubmits
// anything. With produce unset (DEVNET mode) it observes once. It runs
// outside any database transaction.
func (s *Service) awaitFinal(ctx context.Context, chain devnet.ChainID, txID devnet.TxID, blockhash devnet.Hash, bhHeight uint64, produce bool) (ChainObservation, error) {
	params, err := s.window(chain)
	if err != nil {
		return ChainObservation{}, err
	}
	budget := params.BlockhashWindow + params.FinalityDepth + 2
	for round := uint64(0); ; round++ {
		obs, err := s.observe(ctx, nil, chain, txID, blockhash, bhHeight)
		if err != nil || obs.Final() || !produce || round >= budget {
			return obs, err
		}
		n := uint64(1)
		switch obs.Status.State {
		case devnet.TxConfirmed, devnet.TxFailed:
			tip, err := s.provider.Tip(ctx, chain)
			if err != nil {
				return obs, err
			}
			if target := obs.Status.Height + params.FinalityDepth; tip.Height < target {
				n = target - tip.Height
			}
		case devnet.TxNotFound:
			n = max(1, params.BlockhashWindow/4)
		}
		if _, err := s.provider.ProduceBlocks(ctx, chain, min(n, devnethost.MaxBlocksPerCall)); err != nil {
			return obs, err
		}
	}
}

// chainTip reads chain's tip inside a transaction: the blockhash a plan
// references, and whether the host can serve the chain at all.
func (s *Service) chainTip(ctx context.Context, q db.Querier, chain devnet.ChainID) (devnet.BlockInfo, error) {
	var tip devnet.BlockInfo
	err := s.provider.ViewIn(ctx, q, func(v ChainView) error {
		var err error
		tip, err = v.Tip(chain)
		return err
	})
	return tip, err
}

func parseHeight(s string) uint64 {
	h, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return h
}

// submission is how a submit attempt ended.
type submission int

const (
	// submitted: the chain accepted the transaction.
	submitted submission = iota
	// submitUnknown: the outcome is unknown; only reconciliation by tx id
	// resolves it.
	submitUnknown
	// submitRefused: the chain refused the transaction outright; it can
	// never land.
	submitRefused
)

// definiteRefusals are devnet rejections that prove the transaction was not
// accepted and, being stateless or expired, never will be.
var definiteRefusals = map[devnet.ErrorCode]bool{
	devnet.CodeMalformedTransaction: true, devnet.CodeInvalidMessage: true, devnet.CodeInvalidChainID: true,
	devnet.CodeInvalidBase58: true, devnet.CodeWrongChain: true, devnet.CodeBlockhashNotFound: true,
	devnet.CodeSignatureMissing: true, devnet.CodeSignatureExtra: true, devnet.CodeSignatureInvalid: true,
}

// submit hands signed bytes to the chain and classifies the result. Anything
// that is not a definite acceptance or a definite refusal is unknown: the
// caller records it as such and never retries it.
func (s *Service) submit(ctx context.Context, chain devnet.ChainID, raw []byte) (submission, error) {
	_, err := s.provider.Submit(ctx, chain, raw)
	switch {
	case err == nil:
		return submitted, nil
	case definiteRefusals[devnet.ReasonOf(err)]:
		return submitRefused, err
	default:
		return submitUnknown, err
	}
}

// submissionUnknown is the error a caller gets when a submission's outcome
// is unknown.
func submissionUnknown(txID devnet.TxID, cause error) error {
	e := errs.New(errs.CodeSubmissionStateUnknown,
		"stableasset: the submission's outcome is unknown; it will be reconciled by transaction id, never resubmitted").
		WithField("tx_id", string(txID))
	if cause != nil {
		e = e.WithField("cause", cause.Error())
	}
	return e
}
