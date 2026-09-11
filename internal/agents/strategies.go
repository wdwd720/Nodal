package agents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// CompilerBackend turns a natural-language description into a validated,
// typed Strategy IR. *strategy.Compiler satisfies it exactly.
//
// It is an interface for one reason and it is not testability: a deployment
// that has no model provider configured has no compiler, and the difference
// between "no compiler" and "a compiler that failed" is the difference between
// telling the user the truth and inventing a reason. A nil backend is a
// declared, reported state, not a missing dependency.
type CompilerBackend interface {
	CompileNL(ctx context.Context, req strategy.NLRequest) (strategy.Result, error)
}

// RefsLoader supplies the registry snapshot the compiler's TYPE and RISK_COMPAT
// stages validate against: instruments, venues, tools and the composed risk
// policy.
//
// It is separate from the backend because they fail independently and for
// different reasons. A build with a model key and no registry could produce IR
// that referenced nothing real, and validating a user's strategy against an
// EMPTY registry would reject every instrument they named and blame them for
// it. Both must be present before a compile is attempted.
type RefsLoader interface {
	Refs(ctx context.Context, q db.Querier, now time.Time) (strategy.ValidationRefs, error)
}

// CompilerUnavailable is the failure code recorded on a compile attempt, and
// returned to the caller, when this deployment has no compiler backend.
//
// It is a failure CODE and not an outcome because compile_attempts.outcome is a
// closed CHECK (00500) whose honest member here is MODEL_UNAVAILABLE: no model
// was reachable, which is exactly what happened. The code says WHY it was not
// reachable — not configured, as opposed to down or rate-limited — so the
// operator reading the row and the user reading the API are told the same
// thing, and neither is told a compiler tried and failed.
const CompilerUnavailable = "COMPILER_UNAVAILABLE"

// compilerUnavailableDetail is the sentence the API returns. It says what did
// not happen, what was recorded, and what would change it.
const compilerUnavailableDetail = "This deployment has no strategy compiler backend configured, so no IR was " +
	"produced and nothing was inferred from your description. The attempt is recorded. A compiler needs a model " +
	"provider credential and a validation registry; until both exist, a strategy cannot be compiled here and an " +
	"agent cannot be created from one."

// StrategyDeps are the strategy service's collaborators.
type StrategyDeps struct {
	DB    *db.DB
	Clock clock.Clock
	// Compiler is nil on a deployment with no model provider.
	Compiler CompilerBackend
	// Structured is the compiler that reads a DECLARED strategy rather than a
	// description. It is nil on every deployment that is not a sandbox tier.
	// At most one of Compiler and Structured answers a compile: a deployment
	// that somehow had both would be two products.
	Structured StructuredCompilerBackend
	// Refs is nil on a deployment with no validation registry. When Structured
	// is set it must also implement StructuredRefsLoader, which
	// NewStrategyService checks rather than discovering at compile time.
	Refs RefsLoader
	// CompilerVersion is recorded on every attempt.
	CompilerVersion string
	// Environment is the deployment environment, recorded on every version a
	// sandbox compiler produces so migration 00812's CHECK can refuse the pair
	// (sandbox, PROD).
	Environment string
	// Audit appends the acceptance event on the owner's account stream. Nil is
	// refused when nothing else is: an acceptance that leaves no trail is the
	// one act in this file that must leave one.
	Audit AuditAppender
}

// AuditAppender is the part of internal/audit this package uses. It appends
// inside the caller's transaction, so the acceptance row and its audit event
// commit together or not at all.
type AuditAppender interface {
	Append(ctx context.Context, tx pgx.Tx, e audit.Event) (audit.Appended, error)
}

// StrategyService is the strategy half of the agent creation flow (goal §18):
// describe, compile, review, approve.
type StrategyService struct {
	db         *db.DB
	clk        clock.Clock
	compiler   CompilerBackend
	structured StructuredCompilerBackend
	refs       RefsLoader
	registry   StructuredRefsLoader
	version    string
	env        string
	audit      AuditAppender
}

