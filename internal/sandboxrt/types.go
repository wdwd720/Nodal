package sandboxrt

import (
	"github.com/nodal/controlplane/internal/id"
)

// Termination is how a sandbox run ended. It is decided by the host from
// facts the host observed (a refused memory growth, a full output buffer, an
// expired deadline, the guest's exit status), never from what the guest
// printed.
type Termination string

// Terminations.
const (
	// TerminationOK: the module exited 0 and its output parsed.
	TerminationOK Termination = "OK"
	// TerminationTimeout: the wall-time limit expired and the module was
	// closed (an infinite loop ends here).
	TerminationTimeout Termination = "TIMEOUT"
	// TerminationMemoryLimit: the module asked for more linear memory than
	// its limit and the allocator refused it (or its declared minimum is
	// already above the limit).
	TerminationMemoryLimit Termination = "MEMORY_LIMIT"
	// TerminationOutputLimit: stdout or stderr reached its limit and the run
	// was stopped.
	TerminationOutputLimit Termination = "OUTPUT_LIMIT"
	// TerminationTrap: the module stopped abnormally: a WebAssembly trap, a
	// Go runtime fatal error or panic, or a non-zero exit status.
	TerminationTrap Termination = "TRAP"
	// TerminationInvalidOutput: the module exited 0 but wrote something
	// other than well-formed intent and log records. Nothing it wrote is
	// used.
	TerminationInvalidOutput Termination = "INVALID_OUTPUT"
	// TerminationKilledBySwitch: CAPABILITY_DISABLE for USER_CODE_SANDBOX
	// (or the global kill) was active, so the module was never started.
	TerminationKilledBySwitch Termination = "KILLED_BY_SWITCH"
	// TerminationInvalidModule: the artifact was refused before
	// instantiation: its bytes do not match its digest, it imports anything
	// outside WASI preview1, it imports a memory, or it has no _start.
	TerminationInvalidModule Termination = "INVALID_MODULE"
)

var allTerminations = []Termination{
	TerminationOK, TerminationTimeout, TerminationMemoryLimit, TerminationOutputLimit, TerminationTrap,
	TerminationInvalidOutput, TerminationKilledBySwitch, TerminationInvalidModule,
}

// AllTerminations returns every termination reason (the sandbox_runs
// termination CHECK lists exactly these).
func AllTerminations() []Termination { return append([]Termination(nil), allTerminations...) }

// Valid reports whether t is a declared termination.
func (t Termination) Valid() bool {
	for _, x := range allTerminations {
		if x == t {
			return true
		}
	}
	return false
}

// ViolationCode is a stable, machine-readable reason a record or proposal was
// refused. Run-level output violations make the run INVALID_OUTPUT;
// proposal-level and manifest violations drop only the proposal.
type ViolationCode string

// Output violations: the run's output is not the protocol.
const (
	// ViolationOutputNotJSON: a line is not exactly one JSON object.
	ViolationOutputNotJSON ViolationCode = "OUTPUT_NOT_JSON"
	// ViolationOutputRecordType: a record's type is not intent or log -- a
	// "sign", "transfer" or "withdraw" record lands here.
	ViolationOutputRecordType ViolationCode = "OUTPUT_RECORD_TYPE_UNKNOWN"
	// ViolationOutputRecordMalformed: a record has a missing, unknown,
	// duplicate or wrongly typed field, or a sequence number out of order.
	ViolationOutputRecordMalformed ViolationCode = "OUTPUT_RECORD_MALFORMED"
	// ViolationOutputTruncated: the output does not end with a newline.
	ViolationOutputTruncated ViolationCode = "OUTPUT_TRUNCATED"
	// ViolationOutputTooManyRecords: more records than the limits allow.
	ViolationOutputTooManyRecords ViolationCode = "OUTPUT_TOO_MANY_RECORDS"
)

