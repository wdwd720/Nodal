package transfers

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/money"
)

// Discrepancy codes Reconcile reports. Every one is a SEV1: the register,
// the effects and the ledger no longer tell the same story.
const (
	DiscPendingCommitted    = "TRANSFER_PENDING_COMMITTED"
	DiscEffectMismatch      = "TRANSFER_EFFECT_MISMATCH"
	DiscEffectWithoutRecord = "TRANSFER_EFFECT_WITHOUT_TRANSFER"
	DiscEffectJournalCount  = "TRANSFER_EFFECT_JOURNAL_COUNT"
	DiscUnresolvedEffect    = "TRANSFER_UNRESOLVED_EFFECT"
	DiscOrphanJournal       = "TRANSFER_JOURNAL_ORPHAN"
	DiscRegisterLedger      = "TRANSFER_REGISTER_LEDGER_MISMATCH"
	DiscClearingImbalance   = "TRANSFER_CLEARING_IMBALANCE"
	DiscWalletNotConserved  = "TRANSFER_WALLET_NOT_CONSERVED"
)

// Discrepancy is one way the books disagree.
type Discrepancy struct {
	Code       string `json:"code"`
	TransferID string `json:"transfer_id,omitempty"`
	AccountID  string `json:"account_id,omitempty"`
	AssetID    string `json:"asset_id,omitempty"`
	Detail     string `json:"detail"`
}

// AssetIdentity is the transfer clearing identity of one asset:
//
//	Σ_customers TRANSFER_OUT = Σ_customers TRANSFER_IN = Σ quantity of COMPLETED transfers
//	Σ_customers WALLET change over every transfer journal and its reversal = 0
//
// The first line is why the second holds: every unit that left a sender's
// WALLET arrived in a recipient's WALLET, so a transfer changes who owns the
// liability, never the liability.
type AssetIdentity struct {
	AssetID           string         `json:"asset_id"`
	TransferOut       money.Quantity `json:"transfer_out"`
	TransferIn        money.Quantity `json:"transfer_in"`
	RegisterCompleted money.Quantity `json:"register_completed"`
	WalletDelta       money.Quantity `json:"wallet_delta_of_transfer_journals"`
}

// ReconcileReport is the outcome of Reconcile.
type ReconcileReport struct {
	CheckedAt     time.Time       `json:"checked_at"`
	Transfers     int             `json:"transfers"`
	Assets        []AssetIdentity `json:"assets"`
	Discrepancies []Discrepancy   `json:"discrepancies"`
}

// OK reports whether nothing disagrees.
func (r ReconcileReport) OK() bool { return len(r.Discrepancies) == 0 }

