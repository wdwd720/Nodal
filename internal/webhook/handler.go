package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/observability"
)

// Verifier is the provider adapter side of the pipeline: it verifies the
// signature of a raw delivery and parses it into E. It must return an error
// wrapping ErrSignatureInvalid, ErrTimestampOutOfTolerance or ErrMalformed;
// any other error is treated as a signature failure (fail closed).
type Verifier[E Event] interface {
	Name() string
	ParseWebhook(ctx context.Context, raw []byte, headers http.Header) (E, error)
}

// Dispatcher is the domain side: it applies a verified, deduplicated event
// inside the inbox transaction and writes its own outbox event there. An
// error rolls everything back and marks the event FAILED (re-processable).
type Dispatcher[E Event] interface {
	Dispatch(ctx context.Context, tx pgx.Tx, ev E) (Disposition, error)
}

// ArchiveWriter preserves the raw request (step 1) and returns a reference
// stored in provider_events.raw_ref. The object archive provides the
// production implementation; a Put failure refuses the delivery (503) so
// the provider redelivers once the archive is back.
type ArchiveWriter interface {
	Put(ctx context.Context, key, contentType string, body []byte) (ref string, err error)
}

// Defaults.
const (
	DefaultMaxBodyBytes = 256 << 10
	DefaultTolerance    = 300 * time.Second
	DefaultSchema       = 1
)

// Security event kinds written by the pipeline.
const (
	SecurityEventSignatureFailed = "webhook_signature_failed"
	SecurityEventTimestampStale  = "webhook_timestamp_stale"
	SecurityEventMalformed       = "webhook_malformed"
	SecurityEventPayloadMismatch = "webhook_payload_mismatch"
)

// Config wires a Handler.
type Config[E Event] struct {
	Verifier   Verifier[E]
	Dispatcher Dispatcher[E]
	Archive    ArchiveWriter
	DB         *db.DB
	Inbox      *event.Inbox
	Clock      clock.Clock
	// MaxBodyBytes caps the request body; larger bodies answer 413 before
	// any verification. Default DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// Tolerance bounds |now − SignedAt| for deliveries whose signature
	// carries a timestamp. Default DefaultTolerance. Zero is refused.
	Tolerance time.Duration
	// SchemaVersion is recorded on the inbox row. Default DefaultSchema.
	SchemaVersion int
}

// Handler is the HTTP endpoint of one provider's webhooks.
type Handler[E Event] struct {
	cfg Config[E]
}

// NewHandler validates the configuration and applies defaults.
func NewHandler[E Event](cfg Config[E]) (*Handler[E], error) {
	if cfg.Verifier == nil || cfg.Dispatcher == nil || cfg.Archive == nil || cfg.DB == nil || cfg.Inbox == nil {
		return nil, errs.New(errs.CodeValidationFailed, "webhook: verifier, dispatcher, archive, db and inbox are required")
	}
	if cfg.Verifier.Name() == "" {
		return nil, errs.New(errs.CodeValidationFailed, "webhook: verifier name is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System()
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.Tolerance == 0 {
		cfg.Tolerance = DefaultTolerance
	}
	if cfg.Tolerance < 0 {
		return nil, errs.New(errs.CodeValidationFailed, "webhook: tolerance must be positive")
	}
	if cfg.SchemaVersion <= 0 {
		cfg.SchemaVersion = DefaultSchema
	}
	return &Handler[E]{cfg: cfg}, nil
}

// Outcome names what the pipeline did with a delivery.
type Outcome string

// Outcomes.
const (
	OutcomeProcessed        Outcome = "processed"
	OutcomeIgnored          Outcome = "ignored"
	OutcomeDuplicate        Outcome = "duplicate"
	OutcomeInProgress       Outcome = "in_progress"
	OutcomeFailed           Outcome = "failed"
	OutcomeRejected         Outcome = "rejected"
	OutcomeTooLarge         Outcome = "too_large"
	OutcomeMethodNotAllowed Outcome = "method_not_allowed"
	OutcomeUnavailable      Outcome = "unavailable"
)

// Result is the pipeline's decision for one delivery.
type Result struct {
	Status  int
	Outcome Outcome
	EventID string
	Reason  string
}

// RequestMeta is the transport context recorded on security events.
type RequestMeta struct {
	RemoteIP  string
	UserAgent string
	RequestID string
}

// ServeHTTP implements http.Handler.
func (h *Handler[E]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		writeResult(w, Result{Status: http.StatusMethodNotAllowed, Outcome: OutcomeMethodNotAllowed})
		return
	}
	meta := RequestMeta{RemoteIP: remoteIP(r.RemoteAddr), UserAgent: r.UserAgent(), RequestID: observability.RequestID(ctx)}
	if meta.RequestID == "" {
		meta.RequestID = observability.NewRequestID()
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeResult(w, Result{Status: http.StatusRequestEntityTooLarge, Outcome: OutcomeTooLarge})
			return
		}
		writeResult(w, Result{Status: http.StatusBadRequest, Outcome: OutcomeRejected, Reason: "unreadable body"})
		return
	}
	res := h.Handle(ctx, raw, r.Header, meta)
	writeResult(w, res)
}

