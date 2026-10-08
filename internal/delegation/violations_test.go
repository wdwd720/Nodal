package delegation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

// violationWhy is every member of AuthorityViolationCodes and why it is a
// request beyond the owner's grant.
var violationWhy = map[string]string{
	ReasonAccountMismatch:                     "scope: the capability is another account's",
	ReasonAgentMismatch:                       "scope: the capability is another agent's",
	ReasonStrategyMismatch:                    "scope: the agent runs a strategy the grant does not name",
	ReasonEnvironmentMismatch:                 "scope: the grant was made for another environment",
	ReasonModeMismatch:                        "scope: the grant was made for another mode",
	ReasonActionNotGranted:                    "scope: the grant does not name the action",
	ReasonInstrumentNotGranted:                "scope: the grant does not name the instrument class",
	ReasonChainNotGranted:                     "scope: the grant does not name the chain",
	ReasonWithdrawalAgentScope:                "scope: the withdrawal policy finds the proposal outside the agent's grant",
	ReasonSingleLimitExceeded:                 "limits: above the grant's single-trade notional",
	ReasonDailyLimitExceeded:                  "limits: above the grant's daily notional",
	ReasonLifetimeLimitExceeded:               "limits: above the grant's lifetime notional",
	ReasonWithdrawalAgentLimit:                "limits: the withdrawal policy finds the proposal beyond the agent's grant",
	ReasonCapabilityExpired:                   "expiry: the grant has expired",
	ReasonCapabilityNotYetValid:               "expiry: the use is before the grant was made, outside its lifetime on the other side",
	ReasonCapabilityRevoked:                   "revocation: the owner (or an operator) revoked the grant",
	ReasonActionMismatch:                      "the action label: the swap's label is not what the swap does (SwapAction)",
	ReasonDestinationNotGranted:               "a destination not on the capability's destination allow-list",
	ReasonWithdrawalDestinationNotAllowlisted: "a destination not on the account's withdrawal allow-list",
	ReasonDestinationRemoved:                  "a destination the owner took off the account's withdrawal allow-list after the proposal",
	RefusalAuthorityRefused:                   "the delegated authority refused as a whole (declared for it; no path records it today)",
}

