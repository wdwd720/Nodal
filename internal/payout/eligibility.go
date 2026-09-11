package payout

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// EligibilityInput is everything a decision depends on. Nothing is discovered:
// the policy, the clock, the verification level and the capability set are all
// supplied, so the same inputs always produce the same Decision and a decision
// made in March can be replayed in June.
type EligibilityInput struct {
	AccountID accounts.AccountID
	Requested money.Quantity

	Policy     valuedomain.Policy
	Verified   valuedomain.VerificationLevel
	ActiveCaps map[valuedomain.CapabilityKey]bool
	Now        time.Time

	// AccountFrozen and FraudFlagged are account-level facts that override
	// everything below them. They are inputs rather than lookups because the
	// caller already knows them and a second lookup could disagree with the
	// first.
	AccountFrozen bool
	FraudFlagged  bool

	// The three compliance facts GET /v1/me/eligibility refuses on.
	//
	// Until F-226 this type had a field for none of them, so an open sanctions
	// review, a restriction recorded against the account and an unsupported
	// jurisdiction stopped the eligibility page and stopped nothing on the
	// conversion path: the page reported ACCOUNT_RESTRICTED and 0 withdrawable
	// while Create reserved the whole balance and Submit settled it. They carry
	// the same shapes internal/eligibility uses, because two surfaces
	// disagreeing about one fact is the defect rather than a detail of it
	// (D-120).
	//
	// None of them has a permissive zero value. An empty SanctionsState is
	// nobody having supplied one, which blocks; UNKNOWN is a supplied answer --
	// nobody has screened this person yet -- and is read exactly as
	// eligibility.ExplainWithdrawal reads it. JurisdictionSupported false is
	// "we do not know where this person is", which is not permission.
	SanctionsState compliance.SanctionsState
	// AccountRestrictions are the restriction codes recorded against the
	// account and the compliance profile. Non-empty blocks.
	AccountRestrictions []string
	// JurisdictionSupported is the verification rule table's verdict on where
	// the person is.
	JurisdictionSupported bool

	// DestinationVerified is whether the chosen destination may receive value.
	DestinationVerified bool
	// ProviderSupports is whether the chosen provider supports the
	// destination kind and currency.
	ProviderSupports bool
}

// Lots is the part of internal/credit the engine reads.
type Lots interface {
	EligibleLots(ctx context.Context, q db.Querier, r credit.BalanceRequest) ([]credit.Lot, money.Quantity, error)
	// Lots returns EVERY lot the account holds, eligible or not. Explaining a
	// shortfall needs the ones that were refused, and EligibleLots by
	// definition does not return those.
	Lots(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]credit.Lot, error)
}

// Engine decides how much of a request may be paid out, and why not otherwise.
//
// It is deliberately a separate object from the payout Service. PART XXXIII
// requires eligibility, compliance and risk to be separate decisions rather
// than one merged verdict, and separating the decision from the action means
// the decision can be computed, shown to a user, stored, and replayed without
// anything moving.
type Engine struct {
	lots Lots
}

// NewEngine returns an Engine.
func NewEngine(lots Lots) *Engine {
	if lots == nil {
		panic("payout: NewEngine requires a credit lot source")
	}
	return &Engine{lots: lots}
}

// Additional reasons the engine can give beyond the policy's own.
const (
	// ReasonAccountFrozen is an account-level block.
	ReasonAccountFrozen valuedomain.PermitReason = "ACCOUNT_FROZEN"
	// ReasonFraudFlagged is a fraud hold.
	ReasonFraudFlagged valuedomain.PermitReason = "FRAUD_REVIEW"
	// ReasonDestinationNotVerified is an unverified payout destination.
	ReasonDestinationNotVerified valuedomain.PermitReason = "DESTINATION_NOT_VERIFIED"
	// ReasonProviderCannotPay is a provider that does not support the
	// destination or currency.
	ReasonProviderCannotPay valuedomain.PermitReason = "PROVIDER_CANNOT_PAY"
	// ReasonInsufficientEligibleValue is the ordinary shortfall: the account
	// holds Credits, and not enough of them are of a kind that may leave.
	ReasonInsufficientEligibleValue valuedomain.PermitReason = "INSUFFICIENT_ELIGIBLE_VALUE"
	// ReasonAccountRestricted is a freeze, a compliance hold or a sanctions
	// screen that is not clear. The word is eligibility.WithdrawalAccountRestricted's,
	// because the two surfaces answer the same question (D-120).
	ReasonAccountRestricted valuedomain.PermitReason = "ACCOUNT_RESTRICTED"
	// ReasonJurisdictionRestricted is a place this is not offered, or a place
	// nobody has established.
	ReasonJurisdictionRestricted valuedomain.PermitReason = "JURISDICTION_RESTRICTED"
	// ReasonMinimumNotMet is a payout the provider will not send because it
	// nets less than its published minimum. It is carried as a refusal field on
	// the error Create returns rather than as a shortfall, because the answer is
	// not "some of this may leave" -- it is "none of it can be sent".
	ReasonMinimumNotMet valuedomain.PermitReason = "MINIMUM_NOT_MET"
)

