package funding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/webhook"
)

// Database is the transaction runner and querier the service uses.
// *db.DB satisfies it.
type Database interface {
	db.Querier
	InTx(ctx context.Context, opts db.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error
}

// Ledger is what the service needs from internal/ledger: the posting
// service and a balance read. *ledger.Service satisfies it.
type Ledger interface {
	Post(ctx context.Context, tx pgx.Tx, p ledger.Posting) (ledger.PostResult, error)
	Balance(ctx context.Context, q db.Querier, ref ledger.AccountRef) (money.Quantity, error)
}

// Holds is what the service needs from internal/capital: withdrawal holds
// for reversibility windows. *capital.Service satisfies it.
type Holds interface {
	PlaceHold(ctx context.Context, tx pgx.Tx, h capital.WithdrawalHold) (capital.WithdrawalHold, error)
	ReleaseHold(ctx context.Context, tx pgx.Tx, hid capital.WithdrawalHoldID, releasedBy string) (capital.WithdrawalHold, error)
}

// Accounts is what the service needs from internal/accounts: status reads
// and the FROZEN transition on deficit. *accounts.Repository satisfies it.
type Accounts interface {
	Get(ctx context.Context, q db.Querier, accountID accounts.AccountID) (accounts.Account, error)
	Transition(ctx context.Context, tx pgx.Tx, accountID accounts.AccountID, ch accounts.StatusChange, now time.Time) (accounts.Account, error)
}

// Assets reads the asset registry (decimals, symbol, status).
// *assets.Repository satisfies it.
type Assets interface {
	Get(ctx context.Context, q db.Querier, assetID assets.AssetID) (assets.Asset, error)
}

// GateChecker is the capability-gate guard. *gates.Checker satisfies it.
type GateChecker interface {
	RequireActive(ctx context.Context, q db.Querier, cap gates.Capability) error
}

// KillSwitchChecker is the authoritative kill-switch check.
// *killswitch.Checker satisfies it.
type KillSwitchChecker interface {
	Check(ctx context.Context, q db.Querier, a killswitch.Action) error
}

// ReconcilePolicy bounds the provider-vs-chain quantity comparison
// (RECONCILIATION §5): the two must agree within ToleranceBPS of the
// provider-reported amount (provider network fees, rounding). An unknown
// provider amount never reconciles automatically.
type ReconcilePolicy struct {
	ToleranceBPS money.BPS
}

// AvailabilityPolicy is applied when a reconciled deposit becomes
// AVAILABLE (PART 27). HoldDuration is how long a withdrawal hold binds the
// settled quantity (0 means ReversibleFor); BuyingPowerEligibleAt delays
// buying-power eligibility (zero means immediately); ReversibleFor is the
// provider reversibility window after which withdrawal eligibility may be
// granted. Version names the policy in the deposit row.
type AvailabilityPolicy struct {
	Version               string
	HoldDuration          time.Duration
	BuyingPowerEligibleAt time.Time
	ReversibleFor         time.Duration
}

// Validate rejects negative durations and an empty version.
func (p AvailabilityPolicy) Validate() error {
	switch {
	case strings.TrimSpace(p.Version) == "":
		return errs.New(errs.CodeValidationFailed, "funding: availability policy version is required")
	case p.HoldDuration < 0 || p.ReversibleFor < 0:
		return errs.New(errs.CodeValidationFailed, "funding: availability policy durations must not be negative")
	}
	return nil
}

// Config is the static configuration of the Service.
type Config struct {
	Env          config.Environment
	ProviderMode config.ProviderMode
	Reconcile    ReconcilePolicy
	Availability AvailabilityPolicy
	// SessionTTL is how long a deposit may sit in CREATED, SESSION_CREATED
	// or CUSTOMER_ACTION_REQUIRED before ExpireStale marks it EXPIRED.
	SessionTTL time.Duration
	// SettlementTimeout is how long a PROVIDER_CONFIRMED deposit may wait
	// for a chain receipt before the driver escalates to REVIEW_REQUIRED.
	SettlementTimeout time.Duration
}

// Deps are the collaborators of the Service. Every field is required.
type Deps struct {
	DB           Database
	Clock        clock.Clock
	Repo         *Repository
	Provider     FundingProvider
	Ledger       Ledger
	Holds        Holds
	Accounts     Accounts
	Assets       Assets
	Gates        GateChecker
	KillSwitches KillSwitchChecker
}

// Service implements the funding use cases. It holds no mutable state.
type Service struct {
	cfg Config
	d   Deps
}

// Settler is the fixed settlement contract (FINANCIAL_MODEL §7).
type Settler interface {
	RecordSettlement(ctx context.Context, tx pgx.Tx, id DepositID, observed money.Quantity, sig string, slot int64) error
	MarkAvailable(ctx context.Context, tx pgx.Tx, id DepositID, policy AvailabilityPolicy) error
	Reverse(ctx context.Context, tx pgx.Tx, id DepositID, reason, evidence string) (ReversalResult, error)
}

var (
	_ Settler                          = (*Service)(nil)
	_ webhook.Dispatcher[WebhookEvent] = (*Service)(nil)
)

// NewService validates the configuration and dependencies. It refuses a
// fake provider outside LOCAL/TEST/DEV and a missing gate or kill-switch
// checker (both fail closed by absence at wiring time, never at runtime).
func NewService(cfg Config, d Deps) (*Service, error) {
	if !cfg.Env.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "funding: unknown environment %q", cfg.Env)
	}
	if !cfg.ProviderMode.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "funding: unknown provider mode %q", cfg.ProviderMode)
	}
	if cfg.ProviderMode == config.ProviderModeFake && !cfg.Env.AllowsFakeProviders() {
		return nil, errs.Newf(errs.CodeValidationFailed, "funding: fake provider is not allowed in %s", cfg.Env)
	}
	if cfg.Reconcile.ToleranceBPS < 0 || cfg.Reconcile.ToleranceBPS > 10_000 {
		return nil, errs.New(errs.CodeValidationFailed, "funding: reconcile tolerance must be within 0..10000 bps")
	}
	if err := cfg.Availability.Validate(); err != nil {
		return nil, err
	}
	if cfg.SessionTTL <= 0 || cfg.SettlementTimeout <= 0 {
		return nil, errs.New(errs.CodeValidationFailed, "funding: session ttl and settlement timeout must be positive")
	}
	switch {
	case d.DB == nil, d.Clock == nil, d.Repo == nil, d.Provider == nil, d.Ledger == nil,
		d.Holds == nil, d.Accounts == nil, d.Assets == nil, d.Gates == nil, d.KillSwitches == nil:
		return nil, errs.New(errs.CodeValidationFailed, "funding: every dependency is required")
	}
	return &Service{cfg: cfg, d: d}, nil
}

