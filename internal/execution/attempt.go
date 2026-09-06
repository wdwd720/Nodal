package execution

import (
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// AttemptStatus is the execution_attempts.status value.
type AttemptStatus string

// Attempt statuses.
const (
	AttemptBuilt              AttemptStatus = "BUILT"
	AttemptInspected          AttemptStatus = "INSPECTED"
	AttemptInspectionRejected AttemptStatus = "INSPECTION_REJECTED"
	AttemptSigningRequested   AttemptStatus = "SIGNING_REQUESTED"
	AttemptSigned             AttemptStatus = "SIGNED"
	AttemptSigningRejected    AttemptStatus = "SIGNING_REJECTED"
	AttemptSubmitting         AttemptStatus = "SUBMITTING"
	AttemptSubmitted          AttemptStatus = "SUBMITTED"
	AttemptSubmissionUnknown  AttemptStatus = "SUBMISSION_UNKNOWN"
	AttemptObserved           AttemptStatus = "OBSERVED"
	AttemptConfirmed          AttemptStatus = "CONFIRMED"
	AttemptFinalized          AttemptStatus = "FINALIZED"
	AttemptFailed             AttemptStatus = "FAILED"
	AttemptExpired            AttemptStatus = "EXPIRED"
	AttemptAdopted            AttemptStatus = "ADOPTED"
)

// AllAttemptStatuses returns every status in declaration order.
func AllAttemptStatuses() []AttemptStatus {
	return []AttemptStatus{
		AttemptBuilt, AttemptInspected, AttemptInspectionRejected, AttemptSigningRequested, AttemptSigned,
		AttemptSigningRejected, AttemptSubmitting, AttemptSubmitted, AttemptSubmissionUnknown, AttemptObserved,
		AttemptConfirmed, AttemptFinalized, AttemptFailed, AttemptExpired, AttemptAdopted,
	}
}

// AttemptTransitions is the explicit attempt state machine. ADOPTED is the
// PART 48 outcome of a SUBMISSION_UNKNOWN attempt whose transaction was found
// on chain; it then observes finality like a submitted one.
var AttemptTransitions = map[AttemptStatus][]AttemptStatus{
	AttemptBuilt:              {AttemptInspected, AttemptInspectionRejected, AttemptExpired, AttemptFailed},
	AttemptInspected:          {AttemptSigningRequested, AttemptExpired, AttemptFailed},
	AttemptInspectionRejected: {},
	AttemptSigningRequested:   {AttemptSigned, AttemptSigningRejected, AttemptFailed},
	AttemptSigned:             {AttemptSubmitting, AttemptExpired, AttemptFailed},
	AttemptSigningRejected:    {},
	AttemptSubmitting:         {AttemptSubmitted, AttemptSubmissionUnknown, AttemptFailed, AttemptExpired},
	AttemptSubmitted:          {AttemptObserved, AttemptConfirmed, AttemptFinalized, AttemptSubmissionUnknown, AttemptFailed, AttemptExpired},
	AttemptSubmissionUnknown:  {AttemptAdopted, AttemptExpired, AttemptFailed},
	AttemptObserved:           {AttemptConfirmed, AttemptFinalized, AttemptFailed, AttemptExpired},
	AttemptConfirmed:          {AttemptFinalized, AttemptFailed},
	AttemptFinalized:          {},
	AttemptFailed:             {},
	AttemptExpired:            {},
	AttemptAdopted:            {AttemptObserved, AttemptConfirmed, AttemptFinalized, AttemptFailed, AttemptExpired},
}

// Valid reports whether s is declared.
func (s AttemptStatus) Valid() bool {
	_, ok := AttemptTransitions[s]
	return ok
}

// Terminal reports whether s has no outgoing transition.
func (s AttemptStatus) Terminal() bool {
	return s.Valid() && len(AttemptTransitions[s]) == 0
}

// Recoverable reports whether the attempt's fate is not yet known and the
// recovery worker must keep watching it (index execution_attempts_unknown_idx).
func (s AttemptStatus) Recoverable() bool {
	switch s {
	case AttemptSubmitted, AttemptSubmissionUnknown, AttemptObserved, AttemptConfirmed:
		return true
	}
	return false
}

// CanTransitionAttempt reports whether from → to is legal.
func CanTransitionAttempt(from, to AttemptStatus) bool {
	for _, s := range AttemptTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

func checkAttemptTransition(id AttemptID, from, to AttemptStatus) error {
	if CanTransitionAttempt(from, to) {
		return nil
	}
	return errs.Newf(errs.CodeInvalidStateTransition, "attempt is %s; %s -> %s is not allowed", from, from, to).
		WithField("attempt_id", id.String()).WithField("from", string(from)).WithField("to", string(to))
}

// Attempt mirrors one execution_attempts row (migration 00300). Every field
// after Status is evidence written once as the attempt advances.
type Attempt struct {
	ID                   AttemptID
	OrderID              OrderID
	PlanID               string // uuid text
	AttemptNo            int32
	WalletID             string // uuid text
	Provider             string
	ProviderRequestID    string
	QuoteID              string // uuid text
	UnsignedTxHash       []byte
	UnsignedTxRef        string
	SignedTxHash         []byte
	TxSignature          string
	RecentBlockhash      string
	LastValidBlockHeight *int64
	SimulationRef        string
	SimulationOK         *bool
	InspectionResult     json.RawMessage
	SigningDecisionID    string // uuid text
	Status               AttemptStatus
	Finality             FinalityLevel
	SubmittedAt          *time.Time
	SubmitResponseRef    string
	ObservedAt           *time.Time
	ConfirmedAt          *time.Time
	FinalizedAt          *time.Time
	Error                string
	CorrelationID        string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Validate checks the structural rules of a new attempt.
func (a Attempt) Validate() error {
	problems := map[string]any{}
	if a.ID.IsZero() {
		problems["id"] = "required"
	}
	if a.OrderID.IsZero() {
		problems["order_id"] = "required"
	}
	for k, v := range map[string]string{"plan_id": a.PlanID, "wallet_id": a.WalletID, "provider": a.Provider, "quote_id": a.QuoteID, "correlation_id": a.CorrelationID} {
		if v == "" {
			problems[k] = "required"
		}
	}
	if !a.Status.Valid() {
		problems["status"] = "unknown status"
	}
	if a.Finality != "" && !a.Finality.Valid() {
		problems["finality"] = "unknown finality level"
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "execution: invalid attempt").WithFields(problems)
	}
	return nil
}

// AttemptPatch names the evidence fields Update may set. A nil pointer
// leaves the column untouched. Status changes are validated against
// AttemptTransitions.
type AttemptPatch struct {
	Status               *AttemptStatus
	ProviderRequestID    *string
	UnsignedTxHash       []byte
	UnsignedTxRef        *string
	RecentBlockhash      *string
	LastValidBlockHeight *int64
	SimulationRef        *string
	SimulationOK         *bool
	InspectionResult     json.RawMessage
	SigningDecisionID    *string
	Finality             *FinalityLevel
	SubmittedAt          *time.Time
	SubmitResponseRef    *string
	ObservedAt           *time.Time
	ConfirmedAt          *time.Time
	FinalizedAt          *time.Time
	Error                *string
	// Reason is recorded on the outbox event when Status changes.
	Reason string
}
