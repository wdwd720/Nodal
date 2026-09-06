package strategy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/model"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// Attempt bounds. The database CHECK allows attempt_no 1..8; the compiler's
// own ceiling is lower, and configuration cannot raise it past the CHECK.
const (
	DefaultMaxAttempts = 3
	HardMaxAttempts    = 8
)

// Outcomes of a compile request (compile_attempts.outcome).
const (
	OutcomeSuccess             = "SUCCESS"
	OutcomeRejected            = "REJECTED"
	OutcomeNeedsClarification  = "NEEDS_CLARIFICATION"
	OutcomeModelUnavailable    = "MODEL_UNAVAILABLE"
	OutcomeTimeout             = "TIMEOUT"
	maxStructuredOutputBytes   = 256 * 1024
	compilerVersionUnspecified = "unspecified"
)

type (
	strategyKind struct{}
	versionKind  struct{}
	attemptKind  struct{}
)

// Typed identifiers.
type (
	StrategyID = id.ID[strategyKind]
	VersionID  = id.ID[versionKind]
	AttemptID  = id.ID[attemptKind]
)

// NewStrategyID returns a fresh strategy id.
func NewStrategyID() StrategyID { return id.New[strategyKind]() }

// NewVersionID returns a fresh version id.
func NewVersionID() VersionID { return id.New[versionKind]() }

// NewAttemptID returns a fresh compile attempt id.
func NewAttemptID() AttemptID { return id.New[attemptKind]() }

// ParseStrategyID parses the canonical form.
func ParseStrategyID(s string) (StrategyID, error) { return id.Parse[strategyKind](s) }

// ParseVersionID parses the canonical form.
func ParseVersionID(s string) (VersionID, error) { return id.Parse[versionKind](s) }

// ParseAttemptID parses the canonical form.
func ParseAttemptID(s string) (AttemptID, error) { return id.Parse[attemptKind](s) }

// Attempt is one row of compile_attempts: every attempt is recorded,
// successful or not, with the model provenance that produced it.
type Attempt struct {
	ID           AttemptID
	StrategyID   StrategyID
	RequestID    string
	AttemptNo    int
	SourceKind   ir.LineageSource
	InputHash    []byte
	StageReached string
	Outcome      string
	FailureCodes []string
	Fields       map[string]string
	// Clarifications is the model's own list of what it could not decide.
	Clarifications []string
	// Structured is the raw candidate body, persisted verbatim so a
	// rejected compile can be inspected later.
	Structured json.RawMessage
	Provenance model.Provenance
	VersionID  *VersionID
	CreatedAt  time.Time

	// rationaleValue is the model's structured explanation. It is unexported
	// so Attempt stays a record of what was persisted: the explanation is
	// surfaced on Result, not stored as a separate column.
	rationaleValue Rationale
}

// Version is a compiled strategy_versions row.
type Version struct {
	ID              VersionID
	StrategyID      StrategyID
	Version         int
	SchemaVersion   int
	IR              *ir.IR
	IRHash          []byte
	EffectSet       []string
	Status          string
	SourceKind      ir.LineageSource
	SourceHash      []byte
	CompilerVersion string
	SDKVersion      string
	AttemptID       *AttemptID
	RiskPolicy      string
	RiskPolicyHash  []byte
	HumanReadable   string
	BuiltAt         time.Time
}

// Version statuses (strategy_versions.status).
const (
	StatusCompiled   = "COMPILED"
	StatusAccepted   = "ACCEPTED"
	StatusRejected   = "REJECTED"
	StatusSuperseded = "SUPERSEDED"
	StatusRevoked    = "REVOKED"
)

// Result is the outcome of one compile request.
type Result struct {
	Version  *Version
	Attempts []Attempt
	Outcome  string
	Codes    []string
	// Clarifications is non-empty when the request was too ambiguous to
	// compile; no version exists in that case.
	Clarifications []string
	// Rationale is the structured explanation shown alongside the rendered
	// strategy. Never chain-of-thought.
	Rationale Rationale
}

// Config configures the compiler.
type Config struct {
	// MaxAttempts is the per-request attempt ceiling, clamped to
	// HardMaxAttempts so configuration cannot exceed the database CHECK.
	MaxAttempts int
	// MaxOutputTokens caps each model response.
	MaxOutputTokens int64
	// ModelID names the model; empty uses the provider default.
	ModelID string
	// CompilerVersion is recorded on every version so a document can be
	// traced to the code that produced it.
	CompilerVersion string
	// Budget bounds spend across the whole request.
	Budget model.Budget
	// Prices estimates cost before each call.
	Prices model.PriceTable
}