// Config returns the service configuration.
func (s *Service) Config() Config { return s.cfg }

// Provider returns the wired funding provider.
func (s *Service) Provider() FundingProvider { return s.d.Provider }

// Repo returns the deposit repository.
func (s *Service) Repo() *Repository { return s.d.Repo }

// RequiresLiveFundingGate reports whether Start consults the LIVE_FUNDING
// capability gate: always in STAGING/PROD, and everywhere the provider mode
// is live. Fake and sandbox providers in LOCAL/TEST/DEV move no real money
// and are exempt.
func (s *Service) RequiresLiveFundingGate() bool {
	if s.cfg.ProviderMode == config.ProviderModeLive {
		return true
	}
	return !s.cfg.Env.AllowsFakeProviders()
}

// authError maps internal/security sentinels onto stable codes.
func authError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, security.ErrUnauthenticated):
		return errs.Wrap(err, errs.CodeUnauthenticated, "authentication required")
	case errors.Is(err, security.ErrStepUpRequired):
		return errs.Wrap(err, errs.CodeStepUpRequired, "step-up authentication required")
	case errors.Is(err, security.ErrCrossTenant):
		return errs.Wrap(err, errs.CodeForbidden, "account is not accessible to this principal")
	default:
		return errs.Wrap(err, errs.CodeForbidden, "forbidden")
	}
}

// StartRequest is the customer's "add funds" command.
type StartRequest struct {
	AccountID           accounts.AccountID
	AssetID             assets.AssetID // settlement asset the deposit must deliver (USDC)
	DestinationNetwork  string         // provider network name, e.g. "solana"
	DestinationCurrency string         // provider currency name, e.g. "usdc"; must match the asset symbol
	DestinationAddress  string         // the customer's embedded wallet address
	DestinationWalletID string         // uuid text of the wallet row, when known
	FiatCurrency        string         // "usd" or "eur"
	FiatAmountMinor     *int64         // optional fixed fiat amount in minor units
	CustomerIPAddress   string
	Customer            *CustomerInformation
	IdempotencyKey      string
	CorrelationID       string
	RequestID           string
}

// Validate checks the request shape.
func (r StartRequest) Validate() error {
	problems := map[string]any{}
	if r.AccountID.IsZero() {
		problems["account_id"] = "required"
	}
	if r.AssetID.IsZero() {
		problems["asset_id"] = "required"
	}
	if strings.TrimSpace(r.DestinationNetwork) == "" {
		problems["destination_network"] = "required"
	}
	if strings.TrimSpace(r.DestinationCurrency) == "" {
		problems["destination_currency"] = "required"
	}
	if strings.TrimSpace(r.DestinationAddress) == "" {
		problems["destination_address"] = "required"
	}
	switch strings.ToLower(r.FiatCurrency) {
	case "usd", "eur":
	default:
		problems["fiat_currency"] = "must be usd or eur"
	}
	if r.FiatAmountMinor != nil && *r.FiatAmountMinor <= 0 {
		problems["fiat_amount_minor"] = "must be positive"
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		problems["idempotency_key"] = "required"
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "funding: invalid start request").WithFields(problems)
	}
	return nil
}

// StartResult is the outcome of Start. Session.ClientSecret is handed to
// the customer exactly once and is never stored.
type StartResult struct {
	Deposit  Deposit
	Session  Session
	Replayed bool
}

