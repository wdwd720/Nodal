package instruments

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

// The canonical instrument model (advanced goal §4; ADR-0020).
//
// One Instrument row, one InstrumentType, and one Terms document describe
// every tradable thing: spot crypto, a stable-value token, an equity or ETF, a
// dated or perpetual future, an option, a prediction-market contract and an
// internal synthetic. Terms are versioned: every change is a new immutable
// instrument_versions row with a content hash, and instrument_terms holds the
// current version in normalized columns. Nothing here is venue-specific; a
// venue listing maps an instrument onto one venue.
//
// Every advanced kind settles only in a SIMULATED asset in this build
// (migration 00732 enforces it), which keeps "EVENT_OUTCOME instruments are
// never activatable" true for real value while letting a sandbox prediction
// market trade.

// Additional instrument types.
const (
	TypeStableValue InstrumentType = "STABLE_VALUE"
	TypeEquity      InstrumentType = "EQUITY"
	TypeETF         InstrumentType = "ETF"
	TypeFuture      InstrumentType = "FUTURE"
	TypePerpetual   InstrumentType = "PERPETUAL"
	TypeOption      InstrumentType = "OPTION"
	TypeSynthetic   InstrumentType = "SYNTHETIC"
)

// AllTypes returns every instrument type: SPOT_PAIR is spot crypto and
// EVENT_OUTCOME is a prediction-market contract.
func AllTypes() []InstrumentType {
	return []InstrumentType{
		TypeSpotPair, TypeEventOutcome, TypeStableValue, TypeEquity, TypeETF,
		TypeFuture, TypePerpetual, TypeOption, TypeSynthetic,
	}
}

// Valid reports whether t is a declared type.
func (t InstrumentType) Valid() bool {
	for _, x := range AllTypes() {
		if x == t {
			return true
		}
	}
	return false
}

// Advanced reports whether t is described by Terms and settles only in
// SIMULATED value in this build.
func (t InstrumentType) Advanced() bool { return t != TypeSpotPair }

// IsDerivative reports whether positions in t can be short.
func (t InstrumentType) IsDerivative() bool {
	return t == TypeFuture || t == TypePerpetual || t == TypeOption
}

// OptionRight is CALL or PUT.
type OptionRight string

// Option rights.
const (
	Call OptionRight = "CALL"
	Put  OptionRight = "PUT"
)

// ExerciseStyle is EUROPEAN or AMERICAN.
type ExerciseStyle string

// Exercise styles.
const (
	European ExerciseStyle = "EUROPEAN"
	American ExerciseStyle = "AMERICAN"
)

// SettlementStyle is CASH or PHYSICAL.
type SettlementStyle string

// Settlement styles.
const (
	SettleCash     SettlementStyle = "CASH"
	SettlePhysical SettlementStyle = "PHYSICAL"
)

// MarginModel names how a position in the instrument is margined.
type MarginModel string

// Margin models.
const (
	MarginNone          MarginModel = "NONE"           // fully paid spot
	MarginSpot          MarginModel = "SPOT_MARGIN"    // spot that may be bought on margin
	MarginLinear        MarginModel = "LINEAR"         // futures and perpetuals
	MarginOption        MarginModel = "OPTION"         // long premium paid, short scenario margin
	MarginBinaryOutcome MarginModel = "BINARY_OUTCOME" // fully collateralized outcome shares
)

// CorporateActionBehavior says what a corporate action does to positions.
type CorporateActionBehavior string

// Corporate action behaviours.
const (
	CorporateActionsNone   CorporateActionBehavior = "NONE"
	CorporateActionsAdjust CorporateActionBehavior = "ADJUST"
)

// Transferability says whether positions can move between accounts.
type Transferability string

// Transferabilities.
const (
	Transferable    Transferability = "TRANSFERABLE"
	NonTransferable Transferability = "NON_TRANSFERABLE"
)

// AllOptionRights returns every option right.
func AllOptionRights() []OptionRight { return []OptionRight{Call, Put} }

// AllExerciseStyles returns every exercise style.
func AllExerciseStyles() []ExerciseStyle { return []ExerciseStyle{European, American} }

// AllSettlementStyles returns every settlement style.
func AllSettlementStyles() []SettlementStyle { return []SettlementStyle{SettleCash, SettlePhysical} }

