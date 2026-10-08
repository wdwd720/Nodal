package bridge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// books applies postings to normal-side balances, as ledger_apply_entry does.
type books map[ledger.AccountRef]money.Quantity

func (b books) apply(t *rapid.T, p ledger.Posting) {
	require.NoError(t, p.Validate(), "%s", p.Kind)
	for _, e := range p.Entries {
		q := e.Quantity
		if e.Side != e.Account.Code.NormalSide() {
			q = q.Neg()
		}
		b[e.Account] = b[e.Account].Add(q)
		if !e.Account.Code.AllowsNegative() {
			require.False(t, b[e.Account].IsNegative(), "%s %s went negative", e.Account.OwnerType, e.Account.Code)
		}
	}
}

// newID mints a fresh identifier of another package's kind.
func newID[T any, PT interface {
	*T
	UnmarshalText([]byte) error
}]() T {
	var v T
	if err := PT(&v).UnmarshalText([]byte(id.New[id.Any]().String())); err != nil {
		panic(err)
	}
	return v
}

func testRoute() Route {
	return Route{ID: id.New[routeKind](), FacilityID: newID[econ.FacilityID](), SourceAssetID: newID[assets.AssetID](),
		DestinationAssetID: newID[assets.AssetID](), Decimals: 6}
}

// TestProp_PostingsConserveValueAcrossChains: over random legs that fail
// before finality, stay in flight, complete or are refunded, the three
// bridge postings are valid double entries that balance per party, keep
// BRIDGE_LOCKED = BRIDGE_OUTSTANDING + BRIDGE_IN_FLIGHT, and leave the
// customer's value on the source, in flight and on the destination equal to
// what it started with: no unit is counted on both chains, or on neither.
func TestProp_PostingsConserveValueAcrossChains(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		r := testRoute()
		acct := newID[accounts.AccountID]()
		funded := money.QuantityFromInt64(rapid.Int64Range(1, 1_000_000_000).Draw(rt, "funded"))
		b := books{}
		walletSrc := ledger.CustomerAccount(acct, ledger.CodeWallet, r.SourceAssetID)
		inFlight := ledger.CustomerAccount(acct, ledger.CodeBridgeInFlight, r.SourceAssetID)
		walletDst := ledger.CustomerAccount(acct, ledger.CodeWallet, r.DestinationAssetID)
		b[walletSrc] = funded
		now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
		var legs []Leg
		n := rapid.IntRange(1, 12).Draw(rt, "legs")
		for i := 0; i < n; i++ {
			free := b[walletSrc]
			if !free.IsPositive() {
				break
			}
			ceiling, err := free.Int64()
			require.NoError(rt, err)
			l := Leg{ID: id.New[legKind](), RouteID: r.ID, AccountID: acct, Quantity: money.QuantityFromInt64(rapid.Int64Range(1, ceiling).Draw(rt, "q")),
				Status: StateBridgeRequested}
			tx := func() *ledger.TransactionID { v := ledger.NewTransactionID(); return &v }
			switch rapid.IntRange(0, 3).Draw(rt, "path") {
			case 0: // the lock never finalized: nothing posts
				l.Status = StateFailed
			case 1: // in flight
				b.apply(rt, sourceFinalizedPosting(r, l, now))
				l.Status, l.SourceJournal = StateMessageInFlight, tx()
			case 2: // completed
				b.apply(rt, sourceFinalizedPosting(r, l, now))
				b.apply(rt, destinationFinalizedPosting(r, l, "release", now))
				l.Status, l.SourceJournal, l.DestinationJournal = StateReconciled, tx(), tx()
			case 3: // refunded after the destination never released
				b.apply(rt, sourceFinalizedPosting(r, l, now))
				b.apply(rt, refundPosting(r, l, "refund", now))
				l.Status, l.SourceJournal, l.RefundJournal = StateFailed, tx(), tx()
			}
			legs = append(legs, l)

			locked := b[r.account(ledger.CodeBridgeLocked, r.SourceAssetID)]
			outstanding := b[r.account(ledger.CodeBridgeOutstanding, r.DestinationAssetID)]
			require.Zero(rt, locked.Cmp(outstanding.Add(b[inFlight])), "locked %s, outstanding %s, in flight %s", locked, outstanding, b[inFlight])
			require.Zero(rt, b[walletSrc].Add(b[inFlight]).Add(b[walletDst]).Cmp(funded), "value appeared or vanished across chains")

			var heldInFlight, heldDst money.Quantity
			for _, x := range legs {
				switch x.Holding() {
				case HoldingInFlight:
					heldInFlight = heldInFlight.Add(x.Quantity)
				case HoldingDestination:
					heldDst = heldDst.Add(x.Quantity)
				}
			}
			require.Zero(rt, heldInFlight.Cmp(b[inFlight]), "the legs in flight are the in-flight balance")
			require.Zero(rt, heldDst.Cmp(b[walletDst]), "the completed legs are the destination wallet")
		}
	})
}
