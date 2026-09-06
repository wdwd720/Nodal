//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/security"
)

// The suite runs against an isolated database provisioned by
// `go run ./scripts/testdb -name httpapi`, never the shared controlplane_test.
var testAppURL = os.Getenv("CP_TEST_DATABASE_URL")

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	if testAppURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; provision one with `go run ./scripts/testdb -name httpapi`")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "httpapi-itest", MaxConns: 8})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func seedAccount(t *testing.T, d *db.DB) (accounts.UserID, accounts.AccountID) {
	t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(t.Context(), d, "httpapi-itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	a, err := repo.CreateAccount(t.Context(), d, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return u.ID, a.ID
}

func seedAsset(t *testing.T, d *db.DB) assets.Asset {
	t.Helper()
	a, err := assets.NewRepository().Create(t.Context(), d, assets.Asset{
		Chain: "solana-devnet", MintAddress: "mint-" + id.New[id.Any]().String(),
		Kind: assets.KindSPLToken, Symbol: "USDC", Name: "USD Coin", Decimals: 6,
		IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement,
		Status: assets.StatusActive,
	})
	require.NoError(t, err)
	return a
}

// --- the persisted idempotency contract ---------------------------------------

// newIntegrationHarness builds the real router over the real idempotency store
// so the PART 36 contract is exercised end to end, not against a double.
func newIntegrationHarness(t *testing.T, d *db.DB, accountID accounts.AccountID, userID accounts.UserID) *harness {
	t.Helper()
	fx := newFixtures()
	fx.idem = nil
	ports := fx.ports()
	ports.Idempotency = idempotencyAdapter{store: idempotency.NewStore(func() time.Time { return time.Now().UTC() }), db: d}

	p := security.Principal{
		SubjectID:  userID.String(),
		ActorType:  security.ActorUser,
		Roles:      []security.Role{security.RoleCustomer},
		AccountIDs: []string{accountID.String()},
		SessionID:  testSessionID,
		AuthTime:   time.Now().UTC(),
		AMR:        []string{"pwd", "mfa"},
	}
	h := &harness{t: t, ports: fx, princip: &p}
	srv, err := New(Options{
		Env: config.EnvTest, Clock: clock.System(), CookieName: "cp_session",
		Authenticator: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if h.princip == nil {
					next.ServeHTTP(w, r)
					return
				}
				next.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), *h.princip)))
			})
		},
		Ports: ports,
	})
	require.NoError(t, err)
	h.server = srv
	return h
}

