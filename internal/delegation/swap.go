package delegation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Swap refusal reasons (the intent's rejection code and the error's
// "reason" field).
const (
	RefusalRouteDenied        = "DELEGATION_ROUTE_DENIED"
	RefusalKillSwitch         = "KILL_SWITCH_ACTIVE"
	RefusalPriceUnavailable   = "DELEGATION_PRICE_UNAVAILABLE"
	RefusalVenueInventory     = "DELEGATION_VENUE_INVENTORY"
	RefusalVenueUnavailable   = "DELEGATION_VENUE_UNAVAILABLE"
	RefusalInsufficientFunds  = "DELEGATION_INSUFFICIENT_FUNDS"
	RefusalRiskRejected       = "RISK_REJECTED"
	RefusalSigningRejected    = "SIGNING_REJECTED"
	RefusalDeclinedByOwner    = "DECLINED_BY_OWNER"
	RefusalGateClosed         = "CAPABILITY_NOT_APPROVED"
	RefusalSubmissionRefused  = "SUBMISSION_REFUSED"
	RefusalSubmissionExpired  = "SUBMISSION_EXPIRED"
	RefusalChainFailed        = "CHAIN_FAILED"
	RefusalReconcileMismatch  = "CHAIN_MISMATCH"
	RefusalAuthorityRefused   = "DELEGATION_AUTHORITY_REFUSED"
	effectKindDelegatedSwap   = "DELEGATED_SWAP"
	effectKindSandboxWithdraw = "SANDBOX_WITHDRAWAL"
)

// SwapRequest is an agent's request to exchange AmountIn of the pay asset for
// the receive asset at a registered venue, under one of its capabilities.
// Nonce is the agent's request nonce: one request per (capability, nonce),
// for ever.
type SwapRequest struct {
	CapabilityID   CapabilityID
	AccountID      accounts.AccountID
	Nonce          uint64
	Action         ActionClass
	VenueID        VenueID
	PayAssetID     assets.AssetID
	ReceiveAssetID assets.AssetID
	AmountIn       money.Quantity
	MaxSlippageBPS money.BPS
	Jurisdiction   string
	CorrelationID  string
}

func (r SwapRequest) validate() error {
	var problems []string
	if r.CapabilityID.IsZero() || r.AccountID.IsZero() || r.VenueID.IsZero() || r.PayAssetID.IsZero() || r.ReceiveAssetID.IsZero() {
		problems = append(problems, "capability, account, venue and both assets are required")
	}
	if r.Nonce == 0 {
		problems = append(problems, "the request nonce must be positive")
	}
	if !r.Action.IsSwap() {
		problems = append(problems, "the action must be ACQUIRE_EXPOSURE or REDUCE_EXPOSURE")
	}
	if r.PayAssetID == r.ReceiveAssetID {
		problems = append(problems, "the pay and receive assets must differ")
	}
	if !r.AmountIn.IsPositive() {
		problems = append(problems, "the amount in must be positive")
	} else if _, err := devnet.AmountOf(r.AmountIn); err != nil {
		problems = append(problems, "the amount in must fit a devnet amount (u64)")
	}
	if r.MaxSlippageBPS < 0 || r.MaxSlippageBPS > money.OneHundredPercent {
		problems = append(problems, "the slippage bound must be 0..10000 basis points")
	}
	if len(r.CorrelationID) > 200 || len(r.Jurisdiction) > 32 {
		problems = append(problems, "correlation id and jurisdiction are bounded")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "delegation: invalid swap request").WithField("problems", problems)
	}
	return nil
}

// SwapTicket is what RequestSwap recorded. A Refusal is already recorded in
// the transaction (intent REJECTED, risk decision persisted) and the caller
// commits it.
type SwapTicket struct {
	Swap    Swap
	Replay  bool
	Refusal error
}

type swapTerms struct {
	Action         string `json:"action"`
	VenueID        string `json:"venue_id"`
	ReceiveAssetID string `json:"receive_asset_id"`
	RequestNonce   string `json:"request_nonce"`
	MaxSlippageBPS string `json:"max_slippage_bps"`
}