// Evaluate produces a Decision. It moves nothing.
func (e *Engine) Evaluate(ctx context.Context, q db.Querier, in EligibilityInput) (Decision, error) {
	if in.AccountID.IsZero() {
		return Decision{}, errs.New(errs.CodeValidationFailed, "eligibility requires an account")
	}
	if in.Requested.Sign() <= 0 {
		return Decision{}, errs.New(errs.CodeValidationFailed, "eligibility requires a positive amount")
	}
	if in.Now.IsZero() {
		return Decision{}, errs.New(errs.CodeValidationFailed, "eligibility requires the current time")
	}
	hash, err := in.Policy.Hash()
	if err != nil {
		return Decision{}, err
	}

	d := Decision{
		Requested:            in.Requested,
		Eligible:             money.Quantity{},
		PolicyVersion:        in.Policy.Version,
		PolicyHash:           hash,
		RequiredVerification: highestRequiredVerification(in.Policy),
	}

	// Account-level blocks come first and are absolute. A frozen account does
	// not get a partial payout, and computing one would be misleading.
	var blocks []valuedomain.PermitReason
	if in.AccountFrozen {
		blocks = append(blocks, ReasonAccountFrozen)
	}
	if in.FraudFlagged {
		blocks = append(blocks, ReasonFraudFlagged)
	}
	if restricted(in) {
		blocks = append(blocks, ReasonAccountRestricted)
	}
	if !in.JurisdictionSupported {
		blocks = append(blocks, ReasonJurisdictionRestricted)
	}
	if !in.DestinationVerified {
		blocks = append(blocks, ReasonDestinationNotVerified)
	}
	if !in.ProviderSupports {
		blocks = append(blocks, ReasonProviderCannotPay)
	}
	if len(blocks) > 0 {
		d.Reasons = blocks
		return d, nil
	}

	lots, total, err := e.lots.EligibleLots(ctx, q, credit.BalanceRequest{
		AccountID:  in.AccountID,
		Policy:     in.Policy,
		Verified:   in.Verified,
		ActiveCaps: in.ActiveCaps,
		Now:        in.Now,
	})
	if err != nil {
		return Decision{}, err
	}

	// Take lots in consumption order until the request is covered. Taking them
	// in order matters: the same request must always draw on the same lots, or
	// two identical requests could leave an account with different provenance.
	remaining := in.Requested
	for _, lot := range lots {
		if !remaining.IsPositive() {
			break
		}
		take := lot.Remaining.Min(remaining)
		if !take.IsPositive() {
			continue
		}
		d.Lots = append(d.Lots, lot)
		d.Eligible = d.Eligible.Add(take)
		remaining = remaining.Sub(take)
	}
	d.Origins = credit.EligibleOrigins(d.Lots)

	if remaining.IsPositive() {
		d.Reasons = append(d.Reasons, ReasonInsufficientEligibleValue)
		// Explain WHY the rest is not eligible, rather than only that it is
		// not. The user's next question is always "why", and the reasons come
		// from the same policy evaluation that produced the shortfall.
		d.Reasons = append(d.Reasons, e.shortfallReasons(ctx, q, in)...)

		// Would verifying fix it? Re-run the identical evaluation with the
		// verification level raised and nothing else changed. If that covers
		// the request, the user is not ineligible -- they are unverified, and
		// those are different answers deserving different words.
		if !in.Verified.AtLeast(d.RequiredVerification) {
			raised := in
			raised.Verified = d.RequiredVerification
			_, coverable, lerr := e.lots.EligibleLots(ctx, q, credit.BalanceRequest{
				AccountID:  raised.AccountID,
				Policy:     raised.Policy,
				Verified:   raised.Verified,
				ActiveCaps: raised.ActiveCaps,
				Now:        raised.Now,
			})
			if lerr == nil && coverable.Cmp(in.Requested) >= 0 {
				d.VerificationWouldSuffice = true
			}
		}
		_ = total
	}
	return d, nil
}

// restricted reports whether a compliance fact stops this account outright.
//
// The sanctions screen is read the way eligibility.applyAccountFacts reads it:
// HIT and REVIEW are restrictions, CLEAR and UNKNOWN are not. Anything else --
// including the empty string a caller that forgot the field would send -- is
// nobody having answered, and nobody having answered is not a clearance.
func restricted(in EligibilityInput) bool {
	if len(in.AccountRestrictions) > 0 {
		return true
	}
	switch in.SanctionsState {
	case compliance.SanctionsClear, compliance.SanctionsUnknown:
		return false
	default:
		return true
	}
}

// shortfallReasons collects the distinct reasons the account's remaining
// Credits were refused, in the policy's canonical order and without duplicates.
func (e *Engine) shortfallReasons(ctx context.Context, q db.Querier, in EligibilityInput) []valuedomain.PermitReason {
	// Every lot the account holds, including the ones the policy refused.
	//
	// An earlier version asked EligibleLots for this and got nothing useful:
	// EligibleLots filters OUT exactly the lots whose refusal is the answer, so
	// a user whose whole balance was ineligible was told only "insufficient",
	// never why. Explaining a refusal has to start from the refused set.
	lots, err := e.lots.Lots(ctx, q, in.AccountID)
	if err != nil {
		return nil
	}
	seen := map[valuedomain.PermitReason]bool{}
	var out []valuedomain.PermitReason
	policyValid := in.Policy.Validate() == nil
	for _, lot := range lots {
		ok, reasons := in.Policy.Permits(valuedomain.PermitInput{
			Origin:      lot.Origin,
			OriginFloor: lot.OriginFloor,
			Finality:    lot.Finality,
			Domain:      valuedomain.InternalCredit,
			Verified:    in.Verified,
			HeldDays:    lot.AgeDays(in.Now),
			ActiveCaps:  in.ActiveCaps,
			PolicyValid: policyValid,
		})
		if ok {
			continue
		}
		for _, r := range reasons {
			if seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// highestRequiredVerification is the strictest level any permitting rule
// demands, so a caller can tell the user what to do next.
func highestRequiredVerification(p valuedomain.Policy) valuedomain.VerificationLevel {
	best := valuedomain.VerificationNone
	for _, r := range p.Rules {
		if !r.PayoutAllowed {
			continue
		}
		if r.RequiredVerification.AtLeast(best) {
			best = r.RequiredVerification
		}
	}
	return best
}
