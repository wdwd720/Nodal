package redpandabus

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
)

// SASL mechanisms accepted in CP_REDPANDA_SASL_MECHANISM.
const (
	SASLPlain       = "PLAIN"
	SASLScramSHA256 = "SCRAM-SHA-256"
	SASLScramSHA512 = "SCRAM-SHA-512"
)

// defaultMaxPollRecords bounds one poll so a handler that is slow cannot
// hold a rebalance off for an unbounded batch.
const defaultMaxPollRecords = 500

// Client is the Kafka-protocol event.Bus, built on franz-go against
// Redpanda. It is the production transport; Loopback is its fake-mode twin
// and the two enforce the same partition rule (see ValidatePartitionKey).
//
// One franz-go client produces; each Subscribe gets its own client because a
// consumer-group client belongs to exactly one group.
type Client struct {
	opts     Options
	base     []kgo.Opt // connection options shared by producer and consumers
	producer *kgo.Client

	mu       sync.Mutex
	closed   bool
	groups   map[string]bool // "topic\x00group" already subscribed
	consumer []*kgo.Client
	wg       sync.WaitGroup
}

var _ event.Bus = (*Client)(nil)

// Open is the composition root's single entry point: it returns the bus the
// environment and provider mode call for and never silently substitutes one
// for the other. Fake mode gets Loopback, which NewLoopback refuses outside
// LOCAL/TEST/DEV; every other mode gets the real client, which fails to
// build unless a broker actually answers.
//
// There is deliberately no fallback from the real client to Loopback. A
// market-data bus that appears to work and delivers nothing is worse than
// one that will not start: the first is discovered days later by a strategy
// that has been trading on silence.
func Open(ctx context.Context, env config.Environment, mode config.ProviderMode, cfg config.RedpandaConfig, resolver config.Resolver, opts Options) (event.Bus, error) {
	switch mode {
	case config.ProviderModeFake:
		return NewLoopback(env, opts)
	case config.ProviderModeSandbox, config.ProviderModeLive:
		return New(ctx, cfg, resolver, opts)
	default:
		return nil, fmt.Errorf("redpandabus: unknown provider mode %q", mode)
	}
}

// New builds the franz-go bus and proves the brokers are reachable before
// returning: an unreachable broker is a startup failure, not a state to
// discover on the first publish.
//
// SASL credentials are resolved once from their SecretRefs and never logged;
// the franz-go logger adapter is level-limited and carries no credentials.
func New(ctx context.Context, cfg config.RedpandaConfig, resolver config.Resolver, opts Options) (*Client, error) {
	if len(cfg.Brokers) == 0 {
		return nil, ErrBrokersRequired
	}
	opts = opts.withDefaults()
	base := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID(opts.ClientID),
		kgo.WithLogger(&kgoLogger{log: opts.Logger}),
	}
	if cfg.RequireTLS {
		base = append(base, kgo.DialTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	}
	if cfg.SASLMechanism != "" {
		mech, err := saslMechanism(ctx, cfg, resolver)
		if err != nil {
			return nil, err
		}
		base = append(base, kgo.SASL(mech))
	}

	produce := append(slicesClone(base),
		// Exactly the durability the relay assumes: it marks an outbox row
		// published when Publish returns nil, so nil must mean every in-sync
		// replica has the record. The idempotent producer (franz-go's
		// default) makes a retried batch land once and in order.
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProduceRequestTimeout(opts.ProduceTimeout),
		kgo.RecordDeliveryTimeout(opts.ProduceTimeout),
	)
	if opts.AllowAutoTopicCreation {
		produce = append(produce, kgo.AllowAutoTopicCreation())
	}
	producer, err := kgo.NewClient(produce...)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "redpandabus: build producer")
	}
	pctx, cancel := context.WithTimeout(ctx, opts.ProduceTimeout)
	defer cancel()
	if err := producer.Ping(pctx); err != nil {
		producer.Close()
		return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "redpandabus: brokers are unreachable").
			WithField("brokers", strings.Join(cfg.Brokers, ","))
	}
	return &Client{opts: opts, base: base, producer: producer, groups: map[string]bool{}}, nil
}

