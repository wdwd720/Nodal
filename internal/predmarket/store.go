package predmarket

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

// lockMode says how a market row is read.
type lockMode int

const (
	lockNone lockMode = iota
	// lockShare lets concurrent issuances and redemptions proceed together
	// while keeping a close, void or settlement out until they commit.
	lockShare
	// lockUpdate serializes lifecycle changes of one market.
	lockUpdate
)

const marketColumns = `id, symbol, question, mode, facility_id, instrument_id, collateral_asset_id, unit_quantity::text,
	open_at, close_at, dispute_window_ms, oracle_source, oracle_public_key, definition_hash, status, current_resolution_id,
	created_by_actor_type, created_by_actor_id, created_at, updated_at`

func scanMarket(row pgx.Row) (Market, error) {
	var (
		m        Market
		unit     string
		windowMS int64
		pub      []byte
	)
	if err := row.Scan(&m.ID, &m.Symbol, &m.Question, &m.Mode, &m.FacilityID, &m.InstrumentID, &m.CollateralAssetID, &unit,
		&m.OpenAt, &m.CloseAt, &windowMS, &m.OracleSource, &pub, &m.DefinitionHash, &m.Status, &m.CurrentResolutionID,
		&m.CreatedBy.Type, &m.CreatedBy.ID, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return Market{}, err
	}
	q, err := money.ScanQuantity(unit)
	if err != nil {
		return Market{}, errs.Wrap(err, errs.CodeInternal, "predmarket: stored unit is not an integer")
	}
	m.UnitQuantity = q
	m.DisputeWindow = time.Duration(windowMS) * time.Millisecond
	m.OraclePublicKey = ed25519.PublicKey(pub)
	m.OpenAt, m.CloseAt = m.OpenAt.UTC(), m.CloseAt.UTC()
	m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	return m, nil
}

// Get returns a market with its outcomes.
func (s *Service) Get(ctx context.Context, q db.Querier, marketID MarketID) (Market, error) {
	return s.loadMarket(ctx, q, `id = $1`, lockNone, marketID)
}

// GetBySymbol returns the market with the given symbol.
func (s *Service) GetBySymbol(ctx context.Context, q db.Querier, symbol string) (Market, error) {
	return s.loadMarket(ctx, q, `symbol = $1`, lockNone, symbol)
}

func (s *Service) lockMarket(ctx context.Context, tx pgx.Tx, marketID MarketID, mode lockMode) (Market, error) {
	return s.loadMarket(ctx, tx, `id = $1`, mode, marketID)
}

