package treasury

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// Document kinds: the domain-separation tags of the canonical forms, checked
// by the database (TY003).
const (
	PolicyDocumentKind        = "treasury.policy.v1"
	SigningPolicyDocumentKind = "treasury.signing_policy.v1"
)

// Policy bounds.
const (
	// MaxMarkAgeCeiling is the stalest mark a policy may accept.
	MaxMarkAgeCeiling = 24 * time.Hour
	// MaxLimitEntries bounds each limit list.
	MaxLimitEntries = 256
	maxLabelLength  = 100
)

// AssetLimit caps how much of one asset the house book may hold, counting
// every working buy order (base units of the asset).
type AssetLimit struct {
	Asset       assets.AssetID
	MaxQuantity money.Quantity
}

// InstrumentLimit caps the house book's exposure through one instrument: the
// mark value of the base asset it holds plus the notional of its working buy
// orders on the instrument (valuation-asset base units).
type InstrumentLimit struct {
	Instrument  instruments.InstrumentID
	MaxExposure money.Quantity
}

// Policy is a house book's risk limits: versioned, hashed, append-only. Every
// amount is in base units of the valuation asset unless it says otherwise.
//
// An asset with no AssetLimit cannot be accumulated, and an instrument with no
// InstrumentLimit cannot be bought through: the limits fail closed. Neither
// stops a sale, which only ever reduces risk.
type Policy struct {
	Label          string
	ValuationAsset assets.AssetID
	// DailyLossLimit bounds the loss measure: the realized PnL of the UTC
	// trading day plus every open loss at a fresh mark. A breach stops new
	// risk until the measure recovers.
	DailyLossLimit money.Quantity
	// MaxMarkAge is the stalest mark a limit or a paper fill may use.
	MaxMarkAge            time.Duration
	MaxInventory          []AssetLimit
	MaxInstrumentExposure []InstrumentLimit
	// PaperSpreadBPS and PaperFeeBPS price the house paper venue: the
	// disclosed spread on the reference mark and an explicit venue fee.
	PaperSpreadBPS money.BPS
	PaperFeeBPS    money.BPS
}

// Validate checks the policy. It returns nil or a VALIDATION_FAILED error
// naming every problem.
func (p Policy) Validate() error {
	var problems []string
	if err := checkLabel(p.Label); err != "" {
		problems = append(problems, "label "+err)
	}
	if p.ValuationAsset.IsZero() {
		problems = append(problems, "valuation_asset required")
	}
	if !p.DailyLossLimit.IsPositive() {
		problems = append(problems, "daily_loss_limit must be positive")
	}
	switch {
	case p.MaxMarkAge <= 0 || p.MaxMarkAge > MaxMarkAgeCeiling:
		problems = append(problems, "max_mark_age must be within (0, 24h]")
	case p.MaxMarkAge%time.Millisecond != 0:
		problems = append(problems, "max_mark_age must be a whole number of milliseconds")
	}
	if len(p.MaxInventory) > MaxLimitEntries || len(p.MaxInstrumentExposure) > MaxLimitEntries {
		problems = append(problems, "too many limit entries")
	}
	seenAsset := map[assets.AssetID]bool{}
	for i, l := range p.MaxInventory {
		key := "max_inventory[" + strconv.Itoa(i) + "]"
		switch {
		case l.Asset.IsZero():
			problems = append(problems, key+".asset required")
		case l.Asset == p.ValuationAsset:
			problems = append(problems, key+" names the valuation asset, which is cash and carries no inventory limit")
		case seenAsset[l.Asset]:
			problems = append(problems, key+" duplicates an asset")
		}
		seenAsset[l.Asset] = true
		if l.MaxQuantity.IsNegative() {
			problems = append(problems, key+".max_quantity must not be negative")
		}
	}
	seenInst := map[instruments.InstrumentID]bool{}
	for i, l := range p.MaxInstrumentExposure {
		key := "max_instrument_exposure[" + strconv.Itoa(i) + "]"
		switch {
		case l.Instrument.IsZero():
			problems = append(problems, key+".instrument required")
		case seenInst[l.Instrument]:
			problems = append(problems, key+" duplicates an instrument")
		}
		seenInst[l.Instrument] = true
		if l.MaxExposure.IsNegative() {
			problems = append(problems, key+".max_exposure must not be negative")
		}
	}
	if p.PaperSpreadBPS < 0 || p.PaperSpreadBPS > fees.MaxSpreadBPS {
		problems = append(problems, "paper_spread_bps out of range")
	}
	if p.PaperFeeBPS < 0 || p.PaperFeeBPS > money.OneHundredPercent {
		problems = append(problems, "paper_fee_bps out of range")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "treasury: invalid policy").WithField("problems", problems)
	}
	return nil
}

