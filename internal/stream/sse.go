package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// DefaultMaxStreamsPerUser bounds how many streams one person may hold open at
// once.
//
// A stream is one request that never ends, and this deployment is one free
// instance. Four is a browser with a few tabs; a client that opens one per
// component, or a script that opens them in a loop, would otherwise hold every
// connection the process has and take the REST surface down with it. The
// refusal is 429 with Retry-After, not a silent close, so the client can tell
// the difference between "too many" and "broken".
const DefaultMaxStreamsPerUser = 4

// Resume supplies the events a reconnecting client missed, from durable
// storage, given the instant its Last-Event-ID encodes.
//
// It exists because a hub is memory. The replay buffer covers a redeploy for
// nobody and a five-minute disconnection only if the process stayed up. The
// notifications table does not have that problem, so the composition root
// passes a hook that reads it; a deployment that passes none simply resumes
// from the buffer, and says so by sending resync.
//
// truncated reports that there were more than the hook was willing to return,
// in which case the client is sent resync and refetches over REST.
type Resume func(ctx context.Context, p security.Principal, since time.Time) (events []Event, truncated bool, err error)

// Handler serves GET /v1/events/stream.
type Handler struct {
	hub       *Hub
	heartbeat time.Duration
	stillLive StillLive
	resume    Resume

	mu         sync.Mutex
	open       map[string]int
	maxPerUser int
}

// SetResume installs the durable resume hook. Passing nil leaves the handler
// resuming from the in-memory buffer alone.
func (h *Handler) SetResume(r Resume) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resume = r
}

// SetMaxStreamsPerUser overrides the concurrency cap. A value below one means
// unlimited, which is only correct where nothing else shares the process.
func (h *Handler) SetMaxStreamsPerUser(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.maxPerUser = n
}

// OpenStreams reports how many streams are open for subject, for a test and
// for an operator asking why the instance is busy.
func (h *Handler) OpenStreams(subject string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.open[subject]
}

// acquire takes one of the subject's stream slots, or reports that they have
// none left.
func (h *Handler) acquire(subject string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maxPerUser > 0 && h.open[subject] >= h.maxPerUser {
		return false
	}
	h.open[subject]++
	return true
}

func (h *Handler) release(subject string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[subject] <= 1 {
		delete(h.open, subject)
		return
	}
	h.open[subject]--
}

func (h *Handler) resumeHook() Resume {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resume
}

// StillLive re-checks that the session a stream belongs to is still valid, and
// returns an error when it is not.
//
// Revocation in this system is per request: auth.Manager re-reads the session
// row on every one, so a revoked session dies at the next call. An SSE stream is
// ONE request that never ends, so the per-request model had no purchase on it
// (F-116). A stolen cookie's stream kept delivering the victim's balance, order
// and deposit events after they logged out, after an operator revoked the
// session, and past the session's own absolute expiry -- while every runbook
// presents revocation as the containment for a leaked cookie.
//
// The principal cannot answer this on its own: it is a snapshot taken when the
// stream opened, and it carries no session expiry.
type StillLive func(ctx context.Context, sessionID string) error

// NewHandler returns an SSE handler; heartbeat comments keep proxies from
// closing idle connections.
//
// stillLive is re-checked on every heartbeat. Passing nil means the stream is
// never re-checked, which is only correct where there is no session to revoke;
// the composition root passes the session manager.
func NewHandler(hub *Hub, heartbeat time.Duration, stillLive StillLive) *Handler {
	if heartbeat <= 0 {
		heartbeat = 15 * time.Second
	}
	return &Handler{
		hub:        hub,
		heartbeat:  heartbeat,
		stillLive:  stillLive,
		open:       map[string]int{},
		maxPerUser: DefaultMaxStreamsPerUser,
	}
}

