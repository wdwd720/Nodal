//go:build integration

package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
)

// An account that reconciliation freezes can be unfrozen.
//
// The two halves were wired asymmetrically (F-52). `cmd/reconciliation-worker`
// raises records on a ticker and `blocks_new_risk` is read by buying power, so
// an automated check removes an account's capacity to trade. `cmd/api` set
// `Reconcile: nil`, so both admin endpoints answered UNSUPPORTED and
// `Engine.ResolveManual` had no caller outside its own tests. A deployment
// could freeze somebody and had no button.
//
// This walks it: raise a blocking record, see the block, resolve it over HTTP
// as an operator, see the block gone. The "see the block" step is what makes
// the test about the harm rather than about a status column — a resolution that
// changed the row and left the account blocked would pass a weaker test.

func newReconciliationPort(t *testing.T, d *db.DB, clk clock.Clock) ReconciliationPort {
	t.Helper()
	engine, err := reconciliation.NewEngine(reconciliation.Config{
		DB:      d,
		Clock:   clk,
		Records: reconciliation.NewRepository(clk, event.NewOutbox(clk), audit.NewWriter()),
		Policy:  reconciliation.DefaultPolicy(),
		Metrics: reconciliation.NoopMetrics(),
	})
	require.NoError(t, err)
	port := NewReconciliationPort(ReconciliationDeps{Engine: engine, ReadModel: NewReadModel(d), DB: d})
	require.NotNil(t, port, "the port must exist; a nil one answers UNSUPPORTED and proves nothing")
	return port
}

// blockingRecord writes a blocking reconciliation record for an account, in the
// status a worker-detected difference actually reaches.
//
// MISMATCH, not OPEN. OPEN means the engine has not yet decided whether there
// is a difference at all, and its only transitions are the engine's own
// (MATCHED, MISMATCH) — an operator cannot resolve one and should not be able
// to. MISMATCH is what a real detection produces and what an operator is asked
// to answer.
//
// Written directly because these tests are about the RESOLUTION: reaching it
// through the engine's detection path would drag in observers and adapters that
// have nothing to do with the question.
func blockingRecord(t *testing.T, d *db.DB, account accounts.AccountID) string {
	return recordInStatus(t, d, account, "MISMATCH")
}

func recordInStatus(t *testing.T, d *db.DB, account accounts.AccountID, status string) string {
	t.Helper()
	id := newKey()
	_, err := d.Exec(t.Context(),
		`INSERT INTO reconciliation_records
		   (id, kind, mode, scope_type, scope_id, account_id, expected, observed, difference,
		    status, material, blocks_new_risk, opened_at)
		 VALUES ($1,'WALLET_BALANCE','PERIODIC','account',$2,$3,'{}','{}','{"delta":"1"}',
		         $4, false, true, now())`,
		id, account.String(), account, status)
	require.NoError(t, err, "seeding a blocking reconciliation record")
	return id
}

func blockCount(t *testing.T, d *db.DB, account accounts.AccountID) int {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT count(*) FROM reconciliation_records
		  WHERE account_id = $1 AND blocks_new_risk
		    AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED')`,
		account).Scan(&n))
	return n
}

