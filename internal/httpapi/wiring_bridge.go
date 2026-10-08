package httpapi

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// BridgeDeps holds what /advanced/bridge needs. The routes are wired only
// when every required field is present -- which in cmd/api means a sandbox
// chain is configured -- and answer UNSUPPORTED otherwise.
type BridgeDeps struct {
	// Service is the cross-chain capital controller. It decides every rule.
	Service *bridge.Service
	// Host is the simulated chain host the service runs on (shared with the
	// stable asset); the chain routes are its own calls.
	Host *devnethost.Host
	// Haircut is the deployment's haircut policy for value in flight, the
	// policy the holdings route reports the bridge's part of buying power
	// under. Its zero value is bridge.DefaultHaircutPolicy, which counts
	// nothing that is not final.
	Haircut bridge.HaircutPolicy
	// Identity, Verification and Jurisdiction are the account facts, as for
	// the stable asset (StableDeps): from the deployment, never a request.
	Identity     StableIdentityReader
	Verification VerificationResolver
	Jurisdiction JurisdictionResolver
}

func (d BridgeDeps) complete() bool {
	return d.Service != nil && d.Host != nil && d.Identity != nil
}

// wireBridge wires the bridge port when its dependencies are present.
func wireBridge(p *Ports, d WireDeps) {
	bd := d.Advanced.Bridge
	if !bd.complete() || d.DB == nil {
		return
	}
	policy := bd.Haircut
	if policy == (bridge.HaircutPolicy{}) {
		policy = bridge.DefaultHaircutPolicy()
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.System()
	}
	repo := d.Assets
	if repo == nil {
		repo = assets.NewRepository()
	}
	p.Bridge = bridgeAdapter{
		svc: bd.Service, host: bd.Host, haircut: policy, assets: repo, db: d.DB, clk: clk,
		facts: stableBridgeFacts{identity: bd.Identity, verification: bd.Verification, jurisdiction: bd.Jurisdiction},
	}
}

// bridgeAdapter serves BridgePort over the bridge service and the chain host.
//
// Bridge and ReconcileLeg call the service's own workflows, which own their
// transactions; each route command runs one service operation in its own READ
// COMMITTED transaction. Every read runs in one read-only REPEATABLE READ
// snapshot and reads the chains through it; where the package exports no
// listing (routes, an account's legs, a leg's attempts and transitions) the
// adapter reads its tables, strictly with SELECTs, and decides nothing.
type bridgeAdapter struct {
	svc     *bridge.Service
	host    *devnethost.Host
	haircut bridge.HaircutPolicy
	facts   stableBridgeFacts
	assets  *assets.Repository
	db      *db.DB
	clk     clock.Clock
}

var _ BridgePort = bridgeAdapter{}

func (a bridgeAdapter) read(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

func (a bridgeAdapter) write(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}, fn)
}

// bridgeLegKeyPrefix namespaces the client's Idempotency-Key for the leg's
// economic intent.
const bridgeLegKeyPrefix = "http:bridge:leg:"

const bridgeRouteIDsSQL = `SELECT id FROM bridge_routes ORDER BY created_at, id LIMIT $1`

const bridgeLegIDsSQL = `SELECT id FROM bridge_legs
	WHERE account_id = $1 AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $4`

const bridgeAttemptsSQL = `SELECT attempt, kind, chain_id, tx_id, blockhash_height, created_at
	FROM bridge_release_attempts WHERE leg_id = $1 ORDER BY attempt`

const bridgeLegTransitionsSQL = `SELECT from_status, to_status, actor_type, actor_id, reason, occurred_at
	FROM bridge_leg_transitions WHERE leg_id = $1 ORDER BY occurred_at, id`