// NewStrategyService builds the service.
func NewStrategyService(deps StrategyDeps) (*StrategyService, error) {
	if deps.DB == nil {
		return nil, errs.New(errs.CodeValidationFailed, "agents: strategy service requires a database")
	}
	clk := deps.Clock
	if clk == nil {
		clk = clock.System()
	}
	v := deps.CompilerVersion
	if v == "" {
		v = "unspecified"
	}
	svc := &StrategyService{
		db: deps.DB, clk: clk, compiler: deps.Compiler, structured: deps.Structured,
		refs: deps.Refs, version: v, env: deps.Environment, audit: deps.Audit,
	}
	if deps.Structured != nil {
		if deps.Compiler != nil {
			return nil, errs.New(errs.CodeValidationFailed,
				"agents: a deployment has one compiler; wiring a model compiler and a structured one together would make which of them answered a coin toss")
		}
		loader, ok := deps.Refs.(StructuredRefsLoader)
		if !ok {
			return nil, errs.New(errs.CodeValidationFailed,
				"agents: a structured compiler needs a registry that can resolve an instrument NAME; wire a StructuredRefsLoader beside it")
		}
		svc.registry = loader
	}
	return svc, nil
}

// CompilerConfigured reports whether this deployment can compile at all. The
// API exposes it so the create-agent flow can say so before a user types a
// description, rather than after.
func (s *StrategyService) CompilerConfigured() bool {
	return (s.compiler != nil || s.structured != nil) && s.refs != nil
}

// CompilerInfo reports WHICH compiler this deployment has, so the product can
// say so rather than implying there is only one kind.
//
// A deployment with none answers the zero value, which reads as "no compiler"
// everywhere it is rendered and is the same fact CompilerConfigured reports.
func (s *StrategyService) CompilerInfo() CompilerInfo {
	switch {
	case !s.CompilerConfigured():
		return CompilerInfo{}
	case s.structured != nil:
		return CompilerInfo{
			Name: s.structured.CompilerName(), Sandbox: s.structured.SandboxCompiler(), Structured: true,
		}
	default:
		return CompilerInfo{Name: "model", Sandbox: false, Structured: false}
	}
}