// Reconcile proves, from the database alone, that:
//
//   - no transfer is left PENDING and no transfer effect PENDING or UNKNOWN
//     (both only exist inside the transaction that decides them);
//   - every COMPLETED transfer names an APPLIED effect (key p2p:<id>) that
//     applied its journal, and every REVERSED one a COMPENSATED effect whose
//     compensation is its reversal journal;
//   - every APPLIED or COMPENSATED transfer effect has exactly one
//     P2P_TRANSFER journal, and no transfer journal exists that no transfer
//     names;
//   - per party and asset, the ledger's TRANSFER_OUT and TRANSFER_IN equal
//     the register's COMPLETED outgoing and incoming quantities;
//   - per asset, Σ TRANSFER_OUT = Σ TRANSFER_IN, and the customer WALLET
//     change summed over every transfer journal and reversal is zero.
//
// With scope, only those assets are reconciled (effects and journals that
// cannot be attributed to an asset are then skipped). It reads only; run it
// in a REPEATABLE READ transaction for a consistent snapshot.
func (s *Service) Reconcile(ctx context.Context, q db.Querier, scope ...assets.AssetID) (ReconcileReport, error) {
	rep := ReconcileReport{CheckedAt: s.clk.Now().UTC(), Assets: []AssetIdentity{}, Discrepancies: []Discrepancy{}}
	var filter []string
	for _, a := range scope {
		filter = append(filter, a.String())
	}
	add := func(d Discrepancy) { rep.Discrepancies = append(rep.Discrepancies, d) }

	if err := q.QueryRow(ctx, `SELECT count(*) FROM internal_transfers WHERE ($1::uuid[] IS NULL OR asset_id = ANY($1))`,
		filter).Scan(&rep.Transfers); err != nil {
		return ReconcileReport{}, dbErr("count transfers", err)
	}

	// 1. Nothing is left in flight.
	if err := eachRow(ctx, q, `SELECT id::text, asset_id::text FROM internal_transfers
		WHERE status = 'PENDING' AND ($1::uuid[] IS NULL OR asset_id = ANY($1)) ORDER BY id`, []any{filter},
		func(r pgx.Rows) error {
			var d Discrepancy
			if err := r.Scan(&d.TransferID, &d.AssetID); err != nil {
				return err
			}
			d.Code, d.Detail = DiscPendingCommitted, "a committed transfer is still PENDING"
			add(d)
			return nil
		}); err != nil {
		return ReconcileReport{}, err
	}

	// 2. Register <-> effect <-> journal, transfer by transfer.
	if err := eachRow(ctx, q, `SELECT t.id::text, t.asset_id::text, t.status, coalesce(t.journal_transaction_id::text,''),
			coalesce(t.reversal_journal_transaction_id::text,''), coalesce(e.effect_key,''), coalesce(e.status,''),
			coalesce(e.journal_transaction_id::text,''), coalesce(e.compensation_journal_transaction_id::text,'')
		FROM internal_transfers t LEFT JOIN economic_effects e ON e.id = t.effect_id
		WHERE t.status IN ('COMPLETED','REVERSED') AND ($1::uuid[] IS NULL OR t.asset_id = ANY($1))
		ORDER BY t.id`, []any{filter},
		func(r pgx.Rows) error {
			var tid, asset, status, journal, reversal, key, estatus, ejournal, ecomp string
			if err := r.Scan(&tid, &asset, &status, &journal, &reversal, &key, &estatus, &ejournal, &ecomp); err != nil {
				return err
			}
			want := string(EffectStatusFor(Status(status)))
			ok := key == "p2p:"+tid && estatus == want && ejournal == journal && journal != ""
			if Status(status) == StatusReversed {
				ok = ok && ecomp == reversal && reversal != ""
			}
			if !ok {
				add(Discrepancy{Code: DiscEffectMismatch, TransferID: tid, AssetID: asset,
					Detail: "transfer " + status + " but its effect is " + estatus + " for journal " + ejournal})
			}
			return nil
		}); err != nil {
		return ReconcileReport{}, err
	}

	// 3. Effects: none unresolved, none without a transfer, each with
	// exactly one journal.
	if len(filter) == 0 {
		if err := eachRow(ctx, q, `SELECT e.id::text, e.status FROM economic_effects e
			WHERE e.subsystem = 'TRANSFERS' AND e.effect_kind = 'P2P_TRANSFER'
			  AND (e.status IN ('PENDING','UNKNOWN')
			       OR (e.status IN ('APPLIED','COMPENSATED') AND NOT EXISTS (SELECT 1 FROM internal_transfers t WHERE t.effect_id = e.id)))
			ORDER BY e.id`, nil,
			func(r pgx.Rows) error {
				var eid, status string
				if err := r.Scan(&eid, &status); err != nil {
					return err
				}
				if status == "PENDING" || status == "UNKNOWN" {
					add(Discrepancy{Code: DiscUnresolvedEffect, Detail: "transfer effect " + eid + " is " + status})
				} else {
					add(Discrepancy{Code: DiscEffectWithoutRecord, Detail: "transfer effect " + eid + " is " + status + " but no transfer names it"})
				}
				return nil
			}); err != nil {
			return ReconcileReport{}, err
		}
	}
	if err := eachRow(ctx, q, `SELECT t.id::text, t.asset_id::text, count(j.id)
		FROM internal_transfers t
		JOIN economic_effects e ON e.id = t.effect_id AND e.status IN ('APPLIED','COMPENSATED')
		LEFT JOIN journal_transactions j ON j.kind = 'P2P_TRANSFER' AND j.reference_type = 'p2p_transfer' AND j.reference_id = t.id::text
		WHERE ($1::uuid[] IS NULL OR t.asset_id = ANY($1))
		GROUP BY t.id, t.asset_id HAVING count(j.id) <> 1 ORDER BY t.id`, []any{filter},
		func(r pgx.Rows) error {
			var d Discrepancy
			var n int64
			if err := r.Scan(&d.TransferID, &d.AssetID, &n); err != nil {
				return err
			}
			d.Code, d.Detail = DiscEffectJournalCount, "an applied transfer effect must have exactly one P2P_TRANSFER journal"
			add(d)
			return nil
		}); err != nil {
		return ReconcileReport{}, err
	}
	if err := eachRow(ctx, q, `SELECT j.id::text, j.kind FROM journal_transactions j
		WHERE (j.kind = 'P2P_TRANSFER' OR (j.kind = 'COMPENSATION' AND j.reference_type = 'p2p_transfer_reversal'))
		  AND NOT EXISTS (SELECT 1 FROM internal_transfers t
		                   WHERE t.journal_transaction_id = j.id OR t.reversal_journal_transaction_id = j.id)
		  AND ($1::uuid[] IS NULL OR EXISTS (SELECT 1 FROM journal_entries x WHERE x.transaction_id = j.id AND x.asset_id = ANY($1)))
		ORDER BY j.id`, []any{filter},
		func(r pgx.Rows) error {
			var jid, kind string
			if err := r.Scan(&jid, &kind); err != nil {
				return err
			}
			add(Discrepancy{Code: DiscOrphanJournal, Detail: kind + " journal " + jid + " is named by no transfer"})
			return nil
		}); err != nil {
		return ReconcileReport{}, err
	}

	// 4. Ledger against register, per party and asset.
	type pa struct{ party, asset string }
	ledgerOut, ledgerIn := map[pa]money.Quantity{}, map[pa]money.Quantity{}
	regOut, regIn := map[pa]money.Quantity{}, map[pa]money.Quantity{}
	if err := eachRow(ctx, q, `SELECT la.owner_id::text, la.asset_id::text, la.code, coalesce(b.balance, 0)::text
		FROM ledger_accounts la LEFT JOIN ledger_balances b ON b.ledger_account_id = la.id
		WHERE la.owner_type = 'CUSTOMER' AND la.code IN ('TRANSFER_OUT','TRANSFER_IN')
		  AND ($1::uuid[] IS NULL OR la.asset_id = ANY($1))`, []any{filter},
		func(r pgx.Rows) error {
			var party, asset, code, bal string
			if err := r.Scan(&party, &asset, &code, &bal); err != nil {
				return err
			}
			v, err := money.ScanQuantity(bal)
			if err != nil {
				return err
			}
			if code == "TRANSFER_OUT" {
				ledgerOut[pa{party, asset}] = v
			} else {
				ledgerIn[pa{party, asset}] = v
			}
			return nil
		}); err != nil {
		return ReconcileReport{}, err
	}
	for _, side := range []struct {
		col string
		dst map[pa]money.Quantity
	}{{"sender_account_id", regOut}, {"recipient_account_id", regIn}} {
		if err := eachRow(ctx, q, `SELECT `+side.col+`::text, asset_id::text, sum(quantity)::text FROM internal_transfers
			WHERE status = 'COMPLETED' AND ($1::uuid[] IS NULL OR asset_id = ANY($1)) GROUP BY 1, 2`, []any{filter},
			func(r pgx.Rows) error {
				var party, asset, sum string
				if err := r.Scan(&party, &asset, &sum); err != nil {
					return err
				}
				v, err := money.ScanQuantity(sum)
				if err != nil {
					return err
				}
				side.dst[pa{party, asset}] = v
				return nil
			}); err != nil {
			return ReconcileReport{}, err
		}
	}
	perAsset := map[string]*AssetIdentity{}
	identity := func(asset string) *AssetIdentity {
		if a, ok := perAsset[asset]; ok {
			return a
		}
		a := &AssetIdentity{AssetID: asset}
		perAsset[asset] = a
		return a
	}
	for _, pair := range []struct {
		name            string
		ledger, regster map[pa]money.Quantity
	}{{"TRANSFER_OUT", ledgerOut, regOut}, {"TRANSFER_IN", ledgerIn, regIn}} {
		keys := map[pa]bool{}
		for k := range pair.ledger {
			keys[k] = true
		}
		for k := range pair.regster {
			keys[k] = true
		}
		sorted := make([]pa, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].asset != sorted[j].asset {
				return sorted[i].asset < sorted[j].asset
			}
			return sorted[i].party < sorted[j].party
		})
		for _, k := range sorted {
			l, r := pair.ledger[k], pair.regster[k]
			id := identity(k.asset)
			if pair.name == "TRANSFER_OUT" {
				id.TransferOut = id.TransferOut.Add(l)
				id.RegisterCompleted = id.RegisterCompleted.Add(r)
			} else {
				id.TransferIn = id.TransferIn.Add(l)
			}
			if !l.Equal(r) {
				add(Discrepancy{Code: DiscRegisterLedger, AccountID: k.party, AssetID: k.asset,
					Detail: pair.name + " is " + l.String() + " in the ledger and " + r.String() + " in the register"})
			}
		}
	}

	// 5. The WALLET legs of every transfer journal and reversal cancel.
	if err := eachRow(ctx, q, `SELECT e.asset_id::text, sum(CASE WHEN e.side = 'DEBIT' THEN e.quantity ELSE -e.quantity END)::text
		FROM journal_entries e
		JOIN journal_transactions j ON j.id = e.transaction_id
		JOIN ledger_accounts la ON la.id = e.ledger_account_id
		WHERE la.owner_type = 'CUSTOMER' AND la.code = 'WALLET'
		  AND (j.kind = 'P2P_TRANSFER' OR (j.kind = 'COMPENSATION' AND j.reference_type = 'p2p_transfer_reversal'))
		  AND ($1::uuid[] IS NULL OR e.asset_id = ANY($1))
		GROUP BY e.asset_id`, []any{filter},
		func(r pgx.Rows) error {
			var asset, sum string
			if err := r.Scan(&asset, &sum); err != nil {
				return err
			}
			v, err := money.ScanQuantity(sum)
			if err != nil {
				return err
			}
			identity(asset).WalletDelta = v
			return nil
		}); err != nil {
		return ReconcileReport{}, err
	}
	assetsSorted := make([]string, 0, len(perAsset))
	for a := range perAsset {
		assetsSorted = append(assetsSorted, a)
	}
	sort.Strings(assetsSorted)
	for _, a := range assetsSorted {
		id := *perAsset[a]
		if !id.TransferOut.Equal(id.TransferIn) || !id.TransferOut.Equal(id.RegisterCompleted) {
			add(Discrepancy{Code: DiscClearingImbalance, AssetID: a,
				Detail: "TRANSFER_OUT " + id.TransferOut.String() + ", TRANSFER_IN " + id.TransferIn.String() +
					", register " + id.RegisterCompleted.String()})
		}
		if !id.WalletDelta.IsZero() {
			add(Discrepancy{Code: DiscWalletNotConserved, AssetID: a,
				Detail: "transfer journals changed customer WALLET by " + id.WalletDelta.String()})
		}
		rep.Assets = append(rep.Assets, id)
	}
	return rep, nil
}

// EffectStatusFor is the economic effect status a settled transfer status
// implies; the empty status for an unsettled one.
func EffectStatusFor(s Status) econ.EffectStatus {
	switch s {
	case StatusCompleted:
		return econ.EffectApplied
	case StatusReversed:
		return econ.EffectCompensated
	}
	return ""
}

func eachRow(ctx context.Context, q db.Querier, sql string, args []any, fn func(pgx.Rows) error) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return dbErr("reconcile query", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return dbErr("reconcile scan", err)
		}
	}
	if err := rows.Err(); err != nil {
		return dbErr("reconcile rows", err)
	}
	return nil
}
