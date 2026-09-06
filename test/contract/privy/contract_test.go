package privy_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/privy-io/go-sdk/authorization"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/privy"
	"github.com/nodal/controlplane/internal/signing/inspect"
	"github.com/nodal/controlplane/internal/signing/signingtest"
	"github.com/nodal/controlplane/internal/wallet"
)

const (
	appID     = "cmapp0000000000000000000"
	appSecret = "test-app-secret" //nolint:gosec // contract test credential for the fake server
	ownerKQ   = "kq0000000000000000000001"
	policyID  = "pol000000000000000000001"
	walletID  = "wal000000000000000000001"
)

// fakePrivy emulates the documented endpoints. Behavior is switched per
// test through mode.
type fakePrivy struct {
	t        *testing.T
	key      solana.PrivateKey
	mu       sync.Mutex
	mode     string
	requests atomic.Int32
	lastRPC  map[string]any
	policy   map[string]any
	wallet   map[string]any
}

func newFakePrivy(t *testing.T) *fakePrivy {
	f := &fakePrivy{t: t, key: signingtest.KeyFromSeed("privy-wallet")}
	programs := inspect.DefaultAllowedPrograms()
	f.policy = map[string]any{
		"id": policyID, "chain_type": "solana", "name": "delegated-swap-wallet", "owner_id": ownerKQ, "version": "1.0", "created_at": 1_757_000_000_000,
		"rules": []map[string]any{
			{"id": "rule1", "name": "allow-known-programs", "method": "signTransaction", "action": "ALLOW", "conditions": []map[string]any{
				{"field_source": "solana_program_instruction", "field": "programId", "operator": "in", "value": programs},
			}},
			{"id": "rule2", "name": "deny-everything-else", "method": "*", "action": "DENY", "conditions": []map[string]any{}},
		},
	}
	f.wallet = map[string]any{
		"id": walletID, "address": f.key.PublicKey().String(), "chain_type": "solana", "policy_ids": []string{policyID},
		"owner_id": ownerKQ, "additional_signers": []map[string]any{}, "created_at": 1_757_000_000_000, "public_key": "",
	}
	return f
}

func (f *fakePrivy) setMode(m string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = m
}

func (f *fakePrivy) getMode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mode
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakePrivy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	// Documented: Basic auth + privy-app-id on every request.
	user, pass, ok := r.BasicAuth()
	if !ok || user != appID || pass != appSecret || r.Header.Get("privy-app-id") != appID {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid app credentials"})
		return
	}
	switch f.getMode() {
	case "timeout":
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		return
	case "rate_limit":
		w.Header().Set("Retry-After", "3")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many requests"})
		return
	case "5xx":
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream unavailable"})
		return
	case "unauthorized":
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid app credentials"})
		return
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v1/wallets":
		f.createWallet(w, r)
	case r.Method == http.MethodGet && path == "/v1/wallets/"+walletID:
		if f.getMode() == "missing_field" {
			writeJSON(w, http.StatusOK, map[string]any{"id": walletID, "chain_type": "solana"})
			return
		}
		if f.getMode() == "no_policy" {
			wl := cloneMap(f.wallet)
			wl["policy_ids"] = []string{}
			writeJSON(w, http.StatusOK, wl)
			return
		}
		if f.getMode() == "archived" {
			wl := cloneMap(f.wallet)
			wl["archived_at"] = 1_757_000_001_000
			writeJSON(w, http.StatusOK, wl)
			return
		}
		writeJSON(w, http.StatusOK, f.wallet)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/wallets/"):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "wallet not found"})
	case r.Method == http.MethodGet && path == "/v1/policies/"+policyID:
		if f.getMode() == "loose_policy" {
			p := cloneMap(f.policy)
			p["rules"] = []map[string]any{{"id": "r", "name": "allow-all", "method": "*", "action": "ALLOW", "conditions": []map[string]any{}}}
			writeJSON(w, http.StatusOK, p)
			return
		}
		writeJSON(w, http.StatusOK, f.policy)
	case r.Method == http.MethodPost && path == "/v1/wallets/"+walletID+"/rpc":
		f.rpc(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (f *fakePrivy) createWallet(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if body["chain_type"] != "solana" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported chain_type"})
		return
	}
	if r.Header.Get("privy-idempotency-key") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing idempotency key"})
		return
	}
	if f.getMode() == "missing_field" {
		writeJSON(w, http.StatusOK, map[string]any{"id": walletID, "chain_type": "solana"})
		return
	}
	wl := cloneMap(f.wallet)
	wl["external_id"] = body["external_id"]
	writeJSON(w, http.StatusOK, wl)
}

