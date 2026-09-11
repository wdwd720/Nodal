package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/security"
)

// Idempotency-Key bounds from the spec (components.parameters.IdempotencyKey).
const (
	minIdempotencyKeyLen = 8
	maxIdempotencyKeyLen = 128
)

// commandMeta describes what a command produced, for the idempotency record.
type commandMeta struct {
	Status       int
	ResourceType string
	ResourceID   string
}

// outcome is the result of a command run under the idempotency contract.
type outcome[T any] struct {
	Value    T
	Meta     commandMeta
	Replayed bool
}

// runCommand executes fn exactly once for this (actor, operation, key) and
// records its outcome so a retry replays rather than re-executes (PART 36).
//
// A business rejection is a conclusion: it is recorded, so replaying the key
// reproduces the same 4xx problem instead of running the command again. An
// internal failure, a rate limit and a provider outage are not conclusions:
// they are recorded as failures so a retry may still succeed. A submission
// whose state is unknown is recorded, because a blind re-run is exactly what
// must not happen.
func runCommand[T any](ctx context.Context, s *Server, key string, fn func(context.Context) (T, commandMeta, error)) (outcome[T], error) {
	var zero outcome[T]

	if s.opts.Ports.Idempotency == nil {
		return zero, errNotWired("idempotent commands")
	}
	if err := validateIdempotencyKey(key); err != nil {
		return zero, err
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return zero, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	r, ok := requestFrom(ctx)
	if !ok {
		return zero, errs.New(errs.CodeInternal, "internal error")
	}

	cmd := IdempotentCommand{
		ActorID:     p.SubjectID,
		Endpoint:    operationFrom(ctx),
		Key:         key,
		RequestHash: idempotency.HashRequest(r.Method, r.URL.Path, rawBody(ctx)),
		TTL:         s.opts.IdempotencyTTL,
	}

	res, err := s.opts.Ports.Idempotency.Run(ctx, cmd, func(ctx context.Context) (CommandResult, error) {
		v, meta, ferr := fn(ctx)
		if ferr != nil {
			e := classify(ctx, ferr)
			problem := errs.ToProblem(e, r.URL.Path, "")
			body, merr := json.Marshal(problem)
			if merr != nil {
				return CommandResult{}, ferr
			}
			return CommandResult{Status: problem.Status, Body: body}, ferr
		}
		body, merr := json.Marshal(v)
		if merr != nil {
			return CommandResult{}, errs.Wrap(merr, errs.CodeInternal, "internal error")
		}
		if meta.Status == 0 {
			meta.Status = http.StatusOK
		}
		return CommandResult{
			Status:       meta.Status,
			ResourceType: meta.ResourceType,
			ResourceID:   meta.ResourceID,
			Body:         body,
			// What the RECORD may keep, which is not always the whole answer:
			// a response carrying something the product documents as never
			// stored is stripped of it before it reaches idempotency_keys
			// (F-231, D-125). This caller still gets `body`.
			StoredBody: redactForStorage(cmd.Endpoint, body),
		}, nil
	})
	if err != nil {
		return zero, err
	}

	if res.Status >= 400 {
		// A recorded rejection: reproduce it verbatim, with this request's
		// own instance and request id.
		return zero, problemFromBody(res.Body, res.Status)
	}

	var v T
	if len(res.Body) > 0 {
		if uerr := json.Unmarshal(res.Body, &v); uerr != nil {
			return zero, errs.Wrap(uerr, errs.CodeInternal, "internal error")
		}
	}
	return outcome[T]{
		Value:    v,
		Meta:     commandMeta{Status: res.Status, ResourceType: res.ResourceType, ResourceID: res.ResourceID},
		Replayed: res.Replayed,
	}, nil
}

// problemFromBody rebuilds the recorded rejection as an *errs.Error so the
// normal problem+json path renders it.
func problemFromBody(body []byte, status int) error {
	var p errs.Problem
	if len(body) > 0 && json.Unmarshal(body, &p) == nil && p.Code != "" {
		e := errs.New(p.Code, p.Detail)
		if len(p.Fields) > 0 {
			e = e.WithFields(p.Fields)
		}
		return e
	}
	if status >= 500 {
		return errs.New(errs.CodeInternal, "internal error")
	}
	return errs.New(errs.CodeConflict, "the recorded outcome of this idempotency key could not be replayed")
}

// validateIdempotencyKey enforces components.parameters.IdempotencyKey.
//
// The charset is deliberately narrower than "printable ASCII". The key becomes
// part of idempotency_keys' primary key, is echoed back in responses, and is
// written to structured logs and audit records. Restricting it at the edge
// means none of those sinks has to be the place that gets quoting right, today
// and forever, for a value an untrusted client chose. It costs legitimate
// callers nothing: newIdempotencyKey() returns a UUID, and every key this repo
// generates is already a UUID or a dash-joined token.
func validateIdempotencyKey(key string) error {
	if len(key) < minIdempotencyKeyLen || len(key) > maxIdempotencyKeyLen {
		return validationError("Idempotency-Key", "Idempotency-Key must be 8 to 128 characters")
	}
	for i := 0; i < len(key); i++ {
		if !isIdempotencyKeyByte(key[i]) {
			return validationError("Idempotency-Key",
				"Idempotency-Key must contain only letters, digits, and the characters . _ : -")
		}
	}
	return nil
}

func isIdempotencyKeyByte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '.', b == '_', b == ':', b == '-':
		return true
	}
	return false
}

// idempotencyOutcomeIsConclusion reports whether a failed command's answer is
// a definite outcome that must be replayed rather than re-executed.
func idempotencyOutcomeIsConclusion(status int, code errs.Code) bool {
	if status >= 500 {
		return false
	}
	switch code {
	case errs.CodeInternal, errs.CodeProviderUnavailable, errs.CodeVenueUnavailable,
		errs.CodeRateLimited, errs.CodeIdempotencyInProgress:
		return false
	default:
		return true
	}
}
