package bridge

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Route joins a SIMULATED source asset on one simulated chain to its twin on
// another through one BRIDGE facility. The same paper dollar on two chains is
// two assets; the destination asset's devnet mint can only be minted by a
// BRIDGE_RELEASE (its mint authority is an address nobody holds).
type Route struct {
	ID                 RouteID
	Name               string
	FacilityID         econ.FacilityID
	Network            string
	SourceChain        devnet.ChainID
	SourceAssetID      assets.AssetID
	SourceMint         devnet.PublicKey
	DestinationChain   devnet.ChainID
	DestinationAssetID assets.AssetID
	DestinationMint    devnet.PublicKey
	Decimals           uint8
	Attester           devnet.PublicKey
	// UnitPrice is the route's value of one whole unit in USD, used for the
	// risk notional and for buying-power marks (a paper dollar is 1.00).
	UnitPrice          money.Price
	MaxReleaseAttempts int
	Status             RouteStatus
	CreatedAt          time.Time
}

func (r Route) account(code ledger.Code, asset assets.AssetID) ledger.AccountRef {
	return ledger.AccountRef{OwnerType: ledger.OwnerFacility, OwnerID: r.FacilityID.String(), Code: code, AssetID: asset}
}

// RouteDefinition describes a route to create.
type RouteDefinition struct {
	// Name is generic: 1..63 bytes of [a-z0-9-].
	Name string
	// SourceAssetID is a SIMULATED asset on a simulated chain whose
	// mint_address is a devnet mint that exists.
	SourceAssetID     assets.AssetID
	DestinationChain  devnet.ChainID
	DestinationSymbol string
	UnitPrice         money.Price
	// MaxReleaseAttempts bounds the relays of one leg before manual review.
	MaxReleaseAttempts int
}

// sandboxDollarSymbol is the symbol of the platform's one sandbox dollar,
// which econ owns. No bridged twin is ever created under it.
const sandboxDollarSymbol = "SBX-USD"

// Validate checks the definition before anything is created.
func (d RouteDefinition) Validate() error {
	var problems []string
	if !validRouteName(d.Name) {
		problems = append(problems, "name must be 1..63 bytes of [a-z0-9-] starting with a letter or digit")
	}
	sym := strings.TrimSpace(d.DestinationSymbol)
	switch {
	case sym == "" || sym != d.DestinationSymbol || len(sym) > 32:
		problems = append(problems, "destination symbol must be 1..32 bytes without surrounding whitespace")
	case strings.EqualFold(sym, sandboxDollarSymbol):
		problems = append(problems, "the symbol "+sandboxDollarSymbol+" belongs to the platform's one sandbox dollar")
	}
	if d.SourceAssetID.IsZero() {
		problems = append(problems, "a source asset is required")
	}
	if d.UnitPrice.Validate() != nil || !d.UnitPrice.Mantissa.IsPositive() {
		problems = append(problems, "the unit price must be a valid positive price")
	}
	if d.MaxReleaseAttempts < 1 || d.MaxReleaseAttempts > 20 {
		problems = append(problems, "max release attempts must be in [1, 20]")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "bridge: invalid route definition").WithField("problems", problems)
	}
	return nil
}

// DestinationMintAddress is the devnet mint of a route's destination asset:
// a domain-separated hash nobody holds a key for.
func DestinationMintAddress(network, name string) devnet.PublicKey {
	return devnet.PublicKey(sha256.Sum256([]byte("nodal-sandbox/bridge/mint/v1\x00" + network + "\x00" + name)))
}

// unspendableAuthority is the destination mint's authority: an address no
// key controls, so TOKEN_MINT_TO can never mint the bridged asset. Only a
// BRIDGE_RELEASE, signed by the attester, mints it.
func unspendableAuthority(network, name string) devnet.PublicKey {
	return devnet.PublicKey(sha256.Sum256([]byte("nodal-sandbox/bridge/mint-authority/v1\x00" + network + "\x00" + name)))
}

func validRouteName(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !alnum && (c != '-' || i == 0) {
			return false
		}
	}
	return true
}

