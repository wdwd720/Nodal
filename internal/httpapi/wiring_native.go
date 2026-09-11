package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Adapters for the Nodal-native economy.
//
// They are where transaction boundaries live: the domain services take a
// pgx.Tx because a Credit movement, its provenance and its journal posting are
// one atomic act, and the HTTP layer above them knows nothing about that.
//
// They are also where deployment facts are resolved. A handler is given an
// account id and an amount; the payout policy, the caller's verification level
// and the active capability set are properties of the deployment, and asking
// the client for them would let the client choose them.

// NativeEconomyDeps are the domain services behind the internal economy. Each
// is optional: a deployment that has not provisioned the internal economy
// leaves them nil and the corresponding routes answer UNSUPPORTED.
type NativeEconomyDeps struct {
	Credits       *credit.Service
	NativeAssets  *nativeasset.Service
	NativeMarkets *nativemarket.Service
	Payouts       *payout.Service
	PayoutEngine  *payout.Engine
	Commerce      *commerce.Service

	// CreditPurchases sells Credits for fiat. Nil means this deployment has no
	// payment provider configured, and the purchase endpoints answer
	// UNSUPPORTED rather than pretending -- which is the correct answer for a
	// deployment that literally cannot take a payment.
	CreditPurchases *credit.PurchaseService

	// PayoutPolicy is the deployment's current payout policy. Nil means the
	// fail-closed default, under which no origin may be withdrawn.
	PayoutPolicy *valuedomain.Policy
	// Capabilities reports which conversion and payout capabilities are
	// ACTIVE. Nil means none are, which is the correct reading for a fresh
	// deployment.
	Capabilities CapabilityResolver
	// Verification reports an account's financial verification level. Nil
	// means NONE: a deployment that cannot establish identity has not
	// established it.
	Verification VerificationResolver
	// LegalRouter evaluates the deployment's machine-readable capability
	// policy. Nil means the conservative default, under which the only thing
	// anybody may do is simulate -- see wiring_compiler.go.
	LegalRouter *legalrouter.Router
	// Jurisdiction reports which jurisdiction's rules apply to an account.
	// Nil means UNKNOWN, which the conservative policy denies. It is never
	// inferred from an IP address here.
	Jurisdiction JurisdictionResolver
	// Clock is used by the compiler for deadline checks. Nil falls back to
	// the system clock.
	Clock clock.Clock
	// KillSwitches pre-checks the emergency controls at the boundary, before a
	// quote is consumed or a provider is called. Nil skips the pre-check and
	// changes nothing about the authoritative one, which every domain service
	// makes inside its own transaction -- see preCheckKillSwitches.
	KillSwitches KillSwitchPreChecker
}

// KillSwitchPreChecker answers whether an active switch blocks an action, from
// an in-process snapshot at most one second old (*killswitch.CachedChecker).
//
// It is a PRE-check by name because POLICY_AUTHORITY §2 permits a cache to
// serve only pre-checks: the authoritative answer is Checker.Check with the
// authorizing transaction's own Querier, which is where internal/payout and the
// other domain services make it.
type KillSwitchPreChecker interface {
	PreCheck(ctx context.Context, a killswitch.Action) error
}

// now is the compiler's notion of the present.
func (d NativeEconomyDeps) now() time.Time {
	if d.Clock != nil {
		return d.Clock.Now()
	}
	return clock.System().Now()
}

// conservativeRouterOnce builds the fail-closed default policy once. It cannot
// fail -- ConservativePolicy is validated by its own test -- and a panic here
// would mean the fail-closed default is unusable, which is worse than any
// refusal it could produce.
var (
	conservativeOnce   sync.Once
	conservativeRouter *legalrouter.Router
)

func conservativeRouterOnce() *legalrouter.Router {
	conservativeOnce.Do(func() {
		r, err := legalrouter.New(legalrouter.ConservativePolicy())
		if err != nil {
			panic("httpapi: the conservative legal policy does not validate: " + err.Error())
		}
		conservativeRouter = r
	})
	return conservativeRouter
}

