package httpapi

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/gen/api"
)

// The tests in this file assert structural properties of the HTTP surface that
// no amount of handler review can guarantee by inspection. They are cheap and
// they fail loudly if the surface ever widens.

// TestThereIsNoBalanceEditingEndpoint: money moves only through the domain
// packages' posting paths. The API exposes no route that edits a balance or
// writes a ledger row, and the ledger port it holds is read-only.
func TestThereIsNoBalanceEditingEndpoint(t *testing.T) {
	t.Parallel()

	// The ledger port can only read.
	lt := reflect.TypeOf((*LedgerPort)(nil)).Elem()
	require.Equal(t, 1, lt.NumMethod())
	assert.Equal(t, "ListTransactions", lt.Method(0).Name)

	forbidden := []string{
		"balance", "balances", "post", "posting", "journal/entries",
		"credit", "credits", "debit", "adjust", "adjustment", "mint", "burn",
	}
	h := newHarness(t)
	for _, probe := range mountedRoutes(t, h.server) {
		lower := strings.ToLower(probe.path)
		readOnly := probe.method == "GET" || probe.method == "HEAD"
		for _, word := range forbidden {
			// "/ledger/transactions" is a read; it is the only route whose
			// path names the ledger at all, and it is a GET.
			if word == "post" && probe.method == "POST" {
				continue
			}
			if !strings.Contains(lower, "/"+word) {
				continue
			}
			// The invariant is about EDITING a balance, and a GET cannot edit
			// anything. GET /v1/credits/balance is the read PART XX requires:
			// gross, spendable, frozen and payout-eligible, with the reasons
			// for the gap. Refusing the word outright would forbid the product
			// from telling a user what they hold.
			//
			// So the check is narrowed by method and widened in exchange: a
			// state-changing route may not contain any of these words at all,
			// which now also forbids POST /credits/anything — something the
			// previous version permitted, because it never looked at the
			// method.
			assert.True(t, readOnly,
				"%s %s changes state and names a balance concept; money moves only through the domain packages' posting paths",
				probe.method, probe.path)
		}
		if strings.Contains(lower, "/ledger") {
			assert.Equal(t, "GET", probe.method, "the ledger is read-only over HTTP")
		}
	}

	// And the balance-shaped reads that DO exist are reads.
	for _, probe := range mountedRoutes(t, h.server) {
		lower := strings.ToLower(probe.path)
		if strings.Contains(lower, "/credits/") || strings.Contains(lower, "/balance") {
			assert.Equal(t, "GET", probe.method,
				"%s %s must be a read; Credits move only through internal/credit", probe.method, probe.path)
		}
	}
}

// TestThereIsNoSigningWalletOrPolicyEndpoint: no endpoint lets any caller
// sign, hold a key, or change risk or capital policy. Those live behind the
// signing service, the wallet provider and the admin action table, none of
// which the HTTP surface exposes.
func TestThereIsNoSigningWalletOrPolicyEndpoint(t *testing.T) {
	t.Parallel()
	forbidden := []string{
		"/sign", "/signing", "/wallet", "/wallets", "/keys", "/key",
		"/risk/policy", "/capital", "/envelope", "/envelopes", "/reservations",
	}
	h := newHarness(t)
	for _, probe := range mountedRoutes(t, h.server) {
		lower := strings.ToLower(probe.path)
		for _, word := range forbidden {
			assert.NotContains(t, lower, word,
				"%s %s exposes an authority the HTTP surface must not have", probe.method, probe.path)
		}
	}
}

// TestOnlyTheDeclaredMutatingOperationsChangeState: every POST or DELETE the
// server mounts must be marked Mutating in its policy (and therefore carry an
// Idempotency-Key) unless it is one of the three deliberate exceptions.
func TestEveryStateChangingRouteIsMarkedMutating(t *testing.T) {
	t.Parallel()
	// Four POST/DELETE operations deliberately change nothing that needs an
	// idempotency record: a quote preview reserves nothing and persists
	// nothing, logout is idempotent by construction, the webhook endpoint is
	// deduplicated by the provider event id inside internal/webhook, and a
	// session revocation is idempotent on the session id. The spec gives
	// none of them an Idempotency-Key parameter.
	exceptions := map[string]struct{}{
		"PostQuotesPreview":       {},
		"PostAuthLogout":          {},
		"PostWebhooksProvider":    {},
		"DeleteSessionsSessionId": {},
	}
	for op, pol := range operationPolicies {
		if strings.HasPrefix(op, "Get") {
			assert.False(t, pol.Mutating, "%s is a read and must not be marked mutating", op)
			continue
		}
		if _, ok := exceptions[op]; ok {
			continue
		}
		assert.True(t, pol.Mutating, "%s changes state and must be marked mutating", op)
	}
}

// TestEveryMutatingOperationRequiresAnIdempotencyKey: the generated request
// object of every mutating operation must carry a non-pointer
// IdempotencyKey field, which is how the spec marks the header required. A
// spec change that made it optional would silently drop the guarantee.
func TestEveryMutatingOperationRequiresAnIdempotencyKey(t *testing.T) {
	t.Parallel()
	exceptions := map[string]struct{}{
		"PostQuotesPreview":       {},
		"PostAuthLogout":          {},
		"PostWebhooksProvider":    {},
		"DeleteSessionsSessionId": {},
	}
	iface := reflect.TypeOf((*api.StrictServerInterface)(nil)).Elem()
	for i := 0; i < iface.NumMethod(); i++ {
		m := iface.Method(i)
		pol, ok := policyFor(m.Name)
		if !ok || !pol.Mutating {
			continue
		}
		if _, skip := exceptions[m.Name]; skip {
			continue
		}
		// The request object is the method's second input after ctx.
		req := m.Type.In(1)
		params, found := req.FieldByName("Params")
		require.True(t, found, "%s has no Params on its request object", m.Name)
		key, found := params.Type.FieldByName("IdempotencyKey")
		require.True(t, found, "%s does not take an Idempotency-Key", m.Name)
		assert.Equal(t, reflect.String, key.Type.Kind(),
			"%s must take a required (non-pointer) Idempotency-Key", m.Name)
	}
}
