package proof

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// BundleRef names the record a proof bundle is built around: exactly one
// of a fill id or a trade intent id.
type BundleRef struct {
	FillID   string `json:"fill_id,omitempty"`
	IntentID string `json:"intent_id,omitempty"`
}

// Bundle is the PART 88 proof content for one fill or intent: persisted
// hashes and references for every link of the decision-to-settlement chain,
// the audit events that recorded them, and a Merkle membership proof for
// every event already covered by a checkpoint. Links that do not exist are
// listed in Absent; nothing is ever synthesized.
type Bundle struct {
	GeneratedAt     time.Time           `json:"generated_at"`
	BuildVersion    string              `json:"build_version"`
	Ref             BundleRef           `json:"ref"`
	StrategyVersion *StrategyVersionRef `json:"strategy_version,omitempty"`
	Model           *ModelRef           `json:"model,omitempty"`
	Prediction      *PredictionRef      `json:"prediction,omitempty"`
	Eligibility     []EligibilityRef    `json:"eligibility_decisions,omitempty"`
	Risk            []RiskRef           `json:"risk_decisions,omitempty"`
	Intent          *IntentRef          `json:"intent,omitempty"`
	Plan            *PlanRef            `json:"plan,omitempty"`
	Quote           *QuoteRef           `json:"quote,omitempty"`
	Order           *OrderRef           `json:"order,omitempty"`
	Attempts        []AttemptRef        `json:"execution_attempts,omitempty"`
	Fills           []FillRef           `json:"fills,omitempty"`
	Journal         []JournalRef        `json:"journal_transactions,omitempty"`
	AuditEvents     []AuditEventRef     `json:"audit_events"`
	Absent          []string            `json:"absent"`
}

// StrategyVersionRef is strategy_versions: the compiled IR hash (PART 88
// "Strategy IR") and its provenance.
type StrategyVersionRef struct {
	ID                string `json:"id"`
	StrategyID        string `json:"strategy_id"`
	Version           int32  `json:"version"`
	Status            string `json:"status"`
	IRHash            []byte `json:"ir_hash"`
	SourceHash        []byte `json:"source_hash"`
	CompilerVersion   string `json:"compiler_version"`
	RiskPolicyVersion string `json:"risk_policy_version"`
	RiskPolicyHash    []byte `json:"risk_policy_hash"`
}

// ModelRef is model_calls: the model identifier and the input/output hashes.
type ModelRef struct {
	ID                    string    `json:"id"`
	Provider              string    `json:"provider"`
	ModelID               string    `json:"model_id"`
	PromptTemplateVersion string    `json:"prompt_template_version"`
	InputHash             []byte    `json:"input_hash"`
	OutputHash            []byte    `json:"output_hash"`
	PromptRef             *string   `json:"prompt_ref"`
	ResponseRef           *string   `json:"response_ref"`
	RequestAt             time.Time `json:"request_at"`
}

// PredictionRef is predictions: the information set hash and linkage.
type PredictionRef struct {
	ID                 string    `json:"id"`
	AgentID            string    `json:"agent_id"`
	AgentVersion       int64     `json:"agent_version"`
	StrategyVersionID  string    `json:"strategy_version_id"`
	Mode               string    `json:"mode"`
	InformationSetHash []byte    `json:"information_set_hash"`
	ModelCallID        *string   `json:"model_call_id"`
	TemplateVersion    *string   `json:"template_version"`
	CommittedAt        time.Time `json:"committed_at"`
}

// EligibilityRef is eligibility_decisions.
type EligibilityRef struct {
	ID            string    `json:"id"`
	Eligible      bool      `json:"eligible"`
	PolicyVersion string    `json:"policy_version"`
	ContextHash   []byte    `json:"context_hash"`
	ReasonCodes   []string  `json:"reason_codes"`
	EvaluatedAt   time.Time `json:"evaluated_at"`
}

// RiskRef is risk_decisions.
type RiskRef struct {
	ID               string    `json:"id"`
	Stage            string    `json:"stage"`
	PolicyVersion    string    `json:"policy_version"`
	PolicyHash       []byte    `json:"policy_hash"`
	Decision         string    `json:"decision"`
	ReasonCodes      []string  `json:"reason_codes"`
	EvaluatorVersion string    `json:"evaluator_version"`
	QuoteID          *string   `json:"quote_id"`
	EvaluatedAt      time.Time `json:"evaluated_at"`
}

// IntentRef is trade_intents with its content hash and links.
type IntentRef struct {
	ID                    string    `json:"id"`
	AccountID             string    `json:"account_id"`
	ActorType             string    `json:"actor_type"`
	ActorID               string    `json:"actor_id"`
	AgentID               *string   `json:"agent_id"`
	StrategyVersionID     *string   `json:"strategy_version_id"`
	PredictionID          *string   `json:"prediction_id"`
	Action                string    `json:"action"`
	Mode                  string    `json:"mode"`
	Status                string    `json:"status"`
	CorrelationID         string    `json:"correlation_id"`
	ContentHash           []byte    `json:"content_hash"`
	EligibilityDecisionID *string   `json:"eligibility_decision_id"`
	RiskDecisionID        *string   `json:"risk_decision_id"`
	PlanID                *string   `json:"plan_id"`
	OrderID               *string   `json:"order_id"`
	RequestedAt           time.Time `json:"requested_at"`
}

