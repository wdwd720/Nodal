//go:build integration && e2e

package e2e

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/httpapi"
)

// zeroUUID is a syntactically valid UUIDv7 that nothing is keyed on, so it is
// a genuine "unknown id" rather than a malformed one.
const zeroUUID = "01a07687-0000-7000-8000-000000000000"

// forbiddenInProblems is the explicit leakage list. Every entry is something a
// caller must never be able to read out of a refusal: the database DSN and its
// password, the driver, raw SQL, a stack frame, a source path, or the name of
// a table. Entries that are also legitimate URL path segments (accounts,
// sessions, deposits, …) are deliberately absent — a list that produced false
// positives would be turned off, and a leakage check that is turned off is
// worse than none.
var forbiddenInProblems = []string{
	// Connection strings and credentials.
	"postgres://", "postgresql://", "cp_app_local", "cp_migrate_local",
	"cp_admin_local", "127.0.0.1:5433", "sslmode=",
	// Driver and SQL internals.
	"pgx", "pgconn", "pgxpool", "sql:", "SQLSTATE", "SELECT ", "INSERT INTO",
	"DELETE FROM", "violates unique constraint", "duplicate key value",
	// Runtime internals.
	"goroutine", "panic:", ".go:", "runtime error", "github.com/nodal/controlplane",
	// Table names. None of these can appear in a /v1 path, so a hit is a leak.
	"trade_intents", "journal_transactions", "journal_entries", "idempotency_keys",
	"ledger_accounts", "ledger_balances", "operator_roles", "outbox_events",
	"inbox_messages", "audit_events", "asset_reservations", "capability_gates",
	"kill_switches", "execution_plans", "risk_decisions", "eligibility_decisions",
	"position_lots", "provider_events", "security_events", "login_attempts",
	"account_status_transitions", "deposit_transitions", "intent_transitions",
}

