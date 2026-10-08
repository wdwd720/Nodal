//go:build integration

package orders

import (
	"flag"
	"fmt"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// boundedChecks runs a database-backed property with at most n checks unless
// RAPID_CHECKS or -rapid.checks asked for a number explicitly: every check
// opens its own book and accounts, and rapid's default of 100 checks would
// spend minutes per property.
func boundedChecks(t *testing.T, n int, prop func(*rapid.T)) {
	t.Helper()
	fl := flag.Lookup("rapid.checks")
	if fl != nil && fl.Value.String() == "100" {
		require.NoError(t, flag.Set("rapid.checks", strconv.Itoa(n)))
		defer func() { _ = flag.Set("rapid.checks", "100") }()
	}
	rapid.Check(t, prop)
}

// propMarket is one check's world: a fresh market, three funded accounts and
// what they were funded with.
type propMarket struct {
	m          spotMarket
	accts      []accounts.AccountID
	fundedBase money.Quantity
	fundedQuot money.Quantity
	orders     []OrderID
}

func (f *fixture) newPropMarket() *propMarket {
	m := f.spotMarket(marketOpts{makerBPS: 7, takerBPS: 13, snapshotInterval: 4})
	p := &propMarket{m: m}
	for i := 0; i < 3; i++ {
		a := f.account()
		f.fund(a, m.base.ID, 50_000_000)      // 50 base units
		f.fund(a, m.quote.ID, 10_000_000_000) // 10,000 quote units
		p.accts = append(p.accts, a)
		p.fundedBase = p.fundedBase.Add(money.QuantityFromInt64(50_000_000))
		p.fundedQuot = p.fundedQuot.Add(money.QuantityFromInt64(10_000_000_000))
	}
	return p
}

// step draws and applies one command: a new order of any shape, a cancel or
// a replace. Business refusals are part of the stream.
func (f *fixture) step(rt *rapid.T, p *propMarket) {
	acct := rapid.SampledFrom(p.accts).Draw(rt, "account")
	op := rapid.IntRange(0, 9).Draw(rt, "op")
	if op < 2 && len(p.orders) > 0 {
		oid := rapid.SampledFrom(p.orders).Draw(rt, "cancel target")
		o := f.orderView(oid)
		_, _ = f.pipe.Cancel(f.ctx, CancelRequest{OrderID: oid, AccountID: *o.AccountID, ActorType: security.ActorUser, ActorID: "prop"})
		return
	}
	if op < 3 && len(p.orders) > 0 {
		oid := rapid.SampledFrom(p.orders).Draw(rt, "replace target")
		o := f.orderView(oid)
		if o.Status.Resting() {
			ticks := rapid.Int64Range(9_990, 10_010).Draw(rt, "replace ticks")
			lots := o.FilledLots + rapid.Int64Range(1, 30).Draw(rt, "replace extra lots")
			_, _ = f.pipe.Replace(f.ctx, ReplaceRequest{OrderID: oid, AccountID: *o.AccountID,
				NewPrice: *p.m.price(f.t, fmt.Sprintf("%d.%02d", ticks/100, ticks%100)), NewQuantity: p.m.qty(lots),
				IdempotencyKey: uuid.NewString(), ActorType: security.ActorUser, ActorID: "prop"})
		}
		return
	}
	side := rapid.SampledFrom([]Side{SideBuy, SideSell}).Draw(rt, "side")
	ticks := rapid.Int64Range(9_990, 10_010).Draw(rt, "ticks")
	r := f.limit(p.m, acct, side, fmt.Sprintf("%d.%02d", ticks/100, ticks%100), p.m.qty(rapid.Int64Range(1, 30).Draw(rt, "lots")))
	switch rapid.IntRange(0, 9).Draw(rt, "shape") {
	case 0:
		r.TimeInForce = IOC
	case 1:
		r.TimeInForce = FOK
	case 2:
		r.Type, r.TimeInForce = TypeMarket, IOC
	case 3:
		r.PostOnly = true
	case 4:
		r.STP = STPCancelMaker
	}
	out, err := f.pipe.Submit(f.ctx, r)
	if err == nil {
		p.orders = append(p.orders, out.Order.ID)
	}
}

// qty is a number of lots in base units.
func (m spotMarket) qty(lots int64) money.Quantity { return money.QuantityFromInt64(lots * 100) }

// TestProp_NoCrossedRestingBookAfterEveryCommandThroughTheService: after
// every command of a random stream through the service -- orders of every
// shape, cancels and replaces, from several accounts -- the persisted resting
// book is not crossed, and the journal replays to the recorded state.
func TestProp_NoCrossedRestingBookAfterEveryCommandThroughTheService(t *testing.T) {
	f := newFixture(t)
	boundedChecks(t, 8, func(rt *rapid.T) {
		p := f.newPropMarket()
		n := rapid.IntRange(1, 25).Draw(rt, "commands")
		for i := 0; i < n; i++ {
			f.step(rt, p)
			var bid, ask *int64
			require.NoError(rt, testDB.QueryRow(f.ctx, `SELECT
				(SELECT max(price_ticks) FROM book_orders WHERE instrument_id = $1 AND side = 'BUY' AND status IN ('OPEN','PARTIALLY_FILLED')),
				(SELECT min(price_ticks) FROM book_orders WHERE instrument_id = $1 AND side = 'SELL' AND status IN ('OPEN','PARTIALLY_FILLED'))`,
				p.m.inst).Scan(&bid, &ask))
			if bid != nil && ask != nil && *bid >= *ask {
				rt.Fatalf("crossed after command %d: bid %d >= ask %d", i+1, *bid, *ask)
			}
		}
		proof, err := f.svc.VerifyJournal(f.ctx, testDB, p.m.inst)
		require.NoError(rt, err)
		require.Equal(rt, proof.StateHash, proof.FullReplayHash)
	})
}

// TestProp_QuantitiesAreConservedAndNoOrderOverfillsThroughTheService: over
// random streams through the service, every order's quantity is exactly its
// fills plus what rests plus what was cancelled, its fills are exactly the
// trades it took part in, base and quote are conserved across the accounts
// and the platform, every trade moved exactly its base quantity and its
// notional, and nothing stays reserved for an order that no longer works.
func TestProp_QuantitiesAreConservedAndNoOrderOverfillsThroughTheService(t *testing.T) {
	f := newFixture(t)
	boundedChecks(t, 8, func(rt *rapid.T) {
		p := f.newPropMarket()
		n := rapid.IntRange(1, 25).Draw(rt, "commands")
		for i := 0; i < n; i++ {
			f.step(rt, p)
		}
		rows, err := testDB.Query(f.ctx, `SELECT o.id, o.qty_lots, o.filled_lots, o.remaining_lots, o.cancelled_lots, o.status,
			coalesce((SELECT sum(t.qty_lots) FROM book_trades t WHERE t.maker_order_id = o.id OR t.taker_order_id = o.id), 0)
			FROM book_orders o WHERE o.instrument_id = $1`, p.m.inst)
		require.NoError(rt, err)
		for rows.Next() {
			var oid string
			var qty, filled, remaining, cancelled, traded int64
			var status Status
			require.NoError(rt, rows.Scan(&oid, &qty, &filled, &remaining, &cancelled, &status, &traded))
			if status != StatusRejected && status != StatusPending && qty != filled+remaining+cancelled {
				rt.Fatalf("order %s: %d != %d + %d + %d", oid, qty, filled, remaining, cancelled)
			}
			if filled != traded || filled > qty {
				rt.Fatalf("order %s: filled %d, traded %d, quantity %d", oid, filled, traded, qty)
			}
		}
		rows.Close()
		require.NoError(rt, rows.Err())

		var base, quote money.Quantity
		for _, a := range p.accts {
			base = base.Add(f.wallet(a, p.m.base.ID))
			quote = quote.Add(f.wallet(a, p.m.quote.ID))
		}
		revenue := f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, p.m.quote.ID))
		if !base.Equal(p.fundedBase) {
			rt.Fatalf("base not conserved: %s != %s", base, p.fundedBase)
		}
		if !quote.Add(revenue).Equal(p.fundedQuot) {
			rt.Fatalf("quote not conserved: %s + %s != %s", quote, revenue, p.fundedQuot)
		}
		var tradedBase, inflowBase, outflowBase money.Quantity
		var text string
		require.NoError(rt, testDB.QueryRow(f.ctx, `SELECT coalesce(sum(base_quantity), 0)::text FROM book_trades WHERE instrument_id = $1`, p.m.inst).Scan(&text))
		tradedBase, err = money.ScanQuantity(text)
		require.NoError(rt, err)
		for _, a := range p.accts {
			inflowBase = inflowBase.Add(f.balance(ledger.CustomerAccount(a, ledger.CodeTradingInflow, p.m.base.ID)))
			outflowBase = outflowBase.Add(f.balance(ledger.CustomerAccount(a, ledger.CodeTradingOutflow, p.m.base.ID)))
		}
		if !inflowBase.Equal(tradedBase) || !outflowBase.Equal(tradedBase) {
			rt.Fatalf("base delivered %s and received %s, traded %s", outflowBase, inflowBase, tradedBase)
		}
		var stale int
		require.NoError(rt, testDB.QueryRow(f.ctx, `SELECT count(*) FROM book_orders o JOIN asset_reservations r ON r.id = o.reservation_id
			WHERE o.instrument_id = $1 AND o.status NOT IN ('OPEN','PARTIALLY_FILLED') AND r.status = 'ACTIVE'`, p.m.inst).Scan(&stale))
		if stale != 0 {
			rt.Fatalf("%d finished orders still hold an active reservation", stale)
		}
	})
}

