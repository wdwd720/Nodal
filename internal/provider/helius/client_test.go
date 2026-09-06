package helius_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/helius"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
)

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	ctx := context.Background()
	resolver := config.EnvResolver{Lookup: func(string) (string, bool) { return apiKey, true }}
	_, d := newClient(t, s, nil)
	base := func() (config.ProviderConfig, helius.Options, helius.Deps) {
		return config.ProviderConfig{Mode: config.ProviderModeSandbox, APIKeyRef: "env://K"}, helius.Options{},
			helius.Deps{Deps: solanarpc.Deps{Archive: d.archive, Resolver: resolver}}
	}
	cfg, opts, deps := base()
	cfg.Mode = config.ProviderModeFake
	_, err := helius.New(ctx, cfg, opts, deps)
	require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))

	cfg, opts, deps = base()
	cfg.Mode = "x"
	_, err = helius.New(ctx, cfg, opts, deps)
	require.Error(t, err)

	cfg, opts, deps = base()
	cfg.APIKeyRef = ""
	_, err = helius.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "APIKeyRef is required")

	cfg, opts, deps = base()
	deps.Resolver = nil
	_, err = helius.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "resolver")

	cfg, opts, deps = base()
	deps.Archive = nil
	_, err = helius.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "archive")

	cfg, opts, deps = base()
	deps.Resolver = config.EnvResolver{Lookup: func(string) (string, bool) { return " ", true }}
	_, err = helius.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "empty")

	cfg, opts, deps = base()
	c, err := helius.New(ctx, cfg, opts, deps)
	require.NoError(t, err)
	require.Equal(t, helius.ProviderName, c.Name())
	require.Equal(t, provider.CodeComplete, c.VerificationLabel())
	require.Equal(t, provider.Healthy, c.Health())
	require.NotNil(t, c.Tracker())
	require.Equal(t, "https://devnet.helius-rpc.com", c.RPC().RedactedEndpoint(), "sandbox = devnet; key stripped")
	require.Equal(t, "wss://devnet.helius-rpc.com/", c.RedactedWSURL())
	require.NotContains(t, c.RedactedWSURL(), apiKey)

	cfg, opts, deps = base()
	cfg.Mode = config.ProviderModeLive
	cfg.Name = "helius-primary"
	c, err = helius.New(ctx, cfg, opts, deps)
	require.NoError(t, err)
	require.Equal(t, "helius-primary", c.Name())
	require.Equal(t, "https://mainnet.helius-rpc.com", c.RPC().RedactedEndpoint())

	cfg, opts, deps = base()
	cfg.Mode = config.ProviderModeLive
	cfg.BaseURL = "http://insecure.example.com"
	_, err = helius.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "https", "live mode refuses plaintext")

	cfg, opts, deps = base()
	cfg.BaseURL = "::bad"
	_, err = helius.New(ctx, cfg, opts, deps)
	require.Error(t, err)

	cfg, opts, deps = base()
	opts.WSURL = "https://not-a-socket"
	_, err = helius.New(ctx, cfg, opts, deps)
	require.ErrorContains(t, err, "WSURL")
	opts.WSURL = "wss://stream.example.com/v1"
	c, err = helius.New(ctx, cfg, opts, deps)
	require.NoError(t, err)
	require.Equal(t, "wss://stream.example.com/v1", c.RedactedWSURL())
}

