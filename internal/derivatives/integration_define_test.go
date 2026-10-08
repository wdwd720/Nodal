//go:build integration

package derivatives

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// definition is a contract on the fixture's spot pair, settled in its sandbox
// dollar, on the grid the other tests trade (0.01 of the underlying a
// contract, a $0.50 tick at price scale 2).
func (f *fixture) definition(kind instruments.InstrumentType) ContractDefinition {
	d := ContractDefinition{
		Kind: kind, Symbol: "DEF-" + string(kind) + "-" + strings.ToUpper(uuid.NewString()[:8]),
		UnderlyingInstrumentID: f.spot, SettlementAssetID: f.usd, ContractMultiplier: q(1_000_000), PriceScale: 2,
		TickSize: q(50), LotSize: q(1), MinQuantity: q(1), OracleMaxAgeMS: 60_000, OracleSources: []string{"itest"},
		RiskClass: assets.RiskSpeculative, Actor: econ.Actor{Type: security.ActorOperator, ID: "itest-operator"},
	}
	expiry := f.clk.Now().Add(48 * time.Hour)
	switch kind {
	case instruments.TypePerpetual:
		d.FundingIntervalMS = 8 * 3600 * 1000
	case instruments.TypeFuture:
		d.Expiry = &expiry
	case instruments.TypeOption:
		strike := dollars(65_000)
		d.Expiry, d.StrikeMantissa, d.OptionRight, d.ExerciseStyle = &expiry, &strike, instruments.Call, instruments.European
	}
	return d
}

func (f *fixture) define(d ContractDefinition) (instruments.Instrument, instruments.Version, error) {
	var ins instruments.Instrument
	var ver instruments.Version
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ins, ver, err = f.svc.DefineContract(ctx, tx, d)
		return err
	})
	return ins, ver, err
}

// TestIntegration_ADefinedContractIsOneEveryOperationTrades: each kind is
// created through the one instrument path with the terms this package fixes,
// resolves into the Spec every operation reads, and is traded like any other
// contract -- a position opens in the perpetual at its first mark.
func TestIntegration_ADefinedContractIsOneEveryOperationTrades(t *testing.T) {
	f := newFixture(t)
	for _, kind := range []instruments.InstrumentType{instruments.TypePerpetual, instruments.TypeFuture, instruments.TypeOption} {
		d := f.definition(kind)
		ins, ver, err := f.define(d)
		require.NoError(t, err, kind)
		assert.Equal(t, kind, ins.Type)
		assert.Equal(t, assets.StatusActive, ins.Status)
		assert.Equal(t, d.Symbol, ins.CanonicalName, "named by its symbol when no name is given")
		assert.Equal(t, assets.RiskSpeculative, ins.RiskClass)
		assert.Equal(t, 1, ver.Version)
		assert.Equal(t, "OPERATOR", ver.ActorType)
		assert.Equal(t, "itest-operator", ver.ActorID)

		terms := ver.Terms
		assert.Equal(t, Calendar24x7, terms.TradingCalendar)
		assert.Equal(t, SandboxVenue, terms.VenueCode)
		assert.Equal(t, instruments.SettleCash, terms.SettlementStyle)
		assert.Equal(t, instruments.NonTransferable, terms.Transferability)
		assert.Equal(t, instruments.CorporateActionsNone, terms.CorporateActionBehavior)
		want := instruments.MarginLinear
		if kind == instruments.TypeOption {
			want = instruments.MarginOption
		}
		assert.Equal(t, want, terms.MarginModel)

		spec := f.spec(ins.ID)
		assert.Equal(t, "5000", spec.TickValue.String(), "one tick of one contract is 5,000 base units of the sandbox dollar")
		assert.Equal(t, f.btc, spec.UnderlyingAssetID)
	}

	perp, _, err := f.define(f.definition(instruments.TypePerpetual))
	require.NoError(t, err)
	f.mark(perp.ID, 65_000, 65_000)
	a := f.account()
	f.fund(a, usdUnits(100_000))
	out := f.open(a, perp.ID, Long, 2)
	require.NotNil(t, out.Position, "a position opens in a defined contract")
	assert.Equal(t, "2", out.Position.Quantity.String())
}

