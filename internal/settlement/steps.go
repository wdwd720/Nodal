package settlement

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/risk"
)

// DefaultTimeouts returns the per-step timeouts.
func DefaultTimeouts() map[StepType]time.Duration {
	return map[StepType]time.Duration{
		StepValidateEligibility:   5 * time.Second,
		StepEvaluateRisk:          5 * time.Second,
		StepReserveCapital:        10 * time.Second,
		StepResolveVenueListing:   5 * time.Second,
		StepLocateSettlementAsset: 15 * time.Second,
		StepAcquireQuote:          10 * time.Second,
		StepValidateQuote:         5 * time.Second,
		StepFinalRiskCheck:        10 * time.Second,
		StepBuildTransaction:      15 * time.Second,
		StepInspectTransaction:    10 * time.Second,
		StepRequestSignature:      30 * time.Second,
		StepSubmit:                30 * time.Second,
		StepObserveFinality:       180 * time.Second,
		StepReconcile:             60 * time.Second,
		StepPostLedger:            30 * time.Second,
		StepUpdatePosition:        30 * time.Second,
		StepReleaseReservation:    10 * time.Second,
	}
}

// StepRetryClass is the PART 106 class of each V1 step: pure checks and
// reads are SAFE_RETRY, ledger/position/reservation/signature steps keyed by
// fill, attempt or plan are IDEMPOTENT_WRITE, SUBMIT is UNKNOWN_EFFECT_WRITE.
func StepRetryClass(t StepType) provider.RetryClass {
	switch t {
	case StepSubmit:
		return provider.UnknownEffectWrite
	case StepReserveCapital, StepRequestSignature, StepPostLedger, StepUpdatePosition, StepReleaseReservation:
		return provider.IdempotentWrite
	default:
		return provider.SafeRetry
	}
}

// StepCompensation is the compensation policy of each V1 step.
func StepCompensation(t StepType) CompensationPolicy {
	switch t {
	case StepValidateEligibility, StepEvaluateRisk, StepReleaseReservation:
		return CompensationNone
	case StepReserveCapital, StepResolveVenueListing, StepLocateSettlementAsset, StepAcquireQuote, StepValidateQuote,
		StepFinalRiskCheck, StepBuildTransaction, StepInspectTransaction, StepRequestSignature:
		return CompensationReleaseReservation
	default:
		return CompensationReconcileOnly
	}
}

// SemanticKey is the semantic idempotency key "<plan_id>:<seq>:<type>".
func SemanticKey(planID PlanID, seq int32, t StepType) string {
	return fmt.Sprintf("%s:%d:%s", planID.String(), seq, t)
}