// Start creates a deposit and its provider session (PART 162 steps 1–2).
// It requires funding:create on the account, an ACTIVE account, an asset
// that accepts new exposure, the LIVE_FUNDING gate where
// RequiresLiveFundingGate, and no blocking kill switch (NEW_RISK with the
// funding flag). The deposit is persisted CREATED before the provider is
// called (the provider receives the deposit id as its idempotency key), so
// a retried command with the same key never mints a second session. The
// provider call runs outside any database transaction.
func (s *Service) Start(ctx context.Context, principal security.Principal, req StartRequest) (StartResult, error) {
	ctx = security.WithPrincipal(ctx, principal)
	if err := security.Require(ctx, security.PermFundingCreate); err != nil {
		return StartResult{}, authError(err)
	}
	if err := req.Validate(); err != nil {
		return StartResult{}, err
	}
	if err := security.RequireAccount(ctx, req.AccountID.String()); err != nil {
		return StartResult{}, authError(err)
	}
	actor := TransitionEvidence{
		ActorType: principal.ActorType, ActorID: principal.SubjectID, Reason: "customer requested funding",
		CorrelationID: req.CorrelationID, RequestID: req.RequestID,
	}
	if err := actor.Validate(); err != nil {
		return StartResult{}, err
	}

	var (
		deposit Deposit
		replay  bool
	)
	err := s.d.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}, func(ctx context.Context, tx pgx.Tx) error {
		existing, found, err := s.d.Repo.GetByIdempotencyKey(ctx, tx, req.IdempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if existing.AccountID != req.AccountID {
				return errs.New(errs.CodeInvalidIdempotencyReuse, "funding: idempotency key belongs to another deposit")
			}
			if existing.Status != StatusCreated {
				deposit, replay = existing, true
				return nil
			}
		}
		if err := s.authorizeNewFunding(ctx, tx, req); err != nil {
			return err
		}
		d, _, err := s.d.Repo.Create(ctx, tx, CreateDeposit{
			AccountID: req.AccountID, Provider: s.d.Provider.Name(), ExpectedAssetID: req.AssetID,
			FiatAmountMinor: req.FiatAmountMinor, FiatCurrency: req.FiatCurrency,
			DestinationWalletID: req.DestinationWalletID, DestinationAddress: req.DestinationAddress,
			IdempotencyKey: req.IdempotencyKey, CorrelationID: req.CorrelationID,
		}, actor)
		if err != nil {
			return err
		}
		deposit = d
		return nil
	})
	if err != nil {
		return StartResult{}, err
	}
	if replay {
		session, err := s.d.Provider.GetSession(ctx, deposit.ProviderSessionID)
		if err != nil {
			return StartResult{}, err
		}
		return StartResult{Deposit: deposit, Session: session, Replayed: true}, nil
	}

	session, err := s.d.Provider.CreateSession(ctx, s.sessionRequest(deposit, req))
	if err != nil {
		return StartResult{}, s.failSessionCreation(ctx, deposit, actor, err)
	}
	if session.ID == "" {
		return StartResult{}, errs.New(errs.CodeProviderUnavailable, "funding: provider returned a session without an id")
	}
	err = s.d.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := s.d.Repo.GetForUpdate(ctx, tx, deposit.ID)
		if err != nil {
			return err
		}
		if cur.Status != StatusCreated {
			// A concurrent Start with the same key bound the same provider
			// session (the provider deduplicated on the deposit id).
			if cur.ProviderSessionID == session.ID {
				deposit, replay = cur, true
				return nil
			}
			return errs.New(errs.CodeConflict, "funding: deposit changed while the provider session was being created").
				WithField("deposit_id", deposit.ID.String()).WithField("status", string(cur.Status))
		}
		patch := Patch{ProviderSessionID: &session.ID}
		if session.DestinationAmount != "" {
			if q, ok := s.expectedQuantity(ctx, tx, cur, session); ok {
				patch.ExpectedQuantity = &q
			}
		}
		ev := actor
		ev.Reason = "provider session created"
		ev.EvidenceRef = "provider_session:" + session.ID
		d, err := s.d.Repo.TransitionWithPatch(ctx, tx, deposit.ID, StatusSessionCreated, ev, patch)
		if err != nil {
			return err
		}
		deposit = d
		return nil
	})
	if err != nil {
		return StartResult{}, err
	}
	return StartResult{Deposit: deposit, Session: session, Replayed: replay}, nil
}

// authorizeNewFunding runs the account, asset, gate and kill-switch checks
// inside the authorizing transaction.
func (s *Service) authorizeNewFunding(ctx context.Context, tx pgx.Tx, req StartRequest) error {
	acct, err := s.d.Accounts.Get(ctx, tx, req.AccountID)
	if err != nil {
		return err
	}
	if !acct.Status.AllowsNewRisk() {
		code := errs.CodeForbidden
		if acct.Status == accounts.StatusFrozen {
			code = errs.CodeAccountFrozen
		}
		return errs.Newf(code, "account status %s does not allow new funding", acct.Status).WithField("account_status", string(acct.Status))
	}
	asset, err := s.d.Assets.Get(ctx, tx, req.AssetID)
	if err != nil {
		return err
	}
	if !asset.Status.AllowsIncreasingExposure() {
		return errs.Newf(errs.CodeAssetRestricted, "asset %s is %s", asset.Symbol, asset.Status).WithField("asset_status", string(asset.Status))
	}
	if !strings.EqualFold(asset.Symbol, req.DestinationCurrency) {
		return errs.New(errs.CodeValidationFailed, "funding: destination currency does not match the settlement asset").
			WithField("destination_currency", req.DestinationCurrency).WithField("asset_symbol", asset.Symbol)
	}
	if s.RequiresLiveFundingGate() {
		if err := s.d.Gates.RequireActive(ctx, tx, gates.LiveFunding); err != nil {
			return err
		}
	}
	return s.d.KillSwitches.Check(ctx, tx, killswitch.Action{
		Class: killswitch.NewRisk, AccountID: req.AccountID.String(), Provider: s.d.Provider.Name(), Funding: true,
	})
}

func (s *Service) sessionRequest(d Deposit, req StartRequest) CreateSessionRequest {
	out := CreateSessionRequest{
		IdempotencyKey:      d.ID.String(),
		DestinationNetwork:  strings.ToLower(req.DestinationNetwork),
		DestinationCurrency: strings.ToLower(req.DestinationCurrency),
		WalletAddress:       req.DestinationAddress,
		LockWalletAddress:   true,
		SourceCurrency:      strings.ToLower(req.FiatCurrency),
		CustomerIPAddress:   req.CustomerIPAddress,
		Customer:            req.Customer,
		Metadata:            map[string]string{"deposit_id": d.ID.String(), "account_id": d.AccountID.String()},
	}
	if req.FiatAmountMinor != nil {
		out.SourceAmount = formatMinor2(*req.FiatAmountMinor)
	}
	return out
}

