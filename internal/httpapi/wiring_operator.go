package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/valuation"
)

// OperatorDeps holds what the operator console reads and writes. Prices,
// Audit and a positive MaxMarkAge are required together (with the kill
// switch controller of WireDeps); leaving any out leaves /advanced/ops
// UNSUPPORTED rather than half-answering.
type OperatorDeps struct {
	// Prices records and reads the sandbox reference marks.
	Prices *valuation.PriceStore
	// Audit receives the audit event of every recorded mark.
	Audit audit.Writer
	// MaxMarkAge is how old a mark may be when it is recorded, and the age
	// past which the console reports a mark stale. It should be the bound the
	// deployment's readers accept a sandbox mark at.
	MaxMarkAge time.Duration
	// Chains is the devnet host, when this deployment runs one. Nil answers
	// UNSUPPORTED on /advanced/ops/chains: there is no chain to report.
	Chains *devnethost.Host
}

func (d OperatorDeps) complete() bool {
	return d.Prices != nil && d.Audit != nil && d.MaxMarkAge > 0
}

// ReferenceMarkMaxScale is the most fractional digits a reference mark may
// carry: the price scale bound of the canonical instruments
// (instruments.Terms.PriceScale), beyond which a price is over-precise.
const ReferenceMarkMaxScale = 18

// referenceMarkMaxDigits bounds the mantissa to what asset_prices stores
// (numeric(38,0)).
const referenceMarkMaxDigits = 38

// opsListBound bounds every list of the overview.
const opsListBound = 100

// wireOperator wires the operator console when its dependencies are present.
func wireOperator(p *Ports, d WireDeps) {
	o := d.Advanced.Operator
	if !o.complete() || d.KillSwitches == nil {
		return
	}
	p.Operator = operatorAdapter{
		db: d.DB, clk: d.Clock, env: d.Env, prices: o.Prices, audit: o.Audit, maxAge: o.MaxMarkAge,
		kills: d.KillSwitches, assets: assets.NewRepository(), chains: o.Chains,
	}
}

// operatorAdapter serves OperatorPort. Reads run in one read-only REPEATABLE
// READ snapshot; recording a mark runs in its own transaction with its audit
// event. It is the one place the rules of a sandbox reference mark live:
// there is no domain package of marks, only valuation's price store.
type operatorAdapter struct {
	db     *db.DB
	clk    clock.Clock
	env    config.Environment
	prices *valuation.PriceStore
	audit  audit.Writer
	maxAge time.Duration
	kills  *killswitch.Controller
	assets *assets.Repository
	chains *devnethost.Host
}

var _ OperatorPort = operatorAdapter{}