func TestRPCDelegation_AuthInQuery(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	s.handle("getBlockHeight", func(rpcCall) (any, *rpcErr) { return 288000000, nil })
	s.handle("getLatestBlockhash", func(rpcCall) (any, *rpcErr) {
		return ctxSlot("1", map[string]any{"blockhash": "EkSnNWid2cvwEVnVx9aBqawnmiCNiDgp3gUdkDPTKN1N", "lastValidBlockHeight": 288000150}), nil
	})
	s.handle("isBlockhashValid", func(rpcCall) (any, *rpcErr) { return ctxSlot("1", true), nil })
	s.handle("getSignatureStatuses", func(rpcCall) (any, *rpcErr) {
		return ctxSlot("1", []any{map[string]any{"slot": 250000123, "confirmations": nil, "err": nil, "confirmationStatus": "finalized"}}), nil
	})
	s.handle("getTransaction", func(rpcCall) (any, *rpcErr) { return validTx(sigA), nil })
	s.handle("simulateTransaction", func(rpcCall) (any, *rpcErr) {
		return ctxSlot("1", map[string]any{"err": nil, "logs": []string{}, "unitsConsumed": 10}), nil
	})
	c, d := newClient(t, s, nil)
	ctx := context.Background()

	h, err := c.GetBlockHeight(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(288000000), h)
	require.Equal(t, "api-key="+apiKey, s.recorded("getBlockHeight")[0].Query)
	bh, err := c.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	require.Equal(t, "helius", bh.Source)
	ok, err := c.IsBlockhashValid(ctx, bh.Blockhash)
	require.NoError(t, err)
	require.True(t, ok)
	obs, err := c.GetTransaction(ctx, sigA)
	require.NoError(t, err)
	require.True(t, obs.Found)
	require.Equal(t, chain.CommitmentFinalized, obs.Commitment)
	require.Equal(t, "helius", obs.Source)
	sts, err := c.GetSignatureStatuses(ctx, []string{sigA})
	require.NoError(t, err)
	require.True(t, sts[0].Found)
	sim, err := c.Simulate(ctx, []byte{1}, chain.SimulateOptions{})
	require.NoError(t, err)
	require.True(t, sim.OK)
	require.NotContains(t, d.logs.String(), apiKey, "the key never reaches the logs")
	for _, ref := range d.archive.Refs() {
		obj, _ := d.archive.Get(ref)
		require.Equal(t, "helius", obj.Provider)
	}

	// A refused key is not retried and never rendered.
	bad, _ := newClient(t, s, func(_ *config.ProviderConfig, _ *helius.Options, hd *helius.Deps) {
		hd.Resolver = config.EnvResolver{Lookup: func(string) (string, bool) { return "WRONG", true }}
	})
	_, err = bad.GetBlockHeight(ctx)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, 401, e.Fields["http_status"])
	require.Equal(t, 1, e.Fields["attempts"])
	require.NotContains(t, err.Error(), "WRONG")
}

