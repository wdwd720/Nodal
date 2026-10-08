package delegation

import (
	"sort"

	"github.com/nodal/controlplane/internal/risk"
)

// Violations of delegated authority (advanced_authority_violations, goal
// §29).
//
// A refusal of a delegated request is a violation of authority when the
// request asks for something the owner's grant does not cover:
//
//   - outside the capability's scope: another account's or another agent's
//     capability, another strategy, environment or mode, or an action, an
//     instrument class or a chain the grant does not name;
//   - beyond its limits: the single-trade, daily or lifetime notional, as the
//     capability measures them or as the withdrawal policy measures the
//     agent's scope and limits;
//   - outside its lifetime: after its expiry, or before it was granted;
//   - after its revocation;
//   - under an action label that is not what the swap does (SwapAction);
//   - to a destination not on an allow-list the owner keeps: the
//     capability's destination allow-list, or the account's withdrawal
//     destinations -- also when the owner took the destination off after the
//     proposal.
//
// No other refusal is one. Timing is not (a lapsed quote, a cooling period,
// an approval past its deadline), nor are market and venue conditions (no
// fresh price, slippage, the venue's inventory, a changed chain fee), the
// platform's own policy (the signing policy's bounds, the settlement
// compiler, kill switches, the account's compliance state, its withdrawal
// risk score and its own amount limits), state (a subject that is not
// signable or not approved, an account or an agent that is not active, the
// owner's decline) or integrity (a capability or a plan that no longer
// hashes to what was granted or planned, a message that is not the compiled
// one, a signer key that is not the registered one): each is refused and
// recorded as what it is, not as an agent exceeding its grant.
//
// TestAuthorityViolationCodes_PinTheSetAndSayWhy pins the set, member by
// member, and TestAuthorityViolationCodes_ClassifyEveryRefusalCode classifies
// every refusal code this package declares, so a new code cannot go
// unclassified.

// authorityViolationCodes are the members of AuthorityViolationCodes.
var authorityViolationCodes = []string{
	// Scope.
	ReasonAccountMismatch, ReasonAgentMismatch, ReasonStrategyMismatch, ReasonEnvironmentMismatch, ReasonModeMismatch,
	ReasonActionNotGranted, ReasonInstrumentNotGranted, ReasonChainNotGranted, ReasonWithdrawalAgentScope,
	// Limits.
	ReasonSingleLimitExceeded, ReasonDailyLimitExceeded, ReasonLifetimeLimitExceeded, ReasonWithdrawalAgentLimit,
	// Lifetime and revocation.
	ReasonCapabilityExpired, ReasonCapabilityNotYetValid, ReasonCapabilityRevoked,
	// The action label.
	ReasonActionMismatch,
	// Destinations off an allow-list.
	ReasonDestinationNotGranted, ReasonWithdrawalDestinationNotAllowlisted, ReasonDestinationRemoved,
	// The authority refused as a whole.
	RefusalAuthorityRefused,
}

// AuthorityViolationCodes returns the refusal codes of this package that are
// violations of delegated authority, sorted. A refusal recorded under one of
// them -- an agent intent's rejection code at admission, or one of a signing
// decision's reason codes -- is a request beyond the owner's grant.
func AuthorityViolationCodes() []string {
	out := append([]string(nil), authorityViolationCodes...)
	sort.Strings(out)
	return out
}

// IsAuthorityViolation reports whether code is one of
// AuthorityViolationCodes.
func IsAuthorityViolation(code string) bool {
	for _, c := range authorityViolationCodes {
		if c == code {
			return true
		}
	}
	return false
}

// BlockedBeforeSubmission is the failure code of a signed swap or withdrawal
// that its last check before submission stopped for code: what
// GateSwapSubmission and GateWithdrawalSubmission record, so a stop for a
// violation of authority can be told from one for a kill switch or a freeze.
func BlockedBeforeSubmission(code string) string { return ReasonBlockedBeforeSubmission + ":" + code }

// AuthorityViolationRiskReasons returns the Risk Kernel V2 reasons a
// capability check that fails at admission is refused under, sorted.
//
// An admission does not record the capability's own codes in a column: it
// hands the check to Risk V2 as its authority section
// (AuthorityCheck.Snapshot), the kernel refuses with these reasons (an
// expiry or a use before the grant is RISK_AUTHORITY_EXPIRED), and the
// intent is rejected with RISK_REJECTED. RISK_AUTHORITY_MISSING is not one of
// them: an admission checks the capability's account and agent before the
// intent exists (ReasonAccountMismatch, a refusal that leaves no row), so a
// capability missing from the section is one whose content no longer hashes
// to its grant (ReasonCapabilityTampered) -- tampering, not a request beyond
// the grant.
func AuthorityViolationRiskReasons() []string {
	out := []string{risk.ReasonAuthorityExpired, risk.ReasonAuthorityLimit, risk.ReasonAuthorityRevoked, risk.ReasonAuthorityScope}
	sort.Strings(out)
	return out
}
