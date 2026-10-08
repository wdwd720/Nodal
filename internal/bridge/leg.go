package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
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

// Leg is a bridge_legs row: one movement of capital from a route's source
// chain to its destination.
type Leg struct {
	ID                  LegID
	RouteID             RouteID
	AccountID           accounts.AccountID
	IntentID            econ.IntentID
	Mode                gates.Mode
	Quantity            money.Quantity
	SourceOwner         devnet.PublicKey
	DestinationOwner    devnet.PublicKey
	Nonce               uint64
	Plan                BridgePlan
	PlanHash            [32]byte
	Status              CrossChainCapitalState
	ReservationID       *capital.ReservationID
	SourceEffectID      *econ.EffectID
	DestinationEffectID *econ.EffectID
	RefundEffectID      *econ.EffectID
	LockTxID            devnet.TxID
	LockTx              []byte
	LockBlockhash       devnet.Hash
	LockBlockhashHeight uint64
	SourceJournal       *ledger.TransactionID
	DestinationJournal  *ledger.TransactionID
	RefundJournal       *ledger.TransactionID
	FailureCode         string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Holding says which ledger account holds the leg's value now.
func (l Leg) Holding() LedgerHolding {
	switch {
	case l.RefundJournal != nil:
		return HoldingNone
	case l.DestinationJournal != nil:
		return HoldingDestination
	case l.SourceJournal != nil:
		return HoldingInFlight
	case l.Status == StateFailed:
		return HoldingNone
	}
	return HoldingSourceReserved
}

func (l Leg) ref() LegRef {
	return LegRef{LegID: l.ID, AccountID: l.AccountID, Quantity: l.Quantity, SourceOwner: l.SourceOwner,
		DestinationOwner: l.DestinationOwner, Nonce: l.Nonce, LockTxID: l.LockTxID}
}

const legColumns = `id, route_id, account_id, intent_id, mode, quantity::text, source_owner, destination_owner,
	lock_nonce::text, plan, plan_hash, status, reservation_id, source_effect_id, destination_effect_id, refund_effect_id,
	coalesce(lock_tx_id, ''), lock_tx, coalesce(lock_blockhash, ''), coalesce(lock_blockhash_height, 0),
	source_journal_transaction_id, destination_journal_transaction_id, refund_journal_transaction_id,
	coalesce(failure_code, ''), created_at, updated_at`

func scanLeg(row pgx.Row) (Leg, error) {
	var l Leg
	var qty, srcOwner, dstOwner, nonce, lockTxID, lockBH string
	var plan, planHash []byte
	var bhHeight int64
	if err := row.Scan(&l.ID, &l.RouteID, &l.AccountID, &l.IntentID, &l.Mode, &qty, &srcOwner, &dstOwner, &nonce, &plan,
		&planHash, &l.Status, &l.ReservationID, &l.SourceEffectID, &l.DestinationEffectID, &l.RefundEffectID, &lockTxID,
		&l.LockTx, &lockBH, &bhHeight, &l.SourceJournal, &l.DestinationJournal, &l.RefundJournal, &l.FailureCode,
		&l.CreatedAt, &l.UpdatedAt); err != nil {
		return Leg{}, err
	}
	var err error
	if l.Quantity, err = money.ScanQuantity(qty); err != nil {
		return Leg{}, err
	}
	if l.SourceOwner, err = devnet.ParsePublicKey(srcOwner); err != nil {
		return Leg{}, err
	}
	if l.DestinationOwner, err = devnet.ParsePublicKey(dstOwner); err != nil {
		return Leg{}, err
	}
	if l.Nonce, err = strconv.ParseUint(nonce, 10, 64); err != nil {
		return Leg{}, err
	}
	if err := json.Unmarshal(plan, &l.Plan); err != nil {
		return Leg{}, err
	}
	if len(planHash) != 32 {
		return Leg{}, errs.New(errs.CodeInternal, "bridge: stored plan hash has the wrong length")
	}
	copy(l.PlanHash[:], planHash)
	l.LockTxID = devnet.TxID(lockTxID)
	if lockBH != "" {
		if l.LockBlockhash, err = devnet.ParseHash(lockBH); err != nil {
			return Leg{}, err
		}
	}
	if bhHeight < 0 {
		return Leg{}, errs.New(errs.CodeInternal, "bridge: stored blockhash height is negative")
	}
	l.LockBlockhashHeight = uint64(bhHeight)
	l.CreatedAt, l.UpdatedAt = l.CreatedAt.UTC(), l.UpdatedAt.UTC()
	return l, nil
}

func insertLeg(ctx context.Context, tx pgx.Tx, l Leg, actor econ.Actor, now time.Time) error {
	plan, err := json.Marshal(l.Plan)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "bridge: encode plan")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bridge_legs (id, route_id, account_id, intent_id, mode, quantity, source_owner,
		destination_owner, lock_nonce, plan, plan_hash, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8,$9::numeric,$10,$11,'AVAILABLE',$12,$12)`,
		l.ID, l.RouteID, l.AccountID, l.IntentID, l.Mode, l.Quantity.String(), l.SourceOwner.String(),
		l.DestinationOwner.String(), strconv.FormatUint(l.Nonce, 10), plan, l.PlanHash[:], now.UTC().Truncate(time.Microsecond)); err != nil {
		return dbErr("record leg", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bridge_leg_transitions (id, leg_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,'NONE','AVAILABLE',$3,$4,'requested')`, id.New[id.Any](), l.ID, actor.Type, actor.ID); err != nil {
		return dbErr("record leg birth", err)
	}
	return nil
}

