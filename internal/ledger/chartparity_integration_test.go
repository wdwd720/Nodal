//go:build integration

package ledger

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The chart of accounts, compared across the two languages that hold it.
//
// `internal/ledger`'s registry says, per account code, which side is normal and
// whether the balance may go below zero. LG001 — the negative-balance guard —
// reads the second of those from the ROW, and until migration 00717 the column
// was `NOT NULL DEFAULT false` with cp_app holding INSERT, so whoever created
// an account chose its own exemption from the guard (F-49).
//
// The fix duplicates the chart into SQL, which is precisely the maintainability
// risk this project's own findings register lists as deliberately unraised —
// and F-43 was that risk arriving in a security control, where a capability was
// high-risk in Go and not in SQL for a whole session. So the copy is compared
// rather than trusted: this drives both SQL functions for every code the Go
// registry declares.

func TestIntegration_GoAndSQLAgreeOnTheChartOfAccounts(t *testing.T) {
	requireEnv(t)
	codes := AllCodes()
	require.NotEmpty(t, codes, "the chart is empty; this test would compare nothing")

	var disagreements []string
	negatives := 0
	for _, c := range codes {
		var sqlNegative bool
		var sqlSide string
		require.NoError(t, testDB.QueryRow(t.Context(),
			`SELECT cp_ledger_code_allows_negative($1), cp_ledger_code_normal_side($1)`, string(c)).
			Scan(&sqlNegative, &sqlSide))

		if goNegative := c.AllowsNegative(); goNegative != sqlNegative {
			disagreements = append(disagreements,
				string(c)+" allow_negative: Go says "+boolWord(goNegative)+", SQL says "+boolWord(sqlNegative))
		}
		if goSide := string(c.NormalSide()); goSide != sqlSide {
			disagreements = append(disagreements,
				string(c)+" normal_side: Go says "+goSide+", SQL says "+sqlSide)
		}
		if c.AllowsNegative() {
			negatives++
		}
	}
	sort.Strings(disagreements)
	assert.Empty(t, disagreements,
		"%d disagreement(s) between internal/ledger's chart and the SQL copy that LG001 and the "+
			"ledger_accounts CHECK are built on:\n  %v", len(disagreements), disagreements)

	// Negative controls. If every code allowed a negative balance, or none did,
	// the comparison above would agree perfectly and prove nothing about a
	// guard whose whole job is to tell the two apart.
	assert.Positive(t, negatives, "no code allows a negative balance; the comparison is vacuous")
	assert.Less(t, negatives, len(codes), "every code allows a negative balance; LG001 would guard nothing")
}

// TestIntegration_AnAccountCannotExemptItselfFromTheNegativeBalanceGuard is
// F-49 from the outside: the row that would have carried the exemption.
func TestIntegration_AnAccountCannotExemptItselfFromTheNegativeBalanceGuard(t *testing.T) {
	requireEnv(t)
	asset := createAsset(t, "CHRT", 6, false)

	// WALLET does not allow a negative balance. An INSERT that says it does
	// must be refused by the database, whatever the caller believes.
	_, err := testDB.Exec(t.Context(),
		`INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side, allow_negative, status, created_at)
		 VALUES ($1,'PLATFORM',$3,'WALLET',$2,'DEBIT',true,'OPEN',now())`,
		NewLedgerAccountID(), asset, PlatformOwnerID)
	require.Error(t, err, "an account may not choose its own exemption from LG001")
	assert.Contains(t, err.Error(), "ledger_accounts_match_chart")

	// And the normal side is not a caller's choice either.
	_, err = testDB.Exec(t.Context(),
		`INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side, allow_negative, status, created_at)
		 VALUES ($1,'PLATFORM',$3,'WALLET',$2,'CREDIT',false,'OPEN',now())`,
		NewLedgerAccountID(), asset, PlatformOwnerID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ledger_accounts_match_chart")

	// The honest row is accepted, so the constraint refuses forgeries and not
	// the application.
	_, err = testDB.Exec(t.Context(),
		`INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side, allow_negative, status, created_at)
		 VALUES ($1,'PLATFORM',$3,'WALLET',$2,'DEBIT',false,'OPEN',now())`,
		NewLedgerAccountID(), asset, PlatformOwnerID)
	assert.NoError(t, err)
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