// notAViolation is every other refusal code this package declares and why
// it is not a request beyond the grant.
var notAViolation = map[string]string{
	// Timing.
	ReasonQuoteExpired:       "timing: the venue's quote lapsed before signing",
	ReasonCoolingPeriod:      "timing: the withdrawal is still in its cooling period",
	ReasonApprovalExpired:    "timing: nobody decided before the deadline",
	RefusalSubmissionExpired: "timing: the transaction's blockhash expired before it landed",
	// Market and venue conditions.
	RefusalPriceUnavailable:      "market: no fresh price",
	ReasonSlippageExceeded:       "market: the quote moved beyond the agent's slippage bound",
	ReasonAmountTooSmall:         "market: the amount is worth nothing at the price, or does not fit the chain",
	RefusalVenueInventory:        "venue: the venue's house book cannot deliver",
	RefusalVenueUnavailable:      "venue: the venue cannot trade",
	ReasonVenueSignerUnavailable: "venue: the venue cannot co-sign",
	ReasonVenueHouseNotDedicated: "venue: the venue's house book is not its own",
	ReasonChainFeeChanged:        "market: the chain's fee changed since the plan",
	// The platform's policy.
	ReasonFeeAbovePolicy:               "platform policy: the signing policy's fee bound",
	ReasonSlippageAbovePolicy:          "platform policy: the signing policy's slippage bound",
	RefusalRouteDenied:                 "platform policy: the settlement compiler refused the route",
	RefusalKillSwitch:                  "platform policy: a kill switch is active",
	RefusalGateClosed:                  "platform policy: the capability gate is closed",
	ReasonBlockedBeforeSubmission:      "platform policy: a kill switch or a freeze stopped it before submission",
	RefusalRiskRejected:                "Risk V2's refusal: AuthorityViolationRiskReasons says which of its reasons are violations",
	RefusalSigningRejected:             "the signing provider's refusal: its decision's reason codes say whether it was a violation",
	ReasonWithdrawalComplianceNotClear: "platform policy: the account's identity or sanctions state",
	ReasonWithdrawalRiskScore:          "platform policy: the proposal's risk score, over facts the agent does not choose",
	ReasonWithdrawalPerWithdrawalLimit: "the account's own amount limit, which bounds every withdrawal of the account, not the agent's grant",
	ReasonWithdrawalDailyLimit:         "the account's own daily limit, which bounds every withdrawal of the account, not the agent's grant",
	ReasonWithdrawalVelocity:           "the account's own count limit, which bounds every withdrawal of the account, not the agent's grant",
	RefusalPolicyRejected:              "the withdrawal policy's refusal: its own reason codes say whether it was a violation",
	ReasonHumanNotTestMode:             "not a refusal: the proposal waits for the owner",
	ReasonHumanAutoApprovalDisabled:    "not a refusal: the proposal waits for the owner",
	ReasonHumanAmountAboveAuto:         "not a refusal: the proposal waits for the owner",
	ReasonHumanDestinationTooNew:       "not a refusal: the proposal waits for the owner",
	ReasonHumanAccountTooNew:           "not a refusal: the proposal waits for the owner",
	ReasonHumanRiskScoreAboveAuto:      "not a refusal: the proposal waits for the owner",
	ReasonHumanAnomaly:                 "not a refusal: the proposal waits for the owner",
	RefusalInsufficientFunds:           "the account's funds, not its grant",
	ReasonPolicyMissing:                "state: the account has no withdrawal policy",
	ReasonSignerKeyNotRegistered:       "state: the account has no delegated wallet key on the chain",
	ReasonNetworkUnrecorded:            "state: the subject was admitted before its network was recorded",
	ReasonSubjectNotSignable:           "state: the subject is not in a signable status",
	ReasonApprovalRequired:             "state: the subject is not approved, and an unapproved subject is not signable",
	ReasonAccountNotActive:             "state: the account is frozen or restricted",
	ReasonAgentNotActive:               "state: the agent is paused or retired, by its own lifecycle",
	RefusalAccountFrozen:               "state: the account is frozen",
	RefusalDeclinedByOwner:             "the owner's decision, not the agent's overreach",
	RefusalCancelledByOwner:            "the owner's decision, not the agent's overreach",
	RefusalSubmissionRefused:           "chain: the chain refused the transaction",
	RefusalChainFailed:                 "chain: the transaction failed on the chain",
	RefusalReconcileMismatch:           "chain: the finalized transaction is not the signed one",
	ReasonCapabilityTampered:           "integrity: the grant no longer hashes to what was granted",
	ReasonApprovalPlanMismatch:         "integrity: the approval is of another plan than the one presented",
	ReasonMessageNotPersisted:          "integrity: the message is not the compiled one",
	ReasonSignerKeyMismatch:            "integrity: the wallet is not the registered key",
	ReasonPlanTampered:                 "integrity: the plan no longer hashes to what was planned",
	InspectMalformed:                   "integrity: the transaction does not decode",
	InspectInvalidPlan:                 "integrity: the plan does not compile",
	InspectPlanHashMismatch:            "integrity: the plan's hash",
	InspectChainMismatch:               "integrity: the transaction's chain is not the plan's",
	InspectBlockhashMismatch:           "integrity: the transaction's blockhash",
	InspectFeePayerMismatch:            "integrity: the transaction's fee payer",
	InspectFeeMismatch:                 "integrity: the transaction's fee",
	InspectInstructionCount:            "integrity: the transaction's instructions",
	InspectProgramMismatch:             "integrity: the transaction's programs",
	InspectDestinationMismatch:         "integrity: the transaction pays someone the plan does not",
	InspectTokenAccountMismatch:        "integrity: the transaction's token accounts",
	InspectAmountMismatch:              "integrity: the transaction's amounts",
	InspectSlippageExceeded:            "integrity: the transaction receives less than the plan's minimum",
	InspectMemoMismatch:                "integrity: the transaction's memo",
	InspectSignerSetMismatch:           "integrity: the transaction's signers",
	InspectSignatureInvalid:            "integrity: a signature does not verify",
	InspectNotThePlanned:               "integrity: the transaction is not the planned one",
	VenueNotTheVenue:                   "integrity: the co-signer is not the venue",
	VenueNotThePlan:                    "integrity: the venue's leg is not the plan",
	VenuePaysFee:                       "integrity: the venue would pay the fee",
	VenuePlanTampered:                  "integrity: the venue's view of the plan's hash",
	VenueLegsNotPlanned:                "integrity: the venue's legs are not the planned ones",
}