// bridgeEffectStatesSQL reads the status of each of a leg's effects and, from
// their transition rows, when each last became UNKNOWN and last left it.
const bridgeEffectStatesSQL = `SELECT e.id, e.status,
	(SELECT max(t.occurred_at) FROM economic_effect_transitions t WHERE t.effect_id = e.id AND t.to_status = 'UNKNOWN'),
	(SELECT max(t.occurred_at) FROM economic_effect_transitions t WHERE t.effect_id = e.id AND t.from_status = 'UNKNOWN')
	FROM economic_effects e WHERE e.id = ANY($1::uuid[])`

// bridgeWalletHoldingsSQL is an account's WALLET balances of SIMULATED
// assets: what it holds on each chain, on the books.
const bridgeWalletHoldingsSQL = `SELECT la.asset_id, b.balance::text FROM ledger_accounts la
	JOIN ledger_balances b ON b.ledger_account_id = la.id
	WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1 AND la.code = 'WALLET' AND la.value_domain = 'SIMULATED'
	  AND b.balance <> 0
	ORDER BY la.asset_id`

// --- reads -----------------------------------------------------------------------

func (a bridgeAdapter) Routes(ctx context.Context, limit int) ([]BridgeRouteView, error) {
	var out []BridgeRouteView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out = nil
		rows, err := tx.Query(ctx, bridgeRouteIDsSQL, clampLimit(limit))
		if err != nil {
			return err
		}
		var ids []bridge.RouteID
		for rows.Next() {
			var rid bridge.RouteID
			if err := rows.Scan(&rid); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, rid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		l := newStableLabels(a.assets)
		for _, rid := range ids {
			r, err := a.svc.GetRoute(ctx, tx, rid)
			if err != nil {
				return err
			}
			v, err := bridgeRouteViewOf(ctx, tx, l, r)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

func (a bridgeAdapter) Route(ctx context.Context, rid bridge.RouteID) (BridgeRouteView, error) {
	var out BridgeRouteView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		r, err := a.svc.GetRoute(ctx, tx, rid)
		if err != nil {
			return err
		}
		out, err = bridgeRouteViewOf(ctx, tx, newStableLabels(a.assets), r)
		return err
	})
	return out, err
}

func (a bridgeAdapter) Legs(ctx context.Context, account accounts.AccountID, cursor string, limit int) (BridgeLegPage, error) {
	at, after, err := stableBridgeCursor(cursor)
	if err != nil {
		return BridgeLegPage{}, err
	}
	limit = clampLimit(limit)
	var page BridgeLegPage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		page = BridgeLegPage{}
		rows, err := tx.Query(ctx, bridgeLegIDsSQL, account, at, after, limit+1)
		if err != nil {
			return err
		}
		var ids []bridge.LegID
		for rows.Next() {
			var lid bridge.LegID
			if err := rows.Scan(&lid); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, lid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		c := a.newLegContext()
		for _, lid := range ids {
			leg, err := a.svc.GetLeg(ctx, tx, lid)
			if err != nil {
				return err
			}
			v, err := c.view(ctx, tx, leg)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, v)
		}
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
			last := page.Items[limit-1].Leg
			page.NextCursor = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
		}
		return nil
	})
	return page, err
}

