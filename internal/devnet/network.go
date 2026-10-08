package devnet

import (
	"crypto/sha256"
	"slices"
	"sync"

	"github.com/nodal/controlplane/internal/errs"
)

// Network is a registry of named chains. Chains are independent: each has
// its own state, blocks and faults, and the bridge connects them only through
// relayed messages (BuildRelease). It is safe for concurrent use.
type Network struct {
	mu     sync.Mutex
	chains map[ChainID]*Chain
}

// NewNetwork returns an empty network.
func NewNetwork() *Network {
	return &Network{chains: map[ChainID]*Chain{}}
}

// AddChain creates a chain from cfg and registers it. A second chain with
// the same id is refused with CONFLICT / CHAIN_EXISTS.
func (n *Network) AddChain(cfg ChainConfig) (*Chain, error) {
	c, err := NewChain(cfg)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, exists := n.chains[cfg.ID]; exists {
		return nil, newError(errs.CodeConflict, CodeChainExists, "chain "+string(cfg.ID)+" already exists")
	}
	n.chains[cfg.ID] = c
	return c, nil
}

// Chain returns the chain named id, or NOT_FOUND / CHAIN_NOT_FOUND.
func (n *Network) Chain(id ChainID) (*Chain, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	c, ok := n.chains[id]
	if !ok {
		return nil, newError(errs.CodeNotFound, CodeChainNotFound, "chain "+string(id)+" not found")
	}
	return c, nil
}

// ChainIDs returns the registered chain ids in ascending order.
func (n *Network) ChainIDs() []ChainID {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sortedIDsLocked()
}

// StateHash commits to every chain's StateHash in chain-id order.
func (n *Network) StateHash() Hash {
	n.mu.Lock()
	defer n.mu.Unlock()
	w := &writer{}
	w.str(networkDomain)
	ids := n.sortedIDsLocked()
	w.length(len(ids))
	for _, id := range ids {
		w.label(string(id))
		w.hash(n.chains[id].StateHash())
	}
	return Hash(sha256.Sum256(w.buf))
}

func (n *Network) sortedIDsLocked() []ChainID {
	ids := make([]ChainID, 0, len(n.chains))
	for id := range n.chains {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
