package treasury

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// FundRequest injects simulated house capital into a house book.
type FundRequest struct {
	HouseAccountID econ.HouseAccountID
	Asset          assets.AssetID
	Quantity       money.Quantity
	// Mode is PAPER (default) or SANDBOX: the mode PRINCIPAL_TRADING is
	// authorized in.
	Mode           gates.Mode
	IdempotencyKey string
	Reason         string
	Actor          econ.Actor
}

// Funding is a stored house funding.
type Funding struct {
	ID                   FundingID
	HouseAccountID       econ.HouseAccountID
	Asset                assets.AssetID
	Quantity             money.Quantity
	CostBasis            money.Quantity
	Mark                 *money.Price
	EffectID             econ.EffectID
	JournalTransactionID ledger.TransactionID
	EvidenceRef          string
	FundedAt             time.Time
	Action               Action
}

// Fund posts HOUSE_FUNDING (H: Dr HOUSE_INVENTORY / Cr HOUSE_CAPITAL) as an
// exactly-once effect. Only an operator funds a house book. Funding in the
// valuation asset is cash; any other asset enters the position register at
// its fresh mark (rounded up), so funding never creates a gain, and a funding
// without a fresh mark is refused.
func (s *Service) Fund(ctx context.Context, tx pgx.Tx, r FundRequest) (Funding, error) {
	if err := validKey(r.IdempotencyKey); err != nil {
		return Funding{}, err
	}
	if !r.Quantity.IsPositive() {
		return Funding{}, errs.New(errs.CodeValidationFailed, "treasury: a funding quantity must be positive")
	}
	mode := r.Mode
	if mode == "" {
		mode = gates.ModePaper
	}
	// Refused before the book is locked (supply.go).
	if err := refuseSupplyControlledFunding(ctx, tx, r.Asset); err != nil {
		return Funding{}, err
	}
	book, err := s.lockBook(ctx, tx, r.HouseAccountID)
	if err != nil {
		return Funding{}, err
	}
	sp, err := s.currentSigningPolicy(ctx, tx, book.HouseAccountID)
	if err != nil {
		return Funding{}, err
	}
	if err := sp.Authorize(r.Actor, ActionFund); err != nil {
		return Funding{}, err
	}
	reqHash, err := econ.RequestHash(map[string]any{"asset": r.Asset.String(), "quantity": r.Quantity.String(), "mode": string(mode)})
	if err != nil {
		return Funding{}, err
	}
	if prior, found, err := s.actionByKey(ctx, tx, book.HouseAccountID, r.IdempotencyKey); err != nil {
		return Funding{}, err
	} else if found {
		if err := sameRequest(prior, ActionFund, reqHash); err != nil {
			return Funding{}, err
		}
		return s.fundingByID(ctx, tx, *prior.FundingID, prior)
	}
	if err := s.requireCapability(ctx, tx, gates.PrincipalTrading, mode); err != nil {
		return Funding{}, err
	}
	asset, err := s.assets.Get(ctx, tx, r.Asset)
	if err != nil {
		return Funding{}, err
	}
	if asset.Kind != assets.KindSimulated || asset.ValueDomain != valuedomain.Simulated {
		return Funding{}, errs.New(errs.CodeForbidden, "treasury: a house book holds only SIMULATED value")
	}
	if _, err := s.syncLocked(ctx, tx, book); err != nil {
		return Funding{}, err
	}
	now := s.clk.Now().UTC()
	f := Funding{ID: id.New[fundingKind](), HouseAccountID: book.HouseAccountID, Asset: asset.ID, Quantity: r.Quantity, FundedAt: now}
	if asset.ID == book.ValuationAsset {
		f.CostBasis = r.Quantity
	} else {
		pv, found, err := s.currentPolicy(ctx, tx, book.HouseAccountID)
		if err != nil {
			return Funding{}, err
		}
		mark, err := s.markOf(ctx, tx, asset.ID, book.ValuationAsset, s.maxMarkAge(pv, found), now)
		if err != nil {
			return Funding{}, err
		}
		if mark == nil {
			return Funding{}, rejection(errs.CodeStaleMarketData, ReasonMarkUnavailable,
				"non-cash funding enters the register at a fresh mark, and there is none")
		}
		valuationAsset, err := s.assets.Get(ctx, tx, book.ValuationAsset)
		if err != nil {
			return Funding{}, err
		}
		if f.CostBasis, err = money.Notional(r.Quantity, asset.Decimals, *mark, valuationAsset.Decimals, FundingCostRounding); err != nil {
			return Funding{}, errs.Wrap(err, errs.CodeInternal, "treasury: funding cost basis")
		}
		f.Mark = mark
	}
	key := "treasury:fund:" + book.HouseAccountID.String() + ":" + r.IdempotencyKey
	eff, existed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: key, Subsystem: econ.SubsystemTreasury, Kind: "HOUSE_FUNDING",
		Request: map[string]any{"house_account_id": book.HouseAccountID.String(), "asset": asset.ID.String(),
			"quantity": r.Quantity.String(), "cost_basis": f.CostBasis.String()},
		PostsLedger: true, Actor: r.Actor,
	})
	if err != nil {
		return Funding{}, err
	}
	if existed {
		return Funding{}, errs.New(errs.CodeConflict, "treasury: this funding's effect was already claimed").WithField("effect_id", eff.ID.String())
	}
	posted, err := s.ledger.Post(postingContext(ctx, r.Actor), tx, ledger.Posting{
		Kind: ledger.KindHouseFunding, IdempotencyKey: key,
		Reference:   ledger.FinancialEventReference{Type: "treasury_funding", ID: f.ID.String()},
		EffectiveAt: now, Description: "simulated house capital injected into a house book",
		Entries: []ledger.Entry{
			{Account: book.account(ledger.CodeHouseInventory, asset.ID), Side: ledger.Debit, Quantity: r.Quantity},
			{Account: book.account(ledger.CodeHouseCapital, asset.ID), Side: ledger.Credit, Quantity: r.Quantity},
		},
		Metadata: map[string]any{"ledger_segment": LedgerSegment, "house_account_id": book.HouseAccountID.String()},
	})
	if err != nil {
		return Funding{}, err
	}
	f.JournalTransactionID = posted.TransactionID
	doc := map[string]any{
		"kind": "treasury.funding.v1", "house_account_id": book.HouseAccountID.String(), "asset": asset.ID.String(),
		"quantity": r.Quantity.String(), "cost_basis": f.CostBasis.String(), "journal_transaction_id": posted.TransactionID.String(),
		"audit_tags": auditTags(book), "reason": r.Reason,
	}
	var mantissa, scale, observed any
	if f.Mark != nil {
		doc["mark"] = MarkRecord{Mantissa: f.Mark.Mantissa, Scale: f.Mark.Scale, Source: f.Mark.Source, At: f.Mark.At.UTC()}
		mantissa, scale, observed = f.Mark.Mantissa.String(), f.Mark.Scale, f.Mark.At.UTC()
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "treasury/" + book.HouseAccountID.String() + "/funding/" + f.ID.String(), Document: doc})
	if err != nil {
		return Funding{}, err
	}
	f.EvidenceRef = ev.URI
	applied, err := s.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{
		JournalTransactionID: &posted.TransactionID, Evidence: &ev, Actor: r.Actor, Reason: "house funding posted",
	})
	if err != nil {
		return Funding{}, err
	}
	f.EffectID = applied.ID
	if _, err := tx.Exec(ctx, `INSERT INTO treasury_fundings (id, house_account_id, asset_id, quantity, cost_basis, mark_mantissa,
		mark_scale, mark_observed_at, idempotency_key, effect_id, journal_transaction_id, evidence_ref, evidence_sha256, funded_at)
		VALUES ($1,$2,$3,$4::numeric,$5::numeric,$6::numeric,$7,$8,$9,$10,$11,$12,$13,$14)`,
		f.ID, book.HouseAccountID, asset.ID, r.Quantity.String(), f.CostBasis.String(), mantissa, scale, observed,
		r.IdempotencyKey, applied.ID, posted.TransactionID, ev.URI, ev.SHA256, now); err != nil {
		return Funding{}, dbErr("record funding", err)
	}
	if asset.ID != book.ValuationAsset {
		pos, err := positionOf(ctx, tx, book.HouseAccountID, asset.ID)
		if err != nil {
			return Funding{}, err
		}
		m, err := pos.Increase(r.Quantity, f.CostBasis, money.Quantity{})
		if err != nil {
			return Funding{}, err
		}
		fid := f.ID
		if _, err := insertEvent(ctx, tx, book.HouseAccountID, EventFunding, asset.ID, m,
			eventSource{funding: &fid, journal: posted.TransactionID}, now); err != nil {
			return Funding{}, err
		}
	}
	a := Action{
		ID: id.New[actionKind](), HouseAccountID: book.HouseAccountID, Kind: ActionFund, Outcome: OutcomeApplied, Actor: r.Actor,
		SigningPolicyVersion: sp.Version, IdempotencyKey: r.IdempotencyKey, RequestHash: reqHash, FundingID: &f.ID,
		EffectID: &f.EffectID, OccurredAt: now,
	}
	if err := s.recordAction(ctx, tx, book, &a, r.Reason, map[string]any{
		"funding_id": f.ID.String(), "asset": asset.ID.String(), "quantity": r.Quantity.String(), "cost_basis": f.CostBasis.String(),
	}); err != nil {
		return Funding{}, err
	}
	f.Action = a
	return f, nil
}

