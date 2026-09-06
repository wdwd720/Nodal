package signing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/signing/inspect"
	"github.com/nodal/controlplane/internal/wallet"
)

// Linkage reason codes: the persisted rows do not form an approved,
// signable chain. They are recorded on the REJECTED decision like inspector
// reason codes.
const (
	ReasonAttemptLinkage        = "ATTEMPT_LINKAGE_MISMATCH"
	ReasonAttemptState          = "ATTEMPT_STATE_NOT_SIGNABLE"
	ReasonTxHashMismatch        = "TX_HASH_MISMATCH"
	ReasonPlanNotApproved       = "PLAN_NOT_APPROVED"
	ReasonPlanDryRun            = "PLAN_DRY_RUN"
	ReasonPlanLinkage           = "PLAN_LINKAGE_MISMATCH"
	ReasonIntentState           = "INTENT_STATE_NOT_SIGNABLE"
	ReasonRiskNotAllowed        = "RISK_NOT_ALLOWED"
	ReasonRiskLinkage           = "RISK_LINKAGE_MISMATCH"
	ReasonQuoteExpired          = "QUOTE_EXPIRED"
	ReasonQuoteLinkage          = "QUOTE_LINKAGE_MISMATCH"
	ReasonReservationInactive   = "RESERVATION_INACTIVE"
	ReasonReservationMismatch   = "RESERVATION_MISMATCH"
	ReasonWalletInactive        = "WALLET_INACTIVE"
	ReasonDelegationNotVerified = "DELEGATION_NOT_VERIFIED"
	ReasonWalletAccountMismatch = "WALLET_ACCOUNT_MISMATCH"
	ReasonAssetStatus           = "ASSET_STATUS"
	ReasonChainMismatch         = "CHAIN_MISMATCH"
	ReasonUnsupportedAsset      = "UNSUPPORTED_ASSET"
	ReasonCallerExpectation     = "CALLER_EXPECTATION_MISMATCH"
)

// signableAttemptStatuses are the execution_attempts.status values in which
// a signature may be requested.
var signableAttemptStatuses = map[string]bool{"BUILT": true, "INSPECTED": true, "SIGNING_REQUESTED": true}

// loaded is everything the boundary re-read from Postgres for one Sign.
type loaded struct {
	attempt     attemptRow
	plan        planRow
	intent      intentRow
	quote       quoteRow
	reservation reservationRow
	risk        riskRow
	wallet      wallet.Wallet
	inputAsset  assets.Asset
	outputAsset assets.Asset
}

// chainData are the facts fetched from injected sources before inspection.
type chainData struct {
	currentHeight uint64
	lookupTables  map[string][]string
	simulation    *inspect.SimulationResult
}

// hardConstraints is the subset of execution_plans.hard_constraints the
// boundary reads (settlement.HardConstraints json tags; the two allow-list
// keys are the per-plan additions hook and may be absent).
type hardConstraints struct {
	MaxInputQuantity       money.Quantity `json:"max_input_quantity"`
	MinOutputQuantity      money.Quantity `json:"min_output_quantity"`
	MaxSlippageBPS         money.BPS      `json:"max_slippage_bps"`
	MaxPriorityFeeLamports money.Quantity `json:"max_priority_fee_lamports"`
	MaxComputeUnits        uint32         `json:"max_compute_units"`
	AllowedProgramIDs      []string       `json:"allowed_program_ids"`
	AllowedFeeAccounts     []string       `json:"allowed_fee_accounts"`
	RoutePrograms          []string       `json:"route_program_ids"`
}

// riskConstraints is the subset of risk_decisions.resulting_constraints the
// boundary reads (risk.persistedConstraints → constraints.max_slippage_bps).
type riskConstraints struct {
	Constraints struct {
		MaxSlippageBPS money.BPS `json:"max_slippage_bps"`
	} `json:"constraints"`
}

// linkageProblem is one reason the rows cannot be signed for.
type linkageProblem struct {
	code   string
	detail string
}

