package matching

import (
	"slices"

	"github.com/nodal/controlplane/internal/errs"
)

// JournalEntry is one sequenced command: Seq is the CommandSeq the book
// assigned when it applied the command.
type JournalEntry struct {
	Seq     int64
	Command Command
}

// Journal is an append-only, gap-free sequence of journal entries: each
// entry's Seq is one more than the previous one's. The first entry may start
// anywhere at or above 1, so a journal can also hold the tail that follows a
// snapshot. The zero value is an empty journal. A Journal is not safe for
// concurrent use; the Sequencer keeps its own under its lock.
type Journal struct {
	entries []JournalEntry
}

// Append adds an entry, which must continue the sequence.
func (j *Journal) Append(e JournalEntry) error {
	if e.Command == nil {
		return errs.New(errs.CodeValidationFailed, "matching: journal entry has no command")
	}
	if n := len(j.entries); n > 0 && e.Seq != j.entries[n-1].Seq+1 {
		return errs.New(errs.CodeConflict, "matching: journal entry does not continue the sequence").
			WithField("expected", j.entries[n-1].Seq+1).WithField("got", e.Seq)
	}
	if e.Seq < 1 {
		return errs.New(errs.CodeValidationFailed, "matching: journal sequence starts at 1").WithField("got", e.Seq)
	}
	j.entries = append(j.entries, e)
	return nil
}

// Len returns the number of entries.
func (j *Journal) Len() int { return len(j.entries) }

// Entries returns a copy of all entries.
func (j *Journal) Entries() []JournalEntry { return slices.Clone(j.entries) }

// Since returns a copy of the entries whose Seq is greater than seq: the
// tail to apply to a book restored from a snapshot taken at CommandSeq seq.
func (j *Journal) Since(seq int64) []JournalEntry {
	i, _ := slices.BinarySearchFunc(j.entries, seq+1, func(e JournalEntry, s int64) int {
		switch {
		case e.Seq < s:
			return -1
		case e.Seq > s:
			return 1
		}
		return 0
	})
	return slices.Clone(j.entries[i:])
}

// Replay applies a journal to a new book and returns the book and the result
// of every entry, which are identical to the results the commands produced
// when they were first applied. The first entry must have Seq 1.
func Replay(cfg Config, entries []JournalEntry) (*Book, []Result, error) {
	b, err := NewBook(cfg)
	if err != nil {
		return nil, nil, err
	}
	results, err := b.Replay(entries)
	if err != nil {
		return nil, nil, err
	}
	return b, results, nil
}

// Replay applies journal entries that continue this book's sequence: the
// first entry's Seq must be CommandSeq()+1 and each next one one more. It
// returns the results in entry order. On error the book holds the state
// after the last entry applied, and the results so far are returned.
func (b *Book) Replay(entries []JournalEntry) ([]Result, error) {
	results := make([]Result, 0, len(entries))
	for _, e := range entries {
		if e.Seq != b.cmdSeq+1 {
			return results, errs.New(errs.CodeConflict, "matching: journal entry does not continue the book's sequence").
				WithField("expected", b.cmdSeq+1).WithField("got", e.Seq)
		}
		res, err := b.Apply(e.Command)
		if err != nil {
			return results, errs.Wrap(err, errs.CodeValidationFailed, "matching: journal entry is not a command").
				WithField("seq", e.Seq)
		}
		results = append(results, res)
	}
	return results, nil
}
