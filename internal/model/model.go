package model

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/money"
)

// Purpose distinguishes a compile-time call from a runtime one; it is
// persisted on every provenance row (model_calls.purpose).
type Purpose string

// Purposes.
const (
	PurposeCompile  Purpose = "COMPILE"
	PurposeRuntime  Purpose = "RUNTIME"
	PurposeBacktest Purpose = "BACKTEST"
)

// Valid reports whether p is declared.
func (p Purpose) Valid() bool {
	return p == PurposeCompile || p == PurposeRuntime || p == PurposeBacktest
}

// SegmentKind labels a prompt segment. There is no kind for instructions:
// the system policy is a separate, non-repeating field of Request.
type SegmentKind string

// Segment kinds (STRATEGY_IR.md §10).
const (
	SegmentToolResult SegmentKind = "TOOL_RESULT"
	SegmentUntrusted  SegmentKind = "UNTRUSTED"
)

// Valid reports whether k is declared.
func (k SegmentKind) Valid() bool { return k == SegmentToolResult || k == SegmentUntrusted }

// Segment is one labeled block of data in a prompt. Content is always
// data: a segment can never contribute an instruction, a tool, an effect or
// a destination, and the renderer wraps it so the model sees it as data.
type Segment struct {
	Kind SegmentKind
	// Label names the segment for the reader ("validation_errors",
	// "token_description"). It is rendered, so it is checked like content.
	Label string
	// ProvenanceRef ties the segment to the row it came from (a
	// tool_invocations id, a compile_attempts id). Persisted with the call.
	ProvenanceRef string
	Content       string
}

// Request is one model invocation. The three prompt sections are separate
// fields rather than a single string so that no caller can accidentally
// concatenate untrusted text into the instruction channel (PART 67).
type Request struct {
	// TemplateVersion identifies the SystemPolicy text. Persisted so an old
	// compile can be reproduced with the prompt it actually used.
	TemplateVersion string
	// SystemPolicy is the instruction channel. It comes from a versioned
	// constant in the repository, never from user input.
	SystemPolicy string
	ToolResults  []Segment
	Untrusted    []Segment
	// OutputSchema constrains the response. It must be a closed JSON schema
	// (additionalProperties false throughout); the provider rejects open
	// maps, and so does Validate.
	OutputSchema    json.RawMessage
	MaxOutputTokens int64
	// Model is the provider model identifier ("claude-opus-5"). Empty means
	// the provider's configured default.
	Model    string
	Purpose  Purpose
	Deadline time.Time
	// TenantHash is an opaque per-user identifier for provider-side abuse
	// attribution. It is a hash, never an email or an account id.
	TenantHash string
}

// Usage is the token accounting of one call, as integers.
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	// Cost is priced from a configured table into USD minor units. It is
	// never a float and never a provider-reported currency amount.
	Cost money.USD
}

// TotalInputTokens is the provider's definition: cache reads and cache
// writes are billed separately from fresh input tokens but all count as
// input for reporting.
func (u Usage) TotalInputTokens() int64 {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// Response is one completed model invocation.
type Response struct {
	Provider string
	ModelID  string
	// Structured is the schema-constrained JSON body. It is a candidate:
	// nothing acts on it until the caller has parsed and validated it.
	Structured json.RawMessage
	// StopReason is the provider's stop reason. Only a normal completion is
	// a usable result; anything else (truncation, refusal, an unknown value)
	// is an error, because a truncated JSON document must never be repaired.
	StopReason  string
	Usage       Usage
	RequestedAt time.Time
	RespondedAt time.Time
	// InputHash is sha256 of the canonical rendered prompt, OutputHash of
	// the structured body. Both are persisted (PART 65).
	InputHash  []byte
	OutputHash []byte
	// RequestID is the provider's request id, kept for support escalation.
	RequestID string
	Latency   time.Duration
}

// Provider is a language model. Implementations must not retry in a way
// that hides cost: every dial is billable and every attempt is recorded.
type Provider interface {
	// Name is the provider identifier persisted with each call ("anthropic").
	Name() string
	// Complete performs one schema-constrained inference. It returns a typed
	// error (see errors.go) and never a synthetic or partial result.
	Complete(ctx context.Context, req Request) (Response, error)
}
