//go:build integration && chaos

package chaos

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// capitalOutbox forwards capital's narrow Emitter onto the real transactional
// outbox, the same adapter the settlement integration suite uses. Capital
// deliberately does not import internal/event (D-032), so the composition
// root supplies this.
type capitalOutbox struct {
	outbox *event.Outbox
	clk    clock.Clock
}

func (e capitalOutbox) Emit(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	if _, ok := event.Lookup(event.Topic(topic)); !ok {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	agg := ""
	if ev, ok := payload.(capital.ReservationEvent); ok {
		agg = ev.ReservationID
	}
	if agg == "" {
		return nil
	}
	return e.outbox.Enqueue(ctx, tx, topic, event.Envelope{
		ID: event.NewEventID().String(), Type: topic, SchemaVersion: event.Topic(topic).Version(),
		Source: "chaos", AggregateType: event.AggregateReservation, AggregateID: agg,
		OccurredAt: e.clk.Now(), Payload: body,
	})
}

// financialWorld is one account with a funded wallet, plus the real services.
type financialWorld struct {
	clk     *clock.Fake
	account accounts.AccountID
	usdc    assets.AssetID
	ledger  *ledger.Service
	capital *capital.Service
	outbox  *event.Outbox
	ctx     context.Context
}

func newFinancialWorld(t *testing.T, funded int64) *financialWorld {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	_, acct := newAccount(t)
	usdc := newAsset(t, "USDC", 6, true)
	outbox := event.NewOutbox(clk)
	led := ledger.NewService(clk, "chaos-itest")
	cap := capital.NewService(clk, capitalOutbox{outbox: outbox, clk: clk})

	// A SYSTEM principal: postings need an actor, and the ledger refuses an
	// agent one. No principal at all also resolves to SYSTEM, but naming it
	// makes the test's authority explicit.
	ctx := security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: "chaos-operator", ActorType: security.ActorSystem,
	})

	w := &financialWorld{clk: clk, account: acct, usdc: usdc, ledger: led, capital: cap, outbox: outbox, ctx: ctx}
	if funded > 0 {
		p, err := ledger.FundingSettledPosting(ledger.FundingInputs{
			AccountID: acct, DepositID: "chaos-fund-" + chaosToken(), AssetID: usdc,
			Quantity: money.QuantityFromInt64(funded), EffectiveAt: clk.Now(),
		})
		require.NoError(t, err)
		_, err = led.PostInTx(ctx, testDB, p)
		require.NoError(t, err)
	}
	return w
}

func (w *financialWorld) walletBalance(t *testing.T) money.Quantity {
	t.Helper()
	b, err := w.ledger.Balance(w.ctx, testDB, ledger.CustomerAccount(w.account, ledger.CodeWallet, w.usdc))
	require.NoError(t, err)
	return b
}

func (w *financialWorld) activeReservations(t *testing.T) []capital.Reservation {
	t.Helper()
	rs, err := w.capital.ListActive(w.ctx, testDB, w.account.String(), w.usdc)
	require.NoError(t, err)
	return rs
}

// requireNoDrift is the "no silent divergence" half of the invariant: the
// stored aggregates must still equal what the rows say they should be.
// Scoped to this world's account so the suite is re-runnable and does not
// inherit another test's mess.
func requireNoDrift(t *testing.T, w *financialWorld, when string) {
	t.Helper()
	cd, err := capital.VerifyReservationTotals(w.ctx, testDB)
	require.NoError(t, err)
	for _, d := range cd {
		assert.NotEqualf(t, w.account, d.AccountID,
			"%s: capital reservation totals drifted for this account: recorded %s, computed %s",
			when, d.Recorded, d.Computed)
	}
	ld, err := ledger.VerifyBalances(w.ctx, testDB)
	require.NoError(t, err)
	for _, d := range ld {
		assert.NotEqualf(t, w.account.String(), d.Account.OwnerID,
			"%s: ledger balance drifted for this account: stored %s, computed %s",
			when, d.StoredBalance, d.ComputedBalance)
	}
}

