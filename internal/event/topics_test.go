package event

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allTopicConstants must list every Topic constant; the test below fails if
// a constant is added without being registered, or registered without a
// constant.
var allTopicConstants = []Topic{
	TopicLedgerTransactionPosted,
	TopicCapitalReservationCreated,
	TopicCapitalReservationConsumed,
	TopicCapitalReservationReleased,
	TopicCapitalReservationExpired,
	TopicCapitalReservationLocked,
	TopicCapitalHoldPlaced,
	TopicCapitalHoldReleased,
	TopicCapitalEnvelopeChanged,
	TopicCapitalEnvelopeCreated,
	TopicCapitalEnvelopeUpdated,
	TopicCapitalEnvelopeStatusChanged,
	TopicCapitalEnvelopePnLApplied,
	TopicCapitalEnvelopeExhausted,
	TopicCapitalEnvelopeUndeployed,
	TopicFundingDepositTransitioned,
	TopicIntentTransitioned,
	TopicOrderTransitioned,
	TopicExecutionAttemptTransitioned,
	TopicFillObserved,
	TopicReconciliationRecordTransitioned,
	TopicGateTransitioned,
	TopicKillSwitchChanged,
	TopicAuditEventAppended,
	TopicSecurityEvent,
	TopicAgentRunCompleted,
	TopicPredictionCommitted,
}

func TestTopics_RegistryComplete(t *testing.T) {
	t.Parallel()
	required := []string{
		"ledger.transaction.posted",
		"capital.reservation.created", "capital.reservation.consumed", "capital.reservation.released", "capital.reservation.expired",
		"capital.reservation.locked",
		"capital.hold.placed", "capital.hold.released",
		"capital.envelope.changed", "capital.envelope.created", "capital.envelope.updated",
		"capital.envelope.status_changed", "capital.envelope.pnl_applied",
		"capital.envelope.exhausted", "capital.envelope.undeployed",
		"funding.deposit.transitioned",
		"intent.transitioned",
		"order.transitioned",
		"execution.attempt.transitioned",
		"fill.observed",
		"reconciliation.record.transitioned",
		"gate.transitioned",
		"killswitch.changed",
		"audit.event.appended",
		"security.event",
		"agent.run.completed",
		"prediction.committed",
	}
	for _, name := range required {
		_, ok := Lookup(Topic(name))
		assert.True(t, ok, "required topic %q is not registered", name)
	}

	specs := Topics()
	require.Len(t, specs, len(allTopicConstants), "registry and constant list disagree")
	assert.True(t, sort.SliceIsSorted(specs, func(i, j int) bool { return specs[i].Topic < specs[j].Topic }))

	seen := map[Topic]bool{}
	for _, c := range allTopicConstants {
		sp, ok := Lookup(c)
		require.True(t, ok, "constant %q is not registered", c)
		assert.False(t, seen[c], "constant %q listed twice", c)
		seen[c] = true

		assert.Regexp(t, `^[a-z0-9_.]+$`, string(c))
		assert.Equal(t, c, sp.Topic)
		assert.GreaterOrEqual(t, sp.SchemaVersion, 1, "%s has no schema version", c)
		assert.Equal(t, sp.SchemaVersion, c.Version())
		assert.NotEmpty(t, sp.AggregateType, "%s has no aggregate type", c)
		require.NotNil(t, sp.Key, "%s has no partition-key rule", c)

		// The rule yields the aggregate id for a matching envelope and
		// refuses a foreign aggregate.
		ev := validEnvelope(c, "agg-42")
		key, err := sp.Key(ev)
		require.NoError(t, err, c)
		assert.Equal(t, "agg-42", key, c)
		ev.AggregateType = "something_else"
		_, err = sp.Key(ev)
		assert.Error(t, err, c)
		assert.True(t, sp.Accepts(string(c)))
		assert.False(t, sp.Accepts("other.type"))
	}
}

func TestTopics_UnknownTopic(t *testing.T) {
	t.Parallel()
	_, ok := Lookup("nope.nothing")
	assert.False(t, ok)
	assert.Equal(t, 0, Topic("nope.nothing").Version())
	_, ok = Topic("nope.nothing").Spec()
	assert.False(t, ok)
	assert.Equal(t, "order.transitioned", TopicOrderTransitioned.String())
}

func TestAggregateKey(t *testing.T) {
	t.Parallel()
	key := AggregateKey("order")
	_, err := key(Envelope{AggregateType: "order"})
	assert.Error(t, err, "empty aggregate id")
	got, err := key(Envelope{AggregateType: "order", AggregateID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, "o-1", got)
}

func TestSpec_AcceptsExplicitTypes(t *testing.T) {
	t.Parallel()
	sp := Spec{Topic: "x.y", EventTypes: []string{"x.y.created", "x.y.deleted"}}
	assert.True(t, sp.Accepts("x.y.created"))
	assert.False(t, sp.Accepts("x.y"))
}