// TestE2E_ErrorContract walks a spread of refusals and holds every one of them
// to the same contract: application/problem+json, a stable machine-readable
// code, a request id an operator can quote back, a status in the body that
// matches the status on the wire, and no internal detail of any kind.
func TestE2E_ErrorContract(t *testing.T) {
	requireEnv(t)
	srv := startAPI(t)
	srv.dumpLogs(t)
	c := newClient(t, srv)
	ctx := t.Context()

	customerA := c.signIn(ctx, "customer-a:mfa")
	customerB := c.signIn(ctx, "customer-b:mfa")
	admin := c.signIn(ctx, "admin:mfa")

	// The cross-tenant caller is customer-b reaching for customer-a's
	// account. Under the negative control it is customer-a reaching for her
	// own, which is answered 200 — the shape of a broken tenant check.
	crossTenant := customerB
	if e2eBreak(t, "cross_tenant_allowed") {
		crossTenant = customerA
	}

	oversized := oversizedJSONBody(httpapi.DefaultMaxBodyBytes)

	cases := []struct {
		name       string
		exchange   func(context.Context) response
		wantStatus int
		wantCode   errs.Code
		// requestIDPinnedEmpty records a KNOWN PRODUCTION DEFECT rather than
		// hiding it: see the comment on the csrf case.
		requestIDPinnedEmpty bool
	}{
		{
			name:       "anonymous_is_401",
			exchange:   func(ctx context.Context) response { return c.get(ctx, "/v1/me") },
			wantStatus: http.StatusUnauthorized,
			wantCode:   errs.CodeUnauthenticated,
		},
		{
			name: "unknown_session_cookie_is_401",
			exchange: func(ctx context.Context) response {
				// A cookie that is not a session must be treated as no
				// session at all, never as a hint about what a real one
				// looks like.
				return c.get(ctx, "/v1/me", asRawCookie("cp_session", "not-a-session-"+runToken))
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   errs.CodeUnauthenticated,
		},
		{
			name: "cross_tenant_read_is_403",
			exchange: func(ctx context.Context) response {
				return c.get(ctx, "/v1/accounts/"+seeded.CustomerAAccountID+"/buying-power",
					asSession(crossTenant))
			},
			wantStatus: http.StatusForbidden,
			wantCode:   errs.CodeForbidden,
		},
		{
			name: "cross_tenant_ledger_is_403",
			exchange: func(ctx context.Context) response {
				return c.get(ctx, "/v1/accounts/"+seeded.CustomerAAccountID+"/ledger/transactions",
					asSession(crossTenant))
			},
			wantStatus: http.StatusForbidden,
			wantCode:   errs.CodeForbidden,
		},
		{
			name: "unknown_intent_is_404",
			exchange: func(ctx context.Context) response {
				return c.get(ctx, "/v1/intents/"+zeroUUID, asSession(customerA))
			},
			wantStatus: http.StatusNotFound,
			wantCode:   errs.CodeNotFound,
		},
		{
			name: "unknown_route_is_404",
			exchange: func(ctx context.Context) response {
				return c.get(ctx, "/v1/no/such/resource", asSession(customerA))
			},
			wantStatus: http.StatusNotFound,
			wantCode:   errs.CodeNotFound,
		},
		{
			name: "unwired_capability_is_422",
			exchange: func(ctx context.Context) response {
				// The reconciliation engine is owned by its own binary; the
				// API exposes no resolution path, and says so rather than
				// pretending.
				return c.postJSON(ctx,
					"/v1/admin/reconciliation/records/"+zeroUUID+"/resolve",
					map[string]string{"resolution": "CORRECTED", "reason": "e2e error contract probe"},
					asSession(admin), idempotency(idemKey("recon")))
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   errs.CodeUnsupported,
		},
		{
			name: "malformed_json_is_400",
			exchange: func(ctx context.Context) response {
				return c.postRaw(ctx, "/v1/intents", []byte(`{"account_id": `),
					asSession(customerA), idempotency(idemKey("malformed")))
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   errs.CodeValidationFailed,
		},
		{
			name: "wrong_method_is_405",
			exchange: func(ctx context.Context) response {
				return c.do(ctx, http.MethodDelete, "/v1/accounts", nil, asSession(customerA))
			},
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   errs.CodeValidationFailed,
		},
		{
			name: "oversized_body_is_400",
			exchange: func(ctx context.Context) response {
				return c.postRaw(ctx, "/v1/intents", oversized,
					asSession(customerA), idempotency(idemKey("oversize")))
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   errs.CodeValidationFailed,
		},
		{
			name: "missing_idempotency_key_is_400",
			exchange: func(ctx context.Context) response {
				return c.postJSON(ctx, "/v1/intents", intentCommand{
					AccountID: seeded.CustomerAAccountID, InstrumentID: seeded.InstrumentID,
					Action: "ACQUIRE_NOTIONAL", NotionalUSD: "1.00", Mode: "PAPER",
				}, asSession(customerA))
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   errs.CodeValidationFailed,
		},
		{
			name: "cross_site_post_is_403",
			exchange: func(ctx context.Context) response {
				return c.postJSON(ctx, "/v1/intents", intentCommand{
					AccountID: seeded.CustomerAAccountID, InstrumentID: seeded.InstrumentID,
					Action: "ACQUIRE_NOTIONAL", NotionalUSD: "1.00", Mode: "PAPER",
				}, asSession(customerA), idempotency(idemKey("csrf")), noCSRFHeader())
			},
			wantStatus: http.StatusForbidden,
			wantCode:   errs.CodeForbidden,
			// DEFECT (reported, not fixed): internal/auth/httpmw/problem.go
			// builds this document with RequestID = r.Header.Get("X-Request-Id"),
			// i.e. the id the CLIENT sent, instead of the id the server
			// generated and returned in the X-Request-Id response header
			// (observability.RequestID(r.Context())). A normal client sends
			// no such header, so this refusal — the one a browser hits when
			// something is wrong with its origin — is the only refusal in the
			// API that cannot be correlated with a server log line.
			//
			// This flag PINS the observed behavior rather than excusing it:
			// when the defect is fixed this case fails, and the fix is to
			// delete the flag, not to widen it.
			requestIDPinnedEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.exchange(ctx)
			require.Equalf(t, tc.wantStatus, resp.Status,
				"unexpected status; body: %s", resp.Body)

			p := resp.problem(t)
			assert.Equalf(t, tc.wantCode, p.Code, "the machine-readable code is the contract; body: %s", resp.Body)
			assert.Equalf(t, tc.wantStatus, p.Status,
				"the status inside the document must match the status on the wire; body: %s", resp.Body)
			assert.NotEmptyf(t, p.Title, "a problem always carries a title; body: %s", resp.Body)
			assert.NotEmptyf(t, p.Type, "a problem always carries a type; body: %s", resp.Body)

			if tc.requestIDPinnedEmpty {
				assert.Emptyf(t, p.RequestID,
					"this pins a KNOWN DEFECT (httpmw problem documents carry no server request id). "+
						"If this now fails, the defect was fixed: delete requestIDPinnedEmpty from this case. body: %s",
					resp.Body)
			} else {
				assert.NotEmptyf(t, p.RequestID,
					"a refusal an operator cannot correlate with a log line is not an answer; body: %s", resp.Body)
			}
			// Whatever the body says, the transport must always carry the id.
			assert.NotEmpty(t, resp.Header.Get("X-Request-Id"),
				"every response carries the server-generated X-Request-Id header")

			requireNoLeakedInternals(t, resp.Body)
		})
	}
}

// requireNoLeakedInternals scans the RAW response bytes — not a decoded
// struct, which would hide anything the decoder dropped — for every forbidden
// substring.
func requireNoLeakedInternals(t *testing.T, body []byte) {
	t.Helper()

	scanned := body
	if e2eBreak(t, "error_body_leaks") {
		// Splice a DSN into the bytes the scan reads. If the scan does not
		// fire on this, it is not looking at the response at all.
		scanned = append(append([]byte{}, body...),
			[]byte(` postgres://cp_app:cp_app_local@127.0.0.1:5433/controlplane`)...)
	}

	lowered := strings.ToLower(string(scanned))
	for _, needle := range forbiddenInProblems {
		assert.NotContainsf(t, lowered, strings.ToLower(needle),
			"refusal leaks %q; a caller must never be able to read internals out of an error. body: %s",
			needle, scanned)
	}
	// The absolute path of the build tree is never part of an answer.
	if repoRoot != "" {
		assert.NotContains(t, lowered, strings.ToLower(repoRoot),
			"refusal leaks the source tree path")
		assert.NotContains(t, lowered, strings.ToLower(strings.ReplaceAll(repoRoot, `\`, `/`)),
			"refusal leaks the source tree path")
	}
	// The database name identifies the deployment's storage; it is not a
	// caller's business either.
	if testDBName != "" {
		assert.NotContains(t, lowered, strings.ToLower(testDBName),
			"refusal leaks the database name")
	}
}

// oversizedJSONBody builds a syntactically valid JSON document larger than the
// configured limit, so the refusal is about size and not about parsing.
func oversizedJSONBody(limit int64) []byte {
	pad := bytes.Repeat([]byte("a"), int(limit)+4096)
	out := make([]byte, 0, len(pad)+64)
	out = append(out, []byte(`{"account_id":"`)...)
	out = append(out, pad...)
	out = append(out, []byte(`"}`)...)
	return out
}
