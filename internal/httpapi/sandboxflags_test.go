package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/credit"
)

// The UI labels temperatures from what the API says, never from where the
// bundle was loaded: /v1/version states whether the deployment is a sandbox
// tier, and a Credit purchase says whether it ran through a live provider.
func TestVersionSaysWhetherThisIsASandboxTier(t *testing.T) {
	t.Parallel()
	for _, sandbox := range []bool{false, true} {
		h := newHarness(t)
		h.as(nil)
		h.server.opts.SandboxTier = sandbox
		res := h.do(http.MethodGet, "/v1/version", nil)
		require.Equal(t, http.StatusOK, res.Code)
		var body struct {
			SandboxTier *bool `json:"sandbox_tier"`
		}
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
		require.NotNil(t, body.SandboxTier, "the flag is always present, so a UI cannot mistake absence for live")
		assert.Equal(t, sandbox, *body.SandboxTier)
	}
}

func TestCreditPurchaseCarriesItsTemperature(t *testing.T) {
	t.Parallel()
	f := credit.Funding{ID: credit.NewFundingID(), AccountID: testAccountID, State: credit.FundingCaptured, Provider: "stripe_credit"}
	for _, sandbox := range []bool{false, true} {
		out := toAPICreditPurchase(f, sandbox)
		require.NotNil(t, out.Sandbox)
		assert.Equal(t, sandbox, *out.Sandbox)
	}
}