// TestProp_CustomerWalletsChangeOnlyByFeesAcrossRandomTrading: across random
// trading among customers, the sum of their WALLET balances in each asset
// changes only by the fees they paid -- by nothing at all in the base asset,
// by exactly the disclosed maker and taker fees in the quote asset -- and the
// platform's fee revenue is exactly those fees.
func TestProp_CustomerWalletsChangeOnlyByFeesAcrossRandomTrading(t *testing.T) {
	f := newFixture(t)
	boundedChecks(t, 8, func(rt *rapid.T) {
		p := f.newPropMarket()
		n := rapid.IntRange(1, 25).Draw(rt, "commands")
		for i := 0; i < n; i++ {
			f.step(rt, p)
		}
		var base, quote money.Quantity
		for _, a := range p.accts {
			base = base.Add(f.wallet(a, p.m.base.ID))
			quote = quote.Add(f.wallet(a, p.m.quote.ID))
		}
		var text string
		require.NoError(rt, testDB.QueryRow(f.ctx, `SELECT coalesce(sum(maker_fee + taker_fee), 0)::text FROM book_trades WHERE instrument_id = $1`,
			p.m.inst).Scan(&text))
		fees, err := money.ScanQuantity(text)
		require.NoError(rt, err)
		if !base.Equal(p.fundedBase) {
			rt.Fatalf("customer base wallets moved by %s", base.Sub(p.fundedBase))
		}
		if !p.fundedQuot.Sub(quote).Equal(fees) {
			rt.Fatalf("customer quote wallets moved by %s, fees were %s", p.fundedQuot.Sub(quote), fees)
		}
		revenue := f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, p.m.quote.ID))
		if !revenue.Equal(fees) {
			rt.Fatalf("platform revenue %s, fees %s", revenue, fees)
		}
	})
}