// PlanRef is execution_plans with its plan hash.
type PlanRef struct {
	ID             string          `json:"id"`
	Version        int32           `json:"version"`
	PlannerVersion string          `json:"planner_version"`
	Status         string          `json:"status"`
	PlanHash       []byte          `json:"plan_hash"`
	RiskDecisionID *string         `json:"risk_decision_id"`
	QuoteID        *string         `json:"quote_id"`
	PolicyVersions json.RawMessage `json:"policy_versions"`
	ApprovedAt     *time.Time      `json:"approved_at"`
}

// QuoteRef is quotes with the raw-response and route hashes.
type QuoteRef struct {
	ID                string    `json:"id"`
	Provider          string    `json:"provider"`
	ProviderRequestID *string   `json:"provider_request_id"`
	RouteHash         []byte    `json:"route_hash"`
	RawResponseHash   []byte    `json:"raw_response_hash"`
	RawResponseRef    *string   `json:"raw_response_ref"`
	ReceivedAt        time.Time `json:"received_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// OrderRef is orders.
type OrderRef struct {
	ID            string    `json:"id"`
	IntentID      string    `json:"intent_id"`
	PlanID        string    `json:"plan_id"`
	QuoteID       string    `json:"quote_id"`
	Status        string    `json:"status"`
	CorrelationID string    `json:"correlation_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// AttemptRef is execution_attempts: the transaction hashes and the external
// transaction signature, with the signing decision and result when present.
type AttemptRef struct {
	ID              string              `json:"id"`
	AttemptNo       int32               `json:"attempt_no"`
	Status          string              `json:"status"`
	UnsignedTxHash  []byte              `json:"unsigned_tx_hash"`
	SignedTxHash    []byte              `json:"signed_tx_hash"`
	TxSignature     *string             `json:"tx_signature"`
	CorrelationID   string              `json:"correlation_id"`
	SubmittedAt     *time.Time          `json:"submitted_at"`
	FinalizedAt     *time.Time          `json:"finalized_at"`
	SigningDecision *SigningDecisionRef `json:"signing_decision,omitempty"`
	SigningResult   *SigningResultRef   `json:"signing_result,omitempty"`
}

// SigningDecisionRef is signing_decisions.
type SigningDecisionRef struct {
	ID               string    `json:"id"`
	Decision         string    `json:"decision"`
	ExpectedTxHash   []byte    `json:"expected_tx_hash"`
	InspectedTxHash  []byte    `json:"inspected_tx_hash"`
	InspectorVersion string    `json:"inspector_version"`
	DecidedAt        time.Time `json:"decided_at"`
}

// SigningResultRef is signing_results: the signed transaction hash and the
// 64-byte signature.
type SigningResultRef struct {
	ID              string    `json:"id"`
	SignedTxHash    []byte    `json:"signed_tx_hash"`
	Signature       []byte    `json:"signature"`
	ProviderSignRef *string   `json:"provider_sign_ref"`
	SignedAt        time.Time `json:"signed_at"`
}

// FillRef is fills.
type FillRef struct {
	ID                   string    `json:"id"`
	OrderID              string    `json:"order_id"`
	AttemptID            *string   `json:"attempt_id"`
	Venue                string    `json:"venue"`
	ExternalFillID       string    `json:"external_fill_id"`
	TxSignature          *string   `json:"tx_signature"`
	Slot                 *int64    `json:"slot"`
	Source               string    `json:"source"`
	Finality             string    `json:"finality"`
	ObservedAt           time.Time `json:"observed_at"`
	RawRef               *string   `json:"raw_ref"`
	JournalTransactionID *string   `json:"journal_transaction_id"`
}

// JournalRef is journal_transactions with its content hash.
type JournalRef struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	IdempotencyKey string    `json:"idempotency_key"`
	ReferenceType  string    `json:"reference_type"`
	ReferenceID    string    `json:"reference_id"`
	ContentHash    []byte    `json:"content_hash"`
	PostedAt       time.Time `json:"posted_at"`
	BuildVersion   *string   `json:"build_version"`
}

// AuditEventRef is one audit_events row with its chain hashes and, when the
// row is already under a checkpoint, its membership proof.
type AuditEventRef struct {
	ID            string      `json:"id"`
	Stream        string      `json:"stream"`
	StreamSeq     int64       `json:"stream_seq"`
	ActorType     string      `json:"actor_type"`
	ActorID       string      `json:"actor_id"`
	Action        string      `json:"action"`
	ResourceType  string      `json:"resource_type"`
	ResourceID    string      `json:"resource_id"`
	CorrelationID *string     `json:"correlation_id"`
	OccurredAt    time.Time   `json:"occurred_at"`
	ContentHash   []byte      `json:"content_hash"`
	PrevHash      []byte      `json:"prev_hash"`
	BuildVersion  *string     `json:"build_version"`
	Checkpoint    *Membership `json:"checkpoint,omitempty"`
}

