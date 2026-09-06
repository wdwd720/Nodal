package privy

import (
	"fmt"
	"sort"
	"strings"

	privyclient "github.com/privy-io/go-sdk"
)

// DefaultPolicyRules renders the policy shape privy.md recommends for a
// delegated swap wallet: allow signTransaction only when every instruction's
// programId is in allowed, then deny everything. It is exported so
// operators can create the policy with the same allow-list the adapter
// verifies; the adapter itself never creates policies (POST /v1/policies is
// UNKNOWN_EFFECT_WRITE without an idempotency key).
func DefaultPolicyRules(allowed []string) []privyclient.PolicyNewParamsRule {
	programs := append([]string(nil), allowed...)
	sort.Strings(programs)
	return []privyclient.PolicyNewParamsRule{
		{
			Name:   "allow-known-programs",
			Method: privyclient.PolicyMethodSignTransaction,
			Action: privyclient.PolicyActionAllow,
			Conditions: []privyclient.PolicyConditionUnion{{
				OfSolanaProgramInstruction: &privyclient.SolanaProgramInstructionCondition{
					Field:       privyclient.SolanaProgramInstructionConditionFieldProgramID,
					FieldSource: privyclient.SolanaProgramInstructionConditionFieldSourceSolanaProgramInstruction,
					Operator:    privyclient.ConditionOperatorIn,
					Value:       privyclient.ConditionValueUnion{OfStringArray: programs},
				},
			}},
		},
		{
			Name:       "deny-everything-else",
			Method:     privyclient.PolicyMethodStar,
			Action:     privyclient.PolicyActionDeny,
			Conditions: []privyclient.PolicyConditionUnion{},
		},
	}
}

// policyRule is the provider-neutral view of one policy rule used by
// verifyPolicy (and by tests without the SDK types).
type policyRule struct {
	Method     string
	Action     string
	Conditions []policyCondition
}

type policyCondition struct {
	FieldSource string
	Field       string
	Operator    string
	Values      []string
}

func fromSDKPolicy(p *privyclient.Policy) (chainType string, rules []policyRule) {
	if p == nil {
		return "", nil
	}
	for _, r := range p.Rules {
		pr := policyRule{Method: string(r.Method), Action: string(r.Action)}
		for _, c := range r.Conditions {
			pc := policyCondition{FieldSource: c.FieldSource, Field: c.Field, Operator: c.Operator}
			if c.Value.OfString != "" {
				pc.Values = []string{c.Value.OfString}
			}
			pc.Values = append(pc.Values, c.Value.OfStringArray...)
			pr.Conditions = append(pr.Conditions, pc)
		}
		rules = append(rules, pr)
	}
	return string(p.ChainType), rules
}

// verifyPolicy checks that a policy enforces exactly the configured
// programId allow-list for signTransaction and denies everything else. It
// returns the allow-listed programs and a list of problems (empty = verified).
func verifyPolicy(chainType string, rules []policyRule, want []string) (allowed, problems []string) {
	if chainType != string(privyclient.WalletChainTypeSolana) {
		problems = append(problems, fmt.Sprintf("policy chain_type %q is not solana", chainType))
	}
	wantSet := map[string]struct{}{}
	for _, w := range want {
		wantSet[w] = struct{}{}
	}
	var (
		allowRule    *policyRule
		denyAllFound bool
	)
	for i := range rules {
		r := rules[i]
		switch strings.ToUpper(r.Action) {
		case "ALLOW":
			if r.Method != string(privyclient.PolicyMethodSignTransaction) {
				problems = append(problems, fmt.Sprintf("ALLOW rule for method %q (only signTransaction may be allowed)", r.Method))
				continue
			}
			if allowRule != nil {
				problems = append(problems, "more than one ALLOW rule for signTransaction")
				continue
			}
			allowRule = &r
		case "DENY":
			if r.Method == string(privyclient.PolicyMethodStar) && len(r.Conditions) == 0 {
				denyAllFound = true
			}
		default:
			problems = append(problems, fmt.Sprintf("rule with unknown action %q", r.Action))
		}
	}
	if allowRule == nil {
		problems = append(problems, "no ALLOW rule for signTransaction")
		return nil, problems
	}
	if len(allowRule.Conditions) != 1 {
		problems = append(problems, fmt.Sprintf("signTransaction ALLOW rule has %d conditions, expected exactly one programId condition", len(allowRule.Conditions)))
		return nil, problems
	}
	c := allowRule.Conditions[0]
	if c.FieldSource != string(privyclient.SolanaProgramInstructionConditionFieldSourceSolanaProgramInstruction) ||
		c.Field != string(privyclient.SolanaProgramInstructionConditionFieldProgramID) ||
		c.Operator != string(privyclient.ConditionOperatorIn) {
		problems = append(problems, fmt.Sprintf("signTransaction ALLOW condition is %s/%s/%s, expected solana_program_instruction/programId/in", c.FieldSource, c.Field, c.Operator))
	}
	got := map[string]struct{}{}
	for _, v := range c.Values {
		got[v] = struct{}{}
		allowed = append(allowed, v)
	}
	sort.Strings(allowed)
	for w := range wantSet {
		if _, ok := got[w]; !ok {
			problems = append(problems, "policy does not allow required program "+w)
		}
	}
	for g := range got {
		if _, ok := wantSet[g]; !ok {
			problems = append(problems, "policy allows unexpected program "+g)
		}
	}
	if !denyAllFound {
		problems = append(problems, "no final DENY rule for method *")
	}
	sort.Strings(problems)
	return allowed, problems
}
