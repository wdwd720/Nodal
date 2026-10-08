package devnethost

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

func TestConfig_ValidateRefusesEveryMalformedShape(t *testing.T) {
	require.NoError(t, testConfig(config.EnvTest).Validate())
	cases := map[string]func(*Config){
		"empty network":        func(c *Config) { c.Network = "" },
		"upper-case network":   func(c *Config) { c.Network = "Unit" },
		"leading dash network": func(c *Config) { c.Network = "-unit" },
		"long network":         func(c *Config) { c.Network = string(make([]byte, 64)) },
		"unknown environment":  func(c *Config) { c.Environment = "SANDBOX" },
		"one chain only":       func(c *Config) { c.Chains = c.Chains[:1] },
		"unknown chain":        func(c *Config) { c.Chains[0].ID = "sbx-bitcoin" },
		"duplicate chain":      func(c *Config) { c.Chains[1] = c.Chains[0] },
		"zero finality depth":  func(c *Config) { c.Chains[0].FinalityDepth = 0 },
		"zero window":          func(c *Config) { c.Chains[1].BlockhashWindow = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(config.EnvTest)
			mutate(&cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// TestHost_ReplayReproducesTheStateHash: a history that exercised every kind
// of event -- submissions accepted, dropped and timed out, block production,
// delays, both reorg modes, operator calls -- replays into a fresh host with
// the same StateHash, tip and transaction statuses.
func TestHost_ReplayReproducesTheStateHash(t *testing.T) {
	w := newWorld(t, config.EnvTest)
	alice := testKey("alice")
	var ids []devnet.TxID

	ids = append(ids, w.submit(ChainSolana, w.signedMint(alice.PublicKey(), 500)))
	w.produce(ChainSolana, 1)
	require.NoError(t, w.h.DropNext(w.ctx, ChainSolana, 1))
	ids = append(ids, w.submit(ChainSolana, w.signedMemo(ChainSolana, "dropped")))
	require.NoError(t, w.h.TimeoutButLand(w.ctx, ChainSolana))
	landed, err := w.h.Submit(w.ctx, ChainSolana, w.signedMemo(ChainSolana, "timeout-land"))
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	ids = append(ids, landed)
	require.NoError(t, w.h.TimeoutAndDrop(w.ctx, ChainSolana))
	lost, err := w.h.Submit(w.ctx, ChainSolana, w.signedMemo(ChainSolana, "timeout-drop"))
	require.Equal(t, devnet.CodeSubmissionTimeout, devnet.ReasonOf(err))
	ids = append(ids, lost)
	w.produce(ChainSolana, 1)
	require.NoError(t, w.h.DelayInclusion(w.ctx, ChainSolana, 2))
	ids = append(ids, w.submit(ChainSolana, w.signedMemo(ChainSolana, "delayed")))
	require.NoError(t, w.h.ClearFaults(w.ctx, ChainSolana))
	w.produce(ChainSolana, 3)
	_, err = w.h.Reorg(w.ctx, ChainSolana, 1, devnet.ReorgRequeue)
	require.NoError(t, err)
	ids = append(ids, w.submit(ChainEthereum, w.signedMemoEth("eth")))
	w.produce(ChainEthereum, 1)
	_, err = w.h.Reorg(w.ctx, ChainEthereum, 1, devnet.ReorgDrop)
	require.NoError(t, err)
	w.produce(ChainSolana, 2)
	require.NoError(t, w.h.SetMintFrozen(w.ctx, ChainSolana, w.usd, true))

	want := w.stateHash()
	replayed := openHost(t, w.store, testConfig(config.EnvTest))
	got, err := replayed.StateHash(w.ctx)
	require.NoError(t, err)
	assert.Equal(t, want, got, "replay reproduces the StateHash")
	assert.Equal(t, w.h.Applied(), replayed.Applied())
	for _, chain := range Chains() {
		a, err := w.h.Tip(w.ctx, chain)
		require.NoError(t, err)
		b, err := replayed.Tip(w.ctx, chain)
		require.NoError(t, err)
		assert.Equal(t, a, b, "same tip on %s", chain)
	}
	for _, id := range ids {
		for _, chain := range Chains() {
			a, err := w.h.Status(w.ctx, chain, id)
			require.NoError(t, err)
			b, err := replayed.Status(w.ctx, chain, id)
			require.NoError(t, err)
			assert.Equal(t, a, b, "same status for %s on %s", id, chain)
		}
	}
	bal, err := replayed.TokenBalance(w.ctx, ChainSolana, alice.PublicKey(), w.usd)
	require.NoError(t, err)
	assert.Equal(t, uint64(500), bal)
}

func (w *world) signedMemoEth(note string) []byte {
	w.t.Helper()
	require.NoError(w.t, w.h.EnsureNativeBalance(w.ctx, ChainEthereum, w.payer.PublicKey(), 1_000, 1_000_000))
	return w.signedMemo(ChainEthereum, note)
}

func TestHost_RefusedCallsAndAcknowledgementsAreNotJournaled(t *testing.T) {
	w := newWorld(t, config.EnvTest)
	before := len(w.store.all(testNetwork))

	_, err := w.h.Submit(w.ctx, ChainSolana, []byte("garbage"))
	require.Error(t, err)
	_, err = w.h.Submit(w.ctx, ChainEthereum, w.signedMemo(ChainSolana, "wrong chain"))
	assert.Equal(t, devnet.CodeWrongChain, devnet.ReasonOf(err))
	_, err = w.h.Submit(w.ctx, "sbx-bitcoin", w.signedMemo(ChainSolana, "no such chain"))
	require.Error(t, err)
	require.Error(t, w.h.EnsureMint(w.ctx, ChainSolana, devnet.MintConfig{Address: w.usd, Decimals: 9, Authority: w.issuer.PublicKey()}),
		"the same mint with other decimals is a conflict")
	require.NoError(t, w.h.EnsureMint(w.ctx, ChainSolana, devnet.MintConfig{Address: w.usd, Decimals: 6, Authority: w.issuer.PublicKey()}),
		"an identical mint is success")
	require.NoError(t, w.h.EnsureNativeBalance(w.ctx, ChainSolana, w.payer.PublicKey(), 1, 1), "already funded")
	_, err = w.h.Reorg(w.ctx, ChainSolana, 5, devnet.ReorgDrop)
	require.NoError(t, err, "a reorg of an empty chain removes nothing")
	require.NoError(t, w.h.DropNext(w.ctx, ChainSolana, 0))
	assert.Len(t, w.store.all(testNetwork), before, "nothing that changed nothing was journaled")

	raw := w.signedMemo(ChainSolana, "once")
	id := w.submit(ChainSolana, raw)
	afterFirst := w.store.count(testNetwork, EventSubmit)
	require.Equal(t, id, w.submit(ChainSolana, raw), "resubmission is acknowledged")
	assert.Equal(t, afterFirst, w.store.count(testNetwork, EventSubmit), "an acknowledgement is not an event")

	// A dropped transaction was never held, so sending it again is a new
	// acceptance and is journaled.
	require.NoError(t, w.h.DropNext(w.ctx, ChainSolana, 1))
	dropped := w.signedMemo(ChainSolana, "dropped")
	did := w.submit(ChainSolana, dropped)
	assert.Equal(t, devnet.TxNotFound, w.status(ChainSolana, did).State)
	require.Equal(t, did, w.submit(ChainSolana, dropped))
	assert.Equal(t, devnet.TxPending, w.status(ChainSolana, did).State)
	assert.Equal(t, afterFirst+2, w.store.count(testNetwork, EventSubmit))
}

func TestHost_FaultInjectionIsForbiddenOutsideLocalTestDev(t *testing.T) {
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		t.Run(string(env), func(t *testing.T) {
			w := newWorld(t, env)
			ctx := w.ctx
			faults := map[string]error{
				"DropNext":       w.h.DropNext(ctx, ChainSolana, 1),
				"TimeoutButLand": w.h.TimeoutButLand(ctx, ChainSolana),
				"TimeoutAndDrop": w.h.TimeoutAndDrop(ctx, ChainSolana),
				"DelayInclusion": w.h.DelayInclusion(ctx, ChainSolana, 1),
				"ClearFaults":    w.h.ClearFaults(ctx, ChainSolana),
				"SetMintFrozen":  w.h.SetMintFrozen(ctx, ChainSolana, w.usd, true),
			}
			_, faults["Reorg"] = w.h.Reorg(ctx, ChainSolana, 1, devnet.ReorgDrop)
			for name, err := range faults {
				require.Error(t, err, name)
				assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), name)
				assert.Equal(t, ReasonFaultInjectionForbidden, ReasonOf(err), name)
			}
			// The simulation itself runs everywhere.
			id := w.submit(ChainSolana, w.signedMemo(ChainSolana, "ok"))
			w.produce(ChainSolana, 1)
			assert.Equal(t, devnet.TxConfirmed, w.status(ChainSolana, id).State)
		})
	}
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		w := newWorld(t, env)
		require.NoError(t, w.h.DropNext(w.ctx, ChainSolana, 1), string(env))
	}
}