func saslMechanism(ctx context.Context, cfg config.RedpandaConfig, resolver config.Resolver) (sasl.Mechanism, error) {
	if resolver == nil {
		return nil, errors.New("redpandabus: secret resolver is required for SASL")
	}
	user, err := resolver.Resolve(ctx, cfg.SASLUsernameRef)
	if err != nil {
		return nil, fmt.Errorf("redpandabus: resolve sasl username: %w", err)
	}
	pass, err := resolver.Resolve(ctx, cfg.SASLPasswordRef)
	if err != nil {
		// Deliberately not %w on anything that could render the secret.
		return nil, errors.New("redpandabus: resolve sasl password failed")
	}
	if user == "" || pass == "" {
		return nil, errors.New("redpandabus: sasl username and password must both resolve to a value")
	}
	switch strings.ToUpper(cfg.SASLMechanism) {
	case SASLScramSHA256:
		return scram.Auth{User: user, Pass: pass}.AsSha256Mechanism(), nil
	case SASLScramSHA512:
		return scram.Auth{User: user, Pass: pass}.AsSha512Mechanism(), nil
	case SASLPlain:
		return plain.Auth{User: user, Pass: pass}.AsMechanism(), nil
	default:
		return nil, fmt.Errorf("redpandabus: unsupported sasl mechanism %q (want %s, %s or %s)",
			cfg.SASLMechanism, SASLScramSHA256, SASLScramSHA512, SASLPlain)
	}
}

// Publish produces one record and returns only once the brokers acknowledged
// it. Any produce failure is returned; none of them is ever reported as a
// successful publish, because the relay would then mark the outbox row
// published and the event would be lost with no trace.
//
// It also always returns within ProduceTimeout. That bound has to be
// enforced here rather than left to franz-go: a broker that holds the
// connection open and never answers leaves the batch permanently in flight,
// where neither RecordDeliveryTimeout (which only measures a batch that is
// not in flight) nor the produce request's own timeout field (which only a
// responsive broker honors) ever fires, and ProduceSync blocks for as long
// as the broker stays silent.
//
// A timeout is reported as a failure even though the record may still be
// delivered afterwards. That asymmetry is deliberate: an unknown outcome
// reported as failure costs a duplicate, which at-least-once delivery and
// dedup on event_id already absorb, while an unknown outcome reported as
// success costs the event.
func (c *Client) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if topic == "" {
		return errors.New("redpandabus: topic is required")
	}
	if err := ValidatePartitionKey(topic, key, headers); err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	rec := &kgo.Record{Topic: topic, Key: []byte(key), Value: value, Headers: recordHeaders(headers)}
	pctx, cancel := context.WithTimeout(ctx, c.opts.ProduceTimeout)
	defer cancel()
	acked := make(chan error, 1)
	c.producer.Produce(pctx, rec, func(_ *kgo.Record, err error) { acked <- err })
	select {
	case err := <-acked:
		if err != nil {
			return errs.Wrap(err, errs.CodeProviderUnavailable, "redpandabus: publish").
				WithFields(map[string]any{"topic": topic, "partition_key": key})
		}
		return nil
	case <-pctx.Done():
		return errs.Wrap(pctx.Err(), errs.CodeProviderUnavailable,
			"redpandabus: publish was not acknowledged in time; the record may or may not have landed, so the caller must retry and consumers dedup").
			WithFields(map[string]any{"topic": topic, "partition_key": key, "produce_timeout": c.opts.ProduceTimeout.String()})
	}
}

