package stableasset

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// MintRecord is a stable_mint_requests row.
type MintRecord struct {
	ID                   MintRequestID
	StableAssetID        StableAssetID
	AccountID            accounts.AccountID
	IntentID             econ.IntentID
	EffectID             econ.EffectID
	Mode                 gates.Mode
	Quantity             money.Quantity
	ReserveAssetID       assets.AssetID
	ReserveQuantity      money.Quantity
	ReservationID        capital.ReservationID
	Recipient            devnet.PublicKey
	Plan                 MintPlan
	PlanHash             [32]byte
	SigningPlan          SigningPlan
	Chain                devnet.ChainID
	TxID                 devnet.TxID
	SignedTx             []byte
	RecentBlockhash      devnet.Hash
	Status               MintStatus
	FailureCode          string
	JournalTransactionID *ledger.TransactionID
	FinalSlot            *int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// BurnRecord is a stable_burn_requests row.
type BurnRecord struct {
	ID                   BurnRequestID
	StableAssetID        StableAssetID
	AccountID            accounts.AccountID
	IntentID             econ.IntentID
	EffectID             econ.EffectID
	Mode                 gates.Mode
	Quantity             money.Quantity
	PayoutAssetID        assets.AssetID
	PayoutQuantity       money.Quantity
	ReservationID        capital.ReservationID
	Holder               devnet.PublicKey
	Plan                 BurnPlan
	PlanHash             [32]byte
	SigningPlan          SigningPlan
	Chain                devnet.ChainID
	TxID                 devnet.TxID
	SignedTx             []byte
	RecentBlockhash      devnet.Hash
	Status               BurnStatus
	FailureCode          string
	JournalTransactionID *ledger.TransactionID
	FinalSlot            *int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

const mintColumns = `id, stable_asset_id, account_id, intent_id, effect_id, mode, quantity::text, reserve_asset_id,
	reserve_quantity::text, reservation_id, recipient, plan, plan_hash, signing_plan, chain_id, tx_id, signed_tx,
	recent_blockhash, status, coalesce(failure_code, ''), journal_transaction_id, final_slot, created_at, updated_at`

const burnColumns = `id, stable_asset_id, account_id, intent_id, effect_id, mode, quantity::text, payout_asset_id,
	payout_quantity::text, reservation_id, holder, plan, plan_hash, signing_plan, chain_id, tx_id, signed_tx,
	recent_blockhash, status, coalesce(failure_code, ''), journal_transaction_id, final_slot, created_at, updated_at`

// scanned holds the columns both request tables share.
type scanned struct {
	qty, otherQty, owner, chain, txID, blockhash string
	plan, signingPlan, planHash                  []byte
}

func (sc scanned) decode(planInto any, signing *SigningPlan, hash *[32]byte, qty, other *money.Quantity,
	owner *devnet.PublicKey, chain *devnet.ChainID, txID *devnet.TxID, bh *devnet.Hash) error {
	var err error
	if *qty, err = money.ScanQuantity(sc.qty); err != nil {
		return err
	}
	if *other, err = money.ScanQuantity(sc.otherQty); err != nil {
		return err
	}
	if *owner, err = devnet.ParsePublicKey(sc.owner); err != nil {
		return err
	}
	if *bh, err = devnet.ParseHash(sc.blockhash); err != nil {
		return err
	}
	if err := json.Unmarshal(sc.plan, planInto); err != nil {
		return err
	}
	if err := json.Unmarshal(sc.signingPlan, signing); err != nil {
		return err
	}
	if len(sc.planHash) != 32 {
		return errs.New(errs.CodeInternal, "stableasset: stored plan hash has the wrong length")
	}
	copy(hash[:], sc.planHash)
	*chain, *txID = devnet.ChainID(sc.chain), devnet.TxID(sc.txID)
	return nil
}

func scanMint(row pgx.Row) (MintRecord, error) {
	var r MintRecord
	var sc scanned
	if err := row.Scan(&r.ID, &r.StableAssetID, &r.AccountID, &r.IntentID, &r.EffectID, &r.Mode, &sc.qty, &r.ReserveAssetID,
		&sc.otherQty, &r.ReservationID, &sc.owner, &sc.plan, &sc.planHash, &sc.signingPlan, &sc.chain, &sc.txID, &r.SignedTx,
		&sc.blockhash, &r.Status, &r.FailureCode, &r.JournalTransactionID, &r.FinalSlot, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return MintRecord{}, err
	}
	if err := sc.decode(&r.Plan, &r.SigningPlan, &r.PlanHash, &r.Quantity, &r.ReserveQuantity, &r.Recipient,
		&r.Chain, &r.TxID, &r.RecentBlockhash); err != nil {
		return MintRecord{}, errs.Wrap(err, errs.CodeInternal, "stableasset: stored mint request does not decode")
	}
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	return r, nil
}

func scanBurn(row pgx.Row) (BurnRecord, error) {
	var r BurnRecord
	var sc scanned
	if err := row.Scan(&r.ID, &r.StableAssetID, &r.AccountID, &r.IntentID, &r.EffectID, &r.Mode, &sc.qty, &r.PayoutAssetID,
		&sc.otherQty, &r.ReservationID, &sc.owner, &sc.plan, &sc.planHash, &sc.signingPlan, &sc.chain, &sc.txID, &r.SignedTx,
		&sc.blockhash, &r.Status, &r.FailureCode, &r.JournalTransactionID, &r.FinalSlot, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return BurnRecord{}, err
	}
	if err := sc.decode(&r.Plan, &r.SigningPlan, &r.PlanHash, &r.Quantity, &r.PayoutQuantity, &r.Holder,
		&r.Chain, &r.TxID, &r.RecentBlockhash); err != nil {
		return BurnRecord{}, errs.Wrap(err, errs.CodeInternal, "stableasset: stored burn request does not decode")
	}
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	return r, nil
}

func insertMint(ctx context.Context, tx pgx.Tx, r MintRecord, actor econ.Actor, now time.Time) error {
	plan, err := json.Marshal(r.Plan)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "stableasset: encode plan")
	}
	signing, err := json.Marshal(r.SigningPlan)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "stableasset: encode signing plan")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stable_mint_requests
		(id, stable_asset_id, account_id, intent_id, effect_id, mode, quantity, reserve_asset_id, reserve_quantity,
		 reservation_id, recipient, plan, plan_hash, signing_plan, chain_id, tx_id, signed_tx, recent_blockhash, status,
		 created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9::numeric,$10,$11,$12,$13,$14,$15,$16,$17,$18,'SUBMITTING',$19,$19)`,
		r.ID, r.StableAssetID, r.AccountID, r.IntentID, r.EffectID, r.Mode, r.Quantity.String(), r.ReserveAssetID,
		r.ReserveQuantity.String(), r.ReservationID, r.Recipient.String(), plan, r.PlanHash[:], signing, string(r.Chain),
		string(r.TxID), r.SignedTx, r.RecentBlockhash.String(), stamp(now)); err != nil {
		return dbErr("record mint request", err)
	}
	return birth(ctx, tx, "stable_mint_request_transitions", r.ID.String(), string(MintSubmitting), actor)
}

func insertBurn(ctx context.Context, tx pgx.Tx, r BurnRecord, actor econ.Actor, now time.Time) error {
	plan, err := json.Marshal(r.Plan)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "stableasset: encode plan")
	}
	signing, err := json.Marshal(r.SigningPlan)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "stableasset: encode signing plan")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stable_burn_requests
		(id, stable_asset_id, account_id, intent_id, effect_id, mode, quantity, payout_asset_id, payout_quantity,
		 reservation_id, holder, plan, plan_hash, signing_plan, chain_id, tx_id, signed_tx, recent_blockhash, status,
		 created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9::numeric,$10,$11,$12,$13,$14,$15,$16,$17,$18,'SUBMITTING',$19,$19)`,
		r.ID, r.StableAssetID, r.AccountID, r.IntentID, r.EffectID, r.Mode, r.Quantity.String(), r.PayoutAssetID,
		r.PayoutQuantity.String(), r.ReservationID, r.Holder.String(), plan, r.PlanHash[:], signing, string(r.Chain),
		string(r.TxID), r.SignedTx, r.RecentBlockhash.String(), stamp(now)); err != nil {
		return dbErr("record burn request", err)
	}
	return birth(ctx, tx, "stable_burn_request_transitions", r.ID.String(), string(BurnSubmitting), actor)
}