// TestHost_ALostCommitLeavesOnlyTheCommittedHistory: when the event store
// fails after the chain accepted a submission, the host reports the
// submission UNKNOWN with its tx id and rebuilds from what is committed, so
// it never runs a chain that differs from its durable history.
func TestHost_ALostCommitLeavesOnlyTheCommittedHistory(t *testing.T) {
	w := newWorld(t, config.EnvTest)
	before := w.stateHash()
	raw := w.signedMemo(ChainSolana, "lost")
	w.store.failAppend = 1
	id, err := w.h.Submit(w.ctx, ChainSolana, raw)
	require.Error(t, err)
	assert.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	assert.Equal(t, ReasonSubmissionPersistenceUnknown, ReasonOf(err))
	require.NotEmpty(t, id, "the tx id comes back with the unknown outcome")
	assert.Equal(t, devnet.TxNotFound, w.status(ChainSolana, id).State, "the uncommitted submission is gone")
	assert.Equal(t, before, w.stateHash())

	// A lost block production changes nothing either.
	tip, err := w.h.Tip(w.ctx, ChainSolana)
	require.NoError(t, err)
	w.store.failAppend = 1
	_, err = w.h.ProduceBlocks(w.ctx, ChainSolana, 2)
	require.Error(t, err)
	assert.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	again, err := w.h.Tip(w.ctx, ChainSolana)
	require.NoError(t, err)
	assert.Equal(t, tip, again)

	// The same bytes can be submitted once the store is back.
	require.Equal(t, id, w.submit(ChainSolana, raw))
	w.produce(ChainSolana, 1)
	assert.Equal(t, devnet.TxConfirmed, w.status(ChainSolana, id).State)

	// A store that cannot be read refuses reads rather than answering stale.
	w.store.failReads = true
	_, err = w.h.Status(w.ctx, ChainSolana, id)
	require.Error(t, err)
	w.store.failReads = false
	assert.Equal(t, devnet.TxConfirmed, w.status(ChainSolana, id).State)
}

