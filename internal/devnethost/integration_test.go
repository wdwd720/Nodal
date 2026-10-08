//go:build integration

package devnethost

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/devnet"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "devnethost integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "devnethost-itest", MaxConns: 20}); err != nil {
		fmt.Fprintln(os.Stderr, "devnethost integration: open:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
}

// pgConfig is the unit-test configuration on a network of its own.
func pgConfig(t *testing.T) Config {
	cfg := testConfig(config.EnvTest)
	cfg.Network = "itest-" + uuid.NewString()[:8]
	return cfg
}

// pgWorld is a world whose host persists to devnet_events.
func pgWorld(t *testing.T) (*world, Config) {
	requireEnv(t)
	cfg := pgConfig(t)
	w := &world{t: t, ctx: context.Background(), payer: testKey("payer"), issuer: testKey("issuer"), usd: testAddr("usd")}
	w.h = openHost(t, NewPgStore(testDB), cfg)
	require.NoError(t, w.h.Airdrop(w.ctx, ChainSolana, w.payer.PublicKey(), 1_000_000))
	require.NoError(t, w.h.EnsureMint(w.ctx, ChainSolana, devnet.MintConfig{Address: w.usd, Decimals: 6, Authority: w.issuer.PublicKey()}))
	return w, cfg
}

func countEvents(t *testing.T, network string, kind EventKind) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = $2`, network, kind).Scan(&n))
	return n
}

// TestIntegration_ReplayFromTheDatabaseReproducesTheStateHash: a host opened
// on the persisted history -- a process restart -- rebuilds the network with
// the same StateHash and the same statuses, faults and reorgs included.
func TestIntegration_ReplayFromTheDatabaseReproducesTheStateHash(t *testing.T) {
	w, cfg := pgWorld(t)
	alice := testKey("alice").PublicKey()
	minted := w.submit(ChainSolana, w.signedMint(alice, 250))
	w.produce(ChainSolana, 1)
	require.NoError(t, w.h.TimeoutButLand(w.ctx, ChainSolana))
	landed, err := w.h.Submit(w.ctx, ChainSolana, w.signedMemo(ChainSolana, "unknown but landed"))
	require.Error(t, err)
	require.NoError(t, w.h.DropNext(w.ctx, ChainSolana, 1))
	dropped := w.submit(ChainSolana, w.signedMemo(ChainSolana, "dropped"))
	w.produce(ChainSolana, 2)
	_, err = w.h.Reorg(w.ctx, ChainSolana, 1, devnet.ReorgRequeue)
	require.NoError(t, err)
	w.produce(ChainSolana, 1+testDepth)

	restarted := openHost(t, NewPgStore(testDB), cfg)
	assert.Equal(t, w.stateHash(), mustHash(t, restarted))
	for _, id := range []devnet.TxID{minted, landed, dropped} {
		a := w.status(ChainSolana, id)
		b, err := restarted.Status(w.ctx, ChainSolana, id)
		require.NoError(t, err)
		assert.Equal(t, a, b)
	}
	assert.Equal(t, devnet.TxFinalized, w.status(ChainSolana, minted).State)
	assert.Equal(t, devnet.TxNotFound, w.status(ChainSolana, dropped).State)

	// The submission rows carry what the goal asks for: raw bytes, chain,
	// tx id and submission time.
	var raw []byte
	var chain string
	var at time.Time
	require.NoError(t, testDB.QueryRow(w.ctx, `SELECT raw_tx, chain_id, occurred_at FROM devnet_events
		WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`, cfg.Network, string(minted)).Scan(&raw, &chain, &at))
	assert.NotEmpty(t, raw)
	assert.Equal(t, string(ChainSolana), chain)
	assert.False(t, at.IsZero())
}

// TestIntegration_TheEventLogIsAppendOnly: the application role cannot
// change or delete history, even bypassing Go.
func TestIntegration_TheEventLogIsAppendOnly(t *testing.T) {
	w, cfg := pgWorld(t)
	w.submit(ChainSolana, w.signedMemo(ChainSolana, "x"))
	_, err := testDB.Exec(w.ctx, `UPDATE devnet_events SET args = '{}' WHERE network = $1 AND seq = 2`, cfg.Network)
	require.Error(t, err)
	_, err = testDB.Exec(w.ctx, `DELETE FROM devnet_events WHERE network = $1`, cfg.Network)
	require.Error(t, err)
	assert.Equal(t, 4, len(mustEvents(t, cfg.Network)), "genesis, airdrop, mint, submit survive")
}

func mustEvents(t *testing.T, network string) []Event {
	t.Helper()
	evs, err := NewPgStore(testDB).Since(context.Background(), nil, network, 0)
	require.NoError(t, err)
	return evs
}

// TestIntegration_TheDatabaseRefusesAGapInTheHistory: a row that is not the
// next one of its network is refused by the database (DH001), and GENESIS
// can only be the first row.
func TestIntegration_TheDatabaseRefusesAGapInTheHistory(t *testing.T) {
	w, cfg := pgWorld(t)
	evs := mustEvents(t, cfg.Network)
	last := evs[len(evs)-1]
	insert := func(seq uint64, kind EventKind, chain any) error {
		_, err := testDB.Exec(w.ctx, `INSERT INTO devnet_events (id, network, seq, kind, chain_id, args, outcome, state_hash, prev_hash, event_hash, occurred_at)
			VALUES ($1,$2,$3::numeric,$4,$5,'{}','APPLIED',$6,$6,$6,now())`,
			uuid.New(), cfg.Network, strconv.FormatUint(seq, 10), string(kind), chain, last.StateHash[:])
		return err
	}
	err := insert(last.Seq+2, EventClearFaults, string(ChainSolana))
	require.Error(t, err)
	assert.Equal(t, "DH001", db.SQLState(err))
	err = insert(last.Seq+1, EventGenesis, nil)
	require.Error(t, err, "GENESIS is only ever the first event")
	err = insert(last.Seq, EventClearFaults, string(ChainSolana))
	require.Error(t, err, "a taken position")

	// A submission's outcome is ACCEPTED or TIMEOUT, and only a submission
	// has one of those (devnet_events_submit_outcome).
	withOutcome := func(kind EventKind, outcome string) error {
		_, err := testDB.Exec(w.ctx, `INSERT INTO devnet_events (id, network, seq, kind, chain_id, args, outcome, state_hash, prev_hash, event_hash, occurred_at)
			VALUES ($1,$2,$3::numeric,$4,$5,'{}',$6,$7,$7,$7,now())`,
			uuid.New(), cfg.Network, strconv.FormatUint(last.Seq+1, 10), string(kind), string(ChainSolana), outcome, last.StateHash[:])
		return err
	}
	err = withOutcome(EventSubmit, "APPLIED")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "devnet_events_submit_outcome", "%v", err)
	err = withOutcome(EventClearFaults, "ACCEPTED")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "devnet_events_submit_outcome", "%v", err)
}

// TestIntegration_AForgedEventIsRefusedOnReplay: a row inserted around Go at
// the right position, even with a valid hash chain, does not survive a
// restart unless replaying it reproduces the state it claims.
func TestIntegration_AForgedEventIsRefusedOnReplay(t *testing.T) {
	w, cfg := pgWorld(t)
	evs := mustEvents(t, cfg.Network)
	last := evs[len(evs)-1]
	forged := Event{
		Network: cfg.Network, Seq: last.Seq + 1, Kind: EventAirdrop, Chain: ChainSolana,
		Args:      mustArgs(t, airdropArgs{To: testKey("mallory").PublicKey().String(), Lamports: "1000000000"}),
		Outcome:   OutcomeApplied,
		StateHash: last.StateHash, // the lie: an airdrop that changes nothing
		PrevHash:  last.EventHash, OccurredAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	sum, err := forged.computeHash()
	require.NoError(t, err)
	forged.EventHash = sum
	tx, err := testDB.Pool().Begin(w.ctx)
	require.NoError(t, err)
	require.NoError(t, insertEvent(w.ctx, tx, forged))
	require.NoError(t, tx.Commit(w.ctx))

	_, err = Open(w.ctx, NewPgStore(testDB), cfg, testClock())
	require.Error(t, err)
	assert.Equal(t, ReasonReplayDiverged, ReasonOf(err))

	// The running host refuses to act on it too: its next call catches up,
	// fails to reproduce the forged row, and fails closed.
	_, err = w.h.Status(w.ctx, ChainSolana, "x")
	require.Error(t, err)
	assert.Equal(t, ReasonReplayDiverged, ReasonOf(err))
}

func mustArgs(t *testing.T, v any) []byte {
	t.Helper()
	b, err := canonicalArgs(v)
	require.NoError(t, err)
	return b
}

// TestIntegration_AReadInsideATransactionNeedsNoSecondConnection: a caller
// holding a transaction reads the chain through ViewIn on that transaction,
// so even a pool of one connection -- held by the caller -- cannot deadlock
// the read (F-27). The pool-based View, by contrast, would have to wait.
func TestIntegration_AReadInsideATransactionNeedsNoSecondConnection(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	one, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "devnethost-one-conn", MaxConns: 1})
	require.NoError(t, err)
	defer one.Close()
	cfg := pgConfig(t)
	h := openHost(t, NewPgStore(one), cfg)
	tx, err := one.Pool().Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	require.NoError(t, h.ViewIn(ctx, tx, func(v View) error {
		_, err := v.Tip(ChainSolana)
		return err
	}), "the read runs on the caller's own connection")

	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	err = h.View(short, func(v View) error { return nil })
	require.Error(t, err, "a pooled read needs the connection the transaction holds")
}

// TestIntegration_TwoHostsShareOneChainHistory: two processes on one database
// append to one totally ordered history and observe each other's work.
func TestIntegration_TwoHostsShareOneChainHistory(t *testing.T) {
	w, cfg := pgWorld(t)
	other := openHost(t, NewPgStore(testDB), cfg)
	id := w.submit(ChainSolana, w.signedMint(testKey("bob").PublicKey(), 9))
	_, err := other.ProduceBlocks(w.ctx, ChainSolana, 1+testDepth)
	require.NoError(t, err)
	assert.Equal(t, devnet.TxFinalized, w.status(ChainSolana, id).State)
	assert.Equal(t, w.stateHash(), mustHash(t, other))
	assert.Equal(t, w.h.Applied(), other.Applied())
}

// TestIntegration_ConcurrentSubmissionsAreJournaledExactlyOnce: goroutines on
// two hosts submit concurrently; every acceptance is exactly one SUBMIT row,
// the history is gapless, and a restart replays to the same state.
func TestIntegration_ConcurrentSubmissionsAreJournaledExactlyOnce(t *testing.T) {
	requireEnv(t)
	cfg := pgConfig(t)
	for i := range cfg.Chains {
		cfg.Chains[i].BlockhashWindow = 200
	}
	w := &world{t: t, ctx: context.Background(), payer: testKey("payer"), issuer: testKey("issuer"), usd: testAddr("usd")}
	w.h = openHost(t, NewPgStore(testDB), cfg)
	require.NoError(t, w.h.Airdrop(w.ctx, ChainSolana, w.payer.PublicKey(), 10_000_000))
	other := openHost(t, NewPgStore(testDB), cfg)
	hosts := []*Host{w.h, other}

	const goroutines, each = 6, 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := map[devnet.TxID]bool{}
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := hosts[g%2]
			for j := 0; j < each; j++ {
				bh, err := h.LatestBlockhash(w.ctx, ChainSolana)
				if !assert.NoError(t, err) {
					return
				}
				tx, err := devnet.Sign(devnet.Message{ChainID: ChainSolana, RecentBlockhash: bh, FeePayer: w.payer.PublicKey(),
					Instructions: []devnet.Instruction{devnet.Memo{Data: []byte(fmt.Sprintf("g%d-%d", g, j))}}}, w.payer)
				if !assert.NoError(t, err) {
					return
				}
				raw, err := tx.Encode()
				if !assert.NoError(t, err) {
					return
				}
				id, err := h.Submit(w.ctx, ChainSolana, raw)
				if !assert.NoError(t, err) {
					return
				}
				// A duplicate of the same bytes is an acknowledgement.
				again, err := hosts[(g+1)%2].Submit(w.ctx, ChainSolana, raw)
				assert.NoError(t, err)
				assert.Equal(t, id, again)
				mu.Lock()
				accepted[id] = true
				mu.Unlock()
				_, err = h.ProduceBlocks(w.ctx, ChainSolana, 1)
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()
	assert.Len(t, accepted, goroutines*each)
	assert.Equal(t, goroutines*each, countEvents(t, cfg.Network, EventSubmit), "one row per acceptance, none per acknowledgement")

	var gaps int
	require.NoError(t, testDB.QueryRow(w.ctx, `SELECT count(*) FROM (
		SELECT seq, lag(seq) OVER (ORDER BY seq) AS prev FROM devnet_events WHERE network = $1) s
		WHERE prev IS NOT NULL AND seq <> prev + 1`, cfg.Network).Scan(&gaps))
	assert.Zero(t, gaps)
	restarted := openHost(t, NewPgStore(testDB), cfg)
	assert.Equal(t, w.stateHash(), mustHash(t, restarted))
}