// RequestSwap admits an agent's swap: authenticate the agent and its
// capability, record the intent, route it, price it at the venue, measure
// the capability against it, run Risk V2, reserve the pay asset, record the
// usage and the plan. Nothing is signed. In the caller's transaction.
func (s *Service) RequestSwap(ctx context.Context, tx pgx.Tx, req SwapRequest) (SwapTicket, error) {
	if err := req.validate(); err != nil {
		return SwapTicket{}, err
	}
	_, agent, err := s.agentFor(ctx, tx, req.AccountID)
	if err != nil {
		return SwapTicket{}, err
	}
	if !agent.usable() {
		return SwapTicket{}, errs.New(errs.CodeForbidden, "delegation: the agent is "+agent.State).WithField("reason", ReasonAgentNotActive)
	}
	c, err := getCapability(ctx, tx, req.CapabilityID, true)
	if err != nil && !errs.HasCode(err, errs.CodeNotFound) {
		return SwapTicket{}, err
	}
	if err != nil || c.Terms.AccountID != req.AccountID || !strings.EqualFold(c.Terms.AgentID, agent.ID) {
		return SwapTicket{}, errs.New(errs.CodeForbidden, "delegation: the agent holds no such capability on this account").
			WithField("reason", ReasonAccountMismatch)
	}
	if err := s.authorizer.Require(ctx, tx, gates.AgentSigning, c.Terms.Mode); err != nil {
		return SwapTicket{}, err
	}
	venue, err := getVenue(ctx, tx, req.VenueID)
	if err != nil {
		return SwapTicket{}, err
	}
	pay, err := loadAsset(ctx, tx, req.PayAssetID)
	if err != nil {
		return SwapTicket{}, err
	}
	recv, err := loadAsset(ctx, tx, req.ReceiveAssetID)
	if err != nil {
		return SwapTicket{}, err
	}
	payMint, recvMint, err := devnetMints(venue.Chain, pay, recv)
	if err != nil {
		return SwapTicket{}, err
	}
	wallet, err := s.signer.EnsureKey(ctx, tx, req.AccountID, venue.Chain)
	if err != nil {
		return SwapTicket{}, err
	}
	now := dbTime(s.clk.Now())
	// What the swap does is decided by the direction of its value. The
	// agent's label is only its claim: the intent records it, and a claim
	// that disagrees is refused below, recorded.
	action := SwapAction(pay, recv)
	kind := econ.KindAcquireExposure
	if req.Action == ActionReduceExposure {
		kind = econ.KindReduceExposure
	}
	exposure := recv
	if action == ActionReduceExposure {
		exposure = pay
	}
	terms, err := json.Marshal(swapTerms{Action: string(req.Action), VenueID: venue.ID.String(), ReceiveAssetID: recv.ID.String(),
		RequestNonce: strconv.FormatUint(req.Nonce, 10), MaxSlippageBPS: strconv.FormatInt(int64(req.MaxSlippageBPS), 10)})
	if err != nil {
		return SwapTicket{}, errs.Wrap(err, errs.CodeInternal, "delegation: encode terms")
	}
	amountIn := req.AmountIn
	in, existing, err := s.intents.Submit(ctx, tx, econ.Intent{
		AccountID: req.AccountID, Kind: kind, Mode: c.Terms.Mode, Capability: gates.AgentSigning,
		ActorType: security.ActorAgent, ActorID: agent.ID, AgentID: agent.ID, StrategyRef: c.Terms.StrategyID,
		DelegationID: c.ID.String(), AssetID: &pay.ID, Quantity: &amountIn, Terms: terms,
		IdempotencyKey: "delegation:swap:" + c.ID.String() + ":" + strconv.FormatUint(req.Nonce, 10), CorrelationID: req.CorrelationID,
	})
	if err != nil {
		return SwapTicket{}, err
	}
	if existing {
		return s.replaySwap(ctx, tx, in)
	}
	refuse := func(code errs.Code, reason, detail string) (SwapTicket, error) {
		rerr := refused(code, reason, detail)
		if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, serviceActor, rerr.Error(), reason); err != nil {
			return SwapTicket{}, err
		}
		return SwapTicket{Refusal: rerr}, nil
	}
	if req.Action != action {
		return refuse(errs.CodeForbidden, ReasonActionMismatch, "the swap pays "+pay.Symbol+" for "+recv.Symbol+", which is "+
			string(action)+", not "+string(req.Action))
	}

	// Route: the settlement compiler's agent-authority ladder and legal router.
	rt, err := econ.Route(econ.RouteRequest{
		Intent:         in,
		Subject:        settlement.Subject{Type: settlement.SubjectInstrument, ID: exposure.ID.String(), AssetID: exposure.ID.String()},
		AgentAuthority: agentauthority.LevelUserApprovedRule, Jurisdiction: req.Jurisdiction, Verification: valuedomain.VerificationNone,
	}, s.router, now)
	if err != nil {
		return SwapTicket{}, err
	}
	if !rt.MayExecuteNow() {
		return refuse(errs.CodeForbidden, RefusalRouteDenied, "the settlement compiler refused the swap: "+strings.Join(rt.Reasons, ","))
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRouted, serviceActor, "routed through the settlement compiler", ""); err != nil {
		return SwapTicket{}, err
	}
	// An agent's request is new agent activity: every matching switch stops
	// it, including one on the strategy version the agent runs.
	kill := agent.killAction(killswitch.NewRisk, req.AccountID, gates.AgentSigning)
	kill.Venue, kill.Chain = venue.ID.String(), string(venue.Chain)
	if err := s.kills.Check(ctx, tx, kill); err != nil {
		if errs.CodeOf(err) == errs.CodeKillSwitchActive {
			return refuse(errs.CodeKillSwitchActive, RefusalKillSwitch, err.Error())
		}
		return SwapTicket{}, err
	}
	// Price at the venue.
	payPrice, payPar, perr := s.priceOf(ctx, tx, pay, now)
	recvPrice, recvPar, rerr := s.priceOf(ctx, tx, recv, now)
	if perr != nil || rerr != nil {
		for _, e := range []error{perr, rerr} {
			if e != nil && !errs.HasCode(e, errs.CodeStaleMarketData) {
				return SwapTicket{}, e
			}
		}
		return refuse(errs.CodeStaleMarketData, RefusalPriceUnavailable, "the venue has no fresh price for one of the assets")
	}
	if req.MaxSlippageBPS > s.signer.Policy().MaxSlippageBPS {
		return refuse(errs.CodeValidationFailed, ReasonSlippageAbovePolicy, "the slippage bound is above the signing policy's")
	}
	quote, err := ComputeQuote(QuoteInput{AmountIn: amountIn, PayDecimals: pay.Decimals, ReceiveDecimals: recv.Decimals,
		PayPrice: payPrice, ReceivePrice: recvPrice, SpreadBPS: venue.SpreadBPS, MaxSlippageBPS: req.MaxSlippageBPS})
	if err != nil {
		if r := reasonOf(err); r == ReasonSlippageExceeded || r == ReasonAmountTooSmall {
			return refuse(errs.CodeOf(err), r, err.Error())
		}
		return SwapTicket{}, err
	}
	if _, err := devnet.AmountOf(quote.AmountOut); err != nil {
		return refuse(errs.CodeOverflow, ReasonAmountTooSmall, "the receive amount does not fit a devnet amount")
	}
	if ok, err := s.venueCovers(ctx, tx, venue, recv.ID, quote.AmountOut); err != nil {
		return SwapTicket{}, err
	} else if !ok {
		return refuse(errs.CodeVenueLiquidityInsufficient, RefusalVenueInventory, "the venue's house book cannot cover the receive amount")
	}
	// The capability against this exact use, then Risk V2 with it as the
	// authority section.
	classes, classified := swapClasses(pay, recv)
	dayUsed, totalUsed, err := usageTotals(ctx, tx, c.ID, now)
	if err != nil {
		return SwapTicket{}, err
	}
	check := c.Check(Use{AccountID: req.AccountID, AgentID: agent.ID, StrategyID: agent.StrategyID, Action: action,
		Classes: classes, Chain: venue.Chain, Destination: venue.Address, Notional: quote.NotionalUSD, DayUsed: dayUsed,
		TotalUsed: totalUsed, Environment: s.env, Mode: c.Terms.Mode, Now: now})
	if !classified {
		check.InScope = false
		check.Codes = sortedCodes(append(check.Codes, ReasonInstrumentNotGranted))
	}
	decision, err := s.evaluateSwapRisk(ctx, tx, in, action, agent, exposure, pay, payPrice, payPar, recvPrice, recvPar, venue, quote,
		check, now)
	if err != nil {
		return SwapTicket{}, err
	}
	if rej := decision.RejectionError(); rej != nil {
		return refuse(errs.CodeRiskRejected, RefusalRiskRejected, rej.Error()+" "+strings.Join(check.Codes, ","))
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRiskApproved, serviceActor, "risk kernel allowed the swap", ""); err != nil {
		return SwapTicket{}, err
	}
	res, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: req.AccountID.String(), AssetID: pay.ID, Quantity: amountIn, ActorType: serviceActor.Type, ActorID: serviceActor.ID,
		EconomicIntentID: in.ID.String(), IdempotencyKey: "delegation:swap:" + in.ID.String(), TTL: s.params.ReservationTTL,
		Reason: "delegated swap: units to pay",
	})
	if err != nil {
		switch errs.CodeOf(err) {
		case errs.CodeInsufficientBuyingPower, errs.CodeAccountFrozen, errs.CodeAssetRestricted:
			return refuse(errs.CodeOf(err), RefusalInsufficientFunds, err.Error())
		}
		return SwapTicket{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusReserved, serviceActor, "pay asset reserved", ""); err != nil {
		return SwapTicket{}, err
	}
	sid := id.New[swapKind]()
	usage := newUsageID()
	if err := insertUsage(ctx, tx, usage, c, SubjectSwap, sid.String(), action, quote.NotionalUSD, now); err != nil {
		return SwapTicket{}, err
	}
	fee, ok := HostFees(s.host)(venue.Chain)
	if !ok {
		return SwapTicket{}, errs.New(errs.CodeValidationFailed, "delegation: the host does not run chain "+string(venue.Chain))
	}
	plan := SwapPlan{
		Version: swapPlanVersion, SwapID: sid.String(), AccountID: req.AccountID.String(), AgentID: agent.ID,
		CapabilityID: c.ID.String(), CapabilityHash: c.HashHex(), IntentID: in.ID.String(),
		RequestNonce: strconv.FormatUint(req.Nonce, 10), ActionClass: string(action), Mode: string(c.Terms.Mode),
		Chain: string(venue.Chain), Wallet: wallet.String(), VenueID: venue.ID.String(), VenueAddress: venue.Address.String(),
		PayAssetID: pay.ID.String(), PayMint: payMint.String(), PayDecimals: strconv.Itoa(int(pay.Decimals)),
		ReceiveAssetID: recv.ID.String(), ReceiveMint: recvMint.String(), ReceiveDecimals: strconv.Itoa(int(recv.Decimals)),
		AmountIn: amountIn.String(), AmountOut: quote.AmountOut.String(), MinAmountOut: quote.MinAmountOut.String(),
		PayPrice: planPrice(payPrice, payPar), ReceivePrice: planPrice(recvPrice, recvPar),
		SpreadBPS: strconv.FormatInt(int64(venue.SpreadBPS), 10), MaxSlippageBPS: strconv.FormatInt(int64(req.MaxSlippageBPS), 10),
		NotionalUSD: quote.NotionalUSD.String(), FeePerTx: strconv.FormatUint(fee, 10),
		RequestedAt: now, QuoteExpiresAt: now.Add(s.params.QuoteTTL),
	}
	planHash, err := plan.Hash()
	if err != nil {
		return SwapTicket{}, err
	}
	planDoc, err := json.Marshal(plan)
	if err != nil {
		return SwapTicket{}, errs.Wrap(err, errs.CodeInternal, "delegation: encode plan")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_swaps (id, capability_id, account_id, agent_id, request_nonce, intent_id,
		usage_id, reservation_id, venue_id, mode, action_class, chain_id, wallet, pay_asset_id, receive_asset_id, amount_in,
		amount_out, min_amount_out, notional_usd_minor, requested_at, quote_expires_at, requires_approval, plan, plan_hash, status,
		network)
		VALUES ($1,$2,$3,$4::uuid,$5::numeric,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16::numeric,$17::numeric,$18::numeric,$19,
		        $20,$21,$22,$23,$24,'ADMITTED',$25)`,
		sid, c.ID, req.AccountID, agent.ID, strconv.FormatUint(req.Nonce, 10), in.ID, usage, res.ID, venue.ID, c.Terms.Mode,
		action, venue.Chain, wallet.String(), pay.ID, recv.ID, amountIn.String(), quote.AmountOut.String(),
		quote.MinAmountOut.String(), quote.NotionalUSD.Minor(), plan.RequestedAt, plan.QuoteExpiresAt,
		c.Terms.RequiresHumanApproval, planDoc, planHash[:], s.Network()); err != nil {
		return SwapTicket{}, dbErr("record swap", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_swap_transitions (id, swap_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,'NONE','ADMITTED',$3,$4,'admitted under the capability')`, id.New[id.Any](), sid, security.ActorAgent, agent.ID); err != nil {
		return SwapTicket{}, dbErr("record swap admission", err)
	}
	if err := s.capital.LockForOrder(ctx, tx, res.ID, sid.String()); err != nil {
		return SwapTicket{}, err
	}
	sw, err := getSwap(ctx, tx, sid, true)
	if err != nil {
		return SwapTicket{}, err
	}
	if sw.RequiresApproval {
		if sw, err = s.transitionSwap(ctx, tx, sw, SwapAwaitingApproval, serviceActor, "the capability requires the owner's approval", subjectSet{}); err != nil {
			return SwapTicket{}, err
		}
	}
	if err := s.auditEvent(ctx, tx, Actor{Type: security.ActorAgent, ID: agent.ID}, "delegation.swap.requested", "delegated_swap",
		sid.String(), req.AccountID, "agent requested a delegated swap", map[string]any{"swap_id": sid.String(),
			"capability_id": c.ID.String(), "plan_hash": hex.EncodeToString(planHash[:]), "notional_usd": quote.NotionalUSD.String()}); err != nil {
		return SwapTicket{}, err
	}
	return SwapTicket{Swap: sw}, nil
}

