package agent

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// EnvelopeSnapshot is the read-only view of a capital envelope the runtime
// needs: the authority fields the broker intersects budgets with, and the
// allowlists the evaluator sizes against.
//
// internal/agent does not import internal/capital (AGENT_RUNTIME.md §4,
// depguard rule agent-authority). Agent code can never change an envelope's
// authority: allocation, limits, allowlists, policy version, status and
// validity window move only through capital.EnvelopeAdmin under a non-agent
// principal. The only envelope write on the agent path is the reservation
// performed later by the intent pipeline, and it can only decrease
// available_usd_minor.
type EnvelopeSnapshot struct {
	ID                  string
	AccountID           string
	AgentID             string
	StrategyVersionID   string
	Status              string
	AllocationUSD       money.USD
	AvailableUSD        money.USD
	ReservedUSD         money.USD
	DeployedUSD         money.USD
	MaxSingleTradeUSD   money.USD
	MaxPositionUSD      money.USD
	MaxDailyLossUSD     money.USD
	DailyLossUSD        money.USD
	MaxDataSpendPerDay  money.USD
	MaxModelSpendPerDay money.USD
	MaxOrderRatePerHour int
	AllowedInstruments  []string
	AllowedAssetClasses []string
	AllowedVenues       []string
	PolicyVersion       string
	Version             int64
	EffectiveAt         time.Time
	ExpiresAt           *time.Time
}

// EnvelopeStatusActive is the only status a run may execute against.
const EnvelopeStatusActive = "ACTIVE"

// Usable reports whether the envelope may back a run at now: ACTIVE, in its
// validity window, and with something available.
func (e EnvelopeSnapshot) Usable(now time.Time) bool {
	if e.ID == "" || e.Status != EnvelopeStatusActive {
		return false
	}
	if !e.EffectiveAt.IsZero() && now.Before(e.EffectiveAt) {
		return false
	}
	if e.ExpiresAt != nil && !now.Before(*e.ExpiresAt) {
		return false
	}
	return true
}

// AllowsInstrument reports whether instrumentID is on the envelope's
// allowlist. An empty allowlist permits nothing: an envelope that has not
// named its instruments cannot trade any (fail closed).
func (e EnvelopeSnapshot) AllowsInstrument(instrumentID string) bool {
	if instrumentID == "" {
		return false
	}
	for _, v := range e.AllowedInstruments {
		if v == instrumentID {
			return true
		}
	}
	return false
}

// EnvelopeReader reads one envelope's authority snapshot. It is deliberately
// read-only: there is no method on this interface that can change anything.
type EnvelopeReader interface {
	Envelope(ctx context.Context, q db.Querier, envelopeID string) (EnvelopeSnapshot, error)
}

const selectEnvelopeSQL = `
SELECT id::text, account_id::text, agent_id::text, strategy_version_id::text, status,
       allocation_usd_minor, available_usd_minor, reserved_usd_minor, deployed_usd_minor,
       max_single_trade_usd_minor, max_position_usd_minor, max_daily_loss_usd_minor, daily_loss_usd_minor,
       max_data_spend_usd_minor, max_model_spend_usd_minor, max_order_rate_per_hour,
       allowed_instruments::text[], allowed_asset_classes, allowed_venues,
       policy_version, version, effective_at, expires_at
  FROM capital_envelopes WHERE id = $1`

// PGEnvelopeReader reads capital_envelopes. It issues SELECTs only; the
// package holds no statement that writes the table.
type PGEnvelopeReader struct{}

var _ EnvelopeReader = PGEnvelopeReader{}

// NewEnvelopeReader returns the PostgreSQL envelope reader.
func NewEnvelopeReader() PGEnvelopeReader { return PGEnvelopeReader{} }

// Envelope returns the snapshot, or NOT_FOUND.
func (PGEnvelopeReader) Envelope(ctx context.Context, q db.Querier, envelopeID string) (EnvelopeSnapshot, error) {
	if envelopeID == "" {
		return EnvelopeSnapshot{}, errs.New(errs.CodeValidationFailed, "agent: envelope id is required")
	}
	var (
		e                                     EnvelopeSnapshot
		alloc, avail, reserved, deployed      int64
		maxTrade, maxPos, maxDaily, dailyLoss int64
		maxData, maxModel                     int64
	)
	err := q.QueryRow(ctx, selectEnvelopeSQL, envelopeID).Scan(
		&e.ID, &e.AccountID, &e.AgentID, &e.StrategyVersionID, &e.Status,
		&alloc, &avail, &reserved, &deployed,
		&maxTrade, &maxPos, &maxDaily, &dailyLoss,
		&maxData, &maxModel, &e.MaxOrderRatePerHour,
		&e.AllowedInstruments, &e.AllowedAssetClasses, &e.AllowedVenues,
		&e.PolicyVersion, &e.Version, &e.EffectiveAt, &e.ExpiresAt,
	)
	if err != nil {
		if isNoRows(err) {
			return EnvelopeSnapshot{}, errs.Newf(errs.CodeNotFound, "agent: capital envelope %s not found", envelopeID).
				WithField("envelope_id", envelopeID)
		}
		return EnvelopeSnapshot{}, errs.Wrap(err, errs.CodeInternal, "agent: load capital envelope")
	}
	e.AllocationUSD = money.USDFromMinor(alloc)
	e.AvailableUSD = money.USDFromMinor(avail)
	e.ReservedUSD = money.USDFromMinor(reserved)
	e.DeployedUSD = money.USDFromMinor(deployed)
	e.MaxSingleTradeUSD = money.USDFromMinor(maxTrade)
	e.MaxPositionUSD = money.USDFromMinor(maxPos)
	e.MaxDailyLossUSD = money.USDFromMinor(maxDaily)
	e.DailyLossUSD = money.USDFromMinor(dailyLoss)
	e.MaxDataSpendPerDay = money.USDFromMinor(maxData)
	e.MaxModelSpendPerDay = money.USDFromMinor(maxModel)
	return e, nil
}