func (c Config) attempts() int {
	n := c.MaxAttempts
	if n <= 0 {
		n = DefaultMaxAttempts
	}
	if n > HardMaxAttempts {
		n = HardMaxAttempts
	}
	return n
}

// Compiler turns a request into a validated strategy version.
type Compiler struct {
	provider model.Provider
	clk      clock.Clock
	cfg      Config
}

// NewCompiler builds a compiler. The provider is wrapped in the credential
// guard here, so there is no constructor that yields an unguarded compiler.
func NewCompiler(provider model.Provider, clk clock.Clock, cfg Config) *Compiler {
	if clk == nil {
		clk = clock.System()
	}
	if cfg.CompilerVersion == "" {
		cfg.CompilerVersion = compilerVersionUnspecified
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = 16000
	}
	if len(cfg.Prices) == 0 {
		cfg.Prices = model.DefaultPriceTable()
	}
	return &Compiler{provider: model.NewGuarded(provider, model.NewGuard()), clk: clk, cfg: cfg}
}

// NLRequest is a natural-language compile request.
type NLRequest struct {
	StrategyID     StrategyID
	RequestID      string
	OwnerAccountID string
	OwnerUserID    string
	Text           string
	// Version is the version number this compile would produce.
	Version int
	// TenantHash is the opaque per-user identifier sent to the provider.
	TenantHash string
	Refs       ValidationRefs
}

// DocumentRequest is the SDK or clone path: a document already exists, so
// the pipeline starts at PARSE and no model is involved.
type DocumentRequest struct {
	StrategyID     StrategyID
	RequestID      string
	OwnerAccountID string
	OwnerUserID    string
	Document       []byte
	Version        int
	Source         ir.LineageSource
	SDKVersion     string
	Refs           ValidationRefs
}

// CompileNL runs the natural-language pipeline (STRATEGY_IR.md §4).
//
// Every attempt is recorded. A retry feeds the previous validation errors
// back as tool results, never as instructions. Terminal rejections — a
// forbidden effect, a risk incompatibility — stop the loop immediately:
// they are decisions about what the platform permits, and retrying them
// would be asking the model to negotiate with the policy.
func (c *Compiler) CompileNL(ctx context.Context, req NLRequest) (Result, error) {
	if err := validateNLRequest(req); err != nil {
		return Result{}, err
	}
	inputSum := sha256.Sum256([]byte(req.Text))
	result := Result{Outcome: OutcomeRejected}
	budget := c.cfg.Budget
	var feedback []AttemptFeedback

	for attemptNo := 1; attemptNo <= c.cfg.attempts(); attemptNo++ {
		modelReq := BuildRequest(req.Text, feedback, c.cfg.MaxOutputTokens, c.cfg.ModelID, req.TenantHash)

		// The budget is checked before the provider is contacted, so an
		// exhausted allowance costs nothing.
		if err := c.checkBudget(budget, modelReq); err != nil {
			attempt := c.failedAttempt(req, attemptNo, inputSum[:], StagePrompt, OutcomeModelUnavailable, modelReq, err)
			result.Attempts = append(result.Attempts, attempt)
			result.Outcome = OutcomeModelUnavailable
			result.Codes = []string{string(errs.CodeOf(err))}
			return result, nil
		}

		requestedAt := c.clk.Now().UTC()
		resp, err := c.provider.Complete(ctx, modelReq)
		if err != nil {
			attempt := c.failedAttempt(req, attemptNo, inputSum[:], StageResponse, outcomeForError(err), modelReq, err)
			attempt.Provenance = model.NewFailureProvenance(modelReq, c.provider.Name(), requestedAt, c.clk.Now().UTC(), err)
			result.Attempts = append(result.Attempts, attempt)
			// A provider failure is never a synthetic answer (PART 177).
			result.Outcome = outcomeForError(err)
			result.Codes = []string{string(errs.CodeOf(err))}
			return result, nil
		}
		if b, cerr := budget.Consume(resp.Usage.Cost); cerr == nil {
			budget = b
		}

		attempt, version, report, done := c.processResponse(req, attemptNo, inputSum[:], modelReq, resp)
		result.Attempts = append(result.Attempts, attempt)
		result.Rationale = attempt.rationale()
		result.Clarifications = attempt.Clarifications

		switch {
		case done && version != nil:
			result.Version = version
			result.Outcome = OutcomeSuccess
			result.Codes = nil
			return result, nil
		case attempt.Outcome == OutcomeNeedsClarification:
			result.Outcome = OutcomeNeedsClarification
			result.Codes = nil
			return result, nil
		case report.Terminal():
			// Not retried: the answer to "may this strategy move money" does
			// not change because it was asked again.
			result.Outcome = OutcomeRejected
			result.Codes = report.Codes()
			return result, nil
		default:
			result.Codes = report.Codes()
			feedback = append(feedback, FeedbackFrom(attempt.ID.String(), report))
		}
	}
	return result, nil
}

