package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
)

// The strategy marketplace (internal/marketplace) and the sandboxed runs
// behind it (internal/sandboxrt). Every figure behind this port is value
// domain SIMULATED, and every performance figure is a PAPER result.

// MarketplaceVersionSummary is the part of a listing version a list shows.
type MarketplaceVersionSummary struct {
	ID            marketplace.VersionID
	ListingID     marketplace.ListingID
	Number        int
	Status        marketplace.Status
	Moderation    marketplace.Moderation
	RiskClass     marketplace.RiskClass
	Description   string
	RejectionCode string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// MarketplaceListingSummary is a listing with the one version a list shows:
// the newest installable one in the catalogue, the newest of any status in an
// account's own list.
type MarketplaceListingSummary struct {
	Listing marketplace.Listing
	Latest  MarketplaceVersionSummary
}

// MarketplaceReview is one marketplace_reviews row.
type MarketplaceReview struct {
	Decision   marketplace.ReviewDecision
	ReasonCode string
	Note       string
	Reviewer   econ.Actor
	ReviewedAt time.Time
}

// MarketplaceEvaluation is a recorded simulation as read back.
type MarketplaceEvaluation struct {
	Evaluation     marketplace.Evaluation
	SnapshotsCount int
	CreatedAt      time.Time
}

// MarketplaceVersionView is one version with everything recorded about it.
type MarketplaceVersionView struct {
	Version     marketplace.Version
	Validation  *marketplace.Validation
	Reviews     []MarketplaceReview
	Provenance  marketplace.Provenance
	Evaluations []MarketplaceEvaluation
	// Performance holds the paper performance snapshots of installs of this
	// version, newest first.
	Performance []marketplace.PerformanceSnapshot
}

// MarketplaceListingView is a listing with its versions, newest first. The
// handler decides which of them a reader may see.
type MarketplaceListingView struct {
	Listing  marketplace.Listing
	Versions []MarketplaceVersionView
}

// MarketplaceQueueItem is a version awaiting validation or review.
type MarketplaceQueueItem struct {
	Listing marketplace.Listing
	Version MarketplaceVersionSummary
}

// MarketplaceInstallView is an install with the listing and version it
// installed and its latest paper performance snapshot, if any.
type MarketplaceInstallView struct {
	Install           marketplace.Install
	Listing           marketplace.Listing
	Version           MarketplaceVersionSummary
	LatestPerformance *marketplace.PerformanceSnapshot
}

// MarketplaceEffectState is what became of a dispatch effect.
type MarketplaceEffectState struct {
	Status      econ.EffectStatus
	FailureCode string
	ExternalRef string
}

// MarketplaceFollowView is a follow run with what became of every intent it
// produced: the Risk Kernel V2 decision and the dispatch effect, both read
// from the records the service wrote.
type MarketplaceFollowView struct {
	Run       marketplace.FollowRun
	Decisions map[econ.IntentID]risk.AdvancedDecisionRecord
	Effects   map[econ.EffectID]MarketplaceEffectState
}

// MarketplaceRunView is one sandbox run, why it ended and its violations.
type MarketplaceRunView struct {
	Run          sandboxrt.RunRecord
	FollowRunID  *marketplace.FollowRunID
	EvaluationID *marketplace.EvaluationID
	Violations   []sandboxrt.Violation
}

// MarketplaceCreateListing creates a listing with its first version.
type MarketplaceCreateListing struct {
	Account accounts.AccountID
	Actor   econ.Actor
	Title   string
	Version marketplace.VersionRequest
}

// MarketplaceNewVersion publishes the next version of a listing.
type MarketplaceNewVersion struct {
	ListingID marketplace.ListingID
	Actor     econ.Actor
	Version   marketplace.VersionRequest
}

// MarketplaceFork forks a version into a new listing of the account.
type MarketplaceFork struct {
	ParentID marketplace.VersionID
	Account  accounts.AccountID
	Actor    econ.Actor
	Title    string
	Version  marketplace.VersionRequest
}

// MarketplaceSimulate runs a built version against a fixed snapshot set.
type MarketplaceSimulate struct {
	VersionID      marketplace.VersionID
	Actor          econ.Actor
	DatasetVersion string
	Snapshots      []sandboxrt.Input
}

// MarketplaceInstallRequest installs a version for an account.
type MarketplaceInstallRequest struct {
	VersionID marketplace.VersionID
	Account   accounts.AccountID
	Mode      gates.Mode
	Actor     econ.Actor
}

// MarketplaceFollowRequest runs one explicit paper follow of an install.
type MarketplaceFollowRequest struct {
	InstallID     marketplace.InstallID
	Actor         econ.Actor
	Input         sandboxrt.Input
	CorrelationID string
}

// MarketplacePort serves the strategy marketplace. Reads return records as
// recorded; what a reader may see of them is the handler's decision. Every
// write runs one service operation in one transaction.
type MarketplacePort interface {
	// Catalog pages the listings that have an APPROVED, CLEARED version.
	Catalog(ctx context.Context, cursor string, limit int) ([]MarketplaceListingSummary, string, error)
	// AccountListings pages the listings an account created.
	AccountListings(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]MarketplaceListingSummary, string, error)
	// Listing returns a listing with every version.
	Listing(ctx context.Context, lid marketplace.ListingID) (MarketplaceListingView, error)
	// ListingRecord returns a listing without its versions.
	ListingRecord(ctx context.Context, lid marketplace.ListingID) (marketplace.Listing, error)
	// VersionRecord returns a version with its listing.
	VersionRecord(ctx context.Context, vid marketplace.VersionID) (marketplace.Listing, marketplace.Version, error)
	// ReviewQueue pages the versions in the given statuses, oldest first.
	ReviewQueue(ctx context.Context, statuses []marketplace.Status, cursor string, limit int) ([]MarketplaceQueueItem, string, error)
	// Installs pages an account's installs.
	Installs(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]MarketplaceInstallView, string, error)
	// InstallRecord returns one install.
	InstallRecord(ctx context.Context, iid marketplace.InstallID) (marketplace.Install, error)
	// Follows pages an account's follow runs, optionally of one install.
	Follows(ctx context.Context, account accounts.AccountID, install *marketplace.InstallID, cursor string, limit int) ([]MarketplaceFollowView, string, error)
	// Runs pages an account's sandbox runs: its follows, and the simulations of
	// its own listings.
	Runs(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]MarketplaceRunView, string, error)

	CreateListing(ctx context.Context, r MarketplaceCreateListing) (MarketplaceListingView, error)
	NewVersion(ctx context.Context, r MarketplaceNewVersion) (marketplace.Version, error)
	Fork(ctx context.Context, r MarketplaceFork) (MarketplaceListingView, error)
	Submit(ctx context.Context, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, error)
	Simulate(ctx context.Context, r MarketplaceSimulate) (MarketplaceEvaluation, error)
	Validate(ctx context.Context, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, marketplace.Validation, error)
	StartReview(ctx context.Context, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, error)
	Approve(ctx context.Context, vid marketplace.VersionID, actor econ.Actor, reasonCode, note string) (marketplace.Version, error)
	Reject(ctx context.Context, vid marketplace.VersionID, actor econ.Actor, reasonCode, note string) (marketplace.Version, error)
	Moderate(ctx context.Context, vid marketplace.VersionID, actor econ.Actor, to marketplace.Moderation, reason string) (marketplace.Version, error)
	Deprecate(ctx context.Context, vid marketplace.VersionID, actor econ.Actor, reason string) (marketplace.Version, error)
	// InstallVersion reports existed when the version was already installed.
	InstallVersion(ctx context.Context, r MarketplaceInstallRequest) (view MarketplaceInstallView, existed bool, err error)
	RemoveInstall(ctx context.Context, iid marketplace.InstallID, actor econ.Actor, reason string) (MarketplaceInstallView, error)
	Follow(ctx context.Context, r MarketplaceFollowRequest) (MarketplaceFollowView, error)
	RecordPerformance(ctx context.Context, iid marketplace.InstallID, actor econ.Actor) (marketplace.PerformanceSnapshot, error)
}
