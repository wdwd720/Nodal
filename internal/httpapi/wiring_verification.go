package httpapi

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/eligibility"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/terms"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// Adapters for the withdrawal journey.
//
// They are where deployment facts are resolved, exactly as the native-economy
// adapters are: a handler is given an account id, and the payout policy, the
// verification level, the active capability set and the provider's answer are
// properties of the deployment. Asking the client for any of them would let the
// client choose them.

// WithdrawalDeps are the services behind verification, eligibility and the
// conversion request. Each is optional; a nil service leaves its port nil and
// the routes answer UNSUPPORTED.
type WithdrawalDeps struct {
	// Verification is the identity boundary. Nil means this deployment has no
	// identity vendor configured, which is the honest state of a deployment
	// with no contract, and every verification route answers UNSUPPORTED.
	Verification *verification.Service
	// BaseVerification is the level Nodal establishes by itself -- a verified
	// e-mail address on an ACTIVE account. The profile view composes the
	// evidence rule on top of it, so this is deliberately the NODAL_IDENTITY
	// resolver and not the composite one, which would be circular.
	BaseVerification verification.BaseResolver
	// Compliance reads the attribute half of the profile for the eligibility
	// explanation: restrictions, jurisdiction, sanctions state.
	Compliance *compliance.Repository
	// Accounts is read for the account status, because a frozen account
	// withdraws nothing whatever its Credits say.
	Accounts *accounts.Repository
	// Pricing converts Credits to money for the quote. It comes from the
	// deployment's Credit purchase service so the payout side and the purchase
	// side use one rate.
	Pricing *credit.PurchaseService
	// CreditDecimals is the scale of the Credit asset. It is resolved once at
	// wiring time from the asset registry rather than assumed to be six.
	CreditDecimals uint8
	// Terms reports which legal documents a person still owes. It is what
	// answers §48's withdrawal disclosure: the document is required at the
	// moment somebody asks to take value out and deliberately not at signup,
	// so it is read here rather than at onboarding.
	//
	// Nil means nothing can be established, and nothing established means the
	// disclosure is outstanding: a deployment that has not wired the legal
	// registry has not obtained anybody's agreement to it, and that is the
	// direction a mistake must fall.
	Terms TermsOutstanding
	// Environment is written onto every quote.
	Environment string
	// SandboxTier is cfg.SandboxTier(), carried so a quote can be labelled a
	// rehearsal even when the provider itself does not say so.
	SandboxTier bool
}

// TermsOutstanding reports the documents required at one point in the journey
// that a person has not accepted at the bytes now served.
//
// It is an interface rather than *profile.Service so this package does not
// acquire the profile domain for one question, and so a test can answer it
// without a database.
type TermsOutstanding interface {
	Outstanding(ctx context.Context, q db.Querier, userID string, r terms.Requirement) ([]terms.DocumentID, error)
}

// disclosureAccepted reports whether this account's owner has accepted the
// current WITHDRAWAL_DISCLOSURE.
//
// It takes the caller's querier so the question is asked of the same snapshot
// the payout is being decided in: reading it on the pool while the decision
// runs in a transaction would be two answers to one question.
//
// Every failure path answers false. An error reading the acceptances is not
// consent, and neither is an unwired registry.
func (w WithdrawalDeps) disclosureAccepted(ctx context.Context, q db.Querier, accountID accounts.AccountID) (bool, error) {
	if w.Terms == nil {
		return false, nil
	}
	var owner accounts.UserID
	if err := q.QueryRow(ctx, `SELECT owner_user_id FROM accounts WHERE id = $1`, accountID).Scan(&owner); err != nil {
		return false, errs.Wrap(err, errs.CodeInternal, "terms: read account owner")
	}
	outstanding, err := w.Terms.Outstanding(ctx, q, owner.String(), terms.BeforeWithdrawal)
	if err != nil {
		return false, err
	}
	return len(outstanding) == 0, nil
}

// accountFacts are the compliance facts every decision on the withdrawal
// journey is made against.
//
// One reader, because there used to be none: GET /v1/me/eligibility read the
// restrictions, the jurisdiction verdict and the sanctions screen and refused on
// them, and the conversion path read no compliance column at all -- so an
// account under an open sanctions review was told it could withdraw nothing
// while payout.Service.Create reserved its whole balance and Submit settled it
// (F-226, D-120). Two surfaces answering one question from two readers is the
// defect; this is the one reader.
type accountFacts struct {
	// Restrictions are the codes recorded against the compliance profile, plus
	// the sanctions screen when it is not clear, in the form
	// eligibility.ExplainWithdrawal expects.
	Restrictions []string
	// Sanctions is the screening decision itself, passed on unflattened so the
	// payout engine can read it the way this adapter does.
	Sanctions compliance.SanctionsState
	// JurisdictionSupported is the verification rule table's verdict.
	JurisdictionSupported bool
}

