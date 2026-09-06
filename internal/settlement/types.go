package settlement

import (
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider"
)

// StepType is a typed plan step (execution_plan_steps.type).
type StepType string

// V1 spot-swap step types in canonical order (PART 38).
const (
	StepValidateEligibility   StepType = "VALIDATE_ELIGIBILITY"
	StepEvaluateRisk          StepType = "EVALUATE_RISK"
	StepReserveCapital        StepType = "RESERVE_CAPITAL"
	StepResolveVenueListing   StepType = "RESOLVE_VENUE_LISTING"
	StepLocateSettlementAsset StepType = "LOCATE_SETTLEMENT_ASSET"
	StepAcquireQuote          StepType = "ACQUIRE_QUOTE"
	StepValidateQuote         StepType = "VALIDATE_QUOTE"
	StepFinalRiskCheck        StepType = "FINAL_RISK_CHECK"
	StepBuildTransaction      StepType = "BUILD_TRANSACTION"
	StepInspectTransaction    StepType = "INSPECT_TRANSACTION"
	StepRequestSignature      StepType = "REQUEST_SIGNATURE"
	StepSubmit                StepType = "SUBMIT"
	StepObserveFinality       StepType = "OBSERVE_FINALITY"
	StepReconcile             StepType = "RECONCILE"
	StepPostLedger            StepType = "POST_LEDGER"
	StepUpdatePosition        StepType = "UPDATE_POSITION"
	StepReleaseReservation    StepType = "RELEASE_RESERVATION"
)

// Reserved step types (PART 230): representable, never emitted in V1. The
// CROSS_CHAIN capability stays disabled; there is no fake instant bridging.
const (
	StepConvert      StepType = "CONVERT"
	StepTransfer     StepType = "TRANSFER"
	StepWaitFinality StepType = "WAIT_FINALITY"
	StepTrade        StepType = "TRADE"
)

// V1StepSequence returns the canonical V1 step order.
func V1StepSequence() []StepType {
	return []StepType{
		StepValidateEligibility, StepEvaluateRisk, StepReserveCapital, StepResolveVenueListing, StepLocateSettlementAsset,
		StepAcquireQuote, StepValidateQuote, StepFinalRiskCheck, StepBuildTransaction, StepInspectTransaction,
		StepRequestSignature, StepSubmit, StepObserveFinality, StepReconcile, StepPostLedger, StepUpdatePosition, StepReleaseReservation,
	}
}

// ReservedStepTypes returns the step types the V1 planner never emits.
func ReservedStepTypes() []StepType {
	return []StepType{StepConvert, StepTransfer, StepWaitFinality, StepTrade}
}

// Reserved reports whether t is a reserved cross-chain step type.
func (t StepType) Reserved() bool {
	switch t {
	case StepConvert, StepTransfer, StepWaitFinality, StepTrade:
		return true
	}
	return false
}

// Valid reports whether t is declared (V1 or reserved).
func (t StepType) Valid() bool {
	if t.Reserved() {
		return true
	}
	for _, s := range V1StepSequence() {
		if s == t {
			return true
		}
	}
	return false
}

// PostSubmission reports whether the step runs after the transaction may
// exist externally. Post-submission steps are never blocked by kill
// switches, agent pauses or provider breakers (PART 52, PART 107).
func (t StepType) PostSubmission() bool {
	switch t {
	case StepObserveFinality, StepReconcile, StepPostLedger, StepUpdatePosition, StepReleaseReservation:
		return true
	}
	return false
}

// DryRunStop reports whether a dry-run plan stops after this step
// (PART 220): everything up to and including INSPECT_TRANSACTION runs.
func (t StepType) DryRunStop() bool { return t == StepInspectTransaction }