// TestHost_TwoHostsOnOneHistoryConverge: two processes sharing a database see
// one chain: each catches up with the other's events before every call.
func TestHost_TwoHostsOnOneHistoryConverge(t *testing.T) {
	w := newWorld(t, config.EnvTest)
	other := openHost(t, w.store, testConfig(config.EnvTest))

	id := w.submit(ChainSolana, w.signedMemo(ChainSolana, "from a"))
	st, err := other.Status(w.ctx, ChainSolana, id)
	require.NoError(t, err)
	assert.Equal(t, devnet.TxPending, st.State, "b sees a's submission")
	_, err = other.ProduceBlocks(w.ctx, ChainSolana, 1+testDepth)
	require.NoError(t, err)
	assert.Equal(t, devnet.TxFinalized, w.status(ChainSolana, id).State, "a sees b's blocks")

	sa := w.stateHash()
	sb, err := other.StateHash(w.ctx)
	require.NoError(t, err)
	assert.Equal(t, sa, sb)
	assert.Equal(t, w.h.Applied(), other.Applied())
}

func TestHost_AHistoryFromAnotherConfigurationIsRefused(t *testing.T) {
	w := newWorld(t, config.EnvTest)
	cfg := testConfig(config.EnvTest)
	cfg.Chains[0].FinalityDepth++
	_, err := Open(w.ctx, w.store, cfg, testClock())
	require.Error(t, err)
	assert.Equal(t, ReasonConfigMismatch, ReasonOf(err))

	// The environment is not part of the history: a DEV host may open it.
	_, err = Open(w.ctx, w.store, testConfig(config.EnvDev), testClock())
	require.NoError(t, err)
}