// Typed evidence inputs per step. They make the plan self-describing: the
// executor reads what a step needs from the approved plan, never from a
// fresh copy of the input.
type (
	// EligibilityStepInput is what VALIDATE_ELIGIBILITY re-checks.
	EligibilityStepInput struct {
		Eligible      bool      `json:"eligible"`
		PolicyVersion string    `json:"policy_version"`
		DecisionHash  string    `json:"decision_hash"`
		ContextHash   string    `json:"context_hash"`
		EvaluatedAt   time.Time `json:"evaluated_at"`
	}
	// RiskStepInput is what EVALUATE_RISK re-checks.
	RiskStepInput struct {
		Verdict              risk.Verdict              `json:"verdict"`
		Stage                risk.Stage                `json:"stage"`
		ActionClass          string                    `json:"action_class"`
		PolicyVersion        string                    `json:"policy_version"`
		PolicyHash           string                    `json:"policy_hash"`
		DecisionHash         string                    `json:"decision_hash"`
		EffectiveNotionalUSD money.USD                 `json:"effective_notional_usd"`
		Constraints          risk.ResultingConstraints `json:"constraints"`
	}
	// ReserveStepInput is the reservation RESERVE_CAPITAL makes (or verifies).
	ReserveStepInput struct {
		AccountID  string         `json:"account_id"`
		AssetID    assets.AssetID `json:"asset_id"`
		Quantity   money.Quantity `json:"quantity"`
		USDMinor   int64          `json:"usd_minor"`
		EnvelopeID string         `json:"envelope_id,omitempty"`
		IntentID   string         `json:"intent_id"`
		ActorType  string         `json:"actor_type"`
		ActorID    string         `json:"actor_id"`
		TTLMS      int64          `json:"ttl_ms"`
		Reason     string         `json:"reason"`
	}
	// ListingStepInput is the selected listing.
	ListingStepInput struct {
		ListingID        string          `json:"listing_id"`
		VenueID          string          `json:"venue_id"`
		Venue            string          `json:"venue"`
		Provider         string          `json:"provider"`
		Network          string          `json:"network"`
		VenueNativeID    string          `json:"venue_native_id"`
		BaseMint         string          `json:"base_mint"`
		QuoteMint        string          `json:"quote_mint"`
		BasePrecision    uint8           `json:"base_precision"`
		QuotePrecision   uint8           `json:"quote_precision"`
		MinNotionalQuote money.Quantity  `json:"min_notional_quote"`
		MaxNotionalQuote *money.Quantity `json:"max_notional_quote,omitempty"`
		VenueFeeBPS      money.BPS       `json:"venue_fee_bps"`
	}
	// SettlementAssetStepInput is what LOCATE_SETTLEMENT_ASSET verifies on
	// chain: the wallet holds the input quantity of the input asset.
	SettlementAssetStepInput struct {
		AssetID          assets.AssetID `json:"asset_id"`
		Mint             string         `json:"mint"`
		Decimals         uint8          `json:"decimals"`
		Chain            string         `json:"chain"`
		WalletID         string         `json:"wallet_id"`
		WalletAddress    string         `json:"wallet_address"`
		RequiredQuantity money.Quantity `json:"required_quantity"`
		Basis            string         `json:"basis"`
	}
	// QuoteStepInput is the quote request ACQUIRE_QUOTE sends.
	QuoteStepInput struct {
		Request execution.QuoteRequest `json:"request"`
	}
	// ValidateQuoteStepInput carries the bounds VALIDATE_QUOTE enforces.
	ValidateQuoteStepInput struct {
		MaxInputQuantity  money.Quantity `json:"max_input_quantity"`
		MinOutputQuantity money.Quantity `json:"min_output_quantity"`
		MaxSlippageBPS    money.BPS      `json:"max_slippage_bps"`
		MaxFeeBPS         money.BPS      `json:"max_fee_bps"`
		MaxPriceImpactBPS money.BPS      `json:"max_price_impact_bps"`
		QuoteMaxAgeMS     int64          `json:"quote_max_age_ms"`
		Deadline          time.Time      `json:"deadline"`
		Provider          string         `json:"provider"`
	}
	// FinalRiskStepInput is the FINAL-stage request FINAL_RISK_CHECK makes.
	FinalRiskStepInput struct {
		Stage          risk.Stage `json:"stage"`
		PolicyVersion  string     `json:"pre_trade_policy_version"`
		MaxNotionalUSD money.USD  `json:"max_notional_usd"`
		AgentID        string     `json:"agent_id,omitempty"`
		ModelID        string     `json:"model_id,omitempty"`
	}
	// BuildStepInput bounds the transaction BUILD_TRANSACTION requests.
	BuildStepInput struct {
		WalletAddress          string         `json:"wallet_address"`
		MinOutputQuantity      money.Quantity `json:"min_output_quantity"`
		MaxSlippageBPS         money.BPS      `json:"max_slippage_bps"`
		MaxPriorityFeeLamports money.Quantity `json:"max_priority_fee_lamports"`
		MaxComputeUnits        uint32         `json:"max_compute_units"`
	}
	// InspectStepInput is the expectation set INSPECT_TRANSACTION enforces
	// (EXECUTION.md §2); the plan hash and quote id are added at run time.
	InspectStepInput struct {
		AllowedProgramIDs      []string       `json:"allowed_program_ids"`
		AllowedFeeAccounts     []string       `json:"allowed_fee_accounts"`
		WalletAddress          string         `json:"wallet_address"`
		InputMint              string         `json:"input_mint"`
		OutputMint             string         `json:"output_mint"`
		MaxInputDebit          money.Quantity `json:"max_input_debit"`
		MinOutputQuantity      money.Quantity `json:"min_output_quantity"`
		MaxSlippageBPS         money.BPS      `json:"max_slippage_bps"`
		MaxPriorityFeeLamports money.Quantity `json:"max_priority_fee_lamports"`
		MaxComputeUnits        uint32         `json:"max_compute_units"`
		MaxNetworkFee          money.Quantity `json:"max_network_fee"`
	}
	// SignStepInput names the wallet REQUEST_SIGNATURE signs with.
	SignStepInput struct {
		WalletID string `json:"wallet_id"`
		Purpose  string `json:"purpose"`
	}
	// SubmitStepInput names the provider and chain SUBMIT uses.
	SubmitStepInput struct {
		Provider string `json:"provider"`
		Chain    string `json:"chain"`
	}
	// ObserveStepInput says which finality OBSERVE_FINALITY waits for.
	ObserveStepInput struct {
		RequiredFinality execution.FinalityLevel `json:"required_finality"`
		PollIntervalMS   int64                   `json:"poll_interval_ms"`
		Provider         string                  `json:"provider"`
		Chain            string                  `json:"chain"`
	}
	// ReconcileStepInput scopes the reconciliation sweep.
	ReconcileStepInput struct {
		WalletAddress string `json:"wallet_address"`
		Provider      string `json:"provider"`
	}
	// PostLedgerStepInput carries the fee policy and asset references POST_LEDGER
	// posts with.
	PostLedgerStepInput struct {
		FeePolicy        FeePolicyRef            `json:"fee_policy"`
		RequiredFinality execution.FinalityLevel `json:"required_finality"`
		InputAsset       AssetRef                `json:"input_asset"`
		OutputAsset      AssetRef                `json:"output_asset"`
		NetworkFeeAsset  AssetRef                `json:"network_fee_asset"`
		Side             execution.Side          `json:"side"`
	}
	// UpdatePositionStepInput carries what UPDATE_POSITION needs for lots.
	UpdatePositionStepInput struct {
		Side             execution.Side          `json:"side"`
		BaseAsset        AssetRef                `json:"base_asset"`
		QuoteAsset       AssetRef                `json:"quote_asset"`
		RequiredFinality execution.FinalityLevel `json:"required_finality"`
		Venue            string                  `json:"venue"`
		WalletID         string                  `json:"wallet_id"`
		ValuationSource  string                  `json:"valuation_source"`
	}
	// ReleaseStepInput names the release reason.
	ReleaseStepInput struct {
		Reason string `json:"reason"`
	}
)