// complianceFacts reads them, on the caller's querier so the answer belongs to
// the same snapshot the decision is made in.
//
// A deployment with no compliance repository establishes nothing, and nothing
// established is not permission: the zero value blocks in both engines, which is
// the direction a mistake here has to fall.
func (w WithdrawalDeps) complianceFacts(ctx context.Context, q db.Querier, accountID accounts.AccountID) (accountFacts, error) {
	var out accountFacts
	if w.Compliance == nil {
		return out, nil
	}
	var owner accounts.UserID
	if err := q.QueryRow(ctx, `SELECT owner_user_id FROM accounts WHERE id = $1`, accountID).Scan(&owner); err != nil {
		// An absent account is answered NOT_FOUND by the account read that
		// every caller makes first; anything here is an infrastructure failure
		// and must not be reported as "no restrictions".
		return accountFacts{}, errs.Wrap(err, errs.CodeInternal, "compliance: read account owner")
	}
	profile, err := w.Compliance.Get(ctx, q, owner)
	if err != nil && errs.CodeOf(err) != errs.CodeNotFound {
		return accountFacts{}, err
	}
	out.Restrictions = append([]string(nil), profile.Restrictions...)
	out.Sanctions = profile.SanctionsState
	out.JurisdictionSupported, _ = rules.CheckJurisdiction(rules.Jurisdiction{
		Country: profile.JurisdictionCountry, Region: profile.JurisdictionRegion,
	})
	if profile.SanctionsState == compliance.SanctionsHit || profile.SanctionsState == compliance.SanctionsReview {
		// A sanctions hit or an open review is an account-level restriction in
		// every sense that matters here, and reporting it as one is what makes
		// the explanation match what the payout engine will actually do.
		out.Restrictions = append(out.Restrictions, "SANCTIONS_"+string(profile.SanctionsState))
	}
	return out, nil
}

// wireWithdrawal attaches the verification, eligibility and conversion ports
// that have services behind them and leaves the rest nil.
func wireWithdrawal(p *Ports, d WireDeps) {
	w := d.Withdrawal
	if w.Verification != nil {
		p.Verification = verificationAdapter{svc: w.Verification, base: w.BaseVerification, db: d.DB}
	}
	// The eligibility explanation needs the Credit ledger and the payout
	// policy. Without Credits there is nothing to explain, and answering with
	// an empty breakdown would read as "you hold nothing" rather than "this
	// deployment has no internal economy".
	if d.NativeEconomy.Credits != nil {
		p.Eligibility = eligibilityAdapter{
			deps: d.NativeEconomy, withdrawal: w, db: d.DB, clk: d.Clock,
		}
	}
	if d.NativeEconomy.Payouts != nil {
		p.Conversion = conversionAdapter{
			deps: d.NativeEconomy, withdrawal: w, db: d.DB, clk: d.Clock,
		}
	}
}

// --- verification -----------------------------------------------------------

type verificationAdapter struct {
	svc  *verification.Service
	base verification.BaseResolver
	db   *db.DB
}

func (a verificationAdapter) SandboxTier() bool { return a.svc.SandboxTier() }

// Profile assembles the §24 view. The base level comes from the same resolver
// the payout engine reads, so the profile screen and the payout decision can
// never disagree about what somebody has established.
func (a verificationAdapter) Profile(ctx context.Context, accountID accounts.AccountID) (verification.Snapshot, error) {
	base, err := a.baseLevel(ctx, accountID)
	if err != nil {
		return verification.Snapshot{}, err
	}
	return a.svc.Snapshot(ctx, a.db, accountID, base)
}

// baseLevel is the level Nodal establishes by itself. It is deliberately the
// NODAL_IDENTITY resolver rather than the composite one: composing the
// composite with itself would be circular, and the snapshot applies the
// evidence rule on top.
//
// A deployment with no base resolver reports NONE, under which no level is
// reachable at all. That is the correct reading: if nothing can say the account
// is in good standing, nothing should say it is verified.
func (a verificationAdapter) baseLevel(ctx context.Context, accountID accounts.AccountID) (valuedomain.VerificationLevel, error) {
	if a.base == nil {
		return valuedomain.VerificationNone, nil
	}
	return a.base.Level(ctx, accountID)
}

