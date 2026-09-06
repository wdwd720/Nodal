package settlement

import (
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/eligibility"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
)

// Action mirrors intent.Action (PART 35). BUY_EVENT_OUTCOME is not
// representable here: the intent layer rejects it before planning.
type Action string

// Intent actions.
const (
	ActionAcquireNotional Action = "ACQUIRE_NOTIONAL"
	ActionReduceNotional  Action = "REDUCE_NOTIONAL"
	ActionClosePosition   Action = "CLOSE_POSITION"
	ActionTargetExposure  Action = "TARGET_EXPOSURE"
)

// Valid reports whether a is declared.
func (a Action) Valid() bool {
	switch a {
	case ActionAcquireNotional, ActionReduceNotional, ActionClosePosition, ActionTargetExposure:
		return true
	}
	return false
}

// IntentConstraints mirrors intent.Constraints (SETTLEMENT_COMPILER §2).
// Zero values mean "unspecified"; the plan then takes the risk kernel's
// bound.
type IntentConstraints struct {
	MaxSlippageBPS    money.BPS       `json:"max_slippage_bps"`
	MaxFeeBPS         money.BPS       `json:"max_fee_bps"`
	MaxPriceImpactBPS money.BPS       `json:"max_price_impact_bps"`
	MaxPrice          *money.Price    `json:"max_price,omitempty"`
	MinReceive        *money.Quantity `json:"min_receive,omitempty"`
	AllowedVenues     []string        `json:"allowed_venues,omitempty"`
	QuoteFreshness    time.Duration   `json:"quote_freshness"`
	ExecutionDeadline time.Time       `json:"execution_deadline"`
}

// IntentSnapshot is the local projection of intent.TradeIntent: the
// identifiers and fields the planner and executor need. The integrator maps
// the real intent onto it.
type IntentSnapshot struct {
	ID                string             `json:"id"`
	AccountID         string             `json:"account_id"`
	ActorType         security.ActorType `json:"actor_type"`
	ActorID           string             `json:"actor_id"`
	AgentID           string             `json:"agent_id,omitempty"`
	StrategyVersionID string             `json:"strategy_version_id,omitempty"`
	PredictionID      string             `json:"prediction_id,omitempty"`
	Action            Action             `json:"action"`
	InstrumentID      string             `json:"instrument_id"`
	NotionalUSD       *money.USD         `json:"notional_usd,omitempty"`
	TargetExposureUSD *money.USD         `json:"target_exposure_usd,omitempty"`
	Quantity          *money.Quantity    `json:"quantity,omitempty"`
	Constraints       IntentConstraints  `json:"constraints"`
	Deadline          time.Time          `json:"deadline"`
	RequestedAt       time.Time          `json:"requested_at"`
	IdempotencyKey    string             `json:"idempotency_key"`
	CorrelationID     string             `json:"correlation_id"`
	Mode              execution.Mode     `json:"mode"`
	// EnvelopeID is the capital envelope backing an agent intent, if any.
	EnvelopeID string `json:"envelope_id,omitempty"`
}

// AccountState is the account-level input of the planner.
type AccountState struct {
	ID              string          `json:"id"`
	Status          accounts.Status `json:"status"`
	Restrictions    []string        `json:"restrictions,omitempty"`
	Mode            execution.Mode  `json:"mode"`
	WalletID        string          `json:"wallet_id"`
	WalletAddress   string          `json:"wallet_address"`
	WalletChain     string          `json:"wallet_chain"`
	CostBasisMethod string          `json:"cost_basis_method"`
}

// ChainStatus is the observed state of one chain / network.
type ChainStatus struct {
	Health              provider.Health `json:"health"`
	Height              uint64          `json:"height"`
	EstimatedNetworkFee money.Quantity  `json:"estimated_network_fee"`
	NetworkFeeAsset     assets.AssetID  `json:"network_fee_asset"`
	ObservedAt          time.Time       `json:"observed_at"`
}

// SystemHealth is the health input (§3).
type SystemHealth struct {
	Providers                   map[string]provider.Health `json:"providers"`
	Chains                      map[string]ChainStatus     `json:"chains"`
	ActiveKillSwitches          []killswitch.Switch        `json:"active_kill_switches"`
	KillSwitchPolicy            killswitch.Policy          `json:"kill_switch_policy"`
	ReconciliationBlocksNewRisk bool                       `json:"reconciliation_blocks_new_risk"`
}

// VenueCandidate is a venue referenced by the candidate listings together
// with the execution provider that serves it, its disclosed fee, and the
// on-chain identities the signing inspector must allow for it: the venue's
// program ids and the accounts that may receive value (fee accounts).
type VenueCandidate struct {
	Venue       instruments.Venue `json:"venue"`
	Provider    string            `json:"provider"`
	FeeBPS      money.BPS         `json:"fee_bps"`
	ProgramIDs  []string          `json:"program_ids,omitempty"`
	FeeAccounts []string          `json:"fee_accounts,omitempty"`
}

// PlannerInput is the complete, typed input of Planner.Plan (§3). Prices
// are USD-quoted (QuoteAsset "USD") observations per asset; Holdings are the
// account's open positions; Liquidity is the known liquidity estimate per
// listing (absent means unknown and is left to the FINAL risk check).
type PlannerInput struct {
	Intent         IntentSnapshot                           `json:"intent"`
	Account        AccountState                             `json:"account"`
	BuyingPower    capital.BuyingPower                      `json:"buying_power"`
	Instrument     instruments.Instrument                   `json:"instrument"`
	Listings       []instruments.VenueListing               `json:"listings"`
	Venues         []VenueCandidate                         `json:"venues"`
	Assets         map[assets.AssetID]assets.Asset          `json:"assets"`
	AssetPolicies  map[assets.AssetID]valuation.AssetPolicy `json:"asset_policies"`
	Prices         map[assets.AssetID]money.Price           `json:"prices"`
	Holdings       []positions.Holding                      `json:"holdings"`
	Liquidity      map[instruments.ListingID]money.USD      `json:"liquidity"`
	Health         SystemHealth                             `json:"health"`
	Eligibility    eligibility.Decision                     `json:"eligibility"`
	Risk           risk.Decision                            `json:"risk"`
	FeePolicy      fees.Policy                              `json:"fee_policy"`
	FinalityPolicy execution.FinalityPolicy                 `json:"finality_policy"`
	Now            time.Time                                `json:"now"`
	PlannerVersion string                                   `json:"planner_version"`
	DryRun         bool                                     `json:"dry_run"`
	Version        int32                                    `json:"version"`
}