// KillSwitchClass returns the kill-switch action class the executor checks
// before running the step, given the plan's own class (NEW_RISK or
// REDUCE_RISK). RESERVE_CAPITAL and SUBMIT commit new risk and are checked
// with the plan class; post-submission steps carry the never-blocked
// classes; every other step is a local check or a read and is not guarded.
func (t StepType) KillSwitchClass(planClass killswitch.ActionClass) (killswitch.ActionClass, bool) {
	switch t {
	case StepReserveCapital, StepSubmit:
		return planClass, true
	case StepObserveFinality:
		return killswitch.Observe, true
	case StepReconcile:
		return killswitch.Reconcile, true
	case StepPostLedger:
		return killswitch.LedgerPost, true
	case StepUpdatePosition, StepReleaseReservation:
		return killswitch.Settle, true
	}
	return "", false
}

// StepState is execution_plan_steps.state.
type StepState string

// Step states.
const (
	StepPending     StepState = "PENDING"
	StepRunning     StepState = "RUNNING"
	StepSucceeded   StepState = "SUCCEEDED"
	StepFailed      StepState = "FAILED"
	StepSkipped     StepState = "SKIPPED"
	StepUnknown     StepState = "UNKNOWN"
	StepCompensated StepState = "COMPENSATED"
)

// Valid reports whether s is declared.
func (s StepState) Valid() bool {
	switch s {
	case StepPending, StepRunning, StepSucceeded, StepFailed, StepSkipped, StepUnknown, StepCompensated:
		return true
	}
	return false
}

// Done reports whether the step needs no further execution.
func (s StepState) Done() bool { return s == StepSucceeded || s == StepSkipped || s == StepCompensated }

// PlanStatus is execution_plans.status.
type PlanStatus string

// Plan statuses.
const (
	PlanDraft       PlanStatus = "DRAFT"
	PlanApproved    PlanStatus = "APPROVED"
	PlanExecuting   PlanStatus = "EXECUTING"
	PlanCompleted   PlanStatus = "COMPLETED"
	PlanFailed      PlanStatus = "FAILED"
	PlanSuperseded  PlanStatus = "SUPERSEDED"
	PlanNoValidPlan PlanStatus = "NO_VALID_PLAN"
)

// Valid reports whether s is declared.
func (s PlanStatus) Valid() bool {
	switch s {
	case PlanDraft, PlanApproved, PlanExecuting, PlanCompleted, PlanFailed, PlanSuperseded, PlanNoValidPlan:
		return true
	}
	return false
}

// Terminal reports whether the plan will not run (again).
func (s PlanStatus) Terminal() bool {
	switch s {
	case PlanCompleted, PlanFailed, PlanSuperseded, PlanNoValidPlan:
		return true
	}
	return false
}

// planTransitions is the plan lifecycle.
var planTransitions = map[PlanStatus][]PlanStatus{
	PlanDraft:       {PlanApproved, PlanSuperseded, PlanFailed},
	PlanApproved:    {PlanExecuting, PlanSuperseded, PlanFailed},
	PlanExecuting:   {PlanCompleted, PlanFailed, PlanSuperseded},
	PlanCompleted:   {},
	PlanFailed:      {},
	PlanSuperseded:  {},
	PlanNoValidPlan: {},
}

