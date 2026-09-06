package ledger

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
)

// TestCanonicalContent_Golden pins the exact canonical form the proof system
// will chain: sorted keys, entries sorted by their own canonical form, no
// HTML escaping, RFC3339Nano UTC, null reversal_of.
func TestCanonicalContent_Golden(t *testing.T) {
	t.Parallel()
	acct, err := accounts.ParseAccountID("01920000-0000-7000-8000-000000000001")
	require.NoError(t, err)
	asset, err := assets.ParseAssetID("01920000-0000-7000-8000-0000000000aa")
	require.NoError(t, err)
	p := Posting{
		Kind:           KindFundingSettled,
		IdempotencyKey: "deposit:d1:settled",
		Reference:      FinancialEventReference{Type: "deposit", ID: "d1"},
		EffectiveAt:    time.Date(2026, 9, 5, 12, 0, 0, 1, time.FixedZone("plus2", 2*3600)),
		Description:    "ignored by the hash",
		CorrelationID:  "ignored too",
		Entries: []Entry{
			{Account: CustomerAccount(acct, CodeWallet, asset), Side: Debit, Quantity: q(100)},
			{Account: CustomerAccount(acct, CodeCapital, asset), Side: Credit, Quantity: q(100)},
		},
		Metadata: map[string]any{"z": 1, "a": "<&>"},
	}
	const wantEntryCapital = `{"account":{"asset_id":"01920000-0000-7000-8000-0000000000aa","code":"CAPITAL","owner_id":"01920000-0000-7000-8000-000000000001","owner_type":"CUSTOMER"},"quantity":"100","side":"CREDIT"}`
	const wantEntryWallet = `{"account":{"asset_id":"01920000-0000-7000-8000-0000000000aa","code":"WALLET","owner_id":"01920000-0000-7000-8000-000000000001","owner_type":"CUSTOMER"},"quantity":"100","side":"DEBIT"}`
	want := `{"effective_at":"2026-09-05T10:00:00.000000001Z","entries":[` + wantEntryCapital + `,` + wantEntryWallet + `],` +
		`"idempotency_key":"deposit:d1:settled","kind":"FUNDING_SETTLED","metadata":{"a":"<&>","z":1},` +
		`"reference":{"id":"d1","type":"deposit"},"reversal_of":null}`

	got, err := CanonicalContent(p)
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
	assert.True(t, json.Valid(got))

	hash, err := ContentHash(p)
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(want))
	assert.Equal(t, sum[:], hash)
	assert.Len(t, hash, sha256.Size)
}

func TestContentHash_StableUnderReorder(t *testing.T) {
	t.Parallel()
	base := fundingPosting(100)
	base.Entries = append(base.Entries,
		Entry{Account: cust(CodeWallet, testSOL), Side: Debit, Quantity: q(5)},
		Entry{Account: cust(CodeTradingInflow, testSOL), Side: Credit, Quantity: q(5)})
	base.Metadata = map[string]any{"b": []any{1, "x"}, "a": map[string]any{"y": true, "x": nil}}
	want, err := ContentHash(base)
	require.NoError(t, err)

	reordered := base
	reordered.Entries = []Entry{base.Entries[3], base.Entries[1], base.Entries[2], base.Entries[0]}
	reordered.Metadata = map[string]any{"a": map[string]any{"x": nil, "y": true}, "b": []any{1, "x"}}
	reordered.Description = "different description"
	reordered.CorrelationID = "different correlation"
	usd := int64(12345)
	ref := "price:1"
	reordered.Entries[0].USDValueMinor = &usd
	reordered.Entries[0].PriceRef = &ref
	got, err := ContentHash(reordered)
	require.NoError(t, err)
	assert.Equal(t, want, got, "entry order, metadata key order, description, correlation and valuation must not change the hash")

	nilMeta := fundingPosting(100)
	emptyMeta := fundingPosting(100)
	emptyMeta.Metadata = map[string]any{}
	h1, err := ContentHash(nilMeta)
	require.NoError(t, err)
	h2, err := ContentHash(emptyMeta)
	require.NoError(t, err)
	assert.Equal(t, h1, h2, "nil and empty metadata hash identically")
}

func TestContentHash_SensitiveToContent(t *testing.T) {
	t.Parallel()
	base := fundingPosting(100)
	want, err := ContentHash(base)
	require.NoError(t, err)
	reversal := NewTransactionID()
	variants := map[string]func(p *Posting){
		"quantity":     func(p *Posting) { p.Entries[0].Quantity = q(101); p.Entries[1].Quantity = q(101) },
		"side":         func(p *Posting) { p.Entries[0].Side, p.Entries[1].Side = Credit, Debit },
		"asset":        func(p *Posting) { p.Entries[0].Account.AssetID = testSOL; p.Entries[1].Account.AssetID = testSOL },
		"code":         func(p *Posting) { p.Entries[1].Account.Code = CodeTradingInflow },
		"kind":         func(p *Posting) { p.Kind = KindSeed },
		"key":          func(p *Posting) { p.IdempotencyKey = "deposit:d2:settled" },
		"reference":    func(p *Posting) { p.Reference.ID = "d2" },
		"effective_at": func(p *Posting) { p.EffectiveAt = p.EffectiveAt.Add(time.Nanosecond) },
		"reversal_of":  func(p *Posting) { p.ReversalOf = &reversal },
		"metadata":     func(p *Posting) { p.Metadata = map[string]any{"k": "v"} },
	}
	for name, f := range variants {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fundingPosting(100)
			p.Entries = append([]Entry(nil), p.Entries...)
			f(&p)
			got, err := ContentHash(p)
			require.NoError(t, err)
			assert.NotEqual(t, want, got)
		})
	}
}

func TestCanonicalJSON(t *testing.T) {
	t.Parallel()
	got, err := canonicalJSON(map[string]any{
		"z": []any{map[string]any{"b": json.Number("1e3"), "a": "<tag>&'\""}, nil, false},
		"a": map[string]any{"k": "v"},
	})
	require.NoError(t, err)
	assert.Equal(t, `{"a":{"k":"v"},"z":[{"a":"<tag>&'\"","b":1e3},null,false]}`, string(got))

	got, err = canonicalJSON(struct {
		B int    `json:"b"`
		A string `json:"a"`
	}{B: 2, A: "x"})
	require.NoError(t, err)
	assert.Equal(t, `{"a":"x","b":2}`, string(got))

	got, err = canonicalJSON(q(-42))
	require.NoError(t, err)
	assert.Equal(t, `"-42"`, string(got), "quantities are strings, never JSON numbers")
}

func TestCanonicalContent_RejectsUnencodableMetadata(t *testing.T) {
	t.Parallel()
	p := fundingPosting(100)
	p.Metadata = map[string]any{"ch": make(chan int)}
	_, err := CanonicalContent(p)
	requireCode(t, err, errs.CodeValidationFailed)
	_, err = ContentHash(p)
	requireCode(t, err, errs.CodeValidationFailed)
}