func (f *fakePrivy) rpc(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	f.mu.Lock()
	f.lastRPC = map[string]any{
		"body": body, "idempotency": r.Header.Get("privy-idempotency-key"),
		"authorization": r.Header.Get("privy-authorization-signature"), "expiry": r.Header.Get("privy-request-expiry"),
	}
	f.mu.Unlock()
	if body["method"] != "signTransaction" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported method"})
		return
	}
	params, _ := body["params"].(map[string]any)
	txB64, _ := params["transaction"].(string)
	unsigned, err := base64.StdEncoding.DecodeString(txB64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "transaction is not base64"})
		return
	}
	tx, err := solana.TransactionFromBytes(unsigned)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid transaction", "code": "INVALID_TRANSACTION"})
		return
	}
	switch f.getMode() {
	case "policy_violation":
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "Policy violation: program not allowed", "code": "POLICY_VIOLATION"})
		return
	case "missing_field":
		writeJSON(w, http.StatusOK, map[string]any{"method": "signTransaction", "data": map[string]any{"encoding": "base64"}})
		return
	case "wrong_method":
		writeJSON(w, http.StatusOK, map[string]any{"method": "signMessage", "data": map[string]any{"signature": "abc", "encoding": "base64"}})
		return
	case "altered_message":
		// Sign a different message: the adapter must refuse it.
		other := signingtest.NewSwap()
		other.WalletKey = f.key
		other.Derive()
		other.InAmount++
		tx, err = solana.TransactionFromBytes(other.Golden(inspect.VersionLegacy))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "fixture"})
			return
		}
	}
	pub := f.key.PublicKey()
	if _, err := tx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
		if k.Equals(pub) {
			return &f.key
		}
		return nil
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "fee payer is not this wallet", "code": "INVALID_TRANSACTION"})
		return
	}
	signed, err := tx.MarshalBinary()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "marshal"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"method": "signTransaction", "data": map[string]any{
		"signed_transaction": base64.StdEncoding.EncodeToString(signed), "encoding": "base64",
	}})
}

func newAdapter(t *testing.T, srv *httptest.Server, timeout time.Duration) *privy.Adapter {
	t.Helper()
	kp, err := authorization.GenerateP256KeyPair()
	require.NoError(t, err)
	resolver, err := config.NewPlainResolver(config.EnvTest)
	require.NoError(t, err)
	cfg := config.ProviderConfig{Mode: config.ProviderModeSandbox, Name: privy.Name, BaseURL: srv.URL, APIKeyRef: config.SecretRef(appSecret), Timeout: timeout}
	a, err := privy.New(context.Background(), cfg, privy.Options{
		Env: config.EnvTest, AppID: appID, AuthorizationKeyRef: config.SecretRef(kp.PrivateKey),
		OwnerQuorumID: ownerKQ, PolicyID: policyID, CAIP2: privy.CAIP2Devnet, HTTPClient: srv.Client(),
	}, resolver, nil)
	require.NoError(t, err)
	return a
}

func signRequest(t *testing.T, f *fakePrivy) (wallet.SignRequest, []byte) {
	t.Helper()
	s := signingtest.NewSwap()
	s.WalletKey = f.key
	s.Derive()
	raw := s.Golden(inspect.VersionV0)
	return wallet.SignRequest{ProviderWalletID: walletID, Chain: "solana-devnet", UnsignedTx: raw, IdempotencyKey: "0192f7a0-1b2c-7d3e-8f4a-5b6c7d8e9f01", Purpose: wallet.PurposeSwap}, raw
}

