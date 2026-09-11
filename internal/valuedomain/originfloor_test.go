package valuedomain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The restriction ordering is a statement about the policies this build has, so
// it is tested against them rather than against a list somebody wrote down
// (D-131).

func TestOriginRestriction_AgreesWithEveryPolicyInThisBuild(t *testing.T) {
	t.Parallel()
	policies := buildPolicies()
	require.Len(t, policies, 2, "the ordering is computed from these; a new one changes the answer")

	for _, o := range AllOrigins() {
		permitting := 0
		for _, p := range policies {
			if p.Rule(o).PayoutAllowed {
				permitting++
			}
		}
		switch o.Restriction() {
		case OriginClosedEverywhere:
			assert.Zerof(t, permitting, "%s ranks closed everywhere and a policy releases it", o)
		case OriginClosedSomewhere:
			assert.Greaterf(t, permitting, 0, "%s ranks closed somewhere and no policy releases it", o)
			assert.Lessf(t, permitting, len(policies), "%s ranks closed somewhere and every policy releases it", o)
		case OriginPermittedEverywhere:
			assert.Equalf(t, len(policies), permitting, "%s ranks permitted everywhere and a policy refuses it", o)
		}
	}
}

// The five origins that are demo money, a correction or somebody else's
// settlement are the most restricted there is, and nothing in this build
// releases them. This is the half of the ordering a reader needs to be able to
// check by eye.
func TestOriginRestriction_GrantsAndCorrectionsAreTheMostRestricted(t *testing.T) {
	t.Parallel()
	for _, o := range []CreditOrigin{
		OriginPromotional, OriginRefund, OriginAdminAdjustment,
		OriginProviderSettlement, OriginCompetitionReward,
	} {
		assert.Equalf(t, OriginClosedEverywhere, o.Restriction(), "%s", o)
		assert.Falsef(t, DefaultPolicy().Rule(o).PayoutAllowed, "%s", o)
		assert.Falsef(t, SandboxPolicy().Rule(o).PayoutAllowed, "%s", o)
	}
	for _, o := range []CreditOrigin{
		OriginPurchased, OriginCreatorEarning, OriginDataSaleEarning,
		OriginAgentServiceEarning, OriginMarketCreatorEarning, OriginMarketTradingProceeds,
	} {
		assert.Equalf(t, OriginClosedSomewhere, o.Restriction(), "%s", o)
		assert.Truef(t, MoreRestricted(OriginPromotional, o),
			"a promotional grant must be more restricted than %s", o)
	}

	// DefaultPolicy releases nothing, so no origin can be permitted everywhere
	// while it exists. The level is declared anyway: the ordering is a statement
	// about policies, not about today's two.
	for _, o := range AllOrigins() {
		assert.NotEqualf(t, OriginPermittedEverywhere, o.Restriction(),
			"%s: DefaultPolicy releases nothing, so nothing is permitted everywhere", o)
	}
}

func TestMostRestrictedOrigin_TakesTheWorstAndIsDeterministic(t *testing.T) {
	t.Parallel()

	assert.Equal(t, CreditOrigin(""), MostRestrictedOrigin(),
		"nothing to compare establishes nothing, and Permits refuses an undeclared origin")
	assert.Equal(t, OriginPurchased, MostRestrictedOrigin(OriginPurchased))
	assert.Equal(t, OriginPromotional,
		MostRestrictedOrigin(OriginPurchased, OriginPromotional, OriginCreatorEarning))
	assert.Equal(t, OriginPromotional,
		MostRestrictedOrigin(OriginPromotional, OriginPurchased))

	// A tie is broken by the name, so the answer does not depend on the order
	// the parents came back in. Migration 00816 orders the same way.
	forward := MostRestrictedOrigin(OriginRefund, OriginPromotional, OriginAdminAdjustment)
	backward := MostRestrictedOrigin(OriginAdminAdjustment, OriginPromotional, OriginRefund)
	assert.Equal(t, forward, backward)
	assert.Equal(t, OriginAdminAdjustment, forward, "ADMIN_ADJUSTMENT sorts first among the closed origins")

	// An origin nothing declares is the most restricted thing there is, so a
	// typo cannot become the floor somebody's money is measured against.
	assert.Equal(t, OriginClosedEverywhere, CreditOrigin("MADE_UP").Restriction())
	assert.Equal(t, CreditOrigin("MADE_UP"),
		MostRestrictedOrigin(OriginPurchased, "MADE_UP"))
}

// The rule the whole file exists for, stated as a test: under the policy a
// sandbox tier runs, proceeds of a promotional grant are refused and proceeds
// of a purchase are not.
func TestPolicy_PermitsReadsTheFloorAsWellAsTheOrigin(t *testing.T) {
	t.Parallel()
	p := SandboxPolicy()
	base := PermitInput{
		Origin:      OriginMarketTradingProceeds,
		Finality:    FinalityUnfunded,
		Domain:      InternalCredit,
		Verified:    VerificationPayoutKYC,
		ActiveCaps:  map[CapabilityKey]bool{CapPayoutReserve: true},
		PolicyValid: true,
	}

	granted := base
	granted.OriginFloor = OriginPromotional
	ok, reasons := p.Permits(granted)
	assert.False(t, ok, "a grant round-tripped through a market is still a grant (goal §23)")
	assert.Contains(t, reasons, ReasonOriginForbidden)

	bought := base
	bought.OriginFloor = OriginPurchased
	ok, reasons = p.Permits(bought)
	assert.True(t, ok, "proceeds of a purchase are withdrawable: %v", reasons)

	// And the floor cannot RAISE an origin the policy forbids, which is the
	// mistake the rule would be if it were a maximum instead of a minimum.
	grant := base
	grant.Origin = OriginPromotional
	grant.OriginFloor = OriginPurchased
	ok, _ = p.Permits(grant)
	assert.False(t, ok, "a promotional lot is refused whatever funded it")

	// An unstated floor refuses rather than defaulting to the origin.
	missing := base
	ok, reasons = p.Permits(missing)
	assert.False(t, ok)
	assert.Contains(t, reasons, ReasonUnknownOrigin,
		"a caller that has not established the provenance has not established that it may leave")
}
