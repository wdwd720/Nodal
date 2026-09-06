package chaintest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider"
)

// Method names accepted by Fault; MethodAll applies to every method.
const (
	MethodAll                  = "*"
	MethodGetTransaction       = "GetTransaction"
	MethodGetSignatureStatuses = "GetSignatureStatuses"
	MethodGetBalances          = "GetBalances"
	MethodGetBlockHeight       = "GetBlockHeight"
	MethodGetLatestBlockhash   = "GetLatestBlockhash"
	MethodIsBlockhashValid     = "IsBlockhashValid"
	MethodSimulate             = "Simulate"
	MethodSearchWalletActivity = "SearchWalletActivity"
	MethodStreamWalletEvents   = "StreamWalletEvents"
)

// FaultKind selects an injected failure.
type FaultKind string

// Fault kinds.
const (
	// FaultTimeout blocks until ctx is done and returns PROVIDER_UNAVAILABLE.
	FaultTimeout FaultKind = "TIMEOUT"
	// FaultError returns an error with Fault.Code (default PROVIDER_UNAVAILABLE).
	FaultError FaultKind = "ERROR"
	// FaultWrongDeltas corrupts the token deltas / amounts of the answer
	// (the transaction is still found).
	FaultWrongDeltas FaultKind = "WRONG_DELTAS"
	// FaultNotFound answers "not found" regardless of the script or chain.
	FaultNotFound FaultKind = "NOT_FOUND"
	// FaultLatency delays the answer by Fault.Delay (bounded by ctx).
	FaultLatency FaultKind = "LATENCY"
)

// Fault is an injected failure. Remaining > 0 limits how many calls it
// affects; 0 means until cleared.
type Fault struct {
	Kind      FaultKind
	Code      errs.Code
	Delay     time.Duration
	Remaining int
}

// Fake is a scripted chain.SolanaDataProvider. Reads come from the bound
// Chain (see Chain.NewObserver) or from scripted values; faults apply first.
// It is safe for concurrent use.
type Fake struct {
	name string
	clk  clock.Clock

	mu        sync.Mutex
	chain     *Chain
	lag       uint64
	txs       map[string]chain.TxObservation
	statuses  map[string]chain.SignatureStatus
	balances  map[string][]chain.BalanceObservation
	height    uint64
	blockhash chain.Blockhash
	valid     map[string]bool
	sim       chain.SimulationResult
	simErr    error
	activity  map[string][]chain.TxObservation
	faults    map[string]*Fault
	calls     map[string]int
	health    provider.Health
	events    chan chain.WalletEvent
	streaming bool
}

var _ chain.SolanaDataProvider = (*Fake)(nil)

// NewFake returns a healthy, empty fake.
func NewFake(name string, clk clock.Clock) *Fake {
	if clk == nil {
		clk = clock.System()
	}
	return &Fake{
		name: name, clk: clk,
		txs: map[string]chain.TxObservation{}, statuses: map[string]chain.SignatureStatus{},
		balances: map[string][]chain.BalanceObservation{}, valid: map[string]bool{},
		activity: map[string][]chain.TxObservation{}, faults: map[string]*Fault{}, calls: map[string]int{},
		health: provider.Healthy, sim: chain.SimulationResult{OK: true},
		events: make(chan chain.WalletEvent, 256), streaming: true,
	}
}

// Name implements chain.ChainObserver.
func (f *Fake) Name() string { return f.name }

// Health implements chain.ChainObserver.
func (f *Fake) Health() provider.Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.health
}

// SetHealth scripts the reported health.
func (f *Fake) SetHealth(h provider.Health) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health = h
	return f
}

// SetLag changes how many slots behind the bound chain this observer reads.
func (f *Fake) SetLag(lag uint64) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lag = lag
	return f
}

// SetStreaming enables or disables STREAM hints from the bound chain.
func (f *Fake) SetStreaming(on bool) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streaming = on
	return f
}

// SetTransaction scripts an observation (Found is forced true).
func (f *Fake) SetTransaction(obs chain.TxObservation) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	obs.Found = true
	f.txs[obs.Signature] = obs
	return f
}

// RemoveTransaction forgets a scripted observation.
func (f *Fake) RemoveTransaction(sig string) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.txs, sig)
	return f
}

