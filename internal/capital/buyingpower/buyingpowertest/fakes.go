// Package buyingpowertest provides in-memory fakes of the buying-power
// engine's reader interfaces. It is test-only and must never be imported by
// production code.
package buyingpowertest

import (
	"context"
	"sync"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/db"
)

// KillSwitches is a KillSwitchReader returning a fixed list.
type KillSwitches struct {
	mu     sync.Mutex
	States []buyingpower.KillSwitchState
	Err    error
}

// Active implements buyingpower.KillSwitchReader.
func (f *KillSwitches) Active(_ context.Context, _ db.Querier, _ accounts.AccountID) ([]buyingpower.KillSwitchState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	return append([]buyingpower.KillSwitchState(nil), f.States...), nil
}

// ReconciliationBlocks is a ReconciliationBlockReader returning a fixed list.
type ReconciliationBlocks struct {
	mu      sync.Mutex
	Records []buyingpower.ReconciliationBlock
	Err     error
}

// Blocks implements buyingpower.ReconciliationBlockReader.
func (f *ReconciliationBlocks) Blocks(_ context.Context, _ db.Querier, _ accounts.AccountID) ([]buyingpower.ReconciliationBlock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	return append([]buyingpower.ReconciliationBlock(nil), f.Records...), nil
}