// AllMarginModels returns every margin model.
func AllMarginModels() []MarginModel {
	return []MarginModel{MarginNone, MarginSpot, MarginLinear, MarginOption, MarginBinaryOutcome}
}

// AllCorporateActionBehaviors returns every corporate action behaviour.
func AllCorporateActionBehaviors() []CorporateActionBehavior {
	return []CorporateActionBehavior{CorporateActionsNone, CorporateActionsAdjust}
}

// AllTransferabilities returns every transferability.
func AllTransferabilities() []Transferability {
	return []Transferability{Transferable, NonTransferable}
}

// Terms is the canonical, versioned contract description of an instrument.
// Prices are expressed at PriceScale decimals (a price of 65000.50 at scale 2
// is 6500050); TickSize is in those price units. Quantities are base units of
// the base asset (or contracts for derivatives).
type Terms struct {
	Kind        InstrumentType `json:"kind"`
	Symbol      string         `json:"symbol"`
	VenueCode   string         `json:"venue_code"`
	ExchangeMIC string         `json:"exchange_mic,omitempty"`

	BaseAssetID       *assets.AssetID `json:"base_asset_id,omitempty"`
	QuoteAssetID      *assets.AssetID `json:"quote_asset_id,omitempty"`
	SettlementAssetID assets.AssetID  `json:"settlement_asset_id"`
	// UnderlyingInstrumentID is the instrument a derivative or synthetic
	// references.
	UnderlyingInstrumentID *InstrumentID `json:"underlying_instrument_id,omitempty"`

	// ContractMultiplier is base units of the underlying per contract (1 for
	// spot).
	ContractMultiplier money.Quantity `json:"contract_multiplier"`
	PriceScale         int32          `json:"price_scale"`
	TickSize           money.Quantity `json:"tick_size"`
	LotSize            money.Quantity `json:"lot_size"`
	MinQuantity        money.Quantity `json:"min_quantity"`
	QuantityDecimals   uint8          `json:"quantity_decimals"`

	Expiry          *time.Time      `json:"expiry,omitempty"`
	StrikeMantissa  *money.Quantity `json:"strike_mantissa,omitempty"` // at PriceScale
	OptionRight     OptionRight     `json:"option_right,omitempty"`
	ExerciseStyle   ExerciseStyle   `json:"exercise_style,omitempty"`
	SettlementStyle SettlementStyle `json:"settlement_style"`
	// FundingIntervalMS is the perpetual funding interval.
	FundingIntervalMS int64 `json:"funding_interval_ms,omitempty"`

	TradingCalendar  string   `json:"trading_calendar"` // "24x7" or an exchange MIC calendar such as "XNYS"
	JurisdictionTags []string `json:"jurisdiction_tags"`

	OracleMaxAgeMS int64    `json:"oracle_max_age_ms"`
	OracleSources  []string `json:"oracle_sources"`

	MarginModel             MarginModel             `json:"margin_model"`
	CorporateActionBehavior CorporateActionBehavior `json:"corporate_action_behavior"`
	Transferability         Transferability         `json:"transferability"`

	// Outcomes is the outcome set of an EVENT_OUTCOME contract.
	Outcomes []string `json:"outcomes,omitempty"`
}