// SetStatus scripts a signature status.
func (f *Fake) SetStatus(st chain.SignatureStatus) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[st.Signature] = st
	return f
}

// SetBalances scripts the balances of owner.
func (f *Fake) SetBalances(owner string, bs ...chain.BalanceObservation) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balances[owner] = append([]chain.BalanceObservation(nil), bs...)
	return f
}

// SetBlockHeight scripts the block height (ignored when chain-bound).
func (f *Fake) SetBlockHeight(h uint64) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.height = h
	return f
}

// SetBlockhash scripts the latest blockhash and marks it valid.
func (f *Fake) SetBlockhash(b chain.Blockhash) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockhash = b
	f.valid[b.Blockhash] = true
	return f
}

// SetBlockhashValid scripts IsBlockhashValid for one hash.
func (f *Fake) SetBlockhashValid(h string, ok bool) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valid[h] = ok
	return f
}

// SetSimulation scripts Simulate.
func (f *Fake) SetSimulation(r chain.SimulationResult, err error) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sim, f.simErr = r, err
	return f
}

// SetActivity scripts SearchWalletActivity for wallet (newest first).
func (f *Fake) SetActivity(wallet string, txs ...chain.TxObservation) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activity[wallet] = append([]chain.TxObservation(nil), txs...)
	return f
}

// Fault installs a fault for method (or MethodAll).
func (f *Fake) Fault(method string, fault Fault) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	fc := fault
	f.faults[method] = &fc
	return f
}

// ClearFaults removes every fault.
func (f *Fake) ClearFaults() *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = map[string]*Fault{}
	return f
}

// Calls returns how many times method was invoked.
func (f *Fake) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// Emit pushes a stream event to StreamWalletEvents consumers.
func (f *Fake) Emit(ev chain.WalletEvent) {
	f.events <- ev
}

// hint is called by the bound chain when a transaction lands.
func (f *Fake) hint(sig string, slot uint64, index uint32) {
	f.mu.Lock()
	on := f.streaming
	f.mu.Unlock()
	if !on {
		return
	}
	seq := chain.SequenceOf(slot, index)
	select {
	case f.events <- chain.WalletEvent{Kind: chain.EventTransaction, Origin: chain.OriginStream, Observation: chain.TxObservation{Signature: sig, Slot: slot}, Sequence: &seq}:
	default:
	}
}

// begin counts the call and applies any fault. It returns the fault kind
// to apply after the base answer (WRONG_DELTAS / NOT_FOUND) or an error.
func (f *Fake) begin(ctx context.Context, method string) (FaultKind, error) {
	f.mu.Lock()
	f.calls[method]++
	fault := f.faults[method]
	if fault == nil {
		fault = f.faults[MethodAll]
	}
	var applied Fault
	if fault != nil {
		applied = *fault
		if fault.Remaining > 0 {
			fault.Remaining--
			if fault.Remaining == 0 {
				delete(f.faults, method)
				if f.faults[MethodAll] == fault {
					delete(f.faults, MethodAll)
				}
			}
		}
	}
	f.mu.Unlock()
	if fault == nil {
		return "", nil
	}
	switch applied.Kind {
	case FaultTimeout:
		<-ctx.Done()
		return "", errs.Wrap(ctx.Err(), errs.CodeProviderUnavailable, f.name+": timed out")
	case FaultError:
		code := applied.Code
		if code == "" {
			code = errs.CodeProviderUnavailable
		}
		return "", errs.New(code, f.name+": injected fault")
	case FaultLatency:
		t := time.NewTimer(applied.Delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return "", errs.Wrap(ctx.Err(), errs.CodeProviderUnavailable, f.name+": timed out")
		case <-t.C:
		}
		return "", nil
	}
	return applied.Kind, nil
}

func (f *Fake) stamp(obs chain.TxObservation) chain.TxObservation {
	now := f.clk.Now()
	obs.Source = f.name
	obs.ObservedAt, obs.ReceivedAt = now, now
	if obs.RawRef == "" {
		obs.RawRef = "fake://" + f.name + "/" + obs.Signature
	}
	return obs
}

