package reconciliation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

// TestRecordMatch_RecordsMatchesOnly: RecordMatch is the convergent path of a
// MATCHED comparison only. A mismatch or an inconclusive comparison is
// refused before anything is read or written -- it must open or refresh a
// record of its own -- and so is a match that is not a valid request.
func TestRecordMatch_RecordsMatchesOnly(t *testing.T) {
	t.Parallel()
	repo := NewRepository(clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)), nil, nil)
	for _, status := range []Status{StatusMismatch, StatusOpen, StatusResolvedManual, ""} {
		_, opened, err := repo.RecordMatch(context.Background(), nil, OpenRequest{
			Kind: KindBookJournal, Mode: ModePeriodic, ScopeType: "order_book", ScopeID: "book", Status: status,
		})
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "%q", status)
		assert.False(t, opened)
	}
	_, _, err := repo.RecordMatch(context.Background(), nil, OpenRequest{Kind: "BALANCE_EDIT", Mode: ModePeriodic,
		ScopeType: "order_book", ScopeID: "book", Status: StatusMatched})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "an unknown kind is refused")
	_, _, err = repo.RecordMatch(context.Background(), nil, OpenRequest{Kind: KindBookJournal, Mode: ModePeriodic,
		ScopeType: "order_book", ScopeID: "book", Status: StatusMatched, BlocksNewRisk: true})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a match never blocks new risk")
}
