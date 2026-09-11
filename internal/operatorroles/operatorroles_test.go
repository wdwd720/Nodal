package operatorroles

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

const issuer = "https://nodal-az1hxe.us1.zitadel.cloud"

// The directory may name every declared role except BREAK_GLASS and CUSTOMER,
// and it is derived from the matrix rather than typed out, so a role added there
// cannot be silently absent here.
func TestDirectory_IsEveryRoleExceptBreakGlassAndCustomer(t *testing.T) {
	t.Parallel()
	got := Directory()
	assert.Len(t, got, len(security.AllRoles())-2)
	for _, r := range got {
		assert.NotEqual(t, security.RoleBreakGlass, r)
		assert.NotEqual(t, security.RoleCustomer, r,
			"CUSTOMER is what a principal this directory says nothing about already is; naming it issues an OPERATOR session with no operator permissions (F-179)")
		assert.True(t, r.Valid(), "%s is not a declared role", r)
	}
	for i := 1; i < len(got); i++ {
		assert.Less(t, string(got[i-1]), string(got[i]), "Directory is not sorted")
	}
	// The negative control: the matrix does still contain both roles we
	// exclude, so this is excluding something rather than agreeing with an
	// empty set.
	var sawBreakGlass, sawCustomer bool
	for _, r := range security.AllRoles() {
		switch r {
		case security.RoleBreakGlass:
			sawBreakGlass = true
		case security.RoleCustomer:
			sawCustomer = true
		}
	}
	require.True(t, sawBreakGlass)
	require.True(t, sawCustomer)
}

// A CUSTOMER declaration is refused by name, and the message says why rather
// than reporting "not a role the directory may name" -- which would be true and
// would read as a typo.
func TestParseDeclarations_RefusesCustomer(t *testing.T) {
	t.Parallel()
	_, err := ParseDeclarations(issuer + "|284169943049306115=CUSTOMER")
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "CUSTOMER is not an operator role")
}

// The transition actions are a closed list, paired with 00799's CHECK by
// test/integration/enums.
func TestTransitionActions_AreDeclaredAndValid(t *testing.T) {
	t.Parallel()
	got := AllTransitionActions()
	assert.Equal(t, []TransitionAction{ActionRevoke, ActionSetExpiry}, got)
	for _, a := range got {
		assert.True(t, a.Valid(), "%s", a)
	}
	assert.False(t, TransitionAction("GRANT").Valid(),
		"a grant is the INSERT that creates the row, not a transition on it")
}

func TestParseDeclarations_Empty(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "   ", "\t\n"} {
		got, err := ParseDeclarations(in)
		require.NoError(t, err)
		assert.Empty(t, got)
	}
}

func TestParseDeclarations_Accepts(t *testing.T) {
	t.Parallel()
	got, err := ParseDeclarations(issuer + "|2841|ADMIN")
	require.Error(t, err, "a pipe-only entry with no = is not the format")
	assert.Nil(t, got)

	got, err = ParseDeclarations(issuer + "|284169943049306115=ADMIN")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, issuer, got[0].Issuer)
	assert.Equal(t, "284169943049306115", got[0].Subject)
	assert.Equal(t, security.RoleAdmin, got[0].Role)

	// Whitespace around entries, and a lower-case role, are tolerated: an
	// operator pasting a value into a dashboard should not be defeated by a
	// space.
	got, err = ParseDeclarations("  " + issuer + "|1=admin ,  " + issuer + "|2=OPERATIONS  ")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, security.RoleAdmin, got[0].Role)
	assert.Equal(t, security.RoleOperations, got[1].Role)
	assert.Equal(t, issuer+"|1=ADMIN", got[0].String())
}

func TestParseDeclarations_Refuses(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"no separator at all":  "nonsense",
		"no role":              issuer + "|1=",
		"no subject":           issuer + "|=ADMIN",
		"empty subject spaced": issuer + "| =ADMIN",
		"no issuer":            "|1=ADMIN",
		"an empty entry":       issuer + "|1=ADMIN,,",
		"an unknown role":      issuer + "|1=SUPERUSER",
		"a role with a space":  issuer + "|1=ADMIN ROLE",
		"a duplicate":          issuer + "|1=ADMIN," + issuer + "|1=ADMIN",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDeclarations(in)
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// BREAK_GLASS is the approve side of dual control. A standing grant of it would
// hand one principal both halves, so it is refused by name with a message that
// says why, in both cases and both spellings.
func TestParseDeclarations_RefusesBreakGlassByName(t *testing.T) {
	t.Parallel()
	for _, in := range []string{issuer + "|1=BREAK_GLASS", issuer + "|1=break_glass"} {
		_, err := ParseDeclarations(in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dual control")
	}
}

// PROD: nothing, or exactly one ADMIN. The variable makes a FIRST operator
// possible; a staff list in an environment variable would have replaced a
// reviewable directory with an unreviewable one.
func TestIsSafeForProduction(t *testing.T) {
	t.Parallel()
	must := func(raw string) []Declaration {
		d, err := ParseDeclarations(raw)
		require.NoError(t, err)
		return d
	}
	assert.True(t, IsSafeForProduction(nil))
	assert.True(t, IsSafeForProduction(must("")))
	assert.True(t, IsSafeForProduction(must(issuer+"|1=ADMIN")))

	assert.False(t, IsSafeForProduction(must(issuer+"|1=OPERATIONS")),
		"a single non-ADMIN grant leaves the deployment unable to grant anybody anything, which is the state this exists to end")
	assert.False(t, IsSafeForProduction(must(issuer+"|1=ADMIN,"+issuer+"|2=ADMIN")))
	assert.False(t, IsSafeForProduction(must(issuer+"|1=ADMIN,"+issuer+"|1=SECURITY")))
}

func TestConstants_NameTheVariableTheyComeFrom(t *testing.T) {
	t.Parallel()
	// A row whose provenance is not legible from the row is a row nobody can
	// review, so both the actor and the reason name the declaration.
	assert.Contains(t, ActorID, "CP_AUTH_BOOTSTRAP_OPERATORS")
	assert.Contains(t, GrantReason, "CP_AUTH_BOOTSTRAP_OPERATORS")
	assert.Equal(t, "operator_role.bootstrapped", AuditAction)
}