// TestHost_ATamperedHistoryIsRefused: every way of altering a stored event
// -- its arguments, its recorded state, or a forgery that recomputes the
// hash chain but not the state -- stops the host from opening.
func TestHost_ATamperedHistoryIsRefused(t *testing.T) {
	w := newWorld(t, config.EnvTest)
	w.submit(ChainSolana, w.signedMint(testKey("bob").PublicKey(), 7))
	w.produce(ChainSolana, 1)
	events := w.store.all(testNetwork)
	original := events[1] // the payer's airdrop

	tamper := func(mutate func(*Event)) error {
		ev := cloneEvent(original)
		mutate(&ev)
		w.store.replace(testNetwork, ev)
		defer w.store.replace(testNetwork, original)
		_, err := Open(w.ctx, w.store, testConfig(config.EnvTest), testClock())
		return err
	}
	cases := map[string]func(*Event){
		"arguments changed": func(e *Event) {
			e.Args = json.RawMessage(`{"lamports":"999999999","to":"` + testKey("payer").PublicKey().String() + `"}`)
		},
		"state hash changed": func(e *Event) { e.StateHash[0] ^= 0xFF },
		"consistent forgery": func(e *Event) {
			e.Args = json.RawMessage(`{"lamports":"2000000","to":"` + testKey("payer").PublicKey().String() + `"}`)
			sum, err := e.computeHash()
			require.NoError(t, err)
			e.EventHash = sum
		},
	}
	for name, mutate := range cases {
		err := tamper(mutate)
		require.Error(t, err, name)
		assert.Equal(t, ReasonReplayDiverged, ReasonOf(err), name)
	}
	_, err := Open(w.ctx, w.store, testConfig(config.EnvTest), testClock())
	require.NoError(t, err, "the untampered history still opens")
}

