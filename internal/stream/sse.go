package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// Handler serves GET /v1/events/stream.
type Handler struct {
	hub       *Hub
	heartbeat time.Duration
	stillLive StillLive
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
	return &Handler{hub: hub, heartbeat: heartbeat, stillLive: stillLive}
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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub, replay := h.hub.Subscribe(p, after, 256)
	defer h.hub.Unsubscribe(sub)
	for _, e := range replay {
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
		case e, ok := <-sub.Events():
			if !ok {
				// Dropped as a slow consumer: tell the client to resync and end.
				_ = writeEvent(w, Event{ID: 0, Type: TypeResync, OccurredAt: time.Now().UTC()})
				flusher.Flush()
				return
			}
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
