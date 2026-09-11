package agents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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
	// Refs is nil on a deployment with no validation registry.
	Refs RefsLoader
	// CompilerVersion is recorded on every attempt.
	CompilerVersion string
}

// StrategyService is the strategy half of the agent creation flow (goal §18):
// describe, compile, review, approve.
type StrategyService struct {
	db       *db.DB
	clk      clock.Clock
	compiler CompilerBackend
	refs     RefsLoader
	version  string
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
	return &StrategyService{db: deps.DB, clk: clk, compiler: deps.Compiler, refs: deps.Refs, version: v}, nil
}

// CompilerConfigured reports whether this deployment can compile at all. The
// API exposes it so the create-agent flow can say so before a user types a
// description, rather than after.
func (s *StrategyService) CompilerConfigured() bool { return s.compiler != nil && s.refs != nil }

// Strategy is one strategies row plus its current version, as the product shows
// it.
type Strategy struct {
	ID          string
	AccountID   string
	OwnerUserID string
	Name        string
	Description string
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
	IR        json.RawMessage
	BuiltAt   time.Time
	CreatedAt time.Time
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
	// Version is present only on SUCCESS.
	Version *StrategyVersion
}

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
	stored := description
	if len(req.Constraints) > 0 {
		stored = description + "\n\n[structured constraints]\n" + string(req.Constraints)
	}
	err = s.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, ierr := tx.Exec(ctx, `
			INSERT INTO strategies (id, owner_account_id, owner_user_id, name, description, source_kind, status,
			                        created_by_actor_type, created_by_actor_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'NATURAL_LANGUAGE', 'ACTIVE', $6, $7, $8, $8)`,
			id, req.AccountID, p.SubjectID, name, stored, string(p.ActorType), p.SubjectID, now)
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
       s.source_kind, s.status, s.created_at, s.updated_at,
       coalesce(v.id::text, ''), coalesce(v.version, 0), coalesce(v.status, ''),
       coalesce(encode(v.ir_hash, 'hex'), ''), coalesce(v.effect_set, '{}'::text[]),
       coalesce(v.human_readable, ''), coalesce(v.ir, 'null'::jsonb), v.built_at, v.created_at`

func scanStrategy(row pgx.Row) (Strategy, error) {
	var (
		st                  Strategy
		vid, vstatus, vhash string
		vversion            int
		effects             []string
		human               string
		irDoc               []byte
		builtAt, vcreated   *time.Time
	)
	err := row.Scan(&st.ID, &st.AccountID, &st.OwnerUserID, &st.Name, &st.Description,
		&st.SourceKind, &st.Status, &st.CreatedAt, &st.UpdatedAt,
		&vid, &vversion, &vstatus, &vhash, &effects, &human, &irDoc, &builtAt, &vcreated)
	if err != nil {
		return Strategy{}, err
	}
	if vid != "" {
		v := StrategyVersion{
			ID: vid, Version: vversion, Status: vstatus, IRHashHex: vhash,
			EffectSet: effects, HumanReadable: human, IR: json.RawMessage(irDoc),
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

	refs, err := s.refs.Refs(ctx, s.db, s.clk.Now().UTC())
	if err != nil {
		return CompileOutcome{}, err
	}
	sum := sha256.Sum256([]byte(st.AccountID))
	res, err := s.compiler.CompileNL(ctx, strategy.NLRequest{
		StrategyID: sid, RequestID: requestID, OwnerAccountID: st.AccountID, OwnerUserID: st.OwnerUserID,
		Text: st.Description, Version: nextVersion, TenantHash: hex(sum[:]), Refs: refs,
	})
	if err != nil {
		return CompileOutcome{}, err
	}
	return s.persist(ctx, sid, requestID, res, p, correlationID)
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
func (s *StrategyService) persist(ctx context.Context, sid strategy.StrategyID, requestID string, res strategy.Result,
	p security.Principal, correlationID string,
) (CompileOutcome, error) {
	var out CompileOutcome
	err := s.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if res.Version != nil {
			if err := s.insertVersion(ctx, tx, res.Version); err != nil {
				return err
			}
		}
		for _, a := range res.Attempts {
			if err := s.insertAttempt(ctx, tx, sid, requestID, a, p, correlationID); err != nil {
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
	if res.Version != nil {
		out.Version = &StrategyVersion{
			ID: res.Version.ID.String(), Version: res.Version.Version, Status: res.Version.Status,
			IRHashHex: hex(res.Version.IRHash), EffectSet: res.Version.EffectSet,
			HumanReadable: res.Version.HumanReadable, BuiltAt: res.Version.BuiltAt,
		}
		if doc, merr := json.Marshal(res.Version.IR); merr == nil {
			out.Version.IR = doc
		}
	}
	return out, nil
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
		    model_budget, data_budget, envelope_requirements, human_readable, built_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7::text[], $8, $9, $10, $11, $12, $13,
		        '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, $14, $15)`,
		v.ID, v.StrategyID, v.Version, schemaVersion, doc, v.IRHash, v.EffectSet, v.Status, string(v.SourceKind),
		v.SourceHash, v.CompilerVersion, v.RiskPolicy, v.RiskPolicyHash, v.HumanReadable, v.BuiltAt)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agents: insert strategy version")
	}
	return nil
}

func (s *StrategyService) insertAttempt(ctx context.Context, tx pgx.Tx, sid strategy.StrategyID, requestID string,
	a strategy.Attempt, p security.Principal, correlationID string,
) error {
	codes := a.FailureCodes
	if codes == nil {
		codes = []string{}
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
		    parse_result, stage_reached, outcome, failure_codes,
		    structured_output, strategy_version_id, requested_by_user_id, correlation_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::text[], $14, $15, $16, $17, $18)`,
		a.ID, sid, requestID, a.AttemptNo, string(a.SourceKind), a.InputHash,
		nullText(a.Provenance.TemplateVersion), nullText(a.Provenance.Provider), nullText(a.Provenance.ModelID),
		parseResultOf(a), a.StageReached, a.Outcome, codes,
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
	if err := security.RequireAccount(ctx, accountID); err != nil {
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
	if err := security.RequireAccount(ctx, accountID); err != nil {
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
