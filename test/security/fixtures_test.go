//go:build integration

package security

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// instrumentID returns the seeded SOL/USDC instrument, which is what makes a
// trade intent acceptable. The seeder is idempotent, so this is stable across
// runs against one database.
func instrumentID(t *testing.T, s session) string {
	t.Helper()
	r := getAs(t, s.Token, "/v1/instruments")
	require.Equal(t, http.StatusOK, r.Status, "GET /v1/instruments: %s", r.text())
	var list []struct {
		ID            string `json:"id"`
		CanonicalName string `json:"canonical_name"`
		Status        string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(r.Body, &list))
	for _, i := range list {
		if i.CanonicalName == "SOL/USDC" && i.Status == "ACTIVE" {
			return i.ID
		}
	}
	require.FailNow(t, "the seeded SOL/USDC instrument is missing; run `go run ./scripts/seed`")
	return ""
}

// intentBody is a well-formed, minimal PAPER intent for the given account.
func intentBody(accountID, instrument string) string {
	return fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"action":"ACQUIRE_NOTIONAL","mode":"PAPER","notional_usd":"10.00"}`,
		accountID, instrument)
}

// createIntent submits one intent and returns its id. POST /v1/intents is the
// suite's workhorse command: it is the only mutating operation a plain
// customer can drive to a 2xx in this deployment (a deposit needs a wallet the
// LOCAL stack does not provision), so it is what the idempotency and
// concurrency guards are measured on.
func createIntent(t *testing.T, s session, idemKey string) string {
	t.Helper()
	inst := instrumentID(t, s)
	acct := firstAccount(t, s)
	r := postAs(t, s.Token, "/v1/intents", idemKey, intentBody(acct, inst))
	require.Equal(t, http.StatusAccepted, r.Status, "POST /v1/intents: %s", r.text())
	var doc struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(r.Body, &doc))
	require.NotEmpty(t, doc.ID)
	return doc.ID
}

// intentCount counts the intents an account holds that were created by this
// run, identified by the idempotency key prefix the run owns.
func intentCount(t *testing.T, accountID, keyPrefix string) int {
	t.Helper()
	return countRows(t,
		`SELECT count(*) FROM trade_intents WHERE account_id = $1 AND idempotency_key LIKE $2`,
		accountID, keyPrefix+"%")
}

// idempotencyRows counts stored idempotency records for one actor and key.
func idempotencyRows(t *testing.T, actorID, idemKey string) int {
	t.Helper()
	return countRows(t, `SELECT count(*) FROM idempotency_keys WHERE actor_id = $1 AND key = $2`, actorID, idemKey)
}

// TestIDOR_CoversEveryAccountScopedRoute proves accountSubPaths is complete
// against the contract rather than against memory: every /accounts/{accountId}
// path in openapi/openapi.yaml must be probed by the cross-tenant tests, so a
// new account-scoped route cannot be added without a tenant probe.
func TestIDOR_CoversEveryAccountScopedRoute(t *testing.T) {
	root, err := moduleRoot()
	require.NoError(t, err)
	spec, err := os.ReadFile(filepath.Join(root, "openapi", "openapi.yaml"))
	require.NoError(t, err)

	// Top-level path keys are two-space indented in this document.
	re := regexp.MustCompile(`(?m)^  (/accounts/\{accountId\}[^:]*):`)
	var found []string
	for _, m := range re.FindAllStringSubmatch(string(spec), -1) {
		found = append(found, strings.TrimPrefix(m[1], "/accounts/{accountId}"))
	}
	require.NotEmpty(t, found, "no /accounts/{accountId} paths were found; the spec layout changed and this test is now vacuous")

	sort.Strings(found)
	probed := append([]string(nil), accountSubPaths...)
	sort.Strings(probed)
	require.Equal(t, found, probed,
		"accountSubPaths does not match the account-scoped routes in openapi/openapi.yaml")
}

// dbNow reads the database clock, so time-windowed assertions compare like
// with like rather than mixing the test host's clock with the server's.
func dbNow(t *testing.T) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var now time.Time
	require.NoError(t, testPool.QueryRow(ctx, `SELECT now()`).Scan(&now))
	return now.UTC()
}
