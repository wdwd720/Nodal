//go:build integration

package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// An operator cannot spend a customer's Credits through the customer API.
//
// This is F-36 over HTTP. `security.RequireAccount` returns nil for anyone
// holding `account:read_any`, and RoleAdmin holds that permission alongside the
// whole customer surface — `native_market:trade`, `commerce:buy`,
// `payout:create`, `withdrawal:create` — because it is every permission except
// the dual-control and agent-only sets. Fourteen write routes scoped through
// that read override, so a single ADMIN session could trade out of any
// customer's balance with no second signature and no admin action.
//
// The unit tests in internal/security prove the two scoping functions answer
// differently. This proves the route actually uses the right one, with a real
// market, a real balance and a real HTTP request — because "the helper is
// correct" and "the handler calls it" are different claims, and F-24 was the
// second one failing while the first held.
func TestIntegration_AnOperatorCannotTradeOutOfACustomersAccount(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	h.launchMarket(t, commerceCreditAsset(t, d))

	customer := seedCustomerAccount(t, d)
	h.fundCredits(t, customer, "10000000000") // 10,000 Credits
	before := h.creditBalance(t, customer)
	require.True(t, before.IsPositive(), "the customer must have something to steal")

	// An ADMIN operator, freshly authenticated, holding account:read_any and
	// native_market:trade. Everything except ownership of that account.
	admin := seedOperator(t, d, security.RoleAdmin)
	res := h.as(&admin).do(http.MethodPost,
		"/v1/native-markets/"+h.market.ID.String()+"/orders",
		map[string]any{
			"account_id": customer.String(),
			"side":       "BUY",
			"amount":     "1000000000",
			"min_output": "0",
		}, "Idempotency-Key", newKey())

	assert.Equal(t, http.StatusForbidden, res.Code,
		"an operator trading out of a customer's balance must be refused; body=%s", res.Body.String())
	assert.Equal(t, before.String(), h.creditBalance(t, customer).String(),
		"the customer's Credits must not have moved")

	// The same operator may still READ that account, which is what
	// account:read_any is for and what the fix must not have broken.
	read := h.as(&admin).do(http.MethodGet, "/v1/accounts/"+customer.String(), nil)
	assert.Equal(t, http.StatusOK, read.Code, "body=%s", read.Body.String())

	// And they may still trade out of their OWN account. An operator is a
	// customer too; the fix is about whose account, not about who they are.
	own := seedCustomerAccount(t, d)
	admin.AccountIDs = append(admin.AccountIDs, own.String())
	admin.AuthTime = testNow.Add(-time.Minute)
	h.fundCredits(t, own, "10000000000")
	mine := h.as(&admin).do(http.MethodPost,
		"/v1/native-markets/"+h.market.ID.String()+"/orders",
		map[string]any{
			"account_id": own.String(),
			"side":       "BUY",
			"amount":     "1000000000",
			"min_output": "0",
		}, "Idempotency-Key", newKey())
	assert.Equal(t, http.StatusCreated, mine.Code, "body=%s", mine.Body.String())
}

// TestIntegration_ACustomerCanTradeOverHTTP is F-37, and it is the reason the
// test above needed a real port rather than the admin plane alone.
//
// POST /v1/native-markets/{marketId}/orders had never worked in any
// deployment. `ExecuteRequest` requires `effective_at`; the handler does not
// set it, and the adapter -- where the payout and commerce adapters both set
// theirs from the deployment clock -- did not either. Every request was refused
// VALIDATION_FAILED. Nothing noticed because nothing drove this route over
// HTTP: the load script says in its own comment that it does not trade, and the
// browser suite buys from the marketplace, which goes through a different
// adapter.
//
// So this walks it: a customer, their own account, a live market, one request,
// and units arriving.
func TestIntegration_ACustomerCanTradeOverHTTP(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	h.launchMarket(t, commerceCreditAsset(t, d))

	account := seedCustomerAccount(t, d)
	h.fundCredits(t, account, "10000000000")
	customer := security.Principal{
		SubjectID: "cust-" + newKey()[:8], ActorType: security.ActorUser,
		Roles:      []security.Role{security.RoleCustomer},
		AccountIDs: []string{account.String()},
		SessionID:  testSessionID, AuthTime: testNow.Add(-time.Minute),
		AMR: []string{"pwd"},
	}

	before := h.creditBalance(t, account)
	res := h.as(&customer).do(http.MethodPost,
		"/v1/native-markets/"+h.market.ID.String()+"/orders",
		map[string]any{
			"account_id": account.String(),
			"side":       "BUY",
			"amount":     "1000000000",
			"min_output": "1",
		}, "Idempotency-Key", newKey())
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())

	var fill struct {
		AssetsOut string `json:"assets_out"`
		CreditsIn string `json:"credits_in"`
	}
	res.json(&fill)
	assert.NotEmpty(t, fill.AssetsOut, "a filled order must say what the buyer received")

	after := h.creditBalance(t, account)
	assert.Equal(t, before.Sub(after).String(), fill.CreditsIn,
		"the Credits that left the account must be the Credits the fill reports")
}

func (h *domainAHarness) creditBalance(t *testing.T, account interface{ String() string }) money.Quantity {
	t.Helper()
	var text string
	require.NoError(t, h.db.QueryRow(t.Context(),
		`SELECT coalesce((SELECT b.balance FROM ledger_accounts la
		    JOIN ledger_balances b ON b.ledger_account_id = la.id
		   WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1
		     AND la.code = $2), 0)::text`,
		account.String(), string(ledger.CodeCreditBalance)).Scan(&text))
	q, err := money.ParseQuantity(text)
	require.NoError(t, err)
	return q
}