// TestChaos_DatabaseDiesMidTransaction asserts the invariant "no duplicate
// financial effect and no partial one" when the database vanishes in the
// middle of a money-moving transaction.
//
// The fault is a real one, not a simulated error: an admin connection calls
// pg_terminate_backend on the exact backend running the transaction, after
// every write in it has been issued but before COMMIT. That is precisely what
// a Postgres failover, a connection reaper or an operator restart does to an
// in-flight transaction, and it is sharper than pausing the container because
// it hits one backend and leaves the shared stack alone.
//
// What must hold afterwards:
//   - nothing committed: no reservation, no posting, no outbox row;
//   - the aggregates do not drift;
//   - retrying under the SAME idempotency key produces exactly ONE effect,
//     not two, and not zero.
func TestChaos_DatabaseDiesMidTransaction(t *testing.T) {
	requireEnv(t)
	requireHealthyStack(t, PostgresContainer())

	const funded = 10_000_000 // 10 USDC in base units
	const reserve = 4_000_000
	w := newFinancialWorld(t, funded)
	startBalance := w.walletBalance(t)
	require.Equal(t, "10000000", startBalance.String(), "precondition: the wallet is funded")
	require.Empty(t, w.activeReservations(t), "precondition: no reservations yet")

	idemKey := "chaos-reserve-" + chaosToken()
	req := capital.ReserveRequest{
		AccountID: w.account.String(), AssetID: w.usdc,
		Quantity: money.QuantityFromInt64(reserve), USDMinor: 400,
		ActorType: security.ActorUser, ActorID: "chaos-user",
		IdempotencyKey: idemKey, TTL: time.Hour, Reason: "chaos",
	}

	// The posting that must land in the SAME transaction as the reservation.
	// A reservation without its posting, or a posting without its reservation,
	// is the partial financial effect the whole design exists to prevent.
	// A real settled-deposit posting, not a synthetic one: its idempotency
	// key is derived from the deposit id, so a replay is absorbed by the
	// ledger's own content hash rather than by anything this test invents.
	depositID := "chaos-dep-" + idemKey
	posting := func() ledger.Posting {
		p, err := ledger.FundingSettledPosting(ledger.FundingInputs{
			AccountID: w.account, DepositID: depositID, AssetID: w.usdc,
			Quantity: money.QuantityFromInt64(1), EffectiveAt: w.clk.Now(),
		})
		require.NoError(t, err)
		return p
	}
	postingKey := posting().IdempotencyKey

	separateTransactions := chaosBreak(t, "db_partial_write")

	// --- the fault -----------------------------------------------------------
	// One attempt, killed mid-transaction. db.InTx retries serialization
	// failures, not a terminated connection, so this closure runs once.
	var killed int64
	attemptErr := func() error {
		if separateTransactions {
			// NEGATIVE CONTROL: the reservation commits in its own
			// transaction, then the backend dies before the posting. The
			// account is left holding a reservation that no posting backs.
			if err := testDB.InTx(w.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
				_, err := w.capital.Reserve(ctx, tx, req)
				return err
			}); err != nil {
				return err
			}
			return testDB.InTx(w.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
				var pid int
				if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					return err
				}
				n, err := terminateBackend(w.ctx, adminDSN, pid)
				if err != nil {
					return err
				}
				killed += n
				_, err = w.ledger.Post(ctx, tx, posting())
				return err
			})
		}
		return testDB.InTx(w.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var pid int
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			if _, err := w.capital.Reserve(ctx, tx, req); err != nil {
				return err
			}
			if _, err := w.ledger.Post(ctx, tx, posting()); err != nil {
				return err
			}
			// Every write is issued. Kill the backend now: the transaction
			// can no longer commit, and nothing it did may survive.
			n, err := terminateBackend(w.ctx, adminDSN, pid)
			if err != nil {
				return err
			}
			killed += n
			// One more statement so the client actually observes the death
			// rather than discovering it at COMMIT.
			_, err = tx.Exec(ctx, "SELECT 1")
			return err
		})
	}()

	t.Logf("chaos: faulted transaction returned: %v (backends killed: %d)", attemptErr, killed)
	require.Positive(t, killed, "no backend was terminated; the fault was never injected and the test proves nothing. "+
		"The transaction returned %v before reaching the injection point", attemptErr)
	require.Error(t, attemptErr, "the transaction must fail once its backend is terminated")

	waitDB(t, 60*time.Second)

	// --- the invariant -------------------------------------------------------
	assert.Empty(t, w.activeReservations(t),
		"a reservation survived a transaction that never committed: capital is held against nothing")
	assert.Equal(t, startBalance.String(), w.walletBalance(t).String(),
		"the wallet balance moved in a transaction that never committed")
	assert.Zero(t, countPostingsByKey(t, postingKey),
		"a ledger posting survived a transaction that never committed")
	assert.Zero(t, countOutboxForAccount(t, w.account.String()),
		"an outbox event survived a transaction that never committed: a consumer would act on a fact that never happened")
	requireNoDrift(t, w, "after the fault")

	// --- recovery: the retry must produce exactly one effect ------------------
	require.NoError(t, testDB.InTx(w.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := w.capital.Reserve(ctx, tx, req); err != nil {
			return err
		}
		_, err := w.ledger.Post(ctx, tx, posting())
		return err
	}), "the retry after the fault must succeed")

	// And a SECOND retry under the same key must be absorbed, not doubled:
	// the recovery path is the one most likely to double-spend.
	require.NoError(t, testDB.InTx(w.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := w.capital.Reserve(ctx, tx, req); err != nil {
			return err
		}
		_, err := w.ledger.Post(ctx, tx, posting())
		return err
	}), "a replay under the same idempotency key must be absorbed, not rejected")

	active := w.activeReservations(t)
	assert.Len(t, active, 1, "recovery produced %d reservations; exactly one is the only correct answer", len(active))
	if len(active) == 1 {
		assert.Equal(t, money.QuantityFromInt64(reserve).String(), active[0].Quantity.String())
	}
	assert.Equal(t, 1, countPostingsByKey(t, postingKey),
		"recovery produced a duplicate ledger posting")
	assert.Equal(t, 1, countOutboxForAccount(t, w.account.String()),
		"recovery produced %d capital events; the reservation happened exactly once so exactly one event must exist",
		countOutboxForAccount(t, w.account.String()))
	requireNoDrift(t, w, "after recovery")
}

// terminateBackend kills one specific backend by pid from an admin connection
// on another database, and returns how many were killed.
func terminateBackend(ctx context.Context, admin string, pid int) (int64, error) {
	conn, err := pgxConnect(ctx, admin)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close(ctx) }()
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_terminate_backend($1)", pid).Scan(&ok); err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	return 1, nil
}

func countPostingsByKey(t *testing.T, key string) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM journal_transactions WHERE idempotency_key = $1`, key).Scan(&n))
	return n
}

// countOutboxForAccount counts capital events whose payload names this
// account. Scoped to the account so the count means something on a database
// other tests have already written to.
func countOutboxForAccount(t *testing.T, accountID string) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events
		  WHERE topic LIKE 'capital.reservation.%' AND payload->>'account_id' = $1`, accountID).Scan(&n))
	return n
}
