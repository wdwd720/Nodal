package nativeasset

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Service creates and governs native assets.
type Service struct {
	clk      clock.Clock
	assets   *assets.Repository
	screener Screener
}

// NewService returns a Service. The screener is optional; without one, only
// the local deterministic rules apply, which is a weaker floor and not an
// absent one.
func NewService(clk clock.Clock, screener Screener) *Service {
	if clk == nil {
		panic("nativeasset: NewService requires a clock")
	}
	return &Service{clk: clk, assets: assets.NewRepository(), screener: screener}
}

// CreateRequest is a user's proposal for a new asset.
type CreateRequest struct {
	CreatorAccountID accounts.AccountID
	Name             string
	Symbol           string
	Description      string
	ImageURL         string
	Metadata         map[string]any
	Supply           SupplyModel
	// Decimals is the asset's scale. It is fixed at creation and cannot be
	// changed, because every quantity ever recorded is in these units.
	Decimals uint8
	// Policy defaults to ConservativePolicy when zero-valued.
	Policy *PolicyProfile
}

// DefaultDecimals is the scale every native asset uses unless a creator asks
// for another. Six places matches the Credit and is fine enough that a
// freshly launched market can price a unit far below one Credit without
// rounding to nothing.
const DefaultDecimals uint8 = 6

// MaxDecimals bounds the scale. The registry column is a smallint capped at
// 18 and the ledger stores NUMERIC(38,0); 18 places on a supply of 10^12
// would be 10^30 base units, which still fits, and more would not.
const MaxDecimals uint8 = 18

// Validate checks the request without touching the database.
func (r CreateRequest) Validate() error {
	if r.CreatorAccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a native asset needs a creator")
	}
	if r.Decimals > MaxDecimals {
		return errs.Newf(errs.CodeValidationFailed, "decimals must be at most %d", MaxDecimals)
	}
	if err := r.Supply.Validate(); err != nil {
		return err
	}
	p := ConservativePolicy()
	if r.Policy != nil {
		p = *r.Policy
	}
	return p.Validate()
}

