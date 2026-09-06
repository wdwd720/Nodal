package instruments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

type (
	instrumentKind struct{}
	venueKind      struct{}
	exposureKind   struct{}
	listingKind    struct{}
)

// Typed identifiers.
type (
	InstrumentID = id.ID[instrumentKind]
	VenueID      = id.ID[venueKind]
	ExposureID   = id.ID[exposureKind]
	ListingID    = id.ID[listingKind]
)

// NewInstrumentID returns a fresh instrument id.
func NewInstrumentID() InstrumentID { return id.New[instrumentKind]() }

// ParseInstrumentID parses the canonical form.
func ParseInstrumentID(s string) (InstrumentID, error) { return id.Parse[instrumentKind](s) }

// InstrumentType is the instrument category.
type InstrumentType string

// Instrument types. EVENT_OUTCOME is representable but never activatable in V1.
const (
	TypeSpotPair     InstrumentType = "SPOT_PAIR"
	TypeEventOutcome InstrumentType = "EVENT_OUTCOME"
)

// Status mirrors assets.Status semantics for instruments (PART 33).
type Status = assets.Status

// VenueStatus / ListingStatus share one vocabulary.
type VenueStatus string

// Venue and listing statuses.
const (
	VenueActive   VenueStatus = "ACTIVE"
	VenueDegraded VenueStatus = "DEGRADED"
	VenueDisabled VenueStatus = "DISABLED"
)

// Valid reports whether the status is declared.
func (s VenueStatus) Valid() bool {
	return s == VenueActive || s == VenueDegraded || s == VenueDisabled
}

// AllowsNewActions reports whether a venue/listing may be selected for new execution.
func (s VenueStatus) AllowsNewActions() bool { return s == VenueActive || s == VenueDegraded }

// VenueKind categorizes venues.
type VenueKind string

// Venue kinds.
const (
	VenueDEXAggregator VenueKind = "DEX_AGGREGATOR"
	VenueDEX           VenueKind = "DEX"
	VenueCEX           VenueKind = "CEX"
	VenueOTC           VenueKind = "OTC"
)

// Venue is an execution venue (e.g. the Jupiter aggregator).
type Venue struct {
	ID              VenueID
	Code            string
	Name            string
	Kind            VenueKind
	Chain           string
	Status          VenueStatus
	SettlementRules json.RawMessage
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ExposureKind distinguishes price exposure from event outcomes.
type ExposureKind string

// Exposure kinds.
const (
	ExposureAssetPrice   ExposureKind = "ASSET_PRICE"
	ExposureEventOutcome ExposureKind = "EVENT_OUTCOME"
)

// EconomicExposure is what an instrument economically represents.
type EconomicExposure struct {
	ID                ExposureID
	Kind              ExposureKind
	Description       string
	UnderlyingAssetID *assets.AssetID
	EventTerms        json.RawMessage
	CreatedAt         time.Time
}

// Instrument is the tradable definition.
type Instrument struct {
	ID                InstrumentID
	Type              InstrumentType
	CanonicalName     string
	ExposureID        ExposureID
	BaseAssetID       *assets.AssetID
	QuoteAssetID      *assets.AssetID
	SettlementAssetID assets.AssetID
	RiskClass         assets.RiskClass
	Status            Status
	ActiveFrom        time.Time
	ActiveUntil       *time.Time
	PolicyRef         string
	MetadataVersion   int32
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// IsActiveAt reports whether the instrument's validity window contains t.
func (i Instrument) IsActiveAt(t time.Time) bool {
	if t.Before(i.ActiveFrom) {
		return false
	}
	return i.ActiveUntil == nil || t.Before(*i.ActiveUntil)
}

// VenueListing is an instrument as exposed by one venue.
type VenueListing struct {
	ID               ListingID
	VenueID          VenueID
	InstrumentID     InstrumentID
	VenueNativeID    string
	Network          string
	BaseMint         string
	QuoteMint        string
	TickSize         *money.Quantity
	BasePrecision    uint8
	QuotePrecision   uint8
	MinNotionalQuote money.Quantity
	MaxNotionalQuote *money.Quantity
	Status           VenueStatus
	SettlementRules  json.RawMessage
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Repository persists the instrument model.
type Repository struct{}

// NewRepository returns a Repository.
func NewRepository() *Repository { return &Repository{} }

// precision converts a DB smallint precision into uint8 with an explicit
// bounds check (the column CHECK allows 0..18; never trust a cast).
func precision(v int16) (uint8, error) {
	if v < 0 || v > 18 {
		return 0, fmt.Errorf("instruments: precision %d out of range", v)
	}
	return uint8(v), nil
}

func rawOrEmpty(b json.RawMessage) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}

// CreateVenue registers a venue; duplicate code → CONFLICT.
func (r *Repository) CreateVenue(ctx context.Context, q db.Querier, v Venue) (Venue, error) {
	if v.Code == "" || v.Name == "" {
		return Venue{}, errs.New(errs.CodeValidationFailed, "venue code and name required")
	}
	switch v.Kind {
	case VenueDEXAggregator, VenueDEX, VenueCEX, VenueOTC:
	default:
		return Venue{}, errs.Newf(errs.CodeValidationFailed, "unknown venue kind %q", v.Kind)
	}
	if !v.Status.Valid() {
		return Venue{}, errs.Newf(errs.CodeValidationFailed, "unknown venue status %q", v.Status)
	}
	if v.ID.IsZero() {
		v.ID = id.New[venueKind]()
	}
	row := q.QueryRow(ctx, `INSERT INTO venues (id, code, name, kind, chain, status, settlement_rules)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7) RETURNING `+venueColumns, v.ID, v.Code, v.Name, v.Kind, v.Chain, v.Status, rawOrEmpty(v.SettlementRules))
	out, err := scanVenue(row)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Venue{}, errs.Wrap(err, errs.CodeConflict, "venue code already registered")
		}
		return Venue{}, fmt.Errorf("instruments: create venue: %w", err)
	}
	return out, nil
}

