package equities

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// CorporateAction is an equity_corporate_actions row.
type CorporateAction struct {
	ID              CorporateActionID
	InstrumentID    instruments.InstrumentID
	Type            CorporateActionType
	Status          CorporateActionStatus
	EffectiveDate   Date
	ShareAssetID    assets.AssetID
	CashAssetID     assets.AssetID
	Ratio           *Ratio
	CashPerShare    *PricePoint
	CashInLieuPrice *PricePoint
	ExDate          Date
	RecordDate      Date
	PayDate         Date
	FinalDate       Date
	NewSymbol       string
	Rounding        CorporateRounding
	Reason          string
	ContentHash     []byte
	AnnouncedBy     econ.Actor
	AppliedAt       *time.Time
	CreatedAt       time.Time
}

const actionColumns = `id, instrument_id, action_type, status, effective_date, share_asset_id, cash_asset_id, ratio_numerator,
	ratio_denominator, coalesce(cash_per_share::text, ''), cash_per_share_scale, coalesce(cash_in_lieu_price::text, ''),
	cash_in_lieu_price_scale, ex_date, record_date, pay_date, final_date, coalesce(new_symbol, ''), rounding_policy, reason,
	content_hash, announced_by_actor_type, announced_by_actor_id, applied_at, created_at`

func scanAction(row pgx.Row) (CorporateAction, error) {
	var (
		a                          CorporateAction
		typ, status, rounding      string
		effective                  time.Time
		num, den                   *int64
		perShare, inLieu           string
		perShareScale, inLieuScale *int32
		ex, record, pay, final     *time.Time
		actorType                  string
	)
	if err := row.Scan(&a.ID, &a.InstrumentID, &typ, &status, &effective, &a.ShareAssetID, &a.CashAssetID, &num, &den,
		&perShare, &perShareScale, &inLieu, &inLieuScale, &ex, &record, &pay, &final, &a.NewSymbol, &rounding, &a.Reason,
		&a.ContentHash, &actorType, &a.AnnouncedBy.ID, &a.AppliedAt, &a.CreatedAt); err != nil {
		return CorporateAction{}, err
	}
	a.Type, a.Status, a.Rounding = CorporateActionType(typ), CorporateActionStatus(status), CorporateRounding(rounding)
	a.AnnouncedBy.Type = security.ActorType(actorType)
	a.EffectiveDate = DateOf(effective)
	a.CreatedAt = a.CreatedAt.UTC()
	if a.AppliedAt != nil {
		t := a.AppliedAt.UTC()
		a.AppliedAt = &t
	}
	if num != nil && den != nil {
		a.Ratio = &Ratio{Numerator: *num, Denominator: *den}
	}
	var err error
	if a.CashPerShare, err = pricePoint(perShare, perShareScale); err != nil {
		return CorporateAction{}, err
	}
	if a.CashInLieuPrice, err = pricePoint(inLieu, inLieuScale); err != nil {
		return CorporateAction{}, err
	}
	for _, d := range []struct {
		dst *Date
		src *time.Time
	}{{&a.ExDate, ex}, {&a.RecordDate, record}, {&a.PayDate, pay}, {&a.FinalDate, final}} {
		if d.src != nil {
			*d.dst = DateOf(*d.src)
		}
	}
	return a, nil
}

func pricePoint(mantissa string, scale *int32) (*PricePoint, error) {
	if mantissa == "" || scale == nil {
		return nil, nil
	}
	m, err := money.ScanQuantity(mantissa)
	if err != nil {
		return nil, err
	}
	return &PricePoint{Mantissa: m, Scale: *scale}, nil
}

func dateArg(d Date) any {
	if d.IsZero() {
		return nil
	}
	return d.Time()
}

