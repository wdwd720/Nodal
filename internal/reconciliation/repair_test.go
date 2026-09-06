package reconciliation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

func repairInputs(t *testing.T, diff money.Quantity) BalanceRepairInputs {
	t.Helper()
	return BalanceRepairInputs{
		RecordID: NewRecordID(), AccountID: accounts.NewAccountID(), AssetID: assets.NewAssetID(),
		Difference: diff, ReasonCode: "DUST", EffectiveAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}
}

func TestBalanceRepair_ChainHoldsMore(t *testing.T) {
	t.Parallel()
	in := repairInputs(t, q(10_000))
	rep, err := BalanceRepair(in)
	require.NoError(t, err)
	require.NoError(t, rep.Validate(in.RecordID))

	assert.Equal(t, ledger.KindReconciliationAdjustment, rep.Posting.Kind)
	assert.Equal(t, "reconciliation:"+in.RecordID.String(), rep.Posting.IdempotencyKey)
	assert.Equal(t, ledger.FinancialEventReference{Type: "reconciliation_record", ID: in.RecordID.String()}, rep.Posting.Reference)
	assert.Equal(t, "DUST", rep.Posting.ReasonCode())

	require.Len(t, rep.Posting.Entries, 2)
	assert.Equal(t, ledger.CodeWallet, rep.Posting.Entries[0].Account.Code)
	assert.Equal(t, ledger.Debit, rep.Posting.Entries[0].Side)
	assert.Equal(t, ledger.CodeReconciliationAdjustment, rep.Posting.Entries[1].Account.Code)
	assert.Equal(t, ledger.Credit, rep.Posting.Entries[1].Side)
	assert.True(t, rep.Posting.Entries[0].Quantity.Equal(q(10_000)))
	assert.True(t, rep.Posting.Entries[1].Quantity.Equal(q(10_000)))
}

func TestBalanceRepair_ChainHoldsLess(t *testing.T) {
	t.Parallel()
	in := repairInputs(t, q(-10_000))
	rep, err := BalanceRepair(in)
	require.NoError(t, err)
	require.NoError(t, rep.Validate(in.RecordID))

	assert.Equal(t, ledger.Credit, rep.Posting.Entries[0].Side, "WALLET is credited when the chain holds less")
	assert.Equal(t, ledger.Debit, rep.Posting.Entries[1].Side)
	// The quantity is the absolute difference: a posting never carries a
	// negative quantity, the side says the direction.
	assert.True(t, rep.Posting.Entries[0].Quantity.Equal(q(10_000)))
}

func TestBalanceRepair_BalancesPerAsset(t *testing.T) {
	t.Parallel()
	for _, diff := range []money.Quantity{q(1), q(-1), q(999_999_999)} {
		in := repairInputs(t, diff)
		rep, err := BalanceRepair(in)
		require.NoError(t, err)
		debit, credit := q(0), q(0)
		for _, e := range rep.Posting.Entries {
			require.Equal(t, in.AssetID, e.Account.AssetID, "every entry is in the same asset")
			if e.Side == ledger.Debit {
				debit = debit.Add(e.Quantity)
			} else {
				credit = credit.Add(e.Quantity)
			}
		}
		assert.True(t, debit.Equal(credit), "Σdebit == Σcredit for %s", diff)
	}
}

func TestBalanceRepair_RefusesNothingToRepair(t *testing.T) {
	t.Parallel()
	_, err := BalanceRepair(repairInputs(t, q(0)))
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	in := repairInputs(t, q(1))
	in.ReasonCode = ""
	_, err = BalanceRepair(in)
	assert.Error(t, err, "a repair without a reason code is not auditable")

	in = repairInputs(t, q(1))
	in.AccountID = accounts.AccountID{}
	_, err = BalanceRepair(in)
	assert.Error(t, err)
}

func TestRepair_ValidateRejectsWrongShape(t *testing.T) {
	t.Parallel()
	in := repairInputs(t, q(5))
	rep, err := BalanceRepair(in)
	require.NoError(t, err)

	other := NewRecordID()
	assert.Error(t, rep.Validate(other), "a repair may only be attached to the record it references")

	tradeFill := rep
	tradeFill.Posting.Kind = ledger.KindTradeFill
	assert.Error(t, tradeFill.Validate(in.RecordID), "a repair is never a TRADE_FILL")

	noKey := rep
	noKey.Posting.IdempotencyKey = "something-else"
	assert.Error(t, noKey.Validate(in.RecordID), "the idempotency key pins one repair per record")

	noReason := rep
	noReason.Posting.Metadata = map[string]any{}
	assert.Error(t, noReason.Validate(in.RecordID))
}

func TestRepair_PositionHalfFollowsTheSign(t *testing.T) {
	t.Parallel()
	usd := money.USDFromMinor(250)

	in := repairInputs(t, q(7))
	in.USDValue = &usd
	rep, err := BalanceRepair(in)
	require.NoError(t, err)
	rep, err = rep.WithPositionRepair(in, "reconciliation:test")
	require.NoError(t, err)
	require.NotNil(t, rep.Position)
	require.NotNil(t, rep.Position.Acquire)
	assert.Nil(t, rep.Position.Dispose)
	assert.True(t, rep.Position.Acquire.Quantity.Equal(q(7)))
	require.NoError(t, rep.Validate(in.RecordID))

	in = repairInputs(t, q(-7))
	in.USDValue = &usd
	rep, err = BalanceRepair(in)
	require.NoError(t, err)
	rep, err = rep.WithPositionRepair(in, "reconciliation:test")
	require.NoError(t, err)
	require.NotNil(t, rep.Position.Dispose)
	assert.Nil(t, rep.Position.Acquire)
	assert.True(t, rep.Position.Dispose.Quantity.Equal(q(7)))
	require.NoError(t, rep.Validate(in.RecordID))

	_, err = rep.WithPositionRepair(in, "")
	assert.Error(t, err, "a position repair needs a named valuation source")
}

func TestRepairIdempotencyKey_IsOnePerRecord(t *testing.T) {
	t.Parallel()
	a, b := NewRecordID(), NewRecordID()
	assert.Equal(t, RepairIdempotencyKey(a), RepairIdempotencyKey(a))
	assert.NotEqual(t, RepairIdempotencyKey(a), RepairIdempotencyKey(b))
}
