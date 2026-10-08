package sandboxrt

import (
	"crypto/sha256"
	"sort"

	"github.com/nodal/controlplane/internal/errs"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
)

// Result is a run as the host understands it.
type Result struct {
	RawRun
	// Proposals are the validated intent proposals of an OK run, in record
	// order. Empty for every other termination: nothing a failed run wrote
	// is used.
	Proposals []Proposal
	// Logs are the valid log records of an OK run.
	Logs []LogRecord
	// Violations are every refused record: the run-level ones that made the
	// run INVALID_OUTPUT, or the proposal- and log-level ones of an OK run.
	Violations []Violation
	// OutputDigest is sha256(stdout).
	OutputDigest []byte
	// RunDigest is sha256(artifact digest || input digest || stdout): the
	// same artifact on the same input producing the same bytes has the same
	// run digest.
	RunDigest []byte
}

// RunDigest computes sha256(artifact digest || input digest || output).
func RunDigest(artifactDigest, inputDigest, output []byte) []byte {
	h := sha256.New()
	h.Write(artifactDigest)
	h.Write(inputDigest)
	h.Write(output)
	return h.Sum(nil)
}

// Interpret reads a raw run. A run that did not exit 0 keeps its
// termination and yields nothing. A run that exited 0 is INVALID_OUTPUT if
// any line breaks the protocol; otherwise every intent record is validated
// against the snapshot the strategy ran on, and every malformed proposal or
// log record is dropped with a violation.
func Interpret(raw RawRun, limits Limits) (Result, error) {
	out := sha256.Sum256(raw.Stdout)
	res := Result{RawRun: raw, OutputDigest: out[:], RunDigest: RunDigest(raw.ArtifactDigest, raw.InputDigest, raw.Stdout)}
	if raw.Termination != TerminationOK {
		return res, nil
	}
	parsed := ParseOutput(raw.Stdout, limits.MaxRecords)
	if len(parsed.Violations) > 0 {
		res.Termination = TerminationInvalidOutput
		res.Detail = string(parsed.Violations[0].Code)
		res.Violations = parsed.Violations
		return res, nil
	}
	snap, err := nodalsdk.DecodeSnapshot(raw.Input)
	if err != nil {
		// The host validated this exact document before the run; failing to
		// read it back is a host defect, never a strategy outcome.
		return Result{}, errs.Wrap(err, errs.CodeInternal, "sandboxrt: the run's own input no longer decodes")
	}
	for _, rec := range parsed.Intents {
		p, v := ValidateProposal(rec, snap)
		if v != nil {
			res.Violations = append(res.Violations, *v)
			continue
		}
		res.Proposals = append(res.Proposals, p)
	}
	for _, rec := range parsed.Logs {
		if v := ValidateLog(rec); v != nil {
			res.Violations = append(res.Violations, *v)
			continue
		}
		res.Logs = append(res.Logs, rec)
	}
	sortViolations(res.Violations)
	return res, nil
}

func sortViolations(v []Violation) {
	sort.SliceStable(v, func(i, j int) bool {
		if v[i].Seq != v[j].Seq {
			return v[i].Seq < v[j].Seq
		}
		return v[i].Code < v[j].Code
	})
}
