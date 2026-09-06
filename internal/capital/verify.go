package capital

import (
	"context"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/money"
)

// Drift is one (account, asset) whose asset_reservation_totals.reserved
// disagrees with Σ(quantity − consumed_quantity) over ACTIVE reservations.
type Drift struct {
	AccountID accounts.AccountID
	AssetID   assets.AssetID
	Recorded  money.Quantity // totals.reserved
	Computed  money.Quantity // Σ active remaining
}

// Delta returns Recorded − Computed.
func (d Drift) Delta() money.Quantity { return d.Recorded.Sub(d.Computed) }

// VerifyReservationTotals recomputes the reserved quantity per (account,
// asset) from ACTIVE reservations and returns every pair whose totals row
// disagrees, including pairs with reservations but no totals row and vice
// versa. It reads one consistent snapshot; a periodic job raises a SEV1 on
// any drift. An empty result means the totals are exact.
func VerifyReservationTotals(ctx context.Context, q db.Querier) ([]Drift, error) {
	rows, err := q.Query(ctx, `
		WITH active AS (
			SELECT account_id, asset_id, sum(quantity - consumed_quantity) AS remaining
			FROM asset_reservations WHERE status = 'ACTIVE' GROUP BY account_id, asset_id
		)
		SELECT coalesce(t.account_id, a.account_id), coalesce(t.asset_id, a.asset_id),
		       coalesce(t.reserved, 0)::text, coalesce(a.remaining, 0)::text
		FROM asset_reservation_totals t
		FULL OUTER JOIN active a ON a.account_id = t.account_id AND a.asset_id = t.asset_id
		WHERE coalesce(t.reserved, 0) <> coalesce(a.remaining, 0)
		ORDER BY 1, 2`)
	if err != nil {
		return nil, dbErr("verify reservation totals", err)
	}
	defer rows.Close()
	out := []Drift{}
	for rows.Next() {
		var d Drift
		if err := rows.Scan(&d.AccountID, &d.AssetID, &d.Recorded, &d.Computed); err != nil {
			return nil, dbErr("scan drift", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("iterate drift", err)
	}
	return out, nil
}

// EnvelopeDrift is one envelope whose budget flow is inconsistent: either
// available + reserved + deployed ≠ allocation, or reserved ≠ Σ(usd −
// consumed_usd) over its ACTIVE reservations.
type EnvelopeDrift struct {
	EnvelopeID     EnvelopeID
	Allocation     money.USD
	Available      money.USD
	Reserved       money.USD
	Deployed       money.USD
	ActiveReserved money.USD // Σ active remaining usd
}

// VerifyEnvelopeBudgets returns every envelope whose budget flow is
// inconsistent. An empty result means every envelope conserves its
// allocation and its reserved budget matches its active reservations.
func VerifyEnvelopeBudgets(ctx context.Context, q db.Querier) ([]EnvelopeDrift, error) {
	rows, err := q.Query(ctx, `
		WITH active AS (
			SELECT envelope_id, sum(usd_minor - consumed_usd_minor)::bigint AS remaining
			FROM asset_reservations WHERE status = 'ACTIVE' AND envelope_id IS NOT NULL GROUP BY envelope_id
		)
		SELECT e.id, e.allocation_usd_minor, e.available_usd_minor, e.reserved_usd_minor, e.deployed_usd_minor, coalesce(a.remaining, 0)
		FROM capital_envelopes e
		LEFT JOIN active a ON a.envelope_id = e.id
		WHERE e.available_usd_minor + e.reserved_usd_minor + e.deployed_usd_minor <> e.allocation_usd_minor
		   OR e.reserved_usd_minor <> coalesce(a.remaining, 0)
		ORDER BY e.id`)
	if err != nil {
		return nil, dbErr("verify envelope budgets", err)
	}
	defer rows.Close()
	out := []EnvelopeDrift{}
	for rows.Next() {
		var d EnvelopeDrift
		if err := rows.Scan(&d.EnvelopeID, &d.Allocation, &d.Available, &d.Reserved, &d.Deployed, &d.ActiveReserved); err != nil {
			return nil, dbErr("scan envelope drift", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("iterate envelope drift", err)
	}
	return out, nil
}