// newRouteIntentID derives a stable intent id for a compiled command. It is
// derived from the idempotency key rather than random so that recompiling the
// same command produces the same intent, which is what makes a stored routing
// decision comparable across a retry.
func newRouteIntentID(cc compileContext) string {
	sum := sha256.Sum256([]byte(string(cc.Action) + "|" + cc.AccountID.String() + "|" + cc.IdempotencyKey))
	return hex.EncodeToString(sum[:16])
}

// parseOptionalQuantity parses an exact base-unit amount, treating an empty
// string as "this action moves nothing".
func parseOptionalQuantity(s string) (*money.Quantity, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	q, err := money.ParseQuantity(s)
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// CapabilityResolver reports the currently ACTIVE capabilities.
type CapabilityResolver interface {
	Active(ctx context.Context) (map[valuedomain.CapabilityKey]bool, error)
}

// VerificationResolver reports an account's financial verification level,
// which is deliberately distinct from having a Nodal session (PART XLVII).
type VerificationResolver interface {
	Level(ctx context.Context, accountID accounts.AccountID) (valuedomain.VerificationLevel, error)
}

// policyOf returns the deployment's payout policy, defaulting to fail-closed.
func (d NativeEconomyDeps) policyOf() valuedomain.Policy {
	if d.PayoutPolicy != nil {
		return *d.PayoutPolicy
	}
	return valuedomain.DefaultPolicy()
}

func (d NativeEconomyDeps) capsOf(ctx context.Context) (map[valuedomain.CapabilityKey]bool, error) {
	if d.Capabilities == nil {
		return nil, nil
	}
	return d.Capabilities.Active(ctx)
}

func (d NativeEconomyDeps) verificationOf(ctx context.Context, accountID accounts.AccountID) (valuedomain.VerificationLevel, error) {
	if d.Verification == nil {
		return valuedomain.VerificationNone, nil
	}
	return d.Verification.Level(ctx, accountID)
}

// --- credits ---------------------------------------------------------------

type creditsAdapter struct {
	deps NativeEconomyDeps
	db   *db.DB
	clk  clock.Clock
}

func (a creditsAdapter) Balance(ctx context.Context, accountID accounts.AccountID) (credit.Balances, error) {
	caps, err := a.deps.capsOf(ctx)
	if err != nil {
		return credit.Balances{}, err
	}
	level, err := a.deps.verificationOf(ctx, accountID)
	if err != nil {
		return credit.Balances{}, err
	}
	return a.deps.Credits.Balances(ctx, a.db, credit.BalanceRequest{
		AccountID:  accountID,
		Policy:     a.deps.policyOf(),
		Verified:   level,
		ActiveCaps: caps,
		Now:        a.clk.Now(),
	})
}

// Pricing implements CreditsPort.
func (a creditsAdapter) Pricing(_ context.Context) (credit.PricingPolicy, error) {
	if a.deps.CreditPurchases == nil {
		return credit.PricingPolicy{}, errNotWired("credit purchases")
	}
	return a.deps.CreditPurchases.Pricing(), nil
}

// StartPurchase implements CreditsPort.
//
// The Credit quantity is decided inside PurchaseService from its pricing
// policy. Nothing on the way here could have carried one: the command has an
// amount of money and no quantity field, and neither does the request schema.
func (a creditsAdapter) StartPurchase(ctx context.Context, r StartCreditPurchase) (credit.StartedPurchase, error) {
	if a.deps.CreditPurchases == nil {
		return credit.StartedPurchase{}, errNotWired("credit purchases")
	}
	// No transaction is opened here. StartPurchase needs three of them, with
	// the provider call between the first and the third, and a caller that
	// wrapped the whole thing would put that call back inside a transaction --
	// which is the defect F-96 removed.
	out, err := a.deps.CreditPurchases.StartPurchase(ctx, a.db, credit.StartPurchaseRequest{
		AccountID:      r.AccountID,
		Amount:         money.USDFromMinor(r.AmountMinor),
		Currency:       r.Currency,
		IdempotencyKey: r.IdempotencyKey,
	})
	if err != nil {
		return credit.StartedPurchase{}, err
	}
	return out, nil
}

// Purchase implements CreditsPort.
func (a creditsAdapter) Purchase(ctx context.Context, id credit.FundingID) (credit.Funding, error) {
	if a.deps.CreditPurchases == nil {
		return credit.Funding{}, errNotWired("credit purchases")
	}
	return a.deps.Credits.Funding(ctx, a.db, id)
}

// --- native assets ---------------------------------------------------------

type nativeAssetsAdapter struct {
	deps NativeEconomyDeps
	db   *db.DB
}

func (a nativeAssetsAdapter) Create(ctx context.Context, r CreateNativeAsset) (nativeasset.Asset, nativeasset.Verdict, error) {
	// Publishing an asset is a Domain A action and goes through the compiler
	// like every other one. The conservative policy answers
	// REQUIRES_USER_CONFIRMATION for it at low agent authority, which the
	// compiler turns into a refusal here: an API call is not a confirmation.
	if _, cerr := a.deps.compileRoute(ctx, compileContext{
		Action: settlement.ActionCreateNativeAsset,
		Subject: settlement.Subject{
			Type: settlement.SubjectNativeAsset, ID: r.Symbol,
		},
		AccountID:      r.AccountID,
		IdempotencyKey: r.IdempotencyKey,
		CorrelationID:  r.CorrelationID,
	}); cerr != nil {
		return nativeasset.Asset{}, nativeasset.Verdict{}, cerr
	}
	var (
		asset   nativeasset.Asset
		verdict nativeasset.Verdict
	)
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var cerr error
		asset, verdict, cerr = a.deps.NativeAssets.CreateDraft(ctx, tx, nativeasset.CreateRequest{
			CreatorAccountID: r.AccountID,
			Name:             r.Name,
			Symbol:           r.Symbol,
			Description:      r.Description,
			ImageURL:         r.ImageURL,
			Decimals:         r.Decimals,
			Supply: nativeasset.SupplyModel{
				MaxSupply:         r.MaxSupply,
				CreatorAllocation: r.CreatorAllocation,
			},
		})
		return cerr
	})
	return asset, verdict, err
}

