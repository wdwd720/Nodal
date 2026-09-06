package intent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// SubmitEndpoint is the idempotency endpoint name of Service.Submit.
const SubmitEndpoint = "intent.submit"

// DefaultIdempotencyTTL is how long a submission's idempotency record is
// retained (PART 36: the key stays stable until the command concludes, and a
// retry within the window replays the original result).
const DefaultIdempotencyTTL = 24 * time.Hour

// SubmitRequest is the command body of Service.Submit. The actor is derived
// from the principal; ActorType is optional and, when set, must match the
// principal's actor type. RequestedAt defaults to the service clock and
// CorrelationID to the context's correlation id (then request id, then a
// fresh identifier). StrategyVersionID and PredictionID are required for
// agents and refused for everybody else.
type SubmitRequest struct {
	AccountID         string
	ActorType         security.ActorType
	Action            Action
	InstrumentID      instruments.InstrumentID
	NotionalUSD       *money.USD
	TargetExposureUSD *money.USD
	Quantity          *money.Quantity
	Constraints       Constraints
	Deadline          time.Time
	RequestedAt       time.Time
	IdempotencyKey    string
	CorrelationID     string
	Mode              Mode
	StrategyVersionID string
	PredictionID      string
}

// Service is the command entry point for manual and agent intents. It
// performs no rate limiting: order-rate limits are a risk-kernel policy
// evaluated later in the pipeline.
type Service struct {
	repo *Repository
	idem *idempotency.Store
	clk  clock.Clock
	ttl  time.Duration
}

// NewService wires a Service. ttl <= 0 selects DefaultIdempotencyTTL.
func NewService(repo *Repository, idem *idempotency.Store, clk clock.Clock, ttl time.Duration) *Service {
	if repo == nil {
		panic("intent: NewService: nil repository")
	}
	if idem == nil {
		panic("intent: NewService: nil idempotency store")
	}
	if clk == nil {
		panic("intent: NewService: nil clock")
	}
	if ttl <= 0 {
		ttl = DefaultIdempotencyTTL
	}
	return &Service{repo: repo, idem: idem, clk: clk, ttl: ttl}
}