// CompileDocument runs the SDK and clone path: PARSE onward, no model.
// Validation from PARSE is identical to the natural-language path, so an
// SDK document gets no additional trust for having been typed by a human.
func (c *Compiler) CompileDocument(ctx context.Context, req DocumentRequest) (Result, error) {
	if req.StrategyID.IsZero() {
		return Result{}, errs.New(errs.CodeValidationFailed, "strategy: document request needs a strategy id")
	}
	if len(req.Document) == 0 {
		return Result{}, errs.New(errs.CodeValidationFailed, "strategy: document request needs a document")
	}
	source := req.Source
	if source == "" {
		source = ir.SourceTypeScriptSDK
	}
	sum := sha256.Sum256(req.Document)
	attempt := Attempt{
		ID:         NewAttemptID(),
		StrategyID: req.StrategyID,
		RequestID:  req.RequestID,
		AttemptNo:  1,
		SourceKind: source,
		InputHash:  sum[:],
		Structured: append(json.RawMessage(nil), req.Document...),
		CreatedAt:  c.clk.Now().UTC(),
		Provenance: model.Provenance{
			TemplateVersion: "none/sdk",
			Purpose:         model.PurposeCompile,
			ParseResult:     model.ParseNotAttempted,
			RequestedAt:     c.clk.Now().UTC(),
			Usage:           model.Usage{Cost: money.USDFromMinor(0)},
		},
	}

	doc, err := ir.Decode(req.Document)
	if err != nil {
		attempt.StageReached = StageParse
		attempt.Outcome = OutcomeRejected
		attempt.FailureCodes = []string{ir.CodeParseFailed}
		attempt.Provenance.ParseResult = model.ParseInvalidJSON
		return Result{Attempts: []Attempt{attempt}, Outcome: OutcomeRejected, Codes: attempt.FailureCodes}, nil
	}
	attempt.Provenance.ParseResult = model.ParseOK

	version, report := c.finalize(doc, finalizeInput{
		StrategyID:     req.StrategyID,
		OwnerAccountID: req.OwnerAccountID,
		OwnerUserID:    req.OwnerUserID,
		Version:        req.Version,
		Source:         source,
		SourceHash:     sum[:],
		SDKVersion:     req.SDKVersion,
		AttemptID:      attempt.ID,
		Refs:           req.Refs,
	})
	attempt.StageReached = report.Stage
	attempt.FailureCodes = report.Codes()
	attempt.Fields = report.Fields()
	if version == nil {
		attempt.Outcome = OutcomeRejected
		return Result{Attempts: []Attempt{attempt}, Outcome: OutcomeRejected, Codes: report.Codes()}, nil
	}
	attempt.Outcome = OutcomeSuccess
	attempt.VersionID = &version.ID
	return Result{Version: version, Attempts: []Attempt{attempt}, Outcome: OutcomeSuccess}, nil
}

