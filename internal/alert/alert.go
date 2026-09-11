// Package alert delivers operational alerts somewhere a human will see them.
//
// # What this is for
//
// `reconciliation.Metrics.Raise` records an alert three ways: an in-process
// counter, an OpenTelemetry instrument, and a callback. On the deployed system
// the counter dies with the process, the meter is a no-op while
// `CP_TELEMETRY_OTLP_ENDPOINT` is unset, and the callback had no production
// caller anywhere in the repository. F-118 is that finding: **nothing pages,
// and a sweep for every paging vendor in the tree returned one hit, in a
// Terraform variable description.**
//
// This package is the callback's production caller.
//
// # Why a webhook and not a vendor
//
// The launch tier may not incur a fixed monthly cost, which rules out most of
// what "alerting" normally means. What it does not rule out is an outbound
// HTTPS POST, which every free tier of every relevant service accepts: Slack
// and Discord incoming webhooks, ntfy, healthchecks.io, a Cloudflare Worker, an
// email relay someone already has. So the destination is a URL the deployment
// supplies and this package commits to no vendor. That is the architecture
// deriving the answer rather than a preference: the constraint is "$0 and it
// must leave the process", and a webhook is the only shape that satisfies both
// without choosing for the operator.
//
// "Accepts a POST" is not "accepts any POST". Slack wants {"text": ...},
// Discord wants {"content": ...} and refuses anything else with a 400; ntfy
// wants a text body and reads the title from a header. A destination that
// answers 400 to every alert is the original finding with a URL attached, so
// the sink speaks each of those shapes and picks one from the host unless
// told otherwise (Format). The generic shape -- the Event as JSON -- is for
// the Worker or relay somebody writes themselves.
//
// # Three rules this package will not break
//
//  1. **An alert must never fail a financial transaction.** `Raise` is called
//     from inside reconciliation, which runs inside transactions that move
//     money. Delivery is therefore asynchronous over a bounded queue, and a
//     full queue drops rather than blocks. A dropped alert is bad; a wedged
//     ledger write because a webhook host is slow is worse.
//
//  2. **Only allowlisted fields leave the process.** Alerts today carry
//     identifiers and booleans, but this is an egress path and the set of
//     things `Raise` might carry tomorrow is not fixed. Unknown keys are
//     dropped and counted rather than forwarded, so a new field cannot silently
//     start leaving the building.
//
//  3. **Silence is reported.** When no destination is configured this says so
//     once, at WARN, naming what will not happen -- the same shape
//     `runOpsRetention` uses. A control that is off must not be quiet about it.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Severity is the alert's level. It mirrors reconciliation.Severity without
// importing it: this package must not depend on the packages that raise.
type Severity string

const (
	// SEV1 is a financial-integrity violation.
	SEV1 Severity = "SEV1"
	// SEV2 is everything else worth waking up for.
	SEV2 Severity = "SEV2"
)

// Rank orders severities so a minimum threshold can be compared. Higher is
// more severe.
func (s Severity) Rank() int {
	if s == SEV1 {
		return 2
	}
	return 1
}

