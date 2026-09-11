package credit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Poster is the part of internal/ledger this package uses.
type Poster interface {
	Post(ctx context.Context, tx pgx.Tx, p ledger.Posting) (ledger.PostResult, error)
}

// Service issues, consumes, restores and reverses Credits.
type Service struct {
	poster Poster
	clk    clock.Clock

	// assetID is the single CREDIT asset, resolved once and cached. There can
	// only ever be one (migration 00711's partial unique index), so caching it
	// cannot go stale in a way that matters.
	assetID assets.AssetID
	// decimals is that asset's scale, cached beside it and for the same
	// reason: a lot's quantity is a count of base units, and an asset's
	// decimals is immutable once registered.
	decimals uint8
}

// NewService returns a Service. Neither argument may be nil.
func NewService(poster Poster, clk clock.Clock) *Service {
	if poster == nil {
		panic("credit: NewService requires a ledger poster")
	}
	if clk == nil {
		panic("credit: NewService requires a clock")
	}
	return &Service{poster: poster, clk: clk}
}

// AssetID returns the Credit asset, resolving it on first use.
//
// A deployment with no Credit asset is a configuration error, not a condition
// to work around: it fails with NOT_FOUND rather than creating one, because
// minting the unit of account is a deliberate act with a migration behind it.
func (s *Service) AssetID(ctx context.Context, q db.Querier) (assets.AssetID, error) {
	if !s.assetID.IsZero() {
		return s.assetID, nil
	}
	var got assets.AssetID
	err := q.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&got)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return got, errs.New(errs.CodeNotFound,
				"no Credit asset is registered; the internal economy is not provisioned in this environment")
		}
		return got, errs.Wrap(err, errs.CodeInternal, "credit: resolve credit asset")
	}
	s.assetID = got
	return got, nil
}

// AssetDecimals returns the scale of the registered CREDIT asset.
//
// Every Credit figure this package stores, returns and posts is an integer
// count of that asset's BASE UNITS. Anything that has to turn a count of whole
// Credits into one of those figures -- a pricing policy, a payout quote, a
// browser rendering a balance -- needs this number, and every place that
// assumed it was six instead of reading it was a place that could be wrong
// about money (F-151). It is resolved once and cached, because an asset's
// decimals is immutable after registration (migration 00711).
func (s *Service) AssetDecimals(ctx context.Context, q db.Querier) (uint8, error) {
	if !s.assetID.IsZero() && s.decimals > 0 {
		return s.decimals, nil
	}
	var (
		got      assets.AssetID
		decimals uint8
	)
	err := q.QueryRow(ctx, `SELECT id, decimals FROM assets WHERE kind = 'CREDIT'`).Scan(&got, &decimals)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errs.New(errs.CodeNotFound,
				"no Credit asset is registered; the internal economy is not provisioned in this environment")
		}
		return 0, errs.Wrap(err, errs.CodeInternal, "credit: resolve credit asset scale")
	}
	s.assetID, s.decimals = got, decimals
	return decimals, nil
}