var (
	symbolPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._/-]{0,63}$`)
	micPattern    = regexp.MustCompile(`^[A-Z0-9]{4}$`)
)

// Validate checks the terms against their kind. It fails closed: a field the
// kind needs and does not carry is a problem, and so is a field the kind must
// not carry.
func (t Terms) Validate() error {
	var p []string
	if !t.Kind.Valid() {
		p = append(p, fmt.Sprintf("unknown kind %q", t.Kind))
	}
	if !symbolPattern.MatchString(t.Symbol) {
		p = append(p, "symbol must be 1..64 of A-Z 0-9 . _ / -, starting alphanumeric")
	}
	if strings.TrimSpace(t.VenueCode) == "" {
		p = append(p, "venue code required")
	}
	if t.SettlementAssetID.IsZero() {
		p = append(p, "settlement asset required")
	}
	if !t.ContractMultiplier.IsPositive() {
		p = append(p, "contract multiplier must be positive")
	}
	if t.PriceScale < 0 || t.PriceScale > 18 {
		p = append(p, "price scale must be within 0..18")
	}
	if !t.TickSize.IsPositive() || !t.LotSize.IsPositive() {
		p = append(p, "tick size and lot size must be positive")
	}
	if t.MinQuantity.IsNegative() {
		p = append(p, "min quantity must not be negative")
	} else if !t.MinQuantity.IsZero() {
		if _, err := t.MinQuantity.Div(t.LotSize, money.RoundExact); err != nil {
			p = append(p, "min quantity must be a whole number of lots")
		}
	}
	if t.QuantityDecimals > 18 {
		p = append(p, "quantity decimals must be <= 18")
	}
	switch t.SettlementStyle {
	case SettleCash, SettlePhysical:
	default:
		p = append(p, "settlement style must be CASH or PHYSICAL")
	}
	if t.TradingCalendar == "" {
		p = append(p, "trading calendar required")
	}
	if t.OracleMaxAgeMS <= 0 {
		p = append(p, "oracle max age must be positive: every instrument states how fresh its mark must be")
	}
	switch t.MarginModel {
	case MarginNone, MarginSpot, MarginLinear, MarginOption, MarginBinaryOutcome:
	default:
		p = append(p, "unknown margin model")
	}
	switch t.CorporateActionBehavior {
	case CorporateActionsNone, CorporateActionsAdjust:
	default:
		p = append(p, "unknown corporate action behaviour")
	}
	switch t.Transferability {
	case Transferable, NonTransferable:
	default:
		p = append(p, "unknown transferability")
	}
	p = append(p, t.kindProblems()...)
	if len(p) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid instrument terms").WithField("problems", p)
	}
	return nil
}

func (t Terms) kindProblems() []string {
	var p []string
	needUnderlying := func() {
		if t.UnderlyingInstrumentID == nil || t.UnderlyingInstrumentID.IsZero() {
			p = append(p, string(t.Kind)+" requires an underlying instrument")
		}
	}
	noOptionFields := func() {
		if t.StrikeMantissa != nil || t.OptionRight != "" || t.ExerciseStyle != "" {
			p = append(p, string(t.Kind)+" carries no strike, right or exercise style")
		}
	}
	switch t.Kind {
	case TypeSpotPair, TypeStableValue:
		if t.BaseAssetID == nil || t.QuoteAssetID == nil || *t.BaseAssetID == *t.QuoteAssetID {
			p = append(p, string(t.Kind)+" requires two different base and quote assets")
		}
		if t.Expiry != nil {
			p = append(p, string(t.Kind)+" does not expire")
		}
		noOptionFields()
	case TypeEquity, TypeETF:
		if !micPattern.MatchString(t.ExchangeMIC) {
			p = append(p, string(t.Kind)+" requires a four-character exchange MIC")
		}
		if t.BaseAssetID == nil {
			p = append(p, string(t.Kind)+" requires the share asset as base asset")
		}
		if t.CorporateActionBehavior != CorporateActionsAdjust {
			p = append(p, string(t.Kind)+" positions are adjusted by corporate actions")
		}
		noOptionFields()
	case TypeFuture:
		needUnderlying()
		if t.Expiry == nil {
			p = append(p, "a dated future requires an expiry")
		}
		if t.MarginModel != MarginLinear {
			p = append(p, "a future is margined LINEAR")
		}
		noOptionFields()
	case TypePerpetual:
		needUnderlying()
		if t.Expiry != nil {
			p = append(p, "a perpetual does not expire")
		}
		if t.FundingIntervalMS <= 0 {
			p = append(p, "a perpetual requires a funding interval")
		}
		if t.MarginModel != MarginLinear {
			p = append(p, "a perpetual is margined LINEAR")
		}
		noOptionFields()
	case TypeOption:
		needUnderlying()
		if t.Expiry == nil {
			p = append(p, "an option requires an expiry")
		}
		if t.StrikeMantissa == nil || !t.StrikeMantissa.IsPositive() {
			p = append(p, "an option requires a positive strike")
		}
		if t.OptionRight != Call && t.OptionRight != Put {
			p = append(p, "an option is a CALL or a PUT")
		}
		if t.ExerciseStyle != European && t.ExerciseStyle != American {
			p = append(p, "an option is EUROPEAN or AMERICAN")
		}
		if t.MarginModel != MarginOption {
			p = append(p, "an option is margined OPTION")
		}
	case TypeEventOutcome:
		if len(t.Outcomes) < 2 {
			p = append(p, "a prediction contract has at least two outcomes")
		}
		seen := map[string]bool{}
		for _, o := range t.Outcomes {
			if !symbolPattern.MatchString(o) || seen[o] {
				p = append(p, "outcomes must be distinct symbols")
				break
			}
			seen[o] = true
		}
		if t.Expiry == nil {
			p = append(p, "a prediction contract states when trading closes (expiry)")
		}
		if t.SettlementStyle != SettleCash || t.MarginModel != MarginBinaryOutcome {
			p = append(p, "a prediction contract is cash settled and fully collateralized")
		}
		noOptionFields()
	case TypeSynthetic:
		needUnderlying()
	}
	if t.Kind != TypeEventOutcome && len(t.Outcomes) > 0 {
		p = append(p, "only a prediction contract has outcomes")
	}
	if t.Kind != TypePerpetual && t.FundingIntervalMS != 0 {
		p = append(p, "only a perpetual has a funding interval")
	}
	return p
}

// normalized returns t with sorted, de-duplicated tag lists and UTC times so
// that equal terms hash equally.
func (t Terms) normalized() Terms {
	tags := append([]string(nil), t.JurisdictionTags...)
	sort.Strings(tags)
	t.JurisdictionTags = dedupeSorted(tags)
	src := append([]string(nil), t.OracleSources...)
	sort.Strings(src)
	t.OracleSources = dedupeSorted(src)
	if t.Expiry != nil {
		e := t.Expiry.UTC()
		t.Expiry = &e
	}
	if t.JurisdictionTags == nil {
		t.JurisdictionTags = []string{}
	}
	if t.OracleSources == nil {
		t.OracleSources = []string{}
	}
	return t
}

func dedupeSorted(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// CanonicalJSON is the deterministic encoding the version hash covers.
func (t Terms) CanonicalJSON() ([]byte, error) {
	return json.Marshal(t.normalized())
}

// ContentHash returns sha256 of the canonical terms.
func (t Terms) ContentHash() ([]byte, error) {
	b, err := t.CanonicalJSON()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// Version is one immutable instrument_versions row.
type Version struct {
	InstrumentID InstrumentID
	Version      int
	Terms        Terms
	ContentHash  []byte
	Reason       string
	ActorType    string
	ActorID      string
	CreatedAt    time.Time
}

// AdvancedSpec creates an instrument described by Terms.
type AdvancedSpec struct {
	Terms         Terms
	CanonicalName string
	RiskClass     assets.RiskClass
	Status        Status
	ActiveFrom    time.Time
	ActiveUntil   *time.Time
	ActorType     string
	ActorID       string
}

// CreateAdvanced creates the economic exposure, the instrument, its current
// terms and version 1, in the caller's transaction.
func (r *Repository) CreateAdvanced(ctx context.Context, tx pgx.Tx, spec AdvancedSpec) (Instrument, Version, error) {
	t := spec.Terms.normalized()
	if err := t.Validate(); err != nil {
		return Instrument{}, Version{}, err
	}
	if !t.Kind.Advanced() {
		return Instrument{}, Version{}, errs.New(errs.CodeValidationFailed, "a SPOT_PAIR is created with CreateSpotPair")
	}
	if strings.TrimSpace(spec.CanonicalName) == "" || !spec.Status.Valid() || spec.ActiveFrom.IsZero() {
		return Instrument{}, Version{}, errs.New(errs.CodeValidationFailed, "canonical name, a known status and active_from are required")
	}
	if spec.ActorType == "" || spec.ActorType == "AGENT" || strings.TrimSpace(spec.ActorID) == "" {
		return Instrument{}, Version{}, errs.New(errs.CodeValidationFailed, "a non-agent actor is required to define an instrument")
	}
	exposure := "ASSET_PRICE"
	if t.Kind == TypeEventOutcome {
		exposure = "EVENT_OUTCOME"
	}
	var underlyingAsset any
	if t.BaseAssetID != nil {
		underlyingAsset = *t.BaseAssetID
	}
	expID := id.New[exposureKind]()
	var eventTerms any
	if t.Kind == TypeEventOutcome {
		b, err := json.Marshal(map[string]any{"outcomes": t.Outcomes})
		if err != nil {
			return Instrument{}, Version{}, err
		}
		eventTerms = b
	}
	if _, err := tx.Exec(ctx, `INSERT INTO economic_exposures (id, kind, description, underlying_asset_id, event_terms) VALUES ($1,$2,$3,$4,$5)`,
		expID, exposure, "exposure to "+spec.CanonicalName, underlyingAsset, eventTerms); err != nil {
		return Instrument{}, Version{}, fmt.Errorf("instruments: create exposure: %w", err)
	}
	row := tx.QueryRow(ctx, `INSERT INTO instruments (id, type, canonical_name, exposure_id, base_asset_id, quote_asset_id,
		settlement_asset_id, risk_class, status, active_from, active_until)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+instrumentColumns,
		NewInstrumentID(), t.Kind, spec.CanonicalName, expID, t.BaseAssetID, t.QuoteAssetID, t.SettlementAssetID,
		spec.RiskClass, spec.Status, spec.ActiveFrom.UTC(), spec.ActiveUntil)
	ins, err := scanInstrument(row)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Instrument{}, Version{}, errs.Wrap(err, errs.CodeConflict, "instrument already exists")
		}
		return Instrument{}, Version{}, mapInstrumentErr("create instrument", err)
	}
	v, err := r.writeVersion(ctx, tx, ins.ID, 1, t, "created", spec.ActorType, spec.ActorID)
	if err != nil {
		return Instrument{}, Version{}, err
	}
	return ins, v, nil
}

