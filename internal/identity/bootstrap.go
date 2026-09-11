package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/operatorroles"
	"github.com/nodal/controlplane/internal/security"
)

// OperatorBootstrap writes the operator-directory rows a deployment declared,
// at the login of the identity they name.
//
// It lives in this package because this is where the directory is read, and
// because putting the writer next to the reader is what makes it checkable that
// the two agree. internal/operatorroles holds the declaration format and the
// rules about it, and imports nothing, so internal/config can validate a
// deployment's declaration without importing the login path.
//
// The reasoning for the mechanism -- and for refusing the alternative of reading
// a role claim out of the ID token -- is in internal/operatorroles' package doc
// and in ADR-0024. The short version is that this package's own contract says it
// must never take roles from provider claims, and a claim cannot carry
// granted_by, a reason, or an expiry.
type OperatorBootstrap struct {
	decls []operatorroles.Declaration
	audit audit.Writer
}

// NewOperatorBootstrap returns a bootstrap over the declared entries. An empty
// list is legal and yields one that does nothing.
func NewOperatorBootstrap(decls []operatorroles.Declaration, w audit.Writer) (*OperatorBootstrap, error) {
	if w == nil {
		return nil, errs.New(errs.CodeValidationFailed, "identity: an audit writer is required to record a bootstrap grant")
	}
	return &OperatorBootstrap{decls: append([]operatorroles.Declaration(nil), decls...), audit: w}, nil
}

// Declarations returns what was declared.
func (b *OperatorBootstrap) Declarations() []operatorroles.Declaration {
	if b == nil {
		return nil
	}
	return append([]operatorroles.Declaration(nil), b.decls...)
}

// LogFields renders every declaration for the single line written at boot. A
// control whose effect is invisible in the logs is a control nobody can check
// happened, so the entries are named in full; a subject is an opaque provider
// identifier, not a name or an address.
func (b *OperatorBootstrap) LogFields() []string {
	decls := b.Declarations()
	out := make([]string, 0, len(decls))
	for _, d := range decls {
		out = append(out, d.String())
	}
	return out
}

// RolesFor returns the roles declared for one identity.
func (b *OperatorBootstrap) RolesFor(issuer, subject string) []security.Role {
	if b == nil {
		return nil
	}
	var out []security.Role
	for _, d := range b.decls {
		if d.Issuer == issuer && d.Subject == subject {
			out = append(out, d.Role)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Ensure writes the directory rows declared for this identity and returns the
// roles it granted in THIS call -- which is empty on every login after the
// first, and on every login by an identity the declaration does not name.
//
// It runs inside the login transaction, so the grant and the session that first
// carries it commit together or not at all. The insert is ON CONFLICT DO
// NOTHING, which is what makes it idempotent and is also what keeps a revoked
// grant revoked: an operator who took a bootstrap role away does not find it
// back at the grantee's next login.
func (b *OperatorBootstrap) Ensure(ctx context.Context, tx pgx.Tx, issuer, subject, userID string, now time.Time) ([]security.Role, error) {
	if b == nil || len(b.decls) == 0 {
		return nil, nil
	}
	want := b.RolesFor(issuer, subject)
	if len(want) == 0 {
		return nil, nil
	}
	var granted []security.Role
	for _, role := range want {
		tag, err := tx.Exec(ctx, `INSERT INTO operator_roles (user_id, role, granted_by, granted_at, reason)
			VALUES ($1, $2, NULL, $3, $4) ON CONFLICT (user_id, role) DO NOTHING`,
			userID, string(role), now.UTC(), operatorroles.GrantReason)
		if err != nil {
			return nil, fmt.Errorf("identity: bootstrap grant %s: %w", role, err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		granted = append(granted, role)
		payload, _ := json.Marshal(map[string]any{
			"role": string(role), "idp_issuer": issuer, "idp_subject": subject,
			"source": "CP_AUTH_BOOTSTRAP_OPERATORS",
		})
		if _, err := b.audit.Append(ctx, tx, audit.Event{
			Stream: audit.AdminStream, ActorType: string(security.ActorSystem), ActorID: operatorroles.ActorID,
			Action: operatorroles.AuditAction, ResourceType: "operator_role",
			ResourceID: userID + "/" + string(role), Reason: operatorroles.GrantReason,
			Payload: payload, OccurredAt: now,
		}); err != nil {
			return nil, err
		}
	}
	return granted, nil
}
