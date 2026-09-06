package stream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// Handler serves GET /v1/events/stream.
type Handler struct {
	hub       *Hub
	heartbeat time.Duration
}

// NewHandler returns an SSE handler; heartbeat comments keep proxies from
// closing idle connections.
func NewHandler(hub *Hub, heartbeat time.Duration) *Handler {
	if heartbeat <= 0 {
		heartbeat = 15 * time.Second
	}
	return &Handler{hub: hub, heartbeat: heartbeat}
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
