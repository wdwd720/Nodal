//go:build integration

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/agents"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/notifications"
	"github.com/nodal/controlplane/internal/security"
)

// The composition root's agent publisher against the real table (D-082).
//
// The claim worth proving is not that a pause writes a notification -- that is
// one Emit call -- but that the publisher and the follower, which read the same
// pause row by two different routes and at two different times, produce exactly
// one telling between them. Nothing but a database can show that, because what
// makes it true is a unique index.

func agentTestRows(t *testing.T, d *db.DB) (accounts.UserID, accounts.AccountID, agent.AgentID) {
	t.Helper()
	ctx := context.Background()
	repo := accounts.NewRepository()
	user, err := repo.CreateUser(ctx, d, "https://idp.test", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	acct, err := repo.CreateAccount(ctx, d, user.ID, accounts.KindCustomer)
	require.NoError(t, err)

	strategyID := id.New[id.Any]().String()
	_, err = d.Exec(ctx, `INSERT INTO strategies
		(id, owner_account_id, owner_user_id, name, source_kind, status, created_by_actor_type, created_by_actor_id)
		VALUES ($1::uuid, $2, $3, $4, 'NATURAL_LANGUAGE', 'ACTIVE', 'USER', $5)`,
		strategyID, acct.ID, user.ID, "strategy-"+strategyID[:8], user.ID.String())
	require.NoError(t, err)

	agentID := agent.NewAgentID()
	_, err = d.Exec(ctx, `INSERT INTO agents
		(id, account_id, strategy_id, name, stage, state, created_by_actor_type, created_by_actor_id)
		VALUES ($1, $2, $3::uuid, 'an agent', 'DRAFT', 'DRAFT', 'USER', $4)`,
		agentID, acct.ID, strategyID, user.ID.String())
	require.NoError(t, err)
	return user.ID, acct.ID, agentID
}

// openAPause writes the agent_pauses row the publisher and the follower both
// read. The publisher is handed an event describing a row that already exists,
// which is exactly the order internal/agents calls it in.
func openAPause(t *testing.T, d *db.DB, agentID agent.AgentID, actorType, actorID, code, reason string, at time.Time) string {
	t.Helper()
	pauseID := id.New[id.Any]().String()
	_, err := d.Exec(context.Background(), `INSERT INTO agent_pauses
		(id, agent_id, reason_code, reason, open_orders_policy, paused_by_actor_type, paused_by_actor_id, paused_at)
		VALUES ($1::uuid, $2, $3, $4, 'LEAVE', $5, $6, $7)`,
		pauseID, agentID, code, reason, actorType, actorID, at.UTC())
	require.NoError(t, err)
	return pauseID
}

func notificationsFor(t *testing.T, d *db.DB, uid accounts.UserID, kind notifications.Kind) int {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND kind = $2`, uid, string(kind)).Scan(&n))
	return n
}

// TestIntegration_AnOperatorPauseTellsTheOwnerOnceAndOnlyOnce.
//
// Two writers, one row, one telling. The publisher emits the instant the agent
// transaction commits; the follower re-reads agent_pauses up to a tick later
// and again on every lap after that. They agree on the dedup key by
// construction -- both derive it from the pause row through
// notifications.AgentPauseRef -- so the unique index on (user_id, dedup_key) is
// what makes the second one a no-op rather than a duplicate inbox entry.
func TestIntegration_AnOperatorPauseTellsTheOwnerOnceAndOnlyOnce(t *testing.T) {
	d := openNotificationsDB(t)
	ctx := context.Background()
	uid, acct, agentID := agentTestRows(t, d)

	producer := notifications.NewProducer(time.Now, false)
	follower := notifications.NewFollower(producer)
	// Start every cursor before anything happens, the way a running process
	// does: a source with no cursor starts at now, not at the beginning of
	// history.
	_, err := follower.RunOnce(ctx, d, nil)
	require.NoError(t, err)

	at := time.Now().UTC()
	pauseID := openAPause(t, d, agentID, "OPERATOR", "op-1", "OPERATOR", "a compliance review", at)

	notifier := agentNotifier{db: d, producer: producer}
	require.NoError(t, notifier.Publish(ctx, agents.Event{
		Kind:      agents.EventAgentPaused,
		AgentID:   agentID,
		AccountID: acct.String(),
		PauseID:   pauseID,
		State:     agent.StatePaused,
		ActorType: string(security.ActorOperator),
		ActorID:   "op-1",
		Reason:    "a compliance review",
		// The publisher runs after the agent transaction committed, so the
		// event's instant is the transition's and not the publisher's.
		OccurredAt: at,
	}))

	require.Equal(t, 1, notificationsFor(t, d, uid, notifications.KindAgentPaused),
		"the owner is told their agent was stopped by somebody else")

	// What the person reads, and what the client renders from.
	var title, body string
	var data []byte
	require.NoError(t, d.QueryRow(ctx,
		`SELECT title, body, data FROM notifications WHERE user_id = $1 AND kind = 'AGENT_PAUSED'`,
		uid).Scan(&title, &body, &data))
	assert.Equal(t, "Your agent was paused by Nodal", title)
	assert.Contains(t, body, "a compliance review")
	assert.NotContains(t, body, "op-1", "the operator who acted is not named to the customer")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(data, &payload))
	assert.Equal(t, agentID.String(), payload["agent_id"])
	assert.Equal(t, pauseID, payload["pause_id"])
	assert.Equal(t, "OPERATOR", payload["reason_code"])

	// Now the follower's pass over the same row. It must write nothing.
	written, err := follower.RunOnce(ctx, d, nil)
	require.NoError(t, err)
	assert.Zero(t, written, "the follower must not tell somebody a second time")
	assert.Equal(t, 1, notificationsFor(t, d, uid, notifications.KindAgentPaused))

	// And the lap, which re-reads its own window on every pass, still must not.
	written, err = follower.RunOnce(ctx, d, nil)
	require.NoError(t, err)
	assert.Zero(t, written)
	assert.Equal(t, 1, notificationsFor(t, d, uid, notifications.KindAgentPaused))

	// A publisher retry -- the same event delivered twice -- is also one row.
	require.NoError(t, notifier.Publish(ctx, agents.Event{
		Kind: agents.EventAgentPaused, AgentID: agentID, AccountID: acct.String(),
		PauseID: pauseID, ActorType: string(security.ActorOperator), ActorID: "op-1",
		Reason: "a compliance review", OccurredAt: at,
	}))
	assert.Equal(t, 1, notificationsFor(t, d, uid, notifications.KindAgentPaused))
}

// TestIntegration_TheOwnersOwnPauseTellsNobody, by either route.
//
// The publisher and the follower apply the same predicate on the same column.
// If one of them ever stopped, this fails, which is the point: two writers with
// two opinions about who hears what is worse than either opinion.
func TestIntegration_TheOwnersOwnPauseTellsNobody(t *testing.T) {
	d := openNotificationsDB(t)
	ctx := context.Background()
	uid, acct, agentID := agentTestRows(t, d)

	producer := notifications.NewProducer(time.Now, false)
	follower := notifications.NewFollower(producer)
	_, err := follower.RunOnce(ctx, d, nil)
	require.NoError(t, err)

	at := time.Now().UTC()
	pauseID := openAPause(t, d, agentID, "USER", uid.String(), "OWNER_REQUEST", "paused by its owner", at)

	notifier := agentNotifier{db: d, producer: producer}
	require.NoError(t, notifier.Publish(ctx, agents.Event{
		Kind: agents.EventAgentPaused, AgentID: agentID, AccountID: acct.String(),
		PauseID: pauseID, State: agent.StatePaused, ActorType: string(security.ActorUser),
		ActorID: uid.String(), Reason: "paused by its owner", OccurredAt: at,
	}))
	assert.Zero(t, notificationsFor(t, d, uid, notifications.KindAgentPaused),
		"a person is not told what they themselves just did")

	written, err := follower.RunOnce(ctx, d, nil)
	require.NoError(t, err)
	assert.Zero(t, written, "and the follower agrees, because it is the same predicate")
	assert.Zero(t, notificationsFor(t, d, uid, notifications.KindAgentPaused))
}

// TestIntegration_TheOtherAgentEventsTellNobody.
//
// Every remaining kind is an act the owner performed a second earlier. The one
// that would be tempting is AGENT_DISABLED, and the only kind that could carry
// it is SYSTEM, which a person may not switch off: an unsuppressible
// notification for something the reader did is the worst version of this.
func TestIntegration_TheOtherAgentEventsTellNobody(t *testing.T) {
	d := openNotificationsDB(t)
	ctx := context.Background()
	uid, acct, agentID := agentTestRows(t, d)

	notifier := agentNotifier{db: d, producer: notifications.NewProducer(time.Now, false)}
	for _, kind := range agents.EventKinds() {
		if kind == agents.EventAgentPaused {
			continue
		}
		require.NoError(t, notifier.Publish(ctx, agents.Event{
			Kind: kind, AgentID: agentID, AccountID: acct.String(),
			ActorType: string(security.ActorUser), ActorID: uid.String(),
			Reason: "the owner did this", OccurredAt: time.Now().UTC(),
		}), "publishing %s must not fail", kind)
	}
	var total int
	require.NoError(t, d.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1`, uid).Scan(&total))
	assert.Zero(t, total, "only somebody else's pause is news")
}