// Event is one alert, in the shape that leaves the process.
type Event struct {
	Name     string            `json:"name"`
	Severity Severity          `json:"severity"`
	Detail   string            `json:"detail,omitempty"`
	RecordID string            `json:"record_id,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
	At       time.Time         `json:"at"`

	// Environment and Service identify the sender, because a webhook with no
	// idea which deployment it came from is a page nobody can act on.
	Environment string `json:"environment"`
	Service     string `json:"service"`

	// DroppedFields counts keys removed by the allowlist. Sent rather than
	// hidden: an operator seeing a non-zero count knows the alert carried
	// something this package would not forward, and can go and look.
	DroppedFields int `json:"dropped_fields,omitempty"`
}

// allowedFields is every key an alert may carry out of the process.
//
// Derived from the call sites rather than invented: `deposit_id`, `signature`,
// `material`, `blocks_new_risk`, `attempt_id`, `order_id`, `account_id`. They
// are identifiers and booleans, which is what makes them safe to send. A key
// added to a Raise call tomorrow is dropped until somebody adds it here and
// decides it is safe to send.
var allowedFields = map[string]bool{
	"account_id":      true,
	"attempt_id":      true,
	"blocks_new_risk": true,
	"deposit_id":      true,
	"material":        true,
	"order_id":        true,
	"signature":       true,
}

// AllowedFields returns the allowlist, for tests and for the operator-facing
// documentation to render.
func AllowedFields() []string {
	out := make([]string, 0, len(allowedFields))
	for k := range allowedFields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// filterFields keeps the allowlisted keys and counts the rest.
func filterFields(in map[string]any) (map[string]string, int) {
	if len(in) == 0 {
		return nil, 0
	}
	out := make(map[string]string, len(in))
	dropped := 0
	for k, v := range in {
		if !allowedFields[k] {
			dropped++
			continue
		}
		out[k] = fmt.Sprintf("%v", v)
	}
	if len(out) == 0 {
		out = nil
	}
	return out, dropped
}

// Format is the payload shape a destination understands.
type Format string

const (
	// FormatAuto picks from the host: hooks.slack.com, discord.com and
	// ntfy.sh are recognised; anything else is generic.
	FormatAuto Format = "auto"
	// FormatGeneric posts the Event as JSON.
	FormatGeneric Format = "generic"
	// FormatSlack posts {"text": summary}. Mattermost and Rocket.Chat
	// incoming webhooks accept the same shape.
	FormatSlack Format = "slack"
	// FormatDiscord posts {"content": summary}, under Discord's 2000-char cap.
	FormatDiscord Format = "discord"
	// FormatNtfy posts the summary as a text body with Title, Priority and
	// Tags headers, which is what ntfy renders as a notification.
	FormatNtfy Format = "ntfy"
)

// Formats lists every accepted value, for validation and documentation.
func Formats() []Format {
	return []Format{FormatAuto, FormatGeneric, FormatSlack, FormatDiscord, FormatNtfy}
}

// Valid reports whether f is one of Formats.
func (f Format) Valid() bool {
	for _, k := range Formats() {
		if f == k {
			return true
		}
	}
	return false
}

// DetectFormat picks the payload shape for a host. Unknown hosts are generic,
// which is the shape that is at least never rejected for its structure.
func DetectFormat(host string) Format {
	h := strings.ToLower(host)
	switch {
	case h == "hooks.slack.com":
		return FormatSlack
	case h == "discord.com" || h == "discordapp.com" || strings.HasSuffix(h, ".discord.com"):
		return FormatDiscord
	case h == "ntfy.sh":
		return FormatNtfy
	}
	return FormatGeneric
}

// summaryLimit is under Discord's 2000-character content cap, the smallest
// of the recognised destinations; Slack and ntfy allow more.
const summaryLimit = 1900

// Summary renders the event as one line for destinations that show text:
//
//	[SEV1] STAGING nodal-api ledger_integrity_violation: <detail> (record <id>) k=v k=v (+n fields withheld)
//
// Fields are the allowlisted ones only, in key order so two renderings of one
// event are identical. Bounded, because a destination that truncates silently
// would cut the record id off the end.
func (e Event) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s %s %s", e.Severity, e.Environment, e.Service, e.Name)
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.RecordID != "" {
		fmt.Fprintf(&b, " (record %s)", e.RecordID)
	}
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%s", k, e.Fields[k])
	}
	if e.DroppedFields > 0 {
		fmt.Fprintf(&b, " (+%d fields withheld)", e.DroppedFields)
	}
	s := b.String()
	if r := []rune(s); len(r) > summaryLimit {
		s = string(r[:summaryLimit-1]) + "\u2026"
	}
	return s
}

// Sink delivers one event. Implementations must respect the context deadline.
type Sink interface {
	Deliver(ctx context.Context, e Event) error
	// Describe names the destination for a startup log line, without its
	// secret. "webhook api.example.com" rather than the URL.
	Describe() string
}

// Dispatcher accepts alerts from the raise path and delivers them elsewhere.
//
// Enqueue never blocks and never returns an error, because its callers are
// inside money-moving transactions and have nothing useful to do with either.
type Dispatcher struct {
	sink    Sink
	minSev  Severity
	timeout time.Duration
	log     *slog.Logger

	queue   chan Event
	wg      sync.WaitGroup
	closing atomic.Bool

	enqueued  atomic.Int64
	delivered atomic.Int64
	dropped   atomic.Int64
	failed    atomic.Int64
}

// Options configures a Dispatcher. Zero values are filled with the defaults
// below.
type Options struct {
	Sink        Sink
	MinSeverity Severity
	Timeout     time.Duration
	QueueSize   int
	Logger      *slog.Logger
}

const (
	defaultQueueSize = 256
	defaultTimeout   = 5 * time.Second
)

// NewDispatcher starts a Dispatcher and its delivery goroutine.
//
// The queue is deliberately small. It exists to decouple the raise path from
// one slow HTTP call, not to buffer an outage: a destination that is down for
// minutes will drop, and dropping loudly is the honest behaviour for an alert
// channel that cannot deliver.
func NewDispatcher(o Options) *Dispatcher {
	if o.QueueSize <= 0 {
		o.QueueSize = defaultQueueSize
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.MinSeverity == "" {
		o.MinSeverity = SEV2
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	d := &Dispatcher{
		sink:    o.Sink,
		minSev:  o.MinSeverity,
		timeout: o.Timeout,
		log:     o.Logger,
		queue:   make(chan Event, o.QueueSize),
	}
	d.wg.Add(1)
	go d.run()
	return d
}

// Enqueue offers an alert for delivery. It never blocks.
func (d *Dispatcher) Enqueue(e Event) {
	if d == nil || d.sink == nil || d.closing.Load() {
		return
	}
	if e.Severity.Rank() < d.minSev.Rank() {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	select {
	case d.queue <- e:
		d.enqueued.Add(1)
	default:
		// Full. Dropping is the deliberate choice: the alternative is blocking
		// a transaction that is moving money on a webhook host's latency.
		n := d.dropped.Add(1)
		d.log.Error("alert dropped: the delivery queue is full",
			"alert", e.Name, "severity", string(e.Severity), "dropped_total", n,
			"consequence", "this alert was logged but not delivered anywhere")
	}
}

func (d *Dispatcher) run() {
	defer d.wg.Done()
	for e := range d.queue {
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
		err := d.sink.Deliver(ctx, e)
		cancel()
		if err != nil {
			n := d.failed.Add(1)
			d.log.Error("alert delivery failed",
				"alert", e.Name, "severity", string(e.Severity), "error", err.Error(),
				"failed_total", n,
				"consequence", "this alert was logged but not delivered anywhere")
			continue
		}
		d.delivered.Add(1)
	}
}

// Close stops accepting alerts and waits for the queue to drain.
//
// Draining rather than discarding, because the alerts most worth delivering are
// the ones raised just before a shutdown.
func (d *Dispatcher) Close() {
	if d == nil || d.sink == nil {
		return
	}
	if d.closing.Swap(true) {
		return
	}
	close(d.queue)
	d.wg.Wait()
	d.log.Info("alert dispatcher stopped",
		"enqueued", d.enqueued.Load(), "delivered", d.delivered.Load(),
		"dropped", d.dropped.Load(), "failed", d.failed.Load())
}

// Stats returns delivery counters, for the health surface and for tests.
func (d *Dispatcher) Stats() (enqueued, delivered, dropped, failed int64) {
	if d == nil {
		return 0, 0, 0, 0
	}
	return d.enqueued.Load(), d.delivered.Load(), d.dropped.Load(), d.failed.Load()
}

// EventFrom builds an Event from the raise path's loose shape, applying the
// field allowlist. It is the only way an Event is constructed from outside.
func EventFrom(name string, sev Severity, detail, recordID string, fields map[string]any, at time.Time, env, service string) Event {
	f, dropped := filterFields(fields)
	return Event{
		Name: name, Severity: sev, Detail: detail, RecordID: recordID,
		Fields: f, At: at, Environment: env, Service: service, DroppedFields: dropped,
	}
}

// WebhookSink posts an alert as JSON to one URL.
//
// One attempt, bounded by the caller's context. No retry loop: a retry inside
// the delivery goroutine delays every alert behind it, and the queue in front
// is already the buffer. A destination that needs retries needs a destination
// that works.
type WebhookSink struct {
	url    string
	host   string
	format Format
	client *http.Client
}

// NewWebhookSink builds a sink for url. The host is kept separately so the
// destination can be named in logs without printing a URL that is usually a
// secret -- a Slack webhook URL IS its credential. An empty or auto format is
// resolved from the host once, here, so Describe can say what was chosen.
func NewWebhookSink(url, host string, format Format, client *http.Client) *WebhookSink {
	if client == nil {
		client = &http.Client{}
	}
	if format == "" || format == FormatAuto || !format.Valid() {
		format = DetectFormat(host)
	}
	return &WebhookSink{url: url, host: host, format: format, client: client}
}

// Describe names the destination and the shape it is spoken to in, without
// the secret.
func (w *WebhookSink) Describe() string { return "webhook " + w.host + " (" + string(w.format) + ")" }

// Format is the resolved payload shape.
func (w *WebhookSink) Format() Format { return w.format }

// encode renders e in the destination's shape and returns the body, the
// content type and any headers the shape carries.
func (w *WebhookSink) encode(e Event) ([]byte, string, http.Header, error) {
	hdr := http.Header{}
	switch w.format {
	case FormatSlack:
		b, err := json.Marshal(map[string]string{"text": e.Summary()})
		return b, "application/json", hdr, err
	case FormatDiscord:
		b, err := json.Marshal(map[string]string{"content": e.Summary()})
		return b, "application/json", hdr, err
	case FormatNtfy:
		hdr.Set("Title", "["+string(e.Severity)+"] "+e.Name)
		if e.Severity == SEV1 {
			hdr.Set("Priority", "5")
			hdr.Set("Tags", "rotating_light")
		} else {
			hdr.Set("Priority", "4")
			hdr.Set("Tags", "warning")
		}
		return []byte(e.Summary()), "text/plain; charset=utf-8", hdr, nil
	default:
		b, err := json.Marshal(e)
		return b, "application/json", hdr, err
	}
}

// Deliver posts the event.
func (w *WebhookSink) Deliver(ctx context.Context, e Event) error {
	body, contentType, hdr, err := w.encode(e)
	if err != nil {
		return fmt.Errorf("alert: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("alert: request: %w", err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("alert: post to %s: %w", w.host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is drained and discarded: a destination's reply is not this
	// system's business, and an undrained body leaks a connection.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("alert: %s answered %d", w.host, resp.StatusCode)
	}
	return nil
}