// Membership is the Merkle membership of one event in a checkpoint. Verified
// reports that the proof was checked against the checkpoint's merkle_root
// while the bundle was built; Error carries the reason when it was not.
type Membership struct {
	CheckpointID  string `json:"checkpoint_id"`
	CheckpointSeq int64  `json:"checkpoint_seq"`
	MerkleRoot    []byte `json:"merkle_root"`
	Proof         Proof  `json:"proof"`
	Verified      bool   `json:"verified"`
	Error         string `json:"error,omitempty"`
}

// Bundler builds proof bundles.
type Bundler struct {
	clk          clock.Clock
	buildVersion string
}

// NewBundler wires a Bundler (config.BuildVersion when buildVersion is
// empty).
func NewBundler(clk clock.Clock, buildVersion string) (*Bundler, error) {
	if clk == nil {
		return nil, errors.New("proof: bundler requires a clock")
	}
	if buildVersion == "" {
		buildVersion = config.BuildVersion
	}
	return &Bundler{clk: clk, buildVersion: buildVersion}, nil
}

// ResolveRef classifies idText as a fill id or an intent id by looking both
// tables up. It returns NOT_FOUND when neither holds the id.
func ResolveRef(ctx context.Context, q db.Querier, idText string) (BundleRef, error) {
	if _, err := id.ParseAny(idText); err != nil {
		return BundleRef{}, errs.Newf(errs.CodeValidationFailed, "proof: %q is not a canonical id", idText).WithField("field", "id")
	}
	var isFill, isIntent bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fills WHERE id = $1), EXISTS (SELECT 1 FROM trade_intents WHERE id = $1)`, idText).
		Scan(&isFill, &isIntent); err != nil {
		return BundleRef{}, errs.Wrap(err, errs.CodeInternal, "proof: resolve bundle ref")
	}
	switch {
	case isFill:
		return BundleRef{FillID: idText}, nil
	case isIntent:
		return BundleRef{IntentID: idText}, nil
	}
	return BundleRef{}, errs.Newf(errs.CodeNotFound, "proof: no fill or intent with id %s", idText)
}

// bundleBuild carries state while one bundle is assembled.
type bundleBuild struct {
	ctx         context.Context
	q           db.Querier
	out         *Bundle
	resourceIDs []string
	corrIDs     []string
	trees       map[string]bundleTree
}

type bundleTree struct {
	tree    *Tree
	offsets map[string]int
	cp      Checkpoint
}

func (b *bundleBuild) absent(link, reason string) {
	b.out.Absent = append(b.out.Absent, link+": "+reason)
}

func (b *bundleBuild) resource(ids ...string) {
	for _, s := range ids {
		if s != "" {
			b.resourceIDs = append(b.resourceIDs, s)
		}
	}
}

func (b *bundleBuild) correlation(ids ...*string) {
	for _, s := range ids {
		if s != nil && *s != "" {
			b.corrIDs = append(b.corrIDs, *s)
		}
	}
}

// Bundle assembles the proof bundle for ref.
func (bl *Bundler) Bundle(ctx context.Context, q db.Querier, ref BundleRef) (Bundle, error) {
	if (ref.FillID == "") == (ref.IntentID == "") {
		return Bundle{}, errs.New(errs.CodeValidationFailed, "proof: bundle ref needs exactly one of fill_id or intent_id")
	}
	for name, v := range map[string]string{"fill_id": ref.FillID, "intent_id": ref.IntentID} {
		if v != "" {
			if _, err := id.ParseAny(v); err != nil {
				return Bundle{}, errs.Newf(errs.CodeValidationFailed, "proof: %s is not a canonical id", name).WithField("field", name)
			}
		}
	}
	out := Bundle{GeneratedAt: bl.clk.Now(), BuildVersion: bl.buildVersion, Ref: ref, AuditEvents: []AuditEventRef{}, Absent: []string{}}
	b := &bundleBuild{ctx: ctx, q: q, out: &out, trees: map[string]bundleTree{}}
	if err := b.collect(ref); err != nil {
		return Bundle{}, err
	}
	return out, nil
}

func (b *bundleBuild) collect(ref BundleRef) error {
	var (
		order   *OrderRef
		intent  *IntentRef
		fills   []FillRef
		err     error
		orderID string
	)
	if ref.FillID != "" {
		fill, found, ferr := loadFill(b.ctx, b.q, ref.FillID)
		if ferr != nil {
			return ferr
		}
		if !found {
			return errs.Newf(errs.CodeNotFound, "proof: fill %s not found", ref.FillID)
		}
		orderID = fill.OrderID
		if order, err = b.loadOrder(`WHERE id = $1`, orderID, "order"); err != nil {
			return err
		}
		if order == nil {
			b.absent("intent", "order "+orderID+" not found, so the intent cannot be reached")
		} else if intent, err = b.loadIntent(order.IntentID); err != nil {
			return err
		}
	} else {
		if intent, err = b.loadIntent(ref.IntentID); err != nil {
			return err
		}
		if intent == nil {
			return errs.Newf(errs.CodeNotFound, "proof: intent %s not found", ref.IntentID)
		}
		if order, err = b.loadOrder(`WHERE intent_id = $1`, intent.ID, "order"); err != nil {
			return err
		}
		if order != nil {
			orderID = order.ID
		}
	}
	if orderID != "" {
		fills, err = loadFills(b.ctx, b.q, orderID)
		if err != nil {
			return err
		}
		if len(fills) == 0 {
			b.absent("fills", "order "+orderID+" has no fills")
		}
	}
	b.out.Intent, b.out.Order, b.out.Fills = intent, order, fills
	if order != nil {
		b.resource(order.ID)
		b.correlation(&order.CorrelationID)
	}
	for _, f := range fills {
		b.resource(f.ID)
	}
	if err := b.collectIntentLinks(intent); err != nil {
		return err
	}
	if err := b.collectExecution(order, fills); err != nil {
		return err
	}
	if err := b.collectAuditEvents(); err != nil {
		return err
	}
	sort.Strings(b.out.Absent)
	return nil
}

func (b *bundleBuild) collectIntentLinks(intent *IntentRef) error {
	if intent == nil {
		return nil
	}
	b.resource(intent.ID)
	b.correlation(&intent.CorrelationID)
	planID := intent.PlanID
	if planID == nil && b.out.Order != nil {
		planID = &b.out.Order.PlanID
	}
	if planID == nil {
		b.absent("plan", "intent has no plan_id and no order names one")
	} else {
		plan, err := loadPlan(b.ctx, b.q, *planID)
		if err != nil {
			return err
		}
		if plan == nil {
			b.absent("plan", "execution_plans row "+*planID+" not found")
		} else {
			b.out.Plan = plan
			b.resource(plan.ID)
		}
	}
	var quoteID *string
	switch {
	case b.out.Order != nil:
		quoteID = &b.out.Order.QuoteID
	case b.out.Plan != nil && b.out.Plan.QuoteID != nil:
		quoteID = b.out.Plan.QuoteID
	}
	if quoteID == nil {
		b.absent("quote", "neither the order nor the plan names a quote")
	} else {
		quote, err := loadQuote(b.ctx, b.q, *quoteID)
		if err != nil {
			return err
		}
		if quote == nil {
			b.absent("quote", "quotes row "+*quoteID+" not found")
		} else {
			b.out.Quote = quote
			b.resource(quote.ID)
		}
	}
	// Decisions are reached both ways: rows that name the intent, and the
	// decision ids the intent and plan name (a decision evaluated before the
	// intent row existed carries no intent_id).
	riskIDs := []*string{intent.RiskDecisionID}
	if b.out.Plan != nil {
		riskIDs = append(riskIDs, b.out.Plan.RiskDecisionID)
	}
	risk, err := loadRisk(b.ctx, b.q, intent.ID, riskIDs)
	if err != nil {
		return err
	}
	if len(risk) == 0 {
		b.absent("risk_decisions", "no risk_decisions row names intent "+intent.ID+" and the intent names none")
	}
	b.out.Risk = risk
	for _, r := range risk {
		b.resource(r.ID)
	}
	elig, err := loadEligibility(b.ctx, b.q, intent.ID, []*string{intent.EligibilityDecisionID})
	if err != nil {
		return err
	}
	if len(elig) == 0 {
		b.absent("eligibility_decisions", "no eligibility_decisions row names intent "+intent.ID+" and the intent names none")
	}
	b.out.Eligibility = elig
	for _, e := range elig {
		b.resource(e.ID)
	}
	strategyVersionID := intent.StrategyVersionID
	if intent.PredictionID == nil {
		b.absent("prediction", "intent has no prediction_id (manual or non-agent intent)")
	} else {
		pred, err := loadPrediction(b.ctx, b.q, *intent.PredictionID)
		if err != nil {
			return err
		}
		if pred == nil {
			b.absent("prediction", "predictions row "+*intent.PredictionID+" not found")
		} else {
			b.out.Prediction = pred
			b.resource(pred.ID)
			if strategyVersionID == nil {
				strategyVersionID = &pred.StrategyVersionID
			}
			if pred.ModelCallID == nil {
				b.absent("model", "prediction has no model_call_id")
			} else {
				model, err := loadModelCall(b.ctx, b.q, *pred.ModelCallID)
				if err != nil {
					return err
				}
				if model == nil {
					b.absent("model", "model_calls row "+*pred.ModelCallID+" not found")
				} else {
					b.out.Model = model
				}
			}
		}
	}
	if b.out.Prediction == nil && b.out.Model == nil && intent.PredictionID == nil {
		b.absent("model", "no prediction, so no model call")
	}
	if strategyVersionID == nil {
		b.absent("strategy_version", "intent has no strategy_version_id and no prediction names one")
	} else {
		sv, err := loadStrategyVersion(b.ctx, b.q, *strategyVersionID)
		if err != nil {
			return err
		}
		if sv == nil {
			b.absent("strategy_version", "strategy_versions row "+*strategyVersionID+" not found")
		} else {
			b.out.StrategyVersion = sv
		}
	}
	return nil
}

func (b *bundleBuild) collectExecution(order *OrderRef, fills []FillRef) error {
	if order == nil {
		b.absent("execution_attempts", "no order, so no attempts")
		b.absent("journal_transactions", "no order, so no fills to post")
		return nil
	}
	attempts, err := loadAttempts(b.ctx, b.q, order.ID)
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		b.absent("execution_attempts", "order "+order.ID+" has no execution_attempts")
	}
	for i := range attempts {
		a := &attempts[i]
		b.resource(a.ID)
		b.correlation(&a.CorrelationID)
		dec, res, err := loadSigning(b.ctx, b.q, a.ID)
		if err != nil {
			return err
		}
		a.SigningDecision, a.SigningResult = dec, res
		if dec == nil {
			b.absent("execution_attempts["+a.ID+"].signing_decision", "attempt has no signing decision")
		} else {
			b.resource(dec.ID)
		}
		if res == nil {
			b.absent("execution_attempts["+a.ID+"].signing_result", "attempt has no signing result")
		}
	}
	b.out.Attempts = attempts
	seen := map[string]bool{}
	for _, f := range fills {
		if f.JournalTransactionID == nil {
			b.absent("journal_transactions[fill "+f.ID+"]", "fill has no journal_transaction_id (not yet posted)")
			continue
		}
		if seen[*f.JournalTransactionID] {
			continue
		}
		seen[*f.JournalTransactionID] = true
		j, err := loadJournal(b.ctx, b.q, *f.JournalTransactionID)
		if err != nil {
			return err
		}
		if j == nil {
			b.absent("journal_transactions[fill "+f.ID+"]", "journal_transactions row "+*f.JournalTransactionID+" not found")
			continue
		}
		b.out.Journal = append(b.out.Journal, *j)
		b.resource(j.ID)
	}
	return nil
}

const selectBundleEventsSQL = `
SELECT id, stream, stream_seq, actor_type, actor_id, action, resource_type, resource_id, correlation_id,
       occurred_at, content_hash, prev_hash, build_version