// Submit moves the creator's own DRAFT to PENDING_REVIEW.
//
// The ownership check is here rather than in internal/nativeasset because it is
// an authorization question about the CALLER, and the domain service is asked
// the same question by an operator path where the answer is different. What the
// domain refuses is the transition; what this refuses is the person.
func (a nativeAssetsAdapter) Submit(ctx context.Context, accountID accounts.AccountID, assetID assets.AssetID) (nativeasset.Asset, error) {
	var out nativeasset.Asset
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		current, gerr := a.deps.NativeAssets.Get(ctx, tx, assetID)
		if gerr != nil {
			return gerr
		}
		if current.CreatorAccountID != accountID {
			// Deliberately NOT_FOUND rather than FORBIDDEN. Telling a stranger
			// that an asset exists but is not theirs is a membership oracle,
			// and TestIDOR_ForeignAndAbsentAccountsAreIndistinguishable holds
			// the same line elsewhere.
			return errs.New(errs.CodeNotFound, "no such asset").
				WithField("asset_id", assetID.String())
		}
		var serr error
		out, serr = a.deps.NativeAssets.SetStatus(ctx, tx, assetID, nativeasset.StatusPendingReview,
			"submitted for review by its creator")
		return serr
	})
	return out, err
}

func (a nativeAssetsAdapter) Get(ctx context.Context, assetID assets.AssetID) (nativeasset.Asset, error) {
	return a.deps.NativeAssets.Get(ctx, a.db, assetID)
}

func (a nativeAssetsAdapter) ListTradable(ctx context.Context, limit int) ([]nativeasset.Asset, error) {
	return a.deps.NativeAssets.ListTradable(ctx, a.db, limit)
}

// --- native markets --------------------------------------------------------

type nativeMarketsAdapter struct {
	deps NativeEconomyDeps
	db   *db.DB
}

// TopHolderLimit is how many holders the market view returns. It is the
// concentration disclosure of PART LIV, and a short list is the honest one: a
// buyer needs to see whether one account holds most of it, not a directory.
const TopHolderLimit = 10