func checkLabel(s string) string {
	switch {
	case s == "":
		return "required"
	case len(s) > maxLabelLength:
		return "must be at most 100 bytes"
	case strings.TrimSpace(s) != s:
		return "must not have surrounding whitespace"
	case !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0:
		return "must be printable UTF-8"
	}
	return ""
}

// InventoryCap returns the cap on an asset, if the policy has one.
func (p Policy) InventoryCap(a assets.AssetID) (money.Quantity, bool) {
	for _, l := range p.MaxInventory {
		if l.Asset == a {
			return l.MaxQuantity, true
		}
	}
	return money.Quantity{}, false
}

// ExposureCap returns the cap on an instrument, if the policy has one.
func (p Policy) ExposureCap(i instruments.InstrumentID) (money.Quantity, bool) {
	for _, l := range p.MaxInstrumentExposure {
		if l.Instrument == i {
			return l.MaxExposure, true
		}
	}
	return money.Quantity{}, false
}

type canonicalAssetLimit struct {
	Asset       string `json:"asset"`
	MaxQuantity string `json:"max_quantity"`
}

type canonicalInstrumentLimit struct {
	Instrument  string `json:"instrument"`
	MaxExposure string `json:"max_exposure"`
}

// canonicalPolicy is the hashed form: fields in a fixed order, amounts as
// decimal strings, lists sorted by id and always present.
type canonicalPolicy struct {
	Kind                  string                     `json:"kind"`
	HouseAccountID        string                     `json:"house_account_id"`
	Label                 string                     `json:"label"`
	ValuationAsset        string                     `json:"valuation_asset"`
	DailyLossLimit        string                     `json:"daily_loss_limit"`
	MaxMarkAgeMS          int64                      `json:"max_mark_age_ms"`
	MaxInventory          []canonicalAssetLimit      `json:"max_inventory"`
	MaxInstrumentExposure []canonicalInstrumentLimit `json:"max_instrument_exposure"`
	PaperSpreadBPS        int64                      `json:"paper_spread_bps"`
	PaperFeeBPS           int64                      `json:"paper_fee_bps"`
}

// Document returns the policy's canonical form for a house book.
func (p Policy) Document(house econ.HouseAccountID) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	inv := make([]canonicalAssetLimit, 0, len(p.MaxInventory))
	for _, l := range p.MaxInventory {
		inv = append(inv, canonicalAssetLimit{Asset: l.Asset.String(), MaxQuantity: l.MaxQuantity.String()})
	}
	sort.Slice(inv, func(i, j int) bool { return inv[i].Asset < inv[j].Asset })
	exp := make([]canonicalInstrumentLimit, 0, len(p.MaxInstrumentExposure))
	for _, l := range p.MaxInstrumentExposure {
		exp = append(exp, canonicalInstrumentLimit{Instrument: l.Instrument.String(), MaxExposure: l.MaxExposure.String()})
	}
	sort.Slice(exp, func(i, j int) bool { return exp[i].Instrument < exp[j].Instrument })
	b, err := json.Marshal(canonicalPolicy{
		Kind: PolicyDocumentKind, HouseAccountID: house.String(), Label: p.Label, ValuationAsset: p.ValuationAsset.String(),
		DailyLossLimit: p.DailyLossLimit.String(), MaxMarkAgeMS: p.MaxMarkAge.Milliseconds(), MaxInventory: inv,
		MaxInstrumentExposure: exp, PaperSpreadBPS: int64(p.PaperSpreadBPS), PaperFeeBPS: int64(p.PaperFeeBPS),
	})
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "treasury: encode policy")
	}
	return b, nil
}

