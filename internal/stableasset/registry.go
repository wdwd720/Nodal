package stableasset

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// PlatformStableAsset is a sandbox stable asset: a SIMULATED asset on a
// simulated chain whose devnet mint the platform's IssuerSigner controls,
// issued by one STABLE_ISSUER facility under a fully reserved policy. The
// names are generic on purpose: nothing here is a public brand.
type PlatformStableAsset struct {
	ID         StableAssetID
	Name       string
	AssetID    assets.AssetID
	FacilityID econ.FacilityID
	// Network is the devnethost network the mint lives on.
	Network  string
	Chain    devnet.ChainID
	Mint     devnet.PublicKey
	Decimals uint8
	Status   Status
	// IssuanceCap bounds outstanding supply, in base units.
	IssuanceCap       money.Quantity
	MintAuthorityMode MintAuthorityMode
	MintAuthority     devnet.PublicKey
	BurnAuthorityMode BurnAuthorityMode
	RedemptionMode    RedemptionMode
	// PegQuoteAssetID is the unit of account the peg and every reserve price
	// are expressed in; Peg is the exact target price of one whole unit.
	PegQuoteAssetID assets.AssetID
	Peg             money.Price
	// OracleSource is the valuation source whose prices the monitor and the
	// reserve verification accept.
	OracleSource          string
	MaxPriceAge           time.Duration
	PegDeviationThreshold money.BPS
	// Attestable says the asset publishes signed reserve snapshots.
	Attestable bool
	// Reserves is the reserve policy: the eligible reserve assets.
	Reserves  []EligibleReserve
	CreatedBy econ.Actor
	CreatedAt time.Time
	UpdatedAt time.Time
}

// EligibleReserve is one asset of the reserve policy.
type EligibleReserve struct {
	AssetID   assets.AssetID
	Kind      ReserveKind
	FactorBPS money.BPS
	// AcceptedForMint: a mint may be paid in this asset (cash equivalents
	// only, with a positive factor).
	AcceptedForMint bool
	Decimals        uint8
}

// Reserve returns the policy entry for asset.
func (a PlatformStableAsset) Reserve(asset assets.AssetID) (EligibleReserve, bool) {
	for _, r := range a.Reserves {
		if r.AssetID == asset {
			return r, true
		}
	}
	return EligibleReserve{}, false
}

// payoutReserve is the cash equivalent a redemption pays out: the first
// mint-accepted cash equivalent of the policy.
func (a PlatformStableAsset) payoutReserve() (EligibleReserve, bool) {
	for _, r := range a.Reserves {
		if r.Kind == ReserveCashEquivalent && r.AcceptedForMint {
			return r, true
		}
	}
	return EligibleReserve{}, false
}

// account returns the issuer facility's ledger account.
func (a PlatformStableAsset) account(code ledger.Code, asset assets.AssetID) ledger.AccountRef {
	return ledger.AccountRef{OwnerType: ledger.OwnerFacility, OwnerID: a.FacilityID.String(), Code: code, AssetID: asset}
}

// ReserveSpec is one reserve asset of a Definition.
type ReserveSpec struct {
	AssetID         assets.AssetID
	Kind            ReserveKind
	FactorBPS       money.BPS
	AcceptedForMint bool
}

// sandboxDollarSymbol is the symbol of the platform's one sandbox dollar,
// which econ owns. The controller never creates an asset under it; a reserve
// in that dollar names its asset id in Definition.Reserves.
const sandboxDollarSymbol = "SBX-USD"

// Definition describes a stable asset to register.
type Definition struct {
	// Name is generic: 1..63 bytes of [a-z0-9-].
	Name   string
	Symbol string
	// PegCurrency is the display currency of the peg (e.g. "USD").
	PegCurrency           string
	Decimals              uint8
	IssuanceCap           money.Quantity
	RedemptionMode        RedemptionMode
	PegQuoteAssetID       assets.AssetID
	PegMantissa           money.Quantity
	PegScale              int32
	OracleSource          string
	MaxPriceAge           time.Duration
	PegDeviationThreshold money.BPS
	Attestable            bool
	Reserves              []ReserveSpec
}