func (a bridgeAdapter) Leg(ctx context.Context, lid bridge.LegID) (BridgeLegDetailView, error) {
	var out BridgeLegDetailView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		leg, err := a.svc.GetLeg(ctx, tx, lid)
		if err != nil {
			return err
		}
		v, err := a.newLegContext().view(ctx, tx, leg)
		if err != nil {
			return err
		}
		out = BridgeLegDetailView{BridgeLegView: v, Attempts: []BridgeAttemptView{}, Transitions: []BridgeLegTransition{}}
		rows, err := tx.Query(ctx, bridgeAttemptsSQL, lid)
		if err != nil {
			return err
		}
		for rows.Next() {
			var at BridgeAttemptView
			var kind, chain, txID string
			var height int64
			if err := rows.Scan(&at.Number, &kind, &chain, &txID, &height, &at.CreatedAt); err != nil {
				rows.Close()
				return err
			}
			if height < 0 {
				rows.Close()
				return errs.New(errs.CodeInternal, "a stored release attempt has a negative blockhash height")
			}
			at.Kind, at.Chain, at.TxID, at.BlockhashHeight = bridge.AttemptKind(kind), devnet.ChainID(chain), devnet.TxID(txID), uint64(height)
			at.CreatedAt = at.CreatedAt.UTC()
			out.Attempts = append(out.Attempts, at)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, bridgeLegTransitionsSQL, lid)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t BridgeLegTransition
			var to string
			if err := rows.Scan(&t.From, &to, &t.ActorType, &t.ActorID, &t.Reason, &t.OccurredAt); err != nil {
				rows.Close()
				return err
			}
			t.To, t.OccurredAt = bridge.CrossChainCapitalState(to), t.OccurredAt.UTC()
			out.Transitions = append(out.Transitions, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// What the chains say of the lock and of every attempt, at one
		// instant, read through this snapshot.
		return a.host.ViewIn(ctx, tx, func(view devnethost.View) error {
			if leg.LockTxID != "" {
				st, err := view.Status(v.Route.Route.SourceChain, leg.LockTxID)
				if err != nil {
					return err
				}
				out.Lock = &BridgeChainTxView{Chain: v.Route.Route.SourceChain, TxID: leg.LockTxID, Status: st}
			}
			for i := range out.Attempts {
				st, err := view.Status(out.Attempts[i].Chain, out.Attempts[i].TxID)
				if err != nil {
					return err
				}
				out.Attempts[i].OnChain = BridgeChainTxView{Chain: out.Attempts[i].Chain, TxID: out.Attempts[i].TxID, Status: st}
			}
			return nil
		})
	})
	return out, err
}