// TestAuthorityViolationCodes_PinTheSetAndSayWhy pins the set: exactly the
// members documented above, sorted, each answered by IsAuthorityViolation,
// none of them also documented as not a violation.
func TestAuthorityViolationCodes_PinTheSetAndSayWhy(t *testing.T) {
	got := AuthorityViolationCodes()
	assert.True(t, sort.StringsAreSorted(got), "sorted")
	want := make([]string, 0, len(violationWhy))
	for code, why := range violationWhy {
		assert.NotEmpty(t, why, code)
		want = append(want, code)
	}
	sort.Strings(want)
	assert.Equal(t, want, got)
	for _, code := range got {
		assert.True(t, IsAuthorityViolation(code), code)
		_, also := notAViolation[code]
		assert.False(t, also, "%s is documented both as a violation and as not one", code)
	}
	for code, why := range notAViolation {
		assert.False(t, IsAuthorityViolation(code), "%s is not a violation: %s", code, why)
	}
	got[0] = "MUTATED"
	assert.NotEqual(t, "MUTATED", AuthorityViolationCodes()[0], "a copy")
	// The timing refusals the goal names are explicitly not violations.
	assert.False(t, IsAuthorityViolation(ReasonQuoteExpired))
	assert.False(t, IsAuthorityViolation(ReasonCoolingPeriod))
	// A signed subject stopped before submission names what stopped it.
	assert.Equal(t, "BLOCKED_BEFORE_SUBMISSION:DELEGATION_CAPABILITY_REVOKED", BlockedBeforeSubmission(ReasonCapabilityRevoked))
}

// codePattern is what a refusal code looks like.
var codePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// declaredRefusalCodes parses this package's sources and returns every
// string constant named Reason*, Refusal*, Inspect* or Venue* whose value
// is a code: the refusal codes the package can record.
func declaredRefusalCodes(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i >= len(vs.Values) || !hasCodePrefix(n.Name) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					if codePattern.MatchString(v) {
						out[v] = n.Name
					}
				}
			}
		}
	}
	return out
}

