//go:build integration

package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/security"
)

// What a by-id read tells a stranger.
//
// Three routes fetched a record by id and then decided what to do about it, and
// each leaked something in the gap:
//
//	GET /v1/payouts/{id}            403 for a foreign payout, 404 for an absent
//	                                one — a membership oracle, and its own
//	                                sibling POST /payouts/{id}/cancel has
//	                                answered 404 to both since F-29
//	GET /v1/internal-products/{id}  a DRAFT product of another seller, with its
//	                                price and fee split, to any customer
//	GET /v1/native-assets/{id}      a DRAFT asset with its moderation state and
//	                                the moderator's notes, to any customer
//
// The list routes beside each of them are filtered — `ListActive`, `ListTradable`
// — so the leak is only through the by-id form. All three now answer NOT_FOUND
// (F-41).

func customerOwning(t *testing.T, account accounts.AccountID) security.Principal {
	t.Helper()
	return security.Principal{
		SubjectID: "cust-" + newKey()[:8], ActorType: security.ActorUser,
		Roles:      []security.Role{security.RoleCustomer},
		AccountIDs: []string{account.String()},
		SessionID:  testSessionID, AuthTime: testNow.Add(-time.Minute),
		AMR: []string{"pwd"},
	}
}

// TestIntegration_ADraftAssetIsNotReadableByAStranger. The response carries
// moderation_state and moderation_notes: internal commentary about somebody
// else's unpublished work.
func TestIntegration_ADraftAssetIsNotReadableByAStranger(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)

	draft := h.newDraftAsset(t)
	stranger := customerOwning(t, seedCustomerAccount(t, d))

	res := h.as(&stranger).do(http.MethodGet, "/v1/native-assets/"+draft.AssetID.String(), nil)
	assert.Equal(t, http.StatusNotFound, res.Code,
		"a stranger must not read an unpublished asset; body=%s", res.Body.String())
	assert.NotContains(t, res.Body.String(), "moderation",
		"and must not be told anything about its moderation")

	// Its creator still reads it — the fix is about strangers, not about
	// hiding somebody's own draft from them.
	creator := customerOwning(t, h.creator)
	mine := h.as(&creator).do(http.MethodGet, "/v1/native-assets/"+draft.AssetID.String(), nil)
	assert.Equal(t, http.StatusOK, mine.Code, "body=%s", mine.Body.String())

	// And a live asset is public, which is the whole point of a market.
	h.launchMarket(t, commerceCreditAsset(t, d))
	live := h.as(&stranger).do(http.MethodGet, "/v1/native-assets/"+h.asset.AssetID.String(), nil)
	assert.Equal(t, http.StatusOK, live.Code, "body=%s", live.Body.String())
}

// TestIntegration_AForeignPayoutIsIndistinguishableFromAnAbsentOne.
func TestIntegration_AForeignPayoutIsIndistinguishableFromAnAbsentOne(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)

	stranger := customerOwning(t, seedCustomerAccount(t, d))
	absent := h.as(&stranger).do(http.MethodGet, "/v1/payouts/"+newKey(), nil)
	require.Equal(t, http.StatusNotFound, absent.Code, "body=%s", absent.Body.String())

	// A payout that exists and belongs to somebody else must answer the same.
	// Seeded directly: the point is the READ, and reaching the write path would
	// drag in the payout policy and the capability gates.
	other := seedCustomerAccount(t, d)
	payoutID := seedPayoutRow(t, d, other)
	foreign := h.as(&stranger).do(http.MethodGet, "/v1/payouts/"+payoutID, nil)

	assert.Equal(t, absent.Code, foreign.Code,
		"a payout that exists and one that does not must answer identically; body=%s", foreign.Body.String())
	assert.Equal(t, absent.problem().Code, foreign.problem().Code)
}

// seedPayoutRow writes a payout request for an account directly, because this
// test is about the read.
//
// `sandbox` is stated because 00817 made it NOT NULL: the nullable form was read
// as a rehearsal by the API and as a real payout by the PROD CHECK, so one of
// the two was wrong on every row that had no recorded fact (D-134).
func seedPayoutRow(t *testing.T, d *db.DB, account accounts.AccountID) string {
	t.Helper()
	asset := commerceCreditAsset(t, d)
	id := newKey()
	_, err := d.Exec(t.Context(),
		`INSERT INTO payout_requests
		   (id, account_id, credit_asset_id, requested_quantity, reserved_quantity, state,
		    policy_version, policy_hash, idempotency_key, sandbox, environment, created_at, updated_at)
		 VALUES ($1,$2,$3,1,0,'DRAFT','itest','itest',$4, true, 'TEST', now(), now())`,
		id, account.String(), asset.String(), "seed-"+id)
	require.NoError(t, err, "seeding a payout row")
	return id
}
