package derivatives

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// Defining a contract.
//
// A future, a perpetual or an option is an instrument like any other: it is
// created the one way every instrument is, instruments.Repository.
// CreateAdvanced, in the caller's transaction. What this package adds is what
// it knows about the contracts it trades. The terms it fixes are filled in here
// -- a contract trades 24x7 (Calendar24x7) on the sandbox venue, settles in
// cash, is margined by its kind's model (LINEAR, or OPTION), carries no
// corporate actions and cannot be transferred -- and before anything is
// written the terms are resolved into the Spec every operation of this
// package trades, runs and settles by (NewSpec). So a contract this package
// could not settle exactly (a tick of one contract that is not a whole number
// of settlement base units, a strike off the tick grid), one that would settle
// in anything but SIMULATED value, and one whose underlying names no asset are
// refused at definition, never discovered on their first trade; and so is one
// settling in a supply-controlled asset, on which no position could ever be
// opened (supply.go). Once written, the contract is read back through the
// same resolution every operation uses (loadSpec).

// SandboxVenue is the venue code of every contract this package defines.
const SandboxVenue = "NODAL-SANDBOX"

// ContractDefinition is what an operator chooses about a new contract.
// Prices -- the tick size and an option's strike -- are integer mantissas at
// PriceScale, in whole settlement units per whole unit of the underlying (the
// convention of instruments.Terms); ContractMultiplier is base units of the
// underlying per contract; LotSize and MinQuantity are contracts.
type ContractDefinition struct {
	Kind   instruments.InstrumentType
	Symbol string
	// CanonicalName is the instrument's name; empty names it by Symbol.
	CanonicalName          string
	UnderlyingInstrumentID instruments.InstrumentID
	SettlementAssetID      assets.AssetID
	ContractMultiplier     money.Quantity
	PriceScale             int32
	TickSize               money.Quantity
	LotSize                money.Quantity
	MinQuantity            money.Quantity
	// Expiry is required of a future and an option, and refused for a
	// perpetual; it must lie after the definition.
	Expiry *time.Time
	// FundingIntervalMS is a perpetual's funding interval.
	FundingIntervalMS int64
	// StrikeMantissa, OptionRight and ExerciseStyle are an option's.
	StrikeMantissa *money.Quantity
	OptionRight    instruments.OptionRight
	ExerciseStyle  instruments.ExerciseStyle
	OracleMaxAgeMS int64
	OracleSources  []string
	RiskClass      assets.RiskClass
	// Actor defines the contract: a person, never an agent.
	Actor econ.Actor
}

// terms returns the definition's instrument terms, with everything this
// package fixes filled in.
func (d ContractDefinition) terms() instruments.Terms {
	t := instruments.Terms{
		Kind: d.Kind, Symbol: d.Symbol, VenueCode: SandboxVenue, SettlementAssetID: d.SettlementAssetID,
		ContractMultiplier: d.ContractMultiplier, PriceScale: d.PriceScale, TickSize: d.TickSize, LotSize: d.LotSize,
		MinQuantity: d.MinQuantity, Expiry: d.Expiry, FundingIntervalMS: d.FundingIntervalMS,
		StrikeMantissa: d.StrikeMantissa, OptionRight: d.OptionRight, ExerciseStyle: d.ExerciseStyle,
		SettlementStyle: instruments.SettleCash, TradingCalendar: Calendar24x7, OracleMaxAgeMS: d.OracleMaxAgeMS,
		OracleSources: append([]string(nil), d.OracleSources...), MarginModel: instruments.MarginLinear,
		CorporateActionBehavior: instruments.CorporateActionsNone, Transferability: instruments.NonTransferable,
	}
	if !d.UnderlyingInstrumentID.IsZero() {
		under := d.UnderlyingInstrumentID
		t.UnderlyingInstrumentID = &under
	}
	if d.Kind == instruments.TypeOption {
		t.MarginModel = instruments.MarginOption
	}
	return t
}