func (a verificationAdapter) Start(ctx context.Context, r StartVerification) (verification.Started, error) {
	return a.svc.Start(ctx, a.db, verification.StartRequest{
		AccountID:     r.AccountID,
		Purpose:       r.Purpose,
		Jurisdiction:  r.Jurisdiction,
		CorrelationID: r.CorrelationID,
	})
}

func (a verificationAdapter) Poll(ctx context.Context, accountID accounts.AccountID, sessionID verification.SessionID) (verification.Session, error) {
	return a.svc.Poll(ctx, a.db, accountID, sessionID)
}

func (a verificationAdapter) SandboxOutcome(ctx context.Context, accountID accounts.AccountID, outcome verification.SandboxOutcome) (verification.Session, error) {
	return a.svc.SetSandboxOutcome(ctx, a.db, accountID, outcome)
}

// --- eligibility ------------------------------------------------------------

type eligibilityAdapter struct {
	deps       NativeEconomyDeps
	withdrawal WithdrawalDeps
	db         *db.DB
	clk        clock.Clock
}

// Withdrawal composes the four independent decisions into the answer §19 asks
// for. Every input is read here and the arithmetic is done by the pure function
// in internal/eligibility, so the explanation can be reproduced from its inputs.
func (a eligibilityAdapter) Withdrawal(ctx context.Context, accountID accounts.AccountID) (eligibility.WithdrawalExplanation, error) {
	var zero eligibility.WithdrawalExplanation
	now := a.clk.Now().UTC()

	caps, err := a.deps.capsOf(ctx)
	if err != nil {
		return zero, err
	}
	level, err := a.deps.verificationOf(ctx, accountID)
	if err != nil {
		return zero, err
	}
	policy := a.deps.policyOf()

	balances, err := a.deps.Credits.Balances(ctx, a.db, credit.BalanceRequest{
		AccountID:  accountID,
		Policy:     policy,
		Verified:   level,
		ActiveCaps: caps,
		Now:        now,
	})
	if err != nil {
		return zero, err
	}
	lots, err := a.deps.Credits.Lots(ctx, a.db, accountID)
	if err != nil {
		return zero, err
	}

	in := eligibility.WithdrawalInput{
		Policy:         policy,
		Verified:       level,
		ActiveCaps:     caps,
		Holdings:       foldHoldings(lots, now),
		PolicyValid:    policy.Validate() == nil,
		Gross:          balances.Gross,
		Spendable:      balances.Spendable,
		Frozen:         balances.Frozen,
		PayoutEligible: balances.PayoutEligible,
		Sandbox:        a.withdrawal.SandboxTier,
	}
	accepted, err := a.withdrawal.disclosureAccepted(ctx, a.db, accountID)
	if err != nil {
		return zero, err
	}
	in.DisclosureAccepted = accepted
	if err := a.applyAccountFacts(ctx, accountID, &in); err != nil {
		return zero, err
	}
	a.applyProviderFacts(ctx, accountID, &in)
	return eligibility.ExplainWithdrawal(in), nil
}

// applyAccountFacts reads the jurisdiction verdict, the account status and the
// restrictions. A profile that does not exist is not an error: somebody who has
// never been asked to verify has an unknown jurisdiction, and an unknown
// jurisdiction fails closed in the rule table.
func (a eligibilityAdapter) applyAccountFacts(ctx context.Context, accountID accounts.AccountID, in *eligibility.WithdrawalInput) error {
	if a.withdrawal.Accounts != nil {
		acct, err := a.withdrawal.Accounts.Get(ctx, a.db, accountID)
		if err != nil {
			return err
		}
		in.AccountFrozen = acct.Status != accounts.StatusActive
	}
	facts, err := a.withdrawal.complianceFacts(ctx, a.db, accountID)
	if err != nil {
		return err
	}
	in.AccountRestrictions = facts.Restrictions
	in.JurisdictionSupported = facts.JurisdictionSupported
	return nil
}