// Validate checks the definition before anything is created.
func (d Definition) Validate() error {
	var problems []string
	if !validName(d.Name) {
		problems = append(problems, "name must be 1..63 bytes of [a-z0-9-] starting with a letter or digit")
	}
	if strings.TrimSpace(d.Symbol) == "" || strings.TrimSpace(d.PegCurrency) == "" {
		problems = append(problems, "symbol and peg currency are required")
	}
	if strings.EqualFold(strings.TrimSpace(d.Symbol), sandboxDollarSymbol) {
		problems = append(problems, "the symbol "+sandboxDollarSymbol+" belongs to the platform's one sandbox dollar")
	}
	if d.Decimals > devnet.MaxDecimals {
		problems = append(problems, "decimals must be at most 18")
	}
	if !d.IssuanceCap.IsPositive() {
		problems = append(problems, "issuance cap must be positive")
	}
	if d.RedemptionMode != RedemptionInstant && d.RedemptionMode != RedemptionQueued {
		problems = append(problems, "redemption mode must be INSTANT or QUEUED")
	}
	if d.PegQuoteAssetID.IsZero() || !d.PegMantissa.IsPositive() || d.PegScale < 0 || d.PegScale > money.MaxPriceScale {
		problems = append(problems, "the target peg needs a quote asset, a positive mantissa and a scale in [0, 38]")
	}
	if strings.TrimSpace(d.OracleSource) == "" || len(d.OracleSource) > 100 {
		problems = append(problems, "oracle source must be 1..100 bytes")
	}
	if d.MaxPriceAge <= 0 {
		problems = append(problems, "max price age must be positive")
	}
	if d.PegDeviationThreshold < 1 || d.PegDeviationThreshold > money.OneHundredPercent {
		problems = append(problems, "peg deviation threshold must be in [1, 10000] bps")
	}
	accepted := 0
	seen := map[assets.AssetID]bool{}
	for _, r := range d.Reserves {
		if r.AssetID.IsZero() || seen[r.AssetID] {
			problems = append(problems, "every reserve asset must be named once")
		}
		seen[r.AssetID] = true
		if r.Kind != ReserveCashEquivalent && r.Kind != ReserveTokenized {
			problems = append(problems, "unknown reserve kind "+string(r.Kind))
		}
		if r.FactorBPS < 0 || r.FactorBPS > money.OneHundredPercent {
			problems = append(problems, "eligibility factor must be in [0, 10000] bps")
		}
		if r.AcceptedForMint {
			if r.Kind != ReserveCashEquivalent || r.FactorBPS == 0 {
				problems = append(problems, "only a cash equivalent with a positive factor may pay for a mint")
			}
			accepted++
		}
	}
	if accepted == 0 {
		problems = append(problems, "at least one cash-equivalent reserve asset must be accepted for mint")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "stableasset: invalid definition").WithField("problems", problems)
	}
	return nil
}