// GetTransaction implements chain.ChainObserver.
func (f *Fake) GetTransaction(ctx context.Context, sig string) (chain.TxObservation, error) {
	kind, err := f.begin(ctx, MethodGetTransaction)
	if err != nil {
		return chain.TxObservation{}, err
	}
	if err := ctx.Err(); err != nil {
		return chain.TxObservation{}, errs.Wrap(err, errs.CodeProviderUnavailable, "context done")
	}
	obs, found := f.lookup(sig)
	if kind == FaultNotFound {
		found = false
	}
	if !found {
		return f.stamp(chain.TxObservation{Signature: sig}), nil
	}
	if kind == FaultWrongDeltas {
		obs = corruptDeltas(obs)
	}
	return f.stamp(obs), nil
}

func (f *Fake) lookup(sig string) (chain.TxObservation, bool) {
	f.mu.Lock()
	ch, lag := f.chain, f.lag
	scripted, ok := f.txs[sig]
	f.mu.Unlock()
	if ch != nil {
		if obs, found := ch.view(sig, lag, chain.CommitmentConfirmed); found {
			return obs, true
		}
		if !ok {
			return chain.TxObservation{}, false
		}
	}
	if !ok {
		return chain.TxObservation{}, false
	}
	if scripted.Commitment == "" {
		scripted.Commitment = chain.CommitmentConfirmed
	}
	return scripted, true
}

func corruptDeltas(obs chain.TxObservation) chain.TxObservation {
	one := money.QuantityFromInt64(1)
	ds := append([]chain.TokenDelta(nil), obs.TokenBalanceDeltas...)
	if len(ds) > 0 {
		ds[0].Post = ds[0].Post.Add(one)
	} else {
		ds = append(ds, chain.TokenDelta{Owner: "corrupt", Mint: "corrupt", TokenAccount: "corrupt", Pre: money.QuantityFromInt64(0), Post: one})
	}
	obs.TokenBalanceDeltas = ds
	return obs
}

// GetSignatureStatuses implements chain.ChainObserver.
func (f *Fake) GetSignatureStatuses(ctx context.Context, sigs []string) ([]chain.SignatureStatus, error) {
	kind, err := f.begin(ctx, MethodGetSignatureStatuses)
	if err != nil {
		return nil, err
	}
	out := make([]chain.SignatureStatus, 0, len(sigs))
	for _, s := range sigs {
		f.mu.Lock()
		ch, lag := f.chain, f.lag
		st, ok := f.statuses[s]
		f.mu.Unlock()
		if ch != nil {
			if cs, found := ch.status(s, lag); found {
				st, ok = cs, true
			}
		}
		if !ok {
			if obs, found := f.lookup(s); found {
				st = chain.SignatureStatus{Signature: s, Found: true, Slot: obs.Slot, Commitment: obs.Commitment, Err: obs.Err}
				ok = true
			}
		}
		if !ok || kind == FaultNotFound {
			st = chain.SignatureStatus{Signature: s}
		}
		out = append(out, st)
	}
	return out, nil
}

// GetBalances implements chain.ChainObserver.
func (f *Fake) GetBalances(ctx context.Context, owner string, mints []string) ([]chain.BalanceObservation, error) {
	kind, err := f.begin(ctx, MethodGetBalances)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	ch, lag := f.chain, f.lag
	scripted := append([]chain.BalanceObservation(nil), f.balances[owner]...)
	f.mu.Unlock()
	var out []chain.BalanceObservation
	if ch != nil {
		out = ch.balancesOf(owner, mints, lag)
	} else {
		want := map[string]bool{}
		for _, m := range mints {
			want[m] = true
		}
		for _, b := range scripted {
			if len(want) == 0 || want[b.Mint] {
				out = append(out, b)
			}
		}
	}
	now := f.clk.Now()
	for i := range out {
		out[i].Source = f.name
		out[i].ObservedAt, out[i].ReceivedAt = now, now
		if kind == FaultWrongDeltas && i == 0 {
			out[i].Amount = out[i].Amount.Add(money.QuantityFromInt64(1))
		}
	}
	chain.SortBalances(out)
	return out, nil
}

// GetBlockHeight implements chain.ChainObserver.
func (f *Fake) GetBlockHeight(ctx context.Context) (uint64, error) {
	if _, err := f.begin(ctx, MethodGetBlockHeight); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chain != nil {
		f.chain.mu.Lock()
		defer f.chain.mu.Unlock()
		return f.chain.heightAt(f.chain.effectiveSlot(f.lag)), nil
	}
	return f.height, nil
}

