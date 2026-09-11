package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/security"
)

// A gate's history is the rows the transition functions wrote, oldest first;
// an entry into SANDBOX is flagged so a console can never render it as an
// approval, and the boot-time actor is named as what it is.
func TestAdminGateHistoryNamesWhoMovedTheGate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	h.ports.gates.history = []gates.Transition{
		{ID: gates.NewTransitionID(), From: gates.StateDisabled, To: gates.StateSandbox, ActorType: security.ActorSystem, ActorID: "config:CP_API_SANDBOX_GATES", Reason: "sandbox tier", OccurredAt: at},
		{ID: gates.NewTransitionID(), From: gates.StateSandbox, To: gates.StateDisabled, ActorType: security.ActorOperator, ActorID: "op-1", Reason: "rehearsal over", EvidenceHash: []byte{0xab, 0xcd}, OccurredAt: at.Add(time.Minute)},
	}

	res := h.do(http.MethodGet, "/v1/admin/gates/NATIVE_MARKET_TRADING/history", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	var out []api.CapabilityGateTransition
	res.json(&out)
	require.Len(t, out, 2)
	assert.True(t, out[0].Sandbox, "entering SANDBOX is flagged")
	assert.Equal(t, "SYSTEM", out[0].ActorType)
	assert.Equal(t, "config:CP_API_SANDBOX_GATES", out[0].ActorId)
	assert.False(t, out[1].Sandbox, "leaving SANDBOX is not a sandbox entry")
	require.NotNil(t, out[1].EvidenceHash)
	assert.Equal(t, "abcd", *out[1].EvidenceHash)

	unknown := h.do(http.MethodGet, "/v1/admin/gates/NOT_A_CAPABILITY/history", nil)
	assert.Equal(t, http.StatusBadRequest, unknown.Code)
}