// birth writes the NONE -> first-status row, so every request has a complete
// transition history from its creation.
func birth(ctx context.Context, tx pgx.Tx, table, requestID, status string, actor econ.Actor) error {
	if _, err := tx.Exec(ctx, `INSERT INTO `+table+` (id, request_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,'NONE',$3,$4,$5,'requested')`, id.New[id.Any](), requestID, status, actor.Type, actor.ID); err != nil {
		return dbErr("record request birth", err)
	}
	return nil
}

func getMint(ctx context.Context, q db.Querier, rid MintRequestID, forUpdate bool) (MintRecord, error) {
	sql := `SELECT ` + mintColumns + ` FROM stable_mint_requests WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	r, err := scanMint(q.QueryRow(ctx, sql, rid))
	if errors.Is(err, pgx.ErrNoRows) {
		return MintRecord{}, errs.New(errs.CodeNotFound, "mint request not found").WithField("request_id", rid.String())
	}
	if err != nil {
		return MintRecord{}, dbErr("load mint request", err)
	}
	return r, nil
}

func getBurn(ctx context.Context, q db.Querier, rid BurnRequestID, forUpdate bool) (BurnRecord, error) {
	sql := `SELECT ` + burnColumns + ` FROM stable_burn_requests WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	r, err := scanBurn(q.QueryRow(ctx, sql, rid))
	if errors.Is(err, pgx.ErrNoRows) {
		return BurnRecord{}, errs.New(errs.CodeNotFound, "burn request not found").WithField("request_id", rid.String())
	}
	if err != nil {
		return BurnRecord{}, dbErr("load burn request", err)
	}
	return r, nil
}

