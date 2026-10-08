package advancedmetrics

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
)

// The counters doc.go lists under execution, circuit breakers, authority
// violations and network violations, counted from rows in the collector's
// window like the others.

// Pair is the two attribute values of a counter that carries two: a breaker
// trip's (kind, reason), an authority violation's (kind, stage).
type Pair [2]string

// Circuit breakers, as the attribute kind of advanced_circuit_breaker_events.
const (
	// BreakerStableAsset is a stable asset paused by its breaker; the reason
	// is the alert that tripped it (stable_peg_alerts.kind: PRICE_DEVIATION,
	// RESERVE_SHORTFALL or SUPPLY_MISMATCH).
	BreakerStableAsset = "STABLE_ASSET"
	// BreakerBridgeRoute is a bridge route paused by its breaker, whose only
	// reason is ReasonReconciliationMismatch.
	BreakerBridgeRoute = "BRIDGE_ROUTE"
	// ReasonReconciliationMismatch: the route's books disagree with the
	// chains (bridge.Service.ReconcileRoute).
	ReasonReconciliationMismatch = "RECONCILIATION_MISMATCH"
)

// Stages of a refused delegated request, as the attribute stage of
// advanced_authority_violations.
const (
	// StageAdmission: refused before anything was planned -- the agent asked
	// for what its grant does not cover.
	StageAdmission = "ADMISSION"
	// StageExecution: an admitted swap or withdrawal refused when it was
	// checked again before signing or submission -- the grant no longer
	// covered it (revoked, expired, a limit used up, a destination removed).
	StageExecution = "EXECUTION"
)

// Counted rows in the window ($1, $2].
const (
	// Every order the internal books recorded, customer or house.
	bookOrdersSQL = `SELECT owner_type, count(*) FROM book_orders WHERE created_at > $1 AND created_at <= $2 GROUP BY owner_type`
	// An order rejected -- at acceptance, at any later stage or by the
	// engine -- never changes again (OB003), so its updated_at is the
	// instant it was rejected, or born rejected.
	bookOrdersRejectedSQL = `SELECT owner_type, count(*) FROM book_orders
		WHERE status = 'REJECTED' AND updated_at > $1 AND updated_at <= $2 GROUP BY owner_type`
	// An order's first fill is its first trade, as maker or taker: counted
	// in the window of that trade and in no other.
	bookOrdersFirstFilledSQL = `SELECT f.owner_type, count(*) FROM (
			SELECT DISTINCT s.order_id, s.owner_type FROM book_trades t
			CROSS JOIN LATERAL (VALUES (t.maker_order_id, t.maker_owner_type), (t.taker_order_id, t.taker_owner_type)) AS s(order_id, owner_type)
			WHERE t.created_at > $1 AND t.created_at <= $2) f
		WHERE NOT EXISTS (SELECT 1 FROM book_trades p WHERE p.created_at <= $1 AND (p.maker_order_id = f.order_id OR p.taker_order_id = f.order_id))
		GROUP BY f.owner_type`
	// The paper venue's fills, at the reference mark (the SIM_TRADE's own
	// notional; the spread and the fees are disclosed charges beside it).
	paperFillsSQL = `SELECT quote_asset_id, sum(notional_at_reference)::text, count(*) FROM paper_fills
		WHERE created_at > $1 AND created_at <= $2 GROUP BY quote_asset_id ORDER BY quote_asset_id`
	// A stable asset's breaker writes exactly one tripping alert per pause.
	stableBreakerTripsSQL = `SELECT kind, count(*) FROM stable_peg_alerts
		WHERE breaker_tripped AND created_at > $1 AND created_at <= $2 GROUP BY kind`
	// A route's breaker pauses it as its own SYSTEM actor ($3..$6).
	routeBreakerTripsSQL = `SELECT count(*) FROM bridge_route_transitions
		WHERE from_status = $3 AND to_status = $4 AND actor_type = $5 AND actor_id = $6 AND occurred_at > $1 AND occurred_at <= $2`
	// A delegated intent rejected under a violation code ($4): at admission
	// when no swap or withdrawal was planned for it, at execution (the
	// standing check before signing, or the expiry sweep's) when one was.
	// The REJECTED transition carries its service's clock.
	violationIntentsSQL = `SELECT i.capability,
			CASE WHEN EXISTS (SELECT 1 FROM delegated_swaps s WHERE s.intent_id = i.id)
			       OR EXISTS (SELECT 1 FROM delegated_withdrawals w WHERE w.intent_id = i.id) THEN $5::text ELSE $6::text END,
			count(*)
		FROM economic_intent_transitions t JOIN economic_intents i ON i.id = t.intent_id
		WHERE t.to_status = 'REJECTED' AND t.occurred_at > $1 AND t.occurred_at <= $2
		  AND i.capability = ANY($3) AND i.rejection_code = ANY($4)
		GROUP BY 1, 2`
	// A delegated admission Risk V2 refused for its authority section ($4).
	violationRiskSQL = `SELECT i.capability, count(*) FROM advanced_risk_decisions d JOIN economic_intents i ON i.id = d.intent_id
		WHERE d.verdict = 'REJECT' AND d.created_at > $1 AND d.created_at <= $2
		  AND i.capability = ANY($3) AND d.reason_codes && $4::text[]
		GROUP BY 1`
	// The signing provider's refusal naming a violation code ($3).
	violationSigningSQL = `SELECT CASE WHEN swap_id IS NOT NULL THEN $4::text ELSE $5::text END, count(*) FROM delegated_signing_decisions
		WHERE verdict = 'REJECTED' AND created_at > $1 AND created_at <= $2 AND reason_codes && $3::text[]
		GROUP BY 1`
	// A signed swap or withdrawal stopped at the last check before its
	// submission for a violation ($3: BLOCKED_BEFORE_SUBMISSION:<code>).
	violationBlockedSQL = `SELECT $4::text, count(*) FROM delegated_swap_transitions t JOIN delegated_swaps s ON s.id = t.swap_id
			WHERE t.to_status = 'FAILED' AND t.occurred_at > $1 AND t.occurred_at <= $2 AND s.failure_code = ANY($3)
		UNION ALL
		SELECT $5::text, count(*) FROM delegated_withdrawal_transitions t JOIN delegated_withdrawals w ON w.id = t.withdrawal_id
			WHERE t.to_status = 'FAILED' AND t.occurred_at > $1 AND t.occurred_at <= $2 AND w.failure_code = ANY($3)`
	// The versions the scanner refused with a forbidden import ($3..$5).
	scanRefusalsSQL = `SELECT version_id FROM marketplace_version_validations
		WHERE outcome = $3 AND failure_reason = $4 AND $5 = ANY(scan_codes) AND created_at > $1 AND created_at <= $2
		ORDER BY version_id`
	versionSourceSQL = `SELECT path, content FROM marketplace_version_sources WHERE version_id = $1 ORDER BY path`
)

