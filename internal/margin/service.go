package margin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuation"
)

// IdentityReader answers whether the person behind a customer account has a
// verified identity. Risk Kernel V2 requires the answer for every intent; an
// error refuses the intent.
type IdentityReader interface {
	IdentityVerified(ctx context.Context, q db.Querier, account accounts.AccountID) (bool, error)
}

// Venue answers whether the paper venue can execute a sale of an asset now.
type Venue interface {
	Available(ctx context.Context, asset assets.AssetID) (bool, error)
}

// PaperVenue is the reference-price paper venue of PAPER and SANDBOX margin
// accounts: every order fills in full at the price the engine computed, and
// the venue is always available.
type PaperVenue struct{}

// Available always reports true: a reference-price paper venue has no book
// that can be closed.
func (PaperVenue) Available(context.Context, assets.AssetID) (bool, error) { return true, nil }

// ExposureSource supplies an account's derivative exposures (the
// derivatives lane implements it). A service without one has no derivative
// book connected and evaluates spot margin only.
type ExposureSource interface {
	DerivativeExposures(ctx context.Context, q db.Querier, account accounts.AccountID) ([]DerivativeExposure, error)
}

// Config is the service's configuration.
type Config struct {
	// Policy is the margin policy every account of this service is governed
	// by. It is registered in margin_policies on first use.
	Policy Policy
	// RiskPolicy is the base Risk Kernel V2 policy; EffectiveRiskPolicy
	// tightens it per margin mode.
	RiskPolicy risk.AdvancedPolicy
	// LendingPoolName and InsuranceFundName name the facilities margin
	// accounts are wired to (econ.Registry.EnsureFacility).
	LendingPoolName   string
	InsuranceFundName string
	// ServiceActorID is the id this service applies effects under.
	ServiceActorID string
	// MaxQueueScan bounds the accounts ProcessLiquidationQueue considers.
	MaxQueueScan int
}

// Dependencies are the collaborators of the service. All are required
// except Derivatives.
type Dependencies struct {
	Clock       clock.Clock
	Ledger      *ledger.Service
	Effects     *econ.EffectStore
	Intents     *econ.IntentStore
	Authorizer  *econ.Authorizer
	Registry    *econ.Registry
	Risk        *risk.Store
	Capital     *capital.Service
	Prices      valuation.PriceSource
	Router      *legalrouter.Router
	Identity    IdentityReader
	KillSwitch  econ.KillSwitchReader
	Venue       Venue
	Audit       audit.Writer
	Derivatives ExposureSource
	// FeeLedger records every liquidation fee: the policy's fee is published
	// as a fee-ledger schedule, each fee is disclosed to its payer and booked
	// against the LIQUIDATION journal that charged it. A service without it
	// would charge fees nobody can find, so it is refused at startup.
	FeeLedger *feeledger.Service
}

// Service runs margin accounts against the database. It holds no mutable
// state; every mutating method runs in the caller's transaction.
//
// A refusal -- by the settlement compiler, the authorizer or Risk Kernel V2
// -- is written before the error is returned (the persisted decision and the
// REJECTED intent), so a caller may commit it to keep the record; a replay
// of the same idempotency key then returns the same refusal. Any other error
// must roll the transaction back, as db.InTx does: an operation that failed
// part way through execution is not a fact.
type Service struct {
	cfg        Config
	policy     Policy
	policyHash string
	d          Dependencies
	st         *store
	actor      econ.Actor
	accounts   *accounts.Repository
	assets     *assets.Repository
}

