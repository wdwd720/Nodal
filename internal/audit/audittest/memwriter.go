// Package audittest provides an in-memory audit.Writer for unit tests of
// packages that emit audit events. It is never wired into production; the
// generic "<pkg>/<pkg>test" rule is enforced by scripts/lintfin.
package audittest

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
)

// Recorded is one appended event together with its chain position.
type Recorded struct {
	audit.Appended
	Event audit.Event
}

// MemWriter records events per stream and maintains a real hash chain using
// audit.HashEvent, so tests can assert on ContentHash/PrevHash exactly as the
// PostgreSQL writer would produce them. It ignores the transaction argument
// (nil is fine) and is safe for concurrent use.
type MemWriter struct {
	// BuildVersion is stamped into every hash; empty means no build version.
	BuildVersion string

	mu      sync.Mutex
	streams map[string][]Recorded
}

var _ audit.Writer = (*MemWriter)(nil)

// New returns an empty MemWriter.
func New() *MemWriter { return &MemWriter{streams: map[string][]Recorded{}} }

// Append validates e exactly like the PostgreSQL writer and links it to the
// previous event of its stream.
func (m *MemWriter) Append(_ context.Context, _ pgx.Tx, e audit.Event) (audit.Appended, error) {
	if err := e.Validate(); err != nil {
		return audit.Appended{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.streams == nil {
		m.streams = map[string][]Recorded{}
	}
	prev := m.streams[e.Stream]
	var prevHash []byte
	if len(prev) > 0 {
		prevHash = prev[len(prev)-1].ContentHash
	}
	seq := int64(len(prev)) + 1
	sum, err := audit.HashEvent(e, seq, prevHash, m.BuildVersion)
	if err != nil {
		return audit.Appended{}, err
	}
	app := audit.Appended{ID: audit.NewEventID(), Stream: e.Stream, StreamSeq: seq, ContentHash: sum, PrevHash: prevHash}
	m.streams[e.Stream] = append(prev, Recorded{Appended: app, Event: e})
	return app, nil
}

// Events returns a copy of the events recorded on stream, in order.
func (m *MemWriter) Events(stream string) []Recorded {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Recorded(nil), m.streams[stream]...)
}

// All returns every recorded event grouped by stream (copies).
func (m *MemWriter) All() map[string][]Recorded {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]Recorded, len(m.streams))
	for s, evs := range m.streams {
		out[s] = append([]Recorded(nil), evs...)
	}
	return out
}