FROM audit_events
WHERE resource_id = ANY($1) OR correlation_id = ANY($2)
ORDER BY stream, stream_seq`

func (b *bundleBuild) collectAuditEvents() error {
	b.resourceIDs = dedupe(b.resourceIDs)
	b.corrIDs = dedupe(b.corrIDs)
	if len(b.resourceIDs) == 0 && len(b.corrIDs) == 0 {
		return nil
	}
	rows, err := b.q.Query(b.ctx, selectBundleEventsSQL, b.resourceIDs, b.corrIDs)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "proof: read bundle audit events")
	}
	var events []AuditEventRef
	for rows.Next() {
		var e AuditEventRef
		if err := rows.Scan(&e.ID, &e.Stream, &e.StreamSeq, &e.ActorType, &e.ActorID, &e.Action, &e.ResourceType, &e.ResourceID,
			&e.CorrelationID, &e.OccurredAt, &e.ContentHash, &e.PrevHash, &e.BuildVersion); err != nil {
			rows.Close()
			return errs.Wrap(err, errs.CodeInternal, "proof: scan bundle audit event")
		}
		e.OccurredAt = e.OccurredAt.UTC()
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "proof: iterate bundle audit events")
	}
	if len(events) == 0 {
		b.absent("audit_events", "no audit event names any collected resource or correlation id")
	}
	for i := range events {
		m, err := b.membership(events[i].Stream, events[i].StreamSeq, events[i].ContentHash)
		if err != nil {
			return err
		}
		events[i].Checkpoint = m
	}
	b.out.AuditEvents = events
	return nil
}

// membership returns the proof of (stream, seq) in the checkpoint covering
// it, or nil when the event is not yet checkpointed.
func (b *bundleBuild) membership(stream string, seq int64, contentHash []byte) (*Membership, error) {
	var cpID CheckpointID
	err := b.q.QueryRow(b.ctx,
		`SELECT checkpoint_id FROM audit_checkpoint_streams WHERE stream = $1 AND from_seq <= $2 AND to_seq >= $2 LIMIT 1`,
		stream, seq).Scan(&cpID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: find covering checkpoint")
	}
	bt, ok := b.trees[cpID.String()]
	if !ok {
		cps, err := listCheckpointByID(b.ctx, b.q, cpID)
		if err != nil {
			return nil, err
		}
		leaves, offsets, err := loadLeaves(b.ctx, b.q, cps.StreamsCovered)
		if err != nil {
			return nil, err
		}
		bt = bundleTree{tree: NewTree(leaves), offsets: offsets, cp: cps}
		b.trees[cpID.String()] = bt
	}
	m := &Membership{CheckpointID: bt.cp.ID.String(), CheckpointSeq: bt.cp.Seq, MerkleRoot: bt.cp.MerkleRoot}
	r, covered := bt.cp.StreamsCovered[stream]
	off, hasOff := bt.offsets[stream]
	if !covered || !hasOff || seq < r.FromSeq || seq > r.ToSeq {
		m.Error = "coverage index names a checkpoint whose signed streams_covered does not include this event"
		return m, nil
	}
	index := off + int(seq-r.FromSeq)
	p, err := bt.tree.Prove(index)
	if err != nil {
		m.Error = err.Error()
		return m, nil
	}
	m.Proof = p
	if err := VerifyMembership(bt.cp.MerkleRoot, contentHash, p); err != nil {
		m.Error = "proof does not verify against the checkpoint root: " + err.Error()
		return m, nil
	}
	m.Verified = true
	return m, nil
}

// loadLeaves returns every covered content hash in leaf order and the leaf
// offset of each stream's range. It fails when a covered event is missing,
// since no honest proof can be built without it.
func loadLeaves(ctx context.Context, q db.Querier, covered map[string]StreamRange) ([][]byte, map[string]int, error) {
	doc := Document{StreamsCovered: covered}
	var leaves [][]byte
	offsets := map[string]int{}
	for _, s := range doc.Streams() {
		r := covered[s]
		hashes, broken, err := loadRange(ctx, q, s, r.FromSeq, r.ToSeq)
		if err != nil {
			return nil, nil, err
		}
		if broken != nil || int64(len(hashes)) != r.ToSeq-r.FromSeq+1 {
			return nil, nil, errs.Newf(errs.CodeConflict, "proof: covered range of stream %s is incomplete in audit_events; run verify", s).WithField("stream", s)
		}
		offsets[s] = len(leaves)
		leaves = append(leaves, hashes...)
	}
	return leaves, offsets, nil
}

func listCheckpointByID(ctx context.Context, q db.Querier, cpID CheckpointID) (Checkpoint, error) {
	var seq int64
	if err := q.QueryRow(ctx, `SELECT seq FROM audit_checkpoints WHERE id = $1`, cpID).Scan(&seq); err != nil {
		return Checkpoint{}, errs.Wrap(err, errs.CodeInternal, "proof: read checkpoint")
	}
	cps, err := listCheckpoints(ctx, q, seq-1, 1)
	if err != nil {
		return Checkpoint{}, err
	}
	if len(cps) != 1 || cps[0].ID != cpID {
		return Checkpoint{}, errs.Newf(errs.CodeInternal, "proof: checkpoint %s not found by seq", cpID)
	}
	return cps[0], nil
}

// ---- row loaders -----------------------------------------------------------

// scanOne runs a single-row query; found is false on pgx.ErrNoRows.
func scanOne(ctx context.Context, q db.Querier, what, sql string, args []any, dest ...any) (bool, error) {
	err := q.QueryRow(ctx, sql, args...).Scan(dest...)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, errs.Wrap(err, errs.CodeInternal, "proof: read "+what)
	}
	return true, nil
}

func loadFill(ctx context.Context, q db.Querier, fillID string) (FillRef, bool, error) {
	var f FillRef
	found, err := scanOne(ctx, q, "fill", `
