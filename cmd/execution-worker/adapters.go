package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/quote"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
)

// quoteStore persists the executor's quote snapshot through internal/quote,
// which owns the quotes table and its validation. The executor holds only
// the provider-neutral projection, so this is the one place the two shapes
// meet.
type quoteStore struct {
	repo *quote.Repository
}

var _ settlement.QuoteStore = quoteStore{}

// Save records the snapshot and returns the quotes row id the order and the
// attempt reference.
func (s quoteStore) Save(ctx context.Context, tx pgx.Tx, q execution.QuoteSnapshot) (string, error) {
	row, err := toQuoteRow(q)
	if err != nil {
		return "", err
	}
	stored, err := s.repo.Record(ctx, tx, row)
	if err != nil {
		return "", err
	}
	return stored.ID.String(), nil
}

// toQuoteRow maps the executor's snapshot onto a quotes row. Every field is
// carried across without arithmetic: a stored quote must reload losslessly.
func toQuoteRow(q execution.QuoteSnapshot) (quote.Quote, error) {
	side := quote.SideBuy
	if q.Side == execution.SideSell {
		side = quote.SideSell
	}
	instrumentID, err := parseText[instruments.InstrumentID](q.InstrumentID)
	if err != nil {
		return quote.Quote{}, errs.Wrap(err, errs.CodeValidationFailed, "execution-worker: quote instrument id")
	}
	listingID, err := parseText[instruments.ListingID](q.VenueListingID)
	if err != nil {
		return quote.Quote{}, errs.Wrap(err, errs.CodeValidationFailed, "execution-worker: quote venue listing id")
	}
	out := quote.Quote{
		ID:                quote.NewQuoteID(),
		Provider:          q.Provider,
		ProviderRequestID: q.ProviderRequestID,
		InstrumentID:      instrumentID,
		VenueListingID:    listingID,
		Side:              side,
		InputAssetID:      q.InputAsset,
		InputQuantity:     q.InputQuantity,
		OutputAssetID:     q.OutputAsset,
		ExpectedOutput:    q.ExpectedOutput,
		MinimumOutput:     q.MinimumOutput,
		PriceImpactBPS:    q.PriceImpactBPS,
		SlippageBPS:       q.SlippageBPS,
		EstNetworkCost:    q.EstNetworkCost,
		EstVenueFee:       q.EstVenueFee,
		PlatformFee:       q.PlatformFee,
		PlatformFeeBPS:    q.PlatformFeeBPS,
		FeePolicyVersion:  q.FeePolicyVersion,
		ReceivedAt:        q.ReceivedAt.UTC(),
		ExpiresAt:         q.ExpiresAt.UTC(),
		// route_hash is defined by the quotes table as the canonical hash of
		// route_summary, so it is recomputed rather than copied: an adapter
		// that hashes its own formatting would store a digest nobody else can
		// reproduce. The provider's untouched response keeps its own digest in
		// raw_response_hash, so no evidence is lost.
		RouteHash:       quote.RouteHash(q.RouteSummary),
		RouteSummary:    q.RouteSummary,
		RawResponseRef:  q.RawResponseRef,
		RawResponseHash: q.RawResponseHash,
	}
	if q.IntentID != "" {
		iid, err := intent.ParseIntentID(q.IntentID)
		if err != nil {
			return quote.Quote{}, errs.Wrap(err, errs.CodeValidationFailed, "execution-worker: quote intent id")
		}
		out.IntentID = &iid
	}
	out.EstNetworkCostAssetID = optionalAsset(q.EstNetworkAsset)
	out.EstVenueFeeAssetID = optionalAsset(q.EstVenueFeeAsset)
	out.PlatformFeeAssetID = optionalAsset(q.PlatformFeeAsset)
	out.EffectivePrice = quote.NewEffectivePrice(
		q.EffectivePrice.Mantissa, q.EffectivePrice.Scale, side, q.InputAsset, q.OutputAsset, q.Provider, out.ReceivedAt)
	return out, nil
}