// AmendTerms writes a new version of an instrument's terms (a symbol change,
// a corporate action adjusting the multiplier, a new oracle source). The kind
// and the settlement asset can never change: a different kind is a different
// instrument.
func (r *Repository) AmendTerms(ctx context.Context, tx pgx.Tx, instrumentID InstrumentID, next Terms, reason, actorType, actorID string) (Version, error) {
	next = next.normalized()
	if err := next.Validate(); err != nil {
		return Version{}, err
	}
	if strings.TrimSpace(reason) == "" || actorType == "" || actorType == "AGENT" || strings.TrimSpace(actorID) == "" {
		return Version{}, errs.New(errs.CodeValidationFailed, "a reason and a non-agent actor are required to amend terms")
	}
	cur, err := r.lockCurrentVersion(ctx, tx, instrumentID)
	if err != nil {
		return Version{}, err
	}
	if cur.Terms.Kind != next.Kind || cur.Terms.SettlementAssetID != next.SettlementAssetID {
		return Version{}, errs.New(errs.CodeValidationFailed, "an instrument's kind and settlement asset never change; define a new instrument")
	}
	nextHash, err := next.ContentHash()
	if err != nil {
		return Version{}, err
	}
	if string(nextHash) == string(cur.ContentHash) {
		return cur, nil
	}
	return r.writeVersion(ctx, tx, instrumentID, cur.Version+1, next, reason, actorType, actorID)
}