// Strategy is one strategies row plus its current version, as the product shows
// it.
type Strategy struct {
	ID          string
	AccountID   string
	OwnerUserID string
	Name        string
	Description string
	// Constraints is the structured strategy the owner declared, verbatim, in
	// its own column (00813). It is what a structured compiler reads, and the
	// description is what a person reads: keeping them apart is what lets this
	// build claim the description is never interpreted.
	Constraints json.RawMessage
	SourceKind  string
	Status      string
	// CurrentVersion is the compiled version this strategy currently offers,
	// when one exists.
	CurrentVersion *StrategyVersion
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// StrategyVersion is one compiled, immutable strategy_versions row.
type StrategyVersion struct {
	ID            string
	Version       int
	Status        string
	IRHashHex     string
	EffectSet     []string
	HumanReadable string
	// IR is the compiled document itself, so the review step can show the
	// universe, the entries, the exits and the limits rather than a summary of
	// them (goal §18: show a human-understandable compiled strategy before
	// activation).
	IR json.RawMessage
	// Sandbox is true when this version was produced by a compiler that exists
	// only on a sandbox tier. Everything built from it is a rehearsal, and the
	// API and the page say so wherever it is shown.
	Sandbox bool
	// Environment is the deployment that compiled it; empty for a version
	// compiled before migration 00812 added the column.
	Environment string
	// AcceptedByUserID and AcceptedAt are the record of a PERSON reading this
	// document and approving it. They are the only evidence goal SS18's review
	// step leaves, and agents.Create refuses a version without them.
	AcceptedByUserID string
	AcceptedAt       *time.Time
	BuiltAt          time.Time
	CreatedAt        time.Time
}

// CompileOutcome is what one compile request produced.
type CompileOutcome struct {
	AttemptID string
	AttemptNo int
	// Outcome is the compile_attempts.outcome value: SUCCESS, REJECTED,
	// NEEDS_CLARIFICATION, MODEL_UNAVAILABLE or TIMEOUT.
	Outcome string
	// FailureCodes are the machine-readable reasons. COMPILER_UNAVAILABLE
	// appears here when the deployment has no compiler.
	FailureCodes []string
	// Clarifications is what the compiler could not decide. Never a guess.
	Clarifications []string
	Detail         string
	// Rationale is the compiler's own explanation of what it produced and why,
	// in words. Never chain-of-thought: it is the structured explanation
	// compile_attempts.explanation holds.
	Rationale Rationale
	// Version is present only on SUCCESS.
	Version *StrategyVersion
}

// Rationale is the compiler's user-visible explanation of one attempt. It is
// persisted in compile_attempts.explanation and shown beside the rendered
// strategy on the review step, so a person approving a document can read why
// each part of it is there.
type Rationale struct {
	Summary string   `json:"summary"`
	Details []string `json:"details"`
}

// Empty reports whether there is nothing to show.
func (r Rationale) Empty() bool { return r.Summary == "" && len(r.Details) == 0 }

// Succeeded reports whether a version was produced.
func (o CompileOutcome) Succeeded() bool {
	return o.Outcome == strategy.OutcomeSuccess && o.Version != nil
}

// CreateStrategyRequest is POST /v1/strategies.
type CreateStrategyRequest struct {
	AccountID string
	Name      string
	// Description is the natural language the user wrote. It is stored as
	// written and never edited into an instruction: what the compiler reads is
	// this text, and what the user reviews is the IR it produced.
	Description string
	// Constraints are optional structured bounds the user stated up front. They
	// are recorded with the description and given to the compiler as part of the
	// request text, never applied silently afterwards.
	Constraints   json.RawMessage
	CorrelationID string
}

// MaxStrategyNameLength mirrors the strategies.name CHECK.
const MaxStrategyNameLength = 120

// MaxDescriptionLength bounds what one request may carry. It is a transport
// bound, not a product opinion: a description longer than this is not a
// strategy, it is a document, and the SDK path exists for those.
const MaxDescriptionLength = 8000

// Create records a strategy. Nothing is compiled here: describing a strategy
// and asking for it to be compiled are two acts, because goal §18 requires the
// user to see the compiled result before anything is activated, and merging
// them would make the description itself the approval.
func (s *StrategyService) Create(ctx context.Context, req CreateStrategyRequest) (Strategy, error) {
	p, err := s.ownerActor(ctx, req.AccountID)
	if err != nil {
		return Strategy{}, err
	}
	name := strings.TrimSpace(req.Name)
	description := strings.TrimSpace(req.Description)
	fields := map[string]any{}
	if l := len(name); l < 1 || l > MaxStrategyNameLength {
		fields["name"] = "must be 1..120 characters"
	}
	if description == "" {
		fields["description"] = "describe what you want the strategy to do"
	}
	if len(description) > MaxDescriptionLength {
		fields["description"] = "must be at most 8000 characters"
	}
	if len(req.Constraints) > 0 && !json.Valid(req.Constraints) {
		fields["constraints"] = "must be a JSON object"
	}
	if len(fields) > 0 {
		return Strategy{}, errs.New(errs.CodeValidationFailed, "agents: strategy is not recordable").WithFields(fields)
	}

	now := s.clk.Now().UTC()
	id := strategy.NewStrategyID()
	// The description is stored exactly as it was written, and the declared
	// constraints go in their own column (00813). They used to be concatenated
	// onto the description, which made "what you wrote" untrue and left a
	// compiler no way to claim it had not read the prose.
	constraints := json.RawMessage("{}")
	if len(req.Constraints) > 0 {
		constraints = req.Constraints
	}
	err = s.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, ierr := tx.Exec(ctx, `
			INSERT INTO strategies (id, owner_account_id, owner_user_id, name, description, constraints, source_kind, status,
			                        created_by_actor_type, created_by_actor_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, 'NATURAL_LANGUAGE', 'ACTIVE', $7, $8, $9, $9)`,
			id, req.AccountID, p.SubjectID, name, description, []byte(constraints), string(p.ActorType), p.SubjectID, now)
		if ierr != nil {
			if db.IsUniqueViolation(ierr) {
				return errs.New(errs.CodeConflict, "agents: a strategy with that name already exists on this account").
					WithField("name", name)
			}
			return errs.Wrap(ierr, errs.CodeInternal, "agents: insert strategy")
		}
		return nil
	})
	if err != nil {
		return Strategy{}, err
	}
	return s.Get(ctx, id.String())
}

const strategyColumns = `s.id::text, s.owner_account_id::text, s.owner_user_id::text, s.name, s.description,
       s.constraints, s.source_kind, s.status, s.created_at, s.updated_at,
       coalesce(v.id::text, ''), coalesce(v.version, 0), coalesce(v.status, ''),
       coalesce(encode(v.ir_hash, 'hex'), ''), coalesce(v.effect_set, '{}'::text[]),
       coalesce(v.human_readable, ''), coalesce(v.ir, 'null'::jsonb),
       coalesce(v.sandbox, false), coalesce(v.environment, ''),
       v.accepted_by_user_id::text, v.accepted_at, v.built_at, v.created_at`

