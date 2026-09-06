package execution

import "github.com/nodal/controlplane/internal/id"

type (
	orderKind      struct{}
	attemptKind    struct{}
	fillKind       struct{}
	transitionKind struct{}
)

// OrderID identifies an orders row.
type OrderID = id.ID[orderKind]

// AttemptID identifies an execution_attempts row.
type AttemptID = id.ID[attemptKind]

// FillID identifies a fills row.
type FillID = id.ID[fillKind]

// TransitionID identifies an order_transitions row.
type TransitionID = id.ID[transitionKind]

// NewOrderID mints an order id.
func NewOrderID() OrderID { return id.New[orderKind]() }

// NewAttemptID mints an attempt id.
func NewAttemptID() AttemptID { return id.New[attemptKind]() }

// NewFillID mints a fill id.
func NewFillID() FillID { return id.New[fillKind]() }

// NewTransitionID mints a transition id.
func NewTransitionID() TransitionID { return id.New[transitionKind]() }

// ParseOrderID parses the canonical form.
func ParseOrderID(s string) (OrderID, error) { return id.Parse[orderKind](s) }

// ParseAttemptID parses the canonical form.
func ParseAttemptID(s string) (AttemptID, error) { return id.Parse[attemptKind](s) }

// ParseFillID parses the canonical form.
func ParseFillID(s string) (FillID, error) { return id.Parse[fillKind](s) }
