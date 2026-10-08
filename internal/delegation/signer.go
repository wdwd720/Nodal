package delegation

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// KeyDerivation names how the provider turns its master seed into a
// delegated wallet key; it is recorded with every registered public key.
const KeyDerivation = "hmac-sha256/delegated-signer/v1"

const signerDomain = "nodal-sandbox/delegated-signer/v1\x00"

// ChainFees answers a chain's configured fee per transaction (the devnet
// host's configuration).
type ChainFees func(chain devnet.ChainID) (uint64, bool)

// SignerConfig configures the DelegatedSigner.
type SignerConfig struct {
	Environment config.Environment
	Clock       clock.Clock
	Policy      SigningPolicy
	Fees        ChainFees
}

// DelegatedSigner is the signing provider of delegated authority. It holds
// the master seed every delegated wallet key is derived from -- inside a
// closure, so no field, getter, formatter or log can reach it -- and signs
// only after it has re-read, from the database, everything that licenses the
// signature: the capability (validated again: status, expiry, content hash,
// account, agent, scope and limits as recorded), the approval, the plan and
// the exact compiled message, which it inspects field by field and then
// holds to the signing policy. It records exactly one decision per swap or
// withdrawal before it signs, so a second attempt is refused by the
// database, and it never signs a REJECTED decision.
type DelegatedSigner struct {
	derive func(account accounts.AccountID, chain devnet.ChainID) devnet.Keypair
	env    config.Environment
	clk    clock.Clock
	policy SigningPolicy
	fees   ChainFees
}

