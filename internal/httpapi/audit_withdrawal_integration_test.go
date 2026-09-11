//go:build integration

package httpapi

// Reproduction for the withdrawal-verification audit (goal §54, F-wv-5).
//
// ADR-0025 §3: "The hosted URL is not stored. Those links are single-use,
// expire in minutes, and are a credential for resuming somebody else's identity
// check; one is handed to the browser that asked for it and written down
// nowhere." docs/product/VERIFICATION_AND_WITHDRAWAL.md §5 lists it under
// "Never stored".
//
// POST /v1/me/verification/sessions is a Mutating operation, so runCommand
// marshals its whole response — hosted_url included — into
// idempotency_keys.response_body, where it stays for IdempotencyTTL (24 hours
// by default) and is readable by cp_app, cp_readonly and cp_ops.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/security"
)

func TestAuditWV_TheHostedVerificationURLIsWrittenDownNowhere(t *testing.T) {
	appURL := testAppDSN(t)
	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{URL: appURL, AppName: "auditwv-itest", MaxConns: 4})
	require.NoError(t, err)
	defer pool.Close()

	fx := newFixtures()
	principal := customerPrincipal()
	// The real persisted idempotency contract, not the in-memory fake the rest
	// of this package's harness uses: the question is what reaches the table.
	ports := fx.ports()
	ports.Idempotency = idempotencyAdapter{store: idempotency.NewStore(nil), db: pool}

	srv, err := New(Options{
		Env:           config.EnvTest,
		BuildVersion:  "audit-build",
		ConfigHash:    "hash-audit",
		PublicBaseURL: "https://app.test",
		CORSOrigins:   []string{"https://app.test"},
		CookieName:    "cp_session",
		SessionTTL:    time.Hour,
		Clock:         clock.NewFake(testNow),
		Authenticator: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), principal)))
			})
		},
		Ports: ports,
	})
	require.NoError(t, err)

	key := "audit-wv-hosted-" + testSessionID
	h := &harness{t: t, ports: fx, princip: &principal, server: srv}
	res := h.do(http.MethodPost, "/v1/me/verification/sessions", map[string]any{
		"account_id":           testAccountID.String(),
		"jurisdiction_country": "US",
		"jurisdiction_region":  "CA",
		"purpose":              "PAYOUT_KYC",
	}, "Idempotency-Key", key, "Origin", "https://app.test")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())

	var started api.StartedVerification
	res.json(&started)
	require.NotNil(t, started.HostedUrl)
	hosted := *started.HostedUrl
	require.NotEmpty(t, hosted)

	var stored string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT coalesce(response_body::text, '') FROM idempotency_keys
		  WHERE endpoint = 'PostMeVerificationSessions' AND key = $1`, key).Scan(&stored))

	t.Logf("F-wv-5: idempotency_keys.response_body = %s", stored)
	assert.False(t, strings.Contains(stored, hosted),
		"F-wv-5: the single-use hosted verification link was written to idempotency_keys.response_body, "+
			"which cp_readonly and cp_ops may SELECT and which cp_app may not DELETE; ADR-0025 §3 says it "+
			"is written down nowhere")
}