SELECT id, order_id, attempt_id, venue, external_fill_id, tx_signature, slot, source, finality, observed_at, raw_ref, journal_transaction_id
FROM fills WHERE id = $1`, []any{fillID},
		&f.ID, &f.OrderID, &f.AttemptID, &f.Venue, &f.ExternalFillID, &f.TxSignature, &f.Slot, &f.Source, &f.Finality, &f.ObservedAt, &f.RawRef, &f.JournalTransactionID)
	f.ObservedAt = f.ObservedAt.UTC()
	return f, found, err
}

func loadFills(ctx context.Context, q db.Querier, orderID string) ([]FillRef, error) {
	rows, err := q.Query(ctx, `
SELECT id, order_id, attempt_id, venue, external_fill_id, tx_signature, slot, source, finality, observed_at, raw_ref, journal_transaction_id
FROM fills WHERE order_id = $1 ORDER BY observed_at, id`, orderID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: read fills")
	}
	defer rows.Close()
	var out []FillRef
	for rows.Next() {
		var f FillRef
		if err := rows.Scan(&f.ID, &f.OrderID, &f.AttemptID, &f.Venue, &f.ExternalFillID, &f.TxSignature, &f.Slot, &f.Source, &f.Finality,
			&f.ObservedAt, &f.RawRef, &f.JournalTransactionID); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "proof: scan fill")
		}
		f.ObservedAt = f.ObservedAt.UTC()
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: iterate fills")
	}
	return out, nil
}

func (b *bundleBuild) loadOrder(where, arg, link string) (*OrderRef, error) {
	var o OrderRef
	found, err := scanOne(b.ctx, b.q, "order",
		`SELECT id, intent_id, plan_id, quote_id, status, correlation_id, created_at FROM orders `+where, []any{arg},
		&o.ID, &o.IntentID, &o.PlanID, &o.QuoteID, &o.Status, &o.CorrelationID, &o.CreatedAt)
	if err != nil {
		return nil, err
	}
	if !found {
		b.absent(link, "no orders row for "+arg)
		return nil, nil
	}
	o.CreatedAt = o.CreatedAt.UTC()
	return &o, nil
}

func (b *bundleBuild) loadIntent(intentID string) (*IntentRef, error) {
	var in IntentRef
	found, err := scanOne(b.ctx, b.q, "intent", `
