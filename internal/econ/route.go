package econ

import (
	"time"

	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// RouteRequest is what Route needs beyond the intent itself: what the intent
// is about, in the compiler's vocabulary, and the account facts the legal
// router keys on.
type RouteRequest struct {
	Intent  Intent
	Subject settlement.Subject
	// AgentAuthority is the level an agent intent acted under. Ignored for a
	// human intent.
	AgentAuthority agentauthority.Level
	Jurisdiction   string
	Verification   valuedomain.VerificationLevel
}

// Route compiles an economic intent through the settlement compiler, so an
// advanced intent faces the same agent-authority ladder, legal router and
// domain checks as every other financial intent. A refusal is a Route with
// Permitted=false and every reason; an error means the request was malformed.
func Route(req RouteRequest, router *legalrouter.Router, now time.Time) (settlement.Route, error) {
	in := req.Intent
	action, ok := settlement.AdvancedActionFor(string(in.Kind))
	if !ok {
		return settlement.Route{}, errs.Newf(errs.CodeValidationFailed, "econ: intent kind %s has no compiler action", in.Kind)
	}
	side, _ := settlement.SideOf(action)
	fi := settlement.FinancialIntent{
		IntentID:       in.ID.String(),
		AccountID:      in.AccountID.String(),
		ActorType:      in.ActorType,
		ActorID:        in.ActorID,
		IdempotencyKey: in.IdempotencyKey,
		CorrelationID:  in.CorrelationID,
		RequestedAt:    now,
		AgentID:        in.AgentID,
		AgentAuthority: req.AgentAuthority,
		CapitalDomain:  valuedomain.Simulated,
		ActionType:     action,
		Subject:        req.Subject,
		Side:           side,
		Quantity:       in.Quantity,
		NotionalUSD:    in.NotionalUSD,
		Jurisdiction:   req.Jurisdiction,
		Verification:   req.Verification,
	}
	if fi.Verification == "" {
		fi.Verification = valuedomain.VerificationNone
	}
	return settlement.Compile(settlement.CompilerInput{Intent: fi, Router: router, Now: now})
}