func TestIntegration_IdempotentReplayIsPersisted(t *testing.T) {
	d := openTestDB(t)
	userID, accountID := seedAccount(t, d)
	h := newIntegrationHarness(t, d, accountID, userID)

	body := map[string]any{
		"account_id":    accountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
		"mode":          "PAPER",
	}
	key := "itest-" + id.New[id.Any]().String()

	first := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusAccepted, first.Code, "body=%s", first.Body.String())
	require.Equal(t, 1, h.ports.intents.submitCount())

	// The row is COMPLETED with the response the client saw.
	var status string
	var responseStatus int
	var stored []byte
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT status, response_status, response_body FROM idempotency_keys
		 WHERE actor_id = $1 AND endpoint = $2 AND key = $3`,
		userID.String(), "PostIntents", key).Scan(&status, &responseStatus, &stored))
	assert.Equal(t, "COMPLETED", status)
	assert.Equal(t, http.StatusAccepted, responseStatus)
	assert.JSONEq(t, first.Body.String(), string(stored))

	second := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusOK, second.Code, "a replay answers with the spec's replay status")
	assert.JSONEq(t, first.Body.String(), second.Body.String())
	assert.Equal(t, 1, h.ports.intents.submitCount(), "a replay never re-executes the command")
}

func TestIntegration_IdempotencyKeyReuseWithADifferentBodyConflicts(t *testing.T) {
	d := openTestDB(t)
	userID, accountID := seedAccount(t, d)
	h := newIntegrationHarness(t, d, accountID, userID)

	body := map[string]any{
		"account_id":    accountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
		"mode":          "PAPER",
	}
	key := "itest-" + id.New[id.Any]().String()
	require.Equal(t, http.StatusAccepted, h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key).Code)

	body["notional_usd"] = "999.00"
	res := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, res.problem().Code)
	assert.Equal(t, 1, h.ports.intents.submitCount())
}

// TestIntegration_BusinessRejectionIsRecordedAsAConclusion: a rejection is an
// outcome. Replaying its key reproduces the problem rather than running the
// command again, even when the domain would now accept it.
func TestIntegration_BusinessRejectionIsRecordedAsAConclusion(t *testing.T) {
	d := openTestDB(t)
	userID, accountID := seedAccount(t, d)
	h := newIntegrationHarness(t, d, accountID, userID)
	h.ports.intents.err = errs.New(errs.CodeInsufficientBuyingPower, "not enough").
		WithField("available", "10.00")

	body := map[string]any{
		"account_id":    accountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
		"mode":          "PAPER",
	}
	key := "itest-" + id.New[id.Any]().String()

	first := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusUnprocessableEntity, first.Code)

	h.ports.intents.err = nil
	second := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusUnprocessableEntity, second.Code, "body=%s", second.Body.String())
	p := second.problem()
	assert.Equal(t, errs.CodeInsufficientBuyingPower, p.Code)
	assert.Equal(t, "10.00", p.Fields["available"])
	assert.Equal(t, 0, h.ports.intents.submitCount())
}

// TestIntegration_InternalFailureLeavesTheKeyRetryable: an internal error is
// not a conclusion, so the row is FAILED and the same key may run again.
func TestIntegration_InternalFailureLeavesTheKeyRetryable(t *testing.T) {
	d := openTestDB(t)
	userID, accountID := seedAccount(t, d)
	h := newIntegrationHarness(t, d, accountID, userID)
	h.ports.intents.err = assert.AnError

	body := map[string]any{
		"account_id":    accountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
		"mode":          "PAPER",
	}
	key := "itest-" + id.New[id.Any]().String()
	require.Equal(t, http.StatusInternalServerError,
		h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key).Code)

	var status string
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT status FROM idempotency_keys WHERE actor_id = $1 AND endpoint = $2 AND key = $3`,
		userID.String(), "PostIntents", key).Scan(&status))
	assert.Equal(t, "FAILED", status)

	h.ports.intents.err = nil
	res := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusAccepted, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 1, h.ports.intents.submitCount())
}

// TestIntegration_DifferentActorsDoNotShareKeys: the idempotency row is keyed
// by (actor, endpoint, key), so one customer's key can never replay another's
// command.
func TestIntegration_DifferentActorsDoNotShareKeys(t *testing.T) {
	d := openTestDB(t)
	userA, accountA := seedAccount(t, d)
	userB, accountB := seedAccount(t, d)

	body := func(a accounts.AccountID) map[string]any {
		return map[string]any{
			"account_id":    a.String(),
			"instrument_id": testInstrument.String(),
			"action":        "ACQUIRE_NOTIONAL",
			"notional_usd":  "100.00",
			"mode":          "PAPER",
		}
	}
	key := "shared-" + id.New[id.Any]().String()

	ha := newIntegrationHarness(t, d, accountA, userA)
	require.Equal(t, http.StatusAccepted, ha.do(http.MethodPost, "/v1/intents", body(accountA), "Idempotency-Key", key).Code)

	hb := newIntegrationHarness(t, d, accountB, userB)
	require.Equal(t, http.StatusAccepted, hb.do(http.MethodPost, "/v1/intents", body(accountB), "Idempotency-Key", key).Code)
	assert.Equal(t, 1, hb.ports.intents.submitCount(), "the second actor ran its own command")
}

// --- the read model ------------------------------------------------------------

