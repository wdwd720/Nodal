// Package infra holds tests about the deployment identity's IAM policies.
//
// They exist because those documents are pasted into an AWS console by hand,
// once, and are then the only thing standing between a compromised Terraform
// session and every other project in the account. A policy is not code that
// fails loudly when it is wrong. It is a JSON file that quietly permits more
// than it was meant to, so the invariants are asserted here instead.
package infra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// maxManagedPolicy is IAM's limit for a customer-managed policy. AWS does
	// not count whitespace, so the check compacts the document first.
	maxManagedPolicy = 6144
	account          = "049286562577"
	dir              = "../../infra/aws"
	boundaryARN      = "arn:aws:iam::049286562577:policy/nodal-task-boundary"
)

type statement struct {
	Sid       string                     `json:"Sid"`
	Principal map[string]json.RawMessage `json:"Principal"`
	Effect    string                     `json:"Effect"`
	Action    json.RawMessage            `json:"Action"`
	Resource  json.RawMessage            `json:"Resource"`
	Condition map[string]any             `json:"Condition"`
}

type policy struct {
	Version   string      `json:"Version"`
	Statement []statement `json:"Statement"`
}

// strings accepts IAM's "one string or a list of them" shape.
func strs(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	require.NoError(t, json.Unmarshal(raw, &many))
	return many
}

func load(t *testing.T, name string) (policy, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err, "%s must exist; BOOTSTRAP.md tells an operator to paste it", name)
	var p policy
	require.NoError(t, json.Unmarshal(b, &p), "%s must be valid JSON", name)
	require.Equal(t, "2012-10-17", p.Version, "%s", name)
	require.NotEmpty(t, p.Statement, "%s", name)
	return p, b
}

// policies is every document an operator creates at bootstrap.
var policies = []string{
	"nodal-terraform-read-policy.json",
	"nodal-terraform-network-policy.json",
	"nodal-terraform-stack-policy.json",
	"nodal-terraform-iam-policy.json",
	"nodal-task-boundary-policy.json",
	"nodal-operator-policy.json",
}

// trustPolicy is the role's own trust document. It is not in `policies`
// because it is a different policy type with a different size limit, and
// because the thing that can be wrong with it is who it names rather than what
// it permits.
const trustPolicy = "nodal-terraform-trust-policy.json"

// maxTrustPolicy is IAM's default limit for an assume-role policy document.
const maxTrustPolicy = 2048

// TestPolicies_FitInsideIAMsLimit is why the single policy was split. It had
// grown to 5,786 of 6,144 characters, which is not a limit you discover in a
// review: you discover it when an apply fails and somebody deletes a statement
// to make room.
func TestPolicies_FitInsideIAMsLimit(t *testing.T) {
	t.Parallel()
	for _, name := range policies {
		p, _ := load(t, name)
		compact, err := json.Marshal(p)
		require.NoError(t, err)
		size := len(compact)
		assert.Less(t, size, maxManagedPolicy, "%s is %d characters", name, size)
		// Headroom, not just fit. A document with fifty characters spare is one
		// permission away from the same problem.
		assert.Less(t, size, maxManagedPolicy-500,
			"%s has only %d characters spare; split it before adding more", name, maxManagedPolicy-size)
	}
}

// unscopeable lists every Allow that names Resource "*" with no Condition, and
// the reason AWS gives us no alternative. It is a closed list on purpose: a new
// unscoped grant fails this test and has to be argued for here, in writing,
// rather than appearing in a diff nobody reads as widening.
var unscopeable = map[string]string{
	// Read-only. None of these return data, only the existence and shape of
	// resources, and EC2's Describe family accepts no resource ARN at all.
	"nodal-terraform-read-policy.json/MetadataAWSCannotScopeToAResource": "Describe/List APIs; EC2 and ELB accept no resource ARN, and none of them returns object data or a secret value",

	// Network. A VPC has no name until it exists, so a create cannot be scoped
	// to one. Creates only add; every mutation is gated by tag below.
	"nodal-terraform-network-policy.json/CreateNetworkObjectsThatHaveNoNameUntilTheyExist": "the resource being created has no ARN to scope to; these actions add and never modify or delete",

	// Stack. Same reason, plus two AWS APIs that are genuinely account-wide.
	"nodal-terraform-stack-policy.json/CreatesAWSWillNotLetUsNameInAdvance": "certificate and KMS key ARNs are server-generated; task definitions and scalable targets take no resource condition",
	"nodal-terraform-stack-policy.json/WAFLoggingNeedsToWriteALogDelivery":  "logs:*LogDelivery and logs:PutResourcePolicy are account-scoped APIs with no resource form",

	// Boundary. A ceiling, not a grant: it can only ever narrow the role it is
	// attached to.
	"nodal-task-boundary-policy.json/TelemetryAndRegistryAuthHaveNoResource": "PutMetricData, GetAuthorizationToken and the X-Ray writers take no resource; this document is a ceiling and grants nothing by itself",
}

