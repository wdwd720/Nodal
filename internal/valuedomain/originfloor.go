package valuedomain

import "sort"

// The origin floor: a derived lot is as withdrawable as the least withdrawable
// thing that funded it, in ORIGIN as well as in finality (D-131, F-261).
//
// D-124 made a derived lot inherit its parents' FINALITY and nothing else,
// which closed the laundering route that runs through the dispute window: buy
// with card-funded Credits, sell them into a market and out again, withdraw,
// charge back. It left the route that runs through the ORIGIN open, and made it
// reachable for the first time:
//
//	a trader holding only PROMOTIONAL, UNFUNDED Credits buys and sells. The
//	proceeds are MARKET_TRADING_PROCEEDS -- an origin SandboxPolicy permits --
//	at UNFUNDED, which PayoutEligible() admits. Before D-124 the proceeds were
//	minted REVERSIBLE for ever, so the finality floor hid this; afterwards the
//	grant is withdrawable value one round trip later.
//
// Goal §23 forbids exactly that shape: "nonwithdrawable source -> trade ->
// magically payout-eligible balance unless the eventual external/legal/provider
// policy explicitly allows it". SandboxPolicy says the opposite of allowing it,
// in its own words: "a promotional grant that could leave the system would be
// the first rule somebody copied".
//
// So every lot carries an ORIGIN FLOOR beside its finality. A lot with no
// parents has its own origin as its floor. A derived lot's floor is the most
// restricted floor among its parents -- inherited with finality, and with the
// same finality: `credit_lot_parents` is append-only and a parent row may only
// be written in the transaction that creates the lot, so a floor is fixed the
// moment the lot exists and nothing later lowers or raises it.
//
// `Policy.Permits` then permits a lot only if it permits BOTH the lot's own
// origin and its floor.
//
// What it costs, said plainly: a trader who buys with a promotional grant and
// sells at a profit cannot withdraw the profit either. That is the intended
// answer. The alternative -- apportioning a lot into a withdrawable part and a
// granted part -- is a second provenance model layered on the one the ledger
// already has, and the first thing it would have to decide is which part the
// profit belongs to, which nobody can answer.

// OriginRestriction is how restricted an origin is across the payout policies
// this build has.
//
// It exists so that "the most restricted of these origins" is one ordering
// rather than an ordering each caller invents. The comparison is deliberately
// over the POLICIES rather than over a hand-written table: a table copied from
// the policies is a list kept in two places, and this repository has a finding
// register full of those.
type OriginRestriction int

// The restriction levels, from most restricted to least.
const (
	// OriginClosedEverywhere is an origin no payout policy in this build
	// releases. PROMOTIONAL, REFUND, ADMIN_ADJUSTMENT, PROVIDER_SETTLEMENT and
	// COMPETITION_REWARD are these: value nobody paid for and nobody earned.
	OriginClosedEverywhere OriginRestriction = 0
	// OriginClosedSomewhere is an origin at least one policy releases and at
	// least one does not. Every origin SandboxPolicy marks withdrawable is
	// here, because DefaultPolicy -- what every deployment including PROD runs
	// until counsel and a provider decide otherwise -- releases nothing.
	OriginClosedSomewhere OriginRestriction = 1
	// OriginPermittedEverywhere is an origin every policy releases. No origin
	// is, and none can be while DefaultPolicy exists; the level is declared
	// rather than omitted so the ordering stays a statement about policies and
	// not a statement about today's two.
	OriginPermittedEverywhere OriginRestriction = 2
)

// buildPolicies are the payout policies this build ships. A policy persisted
// through the approval path is not one of them: it is a deployment's choice,
// and the restriction ordering is a property of the CODE, so that a lot minted
// on one deployment carries the same floor as the same lot minted on another.
func buildPolicies() []Policy { return []Policy{DefaultPolicy(), SandboxPolicy()} }

// originRestriction is computed once, from the policies above.
var originRestriction = func() map[CreditOrigin]OriginRestriction {
	policies := buildPolicies()
	out := make(map[CreditOrigin]OriginRestriction, len(allOrigins))
	for _, o := range allOrigins {
		permitting := 0
		for _, p := range policies {
			if p.Rule(o).PayoutAllowed {
				permitting++
			}
		}
		switch permitting {
		case 0:
			out[o] = OriginClosedEverywhere
		case len(policies):
			out[o] = OriginPermittedEverywhere
		default:
			out[o] = OriginClosedSomewhere
		}
	}
	return out
}()

// Restriction reports how restricted an origin is across this build's policies.
//
// An origin this package does not declare is OriginClosedEverywhere, which is
// the fail-closed answer: a value nothing recognises is not one anything may
// release.
func (o CreditOrigin) Restriction() OriginRestriction {
	r, ok := originRestriction[o]
	if !ok {
		return OriginClosedEverywhere
	}
	return r
}

// MoreRestricted reports whether a is more restricted than b.
//
// Ties are broken by the origin's own name, so that "the most restricted of
// these" is one answer rather than whichever the caller happened to see first.
// Migration 00816's cp_credit_origin_floor_rank orders the same way, and
// test/integration/enums holds the two together: a floor the database computes
// and a floor Go computes must be the same string.
func MoreRestricted(a, b CreditOrigin) bool {
	ra, rb := a.Restriction(), b.Restriction()
	if ra != rb {
		return ra < rb
	}
	return a < b
}

// MostRestrictedOrigin is the most restricted of the given origins, or the
// empty origin when there are none.
//
// An empty answer is not a permissive one: `Permits` reads an origin that is
// not declared as UNKNOWN_ORIGIN and refuses. A caller with no origins to
// compare has established nothing, exactly as LeastFinal says of finalities.
func MostRestrictedOrigin(in ...CreditOrigin) CreditOrigin {
	if len(in) == 0 {
		return ""
	}
	worst := in[0]
	for _, o := range in[1:] {
		if MoreRestricted(o, worst) {
			worst = o
		}
	}
	return worst
}

// OriginsByRestriction returns every declared origin, most restricted first,
// in the order MoreRestricted defines. It is the ordering the SQL rank function
// is compared against.
func OriginsByRestriction() []CreditOrigin {
	out := AllOrigins()
	sort.Slice(out, func(i, j int) bool { return MoreRestricted(out[i], out[j]) })
	return out
}