// NewDelegatedSigner returns the provider for masterSeed. The caller must
// not keep the seed.
func NewDelegatedSigner(masterSeed [32]byte, cfg SignerConfig) (*DelegatedSigner, error) {
	if cfg.Clock == nil || cfg.Fees == nil || !cfg.Environment.IsValid() {
		return nil, errs.New(errs.CodeInternal, "delegation: the signer needs a clock, the chains' fees and a valid environment")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	if masterSeed == ([32]byte{}) {
		return nil, errs.New(errs.CodeValidationFailed, "delegation: the master seed must not be zero")
	}
	seed := masterSeed
	return &DelegatedSigner{
		derive: func(account accounts.AccountID, chain devnet.ChainID) devnet.Keypair {
			mac := hmac.New(sha256.New, seed[:])
			_, _ = mac.Write([]byte(signerDomain + account.String() + "\x00" + string(chain)))
			var s [devnet.SeedSize]byte
			copy(s[:], mac.Sum(nil))
			return devnet.NewKeypairFromSeed(s)
		},
		env: cfg.Environment, clk: cfg.Clock, policy: cfg.Policy, fees: cfg.Fees,
	}, nil
}

// PublicKey returns the account's delegated wallet address on chain.
func (s *DelegatedSigner) PublicKey(account accounts.AccountID, chain devnet.ChainID) devnet.PublicKey {
	return s.derive(account, chain).PublicKey()
}

// Policy returns the signing policy the provider applies.
func (s *DelegatedSigner) Policy() SigningPolicy { return s.policy }

// EnsureKey registers the account's delegated wallet public key on chain,
// once. A registered key that is not the one this provider derives (a
// different master seed) is refused: the provider will not sign for a wallet
// it cannot prove is its own.
func (s *DelegatedSigner) EnsureKey(ctx context.Context, tx pgx.Tx, account accounts.AccountID, chain devnet.ChainID) (devnet.PublicKey, error) {
	if !contains(Chains(), chain) {
		return devnet.PublicKey{}, errs.New(errs.CodeValidationFailed, "delegation: unknown chain "+string(chain))
	}
	want := s.PublicKey(account, chain)
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_signer_keys (id, account_id, chain_id, public_key, derivation)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (account_id, chain_id) DO NOTHING`,
		id.New[id.Any](), account, chain, want.String(), KeyDerivation); err != nil {
		return devnet.PublicKey{}, dbErr("register signer key", err)
	}
	got, ok, err := registeredKey(ctx, tx, account, chain)
	if err != nil {
		return devnet.PublicKey{}, err
	}
	if !ok || got != want {
		return devnet.PublicKey{}, errs.New(errs.CodeConflict, "delegation: the registered signer key is not the provider's").
			WithField("reason", ReasonSignerKeyMismatch)
	}
	return want, nil
}

// SignRequest is a bounded signing request: an identifier and the exact
// bytes to sign. Nothing in it carries authority.
type SignRequest struct {
	Kind      SubjectKind
	SubjectID string
	// Message is the canonical unsigned message.
	Message []byte
	// MessageSHA256 is the caller's hash of Message; compared, never trusted.
	MessageSHA256 [32]byte
}

// SigningDecision is what the provider decided and, when it approved, the
// wallet's signature.
type SigningDecision struct {
	ID              string
	Verdict         Verdict
	ReasonCodes     []string
	Signature       devnet.Signature
	SignerPublicKey devnet.PublicKey
	PlanHash        [32]byte
	MessageSHA256   [32]byte
	DecidedAt       time.Time
}

// signingState is everything the provider re-reads about a subject.
type signingState struct {
	kind         SubjectKind
	subject      string
	account      accounts.AccountID
	capabilityID CapabilityID
	chain        devnet.ChainID
	wallet       devnet.PublicKey
	planHash     [32]byte
	message      []byte
	signable     bool
	needApproval bool
	approval     *approvalRow
	// subjectCodes are refusals about the subject itself that the
	// capability check cannot see (a swap whose label is not what it does).
	subjectCodes []string
	use          Use
	inspect      func(m devnet.Message, c Capability) []string
	policy       func(now time.Time) []string
}

// Sign decides and, if every check passes, signs. A refusal is a decision
// with Verdict REJECTED and no signature, recorded like an approval; an
// error is a malformed request, a subject that cannot be decided on, or an
// infrastructure failure, and records nothing.
func (s *DelegatedSigner) Sign(ctx context.Context, tx pgx.Tx, req SignRequest) (SigningDecision, error) {
	if len(req.Message) == 0 || len(req.Message) > devnet.MaxTransactionSize {
		return SigningDecision{}, errs.New(errs.CodeValidationFailed, "delegation: a signing request carries the message to sign")
	}
	if sha256.Sum256(req.Message) != req.MessageSHA256 {
		return SigningDecision{}, errs.New(errs.CodeValidationFailed, "delegation: the message hash does not match the message")
	}
	if _, err := id.ParseAny(req.SubjectID); err != nil {
		return SigningDecision{}, errs.New(errs.CodeValidationFailed, "delegation: the subject must be a canonical uuid")
	}
	var st signingState
	var err error
	switch req.Kind {
	case SubjectSwap:
		st, err = s.swapState(ctx, tx, req.SubjectID)
	case SubjectWithdrawal:
		st, err = s.withdrawalState(ctx, tx, req.SubjectID)
	default:
		return SigningDecision{}, errs.New(errs.CodeValidationFailed, "delegation: unknown signing subject")
	}
	if err != nil {
		return SigningDecision{}, err
	}
	if st.message == nil {
		return SigningDecision{}, errs.New(errs.CodeInvalidStateTransition, "delegation: nothing has been compiled for this subject")
	}
	if done, err := decisionExists(ctx, tx, st.kind, st.subject); err != nil {
		return SigningDecision{}, err
	} else if done {
		return SigningDecision{}, errs.New(errs.CodeConflict, "delegation: this subject has already been decided; the boundary decides once")
	}
	capability, err := getCapability(ctx, tx, st.capabilityID, false)
	if err != nil {
		return SigningDecision{}, err
	}
	now := dbTime(s.clk.Now())
	var codes []string
	// 1. The capability, validated again from its row.
	st.use.Now, st.use.Environment = now, s.env
	codes = append(codes, capability.Check(st.use).Codes...)
	// 2. The subject: signable, what it says it is, approved when it must be,
	// on the plan it carries.
	if !st.signable {
		codes = append(codes, ReasonSubjectNotSignable)
	}
	codes = append(codes, st.subjectCodes...)
	if st.needApproval {
		switch {
		case st.approval == nil || st.approval.Decision != DecisionApprove:
			codes = append(codes, ReasonApprovalRequired)
		case st.approval.PlanHash != st.planHash:
			codes = append(codes, ReasonApprovalPlanMismatch)
		}
	}
	// 3. The key: the wallet the plan pays from is the one this provider
	// derives for the account and chain, and the registered one.
	derived := s.PublicKey(st.account, st.chain)
	if reg, ok, err := registeredKey(ctx, tx, st.account, st.chain); err != nil {
		return SigningDecision{}, err
	} else if !ok || reg != derived || st.wallet != derived {
		codes = append(codes, ReasonSignerKeyMismatch)
	}
	// 4. The bytes: exactly the compiled message, inspected field by field
	// against the persisted plan.
	if !bytes.Equal(req.Message, st.message) {
		codes = append(codes, ReasonMessageNotPersisted)
	}
	if msg, err := decodeMessage(req.Message); err != nil {
		codes = append(codes, InspectMalformed)
	} else {
		codes = append(codes, st.inspect(msg, capability)...)
	}
	// 5. The signing policy.
	codes = append(codes, st.policy(now)...)
	codes = sortedCodes(codes)

	d := SigningDecision{ID: id.New[id.Any]().String(), Verdict: VerdictApproved, ReasonCodes: codes,
		SignerPublicKey: derived, PlanHash: st.planHash, MessageSHA256: sha256.Sum256(st.message), DecidedAt: now}
	if len(codes) > 0 {
		d.Verdict = VerdictRejected
	} else {
		d.ReasonCodes = []string{}
	}
	var swapID, withdrawalID any
	if st.kind == SubjectSwap {
		swapID = st.subject
	} else {
		withdrawalID = st.subject
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_signing_decisions
		(id, swap_id, withdrawal_id, account_id, capability_id, capability_hash, plan_hash, message_sha256, signer_public_key,
		 verdict, reason_codes, policy_version, policy_hash, inspector_version, decided_at)
		VALUES ($1::uuid,$2::uuid,$3::uuid,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		d.ID, swapID, withdrawalID, st.account, st.capabilityID, capability.ContentHash[:], st.planHash[:],
		d.MessageSHA256[:], derived.String(), d.Verdict, d.ReasonCodes, s.policy.Version, s.policy.Hash(), InspectorVersion,
		d.DecidedAt); err != nil {
		return SigningDecision{}, dbErr("record signing decision", err)
	}
	if d.Verdict != VerdictApproved {
		return d, nil
	}
	sig, err := s.derive(st.account, st.chain).Sign(req.Message)
	if err != nil {
		return SigningDecision{}, errs.Wrap(err, errs.CodeSigningRejected, "delegation: the delegated wallet could not sign")
	}
	d.Signature = sig
	return d, nil
}

// swapState re-reads a swap and everything its signature depends on.
func (s *DelegatedSigner) swapState(ctx context.Context, tx pgx.Tx, subject string) (signingState, error) {
	sid, err := ParseSwapID(subject)
	if err != nil {
		return signingState{}, errs.New(errs.CodeValidationFailed, "delegation: not a swap id")
	}
	sw, err := getSwap(ctx, tx, sid, true)
	if err != nil {
		return signingState{}, err
	}
	venue, err := getVenue(ctx, tx, sw.VenueID)
	if err != nil {
		return signingState{}, err
	}
	payAsset, recvAsset, err := swapAssets(ctx, tx, sw)
	if err != nil {
		return signingState{}, err
	}
	classes, ok := swapClasses(payAsset, recvAsset)
	if !ok {
		classes = nil
	}
	// The capability is measured against what the swap does, derived from
	// its assets again here, never against its label.
	action := SwapAction(payAsset, recvAsset)
	st := signingState{
		kind: SubjectSwap, subject: sw.ID.String(), account: sw.AccountID, capabilityID: sw.CapabilityID, chain: sw.Chain,
		wallet: sw.Wallet, planHash: sw.PlanHash, message: sw.Message, signable: sw.Status.Signable(),
		needApproval: sw.RequiresApproval,
	}
	if action != sw.Action {
		st.subjectCodes = append(st.subjectCodes, ReasonActionMismatch)
	}
	if a, found, err := swapApproval(ctx, tx, sw.ID); err != nil {
		return signingState{}, err
	} else if found {
		st.approval = &a
	}
	strategy, err := capabilityStrategy(ctx, tx, sw.CapabilityID)
	if err != nil {
		return signingState{}, err
	}
	dayUsed, totalUsed, err := usageExcluding(ctx, tx, sw.CapabilityID, sw.UsageID, sw.RequestedAt)
	if err != nil {
		return signingState{}, err
	}
	st.use = Use{AccountID: sw.AccountID, AgentID: sw.AgentID, StrategyID: strategy, Action: action, Classes: classes,
		Chain: sw.Chain, Destination: venue.Address, Notional: sw.NotionalUSD, DayUsed: dayUsed, TotalUsed: totalUsed, Mode: sw.Mode}
	st.inspect = func(m devnet.Message, c Capability) []string {
		fee, _ := s.fees(sw.Chain)
		return InspectSwapMessage(SwapExpectation{Plan: sw.Plan, PlanHash: sw.PlanHash, Blockhash: sw.RecentBlockhash,
			Wallet: s.PublicKey(sw.AccountID, sw.Chain), AllowedChains: c.Terms.ChainAllowlist,
			AllowedDestinations: c.Terms.DestinationAllowlist, ChainFee: fee, MaxFee: s.policy.MaxFeePerTx}, m)
	}
	st.policy = func(now time.Time) []string {
		var codes []string
		parts, err := sw.Plan.parts()
		if err != nil {
			return []string{InspectInvalidPlan}
		}
		codes = append(codes, s.feeCodes(sw.Chain, parts.fee)...)
		if parts.slippage > s.policy.MaxSlippageBPS {
			codes = append(codes, ReasonSlippageAbovePolicy)
		}
		if !now.Before(sw.QuoteExpiresAt) {
			codes = append(codes, ReasonQuoteExpired)
		}
		return codes
	}
	return st, nil
}

// withdrawalState re-reads a withdrawal and everything its signature
// depends on.
func (s *DelegatedSigner) withdrawalState(ctx context.Context, tx pgx.Tx, subject string) (signingState, error) {
	wid, err := ParseWithdrawalID(subject)
	if err != nil {
		return signingState{}, errs.New(errs.CodeValidationFailed, "delegation: not a withdrawal id")
	}
	w, err := getWithdrawal(ctx, tx, wid, true)
	if err != nil {
		return signingState{}, err
	}
	dest, err := getDestination(ctx, tx, w.DestinationID, false)
	if err != nil {
		return signingState{}, err
	}
	asset, err := loadAsset(ctx, tx, w.AssetID)
	if err != nil {
		return signingState{}, err
	}
	var classes []InstrumentClass
	if c, ok := ClassifyAsset(asset); ok {
		classes = []InstrumentClass{c}
	}
	st := signingState{
		kind: SubjectWithdrawal, subject: w.ID.String(), account: w.AccountID, capabilityID: w.CapabilityID, chain: w.Chain,
		wallet: w.Wallet, planHash: w.PlanHash, message: w.Message, signable: w.Status == WithdrawalApproved,
		needApproval: true,
	}
	if a, found, err := withdrawalApproval(ctx, tx, w.ID); err != nil {
		return signingState{}, err
	} else if found {
		st.approval = &a
	}
	strategy, err := capabilityStrategy(ctx, tx, w.CapabilityID)
	if err != nil {
		return signingState{}, err
	}
	dayUsed, totalUsed, err := usageExcluding(ctx, tx, w.CapabilityID, w.UsageID, w.ProposedAt)
	if err != nil {
		return signingState{}, err
	}
	st.use = Use{AccountID: w.AccountID, AgentID: w.AgentID, StrategyID: strategy, Action: ActionProposeWithdrawal,
		Classes: classes, Chain: w.Chain, Destination: w.DestinationAddress, Notional: w.NotionalUSD, DayUsed: dayUsed,
		TotalUsed: totalUsed, Mode: w.Mode}
	st.inspect = func(m devnet.Message, c Capability) []string {
		customer := dest.Address
		if dest.Status != DestinationActive || dest.AccountID != w.AccountID || dest.Chain != w.Chain {
			customer = devnet.PublicKey{}
		}
		fee, _ := s.fees(w.Chain)
		return InspectWithdrawalMessage(WithdrawalExpectation{Plan: w.Plan, PlanHash: w.PlanHash, Blockhash: w.RecentBlockhash,
			Wallet: s.PublicKey(w.AccountID, w.Chain), AllowedChains: c.Terms.ChainAllowlist,
			AllowedDestinations: c.Terms.DestinationAllowlist, CustomerDestination: customer, ChainFee: fee,
			MaxFee: s.policy.MaxFeePerTx}, m)
	}
	st.policy = func(now time.Time) []string {
		var codes []string
		parts, err := w.Plan.parts()
		if err != nil {
			return []string{InspectInvalidPlan}
		}
		codes = append(codes, s.feeCodes(w.Chain, parts.fee)...)
		if now.Before(w.ExecutableAt) {
			codes = append(codes, ReasonCoolingPeriod)
		}
		if dest.Status != DestinationActive {
			codes = append(codes, ReasonDestinationRemoved)
		}
		return codes
	}
	return st, nil
}

// feeCodes holds the planned fee to the chain's configuration and the policy.
func (s *DelegatedSigner) feeCodes(chain devnet.ChainID, planned uint64) []string {
	var codes []string
	if !contains(s.policy.AllowedChains, chain) {
		codes = append(codes, InspectChainMismatch)
	}
	if fee, ok := s.fees(chain); !ok || fee != planned {
		codes = append(codes, ReasonChainFeeChanged)
	}
	if planned > s.policy.MaxFeePerTx {
		codes = append(codes, ReasonFeeAbovePolicy)
	}
	return codes
}

// String returns "DelegatedSigner".
func (s *DelegatedSigner) String() string { return "DelegatedSigner" }

// Format prints String for every verb, so no formatting of the signer can
// reach its key material.
func (s *DelegatedSigner) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

// VenueSigner holds a sandbox venue's settlement key. It co-signs one thing --
// a swap exactly as its persisted plan describes it, in which the venue pays
// the planned receive amount and receives the planned pay amount and pays no
// fee -- and only after its own inspection. Its key lives only in a closure.
type VenueSigner struct {
	public devnet.PublicKey
	sign   func(msg []byte) (devnet.Signature, error)
}

// NewVenueSigner derives the venue's key from seed. The caller must not keep
// the seed.
func NewVenueSigner(seed [devnet.SeedSize]byte) *VenueSigner {
	kp := devnet.NewKeypairFromSeed(seed)
	return &VenueSigner{public: kp.PublicKey(), sign: kp.Sign}
}

// PublicKey returns the venue's settlement address.
func (v *VenueSigner) PublicKey() devnet.PublicKey { return v.public }

// Venue co-signing refusals.
const (
	VenueNotTheVenue    = "VENUE_NOT_THIS_VENUE"
	VenueNotThePlan     = "VENUE_NOT_THE_PLANNED_TRANSACTION"
	VenuePaysFee        = "VENUE_WOULD_PAY_THE_FEE"
	VenuePlanTampered   = "VENUE_PLAN_HASH_MISMATCH"
	VenueLegsNotPlanned = "VENUE_LEGS_NOT_PLANNED"
)

// CoSign inspects msg against the plan and, only if it is exactly the plan's
// transaction at blockhash, returns the venue's signature over it.
func (v *VenueSigner) CoSign(plan SwapPlan, planHash [32]byte, blockhash devnet.Hash, msg devnet.Message) (devnet.Signature, error) {
	var codes []string
	if h, err := plan.Hash(); err != nil || h != planHash {
		codes = append(codes, VenuePlanTampered)
	}
	parts, err := plan.parts()
	if err != nil {
		return devnet.Signature{}, venueRefused(append(codes, VenueNotThePlan))
	}
	if parts.venue != v.public {
		codes = append(codes, VenueNotTheVenue)
	}
	if msg.FeePayer == v.public {
		codes = append(codes, VenuePaysFee)
	}
	if len(msg.Instructions) == 3 {
		pay, ok0 := msg.Instructions[0].(devnet.TokenTransfer)
		recv, ok1 := msg.Instructions[1].(devnet.TokenTransfer)
		if !ok0 || !ok1 || pay.ToOwner != v.public || pay.Amount != parts.amountIn || pay.Mint != parts.payMint ||
			recv.FromOwner != v.public || recv.Amount != parts.amountOut || recv.Mint != parts.receiveMint {
			codes = append(codes, VenueLegsNotPlanned)
		}
	} else {
		codes = append(codes, VenueLegsNotPlanned)
	}
	want, err := plan.Message(blockhash)
	if err != nil || !sameMessage(want, msg) {
		codes = append(codes, VenueNotThePlan)
	}
	if len(codes) > 0 {
		return devnet.Signature{}, venueRefused(codes)
	}
	b, err := msg.Bytes()
	if err != nil {
		return devnet.Signature{}, venueRefused([]string{VenueNotThePlan})
	}
	sig, err := v.sign(b)
	if err != nil {
		return devnet.Signature{}, errs.Wrap(err, errs.CodeSigningRejected, "delegation: the venue could not sign")
	}
	return sig, nil
}

func venueRefused(codes []string) error {
	return errs.New(errs.CodeSigningRejected, "delegation: the venue refused to co-sign").WithField("reason_codes", sortedCodes(codes))
}

// String returns "VenueSigner(<public key>)".
func (v *VenueSigner) String() string { return "VenueSigner(" + v.public.String() + ")" }

// Format prints String for every verb.
func (v *VenueSigner) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, v.String()) }

// assemble joins a message and its signatures, in RequiredSigners order,
// into the wire bytes.
func assemble(msg devnet.Message, sigs ...devnet.Signature) ([]byte, error) {
	raw, err := devnet.Transaction{Message: msg, Signatures: sigs}.Encode()
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "delegation: encode the signed transaction")
	}
	return raw, nil
}

func formatUint(v uint64) string { return strconv.FormatUint(v, 10) }
