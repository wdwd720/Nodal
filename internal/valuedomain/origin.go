package valuedomain

import (
	"strings"

	"github.com/nodal/controlplane/internal/errs"
)

// CreditOrigin is where a unit of internal Credit came from (PART IX).
//
// Origin is not a label on a transaction; it is a property of the units
// themselves, carried in immutable provenance lots. When a user spends
// Credits the lots are consumed in a defined order, and what remains is what
// determines what they may later withdraw. Two users holding "18,450 Credits"
// may have entirely different withdrawable amounts, and that is correct.
type CreditOrigin string

// Credit origins.
const (
	// OriginPurchased is Credits bought with a payment instrument. The funding
	// may still be reversible; see FundingFinality.
	OriginPurchased CreditOrigin = "PURCHASED"

	// OriginPromotional is Credits granted by the platform for marketing,
	// onboarding or goodwill. No consideration was received.
	OriginPromotional CreditOrigin = "PROMOTIONAL"

	// OriginRefund is Credits returned to a user by an approved refund.
	OriginRefund CreditOrigin = "REFUND"

	// OriginCreatorEarning is revenue a user earned selling a creator product.
	OriginCreatorEarning CreditOrigin = "CREATOR_EARNING"

	// OriginDataSaleEarning is revenue a user earned selling data.
	OriginDataSaleEarning CreditOrigin = "DATA_SALE_EARNING"

	// OriginAgentServiceEarning is revenue a user earned selling an agent
	// service.
	OriginAgentServiceEarning CreditOrigin = "AGENT_SERVICE_EARNING"

	// OriginMarketCreatorEarning is a native-market creator fee (PART LXXX).
	// It is deliberately distinct from ordinary creator revenue because its
	// source is speculative trading activity.
	OriginMarketCreatorEarning CreditOrigin = "MARKET_CREATOR_EARNING"

	// OriginMarketTradingProceeds is the proceeds of selling a native asset.
	// This is the most legally sensitive origin in the system.
	OriginMarketTradingProceeds CreditOrigin = "MARKET_TRADING_PROCEEDS"

	// OriginCompetitionReward is a prize or reward from a platform
	// competition.
	OriginCompetitionReward CreditOrigin = "COMPETITION_REWARD"

	// OriginAdminAdjustment is a privileged, audited correction.
	OriginAdminAdjustment CreditOrigin = "ADMIN_ADJUSTMENT"

	// OriginProviderSettlement is value credited as the result of an external
	// provider settlement.
	OriginProviderSettlement CreditOrigin = "PROVIDER_SETTLEMENT"
)

var allOrigins = []CreditOrigin{
	OriginPurchased, OriginPromotional, OriginRefund,
	OriginCreatorEarning, OriginDataSaleEarning, OriginAgentServiceEarning,
	OriginMarketCreatorEarning, OriginMarketTradingProceeds,
	OriginCompetitionReward, OriginAdminAdjustment, OriginProviderSettlement,
}

var originSet = func() map[CreditOrigin]struct{} {
	m := make(map[CreditOrigin]struct{}, len(allOrigins))
	for _, o := range allOrigins {
		m[o] = struct{}{}
	}
	return m
}()

// AllOrigins returns every declared origin in declaration order (a copy).
func AllOrigins() []CreditOrigin { return append([]CreditOrigin(nil), allOrigins...) }

// Valid reports whether o is a declared origin.
func (o CreditOrigin) Valid() bool {
	_, ok := originSet[o]
	return ok
}

func (o CreditOrigin) String() string { return string(o) }

// ParseCreditOrigin parses the canonical uppercase string form.
func ParseCreditOrigin(s string) (CreditOrigin, error) {
	o := CreditOrigin(strings.ToUpper(strings.TrimSpace(s)))
	if !o.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown credit origin %q", s)
	}
	return o, nil
}

// EarnedByUser reports whether the origin represents value the user earned
// through their own supply of goods, services or data, as distinct from value
// they bought, were given, or realised from speculation. The distinction
// matters because it is the one most likely to be treated differently by a
// payout provider (PART XVII).
func (o CreditOrigin) EarnedByUser() bool {
	switch o {
	case OriginCreatorEarning, OriginDataSaleEarning, OriginAgentServiceEarning, OriginMarketCreatorEarning:
		return true
	default:
		return false
	}
}

// FundingFinality is how certain it is that value credited to a user cannot be
// taken back by the party that supplied it (PART XI).
//
// Card funding is the motivating case: a captured payment is not a settled
// payment, and Credits minted from it must not be treated as final until the
// chargeback window has passed. Value that is not FinalitySettled can be spent
// only where policy allows, and can never be paid out.
type FundingFinality string

// Funding finality states.
const (
	// FinalityUnfunded is value with no external funding behind it at all:
	// promotional grants and admin adjustments. It is final in the sense that
	// nobody can claw it back, but it is not "settled money" either.
	FinalityUnfunded FundingFinality = "UNFUNDED"

	// FinalityReversible is value backed by external funding that the funder
	// can still reverse (an authorised or captured card payment inside the
	// dispute window).
	FinalityReversible FundingFinality = "REVERSIBLE"

	// FinalitySettled is value backed by external funding that is final.
	FinalitySettled FundingFinality = "SETTLED"

	// FinalityDisputed is value whose backing funding is under dispute. It is
	// strictly worse than REVERSIBLE: a dispute is in progress right now.
	FinalityDisputed FundingFinality = "DISPUTED"

	// FinalityReversed is value whose backing funding has been taken back.
	// Lots in this state hold no spendable value.
	FinalityReversed FundingFinality = "REVERSED"
)

