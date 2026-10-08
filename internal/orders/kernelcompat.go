package orders

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// allowSeveralIntentSteps lets the caller's transaction move an economic
// intent (or effect) through several statuses, as Service.Submit does.
//
// On this branch the deferred binding of migration 00731 keeps one flagged
// edge per entity per transaction, so a second status change of the same
// intent fails AU001 at COMMIT. Checking the binding IMMEDIATE instead checks
// every UPDATE against the transition row written just before it --
// econ.IntentStore.Transition and econ.EffectStore write the row naming both
// endpoints before the UPDATE -- so every step still needs its own transition
// row and nothing is weakened. Migration 00733 (main) makes the deferred
// binding accumulate edges, after which this helper and its one call are
// deleted.
func allowSeveralIntentSteps(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS economic_intents_require_transition, economic_effects_require_transition IMMEDIATE`); err != nil {
		return dbErr("check intent and effect bindings immediately", err)
	}
	return nil
}