// formatMinor2 renders minor units of a two-decimal fiat currency as the
// decimal string providers expect ("100.00").
func formatMinor2(minor int64) string {
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

// failSessionCreation records a definitive provider rejection as FAILED
// and leaves a retryable failure in CREATED so the same idempotency key can
// retry. Either way the provider error is returned.
func (s *Service) failSessionCreation(ctx context.Context, d Deposit, actor TransitionEvidence, cause error) error {
	switch errs.CodeOf(cause) {
	case errs.CodeProviderUnavailable, errs.CodeRateLimited, errs.CodeInternal, errs.CodeIdempotencyInProgress:
		return cause
	}
	ev := actor
	ev.ActorType, ev.ActorID = security.ActorSystem, SystemActorID
	ev.Reason = "provider rejected session creation: " + string(errs.CodeOf(cause))
	err := s.d.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.d.Repo.Transition(ctx, tx, d.ID, StatusFailed, ev)
		return err
	})
	if err != nil {
		observability.LoggerFrom(ctx).ErrorContext(ctx, "funding: could not record provider rejection",
			"deposit_id", d.ID.String(), "error", err.Error())
	}
	return cause
}

// expectedQuantity converts the provider's destination amount to base units
// of the deposit's asset. ok is false when the amount is unusable (wrong
// currency, precision beyond the asset, unparsable); callers escalate.
func (s *Service) expectedQuantity(ctx context.Context, q db.Querier, d Deposit, session Session) (money.Quantity, bool) {
	asset, err := s.d.Assets.Get(ctx, q, d.ExpectedAssetID)
	if err != nil {
		return money.Quantity{}, false
	}
	if session.DestinationCurrency != "" && !strings.EqualFold(session.DestinationCurrency, asset.Symbol) {
		return money.Quantity{}, false
	}
	qty, err := ParseDecimalAmount(session.DestinationAmount, asset.Decimals)
	if err != nil || !qty.IsPositive() {
		return money.Quantity{}, false
	}
	return qty, true
}

// ApplyOutcome classifies ApplyProviderEvent's result.
type ApplyOutcome string

// Outcomes.
const (
	// ApplyApplied: the deposit changed state.
	ApplyApplied ApplyOutcome = "APPLIED"
	// ApplyNoOp: the event concerned a known deposit but did not advance it
	// (stale or repeated status, terminal or operator-owned deposit).
	ApplyNoOp ApplyOutcome = "NO_OP"
	// ApplyIgnored: the event is not about a deposit this platform owns.
	ApplyIgnored ApplyOutcome = "IGNORED"
)

// ApplyResult is the result of ApplyProviderEvent.
type ApplyResult struct {
	Outcome ApplyOutcome
	Deposit Deposit
	Reason  string
}

// ApplyProviderEvent applies a verified provider event to its deposit
// inside the inbox transaction the webhook pipeline opened (PART 30 step
// 8). Provider statuses map through MapProviderStatus; statuses are applied
// monotonically (a stale or repeated status is a no-op); an unknown status,
// a regression after PROVIDER_CONFIRMED, a mismatched wallet or currency,
// or an unrepresentable amount moves the deposit to REVIEW_REQUIRED. It
// never returns an error for a status it does not understand.
func (s *Service) ApplyProviderEvent(ctx context.Context, tx pgx.Tx, ev WebhookEvent) (ApplyResult, error) {
	if !ev.SessionKnown {
		return ApplyResult{Outcome: ApplyIgnored, Reason: "event type not modeled: " + ev.Identity.EventType}, nil
	}
	if ev.Identity.Provider != s.d.Provider.Name() {
		return ApplyResult{Outcome: ApplyIgnored, Reason: "event from another provider: " + ev.Identity.Provider}, nil
	}
	if ev.Session.ID == "" {
		return ApplyResult{Outcome: ApplyIgnored, Reason: "event carries no session id"}, nil
	}
	d, err := s.d.Repo.GetByProviderSession(ctx, tx, ev.Identity.Provider, ev.Session.ID)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return ApplyResult{Outcome: ApplyIgnored, Reason: "unknown provider session"}, nil
		}
		return ApplyResult{}, err
	}
	evidence := SystemEvidence("provider status "+ev.Session.RawStatus, "provider_event:"+ev.Identity.EventID, d.CorrelationID)
	evidence.Detail = map[string]any{"provider_event_id": ev.Identity.EventID, "provider_event_type": ev.Identity.EventType, "provider_status": ev.Session.RawStatus}

	if d.Status.Final() || d.Status == StatusReviewRequired {
		return ApplyResult{Outcome: ApplyNoOp, Deposit: d, Reason: "deposit is " + string(d.Status)}, nil
	}
	target, known := MapProviderStatus(ev.Session.Status)
	if !known {
		return s.escalate(ctx, tx, d, evidence, "unknown provider status "+ev.Session.RawStatus)
	}
	if target == StatusFailed {
		if CanTransition(d.Status, StatusFailed) {
			return s.apply(ctx, tx, d, StatusFailed, evidence, Patch{})
		}
		return s.escalate(ctx, tx, d, evidence, "provider reported rejection after confirmation")
	}
	curRank, _ := d.Status.Rank()
	targetRank, _ := target.Rank()
	if targetRank <= curRank {
		return ApplyResult{Outcome: ApplyNoOp, Deposit: d, Reason: "provider status does not advance the deposit"}, nil
	}
	if !CanTransition(d.Status, target) {
		return s.escalate(ctx, tx, d, evidence, fmt.Sprintf("provider status %s cannot follow %s", ev.Session.RawStatus, d.Status))
	}
	patch := Patch{}
	if target == StatusProviderConfirmed {
		if ev.Session.WalletAddress != "" && ev.Session.WalletAddress != d.DestinationAddress {
			return s.escalate(ctx, tx, d, evidence, "provider wallet address differs from the deposit destination")
		}
		if ev.Session.TransactionID != "" {
			ref := ev.Session.TransactionID
			patch.ProviderRef = &ref
		}
		if ev.Session.DestinationAmount == "" {
			return s.escalate(ctx, tx, d, evidence, "provider confirmed without a destination amount")
		}
		q, ok := s.expectedQuantity(ctx, tx, d, ev.Session)
		if !ok {
			return s.escalate(ctx, tx, d, evidence, "provider destination amount or currency is not usable for the settlement asset")
		}
		patch.ExpectedQuantity = &q
	}
	return s.apply(ctx, tx, d, target, evidence, patch)
}