// writeResult answers with a minimal JSON body that never echoes internals.
func writeResult(w http.ResponseWriter, res Result) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(res.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"received": res.Status < 300, "outcome": string(res.Outcome)})
}

func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil || ip.Zone() != "" {
		return ""
	}
	return ip.String()
}

// Handle runs the pipeline for an already size-capped body. It is exported
// so transports other than net/http (tests, replay tooling) can drive it.
func (h *Handler[E]) Handle(ctx context.Context, raw []byte, headers http.Header, meta RequestMeta) Result {
	log := observability.LoggerFrom(ctx).With("provider", h.cfg.Verifier.Name(), "request_id", meta.RequestID)
	sum := sha256.Sum256(raw)
	hexHash := event.HashPayload(raw)

	ev, err := h.cfg.Verifier.ParseWebhook(ctx, raw, headers)
	if err != nil {
		kind, reason := classifyVerifyError(err)
		h.securityEvent(ctx, kind, "HIGH", meta, map[string]any{"reason": reason, "payload_hash": hexHash, "body_bytes": len(raw)})
		log.WarnContext(ctx, "webhook rejected", "kind", kind, "reason", reason)
		return Result{Status: http.StatusBadRequest, Outcome: OutcomeRejected, Reason: reason}
	}
	ident := ev.WebhookIdentity()
	switch {
	case ident.Provider != h.cfg.Verifier.Name():
		return h.reject(ctx, log, meta, SecurityEventMalformed, "event provider does not match the verifier", hexHash)
	case strings.TrimSpace(ident.EventID) == "" || len(ident.EventID) > event.MaxInboxKeyLength:
		return h.reject(ctx, log, meta, SecurityEventMalformed, "event id missing or too long", hexHash)
	case strings.TrimSpace(ident.EventType) == "":
		return h.reject(ctx, log, meta, SecurityEventMalformed, "event type missing", hexHash)
	}
	now := h.cfg.Clock.Now().UTC()
	if !ident.SignedAt.IsZero() {
		if delta := now.Sub(ident.SignedAt); delta > h.cfg.Tolerance || delta < -h.cfg.Tolerance {
			return h.reject(ctx, log, meta, SecurityEventTimestampStale, "signature timestamp outside tolerance", hexHash)
		}
	}
	log = log.With("provider_event_id", ident.EventID, "provider_event_type", ident.EventType)

	rawRef, err := h.cfg.Archive.Put(ctx, archiveKey(ident, hexHash), "application/json", raw)
	if err != nil {
		log.ErrorContext(ctx, "webhook archive unavailable", "error", err.Error())
		return Result{Status: http.StatusServiceUnavailable, Outcome: OutcomeUnavailable, EventID: ident.EventID, Reason: "archive unavailable"}
	}

	var disposition Disposition
	var outcome event.Outcome
	err = h.cfg.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO provider_events (id, provider, event_type, provider_event_id, received_at, provider_published_at,
				payload_hash, raw_ref, signature_verified, processing_status, request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, 'RECEIVED', $9)
			ON CONFLICT (provider, provider_event_id) DO NOTHING`,
			id.New[id.Any](), ident.Provider, ident.EventType, ident.EventID, now, nullTime(ident.PublishedAt), sum[:], rawRef, meta.RequestID); err != nil {
			return errs.Wrap(err, errs.CodeInternal, "webhook: insert provider event")
		}
		o, err := h.cfg.Inbox.ProcessHashed(ctx, tx, ident.Provider, ident.EventID, h.cfg.SchemaVersion, hexHash, func(ctx context.Context, tx pgx.Tx) error {
			d, err := h.cfg.Dispatcher.Dispatch(ctx, tx, ev)
			if err != nil {
				return err
			}
			if d != Applied && d != Ignored {
				return errs.Newf(errs.CodeInternal, "webhook: dispatcher returned unknown disposition %q", d)
			}
			disposition = d
			_, err = tx.Exec(ctx, `UPDATE provider_events SET processing_status = $3, processed_at = $4, error = NULL, request_id = $5
				WHERE provider = $1 AND provider_event_id = $2`, ident.Provider, ident.EventID, string(d), h.cfg.Clock.Now().UTC(), meta.RequestID)
			if err != nil {
				return errs.Wrap(err, errs.CodeInternal, "webhook: update provider event")
			}
			return nil
		})
		if err != nil {
			return err
		}
		outcome = o
		return nil
	})
	switch {
	case err == nil:
	case errs.CodeOf(err) == errs.CodeConflict:
		return h.reject(ctx, log, meta, SecurityEventPayloadMismatch, "event id reused with a different payload", hexHash)
	case errs.CodeOf(err) == errs.CodeIdempotencyInProgress:
		return Result{Status: http.StatusConflict, Outcome: OutcomeInProgress, EventID: ident.EventID, Reason: "in progress"}
	default:
		h.recordFailure(ctx, ident, err, meta, now, sum[:], rawRef)
		log.ErrorContext(ctx, "webhook processing failed", "error", err.Error())
		return Result{Status: http.StatusInternalServerError, Outcome: OutcomeFailed, EventID: ident.EventID, Reason: safeReason(err)}
	}
	if outcome == event.Duplicate {
		log.InfoContext(ctx, "webhook duplicate acknowledged")
		return Result{Status: http.StatusOK, Outcome: OutcomeDuplicate, EventID: ident.EventID}
	}
	if disposition == Ignored {
		log.InfoContext(ctx, "webhook ignored")
		return Result{Status: http.StatusOK, Outcome: OutcomeIgnored, EventID: ident.EventID}
	}
	log.InfoContext(ctx, "webhook processed")
	return Result{Status: http.StatusOK, Outcome: OutcomeProcessed, EventID: ident.EventID}
}

func (h *Handler[E]) reject(ctx context.Context, log interface {
	WarnContext(context.Context, string, ...any)
}, meta RequestMeta, kind, reason, hexHash string,
) Result {
	h.securityEvent(ctx, kind, "HIGH", meta, map[string]any{"reason": reason, "payload_hash": hexHash})
	log.WarnContext(ctx, "webhook rejected", "kind", kind, "reason", reason)
	return Result{Status: http.StatusBadRequest, Outcome: OutcomeRejected, Reason: reason}
}

// classifyVerifyError maps a verifier error to a security event kind.
func classifyVerifyError(err error) (kind, reason string) {
	switch {
	case errors.Is(err, ErrTimestampOutOfTolerance):
		return SecurityEventTimestampStale, "signature timestamp outside tolerance"
	case errors.Is(err, ErrMalformed):
		return SecurityEventMalformed, "malformed payload"
	default:
		return SecurityEventSignatureFailed, "signature verification failed"
	}
}

// recordFailure persists FAILED on the evidence and inbox rows in its own
// transaction after the processing transaction rolled back (step 10).
func (h *Handler[E]) recordFailure(ctx context.Context, ident Identity, cause error, meta RequestMeta, now time.Time, hash []byte, rawRef string) {
	text := safeReason(cause)
	err := h.cfg.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO provider_events (id, provider, event_type, provider_event_id, received_at, provider_published_at,
				payload_hash, raw_ref, signature_verified, processing_status, processed_at, error, request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, 'FAILED', $9, $10, $11)
			ON CONFLICT (provider, provider_event_id) DO UPDATE
			SET processing_status = 'FAILED', processed_at = EXCLUDED.processed_at, error = EXCLUDED.error, request_id = EXCLUDED.request_id
			WHERE provider_events.processing_status <> 'PROCESSED'`,
			id.New[id.Any](), ident.Provider, ident.EventType, ident.EventID, now, nullTime(ident.PublishedAt), hash, rawRef,
			h.cfg.Clock.Now().UTC(), text, meta.RequestID); err != nil {
			return err
		}
		return h.cfg.Inbox.MarkFailed(ctx, tx, ident.Provider, ident.EventID, h.cfg.SchemaVersion, hex.EncodeToString(hash), cause)
	})
	if err != nil {
		observability.LoggerFrom(ctx).ErrorContext(ctx, "webhook: could not record failure",
			"provider", ident.Provider, "provider_event_id", ident.EventID, "error", err.Error())
	}
}

