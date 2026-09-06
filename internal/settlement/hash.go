package settlement

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
)

// planHashView is the content the plan hash covers: everything approved,
// nothing execution-time. Row identifiers (plan id, step ids, semantic keys
// derived from the plan id), Hash, Status, timestamps and step runtime
// fields (state, attempts, evidence output, errors) are excluded, so the
// hash identifies what the plan will do, not which row holds it. Step
// dependencies are rendered as sequence numbers for the same reason.
type planHashView struct {
	IntentID                  string          `json:"intent_id"`
	Version                   int32           `json:"version"`
	PlannerVersion            string          `json:"planner_version"`
	NoPlanReasonCodes         []string        `json:"no_plan_reason_codes"`
	HardConstraints           HardConstraints `json:"hard_constraints"`
	EstimatedCosts            EstimatedCosts  `json:"estimated_costs"`
	SelectedVenueListingID    string          `json:"selected_venue_listing_id"`
	SelectedSettlementAssetID string          `json:"selected_settlement_asset_id"`
	InstrumentVersion         int32           `json:"instrument_version"`
	PolicyVersions            PolicyVersions  `json:"policy_versions"`
	RiskDecisionID            string          `json:"risk_decision_id"`
	QuoteID                   string          `json:"quote_id"`
	DryRun                    bool            `json:"dry_run"`
	Steps                     []stepHashView  `json:"steps"`
}

type stepHashView struct {
	Seq                int32           `json:"seq"`
	Type               StepType        `json:"type"`
	DependsOn          []int32         `json:"depends_on"`
	RetryClass         string          `json:"retry_class"`
	TimeoutMS          int64           `json:"timeout_ms"`
	FinalityPolicy     string          `json:"finality_policy"`
	CompensationPolicy string          `json:"compensation_policy"`
	EvidenceInputs     json.RawMessage `json:"evidence_inputs"`
}

// CanonicalContent returns the canonical JSON the plan hash covers.
func CanonicalContent(p Plan) ([]byte, error) {
	seqByID := map[StepID]int32{}
	for _, s := range p.Steps {
		seqByID[s.ID] = s.Seq
	}
	view := planHashView{
		IntentID: p.IntentID, Version: p.Version, PlannerVersion: p.PlannerVersion,
		NoPlanReasonCodes: nonNil(p.NoPlanReasonCodes), HardConstraints: p.HardConstraints, EstimatedCosts: p.EstimatedCosts,
		SelectedVenueListingID: p.SelectedVenueListingID, SelectedSettlementAssetID: p.SelectedSettlementAssetID.String(),
		InstrumentVersion: p.InstrumentVersion, PolicyVersions: p.PolicyVersions, RiskDecisionID: p.RiskDecisionID, QuoteID: p.QuoteID, DryRun: p.DryRun,
		Steps: make([]stepHashView, 0, len(p.Steps)),
	}
	if view.PolicyVersions == nil {
		view.PolicyVersions = PolicyVersions{}
	}
	for _, s := range p.Steps {
		deps := make([]int32, 0, len(s.DependsOn))
		for _, d := range s.DependsOn {
			seq, ok := seqByID[d]
			if !ok {
				return nil, errs.New(errs.CodeValidationFailed, "settlement: step depends on an unknown step").WithField("step", string(s.Type))
			}
			deps = append(deps, seq)
		}
		inputs := s.EvidenceInputs
		if len(bytes.TrimSpace(inputs)) == 0 {
			inputs = json.RawMessage("{}")
		}
		view.Steps = append(view.Steps, stepHashView{
			Seq: s.Seq, Type: s.Type, DependsOn: deps, RetryClass: string(s.RetryClass), TimeoutMS: s.Timeout.Milliseconds(),
			FinalityPolicy: s.FinalityPolicy, CompensationPolicy: string(s.CompensationPolicy), EvidenceInputs: inputs,
		})
	}
	b, err := audit.CanonicalJSON(view)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "settlement: canonical plan content")
	}
	return b, nil
}

// ComputeHash returns sha256(CanonicalContent(p)).
func ComputeHash(p Plan) ([]byte, error) {
	b, err := CanonicalContent(p)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// VerifyHash recomputes the hash and reports whether it matches p.Hash.
func VerifyHash(p Plan) (bool, error) {
	h, err := ComputeHash(p)
	if err != nil {
		return false, err
	}
	return bytes.Equal(h, p.Hash), nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
