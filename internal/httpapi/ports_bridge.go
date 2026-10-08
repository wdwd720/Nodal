package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
)

// BridgePort serves cross-chain capital (internal/bridge; ADR-0020) and the
// simulated chain host it and the stable asset run on (internal/devnethost).
// Every figure behind it is value domain SIMULATED; the chains are a
// simulation the books are reconciled against.
//
// The bridge service decides everything: Bridge and ReconcileLeg run its own
// multi-transaction workflows, route commands run one service operation in
// one transaction, and the reads present its own answers (GetRoute, GetLeg,
// BuyingPower) and, where the package exports no listing, project its tables
// read-only. The chain commands are the host's own calls; the host refuses
// fault injection outside LOCAL, TEST and DEV.
type BridgePort interface {
	// Routes lists the routes, oldest first.
	Routes(ctx context.Context, limit int) ([]BridgeRouteView, error)
	Route(ctx context.Context, id bridge.RouteID) (BridgeRouteView, error)
	Legs(ctx context.Context, account accounts.AccountID, cursor string, limit int) (BridgeLegPage, error)
	// Leg returns one leg with its transactions on the chains and its
	// transitions; the caller scopes it by the leg's own account.
	Leg(ctx context.Context, id bridge.LegID) (BridgeLegDetailView, error)
	// Holdings returns an account's WALLET balances of SIMULATED assets by
	// chain and the bridge's part of its buying power.
	Holdings(ctx context.Context, account accounts.AccountID) (BridgeHoldingsView, error)
	// Chains reads both chains of the host at one instant.
	Chains(ctx context.Context) (BridgeChainsView, error)

	// Bridge runs a leg for the account (bridge.Service.Bridge). A lock
	// submission whose outcome is unknown is not an error here: the leg is
	// returned not settled, with its reconciliation state.
	Bridge(ctx context.Context, c BridgeLegCommand) (BridgeLegOutcome, error)

	CreateRoute(ctx context.Context, def bridge.RouteDefinition, by econ.Actor) (BridgeRouteView, error)
	SetRouteStatus(ctx context.Context, id bridge.RouteID, to bridge.RouteStatus, by econ.Actor, reason string) (BridgeRouteView, error)
	ReconcileRoute(ctx context.Context, id bridge.RouteID, by econ.Actor) (bridge.RouteReconciliation, error)
	// ReconcileLeg drives a leg from both chains by transaction id
	// (bridge.Service.Advance): it never resubmits a lock nor races a release
	// that can still land, and reconciles the leg once its destination is
	// final.
	ReconcileLeg(ctx context.Context, id bridge.LegID) (BridgeLegOutcome, error)
	ProduceBlocks(ctx context.Context, chain devnet.ChainID, n uint64) (BridgeChainView, error)
	InjectFault(ctx context.Context, chain devnet.ChainID, f BridgeFault) (BridgeChainView, error)

	// ResumeRelay, ResolveReview and Refund are an operator's three ways out
	// of a leg's MANUAL_REVIEW (bridge.Service.ResumeRelay, ResolveReview and
	// Refund); the service decides which one a leg admits. Refund runs the
	// service's own workflow, which owns its transactions and submits the
	// refund once.
	ResumeRelay(ctx context.Context, id bridge.LegID, by econ.Actor, reason string) (BridgeLegView, error)
	ResolveReview(ctx context.Context, id bridge.LegID, by econ.Actor, reason string) (BridgeLegView, error)
	Refund(ctx context.Context, id bridge.LegID, by econ.Actor, reason string) (BridgeLegOutcome, error)
}

// BridgeFaultKind names a submission fault of the simulated chain host.
type BridgeFaultKind string

// The faults the host can arm (LOCAL, TEST and DEV only).
const (
	BridgeFaultTimeoutButLand BridgeFaultKind = "TIMEOUT_BUT_LAND"
	BridgeFaultTimeoutAndDrop BridgeFaultKind = "TIMEOUT_AND_DROP"
	BridgeFaultDropNext       BridgeFaultKind = "DROP_NEXT"
	BridgeFaultDelayInclusion BridgeFaultKind = "DELAY_INCLUSION"
	BridgeFaultClearFaults    BridgeFaultKind = "CLEAR_FAULTS"
)