func mintByIntent(ctx context.Context, q db.Querier, iid econ.IntentID) (MintRecord, bool, error) {
	r, err := scanMint(q.QueryRow(ctx, `SELECT `+mintColumns+` FROM stable_mint_requests WHERE intent_id = $1`, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return MintRecord{}, false, nil
	}
	if err != nil {
		return MintRecord{}, false, dbErr("find mint request", err)
	}
	return r, true, nil
}

func burnByIntent(ctx context.Context, q db.Querier, iid econ.IntentID) (BurnRecord, bool, error) {
	r, err := scanBurn(q.QueryRow(ctx, `SELECT `+burnColumns+` FROM stable_burn_requests WHERE intent_id = $1`, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return BurnRecord{}, false, nil
	}
	if err != nil {
		return BurnRecord{}, false, dbErr("find burn request", err)
	}
	return r, true, nil
}

// GetMint returns a mint request.
func (s *Service) GetMint(ctx context.Context, q db.Querier, rid MintRequestID) (MintRecord, error) {
	return getMint(ctx, q, rid, false)
}

// GetBurn returns a burn request.
func (s *Service) GetBurn(ctx context.Context, q db.Querier, rid BurnRequestID) (BurnRecord, error) {
	return getBurn(ctx, q, rid, false)
}

func listMints(ctx context.Context, q db.Querier, where string, args ...any) ([]MintRecord, error) {
	rows, err := q.Query(ctx, `SELECT `+mintColumns+` FROM stable_mint_requests WHERE `+where, args...)
	if err != nil {
		return nil, dbErr("list mint requests", err)
	}
	defer rows.Close()
	var out []MintRecord
	for rows.Next() {
		r, err := scanMint(rows)
		if err != nil {
			return nil, dbErr("scan mint request", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func listBurns(ctx context.Context, q db.Querier, where string, args ...any) ([]BurnRecord, error) {
	rows, err := q.Query(ctx, `SELECT `+burnColumns+` FROM stable_burn_requests WHERE `+where, args...)
	if err != nil {
		return nil, dbErr("list burn requests", err)
	}
	defer rows.Close()
	var out []BurnRecord
	for rows.Next() {
		r, err := scanBurn(rows)
		if err != nil {
			return nil, dbErr("scan burn request", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// openMints returns the asset's mints whose supply is reserved but not final.
func openMints(ctx context.Context, q db.Querier, sid StableAssetID) ([]MintRecord, error) {
	return listMints(ctx, q, `stable_asset_id = $1 AND status IN ('SUBMITTING','SUBMITTED','SUBMISSION_UNKNOWN') ORDER BY created_at, id`, sid)
}

// openBurns returns the asset's burns whose redemption is not posted yet.
func openBurns(ctx context.Context, q db.Querier, sid StableAssetID) ([]BurnRecord, error) {
	return listBurns(ctx, q, `stable_asset_id = $1 AND status IN ('SUBMITTING','SUBMITTED','SUBMISSION_UNKNOWN','REDEMPTION_QUEUED') ORDER BY created_at, id`, sid)
}

// requestTransitionColumns and requestStatusChange are the parts of a request
// transition's two statements that do not name the table.
const (
	requestTransitionColumns = `(id, request_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`
	requestStatusChange = `SET status = $2, failure_code = coalesce(NULLIF($3, ''), failure_code),
		journal_transaction_id = coalesce($4, journal_transaction_id), final_slot = coalesce($5, final_slot)
		WHERE id = $1 AND status = $6`
)

// transition is one bound status change of a request row.
type transition struct {
	table, transitions, id, from, to, reason, failureCode string
	journal                                               *ledger.TransactionID
	finalSlot                                             *int64
	actor                                                 econ.Actor
}

// apply writes the transition row (which licenses the change, migration
// 00726) and the change itself; the UPDATE is conditional on the status the
// caller locked, so a concurrent change is a CONFLICT, never a lost update.
func (t transition) apply(ctx context.Context, tx pgx.Tx) error {
	if strings.TrimSpace(t.reason) == "" {
		t.reason = t.to
	}
	// Each table's statements are spelled out in full so that every one is a
	// compile-time constant (test/security/sqlsource_test.go).
	var insert, update string
	switch {
	case t.table == "stable_mint_requests" && t.transitions == "stable_mint_request_transitions":
		insert = `INSERT INTO stable_mint_request_transitions ` + requestTransitionColumns
		update = `UPDATE stable_mint_requests ` + requestStatusChange
	case t.table == "stable_burn_requests" && t.transitions == "stable_burn_request_transitions":
		insert = `INSERT INTO stable_burn_request_transitions ` + requestTransitionColumns
		update = `UPDATE stable_burn_requests ` + requestStatusChange
	default:
		return errs.Newf(errs.CodeInternal, "stableasset: no request table %q", t.table)
	}
	if _, err := tx.Exec(ctx, insert, id.New[id.Any](), t.id, t.from, t.to, t.actor.Type, t.actor.ID, t.reason); err != nil {
		return dbErr("record request transition", err)
	}
	tag, err := tx.Exec(ctx, update, t.id, t.to, t.failureCode, t.journal, t.finalSlot, t.from)
	if err != nil {
		return dbErr("update request status", err)
	}
	if tag.RowsAffected() != 1 {
		return errs.New(errs.CodeConflict, "stableasset: the request changed concurrently").WithField("request_id", t.id)
	}
	return nil
}

func (s *Service) transitionMint(ctx context.Context, tx pgx.Tx, r MintRecord, to MintStatus, actor econ.Actor, reason, failureCode string,
	journal *ledger.TransactionID, finalSlot *int64) (MintRecord, error) {
	if !CanTransitionMint(r.Status, to) {
		return MintRecord{}, errs.Newf(errs.CodeInvalidStateTransition, "mint request %s cannot move %s -> %s", r.ID, r.Status, to).
			WithField("request_id", r.ID.String())
	}
	if err := (transition{table: "stable_mint_requests", transitions: "stable_mint_request_transitions", id: r.ID.String(),
		from: string(r.Status), to: string(to), reason: reason, failureCode: failureCode, journal: journal, finalSlot: finalSlot,
		actor: actor}).apply(ctx, tx); err != nil {
		return MintRecord{}, err
	}
	if err := s.auditEvent(ctx, tx, actor, "stableasset.mint."+strings.ToLower(string(to)), "stable_mint_request", r.ID.String(),
		r.AccountID.String(), reason, map[string]string{"from": string(r.Status), "to": string(to), "tx_id": string(r.TxID), "failure_code": failureCode}); err != nil {
		return MintRecord{}, err
	}
	return getMint(ctx, tx, r.ID, false)
}

func (s *Service) transitionBurn(ctx context.Context, tx pgx.Tx, r BurnRecord, to BurnStatus, actor econ.Actor, reason, failureCode string,
	journal *ledger.TransactionID, finalSlot *int64) (BurnRecord, error) {
	if !CanTransitionBurn(r.Status, to) {
		return BurnRecord{}, errs.Newf(errs.CodeInvalidStateTransition, "burn request %s cannot move %s -> %s", r.ID, r.Status, to).
			WithField("request_id", r.ID.String())
	}
	if err := (transition{table: "stable_burn_requests", transitions: "stable_burn_request_transitions", id: r.ID.String(),
		from: string(r.Status), to: string(to), reason: reason, failureCode: failureCode, journal: journal, finalSlot: finalSlot,
		actor: actor}).apply(ctx, tx); err != nil {
		return BurnRecord{}, err
	}
	if err := s.auditEvent(ctx, tx, actor, "stableasset.burn."+strings.ToLower(string(to)), "stable_burn_request", r.ID.String(),
		r.AccountID.String(), reason, map[string]string{"from": string(r.Status), "to": string(to), "tx_id": string(r.TxID), "failure_code": failureCode}); err != nil {
		return BurnRecord{}, err
	}
	return getBurn(ctx, tx, r.ID, false)
}
