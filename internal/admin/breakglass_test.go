package admin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

func TestParseBreakGlassParams(t *testing.T) {
	t.Parallel()
	user := uuid.NewString()
	ok, err := ParseBreakGlassParams(json.RawMessage(`{"user_id":"` + user + `","scope":"INC-42 kill release","duration_seconds":900}`))
	require.NoError(t, err)
	assert.Equal(t, BreakGlassParams{UserID: user, Scope: "INC-42 kill release", DurationSeconds: 900}, ok)

	tests := map[string]string{
		"unknown field":  `{"user_id":"` + user + `","scope":"x","duration_seconds":1,"roles":["ADMIN"]}`,
		"bad user id":    `{"user_id":"alice","scope":"x","duration_seconds":1}`,
		"nil user id":    `{"user_id":"00000000-0000-0000-0000-000000000000","scope":"x","duration_seconds":1}`,
		"empty scope":    `{"user_id":"` + user + `","scope":"","duration_seconds":1}`,
		"zero duration":  `{"user_id":"` + user + `","scope":"x","duration_seconds":0}`,
		"too long":       `{"user_id":"` + user + `","scope":"x","duration_seconds":14401}`,
		"negative":       `{"user_id":"` + user + `","scope":"x","duration_seconds":-5}`,
		"not json":       `{"user_id":`,
		"wrong type":     `{"user_id":"` + user + `","scope":"x","duration_seconds":"900"}`,
		"scope with NUL": "{\"user_id\":\"" + user + "\",\"scope\":\"a\\u0000b\",\"duration_seconds\":1}",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseBreakGlassParams(json.RawMessage(in))
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
	max, err := ParseBreakGlassParams(json.RawMessage(`{"user_id":"` + user + `","scope":"x","duration_seconds":14400}`))
	require.NoError(t, err)
	assert.Equal(t, int64(14400), max.DurationSeconds)
}

func TestParseGrant(t *testing.T) {
	t.Parallel()
	g, err := ParseGrant(json.RawMessage(`{"action_id":"a","user_id":"u","scope":"s","expires_at":"2026-09-05T13:00:00+01:00"}`))
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC), g.ExpiresAt)
	assert.Equal(t, time.UTC, g.ExpiresAt.Location())
	assert.True(t, g.Active(time.Date(2026, 9, 5, 11, 59, 0, 0, time.UTC)))
	assert.False(t, g.Active(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)))
	_, err = ParseGrant(json.RawMessage(`{"user_id":"u"}`))
	require.Error(t, err)
	_, err = ParseGrant(json.RawMessage(`null`))
	require.Error(t, err)
	_, err = ParseGrant(json.RawMessage(`{`))
	require.Error(t, err)
	assert.False(t, Grant{}.Active(time.Now()))
}

func TestPrincipalWithBreakGlass(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	userID := uuid.NewString()
	base := security.Principal{
		SubjectID: userID, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin},
		AccountIDs: []string{"acct-1"}, SessionID: "s1", AuthTime: now.Add(-time.Minute), AMR: []string{"mfa"},
	}
	grant := Grant{ActionID: "a1", UserID: userID, Scope: "INC-42", ExpiresAt: now.Add(30 * time.Minute)}

	elevated := PrincipalWithBreakGlass(base, grant)
	require.NoError(t, elevated.Validate())
	require.NotNil(t, elevated.BreakGlassUntil)
	assert.Equal(t, grant.ExpiresAt, *elevated.BreakGlassUntil)
	assert.True(t, elevated.HasRole(security.RoleBreakGlass))
	assert.True(t, elevated.HasRole(security.RoleAdmin))
	assert.True(t, elevated.Has(security.PermKillRelease, now), "dual-control permissions are live")
	assert.True(t, elevated.Has(security.PermGateApprove, now.Add(29*time.Minute)))
	assert.False(t, elevated.Has(security.PermKillRelease, now.Add(30*time.Minute)), "and expire with the grant")

	// The input is untouched and not aliased.
	assert.Nil(t, base.BreakGlassUntil)
	assert.Equal(t, []security.Role{security.RoleAdmin}, base.Roles)
	elevated.Roles[0] = security.RoleCustomer
	elevated.AccountIDs[0] = "other"
	assert.Equal(t, security.RoleAdmin, base.Roles[0])
	assert.Equal(t, "acct-1", base.AccountIDs[0])

	// Applying twice does not duplicate the role; a later grant replaces the expiry.
	again := PrincipalWithBreakGlass(elevated, Grant{UserID: userID, ExpiresAt: now.Add(time.Hour)})
	count := 0
	for _, r := range again.Roles {
		if r == security.RoleBreakGlass {
			count++
		}
	}
	assert.Equal(t, 1, count)
	assert.Equal(t, now.Add(time.Hour), *again.BreakGlassUntil)

	// Someone else's grant, an expiry-less grant, or an agent: no elevation.
	other := PrincipalWithBreakGlass(base, Grant{UserID: uuid.NewString(), ExpiresAt: now.Add(time.Hour)})
	assert.Nil(t, other.BreakGlassUntil)
	assert.False(t, other.HasRole(security.RoleBreakGlass))
	assert.False(t, other.Has(security.PermKillRelease, now))
	noExpiry := PrincipalWithBreakGlass(base, Grant{UserID: userID})
	assert.Nil(t, noExpiry.BreakGlassUntil)
	agent := security.AgentPrincipal(userID, "acct-1")
	ag := PrincipalWithBreakGlass(agent, grant)
	assert.Nil(t, ag.BreakGlassUntil)
	assert.Empty(t, ag.Roles)
	require.NoError(t, ag.Validate())
	assert.False(t, ag.Has(security.PermKillRelease, now))

	// Canonical-form comparison of user ids.
	upper := PrincipalWithBreakGlass(base, Grant{UserID: upperHex(userID), ExpiresAt: now.Add(time.Hour)})
	assert.NotNil(t, upper.BreakGlassUntil)
}

// upperHex upper-cases a uuid to exercise canonical comparison.
func upperHex(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}