func validName(s string) bool {
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

// MintAddress is the devnet mint address of the stable asset named name on a
// network: a domain-separated hash, a key nobody holds. The same definition
// always yields the same mint, so registration can be repeated safely.
func MintAddress(network, name string) devnet.PublicKey {
	return devnet.PublicKey(sha256.Sum256([]byte("nodal-sandbox/stable-asset/mint/v1\x00" + network + "\x00" + name)))
}

// Register creates the devnet mint (idempotently, through the host), then
// the SIMULATED asset, the issuer facility, the registry row and its reserve
// policy in one transaction. Registering an identical definition again
// returns the existing asset; a different definition under the same name is
// CONFLICT.
func (s *Service) Register(ctx context.Context, def Definition, actor econ.Actor) (PlatformStableAsset, error) {
	if err := def.Validate(); err != nil {
		return PlatformStableAsset{}, err
	}
	if err := requireActor(actor); err != nil {
		return PlatformStableAsset{}, err
	}
	mint := MintAddress(s.host.Config().Network, def.Name)
	if err := s.host.EnsureMint(ctx, devnethost.ChainSolana, devnet.MintConfig{
		Address: mint, Decimals: def.Decimals, Authority: s.issuer.PublicKey(),
	}); err != nil {
		return PlatformStableAsset{}, err
	}
	var out PlatformStableAsset
	err := s.db.InTx(ctx, txOptions, func(ctx context.Context, tx pgx.Tx) error {
		a, err := s.registerInTx(ctx, tx, def, mint, actor)
		out = a
		return err
	})
	return out, err
}

func (s *Service) registerInTx(ctx context.Context, tx pgx.Tx, def Definition, mint devnet.PublicKey, actor econ.Actor) (PlatformStableAsset, error) {
	if existing, found, err := s.getByName(ctx, tx, def.Name); err != nil {
		return PlatformStableAsset{}, err
	} else if found {
		if !sameDefinition(existing, def, mint) {
			return PlatformStableAsset{}, errs.New(errs.CodeConflict, "stableasset: a different stable asset is registered under this name").
				WithField("stable_asset_id", existing.ID.String())
		}
		return existing, nil
	}
	facility, err := s.facilities.EnsureFacility(ctx, tx, ledger.FacilityStableIssuer, "stable issuer "+def.Name, actor)
	if err != nil {
		return PlatformStableAsset{}, err
	}
	asset, err := s.assets.Create(ctx, tx, assets.Asset{
		Chain: string(devnethost.ChainSolana), MintAddress: mint.String(), Kind: assets.KindSimulated,
		Symbol: def.Symbol, Name: def.Symbol + " (sandbox stable asset)", Decimals: def.Decimals,
		IsStablecoin: true, PegCurrency: def.PegCurrency, ValueDomain: valuedomain.Simulated,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	if err != nil {
		return PlatformStableAsset{}, err
	}
	sid := id.New[stableAssetKind]()
	if _, err := tx.Exec(ctx, `INSERT INTO stable_assets
		(id, name, asset_id, facility_id, network, chain_id, devnet_mint, decimals, status, issuance_cap,
		 mint_authority_mode, mint_authority, burn_authority_mode, redemption_mode, peg_quote_asset_id, peg_mantissa,
		 peg_scale, oracle_source, max_price_age_ms, peg_deviation_threshold_bps, attestable,
		 created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'ACTIVE',$9::numeric,$10,$11,$12,$13,$14,$15::numeric,$16,$17,$18,$19,$20,$21,$22)`,
		sid, def.Name, asset.ID, facility.ID, s.host.Config().Network, string(devnethost.ChainSolana), mint.String(),
		int16(def.Decimals), def.IssuanceCap.String(), MintAuthorityPlatformSigner, s.issuer.PublicKey().String(),
		BurnAuthorityHolderSigned, def.RedemptionMode, def.PegQuoteAssetID, def.PegMantissa.String(), def.PegScale,
		def.OracleSource, def.MaxPriceAge.Milliseconds(), int(def.PegDeviationThreshold), def.Attestable,
		actor.Type, actor.ID); err != nil {
		return PlatformStableAsset{}, dbErr("register stable asset", err)
	}
	for _, r := range def.Reserves {
		if _, err := tx.Exec(ctx, `INSERT INTO stable_reserve_assets
			(id, stable_asset_id, reserve_asset_id, reserve_kind, eligibility_factor_bps, accepted_for_mint)
			VALUES ($1,$2,$3,$4,$5,$6)`, id.New[id.Any](), sid, r.AssetID, r.Kind, int(r.FactorBPS), r.AcceptedForMint); err != nil {
			return PlatformStableAsset{}, dbErr("register reserve asset", err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stable_asset_transitions (id, stable_asset_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,'NONE','ACTIVE',$3,$4,'registered')`, id.New[id.Any](), sid, actor.Type, actor.ID); err != nil {
		return PlatformStableAsset{}, dbErr("record registration", err)
	}
	a, err := s.get(ctx, tx, sid, false)
	if err != nil {
		return PlatformStableAsset{}, err
	}
	if err := s.auditEvent(ctx, tx, actor, "stableasset.registered", "stable_asset", sid.String(), "", "registered", a.summary()); err != nil {
		return PlatformStableAsset{}, err
	}
	return a, nil
}

func sameDefinition(a PlatformStableAsset, d Definition, mint devnet.PublicKey) bool {
	if a.Mint != mint || a.Decimals != d.Decimals || !a.IssuanceCap.Equal(d.IssuanceCap) || a.RedemptionMode != d.RedemptionMode ||
		a.PegQuoteAssetID != d.PegQuoteAssetID || !a.Peg.Mantissa.Equal(d.PegMantissa) || a.Peg.Scale != d.PegScale ||
		a.OracleSource != d.OracleSource || a.MaxPriceAge != time.Duration(d.MaxPriceAge.Milliseconds())*time.Millisecond ||
		a.PegDeviationThreshold != d.PegDeviationThreshold || a.Attestable != d.Attestable || len(a.Reserves) != len(d.Reserves) {
		return false
	}
	for _, r := range d.Reserves {
		got, ok := a.Reserve(r.AssetID)
		if !ok || got.Kind != r.Kind || got.FactorBPS != r.FactorBPS || got.AcceptedForMint != r.AcceptedForMint {
			return false
		}
	}
	return true
}

const stableColumns = `s.id, s.name, s.asset_id, s.facility_id, s.network, s.chain_id, s.devnet_mint, s.decimals, s.status,
	s.issuance_cap::text, s.mint_authority_mode, s.mint_authority, s.burn_authority_mode, s.redemption_mode,
	s.peg_quote_asset_id, s.peg_mantissa::text, s.peg_scale, s.oracle_source, s.max_price_age_ms,
	s.peg_deviation_threshold_bps, s.attestable, s.created_by_actor_type, s.created_by_actor_id, s.created_at, s.updated_at`

func scanStable(row pgx.Row) (PlatformStableAsset, error) {
	var a PlatformStableAsset
	var chain, mint, authority, cap, pegMantissa string
	var decimals int16
	var maxAge int64
	var threshold int
	if err := row.Scan(&a.ID, &a.Name, &a.AssetID, &a.FacilityID, &a.Network, &chain, &mint, &decimals, &a.Status,
		&cap, &a.MintAuthorityMode, &authority, &a.BurnAuthorityMode, &a.RedemptionMode, &a.PegQuoteAssetID,
		&pegMantissa, &a.Peg.Scale, &a.OracleSource, &maxAge, &threshold, &a.Attestable, &a.CreatedBy.Type,
		&a.CreatedBy.ID, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return PlatformStableAsset{}, err
	}
	var err error
	a.Chain = devnet.ChainID(chain)
	if a.Mint, err = devnet.ParsePublicKey(mint); err != nil {
		return PlatformStableAsset{}, errs.Wrap(err, errs.CodeInternal, "stableasset: stored mint is not a public key")
	}
	if a.MintAuthority, err = devnet.ParsePublicKey(authority); err != nil {
		return PlatformStableAsset{}, errs.Wrap(err, errs.CodeInternal, "stableasset: stored mint authority is not a public key")
	}
	if decimals < 0 || decimals > devnet.MaxDecimals {
		return PlatformStableAsset{}, errs.New(errs.CodeInternal, "stableasset: stored decimals out of range")
	}
	a.Decimals = uint8(decimals)
	if a.IssuanceCap, err = money.ScanQuantity(cap); err != nil {
		return PlatformStableAsset{}, errs.Wrap(err, errs.CodeInternal, "stableasset: stored cap")
	}
	if a.Peg.Mantissa, err = money.ScanQuantity(pegMantissa); err != nil {
		return PlatformStableAsset{}, errs.Wrap(err, errs.CodeInternal, "stableasset: stored peg")
	}
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	a.Peg.QuoteAsset, a.Peg.Source, a.Peg.At = a.PegQuoteAssetID.String(), "TARGET_PEG", a.CreatedAt
	a.MaxPriceAge = time.Duration(maxAge) * time.Millisecond
	a.PegDeviationThreshold = money.BPS(threshold)
	return a, nil
}

// Get returns a stable asset with its reserve policy.
func (s *Service) Get(ctx context.Context, q db.Querier, sid StableAssetID) (PlatformStableAsset, error) {
	return s.get(ctx, q, sid, false)
}

// get loads (and with forUpdate, row-locks) a stable asset. The row lock is
// what serialises every supply-changing step of one asset: admission,
// posting, the monitor and the breaker.
func (s *Service) get(ctx context.Context, q db.Querier, sid StableAssetID, forUpdate bool) (PlatformStableAsset, error) {
	sql := `SELECT ` + stableColumns + ` FROM stable_assets s WHERE s.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	a, err := scanStable(q.QueryRow(ctx, sql, sid))
	if errors.Is(err, pgx.ErrNoRows) {
		return PlatformStableAsset{}, errs.New(errs.CodeNotFound, "stable asset not found").WithField("stable_asset_id", sid.String())
	}
	if err != nil {
		return PlatformStableAsset{}, dbErr("load stable asset", err)
	}
	if a.Reserves, err = loadReserves(ctx, q, sid); err != nil {
		return PlatformStableAsset{}, err
	}
	return a, nil
}

func (s *Service) getByName(ctx context.Context, q db.Querier, name string) (PlatformStableAsset, bool, error) {
	var sid StableAssetID
	err := q.QueryRow(ctx, `SELECT id FROM stable_assets WHERE name = $1`, name).Scan(&sid)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlatformStableAsset{}, false, nil
	}
	if err != nil {
		return PlatformStableAsset{}, false, dbErr("find stable asset", err)
	}
	a, err := s.get(ctx, q, sid, false)
	return a, err == nil, err
}

func loadReserves(ctx context.Context, q db.Querier, sid StableAssetID) ([]EligibleReserve, error) {
	rows, err := q.Query(ctx, `SELECT r.reserve_asset_id, r.reserve_kind, r.eligibility_factor_bps, r.accepted_for_mint, a.decimals
		FROM stable_reserve_assets r JOIN assets a ON a.id = r.reserve_asset_id
		WHERE r.stable_asset_id = $1 ORDER BY r.created_at, r.reserve_asset_id`, sid)
	if err != nil {
		return nil, dbErr("load reserve policy", err)
	}
	defer rows.Close()
	var out []EligibleReserve
	for rows.Next() {
		var r EligibleReserve
		var factor int
		var decimals int16
		if err := rows.Scan(&r.AssetID, &r.Kind, &factor, &r.AcceptedForMint, &decimals); err != nil {
			return nil, dbErr("scan reserve policy", err)
		}
		r.FactorBPS = money.BPS(factor)
		r.Decimals = uint8(decimals) // #nosec G115 -- assets.decimals is CHECKed to [0, 18]
		out = append(out, r)
	}
	return out, rows.Err()
}

// Transition moves a stable asset to a new status, writing the transition
// row, the update and the audit event in the caller's transaction. Resuming a
// paused asset is an operator's decision: the circuit breaker pauses, it
// never resumes.
func (s *Service) Transition(ctx context.Context, tx pgx.Tx, sid StableAssetID, to Status, actor econ.Actor, reason string) (PlatformStableAsset, error) {
	if err := requireActor(actor); err != nil {
		return PlatformStableAsset{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return PlatformStableAsset{}, errs.New(errs.CodeValidationFailed, "stableasset: a status change needs a reason")
	}
	a, err := s.get(ctx, tx, sid, true)
	if err != nil {
		return PlatformStableAsset{}, err
	}
	return s.transitionLocked(ctx, tx, a, to, actor, reason)
}

func (s *Service) transitionLocked(ctx context.Context, tx pgx.Tx, a PlatformStableAsset, to Status, actor econ.Actor, reason string) (PlatformStableAsset, error) {
	if a.Status == to {
		return a, nil
	}
	if !CanTransition(a.Status, to) {
		return PlatformStableAsset{}, errs.Newf(errs.CodeInvalidStateTransition, "stable asset %s cannot move %s -> %s", a.ID, a.Status, to).
			WithField("stable_asset_id", a.ID.String())
	}
	if a.Status == StatusPaused && to == StatusActive && actor.Type != security.ActorOperator {
		return PlatformStableAsset{}, errs.New(errs.CodeForbidden, "stableasset: only an operator resumes a paused stable asset")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stable_asset_transitions (id, stable_asset_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, id.New[id.Any](), a.ID, a.Status, to, actor.Type, actor.ID, reason); err != nil {
		return PlatformStableAsset{}, dbErr("record stable asset transition", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE stable_assets SET status = $2 WHERE id = $1`, a.ID, to); err != nil {
		return PlatformStableAsset{}, dbErr("update stable asset status", err)
	}
	if err := s.auditEvent(ctx, tx, actor, "stableasset."+strings.ToLower(string(to)), "stable_asset", a.ID.String(), "", reason,
		map[string]string{"from": string(a.Status), "to": string(to)}); err != nil {
		return PlatformStableAsset{}, err
	}
	a.Status = to
	return a, nil
}

// summary is the registry row as an audit payload.
func (a PlatformStableAsset) summary() map[string]any {
	return map[string]any{
		"stable_asset_id": a.ID.String(), "asset_id": a.AssetID.String(), "facility_id": a.FacilityID.String(),
		"chain": string(a.Chain), "mint": a.Mint.String(), "issuance_cap": a.IssuanceCap.String(),
		"peg":             a.Peg.Mantissa.String() + "e-" + money.QuantityFromInt64(int64(a.Peg.Scale)).String(),
		"redemption_mode": string(a.RedemptionMode),
	}
}

// auditEvent appends an audit record in the caller's transaction.
func (s *Service) auditEvent(ctx context.Context, tx pgx.Tx, actor econ.Actor, action, resourceType, resourceID, account, reason string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "stableasset: encode audit payload")
	}
	stream := audit.SystemStream
	if account != "" {
		stream = audit.AccountStream(account)
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream: stream, ActorType: string(actor.Type), ActorID: actor.ID, Action: action,
		ResourceType: resourceType, ResourceID: resourceID, Reason: reason, Payload: body, OccurredAt: s.clk.Now().UTC(),
	})
	return err
}

// requireActor refuses a missing actor and every agent: an agent may propose
// an intent elsewhere, but it never drives issuance.
func requireActor(a econ.Actor) error {
	if !a.Type.Valid() || strings.TrimSpace(a.ID) == "" {
		return errs.New(errs.CodeValidationFailed, "stableasset: an actor type and id are required")
	}
	if a.Type == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "stableasset: an agent never mints, burns or administers a stable asset")
	}
	return nil
}

func dbErr(op string, err error) error {
	if db.IsRetryable(err) {
		return err
	}
	if e, ok := errs.As(err); ok {
		return e
	}
	if mapped := ledger.MapError(err); mapped != nil {
		if e, ok := errs.As(mapped); ok && e.Code != errs.CodeInternal {
			return e
		}
	}
	return errs.Wrap(err, errs.CodeInternal, "stableasset: "+op)
}