func TestIntegration_AnOperatorCanClearAReconciliationBlock(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	h.server.opts.Ports.Reconciliation = newReconciliationPort(t, d, h.clk)

	account := seedCustomerAccount(t, d)
	recordID := blockingRecord(t, d, account)
	require.Equal(t, 1, blockCount(t, d, account),
		"the account must actually be blocked, or clearing it proves nothing")

	ops := seedOperator(t, d, security.RoleOperations)
	res := h.as(&ops).do(http.MethodPost,
		"/v1/admin/reconciliation/records/"+recordID+"/resolve",
		map[string]any{
			"reason":       "investigated: the chain observation was a duplicate delivery",
			"evidence_ref": "INC-2026-0042",
		}, "Idempotency-Key", newKey())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	assert.Equal(t, 0, blockCount(t, d, account),
		"the account's capacity must come back; a resolution that leaves the block is not one")

	// The record says who cleared it and why. A cleared block with no reason is
	// the thing a hand-written UPDATE would have left behind.
	var status, reason, evidence, actorID string
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT status, resolution_reason, resolution_evidence_ref, resolved_by_actor_id
		   FROM reconciliation_records WHERE id = $1`, recordID).
		Scan(&status, &reason, &evidence, &actorID))
	assert.Equal(t, "RESOLVED_MANUAL", status)
	assert.Contains(t, reason, "duplicate delivery")
	assert.Equal(t, "INC-2026-0042", evidence)
	assert.Equal(t, ops.SubjectID, actorID)
}

// TestIntegration_AReconciliationResolutionRefusesToPost: the half that is
// deliberately not wired says so, rather than approximating a posting.
func TestIntegration_AReconciliationResolutionRefusesToPost(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	h.server.opts.Ports.Reconciliation = newReconciliationPort(t, d, h.clk)

	account := seedCustomerAccount(t, d)
	recordID := blockingRecord(t, d, account)
	ops := seedOperator(t, d, security.RoleOperations)

	res := h.as(&ops).do(http.MethodPost,
		"/v1/admin/reconciliation/records/"+recordID+"/resolve",
		map[string]any{
			"reason":       "investigated: the difference is real and needs a correction",
			"evidence_ref": "INC-2026-0043",
			"compensation": map[string]any{
				"reason_code": "CHAIN_FEE_UNDERCOUNTED",
				"entries":     []any{},
			},
		}, "Idempotency-Key", newKey())

	assert.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "compensating posting")
	assert.Equal(t, 1, blockCount(t, d, account),
		"a refused resolution must leave the record exactly as it was")
}

// TestIntegration_AnOpenRecordIsNotOperatorResolvable: the state machine's rule,
// asserted rather than worked around.
//
// OPEN means the engine has not yet decided there is a mismatch. Its only
// transitions are the engine's own, and the refusal names both states so an
// operator can see why. The adapter deliberately does not force a path here: it
// moves MISMATCH and ESCALATED into INVESTIGATING because that is the step
// standing between them and a resolution, and leaves OPEN to the machine.
func TestIntegration_AnOpenRecordIsNotOperatorResolvable(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	h.server.opts.Ports.Reconciliation = newReconciliationPort(t, d, h.clk)

	account := seedCustomerAccount(t, d)
	recordID := recordInStatus(t, d, account, "OPEN")
	ops := seedOperator(t, d, security.RoleOperations)

	res := h.as(&ops).do(http.MethodPost,
		"/v1/admin/reconciliation/records/"+recordID+"/resolve",
		map[string]any{"reason": "looks fine to me at a glance", "evidence_ref": "INC-2026-0044"},
		"Idempotency-Key", newKey())

	assert.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "OPEN")
	assert.Equal(t, 1, blockCount(t, d, account))
}

// TestIntegration_AnAgentCanNeverResolveAReconciliationRecord.
func TestIntegration_AnAgentCanNeverResolveAReconciliationRecord(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	h.server.opts.Ports.Reconciliation = newReconciliationPort(t, d, h.clk)

	account := seedCustomerAccount(t, d)
	recordID := blockingRecord(t, d, account)

	agent := security.Principal{
		SubjectID: "agent-1", ActorType: security.ActorAgent,
		AccountIDs: []string{account.String()},
		SessionID:  testSessionID, AuthTime: testNow.Add(-time.Minute),
	}
	res := h.as(&agent).do(http.MethodPost,
		"/v1/admin/reconciliation/records/"+recordID+"/resolve",
		map[string]any{"reason": "resolving on my own authority", "evidence_ref": "none"},
		"Idempotency-Key", newKey())

	assert.NotEqual(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 1, blockCount(t, d, account))
}