// BridgeFault is one fault to arm. Count is the drops of DROP_NEXT and the
// blocks of DELAY_INCLUSION; the other kinds take none.
type BridgeFault struct {
	Kind  BridgeFaultKind
	Count uint64
}

// BridgeRouteView is a route with the labels of its two assets.
type BridgeRouteView struct {
	Route       bridge.Route
	Source      StableAssetLabel
	Destination StableAssetLabel
}

// BridgeLegEffects are the statuses of a leg's economic effects; nil where
// the leg has not claimed that effect.
type BridgeLegEffects struct {
	Source      *econ.EffectStatus
	Destination *econ.EffectStatus
	Refund      *econ.EffectStatus
}

// BridgeLegReconciliation is what a leg's effects say about unknown outcomes:
// whether one is unknown now (Awaiting), and when one last became unknown and
// was last settled from the chains.
type BridgeLegReconciliation struct {
	Awaiting     bool
	UnknownSince *time.Time
	ResolvedAt   *time.Time
}

// BridgeLegView is a leg with its route, its assets' labels, its effects and
// its reconciliation state.
type BridgeLegView struct {
	Leg            bridge.Leg
	Route          BridgeRouteView
	Effects        BridgeLegEffects
	Reconciliation BridgeLegReconciliation
}

// BridgeChainTxView is what a chain says of one transaction at one instant.
type BridgeChainTxView struct {
	Chain  devnet.ChainID
	TxID   devnet.TxID
	Status devnet.TxStatus
}

// BridgeAttemptView is one release or refund attempt of a leg and what its
// chain says of it.
type BridgeAttemptView struct {
	Number          int
	Kind            bridge.AttemptKind
	Chain           devnet.ChainID
	TxID            devnet.TxID
	BlockhashHeight uint64
	CreatedAt       time.Time
	OnChain         BridgeChainTxView
}

// BridgeLegTransition is one state change of a leg.
type BridgeLegTransition struct {
	From       string
	To         bridge.CrossChainCapitalState
	ActorType  string
	ActorID    string
	Reason     string
	OccurredAt time.Time
}

// BridgeLegDetailView is a leg with its lock (nil before it is signed), its
// attempts in order and every state it passed through, oldest first.
type BridgeLegDetailView struct {
	BridgeLegView
	Lock        *BridgeChainTxView
	Attempts    []BridgeAttemptView
	Transitions []BridgeLegTransition
}

// BridgeLegPage is one page of legs, newest first.
type BridgeLegPage struct {
	Items      []BridgeLegView
	NextCursor string
}

// BridgeLegOutcome is a leg as a workflow call left it.
type BridgeLegOutcome struct {
	View    BridgeLegView
	Replay  bool
	Settled bool
}

// BridgeLegCommand asks to move units of a route's source asset across. The
// account facts are resolved by the adapter, never taken from the request.
type BridgeLegCommand struct {
	RouteID        bridge.RouteID
	AccountID      accounts.AccountID
	Quantity       money.Quantity
	Mode           gates.Mode
	IdempotencyKey string
	Actor          econ.Actor
	CorrelationID  string
}

// BridgeHolding is an account's WALLET balance of one SIMULATED asset.
type BridgeHolding struct {
	AssetID  assets.AssetID
	Label    StableAssetLabel
	Quantity money.Quantity
}

// BridgeChainHoldings are an account's holdings on one chain.
type BridgeChainHoldings struct {
	Chain    string
	Holdings []BridgeHolding
}

// BridgeHoldingsView is an account's simulated holdings by chain and the
// bridge's part of its buying power under the deployment's haircut policy.
type BridgeHoldingsView struct {
	AccountID  accounts.AccountID
	AsOf       time.Time
	Chains     []BridgeChainHoldings
	CrossChain bridge.CrossChainBuyingPower
	Labels     map[assets.AssetID]StableAssetLabel
}

// BridgeChainView is one chain of the host at one instant.
type BridgeChainView struct {
	Params          devnethost.ChainParams
	Tip             devnet.BlockInfo
	FinalizedHeight uint64
}

// BridgeChainsView is the host's network and both of its chains.
type BridgeChainsView struct {
	Network               string
	FaultInjectionAllowed bool
	Chains                []BridgeChainView
}