func (a bridgeAdapter) Holdings(ctx context.Context, account accounts.AccountID) (BridgeHoldingsView, error) {
	out := BridgeHoldingsView{AccountID: account, AsOf: a.clk.Now().UTC()}
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out.Chains, out.Labels = nil, map[assets.AssetID]StableAssetLabel{}
		rows, err := tx.Query(ctx, bridgeWalletHoldingsSQL, account.String())
		if err != nil {
			return err
		}
		type balance struct {
			asset assets.AssetID
			qty   money.Quantity
		}
		var balances []balance
		for rows.Next() {
			var b balance
			var text string
			if err := rows.Scan(&b.asset, &text); err != nil {
				rows.Close()
				return err
			}
			if b.qty, err = money.ScanQuantity(text); err != nil {
				rows.Close()
				return errs.Wrap(err, errs.CodeInternal, "a stored wallet balance is not an integer")
			}
			balances = append(balances, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		l := newStableLabels(a.assets)
		byChain := map[string][]BridgeHolding{}
		for _, b := range balances {
			asset, err := l.asset(ctx, tx, b.asset)
			if err != nil {
				return err
			}
			byChain[asset.Chain] = append(byChain[asset.Chain], BridgeHolding{
				AssetID: b.asset, Label: StableAssetLabel{Symbol: asset.Symbol, Decimals: asset.Decimals}, Quantity: b.qty,
			})
		}
		chains := make([]string, 0, len(byChain))
		for c := range byChain {
			chains = append(chains, c)
		}
		sort.Strings(chains)
		for _, c := range chains {
			hs := byChain[c]
			sort.Slice(hs, func(i, j int) bool {
				if hs[i].Label.Symbol != hs[j].Label.Symbol {
					return hs[i].Label.Symbol < hs[j].Label.Symbol
				}
				return hs[i].AssetID.String() < hs[j].AssetID.String()
			})
			out.Chains = append(out.Chains, BridgeChainHoldings{Chain: c, Holdings: hs})
		}
		// The bridge's part of buying power, under the deployment's haircut
		// policy; refused (RECONCILIATION_REQUIRED) when the legs and the
		// ledger disagree.
		if out.CrossChain, err = a.svc.BuyingPower(ctx, tx, account, a.haircut); err != nil {
			return err
		}
		for _, leg := range out.CrossChain.Legs {
			if out.Labels[leg.AssetID], err = l.label(ctx, tx, leg.AssetID); err != nil {
				return err
			}
		}
		for _, h := range out.CrossChain.Holdings {
			aid, perr := assets.ParseAssetID(h.AssetID)
			if perr != nil {
				return errs.Wrap(perr, errs.CodeInternal, "the bridge named an in-flight holding by an asset id that does not parse")
			}
			if out.Labels[aid], err = l.label(ctx, tx, aid); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (a bridgeAdapter) Chains(ctx context.Context) (BridgeChainsView, error) {
	cfg := a.host.Config()
	out := BridgeChainsView{Network: cfg.Network, FaultInjectionAllowed: cfg.FaultInjectionAllowed()}
	err := a.host.View(ctx, func(v devnethost.View) error {
		out.Chains = nil
		for _, id := range devnethost.Chains() {
			cv, err := bridgeChainOf(v, cfg, id)
			if err != nil {
				return err
			}
			out.Chains = append(out.Chains, cv)
		}
		return nil
	})
	return out, err
}

// --- the customer's command ---------------------------------------------------------

func (a bridgeAdapter) Bridge(ctx context.Context, c BridgeLegCommand) (BridgeLegOutcome, error) {
	facts, err := a.facts.of(ctx, a.db, c.AccountID)
	if err != nil {
		return BridgeLegOutcome{}, err
	}
	res, err := a.svc.Bridge(ctx, bridge.BridgeRequest{
		RouteID: c.RouteID, AccountID: c.AccountID, Quantity: c.Quantity, IdempotencyKey: bridgeLegKeyPrefix + c.IdempotencyKey,
		Mode: c.Mode, Actor: c.Actor, CorrelationID: c.CorrelationID,
		Facts: bridge.AccountFacts{Jurisdiction: facts.jurisdiction, Verification: facts.verification, IdentityVerified: facts.identityVerified},
	})
	unknown := err != nil && errs.HasCode(err, errs.CodeSubmissionStateUnknown) && !res.Leg.ID.IsZero()
	if err != nil && !unknown {
		return BridgeLegOutcome{}, err
	}
	// The leg exists: read it back as recorded. A lock whose submission
	// outcome is unknown is neither a success nor a failure; the leg waits,
	// with its effect UNKNOWN, to be settled from the chain by transaction id.
	v, verr := a.legView(ctx, res.Leg.ID)
	if verr != nil {
		return BridgeLegOutcome{}, verr
	}
	return BridgeLegOutcome{View: v, Replay: res.Replay, Settled: !unknown && v.Leg.Status.Terminal()}, nil
}

// --- the operator's commands ------------------------------------------------------------

func (a bridgeAdapter) CreateRoute(ctx context.Context, def bridge.RouteDefinition, by econ.Actor) (BridgeRouteView, error) {
	r, err := a.svc.CreateRoute(ctx, def, by)
	if err != nil {
		return BridgeRouteView{}, err
	}
	return a.Route(ctx, r.ID)
}

func (a bridgeAdapter) SetRouteStatus(ctx context.Context, rid bridge.RouteID, to bridge.RouteStatus, by econ.Actor, reason string) (BridgeRouteView, error) {
	if err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := a.svc.TransitionRoute(ctx, tx, rid, to, by, reason)
		return err
	}); err != nil {
		return BridgeRouteView{}, err
	}
	return a.Route(ctx, rid)
}

func (a bridgeAdapter) ReconcileRoute(ctx context.Context, rid bridge.RouteID, by econ.Actor) (bridge.RouteReconciliation, error) {
	var out bridge.RouteReconciliation
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.ReconcileRoute(ctx, tx, rid, reconciliation.ModeFull, by)
		return err
	})
	return out, err
}

