package httpapi

import (
	"context"
	"encoding/hex"
	"net/http"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
)

// The strategy marketplace (internal/marketplace) and the sandboxed runs
// behind it (ADR-0020). These handlers translate shapes and decide what a
// reader may see of a record; every rule -- the lifecycle, dual control,
// PAPER-only installs, the manifest, Risk V2 on every proposed intent -- is
// the services'.
//
// What a reader sees: a version is PUBLIC once it passed review (APPROVED, or
// DEPRECATED after it) and was not taken down. The creator's account and any
// operator who may read every account see all of a listing; anyone else sees
// its public versions only, and a listing with none answers exactly the
// NOT_FOUND an absent id gets. Account ids and the people behind a listing or
// a review are shown to the same audience only.

// --- reads ------------------------------------------------------------------

// GetAdvancedStrategiesCatalog pages the approved listings.
func (s *Server) GetAdvancedStrategiesCatalog(ctx context.Context, request api.GetAdvancedStrategiesCatalogRequestObject) (api.GetAdvancedStrategiesCatalogResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	items, next, err := port.Catalog(ctx, deref(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.StrategyListingPage{Items: make([]api.StrategyListing, 0, len(items)), NextCursor: nextCursor(next)}
	for _, it := range items {
		page.Items = append(page.Items, toAPIStrategyListing(it, marketplaceSeesAll(ctx, it.Listing)))
	}
	return api.GetAdvancedStrategiesCatalog200JSONResponse(page), nil
}

// GetAdvancedStrategiesListings pages the listings an account created.
func (s *Server) GetAdvancedStrategiesListings(ctx context.Context, request api.GetAdvancedStrategiesListingsRequestObject) (api.GetAdvancedStrategiesListingsResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	items, next, err := port.AccountListings(ctx, account, deref(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.StrategyListingPage{Items: make([]api.StrategyListing, 0, len(items)), NextCursor: nextCursor(next)}
	for _, it := range items {
		page.Items = append(page.Items, toAPIStrategyListing(it, marketplaceSeesAll(ctx, it.Listing)))
	}
	return api.GetAdvancedStrategiesListings200JSONResponse(page), nil
}

// GetAdvancedStrategiesListingsListingId returns one listing to the audience
// that may see it.
func (s *Server) GetAdvancedStrategiesListingsListingId(ctx context.Context, request api.GetAdvancedStrategiesListingsListingIdRequestObject) (api.GetAdvancedStrategiesListingsListingIdResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	lid, err := marketplaceParseID[marketplace.ListingID](request.ListingId, "listingId")
	if err != nil {
		return nil, err
	}
	view, err := port.Listing(ctx, lid)
	if err != nil {
		return nil, err
	}
	all := marketplaceSeesAll(ctx, view.Listing)
	out, err := toAPIStrategyListingDetail(view, all)
	if err != nil {
		return nil, err
	}
	if !all && len(out.Versions) == 0 {
		// NOT_FOUND, never FORBIDDEN, and the very answer an absent id gets.
		return nil, marketplaceListingNotFound(lid)
	}
	return api.GetAdvancedStrategiesListingsListingId200JSONResponse(out), nil
}

// GetAdvancedStrategiesReviewQueue pages the versions awaiting an operator.
func (s *Server) GetAdvancedStrategiesReviewQueue(ctx context.Context, request api.GetAdvancedStrategiesReviewQueueRequestObject) (api.GetAdvancedStrategiesReviewQueueResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	statuses := []marketplace.Status{marketplace.StatusSubmitted, marketplace.StatusValidated, marketplace.StatusInReview}
	if request.Params.Status != nil {
		st := marketplace.Status(*request.Params.Status)
		if !st.Valid() {
			return nil, validationError("status", "unknown version status")
		}
		statuses = []marketplace.Status{st}
	}
	items, next, err := port.ReviewQueue(ctx, statuses, deref(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.StrategyReviewQueuePage{Items: make([]api.StrategyReviewQueueItem, 0, len(items)), NextCursor: nextCursor(next)}
	for _, it := range items {
		page.Items = append(page.Items, api.StrategyReviewQueueItem{
			ListingId: toUUID(it.Listing.ID), ListingTitle: it.Listing.Title, CreatorAccountId: toUUID(it.Listing.CreatorAccountID),
			Version: toAPIStrategyVersionSummary(it.Version),
		})
	}
	return api.GetAdvancedStrategiesReviewQueue200JSONResponse(page), nil
}

// GetAdvancedStrategiesInstalls pages an account's installs.
func (s *Server) GetAdvancedStrategiesInstalls(ctx context.Context, request api.GetAdvancedStrategiesInstallsRequestObject) (api.GetAdvancedStrategiesInstallsResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	items, next, err := port.Installs(ctx, account, deref(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.StrategyInstallPage{ValueDomain: api.SIMULATED, Items: make([]api.StrategyInstall, 0, len(items)), NextCursor: nextCursor(next)}
	for _, it := range items {
		page.Items = append(page.Items, toAPIStrategyInstall(it))
	}
	return api.GetAdvancedStrategiesInstalls200JSONResponse(page), nil
}

// GetAdvancedStrategiesFollows pages an account's follow runs with the intents
// each produced and their Risk V2 decisions.
func (s *Server) GetAdvancedStrategiesFollows(ctx context.Context, request api.GetAdvancedStrategiesFollowsRequestObject) (api.GetAdvancedStrategiesFollowsResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	var install *marketplace.InstallID
	if request.Params.InstallId != nil {
		iid, err := marketplaceParseID[marketplace.InstallID](*request.Params.InstallId, "install_id")
		if err != nil {
			return nil, err
		}
		install = &iid
	}
	items, next, err := port.Follows(ctx, account, install, deref(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.StrategyFollowRunPage{ValueDomain: api.SIMULATED, Items: make([]api.StrategyFollowRun, 0, len(items)), NextCursor: nextCursor(next)}
	for _, it := range items {
		page.Items = append(page.Items, toAPIStrategyFollowRun(it, nil))
	}
	return api.GetAdvancedStrategiesFollows200JSONResponse(page), nil
}

// GetAdvancedStrategiesRuns pages an account's sandbox runs.
func (s *Server) GetAdvancedStrategiesRuns(ctx context.Context, request api.GetAdvancedStrategiesRunsRequestObject) (api.GetAdvancedStrategiesRunsResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	items, next, err := port.Runs(ctx, account, deref(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, marketplaceMaxRunPage))
	if err != nil {
		return nil, err
	}
	page := api.StrategySandboxRunPage{Items: make([]api.StrategySandboxRun, 0, len(items)), NextCursor: nextCursor(next)}
	for _, it := range items {
		page.Items = append(page.Items, toAPIStrategySandboxRun(it))
	}
	return api.GetAdvancedStrategiesRuns200JSONResponse(page), nil
}

// marketplaceMaxRunPage bounds a page of sandbox runs: each carries its
// violations, read run by run.
const marketplaceMaxRunPage = 50

// --- customer writes ----------------------------------------------------------

// PostAdvancedStrategiesListings creates a listing with its first version.
func (s *Server) PostAdvancedStrategiesListings(ctx context.Context, request api.PostAdvancedStrategiesListingsRequestObject) (api.PostAdvancedStrategiesListingsResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	actor, err := marketplaceActor(ctx)
	if err != nil {
		return nil, err
	}
	vr, err := marketplaceVersionRequest(request.Body.Version)
	if err != nil {
		return nil, err
	}
	cmd := MarketplaceCreateListing{Account: account, Actor: actor, Title: request.Body.Title, Version: vr}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StrategyListingDetail, commandMeta, error) {
			view, serr := port.CreateListing(ctx, cmd)
			if serr != nil {
				return api.StrategyListingDetail{}, commandMeta{}, serr
			}
			out, cerr := toAPIStrategyListingDetail(view, true)
			if cerr != nil {
				return api.StrategyListingDetail{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusCreated, ResourceType: "marketplace_listing", ResourceID: view.Listing.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostAdvancedStrategiesListings200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedStrategiesListings201JSONResponse(res.Value), nil
}

// PostAdvancedStrategiesListingsListingIdVersions publishes the next version
// of one of the account's listings.
func (s *Server) PostAdvancedStrategiesListingsListingIdVersions(ctx context.Context, request api.PostAdvancedStrategiesListingsListingIdVersionsRequestObject) (api.PostAdvancedStrategiesListingsListingIdVersionsResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	lid, err := marketplaceParseID[marketplace.ListingID](request.ListingId, "listingId")
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	actor, err := marketplaceActor(ctx)
	if err != nil {
		return nil, err
	}
	vr, err := marketplaceVersionRequest(request.Body.Version)
	if err != nil {
		return nil, err
	}
	l, err := port.ListingRecord(ctx, lid)
	if err != nil {
		return nil, err
	}
	if l.CreatorAccountID != account {
		// A listing of another account is not one of this account's.
		return nil, marketplaceListingNotFound(lid)
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StrategyVersion, commandMeta, error) {
			v, serr := port.NewVersion(ctx, MarketplaceNewVersion{ListingID: lid, Actor: actor, Version: vr})
			if serr != nil {
				return api.StrategyVersion{}, commandMeta{}, serr
			}
			out, cerr := toAPIStrategyVersion(v, true)
			if cerr != nil {
				return api.StrategyVersion{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusCreated, ResourceType: "marketplace_listing_version", ResourceID: v.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostAdvancedStrategiesListingsListingIdVersions200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedStrategiesListingsListingIdVersions201JSONResponse(res.Value), nil
}

// PostAdvancedStrategiesVersionsVersionIdForks forks a version the caller may
// see into a new listing of the caller's account.
func (s *Server) PostAdvancedStrategiesVersionsVersionIdForks(ctx context.Context, request api.PostAdvancedStrategiesVersionsVersionIdForksRequestObject) (api.PostAdvancedStrategiesVersionsVersionIdForksResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	vid, err := marketplaceParseID[marketplace.VersionID](request.VersionId, "versionId")
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	actor, err := marketplaceActor(ctx)
	if err != nil {
		return nil, err
	}
	vr, err := marketplaceVersionRequest(request.Body.Version)
	if err != nil {
		return nil, err
	}
	l, v, err := port.VersionRecord(ctx, vid)
	if err != nil {
		return nil, err
	}
	if !marketplaceSeesAll(ctx, l) && !marketplacePublic(v.Status, v.Moderation) {
		return nil, marketplaceVersionNotFound(vid)
	}
	cmd := MarketplaceFork{ParentID: vid, Account: account, Actor: actor, Title: request.Body.Title, Version: vr}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StrategyListingDetail, commandMeta, error) {
			view, serr := port.Fork(ctx, cmd)
			if serr != nil {
				return api.StrategyListingDetail{}, commandMeta{}, serr
			}
			out, cerr := toAPIStrategyListingDetail(view, true)
			if cerr != nil {
				return api.StrategyListingDetail{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusCreated, ResourceType: "marketplace_listing", ResourceID: view.Listing.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostAdvancedStrategiesVersionsVersionIdForks200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedStrategiesVersionsVersionIdForks201JSONResponse(res.Value), nil
}

// PostAdvancedStrategiesVersionsVersionIdSubmit submits one of the account's
// DRAFT versions for validation and review.
func (s *Server) PostAdvancedStrategiesVersionsVersionIdSubmit(ctx context.Context, request api.PostAdvancedStrategiesVersionsVersionIdSubmitRequestObject) (api.PostAdvancedStrategiesVersionsVersionIdSubmitResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	vid, err := marketplaceParseID[marketplace.VersionID](request.VersionId, "versionId")
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	actor, err := marketplaceActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := marketplaceOwnVersion(ctx, port, vid, account); err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StrategyVersion, commandMeta, error) {
			v, serr := port.Submit(ctx, vid, actor)
			if serr != nil {
				return api.StrategyVersion{}, commandMeta{}, serr
			}
			out, cerr := toAPIStrategyVersion(v, true)
			if cerr != nil {
				return api.StrategyVersion{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "marketplace_listing_version", ResourceID: v.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStrategiesVersionsVersionIdSubmit200JSONResponse(res.Value), nil
}

// marketplaceMaxSimulationSnapshots bounds one simulation request: every
// snapshot is a sandbox run inside the request's transaction, so a larger set
// is a worker's job, not a request's.
const marketplaceMaxSimulationSnapshots = 20

// PostAdvancedStrategiesVersionsVersionIdSimulations runs one of the account's
// built versions against a fixed snapshot set.
func (s *Server) PostAdvancedStrategiesVersionsVersionIdSimulations(ctx context.Context, request api.PostAdvancedStrategiesVersionsVersionIdSimulationsRequestObject) (api.PostAdvancedStrategiesVersionsVersionIdSimulationsResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	vid, err := marketplaceParseID[marketplace.VersionID](request.VersionId, "versionId")
	if err != nil {
		return nil, err
	}
	b := request.Body
	if b == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, b.AccountId)
	if err != nil {
		return nil, err
	}
	actor, err := marketplaceActor(ctx)
	if err != nil {
		return nil, err
	}
	if len(b.Snapshots) == 0 || len(b.Snapshots) > marketplaceMaxSimulationSnapshots {
		return nil, validationError("snapshots", "a simulation request carries 1 to 20 snapshots")
	}
	snapshots := make([]sandboxrt.Input, 0, len(b.Snapshots))
	for _, snap := range b.Snapshots {
		in, err := marketplaceInput(snap)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, in)
	}
	if err := marketplaceOwnVersion(ctx, port, vid, account); err != nil {
		return nil, err
	}
	cmd := MarketplaceSimulate{VersionID: vid, Actor: actor, DatasetVersion: b.DatasetVersion, Snapshots: snapshots}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StrategyEvaluation, commandMeta, error) {
			ev, serr := port.Simulate(ctx, cmd)
			if serr != nil {
				return api.StrategyEvaluation{}, commandMeta{}, serr
			}
			return toAPIStrategyEvaluation(ev, true), commandMeta{Status: http.StatusCreated, ResourceType: "marketplace_evaluation",
				ResourceID: ev.Evaluation.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostAdvancedStrategiesVersionsVersionIdSimulations200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedStrategiesVersionsVersionIdSimulations201JSONResponse(res.Value), nil
}

// PostAdvancedStrategiesInstalls installs a version for one of the caller's
// accounts. The mode is passed through as asked: anything but PAPER is the
// service's refusal to give, not this handler's to rewrite.
func (s *Server) PostAdvancedStrategiesInstalls(ctx context.Context, request api.PostAdvancedStrategiesInstallsRequestObject) (api.PostAdvancedStrategiesInstallsResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	b := request.Body
	if b == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, b.AccountId)
	if err != nil {
		return nil, err
	}
	actor, err := marketplaceActor(ctx)
	if err != nil {
		return nil, err
	}
	vid, err := marketplaceParseID[marketplace.VersionID](b.VersionId, "version_id")
	if err != nil {
		return nil, err
	}
	mode := gates.Mode(b.Mode)
	if !b.Mode.Valid() {
		return nil, validationError("mode", "mode must be one of PAPER, SANDBOX, DEVNET or LIVE")
	}
	l, v, err := port.VersionRecord(ctx, vid)
	if err != nil {
		return nil, err
	}
	if !marketplaceSeesAll(ctx, l) && !marketplacePublic(v.Status, v.Moderation) {
		return nil, marketplaceVersionNotFound(vid)
	}
	cmd := MarketplaceInstallRequest{VersionID: vid, Account: account, Mode: mode, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StrategyInstall, commandMeta, error) {
			view, existed, serr := port.InstallVersion(ctx, cmd)
			if serr != nil {
				return api.StrategyInstall{}, commandMeta{}, serr
			}
			status := http.StatusCreated
			if existed {
				status = http.StatusOK
			}
			return toAPIStrategyInstall(view), commandMeta{Status: status, ResourceType: "marketplace_install", ResourceID: view.Install.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed || res.Meta.Status == http.StatusOK {
		return api.PostAdvancedStrategiesInstalls200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedStrategiesInstalls201JSONResponse(res.Value), nil
}

// PostAdvancedStrategiesInstallsInstallIdRemove removes one of the account's
// ACTIVE installs.
func (s *Server) PostAdvancedStrategiesInstallsInstallIdRemove(ctx context.Context, request api.PostAdvancedStrategiesInstallsInstallIdRemoveRequestObject) (api.PostAdvancedStrategiesInstallsInstallIdRemoveResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	iid, err := marketplaceParseID[marketplace.InstallID](request.InstallId, "installId")
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	actor, err := marketplaceActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := marketplaceOwnInstall(ctx, port, iid, account); err != nil {
		return nil, err
	}
	reason := deref(request.Body.Reason)
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StrategyInstall, commandMeta, error) {
			view, serr := port.RemoveInstall(ctx, iid, actor, reason)
			if serr != nil {
				return api.StrategyInstall{}, commandMeta{}, serr
			}
			return toAPIStrategyInstall(view), commandMeta{Status: http.StatusOK, ResourceType: "marketplace_install", ResourceID: iid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStrategiesInstallsInstallIdRemove200JSONResponse(res.Value), nil
}

// PostAdvancedStrategiesInstallsInstallIdFollows paper-follows one of the
// account's installs on the snapshot the caller supplies.
func (s *Server) PostAdvancedStrategiesInstallsInstallIdFollows(ctx context.Context, request api.PostAdvancedStrategiesInstallsInstallIdFollowsRequestObject) (api.PostAdvancedStrategiesInstallsInstallIdFollowsResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	iid, err := marketplaceParseID[marketplace.InstallID](request.InstallId, "installId")
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	actor, err := marketplaceActor(ctx)
	if err != nil {
		return nil, err
	}
	input, err := marketplaceInput(request.Body.Snapshot)
	if err != nil {
		return nil, err
	}
	if err := marketplaceOwnInstall(ctx, port, iid, account); err != nil {
		return nil, err
	}
	cmd := MarketplaceFollowRequest{InstallID: iid, Actor: actor, Input: input, CorrelationID: observability.CorrelationID(ctx)}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StrategyFollowRun, commandMeta, error) {
			view, serr := port.Follow(ctx, cmd)
			if serr != nil {
				return api.StrategyFollowRun{}, commandMeta{}, serr
			}
			replayed := view.Run.Replayed
			status := http.StatusCreated
			if replayed {
				// This snapshot time was already followed: nothing ran again.
				status = http.StatusOK
			}
			return toAPIStrategyFollowRun(view, &replayed), commandMeta{Status: status, ResourceType: "marketplace_follow_run",
				ResourceID: view.Run.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed || res.Meta.Status == http.StatusOK {
		return api.PostAdvancedStrategiesInstallsInstallIdFollows200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedStrategiesInstallsInstallIdFollows201JSONResponse(res.Value), nil
}

// PostAdvancedStrategiesInstallsInstallIdPerformance records the paper
// performance of one of the account's installs.
func (s *Server) PostAdvancedStrategiesInstallsInstallIdPerformance(ctx context.Context, request api.PostAdvancedStrategiesInstallsInstallIdPerformanceRequestObject) (api.PostAdvancedStrategiesInstallsInstallIdPerformanceResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	iid, err := marketplaceParseID[marketplace.InstallID](request.InstallId, "installId")
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	actor, err := marketplaceActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := marketplaceOwnInstall(ctx, port, iid, account); err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StrategyPerformanceSnapshot, commandMeta, error) {
			snap, serr := port.RecordPerformance(ctx, iid, actor)
			if serr != nil {
				return api.StrategyPerformanceSnapshot{}, commandMeta{}, serr
			}
			return toAPIStrategyPerformance(snap, true), commandMeta{Status: http.StatusCreated, ResourceType: "marketplace_performance_snapshot",
				ResourceID: iid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostAdvancedStrategiesInstallsInstallIdPerformance200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedStrategiesInstallsInstallIdPerformance201JSONResponse(res.Value), nil
}

// --- operator decisions -----------------------------------------------------
//
// Operators reach every version, so nothing is masked here. Dual control is
// the service's: it refuses a reviewer who created the listing or authored
// the version, and the database refuses such a review row again (MK002).

// PostAdvancedStrategiesVersionsVersionIdValidate scans and builds a
// SUBMITTED version.
func (s *Server) PostAdvancedStrategiesVersionsVersionIdValidate(ctx context.Context, request api.PostAdvancedStrategiesVersionsVersionIdValidateRequestObject) (api.PostAdvancedStrategiesVersionsVersionIdValidateResponseObject, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return nil, errNotWired("the strategy marketplace")
	}
	vid, err := marketplaceParseID[marketplace.VersionID](request.VersionId, "versionId")
	if err != nil {
		return nil, err
	}
	actor, err := marketplaceActor(ctx)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StrategyValidationResult, commandMeta, error) {
			v, val, serr := port.Validate(ctx, vid, actor)
			if serr != nil {
				return api.StrategyValidationResult{}, commandMeta{}, serr
			}
			version, cerr := toAPIStrategyVersion(v, marketplaceOperatorSeesAll(ctx))
			if cerr != nil {
				return api.StrategyValidationResult{}, commandMeta{}, cerr
			}
			return api.StrategyValidationResult{ValueDomain: api.SIMULATED, Version: version, Validation: toAPIStrategyValidation(val)},
				commandMeta{Status: http.StatusOK, ResourceType: "marketplace_listing_version", ResourceID: vid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStrategiesVersionsVersionIdValidate200JSONResponse(res.Value), nil
}

// PostAdvancedStrategiesVersionsVersionIdReview takes a VALIDATED version into
// review.
func (s *Server) PostAdvancedStrategiesVersionsVersionIdReview(ctx context.Context, request api.PostAdvancedStrategiesVersionsVersionIdReviewRequestObject) (api.PostAdvancedStrategiesVersionsVersionIdReviewResponseObject, error) {
	out, err := s.marketplaceDecision(ctx, request.VersionId, request.Params.IdempotencyKey,
		func(ctx context.Context, port MarketplacePort, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, error) {
			return port.StartReview(ctx, vid, actor)
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStrategiesVersionsVersionIdReview200JSONResponse(out), nil
}

// PostAdvancedStrategiesVersionsVersionIdApprove approves a version IN_REVIEW.
func (s *Server) PostAdvancedStrategiesVersionsVersionIdApprove(ctx context.Context, request api.PostAdvancedStrategiesVersionsVersionIdApproveRequestObject) (api.PostAdvancedStrategiesVersionsVersionIdApproveResponseObject, error) {
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	code, note := request.Body.ReasonCode, deref(request.Body.Note)
	out, err := s.marketplaceDecision(ctx, request.VersionId, request.Params.IdempotencyKey,
		func(ctx context.Context, port MarketplacePort, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, error) {
			return port.Approve(ctx, vid, actor, code, note)
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStrategiesVersionsVersionIdApprove200JSONResponse(out), nil
}

// PostAdvancedStrategiesVersionsVersionIdReject rejects a version.
func (s *Server) PostAdvancedStrategiesVersionsVersionIdReject(ctx context.Context, request api.PostAdvancedStrategiesVersionsVersionIdRejectRequestObject) (api.PostAdvancedStrategiesVersionsVersionIdRejectResponseObject, error) {
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	code, note := request.Body.ReasonCode, deref(request.Body.Note)
	out, err := s.marketplaceDecision(ctx, request.VersionId, request.Params.IdempotencyKey,
		func(ctx context.Context, port MarketplacePort, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, error) {
			return port.Reject(ctx, vid, actor, code, note)
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStrategiesVersionsVersionIdReject200JSONResponse(out), nil
}

// PostAdvancedStrategiesVersionsVersionIdModeration flags, takes down or
// re-clears a version.
func (s *Server) PostAdvancedStrategiesVersionsVersionIdModeration(ctx context.Context, request api.PostAdvancedStrategiesVersionsVersionIdModerationRequestObject) (api.PostAdvancedStrategiesVersionsVersionIdModerationResponseObject, error) {
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	to := marketplace.Moderation(request.Body.To)
	if !request.Body.To.Valid() {
		return nil, validationError("to", "to must be UNREVIEWED, CLEARED, FLAGGED or TAKEN_DOWN")
	}
	reason := request.Body.Reason
	out, err := s.marketplaceDecision(ctx, request.VersionId, request.Params.IdempotencyKey,
		func(ctx context.Context, port MarketplacePort, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, error) {
			return port.Moderate(ctx, vid, actor, to, reason)
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStrategiesVersionsVersionIdModeration200JSONResponse(out), nil
}

// PostAdvancedStrategiesVersionsVersionIdDeprecate retires an APPROVED version.
func (s *Server) PostAdvancedStrategiesVersionsVersionIdDeprecate(ctx context.Context, request api.PostAdvancedStrategiesVersionsVersionIdDeprecateRequestObject) (api.PostAdvancedStrategiesVersionsVersionIdDeprecateResponseObject, error) {
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	reason := request.Body.Reason
	out, err := s.marketplaceDecision(ctx, request.VersionId, request.Params.IdempotencyKey,
		func(ctx context.Context, port MarketplacePort, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, error) {
			return port.Deprecate(ctx, vid, actor, reason)
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStrategiesVersionsVersionIdDeprecate200JSONResponse(out), nil
}

// marketplaceDecision runs one operator decision on a version under the
// idempotency contract.
func (s *Server) marketplaceDecision(ctx context.Context, versionID api.UUID, key string,
	op func(ctx context.Context, port MarketplacePort, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, error)) (api.StrategyVersion, error) {
	port := s.opts.Ports.Marketplace
	if port == nil {
		return api.StrategyVersion{}, errNotWired("the strategy marketplace")
	}
	vid, err := marketplaceParseID[marketplace.VersionID](versionID, "versionId")
	if err != nil {
		return api.StrategyVersion{}, err
	}
	actor, err := marketplaceActor(ctx)
	if err != nil {
		return api.StrategyVersion{}, err
	}
	res, err := runCommand(ctx, s, key, func(ctx context.Context) (api.StrategyVersion, commandMeta, error) {
		v, serr := op(ctx, port, vid, actor)
		if serr != nil {
			return api.StrategyVersion{}, commandMeta{}, serr
		}
		out, cerr := toAPIStrategyVersion(v, marketplaceOperatorSeesAll(ctx))
		if cerr != nil {
			return api.StrategyVersion{}, commandMeta{}, cerr
		}
		return out, commandMeta{Status: http.StatusOK, ResourceType: "marketplace_listing_version", ResourceID: vid.String()}, nil
	})
	if err != nil {
		return api.StrategyVersion{}, err
	}
	return res.Value, nil
}

// --- scoping helpers ----------------------------------------------------------

// marketplaceActor is the principal as the marketplace records it.
func marketplaceActor(ctx context.Context) (econ.Actor, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return econ.Actor{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	return econ.Actor{Type: p.ActorType, ID: p.SubjectID}, nil
}

// marketplaceSeesAll reports whether the caller may see all of a listing: the
// owner of its creator's account, or an operator who may read any account.
func marketplaceSeesAll(ctx context.Context, l marketplace.Listing) bool {
	return security.RequireAccount(ctx, l.CreatorAccountID.String()) == nil
}

// marketplaceOperatorSeesAll is marketplaceSeesAll for an operator decision,
// where the version is in hand and its listing is not.
func marketplaceOperatorSeesAll(ctx context.Context) bool {
	return security.Require(ctx, security.PermAccountReadAny) == nil
}

// marketplacePublic reports whether a version is public: it passed review
// (APPROVED, or DEPRECATED after it) and was not taken down.
func marketplacePublic(st marketplace.Status, m marketplace.Moderation) bool {
	return (st == marketplace.StatusApproved || st == marketplace.StatusDeprecated) && m != marketplace.ModerationTakenDown
}

// The refusals below are byte-for-byte the ones the service gives an absent
// id, so a stranger cannot tell a record that exists from one that does not.

func marketplaceListingNotFound(lid marketplace.ListingID) error {
	return errs.New(errs.CodeNotFound, "marketplace: listing not found").WithField("listing_id", lid.String())
}

func marketplaceVersionNotFound(vid marketplace.VersionID) error {
	return errs.New(errs.CodeNotFound, "marketplace: listing version not found").WithField("version_id", vid.String())
}

func marketplaceInstallNotFound(iid marketplace.InstallID) error {
	return errs.New(errs.CodeNotFound, "marketplace: install not found").WithField("install_id", iid.String())
}

// marketplaceOwnVersion requires a version of a listing the account created:
// any other version is not one of the account's.
func marketplaceOwnVersion(ctx context.Context, port MarketplacePort, vid marketplace.VersionID, account accounts.AccountID) error {
	l, _, err := port.VersionRecord(ctx, vid)
	if err != nil {
		return err
	}
	if l.CreatorAccountID != account {
		return marketplaceVersionNotFound(vid)
	}
	return nil
}

// marketplaceOwnInstall requires an install of the account.
func marketplaceOwnInstall(ctx context.Context, port MarketplacePort, iid marketplace.InstallID, account accounts.AccountID) error {
	in, err := port.InstallRecord(ctx, iid)
	if err != nil {
		return err
	}
	if in.AccountID != account {
		return marketplaceInstallNotFound(iid)
	}
	return nil
}

// marketplaceTextID is a domain id whose kind is unexported in its package:
// it can only be parsed through its own UnmarshalText.
type marketplaceTextID[T any] interface {
	*T
	UnmarshalText(text []byte) error
	IsZero() bool
}

// marketplaceParseID parses a canonical UUIDv7 into a domain id.
func marketplaceParseID[T any, P marketplaceTextID[T]](u api.UUID, field string) (T, error) {
	var out T
	if err := P(&out).UnmarshalText([]byte(u.String())); err != nil || P(&out).IsZero() {
		var zero T
		return zero, validationError(field, field+" must be a canonical UUID")
	}
	return out, nil
}

// --- request shapes -------------------------------------------------------------

// marketplaceVersionRequest translates a version's content. The source is
// bounded here by the scanner's own limits -- that is the request's size
// bound -- and everything else is the service's to judge.
func marketplaceVersionRequest(in api.StrategyVersionInput) (marketplace.VersionRequest, error) {
	r := marketplace.VersionRequest{
		Description: in.Description, RiskClass: marketplace.RiskClass(in.RiskClass), SourceCommit: in.SourceCommit,
		BacktestEvidenceRef: deref(in.BacktestEvidenceRef),
	}
	if in.SupportedInstrumentIds != nil {
		for _, u := range *in.SupportedInstrumentIds {
			r.SupportedInstrumentIDs = append(r.SupportedInstrumentIDs, u.String())
		}
	}
	if in.DataDependencies != nil {
		r.DataDependencies = append([]string{}, *in.DataDependencies...)
	}
	if in.Fees != nil {
		r.Fees = marketplace.FeeModel{PerformanceFeeBPS: money.BPS(in.Fees.PerformanceFeeBps), ManagementFeeBPS: money.BPS(in.Fees.ManagementFeeBps),
			HighWaterMark: in.Fees.HighWaterMark}
	}
	if in.Config != nil {
		r.Config = make(map[string]string, len(*in.Config))
		for k, v := range *in.Config {
			r.Config[k] = v
		}
	}
	if in.Manifest != nil {
		r.Manifest = marketplaceManifest(*in.Manifest)
	}
	if in.Source != nil {
		limits := sandboxrt.DefaultScanLimits()
		if len(*in.Source) == 0 || len(*in.Source) > limits.MaxFiles {
			return marketplace.VersionRequest{}, validationError("version.source", "a strategy has 1 to 32 source files")
		}
		r.Source = sandboxrt.Source{}
		var total int64
		for name, text := range *in.Source {
			if len(text) > limits.MaxFileBytes {
				return marketplace.VersionRequest{}, validationError("version.source", "a source file is at most 65536 bytes")
			}
			total += int64(len(text))
			r.Source[name] = []byte(text)
		}
		if total > limits.MaxTotalBytes {
			return marketplace.VersionRequest{}, validationError("version.source", "a strategy's source is at most 262144 bytes in all")
		}
	}
	return r, nil
}

// marketplaceManifest translates a manifest in canonical form: the lists
// sorted and de-duplicated and the schema named, so the hash the service
// stores is the hash of exactly what is returned.
func marketplaceManifest(in api.StrategyManifestInput) sandboxrt.Manifest {
	m := sandboxrt.Manifest{Schema: deref(in.Schema), MaxNotionalPerIntent: in.MaxNotionalPerIntent, MaxIntentsPerRun: in.MaxIntentsPerRun,
		MinIntervalSeconds: in.MinIntervalSeconds}
	for _, k := range in.AllowedIntentKinds {
		m.AllowedIntentKinds = append(m.AllowedIntentKinds, econ.IntentKind(k))
	}
	if in.AllowedInstrumentIds != nil {
		for _, u := range *in.AllowedInstrumentIds {
			m.AllowedInstrumentIDs = append(m.AllowedInstrumentIDs, u.String())
		}
	}
	if in.AllowedInstrumentKinds != nil {
		for _, k := range *in.AllowedInstrumentKinds {
			m.AllowedInstrumentKinds = append(m.AllowedInstrumentKinds, string(k))
		}
	}
	return m.Canonicalize()
}

// marketplaceInput translates a snapshot into the strategy input document.
// Its validation -- decimals, ordering, bounds -- is the sandbox's.
func marketplaceInput(s api.StrategySnapshot) (sandboxrt.Input, error) {
	in := sandboxrt.Input{Schema: sandboxrt.InputSchema, AsOf: s.AsOf.UTC(),
		Portfolio: sandboxrt.InputPortfolio{Cash: s.Portfolio.Balance, Positions: []sandboxrt.InputPosition{}}}
	for _, ins := range s.Instruments {
		if ins.BaseDecimals < 0 || ins.BaseDecimals > 18 || ins.QuoteDecimals < 0 || ins.QuoteDecimals > 18 {
			return sandboxrt.Input{}, validationError("snapshot.instruments", "base_decimals and quote_decimals are 0 to 18")
		}
		row := sandboxrt.InputInstrument{InstrumentID: ins.InstrumentId.String(), Kind: string(ins.Kind),
			BaseDecimals: uint8(ins.BaseDecimals), QuoteDecimals: uint8(ins.QuoteDecimals), // #nosec G115 -- bounded to 0..18 above
			Mark: ins.Mark, BestBid: deref(ins.BestBid), BestAsk: deref(ins.BestAsk), Closes: []string{}}
		if ins.Closes != nil {
			row.Closes = append(row.Closes, *ins.Closes...)
		}
		in.Instruments = append(in.Instruments, row)
	}
	for _, p := range s.Portfolio.Positions {
		in.Portfolio.Positions = append(in.Portfolio.Positions, sandboxrt.InputPosition{InstrumentID: p.InstrumentId.String(), Quantity: p.Quantity})
	}
	return in, nil
}

// --- response shapes --------------------------------------------------------------

func marketplaceHex(b []byte) string { return hex.EncodeToString(b) }

func marketplaceHexPtr(b []byte) *string {
	if len(b) == 0 {
		return nil
	}
	s := hex.EncodeToString(b)
	return &s
}

// marketplaceUUIDs renders stored canonical ids. They were validated as
// canonical UUIDs when the version was written; one that is not is a fault, not
// something to drop.
func marketplaceUUIDs(ss []string) ([]api.UUID, error) {
	out := make([]api.UUID, 0, len(ss))
	for _, s := range ss {
		u, err := uuid.Parse(s)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "a stored strategy version names a malformed instrument id")
		}
		out = append(out, u)
	}
	return out, nil
}

func toAPIStrategyVersionSummary(v MarketplaceVersionSummary) api.StrategyVersionSummary {
	return api.StrategyVersionSummary{
		Id: toUUID(v.ID), ListingId: toUUID(v.ListingID), Number: v.Number, Status: api.StrategyVersionStatus(v.Status),
		Moderation: api.StrategyModeration(v.Moderation), RiskClass: api.StrategyRiskClass(v.RiskClass), Description: v.Description,
		StrategyRef: marketplace.Version{ListingID: v.ListingID, Number: v.Number}.StrategyRef(), RejectionCode: strPtr(v.RejectionCode),
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
}

func toAPIStrategyListing(s MarketplaceListingSummary, all bool) api.StrategyListing {
	out := api.StrategyListing{Id: toUUID(s.Listing.ID), Title: s.Listing.Title, CreatedAt: s.Listing.CreatedAt,
		LatestVersion: toAPIStrategyVersionSummary(s.Latest)}
	if s.Listing.ForkedFromVersionID != nil {
		out.ForkedFromVersionId = uuidPtr(*s.Listing.ForkedFromVersionID)
	}
	if all {
		out.CreatorAccountId = uuidPtr(s.Listing.CreatorAccountID)
	}
	return out
}

func toAPIStrategyManifest(m sandboxrt.Manifest) (api.StrategyManifest, error) {
	ids, err := marketplaceUUIDs(m.AllowedInstrumentIDs)
	if err != nil {
		return api.StrategyManifest{}, err
	}
	out := api.StrategyManifest{
		Schema: m.Schema, AllowedInstrumentIds: ids, AllowedIntentKinds: make([]api.StrategyIntentKind, 0, len(m.AllowedIntentKinds)),
		AllowedInstrumentKinds: make([]api.InstrumentKind, 0, len(m.AllowedInstrumentKinds)),
		MaxNotionalPerIntent:   m.MaxNotionalPerIntent, MaxIntentsPerRun: m.MaxIntentsPerRun, MinIntervalSeconds: m.MinIntervalSeconds,
	}
	for _, k := range m.AllowedIntentKinds {
		out.AllowedIntentKinds = append(out.AllowedIntentKinds, api.StrategyIntentKind(k))
	}
	for _, k := range m.AllowedInstrumentKinds {
		out.AllowedInstrumentKinds = append(out.AllowedInstrumentKinds, api.InstrumentKind(k))
	}
	return out, nil
}

func toAPIStrategyVersion(v marketplace.Version, all bool) (api.StrategyVersion, error) {
	manifest, err := toAPIStrategyManifest(v.Manifest)
	if err != nil {
		return api.StrategyVersion{}, err
	}
	supported, err := marketplaceUUIDs(v.SupportedInstrumentIDs)
	if err != nil {
		return api.StrategyVersion{}, err
	}
	config := make(map[string]string, len(v.Config))
	for k, val := range v.Config {
		config[k] = val
	}
	out := api.StrategyVersion{
		ValueDomain: api.SIMULATED, Id: toUUID(v.ID), ListingId: toUUID(v.ListingID), Number: v.Number,
		Status: api.StrategyVersionStatus(v.Status), Moderation: api.StrategyModeration(v.Moderation), Description: v.Description,
		RiskClass: api.StrategyRiskClass(v.RiskClass), SupportedInstrumentIds: supported, Manifest: manifest,
		ManifestHash: marketplaceHex(v.ManifestHash), SourceDigest: marketplaceHex(v.SourceDigest), SourceCommit: v.SourceCommit,
		BacktestEvidenceRef: v.BacktestEvidenceRef, DataDependencies: append([]string{}, v.DataDependencies...),
		Fees: api.StrategyFees{PerformanceFeeBps: int(v.Fees.PerformanceFeeBPS), ManagementFeeBps: int(v.Fees.ManagementFeeBPS),
			HighWaterMark: v.Fees.HighWaterMark},
		Config: config, StrategyRef: v.StrategyRef(), CreatedByActorType: string(v.CreatedBy.Type),
		RejectionCode: strPtr(v.RejectionCode), CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
	if all {
		out.CreatedByActorId = strPtr(v.CreatedBy.ID)
	}
	return out, nil
}

func toAPIStrategyValidation(val marketplace.Validation) api.StrategyValidation {
	out := api.StrategyValidation{
		Outcome: api.StrategyValidationOutcome(val.Outcome), ArtifactDigest: marketplaceHexPtr(val.ArtifactDigest),
		ScanCodes: append([]string{}, val.ScanCodes...), ToolchainVersion: val.ToolchainVersion, SdkVersion: val.SDKVersion,
		RuntimeVersion: val.RuntimeVersion,
	}
	if val.FailureReason != "" {
		f := api.StrategyBuildFailure(val.FailureReason)
		out.FailureReason = &f
	}
	return out
}

func toAPIStrategyProvenance(p marketplace.Provenance) api.StrategyProvenance {
	out := api.StrategyProvenance{
		StrategyRef: p.StrategyRef, SourceCommit: p.SourceCommit, SourceDigest: marketplaceHex(p.SourceDigest),
		ManifestHash: marketplaceHex(p.ManifestHash), ArtifactDigest: marketplaceHexPtr(p.ArtifactDigest),
		ToolchainVersion: strPtr(p.ToolchainVersion), SdkVersion: strPtr(p.SDKVersion), RuntimeVersion: strPtr(p.RuntimeVersion),
		DatasetVersion: strPtr(p.DatasetVersion), ForkedFrom: make([]api.StrategyForkAncestor, 0, len(p.ForkedFrom)),
	}
	if p.EvaluationID != nil {
		out.EvaluationId = uuidPtr(*p.EvaluationID)
	}
	for _, f := range p.ForkedFrom {
		out.ForkedFrom = append(out.ForkedFrom, api.StrategyForkAncestor{
			VersionId: toUUID(f.VersionID), ListingId: toUUID(f.ListingID), Number: f.Number,
			SourceDigest: marketplaceHex(f.SourceDigest), ManifestHash: marketplaceHex(f.ManifestHash),
		})
	}
	return out
}

func toAPIStrategyEvaluation(e MarketplaceEvaluation, withRuns bool) api.StrategyEvaluation {
	ev := e.Evaluation
	out := api.StrategyEvaluation{
		ValueDomain: api.SIMULATED, Id: toUUID(ev.ID), VersionId: toUUID(ev.VersionID), Kind: api.StrategyEvaluationKind(ev.Kind),
		DatasetVersion: ev.DatasetVersion, SnapshotsCount: e.SnapshotsCount, SnapshotsDigest: marketplaceHex(ev.SnapshotsDigest),
		ResultsDigest: marketplaceHex(ev.ResultsDigest), RunsOk: ev.RunsOK, ProposalsCount: ev.ProposalsCount,
		AcceptedCount: ev.AcceptedCount, ViolationsCount: ev.ViolationsCount, EvidenceRef: ev.EvidenceRef,
		EvidenceSha256: marketplaceHex(ev.EvidenceSHA256), CreatedAt: timePtr(&e.CreatedAt),
	}
	if withRuns {
		runs := make([]api.StrategyEvaluationRun, 0, len(ev.Runs))
		for _, r := range ev.Runs {
			runs = append(runs, api.StrategyEvaluationRun{
				Seq: r.Seq, SandboxRunId: toUUID(r.SandboxRunID), AsOf: r.AsOf.UTC(), InputDigest: r.InputDigest, RunDigest: r.RunDigest,
				Termination: api.StrategyRunTermination(r.Termination), Proposals: r.Proposals, Accepted: r.Accepted, Violations: r.Violations,
			})
		}
		out.Runs = &runs
	}
	return out
}

// toAPIStrategyPerformance renders a paper performance snapshot. The install
// is named only to its own account: on a listing, snapshots are other
// accounts' installs.
func toAPIStrategyPerformance(p marketplace.PerformanceSnapshot, withInstall bool) api.StrategyPerformanceSnapshot {
	out := api.StrategyPerformanceSnapshot{
		ValueDomain: api.SIMULATED, VersionId: toUUID(p.VersionID), FollowRunsCount: p.Points, FirstAsOf: p.FirstAsOf,
		LastAsOf: p.LastAsOf, HistoryDigest: marketplaceHex(p.HistoryDigest), ReturnBps: p.ReturnBPS, MaxDrawdownBps: p.MaxDrawdownBPS,
		VolatilityBps: p.VolatilityBPS, TurnoverBps: p.TurnoverBPS, ComputedAt: p.ComputedAt,
	}
	if withInstall {
		out.InstallId = uuidPtr(p.InstallID)
	}
	return out
}

func toAPIStrategyVersionDetail(vv MarketplaceVersionView, all bool) (api.StrategyVersionDetail, error) {
	base, err := toAPIStrategyVersion(vv.Version, all)
	if err != nil {
		return api.StrategyVersionDetail{}, err
	}
	out := api.StrategyVersionDetail{
		ValueDomain: base.ValueDomain, Id: base.Id, ListingId: base.ListingId, Number: base.Number, Status: base.Status,
		Moderation: base.Moderation, Description: base.Description, RiskClass: base.RiskClass,
		SupportedInstrumentIds: base.SupportedInstrumentIds, Manifest: base.Manifest, ManifestHash: base.ManifestHash,
		SourceDigest: base.SourceDigest, SourceCommit: base.SourceCommit, BacktestEvidenceRef: base.BacktestEvidenceRef,
		DataDependencies: base.DataDependencies, Fees: base.Fees, Config: base.Config, StrategyRef: base.StrategyRef,
		CreatedByActorType: base.CreatedByActorType, CreatedByActorId: base.CreatedByActorId, RejectionCode: base.RejectionCode,
		CreatedAt: base.CreatedAt, UpdatedAt: base.UpdatedAt,
		Reviews:     make([]api.StrategyReview, 0, len(vv.Reviews)),
		Provenance:  toAPIStrategyProvenance(vv.Provenance),
		Evaluations: make([]api.StrategyEvaluation, 0, len(vv.Evaluations)),
		Performance: make([]api.StrategyPerformanceSnapshot, 0, len(vv.Performance)),
	}
	if vv.Validation != nil {
		val := toAPIStrategyValidation(*vv.Validation)
		out.Validation = &val
	}
	for _, r := range vv.Reviews {
		rv := api.StrategyReview{Decision: api.StrategyReviewDecision(r.Decision), ReasonCode: r.ReasonCode, Note: r.Note,
			ReviewerActorType: string(r.Reviewer.Type), ReviewedAt: r.ReviewedAt}
		if all {
			rv.ReviewerActorId = strPtr(r.Reviewer.ID)
		}
		out.Reviews = append(out.Reviews, rv)
	}
	for _, e := range vv.Evaluations {
		out.Evaluations = append(out.Evaluations, toAPIStrategyEvaluation(e, false))
	}
	for _, p := range vv.Performance {
		out.Performance = append(out.Performance, toAPIStrategyPerformance(p, false))
	}
	return out, nil
}

// toAPIStrategyListingDetail renders a listing to its audience: everything to
// those who see all of it, the public versions alone to anyone else.
func toAPIStrategyListingDetail(view MarketplaceListingView, all bool) (api.StrategyListingDetail, error) {
	l := view.Listing
	out := api.StrategyListingDetail{ValueDomain: api.SIMULATED, Id: toUUID(l.ID), Title: l.Title, CreatedAt: l.CreatedAt,
		Versions: make([]api.StrategyVersionDetail, 0, len(view.Versions))}
	if l.ForkedFromVersionID != nil {
		out.ForkedFromVersionId = uuidPtr(*l.ForkedFromVersionID)
	}
	if all {
		out.CreatorAccountId = uuidPtr(l.CreatorAccountID)
	}
	for _, vv := range view.Versions {
		if !all && !marketplacePublic(vv.Version.Status, vv.Version.Moderation) {
			continue
		}
		d, err := toAPIStrategyVersionDetail(vv, all)
		if err != nil {
			return api.StrategyListingDetail{}, err
		}
		out.Versions = append(out.Versions, d)
	}
	return out, nil
}

func toAPIStrategyInstall(iv MarketplaceInstallView) api.StrategyInstall {
	in := iv.Install
	out := api.StrategyInstall{
		ValueDomain: api.SIMULATED, Id: toUUID(in.ID), AccountId: toUUID(in.AccountID), VersionId: toUUID(in.VersionID),
		ListingId: toUUID(iv.Listing.ID), ListingTitle: iv.Listing.Title,
		StrategyRef:   marketplace.Version{ListingID: iv.Version.ListingID, Number: iv.Version.Number}.StrategyRef(),
		VersionNumber: iv.Version.Number, VersionStatus: api.StrategyVersionStatus(iv.Version.Status),
		VersionModeration: api.StrategyModeration(iv.Version.Moderation), Mode: api.AdvancedMode(in.Mode),
		Status: api.StrategyInstallStatus(in.Status), InstalledByActorType: string(in.InstalledBy.Type), CreatedAt: in.CreatedAt,
	}
	if iv.LatestPerformance != nil {
		p := toAPIStrategyPerformance(*iv.LatestPerformance, true)
		out.LatestPerformance = &p
	}
	return out
}

func toAPIMarketplaceDecision(r risk.AdvancedDecisionRecord) api.AdvancedRiskDecision {
	return api.AdvancedRiskDecision{
		Id: toUUID(r.ID), IntentId: parseUUIDText(r.IntentID), IntentKind: r.IntentKind, Stage: string(r.Stage),
		Verdict: api.AdvancedRiskDecisionVerdict(r.Verdict), ReasonCodes: append([]string{}, r.ReasonCodes...), InputHash: r.InputHash,
		PolicyVersion: r.PolicyVersion, PolicyHash: r.PolicyHash, EvaluatorVersion: r.EvaluatorVersion, DecisionHash: r.DecisionHash,
		EvaluatedAt: r.EvaluatedAt,
	}
}

// toAPIStrategyFollowRun renders a follow run with what became of every
// intent, from the records the services wrote. replayed is set on the answer
// to a follow request only.
func toAPIStrategyFollowRun(v MarketplaceFollowView, replayed *bool) api.StrategyFollowRun {
	r := v.Run
	out := api.StrategyFollowRun{
		ValueDomain: api.SIMULATED, Id: toUUID(r.ID), InstallId: toUUID(r.InstallID), VersionId: toUUID(r.VersionID),
		AccountId: toUUID(r.AccountID), SandboxRunId: toUUID(r.SandboxRunID), Trigger: api.StrategyFollowTrigger(r.Trigger),
		AsOf: r.AsOf, InputDigest: marketplaceHex(r.InputDigest), RunDigest: marketplaceHex(r.RunDigest),
		Termination: api.StrategyRunTermination(r.Termination), ProposalsCount: r.ProposalsCount, AcceptedCount: r.AcceptedCount,
		ViolationsCount: r.ViolationsCount, IntentsSubmitted: r.IntentsSubmitted, IntentsDispatched: r.IntentsDispatched,
		IntentsRiskRejected: r.IntentsRiskRejected, IntentsFailed: r.IntentsFailed, Equity: r.Equity, Turnover: r.Turnover,
		EvidenceRef: r.EvidenceRef, EvidenceSha256: marketplaceHex(r.EvidenceSHA256), CreatedAt: r.CreatedAt, Replayed: replayed,
		Intents: make([]api.StrategyFollowIntent, 0, len(r.Intents)),
	}
	for _, fi := range r.Intents {
		item := api.StrategyFollowIntent{
			ProposalSeq: fi.ProposalSeq, IntentId: toUUID(fi.IntentID), Kind: string(fi.Kind), Outcome: api.StrategyFollowOutcome(fi.Outcome),
			Notional: fi.Notional, DecisionHash: strPtr(fi.DecisionHash), ExternalRef: strPtr(fi.ExternalRef),
		}
		if d, ok := v.Decisions[fi.IntentID]; ok {
			rd := toAPIMarketplaceDecision(d)
			item.RiskDecision = &rd
		}
		if fi.EffectID != nil {
			item.EffectId = uuidPtr(*fi.EffectID)
			if st, ok := v.Effects[*fi.EffectID]; ok {
				item.EffectStatus = strPtr(string(st.Status))
				item.EffectFailureCode = strPtr(st.FailureCode)
				if item.ExternalRef == nil {
					item.ExternalRef = strPtr(st.ExternalRef)
				}
			}
		}
		out.Intents = append(out.Intents, item)
	}
	return out
}

func toAPIStrategySandboxRun(v MarketplaceRunView) api.StrategySandboxRun {
	rec := v.Run
	out := api.StrategySandboxRun{
		Id: toUUID(rec.ID), Source: api.StrategyRunSourceFollow, StrategyRef: rec.StrategyRef,
		Termination: api.StrategyRunTermination(rec.Termination), ExitCode: rec.ExitCode, ArtifactVerified: rec.ArtifactVerified,
		WallMs: rec.WallMS, OutputBytes: rec.OutputBytes, ArtifactDigest: marketplaceHex(rec.ArtifactDigest),
		InputDigest: marketplaceHex(rec.InputDigest), OutputDigest: marketplaceHex(rec.OutputDigest), RunDigest: marketplaceHex(rec.RunDigest),
		LimitsVersion: rec.LimitsVersion, ProposalsCount: rec.ProposalsCount, AcceptedCount: rec.AcceptedCount,
		ViolationsCount: rec.ViolationsCount, CreatedAt: rec.CreatedAt, Violations: make([]api.StrategyRunViolation, 0, len(v.Violations)),
	}
	if v.FollowRunID != nil {
		out.FollowRunId = uuidPtr(*v.FollowRunID)
	}
	if v.EvaluationID != nil {
		out.Source = api.StrategyRunSourceSimulation
		out.EvaluationId = uuidPtr(*v.EvaluationID)
	}
	for _, vi := range v.Violations {
		out.Violations = append(out.Violations, api.StrategyRunViolation{RecordSeq: vi.Seq, Code: string(vi.Code), Detail: vi.Detail})
	}
	return out
}
