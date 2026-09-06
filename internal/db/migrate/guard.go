package migrate

import (
	"errors"
	"fmt"
)

// ProtectedVersion is the first ledger migration. Once the database version is
// at or above it, DownTo refuses any target below it: accounting history is
// never destroyed by a rollback. The ledger stage must use this exact version
// for its first migration (00100_ledger.sql).
const ProtectedVersion int64 = 100

// ErrProtectedVersion is returned by DownTo when the guard refuses.
var ErrProtectedVersion = errors.New("migrate: refusing to roll back below the ledger-protected version")

// guardDownTo is the whole rollback policy, kept pure for testing:
// rolling back to target is refused iff a protected migration is currently
// applied (current >= protected) and target would undo it (target < protected).
// Negative targets are invalid.
func guardDownTo(current, target, protected int64) error {
	if target < 0 {
		return fmt.Errorf("migrate: invalid target version %d", target)
	}
	if current >= protected && target < protected {
		return fmt.Errorf("%w: current=%d target=%d protected=%d", ErrProtectedVersion, current, target, protected)
	}
	return nil
}