func (a nativeMarketsAdapter) Market(ctx context.Context, marketID nativemarket.MarketID) (MarketView, error) {
	m, err := a.deps.NativeMarkets.Market(ctx, a.db, marketID)
	if err != nil {
		return MarketView{}, err
	}
	st, err := a.deps.NativeMarkets.State(ctx, a.db, marketID)
	if err != nil {
		return MarketView{}, err
	}
	holders, err := a.deps.NativeMarkets.Holders(ctx, a.db, m.AssetID, TopHolderLimit)
	if err != nil {
		return MarketView{}, err
	}
	// The scale comes from the asset registry rather than a constant. A native
	// asset may be created with up to eighteen decimals; six is only the
	// default, and reading it is the difference between a quantity and a
	// quantity wrong by a factor of a million.
	asset, err := assets.NewRepository().Get(ctx, a.db, m.AssetID)
	if err != nil {
		return MarketView{}, err
	}
	return MarketView{Market: m, State: st, Holders: holders, AssetDecimals: int(asset.Decimals)}, nil
}

func (a nativeMarketsAdapter) Quote(ctx context.Context, r nativemarket.QuoteRequest) (NativeQuoteView, error) {
	var q nativemarket.Quote
	var decimals int
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var qerr error
		if q, qerr = a.deps.NativeMarkets.Quote(ctx, tx, r); qerr != nil {
			return qerr
		}
		decimals, qerr = a.assetDecimals(ctx, tx, r.MarketID)
		return qerr
	})
	return NativeQuoteView{Quote: q, AssetDecimals: decimals}, err
}

// assetDecimals reads the scale of the asset a market trades. It is read inside
// the caller's transaction so the figure describes the same market state the
// quote or fill was computed from.
func (a nativeMarketsAdapter) assetDecimals(ctx context.Context, q db.Querier, marketID nativemarket.MarketID) (int, error) {
	m, err := a.deps.NativeMarkets.Market(ctx, q, marketID)
	if err != nil {
		return 0, err
	}
	asset, err := assets.NewRepository().Get(ctx, q, m.AssetID)
	if err != nil {
		return 0, err
	}
	return int(asset.Decimals), nil
}

func (a nativeMarketsAdapter) Execute(ctx context.Context, r nativemarket.ExecuteRequest) (NativeExecuteView, error) {
	// The Settlement Compiler decides first. Everything below this line --
	// the market engine, the ledger's domain isolation, the invariant
	// triggers -- still runs; this is the layer that decides whether the
	// action is one this deployment permits at all, and it is the same layer
	// every other Domain A command passes through.
	m, err := a.deps.NativeMarkets.Market(ctx, a.db, r.MarketID)
	if err != nil {
		return NativeExecuteView{}, err
	}
	action := settlement.ActionBuyNativeAsset
	if r.Side == nativemarket.Sell {
		action = settlement.ActionSellNativeAsset
	}
	if _, cerr := a.deps.compileRoute(ctx, compileContext{
		Action: action,
		Subject: settlement.Subject{
			Type: settlement.SubjectNativeMarket, ID: r.MarketID.String(),
			AssetID: m.AssetID.String(),
		},
		AccountID:      r.AccountID,
		Amount:         r.Amount.String(),
		IdempotencyKey: r.IdempotencyKey,
		CorrelationID:  r.CorrelationID,
	}); cerr != nil {
		return NativeExecuteView{}, cerr
	}

	// The deployment's clock, not the client's. `EffectiveAt` is the instant a
	// posting is dated, so it is a deployment fact like the payout policy and
	// the capability set, and the adapter is where those are resolved.
	//
	// It was simply absent, and the consequence was total: `ExecuteRequest`
	// requires it, so every request to POST /native-markets/{id}/orders was
	// refused VALIDATION_FAILED "an order needs effective_at". The route had
	// never worked in any deployment. Nothing caught it because nothing drove
	// this route over HTTP -- the load script says in its own comment that it
	// does not trade, and the browser suite buys from the marketplace, which
	// goes through commerceAdapter, which does set it. F-37.
	r.EffectiveAt = a.deps.now()

	var res nativemarket.ExecuteResult
	var decimals int
	err = a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var eerr error
		if res, eerr = a.deps.NativeMarkets.Execute(ctx, tx, r); eerr != nil {
			return eerr
		}
		decimals, eerr = a.assetDecimals(ctx, tx, r.MarketID)
		return eerr
	})
	a.recordRiskRefusal(ctx, err)
	return NativeExecuteView{Result: res, AssetDecimals: decimals}, err
}

