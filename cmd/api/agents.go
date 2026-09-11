package main

import (
	"context"

	"github.com/nodal/controlplane/internal/agents"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/gates"
)

// agentCapabilityChecker answers agents.CapabilityChecker from the same gate
// checker every other capability decision in this binary consults.
//
// It is here rather than in internal/agents because internal/gates is on the
// list of packages an agent tree may never import, and internal/agents sits
// directly beside one. The composition root is where a boundary like that is
// meant to be crossed.
type agentCapabilityChecker struct {
	checker *gates.Checker
	// q is the fallback querier for callers not already inside a transaction.
	q db.Querier
}

// Active reports whether the capability is ACTIVE, whether that came from a
// SANDBOX row on a sandbox tier, and the gate's own reason when it is not.
//
// An unknown capability name is reported inactive with a reason that says so,
// rather than passed to the checker: a typo must not read as "no gate row",
// which is the same answer a real, unapproved capability gives.
func (r agentCapabilityChecker) Active(ctx context.Context, q db.Querier, capability string) (bool, bool, string, error) {
	cap := gates.Capability(capability)
	if !cap.Valid() {
		return false, false, "unknown capability " + capability, nil
	}
	querier := q
	if querier == nil {
		querier = r.q
	}
	verdict, err := r.checker.IsActive(ctx, querier, cap)
	if err != nil {
		return false, false, "", err
	}
	return verdict.Active, verdict.Sandbox, verdict.Reason, nil
}

// agentRuntimeDeployment declares which agent worker processes this binary's
// deployment runs.
//
// Both are false, and that is a statement about the deployment rather than a
// placeholder. `cmd/agent-worker` wires no evaluator and its EmitterFor answers
// UNSUPPORTED; `cmd/execution-worker` is not deployed on this tier; and the
// runtime's lifecycle and emitter constructors have no production caller at
// all, which is the premise F-65's deferral rests on and which test/security
// watches. Nothing here names those constructors, deliberately: the watcher
// reads text, and a mention would read as a caller.
//
// The API reports NOT_DEPLOYED because of this value, so the day a worker is
// actually deployed the honest answer changes here, in one place, next to the
// wiring that would deploy it.
func agentRuntimeDeployment() agents.Deployment {
	return agents.Deployment{EvaluatorDeployed: false, ExecutorDeployed: false}
}