func scanStrategy(row pgx.Row) (Strategy, error) {
	var (
		st                  Strategy
		constraints         []byte
		vid, vstatus, vhash string
		vversion            int
		effects             []string
		human               string
		irDoc               []byte
		sandbox             bool
		environment         string
		acceptedBy          *string
		acceptedAt          *time.Time
		builtAt, vcreated   *time.Time
	)
	err := row.Scan(&st.ID, &st.AccountID, &st.OwnerUserID, &st.Name, &st.Description,
		&constraints, &st.SourceKind, &st.Status, &st.CreatedAt, &st.UpdatedAt,
		&vid, &vversion, &vstatus, &vhash, &effects, &human, &irDoc,
		&sandbox, &environment, &acceptedBy, &acceptedAt, &builtAt, &vcreated)
	if err != nil {
		return Strategy{}, err
	}
	st.Constraints = json.RawMessage(constraints)
	if vid != "" {
		v := StrategyVersion{
			ID: vid, Version: vversion, Status: vstatus, IRHashHex: vhash,
			EffectSet: effects, HumanReadable: human, IR: json.RawMessage(irDoc),
			Sandbox: sandbox, Environment: environment, AcceptedAt: acceptedAt,
		}
		if acceptedBy != nil {
			v.AcceptedByUserID = *acceptedBy
		}
		if builtAt != nil {
			v.BuiltAt = *builtAt
		}
		if vcreated != nil {
			v.CreatedAt = *vcreated
		}
		st.CurrentVersion = &v
	}
	return st, nil
}

// Get returns one strategy the caller owns.
func (s *StrategyService) Get(ctx context.Context, strategyID string) (Strategy, error) {
	id, err := strategy.ParseStrategyID(strategyID)
	if err != nil {
		return Strategy{}, errs.New(errs.CodeValidationFailed, "agents: strategyId must be a canonical UUID").
			WithField("strategyId", "must be a canonical UUID")
	}
	row := s.db.QueryRow(ctx, `SELECT `+strategyColumns+`
		FROM strategies s LEFT JOIN strategy_versions v ON v.id = s.current_version_id
		WHERE s.id = $1`, id)
	st, err := scanStrategy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Strategy{}, errs.New(errs.CodeNotFound, "agents: no such strategy").WithField("strategy_id", strategyID)
	}
	if err != nil {
		return Strategy{}, errs.Wrap(err, errs.CodeInternal, "agents: read strategy")
	}
	if err := s.requireRead(ctx, st.AccountID); err != nil {
		// Not yours is not found, for the reason agents.notYours gives: a
		// FORBIDDEN here tells a stranger their guessed id names a real
		// strategy, and the IR under it is the thing this refusal protects.
		if errors.Is(err, security.ErrCrossTenant) {
			return Strategy{}, errs.New(errs.CodeNotFound, "agents: no such strategy").
				WithField("strategy_id", strategyID)
		}
		return Strategy{}, err
	}
	return st, nil
}

