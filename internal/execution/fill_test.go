package execution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func TestEffectivePrice(t *testing.T) {
	t.Parallel()
	// 100 USDC (6 dec) bought 0.666666666 SOL (9 dec) → 150.000000150 USDC/SOL.
	m, scale, err := EffectivePrice(money.QuantityFromInt64(666_666_666), 9, money.QuantityFromInt64(100_000_000), 6)
	require.NoError(t, err)
	require.Equal(t, EffectivePriceScale, scale)
	require.Equal(t, "150.000000150000", m.ToDecimalString(uint8(scale)))
	// Zero base quantity yields a zero price rather than an error.
	m, _, err = EffectivePrice(money.Quantity{}, 9, money.QuantityFromInt64(5), 6)
	require.NoError(t, err)
	require.True(t, m.IsZero())
}

func validFill() Fill {
	return Fill{
		ID: NewFillID(), OrderID: NewOrderID(), AccountID: accounts.NewAccountID(), Venue: "JUPITER", ExternalFillID: "f-1",
		InputAssetID: assets.NewAssetID(), InputQuantity: money.QuantityFromInt64(10), OutputAssetID: assets.NewAssetID(), OutputQuantity: money.QuantityFromInt64(5),
		EffectivePriceMantissa: money.QuantityFromInt64(2), EffectivePriceScale: 0, Source: FillFromProvider, Finality: FinalityConfirmed, ObservedAt: time.Now(),
	}
}

func TestFillValidate(t *testing.T) {
	t.Parallel()
	require.NoError(t, validFill().Validate())
	f := validFill()
	f.Finality = FinalitySubmitted
	require.Error(t, f.Validate(), "a fill exists only once observed")
	f = validFill()
	f.InputQuantity = money.Quantity{}
	require.Error(t, f.Validate())
	f = validFill()
	f.NetworkFeeQuantity = money.QuantityFromInt64(1)
	require.Error(t, f.Validate(), "fee without asset")
	f = validFill()
	f.Source = "GUESS"
	require.Error(t, f.Validate())
	err := Fill{}.Validate()
	require.True(t, errs.HasCode(err, errs.CodeValidationFailed))
}

type memArchive struct{ objects map[string][]byte }

func (m *memArchive) Put(_ context.Context, key, _ string, body []byte) (string, error) {
	if m.objects == nil {
		m.objects = map[string][]byte{}
	}
	m.objects[key] = body
	return "mem://" + key, nil
}

func TestStoreEvidence(t *testing.T) {
	t.Parallel()
	a := &memArchive{}
	ev, err := StoreEvidence(context.Background(), a, EvidenceKey("plan", "p1", "01-quote", "response"), map[string]any{"b": 1, "a": "x"})
	require.NoError(t, err)
	require.Equal(t, "mem://execution/plan/p1/01-quote/response", ev.RawRef)
	body := a.objects["execution/plan/p1/01-quote/response"]
	sum := sha256.Sum256(body)
	require.Equal(t, sum[:], ev.Hash)
	require.Len(t, ev.HexHash(), 64)
	require.Equal(t, len(body), ev.Size)
	// Raw JSON is stored verbatim.
	raw := json.RawMessage(`{"provider":"jupiter","x":[1,2]}`)
	ev2, err := StoreEvidence(context.Background(), a, "execution/raw", raw)
	require.NoError(t, err)
	require.Equal(t, []byte(raw), a.objects["execution/raw"])
	require.Equal(t, HashBytes(raw), ev2.Hash)
	_, err = StoreEvidence(context.Background(), a, "../escape", raw)
	require.True(t, errs.HasCode(err, errs.CodeValidationFailed))
	_, err = StoreEvidence(context.Background(), nil, "k", raw)
	require.True(t, errs.HasCode(err, errs.CodeInternal))
}
