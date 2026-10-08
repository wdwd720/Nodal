package delegation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// perParty sums debits minus credits per (owner, asset): every
// simulated-only posting must net to zero for every party (SM002).
func perParty(p ledger.Posting) map[string]money.Quantity {
	out := map[string]money.Quantity{}
	for _, e := range p.Entries {
		k := string(e.Account.OwnerType) + "/" + e.Account.OwnerID + "/" + e.Account.AssetID.String()
		q := e.Quantity
		if e.Side == ledger.Credit {
			q = q.Neg()
		}
		out[k] = out[k].Add(q)
	}
	return out
}

func TestTheSwapPostingIsTheSimTradeTemplateBalancedPerParty(t *testing.T) {
	sw := Swap{ID: id.New[swapKind](), AccountID: accounts.AccountID(id.New[id.Any]()),
		PayAssetID: assets.AssetID(id.New[id.Any]()), ReceiveAssetID: assets.AssetID(id.New[id.Any]()),
		AmountIn: money.QuantityFromInt64(2_000_000), AmountOut: money.QuantityFromInt64(19_900_000)}
	house := econ.HouseAccountID(id.New[id.Any]())
	p := SwapPosting(sw, house, unitNow)
	assert.Equal(t, ledger.KindSimTrade, p.Kind)
	assert.True(t, p.Kind.IsSimulatedOnly())
	require.Len(t, p.Entries, 8)
	for party, net := range perParty(p) {
		assert.True(t, net.IsZero(), "%s nets %s", party, net)
	}
	assert.Equal(t, ReferenceSwap, p.Reference.Type)
	assert.Equal(t, sw.ID.String(), p.Reference.ID)
	// The customer's WALLET loses exactly the pay amount and gains exactly
	// the receive amount.
	for _, e := range p.Entries {
		if e.Account.OwnerType == ledger.OwnerCustomer && e.Account.Code == ledger.CodeWallet {
			if e.Account.AssetID == sw.PayAssetID {
				assert.Equal(t, ledger.Credit, e.Side)
				assert.True(t, e.Quantity.Equal(sw.AmountIn))
			} else {
				assert.Equal(t, ledger.Debit, e.Side)
				assert.True(t, e.Quantity.Equal(sw.AmountOut))
			}
		}
	}
}

func TestTheWithdrawalPostingIsSandboxWithdrawal(t *testing.T) {
	w := Withdrawal{ID: id.New[withdrawalKind](), AccountID: accounts.AccountID(id.New[id.Any]()),
		AssetID: assets.AssetID(id.New[id.Any]()), Amount: money.QuantityFromInt64(5_000_000)}
	p := WithdrawalPosting(w, unitNow)
	assert.Equal(t, ledger.KindSandboxWithdrawal, p.Kind)
	assert.True(t, p.Kind.IsSimulatedOnly())
	require.Len(t, p.Entries, 2)
	assert.Equal(t, ledger.CodeWithdrawn, p.Entries[0].Account.Code)
	assert.Equal(t, ledger.Debit, p.Entries[0].Side)
	assert.Equal(t, ledger.CodeWallet, p.Entries[1].Account.Code)
	assert.Equal(t, ledger.Credit, p.Entries[1].Side)
	for party, net := range perParty(p) {
		assert.True(t, net.IsZero(), "%s nets %s", party, net)
	}
}