// NewService validates the configuration and wires the service.
func NewService(cfg Config, d Dependencies) (*Service, error) {
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	if cfg.RiskPolicy.Missing() {
		return nil, errs.New(errs.CodeValidationFailed, "margin: a Risk Kernel V2 policy is required")
	}
	if strings.TrimSpace(cfg.LendingPoolName) == "" || strings.TrimSpace(cfg.InsuranceFundName) == "" {
		return nil, errs.New(errs.CodeValidationFailed, "margin: the lending pool and insurance fund must be named")
	}
	if cfg.ServiceActorID == "" {
		cfg.ServiceActorID = "margin-service"
	}
	if cfg.MaxQueueScan <= 0 || cfg.MaxQueueScan > 10_000 {
		cfg.MaxQueueScan = 1_000
	}
	if d.Clock == nil || d.Ledger == nil || d.Effects == nil || d.Intents == nil || d.Authorizer == nil || d.Registry == nil ||
		d.Risk == nil || d.Capital == nil || d.Prices == nil || d.Router == nil || d.Identity == nil || d.KillSwitch == nil ||
		d.Venue == nil || d.Audit == nil || d.FeeLedger == nil {
		return nil, errs.New(errs.CodeValidationFailed, "margin: every dependency except Derivatives is required")
	}
	return &Service{
		cfg: cfg, policy: cfg.Policy, policyHash: cfg.Policy.Hash(), d: d, st: &store{audit: d.Audit},
		actor:    econ.Actor{Type: security.ActorService, ID: cfg.ServiceActorID},
		accounts: accounts.NewRepository(), assets: assets.NewRepository(),
	}, nil
}

// Policy returns the policy the service applies.
func (s *Service) Policy() Policy { return s.policy }

// Requester is who asked for an operation. An agent requester proposes; the
// service applies, and Risk Kernel V2 requires an authority section for it.
type Requester struct {
	Type           security.ActorType
	ID             string
	AgentID        string
	AgentAuthority agentauthority.Level
}

func (r Requester) validate() error {
	if !r.Type.Valid() || strings.TrimSpace(r.ID) == "" {
		return errs.New(errs.CodeValidationFailed, "margin: a requester type and id are required")
	}
	return nil
}

func (s *Service) now() time.Time { return dbTime(s.d.Clock.Now()) }

// keyHash shortens a caller's idempotency key into an effect key component.
func keyHash(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:16])
}

func validKey(k string) error {
	if t := strings.TrimSpace(k); t == "" || t != k || len(k) > 256 {
		return errs.New(errs.CodeValidationFailed, "margin: idempotency key must be 1..256 bytes without surrounding whitespace")
	}
	return nil
}

// authorize checks the capabilities for the account's mode. A risk-reducing
// operation (repaying, posting collateral, accruing, liquidating, covering a
// deficit, evaluating) is never stopped by a kill switch or by a gate that
// was switched off after the account was opened: stopping it would trap the
// risk the switch exists to contain. LIVE, an unsupported mode and an
// unknown environment refuse everything.
func (s *Service) authorize(ctx context.Context, tx pgx.Tx, mode gates.Mode, riskReducing bool, caps ...gates.Capability) error {
	for _, c := range caps {
		if !riskReducing {
			if err := s.d.Authorizer.Require(ctx, tx, c, mode); err != nil {
				return err
			}
			continue
		}
		dec, err := s.d.Authorizer.Decide(ctx, tx, c, mode)
		if err != nil {
			return err
		}
		for _, r := range dec.Reasons {
			if r != gates.ReasonCapabilityKillSwitched && r != gates.ReasonGateNotActive {
				return s.d.Authorizer.Require(ctx, tx, c, mode)
			}
		}
	}
	return nil
}

// --- intents -------------------------------------------------------------

type intentSpec struct {
	kind       econ.IntentKind
	capability gates.Capability
	account    Account
	requester  Requester
	key        string
	asset      assets.AssetID
	quantity   money.Quantity
	terms      map[string]string
	subject    settlement.Subject
	corr       string
}

func (s *Service) submitIntent(ctx context.Context, tx pgx.Tx, sp intentSpec) (econ.Intent, bool, error) {
	terms, err := json.Marshal(sp.terms)
	if err != nil {
		return econ.Intent{}, false, errs.Wrap(err, errs.CodeInternal, "margin: encode intent terms")
	}
	asset := sp.asset
	qty := sp.quantity
	in := econ.Intent{
		AccountID: sp.account.AccountID, Kind: sp.kind, Mode: sp.account.Mode, Capability: sp.capability,
		ActorType: sp.requester.Type, ActorID: sp.requester.ID, AgentID: sp.requester.AgentID,
		AssetID: &asset, Quantity: &qty, Terms: terms, IdempotencyKey: sp.key, CorrelationID: sp.corr,
	}
	return s.d.Intents.Submit(ctx, tx, in)
}

