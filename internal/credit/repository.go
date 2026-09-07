package credit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// consumptionOrderSQL is the ORDER BY that implements ConsumptionRank.
//
// It is a CONSTANT rather than a value built from consumptionRank at init, and
// the reason is a security control rather than style: test/security proves that
// every SQL statement in the repository is built from constants, so that no
// request-derived string can ever be concatenated into one. A statement
// assembled from a package-level var is not provably constant, and weakening
// the check to accommodate this package would weaken it for every package.
//
// The Go map remains the authority on the ordering. buildConsumptionOrderSQL
// regenerates this string from it and TestConsumptionOrderSQL_MatchesTheMap
// asserts the two are identical, so the constant cannot drift from the rank it
// is supposed to implement.
const consumptionOrderSQL = `CASE l.origin` +
	` WHEN 'PROMOTIONAL' THEN 0` +
	` WHEN 'COMPETITION_REWARD' THEN 1` +
	` WHEN 'ADMIN_ADJUSTMENT' THEN 2` +
	` WHEN 'REFUND' THEN 3` +
	` WHEN 'PURCHASED' THEN 4` +
	` WHEN 'PROVIDER_SETTLEMENT' THEN 5` +
	` WHEN 'MARKET_TRADING_PROCEEDS' THEN 6` +
	` WHEN 'MARKET_CREATOR_EARNING' THEN 7` +
	` WHEN 'AGENT_SERVICE_EARNING' THEN 8` +
	` WHEN 'DATA_SALE_EARNING' THEN 9` +
	` WHEN 'CREATOR_EARNING' THEN 10` +
	` ELSE 11 END`

// buildConsumptionOrderSQL regenerates the constant above from consumptionRank.
// It exists so a test can prove the two agree; nothing else calls it.
func buildConsumptionOrderSQL() string {
	type entry struct {
		origin valuedomain.CreditOrigin
		rank   int
	}
	entries := make([]entry, 0, len(consumptionRank))
	for o, r := range consumptionRank {
		entries = append(entries, entry{o, r})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rank < entries[j].rank })

	var b strings.Builder
	b.WriteString("CASE l.origin")
	for _, e := range entries {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", e.origin, e.rank)
	}
	fmt.Fprintf(&b, " ELSE %d END", len(consumptionRank))
	return b.String()
}

// openLotsQuery selects an account's open lots in consumption order.
//
// $3 is "require spendable finality" and $4 is the allowed-origin set; a null
// or empty set means no origin restriction. Expressing both as parameters of
// one constant statement is what lets test/security prove the statement is not
// assembled from anything a request supplied.
const openLotsQuery = `SELECT ` + lotColumns + `
	  FROM credit_lots l
	  JOIN credit_lot_state st ON st.lot_id = l.id
	 WHERE l.account_id = $1 AND l.asset_id = $2 AND st.remaining_quantity > 0
	   AND ($3::boolean = false OR st.finality IN ('UNFUNDED','REVERSIBLE','SETTLED'))
	   AND ($4::text[] IS NULL OR cardinality($4::text[]) = 0 OR l.origin = ANY($4::text[]))
	 ORDER BY ` + consumptionOrderSQL + `, l.created_at, l.id`

// accountLotsQuery is every lot an account holds, in the same order.
const accountLotsQuery = `SELECT ` + lotColumns + `
	  FROM credit_lots l JOIN credit_lot_state st ON st.lot_id = l.id
	 WHERE l.account_id = $1 AND l.asset_id = $2
	 ORDER BY ` + consumptionOrderSQL + `, l.created_at, l.id`

const lotColumns = `l.id, l.account_id, l.asset_id, l.origin, l.initial_finality, l.quantity::text,
	 coalesce(l.funding_reference_type,''), coalesce(l.funding_reference_id,''), l.journal_transaction_id,
	 l.issued_by_actor_type, l.issued_by_actor_id, l.reason, l.created_at,
	 st.remaining_quantity::text, st.finality, st.version`

func scanLot(row pgx.Row) (Lot, error) {
	var (
		l                  Lot
		qty, remaining     string
		refType, refID     string
		origin, initialFin string
		finality           string
	)
	if err := row.Scan(&l.ID, &l.AccountID, &l.AssetID, &origin, &initialFin, &qty,
		&refType, &refID, &l.JournalTxID,
		&l.IssuedByActorType, &l.IssuedByActorID, &l.Reason, &l.CreatedAt,
		&remaining, &finality, &l.Version); err != nil {
		return Lot{}, err
	}
	var err error
	if l.Quantity, err = money.ParseQuantity(qty); err != nil {
		return Lot{}, errs.Wrap(err, errs.CodeInternal, "credit: lot quantity is not an integer")
	}
	if l.Remaining, err = money.ParseQuantity(remaining); err != nil {
		return Lot{}, errs.Wrap(err, errs.CodeInternal, "credit: lot remaining is not an integer")
	}
	l.Origin = valuedomain.CreditOrigin(origin)
	l.InitialFinality = valuedomain.FundingFinality(initialFin)
	l.Finality = valuedomain.FundingFinality(finality)
	if refType != "" {
		l.FundingReference = &Reference{Type: refType, ID: refID}
	}
	l.CreatedAt = l.CreatedAt.UTC()
	return l, nil
}

