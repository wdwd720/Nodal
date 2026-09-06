//go:build integration && e2e

package e2e

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// intentCommand is the body of POST /v1/intents.
//
// Why this command and not POST /v1/funding/deposits, which the brief
// suggested: at this build stage a deposit cannot complete. cmd/api wires the
// funding service, but the seeded account has no wallet, so the command is
// refused before it reaches the idempotency store:
//
//	POST /v1/funding/deposits →
//	400 VALIDATION_FAILED "the account has no active wallet to receive the deposit"
//
// A refusal is recorded as a conclusion by the idempotency layer, so the
// replay would be a replay of a 400 and the test would prove nothing about a
// created effect. POST /v1/intents does complete: it answers 202, writes a
// row to trade_intents, and that row is the effect this test counts.
// (POST /v1/quotes/preview answers 503 PROVIDER_UNAVAILABLE because no venue
// adapter is wired, which is correct behavior and also not a command.)
type intentCommand struct {
	AccountID    string `json:"account_id"`
	InstrumentID string `json:"instrument_id"`
	Action       string `json:"action"`
	NotionalUSD  string `json:"notional_usd"`
	Mode         string `json:"mode"`
}

type apiIntent struct {
	ID            string `json:"id"`
	AccountID     string `json:"account_id"`
	InstrumentID  string `json:"instrument_id"`
	Action        string `json:"action"`
	NotionalUSD   string `json:"notional_usd"`
	Mode          string `json:"mode"`
	Status        string `json:"status"`
	ActorType     string `json:"actor_type"`
	CorrelationID string `json:"correlation_id"`
}

// TestE2E_IdempotencyAcrossProcesses posts the same command twice with the
// same Idempotency-Key and proves exactly one effect exists — by counting
// rows in the database, not by believing the API's own answer — then proves
// the key cannot be reused with a different body.
func TestE2E_IdempotencyAcrossProcesses(t *testing.T) {
	requireEnv(t)
	srv := startAPI(t)
	srv.dumpLogs(t)
	c := newClient(t, srv)
	ctx := t.Context()

	s := c.signIn(ctx, "customer-a:mfa")
	account := seeded.CustomerAAccountID
	key := idemKey("idem")

	// A notional unique to this run makes the effect countable by CONTENT,
	// not only by key. trade_intents has UNIQUE (account_id,
	// idempotency_key), so a count by key can never exceed one whatever the
	// server does; a count by notional has no such index behind it and is
	// therefore the assertion that can actually fail.
	minor := uniqueNotionalMinor(ctx, t, account)
	cmd := intentCommand{
		AccountID:    account,
		InstrumentID: seeded.InstrumentID,
		Action:       "ACQUIRE_NOTIONAL",
		NotionalUSD:  usdString(minor),
		Mode:         "PAPER",
	}
	t.Logf("idempotency key %q, notional %s (%d minor units)", key, cmd.NotionalUSD, minor)

	require.Equalf(t, 0, countIntentsByNotional(ctx, t, account, minor),
		"precondition: no intent for this run's notional exists yet")

	// --- first execution ---------------------------------------------------
	first := c.postJSON(ctx, "/v1/intents", cmd, asSession(s), idempotency(key))
	require.Equalf(t, http.StatusAccepted, first.Status,
		"the first POST executes the command and answers 202: %s", first.Body)

	var created apiIntent
	first.decode(t, &created)
	assert.Equal(t, account, created.AccountID)
	assert.Equal(t, "RECEIVED", created.Status)
	assert.Equal(t, "USER", created.ActorType)
	assert.Equal(t, cmd.NotionalUSD, created.NotionalUSD)

	// The effect exists in the database, and it is the row the API named.
	requireIntentRow(ctx, t, created.ID, account, key, minor)
	require.Equal(t, 1, countIntentsByKey(ctx, t, account, key))
	require.Equal(t, 1, countIntentsByNotional(ctx, t, account, minor))

	// --- replay ------------------------------------------------------------
	// The negative control sends the identical command under a FRESH key, so
	// the server correctly creates a second intent. That is exactly the
	// two-effects condition the count below must catch; the response-shape
	// assertions are skipped under the break so the count is the only thing
	// that fires.
	replayKey, wantSecondStatus := key, http.StatusOK
	brokenTwoEffects := e2eBreak(t, "idempotency_two_effects")
	if brokenTwoEffects {
		replayKey, wantSecondStatus = key+"-broken", http.StatusAccepted
	}
	second := c.postJSON(ctx, "/v1/intents", cmd, asSession(s), idempotency(replayKey))
	require.Equalf(t, wantSecondStatus, second.Status,
		"a replay answers 200, not 202 (202 means it executed again): %s", second.Body)

	if !brokenTwoEffects {
		var replayed apiIntent
		second.decode(t, &replayed)
		assert.Equal(t, created.ID, replayed.ID, "a replay returns the recorded resource, not a new one")
		assert.Equal(t, created.CorrelationID, replayed.CorrelationID,
			"a replay reproduces the recorded response verbatim, correlation id included")
	}

	require.Equalf(t, 1, countIntentsByNotional(ctx, t, account, minor),
		"exactly one effect may exist for one idempotency key; the database is the witness, not the API")
	require.Equal(t, 1, countIntentsByKey(ctx, t, account, key))

	// --- reuse with a different body ---------------------------------------
	different := cmd
	different.NotionalUSD = usdString(minor + 1)
	reuse := c.postJSON(ctx, "/v1/intents", different, asSession(s), idempotency(key))
	require.Equalf(t, http.StatusConflict, reuse.Status,
		"reusing a key with a different body is a conflict: %s", reuse.Body)
	p := reuse.problem(t)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, p.Code)
	assert.NotEmpty(t, p.RequestID)

	// The refused reuse created nothing, under either notional.
	assert.Equal(t, 1, countIntentsByNotional(ctx, t, account, minor))
	assert.Equalf(t, 0, countIntentsByNotional(ctx, t, account, minor+1),
		"a refused reuse must not leave an effect behind")
}