// CurrentTerms returns an instrument's current terms version.
func (r *Repository) CurrentTerms(ctx context.Context, q db.Querier, instrumentID InstrumentID) (Version, error) {
	return r.version(ctx, q, `SELECT v.instrument_id, v.version, v.terms, v.content_hash, v.reason, v.actor_type, v.actor_id, v.created_at
		FROM instrument_versions v JOIN instrument_terms t ON t.instrument_id = v.instrument_id AND t.version = v.version
		WHERE v.instrument_id = $1`, instrumentID)
}

// TermsHistory returns every version of an instrument's terms, oldest first.
func (r *Repository) TermsHistory(ctx context.Context, q db.Querier, instrumentID InstrumentID) ([]Version, error) {
	rows, err := q.Query(ctx, `SELECT instrument_id, version, terms, content_hash, reason, actor_type, actor_id, created_at
		FROM instrument_versions WHERE instrument_id = $1 ORDER BY version`, instrumentID)
	if err != nil {
		return nil, fmt.Errorf("instruments: terms history: %w", err)
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListByType returns instruments of one type ordered by canonical name.
func (r *Repository) ListByType(ctx context.Context, q db.Querier, t InstrumentType, limit int) ([]Instrument, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := q.Query(ctx, `SELECT `+instrumentColumns+` FROM instruments WHERE type = $1 ORDER BY canonical_name, id LIMIT $2`, t, limit)
	if err != nil {
		return nil, fmt.Errorf("instruments: list by type: %w", err)
	}
	defer rows.Close()
	var out []Instrument
	for rows.Next() {
		i, err := scanInstrument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// Listed is an instrument with its current canonical terms.
type Listed struct {
	Instrument Instrument
	Current    Version
}

// ListWithTerms returns the instruments that carry canonical terms, each with
// its current version, ordered by canonical name. An empty type lists every
// type. Run it in a REPEATABLE READ transaction for one consistent snapshot.
func (r *Repository) ListWithTerms(ctx context.Context, q db.Querier, t InstrumentType, limit int) ([]Listed, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := q.Query(ctx, `SELECT `+instrumentColumns+` FROM instruments
		WHERE id IN (SELECT instrument_id FROM instrument_terms) AND ($1 = '' OR type = $1)
		ORDER BY canonical_name, id LIMIT $2`, string(t), limit)
	if err != nil {
		return nil, fmt.Errorf("instruments: list with terms: %w", err)
	}
	var list []Instrument
	for rows.Next() {
		i, err := scanInstrument(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("instruments: list with terms: %w", err)
	}
	if len(list) == 0 {
		return nil, nil
	}
	ids := make([]InstrumentID, len(list))
	for i, in := range list {
		ids[i] = in.ID
	}
	vrows, err := q.Query(ctx, `SELECT v.instrument_id, v.version, v.terms, v.content_hash, v.reason, v.actor_type, v.actor_id, v.created_at
		FROM instrument_terms t JOIN instrument_versions v ON v.instrument_id = t.instrument_id AND v.version = t.version
		WHERE t.instrument_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("instruments: list with terms: %w", err)
	}
	defer vrows.Close()
	current := make(map[InstrumentID]Version, len(list))
	for vrows.Next() {
		v, err := scanVersion(vrows)
		if err != nil {
			return nil, err
		}
		current[v.InstrumentID] = v
	}
	if err := vrows.Err(); err != nil {
		return nil, fmt.Errorf("instruments: list with terms: %w", err)
	}
	out := make([]Listed, 0, len(list))
	for _, in := range list {
		v, ok := current[in.ID]
		if !ok {
			// The terms row was there when the instruments were listed; only a
			// read outside one snapshot can miss it. Refuse rather than return
			// an instrument without terms.
			return nil, errs.New(errs.CodeConflict, "instrument terms changed during the read; retry").
				WithField("instrument_id", in.ID.String())
		}
		out = append(out, Listed{Instrument: in, Current: v})
	}
	return out, nil
}

func (r *Repository) lockCurrentVersion(ctx context.Context, tx pgx.Tx, instrumentID InstrumentID) (Version, error) {
	return r.version(ctx, tx, `SELECT v.instrument_id, v.version, v.terms, v.content_hash, v.reason, v.actor_type, v.actor_id, v.created_at
		FROM instrument_terms t JOIN instrument_versions v ON v.instrument_id = t.instrument_id AND v.version = t.version
		WHERE t.instrument_id = $1 FOR UPDATE OF t`, instrumentID)
}

func (r *Repository) version(ctx context.Context, q db.Querier, sql string, instrumentID InstrumentID) (Version, error) {
	v, err := scanVersion(q.QueryRow(ctx, sql, instrumentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, errs.New(errs.CodeNotFound, "instrument has no terms").WithField("instrument_id", instrumentID.String())
	}
	return v, err
}

func scanVersion(row pgx.Row) (Version, error) {
	var v Version
	var raw []byte
	if err := row.Scan(&v.InstrumentID, &v.Version, &raw, &v.ContentHash, &v.Reason, &v.ActorType, &v.ActorID, &v.CreatedAt); err != nil {
		return Version{}, err
	}
	if err := json.Unmarshal(raw, &v.Terms); err != nil {
		return Version{}, fmt.Errorf("instruments: decode terms: %w", err)
	}
	v.CreatedAt = v.CreatedAt.UTC()
	return v, nil
}

func (r *Repository) writeVersion(ctx context.Context, tx pgx.Tx, instrumentID InstrumentID, version int, t Terms, reason, actorType, actorID string) (Version, error) {
	body, err := t.CanonicalJSON()
	if err != nil {
		return Version{}, err
	}
	sum := sha256.Sum256(body)
	var created time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO instrument_versions (id, instrument_id, version, terms, content_hash, reason, actor_type, actor_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING created_at`,
		id.New[id.Any](), instrumentID, version, body, sum[:], reason, actorType, actorID).Scan(&created); err != nil {
		return Version{}, mapInstrumentErr("write terms version", err)
	}
	var expiry any
	if t.Expiry != nil {
		expiry = *t.Expiry
	}
	var strike any
	if t.StrikeMantissa != nil {
		strike = t.StrikeMantissa.String()
	}
	_, err = tx.Exec(ctx, `INSERT INTO instrument_terms (instrument_id, version, kind, symbol, venue_code, exchange_mic,
		underlying_instrument_id, contract_multiplier, price_scale, tick_size, lot_size, min_quantity, quantity_decimals,
		expiry, strike_mantissa, option_right, exercise_style, settlement_style, funding_interval_ms, trading_calendar,
		jurisdiction_tags, oracle_max_age_ms, oracle_sources, margin_model, corporate_action_behavior, transferability, outcomes)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8::numeric,$9,$10::numeric,$11::numeric,$12::numeric,$13,$14,$15::numeric,
		        NULLIF($16,''),NULLIF($17,''),$18,NULLIF($19,0),$20,$21,$22,$23,$24,$25,$26,$27)
		ON CONFLICT (instrument_id) DO UPDATE SET version = EXCLUDED.version, symbol = EXCLUDED.symbol,
		  venue_code = EXCLUDED.venue_code, exchange_mic = EXCLUDED.exchange_mic,
		  underlying_instrument_id = EXCLUDED.underlying_instrument_id, contract_multiplier = EXCLUDED.contract_multiplier,
		  price_scale = EXCLUDED.price_scale, tick_size = EXCLUDED.tick_size, lot_size = EXCLUDED.lot_size,
		  min_quantity = EXCLUDED.min_quantity, quantity_decimals = EXCLUDED.quantity_decimals, expiry = EXCLUDED.expiry,
		  strike_mantissa = EXCLUDED.strike_mantissa, option_right = EXCLUDED.option_right, exercise_style = EXCLUDED.exercise_style,
		  settlement_style = EXCLUDED.settlement_style, funding_interval_ms = EXCLUDED.funding_interval_ms,
		  trading_calendar = EXCLUDED.trading_calendar, jurisdiction_tags = EXCLUDED.jurisdiction_tags,
		  oracle_max_age_ms = EXCLUDED.oracle_max_age_ms, oracle_sources = EXCLUDED.oracle_sources,
		  margin_model = EXCLUDED.margin_model, corporate_action_behavior = EXCLUDED.corporate_action_behavior,
		  transferability = EXCLUDED.transferability, outcomes = EXCLUDED.outcomes`,
		instrumentID, version, t.Kind, t.Symbol, t.VenueCode, t.ExchangeMIC, t.UnderlyingInstrumentID,
		t.ContractMultiplier.String(), t.PriceScale, t.TickSize.String(), t.LotSize.String(), t.MinQuantity.String(),
		int16(t.QuantityDecimals), expiry, strike, string(t.OptionRight), string(t.ExerciseStyle), t.SettlementStyle,
		t.FundingIntervalMS, t.TradingCalendar, t.JurisdictionTags, t.OracleMaxAgeMS, t.OracleSources, t.MarginModel,
		t.CorporateActionBehavior, t.Transferability, t.Outcomes)
	if err != nil {
		return Version{}, mapInstrumentErr("write current terms", err)
	}
	return Version{InstrumentID: instrumentID, Version: version, Terms: t, ContentHash: sum[:], Reason: reason,
		ActorType: actorType, ActorID: actorID, CreatedAt: created.UTC()}, nil
}

func mapInstrumentErr(op string, err error) error {
	switch db.SQLState(err) {
	case "IN001":
		return errs.Wrap(err, errs.CodeForbidden, "advanced instruments settle only in SIMULATED value in this build")
	case "IN002":
		return errs.Wrap(err, errs.CodeConflict, "instrument terms versions are append-only and must be consecutive")
	case "DV007":
		// Raised by the derivatives subsystem's binding (00755): a contract's
		// terms are frozen while it has open positions.
		return errs.Wrap(err, errs.CodeConflict, "the instrument's terms cannot change while it has open positions")
	}
	return fmt.Errorf("instruments: %s: %w", op, err)
}
