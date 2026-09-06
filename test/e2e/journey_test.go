//go:build integration && e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// seedUSDCBaseUnits is the SEED posting scripts/seed writes to customer-a's
// WALLET: 10,000.000000 USDC at 6 decimals.
const (
	seedUSDCBaseUnits = "10000000000"
	seedUSDCValueUSD  = "10000.00"
)

type apiAccount struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

type apiPrincipal struct {
	SubjectID  string   `json:"subject_id"`
	ActorType  string   `json:"actor_type"`
	Roles      []string `json:"roles"`
	AccountIDs []string `json:"account_ids"`
	AMR        []string `json:"amr"`
	AuthTime   string   `json:"auth_time"`
}

type apiHoldings struct {
	AsOf     string `json:"as_of"`
	Holdings []struct {
		Asset       string `json:"asset"`
		Symbol      string `json:"symbol"`
		Chain       string `json:"chain"`
		MintAddress string `json:"mint_address"`
		Decimals    int    `json:"decimals"`
		Quantity    string `json:"quantity"`
		USDMark     string `json:"usd_mark"`
	} `json:"holdings"`
}

type apiJournalPage struct {
	NextCursor *string `json:"next_cursor"`
	Items      []struct {
		ID          string `json:"id"`
		Kind        string `json:"kind"`
		Description string `json:"description"`
		ContentHash string `json:"content_hash"`
		Reference   struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"reference"`
		Entries []struct {
			Seq         int    `json:"seq"`
			AccountCode string `json:"account_code"`
			Asset       string `json:"asset"`
			Side        string `json:"side"`
			Quantity    string `json:"quantity"`
		} `json:"entries"`
	} `json:"items"`
}

type apiInstrument struct {
	ID            string `json:"id"`
	CanonicalName string `json:"canonical_name"`
	Status        string `json:"status"`
	Type          string `json:"type"`
}

// TestE2E_CustomerJourney is the spine: one signed-in customer walking the
// whole read surface of the real binary, then signing out. Every step asserts
// state, not merely a status code — the seeded capital must be visible to the
// cent, the seed posting must be in the journal, and the cookie must stop
// working the moment the session is revoked. A revoked session that still
// works is the defect this catches.
func TestE2E_CustomerJourney(t *testing.T) {
	requireEnv(t)
	srv := startAPI(t)
	srv.dumpLogs(t)
	c := newClient(t, srv)
	ctx := t.Context()

	identity := "customer-a"
	accountID := seeded.CustomerAAccountID
	userID := seeded.CustomerAUserID
	if e2eBreak(t, "journey_wrong_account") {
		identity = "customer-b"
		accountID = seeded.CustomerBAccountID
		userID = seeded.CustomerBUserID
	}

	s := c.signIn(ctx, identity+":mfa")
	require.Equal(t, "cp_session", s.cookieName,
		"with CP_AUTH_COOKIE_SECURE=false the cookie is the unprefixed name")

	t.Run("principal", func(t *testing.T) {
		resp := c.get(ctx, "/v1/me", asSession(s))
		require.Equalf(t, http.StatusOK, resp.Status, "GET /v1/me: %s", resp.Body)

		var p apiPrincipal
		resp.decode(t, &p)
		// SubjectID is the users.id UUID, never the identity-provider
		// subject: CONVENTIONS "Identity of principals".
		assert.Equal(t, userID, p.SubjectID)
		assert.Equal(t, "USER", p.ActorType)
		assert.Equal(t, []string{"CUSTOMER"}, p.Roles)
		assert.Equal(t, []string{accountID}, p.AccountIDs)
		// ":mfa" asks the dev IdP for AMR [pwd, mfa]; without it the
		// step-up endpoints refuse.
		assert.Subset(t, p.AMR, []string{"pwd", "mfa"})
		assert.NotEmpty(t, p.AuthTime)
	})

	t.Run("accounts", func(t *testing.T) {
		resp := c.get(ctx, "/v1/accounts", asSession(s))
		require.Equalf(t, http.StatusOK, resp.Status, "GET /v1/accounts: %s", resp.Body)

		var list []apiAccount
		resp.decode(t, &list)
		require.Lenf(t, list, 1, "customer sees exactly their own account: %s", resp.Body)
		assert.Equal(t, accountID, list[0].ID)
		assert.Equal(t, "CUSTOMER", list[0].Kind)
		assert.Equal(t, "ACTIVE", list[0].Status)
	})

	t.Run("buying_power_shows_the_seeded_capital", func(t *testing.T) {
		resp := c.get(ctx, "/v1/accounts/"+accountID+"/buying-power", asSession(s))
		require.Equalf(t, http.StatusOK, resp.Status, "GET buying-power: %s", resp.Body)

		obj := resp.jsonObject(t)
		// Money is a string in this API, never a number. Asserting the raw
		// JSON member catches a representation change that a decoded
		// comparison would not.
		for _, member := range []string{
			"portfolio_value", "buying_power", "available_now",
			"reserved", "pending", "withdrawable",
		} {
			raw, ok := obj[member]
			require.Truef(t, ok, "buying power is missing %q: %s", member, resp.Body)
			require.Truef(t, len(raw) > 0 && raw[0] == '"',
				"%q must be a JSON string, not a number: %s", member, raw)
		}
		requireJSONString(t, obj, "buying_power", seedUSDCValueUSD)
		requireJSONString(t, obj, "available_now", seedUSDCValueUSD)
		requireJSONString(t, obj, "portfolio_value", seedUSDCValueUSD)
		requireJSONString(t, obj, "reserved", "0.00")

		var bp struct {
			UnderlyingBalances []struct {
				Asset    string `json:"asset"`
				Symbol   string `json:"symbol"`
				Decimals int    `json:"decimals"`
				Quantity string `json:"quantity"`
				USDValue string `json:"usd_value"`
				Status   string `json:"status"`
			} `json:"underlying_balances"`
			Restrictions []any `json:"restrictions"`
		}
		resp.decode(t, &bp)
		require.NotEmptyf(t, bp.UnderlyingBalances, "no underlying balances: %s", resp.Body)
		var found bool
		for _, b := range bp.UnderlyingBalances {
			if b.Asset != seeded.USDCAssetID {
				continue
			}
			found = true
			assert.Equal(t, "USDC", b.Symbol)
			assert.Equal(t, 6, b.Decimals)
			// Exact base units, never a rounded decimal.
			assert.Equal(t, seedUSDCBaseUnits, b.Quantity)
			assert.Equal(t, seedUSDCValueUSD, b.USDValue)
		}
		assert.Truef(t, found, "the seeded USDC asset %s is not among the underlying balances: %s",
			seeded.USDCAssetID, resp.Body)
		assert.Emptyf(t, bp.Restrictions, "an ACTIVE seeded account should carry no restrictions: %s", resp.Body)
	})

	t.Run("holdings", func(t *testing.T) {
		resp := c.get(ctx, "/v1/accounts/"+accountID+"/holdings", asSession(s))
		require.Equalf(t, http.StatusOK, resp.Status, "GET holdings: %s", resp.Body)

		var h apiHoldings
		resp.decode(t, &h)
		require.NotEmptyf(t, h.Holdings, "no holdings for the seeded account: %s", resp.Body)
		var found bool
		for _, row := range h.Holdings {
			if row.Asset != seeded.USDCAssetID {
				continue
			}
			found = true
			assert.Equal(t, "USDC", row.Symbol)
			assert.Equal(t, settlementChain, row.Chain)
			assert.Equal(t, settlementMint, row.MintAddress)
			assert.Equal(t, seedUSDCBaseUnits, row.Quantity)
			assert.Equal(t, seedUSDCValueUSD, row.USDMark)
		}
		assert.Truef(t, found, "the seeded USDC holding is absent: %s", resp.Body)
	})

	t.Run("ledger_shows_the_seed_posting", func(t *testing.T) {
		resp := c.get(ctx, "/v1/accounts/"+accountID+"/ledger/transactions", asSession(s))
		require.Equalf(t, http.StatusOK, resp.Status, "GET ledger/transactions: %s", resp.Body)

		var page apiJournalPage
		resp.decode(t, &page)
		require.NotEmptyf(t, page.Items, "the journal is empty for a seeded account: %s", resp.Body)

		var seedTx *int
		for i := range page.Items {
			if page.Items[i].Kind == "SEED" && page.Items[i].Reference.Type == "seed" {
				idx := i
				seedTx = &idx
				break
			}
		}
		require.NotNilf(t, seedTx, "no SEED transaction in the journal: %s", resp.Body)

		tx := page.Items[*seedTx]
		assert.NotEmpty(t, tx.ContentHash, "a posted transaction always carries its content hash")
		require.Lenf(t, tx.Entries, 2, "the seed posting is a two-entry double entry: %+v", tx.Entries)

		byCode := map[string]struct {
			Side, Quantity, Asset string
		}{}
		for _, e := range tx.Entries {
			byCode[e.AccountCode] = struct{ Side, Quantity, Asset string }{e.Side, e.Quantity, e.Asset}
		}
		wallet, ok := byCode["WALLET"]
		require.Truef(t, ok, "no WALLET entry in the seed posting: %+v", tx.Entries)
		assert.Equal(t, "DEBIT", wallet.Side)
		assert.Equal(t, seedUSDCBaseUnits, wallet.Quantity)
		assert.Equal(t, seeded.USDCAssetID, wallet.Asset)

		capital, ok := byCode["CAPITAL"]
		require.Truef(t, ok, "no CAPITAL entry in the seed posting: %+v", tx.Entries)
		assert.Equal(t, "CREDIT", capital.Side)
		assert.Equal(t, seedUSDCBaseUnits, capital.Quantity)
	})

	t.Run("instruments", func(t *testing.T) {
		resp := c.get(ctx, "/v1/instruments", asSession(s))
		require.Equalf(t, http.StatusOK, resp.Status, "GET /v1/instruments: %s", resp.Body)

		var list []apiInstrument
		resp.decode(t, &list)
		var found bool
		for _, i := range list {
			if i.ID == seeded.InstrumentID {
				found = true
				assert.Equal(t, "SOL/USDC", i.CanonicalName)
				assert.Equal(t, "ACTIVE", i.Status)
				assert.Equal(t, "SPOT_PAIR", i.Type)
			}
		}
		assert.Truef(t, found, "the seeded SOL/USDC instrument %s is absent: %s",
			seeded.InstrumentID, resp.Body)
	})

	t.Run("orders_and_intents_are_account_scoped", func(t *testing.T) {
		// Both collections require account_id; without it the generated
		// server refuses before any handler runs.
		for _, path := range []string{"/v1/orders", "/v1/intents"} {
			missing := c.get(ctx, path, asSession(s))
			require.Equalf(t, http.StatusBadRequest, missing.Status,
				"GET %s without account_id: %s", path, missing.Body)
			assert.Equal(t, errs.CodeValidationFailed, missing.problem(t).Code)

			resp := c.get(ctx, path+"?account_id="+accountID, asSession(s))
			require.Equalf(t, http.StatusOK, resp.Status, "GET %s: %s", path, resp.Body)
			var page struct {
				Items      []map[string]any `json:"items"`
				NextCursor *string          `json:"next_cursor"`
			}
			resp.decode(t, &page)
			assert.NotNil(t, page.Items, "a page always carries an items array: %s", resp.Body)
		}
	})

	t.Run("logout_revokes_the_cookie", func(t *testing.T) {
		// Prove the cookie worked immediately before the logout, so the
		// 401 afterwards can only be the revocation.
		before := c.get(ctx, "/v1/me", asSession(s))
		require.Equalf(t, http.StatusOK, before.Status, "the session must work before logout: %s", before.Body)

		out := c.postJSON(ctx, "/v1/auth/logout", nil, asSession(s))
		require.Equalf(t, http.StatusNoContent, out.Status, "POST /v1/auth/logout: %s", out.Body)

		probe := s
		if e2eBreak(t, "logout_not_revoked") {
			probe = c.signIn(ctx, identity+":mfa")
		}

		after := c.get(ctx, "/v1/me", asSession(probe))
		require.Equalf(t, http.StatusUnauthorized, after.Status,
			"the revoked cookie must be refused; a session that survives logout is the defect this catches. body: %s",
			after.Body)
		p := after.problem(t)
		assert.Equal(t, errs.CodeUnauthenticated, p.Code)
		assert.NotEmpty(t, p.RequestID, "a refusal must be traceable")

		// The revocation is server-side state, not a cookie-clearing hint:
		// assert it in the database rather than trusting the API.
		var revokedAt *string
		require.NoError(t, testDB.Pool().QueryRow(ctx,
			`SELECT revoked_at::text FROM sessions WHERE id = $1`, s.sessionID).Scan(&revokedAt))
		require.NotNilf(t, revokedAt, "sessions.revoked_at is still NULL for %s after logout", s.sessionID)
	})
}