// moveIntent transitions an intent (transition row, outbox and audit in the
// caller's transaction). The whole lifecycle fits one transaction because
// the operation began with bindingsImmediate.
func (s *Service) moveIntent(ctx context.Context, tx pgx.Tx, in *econ.Intent, to econ.IntentStatus, reason, code string) error {
	out, err := s.d.Intents.Transition(ctx, tx, in.ID, to, s.actor, reason, code)
	if err != nil {
		return err
	}
	*in = out
	return nil
}

// abandon moves a non-terminal intent to REJECTED (before execution) or
// FAILED (during execution) with the error's code, and returns the error. If
// the transaction can no longer be written to, the original error wins.
func (s *Service) abandon(ctx context.Context, tx pgx.Tx, in *econ.Intent, cause error) error {
	if in == nil || in.Status.Terminal() {
		return cause
	}
	code := string(errs.CodeOf(cause))
	if ee, ok := errs.As(cause); ok {
		if rc, ok := ee.Fields["reason_codes"].([]string); ok && len(rc) > 0 {
			code = rc[0]
		}
	}
	to := econ.StatusRejected
	if in.Status == econ.StatusExecuting {
		to = econ.StatusFailed
	}
	if err := s.moveIntent(ctx, tx, in, to, "refused: "+code, code); err != nil {
		return cause
	}
	return cause
}

// replayIntent answers a resubmitted intent: COMPLETED means done (the caller
// returns the stored outcome), REJECTED or FAILED returns the same refusal,
// anything else is still in flight elsewhere.
func replayIntent(in econ.Intent) error {
	switch in.Status {
	case econ.StatusCompleted:
		return nil
	case econ.StatusRejected, econ.StatusFailed:
		code := errs.Code(in.RejectionCode)
		if strings.HasPrefix(in.RejectionCode, "RISK_") {
			return errs.New(errs.CodeRiskRejected, "margin: this request was refused").
				WithField("reason_codes", []string{in.RejectionCode}).WithField("intent_id", in.ID.String())
		}
		if code == "" {
			code = errs.CodeConflict
		}
		return errs.New(code, "margin: this request was refused").WithField("intent_id", in.ID.String())
	}
	return errs.New(errs.CodeIdempotencyInProgress, "margin: this request is still in flight").WithField("intent_id", in.ID.String())
}

// route compiles the intent through the settlement compiler. Margin executes
// what it routes, so it requires MayExecuteNow, not merely Permitted: a route
// that is permitted only after a person confirms it (or an operator reviews
// it) is permission to ask, and margin has nobody to ask. Such an intent is
// refused, never executed.
func (s *Service) route(ctx context.Context, tx pgx.Tx, in *econ.Intent, sp intentSpec) error {
	r, err := econ.Route(econ.RouteRequest{Intent: *in, Subject: sp.subject, AgentAuthority: sp.requester.AgentAuthority}, s.d.Router, s.d.Clock.Now())
	if err != nil {
		return err
	}
	if !r.Permitted {
		return errs.New(errs.CodeForbidden, "margin: the settlement compiler refused this intent").WithField("reasons", r.Reasons)
	}
	if !r.MayExecuteNow() {
		return errs.Newf(errs.CodeForbidden, "margin: the settlement compiler permits this intent only after %s; margin executes nothing awaiting confirmation",
			r.RequiredConfirmation).
			WithField("required_confirmation", string(r.RequiredConfirmation)).
			WithField("reasons", r.Reasons)
	}
	return s.moveIntent(ctx, tx, in, econ.StatusRouted, "routed", "")
}

// --- risk ------------------------------------------------------------------

