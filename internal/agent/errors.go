package agent

import (
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
)

// Custom SQLSTATEs raised by the agent-runtime migrations (00501, 00502).
const (
	// SQLStateAgentIntentRequiresPrediction (AG001): an AGENT intent was
	// inserted without prediction / strategy version / agent linkage.
	SQLStateAgentIntentRequiresPrediction = "AG001"
	// SQLStatePredictionAfterIntent (AG002): the prediction was committed
	// after the intent was requested. Prediction must predate execution.
	SQLStatePredictionAfterIntent = "AG002"
	// SQLStatePredictionIntentMismatch (AG003): agent, strategy version, mode
	// or account of the prediction and the intent disagree.
	SQLStatePredictionIntentMismatch = "AG003"
	// SQLStateAgentRunImmutable (AG004): the identity, mode or linkage of an
	// agent run was changed, or a run was deleted.
	SQLStateAgentRunImmutable = "AG004"
	// SQLStateAuditTransitionRequired (AU001): a state column changed without
	// a matching transition row in the same transaction (migration 00603 and,
	// for agents, migration 00690).
	SQLStateAuditTransitionRequired = "AU001"
)

// IsAgentRunImmutable reports whether err is the AG004 guard.
func IsAgentRunImmutable(err error) bool { return db.SQLState(err) == SQLStateAgentRunImmutable }

// IsPredictionAfterIntent reports whether err is the AG002 guard: the trigger
// refused an intent whose prediction was committed after it.
func IsPredictionAfterIntent(err error) bool {
	return db.SQLState(err) == SQLStatePredictionAfterIntent
}

// IsPredictionIntentMismatch reports whether err is the AG003 guard.
func IsPredictionIntentMismatch(err error) bool {
	return db.SQLState(err) == SQLStatePredictionIntentMismatch
}

// IsIntentRequiresPrediction reports whether err is the AG001 guard.
func IsIntentRequiresPrediction(err error) bool {
	return db.SQLState(err) == SQLStateAgentIntentRequiresPrediction
}

// IsTransitionRequired reports whether err is the AU001 binding guard: a
// state change was attempted without its transition row.
func IsTransitionRequired(err error) bool {
	return db.SQLState(err) == SQLStateAuditTransitionRequired
}

// IsImmutableRow reports whether err is any of the immutability guards that
// protect agent-runtime history: the generic forbid_mutation trigger (LG003)
// used by lifecycle transitions, tool invocations, model calls and
// predictions, the agent-run guard (AG004), or a missing privilege.
func IsImmutableRow(err error) bool {
	return db.IsMutationForbidden(err) || IsAgentRunImmutable(err)
}

// isNoRows reports whether err is pgx's empty-result sentinel.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