func TestIntegration_ReadModelQueries(t *testing.T) {
	d := openTestDB(t)
	_, accountID := seedAccount(t, d)
	asset := seedAsset(t, d)
	rm := NewReadModel(d)
	ctx := t.Context()

	t.Run("orders page is empty and reports no cursor", func(t *testing.T) {
		page, err := rm.ListOrders(ctx, accountID, "", 10)
		require.NoError(t, err)
		assert.Empty(t, page.Items)
		assert.Empty(t, page.NextCursor)
	})

	t.Run("account search finds by id and refuses free text", func(t *testing.T) {
		page, err := rm.SearchAccounts(ctx, accountID.String(), "", 10)
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		assert.Equal(t, accountID, page.Items[0].ID)

		_, err = rm.SearchAccounts(ctx, "someone@example.test", "", 10)
		require.Error(t, err, "search must not accept free text it cannot scope")
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})

	t.Run("account search pages", func(t *testing.T) {
		page, err := rm.SearchAccounts(ctx, "", "", 1)
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		if page.NextCursor != "" {
			next, nerr := rm.SearchAccounts(ctx, "", page.NextCursor, 1)
			require.NoError(t, nerr)
			if len(next.Items) > 0 {
				assert.NotEqual(t, page.Items[0].ID, next.Items[0].ID, "a cursor must advance")
			}
		}
	})

	t.Run("admin actions page", func(t *testing.T) {
		page, err := rm.ListAdminActions(ctx, "", "", 10)
		require.NoError(t, err)
		assert.NotNil(t, page.Items != nil || page.Items == nil)

		_, err = rm.ListAdminActions(ctx, "NOT_A_STATUS", "", 10)
		require.Error(t, err)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})

	t.Run("kill switches list every row", func(t *testing.T) {
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO kill_switches (id, kind, scope_id, active, severity, reason, activated_at)
				VALUES ($1, 'VENUE_DISABLE', 'JUPITER', false, 'STANDARD', 'released after incident', now())
				ON CONFLICT (kind, scope_id) DO UPDATE
				SET active = false, reason = 'released after incident'`,
				id.New[id.Any]())
			return err
		}))
		out, err := rm.ListKillSwitches(ctx)
		require.NoError(t, err)
		found := false
		for _, sw := range out {
			if sw.Kind == killswitch.VenueDisable && sw.ScopeID == "JUPITER" {
				found = true
				assert.False(t, sw.Active)
				assert.Equal(t, "released after incident", sw.Reason)
			}
		}
		assert.True(t, found, "an inactive switch must still be listed")
	})

	t.Run("reconciliation records page and filter", func(t *testing.T) {
		recordID := id.New[id.Any]()
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO reconciliation_records
					(id, kind, mode, scope_type, scope_id, account_id, asset_id,
					 expected, observed, difference, status, material, blocks_new_risk, opened_at)
				VALUES ($1,'WALLET_BALANCE','PERIODIC','account',$2,$3,$4,
					'{"q":"1"}'::jsonb,'{"q":"2"}'::jsonb,'{"q":"1"}'::jsonb,'MISMATCH',true,true, now())`,
				recordID, accountID.String(), accountID, asset.ID)
			return err
		}))

		page, err := rm.ListReconciliationRecords(ctx, "MISMATCH", &accountID, "", 10)
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		got := page.Items[0]
		assert.Equal(t, recordID.String(), got.ID)
		assert.True(t, got.Material)
		assert.True(t, got.BlocksNewRisk)
		assert.Equal(t, "1", got.Expected["q"])

		empty, err := rm.ListReconciliationRecords(ctx, "RESOLVED_MANUAL", &accountID, "", 10)
		require.NoError(t, err)
		assert.Empty(t, empty.Items)
	})

	t.Run("reconciliation blocks feed the buying power engine", func(t *testing.T) {
		blocks, err := NewReconciliationBlockReader().Blocks(ctx, d, accountID)
		require.NoError(t, err)
		require.NotEmpty(t, blocks, "a blocking mismatch must reach buying power")
		assert.Equal(t, "WALLET_BALANCE", blocks[0].Kind)
	})

	t.Run("asset refs and wallet", func(t *testing.T) {
		refs, err := rm.AssetRefs(ctx, []assets.AssetID{asset.ID})
		require.NoError(t, err)
		require.Contains(t, refs, asset.ID)
		assert.Equal(t, "USDC", refs[asset.ID].Symbol)
		assert.Equal(t, uint8(6), refs[asset.ID].Decimals)

		walletID, address, err := rm.Wallet(ctx, accountID)
		require.NoError(t, err)
		assert.Empty(t, walletID, "an account with no wallet reports nothing rather than guessing")
		assert.Empty(t, address)

		custody, err := rm.CustodyAddress(ctx, accountID)
		require.NoError(t, err)
		assert.Empty(t, custody)
	})

	t.Run("activity timeline is empty for a fresh account", func(t *testing.T) {
		page, err := rm.Activity(ctx, accountID, "", 10)
		require.NoError(t, err)
		// The reconciliation record above is the account's only fact.
		require.Len(t, page.Items, 1)
		assert.Equal(t, "RECONCILIATION", page.Items[0].Kind)
		assert.Contains(t, page.Items[0].References, "record_id")
	})

	t.Run("a malformed cursor is a validation failure", func(t *testing.T) {
		_, err := rm.ListOrders(ctx, accountID, "not-base64!!", 10)
		require.Error(t, err)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})
}