// decodePolicy rebuilds a policy from its canonical form and proves it: the
// result must validate and re-encode to exactly the stored bytes.
func decodePolicy(doc []byte) (Policy, econ.HouseAccountID, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	var c canonicalPolicy
	if err := dec.Decode(&c); err != nil {
		return Policy{}, econ.HouseAccountID{}, errs.Wrap(err, errs.CodeInternal, "treasury: stored policy is not a policy")
	}
	bad := func(what string) error {
		return errs.New(errs.CodeInternal, "treasury: stored policy has an invalid "+what)
	}
	if c.Kind != PolicyDocumentKind {
		return Policy{}, econ.HouseAccountID{}, bad("kind")
	}
	house, err := econ.ParseHouseAccountID(c.HouseAccountID)
	if err != nil {
		return Policy{}, econ.HouseAccountID{}, bad("house book")
	}
	valuation, err := assets.ParseAssetID(c.ValuationAsset)
	if err != nil {
		return Policy{}, econ.HouseAccountID{}, bad("valuation asset")
	}
	limit, err := money.ParseQuantity(c.DailyLossLimit)
	if err != nil {
		return Policy{}, econ.HouseAccountID{}, bad("daily loss limit")
	}
	p := Policy{
		Label: c.Label, ValuationAsset: valuation, DailyLossLimit: limit,
		MaxMarkAge: time.Duration(c.MaxMarkAgeMS) * time.Millisecond, PaperSpreadBPS: money.BPS(c.PaperSpreadBPS),
		PaperFeeBPS: money.BPS(c.PaperFeeBPS),
	}
	for _, l := range c.MaxInventory {
		a, err := assets.ParseAssetID(l.Asset)
		if err != nil {
			return Policy{}, econ.HouseAccountID{}, bad("inventory asset")
		}
		q, err := money.ParseQuantity(l.MaxQuantity)
		if err != nil {
			return Policy{}, econ.HouseAccountID{}, bad("inventory cap")
		}
		p.MaxInventory = append(p.MaxInventory, AssetLimit{Asset: a, MaxQuantity: q})
	}
	for _, l := range c.MaxInstrumentExposure {
		i, err := instruments.ParseInstrumentID(l.Instrument)
		if err != nil {
			return Policy{}, econ.HouseAccountID{}, bad("exposure instrument")
		}
		q, err := money.ParseQuantity(l.MaxExposure)
		if err != nil {
			return Policy{}, econ.HouseAccountID{}, bad("exposure cap")
		}
		p.MaxInstrumentExposure = append(p.MaxInstrumentExposure, InstrumentLimit{Instrument: i, MaxExposure: q})
	}
	again, err := p.Document(house)
	if err != nil {
		return Policy{}, econ.HouseAccountID{}, errs.Wrap(err, errs.CodeInternal, "treasury: stored policy does not validate")
	}
	if !bytes.Equal(again, doc) {
		return Policy{}, econ.HouseAccountID{}, errs.New(errs.CodeInternal, "treasury: stored policy is not in canonical form")
	}
	return p, house, nil
}

// hashHex is the lowercase hex sha256 of b.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PolicyVersion is one stored version of a house book's risk policy.
type PolicyVersion struct {
	ID             PolicyID
	HouseAccountID econ.HouseAccountID
	Version        int
	Policy         Policy
	Document       []byte
	Hash           string
	Actor          econ.Actor
	Reason         string
	EffectiveAt    time.Time
}

