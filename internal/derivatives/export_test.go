package derivatives

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ApplyFill applies a fill directly, for tests only.
//
// Production code has no such method: nothing outside this package applies a
// fill (executeFill is reached through OpenPosition and ClosePosition), which
// is why it is unexported and why test/reachability does not list it. Tests --
// this package's own book-fill tests and the review reproductions written
// against the earlier exported name -- reach the same path through this alias,
// so what they exercise is exactly what production runs, including the
// authorization of the fill against its risk decision.
func (s *Service) ApplyFill(ctx context.Context, tx pgx.Tx, f Fill) (FillResult, error) {
	return s.executeFill(ctx, tx, f)
}
