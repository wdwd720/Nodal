package redpandabus_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/reality/redpandabus"
)

// The partition rule is the whole of the ordering guarantee: Kafka orders
// within a partition, the partition comes from the key, so the key must be
// the aggregate id the registry names. A wrong key is the dangerous case
// because it publishes perfectly well and only shows up later as one
// aggregate's events arriving out of order.
func TestValidatePartitionKey(t *testing.T) {
	t.Parallel()
	registered := string(event.TopicOrderTransitioned)
	spec, ok := event.Lookup(event.TopicOrderTransitioned)
	require.True(t, ok, "the test needs a registered topic")
	good := map[string]string{
		event.HeaderAggregateType: spec.AggregateType,
		event.HeaderAggregateID:   "order-7",
	}
	require.NoError(t, redpandabus.ValidatePartitionKey(registered, "order-7", good))

	// An unregistered topic is not envelope traffic: reality's normalized
	// event stream is keyed by dedup_id and carries no aggregate.
	require.NoError(t, redpandabus.ValidatePartitionKey("reality.normalized.wallet_events", "sig1/WalletAAA", nil))

	bad := map[string]struct {
		topic   string
		key     string
		headers map[string]string
	}{
		"empty key on an unregistered topic still round-robins": {"reality.normalized.wallet_events", "", nil},
		"empty key on a registered topic":                       {registered, "", good},
		"key is not the aggregate id":                           {registered, "order-8", good},
		"aggregate id header missing":                           {registered, "order-7", map[string]string{event.HeaderAggregateType: spec.AggregateType}},
		"aggregate type header missing":                         {registered, "order-7", map[string]string{event.HeaderAggregateID: "order-7"}},
		"aggregate type is another aggregate": {registered, "order-7", map[string]string{
			event.HeaderAggregateType: event.AggregateIntent, event.HeaderAggregateID: "order-7",
		}},
		"no headers at all on a registered topic": {registered, "order-7", nil},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := redpandabus.ValidatePartitionKey(c.topic, c.key, c.headers)
			require.Error(t, err)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// Every registered topic must be publishable under its own rule, so the rule
// can never be one the registry cannot satisfy.
func TestValidatePartitionKey_AcceptsEveryRegisteredTopic(t *testing.T) {
	t.Parallel()
	specs := event.Topics()
	require.NotEmpty(t, specs)
	for _, s := range specs {
		headers := map[string]string{
			event.HeaderAggregateType: s.AggregateType,
			event.HeaderAggregateID:   "agg-1",
		}
		require.NoErrorf(t, redpandabus.ValidatePartitionKey(string(s.Topic), "agg-1", headers), "topic %s", s.Topic)
	}
}
