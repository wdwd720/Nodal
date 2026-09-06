package privy

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	privyclient "github.com/privy-io/go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/signing/inspect"
)

func TestOptions_Validate(t *testing.T) {
	t.Parallel()
	cfg := config.ProviderConfig{Mode: config.ProviderModeSandbox, Name: Name, BaseURL: "https://api.staging.privy.io", APIKeyRef: "env://PRIVY_SECRET", Timeout: time.Second}
	good := Options{Env: config.EnvTest, AppID: "app", AuthorizationKeyRef: "env://PRIVY_AUTH_KEY", OwnerQuorumID: "kq", PolicyID: "pol"}
	require.NoError(t, good.withDefaults(cfg).validate(cfg))
	d := good.withDefaults(cfg)
	assert.Equal(t, cfg.APIKeyRef, d.AppSecretRef)
	assert.Equal(t, "kq", d.SignerQuorumID)
	assert.Equal(t, CAIP2Mainnet, d.CAIP2)
	assert.Equal(t, "solana-mainnet", d.Chain)
	assert.Equal(t, inspect.DefaultAllowedPrograms(), d.AllowedProgramIDs)

	fake := cfg
	fake.Mode = config.ProviderModeFake
	assert.Error(t, good.withDefaults(fake).validate(fake), "fake mode is not a Privy adapter")

	for name, mutate := range map[string]func(o *Options, c *config.ProviderConfig){
		"no app id":       func(o *Options, _ *config.ProviderConfig) { o.AppID = "" },
		"no secret":       func(o *Options, c *config.ProviderConfig) { c.APIKeyRef = "" },
		"no auth key":     func(o *Options, _ *config.ProviderConfig) { o.AuthorizationKeyRef = "" },
		"no owner":        func(o *Options, _ *config.ProviderConfig) { o.OwnerQuorumID = "" },
		"no policy":       func(o *Options, _ *config.ProviderConfig) { o.PolicyID = "" },
		"bad caip2":       func(o *Options, _ *config.ProviderConfig) { o.CAIP2 = "eip155:1" },
		"http in prod":    func(o *Options, c *config.ProviderConfig) { o.Env = config.EnvProd; c.BaseURL = "http://localhost" },
		"plain secret":    func(o *Options, c *config.ProviderConfig) { o.Env = config.EnvProd; c.APIKeyRef = "plain-secret" },
		"unknown mode":    func(_ *Options, c *config.ProviderConfig) { c.Mode = "weird" },
		"devnet chain ok": nil,
	} {
		o, c := good, cfg
		if mutate == nil {
			o.CAIP2 = CAIP2Devnet
			d := o.withDefaults(c)
			require.NoError(t, d.validate(c))
			assert.Equal(t, "solana-devnet", d.Chain)
			continue
		}
		mutate(&o, &c)
		assert.Error(t, o.withDefaults(c).validate(c), name)
	}
}

