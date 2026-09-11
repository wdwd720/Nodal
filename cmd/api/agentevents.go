package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agents"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/notifications"
	"github.com/nodal/controlplane/internal/security"
)

// The agent surface's publisher (D-082).
//
// internal/agents imports neither internal/notifications nor internal/activity,
// deliberately (D-073): what a person is told about an agent is those packages'
// decision and not a property of the lifecycle. So the adapter that turns an
// agent event into a notification lives here, in the composition root, which is
// where a boundary like that is meant to be crossed.
//
// # What it emits, and what it deliberately does not
//
// One kind: AGENT_PAUSED, and only for a pause somebody OTHER than the owner
// opened. An owner who paused their own agent pressed the button; a
// notification telling them what they just did is the noise that teaches people
// to ignore the inbox. What a person cannot know without being told is that an
// operator, the kill switch, a budget or a risk control stopped their agent.
//
// AGENT_DISABLED is NOT emitted, and not as an oversight. Disabling is an
// owner-only act -- internal/agents.ownerActor refuses every principal but the
// account's own USER, and the operator surface can only pause -- so a
// disabled-agent notification would always be telling somebody something they
// had done a second earlier. The only kind that could carry it is SYSTEM, which
// Kind.Suppressible reports false for: an unsuppressible notification for an
// act the reader performed is the worst version of this. The timeline carries
// AGENT_DISABLED (internal/activity), which is where a record of your own
// actions belongs.
//
// AGENT_CREATED, AGENT_ENABLED, AGENT_RESUMED and AGENT_ARCHIVED are silent for
// the same reason: every one of them is the owner's own act.
//
// # Why it opens its own transaction
//
// agents.Publisher is called AFTER the agent transaction committed, on purpose:
// an agent that could not be paused because a notification failed would be a
// control defeated by a mailbox. So the row this describes is already durable,
// and the notification is written in a transaction of its own -- which is the
// shape internal/notifications.Follower already has and the guarantee D-069
// states: a notification exists only for a state change that happened.
//
// # Why the follower does not then tell them twice
//
// The follower's agent_pauses source reads the same rows a tick later, under
// the same actor predicate, and derives its dedup key from the same pause row
// through the same exported helpers. Producer.Emit is idempotent on
// (user_id, dedup_key) with a unique index behind it, so whichever of the two
// arrives second reads the row that is already there and reports Created=false.
// This publisher is the latency; the follower is the guarantee.
type agentNotifier struct {
	db       *db.DB
	producer *notifications.Producer
	hub      notifications.Publisher
	log      *slog.Logger
}

// Publish implements agents.Publisher.
//
// Its error is logged and swallowed by internal/agents, never returned to the
// caller, so this returns one only to say what went wrong in that log line.
func (a agentNotifier) Publish(ctx context.Context, e agents.Event) error {
	if a.db == nil || a.producer == nil {
		return nil
	}
	if e.Kind != agents.EventAgentPaused || e.PauseID == "" {
		return nil
	}
	// An owner's own pause tells nobody. The predicate is the same one
	// internal/notifications' agent_pauses source applies, so the two cannot
	// disagree about who hears what.
	if e.ActorType == string(security.ActorUser) {
		return nil
	}
	accountID, err := accounts.ParseAccountID(e.AccountID)
	if err != nil {
		return err
	}

	// The follower runs as the system and so does this: it writes a
	// notification addressed to somebody else, which no customer principal may
	// do and no operator principal should.
	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: "agent-events", ActorType: security.ActorSystem,
	})

	title, body := notifications.AgentPauseCopy(pauseReasonOf(e), e.Reason)
	var emitted notifications.Emitted
	err = a.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		owner, oerr := ownerOfAccount(ctx, tx, accountID)
		if oerr != nil {
			return oerr
		}
		emitted, err = a.producer.Emit(ctx, tx, notifications.Notification{
			UserID:    owner,
			AccountID: &accountID,
			Kind:      notifications.KindAgentPaused,
			Title:     title,
			Body:      body,
			// Keyed on the PAUSE ROW, exactly as the follower keys it: a second
			// pause of the same agent is a second telling, and the same pause
			// seen twice is one.
			Ref:           notifications.AgentPauseRef(e.PauseID),
			Occurrence:    e.PauseID,
			CorrelationID: e.CorrelationID,
			OccurredAt:    e.OccurredAt,
			Data:          agentEventData(e),
		})
		return err
	})
	if err != nil {
		return err
	}
	// Published after the commit and outside it, for the reason the follower
	// documents: a hub is memory, and memory cannot be rolled back.
	if emitted.Created && a.hub != nil {
		a.hub.Notify(emitted.Notification)
		a.hub.Signal(notifications.Signal{
			UserID: emitted.Notification.UserID.String(),
			Scope:  notifications.ScopeAgent,
			Ref:    e.AgentID.String(),
		})
	}
	return nil
}

// agentEventData is what the client renders without a second request:
// identifiers and an enum value, and nothing else. No figure, no balance, no
// agent name -- a name is owner-supplied text and this payload is displayed.
func agentEventData(e agents.Event) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"agent_id":    e.AgentID.String(),
		"pause_id":    e.PauseID,
		"reason_code": pauseReasonOf(e),
	})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// pauseReasonOf is the reason code the pause was opened under, derived from the
// event's actor rather than read back from the row.
//
// This surface opens exactly two: OWNER_REQUEST for an owner's pause and
// OPERATOR for an admin one (internal/agents pauses under no other code). The
// owner's is filtered out above, so what reaches here is an operator's; naming
// it explicitly rather than defaulting keeps the copy honest if internal/agents
// ever grows a third.
func pauseReasonOf(e agents.Event) string {
	if e.ActorType == string(security.ActorOperator) {
		return "OPERATOR"
	}
	return ""
}

// ownerOfAccount resolves the person a notification about this account is
// addressed to.
func ownerOfAccount(ctx context.Context, q db.Querier, accountID accounts.AccountID) (accounts.UserID, error) {
	var owner accounts.UserID
	if err := q.QueryRow(ctx, `SELECT owner_user_id FROM accounts WHERE id = $1`, accountID).Scan(&owner); err != nil {
		return accounts.UserID{}, err
	}
	return owner, nil
}