func (s *Service) fundingByID(ctx context.Context, tx pgx.Tx, fid FundingID, a Action) (Funding, error) {
	f := Funding{ID: fid, Action: a}
	var qty, cost string
	var mantissa *string
	var scale *int32
	var observed *time.Time
	err := tx.QueryRow(ctx, `SELECT house_account_id, asset_id, quantity::text, cost_basis::text, mark_mantissa::text, mark_scale,
		mark_observed_at, effect_id, journal_transaction_id, evidence_ref, funded_at FROM treasury_fundings WHERE id = $1`, fid).
		Scan(&f.HouseAccountID, &f.Asset, &qty, &cost, &mantissa, &scale, &observed, &f.EffectID, &f.JournalTransactionID,
			&f.EvidenceRef, &f.FundedAt)
	if err != nil {
		return Funding{}, notFound(err, "funding", fid.String())
	}
	if f.Quantity, err = money.ScanQuantity(qty); err == nil {
		f.CostBasis, err = money.ScanQuantity(cost)
	}
	if err != nil {
		return Funding{}, errs.Wrap(err, errs.CodeInternal, "treasury: stored funding amounts")
	}
	if mantissa != nil && scale != nil && observed != nil {
		m, err := money.ScanQuantity(*mantissa)
		if err != nil {
			return Funding{}, errs.Wrap(err, errs.CodeInternal, "treasury: stored funding mark")
		}
		f.Mark = &money.Price{Mantissa: m, Scale: *scale, QuoteAsset: "", At: observed.UTC()}
	}
	f.FundedAt = f.FundedAt.UTC()
	return f, nil
}
