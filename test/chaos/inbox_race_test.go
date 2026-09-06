//go:build integration && chaos

package chaos

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestChaos_DuplicateDeliveryUnderConcurrencyAndDatabaseFault is the
// "no duplicate financial effect" invariant for INBOUND events (PART 46).
//
// Delivery is at-least-once, so the same provider event arrives more than
// once, and in a fleet it arrives at several workers at the same time. The
// inbox is the only thing standing between that and a double credit. Two
// existing suites cover the serial duplicate and the concurrent duplicate on
// a healthy database; neither covers what happens when the database kills a
// worker mid-effect while its rivals are mid-flight. That is the interesting
// case, because a killed worker leaves an inbox row that either committed
// with its effect or did not commit at all — and if those two could ever
// disagree, the money moves twice.
//
// The effect deliberately has TWO parts, and the second is the one that
// matters. A settled-deposit posting is content-hashed by the ledger, so it
// is idempotent all by itself — the first version of this test used only that
// and PASSED WITH THE INBOX REMOVED, proving nothing about the inbox at all.
// The outbox event is the honest half: it carries a fresh event id every
// time, exactly like a position update, a notification or a provider call,
// and nothing but the inbox stops twelve of them being written. Both are
// asserted, because the ledger's independent idempotency is a real second
// line of defense worth pinning, but only the outbox count can fail when the
// inbox is bypassed.
//
// N workers process one message id concurrently. Partway through, the
// backends of some of them are terminated. Afterwards:
//   - the financial effect exists EXACTLY once;
//   - the survivors that saw Duplicate genuinely lost the race, they did not
//     skip because nothing had happened yet;
//   - at least one worker was actually killed, or the fault never happened
//     and the test proves nothing.
func TestChaos_DuplicateDeliveryUnderConcurrencyAndDatabaseFault(t *testing.T) {
	requireEnv(t)
	requireHealthyStack(t, PostgresContainer())

	const workers = 12
	w := newFinancialWorld(t, 0)
	inbox := event.NewInbox(w.clk)

	messageID := "provider-msg-" + chaosToken()
	depositID := "chaos-inbox-" + chaosToken()
	depositAggregate := depositID
	posting, err := ledger.FundingSettledPosting(ledger.FundingInputs{
		AccountID: w.account, DepositID: depositID, AssetID: w.usdc,
		Quantity: money.QuantityFromInt64(5_000_000), EffectiveAt: w.clk.Now(),
	})
	require.NoError(t, err)

	bypassInbox := chaosBreak(t, "inbox_bypassed")

	var (
		processed, duplicates, failures atomic.Int64
		killed                          atomic.Int64
		effectRuns                      atomic.Int64
		barrier                         = make(chan struct{})
		wg                              sync.WaitGroup
	)
	// The FIRST worker to reach the effect dies, whichever one that is. That
	// is the single most dangerous instant in the whole flow: it has decided
	// it is the first to see this message and has moved the money, but has
	// not committed. Targeting the winner rather than a fixed index makes the
	// fault deterministic — an earlier version killed workers by index and
	// simply missed, and the precondition below caught that.
	var killer atomic.Bool

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier
			err := testDB.InTx(w.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
				effect := func(ctx context.Context, tx pgx.Tx) error {
					effectRuns.Add(1)
					// The half of the effect that is NOT self-idempotent: a
					// fresh event id every time, so its row count is a direct
					// measure of how many times the effect really ran.
					if err := w.outbox.Enqueue(ctx, tx, event.TopicFundingDepositTransitioned.String(),
						envelope(t, w.clk, event.TopicFundingDepositTransitioned, depositAggregate, 1)); err != nil {
						return err
					}
					if killer.CompareAndSwap(false, true) {
						// This worker dies between deciding to act and
						// committing: the single most dangerous instant.
						var pid int
						if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
							return err
						}
						if n, err := terminateBackend(ctx, adminDSN, pid); err == nil {
							killed.Add(n)
						}
					}
					_, err := w.ledger.Post(ctx, tx, posting)
					return err
				}
				if bypassInbox {
					// NEGATIVE CONTROL: the effect runs with no inbox guard,
					// which is what a worker that "knows" it has not seen this
					// message before would do.
					return effect(ctx, tx)
				}
				outcome, err := inbox.Process(ctx, tx, "chaos-provider", messageID, 1, effect)
				if err != nil {
					return err
				}
				switch outcome {
				case event.Processed:
					processed.Add(1)
				case event.Duplicate:
					duplicates.Add(1)
				}
				return nil
			})
			if err != nil {
				failures.Add(1)
			}
		}()
	}
	close(barrier)
	wg.Wait()
	waitDB(t, 60*time.Second)

	t.Logf("chaos: processed=%d duplicates=%d failures=%d effect_runs=%d killed=%d",
		processed.Load(), duplicates.Load(), failures.Load(), effectRuns.Load(), killed.Load())

	// Preconditions: a race that never raced, or a fault that never fired,
	// proves nothing. Say so instead of passing.
	require.Positive(t, killed.Load(), "no worker was killed; the fault never fired and this test proves nothing")
	require.Positive(t, effectRuns.Load(), "no worker ever reached the effect; the race never happened")

	// THE INVARIANT.
	assert.Equal(t, 1, countPostingsByKey(t, posting.IdempotencyKey),
		"the deposit was credited more than once (or not at all) across %d concurrent deliveries with %d killed workers",
		workers, killed.Load())
	assert.Equal(t, "5000000", w.walletBalance(t).String(),
		"the wallet balance does not match exactly one credit")
	assert.Equal(t, 1, countOutboxForAggregate(t, depositAggregate),
		"the non-idempotent half of the effect ran more than once: %d events were written for a single delivery",
		countOutboxForAggregate(t, depositAggregate))
	requireNoDrift(t, w, "after the concurrent duplicate delivery")

	if !bypassInbox {
		// The inbox must record the message as handled exactly once, and the
		// workers that saw Duplicate must have genuinely lost a race rather
		// than skipped an effect that never ran.
		rec, found, err := inbox.Get(w.ctx, testDB, "chaos-provider", messageID)
		require.NoError(t, err)
		require.True(t, found, "the inbox recorded nothing for a message that was processed")
		assert.NotZero(t, rec.MessageID)
		assert.LessOrEqual(t, processed.Load(), int64(1),
			"%d workers each believed they were the first to process the message", processed.Load())
		assert.Positive(t, duplicates.Load(),
			"no worker observed a duplicate, so the deliveries serialized and the concurrency was never tested; "+
				"raise the worker count or remove the barrier")
	}
}

// countOutboxForAggregate counts outbox rows for one aggregate id. Unlike a
// ledger posting, an outbox event carries a fresh id per write, so its count
// is a direct measure of how many times an effect actually ran.
func countOutboxForAggregate(t *testing.T, aggregateID string) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, aggregateID).Scan(&n))
	return n
}
