package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/risk"
)

// The advanced financial capabilities (ADR-0020). Every figure behind these
// ports is value domain SIMULATED.

// AdvancedInstrumentView is an instrument with its current terms and, when
// requested, every version of them.
type AdvancedInstrumentView struct {
	Instrument instruments.Instrument
	Current    instruments.Version
	History    []instruments.Version
}

// AdvancedBuyingPower is buying power V2 with the asset its figures are
// valued in.
type AdvancedBuyingPower struct {
	V2    buyingpower.V2
	Quote assets.Asset
}

// AdvancedCorePort serves the foundation of the advanced capabilities: their
// gate decisions, the canonical instruments, the economic intents and effects
// of an account, Risk Kernel V2 decisions and buying power V2.
type AdvancedCorePort interface {
	Capabilities(ctx context.Context) (environment string, states []econ.CapabilityState, err error)
	Instruments(ctx context.Context, kind instruments.InstrumentType, limit int) ([]AdvancedInstrumentView, error)
	Instrument(ctx context.Context, id instruments.InstrumentID) (AdvancedInstrumentView, error)
	Intents(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]econ.Intent, string, error)
	Intent(ctx context.Context, id econ.IntentID) (econ.Intent, error)
	Effects(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]econ.Effect, string, error)
	RiskDecisions(ctx context.Context, account accounts.AccountID, limit int) ([]risk.AdvancedDecisionRecord, error)
	BuyingPower(ctx context.Context, account accounts.AccountID) (AdvancedBuyingPower, error)
}
