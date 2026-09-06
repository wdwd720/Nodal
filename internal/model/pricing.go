package model

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// tokensPerMillion is the denominator of every rate in the table.
const tokensPerMillion = 1_000_000

// Price is the per-million-token rate of one model, in USD minor units
// (cents). Integers, never floats: $5.00 per MTok is 500.
type Price struct {
	InputCentsPerMTok      int64
	OutputCentsPerMTok     int64
	CacheReadCentsPerMTok  int64
	CacheWriteCentsPerMTok int64
}

// PriceTable maps a provider model identifier to its rate. It is
// configuration: the values below are the published list prices at the time
// the provider document was verified, and a deployment overrides them
// rather than trusting a hardcoded number to stay current.
type PriceTable map[string]Price

// DefaultPriceTable is the published USD list pricing (docs/api/providers/
// anthropic.md, fetched 2026-09-05), expressed in cents per million tokens.
// Rates are per the standard tier; batch and long-context multipliers are
// applied by the caller if it uses them.
func DefaultPriceTable() PriceTable {
	return PriceTable{
		"claude-opus-5":             {InputCentsPerMTok: 500, OutputCentsPerMTok: 2500, CacheReadCentsPerMTok: 50, CacheWriteCentsPerMTok: 625},
		"claude-sonnet-5":           {InputCentsPerMTok: 200, OutputCentsPerMTok: 1000, CacheReadCentsPerMTok: 20, CacheWriteCentsPerMTok: 250},
		"claude-fable-5-1":          {InputCentsPerMTok: 1000, OutputCentsPerMTok: 5000, CacheReadCentsPerMTok: 25, CacheWriteCentsPerMTok: 1250},
		"claude-haiku-4-5":          {InputCentsPerMTok: 100, OutputCentsPerMTok: 500, CacheReadCentsPerMTok: 10, CacheWriteCentsPerMTok: 125},
		"claude-haiku-4-5-20251001": {InputCentsPerMTok: 100, OutputCentsPerMTok: 500, CacheReadCentsPerMTok: 10, CacheWriteCentsPerMTok: 125},
	}
}

// Models returns the priced model identifiers in sorted order.
func (t PriceTable) Models() []string {
	out := make([]string, 0, len(t))
	for k := range t {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Cost prices usage exactly. Rounding is away from zero (RoundUp) so a
// fractional cent is always charged to us rather than to the budget's
// headroom: a budget must never be overspent because of rounding.
//
// An unpriced model is an error, not a zero. A model whose cost we cannot
// state is a model we cannot bill or budget for, and silently treating it
// as free would defeat both.
func (t PriceTable) Cost(modelID string, u Usage) (money.USD, error) {
	p, ok := t[modelID]
	if !ok {
		return money.USD{}, errs.Newf(errs.CodeValidationFailed, "model: no price configured for %q", modelID).
			WithField("model_id", modelID).
			WithField("priced_models", t.Models())
	}
	for _, n := range []struct {
		name string
		v    int64
	}{
		{"input_tokens", u.InputTokens},
		{"output_tokens", u.OutputTokens},
		{"cache_read_input_tokens", u.CacheReadInputTokens},
		{"cache_creation_input_tokens", u.CacheCreationInputTokens},
	} {
		if n.v < 0 {
			return money.USD{}, errs.Newf(errs.CodeValidationFailed, "model: negative %s", n.name)
		}
	}

	// Sum (tokens × centsPerMTok) in big.Int, then divide once by 10^6 with
	// an explicit rounding mode, so no intermediate step loses a fraction.
	total := new(big.Int)
	for _, term := range []struct{ tokens, rate int64 }{
		{u.InputTokens, p.InputCentsPerMTok},
		{u.OutputTokens, p.OutputCentsPerMTok},
		{u.CacheReadInputTokens, p.CacheReadCentsPerMTok},
		{u.CacheCreationInputTokens, p.CacheWriteCentsPerMTok},
	} {
		term := new(big.Int).Mul(big.NewInt(term.tokens), big.NewInt(term.rate))
		total.Add(total, term)
	}
	cents := divRoundUp(total, big.NewInt(tokensPerMillion))
	if !cents.IsInt64() {
		return money.USD{}, errs.New(errs.CodeOverflow, "model: cost exceeds the representable range")
	}
	return money.USDFromMinor(cents.Int64()), nil
}

// EstimateCost prices a call before it happens, from the request's output
// cap and an input token estimate. It is deliberately pessimistic: it
// assumes the model emits every token it is allowed to, so a budget check
// cannot be passed by a call that then overspends.
func (t PriceTable) EstimateCost(modelID string, estimatedInputTokens, maxOutputTokens int64) (money.USD, error) {
	return t.Cost(modelID, Usage{InputTokens: estimatedInputTokens, OutputTokens: maxOutputTokens})
}

// EstimateInputTokens approximates the token count of a rendered prompt.
// Four bytes per token is the conventional English approximation; it is
// used only to pre-check a budget, never to bill, and it rounds up.
func EstimateInputTokens(rendered string) int64 {
	if rendered == "" {
		return 0
	}
	return int64((len(rendered) + 3) / 4)
}

// divRoundUp divides num by den, rounding away from zero.
func divRoundUp(num, den *big.Int) *big.Int {
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() == 0 {
		return quo
	}
	if (num.Sign() < 0) != (den.Sign() < 0) {
		return quo.Sub(quo, big.NewInt(1))
	}
	return quo.Add(quo, big.NewInt(1))
}

// Validate checks that every rate is non-negative and that the table is not
// empty. A deployment with an empty table cannot make a priced call.
func (t PriceTable) Validate() error {
	if len(t) == 0 {
		return errs.New(errs.CodeValidationFailed, "model: price table is empty")
	}
	for _, id := range t.Models() {
		p := t[id]
		for _, r := range []struct {
			name string
			v    int64
		}{
			{"input", p.InputCentsPerMTok},
			{"output", p.OutputCentsPerMTok},
			{"cache_read", p.CacheReadCentsPerMTok},
			{"cache_write", p.CacheWriteCentsPerMTok},
		} {
			if r.v < 0 {
				return errs.New(errs.CodeValidationFailed, fmt.Sprintf("model: negative %s rate for %q", r.name, id))
			}
		}
	}
	return nil
}
