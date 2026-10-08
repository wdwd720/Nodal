package advancedjobs

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
)

// nilUUID orders before every id this system issues (UUIDv7), so a page that
// starts after it starts at the beginning.
const nilUUID = "00000000-0000-0000-0000-000000000000"

// keyed is a unit a paged job reads in id order.
type keyed interface {
	// cursorKey is the unit's id in canonical text form.
	cursorKey() string
}

// pager remembers where a paged job's last run stopped, so a batch smaller
// than the population still reaches every unit in turn instead of revisiting
// the lowest ids forever. It lives in the job value: a restarted worker begins
// again at the start, which repeats some units and skips none, and every
// operation a paged job drives is safe to repeat.
type pager struct {
	mu    sync.Mutex
	after string
}

// readPage reads up to limit units after the pager's position, read-only,
// and moves the position. A short page means the population has been walked:
// the next run starts over. An empty page part-way through starts over at
// once rather than spending a run on nothing.
func readPage[T keyed](ctx context.Context, p *pager, d *db.DB, limit int,
	read func(ctx context.Context, q db.Querier, after string, limit int) ([]T, error)) ([]T, error) {
	p.mu.Lock()
	after := p.after
	p.mu.Unlock()
	if after == "" {
		after = nilUUID
	}
	var out []T
	err := readOnly(ctx, d, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if out, err = read(ctx, tx, after, limit); err != nil {
			return err
		}
		if len(out) == 0 && after != nilUUID {
			out, err = read(ctx, tx, nilUUID, limit)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(out) < limit {
		p.after = nilUUID
	} else {
		p.after = out[len(out)-1].cursorKey()
	}
	return out, nil
}
