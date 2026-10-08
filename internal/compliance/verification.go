package compliance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// AccountVerification answers whether the owner of an account has a VERIFIED,
// unexpired identity. It is the production identity reader of the advanced
// subsystems (margin, derivatives, ...), which take an interface of exactly
// this method so a test can substitute a fixed answer.
//
// It fails closed: an account with no owner profile, a profile in any state
// but VERIFIED, or one whose verification has expired reads as unverified. A
// database error is an error, never a "no".
type AccountVerification struct {
	clk clock.Clock
}

// NewAccountVerification wires the reader.
func NewAccountVerification(clk clock.Clock) AccountVerification {
	if clk == nil {
		panic("compliance: NewAccountVerification requires a clock")
	}
	return AccountVerification{clk: clk}
}

// IdentityVerified reports whether account's owner is verified now.
func (v AccountVerification) IdentityVerified(ctx context.Context, q db.Querier, account accounts.AccountID) (bool, error) {
	var state string
	var expires *time.Time
	err := q.QueryRow(ctx, `SELECT p.identity_state, p.expires_at
		FROM accounts a JOIN compliance_profiles p ON p.user_id = a.owner_user_id
		WHERE a.id = $1`, account).Scan(&state, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errs.Wrap(fmt.Errorf("compliance: read identity state: %w", err), errs.CodeInternal, "could not read the identity state")
	}
	if IdentityState(state) != IdentityVerified {
		return false, nil
	}
	if expires != nil && !v.clk.Now().Before(*expires) {
		return false, nil
	}
	return true, nil
}