func (s *Service) replaySwap(ctx context.Context, tx pgx.Tx, in econ.Intent) (SwapTicket, error) {
	sw, found, err := swapByIntent(ctx, tx, in.ID)
	if err != nil {
		return SwapTicket{}, err
	}
	if found {
		return SwapTicket{Swap: sw, Replay: true}, nil
	}
	if in.Status == econ.StatusRejected {
		return SwapTicket{Replay: true, Refusal: refused(errs.CodeConflict, in.RejectionCode, "this request was refused")}, nil
	}
	return SwapTicket{}, errs.New(errs.CodeInternal, "delegation: an intent without its swap").WithField("intent_id", in.ID.String())
}

// devnetMints checks both assets are SIMULATED devnet tokens on chain and
// returns their mints.
func devnetMints(chain devnet.ChainID, pay, recv assets.Asset) (devnet.PublicKey, devnet.PublicKey, error) {
	mint := func(a assets.Asset) (devnet.PublicKey, error) {
		if a.Kind != assets.KindSimulated || a.Chain != string(chain) {
			return devnet.PublicKey{}, errs.New(errs.CodeValidationFailed, "delegation: asset "+a.Symbol+" is not a simulated token on "+string(chain))
		}
		if a.Status != assets.StatusActive {
			return devnet.PublicKey{}, errs.New(errs.CodeAssetRestricted, "delegation: asset "+a.Symbol+" is "+string(a.Status))
		}
		k, err := devnet.ParsePublicKey(a.MintAddress)
		if err != nil {
			return devnet.PublicKey{}, errs.New(errs.CodeValidationFailed, "delegation: asset "+a.Symbol+" has no devnet mint")
		}
		return k, nil
	}
	p, err := mint(pay)
	if err != nil {
		return devnet.PublicKey{}, devnet.PublicKey{}, err
	}
	r, err := mint(recv)
	return p, r, err
}

