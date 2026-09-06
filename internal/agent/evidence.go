package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// EvidenceRef is one hashed artifact backing a promotion (PART 69). The hash
// is what makes it evidence rather than a claim: a later reader can re-derive
// it and see whether the artifact still says what the promotion said it did.
type EvidenceRef struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Hash string `json:"hash"` // lowercase hex of sha256
}

// Evidence kinds. The promotion gates of AGENT_RUNTIME.md §2 name exactly
// these; a gate is never inferred from the UI and never from a default.
const (
	EvidenceCompileSuccess        = "COMPILE_SUCCESS"
	EvidenceFinancialPropertyTest = "FINANCIAL_PROPERTY_TESTS"
	EvidenceBacktest              = "BACKTEST"
	EvidenceEffectSet             = "EFFECT_SET"
	EvidenceSecurityFindings      = "SECURITY_FINDINGS"
	EvidenceRiskPolicy            = "RISK_POLICY"
	EvidenceDataDependencies      = "DATA_DEPENDENCIES"
	EvidencePerformanceSnapshot   = "PERFORMANCE_SNAPSHOT"
	EvidencePolicyViolations      = "POLICY_VIOLATIONS"
	EvidenceEnvelope              = "ENVELOPE"
	EvidenceToolBudgetReport      = "TOOL_BUDGET_REPORT"
	EvidenceRealFills             = "REAL_FILLS"
	EvidenceReconciliation        = "RECONCILIATION"
	EvidenceSubmissionUnknown     = "SUBMISSION_UNKNOWN"
	EvidenceUnexpectedSubmissions = "UNEXPECTED_SUBMISSIONS"
	EvidencePredictionOutcomes    = "PREDICTION_OUTCOMES"
	EvidenceCalibrationSnapshot   = "CALIBRATION_SNAPSHOT"
	EvidenceLiveTradingGate       = "LIVE_AGENT_TRADING_GATE"
	EvidenceOperatorAttestation   = "OPERATOR_ATTESTATION"
)

// gateEvidence is the required evidence per promotion target. Missing
// evidence is VALIDATION_FAILED; there is no "sufficiently close".
var gateEvidence = map[Stage][]string{
	StageShadow: {
		EvidenceCompileSuccess,
		EvidenceFinancialPropertyTest,
		EvidenceBacktest,
		EvidenceEffectSet,
		EvidenceSecurityFindings,
		EvidenceRiskPolicy,
		EvidenceDataDependencies,
	},
	StageCanary: {
		EvidenceCompileSuccess,
		EvidenceFinancialPropertyTest,
		EvidenceBacktest,
		EvidenceEffectSet,
		EvidenceSecurityFindings,
		EvidenceRiskPolicy,
		EvidenceDataDependencies,
		EvidencePerformanceSnapshot,
		EvidencePolicyViolations,
		EvidenceEnvelope,
		EvidenceToolBudgetReport,
	},
	StageLimited: {
		EvidencePerformanceSnapshot,
		EvidenceRealFills,
		EvidenceReconciliation,
		EvidenceSubmissionUnknown,
		EvidenceUnexpectedSubmissions,
		EvidencePolicyViolations,
		EvidencePredictionOutcomes,
		EvidenceRiskPolicy,
		EvidenceEnvelope,
	},
	StageLive: {
		EvidencePerformanceSnapshot,
		EvidenceRealFills,
		EvidenceReconciliation,
		EvidenceSubmissionUnknown,
		EvidenceUnexpectedSubmissions,
		EvidencePolicyViolations,
		EvidencePredictionOutcomes,
		EvidenceRiskPolicy,
		EvidenceEnvelope,
		EvidenceCalibrationSnapshot,
		EvidenceLiveTradingGate,
		EvidenceOperatorAttestation,
	},
}

// RequiredEvidence returns the evidence kinds a promotion into stage needs.
// A stage that is not a ladder promotion requires none.
func RequiredEvidence(stage Stage) []string {
	return append([]string(nil), gateEvidence[stage]...)
}

// RequiresApproval reports whether a promotion into stage needs a
// dual-controlled admin_actions row. The agents and transitions CHECKs
// enforce the same rule.
func RequiresApproval(stage Stage) bool { return stage.RealCapital() }

// RequiresEvidence reports whether a promotion into stage needs hashed
// evidence at all.
func RequiresEvidence(stage Stage) bool { return len(gateEvidence[stage]) > 0 }

