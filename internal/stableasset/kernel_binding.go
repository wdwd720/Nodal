package stableasset

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// immediateKernelBindings makes the econ intent and effect transition
// bindings check each status change at the end of its own statement instead
// of at COMMIT. A transaction that moves one intent through several statuses
// (admission: RECEIVED -> ROUTED -> RISK_APPROVED -> RESERVED -> EXECUTING)
// needs it on this branch, where the deferred AU001 binding keeps a single
// edge per entity per transaction. IntentStore.Transition and the
// EffectStore write each transition row before the UPDATE it licenses, so
// every immediate check passes; the binding itself is not weakened.
//
// INTEGRATOR: delete this helper and its two call sites once migration 00733
// (accumulated edges) is merged.
func immediateKernelBindings(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS economic_intents_require_transition, economic_effects_require_transition IMMEDIATE`); err != nil {
		return dbErr("make kernel transition bindings immediate", err)
	}
	return nil
}