const venueColumns = `id, code, name, kind, coalesce(chain,''), status, settlement_rules, created_at, updated_at`

func scanVenue(row pgx.Row) (Venue, error) {
	var v Venue
	var rules []byte
	if err := row.Scan(&v.ID, &v.Code, &v.Name, &v.Kind, &v.Chain, &v.Status, &rules, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return Venue{}, err
	}
	v.SettlementRules = rules
	return v, nil
}

// GetVenueByCode returns a venue.
func (r *Repository) GetVenueByCode(ctx context.Context, q db.Querier, code string) (Venue, error) {
	v, err := scanVenue(q.QueryRow(ctx, `SELECT `+venueColumns+` FROM venues WHERE code = $1`, code))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Venue{}, errs.New(errs.CodeNotFound, "venue not found").WithField("code", code)
		}
		return Venue{}, fmt.Errorf("instruments: get venue: %w", err)
	}
	return v, nil
}

// SetVenueStatus changes a venue's status (non-agent actors only).
func (r *Repository) SetVenueStatus(ctx context.Context, tx pgx.Tx, venueID VenueID, to VenueStatus, actorType, actorID, reason string) (Venue, error) {
	if actorType == "AGENT" || actorType == "" {
		return Venue{}, errs.New(errs.CodeForbidden, "venue status can only be changed by a non-agent actor")
	}
	if !to.Valid() || reason == "" {
		return Venue{}, errs.New(errs.CodeValidationFailed, "valid status and reason required")
	}
	v, err := scanVenue(tx.QueryRow(ctx, `UPDATE venues SET status = $2 WHERE id = $1 RETURNING `+venueColumns, venueID, to))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Venue{}, errs.New(errs.CodeNotFound, "venue not found")
		}
		return Venue{}, fmt.Errorf("instruments: set venue status: %w", err)
	}
	_ = actorID // audit is appended by the caller's audit writer; the venue table keeps no transition log in V1
	return v, nil
}

// SpotPairSpec describes a V1 spot instrument.
type SpotPairSpec struct {
	Base, Quote, Settlement assets.AssetID
	CanonicalName           string // e.g. "SOL/USDC"
	RiskClass               assets.RiskClass
	Status                  Status
	ActiveFrom              time.Time
	PolicyRef               string
}