// CreateRoute creates the destination mint (idempotently, through the host),
// then the destination SIMULATED asset, the BRIDGE facility and the route.
// Creating an identical route again returns it.
func (s *Service) CreateRoute(ctx context.Context, def RouteDefinition, actor econ.Actor) (Route, error) {
	if err := requireActor(actor); err != nil {
		return Route{}, err
	}
	if err := def.Validate(); err != nil {
		return Route{}, err
	}
	src, err := s.assets.Get(ctx, s.db, def.SourceAssetID)
	if err != nil {
		return Route{}, err
	}
	if src.Kind != assets.KindSimulated || !strings.HasPrefix(src.Chain, assets.SimulatedChainPrefix) {
		return Route{}, errs.New(errs.CodeValidationFailed, "bridge: the source must be a SIMULATED asset on a simulated chain")
	}
	srcMint, err := devnet.ParsePublicKey(src.MintAddress)
	if err != nil {
		return Route{}, errs.Wrap(err, errs.CodeValidationFailed, "bridge: the source asset's mint is not a devnet address")
	}
	srcChain := devnet.ChainID(src.Chain)
	if srcChain == def.DestinationChain {
		return Route{}, errs.New(errs.CodeValidationFailed, "bridge: a route joins two chains")
	}
	if _, err := s.host.Mint(ctx, srcChain, srcMint); err != nil {
		return Route{}, err
	}
	network := s.host.Config().Network
	dstMint := DestinationMintAddress(network, def.Name)
	if err := s.host.EnsureMint(ctx, def.DestinationChain, devnet.MintConfig{
		Address: dstMint, Decimals: src.Decimals, Authority: unspendableAuthority(network, def.Name),
	}); err != nil {
		return Route{}, err
	}
	var out Route
	err = s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		if existing, found, err := s.routeByName(ctx, tx, def.Name); err != nil {
			return err
		} else if found {
			if existing.SourceAssetID != def.SourceAssetID || existing.DestinationChain != def.DestinationChain {
				return errs.New(errs.CodeConflict, "bridge: a different route exists under this name")
			}
			out = existing
			return nil
		}
		facility, err := s.facilities.EnsureFacility(ctx, tx, ledger.FacilityBridge, "bridge "+def.Name, actor)
		if err != nil {
			return err
		}
		dst, err := s.assets.Create(ctx, tx, assets.Asset{
			Chain: string(def.DestinationChain), MintAddress: dstMint.String(), Kind: assets.KindSimulated,
			Symbol: def.DestinationSymbol, Name: def.DestinationSymbol + " (bridged sandbox asset)", Decimals: src.Decimals,
			IsStablecoin: src.IsStablecoin, PegCurrency: src.PegCurrency, ValueDomain: valuedomain.Simulated,
			RiskClass: src.RiskClass, Status: assets.StatusActive,
		})
		if err != nil {
			return err
		}
		rid := id.New[routeKind]()
		if _, err := tx.Exec(ctx, `INSERT INTO bridge_routes (id, name, facility_id, network, source_chain, source_asset_id,
			source_mint, destination_chain, destination_asset_id, destination_mint, decimals, attester, unit_price_mantissa,
			unit_price_scale, max_release_attempts, status, created_by_actor_type, created_by_actor_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::numeric,$14,$15,'ACTIVE',$16,$17)`,
			rid, def.Name, facility.ID, network, string(srcChain), src.ID, srcMint.String(), string(def.DestinationChain), dst.ID,
			dstMint.String(), int16(src.Decimals), s.attester.PublicKey().String(), def.UnitPrice.Mantissa.String(), def.UnitPrice.Scale,
			def.MaxReleaseAttempts, actor.Type, actor.ID); err != nil {
			return dbErr("create route", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO bridge_route_transitions (id, route_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'NONE','ACTIVE',$3,$4,'created')`, id.New[id.Any](), rid, actor.Type, actor.ID); err != nil {
			return dbErr("record route creation", err)
		}
		out, err = s.getRoute(ctx, tx, rid, false)
		if err != nil {
			return err
		}
		return s.auditEvent(ctx, tx, actor, "bridge.route.created", "bridge_route", rid.String(), "", "created",
			map[string]string{"source_asset_id": src.ID.String(), "destination_asset_id": dst.ID.String()})
	})
	return out, err
}

const routeColumns = `id, name, facility_id, network, source_chain, source_asset_id, source_mint, destination_chain,
	destination_asset_id, destination_mint, decimals, attester, unit_price_mantissa::text, unit_price_scale,
	max_release_attempts, status, created_at`

func scanRoute(row pgx.Row) (Route, error) {
	var r Route
	var srcChain, srcMint, dstChain, dstMint, attester, mantissa string
	var decimals int16
	if err := row.Scan(&r.ID, &r.Name, &r.FacilityID, &r.Network, &srcChain, &r.SourceAssetID, &srcMint, &dstChain,
		&r.DestinationAssetID, &dstMint, &decimals, &attester, &mantissa, &r.UnitPrice.Scale, &r.MaxReleaseAttempts,
		&r.Status, &r.CreatedAt); err != nil {
		return Route{}, err
	}
	var err error
	r.SourceChain, r.DestinationChain = devnet.ChainID(srcChain), devnet.ChainID(dstChain)
	if r.SourceMint, err = devnet.ParsePublicKey(srcMint); err != nil {
		return Route{}, err
	}
	if r.DestinationMint, err = devnet.ParsePublicKey(dstMint); err != nil {
		return Route{}, err
	}
	if r.Attester, err = devnet.ParsePublicKey(attester); err != nil {
		return Route{}, err
	}
	if r.UnitPrice.Mantissa, err = money.ScanQuantity(mantissa); err != nil {
		return Route{}, err
	}
	r.Decimals = uint8(decimals) // #nosec G115 -- CHECKed to [0, 18]
	r.CreatedAt = r.CreatedAt.UTC()
	r.UnitPrice.QuoteAsset, r.UnitPrice.Source, r.UnitPrice.At = "USD", "ROUTE_UNIT_PRICE", r.CreatedAt
	return r, nil
}

// GetRoute returns a route.
func (s *Service) GetRoute(ctx context.Context, q db.Querier, rid RouteID) (Route, error) {
	return s.getRoute(ctx, q, rid, false)
}

func (s *Service) getRoute(ctx context.Context, q db.Querier, rid RouteID, forUpdate bool) (Route, error) {
	sql := `SELECT ` + routeColumns + ` FROM bridge_routes WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	r, err := scanRoute(q.QueryRow(ctx, sql, rid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Route{}, errs.New(errs.CodeNotFound, "bridge route not found").WithField("route_id", rid.String())
	}
	if err != nil {
		return Route{}, dbErr("load route", err)
	}
	return r, nil
}

// lockRoute takes the route's row lock in a transaction that already holds
// one of its legs. Every transaction that changes what the route
// reconciliation reads -- an admission, a posting, a recorded release
// attempt -- holds this lock, so ReconcileRoute (which holds it too) reads
// the route's books, legs and attempts as of one instant, before it reads the
// chains. Lock order: a leg, then its route; a transaction that holds a
// route never waits for an existing leg.
//
// Every caller acts on the route's chains, so a route of another network is
// refused here (onHost).
func (s *Service) lockRoute(ctx context.Context, tx pgx.Tx, rid RouteID) (Route, error) {
	r, err := s.getRoute(ctx, tx, rid, true)
	if err != nil {
		return Route{}, err
	}
	if err := s.onHost(r); err != nil {
		return Route{}, err
	}
	return r, nil
}

func (s *Service) routeByName(ctx context.Context, q db.Querier, name string) (Route, bool, error) {
	r, err := scanRoute(q.QueryRow(ctx, `SELECT `+routeColumns+` FROM bridge_routes WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Route{}, false, nil
	}
	if err != nil {
		return Route{}, false, dbErr("find route", err)
	}
	return r, true, nil
}

// TransitionRoute moves a route's status with its bound transition row. A
// paused route takes no new legs; resuming is an operator's decision.
func (s *Service) TransitionRoute(ctx context.Context, tx pgx.Tx, rid RouteID, to RouteStatus, actor econ.Actor, reason string) (Route, error) {
	if err := requireActor(actor); err != nil {
		return Route{}, err
	}
	r, err := s.getRoute(ctx, tx, rid, true)
	if err != nil {
		return Route{}, err
	}
	return s.transitionRouteLocked(ctx, tx, r, to, actor, reason)
}

func (s *Service) transitionRouteLocked(ctx context.Context, tx pgx.Tx, r Route, to RouteStatus, actor econ.Actor, reason string) (Route, error) {
	if r.Status == to {
		return r, nil
	}
	if strings.TrimSpace(reason) == "" {
		return Route{}, errs.New(errs.CodeValidationFailed, "bridge: a status change needs a reason")
	}
	if !CanTransitionRoute(r.Status, to) {
		return Route{}, errs.Newf(errs.CodeInvalidStateTransition, "bridge route %s cannot move %s -> %s", r.ID, r.Status, to)
	}
	if r.Status == RoutePaused && to == RouteActive && actor.Type != security.ActorOperator {
		return Route{}, errs.New(errs.CodeForbidden, "bridge: only an operator resumes a paused route")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bridge_route_transitions (id, route_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, id.New[id.Any](), r.ID, r.Status, to, actor.Type, actor.ID, reason); err != nil {
		return Route{}, dbErr("record route transition", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE bridge_routes SET status = $2 WHERE id = $1`, r.ID, to); err != nil {
		return Route{}, dbErr("update route status", err)
	}
	if err := s.auditEvent(ctx, tx, actor, "bridge.route."+strings.ToLower(string(to)), "bridge_route", r.ID.String(), "", reason,
		map[string]string{"from": string(r.Status), "to": string(to)}); err != nil {
		return Route{}, err
	}
	r.Status = to
	return r, nil
}