// venueCovers reports whether the venue's house book holds amountOut of the
// receive asset beyond what its open swaps have already promised. Admissions
// against one venue are serialized by a transaction-scoped advisory lock, so
// two admissions cannot promise the same inventory.
func (s *Service) venueCovers(ctx context.Context, tx pgx.Tx, v Venue, asset assets.AssetID, amountOut money.Quantity) (bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "delegation/venue/"+v.ID.String()); err != nil {
		return false, dbErr("lock venue", err)
	}
	house, err := econ.NewRegistry().HouseAccount(ctx, tx, v.HouseAccountID)
	if err != nil {
		return false, err
	}
	if house.Status != econ.HouseActive {
		return false, nil
	}
	held, err := s.ledger.Balance(ctx, tx, house.Account(ledger.CodeHouseInventory, asset))
	if err != nil {
		return false, err
	}
	var promised string
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(amount_out), 0)::text FROM delegated_swaps
		WHERE venue_id = $1 AND receive_asset_id = $2 AND status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED','SIGNED','SUBMITTED','UNKNOWN')`,
		v.ID, asset).Scan(&promised); err != nil {
		return false, dbErr("sum venue promises", err)
	}
	open, err := money.ScanQuantity(promised)
	if err != nil {
		return false, errs.Wrap(err, errs.CodeInternal, "delegation: venue promises")
	}
	return held.Sub(open).Cmp(amountOut) >= 0, nil
}

// evaluateSwapRisk runs Risk V2 with every section a swap requires (account,
// identity, instrument, oracle when risk-increasing, authority, strategy)
// and the ones that apply (balance, settlement, chain, venue), and persists
// the decision whatever it says. Whether the swap increases risk is its
// action as SwapAction derived it -- never the agent's label.
func (s *Service) evaluateSwapRisk(ctx context.Context, tx pgx.Tx, in econ.Intent, action ActionClass, agent agentRow,
	exposure, pay assets.Asset, payPrice money.Price, payPar bool, recvPrice money.Price, recvPar bool, venue Venue, quote Quote,
	check AuthorityCheck, now time.Time) (risk.AdvancedDecision, error) {
	acct, err := s.loadAccount(ctx, tx, in.AccountID)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	verified, err := s.identity.IdentityVerified(ctx, tx, in.AccountID)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	avail, err := s.capital.Availability(ctx, tx, in.AccountID.String(), pay.ID, now)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	available, err := ValueUSD(avail.Available, pay.Decimals, payPrice, money.RoundDown)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	required, err := ValueUSD(*in.Quantity, pay.Decimals, payPrice, money.RoundDown)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	unknown, err := unknownEffects(ctx, tx, in.AccountID)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	approved, err := strategyActive(ctx, tx, agent.StrategyID)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	kills, err := s.killSwitches(ctx, tx)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	observed := now
	for _, p := range []struct {
		price money.Price
		par   bool
	}{{payPrice, payPar}, {recvPrice, recvPar}} {
		if !p.par && p.price.At.Before(observed) {
			observed = p.price.At
		}
	}
	class, _ := ClassifyAsset(exposure)
	_, venueSigner := s.venues[venue.Address]
	input := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{ID: in.ID.String(), Kind: string(in.Kind), AccountID: in.AccountID.String(),
			ActorType: string(in.ActorType), AgentID: agent.ID, StrategyRef: in.StrategyRef,
			StrategyVersionID: agent.StrategyVersionID, Capability: string(in.Capability), Mode: string(in.Mode),
			InstrumentKind: string(class), NotionalUSD: quote.NotionalUSD, RiskIncreasing: action == ActionAcquireExposure},
		Identity:     &risk.IdentitySnapshot{Verified: verified},
		Account:      &risk.AdvancedAccountSnapshot{Status: string(acct.Status)},
		Instrument:   &risk.InstrumentSnapshot{Status: string(exposure.Status), SessionOpen: true, Expired: false},
		Balance:      &risk.BalanceSnapshot{Available: available, Required: required},
		Oracle:       &risk.OracleSnapshot{Available: true, ObservedAt: observed},
		Venue:        &risk.VenueSnapshot{Healthy: venueSigner},
		Chain:        &risk.ChainSnapshot{Healthy: s.host.Health(ctx, venue.Chain) == nil},
		Settlement:   &risk.SettlementSnapshot{UnknownEffects: unknown},
		Authority:    check.Snapshot(),
		Strategy:     &risk.StrategySnapshot{Approved: approved && agent.usable(), ManifestPermits: check.InScope},
		KillSwitches: kills,
		Now:          now,
	}
	decision := risk.EvaluateAdvanced(s.policy, input)
	if _, err := s.risk.RecordAdvancedDecision(ctx, tx, input, decision, in.CorrelationID); err != nil {
		return risk.AdvancedDecision{}, err
	}
	return decision, nil
}

// transitionSwap moves a swap and writes everything the move licenses: the
// transition row, the set-once columns, the audit event.
func (s *Service) transitionSwap(ctx context.Context, tx pgx.Tx, sw Swap, to SwapStatus, actor Actor, reason string, set subjectSet) (Swap, error) {
	if !CanTransitionSwap(sw.Status, to) {
		return Swap{}, errs.Newf(errs.CodeInvalidStateTransition, "delegation: swap %s cannot move %s -> %s", sw.ID, sw.Status, to)
	}
	reason = boundReason(reason)
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_swap_transitions (id, swap_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, id.New[id.Any](), sw.ID, sw.Status, to, actor.Type, actor.ID, reason); err != nil {
		return Swap{}, dbErr("record swap transition", err)
	}
	args, err := set.setArgs()
	if err != nil {
		return Swap{}, err
	}
	out, err := scanSwap(tx.QueryRow(ctx, `UPDATE delegated_swaps SET `+subjectSetSQL+` WHERE id = $1 RETURNING `+swapColumns,
		append([]any{sw.ID, to}, args...)...))
	if err != nil {
		return Swap{}, dbErr("move swap", err)
	}
	if err := s.auditEvent(ctx, tx, actor, "delegation.swap."+strings.ToLower(string(to)), "delegated_swap", sw.ID.String(),
		sw.AccountID, reason, map[string]any{"swap_id": sw.ID.String(), "from": string(sw.Status), "to": string(to),
			"tx_id": string(out.TxID), "failure_code": out.FailureCode}); err != nil {
		return Swap{}, err
	}
	return out, nil
}

// SwapDecision is the account owner's answer for a swap awaiting approval.
// PlanHash is the plan the owner was shown; a different plan is refused.
type SwapDecision struct {
	SwapID   SwapID
	PlanHash [32]byte
	Approve  bool
	Reason   string
}

