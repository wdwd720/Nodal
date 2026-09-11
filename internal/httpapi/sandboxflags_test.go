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

// The temperature comes from the mode that opened the payment, recorded on the
// funding row, and not from the deployment's current configuration (F-158).
func TestCreditPurchaseCarriesItsTemperature(t *testing.T) {
	t.Parallel()
	base := credit.Funding{ID: credit.NewFundingID(), AccountID: testAccountID, State: credit.FundingCaptured, Provider: "stripe_credit"}
	for mode, sandbox := range map[string]bool{"fake": true, "sandbox": true, "live": false} {
		f := base
		f.ProviderMode = mode
		out := toAPICreditPurchase(f)
		require.NotNil(t, out.Sandbox, mode)
		assert.Equal(t, sandbox, *out.Sandbox, mode)
		require.NotNil(t, out.ProviderMode, mode)
		assert.Equal(t, mode, string(*out.ProviderMode), "the recorded fact is published, not only its interpretation")
	}

	// A funding written before 00793 recorded no mode. It is rendered as
	// sandbox: an unrecorded mode cannot be asserted to be real money, and
	// over-labelling value as simulated is the safe direction of that mistake.
	out := toAPICreditPurchase(base)
	require.NotNil(t, out.Sandbox)
	assert.True(t, *out.Sandbox)
	assert.Nil(t, out.ProviderMode, "and the API does not invent a mode it was never told")
}
