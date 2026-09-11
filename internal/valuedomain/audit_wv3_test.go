package valuedomain

// Reproduction for the THIRD round of the withdrawal-verification audit
// (goal §54). Nothing here changes product code.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// F-wv3-6 — "the most restricted parent" is decided by a rank computed from the
// two policies this BUILD ships, with the origin's own NAME breaking a tie, so
// a lot's floor is not the most restricted parent under the policy that
// actually judges it.
//
// D-131 residual 1 says a policy persisted after a lot was minted "would not
// change any floor already written … which is the conservative direction". It
// is conservative only for a policy that releases a SUPERSET of what this
// build's policies release. B-02 — the open blocker the whole payout model
// waits on — is written as two questions: "is Nodal's closed-loop Credit float
// itself stored value requiring a licence; may trading gains ever be
// withdrawn". The policy that answers "the float is stored value, the gains are
// not" releases MARKET_TRADING_PROCEEDS and closes PURCHASED, which is not a
// superset of anything.
//
// Both origins rank OriginClosedSomewhere under this build, so the floor of a
// lot funded by one of each is settled by `a < b` on the origin STRING:
// "MARKET_TRADING_PROCEEDS" sorts before "PURCHASED". The database's
// cp_credit_origin_floor_rank orders the same way, so the two agree about an
// answer that is arbitrary with respect to any policy.
//
// The result is the shape §23 forbids, reached without any trade being wrong:
// value half of which came from a source the live policy refuses is released,
// because the floor column can hold one origin and its provenance had two.
// ---------------------------------------------------------------------------

func TestAuditWV3_TheFloorIsTheMostRestrictedParentUnderThePolicyThatJudgesIt(t *testing.T) {
	// What counsel could plausibly decide, expressed as this package's own
	// type: trading gains may leave; the purchased float may not.
	gainsOnly := Policy{
		Version: "audit-wv3-gains-only",
		Rules: map[CreditOrigin]OriginRule{
			OriginMarketTradingProceeds: {
				PayoutAllowed:        true,
				RequiredCapability:   CapPayoutReserve,
				RequiredVerification: VerificationPayoutKYC,
			},
		},
	}
	for _, o := range AllOrigins() {
		if _, ok := gainsOnly.Rules[o]; !ok {
			gainsOnly.Rules[o] = OriginRule{PayoutAllowed: false, RequiredVerification: VerificationNone}
		}
	}
	require.NoError(t, gainsOnly.Validate(), "fixture check: this is a policy the approval path would accept")
	require.False(t, gainsOnly.Rule(OriginPurchased).PayoutAllowed,
		"fixture check: the purchased float may not leave under it")

	// A lot funded by two parents: somebody's purchased Credits and somebody's
	// trading gains. This is what the 00816 trigger writes, and what
	// MostRestrictedOrigin computes, when both floors reach it.
	floor := MostRestrictedOrigin(OriginPurchased, OriginMarketTradingProceeds)
	require.Equal(t, OriginMarketTradingProceeds, floor,
		"fixture check: the tie between two OriginClosedSomewhere origins breaks on the name")

	// The lot as the projection now carries it: one floor for display, and the
	// SET of origins its provenance bottoms out in for the permission answer
	// (D-138). The floor is still MARKET_TRADING_PROCEEDS -- the rank did not
	// change and is not meant to -- and PURCHASED is still in the provenance.
	roots := []CreditOrigin{OriginPurchased, OriginMarketTradingProceeds}
	ok, reasons := gainsOnly.Permits(PermitInput{
		Origin:      OriginMarketTradingProceeds,
		OriginFloor: floor,
		RootOrigins: roots,
		Finality:    FinalitySettled,
		Domain:      InternalCredit,
		Verified:    VerificationPayoutKYC,
		ActiveCaps:  map[CapabilityKey]bool{CapPayoutReserve: true},
		PolicyValid: true,
	})
	assert.False(t, ok,
		"F-wv3-6: a lot funded in part by PURCHASED value is released by a policy that "+
			"forbids PURCHASED, because its floor was recorded as the other parent. The "+
			"restriction ordering is computed from DefaultPolicy and SandboxPolicy and ties "+
			"break on the origin's name, so 'the most restricted parent' is not the most "+
			"restricted parent under the policy doing the judging; reasons=%v", reasons)
	assert.Contains(t, reasons, ReasonOriginForbidden,
		"and it is refused as an origin refusal rather than as a malformed input")

	// Not vacuous: the refusal is the POLICY refusing a root, and the same lot
	// under a policy that releases both roots is released. A test that refused
	// everything would agree with any implementation.
	both := Policy{Version: "audit-wv3-both", Rules: map[CreditOrigin]OriginRule{}}
	for _, o := range AllOrigins() {
		both.Rules[o] = OriginRule{PayoutAllowed: false, RequiredVerification: VerificationNone}
	}
	for _, o := range roots {
		both.Rules[o] = OriginRule{
			PayoutAllowed:        true,
			RequiredCapability:   CapPayoutReserve,
			RequiredVerification: VerificationPayoutKYC,
		}
	}
	require.NoError(t, both.Validate())
	permitted, why := both.Permits(PermitInput{
		Origin:      OriginMarketTradingProceeds,
		OriginFloor: floor,
		RootOrigins: roots,
		Finality:    FinalitySettled,
		Domain:      InternalCredit,
		Verified:    VerificationPayoutKYC,
		ActiveCaps:  map[CapabilityKey]bool{CapPayoutReserve: true},
		PolicyValid: true,
	})
	assert.True(t, permitted, "a policy that releases every root releases the lot; reasons=%v", why)

	// And the reason names the root the policy refuses, which is the one a
	// person has to read about: it is PURCHASED, not the ranked floor.
	refused, found := gainsOnly.RefusedRoot(roots)
	assert.True(t, found)
	assert.Equal(t, OriginPurchased, refused)
}