// DefineContract creates a future, a perpetual or an option, in the caller's
// transaction, and returns the instrument and its first terms version. A
// definition this package could not trade is refused before anything is
// written; the caller rolls back on any error.
func (s *Service) DefineContract(ctx context.Context, tx pgx.Tx, d ContractDefinition) (instruments.Instrument, instruments.Version, error) {
	if !d.Kind.IsDerivative() {
		return instruments.Instrument{}, instruments.Version{}, errs.Newf(errs.CodeValidationFailed,
			"derivatives: a contract is a FUTURE, a PERPETUAL or an OPTION, not %q", d.Kind)
	}
	if d.Actor.Type == security.ActorAgent || d.Actor.Type == "" || strings.TrimSpace(d.Actor.ID) == "" {
		return instruments.Instrument{}, instruments.Version{}, errs.New(errs.CodeForbidden,
			"derivatives: a contract is defined by a person, never by an agent")
	}
	switch d.RiskClass {
	case assets.RiskMajor, assets.RiskStandard, assets.RiskSpeculative:
	default:
		return instruments.Instrument{}, instruments.Version{}, errs.New(errs.CodeValidationFailed,
			"derivatives: a contract's risk class is MAJOR, STANDARD or SPECULATIVE").WithField("risk_class", string(d.RiskClass))
	}
	now := s.clk.Now().UTC()
	if d.Expiry != nil && !d.Expiry.After(now) {
		return instruments.Instrument{}, instruments.Version{}, errs.New(errs.CodeValidationFailed,
			"derivatives: a contract's expiry lies after its definition").WithField("expiry", d.Expiry.UTC().Format(time.RFC3339Nano))
	}
	terms := d.terms()
	if err := terms.Validate(); err != nil {
		return instruments.Instrument{}, instruments.Version{}, err
	}

	// Resolve the terms exactly as every operation will, before writing them.
	under, err := s.instruments.Get(ctx, tx, d.UnderlyingInstrumentID)
	if err != nil {
		return instruments.Instrument{}, instruments.Version{}, err
	}
	if under.BaseAssetID == nil {
		return instruments.Instrument{}, instruments.Version{}, errs.New(errs.CodeValidationFailed,
			"derivatives: the underlying instrument names no asset a contract could be on").
			WithField("underlying_instrument_id", under.ID.String())
	}
	ar := assets.NewRepository()
	settlement, err := ar.Get(ctx, tx, d.SettlementAssetID)
	if err != nil {
		return instruments.Instrument{}, instruments.Version{}, err
	}
	underlying, err := ar.Get(ctx, tx, *under.BaseAssetID)
	if err != nil {
		return instruments.Instrument{}, instruments.Version{}, err
	}
	spec, err := NewSpec(instruments.Instrument{Type: d.Kind, SettlementAssetID: settlement.ID, Status: assets.StatusActive},
		terms, settlement, underlying)
	if err != nil {
		return instruments.Instrument{}, instruments.Version{}, err
	}
	if err := refuseSupplyControlledSettlement(ctx, tx, spec); err != nil {
		return instruments.Instrument{}, instruments.Version{}, err
	}

	// A symbol names one contract: two under one symbol could not be told
	// apart on a position, a flow or a mark. The check and the insert are
	// serialized per symbol.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "derivatives.contract-symbol:"+terms.Symbol); err != nil {
		return instruments.Instrument{}, instruments.Version{}, dbErr("lock the contract symbol", err)
	}
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instrument_terms
		WHERE kind IN ('FUTURE','PERPETUAL','OPTION') AND symbol = $1)`, terms.Symbol).Scan(&taken); err != nil {
		return instruments.Instrument{}, instruments.Version{}, dbErr("look up the contract symbol", err)
	}
	if taken {
		return instruments.Instrument{}, instruments.Version{}, errs.New(errs.CodeConflict,
			"derivatives: a contract with this symbol already exists").WithField("symbol", terms.Symbol)
	}

	name := strings.TrimSpace(d.CanonicalName)
	if name == "" {
		name = terms.Symbol
	}
	ins, ver, err := s.instruments.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
		Terms: terms, CanonicalName: name, RiskClass: d.RiskClass, Status: assets.StatusActive, ActiveFrom: now,
		ActorType: string(d.Actor.Type), ActorID: d.Actor.ID,
	})
	if err != nil {
		return instruments.Instrument{}, instruments.Version{}, err
	}
	if _, err := loadSpec(ctx, tx, s.instruments, ins.ID); err != nil {
		return instruments.Instrument{}, instruments.Version{}, err
	}
	return ins, ver, nil
}
