package settlement

import (
	"sort"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/risk"
)

// PlannerVersion is the version string recorded on every plan this
// implementation produces.
const PlannerVersion = "settlement-planner/1"

// Planner turns a PlannerInput into a Plan or a NO_VALID_PLAN error
// (*errs.Error with code NO_VALID_PLAN and field "reasons"), or
// VALIDATION_FAILED for malformed input. Implementations are pure.
type Planner interface {
	Plan(in PlannerInput) (Plan, error)
}

// Options tune the V1 planner. Zero fields take DefaultOptions.
type Options struct {
	// Timeouts per step type; missing types take the defaults.
	Timeouts map[StepType]time.Duration
	// MinimumExecutionWindow is the least time between Now and the
	// deadline for a plan to be feasible.
	MinimumExecutionWindow time.Duration
	// DefaultDeadline applies when the intent names none.
	DefaultDeadline time.Duration
	// DefaultQuoteMaxAge applies when neither the intent nor the risk
	// decision bounds quote age.
	DefaultQuoteMaxAge time.Duration
	// DefaultMaxPriceAge applies to prices of assets whose policy names no
	// maximum age.
	DefaultMaxPriceAge time.Duration
	// NetworkFeeHeadroomBPS widens the chain's fee estimate into the hard
	// maximum (5_000 = +50%).
	NetworkFeeHeadroomBPS money.BPS
	// MaxPriorityFeeLamports and MaxComputeUnits bound the compute budget
	// the inspector accepts.
	MaxPriorityFeeLamports money.Quantity
	MaxComputeUnits        uint32
	// MinDeltaUSD is the smallest TARGET_EXPOSURE delta worth trading.
	MinDeltaUSD money.USD
	// ReservationTTL bounds the capital reservation.
	ReservationTTL time.Duration
	// ObservePollInterval is how often OBSERVE_FINALITY polls.
	ObservePollInterval time.Duration
}

// DefaultOptions returns the documented defaults.
func DefaultOptions() Options {
	return Options{
		Timeouts:               DefaultTimeouts(),
		MinimumExecutionWindow: 20 * time.Second,
		DefaultDeadline:        10 * time.Minute,
		DefaultQuoteMaxAge:     3 * time.Second,
		DefaultMaxPriceAge:     60 * time.Second,
		NetworkFeeHeadroomBPS:  5_000,
		MaxPriorityFeeLamports: money.QuantityFromInt64(1_000_000),
		MaxComputeUnits:        1_400_000,
		MinDeltaUSD:            money.USDFromMinor(100),
		ReservationTTL:         15 * time.Minute,
		ObservePollInterval:    2 * time.Second,
	}
}

func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.Timeouts == nil {
		o.Timeouts = d.Timeouts
	} else {
		merged := DefaultTimeouts()
		for k, v := range o.Timeouts {
			merged[k] = v
		}
		o.Timeouts = merged
	}
	if o.MinimumExecutionWindow <= 0 {
		o.MinimumExecutionWindow = d.MinimumExecutionWindow
	}
	if o.DefaultDeadline <= 0 {
		o.DefaultDeadline = d.DefaultDeadline
	}
	if o.DefaultQuoteMaxAge <= 0 {
		o.DefaultQuoteMaxAge = d.DefaultQuoteMaxAge
	}
	if o.DefaultMaxPriceAge <= 0 {
		o.DefaultMaxPriceAge = d.DefaultMaxPriceAge
	}
	if o.NetworkFeeHeadroomBPS < 0 {
		o.NetworkFeeHeadroomBPS = d.NetworkFeeHeadroomBPS
	}
	if !o.MaxPriorityFeeLamports.IsPositive() {
		o.MaxPriorityFeeLamports = d.MaxPriorityFeeLamports
	}
	if o.MaxComputeUnits == 0 {
		o.MaxComputeUnits = d.MaxComputeUnits
	}
	if !o.MinDeltaUSD.IsPositive() {
		o.MinDeltaUSD = d.MinDeltaUSD
	}
	if o.ReservationTTL <= 0 {
		o.ReservationTTL = d.ReservationTTL
	}
	if o.ObservePollInterval <= 0 {
		o.ObservePollInterval = d.ObservePollInterval
	}
	return o
}