// List returns the caller's strategies, newest first.
func (s *StrategyService) List(ctx context.Context, accountID string, limit int) ([]Strategy, error) {
	if err := s.requireRead(ctx, accountID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(ctx, `SELECT `+strategyColumns+`
		FROM strategies s LEFT JOIN strategy_versions v ON v.id = s.current_version_id
		WHERE s.owner_account_id = $1
		ORDER BY s.created_at DESC, s.id DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "agents: list strategies")
	}
	defer rows.Close()
	out := []Strategy{}
	for rows.Next() {
		st, serr := scanStrategy(rows)
		if serr != nil {
			return nil, errs.Wrap(serr, errs.CodeInternal, "agents: scan strategy")
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "agents: list strategies")
	}
	return out, nil
}

// Compile turns the strategy's description into IR, or records honestly why it
// did not.
//
// Every path through this function writes exactly one compile_attempts row,
// including the path where no compiler exists. That is the whole point of
// recording attempts (PART 64): "we did not try" is as much a fact about a
// strategy as "we tried and it was rejected", and a product that recorded only
// the second would show a user a blank history and no explanation.
func (s *StrategyService) Compile(ctx context.Context, strategyID, requestID, correlationID string) (CompileOutcome, error) {
	st, err := s.Get(ctx, strategyID)
	if err != nil {
		return CompileOutcome{}, err
	}
	p, err := s.ownerActor(ctx, st.AccountID)
	if err != nil {
		return CompileOutcome{}, err
	}
	if st.Status != "ACTIVE" {
		return CompileOutcome{}, errs.Newf(errs.CodeInvalidStateTransition, "agents: this strategy is %s", st.Status).
			WithField("status", st.Status)
	}
	sid, _ := strategy.ParseStrategyID(st.ID)
	if requestID == "" {
		requestID = strategy.NewAttemptID().String()
	}

	nextVersion, err := s.nextVersionNumber(ctx, sid)
	if err != nil {
		return CompileOutcome{}, err
	}

	if !s.CompilerConfigured() {
		return s.recordUnavailable(ctx, sid, requestID, st.Description, p, correlationID)
	}

	now := s.clk.Now().UTC()
	refs, err := s.refs.Refs(ctx, s.db, now)
	if err != nil {
		return CompileOutcome{}, err
	}

	// Which compiler answers is a property of the deployment, never of the
	// request: a caller cannot ask for the other one, and there is never more
	// than one (NewStrategyService refuses a service wired with both).
	if s.structured != nil {
		return s.compileStructured(ctx, st, sid, requestID, nextVersion, refs, now, p, correlationID)
	}

	sum := sha256.Sum256([]byte(st.AccountID))
	res, err := s.compiler.CompileNL(ctx, strategy.NLRequest{
		StrategyID: sid, RequestID: requestID, OwnerAccountID: st.AccountID, OwnerUserID: st.OwnerUserID,
		Text: st.Description, Version: nextVersion, TenantHash: hex(sum[:]), Refs: refs,
	})
	if err != nil {
		return CompileOutcome{}, err
	}
	return s.persist(ctx, sid, requestID, nextVersion, res, p, correlationID)
}

// compileStructured runs the declared-strategy path.
//
// The description is not passed. That is the whole difference between this
// function and the one above it, and it is deliberately visible here rather
// than hidden inside a backend: a reader of this file can see that the text a
// person wrote does not leave the row it was stored in.
func (s *StrategyService) compileStructured(ctx context.Context, st Strategy, sid strategy.StrategyID,
	requestID string, nextVersion int, refs strategy.ValidationRefs, now time.Time,
	p security.Principal, correlationID string,
) (CompileOutcome, error) {
	registry, err := s.registry.Registry(ctx, s.db, now)
	if err != nil {
		return CompileOutcome{}, err
	}
	attemptNo, err := s.nextAttemptNo(ctx, requestID)
	if err != nil {
		return CompileOutcome{}, err
	}
	res, err := s.structured.CompileStructured(ctx, StructuredCompileRequest{
		StrategyID: sid, RequestID: requestID, AttemptNo: attemptNo,
		OwnerAccountID: st.AccountID, OwnerUserID: st.OwnerUserID,
		Version:     nextVersion,
		Constraints: st.Constraints,
		Refs:        refs,
		Registry:    registry,
		Environment: s.env,
	})
	if err != nil {
		return CompileOutcome{}, err
	}
	out, err := s.persist(ctx, sid, requestID, nextVersion, res, p, correlationID)
	if err != nil {
		return CompileOutcome{}, err
	}
	if containsCode(out.FailureCodes, StructuredConstraintsRequired) {
		out.Detail = structuredConstraintsDetail
	}
	return out, nil
}

func containsCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// recordUnavailable writes the attempt that says no compiler exists.
func (s *StrategyService) recordUnavailable(ctx context.Context, sid strategy.StrategyID, requestID, description string,
	p security.Principal, correlationID string,
) (CompileOutcome, error) {
	sum := sha256.Sum256([]byte(description))
	attemptID := strategy.NewAttemptID()
	attemptNo, err := s.nextAttemptNo(ctx, requestID)
	if err != nil {
		return CompileOutcome{}, err
	}
	explanation, _ := json.Marshal(map[string]string{
		"code":   CompilerUnavailable,
		"detail": compilerUnavailableDetail,
	})
	err = s.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, ierr := tx.Exec(ctx, `
			INSERT INTO compile_attempts (
			    id, strategy_id, request_id, attempt_no, source_kind, input_hash,
			    parse_result, stage_reached, outcome, failure_codes, explanation,
			    requested_by_user_id, correlation_id, created_at)
			VALUES ($1, $2, $3, $4, 'NATURAL_LANGUAGE', $5,
			        'NOT_ATTEMPTED', 'PROMPT', $6, $7::text[], $8::jsonb, $9, $10, $11)`,
			attemptID, sid, requestID, attemptNo, sum[:],
			strategy.OutcomeModelUnavailable, []string{CompilerUnavailable}, explanation,
			p.SubjectID, nullText(correlationID), s.clk.Now().UTC())
		if ierr != nil {
			return errs.Wrap(ierr, errs.CodeInternal, "agents: record compile attempt")
		}
		return nil
	})
	if err != nil {
		return CompileOutcome{}, err
	}
	return CompileOutcome{
		AttemptID: attemptID.String(), AttemptNo: attemptNo,
		Outcome: strategy.OutcomeModelUnavailable, FailureCodes: []string{CompilerUnavailable},
		Detail: compilerUnavailableDetail,
	}, nil
}

// persist writes every attempt the compiler made and, on success, the version.
//
// Attempts are written under the REQUEST ID this service issued, never the one
// the backend echoed back: compile_attempts is UNIQUE(request_id, attempt_no)
// and that pair is the bound on how many times one request may be retried, so a
// backend returning an id of its own could widen it or collide with another
// user's.
//
// The same reasoning applies to everything else the backend returns, and until
// F-189 it did not: the Version was written verbatim. A CompilerBackend is a
// SEAM -- ADR-0029 declares it, *strategy.Compiler satisfies it today, and
// whatever a later deployment wires satisfies it tomorrow -- so a backend could
// return a version naming a DIFFERENT account's strategy, at a version number
// nobody reserved, already ACCEPTED, with an ir_hash that describes no
// document, and this service would write it and point the victim's strategy at
// it. checkVersion and checkAttempt are the boundary: what the backend is
// trusted for is the IR, and every identifier around it is this service's.
func (s *StrategyService) persist(ctx context.Context, sid strategy.StrategyID, requestID string, nextVersion int,
	res strategy.Result, p security.Principal, correlationID string,
) (CompileOutcome, error) {
	if res.Version != nil {
		if err := checkVersion(sid, nextVersion, res.Version); err != nil {
			return CompileOutcome{}, err
		}
	}
	for _, a := range res.Attempts {
		if err := checkAttempt(sid, a); err != nil {
			return CompileOutcome{}, err
		}
	}
	var out CompileOutcome
	err := s.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if res.Version != nil {
			if err := s.insertVersion(ctx, tx, res.Version); err != nil {
				return err
			}
		}
		for i, a := range res.Attempts {
			// The rationale belongs to the LAST attempt, which is the one that
			// produced the result being explained.
			var rationale Rationale
			if i == len(res.Attempts)-1 {
				rationale = Rationale{Summary: res.Rationale.Summary, Details: res.Rationale.Assumptions}
			}
			if err := s.insertAttempt(ctx, tx, sid, requestID, a, rationale, res.Clarifications, p, correlationID); err != nil {
				return err
			}
		}
		if res.Version != nil {
			if _, err := tx.Exec(ctx,
				`UPDATE strategies SET current_version_id = $2 WHERE id = $1`, sid, res.Version.ID); err != nil {
				return errs.Wrap(err, errs.CodeInternal, "agents: set current strategy version")
			}
		}
		return nil
	})
	if err != nil {
		return CompileOutcome{}, err
	}
	out.Outcome = res.Outcome
	out.FailureCodes = res.Codes
	out.Clarifications = res.Clarifications
	if n := len(res.Attempts); n > 0 {
		out.AttemptID = res.Attempts[n-1].ID.String()
		out.AttemptNo = res.Attempts[n-1].AttemptNo
	}
	out.Detail = detailFor(res.Outcome)
	out.Rationale = Rationale{Summary: res.Rationale.Summary, Details: res.Rationale.Assumptions}
	if res.Version != nil {
		out.Version = &StrategyVersion{
			ID: res.Version.ID.String(), Version: res.Version.Version, Status: res.Version.Status,
			IRHashHex: hex(res.Version.IRHash), EffectSet: res.Version.EffectSet,
			HumanReadable: res.Version.HumanReadable, BuiltAt: res.Version.BuiltAt,
			Sandbox: s.sandboxVersions(), Environment: s.environmentOfNewVersions(),
		}
		if doc, merr := json.Marshal(res.Version.IR); merr == nil {
			out.Version.IR = doc
		}
	}
	return out, nil
}

// compilerRefusal is the code a backend's own output is refused with. It is
// INTERNAL and not VALIDATION_FAILED because the caller did nothing wrong: the
// user asked for a compile and the thing this deployment wired answered with
// something it is not allowed to say.
func compilerRefusal(format string, a ...any) error {
	return errs.Newf(errs.CodeInternal, "agents: the compiler backend returned "+format, a...)
}

// checkVersion refuses a compiled version that is not the one this service
// asked for.
//
// Four questions, and every one of them is about an identifier this service
// issued rather than about the IR:
//
//   - StrategyID: the version must be a version of the strategy that was
//     compiled. Without this a backend writes under any strategy it can name,
//     including one belonging to an account the caller cannot read.
//   - Version: the number reserved by nextVersionNumber before the call. A
//     backend choosing its own could take a number that is already somebody
//     else's history, or skip the sequence the review flow reads.
//   - Status: COMPILED. ACCEPTED is the record of a PERSON reading the strategy
//     and approving it (00500 pairs it with accepted_by_user_id), and a backend
//     that could return it would be approving on the user's behalf, which is
//     exactly what goal SS18's review step exists to stop.
//   - IRHash: the semantic hash of the IR that came with it. The hash is what
//     every later comparison -- the lineage, the parity fixtures, an audit of
//     "is this the document you approved" -- is made against, so a hash that
//     does not describe the document makes all of them agree about nothing.
func checkVersion(sid strategy.StrategyID, nextVersion int, v *strategy.Version) error {
	if v.StrategyID != sid {
		return compilerRefusal("a version of strategy %s from a compile of strategy %s", v.StrategyID, sid)
	}
	if v.Version != nextVersion {
		return compilerRefusal("version number %d where %d was reserved", v.Version, nextVersion)
	}
	if v.Status != strategy.StatusCompiled {
		return compilerRefusal("a version already in status %s; only a person accepts a strategy", v.Status)
	}
	want, err := ir.SemanticHash(v.IR)
	if err != nil {
		return compilerRefusal("a version whose IR cannot be hashed: %v", err)
	}
	if !bytes.Equal(want, v.IRHash) {
		return compilerRefusal("an ir_hash that does not describe its own IR (%s, want %s)",
			hex(v.IRHash), hex(want))
	}
	return nil
}

// checkAttempt refuses an attempt that is not an attempt at this strategy, or
// that claims a number past the ceiling compile_attempts itself enforces.
func checkAttempt(sid strategy.StrategyID, a strategy.Attempt) error {
	if a.StrategyID != sid {
		return compilerRefusal("an attempt at strategy %s from a compile of strategy %s", a.StrategyID, sid)
	}
	if a.AttemptNo < 1 || a.AttemptNo > strategy.HardMaxAttempts {
		return compilerRefusal("attempt number %d, outside 1..%d", a.AttemptNo, strategy.HardMaxAttempts)
	}
	return nil
}

func (s *StrategyService) insertVersion(ctx context.Context, tx pgx.Tx, v *strategy.Version) error {
	doc, err := json.Marshal(v.IR)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agents: encode compiled ir")
	}
	schemaVersion := ir.SchemaVersion
	_, err = tx.Exec(ctx, `
		INSERT INTO strategy_versions (
		    id, strategy_id, version, schema_version, ir, ir_hash, effect_set, status, source_kind,
		    source_hash, compiler_version, risk_policy_version, risk_policy_hash,
		    model_budget, data_budget, envelope_requirements, human_readable, built_at, sandbox, environment)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7::text[], $8, $9, $10, $11, $12, $13,
		        '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, $14, $15, $16, $17)`,
		v.ID, v.StrategyID, v.Version, schemaVersion, doc, v.IRHash, v.EffectSet, v.Status, string(v.SourceKind),
		v.SourceHash, v.CompilerVersion, v.RiskPolicy, v.RiskPolicyHash, v.HumanReadable, v.BuiltAt,
		s.sandboxVersions(), s.environmentOfNewVersions())
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agents: insert strategy version")
	}
	return nil
}

