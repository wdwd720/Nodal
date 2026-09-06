package signing

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nodal/controlplane/internal/errs"
	signingv1 "github.com/nodal/controlplane/internal/gen/proto/controlplane/signing/v1"
	"github.com/nodal/controlplane/internal/signing/inspect"
)

// Server adapts a Service to the generated gRPC contract. Rejections are
// successful RPCs with approved=false; only infrastructure and provider
// failures are gRPC errors.
type Server struct {
	signingv1.UnimplementedSigningServiceServer
	svc Service
}

// NewServer wraps svc.
func NewServer(svc Service) *Server { return &Server{svc: svc} }

// Register registers the server on a gRPC registrar.
func (s *Server) Register(r grpc.ServiceRegistrar) { signingv1.RegisterSigningServiceServer(r, s) }

// Sign implements signingv1.SigningServiceServer.
func (s *Server) Sign(ctx context.Context, in *signingv1.SignRequest) (*signingv1.SignResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	req := Request{
		AttemptID:      in.GetAttemptId(),
		PlanID:         in.GetPlanId(),
		IntentID:       in.GetIntentId(),
		RiskDecisionID: in.GetRiskDecisionId(),
		WalletID:       in.GetWalletId(),
		UnsignedTx:     in.GetUnsignedTransaction(),
		ExpectedTxHash: in.GetExpectedTransactionHash(),
		SimulationRef:  in.GetSimulationRef(),
		CorrelationID:  in.GetCorrelationId(),
	}
	if tables := in.GetLookupTables(); len(tables) > 0 {
		req.LookupTables = make(map[string][]string, len(tables))
		for _, t := range tables {
			req.LookupTables[t.GetAddress()] = append([]string(nil), t.GetAccountKeys()...)
		}
	}
	d, signed, err := s.svc.Sign(ctx, req)
	if err != nil {
		return nil, toStatus(err)
	}
	resp := toResponse(d)
	resp.SignedTransaction = signed
	return resp, nil
}

// GetDecision implements signingv1.SigningServiceServer.
func (s *Server) GetDecision(ctx context.Context, in *signingv1.GetDecisionRequest) (*signingv1.GetDecisionResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	d, err := s.svc.GetDecision(ctx, in.GetDecisionId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &signingv1.GetDecisionResponse{Decision: toResponse(d)}, nil
}

func toResponse(d Decision) *signingv1.SignResponse {
	checks := make([]*signingv1.Check, 0, len(d.Checks))
	for _, c := range d.Checks {
		checks = append(checks, &signingv1.Check{Name: c.Name, Passed: c.Passed, Detail: c.Detail})
	}
	return &signingv1.SignResponse{
		DecisionId:       d.ID,
		Approved:         d.Approved,
		ReasonCodes:      append([]string(nil), d.ReasonCodes...),
		Checks:           checks,
		InspectorVersion: d.InspectorVersion,
		ProviderSignRef:  d.ProviderSignRef,
		DecidedAt:        d.DecidedAt.UTC().Format(time.RFC3339Nano),
		Replayed:         d.Replayed,
	}
}

// toStatus maps an error to a gRPC status without leaking internal detail.
func toStatus(err error) error {
	var e *errs.Error
	if !errors.As(err, &e) {
		return status.Error(codes.Internal, string(errs.CodeInternal))
	}
	msg := string(e.Code) + ": " + e.Detail
	switch e.Code {
	case errs.CodeValidationFailed:
		return status.Error(codes.InvalidArgument, msg)
	case errs.CodeNotFound:
		return status.Error(codes.NotFound, msg)
	case errs.CodeForbidden, errs.CodeDelegationNotVerified:
		return status.Error(codes.PermissionDenied, msg)
	case errs.CodeUnauthenticated:
		return status.Error(codes.Unauthenticated, msg)
	case errs.CodeConflict, errs.CodeInvalidStateTransition, errs.CodeWalletInactive, errs.CodeSigningRejected:
		return status.Error(codes.FailedPrecondition, msg)
	case errs.CodeRateLimited:
		return status.Error(codes.ResourceExhausted, msg)
	case errs.CodeProviderUnavailable, errs.CodeVenueUnavailable:
		return status.Error(codes.Unavailable, msg)
	case errs.CodeInternal:
		return status.Error(codes.Internal, string(errs.CodeInternal))
	default:
		return status.Error(codes.FailedPrecondition, msg)
	}
}

// DecisionFromResponse converts a wire response back into a Decision
// (for in-process callers that want the domain type).
func DecisionFromResponse(r *signingv1.SignResponse) Decision {
	if r == nil {
		return Decision{}
	}
	d := Decision{
		ID: r.GetDecisionId(), Approved: r.GetApproved(), ReasonCodes: append([]string(nil), r.GetReasonCodes()...),
		InspectorVersion: r.GetInspectorVersion(), ProviderSignRef: r.GetProviderSignRef(), Replayed: r.GetReplayed(),
	}
	for _, c := range r.GetChecks() {
		d.Checks = append(d.Checks, inspect.Check{Name: c.GetName(), Passed: c.GetPassed(), Detail: c.GetDetail()})
	}
	if t, err := time.Parse(time.RFC3339Nano, r.GetDecidedAt()); err == nil {
		d.DecidedAt = t
	}
	return d
}

// inProcessClient satisfies the generated client interface by calling the
// server directly. It carries no network and ignores call options.
type inProcessClient struct {
	srv signingv1.SigningServiceServer
}

// NewInProcessClient returns a SigningServiceClient bound to srv without a
// network. The executor is written against the client interface so moving
// the boundary out of process changes only the constructor.
func NewInProcessClient(srv signingv1.SigningServiceServer) signingv1.SigningServiceClient {
	return &inProcessClient{srv: srv}
}

func (c *inProcessClient) Sign(ctx context.Context, in *signingv1.SignRequest, _ ...grpc.CallOption) (*signingv1.SignResponse, error) {
	return c.srv.Sign(ctx, in)
}

func (c *inProcessClient) GetDecision(ctx context.Context, in *signingv1.GetDecisionRequest, _ ...grpc.CallOption) (*signingv1.GetDecisionResponse, error) {
	return c.srv.GetDecision(ctx, in)
}