// V1Planner is the V1 spot-swap planner. It holds only its options and is
// safe for concurrent use.
type V1Planner struct {
	opts Options
}

// NewPlanner returns a V1Planner.
func NewPlanner(opts Options) *V1Planner {
	return &V1Planner{opts: opts.withDefaults()}
}

// Options returns the effective options.
func (p *V1Planner) Options() Options { return p.opts }

// candidate is one listing under consideration.
type candidate struct {
	listing  instruments.VenueListing
	venue    VenueCandidate
	chain    ChainStatus
	health   provider.Health
	excluded []string
}

// Plan implements Planner (SETTLEMENT_COMPILER §3, §4).
func (p *V1Planner) Plan(in PlannerInput) (Plan, error) {
	if err := validateInput(in); err != nil {
		return Plan{}, err
	}
	rs := newReasonSet()

	// Gates that do not depend on sizing.
	if !in.Eligibility.Eligible {
		rs.add(ReasonEligibilityFailed, strings.Join(in.Eligibility.ReasonCodes, ","))
	}
	if in.Risk.Verdict != risk.Allow {
		rs.add(ReasonRiskRejected, strings.Join(in.Risk.ReasonCodes, ","))
	}

	sz, sized, err := p.size(in, rs)
	if err != nil {
		return Plan{}, err
	}
	if !sized {
		return Plan{}, NoValidPlanError(rs.sorted(), rs.sortedDetails())
	}

	p.checkStatuses(in, sz, rs)
	p.checkAuthority(in, sz, rs)
	deadline := p.deadlineFor(in)
	if !deadline.After(in.Now) || deadline.Sub(in.Now) < p.opts.MinimumExecutionWindow {
		rs.add(ReasonDeadlineImpossible, "deadline "+deadline.Format(time.RFC3339)+" leaves less than "+p.opts.MinimumExecutionWindow.String())
	}
	constraints, err := p.constraints(in, sz, deadline)
	if err != nil {
		return Plan{}, err
	}
	if in.Risk.Constraints.MaxNotionalUSD.IsPositive() && sz.notionalUSD.Cmp(in.Risk.Constraints.MaxNotionalUSD) > 0 {
		rs.add(ReasonNotionalAboveMaximum, "notional "+sz.notionalUSD.String()+" exceeds the risk maximum "+in.Risk.Constraints.MaxNotionalUSD.String())
	}

	chosen, ok := p.selectListing(in, sz, constraints, rs)
	costs, err := p.costs(in, sz, chosen, ok, constraints, rs)
	if err != nil {
		return Plan{}, err
	}
	if !rs.empty() {
		return Plan{}, NoValidPlanError(rs.sorted(), rs.sortedDetails())
	}

	constraints.Venue = chosen.venue.Venue.Code
	constraints.Provider = chosen.venue.Provider
	constraints.Chain = chosen.listing.Network
	constraints.AllowedProgramIDs = sortedUnique(chosen.venue.ProgramIDs)
	constraints.AllowedFeeAccounts = sortedUnique(chosen.venue.FeeAccounts)
	constraints.RouteProgramIDs = []string{}
	constraints.MaxNetworkFee = costs.NetworkFee.Quantity.MulBPS(money.OneHundredPercent+p.opts.NetworkFeeHeadroomBPS, money.RoundUp)
	if nfa, ok := in.Assets[chosen.chain.NetworkFeeAsset]; ok {
		constraints.NetworkFeeAsset = assetRef(nfa)
	}

	version := in.Version
	if version < 1 {
		version = 1
	}
	plannerVersion := in.PlannerVersion
	if plannerVersion == "" {
		plannerVersion = PlannerVersion
	}
	plan := Plan{
		ID:                        NewPlanID(),
		IntentID:                  in.Intent.ID,
		Version:                   version,
		PlannerVersion:            plannerVersion,
		Status:                    PlanDraft,
		NoPlanReasonCodes:         []string{},
		HardConstraints:           constraints,
		EstimatedCosts:            costs,
		SelectedVenueListingID:    chosen.listing.ID.String(),
		SelectedSettlementAssetID: in.Instrument.SettlementAssetID,
		InstrumentVersion:         in.Instrument.MetadataVersion,
		PolicyVersions:            policyVersions(in, plannerVersion),
		DryRun:                    in.DryRun,
		CreatedAt:                 in.Now.UTC(),
	}
	steps, err := p.buildSteps(in, sz, chosen, plan)
	if err != nil {
		return Plan{}, err
	}
	plan.Steps = steps
	if err := plan.Validate(); err != nil {
		return Plan{}, errs.Wrap(err, errs.CodeInternal, "settlement: planner produced an invalid plan")
	}
	hash, err := ComputeHash(plan)
	if err != nil {
		return Plan{}, err
	}
	plan.Hash = hash
	return plan, nil
}

