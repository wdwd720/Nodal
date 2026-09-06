package model_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/model"
	"github.com/nodal/controlplane/internal/model/modeltest"
)

// TestGuard_RefusesCredentialMaterial: secrets never enter model context
// (PART 67). The guard refuses the request rather than scrubbing it.
func TestGuard_RefusesCredentialMaterial(t *testing.T) {
	t.Parallel()
	secrets := map[string]string{
		"pem private key":  "-----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY-----",
		"openssh key":      "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNza\n",
		"anthropic key":    "use sk-ant-api03-AAAABBBBCCCCDDDD to authenticate",
		"generic sk key":   "sk-abcdefghijklmnopqrstuvwxyz012345",
		"aws access key":   "AKIAIOSFODNN7EXAMPLE",
		"aws temp key":     "ASIAIOSFODNN7EXAMPLE",
		"slack token":      "xoxb-123456789012-abcdefghijkl",
		"github token":     "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"jwt":              "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature",
		"bearer header":    "Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345",
		"labeled api key":  "api_key: supersecretvalue123",
		"labeled password": "password=hunter2hunter2",
		"labeled mnemonic": "mnemonic: abandon-abandon-abandon-ability",
		"labeled seed":     "seed = 0123456789abcdef0123",
		"private key pair": "wallet private_key: 5KJvsngHeMpm884wtkJNzQGaCErckhHJBGFsvd3VyK5qMZXj3hS",
	}
	g := model.NewGuard()
	for name, secret := range secrets {
		t.Run(name, func(t *testing.T) {
			// In untrusted content.
			req := baseRequest()
			req.Untrusted = []model.Segment{{Kind: model.SegmentUntrusted, Label: "user_text", Content: secret}}
			err := g.Check(req)
			require.Error(t, err, "must refuse: %s", secret)
			assert.Equal(t, errs.CodeSecretInModelContext, errs.CodeOf(err))
			assert.NotContains(t, err.Error(), secret, "the error must not echo the secret back")

			// And in a tool result.
			req = baseRequest()
			req.ToolResults = []model.Segment{{Kind: model.SegmentToolResult, Label: "tool_output", Content: secret}}
			require.Error(t, g.Check(req))

			// And in the system policy: a template is code, and a key pasted
			// into one is still a leak.
			req = baseRequest()
			req.SystemPolicy = "You compile strategies.\n" + secret
			require.Error(t, g.Check(req))
		})
	}
}

// TestGuard_AllowsLegitimateContent: the guard must not fire on the things
// a real compile carries, or it becomes a liability that gets disabled. IR
// hashes are 64-character hex and must pass.
func TestGuard_AllowsLegitimateContent(t *testing.T) {
	t.Parallel()
	g := model.NewGuard()
	legitimate := []string{
		"buy SOL/USDC with $50 when the 5-minute return exceeds 2%",
		"ir_hash: 8487051d87f34f11fc81bdab65b5baad3f6ec5cc77872331bfa3fa46f72513d5",
		"STRUCTURAL_NO_TRIGGER at triggers: at least one trigger",
		"instrument_id=0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e12",
		"the token description mentions a key market for this asset",
		"max_slippage_bps: 50",
		"strategy_version_id: 0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e13",
		"cardinality: 12345678",
		"tokens_used: 1048576",
	}
	for _, text := range legitimate {
		req := baseRequest()
		req.Untrusted = []model.Segment{{Kind: model.SegmentUntrusted, Label: "user_text", Content: text}}
		assert.NoError(t, g.Check(req), "must not fire on: %s", text)
	}
}

// TestGuarded_RefusedRequestNeverReachesTheProvider: a refused request is
// never billed, so the guard must sit in front of the dial, not after it.
func TestGuarded_RefusedRequestNeverReachesTheProvider(t *testing.T) {
	t.Parallel()
	fake := modeltest.MustNew(config.EnvTest, modeltest.Turn{Body: `{"ok":true}`})
	guarded := model.NewGuarded(fake, model.NewGuard())

	req := baseRequest()
	req.Untrusted = []model.Segment{{Kind: model.SegmentUntrusted, Content: "api_key: supersecretvalue123"}}

	_, err := guarded.Complete(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, errs.CodeSecretInModelContext, errs.CodeOf(err))
	assert.Equal(t, 0, fake.Calls(), "the provider was never dialed, so nothing was billed")

	// A clean request passes through.
	clean := baseRequest()
	clean.Untrusted = []model.Segment{{Kind: model.SegmentUntrusted, Content: "buy SOL when momentum is positive"}}
	resp, err := guarded.Complete(context.Background(), clean)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true}`, string(resp.Structured))
	assert.Equal(t, 1, fake.Calls())
	assert.Equal(t, "fake", guarded.Name())
}
