package predmarket

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// MarketSpec defines a market. It is immutable once the market exists: its
// canonical hash is stored, and creating the same symbol again is a replay
// only when the definition is identical.
type MarketSpec struct {
	// Symbol names the market: 1..32 of A-Z 0-9 _ -, starting alphanumeric.
	Symbol   string
	Question string
	// Outcomes are 2..64 distinct outcome symbols (1..24 of A-Z 0-9 _ -).
	Outcomes []string
	// CollateralAssetID is a USD-pegged, six-decimal SIMULATED asset: the
	// sandbox dollar.
	CollateralAssetID assets.AssetID
	Mode              gates.Mode
	// OpenAt and CloseAt bound trading. CloseAt is the instruments' expiry.
	OpenAt  time.Time
	CloseAt time.Time
	// DisputeWindow is how long after a resolution settlement must wait for
	// disputes; zero settles at once.
	DisputeWindow time.Duration
	// OracleSource and OraclePublicKey name the only oracle whose reports
	// can resolve the market.
	OracleSource    string
	OraclePublicKey ed25519.PublicKey
}

// Validate checks the definition without touching the database.
func (sp MarketSpec) Validate() error {
	var p []string
	if !marketSymbolPattern.MatchString(sp.Symbol) {
		p = append(p, "symbol must be 1..32 of A-Z 0-9 _ -, starting alphanumeric")
	}
	if q := sp.Question; q == "" || len(q) > 500 || !utf8.ValidString(q) || strings.TrimSpace(q) != q || strings.ContainsRune(q, 0) {
		p = append(p, "question must be 1..500 bytes of trimmed text")
	}
	if n := len(sp.Outcomes); n < MinOutcomes || n > MaxOutcomes {
		p = append(p, "a market has 2..64 outcomes")
	}
	seen := map[string]bool{}
	for _, o := range sp.Outcomes {
		if !outcomeSymbolPattern.MatchString(o) || seen[o] {
			p = append(p, "outcomes must be distinct symbols of 1..24 of A-Z 0-9 _ -")
			break
		}
		seen[o] = true
	}
	if sp.CollateralAssetID.IsZero() {
		p = append(p, "collateral asset required")
	}
	if !supportedMode(sp.Mode) {
		p = append(p, "mode must be PAPER or SANDBOX")
	}
	if sp.OpenAt.IsZero() || sp.CloseAt.IsZero() || !sp.CloseAt.After(sp.OpenAt) {
		p = append(p, "close time must be after open time")
	}
	if !sp.OpenAt.Equal(sp.OpenAt.Truncate(time.Microsecond)) || !sp.CloseAt.Equal(sp.CloseAt.Truncate(time.Microsecond)) {
		p = append(p, "open and close times carry at most microsecond precision")
	}
	if sp.DisputeWindow < 0 || sp.DisputeWindow > MaxDisputeWindow || sp.DisputeWindow%time.Millisecond != 0 {
		p = append(p, "dispute window must be whole milliseconds within 0..30 days")
	}
	if !validText(sp.OracleSource, maxSourceLen) {
		p = append(p, "oracle source must be 1..200 printable characters")
	}
	if len(sp.OraclePublicKey) != ed25519.PublicKeySize {
		p = append(p, "oracle public key must be an Ed25519 public key")
	}
	if len(p) > 0 {
		return errs.New(errs.CodeValidationFailed, "predmarket: invalid market definition").WithField("problems", p)
	}
	return nil
}

// DefinitionHash is the sha256 of the definition's canonical JSON.
func (sp MarketSpec) DefinitionHash() ([]byte, error) {
	body, err := audit.CanonicalJSON(map[string]any{
		"symbol": sp.Symbol, "question": sp.Question, "outcomes": sp.Outcomes,
		"collateral_asset_id": sp.CollateralAssetID.String(), "mode": string(sp.Mode),
		"open_at": sp.OpenAt.UTC(), "close_at": sp.CloseAt.UTC(),
		"dispute_window_ms": sp.DisputeWindow.Milliseconds(),
		"oracle_source":     sp.OracleSource, "oracle_public_key": hex.EncodeToString(sp.OraclePublicKey),
	})
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "predmarket: the definition is not canonically encodable")
	}
	sum := sha256.Sum256(body)
	return sum[:], nil
}

// marketTerms is the market instrument: the contract itself. It has no base
// or quote asset, so no book can trade it; it is what issuance, redemption
// and settlement intents name.
func marketTerms(sp MarketSpec) instruments.Terms {
	closeAt := sp.CloseAt.UTC()
	return instruments.Terms{
		Kind: instruments.TypeEventOutcome, Symbol: sp.Symbol, VenueCode: VenueCode,
		SettlementAssetID:  sp.CollateralAssetID,
		ContractMultiplier: money.QuantityFromInt64(1), PriceScale: int32(ShareDecimals),
		TickSize: money.QuantityFromInt64(10_000), LotSize: ShareUnit(), MinQuantity: ShareUnit(),
		QuantityDecimals: ShareDecimals, Expiry: &closeAt, SettlementStyle: instruments.SettleCash,
		TradingCalendar: "24x7", JurisdictionTags: []string{}, OracleMaxAgeMS: OracleMaxAge.Milliseconds(),
		OracleSources: []string{sp.OracleSource}, MarginModel: instruments.MarginBinaryOutcome,
		CorporateActionBehavior: instruments.CorporateActionsNone, Transferability: instruments.Transferable,
		Outcomes: append([]string(nil), sp.Outcomes...),
	}
}

