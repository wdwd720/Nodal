package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
)

// GetEventsStream hands the connection to internal/stream, which owns the SSE
// framing: per-principal filtering, monotonic event ids, bounded replay from
// Last-Event-ID with a `resync` event on a gap, heartbeat comments, a flush
// after every write, and unsubscription when the client disconnects.
//
// The stream is a hint, never truth (PART 109). A client that sees a gap
// refetches canonical REST state; nothing here is authoritative.
func (s *Server) GetEventsStream(ctx context.Context, _ api.GetEventsStreamRequestObject) (api.GetEventsStreamResponseObject, error) {
	if s.opts.Ports.Stream == nil {
		return nil, errNotWired("event streaming")
	}
	r, ok := requestFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeInternal, "internal error")
	}
	// The request is re-bound to the handler context so the stream sees
	// the principal the session middleware resolved.
	return sseResponse{handler: s.opts.Ports.Stream, request: r.WithContext(ctx), stop: s.streamCtx}, nil
}