// recordRiskRefusal persists a risk REJECT that the trade's own transaction
// could not carry.
//
// A refused trade rolls back, and a decision written inside that transaction
// rolls back with it — so the deployment would keep every ALLOW and lose every
// REJECT, which is precisely backwards for an audit trail. The service hands
// the decision out with the error instead, and this writes it in a NEW
// transaction once the failed one is finished.
//
// It runs after InTx returns rather than inside it for the reason F-27 taught:
// taking a second pool connection while holding the first deadlocks the pool
// under concurrency.
//
// A failure to record is logged and does not change the answer. The trade was
// refused either way, and turning a correct RISK_CONCENTRATION into an INTERNAL
// would tell the user something false about their own request.
func (a nativeMarketsAdapter) recordRiskRefusal(ctx context.Context, err error) {
	var refusal *nativemarket.RiskRefusal
	if !errors.As(err, &refusal) {
		return
	}
	rerr := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		return a.deps.NativeMarkets.RecordRefusal(ctx, tx, refusal)
	})
	if rerr != nil {
		observability.LoggerFrom(ctx).ErrorContext(ctx, "risk decision could not be recorded",
			"error", rerr.Error(),
			"reason_codes", refusal.Decision.ReasonCodes,
			"decision_hash", refusal.Decision.Hash,
			"correlation_id", refusal.CorrelationID)
	}
}

// --- payouts ---------------------------------------------------------------

type payoutsAdapter struct {
	deps NativeEconomyDeps
	// withdrawal carries the legal registry reader: §48's disclosure is a
	// withdrawal-journey fact and lives with the rest of them.
	withdrawal WithdrawalDeps
	db         *db.DB
	clk        clock.Clock
}

func (a payoutsAdapter) Create(ctx context.Context, r CreatePayout) (payout.Request, payout.Decision, error) {
	// A payout is the action with the most ways to be wrong, so it passes
	// through the compiler before the eligibility engine rather than instead
	// of it. The compiler answers "may anyone here do this at all"; the
	// engine answers "which of THIS account's units may leave".
	destSubject := "NONE"
	if r.DestinationID != nil {
		destSubject = r.DestinationID.String()
	}
	if _, cerr := a.deps.compileRoute(ctx, compileContext{
		Action: settlement.ActionRequestPayout,
		Subject: settlement.Subject{
			Type: settlement.SubjectPayoutRequest, ID: destSubject,
		},
		AccountID:      r.AccountID,
		Amount:         r.Amount.String(),
		IdempotencyKey: r.IdempotencyKey,
		CorrelationID:  r.CorrelationID,
	}); cerr != nil {
		return payout.Request{}, payout.Decision{}, cerr
	}

	caps, err := a.deps.capsOf(ctx)
	if err != nil {
		return payout.Request{}, payout.Decision{}, err
	}
	level, err := a.deps.verificationOf(ctx, r.AccountID)
	if err != nil {
		return payout.Request{}, payout.Decision{}, err
	}

	// Destination and provider facts. A payout with no destination cannot be
	// eligible, and saying so here means the decision records WHY rather than
	// failing later with a foreign-key error.
	destinationVerified := false
	providerSupports := false
	terms, terr := a.deploymentTerms()
	if terr != nil {
		return payout.Request{}, payout.Decision{}, terr
	}
	if r.DestinationID != nil {
		dest, derr := a.deps.Payouts.Destination(ctx, a.db, *r.DestinationID)
		if derr != nil {
			return payout.Request{}, payout.Decision{}, derr
		}
		if dest.AccountID != r.AccountID {
			// NOT_FOUND, not FORBIDDEN. Every sibling -- DisableDestination,
			// conversionAdapter.Quote, GET /payouts/{id} since F-41 -- answers
			// NOT_FOUND for somebody else's row, and a distinguishable refusal
			// is a membership oracle: anyone could learn which destination ids
			// exist by asking (F-233, F-41's rule).
			return payout.Request{}, payout.Decision{}, errs.New(errs.CodeNotFound,
				"no such payout destination")
		}
		destinationVerified = dest.Status.Usable()
		providerSupports = a.providerSupports(dest)
		if p, perr := a.deps.Payouts.Provider(dest.Provider); perr == nil {
			terms = payout.TermsFrom(p.Capabilities())
		}
	}

	in := payout.EligibilityInput{
		Policy:              a.deps.policyOf(),
		Verified:            level,
		ActiveCaps:          caps,
		Now:                 a.clk.Now(),
		DestinationVerified: destinationVerified,
		ProviderSupports:    providerSupports,
	}

	var (
		req      payout.Request
		decision payout.Decision
	)
	err = a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		// The withdrawal disclosure, read in the reserving transaction. The
		// domain refuses without it (payout.disclosureRefusal); this is where
		// the fact comes from, and asking here rather than on the pool means
		// the answer cannot change between the question and the reservation.
		accepted, aerr := a.withdrawal.disclosureAccepted(ctx, tx, r.AccountID)
		if aerr != nil {
			return aerr
		}
		// The compliance facts, read from the same repository the eligibility
		// page reads and in the transaction that reserves the value, so an
		// account under an open sanctions review cannot be told it may withdraw
		// nothing and have its whole balance reserved anyway (F-226, D-120).
		facts, ferr := a.withdrawal.complianceFacts(ctx, tx, r.AccountID)
		if ferr != nil {
			return ferr
		}
		in.SanctionsState = facts.Sanctions
		in.AccountRestrictions = facts.Restrictions
		in.JurisdictionSupported = facts.JurisdictionSupported

		var cerr error
		req, decision, cerr = a.deps.Payouts.Create(ctx, tx, payout.CreateRequest{
			AccountID:          r.AccountID,
			DestinationID:      r.DestinationID,
			QuoteID:            r.QuoteID,
			Quantity:           r.Amount,
			ProviderTerms:      terms,
			Sandbox:            a.sandbox(),
			Environment:        a.withdrawal.Environment,
			DisclosureAccepted: accepted,
			IdempotencyKey:     r.IdempotencyKey,
			EffectiveAt:        a.clk.Now(),
			CorrelationID:      r.CorrelationID,
		}, in)
		return cerr
	})
	return req, decision, err
}

