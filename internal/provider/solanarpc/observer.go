package solanarpc

import (
	"context"
	"encoding/base64"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/errs"
)

// GetTransaction implements chain.ChainObserver at the client's commitment.
func (c *Client) GetTransaction(ctx context.Context, sig string) (chain.TxObservation, error) {
	return c.GetTransactionAt(ctx, sig, c.commitment)
}

// GetTransactionAt reads sig at an explicit commitment (confirmed or
// finalized) and upgrades the reported commitment through
// getSignatureStatuses when the status is stronger. A status read failure
// keeps the (weaker) read commitment; it never fails the observation.
func (c *Client) GetTransactionAt(ctx context.Context, sig string, commitment chain.Commitment) (chain.TxObservation, error) {
	if err := ValidateSignature(sig); err != nil {
		return chain.TxObservation{}, err
	}
	if commitment != chain.CommitmentConfirmed && commitment != chain.CommitmentFinalized {
		return chain.TxObservation{}, errs.New(errs.CodeValidationFailed, "getTransaction accepts confirmed or finalized only")
	}
	params := []any{sig, map[string]any{
		"commitment":                     string(commitment),
		"maxSupportedTransactionVersion": 0,
		"encoding":                       "json",
	}}
	res, err := c.Call(ctx, "getTransaction", params, sig)
	if err != nil {
		return chain.TxObservation{}, err
	}
	obs, err := ParseTransaction(res.Raw, sig, commitment, c.stamp(res))
	if err != nil {
		if e, ok := errs.As(err); ok {
			return chain.TxObservation{}, e.WithField("raw_ref", res.RawRef)
		}
		return chain.TxObservation{}, err
	}
	if !obs.Found {
		return obs, nil
	}
	sts, serr := c.GetSignatureStatuses(ctx, []string{sig})
	switch {
	case serr != nil:
		c.log.WarnContext(ctx, "signature status unavailable; keeping read commitment",
			slog.String("code", string(errs.CodeOf(serr))), slog.String("commitment", string(obs.Commitment)))
	case sts[0].Found && sts[0].Slot == obs.Slot && sts[0].Commitment.Rank() > obs.Commitment.Rank():
		obs.Commitment = sts[0].Commitment
	}
	obs.ReceivedAt = c.clk.Now()
	return obs, nil
}

func (c *Client) stamp(res CallResult) Stamp {
	return Stamp{Source: c.name, RawRef: res.RawRef, ObservedAt: res.ObservedAt, ReceivedAt: res.ReceivedAt}
}

// GetSignatureStatuses implements chain.ChainObserver (searches history).
func (c *Client) GetSignatureStatuses(ctx context.Context, sigs []string) ([]chain.SignatureStatus, error) {
	if len(sigs) == 0 || len(sigs) > MaxSignatureStatuses {
		return nil, errs.Newf(errs.CodeValidationFailed, "getSignatureStatuses takes 1..%d signatures", MaxSignatureStatuses)
	}
	for _, s := range sigs {
		if err := ValidateSignature(s); err != nil {
			return nil, err
		}
	}
	res, err := c.Call(ctx, "getSignatureStatuses", []any{sigs, map[string]any{"searchTransactionHistory": true}}, sigs[0])
	if err != nil {
		return nil, err
	}
	return ParseSignatureStatuses(res.Raw, sigs)
}

// GetBalances implements chain.ChainObserver: one observation per token
// account (getTokenAccountsByOwner, jsonParsed) for each mint, plus the
// native balance (getBalance) for chain.MintNativeSOL; every token account
// under both token programs when mints is empty.
func (c *Client) GetBalances(ctx context.Context, owner string, mints []string) ([]chain.BalanceObservation, error) {
	if err := ValidatePubkey(owner); err != nil {
		return nil, err
	}
	wantNative := len(mints) == 0
	seen := map[string]bool{}
	var tokenMints []string
	for _, m := range mints {
		if m == chain.MintNativeSOL {
			wantNative = true
			continue
		}
		if err := ValidatePubkey(m); err != nil {
			return nil, errs.New(errs.CodeValidationFailed, "invalid mint")
		}
		if !seen[m] {
			seen[m] = true
			tokenMints = append(tokenMints, m)
		}
	}
	var out []chain.BalanceObservation
	if len(mints) == 0 {
		all, err := c.TokenAccounts(ctx, owner)
		if err != nil {
			return nil, err
		}
		out = append(out, all...)
	}
	for _, m := range tokenMints {
		bs, err := c.tokenAccounts(ctx, owner, map[string]any{"mint": m})
		if err != nil {
			return nil, err
		}
		out = append(out, bs...)
	}
	if wantNative {
		res, err := c.Call(ctx, "getBalance", []any{owner, map[string]any{"commitment": string(c.commitment)}}, owner)
		if err != nil {
			return nil, err
		}
		lamports, slot, err := ParseU64Value(res.Raw)
		if err != nil {
			return nil, err
		}
		amt, err := parseLamports(jsonNumber(lamports), "value")
		if err != nil {
			return nil, err
		}
		out = append(out, chain.BalanceObservation{
			Owner: owner, Mint: chain.MintNativeSOL, TokenAccount: owner, Amount: amt,
			Decimals: chain.NativeDecimals, DecimalsKnown: true, Slot: slot,
			Source: c.name, RawRef: res.RawRef, ObservedAt: res.ObservedAt, ReceivedAt: res.ReceivedAt,
		})
	}
	chain.SortBalances(out)
	return out, nil
}

