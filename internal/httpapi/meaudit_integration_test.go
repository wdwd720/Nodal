//go:build integration

package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// The customer's own audit trail, against the real schema.
//
// The reader is one hand-written UNION over two append-only tables owned by
// other packages, `security_events` (keyed by user) and `audit_events` (keyed
// by the account stream). Both have been reshaped since they were written --
// security_events was repartitioned by 00740, audit_events grew a hash chain
// and checkpoints -- so the query is checked against the migrated schema rather
// than against a fake that would agree with whatever it was told.

func aUserWithAccount(t *testing.T, d *db.DB) (accounts.UserID, accounts.AccountID) {
	t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(t.Context(), d, "me-audit-itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	a, err := repo.CreateAccount(t.Context(), d, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return u.ID, a.ID
}

func TestIntegration_MeAudit_ReadsBothTrailsAndNobodyElses(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	mine, myAccount := aUserWithAccount(t, d)
	theirs, theirAccount := aUserWithAccount(t, d)

	base := time.Now().UTC().Add(-time.Hour)
	for i, u := range []accounts.UserID{mine, mine, theirs} {
		_, err := d.Exec(ctx,
			`INSERT INTO security_events (id, kind, severity, user_id, detail, ip, user_agent, occurred_at)
			 VALUES ($1::uuid, 'login', 'INFO', $2, '{}'::jsonb, '203.0.113.7'::inet, 'Firefox', $3)`,
			id.New[id.Any]().String(), u, base.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
	}

	writer := audit.NewWriter()
	for i, acct := range []accounts.AccountID{myAccount, myAccount, theirAccount} {
		at := base.Add(time.Duration(10+i) * time.Minute)
		stream := audit.AccountStream(acct.String())
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, aerr := writer.Append(ctx, tx, audit.Event{
				Stream:       stream,
				ActorType:    string(security.ActorOperator),
				ActorID:      "operator-who-must-not-be-named",
				Action:       "account.reviewed",
				ResourceType: "account",
				ResourceID:   acct.String(),
				Reason:       "a periodic review",
				OccurredAt:   at,
			})
			return aerr
		}))
	}

	reader := NewMeAuditPort(d)
	page, err := reader.Audit(ctx, mine, []string{myAccount.String()}, "", 50)
	require.NoError(t, err)
	require.Len(t, page.Items, 4, "two sign-ins and two account events, and nobody else's")

	var security, account int
	for _, it := range page.Items {
		switch it.Source {
		case "SECURITY":
			security++
			assert.Equal(t, "login", it.Action)
			assert.Equal(t, "203.0.113.7", it.IP)
		case "ACCOUNT":
			account++
			assert.Equal(t, "account.reviewed", it.Action)
			assert.Equal(t, myAccount.String(), it.ResourceID)
			assert.Equal(t, "OPERATOR", it.ActorType)
		default:
			t.Fatalf("unknown source %q", it.Source)
		}
		assert.NotContains(t, it.Action+it.ActorType+it.ResourceID, "operator-who-must-not-be-named",
			"the operator's identity is never rendered to a customer")
	}
	assert.Equal(t, 2, security)
	assert.Equal(t, 2, account)

	// Newest first, and the keyset pages without repeating or skipping a row.
	for i := 1; i < len(page.Items); i++ {
		assert.False(t, page.Items[i].OccurredAt.After(page.Items[i-1].OccurredAt), "newest first")
	}
	first, err := reader.Audit(ctx, mine, []string{myAccount.String()}, "", 3)
	require.NoError(t, err)
	require.Len(t, first.Items, 3)
	require.NotEmpty(t, first.NextCursor)
	rest, err := reader.Audit(ctx, mine, []string{myAccount.String()}, first.NextCursor, 3)
	require.NoError(t, err)
	require.Len(t, rest.Items, 1)
	assert.Empty(t, rest.NextCursor)
	seen := map[string]bool{}
	for _, it := range append(first.Items, rest.Items...) {
		assert.False(t, seen[it.ID], "the cursor returned a row twice")
		seen[it.ID] = true
	}

	// A principal with no accounts still gets their sign-in trail, and the
	// account half is simply absent rather than being everybody's.
	noAccounts, err := reader.Audit(ctx, mine, nil, "", 50)
	require.NoError(t, err)
	require.Len(t, noAccounts.Items, 2)
	for _, it := range noAccounts.Items {
		assert.Equal(t, "SECURITY", it.Source)
	}

	// And a forged cursor is refused rather than silently treated as page one.
	_, err = reader.Audit(ctx, mine, []string{myAccount.String()}, "not-a-cursor!!", 50)
	require.Error(t, err)
}