// applyProviderFacts asks the configured payout provider whether it could pay
// this person at all, and what its minimum is in Credits.
//
// A deployment with no provider reports PROVIDER_UNAVAILABLE rather than
// silently omitting the reason. That is the honest state of a system with no
// conversion contract (BLOCKERS B-01, B-06) and it is not the person's fault.
func (a eligibilityAdapter) applyProviderFacts(ctx context.Context, accountID accounts.AccountID, in *eligibility.WithdrawalInput) {
	destinations, err := a.deps.Payouts.DestinationsByAccount(ctx, a.db, accountID, 50)
	if err != nil {
		return
	}
	var usable *payout.Destination
	for i := range destinations {
		if destinations[i].Status.Usable() {
			usable = &destinations[i]
			break
		}
	}
	in.DestinationConfigured = usable != nil

	name := ""
	if usable != nil {
		name = usable.Provider
	} else if names := a.deps.Payouts.ProviderNames(); len(names) == 1 {
		name = names[0]
	}
	if name == "" {
		return
	}
	provider, perr := a.deps.Payouts.Provider(name)
	if perr != nil {
		return
	}
	caps := provider.Capabilities()
	in.ProviderName = provider.Name()
	in.ProviderAvailable = caps.Availability.Usable()
	if usable != nil {
		in.ProviderAvailable = in.ProviderAvailable && caps.Supports(usable.Kind)
	}
	if min := caps.MinimumAmount; min.Minor() > 0 && a.withdrawal.Pricing != nil {
		pricing := a.withdrawal.Pricing.Pricing()
		if qty, cerr := creditsForMinor(min.Minor(), a.withdrawal.CreditDecimals,
			pricing.CreditsPerMajorUnit, pricing.MinorUnitsPerMajorUnit); cerr == nil {
			in.MinimumQuantity = qty
		}
	}
}

// foldHoldings turns lots into per-origin buckets.
//
// Two deliberately conservative choices, both stated on eligibility's type: the
// bucket's finality is the LEAST final among its lots, and its age is that of
// the YOUNGEST. A bucket containing one reversible lot is not wholly final, and
// a bucket clears a hold period only when all of it does. Both choices make a
// mixed bucket refuse rather than permit, which is the direction a mistake here
// should fall.
func foldHoldings(lots []credit.Lot, now time.Time) []eligibility.OriginHolding {
	type acc struct {
		qty      money.Quantity
		finality valuedomain.FundingFinality
		age      int
		seen     bool
	}
	byOrigin := map[valuedomain.CreditOrigin]*acc{}
	for _, l := range lots {
		if !l.Remaining.IsPositive() {
			continue
		}
		a, ok := byOrigin[l.Origin]
		if !ok {
			a = &acc{}
			byOrigin[l.Origin] = a
		}
		a.qty = a.qty.Add(l.Remaining)
		age := l.AgeDays(now)
		if !a.seen || age < a.age {
			a.age = age
		}
		if !a.seen || finalityRank(l.Finality) < finalityRank(a.finality) {
			a.finality = l.Finality
		}
		a.seen = true
	}
	out := make([]eligibility.OriginHolding, 0, len(byOrigin))
	for _, origin := range valuedomain.AllOrigins() {
		a, ok := byOrigin[origin]
		if !ok {
			continue
		}
		out = append(out, eligibility.OriginHolding{
			Origin: origin, Quantity: a.qty, Finality: a.finality, HeldDays: a.age,
		})
	}
	return out
}

// finalityRank orders finalities from least final to most. Lower is worse, so
// taking the minimum takes the worst.
func finalityRank(f valuedomain.FundingFinality) int {
	switch f {
	case valuedomain.FinalityReversed:
		return 0
	case valuedomain.FinalityDisputed:
		return 1
	case valuedomain.FinalityReversible:
		return 2
	case valuedomain.FinalityUnfunded:
		return 3
	case valuedomain.FinalitySettled:
		return 4
	}
	// An undeclared finality is worse than every declared one: Policy.Permits
	// reads it as UNKNOWN_FUNDING_FINALITY and refuses.
	return -1
}

// creditsForMinor converts an amount of money in minor units into Credits,
// rounded UP so a minimum expressed in Credits is never below the provider's
// real minimum.
func creditsForMinor(minor int64, decimals uint8, creditsPerMajor, minorPerMajor int64) (money.Quantity, error) {
	if creditsPerMajor <= 0 || minorPerMajor <= 0 {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "no Credit pricing policy")
	}
	scale := money.QuantityFromInt64(1).ScaleUp(decimals)
	num := money.QuantityFromInt64(creditsPerMajor).Mul(scale)
	return money.QuantityFromInt64(minor).MulDiv(num, money.QuantityFromInt64(minorPerMajor), money.RoundUp)
}

