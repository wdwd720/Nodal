package chaintest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/nodal/controlplane/internal/chain"
)

// MemArchive is an in-memory chain.RawArchive. Set Err to make Store fail
// (adapters must then refuse to produce an observation).
type MemArchive struct {
	mu      sync.Mutex
	objects map[string]chain.RawObject
	order   []string
	Err     error
}

// NewMemArchive returns an empty archive.
func NewMemArchive() *MemArchive { return &MemArchive{objects: map[string]chain.RawObject{}} }

// Store implements chain.RawArchive. The reference embeds the body hash so
// tests can assert the archived bytes match what was parsed.
func (a *MemArchive) Store(_ context.Context, obj chain.RawObject) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Err != nil {
		return "", a.Err
	}
	sum := sha256.Sum256(obj.Body)
	ref := fmt.Sprintf("mem://%s/%s/%s-%d", obj.Provider, obj.EventType, hex.EncodeToString(sum[:8]), len(a.order))
	a.objects[ref] = obj
	a.order = append(a.order, ref)
	return ref, nil
}

// Get returns an archived object.
func (a *MemArchive) Get(ref string) (chain.RawObject, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	o, ok := a.objects[ref]
	return o, ok
}

// Len returns the number of stored objects.
func (a *MemArchive) Len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.order)
}

// Refs returns every reference in store order.
func (a *MemArchive) Refs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.order...)
}

// Collect returns a sink that records events and a getter for the recorded
// slice (copy). Set stopAfter > 0 to make the sink return ErrStop after that
// many events.
func Collect(stopAfter int) (sink func(chain.WalletEvent) error, events func() []chain.WalletEvent) {
	var mu sync.Mutex
	var got []chain.WalletEvent
	sink = func(ev chain.WalletEvent) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
		if stopAfter > 0 && len(got) >= stopAfter {
			return ErrStop
		}
		return nil
	}
	events = func() []chain.WalletEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]chain.WalletEvent(nil), got...)
	}
	return sink, events
}

// ErrStop is returned by a Collect sink once it has seen enough events.
var ErrStop = fmt.Errorf("chaintest: sink requested stop")
