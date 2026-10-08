package matching

import (
	"sync"

	"github.com/nodal/controlplane/internal/errs"
)

// Sequencer is the thread-safe front door of one Book. Any number of
// goroutines may Submit concurrently; a mutex makes each Submit apply its
// command and journal it as one indivisible step, so the order in which
// Submits acquire the lock is the total order of the book, the journal holds
// exactly that order, and replaying the journal reproduces every Result that
// Submit returned, event for event.
type Sequencer struct {
	mu      sync.Mutex
	book    *Book
	journal Journal
}

// NewSequencer puts a Sequencer in front of b, which may be new or restored
// from a snapshot; its journal starts with the next command. The Sequencer
// owns b from now on: the caller must not use b directly.
func NewSequencer(b *Book) (*Sequencer, error) {
	if b == nil {
		return nil, errs.New(errs.CodeValidationFailed, "matching: sequencer needs a book")
	}
	return &Sequencer{book: b}, nil
}

// Submit applies cmd and journals it. A command that Apply refuses with an
// error (not a command at all) changes nothing and is not journaled.
func (s *Sequencer) Submit(cmd Command) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.book.Apply(cmd)
	if err != nil {
		return Result{}, err
	}
	s.journal.entries = append(s.journal.entries, JournalEntry{Seq: res.CommandSeq, Command: cmd})
	return res, nil
}

// Journal returns a copy of every entry journaled by this Sequencer.
func (s *Sequencer) Journal() []JournalEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.Entries()
}

// JournalSince returns a copy of the entries after CommandSeq seq.
func (s *Sequencer) JournalSince(seq int64) []JournalEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.Since(seq)
}

// Read runs fn with a read-only view of the book while holding the lock, so
// everything fn reads is one consistent state between two commands -- for
// example a Snapshot together with its CommandSeq. fn must not retain the
// Reader or call back into the Sequencer.
func (s *Sequencer) Read(fn func(Reader)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(readView{s.book})
}