func (s *Service) apply(ctx context.Context, tx pgx.Tx, d Deposit, to Status, ev TransitionEvidence, p Patch) (ApplyResult, error) {
	updated, err := s.d.Repo.TransitionWithPatch(ctx, tx, d.ID, to, ev, p)
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Outcome: ApplyApplied, Deposit: updated, Reason: ev.Reason}, nil
}

// escalate moves a deposit to REVIEW_REQUIRED with the reason; when even
// that is not a legal transition the deposit is left untouched and the
// reason is reported as a no-op (never an error that would fail the inbox).
func (s *Service) escalate(ctx context.Context, tx pgx.Tx, d Deposit, ev TransitionEvidence, reason string) (ApplyResult, error) {
	ev.Reason = reason
	if !CanTransition(d.Status, StatusReviewRequired) {
		observability.LoggerFrom(ctx).WarnContext(ctx, "funding: provider event needs review but deposit cannot be escalated",
			"deposit_id", d.ID.String(), "status", string(d.Status), "reason", reason)
		return ApplyResult{Outcome: ApplyNoOp, Deposit: d, Reason: reason}, nil
	}
	updated, err := s.d.Repo.Transition(ctx, tx, d.ID, StatusReviewRequired, ev)
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Outcome: ApplyApplied, Deposit: updated, Reason: reason}, nil
}

// Dispatch implements webhook.Dispatcher for the ingestion pipeline.
func (s *Service) Dispatch(ctx context.Context, tx pgx.Tx, ev WebhookEvent) (webhook.Disposition, error) {
	res, err := s.ApplyProviderEvent(ctx, tx, ev)
	if err != nil {
		return "", err
	}
	if res.Outcome == ApplyIgnored {
		return webhook.Ignored, nil
	}
	return webhook.Applied, nil
}

// RecordSettlement records an observed chain receipt: PROVIDER_CONFIRMED →
// SETTLEMENT_OBSERVED with the observed quantity, signature and slot
// (RECONCILIATION §5). Only an observed receipt reaches this state.
func (s *Service) RecordSettlement(ctx context.Context, tx pgx.Tx, depositID DepositID, observed money.Quantity, sig string, slot int64) error {
	if !observed.IsPositive() {
		return errs.New(errs.CodeValidationFailed, "funding: observed quantity must be positive")
	}
	if strings.TrimSpace(sig) == "" {
		return errs.New(errs.CodeValidationFailed, "funding: chain signature is required")
	}
	if slot < 0 {
		return errs.New(errs.CodeValidationFailed, "funding: chain slot must not be negative")
	}
	d, err := s.d.Repo.GetForUpdate(ctx, tx, depositID)
	if err != nil {
		return err
	}
	ev := SystemEvidence("chain receipt observed", "chain_tx:"+sig, d.CorrelationID)
	ev.Detail = map[string]any{"observed_quantity": observed.String(), "tx_signature": sig, "chain_slot": slot}
	_, err = s.d.Repo.TransitionWithPatch(ctx, tx, depositID, StatusSettlementObserved, ev,
		Patch{ObservedQuantity: &observed, TxSignature: &sig, ChainSlot: &slot})
	return err
}

// ReconcileResult reports Reconcile's decision.
type ReconcileResult struct {
	Deposit    Deposit
	Agreed     bool
	Expected   money.Quantity
	Observed   money.Quantity
	Difference money.Quantity // |expected − observed|
	Tolerance  money.Quantity
}