// DecideSwap records the owner's decision, bound to the plan hash: APPROVED
// lets the swap be signed; a decline cancels it and releases its funds.
func (s *Service) DecideSwap(ctx context.Context, tx pgx.Tx, d SwapDecision) (Swap, error) {
	reason := strings.TrimSpace(d.Reason)
	if reason == "" || len(reason) > 500 {
		return Swap{}, errs.New(errs.CodeValidationFailed, "delegation: a decision needs a reason of 1..500 characters")
	}
	sw, err := getSwap(ctx, tx, d.SwapID, true)
	if err != nil {
		return Swap{}, err
	}
	p, err := s.ownerFor(ctx, tx, sw.AccountID, s.params.StepUpMaxAge)
	if err != nil {
		return Swap{}, err
	}
	if sw.Status != SwapAwaitingApproval {
		return Swap{}, errs.Newf(errs.CodeInvalidStateTransition, "delegation: swap %s is %s, not awaiting approval", sw.ID, sw.Status)
	}
	if d.PlanHash != sw.PlanHash {
		return Swap{}, errs.New(errs.CodeConflict, "delegation: the decision names a plan other than the swap's").
			WithField("reason", ReasonApprovalPlanMismatch)
	}
	decision := DecisionDecline
	if d.Approve {
		decision = DecisionApprove
	}
	owner := Actor{Type: security.ActorUser, ID: p.SubjectID}
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_swap_approvals (id, swap_id, decision, plan_hash, approver_actor_type,
		approver_actor_id, authenticated_at, decided_at, reason) VALUES ($1,$2,$3,$4,'USER',$5,$6,$7,$8)`,
		id.New[id.Any](), sw.ID, decision, sw.PlanHash[:], p.SubjectID, p.AuthTime.UTC(), dbTime(s.clk.Now()), reason); err != nil {
		return Swap{}, dbErr("record swap decision", err)
	}
	if d.Approve {
		return s.transitionSwap(ctx, tx, sw, SwapApproved, owner, reason, subjectSet{})
	}
	if _, err := s.capital.Release(ctx, tx, sw.ReservationID, "delegated swap declined by the account owner"); err != nil {
		return Swap{}, err
	}
	if err := s.moveIntent(ctx, tx, sw.IntentID, econ.StatusCancelled, "declined by the account owner", ""); err != nil {
		return Swap{}, err
	}
	return s.transitionSwap(ctx, tx, sw, SwapCancelled, owner, reason, subjectSet{failureCode: RefusalDeclinedByOwner})
}

// SignOutcome is what a signing step did. Fresh means this call signed: the
// caller now owns the one submission. A Refusal is recorded and committed.
type SignOutcome struct {
	Swap       Swap
	Withdrawal Withdrawal
	Fresh      bool
	Refusal    error
}

// rejectSwap ends a swap that will not be signed: the reservation returns,
// the intent is REJECTED with the code. Recorded, never an error.
func (s *Service) rejectSwap(ctx context.Context, tx pgx.Tx, sw Swap, code, detail string) (Swap, error) {
	if sw.EffectID != nil {
		if _, err := s.effects.Fail(ctx, tx, *sw.EffectID, code, serviceActor, detail); err != nil {
			return Swap{}, err
		}
	}
	if err := s.releaseIfActive(ctx, tx, sw.ReservationID, "delegated swap refused: "+code); err != nil {
		return Swap{}, err
	}
	if err := s.moveIntent(ctx, tx, sw.IntentID, econ.StatusRejected, detail, code); err != nil {
		return Swap{}, err
	}
	return s.transitionSwap(ctx, tx, sw, SwapRejected, serviceActor, detail, subjectSet{failureCode: boundCode(code)})
}

func (s *Service) releaseIfActive(ctx context.Context, tx pgx.Tx, rid capital.ReservationID, reason string) error {
	r, err := s.capital.Get(ctx, tx, rid)
	if err != nil {
		return err
	}
	if r.Status != capital.ReservationActive {
		return nil
	}
	_, err = s.capital.Release(ctx, tx, rid, reason)
	return err
}

// swapStanding is whether the swap may still go ahead at all: the gate, the
// kill switches (the agent's and its strategy version's among them), what
// the swap does, the account, the agent and the capability, now. What the
// swap does is derived again from its assets (SwapAction): a swap whose label
// no longer matches it does not go ahead, and the account and the capability
// are measured against the derived action, never the label. It returns the
// first reason it may not, or "".
func (s *Service) swapStanding(ctx context.Context, tx pgx.Tx, sw Swap, now time.Time) (string, string, error) {
	if err := s.authorizer.Require(ctx, tx, gates.AgentSigning, sw.Mode); err != nil {
		if e, ok := errs.As(err); ok && (e.Code == errs.CodeCapabilityNotApproved || e.Code == errs.CodeKillSwitchActive) {
			return RefusalGateClosed, err.Error(), nil
		}
		return "", "", err
	}
	venue, err := getVenue(ctx, tx, sw.VenueID)
	if err != nil {
		return "", "", err
	}
	agent, found, err := getAgent(ctx, tx, sw.AgentID)
	if err != nil {
		return "", "", err
	}
	if !found {
		agent = agentRow{ID: sw.AgentID}
	}
	kill := agent.killAction(killswitch.NewRisk, sw.AccountID, gates.AgentSigning)
	kill.Venue, kill.Chain = venue.ID.String(), string(sw.Chain)
	if err := s.kills.Check(ctx, tx, kill); err != nil {
		if errs.CodeOf(err) == errs.CodeKillSwitchActive {
			return RefusalKillSwitch, err.Error(), nil
		}
		return "", "", err
	}
	pay, recv, err := swapAssets(ctx, tx, sw)
	if err != nil {
		return "", "", err
	}
	action := SwapAction(pay, recv)
	if action != sw.Action {
		return ReasonActionMismatch, "the swap is labelled " + string(sw.Action) + " but paying " + pay.Symbol + " for " +
			recv.Symbol + " is " + string(action), nil
	}
	acct, err := s.loadAccount(ctx, tx, sw.AccountID)
	if err != nil {
		return "", "", err
	}
	if (action == ActionAcquireExposure && !acct.Status.AllowsNewRisk()) || !acct.Status.AllowsRiskReduction() {
		return ReasonAccountNotActive, "the account is " + string(acct.Status), nil
	}
	if !found || !agent.usable() {
		return ReasonAgentNotActive, "the agent may not act now", nil
	}
	c, err := getCapability(ctx, tx, sw.CapabilityID, false)
	if err != nil {
		return "", "", err
	}
	classes, _ := swapClasses(pay, recv)
	dayUsed, totalUsed, err := usageExcluding(ctx, tx, sw.CapabilityID, sw.UsageID, sw.RequestedAt)
	if err != nil {
		return "", "", err
	}
	if check := c.Check(Use{AccountID: sw.AccountID, AgentID: sw.AgentID, StrategyID: agent.StrategyID, Action: action,
		Classes: classes, Chain: sw.Chain, Destination: venue.Address, Notional: sw.NotionalUSD, DayUsed: dayUsed,
		TotalUsed: totalUsed, Environment: s.env, Mode: sw.Mode, Now: now}); !check.OK() {
		return check.Codes[0], "the capability no longer covers the swap: " + strings.Join(check.Codes, ","), nil
	}
	return "", "", nil
}

// preSignSwap is what the service checks before it asks the provider: the
// swap's standing, the venue's quote and the venue's co-signer. The provider
// then checks the capability again on its own.
func (s *Service) preSignSwap(ctx context.Context, tx pgx.Tx, sw Swap, now time.Time) (string, string, error) {
	if code, detail, err := s.swapStanding(ctx, tx, sw, now); err != nil || code != "" {
		return code, detail, err
	}
	if !now.Before(sw.QuoteExpiresAt) {
		return ReasonQuoteExpired, "the venue's quote expired before the swap was signed", nil
	}
	venue, err := getVenue(ctx, tx, sw.VenueID)
	if err != nil {
		return "", "", err
	}
	if _, ok := s.venues[venue.Address]; !ok {
		return ReasonVenueSignerUnavailable, "the venue cannot co-sign", nil
	}
	return "", "", nil
}

// SignSwap compiles the swap's transaction from its persisted plan, has the
// provider decide and sign it and the venue co-sign it, inspects the signed
// bytes once more, and records them with the claimed effect: SIGNED. Nothing
// is submitted. A swap that is not signable is returned as it is.
func (s *Service) SignSwap(ctx context.Context, tx pgx.Tx, sid SwapID) (SignOutcome, error) {
	if err := requireExecutor(ctx); err != nil {
		return SignOutcome{}, err
	}
	sw, err := getSwap(ctx, tx, sid, true)
	if err != nil {
		return SignOutcome{}, err
	}
	if !sw.Status.Signable() {
		return SignOutcome{Swap: sw}, nil
	}
	if err := s.requireNetwork(SubjectSwap, sw.ID.String(), sw.Network); err != nil {
		return SignOutcome{}, err
	}
	now := dbTime(s.clk.Now())
	refuse := func(code, detail string) (SignOutcome, error) {
		out, err := s.rejectSwap(ctx, tx, sw, code, detail)
		if err != nil {
			return SignOutcome{}, err
		}
		return SignOutcome{Swap: out, Refusal: refused(errs.CodeSigningRejected, code, detail)}, nil
	}
	if code, detail, err := s.preSignSwap(ctx, tx, sw, now); err != nil {
		return SignOutcome{}, err
	} else if code != "" {
		return refuse(code, detail)
	}
	// The boundary decides once. A swap it already refused (an offered
	// message that was not the compiled one) is never offered again.
	if v, found, err := decisionOf(ctx, tx, SubjectSwap, sw.ID.String()); err != nil {
		return SignOutcome{}, err
	} else if found {
		return refuse(RefusalSigningRejected, "the signing provider already decided this swap ("+string(v)+"); it decides once")
	}
	// Compile, against the chain's current tip, and record what was compiled
	// before anyone signs it.
	tip, err := tipOf(ctx, s.host, tx, sw.Chain)
	if err != nil {
		return SignOutcome{}, err
	}
	msg, err := sw.Plan.Message(tip.Hash)
	if err != nil {
		return refuse(InspectInvalidPlan, "the persisted plan does not compile: "+err.Error())
	}
	msgBytes, err := msg.Bytes()
	if err != nil {
		return refuse(InspectInvalidPlan, "the compiled message does not encode")
	}
	height := tip.Height
	if sw, err = s.setSwapColumns(ctx, tx, sw, subjectSet{message: msgBytes, recentBlockhash: &tip.Hash, blockhashHeight: &height}); err != nil {
		return SignOutcome{}, err
	}
	decision, err := s.signer.Sign(ctx, tx, SignRequest{Kind: SubjectSwap, SubjectID: sw.ID.String(), Message: msgBytes,
		MessageSHA256: sha256.Sum256(msgBytes)})
	if err != nil {
		return SignOutcome{}, err
	}
	if decision.Verdict != VerdictApproved {
		return refuse(RefusalSigningRejected, "the signing provider refused: "+strings.Join(decision.ReasonCodes, ","))
	}
	venue, err := getVenue(ctx, tx, sw.VenueID)
	if err != nil {
		return SignOutcome{}, err
	}
	vs, ok := s.venues[venue.Address]
	if !ok {
		return refuse(ReasonVenueSignerUnavailable, "the venue cannot co-sign")
	}
	venueSig, err := vs.CoSign(sw.Plan, sw.PlanHash, tip.Hash, msg)
	if err != nil {
		return refuse(RefusalSigningRejected, "the venue refused to co-sign: "+reasonOf(err))
	}
	raw, err := assemble(msg, decision.Signature, venueSig)
	if err != nil {
		return SignOutcome{}, err
	}
	c, err := getCapability(ctx, tx, sw.CapabilityID, false)
	if err != nil {
		return SignOutcome{}, err
	}
	fee, _ := HostFees(s.host)(sw.Chain)
	txID, err := InspectSwapTransaction(SwapExpectation{Plan: sw.Plan, PlanHash: sw.PlanHash, Blockhash: tip.Hash,
		Wallet: sw.Wallet, AllowedChains: c.Terms.ChainAllowlist, AllowedDestinations: c.Terms.DestinationAllowlist,
		ChainFee: fee, MaxFee: s.signer.Policy().MaxFeePerTx}, raw)
	if err != nil {
		return refuse(RefusalSigningRejected, "the signed transaction failed inspection: "+reasonOf(err))
	}
	effect, replayed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: "delegation:swap:" + sw.ID.String(), Subsystem: econ.SubsystemAgentAuthority, Kind: effectKindDelegatedSwap,
		IntentID: &sw.IntentID, AccountID: sw.AccountID.String(), PostsLedger: true, Actor: serviceActor,
		Request: map[string]string{"swap_id": sw.ID.String(), "plan_hash": hex.EncodeToString(sw.PlanHash[:]), "tx_id": string(txID)},
	})
	if err != nil {
		return SignOutcome{}, err
	}
	if replayed {
		return SignOutcome{}, errs.New(errs.CodeInternal, "delegation: a swap being signed found its effect already claimed")
	}
	if err := s.moveIntent(ctx, tx, sw.IntentID, econ.StatusExecuting, "signed; submitting once", ""); err != nil {
		return SignOutcome{}, err
	}
	out, err := s.transitionSwap(ctx, tx, sw, SwapSigned, serviceActor, "signed by the delegated wallet and the venue after inspection",
		subjectSet{signedAt: &now, txID: txID, signedTx: raw, effectID: &effect.ID})
	if err != nil {
		return SignOutcome{}, err
	}
	return SignOutcome{Swap: out, Fresh: true}, nil
}

// setSwapColumns writes set-once columns without a status change.
func (s *Service) setSwapColumns(ctx context.Context, tx pgx.Tx, sw Swap, set subjectSet) (Swap, error) {
	if set.empty() {
		return sw, nil
	}
	args, err := set.setArgs()
	if err != nil {
		return Swap{}, err
	}
	// A nil status ($2) keeps the stored one: this writes set-once columns only.
	out, err := scanSwap(tx.QueryRow(ctx, `UPDATE delegated_swaps SET `+subjectSetSQL+` WHERE id = $1 RETURNING `+swapColumns,
		append([]any{sw.ID, nil}, args...)...))
	if err != nil {
		return Swap{}, dbErr("record compiled swap", err)
	}
	return out, nil
}

// GateSwapSubmission is the last check before the one submission: the gate,
// the kill switches, the account, the agent and the capability, now. A swap
// it stops is FAILED (never submitted) and its funds return.
func (s *Service) GateSwapSubmission(ctx context.Context, tx pgx.Tx, sid SwapID) (Swap, error) {
	if err := requireExecutor(ctx); err != nil {
		return Swap{}, err
	}
	sw, err := getSwap(ctx, tx, sid, true)
	if err != nil {
		return Swap{}, err
	}
	if sw.Status != SwapSigned {
		return sw, errs.Newf(errs.CodeInvalidStateTransition, "delegation: swap %s is %s, not awaiting its submission", sw.ID, sw.Status)
	}
	now := dbTime(s.clk.Now())
	// The quote bounds signing, not submission: a signed swap within its
	// blockhash window may still go out if it still stands.
	code, detail, err := s.swapStanding(ctx, tx, sw, now)
	if err != nil {
		return Swap{}, err
	}
	if code == "" {
		return sw, nil
	}
	out, err := s.failSwap(ctx, tx, sw, ReasonBlockedBeforeSubmission+":"+code, "stopped before submission: "+detail)
	if err != nil {
		return Swap{}, err
	}
	return out, refused(errs.CodeKillSwitchActive, ReasonBlockedBeforeSubmission, detail)
}

// failSwap ends a signed swap that did not and will not settle.
func (s *Service) failSwap(ctx context.Context, tx pgx.Tx, sw Swap, code, reason string) (Swap, error) {
	if sw.EffectID != nil {
		e, err := s.effects.Get(ctx, tx, *sw.EffectID)
		if err != nil {
			return Swap{}, err
		}
		if e.Status == econ.EffectPending || e.Status == econ.EffectUnknown {
			if _, err := s.effects.Fail(ctx, tx, *sw.EffectID, boundCode(code), serviceActor, reason); err != nil {
				return Swap{}, err
			}
		}
	}
	if err := s.releaseIfActive(ctx, tx, sw.ReservationID, reason); err != nil {
		return Swap{}, err
	}
	if err := s.finishIntent(ctx, tx, sw.IntentID, false, reason); err != nil {
		return Swap{}, err
	}
	return s.transitionSwap(ctx, tx, sw, SwapFailed, serviceActor, reason, subjectSet{failureCode: boundCode(code)})
}

// RecordSwapSubmission records how the one submission ended: SUBMITTED,
// UNKNOWN (effect and intent UNKNOWN; reconciled by tx id, never resubmitted)
// or a definite refusal (FAILED, funds returned).
func (s *Service) RecordSwapSubmission(ctx context.Context, tx pgx.Tx, sid SwapID, exec execution) (Swap, error) {
	sw, err := getSwap(ctx, tx, sid, true)
	if err != nil || sw.Status != SwapSigned {
		return sw, err
	}
	switch exec.outcome {
	case execSubmitted:
		return s.transitionSwap(ctx, tx, sw, SwapSubmitted, serviceActor, "the chain accepted the transaction", subjectSet{})
	case execUnknown:
		reason := "the submission's outcome is unknown; it is reconciled by transaction id and never resubmitted"
		if exec.err != nil {
			reason += ": " + exec.err.Error()
		}
		if _, err := s.effects.MarkUnknown(ctx, tx, *sw.EffectID, externalRef(sw.Chain, sw.TxID), serviceActor, boundReason(reason)); err != nil {
			return Swap{}, err
		}
		if err := s.moveIntent(ctx, tx, sw.IntentID, econ.StatusUnknown, boundReason(reason), ""); err != nil {
			return Swap{}, err
		}
		return s.transitionSwap(ctx, tx, sw, SwapUnknown, serviceActor, reason, subjectSet{})
	default:
		return s.failSwap(ctx, tx, sw, RefusalSubmissionRefused+":"+string(devnet.ReasonOf(exec.err)), "the chain refused the transaction")
	}
}

// SettleResult is what a settlement step observed.
type SettleResult struct {
	Swap       Swap
	Withdrawal Withdrawal
	// Mismatch reports that the chain finalized something other than the
	// signed transaction under its id: a material DEVNET_CHAIN record was
	// opened and nothing was posted.
	Mismatch bool
}

// SettleSwap acts on the chain's word for a signed swap. FINALIZED: the
// finalized transaction is reconciled against the signed one, the SIM_TRADE
// exchange is posted, the reservation consumed, the evidence archived and the
// effect applied: SETTLED. Failed for good, or never landed and expired:
// FAILED, funds returned. Anything else: unchanged.
func (s *Service) SettleSwap(ctx context.Context, tx pgx.Tx, sid SwapID) (SettleResult, error) {
	if err := requireExecutor(ctx); err != nil {
		return SettleResult{}, err
	}
	sw, err := getSwap(ctx, tx, sid, true)
	if err != nil {
		return SettleResult{}, err
	}
	if sw.Status != SwapSigned && sw.Status != SwapSubmitted && sw.Status != SwapUnknown {
		return SettleResult{Swap: sw}, nil
	}
	if err := s.requireNetwork(SubjectSwap, sw.ID.String(), sw.Network); err != nil {
		return SettleResult{}, err
	}
	obs, err := observe(ctx, s.host, tx, sw.Chain, sw.TxID, sw.BlockhashHeight)
	if err != nil {
		return SettleResult{}, err
	}
	switch {
	case obs.finalized():
		match, err := matchesChain(ctx, s.host, sw.Chain, sw.TxID, sw.SignedTx)
		if err != nil {
			return SettleResult{}, err
		}
		if !match {
			if err := s.openChainMismatch(ctx, tx, SubjectSwap, sw.ID.String(), sw.AccountID, sw.Chain, sw.TxID); err != nil {
				return SettleResult{}, err
			}
			return SettleResult{Swap: sw, Mismatch: true}, nil
		}
		venue, err := getVenue(ctx, tx, sw.VenueID)
		if err != nil {
			return SettleResult{}, err
		}
		now := dbTime(s.clk.Now())
		posted, err := s.ledger.Post(serviceContext(ctx), tx, SwapPosting(sw, venue.HouseAccountID, now))
		if err != nil {
			return SettleResult{}, err
		}
		if _, err := s.capital.Consume(ctx, tx, sw.ReservationID, sw.AmountIn, 0, sw.ID.String()); err != nil {
			return SettleResult{}, err
		}
		approval, _, err := swapApproval(ctx, tx, sw.ID)
		if err != nil {
			return SettleResult{}, err
		}
		var approvalPtr *approvalRow
		if approval.Decision != "" {
			approvalPtr = &approval
		}
		signedSum := sha256.Sum256(sw.SignedTx)
		ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "delegation/swap/" + sw.ID.String(), Document: settlementEvidence{
			Disclaimer: evidenceDisclaimer, Kind: string(SubjectSwap), SubjectID: sw.ID.String(), CapabilityID: sw.CapabilityID.String(),
			IntentID: sw.IntentID.String(), Plan: sw.Plan, PlanHash: hex.EncodeToString(sw.PlanHash[:]),
			Approval: approvalEvidence(approvalPtr), Chain: string(sw.Chain), TxID: string(sw.TxID),
			SignedTxSHA256: hex.EncodeToString(signedSum[:]), ObservedState: obs.State, ObservedSlot: formatUint(obs.Slot),
			FinalizedHeight: formatUint(obs.FinalizedHeight), Reconciled: true, JournalTransactionID: posted.TransactionID.String(),
		}})
		if err != nil {
			return SettleResult{}, err
		}
		if _, err := s.effects.Apply(ctx, tx, *sw.EffectID, econ.ApplyRequest{JournalTransactionID: &posted.TransactionID,
			ReservationID: sw.ReservationID.String(), ExternalRef: externalRef(sw.Chain, sw.TxID), Evidence: &ev, Actor: serviceActor,
			Reason: "finalized on chain and reconciled"}); err != nil {
			return SettleResult{}, err
		}
		if err := s.finishIntent(ctx, tx, sw.IntentID, true, "finalized on chain and posted"); err != nil {
			return SettleResult{}, err
		}
		if sw.Status == SwapSigned {
			if sw, err = s.transitionSwap(ctx, tx, sw, SwapSubmitted, serviceActor, "observed on chain", subjectSet{}); err != nil {
				return SettleResult{}, err
			}
		}
		out, err := s.transitionSwap(ctx, tx, sw, SwapSettled, serviceActor, "finalized, reconciled and posted",
			subjectSet{journal: &posted.TransactionID})
		return SettleResult{Swap: out}, err
	case obs.failedFinal():
		out, err := s.failSwap(ctx, tx, sw, RefusalChainFailed+":"+string(obs.Status.Err), "the transaction failed on chain")
		return SettleResult{Swap: out}, err
	case obs.Expired:
		out, err := s.failSwap(ctx, tx, sw, RefusalSubmissionExpired,
			"the chain does not hold the transaction and its blockhash expired below finality; it can never land")
		return SettleResult{Swap: out}, err
	}
	return SettleResult{Swap: sw}, nil
}

// SwapResult is a swap's state when ExecuteSwap returns.
type SwapResult struct {
	Swap    Swap
	Replay  bool
	Settled bool
}

// ExecuteSwap carries an admitted (or approved) swap to its end: sign in one
// transaction, check once more and submit exactly once between transactions,
// record the outcome, then (SANDBOX) produce blocks and settle. A replay
// signs nothing and submits nothing; it only observes. An UNKNOWN outcome
// returns SUBMISSION_STATE_UNKNOWN and is reconciled, never resubmitted.
func (s *Service) ExecuteSwap(ctx context.Context, sid SwapID) (SwapResult, error) {
	if err := requireExecutor(ctx); err != nil {
		return SwapResult{}, err
	}
	ctx = serviceContext(ctx)
	sw, err := getSwap(ctx, s.db, sid, false)
	if err != nil {
		return SwapResult{}, err
	}
	if err := s.requireNetwork(SubjectSwap, sw.ID.String(), sw.Network); err != nil {
		return SwapResult{Swap: sw}, err
	}
	if sw.Status.Signable() {
		if err := s.host.EnsureNativeBalance(ctx, sw.Chain, sw.Wallet, s.params.GasMinimum, s.params.GasTopUp); err != nil {
			return SwapResult{Swap: sw}, err
		}
	}
	var out SignOutcome
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.SignSwap(ctx, tx, sid)
		return err
	}); err != nil {
		return SwapResult{Swap: sw}, err
	}
	if out.Refusal != nil {
		return SwapResult{Swap: out.Swap, Settled: true}, out.Refusal
	}
	if !out.Fresh {
		res, err := s.driveSwap(ctx, sid, false, false)
		res.Replay = true
		return res, err
	}
	var gateErr error
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out.Swap, err = s.GateSwapSubmission(ctx, tx, sid)
		if err != nil && reasonOf(err) == ReasonBlockedBeforeSubmission {
			gateErr, err = err, nil
		}
		return err
	}); err != nil {
		return SwapResult{Swap: out.Swap}, err
	}
	if gateErr != nil {
		return SwapResult{Swap: out.Swap, Settled: true}, gateErr
	}
	exec := submitOnce(ctx, s.host, out.Swap.Chain, out.Swap.SignedTx)
	var recorded Swap
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		recorded, err = s.RecordSwapSubmission(ctx, tx, sid, exec)
		return err
	}); err != nil {
		return SwapResult{Swap: out.Swap}, err
	}
	switch {
	case recorded.Status == SwapFailed:
		return SwapResult{Swap: recorded, Settled: true}, refused(errs.CodeConflict, recorded.FailureCode, "the chain refused the transaction")
	case exec.outcome == execUnknown:
		return SwapResult{Swap: recorded}, submissionUnknown(recorded.TxID, exec.err)
	}
	return s.driveSwap(ctx, sid, recorded.Mode == gates.ModeSandbox, false)
}

// driveSwap observes a submitted swap until it settles or stops changing.
// Blocks are produced only in SANDBOX mode and, for a swap merely SIGNED,
// only when produceSigned says nobody is about to submit it (the
// reconciler's stale case): a replay can never age out a transaction another
// caller is about to submit.
func (s *Service) driveSwap(ctx context.Context, sid SwapID, produce, produceSigned bool) (SwapResult, error) {
	for step := 0; step < maxDriveSteps; step++ {
		sw, err := getSwap(ctx, s.db, sid, false)
		if err != nil {
			return SwapResult{}, err
		}
		if sw.Status.Terminal() || (sw.Status != SwapSigned && sw.Status != SwapSubmitted && sw.Status != SwapUnknown) {
			return SwapResult{Swap: sw, Settled: sw.Status.Terminal()}, nil
		}
		if _, err := awaitFinal(ctx, s.host, sw.Chain, sw.TxID, sw.BlockhashHeight, produce && (sw.Status != SwapSigned || produceSigned)); err != nil {
			return SwapResult{Swap: sw}, err
		}
		var res SettleResult
		if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = s.SettleSwap(ctx, tx, sid)
			return err
		}); err != nil {
			return SwapResult{Swap: sw}, err
		}
		if res.Mismatch {
			return SwapResult{Swap: res.Swap}, errs.New(errs.CodeReconciliationRequired,
				"delegation: the chain finalized something other than the signed transaction; a reconciliation record is open")
		}
		if res.Swap.Status == sw.Status {
			return SwapResult{Swap: res.Swap, Settled: res.Swap.Status.Terminal()}, nil
		}
	}
	sw, err := getSwap(ctx, s.db, sid, false)
	return SwapResult{Swap: sw, Settled: sw.Status.Terminal()}, err
}

// maxDriveSteps bounds one drive: each step either changes the subject's
// status or ends the drive.
const maxDriveSteps = 8

func submissionUnknown(txID devnet.TxID, cause error) error {
	e := errs.New(errs.CodeSubmissionStateUnknown,
		"delegation: the submission's outcome is unknown; it will be reconciled by transaction id, never resubmitted").
		WithField("tx_id", string(txID))
	if cause != nil {
		e = e.WithField("cause", cause.Error())
	}
	return e
}

// GetSwap returns one swap.
func (s *Service) GetSwap(ctx context.Context, q db.Querier, sid SwapID) (Swap, error) {
	return getSwap(ctx, q, sid, false)
}

func boundReason(r string) string {
	if len(r) > 1000 {
		return r[:1000]
	}
	if strings.TrimSpace(r) == "" {
		return "unspecified"
	}
	return r
}

func boundCode(c string) string {
	if len(c) > 200 {
		return c[:200]
	}
	return c
}