// CreateDraft screens the proposal and, if it passes, registers the asset in
// DRAFT.
//
// The registry row is created with status RESTRICTED, which already means "may
// not increase exposure". A draft therefore cannot be traded even by a code
// path that never heard of this package.
func (s *Service) CreateDraft(ctx context.Context, tx pgx.Tx, r CreateRequest) (Asset, Verdict, error) {
	if err := r.Validate(); err != nil {
		return Asset{}, Verdict{}, err
	}
	if tx == nil {
		return Asset{}, Verdict{}, errs.New(errs.CodeInternal, "nativeasset: CreateDraft requires a transaction")
	}
	names, symbols, err := s.existingNamesAndSymbols(ctx, tx)
	if err != nil {
		return Asset{}, Verdict{}, err
	}
	verdict := Screen(ScreenInput{
		Name: r.Name, Symbol: r.Symbol, Description: r.Description,
		ImageURL: r.ImageURL, Metadata: r.Metadata,
		ExistingNames: names, ExistingSymbols: symbols,
	})
	if s.screener != nil {
		state, note, serr := s.screener.Screen(r.Name, r.Symbol, r.Description)
		if serr != nil {
			// An external screener that is down must not become a way to get
			// unscreened content in, nor a way to stop the product working.
			// Flagging for review is the honest middle.
			verdict = Combine(verdict, ModerationFlagged,
				"external screening unavailable: "+serr.Error())
		} else {
			verdict = Combine(verdict, state, note)
		}
	}
	if verdict.Blocked() {
		return Asset{}, verdict, ErrRejected(verdict)
	}

	decimals := r.Decimals
	if decimals == 0 {
		decimals = DefaultDecimals
	}
	symbol := strings.ToUpper(strings.TrimSpace(r.Symbol))
	name := strings.TrimSpace(r.Name)

	registry, err := s.assets.Create(ctx, tx, assets.Asset{
		Chain:       assets.InternalChain,
		Kind:        assets.KindNativeAsset,
		ValueDomain: valuedomain.InternalNativeAsset,
		Symbol:      symbol,
		Name:        name,
		Decimals:    decimals,
		RiskClass:   assets.RiskSpeculative,
		// RESTRICTED blocks opening exposure while permitting reduction, which
		// is exactly a draft's correct posture.
		Status: assets.StatusRestricted,
	})
	if err != nil {
		return Asset{}, verdict, err
	}

	policy := ConservativePolicy()
	if r.Policy != nil {
		policy = *r.Policy
	}
	meta, err := json.Marshal(orEmpty(r.Metadata))
	if err != nil {
		return Asset{}, verdict, errs.Wrap(err, errs.CodeValidationFailed, "asset metadata is not JSON-encodable")
	}
	var imageURL any
	if r.ImageURL != "" {
		imageURL = r.ImageURL
	}

	a := Asset{
		AssetID: registry.ID, CreatorAccountID: r.CreatorAccountID,
		Name: name, Symbol: symbol, Description: r.Description, ImageURL: r.ImageURL,
		Metadata: r.Metadata, Status: StatusDraft, Supply: r.Supply, Policy: policy,
		Moderation: verdict.State, ModerationNotes: verdict.Explain(),
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO native_assets
		   (asset_id, creator_account_id, name, symbol, description, image_url, metadata, status,
		    max_supply, creator_allocation, treasury_allocation,
		    internal_only, transferable, cashout_eligible, creator_earning_eligible,
		    market_proceeds_eligible, minimum_age, jurisdiction_policy, marketing_restrictions,
		    content_moderation_state, moderation_notes)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,'DRAFT',$8::numeric,$9::numeric,$10::numeric,
		         $11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		 RETURNING created_at, updated_at`,
		a.AssetID, a.CreatorAccountID, a.Name, a.Symbol, a.Description, imageURL, meta,
		r.Supply.MaxSupply.String(), r.Supply.CreatorAllocation.String(), r.Supply.TreasuryAllocation.String(),
		policy.InternalOnly, policy.Transferable, policy.CashoutEligible, policy.CreatorEarningEligible,
		policy.MarketProceedsEligible, policy.MinimumAge, policy.JurisdictionPolicy, policy.MarketingRestrictions,
		string(verdict.State), a.ModerationNotes).Scan(&a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Asset{}, verdict, errs.New(errs.CodeConflict,
				"an asset with this symbol already exists").WithField("symbol", symbol)
		}
		return Asset{}, verdict, mapError(err)
	}
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	return a, verdict, nil
}

// SetStatus moves an asset through its lifecycle, writing the transition row
// the 00603 binding requires in the same statement pair.
//
// Activation is not done here: it has side effects (freezing economics,
// opening the registry status) and deserves its own name.
func (s *Service) SetStatus(ctx context.Context, tx pgx.Tx, assetID assets.AssetID, to Status, reason string) (Asset, error) {
	if strings.TrimSpace(reason) == "" {
		return Asset{}, errs.New(errs.CodeValidationFailed, "a status change requires a reason")
	}
	if !to.Valid() {
		return Asset{}, errs.Newf(errs.CodeValidationFailed, "unknown native asset status %q", to)
	}
	if to == StatusActive {
		return Asset{}, errs.New(errs.CodeValidationFailed,
			"use Activate to make an asset live; it freezes economics and opens the registry entry")
	}
	a, err := s.getForUpdate(ctx, tx, assetID)
	if err != nil {
		return Asset{}, err
	}
	if a.Status == to {
		return a, nil
	}
	if !CanTransition(a.Status, to) {
		return Asset{}, errs.Newf(errs.CodeInvalidStateTransition,
			"native asset cannot go %s -> %s", a.Status, to).
			WithField("asset_id", assetID.String())
	}
	if err := s.writeTransition(ctx, tx, assetID, a.Status, to, reason); err != nil {
		return Asset{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE native_assets SET status = $2 WHERE asset_id = $1`, assetID, string(to)); err != nil {
		return Asset{}, mapError(err)
	}
	// Keep the shared registry status in step, so a halt is visible to every
	// package that consults the registry rather than only to this one.
	if err := s.syncRegistryStatus(ctx, tx, assetID, to); err != nil {
		return Asset{}, err
	}
	a.Status = to
	return a, nil
}

