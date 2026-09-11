// Package operatorroles is the operator directory: which roles it may name, and
// the one mechanism by which a deployment gets its first operator.
//
// # The problem this exists for
//
// `operator_roles` has decided who is an operator since migration 00010, and
// `internal/identity` reads it on every login. Nothing writes it. There is no
// HTTP route that grants a role, no admin action kind that does, and no CLI --
// the only writer in the repository is the LOCAL seed script. A deployment that
// has never had an operator therefore cannot get one, and every route behind
// `gate:propose`, `account:freeze` or `admin:audit_read` is unreachable in it.
//
// # Why the identity provider does not answer it
//
// The obvious mechanism is a role claim: ZITADEL can assert project roles in an
// ID token, and a mapping would need no new configuration surface. It is refused,
// and ADR-0024 records the reasoning:
//
//   - `internal/identity`'s package doc has said since it was written that the
//     package must never "take roles from identity-provider claims", and
//     ADR-0022 put the Nodal user, and therefore Nodal's authority, in Neon.
//     Reading a role claim would reverse both without an ADR.
//   - A claim carries no `granted_by`, no `reason` and no `expires_at`. Those
//     three columns are what makes a standing grant reviewable, and a mechanism
//     that cannot fill them replaces a reviewable record with a provider setting.
//   - Revocation would then live in two places with no reconciliation between
//     them, and the failure mode of the disagreement is a live operator role
//     somebody believes they removed.
//
// This package is a LEAF: it holds the declaration format, the role list and
// the production rule, and imports nothing but internal/security and
// internal/errs, so internal/config can validate a deployment's declaration
// without importing the login path. The writer that acts on a declaration is
// identity.OperatorBootstrap.
//
// # What this does instead
//
// `CP_AUTH_BOOTSTRAP_OPERATORS` declares `issuer|subject=ROLE` entries. At login,
// when the declaration names the identity that just authenticated, the row is
// written to the directory -- with `granted_by` NULL and a reason that says where
// it came from -- and the login then reads the directory exactly as it always
// did. Nothing about how a role becomes authority changes; only how the first row
// appears.
//
// It is the shape D-052 already established for `CP_API_SANDBOX_GATES`: a
// declaration in the deployment blueprint, reconciled idempotently, recorded as
// the SYSTEM actor with the variable's own name as the actor id, and refused
// where it would be dangerous.
//
// # The properties that make it safe
//
//   - **No self-grant.** There is no API. A principal cannot reach this code by
//     making a request; the deployment's own configuration is the only input.
//   - **BREAK_GLASS is unreachable.** The dual-control approve side is a
//     time-boxed elevation through an approved admin action, and both this
//     package and migration 00760's CHECK refuse to write it as a standing role.
//   - **Dual control survives a fully-privileged principal.** Every approval
//     path compares subjects, not roles: `security.RequireDualControl` refuses
//     `p.SubjectID == proposerSubjectID`, `gates` requires two distinct
//     approvers neither of whom proposed, and `admin` requires
//     `approver != proposer`. A bootstrap operator holding every role still
//     cannot be two people.
//   - **A revoked grant stays revoked.** The insert is ON CONFLICT DO NOTHING on
//     `(user_id, role)`, so an operator who revoked a bootstrap grant does not
//     find it restored at the grantee's next login. Removing the declaration is
//     the way to stop it being re-offered; nothing here undoes a revocation.
//   - **PROD is narrowed.** `internal/config` refuses a PROD deployment whose
//     declaration is anything other than empty or exactly one ADMIN, so the
//     variable cannot become a standing staff directory.
package operatorroles

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// ActorID is the actor recorded for a grant this package writes. It names the
// variable rather than a person, because no person made this decision at this
// moment: the deployment declared it.
const ActorID = "config:CP_AUTH_BOOTSTRAP_OPERATORS"

// GrantReason is written to operator_roles.reason so a row's provenance is
// legible from the row.
const GrantReason = "bootstrap: declared by CP_AUTH_BOOTSTRAP_OPERATORS"

// AuditAction is the audit action for a bootstrap grant.
const AuditAction = "operator_role.bootstrapped"

