package reconciliation

import "github.com/nodal/controlplane/internal/id"

type (
	recordKind      struct{}
	transitionKind  struct{}
	observationKind struct{}
)

// RecordID identifies a reconciliation_records row.
type RecordID = id.ID[recordKind]

// TransitionID identifies a reconciliation_transitions row.
type TransitionID = id.ID[transitionKind]

// ObservationID identifies a wallet_balance_observations row.
type ObservationID = id.ID[observationKind]

// NewRecordID mints a record id.
func NewRecordID() RecordID { return id.New[recordKind]() }

// NewTransitionID mints a transition id.
func NewTransitionID() TransitionID { return id.New[transitionKind]() }

// NewObservationID mints a balance observation id.
func NewObservationID() ObservationID { return id.New[observationKind]() }

// ParseRecordID parses the canonical 36-character form.
func ParseRecordID(s string) (RecordID, error) { return id.Parse[recordKind](s) }
