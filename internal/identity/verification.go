package identity

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// NodalIdentityResolver reports the verification level Nodal can establish BY
// ITSELF, and never any level above it.
//
// # Why this exists
//
// `cmd/api` wired no verification resolver at all, so every account resolved to
// `VerificationNone`, and BLOCKERS B-06 recorded that as blocked on an external
// KYC provider. The provider is genuinely external. The conclusion drawn from
// it was too wide.
//
// `valuedomain.VerificationLevel` has four rungs and they answer different
// questions:
//
//	NONE            proven nothing
//	NODAL_IDENTITY  a verified email address and/or passkey: a person can be
//	                reached and can log in. It says nothing about who they are,
//	                where they are, or whether they may receive money.
//	PAYOUT_KYC      identity verification performed by, or accepted by, a
//	                payout provider
//	ENHANCED        enhanced due diligence
//
// Only the top two need a provider. NODAL_IDENTITY is a fact this system
// establishes on its own: the identity provider asserted a verified email
// address, and `internal/identity` persisted that assertion as
// `users.email_hash`.
//
// Reporting NONE for such an account is not conservatism, it is
// under-reporting, and it had a consequence: every Domain A action requires
// NODAL_IDENTITY, so buying anything in the internal marketplace, creating a
// native asset and trading one were unreachable in EVERY deployment, whatever
// its gates and policy said. A control that cannot be satisfied by any user is
// not a control; it is dead code with a reason attached.
//
// # What it deliberately cannot report
//
// PAYOUT_KYC and ENHANCED. Those are what B-06 is actually about, and this
// resolver has no way to reach them and no configuration that would let it.
// The blocker stays, narrowed to what it truly blocks: payouts.
//
// # What it does not see
//
// A passkey-only account with no email address. The ladder says "email address
// and/or passkey" and only the email half is recorded, so such an account
// reports NONE. That is under-reporting again, in the same direction, and it
// is named rather than hidden.
type NodalIdentityResolver struct {
	q db.Querier
}

// NewVerificationResolver returns a resolver reading from q.
func NewVerificationResolver(q db.Querier) (*NodalIdentityResolver, error) {
	if q == nil {
		return nil, errs.New(errs.CodeValidationFailed, "identity: a querier is required")
	}
	return &NodalIdentityResolver{q: q}, nil
}

const verificationSQL = `
SELECT u.status, (u.email_hash IS NOT NULL)
  FROM accounts a
  JOIN users u ON u.id = a.owner_user_id
 WHERE a.id = $1`

// Level reports the account's verification level.
//
// An account that does not exist, whose owning user is not ACTIVE, or whose
// identity provider never asserted a verified email address is NONE. A missing
// account is NOT an error here: the caller is deciding what an account may do,
// and "it may do nothing" is the right answer for an account that is not there.
func (r *NodalIdentityResolver) Level(ctx context.Context, accountID accounts.AccountID) (valuedomain.VerificationLevel, error) {
	var (
		status    string
		haveEmail bool
	)
	err := r.q.QueryRow(ctx, verificationSQL, accountID).Scan(&status, &haveEmail)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return valuedomain.VerificationNone, nil
	case err != nil:
		return valuedomain.VerificationNone, errs.Wrap(err, errs.CodeInternal, "identity: read verification level")
	}
	if status != "ACTIVE" || !haveEmail {
		return valuedomain.VerificationNone, nil
	}
	return valuedomain.VerificationNodalIdentity, nil
}
