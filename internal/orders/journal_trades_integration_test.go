//go:build integration

package orders

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// tamperTrades rewrites a book's recorded trades as the database owner:
// book_trades is append-only for everyone, so the owner first lifts its
// immutability trigger, in the same transaction that puts it back. This is
// outside the threat model -- it is the owner's power -- and exactly what the
// journal proof exists to notice.
func (f *fixture) tamperTrades(sql string, args ...any) {
	f.t.Helper()
	conn := f.owner()
	tx, err := conn.Begin(f.ctx)
	require.NoError(f.t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(f.ctx, `ALTER TABLE book_trades DISABLE TRIGGER book_trades_immutable`)
	require.NoError(f.t, err)
	tag, err := tx.Exec(f.ctx, sql, args...)
	require.NoError(f.t, err)
	require.Positive(f.t, tag.RowsAffected(), "the tampering touched a trade")
	_, err = tx.Exec(f.ctx, `ALTER TABLE book_trades ENABLE TRIGGER book_trades_immutable`)
	require.NoError(f.t, err)
	require.NoError(f.t, tx.Commit(f.ctx))
}

// mismatchOf returns the proof's mismatch for a field, if any.
func mismatchOf(p JournalProof, field string) (TradeMismatch, bool) {
	for _, m := range p.TradeMismatches {
		if m.Field == field {
			return m, true
		}
	}
	return TradeMismatch{}, false
}

// TestIntegration_TheJournalProofComparesTheRecordedTradesAndTheirJournals
// (review 3, observation): the proof replayed the journal to the state hash
// but never compared what the book recorded and posted. Now every replayed
// trade must be its book_trades row -- sequence, price, quantity, parties,
// value and fees -- and the SIM_TRADE journal the row names must move exactly
// that trade's value and fees. A trade the database owner rewrites -- its
// price, a party, a fee, the journal it points at -- makes the proof fail and
// name the difference.
func TestIntegration_TheJournalProofComparesTheRecordedTradesAndTheirJournals(t *testing.T) {
	f := newFixture(t)

	honest := f.tradedMarket()
	proof, err := f.svc.VerifyJournal(f.ctx, testDB, honest.m.inst)
	require.NoError(t, err)
	require.Positive(t, proof.Trades)
	assert.Equal(t, int(proof.Trades), proof.TradesChecked, "every replayed trade was compared with its row and its journal")
	assert.Empty(t, proof.TradeMismatches)

	type trade struct {
		id, journal  string
		ticks        int64
		maker, taker string
		takerFee     string
	}
	recorded := func(m tradedMarket) trade {
		t.Helper()
		var tr trade
		require.NoError(t, testDB.QueryRow(f.ctx, `SELECT id::text, journal_transaction_id::text, price_ticks, maker_owner_id::text,
			taker_owner_id::text, taker_fee::text FROM book_trades WHERE book_id = $1 ORDER BY trade_seq LIMIT 1`, m.book.ID).
			Scan(&tr.id, &tr.journal, &tr.ticks, &tr.maker, &tr.taker, &tr.takerFee))
		return tr
	}
	failed := func(m tradedMarket, field string) TradeMismatch {
		t.Helper()
		p, err := f.svc.VerifyJournal(f.ctx, testDB, m.m.inst)
		require.Error(t, err)
		assert.Equal(t, errs.CodeInternal, errs.CodeOf(err), "a finding about the journal, as every replay finding is")
		mm, ok := mismatchOf(p, field)
		require.True(t, ok, "the proof names %s: %+v", field, p.TradeMismatches)
		assert.Equal(t, 1, p.TradesChecked)
		return mm
	}

	t.Run("the price", func(t *testing.T) {
		m := f.tradedMarket()
		tr := recorded(m)
		f.tamperTrades(`UPDATE book_trades SET price_ticks = price_ticks + 1 WHERE id = $1`, tr.id)
		mm := failed(m, "price_ticks")
		assert.Equal(t, strconv.FormatInt(tr.ticks+1, 10), mm.Recorded)
		assert.Equal(t, strconv.FormatInt(tr.ticks, 10), mm.Replayed)
	})

	t.Run("a party", func(t *testing.T) {
		m := f.tradedMarket()
		tr := recorded(m)
		stranger := uuid.NewString()
		f.tamperTrades(`UPDATE book_trades SET taker_owner_id = $2 WHERE id = $1`, tr.id, stranger)
		mm := failed(m, "taker_owner")
		assert.Equal(t, "CUSTOMER/"+stranger, mm.Recorded)
		assert.Equal(t, "CUSTOMER/"+tr.taker, mm.Replayed)
	})

	t.Run("a fee", func(t *testing.T) {
		m := f.tradedMarket()
		tr := recorded(m)
		f.tamperTrades(`UPDATE book_trades SET taker_fee = taker_fee + 1 WHERE id = $1`, tr.id)
		mm := failed(m, "taker_fee")
		assert.Equal(t, tr.takerFee, mm.Replayed, "the fee the book's schedule charges")
	})

	t.Run("the journal", func(t *testing.T) {
		m := f.tradedMarket()
		tr := recorded(m)
		maker, err := accounts.ParseAccountID(tr.maker)
		require.NoError(t, err)
		taker, err := accounts.ParseAccountID(tr.taker)
		require.NoError(t, err)
		// A SIM_TRADE between the same two customers, naming the trade, that
		// moves half of it and no fee.
		half, halfNotional := money.QuantityFromInt64(500_000), money.QuantityFromInt64(50_000_000)
		var other ledger.TransactionID
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			res, err := f.ledger.Post(ctx, tx, ledger.Posting{
				Kind: ledger.KindSimTrade, IdempotencyKey: "tampered-trade:" + uuid.NewString(),
				Reference: ledger.FinancialEventReference{Type: "book_trade", ID: tr.id}, EffectiveAt: f.clk.Now(),
				Entries: []ledger.Entry{
					{Account: ledger.CustomerAccount(taker, ledger.CodeWallet, m.m.quote.ID), Side: ledger.Credit, Quantity: halfNotional},
					{Account: ledger.CustomerAccount(taker, ledger.CodeTradingOutflow, m.m.quote.ID), Side: ledger.Debit, Quantity: halfNotional},
					{Account: ledger.CustomerAccount(taker, ledger.CodeWallet, m.m.base.ID), Side: ledger.Debit, Quantity: half},
					{Account: ledger.CustomerAccount(taker, ledger.CodeTradingInflow, m.m.base.ID), Side: ledger.Credit, Quantity: half},
					{Account: ledger.CustomerAccount(maker, ledger.CodeWallet, m.m.base.ID), Side: ledger.Credit, Quantity: half},
					{Account: ledger.CustomerAccount(maker, ledger.CodeTradingOutflow, m.m.base.ID), Side: ledger.Debit, Quantity: half},
					{Account: ledger.CustomerAccount(maker, ledger.CodeWallet, m.m.quote.ID), Side: ledger.Debit, Quantity: halfNotional},
					{Account: ledger.CustomerAccount(maker, ledger.CodeTradingInflow, m.m.quote.ID), Side: ledger.Credit, Quantity: halfNotional},
				},
			})
			other = res.TransactionID
			return err
		}))
		f.tamperTrades(`UPDATE book_trades SET journal_transaction_id = $2 WHERE id = $1`, tr.id, other)
		mm := failed(m, "journal CUSTOMER/"+tr.taker+"/WALLET/"+m.m.base.ID.String())
		assert.Equal(t, "500000", mm.Recorded, "the journal moved half the base quantity")
		assert.Equal(t, "1000000", mm.Replayed, "the trade moved one whole unit")
		p, _ := f.svc.VerifyJournal(f.ctx, testDB, m.m.inst)
		_, feeNamed := mismatchOf(p, "journal CUSTOMER/"+tr.taker+"/FEES_PLATFORM/"+m.m.quote.ID.String())
		assert.True(t, feeNamed, "and none of the taker's fee: %+v", p.TradeMismatches)
	})

	// The honest book still proves after all of that.
	_, err = f.svc.VerifyJournal(f.ctx, testDB, honest.m.inst)
	require.NoError(t, err)
}
