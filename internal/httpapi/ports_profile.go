package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/profile"
	"github.com/nodal/controlplane/internal/terms"
)

// ProfilePort is the product-level user record, the legal documents a user has
// accepted, and the lifecycle of their own account (internal/profile).
//
// Every method takes an Actor built from the request principal by the handler.
// None of them takes a subject the caller supplied, which is why no route here
// can reach another user's record -- except AdminUser and Decide, which name a
// user id in the path and are behind operator permissions.
type ProfilePort interface {
	Me(ctx context.Context, a profile.Actor) (profile.Me, error)
	Update(ctx context.Context, a profile.Actor, p profile.Patch) (profile.Profile, error)
	Terms(ctx context.Context, a profile.Actor) (profile.TermsView, error)
	Accept(ctx context.Context, a profile.Actor, ids []terms.DocumentID) (profile.TermsView, error)
	Account(ctx context.Context, a profile.Actor) (profile.AccountView, error)
	RequestClosure(ctx context.Context, a profile.Actor, reason string) (profile.AccountView, error)
	CancelClosure(ctx context.Context, a profile.Actor) (profile.AccountView, error)
	AdminUser(ctx context.Context, userID string) (profile.AdminUserView, error)
	Decide(ctx context.Context, op profile.Actor, userID string, d profile.ClosureDecision, reason string) (profile.AdminUserView, error)
}
