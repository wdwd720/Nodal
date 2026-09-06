package gates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// Approver steps recorded in capability_gates.approvers. The chain is the
// authoritative record of who proposed, approved, activated or resumed the
// current approval version.
const (
	StepPropose  = "PROPOSE"
	StepApprove  = "APPROVE"
	StepActivate = "ACTIVATE"
	StepResume   = "RESUME"
)

// Approver is one entry of the approval chain. EvidenceHash is the hex
// digest of the evidence the principal attested to (see Gate.EvidenceDigest),
// so a later change to the evidence is visible as a mismatch.
type Approver struct {
	UserID       string    `json:"user_id"`
	Role         string    `json:"role"`
	At           time.Time `json:"at"`
	Step         string    `json:"step"`
	EvidenceHash string    `json:"evidence_hash"`
	Note         string    `json:"note,omitempty"`
}

// Gate is a capability_gates row.
type Gate struct {
	ID              GateID
	Capability      Capability
	Environment     string
	State           GateState
	ApprovalVersion int

	LegalReviewRef      string
	ProviderContractRef string
	RiskApprovalRef     string
	SecurityApprovalRef string

	// ProposedBy is the subject that proposed the current approval version,
	// taken from the PROPOSE entry of Approvers (falling back to the
	// proposed_by_user_id column). Empty on a bootstrapped DISABLED row.
	ProposedBy     string
	Approvers      []Approver
	EvidenceHashes []string

	EffectiveAt  *time.Time
	ExpiresAt    *time.Time
	RevokedAt    *time.Time
	RevokeReason string

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// evidenceRecord is the canonical form hashed by EvidenceDigest. Field order
// is alphabetical so encoding/json output is deterministic.
type evidenceRecord struct {
	EvidenceHashes      []string `json:"evidence_hashes"`
	LegalReviewRef      string   `json:"legal_review_ref"`
	ProviderContractRef string   `json:"provider_contract_ref"`
	RiskApprovalRef     string   `json:"risk_approval_ref"`
	SecurityApprovalRef string   `json:"security_approval_ref"`
}

// EvidenceDigest is sha256 over the canonical JSON of the evidence references
// and hashes of the current approval version. It is stored on every
// transition and attested by every approver.
func (g Gate) EvidenceDigest() [32]byte {
	hashes := append([]string{}, g.EvidenceHashes...)
	sort.Strings(hashes)
	b, err := json.Marshal(evidenceRecord{
		EvidenceHashes:      hashes,
		LegalReviewRef:      g.LegalReviewRef,
		ProviderContractRef: g.ProviderContractRef,
		RiskApprovalRef:     g.RiskApprovalRef,
		SecurityApprovalRef: g.SecurityApprovalRef,
	})
	if err != nil {
		// Only strings and a string slice are marshaled; this cannot fail.
		panic("gates: marshal evidence record: " + err.Error())
	}
	return sha256.Sum256(b)
}

// EvidenceDigestHex is EvidenceDigest as lowercase hex.
func (g Gate) EvidenceDigestHex() string {
	d := g.EvidenceDigest()
	return hex.EncodeToString(d[:])
}

// MissingEvidence lists the evidence reference columns that are empty. Only
// meaningful for high-risk capabilities, which require all four.
func (g Gate) MissingEvidence() []string {
	var out []string
	if strings.TrimSpace(g.LegalReviewRef) == "" {
		out = append(out, "legal_review_ref")
	}
	if strings.TrimSpace(g.ProviderContractRef) == "" {
		out = append(out, "provider_contract_ref")
	}
	if strings.TrimSpace(g.RiskApprovalRef) == "" {
		out = append(out, "risk_approval_ref")
	}
	if strings.TrimSpace(g.SecurityApprovalRef) == "" {
		out = append(out, "security_approval_ref")
	}
	return out
}

// DistinctApprovers returns the sorted distinct subjects that approved,
// activated or resumed the current approval version (PROPOSE entries are
// excluded).
func (g Gate) DistinctApprovers() []string {
	seen := map[string]struct{}{}
	for _, a := range g.Approvers {
		if a.Step == StepPropose || a.UserID == "" {
			continue
		}
		seen[a.UserID] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// lastApprover returns the most recent non-PROPOSE chain entry, if any.
func (g Gate) lastApprover() (Approver, bool) {
	for i := len(g.Approvers) - 1; i >= 0; i-- {
		if g.Approvers[i].Step != StepPropose {
			return g.Approvers[i], true
		}
	}
	return Approver{}, false
}

// Proposal is the input to Admin.Propose: the evidence for a new approval
// version. EffectiveAt and ExpiresAt are optional (zero = unset); an unset
// EffectiveAt is filled in at activation. Reason is mandatory and is
// recorded on the transition and the audit event.
type Proposal struct {
	LegalReviewRef      string
	ProviderContractRef string
	RiskApprovalRef     string
	SecurityApprovalRef string
	EvidenceHashes      []string
	EffectiveAt         time.Time
	ExpiresAt           time.Time
	Reason              string
}

var evidenceHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// normalize trims the references, validates and de-duplicates the evidence
// hashes, and checks the time window. It fails with VALIDATION_FAILED.
func (p Proposal) normalize(c Capability, now time.Time) (Proposal, error) {
	out := p
	out.LegalReviewRef = strings.TrimSpace(p.LegalReviewRef)
	out.ProviderContractRef = strings.TrimSpace(p.ProviderContractRef)
	out.RiskApprovalRef = strings.TrimSpace(p.RiskApprovalRef)
	out.SecurityApprovalRef = strings.TrimSpace(p.SecurityApprovalRef)
	out.Reason = strings.TrimSpace(p.Reason)
	if out.Reason == "" {
		return Proposal{}, errs.New(errs.CodeValidationFailed, "reason is required").WithField("reason", "required")
	}
	if IsHighRisk(c) {
		g := Gate{
			LegalReviewRef: out.LegalReviewRef, ProviderContractRef: out.ProviderContractRef,
			RiskApprovalRef: out.RiskApprovalRef, SecurityApprovalRef: out.SecurityApprovalRef,
		}
		if missing := g.MissingEvidence(); len(missing) > 0 {
			return Proposal{}, errs.New(errs.CodeValidationFailed, "high-risk capability requires every evidence reference").
				WithField("missing", missing).WithField("capability", string(c))
		}
	}
	seen := map[string]struct{}{}
	hashes := make([]string, 0, len(p.EvidenceHashes))
	for _, h := range p.EvidenceHashes {
		h = strings.ToLower(strings.TrimSpace(h))
		if !evidenceHashRE.MatchString(h) {
			return Proposal{}, errs.New(errs.CodeValidationFailed, "evidence hash must be a 64-character hex sha256").
				WithField("evidence_hashes", "invalid")
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	out.EvidenceHashes = hashes
	if !p.ExpiresAt.IsZero() {
		if !p.ExpiresAt.After(now) {
			return Proposal{}, errs.New(errs.CodeValidationFailed, "expires_at must be in the future").WithField("expires_at", "past")
		}
		if !p.EffectiveAt.IsZero() && !p.ExpiresAt.After(p.EffectiveAt) {
			return Proposal{}, errs.New(errs.CodeValidationFailed, "expires_at must be after effective_at").WithField("expires_at", "before_effective_at")
		}
	}
	out.EffectiveAt = p.EffectiveAt.UTC()
	out.ExpiresAt = p.ExpiresAt.UTC()
	return out, nil
}

// Verdict is the result of evaluating a gate. Reason is one of the Reason*
// constants (empty when Active) so callers can match it exactly.
type Verdict struct {
	Active          bool
	Reason          string
	State           GateState
	ApprovalVersion int
	EvidenceHashes  []string
}

// Reasons a capability is not active. Each of the five conditions of
// POLICY_AUTHORITY §1 maps to at least one distinct reason.
const (
	// ReasonConfigDisabled: condition 1, deployment configuration does not
	// enable the capability for this environment.
	ReasonConfigDisabled = "capability not enabled in deployment configuration"
	// ReasonNoGateRow: condition 2, there is no persisted gate row at all.
	ReasonNoGateRow = "no gate row"
	// ReasonStateNotActive: condition 2, the row exists but is not ACTIVE.
	ReasonStateNotActive = "gate state is not ACTIVE"
	// ReasonRevoked: condition 3, revoked_at is set.
	ReasonRevoked = "gate is revoked"
	// ReasonNoEffectiveAt: condition 3, effective_at was never set.
	ReasonNoEffectiveAt = "effective_at is not set"
	// ReasonNotYetEffective: condition 3, effective_at is in the future.
	ReasonNotYetEffective = "effective_at is in the future"
	// ReasonExpired: condition 3, expires_at has passed.
	ReasonExpired = "expires_at has passed"
	// ReasonEvidenceMissing: condition 4, a required evidence reference is
	// empty for a high-risk capability.
	ReasonEvidenceMissing = "required evidence references are missing"
	// ReasonNoProposer: condition 5, the approval chain has no proposer, so
	// dual authorization cannot be established.
	ReasonNoProposer = "approval chain has no proposer"
	// ReasonProposerApproved: condition 5, an approver is the proposer.
	ReasonProposerApproved = "an approver is the proposer"
	// ReasonInsufficientApprovers: condition 5, fewer than two distinct
	// approvers.
	ReasonInsufficientApprovers = "fewer than two distinct approvers"
)

// Evaluate is the pure decision behind Checker.IsActive. g is nil when no
// row exists. configEnabled is condition 1 (deployment configuration). The
// conditions are checked in the order of POLICY_AUTHORITY §1 and the first
// failure is reported.
func Evaluate(g *Gate, configEnabled bool, now time.Time) Verdict {
	v := Verdict{}
	if g != nil {
		v.State = g.State
		v.ApprovalVersion = g.ApprovalVersion
		v.EvidenceHashes = append([]string(nil), g.EvidenceHashes...)
	}
	fail := func(reason string) Verdict {
		v.Active = false
		v.Reason = reason
		return v
	}
	if !configEnabled {
		return fail(ReasonConfigDisabled)
	}
	if g == nil {
		return fail(ReasonNoGateRow)
	}
	if g.State != StateActive {
		return fail(ReasonStateNotActive)
	}
	if g.RevokedAt != nil {
		return fail(ReasonRevoked)
	}
	if g.EffectiveAt == nil {
		return fail(ReasonNoEffectiveAt)
	}
	if now.Before(*g.EffectiveAt) {
		return fail(ReasonNotYetEffective)
	}
	if g.ExpiresAt != nil && !now.Before(*g.ExpiresAt) {
		return fail(ReasonExpired)
	}
	if IsHighRisk(g.Capability) && len(g.MissingEvidence()) > 0 {
		return fail(ReasonEvidenceMissing)
	}
	if g.ProposedBy == "" {
		return fail(ReasonNoProposer)
	}
	approvers := g.DistinctApprovers()
	for _, a := range approvers {
		if a == g.ProposedBy {
			return fail(ReasonProposerApproved)
		}
	}
	if len(approvers) < 2 {
		return fail(ReasonInsufficientApprovers)
	}
	v.Active = true
	v.Reason = ""
	return v
}