// processResponse decodes and validates one model answer.
func (c *Compiler) processResponse(req NLRequest, attemptNo int, inputHash []byte, modelReq model.Request, resp model.Response) (Attempt, *Version, Report, bool) {
	attempt := Attempt{
		ID:         NewAttemptID(),
		StrategyID: req.StrategyID,
		RequestID:  req.RequestID,
		AttemptNo:  attemptNo,
		SourceKind: ir.SourceNaturalLanguage,
		InputHash:  inputHash,
		Structured: resp.Structured,
		CreatedAt:  c.clk.Now().UTC(),
	}

	if len(resp.Structured) > maxStructuredOutputBytes {
		attempt.Provenance = model.NewProvenance(modelReq, resp, model.ParseTooLarge)
		attempt.StageReached = StageParse
		attempt.Outcome = OutcomeRejected
		attempt.FailureCodes = []string{ir.CodeParseFailed}
		return attempt, nil, Report{Stage: StageParse, Issues: []ir.Issue{{Code: ir.CodeParseFailed, Detail: "response exceeds the size cap"}}}, false
	}

	var candidate CandidateResponse
	if err := json.Unmarshal(resp.Structured, &candidate); err != nil {
		// Malformed JSON is never repaired; the attempt is recorded and the
		// next one starts from the same user text.
		attempt.Provenance = model.NewProvenance(modelReq, resp, model.ParseInvalidJSON)
		attempt.StageReached = StageParse
		attempt.Outcome = OutcomeRejected
		attempt.FailureCodes = []string{ir.CodeParseFailed}
		return attempt, nil, Report{Stage: StageParse, Issues: []ir.Issue{{Code: ir.CodeParseFailed, Detail: err.Error()}}}, false
	}
	attempt.Provenance = model.NewProvenance(modelReq, resp, model.ParseOK)
	attempt.Clarifications = candidate.ClarificationsNeeded
	attempt.rationaleValue = candidate.Rationale

	// A model that says it could not decide is believed: an ambiguous
	// request ends the compile rather than producing a guess (PART 170,
	// "ambiguous natural language").
	if len(candidate.ClarificationsNeeded) > 0 {
		attempt.StageReached = StageResponse
		attempt.Outcome = OutcomeNeedsClarification
		return attempt, nil, Report{Stage: StageResponse}, false
	}

	doc, err := ir.Decode(candidate.IR)
	if err != nil {
		attempt.StageReached = StageParse
		attempt.Outcome = OutcomeRejected
		attempt.FailureCodes = []string{ir.CodeParseFailed}
		attempt.Provenance.ParseResult = model.ParseSchemaViolation
		var ve *ir.ValidationError
		issues := []ir.Issue{{Code: ir.CodeParseFailed, Detail: err.Error()}}
		if errors.As(err, &ve) {
			issues = ve.Issues
		}
		return attempt, nil, Report{Stage: StageParse, Issues: issues}, false
	}

	version, report := c.finalize(doc, finalizeInput{
		StrategyID:     req.StrategyID,
		OwnerAccountID: req.OwnerAccountID,
		OwnerUserID:    req.OwnerUserID,
		Version:        req.Version,
		Source:         ir.SourceNaturalLanguage,
		SourceHash:     inputHash,
		AttemptID:      attempt.ID,
		Refs:           req.Refs,
	})
	attempt.StageReached = report.Stage
	attempt.FailureCodes = report.Codes()
	attempt.Fields = report.Fields()
	if version == nil {
		attempt.Outcome = OutcomeRejected
		return attempt, nil, report, false
	}
	attempt.Outcome = OutcomeSuccess
	attempt.VersionID = &version.ID
	return attempt, version, report, true
}

type finalizeInput struct {
	StrategyID     StrategyID
	OwnerAccountID string
	OwnerUserID    string
	Version        int
	Source         ir.LineageSource
	SourceHash     []byte
	SDKVersion     string
	AttemptID      AttemptID
	Refs           ValidationRefs
}

// finalize stamps the fields this package owns, validates, and renders.
//
// The stamping happens before validation on purpose: whatever the model (or
// an SDK author) put in strategy_id, owner, version, hash, built_at or
// lineage is discarded, so a document cannot claim to belong to another
// account or to carry an already-approved hash.
func (c *Compiler) finalize(doc *ir.IR, in finalizeInput) (*Version, Report) {
	doc.StrategyID = in.StrategyID.String()
	doc.Owner = ir.Owner{AccountID: in.OwnerAccountID, UserID: in.OwnerUserID}
	doc.Version = in.Version
	doc.SchemaVersion = ir.SchemaVersion
	doc.Hash = nil
	doc.BuiltAt = c.clk.Now().UTC()
	doc.Lineage = ir.Lineage{
		Source:           in.Source,
		SourceHash:       in.SourceHash,
		CompileAttemptID: in.AttemptID.String(),
		CompilerVersion:  c.cfg.CompilerVersion,
		SDKVersion:       in.SDKVersion,
	}
	if in.Refs.Policy.Version != "" {
		doc.RiskPolicy.Version = in.Refs.Policy.Version
	}
	doc.Normalize()

	report := Validate(doc, in.Refs)
	if !report.OK() {
		return nil, report
	}

	// The hash is computed after validation, over the document that passed.
	hash, err := ir.SemanticHash(doc)
	if err != nil {
		return nil, Report{Stage: StageRender, Issues: []ir.Issue{{Code: ir.CodeParseFailed, Field: "hash", Detail: err.Error()}}}
	}
	doc.Hash = hash

	human, err := Render(doc)
	if err != nil {
		return nil, Report{Stage: StageRender, Issues: []ir.Issue{{Code: ir.CodeParseFailed, Field: "render", Detail: err.Error()}}}
	}

	policyHash, _ := hexDecode(in.Refs.PolicyHash)
	return &Version{
		ID:              NewVersionID(),
		StrategyID:      in.StrategyID,
		Version:         in.Version,
		SchemaVersion:   ir.SchemaVersion,
		IR:              doc,
		IRHash:          hash,
		EffectSet:       ir.EffectStrings(doc.Effects),
		Status:          StatusCompiled,
		SourceKind:      in.Source,
		SourceHash:      in.SourceHash,
		CompilerVersion: c.cfg.CompilerVersion,
		SDKVersion:      in.SDKVersion,
		AttemptID:       &in.AttemptID,
		RiskPolicy:      doc.RiskPolicy.Version,
		RiskPolicyHash:  policyHash,
		HumanReadable:   human,
		BuiltAt:         doc.BuiltAt,
	}, Report{Stage: StageAccepted}
}