func hasCodePrefix(name string) bool {
	for _, p := range []string{"Reason", "Refusal", "Inspect", "Venue"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// TestAuthorityViolationCodes_ClassifyEveryRefusalCode: every refusal code
// this package declares is documented above as a violation or as not one, so
// a code added without deciding which it is fails here.
func TestAuthorityViolationCodes_ClassifyEveryRefusalCode(t *testing.T) {
	declared := declaredRefusalCodes(t)
	require.Greater(t, len(declared), 60, "expected to find the package's refusal codes")
	for code, name := range declared {
		_, v := violationWhy[code]
		_, n := notAViolation[code]
		assert.True(t, v || n, "%s (%s) is neither a violation nor documented as not one", name, code)
	}
	for code := range violationWhy {
		assert.Contains(t, declared, code, "documented but not declared")
	}
	for code := range notAViolation {
		assert.Contains(t, declared, code, "documented but not declared")
	}
}

// TestAuthorityViolationRiskReasons_AreWhatAFailedCheckBecomes: every way a
// capability check can fail at admission reaches Risk V2 through the
// authority section, and the kernel refuses it with one of the reasons the
// set names -- except a capability that is not present, which an admission
// sees only for a tampered grant (the account and the agent are checked
// before the intent exists), and which the kernel calls missing.
func TestAuthorityViolationRiskReasons_AreWhatAFailedCheckBecomes(t *testing.T) {
	reasons := AuthorityViolationRiskReasons()
	assert.True(t, sort.StringsAreSorted(reasons))
	assert.Equal(t, []string{risk.ReasonAuthorityExpired, risk.ReasonAuthorityLimit, risk.ReasonAuthorityRevoked, risk.ReasonAuthorityScope}, reasons)
	assert.NotContains(t, reasons, risk.ReasonAuthorityMissing)

	c := unitCapability(t)
	base := unitUse(c)
	// regrant re-hashes a capability whose terms a case changed: a grant made
	// that way, not a row edited behind the database's back.
	regrant := func(k *Capability) {
		h, err := k.ComputeContentHash()
		require.NoError(t, err)
		k.ContentHash = h
	}
	cases := []struct {
		name   string
		mutate func(*Capability, *Use)
		want   string
	}{
		{"expired", func(_ *Capability, u *Use) { u.Now = c.Terms.ExpiresAt }, risk.ReasonAuthorityExpired},
		{"not yet valid", func(_ *Capability, u *Use) { u.Now = c.GrantedAt.Add(-time.Second) }, risk.ReasonAuthorityExpired},
		{"revoked", func(k *Capability, _ *Use) { k.Status = CapabilityRevoked }, risk.ReasonAuthorityRevoked},
		{"another strategy", func(_ *Capability, u *Use) { u.StrategyID = id.New[id.Any]().String() }, risk.ReasonAuthorityScope},
		{"another environment", func(_ *Capability, u *Use) { u.Environment = config.EnvProd }, risk.ReasonAuthorityScope},
		{"another mode", func(_ *Capability, u *Use) { u.Mode = gates.ModeDevnet }, risk.ReasonAuthorityScope},
		{"an action not granted", func(k *Capability, _ *Use) {
			k.Terms.ActionClasses = []ActionClass{ActionReduceExposure}
			regrant(k)
		}, risk.ReasonAuthorityScope},
		{
			"an instrument class not granted", func(_ *Capability, u *Use) { u.Classes = []InstrumentClass{ClassSpeculativeToken} },
			risk.ReasonAuthorityScope,
		},
		{"a chain not granted", func(_ *Capability, u *Use) { u.Chain = devnethost.ChainEthereum }, risk.ReasonAuthorityScope},
		{"a destination not granted", func(_ *Capability, u *Use) { u.Destination = ukey("elsewhere") }, risk.ReasonAuthorityScope},
		{"over the single limit", func(_ *Capability, u *Use) { u.Notional = money.USDFromMinor(100_001) }, risk.ReasonAuthorityLimit},
		{"over the daily limit", func(_ *Capability, u *Use) { u.DayUsed = money.USDFromMinor(490_001) }, risk.ReasonAuthorityLimit},
		{"over the lifetime limit", func(_ *Capability, u *Use) { u.TotalUsed = money.USDFromMinor(990_001) }, risk.ReasonAuthorityLimit},
		{"a tampered grant", func(k *Capability, _ *Use) { k.Terms.MaxSingleTrade = money.USDFromMinor(900_000) }, risk.ReasonAuthorityMissing},
		{"another account (refused before the intent at admission)", func(_ *Capability, u *Use) {
			u.AccountID = accounts.AccountID(id.New[id.Any]())
		}, risk.ReasonAuthorityMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, u := c, base
			k.Terms.ActionClasses = append([]ActionClass(nil), c.Terms.ActionClasses...)
			tc.mutate(&k, &u)
			check := k.Check(u)
			require.False(t, check.OK())
			for _, code := range check.Codes {
				if code != ReasonCapabilityTampered {
					assert.True(t, IsAuthorityViolation(code), "%s: the check's code %s is a violation", tc.name, code)
				}
			}
			d := risk.EvaluateAdvanced(risk.DefaultAdvancedPolicy(), risk.AdvancedInput{
				Stage: risk.StagePreTrade, Now: u.Now,
				Intent: risk.AdvancedIntent{
					ID: id.New[id.Any]().String(), Kind: "ACQUIRE_EXPOSURE",
					AccountID: c.Terms.AccountID.String(), ActorType: "AGENT", AgentID: c.Terms.AgentID, Capability: string(gates.AgentSigning),
				},
				Authority: check.Snapshot(),
			})
			require.Equal(t, risk.Reject, d.Verdict)
			assert.Contains(t, d.ReasonCodes, tc.want)
			counted := false
			for _, r := range d.ReasonCodes {
				for _, v := range reasons {
					counted = counted || r == v
				}
			}
			assert.Equal(t, tc.want != risk.ReasonAuthorityMissing, counted,
				"a failed check is counted unless the capability is missing from the section")
		})
	}
}