SELECT id, account_id, actor_type, actor_id, agent_id, strategy_version_id, prediction_id, action, mode, status, correlation_id,
       content_hash, eligibility_decision_id, risk_decision_id, plan_id, order_id, requested_at
FROM trade_intents WHERE id = $1`, []any{intentID},
		&in.ID, &in.AccountID, &in.ActorType, &in.ActorID, &in.AgentID, &in.StrategyVersionID, &in.PredictionID, &in.Action, &in.Mode, &in.Status, &in.CorrelationID,
		&in.ContentHash, &in.EligibilityDecisionID, &in.RiskDecisionID, &in.PlanID, &in.OrderID, &in.RequestedAt)
	if err != nil {
		return nil, err
	}
	if !found {
		b.absent("intent", "trade_intents row "+intentID+" not found")
		return nil, nil
	}
	in.RequestedAt = in.RequestedAt.UTC()
	return &in, nil
}

func loadPlan(ctx context.Context, q db.Querier, planID string) (*PlanRef, error) {
	var p PlanRef
	var policy []byte
	found, err := scanOne(ctx, q, "plan", `
SELECT id, version, planner_version, status, plan_hash, risk_decision_id, quote_id, policy_versions, approved_at
FROM execution_plans WHERE id = $1`, []any{planID},
		&p.ID, &p.Version, &p.PlannerVersion, &p.Status, &p.PlanHash, &p.RiskDecisionID, &p.QuoteID, &policy, &p.ApprovedAt)
	if err != nil || !found {
		return nil, err
	}
	p.PolicyVersions = json.RawMessage(policy)
	if p.ApprovedAt != nil {
		t := p.ApprovedAt.UTC()
		p.ApprovedAt = &t
	}
	return &p, nil
}

func loadQuote(ctx context.Context, q db.Querier, quoteID string) (*QuoteRef, error) {
	var qt QuoteRef
	found, err := scanOne(ctx, q, "quote", `
