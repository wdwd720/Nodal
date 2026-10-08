package devnethost

import (
	"errors"

	"github.com/nodal/controlplane/internal/errs"
)

// Reason codes the host puts in an error's "reason" field. Devnet's own
// refusals pass through unchanged with devnet's reason codes.
const (
	// ReasonReplayDiverged: replaying the stored history did not reproduce
	// what the history recorded (a different state hash, a different
	// submission outcome, a broken hash chain). The host refuses to run on a
	// history it cannot reproduce.
	ReasonReplayDiverged = "HOST_REPLAY_DIVERGED"
	// ReasonConfigMismatch: the stored GENESIS was produced under a different
	// chain configuration than the host was opened with.
	ReasonConfigMismatch = "HOST_CONFIG_MISMATCH"
	// ReasonStoreUnavailable: the event store could not be read or written.
	ReasonStoreUnavailable = "HOST_STORE_UNAVAILABLE"
	// ReasonFaultInjectionForbidden: a fault hook was called outside LOCAL,
	// TEST and DEV.
	ReasonFaultInjectionForbidden = "FAULT_INJECTION_FORBIDDEN"
	// ReasonSubmissionPersistenceUnknown: the chain accepted a submission but
	// recording it in the event store failed, so whether the submission is
	// part of the durable history is unknown. The caller must observe Status.
	ReasonSubmissionPersistenceUnknown = "SUBMISSION_PERSISTENCE_UNKNOWN"
	// ReasonViewClosed: a View was used after its callback returned.
	ReasonViewClosed = "HOST_VIEW_CLOSED"
)

const reasonField = "reason"

func diverged(detail string) error {
	return errs.New(errs.CodeInternal, "devnethost: replay diverged: "+detail).WithField(reasonField, ReasonReplayDiverged)
}

// storeUnavailable classifies a store failure. An *errs.Error raised by the
// host itself (a replay divergence inside Exclusive, a refused position)
// passes through; anything else -- a connection, lock or commit failure -- is
// PROVIDER_UNAVAILABLE.
func storeUnavailable(err error, op string) error {
	if e, ok := errs.As(err); ok {
		return e
	}
	return errs.Wrap(err, errs.CodeProviderUnavailable, "devnethost: event store: "+op).WithField(reasonField, ReasonStoreUnavailable)
}

func faultForbidden(op string) error {
	return errs.New(errs.CodeForbidden, "devnethost: "+op+" is fault injection, available only in LOCAL, TEST and DEV").
		WithField(reasonField, ReasonFaultInjectionForbidden)
}

// ReasonOf returns the host reason code carried by err, or "" when there is
// none. Devnet refusals carry devnet's codes; use devnet.ReasonOf for those.
func ReasonOf(err error) string {
	var e *errs.Error
	if !errors.As(err, &e) {
		return ""
	}
	r, _ := e.Fields[reasonField].(string)
	return r
}