// ServeHTTP streams events to the authenticated principal until the client
// disconnects. Agents are refused: they consume the bus, not the UI stream.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := security.PrincipalFrom(r.Context())
	if !ok || p.ActorType == security.ActorAgent {
		errs.WriteProblem(w, errs.ToProblem(errs.New(errs.CodeUnauthenticated, "stream requires a user session"), r.URL.Path, r.Header.Get("X-Request-Id")))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		errs.WriteProblem(w, errs.ToProblem(errs.New(errs.CodeUnsupported, "streaming unsupported"), r.URL.Path, r.Header.Get("X-Request-Id")))
		return
	}
	after, err := ParseLastEventID(r.Header.Get("Last-Event-ID"))
	if err != nil {
		errs.WriteProblem(w, errs.ToProblem(errs.New(errs.CodeValidationFailed, err.Error()), r.URL.Path, r.Header.Get("X-Request-Id")))
		return
	}
	if !h.acquire(p.SubjectID) {
		prob := errs.ToProblem(errs.Newf(errs.CodeRateLimited,
			"this account already has %d streams open; close one before opening another", h.maxPerUser),
			r.URL.Path, r.Header.Get("X-Request-Id"))
		// Retry-After comes from errs.WriteProblem, which owns the mapping for
		// RATE_LIMITED; setting a second value here would be two answers to
		// one question.
		errs.WriteProblem(w, prob)
		return
	}
	defer h.release(p.SubjectID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// The browser's own reconnect delay. Stated rather than left to the
	// default because this instance sleeps: a client that reconnects instantly
	// against a cold start hammers it awake, and one that waits a minute makes
	// the product look broken.
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}

	// The durable half of resume. A time-encoded Last-Event-ID names an
	// instant, and the notifications written since it are in a table that
	// outlived whatever ended the last connection.
	replayedDurably := false
	if since, ok := EventTime(after); ok {
		if hook := h.resumeHook(); hook != nil {
			missed, truncated, err := hook(r.Context(), p, since)
			switch {
			case err != nil:
				observability.LoggerFrom(r.Context()).WarnContext(r.Context(),
					"stream: durable resume failed; falling back to resync",
					"since", since, "err", err.Error())
				_ = writeEvent(w, Event{ID: after, Type: TypeResync, OccurredAt: time.Now().UTC()})
			case truncated:
				// More than the hook would return. Saying resync is honest and
				// cheap; sending a partial backlog and calling it complete is
				// neither.
				_ = writeEvent(w, Event{ID: after, Type: TypeResync, OccurredAt: time.Now().UTC()})
				replayedDurably = true
			default:
				for _, e := range missed {
					if err := writeEvent(w, e); err != nil {
						return
					}
				}
				replayedDurably = true
			}
		}
	}

	sub, replay := h.hub.Subscribe(p, after, 256)
	defer h.hub.Unsubscribe(sub)
	for _, e := range replay {
		// The table is authoritative for notifications, so when it has just
		// been read the buffer's copies of the same rows are skipped rather
		// than sent twice. Everything else the buffer holds -- data.changed,
		// order and deposit transitions, resync -- has no durable record and
		// is replayed as before.
		if replayedDurably && e.Type == TypeNotification {
			continue
		}
		if err := writeEvent(w, e); err != nil {
			return
		}
	}
	flusher.Flush()

	ticker := time.NewTicker(h.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// The heartbeat is where the stream re-earns its right to exist.
			// A revoked or expired session ends here rather than at the next
			// request, because for a stream there is no next request.
			if h.stillLive != nil && p.SessionID != "" {
				if err := h.stillLive(r.Context(), p.SessionID); err != nil {
					observability.LoggerFrom(r.Context()).InfoContext(r.Context(),
						"stream: ending a stream whose session is no longer valid",
						"session_id", p.SessionID, "reason", err.Error())
					_ = writeEvent(w, Event{ID: 0, Type: TypeResync, OccurredAt: time.Now().UTC()})
					flusher.Flush()
					return
				}
			}
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-sub.Done():
			// The hub removed this subscriber. When that was the slow-consumer
			// drop, the client is behind and is told to resync; any other
			// removal ends the stream quietly. The departure arrives as a
			// closed `done` rather than a closed event channel, because closing
			// the channel a publisher sends on panics the publisher (F-185).
			if sub.Dropped() {
				_ = writeEvent(w, Event{ID: 0, Type: TypeResync, OccurredAt: time.Now().UTC()})
				flusher.Flush()
			}
			return
		case e := <-sub.Events():
			if err := writeEvent(w, e); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", strconv.FormatUint(e.ID, 10), e.Type, data)
	return err
}