// TokenAccounts returns every token account of owner under the Token and
// Token-2022 programs.
func (c *Client) TokenAccounts(ctx context.Context, owner string) ([]chain.BalanceObservation, error) {
	if err := ValidatePubkey(owner); err != nil {
		return nil, err
	}
	var out []chain.BalanceObservation
	for _, pid := range []string{TokenProgramID, Token2022ProgramID} {
		bs, err := c.tokenAccounts(ctx, owner, map[string]any{"programId": pid})
		if err != nil {
			return nil, err
		}
		out = append(out, bs...)
	}
	chain.SortBalances(out)
	return out, nil
}

func (c *Client) tokenAccounts(ctx context.Context, owner string, filter map[string]any) ([]chain.BalanceObservation, error) {
	params := []any{owner, filter, map[string]any{"commitment": string(c.commitment), "encoding": "jsonParsed"}}
	res, err := c.Call(ctx, "getTokenAccountsByOwner", params, owner)
	if err != nil {
		return nil, err
	}
	bs, _, err := ParseTokenAccounts(res.Raw, owner)
	if err != nil {
		return nil, err
	}
	for i := range bs {
		bs[i].Source, bs[i].RawRef = c.name, res.RawRef
		bs[i].ObservedAt, bs[i].ReceivedAt = res.ObservedAt, res.ReceivedAt
	}
	return bs, nil
}

// GetBlockHeight implements chain.ChainObserver.
func (c *Client) GetBlockHeight(ctx context.Context) (uint64, error) {
	res, err := c.Call(ctx, "getBlockHeight", []any{map[string]any{"commitment": string(c.commitment)}}, "")
	if err != nil {
		return 0, err
	}
	h, _, err := ParseU64Value(res.Raw)
	return h, err
}

// GetLatestBlockhash implements chain.ChainObserver.
func (c *Client) GetLatestBlockhash(ctx context.Context) (chain.Blockhash, error) {
	res, err := c.Call(ctx, "getLatestBlockhash", []any{map[string]any{"commitment": string(c.commitment)}}, "")
	if err != nil {
		return chain.Blockhash{}, err
	}
	b, err := ParseBlockhash(res.Raw)
	if err != nil {
		return chain.Blockhash{}, err
	}
	b.Source, b.ObservedAt, b.ReceivedAt = c.name, res.ObservedAt, res.ReceivedAt
	return b, nil
}

// IsBlockhashValid implements chain.ChainObserver.
func (c *Client) IsBlockhashValid(ctx context.Context, blockhash string) (bool, error) {
	if err := ValidateBlockhash(blockhash); err != nil {
		return false, err
	}
	res, err := c.Call(ctx, "isBlockhashValid", []any{blockhash, map[string]any{"commitment": string(c.commitment)}}, blockhash)
	if err != nil {
		return false, err
	}
	ok, _, err := ParseBoolValue(res.Raw)
	return ok, err
}