// CreateSpotPair creates the ASSET_PRICE exposure on the base asset and the
// SPOT_PAIR instrument in the caller's transaction. Duplicate (base, quote)
// → CONFLICT.
func (r *Repository) CreateSpotPair(ctx context.Context, tx pgx.Tx, spec SpotPairSpec) (Instrument, error) {
	switch {
	case spec.Base.IsZero() || spec.Quote.IsZero() || spec.Settlement.IsZero():
		return Instrument{}, errs.New(errs.CodeValidationFailed, "base, quote and settlement assets required")
	case spec.Base == spec.Quote:
		return Instrument{}, errs.New(errs.CodeValidationFailed, "base and quote must differ")
	case spec.CanonicalName == "":
		return Instrument{}, errs.New(errs.CodeValidationFailed, "canonical name required")
	case !spec.Status.Valid():
		return Instrument{}, errs.Newf(errs.CodeValidationFailed, "unknown status %q", spec.Status)
	case spec.ActiveFrom.IsZero():
		return Instrument{}, errs.New(errs.CodeValidationFailed, "active_from required")
	}
	switch spec.RiskClass {
	case assets.RiskSettlement, assets.RiskMajor, assets.RiskStandard, assets.RiskSpeculative, assets.RiskUnsupported:
	default:
		return Instrument{}, errs.Newf(errs.CodeValidationFailed, "unknown risk class %q", spec.RiskClass)
	}
	expID := id.New[exposureKind]()
	if _, err := tx.Exec(ctx, `INSERT INTO economic_exposures (id, kind, description, underlying_asset_id) VALUES ($1,'ASSET_PRICE',$2,$3)`,
		expID, "price exposure to "+spec.CanonicalName, spec.Base); err != nil {
		return Instrument{}, fmt.Errorf("instruments: create exposure: %w", err)
	}
	row := tx.QueryRow(ctx, `INSERT INTO instruments (id, type, canonical_name, exposure_id, base_asset_id, quote_asset_id, settlement_asset_id, risk_class, status, active_from, policy_ref)
		VALUES ($1,'SPOT_PAIR',$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,'')) RETURNING `+instrumentColumns,
		NewInstrumentID(), spec.CanonicalName, expID, spec.Base, spec.Quote, spec.Settlement, spec.RiskClass, spec.Status, spec.ActiveFrom.UTC(), spec.PolicyRef)
	ins, err := scanInstrument(row)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Instrument{}, errs.Wrap(err, errs.CodeConflict, "spot pair already exists for these assets")
		}
		return Instrument{}, fmt.Errorf("instruments: create instrument: %w", err)
	}
	return ins, nil
}

const instrumentColumns = `id, type, canonical_name, exposure_id, base_asset_id, quote_asset_id, settlement_asset_id, risk_class, status, active_from, active_until, coalesce(policy_ref,''), metadata_version, created_at, updated_at`

func scanInstrument(row pgx.Row) (Instrument, error) {
	var i Instrument
	if err := row.Scan(&i.ID, &i.Type, &i.CanonicalName, &i.ExposureID, &i.BaseAssetID, &i.QuoteAssetID, &i.SettlementAssetID, &i.RiskClass, &i.Status,
		&i.ActiveFrom, &i.ActiveUntil, &i.PolicyRef, &i.MetadataVersion, &i.CreatedAt, &i.UpdatedAt); err != nil {
		return Instrument{}, err
	}
	return i, nil
}

// Get returns an instrument.
func (r *Repository) Get(ctx context.Context, q db.Querier, instrumentID InstrumentID) (Instrument, error) {
	i, err := scanInstrument(q.QueryRow(ctx, `SELECT `+instrumentColumns+` FROM instruments WHERE id = $1`, instrumentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Instrument{}, errs.New(errs.CodeNotFound, "instrument not found").WithField("instrument_id", instrumentID.String())
		}
		return Instrument{}, fmt.Errorf("instruments: get: %w", err)
	}
	return i, nil
}