func (a operatorAdapter) snapshot(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

// --- reference marks ----------------------------------------------------------

var referencePricePattern = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?$`)

// parseReferencePrice reads a price exactly as typed: no rounding, no
// normalisation. It refuses anything but a positive plain decimal with at
// most ReferenceMarkMaxScale fractional digits that asset_prices can store.
func parseReferencePrice(raw string) (money.Quantity, int32, error) {
	m := referencePricePattern.FindStringSubmatch(raw)
	if m == nil {
		return money.Quantity{}, 0, validationError("price",
			"price must be a plain decimal of sandbox dollars per whole unit, such as 2500.125, written out exactly")
	}
	whole, frac := m[1], m[2]
	if len(frac) > ReferenceMarkMaxScale {
		return money.Quantity{}, 0, errs.Newf(errs.CodePrecisionLoss,
			"price has %d fractional digits; a reference mark carries at most %d", len(frac), ReferenceMarkMaxScale).
			WithField("field", "price").WithField("max_scale", ReferenceMarkMaxScale)
	}
	digits := strings.TrimLeft(whole+frac, "0")
	if len(digits) > referenceMarkMaxDigits {
		return money.Quantity{}, 0, validationError("price", "price has more significant digits than a price observation stores (38)")
	}
	q, err := money.ParseQuantity(whole + frac)
	if err != nil {
		return money.Quantity{}, 0, validationError("price", "price is not a representable decimal")
	}
	if !q.IsPositive() {
		return money.Quantity{}, 0, validationError("price", "price must be positive: a SIMULATED asset is never valued at zero")
	}
	return q, int32(len(frac)), nil // #nosec G115 -- at most ReferenceMarkMaxScale by the check above
}

// validReferenceSource accepts 1..100 bytes of printable text without
// surrounding whitespace: the label every reader compares (a stable asset's
// peg monitor counts only its own oracle source).
func validReferenceSource(s string) error {
	if s == "" || len(s) > 100 || strings.TrimSpace(s) != s || !utf8.ValidString(s) {
		return validationError("source", "source must be 1..100 bytes of printable text without surrounding whitespace")
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return validationError("source", "source must be 1..100 bytes of printable text without surrounding whitespace")
		}
	}
	return nil
}

func (a operatorAdapter) RecordMark(ctx context.Context, r ReferenceMarkCommand) (ReferenceMarkRecorded, error) {
	if r.Actor.Type != security.ActorOperator || strings.TrimSpace(r.Actor.ID) == "" {
		return ReferenceMarkRecorded{}, errs.New(errs.CodeForbidden, "a reference mark is recorded by an identified operator")
	}
	mantissa, scale, err := parseReferencePrice(r.Price)
	if err != nil {
		return ReferenceMarkRecorded{}, err
	}
	if err := validReferenceSource(r.Source); err != nil {
		return ReferenceMarkRecorded{}, err
	}
	if r.AssetID == econ.SandboxDollarID() {
		return ReferenceMarkRecorded{}, validationError("asset_id", "the sandbox dollar is the quote of every reference mark; it has no mark of its own")
	}
	if r.QuoteAssetID != nil && *r.QuoteAssetID != econ.SandboxDollarID() {
		return ReferenceMarkRecorded{}, validationError("quote_asset_id", "a reference mark is quoted in the sandbox dollar and nothing else").
			WithField("sandbox_dollar_id", econ.SandboxDollarID().String())
	}
	now := a.clk.Now().UTC()
	observed := now
	if r.ObservedAt != nil {
		observed = r.ObservedAt.UTC()
	}
	if observed.After(now) {
		return ReferenceMarkRecorded{}, validationError("observed_at", "observed_at is in the future; every reader would ignore the mark")
	}
	if now.Sub(observed) > a.maxAge {
		return ReferenceMarkRecorded{}, validationError("observed_at",
			"observed_at is older than the maximum mark age; the mark would be stale before anyone could use it").
			WithField("max_age_ms", a.maxAge.Milliseconds())
	}
	var out ReferenceMarkRecorded
	err = a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		quote, err := operatorSandboxDollar(ctx, tx)
		if err != nil {
			return err
		}
		asset, err := a.assets.Get(ctx, tx, r.AssetID)
		if err != nil {
			return err
		}
		if asset.Kind != assets.KindSimulated {
			return errs.New(errs.CodeForbidden, "a sandbox reference mark values a SIMULATED asset only").
				WithField("asset_id", asset.ID.String()).WithField("asset_kind", string(asset.Kind))
		}
		rec, err := a.prices.RecordPrice(ctx, tx, valuation.PriceObservation{
			AssetID: asset.ID, QuoteAssetID: quote.ID, Mantissa: mantissa, Scale: scale, Source: r.Source,
			ObservedAt: observed, ReceivedAt: now, RawRef: "operator:" + r.Actor.ID,
		})
		if err != nil {
			return err
		}
		mark, err := a.mark(ctx, tx, rec.ID, now)
		if err != nil {
			return err
		}
		if !rec.Duplicate {
			if err := a.auditMark(ctx, tx, r, asset, quote, mark, now); err != nil {
				return err
			}
		}
		out = ReferenceMarkRecorded{Mark: mark, AssetSymbol: asset.Symbol, QuoteSymbol: quote.Symbol, Duplicate: rec.Duplicate, MaxAge: a.maxAge}
		return nil
	})
	return out, err
}

func (a operatorAdapter) auditMark(ctx context.Context, tx pgx.Tx, r ReferenceMarkCommand, asset, quote assets.Asset, m ReferenceMark, now time.Time) error {
	price := m.Mantissa.ToDecimalString(uint8(m.Scale)) // #nosec G115 -- at most ReferenceMarkMaxScale
	body, err := json.Marshal(map[string]string{
		"price_id": m.ID.String(), "asset_id": asset.ID.String(), "asset_symbol": asset.Symbol,
		"quote_asset_id": quote.ID.String(), "mantissa": m.Mantissa.String(), "scale": strconv.Itoa(int(m.Scale)),
		"price": price, "source": m.Source, "observed_at": m.ObservedAt.Format(time.RFC3339Nano), "value_domain": "SIMULATED",
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "encode the reference mark's audit payload")
	}
	_, err = a.audit.Append(ctx, tx, audit.Event{
		Stream: audit.AdminStream, ActorType: string(r.Actor.Type), ActorID: r.Actor.ID,
		Action: "sandbox.reference_mark.recorded", ResourceType: "asset_price", ResourceID: m.ID.String(),
		CorrelationID: r.CorrelationID,
		Reason:        "sandbox reference mark of " + asset.Symbol + " at " + price + " " + quote.Symbol + " from " + m.Source,
		Payload:       body, OccurredAt: now,
	})
	return err
}

const referenceMarkColumns = `id, asset_id, quote_asset_id, mantissa::text, scale, source, observed_at, received_at`

func (a operatorAdapter) scanMark(row pgx.Row, now time.Time) (ReferenceMark, error) {
	var m ReferenceMark
	var mantissa string
	if err := row.Scan(&m.ID, &m.AssetID, &m.QuoteAssetID, &mantissa, &m.Scale, &m.Source, &m.ObservedAt, &m.ReceivedAt); err != nil {
		return ReferenceMark{}, err
	}
	q, err := money.ScanQuantity(mantissa)
	if err != nil {
		return ReferenceMark{}, errs.Wrap(err, errs.CodeInternal, "a stored price mantissa is not an integer")
	}
	m.Mantissa = q
	m.ObservedAt, m.ReceivedAt = m.ObservedAt.UTC(), m.ReceivedAt.UTC()
	m.Age = now.Sub(m.ObservedAt)
	m.Future = m.ObservedAt.After(now)
	m.Stale = m.Age > a.maxAge
	return m, nil
}

func (a operatorAdapter) mark(ctx context.Context, q db.Querier, id valuation.PriceID, now time.Time) (ReferenceMark, error) {
	m, err := a.scanMark(q.QueryRow(ctx, `SELECT `+referenceMarkColumns+` FROM asset_prices WHERE id = $1`, id), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReferenceMark{}, errs.New(errs.CodeInternal, "a recorded price observation could not be read back")
	}
	return m, err
}

func (a operatorAdapter) Marks(ctx context.Context, asset assets.AssetID, limit int) (ReferenceMarkList, error) {
	limit = clampLimit(limit)
	var out ReferenceMarkList
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := a.clk.Now().UTC()
		quote, err := operatorSandboxDollar(ctx, tx)
		if err != nil {
			return err
		}
		subject, err := a.assets.Get(ctx, tx, asset)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+referenceMarkColumns+` FROM asset_prices
			WHERE asset_id = $1 AND quote_asset_id = $2
			ORDER BY observed_at DESC, received_at DESC, id DESC LIMIT $3`, subject.ID, quote.ID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		items := []ReferenceMark{}
		for rows.Next() {
			m, err := a.scanMark(rows, now)
			if err != nil {
				return err
			}
			items = append(items, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = ReferenceMarkList{Asset: subject, Quote: quote, MaxAge: a.maxAge, AsOf: now, Items: items}
		return nil
	})
	return out, err
}

// operatorSandboxDollar is the quote of every reference mark. A deployment
// that has not provisioned it holds no simulated value to mark.
func operatorSandboxDollar(ctx context.Context, q db.Querier) (assets.Asset, error) {
	usd, err := econ.SandboxDollar(ctx, q)
	if errs.HasCode(err, errs.CodeNotFound) {
		return assets.Asset{}, errs.New(errs.CodeUnsupported,
			"the sandbox dollar is not provisioned in this deployment, so there is no reference to mark against")
	}
	return usd, err
}

// --- the overview ------------------------------------------------------------------

// opsAdvancedKinds are the reconciliation kinds of the advanced capabilities
// (ADVANCED_FINANCIAL_MODEL §6, step 13), in the order the overview lists them.
var opsAdvancedKinds = []reconciliation.Kind{
	reconciliation.KindEconomicEffect, reconciliation.KindStableReserve, reconciliation.KindBridge,
	reconciliation.KindPredictionEscrow, reconciliation.KindClearing, reconciliation.KindDevnetChain,
	reconciliation.KindBookJournal, reconciliation.KindTransfers,
}

func opsUnresolvedStatuses() []string {
	var out []string
	for _, s := range reconciliation.AllStatuses() {
		if s.Unresolved() {
			out = append(out, string(s))
		}
	}
	return out
}

func (a operatorAdapter) Overview(ctx context.Context) (OpsOverview, error) {
	var out OpsOverview
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := a.clk.Now().UTC()
		v := OpsOverview{GeneratedAt: now, Environment: string(a.env), MaxMarkAge: a.maxAge, ChainsConfigured: a.chains != nil}
		var err error
		if v.KillSwitches, err = a.kills.Active(ctx, tx); err != nil {
			return err
		}
		if v.StableAssets, err = opsStableAssets(ctx, tx); err != nil {
			return err
		}
		if v.BridgeRoutes, err = opsBridgeRoutes(ctx, tx); err != nil {
			return err
		}
		if v.BridgeLegsInReview, err = opsBridgeLegsInReview(ctx, tx); err != nil {
			return err
		}
		if v.Reconciliation, err = opsReconciliation(ctx, tx, now); err != nil {
			return err
		}
		if v.DualControl, err = opsDualControl(ctx, tx); err != nil {
			return err
		}
		if v.Oracle, err = a.opsOracle(ctx, tx, now); err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

// opsRecordColumns are the reconciliation columns of a LATERAL join named rr.
const opsRecordColumns = `rr.id, rr.kind, rr.status, rr.material, rr.blocks_new_risk, rr.opened_at, rr.resolved_at`

func opsScanRecord(id *uuid.UUID, kind, status *string, material, blocks *bool, opened *time.Time, resolved *time.Time) *OpsReconciliationRecord {
	if id == nil {
		return nil
	}
	r := &OpsReconciliationRecord{ID: *id, Kind: deref(kind), Status: deref(status)}
	if material != nil {
		r.Material = *material
	}
	if blocks != nil {
		r.BlocksNewRisk = *blocks
	}
	if opened != nil {
		r.OpenedAt = opened.UTC()
	}
	if resolved != nil {
		t := resolved.UTC()
		r.ResolvedAt = &t
	}
	return r
}

func opsStableAssets(ctx context.Context, tx pgx.Tx) ([]OpsStableAsset, error) {
	rows, err := tx.Query(ctx, `SELECT s.id, s.name, s.asset_id, s.chain_id, s.status,
			al.kind, al.severity, al.breaker_tripped, al.observed_at, `+opsRecordColumns+`
		FROM stable_assets s
		LEFT JOIN LATERAL (SELECT kind, severity, breaker_tripped, observed_at FROM stable_peg_alerts
			WHERE stable_asset_id = s.id ORDER BY observed_at DESC, id DESC LIMIT 1) al ON true
		LEFT JOIN LATERAL (SELECT id, kind, status, material, blocks_new_risk, opened_at, resolved_at FROM reconciliation_records
			WHERE kind = $1 AND scope_type = $2 AND scope_id = s.id::text ORDER BY opened_at DESC, id DESC LIMIT 1) rr ON true
		ORDER BY s.name, s.id LIMIT $3`,
		string(reconciliation.KindStableReserve), stableasset.ScopeStableAsset, opsListBound)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OpsStableAsset{}
	for rows.Next() {
		var s OpsStableAsset
		var alertKind, alertSeverity *string
		var alertTripped *bool
		var alertAt *time.Time
		var rid *uuid.UUID
		var rkind, rstatus *string
		var rmaterial, rblocks *bool
		var ropened, rresolved *time.Time
		if err := rows.Scan(&s.ID, &s.Name, &s.AssetID, &s.ChainID, &s.Status, &alertKind, &alertSeverity, &alertTripped, &alertAt,
			&rid, &rkind, &rstatus, &rmaterial, &rblocks, &ropened, &rresolved); err != nil {
			return nil, err
		}
		if alertKind != nil && alertAt != nil {
			s.LatestAlert = &OpsPegAlert{Kind: *alertKind, Severity: deref(alertSeverity), ObservedAt: alertAt.UTC()}
			if alertTripped != nil {
				s.LatestAlert.BreakerTripped = *alertTripped
			}
		}
		s.LatestSupplyRecord = opsScanRecord(rid, rkind, rstatus, rmaterial, rblocks, ropened, rresolved)
		out = append(out, s)
	}
	return out, rows.Err()
}

func opsBridgeRoutes(ctx context.Context, tx pgx.Tx) ([]OpsBridgeRoute, error) {
	rows, err := tx.Query(ctx, `SELECT b.id, b.name, b.source_chain, b.destination_chain, b.status, `+opsRecordColumns+`
		FROM bridge_routes b
		LEFT JOIN LATERAL (SELECT id, kind, status, material, blocks_new_risk, opened_at, resolved_at FROM reconciliation_records
			WHERE kind = $1 AND scope_type = $2 AND scope_id = b.id::text AND status = ANY($3::text[])
			ORDER BY opened_at DESC, id DESC LIMIT 1) rr ON true
		ORDER BY b.name, b.id LIMIT $4`,
		string(reconciliation.KindBridge), bridge.ScopeBridgeRoute, opsUnresolvedStatuses(), opsListBound)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OpsBridgeRoute{}
	for rows.Next() {
		var b OpsBridgeRoute
		var rid *uuid.UUID
		var rkind, rstatus *string
		var rmaterial, rblocks *bool
		var ropened, rresolved *time.Time
		if err := rows.Scan(&b.ID, &b.Name, &b.SourceChain, &b.DestinationChain, &b.Status,
			&rid, &rkind, &rstatus, &rmaterial, &rblocks, &ropened, &rresolved); err != nil {
			return nil, err
		}
		b.OpenReconciliation = opsScanRecord(rid, rkind, rstatus, rmaterial, rblocks, ropened, rresolved)
		out = append(out, b)
	}
	return out, rows.Err()
}

// opsBridgeLegsInReview reads the legs in MANUAL_REVIEW, the longest waiting
// first, with when and why each entered review (its latest transition into
// it) and the facts that decide which way out the bridge service admits.
func opsBridgeLegsInReview(ctx context.Context, tx pgx.Tx) ([]OpsBridgeLeg, error) {
	rows, err := tx.Query(ctx, `SELECT l.id, l.route_id, r.name, l.account_id, l.quantity::text, r.decimals, sa.symbol,
			l.destination_journal_transaction_id IS NOT NULL, l.refund_effect_id IS NOT NULL, t.occurred_at, t.reason
		FROM bridge_legs l
		JOIN bridge_routes r ON r.id = l.route_id
		JOIN assets sa ON sa.id = r.source_asset_id
		LEFT JOIN LATERAL (SELECT occurred_at, reason FROM bridge_leg_transitions
			WHERE leg_id = l.id AND to_status = 'MANUAL_REVIEW' ORDER BY occurred_at DESC, id DESC LIMIT 1) t ON true
		WHERE l.status = 'MANUAL_REVIEW'
		ORDER BY t.occurred_at NULLS LAST, l.id LIMIT $1`, opsListBound)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OpsBridgeLeg{}
	for rows.Next() {
		var l OpsBridgeLeg
		var quantity string
		var decimals int16
		var entered *time.Time
		var reason *string
		if err := rows.Scan(&l.ID, &l.RouteID, &l.RouteName, &l.AccountID, &quantity, &decimals, &l.SourceSymbol,
			&l.Released, &l.RefundRequested, &entered, &reason); err != nil {
			return nil, err
		}
		q, err := money.ScanQuantity(quantity)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "a stored leg quantity is not an integer")
		}
		if decimals < 0 || decimals > 18 {
			return nil, errs.New(errs.CodeInternal, "a stored route has decimals out of range")
		}
		l.Quantity, l.Decimals = q, uint8(decimals) // #nosec G115 -- 0..18 by the check above
		if entered != nil {
			t := entered.UTC()
			l.EnteredReviewAt = &t
		}
		l.ReviewReason = deref(reason)
		out = append(out, l)
	}
	return out, rows.Err()
}

func opsReconciliation(ctx context.Context, tx pgx.Tx, now time.Time) ([]OpsReconciliationKind, error) {
	kinds := make([]string, 0, len(opsAdvancedKinds))
	for _, k := range opsAdvancedKinds {
		kinds = append(kinds, string(k))
	}
	rows, err := tx.Query(ctx, `SELECT kind, count(*), count(*) FILTER (WHERE material), count(*) FILTER (WHERE blocks_new_risk),
			min(opened_at)
		FROM reconciliation_records WHERE kind = ANY($1::text[]) AND status = ANY($2::text[]) GROUP BY kind`,
		kinds, opsUnresolvedStatuses())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]OpsReconciliationKind{}
	for rows.Next() {
		var k OpsReconciliationKind
		var oldest *time.Time
		if err := rows.Scan(&k.Kind, &k.Unresolved, &k.Material, &k.BlocksNewRisk, &oldest); err != nil {
			return nil, err
		}
		if oldest != nil {
			t := oldest.UTC()
			age := now.Sub(t)
			k.OldestOpened, k.OldestAge = &t, &age
		}
		found[k.Kind] = k
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]OpsReconciliationKind, 0, len(kinds))
	for _, k := range kinds {
		if v, ok := found[k]; ok {
			out = append(out, v)
			continue
		}
		out = append(out, OpsReconciliationKind{Kind: k})
	}
	return out, nil
}

// opsDualControl counts what awaits a second operator: transfer reversal
// proposals with no decision (migration 00758), cases in MANUAL_REVIEW, and
// resolutions with no decision that answer the review their case is in now
// (00754's cp_margin_review_left).
func opsDualControl(ctx context.Context, tx pgx.Tx) (OpsDualControl, error) {
	var d OpsDualControl
	err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM transfer_reversal_proposals p
		  WHERE NOT EXISTS (SELECT 1 FROM transfer_reversal_decisions d WHERE d.proposal_id = p.id)),
		(SELECT count(*) FROM margin_liquidation_cases WHERE status = 'MANUAL_REVIEW'),
		(SELECT count(*) FROM margin_case_resolutions r JOIN margin_liquidation_cases k ON k.id = r.case_id
		  WHERE k.status = 'MANUAL_REVIEW'
		    AND NOT EXISTS (SELECT 1 FROM margin_case_resolution_decisions d WHERE d.resolution_id = r.id)
		    AND NOT cp_margin_review_left(r.review_transition_id))`).
		Scan(&d.TransferReversalsPending, &d.MarginCasesInReview, &d.MarginResolutionsPending)
	return d, err
}