var allFinalities = []FundingFinality{
	FinalityUnfunded, FinalityReversible, FinalitySettled, FinalityDisputed, FinalityReversed,
}

// AllFinalities returns every declared finality in declaration order (a copy).
func AllFinalities() []FundingFinality { return append([]FundingFinality(nil), allFinalities...) }

// Valid reports whether f is a declared finality.
func (f FundingFinality) Valid() bool {
	for _, x := range allFinalities {
		if x == f {
			return true
		}
	}
	return false
}

func (f FundingFinality) String() string { return string(f) }

// Spendable reports whether value at this finality may fund new internal
// activity. Reversible value is spendable — that is the ordinary product
// experience, and the platform carries the reversal risk knowingly through a
// dispute reserve — but reversed value is not, and disputed value is frozen
// while the dispute runs.
func (f FundingFinality) Spendable() bool {
	return f == FinalityUnfunded || f == FinalityReversible || f == FinalitySettled
}

// PayoutEligible reports whether value at this finality could ever be paid
// out, before any origin policy is consulted. Only settled and unfunded value
// clears this bar: paying out value that a card issuer can still reclaim turns
// a chargeback into an uncollateralised loss, which PART XI forbids.
//
// This is a floor, not a permission. Origin policy applies on top of it.
func (f FundingFinality) PayoutEligible() bool {
	return f == FinalitySettled || f == FinalityUnfunded
}

// finalityRank orders finalities from least final to most, so "the least final
// of these" is a minimum.
//
// It is here rather than in each caller because three of them had their own
// copy of this ordering and a fourth needed one: internal/httpapi folds an
// account's lots into per-origin buckets by it, and internal/credit mints a
// derived lot at the least final finality among the lots that funded it
// (D-124). An ordering copied four times is an ordering that eventually
// disagrees with itself about whether DISPUTED is worse than REVERSIBLE.
//
// An undeclared finality is worse than every declared one: Policy.Permits reads
// it as UNKNOWN_FUNDING_FINALITY and refuses.
func finalityRank(f FundingFinality) int {
	switch f {
	case FinalityReversed:
		return 1
	case FinalityDisputed:
		return 2
	case FinalityReversible:
		return 3
	case FinalityUnfunded:
		return 4
	case FinalitySettled:
		return 5
	}
	return 0
}

// LessFinal reports whether a is less final than b.
func LessFinal(a, b FundingFinality) bool { return finalityRank(a) < finalityRank(b) }

// LeastFinal is the least final of the given finalities, or the zero value when
// there are none. A caller with no finalities to compare has established
// nothing and must not read the answer as permission.
func LeastFinal(in ...FundingFinality) FundingFinality {
	if len(in) == 0 {
		return ""
	}
	worst := in[0]
	for _, f := range in[1:] {
		if LessFinal(f, worst) {
			worst = f
		}
	}
	return worst
}

// ParseFundingFinality parses the canonical uppercase string form.
func ParseFundingFinality(s string) (FundingFinality, error) {
	f := FundingFinality(strings.ToUpper(strings.TrimSpace(s)))
	if !f.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown funding finality %q", s)
	}
	return f, nil
}

// finalityTransitions is the explicit legal transition table for a lot's
// funding finality.
//
// UNFUNDED used to be terminal, on the reasoning that nothing external backs it
// so nothing external can change it. That is true of the lots it was written
// for -- a promotional grant, an admin adjustment -- and false of a DERIVED lot
// minted there. D-124 mints a derived lot at the LEAST FINAL finality among its
// parents, and the ordering puts UNFUNDED between REVERSIBLE and SETTLED: a
// seller's earning funded by a buyer's grant AND a buyer's settled card
// purchase is minted UNFUNDED, and something external backs part of it. When
// that card is charged back the earning has to freeze, and could not (F-273).
//
// So UNFUNDED has exactly one edge, and it points at frozen. It may not become
// REVERSIBLE or SETTLED: there is no funding row to settle, and a promotion out
// of UNFUNDED would be value inventing a backer.
var finalityTransitions = map[FundingFinality][]FundingFinality{
	FinalityUnfunded:   {FinalityDisputed},
	FinalityReversible: {FinalitySettled, FinalityDisputed, FinalityReversed},
	FinalitySettled:    {FinalityDisputed},
	FinalityDisputed:   {FinalitySettled, FinalityReversed, FinalityReversible},
	FinalityReversed:   {},
}

// FrozenFinalities are the finalities that freeze anything derived from them: a
// dispute in progress and one that succeeded.
//
// It is here rather than as a literal in the sweep's SQL because the sweep's
// candidate predicate and its acting branch have to agree about which parents
// freeze a child, and a list written twice is a list that eventually disagrees
// with itself. `credit.SettleDerived` reads it for both.
func FrozenFinalities() []FundingFinality {
	return []FundingFinality{FinalityDisputed, FinalityReversed}
}

// Frozen reports whether value at this finality freezes what was derived from
// it.
func (f FundingFinality) Frozen() bool {
	return f == FinalityDisputed || f == FinalityReversed
}

// CanTransitionFinality reports whether from → to is a legal change.
//
// SETTLED → DISPUTED is permitted because a card network can raise a dispute
// after the window a payment processor considers settled; refusing the
// transition would leave the system unable to record something that had
// already happened. UNFUNDED → DISPUTED is permitted for the reason above the
// table: a derived lot can be minted UNFUNDED and still have a parent somebody
// can reclaim.
func CanTransitionFinality(from, to FundingFinality) bool {
	for _, t := range finalityTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}