// openLotsForUpdate returns the account's lots that still hold units, in
// consumption order, with each row locked.
//
// The lock is taken in the same deterministic order by every caller, so two
// concurrent spenders on the same account queue behind each other instead of
// deadlocking. Ordering by (rank, created_at, id) makes the third key a
// tiebreak that cannot collide, so the order is total.
func (s *Service) openLotsForUpdate(
	ctx context.Context, tx pgx.Tx,
	accountID accounts.AccountID, assetID assets.AssetID,
	requireSpendable bool, allowedOrigins []valuedomain.CreditOrigin,
) ([]Lot, error) {
	// Both filters are PARAMETERS of one constant statement rather than
	// fragments concatenated into a built one. An earlier version assembled
	// the WHERE clause from literals, which was safe and was not provably
	// safe, and test/security proves every statement in the repository is
	// built from constants precisely so that nobody has to take "safe" on
	// trust.
	var origins []string
	if len(allowedOrigins) > 0 {
		origins = make([]string, 0, len(allowedOrigins))
		for _, o := range allowedOrigins {
			origins = append(origins, string(o))
		}
	}
	args := []any{accountID, assetID, requireSpendable, origins}
	// Serialise spenders on this account's Credits before reading the lots.
	//
	// `FOR UPDATE OF st` would be the obvious way and is not available: the
	// projection is deliberately not writable by the application role (that is
	// what stops a remaining quantity from being edited into existence), and
	// PostgreSQL requires UPDATE privilege to take a row lock. Granting it back
	// to get a lock would trade the guarantee for the mechanism.
	//
	// An advisory transaction lock is also the better fit. The contended
	// resource is the ACCOUNT's Credit balance, not any individual lot: two
	// spenders racing on the same account must serialise even when they would
	// select disjoint lots, because whether the lots are disjoint is exactly
	// what is being decided. The lock releases at commit or rollback, so a
	// crashed spender cannot hold it.
	if err := lockAccountCredits(ctx, tx, accountID, assetID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, openLotsQuery, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Lot
	for rows.Next() {
		l, err := scanLot(rows)
		if err != nil {
			return nil, mapError(err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

// lotEvent is one row of credit_lot_events before it is written.
type lotEvent struct {
	lotID       LotID
	kind        string
	delta       *money.Quantity
	toFinality  *valuedomain.FundingFinality
	reference   Reference
	journalTxID *ledger.TransactionID
	reason      string
}

// appendEvent writes one lot event. seq is assigned from the lot's existing
// event count inside the statement, so two concurrent writers cannot compute
// the same value: the UNIQUE (lot_id, seq) constraint would reject the loser,
// and the row lock openLotsForUpdate holds means they do not race in practice.
func (s *Service) appendEvent(ctx context.Context, tx pgx.Tx, e lotEvent) (LotEventID, error) {
	actorType, actorID := actorFrom(ctx)
	var (
		delta any
		toFin any
		jtx   any
	)
	if e.delta != nil {
		delta = e.delta.String()
	}
	if e.toFinality != nil {
		toFin = string(*e.toFinality)
	}
	if e.journalTxID != nil && !e.journalTxID.IsZero() {
		jtx = *e.journalTxID
	}
	evID := NewLotEventID()
	_, err := tx.Exec(ctx,
		`INSERT INTO credit_lot_events
		   (id, lot_id, seq, kind, delta_quantity, to_finality, reference_type, reference_id,
		    journal_transaction_id, reason, actor_type, actor_id)
		 SELECT $1, $2, coalesce(st.event_count, 0) + 1, $3, $4::numeric, $5, $6, $7, $8, $9, $10, $11
		   FROM credit_lot_state st WHERE st.lot_id = $2`,
		evID, e.lotID, e.kind, delta, toFin, e.reference.Type, e.reference.ID,
		jtx, e.reason, actorType, actorID)
	if err != nil {
		return LotEventID{}, mapError(err)
	}
	return evID, nil
}

// lotByJournalTx finds the lot a replayed issuance already created.
func (s *Service) lotByJournalTx(ctx context.Context, q db.Querier, txID ledger.TransactionID) (Lot, error) {
	l, err := scanLot(q.QueryRow(ctx,
		`SELECT `+lotColumns+`
		   FROM credit_lots l JOIN credit_lot_state st ON st.lot_id = l.id
		  WHERE l.journal_transaction_id = $1`, txID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Lot{}, errs.New(errs.CodeInternal,
				"credit: an issuance replayed but its provenance lot is missing").
				WithField("journal_transaction_id", txID.String())
		}
		return Lot{}, mapError(err)
	}
	return l, nil
}

// Lot returns one lot by id.
func (s *Service) Lot(ctx context.Context, q db.Querier, lotID LotID) (Lot, error) {
	l, err := scanLot(q.QueryRow(ctx,
		`SELECT `+lotColumns+`
		   FROM credit_lots l JOIN credit_lot_state st ON st.lot_id = l.id
		  WHERE l.id = $1`, lotID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Lot{}, errs.New(errs.CodeNotFound, "credit lot not found").WithField("lot_id", lotID.String())
		}
		return Lot{}, mapError(err)
	}
	return l, nil
}

// Lots returns every lot an account holds, in consumption order. It is the
// read model behind the balance breakdown and the operator view.
func (s *Service) Lots(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]Lot, error) {
	assetID, err := s.AssetID(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, accountLotsQuery, accountID, assetID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Lot
	for rows.Next() {
		l, err := scanLot(rows)
		if err != nil {
			return nil, mapError(err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LotsByFundingReference returns the lots a given funding event produced. A
// chargeback uses it to find exactly what it must claw back.
func (s *Service) LotsByFundingReference(ctx context.Context, q db.Querier, ref Reference) ([]Lot, error) {
	rows, err := q.Query(ctx,
		`SELECT `+lotColumns+`
		   FROM credit_lots l JOIN credit_lot_state st ON st.lot_id = l.id
		  WHERE l.funding_reference_type = $1 AND l.funding_reference_id = $2
		  ORDER BY l.created_at, l.id`, ref.Type, ref.ID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Lot
	for rows.Next() {
		l, err := scanLot(rows)
		if err != nil {
			return nil, mapError(err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// VerifyProvenance checks the invariant that ties this package to the ledger:
// the sum of an account's remaining lot quantities must equal its
// CREDIT_BALANCE. A divergence means provenance and balance have come apart,
// which is a reconciliation incident, never something to paper over.
func (s *Service) VerifyProvenance(ctx context.Context, q db.Querier, accountID accounts.AccountID) error {
	assetID, err := s.AssetID(ctx, q)
	if err != nil {
		return err
	}
	var lotSum, balance string
	err = q.QueryRow(ctx,
		`SELECT
		   coalesce((SELECT sum(st.remaining_quantity)
		               FROM credit_lots l JOIN credit_lot_state st ON st.lot_id = l.id
		              WHERE l.account_id = $1 AND l.asset_id = $2), 0)::text,
		   coalesce((SELECT b.balance
		               FROM ledger_accounts la JOIN ledger_balances b ON b.ledger_account_id = la.id
		              WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1
		                AND la.code = 'CREDIT_BALANCE' AND la.asset_id = $2), 0)::text`,
		accountID, assetID).Scan(&lotSum, &balance)
	if err != nil {
		return mapError(err)
	}
	if lotSum != balance {
		return errs.Newf(errs.CodeReconciliationRequired,
			"credit provenance does not match the ledger: lots hold %s, CREDIT_BALANCE is %s",
			lotSum, balance).
			WithField("account_id", accountID.String()).
			WithField("lot_sum", lotSum).
			WithField("ledger_balance", balance)
	}
	return nil
}

// lockAccountCredits takes an advisory transaction lock on one account's
// Credits. The key is derived from the account and asset ids so two different
// accounts never contend, and it is stable across processes.
func lockAccountCredits(ctx context.Context, tx pgx.Tx, accountID accounts.AccountID, assetID assets.AssetID) error {
	return advisoryLock(ctx, tx, "credit:"+accountID.String()+":"+assetID.String())
}

// lockLot takes an advisory transaction lock on one lot, used where the change
// is to the lot itself rather than to an account's spendable balance.
func lockLot(ctx context.Context, tx pgx.Tx, lotID LotID) error {
	return advisoryLock(ctx, tx, "credit_lot:"+lotID.String())
}

// advisoryLock hashes key to a bigint and takes pg_advisory_xact_lock on it.
//
// md5 is used purely to spread keys across the lock space; it carries no
// security weight here, and a collision would cost two unrelated accounts a
// little contention rather than any correctness.
func advisoryLock(ctx context.Context, tx pgx.Tx, key string) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock((('x' || substr(md5($1), 1, 16))::bit(64))::bigint)`, key); err != nil {
		return mapError(err)
	}
	return nil
}
