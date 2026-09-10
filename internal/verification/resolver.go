package verification

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// BaseResolver reports the level Nodal can establish by itself — NODAL_IDENTITY
// for an ACTIVE account whose identity provider asserted a verified e-mail
// address, otherwise NONE. `identity.NodalIdentityResolver` is the one this
// system has.
//
// It is an interface rather than a concrete dependency so that this package
// does not import internal/identity, and so that the composition can be tested
// with a base that says NONE without a database.
type BaseResolver interface {
	Level(ctx context.Context, accountID accounts.AccountID) (valuedomain.VerificationLevel, error)
}

// Resolver reports an account's financial verification level from the profile
// state and the evidence behind it.
//
// It replaces the cap `identity.NodalIdentityResolver` documents: that resolver
// deliberately cannot report PAYOUT_KYC or ENHANCED, because it has no way to
// know them. This one does — from a provider decision and the sub-checks that
// justify it — and it composes with the base rather than replacing it, so an
// account whose owner is not ACTIVE still reports NONE however good the KYC
// evidence is.
//
// Fail closed at every step:
//
//   - base NONE            → NONE, whatever the profile says
//   - state not VERIFIED   → the base level
//   - window elapsed       → the base level, even before a sweep moves the
//     state to EXPIRED
//   - evidence incomplete  → the base level
//   - evidence complete    → PAYOUT_KYC, or ENHANCED when the political
//     exposure question was answered too
type Resolver struct {
	base  BaseResolver
	repo  *Repository
	q     db.Querier
	clock clock.Clock
}

// NewResolver returns a Resolver. Every argument is required: a resolver with a
// nil base would report a level for an account nobody checked the standing of.
func NewResolver(base BaseResolver, repo *Repository, q db.Querier, clk clock.Clock) (*Resolver, error) {
	if base == nil || repo == nil || q == nil || clk == nil {
		return nil, errs.New(errs.CodeValidationFailed,
			"verification: a resolver needs a base resolver, a repository, a querier and a clock")
	}
	return &Resolver{base: base, repo: repo, q: q, clock: clk}, nil
}

// Level reports the account's verification level.
//
// A missing account, a missing profile and a person with no evidence are all
// answered rather than errored: the caller is deciding what an account may do,
// and "the least it could be" is the right answer for each of them.
func (r *Resolver) Level(ctx context.Context, accountID accounts.AccountID) (valuedomain.VerificationLevel, error) {
	base, err := r.base.Level(ctx, accountID)
	if err != nil {
		return valuedomain.VerificationNone, err
	}
	if base == valuedomain.VerificationNone {
		return valuedomain.VerificationNone, nil
	}
	owner, err := r.repo.OwnerOf(ctx, r.q, accountID)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return valuedomain.VerificationNone, nil
		}
		return valuedomain.VerificationNone, err
	}
	state, _, expiresAt, err := r.repo.ProfileState(ctx, r.q, owner)
	if err != nil {
		return valuedomain.VerificationNone, err
	}
	if state != StateVerified {
		// Skip the evidence read entirely: nothing above the base is reachable
		// without a VERIFIED standing, and a query per payout decision that
		// cannot change the answer is a query nobody should pay for.
		return base, nil
	}
	checks, err := r.repo.ChecksForUser(ctx, r.q, owner)
	if err != nil {
		return valuedomain.VerificationNone, err
	}
	return levelFrom(base, state, expiresAt, checks, r.clock.Now().UTC()), nil
}

// LevelForUser is Level keyed on the person rather than one of their accounts.
// Verification is a property of a person: somebody with three accounts verifies
// once, and all three see the same level.
func (r *Resolver) LevelForUser(ctx context.Context, userID accounts.UserID, base valuedomain.VerificationLevel) (valuedomain.VerificationLevel, error) {
	state, _, expiresAt, err := r.repo.ProfileState(ctx, r.q, userID)
	if err != nil {
		return valuedomain.VerificationNone, err
	}
	var checks []Check
	if state == StateVerified {
		if checks, err = r.repo.ChecksForUser(ctx, r.q, userID); err != nil {
			return valuedomain.VerificationNone, err
		}
	}
	return levelFrom(base, state, expiresAt, checks, r.clock.Now().UTC()), nil
}

// StaticBase is a BaseResolver that always reports one level. It exists for
// tests and for a deployment that has deliberately decided every account it
// serves has a Nodal identity; it is never wired in cmd/api, where the real
// base reads the account's standing.
type StaticBase valuedomain.VerificationLevel

// Level implements BaseResolver.
func (s StaticBase) Level(context.Context, accounts.AccountID) (valuedomain.VerificationLevel, error) {
	return valuedomain.VerificationLevel(s), nil
}

// expired reports whether a validity window has passed at t. It is here rather
// than inline so the sweep that moves a profile to EXPIRED and the resolver
// that refuses to trust a stale row agree by construction.
func expired(expiresAt *time.Time, at time.Time) bool {
	return expiresAt != nil && !at.Before(*expiresAt)
}