func getLeg(ctx context.Context, q db.Querier, lid LegID, forUpdate bool) (Leg, error) {
	sql := `SELECT ` + legColumns + ` FROM bridge_legs WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	l, err := scanLeg(q.QueryRow(ctx, sql, lid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Leg{}, errs.New(errs.CodeNotFound, "bridge leg not found").WithField("leg_id", lid.String())
	}
	if err != nil {
		return Leg{}, dbErr("load leg", err)
	}
	return l, nil
}

func legByIntent(ctx context.Context, q db.Querier, iid econ.IntentID) (Leg, bool, error) {
	l, err := scanLeg(q.QueryRow(ctx, `SELECT `+legColumns+` FROM bridge_legs WHERE intent_id = $1`, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Leg{}, false, nil
	}
	if err != nil {
		return Leg{}, false, dbErr("find leg", err)
	}
	return l, true, nil
}

func listLegs(ctx context.Context, q db.Querier, where string, args ...any) ([]Leg, error) {
	rows, err := q.Query(ctx, `SELECT `+legColumns+` FROM bridge_legs WHERE `+where, args...)
	if err != nil {
		return nil, dbErr("list legs", err)
	}
	defer rows.Close()
	var out []Leg
	for rows.Next() {
		l, err := scanLeg(rows)
		if err != nil {
			return nil, dbErr("scan leg", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetLeg returns a leg.
func (s *Service) GetLeg(ctx context.Context, q db.Querier, lid LegID) (Leg, error) {
	return getLeg(ctx, q, lid, false)
}

// legSet are the set-once columns a transition may fill in.
type legSet struct {
	reservation         *capital.ReservationID
	sourceEffect        *econ.EffectID
	destinationEffect   *econ.EffectID
	refundEffect        *econ.EffectID
	lockTxID            devnet.TxID
	lockTx              []byte
	lockBlockhash       *devnet.Hash
	lockBlockhashHeight *uint64
	sourceJournal       *ledger.TransactionID
	destinationJournal  *ledger.TransactionID
	refundJournal       *ledger.TransactionID
	failureCode         string
}

// updateLeg writes the set-once columns and, when to is not empty, the new
// status, in ONE statement: the row's CHECKs (a FAILED leg has a code, a
// refunded leg is FAILED, a source-finalized leg names its journal) hold per
// statement, so the status and the columns that justify it change together.
// The update is conditional on the status the caller locked.
func (s *Service) updateLeg(ctx context.Context, tx pgx.Tx, l Leg, to CrossChainCapitalState, set legSet) error {
	var bh, height any
	if set.lockBlockhash != nil {
		bh = set.lockBlockhash.String()
	}
	if set.lockBlockhashHeight != nil {
		height = int64(*set.lockBlockhashHeight) // #nosec G115 -- a block height is far below 2^63
	}
	var lockTx any
	if len(set.lockTx) > 0 {
		lockTx = set.lockTx
	}
	status := l.Status
	if to != "" {
		status = to
	}
	tag, err := tx.Exec(ctx, `UPDATE bridge_legs SET status = $14,
		reservation_id = coalesce($2, reservation_id), source_effect_id = coalesce($3, source_effect_id),
		destination_effect_id = coalesce($4, destination_effect_id), refund_effect_id = coalesce($5, refund_effect_id),
		lock_tx_id = coalesce(NULLIF($6, ''), lock_tx_id), lock_tx = coalesce($7, lock_tx),
		lock_blockhash = coalesce($8, lock_blockhash), lock_blockhash_height = coalesce($9, lock_blockhash_height),
		source_journal_transaction_id = coalesce($10, source_journal_transaction_id),
		destination_journal_transaction_id = coalesce($11, destination_journal_transaction_id),
		refund_journal_transaction_id = coalesce($12, refund_journal_transaction_id),
		failure_code = coalesce(NULLIF($13, ''), failure_code)
		WHERE id = $1 AND status = $15`, l.ID, set.reservation, set.sourceEffect, set.destinationEffect, set.refundEffect,
		string(set.lockTxID), lockTx, bh, height, set.sourceJournal, set.destinationJournal, set.refundJournal, set.failureCode,
		status, l.Status)
	if err != nil {
		return dbErr("update leg", err)
	}
	if tag.RowsAffected() != 1 {
		return errs.New(errs.CodeConflict, "bridge: the leg changed concurrently").WithField("leg_id", l.ID.String())
	}
	return nil
}

// transitionLeg writes the transition row (which licenses the change), the
// set-once columns and the status, conditional on the status the caller
// locked, and the audit record.
func (s *Service) transitionLeg(ctx context.Context, tx pgx.Tx, l Leg, to CrossChainCapitalState, actor econ.Actor, reason string, set legSet) (Leg, error) {
	if !CanTransition(l.Status, to) {
		return Leg{}, errs.Newf(errs.CodeInvalidStateTransition, "bridge leg %s cannot move %s -> %s", l.ID, l.Status, to).
			WithField("leg_id", l.ID.String())
	}
	if strings.TrimSpace(reason) == "" {
		reason = string(to)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bridge_leg_transitions (id, leg_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, id.New[id.Any](), l.ID, l.Status, to, actor.Type, actor.ID, reason); err != nil {
		return Leg{}, dbErr("record leg transition", err)
	}
	if err := s.updateLeg(ctx, tx, l, to, set); err != nil {
		return Leg{}, err
	}
	if err := s.auditEvent(ctx, tx, actor, "bridge.leg."+strings.ToLower(string(to)), "bridge_leg", l.ID.String(), l.AccountID.String(),
		reason, map[string]string{"from": string(l.Status), "to": string(to)}); err != nil {
		return Leg{}, err
	}
	return getLeg(ctx, tx, l.ID, false)
}

// Attempt is a bridge_release_attempts row.
type Attempt struct {
	LegID           LegID
	Number          int
	Kind            AttemptKind
	Chain           devnet.ChainID
	TxID            devnet.TxID
	Raw             []byte
	RecentBlockhash devnet.Hash
	BlockhashHeight uint64
	CreatedAt       time.Time
}

func insertAttempt(ctx context.Context, tx pgx.Tx, a Attempt) error {
	_, err := tx.Exec(ctx, `INSERT INTO bridge_release_attempts (id, leg_id, attempt, kind, chain_id, tx_id, raw_tx,
		recent_blockhash, blockhash_height) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id.New[id.Any](), a.LegID, a.Number, a.Kind,
		string(a.Chain), string(a.TxID), a.Raw, a.RecentBlockhash.String(), int64(a.BlockhashHeight)) // #nosec G115 -- a block height
	if err != nil {
		return dbErr("record release attempt", err)
	}
	return nil
}