// Proposal violations: one proposal or log record is dropped.
const (
	// ViolationKindNotProposable: the intent kind is unknown or one a
	// strategy may never propose (TRANSFER_VALUE, WITHDRAW, BRIDGE_VALUE...).
	ViolationKindNotProposable ViolationCode = "KIND_NOT_PROPOSABLE"
	// ViolationInstrumentInvalid: the instrument is not a canonical uuid.
	ViolationInstrumentInvalid ViolationCode = "INSTRUMENT_INVALID"
	// ViolationInstrumentNotInSnapshot: the instrument is not in the input.
	ViolationInstrumentNotInSnapshot ViolationCode = "INSTRUMENT_NOT_IN_SNAPSHOT"
	// ViolationSideInvalid: not BUY/SELL, or a side the kind does not allow.
	ViolationSideInvalid ViolationCode = "SIDE_INVALID"
	// ViolationQuantityInvalid: not an exact positive integer of base units.
	ViolationQuantityInvalid ViolationCode = "QUANTITY_INVALID"
	// ViolationLimitPriceInvalid: present and not a positive decimal.
	ViolationLimitPriceInvalid ViolationCode = "LIMIT_PRICE_INVALID"
	// ViolationLogInvalid: a log record's event or fields are malformed.
	ViolationLogInvalid ViolationCode = "LOG_INVALID"
)

// Manifest violations: the proposal is well-formed but asks for more than
// the strategy declared.
const (
	ViolationManifestKind       ViolationCode = "MANIFEST_KIND_NOT_ALLOWED"
	ViolationManifestInstrument ViolationCode = "MANIFEST_INSTRUMENT_NOT_ALLOWED"
	ViolationManifestNotional   ViolationCode = "MANIFEST_NOTIONAL_EXCEEDED"
	ViolationManifestMaxIntents ViolationCode = "MANIFEST_MAX_INTENTS_EXCEEDED"
)

var allViolationCodes = []ViolationCode{
	ViolationOutputNotJSON, ViolationOutputRecordType, ViolationOutputRecordMalformed, ViolationOutputTruncated,
	ViolationOutputTooManyRecords,
	ViolationKindNotProposable, ViolationInstrumentInvalid, ViolationInstrumentNotInSnapshot, ViolationSideInvalid,
	ViolationQuantityInvalid, ViolationLimitPriceInvalid, ViolationLogInvalid,
	ViolationManifestKind, ViolationManifestInstrument, ViolationManifestNotional, ViolationManifestMaxIntents,
}

// AllViolationCodes returns every run violation code (the
// sandbox_run_violations code CHECK lists exactly these).
func AllViolationCodes() []ViolationCode { return append([]ViolationCode(nil), allViolationCodes...) }

// Valid reports whether c is a declared violation code.
func (c ViolationCode) Valid() bool {
	for _, x := range allViolationCodes {
		if x == c {
			return true
		}
	}
	return false
}

// RunLevel reports whether the violation invalidates the whole run.
func (c ViolationCode) RunLevel() bool {
	switch c {
	case ViolationOutputNotJSON, ViolationOutputRecordType, ViolationOutputRecordMalformed,
		ViolationOutputTruncated, ViolationOutputTooManyRecords:
		return true
	}
	return false
}

// Violation is one refused record or proposal.
type Violation struct {
	Code ViolationCode `json:"code"`
	// Seq is the 1-based output record number (0 when the violation is not
	// tied to one record, e.g. truncation).
	Seq    int    `json:"seq"`
	Detail string `json:"detail"`
}

// Side is a proposal's side.
type Side string

// Sides.
const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

type (
	runKind      struct{}
	artifactKind struct{}
)

// RunID identifies a sandbox run.
type RunID = id.ID[runKind]

// ArtifactID identifies a strategy artifact row.
type ArtifactID = id.ID[artifactKind]

// NewRunID returns a fresh run id.
func NewRunID() RunID { return id.New[runKind]() }

// ParseRunID parses the canonical form.
func ParseRunID(s string) (RunID, error) { return id.Parse[runKind](s) }