// outcomeTerms is one outcome's tradable instrument: its share (base)
// against the collateral (quote), priced at six decimals of collateral per
// share and traded in whole shares.
func outcomeTerms(sp MarketSpec, outcome string, share assets.AssetID, market instruments.InstrumentID) instruments.Terms {
	t := marketTerms(sp)
	t.Symbol = sp.Symbol + "." + outcome
	base, quote := share, sp.CollateralAssetID
	t.BaseAssetID, t.QuoteAssetID = &base, &quote
	t.UnderlyingInstrumentID = &market
	return t
}

// CreateMarket defines a market in DRAFT: its escrow facility, its market
// instrument, one SIMULATED share asset and one instrument per outcome, and
// the registry tying them together. Every instrument starts HALTED and opens
// with the market. Creating an existing symbol with the same definition is a
// replay (existing = true); with another definition it is refused.
func (s *Service) CreateMarket(ctx context.Context, tx pgx.Tx, spec MarketSpec, by econ.Actor) (Market, bool, error) {
	by, err := canonicalOperator(by)
	if err != nil {
		return Market{}, false, err
	}
	if err := spec.Validate(); err != nil {
		return Market{}, false, err
	}
	hash, err := spec.DefinitionHash()
	if err != nil {
		return Market{}, false, err
	}
	var out Market
	var existing bool
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		if err := s.authorize(ctx, tx, spec.Mode); err != nil {
			return err
		}
		// Creators of one symbol queue here, so the second sees the first.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('predmarket:symbol:' || $1, 0))`, spec.Symbol); err != nil {
			return dbErr("serialize market creation", err)
		}
		if prior, err := s.GetBySymbol(ctx, tx, spec.Symbol); err == nil {
			if string(prior.DefinitionHash) != string(hash) {
				return errs.New(errs.CodeInvalidIdempotencyReuse, "predmarket: a market with this symbol already exists with another definition").
					WithField("market_id", prior.ID.String())
			}
			out, existing = prior, true
			return nil
		} else if errs.CodeOf(err) != errs.CodeNotFound {
			return err
		}
		if !spec.CloseAt.After(s.now()) {
			return errs.New(errs.CodeValidationFailed, "predmarket: a market's close time must be in the future")
		}
		if err := s.requireCollateral(ctx, tx, spec.CollateralAssetID); err != nil {
			return err
		}
		m, err := s.defineMarket(ctx, tx, spec, hash, by)
		if err != nil {
			return err
		}
		out = m
		return nil
	})
	if err != nil {
		return Market{}, false, err
	}
	return out, existing, nil
}

// requireCollateral accepts only the sandbox dollar: an ACTIVE, USD-pegged,
// six-decimal SIMULATED asset, so one collateral unit is one simulated USD
// and one share base unit pays one collateral base unit exactly.
func (s *Service) requireCollateral(ctx context.Context, q db.Querier, aid assets.AssetID) error {
	a, err := s.assetOf(ctx, q, aid)
	if err != nil {
		return err
	}
	if a.Kind != assets.KindSimulated || a.ValueDomain != valuedomain.Simulated || a.Decimals != ShareDecimals ||
		!a.IsStablecoin || a.PegCurrency != "USD" || a.Status != assets.StatusActive {
		return errs.New(errs.CodeValidationFailed,
			"predmarket: collateral must be an ACTIVE, USD-pegged, six-decimal SIMULATED asset (the sandbox dollar)").
			WithField("asset_id", aid.String())
	}
	return nil
}