// TestPolicies_EveryUnscopedAllowIsAccountedFor is the isolation property, as a
// test rather than as a paragraph.
//
// A statement that allows an action on Resource "*" with no condition can reach
// any resource in the account, including the Lightsail project and anything
// else living here. Each one must either be scoped to a nodal ARN, gated on the
// Project tag, or listed above with the reason AWS leaves no third option.
func TestPolicies_EveryUnscopedAllowIsAccountedFor(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, name := range policies {
		p, _ := load(t, name)
		for _, st := range p.Statement {
			if st.Effect != "Allow" {
				continue // a Deny on "*" is the point of a Deny.
			}
			require.NotEmpty(t, st.Sid, "%s: every statement needs a Sid to be talked about", name)
			key := name + "/" + st.Sid

			scoped := true
			for _, r := range strs(t, st.Resource) {
				if r == "*" {
					scoped = false
				}
			}
			if scoped || len(st.Condition) > 0 {
				assert.NotContains(t, unscopeable, key,
					"%s is scoped or conditioned now; remove it from the unscopeable list", key)
				continue
			}
			reason, ok := unscopeable[key]
			assert.True(t, ok,
				"%s allows %v on every resource in the account with no condition; scope it to a nodal ARN, gate it on aws:ResourceTag/Project, or record here why AWS cannot",
				key, strs(t, st.Action))
			assert.NotEmpty(t, reason)
			seen[key] = true
		}
	}
	for key := range unscopeable {
		assert.True(t, seen[key], "%s is excused and no longer exists; delete the excuse", key)
	}
}

// TestPolicies_NamedResourcesAreOursAndOnlyOurs: every ARN the policies name
// belongs to this account and to the nodal namespace. One ARN with a stray
// wildcard in the wrong segment is how a "scoped" policy reaches a whole
// service.
func TestPolicies_NamedResourcesAreOursAndOnlyOurs(t *testing.T) {
	t.Parallel()
	// The two AWS-owned ARNs the stack legitimately references.
	allowedForeign := map[string]bool{
		"arn:aws:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole": true,
	}
	for _, name := range policies {
		p, _ := load(t, name)
		for _, st := range p.Statement {
			for _, r := range strs(t, st.Resource) {
				if r == "*" || allowedForeign[r] {
					continue
				}
				if strings.Contains(r, "${aws:username}") {
					continue // the operator's own user and MFA device, named by variable
				}
				require.True(t, strings.HasPrefix(r, "arn:aws:"), "%s: %q", name, r)
				parts := strings.SplitN(r, ":", 6)
				require.Len(t, parts, 6, "%s: %q is not a six-field ARN", name, r)
				acct := parts[4]
				assert.True(t, acct == account || acct == "",
					"%s: %q names account %q, which is not this one", name, r, acct)
				tail := parts[5]
				assert.True(t,
					strings.Contains(tail, "nodal-") || strings.Contains(tail, "nodal/") ||
						strings.Contains(tail, "token.actions.githubusercontent.com") ||
						strings.Contains(tail, "RDSOSMetrics"),
					"%s: %q names neither the nodal namespace nor a documented exception", name, r)
			}
		}
	}
}

// TestPolicies_TheDeploymentIdentityCannotRewriteItself.
//
// The IAM policy lets Terraform create roles and policies under nodal-*, which
// is the namespace its own policies live in. Without an explicit Deny, a
// compromised session could publish a new version of nodal-terraform-iam
// granting itself everything, and every other restriction here would be
// decorative. This asserts the Deny exists and covers all four documents plus
// the role.
func TestPolicies_TheDeploymentIdentityCannotRewriteItself(t *testing.T) {
	t.Parallel()
	p, _ := load(t, "nodal-terraform-iam-policy.json")

	var denied []string
	for _, st := range p.Statement {
		if st.Effect != "Deny" {
			continue
		}
		acts := strs(t, st.Action)
		if !slicesContains(acts, "iam:*") {
			continue
		}
		denied = append(denied, strs(t, st.Resource)...)
	}
	require.NotEmpty(t, denied, "nothing denies iam:* on the deployment identity's own documents")

	for _, must := range []string{
		"arn:aws:iam::" + account + ":policy/nodal-terraform-*",
		"arn:aws:iam::" + account + ":policy/nodal-task-boundary",
		"arn:aws:iam::" + account + ":role/nodal-terraform",
	} {
		assert.Contains(t, denied, must, "a compromised session could rewrite %s", must)
	}
}

