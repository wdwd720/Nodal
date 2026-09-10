package nativemarket

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Poster is the part of internal/ledger this package uses.
type Poster interface {
	Post(ctx context.Context, tx pgx.Tx, p ledger.Posting) (ledger.PostResult, error)
}

// Credits is the part of internal/credit this package uses.
type Credits interface {
	Consume(ctx context.Context, tx pgx.Tx, r credit.ConsumeRequest) ([]credit.Allocation, error)
	// RecordLot, not Issue: the trade posting has already moved the Credits as
	// one leg of a two-asset transaction, so this records where they came from
	// without posting a second movement.
	RecordLot(ctx context.Context, tx pgx.Tx, r credit.RecordLotRequest) (credit.Lot, error)
}

// Prices is the part of internal/valuation this package uses.
//
// A native market is Nodal's own venue, so this package is the SOURCE of these
// observations rather than a consumer of somebody else's feed. See reality.go
// for what that means for the timestamps.
type Prices interface {
	RecordPrice(ctx context.Context, q db.Querier, o valuation.PriceObservation) (valuation.RecordedPrice, error)
}

// Audit is the part of internal/audit this package uses. Appending inside the
// caller's transaction is the requirement, not a convenience: an audit row
// that can commit without its trade is a record that can disagree with what
// happened.
type Audit interface {
	Append(ctx context.Context, tx pgx.Tx, e audit.Event) (audit.Appended, error)
}

// Instruments is the part of internal/instruments this package uses.
//
// A Domain A market is registered as a SPOT_PAIR of (native asset / Credit)
// so the rest of the platform can NAME it. Without a row here the prediction
// ledger cannot reference a native market at all — predictions are keyed by
// instrument — and the Reality/Prediction machinery would stay Domain B and C
// only, which is the gap STAGE 15 exists to close.
type Instruments interface {
	CreateSpotPair(ctx context.Context, tx pgx.Tx, spec instruments.SpotPairSpec) (instruments.Instrument, error)
	GetBySpotPair(ctx context.Context, q db.Querier, base, quote assets.AssetID) (instruments.Instrument, error)
	TransitionStatus(ctx context.Context, tx pgx.Tx, id instruments.InstrumentID, ch instruments.StatusChange) (instruments.Instrument, error)
}

// Service runs native markets.
type Service struct {
	poster      Poster
	credits     Credits
	prices      Prices
	auditor     Audit
	instruments Instruments
	risk        Risk
	clk         clock.Clock
}

// NewService returns a Service. No argument may be nil.
//
// The price store and the audit writer are required rather than optional
// because a market that trades without publishing its price or its audit row
// is a market whose history cannot be verified afterwards, and "it was not
// configured" is not a thing anyone should be able to discover later. Tests
// that do not care still have to pass a store; internal/audit and
// internal/valuation both work against any pgx transaction.
func NewService(poster Poster, credits Credits, prices Prices, auditor Audit, insts Instruments, rk Risk, clk clock.Clock) *Service {
	if poster == nil || credits == nil || prices == nil || auditor == nil || insts == nil || rk == nil || clk == nil {
		panic("nativemarket: NewService requires a poster, a credit service, a price store, an audit writer, an instrument registry, a risk kernel and a clock")
	}
	return &Service{poster: poster, credits: credits, prices: prices, auditor: auditor, instruments: insts, risk: rk, clk: clk}
}