func (s *Service) defineMarket(ctx context.Context, tx pgx.Tx, spec MarketSpec, hash []byte, by econ.Actor) (Market, error) {
	mid := NewMarketID()
	facility, err := s.registry.EnsureFacility(ctx, tx, ledger.FacilityPredictionMarket, "predmarket:"+mid.String(), by)
	if err != nil {
		return Market{}, err
	}
	actorType := string(by.Type)
	marketInstrument, _, err := s.instr.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
		Terms: marketTerms(spec), CanonicalName: "PREDICTION " + spec.Symbol, RiskClass: assets.RiskSpeculative,
		Status: assets.StatusHalted, ActiveFrom: spec.OpenAt.UTC(), ActorType: actorType, ActorID: by.ID,
	})
	if err != nil {
		return Market{}, dbErr("define market instrument", err)
	}
	m := Market{
		ID: mid, Symbol: spec.Symbol, Question: spec.Question, Mode: spec.Mode, FacilityID: facility.ID,
		InstrumentID: marketInstrument.ID, CollateralAssetID: spec.CollateralAssetID, UnitQuantity: ShareUnit(),
		OpenAt: spec.OpenAt.UTC(), CloseAt: spec.CloseAt.UTC(), DisputeWindow: spec.DisputeWindow,
		OracleSource: spec.OracleSource, OraclePublicKey: append(ed25519.PublicKey(nil), spec.OraclePublicKey...),
		DefinitionHash: hash, Status: StatusDraft, CreatedBy: by,
	}
	closeAt := spec.CloseAt.UTC()
	for i, sym := range spec.Outcomes {
		share, err := s.assets.Create(ctx, tx, assets.Asset{
			Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
			Symbol: spec.Symbol + "." + sym, Name: "Prediction share " + spec.Symbol + "." + sym + " (simulated)",
			Decimals: ShareDecimals, RiskClass: assets.RiskSpeculative, Status: assets.StatusActive,
		})
		if err != nil {
			return Market{}, dbErr("define share asset", err)
		}
		inst, _, err := s.instr.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
			Terms: outcomeTerms(spec, sym, share.ID, marketInstrument.ID), CanonicalName: spec.Symbol + "." + sym,
			RiskClass: assets.RiskSpeculative, Status: assets.StatusHalted, ActiveFrom: spec.OpenAt.UTC(),
			ActiveUntil: &closeAt, ActorType: actorType, ActorID: by.ID,
		})
		if err != nil {
			return Market{}, dbErr("define outcome instrument", err)
		}
		m.Outcomes = append(m.Outcomes, Outcome{Index: i, Symbol: sym, ShareAssetID: share.ID, InstrumentID: inst.ID})
	}
	if err := insertMarket(ctx, tx, m); err != nil {
		return Market{}, err
	}
	now := s.now()
	if err := insertTransition(ctx, tx, m.ID, statusNone, StatusDraft, by, "market defined", "", now); err != nil {
		return Market{}, err
	}
	ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "predmarket/market/" + m.ID.String(), Document: definitionDoc(m, spec)})
	if err != nil {
		return Market{}, err
	}
	if err := s.auditMarket(ctx, tx, m, statusNone, StatusDraft, by, "market defined", ref.URI,
		map[string]any{"definition_hash": hex.EncodeToString(hash)}, now); err != nil {
		return Market{}, err
	}
	created, err := s.Get(ctx, tx, m.ID)
	if err != nil {
		return Market{}, err
	}
	return created, nil
}

func definitionDoc(m Market, spec MarketSpec) map[string]any {
	outcomes := make([]map[string]any, 0, len(m.Outcomes))
	for _, o := range m.Outcomes {
		outcomes = append(outcomes, map[string]any{
			"index": o.Index, "symbol": o.Symbol, "share_asset_id": o.ShareAssetID.String(), "instrument_id": o.InstrumentID.String(),
		})
	}
	return map[string]any{
		"kind": "PREDICTION_MARKET_DEFINITION", "market_id": m.ID.String(), "symbol": m.Symbol, "question": m.Question,
		"mode": string(m.Mode), "facility_id": m.FacilityID.String(), "instrument_id": m.InstrumentID.String(),
		"collateral_asset_id": m.CollateralAssetID.String(), "unit_quantity": m.UnitQuantity.String(),
		"open_at": m.OpenAt, "close_at": m.CloseAt, "dispute_window_ms": spec.DisputeWindow.Milliseconds(),
		"oracle_source": m.OracleSource, "oracle_public_key": hex.EncodeToString(m.OraclePublicKey),
		"outcomes": outcomes, "definition_hash": hex.EncodeToString(m.DefinitionHash),
		"created_by": map[string]any{"type": string(m.CreatedBy.Type), "id": m.CreatedBy.ID},
	}
}

// setInstrumentStatus moves an instrument to a status unless it is there
// already. Instrument and asset statuses share one transition table.
func (s *Service) setInstrumentStatus(ctx context.Context, tx pgx.Tx, iid instruments.InstrumentID, to assets.Status, by econ.Actor, reason string) error {
	cur, err := s.instr.Get(ctx, tx, iid)
	if err != nil {
		return dbErr("load instrument", err)
	}
	if cur.Status == to {
		return nil
	}
	if _, err := s.instr.TransitionStatus(ctx, tx, iid, instruments.StatusChange{
		To: to, ActorType: string(by.Type), ActorID: by.ID, Reason: reason,
	}); err != nil {
		return dbErr("transition instrument", err)
	}
	return nil
}

// Open moves a DRAFT market to OPEN at or after its open time and before its
// close time, and opens every instrument. Opening an OPEN market is a no-op.
func (s *Service) Open(ctx context.Context, tx pgx.Tx, marketID MarketID, by econ.Actor) (Market, error) {
	by, err := canonicalLifecycleActor(by)
	if err != nil {
		return Market{}, err
	}
	var out Market
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		m, err := s.lockMarket(ctx, tx, marketID, lockUpdate)
		if err != nil {
			return err
		}
		if m.Status == StatusOpen {
			out = m
			return nil
		}
		now := s.now()
		switch {
		case m.Status != StatusDraft:
			return errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s is %s and cannot open", m.Symbol, m.Status)
		case now.Before(m.OpenAt):
			return errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s opens at %s", m.Symbol, m.OpenAt.Format(time.RFC3339))
		case !now.Before(m.CloseAt):
			return errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s closed at %s before it opened; void it", m.Symbol, m.CloseAt.Format(time.RFC3339))
		}
		if err := s.authorize(ctx, tx, m.Mode); err != nil {
			return err
		}
		if err := s.setInstrumentStatus(ctx, tx, m.InstrumentID, assets.StatusActive, by, "prediction market opened"); err != nil {
			return err
		}
		for _, o := range m.Outcomes {
			if err := s.setInstrumentStatus(ctx, tx, o.InstrumentID, assets.StatusActive, by, "prediction market opened"); err != nil {
				return err
			}
		}
		out, err = s.marketTransition(ctx, tx, m, StatusOpen, by, "trading opened", "", nil, nil)
		return err
	})
	return out, err
}