// Subscribe joins the consumer group for (topic, group) and starts consuming
// in the background until ctx is done or Close is called. Auto-commit is
// off: an offset moves only after the handler returned nil, which is what
// makes delivery at-least-once rather than at-most-once. Handlers must
// therefore be idempotent (event.Inbox for Postgres consumers, dedup_id for
// stream consumers).
func (c *Client) Subscribe(ctx context.Context, topic, group string, h event.Handler) error {
	if h == nil || topic == "" || group == "" {
		return errors.New("redpandabus: topic, group and handler are required")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	gk := topic + "\x00" + group
	if c.groups[gk] {
		c.mu.Unlock()
		return fmt.Errorf("redpandabus: group %q already subscribed to %q", group, topic)
	}

	opts := append(slicesClone(c.base),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topic),
		kgo.DisableAutoCommit(),
		kgo.SessionTimeout(c.opts.SessionTimeout),
		// A rebalance can only happen between polls, never while a handler
		// is mid-record, so a partition is never reassigned under a handler
		// whose work has not been committed yet.
		kgo.BlockRebalanceOnPoll(),
		kgo.OnPartitionsRevoked(func(rctx context.Context, _ *kgo.Client, revoked map[string][]int32) {
			c.opts.Logger.InfoContext(rctx, "consumer group partitions revoked",
				slog.String("topic", topic), slog.String("group", group), slog.Int("topics", len(revoked)))
		}),
	)
	if c.opts.AllowAutoTopicCreation {
		opts = append(opts, kgo.AllowAutoTopicCreation())
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		c.mu.Unlock()
		return errs.Wrap(err, errs.CodeProviderUnavailable, "redpandabus: build consumer")
	}
	c.groups[gk] = true
	c.consumer = append(c.consumer, cl)
	c.wg.Add(1)
	c.mu.Unlock()

	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			delete(c.groups, gk)
			c.mu.Unlock()
		}()
		c.consume(ctx, cl, topic, group, h)
	}()
	return nil
}

// consume is the poll loop of one subscription.
func (c *Client) consume(ctx context.Context, cl *kgo.Client, topic, group string, h event.Handler) {
	log := c.opts.Logger.With(slog.String("topic", topic), slog.String("group", group))
	for {
		if ctx.Err() != nil {
			return
		}
		fetches := cl.PollRecords(ctx, defaultMaxPollRecords)
		// Every path out of a poll must unblock rebalancing, or the group
		// stalls for every member.
		stop := c.handleFetches(ctx, cl, fetches, topic, group, h, log)
		cl.AllowRebalance()
		if stop {
			return
		}
	}
}

// handleFetches processes one poll and reports whether the subscription must
// stop.
func (c *Client) handleFetches(ctx context.Context, cl *kgo.Client, fetches kgo.Fetches, topic, group string, h event.Handler, log *slog.Logger) bool {
	if fetches.IsClientClosed() || ctx.Err() != nil {
		return true
	}
	for _, fe := range fetches.Errors() {
		if errors.Is(fe.Err, context.Canceled) || errors.Is(fe.Err, context.DeadlineExceeded) {
			return true
		}
		// A broker lost mid-stream shows up here. franz-go reconnects and
		// re-fetches from the uncommitted offset on its own, so the honest
		// response is to report it and keep polling: nothing was committed,
		// so nothing was lost.
		log.WarnContext(ctx, "fetch error; the client will retry from the uncommitted offset",
			slog.String("fetch_topic", fe.Topic), slog.Int("partition", int(fe.Partition)), slog.String("error", fe.Err.Error()))
		if c.opts.OnError != nil {
			c.opts.OnError(topic, group, fe.Err)
		}
	}

	var (
		mu       sync.Mutex
		commit   []*kgo.Record
		gaveUp   bool
		partWG   sync.WaitGroup
		anyRecrd bool
	)
	fetches.EachPartition(func(p kgo.FetchTopicPartition) {
		recs := p.Records
		if len(recs) == 0 {
			return
		}
		anyRecrd = true
		// One goroutine per partition: partitions are independent, and
		// within a partition records are handled strictly in offset order,
		// which is the ordering guarantee Kafka actually makes.
		partWG.Add(1)
		go func() {
			defer partWG.Done()
			done := make([]*kgo.Record, 0, len(recs))
			for _, r := range recs {
				ok, giveUp := c.deliver(ctx, topic, group, r, h)
				if !ok {
					// Stop this partition here. Never commit past a record
					// that was not handled, or a restart would skip it.
					if giveUp {
						mu.Lock()
						gaveUp = true
						mu.Unlock()
					}
					break
				}
				done = append(done, r)
			}
			mu.Lock()
			commit = append(commit, done...)
			mu.Unlock()
		}()
	})
	partWG.Wait()

	if len(commit) > 0 {
		if err := cl.CommitRecords(context.WithoutCancel(ctx), commit...); err != nil {
			// Not fatal: the records were handled, and an uncommitted offset
			// means they are redelivered, which is exactly the at-least-once
			// contract handlers are already idempotent against.
			log.ErrorContext(ctx, "commit failed; those records will be redelivered", slog.String("error", err.Error()))
			if c.opts.OnError != nil {
				c.opts.OnError(topic, group, err)
			}
		}
	}
	if gaveUp {
		return true
	}
	if !anyRecrd && ctx.Err() != nil {
		return true
	}
	return false
}