// GetLatestBlockhash implements chain.ChainObserver.
func (f *Fake) GetLatestBlockhash(ctx context.Context) (chain.Blockhash, error) {
	if _, err := f.begin(ctx, MethodGetLatestBlockhash); err != nil {
		return chain.Blockhash{}, err
	}
	f.mu.Lock()
	ch := f.chain
	b := f.blockhash
	f.mu.Unlock()
	if ch != nil {
		b = ch.IssueBlockhash()
	}
	now := f.clk.Now()
	b.Source = f.name
	b.ObservedAt, b.ReceivedAt = now, now
	return b, nil
}

// IsBlockhashValid implements chain.ChainObserver.
func (f *Fake) IsBlockhashValid(ctx context.Context, blockhash string) (bool, error) {
	if _, err := f.begin(ctx, MethodIsBlockhashValid); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chain != nil {
		f.chain.mu.Lock()
		defer f.chain.mu.Unlock()
		return f.chain.blockhashValidAt(blockhash, f.chain.effectiveSlot(f.lag)), nil
	}
	return f.valid[blockhash], nil
}

// Simulate implements chain.ChainObserver.
func (f *Fake) Simulate(ctx context.Context, _ []byte, _ chain.SimulateOptions) (chain.SimulationResult, error) {
	if _, err := f.begin(ctx, MethodSimulate); err != nil {
		return chain.SimulationResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.simErr != nil {
		return chain.SimulationResult{}, f.simErr
	}
	r := f.sim
	now := f.clk.Now()
	r.Source = f.name
	r.ObservedAt, r.ReceivedAt = now, now
	return r, nil
}

// SearchWalletActivity implements chain.ChainObserver.
func (f *Fake) SearchWalletActivity(ctx context.Context, wallet string, since time.Time, limit int) ([]chain.TxObservation, error) {
	kind, err := f.begin(ctx, MethodSearchWalletActivity)
	if err != nil {
		return nil, err
	}
	if kind == FaultNotFound {
		return nil, nil
	}
	f.mu.Lock()
	ch, lag := f.chain, f.lag
	scripted := append([]chain.TxObservation(nil), f.activity[wallet]...)
	f.mu.Unlock()
	var out []chain.TxObservation
	if ch != nil {
		out = ch.activity(wallet, since, limit, lag)
	} else {
		for _, tx := range scripted {
			if tx.BlockTime == nil || !tx.BlockTime.Before(since) {
				out = append(out, tx)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Slot > out[j].Slot })
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
	}
	for i := range out {
		out[i].Found = true
		if out[i].Commitment == "" {
			out[i].Commitment = chain.CommitmentConfirmed
		}
		out[i] = f.stamp(out[i])
	}
	return out, nil
}

// StreamWalletEvents implements chain.SolanaDataProvider: it announces a
// RECONNECT, then delivers emitted / chain-landed hints for the wallets as
// RPC observations (GetTransaction), until ctx is done or sink fails.
func (f *Fake) StreamWalletEvents(ctx context.Context, wallets []string, sink func(chain.WalletEvent) error) error {
	if _, err := f.begin(ctx, MethodStreamWalletEvents); err != nil {
		return err
	}
	want := map[string]bool{}
	for _, w := range wallets {
		want[w] = true
	}
	if err := sink(chain.WalletEvent{Kind: chain.EventReconnect, Source: f.name, ReceivedAt: f.clk.Now(), Detail: "fake stream connected"}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-f.events:
			ev.Source = f.name
			ev.ReceivedAt = f.clk.Now()
			if ev.Kind == "" {
				ev.Kind = chain.EventTransaction
			}
			if ev.Kind == chain.EventTransaction {
				obs, err := f.GetTransaction(ctx, ev.Observation.Signature)
				if err != nil {
					return err
				}
				if !obs.Found {
					continue
				}
				ev.Observation = obs
				matched := false
				for w := range want {
					if obs.Touches(w) {
						ev.Wallet = w
						matched = true
						break
					}
				}
				if !matched && ev.Wallet == "" {
					continue
				}
				if ev.Origin == "" {
					ev.Origin = chain.OriginStream
				}
			}
			if err := sink(ev); err != nil {
				return err
			}
		}
	}
}
