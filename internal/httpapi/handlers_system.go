package httpapi

import (
	"context"
	"net/http"

	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/observability"
)

// GetHealthz reports that the process is alive. It touches nothing: liveness
// must never depend on a dependency, or a database blip restarts every replica.
func (s *Server) GetHealthz(_ context.Context, _ api.GetHealthzRequestObject) (api.GetHealthzResponseObject, error) {
	return api.GetHealthz200Response{}, nil
}

// GetReadyz reports readiness from the database and the validated
// configuration only. Provider liveness is deliberately excluded: a degraded
// venue must not take the API out of the load balancer, because observation,
// reconciliation and settlement have to keep running (PART 107).
func (s *Server) GetReadyz(ctx context.Context, _ api.GetReadyzRequestObject) (api.GetReadyzResponseObject, error) {
	if s.draining.Load() {
		// Shutting down: refuse new work while in-flight requests finish.
		return api.GetReadyz503Response{}, nil
	}
	if s.opts.Ports.Health == nil {
		return api.GetReadyz503Response{}, nil
	}
	if err := s.opts.Ports.Health.Ready(ctx); err != nil {
		observability.LoggerFrom(ctx).Warn("readiness check failed", "error", err.Error())
		return api.GetReadyz503Response{}, nil
	}
	return api.GetReadyz200Response{}, nil
}

// GetVersion returns the build version and the hash of the non-secret
// configuration (PART 222). The hash lets an operator prove which
// configuration a running binary loaded without exposing any of it.
func (s *Server) GetVersion(_ context.Context, _ api.GetVersionRequestObject) (api.GetVersionResponseObject, error) {
	return api.GetVersion200JSONResponse{
		BuildVersion: s.opts.BuildVersion,
		ConfigHash:   s.opts.ConfigHash,
		Environment:  string(s.opts.Env),
	}, nil
}

// PostWebhooksProvider hands the raw request bytes to the provider's ingestion
// pipeline (internal/webhook), which verifies the signature over those exact
// bytes, persists the evidence, and dispatches inside the inbox transaction.
// A 200 here means "received and recorded", never "settled".
func (s *Server) PostWebhooksProvider(ctx context.Context, request api.PostWebhooksProviderRequestObject) (api.PostWebhooksProviderResponseObject, error) {
	port, ok := s.opts.Ports.Webhooks[string(request.Provider)]
	if !ok || port == nil {
		return webhookResponse{status: http.StatusNotFound}, nil
	}
	r, ok := requestFrom(ctx)
	if !ok {
		return webhookResponse{status: http.StatusInternalServerError}, nil
	}
	raw := rawBody(ctx)
	if len(raw) == 0 {
		return api.PostWebhooksProvider400Response{}, nil
	}
	res := port.Handle(ctx, raw, r.Header, webhookRequestMeta(s, r))
	return webhookResponse{status: res.Status}, nil
}
