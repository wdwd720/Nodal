package redpandabus

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
)

// ValidatePartitionKey enforces the platform's one partition rule at the
// transport boundary, for the real client and for Loopback alike, so a
// producer that keys wrongly fails the same way in a test as in production.
//
// Kafka guarantees order only within a partition, and a record's partition
// comes from its key. So "every event of one aggregate is delivered in
// order" is true only while the key IS the aggregate id. internal/event's
// registry already declares that rule per topic (Spec.AggregateType and
// Spec.Key = AggregateKey(...)); this reads that registry rather than
// restating it, so the two can never drift.
//
// A registered topic carries an Envelope, and event.PublishHeaders always
// stamps aggregate_type and aggregate_id, so both are required and must
// agree with the registry and with the key. A topic that is not in the
// registry is not envelope traffic — reality's normalized-event stream is
// keyed by dedup_id (POINT_IN_TIME.md §4) and has no aggregate — and is
// allowed through with only the key requirement. That requirement is not a
// formality: an empty key makes the partitioner round-robin, which does not
// merely weaken ordering, it removes it.
func ValidatePartitionKey(topic, key string, headers map[string]string) error {
	if key == "" {
		return errs.New(errs.CodeValidationFailed,
			"redpandabus: a partition key is required; an empty key round-robins and destroys per-aggregate ordering").
			WithField("topic", topic)
	}
	spec, registered := event.Lookup(event.Topic(topic))
	if !registered {
		return nil
	}
	aggregateType, aggregateID := headers[event.HeaderAggregateType], headers[event.HeaderAggregateID]
	fields := map[string]any{"topic": topic}
	switch {
	case aggregateType == "":
		fields[event.HeaderAggregateType] = "required on a registered topic"
	case aggregateType != spec.AggregateType:
		fields[event.HeaderAggregateType] = "must be " + spec.AggregateType + ", got " + aggregateType
	}
	switch {
	case aggregateID == "":
		fields[event.HeaderAggregateID] = "required on a registered topic"
	case aggregateID != key:
		// The dangerous case: this publishes fine and silently scatters one
		// aggregate's events across partitions, where they are reordered.
		fields["partition_key"] = "must be the aggregate id " + aggregateID
	}
	if len(fields) > 1 {
		return errs.New(errs.CodeValidationFailed, "redpandabus: partition key does not follow the topic's registered rule").WithFields(fields)
	}
	return nil
}