func TestContract_Valid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakePrivy(t)
	srv := httptest.NewServer(f)
	defer srv.Close()
	a := newAdapter(t, srv, 2*time.Second)
	assert.Equal(t, provider.CodeComplete, a.VerificationLabel())

	acct := accounts.NewAccountID().String()
	w, err := a.CreateWallet(ctx, acct, "solana-devnet")
	require.NoError(t, err)
	assert.Equal(t, walletID, w.ProviderWalletID)
	assert.Equal(t, f.key.PublicKey().String(), w.Address)
	assert.Equal(t, wallet.KindEmbeddedDelegated, w.Kind)
	assert.Equal(t, wallet.StatusActive, w.Status)
	assert.Equal(t, acct, w.AccountID.String())

	got, err := a.GetWallet(ctx, walletID)
	require.NoError(t, err)
	assert.Equal(t, w.Address, got.Address)

	st, err := a.VerifyDelegation(ctx, walletID)
	require.NoError(t, err)
	assert.True(t, st.Verified(), st.Detail)
	assert.ElementsMatch(t, inspect.DefaultAllowedPrograms(), st.AllowedPrograms)
	assert.Equal(t, policyID, st.PolicyID)

	capability, err := a.Capabilities(ctx)
	require.NoError(t, err)
	assert.True(t, capability.DelegationVerified(), capability.Detail)
	assert.Equal(t, provider.CodeComplete, capability.VerificationLabel)

	req, raw := signRequest(t, f)
	res, err := a.SignTransaction(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, provider.UnknownEffectWrite, res.RetryClass)
	assert.Len(t, res.Signature, 64)
	assert.Contains(t, res.ProviderRef, req.IdempotencyKey)
	stx, err := solana.TransactionFromBytes(res.SignedTx)
	require.NoError(t, err)
	require.NoError(t, stx.VerifySignatures())
	msg, err := stx.Message.MarshalBinary()
	require.NoError(t, err)
	unsigned, err := solana.TransactionFromBytes(raw)
	require.NoError(t, err)
	umsg, err := unsigned.Message.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, umsg, msg, "the signed transaction carries the identical message")
	assert.True(t, ed25519.Verify(ed25519.PublicKey(f.key.PublicKey().Bytes()), msg, res.Signature))

	f.mu.Lock()
	last := f.lastRPC
	f.mu.Unlock()
	assert.Equal(t, req.IdempotencyKey, last["idempotency"], "privy-idempotency-key forwarded")
	assert.NotEmpty(t, last["authorization"], "privy-authorization-signature computed by the SDK")
	assert.NotEmpty(t, last["expiry"], "privy-request-expiry set")
	body, ok := last["body"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "signTransaction", body["method"])
	params, ok := body["params"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "base64", params["encoding"])
}

func TestContract_Failures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		mode  string
		code  errs.Code
		class provider.RetryClass
	}{
		{"invalid (400)", "invalid", errs.CodeValidationFailed, provider.UnknownEffectWrite},
		{"rate limit (429)", "rate_limit", errs.CodeRateLimited, provider.UnknownEffectWrite},
		{"5xx", "5xx", errs.CodeProviderUnavailable, provider.UnknownEffectWrite},
		{"unauthorized (401)", "unauthorized", errs.CodeUnauthenticated, provider.UnknownEffectWrite},
		{"policy violation", "policy_violation", errs.CodeSigningRejected, provider.UnknownEffectWrite},
		{"missing signed_transaction", "missing_field", errs.CodeInternal, provider.UnknownEffectWrite},
		{"unexpected response method", "wrong_method", errs.CodeInternal, ""},
		{"altered message", "altered_message", errs.CodeSigningRejected, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newFakePrivy(t)
			srv := httptest.NewServer(f)
			defer srv.Close()
			a := newAdapter(t, srv, 2*time.Second)
			f.setMode(tc.mode)
			req, _ := signRequest(t, f)
			if tc.mode == "invalid" {
				req.UnsignedTx = []byte("garbage")
				_, err := a.SignTransaction(ctx, req)
				require.Error(t, err)
				assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "rejected before any network call")
				assert.Equal(t, int32(0), f.requests.Load())
				// A syntactically valid transaction the provider rejects as invalid.
				f.setMode("")
				req, _ = signRequest(t, f)
				other := signingtest.NewSwap() // fee payer is not the provider wallet → provider says invalid
				req.UnsignedTx = other.Golden(inspect.VersionLegacy)
				_, err = a.SignTransaction(ctx, req)
				require.Error(t, err)
				assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
				return
			}
			_, err := a.SignTransaction(ctx, req)
			require.Error(t, err)
			assert.Equal(t, tc.code, errs.CodeOf(err), "%v", err)
			if tc.class != "" {
				rc, ok := privy.RetryClassOf(err)
				assert.True(t, ok)
				assert.Equal(t, tc.class, rc)
			}
			e, _ := errs.As(err)
			assert.NotContains(t, e.Detail, "upstream unavailable")
			assert.NotContains(t, e.Detail, "Policy violation: program")
			if tc.mode == "rate_limit" {
				require.NotNil(t, e.RetryAfter)
				assert.Equal(t, 3*time.Second, *e.RetryAfter)
			}
			assert.Equal(t, int32(1), f.requests.Load(), "the adapter never retries on its own")
		})
	}
}

