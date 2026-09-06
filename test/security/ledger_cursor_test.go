//go:build integration

package security

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A pagination cursor is the one request value that is deliberately opaque,
// which makes it the one a reviewer is least likely to check. If the owner
// filter is derived from the cursor rather than from the authenticated
// principal, a customer who can construct a cursor can walk somebody else's
// journal — and the journal is the financial record.
//
// internal/ledger takes the owner as a separate bound parameter and the cursor
// only as a keyset position. This proves that end to end: a cursor is forged
// that points immediately before a journal transaction belonging to another
// account, and the row must not appear.

// ledgerCursor is the format internal/ledger issues: base64url of
// "v1|<RFC3339Nano posted_at>|<transaction id>", compared with
// (posted_at, id) > (cursor.at, cursor.id).
func ledgerCursor(at time.Time, txID string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte("v1|" + at.UTC().Format(time.RFC3339Nano) + "|" + txID))
}

// journalRow is one posted transaction and the customer account that owns it.
type journalRow struct {
	TxID     string
	PostedAt time.Time
	OwnerID  string
}

// aForeignJournalRow returns a posted journal transaction together with the
// customer account it belongs to. It reads through the migrate role, which is
// how the test knows the truth the API is supposed to be hiding.
func aForeignJournalRow(t *testing.T) journalRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var row journalRow
	err := testPool.QueryRow(ctx,
		`SELECT t.id::text, t.posted_at, a.owner_id::text
		   FROM journal_transactions t
		   JOIN journal_entries e ON e.transaction_id = t.id
		   JOIN ledger_accounts a ON a.id = e.ledger_account_id
		  WHERE a.owner_type = 'CUSTOMER'
		  ORDER BY t.posted_at, t.id
		  LIMIT 1`).Scan(&row.TxID, &row.PostedAt, &row.OwnerID)
	require.NoError(t, err,
		"no customer-owned journal transaction exists; run `go run ./scripts/seed` so this test has something to leak")
	return row
}

