package ledger

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// TestValidatePosting is the application-level row of the invariant matrix
// (FINANCIAL_MODEL §8): every structural rule and the per-asset balance.
func TestValidatePosting(t *testing.T) {
	t.Parallel()
	reversal := NewTransactionID()
	zeroID := TransactionID{}
	mutate := func(f func(p *Posting)) Posting {
		p := fundingPosting(100)
		f(&p)
		return p
	}
	cases := []struct {
		name string
		p    Posting
		code errs.Code // "" means valid
	}{
		{"valid funding", fundingPosting(100), ""},
		{"unbalanced", mutate(func(p *Posting) { p.Entries[1].Quantity = q(99) }), errs.CodeLedgerUnbalanced},
		{"single entry", mutate(func(p *Posting) { p.Entries = p.Entries[:1] }), errs.CodeLedgerUnbalanced},
		{"no entries", mutate(func(p *Posting) { p.Entries = nil }), errs.CodeLedgerUnbalanced},
		{"zero quantity", mutate(func(p *Posting) { p.Entries[0].Quantity = q(0); p.Entries[1].Quantity = q(0) }), errs.CodeValidationFailed},
		{"negative quantity", mutate(func(p *Posting) { p.Entries[0].Quantity = q(-100); p.Entries[1].Quantity = q(-100) }), errs.CodeValidationFailed},
		{"bad kind", mutate(func(p *Posting) { p.Kind = "BONUS" }), errs.CodeValidationFailed},
		{"unknown code", mutate(func(p *Posting) { p.Entries[0].Account.Code = "PIGGY_BANK" }), errs.CodeValidationFailed},
		{"bad side", mutate(func(p *Posting) { p.Entries[0].Side = "LEFT" }), errs.CodeValidationFailed},
		{"empty idempotency key", mutate(func(p *Posting) { p.IdempotencyKey = "" }), errs.CodeValidationFailed},
		{"whitespace idempotency key", mutate(func(p *Posting) { p.IdempotencyKey = " deposit:d1 " }), errs.CodeValidationFailed},
		{"oversized idempotency key", mutate(func(p *Posting) { p.IdempotencyKey = strings.Repeat("k", MaxIdempotencyKeyLen+1) }), errs.CodeValidationFailed},
		{"missing reference type", mutate(func(p *Posting) { p.Reference.Type = "" }), errs.CodeValidationFailed},
		{"missing reference id", mutate(func(p *Posting) { p.Reference.ID = "" }), errs.CodeValidationFailed},
		{"zero effective_at", mutate(func(p *Posting) { p.EffectiveAt = time.Time{} }), errs.CodeValidationFailed},
		{"platform owner id mismatch", mutate(func(p *Posting) {
			p.Entries[1].Account = AccountRef{OwnerPlatform, testAccount.String(), CodePlatformFeeRevenue, testUSDC}
		}), errs.CodeValidationFailed},
		{"customer code on platform owner", mutate(func(p *Posting) {
			p.Entries[1].Account = AccountRef{OwnerPlatform, PlatformOwnerID, CodeCapital, testUSDC}
		}), errs.CodeValidationFailed},
		{"cross-asset never balances", mutate(func(p *Posting) { p.Entries[1].Account.AssetID = testSOL }), errs.CodeLedgerUnbalanced},
		{"multi-asset balanced", mutate(func(p *Posting) {
			p.Entries = append(p.Entries,
				Entry{Account: cust(CodeWallet, testSOL), Side: Debit, Quantity: q(5)},
				Entry{Account: cust(CodeTradingInflow, testSOL), Side: Credit, Quantity: q(3)},
				Entry{Account: cust(CodeTradingInflow, testSOL), Side: Credit, Quantity: q(2)})
		}), ""},
		{"multi-asset one leg off", mutate(func(p *Posting) {
			p.Entries = append(p.Entries,
				Entry{Account: cust(CodeWallet, testSOL), Side: Debit, Quantity: q(5)},
				Entry{Account: cust(CodeTradingInflow, testSOL), Side: Credit, Quantity: q(4)})
		}), errs.CodeLedgerUnbalanced},
		{"compensation without reversal_of", mutate(func(p *Posting) {
			p.Kind = KindCompensation
			p.Metadata = map[string]any{MetadataReasonCode: "DUPLICATE_FILL"}
		}), errs.CodeValidationFailed},
		{"compensation without reason code", mutate(func(p *Posting) {
			p.Kind = KindCompensation
			p.ReversalOf = &reversal
		}), errs.CodeValidationFailed},
		{"compensation complete", mutate(func(p *Posting) {
			p.Kind = KindCompensation
			p.ReversalOf = &reversal
			p.Metadata = map[string]any{MetadataReasonCode: "DUPLICATE_FILL"}
		}), ""},
		{"correction complete", mutate(func(p *Posting) {
			p.Kind = KindCorrection
			p.ReversalOf = &reversal
			p.Metadata = map[string]any{MetadataReasonCode: "OPERATOR"}
		}), ""},
		{"reconciliation adjustment needs reason", mutate(func(p *Posting) { p.Kind = KindReconciliationAdjustment }), errs.CodeValidationFailed},
		{"reconciliation adjustment with reason", mutate(func(p *Posting) {
			p.Kind = KindReconciliationAdjustment
			p.Metadata = map[string]any{MetadataReasonCode: "AIRDROP"}
		}), ""},
		{"reversal_of zero", mutate(func(p *Posting) { p.ReversalOf = &zeroID }), errs.CodeValidationFailed},
		{"too many entries", mutate(func(p *Posting) {
			for len(p.Entries) <= MaxEntries {
				p.Entries = append(p.Entries,
					Entry{Account: cust(CodeWallet, testUSDC), Side: Debit, Quantity: q(1)},
					Entry{Account: cust(CodeCapital, testUSDC), Side: Credit, Quantity: q(1)})
			}
		}), errs.CodeValidationFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.p.Validate()
			if c.code == "" {
				require.NoError(t, err)
				return
			}
			requireCode(t, err, c.code)
		})
	}
}

func TestValidatePosting_UnbalancedReportsAsset(t *testing.T) {
	t.Parallel()
	p := fundingPosting(100)
	p.Entries[1].Quantity = q(90)
	err := p.Validate()
	e, ok := errs.As(err)
	require.True(t, ok)
	require.Equal(t, errs.CodeLedgerUnbalanced, e.Code)
	require.Equal(t, testUSDC.String(), e.Fields["asset_id"])
	require.Equal(t, "100", e.Fields["debits"])
	require.Equal(t, "90", e.Fields["credits"])
}

func TestAssetImbalances(t *testing.T) {
	t.Parallel()
	entries := []Entry{
		{Account: cust(CodeWallet, testUSDC), Side: Debit, Quantity: q(7)},
		{Account: cust(CodeWallet, testSOL), Side: Credit, Quantity: q(2)},
		{Account: cust(CodeCapital, testUSDC), Side: Credit, Quantity: q(3)},
	}
	got := assetImbalances(entries)
	require.Len(t, got, 2)
	require.Equal(t, testUSDC, got[0].asset)
	require.Equal(t, "7", got[0].debits.String())
	require.Equal(t, "3", got[0].credits.String())
	require.Equal(t, "4", got[0].net.String())
	require.Equal(t, testSOL, got[1].asset)
	require.Equal(t, "-2", got[1].net.String())
}
