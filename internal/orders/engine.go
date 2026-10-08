package orders

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/matching"
)

// The engines are a cache of the journal, never the truth. A command first
// takes the book's row lock, which serializes it against every other command
// in every process; then it compares the cached engine with the cursor it
// just read (command sequence and state hash). Anything other than an exact
// match -- a fresh process, a transaction that applied a command in memory and
// then rolled back, another process that advanced the book -- discards the
// cached engine and rebuilds it from the latest snapshot and the tail of the
// journal, and the rebuilt state must hash to the hash the journal recorded.
// Memory is never trusted over the journal.

type engineEntry struct {
	mu   sync.Mutex
	book *matching.Book
	seq  int64
	hash [32]byte
}

type engineCache struct {
	mu      sync.Mutex
	entries map[BookID]*engineEntry
}

func newEngineCache() *engineCache { return &engineCache{entries: map[BookID]*engineEntry{}} }

func (c *engineCache) entry(b BookID) *engineEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[b]
	if !ok {
		e = &engineEntry{}
		c.entries[b] = e
	}
	return e
}

// Forget drops the cached engine of a book, so that the next command rebuilds
// it from the journal. It is what an operator (or a test) does to a book whose
// in-process state is suspect; it is never needed for correctness, because
// every command checks the cache against the journal anyway.
func (s *Service) Forget(bookID BookID) {
	e := s.engines.entry(bookID)
	e.mu.Lock()
	e.book = nil
	e.mu.Unlock()
}

// acquireEngine returns the book's engine, locked, consistent with the cursor
// read under the book's row lock. The caller must hold that row lock and must
// call release when its in-memory work is done.
func (s *Service) acquireEngine(ctx context.Context, q db.Querier, c *bookConfig, cur bookCursor) (*engineEntry, error) {
	e := s.engines.entry(c.ID)
	e.mu.Lock()
	if e.book != nil && e.seq == cur.LastCommandSeq && e.hash == cur.StateHash {
		return e, nil
	}
	b, err := rebuild(ctx, q, c, cur, false)
	if err != nil {
		e.book = nil
		e.mu.Unlock()
		return nil, err
	}
	e.book, e.seq, e.hash = b, cur.LastCommandSeq, cur.StateHash
	return e, nil
}

func (e *engineEntry) release() { e.mu.Unlock() }

// invalidate marks the cached engine unusable after a failure part-way
// through a command: whatever the caller does with its transaction, the next
// command rebuilds from the journal.
func (e *engineEntry) invalidate() { e.book = nil }

type snapshotRow struct {
	seq   int64
	bytes []byte
	hash  []byte
}

// rebuild restores a book from its latest snapshot at or before the cursor and
// replays the journal after it. With verifyEach it also checks the state hash
// after every replayed command against the one the journal recorded. The
// final state must hash to the cursor's state hash.
func rebuild(ctx context.Context, q db.Querier, c *bookConfig, cur bookCursor, verifyEach bool) (*matching.Book, error) {
	var snap snapshotRow
	err := q.QueryRow(ctx, `SELECT command_seq, snapshot, state_hash FROM book_snapshots
		WHERE book_id = $1 AND command_seq <= $2 ORDER BY command_seq DESC LIMIT 1`, c.ID, cur.LastCommandSeq).
		Scan(&snap.seq, &snap.bytes, &snap.hash)
	var b *matching.Book
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		b, err = matching.NewBook(c.engineConfig())
		if err != nil {
			return nil, err
		}
		if b.StateHash() != c.GenesisHash {
			return nil, errs.New(errs.CodeInternal, "orders: the empty book does not hash to the book's genesis hash")
		}
	case err != nil:
		return nil, dbErr("read snapshot", err)
	default:
		sum := sha256.Sum256(snap.bytes)
		if !bytes.Equal(sum[:], snap.hash) {
			return nil, errs.New(errs.CodeInternal, "orders: a stored snapshot does not match its hash")
		}
		b, err = matching.Restore(snap.bytes)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "orders: a stored snapshot does not restore")
		}
		if b.CommandSeq() != snap.seq || b.Config() != c.engineConfig() {
			return nil, errs.New(errs.CodeInternal, "orders: a stored snapshot is not this book at its sequence")
		}
	}
	if err := replayTail(ctx, q, c, b, cur.LastCommandSeq, verifyEach); err != nil {
		return nil, err
	}
	if b.StateHash() != cur.StateHash {
		return nil, errs.New(errs.CodeInternal, "orders: replaying the journal does not reproduce the recorded state hash").
			WithField("book_id", c.ID.String()).
			WithField("command_seq", cur.LastCommandSeq)
	}
	return b, nil
}

// replayTail applies the journalled commands after the book's current
// sequence up to and including last.
func replayTail(ctx context.Context, q db.Querier, c *bookConfig, b *matching.Book, last int64, verifyEach bool) error {
	rows, err := q.Query(ctx, `SELECT seq, command, command_sha256, state_hash_after FROM book_commands
		WHERE book_id = $1 AND seq > $2 AND seq <= $3 ORDER BY seq`, c.ID, b.CommandSeq(), last)
	if err != nil {
		return dbErr("read journal", err)
	}
	type entry struct {
		seq       int64
		cmd       []byte
		cmdHash   []byte
		stateHash []byte
	}
	var tail []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.seq, &e.cmd, &e.cmdHash, &e.stateHash); err != nil {
			rows.Close()
			return dbErr("scan journal", err)
		}
		tail = append(tail, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return dbErr("read journal", err)
	}
	for _, e := range tail {
		if e.seq != b.CommandSeq()+1 {
			return errs.New(errs.CodeInternal, "orders: the journal has a gap").WithField("seq", e.seq)
		}
		if !bytes.Equal(sha(e.cmd), e.cmdHash) {
			return errs.New(errs.CodeInternal, "orders: a journalled command does not match its hash").WithField("seq", e.seq)
		}
		cmd, err := DecodeCommand(e.cmd)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "orders: a journalled command does not decode").WithField("seq", e.seq)
		}
		res, err := b.Apply(cmd)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "orders: a journalled command does not apply").WithField("seq", e.seq)
		}
		if res.CommandSeq != e.seq {
			return errs.New(errs.CodeInternal, "orders: replay assigned a different sequence").WithField("seq", e.seq)
		}
		if verifyEach {
			h := b.StateHash()
			if !bytes.Equal(h[:], e.stateHash) {
				return errs.New(errs.CodeInternal, "orders: replay diverged from the journal").WithField("seq", e.seq)
			}
		}
	}
	if b.CommandSeq() != last {
		return errs.New(errs.CodeInternal, "orders: the journal ends before the book's cursor").WithField("want", last)
	}
	return nil
}