// paperSchedule is the fee schedule of the house paper venue under this
// policy version, quoted in the valuation asset.
func (v PolicyVersion) paperSchedule(quoteDecimals uint8) fees.Schedule {
	s := fees.ZeroSchedule(v.Policy.ValuationAsset, quoteDecimals)
	s.Version = "treasury-paper/v" + strconv.Itoa(v.Version) + "/" + v.Policy.Label
	s.SpreadBPS = v.Policy.PaperSpreadBPS
	s.VenuePassThroughBPS = v.Policy.PaperFeeBPS
	return s
}

// SigningPolicy is the separate signing policy of a house book's actions. A
// sandbox house book holds no keys: the policy is a recorded rule about who
// may act, checked in Go and again by the database (TY002).
type SigningPolicy struct {
	// ServiceActorID is the one SERVICE actor that may act for the book.
	ServiceActorID string
	// OperatorsAllowed lets operators place orders and fills too. Operators
	// may always cancel and always take administrative actions.
	OperatorsAllowed bool
}

// Validate checks the policy.
func (s SigningPolicy) Validate() error {
	if err := checkLabel(s.ServiceActorID); err != "" {
		return errs.New(errs.CodeValidationFailed, "treasury: the signing policy's service actor id "+err)
	}
	return nil
}

type canonicalSigningPolicy struct {
	Kind              string   `json:"kind"`
	HouseAccountID    string   `json:"house_account_id"`
	KeyCustody        string   `json:"key_custody"`
	ServiceActorID    string   `json:"service_actor_id"`
	OperatorsAllowed  bool     `json:"operators_allowed"`
	AllowedActorTypes []string `json:"allowed_actor_types"`
	OperatorOnly      []string `json:"operator_only_actions"`
}

// Document returns the signing policy's canonical form for a house book.
func (s SigningPolicy) Document(house econ.HouseAccountID) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var allowed []string
	for _, t := range AuthorizedActorTypes() {
		allowed = append(allowed, string(t))
	}
	var operatorOnly []string
	for _, k := range AllActionKinds() {
		if k.administrative() {
			operatorOnly = append(operatorOnly, string(k))
		}
	}
	b, err := json.Marshal(canonicalSigningPolicy{
		Kind: SigningPolicyDocumentKind, HouseAccountID: house.String(), KeyCustody: KeyCustodyNone,
		ServiceActorID: s.ServiceActorID, OperatorsAllowed: s.OperatorsAllowed, AllowedActorTypes: allowed,
		OperatorOnly: operatorOnly,
	})
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "treasury: encode signing policy")
	}
	return b, nil
}

// SigningPolicyVersion is one stored version of a house book's signing policy.
type SigningPolicyVersion struct {
	ID             PolicyID
	HouseAccountID econ.HouseAccountID
	Version        int
	Policy         SigningPolicy
	Document       []byte
	Hash           string
	Actor          econ.Actor
	Reason         string
	EffectiveAt    time.Time
}

// Authorize decides whether actor may take an action of kind k for the house
// book: an operator for an administrative action; the named house service or
// (when allowed) an operator for anything else; an operator may always
// cancel. A customer, an agent and an anonymous system actor never may.
func (v SigningPolicyVersion) Authorize(actor econ.Actor, k ActionKind) error {
	deny := func(why string) error {
		return rejection(errs.CodeForbidden, ReasonNotAuthorized, why).
			WithField("actor_type", string(actor.Type)).WithField("action", string(k)).
			WithField("house_account_id", v.HouseAccountID.String())
	}
	if strings.TrimSpace(actor.ID) == "" {
		return deny("an actor id is required")
	}
	switch actor.Type {
	case security.ActorOperator:
		if k.administrative() || k == ActionCancelOrder || v.Policy.OperatorsAllowed {
			return nil
		}
		return deny("the house book's signing policy does not let operators trade for it")
	case security.ActorService:
		if k.administrative() {
			return deny(string(k) + " is an operator's action")
		}
		if actor.ID != v.Policy.ServiceActorID {
			return deny("service " + actor.ID + " is not the house service the signing policy names")
		}
		return nil
	}
	return deny(string(actor.Type) + " actors never act for a house book")
}
