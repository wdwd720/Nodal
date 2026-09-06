package helius

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider/solanarpc"
)

// dasTokenAccounts is the getTokenAccounts result (helius.md, "DAS
// balances"). amount is a JSON integer; token_extensions and decimals are
// not in the schema.
type dasTokenAccounts struct {
	Total         json.Number `json:"total"`
	Limit         json.Number `json:"limit"`
	Page          json.Number `json:"page"`
	Cursor        string      `json:"cursor"`
	TokenAccounts []struct {
		Address         string                 `json:"address"`
		Mint            string                 `json:"mint"`
		Owner           string                 `json:"owner"`
		Amount          solanarpc.ExactInteger `json:"amount"`
		DelegatedAmount json.RawMessage        `json:"delegated_amount"`
		Frozen          bool                   `json:"frozen"`
	} `json:"token_accounts"`
}

// ParseDASTokenAccounts parses one getTokenAccounts page for owner. DAS
// reports neither decimals nor a slot: DecimalsKnown is false and Slot is 0.
func ParseDASTokenAccounts(result []byte, owner string) ([]chain.BalanceObservation, error) {
	var page dasTokenAccounts
	if err := solanarpc.DecodeStrict(result, &page); err != nil {
		return nil, err
	}
	out := make([]chain.BalanceObservation, 0, len(page.TokenAccounts))
	for i, ta := range page.TokenAccounts {
		f := fmt.Sprintf("token_accounts[%d]", i)
		if ta.Address == "" || ta.Mint == "" {
			return nil, solanarpc.Malformed("%s: address and mint are required", f)
		}
		if ta.Owner != owner {
			return nil, solanarpc.Malformed("%s: owner mismatch", f)
		}
		amt, err := ta.Amount.Quantity()
		if err != nil {
			return nil, solanarpc.Malformed("%s.amount: %v", f, err)
		}
		if amt.IsNegative() {
			return nil, solanarpc.Malformed("%s.amount is negative", f)
		}
		out = append(out, chain.BalanceObservation{
			Owner: owner, Mint: ta.Mint, TokenAccount: ta.Address, Amount: amt, DecimalsKnown: false,
		})
	}
	chain.SortBalances(out)
	return out, nil
}

// GetBalances implements chain.ChainObserver through DAS getTokenAccounts
// (paged, showZeroBalance) filtered by mints, plus the native lamport
// balance through RPC getBalance when chain.MintNativeSOL is requested or
// mints is empty. DAS is an indexer: its answer carries no slot and may lag
// the RPC view, which the agreement policy surfaces as a block-dependent
// difference rather than silently accepting either side.
func (c *Client) GetBalances(ctx context.Context, owner string, mints []string) ([]chain.BalanceObservation, error) {
	if err := solanarpc.ValidatePubkey(owner); err != nil {
		return nil, err
	}
	wantNative := len(mints) == 0
	want := map[string]bool{}
	for _, m := range mints {
		if m == chain.MintNativeSOL {
			wantNative = true
			continue
		}
		if err := solanarpc.ValidatePubkey(m); err != nil {
			return nil, errs.New(errs.CodeValidationFailed, "invalid mint")
		}
		want[m] = true
	}
	var out []chain.BalanceObservation
	for page := 1; page <= maxDASPages; page++ {
		params := map[string]any{
			"owner":   owner,
			"page":    page,
			"limit":   DASPageLimit,
			"options": map[string]any{"showZeroBalance": true},
		}
		res, err := c.rpc.Call(ctx, "getTokenAccounts", params, owner)
		if err != nil {
			return nil, err
		}
		items, err := ParseDASTokenAccounts(res.Raw, owner)
		if err != nil {
			if e, ok := errs.As(err); ok {
				return nil, e.WithField("raw_ref", res.RawRef)
			}
			return nil, err
		}
		for _, b := range items {
			if len(want) > 0 && !want[b.Mint] {
				continue
			}
			b.Source, b.RawRef, b.ObservedAt, b.ReceivedAt = c.name, res.RawRef, res.ObservedAt, res.ReceivedAt
			out = append(out, b)
		}
		if len(items) < DASPageLimit {
			break
		}
	}
	if wantNative {
		native, err := c.rpc.GetBalances(ctx, owner, []string{chain.MintNativeSOL})
		if err != nil {
			return nil, err
		}
		out = append(out, native...)
	}
	chain.SortBalances(out)
	return out, nil
}
