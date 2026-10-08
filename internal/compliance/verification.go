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

// AccountVerification is the identity rule of the advanced capabilities
// (DECISION_REGISTER D-055), the production identity reader every advanced
// subsystem shares, so no two of them can disagree about who is verified.
//
// An account is verified for SIMULATED activity when its owner is ACTIVE and
// holds at least NODAL_IDENTITY -- a verified email address, the level Nodal
// establishes by itself at login (internal/identity) -- or has a VERIFIED,
// unexpired compliance profile. The provider-backed KYC levels are not
// required for simulated value: no deployment can reach them without a
// provider (BLOCKERS B-06), and a control no user can satisfy is not a
// control (see internal/identity's NodalIdentityResolver). They remain
// required for anything real, which this build does not do.
//
// It fails closed: no such account, an owner who is not ACTIVE, or neither a
// verified email nor an unexpired verified profile reads as unverified. A
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
	var userStatus, state string
	var haveEmail bool
	var expires *time.Time
	err := q.QueryRow(ctx, `SELECT u.status, (u.email_hash IS NOT NULL), coalesce(p.identity_state, ''), p.expires_at
		FROM accounts a
		JOIN users u ON u.id = a.owner_user_id
		LEFT JOIN compliance_profiles p ON p.user_id = a.owner_user_id
		WHERE a.id = $1`, account).Scan(&userStatus, &haveEmail, &state, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errs.Wrap(fmt.Errorf("compliance: read identity state: %w", err), errs.CodeInternal, "could not read the identity state")
	}
	if userStatus != "ACTIVE" {
		return false, nil
	}
	if haveEmail {
		return true, nil
	}
	if IdentityState(state) != IdentityVerified {
		return false, nil
	}
	return expires == nil || v.clk.Now().Before(*expires), nil
}