SELECT id, provider, provider_request_id, route_hash, raw_response_hash, raw_response_ref, received_at, expires_at
FROM quotes WHERE id = $1`, []any{quoteID},
		&qt.ID, &qt.Provider, &qt.ProviderRequestID, &qt.RouteHash, &qt.RawResponseHash, &qt.RawResponseRef, &qt.ReceivedAt, &qt.ExpiresAt)
	if err != nil || !found {
		return nil, err
	}
	qt.ReceivedAt, qt.ExpiresAt = qt.ReceivedAt.UTC(), qt.ExpiresAt.UTC()
	return &qt, nil
}

// nonNil flattens optional ids into a (possibly empty) text array.
func nonNil(ids []*string) []string {
	out := []string{}
	for _, p := range ids {
		if p != nil && *p != "" {
			out = append(out, *p)
		}
	}
	return out
}

func loadRisk(ctx context.Context, q db.Querier, intentID string, extra []*string) ([]RiskRef, error) {
	rows, err := q.Query(ctx, `
SELECT id, stage, policy_version, policy_hash, decision, reason_codes, evaluator_version, quote_id, evaluated_at
FROM risk_decisions WHERE intent_id = $1 OR id = ANY($2::uuid[]) ORDER BY evaluated_at, id`, intentID, nonNil(extra))
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: read risk decisions")
	}
	defer rows.Close()
	var out []RiskRef
	for rows.Next() {
		var r RiskRef
		if err := rows.Scan(&r.ID, &r.Stage, &r.PolicyVersion, &r.PolicyHash, &r.Decision, &r.ReasonCodes, &r.EvaluatorVersion, &r.QuoteID, &r.EvaluatedAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "proof: scan risk decision")
		}
		r.EvaluatedAt = r.EvaluatedAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: iterate risk decisions")
	}
	return out, nil
}

func loadEligibility(ctx context.Context, q db.Querier, intentID string, extra []*string) ([]EligibilityRef, error) {
	rows, err := q.Query(ctx, `
SELECT id, eligible, policy_version, context_hash, reason_codes, evaluated_at
FROM eligibility_decisions WHERE intent_id = $1 OR id = ANY($2::uuid[]) ORDER BY evaluated_at, id`, intentID, nonNil(extra))
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: read eligibility decisions")
	}
	defer rows.Close()
	var out []EligibilityRef
	for rows.Next() {
		var e EligibilityRef
		if err := rows.Scan(&e.ID, &e.Eligible, &e.PolicyVersion, &e.ContextHash, &e.ReasonCodes, &e.EvaluatedAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "proof: scan eligibility decision")
		}
		e.EvaluatedAt = e.EvaluatedAt.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: iterate eligibility decisions")
	}
	return out, nil
}

func loadPrediction(ctx context.Context, q db.Querier, predictionID string) (*PredictionRef, error) {
	var p PredictionRef
	found, err := scanOne(ctx, q, "prediction", `