// Issue mints Credits into an account and records where they came from.
//
// It posts the journal transaction and writes the lot in the caller's
// transaction, so either both land or neither does. The ledger's own triggers
// enforce that the movement balances; migration 00711 enforces that the lot
// ties back to this exact posting.
func (s *Service) Issue(ctx context.Context, tx pgx.Tx, r IssueRequest) (Lot, error) {
	if err := r.Validate(); err != nil {
		return Lot{}, err
	}
	if tx == nil {
		return Lot{}, errs.New(errs.CodeInternal, "credit: Issue requires a transaction")
	}
	assetID, err := s.AssetID(ctx, tx)
	if err != nil {
		return Lot{}, err
	}
	res, err := s.poster.Post(ctx, tx, ledger.Posting{
		Kind:           ledger.KindCreditIssued,
		IdempotencyKey: r.IdempotencyKey,
		Reference:      ledger.FinancialEventReference{Type: r.Reference.Type, ID: r.Reference.ID},
		EffectiveAt:    r.EffectiveAt,
		Description:    r.Reason,
		CorrelationID:  r.CorrelationID,
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(r.AccountID, ledger.CodeCreditBalance, assetID), Side: ledger.Debit, Quantity: r.Quantity},
			{Account: ledger.CustomerAccount(r.AccountID, ledger.CodeCreditIssuance, assetID), Side: ledger.Credit, Quantity: r.Quantity},
		},
		Metadata: map[string]any{
			"credit_origin":   string(r.Origin),
			"credit_finality": string(r.Finality),
		},
	})
	if err != nil {
		return Lot{}, err
	}
	if res.Existing {
		// The posting was a replay. The lot it created is the answer; creating
		// a second lot for the same issuance would double the provenance while
		// the balance stayed put.
		return s.lotByJournalTx(ctx, tx, res.TransactionID)
	}
	return s.RecordLot(ctx, tx, RecordLotRequest{
		AccountID:        r.AccountID,
		Quantity:         r.Quantity,
		Origin:           r.Origin,
		Finality:         r.Finality,
		Parents:          r.Parents,
		Reference:        r.Reference,
		FundingReference: r.FundingReference,
		JournalTxID:      res.TransactionID,
		Reason:           r.Reason,
	})
}