func attemptsOf(ctx context.Context, q db.Querier, lid LegID, kind AttemptKind) ([]Attempt, error) {
	rows, err := q.Query(ctx, `SELECT leg_id, attempt, kind, chain_id, tx_id, raw_tx, recent_blockhash, blockhash_height, created_at
		FROM bridge_release_attempts WHERE leg_id = $1 AND kind = $2 ORDER BY attempt`, lid, kind)
	if err != nil {
		return nil, dbErr("list release attempts", err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var chain, txID, bh string
		var height int64
		if err := rows.Scan(&a.LegID, &a.Number, &a.Kind, &chain, &txID, &a.Raw, &bh, &height, &a.CreatedAt); err != nil {
			return nil, dbErr("scan release attempt", err)
		}
		a.Chain, a.TxID = devnet.ChainID(chain), devnet.TxID(txID)
		if a.RecentBlockhash, err = devnet.ParseHash(bh); err != nil {
			return nil, dbErr("scan release attempt", err)
		}
		a.BlockhashHeight = uint64(height) // #nosec G115 -- CHECKed non-negative
		out = append(out, a)
	}
	return out, rows.Err()
}

func countAllAttempts(ctx context.Context, q db.Querier, lid LegID) (int, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM bridge_release_attempts WHERE leg_id = $1`, lid).Scan(&n); err != nil {
		return 0, dbErr("count release attempts", err)
	}
	return n, nil
}