// execution counts the book orders recorded, filled for the first time and
// rejected, per owner type, and the paper venue's fill notional.
func (c *Collector) execution(ctx context.Context, tx pgx.Tx, w *Counted) error {
	w.BookOrders, w.BookOrdersFilled, w.BookOrdersRejected = map[string]int64{}, map[string]int64{}, map[string]int64{}
	for _, x := range []struct {
		dst map[string]int64
		sql string
	}{{w.BookOrders, bookOrdersSQL}, {w.BookOrdersFilled, bookOrdersFirstFilledSQL}, {w.BookOrdersRejected, bookOrdersRejectedSQL}} {
		if err := countBy(ctx, tx, x.dst, x.sql, w.From, w.To); err != nil {
			return err
		}
	}
	var err error
	w.ExternalRouteVolume, w.UnvaluedRouteFills, err = c.volume(ctx, tx, paperFillsSQL, w)
	return err
}

// breakers counts the circuit breakers that tripped, per kind and reason.
func (c *Collector) breakers(ctx context.Context, tx pgx.Tx, w *Counted) error {
	w.CircuitBreakerEvents = map[Pair]int64{}
	stable := map[string]int64{}
	if err := countBy(ctx, tx, stable, stableBreakerTripsSQL, w.From, w.To); err != nil {
		return err
	}
	for reason, n := range stable {
		w.CircuitBreakerEvents[Pair{BreakerStableAsset, reason}] += n
	}
	var routes int64
	if err := tx.QueryRow(ctx, routeBreakerTripsSQL, w.From, w.To, string(bridge.RouteActive), string(bridge.RoutePaused),
		string(security.ActorSystem), bridge.BreakerActorID).Scan(&routes); err != nil {
		return err
	}
	if routes > 0 {
		w.CircuitBreakerEvents[Pair{BreakerBridgeRoute, ReasonReconciliationMismatch}] += routes
	}
	return nil
}

// delegatedKind is the subject a delegated capability's intent is about.
func delegatedKind(capability string) string {
	if capability == string(gates.AgentWithdrawals) {
		return string(delegation.SubjectWithdrawal)
	}
	return string(delegation.SubjectSwap)
}

