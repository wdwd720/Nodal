//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/alert"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// F-118, whole: a number changes in the database behind the ledger's back, and
// a human's webhook receives a SEV1 about it -- through the pass this process
// runs on its own timer, the metrics seam, the dispatcher and the wire.
//
// Each of those has its own test. This is the one that would fail if any
// two of them were joined wrongly, which is the failure the finding actually
// described: every part existed, and nothing was told anything.
func TestIntegration_DriftInTheDatabaseReachesTheWebhook(t *testing.T) {
	appURL := os.Getenv("CP_TEST_DATABASE_URL")
	migrateURL := os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	if appURL == "" || migrateURL == "" {
		t.Skip("CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL not set")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := openPool(ctx, t, appURL)

	// The destination.
	var (
		mu  sync.Mutex
		got []alert.Event
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e alert.Event
		require.NoError(t, json.NewDecoder(r.Body).Decode(&e))
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	dispatcher := alert.NewDispatcher(alert.Options{
		Sink:   alert.NewWebhookSink(srv.URL, "test", alert.FormatGeneric, srv.Client()),
		Logger: log,
	})

	// The engine, built the way wire.go builds it: resolution-shaped, real
	// instruments, the dispatcher attached at the seam.
	clk := clock.System()
	writer := audit.NewWriterWithBuildVersion(config.BuildVersion)
	metrics := financialMetrics(log)
	attachAlertDispatcher(metrics, dispatcher, &config.Config{Env: config.EnvStaging, ServiceName: "nodal-api"})
	engine, err := reconciliation.NewEngine(reconciliation.Config{
		DB:        app,
		Clock:     clk,
		Records:   reconciliation.NewRepository(clk, event.NewOutbox(clk), writer),
		Policy:    reconciliation.DefaultPolicy(),
		Approvals: admin.NewService(clk, writer),
		Metrics:   metrics,
		Logger:    log,
	})
	require.NoError(t, err)

	// A customer with a wallet account nothing has posted to.
	suffix := id.New[id.Any]().String()
	suffix = suffix[len(suffix)-12:]
	arepo := accounts.NewRepository()
	u, err := arepo.CreateUser(ctx, app, "itest", "drift-"+suffix, nil)
	require.NoError(t, err)
	acct, err := arepo.CreateAccount(ctx, app, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	asset, err := assets.NewRepository().Create(ctx, app, assets.Asset{
		Chain: "solana-devnet", MintAddress: "drift-" + suffix, Kind: assets.KindSPLToken,
		ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "USDC", Name: "USD Coin", Decimals: 6,
		IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	wallet, err := ledger.NewService(clk, config.BuildVersion).
		EnsureAccount(ctx, app, ledger.CustomerAccount(acct.ID, ledger.CodeWallet, asset.ID))
	require.NoError(t, err)

	// Nothing is wrong yet, and nothing must be said.
	verifyOnce(ctx, engine, log)
	mu.Lock()
	require.Empty(t, got, "a clean database raised an alert; the destination will be muted within a week")
	mu.Unlock()

	// A balance appears that no journal entry produced. Only the schema owner
	// can do this -- cp_app has SELECT on ledger_balances and nothing else --
	// which is the point: this is a restored backup, a privileged session or
	// a bug, never the application.
	md := openPool(ctx, t, migrateURL)
	_, err = md.Exec(ctx, `INSERT INTO ledger_balances (ledger_account_id, balance, entry_count, version)
		VALUES ($1, 1, 1, 1)`, wallet.ID)
	require.NoError(t, err)
	t.Cleanup(func() {
		// So a later VerifyInternal on this database is clean again. The
		// reconciliation record stays: it is history.
		_, _ = md.Exec(context.Background(), `DELETE FROM ledger_balances WHERE ledger_account_id = $1`, wallet.ID)
	})

	// The pass the API runs on its timer.
	verifyOnce(ctx, engine, log)
	dispatcher.Close()

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, got, "a ledger balance changed outside a posting and nothing left the process: F-118 is back")
	var found *alert.Event
	for i := range got {
		if got[i].Name == reconciliation.AlertLedgerIntegrity {
			found = &got[i]
			break
		}
	}
	require.NotNil(t, found, "something was delivered, but not the ledger-integrity violation: %+v", got)
	assert.Equal(t, alert.SEV1, found.Severity)
	assert.NotEmpty(t, found.RecordID, "the page must name the record an operator would open")
	assert.Equal(t, "STAGING", found.Environment)
	assert.Equal(t, "nodal-api", found.Service)
	assert.Zero(t, found.DroppedFields, "the raise site attached a field the allowlist does not know; either allow it or stop sending it")

	// The record exists for the operator the page points at.
	var status string
	require.NoError(t, app.QueryRow(ctx,
		`SELECT status FROM reconciliation_records WHERE id = $1`, found.RecordID).Scan(&status))
	assert.Equal(t, string(reconciliation.StatusMismatch), status)
}
