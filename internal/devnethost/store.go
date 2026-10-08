package devnethost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// Store is the append-only event log behind a Host.
type Store interface {
	// Since returns the committed events of network with Seq > after, in
	// ascending order. When q is not nil the read runs on q -- the caller's
	// own transaction -- so a read made inside a transaction never needs a
	// second connection from the pool the transaction came from (F-27).
	Since(ctx context.Context, q db.Querier, network string, after uint64) ([]Event, error)
	// Exclusive runs fn while holding the network's exclusive write lock (a
	// lock every process writing the network honours). fn receives the
	// events committed since after, and returns the event to append (nil for
	// none). The append and the lock end together; when Exclusive returns an
	// error nothing fn returned was appended. Exclusive never re-runs fn.
	Exclusive(ctx context.Context, network string, after uint64, fn func(missed []Event) (*Event, error)) error
}

// PgStore is the Store over the devnet_events table (migration 00747). Each
// Exclusive call is one short READ COMMITTED transaction that takes a
// transaction-scoped advisory lock on the network, so every process that
// shares the database appends to one gapless, totally ordered history.
type PgStore struct {
	db *db.DB
}

// NewPgStore returns a PgStore.
func NewPgStore(d *db.DB) *PgStore {
	if d == nil {
		panic("devnethost: NewPgStore requires a database")
	}
	return &PgStore{db: d}
}

const eventColumns = `network, seq::text, kind, coalesce(chain_id, ''), args, raw_tx, coalesce(tx_id, ''), outcome,
	state_hash, prev_hash, event_hash, occurred_at`

func scanEvent(row pgx.Row) (Event, error) {
	var e Event
	var seq, chain, txID string
	var args []byte
	var stateHash, prevHash, eventHash []byte
	if err := row.Scan(&e.Network, &seq, &e.Kind, &chain, &args, &e.RawTx, &txID, &e.Outcome,
		&stateHash, &prevHash, &eventHash, &e.OccurredAt); err != nil {
		return Event{}, err
	}
	n, err := strconv.ParseUint(seq, 10, 64)
	if err != nil {
		return Event{}, diverged("stored seq is not an unsigned integer")
	}
	if len(stateHash) != devnet.HashSize || len(prevHash) != 32 || len(eventHash) != 32 {
		return Event{}, diverged("stored hash has the wrong length")
	}
	e.Seq = n
	e.Chain = devnet.ChainID(chain)
	e.TxID = devnet.TxID(txID)
	// jsonb normalises key order and spacing; re-canonicalise so the hash
	// covers exactly what was hashed when the event was written.
	if e.Args, err = canonicalArgs(json.RawMessage(args)); err != nil {
		return Event{}, err
	}
	copy(e.StateHash[:], stateHash)
	copy(e.PrevHash[:], prevHash)
	copy(e.EventHash[:], eventHash)
	e.OccurredAt = e.OccurredAt.UTC()
	return e, nil
}

func querySince(ctx context.Context, q db.Querier, network string, after uint64) ([]Event, error) {
	rows, err := q.Query(ctx, `SELECT `+eventColumns+` FROM devnet_events
		WHERE network = $1 AND devnet_events.seq > $2::numeric
		ORDER BY devnet_events.seq`, network, strconv.FormatUint(after, 10)) // qualified: the select list's seq::text shadows the bare name
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Since implements Store.
func (s *PgStore) Since(ctx context.Context, q db.Querier, network string, after uint64) ([]Event, error) {
	if q == nil {
		q = s.db
	}
	evs, err := querySince(ctx, q, network, after)
	if err != nil {
		return nil, storeUnavailable(err, "read events")
	}
	return evs, nil
}

// Exclusive implements Store.
func (s *PgStore) Exclusive(ctx context.Context, network string, after uint64, fn func(missed []Event) (*Event, error)) error {
	// MaxRetries 0: fn changes the caller's in-memory network, so it must
	// run at most once per call.
	err := s.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "devnethost/"+network); err != nil {
			return err
		}
		missed, err := querySince(ctx, tx, network, after)
		if err != nil {
			return err
		}
		ev, err := fn(missed)
		if err != nil || ev == nil {
			return err
		}
		return insertEvent(ctx, tx, *ev)
	})
	if err != nil {
		return storeUnavailable(err, "append event")
	}
	return nil
}

// insertEvent appends one event. The event's position (network, seq) is its
// idempotency key: writing the identical event again is a no-op, and a
// different event at a taken position is refused.
func insertEvent(ctx context.Context, tx pgx.Tx, e Event) error {
	var chain, txID any
	if e.Chain != "" {
		chain = string(e.Chain)
	}
	if e.TxID != "" {
		txID = string(e.TxID)
	}
	var raw any
	if len(e.RawTx) > 0 {
		raw = e.RawTx
	}
	tag, err := tx.Exec(ctx, `INSERT INTO devnet_events
		(id, network, seq, kind, chain_id, args, raw_tx, tx_id, outcome, state_hash, prev_hash, event_hash, occurred_at)
		VALUES ($1,$2,$3::numeric,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (network, seq) DO NOTHING`,
		id.New[id.Any](), e.Network, strconv.FormatUint(e.Seq, 10), string(e.Kind), chain, []byte(e.Args), raw, txID,
		string(e.Outcome), e.StateHash[:], e.PrevHash[:], e.EventHash[:], e.OccurredAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT event_hash FROM devnet_events WHERE network = $1 AND seq = $2::numeric`,
		e.Network, strconv.FormatUint(e.Seq, 10)).Scan(&stored); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errs.New(errs.CodeInternal, "devnethost: conflict on an event position with no visible row")
		}
		return err
	}
	if !bytes.Equal(stored, e.EventHash[:]) {
		return errs.New(errs.CodeConflict, "devnethost: another writer already appended a different event at this position").
			WithField("network", e.Network).WithField("seq", strconv.FormatUint(e.Seq, 10)).
			WithField(reasonField, ReasonReplayDiverged)
	}
	return nil
}