// EncodeStepInput canonicalises a step input document.
func EncodeStepInput(v any) (json.RawMessage, error) {
	b, err := audit.CanonicalJSON(v)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "settlement: encode step input")
	}
	return b, nil
}

// DecodeStepInput decodes a step's evidence inputs into T.
func DecodeStepInput[T any](s Step) (T, error) {
	var out T
	if len(s.EvidenceInputs) == 0 {
		return out, errs.New(errs.CodeInternal, "settlement: step has no evidence inputs").WithField("step", string(s.Type))
	}
	if err := json.Unmarshal(s.EvidenceInputs, &out); err != nil {
		return out, errs.Wrap(err, errs.CodeInternal, "settlement: decode step input").WithField("step", string(s.Type))
	}
	return out, nil
}

// buildSteps emits the V1 DAG: a chain where each step depends on its
// predecessor, with typed inputs, retry classes, timeouts, finality and
// compensation policies.
func (p *V1Planner) buildSteps(in PlannerInput, sz sizing, chosen candidate, plan Plan) ([]Step, error) {
	hc := plan.HardConstraints
	ledgerFinality := hc.FinalityForLedger
	positionFinality := hc.FinalityForPosition
	usdMinor := sz.notionalUSD.Minor()
	// The reservation covers the swap debit (MaxInputQuantity) plus the
	// disclosed platform fee when that fee leaves the wallet in the same
	// asset, so the fee can never exceed what was set aside.
	reserveQty := sz.inputQuantity
	if plan.EstimatedCosts.PlatformFee.Asset == sz.inputAsset.ID && plan.EstimatedCosts.PlatformFee.Quantity.IsPositive() {
		reserveQty = reserveQty.Add(plan.EstimatedCosts.PlatformFee.Quantity)
	}
	inputs := map[StepType]any{
		StepValidateEligibility: EligibilityStepInput{
			Eligible: in.Eligibility.Eligible, PolicyVersion: in.Eligibility.PolicyVersion, DecisionHash: in.Eligibility.Hash,
			ContextHash: in.Eligibility.ContextHash, EvaluatedAt: in.Eligibility.EvaluatedAt.UTC(),
		},
		StepEvaluateRisk: RiskStepInput{
			Verdict: in.Risk.Verdict, Stage: in.Risk.Stage, ActionClass: string(in.Risk.ActionClass), PolicyVersion: in.Risk.PolicyVersion,
			PolicyHash: in.Risk.PolicyHash, DecisionHash: in.Risk.Hash, EffectiveNotionalUSD: in.Risk.EffectiveNotionalUSD, Constraints: in.Risk.Constraints,
		},
		StepReserveCapital: ReserveStepInput{
			AccountID: in.Intent.AccountID, AssetID: sz.inputAsset.ID, Quantity: reserveQty, USDMinor: usdMinor,
			EnvelopeID: in.Intent.EnvelopeID, IntentID: in.Intent.ID, ActorType: string(in.Intent.ActorType), ActorID: in.Intent.ActorID,
			TTLMS: p.opts.ReservationTTL.Milliseconds(), Reason: "plan " + string(in.Intent.Action),
		},
		StepResolveVenueListing: ListingStepInput{
			ListingID: chosen.listing.ID.String(), VenueID: chosen.venue.Venue.ID.String(), Venue: chosen.venue.Venue.Code, Provider: chosen.venue.Provider,
			Network: chosen.listing.Network, VenueNativeID: chosen.listing.VenueNativeID, BaseMint: chosen.listing.BaseMint, QuoteMint: chosen.listing.QuoteMint,
			BasePrecision: chosen.listing.BasePrecision, QuotePrecision: chosen.listing.QuotePrecision,
			MinNotionalQuote: chosen.listing.MinNotionalQuote, MaxNotionalQuote: chosen.listing.MaxNotionalQuote, VenueFeeBPS: chosen.venue.FeeBPS,
		},
		StepLocateSettlementAsset: SettlementAssetStepInput{
			AssetID: sz.inputAsset.ID, Mint: sz.inputAsset.MintAddress, Decimals: sz.inputAsset.Decimals, Chain: chosen.listing.Network,
			WalletID: in.Account.WalletID, WalletAddress: in.Account.WalletAddress, RequiredQuantity: reserveQty, Basis: string(sz.settlementBasis),
		},
		StepAcquireQuote: QuoteStepInput{Request: execution.QuoteRequest{
			IntentID: in.Intent.ID, InstrumentID: in.Intent.InstrumentID, VenueListingID: chosen.listing.ID.String(), VenueNativeID: chosen.listing.VenueNativeID,
			Side: sz.side, InputAsset: sz.inputAsset.ID, InputMint: sz.inputAsset.MintAddress, InputQuantity: sz.inputQuantity,
			OutputAsset: sz.outputAsset.ID, OutputMint: sz.outputAsset.MintAddress, MinOutput: sz.minOutput, MaxSlippageBPS: hc.MaxSlippageBPS,
			WalletAddress: in.Account.WalletAddress, Deadline: hc.Deadline,
		}},
		StepValidateQuote: ValidateQuoteStepInput{
			MaxInputQuantity: hc.MaxInputQuantity, MinOutputQuantity: hc.MinOutputQuantity, MaxSlippageBPS: hc.MaxSlippageBPS, MaxFeeBPS: hc.MaxFeeBPS,
			MaxPriceImpactBPS: hc.MaxPriceImpactBPS, QuoteMaxAgeMS: hc.QuoteMaxAgeMS, Deadline: hc.Deadline, Provider: chosen.venue.Provider,
		},
		StepFinalRiskCheck: FinalRiskStepInput{Stage: risk.StageFinal, PolicyVersion: in.Risk.PolicyVersion, MaxNotionalUSD: hc.MaxNotionalUSD, AgentID: in.Intent.AgentID},
		StepBuildTransaction: BuildStepInput{
			WalletAddress: in.Account.WalletAddress, MinOutputQuantity: hc.MinOutputQuantity, MaxSlippageBPS: hc.MaxSlippageBPS,
			MaxPriorityFeeLamports: hc.MaxPriorityFeeLamports, MaxComputeUnits: hc.MaxComputeUnits,
		},
		StepInspectTransaction: InspectStepInput{
			AllowedProgramIDs: sortedUnique(chosen.venue.ProgramIDs), AllowedFeeAccounts: sortedUnique(chosen.venue.FeeAccounts),
			WalletAddress: in.Account.WalletAddress, InputMint: sz.inputAsset.MintAddress, OutputMint: sz.outputAsset.MintAddress,
			MaxInputDebit: hc.MaxInputQuantity, MinOutputQuantity: hc.MinOutputQuantity, MaxSlippageBPS: hc.MaxSlippageBPS,
			MaxPriorityFeeLamports: hc.MaxPriorityFeeLamports, MaxComputeUnits: hc.MaxComputeUnits, MaxNetworkFee: hc.MaxNetworkFee,
		},
		StepRequestSignature: SignStepInput{WalletID: in.Account.WalletID, Purpose: "swap"},
		StepSubmit:           SubmitStepInput{Provider: chosen.venue.Provider, Chain: chosen.listing.Network},
		StepObserveFinality: ObserveStepInput{
			RequiredFinality: ledgerFinality, PollIntervalMS: p.opts.ObservePollInterval.Milliseconds(), Provider: chosen.venue.Provider, Chain: chosen.listing.Network,
		},
		StepReconcile: ReconcileStepInput{WalletAddress: in.Account.WalletAddress, Provider: chosen.venue.Provider},
		StepPostLedger: PostLedgerStepInput{
			FeePolicy: plan.EstimatedCosts.FeePolicy, RequiredFinality: ledgerFinality, InputAsset: hc.InputAsset, OutputAsset: hc.OutputAsset,
			NetworkFeeAsset: hc.NetworkFeeAsset, Side: sz.side,
		},
		StepUpdatePosition: UpdatePositionStepInput{
			Side: sz.side, BaseAsset: assetRef(sz.baseAsset), QuoteAsset: assetRef(sz.quoteAsset), RequiredFinality: positionFinality,
			Venue: chosen.venue.Venue.Code, WalletID: in.Account.WalletID, ValuationSource: chosen.venue.Provider + "-fill",
		},
		StepReleaseReservation: ReleaseStepInput{Reason: "plan finished"},
	}
	finality := func(t StepType) string {
		switch t {
		case StepObserveFinality, StepPostLedger:
			return string(ledgerFinality)
		case StepUpdatePosition:
			return string(positionFinality)
		}
		return FinalityNone
	}
	seq := V1StepSequence()
	steps := make([]Step, 0, len(seq))
	var prev StepID
	for i, t := range seq {
		body, err := EncodeStepInput(inputs[t])
		if err != nil {
			return nil, err
		}
		timeout := p.opts.Timeouts[t]
		if timeout <= 0 {
			timeout = DefaultTimeouts()[t]
		}
		s := Step{
			ID: NewStepID(), PlanID: plan.ID, Seq: int32(i), Type: t, State: StepPending,
			SemanticIdempotencyKey: SemanticKey(plan.ID, int32(i), t), RetryClass: StepRetryClass(t), Timeout: timeout,
			FinalityPolicy: finality(t), CompensationPolicy: StepCompensation(t), EvidenceInputs: body,
		}
		if i > 0 {
			s.DependsOn = []StepID{prev}
		}
		steps = append(steps, s)
		prev = s.ID
	}
	return steps, nil
}