// Close moves an OPEN market to CLOSED at or after its close time. From then
// on nothing is issued, redeemed or traded: the outcome instruments go
// HALTED, so Risk V2's instrument check and the order book refuse every new
// order in them, and the market instrument goes CLOSE_ONLY, which admits only
// the risk-reducing settlement and refund intents. When the service has
// OutcomeBooks, every order still resting on an outcome instrument is
// cancelled in the same transaction, releasing what it reserved, so no order
// outlives its market. Closing needs no authorization: it only ever removes
// activity, so a kill switch never stops it. Closing a market already past
// OPEN is a no-op.
func (s *Service) Close(ctx context.Context, tx pgx.Tx, marketID MarketID, by econ.Actor) (Market, error) {
	by, err := canonicalLifecycleActor(by)
	if err != nil {
		return Market{}, err
	}
	var out Market
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		m, err := s.lockMarket(ctx, tx, marketID, lockUpdate)
		if err != nil {
			return err
		}
		out, err = s.closeLocked(ctx, tx, m, by)
		return err
	})
	return out, err
}

func (s *Service) closeLocked(ctx context.Context, tx pgx.Tx, m Market, by econ.Actor) (Market, error) {
	switch m.Status {
	case StatusOpen:
	case StatusDraft:
		return Market{}, errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s never opened; void it instead", m.Symbol)
	default:
		return m, nil
	}
	if s.now().Before(m.CloseAt) {
		return Market{}, errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s closes at %s", m.Symbol, m.CloseAt.Format(time.RFC3339))
	}
	for _, o := range m.Outcomes {
		if err := s.setInstrumentStatus(ctx, tx, o.InstrumentID, assets.StatusHalted, by, "prediction market closed: no trading after close"); err != nil {
			return Market{}, err
		}
	}
	if err := s.setInstrumentStatus(ctx, tx, m.InstrumentID, assets.StatusCloseOnly, by, "prediction market closed: settlement only"); err != nil {
		return Market{}, err
	}
	cancelled, err := s.cancelResting(ctx, tx, m, by, "prediction market "+m.Symbol+" closed: no order outlives its market")
	if err != nil {
		return Market{}, err
	}
	var payload map[string]any
	if s.books != nil {
		payload = map[string]any{"resting_orders_cancelled": cancelled}
	}
	return s.marketTransition(ctx, tx, m, StatusClosed, by, "trading closed", "", nil, payload)
}

// cancelResting has the order book cancel every order resting on the
// market's outcome instruments, in tx, when the service has one. Without
// OutcomeBooks it does nothing and returns 0: a settlement or void then
// refuses while any outcome share is still reserved (requireUnencumbered).
func (s *Service) cancelResting(ctx context.Context, tx pgx.Tx, m Market, by econ.Actor, reason string) (int, error) {
	if s.books == nil {
		return 0, nil
	}
	ids := make([]instruments.InstrumentID, 0, len(m.Outcomes))
	for _, o := range m.Outcomes {
		ids = append(ids, o.InstrumentID)
	}
	n, err := s.books.CancelResting(ctx, tx, ids, by, reason)
	if err != nil {
		return 0, dbErr("cancel resting orders on the outcome books", err)
	}
	if n < 0 {
		return 0, errs.New(errs.CodeInternal, "predmarket: the order book reported a negative number of cancelled orders")
	}
	return n, nil
}

// CloseFailure is a due market CloseDueBatch could not close.
type CloseFailure struct {
	MarketID MarketID
	// Attempts counts its consecutive failures; it is not taken again before
	// RetryAfter.
	Attempts   int
	RetryAfter time.Time
	Err        error
}

// CloseBatch is what one CloseDueBatch call did.
type CloseBatch struct {
	Closed   []MarketID
	Failures []CloseFailure
}

// closeDueSQL takes OPEN markets whose close time has passed, earliest first,
// leaving out one whose last close failed and whose backoff is still running
// (unless the clock has gone back past that failure); never-failed markets
// come first. Markets another closer holds are skipped (SKIP LOCKED).
const closeDueSQL = `SELECT pm.id FROM prediction_markets pm
	WHERE pm.status = 'OPEN' AND pm.close_at <= $1
	  AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = pm.id
	                    AND uf.cleared_at IS NULL AND uf.retry_after > $1 AND uf.last_failed_at <= $1)
	ORDER BY EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = pm.id AND uf.cleared_at IS NULL),
	         pm.close_at, pm.id
	LIMIT $2 FOR UPDATE OF pm SKIP LOCKED`

