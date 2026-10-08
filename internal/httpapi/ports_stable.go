package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/stableasset"
)

// StableAssetPort serves the sandbox stable asset (internal/stableasset;
// ADR-0020): a platform stable asset that exists only as SIMULATED value on a
// simulated chain, issued against a fully reserved policy. Every figure behind
// it is value domain SIMULATED.
//
// The stable asset service decides everything. Issue and Redeem run the
// service's own multi-transaction workflows (they own their transactions and
// commit a refusal before returning it); operator commands run one service
// operation in one transaction; reads present the service's own answers
// (Get, ReserveLedger, Snapshots, GetMint, GetBurn) and, where the package
// exports no listing, project its tables read-only.
type StableAssetPort interface {
	// Assets lists the registered stable assets, oldest first.
	Assets(ctx context.Context, limit int) ([]StableAssetView, error)
	// Asset returns one stable asset with its status history and its newest
	// peg-monitor alerts.
	Asset(ctx context.Context, id stableasset.StableAssetID) (StableAssetDetailView, error)
	// Reserve returns the issuer's reserve ledger of one stable asset.
	Reserve(ctx context.Context, id stableasset.StableAssetID) (StableReserveView, error)
	// Snapshots returns the newest limit snapshots of the asset's chain and
	// the verification of the whole chain against the snapshot signer's key.
	Snapshots(ctx context.Context, id stableasset.StableAssetID, limit int) (StableSnapshotChainView, error)

	// Holdings returns an account's holdings of every stable asset and of
	// every reserve asset an issuance may be paid in.
	Holdings(ctx context.Context, account accounts.AccountID) (StableHoldingsView, error)
	Issuances(ctx context.Context, account accounts.AccountID, asset *stableasset.StableAssetID, cursor string, limit int) (StableIssuancePage, error)
	// Issuance returns one mint request; the caller scopes it by the
	// request's own account.
	Issuance(ctx context.Context, id stableasset.MintRequestID) (StableIssuanceView, error)
	Redemptions(ctx context.Context, account accounts.AccountID, asset *stableasset.StableAssetID, cursor string, limit int) (StableRedemptionPage, error)
	// Redemption returns one burn request; the caller scopes it by the
	// request's own account.
	Redemption(ctx context.Context, id stableasset.BurnRequestID) (StableRedemptionView, error)

	// Issue mints for the account (stableasset.Service.Mint). A submission
	// whose outcome is unknown is not an error here: the request is returned
	// in SUBMISSION_UNKNOWN, not settled, with its reconciliation state.
	Issue(ctx context.Context, c StableIssueCommand) (StableIssuanceOutcome, error)
	// Redeem burns for the account (stableasset.Service.Burn), with the same
	// treatment of an unknown outcome.
	Redeem(ctx context.Context, c StableRedeemCommand) (StableRedemptionOutcome, error)

	Register(ctx context.Context, def stableasset.Definition, by econ.Actor) (StableAssetDetailView, error)
	SetStatus(ctx context.Context, id stableasset.StableAssetID, to stableasset.Status, by econ.Actor, reason string) (StableAssetDetailView, error)
	RunMonitor(ctx context.Context, id stableasset.StableAssetID, by econ.Actor) (StableMonitorView, error)
	ReconcileSupply(ctx context.Context, id stableasset.StableAssetID, by econ.Actor) (stableasset.SupplyReconciliation, error)
	TakeSnapshot(ctx context.Context, id stableasset.StableAssetID) (StableSnapshotTakenView, error)
	ProcessRedemptions(ctx context.Context, id stableasset.StableAssetID, limit int) (stableasset.SettleReport, error)
	// ResolveUnknown settles every request whose submission outcome is
	// unknown from the chain's status by transaction id; nothing is
	// resubmitted.
	ResolveUnknown(ctx context.Context, limit int) (stableasset.SettleReport, error)
}

// StableIdentityReader answers whether an account's owner is identity
// verified for simulated activity (DECISION_REGISTER D-055).
// compliance.AccountVerification implements it; the bridge reads identity
// through the same reader, so the two can never disagree.
type StableIdentityReader interface {
	IdentityVerified(ctx context.Context, q db.Querier, account accounts.AccountID) (bool, error)
}

// StableAssetLabel names an asset the way a figure in it is rendered: its
// symbol and the decimals of its base units.
type StableAssetLabel struct {
	Symbol   string
	Decimals uint8
}

// StableAssetView is a stable asset with its asset row's symbol and peg
// currency, and the labels of every reserve asset of its policy.
type StableAssetView struct {
	Asset       stableasset.PlatformStableAsset
	Label       StableAssetLabel
	PegCurrency string
	Reserves    map[assets.AssetID]StableAssetLabel
}

// StableAssetTransition is one status change of a stable asset.
type StableAssetTransition struct {
	From       string
	To         stableasset.Status
	Actor      econ.Actor
	Reason     string
	OccurredAt time.Time
}

// StableAlertView is one peg-monitor finding, as the monitor recorded it.
type StableAlertView struct {
	ID             id.ID[id.Any]
	Kind           stableasset.AlertKind
	Severity       stableasset.Severity
	BreakerTripped bool
	Observed       json.RawMessage
	ObservedAt     time.Time
}