func (s *Service) killSwitches(ctx context.Context, tx pgx.Tx) ([]risk.KillSwitch, error) {
	snap, err := s.d.KillSwitch.Snapshot(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := []risk.KillSwitch{}
	for _, sw := range snap.Switches {
		if sw.Active {
			out = append(out, risk.KillSwitch{Kind: string(sw.Kind), Scope: sw.ScopeID})
		}
	}
	return out, nil
}

// decide evaluates Risk Kernel V2 for an intent, persists the decision and
// moves the intent to RISK_APPROVED or returns the rejection.
func (s *Service) decide(ctx context.Context, tx pgx.Tx, in *econ.Intent, a Account, ai risk.AdvancedIntent, fill func(*risk.AdvancedInput)) (risk.AdvancedDecision, error) {
	cust, err := s.accounts.Get(ctx, tx, a.AccountID)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	verified, err := s.d.Identity.IdentityVerified(ctx, tx, a.AccountID)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	kills, err := s.killSwitches(ctx, tx)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	ai.ID, ai.AccountID, ai.Capability, ai.Mode = in.ID.String(), a.AccountID.String(), string(in.Capability), string(a.Mode)
	ai.ActorType, ai.AgentID = string(in.ActorType), in.AgentID
	input := risk.AdvancedInput{
		Stage: risk.StagePreTrade, Intent: ai,
		Identity:     &risk.IdentitySnapshot{Verified: verified},
		Account:      &risk.AdvancedAccountSnapshot{Status: string(cust.Status)},
		KillSwitches: kills, Now: s.d.Clock.Now().UTC(),
	}
	if fill != nil {
		fill(&input)
	}
	pol := EffectiveRiskPolicy(s.cfg.RiskPolicy, s.policy, a.MarginMode)
	dec := risk.EvaluateAdvanced(pol, input)
	if _, err := s.d.Risk.RecordAdvancedDecision(ctx, tx, input, dec, in.CorrelationID); err != nil {
		return risk.AdvancedDecision{}, err
	}
	if rej := dec.RejectionError(); rej != nil {
		return dec, rej
	}
	return dec, s.moveIntent(ctx, tx, in, econ.StatusRiskApproved, "risk approved: "+dec.Hash, "")
}

// --- effects -----------------------------------------------------------------

// posted is the outcome of one exactly-once posting.
type posted struct {
	Journal  ledger.TransactionID
	Effect   econ.Effect
	Replayed bool
}

type postSpec struct {
	key      string
	kind     string
	intent   *econ.IntentID
	account  accounts.AccountID
	request  any
	posting  ledger.Posting
	evidence any
	reserve  string
	corr     string
}

// postOnce claims the effect, posts the journal, archives the evidence and
// applies the effect. A replay of an APPLIED effect returns its journal and
// posts nothing; an UNKNOWN effect is never retried.
func (s *Service) postOnce(ctx context.Context, tx pgx.Tx, sp postSpec) (posted, error) {
	if err := checkEffectJournal(sp.kind, sp.posting); err != nil {
		return posted{}, err
	}
	acct := ""
	if !sp.account.IsZero() {
		acct = sp.account.String()
	}
	e, replay, err := s.d.Effects.Begin(ctx, tx, econ.BeginRequest{
		Key: sp.key, Subsystem: econ.SubsystemMargin, Kind: sp.kind, IntentID: sp.intent, AccountID: acct,
		Request: sp.request, PostsLedger: true, Actor: s.actor, CorrelationID: sp.corr,
	})
	if err != nil {
		return posted{}, err
	}
	if replay {
		switch e.Status {
		case econ.EffectApplied:
			return posted{Journal: *e.JournalTransactionID, Effect: e, Replayed: true}, nil
		case econ.EffectUnknown:
			return posted{}, errs.New(errs.CodeSubmissionStateUnknown, "margin: this effect's outcome is unknown; it is reconciled, never retried").
				WithField("effect_id", e.ID.String())
		default:
			return posted{}, errs.Newf(errs.CodeConflict, "margin: effect %s is %s", e.ID, e.Status).WithField("effect_id", e.ID.String())
		}
	}
	sp.posting.IdempotencyKey = sp.key
	sp.posting.CorrelationID = sp.corr
	res, err := s.d.Ledger.Post(ctx, tx, sp.posting)
	if err != nil {
		return posted{}, err
	}
	// Effect keys all begin "margin:", so the evidence lands under
	// advanced/margin/<operation>/<identity>.json.
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: strings.ReplaceAll(sp.key, ":", "/"), Document: sp.evidence})
	if err != nil {
		return posted{}, err
	}
	e, err = s.d.Effects.Apply(ctx, tx, e.ID, econ.ApplyRequest{
		JournalTransactionID: &res.TransactionID, ReservationID: sp.reserve, Evidence: &ev, Actor: s.actor, Reason: sp.kind,
	})
	if err != nil {
		return posted{}, err
	}
	return posted{Journal: res.TransactionID, Effect: e}, nil
}