// insertAction inserts an ANNOUNCED action, or returns the existing action
// with the same (instrument, type, effective date) and inserted=false.
func insertAction(ctx context.Context, tx pgx.Tx, a CorporateAction) (CorporateAction, bool, error) {
	var num, den any
	if a.Ratio != nil {
		num, den = a.Ratio.Numerator, a.Ratio.Denominator
	}
	var perShare, perShareScale, inLieu, inLieuScale any
	if a.CashPerShare != nil {
		perShare, perShareScale = a.CashPerShare.Mantissa.String(), a.CashPerShare.Scale
	}
	if a.CashInLieuPrice != nil {
		inLieu, inLieuScale = a.CashInLieuPrice.Mantissa.String(), a.CashInLieuPrice.Scale
	}
	out, err := scanAction(tx.QueryRow(ctx, `INSERT INTO equity_corporate_actions (id, instrument_id, action_type, status,
		effective_date, share_asset_id, cash_asset_id, ratio_numerator, ratio_denominator, cash_per_share, cash_per_share_scale,
		cash_in_lieu_price, cash_in_lieu_price_scale, ex_date, record_date, pay_date, final_date, new_symbol, rounding_policy,
		reason, content_hash, announced_by_actor_type, announced_by_actor_id, created_at, updated_at)
		VALUES ($1,$2,$3,'ANNOUNCED',$4,$5,$6,$7,$8,$9::numeric,$10,$11::numeric,$12,$13,$14,$15,$16,NULLIF($17,''),$18,$19,$20,$21,$22,$23,$23)
		ON CONFLICT (instrument_id, action_type, effective_date) DO NOTHING
		RETURNING `+actionColumns,
		a.ID, a.InstrumentID, string(a.Type), a.EffectiveDate.Time(), a.ShareAssetID, a.CashAssetID, num, den, perShare,
		perShareScale, inLieu, inLieuScale, dateArg(a.ExDate), dateArg(a.RecordDate), dateArg(a.PayDate), dateArg(a.FinalDate),
		a.NewSymbol, string(a.Rounding), a.Reason, a.ContentHash, string(a.AnnouncedBy.Type), a.AnnouncedBy.ID, a.CreatedAt.UTC()))
	if err == nil {
		return out, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CorporateAction{}, false, storeErr("announce corporate action", err)
	}
	existing, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionColumns+` FROM equity_corporate_actions
		WHERE instrument_id = $1 AND action_type = $2 AND effective_date = $3`, a.InstrumentID, string(a.Type), a.EffectiveDate.Time()))
	if err != nil {
		return CorporateAction{}, false, storeErr("load corporate action", err)
	}
	return existing, false, nil
}

func getAction(ctx context.Context, q db.Querier, aid CorporateActionID, forUpdate bool) (CorporateAction, error) {
	sql := `SELECT ` + actionColumns + ` FROM equity_corporate_actions WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	a, err := scanAction(q.QueryRow(ctx, sql, aid))
	if errors.Is(err, pgx.ErrNoRows) {
		return CorporateAction{}, errs.New(errs.CodeNotFound, "corporate action not found").WithField("action_id", aid.String())
	}
	if err != nil {
		return CorporateAction{}, storeErr("load corporate action", err)
	}
	return a, nil
}

// openActions returns the ANNOUNCED and APPLYING actions of an instrument.
func openActions(ctx context.Context, q db.Querier, iid instruments.InstrumentID) ([]CorporateAction, error) {
	rows, err := q.Query(ctx, `SELECT `+actionColumns+` FROM equity_corporate_actions
		WHERE instrument_id = $1 AND status IN ('ANNOUNCED','APPLYING') ORDER BY effective_date, id`, iid)
	if err != nil {
		return nil, storeErr("list corporate actions", err)
	}
	defer rows.Close()
	var out []CorporateAction
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, storeErr("scan corporate action", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func transitionAction(ctx context.Context, tx pgx.Tx, a CorporateAction, to CorporateActionStatus, actor econ.Actor, reason string, now time.Time) (CorporateAction, error) {
	if !CanTransitionAction(a.Status, to) {
		return CorporateAction{}, errs.Newf(errs.CodeInvalidStateTransition, "corporate action %s cannot move %s -> %s", a.ID, a.Status, to).
			WithField("action_id", a.ID.String())
	}
	if _, err := tx.Exec(ctx, `INSERT INTO equity_corporate_action_transitions (id, action_id, from_status, to_status, actor_type,
		actor_id, reason, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id.New[id.Any](), a.ID, string(a.Status), string(to), string(actor.Type), actor.ID, reason, now.UTC()); err != nil {
		return CorporateAction{}, storeErr("record corporate action transition", err)
	}
	var applied any
	if to == ActionApplied {
		applied = now.UTC()
	}
	out, err := scanAction(tx.QueryRow(ctx, `UPDATE equity_corporate_actions SET status = $2, applied_at = coalesce($3, applied_at)
		WHERE id = $1 RETURNING `+actionColumns, a.ID, string(to), applied))
	if err != nil {
		return CorporateAction{}, storeErr("update corporate action", err)
	}
	return out, nil
}