// securityEvent writes a security_events row outside any transaction; it
// is the only persistence a rejected delivery leaves behind.
func (h *Handler[E]) securityEvent(ctx context.Context, kind, severity string, meta RequestMeta, detail map[string]any) {
	detail["provider"] = h.cfg.Verifier.Name()
	body, err := json.Marshal(detail)
	if err != nil {
		body = []byte(`{}`)
	}
	_, err = h.cfg.DB.Exec(ctx, `INSERT INTO security_events (id, kind, severity, detail, ip, user_agent, request_id, occurred_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::inet, NULLIF($6, ''), NULLIF($7, ''), $8)`,
		id.New[id.Any](), kind, severity, body, meta.RemoteIP, truncate(meta.UserAgent, 512), meta.RequestID, h.cfg.Clock.Now().UTC())
	if err != nil {
		observability.LoggerFrom(ctx).ErrorContext(ctx, "webhook: could not record security event", "kind", kind, "error", err.Error())
	}
}

func archiveKey(ident Identity, hexHash string) string {
	return "webhooks/" + ident.Provider + "/" + ident.EventID + "/" + hexHash[:16] + ".json"
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// safeReason renders an error for storage and responses: code and
// client-safe detail for *errs.Error, the text otherwise, bounded.
func safeReason(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if e, ok := errs.As(err); ok {
		s = string(e.Code)
		if e.Detail != "" {
			s += ": " + e.Detail
		}
	}
	return truncate(s, 1000)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