func (s *Service) loadMarket(ctx context.Context, q db.Querier, where string, mode lockMode, arg any) (Market, error) {
	sql := `SELECT ` + marketColumns + ` FROM prediction_markets WHERE ` + where
	switch mode {
	case lockShare:
		sql += ` FOR SHARE`
	case lockUpdate:
		sql += ` FOR UPDATE`
	}
	m, err := scanMarket(q.QueryRow(ctx, sql, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return Market{}, errs.New(errs.CodeNotFound, "predmarket: market not found")
	}
	if err != nil {
		return Market{}, dbErr("load market", err)
	}
	if m.Outcomes, err = loadOutcomes(ctx, q, m.ID); err != nil {
		return Market{}, err
	}
	return m, nil
}

func loadOutcomes(ctx context.Context, q db.Querier, marketID MarketID) ([]Outcome, error) {
	rows, err := q.Query(ctx, `SELECT outcome_index, symbol, share_asset_id, instrument_id
		FROM prediction_outcomes_registry WHERE market_id = $1 ORDER BY outcome_index`, marketID)
	if err != nil {
		return nil, dbErr("load outcomes", err)
	}
	defer rows.Close()
	var out []Outcome
	for rows.Next() {
		var o Outcome
		var idx int16
		if err := rows.Scan(&idx, &o.Symbol, &o.ShareAssetID, &o.InstrumentID); err != nil {
			return nil, dbErr("scan outcome", err)
		}
		o.Index = int(idx)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("load outcomes", err)
	}
	return out, nil
}

func insertMarket(ctx context.Context, tx pgx.Tx, m Market) error {
	_, err := tx.Exec(ctx, `INSERT INTO prediction_markets
		(id, symbol, question, mode, facility_id, instrument_id, collateral_asset_id, outcome_count, unit_quantity,
		 open_at, close_at, dispute_window_ms, oracle_source, oracle_public_key, definition_hash, status,
		 created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::numeric,$10,$11,$12,$13,$14,$15,'DRAFT',$16,$17)`,
		m.ID, m.Symbol, m.Question, m.Mode, m.FacilityID, m.InstrumentID, m.CollateralAssetID, int16(len(m.Outcomes)), // #nosec G115 -- bounded by MaxOutcomes
		m.UnitQuantity.String(), m.OpenAt, m.CloseAt, m.DisputeWindow.Milliseconds(), m.OracleSource,
		[]byte(m.OraclePublicKey), m.DefinitionHash, m.CreatedBy.Type, m.CreatedBy.ID)
	if err != nil {
		return dbErr("insert market", err)
	}
	for _, o := range m.Outcomes {
		if _, err := tx.Exec(ctx, `INSERT INTO prediction_outcomes_registry (id, market_id, outcome_index, symbol, share_asset_id, instrument_id)
			VALUES ($1,$2,$3,$4,$5,$6)`, id.New[id.Any](), m.ID, int16(o.Index), o.Symbol, o.ShareAssetID, o.InstrumentID); err != nil { // #nosec G115 -- bounded by MaxOutcomes
			return dbErr("register outcome", err)
		}
	}
	return nil
}

// marketTransition records a lifecycle step: the transition row first (the
// edge binding reads it), then the status change, then the audit event, all
// in tx. resolution is set only on a transition into RESOLVED.
func (s *Service) marketTransition(ctx context.Context, tx pgx.Tx, m Market, to MarketStatus, actor econ.Actor,
	reason, evidenceRef string, resolution *ResolutionID, payload map[string]any) (Market, error) {
	if !CanTransition(m.Status, to) {
		return Market{}, errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s cannot move %s -> %s", m.Symbol, m.Status, to).
			WithField("market_id", m.ID.String()).WithField("from_status", string(m.Status)).WithField("to_status", string(to))
	}
	now := s.now()
	if err := insertTransition(ctx, tx, m.ID, m.Status, to, actor, reason, evidenceRef, now); err != nil {
		return Market{}, err
	}
	var res any
	if resolution != nil {
		res = *resolution
	}
	next, err := scanMarket(tx.QueryRow(ctx, `UPDATE prediction_markets SET status = $2,
		current_resolution_id = coalesce($3, current_resolution_id) WHERE id = $1 RETURNING `+marketColumns, m.ID, to, res))
	if err != nil {
		return Market{}, dbErr("transition market", err)
	}
	next.Outcomes = m.Outcomes
	if err := s.auditMarket(ctx, tx, next, m.Status, to, actor, reason, evidenceRef, payload, now); err != nil {
		return Market{}, err
	}
	return next, nil
}

func insertTransition(ctx context.Context, tx pgx.Tx, marketID MarketID, from, to MarketStatus, actor econ.Actor, reason, evidenceRef string, at time.Time) error {
	if strings.TrimSpace(reason) == "" {
		reason = string(to)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO prediction_market_transitions
		(id, market_id, from_status, to_status, actor_type, actor_id, reason, evidence_ref, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`,
		id.New[id.Any](), marketID, from, to, actor.Type, actor.ID, reason, evidenceRef, at); err != nil {
		return dbErr("record market transition", err)
	}
	return nil
}

// auditMarket appends the market's lifecycle step to the admin audit stream:
// every step is an operator (or scheduler) action.
func (s *Service) auditMarket(ctx context.Context, tx pgx.Tx, m Market, from, to MarketStatus, actor econ.Actor,
	reason, evidenceRef string, payload map[string]any, at time.Time) error {
	doc := map[string]any{
		"market_id": m.ID.String(), "symbol": m.Symbol, "from_status": string(from), "to_status": string(to),
		"mode": string(m.Mode),
	}
	for k, v := range payload {
		doc[k] = v
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "predmarket: encode audit payload")
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream: audit.AdminStream, ActorType: string(actor.Type), ActorID: actor.ID,
		Action: "predmarket.market." + strings.ToLower(string(to)), ResourceType: "prediction_market", ResourceID: m.ID.String(),
		Reason: reason, EvidenceRef: evidenceRef, Payload: body, OccurredAt: at,
	})
	return err
}

// --- resolutions, disputes, settlements ----------------------------------------

const resolutionColumns = `id, market_id, kind, outcome_index, supersedes_id, dispute_id, report_hash, coalesce(report_source,''),
	report_observed_at, report_signature, evidence_ref, evidence_sha256, accepted_by_actor_type, accepted_by_actor_id,
	reason, accepted_at`

func scanResolution(row pgx.Row) (Resolution, error) {
	var r Resolution
	var idx int16
	if err := row.Scan(&r.ID, &r.MarketID, &r.Kind, &idx, &r.SupersedesID, &r.DisputeID, &r.ReportHash, &r.ReportSource,
		&r.ReportObservedAt, &r.ReportSignature, &r.EvidenceRef, &r.EvidenceSHA256, &r.AcceptedBy.Type, &r.AcceptedBy.ID,
		&r.Reason, &r.AcceptedAt); err != nil {
		return Resolution{}, err
	}
	r.OutcomeIndex = int(idx)
	r.AcceptedAt = r.AcceptedAt.UTC()
	if r.ReportObservedAt != nil {
		t := r.ReportObservedAt.UTC()
		r.ReportObservedAt = &t
	}
	return r, nil
}

func insertResolution(ctx context.Context, tx pgx.Tx, r Resolution) error {
	var observed any
	if r.ReportObservedAt != nil {
		observed = *r.ReportObservedAt
	}
	_, err := tx.Exec(ctx, `INSERT INTO prediction_resolutions
		(id, market_id, kind, outcome_index, supersedes_id, dispute_id, report_hash, report_source, report_observed_at,
		 report_signature, evidence_ref, evidence_sha256, accepted_by_actor_type, accepted_by_actor_id, reason, accepted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10,$11,$12,$13,$14,$15,$16)`,
		r.ID, r.MarketID, r.Kind, int16(r.OutcomeIndex), r.SupersedesID, r.DisputeID, nilIfEmpty(r.ReportHash), r.ReportSource, // #nosec G115 -- bounded by MaxOutcomes
		observed, nilIfEmpty(r.ReportSignature), r.EvidenceRef, r.EvidenceSHA256, r.AcceptedBy.Type, r.AcceptedBy.ID,
		r.Reason, r.AcceptedAt)
	if err != nil {
		return dbErr("record resolution", err)
	}
	return nil
}

func nilIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// Resolution returns one resolution.
func (s *Service) Resolution(ctx context.Context, q db.Querier, rid ResolutionID) (Resolution, error) {
	r, err := scanResolution(q.QueryRow(ctx, `SELECT `+resolutionColumns+` FROM prediction_resolutions WHERE id = $1`, rid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Resolution{}, errs.New(errs.CodeNotFound, "predmarket: resolution not found")
	}
	if err != nil {
		return Resolution{}, dbErr("load resolution", err)
	}
	return r, nil
}

// Resolutions returns every resolution of a market, oldest first.
func (s *Service) Resolutions(ctx context.Context, q db.Querier, marketID MarketID) ([]Resolution, error) {
	rows, err := q.Query(ctx, `SELECT `+resolutionColumns+` FROM prediction_resolutions WHERE market_id = $1
		ORDER BY accepted_at, created_at, id`, marketID)
	if err != nil {
		return nil, dbErr("list resolutions", err)
	}
	defer rows.Close()
	var out []Resolution
	for rows.Next() {
		r, err := scanResolution(rows)
		if err != nil {
			return nil, dbErr("scan resolution", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func resolutionByHash(ctx context.Context, q db.Querier, marketID MarketID, hash []byte) (Resolution, bool, error) {
	r, err := scanResolution(q.QueryRow(ctx, `SELECT `+resolutionColumns+` FROM prediction_resolutions
		WHERE market_id = $1 AND report_hash = $2`, marketID, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Resolution{}, false, nil
	}
	if err != nil {
		return Resolution{}, false, dbErr("find resolution by report", err)
	}
	return r, true, nil
}

const disputeColumns = `id, market_id, resolution_id, raised_by_actor_type, raised_by_actor_id, reason, evidence_ref,
	evidence_sha256, raised_at`

func scanDispute(row pgx.Row) (Dispute, error) {
	var d Dispute
	if err := row.Scan(&d.ID, &d.MarketID, &d.ResolutionID, &d.RaisedBy.Type, &d.RaisedBy.ID, &d.Reason, &d.EvidenceRef,
		&d.EvidenceSHA256, &d.RaisedAt); err != nil {
		return Dispute{}, err
	}
	d.RaisedAt = d.RaisedAt.UTC()
	return d, nil
}

func insertDispute(ctx context.Context, tx pgx.Tx, d Dispute) error {
	if _, err := tx.Exec(ctx, `INSERT INTO prediction_disputes
		(id, market_id, resolution_id, raised_by_actor_type, raised_by_actor_id, reason, evidence_ref, evidence_sha256, raised_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		d.ID, d.MarketID, d.ResolutionID, d.RaisedBy.Type, d.RaisedBy.ID, d.Reason, d.EvidenceRef, d.EvidenceSHA256, d.RaisedAt); err != nil {
		return dbErr("record dispute", err)
	}
	return nil
}

func disputeOf(ctx context.Context, q db.Querier, rid ResolutionID) (Dispute, bool, error) {
	d, err := scanDispute(q.QueryRow(ctx, `SELECT `+disputeColumns+` FROM prediction_disputes WHERE resolution_id = $1`, rid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Dispute{}, false, nil
	}
	if err != nil {
		return Dispute{}, false, dbErr("load dispute", err)
	}
	return d, true, nil
}

// Disputes returns every dispute raised on a market, oldest first.
func (s *Service) Disputes(ctx context.Context, q db.Querier, marketID MarketID) ([]Dispute, error) {
	rows, err := q.Query(ctx, `SELECT `+disputeColumns+` FROM prediction_disputes WHERE market_id = $1 ORDER BY raised_at, id`, marketID)
	if err != nil {
		return nil, dbErr("list disputes", err)
	}
	defer rows.Close()
	var out []Dispute
	for rows.Next() {
		d, err := scanDispute(rows)
		if err != nil {
			return nil, dbErr("scan dispute", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const settlementColumns = `id, market_id, kind, resolution_id, winning_outcome_index, outcome_count, unit_quantity::text,
	refund_rate_per_share::text, rounding_policy, escrow_before::text, total_paid::text, residual::text, holder_count,
	evidence_ref, evidence_sha256, settled_by_actor_type, settled_by_actor_id, settled_at`

func scanSettlement(row pgx.Row) (Settlement, error) {
	var (
		st                      Settlement
		winning                 *int16
		count                   int16
		unit, escrow, paid, rem string
		rate                    *string
		holders                 int32
	)
	if err := row.Scan(&st.ID, &st.MarketID, &st.Kind, &st.ResolutionID, &winning, &count, &unit, &rate, &st.RoundingPolicy,
		&escrow, &paid, &rem, &holders, &st.EvidenceRef, &st.EvidenceSHA256, &st.SettledBy.Type, &st.SettledBy.ID,
		&st.SettledAt); err != nil {
		return Settlement{}, err
	}
	if winning != nil {
		w := int(*winning)
		st.WinningOutcomeIndex = &w
	}
	st.OutcomeCount = int(count)
	st.HolderCount = int(holders)
	var err error
	for _, f := range []struct {
		src string
		dst *money.Quantity
	}{{unit, &st.UnitQuantity}, {escrow, &st.EscrowBefore}, {paid, &st.TotalPaid}, {rem, &st.Residual}} {
		if *f.dst, err = money.ScanQuantity(f.src); err != nil {
			return Settlement{}, errs.Wrap(err, errs.CodeInternal, "predmarket: stored settlement amount is not an integer")
		}
	}
	if rate != nil {
		r, err := money.ScanQuantity(*rate)
		if err != nil {
			return Settlement{}, errs.Wrap(err, errs.CodeInternal, "predmarket: stored refund rate is not an integer")
		}
		st.RefundRatePerShare = &r
	}
	st.SettledAt = st.SettledAt.UTC()
	return st, nil
}

func insertSettlement(ctx context.Context, tx pgx.Tx, st Settlement) error {
	var winning, rate any
	if st.WinningOutcomeIndex != nil {
		winning = int16(*st.WinningOutcomeIndex) // #nosec G115 -- bounded by MaxOutcomes
	}
	if st.RefundRatePerShare != nil {
		rate = st.RefundRatePerShare.String()
	}
	_, err := tx.Exec(ctx, `INSERT INTO prediction_settlements
		(id, market_id, kind, resolution_id, winning_outcome_index, outcome_count, unit_quantity, refund_rate_per_share,
		 rounding_policy, escrow_before, total_paid, residual, holder_count, evidence_ref, evidence_sha256,
		 settled_by_actor_type, settled_by_actor_id, settled_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8::numeric,$9,$10::numeric,$11::numeric,$12::numeric,$13,$14,$15,$16,$17,$18)`,
		st.ID, st.MarketID, st.Kind, st.ResolutionID, winning, int16(st.OutcomeCount), st.UnitQuantity.String(), rate, // #nosec G115 -- bounded by MaxOutcomes
		st.RoundingPolicy, st.EscrowBefore.String(), st.TotalPaid.String(), st.Residual.String(), int32(st.HolderCount), // #nosec G115 -- one row per holder of one market
		st.EvidenceRef, st.EvidenceSHA256, st.SettledBy.Type, st.SettledBy.ID, st.SettledAt)
	if err != nil {
		return dbErr("record settlement", err)
	}
	return nil
}

// SettlementOf returns a market's settlement record, if it has one.
func (s *Service) SettlementOf(ctx context.Context, q db.Querier, marketID MarketID) (Settlement, bool, error) {
	st, err := scanSettlement(q.QueryRow(ctx, `SELECT `+settlementColumns+` FROM prediction_settlements WHERE market_id = $1`, marketID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Settlement{}, false, nil
	}
	if err != nil {
		return Settlement{}, false, dbErr("load settlement", err)
	}
	return st, true, nil
}

// --- positions and books ---------------------------------------------------------

// Positions returns the prediction_positions projection of a market: every
// customer WALLET balance of its outcome shares, by account then outcome.
func (s *Service) Positions(ctx context.Context, q db.Querier, marketID MarketID) ([]Position, error) {
	rows, err := q.Query(ctx, `SELECT market_id, account_id, outcome_index, outcome_symbol, share_asset_id, quantity::text, as_of
		FROM prediction_positions WHERE market_id = $1 ORDER BY account_id, outcome_index`, marketID)
	if err != nil {
		return nil, dbErr("read positions", err)
	}
	defer rows.Close()
	var out []Position
	for rows.Next() {
		var p Position
		var idx int16
		var qty string
		if err := rows.Scan(&p.MarketID, &p.AccountID, &idx, &p.OutcomeSymbol, &p.ShareAssetID, &qty, &p.AsOf); err != nil {
			return nil, dbErr("scan position", err)
		}
		if p.Quantity, err = money.ScanQuantity(qty); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "predmarket: a position is not an integer")
		}
		p.OutcomeIndex = int(idx)
		p.AsOf = p.AsOf.UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// holdings groups a market's positions by account.
func (s *Service) holdings(ctx context.Context, q db.Querier, m Market) ([]Holding, error) {
	positions, err := s.Positions(ctx, q, m.ID)
	if err != nil {
		return nil, err
	}
	byAccount := map[accounts.AccountID]int{}
	var out []Holding
	for _, p := range positions {
		if p.OutcomeIndex < 0 || p.OutcomeIndex >= len(m.Outcomes) {
			return nil, errs.New(errs.CodeInternal, "predmarket: a position names an unknown outcome")
		}
		i, ok := byAccount[p.AccountID]
		if !ok {
			i = len(out)
			byAccount[p.AccountID] = i
			out = append(out, Holding{AccountID: p.AccountID, Shares: make([]money.Quantity, len(m.Outcomes))})
		}
		out[i].Shares[p.OutcomeIndex] = p.Quantity
	}
	return out, nil
}

// Books reads one consistent snapshot of a market's facility books and of
// the customer holdings of its shares. It is ONE statement on purpose: under
// READ COMMITTED every statement has its own snapshot, and an identity
// checked across two statements could see an issuance committed between them.
func (s *Service) Books(ctx context.Context, q db.Querier, m Market) (EscrowBooks, error) {
	rows, err := q.Query(ctx, `
		WITH fac AS (
			SELECT la.code, la.asset_id, coalesce(b.balance, 0) AS balance
			  FROM ledger_accounts la LEFT JOIN ledger_balances b ON b.ledger_account_id = la.id
			 WHERE la.owner_type = 'FACILITY' AND la.owner_id = $2
		)
		SELECT o.outcome_index,
		       coalesce((SELECT balance FROM fac WHERE code = 'PREDICTION_SHARES_ISSUED' AND asset_id = o.share_asset_id), 0)::text,
		       coalesce((SELECT balance FROM fac WHERE code = 'PREDICTION_SHARES_OUTSTANDING' AND asset_id = o.share_asset_id), 0)::text,
		       coalesce((SELECT sum(b.balance) FROM ledger_accounts la JOIN ledger_balances b ON b.ledger_account_id = la.id
		                  WHERE la.value_domain = 'SIMULATED' AND la.asset_id = o.share_asset_id
		                    AND la.owner_type = 'CUSTOMER' AND la.code = 'WALLET'), 0)::text,
		       coalesce((SELECT balance FROM fac WHERE code = 'PREDICTION_ESCROW' AND asset_id = $3), 0)::text,
		       coalesce((SELECT balance FROM fac WHERE code = 'PREDICTION_COLLATERAL' AND asset_id = $3), 0)::text
		  FROM prediction_outcomes_registry o
		 WHERE o.market_id = $1
		 ORDER BY o.outcome_index`, m.ID, m.FacilityID, m.CollateralAssetID)
	if err != nil {
		return EscrowBooks{}, dbErr("read escrow books", err)
	}
	defer rows.Close()
	var b EscrowBooks
	for rows.Next() {
		var idx int16
		var issued, outstanding, held, escrow, collateral string
		if err := rows.Scan(&idx, &issued, &outstanding, &held, &escrow, &collateral); err != nil {
			return EscrowBooks{}, dbErr("scan escrow books", err)
		}
		vals := make([]money.Quantity, 5)
		for i, src := range []string{issued, outstanding, held, escrow, collateral} {
			if vals[i], err = money.ScanQuantity(src); err != nil {
				return EscrowBooks{}, errs.Wrap(err, errs.CodeInternal, "predmarket: a facility balance is not an integer")
			}
		}
		if int(idx) != len(b.SharesIssued) {
			return EscrowBooks{}, errs.New(errs.CodeInternal, "predmarket: the outcome registry has a gap")
		}
		b.SharesIssued = append(b.SharesIssued, vals[0])
		b.SharesOutstanding = append(b.SharesOutstanding, vals[1])
		b.HeldByCustomers = append(b.HeldByCustomers, vals[2])
		b.Escrow, b.Collateral = vals[3], vals[4]
	}
	if err := rows.Err(); err != nil {
		return EscrowBooks{}, dbErr("read escrow books", err)
	}
	if len(b.SharesIssued) != len(m.Outcomes) {
		return EscrowBooks{}, errs.New(errs.CodeInternal, "predmarket: the outcome registry does not match the market")
	}
	return b, nil
}

// disclosedResidual is the residual a REFUND settlement left on the books, or
// zero for a market that has not been refunded.
func (s *Service) disclosedResidual(ctx context.Context, q db.Querier, marketID MarketID) (money.Quantity, error) {
	st, found, err := s.SettlementOf(ctx, q, marketID)
	if err != nil || !found || st.Kind != SettlementRefund {
		return money.Quantity{}, err
	}
	return st.Residual, nil
}

// requireReconciled refuses unless the market's books satisfy the escrow
// identity right now.
func (s *Service) requireReconciled(ctx context.Context, q db.Querier, m Market) (EscrowBooks, error) {
	residual, err := s.disclosedResidual(ctx, q, m.ID)
	if err != nil {
		return EscrowBooks{}, err
	}
	b, err := s.Books(ctx, q, m)
	if err != nil {
		return EscrowBooks{}, err
	}
	if v := b.Violations(residual); len(v) > 0 {
		return EscrowBooks{}, errs.New(errs.CodeReconciliationRequired,
			"predmarket: the market's escrow does not reconcile with its outstanding shares").
			WithField("market_id", m.ID.String()).WithField("violations", v)
	}
	return b, nil
}

// assetOf loads an asset, mapping a missing one to NOT_FOUND.
func (s *Service) assetOf(ctx context.Context, q db.Querier, aid assets.AssetID) (assets.Asset, error) {
	a, err := s.assets.Get(ctx, q, aid)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return assets.Asset{}, err
		}
		return assets.Asset{}, dbErr("load asset", err)
	}
	return a, nil
}