// Create opens a market and mints the asset's entire supply, once.
//
// Supply is minted here and nowhere else. That is what makes "no invisible
// supply changes" (PART XIII) a checkable property rather than a promise:
// there is exactly one mint path, it runs once per asset, and the fill trigger
// re-checks conservation on every subsequent trade.
func (s *Service) Create(ctx context.Context, tx pgx.Tx, r CreateRequest) (Market, error) {
	if err := r.Validate(); err != nil {
		return Market{}, err
	}
	if tx == nil {
		return Market{}, errs.New(errs.CodeInternal, "nativemarket: Create requires a transaction")
	}

	// The mint. Every unit that will ever exist is created in one posting:
	// the pool's inventory, the creator's allocation and the treasury's,
	// balanced against a single platform adjustment leg. There is no second
	// mint anywhere in the system.
	entries := []ledger.Entry{
		{Account: ledger.PlatformAccount(ledger.CodeMarketInventory, r.AssetID), Side: ledger.Debit, Quantity: r.PoolSupply},
	}
	if r.CreatorAllocation.IsPositive() {
		entries = append(entries, ledger.Entry{
			Account:  ledger.CustomerAccount(r.CreatorID, ledger.CodeNativeAssetBalance, r.AssetID),
			Side:     ledger.Debit,
			Quantity: r.CreatorAllocation,
		})
	}
	if r.TreasuryAllocation.IsPositive() {
		entries = append(entries, ledger.Entry{
			Account:  ledger.PlatformAccount(ledger.CodeMarketInventory, r.AssetID),
			Side:     ledger.Debit,
			Quantity: r.TreasuryAllocation,
		})
	}
	entries = append(entries, ledger.Entry{
		Account:  ledger.PlatformAccount(ledger.CodePlatformAdjustment, r.AssetID),
		Side:     ledger.Credit,
		Quantity: r.TotalSupply(),
	})

	post, err := s.poster.Post(ctx, tx, ledger.Posting{
		Kind:           ledger.KindNativeTrade,
		IdempotencyKey: r.IdempotencyKey,
		Reference:      ledger.FinancialEventReference{Type: "native_market_mint", ID: r.AssetID.String()},
		EffectiveAt:    r.EffectiveAt,
		Description:    "native asset supply minted at market creation",
		Entries:        entries,
		Metadata: map[string]any{
			"pool_supply":         r.PoolSupply.String(),
			"creator_allocation":  r.CreatorAllocation.String(),
			"treasury_allocation": r.TreasuryAllocation.String(),
		},
	})
	if err != nil {
		return Market{}, err
	}
	if post.Existing {
		return s.MarketByAsset(ctx, tx, r.AssetID)
	}

	m := Market{
		ID:            NewMarketID(),
		AssetID:       r.AssetID,
		CreditAssetID: r.CreditAssetID,
		Curve:         Curve{VirtualCreditReserve: r.VirtualCreditReserve, InitialAssetReserve: r.PoolSupply},
		Fees:          r.Fees,
		Status:        StatusPending,
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO native_markets
		   (id, asset_id, credit_asset_id, virtual_credit_reserve, initial_asset_reserve,
		    platform_fee_bps, creator_fee_bps, status)
		 VALUES ($1,$2,$3,$4::numeric,$5::numeric,$6,$7,'PENDING')
		 RETURNING created_at, updated_at`,
		m.ID, m.AssetID, m.CreditAssetID,
		m.Curve.VirtualCreditReserve.String(), m.Curve.InitialAssetReserve.String(),
		int(m.Fees.PlatformBPS), int(m.Fees.CreatorBPS)).Scan(&m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Market{}, errs.New(errs.CodeConflict, "this asset already has a market").
				WithField("asset_id", r.AssetID.String())
		}
		return Market{}, mapError(err)
	}
	m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()

	// The opening price, before anybody has traded. Without it a prediction
	// committed on a market's first day has no price at or before its own
	// commit instant and cannot be resolved at all -- the resolver would
	// rightly refuse rather than invent one.
	opening := SpotPrice(m.Curve, State{
		RealCreditReserve: money.Quantity{},
		AssetReserve:      m.Curve.InitialAssetReserve,
	})
	if err := s.publishPrice(ctx, tx, m, opening, r.EffectiveAt, "native_market:"+m.ID.String()); err != nil {
		return Market{}, err
	}
	if err := s.registerInstrument(ctx, tx, m, r); err != nil {
		return Market{}, err
	}
	return m, nil
}

// SetStatus moves a market's trading state, writing its transition row.
func (s *Service) SetStatus(ctx context.Context, tx pgx.Tx, marketID MarketID, to Status, reason string) (Market, error) {
	if strings.TrimSpace(reason) == "" {
		return Market{}, errs.New(errs.CodeValidationFailed, "a market status change requires a reason")
	}
	if !to.Valid() {
		return Market{}, errs.Newf(errs.CodeValidationFailed, "unknown market status %q", to)
	}
	m, err := s.marketForUpdate(ctx, tx, marketID)
	if err != nil {
		return Market{}, err
	}
	if m.Status == to {
		return m, nil
	}
	if !CanTransition(m.Status, to) {
		return Market{}, errs.Newf(errs.CodeInvalidStateTransition,
			"market cannot go %s -> %s", m.Status, to).WithField("market_id", marketID.String())
	}
	actorType, actorID := actorFrom(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO native_market_transitions (id, market_id, from_status, to_status, actor_type, actor_id, reason)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		NewTransitionID(), marketID, string(m.Status), string(to), actorType, actorID, reason); err != nil {
		return Market{}, mapError(err)
	}
	// The INSERT above IS the status change. 00746 revoked UPDATE on
	// native_markets from cp_app, and the trigger on native_market_transitions
	// has already written `status` and stamped `activated_at` from the row --
	// coalescing the stamp, because cp_native_market_curve_frozen raises NM003
	// if activated_at moves once set, which is exactly what the
	// `m.ActivatedAt == nil` guard here used to prevent.
	m.Status = to
	if to == StatusActive && m.ActivatedAt == nil {
		if err := tx.QueryRow(ctx, `SELECT activated_at FROM native_markets WHERE id = $1`, marketID).
			Scan(&m.ActivatedAt); err != nil {
			return Market{}, mapError(err)
		}
	}
	// Keep the platform's instrument registry agreeing with the venue. See
	// reality.go: two sources for "what may be traded" eventually disagree,
	// and the disagreement is found by something moving that should not have.
	if err := s.mirrorInstrumentStatus(ctx, tx, m, to, reason); err != nil {
		return Market{}, err
	}
	return m, nil
}

// Quote prices a hypothetical trade against current state and records it.
//
// The recorded quote is evidence of what the user was shown. It never supplies
// a price to Execute.
func (s *Service) Quote(ctx context.Context, tx pgx.Tx, r QuoteRequest) (Quote, error) {
	if err := r.Validate(); err != nil {
		return Quote{}, err
	}
	m, st, err := s.marketAndState(ctx, tx, r.MarketID)
	if err != nil {
		return Quote{}, err
	}
	if !m.Status.Accepts(r.Side) {
		return Quote{}, notTradable(m, r.Side)
	}
	fill, err := s.price(m, st, r.Side, r.Amount)
	if err != nil {
		return Quote{}, err
	}

	out := fill.AssetsOut
	if r.Side == Sell {
		out = fill.CreditsOut
	}
	qt := Quote{
		ID: NewQuoteID(), MarketID: m.ID, AccountID: r.AccountID, Side: r.Side,
		InputAmount: r.Amount, ExpectedOutput: out,
		PlatformFee: fill.PlatformFee, CreatorFee: fill.CreatorFee,
		SpotPriceBefore: fill.SpotBefore, EffectivePrice: fill.EffectivePrice,
		SlippageBPS:  fill.SlippageBPS(),
		StateVersion: st.Version,
		ExpiresAt:    s.clk.Now().Add(QuoteTTL).UTC(),
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO native_market_quotes
		   (id, market_id, account_id, side, input_amount, expected_output, platform_fee, creator_fee,
		    spot_price_before, effective_price, slippage_bps, state_version, expires_at)
		 VALUES ($1,$2,$3,$4,$5::numeric,$6::numeric,$7::numeric,$8::numeric,$9::numeric,$10::numeric,$11,$12,$13)
		 RETURNING created_at`,
		qt.ID, qt.MarketID, qt.AccountID, string(qt.Side), qt.InputAmount.String(), qt.ExpectedOutput.String(),
		qt.PlatformFee.String(), qt.CreatorFee.String(), qt.SpotPriceBefore.String(), qt.EffectivePrice.String(),
		int(qt.SlippageBPS), qt.StateVersion, qt.ExpiresAt).Scan(&qt.CreatedAt)
	if err != nil {
		return Quote{}, mapError(err)
	}
	qt.CreatedAt = qt.CreatedAt.UTC()
	return qt, nil
}