func (c *Compiler) checkBudget(b model.Budget, req model.Request) error {
	if b.MaxCalls == 0 && b.MaxSpend.IsZero() {
		return nil // no budget configured: the caller is not metering this path
	}
	rendered, err := req.Render()
	if err != nil {
		return err
	}
	modelID := req.Model
	if modelID == "" {
		modelID = model.DefaultModel
	}
	estimate, err := c.cfg.Prices.EstimateCost(modelID, model.EstimateInputTokens(rendered), req.MaxOutputTokens)
	if err != nil {
		// An unpriced model cannot be budgeted, so it is not called.
		return err
	}
	return b.CheckBeforeCall(estimate)
}

func (c *Compiler) failedAttempt(req NLRequest, attemptNo int, inputHash []byte, stage, outcome string, modelReq model.Request, err error) Attempt {
	return Attempt{
		ID:           NewAttemptID(),
		StrategyID:   req.StrategyID,
		RequestID:    req.RequestID,
		AttemptNo:    attemptNo,
		SourceKind:   ir.SourceNaturalLanguage,
		InputHash:    inputHash,
		StageReached: stage,
		Outcome:      outcome,
		FailureCodes: []string{string(errs.CodeOf(err))},
		CreatedAt:    c.clk.Now().UTC(),
		Provenance: model.Provenance{
			Provider:        c.provider.Name(),
			ModelID:         modelReq.Model,
			TemplateVersion: modelReq.TemplateVersion,
			Purpose:         model.PurposeCompile,
			RequestedAt:     c.clk.Now().UTC(),
			ParseResult:     model.ParseNotAttempted,
			Usage:           model.Usage{Cost: money.USDFromMinor(0)},
			Success:         false,
			ErrorCode:       string(errs.CodeOf(err)),
		},
	}
}

// rationaleValue is stored unexported so Attempt stays a plain record while
// the compiler can still surface the explanation on the Result.
func (a Attempt) rationale() Rationale { return a.rationaleValue }

func outcomeForError(err error) string {
	switch errs.CodeOf(err) {
	case errs.CodeRateLimited:
		return OutcomeModelUnavailable
	case errs.CodeSecretInModelContext, errs.CodeValidationFailed:
		return OutcomeRejected
	default:
		return OutcomeModelUnavailable
	}
}

func validateNLRequest(req NLRequest) error {
	fields := map[string]any{}
	if req.StrategyID.IsZero() {
		fields["strategy_id"] = "required"
	}
	if req.RequestID == "" {
		fields["request_id"] = "required: attempts are unique per (request_id, attempt_no)"
	}
	if len(req.Text) == 0 {
		fields["text"] = "required"
	}
	if len(req.Text) > model.MaxSegmentBytes {
		fields["text"] = fmt.Sprintf("longer than %d bytes", model.MaxSegmentBytes)
	}
	if req.Version < 1 {
		fields["version"] = "must be >= 1"
	}
	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "strategy: invalid compile request").WithFields(fields)
	}
	return nil
}

func hexDecode(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		var hi, lo byte
		var err error
		if hi, err = hexNibble(s[2*i]); err != nil {
			return nil, err
		}
		if lo, err = hexNibble(s[2*i+1]); err != nil {
			return nil, err
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, fmt.Errorf("strategy: %q is not a hex digit", c)
}