// Activate makes an asset live and freezes its economics for good.
//
// After this returns, the supply, allocations, symbol and policy profile
// cannot be changed by anyone: the database refuses it (SQLSTATE NM003). That
// is PART XIII's requirement, and it is the reason activation is a separate,
// deliberate call rather than one value of a status setter.
func (s *Service) Activate(ctx context.Context, tx pgx.Tx, assetID assets.AssetID, reason string) (Asset, error) {
	if strings.TrimSpace(reason) == "" {
		return Asset{}, errs.New(errs.CodeValidationFailed, "activation requires a reason")
	}
	a, err := s.getForUpdate(ctx, tx, assetID)
	if err != nil {
		return Asset{}, err
	}
	if a.Status == StatusActive {
		return a, nil
	}
	if !CanTransition(a.Status, StatusActive) {
		return Asset{}, errs.Newf(errs.CodeInvalidStateTransition,
			"native asset cannot go %s -> ACTIVE", a.Status).WithField("asset_id", assetID.String())
	}
	if !a.Moderation.PermitsActivation() {
		return Asset{}, errs.Newf(errs.CodeForbidden,
			"an asset whose moderation state is %s cannot go live", a.Moderation).
			WithField("asset_id", assetID.String()).
			WithField("moderation_state", string(a.Moderation))
	}
	if err := s.writeTransition(ctx, tx, assetID, a.Status, StatusActive, reason); err != nil {
		return Asset{}, err
	}
	now := s.clk.Now().UTC()
	// economics_locked_at is set in the same statement as the status, so there
	// is no instant in which the asset is live and still editable. It is only
	// set the first time: re-activating after a halt must not move the lock.
	if _, err := tx.Exec(ctx,
		`UPDATE native_assets
		    SET status = 'ACTIVE',
		        activated_at = coalesce(activated_at, $2),
		        economics_locked_at = coalesce(economics_locked_at, $2)
		  WHERE asset_id = $1`, assetID, now); err != nil {
		return Asset{}, mapError(err)
	}
	if err := s.syncRegistryStatus(ctx, tx, assetID, StatusActive); err != nil {
		return Asset{}, err
	}
	a.Status = StatusActive
	a.ActivatedAt, a.EconomicsLockedAt = &now, &now
	return a, nil
}

// registryStatusFor maps a native status to the shared registry status. The
// registry is what internal/settlement, internal/risk and the ledger consult,
// so the two must not drift.
func registryStatusFor(s Status) assets.Status {
	switch s {
	case StatusActive:
		return assets.StatusActive
	case StatusCloseOnly:
		return assets.StatusCloseOnly
	case StatusHalted:
		return assets.StatusHalted
	case StatusDelisted:
		return assets.StatusDelisted
	default:
		// DRAFT, PENDING_REVIEW and REJECTED all mean "not tradable, but a
		// holder of nothing loses nothing", which is RESTRICTED.
		return assets.StatusRestricted
	}
}

func (s *Service) syncRegistryStatus(ctx context.Context, tx pgx.Tx, assetID assets.AssetID, to Status) error {
	want := registryStatusFor(to)
	current, err := s.assets.Get(ctx, tx, assetID)
	if err != nil {
		return err
	}
	if current.Status == want {
		return nil
	}
	if !assets.CanTransition(current.Status, want) {
		return errs.Newf(errs.CodeInvalidStateTransition,
			"native asset status %s implies registry status %s, which cannot follow %s",
			to, want, current.Status).WithField("asset_id", assetID.String())
	}
	_, err = s.assets.Transition(ctx, tx, assetID, assets.StatusChange{
		To:        want,
		ActorType: actorTypeFrom(ctx),
		ActorID:   actorIDFrom(ctx),
		Reason:    "native asset status changed to " + string(to),
	})
	return err
}

func (s *Service) writeTransition(ctx context.Context, tx pgx.Tx, assetID assets.AssetID, from, to Status, reason string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO native_asset_transitions (id, asset_id, from_status, to_status, actor_type, actor_id, reason)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		NewTransitionID(), assetID, string(from), string(to), actorTypeFrom(ctx), actorIDFrom(ctx), reason)
	return mapError(err)
}

func actorTypeFrom(ctx context.Context) string {
	if p, ok := security.PrincipalFrom(ctx); ok {
		return string(p.ActorType)
	}
	return "SYSTEM"
}

func actorIDFrom(ctx context.Context) string {
	if p, ok := security.PrincipalFrom(ctx); ok && p.SubjectID != "" {
		return p.SubjectID
	}
	return "nativeasset-service"
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	switch db.SQLState(err) {
	case "NM003":
		return errs.Wrap(err, errs.CodeForbidden,
			"this asset's economics were frozen when it went live and cannot be changed")
	case "AU001":
		return errs.Wrap(err, errs.CodeInternal,
			"a native asset status changed without its transition row")
	}
	var pgErr error = err
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.New(errs.CodeNotFound, "native asset not found")
	}
	return errs.Wrap(pgErr, errs.CodeInternal, "nativeasset: database error")
}