// price runs the curve for one side.
func (s *Service) price(m Market, st State, side Side, amount money.Quantity) (Fill, error) {
	if side == Buy {
		return QuoteBuy(m.Curve, st, amount, m.Fees)
	}
	return QuoteSell(m.Curve, st, amount, m.Fees)
}

func notTradable(m Market, side Side) error {
	return errs.Newf(errs.CodeAssetRestricted,
		"this market does not accept %s orders while it is %s", strings.ToLower(string(side)), m.Status).
		WithField("market_id", m.ID.String()).
		WithField("market_status", string(m.Status))
}

// Execute performs a trade.
//
// Everything happens in the caller's transaction, and the ordering is
// deliberate: the journal posting runs before the fill, so the ledger's own
// triggers (balance, negative balance, value-domain isolation) reject a bad
// trade before market state is touched at all.
func (s *Service) Execute(ctx context.Context, tx pgx.Tx, r ExecuteRequest) (ExecuteResult, error) {
	if err := r.Validate(); err != nil {
		return ExecuteResult{}, err
	}
	if tx == nil {
		return ExecuteResult{}, errs.New(errs.CodeInternal, "nativemarket: Execute requires a transaction")
	}
	if existing, owner, found, err := s.fillByIdempotencyKey(ctx, tx, r.IdempotencyKey); err != nil {
		return ExecuteResult{}, err
	} else if found {
		// Whose replay this is, for the reason recorded in internal/payout: the
		// key is globally unique and the boundary's idempotency record is per
		// actor, so another account's trade is what comes back otherwise --
		// and a market moves on every fill, so it is a trade at a price this
		// caller never saw (F-106).
		if owner != r.AccountID {
			return ExecuteResult{}, errs.New(errs.CodeInvalidIdempotencyReuse,
				"nativemarket: idempotency key belongs to another account").
				WithField("idempotency_key", r.IdempotencyKey)
		}
		return existing, nil
	}

	m, err := s.marketForUpdate(ctx, tx, r.MarketID)
	if err != nil {
		return ExecuteResult{}, err
	}
	st, err := s.state(ctx, tx, m.ID)
	if err != nil {
		return ExecuteResult{}, err
	}
	if !m.Status.Accepts(r.Side) {
		return ExecuteResult{}, notTradable(m, r.Side)
	}

	// Priced against CURRENT state. A quote is never a price source.
	fill, err := s.price(m, st, r.Side, r.Amount)
	if err != nil {
		return ExecuteResult{}, err
	}
	output := fill.AssetsOut
	if r.Side == Sell {
		output = fill.CreditsOut
	}
	if output.Cmp(r.MinOutput) < 0 {
		return ExecuteResult{}, errs.Newf(errs.CodeQuoteExpired,
			"the market moved: this order would return %s, below the %s minimum you set",
			output, r.MinOutput).
			WithField("market_id", m.ID.String()).
			WithField("would_return", output.String()).
			WithField("min_output", r.MinOutput.String())
	}

	creatorID, err := s.creatorOf(ctx, tx, m.AssetID)
	if err != nil {
		return ExecuteResult{}, err
	}

	// The risk kernel, before anything is posted (see risk.go). The settlement
	// compiler has already recorded that this route requires an evaluation;
	// this is the evaluation.
	if err := s.checkRisk(ctx, tx, m, r, fill, creatorID); err != nil {
		return ExecuteResult{}, err
	}

	post, err := s.postTrade(ctx, tx, m, r, fill, creatorID)
	if err != nil {
		return ExecuteResult{}, err
	}
	if err := s.moveCredits(ctx, tx, m, r, fill, creatorID, post.TransactionID); err != nil {
		return ExecuteResult{}, err
	}

	fillID := NewFillID()
	var quoteID any
	if r.QuoteID != nil {
		quoteID = *r.QuoteID
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO native_market_fills
		   (id, market_id, seq, account_id, side, credits_in, credits_out, assets_in, assets_out,
		    credits_to_pool, platform_fee, creator_fee, state_version_before,
		    real_credit_reserve_after, asset_reserve_after, quote_id, journal_transaction_id, idempotency_key)
		 VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8::numeric,$9::numeric,$10::numeric,
		         $11::numeric,$12::numeric,$13,$14::numeric,$15::numeric,$16,$17,$18)`,
		fillID, m.ID, st.Version+1, r.AccountID, string(r.Side),
		fill.CreditsIn.String(), fill.CreditsOut.String(), fill.AssetsIn.String(), fill.AssetsOut.String(),
		fill.CreditsToPool.String(), fill.PlatformFee.String(), fill.CreatorFee.String(),
		st.Version, fill.StateAfter.RealCreditReserve.String(), fill.StateAfter.AssetReserve.String(),
		quoteID, post.TransactionID, r.IdempotencyKey); err != nil {
		return ExecuteResult{}, mapError(err)
	}

	// Reality and proof, in this transaction with the trade (see reality.go).
	at := s.clk.Now()
	if err := s.publishPrice(ctx, tx, m, fill.SpotAfter, at, "native_market_fill:"+fillID.String()); err != nil {
		return ExecuteResult{}, err
	}
	if err := s.recordFill(ctx, tx, m, r, fill, fillID, post.TransactionID.String(), at); err != nil {
		return ExecuteResult{}, err
	}

	alerts, err := s.surveil(ctx, tx, m, r, fill, fillID, creatorID)
	if err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{FillID: fillID, MarketID: m.ID, Fill: fill, Alerts: alerts}, nil
}

// postTrade writes the journal transaction for a fill.
//
// Both sides balance per asset by construction. The Credit side moves the
// user's Credits to the pool, the platform and the creator; the asset side
// moves units between the pool's inventory and the user. Because the
// counterparty is the platform's own pool rather than an external venue, there
// are no contra accounts: this is a real transfer between two parties who are
// both on these books.
func (s *Service) postTrade(ctx context.Context, tx pgx.Tx, m Market, r ExecuteRequest, fill Fill, creatorID accounts.AccountID) (ledger.PostResult, error) {
	var (
		entries []ledger.Entry
		conv    valuedomain.ConversionKey
	)
	userCredit := ledger.CustomerAccount(r.AccountID, ledger.CodeCreditBalance, m.CreditAssetID)
	poolCredit := ledger.PlatformAccount(ledger.CodeMarketReserve, m.CreditAssetID)
	platformFeeAcct := ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, m.CreditAssetID)
	creatorCredit := ledger.CustomerAccount(creatorID, ledger.CodeCreditBalance, m.CreditAssetID)
	userAsset := ledger.CustomerAccount(r.AccountID, ledger.CodeNativeAssetBalance, m.AssetID)
	poolAsset := ledger.PlatformAccount(ledger.CodeMarketInventory, m.AssetID)

	switch r.Side {
	case Buy:
		conv = valuedomain.ConversionKey{From: valuedomain.InternalCredit, To: valuedomain.InternalNativeAsset}
		entries = []ledger.Entry{
			{Account: userCredit, Side: ledger.Credit, Quantity: fill.CreditsIn},
			{Account: poolCredit, Side: ledger.Debit, Quantity: fill.CreditsToPool},
			{Account: poolAsset, Side: ledger.Credit, Quantity: fill.AssetsOut},
			{Account: userAsset, Side: ledger.Debit, Quantity: fill.AssetsOut},
		}
	case Sell:
		conv = valuedomain.ConversionKey{From: valuedomain.InternalNativeAsset, To: valuedomain.InternalCredit}
		entries = []ledger.Entry{
			{Account: poolCredit, Side: ledger.Credit, Quantity: fill.CreditsToPool},
			{Account: userAsset, Side: ledger.Credit, Quantity: fill.AssetsIn},
			{Account: poolAsset, Side: ledger.Debit, Quantity: fill.AssetsIn},
		}
		if fill.CreditsOut.IsPositive() {
			entries = append(entries, ledger.Entry{Account: userCredit, Side: ledger.Debit, Quantity: fill.CreditsOut})
		}
	}
	if fill.PlatformFee.IsPositive() {
		entries = append(entries, ledger.Entry{Account: platformFeeAcct, Side: ledger.Debit, Quantity: fill.PlatformFee})
	}
	if fill.CreatorFee.IsPositive() {
		entries = append(entries, ledger.Entry{Account: creatorCredit, Side: ledger.Debit, Quantity: fill.CreatorFee})
	}

	return s.poster.Post(ctx, tx, ledger.Posting{
		Kind:           ledger.KindNativeTrade,
		IdempotencyKey: "native_fill:" + r.IdempotencyKey,
		Reference:      ledger.FinancialEventReference{Type: "native_market_fill", ID: r.IdempotencyKey},
		EffectiveAt:    r.EffectiveAt,
		CorrelationID:  r.CorrelationID,
		Description:    "native market " + strings.ToLower(string(r.Side)),
		Conversion:     &conv,
		Entries:        entries,
		Metadata: map[string]any{
			"market_id": m.ID.String(),
			"side":      string(r.Side),
		},
	})
}

// moveCredits keeps Credit provenance in step with the journal.
//
// On a buy the trader's lots are consumed; on a sell the proceeds are issued
// as a new lot with origin MARKET_TRADING_PROCEEDS, which the default payout
// policy forbids withdrawing. The creator's fee is issued with origin
// MARKET_CREATOR_EARNING — deliberately distinct from ordinary creator revenue
// because its source is speculative trading (PART LXXX).
func (s *Service) moveCredits(ctx context.Context, tx pgx.Tx, m Market, r ExecuteRequest, fill Fill, creatorID accounts.AccountID, journalTx ledger.TransactionID) error {
	ref := credit.Reference{Type: "native_market_fill", ID: r.IdempotencyKey}

	// The finality new Credits inherit. Proceeds and fees are funded by
	// whatever buyers paid in, and some of that may still be reversible, so
	// value leaving the pool is REVERSIBLE until something establishes
	// otherwise. It is spendable — which is what the product needs — and not
	// payout-eligible, which is the conservative half.
	const derived = valuedomain.FinalityReversible

	switch r.Side {
	case Buy:
		if _, err := s.credits.Consume(ctx, tx, credit.ConsumeRequest{
			AccountID:                r.AccountID,
			Quantity:                 fill.CreditsIn,
			JournalTxID:              journalTx,
			Reference:                ref,
			Reason:                   "native market buy",
			RequireSpendableFinality: true,
		}); err != nil {
			return err
		}
	case Sell:
		if fill.CreditsOut.IsPositive() {
			if _, err := s.credits.RecordLot(ctx, tx, credit.RecordLotRequest{
				AccountID:        r.AccountID,
				Quantity:         fill.CreditsOut,
				Origin:           valuedomain.OriginMarketTradingProceeds,
				Finality:         derived,
				Reference:        ref,
				FundingReference: &credit.Reference{Type: "native_market", ID: m.ID.String()},
				JournalTxID:      journalTx,
				Reason:           "proceeds of a native market sale",
			}); err != nil {
				return err
			}
		}
	}

	if fill.CreatorFee.IsPositive() {
		if _, err := s.credits.RecordLot(ctx, tx, credit.RecordLotRequest{
			AccountID:        creatorID,
			Quantity:         fill.CreatorFee,
			Origin:           valuedomain.OriginMarketCreatorEarning,
			Finality:         derived,
			Reference:        ref,
			FundingReference: &credit.Reference{Type: "native_market", ID: m.ID.String()},
			JournalTxID:      journalTx,
			Reason:           "native market creator fee",
		}); err != nil {
			return err
		}
	}
	return nil
}

func actorFrom(ctx context.Context) (string, string) {
	if p, ok := security.PrincipalFrom(ctx); ok && p.SubjectID != "" {
		return string(p.ActorType), p.SubjectID
	}
	return "SYSTEM", "nativemarket-service"
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	switch db.SQLState(err) {
	case "NM001":
		return errs.Wrap(err, errs.CodeInternal,
			"this trade would break the market's constant-product invariant and was refused by the database")
	case "NM002":
		return errs.Wrap(err, errs.CodeConflict,
			"the market moved while this order was being placed; price it again")
	case "NM003":
		return errs.Wrap(err, errs.CodeForbidden,
			"this market's curve and fees were frozen when it went live")
	case "NM004":
		return errs.Wrap(err, errs.CodeAssetRestricted, "this market is not accepting that trade")
	case "NM005":
		return errs.Wrap(err, errs.CodeInternal, "this trade would break supply conservation")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.New(errs.CodeNotFound, "native market not found")
	}
	if mapped := ledger.MapError(err); mapped != nil {
		return mapped
	}
	return errs.Wrap(err, errs.CodeInternal, "nativemarket: database error")
}