// Submit validates and persists a RECEIVED intent for principal p.
//
// Authorization: p must be a USER, OPERATOR or AGENT (SERVICE and SYSTEM
// cannot submit); it must hold trade:create (intent:create_agent for agents)
// and must pass security.RequireAccount for req.AccountID, so a customer can
// only submit for their own accounts and an agent only for its bound
// account. An agent's intent is always actor_type AGENT with the agent's
// subject as agent id; an agent naming another actor type is refused. When
// the context already carries a principal it must be the same subject.
//
// Idempotency: the key is claimed in idempotency.Store under
// (actor, "intent.submit", key) with the intent's ContentHash as request
// hash. A completed submission with the same key and content replays the
// stored result (the intent with Existing = true, or the original
// rejection); the same key with different content fails with
// INVALID_IDEMPOTENCY_REUSE; a submission still running elsewhere fails with
// IDEMPOTENCY_IN_PROGRESS. Business rejections raised while persisting are
// recorded as the command's result; internal failures leave the key
// re-acquirable. Structurally invalid requests are rejected before the store
// is touched.
func (s *Service) Submit(ctx context.Context, database *db.DB, p security.Principal, req SubmitRequest) (TradeIntent, error) {
	if err := p.Validate(); err != nil {
		return TradeIntent{}, errs.Wrap(err, errs.CodeForbidden, "intent: invalid principal")
	}
	if existing, ok := security.PrincipalFrom(ctx); ok && (existing.SubjectID != p.SubjectID || existing.ActorType != p.ActorType) {
		return TradeIntent{}, errs.New(errs.CodeForbidden, "intent: principal does not match the request context")
	}
	ctx = security.WithPrincipal(ctx, p)
	if !CanSubmit(p.ActorType) {
		return TradeIntent{}, errs.Newf(errs.CodeForbidden, "intent: %s principals cannot submit intents", p.ActorType)
	}
	if req.ActorType != "" && req.ActorType != p.ActorType {
		return TradeIntent{}, errs.Newf(errs.CodeForbidden, "intent: a %s principal cannot submit %s intents", p.ActorType, req.ActorType).
			WithField("actor_type", string(req.ActorType))
	}
	perm := security.PermTradeCreate
	if p.ActorType == security.ActorAgent {
		perm = security.PermIntentCreateAgent
	}
	if err := security.RequireAt(ctx, perm, s.clk.Now); err != nil {
		return TradeIntent{}, mapSecurityError(err)
	}
	if err := security.RequireAccount(ctx, req.AccountID); err != nil {
		return TradeIntent{}, mapSecurityError(err)
	}

	t := s.build(ctx, p, req)
	if err := t.Validate(); err != nil {
		return TradeIntent{}, err
	}
	requestHash := hex.EncodeToString(ContentHash(t))
	actorKey := string(p.ActorType) + ":" + p.SubjectID
	if database == nil {
		return TradeIntent{}, errs.New(errs.CodeInternal, "intent: Submit requires a database")
	}

	var (
		out    TradeIntent
		outErr error
	)
	err := database.InTx(ctx, db.TxOptions{MaxRetries: 3}, func(ctx context.Context, tx pgx.Tx) error {
		out, outErr = TradeIntent{}, nil
		begun, err := s.idem.Begin(ctx, tx, actorKey, SubmitEndpoint, req.IdempotencyKey, requestHash, s.ttl)
		if err != nil {
			return mapIdempotencyError(err)
		}
		switch b := begun.(type) {
		case idempotency.Replay:
			out, outErr = s.replay(ctx, tx, b)
			return nil
		case idempotency.InProgress:
			retry := time.Until(b.ExpiresAt)
			if retry <= 0 || retry > time.Second {
				retry = time.Second
			}
			return errs.New(errs.CodeIdempotencyInProgress, "intent: a submission with this idempotency key is in progress").
				WithRetryAfter(retry)
		case idempotency.Acquired:
			created, err := s.createUnderSavepoint(ctx, tx, t)
			if err != nil {
				if !concludes(err) {
					return err
				}
				if cerr := s.idem.Complete(ctx, tx, actorKey, SubmitEndpoint, req.IdempotencyKey, errs.HTTPStatus(errs.CodeOf(err)), "", "", rejectionBody(err)); cerr != nil {
					return errs.Wrap(cerr, errs.CodeInternal, "intent: record rejection")
				}
				outErr = err
				return nil
			}
			body, err := json.Marshal(submitResponse{IntentID: created.ID.String(), Status: created.Status})
			if err != nil {
				return errs.Wrap(err, errs.CodeInternal, "intent: encode response")
			}
			if err := s.idem.Complete(ctx, tx, actorKey, SubmitEndpoint, req.IdempotencyKey, http.StatusCreated, AuditResourceType, created.ID.String(), body); err != nil {
				return errs.Wrap(err, errs.CodeInternal, "intent: record completion")
			}
			out = created
			return nil
		default:
			return errs.New(errs.CodeInternal, "intent: unknown idempotency outcome")
		}
	})
	if err != nil {
		return TradeIntent{}, err
	}
	return out, outErr
}

// build assembles the TradeIntent from the principal and the request.
func (s *Service) build(ctx context.Context, p security.Principal, req SubmitRequest) TradeIntent {
	t := TradeIntent{
		ID:                NewIntentID(),
		AccountID:         req.AccountID,
		ActorType:         p.ActorType,
		ActorID:           p.SubjectID,
		Action:            req.Action,
		InstrumentID:      req.InstrumentID,
		NotionalUSD:       req.NotionalUSD,
		TargetExposureUSD: req.TargetExposureUSD,
		Quantity:          req.Quantity,
		Constraints:       req.Constraints,
		Deadline:          req.Deadline,
		RequestedAt:       req.RequestedAt,
		IdempotencyKey:    req.IdempotencyKey,
		CorrelationID:     req.CorrelationID,
		Mode:              req.Mode,
	}
	if t.RequestedAt.IsZero() {
		t.RequestedAt = s.clk.Now().UTC()
	}
	if t.CorrelationID == "" {
		t.CorrelationID = observability.CorrelationID(ctx)
	}
	if t.CorrelationID == "" {
		t.CorrelationID = observability.RequestID(ctx)
	}
	if t.CorrelationID == "" {
		t.CorrelationID = id.New[id.Any]().String()
	}
	if p.ActorType == security.ActorAgent {
		agentID := p.SubjectID
		t.AgentID = &agentID
	}
	if req.StrategyVersionID != "" {
		v := req.StrategyVersionID
		t.StrategyVersionID = &v
	}
	if req.PredictionID != "" {
		v := req.PredictionID
		t.PredictionID = &v
	}
	return t
}