func (a bridgeAdapter) ReconcileLeg(ctx context.Context, lid bridge.LegID) (BridgeLegOutcome, error) {
	res, err := a.svc.Advance(ctx, lid)
	if err != nil {
		return BridgeLegOutcome{}, err
	}
	v, err := a.legView(ctx, res.Leg.ID)
	if err != nil {
		return BridgeLegOutcome{}, err
	}
	return BridgeLegOutcome{View: v, Settled: v.Leg.Status.Terminal()}, nil
}

// --- a leg in manual review ------------------------------------------------------

func (a bridgeAdapter) ResumeRelay(ctx context.Context, lid bridge.LegID, by econ.Actor, reason string) (BridgeLegView, error) {
	if err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := a.svc.ResumeRelay(ctx, tx, lid, by, reason)
		return err
	}); err != nil {
		return BridgeLegView{}, err
	}
	return a.legView(ctx, lid)
}

func (a bridgeAdapter) ResolveReview(ctx context.Context, lid bridge.LegID, by econ.Actor, reason string) (BridgeLegView, error) {
	if err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := a.svc.ResolveReview(ctx, tx, lid, by, reason)
		return err
	}); err != nil {
		return BridgeLegView{}, err
	}
	return a.legView(ctx, lid)
}

func (a bridgeAdapter) Refund(ctx context.Context, lid bridge.LegID, by econ.Actor, reason string) (BridgeLegOutcome, error) {
	if _, err := a.svc.Refund(ctx, lid, by, reason); err != nil {
		return BridgeLegOutcome{}, err
	}
	// Final once the refund is (the leg ends FAILED, code REFUNDED); a refund
	// still on its way leaves the leg in review, read as it stands.
	v, err := a.legView(ctx, lid)
	if err != nil {
		return BridgeLegOutcome{}, err
	}
	return BridgeLegOutcome{View: v, Settled: v.Leg.Status.Terminal()}, nil
}

func (a bridgeAdapter) ProduceBlocks(ctx context.Context, chain devnet.ChainID, n uint64) (BridgeChainView, error) {
	if _, err := a.host.ProduceBlocks(ctx, chain, n); err != nil {
		return BridgeChainView{}, err
	}
	return a.chain(ctx, chain)
}

func (a bridgeAdapter) InjectFault(ctx context.Context, chain devnet.ChainID, f BridgeFault) (BridgeChainView, error) {
	var err error
	switch f.Kind {
	case BridgeFaultTimeoutButLand:
		err = a.host.TimeoutButLand(ctx, chain)
	case BridgeFaultTimeoutAndDrop:
		err = a.host.TimeoutAndDrop(ctx, chain)
	case BridgeFaultDropNext:
		err = a.host.DropNext(ctx, chain, f.Count)
	case BridgeFaultDelayInclusion:
		err = a.host.DelayInclusion(ctx, chain, f.Count)
	case BridgeFaultClearFaults:
		err = a.host.ClearFaults(ctx, chain)
	default:
		err = validationError("kind", "kind is not a fault the chain host knows")
	}
	if err != nil {
		return BridgeChainView{}, err
	}
	return a.chain(ctx, chain)
}

// --- views ---------------------------------------------------------------------------------

func (a bridgeAdapter) chain(ctx context.Context, id devnet.ChainID) (BridgeChainView, error) {
	cfg := a.host.Config()
	var out BridgeChainView
	err := a.host.View(ctx, func(v devnethost.View) error {
		var err error
		out, err = bridgeChainOf(v, cfg, id)
		return err
	})
	return out, err
}