SELECT id, agent_id, agent_version, strategy_version_id, mode, information_set_hash, model_call_id, template_version, committed_at
FROM predictions WHERE id = $1`, []any{predictionID},
		&p.ID, &p.AgentID, &p.AgentVersion, &p.StrategyVersionID, &p.Mode, &p.InformationSetHash, &p.ModelCallID, &p.TemplateVersion, &p.CommittedAt)
	if err != nil || !found {
		return nil, err
	}
	p.CommittedAt = p.CommittedAt.UTC()
	return &p, nil
}

func loadModelCall(ctx context.Context, q db.Querier, callID string) (*ModelRef, error) {
	var m ModelRef
	found, err := scanOne(ctx, q, "model call", `
SELECT id, provider, model_id, prompt_template_version, input_hash, output_hash, prompt_ref, response_ref, request_at
FROM model_calls WHERE id = $1`, []any{callID},
		&m.ID, &m.Provider, &m.ModelID, &m.PromptTemplateVersion, &m.InputHash, &m.OutputHash, &m.PromptRef, &m.ResponseRef, &m.RequestAt)
	if err != nil || !found {
		return nil, err
	}
	m.RequestAt = m.RequestAt.UTC()
	return &m, nil
}

func loadStrategyVersion(ctx context.Context, q db.Querier, versionID string) (*StrategyVersionRef, error) {
	var s StrategyVersionRef
	found, err := scanOne(ctx, q, "strategy version", `
SELECT id, strategy_id, version, status, ir_hash, source_hash, compiler_version, risk_policy_version, risk_policy_hash
FROM strategy_versions WHERE id = $1`, []any{versionID},
		&s.ID, &s.StrategyID, &s.Version, &s.Status, &s.IRHash, &s.SourceHash, &s.CompilerVersion, &s.RiskPolicyVersion, &s.RiskPolicyHash)
	if err != nil || !found {
		return nil, err
	}
	return &s, nil
}

func loadAttempts(ctx context.Context, q db.Querier, orderID string) ([]AttemptRef, error) {
	rows, err := q.Query(ctx, `
SELECT id, attempt_no, status, unsigned_tx_hash, signed_tx_hash, tx_signature, correlation_id, submitted_at, finalized_at
FROM execution_attempts WHERE order_id = $1 ORDER BY attempt_no`, orderID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: read execution attempts")
	}
	defer rows.Close()
	var out []AttemptRef
	for rows.Next() {
		var a AttemptRef
		if err := rows.Scan(&a.ID, &a.AttemptNo, &a.Status, &a.UnsignedTxHash, &a.SignedTxHash, &a.TxSignature, &a.CorrelationID, &a.SubmittedAt, &a.FinalizedAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "proof: scan execution attempt")
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: iterate execution attempts")
	}
	return out, nil
}

func loadSigning(ctx context.Context, q db.Querier, attemptID string) (*SigningDecisionRef, *SigningResultRef, error) {
	var d SigningDecisionRef
	foundD, err := scanOne(ctx, q, "signing decision", `
SELECT id, decision, expected_tx_hash, inspected_tx_hash, inspector_version, decided_at
FROM signing_decisions WHERE attempt_id = $1 ORDER BY decided_at DESC LIMIT 1`, []any{attemptID},
		&d.ID, &d.Decision, &d.ExpectedTxHash, &d.InspectedTxHash, &d.InspectorVersion, &d.DecidedAt)
	if err != nil {
		return nil, nil, err
	}
	var r SigningResultRef
	foundR, err := scanOne(ctx, q, "signing result", `
SELECT id, signed_tx_hash, signature, provider_sign_ref, signed_at FROM signing_results WHERE attempt_id = $1`, []any{attemptID},
		&r.ID, &r.SignedTxHash, &r.Signature, &r.ProviderSignRef, &r.SignedAt)
	if err != nil {
		return nil, nil, err
	}
	var (
		dp *SigningDecisionRef
		rp *SigningResultRef
	)
	if foundD {
		d.DecidedAt = d.DecidedAt.UTC()
		dp = &d
	}
	if foundR {
		r.SignedAt = r.SignedAt.UTC()
		rp = &r
	}
	return dp, rp, nil
}

func loadJournal(ctx context.Context, q db.Querier, txID string) (*JournalRef, error) {
	var j JournalRef
	found, err := scanOne(ctx, q, "journal transaction", `
SELECT id, kind, idempotency_key, reference_type, reference_id, content_hash, posted_at, build_version
FROM journal_transactions WHERE id = $1`, []any{txID},
		&j.ID, &j.Kind, &j.IdempotencyKey, &j.ReferenceType, &j.ReferenceID, &j.ContentHash, &j.PostedAt, &j.BuildVersion)
	if err != nil || !found {
		return nil, err
	}
	j.PostedAt = j.PostedAt.UTC()
	return &j, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