// Reconcile compares the provider-reported quantity with the observed chain
// credit (SETTLEMENT_OBSERVED). Within ReconcilePolicy tolerance it posts
// FUNDING_SETTLED for the observed quantity (idempotent on
// "deposit:<id>:settled"), stores the journal transaction id and moves the
// deposit to RECONCILED; otherwise, or when the provider amount is unknown,
// it moves the deposit to REVIEW_REQUIRED and reports Agreed=false without
// error. Never blocked by kill switches (SETTLE class).
func (s *Service) Reconcile(ctx context.Context, tx pgx.Tx, depositID DepositID) (ReconcileResult, error) {
	d, err := s.d.Repo.GetForUpdate(ctx, tx, depositID)
	if err != nil {
		return ReconcileResult{}, err
	}
	if d.Status != StatusSettlementObserved {
		return ReconcileResult{}, errs.Newf(errs.CodeInvalidStateTransition, "deposit is %s, not SETTLEMENT_OBSERVED", d.Status).
			WithField("deposit_id", depositID.String()).WithField("status", string(d.Status))
	}
	if d.ObservedQuantity == nil || !d.ObservedQuantity.IsPositive() {
		return ReconcileResult{}, errs.New(errs.CodeInternal, "funding: SETTLEMENT_OBSERVED deposit has no observed quantity")
	}
	observed := *d.ObservedQuantity
	ev := SystemEvidence("provider and chain agree", "chain_tx:"+d.TxSignature, d.CorrelationID)
	if d.ExpectedQuantity == nil {
		ev.Reason = "provider amount unknown; cannot reconcile automatically"
		ev.Detail = map[string]any{"observed_quantity": observed.String()}
		updated, err := s.d.Repo.Transition(ctx, tx, depositID, StatusReviewRequired, ev)
		if err != nil {
			return ReconcileResult{}, err
		}
		return ReconcileResult{Deposit: updated, Observed: observed}, nil
	}
	expected := *d.ExpectedQuantity
	diff := expected.Sub(observed).Abs()
	tolerance := expected.MulBPS(s.cfg.Reconcile.ToleranceBPS, money.RoundDown)
	res := ReconcileResult{Expected: expected, Observed: observed, Difference: diff, Tolerance: tolerance}
	ev.Detail = map[string]any{
		"expected_quantity": expected.String(), "observed_quantity": observed.String(),
		"difference": diff.String(), "tolerance": tolerance.String(),
	}
	if diff.Cmp(tolerance) > 0 {
		ev.Reason = "provider and chain quantities disagree beyond tolerance"
		updated, err := s.d.Repo.Transition(ctx, tx, depositID, StatusReviewRequired, ev)
		if err != nil {
			return ReconcileResult{}, err
		}
		res.Deposit = updated
		return res, nil
	}
	posting, err := ledger.FundingSettledPosting(ledger.FundingInputs{
		AccountID: d.AccountID, DepositID: d.ID.String(), AssetID: d.ExpectedAssetID, Quantity: observed,
		USD: fiatUSD(d), EffectiveAt: s.d.Clock.Now(), CorrelationID: d.CorrelationID,
	})
	if err != nil {
		return ReconcileResult{}, err
	}
	posted, err := s.d.Ledger.Post(ctx, tx, posting)
	if err != nil {
		return ReconcileResult{}, err
	}
	ev.EvidenceRef = "journal_transaction:" + posted.TransactionID.String()
	updated, err := s.d.Repo.TransitionWithPatch(ctx, tx, depositID, StatusReconciled, ev, Patch{JournalTransactionID: &posted.TransactionID})
	if err != nil {
		return ReconcileResult{}, err
	}
	res.Deposit, res.Agreed = updated, true
	return res, nil
}

// fiatUSD returns the deposit's fiat amount as USD valuation metadata when
// the fiat currency is USD, nil otherwise.
func fiatUSD(d Deposit) *money.USD {
	if d.FiatAmountMinor == nil || !strings.EqualFold(d.FiatCurrency, "usd") {
		return nil
	}
	u := money.USDFromMinor(*d.FiatAmountMinor)
	return &u
}

// HoldReasonReversibilityWindow is the withdrawal_holds.reason of the hold
// MarkAvailable places for the provider reversibility window.
const HoldReasonReversibilityWindow = "REVERSIBILITY_WINDOW"

// MarkAvailable applies the availability policy to a RECONCILED deposit:
// AVAILABLE with buying_power_eligible per policy, reversible_until = now +
// ReversibleFor, withdrawal_eligible false until the window has passed
// (PromoteWithdrawalEligibility), and a withdrawal hold on the settled
// quantity for the hold duration. A zero ReversibleFor grants withdrawal
// eligibility immediately when the fraud state allows it.
func (s *Service) MarkAvailable(ctx context.Context, tx pgx.Tx, depositID DepositID, policy AvailabilityPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	d, err := s.d.Repo.GetForUpdate(ctx, tx, depositID)
	if err != nil {
		return err
	}
	if d.Status != StatusReconciled {
		return errs.Newf(errs.CodeInvalidStateTransition, "deposit is %s, not RECONCILED", d.Status).
			WithField("deposit_id", depositID.String()).WithField("status", string(d.Status))
	}
	if d.JournalTransactionID == nil || d.ObservedQuantity == nil {
		return errs.New(errs.CodeInternal, "funding: RECONCILED deposit lacks its posting or observed quantity")
	}
	now := s.d.Clock.Now().UTC()
	reversibleUntil := now.Add(policy.ReversibleFor)
	buyingPower := policy.BuyingPowerEligibleAt.IsZero() || !now.Before(policy.BuyingPowerEligibleAt)
	withdrawal := policy.ReversibleFor == 0 && d.FraudState.AllowsWithdrawal()
	holdFor := policy.HoldDuration
	if holdFor == 0 {
		holdFor = policy.ReversibleFor
	}
	ev := SystemEvidence("availability policy "+policy.Version+" applied", "journal_transaction:"+d.JournalTransactionID.String(), d.CorrelationID)
	ev.Detail = map[string]any{
		"policy_version": policy.Version, "reversible_until": reversibleUntil.Format(time.RFC3339Nano),
		"buying_power_eligible": buyingPower, "withdrawal_eligible": withdrawal,
	}
	if holdFor > 0 {
		expires := now.Add(holdFor)
		hold, err := s.d.Holds.PlaceHold(ctx, tx, capital.WithdrawalHold{
			AccountID: d.AccountID, AssetID: d.ExpectedAssetID, Quantity: *d.ObservedQuantity,
			Reason: HoldReasonReversibilityWindow, DepositID: d.ID.String(), ExpiresAt: &expires,
		})
		if err != nil {
			return err
		}
		ev.Detail["withdrawal_hold_id"] = hold.ID.String()
	}
	version := policy.Version
	_, err = s.d.Repo.TransitionWithPatch(ctx, tx, depositID, StatusAvailable, ev, Patch{
		ReversibleUntil: &reversibleUntil, BuyingPowerEligible: &buyingPower, WithdrawalEligible: &withdrawal,
		AvailabilityPolicyVersion: &version,
	})
	return err
}