// validateInput checks the structural preconditions of planning.
func validateInput(in PlannerInput) error {
	problems := map[string]any{}
	if in.Intent.ID == "" {
		problems["intent.id"] = "required"
	}
	if in.Intent.AccountID == "" {
		problems["intent.account_id"] = "required"
	}
	if in.Account.ID != "" && in.Account.ID != in.Intent.AccountID {
		problems["account.id"] = "does not match the intent"
	}
	if !in.Intent.Action.Valid() {
		problems["intent.action"] = "unknown action"
	}
	if in.Intent.InstrumentID == "" || in.Instrument.ID.IsZero() || in.Instrument.ID.String() != in.Intent.InstrumentID {
		problems["instrument"] = "instrument does not match the intent"
	}
	if in.Instrument.Type != instruments.TypeSpotPair {
		problems["instrument.type"] = "V1 plans spot pairs only"
	}
	if !in.Intent.Mode.Valid() {
		problems["intent.mode"] = "unknown mode"
	}
	if in.Now.IsZero() {
		problems["now"] = "required"
	}
	if in.Risk.Stage != risk.StagePreTrade {
		problems["risk.stage"] = "must be PRE_TRADE"
	}
	if in.Account.WalletID == "" || in.Account.WalletChain == "" || in.Account.WalletAddress == "" {
		problems["account.wallet"] = "wallet id, chain and address are required"
	}
	if err := in.FeePolicy.Validate(); err != nil {
		problems["fee_policy"] = err.Error()
	}
	if err := in.FinalityPolicy.Validate(); err != nil {
		problems["finality_policy"] = err.Error()
	}
	if in.Intent.Constraints.MaxSlippageBPS < 0 || in.Intent.Constraints.MaxFeeBPS < 0 || in.Intent.Constraints.MaxPriceImpactBPS < 0 {
		problems["intent.constraints"] = "basis-point bounds must not be negative"
	}
	if in.Intent.Constraints.QuoteFreshness < 0 {
		problems["intent.constraints.quote_freshness"] = "must not be negative"
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "settlement: invalid planner input").WithFields(problems)
	}
	return nil
}

// checkStatuses applies instrument and asset status (PART 33): HALTED and
// DELISTED block everything; CLOSE_ONLY, RESTRICTED and DELISTING block
// increasing exposure. Asset statuses are folded into INSTRUMENT_STATUS.
func (p *V1Planner) checkStatuses(in PlannerInput, sz sizing, rs *reasonSet) {
	check := func(what string, st assets.Status) {
		if sz.isBuy() {
			if !st.AllowsIncreasingExposure() {
				rs.add(ReasonInstrumentStatus, what+" is "+string(st)+": buys are not allowed")
			}
			return
		}
		if !st.AllowsReducingExposure() {
			rs.add(ReasonInstrumentStatus, what+" is "+string(st)+": no transactions are allowed")
		}
	}
	check("instrument", in.Instrument.Status)
	if !in.Instrument.IsActiveAt(in.Now) {
		rs.add(ReasonInstrumentStatus, "instrument is outside its validity window")
	}
	check("base asset", sz.baseAsset.Status)
	check("quote asset", sz.quoteAsset.Status)
	if pol, ok := in.AssetPolicies[sz.baseAsset.ID]; ok && pol.Status != "" {
		check("base asset policy", pol.Status)
	}
}