// List returns instruments ordered by canonical name (bounded).
func (r *Repository) List(ctx context.Context, q db.Querier, limit int) ([]Instrument, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := q.Query(ctx, `SELECT `+instrumentColumns+` FROM instruments ORDER BY canonical_name, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("instruments: list: %w", err)
	}
	defer rows.Close()
	var out []Instrument
	for rows.Next() {
		i, err := scanInstrument(rows)
		if err != nil {
			return nil, fmt.Errorf("instruments: list scan: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// StatusChange is an audited instrument status transition request.
type StatusChange struct {
	To            Status
	ActorType     string
	ActorID       string
	Reason        string
	PolicyVersion string
}

// TransitionStatus applies an instrument status change under a row lock,
// recording it in instrument_status_transitions. Uses the asset transition
// table so instruments and assets share one safety vocabulary.
func (r *Repository) TransitionStatus(ctx context.Context, tx pgx.Tx, instrumentID InstrumentID, ch StatusChange) (Instrument, error) {
	if ch.ActorType == "AGENT" || ch.ActorType == "" {
		return Instrument{}, errs.New(errs.CodeForbidden, "instrument status can only be changed by a non-agent actor")
	}
	if ch.Reason == "" || !ch.To.Valid() {
		return Instrument{}, errs.New(errs.CodeValidationFailed, "valid status and reason required")
	}
	cur, err := scanInstrument(tx.QueryRow(ctx, `SELECT `+instrumentColumns+` FROM instruments WHERE id = $1 FOR UPDATE`, instrumentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Instrument{}, errs.New(errs.CodeNotFound, "instrument not found")
		}
		return Instrument{}, fmt.Errorf("instruments: lock: %w", err)
	}
	if !assets.CanTransition(cur.Status, ch.To) {
		return Instrument{}, errs.Newf(errs.CodeInvalidStateTransition, "instrument status %s -> %s is not allowed", cur.Status, ch.To).
			WithField("from", string(cur.Status)).WithField("to", string(ch.To))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO instrument_status_transitions (id, instrument_id, from_status, to_status, actor_type, actor_id, reason, policy_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))`, id.New[id.Any](), instrumentID, cur.Status, ch.To, ch.ActorType, ch.ActorID, ch.Reason, ch.PolicyVersion); err != nil {
		return Instrument{}, fmt.Errorf("instruments: record transition: %w", err)
	}
	updated, err := scanInstrument(tx.QueryRow(ctx, `UPDATE instruments SET status = $2 WHERE id = $1 RETURNING `+instrumentColumns, instrumentID, ch.To))
	if err != nil {
		return Instrument{}, fmt.Errorf("instruments: update status: %w", err)
	}
	return updated, nil
}

// CreateListing registers a venue listing; duplicate (venue, instrument) → CONFLICT.
func (r *Repository) CreateListing(ctx context.Context, q db.Querier, l VenueListing) (VenueListing, error) {
	switch {
	case l.VenueID.IsZero() || l.InstrumentID.IsZero():
		return VenueListing{}, errs.New(errs.CodeValidationFailed, "venue and instrument required")
	case l.VenueNativeID == "" || l.Network == "":
		return VenueListing{}, errs.New(errs.CodeValidationFailed, "venue native id and network required")
	case l.BasePrecision > 18 || l.QuotePrecision > 18:
		return VenueListing{}, errs.New(errs.CodeValidationFailed, "precision must be <= 18")
	case l.MinNotionalQuote.Sign() < 0:
		return VenueListing{}, errs.New(errs.CodeValidationFailed, "min notional must not be negative")
	case l.MaxNotionalQuote != nil && l.MaxNotionalQuote.Cmp(l.MinNotionalQuote) < 0:
		return VenueListing{}, errs.New(errs.CodeValidationFailed, "max notional below min notional")
	case !l.Status.Valid():
		return VenueListing{}, errs.Newf(errs.CodeValidationFailed, "unknown listing status %q", l.Status)
	}
	if l.ID.IsZero() {
		l.ID = id.New[listingKind]()
	}
	var tick, maxNotional *string
	if l.TickSize != nil {
		s := l.TickSize.String()
		tick = &s
	}
	if l.MaxNotionalQuote != nil {
		s := l.MaxNotionalQuote.String()
		maxNotional = &s
	}
	row := q.QueryRow(ctx, `INSERT INTO venue_listings (id, venue_id, instrument_id, venue_native_id, network, base_mint, quote_mint, tick_size, base_precision, quote_precision, min_notional_quote, max_notional_quote, status, settlement_rules)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8::numeric,$9,$10,$11::numeric,$12::numeric,$13,$14) RETURNING `+listingColumns,
		l.ID, l.VenueID, l.InstrumentID, l.VenueNativeID, l.Network, l.BaseMint, l.QuoteMint, tick, int16(l.BasePrecision), int16(l.QuotePrecision),
		l.MinNotionalQuote.String(), maxNotional, l.Status, rawOrEmpty(l.SettlementRules))
	out, err := scanListing(row)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return VenueListing{}, errs.Wrap(err, errs.CodeConflict, "listing already exists for this venue and instrument")
		}
		return VenueListing{}, fmt.Errorf("instruments: create listing: %w", err)
	}
	return out, nil
}