// CloseDue is CloseDueBatch, returning the markets it closed. A market it
// could not close is recorded in the upkeep failure ledger and backed off --
// CloseDueBatch reports it -- and never stops the rest.
func (s *Service) CloseDue(ctx context.Context, tx pgx.Tx, by econ.Actor, limit int) ([]MarketID, error) {
	b, err := s.CloseDueBatch(ctx, tx, by, limit)
	if err != nil {
		return nil, err
	}
	return b.Closed, nil
}

// CloseDueBatch closes up to limit OPEN markets whose close time has passed,
// for the scheduler. Markets another closer holds are skipped, not waited on.
// Each market is closed in its own savepoint of tx, all or nothing: one that
// cannot be closed, including one the database would only refuse at COMMIT,
// is rolled back alone, recorded with a backoff and reported in Failures, and
// the others are closed. An error means the batch as a whole could not
// proceed.
func (s *Service) CloseDueBatch(ctx context.Context, tx pgx.Tx, by econ.Actor, limit int) (CloseBatch, error) {
	by, err := canonicalLifecycleActor(by)
	if err != nil {
		return CloseBatch{}, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	checkDeferred, err := deferredChecksPass(ctx, tx)
	if err != nil {
		return CloseBatch{}, err
	}
	now := s.now()
	var due []MarketID
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, closeDueSQL, now, limit, UpkeepCloseDue)
		if err != nil {
			return dbErr("select due markets", err)
		}
		defer rows.Close()
		for rows.Next() {
			var mid MarketID
			if err := rows.Scan(&mid); err != nil {
				return dbErr("scan due market", err)
			}
			due = append(due, mid)
		}
		if err := rows.Err(); err != nil {
			return dbErr("select due markets", err)
		}
		return nil
	})
	if err != nil {
		return CloseBatch{}, err
	}
	out := CloseBatch{Closed: make([]MarketID, 0, len(due))}
	for _, mid := range due {
		failed, err := upkeepItem(ctx, tx, UpkeepCloseDue, mid, now, checkDeferred, func(sp pgx.Tx) error {
			m, err := s.lockMarket(ctx, sp, mid, lockUpdate)
			if err != nil {
				return err
			}
			_, err = s.closeLocked(ctx, sp, m, by)
			return err
		})
		if err != nil {
			return CloseBatch{}, err
		}
		if failed != nil {
			out.Failures = append(out.Failures, CloseFailure{MarketID: mid, Attempts: failed.attempts,
				RetryAfter: failed.retryAfter, Err: failed.err})
			continue
		}
		out.Closed = append(out.Closed, mid)
	}
	return out, nil
}

// RequestResolution moves a CLOSED market to RESOLVING: an oracle report is
// now awaited. It is a no-op for a market already RESOLVING.
func (s *Service) RequestResolution(ctx context.Context, tx pgx.Tx, marketID MarketID, by econ.Actor) (Market, error) {
	by, err := canonicalLifecycleActor(by)
	if err != nil {
		return Market{}, err
	}
	var out Market
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		m, err := s.lockMarket(ctx, tx, marketID, lockUpdate)
		if err != nil {
			return err
		}
		if m.Status == StatusResolving {
			out = m
			return nil
		}
		if m.Status != StatusClosed || s.now().Before(m.CloseAt) {
			return errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s is %s; resolution waits for close", m.Symbol, m.Status)
		}
		out, err = s.marketTransition(ctx, tx, m, StatusResolving, by, "oracle report requested", "", nil, nil)
		return err
	})
	return out, err
}

// ResolveRequest submits a verified oracle report as a market's resolution.
type ResolveRequest struct {
	MarketID MarketID
	Report   OracleReport
	Actor    econ.Actor
	Reason   string
}

// ResolveResult is a market after a resolution step.
type ResolveResult struct {
	Market     Market
	Resolution Resolution
	// Replay is true when the report had already been accepted: nothing
	// changed and nothing was recorded.
	Replay bool
}