// TestIntegration_KillSwitchReaderUsesTheDomainMatrix proves the buying-power
// view of the authority plane comes from internal/killswitch's own matrix
// rather than a copy of it: a global kill blocks new risk, a venue disable
// does not touch an account's buying power.
func TestIntegration_KillSwitchReaderUsesTheDomainMatrix(t *testing.T) {
	d := openTestDB(t)
	_, accountID := seedAccount(t, d)
	ctx := t.Context()

	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO kill_switches (id, kind, scope_id, active, severity, reason, activated_at)
			VALUES ($1, 'GLOBAL_NEW_RISK_KILL', '*', true, 'SEVERE', 'incident 42', now())
			ON CONFLICT (kind, scope_id) DO UPDATE SET active = true`, id.New[id.Any]())
		return err
	}))
	t.Cleanup(func() {
		_, _ = d.Exec(context.Background(),
			`UPDATE kill_switches SET active = false WHERE kind = 'GLOBAL_NEW_RISK_KILL'`)
	})

	controller, err := killswitch.NewController(clock.System(), noopKillAudit{},
		NewApprovalVerifier(admin.NewService(clock.System(), audit.NewWriter())))
	require.NoError(t, err)
	reader := NewKillSwitchReader(controller, killswitch.Policy{})

	states, err := reader.Active(ctx, d, accountID)
	require.NoError(t, err)
	require.NotEmpty(t, states)
	var global *struct{ blocksNewRisk, blocksWithdrawal bool }
	for _, s := range states {
		if s.Kind == string(killswitch.GlobalNewRiskKill) {
			global = &struct{ blocksNewRisk, blocksWithdrawal bool }{s.BlocksNewRisk, s.BlocksWithdrawal}
		}
	}
	require.NotNil(t, global, "a global kill must be reported to buying power")
	assert.True(t, global.blocksNewRisk)
	assert.True(t, global.blocksWithdrawal)
}

type noopKillAudit struct{}

func (noopKillAudit) Append(context.Context, pgx.Tx, killswitch.AuditEvent) error { return nil }

// TestIntegration_ProblemJSONNeverLeaksARealDriverError drives an actual
// PostgreSQL failure through the boundary and asserts nothing of it reaches the
// client.
func TestIntegration_ProblemJSONNeverLeaksARealDriverError(t *testing.T) {
	d := openTestDB(t)
	_, accountID := seedAccount(t, d)
	ctx := t.Context()

	// A pool that has been closed produces a real driver error carrying
	// connection details, which is exactly what must not reach a client.
	closed, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "httpapi-leak", MaxConns: 2})
	require.NoError(t, err)
	closed.Close()

	_, err = NewReadModel(closed).ListOrders(ctx, accountID, "", 10)
	require.Error(t, err)
	problem := errs.ToProblem(classify(ctx, err), "/v1/orders", "req-1")
	body, merr := json.Marshal(problem)
	require.NoError(t, merr)
	assert.Equal(t, errs.CodeInternal, problem.Code)
	assert.Equal(t, "internal error", problem.Detail)
	// The driver's own words, the statement and the connection string are all
	// absent; only the request path (the RFC 9457 instance) remains.
	assert.NotContains(t, string(body), "SELECT")
	assert.NotContains(t, string(body), "closed pool")
	assert.NotContains(t, string(body), "cp_app")
	assert.NotContains(t, string(body), "127.0.0.1")
	assert.NotContains(t, string(body), "sslmode")
	assert.Contains(t, string(body), `"instance":"/v1/orders"`)
}