// Directory returns the roles the operator directory may name: every declared
// role except BREAK_GLASS.
//
// It is the Go half of the pair test/integration/enums holds against
// operator_roles_role_check, and it is derived from security.AllRoles rather
// than typed out, so a role added to the matrix cannot be silently absent here.
func Directory() []security.Role {
	all := security.AllRoles()
	out := make([]security.Role, 0, len(all))
	for _, r := range all {
		if r == security.RoleBreakGlass {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Declaration is one bootstrap entry: an identity, and the role it is given.
type Declaration struct {
	Issuer  string
	Subject string
	Role    security.Role
}

// String renders a declaration for a log line. The subject is an opaque
// provider identifier, not a name or an address, and it is printed whole
// because an operator reading the boot log has to be able to check it against
// the identity provider.
func (d Declaration) String() string {
	return fmt.Sprintf("%s|%s=%s", d.Issuer, d.Subject, d.Role)
}

// ParseDeclarations reads the CP_AUTH_BOOTSTRAP_OPERATORS format:
//
//	<issuer>|<subject>=<ROLE>[,<issuer>|<subject>=<ROLE>...]
//
// An empty string declares nothing, which is the normal case for a deployment
// whose operators already exist.
func ParseDeclarations(raw string) ([]Declaration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	allowed := make(map[security.Role]struct{}, len(Directory()))
	for _, r := range Directory() {
		allowed[r] = struct{}{}
	}
	seen := map[string]struct{}{}
	var out []Declaration
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, errs.New(errs.CodeValidationFailed,
				"operatorroles: an empty entry; the format is issuer|subject=ROLE, comma separated")
		}
		pipe := strings.LastIndex(entry, "|")
		eq := strings.LastIndex(entry, "=")
		if pipe <= 0 || eq <= pipe+1 {
			return nil, errs.Newf(errs.CodeValidationFailed,
				"operatorroles: %q is not issuer|subject=ROLE", entry)
		}
		d := Declaration{
			Issuer:  strings.TrimSpace(entry[:pipe]),
			Subject: strings.TrimSpace(entry[pipe+1 : eq]),
			Role:    security.Role(strings.ToUpper(strings.TrimSpace(entry[eq+1:]))),
		}
		switch {
		case d.Issuer == "" || strings.ContainsAny(d.Issuer, " \t"):
			return nil, errs.Newf(errs.CodeValidationFailed, "operatorroles: %q has no usable issuer", entry)
		case d.Subject == "" || strings.ContainsAny(d.Subject, " \t"):
			return nil, errs.Newf(errs.CodeValidationFailed, "operatorroles: %q has no usable subject", entry)
		case d.Role == security.RoleBreakGlass:
			return nil, errs.New(errs.CodeValidationFailed,
				"operatorroles: BREAK_GLASS is a time-boxed elevation granted by an approved admin action, never a standing role; declaring it here would hand one principal both sides of dual control")
		}
		if _, ok := allowed[d.Role]; !ok {
			return nil, errs.Newf(errs.CodeValidationFailed, "operatorroles: %q is not a role the operator directory may name", d.Role)
		}
		key := d.Issuer + "|" + d.Subject + "=" + string(d.Role)
		if _, dup := seen[key]; dup {
			return nil, errs.Newf(errs.CodeValidationFailed, "operatorroles: %q is declared twice", key)
		}
		seen[key] = struct{}{}
		out = append(out, d)
	}
	return out, nil
}

// IsSafeForProduction reports whether a declaration may be carried by a PROD
// deployment: nothing at all, or exactly one entry granting ADMIN.
//
// One entry because the variable's purpose is to make the FIRST operator
// possible, and one operator is what "first" means; every subsequent grant is a
// decision a person makes with a reason attached, in a directory a person can
// read. ADMIN because a first operator who cannot reach the admin plane cannot
// grant anybody else anything, so any other single role would leave the
// deployment in exactly the state this mechanism exists to end.
func IsSafeForProduction(decls []Declaration) bool {
	switch len(decls) {
	case 0:
		return true
	case 1:
		return decls[0].Role == security.RoleAdmin
	default:
		return false
	}
}