// TestIntegration_ADefinitionThePackageCouldNotTradeIsRefusedWithNothingWritten:
// every refusal comes before the instrument is written, so a refused
// definition leaves no instrument, terms or version behind.
func TestIntegration_ADefinitionThePackageCouldNotTradeIsRefusedWithNothingWritten(t *testing.T) {
	f := newFixture(t)
	var supply econtest.Supply
	var realAsset assets.Asset
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if supply, err = econtest.RegisterSupply(ctx, tx); err != nil {
			return err
		}
		realAsset, err = assets.NewRepository().Create(ctx, tx, assets.Asset{
			Chain: "solana-devnet", MintAddress: "mint-" + uuid.NewString(), Kind: assets.KindSPLToken,
			ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "DEFREAL", Name: "a real token", Decimals: 6,
			IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
		})
		return err
	}))

	taken := f.definition(instruments.TypePerpetual)
	_, _, err := f.define(taken)
	require.NoError(t, err)

	past := f.clk.Now().Add(-time.Minute)
	cases := []struct {
		name   string
		change func(d *ContractDefinition)
		code   errs.Code
	}{
		{"not a derivative kind", func(d *ContractDefinition) { d.Kind = instruments.TypeSpotPair }, errs.CodeValidationFailed},
		{"defined by an agent", func(d *ContractDefinition) { d.Actor = econ.Actor{Type: security.ActorAgent, ID: "agent-1"} }, errs.CodeForbidden},
		{"defined by nobody", func(d *ContractDefinition) { d.Actor = econ.Actor{} }, errs.CodeForbidden},
		{"an unsupported risk class", func(d *ContractDefinition) { d.RiskClass = assets.RiskUnsupported }, errs.CodeValidationFailed},
		{"a perpetual that expires", func(d *ContractDefinition) { d.Expiry = &past }, errs.CodeValidationFailed},
		{"no funding interval", func(d *ContractDefinition) { d.FundingIntervalMS = 0 }, errs.CodeValidationFailed},
		{"a symbol the instrument model refuses", func(d *ContractDefinition) { d.Symbol = "lower case" }, errs.CodeValidationFailed},
		{"no oracle age", func(d *ContractDefinition) { d.OracleMaxAgeMS = 0 }, errs.CodeValidationFailed},
		// A contract of one base unit of the underlying (10^-8 of it) moves
		// 0.005 of a base unit of the sandbox dollar a $0.50 tick: no flow of
		// it could ever be paid exactly.
		{"a tick that is not a whole base unit", func(d *ContractDefinition) { d.ContractMultiplier = q(1) }, errs.CodeValidationFailed},
		{"an absent underlying", func(d *ContractDefinition) { d.UnderlyingInstrumentID = instruments.NewInstrumentID() }, errs.CodeNotFound},
		{"an absent settlement asset", func(d *ContractDefinition) { d.SettlementAssetID = assets.NewAssetID() }, errs.CodeNotFound},
		{"real value", func(d *ContractDefinition) { d.SettlementAssetID = realAsset.ID }, errs.CodeForbidden},
		{"a symbol already taken", func(d *ContractDefinition) { d.Symbol = taken.Symbol }, errs.CodeConflict},
	}
	for _, c := range cases {
		d := f.definition(instruments.TypePerpetual)
		c.change(&d)
		_, _, err := f.define(d)
		require.Error(t, err, c.name)
		assert.Equal(t, c.code, errs.CodeOf(err), "%s: %v", c.name, err)
		if c.name != "a symbol already taken" {
			assert.Zero(t, f.count(`SELECT count(*) FROM instrument_terms WHERE symbol = $1`, d.Symbol), "%s wrote terms", c.name)
		}
	}
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM instrument_terms WHERE symbol = $1`, taken.Symbol), "the symbol names one contract")

	// An option's strike must lie on its tick grid, and its expiry after now.
	off := f.definition(instruments.TypeOption)
	strike := dollars(65_000).Add(q(1))
	off.StrikeMantissa = &strike
	_, _, err = f.define(off)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a strike off the tick grid: %v", err)
	expired := f.definition(instruments.TypeFuture)
	expired.Expiry = &past
	_, _, err = f.define(expired)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a future that has already expired: %v", err)

	// A contract settled in a supply-controlled asset could never open a
	// position (supply.go): refused at definition, by name.
	for _, controlled := range supply.Controlled() {
		d := f.definition(instruments.TypePerpetual)
		d.SettlementAssetID = controlled.Asset
		_, _, err := f.define(d)
		econtest.RequireRefusal(t, err, controlled.Asset, controlled.Controller)
		assert.Zero(t, f.count(`SELECT count(*) FROM instrument_terms WHERE symbol = $1`, d.Symbol))
	}
}