func (s *StrategyService) insertAttempt(ctx context.Context, tx pgx.Tx, sid strategy.StrategyID, requestID string,
	a strategy.Attempt, rationale Rationale, clarifications []string, p security.Principal, correlationID string,
) error {
	codes := a.FailureCodes
	if codes == nil {
		codes = []string{}
	}
	// explanation and clarifications are columns 00500 created for exactly this
	// and that nothing ever wrote. An attempt that recorded its codes and threw
	// away the sentences explaining them left a user reading a list of
	// identifiers, which is the shape of record this table exists to avoid.
	explanation, merr := json.Marshal(map[string]any{
		"summary": rationale.Summary,
		"details": stringsOrEmpty(rationale.Details),
		"fields":  a.Fields,
	})
	if merr != nil {
		return errs.Wrap(merr, errs.CodeInternal, "agents: encode compile explanation")
	}
	clarificationsJSON, merr := json.Marshal(stringsOrEmpty(clarifications))
	if merr != nil {
		return errs.Wrap(merr, errs.CodeInternal, "agents: encode compile clarifications")
	}
	var versionID *string
	if a.VersionID != nil {
		v := a.VersionID.String()
		versionID = &v
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO compile_attempts (
		    id, strategy_id, request_id, attempt_no, source_kind, input_hash,
		    prompt_template_version, model_provider, model_id,
		    parse_result, stage_reached, outcome, failure_codes, clarifications, explanation,
		    structured_output, strategy_version_id, requested_by_user_id, correlation_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::text[], $14::jsonb, $15::jsonb,
		        $16, $17, $18, $19, $20)`,
		a.ID, sid, requestID, a.AttemptNo, string(a.SourceKind), a.InputHash,
		nullText(a.Provenance.TemplateVersion), nullText(a.Provenance.Provider), nullText(a.Provenance.ModelID),
		parseResultOf(a), a.StageReached, a.Outcome, codes, clarificationsJSON, explanation,
		jsonOrNil(a.Structured), versionID, p.SubjectID, nullText(correlationID), a.CreatedAt)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agents: record compile attempt")
	}
	return nil
}

func parseResultOf(a strategy.Attempt) string {
	if r := string(a.Provenance.ParseResult); r != "" {
		return r
	}
	return "NOT_ATTEMPTED"
}

func jsonOrNil(raw json.RawMessage) any {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	return []byte(raw)
}

func detailFor(outcome string) string {
	switch outcome {
	case strategy.OutcomeSuccess:
		return "Compiled. Review the strategy below before you create an agent from it; nothing is active until you do."
	case strategy.OutcomeNeedsClarification:
		return "The compiler could not decide what you meant and did not guess. Answer the questions and compile again."
	case strategy.OutcomeRejected:
		return "The compiled strategy was refused by the platform's rules. The reasons are listed; nothing was created."
	case strategy.OutcomeModelUnavailable:
		return compilerUnavailableDetail
	case strategy.OutcomeTimeout:
		return "The compiler did not answer in time. Nothing was produced and the attempt is recorded."
	}
	return "The compile attempt is recorded."
}

func (s *StrategyService) nextVersionNumber(ctx context.Context, sid strategy.StrategyID) (int, error) {
	var n int
	err := s.db.QueryRow(ctx,
		`SELECT coalesce(max(version), 0) + 1 FROM strategy_versions WHERE strategy_id = $1`, sid).Scan(&n)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "agents: read strategy version number")
	}
	return n, nil
}

func (s *StrategyService) nextAttemptNo(ctx context.Context, requestID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx,
		`SELECT coalesce(max(attempt_no), 0) + 1 FROM compile_attempts WHERE request_id = $1`, requestID).Scan(&n)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "agents: read attempt number")
	}
	if n > strategy.HardMaxAttempts {
		return 0, errs.New(errs.CodeConflict, "agents: this compile request has used every attempt it is allowed").
			WithField("request_id", requestID)
	}
	return n, nil
}

func (s *StrategyService) ownerActor(ctx context.Context, accountID string) (security.Principal, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return security.Principal{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	if p.ActorType != security.ActorUser {
		return security.Principal{}, errs.Newf(errs.CodeForbidden,
			"agents: %s principals cannot write a customer's strategy", p.ActorType).
			WithField("actor_type", string(p.ActorType))
	}
	if err := security.RequireAt(ctx, security.PermStrategyWrite, s.clk.Now); err != nil {
		return security.Principal{}, authError(err)
	}
	// The owner-only twin: account:read_any is a read capability and writing
	// somebody's strategy is not a read (ADR-0022's rule, and the reason
	// RequireAccountOwner exists).
	if err := security.RequireAccountOwner(ctx, accountID); err != nil {
		return security.Principal{}, authError(err)
	}
	return p, nil
}

func (s *StrategyService) requireRead(ctx context.Context, accountID string) error {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	if p.ActorType == security.ActorOperator {
		if err := security.RequireAnyAt(ctx, s.clk.Now, security.PermAccountReadAny); err != nil {
			return authError(err)
		}
		return nil
	}
	if err := security.RequireAt(ctx, security.PermStrategyRead, s.clk.Now); err != nil {
		return authError(err)
	}
	// An operator is answered above. A customer reads their own.
	if err := security.RequireAccountOwner(ctx, accountID); err != nil {
		return authError(err)
	}
	return nil
}

const hexDigits = "0123456789abcdef"

// hex renders bytes without importing encoding/hex for two call sites that both
// want a lower-case, unpadded string.
func hex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&0x0f])
	}
	return string(out)
}