// authority counts the refused delegated requests that violated authority,
// per subject kind and stage. The four sources are disjoint: a request
// refused under a violation code is rejected with that code; one Risk V2
// refused is rejected with RISK_REJECTED; one the signing provider refused,
// with SIGNING_REJECTED; and one stopped before submission fails rather than
// being rejected.
func (c *Collector) authority(ctx context.Context, tx pgx.Tx, w *Counted) error {
	w.AuthorityViolations = map[Pair]int64{}
	capabilities := []string{string(gates.AgentSigning), string(gates.AgentWithdrawals)}
	codes := delegation.AuthorityViolationCodes()
	swap, withdrawal := string(delegation.SubjectSwap), string(delegation.SubjectWithdrawal)

	byIntent := map[Pair]int64{}
	if err := countPairs(ctx, tx, byIntent, violationIntentsSQL, w.From, w.To, capabilities, codes, StageExecution, StageAdmission); err != nil {
		return err
	}
	for k, n := range byIntent {
		w.AuthorityViolations[Pair{delegatedKind(k[0]), k[1]}] += n
	}
	byRisk := map[string]int64{}
	if err := countBy(ctx, tx, byRisk, violationRiskSQL, w.From, w.To, capabilities, delegation.AuthorityViolationRiskReasons()); err != nil {
		return err
	}
	for capability, n := range byRisk {
		w.AuthorityViolations[Pair{delegatedKind(capability), StageAdmission}] += n
	}
	blocked := make([]string, 0, len(codes))
	for _, code := range codes {
		blocked = append(blocked, delegation.BlockedBeforeSubmission(code))
	}
	for _, x := range []struct {
		sql  string
		args []any
	}{
		{violationSigningSQL, []any{w.From, w.To, codes, swap, withdrawal}},
		{violationBlockedSQL, []any{w.From, w.To, blocked, swap, withdrawal}},
	} {
		byKind := map[string]int64{}
		if err := countBy(ctx, tx, byKind, x.sql, x.args...); err != nil {
			return err
		}
		for kind, n := range byKind {
			if n > 0 {
				w.AuthorityViolations[Pair{kind, StageExecution}] += n
			}
		}
	}
	return nil
}

// network counts the strategy versions the scanner refused for an import
// that would reach the network or another process: it reads which versions
// the scanner refused with a forbidden import (their validation rows), and
// asks the scanner which imports those were, from each version's stored
// source -- immutable and checked against the version's digest when it was
// stored. A version counts once, however many such imports it has.
func (c *Collector) network(ctx context.Context, tx pgx.Tx, w *Counted) error {
	rows, err := tx.Query(ctx, scanRefusalsSQL, w.From, w.To, string(marketplace.ValidationFailed), sandboxrt.BuildFailureScan,
		string(sandboxrt.ScanImportForbidden))
	if err != nil {
		return err
	}
	var versions []marketplace.VersionID
	for rows.Next() {
		var v marketplace.VersionID
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		versions = append(versions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, v := range versions {
		src, err := versionSource(ctx, tx, v)
		if err != nil {
			return err
		}
		if len(sandboxrt.Scan(src, sandboxrt.DefaultScanLimits()).NetworkOrProcessImports()) > 0 {
			w.SandboxNetworkViolations++
		}
	}
	return nil
}

func versionSource(ctx context.Context, tx pgx.Tx, v marketplace.VersionID) (sandboxrt.Source, error) {
	rows, err := tx.Query(ctx, versionSourceSQL, v)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	src := sandboxrt.Source{}
	for rows.Next() {
		var path string
		var content []byte
		if err := rows.Scan(&path, &content); err != nil {
			return nil, err
		}
		src[path] = content
	}
	return src, rows.Err()
}

// recordEvents writes a window's execution, breaker, authority and network
// counts to the instruments. Both rate pairs share their denominator: every
// book order recorded.
func (c *Collector) recordEvents(ctx context.Context, w *Counted) {
	m, extra := c.cfg.Metrics, c.cfg.Extra
	owner := func(o string) metric.MeasurementOption {
		return observability.WithSafeAttrs(attribute.String("owner_type", o))
	}
	for _, r := range []struct {
		pair          observability.RatePair
		orders, which map[string]int64
	}{
		{m.FillRate, w.BookOrders, w.BookOrdersFilled},
		{m.RejectionRate, w.BookOrders, w.BookOrdersRejected},
	} {
		for _, o := range sortedKeys(r.orders) {
			r.pair.Denominator.Add(ctx, r.orders[o], owner(o))
		}
		for _, o := range sortedKeys(r.which) {
			r.pair.Numerator.Add(ctx, r.which[o], owner(o))
		}
	}
	m.ExternalRouteVolume.Add(ctx, w.ExternalRouteVolume)
	extra.UnvaluedRouteFills.Add(ctx, w.UnvaluedRouteFills)
	for _, k := range sortedPairs(w.CircuitBreakerEvents) {
		m.CircuitBreakerEvents.Add(ctx, w.CircuitBreakerEvents[k],
			observability.WithSafeAttrs(attribute.String("kind", k[0]), attribute.String("reason", k[1])))
	}
	for _, k := range sortedPairs(w.AuthorityViolations) {
		m.AuthorityViolations.Add(ctx, w.AuthorityViolations[k],
			observability.WithSafeAttrs(attribute.String("kind", k[0]), attribute.String("stage", k[1])))
	}
	m.SandboxNetworkViolations.Add(ctx, w.SandboxNetworkViolations)
}

// countPairs adds a (key, key, count) query's rows into dst.
func countPairs(ctx context.Context, q pgx.Tx, dst map[Pair]int64, sql string, args ...any) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a, b string
		var n int64
		if err := rows.Scan(&a, &b, &n); err != nil {
			return err
		}
		dst[Pair{a, b}] += n
	}
	return rows.Err()
}

func sortedPairs(m map[Pair]int64) []Pair {
	out := make([]Pair, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}