// Application is a corporate_action_applications row: one account's share of
// one corporate action, applied once.
type Application struct {
	ID                   ApplicationID
	ActionID             CorporateActionID
	AccountID            accounts.AccountID
	EntitledQuantity     money.Quantity
	SharesDelta          money.Quantity
	CashAmount           money.Quantity
	ExactCashNumerator   money.Quantity
	ExactCashDenominator money.Quantity
	Rounding             CorporateRounding
	JournalTransactionID *ledger.TransactionID
	EffectID             *econ.EffectID
	CreatedAt            time.Time
}

const applicationColumns = `id, action_id, account_id, entitled_quantity::text, shares_delta::text, cash_amount::text,
	exact_cash_numerator::text, exact_cash_denominator::text, rounding_policy, journal_transaction_id, effect_id, created_at`

func scanApplication(row pgx.Row) (Application, error) {
	var (
		a                               Application
		entitled, delta, cash, num, den string
		rounding                        string
		journal                         ledger.TransactionID
		effect                          econ.EffectID
	)
	if err := row.Scan(&a.ID, &a.ActionID, &a.AccountID, &entitled, &delta, &cash, &num, &den, &rounding, &journal, &effect,
		&a.CreatedAt); err != nil {
		return Application{}, err
	}
	a.Rounding, a.CreatedAt = CorporateRounding(rounding), a.CreatedAt.UTC()
	for _, p := range []struct {
		dst *money.Quantity
		src string
	}{{&a.EntitledQuantity, entitled}, {&a.SharesDelta, delta}, {&a.CashAmount, cash}, {&a.ExactCashNumerator, num},
		{&a.ExactCashDenominator, den}} {
		v, err := money.ScanQuantity(p.src)
		if err != nil {
			return Application{}, err
		}
		*p.dst = v
	}
	if !journal.IsZero() {
		a.JournalTransactionID = &journal
	}
	if !effect.IsZero() {
		a.EffectID = &effect
	}
	return a, nil
}

func insertApplication(ctx context.Context, tx pgx.Tx, a Application) (Application, error) {
	var journal, effect any
	if a.JournalTransactionID != nil {
		journal = *a.JournalTransactionID
	}
	if a.EffectID != nil {
		effect = *a.EffectID
	}
	out, err := scanApplication(tx.QueryRow(ctx, `INSERT INTO corporate_action_applications (id, action_id, account_id,
		entitled_quantity, shares_delta, cash_amount, exact_cash_numerator, exact_cash_denominator, rounding_policy,
		journal_transaction_id, effect_id, created_at)
		VALUES ($1,$2,$3,$4::numeric,$5::numeric,$6::numeric,$7::numeric,$8::numeric,$9,$10,$11,$12)
		RETURNING `+applicationColumns,
		a.ID, a.ActionID, a.AccountID, a.EntitledQuantity.String(), a.SharesDelta.String(), a.CashAmount.String(),
		a.ExactCashNumerator.String(), a.ExactCashDenominator.String(), string(a.Rounding), journal, effect, a.CreatedAt.UTC()))
	if err != nil {
		return Application{}, storeErr("record corporate action application", err)
	}
	return out, nil
}

// Applications returns every application of a corporate action, by account.
func Applications(ctx context.Context, q db.Querier, aid CorporateActionID) ([]Application, error) {
	rows, err := q.Query(ctx, `SELECT `+applicationColumns+` FROM corporate_action_applications WHERE action_id = $1 ORDER BY account_id`, aid)
	if err != nil {
		return nil, storeErr("list applications", err)
	}
	defer rows.Close()
	var out []Application
	for rows.Next() {
		a, err := scanApplication(rows)
		if err != nil {
			return nil, storeErr("scan application", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func applicationFor(ctx context.Context, q db.Querier, aid CorporateActionID, acct accounts.AccountID) (Application, bool, error) {
	a, err := scanApplication(q.QueryRow(ctx, `SELECT `+applicationColumns+` FROM corporate_action_applications
		WHERE action_id = $1 AND account_id = $2`, aid, acct))
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, false, nil
	}
	if err != nil {
		return Application{}, false, storeErr("load application", err)
	}
	return a, true, nil
}
