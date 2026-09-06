package model

import (
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// ParseResult records what happened when the structured output was decoded
// (compile_attempts.parse_result, model_calls.parse_result).
type ParseResult string

// Parse results.
const (
	ParseOK              ParseResult = "OK"
	ParseInvalidJSON     ParseResult = "INVALID_JSON"
	ParseSchemaViolation ParseResult = "SCHEMA_VIOLATION"
	ParseTooLarge        ParseResult = "TOO_LARGE"
	ParseNotAttempted    ParseResult = "NOT_ATTEMPTED"
)

// Valid reports whether r is one of the values the CHECK constraint allows.
func (r ParseResult) Valid() bool {
	switch r {
	case ParseOK, ParseInvalidJSON, ParseSchemaViolation, ParseTooLarge, ParseNotAttempted:
		return true
	}
	return false
}

// Provenance is the record of one model interaction (goal PART 65). It is
// what makes a compiled strategy admissible: provider, model identity, the
// prompt that produced it, the output, and what parsing made of it.
//
// It deliberately has no field for reasoning traces. Hidden
// chain-of-thought is never requested, stored or exposed (PART 178); the
// user-facing explanation is the structured rationale inside Structured.
type Provenance struct {
	Provider        string
	ModelID         string
	TemplateVersion string
	Purpose         Purpose

	RequestedAt time.Time
	RespondedAt time.Time
	Latency     time.Duration

	// InputHash is sha256 of the rendered prompt; PromptRef is the archive
	// URI of the prompt itself under retention class MODEL_IO. The prompt is
	// archived rather than inlined so an injection attempt is evidence.
	InputHash  []byte
	PromptRef  string
	OutputHash []byte
	OutputRef  string

	Structured  json.RawMessage
	ParseResult ParseResult
	StopReason  string

	Usage Usage
	// RequestID is the provider's request id.
	RequestID string
	Success   bool
	ErrorCode string
}

// NewProvenance builds the record for a successful call. The caller
// supplies the parse result, because whether the body was usable is decided
// after this package hands it back.
func NewProvenance(req Request, resp Response, parse ParseResult) Provenance {
	return Provenance{
		Provider:        resp.Provider,
		ModelID:         resp.ModelID,
		TemplateVersion: req.TemplateVersion,
		Purpose:         req.Purpose,
		RequestedAt:     resp.RequestedAt,
		RespondedAt:     resp.RespondedAt,
		Latency:         resp.Latency,
		InputHash:       resp.InputHash,
		OutputHash:      resp.OutputHash,
		Structured:      resp.Structured,
		ParseResult:     parse,
		StopReason:      resp.StopReason,
		Usage:           resp.Usage,
		RequestID:       resp.RequestID,
		Success:         true,
	}
}

// NewFailureProvenance builds the record for a call that did not produce a
// usable response. A failure is recorded with the same rigor as a success:
// an attempt that vanished is indistinguishable from one that never
// happened, and PART 177 forbids inventing output to fill the gap.
func NewFailureProvenance(req Request, provider string, requestedAt, failedAt time.Time, err error) Provenance {
	return Provenance{
		Provider:        provider,
		ModelID:         req.Model,
		TemplateVersion: req.TemplateVersion,
		Purpose:         req.Purpose,
		RequestedAt:     requestedAt,
		RespondedAt:     failedAt,
		Latency:         failedAt.Sub(requestedAt),
		ParseResult:     ParseNotAttempted,
		Usage:           Usage{Cost: money.USDFromMinor(0)},
		Success:         false,
		ErrorCode:       string(errs.CodeOf(err)),
	}
}

// Validate checks the record can be persisted: the columns that are NOT
// NULL in model_calls and compile_attempts are present and consistent.
func (p Provenance) Validate() error {
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	if p.Provider == "" {
		fail("provider", "required")
	}
	if p.TemplateVersion == "" {
		fail("template_version", "required: an unreproducible prompt is not admissible")
	}
	if !p.Purpose.Valid() {
		fail("purpose", "must be COMPILE, RUNTIME or BACKTEST")
	}
	if !p.ParseResult.Valid() {
		fail("parse_result", "must be OK, INVALID_JSON, SCHEMA_VIOLATION, TOO_LARGE or NOT_ATTEMPTED")
	}
	if p.RequestedAt.IsZero() {
		fail("requested_at", "required")
	}
	if p.Success {
		if p.ModelID == "" {
			fail("model_id", "required on a successful call")
		}
		if len(p.InputHash) != 32 {
			fail("input_hash", "must be a sha256 digest")
		}
		if len(p.OutputHash) != 32 {
			fail("output_hash", "must be a sha256 digest")
		}
		if p.RespondedAt.IsZero() {
			fail("responded_at", "required on a successful call")
		}
		if p.ParseResult == ParseNotAttempted {
			fail("parse_result", "a successful call must record what parsing made of the body")
		}
	}
	if p.Usage.InputTokens < 0 || p.Usage.OutputTokens < 0 {
		fail("usage", "token counts must not be negative")
	}
	if p.Usage.Cost.IsNegative() {
		fail("usage.cost", "cost must not be negative")
	}
	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "model: invalid provenance record").WithFields(fields)
	}
	return nil
}