func (s *Service) meta(refType, refID, desc string) PostingMeta {
	return PostingMeta{RefType: refType, RefID: refID, At: s.d.Clock.Now().UTC(), Description: desc}
}

// --- facilities --------------------------------------------------------------

func (s *Service) facilities(ctx context.Context, tx pgx.Tx) (econ.Facility, econ.Facility, error) {
	sys := econ.SystemActor(s.cfg.ServiceActorID)
	pool, err := s.d.Registry.EnsureFacility(ctx, tx, ledger.FacilityLendingPool, s.cfg.LendingPoolName, sys)
	if err != nil {
		return econ.Facility{}, econ.Facility{}, err
	}
	fund, err := s.d.Registry.EnsureFacility(ctx, tx, ledger.FacilityInsuranceFund, s.cfg.InsuranceFundName, sys)
	if err != nil {
		return econ.Facility{}, econ.Facility{}, err
	}
	return pool, fund, nil
}

// facilityBooks names the facility books an operation posts to.
type facilityBooks int

const (
	poolBooks facilityBooks = 1 << iota
	fundBooks
)

// lockFacilities takes the row locks of the facilities whose books an
// operation will post to, before it posts anything, and always in one order:
// the lending pool, then the insurance fund. Every margin operation that
// posts to a facility's books takes its lock first, so two transactions that
// share a facility's ledger rows are serialized on its facility row, and no
// two transactions can hold the two facilities in opposite orders: a
// liquidation step, a deficit coverage and a deficit repayment cannot
// deadlock on the pool's and the fund's cash rows. NO KEY UPDATE does not
// block the foreign-key checks of new margin accounts.
func (s *Service) lockFacilities(ctx context.Context, tx pgx.Tx, a Account, books facilityBooks) error {
	return lockFacilityRows(ctx, tx, a.LendingPoolID, a.InsuranceFundID, books)
}

// lockFacilityRows is lockFacilities for a pool and a fund named directly.
func lockFacilityRows(ctx context.Context, tx pgx.Tx, pool, fund econ.FacilityID, books facilityBooks) error {
	for _, f := range []struct {
		book facilityBooks
		id   econ.FacilityID
	}{{poolBooks, pool}, {fundBooks, fund}} {
		if books&f.book == 0 {
			continue
		}
		var got econ.FacilityID
		if err := tx.QueryRow(ctx, `SELECT id FROM facilities WHERE id = $1 FOR NO KEY UPDATE`, f.id).Scan(&got); err != nil {
			return dbErr("lock facility", err)
		}
	}
	return nil
}

func (s *Service) accountFacilities(ctx context.Context, q db.Querier, a Account) (econ.Facility, econ.Facility, error) {
	pool, err := s.d.Registry.Facility(ctx, q, a.LendingPoolID)
	if err != nil {
		return econ.Facility{}, econ.Facility{}, err
	}
	fund, err := s.d.Registry.Facility(ctx, q, a.InsuranceFundID)
	if err != nil {
		return econ.Facility{}, econ.Facility{}, err
	}
	return pool, fund, nil
}

// --- state -------------------------------------------------------------------

// assetInfo is what the service knows about an asset for margin purposes.
type assetInfo struct {
	asset    assets.Asset
	profile  AssetProfile
	eligible bool
}

func worstProfile() AssetProfile {
	return AssetProfile{Volatility: VolExtreme, Liquidity: LiqIlliquid, Venue: VenueTier3, RiskWeightBPS: 10_000}
}