// TestPolicies_EveryRoleTerraformCreatesCarriesTheBoundary: the Deny above
// stops the identity rewriting its own policy. This stops the other escape,
// which is creating a fresh role with an inline policy granting everything and
// a trust policy naming itself.
func TestPolicies_EveryRoleTerraformCreatesCarriesTheBoundary(t *testing.T) {
	t.Parallel()
	p, _ := load(t, "nodal-terraform-iam-policy.json")

	var createRole *statement
	for i, st := range p.Statement {
		if st.Effect == "Allow" && slicesContains(strs(t, st.Action), "iam:CreateRole") {
			createRole = &p.Statement[i]
		}
	}
	require.NotNil(t, createRole, "iam:CreateRole must be allowed by exactly one statement")
	require.NotEmpty(t, createRole.Condition, "iam:CreateRole with no condition can create an unbounded role")

	eq, ok := createRole.Condition["StringEquals"].(map[string]any)
	require.True(t, ok, "expected a StringEquals condition on iam:CreateRole")
	assert.Equal(t, boundaryARN, eq["iam:PermissionsBoundary"],
		"iam:CreateRole must require the boundary, or the boundary is advisory")
}

// TestPolicies_TheBoundaryCannotBeUsedToReachIAMOrAnotherProject: a boundary
// that permitted iam:* would bound nothing worth bounding.
func TestPolicies_TheBoundaryCannotBeUsedToReachIAMOrAnotherProject(t *testing.T) {
	t.Parallel()
	p, _ := load(t, "nodal-task-boundary-policy.json")

	var denied []string
	for _, st := range p.Statement {
		if st.Effect == "Deny" {
			denied = append(denied, strs(t, st.Action)...)
		}
	}
	for _, must := range []string{"iam:*", "lightsail:*", "organizations:*", "account:*", "sts:AssumeRole"} {
		assert.Contains(t, denied, must,
			"a role created under this boundary could still reach %s", must)
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestTrust_NamesOnePrincipalAndItIsNotTheWholeAccount.
//
// "arn:aws:iam::<account>:root" in a trust policy does not mean the root user.
// It means every principal in the account, delegating the decision to whatever
// identity-based policies happen to exist. It is the single easiest way to
// write a trust policy that is far wider than it reads, so it is refused here
// by name.
func TestTrust_NamesOnePrincipalAndItIsNotTheWholeAccount(t *testing.T) {
	t.Parallel()
	p, _ := load(t, trustPolicy)
	compact, err := json.Marshal(p)
	require.NoError(t, err)
	assert.Less(t, len(compact), maxTrustPolicy, "trust policies have their own, smaller limit")

	require.Len(t, p.Statement, 1, "one way in, so there is one thing to read")
	st := p.Statement[0]
	assert.Equal(t, "Allow", st.Effect)
	assert.Equal(t, []string{"sts:AssumeRole"}, strs(t, st.Action))

	require.NotEmpty(t, st.Principal, "a trust policy with no principal trusts nothing or everything")
	for kind, raw := range st.Principal {
		for _, who := range strs(t, raw) {
			assert.NotEqual(t, "arn:aws:iam::"+account+":root", who,
				"%q as a principal is the whole account, not the root user", who)
			assert.NotEqual(t, "*", who, "a wildcard principal trusts every AWS account there is")
			assert.NotEqual(t, account, who, "a bare account id is the same delegation as :root")
			if kind == "AWS" {
				assert.True(t, strings.HasPrefix(who, "arn:aws:iam::"+account+":"),
					"%q is not a principal in this account", who)
			}
		}
	}
}

// TestTrust_RequiresMFA: the principal it names signs in with a password. The
// role it guards can create and destroy every resource in the stack, so a
// stolen password must not be enough on its own.
func TestTrust_RequiresMFA(t *testing.T) {
	t.Parallel()
	p, _ := load(t, trustPolicy)
	st := p.Statement[0]
	require.NotEmpty(t, st.Condition, "no condition means a password is enough")
	b, ok := st.Condition["Bool"].(map[string]any)
	require.True(t, ok, "expected a Bool condition")
	assert.Equal(t, "true", b["aws:MultiFactorAuthPresent"])
}

// TestOperator_CanBecomeTheRoleAndDoNothingElse: the operator identity is a
// doorway, not a set of permissions. Everything it is allowed to do is either
// assuming the deployment role or looking after its own credentials.
func TestOperator_CanBecomeTheRoleAndDoNothingElse(t *testing.T) {
	t.Parallel()
	p, _ := load(t, "nodal-operator-policy.json")
	for _, st := range p.Statement {
		assert.Equal(t, "Allow", st.Effect)
		for _, a := range strs(t, st.Action) {
			switch {
			case a == "sts:AssumeRole":
			case strings.HasPrefix(a, "iam:") && strings.Contains(a+"|", "MFADevice|"):
			case a == "iam:ChangePassword" || a == "iam:GetUser" || a == "iam:ListMFADevices":
			default:
				t.Errorf("the operator may do %q, which is neither becoming the role nor self-service", a)
			}
		}
		for _, r := range strs(t, st.Resource) {
			assert.True(t,
				r == "arn:aws:iam::"+account+":role/nodal-terraform" ||
					strings.Contains(r, "${aws:username}"),
				"the operator names %q, which is neither the role nor its own credentials", r)
		}
	}
}
