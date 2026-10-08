//go:build integration

package feeledger

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_EnsureScheduleIsTheVersionInForceOrRefuses: a subsystem
// whose charges are fixed by its own configuration makes that exact schedule
// the version in force -- published once, then found -- and is refused,
// rather than handed a superseded version, as soon as another version is in
// force. An agent never ensures, and an ensured version is never dated.
func TestIntegration_EnsureScheduleIsTheVersionInForceOrRefuses(t *testing.T) {
	f := newFixture(t)
	quote := f.simAsset("FWQ")
	key := "itest:ensure:" + uuid.NewString()
	ours := f.schedule(quote.ID, "ensure-v1")
	ensure := func(s fees.Schedule, actor econ.Actor, from time.Time) (PublishedSchedule, error) {
		return do(f, func(ctx context.Context, tx pgx.Tx) (PublishedSchedule, error) {
			return f.svc.EnsureSchedule(ctx, tx, PublishRequest{Key: key, Schedule: s, EffectiveFrom: from, Reason: "itest", Actor: actor})
		})
	}
	hash, err := ours.Hash()
	require.NoError(t, err)

	first, err := ensure(ours, service, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, 1, first.Version)
	assert.Equal(t, hash, first.Hash)
	again, err := ensure(ours, service, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, first.ID, again.ID, "the version in force is found, not published again")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM fee_schedules WHERE schedule_key = $1`, key))

	_, err = ensure(ours, econ.Actor{Type: security.ActorAgent, ID: "agent-1"}, time.Time{})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "an agent never ensures a schedule")
	_, err = ensure(ours, service, f.clk.Now().Add(time.Hour))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "an ensured version takes effect now")

	// An operator dates another version an hour ahead: until then ours is
	// still the one in force; from then on ours is refused, and so is a
	// return to its label, which names a version no longer in force.
	other := f.schedule(quote.ID, "ensure-v2")
	other.SpreadBPS = 30
	f.publish(key, other, f.clk.Now().Add(time.Hour))
	_, err = ensure(ours, service, time.Time{})
	require.NoError(t, err)
	f.clk.Advance(2 * time.Hour)
	_, err = ensure(ours, service, time.Time{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	assert.Equal(t, ReasonScheduleNotInForce, ReasonOf(err))
	inForce, err := ensure(other, operator, time.Time{})
	require.NoError(t, err, "the version in force is ensured by whoever names it")
	assert.Equal(t, 2, inForce.Version)
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM fee_schedules WHERE schedule_key = $1`, key))

	// On a fresh key nothing is in force until the first ensure publishes it.
	fresh := "itest:ensure:" + uuid.NewString()
	_, err = f.svc.EffectiveSchedule(f.ctx, testDB, fresh, f.clk.Now())
	assert.Equal(t, ReasonScheduleNotFound, ReasonOf(err))
	key = fresh
	p, err := ensure(ours, service, time.Time{})
	require.NoError(t, err)
	got, err := f.svc.EffectiveSchedule(f.ctx, testDB, fresh, f.clk.Now())
	require.NoError(t, err)
	assert.Equal(t, p.ID, got.ID)
}

