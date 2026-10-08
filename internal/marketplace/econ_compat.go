package marketplace

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/errs"
)

// checkEconTransitionsImmediately makes the transition bindings of economic
// intents and effects check each status change when it happens instead of at
// COMMIT.
//
// A paper-follow run moves one intent through several statuses in one
// transaction (RECEIVED -> ROUTED -> RISK_APPROVED, and execution may take it
// further). On this branch the deferred AU001 binding (migration 00731)
// remembers one edge per entity per transaction, so the earlier changes
// would fail at COMMIT. IntentStore.Transition and EffectStore write the
// transition row before the UPDATE, so an immediate check sees exactly the
// edge it licenses. The binding is not weakened: every change is still
// checked against a transition row naming both endpoints.
//
// INTEGRATOR: delete this helper and its call at merge; main's migration
// 00733 makes the deferred binding accumulate edges.
func checkEconTransitionsImmediately(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS economic_intents_require_transition, economic_effects_require_transition IMMEDIATE`); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "marketplace: check econ transitions immediately")
	}
	return nil
}