// RecordLot creates a provenance lot for units that a journal transaction the
// caller ALREADY POSTED moved into the account.
//
// It exists because Issue does two things — post the movement and record where
// the units came from — and there are flows where the movement is one leg of a
// larger transaction that has already been written. A native-market sale is the
// motivating case: the trade posting already credits the seller's balance as
// part of a six-entry, two-asset transaction, and calling Issue there would
// post a SECOND transaction and credit them twice. Splitting the two halves
// makes that mistake impossible to make silently.
//
// The database still refuses a lot whose journal transaction did not touch this
// account and asset (SQLSTATE CR004), so RecordLot cannot be used to invent
// provenance for units nobody moved.
func (s *Service) RecordLot(ctx context.Context, tx pgx.Tx, r RecordLotRequest) (Lot, error) {
	// The parents decide the finality before the request is validated against
	// it, because a DERIVED lot's finality is not the caller's to state: it is
	// the least final finality among the lots that funded it, and a caller that
	// declared proceeds SETTLED on the strength of a reversible purchase would
	// be the defect F-230 was (D-124).
	if len(r.Parents) > 0 {
		// The parents decide outright. A caller's Finality is the fallback for
		// a mint with no parents -- a grant, or a sale out of a pool whose
		// record could not account for what left it -- and taking the LESS
		// final of the two would be wrong in the direction that matters:
		// UNFUNDED is more final than REVERSIBLE, so proceeds funded by a
		// promotional grant would stay stranded exactly as they were (F-230).
		r.Finality = DerivedFinality(r.Parents)
	}
	if err := r.Validate(); err != nil {
		return Lot{}, err
	}
	if tx == nil {
		return Lot{}, errs.New(errs.CodeInternal, "credit: RecordLot requires a transaction")
	}
	assetID, err := s.AssetID(ctx, tx)
	if err != nil {
		return Lot{}, err
	}
	actorType, actorID := actorFrom(ctx)

	lot := Lot{
		ID:                NewLotID(),
		AccountID:         r.AccountID,
		AssetID:           assetID,
		Origin:            r.Origin,
		Quantity:          r.Quantity,
		Remaining:         r.Quantity,
		Finality:          r.Finality,
		InitialFinality:   r.Finality,
		FundingReference:  r.FundingReference,
		JournalTxID:       r.JournalTxID,
		IssuedByActorType: actorType,
		IssuedByActorID:   actorID,
		Reason:            r.Reason,
	}
	var refType, refID any
	if r.FundingReference != nil {
		refType, refID = r.FundingReference.Type, r.FundingReference.ID
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO credit_lots
		   (id, account_id, asset_id, origin, initial_finality, quantity,
		    funding_reference_type, funding_reference_id, journal_transaction_id,
		    issued_by_actor_type, issued_by_actor_id, reason)
		 VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8,$9,$10,$11,$12)
		 RETURNING created_at`,
		lot.ID, lot.AccountID, lot.AssetID, string(lot.Origin), string(lot.InitialFinality),
		lot.Quantity.String(), refType, refID, lot.JournalTxID,
		lot.IssuedByActorType, lot.IssuedByActorID, lot.Reason).Scan(&lot.CreatedAt)
	if err != nil {
		return Lot{}, mapError(err)
	}
	lot.CreatedAt = lot.CreatedAt.UTC()
	lot.Version = 1
	// A lot with no parents has its own origin as its floor, which is what
	// 00816's cp_credit_lot_open just wrote.
	lot.OriginFloor = lot.Origin
	if err := s.recordParents(ctx, tx, lot.ID, r.Parents); err != nil {
		return Lot{}, err
	}
	if len(r.Parents) > 0 {
		// Read back rather than recomputed here. The floor is lowered by a
		// trigger as each parent row lands, for the same reason the finality
		// column is trigger-written: it is not the application's to assert, and
		// a Go copy of the rule is a second implementation that eventually
		// disagrees with the one the database enforces (D-131).
		var roots []string
		if err := tx.QueryRow(ctx,
			`SELECT origin_floor, root_origins FROM credit_lot_state WHERE lot_id = $1`, lot.ID).
			Scan(&lot.OriginFloor, &roots); err != nil {
			return Lot{}, mapError(err)
		}
		lot.RootOrigins = originsOf(roots)
	}
	return lot, nil
}

// Consume allocates qty across the account's open lots in consumption order
// and records a CONSUME event against each one.
//
// The lots are locked FOR UPDATE in a deterministic order, so concurrent
// spenders queue rather than deadlock and the "exactly N of 100 concurrent
// spends succeed" property holds here as well as in the journal.
func (s *Service) Consume(ctx context.Context, tx pgx.Tx, r ConsumeRequest) ([]Allocation, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, errs.New(errs.CodeInternal, "credit: Consume requires a transaction")
	}
	assetID, err := s.AssetID(ctx, tx)
	if err != nil {
		return nil, err
	}
	lots, err := s.openLotsForUpdate(ctx, tx, r.AccountID, assetID,
		r.RequireSpendableFinality, r.RequirePayoutFinality, r.AllowedOrigins, r.LotIDs)
	if err != nil {
		return nil, err
	}
	// The lot restriction, asserted on the way out as well as applied on the way
	// in. The filter is a parameter of one constant statement and cannot be
	// bypassed by a caller, so this can only fire if the statement and this
	// field come apart -- which is exactly the kind of drift that let a payout
	// consume by origin while its decision was made per lot (D-136, F-270). It
	// refuses; it does not correct.
	if len(r.LotIDs) > 0 {
		allowed := make(map[LotID]bool, len(r.LotIDs))
		for _, l := range r.LotIDs {
			allowed[l] = true
		}
		for _, lot := range lots {
			if !allowed[lot.ID] {
				return nil, errs.Newf(errs.CodeInternal,
					"credit: consume selected lot %s, which is outside the %d lots the caller allowed",
					lot.ID, len(r.LotIDs))
			}
		}
	}

	remaining := r.Quantity
	var allocs []Allocation
	for _, lot := range lots {
		if !remaining.IsPositive() {
			break
		}
		take := lot.Remaining.Min(remaining)
		if !take.IsPositive() {
			continue
		}
		ev, err := s.appendEvent(ctx, tx, lotEvent{
			lotID:       lot.ID,
			kind:        "CONSUME",
			delta:       &take,
			reference:   r.Reference,
			journalTxID: &r.JournalTxID,
			reason:      r.Reason,
		})
		if err != nil {
			return nil, err
		}
		allocs = append(allocs, Allocation{
			LotID: lot.ID, Origin: lot.Origin, OriginFloor: lot.OriginFloor,
			Finality: lot.Finality, Quantity: take, EventID: ev,
		})
		remaining = remaining.Sub(take)
	}
	if remaining.IsPositive() {
		// Deliberately does not say how much IS available: the caller has the
		// balance API for that, and an error path that reports a balance is a
		// balance oracle for anyone who can provoke it.
		return nil, errs.Newf(errs.CodeInsufficientBuyingPower,
			"insufficient Credits with the required provenance to cover %s", r.Quantity).
			WithField("account_id", r.AccountID.String()).
			WithField("requested", r.Quantity.String()).
			WithField("shortfall", remaining.String())
	}
	return allocs, nil
}

// Restore returns previously consumed units to the exact lots they came from.
func (s *Service) Restore(ctx context.Context, tx pgx.Tx, r RestoreRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if tx == nil {
		return errs.New(errs.CodeInternal, "credit: Restore requires a transaction")
	}
	// Sorted so concurrent restores touching overlapping lots take the row
	// locks in the same order.
	allocs := append([]Allocation(nil), r.Allocations...)
	sort.Slice(allocs, func(i, j int) bool { return allocs[i].LotID.String() < allocs[j].LotID.String() })
	for _, a := range allocs {
		qty := a.Quantity
		if _, err := s.appendEvent(ctx, tx, lotEvent{
			lotID:       a.LotID,
			kind:        "RESTORE",
			delta:       &qty,
			reference:   r.Reference,
			journalTxID: &r.JournalTxID,
			reason:      r.Reason,
		}); err != nil {
			return err
		}
	}
	return nil
}

// SetFinality moves a lot's funding finality.
//
// The legal transitions are enforced twice: once by
// valuedomain.CanTransitionFinality for the operator-facing error, and once by
// the database (SQLSTATE CR003) for everything that does not come through
// here. REVERSED is terminal on both sides.
func (s *Service) SetFinality(ctx context.Context, tx pgx.Tx, lotID LotID, to valuedomain.FundingFinality, ref Reference, reason string) error {
	if tx == nil {
		return errs.New(errs.CodeInternal, "credit: SetFinality requires a transaction")
	}
	if lotID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "credit: lot id is required")
	}
	if !to.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "credit: unknown funding finality %q", to)
	}
	if !ref.Valid() {
		return errs.New(errs.CodeValidationFailed, "credit: finality change requires a reference")
	}
	if strings.TrimSpace(reason) == "" {
		return errs.New(errs.CodeValidationFailed, "credit: finality change requires a reason")
	}
	if err := lockLot(ctx, tx, lotID); err != nil {
		return err
	}
	var from valuedomain.FundingFinality
	err := tx.QueryRow(ctx, `SELECT finality FROM credit_lot_state WHERE lot_id = $1`, lotID).Scan(&from)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errs.New(errs.CodeNotFound, "credit lot not found").WithField("lot_id", lotID.String())
		}
		return mapError(err)
	}
	if from == to {
		return nil
	}
	if !valuedomain.CanTransitionFinality(from, to) {
		return errs.Newf(errs.CodeInvalidStateTransition,
			"credit lot funding finality cannot go %s -> %s", from, to).
			WithField("lot_id", lotID.String()).
			WithField("from", string(from)).
			WithField("to", string(to))
	}
	_, err = s.appendEvent(ctx, tx, lotEvent{
		lotID: lotID, kind: "FINALITY", toFinality: &to, reference: ref, reason: reason,
	})
	return err
}

// actorFrom reads the acting principal from the context. Background workers
// with no principal are recorded as the system actor rather than being
// refused: issuing a promotional grant from a scheduled job is legitimate, and
// an unattributed row would be worse than an attributed system one.
func actorFrom(ctx context.Context) (string, string) {
	if p, ok := security.PrincipalFrom(ctx); ok {
		return string(p.ActorType), p.SubjectID
	}
	return "SYSTEM", "credit-service"
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	switch db.SQLState(err) {
	case "CR001":
		return errs.Wrap(err, errs.CodeConflict, "credit lot does not hold the units this change requires")
	case "CR003":
		return errs.Wrap(err, errs.CodeInvalidStateTransition, "illegal credit lot funding finality transition")
	case "CR004":
		return errs.Wrap(err, errs.CodeValidationFailed, "credit lot is not backed by the journal transaction it names")
	}
	if mapped := ledger.MapError(err); mapped != nil {
		return mapped
	}
	return errs.Wrap(err, errs.CodeInternal, fmt.Sprintf("credit: %v", err))
}