// --- the conversion request -------------------------------------------------

type conversionAdapter struct {
	deps       NativeEconomyDeps
	withdrawal WithdrawalDeps
	db         *db.DB
	clk        clock.Clock
}

func (a conversionAdapter) Destinations(ctx context.Context, accountID accounts.AccountID, limit int) ([]payout.Destination, error) {
	return a.deps.Payouts.DestinationsByAccount(ctx, a.db, accountID, limit)
}

// AddDestination registers a provider token.
//
// The provider is the deployment's configured one, not the client's choice: a
// destination naming a provider this deployment does not have could never be
// paid, and letting the request pick one would be letting it pick where money
// goes.
func (a conversionAdapter) AddDestination(ctx context.Context, r AddPayoutDestination) (payout.Destination, error) {
	names := a.deps.Payouts.ProviderNames()
	if len(names) != 1 {
		return payout.Destination{}, errs.New(errs.CodeProviderUnavailable,
			"this deployment has no payout provider, so there is nowhere a destination could send value")
	}
	provider, err := a.deps.Payouts.Provider(names[0])
	if err != nil {
		return payout.Destination{}, err
	}
	caps := provider.Capabilities()
	if !caps.Supports(r.Kind) {
		return payout.Destination{}, errs.Newf(errs.CodeProviderUnavailable,
			"the payout provider %q does not pay a %s destination", provider.Name(), r.Kind)
	}
	if r.Currency != "" && !caps.SupportsCurrency(r.Currency) {
		return payout.Destination{}, errs.Newf(errs.CodeProviderUnavailable,
			"the payout provider %q does not pay in %s", provider.Name(), r.Currency)
	}
	if r.Country != "" && len(caps.SupportedCountries) > 0 {
		if ok, refusals := caps.CanPayRecipient(payout.RecipientProfile{
			Kind: "individual", Country: r.Country,
		}); !ok {
			return payout.Destination{}, errs.Newf(errs.CodeProviderUnavailable,
				"the payout provider %q does not pay recipients in %s", provider.Name(), r.Country).
				WithField("refusals", refusalCodes(refusals))
		}
	}

	var out payout.Destination
	err = a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var cerr error
		out, cerr = a.deps.Payouts.CreateDestination(ctx, tx, payout.Destination{
			AccountID:         r.AccountID,
			Kind:              r.Kind,
			Provider:          provider.Name(),
			ProviderReference: r.ProviderToken,
			DisplayLabel:      r.DisplayLabel,
			MaskedDisplay:     r.MaskedDisplay,
			Currency:          r.Currency,
			Country:           r.Country,
			Sandbox:           caps.Availability == payout.AvailabilitySandbox || a.withdrawal.SandboxTier,
		})
		if cerr != nil {
			return cerr
		}
		// On a sandbox tier the provider "tokenises" the handle without any
		// real numbers and accepts it immediately, so the whole journey can be
		// exercised. On any other deployment the destination stays UNVERIFIED
		// until a real provider says otherwise, which is a webhook nobody has
		// yet because nobody has a contract (BLOCKERS B-01).
		if caps.Availability != payout.AvailabilitySandbox {
			return nil
		}
		out, cerr = a.deps.Payouts.TransitionDestination(ctx, tx, out.ID, payout.DestinationVerified,
			payout.DestinationChange{
				ActorType: security.ActorSystem,
				ActorID:   "sandbox:" + provider.Name(),
				Reason: "the sandbox payout provider tokenised this destination; this is a rehearsal " +
					"and is not a provider's acceptance of a real account",
				OccurredAt: a.clk.Now().UTC(),
			})
		return cerr
	})
	return out, err
}

func (a conversionAdapter) DisableDestination(ctx context.Context, accountID accounts.AccountID, id payout.DestinationID) (payout.Destination, error) {
	var out payout.Destination
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		current, gerr := a.deps.Payouts.Destination(ctx, tx, id)
		if gerr != nil {
			return gerr
		}
		if current.AccountID != accountID {
			// NOT_FOUND rather than FORBIDDEN, as everywhere else here: a
			// distinguishable refusal is a membership oracle.
			return errs.New(errs.CodeNotFound, "no such payout destination")
		}
		var terr error
		out, terr = a.deps.Payouts.TransitionDestination(ctx, tx, id, payout.DestinationDisabled,
			payout.DestinationChange{
				ActorType:  security.ActorUser,
				ActorID:    accountID.String(),
				Reason:     "the account holder stopped using this destination",
				OccurredAt: a.clk.Now().UTC(),
			})
		return terr
	})
	return out, err
}