const listingColumns = `id, venue_id, instrument_id, venue_native_id, network, coalesce(base_mint,''), coalesce(quote_mint,''), tick_size::text, base_precision, quote_precision, min_notional_quote::text, max_notional_quote::text, status, settlement_rules, created_at, updated_at`

func scanListing(row pgx.Row) (VenueListing, error) {
	var (
		l          VenueListing
		tick, maxN *string
		minN       string
		bp, qp     int16
		rules      []byte
	)
	if err := row.Scan(&l.ID, &l.VenueID, &l.InstrumentID, &l.VenueNativeID, &l.Network, &l.BaseMint, &l.QuoteMint, &tick, &bp, &qp, &minN, &maxN, &l.Status, &rules, &l.CreatedAt, &l.UpdatedAt); err != nil {
		return VenueListing{}, err
	}
	var err error
	if l.BasePrecision, err = precision(bp); err != nil {
		return VenueListing{}, err
	}
	if l.QuotePrecision, err = precision(qp); err != nil {
		return VenueListing{}, err
	}
	l.SettlementRules = rules
	q, err := money.ParseQuantity(minN)
	if err != nil {
		return VenueListing{}, fmt.Errorf("instruments: min notional: %w", err)
	}
	l.MinNotionalQuote = q
	if tick != nil {
		t, err := money.ParseQuantity(*tick)
		if err != nil {
			return VenueListing{}, fmt.Errorf("instruments: tick size: %w", err)
		}
		l.TickSize = &t
	}
	if maxN != nil {
		m, err := money.ParseQuantity(*maxN)
		if err != nil {
			return VenueListing{}, fmt.Errorf("instruments: max notional: %w", err)
		}
		l.MaxNotionalQuote = &m
	}
	return l, nil
}

// ListingWithVenue pairs a listing with its venue so callers can evaluate both statuses.
type ListingWithVenue struct {
	Listing VenueListing
	Venue   Venue
}