// createUnderSavepoint runs Repository.Create inside a savepoint so a
// business rejection can be recorded on the idempotency row without
// aborting the outer transaction.
func (s *Service) createUnderSavepoint(ctx context.Context, tx pgx.Tx, t TradeIntent) (TradeIntent, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return TradeIntent{}, errs.Wrap(err, errs.CodeInternal, "intent: savepoint")
	}
	created, err := s.repo.Create(ctx, sp, t)
	if err != nil {
		_ = sp.Rollback(ctx)
		return TradeIntent{}, err
	}
	if err := sp.Commit(ctx); err != nil {
		return TradeIntent{}, errs.Wrap(err, errs.CodeInternal, "intent: release savepoint")
	}
	return created, nil
}

// replay materializes a stored result: the persisted intent, or the
// original rejection.
func (s *Service) replay(ctx context.Context, tx pgx.Tx, r idempotency.Replay) (TradeIntent, error) {
	if r.ResourceID != "" {
		iid, err := ParseIntentID(r.ResourceID)
		if err != nil {
			return TradeIntent{}, errs.Wrap(err, errs.CodeInternal, "intent: stored resource id is not an intent id")
		}
		t, err := s.repo.Get(ctx, tx, iid)
		if err != nil {
			return TradeIntent{}, err
		}
		t.Existing = true
		return t, nil
	}
	var body rejection
	if len(r.ResponseBody) == 0 || json.Unmarshal(r.ResponseBody, &body) != nil || body.Code == "" {
		return TradeIntent{}, errs.New(errs.CodeInternal, "intent: stored idempotency result is unreadable")
	}
	e := errs.New(errs.Code(body.Code), body.Detail)
	if len(body.Fields) > 0 {
		e = e.WithFields(body.Fields)
	}
	return TradeIntent{}, e
}

type submitResponse struct {
	IntentID string `json:"intent_id"`
	Status   Status `json:"status"`
}

type rejection struct {
	Code   string         `json:"code"`
	Detail string         `json:"detail"`
	Fields map[string]any `json:"fields,omitempty"`
}

func rejectionBody(err error) []byte {
	e, _ := errs.As(err)
	b, merr := json.Marshal(rejection{Code: string(e.Code), Detail: e.Detail, Fields: e.Fields})
	if merr != nil {
		b, _ = json.Marshal(rejection{Code: string(e.Code), Detail: e.Detail})
	}
	return b
}

// concludes reports whether err is a deterministic business rejection whose
// result should be stored for replay, as opposed to an internal or database
// failure that must leave the key re-acquirable.
func concludes(err error) bool {
	e, ok := errs.As(err)
	if !ok || !e.Code.Known() || e.Code == errs.CodeInternal {
		return false
	}
	return errs.HTTPStatus(e.Code) < http.StatusInternalServerError
}

func mapSecurityError(err error) error {
	switch {
	case errors.Is(err, security.ErrUnauthenticated):
		return errs.Wrap(err, errs.CodeUnauthenticated, "authentication required")
	case errors.Is(err, security.ErrCrossTenant):
		return errs.Wrap(err, errs.CodeForbidden, "intent: account is not accessible to this principal")
	case errors.Is(err, security.ErrForbidden):
		return errs.Wrap(err, errs.CodeForbidden, "intent: permission denied")
	}
	return errs.Wrap(err, errs.CodeForbidden, "intent: authorization failed")
}

func mapIdempotencyError(err error) error {
	switch {
	case errors.Is(err, idempotency.ErrKeyReuseConflict):
		return errs.Wrap(err, errs.CodeInvalidIdempotencyReuse, "intent: idempotency key already used for a different request")
	case errors.Is(err, idempotency.ErrInvalidArgument):
		return errs.Wrap(err, errs.CodeValidationFailed, "intent: invalid idempotency key")
	}
	return errs.Wrap(err, errs.CodeInternal, "intent: idempotency")
}