// TestLedger_AForgedCursorCannotCrossTenants forges a cursor positioned
// immediately before a journal transaction that belongs to customer-a and
// replays it as customer-b, on customer-b's own account.
//
// The negative control replays the same cursor against the account that DOES
// own the row. The row comes back — as it should, for its owner — and the
// "this transaction must not appear" assertion fires, which is what shows the
// assertion can see a leak rather than merely seeing an empty page.
func TestLedger_AForgedCursorCannotCrossTenants(t *testing.T) {
	requireAPI(t)
	a := mustLogin(t, "customer-a")
	b := mustLogin(t, "customer-b")
	acctA, acctB := firstAccount(t, a), firstAccount(t, b)
	require.NotEqual(t, acctA, acctB)

	target := aForeignJournalRow(t)
	require.Equal(t, acctA, target.OwnerID,
		"this test assumes the seeded journal posting belongs to customer-a; it belongs to %s", target.OwnerID)
	require.Zero(t, countRows(t,
		`SELECT count(*) FROM journal_entries e
		   JOIN ledger_accounts a ON a.id = e.ledger_account_id
		  WHERE e.transaction_id = $1 AND a.owner_type = 'CUSTOMER' AND a.owner_id = $2`,
		target.TxID, acctB),
		"customer-b has an entry in the target transaction, so its absence would prove nothing")

	// One microsecond before the row, which is the smallest step postgres
	// stores. The keyset comparison is strictly greater, so the target is the
	// very next row the query would return if the owner filter were derived
	// from the cursor.
	forged := ledgerCursor(target.PostedAt.Add(-time.Microsecond), target.TxID)

	probeSession, probeAccount := b, acctB
	if secBreak(t, "ledger_cursor_targets_the_owning_account") {
		probeSession, probeAccount = a, acctA
	}

	r := getAs(t, probeSession.Token, "/v1/accounts/"+probeAccount+"/ledger/transactions?cursor="+forged)
	require.Equal(t, http.StatusOK, r.Status,
		"the forged cursor was not accepted, so nothing was paged and this test is vacuous: %s", r.text())

	var page struct {
		Items []struct {
			ID      string `json:"id"`
			Entries []struct {
				Account struct {
					OwnerType string `json:"owner_type"`
					OwnerID   string `json:"owner_id"`
				} `json:"account"`
			} `json:"entries"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(r.Body, &page), "page did not decode: %s", r.text())

	require.NotContains(t, r.text(), target.TxID,
		"a forged cursor returned journal transaction %s, which belongs to account %s",
		target.TxID, target.OwnerID)
	for _, item := range page.Items {
		require.NotEqual(t, target.TxID, item.ID)
		for _, e := range item.Entries {
			if e.Account.OwnerType != "CUSTOMER" {
				continue
			}
			require.Equalf(t, probeAccount, e.Account.OwnerID,
				"transaction %s carries an entry for account %s, which the caller does not own",
				item.ID, e.Account.OwnerID)
		}
	}

	// A cursor that is nonsense must be refused rather than ignored: silently
	// dropping an unparseable cursor turns page 2 into page 1, which is how a
	// client ends up re-reading rows it has already acted on.
	for _, bad := range []string{"not-base64!!", base64.RawURLEncoding.EncodeToString([]byte("v9|x|y")), "", "%00"} {
		res := getAs(t, b.Token, "/v1/accounts/"+acctB+"/ledger/transactions?cursor="+bad)
		require.Less(t, res.Status, 500, "cursor %q produced %d: %s", bad, res.Status, res.text())
		if bad == "" {
			require.Equal(t, http.StatusOK, res.Status, "an empty cursor means the first page")
			continue
		}
		require.Equal(t, http.StatusBadRequest, res.Status,
			"an unparseable cursor %q was accepted: %s", bad, res.text())
	}
}

// TestPagination_ACursorNeverWidensTheOwnerFilter is the same property without
// any forging: customer-a pages its own intents, hands the real cursor it was
// issued to customer-b, and customer-b replays it on customer-b's account.
// Nothing of customer-a's may appear, and the answer must be indistinguishable
// from what customer-b's own cursor would have produced.
func TestPagination_ACursorNeverWidensTheOwnerFilter(t *testing.T) {
	requireAPI(t)
	a := mustLogin(t, "customer-a")
	b := mustLogin(t, "customer-b")
	acctA, acctB := firstAccount(t, a), firstAccount(t, b)

	// Two intents per account so a page of one leaves a cursor behind.
	for i := range 2 {
		createIntent(t, a, key(fmt.Sprintf("cursor-a-%d", i)))
		createIntent(t, b, key(fmt.Sprintf("cursor-b-%d", i)))
	}

	cursorA := firstPageCursor(t, a, acctA)
	require.NotEmpty(t, cursorA, "customer-a's own listing produced no cursor, so there is nothing to hand over")

	stolen := getAs(t, b.Token, "/v1/intents?account_id="+acctB+"&limit=1&cursor="+cursorA)
	require.Equal(t, http.StatusOK, stolen.Status, "replaying a foreign cursor: %s", stolen.text())

	var page struct {
		Items []struct {
			ID        string `json:"id"`
			AccountID string `json:"account_id"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(stolen.Body, &page))
	for _, item := range page.Items {
		require.Equalf(t, acctB, item.AccountID,
			"a cursor issued to customer-a returned intent %s on account %s", item.ID, item.AccountID)
	}
	// And the foreign cursor must not be usable to read customer-a's account
	// directly either, cursor or no cursor.
	requireForbidden(t,
		getAs(t, b.Token, "/v1/intents?account_id="+acctA+"&limit=1&cursor="+cursorA),
		"customer-b listing customer-a's intents with customer-a's cursor")
}

// firstPageCursor asks for a single-item page and returns the cursor the API
// issued for the next one.
func firstPageCursor(t *testing.T, s session, accountID string) string {
	t.Helper()
	r := getAs(t, s.Token, "/v1/intents?account_id="+accountID+"&limit=1")
	require.Equal(t, http.StatusOK, r.Status, "GET /v1/intents: %s", r.text())
	var page struct {
		NextCursor *string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(r.Body, &page))
	if page.NextCursor == nil {
		return ""
	}
	return *page.NextCursor
}