func (s *Service) assetInfo(ctx context.Context, q db.Querier, aid assets.AssetID) (assetInfo, error) {
	a, err := s.assets.Get(ctx, q, aid)
	if err != nil {
		return assetInfo{}, err
	}
	prof, ok := s.policy.ProfileFor(a.ID.String(), string(a.RiskClass))
	if !ok {
		prof = worstProfile()
	}
	return assetInfo{asset: a, profile: prof, eligible: ok}, nil
}

// mark returns the last known mark of an asset in the numeraire within the
// policy's lookback (possibly stale), or nil when there is none.
func (s *Service) mark(ctx context.Context, q db.Querier, aid, numeraire assets.AssetID, now time.Time) (*money.Price, error) {
	p, err := s.d.Prices.Latest(ctx, q, aid, numeraire, time.Duration(s.policy.MarkLookbackMS)*time.Millisecond, now)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeStaleMarketData {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func (s *Service) holdingFor(ctx context.Context, q db.Querier, a Account, aid assets.AssetID, qty money.Quantity, now time.Time) (Holding, assetInfo, error) {
	info, err := s.assetInfo(ctx, q, aid)
	if err != nil {
		return Holding{}, assetInfo{}, err
	}
	h := Holding{AssetID: aid.String(), Decimals: info.asset.Decimals, Quantity: qty, Numeraire: aid == a.NumeraireAssetID, Profile: info.profile}
	if h.Numeraire {
		h.Profile = AssetProfile{}
		return h, info, nil
	}
	if h.Mark, err = s.mark(ctx, q, aid, a.NumeraireAssetID, now); err != nil {
		return Holding{}, assetInfo{}, err
	}
	return h, info, nil
}

// loadState builds the engine's view of an account from the ledger, the
// borrow lots, the price store and the derivatives source. It checks the
// sub-ledger against the ledger: the open lots must owe exactly the
// account's MARGIN_LOAN balance, or nothing is evaluated.
func (s *Service) loadState(ctx context.Context, q db.Querier, a Account, now time.Time) (AccountState, []Lot, error) {
	num, err := s.assets.Get(ctx, q, a.NumeraireAssetID)
	if err != nil {
		return AccountState{}, nil, err
	}
	st := AccountState{
		AccountID: a.AccountID.String(), Mode: a.MarginMode, NumeraireAssetID: a.NumeraireAssetID.String(),
		NumeraireDecimals: num.Decimals, Now: now, Holdings: []Holding{}, Loans: []Loan{}, Derivatives: []DerivativeExposure{},
	}
	if a.IsolatedAssetID != nil {
		st.IsolatedAssetID = a.IsolatedAssetID.String()
	}
	balances, err := s.d.Ledger.BalancesForOwner(ctx, q, ledger.OwnerCustomer, a.AccountID.String())
	if err != nil {
		return AccountState{}, nil, err
	}
	loanBalance := money.Quantity{}
	for _, b := range balances {
		switch b.Account.Code {
		case ledger.CodeMarginCollateral:
			if !b.Balance.IsPositive() {
				continue
			}
			h, _, err := s.holdingFor(ctx, q, a, b.Account.AssetID, b.Balance, now)
			if err != nil {
				return AccountState{}, nil, err
			}
			st.Holdings = append(st.Holdings, h)
		case ledger.CodeMarginLoan:
			if b.Account.AssetID != a.NumeraireAssetID && b.Balance.Sign() != 0 {
				return AccountState{}, nil, errs.New(errs.CodeInternal, "margin: a loan exists outside the account's numeraire").
					WithField("asset_id", b.Account.AssetID.String())
			}
			if b.Account.AssetID == a.NumeraireAssetID {
				loanBalance = b.Balance
			}
		}
	}
	lots, err := s.st.openLots(ctx, q, a.ID, false)
	if err != nil {
		return AccountState{}, nil, err
	}
	owed := money.Quantity{}
	for _, l := range lots {
		st.Loans = append(st.Loans, Loan{LotID: l.ID.String(), Principal: l.OutstandingPrincipal, Interest: l.OutstandingInterest,
			RateBPS: l.RateBPS, AccruedThrough: l.AccruedThrough})
		owed = owed.Add(l.Outstanding())
	}
	if !owed.Equal(loanBalance) {
		return AccountState{}, nil, errs.New(errs.CodeReconciliationRequired, "margin: the borrow lots disagree with the ledger's MARGIN_LOAN balance").
			WithField("margin_account_id", a.ID.String()).WithField("lots", owed.String()).WithField("ledger", loanBalance.String())
	}
	if s.d.Derivatives != nil {
		ds, err := s.d.Derivatives.DerivativeExposures(ctx, q, a.AccountID)
		if err != nil {
			return AccountState{}, nil, err
		}
		st.Derivatives = append(st.Derivatives, ds...)
	}
	return st, lots, nil
}

// refreshHealth re-evaluates an account from the books and moves it between
// NORMAL, WARNING and MARGIN_CALL, issuing or resolving the margin call. An
// account in LIQUIDATING, BANKRUPT or CLOSED keeps its status: its case
// decides.
func (s *Service) refreshHealth(ctx context.Context, tx pgx.Tx, a *Account, reason string) (Evaluation, *Call, error) {
	now := s.now()
	state, _, err := s.loadState(ctx, tx, *a, now)
	if err != nil {
		return Evaluation{}, nil, err
	}
	ev, err := Evaluate(s.policy, state)
	if err != nil {
		return Evaluation{}, nil, err
	}
	switch a.Status {
	case StatusNormal, StatusWarning, StatusMarginCall:
	default:
		return ev, nil, nil
	}
	call, open, err := s.st.openCall(ctx, tx, a.ID)
	if err != nil {
		return Evaluation{}, nil, err
	}
	if err := s.st.setAccountStatus(ctx, tx, a, ev.Health, s.actor, reason, ev.Hash, now); err != nil {
		return Evaluation{}, nil, err
	}
	switch {
	case ev.Health == StatusMarginCall && !open:
		c, err := s.st.issueCall(ctx, tx, *a, ev, s.actor, now)
		if err != nil {
			return Evaluation{}, nil, err
		}
		return ev, &c, nil
	case ev.Health != StatusMarginCall && open:
		if err := s.st.resolveCall(ctx, tx, *a, &call, CallMet, s.actor, "equity restored above maintenance", now); err != nil {
			return Evaluation{}, nil, err
		}
		return ev, &call, nil
	case open:
		return ev, &call, nil
	}
	return ev, nil, nil
}

func (s *Service) requireOwnPolicy(a Account) error {
	if a.PolicyVersion != s.policy.Version {
		return errs.New(errs.CodeConflict, "margin: the account is governed by another policy version than this service runs").
			WithField("account_policy", a.PolicyVersion).WithField("service_policy", s.policy.Version)
	}
	return nil
}

// lockAccount begins a margin operation: it makes the transaction's status
// bindings immediate (bindingsImmediate) and locks the margin account of a
// customer account. Every margin operation on an account is serialized on
// this row, which is what makes two concurrent operations unable to spend
// the same margin.
func (s *Service) lockAccount(ctx context.Context, tx pgx.Tx, acct accounts.AccountID) (Account, error) {
	if err := bindingsImmediate(ctx, tx); err != nil {
		return Account{}, err
	}
	a, err := s.st.accountByCustomer(ctx, tx, acct, true)
	if err != nil {
		return Account{}, err
	}
	return a, s.requireOwnPolicy(a)
}

// Account returns the margin account of a customer account.
func (s *Service) Account(ctx context.Context, q db.Querier, acct accounts.AccountID) (Account, error) {
	return s.st.accountByCustomer(ctx, q, acct, false)
}

// State returns the engine's view of an account now.
func (s *Service) State(ctx context.Context, q db.Querier, acct accounts.AccountID) (AccountState, error) {
	a, err := s.st.accountByCustomer(ctx, q, acct, false)
	if err != nil {
		return AccountState{}, err
	}
	st, _, err := s.loadState(ctx, q, a, s.now())
	return st, err
}