// sandbox is whether a conversion request created now is a rehearsal: either
// the provider is one, or the deployment is (ADR-0023). It is recorded on the
// request rather than asked when somebody reads it (F-232).
func (a payoutsAdapter) sandbox() bool {
	if a.withdrawal.SandboxTier {
		return true
	}
	names := a.deps.Payouts.ProviderNames()
	if len(names) != 1 {
		return false
	}
	p, err := a.deps.Payouts.Provider(names[0])
	if err != nil {
		return false
	}
	return p.Capabilities().Availability == payout.AvailabilitySandbox
}

// deploymentTerms are the published terms of the one payout provider this
// deployment runs, for the case where the request names no destination to
// resolve a provider from.
//
// A deployment with no provider has no terms, and a payout judged against no
// terms is a payout judged against nothing: the refusal is the honest state of a
// system with no conversion contract (BLOCKERS B-01, B-06).
func (a payoutsAdapter) deploymentTerms() (payout.ProviderTerms, error) {
	names := a.deps.Payouts.ProviderNames()
	if len(names) != 1 {
		return payout.ProviderTerms{}, errs.New(errs.CodeProviderUnavailable,
			"this deployment has no payout provider, so there is nothing a payout could be priced against")
	}
	p, err := a.deps.Payouts.Provider(names[0])
	if err != nil {
		return payout.ProviderTerms{}, err
	}
	return payout.TermsFrom(p.Capabilities()), nil
}

// providerSupports asks the configured provider whether it can actually pay
// this destination. PART LXXVI: never infer a capability from marketing copy,
// and never from the fact that a destination row exists.
//
// It asks the SAME question the destination was registered against, from the
// values the destination stored, rather than re-deriving a weaker one. It used
// to check the kind and the currency and stop there -- so a destination in a
// country the provider had since stopped paying, or one in an excluded
// subdivision, was still "supported" at the moment value would leave (F-228,
// D-122). The provider's own answer is the only one worth having, and it is
// free to ask.
func (a payoutsAdapter) providerSupports(d payout.Destination) bool {
	return providerSupportsDestination(a.deps, d)
}