// ListingsForInstrument returns every listing of an instrument with its venue, ordered by venue code.
func (r *Repository) ListingsForInstrument(ctx context.Context, q db.Querier, instrumentID InstrumentID) ([]ListingWithVenue, error) {
	rows, err := q.Query(ctx, `SELECT l.id, l.venue_id, l.instrument_id, l.venue_native_id, l.network, coalesce(l.base_mint,''), coalesce(l.quote_mint,''), l.tick_size::text, l.base_precision, l.quote_precision, l.min_notional_quote::text, l.max_notional_quote::text, l.status, l.settlement_rules, l.created_at, l.updated_at,
			v.id, v.code, v.name, v.kind, coalesce(v.chain,''), v.status, v.settlement_rules, v.created_at, v.updated_at
		FROM venue_listings l JOIN venues v ON v.id = l.venue_id WHERE l.instrument_id = $1 ORDER BY v.code, l.id`, instrumentID)
	if err != nil {
		return nil, fmt.Errorf("instruments: listings: %w", err)
	}
	defer rows.Close()
	var out []ListingWithVenue
	for rows.Next() {
		var (
			l          VenueListing
			v          Venue
			tick, maxN *string
			minN       string
			bp, qp     int16
			lr, vr     []byte
		)
		if err := rows.Scan(&l.ID, &l.VenueID, &l.InstrumentID, &l.VenueNativeID, &l.Network, &l.BaseMint, &l.QuoteMint, &tick, &bp, &qp, &minN, &maxN, &l.Status, &lr, &l.CreatedAt, &l.UpdatedAt,
			&v.ID, &v.Code, &v.Name, &v.Kind, &v.Chain, &v.Status, &vr, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, fmt.Errorf("instruments: listings scan: %w", err)
		}
		if l.BasePrecision, err = precision(bp); err != nil {
			return nil, err
		}
		if l.QuotePrecision, err = precision(qp); err != nil {
			return nil, err
		}
		l.SettlementRules, v.SettlementRules = lr, vr
		if l.MinNotionalQuote, err = money.ParseQuantity(minN); err != nil {
			return nil, fmt.Errorf("instruments: min notional: %w", err)
		}
		if tick != nil {
			t, err := money.ParseQuantity(*tick)
			if err != nil {
				return nil, fmt.Errorf("instruments: tick: %w", err)
			}
			l.TickSize = &t
		}
		if maxN != nil {
			m, err := money.ParseQuantity(*maxN)
			if err != nil {
				return nil, fmt.Errorf("instruments: max notional: %w", err)
			}
			l.MaxNotionalQuote = &m
		}
		out = append(out, ListingWithVenue{Listing: l, Venue: v})
	}
	return out, rows.Err()
}

// SetListingStatus changes a listing's status (non-agent actors only).
func (r *Repository) SetListingStatus(ctx context.Context, tx pgx.Tx, listingID ListingID, to VenueStatus, actorType, reason string) (VenueListing, error) {
	if actorType == "AGENT" || actorType == "" {
		return VenueListing{}, errs.New(errs.CodeForbidden, "listing status can only be changed by a non-agent actor")
	}
	if !to.Valid() || reason == "" {
		return VenueListing{}, errs.New(errs.CodeValidationFailed, "valid status and reason required")
	}
	l, err := scanListing(tx.QueryRow(ctx, `UPDATE venue_listings SET status = $2 WHERE id = $1 RETURNING `+listingColumns, listingID, to))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return VenueListing{}, errs.New(errs.CodeNotFound, "listing not found")
		}
		return VenueListing{}, fmt.Errorf("instruments: set listing status: %w", err)
	}
	return l, nil
}

// AddExternalIdentifier links a provider-native id to an internal entity.
func (r *Repository) AddExternalIdentifier(ctx context.Context, q db.Querier, entityType, entityID, provider, externalID string) error {
	switch entityType {
	case "ASSET", "INSTRUMENT", "VENUE_LISTING", "VENUE":
	default:
		return errs.Newf(errs.CodeValidationFailed, "unknown entity type %q", entityType)
	}
	if provider == "" || externalID == "" || entityID == "" {
		return errs.New(errs.CodeValidationFailed, "provider, external id and entity id required")
	}
	if _, err := q.Exec(ctx, `INSERT INTO external_identifiers (id, entity_type, entity_id, provider, external_id) VALUES ($1,$2,$3::uuid,$4,$5)`,
		id.New[id.Any](), entityType, entityID, provider, externalID); err != nil {
		if db.IsUniqueViolation(err) {
			return errs.Wrap(err, errs.CodeConflict, "external identifier already mapped")
		}
		return fmt.Errorf("instruments: add external identifier: %w", err)
	}
	return nil
}

// ResolveExternal returns the internal entity id for a provider-native id.
func (r *Repository) ResolveExternal(ctx context.Context, q db.Querier, entityType, provider, externalID string) (string, error) {
	var entityID string
	err := q.QueryRow(ctx, `SELECT entity_id::text FROM external_identifiers WHERE entity_type = $1 AND provider = $2 AND external_id = $3`, entityType, provider, externalID).Scan(&entityID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errs.New(errs.CodeNotFound, "external identifier not mapped")
		}
		return "", fmt.Errorf("instruments: resolve external: %w", err)
	}
	return entityID, nil
}