// StableAssetDetailView is a stable asset with its whole status history,
// oldest first, and its newest alerts, newest first.
type StableAssetDetailView struct {
	StableAssetView
	Transitions []StableAssetTransition
	Alerts      []StableAlertView
}

// StableReserveView is the reserve ledger of one asset with the labels of
// every asset it names.
type StableReserveView struct {
	Asset  StableAssetView
	Ledger stableasset.ReserveLedger
	Labels map[assets.AssetID]StableAssetLabel
}

// StableSnapshotVerification is the verification of a whole snapshot chain
// (stableasset.VerifySnapshotChain against the snapshot signer's key).
// FailedIndex is -1 when the chain verifies.
type StableSnapshotVerification struct {
	Checked     int
	FailedIndex int
	Reason      string
	PublicKey   ed25519.PublicKey
}

// Verified reports that every snapshot of the chain verifies.
func (v StableSnapshotVerification) Verified() bool { return v.FailedIndex < 0 }

// VerifiedAt reports whether the snapshot at position index (from 0, oldest
// first) and every snapshot before it verify: the chain is checked in order
// and the first failure stops it, so every snapshot before that failure
// passed every check.
func (v StableSnapshotVerification) VerifiedAt(index int) bool {
	return v.FailedIndex < 0 || index < v.FailedIndex
}

// StableSnapshotChainView is the newest part of a snapshot chain. Offset is
// the position of Items[0] in the whole chain.
type StableSnapshotChainView struct {
	StableAssetID stableasset.StableAssetID
	Total         int
	Offset        int
	Items         []stableasset.SignedSnapshot
	Verification  StableSnapshotVerification
}

// StableSnapshotTakenView is a snapshot just recorded, its position in the
// chain and the chain's verification after it.
type StableSnapshotTakenView struct {
	Snapshot     stableasset.SignedSnapshot
	Index        int
	Verification StableSnapshotVerification
}

// StableRequestReconciliation is what the economic effect of a mint or burn
// says about an unknown outcome: its status now, when (if ever) it became
// UNKNOWN, and when it was settled from the chain.
type StableRequestReconciliation struct {
	EffectStatus econ.EffectStatus
	UnknownSince *time.Time
	ResolvedAt   *time.Time
}

// StableIssuanceView is a mint request with the labels of the stable asset
// and of the reserve asset that paid.
type StableIssuanceView struct {
	Record         stableasset.MintRecord
	Stable         StableAssetLabel
	Reserve        StableAssetLabel
	Reconciliation StableRequestReconciliation
}

// StableRedemptionView is a burn request with the labels of the stable asset
// and of the asset it pays out.
type StableRedemptionView struct {
	Record         stableasset.BurnRecord
	Stable         StableAssetLabel
	Payout         StableAssetLabel
	Reconciliation StableRequestReconciliation
}

// StableIssuancePage is one page of mint requests, newest first.
type StableIssuancePage struct {
	Items      []StableIssuanceView
	NextCursor string
}

// StableRedemptionPage is one page of burn requests, newest first.
type StableRedemptionPage struct {
	Items      []StableRedemptionView
	NextCursor string
}

// StableIssuanceOutcome is a mint request as Issue left it.
type StableIssuanceOutcome struct {
	View    StableIssuanceView
	Replay  bool
	Settled bool
}

// StableRedemptionOutcome is a burn request as Redeem left it.
type StableRedemptionOutcome struct {
	View    StableRedemptionView
	Replay  bool
	Settled bool
}

// StableIssueCommand asks to mint for an account. The account facts the
// settlement compiler and Risk Kernel V2 read are resolved by the adapter
// from the deployment's resolvers, never from the request.
type StableIssueCommand struct {
	StableAssetID  stableasset.StableAssetID
	AccountID      accounts.AccountID
	Quantity       money.Quantity
	ReserveAssetID assets.AssetID
	Mode           gates.Mode
	IdempotencyKey string
	Actor          econ.Actor
	CorrelationID  string
}

// StableRedeemCommand asks to burn for an account.
type StableRedeemCommand struct {
	StableAssetID  stableasset.StableAssetID
	AccountID      accounts.AccountID
	Quantity       money.Quantity
	Mode           gates.Mode
	IdempotencyKey string
	Actor          econ.Actor
	CorrelationID  string
}

// StableHoldingView is an account's holding of one stable asset: the capital
// service's availability arithmetic over its WALLET balance, and the balance
// of its custodied wallet on the simulated chain.
type StableHoldingView struct {
	Asset        stableasset.PlatformStableAsset
	Label        StableAssetLabel
	Availability capital.Availability
	ChainBalance money.Quantity
}

// StablePaymentView is an account's availability of one reserve asset an
// issuance may be paid in.
type StablePaymentView struct {
	AssetID      assets.AssetID
	Label        StableAssetLabel
	Availability capital.Availability
}

// StableHoldingsView is an account's stable asset holdings.
type StableHoldingsView struct {
	AccountID     accounts.AccountID
	HolderAddress string
	AsOf          time.Time
	Items         []StableHoldingView
	Payment       []StablePaymentView
}

// StableMonitorView is one peg-monitor run with what its report is rendered
// against.
type StableMonitorView struct {
	Report stableasset.MonitorReport
	Asset  StableAssetView
	Labels map[assets.AssetID]StableAssetLabel
}
