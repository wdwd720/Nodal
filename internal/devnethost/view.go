package devnethost

import (
	"context"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

// View is a consistent read of the whole network: every read made through
// one View sees the same instant, because the host holds its lock for the
// duration of the callback and nothing can submit, produce or reorg in
// between. Reconciliation needs that: comparing a supply read before a block
// with a transaction status read after it would report a difference that
// never existed.
//
// A View is valid only inside the callback it was passed to. It must not call
// back into the Host (the host lock is held), and every method returns
// HOST_VIEW_CLOSED once the callback has returned.
type View struct {
	net    *devnet.Network
	closed *bool
}

// View runs fn with a consistent View of the network, after catching up with
// the committed history.
func (h *Host) View(ctx context.Context, fn func(v View) error) error { return h.ViewIn(ctx, nil, fn) }

// ViewIn is View for a caller inside a database transaction: the catch-up
// read runs on q (the caller's transaction), never on a second pooled
// connection, so callers holding transactions cannot exhaust the pool and
// wait on each other (F-27).
func (h *Host) ViewIn(ctx context.Context, q db.Querier, fn func(v View) error) error {
	return h.read(ctx, q, func(net *devnet.Network) error {
		closed := false
		defer func() { closed = true }()
		return fn(View{net: net, closed: &closed})
	})
}

// FinalizedHeight returns chain's highest finalized height. A transaction
// whose blockhash lies a full window below it can never land again: no reorg
// reaches below the finalized height.
func (v View) FinalizedHeight(chain devnet.ChainID) (uint64, error) {
	c, err := v.chain(chain)
	if err != nil {
		return 0, err
	}
	return c.FinalizedHeight(), nil
}

// NativeBalance returns owner's native balance.
func (v View) NativeBalance(chain devnet.ChainID, owner devnet.PublicKey) (uint64, error) {
	c, err := v.chain(chain)
	if err != nil {
		return 0, err
	}
	return c.Balance(owner), nil
}

func (v View) chain(id devnet.ChainID) (*devnet.Chain, error) {
	if v.closed == nil || *v.closed {
		return nil, errs.New(errs.CodeInternal, "devnethost: a View was used after its callback returned").
			WithField(reasonField, ReasonViewClosed)
	}
	return v.net.Chain(id)
}

// Status returns a transaction's status.
func (v View) Status(chain devnet.ChainID, id devnet.TxID) (devnet.TxStatus, error) {
	c, err := v.chain(chain)
	if err != nil {
		return devnet.TxStatus{}, err
	}
	return c.Status(id), nil
}

// Supply returns a mint's supply.
func (v View) Supply(chain devnet.ChainID, mint devnet.PublicKey) (uint64, error) {
	c, err := v.chain(chain)
	if err != nil {
		return 0, err
	}
	return c.Supply(mint)
}

// TokenBalance returns owner's balance of mint.
func (v View) TokenBalance(chain devnet.ChainID, owner, mint devnet.PublicKey) (uint64, error) {
	c, err := v.chain(chain)
	if err != nil {
		return 0, err
	}
	return c.TokenBalance(owner, mint), nil
}

// IsBlockhashValid reports whether bh is still within chain's window.
func (v View) IsBlockhashValid(chain devnet.ChainID, bh devnet.Hash) (bool, error) {
	c, err := v.chain(chain)
	if err != nil {
		return false, err
	}
	return c.IsBlockhashValid(bh), nil
}

// LockedTransfer returns the BRIDGE_LOCK of transaction id with its status.
func (v View) LockedTransfer(chain devnet.ChainID, id devnet.TxID) (devnet.LockedTransfer, error) {
	c, err := v.chain(chain)
	if err != nil {
		return devnet.LockedTransfer{}, err
	}
	return c.LockedTransfer(id)
}

// Tip returns chain's canonical tip.
func (v View) Tip(chain devnet.ChainID) (devnet.BlockInfo, error) {
	c, err := v.chain(chain)
	if err != nil {
		return devnet.BlockInfo{}, err
	}
	return c.Tip(), nil
}