// deliver runs the handler until it succeeds, ctx ends, or the retry bound is
// reached. ok reports whether the record was handled and may be committed;
// giveUp distinguishes "retries exhausted" from "shutting down".
func (c *Client) deliver(ctx context.Context, topic, group string, r *kgo.Record, h event.Handler) (ok, giveUp bool) {
	msg := messageOf(r)
	for attempt := 1; ; attempt++ {
		m := msg
		m.Value = append([]byte(nil), msg.Value...)
		m.Headers = maps.Clone(msg.Headers)
		err := h(ctx, m)
		if err == nil {
			return true, false
		}
		if ctx.Err() != nil {
			return false, false
		}
		c.opts.Logger.WarnContext(ctx, "handler failed; redelivering",
			slog.String("topic", topic), slog.String("group", group),
			slog.Int64("offset", r.Offset), slog.Int("attempt", attempt))
		if c.opts.MaxHandlerRetries > 0 && attempt > c.opts.MaxHandlerRetries {
			if c.opts.OnError != nil {
				c.opts.OnError(topic, group, fmt.Errorf("redpandabus: handler exhausted retries on %s: %w", msg.ID, err))
			}
			return false, true
		}
		timer := time.NewTimer(backoffFor(c.opts.RetryBackoff, attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, false
		case <-timer.C:
		}
	}
}

// Close stops every subscription, flushes nothing (Publish is already
// synchronous) and releases the clients. Consumers leave their group
// cooperatively so the remaining members rebalance once rather than waiting
// out a session timeout.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	consumers := c.consumer
	c.consumer = nil
	c.mu.Unlock()

	for _, cl := range consumers {
		cl.CloseAllowingRebalance()
	}
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		c.producer.Close()
		return ctx.Err()
	}
	c.producer.Close()
	return nil
}

// messageOf converts a Kafka record into the transport-neutral message.
func messageOf(r *kgo.Record) event.Message {
	headers := make(map[string]string, len(r.Headers))
	for _, h := range r.Headers {
		headers[h.Key] = string(h.Value)
	}
	return event.Message{
		ID:      messageID(headers, r.Topic, r.Partition, r.Offset),
		Topic:   r.Topic,
		Key:     string(r.Key),
		Value:   append([]byte(nil), r.Value...),
		Headers: headers,
	}
}

func recordHeaders(headers map[string]string) []kgo.RecordHeader {
	if len(headers) == 0 {
		return nil
	}
	out := make([]kgo.RecordHeader, 0, len(headers))
	for k, v := range headers {
		out = append(out, kgo.RecordHeader{Key: k, Value: []byte(v)})
	}
	return out
}

func slicesClone(in []kgo.Opt) []kgo.Opt {
	out := make([]kgo.Opt, len(in), len(in)+8)
	copy(out, in)
	return out
}

// kgoLogger adapts franz-go's logger onto slog. It logs at debug and above
// only through the injected logger, and passes franz-go's key/value pairs
// through untouched: franz-go never puts credentials in them, and this
// adapter adds none of its own.
type kgoLogger struct{ log *slog.Logger }

func (l *kgoLogger) Level() kgo.LogLevel { return kgo.LogLevelWarn }

func (l *kgoLogger) Log(level kgo.LogLevel, msg string, keyvals ...any) {
	var lvl slog.Level
	switch level {
	case kgo.LogLevelError:
		lvl = slog.LevelError
	case kgo.LogLevelWarn:
		lvl = slog.LevelWarn
	case kgo.LogLevelInfo:
		lvl = slog.LevelInfo
	default:
		lvl = slog.LevelDebug
	}
	l.log.Log(context.Background(), lvl, "redpanda: "+msg, keyvals...)
}