func TestVerifyPolicy(t *testing.T) {
	t.Parallel()
	want := []string{inspect.SystemProgram, inspect.TokenProgram, inspect.JupiterV6Program}
	allow := func(programs ...string) policyRule {
		return policyRule{Method: "signTransaction", Action: "ALLOW", Conditions: []policyCondition{{
			FieldSource: "solana_program_instruction", Field: "programId", Operator: "in", Values: programs,
		}}}
	}
	denyAll := policyRule{Method: "*", Action: "DENY"}

	allowed, problems := verifyPolicy("solana", []policyRule{allow(want...), denyAll}, want)
	assert.Empty(t, problems)
	assert.ElementsMatch(t, want, allowed)

	cases := map[string]struct {
		chain string
		rules []policyRule
		want  string
	}{
		"wrong chain":         {"ethereum", []policyRule{allow(want...), denyAll}, "chain_type"},
		"missing program":     {"solana", []policyRule{allow(inspect.SystemProgram), denyAll}, "does not allow required program"},
		"extra program":       {"solana", []policyRule{allow(append(want, inspect.Token2022Program)...), denyAll}, "allows unexpected program"},
		"no deny-all":         {"solana", []policyRule{allow(want...)}, "no final DENY"},
		"no allow":            {"solana", []policyRule{denyAll}, "no ALLOW rule"},
		"allows signAndSend":  {"solana", []policyRule{allow(want...), {Method: "signAndSendTransaction", Action: "ALLOW"}, denyAll}, "only signTransaction may be allowed"},
		"allows export":       {"solana", []policyRule{allow(want...), {Method: "exportPrivateKey", Action: "ALLOW"}, denyAll}, "only signTransaction may be allowed"},
		"wrong operator":      {"solana", []policyRule{{Method: "signTransaction", Action: "ALLOW", Conditions: []policyCondition{{FieldSource: "solana_program_instruction", Field: "programId", Operator: "eq", Values: want}}}, denyAll}, "expected solana_program_instruction/programId/in"},
		"two allow rules":     {"solana", []policyRule{allow(want...), allow(want...), denyAll}, "more than one ALLOW"},
		"conditionless allow": {"solana", []policyRule{{Method: "signTransaction", Action: "ALLOW"}, denyAll}, "expected exactly one programId condition"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, problems := verifyPolicy(tc.chain, tc.rules, want)
			require.NotEmpty(t, problems)
			found := false
			for _, p := range problems {
				if assert.ObjectsAreEqual(true, contains(p, tc.want)) {
					found = true
				}
			}
			assert.True(t, found, "problems %v should mention %q", problems, tc.want)
		})
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestDefaultPolicyRules_VerifyAgainstThemselves(t *testing.T) {
	t.Parallel()
	want := inspect.DefaultAllowedPrograms()
	rules := DefaultPolicyRules(want)
	require.Len(t, rules, 2)
	var converted []policyRule
	for _, r := range rules {
		pr := policyRule{Method: string(r.Method), Action: string(r.Action)}
		for _, c := range r.Conditions {
			cond := c.OfSolanaProgramInstruction
			require.NotNil(t, cond)
			pr.Conditions = append(pr.Conditions, policyCondition{FieldSource: string(cond.FieldSource), Field: string(cond.Field), Operator: string(cond.Operator), Values: cond.Value.OfStringArray})
		}
		converted = append(converted, pr)
	}
	_, problems := verifyPolicy("solana", converted, want)
	assert.Empty(t, problems)
}

func TestMapError(t *testing.T) {
	t.Parallel()
	api := func(status int, body string, headers http.Header) error {
		resp := &http.Response{StatusCode: status, Header: headers}
		if resp.Header == nil {
			resp.Header = http.Header{}
		}
		e := &privyclient.Error{StatusCode: status, Request: &http.Request{Method: "POST"}, Response: resp}
		_ = e.UnmarshalJSON([]byte(body))
		return e
	}
	cases := []struct {
		name  string
		err   error
		code  errs.Code
		class provider.RetryClass
	}{
		{"nil", nil, "", ""},
		{"timeout", context.DeadlineExceeded, errs.CodeProviderUnavailable, provider.UnknownEffectWrite},
		{"400", api(400, `{"error":"invalid transaction"}`, nil), errs.CodeValidationFailed, provider.UnknownEffectWrite},
		{"401", api(401, `{}`, nil), errs.CodeUnauthenticated, provider.SafeRetry},
		{"403 policy", api(403, `{"code":"POLICY_VIOLATION","error":"denied by policy"}`, nil), errs.CodeSigningRejected, provider.UnknownEffectWrite},
		{"403 other", api(403, `{}`, nil), errs.CodeForbidden, provider.SafeRetry},
		{"404", api(404, `{}`, nil), errs.CodeNotFound, provider.SafeRetry},
		{"409", api(409, `{}`, nil), errs.CodeConflict, provider.IdempotentWrite},
		{"429", api(429, `{}`, http.Header{"Retry-After": []string{"7"}}), errs.CodeRateLimited, provider.UnknownEffectWrite},
		{"503", api(503, `<html>`, nil), errs.CodeProviderUnavailable, provider.UnknownEffectWrite},
		{"418", api(418, `{}`, nil), errs.CodeInternal, provider.SafeRetry},
		{"plain", errors.New("boom"), errs.CodeInternal, provider.SafeRetry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mapError("op", tc.class, tc.err)
			if tc.err == nil {
				assert.NoError(t, got)
				return
			}
			require.Error(t, got)
			assert.Equal(t, tc.code, errs.CodeOf(got))
			rc, ok := RetryClassOf(got)
			require.True(t, ok)
			assert.Equal(t, tc.class, rc)
			e, _ := errs.As(got)
			assert.Equal(t, Name, e.Fields[FieldProvider])
			assert.NotContains(t, e.Detail, "invalid transaction", "bodies never reach Detail")
			if tc.name == "429" {
				require.NotNil(t, e.RetryAfter)
				assert.Equal(t, 7*time.Second, *e.RetryAfter)
			}
		})
	}
	assert.Equal(t, "POLICY_VIOLATION", bodyCode(`{"error":"Policy violation: program not allowed"}`))
	assert.Equal(t, "rate_limited", bodyCode(`{"code":"rate_limited"}`))
	assert.Equal(t, "", bodyCode(`{"error":"something went wrong"}`))
	assert.Equal(t, "", bodyCode(``))
	_, ok := RetryClassOf(errors.New("x"))
	assert.False(t, ok)
}

func TestNew_FailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := config.ProviderConfig{Mode: config.ProviderModeSandbox, Name: Name, BaseURL: "http://127.0.0.1:1", APIKeyRef: "secret", Timeout: time.Second}
	resolver, err := config.NewPlainResolver(config.EnvTest)
	require.NoError(t, err)
	base := Options{Env: config.EnvTest, AppID: "app", AuthorizationKeyRef: "bm90LWEta2V5", OwnerQuorumID: "kq", PolicyID: "pol"}

	a, err := New(ctx, cfg, base, resolver, nil)
	require.NoError(t, err)
	assert.Equal(t, Name, a.Name())
	assert.Equal(t, provider.CodeComplete, a.VerificationLabel())
	assert.Equal(t, "solana-mainnet", a.Chain())

	_, err = New(ctx, cfg, base, nil, nil)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "resolver required")

	bad := base
	bad.AuthorizationKeyRef = "not base64 !!!"
	_, err = New(ctx, cfg, bad, resolver, nil)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	empty := cfg
	empty.APIKeyRef = "   "
	_, err = New(ctx, empty, base, resolver, nil)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	fakeMode := cfg
	fakeMode.Mode = config.ProviderModeFake
	_, err = New(ctx, fakeMode, base, resolver, nil)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// Wrong chain requests are refused before any network call.
	_, err = a.CreateWallet(ctx, "0192f7a0-1b2c-7d3e-8f4a-5b6c7d8e9f01", "solana-devnet")
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	_, err = a.CreateWallet(ctx, "nope", "solana-mainnet")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = a.GetWallet(ctx, "")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.NotEmpty(t, CreateWalletIdempotencyKey("a", "b"))
	assert.NotEqual(t, CreateWalletIdempotencyKey("a", "b"), CreateWalletIdempotencyKey("a", "c"))
}