// checkAuthority applies kill switches, reconciliation blocks, account
// status and buying-power restrictions for the plan's action class.
func (p *V1Planner) checkAuthority(in PlannerInput, sz sizing, rs *reasonSet) {
	action := killswitch.Action{
		Class: sz.class, AccountID: in.Intent.AccountID, AgentID: in.Intent.AgentID,
		StrategyVersionID: in.Intent.StrategyVersionID, InstrumentID: in.Intent.InstrumentID,
	}
	if sw, blocked := killswitch.Blocking(in.Health.ActiveKillSwitches, action, in.Health.KillSwitchPolicy); blocked {
		rs.add(ReasonKillSwitch, string(sw.Kind)+"("+sw.ScopeID+")")
	}
	if sz.class == killswitch.NewRisk {
		if in.Health.ReconciliationBlocksNewRisk {
			rs.add(ReasonReconciliationBlocked, "an unresolved reconciliation mismatch blocks new risk")
		}
		if !in.Account.Status.AllowsNewRisk() {
			rs.add(ReasonEligibilityFailed, "account is "+string(in.Account.Status))
		}
		for _, r := range in.BuyingPower.Restrictions {
			if !r.Blocking || r.Scope != buyingpower.ScopeAccount {
				continue
			}
			switch r.Code {
			case buyingpower.RestrictionKillSwitch:
				rs.add(ReasonKillSwitch, r.Detail)
			case buyingpower.RestrictionReconciliationRequired:
				rs.add(ReasonReconciliationBlocked, r.Detail)
			default:
				rs.add(ReasonEligibilityFailed, string(r.Code))
			}
		}
		if in.BuyingPower.AvailableNow.Cmp(sz.notionalUSD) < 0 {
			rs.add(ReasonSettlementAssetUnavailable, "available settlement value "+in.BuyingPower.AvailableNow.String()+" is below the notional "+sz.notionalUSD.String())
		}
	} else if !in.Account.Status.AllowsRiskReduction() {
		rs.add(ReasonEligibilityFailed, "account is "+string(in.Account.Status)+": risk reduction is not allowed")
	}
}

// constraints builds the hard constraints as the strictest of the intent's
// and the risk kernel's bounds (§4).
func (p *V1Planner) constraints(in PlannerInput, sz sizing, deadline time.Time) (HardConstraints, error) {
	rc := in.Risk.Constraints
	ic := in.Intent.Constraints
	quoteAge := strictestDuration(ic.QuoteFreshness, time.Duration(rc.MaxQuoteAgeMS)*time.Millisecond)
	if quoteAge <= 0 {
		quoteAge = p.opts.DefaultQuoteMaxAge
	}
	maxNotional := sz.notionalUSD
	if rc.MaxNotionalUSD.IsPositive() && rc.MaxNotionalUSD.Cmp(maxNotional) < 0 {
		maxNotional = rc.MaxNotionalUSD
	}
	hc := HardConstraints{
		AllowedProgramIDs:      []string{},
		AllowedFeeAccounts:     []string{},
		RouteProgramIDs:        []string{},
		Side:                   sz.side,
		ActionClass:            sz.class,
		InputAsset:             assetRef(sz.inputAsset),
		OutputAsset:            assetRef(sz.outputAsset),
		MaxInputQuantity:       sz.inputQuantity,
		MinOutputQuantity:      sz.minOutput,
		MaxSlippageBPS:         strictestBPS(ic.MaxSlippageBPS, rc.MaxSlippageBPS),
		MaxFeeBPS:              strictestBPS(ic.MaxFeeBPS, rc.MaxFeeBPS),
		MaxPriceImpactBPS:      strictestBPS(ic.MaxPriceImpactBPS, rc.MaxPriceImpactBPS),
		MaxPriorityFeeLamports: p.opts.MaxPriorityFeeLamports,
		MaxComputeUnits:        p.opts.MaxComputeUnits,
		QuoteMaxAgeMS:          quoteAge.Milliseconds(),
		Deadline:               deadline,
		NotionalUSD:            sz.notionalUSD,
		MaxNotionalUSD:         maxNotional,
		MinLiquidityUSD:        rc.MinLiquidityUSD,
		FinalityForLedger:      in.FinalityPolicy.Required(execution.FinalityForLedgerPosting),
		FinalityForPosition:    in.FinalityPolicy.Required(execution.FinalityForPositionProvisional),
	}
	if ic.MaxPrice != nil {
		cp := *ic.MaxPrice
		hc.MaxPrice = &cp
	}
	return hc, nil
}