// Quote is the pre-commitment call. It reserves nothing and writes no ledger
// row: it records what the customer was shown, so that the number they saw and
// the number they are charged are the same number.
func (a conversionAdapter) Quote(ctx context.Context, r CreatePayoutQuote) (payout.Quote, []payout.ProvenanceSlice, error) {
	if a.withdrawal.Pricing == nil {
		return payout.Quote{}, nil, errNotWired("credit pricing")
	}
	pricing := a.withdrawal.Pricing.Pricing()
	caps, err := a.deps.capsOf(ctx)
	if err != nil {
		return payout.Quote{}, nil, err
	}
	level, err := a.deps.verificationOf(ctx, r.AccountID)
	if err != nil {
		return payout.Quote{}, nil, err
	}
	policy := a.deps.policyOf()

	var (
		quote      payout.Quote
		provenance []payout.ProvenanceSlice
	)
	err = a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		dest, derr := a.deps.Payouts.Destination(ctx, tx, r.DestinationID)
		if derr != nil {
			return derr
		}
		if dest.AccountID != r.AccountID {
			return errs.New(errs.CodeNotFound, "no such payout destination")
		}
		// Asked inside the quote's own transaction: a person is shown a price
		// and then asked to commit to it, and both steps have to agree about
		// whether they have signed the document that governs value leaving.
		accepted, aerr := a.withdrawal.disclosureAccepted(ctx, tx, r.AccountID)
		if aerr != nil {
			return aerr
		}
		var qerr error
		quote, qerr = a.deps.Payouts.Quote(ctx, tx, payout.QuoteRequest{
			AccountID:              r.AccountID,
			DestinationID:          r.DestinationID,
			Quantity:               r.Amount,
			CreditsPerMajorUnit:    pricing.CreditsPerMajorUnit,
			MinorUnitsPerMajorUnit: pricing.MinorUnitsPerMajorUnit,
			CreditDecimals:         a.withdrawal.CreditDecimals,
			PricingVersion:         pricing.Version,
			PolicyVersion:          policy.Version,
			Currency:               currencyOf(dest, pricing.Currency),
			Environment:            a.withdrawal.Environment,
			Sandbox:                a.withdrawal.SandboxTier || dest.Sandbox,
			DisclosureAccepted:     accepted,
			IdempotencyKey:         r.IdempotencyKey,
			Now:                    a.clk.Now().UTC(),
		}, dest)
		if qerr != nil {
			return qerr
		}
		// What value WOULD leave, computed from the same engine the payout
		// itself will use, with nothing reserved. A person is entitled to know
		// what is leaving before they commit to it (§23).
		//
		// The compliance facts are read in this transaction and handed to the
		// engine, so the provenance shown beside a quote is the provenance the
		// commit would actually consume rather than an optimistic one computed
		// without them (D-120).
		facts, ferr := a.withdrawal.complianceFacts(ctx, tx, r.AccountID)
		if ferr != nil {
			return ferr
		}
		decision, eerr := a.deps.PayoutEngine.Evaluate(ctx, tx, payout.EligibilityInput{
			AccountID:             r.AccountID,
			Requested:             r.Amount,
			Policy:                policy,
			Verified:              level,
			ActiveCaps:            caps,
			Now:                   a.clk.Now().UTC(),
			DestinationVerified:   dest.Status.Usable(),
			ProviderSupports:      true,
			SanctionsState:        facts.Sanctions,
			AccountRestrictions:   facts.Restrictions,
			JurisdictionSupported: facts.JurisdictionSupported,
		})
		if eerr != nil {
			return eerr
		}
		provenance = payout.DecisionProvenance(decision)
		return nil
	})
	return quote, provenance, err
}

// currencyOf is the destination's currency, or the deployment's Credit pricing
// currency when the destination does not state one.
func currencyOf(d payout.Destination, fallback string) string {
	if d.Currency != "" {
		return d.Currency
	}
	return fallback
}

func refusalCodes(rs []payout.RecipientRefusal) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r))
	}
	return out
}