// PromotionEvidence is everything a promotion must carry (PART 69). It is
// recorded verbatim on the immutable agent_lifecycle_transitions row.
type PromotionEvidence struct {
	StrategyVersionID     string
	IRHash                []byte
	DatasetRef            string
	DatasetHash           []byte
	RiskPolicyVersion     string
	RiskPolicyHash        []byte
	PerformanceSnapshotID string
	BacktestID            string
	CalibrationSnapshotID string
	ErrorRateBPS          money.BPS
	OperationalHealth     json.RawMessage
	// ApprovalID is the admin_actions row of kind AGENT_PROMOTE. It is
	// required for CANARY, LIMITED and LIVE and must be dual-controlled.
	ApprovalID string
	Evidence   []EvidenceRef
	Reason     string
	// Mode is the mode the agent runs in at the target stage. Empty means
	// DefaultModeForStage.
	Mode          Mode
	EnvelopeID    string
	CorrelationID string
	BuildVersion  string
}

// MaxErrorRateBPS mirrors the transitions CHECK (0..10000).
const MaxErrorRateBPS = money.BPS(10_000)

// Validate reports every reason the evidence does not satisfy the gate into
// stage. It never partially accepts: an error lists all missing kinds.
func (e PromotionEvidence) Validate(to Stage) error {
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	if len(e.Reason) < MinReasonLength {
		fail("reason", "must be at least 8 characters")
	}
	if e.ErrorRateBPS < 0 || e.ErrorRateBPS > MaxErrorRateBPS {
		fail("error_rate_bps", "must be between 0 and 10000")
	}
	if RequiresEvidence(to) {
		if e.StrategyVersionID == "" {
			fail("strategy_version_id", "required for a ladder promotion")
		}
		if len(e.IRHash) != sha256.Size {
			fail("ir_hash", "must be a 32-byte sha256")
		}
		if e.RiskPolicyVersion == "" {
			fail("risk_policy_version", "required for a ladder promotion")
		}
		if len(e.RiskPolicyHash) != sha256.Size {
			fail("risk_policy_hash", "must be a 32-byte sha256")
		}
	}
	if RequiresApproval(to) && e.ApprovalID == "" {
		fail("approval_id", "promotion into "+to.String()+" requires a dual-controlled approval")
	}
	if to.RealCapital() && e.EnvelopeID == "" {
		fail("envelope_id", "promotion into "+to.String()+" requires a bound capital envelope")
	}
	if e.Mode != "" && !ModeAllowed(to, e.Mode) {
		fail("mode", "mode "+e.Mode.String()+" is not permitted at stage "+to.String())
	}

	have := map[string]EvidenceRef{}
	for i, ref := range e.Evidence {
		field := "evidence[" + strconv.Itoa(i) + "]"
		switch {
		case ref.Kind == "":
			fail(field+".kind", "required")
		case ref.Ref == "":
			fail(field+".ref", "required")
		}
		if _, err := hex.DecodeString(ref.Hash); err != nil || len(ref.Hash) != 2*sha256.Size {
			fail(field+".hash", "must be lowercase hex of a sha256")
		}
		if strings.ToLower(ref.Hash) != ref.Hash {
			fail(field+".hash", "must be lowercase hex of a sha256")
		}
		have[ref.Kind] = ref
	}
	var missing []string
	for _, want := range gateEvidence[to] {
		if _, ok := have[want]; !ok {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		fail("evidence", "missing: "+strings.Join(missing, ", "))
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.Newf(errs.CodeValidationFailed, "agent: promotion into %s lacks required evidence", to).WithFields(fields)
}

// Hash is the aggregate evidence hash stored on the transition. It covers
// every reference, sorted by (kind, ref), plus the strategy version, IR hash
// and risk policy, so a transition row cannot be re-pointed at different
// evidence later.
func (e PromotionEvidence) Hash() []byte {
	refs := append([]EvidenceRef(nil), e.Evidence...)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].Ref < refs[j].Ref
	})
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(e.StrategyVersionID)
	write(hex.EncodeToString(e.IRHash))
	write(e.RiskPolicyVersion)
	write(hex.EncodeToString(e.RiskPolicyHash))
	write(e.DatasetRef)
	write(hex.EncodeToString(e.DatasetHash))
	write(e.PerformanceSnapshotID)
	write(e.BacktestID)
	write(e.CalibrationSnapshotID)
	write(strconv.FormatInt(int64(e.ErrorRateBPS), 10))
	write(e.ApprovalID)
	for _, r := range refs {
		write(r.Kind)
		write(r.Ref)
		write(strings.ToLower(r.Hash))
	}
	return h.Sum(nil)
}

// EvidenceJSON renders the references for the transition row.
func (e PromotionEvidence) EvidenceJSON() (json.RawMessage, error) {
	refs := e.Evidence
	if refs == nil {
		refs = []EvidenceRef{}
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "agent: encode promotion evidence")
	}
	return b, nil
}

// HashOf is a helper for callers assembling evidence: the lowercase hex
// sha256 of the bytes an artifact consists of.
func HashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