// strictestBPS returns the smaller of two bounds, ignoring unset (zero)
// ones. Both unset yields zero: unbounded by both parties.
func strictestBPS(a, b money.BPS) money.BPS {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func strictestDuration(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// selectListing filters the candidate listings (ACTIVE listing, ACTIVE or
// DEGRADED venue, provider and chain health that allow new actions, no
// blocking switch, allowed venue, notional within the listing's bounds,
// wallet on the listing's network, known liquidity sufficient) and picks
// the best survivor deterministically. Every exclusion becomes a reason
// when nothing survives.
func (p *V1Planner) selectListing(in PlannerInput, sz sizing, hc HardConstraints, rs *reasonSet) (candidate, bool) {
	venues := map[string]VenueCandidate{}
	for _, v := range in.Venues {
		venues[v.Venue.ID.String()] = v
	}
	cands := make([]candidate, 0, len(in.Listings))
	for _, l := range in.Listings {
		if l.InstrumentID != in.Instrument.ID {
			continue
		}
		c := candidate{listing: l}
		v, ok := venues[l.VenueID.String()]
		if !ok {
			c.excluded = append(c.excluded, ReasonVenueDisabled+": venue "+l.VenueID.String()+" is not among the candidates")
			cands = append(cands, c)
			continue
		}
		c.venue = v
		if l.Status != instruments.VenueActive {
			c.exclude(ReasonVenueDisabled, "listing on "+v.Venue.Code+" is "+string(l.Status))
		}
		if !v.Venue.Status.AllowsNewActions() {
			c.exclude(ReasonVenueDisabled, "venue "+v.Venue.Code+" is "+string(v.Venue.Status))
		}
		if len(in.Intent.Constraints.AllowedVenues) > 0 && !containsFold(in.Intent.Constraints.AllowedVenues, v.Venue.Code) {
			c.exclude(ReasonVenueDisabled, "venue "+v.Venue.Code+" is not in the intent's allowed venues")
		}
		h, known := in.Health.Providers[v.Provider]
		c.health = h
		if !known || !h.AllowsNewActions() {
			c.exclude(ReasonProviderDegraded, "provider "+v.Provider+" health "+string(h)+" forbids new actions")
		}
		ch, known := in.Health.Chains[l.Network]
		c.chain = ch
		if !known || !ch.Health.AllowsNewActions() {
			c.exclude(ReasonProviderDegraded, "chain "+l.Network+" health "+string(ch.Health)+" forbids new actions")
		} else if !ch.EstimatedNetworkFee.IsPositive() || ch.NetworkFeeAsset.IsZero() {
			c.exclude(ReasonProviderDegraded, "chain "+l.Network+" has no network fee estimate")
		}
		if l.Network != in.Account.WalletChain {
			c.exclude(ReasonSettlementAssetUnavailable, "wallet is on "+in.Account.WalletChain+", listing settles on "+l.Network)
		}
		action := killswitch.Action{
			Class: sz.class, AccountID: in.Intent.AccountID, AgentID: in.Intent.AgentID, StrategyVersionID: in.Intent.StrategyVersionID,
			Venue: v.Venue.Code, InstrumentID: in.Intent.InstrumentID, Chain: l.Network, Provider: v.Provider,
		}
		if sw, blocked := killswitch.Blocking(in.Health.ActiveKillSwitches, action, in.Health.KillSwitchPolicy); blocked {
			c.exclude(ReasonKillSwitch, string(sw.Kind)+"("+sw.ScopeID+")")
		}
		if sz.notionalQuote.Cmp(l.MinNotionalQuote) < 0 {
			c.exclude(ReasonNotionalBelowMinimum, "notional "+sz.notionalQuote.String()+" is below the listing minimum "+l.MinNotionalQuote.String())
		}
		if l.MaxNotionalQuote != nil && l.MaxNotionalQuote.IsPositive() && sz.notionalQuote.Cmp(*l.MaxNotionalQuote) > 0 {
			c.exclude(ReasonNotionalAboveMaximum, "notional "+sz.notionalQuote.String()+" is above the listing maximum "+l.MaxNotionalQuote.String())
		}
		if liq, known := in.Liquidity[l.ID]; known {
			need := sz.notionalUSD
			if hc.MinLiquidityUSD.Cmp(need) > 0 {
				need = hc.MinLiquidityUSD
			}
			if liq.Cmp(need) < 0 {
				c.exclude(ReasonLiquidityInsufficient, "liquidity "+liq.String()+" on "+v.Venue.Code+" is below "+need.String())
			}
		}
		if v.FeeBPS < 0 {
			c.exclude(ReasonQuoteTooExpensive, "venue fee is malformed")
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		rs.add(ReasonNoEligibleListing, "no listing of the instrument was offered")
		return candidate{}, false
	}
	var survivors []candidate
	for _, c := range cands {
		if len(c.excluded) == 0 {
			survivors = append(survivors, c)
		}
	}
	if len(survivors) == 0 {
		for _, c := range cands {
			for _, ex := range c.excluded {
				code, detail, _ := strings.Cut(ex, ": ")
				rs.add(code, detail)
			}
		}
		return candidate{}, false
	}
	sort.SliceStable(survivors, func(i, j int) bool {
		a, b := survivors[i], survivors[j]
		if ra, rb := venueRank(a.venue.Venue.Status), venueRank(b.venue.Venue.Status); ra != rb {
			return ra < rb
		}
		if ra, rb := a.health.Rank(), b.health.Rank(); ra != rb {
			return ra < rb
		}
		if a.venue.FeeBPS != b.venue.FeeBPS {
			return a.venue.FeeBPS < b.venue.FeeBPS
		}
		if a.venue.Venue.Code != b.venue.Venue.Code {
			return a.venue.Venue.Code < b.venue.Venue.Code
		}
		return a.listing.ID.String() < b.listing.ID.String()
	})
	return survivors[0], true
}

func (c *candidate) exclude(code, detail string) {
	c.excluded = append(c.excluded, code+": "+detail)
}

func venueRank(s instruments.VenueStatus) int {
	switch s {
	case instruments.VenueActive:
		return 0
	case instruments.VenueDegraded:
		return 1
	}
	return 2
}

// sortedUnique returns a sorted, de-duplicated, never-nil copy.
func sortedUnique(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// costs estimates platform fee, venue fee and network fee separately
// (PART 126) and checks the fee bound.
func (p *V1Planner) costs(in PlannerInput, sz sizing, chosen candidate, ok bool, hc HardConstraints, rs *reasonSet) (EstimatedCosts, error) {
	costs := EstimatedCosts{NotionalUSD: sz.notionalUSD, ExpectedOutput: expectedOutput(sz), ReferencePrice: sz.referencePrice}
	fp := in.FeePolicy
	costs.FeePolicy = FeePolicyRef{
		Version: fp.Version, Hash: fp.Hash(), BPS: fp.PlatformFeeBPS, MinFee: fp.MinFee, MaxFee: fp.MaxFee, FeeAsset: fp.FeeAsset, Rounding: fp.Rounding.String(),
	}
	// The platform fee is charged on the quote-asset leg (the input of a buy,
	// the output of a sell); the fee asset must be that asset.
	if fp.FeeAsset != sz.quoteAsset.ID {
		return costs, errs.New(errs.CodeValidationFailed, "settlement: fee policy asset must be the instrument quote asset").
			WithField("fee_asset", fp.FeeAsset.String()).WithField("quote_asset", sz.quoteAsset.ID.String())
	}
	bd, err := fp.Compute(sz.notionalQuote)
	if err != nil {
		return costs, errs.Wrap(err, errs.CodeValidationFailed, "settlement: platform fee")
	}
	pfUSD, err := quoteUnitsToUSD(bd.PlatformFee, sz.quoteAsset.Decimals)
	if err != nil {
		return costs, moneyErr(err)
	}
	costs.PlatformFee = CostEstimate{Quantity: bd.PlatformFee, Asset: fp.FeeAsset, BPS: bd.PlatformFeeBPS, USD: pfUSD}
	totalBPS := bd.PlatformFeeBPS
	if ok {
		vf := sz.notionalQuote.MulBPS(chosen.venue.FeeBPS, money.RoundUp)
		vfUSD, err := quoteUnitsToUSD(vf, sz.quoteAsset.Decimals)
		if err != nil {
			return costs, moneyErr(err)
		}
		costs.VenueFee = CostEstimate{Quantity: vf, Asset: sz.quoteAsset.ID, BPS: chosen.venue.FeeBPS, USD: vfUSD}
		totalBPS += chosen.venue.FeeBPS
		nf := chosen.chain.EstimatedNetworkFee
		nfUSD := money.USD{}
		if nfa, known := in.Assets[chosen.chain.NetworkFeeAsset]; known {
			if price, fresh := p.usdPrice(in, nfa.ID); fresh {
				if u, err := unitsToUSDAtPrice(nf, nfa.Decimals, price); err == nil {
					nfUSD = u
				}
			}
		}
		costs.NetworkFee = CostEstimate{Quantity: nf, Asset: chosen.chain.NetworkFeeAsset, USD: nfUSD}
		total, err := pfUSD.Add(vfUSD)
		if err != nil {
			return costs, moneyErr(err)
		}
		if total, err = total.Add(nfUSD); err != nil {
			return costs, moneyErr(err)
		}
		costs.TotalUSD = total
	}
	if hc.MaxFeeBPS > 0 && totalBPS > hc.MaxFeeBPS {
		rs.add(ReasonQuoteTooExpensive, "platform + venue fee "+totalBPS.String()+" exceeds the fee bound "+hc.MaxFeeBPS.String())
	}
	return costs, nil
}

func expectedOutput(sz sizing) money.Quantity {
	if sz.isBuy() {
		return sz.baseQuantity
	}
	return sz.notionalQuote
}

func assetRef(a assets.Asset) AssetRef {
	return AssetRef{ID: a.ID, Decimals: a.Decimals, Mint: a.MintAddress, Symbol: a.Symbol}
}

// policyVersions records every policy the plan depends on.
func policyVersions(in PlannerInput, plannerVersion string) PolicyVersions {
	pv := PolicyVersions{
		PolicyKeyEligibility: in.Eligibility.PolicyVersion,
		PolicyKeyRisk:        in.Risk.PolicyVersion,
		PolicyKeyRiskHash:    in.Risk.PolicyHash,
		PolicyKeyFees:        in.FeePolicy.Version,
		PolicyKeyBuyingPower: in.BuyingPower.PolicyVersion,
		PolicyKeyFinality:    in.FinalityPolicy.Version,
		PolicyKeyPlanner:     plannerVersion,
	}
	ids := make([]string, 0, len(in.AssetPolicies))
	byID := map[string]string{}
	for id, ap := range in.AssetPolicies {
		ids = append(ids, id.String())
		byID[id.String()] = ap.PolicyVersion
	}
	sort.Strings(ids)
	for _, id := range ids {
		pv[PolicyKeyAssetPrefix+id] = byID[id]
	}
	return pv
}

// FeePolicyFromRef reconstructs the fee policy the plan was priced under.
func FeePolicyFromRef(ref FeePolicyRef) (fees.Policy, error) {
	mode, err := money.ParseRoundingMode(ref.Rounding)
	if err != nil {
		return fees.Policy{}, errs.Wrap(err, errs.CodeValidationFailed, "settlement: fee policy rounding")
	}
	p := fees.Policy{Version: ref.Version, PlatformFeeBPS: ref.BPS, MinFee: ref.MinFee, MaxFee: ref.MaxFee, FeeAsset: ref.FeeAsset, Rounding: mode}
	if err := p.Validate(); err != nil {
		return fees.Policy{}, err
	}
	return p, nil
}