// ReversalResult reports what Reverse posted and did.
type ReversalResult struct {
	DepositID      DepositID
	AccountID      accounts.AccountID
	Amount         money.Quantity
	Covered        money.Quantity
	Shortfall      money.Quantity
	TransactionIDs []ledger.TransactionID
	Deficit        bool
	AccountFrozen  bool
}

// Security event kinds written by Reverse.
const (
	SecurityEventFundingReversed        = "funding_reversed"
	SecurityEventNegativeDeficitAccount = "negative_deficit_accounts"
)

// Reverse applies a provider reversal (chargeback) to an AVAILABLE or
// RECONCILED deposit (FINANCIAL_MODEL §2.2, PART 27). It reads the current
// WALLET balance, posts the compensating transactions built by
// ledger.FundingReversalPostings (T1 covered part, T2 shortfall to the
// credit-normal DEFICIT account), releases the deposit's reversibility
// holds, moves the deposit to REVERSED with fraud_state CONFIRMED_FRAUD and
// no eligibility flags, writes a funding_reversed security event and, on a
// deficit, freezes the account and writes the negative_deficit_accounts
// alert row. The outbox carries funding.reversed and the audit trail the
// full amounts. Reversal is compensation and is never blocked by kill
// switches.
func (s *Service) Reverse(ctx context.Context, tx pgx.Tx, depositID DepositID, reason, evidence string) (ReversalResult, error) {
	if strings.TrimSpace(reason) == "" {
		return ReversalResult{}, errs.New(errs.CodeValidationFailed, "funding: reversal reason is required")
	}
	d, err := s.d.Repo.GetForUpdate(ctx, tx, depositID)
	if err != nil {
		return ReversalResult{}, err
	}
	if d.Status != StatusAvailable && d.Status != StatusReconciled {
		return ReversalResult{}, errs.Newf(errs.CodeInvalidStateTransition, "deposit is %s; only AVAILABLE or RECONCILED deposits can be reversed", d.Status).
			WithField("deposit_id", depositID.String()).WithField("status", string(d.Status))
	}
	if d.JournalTransactionID == nil || d.ObservedQuantity == nil || !d.ObservedQuantity.IsPositive() {
		return ReversalResult{}, errs.New(errs.CodeInternal, "funding: deposit has no settled posting to reverse")
	}
	amount := *d.ObservedQuantity
	wallet, err := s.d.Ledger.Balance(ctx, tx, ledger.CustomerAccount(d.AccountID, ledger.CodeWallet, d.ExpectedAssetID))
	if err != nil {
		return ReversalResult{}, err
	}
	now := s.d.Clock.Now().UTC()
	postings, err := ledger.FundingReversalPostings(ledger.FundingReversalInputs{
		AccountID: d.AccountID, DepositID: d.ID.String(), AssetID: d.ExpectedAssetID, USD: fiatUSD(d),
		EffectiveAt: now, CorrelationID: d.CorrelationID, Reason: reason,
	}, wallet, amount)
	if err != nil {
		return ReversalResult{}, err
	}
	res := ReversalResult{DepositID: d.ID, AccountID: d.AccountID, Amount: amount, Covered: wallet.Min(amount), Shortfall: amount.Sub(wallet.Min(amount))}
	res.Deficit = res.Shortfall.IsPositive()
	for _, p := range postings {
		posted, err := s.d.Ledger.Post(ctx, tx, p)
		if err != nil {
			return ReversalResult{}, err
		}
		res.TransactionIDs = append(res.TransactionIDs, posted.TransactionID)
	}
	if len(res.TransactionIDs) == 0 {
		return ReversalResult{}, errs.New(errs.CodeInternal, "funding: reversal produced no postings")
	}
	if err := s.releaseDepositHolds(ctx, tx, d); err != nil {
		return ReversalResult{}, err
	}
	txIDs := make([]string, 0, len(res.TransactionIDs))
	for _, tid := range res.TransactionIDs {
		txIDs = append(txIDs, tid.String())
	}
	ev := SystemEvidence(reason, evidence, d.CorrelationID)
	ev.Detail = map[string]any{
		"amount": amount.String(), "wallet_balance_before": wallet.String(), "covered": res.Covered.String(),
		"shortfall": res.Shortfall.String(), "deficit": res.Deficit, "journal_transaction_ids": txIDs,
	}
	fraud, off := FraudConfirmed, false
	if _, err := s.d.Repo.TransitionWithPatch(ctx, tx, depositID, StatusReversed, ev, Patch{
		ReversalJournalTransactionID: &res.TransactionIDs[0], FraudState: &fraud, BuyingPowerEligible: &off, WithdrawalEligible: &off,
	}); err != nil {
		return ReversalResult{}, err
	}
	detail := map[string]any{
		"deposit_id": d.ID.String(), "asset_id": d.ExpectedAssetID.String(), "amount": amount.String(),
		"covered": res.Covered.String(), "shortfall": res.Shortfall.String(), "reason": reason, "evidence_ref": evidence,
		"journal_transaction_ids": txIDs,
	}
	if err := insertSecurityEvent(ctx, tx, SecurityEventFundingReversed, "HIGH", d.AccountID, detail, d.CorrelationID, now); err != nil {
		return ReversalResult{}, err
	}
	if !res.Deficit {
		return res, nil
	}
	acct, err := s.d.Accounts.Get(ctx, tx, d.AccountID)
	if err != nil {
		return ReversalResult{}, err
	}
	if acct.Status != accounts.StatusFrozen && acct.Status != accounts.StatusClosed {
		if _, err := s.d.Accounts.Transition(ctx, tx, d.AccountID, accounts.StatusChange{
			To: accounts.StatusFrozen, ActorType: string(security.ActorSystem), ActorID: SystemActorID,
			Reason: "funding reversal deficit on deposit " + d.ID.String(), CorrelationID: d.CorrelationID,
		}, now); err != nil {
			return ReversalResult{}, err
		}
		res.AccountFrozen = true
	}
	detail["account_frozen"] = res.AccountFrozen
	if err := insertSecurityEvent(ctx, tx, SecurityEventNegativeDeficitAccount, "CRITICAL", d.AccountID, detail, d.CorrelationID, now); err != nil {
		return ReversalResult{}, err
	}
	return res, nil
}