// validateLinkage checks that every row references the others as the
// request claims and is in a signable state. Problems are returned, not
// errors: they become a REJECTED decision.
func validateLinkage(l *loaded, req Request, txHash []byte, now time.Time) []linkageProblem {
	var ps []linkageProblem
	add := func(code, detail string) { ps = append(ps, linkageProblem{code: code, detail: detail}) }

	a := l.attempt
	if a.PlanID != req.PlanID {
		add(ReasonAttemptLinkage, "attempt belongs to plan "+a.PlanID)
	}
	if a.WalletID != req.WalletID {
		add(ReasonAttemptLinkage, "attempt belongs to wallet "+a.WalletID)
	}
	if !signableAttemptStatuses[a.Status] {
		add(ReasonAttemptState, "attempt status "+a.Status)
	}
	if len(a.UnsignedTxHash) > 0 && !bytes.Equal(a.UnsignedTxHash, txHash) {
		add(ReasonTxHashMismatch, "attempt unsigned_tx_hash differs from the submitted bytes")
	}
	if !bytes.Equal(req.ExpectedTxHash, txHash) {
		add(ReasonTxHashMismatch, "caller expected_tx_hash differs from sha256(unsigned_tx)")
	}

	p := l.plan
	if p.Status != "APPROVED" || p.ApprovedAt == nil {
		add(ReasonPlanNotApproved, "plan status "+p.Status)
	}
	if p.DryRun {
		add(ReasonPlanDryRun, "dry-run plans never reach signing")
	}
	if p.IntentID != req.IntentID {
		add(ReasonPlanLinkage, "plan belongs to intent "+p.IntentID)
	}
	if p.RiskDecisionID == "" || p.RiskDecisionID != req.RiskDecisionID {
		add(ReasonPlanLinkage, "plan risk decision is "+orNone(p.RiskDecisionID))
	}
	if p.QuoteID == "" {
		add(ReasonPlanLinkage, "plan has no quote")
	} else if a.QuoteID != p.QuoteID {
		add(ReasonQuoteLinkage, "attempt quote "+a.QuoteID+" differs from plan quote "+p.QuoteID)
	}
	if len(p.PlanHash) != 32 {
		add(ReasonPlanLinkage, "plan hash is not 32 bytes")
	}
	if len(req.ClaimedPlanHash) > 0 && !bytes.Equal(req.ClaimedPlanHash, p.PlanHash) {
		add(ReasonCallerExpectation, "caller plan hash differs from the approved plan hash")
	}

	i := l.intent
	switch i.Status {
	case "PLANNED", "EXECUTING":
	default:
		add(ReasonIntentState, "intent status "+i.Status)
	}
	if i.PlanID != "" && i.PlanID != p.ID {
		add(ReasonPlanLinkage, "intent points at plan "+i.PlanID)
	}
	if i.ReservationID == "" {
		add(ReasonReservationMismatch, "intent has no reservation")
	}

	rk := l.risk
	if rk.Decision != "ALLOW" {
		add(ReasonRiskNotAllowed, "risk decision "+rk.Decision)
	}
	if rk.IntentID != "" && rk.IntentID != req.IntentID {
		add(ReasonRiskLinkage, "risk decision belongs to intent "+rk.IntentID)
	}

	q := l.quote
	if q.IntentID != "" && q.IntentID != req.IntentID {
		add(ReasonQuoteLinkage, "quote belongs to intent "+q.IntentID)
	}
	if !q.ExpiresAt.After(now) {
		add(ReasonQuoteExpired, "quote expired at "+q.ExpiresAt.Format(time.RFC3339))
	}
	if !q.MinimumOutput.IsPositive() {
		add(ReasonQuoteLinkage, "quote minimum output is not positive")
	}

	r := l.reservation
	if r.Status != "ACTIVE" {
		add(ReasonReservationInactive, "reservation status "+r.Status)
	}
	if r.LockedByOrderID == "" && !r.ExpiresAt.After(now) {
		add(ReasonReservationInactive, "reservation expired at "+r.ExpiresAt.Format(time.RFC3339))
	}
	if r.LockedByOrderID != "" && r.LockedByOrderID != a.OrderID {
		add(ReasonReservationMismatch, "reservation is locked by another order")
	}
	if r.IntentID != "" && r.IntentID != req.IntentID {
		add(ReasonReservationMismatch, "reservation belongs to intent "+r.IntentID)
	}
	if r.AccountID != i.AccountID {
		add(ReasonReservationMismatch, "reservation belongs to another account")
	}
	if r.AssetID != q.InputAssetID {
		add(ReasonReservationMismatch, "reservation asset differs from the quote input asset")
	}
	if !r.Quantity.Sub(r.ConsumedQuantity).IsPositive() {
		add(ReasonReservationInactive, "reservation has no remaining quantity")
	}

	w := l.wallet
	if w.Status != wallet.StatusActive {
		add(ReasonWalletInactive, "wallet status "+string(w.Status))
	}
	if !w.DelegationVerified() {
		add(ReasonDelegationNotVerified, "wallet delegation has not been verified")
	}
	if w.AccountID.String() != i.AccountID {
		add(ReasonWalletAccountMismatch, "wallet belongs to another account")
	}
	if w.Kind != wallet.KindEmbeddedDelegated {
		add(ReasonWalletInactive, "wallet kind "+string(w.Kind))
	}

	for _, as := range []struct {
		a    assets.Asset
		role string
	}{{l.inputAsset, "input"}, {l.outputAsset, "output"}} {
		if as.a.Chain != w.Chain {
			add(ReasonChainMismatch, as.role+" asset is on "+as.a.Chain+", wallet on "+w.Chain)
		}
		if as.a.Kind == assets.KindFiat {
			add(ReasonUnsupportedAsset, as.role+" asset is fiat")
		}
	}
	if !l.inputAsset.Status.AllowsReducingExposure() {
		add(ReasonAssetStatus, "input asset status "+string(l.inputAsset.Status))
	}
	if !l.outputAsset.Status.AllowsIncreasingExposure() {
		add(ReasonAssetStatus, "output asset status "+string(l.outputAsset.Status))
	}
	sort.Slice(ps, func(x, y int) bool {
		if ps[x].code != ps[y].code {
			return ps[x].code < ps[y].code
		}
		return ps[x].detail < ps[y].detail
	})
	return ps
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// assetMint maps an asset row to (mint, token program, native?).
func assetMint(a assets.Asset, policy Policy) (mint, program string, native bool, err error) {
	switch a.Kind {
	case assets.KindNative:
		return inspect.WrappedSOLMint, inspect.TokenProgram, true, nil
	case assets.KindSPLToken:
		return a.MintAddress, inspect.TokenProgram, false, nil
	case assets.KindSPLToken2022:
		return a.MintAddress, inspect.Token2022Program, false, nil
	}
	return "", "", false, fmt.Errorf("asset kind %q cannot be swapped", a.Kind)
}

// buildExpectations rebuilds inspect.Expectations from persisted rows,
// policy and chain data. Nothing here comes from the caller except
// LookupTables (when no source is configured) and the claimed identity.
func buildExpectations(l *loaded, chain chainData, policy Policy, req Request) (inspect.Expectations, error) {
	var hc hardConstraints
	if len(l.plan.HardConstraints) > 0 {
		if err := json.Unmarshal(l.plan.HardConstraints, &hc); err != nil {
			return inspect.Expectations{}, fmt.Errorf("plan hard_constraints: %w", err)
		}
	}
	var rc riskConstraints
	if len(l.risk.ResultingConstraints) > 0 {
		if err := json.Unmarshal(l.risk.ResultingConstraints, &rc); err != nil {
			return inspect.Expectations{}, fmt.Errorf("risk resulting_constraints: %w", err)
		}
	}
	inMint, inProg, inNative, err := assetMint(l.inputAsset, policy)
	if err != nil {
		return inspect.Expectations{}, err
	}
	outMint, outProg, outNative, err := assetMint(l.outputAsset, policy)
	if err != nil {
		return inspect.Expectations{}, err
	}

	// Max input debit: the strictest of reservation remaining, plan bound and quote input.
	maxIn := l.reservation.Quantity.Sub(l.reservation.ConsumedQuantity)
	if hc.MaxInputQuantity.IsPositive() {
		maxIn = maxIn.Min(hc.MaxInputQuantity)
	}
	if l.quote.InputQuantity.IsPositive() {
		maxIn = maxIn.Min(l.quote.InputQuantity)
	}
	// Min output: the strictest (largest) of quote minimum and plan minimum.
	minOut := l.quote.MinimumOutput
	if hc.MinOutputQuantity.IsPositive() {
		minOut = minOut.Max(hc.MinOutputQuantity)
	}
	// Slippage: the strictest of quote, plan and risk.
	maxSlip := money.BPS(l.quote.SlippageBPS)
	if hc.MaxSlippageBPS > 0 && hc.MaxSlippageBPS < maxSlip {
		maxSlip = hc.MaxSlippageBPS
	}
	if rc.Constraints.MaxSlippageBPS > 0 && rc.Constraints.MaxSlippageBPS < maxSlip {
		maxSlip = rc.Constraints.MaxSlippageBPS
	}
	maxFee := policy.MaxPriorityFeeLamports
	if hc.MaxPriorityFeeLamports.IsPositive() {
		if v, err := hc.MaxPriorityFeeLamports.Int64(); err == nil && v > 0 && uint64(v) < maxFee {
			maxFee = uint64(v)
		}
	}
	maxCU := policy.MaxComputeUnits
	if hc.MaxComputeUnits > 0 && hc.MaxComputeUnits < maxCU {
		maxCU = hc.MaxComputeUnits
	}

	// Token-2022: only when the asset is explicitly enabled and carries no extensions.
	token2022 := false
	for _, a := range []assets.Asset{l.inputAsset, l.outputAsset} {
		if a.Kind == assets.KindSPLToken2022 {
			if policy.Token2022EnabledAssets[a.ID.String()] && !a.HasUnsupportedExtensions() {
				token2022 = true
			} else {
				token2022 = false
				break
			}
		}
	}
	allowed := dedupe(append(append(append([]string(nil), policy.AllowedPrograms...), policy.RoutePrograms...), hc.AllowedProgramIDs...))
	if token2022 {
		allowed = dedupe(append(allowed, inspect.Token2022Program))
	}
	routes := policy.RoutePrograms
	if len(hc.RoutePrograms) > 0 {
		routes = hc.RoutePrograms
	}
	var lastValid uint64
	if l.attempt.LastValidBlockHeight != nil && *l.attempt.LastValidBlockHeight > 0 {
		lastValid = uint64(*l.attempt.LastValidBlockHeight)
	}
	claimedHash := req.ClaimedPlanHash
	if len(claimedHash) == 0 {
		claimedHash = l.plan.PlanHash
	}
	return inspect.Expectations{
		Wallet:                 l.wallet.Address,
		FeePayer:               l.wallet.Address,
		InputMint:              inMint,
		InputTokenProgram:      inProg,
		InputIsNative:          inNative,
		MaxInputDebit:          maxIn,
		OutputMint:             outMint,
		OutputTokenProgram:     outProg,
		OutputIsNative:         outNative,
		MinOutput:              minOut,
		AllowedPrograms:        allowed,
		RoutePrograms:          routes,
		Token2022Allowed:       token2022,
		AllowedFeeAccounts:     hc.AllowedFeeAccounts,
		MaxPriorityFeeLamports: maxFee,
		MaxComputeUnits:        maxCU,
		RecentBlockhash:        l.attempt.RecentBlockhash,
		LastValidBlockHeight:   lastValid,
		CurrentBlockHeight:     chain.currentHeight,
		BlockHeightMargin:      policy.BlockHeightMargin,
		PlanHash:               append([]byte(nil), l.plan.PlanHash...),
		QuoteID:                l.plan.QuoteID,
		Claimed:                inspect.PlanIdentity{PlanHash: claimedHash, QuoteID: l.attempt.QuoteID},
		MaxSlippageBPS:         maxSlip,
		Simulation:             chain.simulation,
		RequireSimulation:      policy.RequireSimulation,
		LookupTables:           chain.lookupTables,
	}, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