// Resolve accepts an oracle report. The report must name this market, come
// from the market's oracle source, verify against the market's oracle key,
// name one of its outcomes, and have been observed at or after close and not
// in the future. The operator must not be the market's creator, nor an
// operator who recorded an oracle observation for it.
//
//   - RESOLVING: the report resolves the market (an ORACLE resolution).
//   - Replaying an accepted report, in any state, changes nothing.
//   - DISPUTED: a different report closes the dispute -- DISPUTE_OVERTURNED
//     when its outcome differs, DISPUTE_UPHELD when it does not -- and may be
//     submitted neither by the operator who raised the dispute nor by the one
//     whose resolution is disputed.
//   - RESOLVED or SETTLED: a different report is refused.
func (s *Service) Resolve(ctx context.Context, tx pgx.Tx, req ResolveRequest) (ResolveResult, error) {
	by, err := canonicalOperator(req.Actor)
	if err != nil {
		return ResolveResult{}, err
	}
	var out ResolveResult
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		m, err := s.lockMarket(ctx, tx, req.MarketID, lockUpdate)
		if err != nil {
			return err
		}
		outcome, err := s.acceptableReport(m, req.Report)
		if err != nil {
			return err
		}
		if err := requireOtherThan(by, m.CreatedBy, "the creator of a market cannot resolve it"); err != nil {
			return err
		}
		if err := requireNotObserver(ctx, tx, m, by); err != nil {
			return err
		}
		hash := req.Report.Hash()
		if prior, found, err := resolutionByHash(ctx, tx, m.ID, hash); err != nil {
			return err
		} else if found {
			out = ResolveResult{Market: m, Resolution: prior, Replay: true}
			return nil
		}
		r := Resolution{
			ID: id.New[resolutionKind](), MarketID: m.ID, OutcomeIndex: outcome.Index, ReportHash: hash,
			ReportSource: req.Report.Source, ReportSignature: req.Report.Signature, AcceptedBy: by,
			Reason: reasonOr(req.Reason, "oracle report accepted"), AcceptedAt: s.now(),
		}
		observed := req.Report.ObservedAt.UTC()
		r.ReportObservedAt = &observed
		switch m.Status {
		case StatusResolving:
			r.Kind = ResolutionOracle
		case StatusDisputed:
			current, dispute, err := s.openDispute(ctx, tx, m)
			if err != nil {
				return err
			}
			if err := requireOtherThan(by, dispute.RaisedBy, "the operator who raised a dispute cannot rule on it"); err != nil {
				return err
			}
			if err := requireOtherThan(by, current.AcceptedBy, "the operator whose resolution is disputed cannot rule on the dispute"); err != nil {
				return err
			}
			r.Kind = ResolutionDisputeUpheld
			if outcome.Index != current.OutcomeIndex {
				r.Kind = ResolutionDisputeOverturned
			}
			r.SupersedesID, r.DisputeID = &current.ID, &dispute.ID
		case StatusResolved, StatusSettled:
			return errs.Newf(errs.CodeConflict,
				"predmarket: market %s is already %s by another report; a different report is accepted only while the market is DISPUTED",
				m.Symbol, m.Status).WithField("market_id", m.ID.String())
		default:
			return errs.Newf(errs.CodeInvalidStateTransition,
				"predmarket: market %s is %s; a report is accepted only after close, once resolution is requested", m.Symbol, m.Status)
		}
		ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{
			Key: "predmarket/oracle/" + m.ID.String() + "/" + hex.EncodeToString(hash), Document: reportDoc(m, req.Report, r),
		})
		if err != nil {
			return err
		}
		r.EvidenceRef, r.EvidenceSHA256 = ref.URI, ref.SHA256
		if err := insertResolution(ctx, tx, r); err != nil {
			return err
		}
		next, err := s.marketTransition(ctx, tx, m, StatusResolved, by, r.Reason, ref.URI, &r.ID, map[string]any{
			"resolution_id": r.ID.String(), "resolution_kind": string(r.Kind), "outcome": outcome.Symbol,
			"report_hash": hex.EncodeToString(hash),
		})
		if err != nil {
			return err
		}
		stored, err := s.Resolution(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		out = ResolveResult{Market: next, Resolution: stored}
		return nil
	})
	return out, err
}

// acceptableReport verifies a report against the market and returns the
// outcome it names.
func (s *Service) acceptableReport(m Market, rep OracleReport) (Outcome, error) {
	refuse := func(code errs.Code, why, detail string) (Outcome, error) {
		return Outcome{}, errs.New(code, "predmarket: oracle report refused: "+detail).
			WithField("market_id", m.ID.String()).WithField("oracle_refusal", why)
	}
	if rep.MarketID != m.ID {
		return refuse(errs.CodeForbidden, "WRONG_MARKET", "it was issued for another market")
	}
	if err := VerifyReport(m.OraclePublicKey, rep); err != nil {
		return Outcome{}, err
	}
	if rep.Source != m.OracleSource {
		return refuse(errs.CodeForbidden, "WRONG_SOURCE", "it comes from a source this market did not name")
	}
	outcome, ok := m.OutcomeBySymbol(rep.Outcome)
	if !ok {
		return refuse(errs.CodeValidationFailed, "UNKNOWN_OUTCOME", "it names an outcome this market does not have")
	}
	if rep.ObservedAt.Before(m.CloseAt) {
		return refuse(errs.CodeValidationFailed, "OBSERVED_BEFORE_CLOSE", "it was observed before trading closed")
	}
	if rep.ObservedAt.After(s.now()) {
		return refuse(errs.CodeValidationFailed, "OBSERVED_IN_FUTURE", "it claims an observation in the future")
	}
	return outcome, nil
}

func reportDoc(m Market, rep OracleReport, r Resolution) map[string]any {
	evidenceSum := sha256.Sum256(rep.Evidence)
	return map[string]any{
		"kind": "PREDICTION_ORACLE_REPORT", "market_id": m.ID.String(), "market_symbol": m.Symbol,
		"resolution_id": r.ID.String(), "resolution_kind": string(r.Kind), "outcome": rep.Outcome,
		"outcome_index": r.OutcomeIndex, "source": rep.Source, "observed_at": rep.ObservedAt.UTC(),
		"evidence_base64": base64.StdEncoding.EncodeToString(rep.Evidence), "evidence_sha256": hex.EncodeToString(evidenceSum[:]),
		"signature_hex": hex.EncodeToString(rep.Signature), "signed_bytes": string(rep.SigningBytes()),
		"oracle_public_key": hex.EncodeToString(m.OraclePublicKey), "report_hash": hex.EncodeToString(rep.Hash()),
		"verified": "ED25519_OK", "accepted_by": map[string]any{"type": string(r.AcceptedBy.Type), "id": r.AcceptedBy.ID},
		"accepted_at": r.AcceptedAt,
	}
}