// providerSupportsDestination is the same question asked from anywhere that
// holds the economy dependencies.
//
// It is a function rather than a method because the QUOTE asks it too, and
// asked it as the literal `true` -- three lines under a comment promising that
// the provenance shown beside a quote is the provenance the commit would
// consume (F-269). Two call sites, one implementation, so they cannot drift.
func providerSupportsDestination(deps NativeEconomyDeps, d payout.Destination) bool {
	if deps.Payouts == nil {
		// No payout service wired at all. Nothing supports anything, which is
		// the answer that refuses rather than the one that proceeds.
		return false
	}
	p, err := deps.Payouts.Provider(d.Provider)
	if err != nil {
		return false
	}
	caps := p.Capabilities()
	if !caps.Supports(d.Kind) {
		return false
	}
	if d.Currency != "" && !caps.SupportsCurrency(d.Currency) {
		return false
	}
	ok, _ := caps.CanPayRecipient(payout.RecipientProfile{
		Kind: payout.RecipientKindIndividual, Country: d.Country, Region: d.Region,
	})
	return ok
}

// Cancel withdraws the account's own pending request.
//
// The ownership check is here, and it answers NOT_FOUND rather than FORBIDDEN
// for somebody else's payout, for the same reason every other account-scoped
// read does: a distinguishable refusal is a membership oracle.
func (a payoutsAdapter) Cancel(ctx context.Context, accountID accounts.AccountID, id payout.RequestID, reason string) (payout.Request, error) {
	var out payout.Request
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		current, gerr := a.deps.Payouts.Get(ctx, tx, id)
		if gerr != nil {
			return gerr
		}
		if current.AccountID != accountID {
			return errs.New(errs.CodeNotFound, "no such payout").WithField("payout_id", id.String())
		}
		var cerr error
		out, cerr = a.deps.Payouts.Cancel(ctx, tx, id, reason)
		return cerr
	})
	return out, err
}

func (a payoutsAdapter) Get(ctx context.Context, id payout.RequestID) (payout.Request, error) {
	return a.deps.Payouts.Get(ctx, a.db, id)
}

func (a payoutsAdapter) ListByAccount(ctx context.Context, accountID accounts.AccountID, limit int) ([]payout.Request, error) {
	return a.deps.Payouts.ListByAccount(ctx, a.db, accountID, limit)
}

// Provenance is what value a payout draws on, in the order it leaves (PART 23).
func (a payoutsAdapter) Provenance(ctx context.Context, id payout.RequestID) ([]payout.ProvenanceSlice, error) {
	return a.deps.Payouts.Provenance(ctx, a.db, id)
}

// SandboxProvider reports whether this deployment pays through a rehearsal
// provider. A deployment runs one payout slot, so "the provider" is
// unambiguous; with none configured the answer is false, because a payout that
// cannot happen is not a rehearsal of anything.
func (a payoutsAdapter) SandboxProvider() bool {
	names := a.deps.Payouts.ProviderNames()
	if len(names) != 1 {
		return false
	}
	p, err := a.deps.Payouts.Provider(names[0])
	if err != nil {
		return false
	}
	return p.Capabilities().Availability == payout.AvailabilitySandbox
}

// wireNativeEconomy attaches the internal-economy ports that have services
// behind them and leaves the rest nil.
func wireNativeEconomy(p *Ports, d WireDeps) {
	n := d.NativeEconomy
	// Unconditional, unlike everything below it. The compiler's inputs are
	// deployment policy rather than a service the deployment may not have, and
	// a deployment with none has made no determination rather than a
	// permissive one -- the zero value IS the conservative policy.
	p.SettlementPolicy = n
	if n.Credits != nil {
		p.Credits = creditsAdapter{deps: n, db: d.DB, clk: d.Clock}
	}
	if n.NativeAssets != nil {
		p.NativeAssets = nativeAssetsAdapter{deps: n, db: d.DB}
	}
	if n.NativeMarkets != nil {
		p.NativeMarkets = nativeMarketsAdapter{deps: n, db: d.DB}
	}
	if n.Payouts != nil {
		p.Payouts = payoutsAdapter{deps: n, withdrawal: d.Withdrawal, db: d.DB, clk: d.Clock}
	}
	if n.Commerce != nil {
		p.Commerce = commerceAdapter{svc: n.Commerce, db: d.DB, clk: d.Clock, deps: n}
	}
}
