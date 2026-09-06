package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/nodal/controlplane/internal/auth/httpmw"
)

// The generated 3xx/204 response types carry no headers, and SSE owns the
// connection for its lifetime. These types implement the same generated
// response interfaces so the strict server stays in charge of dispatch while
// the handler keeps control of what actually reaches the wire.

// redirectResponse is a 302 with a Location header. before, when set, writes
// headers (the session cookie) before the status line.
type redirectResponse struct {
	location string
	before   func(http.ResponseWriter)
}

func (rr redirectResponse) write(w http.ResponseWriter) error {
	if rr.before != nil {
		rr.before(w)
	}
	w.Header().Set("Location", rr.location)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
	return nil
}

func (rr redirectResponse) VisitGetAuthLoginResponse(w http.ResponseWriter) error {
	return rr.write(w)
}

func (rr redirectResponse) VisitGetAuthCallbackResponse(w http.ResponseWriter) error {
	return rr.write(w)
}

// logoutResponse clears the session cookie and answers 204.
type logoutResponse struct {
	name   string
	domain string
	secure bool
}

func (l logoutResponse) VisitPostAuthLogoutResponse(w http.ResponseWriter) error {
	httpmw.ClearSessionCookie(w, l.name, l.domain, l.secure)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// sseResponse hands the connection to internal/stream, which owns the SSE
// framing, the heartbeat and per-client cleanup on disconnect.
type sseResponse struct {
	handler http.Handler
	request *http.Request
	// stop is cancelled when the process starts draining, so a long-lived
	// stream ends instead of blocking a graceful shutdown.
	stop context.Context
}

func (s sseResponse) VisitGetEventsStreamResponse(w http.ResponseWriter) error {
	// A stream outlives any sensible write deadline. Clearing it here rather
	// than turning the server's WriteTimeout off keeps the timeout in force
	// for every ordinary request.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.SetReadDeadline(time.Time{})

	r := s.request
	if s.stop != nil {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stopWatching := context.AfterFunc(s.stop, cancel)
		defer stopWatching()
		r = r.WithContext(ctx)
	}
	s.handler.ServeHTTP(w, r)
	return nil
}

// webhookResponse answers with the status the ingestion pipeline decided. The
// body is empty on purpose: a provider must learn nothing about internal
// state from an acknowledgement.
type webhookResponse struct{ status int }

func (wr webhookResponse) VisitPostWebhooksProviderResponse(w http.ResponseWriter) error {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(wr.status)
	return nil
}

// decodeObject converts a raw JSON object into the map shape the generated
// models use for free-form fields.
func decodeObject(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}