func bridgeChainOf(v devnethost.View, cfg devnethost.Config, id devnet.ChainID) (BridgeChainView, error) {
	params, ok := cfg.Params(id)
	if !ok {
		return BridgeChainView{}, errs.New(errs.CodeNotFound, "the chain host does not run this chain").WithField("chain", string(id))
	}
	tip, err := v.Tip(id)
	if err != nil {
		return BridgeChainView{}, err
	}
	finalized, err := v.FinalizedHeight(id)
	if err != nil {
		return BridgeChainView{}, err
	}
	return BridgeChainView{Params: params, Tip: tip, FinalizedHeight: finalized}, nil
}

func (a bridgeAdapter) legView(ctx context.Context, lid bridge.LegID) (BridgeLegView, error) {
	var out BridgeLegView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		leg, err := a.svc.GetLeg(ctx, tx, lid)
		if err != nil {
			return err
		}
		out, err = a.newLegContext().view(ctx, tx, leg)
		return err
	})
	return out, err
}

func bridgeRouteViewOf(ctx context.Context, q db.Querier, l *stableLabels, r bridge.Route) (BridgeRouteView, error) {
	src, err := l.label(ctx, q, r.SourceAssetID)
	if err != nil {
		return BridgeRouteView{}, err
	}
	dst, err := l.label(ctx, q, r.DestinationAssetID)
	if err != nil {
		return BridgeRouteView{}, err
	}
	return BridgeRouteView{Route: r, Source: src, Destination: dst}, nil
}

// bridgeLegContext caches the routes and assets a set of legs names.
type bridgeLegContext struct {
	svc    *bridge.Service
	labels *stableLabels
	routes map[bridge.RouteID]BridgeRouteView
}

func (a bridgeAdapter) newLegContext() *bridgeLegContext {
	return &bridgeLegContext{svc: a.svc, labels: newStableLabels(a.assets), routes: map[bridge.RouteID]BridgeRouteView{}}
}

func (c *bridgeLegContext) view(ctx context.Context, q db.Querier, leg bridge.Leg) (BridgeLegView, error) {
	rv, ok := c.routes[leg.RouteID]
	if !ok {
		r, err := c.svc.GetRoute(ctx, q, leg.RouteID)
		if err != nil {
			return BridgeLegView{}, err
		}
		if rv, err = bridgeRouteViewOf(ctx, q, c.labels, r); err != nil {
			return BridgeLegView{}, err
		}
		c.routes[leg.RouteID] = rv
	}
	out := BridgeLegView{Leg: leg, Route: rv}
	var ids []string
	for _, e := range []*econ.EffectID{leg.SourceEffectID, leg.DestinationEffectID, leg.RefundEffectID} {
		if e != nil {
			ids = append(ids, e.String())
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, bridgeEffectStatesSQL, ids)
	if err != nil {
		return BridgeLegView{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var eid uuid.UUID
		var status string
		var unknownSince, resolvedAt *time.Time
		if err := rows.Scan(&eid, &status, &unknownSince, &resolvedAt); err != nil {
			return BridgeLegView{}, err
		}
		st := econ.EffectStatus(status)
		switch {
		case leg.SourceEffectID != nil && eid.String() == leg.SourceEffectID.String():
			out.Effects.Source = &st
		case leg.DestinationEffectID != nil && eid.String() == leg.DestinationEffectID.String():
			out.Effects.Destination = &st
		case leg.RefundEffectID != nil && eid.String() == leg.RefundEffectID.String():
			out.Effects.Refund = &st
		}
		if st == econ.EffectUnknown {
			out.Reconciliation.Awaiting = true
		}
		out.Reconciliation.UnknownSince = bridgeLatest(out.Reconciliation.UnknownSince, unknownSince)
		out.Reconciliation.ResolvedAt = bridgeLatest(out.Reconciliation.ResolvedAt, resolvedAt)
	}
	return out, rows.Err()
}

// bridgeLatest returns the later of two optional instants.
func bridgeLatest(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil || !b.After(*a):
		return a
	default:
		return b
	}
}
