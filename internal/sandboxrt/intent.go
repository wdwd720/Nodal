package sandboxrt

import (
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
	"github.com/nodal/controlplane/internal/security"
)

// OriginSandbox is the origin recorded in the terms of every intent a
// sandboxed strategy proposed.
const OriginSandbox = "SANDBOX"

// IntentContext is everything an accepted proposal needs to become an
// economic intent, all of it supplied by the host -- none of it by the
// strategy.
type IntentContext struct {
	AccountID  accounts.AccountID
	Mode       gates.Mode
	Capability gates.Capability
	// ActorID names the service turning proposals into intents. The actor
	// type is always SERVICE: a sandboxed strategy is never an actor.
	ActorID string
	// StrategyRef is "<listing id>@<version>".
	StrategyRef    string
	RunID          RunID
	RunDigest      []byte
	ArtifactDigest []byte
	// KeyPrefix scopes the intents' idempotency keys to one logical run, so
	// replaying that run submits nothing twice.
	KeyPrefix     string
	CorrelationID string
}

// IntentTerms are the terms of a sandbox-originated intent. Notional is the
// quantity at ReferencePrice, the price the host read itself; LimitPrice is the
// strategy's instruction to the venue and never values anything.
type IntentTerms struct {
	Origin         string `json:"origin"`
	Side           Side   `json:"side"`
	LimitPrice     string `json:"limit_price"`
	Notional       string `json:"notional"`
	ReferencePrice string `json:"reference_price"`
	SandboxRunID   string `json:"sandbox_run_id"`
	RunDigest      string `json:"run_digest"`
	ArtifactDigest string `json:"artifact_digest"`
	ProposalSeq    int    `json:"proposal_seq"`
}

func (c IntentContext) validate() error {
	var problems []string
	if c.AccountID.IsZero() {
		problems = append(problems, "account required")
	}
	if strings.TrimSpace(c.ActorID) == "" {
		problems = append(problems, "service actor id required")
	}
	if strings.TrimSpace(c.StrategyRef) == "" || !strings.Contains(c.StrategyRef, "@") {
		problems = append(problems, "strategy ref <listing>@<version> required")
	}
	if c.RunID.IsZero() || len(c.RunDigest) != 32 || len(c.ArtifactDigest) != 32 {
		problems = append(problems, "run id, run digest and artifact digest required")
	}
	if strings.TrimSpace(c.KeyPrefix) == "" || len(c.KeyPrefix) > 200 {
		problems = append(problems, "key prefix must be 1..200 bytes")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "sandboxrt: invalid intent context").WithField("problems", problems)
	}
	return nil
}

// Intent converts an accepted proposal into an econ.Intent. The intent's
// actor is the SERVICE named in the context, its idempotency key is
// <KeyPrefix>:<proposal seq>, and its terms record the sandbox origin: the
// run, the run digest, the artifact digest and the proposal it came from.
// The quantity is the only amount: the notional travels in the terms and in
// the risk input, never as a second amount the router would have to choose
// between.
func (a AcceptedProposal) Intent(c IntentContext) (econ.Intent, error) {
	if err := c.validate(); err != nil {
		return econ.Intent{}, err
	}
	if ref, err := nodalsdk.ParseDecimal(a.ReferencePrice); err != nil || ref.Sign() <= 0 {
		// Only FilterAt makes an AcceptedProposal, always at a reference
		// price; one without is not something a run accepted.
		return econ.Intent{}, errs.New(errs.CodeValidationFailed, "sandboxrt: an accepted proposal names the reference price of its notional")
	}
	terms, err := audit.CanonicalJSON(IntentTerms{
		Origin: OriginSandbox, Side: a.Side, LimitPrice: a.LimitPrice, Notional: a.Notional, ReferencePrice: a.ReferencePrice,
		SandboxRunID: c.RunID.String(), RunDigest: hex.EncodeToString(c.RunDigest),
		ArtifactDigest: hex.EncodeToString(c.ArtifactDigest), ProposalSeq: a.Seq,
	})
	if err != nil {
		return econ.Intent{}, errs.Wrap(err, errs.CodeInternal, "sandboxrt: encode intent terms")
	}
	q := a.Quantity
	in := econ.Intent{
		AccountID:      c.AccountID,
		Kind:           a.Kind,
		Mode:           c.Mode,
		Capability:     c.Capability,
		ActorType:      security.ActorService,
		ActorID:        c.ActorID,
		StrategyRef:    c.StrategyRef,
		InstrumentID:   a.InstrumentID,
		Quantity:       &q,
		Terms:          json.RawMessage(terms),
		IdempotencyKey: c.KeyPrefix + ":" + strconv.Itoa(a.Seq),
		CorrelationID:  c.CorrelationID,
	}
	if err := in.Validate(); err != nil {
		return econ.Intent{}, err
	}
	return in, nil
}

// NotionalUSD renders the exact notional in cents, rounded up: a limit check
// must never see less than the true amount.
func (a AcceptedProposal) NotionalUSD() (money.USD, error) {
	d, err := nodalsdk.ParseDecimal(a.Notional)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeInternal, "sandboxrt: notional unreadable")
	}
	cents, err := d.Rescale(2, nodalsdk.RoundCeil)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "sandboxrt: notional out of range")
	}
	u := cents.Unscaled()
	if !u.IsInt64() {
		return money.USD{}, errs.New(errs.CodeOverflow, "sandboxrt: notional does not fit in USD minor units")
	}
	return money.USDFromMinor(u.Int64()), nil
}

// RiskIntent is the Risk V2 view of a sandbox-originated intent:
// SandboxOriginated is always true, which makes the sandbox section (and,
// through StrategyRef, the strategy section) required -- the kernel rejects
// the intent if either is missing.
func RiskIntent(in econ.Intent, a AcceptedProposal) (risk.AdvancedIntent, error) {
	notional, err := a.NotionalUSD()
	if err != nil {
		return risk.AdvancedIntent{}, err
	}
	return risk.AdvancedIntent{
		ID: in.ID.String(), Kind: string(in.Kind), AccountID: in.AccountID.String(), ActorType: string(in.ActorType),
		AgentID: in.AgentID, StrategyRef: in.StrategyRef, Capability: string(in.Capability), Mode: string(in.Mode),
		InstrumentID: in.InstrumentID, InstrumentKind: a.InstrumentKind, NotionalUSD: notional,
		RiskIncreasing: in.Kind.RiskIncreasing(), SandboxOriginated: true,
	}, nil
}

// SandboxSection is the Risk V2 code-sandbox section for an intent proposed
// by a run: whether the artifact the run executed was verified against its
// digest, and how many violations the run produced. Any violation makes the
// kernel reject every intent of the run.
func SandboxSection(artifactVerified bool, violations int) *risk.SandboxSnapshot {
	return &risk.SandboxSnapshot{ArtifactVerified: artifactVerified, Violations: violations}
}
