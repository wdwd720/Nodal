//go:build integration && e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

const (
	// concurrentCallers is how many goroutines fire the identical command at
	// once. Sixteen is comfortably more than the pool's ability to serialize
	// them by accident.
	concurrentCallers = 16
	// contentionAttempts bounds how many bursts are run while looking for an
	// overlapping one. Every burst is held to the full correctness contract;
	// only the *precondition* (that some caller actually saw the command in
	// flight) is allowed more than one chance, because a scheduler that
	// happens to serialize a burst is not the same fact as a server that
	// serializes every burst.
	contentionAttempts = 3
)

// callResult is one goroutine's answer.
type callResult struct {
	worker   int
	status   int
	code     errs.Code
	intentID string
	body     []byte
	err      error
}

// TestE2E_ConcurrentIdenticalCommands fires the same command, with the same
// Idempotency-Key, from many goroutines at once. It asserts the postcondition
// — exactly one effect in the database, and every answer either the success,
// its replay, or a well-formed IDEMPOTENCY_IN_PROGRESS — and it asserts the
// PRECONDITION that the callers really overlapped. A concurrency test that
// never contends proves nothing, so zero in-progress answers is a failure,
// not a pass.
func TestE2E_ConcurrentIdenticalCommands(t *testing.T) {
	requireEnv(t)
	srv := startAPI(t)
	srv.dumpLogs(t)
	c := newClient(t, srv)
	ctx := t.Context()

	s := c.signIn(ctx, "customer-a:mfa")
	account := seeded.CustomerAAccountID

	serialized := e2eBreak(t, "concurrency_serialized")

	contended := 0
	burstsRun := 0
	for attempt := 1; attempt <= contentionAttempts; attempt++ {
		burstsRun = attempt
		key := idemKey(fmt.Sprintf("conc-%d", attempt))
		minor := uniqueNotionalMinor(ctx, t, account)
		cmd := intentCommand{
			AccountID:    account,
			InstrumentID: seeded.InstrumentID,
			Action:       "ACQUIRE_NOTIONAL",
			NotionalUSD:  usdString(minor),
			Mode:         "PAPER",
		}
		require.Equalf(t, 0, countIntentsByNotional(ctx, t, account, minor),
			"precondition: burst %d starts with no effect for its notional", attempt)

		results := fireBurst(ctx, t, c, s, cmd, key, serialized)
		inProgress := assertBurstIsCorrect(ctx, t, account, key, minor, results)
		contended += inProgress
		t.Logf("burst %d: %d callers, %d saw IDEMPOTENCY_IN_PROGRESS", attempt, len(results), inProgress)
		if inProgress > 0 {
			break
		}
	}

	require.Positivef(t, contended,
		"no caller in %d burst(s) of %d ever saw IDEMPOTENCY_IN_PROGRESS, so the callers never overlapped "+
			"and this test proved nothing about concurrency",
		burstsRun, concurrentCallers)
}

// fireBurst releases every caller from one barrier so they arrive together.
// Under the negative control they are run one after another instead, which is
// what a test that never actually contends looks like.
func fireBurst(
	ctx context.Context, t *testing.T, c *client, s session,
	cmd intentCommand, key string, serialized bool,
) []callResult {
	t.Helper()

	body, err := json.Marshal(cmd)
	require.NoError(t, err)

	post := func(worker int) callResult {
		resp, err := c.try(ctx, http.MethodPost, "/v1/intents", body,
			header("Content-Type", "application/json"), asSession(s), idempotency(key))
		if err != nil {
			return callResult{worker: worker, err: err}
		}
		r := callResult{worker: worker, status: resp.Status, body: resp.Body}
		if resp.Status >= 400 {
			var p errs.Problem
			if json.Unmarshal(resp.Body, &p) == nil {
				r.code = p.Code
			}
			return r
		}
		var intent apiIntent
		if json.Unmarshal(resp.Body, &intent) == nil {
			r.intentID = intent.ID
		}
		return r
	}

	results := make([]callResult, concurrentCallers)
	if serialized {
		for i := range results {
			results[i] = post(i)
		}
		return results
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = post(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return results
}

// assertBurstIsCorrect holds one burst to the whole contract and returns how
// many callers were told the command was already in flight.
func assertBurstIsCorrect(
	ctx context.Context, t *testing.T, account, key string, minor int64, results []callResult,
) int {
	t.Helper()

	var executed, replayed, inProgress int
	var executedID string
	for _, r := range results {
		require.NoErrorf(t, r.err, "caller %d failed at the transport", r.worker)
		require.Lessf(t, r.status, http.StatusInternalServerError,
			"caller %d got %d — a concurrent duplicate is a known, named outcome, never a server error. body: %s",
			r.worker, r.status, r.body)

		switch r.status {
		case http.StatusAccepted:
			executed++
			executedID = r.intentID
		case http.StatusOK:
			replayed++
		case http.StatusConflict:
			require.Equalf(t, errs.CodeIdempotencyInProgress, r.code,
				"caller %d got a 409 that is not IDEMPOTENCY_IN_PROGRESS: %s", r.worker, r.body)
			inProgress++
		default:
			t.Fatalf("caller %d got an unexpected status %d: %s", r.worker, r.status, r.body)
		}
	}

	require.Equalf(t, 1, executed,
		"exactly one caller may execute the command; %d did", executed)
	require.NotEmpty(t, executedID, "the executing caller returned no resource id")

	// Every replay must name the resource the single execution produced —
	// two callers each convinced they created something is the failure mode
	// this whole contract exists to prevent.
	for _, r := range results {
		if r.status == http.StatusOK {
			assert.Equalf(t, executedID, r.intentID,
				"caller %d replayed a different resource: %s", r.worker, r.body)
		}
	}

	// The database is the witness, not the API.
	require.Equalf(t, 1, countIntentsByKey(ctx, t, account, key),
		"more than one intent exists for one idempotency key")
	require.Equalf(t, 1, countIntentsByNotional(ctx, t, account, minor),
		"more than one effect exists for one command; %d executed / %d replayed / %d in progress",
		executed, replayed, inProgress)

	var dbID string
	require.NoError(t, testDB.Pool().QueryRow(ctx,
		`SELECT id::text FROM trade_intents WHERE account_id = $1 AND idempotency_key = $2`,
		account, key).Scan(&dbID))
	require.Equal(t, executedID, dbID, "the row in the database is not the one the API named")

	return inProgress
}