// openDispute returns a DISPUTED market's current resolution and the dispute
// raised against it.
func (s *Service) openDispute(ctx context.Context, q db.Querier, m Market) (Resolution, Dispute, error) {
	if m.CurrentResolutionID == nil {
		return Resolution{}, Dispute{}, errs.New(errs.CodeInternal, "predmarket: a disputed market has no resolution")
	}
	current, err := s.Resolution(ctx, q, *m.CurrentResolutionID)
	if err != nil {
		return Resolution{}, Dispute{}, err
	}
	d, found, err := disputeOf(ctx, q, current.ID)
	if err != nil {
		return Resolution{}, Dispute{}, err
	}
	if !found {
		return Resolution{}, Dispute{}, errs.New(errs.CodeInternal, "predmarket: a disputed market has no dispute on its resolution")
	}
	return current, d, nil
}

// DisputeRequest disputes a RESOLVED market's oracle resolution.
type DisputeRequest struct {
	MarketID MarketID
	Actor    econ.Actor
	// Reason says why, in at least 8 characters.
	Reason string
	// Evidence is the document the dispute rests on; it is archived.
	Evidence any
}

// Dispute moves a RESOLVED market to DISPUTED, recording who disputed it,
// why and on what evidence. Settlement is blocked until the dispute is
// closed by Resolve with a new report, by UpholdResolution, or by an approved
// void (ProposeVoid, then ApproveVoid by another operator). Only an ORACLE
// resolution can be disputed, and only once: a ruling is final. Disputing is
// also the only way to void a RESOLVED market inside its dispute window.
func (s *Service) Dispute(ctx context.Context, tx pgx.Tx, req DisputeRequest) (Dispute, Market, error) {
	by, err := canonicalOperator(req.Actor)
	if err != nil {
		return Dispute{}, Market{}, err
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) < 8 || len(reason) > 2000 {
		return Dispute{}, Market{}, errs.New(errs.CodeValidationFailed, "predmarket: a dispute states its reason in 8..2000 characters")
	}
	if req.Evidence == nil {
		return Dispute{}, Market{}, errs.New(errs.CodeValidationFailed, "predmarket: a dispute carries its evidence")
	}
	var d Dispute
	var out Market
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		m, err := s.lockMarket(ctx, tx, req.MarketID, lockUpdate)
		if err != nil {
			return err
		}
		switch m.Status {
		case StatusResolved:
		case StatusDisputed:
			return errs.Newf(errs.CodeConflict, "predmarket: market %s is already disputed", m.Symbol)
		default:
			return errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s is %s; only a RESOLVED market can be disputed", m.Symbol, m.Status)
		}
		current, err := s.Resolution(ctx, tx, *m.CurrentResolutionID)
		if err != nil {
			return err
		}
		if current.Kind != ResolutionOracle {
			return errs.Newf(errs.CodeConflict, "predmarket: market %s was resolved by a dispute ruling, which is final", m.Symbol)
		}
		d = Dispute{
			ID: id.New[disputeKind](), MarketID: m.ID, ResolutionID: current.ID, RaisedBy: by, Reason: reason, RaisedAt: s.now(),
		}
		ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "predmarket/dispute/" + d.ID.String(), Document: map[string]any{
			"kind": "PREDICTION_DISPUTE", "dispute_id": d.ID.String(), "market_id": m.ID.String(),
			"resolution_id": current.ID.String(), "raised_by": map[string]any{"type": string(by.Type), "id": by.ID},
			"reason": reason, "evidence": req.Evidence, "raised_at": d.RaisedAt,
		}})
		if err != nil {
			return err
		}
		d.EvidenceRef, d.EvidenceSHA256 = ref.URI, ref.SHA256
		if err := insertDispute(ctx, tx, d); err != nil {
			return err
		}
		out, err = s.marketTransition(ctx, tx, m, StatusDisputed, by, reason, ref.URI, nil,
			map[string]any{"dispute_id": d.ID.String(), "resolution_id": current.ID.String()})
		return err
	})
	if err != nil {
		return Dispute{}, Market{}, err
	}
	return d, out, nil
}

// RulingRequest closes a dispute by upholding the disputed resolution.
type RulingRequest struct {
	MarketID MarketID
	Actor    econ.Actor
	Reason   string
	Evidence any
}