// --- database witnesses -------------------------------------------------------

func countIntentsByKey(ctx context.Context, t *testing.T, accountID, key string) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM trade_intents WHERE account_id = $1 AND idempotency_key = $2`,
		accountID, key).Scan(&n))
	return n
}

func countIntentsByNotional(ctx context.Context, t *testing.T, accountID string, minor int64) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM trade_intents WHERE account_id = $1 AND notional_usd_minor = $2`,
		accountID, minor).Scan(&n))
	return n
}

// requireIntentRow proves the row the API named is the row the database holds,
// with the values the command carried.
func requireIntentRow(ctx context.Context, t *testing.T, intentID, accountID, key string, minor int64) {
	t.Helper()
	var (
		gotAccount, gotKey, gotAction, gotMode, gotStatus string
		gotMinor                                          int64
	)
	require.NoErrorf(t, testDB.Pool().QueryRow(ctx,
		`SELECT account_id::text, idempotency_key, action, mode, status, notional_usd_minor
		   FROM trade_intents WHERE id = $1`, intentID).
		Scan(&gotAccount, &gotKey, &gotAction, &gotMode, &gotStatus, &gotMinor),
		"the intent the API returned (%s) is not in the database", intentID)
	assert.Equal(t, accountID, gotAccount)
	assert.Equal(t, key, gotKey)
	assert.Equal(t, "ACQUIRE_NOTIONAL", gotAction)
	assert.Equal(t, "PAPER", gotMode)
	assert.Equal(t, "RECEIVED", gotStatus)
	assert.Equal(t, minor, gotMinor, "money is stored as exact minor units")
}

// uniqueNotionalMinor draws a USD amount, in minor units, that no intent on
// this account is already using, so the suite can run repeatedly against one
// database and still count effects by content.
func uniqueNotionalMinor(ctx context.Context, t *testing.T, accountID string) int64 {
	t.Helper()
	for attempt := 0; attempt < 32; attempt++ {
		n, err := rand.Int(rand.Reader, big.NewInt(99_000))
		require.NoError(t, err)
		minor := n.Int64() + 100 // $1.00 … $990.99
		// +1 is used by the reuse case, so both must be free.
		if countIntentsByNotional(ctx, t, accountID, minor) == 0 &&
			countIntentsByNotional(ctx, t, accountID, minor+1) == 0 {
			return minor
		}
	}
	t.Fatal("could not find an unused notional after 32 attempts; the database is unexpectedly full of intents")
	return 0
}

// usdString renders minor units as the two-fraction-digit decimal string the
// API requires. No float ever touches a money value.
func usdString(minor int64) string {
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}
