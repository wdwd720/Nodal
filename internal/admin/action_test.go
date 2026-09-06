package admin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func TestTransitionTable(t *testing.T) {
	t.Parallel()
	allowed := map[[2]Status]bool{
		{StatusProposed, StatusApproved}:  true,
		{StatusProposed, StatusRejected}:  true,
		{StatusProposed, StatusExecuted}:  true, // non-dual kinds only (enforced by Execute)
		{StatusProposed, StatusFailed}:    true, // non-dual kinds only
		{StatusProposed, StatusExpired}:   true,
		{StatusProposed, StatusCancelled}: true,
		{StatusApproved, StatusExecuted}:  true,
		{StatusApproved, StatusFailed}:    true,
		{StatusApproved, StatusExpired}:   true,
	}
	statuses := AllStatuses()
	require.Len(t, statuses, 7)
	for _, from := range statuses {
		for _, to := range statuses {
			assert.Equal(t, allowed[[2]Status{from, to}], CanTransition(from, to), "%s -> %s", from, to)
		}
	}
	assert.False(t, CanTransition(StatusNone, StatusProposed), "proposal is not a transition of an existing row")
	assert.False(t, CanTransition("BOGUS", StatusApproved))

	for _, s := range statuses {
		assert.True(t, s.Valid())
		assert.Equal(t, s == StatusProposed || s == StatusApproved, s.Pending(), s)
		assert.Equal(t, !s.Pending(), s.Terminal(), s)
	}
	assert.False(t, StatusNone.Valid())
	assert.False(t, StatusNone.Terminal())
	assert.False(t, Status("").Valid())
}

func TestParamsHash(t *testing.T) {
	t.Parallel()
	h1, err := ParamsHash(json.RawMessage(`{"amount":"10.00","account_id":"a1","legs":[{"y":1,"x":2}]}`))
	require.NoError(t, err)
	require.Len(t, h1, 32)
	h2, err := ParamsHash(json.RawMessage(" {\n\"legs\" : [ { \"x\" : 2 , \"y\" : 1 } ] , \"account_id\" : \"a1\" , \"amount\" : \"10.00\" }"))
	require.NoError(t, err)
	assert.Equal(t, h1, h2, "key order and whitespace do not change the hash")
	assert.True(t, ParamsMatch(json.RawMessage(`{"legs":[{"x":2,"y":1}],"amount":"10.00","account_id":"a1"}`), h1))

	// Any semantic change is detected.
	for name, tampered := range map[string]string{
		"value":        `{"amount":"10.01","account_id":"a1","legs":[{"y":1,"x":2}]}`,
		"array order":  `{"amount":"10.00","account_id":"a1","legs":[{"x":2,"y":1},{}]}`,
		"extra key":    `{"amount":"10.00","account_id":"a1","legs":[{"y":1,"x":2}],"memo":""}`,
		"missing key":  `{"amount":"10.00","legs":[{"y":1,"x":2}]}`,
		"type change":  `{"amount":"10.00","account_id":"a1","legs":[{"y":"1","x":2}]}`,
		"empty object": `{}`,
	} {
		assert.False(t, ParamsMatch(json.RawMessage(tampered), h1), name)
	}
	assert.False(t, ParamsMatch(json.RawMessage(`{"a":`), h1), "invalid JSON never matches")
	assert.False(t, ParamsMatch(json.RawMessage(`{"amount":"10.00","account_id":"a1","legs":[{"y":1,"x":2}]}`), []byte("nope")))

	// nil and {} are the same empty params.
	hNil, err := ParamsHash(nil)
	require.NoError(t, err)
	hEmpty, err := ParamsHash(json.RawMessage(` {} `))
	require.NoError(t, err)
	assert.Equal(t, hNil, hEmpty)

	// Only JSON objects with canonical numbers are accepted.
	for name, bad := range map[string]string{
		"array":   `[1,2]`,
		"string":  `"x"`,
		"null":    `null`,
		"float":   `{"amount":10.5}`,
		"invalid": `{"a":`,
	} {
		_, err := ParamsHash(json.RawMessage(bad))
		require.Error(t, err, name)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
		e, _ := errs.As(err)
		assert.Equal(t, "params", e.Fields["field"], name)
	}
}

func TestSnapshotHash(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	a := Action{
		ID: NewActionID(), Kind: KindLedgerCorrection, TargetType: "account", TargetID: "acct-1",
		Params: json.RawMessage(`{"a":"1"}`), ParamsHash: []byte{1}, Reason: "compensating entry",
		RequiresDual: true, Status: StatusProposed, ProposedBy: "u1", ProposedAt: now, ProposerStepUpAt: now,
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}
	h1, err := snapshotHash(a)
	require.NoError(t, err)
	h2, err := snapshotHash(a)
	require.NoError(t, err)
	assert.Equal(t, h1, h2)
	b := a
	b.Status = StatusApproved
	h3, err := snapshotHash(b)
	require.NoError(t, err)
	assert.NotEqual(t, h1, h3)

	assert.True(t, a.Expired(now.Add(time.Hour)), "expiry is inclusive")
	assert.False(t, a.Expired(now.Add(time.Hour-time.Microsecond)))
}

func TestTextHelpers(t *testing.T) {
	t.Parallel()
	assert.True(t, validText("ok"))
	assert.True(t, validText(""))
	assert.False(t, validText("a\x00b"))
	assert.False(t, validText("\xff"))
	assert.Equal(t, "héll", truncate("héllo", 5))
	assert.Equal(t, "hé", truncate("héllo", 3), "never splits a rune")
	assert.Equal(t, "abc", truncate("abc", 10))
	assert.Nil(t, optional(""))
	assert.Equal(t, "x", *optional("x"))
	assert.Equal(t, "", deref(nil))
}