// CanTransitionPlan reports whether from → to is legal.
func CanTransitionPlan(from, to PlanStatus) bool {
	for _, s := range planTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// CompensationPolicy says what the executor does when a step fails.
type CompensationPolicy string

// Compensation policies.
const (
	CompensationNone               CompensationPolicy = "NONE"
	CompensationReleaseReservation CompensationPolicy = "RELEASE_RESERVATION"
	CompensationReconcileOnly      CompensationPolicy = "RECONCILE_ONLY"
)

// Valid reports whether c is declared.
func (c CompensationPolicy) Valid() bool {
	return c == CompensationNone || c == CompensationReleaseReservation || c == CompensationReconcileOnly
}

// FinalityNone is the finality_policy of steps that wait for nothing.
const FinalityNone = "NONE"

// Step mirrors one execution_plan_steps row (PART 39).
type Step struct {
	ID                     StepID
	PlanID                 PlanID
	Seq                    int32
	Type                   StepType
	DependsOn              []StepID
	State                  StepState
	SemanticIdempotencyKey string
	RetryClass             provider.RetryClass
	Timeout                time.Duration
	FinalityPolicy         string
	CompensationPolicy     CompensationPolicy
	EvidenceInputs         json.RawMessage
	EvidenceOutput         json.RawMessage
	Attempts               int32
	StartedAt              *time.Time
	FinishedAt             *time.Time
	LastError              string
}

// AssetRef is the plan's view of an asset: identity, precision and the
// venue-facing mint, so the executor and the inspector need no lookup.
type AssetRef struct {
	ID       assets.AssetID `json:"id"`
	Decimals uint8          `json:"decimals"`
	Mint     string         `json:"mint"`
	Symbol   string         `json:"symbol,omitempty"`
}

// HardConstraints are the bounds every later step must respect (§4). They
// are the strictest of the intent's constraints and the risk kernel's
// resulting constraints, plus the sized quantities. All numbers are strings
// in JSON.
type HardConstraints struct {
	Side        execution.Side         `json:"side"`
	ActionClass killswitch.ActionClass `json:"action_class"`
	InputAsset  AssetRef               `json:"input_asset"`
	OutputAsset AssetRef               `json:"output_asset"`
	// MaxInputQuantity is the reserved quantity: the most the transaction may
	// debit from the wallet in the input asset.
	MaxInputQuantity money.Quantity `json:"max_input_quantity"`
	// MinOutputQuantity is the floor for the venue's encoded minimum out.
	MinOutputQuantity      money.Quantity `json:"min_output_quantity"`
	MaxSlippageBPS         money.BPS      `json:"max_slippage_bps"`
	MaxFeeBPS              money.BPS      `json:"max_fee_bps"`
	MaxPriceImpactBPS      money.BPS      `json:"max_price_impact_bps"`
	MaxNetworkFee          money.Quantity `json:"max_network_fee"`
	NetworkFeeAsset        AssetRef       `json:"network_fee_asset"`
	MaxPriorityFeeLamports money.Quantity `json:"max_priority_fee_lamports"`
	MaxComputeUnits        uint32         `json:"max_compute_units"`
	QuoteMaxAgeMS          int64          `json:"quote_max_age_ms"`
	Deadline               time.Time      `json:"deadline"`
	NotionalUSD            money.USD      `json:"notional_usd"`
	MaxNotionalUSD         money.USD      `json:"max_notional_usd"`
	MinLiquidityUSD        money.USD      `json:"min_liquidity_usd"`
	// MaxPrice is the limit price per whole base unit in the quote asset:
	// the most paid on a buy, the least received on a sell. Nil when unset.
	MaxPrice *money.Price `json:"max_price,omitempty"`
	Venue    string       `json:"venue"`
	Provider string       `json:"provider"`
	Chain    string       `json:"chain"`
	// FinalityForLedger and FinalityForPosition are the levels the plan
	// waits for before posting and applying fills.
	FinalityForLedger   execution.FinalityLevel `json:"finality_for_ledger"`
	FinalityForPosition execution.FinalityLevel `json:"finality_for_position"`
	// AllowedProgramIDs are the program ids the plan adds to the signing
	// inspector's default allow-list (the selected venue's programs);
	// AllowedFeeAccounts are the accounts allowed to receive value besides
	// the wallet and its token accounts (venue fee accounts);
	// RouteProgramIDs are the program ids the quoted route touches, empty
	// until a quote exists. All three are sorted and never nil, so the
	// inspector fails closed on anything not listed (EXECUTION.md §2).
	AllowedProgramIDs  []string `json:"allowed_program_ids"`
	AllowedFeeAccounts []string `json:"allowed_fee_accounts"`
	RouteProgramIDs    []string `json:"route_program_ids"`
}

// QuoteMaxAge returns QuoteMaxAgeMS as a duration.
func (h HardConstraints) QuoteMaxAge() time.Duration {
	return time.Duration(h.QuoteMaxAgeMS) * time.Millisecond
}

// CostEstimate is one disclosed cost line.
type CostEstimate struct {
	Quantity money.Quantity `json:"quantity"`
	Asset    assets.AssetID `json:"asset"`
	BPS      money.BPS      `json:"bps"`
	USD      money.USD      `json:"usd"`
}

// FeePolicyRef is the platform fee policy the plan was priced under; the
// executor reconstructs fees.Policy from it to compute the fee per fill.
type FeePolicyRef struct {
	Version  string         `json:"version"`
	Hash     string         `json:"hash"`
	BPS      money.BPS      `json:"bps"`
	MinFee   money.Quantity `json:"min_fee"`
	MaxFee   money.Quantity `json:"max_fee"`
	FeeAsset assets.AssetID `json:"fee_asset"`
	Rounding string         `json:"rounding"`
}

// EstimatedCosts shows venue fee, network estimate and platform fee
// separately (PART 126): no hidden spread.
type EstimatedCosts struct {
	NotionalUSD    money.USD      `json:"notional_usd"`
	PlatformFee    CostEstimate   `json:"platform_fee"`
	FeePolicy      FeePolicyRef   `json:"fee_policy"`
	VenueFee       CostEstimate   `json:"venue_fee"`
	NetworkFee     CostEstimate   `json:"network_fee"`
	TotalUSD       money.USD      `json:"total_usd"`
	ExpectedOutput money.Quantity `json:"expected_output"`
	ReferencePrice *money.Price   `json:"reference_price,omitempty"`
}

// PolicyVersions records every policy the plan was produced under.
type PolicyVersions map[string]string

// Policy version keys.
const (
	PolicyKeyEligibility = "eligibility"
	PolicyKeyRisk        = "risk"
	PolicyKeyRiskHash    = "risk_hash"
	PolicyKeyFees        = "fees"
	PolicyKeyBuyingPower = "buying_power"
	PolicyKeyFinality    = "finality"
	PolicyKeyPlanner     = "planner"
	PolicyKeyAssetPrefix = "asset:"
)

// Plan mirrors one execution_plans row with its steps (PART 39).
type Plan struct {
	ID                        PlanID
	IntentID                  string
	Version                   int32
	PlannerVersion            string
	Status                    PlanStatus
	NoPlanReasonCodes         []string
	HardConstraints           HardConstraints
	EstimatedCosts            EstimatedCosts
	SelectedVenueListingID    string
	SelectedSettlementAssetID assets.AssetID
	InstrumentVersion         int32
	PolicyVersions            PolicyVersions
	RiskDecisionID            string
	QuoteID                   string
	Hash                      []byte
	DryRun                    bool
	CreatedAt                 time.Time
	ApprovedAt                *time.Time
	FinishedAt                *time.Time
	Steps                     []Step
	// AccountID is the intent's account (not a plan column; read through the
	// intent). It scopes the audit stream.
	AccountID string
}

// StepByType returns the step of the given type.
func (p Plan) StepByType(t StepType) (Step, bool) {
	for _, s := range p.Steps {
		if s.Type == t {
			return s, true
		}
	}
	return Step{}, false
}

// Validate checks the plan's structure: mandatory V1 steps in order, a
// well-formed acyclic DAG, no reserved step types, valid retry and
// compensation policies, positive timeouts, unique sequence numbers and
// semantic keys (ADR-0014 plan validator).
func (p Plan) Validate() error {
	if p.Status == PlanNoValidPlan {
		if len(p.NoPlanReasonCodes) == 0 {
			return errs.New(errs.CodeValidationFailed, "settlement: NO_VALID_PLAN needs reason codes")
		}
		return nil
	}
	if p.IntentID == "" {
		return errs.New(errs.CodeValidationFailed, "settlement: plan intent id is required")
	}
	if p.Version < 1 {
		return errs.New(errs.CodeValidationFailed, "settlement: plan version must be >= 1")
	}
	if p.PlannerVersion == "" {
		return errs.New(errs.CodeValidationFailed, "settlement: planner version is required")
	}
	if !p.HardConstraints.Side.Valid() {
		return errs.New(errs.CodeValidationFailed, "settlement: plan side is required")
	}
	if !p.HardConstraints.MaxInputQuantity.IsPositive() {
		return errs.New(errs.CodeValidationFailed, "settlement: max input quantity must be positive")
	}
	want := V1StepSequence()
	if len(p.Steps) != len(want) {
		return errs.Newf(errs.CodeValidationFailed, "settlement: plan has %d steps, V1 requires %d", len(p.Steps), len(want))
	}
	seen := map[int32]Step{}
	keys := map[string]bool{}
	for i, s := range p.Steps {
		if s.Type.Reserved() {
			return errs.Newf(errs.CodeValidationFailed, "settlement: reserved step type %s cannot be emitted", s.Type)
		}
		if s.Type != want[i] {
			return errs.Newf(errs.CodeValidationFailed, "settlement: step %d is %s, expected %s", i, s.Type, want[i])
		}
		if s.Seq != int32(i) {
			return errs.Newf(errs.CodeValidationFailed, "settlement: step %s has seq %d, expected %d", s.Type, s.Seq, i)
		}
		if _, dup := seen[s.Seq]; dup {
			return errs.Newf(errs.CodeValidationFailed, "settlement: duplicate seq %d", s.Seq)
		}
		if s.SemanticIdempotencyKey == "" || keys[s.SemanticIdempotencyKey] {
			return errs.Newf(errs.CodeValidationFailed, "settlement: step %s needs a unique semantic idempotency key", s.Type)
		}
		keys[s.SemanticIdempotencyKey] = true
		switch s.RetryClass {
		case provider.SafeRetry, provider.IdempotentWrite, provider.UnknownEffectWrite:
		default:
			return errs.Newf(errs.CodeValidationFailed, "settlement: step %s has unknown retry class %q", s.Type, s.RetryClass)
		}
		if s.Type == StepSubmit && s.RetryClass != provider.UnknownEffectWrite {
			return errs.New(errs.CodeValidationFailed, "settlement: SUBMIT must be UNKNOWN_EFFECT_WRITE")
		}
		if s.Timeout <= 0 {
			return errs.Newf(errs.CodeValidationFailed, "settlement: step %s needs a positive timeout", s.Type)
		}
		if !s.CompensationPolicy.Valid() {
			return errs.Newf(errs.CodeValidationFailed, "settlement: step %s has unknown compensation policy %q", s.Type, s.CompensationPolicy)
		}
		if s.FinalityPolicy == "" {
			return errs.Newf(errs.CodeValidationFailed, "settlement: step %s needs a finality policy", s.Type)
		}
		if !s.State.Valid() {
			return errs.Newf(errs.CodeValidationFailed, "settlement: step %s has unknown state %q", s.Type, s.State)
		}
		// Dependencies may only point at earlier steps: that makes the
		// graph acyclic by construction.
		for _, dep := range s.DependsOn {
			found := false
			for _, prev := range p.Steps[:i] {
				if prev.ID == dep {
					found = true
					break
				}
			}
			if !found {
				return errs.Newf(errs.CodeValidationFailed, "settlement: step %s depends on an unknown or later step", s.Type)
			}
		}
		if i > 0 && len(s.DependsOn) == 0 {
			return errs.Newf(errs.CodeValidationFailed, "settlement: step %s must depend on its predecessor", s.Type)
		}
		seen[s.Seq] = s
	}
	return nil
}