// TestIntegration_ARiskReducingChargeIsNotStoppedByAKillSwitch: a SPREAD_ENGINE
// kill switch (or the global one) refuses a disclosed charge -- unless the
// charge belongs to a risk-reducing operation, which a kill switch never
// stops; LIVE is refused either way.
func TestIntegration_ARiskReducingChargeIsNotStoppedByAKillSwitch(t *testing.T) {
	f := newFixture(t)
	quote := f.simAsset("FWQ")
	base := f.simAsset("FWB")
	key := "itest:risk-reducing:" + uuid.NewString()
	f.publish(key, f.schedule(quote.ID, "rr-v1"), time.Time{})
	payer := f.customer(quote.ID, units(10_000))
	request := func(riskReducing bool, mode gates.Mode) QuoteRequest {
		return QuoteRequest{ScheduleKey: key, Subsystem: econ.SubsystemMargin, IdempotencyKey: "rr-" + uuid.NewString(),
			AccountID: &payer, Mode: mode, Actor: service, RiskReducing: riskReducing,
			Input: fees.QuoteInput{Side: fees.SideSell, BaseQty: units(1), BaseDecimals: 6, QuoteDecimals: 6,
				ReferencePrice: f.price(quote.ID, "100.00")}}
	}
	record := func(r QuoteRequest) (StoredQuote, error) {
		return do(f, func(ctx context.Context, tx pgx.Tx) (StoredQuote, error) { return f.svc.RecordQuote(ctx, tx, r) })
	}
	for _, sw := range []killswitch.Switch{
		{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.SpreadEngine), Active: true},
		{Kind: killswitch.GlobalNewRiskKill, ScopeID: killswitch.GlobalScope, Active: true},
	} {
		f.kills.set(sw)
		_, err := record(request(false, gates.ModePaper))
		assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err), "%s: new activity is stopped", sw.Kind)
		q, err := record(request(true, gates.ModePaper))
		require.NoError(t, err, "%s: a risk-reducing charge is disclosed", sw.Kind)

		// Posted inside the caller's journal and registered there, flag and all.
		booked, err := do(f, func(ctx context.Context, tx pgx.Tx) (Booking, error) {
			journal, effect, err := f.embeddedJournal(ctx, tx, payer, base.ID, q)
			if err != nil {
				return Booking{}, err
			}
			req := EmbeddedRequest{DisclosureID: q.ID, AccountID: payer, JournalTransactionID: journal, EffectID: effect,
				Mode: gates.ModePaper, Actor: service}
			if _, err := f.svc.RecordEmbedded(ctx, tx, req); errs.CodeOf(err) != errs.CodeKillSwitchActive {
				t.Errorf("%s: an ordinary registration under the switch: %v", sw.Kind, err)
			}
			req.RiskReducing = true
			return f.svc.RecordEmbedded(ctx, tx, req)
		})
		require.NoError(t, err)
		assert.Equal(t, BookingEmbedded, booked.BookingMode)
		require.NoError(t, f.svc.VerifyBooking(f.ctx, testDB, booked.ID))
	}
	_, err := record(request(true, gates.ModeLive))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "LIVE is never charged")
	_, err = record(request(true, gates.ModeDevnet))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "nor a mode the fee ledger does not run in")
	f.kills.set()
}

// embeddedJournal posts a simulated buy of one unit of base at the quote's
// reference notional, with the quote's charges inside the same journal, and
// applies it by an effect of its own, as a subsystem charging an embedded fee
// does.
func (f *fixture) embeddedJournal(ctx context.Context, tx pgx.Tx, payer accounts.AccountID, base assets.AssetID, q StoredQuote) (ledger.TransactionID, econ.EffectID, error) {
	notional := q.Disclosure.NotionalAtReference
	qty := money.QuantityFromInt64(1_000_000)
	entries := []ledger.Entry{
		{Account: ledger.CustomerAccount(payer, ledger.CodeWallet, q.Charges.QuoteAsset), Side: ledger.Credit, Quantity: notional},
		{Account: ledger.CustomerAccount(payer, ledger.CodeTradingOutflow, q.Charges.QuoteAsset), Side: ledger.Debit, Quantity: notional},
		{Account: ledger.CustomerAccount(payer, ledger.CodeWallet, base), Side: ledger.Debit, Quantity: qty},
		{Account: ledger.CustomerAccount(payer, ledger.CodeTradingInflow, base), Side: ledger.Credit, Quantity: qty},
	}
	entries = append(entries, ChargeEntries(payer, q.Charges)...)
	ref := uuid.NewString()
	posted, err := f.ledger.Post(ctx, tx, ledger.Posting{Kind: ledger.KindSimTrade, IdempotencyKey: "trade:" + ref,
		Reference: ledger.FinancialEventReference{Type: "paper_fill", ID: ref}, EffectiveAt: f.clk.Now(), Entries: entries})
	if err != nil {
		return ledger.TransactionID{}, econ.EffectID{}, err
	}
	eff, _, err := f.effects.Begin(ctx, tx, econ.BeginRequest{Key: "trade:" + ref, Subsystem: econ.SubsystemOrders, Kind: "PAPER_FILL",
		AccountID: payer.String(), Request: map[string]string{"ref": ref}, PostsLedger: true, Actor: service})
	if err != nil {
		return ledger.TransactionID{}, econ.EffectID{}, err
	}
	if _, err := f.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{JournalTransactionID: &posted.TransactionID, Actor: service}); err != nil {
		return ledger.TransactionID{}, econ.EffectID{}, err
	}
	return posted.TransactionID, eff.ID, nil
}
