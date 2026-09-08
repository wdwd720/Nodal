package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/reality/redpandabus"
)

// busKind is what a provider mode resolves to for THIS binary.
type busKind string

const (
	// busRedpanda is the real Kafka-protocol client; Publish returns only
	// after every in-sync replica acknowledged the record, which is the
	// durability the relay assumes before it marks a row published.
	busRedpanda busKind = "redpanda"
	// busLoopback is redpandabus.Loopback: in-process, no broker, no
	// subscriber outside this process.
	busLoopback busKind = "loopback"
)

// resolveBusBinding decides what the configured provider mode means for the
// relay, and fails closed.
//
// The relay is the one component for which the loopback bus is not a
// harmless development convenience. Every other binary that publishes also
// consumes in the same process, so a loopback still moves its own messages
// end to end. The relay only produces: its consumers — the API's SSE hub,
// the execution worker's waker, every projection — are other processes. A
// relay pointed at a loopback therefore claims rows, "publishes" them into a
// buffer nobody reads, and marks them published. The outbox drains, the
// events are gone, and the first symptom is a read model that quietly stops
// updating.
//
// So fake mode is refused unless an operator opted in explicitly, and the
// opt-in is itself refused outside LOCAL/TEST/DEV (config.Validate's
// RuleNoFakeProviders refuses the mode in STAGING/PROD before this is even
// reached; this is the second lock on the same door).
func resolveBusBinding(env config.Environment, mode config.ProviderMode, allowLoopback bool) (busKind, error) {
	switch mode {
	case config.ProviderModeSandbox, config.ProviderModeLive:
		return busRedpanda, nil
	case config.ProviderModeFake:
		if env.IsProductionLike() {
			return "", fmt.Errorf(
				"the event bus provider is in %q mode, which is never permitted in %s", mode, env,
			)
		}
		if !allowLoopback {
			return "", fmt.Errorf(
				"no real event bus binding for provider mode %q: the fake bus is an in-process loopback with no "+
					"subscriber outside this process, so every relayed event would be marked published and then discarded. "+
					"Point the worker at a broker (%s=sandbox|live with %s), or, in LOCAL/TEST/DEV only, accept that loss "+
					"deliberately with %s=true",
				mode, envVarEventBusMode, envVarRedpandaBrokers, envAllowLoopback,
			)
		}
		return busLoopback, nil
	default:
		return "", fmt.Errorf("unknown event bus provider mode %q (want %s, %s or %s)",
			mode, config.ProviderModeFake, config.ProviderModeSandbox, config.ProviderModeLive)
	}
}

// Config variable names quoted in the failure above. They belong to
// internal/config; they are named here only so the error tells an operator
// exactly which knob to turn.
const (
	envVarEventBusMode    = "CP_PROVIDER_EVENT_BUS_MODE"
	envVarRedpandaBrokers = "CP_REDPANDA_BROKERS"
)

// openBus resolves the binding and builds the bus. redpandabus.New proves
// the brokers answered a ping before returning, so an unreachable broker is
// a startup failure rather than something discovered on the first publish.
func (d *deps) openBus(ctx context.Context) (event.Bus, error) {
	// wire already resolved and refused; re-resolving here keeps openBus
	// honest for any caller that builds deps by hand.
	kind, err := resolveBusBinding(d.cfg.Env, d.cfg.Providers.EventBus.Mode, d.allowLoopback)
	if err != nil {
		return nil, err
	}
	if kind == busLoopback {
		d.log.Warn("relay-worker: publishing to the in-process loopback bus; relayed events are marked published and then lost with this process. "+
			"Never run this against a database whose events anything depends on.",
			"env", d.cfg.Env, "mode", d.cfg.Providers.EventBus.Mode, "opt_in", envAllowLoopback)
	}
	bus, err := redpandabus.Open(ctx, d.cfg.Env, d.cfg.Providers.EventBus.Mode, d.cfg.Redpanda, d.resolver,
		redpandabus.Options{
			ClientID: serviceName,
			Logger:   d.log,
			// Production topics are provisioned, and a publish to a topic
			// that is not provisioned must fail loudly and leave the row
			// unpublished rather than invent a stream nothing consumes.
			// Outside production-like environments a developer's broker is
			// empty and letting it create topics on first use is what makes
			// the local stack work at all.
			AllowAutoTopicCreation: !d.cfg.Env.IsProductionLike(),
		})
	if err != nil {
		return nil, fmt.Errorf("event bus: %w", err)
	}
	d.log.Info("relay-worker: event bus ready", "binding", string(kind),
		"mode", d.cfg.Providers.EventBus.Mode, "brokers", d.cfg.Redpanda.Brokers, "require_tls", d.cfg.Redpanda.RequireTLS)
	return &topicGuardBus{inner: bus, log: d.log, metrics: d.metrics}, nil
}

// topicGuardBus reports outbox rows whose topic internal/event does not
// register, and then publishes them anyway.
//
// The registry is authoritative and test/contract/eventtopics keeps every
// producer inside it, so such a row means something is genuinely wrong: a
// producer bypassed Outbox.Enqueue, or a topic was renamed without the
// registry following. Neither is a reason to drop a committed financial
// event. The row is published if the transport accepts it and stays
// unpublished and retryable if it does not — either way the defect is a WARN
// and a counter, never a silent deletion.
type topicGuardBus struct {
	inner   event.Bus
	log     *slog.Logger
	metrics *relayMetrics
}

var _ event.Bus = (*topicGuardBus)(nil)

func (b *topicGuardBus) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	if _, ok := event.Lookup(event.Topic(topic)); !ok {
		if b.metrics != nil {
			b.metrics.onUnregisteredTopic(ctx, topic)
		}
		b.log.WarnContext(ctx, "outbox row names a topic that is not in the event registry; publishing it anyway",
			"topic", topic,
			"event_id", headers[event.HeaderEventID],
			"event_type", headers[event.HeaderEventType],
			"source", headers[event.HeaderSource],
			"partition_key", key,
			"hint", "a producer that emits an unregistered topic is a defect; register it in internal/event or fix the producer")
	}
	return b.inner.Publish(ctx, topic, key, value, headers)
}

func (b *topicGuardBus) Subscribe(ctx context.Context, topic, group string, h event.Handler) error {
	return b.inner.Subscribe(ctx, topic, group, h)
}

func (b *topicGuardBus) Close(ctx context.Context) error { return b.inner.Close(ctx) }