// TestHost_ConcurrentCallersSeeOneOrder: many goroutines submitting and
// producing blocks through two hosts end with every transaction applied
// exactly once, and the log replays to the same state.
func TestHost_ConcurrentCallersSeeOneOrder(t *testing.T) {
	// A window wider than every block the goroutines produce together, so a
	// blockhash read before a submission is still valid when it arrives.
	wide := func(c *Config) {
		for i := range c.Chains {
			c.Chains[i].BlockhashWindow = 200
		}
	}
	w := newWorld(t, config.EnvTest, wide)
	cfg := testConfig(config.EnvTest)
	wide(&cfg)
	other := openHost(t, w.store, cfg)
	hosts := []*Host{w.h, other}
	owners := make([]devnet.PublicKey, 8)
	for i := range owners {
		owners[i] = testKey(fmt.Sprintf("owner-%d", i)).PublicKey()
	}
	var mu sync.Mutex
	var ids []devnet.TxID
	var wg sync.WaitGroup
	for i, owner := range owners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := hosts[i%2]
			for j := 0; j < 3; j++ {
				bh, err := h.LatestBlockhash(w.ctx, ChainSolana)
				if !assert.NoError(t, err) {
					return
				}
				raw := w.sign(devnet.Message{ChainID: ChainSolana, RecentBlockhash: bh, FeePayer: w.payer.PublicKey(),
					Instructions: []devnet.Instruction{
						devnet.TokenMintTo{Mint: w.usd, ToOwner: owner, Amount: 10, Authority: w.issuer.PublicKey()},
						devnet.Memo{Data: []byte(fmt.Sprintf("%d/%d", i, j))},
					}}, w.payer, w.issuer)
				id, err := h.Submit(w.ctx, ChainSolana, raw)
				if !assert.NoError(t, err) {
					return
				}
				mu.Lock()
				ids = append(ids, id)
				mu.Unlock()
				_, err = h.ProduceBlocks(w.ctx, ChainSolana, 1)
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()
	w.produce(ChainSolana, 1+testDepth)
	for _, id := range ids {
		assert.Equal(t, devnet.TxFinalized, w.status(ChainSolana, id).State)
	}
	for _, owner := range owners {
		bal, err := w.h.TokenBalance(w.ctx, ChainSolana, owner, w.usd)
		require.NoError(t, err)
		assert.Equal(t, uint64(30), bal, "each mint applied exactly once")
	}
	supply, err := other.Supply(w.ctx, ChainSolana, w.usd)
	require.NoError(t, err)
	assert.Equal(t, uint64(30*len(owners)), supply)
	replayed := openHost(t, w.store, cfg)
	a, b := w.stateHash(), mustHash(t, replayed)
	assert.Equal(t, a, b)
}

func mustHash(t *testing.T, h *Host) devnet.Hash {
	t.Helper()
	sh, err := h.StateHash(context.Background())
	require.NoError(t, err)
	return sh
}

func TestView_IsConsistentAndClosesWithItsCallback(t *testing.T) {
	w := newWorld(t, config.EnvTest)
	alice := testKey("alice").PublicKey()
	id := w.submit(ChainSolana, w.signedMint(alice, 42))
	w.produce(ChainSolana, 1)
	var kept View
	require.NoError(t, w.h.View(w.ctx, func(v View) error {
		st, err := v.Status(ChainSolana, id)
		require.NoError(t, err)
		assert.Equal(t, devnet.TxConfirmed, st.State)
		s, err := v.Supply(ChainSolana, w.usd)
		require.NoError(t, err)
		bal, err := v.TokenBalance(ChainSolana, alice, w.usd)
		require.NoError(t, err)
		assert.Equal(t, s, bal)
		kept = v
		return nil
	}))
	_, err := kept.Supply(ChainSolana, w.usd)
	require.Error(t, err)
	assert.Equal(t, ReasonViewClosed, ReasonOf(err))
}

func TestHost_BoundsArePerCall(t *testing.T) {
	w := newWorld(t, config.EnvTest)
	_, err := w.h.ProduceBlocks(w.ctx, ChainSolana, MaxBlocksPerCall+1)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(w.h.DropNext(w.ctx, ChainSolana, MaxFaultCount+1)))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(w.h.EnsureNativeBalance(w.ctx, ChainSolana, testAddr("x"), 10, 5)))
	_, err = w.h.Reorg(w.ctx, ChainSolana, 1, devnet.ReorgMode(9))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	tip, err := w.h.ProduceBlocks(w.ctx, ChainSolana, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), tip.Height, "zero blocks reads the tip")
}

func TestEventKindsAndOutcomesAreDeclaredOnce(t *testing.T) {
	seen := map[EventKind]bool{}
	for _, k := range AllEventKinds() {
		require.True(t, k.Valid())
		require.False(t, seen[k])
		seen[k] = true
	}
	assert.False(t, EventKind("NOPE").Valid())
	assert.Len(t, AllOutcomes(), 3)
	assert.Equal(t, []Outcome{OutcomeAccepted, OutcomeTimeout}, SubmitOutcomes())
}
