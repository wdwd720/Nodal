package settlement

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// Transactor runs a function inside a database transaction. *db.DB
// satisfies it; tests use an in-memory fake that passes a nil transaction.
type Transactor interface {
	InTx(ctx context.Context, opts db.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error
}

// PlanStore is the plan persistence the executor needs. *PlanRepository
// satisfies it.
type PlanStore interface {
	Get(ctx context.Context, q db.Querier, planID PlanID) (Plan, error)
	SetStatus(ctx context.Context, tx pgx.Tx, planID PlanID, from, to PlanStatus, reason string) (Plan, error)
	MarkStep(ctx context.Context, tx pgx.Tx, stepID StepID, from, to StepState, out StepOutcome) (Step, error)
	ResetSteps(ctx context.Context, tx pgx.Tx, planID PlanID, fromSeq int32) error
}

// OrderStore is the order and fill persistence the executor needs.
// *execution.Repository satisfies it.
type OrderStore interface {
	Create(ctx context.Context, tx pgx.Tx, o execution.Order) (execution.Order, error)
	Get(ctx context.Context, q db.Querier, orderID execution.OrderID) (execution.Order, error)
	GetByPlan(ctx context.Context, q db.Querier, planID string) (execution.Order, error)
	Transition(ctx context.Context, tx pgx.Tx, orderID execution.OrderID, to execution.OrderStatus, ev execution.TransitionEvidence) (execution.Order, error)
	RecordFill(ctx context.Context, tx pgx.Tx, f execution.Fill) (execution.Fill, error)
	ListFills(ctx context.Context, q db.Querier, orderID execution.OrderID) ([]execution.Fill, error)
	MarkFillPosted(ctx context.Context, tx pgx.Tx, fillID execution.FillID, journalTxID string) error
	MarkPositionApplied(ctx context.Context, tx pgx.Tx, fillID execution.FillID, at time.Time) error
}

// AttemptStore is the attempt persistence the executor needs.
// *execution.AttemptRepository satisfies it.
type AttemptStore interface {
	Create(ctx context.Context, tx pgx.Tx, a execution.Attempt) (execution.Attempt, error)
	Get(ctx context.Context, q db.Querier, attemptID execution.AttemptID) (execution.Attempt, error)
	Update(ctx context.Context, tx pgx.Tx, attemptID execution.AttemptID, p execution.AttemptPatch) (execution.Attempt, error)
	SetSignature(ctx context.Context, tx pgx.Tx, attemptID execution.AttemptID, txSignature string, signedTxHash []byte) (execution.Attempt, error)
	ListForOrder(ctx context.Context, q db.Querier, orderID execution.OrderID) ([]execution.Attempt, error)
}

// QuoteStore persists the quote snapshot ACQUIRE_QUOTE obtains and returns
// its id (the quotes row the order and attempt reference). The integrator
// wires internal/quote's repository; tests use a fake.
type QuoteStore interface {
	Save(ctx context.Context, tx pgx.Tx, q execution.QuoteSnapshot) (string, error)
}

// IntentRef is what the executor needs to know about the intent behind a
// plan: identity, actor, mode, wallet and any reservation made before
// planning.
type IntentRef struct {
	ID                string
	AccountID         string
	ActorType         security.ActorType
	ActorID           string
	AgentID           string
	StrategyVersionID string
	InstrumentID      string
	Mode              execution.Mode
	CorrelationID     string
	ReservationID     string
	EnvelopeID        string
	WalletID          string
	WalletAddress     string
	WalletChain       string
	WalletProvider    string
}

// IntentReader loads the IntentRef of a plan's intent.
type IntentReader interface {
	Intent(ctx context.Context, q db.Querier, intentID string) (IntentRef, error)
}

// CapitalService is the reservation contract the executor uses:
// capital.Reserver plus the reads and the final consumption.
// *capital.Service satisfies it.
type CapitalService interface {
	capital.Reserver
	Get(ctx context.Context, q db.Querier, rid capital.ReservationID) (capital.Reservation, error)
	ConsumeFinal(ctx context.Context, tx pgx.Tx, rid capital.ReservationID, qty money.Quantity, usdMinor int64, orderID string) (capital.Reservation, error)
}

// KillSwitchChecker is the authoritative kill-switch check.
// *killswitch.Checker satisfies it.
type KillSwitchChecker interface {
	Check(ctx context.Context, q db.Querier, a killswitch.Action) error
	Snapshot(ctx context.Context, q db.Querier) (killswitch.Snapshot, error)
}

// FinalRiskRequest is what FINAL_RISK_CHECK asks the risk service to
// evaluate against the fresh quote.
type FinalRiskRequest struct {
	PlanID        PlanID
	PlanHash      []byte
	IntentID      string
	AccountID     string
	AgentID       string
	Quote         execution.QuoteSnapshot
	Constraints   HardConstraints
	KillSwitches  killswitch.Snapshot
	CorrelationID string
	Now           time.Time
}

// FinalRiskChecker evaluates the FINAL stage (risk.Evaluate with the
// effective policy and a fresh account snapshot) and records the decision
// in tx. It returns the decision and the persisted decision id. The
// integrator wires internal/risk; tests use a fake.
type FinalRiskChecker interface {
	CheckFinal(ctx context.Context, tx pgx.Tx, req FinalRiskRequest) (risk.Decision, string, error)
}

// InspectExpectations are the expectations INSPECT_TRANSACTION and the
// signer enforce (EXECUTION.md §2), derived from the approved plan.
type InspectExpectations struct {
	InspectStepInput
	PlanHash             []byte
	QuoteID              string
	RecentBlockhash      string
	LastValidBlockHeight uint64
	InputAsset           assets.AssetID
	OutputAsset          assets.AssetID
}

// InspectRequest is the input of TransactionInspector.Inspect.
type InspectRequest struct {
	PlanID       PlanID
	AttemptID    execution.AttemptID
	Action       execution.UnsignedAction
	Expectations InspectExpectations
}

// InspectResult is the inspector's verdict. Any failed check yields
// Approved=false with sorted reason codes.
type InspectResult struct {
	Approved         bool
	ReasonCodes      []string
	Checks           json.RawMessage
	InspectorVersion string
	SimulationOK     *bool
	SimulationRef    string
}

// TransactionInspector decodes and inspects an unsigned transaction against
// the plan's expectations. It is pure with respect to money: it never
// signs and never submits. The integrator wires signing/inspect.
type TransactionInspector interface {
	Inspect(ctx context.Context, req InspectRequest) (InspectResult, error)
}

// SignRequest is what REQUEST_SIGNATURE sends to the signing service. The
// service independently re-loads the plan, quote, risk decision and
// reservation and rebuilds its own expectations from persisted data.
type SignRequest struct {
	AttemptID      execution.AttemptID
	PlanID         PlanID
	IntentID       string
	RiskDecisionID string
	WalletID       string
	UnsignedTx     []byte
	ExpectedTxHash []byte
	PlanHash       []byte
	QuoteID        string
	Expectations   InspectExpectations
	IdempotencyKey string
}

// SignResult is the signing service's answer. When Approved is false the
// reason codes say why and SignedTx is empty.
type SignResult struct {
	DecisionID  string
	Approved    bool
	ReasonCodes []string
	SignedTx    []byte
	TxSignature string
	RawRef      string
}

// Signer is the bounded signing boundary (EXECUTION.md §3). Only the live
// Executor holds one; the dry-run executor is constructed without.
type Signer interface {
	Sign(ctx context.Context, req SignRequest) (SignResult, error)
}

// TxObservation is one chain observer's view of a signature.
type TxObservation struct {
	Signature  string
	Found      bool
	Slot       uint64
	Finality   execution.FinalityLevel
	Failed     bool
	Error      string
	ObservedAt time.Time
	RawRef     string
}

// BalanceObservation is one token balance of a wallet.
type BalanceObservation struct {
	Mint       string
	Quantity   money.Quantity
	ObservedAt time.Time
}

// ChainObserver reads chain state independently of the execution provider
// (EXECUTION.md §6). Observation is never blocked by kill switches or
// provider breakers.
type ChainObserver interface {
	Name() string
	GetTransaction(ctx context.Context, signature string) (TxObservation, error)
	GetBalances(ctx context.Context, wallet string, mints []string) ([]BalanceObservation, error)
	GetBlockHeight(ctx context.Context) (uint64, error)
}

// RecoveryOutcome is the result of the unknown-submission recovery path.
type RecoveryOutcome string

// Recovery outcomes (EXECUTION.md §4 steps 6-8).
const (
	// RecoveryAdopted: the transaction was found; the attempt is adopted
	// and the plan continues with finality observation.
	RecoveryAdopted RecoveryOutcome = "ADOPTED"
	// RecoveryProvenAbsent: both observers agree the signature is absent and
	// the chain height passed LastValidBlockHeight + margin; the transaction
	// can never land.
	RecoveryProvenAbsent RecoveryOutcome = "PROVEN_ABSENT"
	// RecoveryUnresolved: observers disagree or evidence is incomplete; the
	// order needs the operator / reconciliation path.
	RecoveryUnresolved RecoveryOutcome = "UNRESOLVED"
)

// RecoveryRequest describes the unknown submission to investigate.
type RecoveryRequest struct {
	PlanID               PlanID
	PlanHash             []byte
	OrderID              execution.OrderID
	AttemptID            execution.AttemptID
	TxSignature          string
	WalletAddress        string
	Provider             string
	Chain                string
	LastValidBlockHeight uint64
	Since                time.Time
}

// RecoveryResult is the Recoverer's verdict with its evidence.
type RecoveryResult struct {
	Outcome     RecoveryOutcome
	ExternalRef execution.ExternalReference
	Status      execution.ExecutionStatus
	EvidenceRef string
	Reason      string
}

// Recoverer implements the PART 48 investigation (provider status, chain
// observers, wallet activity scan) for a submission whose outcome is
// unknown. The integrator wires internal/reconciliation; tests use a fake.
type Recoverer interface {
	Recover(ctx context.Context, req RecoveryRequest) (RecoveryResult, error)
}

// Sleeper waits for d or until ctx is done. Tests inject a no-op.
type Sleeper func(ctx context.Context, d time.Duration) error

// Phase is a point in a step's life at which Hooks.OnStep is called.
type Phase string

// Step phases. BeforeEffect runs after RUNNING is persisted and before any
// side effect; AfterEffect runs after the side effect and before the
// terminal state is persisted; AfterPersist runs after it is persisted.
const (
	PhaseBeforeEffect Phase = "BEFORE_EFFECT"
	PhaseAfterEffect  Phase = "AFTER_EFFECT"
	PhaseAfterPersist Phase = "AFTER_PERSIST"
)

// Hooks are optional observation points. An error returned from OnStep
// aborts Run immediately, persisting nothing further: fault-injection tests
// use it to simulate a crash at an exact step boundary.
type Hooks struct {
	OnStep func(ctx context.Context, step Step, phase Phase) error
}

// StepOutputs are the typed evidence outputs the executor records per step
// and restores from on resume.
type (
	// ReserveOutput records the reservation RESERVE_CAPITAL holds.
	ReserveOutput struct {
		ReservationID string         `json:"reservation_id"`
		Quantity      money.Quantity `json:"quantity"`
		Existing      bool           `json:"existing"`
		DryRun        bool           `json:"dry_run,omitempty"`
	}
	// LocateOutput records the observed wallet balance.
	LocateOutput struct {
		WalletAddress string         `json:"wallet_address"`
		Mint          string         `json:"mint"`
		Balance       money.Quantity `json:"balance"`
		Required      money.Quantity `json:"required"`
		Observer      string         `json:"observer"`
		ObservedAt    time.Time      `json:"observed_at"`
	}
	// QuoteOutput records the quote obtained.
	QuoteOutput struct {
		QuoteID string                  `json:"quote_id"`
		Quote   execution.QuoteSnapshot `json:"quote"`
		RawRef  string                  `json:"raw_ref"`
	}
	// OrderOutput records the order VALIDATE_QUOTE created (or found).
	OrderOutput struct {
		OrderID string `json:"order_id"`
		QuoteID string `json:"quote_id"`
		DryRun  bool   `json:"dry_run,omitempty"`
	}
	// FinalRiskOutput records the FINAL decision.
	FinalRiskOutput struct {
		DecisionID   string       `json:"decision_id"`
		DecisionHash string       `json:"decision_hash"`
		Verdict      risk.Verdict `json:"verdict"`
		ReasonCodes  []string     `json:"reason_codes"`
	}
	// BuildOutput records the built transaction and the attempt holding it.
	BuildOutput struct {
		AttemptID   string                   `json:"attempt_id"`
		AttemptNo   int32                    `json:"attempt_no"`
		Action      execution.UnsignedAction `json:"action"`
		EvidenceRef string                   `json:"evidence_ref"`
		DryRun      bool                     `json:"dry_run,omitempty"`
	}
	// InspectOutput records the inspector's verdict.
	InspectOutput struct {
		Approved         bool     `json:"approved"`
		ReasonCodes      []string `json:"reason_codes"`
		InspectorVersion string   `json:"inspector_version"`
		EvidenceRef      string   `json:"evidence_ref"`
	}
	// SignOutput records the signature.
	SignOutput struct {
		DecisionID   string `json:"decision_id"`
		TxSignature  string `json:"tx_signature"`
		SignedTx     []byte `json:"signed_tx"`
		SignedTxHash []byte `json:"signed_tx_hash"`
		EvidenceRef  string `json:"evidence_ref"`
	}
	// SubmitOutput records the submission (or its adoption).
	SubmitOutput struct {
		ExternalRef execution.ExternalReference `json:"external_ref"`
		AcceptedAt  time.Time                   `json:"accepted_at"`
		EvidenceRef string                      `json:"evidence_ref"`
		Adopted     bool                        `json:"adopted,omitempty"`
		Recovery    string                      `json:"recovery,omitempty"`
	}
	// ObserveOutput records the finality observed and the fills recorded.
	ObserveOutput struct {
		State       execution.ExternalState `json:"state"`
		Finality    execution.FinalityLevel `json:"finality"`
		Slot        uint64                  `json:"slot"`
		FillIDs     []string                `json:"fill_ids"`
		Observer    string                  `json:"observer"`
		EvidenceRef string                  `json:"evidence_ref"`
	}
	// ReconcileOutput records the sweep.
	ReconcileOutput struct {
		Events      int            `json:"events"`
		FillIDs     []string       `json:"fill_ids"`
		TotalInput  money.Quantity `json:"total_input"`
		TotalOutput money.Quantity `json:"total_output"`
		Matched     bool           `json:"matched"`
		EvidenceRef string         `json:"evidence_ref"`
	}
	// PostLedgerOutput records the postings.
	PostLedgerOutput struct {
		Postings []FillPosting `json:"postings"`
	}
	// FillPosting is one fill's journal transaction.
	FillPosting struct {
		FillID        string `json:"fill_id"`
		TransactionID string `json:"transaction_id"`
		Existing      bool   `json:"existing"`
	}
	// UpdatePositionOutput records the lots touched.
	UpdatePositionOutput struct {
		Applied []string `json:"applied"`
		Notes   []string `json:"notes,omitempty"`
	}
	// ReleaseOutput records the reservation's final state.
	ReleaseOutput struct {
		ReservationID string         `json:"reservation_id"`
		Status        string         `json:"status"`
		Consumed      money.Quantity `json:"consumed"`
		Released      money.Quantity `json:"released"`
	}
)
