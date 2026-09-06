package ledger

import (
	"context"

	"github.com/nodal/controlplane/internal/db"
)

// VerifyBalances recomputes Σ entries (signed in normal-side terms) and the
// entry count for every ledger account and compares them with
// ledger_balances. It returns one Drift per disagreeing account, in owner /
// code / asset order; an empty result means the projection is intact.
//
// Balances are maintained by a trigger inside the posting transaction, so
// any drift means a write reached ledger_balances outside a posting (a
// privileged role, a restored backup, a bug) and is a SEV1
// ledger_integrity_violation. The single statement reads one snapshot, so
// concurrent postings cannot produce false positives.
func VerifyBalances(ctx context.Context, q db.Querier) ([]Drift, error) {
	rows, err := q.Query(ctx,
		`SELECT a.id, a.owner_type, a.owner_id::text, a.code, a.asset_id,
		        COALESCE(b.balance, 0)::text, COALESCE(b.entry_count, 0),
		        COALESCE(s.computed, 0)::text, COALESCE(s.cnt, 0)
		   FROM ledger_accounts a
		   LEFT JOIN ledger_balances b ON b.ledger_account_id = a.id
		   LEFT JOIN LATERAL (
		        SELECT sum(CASE WHEN (e.side = 'DEBIT') = (a.normal_side = 'DEBIT') THEN e.quantity ELSE -e.quantity END) AS computed,
		               count(*) AS cnt
		          FROM journal_entries e
		         WHERE e.ledger_account_id = a.id
		   ) s ON true
		  WHERE COALESCE(b.balance, 0) <> COALESCE(s.computed, 0)
		     OR COALESCE(b.entry_count, 0) <> COALESCE(s.cnt, 0)
		  ORDER BY a.owner_type, a.owner_id, a.code, a.asset_id`)
	if err != nil {
		return nil, MapError(err)
	}
	defer rows.Close()
	var out []Drift
	for rows.Next() {
		var d Drift
		var stored, computed string
		if err := rows.Scan(&d.LedgerAccountID, &d.Account.OwnerType, &d.Account.OwnerID, &d.Account.Code, &d.Account.AssetID,
			&stored, &d.StoredEntryCount, &computed, &d.ComputedEntryCount); err != nil {
			return nil, MapError(err)
		}
		if d.StoredBalance, err = scanQuantity(stored); err != nil {
			return nil, err
		}
		if d.ComputedBalance, err = scanQuantity(computed); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, MapError(err)
	}
	return out, nil
}