// opsOracle reports, for every SIMULATED asset other than the sandbox dollar
// that someone holds (a non-zero balance under a holding code, any owner),
// the latest mark in the sandbox dollar observed no later than now.
func (a operatorAdapter) opsOracle(ctx context.Context, tx pgx.Tx, now time.Time) ([]OpsOracleAsset, error) {
	holding := make([]string, 0, len(ledger.HoldingCodes()))
	for _, c := range ledger.HoldingCodes() {
		holding = append(holding, string(c))
	}
	rows, err := tx.Query(ctx, `SELECT a.id, a.symbol, p.observed_at, p.source
		FROM assets a
		LEFT JOIN LATERAL (SELECT observed_at, source FROM asset_prices
			WHERE asset_id = a.id AND quote_asset_id = $1 AND observed_at <= $3
			ORDER BY observed_at DESC, received_at DESC, id DESC LIMIT 1) p ON true
		WHERE a.kind = $4 AND a.id <> $1
		  AND EXISTS (SELECT 1 FROM ledger_accounts la JOIN ledger_balances lb ON lb.ledger_account_id = la.id
		               WHERE la.asset_id = a.id AND la.code = ANY($2::text[]) AND lb.balance <> 0)
		ORDER BY a.symbol, a.id LIMIT $5`,
		econ.SandboxDollarID(), holding, now, string(assets.KindSimulated), opsListBound)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OpsOracleAsset{}
	for rows.Next() {
		var o OpsOracleAsset
		var observed *time.Time
		var source *string
		if err := rows.Scan(&o.AssetID, &o.Symbol, &observed, &source); err != nil {
			return nil, err
		}
		o.Stale = true
		if observed != nil {
			t := observed.UTC()
			age := now.Sub(t)
			o.ObservedAt, o.Source, o.Age = &t, deref(source), &age
			o.Stale = age > a.maxAge
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// --- the simulated chains --------------------------------------------------------

func (a operatorAdapter) Chains(ctx context.Context) (OpsChains, error) {
	if a.chains == nil {
		return OpsChains{}, errs.New(errs.CodeUnsupported, "no simulated chain host is configured in this deployment")
	}
	network := a.chains.Config().Network
	out := OpsChains{Network: network, ObservedAt: a.clk.Now().UTC()}
	err := a.chains.View(ctx, func(v devnethost.View) error {
		for _, chain := range devnethost.Chains() {
			tip, err := v.Tip(chain)
			if err != nil {
				return err
			}
			finalized, err := v.FinalizedHeight(chain)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, OpsChainTip{
				ChainID: string(chain), Slot: tip.Slot, Height: tip.Height, Hash: tip.Hash.String(), Final: tip.Final,
				FinalizedHeight: finalized,
			})
		}
		return nil
	})
	if err != nil {
		return OpsChains{}, err
	}
	// When the host last produced or reorganised blocks on each chain, from
	// its persisted history.
	err = a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for i := range out.Items {
			var at *time.Time
			if err := tx.QueryRow(ctx, `SELECT max(occurred_at) FROM devnet_events
				WHERE network = $1 AND chain_id = $2 AND kind IN ('PRODUCE_BLOCKS','REORG')`,
				network, out.Items[i].ChainID).Scan(&at); err != nil {
				return err
			}
			if at != nil {
				t := at.UTC()
				out.Items[i].LastBlockProducedAt = &t
			}
		}
		return nil
	})
	return out, err
}