func TestGetBalances_DAS(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	s.handle("getTokenAccounts", func(call rpcCall) (any, *rpcErr) {
		p := namedParams(t, call)
		require.Equal(t, wallet, p["owner"])
		require.Equal(t, json.Number("1000"), p["limit"])
		require.Equal(t, map[string]any{"showZeroBalance": true}, p["options"])
		switch p["page"] {
		case json.Number("1"):
			accounts := make([]any, 0, helius.DASPageLimit)
			accounts = append(accounts, dasAccount(ataUSDC, mintUSDC, wallet, 1000000), dasAccount(ataBONK, mintBONK, wallet, 123456789))
			for len(accounts) < helius.DASPageLimit {
				accounts = append(accounts, dasAccount(other, "So11111111111111111111111111111111111111112", wallet, 0))
			}
			return dasPage(1001, 1000, 1, accounts...), nil
		case json.Number("2"):
			return dasPage(1001, 1000, 2, dasAccount("3Kx9vLm2pQ7nR4tY6uJ8oP1aS5dF7gH9jK2mZ4xC6vB8", mintUSDC, wallet, 7)), nil
		}
		t.Fatalf("unexpected page %v", p["page"])
		return nil, nil
	})
	s.handle("getBalance", func(rpcCall) (any, *rpcErr) { return ctxSlot("77", 9995000), nil })
	c, _ := newClient(t, s, nil)
	ctx := context.Background()

	bs, err := c.GetBalances(ctx, wallet, []string{mintUSDC, chain.MintNativeSOL})
	require.NoError(t, err)
	require.Len(t, bs, 3, "two USDC accounts across two pages plus native")
	require.Equal(t, mintUSDC, bs[0].Mint)
	require.Equal(t, "7", bs[0].Amount.String())
	require.False(t, bs[0].DecimalsKnown, "DAS reports no decimals")
	require.Equal(t, uint64(0), bs[0].Slot, "DAS reports no slot")
	require.Equal(t, "helius", bs[0].Source)
	require.NotEmpty(t, bs[0].RawRef)
	require.Equal(t, "1000000", bs[1].Amount.String())
	require.Equal(t, chain.MintNativeSOL, bs[2].Mint)
	require.Equal(t, "9995000", bs[2].Amount.String())
	require.True(t, bs[2].DecimalsKnown)
	require.Equal(t, uint64(77), bs[2].Slot)
	require.Len(t, s.recorded("getTokenAccounts"), 2)

	bs, err = c.GetBalances(ctx, wallet, []string{mintBONK})
	require.NoError(t, err)
	require.Len(t, bs, 1)
	require.Equal(t, "123456789", bs[0].Amount.String())
	require.Len(t, s.recorded("getBalance"), 1, "native only when asked")

	bs, err = c.GetBalances(ctx, wallet, nil)
	require.NoError(t, err)
	require.Len(t, bs, 1002, "everything plus native")

	_, err = c.GetBalances(ctx, "bad", nil)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = c.GetBalances(ctx, wallet, []string{"bad"})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// Float amounts and owner mismatches are malformed, never rounded.
	s.handle("getTokenAccounts", func(rpcCall) (any, *rpcErr) {
		return dasPage(1, 1000, 1, dasAccount(ataUSDC, mintUSDC, wallet, 1.5)), nil
	})
	_, err = c.GetBalances(ctx, wallet, nil)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.NotEmpty(t, e.Fields["raw_ref"])
	s.handle("getTokenAccounts", func(rpcCall) (any, *rpcErr) {
		return dasPage(1, 1000, 1, dasAccount(ataUSDC, mintUSDC, other, 1)), nil
	})
	_, err = c.GetBalances(ctx, wallet, nil)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	s.handle("getTokenAccounts", func(rpcCall) (any, *rpcErr) {
		return dasPage(1, 1000, 1, dasAccount(ataUSDC, mintUSDC, wallet, "12345678901234567890123")), nil
	})
	bs, err = c.GetBalances(ctx, wallet, []string{mintUSDC})
	require.NoError(t, err, "integers beyond 2^53 survive as strings or big numbers")
	require.Equal(t, "12345678901234567890123", bs[0].Amount.String())
}

func TestParseDASTokenAccounts_BigNumberExact(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"total":1,"limit":1000,"page":1,"token_accounts":[{"address":"` + ataUSDC + `","mint":"` + mintUSDC + `","owner":"` + wallet + `","amount":18446744073709551617,"delegated_amount":0,"frozen":true}]}`)
	bs, err := helius.ParseDASTokenAccounts(raw, wallet)
	require.NoError(t, err)
	require.Equal(t, "18446744073709551617", bs[0].Amount.String(), "above uint64 and 2^53, still exact")
	_, err = helius.ParseDASTokenAccounts([]byte(`{"token_accounts":[{"address":"a","mint":"m","owner":"`+wallet+`","amount":1e3}]}`), wallet)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = helius.ParseDASTokenAccounts([]byte(`{"token_accounts":[{"address":"","mint":"m","owner":"`+wallet+`","amount":1}]}`), wallet)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = helius.ParseDASTokenAccounts([]byte(`{"token_accounts":[{"address":"a","mint":"m","owner":"`+wallet+`","amount":-1}]}`), wallet)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = helius.ParseDASTokenAccounts([]byte(`[]`), wallet)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestSearchWalletActivity_Delegates(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	quietRPC(s)
	s.handle("getSignaturesForAddress", func(rpcCall) (any, *rpcErr) { return []any{sigItem(sigA, 250000123, 1757073600)}, nil })
	c, _ := newClient(t, s, nil)
	txs, err := c.SearchWalletActivity(context.Background(), wallet, time.Unix(1757073000, 0), 10)
	require.NoError(t, err)
	require.Len(t, txs, 1)
	require.Equal(t, sigA, txs[0].Signature)
	require.True(t, strings.HasPrefix(txs[0].RawRef, "mem://helius/getTransaction/"))
}
