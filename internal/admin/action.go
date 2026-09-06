package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

type (
	actionKind     struct{}
	transitionKind struct{}
)

// ActionID identifies an admin_actions row.
type ActionID = id.ID[actionKind]

// NewActionID returns a fresh action id.
func NewActionID() ActionID { return id.New[actionKind]() }

// ParseActionID parses the canonical form.
func ParseActionID(s string) (ActionID, error) { return id.Parse[actionKind](s) }

// MinReasonLength is the minimum number of characters in a proposal reason
// (mirrors the table CHECK).
const MinReasonLength = 8

// Status is the state of an action.
type Status string

// Statuses.
const (
	StatusProposed  Status = "PROPOSED"
	StatusApproved  Status = "APPROVED"
	StatusRejected  Status = "REJECTED"
	StatusExecuted  Status = "EXECUTED"
	StatusFailed    Status = "FAILED"
	StatusExpired   Status = "EXPIRED"
	StatusCancelled Status = "CANCELLED"

	// StatusNone is the from_status recorded for the proposal transition.
	StatusNone Status = "NONE"
)

var allStatuses = []Status{StatusProposed, StatusApproved, StatusRejected, StatusExecuted, StatusFailed, StatusExpired, StatusCancelled}

// AllStatuses returns every persisted status in declaration order.
func AllStatuses() []Status { return append([]Status(nil), allStatuses...) }

// Valid reports whether s is a persisted status.
func (s Status) Valid() bool {
	for _, x := range allStatuses {
		if x == s {
			return true
		}
	}
	return false
}

// Pending reports whether the action can still move forward.
func (s Status) Pending() bool { return s == StatusProposed || s == StatusApproved }

// Terminal reports whether no transition leaves s.
func (s Status) Terminal() bool { return s.Valid() && len(transitions[s]) == 0 }

// transitions is the complete state machine. EXECUTED and FAILED are reached
// from PROPOSED only for kinds that do not require dual control; Execute
// enforces that per kind.
var transitions = map[Status][]Status{
	StatusProposed:  {StatusApproved, StatusRejected, StatusExecuted, StatusFailed, StatusExpired, StatusCancelled},
	StatusApproved:  {StatusExecuted, StatusFailed, StatusExpired},
	StatusRejected:  {},
	StatusExecuted:  {},
	StatusFailed:    {},
	StatusExpired:   {},
	StatusCancelled: {},
}

// CanTransition reports whether the state machine allows from → to.
func CanTransition(from, to Status) bool {
	for _, x := range transitions[from] {
		if x == to {
			return true
		}
	}
	return false
}

// Proposal is the input to Propose.
type Proposal struct {
	Kind       Kind
	TargetType string
	TargetID   string
	// Params is a JSON object typed by the kind; it is canonicalized and
	// hashed. nil means {}.
	Params json.RawMessage
	// Reason is mandatory and at least MinReasonLength characters.
	Reason string
	// CorrelationID links the action to the request or incident that
	// produced it; empty falls back to the context's correlation id.
	CorrelationID string
}

// Action is one admin_actions row. Times are UTC with microsecond
// precision; optional columns are pointers (nil = NULL). The JSON tags
// define the snapshot hashed into audit before/after hashes.
type Action struct {
	ID               ActionID        `json:"id"`
	Kind             Kind            `json:"kind"`
	TargetType       string          `json:"target_type"`
	TargetID         string          `json:"target_id"`
	Params           json.RawMessage `json:"params"`
	ParamsHash       []byte          `json:"params_hash"`
	Reason           string          `json:"reason"`
	RequiresDual     bool            `json:"requires_dual"`
	Status           Status          `json:"status"`
	ProposedBy       string          `json:"proposed_by"`
	ProposedAt       time.Time       `json:"proposed_at"`
	ProposerStepUpAt time.Time       `json:"proposer_step_up_at"`
	ApprovedBy       *string         `json:"approved_by"`
	ApprovedAt       *time.Time      `json:"approved_at"`
	ApproverStepUpAt *time.Time      `json:"approver_step_up_at"`
	ApprovalNote     *string         `json:"approval_note"`
	RejectedBy       *string         `json:"rejected_by"`
	RejectedAt       *time.Time      `json:"rejected_at"`
	RejectedReason   *string         `json:"rejected_reason"`
	ExecutedAt       *time.Time      `json:"executed_at"`
	ExecutionResult  json.RawMessage `json:"execution_result"`
	ExecutionError   *string         `json:"execution_error"`
	ExpiresAt        time.Time       `json:"expires_at"`
	CorrelationID    *string         `json:"correlation_id"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// Expired reports whether the action can no longer be approved or executed
// at now (expiry is inclusive: expires_at itself is expired).
func (a Action) Expired(now time.Time) bool { return !now.Before(a.ExpiresAt) }

// ParamsHash returns sha256(audit.CanonicalJSON(params)) after validating
// that params is a JSON object. It is what Propose stores and Execute
// recomputes; key order and whitespace in params never change it.
func ParamsHash(params json.RawMessage) ([]byte, error) {
	canonical, err := canonicalParams(params)
	if err != nil {
		return nil, err
	}
	return hashBytes(canonical), nil
}

// ParamsMatch reports whether stored params re-hash to hash.
func ParamsMatch(params json.RawMessage, hash []byte) bool {
	got, err := ParamsHash(params)
	if err != nil {
		return false
	}
	return bytes.Equal(got, hash)
}

func hashBytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// canonicalParams validates that params is a JSON object (nil → {}) and
// returns its canonical text.
func canonicalParams(params json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(params)) == 0 {
		params = json.RawMessage(`{}`)
	}
	canonical, err := audit.CanonicalJSON(params)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "admin: params must be canonical JSON").WithField("field", "params")
	}
	if len(canonical) == 0 || canonical[0] != '{' {
		return nil, errs.New(errs.CodeValidationFailed, "admin: params must be a JSON object").WithField("field", "params")
	}
	return canonical, nil
}

// snapshotHash hashes the action's persisted state for audit before/after
// hashes.
func snapshotHash(a Action) ([]byte, error) {
	b, err := audit.CanonicalJSON(a)
	if err != nil {
		return nil, fmt.Errorf("admin: snapshot hash: %w", err)
	}
	return hashBytes(b), nil
}

// validText reports whether s can be stored in a text column.
func validText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut
}