func TestContract_Timeout(t *testing.T) {
	t.Parallel()
	f := newFakePrivy(t)
	srv := httptest.NewServer(f)
	defer srv.Close()
	a := newAdapter(t, srv, 300*time.Millisecond)
	f.setMode("timeout")
	req, _ := signRequest(t, f)
	start := time.Now()
	_, err := a.SignTransaction(context.Background(), req)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err), "%v", err)
	rc, ok := privy.RetryClassOf(err)
	assert.True(t, ok)
	assert.Equal(t, provider.UnknownEffectWrite, rc)

	// Caller cancellation is honored too.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = a.GetWallet(ctx, walletID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || errs.CodeOf(err) == errs.CodeProviderUnavailable)
}

func TestContract_WalletAndDelegationStates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakePrivy(t)
	srv := httptest.NewServer(f)
	defer srv.Close()
	a := newAdapter(t, srv, 2*time.Second)

	_, err := a.GetWallet(ctx, "wal-unknown")
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	rc, _ := privy.RetryClassOf(err)
	assert.Equal(t, provider.SafeRetry, rc)

	f.setMode("no_policy")
	st, err := a.VerifyDelegation(ctx, walletID)
	require.NoError(t, err)
	assert.False(t, st.Verified())
	assert.Contains(t, st.Detail, "configured policy is not attached")

	f.setMode("archived")
	st, err = a.VerifyDelegation(ctx, walletID)
	require.NoError(t, err)
	assert.Equal(t, wallet.DelegationRevoked, st.State)
	w, err := a.GetWallet(ctx, walletID)
	require.NoError(t, err)
	assert.Equal(t, wallet.StatusRevoked, w.Status)

	f.setMode("loose_policy")
	st, err = a.VerifyDelegation(ctx, walletID)
	require.NoError(t, err)
	assert.False(t, st.Verified())
	assert.Contains(t, st.Detail, "only signTransaction may be allowed")
	capability, err := a.Capabilities(ctx)
	require.NoError(t, err)
	assert.False(t, capability.DelegationVerified(), "an allow-all policy never verifies delegated signing")

	f.setMode("missing_field")
	_, err = a.GetWallet(ctx, walletID)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err), "wallet without address is unusable")
	_, err = a.CreateWallet(ctx, accounts.NewAccountID().String(), "solana-devnet")
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))

	f.setMode("5xx")
	_, err = a.Capabilities(ctx)
	assert.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	f.setMode("rate_limit")
	_, err = a.CreateWallet(ctx, accounts.NewAccountID().String(), "solana-devnet")
	assert.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	rc, _ = privy.RetryClassOf(err)
	assert.Equal(t, provider.IdempotentWrite, rc)

	// The idempotency key for wallet creation is stable per (account, chain).
	acct := accounts.NewAccountID().String()
	assert.Equal(t, privy.CreateWalletIdempotencyKey(acct, "solana-devnet"), privy.CreateWalletIdempotencyKey(acct, "solana-devnet"))
	h := sha256.Sum256([]byte("wallet:" + acct + ":solana-devnet"))
	assert.Contains(t, privy.CreateWalletIdempotencyKey(acct, "solana-devnet"), "wallet-")
	_ = h
}