// UpholdResolution closes a DISPUTED market's dispute keeping its outcome:
// DISPUTED -> RESOLVED with a DISPUTE_UPHELD resolution. The ruling is
// independent: neither the market's creator, nor the operator who raised the
// dispute, nor the operator whose resolution is disputed, nor one who
// recorded an oracle observation for the market may rule.
func (s *Service) UpholdResolution(ctx context.Context, tx pgx.Tx, req RulingRequest) (ResolveResult, error) {
	by, err := canonicalOperator(req.Actor)
	if err != nil {
		return ResolveResult{}, err
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) < 8 || len(reason) > 2000 {
		return ResolveResult{}, errs.New(errs.CodeValidationFailed, "predmarket: a ruling states its reason in 8..2000 characters")
	}
	var out ResolveResult
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		m, err := s.lockMarket(ctx, tx, req.MarketID, lockUpdate)
		if err != nil {
			return err
		}
		if m.Status != StatusDisputed {
			return errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s is %s; only a dispute can be ruled on", m.Symbol, m.Status)
		}
		current, dispute, err := s.openDispute(ctx, tx, m)
		if err != nil {
			return err
		}
		if err := requireOtherThan(by, m.CreatedBy, "the creator of a market cannot resolve it"); err != nil {
			return err
		}
		if err := requireOtherThan(by, dispute.RaisedBy, "the operator who raised a dispute cannot rule on it"); err != nil {
			return err
		}
		if err := requireOtherThan(by, current.AcceptedBy, "the operator whose resolution is disputed cannot uphold it"); err != nil {
			return err
		}
		if err := requireNotObserver(ctx, tx, m, by); err != nil {
			return err
		}
		r := Resolution{
			ID: id.New[resolutionKind](), MarketID: m.ID, Kind: ResolutionDisputeUpheld, OutcomeIndex: current.OutcomeIndex,
			SupersedesID: &current.ID, DisputeID: &dispute.ID, AcceptedBy: by, Reason: reason, AcceptedAt: s.now(),
		}
		ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "predmarket/ruling/" + r.ID.String(), Document: map[string]any{
			"kind": "PREDICTION_DISPUTE_RULING", "ruling": string(r.Kind), "market_id": m.ID.String(),
			"resolution_id": r.ID.String(), "supersedes_id": current.ID.String(), "dispute_id": dispute.ID.String(),
			"outcome_index": r.OutcomeIndex, "ruled_by": map[string]any{"type": string(by.Type), "id": by.ID},
			"reason": reason, "evidence": req.Evidence, "ruled_at": r.AcceptedAt,
		}})
		if err != nil {
			return err
		}
		r.EvidenceRef, r.EvidenceSHA256 = ref.URI, ref.SHA256
		if err := insertResolution(ctx, tx, r); err != nil {
			return err
		}
		next, err := s.marketTransition(ctx, tx, m, StatusResolved, by, reason, ref.URI, &r.ID, map[string]any{
			"resolution_id": r.ID.String(), "resolution_kind": string(r.Kind), "dispute_id": dispute.ID.String(),
		})
		if err != nil {
			return err
		}
		stored, err := s.Resolution(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		out = ResolveResult{Market: next, Resolution: stored}
		return nil
	})
	return out, err
}

// InstrumentSnapshot returns the Risk V2 instrument section for an order or
// intent on one of a market's instruments at the service's current time: the
// instrument's status, whether the market's trading session is open (the
// market is OPEN and now lies in [open, close)), and whether close has
// passed. It is what the internal book evaluates outcome-share orders with.
func (s *Service) InstrumentSnapshot(ctx context.Context, q db.Querier, marketID MarketID, iid instruments.InstrumentID) (risk.InstrumentSnapshot, error) {
	m, err := s.Get(ctx, q, marketID)
	if err != nil {
		return risk.InstrumentSnapshot{}, err
	}
	belongs := iid == m.InstrumentID
	for _, o := range m.Outcomes {
		belongs = belongs || iid == o.InstrumentID
	}
	if !belongs {
		return risk.InstrumentSnapshot{}, errs.New(errs.CodeNotFound, "predmarket: the instrument is not one of this market's")
	}
	inst, err := s.instr.Get(ctx, q, iid)
	if err != nil {
		return risk.InstrumentSnapshot{}, dbErr("load instrument", err)
	}
	return risk.InstrumentSnapshot{
		Status: string(inst.Status), SessionOpen: m.tradingOpen(s.now()), Expired: !s.now().Before(m.CloseAt),
	}, nil
}

// tradingOpen reports whether the market's trading session is open at now.
func (m Market) tradingOpen(now time.Time) bool {
	return m.Status == StatusOpen && !now.Before(m.OpenAt) && now.Before(m.CloseAt)
}

// ListByStatus returns up to limit markets in a status, oldest close first.
func (s *Service) ListByStatus(ctx context.Context, q db.Querier, status MarketStatus, limit int) ([]Market, error) {
	if !status.Valid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "predmarket: unknown market status %q", status)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT id FROM prediction_markets WHERE status = $1 ORDER BY close_at, id LIMIT $2`, status, limit)
	if err != nil {
		return nil, dbErr("list markets", err)
	}
	var ids []MarketID
	for rows.Next() {
		var mid MarketID
		if err := rows.Scan(&mid); err != nil {
			rows.Close()
			return nil, dbErr("scan market id", err)
		}
		ids = append(ids, mid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, dbErr("list markets", err)
	}
	out := make([]Market, 0, len(ids))
	for _, mid := range ids {
		m, err := s.Get(ctx, q, mid)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func reasonOr(reason, fallback string) string {
	if r := strings.TrimSpace(reason); r != "" {
		return r
	}
	return fallback
}