// releaseDepositHolds releases every active withdrawal hold tied to the
// deposit: the reversed quantity is gone, so the hold would only
// over-restrict what remains.
func (s *Service) releaseDepositHolds(ctx context.Context, tx pgx.Tx, d Deposit) error {
	rows, err := tx.Query(ctx, `SELECT id FROM withdrawal_holds WHERE deposit_id = $1 AND released_at IS NULL ORDER BY created_at, id`, d.ID)
	if err != nil {
		return dbErr("list deposit holds", err)
	}
	var ids []capital.WithdrawalHoldID
	for rows.Next() {
		var hid capital.WithdrawalHoldID
		if err := rows.Scan(&hid); err != nil {
			rows.Close()
			return dbErr("scan hold id", err)
		}
		ids = append(ids, hid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return dbErr("iterate deposit holds", err)
	}
	for _, hid := range ids {
		if _, err := s.d.Holds.ReleaseHold(ctx, tx, hid, SystemActorID); err != nil {
			return err
		}
	}
	return nil
}

// insertSecurityEvent writes one security_events row (PART 130) inside tx.
func insertSecurityEvent(ctx context.Context, tx pgx.Tx, kind, severity string, accountID accounts.AccountID, detail map[string]any, requestID string, now time.Time) error {
	body, err := json.Marshal(detail)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "funding: encode security event")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO security_events (id, kind, severity, account_id, detail, request_id, occurred_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7)`,
		id.New[id.Any](), kind, severity, accountID, body, requestID, now.UTC()); err != nil {
		return dbErr("insert security event", err)
	}
	return nil
}

// ExpireStale moves deposits stuck in CREATED, SESSION_CREATED or
// CUSTOMER_ACTION_REQUIRED for longer than SessionTTL to EXPIRED (user
// abandoned the provider flow). It returns the expired ids.
func (s *Service) ExpireStale(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]DepositID, error) {
	cutoff := now.UTC().Add(-s.cfg.SessionTTL)
	ids, err := s.d.Repo.LockDue(ctx, tx, []Status{StatusCreated, StatusSessionCreated, StatusCustomerActionRequired}, cutoff, limit)
	if err != nil {
		return nil, err
	}
	ev := SystemEvidence("session ttl elapsed without customer completion", "", "")
	ev.Detail = map[string]any{"session_ttl": s.cfg.SessionTTL.String(), "cutoff": cutoff.Format(time.RFC3339Nano)}
	for _, depositID := range ids {
		if _, err := s.d.Repo.Transition(ctx, tx, depositID, StatusExpired, ev); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// PromoteWithdrawalEligibility sets withdrawal_eligible on AVAILABLE
// deposits whose reversible window has passed with a clean fraud state
// (PART 27: distinct from buying-power eligibility). It returns the ids it
// changed.
func (s *Service) PromoteWithdrawalEligibility(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]DepositID, error) {
	ids, err := s.d.Repo.LockWithdrawalCandidates(ctx, tx, now, limit)
	if err != nil {
		return nil, err
	}
	ev := SystemEvidence("reversible window elapsed with clean fraud state", "", "")
	var changed []DepositID
	for _, depositID := range ids {
		if _, ok, err := s.d.Repo.SetWithdrawalEligible(ctx, tx, depositID, now, ev); err != nil {
			return nil, err
		} else if ok {
			changed = append(changed, depositID)
		}
	}
	return changed, nil
}

// GetDeposit returns a deposit to a principal holding funding:read on its
// account.
func (s *Service) GetDeposit(ctx context.Context, depositID DepositID) (Deposit, error) {
	if err := security.Require(ctx, security.PermFundingRead); err != nil {
		return Deposit{}, authError(err)
	}
	d, err := s.d.Repo.Get(ctx, s.d.DB, depositID)
	if err != nil {
		return Deposit{}, err
	}
	if err := security.RequireAccount(ctx, d.AccountID.String()); err != nil {
		// Indistinguishable from a missing deposit (tenant scoping).
		return Deposit{}, errs.New(errs.CodeNotFound, "deposit not found").WithField("deposit_id", depositID.String())
	}
	return d, nil
}

// ListDeposits lists an account's deposits for a principal holding
// funding:read on it.
func (s *Service) ListDeposits(ctx context.Context, accountID accounts.AccountID, cursor string, limit int) ([]Deposit, string, error) {
	if err := security.Require(ctx, security.PermFundingRead); err != nil {
		return nil, "", authError(err)
	}
	if err := security.RequireAccount(ctx, accountID.String()); err != nil {
		return nil, "", authError(err)
	}
	return s.d.Repo.ListByAccount(ctx, s.d.DB, accountID, cursor, limit)
}