func optionalAsset(a assets.AssetID) *assets.AssetID {
	if a.IsZero() {
		return nil
	}
	return &a
}

// textParser is satisfied by the id alias types, which are all
// encoding.TextUnmarshaler through id.ID[K].
type textParser[T any] interface {
	*T
	UnmarshalText([]byte) error
}

func parseText[T any, P textParser[T]](s string) (T, error) {
	var v T
	if err := P(&v).UnmarshalText([]byte(s)); err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}

// intentReader answers the executor's question "whose intent is this plan
// for, and which wallet signs it". It reads only committed rows: the
// executor must never learn an account or a wallet from anywhere but the
// database.
type intentReader struct {
	repo *intent.Repository
}

var _ settlement.IntentReader = intentReader{}

const walletSQL = `
SELECT id::text, address, chain, provider
  FROM wallets
 WHERE account_id = $1::uuid AND status = 'ACTIVE'
 ORDER BY created_at
 LIMIT 1`

// Intent loads the IntentRef of a plan's intent.
func (r intentReader) Intent(ctx context.Context, q db.Querier, intentID string) (settlement.IntentRef, error) {
	iid, err := intent.ParseIntentID(intentID)
	if err != nil {
		return settlement.IntentRef{}, errs.Wrap(err, errs.CodeValidationFailed, "execution-worker: intent id")
	}
	ti, err := r.repo.Get(ctx, q, iid)
	if err != nil {
		return settlement.IntentRef{}, err
	}
	ref := settlement.IntentRef{
		ID:            ti.ID.String(),
		AccountID:     ti.AccountID,
		ActorType:     ti.ActorType,
		ActorID:       ti.ActorID,
		InstrumentID:  ti.InstrumentID.String(),
		Mode:          execution.Mode(ti.Mode),
		CorrelationID: ti.CorrelationID,
		ReservationID: ti.Links.ReservationID,
	}
	if ti.AgentID != nil {
		ref.AgentID = *ti.AgentID
	}
	if ti.StrategyVersionID != nil {
		ref.StrategyVersionID = *ti.StrategyVersionID
	}
	err = q.QueryRow(ctx, walletSQL, ti.AccountID).Scan(&ref.WalletID, &ref.WalletAddress, &ref.WalletChain, &ref.WalletProvider)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return settlement.IntentRef{}, errs.New(errs.CodeNotFound, "execution-worker: the account has no active wallet").
			WithField("account_id", ti.AccountID)
	case err != nil:
		return settlement.IntentRef{}, errs.Wrap(err, errs.CodeInternal, "execution-worker: load wallet")
	}
	if ref.ActorType == "" {
		ref.ActorType = security.ActorSystem
	}
	return ref, nil
}

// capitalEmitter forwards capital's domain events onto the transactional
// outbox. Topics that are not registered are dropped rather than failing the
// financial transaction that produced them: an unregistered topic is a
// producer bug, not a reason to refuse a reservation.
type capitalEmitter struct {
	outbox *event.Outbox
	clk    clock.Clock
}

var _ capital.Emitter = capitalEmitter{}

// Emit enqueues one capital event in the caller's transaction.
func (e capitalEmitter) Emit(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	spec, ok := event.Lookup(event.Topic(topic))
	if !ok {
		return nil
	}
	ev, ok := payload.(capital.ReservationEvent)
	if !ok || ev.ReservationID == "" {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "execution-worker: encode capital event")
	}
	return e.outbox.Enqueue(ctx, tx, topic, event.Envelope{
		ID:            event.NewEventID().String(),
		Type:          topic,
		SchemaVersion: spec.SchemaVersion,
		Source:        "capital",
		AggregateType: event.AggregateReservation,
		AggregateID:   ev.ReservationID,
		OccurredAt:    e.clk.Now().UTC(),
		Payload:       body,
	})
}