// Simulate implements chain.ChainObserver (simulateTransaction, base64).
func (c *Client) Simulate(ctx context.Context, rawTx []byte, opts chain.SimulateOptions) (chain.SimulationResult, error) {
	if len(rawTx) == 0 {
		return chain.SimulationResult{}, errs.New(errs.CodeValidationFailed, "empty transaction")
	}
	if opts.SigVerify && opts.ReplaceRecentBlockhash {
		return chain.SimulationResult{}, errs.New(errs.CodeValidationFailed, "sigVerify conflicts with replaceRecentBlockhash")
	}
	commitment := opts.Commitment
	if commitment == "" {
		commitment = c.commitment
	}
	if !commitment.Valid() {
		return chain.SimulationResult{}, errs.New(errs.CodeValidationFailed, "invalid commitment")
	}
	cfg := map[string]any{
		"encoding":               "base64",
		"commitment":             string(commitment),
		"sigVerify":              opts.SigVerify,
		"replaceRecentBlockhash": opts.ReplaceRecentBlockhash,
		"innerInstructions":      opts.InnerInstructions,
	}
	if len(opts.ReturnAccounts) > 0 {
		for _, a := range opts.ReturnAccounts {
			if err := ValidatePubkey(a); err != nil {
				return chain.SimulationResult{}, errs.New(errs.CodeValidationFailed, "invalid account in ReturnAccounts")
			}
		}
		cfg["accounts"] = map[string]any{"addresses": opts.ReturnAccounts, "encoding": "jsonParsed"}
	}
	res, err := c.Call(ctx, "simulateTransaction", []any{base64.StdEncoding.EncodeToString(rawTx), cfg}, "")
	if err != nil {
		return chain.SimulationResult{}, err
	}
	sim, err := ParseSimulation(res.Raw, opts)
	if err != nil {
		return chain.SimulationResult{}, err
	}
	sim.Source, sim.RawRef, sim.ObservedAt, sim.ReceivedAt = c.name, res.RawRef, res.ObservedAt, res.ReceivedAt
	return sim, nil
}

// SearchWalletActivity implements chain.ChainObserver: signatures for the
// wallet address and each of its token accounts (getSignaturesForAddress,
// paged with `before`), merged newest first, then observed one by one
// through GetTransaction. `since` is compared against the chain's blockTime
// (untrusted clock): it only bounds how far back paging goes.
func (c *Client) SearchWalletActivity(ctx context.Context, wallet string, since time.Time, limit int) ([]chain.TxObservation, error) {
	if err := ValidatePubkey(wallet); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > c.maxTx {
		limit = c.maxTx
	}
	addresses := []string{wallet}
	accounts, err := c.TokenAccounts(ctx, wallet)
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		addresses = append(addresses, a.TokenAccount)
	}
	infos := map[string]SignatureInfo{}
	for _, addr := range addresses {
		if err := c.signaturesSince(ctx, addr, since, limit, infos); err != nil {
			return nil, err
		}
	}
	ordered := make([]SignatureInfo, 0, len(infos))
	for _, si := range infos {
		ordered = append(ordered, si)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Slot != ordered[j].Slot {
			return ordered[i].Slot > ordered[j].Slot
		}
		return ordered[i].Signature < ordered[j].Signature
	})
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	results := make([]chain.TxObservation, len(ordered))
	errsCh := make(chan error, len(ordered))
	sem := make(chan struct{}, c.conc)
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i, si := range ordered {
		wg.Add(1)
		go func(i int, sig string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-fctx.Done():
				return
			}
			defer func() { <-sem }()
			obs, err := c.GetTransaction(fctx, sig)
			if err != nil {
				errsCh <- err
				cancel()
				return
			}
			results[i] = obs
		}(i, si.Signature)
	}
	wg.Wait()
	close(errsCh)
	if err := <-errsCh; err != nil {
		return nil, err
	}
	out := make([]chain.TxObservation, 0, len(results))
	for _, obs := range results {
		if obs.Found {
			out = append(out, obs)
		}
	}
	return out, nil
}

// signaturesSince pages getSignaturesForAddress backwards until an item
// older than since, an exhausted page, the limit, or maxActivityPages.
func (c *Client) signaturesSince(ctx context.Context, addr string, since time.Time, limit int, into map[string]SignatureInfo) error {
	pageLimit := limit
	if pageLimit > c.pageSize {
		pageLimit = c.pageSize
	}
	before := ""
	collected := 0
	for page := 0; page < maxActivityPages && collected < limit; page++ {
		cfg := map[string]any{"limit": pageLimit, "commitment": string(c.commitment)}
		if before != "" {
			cfg["before"] = before
		}
		res, err := c.Call(ctx, "getSignaturesForAddress", []any{addr, cfg}, addr)
		if err != nil {
			return err
		}
		items, err := ParseSignaturesForAddress(res.Raw)
		if err != nil {
			return err
		}
		for _, it := range items {
			if it.BlockTime != nil && it.BlockTime.Before(since) {
				return nil
			}
			if _, dup := into[it.Signature]; !dup {
				into[it.Signature] = it
				collected++
			}
			before = it.Signature
		}
		if len(items) < pageLimit {
			return nil
		}
	}
	return nil
}
